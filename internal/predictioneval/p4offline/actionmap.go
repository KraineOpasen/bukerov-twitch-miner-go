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

	// FACTUAL_STEALTH_OFF: the stealth stage may only be NOT_APPLICABLE
	// (executed or never reached). Any realization is a leak.
	stealthClean := (ev.Stealth.Outcome == predictioneval.StealthNotApplicable && (ev.Stealth.State == exec || ev.Stealth.State == nr)) ||
		(ev.Stealth.Outcome == "" && ev.Stealth.State == nr)
	s.require(stealthClean, "STEALTH_REALIZATION_PRESENT")

	healthWitnessedOpen := ev.Health.State == predictioneval.StageStateWitnessed && ev.Health.Verdict != predictioneval.HealthDenied
	gateOpen := ev.StakeGate.State == exec && ev.StakeGate.Reason != predictioneval.GateReserveViolation
	postGateNotReached := ev.Clamp.State == nr && ev.Minimum.State == nr

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
		s.require(ev.Choice.State == exec, "CHOICE_NOT_EXECUTED")
		s.require(ev.Filter.State == exec, "FILTER_NOT_EXECUTED")
		s.require(ev.Health.State == predictioneval.StageStateWitnessed && ev.Health.Verdict == predictioneval.HealthDenied, "HEALTH_NOT_DENIED")
		s.require(ev.StakeGate.State == nr && postGateNotReached, "STAGES_AFTER_HEALTH_REACHED")
		s.require(ev.LegacyFailure == "", "LEGACY_FAILURE_PRESENT")
	case predictioneval.ActionReserveViolation:
		m.Class, m.SkipReason = ActionPolicySkip, ev.Action
		s.require(ev.Choice.State == exec, "CHOICE_NOT_EXECUTED")
		s.require(ev.Filter.State == exec, "FILTER_NOT_EXECUTED")
		s.require(healthWitnessedOpen, "HEALTH_CONTRADICTS_GATE")
		s.require(ev.StakeGate.State == exec && ev.StakeGate.Reason == predictioneval.GateReserveViolation, "STAKE_GATE_NOT_RESERVE_VIOLATION")
		s.require(postGateNotReached, "STAGES_AFTER_RESERVE_VIOLATION_REACHED")
		s.require(ev.LegacyFailure == "", "LEGACY_FAILURE_PRESENT")
	case predictioneval.ActionBelowMinimum:
		m.Class, m.SkipReason = ActionPolicySkip, ev.Action
		s.require(ev.Choice.State == exec, "CHOICE_NOT_EXECUTED")
		s.require(ev.Filter.State == exec && !ev.Filter.Applied, "FILTER_CONTRADICTS_MINIMUM_EXIT")
		s.require(healthWitnessedOpen, "HEALTH_CONTRADICTS_GATE")
		s.require(gateOpen, "STAKE_GATE_CONTRADICTS_MINIMUM_EXIT")
		s.require(ev.Clamp.State == exec && ev.Clamp.HasFinal, "NO_FINAL_STAKE")
		s.require(ev.Minimum.State == exec && ev.Minimum.Below, "MINIMUM_NOT_BELOW")
		s.require(ev.PolicyAmountKnown, "POLICY_AMOUNT_UNKNOWN")
		s.require(ev.LegacyFailure == "", "LEGACY_FAILURE_PRESENT")
	case predictioneval.ActionFilterRejected:
		m.Class, m.SkipReason = ActionPolicySkip, ev.Action
		s.require(ev.Choice.State == exec, "CHOICE_NOT_EXECUTED")
		s.require(ev.Filter.State == exec && ev.Filter.Skip && ev.Filter.Applied, "FILTER_NOT_APPLIED")
		s.require(healthWitnessedOpen, "HEALTH_CONTRADICTS_GATE")
		s.require(gateOpen, "STAKE_GATE_CONTRADICTS_FILTER_EXIT")
		s.require(ev.Clamp.State == exec && ev.Clamp.HasFinal, "NO_FINAL_STAKE")
		s.require(ev.Minimum.State == exec && !ev.Minimum.Below, "MINIMUM_CONTRADICTS_FILTER_EXIT")
		s.require(ev.PolicyAmountKnown, "POLICY_AMOUNT_UNKNOWN")
		s.require(ev.LegacyFailure == "", "LEGACY_FAILURE_PRESENT")
	case predictioneval.ActionPreDecisionExit:
		m.Class = ActionCoverageOnly
		s.require(ev.Choice.State == nr && ev.BaseStake.State == nr && ev.Stealth.State == nr &&
			ev.Filter.State == nr && ev.Health.State == nr && ev.StakeGate.State == nr && postGateNotReached,
			"STAGES_REACHED_BEFORE_DECISION")
		s.require(ev.LegacyFailure == "", "LEGACY_FAILURE_PRESENT")
	case predictioneval.ActionLegacyFailure:
		m.Class = ActionLegacyFailure
		s.require(ev.LegacyFailure != "", "LEGACY_FAILURE_UNNAMED")
		s.require(ev.Choice.State == predictioneval.StageStateLegacyFailure ||
			ev.BaseStake.State == predictioneval.StageStateLegacyFailure ||
			ev.Filter.State == predictioneval.StageStateLegacyFailure, "NO_STAGE_FAILED")
	case predictioneval.ActionIndeterminate:
		m.Class = ActionUnknownInput
		s.require(ev.BaseStake.State == predictioneval.StageStateIndeterminate ||
			ev.StakeGate.State == predictioneval.StageStateIndeterminate, "NO_STAGE_INDETERMINATE")
		s.require(ev.LegacyFailure == "", "LEGACY_FAILURE_PRESENT")
	case predictioneval.ActionUnsupported:
		m.Class = ActionUnknownInput
		s.require(ev.Choice.State == predictioneval.StageStateUnsupported ||
			ev.StakeGate.State == predictioneval.StageStateUnsupported, "NO_STAGE_UNSUPPORTED")
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
