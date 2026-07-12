package watcher

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

type MinuteWatcher struct {
	client     *api.TwitchClient
	streamers  []*models.Streamer
	priorities []config.Priority
	settings   config.RateLimitSettings

	// store persists accumulated watch time per streamer so rotation
	// fairness survives restarts. May be nil (e.g. analytics disabled), in
	// which case rotation falls back to in-memory recency only.
	store *WatchTimeStore

	// rotation is only ever read/written from the loop() goroutine, so it
	// needs no locking of its own.
	rotation rotationState

	// selectionReasons and selectionMode are per-tick scratch state for the
	// debug snapshot, rebuilt on every processWatching pass. Like rotation,
	// they are only touched from the loop() goroutine; the copy other
	// goroutines may read is debugState below.
	selectionReasons map[int]string
	selectionMode    string

	// debugState is the last published watch-decision snapshot, guarded by
	// debugMu because the debug HTTP endpoint reads it from its own goroutine.
	debugMu    sync.Mutex
	debugState DebugState

	ctx    context.Context
	cancel context.CancelFunc

	// sender performs the actual watch-minute reporting (playback token,
	// playlist touch, spade event). Shared mechanism with the discovery slot.
	sender *MinuteSender

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
	}
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

func (w *MinuteWatcher) UpdateSettings(priorities []config.Priority, settings config.RateLimitSettings) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.priorities = priorities
	w.settings = settings
}

func (w *MinuteWatcher) randomizedDelay(base time.Duration) time.Duration {
	jitter := (rand.Float64() - 0.5) * 0.4
	return time.Duration(float64(base) * (1.0 + jitter))
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
	w.selectionReasons = make(map[int]string)
	w.selectionMode = ModeIdle

	onlineStreamers := w.getOnlineStreamers()
	if len(onlineStreamers) == 0 {
		w.publishDebugState(nil, ModeIdle)
		return
	}

	for _, idx := range onlineStreamers {
		if w.streamers[idx].Stream.UpdateElapsed() > 10*time.Minute {
			w.client.CheckStreamerOnline(w.streamers[idx])
		}
	}

	watching := w.selectStreamersToWatch(onlineStreamers)
	w.publishDebugState(watching, w.selectionMode)
	if len(watching) == 0 {
		return
	}

	var watchingNames []string
	for _, idx := range watching {
		watchingNames = append(watchingNames, w.streamers[idx].Username)
	}
	slog.Debug("Watching streams", "count", len(watching), "max", constants.MaxSimultaneousStreams, "streamers", watchingNames)

	sleepBetween := time.Duration(w.settings.MinuteWatchedInterval) * time.Second / time.Duration(len(watching))

	reported := false
	for _, idx := range watching {
		streamer := w.streamers[idx]

		if err := w.sendMinuteWatched(streamer); err != nil {
			slog.Debug("Failed to send minute watched", "streamer", streamer.Username, "error", err)
		} else {
			reported = true
			slog.Debug("Sent minute watched", "streamer", streamer.Username, "minutesWatched", streamer.Stream.MinuteWatched)
			delta := streamer.Stream.UpdateMinuteWatched()
			if w.store != nil && delta > 0 {
				if err := w.store.RecordMinutes(streamer.Username, delta, time.Now()); err != nil {
					slog.Debug("Failed to record watch time", "streamer", streamer.Username, "error", err)
				}
			}
		}

		select {
		case <-w.ctx.Done():
			return
		case <-time.After(w.randomizedDelay(sleepBetween)):
		}
	}

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

func (w *MinuteWatcher) getOnlineStreamers() []int {
	var online []int
	for i, s := range w.streamers {
		if s.GetIsOnline() {
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
	bestRestricted := false
	for _, idx := range onlineIndexes {
		if idx == pair[0] || idx == pair[1] {
			continue
		}
		if !w.isBoostEligible(idx) {
			continue
		}
		// A channel-restricted campaign can only ever progress by watching
		// this exact channel, so it always outranks a candidate that merely
		// has an unrestricted campaign or a watch streak in progress.
		restricted := w.streamers[idx].HasChannelRestrictedCampaign()
		switch {
		case best == -1:
			best, bestRestricted = idx, restricted
		case restricted && !bestRestricted:
			best, bestRestricted = idx, restricted
		case restricted == bestRestricted && w.rotation.lastWatched[idx].Before(w.rotation.lastWatched[best]):
			best, bestRestricted = idx, restricted
		}
	}
	if best == -1 {
		return pair
	}

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
		s.Stream.MinuteWatched < 7
}

func (w *MinuteWatcher) nearStreakCompletion(idx int) bool {
	s := w.streamers[idx]
	if !s.Settings.WatchStreak || !s.Stream.WatchStreakMissing {
		return false
	}
	mw := s.Stream.MinuteWatched
	return mw >= 5 && mw < 7
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
					s.Stream.MinuteWatched < 7 {
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
