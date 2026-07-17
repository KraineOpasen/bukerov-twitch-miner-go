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
	// streakEarnedBroadcastID remembers WHICH broadcast the last watch-streak
	// grant belonged to, and streakEarnedAt when it was granted. Twitch (by
	// all observed data — never two grants on one broadcast) pays a streak at
	// most once per broadcast, so pursuing one again on the same BroadcastID
	// only burns the boost slot and emits misleading WARNs. Both fields are
	// owned by mu like every other Stream field; they survive an
	// InitWatchStreak re-arm on purpose (a blip must not forget the grant)
	// and are only ever overwritten by MarkStreakEarned/HydrateStreakGrant.
	streakEarnedBroadcastID string
	streakEarnedAt          time.Time
	MinuteWatched           float64

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

	// The broadcast ID is the identity of the current live session. Two rules:
	//  - never clobber a known ID with an empty one (a partial GQL response
	//    must not un-identify the stream);
	//  - a CHANGED non-empty ID is the authoritative "new broadcast" signal,
	//    so the watch streak re-arms HERE — not (only) on the offline-duration
	//    heuristic in Streamer.SetOnline. This also covers the stale-cache
	//    window where a quick restream reuses the online transition before
	//    UpdateRequired's 10-minute cache expires: the re-arm then happens on
	//    the first refresh that observes the new ID.
	if broadcastID != "" {
		if s.BroadcastID != "" && broadcastID != s.BroadcastID {
			s.armWatchStreakLocked()
		}
		s.BroadcastID = broadcastID
	}
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

// GetBroadcastID returns the Twitch broadcast (stream) ID of the current
// stream, or "" until the api client's first successful stream-info fetch. The
// value is the Twitch stream.id (set by Update): stable for the lifetime of one
// broadcast and different for a new one. It is exposed purely so the diagnostic
// logs can carry a broadcast identity that tells a slot re-assignment on the
// SAME broadcast apart from an attempt on a NEW one; no selection logic consults
// it. Takes the stream lock like every other accessor — never read the field
// directly off the goroutines that log it.
func (s *Stream) GetBroadcastID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.BroadcastID
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
	s.armWatchStreakLocked()
}

// armWatchStreakLocked is the shared re-arm primitive (caller holds mu). It
// deliberately does NOT touch streakEarnedBroadcastID/streakEarnedAt: a
// re-arm caused by a blip on the SAME broadcast must not forget that the
// streak was already granted there — that amnesia was exactly the phantom-
// pursuit bug. StreakPending compares the remembered grant against the
// current BroadcastID, so a grant from a previous broadcast never blocks a
// genuinely new one.
func (s *Stream) armWatchStreakLocked() {
	s.WatchStreakMissing = true
	s.MinuteWatched = 0
	s.minuteWatchedUpdated = time.Time{}
}

// MarkStreakEarned records a live watch-streak grant: the streak is no longer
// missing, and it belongs to the given broadcast (empty when the broadcast
// was not yet identified — then only the missing flag is cleared and the next
// re-arm falls back to the old time-based behavior).
func (s *Stream) MarkStreakEarned(broadcastID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.WatchStreakMissing = false
	s.streakEarnedBroadcastID = broadcastID
	s.streakEarnedAt = time.Now()
}

// HydrateStreakGrant seeds a persisted grant (streak cache) into a freshly
// created Stream at startup. It intentionally leaves WatchStreakMissing true:
// the block is enforced by StreakPending comparing broadcast IDs, so if the
// channel is meanwhile on a NEW broadcast the pursuit starts normally.
func (s *Stream) HydrateStreakGrant(broadcastID string, grantedAt time.Time) {
	if broadcastID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streakEarnedBroadcastID = broadcastID
	s.streakEarnedAt = grantedAt
}

// StreakEarnedGrant returns the remembered grant (broadcast ID + time), for
// the persistence layer. Empty ID means no identified grant.
func (s *Stream) StreakEarnedGrant() (string, time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.streakEarnedBroadcastID, s.streakEarnedAt
}

// StreakPending reports whether a watch streak is still worth pursuing on the
// CURRENT broadcast. It is the single predicate the watcher uses (under mu,
// racefree):
//   - streak not missing -> false (granted and no re-arm since);
//   - broadcast identified -> pending only if the remembered grant belongs to
//     a DIFFERENT (or no) broadcast;
//   - broadcast not identified yet -> if a grant is remembered, pursuit is
//     DEFERRED until the broadcast is identified (never burn the boost slot
//     blind right after a restart); with no remembered grant this degrades to
//     the historical behavior and pursues.
func (s *Stream) StreakPending() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.WatchStreakMissing {
		return false
	}
	if s.BroadcastID != "" {
		return s.streakEarnedBroadcastID == "" || s.BroadcastID != s.streakEarnedBroadcastID
	}
	return s.streakEarnedBroadcastID == ""
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
