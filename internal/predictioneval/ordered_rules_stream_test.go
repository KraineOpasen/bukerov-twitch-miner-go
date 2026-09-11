package predictioneval_test

// THE BOUNDARY, THE IDENTITIES AND THE BINDINGS.
//
// The mechanism is only as honest as the stream it runs on. These tests hold
// the projection to the three things it exists to guarantee: that the traversal
// stops before the world was acted on, that nothing in the stream was joined or
// back-dated into existence, and that a result cannot be re-attached to inputs
// it was not computed from.

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
)

func orIntervention(id string, pos int64, kind predictioneval.OrderedRulesInterventionKind,
	rel predictioneval.OrderedRulesRelevance, detail string) predictioneval.OrderedRulesIntervention {
	return predictioneval.OrderedRulesIntervention{
		Identity: id, Position: pos, HasPosition: true, Kind: kind, Relevance: rel, Detail: detail,
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
		Presence:               predictioneval.SuppliedKnown,
		Value:                  5000,
		Provenance:             "a later schedule read on the same channel",
		AvailableAtPosition:    25,
		HasAvailableAtPosition: true,
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

// TestOrderedRulesKnownValueMustDeclareItsAvailability pins the presence of the
// availability position, not merely its value.
//
// The back-dating rule above is only as strong as the position it compares
// against, and that position is an int64 whose zero is a LEGITIMATE causal
// position. So a value that never declared one — a payload that simply omits
// the field, decoded into a zero — used to clear the comparison for every
// candidate at position zero or later, which is the whole interval of any
// stream starting at zero. The guard read as enforced and was not.
//
// This is the same failure SuppliedPresence exists to prevent, one level down:
// a number whose zero means two different things cannot carry the distinction.
func TestOrderedRulesKnownValueMustDeclareItsAvailability(t *testing.T) {
	// The exact shape a wire projection leaves behind: the field is absent.
	var decoded predictioneval.SuppliedInt64
	if err := json.Unmarshal([]byte(
		`{"presence":"KNOWN","value":1000,"provenance":"wire row"}`), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.HasAvailableAtPosition || decoded.AvailableAtPosition != 0 {
		t.Fatalf("this case's premise is that the omitted field decodes to an undeclared zero; got "+
			"%d declared=%v", decoded.AvailableAtPosition, decoded.HasAvailableAtPosition)
	}

	t.Run("an undeclared balance", func(t *testing.T) {
		c := orCandidate("c1", 500, decoded, orOutcome("A", 4), orOutcome("B", 6))
		_, err := predictioneval.ProjectOrderedRulesStream(
			orSource([]predictioneval.OrderedRulesCandidate{c}, nil), orAdmission())
		if !errors.Is(err, predictioneval.ErrOrderedRulesScopeIncomplete) {
			t.Fatalf("got %v, want an incomplete-scope refusal: a KNOWN value that never said when it "+
				"became available cannot be checked against the candidate that would read it", err)
		}
	})

	t.Run("undeclared outcome points", func(t *testing.T) {
		o := orOutcome("A", 4)
		o.Points.HasAvailableAtPosition = false
		c := orCandidate("c1", 500, orKnownBalance(1000), o, orOutcome("B", 6))
		_, err := predictioneval.ProjectOrderedRulesStream(
			orSource([]predictioneval.OrderedRulesCandidate{c}, nil), orAdmission())
		if !errors.Is(err, predictioneval.ErrOrderedRulesScopeIncomplete) {
			t.Fatalf("got %v, want an incomplete-scope refusal; the rule is on every supplied field, "+
				"not only the balance", err)
		}
	})

	// A non-KNOWN value declares nothing and is not asked to: there is no value
	// whose availability could matter.
	t.Run("a MISSING value needs no availability", func(t *testing.T) {
		c := orCandidate("c1", 500, orMissingBalance(), orOutcome("A", 4), orOutcome("B", 6))
		if _, err := predictioneval.ProjectOrderedRulesStream(
			orSource([]predictioneval.OrderedRulesCandidate{c}, nil), orAdmission()); err != nil {
			t.Fatalf("a MISSING balance carries no value to back-date: %v", err)
		}
	})

	// Non-vacuity, and the point of the flag: the SAME zero, once DECLARED, is
	// accepted — so the refusal is about the declaration and not about the
	// number. Position zero stays a usable position.
	t.Run("a declared zero is accepted", func(t *testing.T) {
		declared := decoded
		declared.HasAvailableAtPosition = true
		c := orCandidate("c1", 0, declared, orOutcome("A", 4), orOutcome("B", 6))
		if _, err := predictioneval.ProjectOrderedRulesStream(
			orSource([]predictioneval.OrderedRulesCandidate{c}, nil), orAdmission()); err != nil {
			t.Fatalf("a value declared available from position zero must be accepted: %v", err)
		}
	})
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

	// The admission manifest's source list. Every other supplied collection is
	// bounded by COUNT before its bytes are charged; this one was bounded only
	// by the aggregate byte budget, which charges payload — so any number of
	// EMPTY references passed for free while each still had to be retained,
	// copied and digested.
	t.Run("too many admission source references", func(t *testing.T) {
		adm := orAdmission()
		adm.SourceReferences = make([]string, predictioneval.MaxOrderedRulesSourceReferences+1)
		stream, err := predictioneval.ProjectOrderedRulesStream(
			orSource([]predictioneval.OrderedRulesCandidate{orTwoOutcomePool("c1", 10)}, nil), adm)
		if !errors.Is(err, predictioneval.ErrOrderedRulesOverBound) {
			t.Fatalf("got %v, want an over-bound refusal; empty strings carry no payload bytes, so a "+
				"byte budget alone never reaches them", err)
		}
		if len(stream.Admission.SourceReferences) != 0 {
			t.Fatalf("a refused projection must retain nothing, not %d references",
				len(stream.Admission.SourceReferences))
		}

		// Non-vacuity: exactly at the bound is accepted, so this is the bound.
		adm.SourceReferences = make([]string, predictioneval.MaxOrderedRulesSourceReferences)
		if _, err := predictioneval.ProjectOrderedRulesStream(
			orSource([]predictioneval.OrderedRulesCandidate{orTwoOutcomePool("c1", 10)}, nil),
			adm); err != nil {
			t.Fatalf("a list exactly at the bound must be accepted: %v", err)
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

// TestOrderedRulesOversizedInputIsRefusedBeforeItIsRead pins the ORDER of the
// bound checks against the work they bound.
//
// EvaluateOrderedRules is exported and takes the stream by value, so it can be
// handed one the projection never produced. It already refuses such a stream on
// the SelectionDigest — but that comparison is circular, since detecting a
// forgery requires digesting the forgery first. The three whole-input digests
// therefore used to run BEFORE every bound, and a draw trace eight times past
// MaxOrderedRulesDrawWords was hashed in full and only then refused for being
// too long: the bound was consulted after the work it exists to bound.
//
// The assertions below are on the RESULT, never on elapsed time. A refusal that
// declined to read the input carries no whole-input digest, so the absence of
// one is the observable proof that nothing walked the input — and it is a
// deterministic fact rather than a measurement.
func TestOrderedRulesOversizedInputIsRefusedBeforeItIsRead(t *testing.T) {
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 40, 100, 0, 10)}, 95, 100, 0, 10)

	// forged builds a stream the projection never produced. Its SelectionDigest
	// is deliberately wrong, so if the shape gate does NOT fire, the digest
	// mismatch is what refuses it — which is exactly the confusion being pinned.
	forged := func(candidates int) predictioneval.OrderedRulesStream {
		cs := make([]predictioneval.OrderedRulesCandidate, candidates)
		for i := range cs {
			cs[i] = orCandidate("c"+itoaTest(i), int64(i), orKnownBalance(1000),
				orOutcome("A", 4), orOutcome("B", 6))
		}
		return predictioneval.OrderedRulesStream{
			ContractVersion: predictioneval.OrderedRulesStreamContractVersion,
			Candidates:      cs,
			SelectionDigest: "a digest this stream does not have",
		}
	}

	unread := func(t *testing.T, ev predictioneval.OrderedRulesEvaluation, wantReason string) {
		t.Helper()
		if ev.Status != predictioneval.StatusRefused || ev.Reason != wantReason {
			t.Fatalf("status %q reason %q, want REFUSED / %s", ev.Status, ev.Reason, wantReason)
		}
		if ev.StreamDigest != "" || ev.ConfigDigest != "" || ev.EntropyDigest != "" {
			t.Fatalf("a refusal that never read the input must attest to nothing about it; got "+
				"stream=%q config=%q entropy=%q", ev.StreamDigest, ev.ConfigDigest, ev.EntropyDigest)
		}
		if ev.Selected != nil {
			t.Fatal("a refused evaluation must decide nothing")
		}
	}

	t.Run("candidates past the bound", func(t *testing.T) {
		unread(t, predictioneval.EvaluateOrderedRules(
			forged(predictioneval.MaxOrderedRulesCandidates+1), cfg, orDraws()),
			predictioneval.ReasonStreamShapeOverBound)
	})

	t.Run("one candidate's outcome vector past the bound", func(t *testing.T) {
		outs := make([]predictioneval.OrderedRulesOutcome, predictioneval.MaxOrderedRulesOutcomes+1)
		for i := range outs {
			outs[i] = orOutcome("o"+itoaTest(i), int64(i+1))
		}
		st := forged(1)
		st.Candidates[0].Outcomes = outs
		unread(t, predictioneval.EvaluateOrderedRules(st, cfg, orDraws()),
			predictioneval.ReasonStreamShapeOverBound)
	})

	t.Run("qualifications past the bound", func(t *testing.T) {
		st := forged(1)
		st.Qualifications = make([]string, predictioneval.MaxOrderedRulesQualifications+1)
		unread(t, predictioneval.EvaluateOrderedRules(st, cfg, orDraws()),
			predictioneval.ReasonStreamShapeOverBound)
	})

	t.Run("admission source references past the bound", func(t *testing.T) {
		st := forged(1)
		st.Admission.SourceReferences = make([]string,
			predictioneval.MaxOrderedRulesSourceReferences+1)
		unread(t, predictioneval.EvaluateOrderedRules(st, cfg, orDraws()),
			predictioneval.ReasonStreamShapeOverBound)
	})

	// The sharpest case: a bound that already existed, checked after the hash
	// of the very thing it bounds. The trace is over MaxOrderedRulesDrawWords,
	// and the stream is forged — so the reason reported says which check ran
	// first. Before the repair this was the digest mismatch.
	t.Run("an over-bound trace reports its own bound, not a digest mismatch", func(t *testing.T) {
		unread(t, predictioneval.EvaluateOrderedRules(forged(1), cfg,
			orDraws(make([]uint64, predictioneval.MaxOrderedRulesDrawWords+1)...)),
			predictioneval.ReasonDrawWordsOverBound)
	})

	t.Run("rules past the bound", func(t *testing.T) {
		rules := make([]predictioneval.OrderedRule, predictioneval.MaxOrderedRulesRules+1)
		for i := range rules {
			rules[i] = orRule(predictioneval.ComparatorLe, 100, 100, 0, 10)
		}
		unread(t, predictioneval.EvaluateOrderedRules(forged(1),
			orConfig(rules, 95, 100, 0, 10), orDraws()),
			predictioneval.ReasonRuleCountOverBound)
	})

	// The per-string limit, which the aggregate does NOT imply. The projection
	// refuses a candidate provenance past MaxOrderedRulesIdentifierBytes
	// outright, and one 64 MiB note sits well inside a 128 MiB budget — so a
	// gate that checked only the total left exactly that input to be hashed.
	t.Run("one string past the per-string limit", func(t *testing.T) {
		over := strings.Repeat("x", predictioneval.MaxOrderedRulesIdentifierBytes+1)
		st := forged(1)
		st.Candidates[0].Provenance = over

		// The premise: the projection refuses this very value.
		c := orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6))
		c.Provenance = over
		if _, err := predictioneval.ProjectOrderedRulesStream(
			orSource([]predictioneval.OrderedRulesCandidate{c}, nil), orAdmission()); err == nil {
			t.Fatal("this case's premise is that the projection refuses this string; it did not")
		}
		unread(t, predictioneval.EvaluateOrderedRules(st, cfg, orDraws()),
			predictioneval.ReasonStreamTextOverBound)

		// Non-vacuity: one byte shorter is admissible, so the bound is the bound.
		st.Candidates[0].Provenance = over[:len(over)-1]
		if ev := predictioneval.EvaluateOrderedRules(st, cfg, orDraws()); ev.Reason !=
			predictioneval.ReasonStreamDigestMismatch {
			t.Fatalf("reason %q, want %s: a string exactly at the limit is admissible",
				ev.Reason, predictioneval.ReasonStreamDigestMismatch)
		}
	})

	// The same length-before-meaning rule on the EVALUATOR side. The projection
	// gates its vocabulary, but EvaluateOrderedRules can be handed a stream the
	// projection never saw, and there the value is hashed by the digests and
	// then quoted by the projection validators the invariant pass calls.
	// Measured before this gate, a 64 MiB contract version cost about two
	// seconds across the two calls a forgery needs, and was refused only at the
	// end, for being impossible rather than for being enormous.
	t.Run("vocabulary past the per-string limit", func(t *testing.T) {
		big := strings.Repeat("x", predictioneval.MaxOrderedRulesIdentifierBytes+1)
		for _, tc := range []struct {
			name   string
			break_ func(st *predictioneval.OrderedRulesStream)
		}{
			{"stream contract version", func(st *predictioneval.OrderedRulesStream) {
				st.ContractVersion = big
			}},
			{"scope contract version", func(st *predictioneval.OrderedRulesStream) {
				st.Scope.SourceContractVersion = big
			}},
			{"scope coverage", func(st *predictioneval.OrderedRulesStream) {
				st.Scope.Coverage = predictioneval.OrderedRulesCoverage(big)
			}},
			{"admission view kind", func(st *predictioneval.OrderedRulesStream) {
				st.Admission.ViewKind = predictioneval.OrderedRulesViewKind(big)
			}},
			{"cutoff kind", func(st *predictioneval.OrderedRulesStream) {
				st.Cutoff.Kind = predictioneval.OrderedRulesInterventionKind(big)
			}},
			{"cutoff basis", func(st *predictioneval.OrderedRulesStream) {
				st.Cutoff.Basis = predictioneval.OrderedRulesCutoffBasis(big)
			}},
			{"candidate source kind", func(st *predictioneval.OrderedRulesStream) {
				st.Candidates[0].SourceKind = predictioneval.OrderedRulesSourceKind(big)
			}},
			{"candidate membership", func(st *predictioneval.OrderedRulesStream) {
				st.Candidates[0].EpisodeMembership = predictioneval.OrderedRulesMembership(big)
			}},
			{"outcome-vector presence", func(st *predictioneval.OrderedRulesStream) {
				st.Candidates[0].OutcomesPresence = predictioneval.SuppliedPresence(big)
			}},
			{"balance presence", func(st *predictioneval.OrderedRulesStream) {
				st.Candidates[0].Balance.Presence = predictioneval.SuppliedPresence(big)
			}},
			{"outcome points presence", func(st *predictioneval.OrderedRulesStream) {
				st.Candidates[0].Outcomes[0].Points.Presence = predictioneval.SuppliedPresence(big)
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				st := forged(1)
				tc.break_(&st)
				unread(t, predictioneval.EvaluateOrderedRules(st, cfg, orDraws()),
					predictioneval.ReasonStreamTextOverBound)
			})
		}

		t.Run("entropy semantics version", func(t *testing.T) {
			d := orDraws()
			d.EntropySemanticsVersion = big
			unread(t, predictioneval.EvaluateOrderedRules(forged(1), cfg, d),
				predictioneval.ReasonStreamTextOverBound)
		})

		t.Run("a rule comparator", func(t *testing.T) {
			rules := []predictioneval.OrderedRule{orRule(predictioneval.ComparatorLe, 40, 100, 0, 10)}
			rules[0].Comparator = predictioneval.OrderedRuleComparator(big)
			unread(t, predictioneval.EvaluateOrderedRules(forged(1),
				orConfig(rules, 95, 100, 0, 10), orDraws()),
				predictioneval.ReasonStreamTextOverBound)
		})
	})

	// The retained TEXT in aggregate. Every field below points at the SAME
	// string, so the budget is exceeded by what would be retained while the
	// test allocates one copy of it — the sum is over lengths, and Go strings
	// do not copy on assignment.
	//
	// The fields chosen are the cheapest construction, not the only one: they
	// are the free-form singletons that carry no per-string limit, so a handful
	// of them reach the budget with one shared string.
	//
	// An earlier version of this comment claimed that bounding those two would
	// make the aggregate check UNREACHABLE, and that was wrong — asserted
	// without doing the arithmetic. The individually bounded slots alone exceed
	// the budget, so the check stays reachable and this case must be rewritten
	// rather than removed if the singletons are ever bounded. The subtest below
	// does that arithmetic instead of asserting it, so the claim cannot rot the
	// way the first one did.
	// These seven were the LAST charged strings with no individual bound, on
	// either side. The projection gained one; this gate did not, and the gap
	// was not academic: a namespace just under the aggregate passed here, was
	// hashed by all three whole-input digests, and was only then refused by the
	// invariant pass — 646 ms and 251 MB allocated to say no, from an input the
	// projection rejects on its length alone. They are bounded here now, so
	// none of them can reach the aggregate at all, and the sibling case below
	// carries the reachability claim instead.
	t.Run("a retained string past the per-string limit", func(t *testing.T) {
		over := strings.Repeat("x", predictioneval.MaxOrderedRulesIdentifierBytes+1)
		for _, tc := range []struct {
			name  string
			apply func(*predictioneval.OrderedRulesStream)
		}{
			{"scope namespace", func(st *predictioneval.OrderedRulesStream) { st.Scope.Namespace = over }},
			{"scope episode id", func(st *predictioneval.OrderedRulesStream) { st.Scope.EpisodeID = over }},
			{"scope account context", func(st *predictioneval.OrderedRulesStream) { st.Scope.AccountContext = over }},
			{"scope association evidence", func(st *predictioneval.OrderedRulesStream) { st.Scope.AssociationEvidence = over }},
			{"admission manifest id", func(st *predictioneval.OrderedRulesStream) { st.Admission.ManifestID = over }},
			{"admission population", func(st *predictioneval.OrderedRulesStream) { st.Admission.Population = over }},
			{"admission order basis", func(st *predictioneval.OrderedRulesStream) { st.Admission.OrderBasis = over }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				st := forged(1)
				tc.apply(&st)
				unread(t, predictioneval.EvaluateOrderedRules(st, cfg, orDraws()),
					predictioneval.ReasonStreamTextOverBound)
			})
		}
	})

	// The reachability claim above, computed rather than believed.
	//
	// Slot counts per candidate and per outcome are read off the budget walk:
	// a candidate bounds its identity, provenance, outcome reason, source kind,
	// membership, outcome-vector presence and its balance's presence,
	// provenance and reason; an outcome bounds its identity and its points'
	// presence, provenance and reason.
	t.Run("the aggregate check stays reachable without the free-form fields", func(t *testing.T) {
		// Only fields that are both bounded AND charged to the stream ceiling
		// count here. The derived text — qualifications, the boundary, the
		// stream's own contract version — is bounded but deliberately not
		// charged, and the config and trace have their own ceiling, so none of
		// them can carry the stream total over.
		const perCandidate, perOutcome, fixedStream = 9, 4, 4
		slots := predictioneval.MaxOrderedRulesCandidates*perCandidate +
			predictioneval.MaxOrderedRulesCandidates*predictioneval.MaxOrderedRulesOutcomes*perOutcome +
			predictioneval.MaxOrderedRulesSourceReferences + fixedStream
		capacity := int64(slots) * int64(predictioneval.MaxOrderedRulesIdentifierBytes)
		if capacity <= predictioneval.MaxOrderedRulesAggregateBytes {
			t.Fatalf("the individually bounded fields top out at %d bytes against a budget of %d, so "+
				"the aggregate check would be reachable only through the free-form singletons. The "+
				"case above must then be the only one that can reach it — check that it still can.",
				capacity, int64(predictioneval.MaxOrderedRulesAggregateBytes))
		}
	})

	// Non-vacuity: EXACTLY at the counts, the shape gate does not fire and the
	// forged stream falls through to the digest mismatch it deserves. So the
	// gate refuses oversized input rather than everything.
	t.Run("exactly at the bounds the shape gate stands aside", func(t *testing.T) {
		st := forged(predictioneval.MaxOrderedRulesCandidates)
		st.Qualifications = make([]string, predictioneval.MaxOrderedRulesQualifications)
		st.Admission.SourceReferences = make([]string,
			predictioneval.MaxOrderedRulesSourceReferences)
		ev := predictioneval.EvaluateOrderedRules(st, cfg,
			orDraws(make([]uint64, predictioneval.MaxOrderedRulesDrawWords)...))
		if ev.Reason != predictioneval.ReasonStreamDigestMismatch {
			t.Fatalf("reason %q, want %s: an input at its bounds is admissible and must be judged "+
				"on its contents", ev.Reason, predictioneval.ReasonStreamDigestMismatch)
		}
		if ev.StreamDigest == "" {
			t.Fatal("an input that passed the shape gate WAS read, so it must carry its digest")
		}
	})
}

// orRewriteRemovalCount sets a stream's removal count and rewrites the boundary
// qualification to match, so the forgery stays self-consistent and is refused
// for the count itself rather than for a qualification that no longer agrees.
func orRewriteRemovalCount(t *testing.T, st predictioneval.OrderedRulesStream,
	count int) predictioneval.OrderedRulesStream {
	t.Helper()
	was := st.Cutoff.DroppedAtOrAfter
	if was <= 0 {
		t.Fatalf("premise: the boundary must really have removed something; it removed %d", was)
	}
	old, now := "count="+itoaTest(was), "count="+itoaTest(count)
	rewritten := false
	for i, q := range st.Qualifications {
		if strings.HasSuffix(q, old) {
			st.Qualifications[i] = strings.TrimSuffix(q, old) + now
			rewritten = true
		}
	}
	if !rewritten {
		t.Fatalf("premise: no boundary qualification carried %q, so rewriting it proves nothing; "+
			"qualifications were %v", old, st.Qualifications)
	}
	st.Cutoff.DroppedAtOrAfter = count
	return st
}

// TestOrderedRulesAMatchingDigestIsNotProofOfProjection pins the evaluator
// against a stream that is internally consistent and still impossible.
//
// The digest was being read as provenance, and it cannot be. It is unkeyed,
// deterministic and computed over exported fields, so any caller able to build
// the struct can compute the matching value — and it is worse than that in
// practice: a refusal RETURNS the recomputed digest in StreamDigest, so the
// attack is two calls and no cryptography. Copy the value the first call hands
// back into SelectionDigest and the second call verifies.
//
// Hiding the digest from refusals would not fix it. The algorithm is in this
// repository, and the type carries JSON tags precisely so a projected stream
// can be serialized and read back — a check a legitimate reader can repeat is
// one an illegitimate reader can repeat. What makes a stream safe to traverse
// is checking it, so the projection's invariants are re-established on ingest.
//
// Every case below starts from a stream the projection REALLY produced and
// breaks exactly one thing. That matters: a hand-built base would let a case
// pass for some unrelated reason it was never shaped correctly to begin with,
// which is precisely how an earlier version of this test fooled itself.
func TestOrderedRulesAMatchingDigestIsNotProofOfProjection(t *testing.T) {
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 40, 100, 0, 10)}, 95, 100, 0, 10)
	candidates := func() []predictioneval.OrderedRulesCandidate {
		return []predictioneval.OrderedRulesCandidate{
			orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6))}
	}

	// selfConsistent runs the oracle: take the digest the evaluator publishes
	// on refusal, put it back, and hand the same stream in again.
	selfConsistent := func(t *testing.T, st predictioneval.OrderedRulesStream) predictioneval.OrderedRulesEvaluation {
		t.Helper()
		st.SelectionDigest = "deliberately wrong"
		first := predictioneval.EvaluateOrderedRules(st, cfg, orDraws())
		if first.Reason != predictioneval.ReasonStreamDigestMismatch || first.StreamDigest == "" {
			t.Fatalf("the probe must reach the digest comparison and be refused by it; got reason %q",
				first.Reason)
		}
		st.SelectionDigest = first.StreamDigest
		return predictioneval.EvaluateOrderedRules(st, cfg, orDraws())
	}

	// cut is a stream the projection produced WITH a boundary, so its cutoff
	// and qualifications are genuine rather than invented here.
	cut := func(t *testing.T) predictioneval.OrderedRulesStream {
		t.Helper()
		return orProject(t, candidates(), []predictioneval.OrderedRulesIntervention{
			orIntervention("call-1", 5, predictioneval.InterventionAutoCallStarted,
				predictioneval.RelevanceProven, "a real call")})
	}

	for _, tc := range []struct {
		name   string
		stream func(t *testing.T) predictioneval.OrderedRulesStream
	}{
		{"a KNOWN balance that never declared its availability", func(t *testing.T) predictioneval.OrderedRulesStream {
			st := orProject(t, candidates(), nil)
			st.Candidates[0].Balance.HasAvailableAtPosition = false
			return st
		}},
		// The declaration matters HERE more than in the projection. Enforcing it
		// only there left the evaluator comparing a position the caller never
		// declared: the projection refused the input while this path admitted
		// the same stream, reached WOULD_ATTEMPT, and handed the first
		// opportunity to a candidate whose position was a decoded zero.
		{"a pool naming the same outcome twice", func(t *testing.T) predictioneval.OrderedRulesStream {
			st := orProject(t, candidates(), nil)
			st.Candidates[0].Outcomes[1].Identity = st.Candidates[0].Outcomes[0].Identity
			return st
		}},
		{"a candidate whose causal position was never declared", func(t *testing.T) predictioneval.OrderedRulesStream {
			st := orProject(t, candidates(), nil)
			st.Candidates[0].HasPosition = false
			return st
		}},
		{"a scope whose interval endpoints were never declared", func(t *testing.T) predictioneval.OrderedRulesStream {
			st := orProject(t, candidates(), nil)
			st.Scope.HasInterval = false
			return st
		}},
		{"a balance available only AFTER the candidate", func(t *testing.T) predictioneval.OrderedRulesStream {
			st := orProject(t, candidates(), nil)
			st.Candidates[0].Balance.AvailableAtPosition = 25
			return st
		}},
		{"outcome points that never declared availability", func(t *testing.T) predictioneval.OrderedRulesStream {
			st := orProject(t, candidates(), nil)
			st.Candidates[0].Outcomes[0].Points.HasAvailableAtPosition = false
			return st
		}},
		{"membership that was never proven", func(t *testing.T) predictioneval.OrderedRulesStream {
			st := orProject(t, candidates(), nil)
			st.Candidates[0].EpisodeMembership = predictioneval.MembershipUnknown
			return st
		}},
		{"two candidates the causal order cannot separate", func(t *testing.T) predictioneval.OrderedRulesStream {
			st := orProject(t, candidates(), nil)
			st.Candidates = append(st.Candidates,
				orCandidate("c2", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)))
			return st
		}},
		{"a repeated identity", func(t *testing.T) predictioneval.OrderedRulesStream {
			st := orProject(t, candidates(), nil)
			st.Candidates = append(st.Candidates,
				orCandidate("c1", 20, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)))
			return st
		}},
		{"a scope missing its association evidence", func(t *testing.T) predictioneval.OrderedRulesStream {
			st := orProject(t, candidates(), nil)
			st.Scope.AssociationEvidence = ""
			return st
		}},
		{"a snapshot inside a channel-candidate view", func(t *testing.T) predictioneval.OrderedRulesStream {
			st := orProject(t, candidates(), nil)
			st.Candidates[0].SourceKind = predictioneval.SourceKindCalculateSnapshot
			return st
		}},

		// The DERIVED fields. A caller handing these in is asserting facts
		// about interventions and coverage the stream no longer carries, so
		// the only defence is that the assertion be internally possible and
		// exactly what this stream's own fields imply.
		{"a candidate retained past the boundary that cut it", func(t *testing.T) predictioneval.OrderedRulesStream {
			st := cut(t)
			st.Candidates = append(st.Candidates, candidates()...)
			return st
		}},
		{"an unestablished boundary claiming a factual basis", func(t *testing.T) predictioneval.OrderedRulesStream {
			st := orProject(t, candidates(), nil)
			st.Cutoff.Basis = predictioneval.CutoffFactualIntervention
			return st
		}},
		{"an unestablished boundary that still names an intervention", func(t *testing.T) predictioneval.OrderedRulesStream {
			st := orProject(t, candidates(), nil)
			st.Cutoff.Identity = "a call that was never supplied"
			return st
		}},
		{"an unestablished boundary that claims it removed candidates", func(t *testing.T) predictioneval.OrderedRulesStream {
			st := orProject(t, candidates(), nil)
			st.Cutoff.DroppedAtOrAfter = 3
			return st
		}},
		{"an established boundary with no identity", func(t *testing.T) predictioneval.OrderedRulesStream {
			st := cut(t)
			st.Cutoff.Identity = ""
			return st
		}},
		{"an established boundary with a kind outside the vocabulary", func(t *testing.T) predictioneval.OrderedRulesStream {
			st := cut(t)
			st.Cutoff.Kind = "SOMETHING_ELSE"
			return st
		}},
		{"an established boundary outside the declared interval", func(t *testing.T) predictioneval.OrderedRulesStream {
			st := cut(t)
			st.Cutoff.Position = st.Scope.IntervalToPosition + 1
			return st
		}},
		{"a removal count that cannot be negative", func(t *testing.T) predictioneval.OrderedRulesStream {
			st := cut(t)
			st.Cutoff.DroppedAtOrAfter = -1
			return st
		}},
		{"more removed than positions exist at or after the boundary", func(t *testing.T) predictioneval.OrderedRulesStream {
			// The boundary sits on the interval's LAST position, so exactly one
			// position is removable however large the source was. Both other
			// removal bounds pass here: two is far under the candidate ceiling.
			st := orProject(t, []predictioneval.OrderedRulesCandidate{
				orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
				orCandidate("c2", 10000, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
			}, []predictioneval.OrderedRulesIntervention{
				orIntervention("call-1", 10000, predictioneval.InterventionAutoCallStarted,
					predictioneval.RelevanceProven, "a real call")})
			if st.Cutoff.Position != st.Scope.IntervalToPosition {
				t.Fatalf("premise: the boundary must sit on the last position; %d vs %d",
					st.Cutoff.Position, st.Scope.IntervalToPosition)
			}
			return orRewriteRemovalCount(t, st, 2)
		}},
		{"more removed than any admissible source could hold", func(t *testing.T) predictioneval.OrderedRulesStream {
			return orRewriteRemovalCount(t, cut(t), predictioneval.MaxOrderedRulesCandidates+1)
		}},
		{"removed plus retained past what a source could hold", func(t *testing.T) predictioneval.OrderedRulesStream {
			// Each side is individually admissible; their SUM is not, because
			// every source candidate is either retained or counted as removed.
			// One candidate before the boundary and one after it, so the
			// boundary genuinely removed something and genuinely kept
			// something.
			st := orProject(t, []predictioneval.OrderedRulesCandidate{
				orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
				orCandidate("c2", 20, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
			}, []predictioneval.OrderedRulesIntervention{
				orIntervention("call-1", 15, predictioneval.InterventionAutoCallStarted,
					predictioneval.RelevanceProven, "a real call")})
			if len(st.Candidates) != 1 || st.Cutoff.DroppedAtOrAfter != 1 {
				t.Fatalf("premise: one kept and one removed; kept %d, removed %d",
					len(st.Candidates), st.Cutoff.DroppedAtOrAfter)
			}
			return orRewriteRemovalCount(t, st, predictioneval.MaxOrderedRulesCandidates)
		}},
		{"incomplete coverage without the limitation it requires", func(t *testing.T) predictioneval.OrderedRulesStream {
			src := orSource(candidates(), nil)
			src.Scope.Coverage = predictioneval.CoverageGapsPresent
			st, err := predictioneval.ProjectOrderedRulesStream(src, orAdmission())
			if err != nil {
				t.Fatalf("project: %v", err)
			}
			if len(st.Qualifications) < 2 {
				t.Fatalf("premise: incomplete coverage must carry its own qualification; got %v",
					st.Qualifications)
			}
			// Strip the coverage limitation and keep the rest.
			st.Qualifications = st.Qualifications[1:]
			return st
		}},
		{"a limitation the evidence does not support", func(t *testing.T) predictioneval.OrderedRulesStream {
			st := orProject(t, candidates(), nil)
			st.Qualifications = append(st.Qualifications, "FABRICATED_LIMITATION: invented here")
			return st
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := selfConsistent(t, tc.stream(t))
			if ev.Status != predictioneval.StatusRefused ||
				ev.Reason != predictioneval.ReasonStreamInvariantViolated {
				t.Fatalf("status %q reason %q, want REFUSED / %s: the digest verified, so only a "+
					"revalidation of the projection's invariants can stop this",
					ev.Status, ev.Reason, predictioneval.ReasonStreamInvariantViolated)
			}
			if ev.Selected != nil {
				t.Fatal("a refused evaluation must decide nothing")
			}
		})
	}

	// Non-vacuity in both directions. A projected stream still evaluates — so
	// the pass refuses the impossible rather than everything — and the SAME
	// stream still survives the oracle round trip, which proves the refusals
	// above come from the invariants and not from a digest that stopped
	// matching. The second half is what caught this test lying to itself once
	// already, when its base stream was hand-built and never projection-shaped.
	t.Run("a projected stream still evaluates", func(t *testing.T) {
		if ev := predictioneval.EvaluateOrderedRules(orProject(t, candidates(), nil), cfg,
			orDraws()); ev.Status != predictioneval.StatusWouldAttempt {
			t.Fatalf("status %q reason %q, want WOULD_ATTEMPT", ev.Status, ev.Reason)
		}
	})

	t.Run("an unbroken stream survives the oracle", func(t *testing.T) {
		if ev := selfConsistent(t, orProject(t, candidates(), nil)); ev.Status !=
			predictioneval.StatusWouldAttempt {
			t.Fatalf("status %q reason %q, want WOULD_ATTEMPT: a stream that breaks no invariant "+
				"must pass, or the cases above prove nothing about WHICH check refused them",
				ev.Status, ev.Reason)
		}
	})
}

// TestOrderedRulesAProjectedStreamIsAlwaysAdmissible pins the invariant the
// other budget cases are written against, in the direction that was false.
//
// The evaluator's budget mirrors the projection's so that an input the
// projection would have admitted is not refused here. That claim was wrong in
// one direction and the mirror did not show it: the evaluator also charged text
// the projection never charged — the qualifications IT generated, the boundary
// IT computed, the stream's own contract version — plus the config and trace,
// which the projection never sees at all. A source filling the projection's
// budget therefore projected successfully and was then refused by the evaluator
// for bytes. Measured: the projection admitted a 134,217,336-byte namespace and
// the evaluator answered STREAM_BYTES_OVER_BOUND on that very stream.
//
// Derived text is now bounded without being charged, and the config and trace
// carry their own ceiling. Two inputs, two ceilings, and the derived fields on
// neither.
// WHAT THIS CASE STILL ESTABLISHES, AND WHAT IT NO LONGER CAN.
//
// It was written to probe the projection's exact AGGREGATE maximum, and it can
// no longer reach one. Two later changes moved the boundary out from under it:
// the scope's namespace acquired an individual 4 KiB limit, so the search below
// now stops at that per-string bound; and the projection began charging every
// byte at its worst-case encoded width while the evaluator's gate went on
// charging raw length, so a projected stream can occupy at most a sixth of the
// gate's ceiling no matter how it is shaped.
//
// So this is now the per-string-boundary case: a stream the projection admits
// at a field's own limit is an ordinary stream to the evaluator. The invariant
// about the AGGREGATE is stronger than ever — a six-fold margin rather than a
// few hundred bytes — but it is no longer THIS case that pins it, and a probe
// that cannot fail is worth nothing. The rule that derived text is never
// charged is pinned at the gate's own boundary instead, by
// TestOrderedRulesTheGateDoesNotChargeTextItDidNotReceive, which is reachable
// only by a forged stream and asserts against the declared ceiling rather than
// against a boundary it re-derives.
func TestOrderedRulesAProjectedStreamIsAlwaysAdmissible(t *testing.T) {
	// One allocation, then slices of it: re-cutting a large string per probe
	// would cost more than the search is worth, and s[:n] shares the backing
	// array.
	big := strings.Repeat("x", predictioneval.MaxOrderedRulesAggregateBytes)
	source := func(n int) predictioneval.OrderedRulesSource {
		src := orSource([]predictioneval.OrderedRulesCandidate{
			orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6))}, nil)
		src.Scope.Namespace = big[:n]
		return src
	}

	lo, hi := 0, len(big)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if _, err := predictioneval.ProjectOrderedRulesStream(source(mid), orAdmission()); err == nil {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	t.Logf("the projection's maximum namespace here is %d bytes — the per-string bound, not the "+
		"aggregate, which sits %d bytes further up",
		lo, predictioneval.MaxOrderedRulesAggregateBytes-lo)
	if lo != predictioneval.MaxOrderedRulesIdentifierBytes {
		t.Fatalf("the search settled at %d, not the per-string bound of %d; if the bound moved, this "+
			"case's description no longer matches what it probes",
			lo, predictioneval.MaxOrderedRulesIdentifierBytes)
	}
	if _, err := predictioneval.ProjectOrderedRulesStream(source(lo+1), orAdmission()); err == nil {
		t.Fatal("premise: one byte more must be refused, or this is not the projection's maximum")
	}

	stream, err := predictioneval.ProjectOrderedRulesStream(source(lo), orAdmission())
	if err != nil {
		t.Fatalf("this case's premise is that the PROJECTION admits this source: %v", err)
	}
	if len(stream.Qualifications) == 0 {
		t.Fatal("premise: the projection must have derived at least one qualification, or the " +
			"asymmetry this case exists for is not present")
	}

	ev := predictioneval.EvaluateOrderedRules(stream,
		orConfig([]predictioneval.OrderedRule{
			orRule(predictioneval.ComparatorLe, 40, 100, 0, 10)}, 95, 100, 0, 10),
		orDraws())
	if ev.Reason == predictioneval.ReasonStreamBytesOverBound {
		t.Fatalf("the projection admitted this stream and the evaluator refused it for bytes; the "+
			"text the projection never charged — %d qualifications, the boundary, the contract "+
			"version — must not count against the same ceiling",
			len(stream.Qualifications))
	}
	if ev.Status != predictioneval.StatusWouldAttempt {
		t.Fatalf("status %q reason %q, want WOULD_ATTEMPT: a projected stream at the projection's "+
			"own limit is still an ordinary stream", ev.Status, ev.Reason)
	}
}

// TestOrderedRulesVocabularyIsBoundedBeforeItIsQuoted pins length before
// meaning on every value that can reach an error formatter.
//
// The vocabulary checks reject by QUOTING the value they refuse, and
// strconv.Quote scans the whole string and allocates an expanded copy. A
// vocabulary field is a closed set of short words, so an enormous one is not a
// near-miss — it is an input whose only effect is the cost of refusing it.
// Measured before the gate, a 64 MiB contract version took a second to refuse
// and produced a 67 MB error message: the refusal was the denial of service.
//
// The assertion is that the message does not grow with the input, which is the
// property that matters and is deterministic. It needs no large allocation to
// hold: a value one byte past the per-string bound already distinguishes a
// gate from a quote.
func TestOrderedRulesVocabularyIsBoundedBeforeItIsQuoted(t *testing.T) {
	big := strings.Repeat("x", predictioneval.MaxOrderedRulesIdentifierBytes+1)

	for _, tc := range []struct {
		name   string
		break_ func(src *predictioneval.OrderedRulesSource, adm *predictioneval.CommonAdmission)
	}{
		{"scope contract version", func(src *predictioneval.OrderedRulesSource, _ *predictioneval.CommonAdmission) {
			src.Scope.SourceContractVersion = big
		}},
		{"scope coverage", func(src *predictioneval.OrderedRulesSource, _ *predictioneval.CommonAdmission) {
			src.Scope.Coverage = predictioneval.OrderedRulesCoverage(big)
		}},
		{"admission view kind", func(_ *predictioneval.OrderedRulesSource, adm *predictioneval.CommonAdmission) {
			adm.ViewKind = predictioneval.OrderedRulesViewKind(big)
		}},
		{"candidate source kind", func(src *predictioneval.OrderedRulesSource, _ *predictioneval.CommonAdmission) {
			src.Candidates[0].SourceKind = predictioneval.OrderedRulesSourceKind(big)
		}},
		{"candidate membership", func(src *predictioneval.OrderedRulesSource, _ *predictioneval.CommonAdmission) {
			src.Candidates[0].EpisodeMembership = predictioneval.OrderedRulesMembership(big)
		}},
		{"candidate outcome-vector presence", func(src *predictioneval.OrderedRulesSource, _ *predictioneval.CommonAdmission) {
			src.Candidates[0].OutcomesPresence = predictioneval.SuppliedPresence(big)
		}},
		{"balance presence", func(src *predictioneval.OrderedRulesSource, _ *predictioneval.CommonAdmission) {
			src.Candidates[0].Balance.Presence = predictioneval.SuppliedPresence(big)
		}},
		{"outcome points presence", func(src *predictioneval.OrderedRulesSource, _ *predictioneval.CommonAdmission) {
			src.Candidates[0].Outcomes[0].Points.Presence = predictioneval.SuppliedPresence(big)
		}},
		{"intervention kind", func(src *predictioneval.OrderedRulesSource, _ *predictioneval.CommonAdmission) {
			src.Interventions = []predictioneval.OrderedRulesIntervention{
				orIntervention("call-1", 20, predictioneval.OrderedRulesInterventionKind(big),
					predictioneval.RelevanceProven, "a real call")}
		}},
		{"intervention relevance", func(src *predictioneval.OrderedRulesSource, _ *predictioneval.CommonAdmission) {
			src.Interventions = []predictioneval.OrderedRulesIntervention{
				orIntervention("call-1", 20, predictioneval.InterventionAutoCallStarted,
					predictioneval.OrderedRulesRelevance(big), "a real call")}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := orSource([]predictioneval.OrderedRulesCandidate{
				orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6))}, nil)
			adm := orAdmission()
			tc.break_(&src, &adm)

			_, err := predictioneval.ProjectOrderedRulesStream(src, adm)
			if !errors.Is(err, predictioneval.ErrOrderedRulesOverBound) {
				t.Fatalf("got %v, want an over-bound refusal before anything quotes the value", err)
			}
			if len(err.Error()) >= len(big) {
				t.Fatalf("the refusal message is %d bytes for a %d-byte input, so it grew WITH the "+
					"input — the value reached a formatter", len(err.Error()), len(big))
			}
		})
	}
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

	t.Run("an admission source reference", func(t *testing.T) {
		adm := orAdmission()
		adm.SourceReferences = []string{big}
		_, err := predictioneval.ProjectOrderedRulesStream(
			orSource([]predictioneval.OrderedRulesCandidate{orTwoOutcomePool("c1", 10)}, nil), adm)
		if !errors.Is(err, predictioneval.ErrOrderedRulesOverBound) {
			t.Fatalf("got %v, want an over-bound refusal; a retained reference is free text like any "+
				"other and was the one the loop charged without checking", err)
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

// TestOrderedRulesAShapeRefusalReExportsNoneOfTheInput pins the second half of
// what the shape gate is for.
//
// Deciding the refusal in O(1) is only half the guarantee. If the refusal then
// carries a caller-controlled string back out, the cost simply moves from the
// gate to whoever serializes the result — and the type carries JSON tags
// precisely because serializing it is the intended use. A gigabyte of cutoff
// identity costs nothing to refuse and several gigabytes to encode, JSON
// escaping included.
//
// The cutoff is the only caller-controlled text the refusal literal could
// carry, and at that point it has NOT passed the per-string bound: the gate
// refuses on the FIRST condition that trips, and most of them never look at the
// cutoff at all. So the refusal re-exports none of it, for the same reason it
// carries no digest — the function declined to read the input, so it can attest
// to nothing about it.
func TestOrderedRulesAShapeRefusalReExportsNoneOfTheInput(t *testing.T) {
	// Large enough that carrying it is unmistakable in the encoded size, small
	// enough to stay cheap: the defect is a ratio, not a threshold.
	const cutoffBytes = 1 << 20

	base := orProject(t, []predictioneval.OrderedRulesCandidate{
		orCandidate("c1", 10, orKnownBalance(100), orOutcome("A", 4), orOutcome("B", 6)),
	}, nil)

	forge := func() predictioneval.OrderedRulesStream {
		st := base
		st.Candidates = append([]predictioneval.OrderedRulesCandidate(nil), base.Candidates...)
		st.Cutoff = predictioneval.OrderedRulesCutoff{
			Established: true,
			Position:    9,
			Kind:        predictioneval.OrderedRulesInterventionKind(strings.Repeat("k", cutoffBytes)),
			Identity:    strings.Repeat("i", cutoffBytes),
			Basis:       predictioneval.OrderedRulesCutoffBasis(strings.Repeat("b", cutoffBytes)),
		}
		return st
	}

	tooManyCandidates := forge()
	tooManyCandidates.Candidates = make([]predictioneval.OrderedRulesCandidate,
		predictioneval.MaxOrderedRulesCandidates+1)

	tooManyRules := make([]predictioneval.OrderedRule, predictioneval.MaxOrderedRulesRules+1)
	for i := range tooManyRules {
		tooManyRules[i] = orRule(predictioneval.ComparatorLe, 50, 100, 0, 10)
	}

	cases := []struct {
		name   string
		stream predictioneval.OrderedRulesStream
		cfg    predictioneval.OrderedRulesConfig
		draws  predictioneval.SuppliedDrawTrace
		reason string
	}{{
		// The first three never examine the cutoff at all, which is the point:
		// the re-export is unconditional, not a side effect of bounding text.
		name:   "draw words over bound",
		stream: forge(),
		cfg:    orAlwaysAdmitConfig(),
		draws: predictioneval.SuppliedDrawTrace{
			RunID:                   "r",
			EntropySemanticsVersion: predictioneval.OrderedRulesEntropySemanticsVersion,
			Words:                   make([]uint64, predictioneval.MaxOrderedRulesDrawWords+1),
		},
		reason: predictioneval.ReasonDrawWordsOverBound,
	}, {
		name:   "rule count over bound",
		stream: forge(),
		cfg:    orConfig(tooManyRules, 95, 100, 0, 10),
		draws:  orDraws(),
		reason: predictioneval.ReasonRuleCountOverBound,
	}, {
		name:   "stream shape over bound",
		stream: tooManyCandidates,
		cfg:    orAlwaysAdmitConfig(),
		draws:  orDraws(),
		reason: predictioneval.ReasonStreamShapeOverBound,
	}, {
		name:   "the cutoff text is itself what trips the gate",
		stream: forge(),
		cfg:    orAlwaysAdmitConfig(),
		draws:  orDraws(),
		reason: predictioneval.ReasonStreamTextOverBound,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := predictioneval.EvaluateOrderedRules(tc.stream, tc.cfg, tc.draws)

			switch {
			case ev.Status != predictioneval.StatusRefused:
				t.Fatalf("status = %q, want REFUSED", ev.Status)
			case ev.Reason != tc.reason:
				t.Fatalf("reason = %q, want %q", ev.Reason, tc.reason)
			}

			if ev.Cutoff != (predictioneval.OrderedRulesCutoff{}) {
				t.Fatalf("a shape refusal carried the supplied cutoff back out:\n"+
					"  kind %d bytes, identity %d bytes, basis %d bytes\n"+
					"The gate declined to read this input, so the result may attest to nothing "+
					"about it — least of all by re-exporting text it never bounded.",
					len(ev.Cutoff.Kind), len(ev.Cutoff.Identity), len(ev.Cutoff.Basis))
			}

			// The consequence, stated as the property it actually is: refusing
			// must not cost more to encode than it cost to decide.
			b, err := json.Marshal(ev)
			if err != nil {
				t.Fatalf("marshalling the refusal failed: %v", err)
			}
			if len(b) > 4096 {
				t.Fatalf("the refusal encodes to %d bytes from a %d-byte cutoff; a refusal decided "+
					"without reading the input must not grow with it", len(b), cutoffBytes)
			}
		})
	}
}

// TestOrderedRulesAnOmittedMandatoryFieldIsNotAnExplicitZero pins presence for
// the scalars that had none.
//
// Every scalar the caller supplies in this model carries a presence, because a
// zero is a legitimate value everywhere and a missing one read as zero is a
// fabricated input. Four mandatory fields were exceptions — a candidate's
// causal position, an intervention's position, the scope's interval endpoints
// and the config's default — and the exception was invisible while the types
// were only ever built in Go. They carry JSON tags, so decoding is a supported
// way to build them, and there omission is silent:
//
//   - an omitted candidate position decodes to zero, is admitted as the
//     EARLIEST candidate whenever the interval contains zero, and takes the
//     opportunity from whichever candidate really was first;
//   - an omitted intervention position cuts the stream at zero, removing every
//     candidate;
//   - omitted interval endpoints decode to [0,0], which is a real interval;
//   - an omitted default decodes to a USABLE [0,0] rule that admits any
//     zero-share outcome and reports a stake of zero with presence KNOWN —
//     a fabricated value presented as known, from a config that was never
//     supplied. The donor's own `small` preset ships bounds of exactly zero,
//     so this cannot be fixed by treating all-zero as absent.
func TestOrderedRulesAnOmittedMandatoryFieldIsNotAnExplicitZero(t *testing.T) {
	const scopeFlag = `"hasInterval":true,`
	const candFlag = `"hasPosition":true,"sourceKind"`
	const invFlag = `"hasPosition":true,"kind"`
	const defFlag = `"hasDefault":true,`

	sourceJSON := `{"scope":{"namespace":"ns","episodeId":"e1","accountContext":"ac",
	  "associationEvidence":"ae","sourceContractVersion":"` + predictioneval.OrderedRulesStreamContractVersion + `",
	  "coverage":"COMPLETE_DECLARED","intervalFromPosition":0,"intervalToPosition":10000,` + scopeFlag + `
	  "hasNothingElse":0},
	 "candidates":[{"identity":"c1","position":10,` + candFlag + `:"CHANNEL_UPDATE",
	   "episodeMembership":"PROVEN","outcomesPresence":"KNOWN","provenance":"p","outcomes":[
	     {"identity":"A","points":{"presence":"KNOWN","value":4,"provenance":"p","hasAvailableAtPosition":true}},
	     {"identity":"B","points":{"presence":"KNOWN","value":6,"provenance":"p","hasAvailableAtPosition":true}}],
	   "balance":{"presence":"KNOWN","value":1000,"provenance":"p","hasAvailableAtPosition":true}}],
	 "interventions":[{"identity":"call-1","position":9000,` + invFlag + `:"AUTO_CALL_STARTED",
	   "relevance":"PROVEN_RELEVANT","detail":"d"}]}`
	configJSON := `{"configId":"cfg",` + defFlag + `
	  "detailed":[{"comparator":"Le","rawThresholdPercent":100,"rawAttemptRatePercent":100,
	    "points":{"maxValue":0,"rawPercent":10}}],
	  "default":{"rawMinPercent":95,"rawMaxPercent":100,"points":{"maxValue":0,"rawPercent":10}}}`

	run := func(t *testing.T, src, cfgText string) (predictioneval.OrderedRulesEvaluation, error) {
		t.Helper()
		var s predictioneval.OrderedRulesSource
		if err := json.Unmarshal([]byte(src), &s); err != nil {
			t.Fatalf("unmarshalling the source failed: %v", err)
		}
		var cfg predictioneval.OrderedRulesConfig
		if err := json.Unmarshal([]byte(cfgText), &cfg); err != nil {
			t.Fatalf("unmarshalling the config failed: %v", err)
		}
		stream, err := predictioneval.ProjectOrderedRulesStream(s, orAdmission())
		if err != nil {
			return predictioneval.OrderedRulesEvaluation{}, err
		}
		return predictioneval.EvaluateOrderedRules(stream, cfg, orDraws(orWordAdmit)), nil
	}

	// The control. Without it every case below could pass for the wrong reason.
	t.Run("every declaration present", func(t *testing.T) {
		ev, err := run(t, sourceJSON, configJSON)
		if err != nil {
			t.Fatalf("a fully declared source must project: %v", err)
		}
		if ev.Status != predictioneval.StatusWouldAttempt {
			t.Fatalf("the control must reach a decision, got status %q reason %q", ev.Status, ev.Reason)
		}
	})

	cases := []struct {
		name     string
		drop     string
		inSource bool
		want     string // substring of the projection error, or a refusal reason
	}{
		{"candidate position", candFlag, true, "causal position"},
		{"intervention position", invFlag, true, "does not declare its position"},
		{"scope interval", scopeFlag, true, "interval endpoints"},
		{"config default", defFlag, false, predictioneval.ReasonConfigDefaultNotSupplied},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, cfgText := sourceJSON, configJSON
			if tc.inSource {
				src = strings.Replace(src, tc.drop, strings.TrimPrefix(tc.drop, `"hasPosition":true,`), 1)
				if tc.drop == scopeFlag {
					src = strings.Replace(sourceJSON, scopeFlag, "", 1)
				}
				if src == sourceJSON {
					t.Fatalf("the fixture did not change, so this case would assert nothing")
				}
			} else {
				cfgText = strings.Replace(cfgText, tc.drop, "", 1)
				if cfgText == configJSON {
					t.Fatalf("the fixture did not change, so this case would assert nothing")
				}
			}

			ev, err := run(t, src, cfgText)
			if tc.inSource {
				if err == nil {
					t.Fatalf("an omitted %s was accepted; omission decoded to a usable zero", tc.name)
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("refused for the wrong reason: %v", err)
				}
				return
			}
			switch {
			case ev.Status != predictioneval.StatusRefused:
				t.Fatalf("an omitted %s produced status %q, not a refusal; the model decided on a "+
					"configuration that was never supplied", tc.name, ev.Status)
			case ev.Reason != tc.want:
				t.Fatalf("reason = %q, want %q", ev.Reason, tc.want)
			}
		})
	}
}

// TestOrderedRulesAnAdmittedStreamEncodesInsideItsDeclaredCeiling is the
// end-to-end half of the charged-width repair.
//
// The unit half proves no byte encodes wider than the charged expansion. This
// half proves the charge is actually applied everywhere it has to be: it takes
// the widest source the projection will admit, built entirely from the
// worst-escaping byte there is, and encodes the stream that comes back.
//
// Charging raw length instead put this at roughly six times the ceiling — a
// source sitting inside a declared 128 MiB boundary encoding to about 768 MiB.
// The remaining margin over the ceiling is JSON structure — field names and
// punctuation — which is bounded by the element counts rather than by any
// caller-supplied text, and is what the ceiling never claimed to cover.
func TestOrderedRulesAnAdmittedStreamEncodesInsideItsDeclaredCeiling(t *testing.T) {
	const nc, no = predictioneval.MaxOrderedRulesCandidates, predictioneval.MaxOrderedRulesOutcomes

	// The fixture is adversarial on purpose, and an earlier version of it was
	// not — which is why it missed this. Every charged byte is the
	// worst-escaping one, and ASCII is kept to the floor the vocabulary forces,
	// because ASCII is charged six times what it encodes to and that surplus
	// silently absorbs the structural overhead being measured. Outcome
	// identities need not be unique, so they cost no ASCII at all; candidate
	// identities are made unique by LENGTH rather than by any readable byte.
	build := func(idLen int) (predictioneval.OrderedRulesStream, error) {
		nul := strings.Repeat("\x00", idLen+nc+no)
		cs := make([]predictioneval.OrderedRulesCandidate, 0, nc)
		for i := 0; i < nc; i++ {
			outs := make([]predictioneval.OrderedRulesOutcome, 0, no)
			for j := 0; j < no; j++ {
				// Unique WITHIN the candidate, as the model now requires, and
				// unique by LENGTH so the fixture still carries no ASCII.
				o := orOutcome(nul[:idLen+j], int64(j+1))
				o.Points.Provenance = "p"
				outs = append(outs, o)
			}
			bal := orKnownBalance(100)
			bal.Provenance = "p"
			c := orCandidate(nul[:idLen+i], int64(i+1), bal, outs...)
			c.Provenance = "p"
			cs = append(cs, c)
		}
		return predictioneval.ProjectOrderedRulesStream(orSource(cs, nil), orAdmission())
	}

	// The widest identifier the projection still admits at this shape.
	// The floor clears the longest generated prefix, so every probe is really
	// idLen bytes wide and the search measures the bound and not the fixture.
	lo, hi := 8, predictioneval.MaxOrderedRulesIdentifierBytes-nc-no
	if _, err := build(lo); err != nil {
		t.Fatalf("the smallest fixture must project: %v", err)
	}
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if _, err := build(mid); err != nil {
			hi = mid - 1
		} else {
			lo = mid
		}
	}
	if _, err := build(lo + 1); err == nil && lo+1 <= predictioneval.MaxOrderedRulesIdentifierBytes {
		t.Fatalf("binary search did not find the boundary: %d still projects", lo+1)
	}

	stream, err := build(lo)
	if err != nil {
		t.Fatalf("the widest admitted fixture must project: %v", err)
	}
	encoded, err := json.Marshal(stream)
	if err != nil {
		t.Fatalf("marshalling the projected stream failed: %v", err)
	}

	ceiling := int64(predictioneval.MaxOrderedRulesAggregateBytes)
	ratio := float64(len(encoded)) / float64(ceiling)
	t.Logf("widest admitted identifier = %d bytes of the worst-escaping byte", lo)
	t.Logf("encoded stream = %d bytes against a ceiling of %d (%.2fx)", len(encoded), ceiling, ratio)

	// Text is charged at its encoded width, so the text alone cannot pass the
	// ceiling; everything above it is structure bounded by the element counts.
	if int64(len(encoded)) > ceiling {
		t.Fatalf("an ADMITTED stream encodes to %d bytes, %.2fx its declared ceiling of %d. Charging "+
			"supplied text at its raw length rather than its encoded width puts this near 6x; a "+
			"smaller overshoot means the stream grew a field the ceiling does not account for.",
			len(encoded), ratio, ceiling)
	}
}

// TestOrderedRulesEveryChargedStringIsAlsoBoundedIndividually closes the gap
// between what the per-string limit claimed to cover and what it covered.
//
// The aggregate and the per-string limit answer different questions, and the
// package says so: one 64 MiB note sits well inside a 128 MiB budget while
// being a value the projection refuses outright. That was true of every charged
// string except seven — the scope's namespace, episode id, account context and
// association evidence, and the admission's manifest id, population and order
// basis — which were only ever checked for being non-EMPTY. Nothing bounded
// their length, so the aggregate was their only limit and a single one of them
// could take most of it: an 8 MiB admission population was admitted outright.
//
// It also made the encoded-ceiling guarantee reachable, because one unbounded
// field can fill the remaining charge byte-exactly and leave the structural
// overhead to land past the ceiling.
func TestOrderedRulesEveryChargedStringIsAlsoBoundedIndividually(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{
		orCandidate("c1", 10, orKnownBalance(100), orOutcome("A", 4), orOutcome("B", 6)),
	}
	// Past the per-string bound, but far inside the aggregate even when charged
	// at the worst-case encoded width — so only a per-string limit can refuse it.
	over := strings.Repeat("x", predictioneval.MaxOrderedRulesIdentifierBytes+1)

	cases := []struct {
		name  string
		apply func(*predictioneval.OrderedRulesSource, *predictioneval.CommonAdmission)
	}{
		{"scope namespace", func(s *predictioneval.OrderedRulesSource, _ *predictioneval.CommonAdmission) {
			s.Scope.Namespace = over
		}},
		{"scope episode id", func(s *predictioneval.OrderedRulesSource, _ *predictioneval.CommonAdmission) {
			s.Scope.EpisodeID = over
		}},
		{"scope account context", func(s *predictioneval.OrderedRulesSource, _ *predictioneval.CommonAdmission) {
			s.Scope.AccountContext = over
		}},
		{"scope association evidence", func(s *predictioneval.OrderedRulesSource, _ *predictioneval.CommonAdmission) {
			s.Scope.AssociationEvidence = over
		}},
		{"admission manifest id", func(_ *predictioneval.OrderedRulesSource, a *predictioneval.CommonAdmission) {
			a.ManifestID = over
		}},
		{"admission population", func(_ *predictioneval.OrderedRulesSource, a *predictioneval.CommonAdmission) {
			a.Population = over
		}},
		{"admission order basis", func(_ *predictioneval.OrderedRulesSource, a *predictioneval.CommonAdmission) {
			a.OrderBasis = over
		}},
	}

	// The control: the same fixture at the bound projects, so each case below
	// fails for its own length and not for something else in the fixture.
	if _, err := predictioneval.ProjectOrderedRulesStream(orSource(cs, nil), orAdmission()); err != nil {
		t.Fatalf("the unmodified fixture must project: %v", err)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, adm := orSource(cs, nil), orAdmission()
			tc.apply(&src, &adm)
			_, err := predictioneval.ProjectOrderedRulesStream(src, adm)
			if err == nil {
				t.Fatalf("a %d-byte %s was admitted; the per-string bound does not reach it, so the "+
					"aggregate is its only limit and one field can take most of it",
					len(over), tc.name)
			}
			if !errors.Is(err, predictioneval.ErrOrderedRulesOverBound) {
				t.Fatalf("refused, but not as an over-bound value: %v", err)
			}
		})
	}
}

// TestOrderedRulesTheGateDoesNotChargeTextItDidNotReceive pins the derived-text
// rule where it can still fail.
//
// The rule is that the shape gate charges only text the PROJECTION charged:
// what the projection derives — the qualifications it generates, the boundary
// it computes, the stream's own contract version — is length-bounded but never
// counted, because the projection never counted it either. Charging it let a
// stream the projection admitted be refused here for bytes.
//
// That rule used to be pinned by probing the PROJECTION's exact maximum, and
// that probe has since stopped being able to fail. Once the projection began
// charging every byte at its worst-case encoded width while this gate went on
// charging raw length, a projected stream could reach at most a sixth of the
// gate's ceiling — so a few hundred kilobytes of derived text can no longer tip
// it, and restoring the defect left the whole suite green. The invariant is
// still true, and more comfortably than before; it was the TEST that stopped
// testing it.
//
// So the probe moves to this gate's own boundary, which is reachable only by a
// stream the projection would never produce. That is not a weakness of the
// case: the gate runs BEFORE the digest and the invariant pass precisely so it
// can judge a forgery cheaply, so a forged stream is exactly the input whose
// byte accounting it has to get right.
func TestOrderedRulesTheGateDoesNotChargeTextItDidNotReceive(t *testing.T) {
	const (
		nc  = predictioneval.MaxOrderedRulesCandidates
		no  = predictioneval.MaxOrderedRulesOutcomes
		max = predictioneval.MaxOrderedRulesIdentifierBytes
	)
	buf := strings.Repeat("x", max)

	// Derived text at its own maxima. None of it may be charged, and together
	// it is far larger than one step of the search below, so the case cannot
	// pass by landing inside the search's granularity.
	derived := make([]string, predictioneval.MaxOrderedRulesQualifications)
	for i := range derived {
		derived[i] = buf
	}
	derivedBytes := len(derived)*max + 3*max + max

	stream := func(n int, withDerived bool) predictioneval.OrderedRulesStream {
		s := orProject(t, []predictioneval.OrderedRulesCandidate{
			orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6))}, nil)
		if withDerived {
			s.ContractVersion = buf
			s.Qualifications = derived
			s.Cutoff = predictioneval.OrderedRulesCutoff{
				Established: true, Position: 9,
				Kind:     predictioneval.OrderedRulesInterventionKind(buf),
				Basis:    predictioneval.OrderedRulesCutoffBasis(buf),
				Identity: buf,
			}
		}
		// The charged half: four bounded strings on each of 8,192 outcomes.
		cs := make([]predictioneval.OrderedRulesCandidate, 0, nc)
		for i := 0; i < nc; i++ {
			outs := make([]predictioneval.OrderedRulesOutcome, 0, no)
			for j := 0; j < no; j++ {
				outs = append(outs, predictioneval.OrderedRulesOutcome{
					Identity: buf[:n],
					Points: predictioneval.SuppliedInt64{
						Presence:   predictioneval.SuppliedPresence(buf[:n]),
						Provenance: buf[:n],
						Reason:     buf[:n],
					},
				})
			}
			cs = append(cs, predictioneval.OrderedRulesCandidate{
				Identity: "c" + itoaTest(i), Position: int64(i + 1), HasPosition: true,
				Outcomes: outs,
			})
		}
		s.Candidates = cs
		return s
	}

	// The gate's largest accepted charged payload, with the derived text
	// present and with it absent.
	//
	// The comparison is between two MEASUREMENTS OF THE SAME CODE that differ
	// only in their input, which is what makes it immune to the mistake that
	// made the previous two versions of this case vacuous. Searching for the
	// boundary and then asserting at the boundary found proves nothing: the
	// search re-derives the boundary under whatever the code does, so charging
	// extra text simply moves it down and the assertion passes anyway. Asserting
	// that the boundary does not MOVE when derived text is added cannot be
	// satisfied that way — if the derived text is charged, the maximum drops.
	maxRaw := func(withDerived bool) int {
		t.Helper()
		over := func(n int) bool {
			ev := predictioneval.EvaluateOrderedRules(stream(n, withDerived), orAlwaysAdmitConfig(), orDraws())
			return ev.Reason == predictioneval.ReasonStreamBytesOverBound
		}
		if over(0) {
			t.Fatal("premise: an empty charged payload must not be over the byte bound")
		}
		lo, hi := 0, max
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if over(mid) {
				hi = mid - 1
			} else {
				lo = mid
			}
		}
		if lo < max && !over(lo+1) {
			t.Fatalf("binary search did not find the boundary: %d is still admitted", lo+1)
		}
		return lo
	}

	withDerived, without := maxRaw(true), maxRaw(false)
	step := int64(nc) * int64(no) * 4
	t.Logf("largest accepted outcome string: %d with derived text, %d without (one step charges %d bytes; "+
		"derived text is %d bytes)", withDerived, without, step, derivedBytes)

	if int64(derivedBytes) < step {
		t.Fatalf("this case is vacuous: one search step charges %d bytes and the derived text is only "+
			"%d, so charging it could hide inside the search's granularity", step, derivedBytes)
	}
	if withDerived != without {
		t.Fatalf("adding %d bytes of DERIVED text moved the gate's limit from %d to %d bytes per "+
			"outcome string. Text the projection generates — %d qualifications, the boundary and the "+
			"stream's contract version — is being charged against the ceiling the projection charged.",
			derivedBytes, without, withDerived, len(derived))
	}

	// The config and the trace have no projection to mirror, so they carry a
	// ceiling of their OWN. Charging them to the stream's would let an
	// unrelated argument refuse a stream that is otherwise exactly at its
	// limit, so the identifier below is far larger than one search step.
	t.Run("an unrelated config identifier is not charged to the stream", func(t *testing.T) {
		cfg := orAlwaysAdmitConfig()
		cfg.ConfigID = strings.Repeat("i", 1<<20)
		ev := predictioneval.EvaluateOrderedRules(stream(withDerived, true), cfg, orDraws())
		if ev.Reason == predictioneval.ReasonStreamBytesOverBound {
			t.Fatalf("a %d-byte config identifier pushed the STREAM over its byte ceiling; the "+
				"projection never saw the config, so it must carry its own", len(cfg.ConfigID))
		}
	})
}

// TestOrderedRulesTheGateRefusesBytesTheProjectionWouldHaveRefused pins the
// byte rule in the direction the mirror was missing.
//
// The projection charges every supplied byte at its worst-case encoded width
// and withholds the structural reserve; this gate compared the RAW total, and
// the two differ by a factor of six. So a forged stream carrying 32 MiB of
// perfectly valid provenance — inside every count, inside every per-string
// bound, inside the raw aggregate — passed the gate and every semantic
// invariant, while ProjectOrderedRulesStream refuses that same source outright.
// With the digest oracle that stream reached WOULD_ATTEMPT.
//
// The two sides now apply the SAME condition, so this asserts the property
// rather than a margin: whatever the projection refuses for bytes, the gate
// refuses for bytes.
func TestOrderedRulesTheGateRefusesBytesTheProjectionWouldHaveRefused(t *testing.T) {
	const (
		nc  = predictioneval.MaxOrderedRulesCandidates
		no  = predictioneval.MaxOrderedRulesOutcomes
		max = predictioneval.MaxOrderedRulesIdentifierBytes
	)
	buf := strings.Repeat("p", max)

	cs := make([]predictioneval.OrderedRulesCandidate, 0, nc)
	for i := 0; i < nc; i++ {
		outs := make([]predictioneval.OrderedRulesOutcome, 0, no)
		for j := 0; j < no; j++ {
			o := orOutcome("o", int64(j+1))
			o.Points.Provenance = buf
			outs = append(outs, o)
		}
		cs = append(cs, orCandidate("c"+itoaTest(i), int64(i+1), orKnownBalance(100), outs...))
	}

	// Every value here is individually legal; only the charged total is not.
	raw := int64(nc) * int64(no) * int64(max)
	if raw > predictioneval.MaxOrderedRulesAggregateBytes {
		t.Fatalf("this case's premise is that the RAW total is inside the aggregate, so only the "+
			"charged width can refuse it; raw is %d against %d",
			raw, int64(predictioneval.MaxOrderedRulesAggregateBytes))
	}
	if _, err := predictioneval.ProjectOrderedRulesStream(orSource(cs, nil), orAdmission()); err == nil {
		t.Fatal("premise: the projection must refuse this source, or there is nothing to mirror")
	}

	// Forge it into a stream and run the oracle: a refusal publishes the digest
	// it wanted, so copying that back is two calls and no cryptography.
	st := orProject(t, []predictioneval.OrderedRulesCandidate{
		orCandidate("c1", 10, orKnownBalance(100), orOutcome("A", 4), orOutcome("B", 6))}, nil)
	st.Candidates = cs

	first := predictioneval.EvaluateOrderedRules(st, orAlwaysAdmitConfig(), orDraws())
	if first.Reason != predictioneval.ReasonStreamBytesOverBound {
		t.Fatalf("the gate admitted %d raw bytes the projection refuses for encoded width: reason %q. "+
			"The projection charges these at %d, past its own ceiling.",
			raw, first.Reason, raw*6)
	}
	// And it stays refused once the digest is made to agree, which is the only
	// route by which a forged stream gets a second look.
	st.SelectionDigest = first.StreamDigest
	if second := predictioneval.EvaluateOrderedRules(st, orAlwaysAdmitConfig(), orDraws()); second.Reason !=
		predictioneval.ReasonStreamBytesOverBound {
		t.Fatalf("with a matching digest the same stream was answered %q / %q; the byte rule must not "+
			"depend on the digest, which proves change and not origin", second.Status, second.Reason)
	}
}

// TestOrderedRulesAVersionMismatchIsDecidedBeforeAnythingIsHashed pins the
// order of the two cheapest checks in the evaluator.
//
// Both are one comparison against a constant, and both decide that the input is
// not evaluable at all — so computing three whole-input digests first does the
// work before the check that governs it. The config identifier and the run
// identifier make that expensive: they ride the separate config-and-trace
// ceiling and carry no per-string bound, deliberately, so they can be enormous.
// Measured with a 125 MiB config identifier beside a stream whose contract
// version is simply wrong, the refusal took 1.23 seconds and allocated
// 251,662,352 bytes to compare two short strings and find them different.
//
// The assertion is on the DIGESTS rather than on time or allocation, because a
// timing assertion is not a deterministic test: an absent digest is the direct
// evidence that nothing was hashed, and it is exactly what the refusal is
// entitled to say — it declined to read the input, so it attests to nothing.
func TestOrderedRulesAVersionMismatchIsDecidedBeforeAnythingIsHashed(t *testing.T) {
	good := orProject(t, []predictioneval.OrderedRulesCandidate{
		orCandidate("c1", 10, orKnownBalance(100), orOutcome("A", 4), orOutcome("B", 6))}, nil)

	// Large enough that hashing it would be unmistakable, and legal: neither
	// identifier is bounded per string.
	big := strings.Repeat("i", 1<<20)

	for _, tc := range []struct {
		name   string
		mutate func(*predictioneval.OrderedRulesStream, *predictioneval.OrderedRulesConfig,
			*predictioneval.SuppliedDrawTrace)
		want string
	}{
		{"the stream's contract version", func(st *predictioneval.OrderedRulesStream,
			cfg *predictioneval.OrderedRulesConfig, d *predictioneval.SuppliedDrawTrace) {
			st.ContractVersion = "not-the-contract-version"
			cfg.ConfigID = big
			d.RunID = big
		}, predictioneval.ReasonStreamContractMismatch},
		{"the entropy semantics version", func(_ *predictioneval.OrderedRulesStream,
			cfg *predictioneval.OrderedRulesConfig, d *predictioneval.SuppliedDrawTrace) {
			d.EntropySemanticsVersion = "not-the-entropy-semantics-version"
			cfg.ConfigID = big
			d.RunID = big
		}, predictioneval.ReasonEntropySemanticsMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, cfg, draws := good, orAlwaysAdmitConfig(), orDraws()
			tc.mutate(&st, &cfg, &draws)

			ev := predictioneval.EvaluateOrderedRules(st, cfg, draws)
			switch {
			case ev.Status != predictioneval.StatusRefused:
				t.Fatalf("status = %q, want REFUSED", ev.Status)
			case ev.Reason != tc.want:
				t.Fatalf("reason = %q, want %q", ev.Reason, tc.want)
			}
			if ev.StreamDigest != "" || ev.ConfigDigest != "" || ev.EntropyDigest != "" ||
				ev.ConsumedInputDigest != "" {
				t.Fatalf("a %s mismatch was answered with digests over %d bytes of auxiliary text. "+
					"The comparison that decided this refusal is one string against a constant; "+
					"hashing the whole input first is the work the check was supposed to govern.",
					tc.name, 2*len(big))
			}
		})
	}
}

// TestOrderedRulesTheCutoffIdentityIsChargedLikeTheTextItCameFrom pins the one
// piece of the boundary that is NOT derived.
//
// The cutoff's kind and basis are labels the projection computes, so they are
// bounded and never charged. Its identity is different in kind: the projection
// COPIES it from a supplied intervention identity, and charges those bytes
// against the source budget. Treating it as derived let a forged stream carry
// text that no source could have supplied — every stream with an established
// cutoff had at least the intervention whose identity it names, and the
// projection charged every intervention.
//
// Charging it stays inside the projection's own total for the same reason, so
// this tightens the gate without breaking "admitted by the projection implies
// admitted here".
func TestOrderedRulesTheCutoffIdentityIsChargedLikeTheTextItCameFrom(t *testing.T) {
	const (
		nc  = predictioneval.MaxOrderedRulesCandidates
		no  = predictioneval.MaxOrderedRulesOutcomes
		max = predictioneval.MaxOrderedRulesIdentifierBytes
	)
	buf := strings.Repeat("x", max)

	// Sized so the charged bulk sits just under the ceiling, leaving slack that
	// the fine dimension below can consume a byte at a time. Full-length
	// identities on every outcome would exceed the ceiling on their own.
	const bulkID = 2640

	build := func(cutoffID string, fine int) predictioneval.OrderedRulesStream {
		s := orProject(t, []predictioneval.OrderedRulesCandidate{
			orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6))}, nil)
		if cutoffID != "" {
			s.Cutoff = predictioneval.OrderedRulesCutoff{
				Established: true, Position: 9,
				Kind:     predictioneval.InterventionAutoCallStarted,
				Basis:    predictioneval.CutoffFactualIntervention,
				Identity: cutoffID,
			}
		}
		cs := make([]predictioneval.OrderedRulesCandidate, 0, nc)
		for i := 0; i < nc; i++ {
			outs := make([]predictioneval.OrderedRulesOutcome, 0, no)
			for j := 0; j < no; j++ {
				outs = append(outs, predictioneval.OrderedRulesOutcome{Identity: buf[:bulkID]})
			}
			cs = append(cs, predictioneval.OrderedRulesCandidate{
				Identity: "c" + itoaTest(i), Position: int64(i + 1), HasPosition: true, Outcomes: outs,
			})
		}
		// The fine dimension is spread over several strings because one caps
		// out at the per-string bound long before the aggregate binds.
		for i := 0; fine > 0 && i < len(cs); i++ {
			k := fine
			if k > max {
				k = max
			}
			cs[i].OutcomesReason = strings.Repeat("r", k)
			fine -= k
		}
		s.Candidates = cs
		return s
	}
	admitted := func(cutoffID string, fine int) bool {
		return predictioneval.EvaluateOrderedRules(build(cutoffID, fine), orAlwaysAdmitConfig(),
			orDraws()).Reason != predictioneval.ReasonStreamBytesOverBound
	}
	maxFine := func(cutoffID string) int {
		t.Helper()
		if !admitted(cutoffID, 0) {
			t.Fatalf("premise: the fixture must be admitted with no fine text (cutoff present: %v)", cutoffID != "")
		}
		lo, hi := 0, max*nc
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if admitted(cutoffID, mid) {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		return lo
	}

	none, withCutoff := maxFine(""), maxFine(buf)
	t.Logf("fine bytes admitted: %d with no cutoff, %d with a %d-byte cutoff identity", none, withCutoff, max)
	if none-withCutoff != max {
		t.Fatalf("declaring a %d-byte cutoff identity cost %d bytes of budget, want exactly %d. The "+
			"projection charged the intervention identity this was copied from, so a gate that "+
			"excludes it admits streams no source could have produced.",
			max, none-withCutoff, max)
	}
}

// TestOrderedRulesDuplicateOutcomeIdentityIsRejected closes the last identity
// in the model without a uniqueness rule.
//
// Candidate identities are unique within a source and intervention identities
// are unique within the boundary scan, both for the same stated reason: two
// things carrying the same identity are an input conflict, and neither dropping
// one nor keeping both is a safe reading. Outcome identities had no such rule,
// so a pool could name the same outcome twice — which is not a pool that can
// exist, while the model computed shares over it and reached a decision.
//
// The selection itself is not ambiguous — it carries OutcomeIndex beside
// OutcomeIdentity — so the harm is not a misattributed choice. It is that the
// model answered at all about a pool the source domain cannot produce.
func TestOrderedRulesDuplicateOutcomeIdentityIsRejected(t *testing.T) {
	// Different points, so the duplicate cannot be dismissed as a harmless copy.
	cs := []predictioneval.OrderedRulesCandidate{
		orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("A", 6)),
	}
	_, err := predictioneval.ProjectOrderedRulesStream(orSource(cs, nil), orAdmission())
	if err == nil {
		t.Fatal("a pool naming the same outcome twice was projected; every other identity in this " +
			"model is unique within its scope, and this was the exception")
	}
	if !errors.Is(err, predictioneval.ErrOrderedRulesDuplicateIdentity) {
		t.Fatalf("refused, but not as a duplicate identity: %v", err)
	}

	// The control: identical POINTS under distinct identities stay legal, which
	// is what keeps the rule about identity rather than about values.
	ok := []predictioneval.OrderedRulesCandidate{
		orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 5), orOutcome("B", 5)),
	}
	if _, err := predictioneval.ProjectOrderedRulesStream(orSource(ok, nil), orAdmission()); err != nil {
		t.Fatalf("two outcomes with equal points and distinct identities must project: %v", err)
	}
}

// TestOrderedRulesTheConsumedPrefixBindsEveryMandatoryDeclaration closes the
// source-identity block of the consumed-prefix digest.
//
// That block bound WHICH source the prefix came from, and it did so field by
// field — which meant every mandatory declaration it happened to omit was a
// difference the digest could not see. Three were omitted: the scope's
// association evidence, and the admission's population and order basis. Two
// sources declaring different accounts-are-the-same evidence, one solid and one
// only just admissible, produced the same consumed-prefix binding, so a result
// computed under one could be presented as a result computed under the other.
// That is the recombination the four separate digests exist to prevent.
//
// Each case varies ONE declaration in the SOURCE and re-projects, rather than
// editing a projected stream, because the property is about two sources the
// projection ADMITS: a result computed over one must not be presentable as a
// result computed over the other. Editing a projected stream tests something
// else — the edited stream is refused by the invariant pass, so the comparison
// would be between an admitted prefix and a rejected one.
//
// An earlier version of this comment justified that choice by claiming a
// refusal carries no consumed digest. It does. EvaluateOrderedRules computes
// one on every refusal it reaches mid-traversal, deliberately and for a stated
// reason — a refusal that has already read candidates and spent words is
// misdescribed by a digest claiming an empty prefix. Only the two unread
// refusals, the shape gate and the version mismatch, carry none. The premise
// assertions below are what actually keep each case honest.
func TestOrderedRulesTheConsumedPrefixBindsEveryMandatoryDeclaration(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{
		orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
	}
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 40, 100, 0, 10)}, 95, 100, 0, 10)

	base := predictioneval.EvaluateOrderedRules(orProject(t, cs, nil), cfg, orDraws())
	if base.Status != predictioneval.StatusWouldAttempt || base.ConsumedInputDigest == "" {
		t.Fatalf("premise: the intact input must evaluate and carry a consumed digest; got %q / %q",
			base.Status, base.ConsumedInputDigest)
	}

	// Every mandatory scope and admission declaration that can be varied on its
	// own. Three are deliberately absent, and each for a stated reason rather
	// than because it was awkward:
	//
	//   - the scope's source contract version is pinned to
	//     OrderedRulesStreamContractVersion, so no other value projects;
	//   - the admission's view kind cannot move without also moving every
	//     candidate's source kind, which the projection couples to it — it is
	//     bound, and TestOrderedRulesDigestsBindTheMandatoryFieldDeclarations'
	//     neighbours cover the coupling;
	//   - the declared interval's UPPER endpoint is bound in the whole-stream
	//     digest and deliberately NOT in the consumed one, because appending a
	//     fact past the boundary can legitimately widen it and this digest must
	//     survive that append. Its LOWER endpoint is not symmetric with it, is
	//     bound in both, and has its own case below — with the asymmetry
	//     asserted at the end rather than merely described here.
	cases := []struct {
		name  string
		apply func(*predictioneval.OrderedRulesSource, *predictioneval.CommonAdmission)
	}{
		{"scope namespace", func(s *predictioneval.OrderedRulesSource, _ *predictioneval.CommonAdmission) {
			s.Scope.Namespace = "a-different-namespace"
		}},
		{"scope episode id", func(s *predictioneval.OrderedRulesSource, _ *predictioneval.CommonAdmission) {
			s.Scope.EpisodeID = "episode-2"
		}},
		{"scope account context", func(s *predictioneval.OrderedRulesSource, _ *predictioneval.CommonAdmission) {
			s.Scope.AccountContext = "a-different-account-context"
		}},
		{"scope association evidence", func(s *predictioneval.OrderedRulesSource, _ *predictioneval.CommonAdmission) {
			s.Scope.AssociationEvidence = "the pool id matched, which proves nothing about an account"
		}},
		{"scope coverage", func(s *predictioneval.OrderedRulesSource, _ *predictioneval.CommonAdmission) {
			s.Scope.Coverage = predictioneval.CoverageTruncatedPrefix
		}},
		{"admission manifest id", func(_ *predictioneval.OrderedRulesSource, a *predictioneval.CommonAdmission) {
			a.ManifestID = "acceptance-manifest-2"
		}},
		{"admission population", func(_ *predictioneval.OrderedRulesSource, a *predictioneval.CommonAdmission) {
			a.Population = "every channel candidate, including the ones the fixture excluded"
		}},
		{"admission order basis", func(_ *predictioneval.OrderedRulesSource, a *predictioneval.CommonAdmission) {
			a.OrderBasis = "arrival order at the collector, ascending"
		}},
		// The lower endpoint of the declared interval. It says how far BACK the
		// coverage claim reaches, so moving it downwards asserts that nothing
		// intervened over the additional span — a stronger claim about the
		// absence of an earlier intervention, over an identical prefix.
		{"scope interval start", func(s *predictioneval.OrderedRulesSource, _ *predictioneval.CommonAdmission) {
			s.Scope.IntervalFromPosition = -100
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, adm := orSource(cs, nil), orAdmission()
			tc.apply(&src, &adm)
			stream, err := predictioneval.ProjectOrderedRulesStream(src, adm)
			if err != nil {
				t.Fatalf("the varied source must still project, or this case proves nothing: %v", err)
			}
			got := predictioneval.EvaluateOrderedRules(stream, cfg, orDraws())
			// The premise, spelled out: this must be a real evaluation of a
			// real prefix. Without it the comparison below passes on the empty
			// digest a refusal leaves behind.
			if got.Status != predictioneval.StatusWouldAttempt || got.ConsumedInputDigest == "" {
				t.Fatalf("premise: the varied input must evaluate the same prefix; got %q / %q",
					got.Status, got.ConsumedInputDigest)
			}
			if got.ConsumedInputDigest == base.ConsumedInputDigest {
				t.Fatalf("changing the %s left the consumed-prefix digest unchanged, so a result "+
					"computed over one source can be presented as a result computed over the other. "+
					"The prefix is identical in both; the declaration under which it was read is not.",
					tc.name)
			}
		})
	}

	// The counterweight, and the reason the two interval endpoints are not one
	// value. Widening the upper endpoint must move NOTHING here: it is the
	// extent a later fact can legitimately push outwards, and a consumed-prefix
	// digest that followed it would make "later facts cannot change an earlier
	// result" false by construction. Without this case, binding the whole
	// interval would satisfy every assertion above and break that property
	// silently.
	t.Run("widening the interval end does not move it", func(t *testing.T) {
		src, adm := orSource(cs, nil), orAdmission()
		src.Scope.IntervalToPosition *= 10
		stream, err := predictioneval.ProjectOrderedRulesStream(src, adm)
		if err != nil {
			t.Fatalf("the widened source must still project: %v", err)
		}
		got := predictioneval.EvaluateOrderedRules(stream, cfg, orDraws())
		switch {
		case got.Status != predictioneval.StatusWouldAttempt || got.ConsumedInputDigest == "":
			t.Fatalf("premise: the widened input must evaluate the same prefix; got %q / %q",
				got.Status, got.ConsumedInputDigest)
		case got.StreamDigest == base.StreamDigest:
			t.Fatal("premise: the WHOLE-STREAM digest must move when the declared extent changes, " +
				"or this case cannot tell a bound endpoint from an ignored one")
		case got.ConsumedInputDigest != base.ConsumedInputDigest:
			t.Fatal("widening the declared interval's end moved the consumed-prefix digest. That " +
				"endpoint is the extent a post-boundary fact can legitimately push outwards, so " +
				"binding it here makes the no-look-ahead stability false by construction")
		}
	})
}

// TestOrderedRulesAClaimedRemovalIsChargedForTheSourceItImplies pins the byte
// mirror for the one thing the stream asserts but does not carry.
//
// A stream declaring DroppedAtOrAfter > 0 asserts a SOURCE with that many more
// candidates in it, and the projection charged every one of them — its
// validate-and-charge loop runs before the cut, so a removed candidate costs
// exactly what a retained one costs. The gate charged the retained text and the
// cutoff identity and then stopped, so a forgery could sit within 192 charged
// bytes per claimed removal of the ceiling and pass a gate whose whole purpose
// is to refuse what the projection refuses.
//
// The measurement is differential, and deliberately so. Searching for the
// gate's boundary and then asserting at the boundary found proves nothing: the
// search re-derives the boundary under whatever the code does. Two measurements
// of the same code, differing only in the claimed removal count, cannot be
// satisfied that way — if the claim is free, the boundary does not move at all.
func TestOrderedRulesAClaimedRemovalIsChargedForTheSourceItImplies(t *testing.T) {
	const (
		nc      = predictioneval.MaxOrderedRulesCandidates / 2
		no      = predictioneval.MaxOrderedRulesOutcomes
		max     = predictioneval.MaxOrderedRulesIdentifierBytes
		dropped = predictioneval.MaxOrderedRulesCandidates - nc
		// The floor the gate must reserve per claimed removal, restated here
		// from the projection's own vocabulary rather than imported: identity 1
		// + CHANNEL_UPDATE 14 + PROVEN 6 + KNOWN 5 + a KNOWN balance's 5 + its
		// one mandatory provenance byte. A test that read the constant it is
		// checking would agree with any value the constant took.
		floor = 1 + 14 + 6 + 5 + 5 + 1
	)
	buf := strings.Repeat("x", max)

	// fill spreads exactly n raw bytes across the outcome text slots, so the
	// search granularity is ONE byte — four thousand times finer than the
	// reserve being measured, which is what keeps the reserve from hiding
	// inside a rounding step.
	fill := func(n int) []predictioneval.OrderedRulesCandidate {
		take := func() string {
			switch {
			case n <= 0:
				return ""
			case n >= max:
				n -= max
				return buf
			}
			s := buf[:n]
			n = 0
			return s
		}
		cs := make([]predictioneval.OrderedRulesCandidate, 0, nc)
		for i := 0; i < nc; i++ {
			outs := make([]predictioneval.OrderedRulesOutcome, 0, no)
			for j := 0; j < no; j++ {
				outs = append(outs, predictioneval.OrderedRulesOutcome{
					// Unique within the candidate by LENGTH, so the fixture
					// spends no readable text on identities.
					Identity: buf[:j+1],
					Points: predictioneval.SuppliedInt64{
						Presence:   predictioneval.SuppliedMissing,
						Provenance: take(),
						Reason:     take(),
					},
				})
			}
			cs = append(cs, predictioneval.OrderedRulesCandidate{
				Identity: "c" + itoaTest(i), Position: int64(i + 1), HasPosition: true,
				SourceKind:        predictioneval.SourceKindChannelUpdate,
				EpisodeMembership: predictioneval.MembershipProven,
				OutcomesPresence:  predictioneval.SuppliedKnown,
				Outcomes:          outs,
				Balance:           predictioneval.SuppliedInt64{Presence: predictioneval.SuppliedMissing},
			})
		}
		return cs
	}

	// One genuinely projected stream supplies the boundary, so the forgery
	// below differs from a real stream in exactly one field: the claimed count.
	forged := orProject(t, []predictioneval.OrderedRulesCandidate{
		orCandidate("keep", 1, orMissingBalance())},
		[]predictioneval.OrderedRulesIntervention{
			orIntervention("call-1", 5000, predictioneval.InterventionAutoCallStarted,
				predictioneval.RelevanceProven, "boundary"),
		})
	if !forged.Cutoff.Established {
		t.Fatalf("premise: the fixture must establish a boundary to claim removals against")
	}

	gateMax := func(claimed int) int {
		t.Helper()
		over := func(n int) bool {
			st := forged
			st.Candidates = fill(n)
			st.Cutoff.DroppedAtOrAfter = claimed
			return predictioneval.EvaluateOrderedRules(st, orAlwaysAdmitConfig(), orDraws()).Reason ==
				predictioneval.ReasonStreamBytesOverBound
		}
		lo, hi := 0, nc*no*2*max
		if over(lo) {
			t.Fatalf("premise: an empty charged payload must not be over the byte bound")
		}
		if !over(hi) {
			t.Fatalf("premise: %d bytes must be over the byte bound, or the search has no boundary", hi)
		}
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if over(mid) {
				hi = mid - 1
			} else {
				lo = mid
			}
		}
		return lo
	}

	free, claimed := gateMax(0), gateMax(dropped)
	t.Logf("gate admits %d raw bytes claiming no removals, %d claiming %d (difference %d, floor %d)",
		free, claimed, dropped, free-claimed, dropped*floor)
	if claimed <= 0 {
		t.Fatalf("premise: the measurement bottomed out at %d, so the difference is clipped", claimed)
	}
	if free-claimed < dropped*floor {
		t.Fatalf("claiming %d removals moved the gate's limit by %d bytes, not the %d those removals "+
			"must have cost the projection. A stream can assert a source the projection would have "+
			"refused for bytes and still be read: the claim is free.",
			dropped, free-claimed, dropped*floor)
	}

	// The reserve is arithmetic on a number the caller wrote, and it is applied
	// BEFORE the invariant pass that bounds that number — so the two values
	// that would turn the reserve into a discount have to be refused here, not
	// there. A negative count subtracts outright; a count near the integer
	// maximum overflows the multiplication into a negative one.
	t.Run("an impossible removal count never buys budget", func(t *testing.T) {
		const maxInt = int(^uint(0) >> 1)
		over := func(claimed int) bool {
			st := forged
			st.Candidates = fill(free + 1)
			st.Cutoff.DroppedAtOrAfter = claimed
			return predictioneval.EvaluateOrderedRules(st, orAlwaysAdmitConfig(), orDraws()).Reason ==
				predictioneval.ReasonStreamBytesOverBound
		}
		if !over(0) {
			t.Fatalf("premise: %d bytes must be over the byte bound with nothing claimed", free+1)
		}
		for _, claimed := range []int{-1, -1 << 40, maxInt, maxInt - 1, maxInt/floor + 1} {
			if !over(claimed) {
				t.Fatalf("a stream claiming %d removals was admitted at %d bytes, which the same "+
					"stream claiming none is refused for. The claim bought budget instead of "+
					"spending it.", claimed, free+1)
			}
		}
	})

	// The other direction, which is the one an over-large reserve breaks: the
	// gate must never refuse a stream the projection ADMITTED. The removals
	// here are real and are built at exactly the floor, so a reserve one byte
	// too high refuses the projection's own maximum.
	t.Run("a stream the projection admitted is never refused for the removals it really made", func(t *testing.T) {
		const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
		ins := []predictioneval.OrderedRulesIntervention{
			orIntervention("call-1", 5000, predictioneval.InterventionAutoCallStarted,
				predictioneval.RelevanceProven, "boundary"),
		}
		// The cheapest candidate the projection will admit, repeated: a
		// one-byte identity, the shorter source kind, the only membership, the
		// shortest outcome-vector presence, and a KNOWN balance with the single
		// provenance byte checkPresence demands. No outcomes, no provenance, no
		// outcomes reason.
		build := func(n int) (predictioneval.OrderedRulesStream, error) {
			cs := fill(n)
			for i := 0; i < dropped; i++ {
				cs = append(cs, predictioneval.OrderedRulesCandidate{
					Identity: alphabet[i : i+1], Position: int64(5001 + i), HasPosition: true,
					SourceKind:        predictioneval.SourceKindChannelUpdate,
					EpisodeMembership: predictioneval.MembershipProven,
					OutcomesPresence:  predictioneval.SuppliedKnown,
					Balance: predictioneval.SuppliedInt64{
						Presence: predictioneval.SuppliedKnown, Value: 1,
						Provenance: "p", HasAvailableAtPosition: true,
					},
				})
			}
			return predictioneval.ProjectOrderedRulesStream(orSource(cs, ins), orAdmission())
		}

		lo, hi := 0, nc*no*2*max
		if _, err := build(lo); err != nil {
			t.Fatalf("premise: the smallest source must project: %v", err)
		}
		if _, err := build(hi); err == nil {
			t.Fatalf("premise: %d bytes must be past the projection's budget", hi)
		}
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if _, err := build(mid); err != nil {
				hi = mid - 1
			} else {
				lo = mid
			}
		}
		stream, err := build(lo)
		if err != nil {
			t.Fatalf("the widest admitted source must project: %v", err)
		}
		if stream.Cutoff.DroppedAtOrAfter != dropped {
			t.Fatalf("premise: the boundary must really remove %d candidates; it removed %d",
				dropped, stream.Cutoff.DroppedAtOrAfter)
		}
		t.Logf("projection admits %d raw bytes of retained text alongside %d real removals",
			lo, stream.Cutoff.DroppedAtOrAfter)

		if ev := predictioneval.EvaluateOrderedRules(stream, orAlwaysAdmitConfig(),
			orDraws()); ev.Reason == predictioneval.ReasonStreamBytesOverBound {
			t.Fatalf("the gate refused a stream ProjectOrderedRulesStream ADMITTED, at that "+
				"projection's own maximum. The reserve per removal is larger than the %d bytes the "+
				"cheapest admissible candidate actually costs, which is the invariant backwards.",
				floor)
		}
	})
}
