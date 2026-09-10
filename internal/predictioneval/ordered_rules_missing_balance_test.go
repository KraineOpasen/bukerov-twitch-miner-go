package predictioneval_test

// THE FOUR MISSING-BALANCE VECTORS.
//
// A channel candidate carries no balance. That single gap has FOUR distinct
// correct behaviours depending on what the mechanism did on that candidate, and
// the whole point of these tests is that no two of them collapse into one:
//
//	MB1  a rule matched, its draw FAILED, the default missed
//	     → the balance was never needed. Keep scanning.
//	MB2  nothing matched at all
//	     → the balance was never needed. Keep scanning.
//	MB3  a rule matched and its draw SUCCEEDED
//	     → the balance is needed and absent. Stop, with the choice KNOWN and
//	       the stake UNKNOWN.
//	MB4  the default matched
//	     → the same stop, having consumed no draw.
//
// The failures these rule out are the ones a reasonable implementer reaches
// for. Rejecting a candidate up front because its balance is missing loses MB1
// and MB2's opportunities entirely. Substituting a zero turns MB3 and MB4 into
// a confident financial claim the data does not support. Borrowing a balance
// from a neighbouring record backdates information the candidate never held.
//
// The fixture is the audit's: C1 at position 10 holding [A:4, B:6]; C2 at
// position 20; a factual placement call at position 30 bounding both. Rule and
// default alike stake ten percent capped at five hundred — deliberately, so
// that a substituted zero balance would still produce a confident-looking
// stake of zero rather than an obvious failure.

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
)

// mbCall is the factual intervention that bounds every vector below.
func mbCall() []predictioneval.OrderedRulesIntervention {
	return []predictioneval.OrderedRulesIntervention{{
		Identity:  "call-1",
		Position:  30,
		Kind:      predictioneval.InterventionAutoCallStarted,
		Relevance: predictioneval.RelevanceProven,
		Detail:    "automatic placement call started",
	}}
}

// mbEval runs one vector: two balance-less candidates, a bounding call, and an
// exact word list.
func mbEval(t *testing.T, c2Outcomes []predictioneval.OrderedRulesOutcome,
	cfg predictioneval.OrderedRulesConfig, words ...uint64) predictioneval.OrderedRulesEvaluation {
	t.Helper()
	cs := []predictioneval.OrderedRulesCandidate{
		orCandidate("C1", 10, orMissingBalance(), orOutcome("A", 4), orOutcome("B", 6)),
		orCandidate("C2", 20, orMissingBalance(), c2Outcomes...),
	}
	return predictioneval.EvaluateOrderedRules(orProject(t, cs, mbCall()), cfg, orDraws(words...))
}

// mbPointsRule builds the vectors' shared ten-percent, capped-at-500 sizing.
func mbPointsRule(threshold float64) predictioneval.OrderedRule {
	return orRule(predictioneval.ComparatorLe, threshold, 50, 500, 10)
}

func mbConfig(detailed []predictioneval.OrderedRule, defMin, defMax float64) predictioneval.OrderedRulesConfig {
	return orConfig(detailed, defMin, defMax, 500, 10)
}

// mbVisit finds one candidate's visit record.
func mbVisit(t *testing.T, ev predictioneval.OrderedRulesEvaluation, id string) predictioneval.OrderedRulesCandidateVisit {
	t.Helper()
	for _, v := range ev.Visits {
		if v.CandidateIdentity == id {
			return v
		}
	}
	t.Fatalf("no visit recorded for candidate %q; visits = %+v", id, ev.Visits)
	return predictioneval.OrderedRulesCandidateVisit{}
}

// mbAssertUnknownStakeStop is the shared MB3/MB4 assertion: participation KNOWN,
// stake UNKNOWN, and never a zero.
func mbAssertUnknownStakeStop(t *testing.T, ev predictioneval.OrderedRulesEvaluation, outcome string) {
	t.Helper()
	sel := orMustAdmit(t, ev)
	switch {
	case ev.Status != predictioneval.StatusParticipationAdmittedStakeUnknown:
		t.Fatalf("status = %q, want PARTICIPATION_ADMITTED_STAKE_UNKNOWN", ev.Status)
	case ev.Participation != predictioneval.ParticipationAdmitted:
		t.Fatalf("participation = %q, want ADMITTED: the mechanism's choice IS known", ev.Participation)
	case sel.OutcomeIdentity != outcome:
		t.Fatalf("chose outcome %q, want %q", sel.OutcomeIdentity, outcome)
	case ev.Stake.Presence == predictioneval.SuppliedKnown:
		t.Fatalf("the stake must stay UNKNOWN; got KNOWN(%d). A missing balance is never converted to a "+
			"zero, and a zero stake would read as a financial claim the data does not support.",
			ev.Stake.Value)
	case ev.Stake.Value != 0:
		t.Fatalf("an unknown stake carries no value; got %d", ev.Stake.Value)
	}
}

// TestOrderedRulesMissingBalanceFailedDrawContinues is MB1.
//
// C1's rule matches and its draw FAILS; its default misses. The balance was
// never consulted, so the scan continues to C2 — which admits on the second
// word and stops there with an unknown stake. Two words in total.
func TestOrderedRulesMissingBalanceFailedDrawContinues(t *testing.T) {
	ev := mbEval(t,
		[]predictioneval.OrderedRulesOutcome{orOutcome("A", 4), orOutcome("B", 6)},
		mbConfig([]predictioneval.OrderedRule{mbPointsRule(50)}, 80, 90),
		orWordRefuse, orWordAdmit)

	c1 := mbVisit(t, ev, "C1")
	switch {
	case c1.Verdict != predictioneval.CandidateNoMatch:
		t.Fatalf("C1 verdict = %q, want NO_MATCH: a failed draw is not a reason to end the scan",
			c1.Verdict)
	case c1.BalanceUse != predictioneval.BalanceNotEvaluated:
		t.Fatalf("C1 balance use = %q, want NOT_EVALUATED: nothing admitted, so the balance was never "+
			"needed and its absence is not a finding", c1.BalanceUse)
	case c1.RawWordsConsumedHere != 1:
		t.Fatalf("C1 spent %d words, want 1", c1.RawWordsConsumedHere)
	}

	mbAssertUnknownStakeStop(t, ev, "A")
	if ev.Selected.CandidateIdentity != "C2" {
		t.Fatalf("admitted on %q, want \"C2\": a missing balance on C1 must not end the scan",
			ev.Selected.CandidateIdentity)
	}
	if ev.RawWordsConsumed != 2 {
		t.Fatalf("consumed %d words, want 2", ev.RawWordsConsumed)
	}
}

// TestOrderedRulesMissingBalanceNoMatchContinues is MB2.
//
// Nothing on C1 matches at all — the rule's comparator misses both outcomes, so
// no draw is even taken, and the default misses too. The scan continues and C2,
// whose lopsided pool brings its first outcome under the threshold, admits on
// the single word.
func TestOrderedRulesMissingBalanceNoMatchContinues(t *testing.T) {
	ev := mbEval(t,
		[]predictioneval.OrderedRulesOutcome{orOutcome("A", 1), orOutcome("B", 9)},
		mbConfig([]predictioneval.OrderedRule{mbPointsRule(30)}, 80, 90),
		orWordAdmit)

	c1 := mbVisit(t, ev, "C1")
	switch {
	case c1.Verdict != predictioneval.CandidateNoMatch:
		t.Fatalf("C1 verdict = %q, want NO_MATCH", c1.Verdict)
	case c1.BalanceUse != predictioneval.BalanceNotEvaluated:
		t.Fatalf("C1 balance use = %q, want NOT_EVALUATED", c1.BalanceUse)
	case c1.RawWordsConsumedHere != 0:
		t.Fatalf("C1 spent %d words, want 0: an unmatched comparator never reaches the draw",
			c1.RawWordsConsumedHere)
	}

	mbAssertUnknownStakeStop(t, ev, "A")
	if ev.Selected.CandidateIdentity != "C2" {
		t.Fatalf("admitted on %q, want \"C2\"", ev.Selected.CandidateIdentity)
	}
	if ev.RawWordsConsumed != 1 {
		t.Fatalf("consumed %d words, want 1", ev.RawWordsConsumed)
	}
}

// TestOrderedRulesMissingBalanceAdmittedDrawStopsWithUnknownStake is MB3.
//
// C1's rule matches and its draw SUCCEEDS. Now the balance matters and is not
// there: the mechanism knows it would have bet outcome A and cannot say how
// much. The run stops on C1 — C2 and the second word are never reached, because
// they belong to a world in which this participation did not happen.
func TestOrderedRulesMissingBalanceAdmittedDrawStopsWithUnknownStake(t *testing.T) {
	ev := mbEval(t,
		[]predictioneval.OrderedRulesOutcome{orOutcome("A", 4), orOutcome("B", 6)},
		mbConfig([]predictioneval.OrderedRule{mbPointsRule(50)}, 80, 90),
		orWordAdmit, orWordRefuse)

	mbAssertUnknownStakeStop(t, ev, "A")
	c1 := mbVisit(t, ev, "C1")
	switch {
	case ev.Selected.CandidateIdentity != "C1":
		t.Fatalf("admitted on %q, want \"C1\"", ev.Selected.CandidateIdentity)
	case ev.Selected.Basis != predictioneval.SelectionDetailedRule:
		t.Fatalf("basis = %q, want DETAILED_RULE", ev.Selected.Basis)
	case c1.BalanceUse != predictioneval.BalanceRequiredButMissing:
		t.Fatalf("C1 balance use = %q, want REQUIRED_BUT_MISSING", c1.BalanceUse)
	case ev.RawWordsConsumed != 1:
		t.Fatalf("consumed %d words, want 1: the second word belongs to a candidate never reached",
			ev.RawWordsConsumed)
	case ev.CandidatesConsumed != 1:
		t.Fatalf("consumed %d candidates, want 1", ev.CandidatesConsumed)
	}
	if ev.Reason != predictioneval.ReasonBalanceNotSupplied {
		t.Fatalf("reason = %q, want BALANCE_NOT_SUPPLIED", ev.Reason)
	}
}

// TestOrderedRulesMissingBalanceDefaultStopsWithoutDraw is MB4.
//
// No detailed rules at all, and the default bounds contain C1's first share. It
// admits, consuming NOTHING — the word list is empty and that is not an error.
// The stop is the same as MB3's: choice known, stake unknown.
func TestOrderedRulesMissingBalanceDefaultStopsWithoutDraw(t *testing.T) {
	ev := mbEval(t,
		[]predictioneval.OrderedRulesOutcome{orOutcome("A", 4), orOutcome("B", 6)},
		mbConfig(nil, 40, 50))

	mbAssertUnknownStakeStop(t, ev, "A")
	c1 := mbVisit(t, ev, "C1")
	switch {
	case ev.Selected.CandidateIdentity != "C1":
		t.Fatalf("admitted on %q, want \"C1\"", ev.Selected.CandidateIdentity)
	case ev.Selected.Basis != predictioneval.SelectionDefault:
		t.Fatalf("basis = %q, want DEFAULT", ev.Selected.Basis)
	case ev.Selected.RuleIndex != -1:
		t.Fatalf("rule index = %d, want -1 for a default admission", ev.Selected.RuleIndex)
	case c1.BalanceUse != predictioneval.BalanceRequiredButMissing:
		t.Fatalf("C1 balance use = %q, want REQUIRED_BUT_MISSING", c1.BalanceUse)
	case ev.RawWordsConsumed != 0:
		t.Fatalf("a default admission consumes no draw: %d", ev.RawWordsConsumed)
	case ev.BernoulliEvaluations != 0:
		t.Fatalf("no rule was reachable, so no Bernoulli ran: %d", ev.BernoulliEvaluations)
	case ev.CandidatesConsumed != 1:
		t.Fatalf("consumed %d candidates, want 1", ev.CandidatesConsumed)
	}
}

// TestOrderedRulesMissingBalanceFailedDrawThenDefaultKeepsOneDraw is MB4b.
//
// The two halves compose: C1's rule matches, its draw fails and spends a word,
// and then the SAME outcome's default admits without spending another. The
// failed rule's word is not refunded by the default's success.
func TestOrderedRulesMissingBalanceFailedDrawThenDefaultKeepsOneDraw(t *testing.T) {
	ev := mbEval(t,
		[]predictioneval.OrderedRulesOutcome{orOutcome("A", 4), orOutcome("B", 6)},
		mbConfig([]predictioneval.OrderedRule{mbPointsRule(50)}, 40, 50),
		orWordRefuse)

	mbAssertUnknownStakeStop(t, ev, "A")
	switch {
	case ev.Selected.CandidateIdentity != "C1":
		t.Fatalf("admitted on %q, want \"C1\"", ev.Selected.CandidateIdentity)
	case ev.Selected.Basis != predictioneval.SelectionDefault:
		t.Fatalf("basis = %q, want DEFAULT", ev.Selected.Basis)
	case ev.RawWordsConsumed != 1:
		t.Fatalf("consumed %d words, want 1: the default adds none and the failed rule's stays spent",
			ev.RawWordsConsumed)
	}
}

// TestOrderedRulesBalanceIsNeverForwardFilledToALaterCandidate pins the OTHER
// borrow direction.
//
// Carrying a balance BACKWARDS is the obvious mistake and is refused at
// projection. Carrying one FORWARDS is the subtle one, because it looks
// harmless: the earlier candidate really did hold that balance, and reusing it
// requires no back-dating at all. It is still a value the later candidate never
// held, and the model would report a confident stake for it.
//
// C1 holds a known balance and its draw fails; C2 holds none and admits. The
// stake must stay unknown.
func TestOrderedRulesBalanceIsNeverForwardFilledToALaterCandidate(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{
		orCandidate("C1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
		orCandidate("C2", 20, orMissingBalance(), orOutcome("A", 4), orOutcome("B", 6)),
	}
	ev := predictioneval.EvaluateOrderedRules(orProject(t, cs, mbCall()),
		mbConfig([]predictioneval.OrderedRule{mbPointsRule(50)}, 80, 90),
		orDraws(orWordRefuse, orWordAdmit))

	mbAssertUnknownStakeStop(t, ev, "A")
	if ev.Selected.CandidateIdentity != "C2" {
		t.Fatalf("admitted on %q, want \"C2\"", ev.Selected.CandidateIdentity)
	}
	// C1's balance of 1000 at ten percent, capped at 500, would have sized 100.
	// Seeing that number here means a balance travelled forward.
	if ev.Stake.Presence == predictioneval.SuppliedKnown {
		t.Fatalf("C2's stake was sized as %d from C1's balance; a balance belongs to the candidate it "+
			"was supplied with and to no other", ev.Stake.Value)
	}
	if mbVisit(t, ev, "C2").BalanceUse != predictioneval.BalanceRequiredButMissing {
		t.Fatalf("C2 balance use = %q, want REQUIRED_BUT_MISSING",
			mbVisit(t, ev, "C2").BalanceUse)
	}
}

// TestOrderedRulesZeroPercentDoesNotLicenseAZeroStakeForAMissingBalance closes
// the one shortcut that looks mathematically safe.
//
// With a percentage of zero the stake is zero for EVERY balance, so it is
// tempting to answer without consulting one — and a cap of zero looks the same
// way. But the model's claim would then be "it would have staked zero", which
// is a statement about an amount, and the amount is exactly what is unknown
// here. The distinction is epistemic, not arithmetic: participation is known,
// the stake is not, and a computed-looking zero would erase that.
func TestOrderedRulesZeroPercentDoesNotLicenseAZeroStakeForAMissingBalance(t *testing.T) {
	for _, tc := range []struct {
		name     string
		percent  float64
		maxValue uint32
	}{
		{"a zero percentage", 0, 500},
		{"a zero percentage and a zero cap", 0, 0},
		{"a cap of zero with a real percentage", 10, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := []predictioneval.OrderedRulesCandidate{
				orCandidate("C1", 10, orMissingBalance(), orOutcome("A", 4), orOutcome("B", 6)),
			}
			cfg := orConfig([]predictioneval.OrderedRule{
				orRule(predictioneval.ComparatorLe, 50, 100, tc.maxValue, tc.percent),
			}, 80, 90, tc.maxValue, tc.percent)
			ev := predictioneval.EvaluateOrderedRules(orProject(t, cs, mbCall()), cfg, orDraws())

			mbAssertUnknownStakeStop(t, ev, "A")
			if ev.Reason != predictioneval.ReasonBalanceNotSupplied {
				t.Fatalf("reason = %q, want BALANCE_NOT_SUPPLIED", ev.Reason)
			}
		})
	}

	// Non-vacuity: the SAME zero-percent config with a known balance does
	// produce a computable zero, so the stops above are about the missing
	// balance and not about the percentage.
	known := []predictioneval.OrderedRulesCandidate{
		orCandidate("C1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
	}
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 50, 100, 500, 0),
	}, 80, 90, 500, 0)
	ev := predictioneval.EvaluateOrderedRules(orProject(t, known, mbCall()), cfg, orDraws())
	if ev.Status != predictioneval.StatusWouldAttempt || ev.Stake.Value != 0 ||
		ev.Stake.Presence != predictioneval.SuppliedKnown {
		t.Fatalf("with a balance in hand a zero percentage is a computable zero: status %q stake %+v",
			ev.Status, ev.Stake)
	}
}

// TestOrderedRulesInvalidBalanceIsDistinctFromMissing pins the two non-KNOWN
// presences apart.
//
// "We do not have it" and "we have it and it is unusable" are different pieces
// of evidence, and a reader deciding whether the gap is fixable needs to know
// which one it is. Both stop, and they stop with different reasons.
func TestOrderedRulesInvalidBalanceIsDistinctFromMissing(t *testing.T) {
	invalid := predictioneval.SuppliedInt64{
		Presence: predictioneval.SuppliedInvalid,
		Reason:   "stored balance failed the source's own domain check",
	}
	cs := []predictioneval.OrderedRulesCandidate{
		orCandidate("C1", 10, invalid, orOutcome("A", 4), orOutcome("B", 6)),
	}
	ev := predictioneval.EvaluateOrderedRules(orProject(t, cs, mbCall()),
		mbConfig([]predictioneval.OrderedRule{mbPointsRule(50)}, 80, 90), orDraws(orWordAdmit))

	mbAssertUnknownStakeStop(t, ev, "A")
	switch {
	case ev.Reason != predictioneval.ReasonBalanceInvalid:
		t.Fatalf("reason = %q, want BALANCE_INVALID — an unusable balance is not an absent one", ev.Reason)
	case ev.Stake.Presence != predictioneval.SuppliedInvalid:
		t.Fatalf("stake presence = %q, want INVALID", ev.Stake.Presence)
	case mbVisit(t, ev, "C1").BalanceUse != predictioneval.BalanceRequiredButInvalid:
		t.Fatalf("balance use = %q, want REQUIRED_BUT_INVALID", mbVisit(t, ev, "C1").BalanceUse)
	}
	// The caller's own explanation survives into the result rather than being
	// replaced by this package's closed reason.
	if ev.Stake.Reason != invalid.Reason {
		t.Fatalf("stake reason = %q, want the caller's %q", ev.Stake.Reason, invalid.Reason)
	}
}

// TestOrderedRulesMissingBalanceIsNeverBorrowedFromAnotherCandidate pins the
// last route to a fabricated stake.
//
// C2 has a perfectly good balance. C1 does not. An implementation that
// forward-filled, back-filled or "used the nearest known balance" would size
// C1's stake from C2's number — a value C1 could not possibly have read.
func TestOrderedRulesMissingBalanceIsNeverBorrowedFromAnotherCandidate(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{
		orCandidate("C1", 10, orMissingBalance(), orOutcome("A", 4), orOutcome("B", 6)),
		orCandidate("C2", 20, orKnownBalance(5000), orOutcome("A", 4), orOutcome("B", 6)),
	}
	ev := predictioneval.EvaluateOrderedRules(orProject(t, cs, mbCall()),
		mbConfig([]predictioneval.OrderedRule{mbPointsRule(50)}, 80, 90), orDraws(orWordAdmit))

	mbAssertUnknownStakeStop(t, ev, "A")
	if ev.Selected.CandidateIdentity != "C1" {
		t.Fatalf("admitted on %q, want \"C1\"", ev.Selected.CandidateIdentity)
	}
	// C2's balance would have sized a stake of 500 — the cap. Seeing it here
	// would mean a balance travelled backwards between candidates.
	if ev.Stake.Presence == predictioneval.SuppliedKnown {
		t.Fatalf("C1's stake was sized as %d from a balance it never had", ev.Stake.Value)
	}
}
