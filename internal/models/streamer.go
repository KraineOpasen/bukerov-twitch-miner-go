package models

import (
	"fmt"
	"sync"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
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
	MakePredictions bool `json:"makePredictions"`
	FollowRaid      bool `json:"followRaid"`
	ClaimDrops      bool `json:"claimDrops"`
	ClaimMoments    bool `json:"claimMoments"`
	WatchStreak     bool `json:"watchStreak"`
	CommunityGoals  bool `json:"communityGoals"`
	// CommunityGoalsMaxPercent caps a single community-goal contribution to this
	// percentage of the current channel-point balance (1-100). 0 means no
	// percentage cap. Only used when CommunityGoals is enabled.
	CommunityGoalsMaxPercent int `json:"communityGoalsMaxPercent"`
	// CommunityGoalsMaxAmount caps a single community-goal contribution to this
	// absolute number of points. 0 means no absolute cap. Only used when
	// CommunityGoals is enabled.
	CommunityGoalsMaxAmount int          `json:"communityGoalsMaxAmount"`
	Chat                    ChatPresence `json:"chat"`
	ChatLogs                *bool        `json:"chatLogs,omitempty"`
	Bet                     BetSettings  `json:"bet"`
	Preference              Preference   `json:"preference,omitempty"`
}

func DefaultStreamerSettings() StreamerSettings {
	return StreamerSettings{
		MakePredictions:          true,
		FollowRaid:               true,
		ClaimDrops:               true,
		ClaimMoments:             true,
		WatchStreak:              true,
		CommunityGoals:           false,
		CommunityGoalsMaxPercent: 10,
		CommunityGoalsMaxAmount:  0,
		Chat:                     ChatOnline,
		Bet:                      DefaultBetSettings(),
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
		events.Record(events.TypeStreamerOffline, s.Username, "")
	}
}

func (s *Streamer) SetOnline() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.IsOnline {
		s.OnlineAt = time.Now()
		s.IsOnline = true
		s.Stream.InitWatchStreak()
		events.Record(events.TypeStreamerOnline, s.Username, "")
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

// CampaignSummary is a read-only view of an assigned drop campaign, exposed
// for the debug snapshot without handing out the mutable *Campaign itself.
type CampaignSummary struct {
	Name              string
	Game              string
	EndAt             time.Time
	ChannelRestricted bool
	RemainingDrops    int
}

// ActiveCampaignsSummary returns summaries of the drop campaigns currently
// assigned to this streamer's stream (empty when offline or none match).
func (s *Streamer) ActiveCampaignsSummary() []CampaignSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()

	summaries := make([]CampaignSummary, 0, len(s.Stream.Campaigns))
	for _, c := range s.Stream.Campaigns {
		summary := CampaignSummary{
			Name:              c.Name,
			EndAt:             c.EndAt,
			ChannelRestricted: c.IsChannelRestricted(),
			RemainingDrops:    len(c.Drops),
		}
		if c.Game != nil {
			summary.Game = c.Game.Name
		}
		summaries = append(summaries, summary)
	}
	return summaries
}

// CampaignProgress is a compact, read-only view of the drop campaign a
// streamer is currently progressing, sized for the dashboard mini progress
// bar. Percent is the campaign's overall 0-100 progress toward its reward.
type CampaignProgress struct {
	CampaignName      string
	Game              string
	DropName          string
	Percent           int
	MinutesWatched    int
	MinutesRequired   int
	ChannelRestricted bool
}

// ActiveCampaignProgress returns a compact progress summary of the assigned
// drop campaign this streamer is furthest along on, for the dashboard mini
// progress bar. It returns nil when the streamer has no assigned campaign
// with a measurable current drop.
func (s *Streamer) ActiveCampaignProgress() *CampaignProgress {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var best *Campaign
	bestPct := -1
	for _, c := range s.Stream.Campaigns {
		if c.CurrentDrop() == nil {
			continue
		}
		if pct := c.OverallProgressPercent(); pct > bestPct {
			bestPct = pct
			best = c
		}
	}
	if best == nil {
		return nil
	}

	drop := best.CurrentDrop()
	progress := &CampaignProgress{
		CampaignName:      best.Name,
		DropName:          drop.Name,
		Percent:           best.OverallProgressPercent(),
		MinutesWatched:    drop.CurrentMinutesWatched,
		MinutesRequired:   drop.MinutesRequired,
		ChannelRestricted: best.IsChannelRestricted(),
	}
	if best.Game != nil {
		progress.Game = best.Game.Name
	}
	return progress
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
