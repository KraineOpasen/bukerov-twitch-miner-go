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
	order    []int    // online streamer indexes the current schedule was built from (ascending)
	schedule [][2]int // sequence of index-pairs to cycle through, one per rotation tick
	pos      int      // index into schedule of the pair currently being watched

	lastSwitch time.Time // when schedule[pos] last changed

	lastWatched map[int]time.Time // last tick each streamer index was actually watched (fairness bookkeeping)
	deferredFor map[int]bool      // streamers whose scheduled swap-out was already postponed once
}

func NewMinuteWatcher(
	client *api.TwitchClient,
	streamers []*models.Streamer,
	priorities []config.Priority,
	settings config.RateLimitSettings,
) *MinuteWatcher {
	return &MinuteWatcher{
		client:     client,
		streamers:  streamers,
		priorities: priorities,
		settings:   settings,
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
			streamer.Stream.UpdateMinuteWatched()
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
		// Not enough online streamers to need rotation; drop any stale
		// schedule so a fresh one is built next time we go above the limit.
		w.rotation.order = nil
		w.rotation.schedule = nil
		w.rotation.pos = 0
		return w.selectByPriority(onlineIndexes)
	}
	return w.selectRotating(onlineIndexes)
}

// selectRotating implements the fair watch-pair rotation for the case where
// more streamers are online than fit in a watch slot.
//
// Base schedule (fairness guarantee):
// The online streamers (sorted ascending, i.e. stable config order) are
// split into a sequence of pairs that gets cycled through one pair per
// RotationInterval:
//   - Even count N: split into N/2 disjoint consecutive pairs (0,1)(2,3)...
//     Every streamer appears in exactly one pair per cycle, and the cycle
//     only takes N/2 ticks since every tick covers two new streamers.
//   - Odd count N: a disjoint split always leaves one streamer over, so
//     instead we use a sliding circular window of size 2 - (0,1)(1,2)...
//     (N-1,0) - advancing by one streamer per tick. Every streamer appears
//     in exactly two consecutive pairs (once on each side of the window)
//     over the full N-tick cycle, so shares stay equal.
//
// This base schedule alone already guarantees every online channel gets a
// watch turn within one cycle, regardless of priority.
//
// Priority as weight, not exclusivity:
// Before returning the scheduled pair, any online streamer with an active
// drop (DropsCondition) or a watch streak in progress (mirroring the
// existing STREAK/DROPS priority conditions) can take over one seat in the
// pair for that tick - ephemerally, without consuming or advancing the base
// schedule position. The seat sacrificed is whichever of the two scheduled
// members was watched most recently, so the other keeps its guaranteed
// fairness slot. The displaced member simply gets its scheduled turn next
// cycle instead of this tick, so no channel is permanently exclusive and no
// channel is ever locked out.
//
// Avoiding last-second interruptions:
// When the schedule is about to rotate a streamer out of the pair, if that
// streamer is within a few minutes of completing its watch streak
// (mirrors the `< 7 minutes` threshold used elsewhere for STREAK), the swap
// is postponed by one extra tick so it isn't yanked right before the streak
// completes. This is a best-effort heuristic based only on watch-streak
// timing - imminent drop-campaign completion isn't tracked here (that would
// require deeper integration with the drops package) and can still cause a
// mid-progress interruption; see PR description for this known limitation.
// Each streamer can only have its swap-out postponed once per approach, so
// this can never stall the rotation indefinitely.
func (w *MinuteWatcher) selectRotating(onlineIndexes []int) []int {
	now := time.Now()
	rotationInterval := time.Duration(w.settings.RotationInterval) * time.Second
	if rotationInterval <= 0 {
		rotationInterval = 15 * time.Minute
	}

	if !sameOrder(w.rotation.order, onlineIndexes) {
		w.rebuildRotation(onlineIndexes, now)
	} else if now.Sub(w.rotation.lastSwitch) >= rotationInterval {
		w.advanceRotation(now)
	}

	if len(w.rotation.schedule) == 0 {
		return nil
	}

	pair := w.applyPriorityBoost(w.rotation.schedule[w.rotation.pos])

	if w.rotation.lastWatched == nil {
		w.rotation.lastWatched = make(map[int]time.Time)
	}
	w.rotation.lastWatched[pair[0]] = now
	w.rotation.lastWatched[pair[1]] = now

	return []int{pair[0], pair[1]}
}

func (w *MinuteWatcher) rebuildRotation(onlineIndexes []int, now time.Time) {
	order := make([]int, len(onlineIndexes))
	copy(order, onlineIndexes)

	w.rotation.order = order
	w.rotation.schedule = buildRotationSchedule(order)
	w.rotation.pos = 0
	w.rotation.lastSwitch = now

	if w.rotation.lastWatched == nil {
		w.rotation.lastWatched = make(map[int]time.Time)
	}
	if w.rotation.deferredFor == nil {
		w.rotation.deferredFor = make(map[int]bool)
	}
}

// buildRotationSchedule builds the base fairness schedule described in
// selectRotating's doc comment.
func buildRotationSchedule(order []int) [][2]int {
	n := len(order)
	if n < 2 {
		return nil
	}

	schedule := make([][2]int, 0, n)
	if n%2 == 0 {
		for i := 0; i+1 < n; i += 2 {
			schedule = append(schedule, [2]int{order[i], order[i+1]})
		}
	} else {
		for i := 0; i < n; i++ {
			schedule = append(schedule, [2]int{order[i], order[(i+1)%n]})
		}
	}
	return schedule
}

// advanceRotation moves the schedule to the next pair, unless a streamer
// about to be rotated out is close to completing its watch streak and
// hasn't already had its swap-out postponed once.
func (w *MinuteWatcher) advanceRotation(now time.Time) {
	n := len(w.rotation.schedule)
	if n == 0 {
		return
	}

	nextPos := (w.rotation.pos + 1) % n
	current := w.rotation.schedule[w.rotation.pos]
	next := w.rotation.schedule[nextPos]

	nextSet := map[int]bool{next[0]: true, next[1]: true}
	for _, idx := range current {
		if nextSet[idx] {
			continue
		}
		if w.nearStreakCompletion(idx) && !w.rotation.deferredFor[idx] {
			w.rotation.deferredFor[idx] = true
			w.rotation.lastSwitch = now
			return
		}
	}

	for _, idx := range next {
		delete(w.rotation.deferredFor, idx)
	}

	w.rotation.pos = nextPos
	w.rotation.lastSwitch = now
}

// applyPriorityBoost lets one DROPS/STREAK-eligible streamer take over the
// pair seat most recently watched, without touching the base schedule.
func (w *MinuteWatcher) applyPriorityBoost(pair [2]int) [2]int {
	var best = -1
	for _, idx := range w.rotation.order {
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

func sameOrder(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
