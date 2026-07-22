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
	// campaignAvailability is the tri-state result of the last channel-side
	// per-channel campaign lookup (see campaign_availability.go). Unknown means
	// the lookup failed and CampaignIDs is stale continuity data, not a fresh
	// authoritative "available here". Owned by mu like every other Stream field.
	//
	// campaignAvailObs is the monotonic OBSERVATION generation (mirrors the Channel
	// Points context's capObs): each lookup begins a new observation BEFORE its I/O
	// and only the latest-begun observation may publish, so a newer request always
	// wins regardless of completion order (newest-STARTED-wins, not
	// first-completion-wins). campaignAvailObservedAt/UnknownSince/LastKnownAt carry
	// the bounded-continuity timestamps: UnknownSince is stamped at the first
	// Unknown after a Known and preserved across a run of Unknowns (so repeated
	// failures never extend the grace); LastKnownAt is the last authoritative Known.
	campaignAvailability    CampaignAvailabilityState
	campaignAvailObs        uint64
	campaignAvailObservedAt time.Time
	campaignUnknownSince    time.Time
	campaignLastKnownAt     time.Time

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

	// streakWatchEvents counts the real "WATCH" points-earned events Twitch has
	// delivered for the CURRENT broadcast. It is evidence (not proof of a grant)
	// that Twitch is actually crediting our view — two of them is a far more
	// reliable "the view is being counted" signal than a wall-clock timer (see
	// rdavydov/Twitch-Channel-Points-Miner-v2#782). Session-local: reset to 0 by
	// armWatchStreakLocked whenever the streak re-arms (new broadcast / fresh
	// online), so it never carries across broadcasts and a restart starts it at
	// zero. It only ever RELEASES the streak boost seat — it never records a
	// 300-450 grant (that stays exclusively the WATCH_STREAK points event).
	streakWatchEvents int

	// streakPursuitTimedOut latches once the bounded streak-pursuit window (the
	// hard cap of CONTINUOUSLY-watched minutes) elapsed for the CURRENT broadcast
	// with no authoritative WATCH_STREAK grant. It is what stops the watcher from
	// re-opening a fresh pursuit window for the same broadcast after a real slot
	// loss resets the continuous-minute counter (ResetWatchContinuity): without it,
	// exhaustion is measured only by MinuteWatched, which the slot-loss reset zeroes,
	// so the same broadcast would cycle 20-minute pursuit windows forever. Set the
	// first time StreakPursuitExhausted observes the cap; deliberately NOT cleared by
	// ResetWatchContinuity or by WATCH evidence; cleared only on re-arm (a genuinely
	// new broadcast, via armWatchStreakLocked). It never awards points and never
	// substitutes for the WATCH_STREAK grant — a late real grant is still accepted
	// and recorded once (StreakPending is unaffected by this field).
	streakPursuitTimedOut bool

	// spadeURL is written by the api client (stream bring-up, session refresh)
	// and read by the minute sender and health probes on other goroutines —
	// unexported so every access takes the lock.
	spadeURL string

	payload              []MinuteWatchedEvent
	lastUpdate           time.Time
	minuteWatchedUpdated time.Time

	// sessionGen is the monotonic PLAYBACK-SESSION generation: it increments on
	// every change to the coherent watch session a beacon depends on — a new (or
	// changed) broadcast ID, a new spade URL, or a re-published beacon payload.
	// The minute sender captures it alongside a SessionSnapshot and re-checks it
	// just before the beacon, so a session that changed mid-send (a new broadcast,
	// a completed refresh) is detected and the stale beacon suppressed instead of
	// mixing an old payload with a new spade URL. Its zero value means the session
	// is uninitialised (no bring-online yet).
	//
	// sessionObs is the monotonic full-session OBSERVATION generation (mirrors
	// campaignAvailObs / the Channel Points capObs): a full session refresh begins
	// a new observation BEFORE its network I/O and only the latest-begun
	// observation may publish the spade URL, so a slow older refresh can never
	// clobber a newer one (newest-STARTED-wins, not first-completion-wins). Both
	// are owned by mu like every other Stream field.
	sessionGen uint64
	sessionObs uint64

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
		if broadcastID != s.BroadcastID {
			if s.BroadcastID != "" {
				s.armWatchStreakLocked()
			}
			// A new (or first) broadcast is a new playback session: bump the
			// generation so any beacon captured against the previous broadcast is
			// treated as stale, and a stale full-session refresh started against the
			// old broadcast cannot be published over it.
			s.sessionGen++
		}
		s.BroadcastID = broadcastID
	}
	s.Title = title
	s.Game = game
	s.Tags = tags
	s.ViewersCount = viewersCount
	s.lastUpdate = time.Now()
}

// StreamUpdateInterval is the normal cadence at which the api client refreshes a
// stream's info (UpdateRequired's threshold). It is named so downstream policy —
// notably the campaign-availability continuity grace — can be DERIVED from it
// rather than duplicating the magic duration.
const StreamUpdateInterval = 2 * time.Minute

func (s *Stream) UpdateRequired() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.lastUpdate.IsZero() {
		return true
	}
	return time.Since(s.lastUpdate) >= StreamUpdateInterval
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

// SetSpadeURL records the spade endpoint (api client only). It bumps the
// playback-session generation when the URL actually changes, so a beacon
// captured against the previous spade endpoint is treated as stale.
func (s *Stream) SetSpadeURL(url string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setSpadeURLLocked(url)
}

// setSpadeURLLocked publishes a spade URL under the caller-held lock, bumping the
// session generation only on a real change (a no-op re-publish must not appear as
// a session change and needlessly stale an in-flight send).
func (s *Stream) setSpadeURLLocked(url string) {
	if url == s.spadeURL {
		return
	}
	s.spadeURL = url
	s.sessionGen++
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

// SetCampaignIDs replaces the advertised campaign ID list and marks channel-side
// availability KNOWN (a set of resolved IDs is by definition a resolved lookup).
// Production uses the observation-guarded BeginCampaignAvailabilityObservation /
// ApplyCampaignAvailability pair; this setter remains for single-goroutine test
// setup and any legacy caller. It bumps the observation generation (so any
// in-flight older observation becomes stale) and resets the Unknown-continuity
// timestamps, since a fresh Known list ends any prior unknown streak.
func (s *Stream) SetCampaignIDs(ids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.campaignAvailObs++
	s.CampaignIDs = ids
	s.campaignAvailability = CampaignAvailabilityKnown
	s.campaignAvailObservedAt = time.Now()
	s.campaignLastKnownAt = s.campaignAvailObservedAt
	s.campaignUnknownSince = time.Time{}
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

// BuildMinuteWatchedPayload builds the beacon events for a minute-watched report
// from the identity of one observed broadcast. It is pure (no lock, no I/O), so a
// full session refresh can build the payload off-lock as part of an immutable
// PlaybackSessionCandidate and publish it atomically with the spade URL and
// broadcast ID (see ApplyPlaybackSessionIfCurrent), instead of via a separate
// SetPayload write.
func BuildMinuteWatchedPayload(channelID, broadcastID, userID, channel string, game *Game) []MinuteWatchedEvent {
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

	return []MinuteWatchedEvent{
		{
			Event:      "minute-watched",
			Properties: properties,
		},
	}
}

func (s *Stream) SetPayload(channelID, broadcastID, userID, channel string, game *Game) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.payload = BuildMinuteWatchedPayload(channelID, broadcastID, userID, channel, game)
	// A freshly built payload (new broadcast, refreshed game/user context) is a new
	// playback session for beacon purposes.
	s.sessionGen++
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
	// The WATCH-evidence counter is session-local to one broadcast's pursuit, so
	// it resets together with the minute counter and its timestamp — never carried
	// across broadcasts (that would let a stale count short-circuit a fresh
	// pursuit, and, symmetrically, a reset that forgot it would let the counter
	// keep growing across broadcasts).
	s.streakWatchEvents = 0
	// The pursuit-timeout latch is bound to one broadcast: a genuinely new
	// broadcast is a fresh streak worth pursuing, so clear it here (the ONLY place
	// it clears). It is intentionally NOT cleared on a mere continuity reset.
	s.streakPursuitTimedOut = false
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

// NoteWatchPointsEvent records that Twitch delivered a real "WATCH" points-earned
// event for the current broadcast (evidence the view is being credited) and
// returns the new count. Called from the PubSub handler; racefree via mu. It is
// deliberately additive-only and session-local (reset on re-arm), so a PubSub
// reconnect — which resubscribes for FUTURE events and never replays past ones —
// cannot double-count a prior broadcast's evidence into a fresh pursuit.
func (s *Stream) NoteWatchPointsEvent() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streakWatchEvents++
	return s.streakWatchEvents
}

// StreakWatchEvidence returns how many real WATCH points events Twitch has
// delivered for the current broadcast (see streakWatchEvents).
func (s *Stream) StreakWatchEvidence() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.streakWatchEvents
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

// ResetWatchContinuity breaks the continuous watched-minutes accumulator the
// instant the channel stops being watched for a reason the in-band report gap
// cannot see — specifically a real watch-slot loss/switch, which the watcher
// detects as a held->released transition and reports here. It zeroes ONLY
// MinuteWatched and its timestamp, so the next successful report re-anchors from
// zero exactly like a fresh continuity segment and the wall-clock interval during
// which the channel held no slot is never credited.
//
// It complements UpdateMinuteWatched's own gap>maxGap reset: that catches a missed
// report while the slot is still HELD; this catches the slot itself being lost and
// regained within maxGap, which the timestamp gap alone cannot distinguish from
// continuous viewing. The streak IDENTITY is deliberately left intact —
// WatchStreakMissing, streakEarnedBroadcastID/At, the streakWatchEvents evidence
// counter, and the streakPursuitTimedOut latch are untouched — so StreakPending is
// unchanged (a late real WATCH_STREAK is still accepted), a mere rotation never
// re-arms the pursuit, and a broadcast that already timed out is NOT handed a fresh
// pursuit window just because its continuous minutes were reset.
func (s *Stream) ResetWatchContinuity() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.MinuteWatched = 0
	s.minuteWatchedUpdated = time.Time{}
}

// StreakPursuitExhausted reports whether the bounded streak-pursuit window has
// elapsed for the current broadcast, LATCHING that timed-out state the first time
// the continuously-watched minutes reach capMinutes. The latch is what makes the
// decision survive the continuity reset a slot loss triggers: once a broadcast has
// burned its bounded window it is never granted a fresh one (StreakPursuitExhausted
// keeps returning true even after MinuteWatched is reset to zero), until a genuinely
// new broadcast re-arms via armWatchStreakLocked. Setting the latch is done here,
// atomically with the exhaustion decision the watcher's isBoostEligible consults, so
// there is no separate "release" call to keep in sync. capMinutes is passed in so
// the watcher owns the policy constant; a non-positive cap disables the
// minutes-based trigger (the latch, once set, still holds). It awards no points and
// never marks the streak earned — that stays exclusively the WATCH_STREAK grant.
func (s *Stream) StreakPursuitExhausted(capMinutes float64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streakPursuitTimedOut {
		return true
	}
	if capMinutes > 0 && s.MinuteWatched >= capMinutes {
		s.streakPursuitTimedOut = true
		return true
	}
	return false
}

// StreakPursuitTimedOut reports whether the bounded pursuit window has already
// latched timed-out for the current broadcast (see streakPursuitTimedOut). Pure
// read; it neither sets the latch nor consults the minute counter.
func (s *Stream) StreakPursuitTimedOut() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.streakPursuitTimedOut
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

// GetTags returns a copy of the current stream tags, so callers can render
// them without holding the lock or racing the next Update.
func (s *Stream) GetTags() []Tag {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.Tags) == 0 {
		return nil
	}
	out := make([]Tag, len(s.Tags))
	copy(out, s.Tags)
	return out
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
