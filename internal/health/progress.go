package health

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/drops"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/watcher"
)

// DropsView is the slice of the drops tracker the progress watchdog needs:
// the published campaign snapshots, the sync/observation bookkeeping, and the
// two forced-sync recovery levers. Satisfied by *drops.DropsTracker.
type DropsView interface {
	Campaigns() []*models.Campaign
	SyncStatus() drops.SyncStatus
	SyncNow()
	TriggerProgressSync()
}

// WatchView is the slice of the slot broker the watchdog needs: who holds a
// slot, per-channel delivery accounting, and the staged session-refresh
// levers (the broker executes those on its own loop goroutine — the watchdog
// never mutates a live streamer itself). Satisfied by *watcher.MinuteWatcher.
type WatchView interface {
	BrokerSnapshot() watcher.BrokerSnapshot
	IsWatching(login string) bool
	ReportStats(login string) (watcher.ReportStats, bool)
	RequestSessionRefresh(login string, mode watcher.RefreshMode)
	LastSessionRefresh(login string) (watcher.SessionRefreshOutcome, bool)
}

// DropNotifier is told when a drop is confirmed stalled beyond recovery, and
// when a previously-stalled drop starts progressing again. Satisfied by the
// miner's notification adapter (may be nil).
type DropNotifier interface {
	NotifyDropStalled(campaign, drop, channel, detail string)
	NotifyDropRecovered(campaign, drop, channel, detail string)
}

// StreamerResolver maps a channel login to its live streamer object
// (configured list or discovery's current channel). Returns nil when unknown.
// The watchdog only READS the streamer through its locked accessors; all
// mutation goes through the broker's staged session refresh.
type StreamerResolver func(login string) *models.Streamer

// WatchdogConfig is the progress watchdog's runtime configuration. Thresholds
// are deliberately conservative: Twitch credits drop minutes in batches
// (typically every ~15 minutes), so a short quiet period is normal and must
// never confirm a stall.
type WatchdogConfig struct {
	Enabled            bool
	StallDelay         time.Duration // min wall time without progress before a stall can confirm
	StallConfirmations int           // consecutive completed inventory observations without progress
	RecoveryCooldown   time.Duration // min gap between two recovery-stage executions
	AvoidTTL           time.Duration // how long a switched-away channel stays excluded
	Rearm              time.Duration // after pipeline exhaustion, when it may run again
}

// Drop progress statuses shown on the Drops page and in the debug snapshot.
const (
	ProgressHealthy    = "healthy"
	ProgressRecovering = "recovering"
	ProgressStalled    = "stalled"
)

const (
	// watchdogEvalCadence is how often the watchdog re-evaluates all tracked
	// drops (jittered ±20%, matching the project convention).
	watchdogEvalCadence = time.Minute
	// stallMinReports is how many successful minute-watched deliveries must
	// have gone to the farming channel since the last observed progress before
	// a stall can confirm — "we are demonstrably watching, Twitch is
	// demonstrably not crediting". Roughly five watched minutes.
	stallMinReports = 5
	// recoveryStageTimeout bounds the blocking recovery stages (forced full
	// resync, transport probe) on the watchdog goroutine.
	recoveryStageTimeout = 60 * time.Second
)

// DropProgress is the published per-drop watchdog state: what the Drops page
// badge and the debug snapshot render. It carries no URLs or tokens.
type DropProgress struct {
	CampaignID           string    `json:"campaignId"`
	CampaignName         string    `json:"campaignName"`
	DropID               string    `json:"dropId"`
	DropName             string    `json:"dropName"`
	Channel              string    `json:"channel,omitempty"`
	LastMinutes          int       `json:"lastMinutes"`
	LastProgressAt       time.Time `json:"lastProgressAt,omitzero"`
	ReportsSinceProgress int       `json:"reportsSinceProgress"`
	NoProgressObs        int       `json:"noProgressObservations"`
	Status               string    `json:"status"`
	RecoveryStage        int       `json:"recoveryStage,omitempty"`
	RecoveryStageName    string    `json:"recoveryStageName,omitempty"`
	LastRecoveryAt       time.Time `json:"lastRecoveryAt,omitzero"`
	Detail               string    `json:"detail,omitempty"`
}

// ProgressSnapshot is the immutable published view of every tracked drop's
// watchdog state.
type ProgressSnapshot struct {
	Enabled     bool           `json:"enabled"`
	EvaluatedAt time.Time      `json:"evaluatedAt,omitzero"`
	Drops       []DropProgress `json:"drops"`
}

// dropState is the watchdog's internal per-drop state (keyed by
// campaignID+dropID). The embedded DropProgress is the published part.
type dropState struct {
	DropProgress

	// evidenceSince is when the current uninterrupted stall-evidence window
	// began: the moment every confirmation gate started holding. Zero while any
	// gate fails. All three stall thresholds (delay, observations, reports)
	// count only inside this window, so a confirmed stall always represents at
	// least StallDelay of DEMONSTRABLE farming without credit — evidence
	// accrued while the channel was offline, rotated out, or ineligible never
	// carries over (that would confirm a stall minutes after farming resumes,
	// well inside Twitch's ~15-minute crediting batch).
	evidenceSince      time.Time
	lastObservedSyncAt time.Time // ProgressLastSyncAt already counted as an observation
	baselineReports    int       // farming channel's success count at last progress
	statsChannel       string    // channel the baseline belongs to
	avoidedChannel     string    // channel this episode's switch stage excluded ("" if none)
	exhaustedAt        time.Time // when the pipeline ran out of stages
	notifiedStalled    bool      // critical notification already sent this episode
}

// resetEvidence discards the stall-evidence window (a gate failed): the delay
// clock, observation counter, and delivery baseline all restart once farming
// is demonstrably active again. The recovery stage and notification flags
// survive — a transient gate blip must not restart the pipeline, only pause it
// and demand fresh evidence before the next stage.
func (st *dropState) resetEvidence() {
	st.evidenceSince = time.Time{}
	st.NoProgressObs = 0
	st.ReportsSinceProgress = 0
	st.statsChannel = "" // force a delivery re-baseline even for the same channel
}

// recoveryStage is one step of the staged recovery pipeline. run executes on
// the watchdog goroutine and returns a redacted human detail; it must be
// idempotent (a re-run after a cooldown performs the same bounded work).
type recoveryStage struct {
	name  string
	label string
	run   func(w *ProgressWatchdog, st *dropState, now time.Time) string
}

// recoveryStages is the ordered pipeline. Stages 3/5 stage their work into the
// slot broker (single-writer rule) instead of mutating the streamer here;
// stage 4 is read-only on the streamer. The spec's "refresh playback token"
// and "refresh playlist" steps map onto the transport probe: the sender holds
// no token/playlist cache to invalidate (both are fetched fresh on every
// send), so a staged, stage-instrumented verification is the honest
// equivalent. The pipeline is finite — after the last stage the drop is
// STALLED until progress resumes or the rearm window elapses.
var recoveryStages = []recoveryStage{
	{
		name:  "progress_sync",
		label: "forced inventory sync",
		run: func(w *ProgressWatchdog, _ *dropState, _ time.Time) string {
			w.drops.TriggerProgressSync()
			return "forced a lightweight inventory sync"
		},
	},
	{
		name:  "full_resync",
		label: "full campaign resync",
		run: func(w *ProgressWatchdog, _ *dropState, _ time.Time) string {
			// SyncNow re-runs the whole discovery pipeline including the
			// campaign/channel intersection recompute. It is ctx-unaware, so it
			// runs under the same detached watchdog the canary uses; an abandoned
			// run completes in the background (serialized by the tracker) and its
			// result is observed on a later tick.
			ctx, cancel := context.WithTimeout(w.parentCtx(), recoveryStageTimeout)
			defer cancel()
			if err := runDetached(ctx, w.drops.SyncNow); err != nil {
				return "forced a full campaign resync (still completing in the background)"
			}
			return "forced a full campaign resync: dashboard, details, inventory, and channel intersection refreshed"
		},
	},
	{
		name:  "stream_info",
		label: "stream info refresh",
		run: func(w *ProgressWatchdog, st *dropState, _ time.Time) string {
			w.watch.RequestSessionRefresh(st.Channel, watcher.RefreshStreamInfo)
			return "asked the slot broker to re-fetch stream info (broadcast, game, campaign IDs, payload)"
		},
	},
	{
		name:  "transport_probe",
		label: "watch transport probe",
		run: func(w *ProgressWatchdog, st *dropState, _ time.Time) string {
			streamer := w.resolveStreamer(st.Channel)
			if streamer == nil || w.prober == nil {
				return "transport probe skipped: channel object unavailable"
			}
			ctx, cancel := context.WithTimeout(w.parentCtx(), recoveryStageTimeout)
			defer cancel()
			res := w.prober.Probe(ctx, streamer)
			if res.OK {
				return "watch transport verified end-to-end: playback token, playlist, segment, and beacon all accepted"
			}
			return fmt.Sprintf("watch transport probe failed at the %s stage (%s)", res.Stage, res.ErrorCode)
		},
	},
	{
		name:  "session_recreate",
		label: "watch session recreate",
		run: func(w *ProgressWatchdog, st *dropState, _ time.Time) string {
			w.watch.RequestSessionRefresh(st.Channel, watcher.RefreshSession)
			return "asked the slot broker to recreate the watch session (spade URL, stream info, beacon payload)"
		},
	},
	{
		name:  "channel_switch",
		label: "channel switch",
		run: func(w *ProgressWatchdog, st *dropState, now time.Time) string {
			if w.avoid == nil || st.Channel == "" {
				return "channel switch skipped: no avoid list or no farming channel"
			}
			cfg := w.snapshotCfg()
			w.avoid.Avoid(st.Channel, now.Add(cfg.AvoidTTL), "drop progress stalled on this channel despite session recovery")
			st.avoidedChannel = st.Channel
			return fmt.Sprintf("temporarily excluded %s from watching (%s) — the slot broker will pick the next eligible channel", st.Channel, cfg.AvoidTTL)
		},
	},
	{
		name:  "notify",
		label: "critical notification",
		run: func(w *ProgressWatchdog, st *dropState, now time.Time) string {
			st.exhaustedAt = now
			if w.notifier != nil && !st.notifiedStalled {
				st.notifiedStalled = true
				w.notifier.NotifyDropStalled(st.CampaignName, st.DropName, st.Channel, st.Detail)
			}
			events.Record(events.TypeDropStalled, st.Channel, st.CampaignName+" / "+st.DropName)
			return "automatic recovery exhausted — operator notified"
		},
	},
}

// ProgressWatchdog detects a tracked drop whose minutes stop accruing even
// though everything upstream looks healthy (OAuth, GQL, channel online,
// beacons accepted, campaign active) and runs the staged recovery pipeline
// above. Detection is deliberately conjunctive — a stall confirms only when
// every gate holds (see evaluateDrop) — and recovery is finite: each stage is
// cooldown-bounded and idempotent, and the pipeline never loops.
type ProgressWatchdog struct {
	center   *Center
	drops    DropsView
	watch    WatchView
	prober   Prober
	notifier DropNotifier
	avoid    *AvoidList
	resolver StreamerResolver
	now      func() time.Time

	mu     sync.Mutex
	cfg    WatchdogConfig
	states map[string]*dropState // loop-owned; under mu only for UpdateSettings-driven resets
	ctx    context.Context
	cancel context.CancelFunc

	snap atomic.Pointer[ProgressSnapshot]
}

// NewProgressWatchdog builds the watchdog. notifier, avoid, and resolver may
// be nil (the corresponding stages then degrade to documented no-ops).
func NewProgressWatchdog(center *Center, dropsView DropsView, watch WatchView, prober Prober, notifier DropNotifier, avoid *AvoidList, resolver StreamerResolver, cfg WatchdogConfig) *ProgressWatchdog {
	w := &ProgressWatchdog{
		center:   center,
		drops:    dropsView,
		watch:    watch,
		prober:   prober,
		notifier: notifier,
		avoid:    avoid,
		resolver: resolver,
		now:      time.Now,
		cfg:      cfg,
		states:   make(map[string]*dropState),
	}
	w.publish(ProgressSnapshot{Enabled: cfg.Enabled})
	return w
}

func (w *ProgressWatchdog) Start(ctx context.Context) {
	w.mu.Lock()
	w.ctx, w.cancel = context.WithCancel(ctx)
	w.mu.Unlock()
	go w.loop()
}

func (w *ProgressWatchdog) Stop() {
	w.mu.Lock()
	if w.cancel != nil {
		w.cancel()
	}
	w.mu.Unlock()
}

// UpdateSettings applies a runtime configuration change without a restart.
func (w *ProgressWatchdog) UpdateSettings(cfg WatchdogConfig) {
	w.mu.Lock()
	w.cfg = cfg
	w.mu.Unlock()
}

// Snapshot returns the last published per-drop watchdog state. Lock-free.
func (w *ProgressWatchdog) Snapshot() ProgressSnapshot {
	if s := w.snap.Load(); s != nil {
		out := *s
		out.Drops = append([]DropProgress(nil), s.Drops...)
		return out
	}
	return ProgressSnapshot{}
}

// AvoidEntries exposes the active channel exclusions for the debug snapshot.
func (w *ProgressWatchdog) AvoidEntries() []AvoidEntry {
	if w.avoid == nil {
		return nil
	}
	return w.avoid.Entries()
}

func (w *ProgressWatchdog) loop() {
	w.mu.Lock()
	ctx := w.ctx
	w.mu.Unlock()
	for {
		j := (rand.Float64() - 0.5) * 0.4
		timer := time.NewTimer(time.Duration(float64(watchdogEvalCadence) * (1 + j)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			w.evaluate(w.now())
		}
	}
}

func (w *ProgressWatchdog) parentCtx() context.Context {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ctx != nil {
		return w.ctx
	}
	return context.Background()
}

func (w *ProgressWatchdog) snapshotCfg() WatchdogConfig {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cfg
}

// resolveStreamer returns the live streamer for a login, or nil.
func (w *ProgressWatchdog) resolveStreamer(login string) *models.Streamer {
	if w.resolver == nil || login == "" {
		return nil
	}
	return w.resolver(login)
}

// twitchOutage reports whether the health center currently shows evidence of
// a Twitch-side (or account-side) outage — GQL, PubSub, OAuth, or the canary's
// watch transport failing. During an outage stalls are expected and must not
// confirm (the spec's "no active Twitch outage state" gate).
func (w *ProgressWatchdog) twitchOutage() (bool, string) {
	if w.center == nil {
		return false, ""
	}
	snap := w.center.Snapshot()
	for _, name := range []string{SignalOAuth, SignalGQLAPI, SignalPubSub, SignalWatchTransport} {
		// A degraded (flapping/repeatedly-failing) transport counts as an outage
		// here too: while the network is impaired, drop stalls are expected and
		// must not be confirmed against the streamer.
		if sig, ok := snap.Signal(name); ok && (sig.Status == StatusFailed || sig.Status == StatusDegraded) {
			return true, name
		}
	}
	return false, ""
}

// farmingChannel returns the slotted login whose streamer is assigned this
// campaign (game match + advertised campaign + channel allow-list, as encoded
// by the drops tracker's intersection), or "".
func (w *ProgressWatchdog) farmingChannel(campaign *models.Campaign) string {
	for _, slot := range w.watch.BrokerSnapshot().Slots {
		streamer := w.resolveStreamer(slot.Channel)
		if streamer == nil {
			continue
		}
		for _, c := range streamer.Stream.GetCampaigns() {
			if c.ID == campaign.ID {
				return slot.Channel
			}
		}
	}
	return ""
}

// evaluate is one watchdog pass over every tracked campaign's current drop:
// update progress evidence, check the stall gates, and — at most once per
// pass, to keep API load minimal — run the next recovery stage of the
// worst-off drop. Runs on the watchdog goroutine.
func (w *ProgressWatchdog) evaluate(now time.Time) {
	cfg := w.snapshotCfg()
	if !cfg.Enabled {
		w.mu.Lock()
		w.states = make(map[string]*dropState)
		w.mu.Unlock()
		w.publish(ProgressSnapshot{Enabled: false, EvaluatedAt: now})
		return
	}

	sync := w.drops.SyncStatus()
	outage, outageSignal := w.twitchOutage()

	seen := make(map[string]bool)
	var stageBudget bool // at most one recovery-stage execution per pass

	for _, campaign := range w.drops.Campaigns() {
		st, key, drop := w.trackDrop(campaign, sync, now)
		if st == nil {
			continue
		}
		seen[key] = true
		w.observeProgress(st, campaign, drop, sync, now)

		if hold, why := w.gatesHold(st, campaign, drop, sync, outage, outageSignal, cfg, now); !hold {
			// A gate failing means a stall cannot be *confirmed* right now. The
			// recovery stage and notification flags survive (a one-tick slot
			// rotation must not restart the pipeline), but the stall EVIDENCE is
			// discarded: whatever accrued while the gate failed does not prove
			// farming-without-credit, and carrying it over would confirm a stall
			// minutes after farming resumes.
			st.resetEvidence()
			if st.Status != ProgressStalled {
				st.Status = ProgressHealthy
			}
			st.Detail = why
			continue
		}

		if st.evidenceSince.IsZero() {
			// Every gate holds again: a fresh evidence window starts here. Seed
			// the observation cursor at the CURRENT sync timestamp so inventory
			// reads that completed before this moment (including the one whose
			// data showed the last progress) are never counted as no-progress
			// observations.
			st.evidenceSince = now
			st.lastObservedSyncAt = sync.ProgressLastSyncAt
			st.NoProgressObs = 0
		} else if sync.ProgressLastError == "" && !sync.ProgressLastSyncAt.IsZero() && sync.ProgressLastSyncAt.After(st.lastObservedSyncAt) {
			// A NEW inventory observation completed successfully inside the
			// evidence window without progress — "checked and unchanged", never
			// "could not check".
			st.lastObservedSyncAt = sync.ProgressLastSyncAt
			st.NoProgressObs++
		}

		stalled := now.Sub(st.evidenceSince) >= cfg.StallDelay &&
			st.NoProgressObs >= cfg.StallConfirmations &&
			st.ReportsSinceProgress >= stallMinReports
		if !stalled {
			if st.Status != ProgressStalled {
				st.Status = ProgressHealthy
				st.Detail = fmt.Sprintf("progress monitored: last advance %s ago, %s of farming evidence, %d clean observations, %d reports",
					now.Sub(st.LastProgressAt).Round(time.Minute), now.Sub(st.evidenceSince).Round(time.Minute),
					st.NoProgressObs, st.ReportsSinceProgress)
			}
			continue
		}

		if !stageBudget && w.advanceRecovery(st, cfg, now) {
			stageBudget = true
		}
	}

	// Drop state for campaigns/drops no longer tracked (claimed, claimable,
	// expired, campaign gone) — their episodes are over. An episode that
	// escalated must not leave dangling effects behind: the avoided channel
	// gets a clean slate, and a standing critical alert is explicitly closed
	// (a claimable/claimed drop means the stall resolved; an ended campaign
	// makes the alert moot either way).
	w.mu.Lock()
	for key, st := range w.states {
		if seen[key] {
			continue
		}
		if st.avoidedChannel != "" && w.avoid != nil {
			w.avoid.Clear(st.avoidedChannel)
		}
		if st.notifiedStalled && w.notifier != nil {
			w.notifier.NotifyDropRecovered(st.CampaignName, st.DropName, st.Channel,
				"the drop left the tracked set (claimed, claimable, or campaign ended) — the stall alert no longer applies")
		}
		delete(w.states, key)
	}
	w.mu.Unlock()

	w.publishFromStates(now)
}

// trackDrop finds (or creates) the watchdog state for the campaign's current
// drop. Returns nil when the campaign has nothing the watchdog should track.
func (w *ProgressWatchdog) trackDrop(campaign *models.Campaign, sync drops.SyncStatus, now time.Time) (*dropState, string, *models.Drop) {
	if campaign.Status != models.CampaignActive || now.After(campaign.EndAt) {
		return nil, "", nil
	}
	drop := campaign.CurrentDrop()
	if drop == nil || drop.IsClaimed || drop.IsClaimable || !drop.DateTimeMatch() {
		// Claimable means fully progressed — claiming is the claim flow's job,
		// not a stall.
		return nil, "", nil
	}

	key := campaign.ID + "\x00" + drop.ID
	w.mu.Lock()
	defer w.mu.Unlock()
	st, ok := w.states[key]
	if !ok {
		st = &dropState{DropProgress: DropProgress{
			CampaignID:     campaign.ID,
			CampaignName:   campaign.Name,
			DropID:         drop.ID,
			DropName:       drop.Name,
			LastMinutes:    drop.CurrentMinutesWatched,
			LastProgressAt: now,
			Status:         ProgressHealthy,
			Detail:         "tracking started",
		},
			// Seed the observation cursor so an inventory read that completed
			// BEFORE tracking began can never count as the first no-progress
			// observation.
			lastObservedSyncAt: sync.ProgressLastSyncAt,
		}
		w.states[key] = st
	}
	return st, key, drop
}

// observeProgress folds the latest campaign snapshot, inventory observation,
// and delivery accounting into the drop's state — including the healthy reset
// (and recovered notification) when minutes advanced.
func (w *ProgressWatchdog) observeProgress(st *dropState, campaign *models.Campaign, drop *models.Drop, sync drops.SyncStatus, now time.Time) {
	channel := w.farmingChannel(campaign)
	if channel == "" && st.Channel != "" && w.watch.IsWatching(st.Channel) {
		// The previous farming channel still holds a slot but the campaign
		// assignment vanished from it — keep the channel so gatesHold can name
		// the eligibility loss precisely instead of a generic "no channel".
		channel = st.Channel
	}
	if channel != st.statsChannel {
		// The farming channel changed (rotation, displacement, or our own
		// switch stage): re-baseline the delivery accounting against it.
		st.statsChannel = channel
		st.ReportsSinceProgress = 0
		st.baselineReports = 0
		if stats, ok := w.watch.ReportStats(channel); ok {
			st.baselineReports = stats.Successes
		}
	}
	st.Channel = channel

	if channel != "" {
		if stats, ok := w.watch.ReportStats(channel); ok {
			if n := stats.Successes - st.baselineReports; n >= 0 {
				st.ReportsSinceProgress = n
			}
		}
	}

	if drop.CurrentMinutesWatched > st.LastMinutes {
		recovered := st.notifiedStalled
		if recovered && w.notifier != nil {
			w.notifier.NotifyDropRecovered(st.CampaignName, st.DropName, channel,
				fmt.Sprintf("progress resumed: %d/%d minutes", drop.CurrentMinutesWatched, drop.MinutesRequired))
		}
		if st.Status == ProgressStalled || st.RecoveryStage > 0 {
			events.Record(events.TypeDropRecovered, channel, st.CampaignName+" / "+st.DropName)
		}
		if st.avoidedChannel != "" && w.avoid != nil {
			// The drop moves again; the excluded channel gets a clean slate.
			w.avoid.Clear(st.avoidedChannel)
		}
		*st = dropState{DropProgress: DropProgress{
			CampaignID:     st.CampaignID,
			CampaignName:   st.CampaignName,
			DropID:         st.DropID,
			DropName:       st.DropName,
			Channel:        channel,
			LastMinutes:    drop.CurrentMinutesWatched,
			LastProgressAt: now,
			Status:         ProgressHealthy,
			Detail:         fmt.Sprintf("progress advancing: %d/%d minutes", drop.CurrentMinutesWatched, drop.MinutesRequired),
		},
			statsChannel: channel,
			// Seed the observation cursor so the very sync whose data showed
			// this progress can never be re-counted as a no-progress
			// observation of the fresh episode. The evidence window itself
			// restarts in evaluate once the gates hold.
			lastObservedSyncAt: sync.ProgressLastSyncAt,
		}
		if stats, ok := w.watch.ReportStats(channel); ok {
			st.baselineReports = stats.Successes
		}
		return
	}

	st.LastMinutes = drop.CurrentMinutesWatched
	// No-progress observations are counted in evaluate, and only inside an
	// active evidence window (every gate holding) — see dropState.evidenceSince.
}

// gatesHold checks every stall-confirmation gate that is not a threshold:
// all must hold simultaneously or the stall is unconfirmable by design (the
// conservative, false-positive-averse core of the watchdog). Returns the
// human-readable reason of the first failing gate for explainability.
func (w *ProgressWatchdog) gatesHold(st *dropState, campaign *models.Campaign, drop *models.Drop, sync drops.SyncStatus, outage bool, outageSignal string, cfg WatchdogConfig, now time.Time) (bool, string) {
	if outage {
		return false, fmt.Sprintf("not counting a stall: Twitch connectivity is degraded (%s failing)", outageSignal)
	}
	// Inventory observability: a stall can only be confirmed while we can
	// actually SEE the drop's progress. A currently-failing progress sync, or
	// none completing within the stall-delay window, means "cannot check" —
	// Twitch may have credited the drop invisibly, so confirmation (and the
	// evidence clock) must wait for observability to return.
	if sync.ProgressLastError != "" {
		return false, "not counting a stall: inventory reads are currently failing, drop progress is unobservable"
	}
	if !sync.ProgressLastSyncAt.IsZero() && now.Sub(sync.ProgressLastSyncAt) > cfg.StallDelay {
		return false, "not counting a stall: no inventory observation completed within the stall-delay window"
	}
	if st.Channel == "" {
		return false, "no slotted channel is farming this campaign right now (rotation, offline, or waiting)"
	}
	if !w.watch.IsWatching(st.Channel) {
		return false, fmt.Sprintf("%s does not hold a watch slot right now", st.Channel)
	}

	streamer := w.resolveStreamer(st.Channel)
	if streamer == nil {
		return false, "farming channel is not resolvable to a live streamer object"
	}
	if campaign.Game != nil && streamer.Stream.GameID() != campaign.Game.ID {
		return false, fmt.Sprintf("%s switched away from %s — the campaign cannot progress there", st.Channel, campaign.Game.Name)
	}
	// Channel-side eligibility: the tracker's intersection (game + advertised
	// campaign + allow-list) must still assign this campaign to the channel.
	eligible := false
	for _, c := range streamer.Stream.GetCampaigns() {
		if c.ID == campaign.ID {
			eligible = true
			break
		}
	}
	if !eligible {
		return false, fmt.Sprintf("campaign is no longer assigned to %s (eligibility/intersection changed)", st.Channel)
	}
	if drop.HasPreconditionsMet != nil && !*drop.HasPreconditionsMet {
		return false, "drop preconditions not met on Twitch's side (previous drop or account link pending)"
	}
	return true, ""
}

// advanceRecovery runs the next recovery stage if the cooldown allows,
// returning true when a stage actually executed. The pipeline is finite; once
// exhausted the drop stays STALLED until progress resumes or Rearm elapses.
func (w *ProgressWatchdog) advanceRecovery(st *dropState, cfg WatchdogConfig, now time.Time) bool {
	if !st.exhaustedAt.IsZero() {
		if cfg.Rearm <= 0 || now.Sub(st.exhaustedAt) < cfg.Rearm {
			st.Status = ProgressStalled
			return false
		}
		// Re-arm: a fresh pipeline pass for a long-stalled drop.
		st.exhaustedAt = time.Time{}
		st.RecoveryStage = 0
	}
	if !st.LastRecoveryAt.IsZero() && now.Sub(st.LastRecoveryAt) < cfg.RecoveryCooldown {
		return false
	}
	if st.RecoveryStage >= len(recoveryStages) {
		st.Status = ProgressStalled
		return false
	}

	stage := recoveryStages[st.RecoveryStage]
	st.RecoveryStage++
	st.RecoveryStageName = stage.name
	st.LastRecoveryAt = now
	st.Detail = stage.run(w, st, now)
	if st.RecoveryStage >= len(recoveryStages) {
		st.Status = ProgressStalled
	} else {
		st.Status = ProgressRecovering
	}
	events.Record(events.TypeDropRecoveryStep, st.Channel,
		fmt.Sprintf("%s / %s: %s (stage %d/%d)", st.CampaignName, st.DropName, stage.label, st.RecoveryStage, len(recoveryStages)))
	return true
}

// publishFromStates rebuilds and publishes the immutable snapshot.
func (w *ProgressWatchdog) publishFromStates(now time.Time) {
	w.mu.Lock()
	list := make([]DropProgress, 0, len(w.states))
	for _, st := range w.states {
		list = append(list, st.DropProgress)
	}
	w.mu.Unlock()

	// Stable order: worst status first, then campaign name.
	rank := func(s string) int {
		switch s {
		case ProgressStalled:
			return 0
		case ProgressRecovering:
			return 1
		default:
			return 2
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if ri, rj := rank(list[i].Status), rank(list[j].Status); ri != rj {
			return ri < rj
		}
		return list[i].CampaignName < list[j].CampaignName
	})

	w.publish(ProgressSnapshot{Enabled: true, EvaluatedAt: now, Drops: list})
}

func (w *ProgressWatchdog) publish(s ProgressSnapshot) {
	w.snap.Store(&s)
}

// ProgressSignal composes the drops_progress health signal from the published
// snapshot: STALLED if any drop is stalled, OK with a recovering stage note if
// any is recovering, OK when all healthy, IDLE when nothing is tracked. The
// miner's health tick records it, keeping the Center single-writer.
func (w *ProgressWatchdog) ProgressSignal(now time.Time) Signal {
	snap := w.Snapshot()
	sig := Signal{Name: SignalDropsProgress, Status: StatusOK, CheckedAt: now}

	var stalled, recovering *DropProgress
	for i := range snap.Drops {
		switch snap.Drops[i].Status {
		case ProgressStalled:
			if stalled == nil {
				stalled = &snap.Drops[i]
			}
		case ProgressRecovering:
			if recovering == nil {
				recovering = &snap.Drops[i]
			}
		}
	}

	switch {
	case stalled != nil:
		sig.Status = StatusStalled
		sig.Detail = fmt.Sprintf("%q progress stalled on %s despite recovery", stalled.DropName, stalled.Channel)
		sig.ErrorCode = "drop_progress_stalled"
		sig.Stage = stalled.RecoveryStageName
	case recovering != nil:
		sig.Detail = fmt.Sprintf("%q stalled — automatic recovery running (%s)", recovering.DropName, recovering.RecoveryStageName)
		sig.Stage = "recovering:" + recovering.RecoveryStageName
	case len(snap.Drops) > 0:
		sig.Detail = fmt.Sprintf("%d drop(s) tracked, progress advancing normally", len(snap.Drops))
	default:
		sig.Status = StatusIdle
		sig.Detail = "no active drop campaign"
	}
	return sig
}
