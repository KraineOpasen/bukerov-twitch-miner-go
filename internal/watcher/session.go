package watcher

import (
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// This file is the broker's Stage 3 surface for the drop-progress watchdog:
// per-channel minute-watched delivery accounting, the staged watch-session
// refresh, and the temporary avoid-list hook. All three follow the same
// ownership rule as the rest of the broker (see MinuteWatcher): external
// goroutines only stage requests or read atomically-published snapshots;
// every mutation of live streamer state happens on the loop goroutine, which
// stays the single writer of the watch session for slotted channels.

// sessionRefresher is the slice of the Twitch client the broker needs to
// rebuild a slotted channel's watch session on demand. Narrowed to an
// interface so refresh execution can be tested with a fake. Satisfied by
// *api.TwitchClient.
type sessionRefresher interface {
	GetSpadeURL(streamer *models.Streamer) error
	UpdateStream(streamer *models.Streamer) error
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

// SessionRefreshOutcome is the published result of the last executed (or
// skipped) session refresh for a channel. Detail is human-readable and
// redacted - never a URL or token.
type SessionRefreshOutcome struct {
	Login     string      `json:"login"`
	Mode      RefreshMode `json:"mode"`
	Requested time.Time   `json:"requested"`
	Completed time.Time   `json:"completed"`
	OK        bool        `json:"ok"`
	Detail    string      `json:"detail"`
}

// pendingRefresh is one staged refresh request (coalesced per login).
type pendingRefresh struct {
	mode      RefreshMode
	requested time.Time
}

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

// RequestSessionRefresh stages a watch-session refresh for a channel. The
// broker loop executes it at the start of its next tick - on the loop
// goroutine, only if the channel still holds a watch slot - and publishes the
// outcome for LastSessionRefresh. Requests coalesce per login, keeping the
// strongest mode, so callers cannot queue duplicate work. Safe for concurrent
// use from any goroutine.
func (w *MinuteWatcher) RequestSessionRefresh(login string, mode RefreshMode) {
	if login == "" {
		return
	}
	if mode != RefreshSession {
		mode = RefreshStreamInfo
	}
	w.mu.Lock()
	if w.pendingRefresh == nil {
		w.pendingRefresh = make(map[string]pendingRefresh)
	}
	if prev, ok := w.pendingRefresh[login]; !ok || refreshModeRank(mode) > refreshModeRank(prev.mode) {
		w.pendingRefresh[login] = pendingRefresh{mode: mode, requested: time.Now()}
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
func (w *MinuteWatcher) drainPendingRefreshes() map[string]pendingRefresh {
	w.mu.Lock()
	defer w.mu.Unlock()
	pending := w.pendingRefresh
	w.pendingRefresh = nil
	return pending
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

	outcomes := make([]SessionRefreshOutcome, 0, len(pending))
	var wg sync.WaitGroup
	for login, req := range pending {
		outcome := SessionRefreshOutcome{
			Login:     login,
			Mode:      req.mode,
			Requested: req.requested,
		}

		streamer, held := slotted[login]
		switch {
		case !held:
			outcome.Detail = "skipped: channel no longer holds a watch slot"
			outcome.Completed = time.Now()
			outcomes = append(outcomes, outcome)
		case w.refresher == nil:
			outcome.Detail = "skipped: no refresh-capable client (test harness)"
			outcome.Completed = time.Now()
			outcomes = append(outcomes, outcome)
		default:
			// Distinct logins mean distinct streamer objects (one channel never
			// holds two slots), so each worker mutates only its own streamer and
			// writes only its own pre-allocated outcome slot - no shared state
			// beyond the WaitGroup join below.
			outcomes = append(outcomes, outcome)
			idx := len(outcomes) - 1
			wg.Add(1)
			go func(idx int, streamer *models.Streamer, mode RefreshMode) {
				defer wg.Done()
				outcomes[idx].OK, outcomes[idx].Detail = w.refreshSession(streamer, mode)
				outcomes[idx].Completed = time.Now()
			}(idx, streamer, req.mode)
		}
	}
	wg.Wait()

	for i := range outcomes {
		slog.Info("Watch session refresh",
			"channel", outcomes[i].Login, "mode", string(outcomes[i].Mode),
			"ok", outcomes[i].OK, "detail", outcomes[i].Detail)
	}
	w.publishRefreshOutcomes(outcomes)
}

// refreshSession rebuilds a slotted channel's watch session on the loop
// goroutine: RefreshSession re-scrapes the spade URL first, then both modes
// force stream info (broadcast ID, game, campaign IDs, beacon payload) past
// the 2-minute gate. Detail strings are redacted by construction.
func (w *MinuteWatcher) refreshSession(streamer *models.Streamer, mode RefreshMode) (bool, string) {
	if mode == RefreshSession {
		if err := w.refresher.GetSpadeURL(streamer); err != nil {
			return false, "spade URL re-fetch failed"
		}
	}

	streamer.Stream.ForceUpdateRequired()
	if err := w.refresher.UpdateStream(streamer); err != nil {
		if mode == RefreshSession {
			return false, "spade URL re-fetched, but stream info refresh failed"
		}
		return false, "stream info refresh failed"
	}

	if mode == RefreshSession {
		return true, "watch session recreated: spade URL, stream info, and beacon payload refreshed"
	}
	return true, "stream info re-fetched: broadcast, game, campaign IDs, and beacon payload refreshed"
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
