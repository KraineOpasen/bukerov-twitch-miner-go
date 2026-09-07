package twitch

// Watch Streak milestone OBSERVATION.
//
// This file adds exactly one narrow, READ-ONLY capability: sample Twitch's
// RewardList operation for a channel and report, as a structured diagnostic
// fact, what the Watch Streak milestone node actually contained.
//
// It is observability, nothing else. It:
//   - grants no reward and synthesizes none;
//   - owns no state, no timer, no goroutine and no cache;
//   - changes no Stream state, no Channel Points capability, no pursuit or
//     recovery, no selection/rotation/broker state;
//   - blocks no claim or redemption path;
//   - assigns NO meaning to the values it reads.
//
// The field names below (value, watchStreakThreshold, watchStreakCopoBonus,
// state, milestone IDs, ...) are OBSERVED TWITCH FACTS, and this package
// deliberately does not interpret them.
//
// Two things remain unknown here, and the distinction between them and the
// ladder is sharp. Twitch's Viewer Channel Point Guide documents the reward ladder
// itself — streak count 2 -> +300, 3 -> +350, 4 -> +400, 5 or more -> +450 —
// so the ladder is a current official fact, recorded with its source and
// retrieval date in internal/miner/watch_streak_milestone_observation.go.
//
// What remains UNKNOWN is which RewardList field, if any, carries the
// authoritative STREAK COUNT in the live response. Nothing here may assume
// that value, watchStreakThreshold or watchStreakCopoBonus is that field: they
// keep their raw observed meanings until source or runtime evidence proves
// otherwise. Knowing the ladder is not knowing where the rung number lives.
//
// The second unknown is the RewardList persisted query's live acceptance, which
// stays PENDING runtime evidence (see the provenance note below).
//
// Provenance: the RewardList wire facts are clean-room protocol evidence from
// mpforce1/Twitch-Channel-Points-Miner, ref
// f1dda17ad61562ca2e93d975ee0a24e8b2f7ea0c, tree
// 650ddd7ed974f78fab1956e41c0849ea177545b2, GPL-3.0. Two classes of fact come
// from there: the operation name, persisted-query version and hash and the two
// variable names; and the PER-FIELD WIRE ENCODING below — that the nested
// milestone node's value is string-encoded while watchStreakThreshold and
// watchStreakCopoBonus are number-encoded. No donor code text was copied; the
// donor identifiers named in comments are evidence attribution, not expression.
// Live acceptance of that hash is PENDING runtime evidence; an
// UNSUPPORTED_QUERY outcome is an honest, expected result, never a reason to
// invent a replacement hash.

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/gql"
)

// MilestoneFieldPresence is the parser's quality/presence classification for
// one observed field or container.
//
// The five values are deliberately NOT collapsible. A field that Twitch did
// not send (MISSING), a field it sent as JSON null (NULL), a container it sent
// empty (EMPTY), a well-typed value (VALID) and a value of the wrong shape
// (MALFORMED) are five different observations. Nothing here coerces MISSING,
// NULL or MALFORMED into a zero, an empty slice or a false — a value that was
// not observed must stay unobserved, not become a fabricated datum.
type MilestoneFieldPresence string

const (
	// MilestoneFieldMissing: the key was absent from its parent object (or the
	// parent object itself was absent).
	MilestoneFieldMissing MilestoneFieldPresence = "MISSING"
	// MilestoneFieldNull: the key was present and JSON null.
	MilestoneFieldNull MilestoneFieldPresence = "NULL"
	// MilestoneFieldValid: the key was present with the expected shape.
	MilestoneFieldValid MilestoneFieldPresence = "VALID"
	// MilestoneFieldEmpty: a present, well-typed container with zero elements.
	// Distinct from MISSING and NULL: "Twitch says there are none" is evidence,
	// "Twitch said nothing" is not.
	MilestoneFieldEmpty MilestoneFieldPresence = "EMPTY"
	// MilestoneFieldMalformed: the key was present but not the expected shape
	// (wrong JSON type, or a number that is not an exact integer).
	MilestoneFieldMalformed MilestoneFieldPresence = "MALFORMED"
)

// MilestoneStringField is one observed string-valued field plus its presence
// classification. Value is meaningful only when Presence is VALID; an empty
// Value with a VALID presence is a genuinely observed empty string.
type MilestoneStringField struct {
	Presence MilestoneFieldPresence
	Value    string
}

// MilestoneWireKind records which JSON encoding a validly observed integer
// actually arrived in.
//
// It exists because RewardList does not use one encoding for its integers:
// watchStreakThreshold and watchStreakCopoBonus arrive as JSON numbers, while
// the nested milestone node's value arrives as a JSON string. Normalising both
// to 4 and stopping there would erase which form Twitch sent, and that form is
// itself protocol evidence this observation exists to collect. So the parsed
// integer and its wire kind are kept in separate slots and neither substitutes
// for the other.
type MilestoneWireKind string

const (
	// MilestoneWireKindUnset: no encoding was observed, because no integer was
	// validly read. It is the zero value on purpose: a field that is MISSING,
	// NULL or MALFORMED must not appear to have arrived in some encoding.
	MilestoneWireKindUnset MilestoneWireKind = ""
	// MilestoneWireKindNumber: the value arrived as a JSON number.
	MilestoneWireKindNumber MilestoneWireKind = "NUMBER"
	// MilestoneWireKindString: the value arrived as a JSON string holding an
	// optionally signed run of decimal digits.
	MilestoneWireKindString MilestoneWireKind = "STRING"
)

// MilestoneIntField is one observed integer-valued field plus its presence
// classification and the wire kind it arrived in. A JSON number that is not a
// finite, exactly representable integer is MALFORMED, never truncated to a
// plausible-looking integer.
//
// Value and WireKind are meaningful only when Presence is VALID; otherwise both
// stay at their zero values, so nothing reads as an observed 0 in an observed
// encoding.
type MilestoneIntField struct {
	Presence MilestoneFieldPresence
	Value    int
	WireKind MilestoneWireKind
}

// MilestoneBroadcastIdentifiers is one missedStreams entry's
// broadcastIdentifiers array.
//
// Partial data is reported as partial: IDs holds the identifiers that were
// well-formed and Elements carries every element's own classification, so a
// reader can never mistake a partially parsed array for a complete one and no
// element silently disappears.
//
// MalformedCount and Presence are about SHAPE only. An id that is absent or
// explicitly null is a null/missing observation, not a shape error, so it does
// not make the array MALFORMED — it appears in Elements as MISSING or NULL.
// Presence goes MALFORMED only when an element is genuinely the wrong shape (a
// non-object element, or an id of the wrong JSON type). Collapsing the two
// would lose the same NULL/MALFORMED distinction this parser preserves
// everywhere else.
//
// Elements carries the PER-ELEMENT observation, in wire order. It exists
// because a single MalformedCount would collapse several different
// observations — an absent id key, an explicit id: null, a wrong-typed id, a
// null element, and an element that is not an object at all — into one number,
// destroying exactly the MISSING != NULL != VALID != MALFORMED distinction
// this parser exists to preserve.
type MilestoneBroadcastIdentifiers struct {
	Presence       MilestoneFieldPresence
	Count          int
	MalformedCount int
	IDs            []string
	Elements       []MilestoneBroadcastIdentifier
}

// MilestoneBroadcastIdentifier is one observed broadcastIdentifiers element.
//
// Presence classifies the ELEMENT itself; ID classifies the element's id node.
// They are separate for the same reason MilestoneMissedStream separates its own
// presence from its child array: a null ELEMENT (which has no id node at all)
// and a well-formed element whose id is explicitly null are different wire
// facts, and sharing one slot would make them indistinguishable.
//
// When Presence is not VALID there was no element object to look inside, so ID
// is left at its zero value rather than being given a fabricated observation.
type MilestoneBroadcastIdentifier struct {
	Presence MilestoneFieldPresence
	ID       MilestoneStringField
}

// MilestoneMissedStream is one observed missedStreams element.
//
// Presence is the ELEMENT's own classification, and it is deliberately separate
// from BroadcastIdentifiers.Presence, which classifies the element's CHILD
// array. Without it the two nodes collapse: a null element and a well-formed
// element whose broadcastIdentifiers is explicitly null are different wire
// facts, and writing the element's NULL into its child's slot would make them
// indistinguishable — the same MISSING != NULL != EMPTY != VALID != MALFORMED
// collapse this parser exists to prevent, one node up.
//
// When Presence is not VALID there was no element object to look inside, so
// BroadcastIdentifiers is left at its zero value and its Presence renders as
// "never reached" rather than as an observed classification. Nothing here
// fabricates a child observation for a parent that does not exist.
type MilestoneMissedStream struct {
	Presence             MilestoneFieldPresence
	BroadcastIdentifiers MilestoneBroadcastIdentifiers
}

// MilestoneMissedStreams is the missedStreams container. Count is the number of
// elements Twitch sent.
//
// MalformedCount is about SHAPE only: it counts elements that were present,
// non-null and NOT objects. An explicitly null element is a null observation
// rather than a shape error, so it is recorded as Presence NULL on its own
// entry and is NOT counted here — the same rule
// MilestoneBroadcastIdentifiers applies to its own elements. Container
// Presence follows MalformedCount: a list whose only irregularity is a null
// element is still a VALID list that contains a null.
type MilestoneMissedStreams struct {
	Presence       MilestoneFieldPresence
	Count          int
	MalformedCount int
	Entries        []MilestoneMissedStream
}

// WatchStreakMilestoneSnapshot is the parsed content of one RewardList
// response, field for field, with a presence classification for every node on
// the path. It is a pure description of what arrived; it holds no derived
// state, no interpretation and no verdict.
//
// Node naming follows the wire exactly:
//
//	data.channel                                  -> ChannelPresence
//	data.channel.id                               -> ChannelID
//	data.channel.self                             -> SelfPresence
//	data.channel.self.watchStreakMilestone        -> SelfMilestonePresence  (S)
//	S.watchStreakMilestone                        -> MilestoneNodePresence  (V)
type WatchStreakMilestoneSnapshot struct {
	DataPresence    MilestoneFieldPresence
	ChannelPresence MilestoneFieldPresence
	ChannelID       MilestoneStringField
	SelfPresence    MilestoneFieldPresence

	// S = data.channel.self.watchStreakMilestone
	SelfMilestonePresence MilestoneFieldPresence
	WatchStreakThreshold  MilestoneIntField
	WatchStreakCopoBonus  MilestoneIntField
	State                 MilestoneStringField
	ExpiresAt             MilestoneStringField
	MissedStreams         MilestoneMissedStreams

	// V = S.watchStreakMilestone
	MilestoneNodePresence MilestoneFieldPresence
	MilestoneID           MilestoneStringField
	MilestoneValue        MilestoneIntField
	AchievementTimestamp  MilestoneStringField
	ShareStatus           MilestoneStringField
}

// WatchStreakMilestoneOutcome is the bounded outcome vocabulary of one
// observation attempt. Every non-OBSERVED value is UNKNOWN/UNAVAILABLE
// evidence: it says the observation did not happen, never that the milestone
// is absent, zero, or unchanged.
type WatchStreakMilestoneOutcome string

const (
	// MilestoneObserved: a response was received and parsed. The snapshot's own
	// presence fields say how much of it was actually there.
	MilestoneObserved WatchStreakMilestoneOutcome = "OBSERVED"
	// MilestoneGraphQLError: HTTP succeeded but Twitch returned a top-level
	// GraphQL errors array, so there is no authoritative data. Nothing is
	// parsed from such a response.
	MilestoneGraphQLError WatchStreakMilestoneOutcome = "GRAPHQL_ERROR"
	// MilestoneUnsupported: every candidate client ID answered
	// PersistedQueryNotFound. The shipped hash is not accepted right now. This
	// is the outcome that would falsify the RewardList protocol premise; it is
	// reported, never worked around.
	MilestoneUnsupported WatchStreakMilestoneOutcome = "UNSUPPORTED_QUERY"
	// MilestoneUnavailable: the request failed at the transport/auth layer.
	MilestoneUnavailable WatchStreakMilestoneOutcome = "UNAVAILABLE"
	// MilestoneCancelled: the owning context was cancelled; the request was
	// released rather than completed.
	MilestoneCancelled WatchStreakMilestoneOutcome = "CANCELLED"
	// MilestoneSkipped: no request was made at all (no channel identity, or the
	// context was already done before the attempt).
	MilestoneSkipped WatchStreakMilestoneOutcome = "SKIPPED"
)

// MilestoneFailureClass is a bounded, allowlisted failure label. It exists so a
// failure can be reported WITHOUT logging an arbitrary raw error string, which
// could carry a URL, a header echo, a token fragment or any other unvetted
// payload text.
type MilestoneFailureClass string

const (
	MilestoneFailureNone      MilestoneFailureClass = ""
	MilestoneFailureCancelled MilestoneFailureClass = "CANCELLED"
	MilestoneFailureDeadline  MilestoneFailureClass = "DEADLINE_EXCEEDED"
	// MilestoneFailureTransportTimeout: the HTTP client's OWN timeout expired
	// while the owner's context was still healthy. net/http reports that as an
	// error satisfying errors.Is(err, context.DeadlineExceeded), so without
	// consulting the owner it is indistinguishable from an operator shutdown —
	// and a stalled Twitch would be recorded as "we cancelled", which is the
	// opposite of what happened.
	MilestoneFailureTransportTimeout MilestoneFailureClass = "TRANSPORT_TIMEOUT"
	MilestoneFailureQueryNotFound    MilestoneFailureClass = "PERSISTED_QUERY_NOT_FOUND"
	MilestoneFailureUnauthorized     MilestoneFailureClass = "UNAUTHORIZED"
	MilestoneFailureTransport        MilestoneFailureClass = "TRANSPORT"
	MilestoneFailureGraphQLTopLevel  MilestoneFailureClass = "GRAPHQL_TOP_LEVEL_ERRORS"
	MilestoneFailureNoChannelID      MilestoneFailureClass = "NO_CHANNEL_ID"
	MilestoneFailureContextDone      MilestoneFailureClass = "CONTEXT_ALREADY_DONE"
	// MilestoneFailureNoDataNode: the response carried no top-level GraphQL
	// errors AND no data object. A GraphQL data response always has one, so
	// this is an edge/proxy rejection body (a 4xx or a gateway page with a JSON
	// payload) that the shared transport returns verbatim for statuses it does
	// not special-case. Reporting it as OBSERVED would record "Twitch answered
	// and there is no milestone" when the request was in fact rejected.
	MilestoneFailureNoDataNode MilestoneFailureClass = "NO_DATA_NODE"
	// MilestoneFailureMalformedErrors: an `errors` node was PRESENT but was not
	// an array. A GraphQL errors node is only ever valid as a list, so every
	// other shape is a rejection wearing the wrong clothes. gql.HasTopLevelErrors
	// answers "is there a non-empty array", which is false for an object, a
	// string, a number or null - so without this the rejection would be walked
	// past and its accompanying data recorded as an observed milestone.
	MilestoneFailureMalformedErrors MilestoneFailureClass = "MALFORMED_ERRORS_NODE"
	// MilestoneFailureHTTPStatus: the response did not carry a 2xx status. The
	// shared read transport special-cases only 401 and 403 and hands back any
	// other non-2xx JSON body verbatim, so a 400, a 404 or a redirect page whose
	// payload happens to contain a data object would otherwise satisfy the
	// data-presence check and be recorded as evidence.
	MilestoneFailureHTTPStatus MilestoneFailureClass = "HTTP_STATUS"

	// MilestoneFailureAmbiguousJSON marks a response whose raw JSON carried
	// duplicate object members. Decoding picks the last one, so an explicit
	// rejection can be erased by a benign duplicate beside it; the two readings
	// are not distinguishable after the fact, and neither is evidence.
	MilestoneFailureAmbiguousJSON MilestoneFailureClass = "AMBIGUOUS_JSON"
)

// milestoneObservationSequence is the process-wide monotonic counter stamped on
// every observation AT REQUEST START.
//
// It is the only ordering authority for these records. Completion order is NOT
// evidence of recency: an older request may finish after a newer one (slow
// response, retry backoff, scheduler), and log-emission order follows
// completion. Because the sequence is assigned before the request is sent, a
// late-completing older observation cannot be presented as newer evidence: it
// carries the lower sequence whatever order the records were written in, and
// nothing in a record claims to be the latest.
//
// It is a counter, not a cache: it retains no milestone value, no channel and
// no history.
var milestoneObservationSequence atomic.Uint64

// WatchStreakMilestoneObservation is one complete diagnostic record: what was
// asked, when the request started and ended, what came back, and how well
// formed it was.
//
// Two observations that carry identical milestone values at different times are
// DISTINCT observations. Nothing here deduplicates them — the temporal evidence
// (Sequence, RequestStart, RequestEnd) is the point of the record.
type WatchStreakMilestoneObservation struct {
	// Sequence is stamped at request start; see milestoneObservationSequence.
	Sequence uint64

	// RequestedChannelID is the channelID variable actually sent.
	RequestedChannelID string
	// RequestedLogin is the local roster label for the same target. It is
	// context for a human reader; it is not part of the request.
	RequestedLogin string

	// RequestStart/RequestEnd bound this observation's own sampling window.
	// Neither is the achievement time: see Snapshot.AchievementTimestamp, which
	// belongs to the observed achievement field and is kept separately for
	// exactly that reason.
	RequestStart time.Time
	RequestEnd   time.Time

	Outcome      WatchStreakMilestoneOutcome
	FailureClass MilestoneFailureClass

	Snapshot WatchStreakMilestoneSnapshot
}

// Duration is the observation's own request window. It is a sampling duration,
// never an achievement age.
func (o WatchStreakMilestoneObservation) Duration() time.Duration {
	if o.RequestEnd.IsZero() || o.RequestStart.IsZero() {
		return 0
	}
	return o.RequestEnd.Sub(o.RequestStart)
}

// ObserveWatchStreakMilestone performs at most ONE read-only RewardList request
// for channelID and returns the resulting diagnostic record.
//
// It never returns an error, by design. Every failure mode — cancelled,
// unsupported hash, transport failure, GraphQL error, partial response — is an
// observational outcome carried in the record itself. A caller therefore cannot
// accidentally turn an observation failure into a business failure: there is no
// error to propagate, no state to roll back and no decision to skip.
//
// Request amplification is exactly the shared read transport's existing
// contract and nothing more: one postGQLRequest call, whose bounded internals
// (per-operation client-ID candidates, gqlMaxRetries transient retries with
// interruptible backoff, and at most one auth-recovery replay of the identical
// body) are the same ones every other read operation in this package already
// uses. This function adds no retry, no backoff and no second request of its
// own.
//
// The request is marked DIAGNOSTIC (see diagnosticRequestKey), so its outcome
// is invisible to the shared connectivity accounting in both directions: it
// records no functional failure, refreshes no success timestamp, drives no
// credential recovery or operator reauth escalation, and raises no WARN/ERROR
// for its own outcome. That is what keeps a failed observation from degrading
// health.SignalGQLAPI and closing the auto-bet gate — and equally keeps a
// succeeding observation from masking a real business-path outage. The shared
// client-ID pool is isolated too: this read caches its own working ID but never
// promotes the process-wide default and never raises the stale-hash promotion
// WARN; diagnosticRequestKey documents why.
//
// ctx is the caller's existing loop context. Cancellation is honored before the
// request is built and, through http.NewRequestWithContext and the
// interruptible retry wait, while it is in flight.
func (c *TwitchClient) ObserveWatchStreakMilestone(ctx context.Context, channelID, login string) WatchStreakMilestoneObservation {
	obs := WatchStreakMilestoneObservation{
		Sequence:           milestoneObservationSequence.Add(1),
		RequestedChannelID: channelID,
		RequestedLogin:     login,
	}

	// No channel identity means there is no request to make. Report it as a
	// skipped observation rather than sending a request that cannot be scoped.
	if channelID == "" {
		now := time.Now()
		obs.RequestStart, obs.RequestEnd = now, now
		obs.Outcome, obs.FailureClass = MilestoneSkipped, MilestoneFailureNoChannelID
		return obs
	}
	if err := ctx.Err(); err != nil {
		now := time.Now()
		obs.RequestStart, obs.RequestEnd = now, now
		obs.Outcome, obs.FailureClass = MilestoneSkipped, MilestoneFailureContextDone
		return obs
	}

	op := constants.RewardList.WithVariables(map[string]interface{}{
		"channelID":                        channelID,
		"shouldIncludeAllSuspendedStreaks": false,
	})

	// withDiagnosticRequest is what makes this observation genuinely
	// side-effect free. Without it the shared read transport would fold this
	// request's outcome into the process-wide connectivity accounting, and a
	// RewardList hash Twitch does not accept — the outcome that is EXPECTED
	// here until live acceptance is separately evidenced — would drive
	// ConnHealth.RecentFunctionalFailures past the degrade threshold on the
	// second target, pin health.SignalGQLAPI at DEGRADED, and block every
	// automated prediction bet for as long as the miner runs. Cancellation and
	// deadlines propagate through the wrapper unchanged.
	obs.RequestStart = time.Now()
	resp, statusCode, err := c.postGQLRequestWithStatus(withDiagnosticRequest(ctx), op)
	obs.RequestEnd = time.Now()

	if err != nil {
		obs.Outcome, obs.FailureClass = classifyMilestoneRequestError(ctx, err)
		// Refine ONLY the generic fallback. Every named class above it is a
		// more specific fact than the status - a cancellation, the transport's
		// own timeout, an exhausted hash, a token rejection, ambiguous JSON -
		// and must survive untouched. What is left is the catch-all, which
		// answers "transport" for failures that were in truth an HTTP refusal:
		// a 4xx whose body did not parse reached the record as TRANSPORT even
		// though the status alone had already settled it.
		//
		// Transient statuses keep TRANSPORT deliberately. A 429 or a 5xx here
		// means the retry schedule ran and gave up, and that is the more
		// useful fact than the last status seen.
		if obs.FailureClass == MilestoneFailureTransport &&
			statusCode != 0 && (statusCode < 200 || statusCode > 299) &&
			!gql.IsTransientStatus(statusCode) {
			// 401 is settled by the status alone and needs no body at all, so
			// it keeps its own, more specific class whatever became of the
			// body. Without this, a rejection whose body merely happened to be
			// oversized would fail the read before the transport reached its
			// 401 branch, land on the generic fallback, and be refined to
			// HTTP_STATUS - letting an attacker-chosen body SIZE decide which
			// failure this observation records.
			if statusCode == http.StatusUnauthorized {
				obs.FailureClass = MilestoneFailureUnauthorized
			} else {
				obs.FailureClass = MilestoneFailureHTTPStatus
			}
		}
		return obs
	}

	// Success is judged on the STATUS, not on the shape of what came back. The
	// shared transport special-cases only 401 and 403 and returns every other
	// non-2xx JSON body verbatim, so a 400, a 404 or a redirect page carrying a
	// data object would otherwise pass the data-presence check below and be
	// recorded as an observed milestone. A refused request is not evidence.
	if statusCode < 200 || statusCode > 299 {
		obs.Outcome, obs.FailureClass = MilestoneUnavailable, MilestoneFailureHTTPStatus
		return obs
	}

	// A top-level GraphQL errors node means Twitch returned no authoritative
	// data, even at HTTP 200 and even alongside a partially populated data
	// node. Stop before parsing: a service-layer error must never be presented
	// as an observed milestone.
	//
	// Shape is checked here rather than deferring to gql.HasTopLevelErrors,
	// which asks only "is there a non-empty ARRAY". A present errors node that
	// is an object, a string, a number or null answers false there and would be
	// walked past; a GraphQL errors node is valid only as a list, so anything
	// else is a rejection this read must fail closed on.
	if raw, present := resp["errors"]; present {
		errs, isArray := raw.([]interface{})
		switch {
		case !isArray:
			obs.Outcome, obs.FailureClass = MilestoneUnavailable, MilestoneFailureMalformedErrors
			return obs
		case len(errs) > 0:
			obs.Outcome, obs.FailureClass = MilestoneGraphQLError, MilestoneFailureGraphQLTopLevel
			return obs
		}
		// Present, an array, and empty: that reports no error, so the data
		// alongside it is still a real observation.
	}

	// A GraphQL data response always carries a data object, so its absence here
	// means this was not one — record it as UNAVAILABLE rather than as an
	// observation in which every field happened to be missing.
	snap := parseWatchStreakMilestone(resp)
	if snap.DataPresence != MilestoneFieldValid {
		obs.Outcome, obs.FailureClass = MilestoneUnavailable, MilestoneFailureNoDataNode
		return obs
	}

	obs.Outcome = MilestoneObserved
	obs.Snapshot = snap
	return obs
}

// classifyMilestoneRequestError maps a transport error to the bounded outcome
// and failure-class vocabulary. The error's own text is never retained: only
// the class survives, so no unvetted string can reach a log record.
//
// ctx is consulted, not just err, because net/http's Client.Timeout produces an
// error that satisfies errors.Is(err, context.DeadlineExceeded) even when the
// owner's context is perfectly healthy. Classifying on the error alone would
// report a stalled Twitch as CANCELLED — an outcome whose own definition is
// "the owning context was cancelled" — and quietly turn a remote fault into an
// apparent local shutdown.
func classifyMilestoneRequestError(ctx context.Context, err error) (WatchStreakMilestoneOutcome, MilestoneFailureClass) {
	ownerDone := ctx != nil && ctx.Err() != nil
	switch {
	case errors.Is(err, context.Canceled) && ownerDone:
		return MilestoneCancelled, MilestoneFailureCancelled
	case errors.Is(err, context.DeadlineExceeded) && ownerDone:
		return MilestoneCancelled, MilestoneFailureDeadline
	case errors.Is(err, context.DeadlineExceeded):
		// The owner is alive: this is the HTTP client's own timeout.
		return MilestoneUnavailable, MilestoneFailureTransportTimeout
	case errors.Is(err, context.Canceled):
		// Cancellation with a live owner: an inner scope gave up, which is a
		// transport fault from this observation's point of view.
		return MilestoneUnavailable, MilestoneFailureTransport
	case errors.Is(err, ErrPersistedQueryNotFound):
		return MilestoneUnsupported, MilestoneFailureQueryNotFound
	case errors.Is(err, ErrUnauthorized):
		return MilestoneUnavailable, MilestoneFailureUnauthorized
	case errors.Is(err, errAmbiguousDiagnosticJSON):
		return MilestoneUnavailable, MilestoneFailureAmbiguousJSON
	default:
		return MilestoneUnavailable, MilestoneFailureTransport
	}
}

// parseWatchStreakMilestone reads a decoded RewardList response into a
// snapshot. It is pure: no I/O, no state, no locks, no error return — an
// unparseable node becomes a presence classification, never a failure and
// never a fabricated value.
//
// Only the audited fields are read. Anything else Twitch sends is ignored
// rather than speculatively captured.
func parseWatchStreakMilestone(resp map[string]interface{}) WatchStreakMilestoneSnapshot {
	var snap WatchStreakMilestoneSnapshot

	data, dataPresence := milestoneObject(resp, "data")
	snap.DataPresence = dataPresence

	channel, channelPresence := milestoneObject(data, "channel")
	snap.ChannelPresence = channelPresence
	snap.ChannelID = milestoneString(channel, "id")

	self, selfPresence := milestoneObject(channel, "self")
	snap.SelfPresence = selfPresence

	// S = data.channel.self.watchStreakMilestone
	s, sPresence := milestoneObject(self, "watchStreakMilestone")
	snap.SelfMilestonePresence = sPresence
	snap.WatchStreakThreshold = milestoneInt(s, "watchStreakThreshold")
	snap.WatchStreakCopoBonus = milestoneInt(s, "watchStreakCopoBonus")
	snap.State = milestoneString(s, "state")
	snap.ExpiresAt = milestoneString(s, "expiresAt")
	snap.MissedStreams = parseMilestoneMissedStreams(s)

	// V = S.watchStreakMilestone (the nested achievement node)
	v, vPresence := milestoneObject(s, "watchStreakMilestone")
	snap.MilestoneNodePresence = vPresence
	snap.MilestoneID = milestoneString(v, "id")
	snap.MilestoneValue = milestoneNumericStringOrInt(v, "value")
	snap.AchievementTimestamp = milestoneString(v, "achievementTimestamp")
	snap.ShareStatus = milestoneString(v, "shareStatus")

	return snap
}

// parseMilestoneMissedStreams reads S.missedStreams, preserving the difference
// between a missing key, an explicit null, an empty array and a populated one,
// and reporting malformed elements as malformed instead of dropping them.
func parseMilestoneMissedStreams(parent map[string]interface{}) MilestoneMissedStreams {
	raw, present := parent["missedStreams"]
	switch {
	case !present:
		return MilestoneMissedStreams{Presence: MilestoneFieldMissing}
	case raw == nil:
		return MilestoneMissedStreams{Presence: MilestoneFieldNull}
	}
	list, ok := raw.([]interface{})
	if !ok {
		return MilestoneMissedStreams{Presence: MilestoneFieldMalformed}
	}
	if list == nil {
		return MilestoneMissedStreams{Presence: MilestoneFieldNull}
	}
	out := MilestoneMissedStreams{Count: len(list)}
	if len(list) == 0 {
		out.Presence = MilestoneFieldEmpty
		return out
	}
	out.Entries = make([]MilestoneMissedStream, 0, len(list))
	for _, element := range list {
		if element == nil {
			// An explicitly null ELEMENT is a null, not a shape error — the
			// same rule parseMilestoneBroadcastIdentifiers applies one level
			// down. The NULL belongs to the ELEMENT: there is no element object
			// here, so its child array was never observed and is left unset
			// rather than being given a fabricated NULL of its own.
			out.Entries = append(out.Entries, MilestoneMissedStream{Presence: MilestoneFieldNull})
			continue
		}
		entry, ok := element.(map[string]interface{})
		if !ok || entry == nil {
			// Present, non-null and the wrong shape: a genuine shape error, and
			// again no child to look inside.
			out.MalformedCount++
			out.Entries = append(out.Entries, MilestoneMissedStream{Presence: MilestoneFieldMalformed})
			continue
		}
		out.Entries = append(out.Entries, MilestoneMissedStream{
			Presence:             MilestoneFieldValid,
			BroadcastIdentifiers: parseMilestoneBroadcastIdentifiers(entry),
		})
	}
	if out.MalformedCount > 0 {
		out.Presence = MilestoneFieldMalformed
	} else {
		out.Presence = MilestoneFieldValid
	}
	return out
}

// parseMilestoneBroadcastIdentifiers reads one missedStreams entry's
// broadcastIdentifiers array, collecting the well-formed ids and counting the
// malformed elements separately.
func parseMilestoneBroadcastIdentifiers(entry map[string]interface{}) MilestoneBroadcastIdentifiers {
	raw, present := entry["broadcastIdentifiers"]
	switch {
	case !present:
		return MilestoneBroadcastIdentifiers{Presence: MilestoneFieldMissing}
	case raw == nil:
		return MilestoneBroadcastIdentifiers{Presence: MilestoneFieldNull}
	}
	list, ok := raw.([]interface{})
	if !ok {
		return MilestoneBroadcastIdentifiers{Presence: MilestoneFieldMalformed}
	}
	if list == nil {
		return MilestoneBroadcastIdentifiers{Presence: MilestoneFieldNull}
	}
	out := MilestoneBroadcastIdentifiers{Count: len(list)}
	if len(list) == 0 {
		out.Presence = MilestoneFieldEmpty
		return out
	}
	out.Elements = make([]MilestoneBroadcastIdentifier, 0, len(list))
	for _, element := range list {
		if element == nil {
			// An explicitly null ELEMENT is a null, not a shape error, and it
			// has no id node — so the NULL belongs to the ELEMENT and its ID is
			// left unset rather than given a fabricated NULL of its own.
			out.Elements = append(out.Elements, MilestoneBroadcastIdentifier{Presence: MilestoneFieldNull})
			continue
		}
		identifier, ok := element.(map[string]interface{})
		if !ok || identifier == nil {
			// Present, non-null and the wrong shape: a genuine shape error, and
			// again no id node to look inside.
			out.Elements = append(out.Elements, MilestoneBroadcastIdentifier{Presence: MilestoneFieldMalformed})
			out.MalformedCount++
			continue
		}
		id := milestoneString(identifier, "id")
		out.Elements = append(out.Elements, MilestoneBroadcastIdentifier{
			Presence: MilestoneFieldValid,
			ID:       id,
		})
		switch id.Presence {
		case MilestoneFieldValid:
			out.IDs = append(out.IDs, id.Value)
		case MilestoneFieldMalformed:
			out.MalformedCount++
		}
		// MISSING and NULL ids yield no identifier and no shape error; they are
		// carried by Elements alone.
	}
	if out.MalformedCount > 0 {
		out.Presence = MilestoneFieldMalformed
	} else {
		out.Presence = MilestoneFieldValid
	}
	return out
}

// milestoneObject classifies and returns a nested object node. A nil parent
// yields MISSING (an absent parent cannot make its child present), a JSON null
// yields NULL, a non-object yields MALFORMED, and only a real object yields
// VALID.
func milestoneObject(parent map[string]interface{}, key string) (map[string]interface{}, MilestoneFieldPresence) {
	raw, present := parent[key]
	switch {
	case !present:
		return nil, MilestoneFieldMissing
	case raw == nil:
		return nil, MilestoneFieldNull
	}
	obj, ok := raw.(map[string]interface{})
	if !ok {
		return nil, MilestoneFieldMalformed
	}
	if obj == nil {
		return nil, MilestoneFieldNull
	}
	return obj, MilestoneFieldValid
}

// milestoneString classifies one string-valued field. A present-but-empty
// string is VALID with an empty value — genuinely observed, not missing.
func milestoneString(parent map[string]interface{}, key string) MilestoneStringField {
	raw, present := parent[key]
	switch {
	case !present:
		return MilestoneStringField{Presence: MilestoneFieldMissing}
	case raw == nil:
		return MilestoneStringField{Presence: MilestoneFieldNull}
	}
	value, ok := raw.(string)
	if !ok {
		return MilestoneStringField{Presence: MilestoneFieldMalformed}
	}
	return MilestoneStringField{Presence: MilestoneFieldValid, Value: value}
}

// milestoneInt classifies one integer-valued field that Twitch sends as a JSON
// number. A string is the wrong shape here and is MALFORMED; only the nested
// milestone value node is known to arrive string-encoded, and it is read by
// milestoneNumericStringOrInt instead.
func milestoneInt(parent map[string]interface{}, key string) MilestoneIntField {
	raw, present := parent[key]
	switch {
	case !present:
		return MilestoneIntField{Presence: MilestoneFieldMissing}
	case raw == nil:
		return MilestoneIntField{Presence: MilestoneFieldNull}
	}
	number, ok := raw.(float64)
	if !ok {
		return MilestoneIntField{Presence: MilestoneFieldMalformed}
	}
	return milestoneIntFromNumber(number)
}

// milestoneNumericStringOrInt classifies one integer-valued field that Twitch
// sends in EITHER JSON encoding, recording which one arrived.
//
// This is protocol tolerance, not interpretation. RewardList's milestone value
// node is string-encoded on the wire, so reading only the number form would
// discard the single field this observation exists to see and report the loss
// as if Twitch had sent something malformed. Accepting the string form assigns
// it no meaning: it does not make the value a streak count, a rung, or a link
// to any grant. Which field carries the authoritative streak count stays
// UNKNOWN.
//
// The accepted string form is exactly what strconv.Atoi accepts and no more: an
// optional sign then decimal digits (so "+4" and "0004" are read as 4). Anything
// else is MALFORMED, never coerced — a bool, an object, an array, "4.0", "0x4",
// " 4", "", and a digit run too large for a platform int.
//
// Normalisation keeps the integer and the encoding, not the lexical form: "4",
// "+4" and "0004" all report 4/STRING, so a purely lexical change on the wire is
// not visible in the record. That is the contracted semantics, and it is the
// one thing the wire kind does not tell a reader.
func milestoneNumericStringOrInt(parent map[string]interface{}, key string) MilestoneIntField {
	raw, present := parent[key]
	switch {
	case !present:
		return MilestoneIntField{Presence: MilestoneFieldMissing}
	case raw == nil:
		return MilestoneIntField{Presence: MilestoneFieldNull}
	}
	switch typed := raw.(type) {
	case float64:
		return milestoneIntFromNumber(typed)
	case string:
		// The err check is load-bearing and must not be dropped: on a digit run
		// too large for the platform int, strconv.Atoi returns the SATURATED
		// magnitude (MaxInt/MinInt) TOGETHER WITH ErrRange — it does not leave
		// the value alone. Using it would record MaxInt as an observed
		// milestone, which is precisely the fabricated datum this parser
		// exists to prevent. Out of range is MALFORMED.
		//
		// Within that bound a decimal string needs no 2^53 guard, because it
		// carries its integer exactly: the guard on the number path exists only
		// because JSON numbers decode through float64 and lose identity above
		// 2^53, which never happens to a string. So on a 64-bit build the two
		// encodings deliberately disagree between 2^53 and MaxInt — the string
		// is still exact there while the number is not. On a 32-bit build the
		// platform int bound bites first and both refuse near 2^31, so the
		// disagreement is a property of the wider int, not a universal one.
		value, err := strconv.Atoi(typed)
		if err != nil {
			return MilestoneIntField{Presence: MilestoneFieldMalformed}
		}
		return MilestoneIntField{
			Presence: MilestoneFieldValid,
			Value:    value,
			WireKind: MilestoneWireKindString,
		}
	default:
		return MilestoneIntField{Presence: MilestoneFieldMalformed}
	}
}

// milestoneIntFromNumber applies the exact-integer rules to a decoded JSON
// number. A number that is not finite, not integral, or not exactly
// representable as an int is MALFORMED — it is never truncated, rounded or
// clamped into a value that would read as an observed fact.
func milestoneIntFromNumber(number float64) MilestoneIntField {
	if math.IsNaN(number) || math.IsInf(number, 0) || number != math.Trunc(number) {
		return MilestoneIntField{Presence: MilestoneFieldMalformed}
	}
	// Below 2^53 every integer has exactly one float64 representation; 2^53
	// itself is refused because a wire value of 2^53+1 has already rounded to
	// it during JSON decoding and cannot be told apart. The platform int bound
	// guards 32-bit builds. Same rule as the exact points ledger.
	const exactLimit = float64(1 << 53)
	if number >= exactLimit || number <= -exactLimit ||
		number > float64(math.MaxInt) || number < float64(math.MinInt) {
		return MilestoneIntField{Presence: MilestoneFieldMalformed}
	}
	return MilestoneIntField{
		Presence: MilestoneFieldValid,
		Value:    int(number),
		WireKind: MilestoneWireKindNumber,
	}
}
