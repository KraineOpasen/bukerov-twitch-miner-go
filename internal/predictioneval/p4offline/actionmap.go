package p4offline

import "github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"

// SEAM 7: the exhaustive native action map, p4-native-action-map/v1.
//
// Each native evaluator reports a terminal action or status AND a stage-by-
// stage shape. The map reads BOTH: a status name alone is not evidence of the
// path that produced it, because an Evaluation is an exported value a caller
// can build, decode or edit. A shape that contradicts its claimed action is
// UNSUPPORTED_SHAPE — fail closed — and never a skip.
//
// The table, fixed by the protocol:
//
//	P2  WOULD_ATTEMPT_PLACEMENT      -> WOULD_ATTEMPT
//	P2  HEALTH_GATED (proved)        -> POLICY_SKIP
//	P2  RESERVE_VIOLATION (proved)   -> POLICY_SKIP
//	P2  BELOW_MINIMUM_POINTS (proved)-> POLICY_SKIP
//	P2  FILTER_REJECTED (proved)     -> POLICY_SKIP
//	P2  PRE_DECISION_EXIT            -> COVERAGE_ONLY
//	P2  LEGACY_FAILURE               -> LEGACY_FAILURE
//	P2  INDETERMINATE                -> UNKNOWN_INPUT
//	P2  UNSUPPORTED                  -> UNKNOWN_INPUT
//	P3b WOULD_ATTEMPT                -> WOULD_ATTEMPT
//	P3b PARTICIPATION_ADMITTED_STAKE_UNKNOWN -> itself (distinct)
//	P3b NO_ATTEMPT_IN_SUPPLIED_PREFIX        -> itself (distinct; no zero)
//	P3b UNKNOWN_INPUT                -> UNKNOWN_INPUT
//	P3b REFUSED                      -> REFUSED
//	anything else, or any contradicted shape -> UNSUPPORTED_SHAPE
//
// "Proved" means the stage shape witnesses the path to the skip: the stages
// before the exit ran as the path runs them — the choice and the filter
// executed, the base stake and the stealth stage exactly when the choice
// was selected — the health gate standing where the path puts it
// (witnessed DENIED for HEALTH_GATED, witnessed with a verdict the
// vocabulary names as open — DISABLED, NO_GATE or ALLOWED — for every path
// past the gate), the stake-gate stages executed or not reached as the path
// runs them, and nothing reached past the exit. The results the evaluator
// can reach on either side of the gate (LEGACY_FAILURE, INDETERMINATE,
// UNSUPPORTED) are bound to one stopping stage the same way: the stages
// before the stop ran as the path runs them, no other stage is in a
// stopping state, every stage after the stop is not reached, and the gate
// is NOT_REACHED — carrying no verdict — before the stop or witnessed open
// past it. Invariants of the core hold on every shape: a base-stake stage
// ran exactly when the choice was selected; a clamp that did not run has no
// final stake; a stage the path never ran carries its initial value and
// nothing else (the choice at index -1, the stealth stage of a choice out
// of range marked NOT_APPLICABLE); under FACTUAL_STEALTH_OFF the policy
// amount is known exactly when the stake block ran or was skipped by a
// choice out of range; an executed stake gate's reason is one of its
// vocabulary and its proposal is the policy amount it was handed; a percent
// cap reduced that proposal — its limit is below it — and the allowance it
// published is that limit under the core's zero floor; the clamp applied
// exactly when that reason was the percent cap, and its final stake is the
// gate's allowance when it applied and the policy amount otherwise (never
// the allowance, which the core floors to zero on a negative proposal it
// did not cap); the filter applied only at the filter exit; and
// a stage that stopped the evaluation carries its stop and nothing it
// would have answered (the choice at index -1; the stake gate, when
// INDETERMINATE, only the proposal it was handed). One invariant of the
// PROJECTION holds too — the
// minimum threshold is the pinned one: the projection pins it on every
// case it hands to the factset seam and the seam refuses any other by its
// values, so the map requires it on every shape. Under FACTUAL_STEALTH_OFF
// the stealth stage never stops the evaluation: it is not reached, or
// executed as NOT_APPLICABLE with the policy amount echoed as its realization
// and nothing drawn; a stealth stage in any other shape — a realization of
// its own, a draw, a flag — is refused whatever the
// arm says (STEALTH_REALIZATION_PRESENT), and a stopping one is named a
// second time beside the arm's own stop (ANOTHER_STAGE_STOPPED), so the
// arms read the choice, base-stake, filter and stake-gate stages as the
// only stops.
//
// The map reads the native action; the stage states; the selection; the
// stealth stage whole; the health verdict, and on a gate that never ran
// its reason; an executed stake gate's reason and proposal and, under the
// percent cap, its limit and allowance; the filter's and the minimum's answers where an arm's
// exit turns on them, and that the filter applied only at its exit; the
// known-amount flag; the clamp's final-stake presence, whether it applied
// and its final stake; the minimum threshold; the presence of a failure
// name; every field of a stage that did not run, of a choice, base stake
// or filter that stopped the evaluation, and of a stake gate that stopped
// it UNSUPPORTED or INDETERMINATE; and the policy amount, only against the
// realization an executed stealth stage echoes, the proposal of a stake
// gate that executed or stopped INDETERMINATE and the final stake of an
// uncapped clamp. Not read here: the amounts, indices, identities and limits a
// stage that ran computed beyond those, the policy amount otherwise, the
// action reason and the spelling of the failure name — bound through the
// digests of the inputs and, with the action reason, through the framed
// action, choice and stake (the failure name is spelled again by the
// framed action reason); whether the base stake was capped, read nowhere
// in this package on a stage that ran, which re-evaluates a stored result
// rather than trusting it; the model provenance and the common-input
// digest, which the P2 result witness leaves unframed and frames
// respectively; a witnessed gate's reason and an executed filter's skip
// and comparison where no exit turns on them; and the limitations list,
// which that witness leaves unframed, as its doc says.
//
// Two refusals reach past what the evaluator produces. A minimum threshold
// other than the pinned one is refused on every shape (MINIMUM_NOT_PINNED):
// the projection pins it on every case it hands to the factset seam and
// the seam refuses any other by its values, so only a hand-built
// evaluation carries one. And the one shape the evaluator itself produces
// under a zero minimum — a WOULD_ATTEMPT_PLACEMENT with no selected choice,
// which the pinned policy reaches when a strategy outside its closed set
// leaves the choice at -1 and the zero amount passes a zero minimum — is
// refused on purpose: an attempt without a chosen outcome has no choice to
// score; it is an UNSUPPORTED_SHAPE whose contradictions name the
// unselected choice (CHOICE_NOT_SELECTED), the stake-block stages it never
// ran and the unpinned minimum, never a WOULD_ATTEMPT.
//
// Under FACTUAL_STEALTH_OFF a P2 evaluation whose stealth stage reports a
// realization — conditioned or unproven — is illegal whatever its action:
// such a value could only have come from a case this protocol excludes.

// ActionClass is the closed P4 action vocabulary.
type ActionClass string

const (
	ActionWouldAttempt                      ActionClass = "WOULD_ATTEMPT"
	ActionPolicySkip                        ActionClass = "POLICY_SKIP"
	ActionCoverageOnly                      ActionClass = "COVERAGE_ONLY"
	ActionLegacyFailure                     ActionClass = "LEGACY_FAILURE"
	ActionUnknownInput                      ActionClass = "UNKNOWN_INPUT"
	ActionParticipationAdmittedStakeUnknown ActionClass = "PARTICIPATION_ADMITTED_STAKE_UNKNOWN"
	ActionNoAttemptInSuppliedPrefix         ActionClass = "NO_ATTEMPT_IN_SUPPLIED_PREFIX"
	ActionRefused                           ActionClass = "REFUSED"
	// ActionUnsupportedShape is the fail-closed class: an unknown, new or
	// contradicted native shape. It is never treated as a skip.
	ActionUnsupportedShape ActionClass = "UNSUPPORTED_SHAPE"
)

// ActionMapping is one native action mapped, with its legality.
type ActionMapping struct {
	MapVersion   string      `json:"mapVersion"`
	Policy       string      `json:"policy"`
	NativeAction string      `json:"nativeAction"`
	Class        ActionClass `json:"class"`
	Legal        bool        `json:"legal"`
	// Illegality names every contradiction found between the native action
	// and the native shape.
	Illegality []string `json:"illegality,omitempty"`
	// SkipReason is the proved native reason behind a POLICY_SKIP.
	SkipReason string `json:"skipReason,omitempty"`
}

// shapeCheck accumulates contradictions.
type shapeCheck struct{ found []string }

func (s *shapeCheck) require(ok bool, name string) {
	if !ok {
		s.found = append(s.found, name)
	}
}

// requireCoherentSelection holds a carried P3b selection to the invariants the
// native mechanism's own contract gives it: the candidate and the outcome are
// slice indices inside the ceilings the producer refuses past and each carries
// an identity, the basis is one of the two named halves of the mechanism, and
// RuleIndex is the admitting rule's index under DETAILED_RULE or -1 under
// DEFAULT — the producer admits under no other pairing (ordered_rules.go passes
// the matched rule's index with DETAILED_RULE and a literal -1 with DEFAULT;
// ordered_rules_types.go documents the -1).
//
// Every index the selection names is also one the traversal's OWN counters say
// it reached, the candidate it names is the one the evaluation says it stopped
// on, and the whole selection agrees with the evaluation's record of the step
// that admitted it — including that step's own shape, because a witness that
// contradicts itself witnesses nothing.
//
// These are intrinsic: nothing here re-runs the evaluator, recomputes a share
// or a draw, or re-derives any field from an input this map does not take. A
// selection that fails one of them is a shape the mechanism cannot have
// produced.
//
// The set is BOUNDED BY WHAT THE EVALUATION ITSELF CARRIES, and it is worth
// naming what witnesses what rather than calling it closed:
//
//	CandidateIdentity   its presence, StoppedAtCandidate, the admitting visit
//	CandidatePosition   StoppedAtPosition, the admitting trace entry, the visit
//	CandidateIndex      CandidatesConsumed, how many candidates were visited,
//	                    the trace entry, the visit
//	OutcomeIndex        OutcomesConsidered and the trace entry
//	RuleIndex           Basis, RulesConsidered and the trace entry
//	Basis               RuleIndex, and the admitting trace entry's own Step
//	ShareBits           the trace's steps for the admitting slot, and nothing
//	                    else; this reads the admitting one
//	OutcomeIdentity     its presence, and nothing else
//
// Neither of the last two is an oversight, and they are limited by DIFFERENT
// things. OutcomeIdentity is the cost of staying intrinsic: it is contradicted
// only by the candidate's outcome vector, which lives in the STREAM and not in
// the evaluation, and the trace deliberately carries indices rather than
// identities. ShareBits may have intrinsic witnesses to spare, because the trace
// stamps the same share on EVERY step of one evaluated slot — however many steps
// that slot turns out to have, which on a rule that matches first time is one.
// What limits it is not intrinsicness but the same O(len(Trace)) cost named
// below; this reads the admitting step's copy.
//
// What it deliberately does NOT read, so that the boundary is stated rather than
// discovered:
//
//   - the admitting visit's BalanceUse, PoolTotalKnown, PoolTotal and
//     RawWordsConsumedHere;
//   - the CONTENTS of every trace entry and every visit except the last. Their
//     COUNTS are not symmetric and the asymmetry is deliberate: the number of
//     VISITS is pinned, because a candidate iteration appends exactly one visit
//     on every one of its exit paths, so it is the admitting index plus one. The
//     trace is read only for whether an entry exists at all. A candidate
//     refused for too few outcomes appends a visit and NO trace entry, so no
//     count relation against the trace follows from the selection's candidate
//     index. One DOES hold — on a run that admitted, each rule the traversal
//     reached appends exactly one RULE_* step, so those steps number
//     RulesConsidered — and it is unread for a different reason: it is
//     O(len(Trace)), and an honest run that exhausts the work budget retains
//     MaxOrderedRulesWork entries;
//   - BernoulliEvaluations, which is not read in any form, in relation or
//     otherwise; and Stake.Value and Stake.Reason, of which the arms read
//     Presence and hold it to its vocabulary. RawWordsConsumed IS read, but
//     only as one of the two bounds on the admitting step's word index;
//   - the admitting step's RawWordValue on a RULE_DRAW. It is read on a
//     DEFAULT_BOUNDS step, where the producer fixes it at zero because that
//     half spends nothing, and it is not read on the half that may really have
//     drawn a word, because the evaluation says WHICH word was drawn and never
//     what that word was — the words live in the draw trace, not in here;
//   - Cutoff and Qualifications.
//
// Those are the traversal's own bookkeeping, or the caller's framing of the
// input, rather than anything the selection claims; checking them would make
// this a validator for every exported field of an evaluation instead of a
// coherence check on the one thing it maps.
//
// The four digests and DonorRevision are unread for a DIFFERENT reason, and it
// is worth not lumping them in: they are input bindings and a pinned provenance
// header, not bookkeeping. See the note above MapP2Action — binding an
// evaluation to the stream, config and entropy it claims is the caller's job,
// not this map's.
//
// A shape that is self-consistent HERE may still be inconsistent THERE.
//
// And this is a SHAPE CHECK, not an authentication: every relation compares the
// evaluation against itself, so a forger who edits a witness along with the
// field it witnesses defeats that relation. What the relations buy is cost and
// self-consistency. Provenance is established elsewhere — the caller that
// scores a P3b case recomputes the evaluation and then binds it to the stream
// and config digests.
func requireCoherentSelection(s *shapeCheck, ev predictioneval.OrderedRulesEvaluation) {
	sel := ev.Selected
	if sel == nil {
		return // the arm's own NO_SELECTION requirement already names this.
	}
	// An index addresses a slot in a stream the producer refuses past its
	// declared ceilings (ordered_rules_types.go), so an index at or beyond one
	// names a slot no traversal it ran could have reached. Bounding the indices
	// here is also what keeps the counter relations below out of wrapping
	// arithmetic: CandidatesConsumed-1 wraps, and a bounded index cannot meet a
	// wrapped counter.
	candOK := sel.CandidateIndex >= 0
	s.require(candOK, "SELECTION_CANDIDATE_INDEX_NEGATIVE")
	if candOK {
		candOK = sel.CandidateIndex < predictioneval.MaxOrderedRulesCandidates
		s.require(candOK, "SELECTION_CANDIDATE_INDEX_OVER_CEILING")
	}
	outOK := sel.OutcomeIndex >= 0
	s.require(outOK, "SELECTION_OUTCOME_INDEX_NEGATIVE")
	if outOK {
		outOK = sel.OutcomeIndex < predictioneval.MaxOrderedRulesOutcomes
		s.require(outOK, "SELECTION_OUTCOME_INDEX_OVER_CEILING")
	}
	s.require(sel.CandidateIdentity != "", "SELECTION_CANDIDATE_IDENTITY_EMPTY")
	s.require(sel.OutcomeIdentity != "", "SELECTION_OUTCOME_IDENTITY_EMPTY")
	ruleOK := false
	basisOK := true
	switch sel.Basis {
	case predictioneval.SelectionDefault:
		s.require(sel.RuleIndex == -1, "SELECTION_BASIS_CONTRADICTS_RULE_INDEX")
	case predictioneval.SelectionDetailedRule:
		ruleOK = sel.RuleIndex >= 0
		s.require(ruleOK, "SELECTION_BASIS_CONTRADICTS_RULE_INDEX")
		if ruleOK {
			ruleOK = sel.RuleIndex < predictioneval.MaxOrderedRulesRules
			s.require(ruleOK, "SELECTION_RULE_INDEX_OVER_CEILING")
		}
		if ruleOK {
			// The admitting rule is one the traversal actually walked: the
			// producer counts every rule it reaches on the way
			// (RulesConsidered++ at the top of the rule loop) and admits with
			// that same loop's index, so the counter has already passed the
			// index it admits on. The counter is CUMULATIVE across candidates
			// and outcomes, so this is a necessary condition and not an
			// equality; the ceiling above is what bounds it per slot.
			s.require(sel.RuleIndex < ev.RulesConsidered, "SELECTION_CONTRADICTS_RULES_CONSIDERED")
		}
	default:
		basisOK = false
		s.require(false, "SELECTION_BASIS_OUTSIDE_VOCABULARY")
	}
	// The selection names the candidate the traversal STOPPED on: the producer
	// records the stop fields and mints the selection from the same candidate
	// in the same iteration, so a selection naming a candidate the traversal
	// never reached is a shape it cannot have produced. Still intrinsic — the
	// evaluation is compared only against itself, nothing is recomputed. A
	// field already named as malformed above is not named a second time here.
	if candOK {
		s.require(sel.CandidateIndex == ev.CandidatesConsumed-1, "SELECTION_CONTRADICTS_CANDIDATES_CONSUMED")
		// The same index counted a second way. Every candidate iteration appends
		// exactly one visit before it ends, on every one of its exit paths, and
		// the admitting one appends last — so the admitting candidate's index is
		// also one less than the number of visits recorded. A forger has to
		// account for every candidate the traversal visited, not just the one it
		// lands on — its COUNT, that is; the contents of the earlier visits are
		// not read.
		s.require(sel.CandidateIndex == len(ev.Visits)-1, "SELECTION_CONTRADICTS_VISIT_COUNT")
	}
	if sel.CandidateIdentity != "" {
		s.require(sel.CandidateIdentity == ev.StoppedAtCandidate, "SELECTION_CONTRADICTS_STOPPED_CANDIDATE")
	}
	// The same relation on the outcome the selection names, against the same
	// kind of cumulative counter.
	if outOK {
		s.require(sel.OutcomeIndex < ev.OutcomesConsidered, "SELECTION_CONTRADICTS_OUTCOMES_CONSIDERED")
	}
	// A selection exists only where the traversal stopped on the candidate it
	// names: the producer records the stop BEFORE it mints the selection, so a
	// selection beside an evaluation claiming no stop position is a shape it
	// cannot emit — and without this the position relation below would compare
	// against a field the evaluation says it never set.
	s.require(ev.HasStopPosition, "SELECTION_WITHOUT_STOP_POSITION")
	s.require(sel.CandidatePosition == ev.StoppedAtPosition, "SELECTION_CONTRADICTS_STOPPED_POSITION")

	// The evaluation's own record of the step that admitted. The producer
	// appends that step and admits from the same iteration — a matched draw
	// breaks out of the rule loop and admits, a default admits immediately
	// after its own entry, and neither appends again before returning — so the
	// admitting step is the LAST trace entry and it was minted from the same
	// candidate, outcome, rule, position and share as the selection. Comparing
	// against it re-runs nothing; it is the evaluation contradicting itself.
	// It is also the only record of a share: nothing else in the evaluation
	// carries one, and the type's own documentation says the field travels the
	// same way on the entry and on the selection. The trace stamps that share on
	// every step of one evaluated slot, so the evaluation may hold several
	// copies; this compares against the admitting one.
	if len(ev.Trace) == 0 {
		s.require(false, "SELECTION_WITHOUT_ADMITTING_TRACE")
	} else {
		last := ev.Trace[len(ev.Trace)-1]
		s.require(last.Admitted, "SELECTION_TRACE_STEP_NOT_ADMITTING")
		s.require(last.CandidateIndex == sel.CandidateIndex && last.OutcomeIndex == sel.OutcomeIndex &&
			last.RuleIndex == sel.RuleIndex, "SELECTION_CONTRADICTS_ADMITTING_TRACE")
		s.require(last.CandidatePosition == sel.CandidatePosition, "SELECTION_CONTRADICTS_TRACE_POSITION")
		s.require(last.ShareBits == sel.ShareBits, "SELECTION_CONTRADICTS_TRACE_SHARE")
		// The step and the basis are two names for WHICH HALF of the mechanism
		// admitted, and the producer writes them together: a matched draw is a
		// RULE_DRAW step admitted under DETAILED_RULE, the current outcome's
		// default is a DEFAULT_BOUNDS step admitted under DEFAULT. Without this
		// a DEFAULT admission could sit on a step that took a draw at all — and
		// the default half is documented to consume none. It pins WHICH HALF the
		// step claims; the block below then holds the step to that half's own
		// shape.
		if basisOK {
			want := predictioneval.TraceStepDefaultBounds
			if sel.Basis == predictioneval.SelectionDetailedRule {
				want = predictioneval.TraceStepRuleDraw
			}
			s.require(last.Step == want, "SELECTION_CONTRADICTS_ADMITTING_STEP")
		}
		// The step's OWN fields, which the producer fixes per half. Both halves
		// admit only with ComparatorMatched set, though it carries different
		// things: the rule's comparator on a RULE_DRAW, and the in-bounds test
		// on a DEFAULT_BOUNDS, which has no comparator. A RULE_DRAW that
		// admitted took the draw and won it — it may or may not have SPENT a
		// word, since a rate of exactly one succeeds without one — while a
		// DEFAULT_BOUNDS admission takes no draw at all and spends nothing.
		// These hold of the ADMITTING entry only: the producer writes a
		// RULE_DRAW with BernoulliEvaluated false when entropy runs out, and
		// that entry never admits. The entry is already in hand,
		// and it is the witness the selection is being held to, so its own
		// coherence is part of that witness rather than a separate audit.
		switch last.Step {
		case predictioneval.TraceStepRuleDraw:
			s.require(last.ComparatorMatched && last.BernoulliEvaluated && last.BernoulliResult,
				"ADMITTING_STEP_CONTRADICTS_ITS_KIND")
		case predictioneval.TraceStepDefaultBounds:
			s.require(last.ComparatorMatched && !last.BernoulliEvaluated && !last.BernoulliResult &&
				last.RawWordIndex == -1 && last.RawWordValue == 0, "ADMITTING_STEP_CONTRADICTS_ITS_KIND")
		}
		// A word it claims to have spent is one the run says it consumed. -1 is
		// the producer's "no word", so only a non-negative index is bounded.
		if last.RawWordIndex >= 0 {
			// Bounded twice, like every other index here: against the declared
			// ceiling on the supplied trace, and against what the run says it
			// consumed.
			// The counter alone is not enough — nothing else constrains it, so
			// on its own it is a free variable a forger sets to match.
			s.require(last.RawWordIndex < predictioneval.MaxOrderedRulesDrawWords,
				"ADMITTING_STEP_RAW_WORD_OUT_OF_RANGE")
			s.require(last.RawWordIndex < ev.RawWordsConsumed, "ADMITTING_STEP_RAW_WORD_OUT_OF_RANGE")
		}
		s.require(last.RawWordIndex >= -1, "ADMITTING_STEP_RAW_WORD_OUT_OF_RANGE")
	}

	// The second record of the same admission. The producer mints a visit at the
	// top of each candidate iteration from the same candidate the selection is
	// minted from, marks it ADMITTED, and appends no further visit after it, so
	// the final visit names the admitting candidate.
	//
	// Its identity and position are a third copy of values the stop fields
	// already carry. What it adds that nothing else in the evaluation does is
	// that the last visit ADMITTED: an evaluation claiming an admission whose own
	// per-candidate record says NO_MATCH is refused here and nowhere else. An
	// evaluation that records NO visit is refused twice over — here, and by the
	// count relation above, which such an evaluation also contradicts.
	if len(ev.Visits) == 0 {
		s.require(false, "SELECTION_WITHOUT_ADMITTING_VISIT")
	} else {
		v := ev.Visits[len(ev.Visits)-1]
		s.require(v.Verdict == predictioneval.CandidateAdmitted, "SELECTION_VISIT_NOT_ADMITTED")
		s.require(v.CandidateIndex == sel.CandidateIndex && v.CandidateIdentity == sel.CandidateIdentity &&
			v.CandidatePosition == sel.CandidatePosition, "SELECTION_CONTRADICTS_ADMITTING_VISIT")
	}
}

// Nothing here reads the evaluation's digests, so a DIRECT caller of an
// exported Map function gets a shape check and no binding to a stream, a config
// or an entropy trace. The package's own scorer does not rely on that: it
// recomputes the evaluation, maps it, and then refuses the result unless those
// digests match the projection and the verified ruleset. The binding is the
// caller's, and it happens after the map has answered.
//
// MapP2Action maps a native P2 evaluation.
func MapP2Action(ev predictioneval.Evaluation) ActionMapping {
	m := ActionMapping{MapVersion: NativeActionMapVersion, Policy: PolicyP2, NativeAction: ev.Action}
	const (
		exec = predictioneval.StageStateExecuted
		nr   = predictioneval.StageStateNotReached
	)
	var s shapeCheck

	// Under FACTUAL_STEALTH_OFF the stealth stage is either not reached or
	// executed as NOT_APPLICABLE with nothing drawn: it does not apply, no
	// reduction is derived, the realization is sound, and the realization
	// it echoes is the policy amount, which the stage hands on unchanged.
	stealthClean := (ev.Stealth.State == nr && (ev.Stealth.Outcome == "" || ev.Stealth.Outcome == predictioneval.StealthNotApplicable)) ||
		(ev.Stealth.State == exec && ev.Stealth.Outcome == predictioneval.StealthNotApplicable && !ev.Stealth.Applies &&
			ev.Stealth.Reduction == 0 && !ev.Stealth.ReductionDerived && ev.Stealth.RealizationSound &&
			ev.Stealth.Realized == ev.PolicyAmount)
	s.require(stealthClean, "STEALTH_REALIZATION_PRESENT")

	// A path that proceeds past the health gate is legal only under a
	// verdict the vocabulary names as OPEN. The core witnesses the verdict
	// verbatim and gates only on DENIED, so an unknown, empty or NOT_REACHED
	// verdict would otherwise pass here as an open gate; it does not.
	healthOpenVerdict := ev.Health.Verdict == predictioneval.HealthDisabled ||
		ev.Health.Verdict == predictioneval.HealthNoGate || ev.Health.Verdict == predictioneval.HealthAllowed
	healthWitnessedOpen := ev.Health.State == predictioneval.StageStateWitnessed && healthOpenVerdict
	// A gate the path never reached carries no verdict and no reason.
	healthNotReached := ev.Health.State == nr && ev.Health.Verdict == "" && ev.Health.Reason == ""
	gateOpen := ev.StakeGate.State == exec && ev.StakeGate.Reason != predictioneval.GateReserveViolation
	postGateNotReached := ev.Clamp.State == nr && ev.Minimum.State == nr

	// Invariants of the core that hold on every path: the base-stake stage
	// ran exactly when the choice was selected (a choice out of range never
	// enters the stake block); a clamp that did not run has no final; an
	// executed stake gate's reason is one of its vocabulary; a stage the
	// path never ran carries its initial value — no answer, no reason, no
	// amount (the choice starts at index -1, and the stealth stage of a
	// choice out of range is marked NOT_APPLICABLE without running); an
	// executed gate's proposal is the policy amount, and a percent cap
	// reduced it, its allowance being that limit under the zero floor; and
	// under FACTUAL_STEALTH_OFF the policy amount is known exactly when the
	// stake block ran or was skipped by a choice out of range. One
	// invariant of the projection: the minimum threshold is the pinned one
	// on every case it hands to the factset seam, and the seam refuses any
	// other by its values.
	s.require(ev.Choice.Selected == (ev.BaseStake.State != nr), "SELECTION_CONTRADICTS_BASE_STAKE")
	s.require(ev.Clamp.State != nr || !ev.Clamp.HasFinal, "FINAL_STAKE_WITHOUT_CLAMP")
	s.require(ev.Minimum.Threshold == predictioneval.PinnedMinimumStake, "MINIMUM_NOT_PINNED")
	s.require(ev.StakeGate.State != exec || ev.StakeGate.Reason == predictioneval.GateNone ||
		ev.StakeGate.Reason == predictioneval.GatePercent || ev.StakeGate.Reason == predictioneval.GateReserveViolation,
		"GATE_REASON_OUTSIDE_VOCABULARY")
	// The gate was handed the policy amount as its proposal. A percent cap
	// is a real reduction of it: the core caps only when the limit it
	// computed is BELOW the proposal, and publishes as its allowance that
	// limit under its own zero floor — so a negative limit allows zero,
	// which is a skip, not an attempt.
	percentCapped := ev.StakeGate.State == exec && ev.StakeGate.Reason == predictioneval.GatePercent
	s.require(ev.StakeGate.State != exec || ev.StakeGate.Proposed == ev.PolicyAmount, "GATE_PROPOSAL_CONTRADICTS_POLICY_AMOUNT")
	s.require(!percentCapped || ev.StakeGate.Limit < ev.StakeGate.Proposed, "GATE_CAP_DID_NOT_REDUCE_THE_PROPOSAL")
	s.require(!percentCapped || ev.StakeGate.Allowed == max(ev.StakeGate.Limit, 0), "GATE_ALLOWANCE_CONTRADICTS_THE_CAP")
	// The clamp applied exactly when the gate's reason was the percent cap,
	// and its final stake is the gate's allowance when it applied, the
	// policy amount otherwise. The filter applied only at the filter exit.
	clampFollowsGate := ev.Clamp.Applied == (ev.StakeGate.Reason == predictioneval.GatePercent) &&
		((ev.Clamp.Applied && ev.Clamp.FinalAmount == ev.StakeGate.Allowed) || (!ev.Clamp.Applied && ev.Clamp.FinalAmount == ev.PolicyAmount))
	s.require(ev.Clamp.State != exec || clampFollowsGate, "CLAMP_CONTRADICTS_GATE")
	s.require(ev.Filter.State != exec || !ev.Filter.Applied || ev.Action == predictioneval.ActionFilterRejected,
		"FILTER_APPLIED_WITHOUT_A_FILTER_EXIT")
	// A stage that stopped the evaluation carries its stop and nothing it
	// would have answered: the choice at index -1, the base stake and the
	// filter bare, the stake gate bare when UNSUPPORTED and, when
	// INDETERMINATE, holding only the proposal it was handed.
	stopping := func(st string) bool {
		return st == predictioneval.StageStateLegacyFailure || st == predictioneval.StageStateIndeterminate ||
			st == predictioneval.StageStateUnsupported
	}
	stoppedIntact := (!stopping(ev.Choice.State) || ev.Choice == predictioneval.ChoiceStage{State: ev.Choice.State, Index: -1}) &&
		(!stopping(ev.BaseStake.State) || ev.BaseStake == predictioneval.BaseStakeStage{State: ev.BaseStake.State}) &&
		(!stopping(ev.Filter.State) || ev.Filter == predictioneval.FilterStage{State: ev.Filter.State}) &&
		(ev.StakeGate.State != predictioneval.StageStateUnsupported || ev.StakeGate == predictioneval.StakeGateStage{State: ev.StakeGate.State}) &&
		(ev.StakeGate.State != predictioneval.StageStateIndeterminate ||
			ev.StakeGate == predictioneval.StakeGateStage{State: ev.StakeGate.State, Proposed: ev.PolicyAmount})
	s.require(stoppedIntact, "STOPPED_STAGE_CARRIES_AN_ANSWER")
	// The stealth stage of a choice out of range is marked NOT_APPLICABLE
	// without running; any other unreached stealth stage is bare.
	stealthUnreached := predictioneval.StealthStage{State: nr}
	if ev.Choice.State == exec && !ev.Choice.Selected {
		stealthUnreached.Outcome = predictioneval.StealthNotApplicable
	}
	unreachedIntact := (ev.Choice.State != nr || ev.Choice == predictioneval.ChoiceStage{State: nr, Index: -1}) &&
		(ev.BaseStake.State != nr || ev.BaseStake == predictioneval.BaseStakeStage{State: nr}) &&
		(ev.Stealth.State != nr || ev.Stealth == stealthUnreached) &&
		(ev.Filter.State != nr || ev.Filter == predictioneval.FilterStage{State: nr}) &&
		(ev.Health.State != nr || healthNotReached) &&
		(ev.StakeGate.State != nr || ev.StakeGate == predictioneval.StakeGateStage{State: nr}) &&
		(ev.Clamp.State != nr || ev.Clamp == predictioneval.ClampStage{State: nr}) &&
		(ev.Minimum.State != nr || !ev.Minimum.Below)
	s.require(unreachedIntact, "UNREACHED_STAGE_CARRIES_A_VALUE")
	s.require(ev.PolicyAmountKnown == (ev.Choice.State == exec && (ev.BaseStake.State == exec || !ev.Choice.Selected)),
		"AMOUNT_KNOWN_CONTRADICTS_STAKE_BLOCK")

	// The stage states in evaluation order, for the results that stop the
	// evaluator: the one stopping stage is the only stage in a stopping
	// state, and every stage after it is NOT_REACHED.
	stageStates := []string{ev.Choice.State, ev.BaseStake.State, ev.Stealth.State, ev.Filter.State,
		ev.Health.State, ev.StakeGate.State, ev.Clamp.State, ev.Minimum.State}
	const choiceStage, baseStakeStage, stealthStage, filterStage, healthStage, stakeGateStage, minimumStage = 0, 1, 2, 3, 4, 5, 7
	// stopAt returns the first of the candidate stages in state st, or -1.
	stopAt := func(st string, candidates ...int) int {
		for _, i := range candidates {
			if stageStates[i] == st {
				return i
			}
		}
		return -1
	}
	onlyStop := func(i int) bool {
		for j, st := range stageStates {
			if j != i && stopping(st) {
				return false
			}
		}
		return true
	}
	unreachedAfter := func(i int) bool {
		for _, st := range stageStates[i+1:] {
			if st != nr {
				return false
			}
		}
		return true
	}
	// executedBefore reports whether every stage before stage i ran as the
	// path runs it: the choice and the filter executed, the base stake and
	// the stealth stage exactly when the choice was selected (a choice out
	// of range skips the stake block). The health gate is bound by each
	// arm's own clause.
	executedBefore := func(i int) bool {
		return (i <= choiceStage || ev.Choice.State == exec) &&
			(i <= baseStakeStage || (ev.BaseStake.State == exec) == ev.Choice.Selected) &&
			(i <= stealthStage || (ev.Stealth.State == exec) == ev.Choice.Selected) &&
			(i <= filterStage || ev.Filter.State == exec)
	}

	switch ev.Action {
	case predictioneval.ActionWouldAttemptPlacement:
		m.Class = ActionWouldAttempt
		s.require(ev.Choice.State == exec && ev.Choice.Selected, "CHOICE_NOT_SELECTED")
		s.require(ev.BaseStake.State == exec, "BASE_STAKE_NOT_EXECUTED")
		s.require(ev.Stealth.State == exec, "STEALTH_STAGE_NOT_EXECUTED")
		s.require(ev.Filter.State == exec && !ev.Filter.Skip && !ev.Filter.Applied, "FILTER_CONTRADICTS_PLACEMENT")
		s.require(healthWitnessedOpen, "HEALTH_CONTRADICTS_PLACEMENT")
		s.require(gateOpen, "STAKE_GATE_CONTRADICTS_PLACEMENT")
		s.require(ev.Clamp.State == exec && ev.Clamp.HasFinal, "NO_FINAL_STAKE")
		s.require(ev.Minimum.State == exec && !ev.Minimum.Below, "MINIMUM_CONTRADICTS_PLACEMENT")
		s.require(ev.PolicyAmountKnown, "POLICY_AMOUNT_UNKNOWN")
		s.require(ev.LegacyFailure == "", "LEGACY_FAILURE_PRESENT")
	case predictioneval.ActionHealthGated:
		m.Class, m.SkipReason = ActionPolicySkip, ev.Action
		s.require(executedBefore(healthStage), "STAGES_NOT_RUN_BEFORE_EXIT")
		s.require(ev.Health.State == predictioneval.StageStateWitnessed && ev.Health.Verdict == predictioneval.HealthDenied, "HEALTH_NOT_DENIED")
		s.require(ev.StakeGate.State == nr && postGateNotReached, "STAGES_AFTER_HEALTH_REACHED")
		s.require(ev.LegacyFailure == "", "LEGACY_FAILURE_PRESENT")
	case predictioneval.ActionReserveViolation:
		m.Class, m.SkipReason = ActionPolicySkip, ev.Action
		s.require(executedBefore(stakeGateStage), "STAGES_NOT_RUN_BEFORE_EXIT")
		s.require(healthWitnessedOpen, "HEALTH_CONTRADICTS_GATE")
		s.require(ev.StakeGate.State == exec && ev.StakeGate.Reason == predictioneval.GateReserveViolation, "STAKE_GATE_NOT_RESERVE_VIOLATION")
		s.require(postGateNotReached, "STAGES_AFTER_RESERVE_VIOLATION_REACHED")
		s.require(ev.LegacyFailure == "", "LEGACY_FAILURE_PRESENT")
	case predictioneval.ActionBelowMinimum:
		m.Class, m.SkipReason = ActionPolicySkip, ev.Action
		s.require(ev.Filter.State == exec && !ev.Filter.Applied, "FILTER_CONTRADICTS_MINIMUM_EXIT")
		s.require(executedBefore(minimumStage), "STAGES_NOT_RUN_BEFORE_EXIT")
		s.require(healthWitnessedOpen, "HEALTH_CONTRADICTS_GATE")
		s.require(gateOpen, "STAKE_GATE_CONTRADICTS_MINIMUM_EXIT")
		s.require(ev.Clamp.State == exec && ev.Clamp.HasFinal, "NO_FINAL_STAKE")
		s.require(ev.Minimum.State == exec && ev.Minimum.Below, "MINIMUM_NOT_BELOW")
		s.require(ev.PolicyAmountKnown, "POLICY_AMOUNT_UNKNOWN")
		s.require(ev.LegacyFailure == "", "LEGACY_FAILURE_PRESENT")
	case predictioneval.ActionFilterRejected:
		m.Class, m.SkipReason = ActionPolicySkip, ev.Action
		s.require(ev.Filter.State == exec && ev.Filter.Skip && ev.Filter.Applied, "FILTER_NOT_APPLIED")
		s.require(executedBefore(minimumStage), "STAGES_NOT_RUN_BEFORE_EXIT")
		s.require(healthWitnessedOpen, "HEALTH_CONTRADICTS_GATE")
		s.require(gateOpen, "STAKE_GATE_CONTRADICTS_FILTER_EXIT")
		s.require(ev.Clamp.State == exec && ev.Clamp.HasFinal, "NO_FINAL_STAKE")
		s.require(ev.Minimum.State == exec && !ev.Minimum.Below, "MINIMUM_CONTRADICTS_FILTER_EXIT")
		s.require(ev.PolicyAmountKnown, "POLICY_AMOUNT_UNKNOWN")
		s.require(ev.LegacyFailure == "", "LEGACY_FAILURE_PRESENT")
	case predictioneval.ActionPreDecisionExit:
		m.Class = ActionCoverageOnly
		s.require(ev.Choice.State == nr && ev.BaseStake.State == nr && ev.Stealth.State == nr &&
			ev.Filter.State == nr && healthNotReached && ev.StakeGate.State == nr && postGateNotReached,
			"STAGES_REACHED_BEFORE_DECISION")
		s.require(ev.LegacyFailure == "", "LEGACY_FAILURE_PRESENT")
	// The three results below can stop the evaluator on either side of the
	// health gate; the one stopping stage decides where the gate must stand
	// and ends the evaluation. Every legacy-failure site and the choice and
	// base-stake stops precede the gate, which is then NOT_REACHED; a
	// stake-gate stop lies past it and is reachable only through a witnessed
	// open verdict; the stages before the stop ran as the path runs them, no
	// other stage is in a stopping state, and every stage after the stop is
	// NOT_REACHED.
	case predictioneval.ActionLegacyFailure:
		m.Class = ActionLegacyFailure
		s.require(ev.LegacyFailure != "", "LEGACY_FAILURE_UNNAMED")
		stop := stopAt(predictioneval.StageStateLegacyFailure, choiceStage, baseStakeStage, filterStage)
		s.require(stop >= 0, "NO_STAGE_FAILED")
		s.require(stop < 0 || executedBefore(stop), "STAGES_NOT_RUN_BEFORE_STOP")
		s.require(stop < 0 || onlyStop(stop), "ANOTHER_STAGE_STOPPED")
		s.require(healthNotReached, "HEALTH_REACHED_AFTER_LEGACY_FAILURE")
		s.require(stop < 0 || unreachedAfter(stop), "STAGES_REACHED_AFTER_LEGACY_FAILURE")
	case predictioneval.ActionIndeterminate:
		m.Class = ActionUnknownInput
		stop := stopAt(predictioneval.StageStateIndeterminate, baseStakeStage, stakeGateStage)
		s.require(stop >= 0, "NO_STAGE_INDETERMINATE")
		s.require(stop < 0 || executedBefore(stop), "STAGES_NOT_RUN_BEFORE_STOP")
		s.require(stop < 0 || onlyStop(stop), "ANOTHER_STAGE_STOPPED")
		s.require(stop < 0 || (stop == baseStakeStage && healthNotReached) || (stop == stakeGateStage && healthWitnessedOpen),
			"HEALTH_CONTRADICTS_INDETERMINATE_STAGE")
		s.require(stop < 0 || unreachedAfter(stop), "STAGES_REACHED_AFTER_INDETERMINATE_STAGE")
		s.require(ev.LegacyFailure == "", "LEGACY_FAILURE_PRESENT")
	case predictioneval.ActionUnsupported:
		m.Class = ActionUnknownInput
		stop := stopAt(predictioneval.StageStateUnsupported, choiceStage, stakeGateStage)
		s.require(stop >= 0, "NO_STAGE_UNSUPPORTED")
		s.require(stop < 0 || executedBefore(stop), "STAGES_NOT_RUN_BEFORE_STOP")
		s.require(stop < 0 || onlyStop(stop), "ANOTHER_STAGE_STOPPED")
		s.require(stop < 0 || (stop == choiceStage && healthNotReached) || (stop == stakeGateStage && healthWitnessedOpen),
			"HEALTH_CONTRADICTS_UNSUPPORTED_STAGE")
		s.require(stop < 0 || unreachedAfter(stop), "STAGES_REACHED_AFTER_UNSUPPORTED_STAGE")
		s.require(ev.LegacyFailure == "", "LEGACY_FAILURE_PRESENT")
	default:
		m.Class = ActionUnsupportedShape
		s.found = append(s.found, "UNKNOWN_NATIVE_ACTION")
	}
	return s.finish(m)
}

// finish seals the mapping: any contradiction fails closed.
func (s *shapeCheck) finish(m ActionMapping) ActionMapping {
	if len(s.found) > 0 {
		m.Class = ActionUnsupportedShape
		m.Legal = false
		m.Illegality = s.found
		m.SkipReason = ""
		return m
	}
	m.Legal = true
	return m
}

// MapP3bAction maps a native P3b evaluation.
//
// Every status keeps its own class: PARTICIPATION_ADMITTED_STAKE_UNKNOWN and
// NO_ATTEMPT_IN_SUPPLIED_PREFIX are distinct from each other and from every
// P2 class, and none of them is ever POLICY_SKIP.
func MapP3bAction(ev predictioneval.OrderedRulesEvaluation) ActionMapping {
	m := ActionMapping{MapVersion: NativeActionMapVersion, Policy: PolicyP3b, NativeAction: string(ev.Status)}
	var s shapeCheck
	s.require(ev.EvidenceLabel == predictioneval.OrderedRulesEvidenceLabel, "EVIDENCE_LABEL_FOREIGN")
	s.require(ev.ModelVersion == predictioneval.OrderedRulesModelVersion, "MODEL_VERSION_FOREIGN")
	s.require(ev.EntropySemanticsVersion == predictioneval.OrderedRulesEntropySemanticsVersion, "ENTROPY_SEMANTICS_FOREIGN")
	// Both are closed vocabularies, and the two booleans below are EQUALITY
	// tests — so a value outside its vocabulary reads as the negative state and
	// quietly satisfies every `!admitted` and `!stakeKnown` requirement the arms
	// make. Bounding them first is what makes those requirements mean what they
	// say.
	s.require(ev.Participation == predictioneval.ParticipationAdmitted ||
		ev.Participation == predictioneval.ParticipationNotAdmitted, "PARTICIPATION_OUTSIDE_VOCABULARY")
	s.require(ev.Stake.Presence == predictioneval.SuppliedKnown ||
		ev.Stake.Presence == predictioneval.SuppliedMissing ||
		ev.Stake.Presence == predictioneval.SuppliedInvalid, "STAKE_PRESENCE_OUTSIDE_VOCABULARY")
	admitted := ev.Participation == predictioneval.ParticipationAdmitted
	stakeKnown := ev.Stake.Presence == predictioneval.SuppliedKnown
	// The producer initialises Stake to {MISSING, NOT_EVALUATED} at both of its
	// construction sites and overwrites it ONLY where it admits, so every status
	// but the two admitting ones carries MISSING. The two admitting arms below
	// constrain the presence themselves.
	switch ev.Status {
	case predictioneval.StatusWouldAttempt, predictioneval.StatusParticipationAdmittedStakeUnknown:
	default:
		s.require(ev.Stake.Presence == predictioneval.SuppliedMissing, "STAKE_PRESENCE_CONTRADICTS_STATUS")
	}
	switch ev.Status {
	case predictioneval.StatusWouldAttempt:
		m.Class = ActionWouldAttempt
		s.require(admitted, "PARTICIPATION_NOT_ADMITTED")
		s.require(ev.Selected != nil, "NO_SELECTION")
		requireCoherentSelection(&s, ev)
		s.require(stakeKnown, "STAKE_NOT_KNOWN")
		s.require(ev.Reason == "", "REASON_ON_ATTEMPT")
		// requireCoherentSelection requires HasStopPosition whenever a
		// selection is CARRIED, so repeating it here unconditionally would
		// report one contradiction under two names. It returns early when there
		// is no selection, though, and that shape still needs the stop position
		// named — hence the guard rather than a plain drop.
		s.require((ev.Selected != nil || ev.HasStopPosition) && ev.CandidatesConsumed >= 1, "NO_CANDIDATE_REACHED")
	case predictioneval.StatusParticipationAdmittedStakeUnknown:
		m.Class = ActionParticipationAdmittedStakeUnknown
		s.require(admitted, "PARTICIPATION_NOT_ADMITTED")
		s.require(ev.Selected != nil, "NO_SELECTION")
		requireCoherentSelection(&s, ev)
		s.require(!stakeKnown, "STAKE_KNOWN_ON_UNKNOWN_STATUS")
		s.require(ev.Reason == predictioneval.ReasonBalanceNotSupplied || ev.Reason == predictioneval.ReasonBalanceInvalid ||
			ev.Reason == predictioneval.ReasonBalanceOutOfDomain, "STAKE_UNKNOWN_REASON_FOREIGN")
		// The producer writes the presence and the reason together, in one
		// switch: a balance that was not supplied is MISSING, one that was
		// invalid or outside the u32 domain is INVALID. A pairing it does not
		// write is a shape it did not produce.
		switch ev.Reason {
		case predictioneval.ReasonBalanceNotSupplied:
			s.require(ev.Stake.Presence == predictioneval.SuppliedMissing, "STAKE_PRESENCE_CONTRADICTS_REASON")
		case predictioneval.ReasonBalanceInvalid, predictioneval.ReasonBalanceOutOfDomain:
			s.require(ev.Stake.Presence == predictioneval.SuppliedInvalid, "STAKE_PRESENCE_CONTRADICTS_REASON")
		}
	case predictioneval.StatusNoAttemptInSuppliedPrefix:
		m.Class = ActionNoAttemptInSuppliedPrefix
		s.require(!admitted, "PARTICIPATION_ADMITTED_ON_NO_ATTEMPT")
		s.require(ev.Selected == nil, "SELECTION_ON_NO_ATTEMPT")
		s.require(!stakeKnown, "STAKE_ON_NO_ATTEMPT")
		s.require(ev.Reason == "", "REASON_ON_NO_ATTEMPT")
	case predictioneval.StatusUnknownInput:
		m.Class = ActionUnknownInput
		s.require(!admitted, "PARTICIPATION_ADMITTED_ON_UNKNOWN_INPUT")
		s.require(ev.Selected == nil, "SELECTION_ON_UNKNOWN_INPUT")
		s.require(!stakeKnown, "STAKE_ON_UNKNOWN_INPUT")
		s.require(ev.Reason != "", "UNKNOWN_INPUT_WITHOUT_REASON")
	case predictioneval.StatusRefused:
		m.Class = ActionRefused
		s.require(!admitted, "PARTICIPATION_ADMITTED_ON_REFUSAL")
		s.require(ev.Selected == nil, "SELECTION_ON_REFUSAL")
		s.require(!stakeKnown, "STAKE_ON_REFUSAL")
		s.require(ev.Reason != "", "REFUSAL_WITHOUT_REASON")
	default:
		m.Class = ActionUnsupportedShape
		s.found = append(s.found, "UNKNOWN_NATIVE_STATUS")
	}
	return s.finish(m)
}
