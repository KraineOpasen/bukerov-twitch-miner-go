package watcher

import (
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// This file is the broker's Stage 3 surface for the drop-progress watchdog:
// per-channel minute-watched delivery accounting, the staged watch-session
// refresh, and the temporary avoid-list hook. All three follow the same
// ownership rule as the rest of the broker (see MinuteWatcher): external
// goroutines only stage requests or read atomically-published snapshots;
// every mutation of live streamer state happens on the loop goroutine, which
// stays the single writer of the watch session for slotted channels.

// sessionRefresher is the slice of the Twitch client the broker needs to rebuild
// a slotted channel's watch session on demand. It fetches OFF the Stream lock and
// publishes the whole tuple in ONE atomic, optimistic apply guarded by the
// expected broadcast/generation. Narrowed to an interface so refresh execution can
// be tested with a fake. Satisfied by *api.TwitchClient.
type sessionRefresher interface {
	RefreshPlaybackSession(streamer *models.Streamer, fetchSpade bool, expected models.ExpectedSession) api.SessionRefreshResult
}

// AvoidChecker reports whether a channel is temporarily excluded from watch
// selection (the progress watchdog's channel-switch recovery stage). Satisfied
// by *health.AvoidList; may be nil (nothing avoided).
type AvoidChecker interface {
	IsAvoided(login string) bool
}

// ReportStats is the minute-watched delivery accounting for one currently
// slotted channel. Counters accumulate while the channel keeps holding a slot
// and reset when it leaves the allocation (rotation, displacement, offline) -
// the progress watchdog only reasons about channels that are actively watched,
// so history beyond the current tenure is intentionally not kept.
type ReportStats struct {
	Successes   int       `json:"successes"`
	Failures    int       `json:"failures"`
	LastSuccess time.Time `json:"lastSuccess,omitzero"`
	LastFailure time.Time `json:"lastFailure,omitzero"`
}

// RefreshMode selects how much of a channel's watch session a staged refresh
// rebuilds.
type RefreshMode string

const (
	// RefreshStreamInfo re-fetches stream info past the 2-minute gate:
	// broadcast ID, game, advertised campaign IDs, and the beacon payload.
	RefreshStreamInfo RefreshMode = "stream_info"
	// RefreshSession additionally re-scrapes the spade URL first - the full
	// "recreate the watch session" recovery stage.
	RefreshSession RefreshMode = "session"
)

// refreshModeRank orders refresh modes so coalescing keeps the stronger one.
func refreshModeRank(m RefreshMode) int {
	if m == RefreshSession {
		return 2
	}
	return 1
}

// SessionRefreshRequest is a correlated request to rebuild a slotted channel's
// watch session. It binds the refresh to the exact broadcast/session it was
// requested for, so the broker can reject (as stale/skipped) a request that no
// longer matches the live channel before doing any I/O, and the caller (the
// progress watchdog) can match the resulting outcome to the exact recovery
// episode it belongs to instead of accepting any outcome for the login.
//
// ExpectedBroadcastID / ExpectedGeneration being zero is an EXPLICIT
// "unspecified" (the requester did not know the session identity), never a
// wildcard that matches anything: the broker only rejects on a concrete mismatch.
type SessionRefreshRequest struct {
	RequestID           string
	Login               string
	Mode                RefreshMode
	ExpectedBroadcastID string
	ExpectedGeneration  uint64
	Signature           string // privacy-safe recovery signature (see RecoverySignature)
	Requested           time.Time
}

// supersedes reports whether r should replace an already-staged request prev for
// the same login. An identical signature is deduped (the same work is already
// staged); otherwise a newer session (higher expected generation) wins, then a
// stronger mode, then a newer request timestamp.
func (r SessionRefreshRequest) supersedes(prev SessionRefreshRequest) bool {
	if r.Signature != "" && r.Signature == prev.Signature {
		return false
	}
	if r.ExpectedGeneration != prev.ExpectedGeneration {
		return r.ExpectedGeneration > prev.ExpectedGeneration
	}
	if refreshModeRank(r.Mode) != refreshModeRank(prev.Mode) {
		return refreshModeRank(r.Mode) > refreshModeRank(prev.Mode)
	}
	return r.Requested.After(prev.Requested)
}

// SessionRefreshOutcome is the published, correlation-capable result of the last
// executed (or skipped/stale) session refresh for a channel. It carries the exact
// request identity and the expected-vs-current session identity so a caller can
// prove an outcome belongs to its own recovery episode. Detail is human-readable
// and redacted - never a URL, token, or payload; Reason is a bounded code.
type SessionRefreshOutcome struct {
	RequestID                 string      `json:"requestId"`
	ObservationID             uint64      `json:"observationId"`
	Login                     string      `json:"login"`
	Mode                      RefreshMode `json:"mode"`
	ExpectedBroadcastID       string      `json:"expectedBroadcastId,omitempty"`
	CurrentBroadcastID        string      `json:"currentBroadcastId,omitempty"`
	ExpectedSessionGeneration uint64      `json:"expectedSessionGeneration,omitempty"`
	CurrentSessionGeneration  uint64      `json:"currentSessionGeneration,omitempty"`
	AppliedSessionGeneration  uint64      `json:"appliedSessionGeneration,omitempty"`
	Signature                 string      `json:"signature,omitempty"`
	Requested                 time.Time   `json:"requested"`
	Started                   time.Time   `json:"started,omitzero"`
	Completed                 time.Time   `json:"completed"`
	Success                   bool        `json:"success"`
	Stale                     bool        `json:"stale,omitempty"`
	Skipped                   bool        `json:"skipped,omitempty"`
	Reason                    string      `json:"reason"`
	Detail                    string      `json:"detail"`
}

// Bounded reason-code vocabulary for a SessionRefreshOutcome. The stale variants
// deliberately reuse the models apply-reason strings so a broker outcome carries
// the exact reason the atomic apply (or the pre-I/O guard) rejected the refresh.
const (
	RefreshReasonOK              = "ok"
	RefreshReasonNotSlotted      = "not_slotted"
	RefreshReasonNoClient        = "no_client"
	RefreshReasonBroadcastMoved  = models.SessionStaleBroadcast     // "broadcast_changed"
	RefreshReasonGenerationMoved = models.SessionStaleGeneration    // "generation_drift"
	RefreshReasonSuperseded      = models.SessionStaleSupersededObs // "superseded_observation"
	RefreshReasonSpadeFailed     = "spade_failed"
	RefreshReasonStreamInfoFail  = "stream_info_failed"
)

// SetAvoidChecker registers the temporary avoid list consulted during watch
// selection. Safe for concurrent use; takes effect on the next tick.
func (w *MinuteWatcher) SetAvoidChecker(a AvoidChecker) {
	w.mu.Lock()
	w.avoid = a
	w.mu.Unlock()
}

// SetCampaignScores publishes the campaign-policy engine's per-login scores
// (higher = higher priority) for the DROPS priority tie-break. Lock-free for
// the reader (the loop goroutine); pass nil to clear. Safe for concurrent use.
func (w *MinuteWatcher) SetCampaignScores(scores map[string]int) {
	if scores == nil {
		w.campaignScores.Store(nil)
		return
	}
	w.campaignScores.Store(&scores)
}

// SetPreferConfiguredOverDiscovery controls whether a directory-discovered
// candidate may displace a configured (tracked) streamer that already holds a
// watch slot. When true, discovery never evicts the configured list — it only
// fills otherwise-idle slots; when false (the default) the pre-existing
// rank-based arbitration applies, so a discovered active-drop channel can bump
// a configured streamer held only by points or fair rotation. Lock-free for the
// reader (the loop goroutine). Safe for concurrent use.
func (w *MinuteWatcher) SetPreferConfiguredOverDiscovery(prefer bool) {
	w.preferConfigured.Store(prefer)
}

// orderByCampaignScore returns a copy of the streamer indexes ordered by the
// published campaign-policy score (highest first, stable). With no scores
// published it returns the input order unchanged, so the pre-policy DROPS
// order is bit-identical. Runs on the loop goroutine.
func (w *MinuteWatcher) orderByCampaignScore(indexes []int) []int {
	scoresPtr := w.campaignScores.Load()
	if scoresPtr == nil {
		return indexes
	}
	scores := *scoresPtr
	ordered := make([]int, len(indexes))
	copy(ordered, indexes)
	sort.SliceStable(ordered, func(i, j int) bool {
		return scores[w.streamers[ordered[i]].Username] > scores[w.streamers[ordered[j]].Username]
	})
	return ordered
}

// RequestSessionRefresh stages a correlated watch-session refresh for a channel.
// The broker loop executes it at the start of its next tick - on the loop
// goroutine, only if the channel still holds a watch slot AND still matches the
// request's expected broadcast - and publishes the correlated outcome for
// LastSessionRefresh. Requests coalesce per login (see supersedes): an identical
// signature is deduped, a newer session or stronger mode wins. Safe for
// concurrent use from any goroutine.
func (w *MinuteWatcher) RequestSessionRefresh(req SessionRefreshRequest) {
	if req.Login == "" {
		return
	}
	if req.Mode != RefreshSession {
		req.Mode = RefreshStreamInfo
	}
	if req.Requested.IsZero() {
		req.Requested = time.Now()
	}
	w.mu.Lock()
	if w.pendingRefresh == nil {
		w.pendingRefresh = make(map[string]SessionRefreshRequest)
	}
	if prev, ok := w.pendingRefresh[req.Login]; !ok || req.supersedes(prev) {
		w.pendingRefresh[req.Login] = req
	}
	w.mu.Unlock()
}

// LastSessionRefresh returns the outcome of the most recent executed (or
// skipped) refresh for the channel. Lock-free.
func (w *MinuteWatcher) LastSessionRefresh(login string) (SessionRefreshOutcome, bool) {
	m := w.refreshOutcomes.Load()
	if m == nil {
		return SessionRefreshOutcome{}, false
	}
	o, ok := (*m)[login]
	return o, ok
}

// ReportStats returns the minute-watched delivery accounting for a channel
// currently holding a watch slot (false once it leaves the allocation).
// Lock-free.
func (w *MinuteWatcher) ReportStats(login string) (ReportStats, bool) {
	m := w.reportStatsSnap.Load()
	if m == nil {
		return ReportStats{}, false
	}
	s, ok := (*m)[login]
	return s, ok
}

// drainPendingRefreshes takes the staged refresh requests. Runs on the loop
// goroutine once per tick.
func (w *MinuteWatcher) drainPendingRefreshes() map[string]SessionRefreshRequest {
	w.mu.Lock()
	defer w.mu.Unlock()
	pending := w.pendingRefresh
	w.pendingRefresh = nil
	return pending
}

// nextRefreshObs returns a unique, monotonic observation id for one refresh
// execution, so every outcome the broker publishes is distinguishable even for
// the same login/broadcast across ticks. Concurrency-safe.
func (w *MinuteWatcher) nextRefreshObs() uint64 {
	return w.refreshObsSeq.Add(1)
}

// executeSessionRefreshes runs the staged refreshes against the channels that
// actually hold a slot this tick, before the sends - so a successful refresh
// takes effect for this very tick. Requests for channels that lost their slot
// are completed as skipped (idempotent: the watchdog re-stages if still
// needed).
//
// Tick-delay budget (worst case, nominal one attempt per HTTP round, each
// bounded by the api client's 30s timeout): one RefreshSession is up to FOUR
// network rounds - GetSpadeURL is two plain HTTP requests (channel page +
// settings.js) and UpdateStream is up to two GQL calls (stream info +
// campaign IDs) - so up to ~120s per channel; RefreshStreamInfo is up to two
// rounds, ~60s. Executed sequentially, both slots pending RefreshSession
// would stack to ~240s, which cannot fit the continuity window: a slotted
// channel's inter-send gap is the jittered inter-tick sleep (<=1.2*interval)
// plus this delay, against maxContinuousGap = 2*interval - so the refresh
// budget is ~0.8*interval (48s at the default 60s interval, at most 96s at
// the 120s clamp). Refreshes for distinct channels therefore run in
// PARALLEL (one goroutine per slotted request, joined before the sends):
// the bound becomes the per-channel maximum, not the sum. Even so, a
// pathological worst case (every round riding its full 30s timeout, or GQL
// retries stacking - the same pre-existing property as this loop's inline
// CheckStreamerOnline calls) can exceed the budget at any allowed interval;
// the consequence is bounded and self-consistent, not unsafe:
// Stream.UpdateMinuteWatched treats the oversized gap as a continuity break
// and restarts the watch-streak accounting for that channel - mirroring the
// server-side session break Twitch itself applies after such a reporting gap
// - while drop-progress accrual (Twitch-side) is unaffected. In practice
// rounds complete in well under a second and refreshes are rare: they exist
// only as watchdog recovery stages, at most one new request per ~1-minute
// watchdog pass, cooldown-bounded.
func (w *MinuteWatcher) executeSessionRefreshes(slots []slotOccupant) {
	pending := w.drainPendingRefreshes()
	if len(pending) == 0 {
		return
	}

	slotted := make(map[string]*models.Streamer, len(slots))
	for _, sl := range slots {
		slotted[sl.streamer.Username] = sl.streamer
	}

	now := time.Now()

	// Stable, deterministic order (sorted by login). The outcomes slice is
	// allocated at its FINAL length up front and NEVER appended to afterwards, so
	// every refresh worker writes only its own pre-allocated element (via a stable
	// pointer) and no goroutine ever reads a slice header the parent is mutating -
	// the fix for the former append(outcomes)+captured-slice data race.
	logins := make([]string, 0, len(pending))
	for login := range pending {
		logins = append(logins, login)
	}
	sort.Strings(logins)

	outcomes := make([]SessionRefreshOutcome, len(logins))
	var wg sync.WaitGroup

	for i, login := range logins {
		req := pending[login]
		outcomes[i] = SessionRefreshOutcome{
			RequestID:                 req.RequestID,
			ObservationID:             w.nextRefreshObs(),
			Login:                     login,
			Mode:                      req.Mode,
			ExpectedBroadcastID:       req.ExpectedBroadcastID,
			ExpectedSessionGeneration: req.ExpectedGeneration,
			Signature:                 req.Signature,
			Requested:                 req.Requested,
		}
		out := &outcomes[i]

		streamer, held := slotted[login]
		switch {
		case !held:
			// Lost the slot: NOT a completed transport recovery. The requester must
			// re-confirm active farming before this stage counts as done.
			out.Skipped = true
			out.Reason = RefreshReasonNotSlotted
			out.Detail = "skipped: channel no longer holds a watch slot"
			out.Completed = time.Now()
		case w.refresher == nil:
			out.Skipped = true
			out.Reason = RefreshReasonNoClient
			out.Detail = "skipped: no refresh-capable client (test harness)"
			out.CurrentSessionGeneration = streamer.Stream.SessionGeneration()
			out.Completed = time.Now()
		default:
			// Pre-I/O correlation guard: the request must still describe the live
			// session. The current broadcast AND generation must EXACTLY match the
			// concrete expected values (a current empty broadcast does NOT match a
			// concrete expected one). A mismatch means the session moved on - reject
			// as stale WITHOUT any I/O or session mutation. Zero expected fields are
			// "unspecified", not a wildcard, so they never trigger a mismatch.
			curBroadcast := streamer.Stream.GetBroadcastID()
			curGen := streamer.Stream.SessionGeneration()
			out.CurrentBroadcastID = curBroadcast
			out.CurrentSessionGeneration = curGen
			if reason, stale := requestStale(req, curBroadcast, curGen); stale {
				out.Stale = true
				out.Reason = reason
				out.Detail = "stale: the live session no longer matches the staged request; no refresh run"
				out.Completed = time.Now()
				continue
			}

			out.Started = now
			wg.Add(1)
			go func(out *SessionRefreshOutcome, streamer *models.Streamer, req SessionRefreshRequest) {
				defer wg.Done()
				w.runSessionRefresh(out, streamer, req)
			}(out, streamer, req)
		}
	}
	wg.Wait()

	for i := range outcomes {
		slog.Info("Watch session refresh",
			"channel", outcomes[i].Login, "mode", string(outcomes[i].Mode),
			"requestId", outcomes[i].RequestID, "success", outcomes[i].Success,
			"stale", outcomes[i].Stale, "skipped", outcomes[i].Skipped,
			"reason", outcomes[i].Reason, "detail", outcomes[i].Detail)
	}
	w.publishRefreshOutcomes(outcomes)
}

// requestStale reports whether a staged refresh request no longer matches the live
// session and must be rejected before any I/O. A concrete expected broadcast or
// generation must match EXACTLY; a current empty broadcast fails a concrete
// expected one. Zero expected fields are unspecified and never fail.
func requestStale(req SessionRefreshRequest, curBroadcast string, curGen uint64) (string, bool) {
	if req.ExpectedBroadcastID != "" && curBroadcast != req.ExpectedBroadcastID {
		return RefreshReasonBroadcastMoved, true
	}
	if req.ExpectedGeneration != 0 && curGen != req.ExpectedGeneration {
		return RefreshReasonGenerationMoved, true
	}
	return "", false
}

// runSessionRefresh rebuilds a slotted channel's watch session in one atomic
// publication and records the correlated outcome into its own pre-allocated slot.
// RefreshSession re-scrapes the spade URL first; both modes force stream info past
// the 2-minute gate, then publish broadcast + spade URL + payload together. The
// atomic apply re-checks the expected broadcast/generation, so a session that
// changed during the I/O yields a stale outcome without a partial overwrite.
// Detail strings never carry a URL or token by construction.
func (w *MinuteWatcher) runSessionRefresh(out *SessionRefreshOutcome, streamer *models.Streamer, req SessionRefreshRequest) {
	// RefreshPlaybackSession always fetches fresh stream info (a recovery refresh
	// is never gated by the 2-minute cadence), so no ForceUpdateRequired is needed.
	res := w.refresher.RefreshPlaybackSession(streamer, req.Mode == RefreshSession,
		models.ExpectedSession{BroadcastID: req.ExpectedBroadcastID, Generation: req.ExpectedGeneration})

	out.AppliedSessionGeneration = res.AppliedGeneration
	out.CurrentSessionGeneration = res.CurrentGeneration
	out.CurrentBroadcastID = res.CurrentBroadcastID
	out.Completed = time.Now()

	switch {
	case res.Stale:
		// The session was superseded (newer observation, broadcast, or generation)
		// during the I/O: NOT applied, NOT a success.
		out.Stale = true
		out.Reason = staleOutcomeReason(res.Reason)
		out.Detail = "stale: the session changed during the refresh; not published over the newer session"
	case res.Applied:
		out.Success = true
		out.Reason = RefreshReasonOK
		if req.Mode == RefreshSession {
			out.Detail = "watch session recreated: spade URL, stream info, and beacon payload refreshed"
		} else {
			out.Detail = "stream info re-fetched: broadcast, game, campaign IDs, and beacon payload refreshed"
		}
	case res.Stage == "spade":
		out.Reason = RefreshReasonSpadeFailed
		out.Detail = "spade URL re-fetch failed"
	default:
		out.Reason = RefreshReasonStreamInfoFail
		out.Detail = "stream info refresh failed"
	}
}

// staleOutcomeReason maps the atomic apply's bounded stale reason onto the broker
// outcome vocabulary (they share strings; an unexpected value falls back to the
// superseded code).
func staleOutcomeReason(reason string) string {
	switch reason {
	case models.SessionStaleBroadcast:
		return RefreshReasonBroadcastMoved
	case models.SessionStaleGeneration:
		return RefreshReasonGenerationMoved
	default:
		return RefreshReasonSuperseded
	}
}

// publishRefreshOutcomes merges the batch into the published last-outcome-per-
// login map (copy-on-write). Runs on the loop goroutine.
func (w *MinuteWatcher) publishRefreshOutcomes(outcomes []SessionRefreshOutcome) {
	prev := w.refreshOutcomes.Load()
	next := make(map[string]SessionRefreshOutcome, len(outcomes))
	if prev != nil {
		for k, v := range *prev {
			next[k] = v
		}
	}
	for _, o := range outcomes {
		next[o.Login] = o
	}
	w.refreshOutcomes.Store(&next)
}

// noteReportOutcome updates the loop-owned per-channel delivery counters after
// one minute-watched send attempt. Runs on the loop goroutine.
func (w *MinuteWatcher) noteReportOutcome(login string, ok bool, now time.Time) {
	if w.reportStats == nil {
		w.reportStats = make(map[string]ReportStats)
	}
	s := w.reportStats[login]
	if ok {
		s.Successes++
		s.LastSuccess = now
	} else {
		s.Failures++
		s.LastFailure = now
	}
	w.reportStats[login] = s
}

// publishReportStats prunes the delivery counters to the channels still
// holding a slot and publishes an immutable copy. Runs on the loop goroutine
// at the end of each tick.
func (w *MinuteWatcher) publishReportStats(slots []slotOccupant) {
	pruned := make(map[string]ReportStats, len(slots))
	for _, sl := range slots {
		if s, ok := w.reportStats[sl.streamer.Username]; ok {
			pruned[sl.streamer.Username] = s
		}
	}
	w.reportStats = pruned

	snap := make(map[string]ReportStats, len(pruned))
	for k, v := range pruned {
		snap[k] = v
	}
	w.reportStatsSnap.Store(&snap)
}
