package health

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/watcher"
)

// TwitchClient is the slice of the Twitch client the canary needs to bring its
// probe channel online. Satisfied by *api.TwitchClient.
type TwitchClient interface {
	GetChannelID(username string) (string, error)
	CheckStreamerOnline(streamer *models.Streamer)
}

// Prober runs the instrumented watch-transport sequence (playback token ->
// playlist/segment -> spade beacon). Satisfied by *watcher.MinuteSender — there
// is no separate beacon implementation.
type Prober interface {
	Probe(ctx context.Context, streamer *models.Streamer) watcher.ProbeResult
}

// Notifier is told about watch-transport health transitions. Satisfied by
// *notifications.Manager (may be nil).
type Notifier interface {
	NotifyHealthTransition(signal string, healthy bool, detail string)
}

// SlotView reports how many watch slots are currently free, for opportunistic
// scheduling. Satisfied by *watcher.MinuteWatcher (may be nil, meaning "always
// free" — the canary then runs purely on its interval).
type SlotView interface {
	FreeSlots() int
}

// CanaryConfig is the canary's runtime configuration.
type CanaryConfig struct {
	Enabled      bool
	Channel      string
	Interval     time.Duration
	MaxStaleness time.Duration
}

const (
	// canaryTimeout bounds a single probe end-to-end.
	canaryTimeout = 60 * time.Second
	// probeEvalCadence is how often the loop re-evaluates whether to probe. It
	// is deliberately shorter than a typical interval so an opportunistic
	// slot-free window is caught promptly; the actual decision (interval elapsed,
	// slot free, or past max staleness) is made in maybeRun.
	probeEvalCadence = 10 * time.Minute
)

// Canary periodically verifies the Twitch watch transport by running one real
// minute-watched probe against a configured channel and recording the outcome
// as the watch_transport health signal.
//
// It is the single, documented, rare exception to the "at most two watch slots"
// rule: it never holds a broker slot and is not a candidate source. It fires
// opportunistically when a broker slot is free, or is forced once the transport
// has not been confirmed for MaxStaleness — so at most one extra beacon can
// briefly coincide with two busy slots, and only on the max-staleness schedule.
// It confirms Twitch accepts the watch transport and beacon; without an active
// drop campaign it does NOT prove accrual of a specific drop.
type Canary struct {
	center   *Center
	client   TwitchClient
	prober   Prober
	notifier Notifier
	slots    SlotView
	now      func() time.Time
	timeout  time.Duration

	mu       sync.Mutex
	cfg      CanaryConfig
	streamer *models.Streamer // cached ephemeral probe channel

	running     atomic.Bool // duplicate-run suppression
	lastHealthy bool        // last transport health, for transition detection (mu-guarded)

	trigger chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
}

// NewCanary builds a canary. slots and notifier may be nil.
func NewCanary(center *Center, client TwitchClient, prober Prober, notifier Notifier, slots SlotView, cfg CanaryConfig) *Canary {
	return &Canary{
		center:      center,
		client:      client,
		prober:      prober,
		notifier:    notifier,
		slots:       slots,
		now:         time.Now,
		timeout:     canaryTimeout,
		cfg:         cfg,
		lastHealthy: true, // assume healthy until a probe proves otherwise, so the first failure notifies
		trigger:     make(chan struct{}, 1),
	}
}

func (c *Canary) Start(ctx context.Context) {
	c.mu.Lock()
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.mu.Unlock()
	go c.loop()
}

func (c *Canary) Stop() {
	c.mu.Lock()
	if c.cancel != nil {
		c.cancel()
	}
	c.mu.Unlock()
}

// RunNow requests an immediate probe (the "Run canary now" button). It is
// non-blocking: the probe runs on the loop goroutine and the result appears in
// the next health snapshot. A manual run probes as long as a channel is
// configured, even if the scheduled canary is disabled.
func (c *Canary) RunNow() {
	select {
	case c.trigger <- struct{}{}:
	default:
	}
}

// UpdateSettings applies a runtime configuration change (no restart). Changing
// the channel drops the cached ephemeral streamer so the next probe re-resolves.
func (c *Canary) UpdateSettings(cfg CanaryConfig) {
	c.mu.Lock()
	if !strings.EqualFold(c.cfg.Channel, cfg.Channel) {
		c.streamer = nil
	}
	c.cfg = cfg
	c.mu.Unlock()
}

func (c *Canary) loop() {
	c.mu.Lock()
	ctx := c.ctx
	c.mu.Unlock()
	for {
		timer := time.NewTimer(c.nextWait())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-c.trigger:
			timer.Stop()
			c.runOnce(true) // manual: probe regardless of the enabled flag
		case <-timer.C:
			c.maybeRun()
		}
	}
}

// nextWait returns the jittered idle cadence between evaluations (±20%, matching
// the watcher's jitter convention).
func (c *Canary) nextWait() time.Duration {
	j := (rand.Float64() - 0.5) * 0.4
	return time.Duration(float64(probeEvalCadence) * (1 + j))
}

// maybeRun decides whether a scheduled probe is due: run when the interval has
// elapsed AND a broker slot is free (opportunistic), or unconditionally once the
// transport has not been confirmed for MaxStaleness (forced).
func (c *Canary) maybeRun() {
	cfg := c.snapshotCfg()
	if !cfg.Enabled || cfg.Channel == "" {
		return
	}
	since := c.sinceLastTransportCheck()
	due := since >= cfg.Interval
	stale := cfg.MaxStaleness > 0 && since >= cfg.MaxStaleness
	slotFree := c.slots == nil || c.slots.FreeSlots() > 0
	if stale || (due && slotFree) {
		c.runOnce(false)
	}
}

// runOnce performs a single probe, guarded so concurrent runs never overlap
// (duplicate-run suppression). manual=true probes even when the scheduled canary
// is disabled, as long as a channel is configured.
func (c *Canary) runOnce(manual bool) {
	if !c.running.CompareAndSwap(false, true) {
		return
	}
	defer c.running.Store(false)

	cfg := c.snapshotCfg()
	if cfg.Channel == "" || (!manual && !cfg.Enabled) {
		return
	}

	c.mu.Lock()
	parent := c.ctx
	c.mu.Unlock()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, c.timeout)
	defer cancel()

	sig := c.probe(ctx, cfg.Channel)
	c.center.Record(sig)
	c.handleTransition(sig)
}

// probe runs the full watch-transport check and returns the redacted signal.
func (c *Canary) probe(ctx context.Context, channel string) Signal {
	start := c.now()

	streamer, err := c.streamerFor(channel)
	if err != nil {
		return c.failSignal("stream_info", "could not resolve the canary channel", "channel_resolve_failed", start)
	}

	// Bring the channel online (spade URL + payload) — same path a watched
	// streamer uses. Cancellation between here and the beacon is honored by the
	// probe's context.
	select {
	case <-ctx.Done():
		return c.failSignal("stream_info", "probe cancelled", "cancelled", start)
	default:
	}
	c.client.CheckStreamerOnline(streamer)
	if !streamer.GetIsOnline() {
		return c.failSignal("stream_info", "the canary channel is offline", "channel_offline", start)
	}
	if streamer.Stream.SpadeURL == "" {
		return c.failSignal("spade_url", "spade URL was not discovered", "spade_url_missing", start)
	}

	res := c.prober.Probe(ctx, streamer)
	if !res.OK {
		return Signal{
			Name:      SignalWatchTransport,
			Status:    StatusFailed,
			CheckedAt: c.now(),
			Duration:  res.Duration,
			Stage:     string(res.Stage),
			ErrorCode: res.ErrorCode,
			Detail:    probeDetail(res),
		}
	}
	return Signal{
		Name:      SignalWatchTransport,
		Status:    StatusOK,
		CheckedAt: c.now(),
		Duration:  res.Duration,
		Stage:     string(watcher.StageBeacon),
		Detail:    "Twitch accepted the watch beacon (transport OK; does not prove drop accrual)",
	}
}

func (c *Canary) failSignal(stage, detail, code string, start time.Time) Signal {
	return Signal{
		Name:      SignalWatchTransport,
		Status:    StatusFailed,
		CheckedAt: c.now(),
		Duration:  c.now().Sub(start),
		Stage:     stage,
		Detail:    detail,
		ErrorCode: code,
	}
}

// probeDetail renders a safe, human summary from the redacted ProbeResult —
// stage plus HTTP status only, never the raw error or the signed URL.
func probeDetail(res watcher.ProbeResult) string {
	if res.Status > 0 {
		return fmt.Sprintf("watch beacon failed at the %s stage (HTTP %d)", res.Stage, res.Status)
	}
	return fmt.Sprintf("watch beacon failed at the %s stage", res.Stage)
}

// handleTransition fires a notification only when the transport health actually
// flips (healthy->failed or failed->recovered), never on repeated same-state
// results. The baseline is assumed healthy, so the first failed probe notifies.
func (c *Canary) handleTransition(sig Signal) {
	healthy := sig.Healthy()

	c.mu.Lock()
	prev := c.lastHealthy
	c.lastHealthy = healthy
	c.mu.Unlock()

	if prev == healthy {
		return
	}
	if c.notifier != nil {
		c.notifier.NotifyHealthTransition(SignalWatchTransport, healthy, sig.Detail)
	}
}

// streamerFor returns the cached ephemeral probe streamer for channel, resolving
// its channel ID (needed for the beacon payload) once. The network resolve runs
// without holding the lock.
func (c *Canary) streamerFor(channel string) (*models.Streamer, error) {
	c.mu.Lock()
	cached := c.streamer
	c.mu.Unlock()
	if cached != nil && strings.EqualFold(cached.Username, channel) {
		return cached, nil
	}

	id, err := c.client.GetChannelID(channel)
	if err != nil {
		return nil, err
	}

	s := models.NewStreamer(channel, models.StreamerSettings{ClaimDrops: false, Chat: models.ChatNever})
	s.ChannelID = id

	c.mu.Lock()
	c.streamer = s
	c.mu.Unlock()
	return s, nil
}

func (c *Canary) snapshotCfg() CanaryConfig {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg
}

func (c *Canary) sinceLastTransportCheck() time.Duration {
	sig, ok := c.center.Signal(SignalWatchTransport)
	if !ok || sig.CheckedAt.IsZero() {
		return time.Duration(1<<62 - 1) // effectively infinite: due and stale
	}
	return c.now().Sub(sig.CheckedAt)
}
