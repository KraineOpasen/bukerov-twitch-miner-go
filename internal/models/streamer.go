package models

import (
	"fmt"
	"sort"
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
	// DisableWatch is a hard opt-out from the watch rotation: when true the bot
	// never sends minute-watched events for this streamer, even when it's the
	// only online channel (unlike PreferenceAvoid, which is only a soft
	// exclusion). The streamer is still tracked (points, pubsub, status) - it
	// just never occupies one of the two Twitch watch slots.
	DisableWatch bool `json:"disableWatch,omitempty"`
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

// StreamerStatus is the strict tri-state liveness of a streamer. Its zero value
// is StatusUnknown by construction (uint8 iota) so a freshly created / never
// checked streamer is UNKNOWN, never a false "offline". Only an authoritative
// positive signal makes a streamer online, and only an authoritative negative
// signal makes it offline; every inconclusive outcome (network/GQL/PQNF/timeout/
// malformed/Spade failure) is UNKNOWN, which must not be treated as offline.
type StreamerStatus uint8

const (
	StatusUnknown StreamerStatus = iota // zero value — state not authoritatively confirmed
	StatusOnline                        // confirmed live
	StatusOffline                       // authoritatively confirmed not live
)

func (s StreamerStatus) String() string {
	switch s {
	case StatusOnline:
		return "online"
	case StatusOffline:
		return "offline"
	default:
		return "unknown"
	}
}

// StatusReason is a compact, privacy-safe classification of WHY a streamer's
// current status is what it is (chiefly why it is unknown). It carries no raw
// Twitch payload, token, cookie, header or claim identifier — only a stable code.
type StatusReason string

const (
	ReasonInitial               StatusReason = "initial"
	ReasonTransportError        StatusReason = "transport_error"
	ReasonTimeout               StatusReason = "timeout"
	ReasonGraphQLError          StatusReason = "graphql_error"
	ReasonPersistedQueryMissing StatusReason = "persisted_query_not_found"
	ReasonUnauthorized          StatusReason = "unauthorized"
	ReasonMalformedResponse     StatusReason = "malformed_response"
	ReasonSpadeUnavailable      StatusReason = "spade_unavailable"
	ReasonCheckFailed           StatusReason = "check_failed"
)

// StatusTransition is the immutable result of a single atomic status transition.
// The transition method computes it under the Streamer lock and returns it so
// callers can log the change and fire external callbacks OUTSIDE the lock,
// exactly once per logical transition even when detections race.
type StatusTransition struct {
	Previous StreamerStatus
	Current  StreamerStatus
	Reason   StatusReason
	// OnlineConfirmed is true only on a genuine offline/first-detection→online
	// transition worth logging and notifying — NOT on a recovery continuation
	// (unknown whose last confirmed status was online → online).
	OnlineConfirmed bool
	// OfflineConfirmed is true only on a genuine online→offline authoritative
	// transition — NOT on an initial-unknown→offline (streamer never was online)
	// nor on re-confirming an already-offline streamer.
	OfflineConfirmed bool
	// Stale is true when a seq-guarded check result was discarded because a newer
	// authoritative transition had already been applied (see ApplyCheckResultIfCurrent).
	Stale bool
}

// Changed reports whether this transition actually moved the status.
func (t StatusTransition) Changed() bool { return t.Previous != t.Current }

type Streamer struct {
	Username string
	// ChannelID and Settings are configuration; Status and the timestamps below
	// are the single writable source of truth for liveness (there is no separate
	// IsOnline bool — GetIsOnline() is a derived helper).
	ChannelID string
	Settings  StreamerSettings

	// Status is the current authoritative liveness (single source of truth).
	Status StreamerStatus
	// LastConfirmedStatus is the last status that was authoritatively confirmed
	// (only ever StatusOnline or StatusOffline; StatusUnknown until the first
	// authoritative signal). It survives transitions INTO unknown so callers can
	// distinguish initial-unknown from unknown-after-online / unknown-after-offline.
	LastConfirmedStatus StreamerStatus
	// StatusReason is the compact code for the current status (mainly why unknown).
	StatusReason StatusReason
	// LastConfirmedAt is when Status last became a confirmed (online/offline) value.
	LastConfirmedAt time.Time
	// UnknownSince is when the status last became unknown (zero when not unknown).
	UnknownSince time.Time
	// StatusChangedAt is when Status last changed value.
	StatusChangedAt time.Time
	// statusSeq increments on every CONFIRMED (online/offline) transition. A
	// network check captures it BEFORE its I/O and applies its result only if it
	// is unchanged, so a slow/stale check can never overwrite a newer authoritative
	// PubSub transition. Unknown transitions do NOT bump it, so a genuine confirm
	// always wins over a concurrent inconclusive check.
	statusSeq uint64

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
		Username: username,
		Settings: settings,
		// Status is left at its zero value (StatusUnknown): a freshly created
		// streamer is UNKNOWN, not offline. OfflineAt/OnlineAt/LastConfirmedAt
		// are intentionally left zero — do not stamp an offline at creation.
		StatusReason:   ReasonInitial,
		CommunityGoals: make(map[string]*CommunityGoal),
		Stream:         NewStream(),
		History:        make(map[string]*HistoryEntry),
	}
}

func (s *Streamer) String() string {
	return fmt.Sprintf("Streamer(%s, %d points)", s.Username, s.ChannelPoints)
}

// watchStreakContinuityGrace is how briefly a streamer may drop offline and
// return while still counting as the SAME continuous broadcast for watch-streak
// purposes. It is now a FALLBACK, consulted only inside applyStatusLocked on a
// genuine new-online detection where no broadcast ID is yet known (BroadcastID
// == ""): the authoritative re-arm lives in Stream.Update's changed-broadcast-ID
// comparison, which already survives online→unknown→online recovery (unknown
// never writes OfflineAt, so the gap heuristic can no longer see a blip). Matches
// StreamUpElapsed's 2-minute notion of a settled stream.
const watchStreakContinuityGrace = 2 * time.Minute

// applyStatusLocked performs one atomic tri-state transition to next with mu
// already held. It mutates only status/timestamp/reason/seq fields (and, on a
// genuine first-online with no known broadcast, arms the streak); it performs NO
// network I/O and emits NO events. It returns the immutable StatusTransition so
// the public wrappers can emit events and fire callbacks after unlocking.
//
// Semantics per target:
//   - StatusOnline: recovery from unknown-whose-last-confirmed-was-online is a
//     CONTINUATION (no OnlineConfirmed, OnlineAt/streak untouched). Any other
//     move into online is a genuine detection (OnlineConfirmed, OnlineAt set,
//     streak re-armed as a fallback only when BroadcastID=="").
//   - StatusOffline: OfflineConfirmed only when the streamer was actually online
//     (prev==online or last-confirmed-online) — never a fake online→offline for a
//     streamer that was never online. OfflineAt is set on a genuine new offline
//     and preserved when merely re-confirming offline.
//   - StatusUnknown: EVENT-FREE. Sets reason + UnknownSince (first time only) and
//     leaves LastConfirmedStatus/LastConfirmedAt/OfflineAt/OnlineAt/Stream/streak
//     untouched, and does NOT bump statusSeq (unknown is a weak observation).
func (s *Streamer) applyStatusLocked(next StreamerStatus, reason StatusReason) StatusTransition {
	prev := s.Status
	now := time.Now()
	s.LastChecked = now
	tr := StatusTransition{Previous: prev, Current: next, Reason: reason}

	switch next {
	case StatusOnline:
		recovery := prev == StatusUnknown && s.LastConfirmedStatus == StatusOnline
		s.Status = StatusOnline
		s.LastConfirmedStatus = StatusOnline
		s.LastConfirmedAt = now
		s.StatusReason = ""
		s.UnknownSince = time.Time{}
		s.statusSeq++
		if prev != StatusOnline {
			s.StatusChangedAt = now
		}
		if prev != StatusOnline && !recovery {
			// Genuine new online detection (offline→online, initial-unknown→online,
			// or unknown-last-offline→online): stamp the broadcast start and, as a
			// fallback, re-arm the streak only when no broadcast ID is known yet
			// (Stream.Update's changed-ID path is the authoritative re-arm).
			s.OnlineAt = now
			if s.Stream.GetBroadcastID() == "" {
				freshBroadcast := s.OfflineAt.IsZero() || now.Sub(s.OfflineAt) >= watchStreakContinuityGrace
				if freshBroadcast {
					s.Stream.InitWatchStreak()
				}
			}
			tr.OnlineConfirmed = true
		}

	case StatusOffline:
		everOnline := prev == StatusOnline || s.LastConfirmedStatus == StatusOnline
		wasOffline := prev == StatusOffline
		s.Status = StatusOffline
		s.LastConfirmedStatus = StatusOffline
		s.LastConfirmedAt = now
		s.StatusReason = ""
		s.UnknownSince = time.Time{}
		s.statusSeq++
		if prev != StatusOffline {
			s.StatusChangedAt = now
		}
		// Set OfflineAt on a genuine new offline (from online) or the first-ever
		// authoritative offline; preserve it when merely re-confirming offline
		// (e.g. unknown-last-offline→offline) so offline duration stays honest.
		if s.OfflineAt.IsZero() || prev == StatusOnline {
			s.OfflineAt = now
		}
		if !wasOffline && everOnline {
			tr.OfflineConfirmed = true
		}

	default: // StatusUnknown
		s.Status = StatusUnknown
		s.StatusReason = reason
		if s.UnknownSince.IsZero() {
			s.UnknownSince = now
		}
		if prev != StatusUnknown {
			s.StatusChangedAt = now
		}
		// Deliberately DO NOT touch LastConfirmedStatus/LastConfirmedAt/OfflineAt/
		// OnlineAt/Stream/broadcastID/payload/campaignIDs/streak/ChannelPoints, and
		// DO NOT bump statusSeq.
	}
	return tr
}

// emitTransition records the diagnostic event for a genuine confirmed transition,
// after the Streamer lock is released. Exactly-once per logical transition: only
// the goroutine that actually flipped the state gets OnlineConfirmed/OfflineConfirmed.
func (s *Streamer) emitTransition(tr StatusTransition) {
	switch {
	case tr.OnlineConfirmed:
		events.Record(events.TypeStreamerOnline, s.Username, "")
	case tr.OfflineConfirmed:
		events.Record(events.TypeStreamerOffline, s.Username, "")
	}
}

// SetConfirmedOnline records an authoritative positive live signal (successful
// GQL with a valid stream object, PubSub stream-up, or a viewcount-confirmed
// check). It returns the transition; tr.OnlineConfirmed is true only on a genuine
// new detection (not a recovery continuation from a transient unknown), so
// callers log/notify exactly once and never on a mere recovery.
func (s *Streamer) SetConfirmedOnline() StatusTransition {
	s.mu.Lock()
	tr := s.applyStatusLocked(StatusOnline, "")
	s.mu.Unlock()
	s.emitTransition(tr)
	return tr
}

// SetConfirmedOffline records an authoritative negative signal (PubSub
// stream-down, or a structurally valid GQL response where the user exists and
// stream is explicitly null). It fires from online OR unknown so a stream-down
// during an unknown blip still settles offline and releases the watch slot.
func (s *Streamer) SetConfirmedOffline() StatusTransition {
	s.mu.Lock()
	tr := s.applyStatusLocked(StatusOffline, "")
	s.mu.Unlock()
	s.emitTransition(tr)
	return tr
}

// SetUnknown records that the current live state could NOT be authoritatively
// confirmed (transport/timeout/GQL/PQNF/auth/malformed/Spade failure, cancelled
// context, ...). It is event-free and preserves all confirmed state: it never
// sets OfflineAt, never emits an offline event, never logs "went offline", never
// clears Stream/broadcastID/payload/campaign IDs, never resets the watch streak,
// and never changes ChannelPoints.
func (s *Streamer) SetUnknown(reason StatusReason) StatusTransition {
	s.mu.Lock()
	tr := s.applyStatusLocked(StatusUnknown, reason)
	s.mu.Unlock()
	s.emitTransition(tr)
	return tr
}

// ApplyCheckResultIfCurrent applies a network stream-check result but ONLY if no
// newer authoritative (online/offline) transition has been applied since obsSeq
// was captured (via StatusSnapshot, before the I/O). This is the stale-observation
// guard: a slow GQL result that observed the stream live cannot resurrect a
// channel an intervening PubSub stream-down already released, and a stale failure
// cannot overwrite a newer confirmed online. If discarded, the returned
// transition has Stale=true and Changed()==false.
func (s *Streamer) ApplyCheckResultIfCurrent(obsSeq uint64, next StreamerStatus, reason StatusReason) StatusTransition {
	s.mu.Lock()
	if obsSeq != s.statusSeq {
		cur := s.Status
		s.mu.Unlock()
		return StatusTransition{Previous: cur, Current: cur, Reason: reason, Stale: true}
	}
	tr := s.applyStatusLocked(next, reason)
	s.mu.Unlock()
	s.emitTransition(tr)
	return tr
}

// StatusSnapshot returns the current status and the status sequence, read under
// the lock. Network callers capture this BEFORE any I/O and pass the sequence to
// ApplyCheckResultIfCurrent so a stale result can be discarded.
func (s *Streamer) StatusSnapshot() (StreamerStatus, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Status, s.statusSeq
}

// GetStatus returns the current tri-state liveness.
func (s *Streamer) GetStatus() StreamerStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Status
}

// GetStatusReason returns the current status reason code.
func (s *Streamer) GetStatusReason() StatusReason {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.StatusReason
}

// GetLastConfirmedStatus returns the last authoritatively confirmed status
// (StatusUnknown until the first confirmation).
func (s *Streamer) GetLastConfirmedStatus() StreamerStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.LastConfirmedStatus
}

// GetUnknownSince returns when the status last became unknown (zero when not
// currently unknown).
func (s *Streamer) GetUnknownSince() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.UnknownSince
}

// SetStreamUpTime records the PubSub stream-up timestamp under the lock (replaces
// the previous unsynchronised field write on the PubSub read-loop goroutine).
func (s *Streamer) SetStreamUpTime(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.StreamUpTime = t
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
		// Recorded via the Stream's own lock — this field is owned by
		// Stream.mu, never written under the Streamer mutex (that split
		// ownership was a data race with the watcher loop's readers).
		s.Stream.MarkStreakEarned(s.Stream.GetBroadcastID())
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

// DropsCondition reports drop-farming eligibility for confirmed-online streamers
// only. Unknown yields false (fail closed), but note SetUnknown never clears the
// campaign IDs, and the watcher retains an already-farming slot during a transient
// unknown — so a brief blip does not lose drop progress.
func (s *Streamer) DropsCondition() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.Settings.ClaimDrops &&
		s.Status == StatusOnline &&
		len(s.Stream.GetCampaignIDs()) > 0
}

// HasChannelRestrictedCampaign reports whether this streamer currently has
// an assigned drop campaign that only credits progress on this specific
// channel (as opposed to any channel streaming the campaign's game). Such a
// campaign cannot be farmed by watching a different streamer, so channel
// selection should prioritize keeping this one watched.
func (s *Streamer) HasChannelRestrictedCampaign() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, c := range s.Stream.GetCampaigns() {
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

	campaigns := s.Stream.GetCampaigns()
	summaries := make([]CampaignSummary, 0, len(campaigns))
	for _, c := range campaigns {
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
	for _, c := range s.Stream.GetCampaigns() {
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

// CommunityGoalProgress is a compact, read-only view of an in-progress
// community goal on this streamer, sized for the dashboard.
type CommunityGoalProgress struct {
	GoalID            string
	Title             string
	PointsContributed int
	GoalAmount        int
	Percent           int
}

// ActiveCommunityGoals returns compact progress views of this streamer's
// community goals that are currently running and in stock, sorted by highest
// completion first. Returns nil when there are none, so callers can hide the
// section entirely. Read-locked; safe from any goroutine.
func (s *Streamer) ActiveCommunityGoals() []CommunityGoalProgress {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []CommunityGoalProgress
	for _, g := range s.CommunityGoals {
		if g == nil || g.Status != CommunityGoalStarted || !g.IsInStock {
			continue
		}
		percent := 0
		if g.GoalAmount > 0 {
			percent = (g.PointsContributed * 100) / g.GoalAmount
			if percent > 100 {
				percent = 100
			}
		}
		out = append(out, CommunityGoalProgress{
			GoalID:            g.GoalID,
			Title:             g.Title,
			PointsContributed: g.PointsContributed,
			GoalAmount:        g.GoalAmount,
			Percent:           percent,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Percent > out[j].Percent })
	return out
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

// GetIsOnline is a derived compatibility helper: it reports CONFIRMED online
// only (Status == StatusOnline). Both unknown and offline return false, so every
// risky-action gate (predictions, raids, chat join, drops, watcher new-slot
// selection, bonus polling) fails closed on unknown automatically. Offline must
// NEVER be derived as !GetIsOnline() — a caller needing confirmed-offline must
// test GetStatus() == StatusOffline.
func (s *Streamer) GetIsOnline() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Status == StatusOnline
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
