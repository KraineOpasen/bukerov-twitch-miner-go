package p4offline

import (
	"errors"
	"sort"
	"strconv"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
)

// SEAMS 1–3: raw evidence, session, episode and common-boundary validation;
// first automated opportunity selection WITHOUT later substitution; globally
// reconciled source-round identity.
//
// The input is a [predictioneval.SourceDataset] — persisted P1 facts a reader
// already acquired and verified, projected as pure values. This package never
// reads a store. Seam 1 admits a SESSION, then an EPISODE (one admission of
// one round on one pool), then ONE opportunity per episode: the first
// automatic decision attempt the raw evidence shows. Everything it refuses is
// refused with a closed reason and never silently replaced by a more
// convenient fact.
//
// # The boundary
//
// C is the causal position at which the selected attempt's terminal envelope
// — the common factset both policies are evaluated over — became available.
// F is the earliest raw CALL_STARTED relevant to the episode, taken across
// every incarnation of the same public round and across automatic, manual
// and unattributable calls alike. The protocol requires C < F. C == F fails.
// When no F exists at all, the absence must be PROVEN by complete coverage
// (nothing in the session could have hidden a call); an unproven absence is
// not an absence. Coverage is required ALWAYS, F or no F: an unrecorded call
// could sit before a recorded one.
//
// # A selection is a report, not a credential
//
// [EvidenceSelection] and [EpisodeSelection] are exported values with JSON
// tags. Nothing in this package trusts their flags: every consumer that needs
// an episode to be selected re-runs [SelectEpisodes] over the dataset.

// Session refusal reasons. Closed vocabulary.
const (
	RefusalSessionReadingNotAsFinalized = "SESSION_READING_NOT_AS_FINALIZED"
	RefusalSessionCloseStateNotComplete = "SESSION_CLOSE_STATE_NOT_COMPLETE"
	RefusalProducerRevisionUnsupported  = "PRODUCER_REVISION_UNSUPPORTED"
	RefusalSessionCountersNegative      = "SESSION_COUNTERS_NEGATIVE"
	RefusalSessionFactsLost             = "SESSION_FACTS_LOST"
	RefusalSessionFactsIncomplete       = "SESSION_FACTS_INCOMPLETE"
	RefusalSessionWitnessesUnchecked    = "SESSION_WITNESSES_UNCHECKED"
	RefusalSessionNoWitnessesVerified   = "SESSION_NO_WITNESSES_VERIFIED"
	RefusalSessionWitnessesIncomplete   = "SESSION_WITNESSES_INCOMPLETE"
	RefusalSessionForeignFacts          = "SESSION_FOREIGN_FACTS"
	RefusalSessionAnomalyPrefix         = "SESSION_ANOMALY:"
	RefusalSessionP2Exclusion           = "SESSION_P2_EXCLUSION:"
)

// Episode exclusion reasons. Closed vocabulary.
const (
	ExclusionNoAutomaticOpportunity         = "NO_AUTOMATIC_OPPORTUNITY"
	ExclusionFirstOpportunityUnusable       = "FIRST_OPPORTUNITY_UNUSABLE"
	ExclusionManualIntervention             = "MANUAL_INTERVENTION"
	ExclusionUnattributedIntervention       = "UNATTRIBUTED_INTERVENTION"
	ExclusionBoundaryNotProven              = "BOUNDARY_NOT_PROVEN"
	ExclusionSupersededByEarlierIncarnation = "SUPERSEDED_BY_EARLIER_INCARNATION"
	ExclusionSourceRoundIdentityMissing     = "SOURCE_ROUND_IDENTITY_MISSING"
)

// BoundaryReason is the closed vocabulary of a boundary verdict.
type BoundaryReason string

// Boundary reasons.
const (
	BoundaryProven                 BoundaryReason = "BOUNDARY_PROVEN"
	BoundaryCutoffNotBeforeCall    BoundaryReason = "C_NOT_BEFORE_F"
	BoundaryNoCallCoverageUnproven BoundaryReason = "NO_CALL_COVERAGE_UNPROVEN"
	BoundaryCutoffUnknown          BoundaryReason = "CUTOFF_UNKNOWN"
)

// Coverage-break reasons.
const (
	CoverageOrphanCallReturned = "ORPHAN_CALL_RETURNED"
	CoverageUnclassifiedFact   = "UNCLASSIFIED_FACT_ON_ROUND"
	CoverageUndecodableFact    = "UNDECODABLE_FACT_ON_ROUND"
	CoverageNotProven          = "COVERAGE_NOT_PROVEN"
)

// SourceRoundStatus is the closed vocabulary of a reconciled source round.
type SourceRoundStatus string

// Source-round registry statuses.
const (
	SourceRoundUnique                SourceRoundStatus = "UNIQUE"
	SourceRoundDeduplicatedIdentical SourceRoundStatus = "DEDUPLICATED_IDENTICAL"
	SourceRoundConflict              SourceRoundStatus = "CONFLICT"
	SourceRoundInvalid               SourceRoundStatus = "INVALID"
)

// Vocabulary of the store this package reads through the P2 mirror. The
// kinds below are not declared by predictioneval; they are the store's own
// names, re-declared here because a manual root fact and an unclassified
// fact both bear on an episode's admissibility.
const (
	kindManualControl      = "manual_control"
	kindChannelEvent       = "channel_event"
	kindScheduleDecision   = "schedule_decision"
	kindUserPredictionMade = "user_prediction_made"
	kindRoundCleanup       = "round_cleanup"
)

// EpisodeIdentity identifies one episode: one admission of one round on one
// pool inside one collector session, with the public round identity beside it.
type EpisodeIdentity struct {
	CollectorEpoch     int64  `json:"collectorEpoch"`
	CollectorSessionID string `json:"collectorSessionId"`
	PoolInstanceID     string `json:"poolInstanceId"`
	RoundIncarnationID string `json:"roundIncarnationId"`
	// EventID is the PUBLIC round identity — the Twitch prediction event id —
	// which is what makes two episodes the same source round.
	EventID string `json:"eventId"`
}

// String renders the identity canonically (length-prefixed, so a component
// containing a delimiter cannot alias another identity).
func (e EpisodeIdentity) String() string {
	var c canonical
	c.i64(e.CollectorEpoch)
	c.str(e.CollectorSessionID)
	c.str(e.PoolInstanceID)
	c.str(e.RoundIncarnationID)
	c.str(e.EventID)
	return hexEncode(c.bytes())
}

// CallKind classifies a raw CALL_STARTED signal.
type CallKind string

const (
	// CallKindAuto is a call carrying an attempt discriminator and not marked
	// manual: the automatic placement path.
	CallKindAuto CallKind = "AUTO"
	// CallKindManual is a call marked manual by the producer.
	CallKindManual CallKind = "MANUAL"
	// CallKindAmbiguous is a placement fact whose kind cannot be established:
	// no attempt discriminator and no manual mark, an undecodable payload, or
	// a phase outside the closed vocabulary. It still bounds the stream at its
	// own position, because the conservative boundary is the earlier one.
	CallKindAmbiguous CallKind = "AMBIGUOUS"
)

// CallSignal is one raw placement-call signal relevant to an episode.
type CallSignal struct {
	Position      int64    `json:"position"`
	ObservationID string   `json:"observationId"`
	Kind          CallKind `json:"kind"`
}

// NoCallCoverageProof is the proof that NO call could have gone unrecorded.
//
// It is required whenever a boundary is established, F or no F: even when an
// F exists, an unrecorded call could sit before it. Its zero value is NOT
// proven — a caller who supplies nothing has proven nothing. It is proven
// only when nothing matched to the episode is unclassified, undecodable, or
// a CALL_RETURNED with no recorded start.
type NoCallCoverageProof struct {
	Proven  bool     `json:"proven"`
	Reasons []string `json:"reasons,omitempty"`
}

// BoundaryProof is the COMMON_CUTOFF proof C < F for one episode.
//
// Proven is the CONJUNCTION: coverage proved AND the cutoff strictly before the
// earliest call. A recorded call at or before the cutoff leaves it false with
// C_NOT_BEFORE_F, so it is not the narrower "did a call go unrecorded"
// question — that one is the sibling NoCallCoverage.Proven, and the two fields
// share a name without sharing a meaning.
//
// It is also NOT a verdict on the episode. It can be true of a round the
// selection excludes for another reason entirely — a manual or unattributed
// intervention, for instance, whose start and return pair perfectly well. Read
// it beside Excluded, never instead of it; the factset builder does.
//
// AND IT PRESUPPOSES A CUTOFF, which the sentence above states as though it
// could not be missing. [ProveCommonCutoff] takes the cutoff as a bare int64
// with no presence bit, so it cannot tell "no cutoff" from "a cutoff at
// position 0": called with a zero, or a negative, it reports BOUNDARY_PROVEN
// on evidence that proves nothing. That is the most permissive value the
// parameter can take, so the mistake fails OPEN. The vocabulary already names
// the honest verdict — [BoundaryCutoffUnknown] — but the predicate never
// returns it; [SelectEpisodes] records it by hand on the branch where the
// first opportunity is unusable, which is why this package's own path is
// safe.
//
// So correct use of the exported predicate requires knowledge its signature
// does not carry, and a caller who has no cutoff must not call it. Carrying
// the presence in the signature would be the real fix, and it would change an
// approved seam's shape and refuse a shape nothing in the producer forbids (an
// observation id is a plain TEXT column, so an empty one is storable). That is
// an owner decision, not a mechanical one; it is recorded as a limitation
// rather than taken here, and the behaviour is pinned by a test so it cannot
// drift while the decision is outstanding.
type BoundaryProof struct {
	Rule                      string              `json:"rule"`
	CutoffPosition            int64               `json:"cutoffPosition"`
	CutoffObservationID       string              `json:"cutoffObservationId,omitempty"`
	EarliestCallPresent       bool                `json:"earliestCallPresent"`
	EarliestCallPosition      int64               `json:"earliestCallPosition"`
	EarliestCallObservationID string              `json:"earliestCallObservationId,omitempty"`
	EarliestCallKind          CallKind            `json:"earliestCallKind,omitempty"`
	NoCallCoverage            NoCallCoverageProof `json:"noCallCoverage"`
	Proven                    bool                `json:"proven"`
	Reason                    BoundaryReason      `json:"reason"`
}

// EpisodeSelection is the outcome of seams 1–2 for one episode.
type EpisodeSelection struct {
	Episode EpisodeIdentity `json:"episode"`
	// Quality is the episode's PARTIAL verdict from seams 1–3 alone. The
	// case's quality is [AssessCaseQuality], which merges this with every
	// later seam; a selected episode reads PRIMARY_SCORABLE here because
	// nothing has disqualified it YET.
	Quality QualityRecord `json:"quality"`
	// Excluded / ExclusionReasons say whether and why the episode is not
	// evidence. Every reason is kept; none silently wins.
	Excluded         bool     `json:"excluded"`
	ExclusionReasons []string `json:"exclusionReasons,omitempty"`
	// FirstOpportunity is the first automatic attempt the RAW evidence shows
	// on the episode, whether or not it is usable. It is established from the
	// facts, not from the list of attempts P2 could materialize, so an
	// unusable first attempt cannot vanish and hand its place to the next.
	FirstOpportunity *predictioneval.AttemptKey `json:"firstOpportunity,omitempty"`
	// FirstOpportunityPosition is the causal position of the first fact
	// that names the first opportunity, or of the episode's first fact when
	// there is none; it orders incarnations of one public round.
	FirstOpportunityPosition int64 `json:"firstOpportunityPosition"`
	FirstOpportunityUsable   bool  `json:"firstOpportunityUsable"`
	// P2Exclusions carries the P2 materialization refusals of the first
	// opportunity when it is unusable.
	P2Exclusions []string `json:"p2Exclusions,omitempty"`
	// LaterAttempts counts the automatic attempts AFTER the first. They are
	// reported so refused substitution is visible, never selected.
	LaterAttempts int `json:"laterAttempts"`
	// Attempt is the selected opportunity's P2 knowledge, present only when
	// the first opportunity is usable.
	Attempt       *predictioneval.AttemptKnowledge `json:"attempt,omitempty"`
	Boundary      BoundaryProof                    `json:"boundary"`
	ManualSignals []string                         `json:"manualSignals,omitempty"`
}

// EvidenceSelection is the outcome of seams 1–3 over one dataset.
type EvidenceSelection struct {
	ProtocolVersion string                          `json:"protocolVersion"`
	Source          predictioneval.SourceProvenance `json:"source"`
	SessionAdmitted bool                            `json:"sessionAdmitted"`
	SessionRefusals []string                        `json:"sessionRefusals,omitempty"`
	// Episodes are every episode the session holds, in causal order, each
	// selected or excluded with reasons.
	Episodes []EpisodeSelection `json:"episodes,omitempty"`
	// DuplicateSourceRounds names public rounds more than one episode claims.
	DuplicateSourceRounds []string `json:"duplicateSourceRounds,omitempty"`
}

// SourceRoundClaim is one selected opportunity's claim on a public round.
type SourceRoundClaim struct {
	Episode       EpisodeIdentity           `json:"episode"`
	Attempt       predictioneval.AttemptKey `json:"attempt"`
	FactsetDigest string                    `json:"factsetDigest"`
}

// SourceRoundEntry is one public round's reconciled status.
type SourceRoundEntry struct {
	EventID string             `json:"eventId"`
	Status  SourceRoundStatus  `json:"status"`
	Claims  []SourceRoundClaim `json:"claims"`
	// Canonical is the ONE claim that stands for the round, present for
	// UNIQUE and DEDUPLICATED_IDENTICAL only. A CONFLICT has none: two
	// different pieces of evidence for one round are not evidence for it.
	Canonical *SourceRoundClaim `json:"canonical,omitempty"`
}

// SourceRoundRegistry is the reconciled source-round identity set of the
// claims the caller supplied. An event id can carry two entries: its
// reconciled one and an INVALID one for claims that named it without a
// factset digest.
type SourceRoundRegistry struct {
	Version string             `json:"version"`
	Entries []SourceRoundEntry `json:"entries"`
	Digest  string             `json:"digest"`
}

// SelectEpisodes is seams 1–3 over one dataset.
//
// It returns an error only for a caller contract violation (records out of
// causal order). Every evidential refusal is typed on the result.
func SelectEpisodes(ds predictioneval.SourceDataset) (EvidenceSelection, error) {
	out := EvidenceSelection{ProtocolVersion: ProtocolVersion, Source: ds.Source}
	pk, err := predictioneval.MaterializePairedKnowledge(ds)
	if err != nil {
		return EvidenceSelection{}, err
	}
	out.SessionRefusals = sessionRefusals(ds, pk)
	if len(out.SessionRefusals) > 0 {
		return out, nil
	}
	out.SessionAdmitted = true

	attempts := make(map[predictioneval.AttemptKey]*predictioneval.AttemptKnowledge, len(pk.Attempts))
	for i := range pk.Attempts {
		attempts[pk.Attempts[i].Key] = &pk.Attempts[i]
	}

	// Episodes, keyed by (pool, incarnation), in causal order of first
	// appearance. Records with no incarnation are not episodes; they can
	// still be signals.
	order := []poolRound{}
	groups := map[poolRound][]*predictioneval.SourceRecord{}
	for i := range ds.Records {
		r := &ds.Records[i]
		if r.RoundIncarnationID == "" {
			continue
		}
		k := poolRound{r.PoolInstanceID, r.RoundIncarnationID}
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], r)
	}

	knownEvents := map[string]bool{}
	for _, k := range order {
		for _, r := range groups[k] {
			if r.EventID != "" {
				knownEvents[r.EventID] = true
			}
		}
	}
	signals := indexSignals(ds.Records, knownEvents)

	for _, k := range order {
		recs := groups[k]
		ep := EpisodeSelection{
			Quality: NewQualityRecord(),
			Episode: EpisodeIdentity{
				CollectorEpoch:     ds.Source.CollectorEpoch,
				CollectorSessionID: ds.Source.CollectorSessionID,
				PoolInstanceID:     k.pool,
				RoundIncarnationID: k.round,
				EventID:            recs[0].EventID,
			},
			Boundary: BoundaryProof{Rule: BoundaryRuleCommonCutoff, Reason: BoundaryCutoffUnknown},
		}
		// The public identity must be consistent across the episode's facts.
		for _, r := range recs {
			if r.EventID != ep.Episode.EventID {
				ep.Episode.EventID = ""
				break
			}
		}
		ep.FirstOpportunityPosition = recs[0].CollectorSequence

		exclude := func(reason string) {
			// Repeating a reason already recorded is a no-op in the OUTPUT —
			// ExclusionReasons and Quality.Reasons both go through appendOnce,
			// and Downgrade appends to History only on an actual rank drop, so
			// the second call with the same reason changes nothing. It is not a
			// no-op in COST: Downgrade normalises, which clones the reasons and
			// the history every time. On a colliding-event dataset this fires
			// once per matched foreign call, so the clone is per-signal.
			before := len(ep.ExclusionReasons)
			ep.ExclusionReasons = appendOnce(ep.ExclusionReasons, reason)
			if ep.Excluded && len(ep.ExclusionReasons) == before {
				return
			}
			ep.Excluded = true
			ep.Quality = ep.Quality.Downgrade(QualityExcluded, reason)
		}

		// ---- Seam 2: the first automatic opportunity, from RAW facts. -----
		// Selection order is a fact about the RAW evidence: the earliest
		// automatic row is the episode's first opportunity whether or not it
		// names an attempt. A row that names none is an opportunity nothing
		// can be materialized for — unusable, and a later attempt must not be
		// searched out to stand in for it, or the episode would be scored on
		// an opportunity that was not its first.
		var firstID int64
		var firstSeq int64
		var earliestSeq int64 // orders the scan only; never an episode field
		earliestNamesAttempt := false
		sawAutomaticRow := false
		attemptIDs := map[int64]bool{}
		attemptObs := map[int64][]string{}
		for _, r := range recs {
			if r.Kind != predictioneval.KindAutoDecision {
				continue
			}
			id, ok := r.Payload.Counters[predictioneval.CounterAutoAttemptID]
			namesAttempt := ok && id > 0
			if !sawAutomaticRow || r.CollectorSequence < earliestSeq {
				sawAutomaticRow = true
				earliestSeq, earliestNamesAttempt = r.CollectorSequence, namesAttempt
			}
			if !namesAttempt {
				continue
			}
			attemptIDs[id] = true
			attemptObs[id] = append(attemptObs[id], r.ObservationID)
			if firstID == 0 || r.CollectorSequence < firstSeq {
				firstID, firstSeq = id, r.CollectorSequence
			}
		}
		switch {
		case !sawAutomaticRow:
			exclude(ExclusionNoAutomaticOpportunity)
		case !earliestNamesAttempt:
			// Every id-bearing attempt on the round is a substitution this
			// refuses. FirstOpportunityPosition keeps the episode's own first
			// position: seam 3 orders supersession by it, and an episode whose
			// earliest automatic row names no attempt must not lose its place
			// in that order — it is still the round's earliest incarnation.
			ep.LaterAttempts = len(attemptIDs)
			exclude(ExclusionFirstOpportunityUnusable)
		default:
			ep.FirstOpportunityPosition = firstSeq
			key := predictioneval.AttemptKey{
				CollectorEpoch:     ds.Source.CollectorEpoch,
				CollectorSessionID: ds.Source.CollectorSessionID,
				PoolInstanceID:     k.pool,
				AttemptID:          uint64(firstID),
			}
			ep.FirstOpportunity = &key
			ep.LaterAttempts = len(attemptIDs) - 1
			if a, ok := attempts[key]; ok && a.RoundIncarnationID == k.round {
				copyA := *a
				ep.Attempt = &copyA
				ep.FirstOpportunityUsable = true
				terminal := a.CommonInputSlice[a.TerminalIndex]
				ep.Boundary.CutoffPosition = terminal.CollectorSequence
				ep.Boundary.CutoffObservationID = terminal.ObservationID
			} else {
				ep.P2Exclusions = p2ExclusionsFor(pk.Excluded, key, attemptObs[firstID])
				exclude(ExclusionFirstOpportunityUnusable)
			}
		}

		// ---- Seam 1: interventions and calls matched to the episode. ------
		// Only the EARLIEST matched call is ever read — ProveCommonCutoff takes
		// it and so does the unusable-cutoff branch below, both through
		// earliestCall, which is a minimum and not a count. Accumulating every
		// matched call was therefore never required output work, and on a
		// colliding-event dataset it is exactly the per-episode O(matched)
		// growth the merge removed one layer down: the slice IS the bucket
		// again. The running minimum uses earliestCall's own comparison so the
		// answer is identical.
		var earliest CallSignal
		var haveCall bool
		coverage := NoCallCoverageProof{Proven: true}
		// The manual signals are deduplicated through a SEEN-SET, not through
		// appendOnce, and the difference is the whole cost. appendOnce scans the
		// accumulated list on every call, and an ObservationID is distinct per
		// record in any well-formed dataset, so the scan never dedups anything
		// and is pure O(M^2) in the manual facts matched to one episode.
		// Measured on the fixtures' own manual-call shape, through the exported
		// SelectEpisodes: 8,000 -> 131 ms, 16,000 -> 331 ms, 32,000 -> 1.56 s,
		// 64,000 -> 7.31 s, i.e. ~4.7x per 2x input. With the set: 8.3 ms,
		// 15.5 ms, 34.5 ms, 77.2 ms -- linear, and 95x faster at M = 64,000,
		// with the list's contents and ORDER identical at every M.
		//
		// This corrects a disposition as well as a cost. The comment on
		// eachMatching used to say the declared ingest ceiling was "the only
		// real answer" to this residue. It was not: the set is, it changes no
		// output, and an independent lane demonstrated that before this was
		// written.
		//
		// THE COST HALF IS NOT SEPARATELY TESTABLE HERE, and that is stated
		// rather than left as a mutation survivor. Reverting to appendOnce
		// produces the SAME list, in the same order: what it costs is CPU, in
		// the scan, and nothing in it allocates more. This suite asserts
		// allocation ratios and never wall-clock -- a time threshold on shared
		// CI is flaky and proves nothing about complexity -- so no assertion
		// here can tell the two apart. What IS pinned is the property the
		// scan was there for: reverting to a bare append, which drops the
		// deduplication, fails the repeated-observation case beside the growth
		// measurement.
		seenManual := map[string]bool{}
		signals.eachMatching(ep.Episode, func(sig rawSignal) {
			if sig.manual && !seenManual[sig.rec.ObservationID] {
				seenManual[sig.rec.ObservationID] = true
				ep.ManualSignals = append(ep.ManualSignals, sig.rec.ObservationID)
			}
			if sig.call {
				c := CallSignal{
					Position: sig.rec.CollectorSequence, ObservationID: sig.rec.ObservationID, Kind: sig.callKind,
				}
				if !haveCall || c.Position < earliest.Position ||
					(c.Position == earliest.Position && c.ObservationID < earliest.ObservationID) {
					earliest, haveCall = c, true
				}
				// A call the episode's OWN automatic attempts did not make is
				// an intervention: manual when marked, otherwise of unknown
				// origin — and unknown is not automatic. An automatic attempt
				// id names one of the episode's own attempts only on the
				// episode's own pool, session and epoch: the counter restarts
				// per pool, so another pool's attempt 1 on the same public
				// round is another attempt.
				if sig.callKind == CallKindAuto {
					if id := sig.attemptID; !attemptIDs[id] || !sameAttemptScope(sig.rec, ep.Episode) {
						exclude(ExclusionUnattributedIntervention)
					}
				} else if !sig.manual {
					exclude(ExclusionUnattributedIntervention)
				}
			}
			if sig.orphanReturn {
				coverage.Proven = false
				coverage.Reasons = appendOnce(coverage.Reasons, CoverageOrphanCallReturned)
			}
			if sig.unclassified {
				coverage.Proven = false
				coverage.Reasons = appendOnce(coverage.Reasons, CoverageUnclassifiedFact)
			}
			// The D16 contradiction belongs to the incarnation that recorded
			// the start, and to that one only. Signals are matched by EventID
			// among other keys, so without this scope an unspent start reaches
			// every episode of the public round — and it does NOT merely add a
			// reason to them. Attempt ids are scoped to the pool rather than to
			// the incarnation, so a sibling can own the id the start carries,
			// never collect UNATTRIBUTED_INTERVENTION, and be excluded by this
			// mark alone. An independent lane demonstrated exactly that flip on
			// an episode whose own evidence was complete.
			if sig.unspentStart &&
				sig.rec.PoolInstanceID == ep.Episode.PoolInstanceID &&
				sig.rec.RoundIncarnationID == ep.Episode.RoundIncarnationID {
				coverage.Proven = false
				coverage.Reasons = appendOnce(coverage.Reasons, CoverageUnclassifiedFact)
			}
			if sig.undecodable {
				coverage.Proven = false
				coverage.Reasons = appendOnce(coverage.Reasons, CoverageUndecodableFact)
			}
		})
		if len(ep.ManualSignals) > 0 {
			exclude(ExclusionManualIntervention)
		}
		// ProveCommonCutoff is exported and keeps its signature; it is handed
		// the one call it would have selected anyway.
		var calls []CallSignal
		if haveCall {
			calls = []CallSignal{earliest}
		}
		if ep.FirstOpportunityUsable {
			ep.Boundary = ProveCommonCutoff(ep.Boundary.CutoffPosition, ep.Boundary.CutoffObservationID, calls, coverage)
			if !ep.Boundary.Proven {
				exclude(ExclusionBoundaryNotProven)
			}
		} else {
			// The cutoff is unknown, so nothing can be proven about it; the
			// signals are still reported.
			ep.Boundary.NoCallCoverage = coverage
			if f, ok := earliestCall(calls); ok {
				ep.Boundary.EarliestCallPresent = true
				ep.Boundary.EarliestCallPosition = f.Position
				ep.Boundary.EarliestCallObservationID = f.ObservationID
				ep.Boundary.EarliestCallKind = f.Kind
			}
		}
		if ep.Episode.EventID == "" {
			exclude(ExclusionSourceRoundIdentityMissing)
		}
		out.Episodes = append(out.Episodes, ep)
	}

	// ---- Seam 3 within the dataset: one public round, one opportunity. ---
	byEvent := map[string][]int{}
	for i := range out.Episodes {
		if id := out.Episodes[i].Episode.EventID; id != "" {
			byEvent[id] = append(byEvent[id], i)
		}
	}
	events := make([]string, 0, len(byEvent))
	for id := range byEvent {
		events = append(events, id)
	}
	sort.Strings(events)
	for _, id := range events {
		idx := byEvent[id]
		if len(idx) < 2 {
			continue
		}
		out.DuplicateSourceRounds = append(out.DuplicateSourceRounds, id)
		sort.SliceStable(idx, func(a, b int) bool {
			return out.Episodes[idx[a]].FirstOpportunityPosition < out.Episodes[idx[b]].FirstOpportunityPosition
		})
		// The EARLIEST incarnation is the round's opportunity, usable or not.
		// Every later one is superseded: it can never substitute.
		for _, i := range idx[1:] {
			ep := &out.Episodes[i]
			ep.Excluded = true
			ep.ExclusionReasons = appendOnce(ep.ExclusionReasons, ExclusionSupersededByEarlierIncarnation)
			ep.Quality = ep.Quality.Downgrade(QualityExcluded, ExclusionSupersededByEarlierIncarnation)
		}
	}
	return out, nil
}

// sessionRefusals is the session gate.
// sessionRefusalKinds reduces a session-refusal list to its DISTINCT members,
// so a refusal can name what went wrong without rendering one entry per
// excluded record.
//
// Every member of the list is a package constant or a producer anomaly prefix,
// so the distinct set is small and bounded by the vocabulary. The list itself
// is NOT: sessionRefusals appends RefusalSessionP2Exclusion once per
// session-level exclusion, and materialization emits one per foreign-session
// record, so its length is the caller's dataset. An independent lane built a
// 20,000-record foreign-session dataset and got an 840,098-byte refusal. That
// is why the caller reports the extent beside these kinds rather than the list.
//
// The sort is DEFENCE IN DEPTH and is deliberately not separately tested: the
// members enter in sessionRefusals' own statement order, which is fixed by the
// source and not by the caller's records, so removing the sort changes nothing
// any reachable input can observe. Writing a test for it would mean varying the
// arrival order of records the producer requires to be in ascending causal
// order. It is here so that a future member added inside a range loop cannot
// make the refusal message depend on the dataset.
func sessionRefusalKinds(rs []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		if seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

func sessionRefusals(ds predictioneval.SourceDataset, pk predictioneval.PairedKnowledge) []string {
	var out []string
	src := ds.Source
	if src.SessionReading != AdmittedSessionReading {
		out = append(out, RefusalSessionReadingNotAsFinalized)
	}
	if src.CloseState != AdmittedSessionCloseState {
		out = append(out, RefusalSessionCloseStateNotComplete)
	}
	if src.ProducerRevision != predictioneval.SupportedProducerRevision {
		out = append(out, RefusalProducerRevisionUnsupported)
	}
	if src.DroppedCount < 0 || src.FactsPresent < 0 || src.CommittedCount < 0 ||
		src.WitnessesVerified < 0 || src.WitnessesUnchecked < 0 {
		out = append(out, RefusalSessionCountersNegative)
	}
	if src.DroppedCount > 0 {
		out = append(out, RefusalSessionFactsLost)
	}
	if src.FactsPresent != src.CommittedCount {
		out = append(out, RefusalSessionFactsIncomplete)
	}
	if src.WitnessesUnchecked > 0 {
		out = append(out, RefusalSessionWitnessesUnchecked)
	}
	if src.WitnessesVerified == 0 && src.FactsPresent > 0 {
		out = append(out, RefusalSessionNoWitnessesVerified)
	} else if src.WitnessesVerified+src.WitnessesUnchecked != src.FactsPresent {
		out = append(out, RefusalSessionWitnessesIncomplete)
	}
	for _, r := range ds.Records {
		if r.CollectorEpoch != src.CollectorEpoch || r.CollectorSessionID != src.CollectorSessionID {
			out = append(out, RefusalSessionForeignFacts)
			break
		}
	}
	for _, a := range pk.Anomalies {
		out = append(out, RefusalSessionAnomalyPrefix+a)
	}
	// Session-level P2 refusals (as opposed to per-attempt ones) carry no key
	// and no observation: they qualify the whole reading.
	for _, e := range pk.Excluded {
		if e.Key == nil && e.ObservationID == "" {
			out = append(out, RefusalSessionP2Exclusion+e.Reason)
		}
	}
	return out
}

// p2ExclusionsFor collects the P2 refusals attributable to one attempt.
//
// THE OBSERVATION IDS ARE INDEXED, not re-scanned per exclusion, and this is the
// third superlinear accumulation found on this seam. The nested form walked the
// whole observation slice for EVERY exclusion whose key did not match -- with
// the emptiness test inside the inner loop, so an exclusion carrying no
// observation id walked it doing nothing, and a match did not break. Both
// operands grow with the caller's dataset (one excluded entry per refused
// record, one observation per record naming the attempt), so the cost was
// O(|excluded| x |obs|) per excluded episode: quadratic in one dataset, through
// the exported SelectEpisodes.
//
// The set is built once per call. Callers reach this once per episode whose
// first opportunity has no materialized attempt, so the index is not hoisted
// further: `obs` is that episode's own list and differs per call.
//
// LIKE THE MANUAL-SIGNAL REPAIR, THIS IS NOT SEPARATELY TESTABLE, and that is
// stated rather than left as a mutation survivor. Reverting to the nested scan
// produces the SAME attribution: what it costs is CPU, and it allocates nothing
// extra, so an allocation ratio -- the only kind of cost assertion this suite
// makes, because a wall-clock threshold on shared CI is flaky and proves
// nothing about complexity -- cannot tell the two apart. What IS pinned is the
// answer: the attribution test drives both directions of the predicate, and the
// growth test asserts the ATTRIBUTION is unchanged beside its ratio.
func p2ExclusionsFor(excluded []predictioneval.Exclusion, key predictioneval.AttemptKey, obs []string) []string {
	var out []string
	byObservation := make(map[string]bool, len(obs))
	for _, id := range obs {
		if id != "" {
			byObservation[id] = true
		}
	}
	// The emptiness test lives in the index build above, not here: a map that
	// never holds "" cannot match an exclusion carrying "". Keeping both was a
	// redundant guard, and a mutation dropping this one survived because it
	// could not change an answer.
	for _, e := range excluded {
		switch {
		case e.Key != nil && *e.Key == key:
			out = appendOnce(out, e.Reason)
		case byObservation[e.ObservationID]:
			out = appendOnce(out, e.Reason)
		}
	}
	return out
}

// poolRound keys an episode inside one session.
type poolRound struct{ pool, round string }

// roundTriple keys a call start by everything a non-automatic call names.
type roundTriple struct{ pool, round, event string }

// autoStartSlot is one recorded automatic call start: the attempt's full
// native identity and the round the start was recorded on.
//
// The round is a COHERENCE check, not a disambiguator. An honest producer
// mints the discriminator from a per-pool counter that never resets (an
// atomic.Uint64 in the pool), so within one epoch, session and pool there is
// exactly one attempt 1 and the identity alone already names it. What the
// round adds is a refusal: supplied evidence in which a start and a return
// carry the same attempt identity but disagree about the incarnation or the
// public round is contradicted evidence, and a start settles only a return
// that agrees with it. The identity half is kept whole — it is the native
// AttemptKey tuple — so the refusal is layered on that identity rather than
// replacing it. Two of that tuple's four components cannot discriminate here
// in practice: a dataset carrying a foreign collector epoch or session is
// refused whole, before any of this runs, so within an admitted session every
// record already agrees on both. They are kept because the key is the native
// identity, not a subset of it chosen for one caller.
//
// The cost is a constant factor, paid on every automatic placement fact: the
// key holds five strings where the identity alone held two, it hashes the pool
// twice, and starts that once shared one counter entry now occupy one entry per
// round. It stays linear per record; it is not free.
//
// A slot holds AT MOST ONE start. The attempt discriminator names one call and
// the producer writes one start per call, so a second start in the same slot is
// not a second call to pair against — it is a fact the producer could not have
// written, and it is refused rather than banked.
type autoStartSlot struct {
	attempt attemptStartKey
	round   roundTriple
}

// autoStartTally is what one slot has seen. Both halves are needed and neither
// substitutes for the other: recorded refuses a duplicate start even after the
// first one has already been spent, and spent is what makes consumption
// one-to-one.
type autoStartTally struct {
	recorded, spent bool
	// at is where the start's own signal sits in the output, so that a start
	// left unspent at the end of the pass can be marked on the fact that made
	// the claim rather than on the round in general.
	at int
}

// autoStartSlotOf names the slot one automatic call fact belongs to. The
// identity half is derived here from the record itself, so recording a start
// and spending one cannot read it differently; the attempt discriminator and
// the round are passed in, and both call sites pass the same per-record
// values under the same guard.
func autoStartSlotOf(r *predictioneval.SourceRecord, attempt int64, round roundTriple) autoStartSlot {
	return autoStartSlot{
		attemptStartKey{r.CollectorEpoch, r.CollectorSessionID, r.PoolInstanceID, attempt},
		round,
	}
}

// attemptStartKey keys an automatic call start by the attempt's FULL
// identity. The automatic attempt counter restarts in every pool, so the
// numeric id alone would pair one pool's orphan return with another pool's
// start and hide the unrecorded call.
type attemptStartKey struct {
	epoch   int64
	session string
	pool    string
	attempt int64
}

// rawSignal is one fact's bearing on an episode's admissibility.
type rawSignal struct {
	rec          *predictioneval.SourceRecord
	manual       bool
	call         bool
	callKind     CallKind
	attemptID    int64
	orphanReturn bool
	unclassified bool
	undecodable  bool
	// unspentStart marks a recorded automatic call start that no return ever
	// spent. It is kept apart from unclassified because it is scoped: the
	// contradiction is about the incarnation that RECORDED the start, and
	// signals reach every episode sharing the public round.
	unspentStart bool
}

// signalIndex holds every fact's signal, indexed by the three ways a fact
// can bear on an episode: the same public round (any incarnation), the same
// incarnation, or an unattributable fact on the same pool — the last
// conservatively, because a fact that names no incarnation and no round any
// episode of the session carries cannot be proven NOT to concern this one.
type signalIndex struct {
	signals            []rawSignal
	byEvent            map[string][]int
	byPoolRound        map[poolRound][]int
	byPoolUnattributed map[string][]int
}

// indexSignals reads every fact once and records what it signals.
// knownEvents is the set of public round identities the session's episodes
// carry, admitted or excluded; a fact naming none of them and no
// incarnation is unattributable.
func indexSignals(recs []predictioneval.SourceRecord, knownEvents map[string]bool) signalIndex {
	ix := signalIndex{
		signals:            classifySignals(recs),
		byEvent:            map[string][]int{},
		byPoolRound:        map[poolRound][]int{},
		byPoolUnattributed: map[string][]int{},
	}
	for i := range ix.signals {
		r := ix.signals[i].rec
		if r.EventID != "" {
			ix.byEvent[r.EventID] = append(ix.byEvent[r.EventID], i)
		}
		if r.RoundIncarnationID != "" {
			k := poolRound{r.PoolInstanceID, r.RoundIncarnationID}
			ix.byPoolRound[k] = append(ix.byPoolRound[k], i)
		}
		if r.RoundIncarnationID == "" && (r.EventID == "" || !knownEvents[r.EventID]) {
			ix.byPoolUnattributed[r.PoolInstanceID] = append(ix.byPoolUnattributed[r.PoolInstanceID], i)
		}
	}
	return ix
}

// eachMatching visits the signals matched to one episode, in index order and
// without duplicates, WITHOUT materializing them.
//
// It used to copy the three posting lists into one slice, sort it, and build a
// []rawSignal — per episode. On a dataset whose episodes share one EventID
// that slice IS the whole event bucket, so N episodes copied and sorted an
// O(N)-sized bucket N times: quadratic in both work and transient allocation,
// The allocation was the part that mattered, because it turns a slow parse
// into an OOM before the verifier can produce a fail-closed answer.
//
// Absolute figures are environment-dependent — two independent measurements of
// the same 12,288-record shape here differed by about 2.5x (2.3 GB and 6.7 GB)
// — so what is pinned is the SHAPE: the old code grew about 16x per 4x input,
// this one grows about 4x, and the regression test asserts ratios rather than
// byte counts for exactly that reason.
//
// Nothing about WHICH signals match, their order, or their deduplication
// changes here. Each posting list is built by appending indices in increasing
// order, so all three are already sorted ascending and carry each index at
// most once; a three-way merge therefore yields exactly the ascending unique
// sequence the sort produced, and consuming every list that holds the current
// minimum is what drops the duplicates the old dedup dropped. The equivalence
// is not left to that argument: evidence_internal_test.go keeps a verbatim copy
// of the replaced implementation and compares the two element by element over
// randomized colliding and non-colliding datasets, and asserts the
// strictly-ascending property that argument rests on. This sentence previously
// described a comparison that did not exist; an independent lane caught it.
//
// WHAT THIS DOES AND DOES NOT REMOVE, because that distinction is the honest
// content of the repair. It removes the per-episode ALLOCATION and the sort's
// log factor. It does NOT make the work linear in the input: byEvent is keyed
// on EventID alone, so the matching RELATION is itself quadratic on a
// colliding-event dataset — every record of an event is matched to every
// episode of that event, and the caller inspects each match. Measured
// independently, the visit count grows exactly 4x per 2x input on that shape.
//
// That residue is the SIZE OF THE RELATION rather than an implementation
// artefact, and shrinking it would change which signals match which episodes,
// which is the one thing this rewrite had to keep identical. What the space
// claim buys is the failure MODE: the verifier now gets slower on a hostile
// input instead of being killed by the allocator before it can produce its
// fail-closed answer. A declared ceiling on this ingest path is still the
// follow-up the measurements argue for.
//
// ONE RESIDUE THAT USED TO SURVIVE HERE, AND NO LONGER DOES. ManualSignals was
// accumulated per matched manual signal through appendOnce, which scans the
// whole accumulated list every time; observation ids are distinct per record,
// so the scan deduplicated nothing and cost O(M^2) in the manual facts matched
// to one episode. This comment used to conclude that the list is exported
// output and therefore "not free to drop", and that a declared ingest ceiling
// was "the only real answer to it". The second half was wrong: a seen-set
// deduplicates identically, in the same order, for linear cost. It is at the
// accumulation site, with the measurements. The CPU residue in the MATCHING
// relation above is real and remains; this one was not the same thing.
//
// One honest note on the deduplication: no PRODUCTION consumer is
// duplicate-sensitive. Seam 1 sets flags idempotently, collects manual signals
// and coverage reasons through appendOnce, and tracks the earliest call as a
// running minimum rather than a count. So a duplicate visit would change no
// verdict today; the dedup is kept because this rewrite had to preserve the old
// behaviour exactly, and it IS pinned — the differential test kills a mutant
// that visits every signal twice. If a future consumer ever counts matched
// signals, this is the line it depends on.
func (ix signalIndex) eachMatching(ep EpisodeIdentity, visit func(rawSignal)) {
	var a []int
	if ep.EventID != "" {
		a = ix.byEvent[ep.EventID]
	}
	b := ix.byPoolRound[poolRound{ep.PoolInstanceID, ep.RoundIncarnationID}]
	c := ix.byPoolUnattributed[ep.PoolInstanceID]
	for i, j, k := 0, 0, 0; i < len(a) || j < len(b) || k < len(c); {
		// The minimum of the live heads, tracked with a FOUND flag rather than
		// a sentinel value. A sentinel would be a value a posting list could in
		// principle hold, and if it ever did, no head would compare below it,
		// no pointer would advance and this loop would not terminate. Indices
		// come from ranging over the signals so that cannot happen today; the
		// flag costs nothing and removes the "today" from that sentence.
		n, found := 0, false
		if i < len(a) && (!found || a[i] < n) {
			n, found = a[i], true
		}
		if j < len(b) && (!found || b[j] < n) {
			n, found = b[j], true
		}
		if k < len(c) && (!found || c[k] < n) {
			n, found = c[k], true
		}
		if !found {
			return
		}
		// Every list holding the minimum advances past it, so an index carried
		// by two lists is visited once and the next minimum is strictly
		// greater.
		if i < len(a) && a[i] == n {
			i++
		}
		if j < len(b) && b[j] == n {
			j++
		}
		if k < len(c) && c[k] == n {
			k++
		}
		visit(ix.signals[n])
	}
}

// classifySignals reads every fact once and records what it signals.
func classifySignals(recs []predictioneval.SourceRecord) []rawSignal {
	out := make([]rawSignal, 0, len(recs))
	// Recorded call starts, for orphan-return detection: calls classified
	// AUTOMATIC by the attempt's full native identity and the round they were
	// recorded on (see autoStartSlot), every other call — manual or ambiguous,
	// whether or not it carries an attempt id — by round alone. A start is SPENT
	// rather than merely present: the producer records one start per return, so
	// a start settles ONE return and is consumed doing it — which is what the
	// automatic side's spent flag records, and what the non-automatic side's
	// count records.
	autoStarts := map[autoStartSlot]autoStartTally{}
	otherStarts := map[roundTriple]int{}
	for i := range recs {
		r := &recs[i]
		sig := rawSignal{rec: r}
		if r.Payload.Manual != nil && *r.Payload.Manual {
			sig.manual = true
		}
		if r.Kind == kindManualControl {
			sig.manual = true
		}
		id, hasID := r.Payload.Counters[predictioneval.CounterAutoAttemptID]
		if hasID && id > 0 {
			sig.attemptID = id
		}
		triple := roundTriple{r.PoolInstanceID, r.RoundIncarnationID, r.EventID}
		switch r.Kind {
		case predictioneval.KindPlacement:
			// A placement fact this package cannot read — undecodable, or in a
			// payload version it does not support — is a fact ABOUT the call
			// record that cannot be read. Its phase could be CALL_RETURNED and
			// the start it would name could precede the cutoff, so it positions
			// itself as a call of unknown shape AND breaks the no-call coverage
			// argument, the way an unreadable fact of any kind does — the arm
			// below applies the same version test to every other kind this
			// package reads.
			switch {
			case r.PayloadUndecodable || r.PayloadVersion != predictioneval.SupportedPayloadVersion:
				sig.call, sig.callKind = true, CallKindAmbiguous
				sig.undecodable = true
			case r.Payload.Phase == predictioneval.PhaseCallStarted:
				sig.call = true
				switch {
				case sig.manual:
					sig.callKind = CallKindManual
				case sig.attemptID > 0:
					sig.callKind = CallKindAuto
				default:
					sig.callKind = CallKindAmbiguous
				}
				if sig.callKind == CallKindAuto {
					// One start per call, and the attempt discriminator names
					// the call: a SECOND start in the same slot is a fact the
					// producer cannot have written. It declares placement as
					// "the single Twitch placement call - one fact immediately
					// before it and one immediately after. Never wraps, retries
					// or alters it", and both of its call sites emit exactly
					// that pair. Banking a second one would let it settle a
					// return that has no start of its own, which is the very
					// thing one-to-one consumption exists to refuse, so the
					// contradicted fact is refused instead of counted.
					slot := autoStartSlotOf(r, sig.attemptID, triple)
					if t := autoStarts[slot]; t.recorded {
						sig.unclassified = true
					} else {
						t.recorded, t.at = true, len(out)
						autoStarts[slot] = t
					}
				} else {
					// No discriminator here, so two starts on one round are two
					// calls the producer could have made — an operator can place
					// again after a failed placement — and the count stands. The
					// One count serves both manual and ambiguous calls, so a
					// start of either kind settles a return of the other. That
					// is contained rather than exploitable: such a start is
					// itself a call signal, so the episode is excluded as a
					// manual or unattributed intervention whichever way it
					// pairs. The at-most-one rule above is stated of the AUTOMATIC emitter,
					// which passes one captured incarnation and event id to both
					// halves of its pair; the manual emitter resolves the
					// incarnation separately for each half, so its two facts are
					// not guaranteed to agree and are not held to that rule.
					otherStarts[triple]++
				}
			case r.Payload.Phase == predictioneval.PhaseCallReturned:
				// Spend one recorded start, and only one recorded on THIS round. A
				// return that finds none has no start of its own, and the start it
				// lacks could precede the cutoff.
				started := false
				if sig.attemptID > 0 && !sig.manual {
					slot := autoStartSlotOf(r, sig.attemptID, triple)
					if t := autoStarts[slot]; t.recorded && !t.spent {
						t.spent = true
						autoStarts[slot] = t
						started = true
					}
				} else if otherStarts[triple] > 0 {
					otherStarts[triple]--
					started = true
				}
				if !started {
					sig.orphanReturn = true
				}
			default:
				// A placement fact in a phase outside the closed vocabulary is a
				// call of unknown shape at its own position — and a fact whose
				// phase cannot be named is not evidence that no call was made:
				// the producer cannot emit it, and it could be a return whose
				// missing start precedes the cutoff. It positions itself and it
				// breaks the coverage argument.
				sig.call, sig.callKind = true, CallKindAmbiguous
				sig.unclassified = true
			}
		case predictioneval.KindAutoDecision, predictioneval.KindUserTerminal, kindChannelEvent,
			kindScheduleDecision, kindUserPredictionMade, kindRoundCleanup, kindManualControl:
			// Unreadable either way: an undecodable payload, or one in a version
			// this package cannot read. Every kind LISTED HERE is read the same
			// way — the producer's source_unknown is not one of them and falls
			// to the arm below — and for the coverage argument nothing
			// downstream substitutes for it.
			// Materialization does examine the version of an AUTOMATIC fact
			// (materialize.go), but it excludes that fact at attempt level under
			// its observation id, which is not a session refusal and says nothing
			// about what else the round carries; for every other kind here it
			// returns before the version is looked at. Either way an unreadable
			// fact matched to the episode is not evidence that no call was made.
			// A CALL phase belongs to placement and to nothing else, on the one
			// ground that is provable rather than inferred: observePlacementCallOf
			// is the only writer of CALL_STARTED and CALL_RETURNED, and it always
			// writes them on a placement fact. The producer's phase table does
			// group them under a `// placement` comment, but that grouping binds
			// no phase to a kind, so it is a naming convention and is not relied
			// on here. A row of any other kind carrying a call phase is a pairing
			// the producer cannot have written, and it is marked contradicted
			// rather than positioned as a call, because only placement facts say
			// where a call happened.
			//
			// The phase is read only on a row whose payload this package CAN
			// read — the placement arm above orders its own tests the same way,
			// and a payload version this package does not support carries no
			// phase vocabulary it can judge.
			//
			// WHAT THIS DOES NOT DO, stated so the limit is not discovered: it
			// does not make the coverage proof a defence against relabelling in
			// general. The proof refuses the marks it names — an orphan return,
			// an unreadable fact, an unnameable one — and a supplier who moves a
			// row between kinds or phases can still erase a refusal two ways this
			// does not reach. A row relabelled onto a pairing the producer CAN
			// write (auto_decision + AUTO_DECIDED, which is why the arm below
			// excludes that kind rather than refusing the phase outright) is
			// indistinguishable from honest data by any intrinsic test. The
			// phase families this arm does NOT refuse — the manual, schedule,
			// terminal, cleanup and round ones, and equally the confirmation,
			// unclassified and unknown values — are left readable because their
			// writers have not been enumerated the way the call and automatic
			// ones have, and the producer's grouping comments are a naming
			// convention rather than a contract. An orphan CALL_RETURNED
			// relabelled to CALL_STARTED becomes an unspent start. That one IS
			// refusable on the producer's own terms and IS now refused, for an
			// AUTOMATIC start, by the pass at the end of this function, under
			// the owner's D16 disposition — at the stated cost of making the placement seam's
			// NOT_RETURNED outcome unreachable through this pipeline, which is
			// why that status is now documented as reserved and non-emittable
			// rather than deleted. This paragraph previously said the opposite,
			// having been written before that decision. Authenticating the RECORDS themselves is what would
			// close the class: ObservationSHA256 is carried on every row and
			// this package verifies no record digest. (It does verify plenty of
			// its own — the factset, the registry, the ruleset, the resolution —
			// but none of those binds a source row to its stored bytes.)
			switch {
			case r.PayloadUndecodable || r.PayloadVersion != predictioneval.SupportedPayloadVersion:
				sig.undecodable = true
			case r.Payload.Phase == predictioneval.PhaseCallStarted,
				r.Payload.Phase == predictioneval.PhaseCallReturned:
				sig.unclassified = true
			case r.Kind != predictioneval.KindAutoDecision &&
				(r.Payload.Phase == predictioneval.PhaseAutoDue ||
					r.Payload.Phase == predictioneval.PhaseAutoDecided ||
					r.Payload.Phase == predictioneval.PhaseAutoSkipped):
				// The automatic half of the same argument, and its erasure is
				// worse than the placement one because it needs no orphan.
				// Seam 2 reads attempt ids out of auto_decision rows, so
				// relabelling the EARLIEST automatic attempt's rows onto
				// another kind hides that attempt from selection while leaving
				// this round's coverage proven — and a LATER attempt then
				// becomes FirstOpportunity, which is the substitution the
				// protocol forbids. Established on the same footing as the call
				// phases: every site that emits AUTO_DUE, AUTO_DECIDED or
				// AUTO_SKIPPED passes ObsKindAutoDecision, so no other kind can
				// carry one.
				sig.unclassified = true
			}
		default:
			// A kind this package does not read at all. Fail closed: a fact it
			// cannot name is not evidence that no call was made.
			//
			// One kind the PRODUCER emits lands here rather than in the arm
			// above — source_unknown, a routed Prediction-domain frame whose
			// family the producer could not classify. Such a frame names no
			// round, so it is matched to every episode on its POOL: refusing
			// coverage for one costs every episode on that pool, not one
			// round's — and a dataset may carry several pools, so it is not
			// the whole session either. Whether an unreadable INBOUND frame should bear on the
			// OUTBOUND-call coverage argument at all is a producer-kind policy
			// question this package does not settle; it is recorded, and until
			// it is settled the reading here stays the conservative one.
			sig.unclassified = true
		}
		out = append(out, sig)
	}
	// A recorded automatic start that no return ever spent.
	//
	// P4 admits only COMPLETE + AS_FINALIZED sessions, and under the pinned
	// producer and lifecycle contract such a session cannot carry a factual
	// automatic CALL_STARTED without its CALL_RETURNED: the return is written
	// unconditionally with any error carried into the fact, the producer's
	// pubsub path recovers from no panic between the two, and a dropped
	// observation stops the session finalizing COMPLETE at all. The pairing is
	// therefore not merely unproven here — it is contradicted by the
	// completeness the source itself claims, so the start is marked on its own
	// fact and the round's coverage argument breaks.
	//
	// This is what makes the placement seam's NOT_RETURNED verdict unreachable
	// through this pipeline, which is the owner's D16 disposition rather than
	// an accident: the contradiction is refused HERE, before any factual
	// placement can be minted from it, instead of being reported downstream as
	// though it were a placement outcome.
	//
	// ONE EDGE THE SCOPE DOES NOT COVER, named rather than implied: a record
	// carrying no incarnation at all forms no episode, so a mark on such a
	// start is honoured NOWHERE rather than on one episode. The automatic
	// emitter always passes the resolved incarnation, so the producer cannot
	// write it; the row is still a positioned call signal and still bears on
	// the cutoff proof; and authenticating rows is out of scope here anyway.
	//
	// SCOPED TO ITS OWN INCARNATION, and the scope is not cosmetic. Signals
	// are matched to episodes by EventID among other keys, so this mark reaches
	// every episode of the public round. A previous version of this comment
	// claimed that could only add a reason and never flip an admission, because
	// such a start carries an attempt id a sibling does not own and is already
	// refused as an unattributed intervention. That was FALSE: attempt ids are
	// scoped to the pool, not the incarnation, so a sibling on the same pool
	// can own the id, collect no intervention, and be excluded by this mark
	// alone. The consuming site therefore honours the mark only on the
	// incarnation that recorded the start; the flag is separate from
	// unclassified for exactly that reason.
	//
	// AUTOMATIC starts only. The automatic emitter passes one captured
	// incarnation and event id to both halves of its pair, so the two are
	// guaranteed to agree and an unspent one is a real contradiction. The
	// manual emitter resolves the incarnation separately per half, so its facts
	// carry no such guarantee — and an episode carrying a manual call is
	// excluded as an intervention however it pairs.
	for _, t := range autoStarts {
		if t.recorded && !t.spent {
			out[t.at].unspentStart = true
		}
	}
	return out
}

// sameAttemptScope reports whether a fact was recorded on the episode's own
// pool, in its own collector session and epoch — the scope within which an
// automatic attempt id identifies one attempt.
func sameAttemptScope(r *predictioneval.SourceRecord, ep EpisodeIdentity) bool {
	return r.CollectorEpoch == ep.CollectorEpoch && r.CollectorSessionID == ep.CollectorSessionID &&
		r.PoolInstanceID == ep.PoolInstanceID
}

func earliestCall(calls []CallSignal) (CallSignal, bool) {
	if len(calls) == 0 {
		return CallSignal{}, false
	}
	f := calls[0]
	for _, c := range calls[1:] {
		if c.Position < f.Position || (c.Position == f.Position && c.ObservationID < f.ObservationID) {
			f = c
		}
	}
	return f, true
}

// ProveCommonCutoff is the boundary predicate: C strictly before the earliest
// call, with an unrecorded call ruled out by coverage.
//
// Coverage is checked FIRST, and it is required always: an unproven coverage
// means a call could sit at an unknown position — possibly before C — so
// neither a present F nor an absent one settles anything. Then C < F,
// strictly: C == F fails. With no F and proven coverage, the absence of a
// call is established.
func ProveCommonCutoff(cutoff int64, cutoffObservationID string, calls []CallSignal, coverage NoCallCoverageProof) BoundaryProof {
	out := BoundaryProof{
		Rule:                BoundaryRuleCommonCutoff,
		CutoffPosition:      cutoff,
		CutoffObservationID: cutoffObservationID,
		NoCallCoverage:      NoCallCoverageProof{Proven: coverage.Proven, Reasons: cloneStrings(coverage.Reasons)},
	}
	if f, ok := earliestCall(calls); ok {
		out.EarliestCallPresent = true
		out.EarliestCallPosition = f.Position
		out.EarliestCallObservationID = f.ObservationID
		out.EarliestCallKind = f.Kind
	}
	if !coverage.Proven {
		out.NoCallCoverage.Reasons = appendOnce(out.NoCallCoverage.Reasons, CoverageNotProven)
		out.Reason = BoundaryNoCallCoverageUnproven
		return out
	}
	if out.EarliestCallPresent {
		if cutoff < out.EarliestCallPosition {
			out.Proven, out.Reason = true, BoundaryProven
		} else {
			out.Reason = BoundaryCutoffNotBeforeCall
		}
		return out
	}
	out.Proven, out.Reason = true, BoundaryProven
	return out
}

// ClaimSourceRound derives one selected opportunity's claim on its public
// round from the dataset and the factset the dataset derives for it. It is
// the sanctioned way to produce a claim. A claim is a plain exported value
// and [ReconcileSourceRounds] reconciles what it is handed: it cannot tell a
// derived claim from a fabricated one, so a fabricated claim can ADD an
// entry to a registry but can never displace a derived one — two different
// claims on one round are a CONFLICT with no canonical claim.
func ClaimSourceRound(ds predictioneval.SourceDataset, fs CommonFactset) (SourceRoundClaim, error) {
	ep, err := derivedOpportunity(ds, fs)
	if err != nil {
		return SourceRoundClaim{}, err
	}
	return SourceRoundClaim{Episode: ep.Episode, Attempt: ep.Attempt.Key, FactsetDigest: fs.Digest}, nil
}

// ReconcileSourceRounds is seam 3 across datasets: one public round, one
// canonical claim, or none.
//
// Two claims are the same evidence only when every field agrees — the same
// episode, the same attempt, the same factset digest — which is what a
// session loaded twice produces. Anything else on one public round is a
// CONFLICT, and a conflict has no canonical claim: it is fail-closed, not
// tie-broken.
func ReconcileSourceRounds(claims []SourceRoundClaim) SourceRoundRegistry {
	out := SourceRoundRegistry{Version: SourceRoundRegistryVersion}
	groups := map[string][]SourceRoundClaim{}
	var invalid []SourceRoundClaim
	for _, c := range claims {
		if c.Episode.EventID == "" || c.FactsetDigest == "" {
			invalid = append(invalid, c)
			continue
		}
		groups[c.Episode.EventID] = append(groups[c.Episode.EventID], c)
	}
	ids := make([]string, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		cs := groups[id]
		sortClaimsByKey(cs)
		entry := SourceRoundEntry{EventID: id, Claims: cs}
		identical := true
		for _, c := range cs[1:] {
			if c != cs[0] {
				identical = false
				break
			}
		}
		switch {
		case !identical:
			entry.Status = SourceRoundConflict
		case len(cs) == 1:
			entry.Status = SourceRoundUnique
			canonical := cs[0]
			entry.Canonical = &canonical
		default:
			entry.Status = SourceRoundDeduplicatedIdentical
			canonical := cs[0]
			entry.Canonical = &canonical
		}
		out.Entries = append(out.Entries, entry)
	}
	sortClaimsByKey(invalid)
	for _, c := range invalid {
		out.Entries = append(out.Entries, SourceRoundEntry{EventID: c.Episode.EventID, Status: SourceRoundInvalid, Claims: []SourceRoundClaim{c}})
	}

	out.Digest = registryDigest(out.Entries)
	return out
}

// registryDigest frames a registry's entries exactly as ReconcileSourceRounds
// digests them, so a registry can be re-verified from its own entries.
func registryDigest(entries []SourceRoundEntry) string {
	var c canonical
	c.str(SourceRoundRegistryVersion)
	c.count(len(entries))
	for _, e := range entries {
		c.str(e.EventID)
		c.str(string(e.Status))
		c.count(len(e.Claims))
		for _, cl := range e.Claims {
			c.str(claimKey(cl))
		}
		c.boolean(e.Canonical != nil)
	}
	return c.digest()
}

// ErrSourceRoundRegistry names a registry that is not what
// [ReconcileSourceRounds] produces from its own entries.
var ErrSourceRoundRegistry = errors.New("p4offline: source-round registry does not re-derive from its entries")

// VerifySourceRoundRegistry re-derives a registry from the claims its own
// entries carry: the registry must be exactly what [ReconcileSourceRounds]
// produces from them — the same entries, statuses and canonical claims,
// under a digest that matches the entries — so a registry whose entries
// were altered after reconciliation, in storage or in memory (a status, a
// canonical claim, an order, a digest), is named as such, while one read
// back unchanged still re-derives. What it proves is that the entries are
// a fixed point of reconciliation over the claims they carry — nothing
// about WHICH claims: a claim withheld before reconciliation, a claim
// removed or replaced afterwards with its entry and the digest re-derived
// over the rest, or an entry removed whole, leaves a registry that
// re-derives. The digest alone frames the claims and whether an
// entry has a canonical claim, not which claim it is; the re-reconciliation
// is what binds that. Like every digest here this detects change, not
// origin: the claims are the ones the caller reconciled, and nothing here
// can tell whether every dataset of the run was among them.
func VerifySourceRoundRegistry(reg SourceRoundRegistry) error {
	var claims []SourceRoundClaim
	for _, e := range reg.Entries {
		claims = append(claims, e.Claims...)
	}
	rebuilt := ReconcileSourceRounds(claims)
	if reg.Version != SourceRoundRegistryVersion || reg.Digest == "" || reg.Digest != registryDigest(reg.Entries) ||
		!sameEntries(rebuilt.Entries, reg.Entries) {
		return ErrSourceRoundRegistry
	}
	return nil
}

// sameEntries compares two entry lists field for field, canonical claims by
// value.
func sameEntries(a, b []SourceRoundEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].EventID != b[i].EventID || a[i].Status != b[i].Status || len(a[i].Claims) != len(b[i].Claims) ||
			(a[i].Canonical == nil) != (b[i].Canonical == nil) {
			return false
		}
		for j := range a[i].Claims {
			if a[i].Claims[j] != b[i].Claims[j] {
				return false
			}
		}
		if a[i].Canonical != nil && *a[i].Canonical != *b[i].Canonical {
			return false
		}
	}
	return true
}

// isCanonical reports whether claim is the registry's one canonical claim
// for its public round: an entry for the round that is UNIQUE or
// DEDUPLICATED_IDENTICAL and whose canonical claim is this claim. It is
// read only behind [VerifySourceRoundRegistry], which has already proved
// the entry to be reconciliation's own, so the canonical claim agrees with
// every claim of its entry by construction.
func (reg SourceRoundRegistry) isCanonical(claim SourceRoundClaim) bool {
	for _, e := range reg.Entries {
		if e.EventID == claim.Episode.EventID && e.Canonical != nil && *e.Canonical == claim &&
			(e.Status == SourceRoundUnique || e.Status == SourceRoundDeduplicatedIdentical) {
			return true
		}
	}
	return false
}

// claimKey renders a claim canonically for ordering and digesting.
// sortClaimsByKey orders claims by claimKey, framing each claim's key ONCE.
//
// The key is not an accessor: claimKey builds a fresh length-prefixed framing
// of the whole claim and hex-encodes it, so it costs and allocates about twice
// the claim's own bytes. Computing it inside the comparator re-framed every
// claim about 2*log2(n) times -- measured at 91x the input over 8,192 claims on
// one shared round, which is exactly the adversarial-but-legal shape
// ReconcileSourceRounds exists to handle, and which VerifySourceRoundRegistry
// pays again for every case AssessDenominatorMembership judges.
//
// claimKey is a pure function of the claim, so decorating does not change WHICH
// claim sorts first; the test beside this asserts that against a reversed
// input. sort.SliceStable is not needed and is not used: the keys are total on
// the claims this orders, because a claim's whole value is framed into its key,
// so equal keys mean equal claims.
func sortClaimsByKey(cs []SourceRoundClaim) {
	if len(cs) < 2 {
		return
	}
	keys := make([]string, len(cs))
	idx := make([]int, len(cs))
	for i := range cs {
		keys[i] = claimKey(cs[i])
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return keys[idx[a]] < keys[idx[b]] })
	sorted := make([]SourceRoundClaim, len(cs))
	for i, j := range idx {
		sorted[i] = cs[j]
	}
	copy(cs, sorted)
}

func claimKey(c SourceRoundClaim) string {
	var k canonical
	k.str(c.Episode.String())
	k.i64(c.Attempt.CollectorEpoch)
	k.str(c.Attempt.CollectorSessionID)
	k.str(c.Attempt.PoolInstanceID)
	k.str(strconv.FormatUint(c.Attempt.AttemptID, 10))
	k.str(c.FactsetDigest)
	return hexEncode(k.bytes())
}
