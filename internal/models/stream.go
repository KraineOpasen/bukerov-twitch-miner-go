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

// UpdateMinuteWatched advances the cumulative watched-minutes counter and
// returns the delta (in minutes) credited by this call. The first call after
// InitWatchStreak returns 0, since there's no prior timestamp to measure from.
func (s *Stream) UpdateMinuteWatched() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	var delta float64
	if !s.minuteWatchedUpdated.IsZero() {
		delta = time.Since(s.minuteWatchedUpdated).Minutes()
		s.MinuteWatched += delta
	}
	s.minuteWatchedUpdated = time.Now()
	return delta
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
