package p4offline_test

// APPROVED DENOMINATOR SEMANTICS (owner reconciliation, TASK 4), pinned at
// the JSON wire form so the expectation is independent of the Go field set.
//
//   - POLICY_CHOICE_ACCURACY: the denominator is, per policy and run, the
//     resolved WOULD_ATTEMPT decisions. A POLICY_SKIP and a P3b
//     NO_ATTEMPT_IN_SUPPLIED_PREFIX are not attempted choices and are never
//     silently counted wrong; a zero denominator is N/A; membership is
//     visible.
//   - PLACED_BET_WIN_RATE: the denominator is only the platform-proven
//     accepted placements that settled WIN or LOSE (this package admits no
//     other acceptance basis). A mere WOULD_ATTEMPT is not sufficient;
//     REFUND, POLICY_SKIP, NO_ATTEMPT and UNKNOWN are excluded.
//
// The payout artifact states the payout seam's half of each condition; the
// case's half (PRIMARY_SCORABLE) is re-derived from the dataset, and the two
// meet only in AssessDenominatorMembership.

import (
	"encoding/json"
	"errors"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval/p4offline"
)

// wireBool reads one boolean key from an artifact's JSON form; a missing or
// non-boolean key is a failure of the artifact to state membership at all.
func wireBool(t *testing.T, artifact any, key string) bool {
	t.Helper()
	raw, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	v, ok := m[key]
	if !ok {
		t.Fatalf("the artifact states no %q membership: %s", key, raw)
	}
	var b bool
	if err := json.Unmarshal(v, &b); err != nil {
		t.Fatalf("%q is not a boolean: %s", key, v)
	}
	return b
}

// wireHasKey reports whether the artifact's JSON carries key. Both
// conversions must succeed: a negative assertion over a nil map would pass
// without proving anything.
func wireHasKey(t *testing.T, artifact any, key string) bool {
	t.Helper()
	raw, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("the artifact is not a JSON object: %v: %s", err, raw)
	}
	_, ok := m[key]
	return ok
}

// TestPlacedBetDenominatorCountsOnlyProvenWinOrLose pins the PLACED_BET_WIN_RATE
// denominator on the payout artifact: a proven placement that settled WIN or
// LOSE is a member; an unproven WOULD_ATTEMPT, a REFUND, a POLICY_SKIP and a
// NO_ATTEMPT_IN_SUPPLIED_PREFIX are not.
func TestPlacedBetDenominatorCountsOnlyProvenWinOrLose(t *testing.T) {
	_, fs, fp, p2 := factualCase(t, coherentCall)
	dec := decisionOf(t, p2, fs)
	proven := p4offline.DerivePlacement(dec, fp, validProof(fp))
	unproven := p4offline.DerivePlacement(dec, fp, nil)
	if proven.Status != p4offline.PlacementAcceptedPlatformProven || unproven.Status != p4offline.PlacementLocalOKPlatformUnproven {
		t.Fatalf("fixture: %+v / %+v", proven.Status, unproven.Status)
	}
	// The placement artifact must not carry a denominator claim of its own:
	// "would attempt" is not "placed bet".
	if wireHasKey(t, proven, "contributesToBetOnlyDenominator") {
		t.Errorf("the placement artifact still carries the provisional contributesToBetOnlyDenominator flag")
	}
	win := p4offline.DerivePayout(dec, proven, winnerArtifact("o1"), linkedRecord(proven, p4offline.KnownInt64(120), p4offline.UnknownInt64("n/a")))
	lose := p4offline.DerivePayout(dec, proven, winnerArtifact("o2"), nil)
	refund := p4offline.DerivePayout(dec, proven, refundArtifact(), linkedRecord(proven, p4offline.UnknownInt64("n/a"), p4offline.KnownInt64(50)))
	unprovenPayout := p4offline.DerivePayout(dec, unproven, winnerArtifact("o1"), nil)
	if win.Outcome != p4offline.PayoutWin || lose.Outcome != p4offline.PayoutLose || refund.Outcome != p4offline.PayoutRefund ||
		unprovenPayout.Outcome != p4offline.PayoutUnknown {
		t.Fatalf("fixture: %s %s %s %s", win.Outcome, lose.Outcome, refund.Outcome, unprovenPayout.Outcome)
	}
	if !wireBool(t, win, "placedBetDenominatorMember") {
		t.Errorf("a proven WIN is in the placed-bet denominator")
	}
	if !wireBool(t, lose, "placedBetDenominatorMember") {
		t.Errorf("a proven LOSE is in the placed-bet denominator")
	}
	if wireBool(t, refund, "placedBetDenominatorMember") {
		t.Errorf("a REFUND is excluded from the placed-bet denominator")
	}
	if wireBool(t, unprovenPayout, "placedBetDenominatorMember") {
		t.Errorf("a WOULD_ATTEMPT whose placement is not platform-proven is not a placed bet")
	}
	_, fsS, fpS, p2S := skippedCase(t)
	skip := decisionOf(t, p2S, fsS)
	skipPayout := p4offline.DerivePayout(skip, p4offline.DerivePlacement(skip, fpS, nil), winnerArtifact("o1"), nil)
	if skipPayout.Outcome != p4offline.PayoutNotApplicable || wireBool(t, skipPayout, "placedBetDenominatorMember") {
		t.Errorf("a POLICY_SKIP is excluded from the placed-bet denominator: %+v", skipPayout)
	}
	rs := mustVerify(t, rulesetFrom(t, cfgDefaultOnly("none", 95, 100)))
	p3b, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, 0))
	if err != nil {
		t.Fatal(err)
	}
	none := decisionOf(t, p3b, fs)
	nonePayout := p4offline.DerivePayout(none, p4offline.DerivePlacement(none, fp, nil), winnerArtifact("o1"), nil)
	if none.Action.Class != p4offline.ActionNoAttemptInSuppliedPrefix || wireBool(t, nonePayout, "placedBetDenominatorMember") {
		t.Errorf("a NO_ATTEMPT_IN_SUPPLIED_PREFIX is excluded from the placed-bet denominator: %+v", nonePayout)
	}
}

// TestPrimaryDenominatorCountsResolvedWouldAttemptOnly pins the
// POLICY_CHOICE_ACCURACY denominator on the payout artifact: a resolved
// WOULD_ATTEMPT is a member whether or not any bet was placed; a skip, a
// no-attempt prefix, an unresolved round and a refund are not; and a
// non-member is never a CORRECT or INCORRECT verdict.
func TestPrimaryDenominatorCountsResolvedWouldAttemptOnly(t *testing.T) {
	_, fs, fp, p2 := factualCase(t, coherentCall)
	dec := decisionOf(t, p2, fs)
	unproven := p4offline.DerivePlacement(dec, fp, nil)
	resolved := p4offline.DerivePayout(dec, unproven, winnerArtifact("o2"), nil)
	if resolved.ChoiceCorrect != p4offline.ChoiceIncorrect || resolved.Outcome != p4offline.PayoutUnknown {
		t.Fatalf("fixture: %+v", resolved)
	}
	if !wireBool(t, resolved, "primaryDenominatorMember") {
		t.Errorf("a resolved WOULD_ATTEMPT is in the primary denominator even with no bet placed: %+v", resolved)
	}
	unresolved := p4offline.DerivePayout(dec, unproven,
		p4offline.ResolutionNotRecorded(p4offline.PublicRoundIdentity{EventID: "e1"}, []string{"o1", "o2"}, nil, "p"), nil)
	if wireBool(t, unresolved, "primaryDenominatorMember") || unresolved.ChoiceCorrect != p4offline.ChoiceUnknown {
		t.Errorf("an unresolved round is not in the primary denominator: %+v", unresolved)
	}
	refund := p4offline.DerivePayout(dec, unproven, refundArtifact(), nil)
	if wireBool(t, refund, "primaryDenominatorMember") || refund.ChoiceCorrect != p4offline.ChoiceNotApplicable {
		t.Errorf("a refund names no winner: %+v", refund)
	}
	_, fsS, fpS, p2S := skippedCase(t)
	skip := decisionOf(t, p2S, fsS)
	skipPayout := p4offline.DerivePayout(skip, p4offline.DerivePlacement(skip, fpS, nil), winnerArtifact("o1"), nil)
	if wireBool(t, skipPayout, "primaryDenominatorMember") || skipPayout.ChoiceCorrect != p4offline.ChoiceNotApplicable {
		t.Errorf("a POLICY_SKIP is not an attempted choice and is not counted wrong: %+v", skipPayout)
	}
	rs := mustVerify(t, rulesetFrom(t, cfgDefaultOnly("none", 95, 100)))
	p3b, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, 0))
	if err != nil {
		t.Fatal(err)
	}
	none := decisionOf(t, p3b, fs)
	nonePayout := p4offline.DerivePayout(none, p4offline.DerivePlacement(none, fp, nil), winnerArtifact("o1"), nil)
	if wireBool(t, nonePayout, "primaryDenominatorMember") || nonePayout.ChoiceCorrect != p4offline.ChoiceNotApplicable {
		t.Errorf("a NO_ATTEMPT_IN_SUPPLIED_PREFIX is not an attempted choice and is not counted wrong: %+v", nonePayout)
	}
	// PARTICIPATION_ADMITTED_STAKE_UNKNOWN is a choice without a stake: its
	// verdict is visible, its membership is not that of a WOULD_ATTEMPT.
	dsU, _, fsU := selectedCase(t, func(e *predictioneval.SourceDecisionEnvelope) { e.Balance = ptrI64(4294967296) }, nil)
	fpU, err := p4offline.ProjectFactualPlacement(preparedDS(dsU), fsU)
	if err != nil {
		t.Fatal(err)
	}
	one := mustVerify(t, rulesetFrom(t, cfgWithRule("one", predictioneval.ComparatorGe, 50, 100)))
	p3bU, err := p4offline.EvaluateP3bCase(fsU, one, synthCoords(fsU, 0))
	if err != nil {
		t.Fatal(err)
	}
	unknown := decisionOf(t, p3bU, fsU)
	if unknown.Action.Class != p4offline.ActionParticipationAdmittedStakeUnknown {
		t.Fatalf("fixture: %+v", unknown.Action)
	}
	unknownPayout := p4offline.DerivePayout(unknown, p4offline.DerivePlacement(unknown, fpU, nil), winnerArtifact("o1"), nil)
	if wireBool(t, unknownPayout, "primaryDenominatorMember") || unknownPayout.ChoiceCorrect != p4offline.ChoiceCorrect {
		t.Errorf("an admitted participation without a stake is described, not counted as a WOULD_ATTEMPT: %+v", unknownPayout)
	}
}

// TestResolvedSkipPlusAttemptCaseIsPrimaryScorable pins the case-level
// consequence: a complete, resolved case in which P2 skipped and P3b
// attempted is PRIMARY_SCORABLE — the P3b attempt enters P3b's denominator,
// the P2 skip is a visible non-member — rather than the whole case being
// dropped to DESCRIPTIVE_ONLY for the skip.
func TestResolvedSkipPlusAttemptCaseIsPrimaryScorable(t *testing.T) {
	dsS, fsS, _, p2S := skippedCase(t)
	skip := decisionOf(t, p2S, fsS)
	if skip.Action.Class != p4offline.ActionPolicySkip {
		t.Fatalf("fixture: %+v", skip.Action)
	}
	rs := mustVerify(t, rulesetFrom(t, cfgWithRule("one", predictioneval.ComparatorGe, 50, 100)))
	p3b, err := p4offline.EvaluateP3bCase(fsS, rs, synthCoords(fsS, 0))
	if err != nil {
		t.Fatal(err)
	}
	attempt := decisionOf(t, p3b, fsS)
	if attempt.Action.Class != p4offline.ActionWouldAttempt {
		t.Fatalf("fixture: %+v", attempt.Action)
	}
	q := p4offline.AssessCaseQuality(preparedDS(dsS), fsS, skip, attempt, winnerArtifact("o1"))
	if q.Quality != p4offline.QualityPrimaryScorable {
		t.Fatalf("a resolved case with a P2 skip and a P3b attempt is PRIMARY_SCORABLE (the skip is a per-policy non-member, not a case exclusion): %+v", q)
	}
	// And the same case with both policies bet on nothing is still admitted
	// evidence — described with two empty denominators, not excluded.
	none := mustVerify(t, rulesetFrom(t, cfgDefaultOnly("none", 95, 100)))
	p3bNone, err := p4offline.EvaluateP3bCase(fsS, none, synthCoords(fsS, 0))
	if err != nil {
		t.Fatal(err)
	}
	q = p4offline.AssessCaseQuality(preparedDS(dsS), fsS, skip, decisionOf(t, p3bNone, fsS), winnerArtifact("o1"))
	if q.Quality != p4offline.QualityPrimaryScorable {
		t.Fatalf("membership is per policy; the case itself stays scorable: %+v", q)
	}
}

// TestDenominatorMembershipIsComposedWithTheCaseQuality pins the composition:
// a payout-seam member counts only on a PRIMARY_SCORABLE case, the case
// quality is re-derived from the dataset (a factset the dataset does not
// derive is EXCLUDED however good its payout looks), the pairing is the
// caller's and is RECORDED (a case one policy cannot be evaluated on counts
// for neither, and the verdict names the counterpart it was judged beside),
// and only DerivePayout's own evidence, bound to the case, the policy, the
// very decision it settled and the resolution, is read at all.
func TestDenominatorMembershipIsComposedWithTheCaseQuality(t *testing.T) {
	ds, fs, fp, p2 := factualCase(t, coherentCall)
	p2dec := decisionOf(t, p2, fs)
	rs := mustVerify(t, rulesetFrom(t, cfgWithRule("one", predictioneval.ComparatorGe, 50, 100)))
	p3b, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, 0))
	if err != nil {
		t.Fatal(err)
	}
	p3bdec := decisionOf(t, p3b, fs)
	res := winnerArtifact("o1")
	proven := p4offline.DerivePlacement(p2dec, fp, validProof(fp))
	p2win := p4offline.DerivePayout(p2dec, proven, res, linkedRecord(proven, p4offline.KnownInt64(120), p4offline.UnknownInt64("n/a")))
	p3bPayout := p4offline.DerivePayout(p3bdec, p4offline.DerivePlacement(p3bdec, fp, nil), res, nil)
	if !p2win.PrimaryDenominatorMember || !p2win.PlacedBetDenominatorMember || !p3bPayout.PrimaryDenominatorMember || p3bPayout.PlacedBetDenominatorMember {
		t.Fatalf("fixture: %+v / %+v", p2win, p3bPayout)
	}

	t.Run("a scorable case counts its payout-seam members", func(t *testing.T) {
		m := p4offline.AssessDenominatorMembership(preparedDS(ds), prepared(t, registryOf(ds, fs)), fs, p2dec, p3bdec, res, p2win)
		if m.Policy != p4offline.PolicyP2 || m.Quality != p4offline.QualityPrimaryScorable || !m.Primary || !m.PlacedBet || len(m.Reasons) != 0 {
			t.Fatalf("%+v", m)
		}
		if m.Derivation != p2dec.Derivation || m.CounterpartDerivation != p3bdec.Derivation || m.Derivation == "" || m.CounterpartDerivation == "" {
			t.Fatalf("the verdict must record the pairing: %+v", m)
		}
		// The verdict is visible at the wire: a false membership is on the
		// wire too, and the pairing travels with it.
		for _, key := range []string{"primary", "placedBet"} {
			if !wireBool(t, m, key) {
				t.Fatalf("%s: %+v", key, m)
			}
		}
		if !wireHasKey(t, m, "quality") || !wireHasKey(t, m, "policy") || !wireHasKey(t, m, "derivation") || !wireHasKey(t, m, "counterpartDerivation") {
			t.Fatalf("the verdict's identity is not on the wire: %+v", m)
		}
		m = p4offline.AssessDenominatorMembership(preparedDS(ds), prepared(t, registryOf(ds, fs)), fs, p2dec, p3bdec, res, p3bPayout)
		if m.Policy != p4offline.PolicyP3b || !m.Primary || m.PlacedBet || wireBool(t, m, "placedBet") {
			t.Fatalf("a counterfactual attempt is a primary member and not a placed bet: %+v", m)
		}
		if m.Derivation != p3bdec.Derivation || m.CounterpartDerivation != p2dec.Derivation {
			t.Fatalf("%+v", m)
		}
		if !strings.Contains(p3bdec.Derivation, strconv.Quote(p3b.Trace.RunID)) || !strings.Contains(p3bdec.Derivation, rs.NativeConfigDigest) ||
			!strings.Contains(p3bdec.Derivation, rs.RawSHA256) || !strings.Contains(p2dec.Derivation, p2.Binding.Digest) {
			t.Fatalf("derivations name the ruleset, the run and the binding: %q / %q", p3bdec.Derivation, p2dec.Derivation)
		}
	})
	t.Run("a skip on a scorable case is a visible non-member", func(t *testing.T) {
		dsS, fsS, fpS, p2S := skippedCase(t)
		skip := decisionOf(t, p2S, fsS)
		p3bS, err := p4offline.EvaluateP3bCase(fsS, rs, synthCoords(fsS, 0))
		if err != nil {
			t.Fatal(err)
		}
		attempt := decisionOf(t, p3bS, fsS)
		skipPayout := p4offline.DerivePayout(skip, p4offline.DerivePlacement(skip, fpS, nil), res, nil)
		m := p4offline.AssessDenominatorMembership(preparedDS(dsS), prepared(t, registryOf(dsS, fsS)), fsS, skip, attempt, res, skipPayout)
		if m.Quality != p4offline.QualityPrimaryScorable || m.Primary || m.PlacedBet {
			t.Fatalf("the case is scorable, the skip counts for nothing and is never counted wrong: %+v", m)
		}
		// A non-member is visibly a non-member on the wire: both false
		// memberships are present, never omitted.
		if wireBool(t, m, "primary") || wireBool(t, m, "placedBet") {
			t.Fatalf("%+v", m)
		}
		attemptPayout := p4offline.DerivePayout(attempt, p4offline.DerivePlacement(attempt, fpS, nil), res, nil)
		if m := p4offline.AssessDenominatorMembership(preparedDS(dsS), prepared(t, registryOf(dsS, fsS)), fsS, skip, attempt, res, attemptPayout); !m.Primary || m.PlacedBet {
			t.Fatalf("the other policy's attempt on the same case counts: %+v", m)
		}
	})
	t.Run("a payout-seam member on a case the dataset does not derive counts for nothing", func(t *testing.T) {
		// The same shapes in another dataset: a genuine, witnessed, fully
		// consistent case — that this dataset never contained.
		dsOther, _, other := selectedCase(t, func(e *predictioneval.SourceDecisionEnvelope) { e.Balance = ptrI64(2000) }, nil)
		otherP2, err := p4offline.EvaluateP2Case(other)
		if err != nil {
			t.Fatal(err)
		}
		otherDec := decisionOf(t, otherP2, other)
		otherFp, err := p4offline.ProjectFactualPlacement(preparedDS(dsOther), other)
		if err != nil {
			t.Fatal(err)
		}
		otherPayout := p4offline.DerivePayout(otherDec, p4offline.DerivePlacement(otherDec, otherFp, nil), res, nil)
		if !otherPayout.PrimaryDenominatorMember {
			t.Fatalf("fixture: the payout seam alone cannot know the case is foreign: %+v", otherPayout)
		}
		m := p4offline.AssessDenominatorMembership(preparedDS(ds), prepared(t, registryOf(ds, other)), other, otherDec, asP3bAttempt(otherDec), res, otherPayout)
		if m.Quality != p4offline.QualityExcluded || m.Primary || m.PlacedBet || m.CounterpartDerivation != "" ||
			!containsString(m.Reasons, "FACTSET_BINDING_MISMATCH") || !containsString(m.Reasons, "CASE_NOT_PRIMARY_SCORABLE") {
			t.Fatalf("%+v", m)
		}
	})
	t.Run("the pairing is the caller's and is recorded", func(t *testing.T) {
		// P2 edited: the case is EXCLUDED, so P3b's genuine member counts for
		// nothing either.
		edited := p2dec
		edited.Stake = p4offline.KnownInt64(51)
		m := p4offline.AssessDenominatorMembership(preparedDS(ds), prepared(t, registryOf(ds, fs)), fs, edited, p3bdec, res, p3bPayout)
		if m.Quality != p4offline.QualityExcluded || m.Primary || !containsString(m.Reasons, "CASE_NOT_PRIMARY_SCORABLE") ||
			m.CounterpartDerivation != "" {
			t.Fatalf("an edited (unusable) counterpart is never echoed as the pairing: %+v", m)
		}
		// An edited derivation is an edited decision, and the evidence
		// derived from the unedited one is not this relabelled decision's:
		// no derivation is recorded for it.
		relabelled := p3bdec
		relabelled.Derivation = "P3B:ruleset=\"other\""
		if m := p4offline.AssessDenominatorMembership(preparedDS(ds), prepared(t, registryOf(ds, fs)), fs, p2dec, relabelled, res, p3bPayout); m.Quality != p4offline.QualityExcluded ||
			!containsString(m.Reasons, "P3B_DECISION_NOT_DERIVED") || !containsString(m.Reasons, "PAYOUT_DECISION_MISMATCH") ||
			m.Derivation != "" {
			t.Fatalf("%+v", m)
		}
		// An unusable counterpart is named by the quality reasons and never
		// echoed as a pairing.
		bogus := p4offline.PolicyDecision{Policy: p4offline.PolicyP3b, Derivation: "P3B:ruleset=\"nobody\""}
		if m := p4offline.AssessDenominatorMembership(preparedDS(ds), prepared(t, registryOf(ds, fs)), fs, p2dec, bogus, res, p2win); m.Primary || m.PlacedBet ||
			m.CounterpartDerivation != "" || m.Derivation != p2dec.Derivation || m.Quality != p4offline.QualityExcluded {
			t.Fatalf("%+v", m)
		}
		// A genuine counterpart of ANOTHER case is foreign here: named by
		// POLICY_BINDING_MISMATCH, never echoed as this case's pairing.
		_, otherFs := selectedFactset(t, func(e *predictioneval.SourceDecisionEnvelope) { e.Balance = ptrI64(2000) }, nil)
		p3bOtherCase, err := p4offline.EvaluateP3bCase(otherFs, rs, synthCoords(otherFs, 0))
		if err != nil {
			t.Fatal(err)
		}
		foreign := decisionOf(t, p3bOtherCase, otherFs)
		if m := p4offline.AssessDenominatorMembership(preparedDS(ds), prepared(t, registryOf(ds, fs)), fs, p2dec, foreign, res, p2win); m.Primary || m.PlacedBet ||
			m.CounterpartDerivation != "" || !containsString(m.Reasons, "POLICY_BINDING_MISMATCH") {
			t.Fatalf("%+v", m)
		}
		// This case's own P2 decision in the P3b slot is not a counterpart
		// either: the two policies are positional.
		if m := p4offline.AssessDenominatorMembership(preparedDS(ds), prepared(t, registryOf(ds, fs)), fs, p2dec, p2dec, res, p2win); m.Primary || m.PlacedBet ||
			m.CounterpartDerivation != "" || m.Quality != p4offline.QualityExcluded {
			t.Fatalf("%+v", m)
		}
		// An edited P2 derivation is refused by the decision's witness AND
		// by the P2 re-derivation, and both say so.
		relabelledP2 := p2dec
		relabelledP2.Derivation = "P2:binding=other"
		if m := p4offline.AssessDenominatorMembership(preparedDS(ds), prepared(t, registryOf(ds, fs)), fs, relabelledP2, p3bdec, res, p3bPayout); m.Primary ||
			!containsString(m.Reasons, "POLICY_DECISION_NOT_DERIVED") || !containsString(m.Reasons, "P2_DECISION_NOT_DERIVED") ||
			m.CounterpartDerivation != "" {
			t.Fatalf("a relabelled counterpart's string never reaches the wire: %+v", m)
		}
		// P3b not determinate (an exhausted trace is UNKNOWN_INPUT): the case
		// is DESCRIPTIVE_ONLY, and P2's proven WIN is described, not counted;
		// the verdict names the counterpart it was judged beside.
		half := mustVerify(t, rulesetFrom(t, cfgWithRule("half", predictioneval.ComparatorGe, 50, 50)))
		empty := mustDrawTrace(t, synthCoords(fs, 0), 0)
		p3bU, err := p4offline.EvaluateP3bWithTrace(fs, half, synthCoords(fs, 0), empty)
		if err != nil {
			t.Fatal(err)
		}
		udec := decisionOf(t, p3bU, fs)
		if udec.Action.Class != p4offline.ActionUnknownInput || udec.Derivation == p3bdec.Derivation {
			t.Fatalf("fixture: %+v %q", udec.Action, udec.Derivation)
		}
		m = p4offline.AssessDenominatorMembership(preparedDS(ds), prepared(t, registryOf(ds, fs)), fs, p2dec, udec, res, p2win)
		if m.Quality != p4offline.QualityDescriptiveOnly || m.Primary || m.PlacedBet ||
			!containsString(m.Reasons, "P3B_NOT_DETERMINATE:UNKNOWN_INPUT") || !containsString(m.Reasons, "CASE_NOT_PRIMARY_SCORABLE") ||
			m.CounterpartDerivation != udec.Derivation {
			t.Fatalf("%+v", m)
		}
		// A payout derived from one genuine P3b decision is not judged as
		// another genuine P3b decision's on the same case.
		if m := p4offline.AssessDenominatorMembership(preparedDS(ds), prepared(t, registryOf(ds, fs)), fs, p2dec, udec, res, p3bPayout); m.Primary || m.PlacedBet ||
			!containsString(m.Reasons, "PAYOUT_DECISION_MISMATCH") || m.Derivation != "" {
			t.Fatalf("the evidence was derived from p3bdec, not from udec: %+v", m)
		}
		other := mustVerify(t, rulesetFrom(t, cfgWithRule("other-one", predictioneval.ComparatorGe, 50, 100)))
		p3bOther, err := p4offline.EvaluateP3bCase(fs, other, synthCoords(fs, 0))
		if err != nil {
			t.Fatal(err)
		}
		odec := decisionOf(t, p3bOther, fs)
		if m := p4offline.AssessDenominatorMembership(preparedDS(ds), prepared(t, registryOf(ds, fs)), fs, p2dec, odec, res, p3bPayout); m.Primary ||
			!containsString(m.Reasons, "PAYOUT_DECISION_MISMATCH") {
			t.Fatalf("another ruleset's decision on the same case: %+v", m)
		}
		if m := p4offline.AssessDenominatorMembership(preparedDS(ds), prepared(t, registryOf(ds, fs)), fs, p2dec, odec, res,
			p4offline.DerivePayout(odec, p4offline.DerivePlacement(odec, fp, nil), res, nil)); !m.Primary || m.Derivation != odec.Derivation {
			t.Fatalf("its own evidence counts: %+v", m)
		}
	})
	t.Run("only DerivePayout's own evidence is read", func(t *testing.T) {
		for name, edit := range map[string]func(*p4offline.PayoutEvidence){
			"placed-bet membership edited off": func(p *p4offline.PayoutEvidence) { p.PlacedBetDenominatorMember = false },
			"primary membership edited off":    func(p *p4offline.PayoutEvidence) { p.PrimaryDenominatorMember = false },
			"outcome":                          func(p *p4offline.PayoutEvidence) { p.Outcome = p4offline.PayoutLose },
			"verdict":                          func(p *p4offline.PayoutEvidence) { p.ChoiceCorrect = p4offline.ChoiceIncorrect },
			"resolution digest": func(p *p4offline.PayoutEvidence) {
				p.ResolutionFactsDigest = winnerArtifact("o2").ResolutionFactsDigest
			},
			"derivation": func(p *p4offline.PayoutEvidence) { p.Derivation = p3bdec.Derivation },
			"net":        func(p *p4offline.PayoutEvidence) { p.Net = p4offline.KnownInt64(71) },
			"stake":      func(p *p4offline.PayoutEvidence) { p.Stake = p4offline.KnownInt64(49) },
			"payout":     func(p *p4offline.PayoutEvidence) { p.Payout = p4offline.KnownInt64(121) },
			"contract":   func(p *p4offline.PayoutEvidence) { p.ContractVersion = "p4-payout-evidence-only/v2" },
			"attempt":    func(p *p4offline.PayoutEvidence) { p.Attempt.AttemptID = 2 },
			"factset":    func(p *p4offline.PayoutEvidence) { p.FactsetDigest = p3bdec.FactsetDigest + "0" },
			"event":      func(p *p4offline.PayoutEvidence) { p.EventID = "e2" },
			"reasons":    func(p *p4offline.PayoutEvidence) { p.Reasons = append(p.Reasons, "EXTRA") },
		} {
			edited := p2win
			edit(&edited)
			if m := p4offline.AssessDenominatorMembership(preparedDS(ds), prepared(t, registryOf(ds, fs)), fs, p2dec, p3bdec, res, edited); m.Primary || m.PlacedBet ||
				!containsString(m.Reasons, "PAYOUT_NOT_DERIVED") {
				t.Fatalf("%s: an edited payout evidence settles nothing: %+v", name, m)
			}
		}
		promoted := p3bPayout
		promoted.PlacedBetDenominatorMember = true
		if m := p4offline.AssessDenominatorMembership(preparedDS(ds), prepared(t, registryOf(ds, fs)), fs, p2dec, p3bdec, res, promoted); m.Primary || m.PlacedBet ||
			!containsString(m.Reasons, "PAYOUT_NOT_DERIVED") {
			t.Fatalf("a membership edited in is refused whole: %+v", m)
		}
		// The genuine POLICY_SKIP evidence of a scorable case with its primary
		// membership edited in: refused whole, never a member.
		dsS, fsS, fpS, p2S := skippedCase(t)
		skip := decisionOf(t, p2S, fsS)
		skipPromoted := p4offline.DerivePayout(skip, p4offline.DerivePlacement(skip, fpS, nil), res, nil)
		skipPromoted.PrimaryDenominatorMember = true
		p3bS, err := p4offline.EvaluateP3bCase(fsS, rs, synthCoords(fsS, 0))
		if err != nil {
			t.Fatal(err)
		}
		if m := p4offline.AssessDenominatorMembership(preparedDS(dsS), prepared(t, registryOf(dsS, fsS)), fsS, skip, decisionOf(t, p3bS, fsS), res, skipPromoted); m.Primary ||
			!containsString(m.Reasons, "PAYOUT_NOT_DERIVED") {
			t.Fatalf("a skip promoted by hand is not a member: %+v", m)
		}
		var decoded p4offline.PayoutEvidence
		raw := mustMarshal(t, p2win)
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if m := p4offline.AssessDenominatorMembership(preparedDS(ds), prepared(t, registryOf(ds, fs)), fs, p2dec, p3bdec, res, decoded); m.Primary || m.PlacedBet ||
			!containsString(m.Reasons, "PAYOUT_NOT_DERIVED") {
			t.Fatalf("a payout evidence read back from storage must be re-derived, not trusted: %+v", m)
		}
		handBuilt := p4offline.PayoutEvidence{ContractVersion: p4offline.PayoutEvidenceVersion, Policy: p4offline.PolicyP2,
			Attempt: fs.Attempt, FactsetDigest: fs.Digest, EventID: "e1", Outcome: p4offline.PayoutWin,
			ChoiceCorrect: p4offline.ChoiceCorrect, PrimaryDenominatorMember: true, PlacedBetDenominatorMember: true,
			Stake: p4offline.KnownInt64(50), Payout: p4offline.KnownInt64(120), Net: p4offline.KnownInt64(70),
			ResolutionFactsDigest: res.ResolutionFactsDigest}
		if m := p4offline.AssessDenominatorMembership(preparedDS(ds), prepared(t, registryOf(ds, fs)), fs, p2dec, p3bdec, res, handBuilt); m.Primary || m.PlacedBet ||
			!containsString(m.Reasons, "PAYOUT_NOT_DERIVED") {
			t.Fatalf("%+v", m)
		}
	})
	t.Run("the payout evidence must be this case's, this policy's and this resolution's", func(t *testing.T) {
		// Another case's genuine payout evidence.
		_, fsS, fpS, p2S := skippedCase(t)
		skip := decisionOf(t, p2S, fsS)
		skipPayout := p4offline.DerivePayout(skip, p4offline.DerivePlacement(skip, fpS, nil), res, nil)
		if m := p4offline.AssessDenominatorMembership(preparedDS(ds), prepared(t, registryOf(ds, fs)), fs, p2dec, p3bdec, res, skipPayout); m.Primary || m.PlacedBet ||
			!containsString(m.Reasons, "PAYOUT_BINDING_MISMATCH") {
			t.Fatalf("%+v", m)
		}
		// This case's P2 evidence handed in beside a P2 slot holding another
		// attempt's decision.
		foreignP2 := p2dec
		foreignP2.Attempt.AttemptID = 2
		if m := p4offline.AssessDenominatorMembership(preparedDS(ds), prepared(t, registryOf(ds, fs)), fs, foreignP2, p3bdec, res, p2win); m.Primary || m.PlacedBet ||
			!containsString(m.Reasons, "PAYOUT_BINDING_MISMATCH") {
			t.Fatalf("%+v", m)
		}
		// The evidence settled against another resolution than the one the
		// case is assessed with.
		otherRes := winnerArtifact("o2")
		lose := p4offline.DerivePayout(p2dec, proven, otherRes, nil)
		if lose.Outcome != p4offline.PayoutLose {
			t.Fatalf("fixture: %+v", lose)
		}
		if m := p4offline.AssessDenominatorMembership(preparedDS(ds), prepared(t, registryOf(ds, fs)), fs, p2dec, p3bdec, res, lose); m.Primary || m.PlacedBet ||
			!containsString(m.Reasons, "PAYOUT_RESOLUTION_MISMATCH") {
			t.Fatalf("%+v", m)
		}
		if m := p4offline.AssessDenominatorMembership(preparedDS(ds), prepared(t, registryOf(ds, fs)), fs, p2dec, p3bdec, otherRes, lose); !m.Primary || !m.PlacedBet {
			t.Fatalf("assessed with its own resolution, a proven LOSE counts: %+v", m)
		}
		unknownPolicy := p2win
		unknownPolicy.Policy = "P9"
		if m := p4offline.AssessDenominatorMembership(preparedDS(ds), prepared(t, registryOf(ds, fs)), fs, p2dec, p3bdec, res, unknownPolicy); m.Primary ||
			!containsString(m.Reasons, "PAYOUT_NOT_DERIVED") || m.Policy != "" || m.Derivation != "" {
			t.Fatalf("a refused verdict echoes no unverified identity: %+v", m)
		}
		// Genuine evidence that was refused before it read the resolution is
		// no member and is not bound to any resolution.
		foreignPlacement := p4offline.DerivePlacement(p3bdec, fp, nil)
		refused := p4offline.DerivePayout(p2dec, foreignPlacement, res, nil)
		if refused.ResolutionFactsDigest != "" || !containsString(refused.Reasons, "POLICY_BINDING_MISMATCH") {
			t.Fatalf("fixture: %+v", refused)
		}
		if m := p4offline.AssessDenominatorMembership(preparedDS(ds), prepared(t, registryOf(ds, fs)), fs, p2dec, p3bdec, res, refused); m.Primary || m.PlacedBet ||
			!containsString(m.Reasons, "PAYOUT_RESOLUTION_UNBOUND") || m.Derivation != p2dec.Derivation {
			t.Fatalf("%+v", m)
		}
	})
}

// TestDenominatorMembershipRequiresTheCanonicalSourceRound pins seam 3 at
// the only place a decision is counted: the case must be the ONE canonical
// claim of its public round in the registry the caller reconciled. A round
// in CONFLICT across datasets, a round the registry never reconciled, a
// registry that does not re-derive from its own claims, and a canonical
// claim swapped in by hand all count nothing — however scorable a single
// dataset's evidence looks on its own.
func TestDenominatorMembershipRequiresTheCanonicalSourceRound(t *testing.T) {
	ds, fs, fp, p2 := factualCase(t, coherentCall)
	p2dec := decisionOf(t, p2, fs)
	rs := mustVerify(t, rulesetFrom(t, cfgWithRule("one", predictioneval.ComparatorGe, 50, 100)))
	p3b, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, 0))
	if err != nil {
		t.Fatal(err)
	}
	p3bdec := decisionOf(t, p3b, fs)
	res := winnerArtifact("o1")
	proven := p4offline.DerivePlacement(p2dec, fp, validProof(fp))
	win := p4offline.DerivePayout(p2dec, proven, res, linkedRecord(proven, p4offline.KnownInt64(120), p4offline.UnknownInt64("n/a")))
	claim, err := p4offline.ClaimSourceRound(preparedDS(ds), fs)
	if err != nil {
		t.Fatal(err)
	}
	// Another collector session claims the same public round with different
	// evidence (another balance, so another factset).
	sB := newSynth()
	sB.session = "p4-synth-session-b"
	sB.source.CollectorSessionID = sB.session
	sB.due("r1", "e1", 1)
	envB := synthPlacedEnvelope()
	envB.Balance = ptrI64(2000)
	sB.terminal("r1", "e1", 1, predictioneval.PhaseAutoDecided, "OK", envB)
	sB.call("r1", "e1", 1, *envB.FinalAmount, *envB.ChoiceIndex)
	dsB := sB.dataset()
	fsB, err := p4offline.BuildCommonFactset(preparedDS(dsB), singleEpisode(t, mustSelect(t, dsB)).Episode)
	if err != nil {
		t.Fatal(err)
	}
	claimB, err := p4offline.ClaimSourceRound(preparedDS(dsB), fsB)
	if err != nil {
		t.Fatal(err)
	}
	if claimB == claim || fsB.Digest == fs.Digest {
		t.Fatalf("fixture: the two sessions must claim e1 differently")
	}

	t.Run("the canonical claim counts", func(t *testing.T) {
		for name, reg := range map[string]p4offline.SourceRoundRegistry{
			"unique":                 p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{claim}),
			"deduplicated identical": p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{claim, claim}),
		} {
			if err := p4offline.VerifySourceRoundRegistry(reg); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if m := p4offline.AssessDenominatorMembership(preparedDS(ds), prepared(t, reg), fs, p2dec, p3bdec, res, win); !m.Primary || !m.PlacedBet || len(m.Reasons) != 0 {
				t.Fatalf("%s: %+v", name, m)
			}
		}
	})
	t.Run("a round in conflict across datasets counts for nobody", func(t *testing.T) {
		reg := p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{claim, claimB})
		if len(reg.Entries) != 1 || reg.Entries[0].Status != p4offline.SourceRoundConflict {
			t.Fatalf("fixture: %+v", reg.Entries)
		}
		m := p4offline.AssessDenominatorMembership(preparedDS(ds), prepared(t, reg), fs, p2dec, p3bdec, res, win)
		if m.Primary || m.PlacedBet || m.Quality != p4offline.QualityPrimaryScorable ||
			!containsString(m.Reasons, "CASE_NOT_CANONICAL_SOURCE_ROUND") {
			t.Fatalf("a scorable case on a conflicting round is described, never counted: %+v", m)
		}
	})
	t.Run("a round the registry never reconciled counts for nobody", func(t *testing.T) {
		for name, reg := range map[string]p4offline.SourceRoundRegistry{
			"empty":         p4offline.ReconcileSourceRounds(nil),
			"another round": p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{claimB}),
		} {
			if m := p4offline.AssessDenominatorMembership(preparedDS(ds), prepared(t, reg), fs, p2dec, p3bdec, res, win); m.Primary || m.PlacedBet ||
				!containsString(m.Reasons, "CASE_NOT_CANONICAL_SOURCE_ROUND") {
				t.Fatalf("%s: %+v", name, m)
			}
		}
	})
	t.Run("a registry that does not re-derive from its entries is not read", func(t *testing.T) {
		reg := p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{claim, claimB})
		reg.Entries[0].Status = p4offline.SourceRoundUnique // the conflict edited away
		c := claim
		reg.Entries[0].Canonical = &c
		if err := p4offline.VerifySourceRoundRegistry(reg); !errors.Is(err, p4offline.ErrSourceRoundRegistry) {
			t.Fatalf("got %v", err)
		}
		// AND NO HANDLE IS ISSUED FOR IT, which is where the seam refuses
		// now: a registry that does not re-derive never becomes a
		// PreparedSourceRounds, so no verdict can be issued under it at all.
		if _, err := p4offline.PrepareSourceRounds(reg); !errors.Is(err, p4offline.ErrSourceRoundRegistry) {
			t.Fatalf("got %v", err)
		}
		var decoded p4offline.SourceRoundRegistry
		raw, err := json.Marshal(p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{claim}))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if err := p4offline.VerifySourceRoundRegistry(decoded); err != nil {
			t.Fatalf("a registry read back unchanged still re-derives: %v", err)
		}
		decoded.Version = "p4-source-round-registry/v0"
		if err := p4offline.VerifySourceRoundRegistry(decoded); !errors.Is(err, p4offline.ErrSourceRoundRegistry) {
			t.Fatalf("got %v", err)
		}
		// The digest itself, edited over intact entries, is not the entries'.
		forged := p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{claim})
		forged.Digest = strings.Repeat("0", 64)
		if err := p4offline.VerifySourceRoundRegistry(forged); !errors.Is(err, p4offline.ErrSourceRoundRegistry) {
			t.Fatalf("got %v", err)
		}
		// AND NO HANDLE IS ISSUED FOR IT, which is where the seam refuses
		// now: a registry that does not re-derive never becomes a
		// PreparedSourceRounds, so no verdict can be issued under it at all.
		if _, err := p4offline.PrepareSourceRounds(forged); !errors.Is(err, p4offline.ErrSourceRoundRegistry) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a canonical claim swapped in by hand does not re-derive", func(t *testing.T) {
		// The digest frames the claims and whether a canonical exists, not
		// which claim it is: the swap survives the digest and is refused
		// because the registry is no longer what reconciling its own claims
		// produces.
		reg := p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{claimB})
		c := claim
		reg.Entries[0].Canonical = &c
		if err := p4offline.VerifySourceRoundRegistry(reg); !errors.Is(err, p4offline.ErrSourceRoundRegistry) {
			t.Fatalf("got %v", err)
		}
		// AND NO HANDLE IS ISSUED FOR IT, which is where the seam refuses
		// now: a registry that does not re-derive never becomes a
		// PreparedSourceRounds, so no verdict can be issued under it at all.
		if _, err := p4offline.PrepareSourceRounds(reg); !errors.Is(err, p4offline.ErrSourceRoundRegistry) {
			t.Fatalf("got %v", err)
		}
		// What no verifier can see: a conflicting claim deleted by hand
		// leaves a registry that IS the reconciliation of the survivor. The
		// registry is only as complete as the claims the caller reconciled.
		if m := p4offline.AssessDenominatorMembership(preparedDS(ds), prepared(t, p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{claim})), fs, p2dec, p3bdec, res, win); !m.Primary {
			t.Fatalf("%+v", m)
		}
	})
}

// TestDenominatorVerdictNamesTheRegistryItWasIssuedUnder pins the audit
// handle seam 3 leaves on the verdict: once the registry re-derived from its
// own entries and was read for the round, the verdict carries that
// registry's digest — whether the case then counted or was refused as not
// canonical — so an audit can follow it to the registry, and from there to
// the runner's record of what was reconciled into it. A registry that did
// not re-derive, or one never read because the verdict was refused earlier,
// leaves the verdict naming no registry at all.
func TestDenominatorVerdictNamesTheRegistryItWasIssuedUnder(t *testing.T) {
	ds, fs, fp, p2 := factualCase(t, coherentCall)
	p2dec := decisionOf(t, p2, fs)
	rs := mustVerify(t, rulesetFrom(t, cfgWithRule("one", predictioneval.ComparatorGe, 50, 100)))
	p3b, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, 0))
	if err != nil {
		t.Fatal(err)
	}
	p3bdec := decisionOf(t, p3b, fs)
	res := winnerArtifact("o1")
	proven := p4offline.DerivePlacement(p2dec, fp, validProof(fp))
	win := p4offline.DerivePayout(p2dec, proven, res, linkedRecord(proven, p4offline.KnownInt64(120), p4offline.UnknownInt64("n/a")))
	claim, err := p4offline.ClaimSourceRound(preparedDS(ds), fs)
	if err != nil {
		t.Fatal(err)
	}
	// Another dataset's claim on the same public round, under another
	// factset: the registry can only take the caller's word for it, which
	// is exactly why the verdict must name the registry it read.
	other := claim
	other.FactsetDigest = strings.Repeat("f", 64)
	canonical := p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{claim})
	conflict := p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{claim, other})
	if canonical.Digest == "" || conflict.Digest == "" || canonical.Digest == conflict.Digest {
		t.Fatalf("fixture: %q %q", canonical.Digest, conflict.Digest)
	}
	t.Run("a counted decision names the registry", func(t *testing.T) {
		m := p4offline.AssessDenominatorMembership(preparedDS(ds), prepared(t, canonical), fs, p2dec, p3bdec, res, win)
		if !m.Primary || m.RegistryDigest != canonical.Digest {
			t.Fatalf("%+v", m)
		}
		if !wireHasKey(t, m, "registryDigest") {
			t.Fatal("the registry digest must reach the wire")
		}
	})
	t.Run("a decision refused as not canonical names the registry that refused it", func(t *testing.T) {
		m := p4offline.AssessDenominatorMembership(preparedDS(ds), prepared(t, conflict), fs, p2dec, p3bdec, res, win)
		if m.Primary || !containsString(m.Reasons, "CASE_NOT_CANONICAL_SOURCE_ROUND") || m.RegistryDigest != conflict.Digest {
			t.Fatalf("%+v", m)
		}
	})
	t.Run("a registry that did not re-derive is not named", func(t *testing.T) {
		forged := canonical
		forged.Digest = strings.Repeat("0", 64)
		if _, err := p4offline.PrepareSourceRounds(forged); !errors.Is(err, p4offline.ErrSourceRoundRegistry) {
			t.Fatalf("a registry that does not re-derive never becomes a handle: %v", err)
		}
		// AND THE HANDLE IS WHAT THE SEAM SEES, so what an unverifiable
		// registry reduces to there is a handle that was never prepared --
		// the zero value, which carries no witness.
		m := p4offline.AssessDenominatorMembership(preparedDS(ds), p4offline.PreparedSourceRounds{}, fs, p2dec, p3bdec, res, win)
		if !containsString(m.Reasons, "SOURCE_ROUND_REGISTRY_NOT_DERIVED") || m.RegistryDigest != "" {
			t.Fatalf("%+v", m)
		}
		if wireHasKey(t, m, "registryDigest") {
			t.Fatal("an unnamed registry is absent from the wire, not an empty string")
		}
	})
	t.Run("a verdict refused before the registry is read names none", func(t *testing.T) {
		m := p4offline.AssessDenominatorMembership(preparedDS(ds), prepared(t, canonical), fs, p2dec, p3bdec, res, p4offline.PayoutEvidence{})
		if !containsString(m.Reasons, "PAYOUT_NOT_DERIVED") || m.RegistryDigest != "" {
			t.Fatalf("%+v", m)
		}
	})
}

// TestDenominatorVerdictSeparatesAnAbortFromAnExclusion carries the processing
// axis to the seam a future evaluation actually sums over.
//
// WHY IT MATTERS HERE RATHER THAN ONLY AT THE QUALITY. A denominator verdict
// with Primary false reads, to anything that counts, as "this case is not a
// member". When the selection ABORTED, that is not what happened: no case was
// judged at all. The two are indistinguishable on Quality -- both EXCLUDED,
// both fail-closed -- so the verdict carries ProcessingComplete, and a caller
// that sums Primary over a set of verdicts must fail stop rather than publish
// the remainder.
func TestDenominatorVerdictSeparatesAnAbortFromAnExclusion(t *testing.T) {
	ds, fs, fp, p2 := factualCase(t, coherentCall)
	p2dec := decisionOf(t, p2, fs)
	rs := mustVerify(t, rulesetFrom(t, cfgWithRule("one", predictioneval.ComparatorGe, 50, 100)))
	p3b, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, 0))
	if err != nil {
		t.Fatal(err)
	}
	p3bdec := decisionOf(t, p3b, fs)
	res := winnerArtifact("o1")
	proven := p4offline.DerivePlacement(p2dec, fp, validProof(fp))
	pe := p4offline.DerivePayout(p2dec, proven, res, linkedRecord(proven, p4offline.KnownInt64(120), p4offline.UnknownInt64("n/a")))
	reg := registryOf(ds, fs)

	t.Run("a counted member reports complete processing", func(t *testing.T) {
		m := p4offline.AssessDenominatorMembership(preparedDS(ds), prepared(t, reg), fs, p2dec, p3bdec, res, pe)
		if !m.Primary || !m.ProcessingComplete {
			t.Fatalf("a case that was read and counted: %+v", m)
		}
		if !wireBool(t, m, "processingComplete") {
			t.Fatalf("the processing axis must be on the wire: %+v", m)
		}
	})

	t.Run("an ORDINARY non-member still reports complete processing", func(t *testing.T) {
		// THE DISCRIMINATING CONTROL. The dataset was read; it simply does not
		// carry this episode. Not a member, and nothing is wrong with the run.
		other := newSynth()
		other.placedAttempt("r9", "e9", 9)
		m := p4offline.AssessDenominatorMembership(preparedDS(other.dataset()), prepared(t, reg), fs, p2dec, p3bdec, res, pe)
		if m.Primary || m.PlacedBet {
			t.Fatalf("the episode is not in that dataset: %+v", m)
		}
		if !m.ProcessingComplete {
			t.Fatalf("an ordinary exclusion is evidence that WAS produced: %+v", m)
		}
		if !containsString(m.Reasons, p4offline.QualityReasonEpisodeExcluded) {
			t.Fatalf("want an ordinary exclusion reason: %v", m.Reasons)
		}
	})

	t.Run("an aborted selection reports INCOMPLETE processing and no membership", func(t *testing.T) {
		refused := collidingManualDataset(1100)
		if _, err := p4offline.SelectEpisodes(refused); !errors.Is(err, p4offline.ErrEvidenceRetention) {
			t.Fatalf("the fixture must abort, or this proves nothing: %v", err)
		}
		m := p4offline.AssessDenominatorMembership(preparedDS(refused), prepared(t, reg), fs, p2dec, p3bdec, res, pe)
		if m.ProcessingComplete {
			t.Fatalf("nothing was read, so nothing may claim completion: %+v", m)
		}
		if m.Primary || m.PlacedBet {
			t.Fatalf("an aborted selection is no member of anything: %+v", m)
		}
		if m.Quality != p4offline.QualityExcluded {
			t.Fatalf("the verdict must still be fail-closed: %+v", m)
		}
		if !containsString(m.Reasons, p4offline.QualityReasonSelectionUnavailable) {
			t.Fatalf("the reason must say the selection could not RUN: %v", m.Reasons)
		}
		if containsString(m.Reasons, p4offline.QualityReasonEpisodeExcluded) {
			t.Fatalf("a dataset that was never read says nothing about this episode: %v", m.Reasons)
		}
		// AND THE REASON SET IS PINNED AS IT ACTUALLY IS. The verdict returns at
		// the quality gate, so it also carries CASE_NOT_PRIMARY_SCORABLE — a
		// membership reason recorded about a dataset nobody read. That is a
		// limit of a closed vocabulary shared with cases that WERE judged, it is
		// documented at the field, and it is asserted here so the documentation
		// and the artifact cannot drift apart.
		if !containsString(m.Reasons, p4offline.MembershipReasonCaseNotPrimary) {
			t.Fatalf("the emitted reason set is what a consumer sees: %v", m.Reasons)
		}
		if wireBool(t, m, "processingComplete") {
			t.Fatalf("a consumer reading the wire must see the abort: %+v", m)
		}
	})
}

// TestAPreparedRunIsLinearInTheCasesItJudges is obligation B's cost evidence.
//
// WHAT THE DEFECT WAS. Every seam that took a SourceDataset re-ran seams 1-3
// over the whole of it, and AssessDenominatorMembership re-verified the whole
// registry -- hashing every claim, flattening the entries and reconciling them
// again -- for each case it judged, then scanned the entries linearly for one
// canonical claim. ONE verdict therefore selected the dataset TWICE and
// verified the registry ONCE, all three proportional to the run's own size, so
// a run of N cases over an N-claim registry grew quadratically: doubling N
// from 64 to 128 to 256 multiplied the run's total by about 3.8 each time.
// doc.go's WHAT REMAINS owns the absolutes.
//
// WHAT IS ASSERTED IS THAT ONE VERDICT IS FLAT IN THE RUN, which is the same
// statement read per case, and it is the honest place to assert it: a run's
// total is one prepare plus N verdicts, so a flat verdict makes the run linear
// and nothing else does. Measuring one verdict also fails LOUDLY under the
// exact regressions this closes -- restoring either per-case selection, or the
// per-case verification, makes this number grow with n and nothing else does.
//
// THE PADDING IS REAL AND SO IS THE CASE. The dataset carries n episodes and
// the registry n claims, but the case judged is the same one at every width,
// so what moves between the two readings is the run's size and not the case's.
//
// WHAT IT DOES NOT PIN, named here rather than left to be discovered: this
// measures ALLOCATION, so it sees a re-derivation and not a re-SCAN. Replacing
// either prepared index with the linear scan it stands in for allocates
// nothing -- a comparison of episode identities or of a claim to an entry's
// canonical -- so the run would go back to O(N^2) comparisons with this test
// green. A wall-clock assertion is what would catch it and this branch has
// retired two of those for failing to reproduce across hosts, so the honest
// record is that the indexes are argued and the re-derivations are measured.
func TestAPreparedRunIsLinearInTheCasesItJudges(t *testing.T) {
	measure := func(f func()) uint64 {
		runtime.GC()
		var a, b runtime.MemStats
		runtime.ReadMemStats(&a)
		f()
		runtime.ReadMemStats(&b)
		return b.TotalAlloc - a.TotalAlloc
	}
	// verdictAt prepares a run of n cases ONCE and returns what one verdict
	// over it costs.
	verdictAt := func(t *testing.T, n int) uint64 {
		t.Helper()
		s := newSynth()
		for i := 0; i < n; i++ {
			round, event := "r1", "e1" // the case under test is the first
			if i > 0 {
				round, event = "rpad"+strconv.Itoa(i), "epad"+strconv.Itoa(i)
			}
			// EACH ROUND GETS ITS OWN ATTEMPT ID, because an attempt key is
			// (epoch, session, pool, attempt id) and nothing in it names the
			// round: reusing one id across rounds makes every round after the
			// first find another round's attempt under its own key and leaves
			// the whole dataset unusable.
			id := int64(i + 1)
			env := synthPlacedEnvelope()
			s.due(round, event, id)
			s.terminal(round, event, id, predictioneval.PhaseAutoDecided, "OK", env)
			s.call(round, event, id, *env.FinalAmount, *env.ChoiceIndex)
		}
		ds := s.dataset()
		src := p4offline.PrepareDataset(ds)
		sel := mustSelect(t, ds)
		if len(sel.Episodes) != n {
			t.Fatalf("fixture: %d episodes, want %d", len(sel.Episodes), n)
		}
		var claims []p4offline.SourceRoundClaim
		var fs p4offline.CommonFactset
		for _, e := range sel.Episodes {
			built, err := p4offline.BuildCommonFactset(src, e.Episode)
			if err != nil {
				t.Fatalf("fixture: %v", err)
			}
			claim, err := p4offline.ClaimSourceRound(src, built)
			if err != nil {
				t.Fatalf("fixture: %v", err)
			}
			claims = append(claims, claim)
			if e.Episode.EventID == "e1" {
				fs = built
			}
		}
		if fs.Digest == "" {
			t.Fatal("fixture: the case under test is not in the dataset")
		}
		reg := prepared(t, p4offline.ReconcileSourceRounds(claims))
		if reg.Rounds() != n {
			t.Fatalf("fixture: the registry carries %d canonical rounds, want %d", reg.Rounds(), n)
		}
		p2, err := p4offline.EvaluateP2Case(fs)
		if err != nil {
			t.Fatal(err)
		}
		p2dec := decisionOf(t, p2, fs)
		rs := mustVerify(t, rulesetFrom(t, cfgWithRule("one", predictioneval.ComparatorGe, 50, 100)))
		p3b, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, 0))
		if err != nil {
			t.Fatal(err)
		}
		p3bdec := decisionOf(t, p3b, fs)
		fp, err := p4offline.ProjectFactualPlacement(src, fs)
		if err != nil {
			t.Fatal(err)
		}
		res := winnerArtifact("o1")
		proven := p4offline.DerivePlacement(p2dec, fp, validProof(fp))
		win := p4offline.DerivePayout(p2dec, proven, res, linkedRecord(proven, p4offline.KnownInt64(120), p4offline.UnknownInt64("n/a")))
		if m := p4offline.AssessDenominatorMembership(src, reg, fs, p2dec, p3bdec, res, win); !m.Primary || !m.PlacedBet {
			t.Fatalf("fixture: the case under test must COUNT, or the measurement is of a refusal: %+v", m.Reasons)
		}
		const reps = 8
		return measure(func() {
			for i := 0; i < reps; i++ {
				if m := p4offline.AssessDenominatorMembership(src, reg, fs, p2dec, p3bdec, res, win); !m.Primary {
					t.Fatal("the verdict changed under repetition")
				}
			}
		}) / reps
	}

	const narrow, wide = 32, 256
	small, large := verdictAt(t, narrow), verdictAt(t, wide)
	t.Logf("one verdict: %d bytes over %d cases, %d over %d", small, narrow, large, wide)
	// EIGHT TIMES THE RUN, AND THE VERDICT MUST NOT NOTICE. The ceiling is
	// generous on purpose: what it has to separate is flat from proportional,
	// and a verdict that re-selected the dataset or re-verified the registry
	// would read multiples of this, not fractions above it.
	if large > small*2 {
		t.Fatalf("one verdict cost %d bytes over %d cases and %d over %d: the run is still being re-derived per case",
			small, narrow, large, wide)
	}
}
