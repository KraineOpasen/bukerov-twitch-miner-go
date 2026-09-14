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
	admitted := ev.Participation == predictioneval.ParticipationAdmitted
	stakeKnown := ev.Stake.Presence == predictioneval.SuppliedKnown
	switch ev.Status {
	case predictioneval.StatusWouldAttempt:
		m.Class = ActionWouldAttempt
		s.require(admitted, "PARTICIPATION_NOT_ADMITTED")
		s.require(ev.Selected != nil, "NO_SELECTION")
		s.require(stakeKnown, "STAKE_NOT_KNOWN")
		s.require(ev.Reason == "", "REASON_ON_ATTEMPT")
		s.require(ev.HasStopPosition && ev.CandidatesConsumed >= 1, "NO_CANDIDATE_REACHED")
	case predictioneval.StatusParticipationAdmittedStakeUnknown:
		m.Class = ActionParticipationAdmittedStakeUnknown
		s.require(admitted, "PARTICIPATION_NOT_ADMITTED")
		s.require(ev.Selected != nil, "NO_SELECTION")
		s.require(!stakeKnown, "STAKE_KNOWN_ON_UNKNOWN_STATUS")
		s.require(ev.Reason == predictioneval.ReasonBalanceNotSupplied || ev.Reason == predictioneval.ReasonBalanceInvalid ||
			ev.Reason == predictioneval.ReasonBalanceOutOfDomain, "STAKE_UNKNOWN_REASON_FOREIGN")
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
