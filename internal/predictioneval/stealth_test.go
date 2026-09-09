package predictioneval_test

// STEALTH: THE ONE STAGE THAT CANNOT BE PROVEN.
//
// When stealth mode reduces a stake the pinned policy consumes a draw from the
// global RNG. No seed was recorded and none can be invented, so an offline
// replay CANNOT independently reproduce the resulting stake. The honest
// alternative — and the one this package implements — is to establish the
// choice, the base stake and whether stealth applies WITHOUT looking at
// anything observed, and only then use the recorded pre-risk stake to pin down
// which of the four legal integer reductions actually happened.
//
// That makes the stage as-operated reconstruction rather than proof, and this
// file exists to keep that distinction real:
//
//   - the observed value must not be able to influence any case where stealth
//     does not apply (otherwise the "conditioning" is just a leak);
//   - the conditioning must actually reproduce REAL draws taken by the REAL
//     policy;
//   - an unproven realization must stay unproven rather than being guessed;
//   - and the scorecard must not count the circular comparison as evidence.

import (
	"math/rand"
	"reflect"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
)

// TestObservedRealizationCannotInfluenceANonStealthCase is the leak falsifier.
//
// [predictioneval.Evaluate] is handed an [predictioneval.ObservedRealization]
// on every call, which is precisely the shape a leak takes: a recorded result
// sitting in scope where an input belongs. If any stage other than the stealth
// stage ever consulted it, the evaluation would change as the observed value
// changed — so this drives a large domain of non-stealth cases with wildly
// different observed values and requires the result to be byte-identical.
func TestObservedRealizationCannotInfluenceANonStealthCase(t *testing.T) {
	rng := rand.New(rand.NewSource(0x4E4F4C45414B)) // "NOLEAK"

	observations := []predictioneval.ObservedRealization{
		{},
		{StealthAmount: int64Ptr(0)},
		{StealthAmount: int64Ptr(1)},
		{StealthAmount: int64Ptr(-99999)},
		{StealthAmount: int64Ptr(123456789)},
		{StealthAmount: int64Ptr(1 << 62)},
	}

	checked := 0
	for i := 0; i < 3000; i++ {
		spec := randomNonStealthSpec(rng, i)
		in := replayInputs(spec)

		base := predictioneval.Evaluate(in, observations[0])
		// A case that legitimately cannot run tells us nothing about leaking.
		if base.Action == predictioneval.ActionUnsupported {
			continue
		}
		// Guard the premise: this domain must genuinely never apply stealth.
		if base.Stealth.Applies {
			t.Fatalf("case %d applied stealth; this test's domain is supposed to exclude it", i)
		}
		checked++

		for _, obs := range observations[1:] {
			got := predictioneval.Evaluate(in, obs)
			if !reflect.DeepEqual(base, got) {
				t.Fatalf("case %d: the observed realization changed a NON-STEALTH evaluation.\n"+
					"That is a causal leak: a recorded result reached a stage that must be "+
					"computed from inputs alone.\nwithout=%+v\nwith=%+v", i, base, got)
			}
		}
	}
	if checked < 1000 {
		t.Fatalf("only %d cases were actually evaluated; the leak test is close to vacuous", checked)
	}
	t.Logf("no leak across %d non-stealth evaluations x %d observed values", checked, len(observations)-1)
}

// TestConditioningReproducesRealStealthDrawsTakenByTheRealPolicy proves the
// conditioning against actual randomness rather than against an assumption
// about it.
//
// It runs the REAL models.Bet.Calculate with stealth mode on — consuming the
// REAL global RNG, exactly as production does — and feeds each resulting stake
// back as the observed realization. The replay must reconstruct that stake,
// derive a reduction inside the policy's only possible range, and label the
// stage as conditioned rather than proven.
func TestConditioningReproducesRealStealthDrawsTakenByTheRealPolicy(t *testing.T) {
	settings := models.BetSettings{
		Strategy:      models.StrategyMostVoted,
		Percentage:    50,
		PercentageGap: 20,
		MaxPoints:     50_000,
		StealthMode:   true,
	}
	// TopPoints (900) is below the 50%-of-balance base stake (1000), so the
	// policy's stealth branch is guaranteed to fire.
	outcomes := []*models.Outcome{
		mkOutcome("o1", 9, 900, 900, 60, 1.66, 60.24),
		mkOutcome("o2", 1, 100, 10, 40, 2.5, 40),
	}
	const balance = 2000

	seenReductions := map[int]int{}
	for i := 0; i < 400; i++ {
		bet := &models.Bet{Outcomes: cloneOutcomes(outcomes), Settings: settings}
		decision := bet.Calculate(balance)

		if decision.Choice != 0 {
			t.Fatalf("precondition failed: chose %d, want slot 0", decision.Choice)
		}
		// The realized stake must be topPoints minus 1..4. If the pinned policy
		// ever changed its draw, this would fail here first.
		reduction := 900 - decision.Amount
		if reduction < 1 || reduction > 4 {
			t.Fatalf("the real policy produced a reduction of %d, outside the modelled 1..4 range; "+
				"the replay's stealth model is now wrong", reduction)
		}
		seenReductions[reduction]++

		spec := caseSpec{
			settings: settings, outcomes: cloneOutcomes(outcomes),
			balance: balance, maxPct: 0, reserve: 0,
			health: predictioneval.HealthAllowed,
		}
		ev := predictioneval.Evaluate(replayInputs(spec), predictioneval.ObservedRealization{
			StealthAmount: int64Ptr(int64(decision.Amount)),
		})

		if ev.Stealth.Outcome != predictioneval.StealthConditionedOnObservedRealization {
			t.Fatalf("stealth outcome = %q, want %q",
				ev.Stealth.Outcome, predictioneval.StealthConditionedOnObservedRealization)
		}
		if !ev.Stealth.Applies {
			t.Fatal("the replay did not establish that stealth applies")
		}
		// The base stake is established INDEPENDENTLY, before any observed
		// value is read: 50% of 2000, uncapped.
		if ev.BaseStake.Amount != 1000 {
			t.Fatalf("base stake = %d, want the independently derived 1000", ev.BaseStake.Amount)
		}
		if ev.Stealth.Realized != decision.Amount {
			t.Fatalf("realized = %d, real policy produced %d", ev.Stealth.Realized, decision.Amount)
		}
		if ev.Stealth.Reduction != reduction || !ev.Stealth.ReductionDerived {
			t.Fatalf("reduction = %d (derived=%v), want %d",
				ev.Stealth.Reduction, ev.Stealth.ReductionDerived, reduction)
		}
		if !containsString(ev.Limitations, predictioneval.LimitationStealthConditioned) {
			t.Fatalf("a conditioned evaluation did not carry %q in its limitations: %v",
				predictioneval.LimitationStealthConditioned, ev.Limitations)
		}
	}
	// The real RNG should have produced more than one reduction over 400 runs;
	// if it produced only one, this test is much weaker than it looks.
	if len(seenReductions) < 2 {
		t.Errorf("only reduction(s) %v occurred across 400 real draws; the conditioning is "+
			"barely exercised", seenReductions)
	}
	t.Logf("reconstructed %d real stealth draws; reduction histogram = %v", 400, seenReductions)
}

// TestAnUnprovenStealthRealizationStaysUnproven requires the model to refuse a
// stake it cannot pin down, rather than substituting the base stake.
//
// Substituting would be the worst available failure: it produces a plausible
// number, downstream stages accept it, and the scorecard reports agreement for
// a stake the miner never used.
func TestAnUnprovenStealthRealizationStaysUnproven(t *testing.T) {
	settings := models.BetSettings{
		Strategy: models.StrategyMostVoted, Percentage: 50, PercentageGap: 20,
		MaxPoints: 50_000, StealthMode: true,
	}
	outcomes := []*models.Outcome{
		mkOutcome("o1", 9, 900, 900, 60, 1.66, 60.24),
		mkOutcome("o2", 1, 100, 10, 40, 2.5, 40),
	}
	spec := caseSpec{
		settings: settings, outcomes: outcomes, balance: 2000,
		health: predictioneval.HealthAllowed,
	}

	for _, tc := range []struct {
		name string
		obs  predictioneval.ObservedRealization
	}{
		{"no realization recorded at all", predictioneval.ObservedRealization{}},
		{"a reduction of 0 is not a legal draw", predictioneval.ObservedRealization{StealthAmount: int64Ptr(900)}},
		{"a reduction of 5 is outside the draw's range", predictioneval.ObservedRealization{StealthAmount: int64Ptr(895)}},
		{"a realization above topPoints is not reachable", predictioneval.ObservedRealization{StealthAmount: int64Ptr(1200)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := predictioneval.Evaluate(replayInputs(spec), tc.obs)

			if ev.Stealth.Outcome != predictioneval.StealthUnproven {
				t.Fatalf("stealth outcome = %q, want UNPROVEN", ev.Stealth.Outcome)
			}
			if ev.PolicyAmountKnown {
				t.Fatalf("the model claimed to know the stake (%d) despite an unproven realization",
					ev.PolicyAmount)
			}
			// The base stake is still independently known — that part never
			// depended on the draw — but nothing downstream may proceed on it.
			if ev.BaseStake.Amount != 1000 {
				t.Fatalf("base stake = %d, want 1000", ev.BaseStake.Amount)
			}
			if ev.Action != predictioneval.ActionIndeterminate {
				t.Fatalf("action = %q, want INDETERMINATE", ev.Action)
			}
			if ev.StakeGate.State != predictioneval.StageStateIndeterminate {
				t.Fatalf("stake gate = %q, want INDETERMINATE", ev.StakeGate.State)
			}
			if ev.Clamp.HasFinal {
				t.Fatal("a final stake was produced from an unproven realization")
			}
		})
	}
}

// TestScoreCountsAConditionedStealthComparisonApartFromIndependentEvidence is
// the anti-self-congratulation check.
//
// Under stealth the replay's stake IS the recorded stake, so comparing them
// always agrees. Counting that as independent evidence would let a scorecard
// claim a verified replay on the strength of a tautology.
func TestScoreCountsAConditionedStealthComparisonApartFromIndependentEvidence(t *testing.T) {
	settings := models.BetSettings{
		Strategy: models.StrategyMostVoted, Percentage: 50, PercentageGap: 20,
		MaxPoints: 50_000, StealthMode: true,
	}
	outcomes := []*models.Outcome{
		mkOutcome("o1", 9, 900, 900, 60, 1.66, 60.24),
		mkOutcome("o2", 1, 100, 10, 40, 2.5, 40),
	}
	spec := caseSpec{
		settings: settings, outcomes: outcomes, balance: 2000,
		health: predictioneval.HealthAllowed,
	}
	const realized = 897 // topPoints 900 minus a legal reduction of 3

	ev := predictioneval.Evaluate(replayInputs(spec), predictioneval.ObservedRealization{
		StealthAmount: int64Ptr(realized),
	})
	if ev.Stealth.Outcome != predictioneval.StealthConditionedOnObservedRealization {
		t.Fatalf("precondition failed: stealth outcome = %q", ev.Stealth.Outcome)
	}

	c := predictioneval.DecisionCase{
		Eligibility: predictioneval.CaseEligibility{Eligible: true, ExercisesPolicy: true},
		Recorded: predictioneval.RecordedResults{
			ChoiceIndex: 0, ChoiceIndexRecorded: true,
			ChoiceOutcomeID:      "o1",
			ChoiceAmount:         realized,
			ChoiceAmountRecorded: true,
			SkipResult:           false, SkipResultRecorded: true,
			StakeAllowed: realized, StakeAllowedRecorded: true,
			StakeReason: "", StakeLimit: 0, StakeLimitRecorded: true,
			ClampApplied: false, ClampAppliedRecorded: true,
			FinalAmount: realized, FinalAmountRecorded: true,
			TerminalReason: "OK",
			HealthStage:    predictioneval.HealthAllowed,
		},
	}
	sc := predictioneval.Score(c, ev, predictioneval.SettlementFacts{})

	var choiceAmount predictioneval.Comparison
	found := false
	for _, cmp := range sc.Comparisons {
		if cmp.Field == "choiceAmount" {
			choiceAmount, found = cmp, true
		}
	}
	if !found {
		t.Fatal("no choiceAmount comparison was produced")
	}
	if choiceAmount.Basis != predictioneval.BasisCircular {
		t.Fatalf("choiceAmount basis = %q, want CIRCULAR: it is compared against the very value "+
			"the stealth stage was conditioned on", choiceAmount.Basis)
	}
	if sc.CircularComparisons == 0 {
		t.Fatal("the circular comparison was not counted as circular")
	}

	// The stake gate and clamp inherit the conditioned quantity, so they are
	// consistency, not proof.
	for _, field := range []string{"stakeAllowed", "finalAmount"} {
		cmp := findComparison(t, sc, field)
		if cmp.Basis != predictioneval.BasisConditioned {
			t.Errorf("%s basis = %q, want CONDITIONED", field, cmp.Basis)
		}
	}
	// The choice and the filter never depended on the draw, so they remain
	// genuine independent evidence.
	for _, field := range []string{"choiceIndex", "choiceOutcomeId", "skipResult"} {
		cmp := findComparison(t, sc, field)
		if cmp.Basis != predictioneval.BasisIndependent {
			t.Errorf("%s basis = %q, want INDEPENDENT: it is a function of the outcome vector "+
				"and the strategy, never of the stake", field, cmp.Basis)
		}
	}
	if sc.IndependentAgree == 0 {
		t.Fatal("no independent evidence at all was recorded for a case that has some")
	}
	// The settlement of a conditioned replay is conditional, never asserted.
	sc2 := predictioneval.Score(c, ev, predictioneval.SettlementFacts{
		PlacementCallStarted: true, PlacementCallReturned: true, PlacementAccepted: true,
	})
	if ev.Action == predictioneval.ActionWouldAttemptPlacement &&
		sc2.Settlement.Assessment != predictioneval.SettlementConditional {
		t.Fatalf("settlement assessment = %q, want CONDITIONAL_ON_STEALTH_REALIZATION",
			sc2.Settlement.Assessment)
	}
	if sc2.Settlement.ROI != predictioneval.SettlementUnknown {
		t.Fatalf("ROI = %q, want UNKNOWN: this package does not compute profitability",
			sc2.Settlement.ROI)
	}
}

// randomNonStealthSpec builds a case guaranteed not to apply stealth: either
// stealth mode is off, or the chosen outcome's top stake is far above any
// stake the settings can produce.
func randomNonStealthSpec(rng *rand.Rand, i int) caseSpec {
	strategies := []models.Strategy{
		models.StrategyMostVoted, models.StrategyHighOdds, models.StrategyPercentage,
		models.StrategySmartMoney, models.StrategySmart, models.StrategyNumber1,
		models.StrategyNumber3, models.Strategy("UNKNOWN"),
	}
	n := 2 + rng.Intn(3)
	outs := make([]*models.Outcome, 0, n)
	for j := 0; j < n; j++ {
		outs = append(outs, mkOutcome(
			"o"+itoaTest(j), rng.Intn(50), rng.Intn(5000),
			// A top stake no percentage of the balances below can reach, so
			// the stealth branch never fires even when the mode is on.
			1_000_000_000+rng.Intn(1000),
			roundTo2(rng.Float64()*100), roundTo2(rng.Float64()*10), roundTo2(rng.Float64()*100),
		))
	}
	s := models.BetSettings{
		Strategy:      strategies[rng.Intn(len(strategies))],
		Percentage:    []int{0, 1, 5, 50}[rng.Intn(4)],
		PercentageGap: rng.Intn(60),
		MaxPoints:     []int{10, 1000, 50_000}[rng.Intn(3)],
		StealthMode:   rng.Intn(2) == 0,
	}
	if rng.Intn(2) == 0 {
		s.FilterCondition = &models.FilterCondition{
			By: []models.OutcomeKey{
				models.OutcomeTotalUsers, models.OutcomePercentageUsers, models.OutcomeDecisionPoints,
			}[rng.Intn(3)],
			Where: []models.Condition{models.ConditionGT, models.ConditionLTE}[rng.Intn(2)],
			Value: roundTo2(rng.Float64() * 300),
		}
	}
	return caseSpec{
		name:     "nonstealth/" + itoaTest(i),
		settings: s,
		outcomes: outs,
		balance:  []int{0, 100, 1000, 50_000}[rng.Intn(4)],
		maxPct:   []int{0, 10, 100}[rng.Intn(3)],
		reserve:  []int{0, 500}[rng.Intn(2)],
		health: []string{
			predictioneval.HealthAllowed, predictioneval.HealthDisabled, predictioneval.HealthNoGate,
		}[rng.Intn(3)],
	}
}

func findComparison(t *testing.T, sc predictioneval.Scorecard, field string) predictioneval.Comparison {
	t.Helper()
	for _, cmp := range sc.Comparisons {
		if cmp.Field == field {
			return cmp
		}
	}
	t.Fatalf("no %q comparison was produced", field)
	return predictioneval.Comparison{}
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func int64Ptr(v int64) *int64 { return &v }
