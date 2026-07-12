package models

import (
	"encoding/base64"
	"encoding/json"
	"sync"
	"time"
)

type Stream struct {
	BroadcastID  string
	Title        string
	Game         *Game
	Tags         []Tag
	ViewersCount int
	SpadeURL     string
	CampaignIDs  []string
	Campaigns    []*Campaign

	WatchStreakMissing bool
	MinuteWatched      float64

	payload              []MinuteWatchedEvent
	lastUpdate           time.Time
	minuteWatchedUpdated time.Time

	mu sync.RWMutex
}

type Tag struct {
	ID            string `json:"id"`
	LocalizedName string `json:"localizedName"`
}

type MinuteWatchedEvent struct {
	Event      string                 `json:"event"`
	Properties map[string]interface{} `json:"properties"`
}

func NewStream() *Stream {
	return &Stream{
		WatchStreakMissing: true,
	}
}

func (s *Stream) Update(broadcastID, title string, game *Game, tags []Tag, viewersCount int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.BroadcastID = broadcastID
	s.Title = title
	s.Game = game
	s.Tags = tags
	s.ViewersCount = viewersCount
	s.lastUpdate = time.Now()
}

func (s *Stream) UpdateRequired() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.lastUpdate.IsZero() {
		return true
	}
	return time.Since(s.lastUpdate) >= 2*time.Minute
}

func (s *Stream) UpdateElapsed() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.lastUpdate.IsZero() {
		return 0
	}
	return time.Since(s.lastUpdate)
}

func (s *Stream) SetPayload(channelID, broadcastID, userID, channel string, game *Game) {
	s.mu.Lock()
	defer s.mu.Unlock()

	properties := map[string]interface{}{
		"channel_id":   channelID,
		"broadcast_id": broadcastID,
		"player":       "site",
		"user_id":      userID,
		"live":         true,
		"channel":      channel,
	}

	if game != nil && game.Name != "" && game.ID != "" {
		properties["game"] = game.Name
		properties["game_id"] = game.ID
	}

	s.payload = []MinuteWatchedEvent{
		{
			Event:      "minute-watched",
			Properties: properties,
		},
	}
}

func (s *Stream) EncodePayload() (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, err := json.Marshal(s.payload)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

func (s *Stream) InitWatchStreak() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.WatchStreakMissing = true
	s.MinuteWatched = 0
	s.minuteWatchedUpdated = time.Time{}
}

// UpdateMinuteWatched advances the continuous watched-minutes counter and
// returns the delta (in minutes) credited by this call. The first call after
// InitWatchStreak returns 0, since there's no prior timestamp to measure from.
//
// maxGap is the largest interval between two consecutive minute-watched reports
// that still counts as continuous viewing of the same broadcast. When the gap
// since the previous report exceeds it, the streamer was not watched
// continuously (rotated out of a watch slot, a failed cycle, a brief offline
// blip, ...). Twitch resets its server-side watch-streak session on such a
// break, so MinuteWatched must restart from zero too: otherwise it would count
// wall-clock elapsed time instead of actually-watched time, cross the
// watch-streak threshold on phantom minutes the viewer never continuously
// watched, and - because the streak-pursuit logic stops chasing a streamer once
// MinuteWatched passes the threshold - abandon a streak that was in fact never
// earned. A non-positive maxGap disables the break check (unbounded
// accumulation, the historical behaviour).
func (s *Stream) UpdateMinuteWatched(maxGap time.Duration) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	if s.minuteWatchedUpdated.IsZero() {
		s.minuteWatchedUpdated = now
		return 0
	}

	gap := now.Sub(s.minuteWatchedUpdated)
	s.minuteWatchedUpdated = now

	if maxGap > 0 && gap > maxGap {
		// Continuity broken - restart the streak progress from scratch and
		// credit nothing for the gap (no viewing actually happened during it).
		s.MinuteWatched = 0
		return 0
	}

	delta := gap.Minutes()
	s.MinuteWatched += delta
	return delta
}

func (s *Stream) GetMinuteWatched() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.MinuteWatched
}

func (s *Stream) GetWatchStreakMissing() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.WatchStreakMissing
}

func (s *Stream) GetTitle() string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.Title
}

func (s *Stream) GetViewersCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.ViewersCount
}

func (s *Stream) GameName() string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.Game == nil {
		return ""
	}
	return s.Game.Name
}

func (s *Stream) GameID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.Game == nil {
		return ""
	}
	return s.Game.ID
}
