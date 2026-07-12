package watcher

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/PatrickWalther/twitch-miner-go/internal/api"
	"github.com/PatrickWalther/twitch-miner-go/internal/config"
	"github.com/PatrickWalther/twitch-miner-go/internal/constants"
	"github.com/PatrickWalther/twitch-miner-go/internal/models"
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

	ctx    context.Context
	cancel context.CancelFunc

	httpClient *http.Client

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
		httpClient: &http.Client{Timeout: 20 * time.Second},
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
	onlineStreamers := w.getOnlineStreamers()
	if len(onlineStreamers) == 0 {
		return
	}

	for _, idx := range onlineStreamers {
		if w.streamers[idx].Stream.UpdateElapsed() > 10*time.Minute {
			w.client.CheckStreamerOnline(w.streamers[idx])
		}
	}

	watching := w.selectStreamersToWatch(onlineStreamers)
	if len(watching) == 0 {
		return
	}

	var watchingNames []string
	for _, idx := range watching {
		watchingNames = append(watchingNames, w.streamers[idx].Username)
	}
	slog.Debug("Watching streams", "count", len(watching), "max", constants.MaxSimultaneousStreams, "streamers", watchingNames)

	sleepBetween := time.Duration(w.settings.MinuteWatchedInterval) * time.Second / time.Duration(len(watching))

	for _, idx := range watching {
		streamer := w.streamers[idx]

		if err := w.sendMinuteWatched(streamer); err != nil {
			slog.Debug("Failed to send minute watched", "streamer", streamer.Username, "error", err)
		} else {
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
}

func (w *MinuteWatcher) getOnlineStreamers() []int {
	var online []int
	for i, s := range w.streamers {
		if s.GetIsOnline() {
			if s.GetOnlineAt().IsZero() || time.Since(s.GetOnlineAt()) > 30*time.Second {
				online = append(online, i)
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
	if len(onlineIndexes) <= constants.MaxSimultaneousStreams {
		// Not enough online streamers to need rotation; drop any stale pair
		// so a fresh one is computed next time we go above the limit.
		w.rotation.hasPair = false
		return w.selectByPriority(onlineIndexes)
	}
	return w.selectRotating(onlineIndexes)
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

// rotateToLeastWatchedPair recomputes the base pair from accumulated watch
// time, unless doing so would interrupt a leaving streamer's near-complete
// watch streak (see selectRotating's doc comment).
func (w *MinuteWatcher) rotateToLeastWatchedPair(onlineIndexes []int, now time.Time) {
	weights := w.watchWeights(onlineIndexes, now)

	candidates := make([]int, len(onlineIndexes))
	copy(candidates, onlineIndexes)
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if weights[a] != weights[b] {
			return weights[a] < weights[b]
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
				return
			}
		}

		for _, idx := range newPair {
			delete(w.rotation.deferredFor, idx)
		}
	}

	w.rotation.activePair = newPair
	w.rotation.hasPair = true
	w.rotation.lastSwitch = now
	w.rotation.nextInterval = w.randomRotationInterval()
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
		if best == -1 || w.rotation.lastWatched[idx].Before(w.rotation.lastWatched[best]) {
			best = idx
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
			sort.Slice(items, func(i, j int) bool {
				if priority == config.PriorityPointsAscending {
					return items[i].points < items[j].points
				}
				return items[i].points > items[j].points
			})
			for _, item := range items {
				if !watching[item.index] {
					watching[item.index] = true
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
						if remainingSlots() <= 0 {
							break
						}
					}
				}
			}

		case config.PriorityDrops:
			for _, idx := range onlineIndexes {
				if w.streamers[idx].DropsCondition() {
					if !watching[idx] {
						watching[idx] = true
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
			sort.Slice(items, func(i, j int) bool {
				return items[i].multiplier > items[j].multiplier
			})
			for _, item := range items {
				if !watching[item.index] {
					watching[item.index] = true
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
	sig, token, err := w.client.GetPlaybackAccessToken(streamer.Username)
	if err != nil {
		return fmt.Errorf("failed to get playback token: %w", err)
	}

	if err := w.simulateWatching(streamer.Username, sig, token); err != nil {
		slog.Debug("Failed to simulate watching", "streamer", streamer.Username, "error", err)
	}

	if streamer.Stream.SpadeURL == "" {
		return fmt.Errorf("no spade URL")
	}

	payload, err := streamer.Stream.EncodePayload()
	if err != nil {
		return fmt.Errorf("failed to encode payload: %w", err)
	}

	req, err := http.NewRequest("POST", streamer.Stream.SpadeURL, strings.NewReader("data="+payload))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", constants.TVUserAgent)

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	return nil
}

func (w *MinuteWatcher) simulateWatching(channel, sig, token string) error {
	playlistURL := fmt.Sprintf("%s/api/channel/hls/%s.m3u8", constants.UsherURL, channel)

	params := url.Values{
		"sig":   {sig},
		"token": {token},
	}

	resp, err := w.httpClient.Get(playlistURL + "?" + params.Encode())
	if err != nil {
		return fmt.Errorf("failed to get playlist: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("playlist request failed with status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read playlist: %w", err)
	}

	lines := strings.Split(string(body), "\n")
	var lowestQualityURL string
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "http") {
			lowestQualityURL = line
			break
		}
	}

	if lowestQualityURL == "" {
		return fmt.Errorf("no stream URL found in playlist")
	}

	streamListResp, err := w.httpClient.Get(lowestQualityURL)
	if err != nil {
		return fmt.Errorf("failed to get stream list: %w", err)
	}
	defer func() { _ = streamListResp.Body.Close() }()

	if streamListResp.StatusCode != http.StatusOK {
		return fmt.Errorf("stream list request failed with status %d", streamListResp.StatusCode)
	}

	streamListBody, err := io.ReadAll(streamListResp.Body)
	if err != nil {
		return fmt.Errorf("failed to read stream list: %w", err)
	}

	streamLines := strings.Split(string(streamListBody), "\n")
	var segmentURL string
	for i := len(streamLines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(streamLines[i])
		if strings.HasPrefix(line, "http") {
			segmentURL = line
			break
		}
	}

	if segmentURL == "" {
		return fmt.Errorf("no segment URL found")
	}

	req, err := http.NewRequest("HEAD", segmentURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create HEAD request: %w", err)
	}
	req.Header.Set("User-Agent", constants.TVUserAgent)

	headResp, err := w.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("HEAD request failed: %w", err)
	}
	defer func() { _ = headResp.Body.Close() }()

	if headResp.StatusCode != http.StatusOK {
		return fmt.Errorf("HEAD request returned status %d", headResp.StatusCode)
	}

	return nil
}
