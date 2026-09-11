package predictioneval

import (
	"math"
	"strconv"
)

// THE ORDERED-RULES MECHANISM.
//
// One pure function, [EvaluateOrderedRules], re-deriving the donor's
// ordered-rules decision over a projected stream. The shape is the donor's own,
// and every part of that shape is load-bearing:
//
//	for each candidate:                 # each is one entry into the logic
//	  if fewer than two outcomes: next candidate
//	  T = checked sum of the outcome points
//	  for each outcome, IN VECTOR ORDER:          # OUTCOME is the outer loop
//	    p = 1 / (T / points)                      # reciprocal, not points/T
//	    for each detailed rule, IN CONFIG ORDER:  # RULE is the inner loop
//	      if comparator does not match: next rule, NO draw
//	      if participation draw succeeds: CHOOSE this outcome, this rule's points
//	      else: next rule           # a failed draw FALLS THROUGH, never breaks
//	    if no rule admitted and p is within THIS outcome's default bounds:
//	      CHOOSE this outcome, the default's points, consuming NO draw
//	    else: next outcome
//
// Four inversions of that shape each produce a different, plausible, wrong
// model, and each has its own falsifier:
//
//   - rules outside, outcomes inside — TestOrderedRulesOutcomeBeforeRulePriority;
//   - the default applied once after all outcomes rather than per outcome —
//     TestOrderedRulesDefaultPreemptsLaterOutcomeDetailed;
//   - a failed draw ending the rule scan instead of falling through —
//     TestOrderedRulesFailedDrawFallsThroughOverlap;
//   - the pool share computed as a direct share instead of the reciprocal —
//     TestOrderedRulesReciprocalRatioGe90Boundary.
//
// # Contract checks run over the WHOLE input, before the mechanism
//
// Worth stating plainly, because it looks at first like a hole in the
// no-look-ahead guarantee. The contract and resource checks — identity, order,
// declared interval, presence vocabulary, string and count ceilings — are
// applied to everything the caller supplied, INCLUDING facts sitting past the
// factual boundary. So appending a malformed post-boundary candidate can turn a
// projection that previously succeeded into a refusal, and appending enough
// entropy to pass the word ceiling can turn a decided run into one.
//
// That is deliberate and it is not the model reading post-treatment data. The
// distinction is what the check is FOR: a size or contract check asks "will
// this package evaluate this input at all", and its answer is REFUSED — a
// status that is explicitly not a mechanism result. The mechanism itself never
// sees a fact past the boundary, and no refusal can become an admission, a
// stake, or a NO_ATTEMPT. What the no-look-ahead guarantee actually forbids is
// a later fact changing a DECISION, and that is what
// TestOrderedRulesAppendingPostCutoffFactsCannotChangeEvaluation pins.
//
// # Where this model STOPS, and why it is not zero
//
// The traversal ends at the EARLIEST of: the first admission with a computable
// stake; the first admission whose stake is not computable; the projected
// factual boundary, which the stream already applied; a required input it
// actually reached and could not read; or a resource bound.
//
// After an admission the model stops rather than continuing, because everything
// past that point belongs to a world in which this bet was never placed. There
// is no counterfactual placement success, no retry, no pool mutation and no
// balance update here — none of those transitions is established by anything
// this package can read, and inventing them would turn a bounded mechanism into
// an unbounded simulation.
//
// A stop with an unknown stake is NOT a stake of zero and NOT an abstention.
// The model knows WHICH outcome it would have bet on and does not know HOW
// MUCH; those are different facts and are reported as different fields. The
// four vectors in which a missing balance must behave differently are pinned by
// TestOrderedRulesMissingBalanceFailedDrawContinues,
// TestOrderedRulesMissingBalanceNoMatchContinues,
// TestOrderedRulesMissingBalanceAdmittedDrawStopsWithUnknownStake and
// TestOrderedRulesMissingBalanceDefaultStopsWithoutDraw.

// Donor arithmetic constants, mirrored exactly.
const (
	// bernoulliAlwaysTrue is rand 0.8.5's ALWAYS_TRUE sentinel: u64::MAX. A
	// distribution holding it returns true WITHOUT drawing, which is why a
	// participation rate of exactly one consumes no entropy.
	bernoulliAlwaysTrue = ^uint64(0)
	// bernoulliScale is rand 0.8.5's SCALE: 2^64, written as the float64 it is.
	bernoulliScale = 18446744073709551616.0
	// u32Ceiling is 2^32, the first float64 above the u32 domain.
	u32Ceiling = 4294967296.0
	// percentDivisor is the donor's single normalization step.
	percentDivisor = 100.0
)

// ReasonStreamDigestMismatch is a stream whose contents no longer match the
// selection digest its projection computed — a stream mutated after its
// boundary was established.
const ReasonStreamDigestMismatch = "STREAM_SELECTION_DIGEST_MISMATCH"

// ReasonSuppliedTextNotEncodable is a supplied identifier that would not
// survive its own JSON round trip. It names the config-and-trace side
// specifically: the stream's own text is refused by the validators the
// invariant pass calls, but ConfigID and RunID are read by no validator at all,
// so this gate is the only place that can refuse them.
const ReasonSuppliedTextNotEncodable = "SUPPLIED_TEXT_NOT_ENCODABLE"

// OrderedRulesCandidateVerdict is what the traversal did with one candidate.
type OrderedRulesCandidateVerdict string

const (
	// CandidateNoMatch is a candidate fully traversed with nothing admitted.
	// The traversal continues to the next candidate: repeated values at
	// different source positions are genuinely new opportunities.
	CandidateNoMatch OrderedRulesCandidateVerdict = "NO_MATCH"
	// CandidateTooFewOutcomes is a pool the donor declines to bet on at all.
	CandidateTooFewOutcomes OrderedRulesCandidateVerdict = "TOO_FEW_OUTCOMES"
	// CandidateAdmitted is a candidate on which participation was admitted.
	CandidateAdmitted OrderedRulesCandidateVerdict = "ADMITTED"
	// CandidateUnknownInput is a candidate carrying a required input the
	// traversal reached and could not read.
	CandidateUnknownInput OrderedRulesCandidateVerdict = "UNKNOWN_INPUT"
	// CandidateRefused is a candidate at which a resource bound was reached.
	CandidateRefused OrderedRulesCandidateVerdict = "REFUSED"
)

// OrderedRulesBalanceUse is how the model treated ONE candidate's balance.
//
// This exists because "the balance was missing" is four different pieces of
// evidence depending on whether the model ever needed it, and collapsing them
// would lose the distinction the four missing-balance vectors turn on.
type OrderedRulesBalanceUse string

const (
	// BalanceNotEvaluated is a balance the model never needed, because nothing
	// on this candidate admitted. It is NOT "missing": the model did not look.
	BalanceNotEvaluated OrderedRulesBalanceUse = "NOT_EVALUATED"
	// BalanceUsed is a known balance actually used to size a stake.
	BalanceUsed OrderedRulesBalanceUse = "USED"
	// BalanceRequiredButMissing is admission reached with no balance supplied.
	BalanceRequiredButMissing OrderedRulesBalanceUse = "REQUIRED_BUT_MISSING"
	// BalanceRequiredButInvalid is admission reached with a balance the caller
	// declared unusable.
	BalanceRequiredButInvalid OrderedRulesBalanceUse = "REQUIRED_BUT_INVALID"
	// BalanceRequiredButOutOfDomain is admission reached with a balance outside
	// the donor's u32 stake domain.
	BalanceRequiredButOutOfDomain OrderedRulesBalanceUse = "REQUIRED_BUT_OUT_OF_DOMAIN"
)

// OrderedRulesCandidateVisit records what happened at one candidate.
//
// It is per-candidate rather than per-run because the run-level status only
// describes the STOP, and three of the four missing-balance vectors are about
// candidates the traversal walked THROUGH.
type OrderedRulesCandidateVisit struct {
	CandidateIndex    int                          `json:"candidateIndex"`
	CandidateIdentity string                       `json:"candidateIdentity"`
	CandidatePosition int64                        `json:"candidatePosition"`
	Verdict           OrderedRulesCandidateVerdict `json:"verdict"`
	BalanceUse        OrderedRulesBalanceUse       `json:"balanceUse"`
	// PoolTotalKnown separates a pool that summed to zero from one that could
	// not be summed at all.
	PoolTotalKnown bool  `json:"poolTotalKnown"`
	PoolTotal      int64 `json:"poolTotal"`
	// RawWordsConsumedHere is how much entropy this candidate alone spent.
	RawWordsConsumedHere int `json:"rawWordsConsumedHere"`
}

// normalized* are the config AFTER its single division by 100.
//
// They are unexported on purpose. The exported contract accepts raw
// percentages only, so there is no way for a caller to hand in an
// already-normalized config and have it divided a second time — which would
// silently turn a 50% participation rate into 0.5%. See
// TestOrderedRulesNormalizeExactlyOnce.
type normalizedPoints struct {
	maxValue uint32
	percent  float64
}

type normalizedRule struct {
	comparator  OrderedRuleComparator
	threshold   float64
	attemptRate float64
	points      normalizedPoints
}

type normalizedConfig struct {
	detailed  []normalizedRule
	defMin    float64
	defMax    float64
	defPoints normalizedPoints
}

// orderedRulesInputCountReason refuses an input whose COUNTS already exceed a
// declared bound. It is the first half of the shape gate; the text budget is
// orderedRulesInputTextReason below.
//
// Every check here is O(1) per element over a number of elements the preceding
// checks have already bounded, so the work this function can be made to do is
// itself bounded.
//
// IT IS NOT "the only thing that runs before the digests", which is what this
// comment said until a reviewer read the caller instead of the comment. By the
// time orderedRulesStreamDigest runs, EvaluateOrderedRules has also performed
// three bounded version comparisons, the text budget, config normalization, the
// structural walk, the cutoff encodability scan, qualification derivation and
// both identity passes. The ordering of those tiers is used as a security
// property throughout this package, so a sentence that hides seven of them is
// not a stale nicety — it misdescribes exactly the thing the reader came here
// to check. The name in the first line was stale too: this function was split
// in two and the doc kept the old one.
//
// The byte sum covers the text the three digests actually read. It uses
// MaxOrderedRulesAggregateBytes rather than a new budget, because that is the
// figure the projection already charges — an input the projection would have
// admitted must not be refused here, and one it would have refused must not be
// hashed here.
func orderedRulesInputCountReason(s OrderedRulesStream, cfg OrderedRulesConfig, d SuppliedDrawTrace) string {
	switch {
	case len(d.Words) > MaxOrderedRulesDrawWords:
		return ReasonDrawWordsOverBound
	case len(cfg.Detailed) > MaxOrderedRulesRules:
		return ReasonRuleCountOverBound
	case len(s.Candidates) > MaxOrderedRulesCandidates,
		len(s.Qualifications) > MaxOrderedRulesQualifications,
		len(s.Admission.SourceReferences) > MaxOrderedRulesSourceReferences:
		return ReasonStreamShapeOverBound
	}
	for i := range s.Candidates {
		if len(s.Candidates[i].Outcomes) > MaxOrderedRulesOutcomes {
			return ReasonStreamShapeOverBound
		}
	}
	return ""
}

// orderedRulesInputTextReason is the gate's SECOND half: the text budget.
//
// It was one function with the counts above until a reviewer pointed out what
// that cost. The budget walks every candidate, every outcome and every
// intervention to total their lengths — count-bounded work, but work the caller
// chooses the size of — and it ran BEFORE the three comparisons against
// constants that decide an input is not evaluable at all. So an unsupported
// contract version paid for the whole walk to be told a nine-byte constant did
// not match. The split exists so those comparisons can sit between the counts
// and this, which is the same arrangement ProjectOrderedRulesStream reached one
// commit earlier for the same field, found by the same reviewer.
func orderedRulesInputTextReason(s OrderedRulesStream, cfg OrderedRulesConfig, d SuppliedDrawTrace) string {
	// One walk, two limits. Both are the projection's own: a single string past
	// MaxOrderedRulesIdentifierBytes, and the retained total past
	// MaxOrderedRulesAggregateBytes. The per-string one is not implied by the
	// aggregate — a 64 MiB provenance note sits well inside a 128 MiB budget
	// while being a value the projection refuses outright — so checking only
	// the total left exactly that input to be hashed in full.
	budget := orderedRulesInputBudget(s, cfg, d)
	switch {
	case budget.overLong:
		return ReasonStreamTextOverBound
	// The projection charges every byte at its worst-case ENCODED width and
	// withholds the structural reserve, so this must too. Comparing the raw
	// total here left a sixfold gap: a forged stream carrying 32 MiB of valid
	// provenance passed every check, while ProjectOrderedRulesStream refuses
	// that same source outright — and a stream this gate admits can still be
	// serialized, which is the cost the charged width exists to bound.
	//
	// Mirroring it exactly also keeps the invariant it was introduced for: the
	// two sides now apply the SAME condition, so a source the projection
	// admitted is admitted here by construction rather than by a margin.
	case budget.bytes*orderedRulesMaxJSONExpansion > orderedRulesTextCeiling,
		budget.other > MaxOrderedRulesAggregateBytes:
		return ReasonStreamBytesOverBound
	}
	return ""
}

// orderedRulesUnreadRefusal is a refusal that attests to NOTHING about the
// input, because it declined to read it.
//
// It carries no whole-input digest and no supplied text — not the boundary, not
// anything else the caller handed in. A digest of something never traversed
// would be the same empty guarantee this refusal exists to avoid, and
// re-exporting text that passed no bound would move the cost of refusing onto
// whoever encodes the result.
func orderedRulesUnreadRefusal(reason string) OrderedRulesEvaluation {
	return OrderedRulesEvaluation{
		EvidenceLabel:           OrderedRulesEvidenceLabel,
		ModelVersion:            OrderedRulesModelVersion,
		DonorRevision:           OrderedRulesDonorRevision,
		EntropySemanticsVersion: OrderedRulesEntropySemanticsVersion,
		Participation:           ParticipationNotAdmitted,
		Stake:                   SuppliedUint32{Presence: SuppliedMissing, Reason: ReasonBalanceNotEvaluated},
		Status:                  StatusRefused,
		Reason:                  reason,
	}
}

// orderedRulesTextBudget accumulates the retained text the whole-input digests
// read, and records whether any single string broke the per-string limit.
//
// The split between its methods mirrors the projection field for field, so an
// input the projection would have admitted is not refused here and one it would
// have refused is not hashed here.
//
// There used to be a third method, for retained strings the projection bounded
// only through the aggregate. It has no callers left, because the projection no
// longer has such strings: the four scope strings and three admission strings
// that were merely charged gained individual bounds, and this side followed.
// That is worth saying rather than quietly deleting — the category existing at
// all was what let a namespace just under the aggregate be hashed in full
// before anything refused it.
type orderedRulesTextBudget struct {
	// bytes is the retained STREAM text, charged exactly as the projection
	// charges it so the two ceilings agree.
	bytes int64
	// other is the config and trace text, which the projection never saw and
	// which therefore cannot share the stream's ceiling.
	other    int64
	overLong bool
}

// bound adds a string the projection ALSO bounds individually, with
// checkIdentifier or checkFreeText.
func (b *orderedRulesTextBudget) bound(v ...string) {
	for _, s := range v {
		b.boundOnly(s)
		b.bytes += int64(len(s))
	}
}

// boundOnly limits a string's length without charging it to the aggregate.
//
// It exists for text the projection DERIVES rather than receives: the
// qualifications it generates, the boundary it computes, and the stream's own
// contract version. The projection never charged those against its budget, so
// charging them here would let a source the projection ADMITTED be refused by
// the evaluator for bytes — which is the invariant backwards. They are still
// length-bounded, and their count is bounded, so the work they can cause is
// bounded without them touching the shared ceiling.
func (b *orderedRulesTextBudget) boundOnly(v ...string) {
	for _, s := range v {
		if len(s) > MaxOrderedRulesIdentifierBytes {
			b.overLong = true
		}
	}
}

// orderedRulesInputBudget walks the retained text the whole-input digests read.
//
// It is called only after the count bounds above have passed, so the loops are
// bounded; every term is a length read, never a copy, so even a single enormous
// string costs one word here rather than a traversal.
func orderedRulesInputBudget(s OrderedRulesStream, cfg OrderedRulesConfig,
	d SuppliedDrawTrace) orderedRulesTextBudget {
	var b orderedRulesTextBudget
	// VOCABULARY IS BOUND, not merely charged. A closed set of short words has
	// no large member, and an enormous one is an input whose only effect is the
	// cost of handling it — which here is the digests that run next and the
	// projection validators the invariant pass calls, one of which quotes what
	// it rejects. Measured before this gate, a 64 MiB contract version cost
	// about two seconds across the two calls a forgery needs.
	//
	// This mirrors the projection field for field, as the division always has.
	// The projection gates its vocabulary now, so this side must too: leaving
	// these on the aggregate would refuse nothing the projection refuses.
	b.boundOnly(s.ContractVersion)
	b.bound(s.Scope.SourceContractVersion, string(s.Scope.Coverage))
	// BOUND, not merely charged. The projection refuses each of these above
	// MaxOrderedRulesIdentifierBytes, so charging them alone left a namespace
	// just under the aggregate to pass this gate, be hashed by all three
	// whole-input digests, and only then be refused by the invariant pass that
	// calls validateScope — measured at 646 ms and 251 MB allocated to say no,
	// which is precisely the length-before-work protection this gate exists to
	// provide.
	b.bound(s.Scope.Namespace, s.Scope.EpisodeID, s.Scope.AccountContext,
		s.Scope.AssociationEvidence)
	b.bound(s.Scope.CoverageDetail, string(s.Admission.ViewKind))
	b.bound(s.Admission.ManifestID, s.Admission.Population, s.Admission.OrderBasis)
	b.bound(s.Admission.SourceReferences...)
	// Kind and Basis are labels the projection DERIVES, so they are bounded and
	// not charged. The identity is not derived: the projection copies it from a
	// supplied intervention identity, and charges those bytes. Excluding it let
	// a forged stream carry text no source could have supplied and still pass
	// this gate. Charging it stays inside the projection's own total, because a
	// stream with an established cutoff had at least the one intervention whose
	// identity it carries, and the projection charged every intervention.
	b.boundOnly(string(s.Cutoff.Kind), string(s.Cutoff.Basis))
	b.bound(s.Cutoff.Identity)
	// The REMOVALS are charged at their floor for the declared view, for the
	// same reason the cutoff identity above is charged: the stream asserts a source, and the
	// projection charged that source in full. Every removed candidate cost it
	// at least orderedRulesDroppedCandidateMinimumBytes, so leaving the claim
	// free let a forgery sit within 192 charged bytes per claimed removal of
	// the ceiling and pass a gate whose entire purpose is to refuse what the
	// projection refuses.
	//
	// Clamped, and read only when positive, because this runs BEFORE
	// orderedRulesCutoffImpossible: at this point the count is whatever the
	// caller wrote. A negative one would otherwise BUY budget, and a count near
	// the integer maximum would overflow the multiplication into one. Clamping
	// only ever lowers the charge, and a stream claiming more removals than the
	// candidate ceiling is refused by the invariant pass regardless.
	if dropped := s.Cutoff.DroppedAtOrAfter; dropped > 0 {
		if dropped > MaxOrderedRulesCandidates {
			dropped = MaxOrderedRulesCandidates
		}
		b.bytes += int64(dropped) * orderedRulesDroppedCandidateMinimumBytes(s.Admission.ViewKind)
	}
	b.boundOnly(s.Qualifications...)
	for i := range s.Candidates {
		c := &s.Candidates[i]
		b.bound(c.Identity, c.Provenance, c.OutcomesReason,
			string(c.SourceKind), string(c.EpisodeMembership), string(c.OutcomesPresence),
			string(c.Balance.Presence), c.Balance.Provenance, c.Balance.Reason)
		for j := range c.Outcomes {
			o := &c.Outcomes[j]
			b.bound(o.Identity, o.Points.Provenance, o.Points.Reason, string(o.Points.Presence))
		}
	}
	// The config and the trace have no projection to mirror, so only their
	// closed vocabularies are bound; their free-form identifiers stay on the
	// aggregate, because inventing a limit nothing else applies would refuse
	// input no other rule refuses.
	//
	// They are charged to a SEPARATE total. The projection never saw them, so
	// adding their bytes to the stream's would again let a stream the
	// projection admitted be refused here — this time because of an unrelated
	// argument. Two inputs, two ceilings.
	b.other += int64(len(cfg.ConfigID) + len(d.RunID))
	b.boundOnly(d.EntropySemanticsVersion)
	for i := range cfg.Detailed {
		b.boundOnly(string(cfg.Detailed[i].Comparator))
	}
	return b
}

// orderedRulesCutoffImpossible reports a boundary the projection could not have
// produced.
//
// The boundary is DERIVED, not supplied: a caller handing one in is asserting a
// fact about interventions the stream no longer carries, so the only thing left
// to check is that the assertion is internally possible. An unestablished
// boundary that names an intervention, or an established one with no identity,
// is a claim about evidence that is not there.
// orderedRulesCutoffImpossible is the whole boundary invariant: the
// constant-bounded half below, plus the one part of it whose cost the caller
// controls.
//
// The split exists because the structural tier calls the half and the invariant
// pass calls this. An earlier version hoisted the WHOLE predicate into the
// structural tier on the claim that it "compares only counts and constants and
// reads no supplied text at any length". That claim was false — the line below
// scans the cutoff identity for UTF-8 validity — and it was published in this
// file and in the specification on the strength of reading the predicate's
// first dozen lines rather than all of it. A reviewer found it by following
// checkIdentifier to invalidUTF8.
//
// The bound made it a small error and not a large one: the scan is one
// identifier capped at MaxOrderedRulesIdentifierBytes, so it could never
// recreate an aggregate-scale traversal. It still broke the tier's contract,
// and a tier whose contract holds "except for one field" is a tier that will
// acquire a second exception.
//
// Splitting was the objection I raised against doing this in the first place —
// that a subset becomes a second place to keep in step with the first. That
// objection is answered by the shape here being ONE definition with TWO
// callers, which is exactly how validateScopeShape, validateAdmissionShape and
// checkPresenceShape already relate to their full validators.
func orderedRulesCutoffImpossible(s OrderedRulesStream) bool {
	if orderedRulesCutoffShapeImpossible(s) {
		return true
	}
	// The SCAN, and the only part of this invariant that reads supplied bytes.
	// It stays out of the structural tier for that reason alone: an established
	// boundary whose identity is not encodable is still a stream the projection
	// could not have produced, so this is a deferral of cost and not of rigour.
	return s.Cutoff.Established && invalidUTF8(s.Cutoff.Identity)
}

// orderedRulesCutoffShapeImpossible is the CONSTANT-BOUNDED half of the
// boundary invariant, and it is what the structural tier runs before the digest.
//
// Every test here is a comparison against a constant, an integer comparison, or
// a length. The identity's emptiness and its bound are O(1) and read nothing;
// the Basis and Kind switches compare against closed sets, so a supplied value
// of matching length is read up to that constant's length and no further. None
// No caller can increase the PER-ELEMENT work here by supplying a longer
// string. The number of elements is a separate question, bounded by this
// repository's own count limits rather than by anything a check here does.
func orderedRulesCutoffShapeImpossible(s OrderedRulesStream) bool {
	c := s.Cutoff
	// The removal count is bounded by arithmetic, not by taste. The projection
	// refuses a source past MaxOrderedRulesCandidates, and inside it every
	// source candidate is either retained or counted as removed — one continue,
	// one append, no third path. So retained plus removed IS the source size,
	// and a stream claiming more than the ceiling between them describes a
	// source that could not have been admitted.
	//
	// Bounding the count alone would be looser than the truth: it would admit
	// a stream retaining a hundred candidates while claiming a hundred more
	// were removed, which is a source of two hundred.
	if c.DroppedAtOrAfter < 0 ||
		c.DroppedAtOrAfter > MaxOrderedRulesCandidates-len(s.Candidates) {
		return true
	}
	if !c.Established {
		// Nothing cut means nothing removed and nothing to name.
		return c.Basis != CutoffNoInterventionDeclared || c.Position != 0 ||
			c.Kind != "" || c.Identity != "" || c.DroppedAtOrAfter != 0
	}
	switch c.Basis {
	case CutoffFactualIntervention, CutoffConservativeAmbiguous:
	default:
		return true
	}
	switch c.Kind {
	case InterventionAutoCallStarted, InterventionManualCallStarted:
	default:
		return true
	}
	// The projection takes the boundary FROM an intervention, and refuses an
	// intervention outside the declared interval, so a boundary outside it
	// could not have come from one.
	if checkIdentifierPresent(c.Identity, "cutoff identity") != nil ||
		len(c.Identity) > MaxOrderedRulesIdentifierBytes ||
		c.Position < s.Scope.IntervalFromPosition || c.Position > s.Scope.IntervalToPosition {
		return true
	}
	// Removals are also bounded by the positions that EXIST at or after the
	// boundary. Causal positions are strictly increasing integers inside an
	// inclusive interval, so a boundary sitting on the interval's last position
	// leaves exactly one removable position however many candidates the source
	// held — and a claim of two describes a source that cannot exist.
	//
	// The span is computed unsigned. Position <= IntervalToPosition is settled
	// above, and for int64 a <= b the unsigned difference is exactly b-a even
	// where b-a overflows a signed int64, so an interval spanning the whole
	// range cannot wrap this into a spurious refusal.
	if c.DroppedAtOrAfter > 0 {
		span := uint64(s.Scope.IntervalToPosition) - uint64(c.Position)
		return span < uint64(c.DroppedAtOrAfter-1)
	}
	return false
}

// orderedRulesQualificationsNotDerived reports qualifications that are not
// exactly the ones this stream's own fields imply.
//
// Equality, not containment. A missing qualification drops a limitation the
// result is required to carry — a stream declaring incomplete coverage without
// SOURCE_COVERAGE_NOT_COMPLETE would produce a mechanism result that reads as
// though the absence of an earlier intervention were established. An ADDED one
// is just as wrong in the other direction: it asserts a limitation the evidence
// does not support. Both are silent, because qualifications travel verbatim
// into every result computed from the stream.
func orderedRulesQualificationsNotDerived(s OrderedRulesStream) bool {
	want := make([]string, 0, 5)
	if s.Scope.Coverage != CoverageCompleteDeclared {
		want = append(want, qualCoverageNotComplete)
	}
	if s.Cutoff.Basis == CutoffConservativeAmbiguous {
		want = append(want, qualConservativeCutoff)
	}
	if s.Cutoff.Basis == CutoffNoInterventionDeclared {
		want = append(want, qualNoInterventionSeen)
	}
	if s.Admission.ViewKind == ViewCalculateOnly {
		want = append(want, qualCalculateOnlyView)
	}
	if s.Cutoff.DroppedAtOrAfter > 0 {
		want = append(want, qualBoundaryRemovedCandidates+strconv.Itoa(s.Cutoff.DroppedAtOrAfter))
	}
	if len(want) != len(s.Qualifications) {
		return true
	}
	for i := range want {
		if want[i] != s.Qualifications[i] {
			return true
		}
	}
	return false
}

// orderedRulesPresenceWordUnknown reports a presence word outside the closed
// set, and it is deliberately a bare comparison rather than a call to
// checkPresenceVocabulary.
//
// The difference is the whole point of where this runs. checkPresenceVocabulary
// bounds the word with checkFreeText first, because its error QUOTES it; that
// scan is proportional to a string the caller chose. Here nothing is quoted —
// the structural tier answers yes or no — so the comparison is against three
// short constants and a caller cannot make THIS COMPARISON cost more by
// supplying a longer word: a longer word fails on length. How many times it
// runs is a different matter, bounded by the candidate and outcome ceilings.
//
// It does NOT read zero bytes, and saying it did was wrong. When a supplied
// word happens to match a constant's LENGTH, Go compares the bytes — up to the
// constant's own length, 36 at the widest in this package. That is bounded by
// this file rather than by the caller, which is the property that matters, but
// it is not nothing and the difference is exactly the kind that misleads a
// later reordering. A reviewer caught the overstatement.
//
// It closes a hole that checkPresenceShape cannot: that function returns
// immediately for every non-KNOWN value, because the availability rules it
// enforces only apply to a value that is present. An INVALID word is non-KNOWN,
// so it passed the whole pre-digest tier and was caught only by the invariant
// pass, AFTER the stream had been hashed. Measured on a stream whose final
// outcome carries a five-byte word outside the set: 9.038 µs with two
// candidates, 3.555366 ms with 128 candidates each holding 4 KiB of text — a
// constant-size fault charged for the entire retained payload. It is 649 ns and
// 7.092 µs now.
//
// A caller sees one thing change beyond the cost: this refusal used to be
// reached past the digest and handed the recomputed value back, so the
// documented two-call oracle worked on it. It now carries nothing, which is the
// same narrowing the rest of this tier already made and narrower is the safe
// direction — one more class of forged stream loses a published route.
//
// The candidate's own OutcomesPresence was already checked here in exactly this
// form. These two are the fields that were missed, not a new kind of rule.
func orderedRulesPresenceWordUnknown(p SuppliedPresence) bool {
	switch p {
	case SuppliedKnown, SuppliedMissing, SuppliedInvalid:
		return false
	default:
		return true
	}
}

// orderedRulesStreamIdentitiesAmbiguous reports two retained candidates sharing
// an identity, or two outcomes sharing one inside the same candidate.
//
// It is the evaluator's mirror of the uniqueness passes ProjectOrderedRulesStream
// runs, and it exists because the two are separate ingests: a stream handed
// directly to EvaluateOrderedRules never passed through the projection. The
// invariant pass settles both questions, so nothing here is checked less than
// before; this only settles them before the hash rather than after.
//
// It covered CANDIDATES ONLY for one commit, and the outcome half was found by
// a reviewer rather than here — the sixth time on this PR that a rule existed on
// one side of a pair and not the other. A stream repeating two SHORT outcome
// identities inside its last candidate was hashed whole first: 35.97074 ms on
// 128 candidates each holding 64 outcomes with 2,400-byte provenance, against
// 69.413 µs for a constant-bounded fault on that identical payload.
//
// It reads identity bytes — the maps hash them, all of them, at lengths the
// caller chooses — so it is NOT in the constant-bounded tier and must not be
// moved there. The candidate pass is bounded by the candidate ceiling times the
// per-identifier bound, half a megabyte between them. The outcome pass
// multiplies that by the outcome ceiling, 32 MiB nominally — but the shape gate
// above has already charged EVERY supplied stream string against
// orderedRulesTextCeiling at its worst-case encoded width, so the real cap on
// both passes together is that aggregate, about 21 MiB of raw text.
//
// It is NOT true that this reads no more than orderedRulesStreamDigest, and an
// earlier draft of this comment said so. A map hit hashes the probe AND then
// compares its bytes against the stored key, so the duplicate that ends this
// tier is read roughly three times over — twice hashed, once compared — where
// the digest would hash each occurrence once. On a stream that is nothing but
// two maximum-length equal identities, this tier reads MORE supplied bytes than
// the digest it precedes. A reviewer checked the argument rather than the
// wording, which is the correction this comment needed.
//
// The claim that survives is the one the tier is actually for: what it reads is
// bounded by the same aggregate the shape gate already charged, with a small
// constant factor for the map, and the refusal it reaches costs that bounded
// pass INSTEAD OF a whole-stream hash followed by the same pass.
func orderedRulesStreamIdentitiesAmbiguous(s OrderedRulesStream) bool {
	seen := make(map[string]bool, len(s.Candidates))
	for i := range s.Candidates {
		id := s.Candidates[i].Identity
		if seen[id] {
			return true
		}
		seen[id] = true
	}
	// A SECOND walk rather than one loop doing both, and deliberately: the
	// candidate pass costs at most half a megabyte, the outcome pass up to the
	// whole text aggregate. Folded into one loop, a repeated CANDIDATE identity
	// pays for every preceding candidate's outcomes first — measured both ways
	// on the same two inputs: 59.849 µs against 698.095 µs with 64 candidates
	// holding 64 outcomes on 2,400-byte identities, and 120.089 µs against
	// 345.477 µs on the outcome-provenance payload above. That is the
	// cheapest-decision-first rule this file keeps relearning, applied inside a
	// single function rather than between two.
	for i := range s.Candidates {
		c := &s.Candidates[i]
		// Scoped PER CANDIDATE, matching both the projection and the invariant
		// pass: two candidates may each carry an outcome called "A", and a
		// shared map would refuse a stream neither of those passes refuses.
		outcomes := make(map[string]bool, len(c.Outcomes))
		for j := range c.Outcomes {
			id := c.Outcomes[j].Identity
			if outcomes[id] {
				return true
			}
			outcomes[id] = true
		}
	}
	return false
}

// orderedRulesStreamStructureBroken is the stream's CONSTANT-BOUNDED tier:
// emptiness tests, integer comparisons, declared flags and closed-set
// comparisons, over the scope, the admission and every retained candidate and
// outcome.
//
// It was called the zero-byte tier until the closed-set comparisons moved in,
// and that name survived them by three commits. It is wrong: a supplied word
// matching a constant's LENGTH is compared byte for byte, up to 36 bytes at the
// widest constant here. What is true, and what the tier is actually for, is
// that no caller can make any check here cost more by supplying something
// LONGER, because longer fails on length. It is not that the tier is
// independent of the input: it walks every retained candidate and outcome, so a
// caller chooses how many times each check runs, within the count ceilings this
// repository sets. Per-element cost is ours; element count is bounded.
//
// That distinction took three attempts to state. The first claim was that the
// tier read no supplied byte, which the closed-set comparisons falsified; the
// second was that nothing here grows with what the caller supplies, which the
// iteration falsified. Both were corrected by reviewers, not by me.
// Emptiness tests and flags really do read nothing; the vocabulary comparisons
// read up to a constant. The distinction matters because a false invariant is
// what let a UTF-8 scan sit here for a commit.
//
// It is a function of its own because TWO callers need it at two different
// points: EvaluateOrderedRules runs it before computing the stream digest, and
// orderedRulesStreamInvariantsBroken runs it again ahead of its own text pass.
// One definition, so the rules cannot drift between them.
//
// An earlier revision said exactly that while the evaluator's call did not
// exist — the reorder had been reverted and this comment was left describing
// it. A reviewer found it by grepping the call sites. It is true now.
//
// Every check here is one the invariant pass below already makes. Nothing is
// added, and both callers answer yes or no.
func orderedRulesStreamStructureBroken(s OrderedRulesStream) bool {
	const where = "retained candidate"
	if validateScopeShape(s.Scope) != nil || validateAdmissionShape(s.Admission) != nil {
		return true
	}
	// The two whole-stream vocabularies, for the same reason and at the same
	// price as the presence words below: each is one comparison against a short
	// constant, neither is quoted here, and both were previously settled only
	// by validateScope and validateAdmission — after the hash.
	switch s.Scope.Coverage {
	case CoverageCompleteDeclared, CoverageGapsPresent, CoverageTruncatedPrefix:
	default:
		return true
	}
	switch s.Admission.ViewKind {
	case ViewChannelCandidateStream, ViewCalculateOnly:
	default:
		return true
	}
	// The SOURCE CONTRACT, which validateScope settles only after the hash. Its
	// refusal there quotes the supplied value, which is why it is not in
	// validateScopeShape and the projection says so. That reason does not reach
	// here: this tier quotes nothing, so the comparison is against one short
	// constant — read to that constant's length when a supplied value matches
	// it, and no further. The supplied version's LENGTH is the caller's, like
	// every other supplied string; what is not the caller's is the WORK, which
	// this comment said wrongly on its first attempt and which is the same
	// conflation the paragraph above exists to correct.
	//
	// AND THE SECOND ATTEMPT WAS WRONG TOO, in the narrow range the first one
	// was careless about. It said no supplied length could raise this check's
	// cost. Go's string equality compares lengths first, so a value SHORTER
	// than OrderedRulesStreamContractVersion is settled without reading a byte
	// — but one of exactly that length is compared byte for byte, so growing a
	// supplied value from one byte to nine does raise the work. A reviewer
	// caught it in the comment that exists to warn about this exact mistake,
	// which is the third time the conflation has been written here.
	//
	// What is true: the comparison work is bounded above by the length of
	// OrderedRulesStreamContractVersion — a constant of this package, not a
	// quantity the caller chooses — and the check runs once per stream rather
	// than once per candidate. Measured
	// against a four-byte unsupported version beside 128 candidates holding
	// 4 KiB each: 2.314278 ms before and 4.457 µs now, against 7.219 µs for the
	// identical fault on a two-candidate stream.
	if s.Scope.SourceContractVersion != OrderedRulesStreamContractVersion {
		return true
	}
	// And the DERIVED boundary — its constant-bounded half, which is the whole of
	// that invariant except one UTF-8 scan of the cutoff identity.
	//
	// The first version of this called orderedRulesCutoffImpossible whole, on
	// the claim that the predicate reads no supplied text. It does: see the
	// note on that function. Taking the half is not a subset maintained
	// separately — it is one definition with two callers, the same relation
	// validateScopeShape and checkPresenceShape already have to their full
	// validators, and the full predicate still runs in the invariant pass.
	if orderedRulesCutoffShapeImpossible(s) {
		return true
	}
	var structuralLast int64
	for i := range s.Candidates {
		c := &s.Candidates[i]
		switch {
		case checkIdentifierPresent(c.Identity, where) != nil,
			!c.HasPosition,
			i > 0 && c.Position <= structuralLast,
			c.Position < s.Scope.IntervalFromPosition,
			c.Position > s.Scope.IntervalToPosition,
			c.EpisodeMembership != MembershipProven,
			s.Cutoff.Established && c.Position >= s.Cutoff.Position,
			checkPresenceShape(c.Balance, where, c.Position) != nil,
			orderedRulesPresenceWordUnknown(c.Balance.Presence):
			return true
		}
		switch c.SourceKind {
		case SourceKindChannelUpdate:
			if s.Admission.ViewKind == ViewCalculateOnly {
				return true
			}
		case SourceKindCalculateSnapshot:
			if s.Admission.ViewKind == ViewChannelCandidateStream {
				return true
			}
		default:
			return true
		}
		switch c.OutcomesPresence {
		case SuppliedKnown, SuppliedMissing, SuppliedInvalid:
		default:
			return true
		}
		for j := range c.Outcomes {
			o := &c.Outcomes[j]
			if checkIdentifierPresent(o.Identity, where) != nil ||
				checkPresenceShape(o.Points, where, c.Position) != nil ||
				orderedRulesPresenceWordUnknown(o.Points.Presence) {
				return true
			}
		}
		structuralLast = c.Position
	}

	return false
}

// orderedRulesStreamInvariantsBroken reports a stream that could not have come
// from [ProjectOrderedRulesStream], however well its digest verifies.
//
// It re-runs SOME of the projection's own checks over the retained stream —
// checkIdentifier, checkPresence, checkFreeText, validateScope and
// validateAdmission are the projection's functions, called here rather than
// restated — so for those rules a stream the projection would have produced
// passes here by construction.
//
// IT DOES NOT FOLLOW THAT THE TWO PATHS CANNOT DRIFT, and this comment claimed
// exactly that until a reviewer pointed at the loop below. Candidate identity
// UNIQUENESS, strict causal ordering, episode membership, source-kind/view
// compatibility, outcome-vector presence and the boundary comparison are
// reimplemented here as inline copies of rules the projection also has. Copies
// drift. These ones already did: this pull request has found NINE separate
// instances of a rule or a documented claim enforced at one of the two ingest
// seams and absent at the other, every one of them found by a reviewer or by an
// audit built specifically to look for them.
//
// So the invariant is a maintenance obligation, not a construction guarantee:
// A CHANGE TO ProjectOrderedRulesStream'S RULES REQUIRES A MATCHING AUDIT OF
// THIS FUNCTION, and vice versa. Stating it the other way round was actively
// harmful — it told a future reader that the sibling path needed no checking,
// which is the precise mistake that produced all nine.
//
// This runs after the shape gate, so every loop below is bounded.
func orderedRulesStreamInvariantsBroken(s OrderedRulesStream) bool {
	const where = "retained candidate"
	// THE DERIVED CHECKS FIRST. Both answer from fields this stream already
	// carries — a boundary's basis, kind and position against the declared
	// interval, and a qualification list against the five conditions that imply
	// it — so each is a handful of comparisons against constants, and the
	// qualification comparison short-circuits on length before it reads a byte.
	//
	// The validators are the opposite: validateAdmission alone walks up to
	// MaxOrderedRulesSourceReferences strings of MaxOrderedRulesIdentifierBytes
	// each. Running them first meant a stream whose cutoff could not exist —
	// decidable without reading any of that — paid the whole 4 MiB scan to
	// reach the same true. Measured on the identical input: 7.027512 ms to
	// 3.938983 ms.
	//
	// The residue is not a miss, and naming it is the honest form of the
	// number. What remains is the stream digest, computed above over the same
	// references because it IS the comparison that let this stream get here;
	// no ordering can avoid it. What the swap removes is the SECOND traversal
	// of those bytes, which is the only part that was ever avoidable.
	//
	// The order is invisible in the result by construction: this function
	// answers yes or no, every branch returns the same true, and the typed
	// status it feeds carries no free text naming which rule tripped.
	//
	// That is also why this swap has NO test, and the gap is stated rather than
	// papered over. The other refusal orderings in this package are pinned by
	// which of two faults wins; here both faults produce the identical
	// StatusRefused / ReasonStreamInvariantViolated, so no observation
	// distinguishes the two orders and the repository's deterministic-test
	// contract rules out pinning one by elapsed time. This comment, not the
	// suite, is what holds the order — a reader who reverses these two blocks
	// will break nothing and lose 3.09 ms of the measurement above.
	if orderedRulesCutoffImpossible(s) || orderedRulesQualificationsNotDerived(s) {
		return true
	}
	if orderedRulesStreamStructureBroken(s) {
		return true
	}
	if validateScope(s.Scope) != nil || validateAdmission(s.Admission) != nil {
		return true
	}
	seen := make(map[string]bool, len(s.Candidates))
	var lastPosition int64
	for i := range s.Candidates {
		c := &s.Candidates[i]
		// The message argument is discarded: this pass answers yes or no, and
		// the typed status carries no free text. A caller wanting the specific
		// violation has the projection itself, which names it.
		switch {
		case checkIdentifier(c.Identity, where) != nil,
			seen[c.Identity],
			// Checked BEFORE the three comparisons below, which are exactly the
			// reason it matters: without it they compare a position the caller
			// never declared, and the projection's refusal of that same input
			// does not reach here. The digest is no substitute — it is unkeyed
			// and a refusal hands back the recomputed value, so stripping the
			// declaration and resubmitting with that value is two calls and no
			// cryptography.
			!c.HasPosition,
			i > 0 && c.Position <= lastPosition,
			c.Position < s.Scope.IntervalFromPosition,
			c.Position > s.Scope.IntervalToPosition,
			c.EpisodeMembership != MembershipProven,
			checkFreeText(c.Provenance, where) != nil,
			checkFreeText(c.OutcomesReason, where) != nil,
			checkPresence(c.Balance, where, c.Position) != nil:
			return true
		}
		switch c.SourceKind {
		case SourceKindChannelUpdate:
			if s.Admission.ViewKind == ViewCalculateOnly {
				return true
			}
		case SourceKindCalculateSnapshot:
			if s.Admission.ViewKind == ViewChannelCandidateStream {
				return true
			}
		default:
			return true
		}
		switch c.OutcomesPresence {
		case SuppliedKnown, SuppliedMissing, SuppliedInvalid:
		default:
			return true
		}
		// Mirrored from the projection, and added at the same time as it rather
		// than a commit later: three findings on this PR were a rule that
		// existed on one side of this pair and not the other.
		outcomesSeen := make(map[string]bool, len(c.Outcomes))
		for j := range c.Outcomes {
			o := &c.Outcomes[j]
			if checkIdentifier(o.Identity, where) != nil ||
				outcomesSeen[o.Identity] ||
				checkPresence(o.Points, where, c.Position) != nil {
				return true
			}
			outcomesSeen[o.Identity] = true
		}
		// The boundary is EXCLUSIVE, and a stream retaining a candidate at or
		// after it is one the projection would have cut.
		if s.Cutoff.Established && c.Position >= s.Cutoff.Position {
			return true
		}
		seen[c.Identity] = true
		lastPosition = c.Position
	}
	return false
}

// EvaluateOrderedRules re-derives the donor mechanism over a projected stream.
//
// It returns no error: every refusal and every unknown is a TYPED STATUS on the
// result, because a bare error would lose the distinction between "this input
// is not evaluable", "the mechanism ran and admitted nothing" and "the
// mechanism admitted and the stake is unknown" — three outcomes a caller must
// not conflate.
//
// The stream must be one [ProjectOrderedRulesStream] produced and did not
// subsequently mutate; the config is RAW percentages; the draws are the only
// entropy this function will ever see. There is no clock, no RNG and no
// fallback: an exhausted trace is an explicit unknown, never a default draw.
func EvaluateOrderedRules(stream OrderedRulesStream, rules OrderedRulesConfig, draws SuppliedDrawTrace) OrderedRulesEvaluation {
	// SHAPE FIRST, before anything walks the input.
	//
	// The three whole-input digests below read every candidate, every outcome,
	// every rule and every entropy word, and the qualification copy allocates
	// a slice the caller sized. All of that used to run BEFORE the bounds that
	// are supposed to govern it, so a trace eight times past
	// MaxOrderedRulesDrawWords was hashed in full and only then refused for
	// being too long — the bound was checked after the work it bounds.
	//
	// The stream's own case is worse, because it cannot be fixed by reordering
	// alone: the SelectionDigest comparison is circular, since detecting a
	// forged stream requires digesting it first. What CAN be bounded is how
	// much a forged one costs, and that is what this does.
	//
	// A refusal here carries no whole-input digest, and that is deliberate: the
	// function declined to read the input, so it can attest to nothing about
	// it. A digest of something never traversed would be the same kind of empty
	// guarantee this refusal exists to avoid. For the same reason the reason
	// codes below take precedence over the contract and digest mismatches — an
	// input too large to read cannot be checked for anything else.
	if reason := orderedRulesInputCountReason(stream, rules, draws); reason != "" {
		// The Cutoff is deliberately ABSENT here, and its absence is the same
		// guarantee as the missing digest rather than a separate one: a refusal
		// that declined to read the input re-exports none of it.
		//
		// It is also the difference between refusing cheaply and only APPEARING
		// to. The gate stops at the FIRST condition that trips, and most of
		// them never look at the cutoff — so its three text fields can reach
		// this point having passed no per-string bound at all. Carrying them
		// out would move the cost from the gate to whoever encodes the result,
		// which the JSON tags say is the intended use: a refusal decided in
		// O(1) would still write a gigabyte of supplied text, six-fold once
		// JSON escaping is counted.
		return orderedRulesUnreadRefusal(reason)
	}

	// THE VERSION CHECKS COME BEFORE THE DIGESTS, for the same reason the gate
	// does. Both are a single comparison against a constant, and both decide
	// that this input is not evaluable at all — so computing three whole-input
	// digests first is doing the work before the check that governs it.
	//
	// The cost was not theoretical. The config identifier and the run
	// identifier ride the separate config-and-trace ceiling and carry no
	// per-string bound, deliberately: inventing one that no other rule applies
	// would refuse input nothing else refuses. But that makes them large, and
	// measured with a 125 MiB config identifier beside a stream whose contract
	// version is simply wrong, this refusal took 1.23 seconds and allocated
	// 251,662,352 bytes — to compare two short strings and find them different.
	// It is 3.8 microseconds and nothing now.
	// LENGTH BEFORE MEANING, for these three and only these three, and it is
	// three len() comparisons rather than the whole budget walk.
	//
	// The comparisons below must sit above that walk — that is the point of
	// splitting the gate — but the package's rule is that an over-long value is
	// refused for its SIZE, and a case pins it for exactly these fields. Both
	// hold together only if the fields being compared are bounded first. So
	// they are, individually, at the same MaxOrderedRulesIdentifierBytes the
	// budget would have applied, and the full walk stays below.
	//
	// The first attempt at this hoist skipped these three lines and inverted
	// that precedence: an over-long contract version came back a MISMATCH
	// rather than OVER_BOUND. The suite caught it. Cost and precedence are
	// different questions, and moving a check for the first has now silently
	// answered the second twice on this PR.
	switch {
	case len(stream.ContractVersion) > MaxOrderedRulesIdentifierBytes,
		len(stream.Scope.SourceContractVersion) > MaxOrderedRulesIdentifierBytes,
		len(draws.EntropySemanticsVersion) > MaxOrderedRulesIdentifierBytes:
		return orderedRulesUnreadRefusal(ReasonStreamTextOverBound)
	}

	switch {
	case stream.ContractVersion != OrderedRulesStreamContractVersion:
		return orderedRulesUnreadRefusal(ReasonStreamContractMismatch)
	case draws.EntropySemanticsVersion != OrderedRulesEntropySemanticsVersion:
		return orderedRulesUnreadRefusal(ReasonEntropySemanticsMismatch)
	// The SCOPE's source contract joins them, and the gate was split so it
	// could. It is the same shape of decision — one supplied string against one
	// constant of this package, quoting nothing — and orderedRulesStreamStructureBroken
	// settles it too, far below. Reaching it only there meant the whole text
	// budget was totalled first: measured on 128 candidates x 64 outcomes with
	// 2,400-byte provenance, 58.568 µs. It is 104 ns here, against a 78 ns
	// floor — the cost of a refusal the count checks alone decide. Which is to
	// say it is now the comparison and nothing else.
	//
	// ProjectOrderedRulesStream reached this arrangement for the same field one
	// commit earlier, and a reviewer had to point out that the evaluator had
	// not — which makes it the ninth rule on this PR standing on one of the two
	// ingest paths and not the other.
	case stream.Scope.SourceContractVersion != OrderedRulesStreamContractVersion:
		return orderedRulesUnreadRefusal(ReasonStreamInvariantViolated)
	}

	// AND THE TEXT BUDGET, below the three comparisons above rather than above
	// them. See orderedRulesInputTextReason for why the gate is two functions.
	if reason := orderedRulesInputTextReason(stream, rules, draws); reason != "" {
		return orderedRulesUnreadRefusal(reason)
	}

	// ENCODABILITY, last of the cheap refusals and deliberately after the two
	// comparisons above.
	//
	// The config identifier and the run identifier are the only retained
	// strings no validator ever reads — neither type is part of the stream, so
	// the projection never sees them and there is no second path to mirror.
	// They carry no per-string LENGTH bound, for a reason that still holds:
	// inventing a limit no other rule applies would refuse input nothing else
	// refuses. Encodability is a different axis. Both types carry JSON tags,
	// and invalid UTF-8 in either was hashed as supplied and then replaced with
	// U+FFFD by the encoder, so the same logical run digested differently
	// before and after its own round trip — ConfigDigest e1a65694… became
	// 5251cf94…, with both evaluations returning WOULD_ATTEMPT.
	//
	// The position is the whole subtlety, and putting it in the shape gate was
	// wrong. The gate runs BEFORE these version comparisons, and these two
	// identifiers can legitimately approach the aggregate ceiling — so scanning
	// them there reintroduced attacker-controlled linear work on exactly the
	// early-refusal path an earlier repair had made constant-time. Measured at
	// that position: a wrong contract version beside a 64 MiB valid identifier
	// took 44.7 ms, against 1.834 µs for the same refusal with a short one.
	// Here, the version mismatch still costs two string comparisons and the
	// scan never runs. The ceilings still come first, in the gate, so a string
	// already past its budget is refused for size rather than read.
	//
	// THE STREAM'S OWN COMPARISON, once the two refusals below have passed.
	//
	// A caller handing in a stream whose SelectionDigest does not match is
	// refused without its TRACE being consulted, traversed or attested to, and
	// without a config, entropy or consumed-prefix digest over either — a
	// mismatch means nothing was consumed, so such a digest would witness
	// something this call never read, which is the same reason the two version
	// refusals above carry no digests at all.
	//
	// The CONFIG is the exception, and an earlier version of this paragraph
	// denied it in all three respects. normalizeOrderedRulesConfig runs below,
	// ahead of the hash, so an unusable config is decided first: a stream
	// carrying a forged SelectionDigest beside a config declaring no default is
	// refused CONFIG_DEFAULT_NOT_SUPPLIED, not STREAM_SELECTION_DIGEST_MISMATCH.
	// The config is consulted and traversed before a mismatch; only "not
	// attested to" survives, that refusal still carrying no config digest.
	//
	// Cost, which is why the position matters rather than only the principle.
	// The identifier scan and the config, entropy and consumed-prefix digests
	// each traverse ConfigID or RunID, and those two may legitimately approach
	// their own 128 MiB ceiling. Measured with a small stream and a 64 MiB
	// identifier: 582.770 ms to report a digest mismatch, against 14.049 µs for
	// the identical refusal with a short one — four traversals of text the
	// refusal does not depend on.
	//
	// The stream digest IS the comparison, so nothing that depends on the
	// comparison can precede it. That is not the same as the digest being
	// undeferrable, which is what this paragraph claimed until the two refusals
	// below were moved ahead of it — they were moved precisely because they do
	// NOT depend on the comparison. What survives is narrower: the hash is the
	// cheapest thing that can decide a MISMATCH, its cost is proportional to
	// the stream being judged, and the gate above has already bounded it.
	//
	// THE STREAM'S STRUCTURE AND THE CONFIG BEFORE THE DIGEST.
	//
	// A stream that could not have been projected is refused whatever its
	// digest says, and an unusable config is refused whatever stream came with
	// it. Neither decision needs the hash, so neither should pay for it: the
	// digest traverses every retained candidate and outcome, up to the whole
	// retained-text ceiling.
	//
	// This was declined once, on a trade that no longer holds, and the history
	// is worth keeping because the trade was real. The guard against an unbound
	// digest field — the reflection walk that caught HasAvailableAtPosition, a
	// P1 on this change — took its digest off EvaluateOrderedRules' result, so
	// deciding the structure first made every structural field refuse before a
	// digest existed and the walk could no longer tell "the digest binds this
	// field" from "the structure refused it". The reviewer who raised the
	// ordering supplied the missing half: the walk belongs in an internal test
	// calling orderedRulesStreamDigest directly, where the probe need not be a
	// stream anything would admit. It is now
	// TestSuppliedDigestBindsEveryFieldThatCouldChangeADecision, in
	// ordered_rules_bindings_internal_test.go; it still fails by name when the
	// flag is unbound, and the coupling that blocked this reorder is gone.
	//
	// What a caller sees changes, deliberately. A structurally impossible
	// stream, or one presented with an unusable config, used to be refused for
	// its digest first, and that refusal handed back the recomputed value — so
	// the documented two-call oracle worked on it. Both are now refused
	// carrying nothing, and the oracle does not function for either. That is
	// strictly narrower: two classes of forgery lose a published route. The
	// oracle still works for everything these two tiers admit, which is what
	// the cases exercising it depend on.
	// THE CONFIG FIRST, above the structural tier and not below it.
	//
	// It was below until a reviewer asked why. normalizeOrderedRulesConfig is
	// arithmetic over a rule count the count gate has already bounded, and an
	// unusable config — one declaring no default at all — is refused whatever
	// stream arrived with it. So walking every candidate and outcome first was
	// caller-selected cardinality paid for a decision that reads one flag:
	// measured on 128 candidates x 64 outcomes with 2,400-byte provenance,
	// 115.046 µs, and 69.14 µs here.
	//
	// NOT to the 78 ns floor, and the remainder is named rather than rounded
	// away: the text budget above still totals every supplied string before
	// this is reached. That walk is the gate's own and has to run — it is what
	// refuses an over-ceiling stream for its size — so it is a real floor for
	// this input where the ~115 µs was not. Moving the config above the text
	// gate as well would invert a precedence the package pins, since an
	// over-long string must be refused for its SIZE.
	//
	// This STRENGTHENS a precedence this model had already established and
	// pinned — an unusable config is refused before the stream invariants are
	// re-walked — rather than inverting one, which is why it is safe where the
	// qualification check was not. That check was cheap enough to hoist too and
	// could not be, because hoisting it put a STREAM fault above the config.
	cfg, configReason := normalizeOrderedRulesConfig(rules)
	if configReason != "" {
		return orderedRulesUnreadRefusal(configReason)
	}

	if orderedRulesStreamStructureBroken(stream) {
		return orderedRulesUnreadRefusal(ReasonStreamInvariantViolated)
	}

	// THE BOUNDARY'S ENCODABILITY, in a bounded-text tier of its own.
	//
	// This is the one part of the cutoff invariant that is not constant-bounded:
	// a UTF-8 scan of an identifier whose length the caller chose. It stayed in
	// the invariant pass for that reason, which was the right reason for the
	// wrong tier — below the identity walk and below the whole-stream hash. So
	// one invalid byte in a boundary identifier cost a near-ceiling stream its
	// entire traversal: 22.932501 ms on 128 candidates x 64 outcomes with
	// 2,400-byte provenance, and the refusal handed back the digest it had
	// computed, so the two-call oracle made that price repeatable. It is
	// 115.041 µs now and carries nothing — the text budget above, not this
	// scan.
	//
	// The scan is bounded because the text gate above has already refused any
	// single string past MaxOrderedRulesIdentifierBytes, so this reads at most
	// 4 KiB — a bounded-text tier, distinct from the constant-bounded one above
	// it and from the identity tier below, and it belongs in none of them.
	//
	// It keeps its position ABOVE the qualifications, mirroring the invariant
	// pass, where orderedRulesCutoffImpossible is evaluated before
	// orderedRulesQualificationsNotDerived.
	if stream.Cutoff.Established && invalidUTF8(stream.Cutoff.Identity) {
		return orderedRulesUnreadRefusal(ReasonStreamInvariantViolated)
	}

	// THE DERIVED QUALIFICATIONS, above the digest and below the config.
	//
	// orderedRulesQualificationsNotDerived derives at most five qualifications
	// from fields the tier above has already checked, builds them itself, and
	// compares them against a list bounded at MaxOrderedRulesQualifications.
	// Every comparison is against a short string this package generated, so it
	// is length-first and an enormous supplied qualification is rejected on
	// length rather than read. Measured with one fabricated qualification
	// beside 128 candidates holding 4 KiB each: 2.297362 ms before, 4.575 µs
	// now, against 7.01 µs for the same fault on two candidates.
	//
	// It sits BELOW the config rather than in the constant-bounded tier, and that
	// position is pinned by a case. Putting it in the tier made a stream fault
	// beat an unusable config, inverting a precedence this package had already
	// established and tested — the config is arithmetic over a bounded rule
	// count and decides first. Cost and precedence are different questions, and
	// moving a check for the first reason silently answered the second.
	if orderedRulesQualificationsNotDerived(stream) {
		return orderedRulesUnreadRefusal(ReasonStreamInvariantViolated)
	}

	// THE BOUNDED IDENTITY TIER, and it is deliberately NOT part of the
	// constant-bounded tier above.
	//
	// Two candidates repeating an identity is a stream the projection could not
	// have produced, and ProjectOrderedRulesStream refuses it before its own
	// payload scans. That fix did not reach here, because EvaluateOrderedRules
	// is a SEPARATE ingest: a caller can build a stream and hand it straight in
	// without the projection ever running. The pre-digest tier checked only
	// that each identity is non-empty, so two three-byte duplicates were
	// settled by the invariant pass with the whole stream already hashed —
	// 8.434 µs on two candidates against 3.549091 ms on 128 holding 4 KiB each.
	// It is 1.043 µs and 31.909 µs now.
	//
	// It gets its own tier because it is NOT free, and folding it into the
	// constant-bounded one would repeat the error this file made with the cutoff's
	// UTF-8 scan exactly one commit ago. Hashing an identity into a map reads
	// its bytes. The candidate envelope is MaxOrderedRulesCandidates x
	// MaxOrderedRulesIdentifierBytes, half a megabyte, against a stream digest
	// that traverses up to the whole retained ceiling — the same asymmetry the
	// projection's repair was taken on, and the reason this is worth a tier
	// rather than a note.
	//
	// It covered CANDIDATES ONLY for one commit, and the outcome half — the
	// projection refuses a candidate repeating an outcome identity, this side
	// left it to the invariant pass — was found by a reviewer, not here. Two
	// SHORT duplicate outcome identities inside the last candidate cost
	// 35.97074 ms on 128 candidates each holding 64 outcomes with 2,400-byte
	// provenance; 286.824 µs now, against 67.342 µs for a constant-bounded fault
	// on that identical payload. Fixing one path is not fixing the class: six
	// findings on this PR have now been a rule present on one side of a paired
	// ingest and absent on the other, and every one of the six was a reviewer's.
	//
	// The outcome pass widens the envelope to the whole text aggregate rather
	// than half a megabyte — see orderedRulesStreamIdentitiesAmbiguous for why
	// that is still bounded, and why the two walks are separate.
	//
	// The cost it adds to a stream that is NOT refused here is real and is not
	// hidden: a valid stream now walks its identities twice, once here and once
	// in the invariant pass, both inside the same bounded envelope.
	//
	// It runs AFTER the constant-bounded tier and not before it, which is the whole
	// point of there being two. The first arrangement had it first, and that
	// made every constant-bounded fault pay half a megabyte of identity hashing before
	// a comparison against a constant could answer: an unsupported source
	// contract went from 4.457 µs to 36.151 µs on the same input. Cheapest
	// decision first, at every tier, is the rule this file keeps relearning.
	if orderedRulesStreamIdentitiesAmbiguous(stream) {
		return orderedRulesUnreadRefusal(ReasonStreamInvariantViolated)
	}

	streamDigest := orderedRulesStreamDigest(stream)
	if stream.SelectionDigest != streamDigest {
		refused := orderedRulesUnreadRefusal(ReasonStreamDigestMismatch)
		// The recomputed digest travels back, deliberately, and it is the ONLY
		// thing that does. It is the documented two-call oracle, and hiding it
		// would raise a forgery's cost from one extra call to reading this
		// repository, which is not a boundary. The invariant pass below is what
		// actually stands in the way.
		refused.StreamDigest = streamDigest
		// The cutoff and the qualifications used to travel with it, and that
		// was wrong in the same way the shape gate above says: a refusal that
		// declined to read the input re-exports none of it.
		//
		// Here the reason is sharper than cost. A mismatch means this stream is
		// NOT what the projection produced, so its cutoff and its
		// qualifications are caller text that no derived check has seen —
		// orderedRulesCutoffImpossible and orderedRulesQualificationsNotDerived
		// both run BELOW this point. Copying them out put attacker-chosen
		// values into two fields whose whole contract is that they are derived:
		// a consumer reading Qualifications off a refusal saw a limitation list
		// the evidence never implied, and one reading Cutoff saw a boundary no
		// intervention established.
		//
		// The bill is real as well as wrong. The gate bounds each qualification
		// to MaxOrderedRulesIdentifierBytes and the count to
		// MaxOrderedRulesQualifications, so the measured re-export was 262,144
		// bytes of supplied text — copied, and then written again, six-fold
		// once JSON escaping is counted, by whoever encodes a refusal that read
		// nothing.
		return refused
	}

	out := OrderedRulesEvaluation{
		EvidenceLabel:           OrderedRulesEvidenceLabel,
		ModelVersion:            OrderedRulesModelVersion,
		DonorRevision:           OrderedRulesDonorRevision,
		EntropySemanticsVersion: OrderedRulesEntropySemanticsVersion,
		Participation:           ParticipationNotAdmitted,
		Stake:                   SuppliedUint32{Presence: SuppliedMissing, Reason: ReasonBalanceNotEvaluated},
		StreamDigest:            streamDigest,
		// ConfigDigest and EntropyDigest are DEFERRED to the point where the
		// traversal actually begins, below. See the note there.
	}
	// Cutoff and Qualifications are NOT set here. They are derived fields, and
	// nothing has yet established that this stream was derived. See below.

	// refuse reports a typed refusal witnessing the prefix ACTUALLY consumed.
	// A refusal reached mid-traversal has already read candidates and spent
	// words, and a digest claiming an empty prefix would misdescribe it.
	refuse := func(reason string, candidatesConsumed, wordsConsumed int) OrderedRulesEvaluation {
		out.Status = StatusRefused
		out.Reason = reason
		out.ConsumedInputDigest = orderedRulesConsumedDigest(stream, out.ConfigDigest, draws,
			candidatesConsumed, wordsConsumed, false)
		return out
	}

	// refuseUnread reports a refusal decided BEFORE the traversal began, and it
	// carries no consumed-prefix digest for the same reason the shape gate, the
	// two version comparisons and the digest mismatch carry none: the function
	// read nothing, so it can attest to nothing.
	//
	// The distinction is not cosmetic, which is what makes it a separate
	// closure rather than refuse(r, 0, 0). orderedRulesConsumedDigest binds the
	// prefix to WHICH config and WHICH run produced it — the recombination
	// defence — and doing that traverses ConfigID and RunID in full. Those two
	// ride the config-and-trace ceiling and carry no per-string bound, so on a
	// path where nothing was consumed the binding was buying an empty
	// distinction at the caller's price: measured with a broken stream
	// invariant beside a 64 MiB ConfigID, 130.908043 ms for a digest over a
	// prefix of nothing.
	//
	// An empty prefix has nothing to recombine. What the refusal still carries
	// is the stream digest it actually computed, and the reason.
	refuseUnread := func(reason string) OrderedRulesEvaluation {
		out.Status = StatusRefused
		out.Reason = reason
		return out
	}

	// THESE TWO ARE CURRENTLY UNREACHABLE, and saying so is the point of the
	// comment. orderedRulesInputShapeReason above tests both conditions, on the
	// same two arguments, against the same two constants, and returns the same
	// two reasons — so the gate always answers first and a caller never reaches
	// the versions here. They are kept as the second path rather than deleted,
	// on the same reasoning the invariant pass re-establishes the projection's
	// rules: a bound enforced in one place is a bound that moves when that
	// place changes. But unreachable code that LOOKS like the enforcement point
	// is its own hazard — it misled the author of the case in
	// ordered_rules_stream_test.go that tried to reach them — so the two
	// answers are not equivalent and the difference belongs here: the gate's
	// refusal carries nothing at all, having declined to read the input, while
	// these would carry the stream digest — computed and matched above — but
	// NOT the cutoff, which is assigned below, after the invariant pass that
	// these two precede.
	//
	// This sentence used to promise the cutoff as well. It was written while
	// that assignment still sat at the top of the function, and moving it down
	// to the line that earns it falsified the promise silently, because an
	// unreachable path has no test to break. Reproduced by neutralising the
	// gate's rule-count case so this one becomes reachable: the refusal came
	// back carrying a stream digest and an empty Cutoff.Identity, against a
	// stream whose projected identity was "call-1". It cannot be pinned by a
	// test for the same reason it drifted, and that is the third ordering in
	// this package documented as unpinnable rather than quietly asserted.
	//
	// Withholding the cutoff here is not an accident waiting to be repaired: it
	// is the rule the derived-field block below states. Nothing has yet
	// established that this stream was DERIVED, and a matching digest does not
	// establish it. If the gate ever stops testing these two conditions, that
	// is the behaviour that appears.
	switch {
	case len(draws.Words) > MaxOrderedRulesDrawWords:
		return refuseUnread(ReasonDrawWordsOverBound)
	case len(rules.Detailed) > MaxOrderedRulesRules:
		return refuseUnread(ReasonRuleCountOverBound)
	}

	if orderedRulesStreamInvariantsBroken(stream) {
		// A matching digest says the stream has not CHANGED. It does not say
		// where it came from, and it never could: the digest is unkeyed and
		// computed over exported fields, so a caller who can build the struct
		// can compute the value — and a refusal above even hands one back in
		// StreamDigest. Hiding that would be theatre, since the algorithm is
		// readable. What actually makes a stream safe to traverse is checking
		// it, so the projection's invariants are re-established here over the
		// stream as handed in.
		return refuseUnread(ReasonStreamInvariantViolated)
	}

	// THE DERIVED FIELDS, populated HERE and not in the literal above, because
	// this is the first line at which the stream has been shown to be one the
	// projection could have produced.
	//
	// Cutoff and Qualifications are computed BY ProjectOrderedRulesStream, and
	// every consumer reads them as facts about a stream that was derived rather
	// than supplied. Setting them at the top meant the two refusals above
	// returned them unchanged — so a config refusal and an invariant refusal
	// both handed back an invented boundary and a fabricated limitation list,
	// inside fields whose contract says otherwise. Reproduced through the
	// documented two-call oracle: both refusals returned Identity
	// "INVENTED-BOUNDARY", DroppedAtOrAfter 3 and two qualifications the
	// evidence never implied.
	//
	// The mismatch path above was repaired for exactly this and the repair
	// stopped one layer short, on the reasoning that a MATCHING digest makes
	// the values self-consistent. That reasoning is wrong, and this file says
	// why three lines up: a matching digest says the stream has not CHANGED,
	// never that the projection produced it. The digest is unkeyed and a
	// refusal hands the recomputed value back, so the oracle reaches these
	// paths carrying any cutoff the caller likes.
	//
	// Populating them here rather than clearing them in refuseUnread is the
	// point: a field that is never set until it is earned cannot be forgotten
	// on a path added later.
	out.Cutoff = stream.Cutoff
	if len(stream.Qualifications) > 0 {
		out.Qualifications = make([]string, len(stream.Qualifications))
		copy(out.Qualifications, stream.Qualifications)
	}

	// ENCODABILITY FIRST, which is where it belongs and not where it started.
	//
	// The rule is about the two digests below: invalid UTF-8 in ConfigID or
	// RunID is hashed as supplied and then replaced with U+FFFD by the encoder,
	// so the same logical run digests differently before and after its own
	// round trip. That makes this a precondition of HASHING those two strings,
	// not of evaluating at all — and the scan runs over strings that ride the
	// config-and-trace ceiling with no per-string bound, so every refusal
	// placed after it paid for it.
	//
	// It has moved twice for that reason. It began inside the shape gate, ahead
	// of the two version comparisons, where a wrong contract version beside a
	// 64 MiB valid identifier cost 44.7 ms instead of 1.834 µs. It then sat
	// ahead of the four pre-traversal refusals, none of which reads either
	// identifier, where a broken stream invariant beside the same identifier
	// cost 73.639716 ms against 3.788 µs. Here it guards exactly what it is
	// about, and every ConfigID-traversing digest in this function is below it.
	//
	// AND IT USES THE LOCAL refuseUnread, NOT THE PACKAGE-LEVEL HELPER. That
	// was the defect a reviewer found here: the package-level
	// orderedRulesUnreadRefusal builds a FRESH result, so this refusal — which
	// is reached only after the stream digest has been computed AND matched,
	// the invariant pass has passed, and the derived fields have been assigned
	// above — came back with an empty StreamDigest and an empty Cutoff. That
	// made it indistinguishable from a refusal decided before the stream was
	// read, which is the one distinction the attestation convention in this
	// function exists to carry. Reproduced: this refusal returned
	// StreamDigest "" where the same stream's digest-mismatch refusal one tier
	// up returned 97f42dfb… and an admissible run returned 94142827….
	//
	// The right helper is the closure, which keeps what was earned — the
	// stream digest, the cutoff and the qualifications — and leaves absent what
	// was not: the config, entropy and consumed-prefix digests, all computed
	// below this line.
	if invalidUTF8(rules.ConfigID) || invalidUTF8(draws.RunID) {
		return refuseUnread(ReasonSuppliedTextNotEncodable)
	}

	// THE TWO WHOLE-INPUT DIGESTS, computed HERE because this is the first
	// point at which the traversal is actually going to happen.
	//
	// They used to be built into the result literal above, which put an
	// unbounded hash of the caller's config and of the caller's whole entropy
	// run ahead of four refusals that depend on neither. Both are whole-input
	// digests in the sense the shape gate already names: a refusal that
	// consumed nothing can attest to nothing, and one that attests anyway is
	// the empty guarantee the rest of this function exists to avoid.
	//
	// This was one of THREE things standing between those four refusals and a
	// constant-time answer, and the staged measurement is worth keeping because
	// each step looked like the whole fix until the next one was measured. On a
	// broken stream invariant beside a 64 MiB ConfigID: 200.890391 ms with the
	// digests eager, 130.908043 ms once they moved here, 73.639716 ms once the
	// empty-prefix consumed digest went (see refuseUnread above), and 4.109 µs
	// once the encodability scan moved down beside them — against 4.052 µs for
	// the identical refusal with a SHORT identifier, which is the same number
	// and is how one knows the identifier is no longer read at all.
	//
	// The entropy side needed only this first step: an unusable config beside
	// 1,048,576 draw words went from 90.380221 ms to 5.928 µs here, because the
	// consumed digest hashes only the words actually spent, which on these
	// paths is none.
	out.ConfigDigest = orderedRulesConfigDigest(rules)
	out.EntropyDigest = orderedRulesEntropyDigest(draws)

	words := draws.Words
	cursor := 0
	work := 0

	for ci := range stream.Candidates {
		c := &stream.Candidates[ci]
		wordsBefore := cursor
		visit := OrderedRulesCandidateVisit{
			CandidateIndex:    ci,
			CandidateIdentity: c.Identity,
			CandidatePosition: c.Position,
			BalanceUse:        BalanceNotEvaluated,
		}
		out.CandidatesConsumed = ci + 1
		out.StoppedAtCandidate = c.Identity
		out.StoppedAtPosition = c.Position
		out.HasStopPosition = true

		// A vector the caller could not recover is NOT the donor's decline, and
		// the difference decides whether the traversal continues. This is
		// checked FIRST, because a short vector is only a decline once the
		// caller has said the vector is the whole of what was there.
		if c.OutcomesPresence != SuppliedKnown {
			visit.Verdict = CandidateUnknownInput
			visit.RawWordsConsumedHere = cursor - wordsBefore
			out.Visits = append(out.Visits, visit)
			out.Status = StatusUnknownInput
			out.Reason = ReasonOutcomeVectorNotKnown
			out.ConsumedInputDigest = orderedRulesConsumedDigest(stream, out.ConfigDigest, draws, ci+1, cursor, false)
			return out
		}

		// The donor declines a pool it cannot compare. This is checked BEFORE
		// the points are read, so a one-outcome pool with an unreadable total
		// is a decline rather than an unknown.
		if len(c.Outcomes) < 2 {
			visit.Verdict = CandidateTooFewOutcomes
			out.Visits = append(out.Visits, visit)
			continue
		}

		total, poolReason := checkedPoolSum(c.Outcomes)
		if poolReason != "" {
			visit.Verdict = CandidateUnknownInput
			visit.RawWordsConsumedHere = cursor - wordsBefore
			out.Visits = append(out.Visits, visit)
			out.Status = StatusUnknownInput
			out.Reason = poolReason
			out.ConsumedInputDigest = orderedRulesConsumedDigest(stream, out.ConfigDigest, draws, ci+1, cursor, false)
			return out
		}
		visit.PoolTotalKnown = true
		visit.PoolTotal = total

		for oi := range c.Outcomes {
			out.OutcomesConsidered++
			share := donorShare(total, c.Outcomes[oi].Points.Value)
			shareBits := math.Float64bits(share)

			admittedRule := -1
			for ri := range cfg.detailed {
				work++
				if work > MaxOrderedRulesWork {
					visit.Verdict = CandidateRefused
					visit.RawWordsConsumedHere = cursor - wordsBefore
					out.Visits = append(out.Visits, visit)
					out.RawWordsConsumed = cursor
					return refuse(ReasonWorkBudgetExceeded, ci+1, cursor)
				}
				out.RulesConsidered++
				rule := &cfg.detailed[ri]

				var matched bool
				switch rule.comparator {
				case ComparatorLe:
					matched = share <= rule.threshold
				case ComparatorGe:
					matched = share >= rule.threshold
				}
				if !matched {
					// A comparator that did not match costs NO entropy. The
					// donor's `does_match && rng.gen_bool(..)` short-circuits,
					// so the draw is never reached.
					out.Trace = append(out.Trace, traceEntry(TraceStepRuleComparator, ci, c, oi, shareBits, ri, false, false, false, -1, 0, false))
					continue
				}

				threshold, ok := bernoulliThreshold(rule.attemptRate)
				if !ok {
					// Unreachable while the admitted domain is 0..100: raw 100
					// normalizes to exactly one and everything below it lands in
					// [0,1). It is kept so that widening that domain later
					// surfaces here as a typed refusal instead of silently
					// producing a distribution rand would have refused to build.
					visit.Verdict = CandidateRefused
					visit.RawWordsConsumedHere = cursor - wordsBefore
					out.Visits = append(out.Visits, visit)
					out.RawWordsConsumed = cursor
					return refuse(ReasonAttemptRateOutOfDomain, ci+1, cursor)
				}

				rawIndex := -1
				var rawValue uint64
				var success bool
				if threshold == bernoulliAlwaysTrue {
					// Rate of exactly one: true, and NO word consumed.
					success = true
				} else {
					if cursor >= len(words) {
						// The draw was REQUIRED and the trace is spent. This is
						// an explicit unknown, never a default draw and never a
						// skip: guessing here would fabricate an opportunity.
						out.Trace = append(out.Trace, traceEntry(TraceStepRuleDraw, ci, c, oi, shareBits, ri, true, false, false, -1, 0, false))
						visit.Verdict = CandidateUnknownInput
						visit.RawWordsConsumedHere = cursor - wordsBefore
						out.Visits = append(out.Visits, visit)
						out.RawWordsConsumed = cursor
						out.Status = StatusUnknownInput
						out.Reason = ReasonEntropyExhausted
						out.ConsumedInputDigest = orderedRulesConsumedDigest(stream, out.ConfigDigest, draws, ci+1, cursor, false)
						return out
					}
					rawIndex = cursor
					rawValue = words[cursor]
					cursor++
					// Rate of exactly zero reaches here too: its threshold is 0,
					// so it consumes a word and then fails on `raw < 0`.
					success = rawValue < threshold
				}
				out.BernoulliEvaluations++
				out.RawWordsConsumed = cursor
				out.Trace = append(out.Trace, traceEntry(TraceStepRuleDraw, ci, c, oi, shareBits, ri, true, true, success, rawIndex, rawValue, success))
				if success {
					admittedRule = ri
					break
				}
				// A matched comparator whose draw failed does NOT end the scan.
				// The donor's `find` keeps going, so a later overlapping rule
				// still gets its own chance on this same outcome.
			}

			if admittedRule >= 0 {
				out.RawWordsConsumed = cursor
				visit.RawWordsConsumedHere = cursor - wordsBefore
				admitOrderedRules(&out, &visit, stream, rules, draws, c, ci, oi, admittedRule,
					shareBits, cfg.detailed[admittedRule].points, SelectionDetailedRule, cursor)
				return out
			}

			// The CURRENT outcome's default, before any later outcome's rules.
			work++
			if work > MaxOrderedRulesWork {
				visit.Verdict = CandidateRefused
				visit.RawWordsConsumedHere = cursor - wordsBefore
				out.Visits = append(out.Visits, visit)
				out.RawWordsConsumed = cursor
				return refuse(ReasonWorkBudgetExceeded, ci+1, cursor)
			}
			// Inclusive on both ends, and never reordered when min > max.
			inDefault := share >= cfg.defMin && share <= cfg.defMax
			out.Trace = append(out.Trace, traceEntry(TraceStepDefaultBounds, ci, c, oi, shareBits, -1, inDefault, false, false, -1, 0, inDefault))
			if inDefault {
				// The default consumes NO new draw. Words already spent by
				// failed detailed rules on this outcome stay spent.
				out.RawWordsConsumed = cursor
				visit.RawWordsConsumedHere = cursor - wordsBefore
				admitOrderedRules(&out, &visit, stream, rules, draws, c, ci, oi, -1,
					shareBits, cfg.defPoints, SelectionDefault, cursor)
				return out
			}
		}

		visit.Verdict = CandidateNoMatch
		visit.RawWordsConsumedHere = cursor - wordsBefore
		out.Visits = append(out.Visits, visit)
	}

	out.Status = StatusNoAttemptInSuppliedPrefix
	out.RawWordsConsumed = cursor
	out.ConsumedInputDigest = orderedRulesConsumedDigest(stream, out.ConfigDigest, draws,
		len(stream.Candidates), cursor, true)
	return out
}

// admitOrderedRules records an admission and sizes the stake if it can.
//
// Participation and stake are set INDEPENDENTLY. That separation is the whole
// point: the mechanism's choice is established by the traversal, while sizing
// needs a balance the traversal may not have. A missing balance therefore
// leaves a known participation beside an unknown stake, and never a zero.
func admitOrderedRules(out *OrderedRulesEvaluation, visit *OrderedRulesCandidateVisit,
	stream OrderedRulesStream, rules OrderedRulesConfig, draws SuppliedDrawTrace,
	c *OrderedRulesCandidate, ci, oi, ruleIndex int,
	shareBits uint64, points normalizedPoints, basis OrderedRulesSelectionBasis, cursor int) {

	visit.Verdict = CandidateAdmitted
	out.Participation = ParticipationAdmitted
	out.Selected = &OrderedRulesSelection{
		CandidateIdentity: c.Identity,
		CandidatePosition: c.Position,
		CandidateIndex:    ci,
		OutcomeIndex:      oi,
		OutcomeIdentity:   c.Outcomes[oi].Identity,
		Basis:             basis,
		RuleIndex:         ruleIndex,
		ShareBits:         shareBits,
	}

	b := c.Balance
	switch {
	case b.Presence == SuppliedInvalid:
		visit.BalanceUse = BalanceRequiredButInvalid
		out.Status = StatusParticipationAdmittedStakeUnknown
		out.Reason = ReasonBalanceInvalid
		out.Stake = SuppliedUint32{Presence: SuppliedInvalid, Reason: balanceReason(b, ReasonBalanceInvalid)}
	case b.Presence != SuppliedKnown:
		visit.BalanceUse = BalanceRequiredButMissing
		out.Status = StatusParticipationAdmittedStakeUnknown
		out.Reason = ReasonBalanceNotSupplied
		out.Stake = SuppliedUint32{Presence: SuppliedMissing, Reason: balanceReason(b, ReasonBalanceNotSupplied)}
	case b.Value < 0 || b.Value > math.MaxUint32:
		// The donor sizes stakes in u32. A balance outside that domain leaves
		// the admission standing and the sizing impossible — it is not clamped
		// into range, because a clamped stake would be a number the donor never
		// would have produced.
		visit.BalanceUse = BalanceRequiredButOutOfDomain
		out.Status = StatusParticipationAdmittedStakeUnknown
		out.Reason = ReasonBalanceOutOfDomain
		out.Stake = SuppliedUint32{Presence: SuppliedInvalid, Reason: ReasonBalanceOutOfDomain}
	default:
		// A known balance sizes the stake, INCLUDING a stake of zero. Zero is
		// what the donor would have requested; it is not a skip, and no minimum
		// is substituted for it.
		visit.BalanceUse = BalanceUsed
		out.Status = StatusWouldAttempt
		out.Stake = SuppliedUint32{Presence: SuppliedKnown, Value: pointsValue(points, uint32(b.Value))}
	}

	out.Visits = append(out.Visits, *visit)
	out.ConsumedInputDigest = orderedRulesConsumedDigest(stream, out.ConfigDigest, draws, ci+1, cursor, false)
}

func balanceReason(b SuppliedInt64, fallback string) string {
	if b.Reason != "" {
		return b.Reason
	}
	return fallback
}

// traceEntry records one evaluated slot.
//
// It takes no identifier: ci and oi address stream.Candidates[ci].Outcomes[oi]
// in the very stream this traversal was handed, and StreamDigest binds which
// stream that is, so repeating two 4 KiB strings on each of up to
// MaxOrderedRulesWork entries would add nothing an index does not already say
// while making the encoded trace scale with the caller's identifier lengths.
// See TestOrderedRulesRetainedTraceStaysInsideTheBudgetWhenSerialized.
func traceEntry(step OrderedRulesTraceStep, ci int, c *OrderedRulesCandidate, oi int,
	shareBits uint64, ruleIndex int,
	comparatorMatched, bernoulliEvaluated, bernoulliResult bool,
	rawIndex int, rawValue uint64, admitted bool) OrderedRulesTraceEntry {
	return OrderedRulesTraceEntry{
		Step:               step,
		CandidateIndex:     ci,
		CandidatePosition:  c.Position,
		OutcomeIndex:       oi,
		ShareBits:          shareBits,
		RuleIndex:          ruleIndex,
		ComparatorMatched:  comparatorMatched,
		BernoulliEvaluated: bernoulliEvaluated,
		BernoulliResult:    bernoulliResult,
		RawWordIndex:       rawIndex,
		RawWordValue:       rawValue,
		Admitted:           admitted,
	}
}

// normalizeOrderedRulesConfig divides every raw percentage by 100 EXACTLY ONCE.
//
// It returns a closed reason rather than an error so the caller can report it
// as a typed status. The admitted domain mirrors the donor's own validation:
// each percentage field independently in 0..100, finite. Min above max is NOT
// corrected — the donor validates the two independently and never swaps them,
// so a min above a max is a configuration that admits nothing, which is a real
// configuration and not this model's to repair.
func normalizeOrderedRulesConfig(c OrderedRulesConfig) (normalizedConfig, string) {
	// The default is MANDATORY, and saying so in a comment is not enforcing it:
	// every field of OrderedRulesDefault has a legitimate zero, so an omitted
	// one decodes to a USABLE [0,0] rule that admits any zero-share outcome and
	// reports a stake of zero with presence KNOWN. Refusing here is the
	// difference between "no configuration" and a configuration that happens to
	// be all zeros, which the donor's own `small` preset really does ship.
	if !c.HasDefault {
		return normalizedConfig{}, ReasonConfigDefaultNotSupplied
	}
	var out normalizedConfig
	if len(c.Detailed) > 0 {
		out.detailed = make([]normalizedRule, 0, len(c.Detailed))
	}
	for i := range c.Detailed {
		r := &c.Detailed[i]
		switch r.Comparator {
		case ComparatorLe, ComparatorGe:
		default:
			return normalizedConfig{}, ReasonConfigOutOfDomain
		}
		if !rawPercentInDomain(r.RawThresholdPercent) ||
			!rawPercentInDomain(r.RawAttemptRatePercent) ||
			!rawPercentInDomain(r.Points.RawPercent) {
			return normalizedConfig{}, ReasonConfigOutOfDomain
		}
		out.detailed = append(out.detailed, normalizedRule{
			comparator:  r.Comparator,
			threshold:   r.RawThresholdPercent / percentDivisor,
			attemptRate: r.RawAttemptRatePercent / percentDivisor,
			points: normalizedPoints{
				maxValue: r.Points.MaxValue,
				percent:  r.Points.RawPercent / percentDivisor,
			},
		})
	}
	if !rawPercentInDomain(c.Default.RawMinPercent) ||
		!rawPercentInDomain(c.Default.RawMaxPercent) ||
		!rawPercentInDomain(c.Default.Points.RawPercent) {
		return normalizedConfig{}, ReasonConfigOutOfDomain
	}
	out.defMin = c.Default.RawMinPercent / percentDivisor
	out.defMax = c.Default.RawMaxPercent / percentDivisor
	out.defPoints = normalizedPoints{
		maxValue: c.Default.Points.MaxValue,
		percent:  c.Default.Points.RawPercent / percentDivisor,
	}
	return out, ""
}

// rawPercentInDomain mirrors the donor's `range(min = 0.0, max = 100.0)`.
// NaN fails both comparisons and is rejected rather than normalized to zero.
func rawPercentInDomain(v float64) bool { return v >= 0 && v <= 100 }

// checkedPoolSum totals an outcome vector in int64 without wrapping.
//
// Every outcome's points must be KNOWN. A missing one is not a zero: the pool
// total feeds every share in the vector, so one absent value makes the whole
// candidate's arithmetic unknown rather than merely one outcome's.
//
// The sign and overflow guards are a DELIBERATE divergence from the donor, and
// worth naming as one because everything else in this file is a faithful
// transcription. The donor folds i64 points with a plain +, so a negative pool
// entry yields a negative share it goes on to compare, and a sum past i64
// wraps. Neither is a decision this model may reproduce: a wrapped total is
// undefined behaviour to lean on, and a negative share is an input this model
// cannot claim came from a real pool. Both are therefore reported as a typed
// unknown rather than normalized to zero or evaluated as if they were sound —
// the model refuses to guess rather than imitating a runtime that never had to
// decide.
func checkedPoolSum(outcomes []OrderedRulesOutcome) (int64, string) {
	var total int64
	for i := range outcomes {
		v := outcomes[i].Points
		if v.Presence != SuppliedKnown {
			return 0, ReasonOutcomePointsNotKnown
		}
		if v.Value < 0 {
			return 0, ReasonOutcomePointsOutOfDomain
		}
		if total > math.MaxInt64-v.Value {
			return 0, ReasonPoolSumOverflow
		}
		total += v.Value
	}
	return total, ""
}

// donorShare computes one outcome's pool share the way the donor does.
//
// The two divisions are NOT algebraically simplified to points/total. In
// binary64 they are different functions: for points=[9,1] the donor's
// 1/(10/9) is one ulp BELOW the nearest double to 0.9, while 9/10 rounds to the
// nearest double AT 0.9 — so a Ge rule with a 90% threshold matches under the
// shortcut and does not match under the donor. See
// TestOrderedRulesReciprocalRatioGe90Boundary.
func donorShare(total, points int64) float64 {
	if points == 0 {
		return 0
	}
	odds := float64(total) / float64(points)
	if odds == 0 {
		return 0
	}
	return 1.0 / odds
}

// bernoulliThreshold mirrors rand 0.8.5's Bernoulli::new.
//
//	if !(0.0..1.0).contains(&p) { if p == 1.0 { ALWAYS_TRUE } else { Err } }
//	p_int = (p * 2^64) as u64
//
// The half-open range is the donor's, not a transcription slip: exactly one is
// special-cased to a sentinel that skips the draw entirely, which is why a
// hundred-percent participation rate consumes no entropy. NaN satisfies neither
// branch and is refused.
func bernoulliThreshold(p float64) (uint64, bool) {
	if !(p >= 0.0 && p < 1.0) {
		if p == 1.0 {
			return bernoulliAlwaysTrue, true
		}
		return 0, false
	}
	return saturatingUint64(p * bernoulliScale), true
}

// pointsValue mirrors the donor's Points::value: multiply in float64, TRUNCATE
// to u32, and only then compare against the cap.
//
// Two of those three choices change the answer, and one does not — worth saying
// exactly, because an overstated claim here is the same error this package
// exists to avoid.
//
// TRUNCATION rather than rounding changes it: ten percent of 999 is
// 99.90000000000001 in binary64, so truncating gives 99 where rounding gives
// 100. A cap of ZERO meaning "no cap" changes it: reading it as a limit would
// turn every uncapped rule into a stake of nothing.
//
// The cast/cap ORDER does not. Over the reachable domain — a normalized
// percentage in [0,1] and a balance in the u32 range, so no NaN, no negative
// and no saturation — flooring before the cap and flooring after it agree for
// every input, because the cap is an integer: floor(min(x,M)) equals
// min(floor(x),M). This form is kept because it is the donor's own, not because
// the alternative would compute something different, and
// TestOrderedRulesPointsTruncateThenCap pins the two choices that do matter
// rather than pretending to pin this one.
func pointsValue(p normalizedPoints, balance uint32) uint32 {
	value := saturatingUint32(p.percent * float64(balance))
	if p.maxValue == 0 {
		return value
	}
	if value < p.maxValue {
		return value
	}
	return p.maxValue
}

// saturatingUint64 mirrors Rust's float-to-integer `as` cast, which SATURATES
// rather than wrapping or trapping: NaN becomes zero, negatives become zero,
// and anything at or above the type's ceiling becomes its maximum. Go leaves
// the same conversion implementation-defined, so the clamping is explicit.
func saturatingUint64(f float64) uint64 {
	if math.IsNaN(f) || f <= 0 {
		return 0
	}
	if f >= bernoulliScale {
		return ^uint64(0)
	}
	return uint64(f)
}

func saturatingUint32(f float64) uint32 {
	if math.IsNaN(f) || f <= 0 {
		return 0
	}
	if f >= u32Ceiling {
		return math.MaxUint32
	}
	return uint32(f)
}
