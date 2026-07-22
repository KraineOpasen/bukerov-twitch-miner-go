package watcher

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/eligibility"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// onlineChecker is the slice of the Twitch client the watcher needs to
// re-verify a stale stream; narrowed to an interface so the broker's send loop
// can be tested with a fake. Satisfied by *api.TwitchClient.
type onlineChecker interface {
	CheckStreamerOnline(streamer *models.Streamer) models.StatusTransition
}

// minuteReporter abstracts MinuteSender so the loop can be exercised in tests
// without real HTTP. Satisfied by *MinuteSender.
type minuteReporter interface {
	Send(streamer *models.Streamer) (simulateErr error, err error)
}

// MinuteWatcher is the unified slot broker: the single owner of the (at most
// constants.MaxSimultaneousStreams) Twitch watch slots. It selects channels
// from the configured streamer list AND from registered candidate sources
// (directory discovery), then is the only component that drives MinuteSender.
// Directory discovery never sends minute-watched itself; it only proposes
// candidates the broker may place in a slot.
type MinuteWatcher struct {
	client     onlineChecker
	streamers  []*models.Streamer
	priorities []config.Priority
	settings   config.RateLimitSettings

	// pendingPriorities/pendingSettings/hasPending stage a runtime settings
	// update from UpdateSettings (any goroutine) under mu; the loop applies
	// them into priorities/settings at the start of the next tick. This keeps
	// priorities/settings loop-owned (read lock-free during selection) while
	// updates stay race-free.
	pendingPriorities []config.Priority
	pendingSettings   config.RateLimitSettings
	hasPending        bool

	// pendingStreamers/hasPendingStreamers stage a runtime replacement of the
	// configured streamer list (Settings-page add/remove) the same way:
	// UpdateStreamers (any goroutine) stages under mu, the loop swaps it into
	// the loop-owned streamers field at the start of the next tick — after
	// remapping every index-keyed piece of loop state (see applyStreamerList).
	pendingStreamers    []*models.Streamer
	hasPendingStreamers bool

	// sources supply extra watch candidates (e.g. directory discovery) that
	// compete for the same slots as the configured list. Guarded by mu; set at
	// startup and snapshotted at the start of each tick.
	sources []CandidateSource

	// store persists accumulated watch time per streamer so rotation
	// fairness survives restarts. May be nil (e.g. analytics disabled), in
	// which case rotation falls back to in-memory recency only.
	store *WatchTimeStore

	// rotation is only ever read/written from the loop() goroutine, so it
	// needs no locking of its own.
	rotation rotationState

	// streakDiag tracks, per streamer index, which watch-streak pursuit events
	// have already been logged for the current streak, so each is logged at
	// most once instead of every tick. Like rotation, it is only touched from
	// the loop() goroutine and needs no locking.
	streakDiag map[int]streakDiagState

	// selectionReasons and selectionMode are per-tick scratch state for the
	// debug snapshot, rebuilt on every processWatching pass. Like rotation,
	// they are only touched from the loop() goroutine; the copy other
	// goroutines may read is debugState below.
	selectionReasons map[int]string
	selectionMode    string

	// lastSlots is the previous tick's slot allocation (login -> reason code +
	// broadcast ID), loop-owned, used to log slot changes only when they
	// actually change. The broadcast ID is captured alongside the reason so a
	// "released" log can name the broadcast the slot was on, even though that
	// streamer is no longer in this tick's slots slice by then.
	lastSlots map[string]slotLogState

	// lastConfiguredWatched is the set of configured channels (login -> the
	// streamer that held the slot) that occupied a watch slot on the PREVIOUS
	// tick. Each tick, any configured channel watched last tick but NOT in this
	// tick's slots has genuinely lost its slot, so its continuous-watch accumulator
	// is reset (see resetLostSlotContinuity) — otherwise regaining the slot within
	// maxContinuousGap would credit the unwatched interval and reach the
	// streak-pursuit cap early. Loop-owned; login-keyed, so (like reportStats) it
	// carries no index and needs no remapping in applyStreamerList.
	lastConfiguredWatched map[string]*models.Streamer

	// displaceParity alternates the displacement victim in the pure
	// cold-start tie case (equal-rank configured occupants with no rotation
	// recency), so neither channel is starved for the whole uptime. Loop-owned;
	// only touched from pickDisplaceable during a processWatching tick.
	displaceParity uint64

	// debugState is the last published watch-decision snapshot, guarded by
	// debugMu because the debug HTTP endpoint reads it from its own goroutine.
	debugMu    sync.Mutex
	debugState DebugState

	// brokerSnapshot/watchingLogins publish the immutable slot allocation for
	// the dashboard, the debug endpoint, and discovery to read lock-free.
	brokerSnapshot atomic.Pointer[BrokerSnapshot]
	watchingLogins atomic.Pointer[map[string]bool]

	// refresher rebuilds a slotted channel's watch session (spade URL, stream
	// info, beacon payload) for the staged session-refresh requests. Set once at
	// construction; nil only in tests that never exercise refreshes.
	refresher sessionRefresher

	// pendingRefresh stages watch-session refresh requests from
	// RequestSessionRefresh (any goroutine) under mu, coalesced per login; the
	// loop drains and executes them at the start of each tick, keeping the loop
	// goroutine the single writer of slotted channels' watch sessions.
	pendingRefresh map[string]pendingRefresh

	// refreshOutcomes publishes the last session-refresh outcome per login for
	// the progress watchdog and the debug snapshot to read lock-free.
	refreshOutcomes atomic.Pointer[map[string]SessionRefreshOutcome]

	// reportStats is the loop-owned per-channel minute-watched delivery
	// accounting for currently slotted channels; reportStatsSnap is its
	// published immutable copy (see session.go).
	reportStats     map[string]ReportStats
	reportStatsSnap atomic.Pointer[map[string]ReportStats]

	// avoid, when set, temporarily excludes channels from watch selection (the
	// progress watchdog's channel-switch recovery stage). Guarded by mu;
	// snapshotted at the start of each tick.
	avoid AvoidChecker

	// campaignScores publishes the campaign-policy engine's per-login score
	// (higher = higher priority). The DROPS priority picker reads it lock-free
	// to break ties among competing drop streamers by the active policy mode;
	// nil (or an absent login) means no policy preference, preserving the
	// pre-policy order. Advisory only — it never overrides the slot cap or the
	// restricted-drop-first rule.
	campaignScores atomic.Pointer[map[string]int]

	// preferConfigured, when true, forbids a non-configured (discovery)
	// candidate from displacing a configured streamer that already holds a slot,
	// so tracked streamers always keep their slot and discovery only fills idle
	// ones. Set from any goroutine, read lock-free by the loop's pickDisplaceable.
	// Default false preserves the pre-existing rank-based arbitration. Advisory
	// only — it never lets discovery exceed the slot cap or take an occupied slot.
	preferConfigured atomic.Bool

	ctx    context.Context
	cancel context.CancelFunc
	// loopDone is closed when the watch loop goroutine exits; Stop waits on
	// it (bounded by stopJoinTimeout) so in-flight watch_time writes drain
	// before the database is closed.
	loopDone chan struct{}

	// sender performs the actual watch-minute reporting (playback token,
	// playlist touch, spade event). The broker is the sole caller of it.
	sender minuteReporter

	// pacer spaces the per-slot sends across the tick interval. nil uses the
	// default context-aware sleep with ±20% jitter; tests override it to avoid
	// real pauses. Returns false when the wait was interrupted by shutdown.
	pacer func(d time.Duration) bool

	// onMinuteWatched, if set, is invoked once after any watch tick that
	// successfully reported at least one minute-watched. The drops tracker uses
	// it to refresh drop progress promptly (a watched minute means real progress
	// was just made) instead of waiting out its sync interval. Guarded by mu.
	onMinuteWatched func()

	// lostMiningMinutes accumulates estimated "idle slot" watch time for the
	// daily summary: per tick, wall-clock minutes for slots that were fillable
	// (a live eligible candidate existed) but produced no watched minute this
	// tick. It counts only genuine lost capacity — a slot left empty because
	// nothing was online is NOT counted. It is an in-memory, process-lifetime
	// best-effort figure: LostMiningMinutes drains it, and a restart resets it.
	// Guarded by lostMu (a distinct lock so the daily-summary goroutine's drain
	// never contends with the loop's mu-protected state).
	lostMu            sync.Mutex
	lostMiningMinutes float64

	mu sync.RWMutex
}

// rotationState tracks the fair watch-pair rotation used when more streamers
// are online than Twitch allows to watch simultaneously
// (constants.MaxSimultaneousStreams). See selectRotating for the algorithm.
type rotationState struct {
	activePair [2]int // streamer indexes currently occupying the watch slots
	hasPair    bool   // whether activePair has been initialized yet

	lastSwitch   time.Time     // when activePair last changed
	nextInterval time.Duration // randomized dwell time chosen for the current activePair

	lastWatched map[int]time.Time // last tick each streamer index was actually watched (fairness tie-break + boost victim selection)
	deferredFor map[int]bool      // streamers whose scheduled swap-out was already postponed once

	// Boost latch: keep the SAME channel in the ephemeral DROPS/STREAK boost
	// seat (and displace the SAME base-pair member) across ticks, instead of
	// re-picking the least-recently-watched eligible channel every tick. Without
	// it, whenever 3+ online channels are boost-eligible the boost churned the
	// watched set on every tick, so no channel was ever watched on consecutive
	// ticks; the continuous viewing a watch streak (and drop progress) needs
	// never accumulated and MinuteWatched was perpetually reset to 0. The latch
	// yields immediately to a strictly higher-priority candidate (see
	// strictlyHigherBoost), so a channel-restricted drop can still preempt it.
	boostLatched bool
	boostTarget  int // off-pair streamer index currently holding the boost seat
	boostVictim  int // base-pair streamer index the boost displaced
}

// clearBoostLatch drops any sticky boost so the next tick re-picks a boost seat
// from scratch. Called whenever the base pair changes or rotation is left.
func (r *rotationState) clearBoostLatch() {
	r.boostLatched = false
	r.boostTarget = -1
	r.boostVictim = -1
}

// streakDiagState records which watch-streak pursuit log lines have already
// been emitted for a streamer's current (still-missing) streak.
type streakDiagState struct {
	pursuing bool // "Pursuing watch streak" already logged
	released bool // "released the boost seat (evidence/bounded window)" already logged
}

// watchStreakThresholdMinutes is the rough number of watched minutes by which
// Twitch usually grants a watch streak. It is NO LONGER a pursuit cutoff — a
// streak is confirmed only by the real WATCH_STREAK points event (see
// isBoostEligible / streakPursuitExhausted), because Twitch does not promise the
// grant at exactly seven minutes and often delivers it later. It survives only
// as a diagnostic reference for the streak progress display.
const watchStreakThresholdMinutes = 7.0

// streakExpectedGrantMinutes is roughly how many CONTINUOUSLY-watched minutes it
// normally takes Twitch to grant a still-earnable watch streak. It is a
// DIAGNOSTIC reference only, never a release trigger on its own: Twitch does not
// promise the grant at a fixed minute and frequently delivers it later. The boost
// seat is released only by the authoritative WATCH_STREAK grant or the bounded
// hard cap below (see streakPursuitExhausted).
const streakExpectedGrantMinutes = 15.0

// streakDeliveryGraceMinutes is the extra continuous-watch time the seat is held
// PAST the expected grant point, so a WATCH_STREAK notice that Twitch triggers on
// (or just after) the expected minute is still captured while the channel is
// actually being watched. The notice arrives asynchronously on the best-effort
// PubSub websocket, decoupled from the minute-watched HTTP report, so the very
// report that pushes the counter to the expected minute can be the one that makes
// Twitch grant the streak — releasing exactly there risks un-watching the channel
// the tick before the grant lands. This grace is our own conservative policy (NOT
// a documented Twitch timing), chosen because capturing the 300-450 streak reward
// outranks rotation speed. It stays bounded: it is continuous-watch time, which
// Stream.UpdateMinuteWatched / ResetWatchContinuity reset to zero on any break.
const streakDeliveryGraceMinutes = 5.0

// streakPursuitCapMinutes is the bounded HARD CAP that releases the streak boost
// seat when Twitch has still not granted the streak — the expected grant point
// plus the delivery grace. It is measured in CONTINUOUSLY-watched minutes
// (Stream.UpdateMinuteWatched resets on a viewing break; ResetWatchContinuity
// resets on a real slot loss), so it can only be reached by genuinely watching the
// channel that long without interruption — well past Twitch's ~10-minute stream
// minimum and the typical grant point. The authoritative WATCH_STREAK grant
// (StreakPending -> false) is the primary, faster release; this cap only bounds
// the "streak will never come" cases (first stream of the series, opted out,
// already earned, or a view Twitch isn't counting at all). Releasing only frees
// the seat: StreakPending stays true, so a LATE real WATCH_STREAK is still accepted
// and recorded exactly once.
const streakPursuitCapMinutes = streakExpectedGrantMinutes + streakDeliveryGraceMinutes

// StreakPursuitCapMinutes is the UI-facing hard pursuit cap, in continuously
// watched minutes: the bounded window (expected grant + delivery grace) after
// which the streak boost seat is released even when Twitch never granted. The web
// dashboard reads it as the watch-streak progress-bar denominator, so the UI and
// the watcher share one 20-minute source of truth instead of a hardcoded copy. It
// is the pursuit/watch window, NOT a promise that a reward is delivered at minute
// 20 — see streakPursuitCapMinutes above for the full semantics.
const StreakPursuitCapMinutes = streakPursuitCapMinutes

func NewMinuteWatcher(
	client *api.TwitchClient,
	streamers []*models.Streamer,
	priorities []config.Priority,
	settings config.RateLimitSettings,
	store *WatchTimeStore,
) *MinuteWatcher {
	return &MinuteWatcher{
		client:     client,
		streamers:  streamers,
		priorities: priorities,
		settings:   settings,
		store:      store,
		sender:     NewMinuteSender(client),
		refresher:  client,
	}
}

// AddSource registers a candidate source (e.g. directory discovery) whose
// proposed channels compete for the same watch slots as the configured list.
// Call before Start. Safe for concurrent use.
func (w *MinuteWatcher) AddSource(src CandidateSource) {
	if src == nil {
		return
	}
	w.mu.Lock()
	w.sources = append(w.sources, src)
	w.mu.Unlock()
}

// stopJoinTimeout bounds how long Stop waits for the watch loop to drain its
// in-flight tick (which may be writing watch_time rows) before giving up so a
// hung loop can never block shutdown indefinitely. Package variable so tests
// can shrink it.
var stopJoinTimeout = 5 * time.Second

func (w *MinuteWatcher) Start(ctx context.Context) {
	w.mu.Lock()
	w.ctx, w.cancel = context.WithCancel(ctx)
	done := make(chan struct{})
	w.loopDone = done
	w.mu.Unlock()

	go func() {
		defer close(done)
		w.loop()
	}()
}

// Stop cancels the watch loop and waits (bounded by stopJoinTimeout) for it
// to finish, so an in-flight tick's watch_time write completes before the
// caller proceeds to close the database.
func (w *MinuteWatcher) Stop() {
	w.mu.Lock()
	if w.cancel != nil {
		w.cancel()
	}
	done := w.loopDone
	w.mu.Unlock()

	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(stopJoinTimeout):
		slog.Warn("Watcher loop did not finish within the stop timeout; proceeding with shutdown", "timeout", stopJoinTimeout)
	}
}

// SetOnMinuteWatched registers a callback invoked after each watch tick that
// successfully reported at least one minute-watched. Pass nil to clear it.
func (w *MinuteWatcher) SetOnMinuteWatched(fn func()) {
	w.mu.Lock()
	w.onMinuteWatched = fn
	w.mu.Unlock()
}

// UpdateSettings stages a runtime priority/rate-limit change. It is applied by
// the loop goroutine at the start of the next tick (see applyPendingSettings),
// so priorities/settings stay loop-owned and readable without locking during
// selection, while the update itself is race-free.
func (w *MinuteWatcher) UpdateSettings(priorities []config.Priority, settings config.RateLimitSettings) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pendingPriorities = priorities
	w.pendingSettings = settings
	w.hasPending = true
}

// UpdateStreamers stages a runtime replacement of the configured streamer
// list (Settings-page add/remove). The loop goroutine applies it at the start
// of the next tick (see applyPendingSettings), so streamers stays loop-owned
// and readable without locking during selection. Two calls before a tick are
// last-write-wins: only the newest list is ever applied. The slice is copied
// so later caller-side mutations cannot reach loop state. A removed streamer
// is released softly: it simply stops being a slot candidate on the next
// tick, and the normal per-tick selection reassigns its slot.
func (w *MinuteWatcher) UpdateStreamers(streamers []*models.Streamer) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pendingStreamers = append([]*models.Streamer(nil), streamers...)
	w.hasPendingStreamers = true
}

// applyPendingSettings moves any staged runtime settings into the loop-owned
// priorities/settings fields. Runs on the loop goroutine at the start of each
// tick; also snapshots the registered sources and the avoid checker.
func (w *MinuteWatcher) applyPendingSettings() ([]CandidateSource, AvoidChecker) {
	w.mu.Lock()
	if w.hasPending {
		w.priorities = w.pendingPriorities
		w.settings = w.pendingSettings
		w.hasPending = false
	}
	var stagedStreamers []*models.Streamer
	applyStreamers := false
	if w.hasPendingStreamers {
		stagedStreamers = w.pendingStreamers
		w.pendingStreamers = nil
		w.hasPendingStreamers = false
		applyStreamers = true
	}
	sources := append([]CandidateSource(nil), w.sources...)
	avoid := w.avoid
	w.mu.Unlock()

	// The swap itself happens outside mu: streamers and the rotation/streak
	// state are loop-owned, and this runs on the loop goroutine.
	if applyStreamers {
		w.applyStreamerList(stagedStreamers)
	}
	return sources, avoid
}

// applyStreamerList replaces the loop-owned streamer list and remaps every
// index-keyed piece of loop state — rotation pair, boost latch, fairness
// recency (lastWatched), swap-out deferrals, and streak log bookkeeping —
// from old indexes to new ones by username. Entries whose streamer left the
// list are dropped. If a rotation-pair or boost seat member was removed, that
// state is reset so the next selection recomputes it from scratch; per-tick
// scratch (selectionReasons) needs nothing, it is rebuilt every tick.
// Username-keyed state (reportStats, pendingRefresh, lastConfiguredWatched,
// WatchTimeStore rows) is index-free and intentionally untouched.
func (w *MinuteWatcher) applyStreamerList(newList []*models.Streamer) {
	oldList := w.streamers
	newIndexByLogin := make(map[string]int, len(newList))
	for i, s := range newList {
		newIndexByLogin[s.Username] = i
	}
	translate := func(oldIdx int) (int, bool) {
		if oldIdx < 0 || oldIdx >= len(oldList) {
			return -1, false
		}
		newIdx, ok := newIndexByLogin[oldList[oldIdx].Username]
		return newIdx, ok
	}

	if len(w.rotation.lastWatched) > 0 {
		remapped := make(map[int]time.Time, len(w.rotation.lastWatched))
		for oldIdx, at := range w.rotation.lastWatched {
			if newIdx, ok := translate(oldIdx); ok {
				remapped[newIdx] = at
			}
		}
		w.rotation.lastWatched = remapped
	}
	if len(w.rotation.deferredFor) > 0 {
		remapped := make(map[int]bool, len(w.rotation.deferredFor))
		for oldIdx, deferred := range w.rotation.deferredFor {
			if newIdx, ok := translate(oldIdx); ok {
				remapped[newIdx] = deferred
			}
		}
		w.rotation.deferredFor = remapped
	}
	if len(w.streakDiag) > 0 {
		remapped := make(map[int]streakDiagState, len(w.streakDiag))
		for oldIdx, state := range w.streakDiag {
			if newIdx, ok := translate(oldIdx); ok {
				remapped[newIdx] = state
			}
		}
		w.streakDiag = remapped
	}

	if w.rotation.hasPair {
		a, okA := translate(w.rotation.activePair[0])
		b, okB := translate(w.rotation.activePair[1])
		if okA && okB {
			w.rotation.activePair = [2]int{a, b}
		} else {
			// A pair member was removed: drop the pair (and the boost seat that
			// references it) so this tick's selection recomputes both.
			w.rotation.hasPair = false
			w.rotation.clearBoostLatch()
		}
	}
	if w.rotation.boostLatched {
		target, okT := translate(w.rotation.boostTarget)
		victim, okV := translate(w.rotation.boostVictim)
		if okT && okV {
			w.rotation.boostTarget = target
			w.rotation.boostVictim = victim
		} else {
			w.rotation.clearBoostLatch()
		}
	}

	w.streamers = newList
}

func (w *MinuteWatcher) randomizedDelay(base time.Duration) time.Duration {
	jitter := (rand.Float64() - 0.5) * 0.4
	return time.Duration(float64(base) * (1.0 + jitter))
}

// pace waits between two per-slot sends, spreading them across the tick
// interval (with ±20% jitter) while remaining responsive to shutdown. Returns
// false if the context was cancelled during the wait, so the send loop stops.
func (w *MinuteWatcher) pace(d time.Duration) bool {
	if w.pacer != nil {
		return w.pacer(d)
	}
	select {
	case <-w.ctx.Done():
		return false
	case <-time.After(w.randomizedDelay(d)):
		return true
	}
}

func (w *MinuteWatcher) loop() {
	for {
		select {
		case <-w.ctx.Done():
			return
		default:
		}

		tickStart := time.Now()
		w.processWatching()

		// processWatching already spreads this tick's per-slot sends across
		// roughly one interval (pace(interval/len(slots)) after each slot), so a
		// continuously-watched channel is reported about once per interval. Sleep
		// only the REMAINDER of the interval here, never a second full one:
		// otherwise the effective per-channel cadence would be ~2×interval, which
		// sits right on the watch-streak continuity threshold
		// (maxContinuousGap = 2×interval, see processWatching) and halves the
		// drop-progress heartbeat rate. When processWatching returned early
		// without pacing (no slots watched), the elapsed time is ~0 and this waits
		// a full jittered interval, so the loop never busy-spins. Jitter is
		// preserved — it now lives on this single wait instead of being duplicated.
		interval := time.Duration(w.settings.MinuteWatchedInterval) * time.Second
		remaining := w.randomizedDelay(interval) - time.Since(tickStart)
		if remaining <= 0 {
			continue
		}
		select {
		case <-w.ctx.Done():
			return
		case <-time.After(remaining):
		}
	}
}

func (w *MinuteWatcher) processWatching() {
	sources, avoid := w.applyPendingSettings()

	w.selectionReasons = make(map[int]string)
	w.selectionMode = ModeIdle
	now := time.Now()

	onlineStreamers := w.getOnlineStreamers(avoid)

	// Re-verify stale streams (network) before selecting.
	for _, idx := range onlineStreamers {
		if w.client != nil && w.streamers[idx].Stream.UpdateElapsed() > 10*time.Minute {
			w.client.CheckStreamerOnline(w.streamers[idx])
		}
	}

	// Phase A: pick from the configured streamer list with the unchanged
	// priority/rotation logic. Phase B: layer external candidates (directory
	// discovery) on top and enforce the global MaxSimultaneousStreams cap.
	var configuredWatch []int
	if len(onlineStreamers) > 0 {
		configuredWatch = w.selectStreamersToWatch(onlineStreamers)
	}
	extra := w.gatherCandidates(sources, avoid)
	slots, waiting := w.arbitrate(configuredWatch, extra, now)

	// The per-streamer debug state reflects the FINAL configured-watched set
	// (a pick displaced by a higher-priority discovery drop is reported as not
	// watched); the broker snapshot is the explainable slot allocation.
	w.publishDebugState(configuredWatchedIndexes(slots), w.selectionMode)
	w.publishBrokerSnapshot(slots, waiting, now)
	w.logSlotChanges(slots)

	// Break continuous-watch accumulation for any configured channel that just
	// lost its watch slot, so a slot regained within maxContinuousGap does not
	// credit the unwatched interval toward the streak-pursuit cap (see
	// resetLostSlotContinuity). Runs before the no-slots early return below, since
	// losing every slot is itself a continuity break.
	w.resetLostSlotContinuity(slots)

	// Execute any staged watch-session refreshes before the sends, so a
	// successful refresh takes effect for this very tick. Requests for channels
	// that lost their slot complete as skipped.
	w.executeSessionRefreshes(slots)

	interval := time.Duration(w.settings.MinuteWatchedInterval) * time.Second

	// Slots that could have been productively filled this tick (granted slots
	// plus channels that contended for one). Used to estimate lost mining time.
	fillable := len(slots) + len(waiting)

	if len(slots) == 0 {
		// No slots granted: any contender that didn't get one is lost capacity.
		w.accrueLostMining(fillable, 0, interval)
		w.publishReportStats(slots)
		return
	}

	var watchingNames []string
	for _, sl := range slots {
		watchingNames = append(watchingNames, sl.streamer.Username)
	}
	slog.Debug("Watching streams", "count", len(slots), "max", constants.MaxSimultaneousStreams, "streamers", watchingNames)

	sleepBetween := interval / time.Duration(len(slots))

	// A continuously-watched streamer is reported once per loop, so consecutive
	// reports land ~interval apart. Anything past twice that means it lost its
	// watch slot for at least a cycle - a break in continuity that resets the
	// watch-streak progress (see Stream.UpdateMinuteWatched).
	maxContinuousGap := 2 * interval

	reported := false
	watchedOK := 0
	for _, sl := range slots {
		streamer := sl.streamer

		if err := w.sendMinuteWatched(streamer); err != nil {
			w.noteReportOutcome(streamer.Username, false, time.Now())
			slog.Debug("Failed to send minute watched", "streamer", streamer.Username, "origin", sl.origin, "error", err)
			// A failed send usually means the stream just ended; re-check the
			// online state so the next tick drops or switches it (and, for a
			// discovery channel, so discovery's own maintenance abandons it).
			if w.client != nil {
				w.client.CheckStreamerOnline(streamer)
			}
		} else {
			reported = true
			watchedOK++
			w.noteReportOutcome(streamer.Username, true, time.Now())
			slog.Debug("Sent minute watched", "streamer", streamer.Username, "origin", sl.origin, "minutesWatched", streamer.Stream.GetMinuteWatched())
			delta := streamer.Stream.UpdateMinuteWatched(maxContinuousGap)
			if sl.idx >= 0 {
				// Configured channel: credit fair-rotation watch time and track
				// streak pursuit. Discovery channels are intentionally excluded
				// from the fairness store and streak accounting.
				if w.store != nil && delta > 0 {
					if err := w.store.RecordMinutes(streamer.Username, delta, time.Now()); err != nil {
						slog.Debug("Failed to record watch time", "streamer", streamer.Username, "error", err)
					}
				}
				w.noteStreakProgress(sl.idx)
			}
		}

		if !w.pace(sleepBetween) {
			return
		}
	}

	w.publishReportStats(slots)

	// Estimate lost mining time for this tick: of the fillable slots, how many
	// produced no watched minute (a granted slot whose send failed while a live
	// candidate existed). Empty slots with no candidate are not counted.
	w.accrueLostMining(fillable, watchedOK, interval)

	// A watched minute means real drop progress was just made; nudge any
	// listener (the drops tracker) to refresh promptly instead of waiting out
	// its sync interval.
	if reported {
		w.mu.RLock()
		hook := w.onMinuteWatched
		w.mu.RUnlock()
		if hook != nil {
			hook()
		}
	}
}

// gatherCandidates collects the proposed channels from every registered
// source, dropping any channel that is on the configured streamer list (the
// broker is the single owner of duplicate-channel prevention across all
// sources), already proposed by an earlier source, or temporarily avoided by
// the progress watchdog (defense in depth - discovery filters avoided
// channels itself, but the broker enforces the exclusion regardless of
// source behavior).
func (w *MinuteWatcher) gatherCandidates(sources []CandidateSource, avoid AvoidChecker) []Candidate {
	if len(sources) == 0 {
		return nil
	}
	configured := make(map[string]bool, len(w.streamers))
	for _, s := range w.streamers {
		configured[s.Username] = true
	}
	var out []Candidate
	seen := make(map[string]bool)
	for _, src := range sources {
		for _, c := range src.WatchCandidates() {
			if c.Streamer == nil {
				continue
			}
			login := c.Streamer.Username
			if configured[login] || seen[login] {
				continue
			}
			if avoid != nil && avoid.IsAvoided(login) {
				continue
			}
			seen[login] = true
			if c.Origin == "" {
				c.Origin = src.SourceName()
			}
			out = append(out, c)
		}
	}
	return out
}

// unknownSlotRetentionGrace bounds how long a channel that was being watched and
// then went UNKNOWN (an online→unknown transient check failure) may keep its watch
// slot without a fresh confirmation. Within the grace its slot and continuous-watch
// accumulator are preserved so a network blip doesn't drop a live drop mid-campaign;
// past it the slot is released to a confirmed-online channel so a permanently-stuck
// unknown (a dead connection) can never pin a slot indefinitely. A failed
// minute-watched send or the stale re-check resolves most cases well within it.
const unknownSlotRetentionGrace = 2 * time.Minute

func (w *MinuteWatcher) getOnlineStreamers(avoid AvoidChecker) []int {
	var online []int
	for i, s := range w.streamers {
		confirmed := s.GetIsOnline()
		// A streamer that just went online→unknown while holding a slot stays a
		// candidate through the blip (continuity retention); it never lets an
		// unknown channel claim a NEW slot — only keep an existing one.
		retained := !confirmed && w.retainsSlotWhileUnknown(s)
		if !confirmed && !retained {
			continue
		}
		// DisableWatch is a hard opt-out: the streamer stays tracked and
		// online for display, but never becomes a watch-slot candidate -
		// even when it's the only online channel (unlike PreferenceAvoid).
		if s.GetSettings().DisableWatch {
			w.noteSelection(i, "watching disabled for this streamer in its settings")
			continue
		}
		// A temporary watchdog avoid works like DisableWatch, but expires on
		// its own: the progress watchdog excludes a channel whose drop
		// progress stalled despite session recovery, so the broker switches
		// to the next eligible channel instead.
		if avoid != nil && avoid.IsAvoided(s.Username) {
			w.noteSelection(i, "temporarily avoided by the drop-progress watchdog (stalled progress recovery)")
			continue
		}
		if retained {
			w.noteSelection(i, "status unconfirmed - retaining the current watch slot and continuity during a transient check failure")
			online = append(online, i)
			continue
		}
		// Capability gate for a NEW slot, via the single centralized policy
		// (eligibility.SlotCandidateEligible): a channel earns a new watch slot
		// only with a confirmed-useful task - an active eligible Drops entitlement,
		// OR a points task whose Channel Points capability is confirmed Enabled. A
		// points-only channel whose capability is Disabled OR merely Unknown gets
		// no new slot (unknown is never a basis to grant one, and never coerced to
		// enabled); Drops are evaluated independently, so a disabled/unknown-points
		// channel with a live drop still qualifies. Retained (BKM-002 continuity)
		// slots handled above bypass this gate, so an in-progress session is never
		// dropped by it.
		// The Drops input is the production-evaluated eligible-assignment signal
		// (HasEligibleAssignedDropCampaign), NOT the stale DropsCondition: a bare
		// advertised campaign ID no longer earns a slot - the drops tracker must
		// have actually assigned an eligible campaign (active entitlement, not
		// claimed, feasible, coherent ACL, allowed channel, confirmed availability).
		// Points capability still grants a slot independently.
		if ok, reason := watcherEligibility.SlotCandidateEligible(s, s.HasEligibleAssignedDropCampaign()); !ok {
			w.noteSelection(i, "not eligible for a new watch slot ("+string(reason)+")")
			continue
		}
		if s.GetOnlineAt().IsZero() || time.Since(s.GetOnlineAt()) > 30*time.Second {
			online = append(online, i)
		} else {
			w.noteSelection(i, "went online less than 30s ago - waiting for the stream to settle before watching")
		}
	}
	return online
}

// retainsSlotWhileUnknown reports whether a currently-UNKNOWN streamer should stay
// a watch candidate this tick to preserve an in-progress session. It requires all
// of: the streamer is unknown but was last confirmed ONLINE (an online→unknown
// blip, not initial-unknown or a confirmed-offline channel); it actually held a
// configured watch slot on the previous tick (so this only ever RETAINS a slot,
// never grants a new one); and the uncertainty is recent (bounded by
// unknownSlotRetentionGrace so a stuck unknown eventually releases the slot). An
// authoritative offline (GQL stream:null or a PubSub stream-down) sets StatusOffline
// and ends retention immediately.
func (w *MinuteWatcher) retainsSlotWhileUnknown(s *models.Streamer) bool {
	if s.GetStatus() != models.StatusUnknown || s.GetLastConfirmedStatus() != models.StatusOnline {
		return false
	}
	if _, held := w.lastConfiguredWatched[s.Username]; !held {
		return false
	}
	since := s.GetUnknownSince()
	return !since.IsZero() && time.Since(since) < unknownSlotRetentionGrace
}

// watcherEligibility is the single centralized eligibility policy used for
// new-slot candidacy (SlotCandidateEligible). It is stateless (system clock);
// there is deliberately no second, divergent capability policy in the watcher.
var watcherEligibility = eligibility.Evaluator{}

// selectStreamersToWatch picks which online streamers to send minute-watched
// events for. Twitch only credits watch time for up to
// constants.MaxSimultaneousStreams (2) channels at once.
//
// With 2 or fewer online streamers there's nothing to choose between: watch
// all of them, exactly as before this rotation feature existed.
//
// With more than 2 online, a fixed top-2-by-priority pick would starve every
// other online channel indefinitely, so instead we rotate the watched pair
// across all online streamers over time (selectRotating), with DROPS/STREAK
// only influencing how often a channel gets an extra turn - never granting
// it a permanent exclusive slot.
func (w *MinuteWatcher) selectStreamersToWatch(onlineIndexes []int) []int {
	candidates := w.filterAvoided(onlineIndexes)
	if len(candidates) <= constants.MaxSimultaneousStreams {
		// Not enough online streamers to need rotation; drop any stale pair
		// so a fresh one is computed next time we go above the limit.
		w.rotation.hasPair = false
		w.rotation.clearBoostLatch()
		w.selectionMode = ModeDirect
		return w.selectByPriority(candidates)
	}
	w.selectionMode = ModeRotation
	return w.selectRotating(candidates)
}

// filterAvoided drops streamers marked PreferenceAvoid from the candidate
// set, unless doing so would leave nothing to watch (e.g. the only online
// channel is marked avoid) - avoid excludes a channel from active watching
// except when it's the only online channel at all.
func (w *MinuteWatcher) filterAvoided(onlineIndexes []int) []int {
	if len(onlineIndexes) <= 1 {
		return onlineIndexes
	}

	filtered := make([]int, 0, len(onlineIndexes))
	var avoided []string
	for _, idx := range onlineIndexes {
		if w.streamers[idx].Settings.Preference == models.PreferenceAvoid {
			avoided = append(avoided, w.streamers[idx].Username)
			w.noteSelection(idx, `excluded from watching: preference is set to "avoid" and other channels are online`)
			continue
		}
		filtered = append(filtered, idx)
	}

	if len(filtered) == 0 {
		// Every online streamer is marked avoid - watching something is
		// still required, so the exclusion is lifted entirely.
		for _, idx := range onlineIndexes {
			w.noteSelection(idx, `"avoid" preference ignored: every online channel is marked avoid, so something must still be watched`)
		}
		return onlineIndexes
	}
	if len(avoided) > 0 {
		slog.Debug("Excluding avoided streamers from watch selection", "avoided", avoided)
	}
	return filtered
}

// isPreferred reports whether the streamer at idx is marked PreferencePrefer.
func (w *MinuteWatcher) isPreferred(idx int) bool {
	return w.streamers[idx].Settings.Preference == models.PreferencePrefer
}

// selectRotating implements the fair watch-pair rotation for the case where
// more streamers are online than fit in a watch slot.
//
// Weighted base pair (fairness guarantee):
// Every RotationInterval (a duration randomized within
// [RotationIntervalMinMinutes, RotationIntervalMaxMinutes], redrawn on every
// actual switch so rotations don't settle into a single predictable
// period), the pair is recomputed from scratch: online streamers are ranked
// by their accumulated watch minutes over the trailing watchTimeWindow
// (persisted in SQLite, see store.go), ascending, and the two with the
// least get the slots. Ties (most commonly at cold start, when nobody has
// any recorded watch time yet) are broken by in-memory recency
// (least-recently-watched first) and finally by index for determinism.
//
// This is a deficit-based scheduler: whoever gets watched accumulates
// minutes and becomes less eligible next time, so the ranking naturally
// surfaces every other online channel over time regardless of the total
// count or its parity - no even/odd special-casing is needed, unlike a
// fixed round-robin schedule.
//
// Priority as weight, not exclusivity:
// On top of the ranked base pair, any online streamer with an active drop
// (DropsCondition) or a watch streak in progress (mirroring the existing
// STREAK/DROPS priority conditions) can take over one seat in the pair -
// without affecting the ranking above. The seat sacrificed is whichever of the
// two base-pair members was watched most recently, so the other keeps its slot.
// The displaced member simply ranks for its turn again next time the base pair
// is recomputed, so no channel is permanently exclusive and no channel is ever
// locked out.
//
// Continuity latch: the boosted channel and the seat it displaces are held
// across ticks (see applyPriorityBoost), NOT re-chosen every tick. A watch
// streak (and unrestricted drop progress) is only credited for continuous
// viewing, so the watched set must stay stable minute-over-minute for it to
// accumulate. Re-picking the least-recently-watched eligible channel every
// tick rotated the watched set on every tick whenever 3+ channels were
// eligible, breaking that continuity so no streak ever completed; the latch
// keeps the same channel boosted until it stops being eligible or a strictly
// higher-priority candidate appears, then hands the seat off.
//
// Avoiding last-second interruptions:
// When the base pair is about to rotate a streamer out, if that streamer is
// still actively pursuing a watch streak (nearStreakCompletion / streakInProgress
// — pending, watched, not exhausted), the swap is postponed by a short fixed
// delay so it isn't yanked mid-pursuit. This is a best-effort heuristic based
// only on watch-streak state - imminent drop-campaign completion isn't tracked
// here (that would require deeper integration with the drops package) and can
// still cause a mid-progress interruption; see PR description for this known
// limitation.
// Each streamer can only have its swap-out postponed once per approach, so
// this can never stall the rotation indefinitely.
func (w *MinuteWatcher) selectRotating(onlineIndexes []int) []int {
	now := time.Now()

	needsNewPair := !w.rotation.hasPair ||
		!containsPair(onlineIndexes, w.rotation.activePair) ||
		now.Sub(w.rotation.lastSwitch) >= w.rotation.nextInterval

	if needsNewPair {
		w.rotateToLeastWatchedPair(onlineIndexes, now)
	}

	pair := w.applyPriorityBoost(w.rotation.activePair, onlineIndexes)

	for _, idx := range pair {
		w.noteSelectionIfEmpty(idx, "watched: holds a rotation slot (had the least accumulated watch time when the pair was last recomputed)")
	}

	if w.rotation.lastWatched == nil {
		w.rotation.lastWatched = make(map[int]time.Time)
	}
	w.rotation.lastWatched[pair[0]] = now
	w.rotation.lastWatched[pair[1]] = now

	return []int{pair[0], pair[1]}
}

// streakDeferDelay is the short wait used to postpone a swap-out that would
// otherwise interrupt a streamer actively pursuing its watch streak -
// deliberately much shorter than the rotation interval itself, and applied at
// most once per approach (see rotateToLeastWatchedPair), so it only bridges a
// brief window and can never stall the rotation.
const streakDeferDelay = 2 * time.Minute

// preferenceWeightBiasMinutes is the fixed handicap (in accumulated watch
// minutes) applied in favor of PreferencePrefer streamers when ranking the
// base rotation pair. It's deliberately small relative to typical rotation
// windows so it only breaks near-ties instead of overriding the fairness
// guarantee.
const preferenceWeightBiasMinutes = 5.0

// rotateToLeastWatchedPair recomputes the base pair from accumulated watch
// time, unless doing so would interrupt a leaving streamer's near-complete
// watch streak (see selectRotating's doc comment).
func (w *MinuteWatcher) rotateToLeastWatchedPair(onlineIndexes []int, now time.Time) {
	weights := w.watchWeights(onlineIndexes, now)

	candidates := make([]int, len(onlineIndexes))
	copy(candidates, onlineIndexes)
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		// A preferred streamer is treated as if it had watched slightly less
		// than it actually has, tipping the ranking in its favor without
		// overriding the fairness guarantee: it still accumulates real watch
		// time and falls back in line once the gap exceeds this handicap.
		wa, wb := weights[a], weights[b]
		if w.isPreferred(a) {
			wa -= preferenceWeightBiasMinutes
		}
		if w.isPreferred(b) {
			wb -= preferenceWeightBiasMinutes
		}
		if wa != wb {
			return wa < wb
		}
		la, lb := w.rotation.lastWatched[a], w.rotation.lastWatched[b]
		if !la.Equal(lb) {
			return la.Before(lb)
		}
		return a < b
	})
	newPair := [2]int{candidates[0], candidates[1]}

	// Only consider postponing the swap if the current pair is still fully
	// online: if a member already went offline, there's nothing to protect
	// (its streak is already lost) and it must not linger in activePair.
	if w.rotation.hasPair && containsPair(onlineIndexes, w.rotation.activePair) {
		for _, idx := range w.rotation.activePair {
			if idx == newPair[0] || idx == newPair[1] {
				continue
			}
			if w.nearStreakCompletion(idx) && !w.rotation.deferredFor[idx] {
				if w.rotation.deferredFor == nil {
					w.rotation.deferredFor = make(map[int]bool)
				}
				w.rotation.deferredFor[idx] = true
				w.rotation.lastSwitch = now
				w.rotation.nextInterval = streakDeferDelay
				w.noteSelection(idx, "watched: rotation swap-out postponed - within minutes of completing its watch streak")
				return
			}
		}

		for _, idx := range newPair {
			delete(w.rotation.deferredFor, idx)
		}
	}

	oldPair := w.rotation.activePair
	hadPair := w.rotation.hasPair
	changed := !hadPair || newPair != oldPair

	w.rotation.activePair = newPair
	w.rotation.hasPair = true
	w.rotation.lastSwitch = now
	w.rotation.nextInterval = w.randomRotationInterval()

	if changed {
		// A fresh base pair invalidates the sticky boost seat/victim, which
		// referenced the previous pair; recompute the boost from scratch.
		w.rotation.clearBoostLatch()
		w.logPairChange(oldPair, hadPair, newPair)
	}
}

// logPairChange emits an INFO log whenever the fair-rotation watch pair is
// recomputed, so the switch is visible in production logs (previously this was
// a DEBUG line and never showed at the default INFO console level). It reports
// exactly which member left and which took its place, plus the reason: a
// prefer-weighted streamer tipping the ranking ("prefer weight") versus the
// plain least-accumulated-watch-time ordering ("fair rotation"). Transient
// DROPS/STREAK boosts (applyPriorityBoost) don't change the stored pair and so
// aren't reported here — they'd fire every tick.
func (w *MinuteWatcher) logPairChange(oldPair [2]int, hadPair bool, newPair [2]int) {
	newNames := []string{w.streamers[newPair[0]].Username, w.streamers[newPair[1]].Username}

	var preferred []string
	for _, idx := range newPair {
		if w.isPreferred(idx) {
			preferred = append(preferred, w.streamers[idx].Username)
		}
	}

	reason := "fair rotation"
	if len(preferred) > 0 {
		reason = "prefer weight"
	}

	if !hadPair {
		slog.Info("Rotating watch pair", "pair", newNames, "reason", "initial pair")
		return
	}

	var swappedIn, swappedOut []string
	for _, idx := range newPair {
		if idx != oldPair[0] && idx != oldPair[1] {
			swappedIn = append(swappedIn, w.streamers[idx].Username)
		}
	}
	for _, idx := range oldPair {
		if idx != newPair[0] && idx != newPair[1] {
			swappedOut = append(swappedOut, w.streamers[idx].Username)
		}
	}

	attrs := []any{
		"pair", newNames,
		"swappedIn", swappedIn,
		"swappedOut", swappedOut,
		"reason", reason,
	}
	if len(preferred) > 0 {
		attrs = append(attrs, "preferred", preferred)
	}
	slog.Info("Rotating watch pair", attrs...)
}

// watchWeights returns each online streamer's accumulated watch minutes
// over the trailing window, used to rank who's most "owed" a turn. Streamers
// absent from the store's response (including when store is nil, e.g.
// analytics disabled) are treated as 0.
func (w *MinuteWatcher) watchWeights(onlineIndexes []int, now time.Time) map[int]float64 {
	weights := make(map[int]float64, len(onlineIndexes))
	if w.store == nil {
		return weights
	}

	usernames := make([]string, len(onlineIndexes))
	for i, idx := range onlineIndexes {
		usernames[i] = w.streamers[idx].Username
	}

	minutes, err := w.store.WindowMinutes(usernames, now)
	if err != nil {
		slog.Debug("Failed to load watch-time window", "error", err)
		return weights
	}

	for _, idx := range onlineIndexes {
		weights[idx] = minutes[w.streamers[idx].Username]
	}
	return weights
}

// randomRotationInterval draws a dwell time uniformly from
// [RotationIntervalMinMinutes, RotationIntervalMaxMinutes].
func (w *MinuteWatcher) randomRotationInterval() time.Duration {
	minMin := w.settings.RotationIntervalMinMinutes
	maxMin := w.settings.RotationIntervalMaxMinutes
	if minMin <= 0 {
		minMin = 30
	}
	if maxMin < minMin {
		maxMin = minMin
	}

	minutes := minMin
	if span := maxMin - minMin; span > 0 {
		minutes += rand.Intn(span + 1)
	}
	return time.Duration(minutes) * time.Minute
}

func containsPair(online []int, pair [2]int) bool {
	var a, b bool
	for _, idx := range online {
		if idx == pair[0] {
			a = true
		}
		if idx == pair[1] {
			b = true
		}
	}
	return a && b
}

// applyPriorityBoost lets one DROPS/STREAK-eligible online streamer take over a
// base-pair seat for the current tick, without affecting the base ranking
// computed by rotateToLeastWatchedPair.
//
// Continuity latch: the boosted channel (and the base-pair seat it displaces)
// are held across ticks rather than re-picked every tick. A watch streak — and
// unrestricted drop progress — is only credited for CONTINUOUS viewing, so the
// watched set has to stay stable minute-over-minute for it to accumulate.
// Before the latch, the boost re-selected the least-recently-watched eligible
// channel every tick, which (whenever 3+ channels were eligible) rotated the
// watched set on every single tick: no channel was ever watched on consecutive
// ticks, MinuteWatched was perpetually reset to 0, and no streak ever
// completed. The latch keeps the same channel in the boost seat until it stops
// being eligible (streak earned, drop done, went offline) or a STRICTLY
// higher-priority candidate appears (e.g. a channel-restricted drop), at which
// point it hands the seat off and the previously-displaced base member is
// re-evaluated so it is no longer starved.
//
// Hold duration is deliberately UNBOUNDED in time — a known, intentional
// property, not a bug. The latch ends only on loss of eligibility
// (isBoostEligible → false) or a strictly-higher candidate; base-pair rotation
// clears it but immediately re-latches the same channel next tick if it is
// still the best. A streak self-limits to ~7 minutes (crossing
// watchStreakThresholdMinutes drops its eligibility), but a live drop campaign
// (DropsCondition, no minute cap) can hold ONE of the two watch slots for as
// long as it stays farmable. That does not starve the other online streamers:
//   - the OTHER slot is the base pair, which keeps rotating normally via the
//     deficit-based fair rotation (rotateToLeastWatchedPair runs on its own
//     30-80 min timer, untouched by the boost), so the most-owed non-boosted
//     channel is still surfaced each window;
//   - the boosted channel records its own watch time (RecordMinutes), so the
//     fair-rotation ranking naturally keeps it OUT of the base pair — no
//     double-dipping — which de-starves the rest;
//   - the only channel not watched while the latch holds is the current victim,
//     deliberately the LESS-owed of the two base-pair members, and its identity
//     moves as the base pair recomputes, so no single channel is locked out.
//
// The cost is throughput: while one slot is held long-term, the remaining
// channels share the single rotating slot instead of two — the accepted price
// of finishing a long drop campaign on the channel that needs it.
func (w *MinuteWatcher) applyPriorityBoost(pair [2]int, onlineIndexes []int) [2]int {
	best := w.selectBoostTarget(pair, onlineIndexes)
	if best == -1 {
		w.rotation.clearBoostLatch()
		return pair
	}

	keepHeld := false
	if w.rotation.boostLatched {
		held := w.rotation.boostTarget
		if held >= 0 && held != pair[0] && held != pair[1] &&
			containsIndex(onlineIndexes, held) && w.isBoostEligible(held) &&
			!w.strictlyHigherBoost(best, held) {
			best = held
			keepHeld = true
		}
	}

	// While the same channel is held, keep displacing the same base seat so the
	// surviving base member also stays continuously watched. On a hand-off to a
	// new target, re-evaluate the victim so a base member that was displaced for
	// the whole previous boost gets its turn instead of staying starved.
	var victim int
	if keepHeld && (w.rotation.boostVictim == pair[0] || w.rotation.boostVictim == pair[1]) &&
		!w.nearStreakCompletion(w.rotation.boostVictim) {
		victim = w.rotation.boostVictim
	} else {
		victim = w.selectBoostVictim(pair)
	}
	if victim == -1 {
		w.rotation.clearBoostLatch()
		return pair
	}

	switch {
	case w.streamers[best].HasChannelRestrictedCampaign():
		w.noteSelection(best, "watched: boosted into a slot - channel-restricted drop campaign only progresses on this exact channel")
	case w.streamers[best].DropsCondition():
		w.noteSelection(best, "watched: boosted into a slot - active drop campaign")
	default:
		w.noteSelection(best, "watched: boosted into a slot - watch streak not yet earned this stream")
	}
	w.noteSelection(victim, "not watched this tick: displaced by a DROPS/STREAK boost (keeps its rotation slot and returns when the boost ends)")

	w.rotation.boostLatched = true
	w.rotation.boostTarget = best
	w.rotation.boostVictim = victim

	if pair[0] == victim {
		pair[0] = best
	} else {
		pair[1] = best
	}
	return pair
}

// selectBoostTarget returns the highest-priority off-pair boost-eligible
// streamer per betterBoostCandidate, or -1 if none is eligible.
func (w *MinuteWatcher) selectBoostTarget(pair [2]int, onlineIndexes []int) int {
	best := -1
	for _, idx := range onlineIndexes {
		if idx == pair[0] || idx == pair[1] {
			continue
		}
		if !w.isBoostEligible(idx) {
			continue
		}
		if best == -1 || w.betterBoostCandidate(idx, best) {
			best = idx
		}
	}
	return best
}

// selectBoostVictim returns the base-pair seat the boost should displace: the
// most-recently-watched member not seconds from completing its streak (so the
// least-recently-watched member keeps its slot), or -1 when both members are
// protected.
func (w *MinuteWatcher) selectBoostVictim(pair [2]int) int {
	victim := -1
	for _, slot := range pair {
		if w.nearStreakCompletion(slot) {
			continue
		}
		if victim == -1 || w.rotation.lastWatched[slot].After(w.rotation.lastWatched[victim]) {
			victim = slot
		}
	}
	return victim
}

// strictlyHigherBoost reports whether candidate cand belongs to a strictly
// higher boost tier than the currently-held boost target held. It mirrors
// betterBoostCandidate's priority tiers WITHOUT its least-recently-watched
// tiebreak: a channel-restricted drop outranks everything, and between two
// streaks-in-progress the one with more banked minutes wins. Same-tier
// candidates are NOT strictly higher, so the continuity latch keeps holding the
// current channel instead of thrashing to an equal-priority alternative.
func (w *MinuteWatcher) strictlyHigherBoost(cand, held int) bool {
	cr := w.streamers[cand].HasChannelRestrictedCampaign()
	hr := w.streamers[held].HasChannelRestrictedCampaign()
	if cr != hr {
		return cr
	}
	cp := w.streakInProgress(cand)
	hp := w.streakInProgress(held)
	if cp != hp {
		return cp
	}
	if cp && hp {
		return w.streamers[cand].Stream.GetMinuteWatched() > w.streamers[held].Stream.GetMinuteWatched()
	}
	return false
}

// containsIndex reports whether idx is present in the online index slice.
func containsIndex(online []int, idx int) bool {
	for _, o := range online {
		if o == idx {
			return true
		}
	}
	return false
}

func (w *MinuteWatcher) isBoostEligible(idx int) bool {
	s := w.streamers[idx]
	if s.DropsCondition() {
		return true
	}
	return s.Settings.WatchStreak &&
		s.Stream.StreakPending() &&
		(s.GetOfflineAt().IsZero() || time.Since(s.GetOfflineAt()) > 30*time.Minute) &&
		!w.streakPursuitExhausted(idx)
}

// streakPursuitExhausted reports whether the streak boost seat should be RELEASED
// even though Twitch has not granted the streak. The release trigger is the
// CONTINUOUSLY-watched minutes reaching streakPursuitCapMinutes — and, crucially,
// once reached it LATCHES per-broadcast (Stream.StreakPursuitExhausted), so a real
// slot loss that resets the continuous-minute counter can NOT re-open a fresh
// pursuit window for the same broadcast. The authoritative WATCH_STREAK grant is
// handled separately (StreakPending -> false makes isBoostEligible false
// immediately, the primary and fastest release).
//
// It deliberately does NOT release on WATCH-points evidence. Twitch pays a watch
// streak only while the channel is actually being watched, and nothing proves the
// grant lands at or before the second WATCH credit; releasing there would trade
// streak reliability for faster rotation and could drop the channel just before
// Twitch pays. The evidence is also unreliable as a trigger: the points-earned
// subscription is account-wide, so a WATCH credit can be produced by an external
// browser tab or arrive late from a prior broadcast on the same channel. So a
// pending streak holds the seat until Twitch grants it or the bounded continuous
// window elapses (then stays released for that broadcast) — maximizing 300-450
// capture while staying bounded. Releasing only frees the seat: StreakPending stays
// true, so a LATE real WATCH_STREAK is still accepted and recorded exactly once.
func (w *MinuteWatcher) streakPursuitExhausted(idx int) bool {
	return w.streamers[idx].Stream.StreakPursuitExhausted(streakPursuitCapMinutes)
}

// streakInProgress reports whether a boost-eligible streamer is actively
// pursuing its watch streak: some watch time banked, streak still missing, and
// the evidence-based pursuit not yet exhausted. Preferring these when picking the
// boost seat lets the watcher finish a streak it already started instead of
// alternating between several fresh pending-streak streamers each tick and
// completing none of them.
func (w *MinuteWatcher) streakInProgress(idx int) bool {
	s := w.streamers[idx]
	return s.Settings.WatchStreak &&
		s.Stream.StreakPending() &&
		s.Stream.GetMinuteWatched() > 0 &&
		!w.streakPursuitExhausted(idx)
}

// betterBoostCandidate reports whether off-pair streamer cand should take the
// single boost seat over the current best. The ranking, highest priority first:
//
//  1. A channel-restricted drop campaign - its progress can only ever be earned
//     by watching this exact channel, so it can't wait for a rotation turn.
//  2. A watch streak already in progress, most-watched first - finish a streak
//     the bot already started (converges) rather than starting a new one and
//     leaving both unfinished (thrashes).
//  3. Least-recently-watched - the original fairness tie-break.
func (w *MinuteWatcher) betterBoostCandidate(cand, best int) bool {
	cr := w.streamers[cand].HasChannelRestrictedCampaign()
	br := w.streamers[best].HasChannelRestrictedCampaign()
	if cr != br {
		return cr
	}

	cp := w.streakInProgress(cand)
	bp := w.streakInProgress(best)
	if cp != bp {
		return cp
	}
	if cp && bp {
		// Both mid-streak: prefer the one with the most watch time banked so the
		// pursuit converges on a single streamer instead of alternating.
		cm := w.streamers[cand].Stream.GetMinuteWatched()
		bm := w.streamers[best].Stream.GetMinuteWatched()
		if cm != bm {
			return cm > bm
		}
	}

	return w.rotation.lastWatched[cand].Before(w.rotation.lastWatched[best])
}

// nearStreakCompletion reports whether the streamer is actively pursuing a watch
// streak that has neither been earned nor exhausted, so an in-flight rotation
// swap-out or a boost displacement should avoid interrupting it. With the
// event-driven model there is no fixed "minutes-to-completion" line any more
// (the pursuit ends on the WATCH_STREAK grant or evidence-based exhaustion), so
// this mirrors streakInProgress. The swap-out deferral it feeds is bounded
// (once per approach), so it can never stall the rotation.
func (w *MinuteWatcher) nearStreakCompletion(idx int) bool {
	return w.streakInProgress(idx)
}

// noteStreakProgress logs watch-streak pursuit for a streamer that just had a
// minute-watched successfully reported. It emits at most one "Pursuing watch
// streak" INFO and one "past the threshold" WARN per streak, so the operator
// can both see the bot actively chasing streaks (previously invisible - the
// only streak log was the earned "Points earned" line, which never appears
// when streaks aren't being credited) and, crucially, tell apart "not watched
// enough yet" from "watched enough but Twitch never granted it".
func (w *MinuteWatcher) noteStreakProgress(idx int) {
	s := w.streamers[idx]
	if !s.Settings.WatchStreak || !s.Stream.StreakPending() {
		// Streak disabled, already earned for THIS broadcast (including a
		// re-armed pursuit on the same broadcast after a blip or restart), or
		// deferred until the broadcast is identified: drop any pursuit state
		// so the next genuinely fresh broadcast reports again from scratch.
		// This is what silences the misleading threshold WARN after a grant.
		delete(w.streakDiag, idx)
		return
	}

	if w.streakDiag == nil {
		w.streakDiag = make(map[int]streakDiagState)
	}
	state := w.streakDiag[idx]
	mw := s.Stream.GetMinuteWatched()
	evidence := s.Stream.StreakWatchEvidence()

	if !state.pursuing {
		state.pursuing = true
		slog.Info("Pursuing watch streak (holding a boost slot until Twitch grants it or the bounded watch window elapses)",
			"streamer", s.Username,
			"continuousWatchedMinutes", mw,
			"watchEvents", evidence,
			"broadcastID", s.Stream.GetBroadcastID())
	}

	// Log the bounded-window release exactly once, as an OUTCOME-NEUTRAL
	// transition. At this point the streak outcome is genuinely unknown: releasing
	// only frees the seat (StreakPending stays true, so a late real WATCH_STREAK is
	// still accepted and recorded once), and the grant travels on the best-effort
	// PubSub transport, which cannot tell "not granted" apart from "not delivered".
	// So this line asserts neither "granted" nor "not granted" — only that the
	// bounded pursuit window elapsed (releaseReason=bounded_timeout, outcome=unknown).
	// The WATCH-evidence counter is a diagnostic field, never a release trigger; the
	// only inference drawn from it is the narrow, non-outcome hint that ZERO WATCH
	// credits for the whole broadcast may point at a transport/authorization problem
	// worth checking (a counted view normally produces WATCH credits).
	if w.streakPursuitExhausted(idx) && !state.released {
		state.released = true
		attrs := []any{
			"streamer", s.Username,
			"broadcastID", s.Stream.GetBroadcastID(),
			"continuousWatchedMinutes", mw,
			"watchEvents", evidence,
			"releaseReason", "bounded_timeout",
			"outcome", "unknown",
		}
		if evidence == 0 {
			slog.Warn("Releasing the watch-streak boost slot: bounded watch window elapsed, streak outcome unknown (a late WATCH_STREAK is still accepted); no WATCH point credits arrived for this broadcast - check authorization/transport", attrs...)
		} else {
			slog.Info("Releasing the watch-streak boost slot: bounded watch window elapsed, streak outcome unknown (a late WATCH_STREAK is still accepted)", attrs...)
		}
	}

	w.streakDiag[idx] = state
}

// selectByPriority is the original priority-based picker, used as-is when
// there are <= constants.MaxSimultaneousStreams online (no rotation needed).
func (w *MinuteWatcher) selectByPriority(onlineIndexes []int) []int {
	// Preferred streamers are moved to the front (stably, so relative order
	// is otherwise unchanged). This only breaks ties within each priority
	// step below - it never lets a preferred streamer skip ahead of one that
	// actually satisfies a higher-ranked priority.
	ordered := make([]int, len(onlineIndexes))
	copy(ordered, onlineIndexes)
	sort.SliceStable(ordered, func(i, j int) bool {
		return w.isPreferred(ordered[i]) && !w.isPreferred(ordered[j])
	})
	onlineIndexes = ordered

	watching := make(map[int]bool)

	remainingSlots := func() int {
		return constants.MaxSimultaneousStreams - len(watching)
	}

	for _, priority := range w.priorities {
		if remainingSlots() <= 0 {
			break
		}

		switch priority {
		case config.PriorityOrder:
			for _, idx := range onlineIndexes {
				if !watching[idx] {
					watching[idx] = true
					w.noteSelection(idx, "watched: selected by ORDER priority (position in the configured streamer list)")
					if remainingSlots() <= 0 {
						break
					}
				}
			}

		case config.PriorityPointsAscending, config.PriorityPointsDescending:
			type indexedPoints struct {
				index  int
				points int
			}
			items := make([]indexedPoints, 0, len(onlineIndexes))
			for _, idx := range onlineIndexes {
				items = append(items, indexedPoints{index: idx, points: w.streamers[idx].GetChannelPoints()})
			}
			sort.SliceStable(items, func(i, j int) bool {
				if priority == config.PriorityPointsAscending {
					return items[i].points < items[j].points
				}
				return items[i].points > items[j].points
			})
			for _, item := range items {
				if !watching[item.index] {
					watching[item.index] = true
					w.noteSelection(item.index, fmt.Sprintf("watched: selected by %s priority (%d channel points)", priority, item.points))
					if remainingSlots() <= 0 {
						break
					}
				}
			}

		case config.PriorityStreak:
			for _, idx := range onlineIndexes {
				s := w.streamers[idx]
				if s.Settings.WatchStreak &&
					s.Stream.StreakPending() &&
					(s.GetOfflineAt().IsZero() || time.Since(s.GetOfflineAt()) > 30*time.Minute) &&
					s.Stream.GetMinuteWatched() < watchStreakThresholdMinutes {
					if !watching[idx] {
						watching[idx] = true
						w.noteSelection(idx, "watched: selected by STREAK priority - watch streak not yet earned this stream")
						if remainingSlots() <= 0 {
							break
						}
					}
				}
			}

		case config.PriorityDrops:
			// Within each DROPS pass, order competing streamers by the campaign
			// policy engine's score (highest first) so the active mode
			// (SMART/ENDING_SOONEST/…) decides between several farmable
			// campaigns. With no policy scores published (GAME_ORDER/disabled)
			// this is a no-op and the configured order is preserved. The
			// restricted-first pass below is kept regardless, so the
			// "channel-restricted drop only progresses here" invariant holds in
			// every mode.
			dropsOrder := w.orderByCampaignScore(onlineIndexes)
			for _, idx := range dropsOrder {
				if w.streamers[idx].DropsCondition() && w.streamers[idx].HasChannelRestrictedCampaign() {
					if !watching[idx] {
						watching[idx] = true
						w.noteSelection(idx, "watched: selected by DROPS priority - channel-restricted drop campaign only progresses on this exact channel")
						if remainingSlots() <= 0 {
							break
						}
					}
				}
			}
			for _, idx := range dropsOrder {
				if remainingSlots() <= 0 {
					break
				}
				if w.streamers[idx].DropsCondition() {
					if !watching[idx] {
						watching[idx] = true
						w.noteSelection(idx, "watched: selected by DROPS priority - active drop campaign")
						if remainingSlots() <= 0 {
							break
						}
					}
				}
			}

		case config.PrioritySubscribed:
			type indexedMultiplier struct {
				index      int
				multiplier float64
			}
			var items []indexedMultiplier
			for _, idx := range onlineIndexes {
				if w.streamers[idx].ViewerHasPointsMultiplier() {
					items = append(items, indexedMultiplier{
						index:      idx,
						multiplier: w.streamers[idx].TotalPointsMultiplier(),
					})
				}
			}
			sort.SliceStable(items, func(i, j int) bool {
				return items[i].multiplier > items[j].multiplier
			})
			for _, item := range items {
				if !watching[item.index] {
					watching[item.index] = true
					w.noteSelection(item.index, fmt.Sprintf("watched: selected by SUBSCRIBED priority (%.1fx points multiplier)", item.multiplier))
					if remainingSlots() <= 0 {
						break
					}
				}
			}
		}
	}

	result := make([]int, 0, len(watching))
	for idx := range watching {
		result = append(result, idx)
	}
	return result
}

func (w *MinuteWatcher) sendMinuteWatched(streamer *models.Streamer) error {
	simulateErr, err := w.sender.Send(streamer)
	if simulateErr != nil {
		slog.Debug("Failed to simulate watching", "streamer", streamer.Username, "error", simulateErr)
	}
	return err
}

// accrueLostMining credits this tick's idle-slot time to the lost-mining
// accumulator. fillable is how many slots could have been productively used
// this tick (bounded by the slot cap), watchedOK is how many actually reported
// a minute, and interval is the tick length. Lost = the shortfall × interval;
// zero when every fillable slot was watched (or nothing was fillable).
func (w *MinuteWatcher) accrueLostMining(fillable, watchedOK int, interval time.Duration) {
	capacity := fillable
	if capacity > constants.MaxSimultaneousStreams {
		capacity = constants.MaxSimultaneousStreams
	}
	lost := capacity - watchedOK
	if lost <= 0 {
		return
	}
	w.lostMu.Lock()
	w.lostMiningMinutes += float64(lost) * interval.Minutes()
	w.lostMu.Unlock()
}

// LostMiningMinutes returns the accumulated estimated lost mining minutes and
// resets the accumulator to zero (drain semantics). Called once per daily
// summary; the returned value covers the period since the previous drain. It is
// in-memory and process-lifetime: a restart resets it, so a summary after a
// mid-day restart only reflects post-restart idle time.
func (w *MinuteWatcher) LostMiningMinutes() float64 {
	w.lostMu.Lock()
	defer w.lostMu.Unlock()
	v := w.lostMiningMinutes
	w.lostMiningMinutes = 0
	return v
}
