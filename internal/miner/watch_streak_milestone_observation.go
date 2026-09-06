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
//   - They never synthesize a Watch Streak reward, and never assert a
//     milestone->points truth table. Whether 300/350/400/450 form an official
//     Twitch ladder is UNKNOWN. A received +450 implies no expected tier.
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
	"time"

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
		"streamer", obs.RequestedLogin,
		"channelId", obs.RequestedChannelID,
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
		// the identity we asked for.
		"observedChannelIdPresence", string(snap.ChannelID.Presence),
		"observedChannelId", milestoneLogString(snap.ChannelID),

		// Audited milestone fields. Values are reported, never interpreted.
		"milestoneIdPresence", string(snap.MilestoneID.Presence),
		"milestoneId", milestoneLogString(snap.MilestoneID),
		"milestoneValuePresence", string(snap.MilestoneValue.Presence),
		"milestoneValue", milestoneLogInt(snap.MilestoneValue),
		"shareStatusPresence", string(snap.ShareStatus.Presence),
		"shareStatus", milestoneLogString(snap.ShareStatus),
		"watchStreakThresholdPresence", string(snap.WatchStreakThreshold.Presence),
		"watchStreakThreshold", milestoneLogInt(snap.WatchStreakThreshold),
		"watchStreakCopoBonusPresence", string(snap.WatchStreakCopoBonus.Presence),
		"watchStreakCopoBonus", milestoneLogInt(snap.WatchStreakCopoBonus),
		"statePresence", string(snap.State.Presence),
		"state", milestoneLogString(snap.State),
		"expiresAtPresence", string(snap.ExpiresAt.Presence),
		"expiresAt", milestoneLogString(snap.ExpiresAt),

		// achievementTimestamp belongs to the observed achievement field. It is
		// NOT the sampling time of this response, which is requestStart/
		// requestEnd above, and the two are never merged.
		"achievementTimestampPresence", string(snap.AchievementTimestamp.Presence),
		"achievementTimestamp", milestoneLogString(snap.AchievementTimestamp),

		"missedStreamsPresence", string(snap.MissedStreams.Presence),
		"missedStreamsCount", snap.MissedStreams.Count,
		"missedStreamsMalformed", snap.MissedStreams.MalformedCount,
	}

	ids, idCount, idMalformed := milestoneBroadcastIdentifierSummary(snap.MissedStreams)
	attrs = append(attrs,
		"broadcastIdentifierCount", idCount,
		"broadcastIdentifierMalformed", idMalformed,
		"broadcastIdentifierSample", ids,

		// The two relationships this feature cannot prove, printed explicitly.
		"localBroadcastContextOnly", localBroadcast,
		"grantLink", unknownLink,
		"expectedMilestone", unknownLink,
	)

	switch obs.Outcome {
	case twitch.MilestoneObserved, twitch.MilestoneGraphQLError, twitch.MilestoneUnsupported:
		// INFO, including UNSUPPORTED_QUERY. That outcome does need an owner
		// decision, but it is NOT this record's job to raise it: the shared GQL
		// transport already logs its own ERROR naming the operation and
		// pointing at internal/constants/gql.go once every candidate client ID
		// has returned PersistedQueryNotFound. Since internal/web/logclass.go
		// keeps unmatched INFO/DEBUG lines off the dashboard log view and never
		// hides a WARN or ERROR, raising this one to WARN would put a SECOND
		// dashboard-visible line next to that ERROR for every online streamer
		// on every bonus cycle — volume, not information. The diagnostic record
		// stays in the retained log, which is where this feature's evidence
		// lives.
		slog.Info("Watch Streak milestone observation", attrs...)
	default:
		slog.Debug("Watch Streak milestone observation unavailable", attrs...)
	}
}

// milestoneBroadcastIdentifierSummary flattens the observed broadcast
// identifiers into exact counts plus a bounded sample. Counts are never
// truncated; only the printed list is.
func milestoneBroadcastIdentifierSummary(missed twitch.MilestoneMissedStreams) (sample []string, total, malformed int) {
	for _, entry := range missed.Entries {
		total += len(entry.BroadcastIdentifiers.IDs)
		malformed += entry.BroadcastIdentifiers.MalformedCount
		for _, id := range entry.BroadcastIdentifiers.IDs {
			if len(sample) < milestoneLogIDSample {
				sample = append(sample, truncateForLog(id))
			}
		}
	}
	return sample, total, malformed
}

// milestoneLogString renders a string field for a log record. A field that was
// not validly observed prints as its presence classification, never as an empty
// string that could be misread as an observed empty value.
func milestoneLogString(f twitch.MilestoneStringField) string {
	if f.Presence != twitch.MilestoneFieldValid {
		return string(f.Presence)
	}
	return truncateForLog(f.Value)
}

// milestoneLogInt renders an integer field for a log record. A field that was
// not validly observed prints as its presence classification, never as 0.
func milestoneLogInt(f twitch.MilestoneIntField) string {
	if f.Presence != twitch.MilestoneFieldValid {
		return string(f.Presence)
	}
	return strconv.Itoa(f.Value)
}

// truncateForLog bounds one observed string. Truncation is marked so a reader
// never mistakes a cut value for a complete one.
func truncateForLog(v string) string {
	if len(v) <= milestoneLogStringCap {
		return v
	}
	return v[:milestoneLogStringCap] + "...(truncated)"
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
// and every other valid integer amount are reported exactly as received, and
// none of them implies an expected milestone.
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
	provenBroadcast := "NONE"
	if fact, ok := admittedGrantFact(grant, msg.EventFingerprint); ok && fact.BroadcastID != "" {
		provenBroadcast = fact.BroadcastID
	}

	slog.Info("Watch Streak grant correlation",
		"record", milestoneCorrelationRecord,
		"streamer", s.GetUsername(),
		"channelId", s.ChannelID,

		// Domain admission — accepted, and under which binding. Never conflated
		// with the ledger outcome below.
		"domainAdmission", string(grant.Admission),
		"domainAccepted", grant.NewlyAccepted(),
		"grantBinding", string(grantBinding(grant, msg.EventFingerprint)),

		// The already-existing event identity and exact event-local amount.
		"eventId", truncateForLog(msg.EventFingerprint),
		"exactTotalPoints", exactPoints,
		"exactAmount", exact,

		// Three distinct clocks, kept distinct.
		"wireTimestamp", wireTimestamp,
		"localAcceptedAt", acceptedAt,
		"accountingOutcomeAt", time.Now().UTC().Format(time.RFC3339Nano),

		// What the exact ledger actually did.
		"ledgerOutcome", string(ledger),

		// Relationships this feature cannot prove, printed explicitly.
		"provenBroadcastId", provenBroadcast,
		"localBroadcastContextOnly", localBroadcast,
		"milestoneLink", unknownLink,
		"expectedMilestone", unknownLink,
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

// grantBinding reports the admitted grant's binding, or the empty binding when
// the fact is not present in the snapshot. It never guesses a bound binding.
func grantBinding(grant models.WatchStreakGrantResult, eventID string) models.WatchStreakGrantBinding {
	if fact, ok := admittedGrantFact(grant, eventID); ok {
		return fact.Binding
	}
	return ""
}
