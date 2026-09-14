package p4offline_test

// SEAM 7: the exhaustive native action map.
//
// The expected CLASS for every native action comes from the protocol's own
// table, never from the code: WOULD_ATTEMPT_PLACEMENT is WOULD_ATTEMPT; the
// four proved skips are POLICY_SKIP; PRE_DECISION_EXIT is coverage-only;
// LEGACY_FAILURE stays LEGACY_FAILURE; INDETERMINATE and UNSUPPORTED are
// UNKNOWN_INPUT. Every legal shape below is a REAL evaluation produced by the
// native evaluator over hand-built inputs, so the map's legality rules cannot
// be stricter than what the evaluator produces over inputs a dataset can
// bind — the one deliberate exception, an attempt with no selected choice
// under a minimum stake no dataset binds, is pinned as a refusal; every
// illegal shape is a legal one with exactly one stage tampered.

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval/p4offline"
)

// p2Inputs is the recurring MOST_VOTED / 5% / balance 1000 / no-gates case
// that places 50 on o1.
func p2Inputs() predictioneval.DecisionInputs {
	outs := make([]predictioneval.OutcomeInput, 0, 2)
	for _, o := range synthOutcomes() {
		outs = append(outs, predictioneval.OutcomeInput{
			Slot: o.Slot, Present: o.Present, ID: o.ID,
			TotalUsers: int(o.TotalUsers), TotalPoints: int(o.TotalPoints), TopPoints: int(o.TopPoints),
			PercentageUsers: o.PercentageUsers, Odds: o.Odds, OddsPercentage: o.OddsPercentage,
		})
	}
	return predictioneval.DecisionInputs{
		CommonInputDigest: "test",
		ReachedDecision:   true,
		Settings: &predictioneval.BetSettingsInput{
			Strategy: predictioneval.StrategyMostVoted, Percentage: 5, PercentageGap: 20, MaxPoints: 50_000,
		},
		Balance: 1000, BalancePresent: true,
		Outcomes: outs, OutcomesPresent: true,
		RiskPresent:  true,
		HealthState:  predictioneval.HealthAllowed,
		MinimumStake: predictioneval.PinnedMinimumStake,
	}
}

func p2Eval(in predictioneval.DecisionInputs) predictioneval.Evaluation {
	return predictioneval.Evaluate(in, predictioneval.ObservedRealization{})
}

// TestP2NativeActionMapIsExhaustiveOverLegalShapes drives every native P2
// action the evaluator can produce through the map.
func TestP2NativeActionMapIsExhaustiveOverLegalShapes(t *testing.T) {
	cases := []struct {
		name   string
		inputs func() predictioneval.DecisionInputs
		native string
		class  p4offline.ActionClass
	}{
		{"placement", p2Inputs, predictioneval.ActionWouldAttemptPlacement, p4offline.ActionWouldAttempt},
		{"health gated", func() predictioneval.DecisionInputs {
			in := p2Inputs()
			in.HealthState = predictioneval.HealthDenied
			return in
		}, predictioneval.ActionHealthGated, p4offline.ActionPolicySkip},
		{"reserve violation", func() predictioneval.DecisionInputs {
			in := p2Inputs()
			in.RiskReservePoints = 960 // 1000 - 50 = 950 < 960
			return in
		}, predictioneval.ActionReserveViolation, p4offline.ActionPolicySkip},
		{"below minimum", func() predictioneval.DecisionInputs {
			in := p2Inputs()
			in.Balance = 100 // 5% = 5 < 10
			return in
		}, predictioneval.ActionBelowMinimum, p4offline.ActionPolicySkip},
		{"filter rejected", func() predictioneval.DecisionInputs {
			in := p2Inputs()
			in.Settings.FilterCondition = &predictioneval.SourceFilterCondition{By: "odds", Where: "GT", Value: 5}
			return in
		}, predictioneval.ActionFilterRejected, p4offline.ActionPolicySkip},
		{"pre-decision exit", func() predictioneval.DecisionInputs {
			in := p2Inputs()
			in.ReachedDecision = false
			in.PreDecisionExit = "NOT_ELIGIBLE"
			return in
		}, predictioneval.ActionPreDecisionExit, p4offline.ActionCoverageOnly},
		{"legacy failure on a hole", func() predictioneval.DecisionInputs {
			in := p2Inputs()
			in.Outcomes[0].Present = false
			return in
		}, predictioneval.ActionLegacyFailure, p4offline.ActionLegacyFailure},
		{"legacy failure at the base stake", func() predictioneval.DecisionInputs {
			in := p2Inputs()
			in.Settings.Strategy = predictioneval.StrategyNumber2 // picks slot 1 without reading it
			in.Outcomes[1].Present = false
			return in
		}, predictioneval.ActionLegacyFailure, p4offline.ActionLegacyFailure},
		{"legacy failure at the filter", func() predictioneval.DecisionInputs {
			in := p2Inputs()
			in.Settings.Strategy = predictioneval.StrategyNumber1 // picks slot 0 without reading it
			in.Settings.FilterCondition = &predictioneval.SourceFilterCondition{By: predictioneval.OutcomeTotalUsers, Where: "GT", Value: 5}
			in.Outcomes[1].Present = false // total_users sums slots 0 and 1
			return in
		}, predictioneval.ActionLegacyFailure, p4offline.ActionLegacyFailure},
		{"unsupported: no settings", func() predictioneval.DecisionInputs {
			in := p2Inputs()
			in.Settings = nil
			return in
		}, predictioneval.ActionUnsupported, p4offline.ActionUnknownInput},
		{"unsupported: no risk inputs", func() predictioneval.DecisionInputs {
			in := p2Inputs()
			in.RiskPresent = false
			return in
		}, predictioneval.ActionUnsupported, p4offline.ActionUnknownInput},
		{"indeterminate: wrapped percent gate", func() predictioneval.DecisionInputs {
			in := p2Inputs()
			in.Balance = math.MaxInt
			in.RiskMaxStakePercent = 3
			return in
		}, predictioneval.ActionIndeterminate, p4offline.ActionUnknownInput},
		{"indeterminate: unrepresentable base stake", func() predictioneval.DecisionInputs {
			in := p2Inputs()
			in.Balance = math.MaxInt
			in.Settings.Percentage = 200
			return in
		}, predictioneval.ActionIndeterminate, p4offline.ActionUnknownInput},
		{"below minimum: no choice selected", func() predictioneval.DecisionInputs {
			in := p2Inputs()
			in.Settings.Strategy = "NOT_A_PINNED_STRATEGY" // the choice stays at -1; the zero amount is below the pinned minimum
			return in
		}, predictioneval.ActionBelowMinimum, p4offline.ActionPolicySkip},
	}
	seen := map[string]bool{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := p2Eval(tc.inputs())
			if ev.Action != tc.native {
				t.Fatalf("fixture produced native action %q, want %q (%s)", ev.Action, tc.native, ev.ActionReason)
			}
			seen[ev.Action] = true
			m := p4offline.MapP2Action(ev)
			if m.MapVersion != p4offline.NativeActionMapVersion || m.Policy != p4offline.PolicyP2 || m.NativeAction != tc.native {
				t.Fatalf("%+v", m)
			}
			if !m.Legal || m.Class != tc.class {
				t.Fatalf("native %q must map LEGALLY to %s: %+v", tc.native, tc.class, m)
			}
			if tc.class == p4offline.ActionPolicySkip && m.SkipReason != tc.native {
				t.Fatalf("a POLICY_SKIP names its proved native reason: %+v", m)
			}
		})
	}
	for _, native := range []string{
		predictioneval.ActionWouldAttemptPlacement, predictioneval.ActionHealthGated,
		predictioneval.ActionReserveViolation, predictioneval.ActionBelowMinimum,
		predictioneval.ActionFilterRejected, predictioneval.ActionPreDecisionExit,
		predictioneval.ActionLegacyFailure, predictioneval.ActionIndeterminate, predictioneval.ActionUnsupported,
	} {
		if !seen[native] {
			t.Errorf("native action %q was not exercised; the map is not proven exhaustive", native)
		}
	}
}

// TestP2NativeActionMapFailsClosedOnIllegalShapes pins that a status name is
// not enough: a stage that contradicts the claimed action is UNSUPPORTED_SHAPE,
// never a skip and never an attempt.
func TestP2NativeActionMapFailsClosedOnIllegalShapes(t *testing.T) {
	placed := p2Eval(p2Inputs())
	gated := func() predictioneval.Evaluation {
		in := p2Inputs()
		in.HealthState = predictioneval.HealthDenied
		return p2Eval(in)
	}()
	exit := func() predictioneval.Evaluation {
		in := p2Inputs()
		in.ReachedDecision = false
		return p2Eval(in)
	}()
	cases := []struct {
		name   string
		base   predictioneval.Evaluation
		tamper func(*predictioneval.Evaluation)
	}{
		{"placement without a final stake", placed, func(e *predictioneval.Evaluation) { e.Clamp.HasFinal = false }},
		{"placement with the stake gate unreached", placed, func(e *predictioneval.Evaluation) { e.StakeGate.State = predictioneval.StageStateNotReached }},
		{"placement under a denied health verdict", placed, func(e *predictioneval.Evaluation) { e.Health.Verdict = predictioneval.HealthDenied }},
		{"placement with a skipping filter", placed, func(e *predictioneval.Evaluation) { e.Filter.Skip = true }},
		{"placement below the minimum", placed, func(e *predictioneval.Evaluation) { e.Minimum.Below = true }},
		{"placement with an unselected choice", placed, func(e *predictioneval.Evaluation) { e.Choice.Selected = false }},
		{"placement with a stealth realization", placed, func(e *predictioneval.Evaluation) {
			e.Stealth.Outcome = predictioneval.StealthConditionedOnObservedRealization
		}},
		{"placement with an unproven stealth draw", placed, func(e *predictioneval.Evaluation) {
			e.Stealth.State = predictioneval.StageStateIndeterminate
			e.Stealth.Outcome = predictioneval.StealthUnproven
		}},
		{"placement carrying a legacy failure", placed, func(e *predictioneval.Evaluation) { e.LegacyFailure = "PINNED_POLICY_PANIC_ABSENT_OUTCOME" }},
		{"placement with an unknown policy amount", placed, func(e *predictioneval.Evaluation) { e.PolicyAmountKnown = false }},
		{"a new native action", placed, func(e *predictioneval.Evaluation) { e.Action = "SOMETHING_NEW" }},
		{"an empty native action", placed, func(e *predictioneval.Evaluation) { e.Action = "" }},
		{"health gate claimed with the stake gate executed", gated, func(e *predictioneval.Evaluation) {
			e.StakeGate.State = predictioneval.StageStateExecuted
		}},
		{"health gate claimed with an allowing verdict", gated, func(e *predictioneval.Evaluation) {
			e.Health.Verdict = predictioneval.HealthAllowed
		}},
		{"pre-decision exit with an executed choice", exit, func(e *predictioneval.Evaluation) {
			e.Choice.State = predictioneval.StageStateExecuted
		}},
		{"a skip name on a placing shape", placed, func(e *predictioneval.Evaluation) { e.Action = predictioneval.ActionBelowMinimum }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := tc.base
			ev.Limitations = append([]string(nil), ev.Limitations...)
			tc.tamper(&ev)
			m := p4offline.MapP2Action(ev)
			if m.Legal || m.Class != p4offline.ActionUnsupportedShape || len(m.Illegality) == 0 {
				t.Fatalf("an illegal shape must fail closed as UNSUPPORTED_SHAPE with the contradiction named: %+v", m)
			}
			if m.Class == p4offline.ActionPolicySkip || m.Class == p4offline.ActionWouldAttempt {
				t.Fatalf("never a skip, never an attempt: %+v", m)
			}
		})
	}
}

// TestP2ActionMapRequiresAnExplicitlyOpenHealthVerdict pins the map's
// fail-closed reading of the witnessed health gate: a path that proceeds past
// the gate is legal only under a verdict the vocabulary names as open —
// DISABLED, NO_GATE or ALLOWED. The core witnesses the verdict verbatim and
// gates only on DENIED, so an evaluation carrying any other witnessed verdict
// (an unknown string, NOT_REACHED, an empty one, a case-folded one) is an
// UNSUPPORTED_SHAPE, never a legal attempt. The factset seam refuses such a
// verdict one step earlier: a reached decision witnesses one of the four gate
// states, so a self-digested factset carrying anything else is INCOMPLETE by
// its values and never reaches the evaluator as COMPLETE.
func TestP2ActionMapRequiresAnExplicitlyOpenHealthVerdict(t *testing.T) {
	_, fs := selectedFactset(t, nil, nil)
	relabel := func(verdict string) p4offline.CommonFactset {
		out := fs
		out.HealthState = verdict
		out.Digest = digestOf(p4offline.SerializeCommonFactset(out))
		return out
	}
	for verdict, reason := range map[string]string{
		"UNKNOWN":                       predictioneval.IneligibleUnknownStageState,
		"":                              predictioneval.IneligibleUnknownStageState,
		"allowed":                       predictioneval.IneligibleUnknownStageState,
		"denied":                        predictioneval.IneligibleUnknownStageState,
		predictioneval.HealthNotReached: predictioneval.IneligibleInconsistentStageStates,
	} {
		out := relabel(verdict)
		if err := p4offline.VerifyCommonFactset(out); !errors.Is(err, p4offline.ErrFactsetInconsistent) || !strings.Contains(err.Error(), reason) {
			t.Fatalf("health %q: a COMPLETE factset with a verdict outside the gate vocabulary is inconsistent by its values (%s), got %v", verdict, reason, err)
		}
		if _, err := p4offline.EvaluateP2Case(out); !errors.Is(err, p4offline.ErrFactsetInconsistent) {
			t.Fatalf("health %q: the evaluator must not read it, got %v", verdict, err)
		}
		// The map's own rule, on an evaluation carrying that verdict past the
		// gate: named, never a legal attempt.
		ev := p2Eval(p2Inputs())
		ev.Limitations = append([]string(nil), ev.Limitations...)
		ev.Health.Verdict = verdict
		if m := p4offline.MapP2Action(ev); m.Legal || m.Class != p4offline.ActionUnsupportedShape ||
			!containsString(m.Illegality, "HEALTH_CONTRADICTS_PLACEMENT") {
			t.Fatalf("health %q: a verdict the vocabulary does not name as open is not an open gate: %+v", verdict, m)
		}
	}
	for _, verdict := range []string{predictioneval.HealthAllowed, predictioneval.HealthDisabled, predictioneval.HealthNoGate} {
		out := relabel(verdict)
		if err := p4offline.VerifyCommonFactset(out); err != nil {
			t.Fatalf("health %q: a self-digested factset under a gate state verifies as itself: %v", verdict, err)
		}
		res, err := p4offline.EvaluateP2Case(out)
		if err != nil {
			t.Fatalf("health %q: %v", verdict, err)
		}
		if !res.Action.Legal || res.Action.Class != p4offline.ActionWouldAttempt {
			t.Fatalf("health %q is an open gate: %+v", verdict, res.Action)
		}
	}
	// DENIED is a gate state too: it verifies, evaluates, and is the proved
	// HEALTH_GATED skip.
	res, err := p4offline.EvaluateP2Case(relabel(predictioneval.HealthDenied))
	if err != nil || !res.Action.Legal || res.Action.Class != p4offline.ActionPolicySkip || res.Action.SkipReason != predictioneval.ActionHealthGated {
		t.Fatalf("DENIED: %v %+v", err, res.Action)
	}
}

// TestP2ActionMapBindsTheHealthGateToTheStoppingStage pins the residual of the
// open-verdict rule for the three native results that can stop the evaluator
// on EITHER side of the health gate. Where the pinned policy stops decides
// where the gate must stand: a stage that stops before it (a choice
// UNSUPPORTED, a base stake INDETERMINATE, every legacy failure) leaves the
// gate NOT_REACHED — carrying no verdict — and a stake gate UNSUPPORTED or
// INDETERMINATE is reachable only through a witnessed open verdict. The stop
// is also where the evaluation ends: the stages before it ran as the path
// runs them, no other stage is in a stopping state, and every stage past the
// stop is NOT_REACHED; a proved skip's stages before its exit ran the same
// way. The invariants the map requires on every shape are tabled here too:
// a base-stake stage ran exactly when the choice was selected; a clamp that
// did not run has no final stake; the minimum threshold is the pinned one;
// an executed gate's reason is one of its vocabulary and its proposal the
// policy amount, the clamp applied exactly when it was the percent cap with
// the final stake the gate allows when it applied and the policy amount
// otherwise, and the filter applied only at its exit; a stage the path never
// ran carries
// its initial value and a stage that stopped it carries its stop and, for
// an indeterminate stake gate, only the proposal it was handed; the
// executed stealth-off stage drew nothing and echoes the base stake; and
// the policy amount is known exactly when the stake block ran or was
// skipped by a choice out of range. A hand-built evaluation that claims
// one and shows the other is an UNSUPPORTED_SHAPE, never a legal unknown.
func TestP2ActionMapBindsTheHealthGateToTheStoppingStage(t *testing.T) {
	unsupportedEarly := func() predictioneval.Evaluation {
		in := p2Inputs()
		in.Settings = nil
		return p2Eval(in)
	}()
	unsupportedLate := func() predictioneval.Evaluation {
		in := p2Inputs()
		in.RiskPresent = false
		return p2Eval(in)
	}()
	indeterminateEarly := func() predictioneval.Evaluation {
		in := p2Inputs()
		in.Balance = math.MaxInt
		in.Settings.Percentage = 200
		return p2Eval(in)
	}()
	indeterminateLate := func() predictioneval.Evaluation {
		in := p2Inputs()
		in.Balance = math.MaxInt
		in.RiskMaxStakePercent = 3
		return p2Eval(in)
	}()
	legacy := func() predictioneval.Evaluation {
		in := p2Inputs()
		in.Outcomes[0].Present = false
		return p2Eval(in)
	}()
	legacyBaseStake := func() predictioneval.Evaluation {
		in := p2Inputs()
		in.Settings.Strategy = predictioneval.StrategyNumber2
		in.Outcomes[1].Present = false
		return p2Eval(in)
	}()
	legacyFilter := func() predictioneval.Evaluation {
		in := p2Inputs()
		in.Settings.Strategy = predictioneval.StrategyNumber1
		in.Settings.FilterCondition = &predictioneval.SourceFilterCondition{By: predictioneval.OutcomeTotalUsers, Where: "GT", Value: 5}
		in.Outcomes[1].Present = false
		return p2Eval(in)
	}()
	gated := func() predictioneval.Evaluation {
		in := p2Inputs()
		in.HealthState = predictioneval.HealthDenied
		return p2Eval(in)
	}()
	placed := p2Eval(p2Inputs())
	exit := func() predictioneval.Evaluation {
		in := p2Inputs()
		in.ReachedDecision = false
		in.PreDecisionExit = "NOT_ELIGIBLE"
		return p2Eval(in)
	}()
	for name, ev := range map[string]predictioneval.Evaluation{
		"unsupported before the gate":      unsupportedEarly,
		"unsupported after the gate":       unsupportedLate,
		"indeterminate before the gate":    indeterminateEarly,
		"indeterminate after the gate":     indeterminateLate,
		"legacy failure at the choice":     legacy,
		"legacy failure at the base stake": legacyBaseStake,
		"legacy failure at the filter":     legacyFilter,
		"health gated":                     gated,
		"placement":                        placed,
		"pre-decision exit":                exit,
	} {
		if m := p4offline.MapP2Action(ev); !m.Legal {
			t.Fatalf("fixture %s: the evaluator's own shape is legal: %+v (action %s)", name, m, ev.Action)
		}
	}
	if unsupportedEarly.Action != predictioneval.ActionUnsupported || unsupportedEarly.Health.State != predictioneval.StageStateNotReached ||
		unsupportedLate.Action != predictioneval.ActionUnsupported || unsupportedLate.StakeGate.State != predictioneval.StageStateUnsupported ||
		indeterminateEarly.Action != predictioneval.ActionIndeterminate || indeterminateEarly.BaseStake.State != predictioneval.StageStateIndeterminate ||
		indeterminateEarly.Health.State != predictioneval.StageStateNotReached ||
		indeterminateLate.Action != predictioneval.ActionIndeterminate || indeterminateLate.StakeGate.State != predictioneval.StageStateIndeterminate ||
		legacy.Action != predictioneval.ActionLegacyFailure || legacy.Choice.State != predictioneval.StageStateLegacyFailure ||
		legacyBaseStake.Action != predictioneval.ActionLegacyFailure || legacyBaseStake.BaseStake.State != predictioneval.StageStateLegacyFailure ||
		legacyFilter.Action != predictioneval.ActionLegacyFailure || legacyFilter.Filter.State != predictioneval.StageStateLegacyFailure ||
		legacy.Health.State != predictioneval.StageStateNotReached || legacyFilter.Health.State != predictioneval.StageStateNotReached {
		t.Fatalf("fixture: %s/%s/%s/%s/%s/%s/%s", unsupportedEarly.Action, unsupportedLate.Action, indeterminateEarly.Action, indeterminateLate.Action,
			legacy.Action, legacyBaseStake.Action, legacyFilter.Action)
	}
	cases := []struct {
		name       string
		base       predictioneval.Evaluation
		tamper     func(*predictioneval.Evaluation)
		illegality string
	}{
		{"unsupported choice with the gate witnessed", unsupportedEarly, func(e *predictioneval.Evaluation) {
			e.Health = predictioneval.HealthStageResult{State: predictioneval.StageStateWitnessed, Verdict: predictioneval.HealthAllowed}
		}, "HEALTH_CONTRADICTS_UNSUPPORTED_STAGE"},
		{"unsupported stake gate with the health gate unreached", unsupportedLate, func(e *predictioneval.Evaluation) {
			e.Health = predictioneval.HealthStageResult{State: predictioneval.StageStateNotReached}
		}, "HEALTH_CONTRADICTS_UNSUPPORTED_STAGE"},
		{"unsupported stake gate under a denied verdict", unsupportedLate, func(e *predictioneval.Evaluation) {
			e.Health.Verdict = predictioneval.HealthDenied
		}, "HEALTH_CONTRADICTS_UNSUPPORTED_STAGE"},
		{"indeterminate stake gate with the health gate unreached", indeterminateLate, func(e *predictioneval.Evaluation) {
			e.Health = predictioneval.HealthStageResult{State: predictioneval.StageStateNotReached}
		}, "HEALTH_CONTRADICTS_INDETERMINATE_STAGE"},
		{"indeterminate stake gate under a denied verdict", indeterminateLate, func(e *predictioneval.Evaluation) {
			e.Health.Verdict = predictioneval.HealthDenied
		}, "HEALTH_CONTRADICTS_INDETERMINATE_STAGE"},
		{"indeterminate base stake with the gate witnessed", indeterminateEarly, func(e *predictioneval.Evaluation) {
			e.Health = predictioneval.HealthStageResult{State: predictioneval.StageStateWitnessed, Verdict: predictioneval.HealthAllowed}
		}, "HEALTH_CONTRADICTS_INDETERMINATE_STAGE"},
		{"legacy failure with the gate witnessed", legacy, func(e *predictioneval.Evaluation) {
			e.Health = predictioneval.HealthStageResult{State: predictioneval.StageStateWitnessed, Verdict: predictioneval.HealthDenied}
		}, "HEALTH_REACHED_AFTER_LEGACY_FAILURE"},
		{"legacy failure with a stale verdict on the unreached gate", legacy, func(e *predictioneval.Evaluation) {
			e.Health.Verdict = predictioneval.HealthDenied
		}, "HEALTH_REACHED_AFTER_LEGACY_FAILURE"},
		{"legacy failure with a stale reason on the unreached gate", legacy, func(e *predictioneval.Evaluation) {
			e.Health.Reason = "stale"
		}, "HEALTH_REACHED_AFTER_LEGACY_FAILURE"},
		{"legacy failure with the stake gate executed", legacy, func(e *predictioneval.Evaluation) {
			e.StakeGate.State = predictioneval.StageStateExecuted
		}, "STAGES_REACHED_AFTER_LEGACY_FAILURE"},
		{"unsupported choice with the stake gate executed", unsupportedEarly, func(e *predictioneval.Evaluation) {
			e.StakeGate.State = predictioneval.StageStateExecuted
		}, "STAGES_REACHED_AFTER_UNSUPPORTED_STAGE"},
		{"unsupported stake gate with the clamp executed", unsupportedLate, func(e *predictioneval.Evaluation) {
			e.Clamp.State = predictioneval.StageStateExecuted
			e.Clamp.HasFinal = true
		}, "STAGES_REACHED_AFTER_UNSUPPORTED_STAGE"},
		{"unsupported at both the choice and the stake gate", unsupportedLate, func(e *predictioneval.Evaluation) {
			e.Choice.State = predictioneval.StageStateUnsupported
		}, "ANOTHER_STAGE_STOPPED"},
		{"indeterminate at both the base stake and the stake gate", indeterminateLate, func(e *predictioneval.Evaluation) {
			e.BaseStake.State = predictioneval.StageStateIndeterminate
		}, "ANOTHER_STAGE_STOPPED"},
		{"legacy failure at two stages", legacyFilter, func(e *predictioneval.Evaluation) {
			e.BaseStake.State = predictioneval.StageStateLegacyFailure
		}, "ANOTHER_STAGE_STOPPED"},
		{"indeterminate base stake beside an unsupported choice", indeterminateEarly, func(e *predictioneval.Evaluation) {
			e.Choice.State = predictioneval.StageStateUnsupported
		}, "ANOTHER_STAGE_STOPPED"},
		{"legacy choice with the base stake executed", legacy, func(e *predictioneval.Evaluation) {
			e.BaseStake.State = predictioneval.StageStateExecuted
		}, "STAGES_REACHED_AFTER_LEGACY_FAILURE"},
		{"legacy base stake with the filter executed", legacyBaseStake, func(e *predictioneval.Evaluation) {
			e.Filter.State = predictioneval.StageStateExecuted
		}, "STAGES_REACHED_AFTER_LEGACY_FAILURE"},
		{"unsupported choice with the base stake executed", unsupportedEarly, func(e *predictioneval.Evaluation) {
			e.BaseStake.State = predictioneval.StageStateExecuted
		}, "STAGES_REACHED_AFTER_UNSUPPORTED_STAGE"},
		{"indeterminate base stake with the filter executed", indeterminateEarly, func(e *predictioneval.Evaluation) {
			e.Filter.State = predictioneval.StageStateExecuted
		}, "STAGES_REACHED_AFTER_INDETERMINATE_STAGE"},
		{"health gate claimed with a selected choice and no base stake", gated, func(e *predictioneval.Evaluation) {
			e.BaseStake = predictioneval.BaseStakeStage{State: predictioneval.StageStateNotReached}
		}, "SELECTION_CONTRADICTS_BASE_STAKE"},
		{"pre-decision exit with a final stake on the unreached clamp", exit, func(e *predictioneval.Evaluation) {
			e.Clamp.HasFinal = true
		}, "FINAL_STAKE_WITHOUT_CLAMP"},
		{"legacy failure claimed with no stage failed", exit, func(e *predictioneval.Evaluation) {
			e.Action, e.LegacyFailure = predictioneval.ActionLegacyFailure, "PINNED_POLICY_PANIC_ABSENT_OUTCOME"
		}, "NO_STAGE_FAILED"},
		{"indeterminate claimed with no stage indeterminate", exit, func(e *predictioneval.Evaluation) {
			e.Action = predictioneval.ActionIndeterminate
		}, "NO_STAGE_INDETERMINATE"},
		{"unsupported claimed with no stage unsupported", exit, func(e *predictioneval.Evaluation) {
			e.Action = predictioneval.ActionUnsupported
		}, "NO_STAGE_UNSUPPORTED"},
		{"indeterminate base stake with the stake gate executed", indeterminateEarly, func(e *predictioneval.Evaluation) {
			e.StakeGate.State = predictioneval.StageStateExecuted
		}, "STAGES_REACHED_AFTER_INDETERMINATE_STAGE"},
		{"pre-decision exit with a stale verdict on the unreached gate", exit, func(e *predictioneval.Evaluation) {
			e.Health.Verdict = predictioneval.HealthAllowed
		}, "STAGES_REACHED_BEFORE_DECISION"},
		{"unsupported stake gate with the choice never reached", unsupportedLate, func(e *predictioneval.Evaluation) {
			e.Choice.State = predictioneval.StageStateNotReached
		}, "STAGES_NOT_RUN_BEFORE_STOP"},
		{"unsupported stake gate with the filter never reached", unsupportedLate, func(e *predictioneval.Evaluation) {
			e.Filter.State = predictioneval.StageStateNotReached
		}, "STAGES_NOT_RUN_BEFORE_STOP"},
		{"legacy filter with the choice never reached", legacyFilter, func(e *predictioneval.Evaluation) {
			e.Choice.State = predictioneval.StageStateNotReached
		}, "STAGES_NOT_RUN_BEFORE_STOP"},
		{"indeterminate stake gate with the stealth stage never run", indeterminateLate, func(e *predictioneval.Evaluation) {
			e.Stealth.State = predictioneval.StageStateNotReached
		}, "STAGES_NOT_RUN_BEFORE_STOP"},
		{"health gate claimed with the stealth stage never run", gated, func(e *predictioneval.Evaluation) {
			e.Stealth.State = predictioneval.StageStateNotReached
		}, "STAGES_NOT_RUN_BEFORE_EXIT"},
		{"pre-decision exit with a minimum verdict on the unreached stage", exit, func(e *predictioneval.Evaluation) {
			e.Minimum.Below = true
		}, "UNREACHED_STAGE_CARRIES_A_VALUE"},
		{"pre-decision exit with a gate reason on the unreached gate", exit, func(e *predictioneval.Evaluation) {
			e.StakeGate.Reason = predictioneval.GateReserveViolation
		}, "UNREACHED_STAGE_CARRIES_A_VALUE"},
		{"unsupported choice with a capped base stake that never ran", unsupportedEarly, func(e *predictioneval.Evaluation) {
			e.BaseStake.Capped = true
		}, "UNREACHED_STAGE_CARRIES_A_VALUE"},
		{"legacy choice with a filter answer on the unreached filter", legacy, func(e *predictioneval.Evaluation) {
			e.Filter.Skip = true
		}, "UNREACHED_STAGE_CARRIES_A_VALUE"},
		{"indeterminate base stake with a stealth flag on the unreached stage", indeterminateEarly, func(e *predictioneval.Evaluation) {
			e.Stealth.Applies = true
		}, "UNREACHED_STAGE_CARRIES_A_VALUE"},
		{"health gate claimed with a clamp answer on the unreached clamp", gated, func(e *predictioneval.Evaluation) {
			e.Clamp.Applied = true
		}, "UNREACHED_STAGE_CARRIES_A_VALUE"},
		{"pre-decision exit with a choice index on the unreached choice", exit, func(e *predictioneval.Evaluation) {
			e.Choice.Index = 0
		}, "UNREACHED_STAGE_CARRIES_A_VALUE"},
		{"pre-decision exit with a known policy amount", exit, func(e *predictioneval.Evaluation) {
			e.PolicyAmountKnown = true
		}, "AMOUNT_KNOWN_CONTRADICTS_STAKE_BLOCK"},
		{"indeterminate base stake with a known policy amount", indeterminateEarly, func(e *predictioneval.Evaluation) {
			e.PolicyAmountKnown = true
		}, "AMOUNT_KNOWN_CONTRADICTS_STAKE_BLOCK"},
		{"health gate claimed with an unknown policy amount", gated, func(e *predictioneval.Evaluation) {
			e.PolicyAmountKnown = false
		}, "AMOUNT_KNOWN_CONTRADICTS_STAKE_BLOCK"},
		{"placement with a gate reason outside the vocabulary", placed, func(e *predictioneval.Evaluation) {
			e.StakeGate.Reason = "not_a_gate_reason"
		}, "GATE_REASON_OUTSIDE_VOCABULARY"},
		{"pre-decision exit with an unpinned minimum threshold", exit, func(e *predictioneval.Evaluation) {
			e.Minimum.Threshold = 0
		}, "MINIMUM_NOT_PINNED"},
		{"health gate claimed with an unpinned minimum threshold", gated, func(e *predictioneval.Evaluation) {
			e.Minimum.Threshold = predictioneval.PinnedMinimumStake + 1
		}, "MINIMUM_NOT_PINNED"},
		{"legacy choice with a not-applicable stealth mark on the unreached stage", legacy, func(e *predictioneval.Evaluation) {
			e.Stealth.Outcome = predictioneval.StealthNotApplicable
		}, "UNREACHED_STAGE_CARRIES_A_VALUE"},
		{"placement with a stealth flag on the executed stage", placed, func(e *predictioneval.Evaluation) {
			e.Stealth.Applies = true
		}, "STEALTH_REALIZATION_PRESENT"},
		{"placement with a derived reduction on the executed stage", placed, func(e *predictioneval.Evaluation) {
			e.Stealth.ReductionDerived = true
		}, "STEALTH_REALIZATION_PRESENT"},
		{"unsupported stake gate with a reason on the stopped gate", unsupportedLate, func(e *predictioneval.Evaluation) {
			e.StakeGate.Reason = predictioneval.GateReserveViolation
		}, "STOPPED_STAGE_CARRIES_AN_ANSWER"},
		{"indeterminate stake gate with an allowance on the stopped gate", indeterminateLate, func(e *predictioneval.Evaluation) {
			e.StakeGate.Allowed = 5
		}, "STOPPED_STAGE_CARRIES_AN_ANSWER"},
		{"unsupported stake gate with a proposal on the stopped gate", unsupportedLate, func(e *predictioneval.Evaluation) {
			e.StakeGate.Proposed = 999
		}, "STOPPED_STAGE_CARRIES_AN_ANSWER"},
		{"legacy filter with an answer on the stopped filter", legacyFilter, func(e *predictioneval.Evaluation) {
			e.Filter.Skip = true
		}, "STOPPED_STAGE_CARRIES_AN_ANSWER"},
		{"legacy choice with an index on the stopped choice", legacy, func(e *predictioneval.Evaluation) {
			e.Choice.Index = 0
		}, "STOPPED_STAGE_CARRIES_AN_ANSWER"},
		{"placement with a clamp that applied under an uncapped gate", placed, func(e *predictioneval.Evaluation) {
			e.Clamp.Applied = true
		}, "CLAMP_CONTRADICTS_GATE"},
		{"placement with a final stake the clamp did not derive", placed, func(e *predictioneval.Evaluation) {
			e.Clamp.FinalAmount = e.PolicyAmount + 1
		}, "CLAMP_CONTRADICTS_GATE"},
		{"unsupported stake gate with a filter that applied", unsupportedLate, func(e *predictioneval.Evaluation) {
			e.Filter.Applied = true
		}, "FILTER_APPLIED_WITHOUT_A_FILTER_EXIT"},
		{"indeterminate base stake with an amount on the stopped stage", indeterminateEarly, func(e *predictioneval.Evaluation) {
			e.BaseStake.Amount = 7
		}, "STOPPED_STAGE_CARRIES_AN_ANSWER"},
		{"placement with a realization of its own on the stealth stage", placed, func(e *predictioneval.Evaluation) {
			e.Stealth.Realized = e.PolicyAmount + 1
		}, "STEALTH_REALIZATION_PRESENT"},
		{"placement with a percent-capped gate and a clamp that did not apply", placed, func(e *predictioneval.Evaluation) {
			e.StakeGate.Reason = predictioneval.GatePercent
		}, "CLAMP_CONTRADICTS_GATE"},
	}
	// A claim with no stopping stage at all is named by that alone: the
	// health clause of the arm does not fire beside it.
	alone := map[string]bool{
		"legacy failure claimed with no stage failed":       true,
		"indeterminate claimed with no stage indeterminate": true,
		"unsupported claimed with no stage unsupported":     true,
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := tc.base
			ev.Limitations = append([]string(nil), ev.Limitations...)
			tc.tamper(&ev)
			m := p4offline.MapP2Action(ev)
			if m.Legal || m.Class != p4offline.ActionUnsupportedShape || !containsString(m.Illegality, tc.illegality) {
				t.Fatalf("%s: want %s, got %+v", tc.name, tc.illegality, m)
			}
			if alone[tc.name] && len(m.Illegality) != 1 {
				t.Fatalf("the one contradiction must be named alone: %+v", m.Illegality)
			}
		})
	}
}

// TestP2ActionMapRefusesAnAttemptWithoutASelectedChoice pins the one shape
// the evaluator itself produces that the map refuses on purpose: a strategy
// outside the pinned policy's closed set leaves the choice at -1, and a zero
// minimum stake — which no dataset binds, the projection pins it — lets the
// zero amount through to WOULD_ATTEMPT_PLACEMENT. An attempt without a
// chosen outcome has no choice to score: it is an UNSUPPORTED_SHAPE whose
// contradictions name the unselected choice, the stake-block stages it
// never ran and the unpinned minimum, never a WOULD_ATTEMPT. The factset
// seam refuses the unpinned minimum one step earlier: a factset digested by
// hand under any other minimum is INCOMPLETE by its values.
func TestP2ActionMapRefusesAnAttemptWithoutASelectedChoice(t *testing.T) {
	_, fs := selectedFactset(t, nil, nil)
	unpinned := fs
	unpinned.MinimumStake = 0
	unpinned.Digest = digestOf(p4offline.SerializeCommonFactset(unpinned))
	if err := p4offline.VerifyCommonFactset(unpinned); !errors.Is(err, p4offline.ErrFactsetInconsistent) ||
		!strings.Contains(err.Error(), p4offline.FactsetReasonMinimumStakeNotPinned) {
		t.Fatalf("a COMPLETE factset under an unpinned minimum is inconsistent by its values, got %v", err)
	}
	if _, err := p4offline.EvaluateP2Case(unpinned); !errors.Is(err, p4offline.ErrFactsetInconsistent) {
		t.Fatalf("the evaluator must not read it, got %v", err)
	}
	in := p2Inputs()
	in.Settings.Strategy = "NOT_A_PINNED_STRATEGY"
	in.MinimumStake = 0
	ev := p2Eval(in)
	if ev.Action != predictioneval.ActionWouldAttemptPlacement || ev.Choice.Selected || ev.BaseStake.State != predictioneval.StageStateNotReached {
		t.Fatalf("fixture: the pinned policy must reach a placement with no selected choice: %s %+v", ev.Action, ev.Choice)
	}
	m := p4offline.MapP2Action(ev)
	if m.Legal || m.Class != p4offline.ActionUnsupportedShape || !containsString(m.Illegality, "CHOICE_NOT_SELECTED") ||
		!containsString(m.Illegality, "BASE_STAKE_NOT_EXECUTED") || !containsString(m.Illegality, "MINIMUM_NOT_PINNED") {
		t.Fatalf("an attempt with no selected choice is refused, never counted: %+v", m)
	}
}

// TestP2ActionMapRequiresACoherentExecutedPercentCap pins the numeric shape of
// the one stage whose answer the counted stake is minted from. The pinned
// policy hands the gate the policy amount as its proposal, caps only when the
// percent limit it computed is BELOW that proposal, and publishes as its
// allowance that limit under its own zero floor; the clamp then applies
// exactly on that cap, with the gate's allowance as the final stake — and the
// policy amount as the final stake when no cap applied. A hand-built
// evaluation that claims a cap whose numbers do not stand in those relations
// is an UNSUPPORTED_SHAPE, however self-consistent the pair it forged.
//
// Every expectation below is derived from the pinned policy's own arithmetic
// over the fixture's literal inputs, never from the map's answer: a balance of
// 1000 at 5% is a base stake of 50, and a 2% cap is 2*1000/100 = 20, which is
// below 50, so the gate caps at 20 and the clamp's final stake is 20.
func TestP2ActionMapRequiresACoherentExecutedPercentCap(t *testing.T) {
	const balance, percent, capPercent = 1000, 5, 2
	const wantPolicy, wantLimit, wantAllowed, wantFinal = 50, 20, 20, 20
	if in := p2Inputs(); in.Balance != balance || in.Settings.Percentage != percent {
		t.Fatalf("fixture: the expectation is derived from balance %d at %d%%, got %d at %d%%",
			balance, percent, in.Balance, in.Settings.Percentage)
	}
	capped := func() predictioneval.Evaluation {
		in := p2Inputs()
		in.RiskMaxStakePercent = capPercent
		return p2Eval(in)
	}()
	if capped.Action != predictioneval.ActionWouldAttemptPlacement || capped.PolicyAmount != wantPolicy ||
		capped.StakeGate.State != predictioneval.StageStateExecuted || capped.StakeGate.Reason != predictioneval.GatePercent ||
		capped.StakeGate.Proposed != wantPolicy || capped.StakeGate.Limit != wantLimit || capped.StakeGate.Allowed != wantAllowed ||
		capped.Clamp.State != predictioneval.StageStateExecuted || !capped.Clamp.Applied || capped.Clamp.FinalAmount != wantFinal {
		t.Fatalf("fixture: the pinned policy caps %d to %d: %s %+v %+v", wantPolicy, wantAllowed, capped.Action, capped.StakeGate, capped.Clamp)
	}
	if m := p4offline.MapP2Action(capped); !m.Legal || m.Class != p4offline.ActionWouldAttempt {
		t.Fatalf("a coherent percent cap is a legal attempt: %+v", m)
	}

	t.Run("the seam mints the capped stake", func(t *testing.T) {
		_, fs := selectedFactset(t, func(env *predictioneval.SourceDecisionEnvelope) {
			env.RiskMaxStakePercent = ptrInt(capPercent)
		}, nil)
		if fs.Balance != balance || fs.Settings == nil || fs.Settings.Percentage != percent || fs.RiskMaxStakePercent != capPercent {
			t.Fatalf("fixture: %d at %d%% under a %d%% cap, got %+v", balance, percent, capPercent, fs)
		}
		res, err := p4offline.EvaluateP2Case(fs)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Action.Legal || res.Action.Class != p4offline.ActionWouldAttempt {
			t.Fatalf("%+v", res.Action)
		}
		if res.Stake != p4offline.KnownInt64(wantFinal) {
			t.Fatalf("the case's stake is the gate's allowance %d, not the uncapped %d: %+v", wantFinal, wantPolicy, res.Stake)
		}
	})

	// Each tamper violates exactly one relation, so each names exactly one
	// contradiction: that is what makes the table discriminating.
	for _, tc := range []struct {
		name       string
		tamper     func(*predictioneval.Evaluation)
		illegality string
	}{
		{"a final stake above the allowance the gate recorded", func(e *predictioneval.Evaluation) {
			e.Clamp.FinalAmount = wantAllowed + 1
		}, "CLAMP_CONTRADICTS_GATE"},
		{"an allowance and a final stake forged together", func(e *predictioneval.Evaluation) {
			e.StakeGate.Allowed, e.Clamp.FinalAmount = 1_000_000, 1_000_000
		}, "GATE_ALLOWANCE_CONTRADICTS_THE_CAP"},
		{"a cap whose allowance is the proposal it did not reduce", func(e *predictioneval.Evaluation) {
			e.StakeGate.Allowed, e.Clamp.FinalAmount = wantPolicy, wantPolicy
		}, "GATE_ALLOWANCE_CONTRADICTS_THE_CAP"},
		{"a cap whose limit reduced nothing", func(e *predictioneval.Evaluation) {
			e.StakeGate.Limit, e.StakeGate.Allowed, e.Clamp.FinalAmount = wantPolicy, wantPolicy, wantPolicy
		}, "GATE_CAP_DID_NOT_REDUCE_THE_PROPOSAL"},
		{"an allowance below the proposal but not the limit", func(e *predictioneval.Evaluation) {
			e.StakeGate.Allowed, e.Clamp.FinalAmount = wantPolicy-1, wantPolicy-1
		}, "GATE_ALLOWANCE_CONTRADICTS_THE_CAP"},
		{"a proposal that is not the policy amount", func(e *predictioneval.Evaluation) {
			e.StakeGate.Proposed = wantPolicy + 1
		}, "GATE_PROPOSAL_CONTRADICTS_POLICY_AMOUNT"},
		{"a cap whose limit exceeds the proposal", func(e *predictioneval.Evaluation) {
			e.StakeGate.Limit, e.StakeGate.Allowed, e.Clamp.FinalAmount =
				wantPolicy*2, wantPolicy*2, wantPolicy*2
		}, "GATE_CAP_DID_NOT_REDUCE_THE_PROPOSAL"},
		{"an allowance below the limit the cap computed", func(e *predictioneval.Evaluation) {
			e.StakeGate.Allowed, e.Clamp.FinalAmount =
				wantAllowed/2, wantAllowed/2
		}, "GATE_ALLOWANCE_CONTRADICTS_THE_CAP"},
		{"a proposal below the policy amount", func(e *predictioneval.Evaluation) {
			e.StakeGate.Proposed = wantPolicy - 1
		}, "GATE_PROPOSAL_CONTRADICTS_POLICY_AMOUNT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := capped
			ev.Limitations = append([]string(nil), ev.Limitations...)
			tc.tamper(&ev)
			m := p4offline.MapP2Action(ev)
			if m.Legal || m.Class != p4offline.ActionUnsupportedShape || !containsString(m.Illegality, tc.illegality) {
				t.Fatalf("%s: want %s, got %+v", tc.name, tc.illegality, m)
			}
			if len(m.Illegality) != 1 {
				t.Fatalf("%s: the one contradiction must be named alone: %+v", tc.name, m.Illegality)
			}
		})
	}

	// Native shapes the cap relations must NOT refuse. The reserve violation
	// and the overflowing percent gate are already driven through the map by
	// TestP2NativeActionMapIsExhaustiveOverLegalShapes.
	t.Run("native shapes the cap relations admit", func(t *testing.T) {
		// The core's own zero floor: -50 at 1% is a base stake of 0, the 2%
		// cap computes -100/100 = -1, which is below that proposal, and the
		// floor publishes an allowance of 0. The stake is then below the
		// pinned minimum, so the case is a proved skip.
		zeroFloor := func() predictioneval.Evaluation {
			in := p2Inputs()
			in.Balance, in.Settings.Percentage, in.RiskMaxStakePercent = -50, 1, capPercent
			return p2Eval(in)
		}()
		if zeroFloor.PolicyAmount != 0 || zeroFloor.StakeGate.Reason != predictioneval.GatePercent ||
			zeroFloor.StakeGate.Proposed != 0 || zeroFloor.StakeGate.Limit != -1 || zeroFloor.StakeGate.Allowed != 0 ||
			!zeroFloor.Clamp.Applied || zeroFloor.Clamp.FinalAmount != 0 {
			t.Fatalf("fixture: a negative limit under the zero floor: %+v %+v", zeroFloor.StakeGate, zeroFloor.Clamp)
		}
		// No cap at all on a negative proposal: the gate floors its allowance
		// to 0 while the clamp's final stake stays the policy amount, which is
		// why the uncapped arm is bound to the policy amount and not to the
		// allowance.
		negative := func() predictioneval.Evaluation {
			in := p2Inputs()
			in.Balance = -1000
			return p2Eval(in)
		}()
		if negative.PolicyAmount != -50 || negative.StakeGate.Reason != predictioneval.GateNone ||
			negative.StakeGate.Allowed != 0 || negative.Clamp.Applied || negative.Clamp.FinalAmount != -50 {
			t.Fatalf("fixture: an uncapped negative proposal: %+v %+v", negative.StakeGate, negative.Clamp)
		}
		// A strategy outside the pinned policy's closed set leaves the choice
		// at -1 and the stake block unrun; under the pinned minimum that is a
		// legal skip, not an attempt.
		noChoice := func() predictioneval.Evaluation {
			in := p2Inputs()
			in.Settings.Strategy = "NOT_A_PINNED_STRATEGY"
			return p2Eval(in)
		}()
		if noChoice.Choice.Selected || noChoice.PolicyAmount != 0 || noChoice.StakeGate.State != predictioneval.StageStateExecuted {
			t.Fatalf("fixture: an unselected choice reaching the gate: %+v %+v", noChoice.Choice, noChoice.StakeGate)
		}
		for name, ev := range map[string]predictioneval.Evaluation{
			"a percent cap under the core's zero floor": zeroFloor,
			"an uncapped negative proposal":             negative,
			"an early skip with no selected choice":     noChoice,
		} {
			if ev.Action != predictioneval.ActionBelowMinimum {
				t.Fatalf("%s: fixture is a below-minimum skip, got %s", name, ev.Action)
			}
			if m := p4offline.MapP2Action(ev); !m.Legal || m.Class != p4offline.ActionPolicySkip ||
				m.SkipReason != predictioneval.ActionBelowMinimum {
				t.Fatalf("%s: a native shape the map must admit: %+v", name, m)
			}
		}
	})
}
