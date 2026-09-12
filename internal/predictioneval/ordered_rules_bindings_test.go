package predictioneval_test

// THE BINDINGS, COMPONENT BY COMPONENT.
//
// The recombination test in the stream suite proves that the four digests move
// when a WHOLE component is swapped. That is necessary and not sufficient: a
// digest can cover most of a component and still miss a field, and the swap
// test would never notice because some OTHER field it does cover moved at the
// same time.
//
// So each binding is exercised against a change to ONE field, chosen to be a
// field a coarser test would step over.

import (
	"errors"
	"math"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
)

func orBaseCandidates() []predictioneval.OrderedRulesCandidate {
	return []predictioneval.OrderedRulesCandidate{orTwoOutcomePool("c1", 10)}
}

func orBaseConfig() predictioneval.OrderedRulesConfig {
	return orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 50, 50, 500, 10),
	}, 95, 100, 250, 5)
}

// TestOrderedRulesConfigDigestCoversEveryConfigField pins the config binding
// field by field.
//
// A cap, a points percentage and a default bound all change what a run would
// stake without changing which outcome it picks — so a digest that skipped one
// would let two materially different configurations produce the same evidence.
func TestOrderedRulesConfigDigestCoversEveryConfigField(t *testing.T) {
	base := orBaseConfig()
	baseDigest := predictioneval.EvaluateOrderedRules(
		orProject(t, orBaseCandidates(), nil), base, orDraws(orWordAdmit)).ConfigDigest

	for _, tc := range []struct {
		name   string
		mutate func(*predictioneval.OrderedRulesConfig)
	}{
		{"the rule's cap", func(c *predictioneval.OrderedRulesConfig) { c.Detailed[0].Points.MaxValue = 501 }},
		{"the rule's points percentage", func(c *predictioneval.OrderedRulesConfig) { c.Detailed[0].Points.RawPercent = 11 }},
		{"the rule's threshold", func(c *predictioneval.OrderedRulesConfig) { c.Detailed[0].RawThresholdPercent = 51 }},
		{"the rule's attempt rate", func(c *predictioneval.OrderedRulesConfig) { c.Detailed[0].RawAttemptRatePercent = 51 }},
		{"the rule's comparator", func(c *predictioneval.OrderedRulesConfig) { c.Detailed[0].Comparator = predictioneval.ComparatorGe }},
		{"the default's minimum", func(c *predictioneval.OrderedRulesConfig) { c.Default.RawMinPercent = 94 }},
		{"the default's maximum", func(c *predictioneval.OrderedRulesConfig) { c.Default.RawMaxPercent = 99 }},
		{"the default's cap", func(c *predictioneval.OrderedRulesConfig) { c.Default.Points.MaxValue = 251 }},
		{"the default's points percentage", func(c *predictioneval.OrderedRulesConfig) { c.Default.Points.RawPercent = 6 }},
		{"the config's identity", func(c *predictioneval.OrderedRulesConfig) { c.ConfigID = "another-config" }},
		{"an added rule", func(c *predictioneval.OrderedRulesConfig) {
			c.Detailed = append(append([]predictioneval.OrderedRule(nil), c.Detailed...),
				orRule(predictioneval.ComparatorGe, 90, 100, 0, 1))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			cfg.Detailed = append([]predictioneval.OrderedRule(nil), base.Detailed...)
			tc.mutate(&cfg)
			got := predictioneval.EvaluateOrderedRules(
				orProject(t, orBaseCandidates(), nil), cfg, orDraws(orWordAdmit)).ConfigDigest
			if got == baseDigest {
				t.Fatalf("changing %s left the config binding unchanged; two different configurations "+
					"would produce indistinguishable evidence", tc.name)
			}
		})
	}
}

// TestOrderedRulesStreamDigestCoversTheBoundaryAndThePresences pins the stream
// binding over the fields that decide what a traversal may READ.
//
// The boundary is the sharpest case: two streams can hold exactly the same
// surviving candidates and still be different evidence, because one of them is
// a prefix of a stream a placement call cut short.
func TestOrderedRulesStreamDigestCoversTheBoundaryAndThePresences(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{orTwoOutcomePool("c1", 10)}
	base := orProject(t, cs, nil).SelectionDigest

	withBoundary := orProject(t, cs, []predictioneval.OrderedRulesIntervention{
		orIntervention("call-1", 20, predictioneval.InterventionAutoCallStarted,
			predictioneval.RelevanceProven, "boundary"),
	}).SelectionDigest
	if withBoundary == base {
		t.Fatal("two streams holding the same surviving candidate — one of them cut short by a real " +
			"placement call — carry the same stream binding")
	}

	// The boundary's own identity and basis are part of it too.
	a := orProject(t, cs, []predictioneval.OrderedRulesIntervention{
		orIntervention("call-a", 20, predictioneval.InterventionAutoCallStarted,
			predictioneval.RelevanceProven, "boundary"),
	}).SelectionDigest
	b := orProject(t, cs, []predictioneval.OrderedRulesIntervention{
		orIntervention("call-b", 20, predictioneval.InterventionAutoCallStarted,
			predictioneval.RelevanceProven, "boundary"),
	}).SelectionDigest
	if a == b {
		t.Fatal("two different placement calls at the same position produce the same stream binding")
	}

	// And a presence: a balance the caller does not have versus one it does.
	known := orCandidate("c1", 10, orKnownBalance(0), orOutcome("A", 4), orOutcome("B", 6))
	missing := orCandidate("c1", 10, orMissingBalance(), orOutcome("A", 4), orOutcome("B", 6))
	if orProject(t, []predictioneval.OrderedRulesCandidate{known}, nil).SelectionDigest ==
		orProject(t, []predictioneval.OrderedRulesCandidate{missing}, nil).SelectionDigest {
		t.Fatal("a KNOWN balance of zero and a MISSING balance carry the same stream binding; those are " +
			"the two things this model exists to keep apart")
	}
}

// TestOrderedRulesStreamDigestBindsTheSourcesOwnQualifications pins the
// stream binding over the fields that describe how much of the source is
// actually there.
//
// Coverage and its detail are not decoration: they decide whether "no
// intervention was supplied" may be read as "none occurred". Two streams with
// identical candidates and different coverage are different evidence.
func TestOrderedRulesStreamDigestBindsTheSourcesOwnQualifications(t *testing.T) {
	project := func(cov predictioneval.OrderedRulesCoverage, detail string) string {
		src := orSource(orBaseCandidates(), nil)
		src.Scope.Coverage = cov
		src.Scope.CoverageDetail = detail
		stream, err := predictioneval.ProjectOrderedRulesStream(src, orAdmission())
		if err != nil {
			t.Fatalf("project: %v", err)
		}
		return stream.SelectionDigest
	}
	complete := project(predictioneval.CoverageCompleteDeclared, "")
	gaps := project(predictioneval.CoverageGapsPresent, "")
	truncated := project(predictioneval.CoverageTruncatedPrefix, "")

	if complete == gaps || complete == truncated || gaps == truncated {
		t.Fatalf("three different coverage declarations must produce three different stream bindings: "+
			"%s %s %s", complete[:16], gaps[:16], truncated[:16])
	}
	if project(predictioneval.CoverageGapsPresent, "two frames lost to a reconnect") == gaps {
		t.Fatal("the coverage DETAIL is retained in the stream and must bind too")
	}
}

func TestOrderedRulesDigestBindsTheReasonBehindAnAbsentValue(t *testing.T) {
	withReason := func(reason string) string {
		c := orCandidate("c1", 10,
			predictioneval.SuppliedInt64{Presence: predictioneval.SuppliedMissing, Reason: reason},
			orOutcome("A", 4), orOutcome("B", 6))
		return orProject(t, []predictioneval.OrderedRulesCandidate{c}, nil).SelectionDigest
	}
	a := withReason("this channel frame carries no balance")
	b := withReason("the stored balance could not be decoded")
	if a == b {
		t.Fatal("two absent balances with different explanations carry the same binding; the reason " +
			"travels into the result, so it must travel into the evidence for it")
	}

	// And a KNOWN zero must not collide with an absent one, which is the same
	// distinction one level down.
	known := orCandidate("c1", 10, orKnownBalance(0), orOutcome("A", 4), orOutcome("B", 6))
	if orProject(t, []predictioneval.OrderedRulesCandidate{known}, nil).SelectionDigest == a {
		t.Fatal("a KNOWN balance of zero and a MISSING balance carry the same binding")
	}
}

// TestOrderedRulesConsumedDigestBindsTheDeclaredCoverage pins coverage as
// evidence about the prefix rather than decoration.
//
// The same consumed prefix read from a source that admits its beginning is
// missing is not the same evidence as one read from a source claiming to be
// complete: the second asserts no earlier intervention occurred and the first
// cannot. A binding that ignored the difference would let the weaker run be
// presented as the stronger one.
func TestOrderedRulesConsumedDigestBindsTheDeclaredCoverage(t *testing.T) {
	run := func(cov predictioneval.OrderedRulesCoverage) predictioneval.OrderedRulesEvaluation {
		src := orSource(orBaseCandidates(), nil)
		src.Scope.Coverage = cov
		stream, err := predictioneval.ProjectOrderedRulesStream(src, orAdmission())
		if err != nil {
			t.Fatalf("project: %v", err)
		}
		return predictioneval.EvaluateOrderedRules(stream, orBaseConfig(), orDraws(orWordAdmit))
	}
	complete := run(predictioneval.CoverageCompleteDeclared)
	truncated := run(predictioneval.CoverageTruncatedPrefix)

	if complete.Status != truncated.Status {
		t.Fatalf("this case needs the two runs to decide alike: %q and %q",
			complete.Status, truncated.Status)
	}
	if complete.ConsumedInputDigest == truncated.ConsumedInputDigest {
		t.Fatal("a run over a source declaring a TRUNCATED prefix carries the same consumed-prefix " +
			"binding as one declaring COMPLETE coverage")
	}
}

// TestOrderedRulesConsumedDigestIgnoresCandidatesTheTraversalNeverReached pins
// the other direction of the same binding.
//
// Candidates that survived the boundary but sat past the stop did not
// contribute to the result, so they must not appear in the evidence for it —
// otherwise "later facts cannot change an earlier result" fails on the digest
// even when the decision itself is stable.
func TestOrderedRulesConsumedDigestIgnoresCandidatesTheTraversalNeverReached(t *testing.T) {
	short := predictioneval.EvaluateOrderedRules(
		orProject(t, orBaseCandidates(), nil), orBaseConfig(), orDraws(orWordAdmit))
	long := predictioneval.EvaluateOrderedRules(
		orProject(t, append(orBaseCandidates(),
			orCandidate("c2", 20, orKnownBalance(7), orOutcome("A", 9), orOutcome("B", 1)),
			orCandidate("c3", 30, orKnownBalance(8), orOutcome("A", 1), orOutcome("B", 9))), nil),
		orBaseConfig(), orDraws(orWordAdmit))

	switch {
	case short.Status != predictioneval.StatusWouldAttempt || long.Status != predictioneval.StatusWouldAttempt:
		t.Fatalf("both runs must stop on the first candidate: %q and %q", short.Status, long.Status)
	case short.CandidatesConsumed != 1 || long.CandidatesConsumed != 1:
		t.Fatalf("consumed %d and %d candidates, want 1 each",
			short.CandidatesConsumed, long.CandidatesConsumed)
	case short.ConsumedInputDigest != long.ConsumedInputDigest:
		t.Fatal("the consumed-prefix binding moved when candidates the traversal never reached were " +
			"appended; it must witness only what was read")
	case short.StreamDigest == long.StreamDigest:
		t.Fatal("the whole-stream binding must MOVE when the stream grows, or the stability above is " +
			"not distinguishing anything")
	}
}

// TestOrderedRulesAllZeroPoolProducesZeroSharesNotNaN pins the guard on the
// division.
//
// A pool in which every outcome holds zero points is legal — the donor's own
// zero check exists for it — and without that check the share would be 0/0.
// NaN then compares false against every threshold, so the run would report a
// confident NO_ATTEMPT over arithmetic that never happened.
func TestOrderedRulesAllZeroPoolProducesZeroSharesNotNaN(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{
		orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 0), orOutcome("B", 0)),
	}
	// A default of [0,0] matches only a share of exactly zero. If the shares
	// were NaN, nothing would match and this would fall through.
	ev := orEval(t, cs, orConfig(nil, 0, 0, 0, 10))
	sel := orMustAdmit(t, ev)
	if sel.OutcomeIdentity != "A" {
		t.Fatalf("selected %q, want \"A\"", sel.OutcomeIdentity)
	}
	if math.IsNaN(math.Float64frombits(uint64(sel.ShareBits))) ||
		uint64(sel.ShareBits) != math.Float64bits(0) {
		t.Fatalf("the recorded share is %#016x, want an exact zero; an all-zero pool must not divide "+
			"zero by zero", uint64(sel.ShareBits))
	}
	for _, e := range ev.Trace {
		if math.IsNaN(math.Float64frombits(uint64(e.ShareBits))) {
			t.Fatalf("a NaN share reached the trace at outcome %d", e.OutcomeIndex)
		}
	}
}

// TestOrderedRulesEqualPositionAmbiguousBoundaryIsUpgradedByProvenEvidence pins
// the other half of the same-position tie-break.
//
// When a proven and an unprovable intervention sit at the SAME position the cut
// does not move — but the label does, and it should improve. Reporting the
// stronger evidence is the honest choice, and it is not the one a naive
// first-wins scan would make.
func TestOrderedRulesEqualPositionAmbiguousBoundaryIsUpgradedByProvenEvidence(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{orTwoOutcomePool("c1", 10)}
	ambiguous := orIntervention("call-ambiguous", 20, predictioneval.InterventionManualCallStarted,
		predictioneval.RelevanceAmbiguous, "association could not be established")
	proven := orIntervention("call-proven", 20, predictioneval.InterventionAutoCallStarted,
		predictioneval.RelevanceProven, "association proven")

	got := orProject(t, cs, []predictioneval.OrderedRulesIntervention{ambiguous, proven})
	if got.Cutoff.Position != 20 {
		t.Fatalf("the cut must stay at 20: %+v", got.Cutoff)
	}
	if got.Cutoff.Basis != predictioneval.CutoffFactualIntervention {
		t.Fatalf("basis = %q, want FACTUAL_INTERVENTION: at the same position the proven call is the "+
			"better evidence for a cut that happens either way", got.Cutoff.Basis)
	}
	if containsSubstring(got.Qualifications, "CUTOFF_FROM_AMBIGUOUS_ASSOCIATION") {
		t.Fatalf("a cut backed by proven evidence must not be qualified as conservative: %v",
			got.Qualifications)
	}
}

// TestOrderedRulesKnownValuesMustCarryProvenance pins the refusal that keeps
// SUPPLIED from quietly becoming anonymous.
//
// A known number with no account of where it came from is exactly the shape of
// a value someone typed in. It is refused rather than accepted and reported as
// KNOWN, because the result would then carry a figure with no traceable origin.
func TestOrderedRulesKnownValuesMustCarryProvenance(t *testing.T) {
	t.Run("a balance", func(t *testing.T) {
		c := orCandidate("c1", 10,
			predictioneval.SuppliedInt64{Presence: predictioneval.SuppliedKnown, Value: 1000},
			orOutcome("A", 4), orOutcome("B", 6))
		if _, err := predictioneval.ProjectOrderedRulesStream(
			orSource([]predictioneval.OrderedRulesCandidate{c}, nil), orAdmission()); err == nil {
			t.Fatal("a KNOWN balance with no provenance must be refused")
		}
	})
	t.Run("an outcome's points", func(t *testing.T) {
		o := predictioneval.OrderedRulesOutcome{Identity: "A",
			Points: predictioneval.SuppliedInt64{Presence: predictioneval.SuppliedKnown, Value: 4}}
		c := orCandidate("c1", 10, orKnownBalance(1000), o, orOutcome("B", 6))
		if _, err := predictioneval.ProjectOrderedRulesStream(
			orSource([]predictioneval.OrderedRulesCandidate{c}, nil), orAdmission()); err == nil {
			t.Fatal("KNOWN points with no provenance must be refused")
		}
	})
}

// TestOrderedRulesResourceBoundsAreExactAtTheirOwnBoundary pins each limit at
// the value itself and one past it.
//
// An off-by-one in a refusal boundary is invisible in ordinary use and changes
// exactly which inputs the model claims to have evaluated whole.
func TestOrderedRulesResourceBoundsAreExactAtTheirOwnBoundary(t *testing.T) {
	mkCandidates := func(n int) []predictioneval.OrderedRulesCandidate {
		cs := make([]predictioneval.OrderedRulesCandidate, n)
		for i := range cs {
			cs[i] = orTwoOutcomePool("c"+itoaTest(i), int64(i+1))
		}
		return cs
	}
	mkOutcomes := func(n int) []predictioneval.OrderedRulesOutcome {
		outs := make([]predictioneval.OrderedRulesOutcome, n)
		for i := range outs {
			outs[i] = orOutcome("o"+itoaTest(i), int64(i+1))
		}
		return outs
	}

	t.Run("candidates at the ceiling are accepted", func(t *testing.T) {
		if _, err := predictioneval.ProjectOrderedRulesStream(
			orSource(mkCandidates(predictioneval.MaxOrderedRulesCandidates), nil), orAdmission()); err != nil {
			t.Fatalf("exactly the ceiling must be accepted: %v", err)
		}
	})
	t.Run("outcomes at the ceiling are accepted", func(t *testing.T) {
		c := orCandidate("c1", 10, orKnownBalance(1000), mkOutcomes(predictioneval.MaxOrderedRulesOutcomes)...)
		if _, err := predictioneval.ProjectOrderedRulesStream(
			orSource([]predictioneval.OrderedRulesCandidate{c}, nil), orAdmission()); err != nil {
			t.Fatalf("exactly the ceiling must be accepted: %v", err)
		}
	})
	t.Run("an identifier at the ceiling is accepted", func(t *testing.T) {
		id := make([]byte, predictioneval.MaxOrderedRulesIdentifierBytes)
		for i := range id {
			id[i] = 'x'
		}
		c := orTwoOutcomePool(string(id), 10)
		if _, err := predictioneval.ProjectOrderedRulesStream(
			orSource([]predictioneval.OrderedRulesCandidate{c}, nil), orAdmission()); err != nil {
			t.Fatalf("exactly the ceiling must be accepted: %v", err)
		}
		if _, err := predictioneval.ProjectOrderedRulesStream(
			orSource([]predictioneval.OrderedRulesCandidate{orTwoOutcomePool(string(id)+"x", 10)}, nil),
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesOverBound) {
			t.Fatalf("one byte past the ceiling must be refused: %v", err)
		}
	})
	t.Run("rules at the ceiling are accepted", func(t *testing.T) {
		rules := make([]predictioneval.OrderedRule, predictioneval.MaxOrderedRulesRules)
		for i := range rules {
			rules[i] = orRule(predictioneval.ComparatorGe, 100, 100, 0, 10)
		}
		ev := predictioneval.EvaluateOrderedRules(orProject(t, orBaseCandidates(), nil),
			orConfig(rules, 95, 100, 0, 10), orDraws())
		if ev.Status == predictioneval.StatusRefused {
			t.Fatalf("exactly the ceiling must be evaluated: reason %q", ev.Reason)
		}
	})
}

// TestOrderedRulesWholeInputChecksAreRefusalsNotMechanismResults pins the one
// place a post-boundary fact CAN change the outcome, and pins what it changes
// it to.
//
// Contract and resource checks run over everything supplied, boundary or not.
// So a malformed candidate past the boundary turns a projection into a refusal
// — and that refusal is the point: it says "this package will not evaluate this
// input", never "the mechanism decided differently". A decision can never be
// reached through this path, which is exactly why it does not weaken the
// no-look-ahead guarantee.
func TestOrderedRulesWholeInputChecksAreRefusalsNotMechanismResults(t *testing.T) {
	ins := []predictioneval.OrderedRulesIntervention{
		orIntervention("call-1", 15, predictioneval.InterventionAutoCallStarted,
			predictioneval.RelevanceProven, "boundary"),
	}
	good := orBaseCandidates()

	// Baseline: the pre-boundary candidate decides.
	base := predictioneval.EvaluateOrderedRules(orProject(t, good, ins), orBaseConfig(),
		orDraws(orWordAdmit))
	if base.Status != predictioneval.StatusWouldAttempt {
		t.Fatalf("status = %q, want WOULD_ATTEMPT", base.Status)
	}

	// A post-boundary candidate that breaks the contract makes the PROJECTION
	// refuse. It does not reach the mechanism and cannot change a decision.
	bad := orTwoOutcomePool("post", 30)
	bad.EpisodeMembership = predictioneval.MembershipUnknown
	_, err := predictioneval.ProjectOrderedRulesStream(
		orSource(append(append([]predictioneval.OrderedRulesCandidate(nil), good...), bad), ins),
		orAdmission())
	if !errors.Is(err, predictioneval.ErrOrderedRulesMembershipUnproven) {
		t.Fatalf("got %v, want an unproven-membership refusal", err)
	}

	// A WELL-FORMED post-boundary candidate changes nothing at all, which is
	// the guarantee that actually matters.
	okPost := orTwoOutcomePool("post", 30)
	later := predictioneval.EvaluateOrderedRules(
		orProject(t, append(append([]predictioneval.OrderedRulesCandidate(nil), good...), okPost), ins),
		orBaseConfig(), orDraws(orWordAdmit))
	// The status is checked FIRST and on its own. Folding it into the same
	// condition short-circuits before `*later.Selected` is evaluated there, and
	// the failure message then dereferences it anyway — so the one regression
	// this case exists to report (a post-boundary fact turning an admission into
	// a non-admitting status) would arrive as a nil-pointer panic instead of the
	// diagnostic.
	if later.Status != base.Status {
		t.Fatalf("a well-formed post-boundary fact changed the status: before = %q, after = %q",
			base.Status, later.Status)
	}
	if later.Selected == nil {
		t.Fatal("a well-formed post-boundary fact removed the selection entirely")
	}
	if *later.Selected != *base.Selected || later.ConsumedInputDigest != base.ConsumedInputDigest {
		t.Fatalf("a well-formed post-boundary fact changed the decision:\n before = %+v\n  after = %+v",
			*base.Selected, *later.Selected)
	}
}

// TestOrderedRulesReportedCountersMatchTheTraversal pins the counters a caller
// would use to check a run's cost.
//
// They are reported on every result and nothing else asserted them, so a
// miscount would travel unchallenged into whatever a reader concluded from it.
func TestOrderedRulesReportedCountersMatchTheTraversal(t *testing.T) {
	// [A:4, B:6] against two Ge rules that never match either share, and a
	// default out of reach: two outcomes, two rules each, two default checks.
	cs := []predictioneval.OrderedRulesCandidate{orTwoOutcomePool("c1", 10)}
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorGe, 90, 100, 0, 10),
		orRule(predictioneval.ComparatorGe, 95, 100, 0, 10),
	}, 96, 100, 0, 10)

	ev := orEval(t, cs, cfg)
	switch {
	case ev.Status != predictioneval.StatusNoAttemptInSuppliedPrefix:
		t.Fatalf("status = %q, want NO_ATTEMPT_IN_SUPPLIED_PREFIX", ev.Status)
	case ev.OutcomesConsidered != 2:
		t.Fatalf("outcomes considered = %d, want 2", ev.OutcomesConsidered)
	case ev.RulesConsidered != 4:
		t.Fatalf("rules considered = %d, want 4 (two rules across two outcomes)", ev.RulesConsidered)
	case ev.BernoulliEvaluations != 0:
		t.Fatalf("no comparator matched, so no draw ran: %d", ev.BernoulliEvaluations)
	case len(ev.Trace) != 6:
		t.Fatalf("trace has %d entries, want 6: four comparator steps and two default checks",
			len(ev.Trace))
	}
}
