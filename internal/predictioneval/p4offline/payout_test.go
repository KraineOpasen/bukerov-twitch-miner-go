package p4offline_test

// SEAMS 10 and 12: evidence-only payout and typed quality.
//
// WIN, LOSE and REFUND are scored BY HAND here from the protocol's own rules
// and the artifact's proofs; nothing is scaled, floored, fee-adjusted,
// retried or repaired.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval/p4offline"
)

func winnerArtifact(winner string) p4offline.ResolutionArtifact {
	ev := goodWinnerEvidence()
	ev.WinnerOutcomeID = winner
	return p4offline.ProjectResolution(ev)
}

func refundArtifact() p4offline.ResolutionArtifact {
	ev := goodWinnerEvidence()
	ev.Claim = p4offline.ResolutionRefund
	ev.WinnerOutcomeID = ""
	ev.ProofBasis = p4offline.ProofBasisPlatformCanceledRefund
	ev.EvidenceReferences = []p4offline.EvidenceReference{{ObservationID: "canceled-1", Kind: "channel_event",
		Phase: "ROUND_UPDATED", RoundState: "CANCELED", EventID: "e1"}}
	return p4offline.ProjectResolution(ev)
}

func linkedRecord(pl p4offline.PlacementEvidence, payout p4offline.Int64Fact, returned p4offline.Int64Fact) *p4offline.PayoutRecord {
	return &p4offline.PayoutRecord{
		LinkageBasis:            p4offline.LinkageBasisAttemptLinkedUserTerminal,
		LinkedCallObservationID: pl.AttributedCallObservationID,
		Reference: p4offline.EvidenceReference{ObservationID: "settlement-1", Kind: "user_terminal",
			Phase: "TERMINAL_ADMITTED", RoundState: "RESOLVED", EventID: "e1"},
		Payout:        payout,
		ReturnedStake: returned,
		ProofRevision: "test-proof/v1",
	}
}

// TestHandwrittenWinLoseRefundScoring scores the factual P2 placement (o1,
// stake 50, platform-proven) against explicit resolutions.
func TestHandwrittenWinLoseRefundScoring(t *testing.T) {
	_, fs, fp, p2 := factualCase(t, coherentCall)
	dec := decisionOf(t, p2, fs)
	proven := p4offline.DerivePlacement(dec, fp, validProof(fp))
	if proven.Status != p4offline.PlacementAcceptedPlatformProven {
		t.Fatalf("fixture: %+v", proven)
	}
	unproven := p4offline.DerivePlacement(dec, fp, nil)

	t.Run("WIN: payout 120 on a stake of 50 nets 70", func(t *testing.T) {
		res := winnerArtifact("o1")
		pe := p4offline.DerivePayout(dec, proven, res, linkedRecord(proven, p4offline.KnownInt64(120), p4offline.UnknownInt64("n/a")))
		if pe.ContractVersion != p4offline.PayoutEvidenceVersion || pe.Policy != p4offline.PolicyP2 ||
			pe.Attempt != fs.Attempt || pe.FactsetDigest != fs.Digest || pe.EventID != "e1" {
			t.Fatalf("%+v", pe)
		}
		if pe.ChoiceCorrect != p4offline.ChoiceCorrect || pe.Outcome != p4offline.PayoutWin ||
			!pe.Stake.Known() || pe.Stake.Value != 50 || !pe.Payout.Known() || pe.Payout.Value != 120 ||
			!pe.Net.Known() || pe.Net.Value != 70 || pe.ResolutionFactsDigest != res.ResolutionFactsDigest {
			t.Fatalf("%+v", pe)
		}
	})
	t.Run("WIN without a linked payout record has an UNKNOWN payout and net", func(t *testing.T) {
		pe := p4offline.DerivePayout(dec, proven, winnerArtifact("o1"), nil)
		if pe.ChoiceCorrect != p4offline.ChoiceCorrect || pe.Outcome != p4offline.PayoutWin ||
			pe.Payout.Known() || pe.Net.Known() || !containsString(pe.Reasons, "PAYOUT_NOT_RECORDED") {
			t.Fatalf("a win is not a number until the payout is recorded: %+v", pe)
		}
	})
	t.Run("a payout record linked to another call does not count", func(t *testing.T) {
		rec := linkedRecord(proven, p4offline.KnownInt64(120), p4offline.UnknownInt64("n/a"))
		rec.LinkedCallObservationID = "other-call"
		pe := p4offline.DerivePayout(dec, proven, winnerArtifact("o1"), rec)
		if pe.Payout.Known() || pe.Net.Known() || !containsString(pe.Reasons, "RECORD_NOT_LINKED") {
			t.Fatalf("%+v", pe)
		}
	})
	t.Run("a payout record on a foreign linkage basis does not count", func(t *testing.T) {
		rec := linkedRecord(proven, p4offline.KnownInt64(120), p4offline.UnknownInt64("n/a"))
		rec.LinkageBasis = "NEAREST_TIME"
		pe := p4offline.DerivePayout(dec, proven, winnerArtifact("o1"), rec)
		if pe.Payout.Known() || pe.Net.Known() || !containsString(pe.Reasons, "RECORD_LINKAGE_NOT_ACCEPTED") {
			t.Fatalf("%+v", pe)
		}
	})
	t.Run("LOSE: the accepted stake is lost, net -50", func(t *testing.T) {
		pe := p4offline.DerivePayout(dec, proven, winnerArtifact("o2"), nil)
		if pe.ChoiceCorrect != p4offline.ChoiceIncorrect || pe.Outcome != p4offline.PayoutLose ||
			!pe.Payout.Known() || pe.Payout.Value != 0 || !pe.Net.Known() || pe.Net.Value != -50 {
			t.Fatalf("%+v", pe)
		}
	})
	t.Run("REFUND: net 0 only when the returned stake is recorded", func(t *testing.T) {
		with := p4offline.DerivePayout(dec, proven, refundArtifact(), linkedRecord(proven, p4offline.KnownInt64(0), p4offline.KnownInt64(50)))
		if with.ChoiceCorrect != p4offline.ChoiceNotApplicable || with.Outcome != p4offline.PayoutRefund ||
			!with.Net.Known() || with.Net.Value != 0 || !with.Payout.Known() || with.Payout.Value != 0 {
			t.Fatalf("%+v", with)
		}
		without := p4offline.DerivePayout(dec, proven, refundArtifact(), nil)
		if without.Outcome != p4offline.PayoutRefund || without.Net.Known() || !containsString(without.Reasons, "REFUND_RETURN_NOT_RECORDED") {
			t.Fatalf("%+v", without)
		}
		short := p4offline.DerivePayout(dec, proven, refundArtifact(), linkedRecord(proven, p4offline.KnownInt64(0), p4offline.KnownInt64(40)))
		if short.Net.Known() || !containsString(short.Reasons, "RETURNED_STAKE_MISMATCH") {
			t.Fatalf("a returned stake that is not the stake is a conflict, not a partial refund: %+v", short)
		}
	})
	t.Run("winner UNKNOWN scores nothing", func(t *testing.T) {
		unknown := p4offline.ResolutionNotRecorded(p4offline.PublicRoundIdentity{EventID: "e1"}, []string{"o1", "o2"}, nil, "p")
		pe := p4offline.DerivePayout(dec, proven, unknown, linkedRecord(proven, p4offline.KnownInt64(120), p4offline.UnknownInt64("n/a")))
		if pe.ChoiceCorrect != p4offline.ChoiceUnknown || pe.Outcome != p4offline.PayoutUnknown || pe.Payout.Known() || pe.Net.Known() {
			t.Fatalf("%+v", pe)
		}
		if !pe.Stake.Known() || pe.Stake.Value != 50 {
			t.Fatalf("the policy's stake is known regardless: %+v", pe.Stake)
		}
	})
	t.Run("an unproven placement keeps the choice scorable and the money unknown", func(t *testing.T) {
		pe := p4offline.DerivePayout(dec, unproven, winnerArtifact("o1"), linkedRecord(proven, p4offline.KnownInt64(120), p4offline.UnknownInt64("n/a")))
		if pe.ChoiceCorrect != p4offline.ChoiceCorrect || pe.Outcome != p4offline.PayoutUnknown || pe.Payout.Known() || pe.Net.Known() ||
			!containsString(pe.Reasons, "PLACEMENT_NOT_PROVEN") {
			t.Fatalf("%+v", pe)
		}
	})
	t.Run("a tampered resolution digest scores nothing", func(t *testing.T) {
		res := winnerArtifact("o1")
		res.WinnerOutcomeID = "o2"
		pe := p4offline.DerivePayout(dec, proven, res, nil)
		if pe.ChoiceCorrect != p4offline.ChoiceUnknown || pe.Outcome != p4offline.PayoutUnknown || !containsString(pe.Reasons, "RESOLUTION_DIGEST_MISMATCH") {
			t.Fatalf("%+v", pe)
		}
	})
	t.Run("a consistently digested artifact that asserts an unproven winner scores nothing", func(t *testing.T) {
		pe := p4offline.DerivePayout(dec, proven, forgedWinnerArtifact("o1"), linkedRecord(proven, p4offline.KnownInt64(120), p4offline.UnknownInt64("n/a")))
		if pe.ChoiceCorrect != p4offline.ChoiceUnknown || pe.Outcome != p4offline.PayoutUnknown || pe.Payout.Known() ||
			!containsString(pe.Reasons, "RESOLUTION_NOT_DERIVABLE") {
			t.Fatalf("%+v", pe)
		}
	})
	t.Run("a resolution for another round scores nothing", func(t *testing.T) {
		ev := goodWinnerEvidence()
		ev.Round.EventID = "e2"
		ev.EvidenceReferences[0].EventID = "e2"
		pe := p4offline.DerivePayout(dec, proven, p4offline.ProjectResolution(ev), nil)
		if pe.ChoiceCorrect != p4offline.ChoiceUnknown || !containsString(pe.Reasons, "RESOLUTION_ROUND_MISMATCH") {
			t.Fatalf("%+v", pe)
		}
	})
	t.Run("a resolution over another outcome set scores nothing", func(t *testing.T) {
		ev := goodWinnerEvidence()
		ev.OrderedOutcomeIDs = []string{"o2", "o1"}
		pe := p4offline.DerivePayout(dec, proven, p4offline.ProjectResolution(ev), nil)
		if pe.ChoiceCorrect != p4offline.ChoiceUnknown || !containsString(pe.Reasons, "RESOLUTION_OUTCOME_SET_MISMATCH") {
			t.Fatalf("the same identities in another order are another set: %+v", pe)
		}
		ev.OrderedOutcomeIDs = []string{"o1", "o2", "o3"}
		if pe := p4offline.DerivePayout(dec, proven, p4offline.ProjectResolution(ev), nil); !containsString(pe.Reasons, "RESOLUTION_OUTCOME_SET_MISMATCH") {
			t.Fatalf("%+v", pe)
		}
	})
	t.Run("a choice whose identity is not its index settles nothing", func(t *testing.T) {
		d := dec
		d.Choice.OutcomeID = "o2"
		pe := p4offline.DerivePayout(d, proven, winnerArtifact("o2"), linkedRecord(proven, p4offline.KnownInt64(120), p4offline.UnknownInt64("n/a")))
		if pe.ChoiceCorrect != p4offline.ChoiceUnknown || pe.Outcome != p4offline.PayoutUnknown || !containsString(pe.Reasons, "CHOICE_INDEX_ID_MISMATCH") {
			t.Fatalf("the factual call was on slot 0 (o1); it cannot be paid as a bet on o2: %+v", pe)
		}
	})
	t.Run("negative money is refused, never subtracted", func(t *testing.T) {
		pe := p4offline.DerivePayout(dec, proven, winnerArtifact("o1"), linkedRecord(proven, p4offline.KnownInt64(-1), p4offline.UnknownInt64("n/a")))
		if pe.Payout.Known() || pe.Net.Known() || !containsString(pe.Reasons, "PAYOUT_NEGATIVE") {
			t.Fatalf("%+v", pe)
		}
		d := dec
		d.Stake = p4offline.KnownInt64(-5)
		negative := p4offline.DerivePlacement(d, fp, validProof(fp))
		pe = p4offline.DerivePayout(d, negative, winnerArtifact("o2"), nil)
		if pe.Net.Known() || !containsString(pe.Reasons, "STAKE_NEGATIVE") {
			t.Fatalf("%+v", pe)
		}
	})
	t.Run("a placement from another policy is not this policy's", func(t *testing.T) {
		rs := mustVerify(t, rulesetFrom(t, cfgWithRule("p3b", predictioneval.ComparatorGe, 50, 100)))
		p3b, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, 0))
		if err != nil {
			t.Fatal(err)
		}
		foreign := p4offline.DerivePlacement(decisionOf(t, p3b, fs), fp, validProof(fp))
		if foreign.Policy != p4offline.PolicyP3b {
			t.Fatalf("fixture: %+v", foreign)
		}
		pe := p4offline.DerivePayout(dec, foreign, winnerArtifact("o1"), nil)
		if pe.Outcome != p4offline.PayoutUnknown || !containsString(pe.Reasons, "POLICY_BINDING_MISMATCH") {
			t.Fatalf("%+v", pe)
		}
	})
	t.Run("an edited, decoded or hand-built placement verdict settles nothing", func(t *testing.T) {
		edited := proven
		edited.PolicyStake = p4offline.KnownInt64(51)
		if pe := p4offline.DerivePayout(dec, edited, winnerArtifact("o1"), nil); pe.Outcome != p4offline.PayoutUnknown ||
			!containsString(pe.Reasons, "PLACEMENT_NOT_DERIVED") {
			t.Fatalf("%+v", pe)
		}
		promoted := unproven
		promoted.Status = p4offline.PlacementAcceptedPlatformProven
		if pe := p4offline.DerivePayout(dec, promoted, winnerArtifact("o1"), linkedRecord(proven, p4offline.KnownInt64(120), p4offline.UnknownInt64("n/a"))); pe.Payout.Known() ||
			!containsString(pe.Reasons, "PLACEMENT_NOT_DERIVED") {
			t.Fatalf("a status edited into ACCEPTED must not pay: %+v", pe)
		}
		var decoded p4offline.PlacementEvidence
		raw := mustMarshal(t, proven)
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if pe := p4offline.DerivePayout(dec, decoded, winnerArtifact("o1"), nil); !containsString(pe.Reasons, "PLACEMENT_NOT_DERIVED") {
			t.Fatalf("a placement verdict read back from storage must be re-derived, not trusted: %+v", pe)
		}
		handBuilt := p4offline.PlacementEvidence{ContractVersion: p4offline.PlacementEvidenceVersion, Policy: p4offline.PolicyP2,
			Attempt: dec.Attempt, FactsetDigest: dec.FactsetDigest, EventID: dec.EventID, Status: p4offline.PlacementAcceptedPlatformProven,
			PolicyStake: dec.Stake, AttributedOutcomeID: "o1", AttributedCallObservationID: "ghost-call"}
		if pe := p4offline.DerivePayout(dec, handBuilt, winnerArtifact("o1"), linkedRecord(handBuilt, p4offline.KnownInt64(120), p4offline.UnknownInt64("n/a"))); pe.Payout.Known() ||
			!containsString(pe.Reasons, "PLACEMENT_NOT_DERIVED") {
			t.Fatalf("%+v", pe)
		}
	})
	t.Run("a placement from another case is not this case's", func(t *testing.T) {
		// Case B: the same public round in another collector session, a
		// PLACE decision, no call recorded. Its own placement is NOT_RECORDED.
		s := newSynth()
		s.session = "p4-synth-session-b"
		s.source.CollectorSessionID = s.session
		s.due("r1", "e1", 1)
		s.terminal("r1", "e1", 1, predictioneval.PhaseAutoDecided, "OK", synthPlacedEnvelope())
		dsB := s.dataset()
		epB := singleEpisode(t, mustSelect(t, dsB))
		fsB, err := p4offline.BuildCommonFactset(preparedDS(dsB), epB.Episode)
		if err != nil {
			t.Fatal(err)
		}
		p2B, err := p4offline.EvaluateP2Case(fsB)
		if err != nil {
			t.Fatal(err)
		}
		decB := decisionOf(t, p2B, fsB)
		if decB.Choice != dec.Choice || decB.Stake != dec.Stake {
			t.Fatalf("fixture: case B must decide exactly as case A did")
		}
		pe := p4offline.DerivePayout(decB, proven, winnerArtifact("o1"), linkedRecord(proven, p4offline.KnownInt64(120), p4offline.UnknownInt64("n/a")))
		if pe.Outcome != p4offline.PayoutUnknown || pe.Payout.Known() || !containsString(pe.Reasons, "PLACEMENT_BINDING_MISMATCH") {
			t.Fatalf("case A's proven placement must not settle case B's identical decision: %+v", pe)
		}
		restakedDec := dec
		restakedDec.Stake = p4offline.KnownInt64(51)
		restaked := p4offline.DerivePlacement(restakedDec, fp, validProof(fp))
		if pe := p4offline.DerivePayout(dec, restaked, winnerArtifact("o1"), nil); !containsString(pe.Reasons, "PLACEMENT_BINDING_MISMATCH") {
			t.Fatalf("a genuine verdict for another stake is another decision's: %+v", pe)
		}
	})
	t.Run("a decision that is not legal or not named settles nothing", func(t *testing.T) {
		unnamed := dec
		unnamed.Policy = ""
		unnamed.Action.Policy = ""
		foreign := proven
		foreign.Policy = ""
		if pe := p4offline.DerivePayout(unnamed, foreign, winnerArtifact("o1"), nil); pe.Outcome != p4offline.PayoutUnknown ||
			!containsString(pe.Reasons, "POLICY_BINDING_MISMATCH") {
			t.Fatalf("%+v", pe)
		}
		illegal := dec
		illegal.Action.Legal = false
		illegal.Action.Class = p4offline.ActionUnsupportedShape
		if pe := p4offline.DerivePayout(illegal, proven, winnerArtifact("o1"), nil); pe.ChoiceCorrect != p4offline.ChoiceUnknown ||
			!containsString(pe.Reasons, "ILLEGAL_NATIVE_SHAPE") {
			t.Fatalf("%+v", pe)
		}
	})
}

// TestPolicySkipVersusNoAttemptPrefixPayout pins the exact zero against the
// absent zero.
func TestPolicySkipVersusNoAttemptPrefixPayout(t *testing.T) {
	_, fs, fp, _ := factualCase(t, coherentCall)
	_, fsS, fpS, p2S := skippedCase(t)
	skip := decisionOf(t, p2S, fsS)
	pl := p4offline.DerivePlacement(skip, fpS, nil)
	pe := p4offline.DerivePayout(skip, pl, winnerArtifact("o1"), nil)
	if pe.Outcome != p4offline.PayoutNotApplicable || pe.ChoiceCorrect != p4offline.ChoiceNotApplicable ||
		!pe.Stake.Known() || pe.Stake.Value != 0 || !pe.Net.Known() || pe.Net.Value != 0 ||
		pe.Payout.Presence != p4offline.PresenceNotApplicable {
		t.Fatalf("POLICY_SKIP: stake and net are exact zeros: %+v", pe)
	}
	t.Run("a skip class cannot manufacture a zero from a known or unknown stake", func(t *testing.T) {
		wrong := skip
		wrong.Stake = p4offline.KnownInt64(50)
		wrongPl := p4offline.DerivePlacement(wrong, fpS, nil)
		if pe := p4offline.DerivePayout(wrong, wrongPl, winnerArtifact("o1"), nil); pe.Stake.Known() || pe.Net.Known() ||
			!containsString(pe.Reasons, "POLICY_SKIP_STAKE_NOT_EXACT_ZERO") {
			t.Fatalf("%+v", pe)
		}
		wrong.Stake = p4offline.UnknownInt64("nobody knows")
		wrongPl = p4offline.DerivePlacement(wrong, fpS, nil)
		if pe := p4offline.DerivePayout(wrong, wrongPl, winnerArtifact("o1"), nil); pe.Stake.Known() || pe.Net.Known() {
			t.Fatalf("%+v", pe)
		}
	})

	rs := mustVerify(t, rulesetFrom(t, cfgDefaultOnly("none", 95, 100)))
	p3b, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, 0))
	if err != nil {
		t.Fatal(err)
	}
	none := decisionOf(t, p3b, fs)
	pl = p4offline.DerivePlacement(none, fp, nil)
	pe = p4offline.DerivePayout(none, pl, winnerArtifact("o1"), nil)
	if pe.Outcome != p4offline.PayoutUnknown || pe.Stake.Known() || pe.Net.Known() || pe.Payout.Known() ||
		pe.ChoiceCorrect != p4offline.ChoiceNotApplicable ||
		!containsString(pe.Reasons, string(p4offline.ActionNoAttemptInSuppliedPrefix)) {
		t.Fatalf("NO_ATTEMPT_IN_SUPPLIED_PREFIX carries no financial zero: %+v", pe)
	}
	if pe.Stake.Presence != p4offline.PresenceUnknown {
		t.Fatalf("unknown, not not-applicable: %+v", pe.Stake)
	}
}

// TestKnownZeroP3bStakeIsScoredOnChoiceOnly pins the known-zero stake: the
// choice is scorable, the counterfactual money is not.
func TestKnownZeroP3bStakeIsScoredOnChoiceOnly(t *testing.T) {
	s := newSynth()
	s.due("r1", "e1", 1)
	env := synthPlacedEnvelope()
	env.Balance = ptrI64(0)
	env.ChoiceAmount = ptrI64(0)
	env.StakeAllowed = ptrI64(0)
	env.FinalAmount = ptrI64(0)
	s.terminal("r1", "e1", 1, predictioneval.PhaseAutoSkipped, "BELOW_MINIMUM_POINTS", env)
	ds := s.dataset()
	ep := singleEpisode(t, mustSelect(t, ds))
	fs, err := p4offline.BuildCommonFactset(preparedDS(ds), ep.Episode)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := p4offline.ProjectFactualPlacement(preparedDS(ds), fs)
	if err != nil {
		t.Fatal(err)
	}
	rs := mustVerify(t, rulesetFrom(t, cfgWithRule("one", predictioneval.ComparatorGe, 50, 100)))
	p3b, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, 0))
	if err != nil {
		t.Fatal(err)
	}
	dec := decisionOf(t, p3b, fs)
	if dec.Action.Class != p4offline.ActionWouldAttempt || !dec.Stake.Known() || dec.Stake.Value != 0 {
		t.Fatalf("fixture: %+v", dec)
	}
	pl := p4offline.DerivePlacement(dec, fp, nil)
	if pl.Status != p4offline.PlacementCounterfactualNotInheritable {
		t.Fatalf("the factual decision skipped; a counterfactual attempt cannot inherit: %+v", pl)
	}
	pe := p4offline.DerivePayout(dec, pl, winnerArtifact("o1"), nil)
	if pe.ChoiceCorrect != p4offline.ChoiceCorrect || pe.Outcome != p4offline.PayoutUnknown ||
		!pe.Stake.Known() || pe.Stake.Value != 0 || pe.Net.Known() {
		t.Fatalf("%+v", pe)
	}
}

// asP3bAttempt relabels a decision as a legal P3b WOULD_ATTEMPT with the same
// choice and stake, for tests that need both policy slots filled from one
// factset the fixtures cannot evaluate natively.
func asP3bAttempt(d p4offline.PolicyDecision) p4offline.PolicyDecision {
	d.Policy = p4offline.PolicyP3b
	d.Action = legalMapping(p4offline.PolicyP3b, string(predictioneval.StatusWouldAttempt), p4offline.ActionWouldAttempt)
	return d
}

// TestQualityRecordsAreClosedMonotoneAndDetached pins the record type: no
// upgrade, a bogus target or receiver is EXCLUDED, and two records branched
// from one parent never share an audit trail.
func TestQualityRecordsAreClosedMonotoneAndDetached(t *testing.T) {
	q := p4offline.NewQualityRecord()
	if q.Quality != p4offline.QualityPrimaryScorable {
		t.Fatalf("%+v", q)
	}
	q = q.Downgrade(p4offline.QualityDescriptiveOnly, "first")
	q = q.Downgrade(p4offline.QualityPrimaryScorable, "attempted upgrade")
	if q.Quality != p4offline.QualityDescriptiveOnly || !containsString(q.Reasons, "attempted upgrade") || len(q.History) != 1 {
		t.Fatalf("an upgrade must change nothing but the record of the attempt: %+v", q)
	}
	if q.Downgrade("BOGUS", "x").Quality != p4offline.QualityExcluded {
		t.Fatal("a quality outside the vocabulary is EXCLUDED")
	}
	merged := q.Merge(p4offline.NewQualityRecord().Downgrade(p4offline.QualityExcluded, "worse"))
	if merged.Quality != p4offline.QualityExcluded || !containsString(merged.Reasons, "first") || !containsString(merged.Reasons, "worse") {
		t.Fatalf("%+v", merged)
	}
	if p4offline.NewQualityRecord().Merge(q).Quality != p4offline.QualityDescriptiveOnly {
		t.Fatal("merge takes the lower quality whichever side it is on")
	}
	t.Run("merging keeps every reason once, in first-seen order", func(t *testing.T) {
		// THE PROPERTY THE REPAIR HAD TO PRESERVE. Merge used appendOnce in a
		// loop, which scans the accumulated list on every call -- O(n*m) in two
		// slices the caller owns, measured through this exported method at
		// ~3.9x per 2x input (3.7 ms at 2,000 distinct reasons, 223 ms at
		// 16,000). It is the fourth superlinear accumulation found on this
		// branch. The seen-set that replaced it is CPU-only, so reverting it
		// survives any allocation-ratio assertion and this suite makes none
		// about wall-clock; what is pinned instead is the ANSWER -- the same
		// list, deduplicated, in the same order -- exactly as the
		// manual-signal repair in evidence.go is pinned.
		var left, right p4offline.QualityRecord
		left.Quality = p4offline.QualityDescriptiveOnly
		left.Reasons = []string{"a", "b", "c"}
		right.Quality = p4offline.QualityDescriptiveOnly
		right.Reasons = []string{"b", "d", "a", "d", "e"}

		got := left.Merge(right).Reasons
		want := []string{"a", "b", "c", "d", "e"}
		if len(got) != len(want) {
			t.Fatalf("every reason exactly once, in first-seen order: got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("every reason exactly once, in first-seen order: got %v, want %v", got, want)
			}
		}
		// And the caller's own slices are untouched: normalised() detaches.
		if len(left.Reasons) != 3 || len(right.Reasons) != 5 {
			t.Fatalf("merge must not write through to either operand: %v / %v", left.Reasons, right.Reasons)
		}
	})

	t.Run("the zero value and a decoded record are EXCLUDED, not immune", func(t *testing.T) {
		var zero p4offline.QualityRecord
		if got := zero.Downgrade(p4offline.QualityExcluded, "x"); got.Quality != p4offline.QualityExcluded ||
			!containsString(got.Reasons, "QUALITY_OUTSIDE_VOCABULARY") || len(got.History) == 0 {
			t.Fatalf("%+v", got)
		}
		if got := zero.Merge(p4offline.NewQualityRecord()); got.Quality != p4offline.QualityExcluded {
			t.Fatalf("a zero receiver cannot win a merge: %+v", got)
		}
		if got := p4offline.NewQualityRecord().Merge(zero); got.Quality != p4offline.QualityExcluded {
			t.Fatalf("a zero argument cannot win a merge: %+v", got)
		}
		var decoded p4offline.QualityRecord
		if err := json.Unmarshal([]byte(`{"quality":"PROBABLY_FINE","reasons":["r"]}`), &decoded); err != nil {
			t.Fatal(err)
		}
		if got := decoded.Downgrade(p4offline.QualityDescriptiveOnly, "y"); got.Quality != p4offline.QualityExcluded {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("branches from one record do not rewrite each other", func(t *testing.T) {
		base := p4offline.NewQualityRecord().Downgrade(p4offline.QualityDescriptiveOnly, "a").
			Downgrade(p4offline.QualityDescriptiveOnly, "b").Downgrade(p4offline.QualityDescriptiveOnly, "c")
		b1 := base.Downgrade(p4offline.QualityExcluded, "d")
		b2 := base.Downgrade(p4offline.QualityExcluded, "e")
		if !containsString(b1.Reasons, "d") || containsString(b1.Reasons, "e") || !containsString(b2.Reasons, "e") || containsString(b2.Reasons, "d") {
			t.Fatalf("branches share a backing array: %v / %v", b1.Reasons, b2.Reasons)
		}
		if len(base.Reasons) != 3 || len(base.History) != 1 {
			t.Fatalf("the parent changed: %+v", base)
		}
		m1 := base.Merge(b1)
		m2 := base.Merge(b2)
		if containsString(m1.Reasons, "e") || containsString(m2.Reasons, "d") {
			t.Fatalf("merges share a backing array: %v / %v", m1.Reasons, m2.Reasons)
		}

		// THE HISTORY HALF, which was unguarded. normalised() detaches BOTH
		// slices and Downgrade's doc says so ("detaches the slices"), but only
		// the Reasons half had a case: deleting the History clone left the whole
		// suite green. History is the audit trail of which downgrade happened,
		// so two branches silently rewriting each other's is worse than the
		// Reasons case, not better.
		//
		// A branch point whose History has SPARE CAPACITY is what makes the
		// sharing observable: without the clone both branches append into the
		// same backing array and the second wins.
		deep := p4offline.NewQualityRecord().
			Downgrade(p4offline.QualityDescriptiveOnly, "h1").
			Downgrade(p4offline.QualityExcluded, "h2")
		h1 := deep.Downgrade(p4offline.QualityExcluded, "h3")
		h2 := deep.Downgrade(p4offline.QualityExcluded, "h4")
		lastReason := func(r p4offline.QualityRecord) string {
			if len(r.History) == 0 {
				return ""
			}
			return r.History[len(r.History)-1].Reason
		}
		// Neither branch actually LOWERS the quality again -- both are already
		// EXCLUDED -- so History must be unchanged on both and on the parent.
		if lastReason(h1) != "h2" || lastReason(h2) != "h2" || lastReason(deep) != "h2" {
			t.Fatalf("a downgrade that lowers nothing must not append history: %q / %q / %q",
				lastReason(h1), lastReason(h2), lastReason(deep))
		}
		if len(h1.History) != len(deep.History) || len(h2.History) != len(deep.History) {
			t.Fatalf("history lengths diverged: %d / %d / %d", len(h1.History), len(h2.History), len(deep.History))
		}
		// And the observable half. SPARE CAPACITY is what makes the sharing
		// visible, and a record built only through Downgrade never has any --
		// every append lands on an exactly-sized clone, so both branches
		// reallocate and the defect hides. A first version of this case missed
		// the mutant for exactly that reason. QualityRecord is an exported type
		// with exported fields, so the branch point is built directly.
		hist := make([]p4offline.QualityStep, 1, 4)
		hist[0] = p4offline.QualityStep{Quality: p4offline.QualityDescriptiveOnly, Reason: "w0"}
		wide := p4offline.QualityRecord{
			Quality: p4offline.QualityDescriptiveOnly,
			Reasons: []string{"w0"},
			History: hist,
		}
		w1 := wide.Downgrade(p4offline.QualityExcluded, "w1")
		w2 := wide.Downgrade(p4offline.QualityExcluded, "w2")
		if lastReason(w1) != "w1" || lastReason(w2) != "w2" {
			t.Fatalf("branches share a history backing array: %q / %q", lastReason(w1), lastReason(w2))
		}
		if len(wide.History) != 1 || wide.History[0].Reason != "w0" {
			t.Fatalf("the parent's history changed: %+v", wide.History)
		}
	})
}

// TestAssessCaseQualityIsTheMinimumOverEverySeam pins seam 12 over the
// dataset: every disqualification lowers, nothing raises, and the episode's
// admission is re-derived from the raw evidence rather than read from a
// selection.
func TestAssessCaseQualityIsTheMinimumOverEverySeam(t *testing.T) {
	ds, fs, _, p2 := factualCase(t, coherentCall)
	p2dec := decisionOf(t, p2, fs)
	rs := mustVerify(t, rulesetFrom(t, cfgWithRule("one", predictioneval.ComparatorGe, 50, 100)))
	p3b, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, 0))
	if err != nil {
		t.Fatal(err)
	}
	p3bdec := decisionOf(t, p3b, fs)
	primary := p4offline.AssessCaseQuality(preparedDS(ds), fs, p2dec, p3bdec, winnerArtifact("o1"))
	if primary.Quality != p4offline.QualityPrimaryScorable || len(primary.Reasons) != 0 {
		t.Fatalf("a complete, legal, resolved case with two choices is PRIMARY_SCORABLE: %+v", primary)
	}
	unresolved := p4offline.AssessCaseQuality(preparedDS(ds), fs, p2dec, p3bdec,
		p4offline.ResolutionNotRecorded(p4offline.PublicRoundIdentity{EventID: "e1"}, []string{"o1", "o2"}, nil, "p"))
	if unresolved.Quality != p4offline.QualityDescriptiveOnly || !containsString(unresolved.Reasons, "RESOLUTION_UNKNOWN") {
		t.Fatalf("%+v", unresolved)
	}
	if q := p4offline.AssessCaseQuality(preparedDS(ds), fs, p2dec, p3bdec, refundArtifact()); q.Quality != p4offline.QualityDescriptiveOnly {
		t.Fatalf("a refund names no winner for the primary metric: %+v", q)
	}

	// No later fact repairs an earlier missing value: a factset missing its
	// balance stays descriptive however good the resolution is.
	dsMissing, _, missing := selectedCase(t, func(e *predictioneval.SourceDecisionEnvelope) { e.Balance = nil }, nil)
	missingDec := p2dec
	missingDec.FactsetDigest = missing.Digest
	if q := p4offline.AssessCaseQuality(preparedDS(dsMissing), missing, missingDec, asP3bAttempt(missingDec), winnerArtifact("o1")); q.Quality != p4offline.QualityDescriptiveOnly ||
		!containsString(q.Reasons, "FACTSET_INCOMPLETE") {
		t.Fatalf("%+v", q)
	}

	t.Run("policy verdicts", func(t *testing.T) {
		illegal := p3bdec
		illegal.Action.Legal = false
		illegal.Action.Class = p4offline.ActionUnsupportedShape
		if q := p4offline.AssessCaseQuality(preparedDS(ds), fs, p2dec, illegal, winnerArtifact("o1")); q.Quality != p4offline.QualityExcluded ||
			!containsString(q.Reasons, "P3B_ILLEGAL_NATIVE_SHAPE") {
			t.Fatalf("no genuine result maps to an illegal shape; an illegal decision is an edited one and is not evidence: %+v", q)
		}
		// P2 is a pure function of the factset: a P2 decision that is not
		// what evaluating the factset yields is not evidence, however well
		// bound it looks.
		for name, edit := range map[string]func(*p4offline.PolicyDecision){
			"stake": func(d *p4offline.PolicyDecision) { d.Stake = p4offline.KnownInt64(51) },
			"choice": func(d *p4offline.PolicyDecision) {
				d.Choice = p4offline.PolicyChoice{Present: true, Index: 1, OutcomeID: "o2"}
			},
			"class": func(d *p4offline.PolicyDecision) {
				d.Action.Class = p4offline.ActionPolicySkip
				d.Action.SkipReason = "X"
				d.Stake = p4offline.KnownInt64(0)
			},
		} {
			edited := p2dec
			edit(&edited)
			if q := p4offline.AssessCaseQuality(preparedDS(ds), fs, edited, p3bdec, winnerArtifact("o1")); q.Quality != p4offline.QualityExcluded ||
				!containsString(q.Reasons, "POLICY_DECISION_NOT_DERIVED") {
				t.Fatalf("edited P2 %s: %+v", name, q)
			}
		}
		// A genuine P3b UNKNOWN_INPUT: negative points are outside the
		// core's domain.
		dsN, _, fsN := selectedCase(t, func(e *predictioneval.SourceDecisionEnvelope) { e.Outcomes[1].TotalPoints = -1 }, nil)
		p2N, err := p4offline.EvaluateP2Case(fsN)
		if err != nil {
			t.Fatal(err)
		}
		p3bN, err := p4offline.EvaluateP3bCase(fsN, rs, synthCoords(fsN, 0))
		if err != nil {
			t.Fatal(err)
		}
		if p3bN.Action.Class != p4offline.ActionUnknownInput {
			t.Fatalf("fixture: %+v", p3bN.Action)
		}
		if q := p4offline.AssessCaseQuality(preparedDS(dsN), fsN, decisionOf(t, p2N, fsN), decisionOf(t, p3bN, fsN), winnerArtifact("o1")); q.Quality != p4offline.QualityDescriptiveOnly ||
			!containsString(q.Reasons, "P3B_NOT_DETERMINATE:UNKNOWN_INPUT") {
			t.Fatalf("an UNKNOWN_INPUT on either side is not scorable: %+v", q)
		}
		// A genuine P2 skip: 5% of a balance of 100 is 5, below the minimum
		// of 10. The policy computed a choice on the way; it bet on nothing.
		sk := newSynth()
		sk.skippedAttempt("r1", "e1", 1)
		dsSkip := sk.dataset()
		fsSkip, err := p4offline.BuildCommonFactset(preparedDS(dsSkip), singleEpisode(t, mustSelect(t, dsSkip)).Episode)
		if err != nil {
			t.Fatal(err)
		}
		p2skip, err := p4offline.EvaluateP2Case(fsSkip)
		if err != nil {
			t.Fatal(err)
		}
		skipDec := decisionOf(t, p2skip, fsSkip)
		p3bSkipCase, err := p4offline.EvaluateP3bCase(fsSkip, rs, synthCoords(fsSkip, 0))
		if err != nil {
			t.Fatal(err)
		}
		if skipDec.Action.Class != p4offline.ActionPolicySkip || !skipDec.Choice.Present {
			t.Fatalf("fixture: a skip with a computed choice, got %+v", skipDec)
		}
		if q := p4offline.AssessCaseQuality(preparedDS(dsSkip), fsSkip, skipDec, decisionOf(t, p3bSkipCase, fsSkip), winnerArtifact("o1")); q.Quality != p4offline.QualityPrimaryScorable ||
			len(q.Reasons) != 0 {
			t.Fatalf("a skip is a per-policy non-member of the primary denominator, not a case downgrade: %+v", q)
		}
		noneRs := mustVerify(t, rulesetFrom(t, cfgDefaultOnly("none", 95, 100)))
		p3bNone, err := p4offline.EvaluateP3bCase(fs, noneRs, synthCoords(fs, 0))
		if err != nil {
			t.Fatal(err)
		}
		none := decisionOf(t, p3bNone, fs)
		if q := p4offline.AssessCaseQuality(preparedDS(ds), fs, p2dec, none, winnerArtifact("o1")); q.Quality != p4offline.QualityPrimaryScorable ||
			len(q.Reasons) != 0 {
			t.Fatalf("a no-attempt prefix is a per-policy non-member of the primary denominator, not a case downgrade: %+v", q)
		}
		// A hand-built P3b decision claiming the factual choice and stake
		// under a fabricated mapping is not derived, however well bound.
		forged := p4offline.PolicyDecision{Policy: p4offline.PolicyP3b, Attempt: fs.Attempt, FactsetDigest: fs.Digest, EventID: "e1",
			CutoffPosition: fs.CutoffPosition, OutcomeIDs: []string{"o1", "o2"},
			Action: p4offline.ActionMapping{MapVersion: p4offline.NativeActionMapVersion, Policy: p4offline.PolicyP3b,
				NativeAction: "ORDERED_RULES_ATTEMPT", Class: p4offline.ActionWouldAttempt, Legal: true},
			Choice: p2dec.Choice, Stake: p2dec.Stake}
		if q := p4offline.AssessCaseQuality(preparedDS(ds), fs, p2dec, forged, winnerArtifact("o1")); q.Quality != p4offline.QualityExcluded ||
			!containsString(q.Reasons, "P3B_DECISION_NOT_DERIVED") {
			t.Fatalf("%+v", q)
		}
		unbound := p3bdec
		unbound.FactsetDigest = "other"
		if q := p4offline.AssessCaseQuality(preparedDS(ds), fs, p2dec, unbound, winnerArtifact("o1")); q.Quality != p4offline.QualityExcluded ||
			!containsString(q.Reasons, "POLICY_BINDING_MISMATCH") {
			t.Fatalf("a decision bound to another factset is not this case's evidence: %+v", q)
		}
		swapped := p4offline.AssessCaseQuality(preparedDS(ds), fs, p3bdec, p2dec, winnerArtifact("o1"))
		if swapped.Quality != p4offline.QualityExcluded {
			t.Fatalf("the two policies are positional: %+v", swapped)
		}
		mismatched := p2dec
		mismatched.Choice.OutcomeID = "o2"
		if q := p4offline.AssessCaseQuality(preparedDS(ds), fs, mismatched, p3bdec, winnerArtifact("o1")); q.Quality != p4offline.QualityExcluded {
			t.Fatalf("an index/identity disagreement is not a usable decision: %+v", q)
		}
	})
	t.Run("episode and factset verdicts", func(t *testing.T) {
		// The same facts plus a manual call after the cutoff: the factset the
		// dataset derives is byte-identical (post-cutoff facts do not move
		// it), and the episode is excluded whole.
		s := newSynth()
		s.placedAttempt("r1", "e1", 1)
		s.manualCall("r1", "e1", 30, 1)
		dsManual := s.dataset()
		q := p4offline.AssessCaseQuality(preparedDS(dsManual), fs, p2dec, p3bdec, winnerArtifact("o1"))
		if q.Quality != p4offline.QualityExcluded || !containsString(q.Reasons, "EPISODE_EXCLUDED") || !containsString(q.Reasons, "MANUAL_INTERVENTION") {
			t.Fatalf("%+v", q)
		}
		if q := p4offline.AssessCaseQuality(preparedDS(predictioneval.SourceDataset{}), fs, p2dec, p3bdec, winnerArtifact("o1")); q.Quality != p4offline.QualityExcluded {
			t.Fatalf("no dataset, no episode: %+v", q)
		}
		_, _, other := selectedCase(t, func(e *predictioneval.SourceDecisionEnvelope) { e.Balance = ptrI64(2000) }, nil)
		otherDec := p2dec
		otherDec.FactsetDigest = other.Digest
		if q := p4offline.AssessCaseQuality(preparedDS(ds), other, otherDec, asP3bAttempt(otherDec), winnerArtifact("o1")); q.Quality != p4offline.QualityExcluded ||
			!containsString(q.Reasons, "FACTSET_BINDING_MISMATCH") {
			t.Fatalf("a factset the dataset does not derive is not this dataset's evidence: %+v", q)
		}
		tamperedFs := fs
		tamperedFs.Balance++
		if q := p4offline.AssessCaseQuality(preparedDS(ds), tamperedFs, p2dec, p3bdec, winnerArtifact("o1")); q.Quality != p4offline.QualityExcluded ||
			!containsString(q.Reasons, "FACTSET_DIGEST_MISMATCH") {
			t.Fatalf("%+v", q)
		}
		// An unverifiable factset's completeness label is not this package's:
		// it is never echoed into the reasons.
		relabelled := fs
		relabelled.Completeness = "GARBAGE"
		qr := p4offline.AssessCaseQuality(preparedDS(ds), relabelled, p2dec, p3bdec, winnerArtifact("o1"))
		if qr.Quality != p4offline.QualityExcluded || !containsString(qr.Reasons, "FACTSET_DIGEST_MISMATCH") {
			t.Fatalf("%+v", qr)
		}
		for _, r := range qr.Reasons {
			if strings.Contains(r, "GARBAGE") {
				t.Fatalf("an unverified label reached the reasons: %+v", qr)
			}
		}
	})
	t.Run("resolution verdicts", func(t *testing.T) {
		tampered := winnerArtifact("o1")
		tampered.WinnerOutcomeID = "o2"
		if q := p4offline.AssessCaseQuality(preparedDS(ds), fs, p2dec, p3bdec, tampered); q.Quality != p4offline.QualityExcluded ||
			!containsString(q.Reasons, "RESOLUTION_DIGEST_MISMATCH") {
			t.Fatalf("a tampered artifact is not evidence: %+v", q)
		}
		if q := p4offline.AssessCaseQuality(preparedDS(ds), fs, p2dec, p3bdec, forgedWinnerArtifact("o1")); q.Quality != p4offline.QualityExcluded ||
			!containsString(q.Reasons, "RESOLUTION_NOT_DERIVABLE") {
			t.Fatalf("a consistently digested but unproven winner is not evidence: %+v", q)
		}
		wrongRound := goodWinnerEvidence()
		wrongRound.Round.EventID = "e2"
		wrongRound.EvidenceReferences[0].EventID = "e2"
		if q := p4offline.AssessCaseQuality(preparedDS(ds), fs, p2dec, p3bdec, p4offline.ProjectResolution(wrongRound)); q.Quality != p4offline.QualityExcluded {
			t.Fatalf("%+v", q)
		}
		wrongSet := goodWinnerEvidence()
		wrongSet.OrderedOutcomeIDs = []string{"o1", "o2", "o3"}
		if q := p4offline.AssessCaseQuality(preparedDS(ds), fs, p2dec, p3bdec, p4offline.ProjectResolution(wrongSet)); q.Quality != p4offline.QualityExcluded ||
			!containsString(q.Reasons, "RESOLUTION_OUTCOME_SET_MISMATCH") {
			t.Fatalf("a resolution over another outcome set is not this round's: %+v", q)
		}
	})
}
