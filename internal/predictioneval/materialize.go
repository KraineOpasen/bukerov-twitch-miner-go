package predictioneval

import (
	"errors"
	"sort"
)

// Seam 1 of 4: from persisted facts to bounded per-attempt knowledge.
//
// This stage does no policy work at all. It groups facts into attempts by the
// discriminator the PRODUCER minted, bounds each attempt's causally-closed
// prefix, and digests that prefix — all BEFORE any model projection, so that
// what a replay is allowed to look at is fixed before anything wants to look
// at it.
//
// The cut it establishes is the one everything downstream depends on: facts up
// to and including an attempt's TERMINAL fact are common knowledge and may
// become inputs; facts after it are post-decision and may only reach [Score].
// The placement calls are the clearest case — they carry the same attempt id
// and are emitted after the decision, so grouping by id alone without ordering
// would quietly feed a bet's own outcome back into the decision that made it.

// ErrRecordsOutOfOrder reports a dataset whose facts are not in the causal
// order the store returns them in. Prefix-bounding is meaningless without it,
// so this is a caller contract violation rather than a data condition.
var ErrRecordsOutOfOrder = errors.New("predictioneval: source records are not in ascending causal order")

// Exclusion reasons. Closed vocabulary: a record or attempt that cannot become
// a decision case says exactly why, and never disappears silently.
const (
	// ExclusionForeignSession — the fact does not belong to the session whose
	// provenance the dataset carries. Reading it under that session's
	// classification would apply a verdict established for other rows.
	ExclusionForeignSession = "FOREIGN_SESSION_FACT"
	// ExclusionSessionIntegrityError — the store classified the whole session
	// as corrupt. Individual facts are not rehabilitated by looking sound.
	ExclusionSessionIntegrityError = "SESSION_INTEGRITY_ERROR"
	// ExclusionUnsupportedProducerRevision — written under a contract this
	// model does not replay.
	ExclusionUnsupportedProducerRevision = "UNSUPPORTED_PRODUCER_REVISION"
	// ExclusionLegacyProducerNoEnvelope — a readable pre-envelope fact. Its
	// decision inputs were never recorded, and this model does not
	// manufacture them from defaults, from the current configuration or from
	// a neighbouring fact.
	ExclusionLegacyProducerNoEnvelope = "LEGACY_PRODUCER_NO_ENVELOPE"
	// ExclusionUnsupportedPayloadVersion — the payload claims a version this
	// model is not bound to.
	ExclusionUnsupportedPayloadVersion = "UNSUPPORTED_PAYLOAD_VERSION"
	// ExclusionPayloadUndecodable — the row's columns survive but its payload
	// does not decode. Its absent fields are unreadable, not empty.
	ExclusionPayloadUndecodable = "PAYLOAD_UNDECODABLE"
	// ExclusionNoAttemptID — an auto fact carrying no attempt discriminator.
	// The scheduled-timer fact for a round that no longer exists is the
	// designed case: no attempt began, so there is nothing to replay.
	ExclusionNoAttemptID = "NO_ATTEMPT_ID"
	// ExclusionNoTerminalFact — the attempt has facts but no terminal one, so
	// its prefix has no end and its decision has no recorded result.
	ExclusionNoTerminalFact = "NO_TERMINAL_FACT"
	// ExclusionTerminalWithoutEnvelope — a terminal fact that carries no
	// envelope under a contract that should always attach one.
	ExclusionTerminalWithoutEnvelope = "TERMINAL_WITHOUT_ENVELOPE"
	// ExclusionMultipleTerminalFacts — two terminal facts for one attempt. One
	// attempt has one ending; two means the dataset is inconsistent.
	ExclusionMultipleTerminalFacts = "MULTIPLE_TERMINAL_FACTS"
	// ExclusionInconsistentRoundIncarnation — the attempt's facts disagree
	// about which admission of the round they describe.
	ExclusionInconsistentRoundIncarnation = "INCONSISTENT_ROUND_INCARNATION"
	// ExclusionOutcomeVectorOverCeiling — more outcomes than the store could
	// ever have persisted whole.
	ExclusionOutcomeVectorOverCeiling = "OUTCOME_VECTOR_OVER_CEILING"
)

// Dataset-level anomalies. These qualify the whole reading rather than one
// attempt, and are reported even when every attempt is otherwise usable.
const (
	AnomalyDuplicateCausalPosition = "DUPLICATE_CAUSAL_POSITION"
	AnomalySessionUnfinalized      = "SESSION_UNFINALIZED"
	AnomalySessionTruncated        = "SESSION_ADMINISTRATIVELY_TRUNCATED"
	AnomalySessionIntegrityError   = "SESSION_INTEGRITY_ERROR"
	AnomalyWitnessesUnchecked      = "WITNESSES_UNCHECKED"
	AnomalyNoWitnessesVerified     = "NO_WITNESSES_VERIFIED"
	AnomalyForeignProducerRevision = "FOREIGN_PRODUCER_REVISION"
	AnomalySessionLostFacts        = "SESSION_LOST_FACTS"
)

// AttemptKey identifies ONE automatic decision attempt.
//
// The attempt id alone is not an identity: it is a per-pool counter that
// restarts with the process, so two different runs both hold attempt 1. The
// pool instance and the collector session are what make it unique, and all of
// them travel together for exactly that reason.
type AttemptKey struct {
	CollectorEpoch     int64  `json:"collectorEpoch"`
	CollectorSessionID string `json:"collectorSessionId"`
	PoolInstanceID     string `json:"poolInstanceId"`
	AttemptID          uint64 `json:"attemptId"`
}

// Exclusion records one record or attempt that did not become a decision case.
type Exclusion struct {
	// Key is set when the exclusion is attributable to an identified attempt.
	Key *AttemptKey `json:"key,omitempty"`
	// ObservationID names the specific fact when the exclusion is about one.
	ObservationID string `json:"observationId,omitempty"`
	Reason        string `json:"reason"`
	Detail        string `json:"detail,omitempty"`
}

// AttemptKnowledge is everything known about one attempt, split at the causal
// cut and digested.
type AttemptKnowledge struct {
	Key AttemptKey `json:"key"`

	// Source is the session provenance this attempt was read under, and
	// Anomalies are the qualifications that apply to the whole reading.
	//
	// They are carried ON THE ATTEMPT rather than left on the enclosing
	// PairedKnowledge because the per-case [Scorecard] is the artifact that
	// gets stored and read later, and a scorecard that cannot name the session
	// it came from is indistinguishable from one produced against a truncated,
	// unwitnessed or foreign-revision dataset. That was a real defect: the
	// qualifications were computed correctly and then stranded one level above
	// the result that needed them.
	Source    SourceProvenance `json:"source"`
	Anomalies []string         `json:"anomalies,omitempty"`

	// RoundIncarnationID is the LOCAL admission of the round this attempt
	// resolved — not the Twitch event id, which a later admission can reuse.
	RoundIncarnationID string `json:"roundIncarnationId"`
	EventID            string `json:"eventId"`

	// RoundCaptureOrigin / RoundCaptureGapCause are the round's frozen
	// admission provenance. A round admitted while capture was incomplete
	// cannot have its missing prefix invented, so this travels with the case
	// and is reported in the scorecard.
	RoundCaptureOrigin   string `json:"roundCaptureOrigin"`
	RoundCaptureGapCause string `json:"roundCaptureGapCause"`

	// CommonInputSlice is the causally-closed prefix: every fact of this
	// attempt up to AND INCLUDING its terminal fact, in causal order.
	CommonInputSlice []SourceRecord `json:"commonInputSlice"`
	// TerminalIndex is the position of the terminal fact within the slice. It
	// is always the last element; the field exists so a reader never has to
	// assume that.
	TerminalIndex int `json:"terminalIndex"`
	// CommonInputDigest witnesses the slice above. Facts appended to the store
	// afterwards cannot reach it.
	CommonInputDigest string `json:"commonInputDigest"`

	// PostDecision are this attempt's facts AFTER the terminal fact —
	// placement calls and anything later. They are visible to [Score] and to
	// nothing else.
	PostDecision []SourceRecord `json:"postDecision,omitempty"`

	// SawDueFact records whether the attempt's opening fact survives. Its
	// absence does not invalidate the case; it does mean the prefix is
	// partial, and a reader is told rather than left to assume.
	SawDueFact bool `json:"sawDueFact"`
	// DueReason is the producer's own claim about whether the decision was
	// about the round it was scheduled for: OK, CONFLICT, or empty when the
	// caller carried no scheduled coordinate and made no claim.
	DueReason string `json:"dueReason,omitempty"`
}

// PairedKnowledge is the whole materialized reading of one dataset.
type PairedKnowledge struct {
	Model  ModelProvenance  `json:"model"`
	Source SourceProvenance `json:"source"`

	Attempts []AttemptKnowledge `json:"attempts"`
	Excluded []Exclusion        `json:"excluded,omitempty"`
	// Anomalies qualify the whole reading. They never silently downgrade an
	// attempt; they are reported beside it.
	Anomalies []string `json:"anomalies,omitempty"`
}

// MaterializePairedKnowledge is seam 1: it groups a dataset's facts into
// attempts and bounds each attempt's common-knowledge slice.
//
// It is pure. It reads no database, consults no clock, and returns a value
// that depends on nothing but its argument.
func MaterializePairedKnowledge(ds SourceDataset) (PairedKnowledge, error) {
	out := PairedKnowledge{
		Model:  CurrentModelProvenance(),
		Source: ds.Source,
	}

	if err := checkCausalOrder(ds.Records); err != nil {
		return PairedKnowledge{}, err
	}
	out.Anomalies = sessionAnomalies(ds)

	// A session the store calls corrupt does not yield cases. The individual
	// rows may look intact — that is exactly the situation the classification
	// exists to overrule.
	if ds.Source.SessionReading == readingIntegrityError {
		out.Excluded = append(out.Excluded, Exclusion{
			Reason: ExclusionSessionIntegrityError,
			Detail: ds.Source.SessionDetail,
		})
		return out, nil
	}

	// The producer revision BINDS, it does not merely annotate.
	//
	// A revision this model does not know may have changed what a field means —
	// recorded a post-gate stake on an exit that previously had none, or
	// redefined the stake gate's return. Replaying it under obs-v2 invariants
	// would produce confident AGREE/DISAGREE verdicts against a contract nobody
	// re-read, which is worse than producing none. So it yields no cases.
	//
	// The pre-envelope contract is NOT in that category: it is known, and it is
	// known to carry no envelope, so it stays readable and says exactly that.
	switch {
	case ds.Source.ProducerRevision == SupportedProducerRevision:
	case isLegacyProducer(ds.Source.ProducerRevision):
	default:
		out.Excluded = append(out.Excluded, Exclusion{
			Reason: ExclusionUnsupportedProducerRevision,
			Detail: ds.Source.ProducerRevision,
		})
		return out, nil
	}

	groups := map[AttemptKey][]SourceRecord{}
	var order []AttemptKey

	// A causal position belongs to ONE collector run, so the key is the
	// (epoch, sequence) pair rather than the sequence alone. Two runs both
	// number their facts from 1, so keying on the sequence would report every
	// multi-epoch dataset as internally duplicated — a loud integrity anomaly
	// raised by nothing more than two sessions being read together.
	//
	// It is also checked AFTER the foreign-session filter, so a fact this
	// dataset does not own cannot raise an anomaly about the one that does.
	type causalPosition struct {
		epoch    int64
		sequence int64
	}
	seenPosition := map[causalPosition]bool{}

	for _, r := range ds.Records {
		if r.CollectorEpoch != ds.Source.CollectorEpoch ||
			r.CollectorSessionID != ds.Source.CollectorSessionID {
			out.Excluded = append(out.Excluded, Exclusion{
				ObservationID: r.ObservationID,
				Reason:        ExclusionForeignSession,
			})
			continue
		}

		pos := causalPosition{epoch: r.CollectorEpoch, sequence: r.CollectorSequence}
		if seenPosition[pos] {
			out.Anomalies = appendOnce(out.Anomalies, AnomalyDuplicateCausalPosition)
		}
		seenPosition[pos] = true

		if r.Kind != KindAutoDecision && r.Kind != KindPlacement {
			// Not part of an automatic decision attempt. Silently irrelevant
			// rather than excluded: a channel_event fact is not a decision
			// that failed to be readable.
			continue
		}
		if r.PayloadUndecodable {
			out.Excluded = append(out.Excluded, Exclusion{
				ObservationID: r.ObservationID,
				Reason:        ExclusionPayloadUndecodable,
			})
			continue
		}
		if r.PayloadVersion != SupportedPayloadVersion {
			out.Excluded = append(out.Excluded, Exclusion{
				ObservationID: r.ObservationID,
				Reason:        ExclusionUnsupportedPayloadVersion,
			})
			continue
		}
		id, ok := attemptIDOf(r)
		if !ok {
			// Under the pre-envelope contract NO auto fact carried the
			// discriminator, because the producer minted none. Reporting that
			// as NO_ATTEMPT_ID would tell an operator no attempt occurred,
			// when in fact a full decision occurred under a contract that did
			// not record its inputs — the opposite meaning.
			reason := ExclusionNoAttemptID
			if isLegacyProducer(ds.Source.ProducerRevision) && r.Kind == KindAutoDecision {
				reason = ExclusionLegacyProducerNoEnvelope
			}
			out.Excluded = append(out.Excluded, Exclusion{
				ObservationID: r.ObservationID,
				Reason:        reason,
				Detail:        r.Payload.Phase + "/" + r.Payload.ReasonCode,
			})
			continue
		}
		key := AttemptKey{
			CollectorEpoch:     r.CollectorEpoch,
			CollectorSessionID: r.CollectorSessionID,
			PoolInstanceID:     r.PoolInstanceID,
			AttemptID:          id,
		}
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		groups[key] = append(groups[key], r)
	}

	// Deterministic output order, independent of map iteration.
	sort.Slice(order, func(i, j int) bool { return attemptKeyLess(order[i], order[j]) })

	for _, key := range order {
		attempt, excl := materializeAttempt(key, groups[key], ds.Source, out.Anomalies)
		out.Excluded = append(out.Excluded, excl...)
		if attempt != nil {
			out.Attempts = append(out.Attempts, *attempt)
		}
	}
	return out, nil
}

// readingIntegrityError mirrors analytics.ReadingIntegrityError. It is
// re-declared for the reasons in records.go and pinned by the reader's
// vocabulary test.
const (
	readingIntegrityError = "INTEGRITY_ERROR"
	readingUnfinalized    = "UNFINALIZED"
	readingTruncated      = "ADMINISTRATIVELY_TRUNCATED"
	readingAsFinalized    = "AS_FINALIZED"
)

// materializeAttempt bounds one attempt's prefix and digests it.
func materializeAttempt(key AttemptKey, recs []SourceRecord, src SourceProvenance, anomalies []string) (*AttemptKnowledge, []Exclusion) {
	var excl []Exclusion

	terminal := -1
	terminals := 0
	for i, r := range recs {
		if isTerminalFact(r) {
			terminals++
			if terminal < 0 {
				terminal = i
			}
		}
	}
	if terminals > 1 {
		return nil, append(excl, Exclusion{Key: &key, Reason: ExclusionMultipleTerminalFacts})
	}
	if terminal < 0 {
		// Distinguish "the attempt has no ending" from "its ending carries no
		// envelope": they are different failures and a reader acts on them
		// differently.
		reason := ExclusionNoTerminalFact
		for _, r := range recs {
			if r.Kind == KindAutoDecision && isTerminalPhase(r.Payload.Phase) {
				reason = ExclusionTerminalWithoutEnvelope
				break
			}
		}
		if reason == ExclusionTerminalWithoutEnvelope && isLegacyProducer(src.ProducerRevision) {
			// Under the pre-envelope contract an absent envelope is the
			// contract, not a defect.
			reason = ExclusionLegacyProducerNoEnvelope
		}
		return nil, append(excl, Exclusion{Key: &key, Reason: reason})
	}

	slice := recs[:terminal+1]
	post := recs[terminal+1:]

	// Every fact of one attempt describes one admission of one round. Facts
	// that disagree mean the grouping crossed a boundary it should not have.
	incarnation := slice[terminal].RoundIncarnationID
	for _, r := range slice {
		if r.RoundIncarnationID != incarnation {
			return nil, append(excl, Exclusion{
				Key:    &key,
				Reason: ExclusionInconsistentRoundIncarnation,
			})
		}
	}

	env := slice[terminal].Payload.DecisionEnvelope
	if env != nil && len(env.Outcomes) > PinnedMaxOutcomes {
		return nil, append(excl, Exclusion{
			Key:    &key,
			Reason: ExclusionOutcomeVectorOverCeiling,
		})
	}

	out := &AttemptKnowledge{
		Key:                  key,
		Source:               src,
		Anomalies:            append([]string(nil), anomalies...),
		RoundIncarnationID:   incarnation,
		EventID:              slice[terminal].EventID,
		RoundCaptureOrigin:   slice[terminal].RoundCaptureOrigin,
		RoundCaptureGapCause: slice[terminal].RoundCaptureGapCause,
		CommonInputSlice:     deepCopyRecords(slice),
		TerminalIndex:        terminal,
		PostDecision:         deepCopyRecords(post),
	}
	for _, r := range slice {
		if r.Kind == KindAutoDecision && r.Payload.Phase == PhaseAutoDue {
			out.SawDueFact = true
			out.DueReason = r.Payload.ReasonCode
		}
	}
	out.CommonInputDigest = commonInputDigest(key.AttemptID, out.CommonInputSlice)
	return out, excl
}

// isTerminalFact reports the ONE fact that ends an attempt and carries its
// envelope.
func isTerminalFact(r SourceRecord) bool {
	return r.Kind == KindAutoDecision &&
		isTerminalPhase(r.Payload.Phase) &&
		r.Payload.DecisionEnvelope != nil
}

func isTerminalPhase(phase string) bool {
	return phase == PhaseAutoDecided || phase == PhaseAutoSkipped
}

func isLegacyProducer(revision string) bool {
	return len(revision) >= len(LegacyProducerRevisionPrefix) &&
		revision[:len(LegacyProducerRevisionPrefix)] == LegacyProducerRevisionPrefix
}

// attemptIDOf reads the minted discriminator. A zero value is not an attempt
// id: the producer's counter starts at 1, so zero means the key was absent.
func attemptIDOf(r SourceRecord) (uint64, bool) {
	v, ok := r.Payload.Counters[CounterAutoAttemptID]
	if !ok || v <= 0 {
		return 0, false
	}
	return uint64(v), true
}

func attemptKeyLess(a, b AttemptKey) bool {
	if a.CollectorEpoch != b.CollectorEpoch {
		return a.CollectorEpoch < b.CollectorEpoch
	}
	if a.CollectorSessionID != b.CollectorSessionID {
		return a.CollectorSessionID < b.CollectorSessionID
	}
	if a.PoolInstanceID != b.PoolInstanceID {
		return a.PoolInstanceID < b.PoolInstanceID
	}
	return a.AttemptID < b.AttemptID
}

// checkCausalOrder rejects a dataset whose facts are not ascending. Bounding a
// prefix by position is only meaningful over an ordered sequence.
func checkCausalOrder(recs []SourceRecord) error {
	for i := 1; i < len(recs); i++ {
		prev, cur := recs[i-1], recs[i]
		if cur.CollectorEpoch < prev.CollectorEpoch {
			return ErrRecordsOutOfOrder
		}
		if cur.CollectorEpoch == prev.CollectorEpoch && cur.CollectorSequence < prev.CollectorSequence {
			return ErrRecordsOutOfOrder
		}
	}
	return nil
}

// sessionAnomalies turns the store's own session classification into the
// qualifications a reader must carry. None of them silently invalidates a
// case; every one of them is reported beside it.
func sessionAnomalies(ds SourceDataset) []string {
	var out []string
	switch ds.Source.SessionReading {
	case readingUnfinalized:
		out = append(out, AnomalySessionUnfinalized)
	case readingTruncated:
		out = append(out, AnomalySessionTruncated)
	case readingIntegrityError:
		out = append(out, AnomalySessionIntegrityError)
	}
	if ds.Source.WitnessesUnchecked > 0 {
		out = append(out, AnomalyWitnessesUnchecked)
	}
	if ds.Source.WitnessesVerified == 0 && ds.Source.FactsPresent > 0 {
		// A digest nobody recomputed witnesses nothing. Saying so is the
		// difference between "verified" and "verifiable".
		out = append(out, AnomalyNoWitnessesVerified)
	}
	if ds.Source.ProducerRevision != SupportedProducerRevision {
		out = append(out, AnomalyForeignProducerRevision)
	}
	if ds.Source.DroppedCount > 0 {
		out = append(out, AnomalySessionLostFacts)
	}
	return out
}

func appendOnce(list []string, v string) []string {
	for _, existing := range list {
		if existing == v {
			return list
		}
	}
	return append(list, v)
}

// deepCopyRecords copies the records AND the reference types inside their
// payloads.
//
// A plain slice copy is not enough: SourcePayload carries a Counters map and a
// DecisionEnvelope pointer, so a shallow copy leaves the materialized slice
// aliasing the caller's dataset. Nothing in the four seams mutates, and the
// reader builds a fresh value per load, so no live failure exists — but the
// common-input digest deliberately does not hash payload contents, delegating
// that to the store's row witness, which is checked BEFORE materialization. An
// alias would therefore let a post-materialize edit change what a replay reads
// while its digest stayed byte-identical, and that is exactly the property this
// package sells.
func deepCopyRecords(in []SourceRecord) []SourceRecord {
	if len(in) == 0 {
		return nil
	}
	out := make([]SourceRecord, len(in))
	for i, r := range in {
		r.Payload = deepCopyPayload(r.Payload)
		out[i] = r
	}
	return out
}

func deepCopyPayload(p SourcePayload) SourcePayload {
	if p.Manual != nil {
		v := *p.Manual
		p.Manual = &v
	}
	if p.OutcomeSlot != nil {
		v := *p.OutcomeSlot
		p.OutcomeSlot = &v
	}
	if p.Counters != nil {
		counters := make(map[string]int64, len(p.Counters))
		for k, v := range p.Counters {
			counters[k] = v
		}
		p.Counters = counters
	}
	p.DecisionEnvelope = deepCopyEnvelope(p.DecisionEnvelope)
	p.AdmissionSettings = deepCopySettings(p.AdmissionSettings)
	return p
}

func deepCopyEnvelope(e *SourceDecisionEnvelope) *SourceDecisionEnvelope {
	if e == nil {
		return nil
	}
	c := *e
	c.Settings = deepCopySettings(e.Settings)
	c.Balance = copyInt64(e.Balance)
	c.BetTotalUsers = copyInt64(e.BetTotalUsers)
	c.BetTotalPoints = copyInt64(e.BetTotalPoints)
	c.ChoiceIndex = copyIntPtr(e.ChoiceIndex)
	c.ChoiceAmount = copyInt64(e.ChoiceAmount)
	c.SkipResult = copyBoolPtr(e.SkipResult)
	c.SkipCompared = copyFloatPtr(e.SkipCompared)
	c.RiskMaxStakePercent = copyIntPtr(e.RiskMaxStakePercent)
	c.RiskReservePoints = copyIntPtr(e.RiskReservePoints)
	c.StakeAllowed = copyInt64(e.StakeAllowed)
	c.StakeLimit = copyInt64(e.StakeLimit)
	c.ClampApplied = copyBoolPtr(e.ClampApplied)
	c.FinalAmount = copyInt64(e.FinalAmount)
	if e.Outcomes != nil {
		c.Outcomes = append([]SourceModelOutcome(nil), e.Outcomes...)
	}
	return &c
}

func deepCopySettings(s *SourceBetSettings) *SourceBetSettings {
	if s == nil {
		return nil
	}
	c := *s
	if s.FilterCondition != nil {
		fc := *s.FilterCondition
		c.FilterCondition = &fc
	}
	return &c
}

func copyIntPtr(v *int) *int {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}

func copyBoolPtr(v *bool) *bool {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}

func copyFloatPtr(v *float64) *float64 {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}
