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
//
// IT CARRIES NO CASE AND NO BINDING. A mapping says what a supplied evaluation
// SHAPE is; it does not say which stream, config or entropy run produced that
// evaluation, and there is no field here for a caller to check one against.
// Reading `m.Class == ActionWouldAttempt && m.Legal` as "this policy would have
// attempted on THIS case" is therefore a step the type does not support: an
// evaluation produced over a different candidate stream maps to exactly the
// same fully-populated, legal-looking value.
//
// What makes the package's own use safe is not this type. A [PolicyDecision]
// can be minted only by [P2CaseResult.Decision] / [P3bCaseResult.Decision]
// from the evaluator's own witnessed result, and the scorer binds the
// evaluation's digests to the projection and the verified ruleset AFTER
// mapping. A caller who consumes an ActionMapping directly has neither.
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
//   - the admitting visit's RawWordsConsumedHere. Its BalanceUse and its
//     PoolTotalKnown used to be listed here and are now READ, as is PoolTotal's
//     SIGN; only the total's VALUE stays unread. BalanceUse because
//     admitOrderedRules writes it and the status in one switch, so on an
//     admitting visit it is an exact function of the status and reason;
//     PoolTotalKnown because checkedPoolSum sets it before the outcome loop, so
//     an admitting visit cannot say the pool went unsummed;
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
//   - Stake.Value and Stake.Reason, of which the arms read Presence and hold
//     it to its vocabulary. THE ARMS, by contrast, now read both: the value is
//     required zero wherever no stake was sized, and the reason is bound on
//     every non-admitting status and on two of the four admitting ones — see
//     the field matrix. RawWordsConsumed IS read, twice: on its own range, at
//     the top of MapP3bAction and for every status, and again as one of the two
//     bounds on the admitting step's word index. The own-range test is the
//     later of the two and exists because the index test skipped the field
//     entirely whenever the admitting step consumed no word. BernoulliEvaluations was
//     listed here as read "in no form, in relation or otherwise" and now IS
//     read, in one direction only: a DETAILED_RULE selection requires at least
//     one, because the producer increments it below the rate branch and so
//     advances it even where the rate is exactly one and no word is drawn. The
//     converse is not asserted here — a DEFAULT selection may carry any count
//     this arm is concerned with, the header range apart,
//     since rules that failed on an earlier outcome advance it too;
//   - the admitting step's RawWordValue on a RULE_DRAW that really DREW,
//     because the evaluation says WHICH word was drawn and never what that
//     word was — the words live in the draw trace, not in here. It IS read on
//     a DEFAULT_BOUNDS step and, since the repair this line records, on a
//     RULE_DRAW whose own index is the no-word sentinel: both are halves where
//     the producer fixes it at zero, so a nonzero value there is the entry
//     contradicting itself rather than a word this package declines to judge;
//   - Cutoff and Qualifications.
//
// Those are the traversal's own bookkeeping, or the caller's framing of the
// input, rather than anything the selection claims; checking them would make
// this a validator for every exported field of an evaluation instead of a
// coherence check on the one thing it maps.
//
// The four digests are unread for a DIFFERENT reason, and it is worth not
// lumping them in. (DonorRevision used to be named here too. It is now READ,
// beside its three sibling header constants — leaving it unchecked was an
// omission rather than the decision this paragraph describes.) They are input
// bindings and a consumed-prefix attestation, not bookkeeping. See the note above MapP2Action — binding an
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
		// A detailed rule admits only by winning its draw, and the producer
		// increments BernoulliEvaluations on the path BELOW the rate branch —
		// so the counter advances even where the rate is exactly one and no
		// word is drawn. Every detailed-rule admission has therefore passed it
		// at least once. The converse does NOT hold and is not asserted: rules
		// that failed on an earlier outcome advance the counter and the default
		// half still admits, so a DEFAULT selection may carry any count inside
		// the range the header already bounds it to.
		s.require(ev.BernoulliEvaluations >= 1, "SELECTION_BASIS_CONTRADICTS_BERNOULLI_COUNT")
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
			// The value is unread on a step that really DREW, because the
			// evaluation says which word was drawn and never what it was. That
			// reasoning stops at the entry's own sentinel: where the step says
			// no word was drawn, the producer fixes the value at zero exactly
			// as it does on the default half, so a nonzero value beside -1 is
			// the entry contradicting itself rather than a word this package
			// declines to judge.
			s.require(last.RawWordIndex != -1 || last.RawWordValue == 0,
				"ADMITTING_STEP_RAW_WORD_VALUE_WITHOUT_WORD")
		case predictioneval.TraceStepDefaultBounds:
			// Split for the same reason the raw-word bounds were: bundling
			// distinct predicates under one name says which of them failed
			// none of the times, and lets them mask each other in a ledger.
			s.require(last.ComparatorMatched && !last.BernoulliEvaluated && !last.BernoulliResult,
				"ADMITTING_STEP_CONTRADICTS_ITS_KIND")
			s.require(last.RawWordIndex == -1, "ADMITTING_STEP_DEFAULT_SPENT_A_WORD")
			s.require(last.RawWordValue == 0, "ADMITTING_STEP_RAW_WORD_VALUE_WITHOUT_WORD")
		}
		// A word it claims to have spent is one the run says it consumed. -1 is
		// the producer's "no word", so only a non-negative index is bounded.
		if last.RawWordIndex >= 0 {
			// Bounded twice, like every other index here: against the declared
			// ceiling on the supplied trace, and against what the run says it
			// consumed.
			// The counter alone is not enough. It is now held to its own range
			// at the top of this function, but that leaves it free WITHIN the
			// range, so on its own it is still a value a forger sets to match
			// the index beside it.
			// Each bound reports under its OWN name. They shared one, and
			// require appends without deduplicating, so a shape both refuse
			// named the same identifier twice and said which bound failed
			// neither time. It is not only a reporting defect: the two masked
			// each other in the mutation ledger, and one survivor was found
			// only after a fixture was built to separate them by hand.
			s.require(last.RawWordIndex < predictioneval.MaxOrderedRulesDrawWords,
				"ADMITTING_STEP_RAW_WORD_OVER_CEILING")
			s.require(last.RawWordIndex < ev.RawWordsConsumed, "ADMITTING_STEP_RAW_WORD_NOT_CONSUMED")
		}
		s.require(last.RawWordIndex >= -1, "ADMITTING_STEP_RAW_WORD_BELOW_NO_WORD")
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
		// The visit's BalanceUse and the evaluation's status are written by the
		// SAME switch in admitOrderedRules, one arm each, so on an admitting
		// visit the first is an exact function of the second. NOT_EVALUATED is
		// what a visit carries BEFORE that switch runs and the admitting visit
		// is appended after it, so it cannot survive onto one.
		//
		// The pairing is checked only where the status names one. A status the
		// arms already refuse names no balance use, and reporting it twice
		// under two names would describe one contradiction as two.
		if want, named := admittingBalanceUse(ev); named {
			s.require(v.BalanceUse == want, "SELECTION_VISIT_CONTRADICTS_BALANCE_USE")
		}
		// checkedPoolSum runs BEFORE the outcome loop and returns early on
		// failure, so PoolTotalKnown is set true on every visit that can go on
		// to admit at all. A false flag on an ADMITTING visit is the
		// evaluation's own record saying the pool could not be summed, which
		// the producer emits as UNKNOWN_INPUT and never as an admission.
		//
		// The flag is a presence bit, as intrinsic and as cheap to read as
		// HasStopPosition. It used to be classified with the VALUE beside it,
		// under a justification — recomputing the sum would reimplement the
		// policy — that is true of the total and not of the flag. The total
		// itself stays unread except for its sign, which checkedPoolSum
		// guarantees by refusing negative points outright.
		s.require(v.PoolTotalKnown, "SELECTION_VISIT_POOL_NOT_SUMMED")
		s.require(v.PoolTotal >= 0, "SELECTION_VISIT_POOL_TOTAL_NEGATIVE")
	}
}

// MapP2Action maps a native P2 evaluation. IT IS A SHAPE CHECK AND NOT A
// BINDING: nothing here reads the evaluation's digests, so a direct caller
// gets no tie to a stream, a config or an entropy trace, and the returned
// [ActionMapping] has no field that names the case. See that type's doc; the
// package's own scorer recomputes the evaluation, maps it, and only then
// refuses the result unless the digests match the projection and the verified
// ruleset.
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

// THE FIELD / STATUS MATRIX FOR SEAM C.
//
// The engineering contract for this seam is: a native evaluation plus its exact
// bindings maps to an exhaustive p4-native-action-map/v1 result, or to a typed
// refusal for a shape the producer cannot have written. That promise is only as
// good as its coverage, so every field of every native structure this mapper
// reads is classified here, exactly once, as one of:
//
//	A  VALIDATED_LOAD_BEARING       — a native invariant names it and it is checked.
//	B  INTENTIONALLY_NON_AUTHORITATIVE — changing it cannot contradict the action
//	                                   shape this seam promises, and no
//	                                   authoritative value is derived from it here.
//
// No field is left accidentally unread: the census is pinned by a reflection
// test that fails if the native structures gain a field, which forces the new
// one to be classified rather than silently ignored. 56 fields, 49 A, 7 B.
//
// The census test pins the field NAMES and the total; it does not pin the A/B
// split, so the counts below are prose and were wrong once already — an
// independent lane caught a sub-total that contradicted both its own list and
// the global figure.
//
// The matrix is NOT an authentication scheme and does not try to become one.
// Every relation compares the evaluation against another part of the SAME
// evaluation, so a forger who edits a witness and the field it witnesses
// together defeats the relation that uses them. Provenance is the caller's job
// and the trust boundary is stated in doc.go; the scorer recomputes the
// evaluation and binds it to the stream and config digests after the map has
// answered.
//
// OrderedRulesEvaluation (25) — 19 A, 6 B
//
//	A EvidenceLabel, ModelVersion, EntropySemanticsVersion, DonorRevision
//	    the four pinned header constants the producer stamps.
//	A Status                exhaustive; an unknown one is UNSUPPORTED_SHAPE.
//	A Participation, Stake  closed vocabularies, bounded BEFORE the arms derive
//	                        their negative booleans by equality.
//	A Reason                per-status closed vocabulary, transcribed from the
//	                        emitting sites; empty exactly where the producer
//	                        writes none.
//	A Selected              present exactly on the two admitting statuses.
//	A HasStopPosition, StoppedAtCandidate, StoppedAtPosition
//	                        the stop the producer records before minting a
//	                        selection, so the two cannot disagree — and, since
//	                        that record is written at the top of the candidate
//	                        loop rather than beside the selection, held on EVERY
//	                        status: present exactly when a candidate was
//	                        consumed, and blank together when none was. The
//	                        POSITION's sign is deliberately unheld; a scope may
//	                        declare negative positions.
//	A CandidatesConsumed, OutcomesConsidered, RulesConsidered
//	                        each bounded on its own producer range, at the top
//	                        of MapP3bAction and for every status, and
//	                        additionally -- where a selection exists -- required
//	                        to have reached the index that selection admits on
//	                        — an equality for CandidatesConsumed, a strict
//	                        inequality for the other two.
//	                        The own-range tests came later: the index relations
//	                        are inside requireCoherentSelection, so they left all
//	                        three unread on every terminal status and on the
//	                        DEFAULT half.
//	A BernoulliEvaluations  bounded on its own producer range for every status,
//	                        and at least one on a DETAILED_RULE admission; the
//	                        producer advances it below the rate branch, inside
//	                        the work-guarded rule loop.
//	A RawWordsConsumed      bounded on its own range, for every status, and
//	                        additionally one of the two bounds on the admitting
//	                        word index. The own-range test was added after the
//	                        index test was found to skip it entirely whenever
//	                        the admitting step consumed no word.
//	A Trace, Visits         the two admitting witnesses — and, on every status,
//	                        Visits is held to one entry per candidate consumed,
//	                        which is what every exit path of the candidate loop
//	                        appends. Trace is held only where the producer fixes
//	                        it: empty on a refusal that never entered the loop.
//	                        It is NOT tied to the candidate count, because a
//	                        candidate refused for too few outcomes appends a
//	                        visit and no trace entry.
//	B StreamDigest, ConfigDigest, EntropyDigest, ConsumedInputDigest
//	                        input bindings and a consumed-prefix attestation.
//	                        They say WHICH inputs produced this, never what the
//	                        action is; the scorer binds them after mapping, and
//	                        nothing here derives a value from them. That is the
//	                        whole reason they stay B, and it is the only reason:
//	                        no digest can make an admitted shape unadmitted or
//	                        the reverse.
//
//	                        THIS ENTRY USED TO GIVE A SECOND REASON AND IT WAS
//	                        FALSE. It said the producer withholds these on its
//	                        unread refusal tier, so a refusal carrying a digest
//	                        it could not have earned was producer-impossible.
//	                        The producer does not tier them that way, and an
//	                        independent lane executed the counterexample. There
//	                        are TWO pre-traversal helpers, not one: the
//	                        package-level orderedRulesUnreadRefusal builds a
//	                        FRESH result and carries nothing, while the local
//	                        refuseUnread closure deliberately keeps what was
//	                        earned — the producer's own comment calls that the
//	                        distinction the convention exists to carry, and its
//	                        StreamDigest is in the result literal from the
//	                        start. DRAW_WORDS_OVER_BOUND, RULE_COUNT_OVER_BOUND
//	                        and SUPPLIED_TEXT_NOT_ENCODABLE reach the closure,
//	                        and STREAM_INVARIANT_VIOLATED reaches BOTH helpers
//	                        from different sites — so the reason does not even
//	                        determine the tier. A refusal carrying a stream
//	                        digest is ordinary producer output.
//	B Cutoff, Qualifications
//	                        derived fields, and the same correction applies to
//	                        them, one step narrower. They are assigned once the
//	                        stream is shown derivable, and the three closure
//	                        refusals above that assignment therefore carry
//	                        neither — but SUPPLIED_TEXT_NOT_ENCODABLE sits BELOW
//	                        it, so a REFUSED evaluation carrying an established
//	                        cutoff and a qualifications list is exactly what the
//	                        producer emits for an unencodable ConfigID. It was
//	                        called producer-impossible here; it is not.
//	                        They stay B for the reason that was always true:
//	                        neither can make an admitted shape unadmitted or the
//	                        reverse.
//
// OrderedRulesSelection (8) — 8 A, 0 B
//
//	A CandidateIndex, OutcomeIndex, RuleIndex   non-negative, inside the
//	    ceilings the producer itself refuses past, and reached by the
//	    traversal's own counters.
//	A Basis                 one of two, and pairs with RuleIndex as the producer
//	                        pairs them (DEFAULT ⇒ -1).
//	A CandidateIdentity, OutcomeIdentity        non-empty and, for the
//	    candidate, equal to the admitting visit's.
//	A CandidatePosition, ShareBits              equal to the admitting trace
//	    entry's and to the recorded stop.
//
// OrderedRulesTraceEntry, the ADMITTING entry only (12) — 12 A, 0 B
//
//	A Admitted, Step        the last entry admitted, and its step names the same
//	                        half as the basis.
//	A CandidateIndex, OutcomeIndex, RuleIndex, CandidatePosition, ShareBits
//	                        equal to the selection's.
//	A ComparatorMatched, BernoulliEvaluated, BernoulliResult
//	                        the shape each half admits with; a RULE_DRAW that
//	                        admitted won its draw, a DEFAULT_BOUNDS took none.
//	A RawWordIndex          -1 or bounded twice, under its own name per bound.
//	A RawWordValue          zero on DEFAULT_BOUNDS and on a RULE_DRAW carrying
//	                        the no-word sentinel. On a step that really drew it
//	                        is deliberately unread — the evaluation says WHICH
//	                        word was drawn, never what it was — which is a
//	                        scoped B inside an otherwise A field. It is not the
//	                        only scoped entry. The counters, the stop fields,
//	                        Trace and Visits WERE read only when a selection was
//	                        carried, which left them unread on every terminal
//	                        status; the header now reads all of them on every
//	                        status, and requireCoherentSelection keeps the
//	                        relations that need an index to point at. The
//	                        visit-per-iteration relation this entry used to name
//	                        as unchecked — len(Visits) equals CandidatesConsumed
//	                        — is one of the header relations.
//	  The CONTENTS of every non-final entry are B: a candidate refused for too
//	  few outcomes appends a visit and no trace entry, so no count relation
//	  against the trace follows from the selection, and reading them would be
//	  O(len(Trace)) on a run that legitimately retains MaxOrderedRulesWork.
//
// OrderedRulesCandidateVisit, the ADMITTING visit only (8) — 7 A, 1 B
//
//	A Verdict               the last visit ADMITTED.
//	A CandidateIndex, CandidateIdentity, CandidatePosition
//	                        equal to the selection's, and the NUMBER of visits
//	                        accounts for the candidate it names.
//	A BalanceUse            an exact function of status and reason:
//	                        admitOrderedRules writes both in one switch.
//	A PoolTotalKnown        true on every admitting visit: checkedPoolSum runs
//	                        before the outcome loop and returns early on
//	                        failure. A presence bit, not the value beside it —
//	                        it was classified B under the value's justification,
//	                        which is the kind of over-broad reasoning this
//	                        matrix exists to surface.
//	A PoolTotal             its SIGN only, which checkedPoolSum guarantees by
//	                        refusing negative points outright. The VALUE stays
//	                        B: recomputing the sum would reimplement the policy.
//	B RawWordsConsumedHere  a per-candidate slice of the evaluation-level
//	                        counter. It DOES carry relations that are not
//	                        checked — it is never negative, never exceeds
//	                        RawWordsConsumed, and equals it exactly on an
//	                        admitting visit at candidate 0 — so the accurate
//	                        reason for leaving it unread is consequence, not
//	                        redundancy: it is bookkeeping, and no action shape
//	                        this seam promises depends on it.
//
// SuppliedUint32, the Stake (3) — 3 A, 0 B
//
//	A Presence              bounded to its three-member vocabulary and bound to
//	                        the status on every arm.
//	A Reason                fixed at NOT_EVALUATED on every non-admitting
//	                        status, by the same initialisation as the presence;
//	                        empty on WOULD_ATTEMPT, whose arm sets no reason at
//	                        all; and the CONSTANT on the out-of-domain arm,
//	                        which builds the stake directly rather than through
//	                        balanceReason. Those three are fully bound. The
//	                        REMAINING two admitting arms — BALANCE_INVALID and
//	                        BALANCE_NOT_SUPPLIED, the only ones that go through
//	                        balanceReason — are a SCOPED B: their text is the
//	                        caller's and is unbounded, but it is still required
//	                        NON-EMPTY, because balanceReason falls back to a
//	                        constant. The exemption once covered all four
//	                        admitting arms flatly, on a justification true of
//	                        those two.
//	A Value                 zero on every status but WOULD_ATTEMPT, which is the
//	                        only arm that sizes a stake; the others write a
//	                        literal that leaves it at its zero. On WOULD_ATTEMPT
//	                        the AMOUNT stays unread — recomputing it means
//	                        reimplementing pointsValue, the policy algorithm,
//	                        which this seam does not do, and it is bound
//	                        downstream through the decision digest. So this is a
//	                        scoped B inside an A field, the mirror of the reason
//	                        beside it. The old entry gave the recomputation
//	                        reason for all five statuses; it was true of one.
//
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
	// The fourth pinned header string, held exactly like its three siblings. It
	// was unread beside them, which was an omission rather than a decision: all
	// four are constants the producer stamps, so leaving one unchecked lets an
	// evaluation claim a donor this model was not derived from.
	s.require(ev.DonorRevision == predictioneval.OrderedRulesDonorRevision, "DONOR_REVISION_FOREIGN")
	// The consumed-word counter, bounded on its OWN range and independently of
	// any trace entry.
	//
	// It used to be read only as the second bound on the admitting word index,
	// inside `if last.RawWordIndex >= 0`. That left it entirely unread on every
	// admission whose final step consumed no word -- the whole DEFAULT half, and
	// a RULE_DRAW at a rate of exactly one -- and on every terminal status,
	// while the matrix classified it load-bearing. A field the matrix claims to
	// read and a reachable path does not is precisely the defect the matrix
	// exists to make impossible, so the bound is stated here, once, for every
	// status rather than inside one arm.
	//
	// The range is the producer's own: `cursor` starts at zero, is only ever
	// incremented, never passes len(words), and a supplied trace longer than
	// MaxOrderedRulesDrawWords is refused before the traversal.
	//
	// That bound is the SUPPLIED TRACE's length, and it is deliberately the
	// looser of the two available. Every consumed word sits inside the same
	// work-guarded rule loop, so the reachable maximum is really
	// MaxOrderedRulesWork -- a quarter of this ceiling. The largest value a
	// traversal can actually reach is 260,112, short of the budget itself
	// because the last candidate stops mid-rule; that figure is derived from the
	// loop's arithmetic, not measured by anything in this repository, and is
	// named here as a derivation. The tighter bound is not asserted here
	// because it rests on a per-iteration accounting argument rather than on a
	// single producer assignment, and the cost of being wrong about it is a
	// refusal of honest output. The looseness is fail-OPEN and is stated rather
	// than left to be discovered.
	//
	// The reason this is not scoped to admissions is worth stating exactly,
	// because the obvious version of it is wrong. The MID-traversal refusals do
	// write the counter from that same cursor. The PRE-traversal ones never
	// assign it at all -- `cursor` is not even declared yet -- so it carries
	// Go's zero. Both land inside the range, which is what this needs, but by
	// two different mechanisms rather than one. The terminal fixture beside
	// this guard refuses pre-traversal, on the arm the simpler claim is false
	// about.
	s.require(ev.RawWordsConsumed >= 0 && ev.RawWordsConsumed <= predictioneval.MaxOrderedRulesDrawWords,
		"EVALUATION_RAW_WORDS_CONSUMED_OUT_OF_RANGE")
	// The four cumulative counters, on their own ranges, for the same reason and
	// found by asking the same question of the rest of the matrix: every one of
	// them WAS read only inside requireCoherentSelection, so on a terminal status
	// -- which carries no selection -- and on the DEFAULT half -- where the
	// detailed-rule arm never runs -- all four went unread while the matrix
	// classified them load-bearing. Three of the four were still unread after
	// the RawWordsConsumed repair, which is the honest measure of how easily
	// this class hides: fixing one instance did not surface its siblings.
	//
	// Each ceiling is the producer's, read off the writer rather than inferred:
	//   - CandidatesConsumed is `ci + 1` over a candidate list
	//     orderedRulesInputCountReason refuses past MaxOrderedRulesCandidates,
	//     and that gate is the first statement of the evaluator;
	//   - OutcomesConsidered advances once per outcome of one candidate, and the
	//     same gate refuses any candidate carrying more than
	//     MaxOrderedRulesOutcomes, so the product bounds the total;
	//   - RulesConsidered is incremented immediately AFTER the work++ that
	//     refuses past MaxOrderedRulesWork, so it can never outrun that budget;
	//   - BernoulliEvaluations advances inside that same work-guarded rule loop.
	//
	// These are NECESSARY conditions and nothing more. A counter inside its
	// range is not thereby consistent with the traversal that claims it; the
	// relations that tie a counter to an index stay where they are, in the
	// selection arms, because only a selection names an index to tie it to.
	//
	// One consequence worth naming, in a file this does not touch. Illegality
	// names every failed predicate, frameAction frames that list, and
	// p3bResultWitness frames the action -- so adding an identifier moves the
	// witness digest of a stored result whose action was ALREADY illegal. No
	// legitimate run is affected, and the check that says so lives in this
	// repository rather than in a review that is not here: every relation added
	// on this path has a producer-driven control asserting that the shapes
	// EvaluateOrderedRules really emits still map legal. And a stored result is
	// re-evaluated rather than trusted, which is the property that makes this
	// benign rather than lucky.
	s.require(ev.CandidatesConsumed >= 0 && ev.CandidatesConsumed <= predictioneval.MaxOrderedRulesCandidates,
		"EVALUATION_CANDIDATES_CONSUMED_OUT_OF_RANGE")
	s.require(ev.OutcomesConsidered >= 0 &&
		ev.OutcomesConsidered <= predictioneval.MaxOrderedRulesCandidates*predictioneval.MaxOrderedRulesOutcomes,
		"EVALUATION_OUTCOMES_CONSIDERED_OUT_OF_RANGE")
	s.require(ev.RulesConsidered >= 0 && ev.RulesConsidered <= predictioneval.MaxOrderedRulesWork,
		"EVALUATION_RULES_CONSIDERED_OUT_OF_RANGE")
	s.require(ev.BernoulliEvaluations >= 0 && ev.BernoulliEvaluations <= predictioneval.MaxOrderedRulesWork,
		"EVALUATION_BERNOULLI_EVALUATIONS_OUT_OF_RANGE")

	// The traversal state, held on EVERY status rather than only where a
	// selection carries it.
	//
	// The stop fields, Trace and Visits were read only inside
	// requireCoherentSelection, which returns at once when Selected is nil. On
	// the three terminal statuses they were therefore unread, and a refusal that
	// declined to read its input could carry a ghost stop, an admitting trace
	// entry and an ADMITTED visit and still map legal. That is the same class
	// the counter bounds above closed, one level out: a field the matrix calls
	// load-bearing that a whole reachable path never reads.
	//
	// Three relations, each from one adjacent block of producer assignments at
	// the top of the candidate loop -- CandidatesConsumed = ci+1,
	// StoppedAtCandidate, StoppedAtPosition, HasStopPosition = true -- plus the
	// fact that every one of that loop's exit paths appends exactly one visit.
	// The package already stakes two identifiers inside requireCoherentSelection
	// on that same visit-per-iteration accounting, so this adds no new class of
	// assumption; it only stops scoping it to shapes that carry a selection.
	//
	// What is deliberately NOT asserted, because honest output breaks it:
	// nothing about StoppedAtPosition's SIGN (a scope may declare negative
	// positions, and a candidate at -99 is legal), and nothing tying Trace to
	// CandidatesConsumed (a candidate refused for too few outcomes appends a
	// visit and no trace entry).
	s.require(len(ev.Visits) == ev.CandidatesConsumed, "VISIT_COUNT_CONTRADICTS_CANDIDATES_CONSUMED")
	s.require(ev.HasStopPosition == (ev.CandidatesConsumed >= 1),
		"STOP_POSITION_CONTRADICTS_CANDIDATES_CONSUMED")
	if !ev.HasStopPosition {
		s.require(ev.StoppedAtCandidate == "" && ev.StoppedAtPosition == 0,
			"STOP_FIELDS_WITHOUT_STOP_POSITION")
	} else {
		// The OTHER half of the same entry, and the half that used to be
		// asserted in the matrix and enforced nowhere. "Blank together when
		// none was" was held; "present exactly when a candidate was consumed"
		// was held only where a SELECTION names the candidate -- inside
		// requireCoherentSelection, which returns at once when Selected is nil
		// -- so on all three terminal statuses it was unread, and an
		// independent lane mapped that shape legal on each of them.
		//
		// The ceiling is the producer's own, read off the writer:
		// orderedRulesStreamInvariantsBroken refuses a candidate whose Identity
		// is empty BEFORE the traversal begins, and the traversal writes
		// StoppedAtCandidate = c.Identity in the same adjacent block that sets
		// HasStopPosition = true. The POSITION is deliberately not bounded here
		// beside it: c.Position is a supplied interval coordinate with no
		// producer-fixed sign, and the relations that tie it to an index stay
		// in the selection arms, where an index exists to tie it to.
		s.require(ev.StoppedAtCandidate != "", "STOP_CANDIDATE_MISSING_WITH_STOP_POSITION")
	}
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
		// And the VALUE. Only the admitting default arm sizes a stake; every
		// other status writes a struct literal that leaves Value at its zero,
		// so a non-zero amount outside WOULD_ATTEMPT is a shape the producer
		// cannot write. This is the same O(1) literal-shape relation as the
		// reason beside it, and reading it does NOT recompute pointsValue: the
		// recomputation argument for leaving the amount unread holds on
		// WOULD_ATTEMPT, where it is computed, and only there.
		s.require(ev.Stake.Value == 0, "STAKE_VALUE_CONTRADICTS_STATUS")
		// The REASON is fixed by the same initialisation as the presence, and
		// was left unread beside it. Two of the four ADMITTING arms are bound
		// as well, each on its own arm below: WOULD_ATTEMPT sets no reason at
		// all, and the out-of-domain arm writes the constant directly. Only the
		// two arms that pass the caller's balance reason through balanceReason
		// are exempt, because that text is not a vocabulary this package can
		// bound. See the field matrix above.
		s.require(ev.Stake.Reason == predictioneval.ReasonBalanceNotEvaluated, "STAKE_REASON_CONTRADICTS_STATUS")
	}
	switch ev.Status {
	case predictioneval.StatusWouldAttempt:
		m.Class = ActionWouldAttempt
		// The admitting DEFAULT arm builds SuppliedUint32{KNOWN, Value} and
		// never sets Reason, so a sized stake carrying one is a shape the
		// producer cannot write. The caller-text exemption below covers the two
		// balanceReason arms; it does not reach this one, which sets no reason
		// at all.
		s.require(ev.Stake.Reason == "", "STAKE_REASON_CONTRADICTS_STATUS")
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
		// None of the three stake-unknown arms sizes a stake either.
		s.require(ev.Stake.Value == 0, "STAKE_VALUE_CONTRADICTS_STATUS")
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
		// Only two of the four admitting arms pass the caller's balance reason
		// through balanceReason. The out-of-domain arm builds the stake with
		// the CONSTANT, so on that arm the reason is as fixed as the presence.
		// An exemption written for the caller-text arms used to cover this one
		// as well, which is the kind of over-broad justification a matrix
		// exists to catch.
		if ev.Reason == predictioneval.ReasonBalanceOutOfDomain {
			s.require(ev.Stake.Reason == predictioneval.ReasonBalanceOutOfDomain, "STAKE_REASON_CONTRADICTS_REASON")
		} else {
			// The other two arms carry caller text, which is not a vocabulary
			// to bound — but balanceReason falls back to a non-empty constant
			// when the caller supplied none, so it is never EMPTY. That is the
			// one predicate those arms do admit.
			s.require(ev.Stake.Reason != "", "STAKE_REASON_EMPTY_ON_ADMISSION")
		}
	case predictioneval.StatusNoAttemptInSuppliedPrefix:
		m.Class = ActionNoAttemptInSuppliedPrefix
		s.require(!admitted, "PARTICIPATION_ADMITTED_ON_NO_ATTEMPT")
		s.require(ev.Selected == nil, "SELECTION_ON_NO_ATTEMPT")
		s.require(!stakeKnown, "STAKE_ON_NO_ATTEMPT")
		s.require(ev.Reason == "", "REASON_ON_NO_ATTEMPT")
	case predictioneval.StatusUnknownInput:
		m.Class = ActionUnknownInput
		// All three sites that write this status sit BELOW the stop block, and
		// each appends its visit before returning, so reaching any of them
		// requires being inside an iteration. Unlike NO_ATTEMPT, this status has
		// no zero-candidate escape -- an empty candidate list never reaches it.
		s.require(ev.HasStopPosition, "UNKNOWN_INPUT_WITHOUT_STOP_POSITION")
		s.require(!admitted, "PARTICIPATION_ADMITTED_ON_UNKNOWN_INPUT")
		s.require(ev.Selected == nil, "SELECTION_ON_UNKNOWN_INPUT")
		s.require(!stakeKnown, "STAKE_ON_UNKNOWN_INPUT")
		s.require(ev.Reason != "", "UNKNOWN_INPUT_WITHOUT_REASON")
		if ev.Reason != "" {
			s.require(unknownInputReasons[ev.Reason], "UNKNOWN_INPUT_REASON_FOREIGN")
		}
	case predictioneval.StatusRefused:
		m.Class = ActionRefused
		// The REASON decides the tier, because the reason is what distinguishes
		// the MID-TRAVERSAL writer from the pre-traversal ones. refuse() is
		// called at exactly three sites carrying exactly two reasons; every
		// other refusal is emitted before the candidate loop begins.
		//
		// WHAT THIS CLAIM IS AND IS NOT, corrected after an independent lane
		// executed the producer and showed the earlier wording false. The
		// earlier version said the other twelve come "from a constructor that
		// builds a fresh result". There are TWO pre-traversal helpers: the
		// package-level orderedRulesUnreadRefusal, which does build a fresh
		// result, and a local refuseUnread closure that deliberately KEEPS what
		// the call had already earned -- the stream digest always, and the
		// cutoff and qualifications for the one site below the derived-field
		// assignment. So the twelve do not all carry nothing.
		//
		// The predicate below is nevertheless right, and it is right for a
		// reason that survives the correction: what it tests is TRAVERSAL
		// state, and both helpers are unreachable once the candidate loop has
		// begun. All four closure sites sit above the loop's first write, so
		// neither helper can return a stop, a trace entry or a visit. A digest
		// or a cutoff on a refusal is ordinary producer output and is not
		// tested here.
		//
		// Guarded on the closed vocabulary so an out-of-vocabulary reason is
		// named once by REFUSAL_REASON_FOREIGN below and not twice here. That
		// also makes this fail-OPEN on a reason neither branch recognises.
		switch {
		case ev.Reason == predictioneval.ReasonWorkBudgetExceeded ||
			ev.Reason == predictioneval.ReasonAttemptRateOutOfDomain:
			s.require(ev.HasStopPosition, "MID_TRAVERSAL_REFUSAL_WITHOUT_STOP")
		case refusalReasons[ev.Reason]:
			s.require(!ev.HasStopPosition && ev.StoppedAtCandidate == "" && ev.StoppedAtPosition == 0 &&
				len(ev.Trace) == 0 && len(ev.Visits) == 0, "UNREAD_REFUSAL_CARRIES_TRAVERSAL_STATE")
		}
		s.require(!admitted, "PARTICIPATION_ADMITTED_ON_REFUSAL")
		s.require(ev.Selected == nil, "SELECTION_ON_REFUSAL")
		s.require(!stakeKnown, "STAKE_ON_REFUSAL")
		s.require(ev.Reason != "", "REFUSAL_WITHOUT_REASON")
		if ev.Reason != "" {
			s.require(refusalReasons[ev.Reason], "REFUSAL_REASON_FOREIGN")
		}
	default:
		m.Class = ActionUnsupportedShape
		s.found = append(s.found, "UNKNOWN_NATIVE_STATUS")
	}
	return s.finish(m)
}

// admittingBalanceUse is the balance use the producer writes beside a status,
// on the visit it appends when it admits.
//
// admitOrderedRules decides both in one switch over the candidate's balance:
// an INVALID balance is REQUIRED_BUT_INVALID beside STAKE_UNKNOWN/BALANCE_INVALID,
// an unsupplied one REQUIRED_BUT_MISSING beside BALANCE_NOT_SUPPLIED, one past
// the u32 domain REQUIRED_BUT_OUT_OF_DOMAIN beside BALANCE_OUT_OF_U32_DOMAIN,
// and a usable one USED beside WOULD_ATTEMPT. The four arms are total over the
// admitting paths, so a status naming none of them is a shape the arms above
// have already refused, and the second return says so rather than guessing.
func admittingBalanceUse(ev predictioneval.OrderedRulesEvaluation) (predictioneval.OrderedRulesBalanceUse, bool) {
	switch ev.Status {
	case predictioneval.StatusWouldAttempt:
		return predictioneval.BalanceUsed, true
	case predictioneval.StatusParticipationAdmittedStakeUnknown:
		switch ev.Reason {
		case predictioneval.ReasonBalanceInvalid:
			return predictioneval.BalanceRequiredButInvalid, true
		case predictioneval.ReasonBalanceNotSupplied:
			return predictioneval.BalanceRequiredButMissing, true
		case predictioneval.ReasonBalanceOutOfDomain:
			return predictioneval.BalanceRequiredButOutOfDomain, true
		}
	}
	return "", false
}

// The closed, status-specific reason vocabularies the producer emits.
//
// Both terminal arms used to test only that a reason was NONEMPTY, which any
// string satisfies. These are transcribed from the emitting sites rather than
// from the reason constants as a block, because the constants do not say which
// status carries which: the three balance reasons belong to the admitting arm
// and are held there, and every remaining reason belongs to exactly one of the
// two sets below.
//
// The census closes: 22 refusal reasons in the ordered-rules vocabulary — 14
// here under REFUSED, 5 under UNKNOWN_INPUT, 3 balance reasons on the
// stake-unknown arm. Bounding a vocabulary refuses honest output as easily as
// forged, so membership was established by ENUMERATING every emitting site —
// including the three gates that return a reason as a value — rather than by
// reading the constant block. What the suite drives end to end from real
// producer output, asserting the mapping stays legal, is a SUBSET of the
// nineteen: TestP3bTerminalReasonsTheProducerEmitsStayLegal names exactly which,
// and this paragraph should not be read as claiming more.
var (
	// UNKNOWN_INPUT is written at three sites: a model vector that is not
	// known, a pool sum the outcomes do not permit (three reasons, from
	// checkedPoolSum), and a draw trace spent mid-rule.
	unknownInputReasons = map[string]bool{
		predictioneval.ReasonOutcomeVectorNotKnown:    true,
		predictioneval.ReasonOutcomePointsNotKnown:    true,
		predictioneval.ReasonOutcomePointsOutOfDomain: true,
		predictioneval.ReasonPoolSumOverflow:          true,
		predictioneval.ReasonEntropyExhausted:         true,
	}
	// REFUSED is written through three helpers — the mid-traversal refusal,
	// the pre-traversal one, and the unread one that attests to nothing — and
	// this is the union over all of their call sites, including the reasons
	// they take as values from the two input gates and from config
	// normalization.
	refusalReasons = map[string]bool{
		predictioneval.ReasonWorkBudgetExceeded:       true,
		predictioneval.ReasonAttemptRateOutOfDomain:   true,
		predictioneval.ReasonDrawWordsOverBound:       true,
		predictioneval.ReasonRuleCountOverBound:       true,
		predictioneval.ReasonStreamShapeOverBound:     true,
		predictioneval.ReasonStreamTextOverBound:      true,
		predictioneval.ReasonStreamBytesOverBound:     true,
		predictioneval.ReasonStreamContractMismatch:   true,
		predictioneval.ReasonEntropySemanticsMismatch: true,
		predictioneval.ReasonStreamInvariantViolated:  true,
		predictioneval.ReasonStreamDigestMismatch:     true,
		predictioneval.ReasonSuppliedTextNotEncodable: true,
		predictioneval.ReasonConfigDefaultNotSupplied: true,
		predictioneval.ReasonConfigOutOfDomain:        true,
	}
)
