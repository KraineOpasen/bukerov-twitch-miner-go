package predictioneval_test

// REPRODUCIBILITY, THE WORK BUDGET, AND THE UNCHANGED BASELINE.
//
// The last three obligations. The first two are about this model; the third is
// about everything around it — the current-configured replay that was already
// here, which this addition must leave exactly as it found it.

import (
	"encoding/json"
	"strings"
	"testing"
	"unsafe"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
)

// TestOrderedRulesIsByteIdenticalAcrossRuns pins determinism.
//
// The same declared inputs, config, trace and versions must produce the same
// canonical result every time — independent of map iteration order, of the
// clock this package cannot read, and of how many times it has run. The
// serialized form is compared, not just the fields, because a map anywhere in
// the result types would show up here and nowhere else.
func TestOrderedRulesIsByteIdenticalAcrossRuns(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{
		orTwoOutcomePool("c1", 10),
		orTwoOutcomePool("c2", 20),
		orTwoOutcomePool("c3", 30),
	}
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 50, 50, 500, 10),
		orRule(predictioneval.ComparatorGe, 55, 25, 0, 5),
	}, 35, 45, 250, 7.5)

	var first []byte
	for run := 0; run < 8; run++ {
		stream := orProject(t, cs, []predictioneval.OrderedRulesIntervention{
			orIntervention("call-1", 40, predictioneval.InterventionAutoCallStarted,
				predictioneval.RelevanceProven, "boundary"),
		})
		ev := predictioneval.EvaluateOrderedRules(stream, cfg, orDraws(orWordRefuse, orWordRefuse, orWordAdmit))
		got, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if run == 0 {
			first = got
			continue
		}
		if string(got) != string(first) {
			t.Fatalf("run %d differs from run 0:\n first = %s\n  this = %s", run, first, got)
		}
	}
	if len(first) == 0 {
		t.Fatal("nothing was serialized; this check would pass vacuously")
	}
}

// TestOrderedRulesConfigAndStreamAreNotMutatedByEvaluation pins that evaluating
// is a read.
//
// A model that normalized the caller's config in place would work perfectly on
// the first call and halve every rate on the second — the classic
// normalize-twice bug, arriving one call late.
func TestOrderedRulesConfigAndStreamAreNotMutatedByEvaluation(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{orTwoOutcomePool("c1", 10)}
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 50, 50, 0, 10),
	}, 95, 100, 0, 10)

	before, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	stream := orProject(t, cs, nil)
	streamBefore, err := json.Marshal(stream)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	first := predictioneval.EvaluateOrderedRules(stream, cfg, orDraws(orWordAdmit))
	second := predictioneval.EvaluateOrderedRules(stream, cfg, orDraws(orWordAdmit))

	after, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	streamAfter, err := json.Marshal(stream)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	switch {
	case string(before) != string(after):
		t.Fatalf("the config was mutated in place:\n before = %s\n  after = %s", before, after)
	case string(streamBefore) != string(streamAfter):
		t.Fatal("the stream was mutated in place")
	case first.Status != second.Status || first.Stake != second.Stake:
		t.Fatalf("the second evaluation of the same inputs differs: %+v then %+v",
			first.Stake, second.Stake)
	}
}

// TestOrderedRulesWorkBudgetRefusesRatherThanTruncates pins the live work
// counter.
//
// The other bounds together permit more predicate slots than the budget allows,
// so the budget has to be counted as it is spent. When it runs out the answer
// is a refusal — not a shortened traversal reported as a complete one.
func TestOrderedRulesWorkBudgetRefusesRatherThanTruncates(t *testing.T) {
	// The widest input the other bounds allow: every candidate, every outcome,
	// every rule, with no rule ever matching so nothing stops early.
	cs := make([]predictioneval.OrderedRulesCandidate, predictioneval.MaxOrderedRulesCandidates)
	outs := make([]predictioneval.OrderedRulesOutcome, predictioneval.MaxOrderedRulesOutcomes)
	for i := range outs {
		outs[i] = orOutcome("o"+itoaTest(i), int64(i+1))
	}
	for i := range cs {
		cs[i] = orCandidate("c"+itoaTest(i), int64(i+1), orKnownBalance(1000), outs...)
	}
	rules := make([]predictioneval.OrderedRule, predictioneval.MaxOrderedRulesRules)
	for i := range rules {
		// A threshold of zero under Le matches only a share of exactly zero,
		// which this pool never produces.
		rules[i] = orRule(predictioneval.ComparatorLe, 0, 100, 0, 10)
	}

	ev := predictioneval.EvaluateOrderedRules(orProject(t, cs, nil),
		orConfig(rules, 95, 100, 0, 10), orDraws())
	if ev.Status != predictioneval.StatusRefused || ev.Reason != predictioneval.ReasonWorkBudgetExceeded {
		t.Fatalf("status %q reason %q, want REFUSED / WORK_BUDGET_EXCEEDED", ev.Status, ev.Reason)
	}
	if ev.Selected != nil {
		t.Fatal("a refused traversal decides nothing")
	}

	// Non-vacuity: a materially smaller input of the same shape completes.
	small := cs[:2]
	ok := predictioneval.EvaluateOrderedRules(orProject(t, small, nil),
		orConfig(rules, 95, 100, 0, 10), orDraws())
	if ok.Status != predictioneval.StatusNoAttemptInSuppliedPrefix {
		t.Fatalf("the control run must complete: status %q reason %q", ok.Status, ok.Reason)
	}
}

// TestOrderedRulesRetainedTraceStaysInsideTheAggregateByteBudget pins the bound
// that actually costs memory.
//
// Every evaluated slot appends one trace entry, so the slot ceiling IS the
// trace length. A ceiling chosen without that in mind lets an input sitting
// inside every other declared bound — and consuming no entropy at all — retain
// well over a hundred megabytes of trace from one call, which is precisely the
// unbounded allocation the bounds exist to prevent.
//
// So the arithmetic is asserted here rather than left to a comment: the worst
// case a traversal can retain must fit the declared aggregate budget with room
// for the arrays append discards on the way there.
func TestOrderedRulesRetainedTraceStaysInsideTheAggregateByteBudget(t *testing.T) {
	entry := int64(unsafe.Sizeof(predictioneval.OrderedRulesTraceEntry{}))
	worst := entry * int64(predictioneval.MaxOrderedRulesWork)
	// Append grows by doubling, so the peak holds the new array plus the old
	// one it is copying from: about 1.5x the final size, and 3x leaves margin.
	if worst*3 > int64(predictioneval.MaxOrderedRulesAggregateBytes) {
		t.Fatalf("a maximal traversal retains %d bytes of trace (%d entries x %d bytes); with append's "+
			"doubling that is past the declared aggregate budget of %d. Narrow MaxOrderedRulesWork "+
			"rather than leaving the budget overstated.",
			worst, predictioneval.MaxOrderedRulesWork, entry,
			predictioneval.MaxOrderedRulesAggregateBytes)
	}
	t.Logf("worst-case retained trace: %d entries x %d bytes = %.1f MiB, budget %.0f MiB",
		predictioneval.MaxOrderedRulesWork, entry, float64(worst)/(1024*1024),
		float64(predictioneval.MaxOrderedRulesAggregateBytes)/(1024*1024))

	// And the bound is really enforced: the widest input the other limits allow
	// is refused, and the trace it comes back with is bounded by the ceiling
	// rather than by the input's size.
	cs := make([]predictioneval.OrderedRulesCandidate, predictioneval.MaxOrderedRulesCandidates)
	outs := make([]predictioneval.OrderedRulesOutcome, predictioneval.MaxOrderedRulesOutcomes)
	for i := range outs {
		outs[i] = orOutcome("o"+itoaTest(i), int64(i+1))
	}
	for i := range cs {
		cs[i] = orCandidate("c"+itoaTest(i), int64(i+1), orKnownBalance(1000), outs...)
	}
	rules := make([]predictioneval.OrderedRule, predictioneval.MaxOrderedRulesRules)
	for i := range rules {
		rules[i] = orRule(predictioneval.ComparatorLe, 0, 100, 0, 10)
	}
	ev := predictioneval.EvaluateOrderedRules(orProject(t, cs, nil),
		orConfig(rules, 95, 100, 0, 10), orDraws())
	if ev.Status != predictioneval.StatusRefused {
		t.Fatalf("status = %q, want REFUSED", ev.Status)
	}
	if len(ev.Trace) > predictioneval.MaxOrderedRulesWork {
		t.Fatalf("the refused result carries %d trace entries, past the ceiling of %d",
			len(ev.Trace), predictioneval.MaxOrderedRulesWork)
	}
}

// TestOrderedRulesAdditionLeavesCurrentConfiguredBaselineByteIdentical is the
// preservation proof.
//
// The ordered-rules core sits in the same package as the current-configured
// replay, and the one thing it must not do is disturb it. The literal below is
// the serialized [predictioneval.Evaluation] of a fixed decision, and it is
// pinned so that any change to the baseline's arithmetic, stage vocabulary,
// limitation text, provenance or JSON shape fails HERE — beside the addition
// that would have caused it — rather than somewhere downstream.
//
// The four seams' own suites still hold their own behaviour; this one exists to
// make "the baseline is unchanged" a mechanical claim rather than a review note.
func TestOrderedRulesAdditionLeavesCurrentConfiguredBaselineByteIdentical(t *testing.T) {
	in := predictioneval.DecisionInputs{
		CommonInputDigest: "pinned-baseline-digest",
		ReachedDecision:   true,
		Settings: &predictioneval.BetSettingsInput{
			Strategy:      "SMART",
			Percentage:    5,
			PercentageGap: 20,
			MaxPoints:     50000,
			MinimumPoints: 0,
			StealthMode:   false,
			Delay:         6,
			DelayMode:     "FROM_END",
		},
		Balance:        1000,
		BalancePresent: true,
		Outcomes: []predictioneval.OutcomeInput{
			{Slot: 0, Present: true, ID: "outcome-a", TotalUsers: 10, TotalPoints: 400,
				TopPoints: 100, PercentageUsers: 50, Odds: 2.5, OddsPercentage: 40},
			{Slot: 1, Present: true, ID: "outcome-b", TotalUsers: 10, TotalPoints: 600,
				TopPoints: 200, PercentageUsers: 50, Odds: 1.6666666666666667, OddsPercentage: 60},
		},
		OutcomesPresent: true,
		RiskPresent:     false,
		HealthState:     "NO_GATE",
		MinimumStake:    predictioneval.PinnedMinimumStake,
	}

	got, err := json.Marshal(predictioneval.Evaluate(in, predictioneval.ObservedRealization{}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != baselineEvaluationGolden {
		t.Fatalf("the current-configured baseline changed.\n got  = %s\n want = %s\n\n"+
			"This test exists so that an addition beside the baseline cannot alter it silently. If the "+
			"change was intended, it belongs in its own commit with its own justification — not folded "+
			"into the ordered-rules core.", got, baselineEvaluationGolden)
	}
}

// baselineEvaluationGolden is the serialized baseline evaluation captured at
// this branch's base commit, before any ordered-rules file existed.
const baselineEvaluationGolden = `{"model":{"modelVersion":"predictioneval/v1","policyRevision":"policy-378d05d6ccc7d2a914730a1e1d023ff754bcf873","supportedProducerRevision":"obs-v2|policy-378d05d6ccc7d2a914730a1e1d023ff754bcf873","platformIntBits":64},"commonInputDigest":"pinned-baseline-digest","choice":{"state":"EXECUTED","index":0,"selected":true,"outcomeId":"outcome-a"},"baseStake":{"state":"EXECUTED","amount":50,"capped":false},"stealth":{"state":"EXECUTED","outcome":"NOT_APPLICABLE","applies":false,"realized":50,"reductionDerived":false,"realizationSound":true},"filter":{"state":"EXECUTED","skip":false,"compared":0,"applied":false},"health":{"state":"WITNESSED","verdict":"NO_GATE"},"stakeGate":{"state":"UNSUPPORTED","proposed":0,"allowed":0,"reason":"","limit":0},"clamp":{"state":"NOT_REACHED","applied":false,"finalAmount":0,"hasFinal":false},"minimum":{"state":"NOT_REACHED","threshold":10,"below":false},"policyAmount":50,"policyAmountKnown":true,"action":"UNSUPPORTED","limitations":["HEALTH_VERDICT_IS_WITNESSED_NOT_RECONSTRUCTED"]}`

// TestOrderedRulesRetainedTraceStaysInsideTheBudgetWhenSerialized pins the half
// of the same budget that unsafe.Sizeof cannot see.
//
// The sibling test above measures the trace as it sits in memory, where two Go
// strings are two 16-byte headers pointing at bytes the stream already owns.
// Serialized they are not headers: every entry writes its identifiers out in
// full. Carrying a candidate and an outcome identity on each of
// MaxOrderedRulesWork entries turned a 31 MiB retained trace into 2.07 GiB of
// JSON — sixteen times the budget this package declares — from an input inside
// every other bound. JSON escaping multiplies that again.
//
// Both identifiers were already recoverable without being repeated:
// CandidateIndex and OutcomeIndex address stream.Candidates[ci].Outcomes[oi]
// directly, and StreamDigest binds which stream that is. So the entry carries
// the indices and the bounded scalars, and the assertion below is that its
// encoded size does not depend on the caller's identifier lengths at all.
func TestOrderedRulesRetainedTraceStaysInsideTheBudgetWhenSerialized(t *testing.T) {
	// A rule that can never match a share in [0,1] and a default admitting
	// nothing: every slot is evaluated, every slot appends, none stops early.
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorGe, 100, 100, 0, 10),
	}, 100, 100, 0, 10)

	entryBytes := func(idLen int) int {
		t.Helper()
		pad := func(s string) string {
			return s + strings.Repeat("x", idLen-len(s))
		}
		ev := predictioneval.EvaluateOrderedRules(orProject(t,
			[]predictioneval.OrderedRulesCandidate{
				orCandidate(pad("c0"), 10, orKnownBalance(100),
					orOutcome(pad("o0"), 1), orOutcome(pad("o1"), 2)),
			}, nil), cfg, orDraws())
		if len(ev.Trace) == 0 {
			t.Fatalf("the fixture recorded no trace entries: status %q reason %q", ev.Status, ev.Reason)
		}
		b, err := json.Marshal(ev.Trace[0])
		if err != nil {
			t.Fatalf("marshalling a trace entry failed: %v", err)
		}
		return len(b)
	}

	short := entryBytes(8)
	widest := entryBytes(predictioneval.MaxOrderedRulesIdentifierBytes)
	if short != widest {
		t.Fatalf("one trace entry encodes to %d bytes with 8-byte identifiers and %d bytes with "+
			"%d-byte ones, so the retained trace scales with text the entry does not need: the "+
			"candidate and outcome are already addressed by their indices.",
			short, widest, predictioneval.MaxOrderedRulesIdentifierBytes)
	}

	worst := int64(widest) * int64(predictioneval.MaxOrderedRulesWork)
	if worst > int64(predictioneval.MaxOrderedRulesAggregateBytes) {
		t.Fatalf("a maximal traversal encodes to %d bytes of trace (%d entries x %d bytes), past the "+
			"declared aggregate budget of %d. The type carries JSON tags, so this is a reachable "+
			"cost, not a hypothetical one.",
			worst, predictioneval.MaxOrderedRulesWork, widest,
			predictioneval.MaxOrderedRulesAggregateBytes)
	}
	t.Logf("worst-case serialized trace: %d entries x %d bytes = %.1f MiB, budget %.0f MiB",
		predictioneval.MaxOrderedRulesWork, widest, float64(worst)/(1024*1024),
		float64(predictioneval.MaxOrderedRulesAggregateBytes)/(1024*1024))
}
