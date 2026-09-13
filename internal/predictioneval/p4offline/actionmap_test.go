package p4offline_test

// SEAM 7: the exhaustive native action map.
//
// The expected CLASS for every native action comes from the protocol's own
// table, never from the code: WOULD_ATTEMPT_PLACEMENT is WOULD_ATTEMPT; the
// four proved skips are POLICY_SKIP; PRE_DECISION_EXIT is coverage-only;
// LEGACY_FAILURE stays LEGACY_FAILURE; INDETERMINATE and UNSUPPORTED are
// UNKNOWN_INPUT. Every legal shape below is a REAL evaluation produced by the
// native evaluator over hand-built inputs, so the map's legality rules cannot
// be stricter than what the evaluator produces; every illegal shape is a
// legal one with exactly one stage tampered.

import (
	"math"
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
