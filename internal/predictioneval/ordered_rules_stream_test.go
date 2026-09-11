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
	"strconv"
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
		// What matters is that the GATE stands aside; the forged probe is then
		// refused by the structural tier, which is a strictly later decision —
		// if the gate had fired, the reason would be one of its over-bound
		// codes rather than this one.
		st.Candidates[0].Provenance = over[:len(over)-1]
		if ev := predictioneval.EvaluateOrderedRules(st, cfg, orDraws()); ev.Reason !=
			predictioneval.ReasonStreamInvariantViolated {
			t.Fatalf("reason %q, want %s: a string exactly at the limit is admissible and must be "+
				"carried past the gate", ev.Reason, predictioneval.ReasonStreamInvariantViolated)
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
	// forged stream falls through to a later decision — the structural tier,
	// which judges what the stream says rather than how big it is. So the gate
	// refuses oversized input rather than everything. Any over-bound code here
	// would mean the gate fired on admissible input.
	t.Run("exactly at the bounds the shape gate stands aside", func(t *testing.T) {
		st := forged(predictioneval.MaxOrderedRulesCandidates)
		st.Qualifications = make([]string, predictioneval.MaxOrderedRulesQualifications)
		st.Admission.SourceReferences = make([]string,
			predictioneval.MaxOrderedRulesSourceReferences)
		ev := predictioneval.EvaluateOrderedRules(st, cfg,
			orDraws(make([]uint64, predictioneval.MaxOrderedRulesDrawWords)...))
		if ev.Reason != predictioneval.ReasonStreamInvariantViolated {
			t.Fatalf("reason %q, want %s: an input at its bounds is admissible and must be judged "+
				"on its contents", ev.Reason, predictioneval.ReasonStreamInvariantViolated)
		}
		// And it carries NO digest. Passing the gate used to mean the input had
		// been read, because the digest was computed next; the structural tier
		// now decides first and computes nothing, so a refusal from it attests
		// to nothing — the same rule the gate itself follows one step earlier.
		if ev.StreamDigest != "" {
			t.Fatalf("a structural refusal carried a stream digest %q. It was decided before one "+
				"was computed, so it witnesses an input this call never hashed.", ev.StreamDigest)
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
	//
	// Two routes end here now, and the difference is worth keeping rather than
	// flattening. A forgery the STRUCTURAL tier can see is refused before a
	// digest is ever computed, so the oracle gets no turn at all — a stronger
	// result than this case asks for, and the reason those probes stopped
	// reporting a digest mismatch. Everything the structural tier admits — a
	// repeated identity, a duplicated outcome, unencodable text — still needs
	// the oracle to reach the pass that judges it. Both routes are counted
	// below, so neither can quietly become the only one.
	var viaStructure, viaOracle int
	selfConsistent := func(t *testing.T, st predictioneval.OrderedRulesStream) predictioneval.OrderedRulesEvaluation {
		t.Helper()
		st.SelectionDigest = "deliberately wrong"
		first := predictioneval.EvaluateOrderedRules(st, cfg, orDraws())
		if first.Reason == predictioneval.ReasonStreamInvariantViolated {
			if first.StreamDigest != "" {
				t.Fatalf("a structural refusal decided before the digest carried one anyway (%q); "+
					"it computed nothing and must attest to nothing", first.StreamDigest)
			}
			viaStructure++
			return first
		}
		if first.Reason != predictioneval.ReasonStreamDigestMismatch || first.StreamDigest == "" {
			t.Fatalf("the probe must be refused either by the structural tier or by the digest "+
				"comparison; got reason %q", first.Reason)
		}
		viaOracle++
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

	// And both routes really were taken. Without this a change that moved every
	// check into one tier would leave the other path untested while every case
	// above still passed.
	switch {
	case viaStructure == 0:
		t.Error("no probe was refused by the structural tier before the digest, so this case no " +
			"longer covers the cheaper of the two routes")
	case viaOracle == 0:
		t.Error("no probe reached the digest comparison, so the two-call oracle — the thing this " +
			"case is named for — is not exercised at all")
	}
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
	//     bound, and TestDigestsBindTheMandatoryFieldDeclarations' neighbours
	//     cover the coupling;
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
// The floor is per VIEW, not per model, and an earlier version of this case did
// not know that. The projection couples the view to the source kind strictly —
// CALCULATE_ONLY admits only CALCULATE_SNAPSHOT (18 bytes), CHANNEL_CANDIDATE_STREAM
// only CHANNEL_UPDATE (14) — so the floor is the EXACT cost in each view, and
// charging the cheaper of the two everywhere undercharged a calculate-only
// stream by 24 encoded bytes a removal. Both views are measured below, each
// against its own floor; a view-independent reserve fails the calculate-only
// one.
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
		// The part of the floor no view changes, restated here from the
		// projection's vocabulary rather than imported: identity 1 + PROVEN 6 +
		// KNOWN 5 + a KNOWN balance's 5 + its one mandatory provenance byte. A
		// test that read the constant it is checking would agree with any value
		// that constant took, so the source-kind lengths below are spelled out
		// as literals too.
		common = 1 + 6 + 5 + 5 + 1
	)
	buf := strings.Repeat("x", max)

	views := []struct {
		name  string
		view  predictioneval.OrderedRulesViewKind
		kind  predictioneval.OrderedRulesSourceKind
		floor int
	}{
		{"channel candidate stream", predictioneval.ViewChannelCandidateStream,
			predictioneval.SourceKindChannelUpdate, common + 14},
		{"calculate only", predictioneval.ViewCalculateOnly,
			predictioneval.SourceKindCalculateSnapshot, common + 18},
	}

	// fill spreads exactly n raw bytes across the outcome text slots, so the
	// search granularity is ONE byte — four thousand times finer than the
	// reserve being measured, which is what keeps the reserve from hiding
	// inside a rounding step.
	fill := func(n int, kind predictioneval.OrderedRulesSourceKind) []predictioneval.OrderedRulesCandidate {
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
				SourceKind:        kind,
				EpisodeMembership: predictioneval.MembershipProven,
				OutcomesPresence:  predictioneval.SuppliedKnown,
				Outcomes:          outs,
				Balance:           predictioneval.SuppliedInt64{Presence: predictioneval.SuppliedMissing},
			})
		}
		return cs
	}

	boundary := []predictioneval.OrderedRulesIntervention{
		orIntervention("call-1", 5000, predictioneval.InterventionAutoCallStarted,
			predictioneval.RelevanceProven, "boundary"),
	}

	for _, v := range views {
		t.Run(v.name, func(t *testing.T) {
			adm := orAdmission()
			adm.ViewKind = v.view

			// One genuinely projected stream supplies the boundary, so the
			// forgery below differs from a real stream in exactly one field:
			// the claimed count.
			keep := orCandidate("keep", 1, orMissingBalance())
			keep.SourceKind = v.kind
			forged, err := predictioneval.ProjectOrderedRulesStream(
				orSource([]predictioneval.OrderedRulesCandidate{keep}, boundary), adm)
			if err != nil {
				t.Fatalf("premise: the boundary fixture must project in this view: %v", err)
			}
			if !forged.Cutoff.Established {
				t.Fatal("premise: the fixture must establish a boundary to claim removals against")
			}

			gateMax := func(claimed int) int {
				t.Helper()
				over := func(n int) bool {
					st := forged
					st.Candidates = fill(n, v.kind)
					st.Cutoff.DroppedAtOrAfter = claimed
					return predictioneval.EvaluateOrderedRules(st, orAlwaysAdmitConfig(), orDraws()).Reason ==
						predictioneval.ReasonStreamBytesOverBound
				}
				lo, hi := 0, nc*no*2*max
				if over(lo) {
					t.Fatal("premise: an empty charged payload must not be over the byte bound")
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
				free, claimed, dropped, free-claimed, dropped*v.floor)
			if claimed <= 0 {
				t.Fatalf("premise: the measurement bottomed out at %d, so the difference is clipped", claimed)
			}
			if free-claimed < dropped*v.floor {
				t.Fatalf("claiming %d removals moved the gate's limit by %d bytes, not the %d those "+
					"removals must have cost the projection in a %s view. A stream can assert a "+
					"source the projection would have refused for bytes and still be read.",
					dropped, free-claimed, dropped*v.floor, v.view)
			}

			// The reserve is arithmetic on a number the caller wrote, and it is
			// applied BEFORE the invariant pass that bounds that number — so
			// the two values that would turn the reserve into a discount have
			// to be refused here, not there. A negative count subtracts
			// outright; a count near the integer maximum overflows the
			// multiplication into a negative one.
			t.Run("an impossible removal count never buys budget", func(t *testing.T) {
				const maxInt = int(^uint(0) >> 1)
				over := func(claimed int) bool {
					st := forged
					st.Candidates = fill(free+1, v.kind)
					st.Cutoff.DroppedAtOrAfter = claimed
					return predictioneval.EvaluateOrderedRules(st, orAlwaysAdmitConfig(), orDraws()).Reason ==
						predictioneval.ReasonStreamBytesOverBound
				}
				if !over(0) {
					t.Fatalf("premise: %d bytes must be over the byte bound with nothing claimed", free+1)
				}
				for _, claimed := range []int{-1, -1 << 40, maxInt, maxInt - 1, maxInt/v.floor + 1} {
					if !over(claimed) {
						t.Fatalf("a stream claiming %d removals was admitted at %d bytes, which the "+
							"same stream claiming none is refused for. The claim bought budget "+
							"instead of spending it.", claimed, free+1)
					}
				}
			})

			// The other direction, which is the one an over-large reserve
			// breaks: the gate must never refuse a stream the projection
			// ADMITTED. The removals here are real and are built at exactly the
			// floor for this view, so a reserve one byte too high refuses the
			// projection's own maximum.
			t.Run("a stream the projection admitted is never refused for the removals it really made", func(t *testing.T) {
				const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
				// The cheapest candidate the projection will admit in this
				// view: a one-byte identity, the source kind the view forces,
				// the only membership, the shortest outcome-vector presence,
				// and a KNOWN balance with the single provenance byte
				// checkPresence demands. No outcomes, no provenance, no
				// outcomes reason.
				build := func(n int) (predictioneval.OrderedRulesStream, error) {
					cs := fill(n, v.kind)
					for i := 0; i < dropped; i++ {
						cs = append(cs, predictioneval.OrderedRulesCandidate{
							Identity: alphabet[i : i+1], Position: int64(5001 + i), HasPosition: true,
							SourceKind:        v.kind,
							EpisodeMembership: predictioneval.MembershipProven,
							OutcomesPresence:  predictioneval.SuppliedKnown,
							Balance: predictioneval.SuppliedInt64{
								Presence: predictioneval.SuppliedKnown, Value: 1,
								Provenance: "p", HasAvailableAtPosition: true,
							},
						})
					}
					return predictioneval.ProjectOrderedRulesStream(orSource(cs, boundary), adm)
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
						"projection's own maximum. The reserve per removal is larger than the %d "+
						"bytes the cheapest admissible candidate actually costs in a %s view, "+
						"which is the invariant backwards.", v.floor, v.view)
				}
			})
		})
	}
}

// TestOrderedRulesAnAdmittedStreamSurvivesItsOwnJSONRoundTrip closes the gap
// between "admitted" and "storable".
//
// The stream type carries JSON tags because a projected stream is meant to be
// written out and read back — that is the reason an unexported marker was
// rejected as an integrity mechanism earlier in this package. But Go's encoder
// does not fail on a byte that is not valid UTF-8; it substitutes U+FFFD. So a
// stream holding one projected, digested over the ORIGINAL bytes, and came back
// from its own round trip holding different bytes and a digest that no longer
// matched: an admitted stream refused as a forgery, with nothing forged.
//
// The control is the part that makes this test mean something. A genuine U+FFFD
// is valid UTF-8 and must still be admitted — otherwise the rule would be
// "refuse the replacement character", which is a different and wrong rule that
// would pass every case below.
func TestOrderedRulesAnAdmittedStreamSurvivesItsOwnJSONRoundTrip(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{
		orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
	}
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 40, 100, 0, 10)}, 95, 100, 0, 10)

	t.Run("a projected stream round trips with its evidence intact", func(t *testing.T) {
		st := orProject(t, cs, nil)
		direct := predictioneval.EvaluateOrderedRules(st, cfg, orDraws())
		if direct.Status != predictioneval.StatusWouldAttempt {
			t.Fatalf("premise: the fixture must evaluate; got %q", direct.Status)
		}
		blob, err := json.Marshal(st)
		if err != nil {
			t.Fatalf("marshalling a projected stream failed: %v", err)
		}
		var back predictioneval.OrderedRulesStream
		if err := json.Unmarshal(blob, &back); err != nil {
			t.Fatalf("unmarshalling a projected stream failed: %v", err)
		}
		rt := predictioneval.EvaluateOrderedRules(back, cfg, orDraws())
		switch {
		case rt.Status != direct.Status || rt.Reason != direct.Reason:
			t.Fatalf("a projected stream changed verdict across its own JSON round trip: "+
				"%q/%q became %q/%q", direct.Status, direct.Reason, rt.Status, rt.Reason)
		case rt.StreamDigest != direct.StreamDigest ||
			rt.ConsumedInputDigest != direct.ConsumedInputDigest:
			t.Fatal("a projected stream's digests moved across its own JSON round trip")
		}
	})

	// The control, first, because the cases after it are only meaningful if
	// this holds: the replacement character itself is valid UTF-8.
	t.Run("a genuine replacement character is still admitted", func(t *testing.T) {
		src, adm := orSource(cs, nil), orAdmission()
		src.Scope.Namespace = "ns-�-tail"
		st, err := predictioneval.ProjectOrderedRulesStream(src, adm)
		if err != nil {
			t.Fatalf("a genuine U+FFFD is valid UTF-8 and must project: %v", err)
		}
		if got := predictioneval.EvaluateOrderedRules(st, cfg, orDraws()); got.Status !=
			predictioneval.StatusWouldAttempt {
			t.Fatalf("status = %q, want the fixture's own verdict", got.Status)
		}
	})

	// One invalid byte, in each retained string the projection keeps.
	const bad = "x-\xff-y"
	for _, tc := range []struct {
		name  string
		apply func(*predictioneval.OrderedRulesSource, *predictioneval.CommonAdmission)
	}{
		{"scope namespace", func(s *predictioneval.OrderedRulesSource, _ *predictioneval.CommonAdmission) {
			s.Scope.Namespace = bad
		}},
		{"scope episode id", func(s *predictioneval.OrderedRulesSource, _ *predictioneval.CommonAdmission) {
			s.Scope.EpisodeID = bad
		}},
		{"scope account context", func(s *predictioneval.OrderedRulesSource, _ *predictioneval.CommonAdmission) {
			s.Scope.AccountContext = bad
		}},
		{"scope association evidence", func(s *predictioneval.OrderedRulesSource, _ *predictioneval.CommonAdmission) {
			s.Scope.AssociationEvidence = bad
		}},
		{"scope coverage detail", func(s *predictioneval.OrderedRulesSource, _ *predictioneval.CommonAdmission) {
			s.Scope.CoverageDetail = bad
		}},
		{"admission manifest id", func(_ *predictioneval.OrderedRulesSource, a *predictioneval.CommonAdmission) {
			a.ManifestID = bad
		}},
		{"admission population", func(_ *predictioneval.OrderedRulesSource, a *predictioneval.CommonAdmission) {
			a.Population = bad
		}},
		{"admission order basis", func(_ *predictioneval.OrderedRulesSource, a *predictioneval.CommonAdmission) {
			a.OrderBasis = bad
		}},
		{"admission source reference", func(_ *predictioneval.OrderedRulesSource, a *predictioneval.CommonAdmission) {
			a.SourceReferences = []string{bad}
		}},
		{"candidate identity", func(s *predictioneval.OrderedRulesSource, _ *predictioneval.CommonAdmission) {
			s.Candidates[0].Identity = bad
		}},
		{"candidate provenance", func(s *predictioneval.OrderedRulesSource, _ *predictioneval.CommonAdmission) {
			s.Candidates[0].Provenance = bad
		}},
		{"candidate outcomes reason", func(s *predictioneval.OrderedRulesSource, _ *predictioneval.CommonAdmission) {
			s.Candidates[0].OutcomesReason = bad
		}},
		{"balance provenance", func(s *predictioneval.OrderedRulesSource, _ *predictioneval.CommonAdmission) {
			s.Candidates[0].Balance.Provenance = bad
		}},
		{"outcome identity", func(s *predictioneval.OrderedRulesSource, _ *predictioneval.CommonAdmission) {
			s.Candidates[0].Outcomes[0].Identity = bad
		}},
		{"outcome points provenance", func(s *predictioneval.OrderedRulesSource, _ *predictioneval.CommonAdmission) {
			s.Candidates[0].Outcomes[0].Points.Provenance = bad
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, adm := orSource(append([]predictioneval.OrderedRulesCandidate(nil), cs...), nil), orAdmission()
			src.Candidates[0].Outcomes = append([]predictioneval.OrderedRulesOutcome(nil),
				cs[0].Outcomes...)
			tc.apply(&src, &adm)
			_, err := predictioneval.ProjectOrderedRulesStream(src, adm)
			if err == nil {
				t.Fatalf("a %s that is not valid UTF-8 was admitted; the stream it produces cannot "+
					"survive its own JSON round trip", tc.name)
			}
			if !errors.Is(err, predictioneval.ErrOrderedRulesNotEncodable) {
				t.Fatalf("refused, but not as unencodable text: %v", err)
			}
		})
	}

	// The mirror. A forged stream carrying the same byte must be refused by the
	// evaluator too, through the two-call digest oracle — otherwise the rule is
	// enforced on one path only, which is the divergence this package keeps
	// having to close.
	t.Run("the evaluator refuses what the projection refused", func(t *testing.T) {
		st := orProject(t, cs, nil)
		st.Scope.Namespace = bad
		first := predictioneval.EvaluateOrderedRules(st, cfg, orDraws())
		st.SelectionDigest = first.StreamDigest
		second := predictioneval.EvaluateOrderedRules(st, cfg, orDraws())
		if second.Status != predictioneval.StatusRefused {
			t.Fatalf("a stream carrying invalid UTF-8 reached %q through the digest oracle; the "+
				"projection refuses that exact source", second.Status)
		}
	})
}

// TestOrderedRulesAWindowExhaustedToItsEndIsBoundToThatEnd pins the second half
// of the interval asymmetry.
//
// The upper endpoint stays out of the consumed digest while an unconsumed
// suffix can still legitimately grow. But when END OF STREAM is what
// established the result, there is no such suffix, and the endpoint stops being
// extent and becomes evidence: two empty COMPLETE_DECLARED sources over [0,10]
// and [0,100] both answer NO_ATTEMPT_IN_SUPPLIED_PREFIX, and only the second
// also claims that nothing existed through position 100.
//
// Both halves are asserted, because binding it unconditionally is the obvious
// wrong repair and passes the first half on its own.
func TestOrderedRulesAWindowExhaustedToItsEndIsBoundToThatEnd(t *testing.T) {
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 40, 100, 0, 10)}, 95, 100, 0, 10)

	eval := func(t *testing.T, to int64, cs []predictioneval.OrderedRulesCandidate,
		ins []predictioneval.OrderedRulesIntervention) predictioneval.OrderedRulesEvaluation {
		t.Helper()
		src, adm := orSource(cs, ins), orAdmission()
		src.Scope.IntervalToPosition = to
		st, err := predictioneval.ProjectOrderedRulesStream(src, adm)
		if err != nil {
			t.Fatalf("projecting over [0,%d] failed: %v", to, err)
		}
		return predictioneval.EvaluateOrderedRules(st, cfg, orDraws())
	}

	t.Run("exhaustion binds the end it reached", func(t *testing.T) {
		near, far := eval(t, 10, nil, nil), eval(t, 100, nil, nil)
		switch {
		case near.Status != predictioneval.StatusNoAttemptInSuppliedPrefix ||
			far.Status != predictioneval.StatusNoAttemptInSuppliedPrefix:
			t.Fatalf("premise: both must end by exhaustion; got %q and %q", near.Status, far.Status)
		case near.Cutoff.Established || far.Cutoff.Established:
			t.Fatal("premise: this case needs no boundary established")
		case near.ConsumedInputDigest == far.ConsumedInputDigest:
			t.Fatal("a traversal that exhausted [0,10] and one that exhausted [0,100] share a " +
				"consumed-prefix binding. Both looked and found nothing, but the second searched " +
				"ninety positions further, and that is the stronger claim of the two")
		}
	})

	// The counterweight: with a boundary established the suffix is real — the
	// candidates it removed — so appending more of them must not move this
	// digest even though the traversal still ends by running out of retained
	// stream. Binding the endpoint unconditionally breaks exactly this.
	t.Run("a boundary keeps the end out even at exhaustion", func(t *testing.T) {
		ins := []predictioneval.OrderedRulesIntervention{
			orIntervention("call-1", 5, predictioneval.InterventionAutoCallStarted,
				predictioneval.RelevanceProven, "boundary"),
		}
		// One candidate before the boundary whose pool the donor declines, so
		// the traversal reaches the end of the retained stream without deciding.
		cs := []predictioneval.OrderedRulesCandidate{
			orCandidate("c1", 1, orKnownBalance(1000), orOutcome("A", 1)),
		}
		near, far := eval(t, 10, cs, ins), eval(t, 100, cs, ins)
		switch {
		case near.Status != predictioneval.StatusNoAttemptInSuppliedPrefix ||
			far.Status != predictioneval.StatusNoAttemptInSuppliedPrefix:
			t.Fatalf("premise: both must end by exhaustion; got %q and %q", near.Status, far.Status)
		case !near.Cutoff.Established || !far.Cutoff.Established:
			t.Fatal("premise: this case needs a boundary established")
		case near.StreamDigest == far.StreamDigest:
			t.Fatal("premise: the WHOLE-STREAM digest must separate the two declared windows, " +
				"or this case cannot tell a bound endpoint from an ignored one")
		case near.ConsumedInputDigest != far.ConsumedInputDigest:
			t.Fatal("with a boundary established, widening the declared end moved the " +
				"consumed-prefix digest. That stream has an unconsumed suffix by construction, so " +
				"binding the end makes the no-look-ahead stability false by construction")
		}
	})
}

// TestOrderedRulesEveryRetainedScopeAndAdmissionStringIsRevalidatedOnIngest
// enumerates instead of listing, because listing is what kept failing.
//
// Five times in this pull request a rule has been enforced in the projection
// and not re-established on ingest, and the fifth was introduced by the repair
// for the fourth: the UTF-8 check went into checkFreeText and checkIdentifier
// on the claim that every retained string passes through them, and two did not.
// Scope.CoverageDetail and each Admission.SourceReferences entry were checked
// only at the projection's own call site, so validateScope and validateAdmission
// — the only validators the invariant pass calls — never revisited them, and a
// forged stream carrying an invalid byte in either reached WOULD_ATTEMPT through
// the two-call digest oracle.
//
// So this walks the two structs by reflection rather than naming their fields.
// A plain string or []string is free text the caller chooses; a named string
// type is a closed vocabulary refused by its own switch. Adding a retained
// free-text field to either struct without validating it fails here without
// anyone remembering to extend a list.
func TestOrderedRulesEveryRetainedScopeAndAdmissionStringIsRevalidatedOnIngest(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{
		orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
	}
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 40, 100, 0, 10)}, 95, 100, 0, 10)

	// The control: untouched, this stream evaluates. Every refusal below is
	// therefore attributable to the byte the case introduced.
	if got := predictioneval.EvaluateOrderedRules(orProject(t, cs, nil), cfg,
		orDraws()); got.Status != predictioneval.StatusWouldAttempt {
		t.Fatalf("premise: the intact fixture must evaluate; got %q", got.Status)
	}

	visited := map[string]bool{}

	// vocabularies names the fields whose values are a CLOSED set, refused by
	// their own switch rather than by a text rule.
	//
	// This used to be inferred from the TYPE — a named string type was taken to
	// be a vocabulary and skipped — and Go does not support that inference. A
	// named string type is a string with a name; nothing obliges it to have a
	// switch, and the next `type CoverageNote string` added to either struct
	// would have been skipped silently by a guard whose entire purpose is to
	// notice a retained string nobody validates.
	//
	// So the exemption is written down, and it is CHECKED rather than trusted:
	// each name below gets a value outside its vocabulary and must be refused
	// for it, and each must actually be reached. An entry that stops being a
	// vocabulary fails here instead of quietly exempting itself.
	vocabularies := map[string]bool{
		"Scope.Coverage":     true,
		"Admission.ViewKind": true,
	}

	// walk probes every string-BACKED field it finds — plain, named, or a slice
	// of either — and requires the forged stream to be refused through the
	// digest oracle. Vocabulary fields are probed with a word outside their set;
	// every other field with one invalid byte.
	walk := func(t *testing.T, owner string, pick func(*predictioneval.OrderedRulesStream) reflect.Value) {
		probe := orProject(t, cs, nil)
		rt := pick(&probe).Type()
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if f.PkgPath != "" {
				continue
			}
			slice := f.Type.Kind() == reflect.Slice && f.Type.Elem().Kind() == reflect.String
			if f.Type.Kind() != reflect.String && !slice {
				continue
			}
			name := owner + "." + f.Name
			visited[name] = true
			vocabulary := vocabularies[name]
			t.Run(name, func(t *testing.T) {
				st := orProject(t, cs, nil)
				fv := pick(&st).Field(i)
				switch {
				case vocabulary:
					fv.SetString("NOT-IN-THE-VOCABULARY")
				case slice:
					probe := reflect.MakeSlice(f.Type, 1, 1)
					probe.Index(0).SetString("ref-\xff")
					fv.Set(probe)
				default:
					fv.SetString(fv.String() + "-\xff")
				}
				first := predictioneval.EvaluateOrderedRules(st, cfg, orDraws())
				st.SelectionDigest = first.StreamDigest
				second := predictioneval.EvaluateOrderedRules(st, cfg, orDraws())
				if second.Status != predictioneval.StatusRefused {
					what := "an invalid byte"
					if vocabulary {
						what = "a word outside its vocabulary"
					}
					t.Fatalf("a stream carrying %s in %s reached %q through the digest oracle. "+
						"ProjectOrderedRulesStream refuses that exact source, so the rule is "+
						"enforced on one path only and the oracle walks straight through it.",
						what, name, second.Status)
				}
			})
		}
	}

	walk(t, "Scope", func(s *predictioneval.OrderedRulesStream) reflect.Value {
		return reflect.ValueOf(&s.Scope).Elem()
	})
	walk(t, "Admission", func(s *predictioneval.OrderedRulesStream) reflect.Value {
		return reflect.ValueOf(&s.Admission).Elem()
	})

	// The walk must actually have reached the two fields that were missing, the
	// two vocabularies claiming an exemption, and enough others that an empty or
	// collapsed traversal cannot pass quietly.
	want := []string{
		"Scope.CoverageDetail", "Admission.SourceReferences",
		"Scope.Namespace", "Scope.AssociationEvidence", "Admission.Population",
	}
	for name := range vocabularies {
		want = append(want, name)
	}
	for _, name := range want {
		if !visited[name] {
			t.Errorf("the reflection walk never reached %s, so this case is not covering what it "+
				"claims; it visited %d fields", name, len(visited))
		}
	}
}

// TestOrderedRulesTheConfigAndTraceIdentifiersMustAlsoBeEncodable extends the
// round-trip rule to the two strings no validator reads.
//
// The config identifier and the run identifier ride a ceiling of their own and
// deliberately carry no per-string bound — inventing a limit no other rule
// applies would refuse input nothing else refuses. That reasoning is about
// LENGTH and says nothing about encodability, and both types carry JSON tags.
// Invalid UTF-8 in either was hashed as supplied and then replaced with U+FFFD
// by the encoder, so the same logical run digested differently before and after
// its own round trip: ConfigDigest e1a65694… became 5251cf94…, the consumed
// digest moved with it, and both evaluations returned WOULD_ATTEMPT.
//
// These two are also the only retained strings the invariant pass never reads,
// so the shape gate is the one place that can refuse them.
//
// Position is the whole subtlety here, and the first attempt got it wrong. The
// scan began life inside the shape gate, which runs BEFORE the two version
// comparisons — so a wrong contract version beside a 64 MiB valid identifier
// went from 1.834 µs to 44.7 ms, reintroducing attacker-controlled linear work
// on exactly the early-refusal path an earlier repair had made constant-time.
// It now runs after those comparisons, and the case below pins that: a run that
// is BOTH version-mismatched and unencodable must be refused for the version.
//
// The ceilings still come first, in the gate. That half is NOT asserted here,
// and is stated rather than left as an apparent gap: demonstrating it needs an
// identifier past MaxOrderedRulesAggregateBytes, and a 128 MiB fixture is two
// orders of magnitude beyond anything else in this suite, whose largest is
// 1 MiB. It is argued in the source and unpinned by a test.
func TestOrderedRulesTheConfigAndTraceIdentifiersMustAlsoBeEncodable(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{
		orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
	}
	rules := []predictioneval.OrderedRule{orRule(predictioneval.ComparatorLe, 40, 100, 0, 10)}
	const bad = "id-\xff-tail"

	base := predictioneval.EvaluateOrderedRules(orProject(t, cs, nil),
		orConfig(rules, 95, 100, 0, 10), orDraws())
	if base.Status != predictioneval.StatusWouldAttempt {
		t.Fatalf("premise: the intact fixture must evaluate; got %q", base.Status)
	}

	for _, tc := range []struct {
		name  string
		apply func(*predictioneval.OrderedRulesConfig, *predictioneval.SuppliedDrawTrace, string)
	}{
		{"config identifier", func(c *predictioneval.OrderedRulesConfig,
			_ *predictioneval.SuppliedDrawTrace, v string) {
			c.ConfigID = v
		}},
		{"run identifier", func(_ *predictioneval.OrderedRulesConfig,
			d *predictioneval.SuppliedDrawTrace, v string) {
			d.RunID = v
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, draws := orConfig(rules, 95, 100, 0, 10), orDraws()
			tc.apply(&cfg, &draws, bad)
			got := predictioneval.EvaluateOrderedRules(orProject(t, cs, nil), cfg, draws)
			switch {
			case got.Reason != predictioneval.ReasonSuppliedTextNotEncodable:
				t.Fatalf("an unencodable %s was answered %q / %q; the same logical run would "+
					"digest differently after its own JSON round trip", tc.name, got.Status, got.Reason)
			// The strongest form of the refusal, and the reason it belongs in
			// the gate: nothing was hashed at all before saying no.
			case got.StreamDigest != "" || got.ConfigDigest != "" ||
				got.EntropyDigest != "" || got.ConsumedInputDigest != "":
				t.Fatal("the refusal carried digests; a value refused for being unencodable must be " +
					"refused before anything is hashed, like the other unread refusals")
			}
		})

		// The control, again: a genuine U+FFFD is valid UTF-8. Without it the
		// rule could be "refuse the replacement character" and the case above
		// would still pass.
		t.Run(tc.name+" accepts a genuine replacement character", func(t *testing.T) {
			cfg, draws := orConfig(rules, 95, 100, 0, 10), orDraws()
			tc.apply(&cfg, &draws, "id-�-tail")
			got := predictioneval.EvaluateOrderedRules(orProject(t, cs, nil), cfg, draws)
			if got.Status != predictioneval.StatusWouldAttempt {
				t.Fatalf("a genuine U+FFFD in the %s is valid UTF-8 and must be admitted; got %q / %q",
					tc.name, got.Status, got.Reason)
			}
		})
	}

	// The ordering that the first attempt at this got wrong. Both faults are
	// present at once, and the CHEAPER refusal must win: two string comparisons
	// rather than a scan of an identifier that may approach the aggregate
	// ceiling. Without this case the scan can drift back ahead of them and
	// every other case here still passes.
	t.Run("a version mismatch is decided before either identifier is scanned", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			broken func(*predictioneval.OrderedRulesStream, *predictioneval.SuppliedDrawTrace)
			want   string
		}{
			{"stream contract version", func(st *predictioneval.OrderedRulesStream,
				_ *predictioneval.SuppliedDrawTrace) {
				st.ContractVersion = "WRONG_CONTRACT_VERSION"
			}, predictioneval.ReasonStreamContractMismatch},
			{"entropy semantics version", func(_ *predictioneval.OrderedRulesStream,
				d *predictioneval.SuppliedDrawTrace) {
				d.EntropySemanticsVersion = "WRONG_ENTROPY_SEMANTICS"
			}, predictioneval.ReasonEntropySemanticsMismatch},
		} {
			t.Run(tc.name, func(t *testing.T) {
				st, draws := orProject(t, cs, nil), orDraws()
				cfg := orConfig(rules, 95, 100, 0, 10)
				cfg.ConfigID = bad
				draws.RunID = bad
				tc.broken(&st, &draws)
				got := predictioneval.EvaluateOrderedRules(st, cfg, draws)
				if got.Reason != tc.want {
					t.Fatalf("a run that is both %s-mismatched and unencodable was refused %q, "+
						"want %q. The version comparison is two strings against constants; the "+
						"scan walks identifiers that may approach the aggregate ceiling, and it "+
						"must not decide first.", tc.name, got.Reason, tc.want)
				}
			})
		}
	})

	// The positive half: with encodable identifiers, the config and the trace
	// survive their own round trip with every digest intact.
	t.Run("an admitted run digests identically across its own round trip", func(t *testing.T) {
		cfg, draws := orConfig(rules, 95, 100, 0, 10), orDraws()
		cfgBlob, err := json.Marshal(cfg)
		if err != nil {
			t.Fatalf("marshalling the config failed: %v", err)
		}
		drawsBlob, err := json.Marshal(draws)
		if err != nil {
			t.Fatalf("marshalling the trace failed: %v", err)
		}
		var cfgBack predictioneval.OrderedRulesConfig
		var drawsBack predictioneval.SuppliedDrawTrace
		if err := json.Unmarshal(cfgBlob, &cfgBack); err != nil {
			t.Fatalf("unmarshalling the config failed: %v", err)
		}
		if err := json.Unmarshal(drawsBlob, &drawsBack); err != nil {
			t.Fatalf("unmarshalling the trace failed: %v", err)
		}
		got := predictioneval.EvaluateOrderedRules(orProject(t, cs, nil), cfgBack, drawsBack)
		switch {
		case got.Status != base.Status:
			t.Fatalf("a round-tripped run changed verdict: %q became %q", base.Status, got.Status)
		case got.ConfigDigest != base.ConfigDigest || got.EntropyDigest != base.EntropyDigest ||
			got.ConsumedInputDigest != base.ConsumedInputDigest:
			t.Fatal("a round-tripped run moved a digest; the identifiers are encodable, so nothing " +
				"in the encoding should have changed")
		}
	})
}

// TestOrderedRulesACheapRefusalIsNotPaidForWithTheWholePayload pins two
// orderings whose only symptom is cost.
//
// Both are the same rule the rest of this package applies — bound the work a
// refusal can be made to do — and both were violated by checks that were
// correct in every other respect. Neither admits anything it should not; they
// were simply reachable at a price the caller chose.
//
// Cost cannot be asserted here: this repository's deterministic-test contract
// bars timing. So each is pinned by the ORDER it implies, which is observable
// and exact. The measurements that motivated them are recorded in the source
// comments beside each fix.
func TestOrderedRulesACheapRefusalIsNotPaidForWithTheWholePayload(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{
		orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
	}
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 40, 100, 0, 10)}, 95, 100, 0, 10)

	// The projection establishes the boundary from the interventions, which are
	// already bounded in count and text, BEFORE walking the candidates, which
	// are the bulk of the admitted budget. An intervention rejectable on its
	// shape alone must therefore win against a rejectable candidate: 44 µs
	// rather than 5.0 ms on a fixture far short of the ceiling.
	t.Run("a malformed intervention is refused before the candidates are walked", func(t *testing.T) {
		broken := []predictioneval.OrderedRulesIntervention{
			orIntervention("call-1", 5000, predictioneval.InterventionAutoCallStarted,
				predictioneval.RelevanceProven, "boundary"),
		}
		broken[0].HasPosition = false

		// A candidate fault the walk would raise, with its own sentinel, so the
		// two are told apart by which rule fired rather than by wording.
		duplicated := []predictioneval.OrderedRulesCandidate{
			orCandidate("same", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
			orCandidate("same", 20, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
		}
		// The premise: each fault really is raised on its own.
		if _, err := predictioneval.ProjectOrderedRulesStream(orSource(cs, broken),
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesScopeIncomplete) {
			t.Fatalf("premise: the intervention alone must be refused as incomplete; got %v", err)
		}
		if _, err := predictioneval.ProjectOrderedRulesStream(orSource(duplicated, nil),
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesDuplicateIdentity) {
			t.Fatalf("premise: the candidates alone must be refused as duplicated; got %v", err)
		}

		_, err := predictioneval.ProjectOrderedRulesStream(orSource(duplicated, broken), orAdmission())
		if !errors.Is(err, predictioneval.ErrOrderedRulesScopeIncomplete) {
			t.Fatalf("a source with both a malformed intervention and a duplicated candidate was "+
				"refused %v. The boundary comes from interventions that are already bounded in "+
				"count and text; the candidate walk is the bulk of the budget and must not run "+
				"first.", err)
		}
	})

	// Candidate uniqueness depends on nothing but the candidate identities, so
	// it is settled before the three payload scans that follow it in the
	// projection — the admission references, the cutoff and every intervention
	// Detail. It used to be settled after all three: 7.981383 ms to report a
	// repeated three-byte identity beside 12 MiB of unrelated supplied text,
	// against 127.186 µs once the pass moved.
	t.Run("a repeated candidate identity is refused before the admission payload", func(t *testing.T) {
		duplicated := []predictioneval.OrderedRulesCandidate{
			orCandidate("same", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
			orCandidate("same", 20, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
		}
		// An ADMISSION fault with its own sentinel, so the two are told apart
		// by which rule fired rather than by wording. A source reference one
		// byte past the per-string bound is refused by validateAdmission, which
		// is the first of the three scans this pass now precedes.
		overBound := orAdmission()
		overBound.SourceReferences = []string{strings.Repeat("x", predictioneval.MaxOrderedRulesIdentifierBytes+1)}

		// The premise: each fault really is raised on its own.
		if _, err := predictioneval.ProjectOrderedRulesStream(orSource(cs, nil),
			overBound); !errors.Is(err, predictioneval.ErrOrderedRulesOverBound) {
			t.Fatalf("premise: the admission alone must be refused as over-bound; got %v", err)
		}
		if _, err := predictioneval.ProjectOrderedRulesStream(orSource(duplicated, nil),
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesDuplicateIdentity) {
			t.Fatalf("premise: the candidates alone must be refused as duplicated; got %v", err)
		}

		_, err := predictioneval.ProjectOrderedRulesStream(orSource(duplicated, nil), overBound)
		if !errors.Is(err, predictioneval.ErrOrderedRulesDuplicateIdentity) {
			t.Fatalf("a source repeating a candidate identity beside an over-bound admission "+
				"reference was refused %v. Uniqueness reads nothing but the identities and is "+
				"capped at MaxOrderedRulesCandidates x MaxOrderedRulesIdentifierBytes; the three "+
				"scans it precedes are three times that envelope, and none of them is consulted "+
				"to decide that two candidates carry the same name.", err)
		}
	})

	// A closed-set word settled at its bound, ahead of the identity pass that
	// the projection runs before validateAdmission. The evaluator's own tier
	// already compared both of these; the projection did not, which made a
	// four-byte view kind cost a walk of every candidate — 3.28 µs on two
	// against 459.124 µs on 128 holding 4 KiB each, and 15.646 µs after.
	// Coverage was the same defect one step earlier: 1.527 µs, 75.238 µs,
	// 15.9 µs.
	t.Run("an unsupported vocabulary word is refused before the candidate identities", func(t *testing.T) {
		duplicated := []predictioneval.OrderedRulesCandidate{
			orCandidate("same", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
			orCandidate("same", 20, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
		}
		// The premise: the candidate fault really is raised on its own, under
		// its OWN sentinel, so the two are told apart by which rule fired.
		if _, err := predictioneval.ProjectOrderedRulesStream(orSource(duplicated, nil),
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesDuplicateIdentity) {
			t.Fatalf("premise: the candidates alone must be refused as duplicated; got %v", err)
		}

		for _, c := range []struct {
			name     string
			mutate   func(*predictioneval.OrderedRulesSource, *predictioneval.CommonAdmission)
			aloneErr error
		}{
			{"admission view kind", func(_ *predictioneval.OrderedRulesSource, a *predictioneval.CommonAdmission) {
				a.ViewKind = "NOT-A-VIEW-KIND"
			}, predictioneval.ErrOrderedRulesVocabulary},
			// This one does NOT discriminate the move, and saying so is the
			// point. Coverage is settled by validateScope, which already ran
			// before the identity pass, so removing the tier's comparison
			// leaves this case green — mutation-verified. It is here because
			// the cost was real (75.238 µs to 15.9 µs) and because it fails if
			// validateScope is ever moved BELOW the identities.
			{"scope coverage", func(src *predictioneval.OrderedRulesSource, _ *predictioneval.CommonAdmission) {
				src.Scope.Coverage = "NOT-A-COVERAGE"
			}, predictioneval.ErrOrderedRulesVocabulary},
		} {
			t.Run(c.name, func(t *testing.T) {
				clean, cleanAdm := orSource(cs, nil), orAdmission()
				c.mutate(&clean, &cleanAdm)
				if _, err := predictioneval.ProjectOrderedRulesStream(clean,
					cleanAdm); !errors.Is(err, c.aloneErr) {
					t.Fatalf("premise: the word alone must be refused %v; got %v", c.aloneErr, err)
				}

				both, bothAdm := orSource(duplicated, nil), orAdmission()
				c.mutate(&both, &bothAdm)
				if _, err := predictioneval.ProjectOrderedRulesStream(both,
					bothAdm); !errors.Is(err, predictioneval.ErrOrderedRulesVocabulary) {
					t.Fatalf("a source with a word outside its closed set AND a repeated candidate "+
						"identity was refused %v. The word is bounded two lines above the "+
						"comparison, so settling it costs one comparison against a short "+
						"constant; the identity pass walks every candidate and hashes each "+
						"identity. The cheap decision comes first.", err)
				}
			})
		}
	})

	// The other side of that move, and the reason it stops where it does. A
	// closed-set word is one comparison against a string already bounded, so
	// the vocabulary tier stays ahead of the identities: a source that fails it
	// must not first pay for 128 of them. This case does not discriminate the
	// move above — it holds in both arrangements — and it is here to fail if a
	// later change pushes the identity pass one tier too far.
	t.Run("a closed-set word is still refused before the candidate identities", func(t *testing.T) {
		duplicated := []predictioneval.OrderedRulesCandidate{
			orCandidate("same", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
			orCandidate("same", 20, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
		}
		duplicated[0].Balance = predictioneval.SuppliedInt64{Presence: "NOT-A-PRESENCE-WORD"}

		if _, err := predictioneval.ProjectOrderedRulesStream(orSource(duplicated, nil),
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesVocabulary) {
			t.Fatalf("a source carrying both a presence word outside the closed set and a repeated "+
				"candidate identity was refused %v. The vocabulary tier is a comparison against a "+
				"short bounded string and must stay ahead of the identity pass.", err)
		}
	})

	// The SOURCE CONTRACT, moved ahead of that same vocabulary tier one commit
	// after the two words above were. It was bounded at its length here and
	// compared only in validateScope, which runs below the candidate and
	// intervention vocabulary tiers — so a four-byte unsupported contract paid
	// for every candidate, outcome and intervention the caller sent:
	// 1.024333 ms at the widest admitted counts, 479.662 µs after. The
	// evaluator's own tier had compared it since the commit before, and this
	// side had not, which is the seventh rule on this PR found standing on one
	// of the two ingest paths and not the other.
	t.Run("an unsupported source contract is refused before the candidate vocabulary", func(t *testing.T) {
		badKind := []predictioneval.OrderedRulesCandidate{
			orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
		}
		badKind[0].SourceKind = "NOT-A-SOURCE-KIND"

		// Each fault alone, under its OWN sentinel, so the pair below is told
		// apart by which rule fired rather than by which message came back.
		if _, err := predictioneval.ProjectOrderedRulesStream(orSource(badKind, nil),
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesVocabulary) {
			t.Fatalf("premise: the candidate alone must be refused as a vocabulary fault; got %v", err)
		}
		clean := orSource(cs, nil)
		clean.Scope.SourceContractVersion = "NOPE"
		if _, err := predictioneval.ProjectOrderedRulesStream(clean,
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesScopeIncomplete) {
			t.Fatalf("premise: the contract alone must be refused as an incomplete scope; got %v", err)
		}

		both := orSource(badKind, nil)
		both.Scope.SourceContractVersion = "NOPE"
		if _, err := predictioneval.ProjectOrderedRulesStream(both,
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesScopeIncomplete) {
			t.Fatalf("a source declaring an unsupported contract AND carrying a candidate outside "+
				"its source-kind vocabulary was refused %v. The contract is bounded two lines "+
				"above its comparison, so settling it costs one comparison against a short "+
				"constant; the vocabulary tier below walks every candidate, every outcome and "+
				"every intervention. The cheap decision comes first.", err)
		}
	})

	// The evaluator compares the stream against its own digest before reading
	// the TRACE, and before scanning either retained identifier for encodability:
	// 14.049 µs rather than 582.770 ms with a 64 MiB identifier beside a small
	// stream. The config is a different case — it is normalised BEFORE the hash,
	// and the case after this one pins that — but a mismatch still attests to
	// neither, carrying no config, entropy or consumed-prefix digest.
	t.Run("a digest mismatch is decided without reading the trace", func(t *testing.T) {
		st := orProject(t, cs, nil)
		st.SelectionDigest = strings.Repeat("0", 64)

		// Both identifiers unencodable, so if either were scanned first this
		// would be refused for encodability instead.
		poisoned := orConfig([]predictioneval.OrderedRule{
			orRule(predictioneval.ComparatorLe, 40, 100, 0, 10)}, 95, 100, 0, 10)
		poisoned.ConfigID = "id-\xff-tail"
		draws := orDraws()
		draws.RunID = "id-\xff-tail"

		got := predictioneval.EvaluateOrderedRules(st, poisoned, draws)
		switch {
		case got.Reason != predictioneval.ReasonStreamDigestMismatch:
			t.Fatalf("a stream whose digest does not match, beside unencodable identifiers, was "+
				"refused %q. The mismatch is settled by the stream alone; the identifiers must "+
				"not be read to reach it.", got.Reason)
		// The recomputed digest still travels back: it is the documented
		// two-call oracle, and the other cases in this file depend on it.
		case got.StreamDigest == "":
			t.Fatal("the mismatch refusal stopped carrying the recomputed stream digest, which " +
				"the documented oracle and several cases in this file depend on")
		// And the three that would each traverse the identifiers do not.
		case got.ConfigDigest != "" || got.EntropyDigest != "" || got.ConsumedInputDigest != "":
			t.Fatal("the mismatch refusal carried a config, entropy or consumed-prefix digest. " +
				"Nothing was consumed, so each of those witnesses an input this call never read " +
				"— and computing them traverses identifiers the refusal does not depend on")
		}
	})

	// And the config's side of it, which the production comment above the
	// comparison denied until this case was written. normalizeOrderedRulesConfig
	// runs BEFORE the hash, so an unusable config beats a mismatch: the stream
	// is never hashed at all, and the refusal names the config.
	t.Run("an unusable config is decided before the stream is hashed", func(t *testing.T) {
		forged := orProject(t, cs, nil)
		forged.SelectionDigest = strings.Repeat("0", 64)

		// The declaration, not the value: a config whose bounds are all zero is
		// a perfectly usable config, so the fault has to be the missing
		// declaration or this case pins nothing.
		unusable := orConfig([]predictioneval.OrderedRule{
			orRule(predictioneval.ComparatorLe, 40, 100, 0, 10)}, 95, 100, 0, 10)
		unusable.HasDefault = false

		// The premise: each fault really is raised on its own.
		if got := predictioneval.EvaluateOrderedRules(forged, cfg,
			orDraws()); got.Reason != predictioneval.ReasonStreamDigestMismatch {
			t.Fatalf("premise: the forged digest alone must be refused as a mismatch; got %q", got.Reason)
		}
		if got := predictioneval.EvaluateOrderedRules(orProject(t, cs, nil), unusable,
			orDraws()); got.Reason != predictioneval.ReasonConfigDefaultNotSupplied {
			t.Fatalf("premise: the config alone must be refused for its missing default; got %q", got.Reason)
		}

		got := predictioneval.EvaluateOrderedRules(forged, unusable, orDraws())
		switch {
		case got.Reason != predictioneval.ReasonConfigDefaultNotSupplied:
			t.Fatalf("a forged stream beside a config declaring no default was refused %q. "+
				"Normalising the config is arithmetic over a bounded rule count; hashing the "+
				"stream traverses every retained candidate and outcome, and the run was never "+
				"going to read that stream — so the config decides first.", got.Reason)
		case got.StreamDigest != "":
			t.Fatalf("the config refusal carried stream digest %q. It was decided before the "+
				"hash ran, so it computed none and can attest to none.", got.StreamDigest)
		}
	})

	// The counterweight: an evaluation that is NOT refused early still reports
	// all four digests. Without this, deferring them could degenerate into
	// never producing them and every case above would still pass.
	t.Run("an admitted run still reports every digest", func(t *testing.T) {
		got := predictioneval.EvaluateOrderedRules(orProject(t, cs, nil), cfg, orDraws())
		switch {
		case got.Status != predictioneval.StatusWouldAttempt:
			t.Fatalf("premise: the intact fixture must evaluate; got %q", got.Status)
		case got.StreamDigest == "" || got.ConfigDigest == "" ||
			got.EntropyDigest == "" || got.ConsumedInputDigest == "":
			t.Fatal("an admitted run must still carry all four digests")
		}
	})

	// The two source COUNTS are slice lengths against constants. They now
	// precede every content scan, including validateAdmission's walk over up to
	// MaxOrderedRulesSourceReferences strings: 3.140461 ms to 220 ns on the
	// identical input.
	t.Run("a source past its candidate count is refused before its references are read", func(t *testing.T) {
		many := make([]predictioneval.OrderedRulesCandidate, predictioneval.MaxOrderedRulesCandidates+1)
		for i := range many {
			many[i] = orCandidate("c"+strconv.Itoa(i), int64(i), orMissingBalance())
		}
		// A reference fault with its own sentinel, so the two are told apart by
		// which rule fired rather than by wording.
		poisoned := orAdmission()
		poisoned.SourceReferences = []string{"ref-\xff"}

		// The premise: each fault really is raised on its own.
		if _, err := predictioneval.ProjectOrderedRulesStream(orSource(many, nil),
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesOverBound) {
			t.Fatalf("premise: the candidate count alone must be refused as over-bound; got %v", err)
		}
		if _, err := predictioneval.ProjectOrderedRulesStream(orSource(cs, nil),
			poisoned); !errors.Is(err, predictioneval.ErrOrderedRulesNotEncodable) {
			t.Fatalf("premise: the reference alone must be refused as unencodable; got %v", err)
		}

		if _, err := predictioneval.ProjectOrderedRulesStream(orSource(many, nil),
			poisoned); !errors.Is(err, predictioneval.ErrOrderedRulesOverBound) {
			t.Fatalf("a source with one candidate too many, beside an unencodable admission "+
				"reference, was refused %v. Counting a slice reads no supplied byte, and "+
				"validateAdmission walks up to %d references of %d bytes to reach the same "+
				"refusal, so the count must be decided first.",
				err, predictioneval.MaxOrderedRulesSourceReferences,
				predictioneval.MaxOrderedRulesIdentifierBytes)
		}
	})

	// The boundary also precedes the INTERVENTION payload. Detail is free text
	// no decision reads, and scanning it first meant an intervention rejectable
	// on its shape was reached only after up to 4 MiB of it: 3.2537 ms to
	// 21.686 µs on the identical input.
	t.Run("a malformed intervention is refused before intervention detail is scanned", func(t *testing.T) {
		// Entry ZERO carries the unencodable detail and entry ONE the shape
		// fault, so the two loops are told apart rather than the two entries:
		// a detail scan that ran first would reach entry zero before
		// establishCutoff ever saw entry one.
		mixed := []predictioneval.OrderedRulesIntervention{
			orIntervention("call-1", 100, predictioneval.InterventionAutoCallStarted,
				predictioneval.RelevanceProven, "detail-\xff"),
			orIntervention("call-2", 200, predictioneval.InterventionAutoCallStarted,
				predictioneval.RelevanceProven, "boundary"),
		}
		mixed[1].HasPosition = false

		detailOnly := []predictioneval.OrderedRulesIntervention{mixed[0]}
		shapeOnly := []predictioneval.OrderedRulesIntervention{
			orIntervention("call-2", 200, predictioneval.InterventionAutoCallStarted,
				predictioneval.RelevanceProven, "boundary"),
		}
		shapeOnly[0].HasPosition = false

		// The premise: each fault really is raised on its own.
		if _, err := predictioneval.ProjectOrderedRulesStream(orSource(cs, detailOnly),
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesNotEncodable) {
			t.Fatalf("premise: the detail alone must be refused as unencodable; got %v", err)
		}
		if _, err := predictioneval.ProjectOrderedRulesStream(orSource(cs, shapeOnly),
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesScopeIncomplete) {
			t.Fatalf("premise: the shape alone must be refused as incomplete; got %v", err)
		}

		if _, err := predictioneval.ProjectOrderedRulesStream(orSource(cs, mixed),
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesScopeIncomplete) {
			t.Fatalf("a source whose first intervention carries unencodable detail and whose "+
				"second declares no position was refused %v. The boundary comes from the "+
				"shape; the detail is text no decision reads, so it must not be scanned to "+
				"reach the same refusal.", err)
		}
	})

	// Every candidate's SHAPE is now settled before any candidate's PAYLOAD is
	// read. Within one candidate the cheap checks already came first; across
	// candidates they did not, so a last candidate declaring no causal position
	// was refused only after every earlier candidate's outcome identities,
	// provenance notes and presence reasons had been walked and charged:
	// 5.533517 ms to 24.191 µs on the identical input.
	t.Run("a later candidate's shape is refused before an earlier one's payload is read", func(t *testing.T) {
		// Candidate ZERO carries the payload fault and candidate ONE the shape
		// fault, so the two PASSES are told apart rather than the two
		// candidates: a single pass reaches zero's provenance long before it
		// reaches one's missing position.
		payload := orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6))
		payload.Provenance = "prov-\xff"
		shape := orCandidate("c2", 20, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6))
		shape.HasPosition = false

		// The premise: each fault really is raised on its own.
		if _, err := predictioneval.ProjectOrderedRulesStream(
			orSource([]predictioneval.OrderedRulesCandidate{payload}, nil),
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesNotEncodable) {
			t.Fatalf("premise: the provenance alone must be refused as unencodable; got %v", err)
		}
		if _, err := predictioneval.ProjectOrderedRulesStream(
			orSource([]predictioneval.OrderedRulesCandidate{shape}, nil),
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesScopeIncomplete) {
			t.Fatalf("premise: the missing position alone must be refused as incomplete; got %v", err)
		}

		if _, err := predictioneval.ProjectOrderedRulesStream(
			orSource([]predictioneval.OrderedRulesCandidate{payload, shape}, nil),
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesScopeIncomplete) {
			t.Fatalf("a source whose first candidate carries unencodable provenance and whose "+
				"second declares no causal position was refused %v. The shape of every "+
				"candidate is bounded work; the payload of one is the bulk of the admitted "+
				"budget, so no payload may be read to reach a refusal the shape already "+
				"settles.", err)
		}
	})

	// The counterweight to the split: cutting the candidate walk in two must not
	// change which sources are ADMITTED, only the order faults are reported in.
	t.Run("splitting the candidate walk admits exactly what it admitted", func(t *testing.T) {
		wide := make([]predictioneval.OrderedRulesCandidate, predictioneval.MaxOrderedRulesCandidates)
		for i := range wide {
			outs := make([]predictioneval.OrderedRulesOutcome, predictioneval.MaxOrderedRulesOutcomes)
			for j := range outs {
				outs[j] = orOutcome("o"+strconv.Itoa(j), int64(j+1))
			}
			wide[i] = orCandidate("c"+strconv.Itoa(i), int64(i), orKnownBalance(1000), outs...)
		}
		st, err := predictioneval.ProjectOrderedRulesStream(orSource(wide, nil), orAdmission())
		if err != nil {
			t.Fatalf("a source at MaxOrderedRulesCandidates x MaxOrderedRulesOutcomes was refused "+
				"%v. The vocabulary charges of every candidate are now counted before the "+
				"first payload check, so a split that moved the aggregate rather than only "+
				"its order would show up here first.", err)
		}
		if len(st.Candidates) != predictioneval.MaxOrderedRulesCandidates {
			t.Fatalf("the widest admitted source retained %d candidates, not %d",
				len(st.Candidates), predictioneval.MaxOrderedRulesCandidates)
		}
	})

	// The admission's mandatory scalars are emptiness tests and a closed-set
	// switch. They now precede the reference walk, which reads up to
	// MaxOrderedRulesSourceReferences strings of MaxOrderedRulesIdentifierBytes
	// and cannot affect that decision: 2.997708 ms to 377 ns.
	t.Run("an incomplete admission is refused before its references are read", func(t *testing.T) {
		incomplete := orAdmission()
		incomplete.Population = ""
		poisoned := orAdmission()
		poisoned.SourceReferences = []string{"ref-\xff"}
		both := incomplete
		both.SourceReferences = poisoned.SourceReferences

		// The premise: each fault really is raised on its own.
		if _, err := predictioneval.ProjectOrderedRulesStream(orSource(cs, nil),
			incomplete); !errors.Is(err, predictioneval.ErrOrderedRulesAdmissionIncomplete) {
			t.Fatalf("premise: the empty population alone must be refused as incomplete; got %v", err)
		}
		if _, err := predictioneval.ProjectOrderedRulesStream(orSource(cs, nil),
			poisoned); !errors.Is(err, predictioneval.ErrOrderedRulesNotEncodable) {
			t.Fatalf("premise: the reference alone must be refused as unencodable; got %v", err)
		}

		if _, err := predictioneval.ProjectOrderedRulesStream(orSource(cs, nil),
			both); !errors.Is(err, predictioneval.ErrOrderedRulesAdmissionIncomplete) {
			t.Fatalf("an admission naming no population, beside an unencodable source reference, was "+
				"refused %v. The population is an emptiness test and the references cannot bear "+
				"on it, so it must not cost a reference walk to reach.", err)
		}
	})

	// An IDENTITY is bytes, not shape. checkIdentifier scans it and the
	// duplicate map hashes it, so the structural checks — a declared position,
	// the causal order, the interval, the outcome count, all integer work — now
	// run over every candidate and every intervention before any identity is
	// read. This is the correction to the two-pass split, which counted an
	// identity as shape.
	t.Run("a later candidate's missing position beats an earlier identity scan", func(t *testing.T) {
		payload := orCandidate("id-\xff", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6))
		shape := orCandidate("c2", 20, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6))
		shape.HasPosition = false

		// The premise: each fault really is raised on its own.
		if _, err := predictioneval.ProjectOrderedRulesStream(
			orSource([]predictioneval.OrderedRulesCandidate{payload}, nil),
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesNotEncodable) {
			t.Fatalf("premise: the identity alone must be refused as unencodable; got %v", err)
		}
		if _, err := predictioneval.ProjectOrderedRulesStream(
			orSource([]predictioneval.OrderedRulesCandidate{shape}, nil),
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesScopeIncomplete) {
			t.Fatalf("premise: the missing position alone must be refused as incomplete; got %v", err)
		}

		if _, err := predictioneval.ProjectOrderedRulesStream(
			orSource([]predictioneval.OrderedRulesCandidate{payload, shape}, nil),
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesScopeIncomplete) {
			t.Fatalf("a source whose first candidate carries an unencodable identity and whose "+
				"second declares no position was refused %v. An identity is scanned and hashed; "+
				"a declared position is an integer test, and every candidate's must be settled "+
				"before any identity is read.", err)
		}
	})

	t.Run("a later intervention's missing position beats an earlier identity scan", func(t *testing.T) {
		payload := orIntervention("id-\xff", 100, predictioneval.InterventionAutoCallStarted,
			predictioneval.RelevanceProven, "boundary")
		shape := orIntervention("call-2", 200, predictioneval.InterventionAutoCallStarted,
			predictioneval.RelevanceProven, "boundary")
		shape.HasPosition = false

		// The premise: each fault really is raised on its own.
		if _, err := predictioneval.ProjectOrderedRulesStream(orSource(cs,
			[]predictioneval.OrderedRulesIntervention{payload}),
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesNotEncodable) {
			t.Fatalf("premise: the identity alone must be refused as unencodable; got %v", err)
		}
		if _, err := predictioneval.ProjectOrderedRulesStream(orSource(cs,
			[]predictioneval.OrderedRulesIntervention{shape}),
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesScopeIncomplete) {
			t.Fatalf("premise: the missing position alone must be refused as incomplete; got %v", err)
		}

		if _, err := predictioneval.ProjectOrderedRulesStream(orSource(cs,
			[]predictioneval.OrderedRulesIntervention{payload, shape}),
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesScopeIncomplete) {
			t.Fatalf("a source whose first intervention carries an unencodable identity and whose "+
				"second declares no position was refused %v. The boundary pass has the same "+
				"two-tier shape as the candidate walk, and 1,023 identities must not be scanned "+
				"to reach an integer test.", err)
		}
	})

	// The NESTED declarations are structural too. A KNOWN value that omits the
	// position it became available at is refused by a flag test, and it used to
	// sit beside the text scans: 20.07073 ms to 250.373 µs, the latter matching
	// the same refusal with one-byte provenance, so it is no longer scaled by
	// the payload at all.
	t.Run("a later candidate's undeclared availability beats an earlier text scan", func(t *testing.T) {
		payload := orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6))
		payload.Provenance = "prov-\xff"
		shape := orCandidate("c2", 20, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6))
		shape.Balance.HasAvailableAtPosition = false

		// The premise: each fault really is raised on its own.
		if _, err := predictioneval.ProjectOrderedRulesStream(
			orSource([]predictioneval.OrderedRulesCandidate{payload}, nil),
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesNotEncodable) {
			t.Fatalf("premise: the provenance alone must be refused as unencodable; got %v", err)
		}
		if _, err := predictioneval.ProjectOrderedRulesStream(
			orSource([]predictioneval.OrderedRulesCandidate{shape}, nil),
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesScopeIncomplete) {
			t.Fatalf("premise: the undeclared availability alone must be refused as incomplete; got %v", err)
		}

		if _, err := predictioneval.ProjectOrderedRulesStream(
			orSource([]predictioneval.OrderedRulesCandidate{payload, shape}, nil),
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesScopeIncomplete) {
			t.Fatalf("a source whose first candidate carries unencodable provenance and whose "+
				"second declares a KNOWN balance without its availability position was refused "+
				"%v. That declaration is a flag test; it must not wait on the text of every "+
				"candidate before it.", err)
		}
	})

	// The aggregate is a sum of chargedWidth over len(), so it is computable
	// without reading a byte — and computing it in the structural pass is what
	// makes that pass a boundary rather than one more step: an over-ceiling
	// source is refused for its SIZE before anything is scanned at all.
	t.Run("an over-ceiling source is refused before any text is scanned", func(t *testing.T) {
		// Just past orderedRulesTextCeiling once charged at the worst-case
		// encoded width, and spread so that NO SINGLE STRING is over its own
		// MaxOrderedRulesIdentifierBytes bound — otherwise the per-string rule
		// refuses it first under the same sentinel and this case pins nothing.
		// The first draft did exactly that and passed against the old order.
		//
		// One shared 2,700-byte string across 128 x 64 outcomes: Go strings are
		// immutable and shared, so this is one allocation, and the charge is
		// 128*64*2700*6 bytes, comfortably past the ceiling.
		const per = 2700
		note := strings.Repeat("p", per)
		wide := make([]predictioneval.OrderedRulesCandidate, predictioneval.MaxOrderedRulesCandidates)
		for i := range wide {
			outs := make([]predictioneval.OrderedRulesOutcome, predictioneval.MaxOrderedRulesOutcomes)
			for j := range outs {
				outs[j] = orOutcome("o"+strconv.Itoa(j), int64(j+1))
				outs[j].Points.Provenance = note
			}
			wide[i] = orCandidate("c"+strconv.Itoa(i), int64(i), orMissingBalance(), outs...)
		}
		// A text fault with its own sentinel, on the FIRST candidate, so a scan
		// that ran first would reach it before the aggregate was known.
		wide[0].OutcomesReason = "reason-\xff"

		// The premise: the text fault really is raised on its own.
		lean := []predictioneval.OrderedRulesCandidate{
			orCandidate("c0", 0, orMissingBalance()),
		}
		lean[0].OutcomesReason = "reason-\xff"
		if _, err := predictioneval.ProjectOrderedRulesStream(orSource(lean, nil),
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesNotEncodable) {
			t.Fatalf("premise: the reason alone must be refused as unencodable; got %v", err)
		}

		if _, err := predictioneval.ProjectOrderedRulesStream(orSource(wide, nil),
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesOverBound) {
			t.Fatalf("a source past the aggregate ceiling, whose first candidate also carries an "+
				"unencodable reason, was refused %v. The ceiling is arithmetic over lengths: "+
				"nothing needs to be read to know the source cannot fit, so it must not be "+
				"scanned to find out.", err)
		}
	})

	// The terminating case for the whole axis: the constant-bounded tier is
	// complete and runs before any CALLER-SCALED read anywhere in the
	// projection. A candidate declaring no position is settled without the
	// admission's references being scanned — 3.062608 ms to 951 ns — and the
	// same holds for the scope's own text, which is why this asserts across
	// both. The tier does compare presence words against a five-byte constant;
	// what it never does is read something whose length the caller chose.
	t.Run("no caller-scaled text is read before the whole structure is settled", func(t *testing.T) {
		shape := []predictioneval.OrderedRulesCandidate{
			orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
			orCandidate("c2", 20, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
		}
		shape[1].HasPosition = false

		refs := orAdmission()
		refs.SourceReferences = []string{"ref-\xff"}

		scopeText := orSource(shape, nil)
		scopeText.Scope.CoverageDetail = "detail-\xff"

		// The premise: each text fault really is raised on its own.
		if _, err := predictioneval.ProjectOrderedRulesStream(orSource(cs, nil),
			refs); !errors.Is(err, predictioneval.ErrOrderedRulesNotEncodable) {
			t.Fatalf("premise: the reference alone must be refused as unencodable; got %v", err)
		}
		clean := orSource(cs, nil)
		clean.Scope.CoverageDetail = "detail-\xff"
		if _, err := predictioneval.ProjectOrderedRulesStream(clean,
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesNotEncodable) {
			t.Fatalf("premise: the coverage detail alone must be refused as unencodable; got %v", err)
		}

		for _, c := range []struct {
			name      string
			source    predictioneval.OrderedRulesSource
			admission predictioneval.CommonAdmission
		}{
			{"admission references", orSource(shape, nil), refs},
			{"scope text", scopeText, orAdmission()},
		} {
			t.Run(c.name, func(t *testing.T) {
				if _, err := predictioneval.ProjectOrderedRulesStream(c.source,
					c.admission); !errors.Is(err, predictioneval.ErrOrderedRulesScopeIncomplete) {
					t.Fatalf("a candidate declaring no causal position, beside unencodable text "+
						"elsewhere in the source, was refused %v. A declared position is a flag "+
						"test; the constant-bounded tier is complete before it, so no text whose "+
						"length the caller chose may be scanned to reach it.", err)
				}
			})
		}
	})

	// An identity's PRESENCE is an emptiness test quoting nothing, so it belongs
	// in the constant-bounded tier even though its encoding and uniqueness do not.
	// 20.344244 ms to 488.369 µs, the latter matching the same refusal with
	// one-byte provenance.
	t.Run("a later empty outcome identity beats an earlier text scan", func(t *testing.T) {
		payload := orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6))
		payload.Provenance = "prov-\xff"
		empty := orCandidate("c2", 20, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6))
		empty.Outcomes[1].Identity = ""

		// The premise: each fault really is raised on its own.
		if _, err := predictioneval.ProjectOrderedRulesStream(
			orSource([]predictioneval.OrderedRulesCandidate{payload}, nil),
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesNotEncodable) {
			t.Fatalf("premise: the provenance alone must be refused as unencodable; got %v", err)
		}
		if _, err := predictioneval.ProjectOrderedRulesStream(
			orSource([]predictioneval.OrderedRulesCandidate{empty}, nil),
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesScopeIncomplete) {
			t.Fatalf("premise: the empty identity alone must be refused as incomplete; got %v", err)
		}

		if _, err := predictioneval.ProjectOrderedRulesStream(
			orSource([]predictioneval.OrderedRulesCandidate{payload, empty}, nil),
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesScopeIncomplete) {
			t.Fatalf("a source whose first candidate carries unencodable provenance and whose "+
				"second holds an outcome with an empty identity was refused %v. Emptiness is a "+
				"length test quoting nothing; only the ENCODING and the uniqueness of an "+
				"identity need bytes read.", err)
		}
	})

	// A candidate's vocabulary is four closed-set fields, length-bounded first
	// because their refusals quote the value. They are settled before the scope
	// and admission text and before the interventions, none of which bear on
	// them: 10.093146 ms to 49.724 µs.
	t.Run("candidate vocabulary is settled before unrelated source text", func(t *testing.T) {
		bad := []predictioneval.OrderedRulesCandidate{
			orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
		}
		bad[0].SourceKind = "NOT-A-SOURCE-KIND"

		poisoned := orAdmission()
		poisoned.SourceReferences = []string{"ref-\xff"}

		// The premise: each fault really is raised on its own.
		if _, err := predictioneval.ProjectOrderedRulesStream(orSource(bad, nil),
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesVocabulary) {
			t.Fatalf("premise: the source kind alone must be refused for its vocabulary; got %v", err)
		}
		if _, err := predictioneval.ProjectOrderedRulesStream(orSource(cs, nil),
			poisoned); !errors.Is(err, predictioneval.ErrOrderedRulesNotEncodable) {
			t.Fatalf("premise: the reference alone must be refused as unencodable; got %v", err)
		}

		if _, err := predictioneval.ProjectOrderedRulesStream(orSource(bad, nil),
			poisoned); !errors.Is(err, predictioneval.ErrOrderedRulesVocabulary) {
			t.Fatalf("a candidate whose source kind is outside its vocabulary, beside an "+
				"unencodable admission reference, was refused %v. Four short closed-set fields "+
				"do not depend on the admission's references, the scope's text or the "+
				"interventions, and must not wait behind them.", err)
		}
	})

	// The NESTED presence words are closed sets too, and leaving the outcomes
	// out of the vocabulary tier meant a last outcome's invalid word waited on
	// every earlier candidate's payload text.
	t.Run("a later outcome's presence word beats an earlier payload scan", func(t *testing.T) {
		payload := orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6))
		payload.Provenance = "prov-\xff"
		voc := orCandidate("c2", 20, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6))
		voc.Outcomes[1].Points.Presence = "NOT-A-PRESENCE"

		// The premise: each fault really is raised on its own.
		if _, err := predictioneval.ProjectOrderedRulesStream(
			orSource([]predictioneval.OrderedRulesCandidate{payload}, nil),
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesNotEncodable) {
			t.Fatalf("premise: the provenance alone must be refused as unencodable; got %v", err)
		}
		if _, err := predictioneval.ProjectOrderedRulesStream(
			orSource([]predictioneval.OrderedRulesCandidate{voc}, nil),
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesVocabulary) {
			t.Fatalf("premise: the presence word alone must be refused for its vocabulary; got %v", err)
		}

		if _, err := predictioneval.ProjectOrderedRulesStream(
			orSource([]predictioneval.OrderedRulesCandidate{payload, voc}, nil),
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesVocabulary) {
			t.Fatalf("a source whose first candidate carries unencodable provenance and whose "+
				"second holds an outcome with a presence word outside its set was refused %v. "+
				"A presence word is short and closed; it must not wait behind payload text.", err)
		}
	})

	// An intervention's kind and relevance are closed sets that depend on
	// neither the admission's references nor any identity.
	t.Run("intervention vocabulary is settled before unrelated source text", func(t *testing.T) {
		bad := []predictioneval.OrderedRulesIntervention{
			orIntervention("call-1", 100, "NOT-A-KIND", predictioneval.RelevanceProven, "boundary"),
		}
		poisoned := orAdmission()
		poisoned.SourceReferences = []string{"ref-\xff"}

		// The premise: each fault really is raised on its own.
		if _, err := predictioneval.ProjectOrderedRulesStream(orSource(cs, bad),
			orAdmission()); !errors.Is(err, predictioneval.ErrOrderedRulesVocabulary) {
			t.Fatalf("premise: the kind alone must be refused for its vocabulary; got %v", err)
		}
		if _, err := predictioneval.ProjectOrderedRulesStream(orSource(cs, nil),
			poisoned); !errors.Is(err, predictioneval.ErrOrderedRulesNotEncodable) {
			t.Fatalf("premise: the reference alone must be refused as unencodable; got %v", err)
		}

		if _, err := predictioneval.ProjectOrderedRulesStream(orSource(cs, bad),
			poisoned); !errors.Is(err, predictioneval.ErrOrderedRulesVocabulary) {
			t.Fatalf("an intervention whose kind is outside its vocabulary, beside an unencodable "+
				"admission reference, was refused %v. Two short closed-set fields do not depend "+
				"on the references or on any identity, and must not wait behind them.", err)
		}
	})

	// An unusable config is arithmetic over a rule count already bounded above.
	// The invariant pass is bounded but not cheap: on a well-formed stream it
	// walks the scope, the admission references and every retained candidate to
	// say so. 7.26947 ms to 4.054034 ms on the identical input, the residue
	// being the stream digest the comparison itself required.
	t.Run("an unusable config is refused before the stream invariants are re-walked", func(t *testing.T) {
		unusable := orConfig(nil, 95, 100, 0, 10)
		unusable.HasDefault = false

		// A stream fault with its own reason code, reached through the oracle
		// so that it survives the digest comparison and gets as far as the
		// invariant pass.
		forge := func(t *testing.T) predictioneval.OrderedRulesStream {
			t.Helper()
			st := orProject(t, cs, nil)
			st.Qualifications = append(st.Qualifications, "FABRICATED")
			st.SelectionDigest = predictioneval.EvaluateOrderedRules(st, cfg, orDraws()).StreamDigest
			return st
		}

		// The premise: each fault really is raised on its own.
		if got := predictioneval.EvaluateOrderedRules(forge(t), cfg,
			orDraws()); got.Reason != predictioneval.ReasonStreamInvariantViolated {
			t.Fatalf("premise: the forged stream alone must be refused for its invariants; got %q",
				got.Reason)
		}
		if got := predictioneval.EvaluateOrderedRules(orProject(t, cs, nil), unusable,
			orDraws()); got.Reason != predictioneval.ReasonConfigDefaultNotSupplied {
			t.Fatalf("premise: the config alone must be refused for its missing default; got %q",
				got.Reason)
		}

		if got := predictioneval.EvaluateOrderedRules(forge(t), unusable,
			orDraws()); got.Reason != predictioneval.ReasonConfigDefaultNotSupplied {
			t.Fatalf("a forged stream beside a config with neither rules nor a default was "+
				"refused %q. Normalising the config is arithmetic over a bounded rule count; "+
				"re-establishing the stream's invariants walks the scope, the references and "+
				"every retained candidate. The run was never going to read the stream, so the "+
				"config decides first.", got.Reason)
		}
	})
}

// TestOrderedRulesAMismatchRefusalReExportsNothingItDidNotRead pins the one
// field a digest mismatch is entitled to carry.
//
// The cutoff and the qualifications are DERIVED fields: the projection computes
// both, and every consumer reads them as facts about a stream that was
// produced, not supplied. A digest mismatch says this stream is not what the
// projection produced — so at that point both are caller text, and the two
// checks that would judge them, orderedRulesCutoffImpossible and
// orderedRulesQualificationsNotDerived, have not run and will not.
//
// Carrying them out was a re-export of unread input in the exact sense the
// shape gate declines: a limitation list the evidence never implied and a
// boundary no intervention established, handed back inside a typed result.
// Measured at the bound the gate admits, 64 qualifications of
// MaxOrderedRulesIdentifierBytes each, that was 262,144 bytes copied and then
// written again by whoever encodes the refusal.
//
// The recomputed digest is the deliberate exception and the counterweight below
// is what keeps this from degenerating: an ADMITTED evaluation must still
// report the genuine cutoff and qualifications, or "carry nothing" would pass
// this case by never producing them at all.
func TestOrderedRulesAMismatchRefusalReExportsNothingItDidNotRead(t *testing.T) {
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 40, 100, 0, 10)}, 95, 100, 0, 10)

	t.Run("a mismatch carries the recomputed digest and nothing else", func(t *testing.T) {
		// A GENUINELY projected stream with a real boundary, so its cutoff and
		// its qualifications are derived rather than invented. That matters
		// more than it used to: an invented cutoff is now refused by the
		// structural tier before the digest is computed at all, so a probe
		// built that way would never reach the comparison this case is about.
		st := orProject(t, []predictioneval.OrderedRulesCandidate{
			orCandidate("c1", 1, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
			orCandidate("c2", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
		}, []predictioneval.OrderedRulesIntervention{
			orIntervention("call-1", 5, predictioneval.InterventionAutoCallStarted,
				predictioneval.RelevanceProven, "a real call")})
		if !st.Cutoff.Established || st.Cutoff.DroppedAtOrAfter == 0 || len(st.Qualifications) == 0 {
			t.Fatalf("premise: the fixture must project a boundary, a removal and qualifications; "+
				"got %+v / %v", st.Cutoff, st.Qualifications)
		}
		st.SelectionDigest = strings.Repeat("0", 64)

		got := predictioneval.EvaluateOrderedRules(st, cfg, orDraws())
		switch {
		case got.Reason != predictioneval.ReasonStreamDigestMismatch:
			t.Fatalf("premise: the probe must be refused for the digest; got %q", got.Reason)
		case got.StreamDigest == "":
			t.Fatal("the mismatch refusal stopped carrying the recomputed stream digest, which " +
				"the documented oracle and several cases in this file depend on")
		case len(got.Qualifications) != 0:
			t.Fatalf("a digest mismatch handed back %d qualifications. A mismatch means this is not "+
				"the stream the projection produced, so they are caller text — the derivation "+
				"check runs below the mismatch and never sees them — and re-exporting them puts "+
				"a limitation list the evidence never implied inside a derived field.",
				len(got.Qualifications))
		case got.Cutoff != (predictioneval.OrderedRulesCutoff{}):
			t.Fatalf("a digest mismatch handed back cutoff %+v. No intervention established it and "+
				"orderedRulesCutoffImpossible never ran, so it is supplied text wearing a "+
				"derived field's name.", got.Cutoff)
		}
	})

	// THE COUNTERWEIGHT. Without it, a change that simply stopped producing
	// either field would satisfy the case above.
	t.Run("an admitted run still reports the derived cutoff and qualifications", func(t *testing.T) {
		ins := []predictioneval.OrderedRulesIntervention{
			orIntervention("i1", 40, predictioneval.InterventionAutoCallStarted,
				predictioneval.RelevanceAmbiguous, "ambiguous call"),
		}
		cut := orProject(t, []predictioneval.OrderedRulesCandidate{
			orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
			orCandidate("c2", 50, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
		}, ins)
		if !cut.Cutoff.Established || len(cut.Qualifications) == 0 {
			t.Fatalf("premise: the fixture must project a boundary and qualifications; got %+v / %v",
				cut.Cutoff, cut.Qualifications)
		}

		got := predictioneval.EvaluateOrderedRules(cut, cfg, orDraws())
		switch {
		case got.Reason == predictioneval.ReasonStreamDigestMismatch:
			t.Fatalf("premise: the projected stream must pass its own digest; got %q", got.Reason)
		case got.Cutoff != cut.Cutoff:
			t.Fatalf("an evaluation that READ the stream reported cutoff %+v, not the projected "+
				"%+v. Refusing to re-export an unread cutoff must not turn into never "+
				"reporting a derived one.", got.Cutoff, cut.Cutoff)
		case len(got.Qualifications) != len(cut.Qualifications):
			t.Fatalf("an evaluation that READ the stream reported %d qualifications, not the "+
				"projected %d", len(got.Qualifications), len(cut.Qualifications))
		}
	})
}

// TestOrderedRulesARefusalDecidedBeforeTheTraversalAttestsToNothing covers the
// four refusals that are settled after the stream verifies but before a single
// candidate is read.
//
// Each of them used to carry all four digests, and three of those four witness
// inputs the call never traversed: ConfigDigest hashes the whole config,
// EntropyDigest hashes every draw word, and ConsumedInputDigest binds an EMPTY
// prefix to WHICH config and WHICH run produced it, which traverses ConfigID
// and RunID in full. Those two identifiers ride the config-and-trace ceiling
// and carry no per-string bound, so the attestations were bought at the
// caller's price for a run that read nothing: measured with a broken stream
// invariant beside a 64 MiB ConfigID, 200.890391 ms against 4.109 µs now, the
// latter being the same figure as the identical refusal with a SHORT identifier
// — which is how one knows the identifier is no longer read at all.
//
// This is the shape gate's own rule applied one layer in: a function that
// declined to read an input can attest to nothing about it. What these
// refusals still carry is the stream digest they genuinely computed.
func TestOrderedRulesARefusalDecidedBeforeTheTraversalAttestsToNothing(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{
		orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
	}
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 40, 100, 0, 10)}, 95, 100, 0, 10)

	// forge reaches the invariant pass through the two-call digest oracle.
	forge := func(t *testing.T) predictioneval.OrderedRulesStream {
		t.Helper()
		st := orProject(t, cs, nil)
		st.Qualifications = append(st.Qualifications, "FABRICATED")
		st.SelectionDigest = predictioneval.EvaluateOrderedRules(st, cfg, orDraws()).StreamDigest
		return st
	}

	tooManyRules := orConfig(make([]predictioneval.OrderedRule, predictioneval.MaxOrderedRulesRules+1),
		95, 100, 0, 10)
	noDefault := orConfig(nil, 95, 100, 0, 10)
	noDefault.HasDefault = false

	// A projected stream carrying ONE short fault outside a closed set, carried
	// through the oracle so it is not refused as a MISMATCH instead — which is
	// a different rule under a different reason and would pin nothing about
	// these. Each of the four was settled only by the invariant pass, after the
	// whole stream had been hashed.
	shortFault := func(t *testing.T, mutate func(*predictioneval.OrderedRulesStream)) predictioneval.OrderedRulesStream {
		t.Helper()
		st := orProject(t, cs, nil)
		mutate(&st)
		st.SelectionDigest = predictioneval.EvaluateOrderedRules(st, cfg, orDraws()).StreamDigest
		return st
	}
	// Like shortFault, but the fixture ESTABLISHES a boundary, because a cutoff
	// that is not established must carry no identity at all — mutating one on a
	// stream without an intervention trips the structural rule instead and pins
	// nothing about the scan. The first draft of the encodability row below did
	// exactly that and failed.
	boundedFault := func(t *testing.T, mutate func(*predictioneval.OrderedRulesStream)) predictioneval.OrderedRulesStream {
		t.Helper()
		st := orProject(t, cs, []predictioneval.OrderedRulesIntervention{
			orIntervention("call-1", 20, predictioneval.InterventionAutoCallStarted,
				predictioneval.RelevanceProven, "boundary")})
		if !st.Cutoff.Established {
			t.Fatal("premise: this fixture must establish a boundary")
		}
		mutate(&st)
		st.SelectionDigest = predictioneval.EvaluateOrderedRules(st, cfg, orDraws()).StreamDigest
		return st
	}
	// Two candidates, because cs above holds one and a duplicate needs a pair.
	duplicateIdentity := func(t *testing.T) predictioneval.OrderedRulesStream {
		t.Helper()
		st := orProject(t, []predictioneval.OrderedRulesCandidate{
			orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
			orCandidate("c2", 20, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
		}, nil)
		st.Candidates[1].Identity = st.Candidates[0].Identity
		st.SelectionDigest = predictioneval.EvaluateOrderedRules(st, cfg, orDraws()).StreamDigest
		return st
	}

	// One candidate whose LAST outcome repeats its first outcome's identity.
	// cs above already carries a candidate with two outcomes, so the fault
	// needs no extra fixture — only a rename that the projection would have
	// refused, carried in through the oracle.
	duplicateOutcomeIdentity := func(t *testing.T) predictioneval.OrderedRulesStream {
		t.Helper()
		return shortFault(t, func(st *predictioneval.OrderedRulesStream) {
			last := &st.Candidates[len(st.Candidates)-1]
			last.Outcomes[len(last.Outcomes)-1].Identity = last.Outcomes[0].Identity
		})
	}

	badPresence := func(t *testing.T) predictioneval.OrderedRulesStream {
		t.Helper()
		return shortFault(t, func(st *predictioneval.OrderedRulesStream) {
			last := &st.Candidates[len(st.Candidates)-1]
			last.Outcomes[len(last.Outcomes)-1].Points.Presence = "BOGUS"
		})
	}

	for _, c := range []struct {
		name   string
		stream predictioneval.OrderedRulesStream
		cfg    predictioneval.OrderedRulesConfig
		words  int
		reason string
		digest bool
	}{
		// DRAW_WORDS_OVER_BOUND and RULE_COUNT_OVER_BOUND are deliberately not
		// in this table. The evaluator has cases for both, but the shape gate
		// tests the same two conditions against the same two constants and
		// answers first, so those cases are unreachable and the gate's own
		// refusal — which carries nothing whatever, not even a stream digest —
		// is what a caller sees. The case below pins that instead, and this
		// table would have asserted a contract that does not exist: the first
		// draft of it did, and failed.
		// CONFIG_DEFAULT_NOT_SUPPLIED is decided BEFORE the stream digest — an
		// unusable config is refused whatever stream came with it — so it
		// carries nothing at all, and `digest` says so.
		{"config carries no default", orProject(t, cs, nil), noDefault, 0,
			predictioneval.ReasonConfigDefaultNotSupplied, false},
		// A fabricated qualification is now settled ABOVE the digest too — the
		// derivation check runs after the config is normalised and before the
		// hash, NOT inside orderedRulesStreamStructureBroken — so this row
		// carries nothing either. It used to be the table's only post-digest
		// row; the unencodable cutoff identity below is what plays that part.
		{"stream invariants broken", forge(t), cfg, 0,
			predictioneval.ReasonStreamInvariantViolated, false},
		// The SAME reason code, decided in a different tier, and that is why
		// both rows are here. A presence word outside the closed set is a
		// constant-size fault that checkPresenceShape cannot see — it returns
		// immediately for every non-KNOWN value — so it used to reach the
		// invariant pass with the whole stream already hashed: 3.555366 ms on
		// 128 candidates holding 4 KiB each, against 7.092 µs once the
		// structural tier compared the word. A caller cannot tell the two rows
		// apart by their reason, only by what the refusal carries.
		{"stream carries a presence word outside the closed set", badPresence(t), cfg, 0,
			predictioneval.ReasonStreamInvariantViolated, false},
		// The same defect in the three other vocabularies the pre-digest tier
		// had not been taught. Each is one comparison against a short constant,
		// so none of them can be made expensive by a longer supplied word.
		{"stream declares a coverage outside the closed set", shortFault(t,
			func(st *predictioneval.OrderedRulesStream) { st.Scope.Coverage = "NOT-A-COVERAGE" }),
			cfg, 0, predictioneval.ReasonStreamInvariantViolated, false},
		{"stream declares a view kind outside the closed set", shortFault(t,
			func(st *predictioneval.OrderedRulesStream) { st.Admission.ViewKind = "NOT-A-VIEW-KIND" }),
			cfg, 0, predictioneval.ReasonStreamInvariantViolated, false},
		{"stream declares a cutoff basis outside the closed set", shortFault(t,
			func(st *predictioneval.OrderedRulesStream) { st.Cutoff.Basis = "NOT-A-BASIS" }),
			cfg, 0, predictioneval.ReasonStreamInvariantViolated, false},
		// The SOURCE CONTRACT, settled above the digest by a comparison against
		// one short constant. validateScope settles it too, but only after the
		// hash, and its refusal quotes the supplied value — which is why the
		// tier compares rather than validates. A four-byte unsupported version
		// beside 128 candidates holding 4 KiB each cost 2.314278 ms; 4.457 µs
		// now.
		{"stream declares an unsupported source contract", shortFault(t,
			func(st *predictioneval.OrderedRulesStream) { st.Scope.SourceContractVersion = "NOPE" }),
			cfg, 0, predictioneval.ReasonStreamInvariantViolated, false},
		// Two candidates sharing an identity, settled above the digest by the
		// bounded identity tier. ProjectOrderedRulesStream already refused this
		// before its own payload scans; EvaluateOrderedRules is a SEPARATE
		// ingest and did not, so a stream handed straight in was hashed whole
		// first — 3.549091 ms on 128 candidates holding 4 KiB each, 31.909 µs
		// now. That tier is bounded, not free, which is why it is its own and
		// not part of the constant-bounded one.
		{"stream repeats a candidate identity", duplicateIdentity(t),
			cfg, 0, predictioneval.ReasonStreamInvariantViolated, false},
		// The SAME defect one level down, and the sixth time on this PR that a
		// rule was found on one side of a pair and not the other. The candidate
		// row above was repaired while per-candidate OUTCOME uniqueness stayed
		// where it had always been, in the invariant pass below the hash — so a
		// forged stream repeating two SHORT outcome identities inside its last
		// candidate still bought the whole-stream digest first: 35.97074 ms on
		// 128 candidates each holding 64 outcomes with 2,400-byte provenance,
		// against 69.413 µs for a constant-bounded fault on the identical
		// payload. Fixing one path is not fixing the class; this row is the
		// class's other half.
		{"stream repeats an outcome identity inside one candidate", duplicateOutcomeIdentity(t),
			cfg, 0, predictioneval.ReasonStreamInvariantViolated, false},
		// And the one part of the boundary invariant that is NOT in the
		// constant-bounded tier, pinned as digest=true for exactly that reason. The
		// cutoff identity's emptiness and its length are settled before the
		// hash; its UTF-8 VALIDITY is a scan of a string the caller chose, so
		// it stays in the invariant pass. An earlier version of this package
		// hoisted the whole predicate and claimed it read no supplied text —
		// it did, here. If this scan is ever moved back into the structural
		// tier, this row stops finding a digest and fails.
		{"stream carries a cutoff identity that is not encodable", boundedFault(t,
			func(st *predictioneval.OrderedRulesStream) { st.Cutoff.Identity = "call-\xff-1" }),
			cfg, 0, predictioneval.ReasonStreamInvariantViolated, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := predictioneval.EvaluateOrderedRules(c.stream, c.cfg,
				orDraws(make([]uint64, c.words)...))
			switch {
			case got.Status != predictioneval.StatusRefused || got.Reason != c.reason:
				t.Fatalf("premise: this input must be refused %q; got %q / %q",
					c.reason, got.Status, got.Reason)
			case c.digest && got.StreamDigest == "":
				t.Fatal("a refusal reached past the digest comparison stopped carrying the " +
					"stream digest it verified to get there")
			case !c.digest && got.StreamDigest != "":
				t.Fatalf("a refusal decided BEFORE the digest carried one anyway (%q). It computed "+
					"no digest, so it can attest to none.", got.StreamDigest)
			case got.ConfigDigest != "", got.EntropyDigest != "":
				t.Fatalf("a refusal decided before the traversal carried a config or entropy "+
					"digest (config %q, entropy %q). Each hashes a whole input this call "+
					"never read — the config's rules, every draw word — and ConfigID rides a "+
					"ceiling with no per-string bound, so the attestation is bought at the "+
					"caller's price and witnesses nothing.", got.ConfigDigest, got.EntropyDigest)
			case got.ConsumedInputDigest != "":
				t.Fatalf("a refusal decided before the traversal carried a consumed-prefix "+
					"digest %q over a prefix of NOTHING. It binds the empty prefix to which "+
					"config and which run produced it, traversing ConfigID and RunID in full "+
					"to distinguish cases that have nothing to recombine.", got.ConsumedInputDigest)
			}
		})
	}

	// The DERIVED fields are the other half of what these refusals may carry,
	// and the repair to the mismatch path stopped one layer short of them.
	//
	// Cutoff and Qualifications are computed by ProjectOrderedRulesStream, and
	// a matching digest does not say the projection computed THESE — it says
	// the stream has not changed since whoever built it. The digest is unkeyed
	// and a refusal hands the recomputed value back, so the two-call oracle
	// reaches both refusals below carrying any boundary the caller likes.
	t.Run("a pre-traversal refusal carries no derived cutoff or qualifications", func(t *testing.T) {
		// A GENUINELY projected boundary, so the cutoff and the qualifications
		// this refusal must not hand back are real rather than invented. An
		// invented cutoff would be refused by the structural tier before either
		// refusal below is reached, which is why the first draft of this case
		// built one and stopped testing what it names.
		st := orProject(t, []predictioneval.OrderedRulesCandidate{
			orCandidate("c1", 1, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
			orCandidate("c2", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
		}, []predictioneval.OrderedRulesIntervention{
			orIntervention("call-1", 5, predictioneval.InterventionAutoCallStarted,
				predictioneval.RelevanceProven, "a real call")})
		if !st.Cutoff.Established || len(st.Qualifications) == 0 {
			t.Fatalf("premise: the fixture must project a boundary and qualifications; got %+v / %v",
				st.Cutoff, st.Qualifications)
		}
		// One fabricated qualification, which the DERIVATION check refuses. That
		// check sits ABOVE the digest and BELOW the config, so this one stream
		// reaches both routes: with a usable config it is refused for its
		// qualifications, and with an unusable one the config answers first.
		// Neither refusal may hand back the boundary or the qualifications the
		// projection really did derive, which is what the fixture above exists
		// to make real rather than invented.
		st.Qualifications = append(st.Qualifications, "FABRICATED")
		st.SelectionDigest = predictioneval.EvaluateOrderedRules(st, cfg, orDraws()).StreamDigest

		for _, c := range []struct {
			name   string
			cfg    predictioneval.OrderedRulesConfig
			reason string
		}{
			{"invariants broken", cfg, predictioneval.ReasonStreamInvariantViolated},
			{"config unusable", noDefault, predictioneval.ReasonConfigDefaultNotSupplied},
		} {
			t.Run(c.name, func(t *testing.T) {
				got := predictioneval.EvaluateOrderedRules(st, c.cfg, orDraws())
				switch {
				case got.Reason != c.reason:
					t.Fatalf("premise: this input must be refused %q; got %q", c.reason, got.Reason)
				case got.Cutoff != (predictioneval.OrderedRulesCutoff{}):
					t.Fatalf("a refusal decided before the traversal handed back cutoff %+v. It is "+
						"a DERIVED field and nothing here has established that this stream was "+
						"derived — a matching digest says only that it has not CHANGED — so it "+
						"must not be populated until the invariant pass has passed.", got.Cutoff)
				case len(got.Qualifications) != 0:
					t.Fatalf("a refusal decided before the traversal handed back qualifications %v "+
						"— a limitation list the evidence never implied, in a field whose "+
						"contract is that it was derived.", got.Qualifications)
				}
			})
		}
	})

	// The two over-bound reasons never reach the block above: the shape gate
	// settles both, and a gate refusal declined to read the input, so it
	// attests to nothing at all — not even the stream digest, which it never
	// computed.
	t.Run("the over-bound reasons are settled by the gate and carry nothing", func(t *testing.T) {
		for _, c := range []struct {
			name   string
			cfg    predictioneval.OrderedRulesConfig
			words  int
			reason string
		}{
			{"draw words", cfg, predictioneval.MaxOrderedRulesDrawWords + 1,
				predictioneval.ReasonDrawWordsOverBound},
			{"rule count", tooManyRules, 0, predictioneval.ReasonRuleCountOverBound},
		} {
			t.Run(c.name, func(t *testing.T) {
				got := predictioneval.EvaluateOrderedRules(orProject(t, cs, nil), c.cfg,
					orDraws(make([]uint64, c.words)...))
				switch {
				case got.Reason != c.reason:
					t.Fatalf("premise: this input must be refused %q; got %q", c.reason, got.Reason)
				case got.StreamDigest != "" || got.ConfigDigest != "" ||
					got.EntropyDigest != "" || got.ConsumedInputDigest != "":
					t.Fatalf("a shape-gate refusal carried a digest (stream %q, config %q, "+
						"entropy %q, consumed %q). The gate declined to READ the input; a "+
						"digest of something never traversed is the empty guarantee it "+
						"exists to avoid.", got.StreamDigest, got.ConfigDigest,
						got.EntropyDigest, got.ConsumedInputDigest)
				}
			})
		}
	})

	// The encodability scan guards the two digests above, so it belongs beside
	// them rather than ahead of the four refusals — none of which reads either
	// identifier.
	t.Run("a broken invariant is decided without scanning the identifiers", func(t *testing.T) {
		poisoned := orConfig([]predictioneval.OrderedRule{
			orRule(predictioneval.ComparatorLe, 40, 100, 0, 10)}, 95, 100, 0, 10)
		poisoned.ConfigID = "id-\xff-tail"

		// The premise: the unencodable identifier really is refused on its own.
		if got := predictioneval.EvaluateOrderedRules(orProject(t, cs, nil), poisoned,
			orDraws()); got.Reason != predictioneval.ReasonSuppliedTextNotEncodable {
			t.Fatalf("premise: the identifier alone must be refused as unencodable; got %q",
				got.Reason)
		}

		if got := predictioneval.EvaluateOrderedRules(forge(t), poisoned,
			orDraws()); got.Reason != predictioneval.ReasonStreamInvariantViolated {
			t.Fatalf("a forged stream beside an unencodable config identifier was refused %q. "+
				"The encodability rule exists because the two digests hash those strings; a "+
				"refusal that computes neither must not scan them to be reached.", got.Reason)
		}
	})

	// THE COUNTERWEIGHT. Without it, never producing the three digests at all
	// would satisfy every case above.
	t.Run("an admitted run still attests to all four", func(t *testing.T) {
		got := predictioneval.EvaluateOrderedRules(orProject(t, cs, nil), cfg, orDraws())
		switch {
		case got.Status != predictioneval.StatusWouldAttempt:
			t.Fatalf("premise: the intact fixture must evaluate; got %q", got.Status)
		case got.StreamDigest == "" || got.ConfigDigest == "" ||
			got.EntropyDigest == "" || got.ConsumedInputDigest == "":
			t.Fatal("an evaluation that READ its inputs must still carry all four digests. " +
				"Withholding them from a refusal must not turn into never computing them.")
		}
	})
}
