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
	"unicode/utf8"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/pubsub"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/twitch"
)

// Record kinds. Stable strings so a retained log can be filtered.
const (
	milestoneObservationRecord = "watch_streak_milestone_observation"
	milestoneCorrelationRecord = "watch_streak_grant_correlation"
)

// unknownLink is the single vocabulary for a relationship this feature cannot
// prove. It is printed explicitly rather than omitted, so a reader sees that
// the question was asked and answered UNKNOWN — not that it was never asked.
const unknownLink = "UNKNOWN"

// milestoneLogStringCap bounds every free-form observed string reaching a log
// record. Twitch's milestone fields are short identifiers and timestamps; a
// response that disagrees gets truncated (and marked truncated) instead of
// writing an unbounded blob into the operator's log.
const milestoneLogStringCap = 128

// milestoneLogIDSample bounds how many observed broadcast identifiers a single
// record prints. Counts are always exact; the identifier list is a sample.
const milestoneLogIDSample = 8

// officialWatchStreakLadder is Twitch's DOCUMENTED Watch Streak reward ladder,
// keyed by streak count.
//
// Source: Twitch, "Viewer Channel Point Guide"
// (https://help.twitch.tv/s/article/viewer-channel-point-guide), supplied as
// owner evidence and recorded 2026-09-06. Independently corroborated by the
// already-audited donor mpforce1/Twitch-Channel-Points-Miner (ref
// f1dda17ad61562ca2e93d975ee0a24e8b2f7ea0c, README blob
// d5b60c22c5e6387cbe69c3e7e951b32d946672f6), which states the same
// progression. The same guide documents that a qualifying stream must run at
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
// exactly representable — the pre-multiplier amount the ladder actually
// describes — and totalPoints is the credited amount #303 accounts. Preferring
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

// observeWatchStreakMilestones is the optional observation stage of one bonus
// cycle.
//
// Lifecycle contract, all of it load-bearing:
//
//   - It owns nothing. It is a plain function call on the EXISTING
//     bonusPollLoop goroutine, invoked only AFTER that cycle's business pass
//     (pollBonuses: bonus claiming and auto-redeem) has fully returned. It is
//     never interleaved with a claim or redemption.
//   - It runs on the loop's own context. Cancellation is checked before every
//     target and is honored inside each in-flight request, so a cancelled loop
//     releases the current request and returns instead of finishing the roster.
//   - At most ONE RewardList request per eligible target per cycle. Eligibility
//     is deliberately narrow and side-effect free: online, with a channel
//     identity to scope the request. It does not consult the watch-streak
//     setting, because that setting governs PURSUIT, and gating observation on
//     it would suppress evidence for exactly the grants this exists to observe.
//   - Every failure is observational only. There is no return value, no error,
//     and no state to roll back — a failed observation cannot degrade mining.
//   - No network or log I/O happens under a domain lock: each accessor takes
//     and releases its own lock and returns a value before the request or the
//     log call.
func (m *Miner) observeWatchStreakMilestones(ctx context.Context) {
	if m.client == nil || m.streamers == nil {
		return
	}
	for _, s := range m.streamers.All() {
		if ctx.Err() != nil {
			return
		}
		if !s.GetIsOnline() {
			continue
		}
		channelID := s.ChannelID
		if channelID == "" {
			continue
		}
		obs := m.client.ObserveWatchStreakMilestone(ctx, channelID, s.GetUsername())
		logWatchStreakMilestoneObservation(obs, s)
	}
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

		// Parser quality / presence classification, node by node.
		"dataPresence", string(snap.DataPresence),
		"channelPresence", string(snap.ChannelPresence),
		"selfPresence", string(snap.SelfPresence),
		"selfMilestonePresence", string(snap.SelfMilestonePresence),
		"milestoneNodePresence", string(snap.MilestoneNodePresence),

		// Observed channel identity as Twitch returned it, kept distinct from
		// the identity we asked for. Every value below carries its own presence
		// classification when it was not validly observed (see
		// milestoneLogString), so the record does not print it twice.
		"observedChannelId", milestoneLogString(snap.ChannelID),

		// Audited milestone fields. Values are reported, never interpreted.
		"milestoneId", milestoneLogString(snap.MilestoneID),
		"milestoneValue", milestoneLogInt(snap.MilestoneValue),
		"shareStatus", milestoneLogString(snap.ShareStatus),
		"watchStreakThreshold", milestoneLogInt(snap.WatchStreakThreshold),
		"watchStreakCopoBonus", milestoneLogInt(snap.WatchStreakCopoBonus),
		"state", milestoneLogString(snap.State),
		"expiresAt", milestoneLogString(snap.ExpiresAt),

		// achievementTimestamp belongs to the observed achievement field. It is
		// NOT the sampling time of this response, which is requestStart/
		// requestEnd above, and the two are never merged.
		"achievementTimestamp", milestoneLogString(snap.AchievementTimestamp),

		"missedStreamsPresence", string(snap.MissedStreams.Presence),
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
		// The nested presence tallies: without these a malformed, null or
		// missing broadcastIdentifiers array is indistinguishable from a
		// genuinely empty one.
		"broadcastIdentifierArrays", ids.Arrays,
		"broadcastIdentifierIds", ids.IDs,
		"broadcastIdentifierSample", ids.Sample,

		// The relationships this feature cannot prove, printed explicitly. The
		// reward ladder is documented (officialWatchStreakLadder), but WHICH of
		// the fields above carries the authoritative streak count is not — so
		// no observation here may be read as a streak-count measurement.
		"localBroadcastContextOnly", truncateForLog(localBroadcast),
		"grantLink", unknownLink,
		"streakCountField", unknownLink,
	)

	// DEBUG for EVERY outcome, including OBSERVED.
	//
	// This record is emitted once per online streamer per bonus cycle, forever.
	// internal/logger's console handler filters on level alone — it has no
	// message allowlist — and ConsoleLevel defaults to INFO, so an INFO record
	// here would print N long lines per minute to stdout/docker logs for as
	// long as the miner runs, drowning a console that is deliberately kept
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
	Arrays    string
	IDs       string
}

func milestoneBroadcastIdentifierSummary(missed twitch.MilestoneMissedStreams) milestoneBroadcastIdentifiers {
	out := milestoneBroadcastIdentifiers{}
	arrays := map[twitch.MilestoneFieldPresence]int{}
	ids := map[twitch.MilestoneFieldPresence]int{}
	for _, entry := range missed.Entries {
		arrays[entry.BroadcastIdentifiers.Presence]++
		out.Total += len(entry.BroadcastIdentifiers.IDs)
		out.Malformed += entry.BroadcastIdentifiers.MalformedCount
		for _, element := range entry.BroadcastIdentifiers.Elements {
			ids[element.Presence]++
		}
		for _, id := range entry.BroadcastIdentifiers.IDs {
			if len(out.Sample) < milestoneLogIDSample {
				out.Sample = append(out.Sample, truncateForLog(id))
			}
		}
	}
	out.Arrays = presenceTally(arrays)
	out.IDs = presenceTally(ids)
	return out
}

// milestoneLogString renders a string field for a log record. A field that was
// not validly observed prints as its presence classification in angle brackets
// — never as an empty string that could be misread as an observed empty value,
// and never ambiguous with a Twitch string that happens to READ like a
// classification (a state field whose observed value is literally "MISSING"
// prints as MISSING; an absent one prints as <MISSING>).
//
// Because the classification is carried in the value itself, the record does
// not also emit a separate <field>Presence attribute for every scalar. That is
// not lost information — it is the same information, once instead of twice, in
// a record emitted for every online streamer on every bonus cycle.
func milestoneLogString(f twitch.MilestoneStringField) string {
	if f.Presence != twitch.MilestoneFieldValid {
		return presenceToken(f.Presence)
	}
	return truncateForLog(f.Value)
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

// presenceToken renders a presence classification so it can never be confused
// with an observed value. An empty classification (a node the parser never
// reached at all, e.g. after a failed request) reads as <UNSET>.
func presenceToken(p twitch.MilestoneFieldPresence) string {
	if p == "" {
		return "<UNSET>"
	}
	return "<" + string(p) + ">"
}

// truncateForLog bounds one observed string. It cuts on a RUNE boundary — a
// byte-offset cut can split a multi-byte UTF-8 sequence and put an invalid rune
// into the operator's log — and marks the truncation so a reader never mistakes
// a cut value for a complete one.
func truncateForLog(v string) string {
	if len(v) <= milestoneLogStringCap {
		return v
	}
	cut := milestoneLogStringCap
	for cut > 0 && !utf8.RuneStart(v[cut]) {
		cut--
	}
	return v[:cut] + "...(truncated)"
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

	// baseline_points is the frame's PRE-MULTIPLIER amount. It is read here for
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
		wireTimestamp = truncateForLog(ts)
	}

	acceptedAt := unknownLink
	if fact, ok := admittedGrantFact(grant, msg.EventFingerprint); ok {
		acceptedAt = fact.AcceptedAt.UTC().Format(time.RFC3339Nano)
	}

	// The currently observed broadcast is LOCAL CONTEXT. The WATCH_STREAK frame
	// carries no provable BroadcastID, so provenBroadcastId is reported as NONE
	// and the current Stream.BroadcastID is never promoted into that role.
	localBroadcast := ""
	if s.Stream != nil {
		localBroadcast = s.Stream.GetBroadcastID()
	}
	// NONE and UNKNOWN are different answers. NONE means the admitted grant fact
	// was found and carries no proven broadcast — a positive observation.
	// UNKNOWN means no fact was found at all, so nothing is known either way;
	// printing NONE there would assert an absence this code cannot see.
	provenBroadcast := unknownLink
	if fact, ok := admittedGrantFact(grant, msg.EventFingerprint); ok {
		provenBroadcast = "NONE"
		if fact.BroadcastID != "" {
			provenBroadcast = fact.BroadcastID
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
		"grantBinding", grantBinding(grant, msg.EventFingerprint),

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
		"provenBroadcastId", truncateForLog(provenBroadcast),
		"localBroadcastContextOnly", truncateForLog(localBroadcast),
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

// grantBinding reports the admitted grant's binding, or this feature's explicit
// UNKNOWN vocabulary when the fact is not present in the snapshot. It never
// guesses a bound binding, and it never prints a bare empty string that a
// reader could mistake for an observed unbound binding.
func grantBinding(grant models.WatchStreakGrantResult, eventID string) string {
	if fact, ok := admittedGrantFact(grant, eventID); ok {
		return string(fact.Binding)
	}
	return unknownLink
}
