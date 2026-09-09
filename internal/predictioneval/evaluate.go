package predictioneval

// Seam 3 of 4: the replay itself.
//
// Evaluate re-derives one decision from [DecisionInputs] alone. It cannot read
// a recorded result, a settlement, a later fact or the current configuration,
// because it is never handed any of them — the signature is the enforcement,
// not a convention.
//
// It walks the pinned decision path in the order the pinned caller walks it,
// which is not the order the stages are written in and is easy to get wrong:
//
//	Calculate → Skip → health gate → stake gate → clamp → MINIMUM → filter
//
// The filter is COMPUTED immediately after Calculate — that is where the
// pinned caller calls Skip — but it is ACTED ON last, after the minimum-stake
// exit. A replay that acted on it where it computed it would report
// FILTER_REJECTED for attempts the miner actually ended with
// BELOW_MINIMUM_POINTS.

// Stage execution states.
const (
	// StageStateExecuted — this model ran the stage and the value is its
	// result.
	StageStateExecuted = "EXECUTED"
	// StageStateNotReached — the replay ended before this stage. Its value
	// does not exist and no zero stands in for it.
	StageStateNotReached = "NOT_REACHED"
	// StageStateUnsupported — the stage's inputs were not recorded, or were
	// recorded in a form this model cannot use. Never a zero, never a false,
	// never a default strategy, never a skip.
	StageStateUnsupported = "UNSUPPORTED"
	// StageStateIndeterminate — the stage ran into something whose value
	// cannot be established: an integer width this machine cannot represent,
	// arithmetic the pinned policy would wrap, or a random draw whose
	// realization the record does not pin down.
	StageStateIndeterminate = "INDETERMINATE"
	// StageStateWitnessed — the value is an EXTERNAL verdict echoed from the
	// record. This model did not compute it and proves nothing about the
	// hidden state that produced it.
	StageStateWitnessed = "WITNESSED"
	// StageStateLegacyFailure — the pinned policy itself fails on this input.
	StageStateLegacyFailure = "LEGACY_FAILURE"
)

// Stealth stage outcomes.
const (
	// StealthNotApplicable — stealth mode was off, or the base stake was below
	// the chosen outcome's top single stake. Established INDEPENDENTLY, with
	// no observed value consulted.
	StealthNotApplicable = "NOT_APPLICABLE"
	// StealthConditionedOnObservedRealization — stealth applied, so the stake
	// depended on a random draw this model cannot reproduce. The choice, the
	// base stake and the fact that stealth applies were all established
	// independently FIRST; only then was the recorded pre-risk stake used to
	// pin down which of the four possible integer reductions occurred.
	//
	// Everything downstream of this is AS-OPERATED RECONSTRUCTION, not
	// independent proof. It is never counted as a passed independent oracle.
	StealthConditionedOnObservedRealization = "CONDITIONED_ON_OBSERVED_REALIZATION"
	// StealthUnproven — stealth applied but the record does not pin the
	// realization down: no pre-risk stake, or one that is not reachable by any
	// legal reduction. The stake stays unknown rather than being guessed.
	StealthUnproven = "UNPROVEN"
)

// Terminal actions of a replayed decision.
const (
	// ActionPreDecisionExit — the attempt ended before the policy ran, for a
	// WITNESSED external reason (eligibility, round state). This model did not
	// re-derive that reason.
	ActionPreDecisionExit  = "PRE_DECISION_EXIT"
	ActionHealthGated      = "HEALTH_GATED"
	ActionReserveViolation = "RESERVE_VIOLATION"
	ActionBelowMinimum     = "BELOW_MINIMUM_POINTS"
	ActionFilterRejected   = "FILTER_REJECTED"
	// ActionWouldAttemptPlacement — the policy would have reached the Twitch
	// placement call with this stake and outcome.
	//
	// It is NOT a claim that a bet was placed, or that one would have
	// succeeded. The call can still be rejected by Twitch, and whether the
	// original one was is a separate recorded fact that only [Score] sees.
	ActionWouldAttemptPlacement = "WOULD_ATTEMPT_PLACEMENT"
	// ActionLegacyFailure — the pinned policy panics on this input. Reported;
	// not reproduced, not fixed.
	ActionLegacyFailure = "LEGACY_FAILURE"
	ActionIndeterminate = "INDETERMINATE"
	ActionUnsupported   = "UNSUPPORTED"
)

// Limitations a result carries about itself.
const (
	LimitationStealthConditioned    = "STEALTH_STAGE_CONDITIONED_ON_OBSERVED_REALIZATION"
	LimitationHealthWitnessed       = "HEALTH_VERDICT_IS_WITNESSED_NOT_RECONSTRUCTED"
	LimitationPreDecisionWitnessed  = "PRE_DECISION_EXIT_IS_WITNESSED_NOT_RECONSTRUCTED"
	LimitationArchDependentOverflow = "PINNED_POLICY_ARITHMETIC_WOULD_WRAP_ON_THIS_WIDTH"
	LimitationUnrepresentableStake  = "STAKE_NOT_REPRESENTABLE_AT_THIS_INT_WIDTH"
)

// ChoiceStage is the strategy's chosen index.
type ChoiceStage struct {
	State string `json:"state"`
	// Index is the policy's chosen index; -1 means it chose nothing.
	Index int `json:"index"`
	// Selected is whether the index actually addresses an outcome, which is
	// the condition the policy gates its stake block on.
	Selected bool `json:"selected"`
	// OutcomeID is the chosen outcome's identifier, verbatim, or empty.
	OutcomeID string `json:"outcomeId,omitempty"`
}

// BaseStakeStage is the pre-stealth stake.
type BaseStakeStage struct {
	State string `json:"state"`
	// Amount is the percentage-of-balance stake after the MaxPoints cap.
	Amount int `json:"amount"`
	// Capped reports that MaxPoints actually BOUND the amount — that the
	// uncapped percentage exceeded it. A stake that merely equals MaxPoints was
	// not constrained by it.
	Capped bool `json:"capped"`
}

// StealthStage is the random-draw stage, and the only conditioned one.
type StealthStage struct {
	State string `json:"state"`
	// Outcome is one of the Stealth* constants.
	Outcome string `json:"outcome"`
	// Applies is established independently of any observed value.
	Applies bool `json:"applies"`
	// Realized is the post-stealth stake. Under a conditioned outcome it is
	// the RECORDED value, not an independently derived one.
	Realized int `json:"realized"`
	// Reduction is the integer the policy subtracted, derived from the
	// recorded realization. The pinned policy can only ever subtract 1..4.
	Reduction        int  `json:"reduction,omitempty"`
	ReductionDerived bool `json:"reductionDerived"`
	RealizationSound bool `json:"realizationSound"`
}

// FilterStage is the filter condition's single evaluation.
type FilterStage struct {
	State string `json:"state"`
	// Skip is the policy's answer: true means the bet is abandoned.
	Skip bool `json:"skip"`
	// Compared is the value the condition was tested against.
	Compared float64 `json:"compared"`
	// Applied records that the filter's answer actually ended the attempt,
	// which happens only after the minimum-stake exit.
	Applied bool `json:"applied"`
}

// HealthStageResult echoes the witnessed account-wide gate.
type HealthStageResult struct {
	State string `json:"state"`
	// Verdict is the recorded gate state, verbatim.
	Verdict string `json:"verdict"`
	Reason  string `json:"reason,omitempty"`
}

// StakeGateStage is the global size gate's complete return.
type StakeGateStage struct {
	State string `json:"state"`
	// Proposed is what this model handed the gate.
	Proposed int `json:"proposed"`
	// Allowed is what the gate RETURNED — not necessarily what the caller
	// adopted, which is the clamp stage's business.
	Allowed int    `json:"allowed"`
	Reason  string `json:"reason"`
	Limit   int    `json:"limit"`
}

// ClampStage is the caller's decision about the gate's allowance.
type ClampStage struct {
	State string `json:"state"`
	// Applied is whether the caller replaced its stake with the allowance.
	Applied bool `json:"applied"`
	// FinalAmount is the stake carried OUT of the gate block. It does not
	// exist on a reserve violation, which returns from inside it.
	FinalAmount int  `json:"finalAmount"`
	HasFinal    bool `json:"hasFinal"`
}

// MinimumStage is the pinned floor applied after the gates.
type MinimumStage struct {
	State string `json:"state"`
	// Threshold is the floor that was applied.
	Threshold int `json:"threshold"`
	// Below is whether the final stake fell under it.
	Below bool `json:"below"`
}

// Evaluation is the complete replayed decision.
type Evaluation struct {
	Model             ModelProvenance `json:"model"`
	CommonInputDigest string          `json:"commonInputDigest"`

	Choice    ChoiceStage       `json:"choice"`
	BaseStake BaseStakeStage    `json:"baseStake"`
	Stealth   StealthStage      `json:"stealth"`
	Filter    FilterStage       `json:"filter"`
	Health    HealthStageResult `json:"health"`
	StakeGate StakeGateStage    `json:"stakeGate"`
	Clamp     ClampStage        `json:"clamp"`
	Minimum   MinimumStage      `json:"minimum"`

	// PolicyAmount is Calculate's complete return: the post-stealth stake, or
	// the initialized zero when the stake block never ran.
	PolicyAmount      int  `json:"policyAmount"`
	PolicyAmountKnown bool `json:"policyAmountKnown"`

	Action string `json:"action"`
	// ActionReason carries the witnessed reason for a PRE_DECISION_EXIT and
	// the failure name for a LEGACY_FAILURE.
	ActionReason  string        `json:"actionReason,omitempty"`
	LegacyFailure LegacyFailure `json:"legacyFailure,omitempty"`

	// Limitations are this result's own caveats, carried so a consumer cannot
	// read more into it than it proves.
	Limitations []string `json:"limitations,omitempty"`
}

// Evaluate is seam 3: it replays one decision from its inputs.
//
// obs is the ONLY observed value it may read, and it is consulted at exactly
// one place: constraining the stealth reduction after the choice, the base
// stake and the applicability of stealth have all been established without it.
// TestObservedRealizationCannotInfluenceANonStealthCase falsifies any leak.
func Evaluate(in DecisionInputs, obs ObservedRealization) Evaluation {
	ev := Evaluation{
		Model:             CurrentModelProvenance(),
		CommonInputDigest: in.CommonInputDigest,
		Choice:            ChoiceStage{State: StageStateNotReached, Index: -1},
		BaseStake:         BaseStakeStage{State: StageStateNotReached},
		Stealth:           StealthStage{State: StageStateNotReached},
		Filter:            FilterStage{State: StageStateNotReached},
		Health:            HealthStageResult{State: StageStateNotReached},
		StakeGate:         StakeGateStage{State: StageStateNotReached},
		Clamp:             ClampStage{State: StageStateNotReached},
		Minimum:           MinimumStage{State: StageStateNotReached, Threshold: in.MinimumStake},
	}

	if !in.ReachedDecision {
		// The attempt ended before the policy. The exit is a witnessed
		// external fact — eligibility and round state are not reconstructible
		// from anything in the envelope — so it is echoed and labelled.
		ev.Action = ActionPreDecisionExit
		ev.ActionReason = in.PreDecisionExit
		ev.Limitations = append(ev.Limitations, LimitationPreDecisionWitnessed)
		return ev
	}

	if in.Settings == nil || !in.BalancePresent || !in.OutcomesPresent {
		ev.Action = ActionUnsupported
		ev.Choice.State = StageStateUnsupported
		return ev
	}
	settings := *in.Settings
	outs := toPolicyOutcomes(in.Outcomes)

	// ---- Calculate, part 1: the choice. -------------------------------
	choice, fail := policyChoice(outs, settings.Strategy, settings.PercentageGap)
	if fail != LegacyFailureNone {
		ev.Choice.State = StageStateLegacyFailure
		return finishLegacyFailure(ev, fail)
	}
	ev.Choice = ChoiceStage{State: StageStateExecuted, Index: choice}

	// ---- Calculate, part 2: the stake block. --------------------------
	inRange := choice >= 0 && choice < len(outs)
	ev.Choice.Selected = inRange
	if !inRange {
		// The policy initializes the decision to {-1, 0, ""} and never enters
		// the stake block. The amount is that initialized zero — a real
		// result, not an absence.
		ev.BaseStake.State = StageStateNotReached
		ev.Stealth = StealthStage{State: StageStateNotReached, Outcome: StealthNotApplicable}
		ev.PolicyAmount, ev.PolicyAmountKnown = 0, true
	} else {
		chosen := outs[choice]
		if !chosen.present {
			// The policy reads the chosen outcome's ID directly here, with no
			// guard: an absent outcome is dereferenced.
			ev.BaseStake.State = StageStateLegacyFailure
			return finishLegacyFailure(ev, LegacyFailureAbsentOutcome)
		}
		ev.Choice.OutcomeID = chosen.id

		base, representable := baseStake(in.Balance, settings.Percentage, settings.MaxPoints)
		if !representable {
			ev.BaseStake.State = StageStateIndeterminate
			ev.Limitations = appendOnce(ev.Limitations, LimitationUnrepresentableStake)
			ev.Action = ActionIndeterminate
			return ev
		}
		ev.BaseStake = BaseStakeStage{
			State:  StageStateExecuted,
			Amount: base,
			// "Bound by MaxPoints", not "equal to MaxPoints". The pinned policy
			// caps on a strict >, so a percentage that lands exactly on the cap
			// was never constrained by it — and a consumer told otherwise would
			// go and adjust the wrong setting.
			Capped: cappedByMaxPoints(in.Balance, settings.Percentage, settings.MaxPoints),
		}

		// ---- Stealth: independent first, conditioned only at the end. --
		ev.Stealth = evaluateStealth(settings.StealthMode, base, chosen.topPoints, obs)
		switch ev.Stealth.Outcome {
		case StealthNotApplicable:
			ev.PolicyAmount, ev.PolicyAmountKnown = base, true
		case StealthConditionedOnObservedRealization:
			ev.PolicyAmount, ev.PolicyAmountKnown = ev.Stealth.Realized, true
			ev.Limitations = appendOnce(ev.Limitations, LimitationStealthConditioned)
		default: // StealthUnproven
			ev.PolicyAmountKnown = false
			ev.Limitations = appendOnce(ev.Limitations, LimitationStealthConditioned)
		}
	}

	// ---- Skip: computed HERE, acted on last. --------------------------
	skip, compared, fail := policySkip(outs, choice, settings.FilterCondition)
	if fail != LegacyFailureNone {
		ev.Filter.State = StageStateLegacyFailure
		return finishLegacyFailure(ev, fail)
	}
	ev.Filter = FilterStage{State: StageStateExecuted, Skip: skip, Compared: compared}

	// ---- Health gate: witnessed, never reconstructed. -----------------
	ev.Health = HealthStageResult{
		State:   StageStateWitnessed,
		Verdict: in.HealthState,
		Reason:  in.HealthReason,
	}
	ev.Limitations = appendOnce(ev.Limitations, LimitationHealthWitnessed)
	if in.HealthState == HealthDenied {
		ev.Action = ActionHealthGated
		return ev
	}

	// ---- Stake gate. ---------------------------------------------------
	if !in.RiskPresent {
		ev.StakeGate.State = StageStateUnsupported
		ev.Action = ActionUnsupported
		return ev
	}
	if !ev.PolicyAmountKnown {
		// The proposal itself is unproven, so the gate's answer would be a
		// guess about a guess.
		ev.StakeGate.State = StageStateIndeterminate
		ev.Action = ActionIndeterminate
		return ev
	}
	allowed, reason, limit, overflow := policyEvaluateStake(
		ev.PolicyAmount, in.Balance, in.RiskMaxStakePercent, in.RiskReservePoints)
	if overflow {
		ev.StakeGate.State = StageStateIndeterminate
		ev.StakeGate.Proposed = ev.PolicyAmount
		ev.Limitations = appendOnce(ev.Limitations, LimitationArchDependentOverflow)
		ev.Action = ActionIndeterminate
		return ev
	}
	ev.StakeGate = StakeGateStage{
		State:    StageStateExecuted,
		Proposed: ev.PolicyAmount,
		Allowed:  allowed,
		Reason:   reason,
		Limit:    limit,
	}

	// ---- Clamp. --------------------------------------------------------
	if reason == GateReserveViolation {
		// The pinned caller returns from INSIDE the gate block, so no
		// post-gate stake exists. Inventing one would claim a stage that did
		// not happen.
		ev.Clamp = ClampStage{State: StageStateNotReached}
		ev.Action = ActionReserveViolation
		return ev
	}
	final := ev.PolicyAmount
	clamped := false
	if reason == GatePercent {
		final, clamped = allowed, true
	}
	ev.Clamp = ClampStage{
		State:       StageStateExecuted,
		Applied:     clamped,
		FinalAmount: final,
		HasFinal:    true,
	}

	// ---- Minimum stake, then the filter. -------------------------------
	ev.Minimum = MinimumStage{
		State:     StageStateExecuted,
		Threshold: in.MinimumStake,
		Below:     final < in.MinimumStake,
	}
	if ev.Minimum.Below {
		ev.Action = ActionBelowMinimum
		return ev
	}
	if skip {
		ev.Filter.Applied = true
		ev.Action = ActionFilterRejected
		return ev
	}
	ev.Action = ActionWouldAttemptPlacement
	return ev
}

// evaluateStealth establishes applicability WITHOUT the observed value, and
// only then uses it to pin the realized integer down.
func evaluateStealth(stealthMode bool, base, topPoints int, obs ObservedRealization) StealthStage {
	if !stealthApplies(stealthMode, base, topPoints) {
		// Independently established: no observed value was consulted, so the
		// realized stake here is genuinely re-derived.
		return StealthStage{
			State:            StageStateExecuted,
			Outcome:          StealthNotApplicable,
			Applies:          false,
			Realized:         base,
			RealizationSound: true,
		}
	}

	out := StealthStage{State: StageStateIndeterminate, Outcome: StealthUnproven, Applies: true}
	if obs.StealthAmount == nil {
		return out
	}
	realized, ok := intFromStored(*obs.StealthAmount)
	if !ok {
		return out
	}
	// The pinned policy computes topPoints - int(rand.Float64()*4+1), so the
	// subtracted integer is always 1, 2, 3 or 4. A recorded realization
	// outside that set was not produced by this stage on these inputs, and is
	// reported unproven rather than accepted.
	reduction, borrow := subInt(topPoints, realized)
	if borrow || reduction < stealthMinReduction || reduction > stealthMaxReduction {
		return out
	}
	return StealthStage{
		State:            StageStateExecuted,
		Outcome:          StealthConditionedOnObservedRealization,
		Applies:          true,
		Realized:         realized,
		Reduction:        reduction,
		ReductionDerived: true,
		RealizationSound: true,
	}
}

// finishLegacyFailure records a panic of the pinned policy as a typed result.
//
// The FAILING STAGE is marked by the caller, immediately before calling this.
// An earlier version took the stage as a *string alongside an Evaluation VALUE,
// which silently lost the marker: the pointer wrote into the caller's variable
// while the function returned its own copy, made before the write.
// TestNegativeChoiceIndexPanicsThePinnedPolicy is what caught that.
func finishLegacyFailure(ev Evaluation, fail LegacyFailure) Evaluation {
	ev.Action = ActionLegacyFailure
	ev.ActionReason = string(fail)
	ev.LegacyFailure = fail
	return ev
}

func toPolicyOutcomes(in []OutcomeInput) []policyOutcome {
	out := make([]policyOutcome, len(in))
	for i, o := range in {
		out[i] = policyOutcome{
			present:         o.Present,
			id:              o.ID,
			totalUsers:      o.TotalUsers,
			totalPoints:     o.TotalPoints,
			topPoints:       o.TopPoints,
			percentageUsers: o.PercentageUsers,
			odds:            o.Odds,
			oddsPercentage:  o.OddsPercentage,
		}
	}
	return out
}
