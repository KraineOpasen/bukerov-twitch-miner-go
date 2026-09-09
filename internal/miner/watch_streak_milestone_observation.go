package miner

// Watch Streak milestone OBSERVABILITY.
//
// Two diagnostic record streams live here, and nothing else:
//
//  1. watch_streak_milestone_observation — one record per read-only RewardList
//     sample, emitted by the optional observation stage composed with the
//     EXISTING bonus poll cycle.
//  2. watch_streak_grant_correlation — one record per newly accepted
//     WATCH_STREAK PubSub grant, stating what the exact ledger ACTUALLY did
//     with it.
//
// Both are pure observability. Nothing in this file grants a reward, changes
// Stream state, changes Channel Points capability, changes pursuit or recovery,
// changes selection/rotation/broker state, or blocks a claim or redemption
// path. There is no scheduler, no goroutine, no timer, no cache, no ledger, no
// migration, no public API and no dashboard/support-bundle surface: retained
// logs are the only persistence this feature has.
//
// What these records deliberately do NOT do:
//
//   - They never synthesize a Watch Streak reward and never change one. The
//     documented reward ladder IS known (see officialWatchStreakLadder), but
//     knowing it changes nothing about what this code does to an amount: every
//     valid integer still passes through the accounting path untouched, and an
//     amount that does not match the ladder is reported as off-ladder, never
//     filtered, rewritten, promoted or rejected.
//   - They never infer a streak COUNT. Which RewardList field, if any, carries
//     the authoritative streak count is still UNKNOWN, so a received +450 is
//     recorded as CONSISTENT WITH the documented 5-or-more tier and nothing
//     more — never as an established streak count, and never as a historical
//     binding.
//   - They never bind a grant to a broadcast. The WATCH_STREAK PubSub frame
//     carries no provable BroadcastID, so the currently observed
//     Stream.BroadcastID is logged as LOCAL CONTEXT ONLY and never as
//     ProvenBroadcastID.
//   - They never claim a milestone snapshot was "expected at grant" or
//     "changed because of grant". A snapshot before an event is a PREVIOUS
//     OBSERVATION and a snapshot after it is a FOLLOWING OBSERVATION; the link
//     between them and the grant is explicitly UNKNOWN. Delayed PubSub,
//     overlapping requests, reverse completion, clock disagreement,
//     post-grant-only sampling, restart, streamer remove/re-add and broadcast
//     change all leave that relationship UNKNOWN, and this file has no path
//     that can upgrade it.

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/pubsub"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/twitch"
)

// Record kinds. Stable strings so a retained log can be filtered.
const (
	milestoneObservationRecord = "watch_streak_milestone_observation"
	milestoneCorrelationRecord = "watch_streak_grant_correlation"
	// milestoneBudgetRecord marks the one record a cycle emits when its budget
	// stopped it before the roster was finished.
	milestoneBudgetRecord = "watch_streak_milestone_budget"
)

// milestoneObservationCycleBudget bounds ONE observation stage, end to end.
//
// The stage runs as a plain call on the bonusPollLoop goroutine, so without a
// bound an optional diagnostic could defer the next BUSINESS bonus pass: the
// loop cannot service its next tick until the stage returns, and a transiently
// failing RewardList inherits the shared read transport's full retry schedule
// per target. That is a diagnostic delaying the chest-claim fallback, which is
// exactly backwards.
//
// The value is SHORTER than bonusPollInterval (60s) so the stage alone can
// never push the next business pass past its tick.
//
// It is also longer than one shared-transport HTTP timeout (30s), but NOT for
// the reason an earlier version of this comment gave. That version claimed the
// margin let a genuinely stalled Twitch surface as TRANSPORT_TIMEOUT rather
// than being masked by our own deadline. Measured, that is false: a
// Client.Timeout error carries statusCode 0, which doGQLRequestWithRetry
// classifies as TRANSIENT, so the first 30s timeout is never returned to this
// caller - it is retried, up to gqlMaxRetries+1 attempts plus backoff. The
// budget therefore expires during attempt 2, the error this stage sees is its
// own context's, and the record reads CANCELLED/DEADLINE_EXCEEDED. Reproduced
// once by hand against a transport that never answers, with the shipped 30s
// client timeout and this 40s budget: 40.009s, DEADLINE_EXCEEDED - a one-off
// figure, not pinned by any test in the tree. The retry half of that
// mechanism is pinned by
// TestAClientTimeoutIsRetriedRatherThanReturned in internal/twitch, the only
// package that can shorten the client's hard-coded 30s timeout without adding
// a production seam for a test's benefit.
//
// So MilestoneFailureTransportTimeout is not reachable through this stage for
// the pure stall the old comment described, and under the cycle allowance
// (three dispatches, fewer than the ladder's gqlMaxRetries+1) the ladder never
// reaches its final attempt through this stage either: a pre-response stall
// on a target that starts with at least two permits ends as the stage's own
// DEADLINE_EXCEEDED (the 30s client timeout plus one backoff put the second
// dispatch past the 40s budget before a third permit could be charged); a
// stall on a target that starts with the cycle's LAST permit ends as
// ALLOWANCE_EXHAUSTED after its first 30s timeout, with budget to spare and
// the owner alive (the retry loop finds no permit for attempt 2 and stops;
// pinned by TestAStallOnTheLastPermitEndsAsAllowanceExhausted in
// internal/twitch), as does a FAST-failing transient - an immediate 5xx or a
// refused connection, not a stall - whatever the permit count. What
// DOES reach TRANSPORT_TIMEOUT through this stage is a body stall: a 2xx
// status received and then a body that never completes, which Client.Timeout
// ends as a non-transient read error on the FIRST dispatch (pinned by
// TestABodyStallAfterA2xxReadsTransportTimeoutOnTheFirstDispatch in
// internal/twitch). It also stays reachable for a caller passing a nil
// allowance with a longer-lived context. The class is kept because it names a
// real, distinguishable outcome at the client boundary.
//
// Making the pure-stall route reachable here would need a budget exceeding
// the whole retry schedule, which is far past bonusPollInterval and defeats
// the bound. That is a cadence decision, not a comment fix, so the comment is
// what changed.
//
// "Alone" is deliberate. This budget starts when the stage does, not at the
// tick, so on its own it would let a long business pass and a full stage ADD:
// a 30s pollBonuses followed by a 40s stage returns at 70s and the coalesced
// tick fires 10s late. The stage therefore does not run on this budget alone:
// its deadline is the EARLIEST of this budget, the next business tick
// (tick + bonusPollInterval, from the timestamp the serviced ticker event
// carries) and the owner's deadline — see milestoneStageDeadline. A cycle
// whose business pass consumed the whole period admits no diagnostic request
// at all rather than being granted a fresh budget. That is an owner cadence
// decision, recorded in SPECIFICATIONS.md: on exactly the cycles where a slow
// Twitch makes the evidence most interesting, business polling wins. Note the
// business pass itself is still unbounded on this loop and can overrun a tick
// with no help from this stage; the rule only guarantees the stage never
// compounds it.
//
// It is a var, not a const, so tests can shorten it; nothing at runtime writes
// it, and it is deliberately not a settings surface.
var milestoneObservationCycleBudget = 40 * time.Second

// unknownLink is the single UNKNOWN vocabulary for any fact this feature cannot
// state - the relationships it cannot prove and the amounts and timestamps it
// cannot read exactly. It is printed explicitly rather than omitted, so a
// reader sees that the question was asked and answered UNKNOWN — not that it
// was never asked.
const unknownLink = "UNKNOWN"

// noProvenBroadcast is the correlation record's positive answer that an
// admitted grant fact carries no proven broadcast; see provenBroadcastId.
const noProvenBroadcast = "NONE"

// milestoneLogStringCap bounds every free-form observed string reaching a log
// record. Twitch's milestone fields are short identifiers and timestamps; a
// response that disagrees gets truncated (and marked truncated) instead of
// writing an unbounded blob into the operator's log.
const milestoneLogStringCap = 128

// milestoneLogIDSample bounds how many observed broadcast identifiers a single
// record prints. Counts are always exact; the identifier list is a sample.
const milestoneLogIDSample = 8

// officialWatchStreakLadder is Twitch's DOCUMENTED Watch Streak reward ladder,
// in ascending streak-count order.
//
// Source: Twitch, "Viewer Channel Point Guide"
// (https://help.twitch.tv/s/article/viewer-channel-point-guide), supplied as
// owner evidence and recorded 2026-09-06. Corroborated against the donor
// mpforce1/Twitch-Channel-Points-Miner at ref
// f1dda17ad61562ca2e93d975ee0a24e8b2f7ea0c (README blob
// d5b60c22c5e6387cbe69c3e7e951b32d946672f6), which lists the same four reward
// AMOUNTS; no separate audit record of that donor exists in this repository,
// so the corroboration is exactly that blob at that ref and nothing more.
// Stated exactly, because the earlier wording ("states the same progression",
// "independently corroborated") claimed more than the donor supports: the amounts are corroborated, the streak-count-to-amount MAPPING
// and the flat "5 or more" rung rest on the Twitch guide alone. That is why
// provenStreakCount reads UNKNOWN on every record - a second source for the
// amounts is not a second source for what they mean. The same guide documents that a qualifying stream must run at
// least 10 minutes and that at least 30 minutes must have elapsed since the
// previous stream ended — recorded here as Twitch's stated rule only; this
// miner's pursuit semantics are unchanged and are NOT derived from it.
//
// The guide states earn rates are SUBJECT TO CHANGE. This is therefore a
// current-official fact carrying a retrieval date, not a permanent invariant,
// and it is used for ONE thing: labelling a diagnostic record. Nothing reads it
// to decide, filter, expect, or synthesize a reward.
//
// The ladder describes BASE amounts. A channel-points multiplier scales the
// credited total above the base, which is why the classifier below prefers the
// frame's own baseline_points and says which basis it used — see
// officialLadderConsistency.
// A slice, not a map, and each rung carries its own tier. A map plus a switch
// with a default branch would silently label any future rung as the flat top
// one — add a documented "6 -> 500" and +500 would be reported as consistent
// with "5 or more", which is exactly the kind of quiet mislabel this feature
// exists to avoid. Ordered iteration also keeps the lookup deterministic
// regardless of what values the ladder ever holds.
var officialWatchStreakLadder = []watchStreakLadderRung{
	{StreakCount: 2, RewardPoints: 300, Tier: ladderStreakCount2},
	{StreakCount: 3, RewardPoints: 350, Tier: ladderStreakCount3},
	{StreakCount: 4, RewardPoints: 400, Tier: ladderStreakCount4},
	// Flat: 450 is the documented reward for a 5th streak and every one after
	// it, so the amount cannot distinguish a 5th from a 50th.
	{StreakCount: 5, RewardPoints: 450, Tier: ladderStreakCount5Plus, FlatFromHere: true},
}

// watchStreakLadderRung is one documented rung. FlatFromHere marks a rung whose
// reward also covers every higher streak count, which is why its tier names a
// set rather than a number.
type watchStreakLadderRung struct {
	StreakCount  int
	RewardPoints int
	Tier         watchStreakLadderTier
	FlatFromHere bool
}

// watchStreakLadderTier is how one observed amount relates to the documented
// ladder. It is a CONSISTENCY label, never a measurement: the ladder maps a
// streak count to an amount, and reading it backwards is many-to-one at the top
// rung, so no tier here establishes an actual streak count.
type watchStreakLadderTier string

const (
	ladderStreakCount2 watchStreakLadderTier = "CONSISTENT_WITH_STREAK_COUNT_2"
	ladderStreakCount3 watchStreakLadderTier = "CONSISTENT_WITH_STREAK_COUNT_3"
	ladderStreakCount4 watchStreakLadderTier = "CONSISTENT_WITH_STREAK_COUNT_4"
	// ladderStreakCount5Plus is a SET, not a count: +450 is the documented
	// reward for a 5th streak and for every streak after it.
	ladderStreakCount5Plus watchStreakLadderTier = "CONSISTENT_WITH_STREAK_COUNT_5_OR_MORE"
	// ladderOffBase: a real, valid, fully accounted amount that does not equal
	// a documented BASE rung. A points multiplier produces exactly this, as
	// would a Twitch rate change. It is a label on the amount, never a
	// rejection of it.
	ladderOffBase watchStreakLadderTier = "OFF_DOCUMENTED_BASE_LADDER"
	// ladderAmountUnknown: the frame carried no exactly representable amount,
	// so there is nothing to compare.
	ladderAmountUnknown watchStreakLadderTier = "AMOUNT_UNKNOWN"
)

// officialLadderConsistency labels an observed amount against the documented
// ladder and reports which wire field it compared.
//
// basePoints is the frame's own baseline_points when that field was present and
// exactly representable, read as the base amount the ladder describes, and
// totalPoints is the credited amount #303 accounts. The field's PRESENCE is
// evidenced by this repository's points-earned fixtures (total_points,
// baseline_points, reason_code and multipliers, with baseline == total when
// multipliers is empty); its PRE-MULTIPLIER meaning is an ASSUMPTION drawn from
// the field's name and the multipliers array beside it, pending a captured
// multiplied frame, and SPECIFICATIONS.md states it as such. Preferring
// the baseline is what keeps a subscriber's multiplied +675 from being reported
// as "off-ladder" when it is a perfectly ordinary 5-or-more streak; when no
// baseline is available the comparison falls back to the total and SAYS SO, so
// nobody reads a multiplied total as evidence of a rate change.
//
// It makes no decision. Its only consumer is a log attribute.
func officialLadderConsistency(totalPoints int, totalExact bool, basePoints int, baseExact bool) (watchStreakLadderTier, string) {
	amount, basis := totalPoints, "TOTAL_POINTS"
	exact := totalExact
	if baseExact {
		amount, basis, exact = basePoints, "BASELINE_POINTS", true
	}
	if !exact {
		return ladderAmountUnknown, basis
	}
	for _, rung := range officialWatchStreakLadder {
		if rung.RewardPoints == amount {
			return rung.Tier, basis
		}
	}
	return ladderOffBase, basis
}

// pointLedgerOutcome is what the EXACT points ledger actually did with one
// accepted points-earned frame.
//
// It exists so a correlation record can never report COMMITTED merely because
// the domain admitted the grant. Domain acceptance and ledger commitment are
// two different facts with two different failure modes, and this type keeps
// them apart.
type pointLedgerOutcome string

const (
	// ledgerCommitted: the exact ledger newly recorded the event.
	ledgerCommitted pointLedgerOutcome = "LEDGER_COMMITTED"
	// ledgerDuplicate: the exact ledger already held this event identity and
	// wrote nothing. No second accounting effect.
	ledgerDuplicate pointLedgerOutcome = "LEDGER_DUPLICATE"
	// ledgerFailed: the exact ledger write was refused or errored. Never
	// reportable as COMMITTED.
	ledgerFailed pointLedgerOutcome = "LEDGER_FAILED"
	// ledgerTimelineOnly: the frame was not admissible to the exact ledger (no
	// event identity, an inexact amount, or no RFC 3339 wire timestamp) and was
	// recorded on the balance timeline only.
	ledgerTimelineOnly pointLedgerOutcome = "TIMELINE_ONLY"
	// ledgerAnalyticsUnavailable: there is no analytics service in this
	// generation, so nothing was offered to any ledger at all. Explicit, not
	// silently folded into FAILED.
	ledgerAnalyticsUnavailable pointLedgerOutcome = "ANALYTICS_UNAVAILABLE"
)

// milestoneCycleDispatchAllowance is how many explicit, authenticated
// diagnostic HTTP attempts ONE observation stage may make in total.
//
// It is shared across every target, every retry and every client-ID fallback
// of that cycle: three permits, not three per target. It is charged once,
// immediately before each dispatch, never refunded on error or timeout and
// never refilled within the cycle, so a roster of 100 fast-failing targets and
// a roster of one slow one both cost at most three attempts per cycle.
//
// This is an APPLICATION attempt cap, chosen so a diagnostic can never lean
// on the shared read transport's retry schedule. It is NOT a Twitch quota and
// says nothing about what the wire is willing to accept.
const milestoneCycleDispatchAllowance = 3

// milestoneCycle is what the bonus poll loop hands one observation stage.
//
//   - Tick is the timestamp carried by the serviced ticker event. A
//     time.Ticker stamps each event with its SCHEDULED fire time, so a tick
//     read late (coalesced behind a long business pass) still reports when it
//     was due. For such a tick, Tick+Period is a LOWER bound on the real next
//     tick and already in the past, so the cycle serviced on it admits nothing
//     even if some real slack remains before that next tick fires: the
//     overdue rule, never a fresh budget. A zero Tick is maximally overdue and
//     admits nothing: the loop is the only production caller and always
//     passes the serviced tick.
//   - Period is the loop's ticker period, so the stage can compute when the
//     NEXT business tick is due without owning a ticker of its own.
//   - Cursor is the loop-owned roster position the previous cycle returned.
//     It is a plain index, normalized against the current roster snapshot on
//     every cycle, so removals and resizes can never dereference a stale
//     streamer.
type milestoneCycle struct {
	Tick   time.Time
	Period time.Duration
	Cursor int
}

// milestoneDeadlineSource names which bound produced a stage's deadline. Kept
// on the budget record so a short cycle can be read as "the business pass ran
// long" (NEXT_TICK) rather than "Twitch was slow" (STAGE_BUDGET). OWNER is
// reported when the owner's deadline was the nearest bound and the record is
// written while owner.Err() is still nil - normally a COUNT cutoff; a TIME
// cutoff can carry it in the instant between the derived deadline firing and
// the owner's own timer firing, because context.WithDeadline on an equal
// deadline creates a second timer. A cycle observed to be ended by the owner's
// own deadline or cancellation emits no record at all, and the production
// owner (the signal context) carries no deadline.
type milestoneDeadlineSource string

const (
	milestoneDeadlineStageBudget milestoneDeadlineSource = "STAGE_BUDGET"
	milestoneDeadlineNextTick    milestoneDeadlineSource = "NEXT_TICK"
	milestoneDeadlineOwner       milestoneDeadlineSource = "OWNER"
)

// milestoneCutoff names WHY a stage stopped before finishing the roster: its
// deadline (TIME) or its dispatch allowance (COUNT). Both leave targets
// unexamined; they are different facts with different remedies.
type milestoneCutoff string

const (
	milestoneCutoffTime  milestoneCutoff = "TIME"
	milestoneCutoffCount milestoneCutoff = "COUNT"
)

// milestoneStageDeadline is the D1 deadline rule, as a pure function so the
// arithmetic can be pinned with literals rather than by sleeping.
//
// The stage may run until the EARLIEST of:
//
//   - stageStart + milestoneObservationCycleBudget (STAGE_BUDGET): the stage
//     alone can never hold the loop for longer than its own budget;
//   - tick + period (NEXT_TICK): the stage yields whatever the business pass
//     left of the cycle, so business work that already ran long is never
//     compounded by a diagnostic. A cycle whose business pass consumed the
//     whole period, or an overdue tick, has no slack and admits nothing — it
//     is NOT granted a fresh budget;
//   - the owner's own deadline (OWNER), when it has one.
//
// On a tie the earlier-listed source is reported. This is a scheduling bound,
// not a real-time guarantee: an in-flight request is released by the deadline
// through its context, not pre-empted.
func milestoneStageDeadline(owner context.Context, tick time.Time, period time.Duration, stageStart time.Time) (time.Time, milestoneDeadlineSource) {
	deadline, source := stageStart.Add(milestoneObservationCycleBudget), milestoneDeadlineStageBudget
	if nextTick := tick.Add(period); nextTick.Before(deadline) {
		deadline, source = nextTick, milestoneDeadlineNextTick
	}
	if ownerDeadline, ok := owner.Deadline(); ok && ownerDeadline.Before(deadline) {
		deadline, source = ownerDeadline, milestoneDeadlineOwner
	}
	return deadline, source
}

// milestoneEligible is the whole eligibility rule: online, with a channel
// identity to scope the request. Deliberately narrow and side-effect free. It
// does not consult the watch-streak setting, because that setting governs
// PURSUIT, and gating observation on it would suppress evidence for exactly
// the grants this exists to observe.
func milestoneEligible(s *models.Streamer) bool {
	return s.GetIsOnline() && s.ChannelID != ""
}

// observeWatchStreakMilestones is the optional observation stage of one bonus
// cycle. It returns the roster cursor the NEXT cycle should start from.
//
// Lifecycle contract, all of it load-bearing:
//
//   - It owns nothing. It is a plain function call on the EXISTING
//     bonusPollLoop goroutine, invoked only AFTER that cycle's business pass
//     (pollBonuses: bonus claiming and auto-redeem) has fully returned. It is
//     never interleaved with a claim or redemption, and it never touches the
//     business ticker: it does not reset it, drain it, or change when the
//     next business pass is due.
//   - It runs on a child of the loop's context whose deadline is the D1 rule
//     (milestoneStageDeadline): the earliest of its own budget, the next
//     business tick and the owner's deadline. A cycle with no slack admits no
//     request at all. Cancellation is checked before every target and is
//     honored inside each in-flight request, so a cancelled loop releases the
//     current request and returns instead of finishing the roster. A stage
//     stopped by its own bound says so, says which bound, and says how much
//     roster it did not examine; an owner shutdown stays silent, because the
//     process is going away and a record nobody will read is not evidence.
//   - It spends at most milestoneCycleDispatchAllowance explicit HTTP attempts
//     per cycle, shared across targets, retries and client-ID fallback. The
//     allowance is checked, without being consumed, before every target; the
//     client charges it immediately before each dispatch and refuses further
//     dispatches, retry waits and fallback candidates once it is spent.
//   - Each eligible target is visited at most once per cycle, in roster order
//     from the loop-owned cursor. The cursor advances past the last target
//     that actually STARTED (made at least one dispatch), whatever that
//     dispatch's outcome; a target refused before its first dispatch keeps its
//     turn and is not reported as observed, so a slow or expensive first
//     target cannot monopolize the roster across cycles. Ineligible entries
//     are skipped and never move the cursor on their own.
//   - Every failure is observational only. There is no error and no state to
//     roll back — a failed observation cannot degrade mining. The only value
//     it returns is the cursor.
//   - No network or log I/O happens under a domain lock: each accessor takes
//     and releases its own lock and returns a value before the request or the
//     log call.
func (m *Miner) observeWatchStreakMilestones(owner context.Context, cycle milestoneCycle) (next int) {
	if m.client == nil || m.streamers == nil {
		return cycle.Cursor
	}

	// One roster snapshot per cycle. Everything below indexes this slice and
	// nothing else, so a streamer removed or re-added mid-cycle changes the
	// NEXT snapshot, never this loop.
	targets := m.streamers.All()
	n := len(targets)
	if n == 0 {
		return 0
	}
	start := ((cycle.Cursor % n) + n) % n
	next = start

	// Eligibility is decided once, up front, so the count of what was NOT
	// examined is exact even when a streamer's online state moves while a
	// request is in flight.
	eligible := make([]bool, n)
	eligibleCount := 0
	for i, s := range targets {
		if milestoneEligible(s) {
			eligible[i] = true
			eligibleCount++
		}
	}
	if eligibleCount == 0 {
		return start
	}

	stageStart := time.Now()
	deadline, source := milestoneStageDeadline(owner, cycle.Tick, cycle.Period, stageStart)
	slack := deadline.Sub(stageStart)
	allowance := twitch.NewDiagnosticAllowance(milestoneCycleDispatchAllowance)

	if owner.Err() != nil {
		// Shutdown: silent, see above.
		return start
	}
	if slack <= 0 {
		// Overdue or fully consumed cycle. Nothing is admitted and nothing is
		// started, so the cursor does not move: the target that was due keeps
		// its turn.
		logWatchStreakMilestoneCutoff(milestoneCutoffTime, source, slack, 0, eligibleCount, allowance)
		return start
	}

	// This deliberately does NOT claim to preserve TRANSPORT_TIMEOUT for a
	// stalled Twitch; see milestoneObservationCycleBudget, where that claim was
	// measured and found false. A stall inside this stage reads
	// CANCELLED/DEADLINE_EXCEEDED, because the transport retries its own 30s
	// timeout and the deadline expires first.
	ctx, cancel := context.WithDeadline(owner, deadline)
	defer cancel()

	started := 0
	var cutoff milestoneCutoff
	for k := 0; k < n; k++ {
		i := (start + k) % n
		if !eligible[i] {
			continue
		}
		// Both refusals happen BEFORE the target's first dispatch, so it keeps
		// its turn: next stays at i.
		if ctx.Err() != nil {
			cutoff, next = milestoneCutoffTime, i
			break
		}
		if allowance.Remaining() == 0 {
			cutoff, next = milestoneCutoffCount, i
			break
		}
		s := targets[i]
		obs := m.client.ObserveWatchStreakMilestone(ctx, s.ChannelID, s.GetUsername(), allowance)
		if obs.Dispatches == 0 {
			// Refused at the client boundary before any request left. Not
			// observed and not reported as observed; the class says why, and
			// the stage answers each in kind.
			if obs.FailureClass == twitch.MilestoneFailureNoChannelID {
				// The target lost its channel identity after the eligibility
				// mask was taken. Nothing was spent and nothing is exhausted,
				// so the roster continues; the target was not started and is
				// counted among the unexamined, and the cursor is not moved
				// on its behalf.
				continue
			}
			// The deadline landed between the check above and the dispatch,
			// or the client found the allowance spent. Keeps its turn. This is
			// a race-window path with no deterministic falsifier: the window
			// is the few instructions between two checks of the same context,
			// and the suite states that rather than faking a seam for it.
			cutoff, next = milestoneCutoffTime, i
			if obs.FailureClass == twitch.MilestoneFailureAllowanceExhausted {
				cutoff = milestoneCutoffCount
			}
			break
		}
		started++
		next = (i + 1) % n
		logWatchStreakMilestoneObservation(obs, s)
	}

	if cutoff != "" && owner.Err() == nil {
		// Owner shutdown and a stage bound end the same loop but are different
		// facts. Only the bound leaves a live miner with an unexplained short
		// cycle, so only the bound is reported.
		logWatchStreakMilestoneCutoff(cutoff, source, slack, started, eligibleCount-started, allowance)
	}
	return next
}

// logWatchStreakMilestoneCutoff records that a cycle stopped before its
// roster was finished, and why.
//
// It carries counts, not identities: the point is that the roster was
// truncated, and naming the streamers that were NOT looked at would add
// per-cycle volume without adding evidence. startedTargets is how many
// targets made at least one dispatch; unexaminedTargets is how many eligible
// targets did not, which is the set the cursor will revisit first, except a
// target whose channel identity disappeared after the mask was taken (counted
// here, skipped without moving the cursor).
// DEBUG for the same reason the observation record is: this recurs once per
// degraded cycle.
func logWatchStreakMilestoneCutoff(cutoff milestoneCutoff, source milestoneDeadlineSource, slack time.Duration,
	started, unexamined int, allowance *twitch.DiagnosticAllowance) {
	slog.Debug("Watch Streak milestone observation stopped before the roster was finished",
		"record", milestoneBudgetRecord,
		"cutoff", string(cutoff),
		"deadlineSource", string(source),
		"slackMs", slack.Milliseconds(),
		"startedTargets", started,
		"unexaminedTargets", unexamined,
		"dispatchesSpent", allowance.Spent(),
		"dispatchesRemaining", allowance.Remaining(),
		"dispatchAllowance", milestoneCycleDispatchAllowance,
		"cycleBudgetSeconds", milestoneObservationCycleBudget.Seconds(),
	)
}

// logWatchStreakMilestoneObservation emits one diagnostic observation record
// through the existing logging system.
//
// Only bounded, allowlisted provenance is written: channel identity, the
// request window, local observation time, the parser's presence
// classifications, the audited milestone fields, achievementTimestamp kept
// SEPARATELY from the sampling times, and the local broadcast context labelled
// as context only.
//
// Never written: the raw GQL payload, OAuth tokens or cookies, request or
// response headers, arbitrary raw error strings (failures arrive already
// reduced to a bounded class), credentials, or any playback token/signature.
//
// Identical milestone values observed at different times produce different
// records. Nothing here deduplicates: the temporal evidence is the evidence.
func logWatchStreakMilestoneObservation(obs twitch.WatchStreakMilestoneObservation, s *models.Streamer) {
	snap := obs.Snapshot

	// Read the local broadcast context once, after the request, as a plain
	// value. It is logged as CONTEXT ONLY and is never a binding: this
	// observation has no proof that the broadcast in view at log time has any
	// relationship to anything the response described.
	localBroadcast := ""
	if s != nil && s.Stream != nil {
		localBroadcast = s.Stream.GetBroadcastID()
	}

	attrs := []any{
		"record", milestoneObservationRecord,
		"sequence", obs.Sequence,
		"streamer", truncateForLog(obs.RequestedLogin),
		"channelId", truncateForLog(obs.RequestedChannelID),
		"requestStart", obs.RequestStart.UTC().Format(time.RFC3339Nano),
		"requestEnd", obs.RequestEnd.UTC().Format(time.RFC3339Nano),
		"requestDurationMs", obs.Duration().Milliseconds(),
		"observedAt", time.Now().UTC().Format(time.RFC3339Nano),
		"outcome", string(obs.Outcome),
		"failureClass", string(obs.FailureClass),
		// Explicit HTTP attempts this observation made against the shared
		// per-cycle allowance: retries and client-ID fallback included.
		"dispatches", obs.Dispatches,

		// Parser quality / presence classification, node by node.
		"dataPresence", presenceToken(snap.DataPresence),
		"channelPresence", presenceToken(snap.ChannelPresence),
		"selfPresence", presenceToken(snap.SelfPresence),
		"selfMilestonePresence", presenceToken(snap.SelfMilestonePresence),
		"milestoneNodePresence", presenceToken(snap.MilestoneNodePresence),

		// Observed channel identity as Twitch returned it, kept distinct from
		// the identity we asked for. Every value below carries its own presence
		// classification when it was not validly observed (see
		// milestoneLogString), so the record does not print it twice.
		"observedChannelId", milestoneLogString(snap.ChannelID),

		// Audited milestone fields. Values are reported, never interpreted.
		"milestoneId", milestoneLogString(snap.MilestoneID),
		"milestoneValue", milestoneLogInt(snap.MilestoneValue),
		// The wire kind is kept in its own slot rather than folded into
		// milestoneValue: "4" and 4 are the same observed integer but not the
		// same wire fact, and a reader must be able to see both without one
		// answer hiding the other.
		"milestoneValueWireKind", milestoneLogWireKind(snap.MilestoneValue),
		"shareStatus", milestoneLogString(snap.ShareStatus),
		"watchStreakThreshold", milestoneLogInt(snap.WatchStreakThreshold),
		"watchStreakCopoBonus", milestoneLogInt(snap.WatchStreakCopoBonus),
		"state", milestoneLogString(snap.State),
		"expiresAt", milestoneLogString(snap.ExpiresAt),

		// achievementTimestamp belongs to the observed achievement field. It is
		// NOT the sampling time of this response, which is requestStart/
		// requestEnd above, and the two are never merged.
		"achievementTimestamp", milestoneLogString(snap.AchievementTimestamp),

		"missedStreamsPresence", presenceToken(snap.MissedStreams.Presence),
		"missedStreamsCount", snap.MissedStreams.Count,
		"missedStreamsMalformed", snap.MissedStreams.MalformedCount,
	}

	ids := milestoneBroadcastIdentifierSummary(snap.MissedStreams)
	attrs = append(attrs,
		// Named for what they actually count: missedStreamsCount counts every
		// element, so an identically-named identifier "count" that silently
		// counted only the well-formed ones would invite a wrong reading.
		"broadcastIdentifierValidIds", ids.Total,
		"broadcastIdentifierMalformed", ids.Malformed,
		// The nested presence tallies, one per NODE. missedStreamElements
		// classifies the elements themselves; broadcastIdentifierArrays
		// classifies the child array of each element that had one. Without
		// both, a null element and an element whose child array is null render
		// identically, and a malformed, null or missing array is
		// indistinguishable from a genuinely empty one.
		"missedStreamElements", ids.Elements,
		"broadcastIdentifierArrays", ids.Arrays,
		"broadcastIdentifierElements", ids.IdentifierElements,
		"broadcastIdentifierIds", ids.IDs,
		"broadcastIdentifierSample", ids.Sample,

		// The relationships this feature cannot prove, printed explicitly. The
		// reward ladder is documented (officialWatchStreakLadder), but WHICH of
		// the fields above carries the authoritative streak count is not — so
		// no observation here may be read as a streak-count measurement.
		"localBroadcastContextOnly", renderWireString(localBroadcast),
		"grantLink", unknownLink,
		"streakCountField", unknownLink,
	)

	// DEBUG for EVERY outcome, including OBSERVED.
	//
	// This record is emitted for every started target on every bonus cycle,
	// forever: up to three per cycle under the shared allowance, one per cycle
	// in the unaccepted-hash steady state. internal/logger's console handler
	// filters on level alone — it has no message allowlist — and ConsoleLevel
	// defaults to INFO, so an INFO record here would print up to three long
	// lines per minute to stdout/docker logs for as long as the miner runs,
	// drowning a console that is deliberately kept
	// sparse. In the outcome this feature currently expects most
	// (UNSUPPORTED_QUERY, until the RewardList hash's live acceptance is
	// evidenced) every observed field is unset, so those lines would carry no
	// information at all.
	//
	// DEBUG loses nothing this feature needs: FileLevel defaults to DEBUG, so
	// the records still reach the retained log — the only persistence this
	// feature has and the only place its evidence was ever meant to live. An
	// operator who raises FileLevel above DEBUG turns the feature off, which is
	// the honest trade for not owning a settings surface.
	//
	// Not WARN or ERROR either: those are never hidden from the dashboard log
	// view, and an outcome that repeats every cycle and that the operator
	// cannot act on is not an alert. The stale-hash question is raised once, by
	// the transport, for BUSINESS reads only — a diagnostic read's own
	// PersistedQueryNotFound is deliberately DEBUG there too (see
	// logStaleHashExhausted), so nothing here is counting on it.
	slog.Debug("Watch Streak milestone observation", attrs...)
}

// milestoneBroadcastIdentifiers is the bounded rendering of every observed
// broadcastIdentifiers array.
//
// It reports exact counts, a bounded sample of the well-formed ids, AND two
// presence tallies: one over the arrays themselves and one over the individual
// id fields. The tallies are load-bearing — without them a MALFORMED array, a
// NULL one, a MISSING one and a genuinely EMPTY one all render as
// "count 0, malformed 0", which is the coercion of unparseable data into an
// observed "this stream has no broadcast identifiers" that this feature must
// never perform.
type milestoneBroadcastIdentifiers struct {
	Sample    []string
	Total     int
	Malformed int
	// Elements is the tally over each missedStreams ELEMENT's own presence.
	// It is what keeps a null element distinguishable from a well-formed
	// element whose broadcastIdentifiers child is null: without it both render
	// as a single NULL somewhere in the record and the two wire facts collapse.
	Elements string
	Arrays   string
	// IdentifierElements tallies each broadcastIdentifiers ELEMENT's own
	// presence; IDs tallies the id NODE of the elements that had one. Two
	// tallies, because a null element and an element whose id is null are
	// different wire facts.
	IdentifierElements string
	IDs                string
}

func milestoneBroadcastIdentifierSummary(missed twitch.MilestoneMissedStreams) milestoneBroadcastIdentifiers {
	out := milestoneBroadcastIdentifiers{}
	elements := map[twitch.MilestoneFieldPresence]int{}
	arrays := map[twitch.MilestoneFieldPresence]int{}
	identifierElements := map[twitch.MilestoneFieldPresence]int{}
	ids := map[twitch.MilestoneFieldPresence]int{}
	for _, entry := range missed.Entries {
		// The ELEMENT's own classification, always.
		elements[entry.Presence]++
		if entry.Presence != twitch.MilestoneFieldValid {
			// No element object, so no child array was ever observed. Tallying
			// its zero-valued presence here would invent a child observation
			// for a parent that does not exist.
			continue
		}
		arrays[entry.BroadcastIdentifiers.Presence]++
		out.Total += len(entry.BroadcastIdentifiers.IDs)
		out.Malformed += entry.BroadcastIdentifiers.MalformedCount
		for _, element := range entry.BroadcastIdentifiers.Elements {
			identifierElements[element.Presence]++
			if element.Presence != twitch.MilestoneFieldValid {
				// No element object, so no id node was ever observed.
				continue
			}
			ids[element.ID.Presence]++
		}
		for _, id := range entry.BroadcastIdentifiers.IDs {
			if len(out.Sample) < milestoneLogIDSample {
				out.Sample = append(out.Sample, renderWireString(id))
			}
		}
	}
	out.Elements = presenceTally(elements)
	out.Arrays = presenceTally(arrays)
	out.IdentifierElements = presenceTally(identifierElements)
	out.IDs = presenceTally(ids)
	return out
}

// milestoneLogString renders a string field for a log record. A field that was
// not validly observed prints as its presence classification in angle brackets
// — never as an empty string that could be misread as an observed empty value.
// A validly observed value that would READ like a classification is not
// allowed to: the brackets are printable, so a wire string spelled "<MISSING>"
// survives sanitizeForLog byte-for-byte and would let the peer choose which
// presence class the record SHOWS. renderWireString therefore quotes any such
// value (an absent state prints as <MISSING>; an observed state spelled
// "<MISSING>" prints as "\"<MISSING>\""; an observed state spelled MISSING,
// with no brackets, prints as MISSING).
//
// Because the classification is carried in the value itself, the record does
// not also emit a separate <field>Presence attribute for every scalar. That is
// not lost information — it is the same information, once instead of twice, in
// a record emitted every cycle for up to three targets.
func milestoneLogString(f twitch.MilestoneStringField) string {
	if f.Presence != twitch.MilestoneFieldValid {
		return presenceToken(f.Presence)
	}
	return renderWireString(f.Value)
}

// renderWireString renders a validly observed WIRE string for a log record so
// that it can never be read as one of the record's own sentinels. The closed
// vocabulary - the bracketed presence tokens, the UNKNOWN and NONE relationship
// sentinels and the truncation marker - is made of printable characters, so
// sanitizeForLog passes a wire value spelled like one of them straight through.
// A value that would collide is rendered quoted (strconv.Quote), which no
// sentinel ever is; every other value renders exactly as before. The check
// runs on the sanitized, untruncated value, so a genuinely truncated long value
// keeps its plain marker and only a wire value that SPELLS the marker is quoted.
//
// Quoting is applied to the ENCODED form's bound, not to an already-bounded
// value: strconv.Quote doubles every backslash and quotation mark, so quoting
// a 128-byte value could otherwise print up to 258 bytes and let the peer
// inflate the record past milestoneLogStringCap. quoteBoundedForLog keeps the
// quoted encoding within the cap, so a colliding value is bounded exactly as
// tightly as a plain one (cap plus the truncation marker).
func renderWireString(v string) string {
	s := sanitizeForLog(v)
	if strings.HasPrefix(s, "<") || s == unknownLink || s == noProvenBroadcast || strings.HasSuffix(s, truncatedMarker) {
		return quoteBoundedForLog(s)
	}
	return boundForLog(s)
}

// quoteBoundedForLog renders an already-sanitized value as a Go-quoted string
// whose quoted encoding is at most milestoneLogStringCap bytes. A value whose
// whole encoding fits is quoted as is. A longer one is cut on a whole rune —
// never inside an escape sequence — so that the quoted part stays a
// well-formed literal, and the cut is marked with truncatedMarker AFTER the
// closing quotation mark, where it reads as this record's marker rather than
// as part of the wire value. The result is therefore never longer than
// boundForLog's own bound of milestoneLogStringCap plus the marker.
func quoteBoundedForLog(v string) string {
	if q := strconv.Quote(v); len(q) <= milestoneLogStringCap {
		return q
	}
	var b strings.Builder
	b.Grow(milestoneLogStringCap + len(truncatedMarker))
	b.WriteByte('"')
	for i := 0; i < len(v); {
		_, width := utf8.DecodeRuneInString(v[i:])
		// strconv.Quote encodes rune by rune, so quoting one rune on its own
		// yields exactly the bytes it would contribute to the whole literal:
		// itself, or an escape sequence. The wrapping quotation marks are
		// stripped.
		enc := strconv.Quote(v[i : i+width])
		enc = enc[1 : len(enc)-1]
		if b.Len()+len(enc)+1 > milestoneLogStringCap {
			break
		}
		b.WriteString(enc)
		i += width
	}
	b.WriteByte('"')
	b.WriteString(truncatedMarker)
	return b.String()
}

// milestoneLogInt renders an integer field for a log record. A field that was
// not validly observed prints as its bracketed presence classification, never
// as 0.
func milestoneLogInt(f twitch.MilestoneIntField) string {
	if f.Presence != twitch.MilestoneFieldValid {
		return presenceToken(f.Presence)
	}
	return strconv.Itoa(f.Value)
}

// milestoneLogWireKind renders which JSON encoding a validly observed integer
// arrived in. A field that was not validly observed has no encoding to report
// and reads as <UNSET>, never as a borrowed or defaulted one.
//
// The vocabulary is closed by CONSTRUCTION, not by convention: WireKind is an
// exported string field, so anything outside the two known encodings renders as
// <UNSET> rather than being passed through into a log attribute. That keeps the
// unsanitized string() conversion safe no matter who fills the struct.
func milestoneLogWireKind(f twitch.MilestoneIntField) string {
	if f.Presence != twitch.MilestoneFieldValid {
		return "<UNSET>"
	}
	switch f.WireKind {
	case twitch.MilestoneWireKindNumber, twitch.MilestoneWireKindString:
		return string(f.WireKind)
	default:
		return "<UNSET>"
	}
}

// presenceToken renders a presence classification so it can never be confused
// with an observed value. An empty classification (a node the parser never
// reached at all, e.g. after a failed request) reads as <UNSET>.
func presenceToken(p twitch.MilestoneFieldPresence) string {
	if p == "" {
		return "<UNSET>"
	}
	return "<" + string(p) + ">"
}

// truncateForLog makes one observed Twitch string safe and bounded for a log
// record.
//
// It does two things, in this order, and both matter:
//
// It first replaces every non-printable rune (and every invalid byte) with a
// single U+FFFD. slog's TextHandler escapes control characters on render, so a
// raw byte can become four rendered ones — bounding the RAW length alone would
// let a control-character-laden response render several times larger than the
// measured record size this feature publishes. Replacing first makes the
// rendered size track the bounded size. It also removes any question of a
// newline or terminal escape reaching an operator's console through a value
// Twitch controls.
//
// It then bounds the result, cutting on a RUNE boundary — a byte-offset cut can
// split a multi-byte UTF-8 sequence — and marks the truncation so a reader
// never mistakes a cut value for a complete one.
func truncateForLog(v string) string {
	return boundForLog(sanitizeForLog(v))
}

// truncatedMarker is appended to a value cut by boundForLog.
const truncatedMarker = "...(truncated)"

// boundForLog cuts an already-sanitized value to milestoneLogStringCap on a
// rune boundary and marks the cut.
func boundForLog(v string) string {
	if len(v) <= milestoneLogStringCap {
		return v
	}
	cut := milestoneLogStringCap
	for cut > 0 && !utf8.RuneStart(v[cut]) {
		cut--
	}
	return v[:cut] + truncatedMarker
}

// sanitizeForLog replaces non-printable and invalid runes with U+FFFD, leaving
// ordinary text untouched. A string that is already printable is returned as-is
// so the common path allocates nothing.
func sanitizeForLog(v string) string {
	safe := true
	for _, r := range v {
		if r == utf8.RuneError || !unicode.IsPrint(r) {
			safe = false
			break
		}
	}
	if safe {
		return v
	}
	var b strings.Builder
	b.Grow(len(v))
	for _, r := range v {
		if r == utf8.RuneError || !unicode.IsPrint(r) {
			b.WriteRune('\uFFFD')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// milestonePresenceOrder is the stable order presence tallies are rendered in,
// so two records are diffable.
var milestonePresenceOrder = []twitch.MilestoneFieldPresence{
	twitch.MilestoneFieldValid,
	twitch.MilestoneFieldEmpty,
	twitch.MilestoneFieldMissing,
	twitch.MilestoneFieldNull,
	twitch.MilestoneFieldMalformed,
}

// presenceTally renders a presence histogram as a compact, stable string such
// as "VALID:2,NULL:1". It is how a repeated nested node keeps its per-element
// classifications in a bounded record: a single aggregate count would collapse
// MISSING, NULL, MALFORMED and a non-object element into one number and lose
// the distinction the parser preserved. An empty histogram renders as "none".
func presenceTally(counts map[twitch.MilestoneFieldPresence]int) string {
	parts := make([]string, 0, len(milestonePresenceOrder))
	for _, p := range milestonePresenceOrder {
		if n := counts[p]; n > 0 {
			parts = append(parts, string(p)+":"+strconv.Itoa(n))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ",")
}

// logWatchStreakGrantCorrelation emits one diagnostic record for a WATCH_STREAK
// grant the domain has NEWLY ACCEPTED, stating separately:
//
//   - what the domain admitted (and under which binding);
//   - the already-existing event identity and the exact event-local
//     total_points, unchanged;
//   - Twitch's own wire timestamp;
//   - local acceptance time (from the admitted grant fact);
//   - what the exact ledger actually did, and when that outcome was known.
//
// It reads the accounting outcome; it never decides it, and it never reports
// COMMITTED from domain acceptance alone. It does not replace #303's accounting
// timestamp semantics: the ledger's own timestamp is the ledger's, and the
// local times recorded here are labelled as local.
//
// Amount invariants: the exact total_points is copied through unchanged. No
// amount is filtered, rewritten, promoted or rejected here — 300, 350, 400, 450
// and every other valid integer amount are reported exactly as received.
//
// The record additionally LABELS the amount against Twitch's documented ladder
// (officialWatchStreakLadder). That label is a consistency statement about the
// amount, not a measurement of the streak: the ladder is flat at 5-or-more, no
// RewardList field is proven to carry the streak count, and an amount that
// matches no documented base rung is reported as off-ladder and otherwise
// treated exactly like any other. provenStreakCount therefore stays UNKNOWN on
// every record, including a +450 one.
func (m *Miner) logWatchStreakGrantCorrelation(
	msg *pubsub.PubSubMessage,
	s *models.Streamer,
	pointGain map[string]interface{},
	grant models.WatchStreakGrantResult,
	ledger pointLedgerOutcome,
) {
	if msg == nil || s == nil {
		return
	}

	total, exact := exactWirePoints(pointGain["total_points"])
	exactPoints := unknownLink
	if exact {
		exactPoints = strconv.Itoa(total)
	}

	// baseline_points is read as the frame's base (pre-multiplier) amount - an
	// assumption from the field's name and the multipliers array beside it, not
	// an attested wire fact; see officialLadderConsistency. It is read here for
	// the ladder label only — the exact ledger still accounts total_points, and
	// #303's accounting is untouched.
	baseline, baselineExact := exactWirePoints(pointGain["baseline_points"])
	baselinePoints := unknownLink
	if baselineExact {
		baselinePoints = strconv.Itoa(baseline)
	}
	ladderTier, ladderBasis := officialLadderConsistency(total, exact, baseline, baselineExact)

	wireTimestamp := unknownLink
	if ts, ok := msg.Data["timestamp"].(string); ok && ts != "" {
		wireTimestamp = renderWireString(ts)
	}

	// One lookup for the whole record. admittedGrantFact is a linear scan over
	// the retained grant snapshot, and every field below asks the same question
	// about the same event, so scanning once per field would grow the cost of a
	// record with the history it reads.
	fact, haveFact := admittedGrantFact(grant, msg.EventFingerprint)

	acceptedAt := unknownLink
	if haveFact {
		acceptedAt = fact.AcceptedAt.UTC().Format(time.RFC3339Nano)
	}

	// The currently observed broadcast is LOCAL CONTEXT. The WATCH_STREAK frame
	// carries no provable BroadcastID, so provenBroadcastId reports what the
	// admitted grant fact actually says — NONE when the fact exists and carries
	// no binding, UNKNOWN when no fact was found — and the current
	// Stream.BroadcastID is never promoted into that role.
	localBroadcast := ""
	if s.Stream != nil {
		localBroadcast = s.Stream.GetBroadcastID()
	}
	// NONE and UNKNOWN are different answers. NONE means the admitted grant fact
	// was found and carries no proven broadcast — a positive observation.
	// UNKNOWN means no fact was found at all, so nothing is known either way;
	// printing NONE there would assert an absence this code cannot see.
	provenBroadcast := unknownLink
	if haveFact {
		provenBroadcast = noProvenBroadcast
		if fact.BroadcastID != "" {
			// A wire value, rendered so it can never spell a sentinel.
			provenBroadcast = renderWireString(fact.BroadcastID)
		}
	}

	slog.Info("Watch Streak grant correlation",
		"record", milestoneCorrelationRecord,
		"streamer", truncateForLog(s.GetUsername()),
		"channelId", truncateForLog(s.ChannelID),

		// Domain admission — accepted, and under which binding. Never conflated
		// with the ledger outcome below.
		"domainAdmission", string(grant.Admission),
		"domainAccepted", grant.NewlyAccepted(),
		"grantBinding", grantBindingOf(fact, haveFact),

		// The already-existing event identity and exact event-local amount.
		"eventId", truncateForLog(msg.EventFingerprint),
		"exactTotalPoints", exactPoints,
		"exactAmount", exact,
		"baselinePoints", baselinePoints,

		// The amount's relationship to Twitch's DOCUMENTED ladder. A label on
		// the amount, never a streak-count measurement: provenStreakCount stays
		// UNKNOWN because no RewardList field is proven to carry it.
		"officialLadderConsistency", string(ladderTier),
		"officialLadderBasis", ladderBasis,
		"provenStreakCount", unknownLink,

		// Three distinct clocks, kept distinct.
		"wireTimestamp", wireTimestamp,
		"localAcceptedAt", acceptedAt,
		"accountingOutcomeAt", time.Now().UTC().Format(time.RFC3339Nano),

		// What the exact ledger actually did.
		"ledgerOutcome", string(ledger),

		// Relationships this feature cannot prove, printed explicitly.
		"provenBroadcastId", provenBroadcast,
		"localBroadcastContextOnly", renderWireString(localBroadcast),
		"milestoneLink", unknownLink,
	)
}

// admittedGrantFact finds the admitted grant fact for eventID in the snapshot
// the domain returned. It reads the persistence snapshot that already exists;
// it creates no state and stores nothing.
func admittedGrantFact(grant models.WatchStreakGrantResult, eventID string) (models.WatchStreakGrantFact, bool) {
	if eventID == "" {
		return models.WatchStreakGrantFact{}, false
	}
	for _, fact := range grant.Persistence.Grants {
		if fact.EventID == eventID {
			return fact, true
		}
	}
	return models.WatchStreakGrantFact{}, false
}

// grantBindingOf reports an admitted grant's binding, or this feature's
// explicit UNKNOWN vocabulary when no fact was found. It never guesses a bound
// binding, and it never prints a bare empty string that a reader could mistake
// for an observed unbound binding.
//
// It takes the already-resolved fact rather than looking it up again, so a
// record that reads several fields off one grant pays for one scan.
func grantBindingOf(fact models.WatchStreakGrantFact, haveFact bool) string {
	if !haveFact {
		return unknownLink
	}
	return string(fact.Binding)
}
