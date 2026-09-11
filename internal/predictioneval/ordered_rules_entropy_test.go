package predictioneval_test

// EXACT ENTROPY CONSUMPTION.
//
// rand 0.8.5 builds a Bernoulli distribution and then samples it, and the two
// steps have an asymmetry that is easy to "optimize" away in either direction:
//
//	Bernoulli::new(p):  p == 1.0        → ALWAYS_TRUE sentinel
//	                    0.0 <= p < 1.0  → p_int = (p * 2^64) as u64
//	sample():           p_int == ALWAYS_TRUE → true, WITHOUT drawing
//	                    otherwise            → draw one u64, return raw < p_int
//
// So a rate of exactly ONE succeeds and consumes NOTHING, while a rate of
// exactly ZERO consumes a word and then fails on `raw < 0`. Both symmetric
// short-cuts — "100% needs no draw, so 0% needs none either", or "always draw
// so the stream stays aligned" — are wrong, in opposite directions, and each
// silently re-aligns every subsequent draw in the run.

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
)

// orTwoOutcomePool is the recurring [A:4, B:6] candidate.
func orTwoOutcomePool(id string, pos int64) predictioneval.OrderedRulesCandidate {
	return orCandidate(id, pos, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6))
}

// TestOrderedRulesPZeroConsumesRawDraw pins the asymmetry's zero side.
//
// A matched comparator at a rate of zero still builds a distribution whose
// threshold is zero, still draws, and only then fails. Skipping the draw would
// leave the word available to the NEXT rule and shift every later decision.
func TestOrderedRulesPZeroConsumesRawDraw(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{orTwoOutcomePool("c1", 10)}
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 50, 0, 0, 10),
	}, 95, 100, 0, 10)

	ev := orEval(t, cs, cfg, orWordAdmit)
	if ev.Selected != nil {
		t.Fatalf("a rate of zero never admits; got %+v", ev.Selected)
	}
	if ev.RawWordsConsumed != 1 {
		t.Fatalf("a matched comparator at a rate of ZERO consumes one word and then fails: consumed %d, "+
			"want 1. Consuming none re-aligns every later draw in the run.", ev.RawWordsConsumed)
	}
	if ev.BernoulliEvaluations != 1 {
		t.Fatalf("the draw was evaluated, so it must be counted: %d", ev.BernoulliEvaluations)
	}
}

// TestOrderedRulesPOneConsumesNoRawDraw pins the asymmetry's one side.
//
// A rate of exactly one hits the ALWAYS_TRUE sentinel and returns without
// touching the generator. Drawing anyway would consume a word the donor never
// spent, and every later draw would read the wrong one.
func TestOrderedRulesPOneConsumesNoRawDraw(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{orTwoOutcomePool("c1", 10)}
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 50, 100, 0, 10),
	}, 95, 100, 0, 10)

	t.Run("with no words at all", func(t *testing.T) {
		// If the model drew here it would report exhaustion instead of admitting.
		ev := orEval(t, cs, cfg)
		sel := orMustAdmit(t, ev)
		if sel.OutcomeIdentity != "A" {
			t.Fatalf("selected %q, want \"A\"", sel.OutcomeIdentity)
		}
		if ev.RawWordsConsumed != 0 {
			t.Fatalf("a rate of ONE consumes no word: consumed %d", ev.RawWordsConsumed)
		}
		if ev.BernoulliEvaluations != 1 {
			t.Fatalf("the distribution was still evaluated, just without drawing: %d",
				ev.BernoulliEvaluations)
		}
	})

	// The case above proves only that no word is REQUIRED. On its own it would
	// still pass if the model happily took a word whenever one was available —
	// which would silently re-align every later draw in a longer run. So the
	// same rate is exercised again with words present and untouched.
	t.Run("with spare words present", func(t *testing.T) {
		ev := orEval(t, cs, cfg, orWordRefuse, orWordAdmit)
		orMustAdmit(t, ev)
		if ev.RawWordsConsumed != 0 {
			t.Fatalf("a rate of ONE must not touch an AVAILABLE word either: consumed %d",
				ev.RawWordsConsumed)
		}
	})

	// And the words really were consumable: the same trace under a rate of one
	// half spends its first word, so the case above is not passing because the
	// trace was somehow unusable.
	t.Run("the spare words were genuinely available", func(t *testing.T) {
		half := orConfig([]predictioneval.OrderedRule{
			orRule(predictioneval.ComparatorLe, 50, 50, 0, 10),
		}, 95, 100, 0, 10)
		if ev := orEval(t, cs, half, orWordRefuse, orWordAdmit); ev.RawWordsConsumed == 0 {
			t.Fatal("the control run consumed nothing, so the assertions above prove nothing")
		}
	})
}

// TestOrderedRulesRawThresholdStrictLess pins the comparison as STRICT.
//
// A rate of one half maps to a threshold of exactly 2^63 with no rounding, so
// the boundary is exact: the word one below admits, the word AT the threshold
// does not. A `<=` here would admit one extra value out of 2^64 — invisible in
// any frequency test, and wrong.
func TestOrderedRulesRawThresholdStrictLess(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{orTwoOutcomePool("c1", 10)}
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 50, 50, 0, 10),
	}, 95, 100, 0, 10)

	if got := orEval(t, cs, cfg, orWordRefuse-1); got.Selected == nil {
		t.Fatalf("the word one BELOW the threshold must admit; got status %q", got.Status)
	}
	if got := orEval(t, cs, cfg, orWordRefuse); got.Selected != nil {
		t.Fatalf("the word AT the threshold must refuse under a strict `raw < threshold`; got %+v",
			got.Selected)
	}
}

// TestOrderedRulesDefaultAddsNoDraw pins the default as entropy-free — and,
// just as importantly, pins that it does not REFUND the words already spent.
//
// The rule matches outcome A and its draw fails, spending one word. The default
// then matches the same outcome and admits. The run's total must be exactly the
// one word the failed rule spent: the default adds none, and the failure does
// not become free in hindsight.
func TestOrderedRulesDefaultAddsNoDraw(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{orTwoOutcomePool("c1", 10)}
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 50, 50, 0, 10),
	}, 35, 45, 0, 10)

	// A SPARE word follows the one the rule spends. Without it this case would
	// still pass if the default drew whenever a word happened to be available,
	// because the trace would already be empty by the time the default ran.
	ev := orEval(t, cs, cfg, orWordRefuse, orWordAdmit)
	sel := orMustAdmit(t, ev)
	if sel.Basis != predictioneval.SelectionDefault || sel.OutcomeIdentity != "A" {
		t.Fatalf("after a failed rule draw the CURRENT outcome's default must admit: got %q by %s",
			sel.OutcomeIdentity, sel.Basis)
	}
	if ev.RawWordsConsumed != 1 {
		t.Fatalf("the default adds no draw and the failed rule's word stays spent: consumed %d, want 1",
			ev.RawWordsConsumed)
	}
	if ev.BernoulliEvaluations != 1 {
		t.Fatalf("only the rule drew, so exactly one Bernoulli was evaluated: %d", ev.BernoulliEvaluations)
	}

	// A default admission with a FULL trace in hand must spend nothing at all.
	bare := orEval(t, cs, orConfig(nil, 35, 45, 0, 10), orWordAdmit, orWordRefuse)
	if s := orMustAdmit(t, bare); s.Basis != predictioneval.SelectionDefault {
		t.Fatalf("basis = %q, want DEFAULT", s.Basis)
	}
	if bare.RawWordsConsumed != 0 {
		t.Fatalf("a default admission with words available must still consume none: %d",
			bare.RawWordsConsumed)
	}
}

// TestOrderedRulesRepeatedDistinctCandidatesConsumeAgain pins that each
// candidate is a NEW opportunity drawing from the CONTINUING sequence.
//
// The three candidates carry identical values at different source positions.
// That is not a duplicate — the donor re-enters its logic on every round update
// — so each gets its own draw, taken from where the previous one left off. A
// one-shot Bernoulli, a per-candidate RNG reset, or a content-based dedupe each
// changes the answer.
//
// The second half is an exact mechanical oracle rather than one lucky seed: all
// eight pass/fail combinations are enumerated. With a single matching rule at
// one half, no reachable default and an early stop, seven of the eight admit
// and the eight runs consume fourteen words in total.
func TestOrderedRulesRepeatedDistinctCandidatesConsumeAgain(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{
		orTwoOutcomePool("c1", 10),
		orTwoOutcomePool("c2", 20),
		orTwoOutcomePool("c3", 30),
	}
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 50, 50, 0, 10),
	}, 95, 100, 0, 10)

	t.Run("the sequence continues across candidates", func(t *testing.T) {
		ev := orEval(t, cs, cfg, orWordRefuse, orWordRefuse, orWordAdmit)
		sel := orMustAdmit(t, ev)
		if sel.CandidateIdentity != "c3" {
			t.Fatalf("admitted on %q, want \"c3\": the third word decides the third candidate only if the "+
				"sequence continued rather than restarting", sel.CandidateIdentity)
		}
		if ev.RawWordsConsumed != 3 {
			t.Fatalf("three candidates each drew once: consumed %d, want 3", ev.RawWordsConsumed)
		}
	})

	t.Run("all eight combinations enumerated", func(t *testing.T) {
		admitted, consumed := 0, 0
		for mask := 0; mask < 8; mask++ {
			words := make([]uint64, 3)
			for i := range words {
				words[i] = orWordRefuse
				if mask&(1<<i) != 0 {
					words[i] = orWordAdmit
				}
			}
			ev := orEval(t, cs, cfg, words...)
			if ev.Selected != nil {
				admitted++
			}
			consumed += ev.RawWordsConsumed
		}
		if admitted != 7 {
			t.Fatalf("with three independent halves and an early stop, seven of eight combinations admit: "+
				"got %d", admitted)
		}
		if consumed != 14 {
			t.Fatalf("the eight runs consume fourteen words in total — a mean of 7/4 per run: got %d",
				consumed)
		}
	})
}

// TestOrderedRulesDuplicateIdentityIsRejected pins the other half of the
// repeated-candidate rule.
//
// Identical VALUES at different positions are new opportunities; an identical
// IDENTITY is a contradiction in the input. It is refused rather than resolved:
// dropping one removes an opportunity, keeping both invents one, and neither is
// the caller's stated intent.
func TestOrderedRulesDuplicateIdentityIsRejected(t *testing.T) {
	dup := orTwoOutcomePool("c1", 20)
	_, err := predictioneval.ProjectOrderedRulesStream(
		orSource([]predictioneval.OrderedRulesCandidate{orTwoOutcomePool("c1", 10), dup}, nil),
		orAdmission())
	if err == nil {
		t.Fatal("a repeated candidate identity must be refused, not silently deduplicated")
	}
	if !errors.Is(err, predictioneval.ErrOrderedRulesDuplicateIdentity) {
		t.Fatalf("got %v, want a duplicate-identity refusal", err)
	}
}

// TestOrderedRulesEntropyExhaustionIsUnknown pins that a spent trace is an
// explicit unknown.
//
// The comparator matched, so the draw was REQUIRED. With no word left the model
// stops and says so. The three tempting alternatives — fall back to a default
// generator, treat the missing word as zero, or skip the rule — each fabricate
// an answer, and the last two would silently change the traversal's outcome.
func TestOrderedRulesEntropyExhaustionIsUnknown(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{orTwoOutcomePool("c1", 10)}
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 50, 50, 0, 10),
	}, 35, 45, 0, 10)

	ev := orEval(t, cs, cfg)
	if ev.Status != predictioneval.StatusUnknownInput || ev.Reason != predictioneval.ReasonEntropyExhausted {
		t.Fatalf("status = %q reason = %q, want UNKNOWN_INPUT / ENTROPY_EXHAUSTED", ev.Status, ev.Reason)
	}
	if ev.Selected != nil {
		t.Fatalf("nothing may be admitted on an unreadable draw; got %+v", ev.Selected)
	}
	// The default bounds below WOULD have matched this outcome. Reaching them
	// after an unreadable draw is exactly the "skip the rule" failure.
	if ev.Participation != predictioneval.ParticipationNotAdmitted {
		t.Fatal("an exhausted trace must not fall through to the default; the draw's answer is unknown, " +
			"not \"no\"")
	}
	if ev.RawWordsConsumed != 0 {
		t.Fatalf("no word existed to consume: %d", ev.RawWordsConsumed)
	}
}

// TestOrderedRulesStopDoesNotReadNextCandidateOrDraw pins the stop as total.
//
// After an admission the traversal is over. The sentinel candidate that follows
// would admit a DIFFERENT outcome, and the sentinel word that follows would
// change the draw — so if either were read, the result would visibly differ.
func TestOrderedRulesStopDoesNotReadNextCandidateOrDraw(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{
		orTwoOutcomePool("c1", 10),
		orCandidate("sentinel", 20, orKnownBalance(1), orOutcome("Z", 9), orOutcome("Y", 1)),
	}
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 50, 50, 0, 10),
	}, 95, 100, 0, 10)

	ev := orEval(t, cs, cfg, orWordAdmit, orWordAdmit)
	sel := orMustAdmit(t, ev)
	switch {
	case sel.CandidateIdentity != "c1":
		t.Fatalf("admitted on %q, want \"c1\"", sel.CandidateIdentity)
	case ev.CandidatesConsumed != 1:
		t.Fatalf("consumed %d candidates, want 1: the sentinel must never be reached", ev.CandidatesConsumed)
	case ev.RawWordsConsumed != 1:
		t.Fatalf("consumed %d words, want 1: the sentinel word must never be drawn", ev.RawWordsConsumed)
	case len(ev.Visits) != 1:
		t.Fatalf("recorded %d visits, want 1", len(ev.Visits))
	}
	for _, e := range ev.Trace {
		if e.CandidateIndex != 0 {
			t.Fatalf("the trace reached candidate index %d after the stop", e.CandidateIndex)
		}
	}
}

// TestOrderedRulesSuppliedDrawTraceIsIndependentOfObservedStealth pins the
// entropy seam shut.
//
// The baseline replay carries an [predictioneval.ObservedRealization]: a
// RECORDED stealth draw, kept so a decision that consumed randomness can be
// reconstructed conditionally. It is an OBSERVED value from a decision that
// already happened, and this model is evaluating a world in which no decision
// happened at all — so feeding it in here would leak the observed outcome into
// the counterfactual and make the result look reproducible when it is not.
//
// The separation is structural rather than conventional: nothing in the
// ordered-rules input types can hold one, and the evaluator's signature cannot
// accept one.
func TestOrderedRulesSuppliedDrawTraceIsIndependentOfObservedStealth(t *testing.T) {
	observed := reflect.TypeOf(predictioneval.ObservedRealization{})
	for _, typ := range []reflect.Type{
		reflect.TypeOf(predictioneval.SuppliedDrawTrace{}),
		reflect.TypeOf(predictioneval.OrderedRulesStream{}),
		reflect.TypeOf(predictioneval.OrderedRulesCandidate{}),
		reflect.TypeOf(predictioneval.OrderedRulesConfig{}),
	} {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if f.Type == observed {
				t.Errorf("%s.%s carries an ObservedRealization; the donor's entropy must be SUPPLIED, "+
					"never borrowed from a recorded stealth draw", typ.Name(), f.Name)
			}
			if strings.Contains(strings.ToLower(f.Name), "stealth") ||
				strings.Contains(strings.ToLower(f.Name), "observedrealization") {
				t.Errorf("%s.%s names an observed realization", typ.Name(), f.Name)
			}
		}
	}

	// And the evaluator takes exactly three inputs, none of them observed.
	fn := reflect.TypeOf(predictioneval.EvaluateOrderedRules)
	if fn.NumIn() != 3 {
		t.Fatalf("EvaluateOrderedRules takes %d inputs, want 3", fn.NumIn())
	}
	for i := 0; i < fn.NumIn(); i++ {
		if fn.In(i) == observed {
			t.Fatalf("EvaluateOrderedRules accepts an ObservedRealization at position %d", i)
		}
	}
}
