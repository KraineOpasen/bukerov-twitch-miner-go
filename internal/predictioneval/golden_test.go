package predictioneval_test

// LITERAL GOLDENS.
//
// The parity suite generates thousands of cases and compares them against the
// real policy. That is the stronger oracle, and it has one weakness: if both
// implementations were wrong in the same way, it would agree. These examples
// close that gap by stating the expected numbers OUTRIGHT, computed by hand
// from the pinned policy's source and written down here with the arithmetic
// shown.
//
// They are also the mutation targets. Each one pins a specific decision the
// pinned policy makes that a plausible edit would change: a tie kept at the
// lowest index, a gap compared strictly, a NUMBER fallback to slot 0, a
// reserve floor that abandons rather than shrinks, an allowance of zero sitting
// beside a stake the caller kept anyway. A mutant that flips any of these is
// caught by name here rather than by a sweep that says only "something drifted".

import (
	"encoding/json"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
)

// goldenCase is one hand-computed decision.
type goldenCase struct {
	name     string
	inputs   predictioneval.DecisionInputs
	observed predictioneval.ObservedRealization

	wantChoice    int
	wantOutcomeID string
	wantBaseStake int
	wantAmount    int
	wantSkip      bool
	wantCompared  float64
	wantAllowed   int
	wantReason    string
	wantLimit     int
	wantClamp     bool
	wantFinal     int
	wantHasFinal  bool
	wantAction    string
}

// gi builds inputs with the boring parts filled in, so each golden shows only
// what it is about.
func gi(strategy string, pct, gap, maxPoints, balance, maxStakePct, reserve int,
	outs []predictioneval.OutcomeInput, fc *predictioneval.SourceFilterCondition, stealth bool,
) predictioneval.DecisionInputs {
	return predictioneval.DecisionInputs{
		ReachedDecision:     true,
		Balance:             balance,
		BalancePresent:      true,
		Outcomes:            outs,
		OutcomesPresent:     true,
		RiskPresent:         true,
		RiskMaxStakePercent: maxStakePct,
		RiskReservePoints:   reserve,
		HealthState:         predictioneval.HealthAllowed,
		MinimumStake:        predictioneval.PinnedMinimumStake,
		Settings: &predictioneval.BetSettingsInput{
			Strategy: strategy, Percentage: pct, PercentageGap: gap,
			MaxPoints: maxPoints, StealthMode: stealth, FilterCondition: fc,
		},
	}
}

func go1(slot int, id string, users, points, top int, pctUsers, odds, oddsPct float64) predictioneval.OutcomeInput {
	return predictioneval.OutcomeInput{
		Slot: slot, Present: true, ID: id,
		TotalUsers: users, TotalPoints: points, TopPoints: top,
		PercentageUsers: pctUsers, Odds: odds, OddsPercentage: oddsPct,
	}
}

func TestGoldenDecisionsMatchTheHandComputedPolicy(t *testing.T) {
	// A plain two-outcome round used by several goldens.
	plain := []predictioneval.OutcomeInput{
		go1(0, "o1", 6, 600, 90, 60, 1.66, 60.24),
		go1(1, "o2", 4, 400, 80, 40, 2.5, 40),
	}
	// An exact tie on every selectable key.
	tied := []predictioneval.OutcomeInput{
		go1(0, "tie-a", 5, 500, 70, 50, 2.0, 50),
		go1(1, "tie-b", 5, 500, 70, 50, 2.0, 50),
	}

	cases := []goldenCase{
		{
			// MOST_VOTED picks max total_users: 6 > 4 -> slot 0.
			// stake = int(1000 * 5/100) = 50; 50 <= MaxPoints.
			// No risk gates: allowed = 50, reason "", limit 0, final 50, 50 >= 10.
			name:       "most voted, no gates, places",
			inputs:     gi(predictioneval.StrategyMostVoted, 5, 20, 50_000, 1000, 0, 0, plain, nil, false),
			wantChoice: 0, wantOutcomeID: "o1", wantBaseStake: 50, wantAmount: 50,
			wantAllowed: 50, wantReason: "", wantLimit: 0,
			wantFinal: 50, wantHasFinal: true, wantAction: predictioneval.ActionWouldAttemptPlacement,
		},
		{
			// returnChoice compares with STRICT >, so an exact tie keeps the
			// FIRST index. A mutant using >= would answer slot 1.
			name:       "an exact tie keeps the lowest index",
			inputs:     gi(predictioneval.StrategyMostVoted, 5, 20, 50_000, 1000, 0, 0, tied, nil, false),
			wantChoice: 0, wantOutcomeID: "tie-a", wantBaseStake: 50, wantAmount: 50,
			wantAllowed: 50, wantFinal: 50, wantHasFinal: true,
			wantAction: predictioneval.ActionWouldAttemptPlacement,
		},
		{
			// SMART: |60 - 40| = 20, gap 20. The comparison is STRICT <, so
			// 20 < 20 is FALSE and the TOTAL_USERS branch runs -> slot 0.
			// A mutant using <= would take the odds branch and answer slot 1
			// (odds 2.5 > 1.66).
			name:       "SMART with difference exactly equal to the gap takes the total-users branch",
			inputs:     gi(predictioneval.StrategySmart, 5, 20, 50_000, 1000, 0, 0, plain, nil, false),
			wantChoice: 0, wantOutcomeID: "o1", wantBaseStake: 50, wantAmount: 50,
			wantAllowed: 50, wantFinal: 50, wantHasFinal: true,
			wantAction: predictioneval.ActionWouldAttemptPlacement,
		},
		{
			// Same vector, gap 21: |60-40| = 20 < 21 -> the ODDS branch runs,
			// and max odds is 2.5 at slot 1.
			name:       "SMART with difference below the gap takes the odds branch",
			inputs:     gi(predictioneval.StrategySmart, 5, 21, 50_000, 1000, 0, 0, plain, nil, false),
			wantChoice: 1, wantOutcomeID: "o2", wantBaseStake: 50, wantAmount: 50,
			wantAllowed: 50, wantFinal: 50, wantHasFinal: true,
			wantAction: predictioneval.ActionWouldAttemptPlacement,
		},
		{
			// NUMBER_3 asks for slot 2, the vector has 2 entries, so
			// returnNumberChoice falls back to slot 0 — NOT to "no choice".
			name:       "NUMBER_3 on a two-outcome round falls back to slot 0",
			inputs:     gi(predictioneval.StrategyNumber3, 5, 20, 50_000, 1000, 0, 0, plain, nil, false),
			wantChoice: 0, wantOutcomeID: "o1", wantBaseStake: 50, wantAmount: 50,
			wantAllowed: 50, wantFinal: 50, wantHasFinal: true,
			wantAction: predictioneval.ActionWouldAttemptPlacement,
		},
		{
			// SMART needs two outcomes; with one it leaves the choice at -1
			// and the stake block never runs, so the amount is the
			// initialized 0. With no filter there is no panic.
			// EvaluateStake(0, ...) -> allowed 0, then 0 < 10.
			name: "SMART with a single outcome chooses nothing and stops at the minimum",
			inputs: gi(predictioneval.StrategySmart, 5, 20, 50_000, 1000, 0, 0,
				[]predictioneval.OutcomeInput{go1(0, "only", 6, 600, 90, 60, 1.66, 60.24)}, nil, false),
			wantChoice: -1, wantOutcomeID: "", wantBaseStake: 0, wantAmount: 0,
			wantAllowed: 0, wantFinal: 0, wantHasFinal: true,
			wantAction: predictioneval.ActionBelowMinimum,
		},
		{
			// MaxPoints is the single absolute cap and is applied INSIDE
			// Calculate: int(1_000_000 * 50/100) = 500_000, capped to 50_000.
			name:       "MaxPoints caps the stake inside Calculate",
			inputs:     gi(predictioneval.StrategyMostVoted, 50, 20, 50_000, 1_000_000, 0, 0, plain, nil, false),
			wantChoice: 0, wantOutcomeID: "o1", wantBaseStake: 50_000, wantAmount: 50_000,
			wantAllowed: 50_000, wantFinal: 50_000, wantHasFinal: true,
			wantAction: predictioneval.ActionWouldAttemptPlacement,
		},
		{
			// The percent gate: proposed int(10_000 * 50/100) = 5000;
			// cap = 10_000 * 1 / 100 = 100; 100 < 5000 so the gate binds.
			// The caller ADOPTS the allowance, so clampApplied is true.
			name:       "the percent gate clamps and the caller adopts the allowance",
			inputs:     gi(predictioneval.StrategyMostVoted, 50, 20, 50_000, 10_000, 1, 0, plain, nil, false),
			wantChoice: 0, wantOutcomeID: "o1", wantBaseStake: 5000, wantAmount: 5000,
			wantAllowed: 100, wantReason: predictioneval.GatePercent, wantLimit: 100,
			wantClamp: true, wantFinal: 100, wantHasFinal: true,
			wantAction: predictioneval.ActionWouldAttemptPlacement,
		},
		{
			// The reserve floor: proposed int(1000 * 50/100) = 500;
			// balance - allowed = 1000 - 500 = 500 < 600, so the bet is
			// ABANDONED rather than shrunk, and the caller returns from
			// INSIDE the gate block — so there is no post-gate stake at all.
			name:       "the reserve floor abandons the bet and produces no post-gate stake",
			inputs:     gi(predictioneval.StrategyMostVoted, 50, 20, 50_000, 1000, 0, 600, plain, nil, false),
			wantChoice: 0, wantOutcomeID: "o1", wantBaseStake: 500, wantAmount: 500,
			wantAllowed: 500, wantReason: predictioneval.GateReserveViolation, wantLimit: 600,
			wantHasFinal: false,
			wantAction:   predictioneval.ActionReserveViolation,
		},
		{
			// GateNone beside a negative proposal. int(-1000 * 5/100) = -50.
			// No percent gate runs, so the clamp-to-zero in EvaluateStake
			// changes the ALLOWANCE to 0 but leaves the reason at GateNone —
			// and because the reason is neither gate, the caller keeps its
			// own -50. The allowance and the adopted stake genuinely differ.
			name:       "a negative proposal leaves GateNone standing beside an allowance of zero",
			inputs:     gi(predictioneval.StrategyMostVoted, 5, 20, 50_000, -1000, 0, 0, plain, nil, false),
			wantChoice: 0, wantOutcomeID: "o1", wantBaseStake: -50, wantAmount: -50,
			wantAllowed: 0, wantReason: predictioneval.GateNone, wantLimit: 0,
			wantClamp: false, wantFinal: -50, wantHasFinal: true,
			wantAction: predictioneval.ActionBelowMinimum,
		},
		{
			// total_users sums the FIRST TWO outcomes: 6 + 4 = 10.
			// GT 5 holds, so the filter does NOT skip.
			name: "a total_users filter sums the first two outcomes",
			inputs: gi(predictioneval.StrategyMostVoted, 5, 20, 50_000, 1000, 0, 0, plain,
				&predictioneval.SourceFilterCondition{
					By: predictioneval.OutcomeTotalUsers, Where: predictioneval.ConditionGT, Value: 5,
				}, false),
			wantChoice: 0, wantOutcomeID: "o1", wantBaseStake: 50, wantAmount: 50,
			wantSkip: false, wantCompared: 10,
			wantAllowed: 50, wantFinal: 50, wantHasFinal: true,
			wantAction: predictioneval.ActionWouldAttemptPlacement,
		},
		{
			// decision_users REMAPS to the total_users accessor but keeps the
			// CHOSEN-OUTCOME branch: slot 0's own total_users = 6, not the
			// sum. GT 10 fails, so the filter skips.
			name: "a decision_users filter reads the chosen outcome, not the sum",
			inputs: gi(predictioneval.StrategyMostVoted, 5, 20, 50_000, 1000, 0, 0, plain,
				&predictioneval.SourceFilterCondition{
					By: predictioneval.OutcomeDecisionUsers, Where: predictioneval.ConditionGT, Value: 10,
				}, false),
			wantChoice: 0, wantOutcomeID: "o1", wantBaseStake: 50, wantAmount: 50,
			wantSkip: true, wantCompared: 6,
			wantAllowed: 50, wantFinal: 50, wantHasFinal: true,
			wantAction: predictioneval.ActionFilterRejected,
		},
		{
			// An operator outside the closed set falls through the switch,
			// which has no default, so the policy SKIPS. A value the store
			// recorded as UNKNOWN behaves identically, which is why an
			// UNKNOWN comparison is replayable exactly.
			name: "an unrecognised comparison operator skips",
			inputs: gi(predictioneval.StrategyMostVoted, 5, 20, 50_000, 1000, 0, 0, plain,
				&predictioneval.SourceFilterCondition{
					By: predictioneval.OutcomeTotalUsers, Where: predictioneval.ValueUnknown, Value: 5,
				}, false),
			wantChoice: 0, wantOutcomeID: "o1", wantBaseStake: 50, wantAmount: 50,
			wantSkip: true, wantCompared: 10,
			wantAllowed: 50, wantFinal: 50, wantHasFinal: true,
			wantAction: predictioneval.ActionFilterRejected,
		},
		{
			// An unrecognised OUTCOME KEY takes the chosen-outcome branch and
			// reads 0 through getOutcomeValue's default. LTE 0 then holds, so
			// the filter does not skip.
			name: "an unrecognised filter key compares zero through the default branch",
			inputs: gi(predictioneval.StrategyMostVoted, 5, 20, 50_000, 1000, 0, 0, plain,
				&predictioneval.SourceFilterCondition{
					By: predictioneval.ValueUnknown, Where: predictioneval.ConditionLTE, Value: 0,
				}, false),
			wantChoice: 0, wantOutcomeID: "o1", wantBaseStake: 50, wantAmount: 50,
			wantSkip: false, wantCompared: 0,
			wantAllowed: 50, wantFinal: 50, wantHasFinal: true,
			wantAction: predictioneval.ActionWouldAttemptPlacement,
		},
		{
			// An unrecognised STRATEGY leaves the choice at -1 (the pinned
			// switch has no default). With no filter there is no panic, and
			// the amount is the initialized 0.
			name:       "an unrecognised strategy chooses nothing",
			inputs:     gi(predictioneval.ValueUnknown, 5, 20, 50_000, 1000, 0, 0, plain, nil, false),
			wantChoice: -1, wantOutcomeID: "", wantBaseStake: 0, wantAmount: 0,
			wantAllowed: 0, wantFinal: 0, wantHasFinal: true,
			wantAction: predictioneval.ActionBelowMinimum,
		},
		{
			// ORDERING. Skip is CALLED right after Calculate but ACTED ON after
			// the minimum-stake exit. int(100 * 1/100) = 1, which is below the
			// pinned floor of 10, and the filter ALSO rejects. The pinned
			// caller reports BELOW_MINIMUM_POINTS; a model that acted on the
			// filter where it computed it would report FILTER_REJECTED.
			name: "a stake below the minimum that ALSO fails the filter reports the minimum exit",
			inputs: gi(predictioneval.StrategyMostVoted, 1, 20, 50_000, 100, 0, 0, plain,
				&predictioneval.SourceFilterCondition{
					By: predictioneval.OutcomeTotalUsers, Where: predictioneval.ConditionGT, Value: 1e9,
				}, false),
			wantChoice: 0, wantOutcomeID: "o1", wantBaseStake: 1, wantAmount: 1,
			wantSkip: true, wantCompared: 10,
			wantAllowed: 1, wantFinal: 1, wantHasFinal: true,
			wantAction: predictioneval.ActionBelowMinimum,
		},
		{
			// Stealth: base = int(2000 * 50/100) = 1000, topPoints 900, and
			// 1000 >= 900 so the branch fires. The recorded realization 897
			// is topPoints - 3, a legal reduction, so the stage is
			// CONDITIONED and the stake is 897.
			name: "a stealth stake is reconstructed from a legal recorded reduction",
			inputs: gi(predictioneval.StrategySmartMoney, 50, 20, 50_000, 2000, 0, 0,
				[]predictioneval.OutcomeInput{
					go1(0, "s1", 9, 900, 900, 60, 1.66, 60.24),
					go1(1, "s2", 1, 100, 10, 40, 2.5, 40),
				}, nil, true),
			observed:   predictioneval.ObservedRealization{StealthAmount: int64Ptr(897)},
			wantChoice: 0, wantOutcomeID: "s1", wantBaseStake: 1000, wantAmount: 897,
			wantAllowed: 897, wantFinal: 897, wantHasFinal: true,
			wantAction: predictioneval.ActionWouldAttemptPlacement,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := predictioneval.Evaluate(tc.inputs, tc.observed)

			if ev.Choice.Index != tc.wantChoice {
				t.Errorf("choice = %d, want %d", ev.Choice.Index, tc.wantChoice)
			}
			if ev.Choice.OutcomeID != tc.wantOutcomeID {
				t.Errorf("outcome id = %q, want %q", ev.Choice.OutcomeID, tc.wantOutcomeID)
			}
			if ev.BaseStake.State == predictioneval.StageStateExecuted &&
				ev.BaseStake.Amount != tc.wantBaseStake {
				t.Errorf("base stake = %d, want %d", ev.BaseStake.Amount, tc.wantBaseStake)
			}
			if ev.PolicyAmount != tc.wantAmount {
				t.Errorf("policy amount = %d, want %d", ev.PolicyAmount, tc.wantAmount)
			}
			if ev.Filter.Skip != tc.wantSkip {
				t.Errorf("skip = %v, want %v", ev.Filter.Skip, tc.wantSkip)
			}
			if ev.Filter.Compared != tc.wantCompared {
				t.Errorf("compared = %v, want %v", ev.Filter.Compared, tc.wantCompared)
			}
			if ev.StakeGate.Allowed != tc.wantAllowed {
				t.Errorf("stake allowed = %d, want %d", ev.StakeGate.Allowed, tc.wantAllowed)
			}
			if ev.StakeGate.Reason != tc.wantReason {
				t.Errorf("stake reason = %q, want %q", ev.StakeGate.Reason, tc.wantReason)
			}
			if ev.StakeGate.Limit != tc.wantLimit {
				t.Errorf("stake limit = %d, want %d", ev.StakeGate.Limit, tc.wantLimit)
			}
			if ev.Clamp.Applied != tc.wantClamp {
				t.Errorf("clamp applied = %v, want %v", ev.Clamp.Applied, tc.wantClamp)
			}
			if ev.Clamp.HasFinal != tc.wantHasFinal {
				t.Errorf("post-gate stake present = %v, want %v", ev.Clamp.HasFinal, tc.wantHasFinal)
			}
			if tc.wantHasFinal && ev.Clamp.FinalAmount != tc.wantFinal {
				t.Errorf("final amount = %d, want %d", ev.Clamp.FinalAmount, tc.wantFinal)
			}
			if ev.Action != tc.wantAction {
				t.Errorf("action = %q, want %q", ev.Action, tc.wantAction)
			}
		})
	}
}

// TestAnEvaluationIsDeterministicAndSerializable backs the "deterministic,
// versioned, machine-readable" claim with an actual check.
//
// A result that is not reproducible cannot be an audit artefact, and one that
// cannot be serialized cannot leave the process.
func TestAnEvaluationIsDeterministicAndSerializable(t *testing.T) {
	in := gi(predictioneval.StrategySmart, 5, 20, 50_000, 1000, 10, 100,
		[]predictioneval.OutcomeInput{
			go1(0, "o1", 6, 600, 90, 60, 1.66, 60.24),
			go1(1, "o2", 4, 400, 80, 40, 2.5, 40),
		},
		&predictioneval.SourceFilterCondition{
			By: predictioneval.OutcomeOdds, Where: predictioneval.ConditionGTE, Value: 1.5,
		}, false)
	in.CommonInputDigest = "deadbeef"

	first, err := json.Marshal(predictioneval.Evaluate(in, predictioneval.ObservedRealization{}))
	if err != nil {
		t.Fatalf("an evaluation could not be marshalled: %v", err)
	}
	for i := 0; i < 50; i++ {
		again, err := json.Marshal(predictioneval.Evaluate(in, predictioneval.ObservedRealization{}))
		if err != nil {
			t.Fatalf("run %d could not be marshalled: %v", i, err)
		}
		if string(again) != string(first) {
			t.Fatalf("run %d differed from run 0:\n first %s\n again %s", i, first, again)
		}
	}

	var decoded map[string]any
	if err := json.Unmarshal(first, &decoded); err != nil {
		t.Fatalf("the serialized evaluation does not decode: %v", err)
	}
	model, ok := decoded["model"].(map[string]any)
	if !ok {
		t.Fatal("the evaluation carries no model provenance")
	}
	for key, want := range map[string]string{
		"modelVersion":              predictioneval.ModelVersion,
		"policyRevision":            predictioneval.PolicyRevision,
		"supportedProducerRevision": predictioneval.SupportedProducerRevision,
	} {
		if got, _ := model[key].(string); got != want {
			t.Errorf("model.%s = %q, want %q", key, got, want)
		}
	}
	if decoded["commonInputDigest"] != "deadbeef" {
		t.Errorf("the evaluation lost its common-input provenance: %v", decoded["commonInputDigest"])
	}
}
