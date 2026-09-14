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
			ep.Excluded = true
			ep.ExclusionReasons = appendOnce(ep.ExclusionReasons, reason)
			ep.Quality = ep.Quality.Downgrade(QualityExcluded, reason)
		}

		// ---- Seam 2: the first automatic opportunity, from RAW facts. -----
		var firstID int64
		var firstSeq int64
		attemptIDs := map[int64]bool{}
		attemptObs := map[int64][]string{}
		for _, r := range recs {
			if r.Kind != predictioneval.KindAutoDecision {
				continue
			}
			id, ok := r.Payload.Counters[predictioneval.CounterAutoAttemptID]
			if !ok || id <= 0 {
				continue
			}
			attemptIDs[id] = true
			attemptObs[id] = append(attemptObs[id], r.ObservationID)
			if firstID == 0 || r.CollectorSequence < firstSeq {
				firstID, firstSeq = id, r.CollectorSequence
			}
		}
		if firstID == 0 {
			exclude(ExclusionNoAutomaticOpportunity)
		} else {
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
		var calls []CallSignal
		coverage := NoCallCoverageProof{Proven: true}
		for _, sig := range signals.matching(ep.Episode) {
			if sig.manual {
				ep.ManualSignals = appendOnce(ep.ManualSignals, sig.rec.ObservationID)
			}
			if sig.call {
				calls = append(calls, CallSignal{
					Position: sig.rec.CollectorSequence, ObservationID: sig.rec.ObservationID, Kind: sig.callKind,
				})
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
			if sig.undecodable {
				coverage.Proven = false
				coverage.Reasons = appendOnce(coverage.Reasons, CoverageUndecodableFact)
			}
		}
		if len(ep.ManualSignals) > 0 {
			exclude(ExclusionManualIntervention)
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
func p2ExclusionsFor(excluded []predictioneval.Exclusion, key predictioneval.AttemptKey, obs []string) []string {
	var out []string
	for _, e := range excluded {
		if e.Key != nil && *e.Key == key {
			out = appendOnce(out, e.Reason)
			continue
		}
		for _, id := range obs {
			if e.ObservationID != "" && e.ObservationID == id {
				out = appendOnce(out, e.Reason)
			}
		}
	}
	return out
}

// poolRound keys an episode inside one session.
type poolRound struct{ pool, round string }

// roundTriple keys a call start by everything a non-automatic call names.
type roundTriple struct{ pool, round, event string }

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

// matching returns the signals bearing on the episode, each once, in causal
// order.
func (ix signalIndex) matching(ep EpisodeIdentity) []rawSignal {
	var idx []int
	if ep.EventID != "" {
		idx = append(idx, ix.byEvent[ep.EventID]...)
	}
	idx = append(idx, ix.byPoolRound[poolRound{ep.PoolInstanceID, ep.RoundIncarnationID}]...)
	idx = append(idx, ix.byPoolUnattributed[ep.PoolInstanceID]...)
	sort.Ints(idx)
	out := make([]rawSignal, 0, len(idx))
	for i, k := range idx {
		if i > 0 && idx[i-1] == k {
			continue
		}
		out = append(out, ix.signals[k])
	}
	return out
}

// classifySignals reads every fact once and records what it signals.
func classifySignals(recs []predictioneval.SourceRecord) []rawSignal {
	out := make([]rawSignal, 0, len(recs))
	// Recorded call starts, for orphan-return detection: keyed by the
	// attempt's full identity (epoch, session, pool, id) for automatic calls
	// — the automatic counter restarts per pool — and by round for calls
	// without one.
	autoStarts := map[attemptStartKey]bool{}
	otherStarts := map[roundTriple]bool{}
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
			switch {
			case r.PayloadUndecodable:
				sig.call, sig.callKind = true, CallKindAmbiguous
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
					autoStarts[attemptStartKey{r.CollectorEpoch, r.CollectorSessionID, r.PoolInstanceID, sig.attemptID}] = true
				} else {
					otherStarts[triple] = true
				}
			case r.Payload.Phase == predictioneval.PhaseCallReturned:
				started := false
				if sig.attemptID > 0 && !sig.manual {
					started = autoStarts[attemptStartKey{r.CollectorEpoch, r.CollectorSessionID, r.PoolInstanceID, sig.attemptID}]
				} else {
					started = otherStarts[triple]
				}
				if !started {
					sig.orphanReturn = true
				}
			default:
				// A placement fact in a phase outside the closed vocabulary
				// is a call of unknown shape at its own position.
				sig.call, sig.callKind = true, CallKindAmbiguous
			}
		case predictioneval.KindAutoDecision, predictioneval.KindUserTerminal, kindChannelEvent,
			kindScheduleDecision, kindUserPredictionMade, kindRoundCleanup, kindManualControl:
			if r.PayloadUndecodable {
				sig.undecodable = true
			}
		default:
			sig.unclassified = true
		}
		out = append(out, sig)
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
		sort.Slice(cs, func(a, b int) bool { return claimKey(cs[a]) < claimKey(cs[b]) })
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
	sort.Slice(invalid, func(a, b int) bool { return claimKey(invalid[a]) < claimKey(invalid[b]) })
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
