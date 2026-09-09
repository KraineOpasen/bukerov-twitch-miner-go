package predictioneval_test

// PARITY AGAINST THE REAL POLICY.
//
// internal/predictioneval re-implements the decision arithmetic of
// internal/models. A second copy of an algorithm proves nothing about itself,
// so this file does not let it try: every case here drives the REAL
// models.Bet.Calculate, models.Bet.Skip and models.EvaluateStake through the
// REAL caller ordering, and requires the replay to agree field for field.
//
// The reference caller below mirrors the pinned auto-bet path in
// internal/pubsub/pool.go (placeAutoBetScheduled). Only the ORDER is
// reproduced here; every value it computes comes out of the production models
// package, so a divergence in the replay's arithmetic cannot hide.
//
// The oracle is BIDIRECTIONAL, which is what makes the legacy-failure claim
// falsifiable rather than decorative: the reference is run under recover(), and
// the replay must report LEGACY_FAILURE exactly when — and only when — the real
// policy panics. A replay that never reported a legacy failure, and a replay
// that reported one spuriously, both fail here.

import (
	"math"
	"math/rand"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
)

// referenceResult is what the pinned production path produces for one case.
type referenceResult struct {
	choice       int
	outcomeID    string
	amount       int
	skip         bool
	compared     float64
	stakeAllowed int
	stakeReason  string
	stakeLimit   int
	clamp        bool
	finalAmount  int
	hasFinal     bool
	action       string
}

// caseSpec is one generated decision.
type caseSpec struct {
	name     string
	settings models.BetSettings
	outcomes []*models.Outcome
	balance  int
	maxPct   int
	reserve  int
	health   string
}

// runReference drives the REAL models package through the REAL caller ordering.
//
// It returns panicked=true when the pinned policy panics, which is a genuine
// production behaviour on some inputs and is reproduced as evidence rather than
// papered over.
func runReference(spec caseSpec) (res referenceResult, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
		}
	}()

	bet := &models.Bet{Outcomes: spec.outcomes, Settings: spec.settings}

	// The pinned order: Calculate, then Skip, then the gates. Skip is CALLED
	// here and ACTED ON last; a replay that acted on it early would report
	// FILTER_REJECTED where the miner reported BELOW_MINIMUM_POINTS.
	decision := bet.Calculate(spec.balance)
	skip, compared := bet.Skip()

	res.choice = decision.Choice
	res.outcomeID = decision.ID
	res.amount = decision.Amount
	res.skip = skip
	res.compared = compared

	if spec.health == predictioneval.HealthDenied {
		res.action = predictioneval.ActionHealthGated
		return res, false
	}

	allowed, reason, limit := models.EvaluateStake(decision.Amount, spec.balance, spec.maxPct, spec.reserve)
	res.stakeAllowed, res.stakeReason, res.stakeLimit = allowed, string(reason), limit

	switch reason {
	case models.GateReserveViolation:
		// Returns from INSIDE the gate block: no post-gate stake exists.
		res.action = predictioneval.ActionReserveViolation
		return res, false
	case models.GatePercent:
		decision.Amount = allowed
		res.clamp = true
	}
	res.finalAmount, res.hasFinal = decision.Amount, true

	if decision.Amount < 10 { // internal/pubsub/pool.go: minPredictionBet
		res.action = predictioneval.ActionBelowMinimum
		return res, false
	}
	if skip {
		res.action = predictioneval.ActionFilterRejected
		return res, false
	}
	res.action = predictioneval.ActionWouldAttemptPlacement
	return res, false
}

// replayInputs converts a case into the replay's input value. Nothing recorded
// is carried across: the replay gets only what the decision READ.
func replayInputs(spec caseSpec) predictioneval.DecisionInputs {
	in := predictioneval.DecisionInputs{
		ReachedDecision:     true,
		Balance:             spec.balance,
		BalancePresent:      true,
		OutcomesPresent:     true,
		RiskPresent:         true,
		RiskMaxStakePercent: spec.maxPct,
		RiskReservePoints:   spec.reserve,
		HealthState:         spec.health,
		MinimumStake:        predictioneval.PinnedMinimumStake,
		Settings: &predictioneval.BetSettingsInput{
			Strategy:      string(spec.settings.Strategy),
			Percentage:    spec.settings.Percentage,
			PercentageGap: spec.settings.PercentageGap,
			MaxPoints:     spec.settings.MaxPoints,
			MinimumPoints: spec.settings.MinimumPoints,
			StealthMode:   spec.settings.StealthMode,
			Delay:         spec.settings.Delay,
			DelayMode:     string(spec.settings.DelayMode),
		},
	}
	if fc := spec.settings.FilterCondition; fc != nil {
		in.Settings.FilterCondition = &predictioneval.SourceFilterCondition{
			By: string(fc.By), Where: string(fc.Where), Value: fc.Value,
		}
	}
	for i, o := range spec.outcomes {
		if o == nil {
			in.Outcomes = append(in.Outcomes, predictioneval.OutcomeInput{Slot: i})
			continue
		}
		in.Outcomes = append(in.Outcomes, predictioneval.OutcomeInput{
			Slot: i, Present: true, ID: o.ID,
			TotalUsers: o.TotalUsers, TotalPoints: o.TotalPoints, TopPoints: o.TopPoints,
			PercentageUsers: o.PercentageUsers, Odds: o.Odds, OddsPercentage: o.OddsPercentage,
		})
	}
	return in
}

// assertParity requires the replay to reproduce the reference exactly.
func assertParity(t *testing.T, spec caseSpec, ref referenceResult, ev predictioneval.Evaluation) {
	t.Helper()

	if ev.Choice.Index != ref.choice {
		t.Errorf("%s: choice = %d, real policy chose %d", spec.name, ev.Choice.Index, ref.choice)
	}
	if ev.Choice.OutcomeID != ref.outcomeID {
		t.Errorf("%s: outcome id = %q, real policy chose %q", spec.name, ev.Choice.OutcomeID, ref.outcomeID)
	}
	if !ev.PolicyAmountKnown || ev.PolicyAmount != ref.amount {
		t.Errorf("%s: policy amount = %d (known=%v), real policy proposed %d",
			spec.name, ev.PolicyAmount, ev.PolicyAmountKnown, ref.amount)
	}
	if ev.Filter.State != predictioneval.StageStateExecuted {
		t.Fatalf("%s: filter stage = %q, want EXECUTED", spec.name, ev.Filter.State)
	}
	if ev.Filter.Skip != ref.skip {
		t.Errorf("%s: skip = %v, real policy said %v", spec.name, ev.Filter.Skip, ref.skip)
	}
	// Bit equality, not a tolerance: both sides ran the same float64
	// arithmetic, so anything but an exact match is real drift.
	if math.Float64bits(ev.Filter.Compared) != math.Float64bits(ref.compared) {
		t.Errorf("%s: compared = %v, real policy computed %v", spec.name, ev.Filter.Compared, ref.compared)
	}
	if ev.Action != ref.action {
		t.Errorf("%s: action = %q, real path took %q", spec.name, ev.Action, ref.action)
	}

	if ref.action == predictioneval.ActionHealthGated {
		if ev.StakeGate.State != predictioneval.StageStateNotReached {
			t.Errorf("%s: health-gated case reached the stake gate (%q)", spec.name, ev.StakeGate.State)
		}
		return
	}

	if ev.StakeGate.State != predictioneval.StageStateExecuted {
		t.Fatalf("%s: stake gate = %q, want EXECUTED", spec.name, ev.StakeGate.State)
	}
	if ev.StakeGate.Allowed != ref.stakeAllowed {
		t.Errorf("%s: stake allowed = %d, real gate returned %d", spec.name, ev.StakeGate.Allowed, ref.stakeAllowed)
	}
	if ev.StakeGate.Reason != ref.stakeReason {
		t.Errorf("%s: stake reason = %q, real gate returned %q", spec.name, ev.StakeGate.Reason, ref.stakeReason)
	}
	if ev.StakeGate.Limit != ref.stakeLimit {
		t.Errorf("%s: stake limit = %d, real gate returned %d", spec.name, ev.StakeGate.Limit, ref.stakeLimit)
	}
	if ev.Clamp.HasFinal != ref.hasFinal {
		t.Errorf("%s: post-gate stake present = %v, real path %v", spec.name, ev.Clamp.HasFinal, ref.hasFinal)
	}
	if ref.hasFinal {
		if ev.Clamp.Applied != ref.clamp {
			t.Errorf("%s: clamp applied = %v, real path %v", spec.name, ev.Clamp.Applied, ref.clamp)
		}
		if ev.Clamp.FinalAmount != ref.finalAmount {
			t.Errorf("%s: final amount = %d, real path carried %d", spec.name, ev.Clamp.FinalAmount, ref.finalAmount)
		}
	}
}

// generateCases builds a deterministic, adversarial domain.
//
// The seed is a constant so a failure is always reproducible; nothing here
// consults the clock or the global RNG.
func generateCases(t *testing.T) []caseSpec {
	t.Helper()
	rng := rand.New(rand.NewSource(0x50325245504C4159)) // "P2REPLAY"

	strategies := []models.Strategy{
		models.StrategyMostVoted, models.StrategyHighOdds, models.StrategyPercentage,
		models.StrategySmartMoney, models.StrategySmart,
		models.StrategyNumber1, models.StrategyNumber2, models.StrategyNumber3, models.StrategyNumber4,
		models.StrategyNumber5, models.StrategyNumber6, models.StrategyNumber7, models.StrategyNumber8,
		// Deliberately outside the closed set: the pinned switch has no
		// default, so these must leave the choice at -1. This is also how a
		// value the store recorded as UNKNOWN behaves.
		models.Strategy("UNKNOWN"), models.Strategy(""), models.Strategy("smart"),
	}
	keys := []models.OutcomeKey{
		models.OutcomePercentageUsers, models.OutcomeOddsPercentage, models.OutcomeOdds,
		models.OutcomeTopPoints, models.OutcomeTotalUsers, models.OutcomeTotalPoints,
		models.OutcomeDecisionUsers, models.OutcomeDecisionPoints,
		models.OutcomeKey("UNKNOWN"),
	}
	conditions := []models.Condition{
		models.ConditionGT, models.ConditionLT, models.ConditionGTE, models.ConditionLTE,
		models.Condition("UNKNOWN"),
	}
	healths := []string{
		predictioneval.HealthAllowed, predictioneval.HealthDisabled,
		predictioneval.HealthNoGate, predictioneval.HealthDenied,
	}
	balances := []int{0, -1, -5000, 7, 199, 1000, 50_000, 1_000_000}
	percentages := []int{0, 1, 5, 50, 100, 200}
	maxPoints := []int{0, 9, 10, 1000, 50_000, math.MaxInt32}
	maxPcts := []int{0, 1, 10, 50, 100}
	reserves := []int{0, 1, 500, 1_000_000}

	var out []caseSpec
	add := func(s caseSpec) { out = append(out, s) }

	// Hand-built boundary vectors: exact ties, an exact gap boundary, and the
	// short vectors that make SMART and the NUMBER strategies behave
	// differently from one another.
	vectors := map[string][]*models.Outcome{
		"empty":  {},
		"single": {mkOutcome("o1", 3, 300, 50, 60, 1.66, 60.24)},
		// Exact tie on every selectable key: the policy's STRICT > must keep
		// slot 0. A replay using >= would pick slot 1 and this catches it.
		"exact-tie": {
			mkOutcome("o1", 5, 500, 70, 50, 2.0, 50),
			mkOutcome("o2", 5, 500, 70, 50, 2.0, 50),
		},
		// difference == PercentageGap exactly: SMART's STRICT < must take the
		// total-users branch.
		"gap-boundary": {
			mkOutcome("o1", 6, 600, 90, 60, 1.66, 60.24),
			mkOutcome("o2", 4, 400, 80, 40, 2.5, 40),
		},
		"three": {
			mkOutcome("o1", 1, 100, 10, 10, 10.0, 10),
			mkOutcome("o2", 8, 800, 400, 80, 1.25, 80),
			mkOutcome("o3", 1, 100, 900, 10, 10.0, 10),
		},
		// A hole in the vector. The pinned policy dereferences it; the replay
		// must say so rather than reading it as a zero-point outcome.
		"hole": {nil, mkOutcome("o2", 4, 400, 80, 40, 2.5, 40)},
		"hole-second": {
			mkOutcome("o1", 6, 600, 90, 60, 1.66, 60.24), nil,
		},
	}
	vectorNames := []string{"empty", "single", "exact-tie", "gap-boundary", "three", "hole", "hole-second"}

	// Exhaustive over the structural axes that interact: strategy x vector x
	// filter key x condition. These are the combinations that decide which
	// branch of Calculate and Skip runs.
	for _, st := range strategies {
		for _, vn := range vectorNames {
			for _, key := range keys {
				for _, cond := range conditions {
					s := models.BetSettings{
						Strategy:      st,
						Percentage:    percentages[rng.Intn(len(percentages))],
						PercentageGap: 20,
						MaxPoints:     maxPoints[rng.Intn(len(maxPoints))],
						StealthMode:   false, // stealth is proven separately; it is not deterministic
						FilterCondition: &models.FilterCondition{
							By: key, Where: cond, Value: float64(rng.Intn(200)),
						},
					}
					add(caseSpec{
						name:     "strategy=" + string(st) + "/vec=" + vn + "/by=" + string(key) + "/where=" + string(cond),
						settings: s,
						outcomes: cloneOutcomes(vectors[vn]),
						balance:  balances[rng.Intn(len(balances))],
						maxPct:   maxPcts[rng.Intn(len(maxPcts))],
						reserve:  reserves[rng.Intn(len(reserves))],
						health:   healths[rng.Intn(len(healths))],
					})
				}
			}
			// The same structural case with NO filter condition at all: nil is
			// a real value the policy short-circuits on, not an absence.
			add(caseSpec{
				name: "strategy=" + string(st) + "/vec=" + vn + "/nofilter",
				settings: models.BetSettings{
					Strategy: st, Percentage: 5, PercentageGap: 20, MaxPoints: 50_000,
				},
				outcomes: cloneOutcomes(vectors[vn]),
				balance:  balances[rng.Intn(len(balances))],
				maxPct:   maxPcts[rng.Intn(len(maxPcts))],
				reserve:  reserves[rng.Intn(len(reserves))],
				health:   healths[rng.Intn(len(healths))],
			})
		}
	}

	// A randomised sweep over the numeric axes, on top of the structural
	// sweep above, so the stake arithmetic and the gates are exercised far
	// beyond the hand-picked vectors.
	for i := 0; i < 4000; i++ {
		n := rng.Intn(5)
		outs := make([]*models.Outcome, 0, n)
		for j := 0; j < n; j++ {
			outs = append(outs, mkOutcome(
				"gen-"+string(rune('a'+j)),
				rng.Intn(50), rng.Intn(5000), rng.Intn(2000),
				roundTo2(rng.Float64()*100), roundTo2(rng.Float64()*10), roundTo2(rng.Float64()*100),
			))
		}
		s := models.BetSettings{
			Strategy:      strategies[rng.Intn(len(strategies))],
			Percentage:    percentages[rng.Intn(len(percentages))],
			PercentageGap: rng.Intn(101),
			MaxPoints:     maxPoints[rng.Intn(len(maxPoints))],
			MinimumPoints: rng.Intn(100),
		}
		if rng.Intn(3) != 0 {
			s.FilterCondition = &models.FilterCondition{
				By:    keys[rng.Intn(len(keys))],
				Where: conditions[rng.Intn(len(conditions))],
				Value: roundTo2(rng.Float64() * 500),
			}
		}
		add(caseSpec{
			name:     "generated/" + itoaTest(i),
			settings: s,
			outcomes: outs,
			balance:  balances[rng.Intn(len(balances))],
			maxPct:   maxPcts[rng.Intn(len(maxPcts))],
			reserve:  reserves[rng.Intn(len(reserves))],
			health:   healths[rng.Intn(len(healths))],
		})
	}
	return out
}

// TestTheReplayReproducesTheRealPolicyOnEveryDeterministicCase is the primary
// oracle for the replay's arithmetic.
//
// It regresses the whole class of "the reimplementation drifted from the
// policy": a >= where the policy has >, a missing NUMBER fallback, the
// decision_* key remapping, the reserve-before-percent priority, the
// minimum-before-filter ordering, and the int truncation in the stake.
func TestTheReplayReproducesTheRealPolicyOnEveryDeterministicCase(t *testing.T) {
	cases := generateCases(t)
	if len(cases) < 5000 {
		t.Fatalf("the generated domain collapsed to %d cases; this test would prove almost nothing", len(cases))
	}

	var (
		panics     int
		agreements int
		byAction   = map[string]int{}
	)
	for _, spec := range cases {
		// The reference mutates its Bet, so each side gets its own outcomes.
		refSpec := spec
		refSpec.outcomes = cloneOutcomes(spec.outcomes)
		ref, panicked := runReference(refSpec)

		ev := predictioneval.Evaluate(replayInputs(spec), predictioneval.ObservedRealization{})

		if panicked {
			panics++
			if ev.Action != predictioneval.ActionLegacyFailure {
				t.Fatalf("%s: the real policy PANICS on this input but the replay reported %q; "+
					"a replay that misses a legacy failure would report a decision the miner could never have made",
					spec.name, ev.Action)
			}
			continue
		}
		if ev.Action == predictioneval.ActionLegacyFailure {
			t.Fatalf("%s: the replay claimed a legacy failure (%s) but the real policy completed normally",
				spec.name, ev.LegacyFailure)
		}
		assertParity(t, spec, ref, ev)
		agreements++
		byAction[ref.action]++
		if t.Failed() {
			t.Fatalf("stopping at first divergence: %s", spec.name)
		}
	}

	// Non-vacuity. A domain that never panicked, or that only ever produced
	// one terminal action, would make the assertions above nearly free.
	if panics == 0 {
		t.Error("no generated case reproduced the pinned policy's panic; the legacy-failure branch is unproven")
	}
	if agreements == 0 {
		t.Fatal("no case completed; the parity assertions never ran")
	}
	for _, want := range []string{
		predictioneval.ActionWouldAttemptPlacement,
		predictioneval.ActionBelowMinimum,
		predictioneval.ActionFilterRejected,
		predictioneval.ActionReserveViolation,
		predictioneval.ActionHealthGated,
	} {
		if byAction[want] == 0 {
			t.Errorf("no generated case ended in %s; that exit path is unproven by this test", want)
		}
	}
	t.Logf("parity over %d cases: %d completed, %d reproduced the pinned policy's panic, actions=%v",
		len(cases), agreements, panics, byAction)
}

// TestNegativeChoiceIndexPanicsThePinnedPolicy pins the legacy failure to the
// REAL models package rather than to this package's belief about it.
//
// If a future change to internal/models fixed the panic, this test would fail
// and the replay's LegacyFailureNegativeChoiceIndex branch would have to be
// revisited — which is exactly the notification that should happen. The panic
// itself is NOT fixed here: production behaviour is out of this task's scope,
// and the replay models it rather than changing it.
func TestNegativeChoiceIndexPanicsThePinnedPolicy(t *testing.T) {
	// SMART with a single outcome leaves Calculate's choice at -1, and a
	// decision-scoped filter key then indexes the outcome slice with it.
	// getOutcomeValue guards only indexes at or PAST the end, never negative
	// ones.
	bet := &models.Bet{
		Outcomes: []*models.Outcome{mkOutcome("o1", 3, 300, 50, 60, 1.66, 60.24)},
		Settings: models.BetSettings{
			Strategy:      models.StrategySmart,
			PercentageGap: 20,
			FilterCondition: &models.FilterCondition{
				By: models.OutcomePercentageUsers, Where: models.ConditionGT, Value: 1,
			},
		},
	}
	decision := bet.Calculate(100)
	if decision.Choice != -1 {
		t.Fatalf("precondition failed: SMART with one outcome chose %d, want -1", decision.Choice)
	}

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("models.Bet.Skip did NOT panic on a negative choice index; " +
					"the replay's legacy-failure model is now wrong and must be revisited")
			}
		}()
		_, _ = bet.Skip()
	}()

	// And the replay reports it as a typed result instead of crashing.
	ev := predictioneval.Evaluate(replayInputs(caseSpec{
		settings: bet.Settings,
		outcomes: bet.Outcomes,
		balance:  100,
		health:   predictioneval.HealthAllowed,
	}), predictioneval.ObservedRealization{})

	if ev.Action != predictioneval.ActionLegacyFailure {
		t.Fatalf("replay action = %q, want %q", ev.Action, predictioneval.ActionLegacyFailure)
	}
	if ev.LegacyFailure != predictioneval.LegacyFailureNegativeChoiceIndex {
		t.Fatalf("legacy failure = %q, want %q", ev.LegacyFailure, predictioneval.LegacyFailureNegativeChoiceIndex)
	}
	if ev.Filter.State != predictioneval.StageStateLegacyFailure {
		t.Fatalf("filter stage = %q, want %q", ev.Filter.State, predictioneval.StageStateLegacyFailure)
	}
	// Stages after the failure must stay unexecuted rather than carrying zeros.
	if ev.StakeGate.State != predictioneval.StageStateNotReached {
		t.Fatalf("stake gate = %q after a legacy failure, want NOT_REACHED", ev.StakeGate.State)
	}
}

// TestTheFilterIsActedOnAfterTheMinimumStakeExit pins an ordering that is easy
// to get wrong and invisible in most cases.
//
// Skip is CALLED immediately after Calculate but ACTED ON after the
// minimum-stake check. A stake below the minimum that ALSO fails the filter
// must report BELOW_MINIMUM_POINTS, because that is the exit the miner takes.
func TestTheFilterIsActedOnAfterTheMinimumStakeExit(t *testing.T) {
	spec := caseSpec{
		name: "below-minimum-and-filtered",
		settings: models.BetSettings{
			Strategy:      models.StrategyMostVoted,
			Percentage:    1,
			PercentageGap: 20,
			MaxPoints:     50_000,
			// A condition that cannot hold, so Skip answers "skip".
			FilterCondition: &models.FilterCondition{
				By: models.OutcomeTotalUsers, Where: models.ConditionGT, Value: 1e9,
			},
		},
		outcomes: []*models.Outcome{
			mkOutcome("o1", 6, 600, 90, 60, 1.66, 60.24),
			mkOutcome("o2", 4, 400, 80, 40, 2.5, 40),
		},
		balance: 100, // 1% of 100 = 1, which is below the pinned minimum of 10
		health:  predictioneval.HealthAllowed,
	}
	ref, panicked := runReference(spec)
	if panicked {
		t.Fatal("the reference panicked; this fixture was meant to complete")
	}
	if !ref.skip {
		t.Fatal("precondition failed: the filter did not reject, so the ordering is not under test")
	}
	if ref.action != predictioneval.ActionBelowMinimum {
		t.Fatalf("the real path ended in %q, want BELOW_MINIMUM_POINTS", ref.action)
	}

	ev := predictioneval.Evaluate(replayInputs(spec), predictioneval.ObservedRealization{})
	if ev.Action != predictioneval.ActionBelowMinimum {
		t.Fatalf("replay action = %q, want BELOW_MINIMUM_POINTS: the filter must not be acted on "+
			"before the minimum-stake exit", ev.Action)
	}
	if !ev.Filter.Skip {
		t.Fatal("the replay lost the filter's answer; it is computed even when it is not acted on")
	}
	if ev.Filter.Applied {
		t.Fatal("the replay marked the filter as applied, but the attempt ended at the minimum-stake exit")
	}
}

// ---- fixture helpers -------------------------------------------------------

func mkOutcome(id string, users, points, top int, pctUsers, odds, oddsPct float64) *models.Outcome {
	return &models.Outcome{
		ID: id, TotalUsers: users, TotalPoints: points, TopPoints: top,
		PercentageUsers: pctUsers, Odds: odds, OddsPercentage: oddsPct,
	}
}

func cloneOutcomes(in []*models.Outcome) []*models.Outcome {
	out := make([]*models.Outcome, len(in))
	for i, o := range in {
		if o == nil {
			continue
		}
		c := *o
		out[i] = &c
	}
	return out
}

func roundTo2(v float64) float64 { return math.Round(v*100) / 100 }

func itoaTest(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
