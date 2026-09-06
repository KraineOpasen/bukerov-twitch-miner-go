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
// That restraint is now the ONLY thing left unknown here, and the distinction
// is sharp. Twitch's Viewer Channel Point Guide documents the reward ladder
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
// Provenance: the RewardList wire facts (operation name, persisted-query
// version and hash, and the two variable names) are clean-room protocol
// evidence from mpforce1/Twitch-Channel-Points-Miner, ref
// f1dda17ad61562ca2e93d975ee0a24e8b2f7ea0c, tree
// 650ddd7ed974f78fab1956e41c0849ea177545b2, GPL-3.0. No donor code text was
// copied. Live acceptance of that hash is PENDING runtime evidence; an
// UNSUPPORTED_QUERY outcome is an honest, expected result, never a reason to
// invent a replacement hash.

import (
	"context"
	"errors"
	"math"
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

// MilestoneIntField is one observed integer-valued field plus its presence
// classification. A JSON number that is not a finite, exactly representable
// integer is MALFORMED, never truncated to a plausible-looking integer.
type MilestoneIntField struct {
	Presence MilestoneFieldPresence
	Value    int
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
// Elements carries the PER-ELEMENT classification of each element's id, in wire
// order. It exists because a single MalformedCount would collapse four
// different observations — an absent id key, an explicit id: null, a
// wrong-typed id, and an element that is not an object at all — into one
// number, destroying exactly the MISSING != NULL != VALID != MALFORMED
// distinction this parser exists to preserve. An element that is not an object
// contributes a MALFORMED entry, since its id cannot be classified at all.
type MilestoneBroadcastIdentifiers struct {
	Presence       MilestoneFieldPresence
	Count          int
	MalformedCount int
	IDs            []string
	Elements       []MilestoneStringField
}

// MilestoneMissedStream is one observed missedStreams element.
type MilestoneMissedStream struct {
	BroadcastIdentifiers MilestoneBroadcastIdentifiers
}

// MilestoneMissedStreams is the missedStreams container. Count is the number of
// elements Twitch sent; MalformedCount is how many of them were not objects.
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
	MilestoneFailureNone            MilestoneFailureClass = ""
	MilestoneFailureCancelled       MilestoneFailureClass = "CANCELLED"
	MilestoneFailureDeadline        MilestoneFailureClass = "DEADLINE_EXCEEDED"
	MilestoneFailureQueryNotFound   MilestoneFailureClass = "PERSISTED_QUERY_NOT_FOUND"
	MilestoneFailureUnauthorized    MilestoneFailureClass = "UNAUTHORIZED"
	MilestoneFailureTransport       MilestoneFailureClass = "TRANSPORT"
	MilestoneFailureGraphQLTopLevel MilestoneFailureClass = "GRAPHQL_TOP_LEVEL_ERRORS"
	MilestoneFailureNoChannelID     MilestoneFailureClass = "NO_CHANNEL_ID"
	MilestoneFailureContextDone     MilestoneFailureClass = "CONTEXT_ALREADY_DONE"
	// MilestoneFailureNoDataNode: the response carried no top-level GraphQL
	// errors AND no data object. A GraphQL data response always has one, so
	// this is an edge/proxy rejection body (a 4xx or a gateway page with a JSON
	// payload) that the shared transport returns verbatim for statuses it does
	// not special-case. Reporting it as OBSERVED would record "Twitch answered
	// and there is no milestone" when the request was in fact rejected.
	MilestoneFailureNoDataNode MilestoneFailureClass = "NO_DATA_NODE"
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
// succeeding observation from masking a real business-path outage. The one
// shared-transport line that is NOT suppressed is the client-ID promotion WARN;
// diagnosticRequestKey documents why.
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
	resp, err := c.postGQLRequest(withDiagnosticRequest(ctx), op)
	obs.RequestEnd = time.Now()

	if err != nil {
		obs.Outcome, obs.FailureClass = classifyMilestoneRequestError(err)
		return obs
	}

	// A top-level GraphQL errors array means Twitch returned no authoritative
	// data, even at HTTP 200 and even alongside a partially populated data
	// node. Stop before parsing: a service-layer error must never be presented
	// as an observed milestone.
	if gql.HasTopLevelErrors(resp) {
		obs.Outcome, obs.FailureClass = MilestoneGraphQLError, MilestoneFailureGraphQLTopLevel
		return obs
	}

	// Only 401 and 403 are special-cased by the shared transport; any other
	// non-2xx body that happens to be JSON is returned verbatim as a result. A
	// GraphQL data response always carries a data object, so its absence here
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
func classifyMilestoneRequestError(err error) (WatchStreakMilestoneOutcome, MilestoneFailureClass) {
	switch {
	case errors.Is(err, context.Canceled):
		return MilestoneCancelled, MilestoneFailureCancelled
	case errors.Is(err, context.DeadlineExceeded):
		return MilestoneCancelled, MilestoneFailureDeadline
	case errors.Is(err, ErrPersistedQueryNotFound):
		return MilestoneUnsupported, MilestoneFailureQueryNotFound
	case errors.Is(err, ErrUnauthorized):
		return MilestoneUnavailable, MilestoneFailureUnauthorized
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
	snap.MilestoneValue = milestoneInt(v, "value")
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
		entry, ok := element.(map[string]interface{})
		if !ok || entry == nil {
			out.MalformedCount++
			out.Entries = append(out.Entries, MilestoneMissedStream{
				BroadcastIdentifiers: MilestoneBroadcastIdentifiers{Presence: MilestoneFieldMalformed},
			})
			continue
		}
		out.Entries = append(out.Entries, MilestoneMissedStream{
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
	out.Elements = make([]MilestoneStringField, 0, len(list))
	for _, element := range list {
		if element == nil {
			// An explicitly null ELEMENT is a null, not a shape error.
			out.Elements = append(out.Elements, MilestoneStringField{Presence: MilestoneFieldNull})
			continue
		}
		identifier, ok := element.(map[string]interface{})
		if !ok || identifier == nil {
			// The element is present, non-null and not an object, so its id
			// cannot be classified as missing or null — only as malformed.
			out.Elements = append(out.Elements, MilestoneStringField{Presence: MilestoneFieldMalformed})
			out.MalformedCount++
			continue
		}
		id := milestoneString(identifier, "id")
		out.Elements = append(out.Elements, id)
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

// milestoneInt classifies one integer-valued field. A JSON number that is not
// finite, not integral, or not exactly representable as an int is MALFORMED —
// it is never truncated, rounded or clamped into a value that would read as an
// observed fact.
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
	return MilestoneIntField{Presence: MilestoneFieldValid, Value: int(number)}
}
