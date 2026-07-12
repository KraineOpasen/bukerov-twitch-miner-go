package models

import (
	"fmt"
	"sync"
	"time"
)

type ChatPresence string

const (
	ChatAlways  ChatPresence = "ALWAYS"
	ChatNever   ChatPresence = "NEVER"
	ChatOnline  ChatPresence = "ONLINE"
	ChatOffline ChatPresence = "OFFLINE"
)

// Preference marks a streamer's rotation preference relative to other online
// streamers. It never overrides DROPS/STREAK priority or fair time-based
// rotation - it only tips the balance when those are otherwise equal.
type Preference string

const (
	PreferenceNone   Preference = ""
	PreferencePrefer Preference = "prefer"
	PreferenceAvoid  Preference = "avoid"
)

type StreamerSettings struct {
	MakePredictions bool         `json:"makePredictions"`
	FollowRaid      bool         `json:"followRaid"`
	ClaimDrops      bool         `json:"claimDrops"`
	ClaimMoments    bool         `json:"claimMoments"`
	WatchStreak     bool         `json:"watchStreak"`
	CommunityGoals  bool         `json:"communityGoals"`
	Chat            ChatPresence `json:"chat"`
	ChatLogs        *bool        `json:"chatLogs,omitempty"`
	Bet             BetSettings  `json:"bet"`
	Preference      Preference   `json:"preference,omitempty"`
}

func DefaultStreamerSettings() StreamerSettings {
	return StreamerSettings{
		MakePredictions: true,
		FollowRaid:      true,
		ClaimDrops:      true,
		ClaimMoments:    true,
		WatchStreak:     true,
		CommunityGoals:  false,
		Chat:            ChatOnline,
		Bet:             DefaultBetSettings(),
	}
}

type HistoryEntry struct {
	Counter int
	Amount  int
}

type Streamer struct {
	Username          string
	ChannelID         string
	Settings          StreamerSettings
	IsOnline          bool
	StreamUpTime      time.Time
	OnlineAt          time.Time
	OfflineAt         time.Time
	LastChecked       time.Time
	ChannelPoints     int
	CommunityGoals    map[string]*CommunityGoal
	ViewerIsMod       bool
	ActiveMultipliers []Multiplier
	Stream            *Stream
	Raid              *Raid
	History           map[string]*HistoryEntry

	mu sync.RWMutex
}

type Multiplier struct {
	Factor float64 `json:"factor"`
}

func NewStreamer(username string, settings StreamerSettings) *Streamer {
	return &Streamer{
		Username:       username,
		Settings:       settings,
		CommunityGoals: make(map[string]*CommunityGoal),
		Stream:         NewStream(),
		History:        make(map[string]*HistoryEntry),
	}
}

func (s *Streamer) String() string {
	return fmt.Sprintf("Streamer(%s, %d points)", s.Username, s.ChannelPoints)
}

func (s *Streamer) SetOffline() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.IsOnline {
		s.OfflineAt = time.Now()
		s.IsOnline = false
	}
}

func (s *Streamer) SetOnline() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.IsOnline {
		s.OnlineAt = time.Now()
		s.IsOnline = true
		s.Stream.InitWatchStreak()
	}
}

func (s *Streamer) UpdateHistory(reasonCode string, earned int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.History[reasonCode]; !exists {
		s.History[reasonCode] = &HistoryEntry{}
	}
	s.History[reasonCode].Counter++
	s.History[reasonCode].Amount += earned

	if reasonCode == "WATCH_STREAK" {
		s.Stream.WatchStreakMissing = false
	}
}

func (s *Streamer) UpdateHistoryWithCounter(reasonCode string, earned, counter int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.History[reasonCode]; !exists {
		s.History[reasonCode] = &HistoryEntry{}
	}
	s.History[reasonCode].Counter += counter
	s.History[reasonCode].Amount += earned
}

func (s *Streamer) StreamUpElapsed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.StreamUpTime.IsZero() || time.Since(s.StreamUpTime) > 2*time.Minute
}

func (s *Streamer) DropsCondition() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.Settings.ClaimDrops &&
		s.IsOnline &&
		len(s.Stream.CampaignIDs) > 0
}

// HasChannelRestrictedCampaign reports whether this streamer currently has
// an assigned drop campaign that only credits progress on this specific
// channel (as opposed to any channel streaming the campaign's game). Such a
// campaign cannot be farmed by watching a different streamer, so channel
// selection should prioritize keeping this one watched.
func (s *Streamer) HasChannelRestrictedCampaign() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, c := range s.Stream.Campaigns {
		if c.IsChannelRestricted() {
			return true
		}
	}
	return false
}

func (s *Streamer) ViewerHasPointsMultiplier() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return len(s.ActiveMultipliers) > 0
}

func (s *Streamer) TotalPointsMultiplier() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	total := 0.0
	for _, m := range s.ActiveMultipliers {
		total += m.Factor
	}
	return total
}

func (s *Streamer) GetPredictionWindow(predictionWindowSeconds float64) float64 {
	delayMode := s.Settings.Bet.DelayMode
	delay := s.Settings.Bet.Delay

	switch delayMode {
	case DelayModeFromStart:
		if delay < predictionWindowSeconds {
			return delay
		}
		return predictionWindowSeconds
	case DelayModeFromEnd:
		result := predictionWindowSeconds - delay
		if result < 0 {
			return 0
		}
		return result
	case DelayModePercentage:
		return predictionWindowSeconds * delay
	default:
		return predictionWindowSeconds
	}
}

func (s *Streamer) AddCommunityGoal(goal *CommunityGoal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.CommunityGoals[goal.GoalID] = goal
}

func (s *Streamer) UpdateCommunityGoal(goal *CommunityGoal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.CommunityGoals[goal.GoalID] = goal
}

func (s *Streamer) DeleteCommunityGoal(goalID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.CommunityGoals, goalID)
}

func (s *Streamer) GetOnlineAt() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.OnlineAt
}

func (s *Streamer) GetOfflineAt() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.OfflineAt
}

func (s *Streamer) GetIsOnline() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.IsOnline
}

func (s *Streamer) GetChannelPoints() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ChannelPoints
}

func (s *Streamer) SetChannelPoints(points int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ChannelPoints = points
}

func (s *Streamer) GetSettings() StreamerSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Settings
}

func (s *Streamer) SetSettings(settings StreamerSettings) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Settings = settings
}

func (s *Streamer) GetLastChecked() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.LastChecked
}

func (s *Streamer) SetLastChecked(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LastChecked = t
}
