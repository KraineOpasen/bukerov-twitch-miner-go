package predictioneval_test

// THE BOUNDARY, THE IDENTITIES AND THE BINDINGS.
//
// The mechanism is only as honest as the stream it runs on. These tests hold
// the projection to the three things it exists to guarantee: that the traversal
// stops before the world was acted on, that nothing in the stream was joined or
// back-dated into existence, and that a result cannot be re-attached to inputs
// it was not computed from.

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
)

func orIntervention(id string, pos int64, kind predictioneval.OrderedRulesInterventionKind,
	rel predictioneval.OrderedRulesRelevance, detail string) predictioneval.OrderedRulesIntervention {
	return predictioneval.OrderedRulesIntervention{
		Identity: id, Position: pos, Kind: kind, Relevance: rel, Detail: detail,
	}
}

// orAlwaysAdmitConfig admits the first outcome of any [A:4,B:6] pool without
// consuming entropy, so a test can isolate WHICH candidate was reachable.
func orAlwaysAdmitConfig() predictioneval.OrderedRulesConfig {
	return orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 100, 100, 0, 10),
	}, 95, 100, 0, 10)
}

// TestOrderedRulesManualCallStartedWithoutAttemptIDCutsTheStream pins the
// boundary as a fact about the ORIGINAL records, not about attempt grouping.
//
// A manual placement carries no automatic attempt discriminator. Grouping facts
// by that discriminator — the way the baseline replay legitimately does — would
// filter this call out entirely, and the model would go on evaluating a round
// somebody had already bet on.
func TestOrderedRulesManualCallStartedWithoutAttemptIDCutsTheStream(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{
		orTwoOutcomePool("c1", 10),
		orTwoOutcomePool("c2", 30),
	}
	ins := []predictioneval.OrderedRulesIntervention{
		orIntervention("manual-1", 20, predictioneval.InterventionManualCallStarted,
			predictioneval.RelevanceProven, "manual placement; no autoAttemptId on the source record"),
	}

	stream := orProject(t, cs, ins)
	switch {
	case !stream.Cutoff.Established:
		t.Fatal("a manual placement call is a factual intervention and must establish the boundary")
	case stream.Cutoff.Position != 20:
		t.Fatalf("cutoff at %d, want 20", stream.Cutoff.Position)
	case len(stream.Candidates) != 1 || stream.Candidates[0].Identity != "c1":
		t.Fatalf("surviving candidates = %+v, want only c1", stream.Candidates)
	case stream.Cutoff.DroppedAtOrAfter != 1:
		t.Fatalf("dropped %d candidates, want 1", stream.Cutoff.DroppedAtOrAfter)
	}

	ev := predictioneval.EvaluateOrderedRules(stream, orAlwaysAdmitConfig(), orDraws())
	if sel := orMustAdmit(t, ev); sel.CandidateIdentity != "c1" {
		t.Fatalf("admitted on %q; nothing at or after the boundary may be evaluated", sel.CandidateIdentity)
	}
}

// TestOrderedRulesManualCallCutsOffOtherIncarnation pins that a call carrying a
// DIFFERENT or empty round incarnation still cuts.
//
// The manual path re-reads the incarnation freshly, so a boundary search
// filtered by the locally-known incarnation would miss a real intervention. The
// caller supplies the association it proved; the model does not re-derive one
// and does not narrow it by incarnation.
func TestOrderedRulesManualCallCutsOffOtherIncarnation(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{orTwoOutcomePool("c1", 10), orTwoOutcomePool("c2", 30)}
	ins := []predictioneval.OrderedRulesIntervention{
		orIntervention("manual-other-incarnation", 20, predictioneval.InterventionManualCallStarted,
			predictioneval.RelevanceProven,
			"manual placement recorded under a different round incarnation; association proven by channel and account context"),
	}
	stream := orProject(t, cs, ins)
	if !stream.Cutoff.Established || stream.Cutoff.Position != 20 || len(stream.Candidates) != 1 {
		t.Fatalf("an intervention under another incarnation must still cut: cutoff %+v, %d candidates",
			stream.Cutoff, len(stream.Candidates))
	}
}

// TestOrderedRulesFailedFactualCallStillCutsOff pins that the boundary does not
// depend on the call having WORKED.
//
// Once a placement call started, the observed world diverged from the
// counterfactual one — whether the call succeeded, errored or timed out. The
// intervention type therefore has no place to record an outcome at all, which
// is checked structurally below: a field for it would be an invitation to
// resume the alternative world after a failure.
func TestOrderedRulesFailedFactualCallStillCutsOff(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{orTwoOutcomePool("c1", 10), orTwoOutcomePool("c2", 30)}
	ins := []predictioneval.OrderedRulesIntervention{
		orIntervention("call-that-failed", 20, predictioneval.InterventionAutoCallStarted,
			predictioneval.RelevanceProven, "call started and returned an error"),
	}
	stream := orProject(t, cs, ins)
	if !stream.Cutoff.Established || len(stream.Candidates) != 1 {
		t.Fatalf("a failed call is still a boundary: cutoff %+v, %d candidates",
			stream.Cutoff, len(stream.Candidates))
	}

	typ := reflect.TypeOf(predictioneval.OrderedRulesIntervention{})
	for i := 0; i < typ.NumField(); i++ {
		name := strings.ToLower(typ.Field(i).Name)
		for _, banned := range []string{"success", "placed", "result", "returned", "stake", "payout", "amount"} {
			if strings.Contains(name, banned) {
				t.Errorf("OrderedRulesIntervention.%s records a call's OUTCOME. The boundary is the call "+
					"having STARTED; carrying its result invites resuming the counterfactual after a "+
					"failure.", typ.Field(i).Name)
			}
		}
	}
}

// TestOrderedRulesTheCutIsExclusiveAtTheBoundaryPosition pins the boundary
// comparison itself.
//
// A candidate sitting at EXACTLY the intervention's position cannot be shown to
// have preceded it — the two coordinates are equal, so nothing establishes which
// came first. It is excluded, which is the conservative reading: including it
// would let the model evaluate a candidate that may already have been acted on,
// and no later evidence could ever reveal the mistake.
func TestOrderedRulesTheCutIsExclusiveAtTheBoundaryPosition(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{
		orTwoOutcomePool("before", 10),
		orTwoOutcomePool("at-the-boundary", 20),
		orTwoOutcomePool("after", 30),
	}
	ins := []predictioneval.OrderedRulesIntervention{
		orIntervention("call-1", 20, predictioneval.InterventionAutoCallStarted,
			predictioneval.RelevanceProven, "call started at the same position"),
	}

	stream := orProject(t, cs, ins)
	if len(stream.Candidates) != 1 || stream.Candidates[0].Identity != "before" {
		t.Fatalf("surviving candidates = %+v, want only \"before\": the cut is EXCLUSIVE, so a candidate "+
			"at the boundary's own position does not survive it", stream.Candidates)
	}
	if stream.Cutoff.DroppedAtOrAfter != 2 {
		t.Fatalf("dropped %d candidates, want 2", stream.Cutoff.DroppedAtOrAfter)
	}

	// And the excluded candidate is genuinely unreachable, not merely absent
	// from the slice: nothing in a traversal may admit on it.
	ev := predictioneval.EvaluateOrderedRules(stream, orAlwaysAdmitConfig(), orDraws())
	if sel := orMustAdmit(t, ev); sel.CandidateIdentity != "before" {
		t.Fatalf("admitted on %q", sel.CandidateIdentity)
	}

	// Non-vacuity: moved one position earlier, that same candidate DOES survive
	// — so the exclusion above is about the comparison and not about the fixture.
	cs[1].Position = 19
	if earlier := orProject(t, cs, ins); len(earlier.Candidates) != 2 {
		t.Fatalf("a candidate strictly before the boundary must survive: %+v", earlier.Candidates)
	}
}

// TestOrderedRulesAmbiguousInterventionTakesTheEarlierConservativeBoundary pins
// which way an unprovable association resolves.
//
// The earlier boundary is the safe one. Preferring the later, provable call
// because the earlier one is merely ambiguous would let the model evaluate
// across a real intervention — so the conservative cut is taken and LABELLED,
// rather than taken silently or skipped.
func TestOrderedRulesAmbiguousInterventionTakesTheEarlierConservativeBoundary(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{
		orTwoOutcomePool("c1", 10), orTwoOutcomePool("c2", 30), orTwoOutcomePool("c3", 50),
	}
	ins := []predictioneval.OrderedRulesIntervention{
		orIntervention("proven-late", 40, predictioneval.InterventionAutoCallStarted,
			predictioneval.RelevanceProven, "proven"),
		orIntervention("ambiguous-early", 20, predictioneval.InterventionManualCallStarted,
			predictioneval.RelevanceAmbiguous, "association with this episode could not be established"),
	}
	stream := orProject(t, cs, ins)
	switch {
	case stream.Cutoff.Position != 20:
		t.Fatalf("cutoff at %d, want the earlier, conservative 20", stream.Cutoff.Position)
	case stream.Cutoff.Basis != predictioneval.CutoffConservativeAmbiguous:
		t.Fatalf("basis = %q, want CONSERVATIVE_AMBIGUOUS_ASSOCIATION", stream.Cutoff.Basis)
	}
	if !containsSubstring(stream.Qualifications, "CUTOFF_FROM_AMBIGUOUS_ASSOCIATION") {
		t.Fatalf("a conservative cut must be qualified, not taken silently: %v", stream.Qualifications)
	}

	// An intervention PROVEN to belong elsewhere does not bound this episode.
	elsewhere := orProject(t, cs, []predictioneval.OrderedRulesIntervention{
		orIntervention("other-episode", 20, predictioneval.InterventionAutoCallStarted,
			predictioneval.RelevanceExcluded, "proven to belong to a different episode"),
	})
	if elsewhere.Cutoff.Established {
		t.Fatalf("an intervention proven unrelated must not cut: %+v", elsewhere.Cutoff)
	}
}

// TestOrderedRulesLaterBalanceCannotRepairEarlierCandidate pins the temporal
// rule on every supplied field.
//
// A balance read later — from a schedule check, a manual validation, a
// decision-time envelope — is information the earlier candidate never held.
// Attaching it is refused outright rather than accepted with a note, because a
// note does not stop the arithmetic from using it.
func TestOrderedRulesLaterBalanceCannotRepairEarlierCandidate(t *testing.T) {
	borrowed := predictioneval.SuppliedInt64{
		Presence:            predictioneval.SuppliedKnown,
		Value:               5000,
		Provenance:          "a later schedule read on the same channel",
		AvailableAtPosition: 25,
	}
	c := orCandidate("c1", 10, borrowed, orOutcome("A", 4), orOutcome("B", 6))
	_, err := predictioneval.ProjectOrderedRulesStream(
		orSource([]predictioneval.OrderedRulesCandidate{c}, nil), orAdmission())
	if !errors.Is(err, predictioneval.ErrOrderedRulesBackdatedValue) {
		t.Fatalf("got %v, want a back-dated-value refusal", err)
	}

	// The same value, available in time, is accepted — so the refusal is about
	// the TIMING and not about the field.
	borrowed.AvailableAtPosition = 10
	c = orCandidate("c1", 10, borrowed, orOutcome("A", 4), orOutcome("B", 6))
	if _, err := predictioneval.ProjectOrderedRulesStream(
		orSource([]predictioneval.OrderedRulesCandidate{c}, nil), orAdmission()); err != nil {
		t.Fatalf("a value available at the candidate's own position must be accepted: %v", err)
	}
}

// TestOrderedRulesRoundAndSourceEpisodeCannotBeJoinedByEventIDAlone pins that
// membership must be PROVEN.
//
// A shared event id, pool instance or nearby timestamp does not establish that
// two facts came from the same account, the same episode or even the same
// connection generation. Joining on one would stitch unrelated rounds into a
// single candidate stream and invent attempt opportunities out of the seam.
func TestOrderedRulesRoundAndSourceEpisodeCannotBeJoinedByEventIDAlone(t *testing.T) {
	c := orTwoOutcomePool("c1", 10)
	c.EpisodeMembership = predictioneval.MembershipUnknown
	_, err := predictioneval.ProjectOrderedRulesStream(
		orSource([]predictioneval.OrderedRulesCandidate{c}, nil), orAdmission())
	if !errors.Is(err, predictioneval.ErrOrderedRulesMembershipUnproven) {
		t.Fatalf("got %v, want an unproven-membership refusal", err)
	}

	// A scope with no association evidence is refused for the same reason.
	src := orSource([]predictioneval.OrderedRulesCandidate{orTwoOutcomePool("c1", 10)}, nil)
	src.Scope.AssociationEvidence = ""
	if _, err := predictioneval.ProjectOrderedRulesStream(src, orAdmission()); err == nil {
		t.Fatal("a scope claiming an episode with no association evidence must be refused")
	}
}

// TestOrderedRulesAmbiguousCausalPositionsAreNotSortedAway pins that an unknown
// order stays unknown.
//
// Two facts at the same position mean the caller does not actually know which
// came first. A tie broken by identity, by timestamp or by slice order would
// produce a confident traversal over an order nobody established — and the
// traversal's answer depends on that order.
func TestOrderedRulesAmbiguousCausalPositionsAreNotSortedAway(t *testing.T) {
	for _, tc := range []struct {
		name   string
		second int64
	}{
		{"equal positions", 10},
		{"decreasing positions", 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := []predictioneval.OrderedRulesCandidate{
				orTwoOutcomePool("c1", 10), orTwoOutcomePool("c2", tc.second),
			}
			_, err := predictioneval.ProjectOrderedRulesStream(orSource(cs, nil), orAdmission())
			if !errors.Is(err, predictioneval.ErrOrderedRulesAmbiguousOrder) {
				t.Fatalf("got %v, want an ambiguous-order refusal", err)
			}
		})
	}
}

// TestOrderedRulesUnknownCandidateMustNotBeFilteredOut pins that an unreadable
// candidate stops the scan instead of being skipped.
//
// Skipping it is the tempting repair and it is wrong twice over: it hands the
// NEXT candidate an opportunity that only exists because the unreadable one was
// removed, and it hides the gap behind a confident answer. The two runs below
// differ in exactly that way.
func TestOrderedRulesUnknownCandidateMustNotBeFilteredOut(t *testing.T) {
	unreadable := orCandidate("c1", 10, orKnownBalance(1000),
		orMissingPoints("A"), orOutcome("B", 6))
	cs := []predictioneval.OrderedRulesCandidate{unreadable, orTwoOutcomePool("c2", 20)}

	ev := predictioneval.EvaluateOrderedRules(orProject(t, cs, nil), orAlwaysAdmitConfig(), orDraws())
	switch {
	case ev.Status != predictioneval.StatusUnknownInput:
		t.Fatalf("status = %q, want UNKNOWN_INPUT", ev.Status)
	case ev.Reason != predictioneval.ReasonOutcomePointsNotKnown:
		t.Fatalf("reason = %q, want OUTCOME_POINTS_NOT_KNOWN", ev.Reason)
	case ev.Selected != nil:
		t.Fatalf("skipping the unreadable candidate would have admitted %+v on the next one", ev.Selected)
	case ev.CandidatesConsumed != 1:
		t.Fatalf("consumed %d candidates, want 1", ev.CandidatesConsumed)
	}

	// Non-vacuity: with the same candidate readable, the run DOES admit — so the
	// stop above is caused by the missing value and not by the fixture's shape.
	fixed := []predictioneval.OrderedRulesCandidate{orTwoOutcomePool("c1", 10), orTwoOutcomePool("c2", 20)}
	if ok := predictioneval.EvaluateOrderedRules(orProject(t, fixed, nil),
		orAlwaysAdmitConfig(), orDraws()); ok.Selected == nil {
		t.Fatalf("the control run must admit; got %q", ok.Status)
	}
}

// TestOrderedRulesWireProjectionDoesNotProveScalarPresence pins the distinction
// a wire projection cannot make.
//
// An outcome holding zero points and an outcome with no points field at all
// close to the same projection, and a session classified complete does not
// recover the difference. So a MISSING value is never read as a zero — not even
// when the source declares complete coverage, and not even though the resulting
// arithmetic would look perfectly reasonable.
func TestOrderedRulesWireProjectionDoesNotProveScalarPresence(t *testing.T) {
	missing := []predictioneval.OrderedRulesCandidate{
		orCandidate("c1", 10, orKnownBalance(1000), orMissingPoints("A"), orOutcome("B", 6)),
	}
	knownZero := []predictioneval.OrderedRulesCandidate{
		orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 0), orOutcome("B", 6)),
	}
	cfg := orConfig(nil, 0, 0, 0, 10)

	if ev := predictioneval.EvaluateOrderedRules(orProject(t, missing, nil), cfg, orDraws()); ev.Status !=
		predictioneval.StatusUnknownInput {
		t.Fatalf("a missing points value is not a zero: status = %q selecting %+v. The source declares "+
			"COMPLETE coverage here, and that still does not recover an unrecorded scalar.",
			ev.Status, ev.Selected)
	}
	// The same shape with an EXPLICIT zero evaluates, and admits on the
	// zero-share outcome — proving the refusal above is about presence.
	ev := predictioneval.EvaluateOrderedRules(orProject(t, knownZero, nil), cfg, orDraws())
	if sel := orMustAdmit(t, ev); sel.OutcomeIdentity != "A" {
		t.Fatalf("an explicit zero must evaluate; selected %q", sel.OutcomeIdentity)
	}
}

// TestOrderedRulesAppendingPostCutoffFactsCannotChangeEvaluation pins
// no-look-ahead.
//
// Everything the model must not see is appended at once: candidates after the
// boundary, richer balances, and more entropy. The decisions and the
// consumed-prefix digest must be identical. The whole-source digests are
// EXPECTED to move — that contrast is what makes the claim checkable rather
// than asserted.
func TestOrderedRulesAppendingPostCutoffFactsCannotChangeEvaluation(t *testing.T) {
	base := []predictioneval.OrderedRulesCandidate{orTwoOutcomePool("c1", 10)}
	ins := []predictioneval.OrderedRulesIntervention{
		orIntervention("call-1", 20, predictioneval.InterventionAutoCallStarted,
			predictioneval.RelevanceProven, "boundary"),
	}
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 50, 50, 0, 10),
	}, 95, 100, 0, 10)

	before := predictioneval.EvaluateOrderedRules(orProject(t, base, ins), cfg, orDraws(orWordAdmit))

	after := append(append([]predictioneval.OrderedRulesCandidate(nil), base...),
		orCandidate("post-1", 30, orKnownBalance(999999), orOutcome("A", 9), orOutcome("B", 1)),
		orCandidate("post-2", 40, orKnownBalance(1), orOutcome("A", 1), orOutcome("B", 9)))
	later := predictioneval.EvaluateOrderedRules(orProject(t, after, ins), cfg,
		orDraws(orWordAdmit, orWordRefuse, orWordAdmit))

	switch {
	case before.Status != later.Status:
		t.Fatalf("status changed from %q to %q when post-boundary facts were appended",
			before.Status, later.Status)
	case before.Stake != later.Stake:
		t.Fatalf("stake changed from %+v to %+v", before.Stake, later.Stake)
	case *before.Selected != *later.Selected:
		t.Fatalf("the selection changed from %+v to %+v", *before.Selected, *later.Selected)
	case before.RawWordsConsumed != later.RawWordsConsumed:
		t.Fatalf("consumption changed from %d to %d", before.RawWordsConsumed, later.RawWordsConsumed)
	case before.ConsumedInputDigest != later.ConsumedInputDigest:
		t.Fatal("the consumed-prefix digest moved when facts beyond the boundary were appended; it must " +
			"witness only what the traversal read")
	}
	// The two whole-subject digests must BOTH move, and for different reasons —
	// that contrast is what makes the consumed-prefix stability above mean
	// something rather than being a hash that ignores its inputs.
	if before.EntropyDigest == later.EntropyDigest {
		t.Fatal("the whole-trace entropy digest must MOVE when the trace grows — otherwise the " +
			"consumed-prefix stability above is not distinguishing anything")
	}
	// Appending these two candidates does not change which candidates SURVIVE
	// the boundary — only how many it removed. The two streams must still be
	// distinguishable: one is a prefix cut short of two candidates, the other a
	// complete one, and that is different evidence even though what survived is
	// identical.
	//
	// What this does NOT pin, stated because the neighbouring cases would let a
	// reader assume otherwise: it is not a guard on the digest's dedicated
	// removal-count line. That line is redundant. The count also reaches the
	// digest through the boundary qualification, which is appended only when the
	// count is non-zero and embeds the exact number — so presence separates zero
	// from non-zero and the text separates every non-zero pair. Dropping the
	// dedicated line changes no digest for any input, which makes it an
	// equivalent mutant rather than an untested one.
	if before.Cutoff.DroppedAtOrAfter != 0 || later.Cutoff.DroppedAtOrAfter != 2 {
		t.Fatalf("this case needs the boundary's removal count to be the only difference: %d then %d",
			before.Cutoff.DroppedAtOrAfter, later.Cutoff.DroppedAtOrAfter)
	}
	if before.StreamDigest == later.StreamDigest {
		t.Fatal("the whole-stream digest must MOVE when the boundary removes additional supplied " +
			"candidates: a stream cut short of two candidates is different evidence from one cut " +
			"short of none, even though the surviving candidates are identical")
	}
}

// TestOrderedRulesCandidatePolicyAndDrawBindingsCannotBeRecombined pins that a
// result names the exact inputs it came from.
//
// Each of the three components is swapped in turn, and each must move its own
// binding. A single blended hash would let a result be presented as having come
// from a config or an entropy run it never saw.
func TestOrderedRulesCandidatePolicyAndDrawBindingsCannotBeRecombined(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{orTwoOutcomePool("c1", 10)}
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 50, 50, 0, 10),
	}, 95, 100, 0, 10)
	stream := orProject(t, cs, nil)
	base := predictioneval.EvaluateOrderedRules(stream, cfg, orDraws(orWordAdmit))

	otherStream := orProject(t, []predictioneval.OrderedRulesCandidate{orTwoOutcomePool("c9", 10)}, nil)
	if got := predictioneval.EvaluateOrderedRules(otherStream, cfg, orDraws(orWordAdmit)); got.StreamDigest ==
		base.StreamDigest || got.ConsumedInputDigest == base.ConsumedInputDigest {
		t.Fatal("a different stream must move both the stream binding and the consumed-prefix binding")
	}

	otherCfg := cfg
	otherCfg.Detailed = []predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 50, 51, 0, 10),
	}
	if got := predictioneval.EvaluateOrderedRules(stream, otherCfg, orDraws(orWordAdmit)); got.ConfigDigest ==
		base.ConfigDigest || got.ConsumedInputDigest == base.ConsumedInputDigest {
		t.Fatal("a different config must move both the config binding and the consumed-prefix binding")
	}

	otherDraws := orDraws(orWordAdmit)
	otherDraws.RunID = "another-run"
	if got := predictioneval.EvaluateOrderedRules(stream, cfg, otherDraws); got.EntropyDigest ==
		base.EntropyDigest || got.ConsumedInputDigest == base.ConsumedInputDigest {
		t.Fatal("a different entropy run must move both the entropy binding and the consumed-prefix binding")
	}

	// The case above varies only the run's NAME, which is hashed separately —
	// so on its own it would still pass if the consumed-prefix digest ignored
	// the word VALUES entirely. This one holds the run identity fixed and
	// changes the one consumed word, which is the difference that actually
	// decided the outcome: the same stream and config admit under one word and
	// refuse under the other.
	flipped := orDraws(orWordRefuse)
	other := predictioneval.EvaluateOrderedRules(stream, cfg, flipped)
	switch {
	case base.Status != predictioneval.StatusWouldAttempt ||
		other.Status != predictioneval.StatusNoAttemptInSuppliedPrefix:
		t.Fatalf("this case needs the two words to decide differently: %q then %q",
			base.Status, other.Status)
	case base.RawWordsConsumed != 1 || other.RawWordsConsumed != 1:
		t.Fatalf("both runs must consume exactly one word: %d and %d",
			base.RawWordsConsumed, other.RawWordsConsumed)
	case other.ConsumedInputDigest == base.ConsumedInputDigest:
		t.Fatal("two runs whose single consumed word differed — one admitting, one refusing — carry the " +
			"same consumed-prefix digest. A result could then be presented as having come from the " +
			"entropy prefix that produced the opposite answer.")
	}

	// A stream mutated AFTER projection no longer matches its own selection
	// digest and is refused rather than evaluated.
	tampered := stream
	tampered.Candidates = append([]predictioneval.OrderedRulesCandidate(nil), stream.Candidates...)
	tampered.Candidates[0].Outcomes = []predictioneval.OrderedRulesOutcome{orOutcome("A", 9), orOutcome("B", 1)}
	got := predictioneval.EvaluateOrderedRules(tampered, cfg, orDraws(orWordAdmit))
	if got.Status != predictioneval.StatusRefused ||
		got.Reason != predictioneval.ReasonStreamDigestMismatch {
		t.Fatalf("a stream edited after projection must be refused: status %q reason %q",
			got.Status, got.Reason)
	}
}

// TestOrderedRulesInputsAndTraceDoNotAliasCallerValues pins the detach.
//
// The caller keeps its own slices. If the projection retained them, a later
// mutation would change what a previously-computed result reads while its
// digest stayed the same — a result that quietly stops describing its inputs.
func TestOrderedRulesInputsAndTraceDoNotAliasCallerValues(t *testing.T) {
	outcomes := []predictioneval.OrderedRulesOutcome{orOutcome("A", 4), orOutcome("B", 6)}
	cs := []predictioneval.OrderedRulesCandidate{
		orCandidate("c1", 10, orKnownBalance(1000), outcomes...),
	}
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 50, 100, 0, 10),
	}, 95, 100, 0, 10)
	draws := orDraws(orWordAdmit)

	stream := orProject(t, cs, nil)
	before := predictioneval.EvaluateOrderedRules(stream, cfg, draws)

	// The trace is snapshotted BEFORE the caller's words are mutated. Without
	// this the case could not fail on a trace that aliased draws.Words at all —
	// nothing below reads before.Trace, and the test's own name promises it.
	if len(before.Trace) == 0 {
		t.Fatal("the run recorded no trace, so the aliasing assertion below would hold vacuously")
	}
	traceSnapshot := make([]predictioneval.OrderedRulesTraceEntry, len(before.Trace))
	copy(traceSnapshot, before.Trace)

	// Mutate everything the caller still holds.
	outcomes[0].Points.Value = 9
	outcomes[1].Points.Value = 1
	cs[0].Balance = orKnownBalance(7)
	cs[0].Outcomes[0].Identity = "MUTATED"
	cfg.Detailed[0].RawThresholdPercent = 1
	draws.Words[0] = orWordRefuse

	after := predictioneval.EvaluateOrderedRules(stream, orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 50, 100, 0, 10),
	}, 95, 100, 0, 10), orDraws(orWordAdmit))

	if len(before.Trace) != len(traceSnapshot) {
		t.Fatalf("the recorded trace changed length after the caller mutated its own inputs: %d then %d",
			len(traceSnapshot), len(before.Trace))
	}
	for i := range traceSnapshot {
		if before.Trace[i] != traceSnapshot[i] {
			t.Fatalf("trace entry %d changed after the caller mutated its own values:\n before = %+v\n"+
				"  after = %+v\nA recorded draw must be a copy, not a window onto the caller's words.",
				i, traceSnapshot[i], before.Trace[i])
		}
	}

	switch {
	case stream.Candidates[0].Outcomes[0].Identity != "A":
		t.Fatal("the projected stream aliases the caller's outcome slice")
	case stream.Candidates[0].Outcomes[0].Points.Value != 4:
		t.Fatal("the projected stream aliases the caller's outcome values")
	case stream.Candidates[0].Balance.Value != 1000:
		t.Fatal("the projected stream aliases the caller's balance")
	case before.StreamDigest != after.StreamDigest:
		t.Fatal("the stream digest moved after the caller mutated its own inputs")
	case before.ConsumedInputDigest != after.ConsumedInputDigest:
		t.Fatal("the consumed-prefix digest moved after the caller mutated its own inputs")
	case before.Stake != after.Stake:
		t.Fatalf("the stake changed from %+v to %+v", before.Stake, after.Stake)
	}
}

// TestOrderedRulesOverBudgetInputIsRefusedWithoutTruncation pins refusal as the
// only response to an oversized input.
//
// Truncating instead would be the quiet failure: a shortened candidate list
// changes which opportunities exist, and the result would still describe itself
// as a complete traversal.
func TestOrderedRulesOverBudgetInputIsRefusedWithoutTruncation(t *testing.T) {
	t.Run("too many candidates", func(t *testing.T) {
		cs := make([]predictioneval.OrderedRulesCandidate, predictioneval.MaxOrderedRulesCandidates+1)
		for i := range cs {
			cs[i] = orTwoOutcomePool("c"+itoaTest(i), int64(i+1))
		}
		stream, err := predictioneval.ProjectOrderedRulesStream(orSource(cs, nil), orAdmission())
		if !errors.Is(err, predictioneval.ErrOrderedRulesOverBound) {
			t.Fatalf("got %v, want an over-bound refusal", err)
		}
		if len(stream.Candidates) != 0 {
			t.Fatalf("a refused projection must return nothing, not a truncated %d-candidate stream",
				len(stream.Candidates))
		}
	})

	t.Run("too many rules", func(t *testing.T) {
		rules := make([]predictioneval.OrderedRule, predictioneval.MaxOrderedRulesRules+1)
		for i := range rules {
			rules[i] = orRule(predictioneval.ComparatorLe, 100, 100, 0, 10)
		}
		ev := predictioneval.EvaluateOrderedRules(
			orProject(t, []predictioneval.OrderedRulesCandidate{orTwoOutcomePool("c1", 10)}, nil),
			orConfig(rules, 95, 100, 0, 10), orDraws())
		if ev.Status != predictioneval.StatusRefused || ev.Reason != predictioneval.ReasonRuleCountOverBound {
			t.Fatalf("status %q reason %q, want REFUSED / RULE_COUNT_OVER_BOUND", ev.Status, ev.Reason)
		}
		if ev.Selected != nil {
			t.Fatal("a refused evaluation must decide nothing")
		}
	})

	t.Run("an outcome vector past the bound", func(t *testing.T) {
		outs := make([]predictioneval.OrderedRulesOutcome, predictioneval.MaxOrderedRulesOutcomes+1)
		for i := range outs {
			outs[i] = orOutcome("o"+itoaTest(i), int64(i+1))
		}
		c := orCandidate("c1", 10, orKnownBalance(1000), outs...)
		_, err := predictioneval.ProjectOrderedRulesStream(
			orSource([]predictioneval.OrderedRulesCandidate{c}, nil), orAdmission())
		if !errors.Is(err, predictioneval.ErrOrderedRulesOverBound) {
			t.Fatalf("got %v, want an over-bound refusal", err)
		}
	})
}

// TestOrderedRulesUnrecoverableOutcomeVectorIsNotTheDonorsDecline pins the
// distinction the vector's presence marker exists for.
//
// A pool holding fewer than two outcomes is a pool the donor declines: it walks
// on to the next candidate, and so does this model. A pool whose vector the
// caller COULD NOT RECOVER looks identical on the wire — a short slice — and is
// the opposite fact. Walking past it hands every later candidate an opportunity
// that exists only because this one was skipped, which is exactly what the
// no-silent-filtering rule forbids.
func TestOrderedRulesUnrecoverableOutcomeVectorIsNotTheDonorsDecline(t *testing.T) {
	// Both runs are identical except for the presence marker on c1.
	build := func(presence predictioneval.SuppliedPresence,
		outs ...predictioneval.OrderedRulesOutcome) []predictioneval.OrderedRulesCandidate {
		c1 := orCandidate("c1", 10, orKnownBalance(1000), outs...)
		c1.OutcomesPresence = presence
		if presence != predictioneval.SuppliedKnown {
			c1.OutcomesReason = "the wire frame's outcome vector could not be recovered"
		}
		return []predictioneval.OrderedRulesCandidate{c1, orTwoOutcomePool("c2", 20)}
	}

	t.Run("an unrecoverable vector stops the traversal", func(t *testing.T) {
		ev := predictioneval.EvaluateOrderedRules(
			orProject(t, build(predictioneval.SuppliedMissing), nil), orAlwaysAdmitConfig(), orDraws())
		switch {
		case ev.Status != predictioneval.StatusUnknownInput:
			t.Fatalf("status = %q, want UNKNOWN_INPUT", ev.Status)
		case ev.Reason != predictioneval.ReasonOutcomeVectorNotKnown:
			t.Fatalf("reason = %q, want OUTCOME_VECTOR_NOT_KNOWN", ev.Reason)
		case ev.Selected != nil:
			t.Fatalf("walking past the unknown candidate admitted %+v on the next one", ev.Selected)
		case ev.CandidatesConsumed != 1:
			t.Fatalf("consumed %d candidates, want 1", ev.CandidatesConsumed)
		}
	})

	t.Run("a partially recovered vector is INVALID, not complete", func(t *testing.T) {
		// A vector truncated from three outcomes to two changes EVERY share,
		// because the pool total feeds all of them — so it may not be passed
		// off as the whole of what was there.
		ev := predictioneval.EvaluateOrderedRules(
			orProject(t, build(predictioneval.SuppliedInvalid, orOutcome("A", 4), orOutcome("B", 6)), nil),
			orAlwaysAdmitConfig(), orDraws())
		if ev.Status != predictioneval.StatusUnknownInput ||
			ev.Reason != predictioneval.ReasonOutcomeVectorNotKnown {
			t.Fatalf("status %q reason %q, want UNKNOWN_INPUT / OUTCOME_VECTOR_NOT_KNOWN",
				ev.Status, ev.Reason)
		}
	})

	t.Run("a KNOWN short vector really is the donor's decline", func(t *testing.T) {
		ev := predictioneval.EvaluateOrderedRules(
			orProject(t, build(predictioneval.SuppliedKnown, orOutcome("A", 4)), nil),
			orAlwaysAdmitConfig(), orDraws())
		sel := orMustAdmit(t, ev)
		if sel.CandidateIdentity != "c2" {
			t.Fatalf("admitted on %q, want \"c2\": a vector the caller vouched for and that holds one "+
				"outcome is the donor's own decline, and the scan continues", sel.CandidateIdentity)
		}
		if ev.Visits[0].Verdict != predictioneval.CandidateTooFewOutcomes {
			t.Fatalf("c1 verdict = %q, want TOO_FEW_OUTCOMES", ev.Visits[0].Verdict)
		}
	})

	// A candidate that declares no presence at all is refused at projection.
	bare := orTwoOutcomePool("c1", 10)
	bare.OutcomesPresence = ""
	if _, err := predictioneval.ProjectOrderedRulesStream(
		orSource([]predictioneval.OrderedRulesCandidate{bare}, nil), orAdmission()); !errors.Is(err,
		predictioneval.ErrOrderedRulesVocabulary) {
		t.Fatalf("got %v, want a vocabulary refusal for an undeclared outcome-vector presence", err)
	}
}

// TestOrderedRulesAFullyCutStreamIsDistinguishableFromAnEmptySource pins that
// the boundary reaches the result.
//
// Both runs below report NO_ATTEMPT_IN_SUPPLIED_PREFIX with the same counters.
// One of them is a round a real placement call had already acted on, with every
// supplied candidate removed by the boundary; the other is a source that held
// nothing. Those are opposite pieces of evidence, and a reader must be able to
// tell them apart from the result alone.
func TestOrderedRulesAFullyCutStreamIsDistinguishableFromAnEmptySource(t *testing.T) {
	early := []predictioneval.OrderedRulesIntervention{
		orIntervention("call-1", 5, predictioneval.InterventionAutoCallStarted,
			predictioneval.RelevanceProven, "a placement call had already acted on this round"),
	}
	cut := predictioneval.EvaluateOrderedRules(
		orProject(t, []predictioneval.OrderedRulesCandidate{
			orTwoOutcomePool("c1", 10), orTwoOutcomePool("c2", 20)}, early),
		orAlwaysAdmitConfig(), orDraws())
	empty := predictioneval.EvaluateOrderedRules(
		orProject(t, nil, early), orAlwaysAdmitConfig(), orDraws())

	if cut.Status != predictioneval.StatusNoAttemptInSuppliedPrefix ||
		empty.Status != predictioneval.StatusNoAttemptInSuppliedPrefix {
		t.Fatalf("both runs must reach NO_ATTEMPT: %q and %q", cut.Status, empty.Status)
	}
	if cut.Cutoff.DroppedAtOrAfter != 2 {
		t.Fatalf("the cut result must report the candidates the boundary removed: %+v", cut.Cutoff)
	}
	if empty.Cutoff.DroppedAtOrAfter != 0 {
		t.Fatalf("the empty source removed nothing: %+v", empty.Cutoff)
	}
	if !containsSubstring(cut.Qualifications, "BOUNDARY_REMOVED_CANDIDATES") {
		t.Fatalf("a boundary that emptied the stream must be qualified on the result: %v",
			cut.Qualifications)
	}
	if containsSubstring(empty.Qualifications, "BOUNDARY_REMOVED_CANDIDATES") {
		t.Fatalf("nothing was removed from the empty source: %v", empty.Qualifications)
	}
	// And the boundary itself travels, so the result names the call.
	if cut.Cutoff.Identity != "call-1" || !cut.Cutoff.Established {
		t.Fatalf("the result must carry the boundary it was evaluated under: %+v", cut.Cutoff)
	}
}

// TestOrderedRulesFreeTextIsBoundedLikeIdentifiers pins the budget over the
// fields a caller controls most freely.
//
// A presence reason, a provenance note and a coverage detail are all retained
// by the projection and hashed into its digest. Bounding only the identifiers
// left the one string a caller can make arbitrarily long entirely unbounded,
// which made the declared aggregate budget unenforceable.
func TestOrderedRulesFreeTextIsBoundedLikeIdentifiers(t *testing.T) {
	huge := make([]byte, predictioneval.MaxOrderedRulesIdentifierBytes+1)
	for i := range huge {
		huge[i] = 'x'
	}
	big := string(huge)

	t.Run("a balance reason", func(t *testing.T) {
		c := orTwoOutcomePool("c1", 10)
		c.Balance = predictioneval.SuppliedInt64{Presence: predictioneval.SuppliedMissing, Reason: big}
		_, err := predictioneval.ProjectOrderedRulesStream(
			orSource([]predictioneval.OrderedRulesCandidate{c}, nil), orAdmission())
		if !errors.Is(err, predictioneval.ErrOrderedRulesOverBound) {
			t.Fatalf("got %v, want an over-bound refusal", err)
		}
	})

	t.Run("an outcome points reason", func(t *testing.T) {
		o := orMissingPoints("A")
		o.Points.Reason = big
		c := orCandidate("c1", 10, orKnownBalance(1000), o, orOutcome("B", 6))
		_, err := predictioneval.ProjectOrderedRulesStream(
			orSource([]predictioneval.OrderedRulesCandidate{c}, nil), orAdmission())
		if !errors.Is(err, predictioneval.ErrOrderedRulesOverBound) {
			t.Fatalf("got %v, want an over-bound refusal", err)
		}
	})

	t.Run("a coverage detail", func(t *testing.T) {
		src := orSource([]predictioneval.OrderedRulesCandidate{orTwoOutcomePool("c1", 10)}, nil)
		src.Scope.CoverageDetail = big
		if _, err := predictioneval.ProjectOrderedRulesStream(src, orAdmission()); !errors.Is(err,
			predictioneval.ErrOrderedRulesOverBound) {
			t.Fatalf("got %v, want an over-bound refusal", err)
		}
	})

	t.Run("an intervention detail", func(t *testing.T) {
		ins := []predictioneval.OrderedRulesIntervention{
			orIntervention("call-1", 20, predictioneval.InterventionAutoCallStarted,
				predictioneval.RelevanceProven, big),
		}
		_, err := predictioneval.ProjectOrderedRulesStream(
			orSource([]predictioneval.OrderedRulesCandidate{orTwoOutcomePool("c1", 10)}, ins),
			orAdmission())
		if !errors.Is(err, predictioneval.ErrOrderedRulesOverBound) {
			t.Fatalf("got %v, want an over-bound refusal", err)
		}
	})

	// Non-vacuity: one byte shorter is accepted, so the bound is the bound.
	c := orTwoOutcomePool("c1", 10)
	c.Balance = predictioneval.SuppliedInt64{
		Presence: predictioneval.SuppliedMissing, Reason: big[:len(big)-1]}
	if _, err := predictioneval.ProjectOrderedRulesStream(
		orSource([]predictioneval.OrderedRulesCandidate{c}, nil), orAdmission()); err != nil {
		t.Fatalf("a reason at exactly the bound must be accepted: %v", err)
	}
}

// TestOrderedRulesEqualPositionBoundaryDoesNotDependOnSupplyOrder pins the
// boundary's identity as a function of the FACTS, not of the order they arrived.
//
// Two equally-strong interventions at the same position cut identically, so the
// traversal cannot tell them apart — but the one named as the boundary is
// hashed into the result. Picking whichever came first in the slice would make
// the same set of facts produce different evidence on a reshuffle.
func TestOrderedRulesEqualPositionBoundaryDoesNotDependOnSupplyOrder(t *testing.T) {
	a := orIntervention("call-a", 20, predictioneval.InterventionAutoCallStarted,
		predictioneval.RelevanceProven, "one")
	b := orIntervention("call-b", 20, predictioneval.InterventionManualCallStarted,
		predictioneval.RelevanceProven, "another")
	cs := []predictioneval.OrderedRulesCandidate{orTwoOutcomePool("c1", 10)}

	forward := orProject(t, cs, []predictioneval.OrderedRulesIntervention{a, b})
	reverse := orProject(t, cs, []predictioneval.OrderedRulesIntervention{b, a})
	if forward.Cutoff != reverse.Cutoff {
		t.Fatalf("the boundary changed with supply order:\n forward = %+v\n reverse = %+v",
			forward.Cutoff, reverse.Cutoff)
	}
	if forward.SelectionDigest != reverse.SelectionDigest {
		t.Fatal("the same facts in a different order produced a different stream digest")
	}
}

// TestOrderedRulesUnknownCoverageIsRefused pins that a source must say how
// complete it is.
//
// Without that declaration, "no intervention was supplied" and "the
// intervention was never collected" are the same input — and the model would
// silently read the second as the first.
func TestOrderedRulesUnknownCoverageIsRefused(t *testing.T) {
	src := orSource([]predictioneval.OrderedRulesCandidate{orTwoOutcomePool("c1", 10)}, nil)
	src.Scope.Coverage = predictioneval.CoverageUnknown
	if _, err := predictioneval.ProjectOrderedRulesStream(src, orAdmission()); !errors.Is(err,
		predictioneval.ErrOrderedRulesUnknownCoverage) {
		t.Fatalf("got %v, want an unknown-coverage refusal", err)
	}

	// A DECLARED gap is accepted and qualified — the limitation travels with the
	// stream instead of blocking it.
	src.Scope.Coverage = predictioneval.CoverageTruncatedPrefix
	stream, err := predictioneval.ProjectOrderedRulesStream(src, orAdmission())
	if err != nil {
		t.Fatalf("a declared gap must be evaluable: %v", err)
	}
	if !containsSubstring(stream.Qualifications, "SOURCE_COVERAGE_NOT_COMPLETE") {
		t.Fatalf("a truncated prefix must be qualified: %v", stream.Qualifications)
	}
	ev := predictioneval.EvaluateOrderedRules(stream, orAlwaysAdmitConfig(), orDraws())
	if !containsSubstring(ev.Qualifications, "SOURCE_COVERAGE_NOT_COMPLETE") {
		t.Fatalf("the qualification must reach the result: %v", ev.Qualifications)
	}
}

// TestOrderedRulesCalculateOnlyViewIsLabelledNotPromoted pins that a
// decision-time slice is never presented as a recovered wire stream.
//
// The narrowing is legitimate and the label is what keeps it honest: calling
// such a slice a create/update stream would quietly shrink a promised full
// stream down to the part that happens to be recoverable.
func TestOrderedRulesCalculateOnlyViewIsLabelledNotPromoted(t *testing.T) {
	c := orTwoOutcomePool("c1", 10)
	c.SourceKind = predictioneval.SourceKindCalculateSnapshot
	adm := orAdmission()
	adm.ViewKind = predictioneval.ViewCalculateOnly

	stream, err := predictioneval.ProjectOrderedRulesStream(
		orSource([]predictioneval.OrderedRulesCandidate{c}, nil), adm)
	if err != nil {
		t.Fatalf("a calculate-only view must be evaluable: %v", err)
	}
	if !containsSubstring(stream.Qualifications, "CALCULATE_ONLY_VIEW") {
		t.Fatalf("the view must be labelled: %v", stream.Qualifications)
	}

	// The same candidate presented as a channel update stream is refused.
	if _, err := predictioneval.ProjectOrderedRulesStream(
		orSource([]predictioneval.OrderedRulesCandidate{c}, nil), orAdmission()); !errors.Is(err,
		predictioneval.ErrOrderedRulesViewMismatch) {
		t.Fatalf("got %v, want a view-mismatch refusal", err)
	}
}

func containsSubstring(list []string, want string) bool {
	for _, s := range list {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}
