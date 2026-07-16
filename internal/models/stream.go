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

	// CampaignIDs and Campaigns are written concurrently (api client refresh,
	// drops sync) and read from other goroutines (watcher selection, drops
	// intersection, progress watchdog). Production code must go through
	// SetCampaignIDs/GetCampaignIDs and SetCampaigns/GetCampaigns; direct field
	// access is tolerated only in single-goroutine test setup.
	CampaignIDs []string
	Campaigns   []*Campaign

	WatchStreakMissing bool
	MinuteWatched      float64

	// spadeURL is written by the api client (stream bring-up, session refresh)
	// and read by the minute sender and health probes on other goroutines —
	// unexported so every access takes the lock.
	spadeURL string

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

// ForceUpdateRequired invalidates the last-update timestamp so the next
// UpdateRequired() reports true immediately, bypassing the 2-minute refresh
// gate. Used by the progress-watchdog session refresh, which must re-fetch
// stream info on demand rather than wait out the gate.
func (s *Stream) ForceUpdateRequired() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastUpdate = time.Time{}
}

// GetSpadeURL returns the spade endpoint discovered for this stream ("" until
// the api client has fetched it).
func (s *Stream) GetSpadeURL() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.spadeURL
}

// SetSpadeURL records the spade endpoint (api client only).
func (s *Stream) SetSpadeURL(url string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.spadeURL = url
}

// GetCampaignIDs returns the campaign IDs Twitch currently advertises on this
// channel. The returned slice is replaced wholesale by SetCampaignIDs and its
// elements are immutable — callers may iterate but must not mutate.
func (s *Stream) GetCampaignIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.CampaignIDs
}

// SetCampaignIDs replaces the advertised campaign ID list (api client only).
func (s *Stream) SetCampaignIDs(ids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.CampaignIDs = ids
}

// GetCampaigns returns the tracked campaigns assigned to this channel by the
// drops tracker. The slice is replaced wholesale by SetCampaigns and the
// campaigns are immutable after publish (see Campaign.Clone) — callers may
// read but must not mutate.
func (s *Stream) GetCampaigns() []*Campaign {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Campaigns
}

// SetCampaigns replaces the assigned tracked campaigns (drops tracker only).
func (s *Stream) SetCampaigns(campaigns []*Campaign) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Campaigns = campaigns
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

// InitWatchStreak arms a fresh watch streak: nothing watched yet, streak still
// missing. It is the unconditional reset primitive; the decision of WHEN to arm
// (a genuine new broadcast vs a brief online-detection blip that should preserve
// progress) is made by the caller — see Streamer.SetOnline and
// watchStreakContinuityGrace.
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
