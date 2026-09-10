package predictioneval_test

// THE MECHANISM'S SHAPE.
//
// Each test here pins one decision the donor makes that a plausible edit would
// change, and each states the arithmetic outright rather than deferring to the
// implementation. The pool [A:4, B:6] recurs because its two shares — the
// double nearest 0.4 and the double nearest 0.6 — straddle every threshold
// these tests use.

import (
	"math"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
)

// TestOrderedRulesOutcomeBeforeRulePriority pins WHICH loop is the outer one.
//
// The rules are ordered so that the FIRST rule matches only the SECOND
// outcome, and the second rule matches only the first. Both participate at a
// rate of one hundred percent, so entropy cannot decide the answer.
//
//	outcome-outer (donor): outcome A is examined first, rule Ge55 misses it,
//	                       rule Le45 admits it → A, rule 1.
//	rule-outer (wrong):    rule Ge55 is examined first, finds outcome B → B, rule 0.
func TestOrderedRulesOutcomeBeforeRulePriority(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{
		orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
	}
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorGe, 55, 100, 0, 10),
		orRule(predictioneval.ComparatorLe, 45, 100, 0, 10),
	}, 95, 100, 0, 10)

	sel := orMustAdmit(t, orEval(t, cs, cfg))
	if sel.OutcomeIdentity != "A" || sel.RuleIndex != 1 {
		t.Fatalf("the outcome vector must be the OUTER loop: got outcome %q by rule %d, want outcome \"A\" "+
			"by rule 1. Choosing outcome \"B\" by rule 0 means the rules were iterated outside the outcomes, "+
			"which changes which outcome wins whenever more than one rule can match.",
			sel.OutcomeIdentity, sel.RuleIndex)
	}
}

// TestOrderedRulesDefaultPreemptsLaterOutcomeDetailed pins the default as a
// PER-OUTCOME step, evaluated before the next outcome's rules.
//
// The single rule matches only outcome B; the default bounds contain only
// outcome A's share. The donor reaches A's default before B's rule, so A wins.
// A model that applied one global default after every outcome would let B's
// rule fire first and choose B.
func TestOrderedRulesDefaultPreemptsLaterOutcomeDetailed(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{
		orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
	}
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorGe, 55, 100, 0, 10),
	}, 35, 45, 0, 10)

	sel := orMustAdmit(t, orEval(t, cs, cfg))
	if sel.OutcomeIdentity != "A" || sel.Basis != predictioneval.SelectionDefault {
		t.Fatalf("the CURRENT outcome's default must be checked before the NEXT outcome's rules: got "+
			"outcome %q by %s, want outcome \"A\" by DEFAULT. Choosing \"B\" means the default was "+
			"deferred until after every outcome had been offered to the rules.",
			sel.OutcomeIdentity, sel.Basis)
	}
}

// TestOrderedRulesFailedDrawFallsThroughOverlap pins the fall-through.
//
// Two rules overlap on outcome A. The first participates half the time and its
// draw is made to FAIL; the second participates always. The donor's `find`
// keeps scanning after a failed draw, so the second rule still gets its chance
// on the same outcome.
//
// A model that broke out of the rule scan on a failed draw would fall to the
// default — which these bounds deliberately miss — and admit nothing at all.
func TestOrderedRulesFailedDrawFallsThroughOverlap(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{
		orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
	}
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 50, 50, 0, 10),
		orRule(predictioneval.ComparatorLe, 50, 100, 0, 10),
	}, 80, 90, 0, 10)

	ev := orEval(t, cs, cfg, orWordRefuse)
	sel := orMustAdmit(t, ev)
	if sel.OutcomeIdentity != "A" || sel.RuleIndex != 1 {
		t.Fatalf("a matched comparator whose draw FAILED must fall through to later overlapping rules: "+
			"got outcome %q rule %d, want outcome \"A\" rule 1", sel.OutcomeIdentity, sel.RuleIndex)
	}
	if ev.RawWordsConsumed != 1 {
		t.Fatalf("the fall-through must keep the failed rule's spent word and add none for the rule "+
			"that admitted at a rate of one hundred percent: consumed %d, want 1", ev.RawWordsConsumed)
	}
}

// TestOrderedRulesInclusiveThresholdsAndDefaultBounds pins every comparison as
// INCLUSIVE, at exact equality.
//
// Outcome A's share is the double nearest 0.4, and so is a raw threshold of 40
// divided by 100: both are the correctly-rounded binary64 value of the same
// exact number. So `Le 40`, `Ge 40` and a default of [40, 40] all meet the
// share exactly, and a `<` or a `>` anywhere would reject all three.
func TestOrderedRulesInclusiveThresholdsAndDefaultBounds(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{
		orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
	}

	// float64 VARIABLES, not constants: Go folds constant expressions at
	// arbitrary precision, which would compute an exact 0.4 here and hide the
	// binary64 behaviour this whole suite is about.
	var poolTotal, pointsA float64 = 10, 4
	share := 1.0 / (poolTotal / pointsA)
	if share != 40.0/100.0 {
		t.Fatalf("this test's premise is that the share and the threshold are the same double; they are "+
			"%v and %v, so the equality boundary is not actually being exercised", share, 40.0/100.0)
	}

	for _, tc := range []struct {
		name string
		cfg  predictioneval.OrderedRulesConfig
		want predictioneval.OrderedRulesSelectionBasis
	}{
		{"Le at exact equality", orConfig([]predictioneval.OrderedRule{
			orRule(predictioneval.ComparatorLe, 40, 100, 0, 10)}, 95, 100, 0, 10),
			predictioneval.SelectionDetailedRule},
		{"Ge at exact equality", orConfig([]predictioneval.OrderedRule{
			orRule(predictioneval.ComparatorGe, 40, 100, 0, 10)}, 95, 100, 0, 10),
			predictioneval.SelectionDefault},
		{"default with both bounds at the share", orConfig(nil, 40, 40, 0, 10),
			predictioneval.SelectionDefault},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sel := orMustAdmit(t, orEval(t, cs, tc.cfg))
			if sel.OutcomeIdentity != "A" {
				t.Fatalf("an inclusive comparison must admit at exact equality: got outcome %q, want \"A\"",
					sel.OutcomeIdentity)
			}
		})
	}

	// Non-vacuity: a threshold one raw percent tighter must NOT match, so the
	// cases above are pinning inclusivity rather than a rule that matches
	// everything. Le 39 misses BOTH shares — 0.4 and 0.6 — and the default is
	// out of reach, so nothing may be admitted.
	strict := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 39, 100, 0, 10)}, 95, 100, 0, 10)
	ev := orEval(t, cs, strict)
	if ev.Status != predictioneval.StatusNoAttemptInSuppliedPrefix {
		t.Fatalf("Le 39 must miss a share of 0.4; got status %q selecting %+v, so the inclusive cases "+
			"above prove nothing", ev.Status, ev.Selected)
	}
}

// TestOrderedRulesReciprocalRatioGe90Boundary is the numeric-fidelity
// falsifier.
//
// The donor computes a pool share as 1/(total/points) — two divisions. The
// obvious algebraic simplification, points/total, is a DIFFERENT function in
// binary64: for the pool [9, 1] the donor's value is one ulp BELOW the nearest
// double to 0.9, while the shortcut lands exactly ON it.
//
// A Ge rule at a raw threshold of 90 therefore separates them: the donor does
// not match, the shortcut does. Nothing else in the vector can admit, so the
// two implementations produce different terminal statuses.
func TestOrderedRulesReciprocalRatioGe90Boundary(t *testing.T) {
	// float64 VARIABLES again. As untyped constants Go would evaluate
	// 1/(10/9) exactly and round once, landing on the same double as 9/10 —
	// which is precisely the collapse this falsifier exists to detect.
	var total, points float64 = 10, 9
	donor := 1.0 / (total / points)
	shortcut := points / total
	threshold := 90.0 / 100.0

	if math.Float64bits(donor) == math.Float64bits(shortcut) {
		t.Fatal("the premise of this falsifier is that the two formulations differ in binary64; on this " +
			"toolchain they do not, so the test cannot distinguish them")
	}
	if !(donor < threshold) || !(shortcut >= threshold) {
		t.Fatalf("the [9,1] vector must straddle a 90%% Ge threshold: donor=%v shortcut=%v threshold=%v",
			donor, shortcut, threshold)
	}

	cs := []predictioneval.OrderedRulesCandidate{
		orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 9), orOutcome("B", 1)),
	}
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorGe, 90, 100, 0, 10),
	}, 95, 100, 0, 10)

	ev := orEval(t, cs, cfg)
	if ev.Status != predictioneval.StatusNoAttemptInSuppliedPrefix {
		t.Fatalf("the donor's reciprocal share for [9,1] is below a 90%% threshold, so nothing may be "+
			"admitted: got status %q selecting %+v. Matching here means the two divisions were collapsed "+
			"into a direct pool share.", ev.Status, ev.Selected)
	}

	// And the trace must carry the donor's exact bits, not a rounded rendering.
	if len(ev.Trace) == 0 {
		t.Fatal("the traversal recorded no steps")
	}
	if got := ev.Trace[0].ShareBits; got != math.Float64bits(donor) {
		t.Fatalf("the recorded share is %#016x, want the donor's %#016x (the shortcut would be %#016x)",
			got, math.Float64bits(donor), math.Float64bits(shortcut))
	}
}

// TestOrderedRulesNormalizeExactlyOnce pins the single division by one hundred.
//
// A raw attempt rate of 1.0 means ONE PERCENT. Three wrong readings are ruled
// out at once:
//
//   - not normalized at all — the rate would be 1.0, which consumes no word;
//   - normalized twice — the rate would be 0.0001, and the word one below the
//     one-percent threshold would refuse;
//   - normalized once — exactly the boundary asserted below.
func TestOrderedRulesNormalizeExactlyOnce(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{
		orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
	}
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 100, 1.0, 0, 10),
	}, 95, 100, 0, 10)

	// The rand 0.8.5 threshold for one percent, computed from the pinned
	// formula rather than from this package.
	// A single constant each, so Go rounds once on assignment and these are the
	// same doubles an explicit float64 declaration would produce. The typed
	// declarations elsewhere in this file exist because their expressions have
	// MORE THAN ONE operation, which Go would otherwise fold at arbitrary
	// precision — see TestOrderedRulesReciprocalRatioGe90Boundary. Keep the
	// multiplication below on variables for the same reason.
	scale := 18446744073709551616.0
	rate := 1.0 / 100.0
	onePercent := uint64(rate * scale)

	admitted := orEval(t, cs, cfg, onePercent-1)
	if admitted.Selected == nil {
		t.Fatalf("the word immediately BELOW the one-percent threshold must admit; got status %q. "+
			"Refusing here means the rate was normalized twice into 0.01%%.", admitted.Status)
	}
	if admitted.RawWordsConsumed != 1 {
		t.Fatalf("a rate of one percent must consume exactly one word; consumed %d. Consuming none means "+
			"the raw 1.0 was read as one hundred percent.", admitted.RawWordsConsumed)
	}

	refused := orEval(t, cs, cfg, onePercent)
	if refused.Selected != nil {
		t.Fatalf("the word AT the one-percent threshold must refuse under a strict comparison; got %+v",
			refused.Selected)
	}
}

// TestOrderedRulesDefaultBoundsVersusExplicitZero pins that an explicit zero
// stays a zero.
//
// The donor's 40 and 60 are per-field serde defaults that fill in a key MISSING
// from a config file. This model accepts a FULLY RESOLVED config, so there is
// no missing key to fill and no defaulting to apply: a supplied zero bound is
// the caller's actual configuration.
//
// The second case pins the other half — a minimum above a maximum is not
// silently reordered. The donor validates the two fields independently and
// never swaps them, so such a config admits nothing.
func TestOrderedRulesDefaultBoundsVersusExplicitZero(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{
		orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
	}

	t.Run("explicit zero bounds are not replaced by 40 and 60", func(t *testing.T) {
		ev := orEval(t, cs, orConfig(nil, 0, 0, 0, 10))
		if ev.Status != predictioneval.StatusNoAttemptInSuppliedPrefix {
			t.Fatalf("a default of [0,0] admits only a share of exactly zero, and neither 0.4 nor 0.6 is "+
				"zero: got %q selecting %+v. Admitting here means 40/60 were substituted for the supplied "+
				"zeros.", ev.Status, ev.Selected)
		}
	})

	t.Run("a zero-share outcome still matches zero bounds", func(t *testing.T) {
		zero := []predictioneval.OrderedRulesCandidate{
			orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 0), orOutcome("B", 10)),
		}
		sel := orMustAdmit(t, orEval(t, zero, orConfig(nil, 0, 0, 0, 10)))
		if sel.OutcomeIdentity != "A" {
			t.Fatalf("an outcome holding zero points has a share of exactly zero and matches [0,0]: got %q",
				sel.OutcomeIdentity)
		}
	})

	t.Run("a minimum above a maximum is not reordered", func(t *testing.T) {
		ev := orEval(t, cs, orConfig(nil, 60, 40, 0, 10))
		if ev.Status != predictioneval.StatusNoAttemptInSuppliedPrefix {
			t.Fatalf("a default of [60,40] admits nothing, because the donor never swaps the bounds: got "+
				"%q selecting %+v", ev.Status, ev.Selected)
		}
	})
}

// TestOrderedRulesPointsTruncateThenCap pins the stake arithmetic's ORDER.
//
// The donor multiplies in float64, TRUNCATES to u32, and only then compares
// against the cap. Rounding instead of truncating, or capping before the cast,
// produces a different number for the same inputs — and a cap of zero means NO
// cap rather than a stake of zero.
func TestOrderedRulesPointsTruncateThenCap(t *testing.T) {
	for _, tc := range []struct {
		name     string
		balance  int64
		maxValue uint32
		percent  float64
		want     uint32
	}{
		// 0.1 * 999 is 99.90000000000001 in binary64. Truncation gives 99;
		// rounding would give 100.
		{"truncates rather than rounds", 999, 0, 10, 99},
		{"a cap of zero is NO cap, not a stake of zero", 1000, 0, 50, 500},
		{"the cap applies after the cast", 1000, 300, 50, 300},
		{"an uncapped value below the cap survives", 1000, 500, 10, 100},
		{"a value equal to the cap yields the cap", 1000, 100, 10, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := []predictioneval.OrderedRulesCandidate{
				orCandidate("c1", 10, orKnownBalance(tc.balance), orOutcome("A", 4), orOutcome("B", 6)),
			}
			cfg := orConfig([]predictioneval.OrderedRule{
				orRule(predictioneval.ComparatorLe, 50, 100, tc.maxValue, tc.percent),
			}, 95, 100, 0, 10)

			ev := orEval(t, cs, cfg)
			orMustAdmit(t, ev)
			if ev.Status != predictioneval.StatusWouldAttempt {
				t.Fatalf("a known balance must produce a computable stake: got %q", ev.Status)
			}
			if ev.Stake.Presence != predictioneval.SuppliedKnown || ev.Stake.Value != tc.want {
				t.Fatalf("stake = %v(%d), want KNOWN(%d)", ev.Stake.Presence, ev.Stake.Value, tc.want)
			}
		})
	}
}

// TestOrderedRulesKnownZeroStakePreservesAdmission pins that zero is an AMOUNT.
//
// A donor-requested stake of zero is what the arithmetic produced; it is not an
// abstention, not a skip, and not an invitation to substitute the pinned
// minimum. Both routes to zero — a zero balance and a zero percentage — keep
// the admission standing.
func TestOrderedRulesKnownZeroStakePreservesAdmission(t *testing.T) {
	cfgWithPercent := func(percent float64) predictioneval.OrderedRulesConfig {
		return orConfig([]predictioneval.OrderedRule{
			orRule(predictioneval.ComparatorLe, 50, 100, 0, percent),
		}, 95, 100, 0, 10)
	}
	for _, tc := range []struct {
		name    string
		balance int64
		percent float64
	}{
		{"a known balance of zero", 0, 10},
		{"a percentage of zero", 1000, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := []predictioneval.OrderedRulesCandidate{
				orCandidate("c1", 10, orKnownBalance(tc.balance), orOutcome("A", 4), orOutcome("B", 6)),
			}
			ev := orEval(t, cs, cfgWithPercent(tc.percent))
			orMustAdmit(t, ev)
			switch {
			case ev.Status != predictioneval.StatusWouldAttempt:
				t.Fatalf("a computable stake of zero is still an attempt: got status %q", ev.Status)
			case ev.Participation != predictioneval.ParticipationAdmitted:
				t.Fatalf("participation = %q, want ADMITTED", ev.Participation)
			case ev.Stake.Presence != predictioneval.SuppliedKnown || ev.Stake.Value != 0:
				t.Fatalf("stake = %v(%d), want KNOWN(0)", ev.Stake.Presence, ev.Stake.Value)
			case ev.Stake.Value == predictioneval.PinnedMinimumStake:
				t.Fatal("the pinned minimum stake was substituted for a computed zero; the donor applies no " +
					"such floor")
			}
		})
	}
}

// TestOrderedRulesBalanceOutsideTheStakeDomainLeavesTheAdmissionStanding pins
// the last route from a bad balance to a confident number.
//
// The donor sizes stakes in u32. A supplied balance outside that domain cannot
// produce a donor stake at all — and in Go the conversion is silent: an int64
// of 2^32 narrows to a uint32 of ZERO, which would look exactly like a
// legitimately computed zero stake.
//
// So the admission stands, the stake stays UNKNOWN, and the balance is neither
// clamped into range nor allowed to wrap. A clamped stake would be a number the
// donor never would have produced.
func TestOrderedRulesBalanceOutsideTheStakeDomainLeavesTheAdmissionStanding(t *testing.T) {
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 50, 100, 0, 10),
	}, 95, 100, 0, 10)

	for _, tc := range []struct {
		name    string
		balance int64
	}{
		// 2^32 narrows to a uint32 zero, and 2^32+50 narrows to 50 — both
		// perfectly plausible-looking stakes.
		{"one past the u32 ceiling", 4294967296},
		{"far past the ceiling", 4294967296 + 500},
		{"negative", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := []predictioneval.OrderedRulesCandidate{
				orCandidate("c1", 10, orKnownBalance(tc.balance), orOutcome("A", 4), orOutcome("B", 6)),
			}
			ev := orEval(t, cs, cfg)
			orMustAdmit(t, ev)
			switch {
			case ev.Status != predictioneval.StatusParticipationAdmittedStakeUnknown:
				t.Fatalf("status = %q, want PARTICIPATION_ADMITTED_STAKE_UNKNOWN", ev.Status)
			case ev.Participation != predictioneval.ParticipationAdmitted:
				t.Fatalf("participation = %q, want ADMITTED: the CHOICE is still known", ev.Participation)
			case ev.Reason != predictioneval.ReasonBalanceOutOfDomain:
				t.Fatalf("reason = %q, want BALANCE_OUT_OF_U32_DOMAIN", ev.Reason)
			case ev.Stake.Presence == predictioneval.SuppliedKnown:
				t.Fatalf("the stake must stay unknown; got KNOWN(%d) — a narrowing conversion produced a "+
					"plausible number from a balance outside the donor's domain", ev.Stake.Value)
			}
			if len(ev.Visits) == 0 ||
				ev.Visits[0].BalanceUse != predictioneval.BalanceRequiredButOutOfDomain {
				t.Fatalf("the visit must record REQUIRED_BUT_OUT_OF_DOMAIN, got %+v", ev.Visits)
			}
		})
	}

	// The ceiling itself is INSIDE the domain and sizes normally, so the cases
	// above are pinning the boundary rather than rejecting large balances.
	cs := []predictioneval.OrderedRulesCandidate{
		orCandidate("c1", 10, orKnownBalance(4294967295), orOutcome("A", 4), orOutcome("B", 6)),
	}
	ev := orEval(t, cs, cfg)
	if ev.Status != predictioneval.StatusWouldAttempt || ev.Stake.Presence != predictioneval.SuppliedKnown {
		t.Fatalf("a balance of exactly the u32 maximum is in domain: status %q stake %+v",
			ev.Status, ev.Stake)
	}
}

// TestOrderedRulesConfigOutsideTheAdmittedNumericDomainIsRefused pins the
// domain guard on every raw percentage.
//
// The donor validates each percentage field independently against 0..100. A
// value outside that range — or a non-finite one — is not normalized, not
// clamped and not treated as a zero: it is a config this model will not
// evaluate, reported as a typed refusal rather than as a mechanism result.
//
// NaN matters most, because it is the value that would otherwise pass silently:
// every comparison against it is false, so an unguarded NaN threshold would
// simply never match and the run would report a confident
// NO_ATTEMPT_IN_SUPPLIED_PREFIX over a configuration nobody could have written.
func TestOrderedRulesConfigOutsideTheAdmittedNumericDomainIsRefused(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{
		orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
	}
	nan, inf := math.NaN(), math.Inf(1)

	for _, tc := range []struct {
		name string
		cfg  predictioneval.OrderedRulesConfig
	}{
		{"threshold above one hundred", orConfig([]predictioneval.OrderedRule{
			orRule(predictioneval.ComparatorLe, 150, 100, 0, 10)}, 95, 100, 0, 10)},
		{"negative threshold", orConfig([]predictioneval.OrderedRule{
			orRule(predictioneval.ComparatorLe, -1, 100, 0, 10)}, 95, 100, 0, 10)},
		{"NaN threshold", orConfig([]predictioneval.OrderedRule{
			orRule(predictioneval.ComparatorLe, nan, 100, 0, 10)}, 95, 100, 0, 10)},
		{"NaN attempt rate", orConfig([]predictioneval.OrderedRule{
			orRule(predictioneval.ComparatorLe, 50, nan, 0, 10)}, 95, 100, 0, 10)},
		{"infinite attempt rate", orConfig([]predictioneval.OrderedRule{
			orRule(predictioneval.ComparatorLe, 50, inf, 0, 10)}, 95, 100, 0, 10)},
		{"points percentage above one hundred", orConfig([]predictioneval.OrderedRule{
			orRule(predictioneval.ComparatorLe, 50, 100, 0, 101)}, 95, 100, 0, 10)},
		{"unknown comparator", orConfig([]predictioneval.OrderedRule{
			orRule(predictioneval.OrderedRuleComparator("Lt"), 50, 100, 0, 10)}, 95, 100, 0, 10)},
		{"NaN default bound", orConfig(nil, nan, 100, 0, 10)},
		{"default bound above one hundred", orConfig(nil, 0, 101, 0, 10)},
		{"default points percentage negative", orConfig(nil, 0, 100, 0, -5)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := orEval(t, cs, tc.cfg)
			if ev.Status != predictioneval.StatusRefused ||
				ev.Reason != predictioneval.ReasonConfigOutOfDomain {
				t.Fatalf("status %q reason %q, want REFUSED / CONFIG_OUT_OF_ADMITTED_DOMAIN "+
					"(selected %+v)", ev.Status, ev.Reason, ev.Selected)
			}
			if ev.Selected != nil || ev.RawWordsConsumed != 0 {
				t.Fatal("a refused config evaluates nothing and spends nothing")
			}
		})
	}

	// Non-vacuity: the boundary values themselves are INSIDE the domain, so
	// these cases reject out-of-range values rather than rejecting extremes.
	for _, ok := range []predictioneval.OrderedRulesConfig{
		orConfig([]predictioneval.OrderedRule{
			orRule(predictioneval.ComparatorLe, 100, 100, 0, 100)}, 0, 100, 0, 0),
		orConfig([]predictioneval.OrderedRule{
			orRule(predictioneval.ComparatorGe, 0, 0, 0, 0)}, 0, 0, 0, 0),
	} {
		if ev := orEval(t, cs, ok, orWordAdmit); ev.Status == predictioneval.StatusRefused {
			t.Fatalf("a config at the domain's own boundaries must be accepted: reason %q", ev.Reason)
		}
	}
}

// TestOrderedRulesNegativeOrOverflowingPoolPointsAreUnknownNotZero pins the
// deliberate divergence from the donor's unchecked arithmetic.
//
// The donor folds i64 points with a plain +, so a negative entry yields a
// negative share it goes on to compare, and a large sum wraps. Neither is
// something this model may reproduce: a wrapped total is undefined behaviour to
// lean on, and a negative share cannot have come from a real pool. Both are
// reported as a typed unknown — and never normalized to zero, which would let a
// corrupted vector produce a confident decision.
func TestOrderedRulesNegativeOrOverflowingPoolPointsAreUnknownNotZero(t *testing.T) {
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 100, 100, 0, 10),
	}, 0, 100, 0, 10)

	for _, tc := range []struct {
		name       string
		a, b       int64
		wantReason string
	}{
		{"a negative outcome", -5, 10, predictioneval.ReasonOutcomePointsOutOfDomain},
		{"a sum past int64", math.MaxInt64, 1, predictioneval.ReasonPoolSumOverflow},
		{"two maxima", math.MaxInt64, math.MaxInt64, predictioneval.ReasonPoolSumOverflow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := []predictioneval.OrderedRulesCandidate{
				orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", tc.a), orOutcome("B", tc.b)),
			}
			ev := orEval(t, cs, cfg, orWordAdmit)
			if ev.Status != predictioneval.StatusUnknownInput || ev.Reason != tc.wantReason {
				t.Fatalf("status %q reason %q, want UNKNOWN_INPUT / %q", ev.Status, ev.Reason,
					tc.wantReason)
			}
			if ev.Selected != nil {
				t.Fatalf("nothing may be decided from an unsound pool; got %+v", ev.Selected)
			}
			if len(ev.Visits) != 1 || ev.Visits[0].PoolTotalKnown {
				t.Fatalf("the visit must record the total as NOT known: %+v", ev.Visits)
			}
		})
	}

	// Non-vacuity: this config admits immediately on a sound pool, so the stops
	// above are caused by the arithmetic and not by an unreachable rule.
	sound := []predictioneval.OrderedRulesCandidate{
		orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
	}
	if ev := orEval(t, sound, cfg, orWordAdmit); ev.Selected == nil {
		t.Fatalf("the control run must admit; got %q", ev.Status)
	}
}

// TestOrderedRulesFewerThanTwoOutcomesIsDeclinedWithoutADraw pins the donor's
// pre-check: a pool it cannot compare is declined before any rule is read, so
// no entropy is spent on it and the traversal moves on.
func TestOrderedRulesFewerThanTwoOutcomesIsDeclinedWithoutADraw(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{
		orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4)),
		orCandidate("c2", 20, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
	}
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 50, 50, 0, 10),
	}, 95, 100, 0, 10)

	ev := orEval(t, cs, cfg, orWordAdmit)
	sel := orMustAdmit(t, ev)
	if sel.CandidateIdentity != "c2" {
		t.Fatalf("the one-outcome candidate must be declined and the traversal continue: admitted on %q",
			sel.CandidateIdentity)
	}
	if ev.RawWordsConsumed != 1 {
		t.Fatalf("the declined candidate must spend no entropy: consumed %d, want 1", ev.RawWordsConsumed)
	}
	if len(ev.Visits) == 0 || ev.Visits[0].Verdict != predictioneval.CandidateTooFewOutcomes {
		t.Fatalf("the first visit must record TOO_FEW_OUTCOMES, got %+v", ev.Visits)
	}
}
