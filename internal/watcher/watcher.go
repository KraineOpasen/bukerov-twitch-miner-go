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
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// onlineChecker is the slice of the Twitch client the watcher needs to
// re-verify a stale stream; narrowed to an interface so the broker's send loop
// can be tested with a fake. Satisfied by *api.TwitchClient.
type onlineChecker interface {
	CheckStreamerOnline(streamer *models.Streamer)
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

	// lastSlots is the previous tick's slot allocation (login -> reason code),
	// loop-owned, used to log slot changes only when they actually change.
	lastSlots map[string]string

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

	ctx    context.Context
	cancel context.CancelFunc

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
}

// streakDiagState records which watch-streak pursuit log lines have already
// been emitted for a streamer's current (still-missing) streak.
type streakDiagState struct {
	pursuing bool // "Pursuing watch streak" already logged
	stalled  bool // "watched past the threshold but no streak" already warned
}

// watchStreakThresholdMinutes is roughly how many minutes of watch time Twitch
// needs before it grants a watch-streak bonus (a streak is earned by viewing
// ~5 minutes of a consecutive broadcast; 7 leaves a margin). It doubles as the
// "should have earned it by now" line: once a streamer has been watched this
// long with the streak still missing, the streak isn't merely under-watched -
// something upstream (auth, viewing simulation, or a Twitch-side change) is
// keeping Twitch from crediting it.
const watchStreakThresholdMinutes = 7.0

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

func (w *MinuteWatcher) Start(ctx context.Context) {
	w.mu.Lock()
	w.ctx, w.cancel = context.WithCancel(ctx)
	w.mu.Unlock()

	go w.loop()
}

func (w *MinuteWatcher) Stop() {
	w.mu.Lock()
	if w.cancel != nil {
		w.cancel()
	}
	w.mu.Unlock()
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

// applyPendingSettings moves any staged runtime settings into the loop-owned
// priorities/settings fields. Runs on the loop goroutine at the start of each
// tick; also snapshots the registered sources and the avoid checker.
func (w *MinuteWatcher) applyPendingSettings() ([]CandidateSource, AvoidChecker) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.hasPending {
		w.priorities = w.pendingPriorities
		w.settings = w.pendingSettings
		w.hasPending = false
	}
	return append([]CandidateSource(nil), w.sources...), w.avoid
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

		w.processWatching()

		interval := time.Duration(w.settings.MinuteWatchedInterval) * time.Second
		select {
		case <-w.ctx.Done():
			return
		case <-time.After(w.randomizedDelay(interval)):
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

	// Execute any staged watch-session refreshes before the sends, so a
	// successful refresh takes effect for this very tick. Requests for channels
	// that lost their slot complete as skipped.
	w.executeSessionRefreshes(slots)

	if len(slots) == 0 {
		w.publishReportStats(slots)
		return
	}

	var watchingNames []string
	for _, sl := range slots {
		watchingNames = append(watchingNames, sl.streamer.Username)
	}
	slog.Debug("Watching streams", "count", len(slots), "max", constants.MaxSimultaneousStreams, "streamers", watchingNames)

	interval := time.Duration(w.settings.MinuteWatchedInterval) * time.Second
	sleepBetween := interval / time.Duration(len(slots))

	// A continuously-watched streamer is reported once per loop, so consecutive
	// reports land ~interval apart. Anything past twice that means it lost its
	// watch slot for at least a cycle - a break in continuity that resets the
	// watch-streak progress (see Stream.UpdateMinuteWatched).
	maxContinuousGap := 2 * interval

	reported := false
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
			w.noteReportOutcome(streamer.Username, true, time.Now())
			slog.Debug("Sent minute watched", "streamer", streamer.Username, "origin", sl.origin, "minutesWatched", streamer.Stream.MinuteWatched)
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

func (w *MinuteWatcher) getOnlineStreamers(avoid AvoidChecker) []int {
	var online []int
	for i, s := range w.streamers {
		if s.GetIsOnline() {
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
			if s.GetOnlineAt().IsZero() || time.Since(s.GetOnlineAt()) > 30*time.Second {
				online = append(online, i)
			} else {
				w.noteSelection(i, "went online less than 30s ago - waiting for the stream to settle before watching")
			}
		}
	}
	return online
}

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
// STREAK/DROPS priority conditions) can take over one seat in the pair for
// the current tick - ephemerally, without affecting the ranking above. The
// seat sacrificed is whichever of the two base-pair members was watched
// most recently, so the other keeps its slot. The displaced member simply
// ranks for its turn again next time the base pair is recomputed, so no
// channel is permanently exclusive and no channel is ever locked out.
//
// Avoiding last-second interruptions:
// When the base pair is about to rotate a streamer out, if that streamer is
// within a few minutes of completing its watch streak (mirrors the
// `< 7 minutes` threshold used elsewhere for STREAK), the swap is postponed
// by a short fixed delay so it isn't yanked right before the streak
// completes. This is a best-effort heuristic based only on watch-streak
// timing - imminent drop-campaign completion isn't tracked here (that would
// require deeper integration with the drops package) and can still cause a
// mid-progress interruption; see PR description for this known limitation.
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
// otherwise interrupt a streamer seconds away from completing its watch
// streak - deliberately much shorter than the rotation interval itself,
// since it only needs to bridge the last couple of minutes to 7.
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

// applyPriorityBoost lets one DROPS/STREAK-eligible online streamer take
// over the pair seat most recently watched, without affecting the base
// ranking computed by rotateToLeastWatchedPair.
func (w *MinuteWatcher) applyPriorityBoost(pair [2]int, onlineIndexes []int) [2]int {
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
	if best == -1 {
		return pair
	}
	bestRestricted := w.streamers[best].HasChannelRestrictedCampaign()

	victim := -1
	for _, slot := range pair {
		if w.nearStreakCompletion(slot) {
			continue
		}
		if victim == -1 || w.rotation.lastWatched[slot].After(w.rotation.lastWatched[victim]) {
			victim = slot
		}
	}
	if victim == -1 {
		return pair
	}

	switch {
	case bestRestricted:
		w.noteSelection(best, "watched: boosted into a slot - channel-restricted drop campaign only progresses on this exact channel")
	case w.streamers[best].DropsCondition():
		w.noteSelection(best, "watched: boosted into a slot - active drop campaign")
	default:
		w.noteSelection(best, "watched: boosted into a slot - watch streak not yet earned this stream")
	}
	w.noteSelection(victim, "not watched this tick: displaced by a DROPS/STREAK boost (keeps its rotation slot and returns when the boost ends)")

	if pair[0] == victim {
		pair[0] = best
	} else {
		pair[1] = best
	}
	return pair
}

func (w *MinuteWatcher) isBoostEligible(idx int) bool {
	s := w.streamers[idx]
	if s.DropsCondition() {
		return true
	}
	return s.Settings.WatchStreak &&
		s.Stream.WatchStreakMissing &&
		(s.GetOfflineAt().IsZero() || time.Since(s.GetOfflineAt()) > 30*time.Minute) &&
		s.Stream.MinuteWatched < watchStreakThresholdMinutes
}

// streakInProgress reports whether a boost-eligible streamer is part-way
// through earning its watch streak: some watch time already banked, streak
// still missing. Preferring these when picking the boost seat lets the watcher
// finish a streak it already started instead of alternating between several
// fresh pending-streak streamers each tick and completing none of them.
func (w *MinuteWatcher) streakInProgress(idx int) bool {
	s := w.streamers[idx]
	return s.Settings.WatchStreak &&
		s.Stream.WatchStreakMissing &&
		s.Stream.MinuteWatched > 0 &&
		s.Stream.MinuteWatched < watchStreakThresholdMinutes
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
		cm := w.streamers[cand].Stream.MinuteWatched
		bm := w.streamers[best].Stream.MinuteWatched
		if cm != bm {
			return cm > bm
		}
	}

	return w.rotation.lastWatched[cand].Before(w.rotation.lastWatched[best])
}

func (w *MinuteWatcher) nearStreakCompletion(idx int) bool {
	s := w.streamers[idx]
	if !s.Settings.WatchStreak || !s.Stream.WatchStreakMissing {
		return false
	}
	mw := s.Stream.MinuteWatched
	return mw >= 5 && mw < watchStreakThresholdMinutes
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
	if !s.Settings.WatchStreak || !s.Stream.GetWatchStreakMissing() {
		// Streak disabled or already earned for this broadcast: drop any pursuit
		// state so the next fresh broadcast reports again from scratch.
		delete(w.streakDiag, idx)
		return
	}

	if w.streakDiag == nil {
		w.streakDiag = make(map[int]streakDiagState)
	}
	state := w.streakDiag[idx]
	mw := s.Stream.GetMinuteWatched()

	if !state.pursuing {
		state.pursuing = true
		slog.Info("Pursuing watch streak",
			"streamer", s.Username,
			"minutesWatched", mw,
			"neededMinutes", watchStreakThresholdMinutes)
	}

	if mw >= watchStreakThresholdMinutes && !state.stalled {
		state.stalled = true
		slog.Warn("Watched past the watch-streak threshold but Twitch has not granted the streak - if this persists the streak is not being credited (check authorization / viewing), not merely under-watched",
			"streamer", s.Username,
			"minutesWatched", mw,
			"thresholdMinutes", watchStreakThresholdMinutes)
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
					s.Stream.WatchStreakMissing &&
					(s.GetOfflineAt().IsZero() || time.Since(s.GetOfflineAt()) > 30*time.Minute) &&
					s.Stream.MinuteWatched < watchStreakThresholdMinutes {
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
			// Streamers holding a channel-restricted campaign go first:
			// that progress can only ever be earned by watching this exact
			// channel, whereas an unrestricted campaign's progress could in
			// principle also be picked up by watching a different streamer
			// with the same game.
			for _, idx := range onlineIndexes {
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
			for _, idx := range onlineIndexes {
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
