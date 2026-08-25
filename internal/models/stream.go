package models

import (
	"encoding/base64"
	"encoding/json"
	"sort"
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

	MinuteWatched float64

	// streakWatchEvents counts the real "WATCH" points-earned events Twitch has
	// delivered for the CURRENT broadcast. It is evidence (not proof of a grant)
	// that Twitch is actually crediting our view — two of them is a far more
	// reliable "the view is being counted" signal than a wall-clock timer (see
	// rdavydov/Twitch-Channel-Points-Miner-v2#782). Session-local: reset to 0 by
	// armWatchStreakLocked whenever the streak re-arms (new broadcast / fresh
	// online), so it never carries across broadcasts and a restart starts it at
	// zero. It is diagnostic delivery evidence only: it never releases pursuit
	// and never records a 300-450 grant (that stays exclusively the
	// WATCH_STREAK points event).
	streakWatchEvents int

	// Watch-streak terminal facts and exact event identities are owned by this
	// Stream and guarded by mu. The timeout is bound to one exact BroadcastID;
	// grants form a persisted ledger keyed by the canonical PubSub event
	// fingerprint. Facts do not expire by wall-clock age: only an exact
	// BroadcastID replacement may re-arm pursuit, and exact replay identity must
	// survive restart. Keeping facts rather than a process-local boolean makes
	// restart, slot loss and broadcast replacement derive the same verdict.
	streakTimeout  *WatchStreakTimeout
	streakGrants   map[string]WatchStreakGrantFact
	streakRevision uint64

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

// WatchStreakPursuitCapMinutes is the single behavioral pursuit cap. It is
// continuous successfully-delivered watch evidence for one exact BroadcastID;
// neither the historical seven-minute hint nor the fifteen-minute diagnostic
// reference participates in state transitions.
const WatchStreakPursuitCapMinutes = 20.0

// WatchStreakState is the active pursuit state for the current BroadcastID.
// GRANTED_UNBOUND is intentionally not a current-broadcast state: it is a grant
// ledger fact that cannot change a pursuit whose BroadcastID was not proven.
type WatchStreakState string

const (
	WatchStreakUnidentified    WatchStreakState = "UNIDENTIFIED"
	WatchStreakEligible        WatchStreakState = "ELIGIBLE"
	WatchStreakPursuing        WatchStreakState = "PURSUING"
	WatchStreakGranted         WatchStreakState = "GRANTED"
	WatchStreakTimedOutUnknown WatchStreakState = "TIMED_OUT_UNKNOWN"
)

type WatchStreakGrantBinding string

const (
	WatchStreakGrantBound   WatchStreakGrantBinding = "GRANTED"
	WatchStreakGrantUnbound WatchStreakGrantBinding = "GRANTED_UNBOUND"
)

// WatchStreakGrantFact is one accepted authoritative WATCH_STREAK event.
// EventID is an opaque canonical payload fingerprint used only for exact replay
// admission. AcceptedAt is local admission time; it is never used to infer a
// BroadcastID. BroadcastID is non-empty only when an independent source proved
// the binding.
type WatchStreakGrantFact struct {
	EventID     string                  `json:"eventId"`
	Binding     WatchStreakGrantBinding `json:"binding"`
	BroadcastID string                  `json:"broadcastId,omitempty"`
	AcceptedAt  time.Time               `json:"acceptedAt"`
}

type WatchStreakTimeout struct {
	BroadcastID string    `json:"broadcastId"`
	TimedOutAt  time.Time `json:"timedOutAt"`
}

// WatchStreakPersistence is an immutable, caller-owned full snapshot of every
// restart-relevant streak fact. Grants are sorted by EventID for deterministic
// cache bytes.
type WatchStreakPersistence struct {
	Revision uint64                 `json:"revision"`
	Timeout  *WatchStreakTimeout    `json:"timeout,omitempty"`
	Grants   []WatchStreakGrantFact `json:"grants,omitempty"`
}

// WatchStreakDecision is one atomic verdict for every selection/protection and
// diagnostic caller. Transitioned is true only for the call that first latches
// the current broadcast's 20-minute timeout; Persistence is then the exact
// under-lock snapshot that must be written by the existing cache owner.
type WatchStreakDecision struct {
	State             WatchStreakState
	BroadcastID       string
	ContinuousMinutes float64
	WatchEvidence     int
	PursuitEligible   bool
	Transitioned      bool
	Persistence       WatchStreakPersistence
}

type WatchStreakGrantEvent struct {
	EventID           string
	AcceptedAt        time.Time
	ProvenBroadcastID string
}

type WatchStreakGrantAdmission string

const (
	WatchStreakGrantNewBound   WatchStreakGrantAdmission = "NEW_BOUND"
	WatchStreakGrantNewUnbound WatchStreakGrantAdmission = "NEW_UNBOUND"
	WatchStreakGrantDuplicate  WatchStreakGrantAdmission = "DUPLICATE"
	WatchStreakGrantInvalid    WatchStreakGrantAdmission = "INVALID"
)

type WatchStreakGrantResult struct {
	Admission   WatchStreakGrantAdmission
	Decision    WatchStreakDecision
	Persistence WatchStreakPersistence
}

func (r WatchStreakGrantResult) NewlyAccepted() bool {
	return r.Admission == WatchStreakGrantNewBound || r.Admission == WatchStreakGrantNewUnbound
}

func NewStream() *Stream {
	return &Stream{}
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

// armWatchStreakLocked resets only current-session evidence (caller holds mu).
// Broadcast-bound terminal facts and accepted grant identities deliberately
// survive: exact-ID matching makes a genuinely new broadcast eligible while a
// same-broadcast blip/restart cannot erase GRANTED or TIMED_OUT_UNKNOWN.
func (s *Stream) armWatchStreakLocked() {
	s.MinuteWatched = 0
	s.minuteWatchedUpdated = time.Time{}
	// The WATCH-evidence counter is session-local to one broadcast's pursuit, so
	// it resets together with the minute counter and its timestamp — never carried
	// across broadcasts (that would let a stale count short-circuit a fresh
	// pursuit, and, symmetrically, a reset that forgot it would let the counter
	// keep growing across broadcasts).
	s.streakWatchEvents = 0
}

func (s *Stream) watchStreakPersistenceLocked() WatchStreakPersistence {
	p := WatchStreakPersistence{Revision: s.streakRevision}
	if s.streakTimeout != nil {
		timeout := *s.streakTimeout
		p.Timeout = &timeout
	}
	if len(s.streakGrants) > 0 {
		p.Grants = make([]WatchStreakGrantFact, 0, len(s.streakGrants))
		for _, grant := range s.streakGrants {
			p.Grants = append(p.Grants, grant)
		}
		sort.Slice(p.Grants, func(i, j int) bool { return p.Grants[i].EventID < p.Grants[j].EventID })
	}
	return p
}

func (s *Stream) WatchStreakPersistence() WatchStreakPersistence {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.watchStreakPersistenceLocked()
}

func (s *Stream) hasBoundGrantLocked(broadcastID string) bool {
	if broadcastID == "" {
		return false
	}
	for _, grant := range s.streakGrants {
		if grant.Binding == WatchStreakGrantBound && grant.BroadcastID == broadcastID {
			return true
		}
	}
	return false
}

func (s *Stream) hasAnyBoundGrantLocked() bool {
	for _, grant := range s.streakGrants {
		if grant.Binding == WatchStreakGrantBound && grant.BroadcastID != "" {
			return true
		}
	}
	return false
}

func (s *Stream) watchStreakDecisionLocked(now time.Time, allowTimeout bool) WatchStreakDecision {
	d := WatchStreakDecision{
		BroadcastID:       s.BroadcastID,
		ContinuousMinutes: s.MinuteWatched,
		WatchEvidence:     s.streakWatchEvents,
	}

	switch {
	case s.BroadcastID == "":
		d.State = WatchStreakUnidentified
	case s.hasBoundGrantLocked(s.BroadcastID):
		d.State = WatchStreakGranted
	case s.streakTimeout != nil && s.streakTimeout.BroadcastID == s.BroadcastID:
		d.State = WatchStreakTimedOutUnknown
	case allowTimeout && s.MinuteWatched >= WatchStreakPursuitCapMinutes:
		if now.IsZero() {
			now = time.Now()
		}
		s.streakTimeout = &WatchStreakTimeout{BroadcastID: s.BroadcastID, TimedOutAt: now}
		s.streakRevision++
		d.State = WatchStreakTimedOutUnknown
		d.Transitioned = true
		d.Persistence = s.watchStreakPersistenceLocked()
	case s.MinuteWatched > 0:
		d.State = WatchStreakPursuing
		d.PursuitEligible = true
	default:
		d.State = WatchStreakEligible
		d.PursuitEligible = true
	}
	return d
}

// EvaluateWatchStreak is the single mutating eligibility/timeout verdict used
// by every production selection and protection path. At exactly 20 delivered
// continuous minutes it atomically latches TIMED_OUT_UNKNOWN to the current
// exact BroadcastID and returns the persistence snapshot from that transition.
func (s *Stream) EvaluateWatchStreak(now time.Time) WatchStreakDecision {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.watchStreakDecisionLocked(now, true)
}

// AcceptWatchStreakGrant atomically admits an authoritative grant event. An
// empty ProvenBroadcastID is always GRANTED_UNBOUND; the current BroadcastID is
// never substituted. Exact EventID replay is a no-op across every downstream
// side effect because only one caller can receive a newly-accepted result.
func (s *Stream) AcceptWatchStreakGrant(event WatchStreakGrantEvent) WatchStreakGrantResult {
	s.mu.Lock()
	defer s.mu.Unlock()

	if event.EventID == "" {
		return WatchStreakGrantResult{
			Admission: WatchStreakGrantInvalid,
			Decision:  s.watchStreakDecisionLocked(event.AcceptedAt, false),
		}
	}
	if _, exists := s.streakGrants[event.EventID]; exists {
		return WatchStreakGrantResult{
			Admission: WatchStreakGrantDuplicate,
			Decision:  s.watchStreakDecisionLocked(event.AcceptedAt, false),
		}
	}
	if event.AcceptedAt.IsZero() {
		event.AcceptedAt = time.Now()
	}
	if s.streakGrants == nil {
		s.streakGrants = make(map[string]WatchStreakGrantFact)
	}

	fact := WatchStreakGrantFact{
		EventID:    event.EventID,
		AcceptedAt: event.AcceptedAt,
	}
	admission := WatchStreakGrantNewUnbound
	if event.ProvenBroadcastID != "" {
		fact.Binding = WatchStreakGrantBound
		fact.BroadcastID = event.ProvenBroadcastID
		admission = WatchStreakGrantNewBound
	} else {
		fact.Binding = WatchStreakGrantUnbound
	}
	s.streakGrants[event.EventID] = fact
	s.streakRevision++
	persistence := s.watchStreakPersistenceLocked()
	return WatchStreakGrantResult{
		Admission:   admission,
		Decision:    s.watchStreakDecisionLocked(event.AcceptedAt, false),
		Persistence: persistence,
	}
}

// HydrateWatchStreak restores only validated cache facts. It never manufactures
// success: malformed bindings are skipped and the maximum persisted revision is
// retained for stale-write rejection.
func (s *Stream) HydrateWatchStreak(p WatchStreakPersistence) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.streakRevision = p.Revision
	s.streakTimeout = nil
	if p.Timeout != nil && p.Timeout.BroadcastID != "" && !p.Timeout.TimedOutAt.IsZero() {
		timeout := *p.Timeout
		s.streakTimeout = &timeout
	}
	s.streakGrants = make(map[string]WatchStreakGrantFact, len(p.Grants))
	for _, grant := range p.Grants {
		validBound := grant.Binding == WatchStreakGrantBound && grant.BroadcastID != ""
		validUnbound := grant.Binding == WatchStreakGrantUnbound && grant.BroadcastID == ""
		if grant.EventID == "" || grant.AcceptedAt.IsZero() || (!validBound && !validUnbound) {
			continue
		}
		s.streakGrants[grant.EventID] = grant
	}
}

// StreakPending is a compatibility diagnostic: true means the current outcome
// remains unresolved, including TIMED_OUT_UNKNOWN. Behavioral selection never
// calls it; EvaluateWatchStreak owns pursuit eligibility.
func (s *Stream) StreakPending() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.BroadcastID == "" {
		return !s.hasAnyBoundGrantLocked()
	}
	return !s.hasBoundGrantLocked(s.BroadcastID)
}

// StreakEarnedGrant returns the newest proven bound grant for compatibility
// diagnostics. Unbound grants deliberately return no invented BroadcastID.
func (s *Stream) StreakEarnedGrant() (string, time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var bid string
	var at time.Time
	for _, grant := range s.streakGrants {
		if grant.Binding == WatchStreakGrantBound && grant.AcceptedAt.After(at) {
			bid, at = grant.BroadcastID, grant.AcceptedAt
		}
	}
	return bid, at
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
// continuous viewing. Grant identities and the broadcast-bound timeout are left
// intact, so a mere rotation never creates another 20-minute window.
func (s *Stream) ResetWatchContinuity() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.MinuteWatched = 0
	s.minuteWatchedUpdated = time.Time{}
}

// StreakPursuitTimedOut is a compatibility diagnostic. It is a pure exact-ID
// read; only EvaluateWatchStreak can create the timeout transition.
func (s *Stream) StreakPursuitTimedOut() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.BroadcastID != "" && s.streakTimeout != nil && s.streakTimeout.BroadcastID == s.BroadcastID
}

func (s *Stream) GetMinuteWatched() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.MinuteWatched
}

func (s *Stream) GetWatchStreakMissing() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.BroadcastID == "" {
		return !s.hasAnyBoundGrantLocked()
	}
	return !s.hasBoundGrantLocked(s.BroadcastID)
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
