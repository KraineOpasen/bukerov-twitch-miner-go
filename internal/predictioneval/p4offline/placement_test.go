package p4offline_test

// SEAM 9: evidence-only placement.
//
// A placement is attributed to a policy only when the policy's decision IS
// the factual decision — same attempt, same outcome, same stake — and even
// then a local CALL_RETURNED with a nil error proves only that the local call
// returned, not that the platform accepted it.

import (
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval/p4offline"
)

// factualCase builds a placed attempt with the given post-decision shape and
// returns the dataset, its factset, the factual placement and the P2 result.
func factualCase(t *testing.T, shape func(*synth, *predictioneval.SourceDecisionEnvelope)) (predictioneval.SourceDataset, p4offline.CommonFactset, p4offline.FactualPlacement, p4offline.P2CaseResult) {
	t.Helper()
	s := newSynth()
	s.due("r1", "e1", 1)
	env := synthPlacedEnvelope()
	s.terminal("r1", "e1", 1, predictioneval.PhaseAutoDecided, "OK", env)
	shape(s, env)
	ds := s.dataset()
	ep := singleEpisode(t, mustSelect(t, ds))
	if ep.Excluded {
		t.Fatalf("%v", ep.ExclusionReasons)
	}
	fs, err := p4offline.BuildCommonFactset(ds, ep.Episode)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := p4offline.ProjectFactualPlacement(ds, fs)
	if err != nil {
		t.Fatalf("ProjectFactualPlacement: %v", err)
	}
	p2, err := p4offline.EvaluateP2Case(fs)
	if err != nil {
		t.Fatal(err)
	}
	return ds, fs, fp, p2
}

func coherentCall(s *synth, env *predictioneval.SourceDecisionEnvelope) {
	s.call("r1", "e1", 1, *env.FinalAmount, *env.ChoiceIndex)
}

func validProof(fp p4offline.FactualPlacement) *p4offline.PlatformAcceptanceProof {
	return &p4offline.PlatformAcceptanceProof{
		Basis:             p4offline.ProofBasisPlatformPredictionConfirmed,
		CallObservationID: fp.CallStartedObservationID,
		Reference: p4offline.EvidenceReference{ObservationID: "confirmed-1", Kind: "user_prediction_made",
			Phase: "PLACEMENT_CONFIRMED", EventID: "e1"},
		ProofRevision: "test-proof/v1",
	}
}

// TestLocalReturnDoesNotProvePlatformAcceptance pins the central rule and
// the shapes around it.
func TestLocalReturnDoesNotProvePlatformAcceptance(t *testing.T) {
	_, fs, fp, p2 := factualCase(t, coherentCall)
	if fp.Coherence != predictioneval.PlacementShapeCoherent || !fp.CallPresent || !fp.Returned || !fp.LocalReasonOK ||
		fp.Stake == nil || *fp.Stake != 50 || fp.Slot == nil || *fp.Slot != 0 || fp.CallStartedPosition <= fp.CutoffPosition ||
		fp.FactsetDigest != fs.Digest || fp.TerminalDecision != "PLACE" || fp.Attempt != fs.Attempt || fp.EventID != "e1" {
		t.Fatalf("%+v", fp)
	}
	dec := decisionOf(t, p2, fs)
	if dec.Policy != p4offline.PolicyP2 || dec.Action.Class != p4offline.ActionWouldAttempt || dec.FactsetDigest != fs.Digest ||
		dec.Attempt != fs.Attempt || dec.CutoffPosition != fs.CutoffPosition || dec.EventID != "e1" ||
		len(dec.OutcomeIDs) != 2 || dec.OutcomeIDs[0] != "o1" || dec.OutcomeIDs[1] != "o2" {
		t.Fatalf("%+v", dec)
	}

	t.Run("local OK without a platform proof is unproven", func(t *testing.T) {
		pl := p4offline.DerivePlacement(dec, fp, nil)
		if pl.ContractVersion != p4offline.PlacementEvidenceVersion || pl.Status != p4offline.PlacementLocalOKPlatformUnproven {
			t.Fatalf("%+v", pl)
		}
		if !pl.PolicyStake.Known() || pl.PolicyStake.Value != 50 || pl.AttributedOutcomeID != "o1" ||
			pl.AttributedCallObservationID != fp.CallStartedObservationID || !pl.ContributesToBetOnlyDenominator {
			t.Fatalf("%+v", pl)
		}
		if pl.Attempt != fs.Attempt || pl.FactsetDigest != fs.Digest || pl.EventID != "e1" {
			t.Fatalf("the placement verdict must carry its case: %+v", pl)
		}
	})
	t.Run("a valid platform proof bound to the call proves acceptance", func(t *testing.T) {
		pl := p4offline.DerivePlacement(dec, fp, validProof(fp))
		if pl.Status != p4offline.PlacementAcceptedPlatformProven {
			t.Fatalf("%+v", pl)
		}
	})
	t.Run("a proof for another call does not transfer", func(t *testing.T) {
		proof := validProof(fp)
		proof.CallObservationID = "some-other-call"
		pl := p4offline.DerivePlacement(dec, fp, proof)
		if pl.Status != p4offline.PlacementLocalOKPlatformUnproven || !containsString(pl.Reasons, "PROOF_NOT_BOUND_TO_CALL") {
			t.Fatalf("%+v", pl)
		}
	})
	t.Run("a proof on a foreign basis does not count", func(t *testing.T) {
		proof := validProof(fp)
		proof.Basis = "LOCAL_RETURN_WAS_OK"
		pl := p4offline.DerivePlacement(dec, fp, proof)
		if pl.Status != p4offline.PlacementLocalOKPlatformUnproven || !containsString(pl.Reasons, "PROOF_BASIS_NOT_ACCEPTED") {
			t.Fatalf("%+v", pl)
		}
	})
	t.Run("a proof from another round does not count", func(t *testing.T) {
		proof := validProof(fp)
		proof.Reference.EventID = "e2"
		pl := p4offline.DerivePlacement(dec, fp, proof)
		if pl.Status != p4offline.PlacementLocalOKPlatformUnproven || !containsString(pl.Reasons, "PROOF_ROUND_MISMATCH") {
			t.Fatalf("%+v", pl)
		}
	})
	t.Run("a local error leaves the platform unknown", func(t *testing.T) {
		_, fs2, fp2, p22 := factualCase(t, func(s *synth, env *predictioneval.SourceDecisionEnvelope) {
			s.placement("r1", "e1", 1, predictioneval.PhaseCallStarted, 50, 0, "OK", "NONE")
			s.placement("r1", "e1", 1, predictioneval.PhaseCallReturned, 50, 0, "REJECTED", "TRANSPORT")
		})
		pl := p4offline.DerivePlacement(decisionOf(t, p22, fs2), fp2, validProof(fp2))
		if pl.Status != p4offline.PlacementLocalErrorPlatformUnknown {
			t.Fatalf("even with a proof supplied, a local error is not turned into acceptance: %+v", pl)
		}
	})
	t.Run("a started call that never returned", func(t *testing.T) {
		_, fs2, fp2, p22 := factualCase(t, func(s *synth, env *predictioneval.SourceDecisionEnvelope) {
			s.placement("r1", "e1", 1, predictioneval.PhaseCallStarted, 50, 0, "OK", "NONE")
		})
		if fp2.Coherence != predictioneval.PlacementShapeIncoherent || !fp2.StartedOnly {
			t.Fatalf("the P2 projection calls a half pair incoherent; the started-only shape must be kept apart: %+v", fp2)
		}
		pl := p4offline.DerivePlacement(decisionOf(t, p22, fs2), fp2, validProof(fp2))
		if pl.Status != p4offline.PlacementNotReturned || pl.AttributedCallObservationID != fp2.CallStartedObservationID {
			t.Fatalf("a proof cannot turn an unreturned call into an accepted one: %+v", pl)
		}
	})
	t.Run("a placing decision with no call recorded", func(t *testing.T) {
		_, fs2, fp2, p22 := factualCase(t, func(*synth, *predictioneval.SourceDecisionEnvelope) {})
		pl := p4offline.DerivePlacement(decisionOf(t, p22, fs2), fp2, nil)
		if pl.Status != p4offline.PlacementNotRecorded || pl.AttributedCallObservationID != "" ||
			!pl.PolicyStake.Known() || pl.PolicyStake.Value != 50 {
			t.Fatalf("no call to attribute, the policy's own stake still reported: %+v", pl)
		}
	})
	t.Run("an incoherent call pair", func(t *testing.T) {
		_, fs2, fp2, p22 := factualCase(t, func(s *synth, env *predictioneval.SourceDecisionEnvelope) {
			s.placement("r1", "e1", 1, predictioneval.PhaseCallStarted, 50, 0, "OK", "NONE")
			s.placement("r1", "e1", 1, predictioneval.PhaseCallReturned, 60, 0, "OK", "NONE")
		})
		pl := p4offline.DerivePlacement(decisionOf(t, p22, fs2), fp2, nil)
		if pl.Status != p4offline.PlacementIncoherent {
			t.Fatalf("%+v", pl)
		}
	})
}

// TestCounterfactualDecisionsCannotInheritTheFactualPlacement pins the
// attribution rule across choice, stake and case identity.
func TestCounterfactualDecisionsCannotInheritTheFactualPlacement(t *testing.T) {
	_, fs, fp, p2 := factualCase(t, coherentCall)
	same := decisionOf(t, p2, fs)
	rs := mustVerify(t, rulesetFrom(t, cfgWithRule("same", predictioneval.ComparatorGe, 50, 100)))
	p3b, err := p4offline.EvaluateP3bCase(fs, rs, "key", 0)
	if err != nil {
		t.Fatal(err)
	}
	other := decisionOf(t, p3b, fs)
	if other.Policy != p4offline.PolicyP3b || other.Action.Class != p4offline.ActionWouldAttempt || other.Choice.OutcomeID != "o1" ||
		!other.Stake.Known() || other.Stake.Value != 100 {
		t.Fatalf("fixture: %+v", other)
	}
	t.Run("a different stake on the same outcome", func(t *testing.T) {
		pl := p4offline.DerivePlacement(other, fp, validProof(fp))
		if pl.Status != p4offline.PlacementCounterfactualNotInheritable || !containsString(pl.Reasons, "STAKE_DIFFERS") ||
			pl.AttributedCallObservationID != "" {
			t.Fatalf("%+v", pl)
		}
		if !pl.PolicyStake.Known() || pl.PolicyStake.Value != 100 {
			t.Fatalf("the policy's own stake stays reported; only the factual call is withheld: %+v", pl.PolicyStake)
		}
	})
	t.Run("a different outcome at the same stake", func(t *testing.T) {
		// o1 holds 60%, o2 40%: a Le-50 rule admits o2 at 5% of 1000 = 50,
		// the factual stake on the other outcome.
		rs := mustVerify(t, rulesetFrom(t, cfgWithRulePoints("pick-o2", predictioneval.ComparatorLe, 50, 100, 5)))
		res, err := p4offline.EvaluateP3bCase(fs, rs, "key", 0)
		if err != nil {
			t.Fatal(err)
		}
		d := decisionOf(t, res, fs)
		if d.Choice.OutcomeID != "o2" || !d.Stake.Known() || d.Stake.Value != 50 {
			t.Fatalf("fixture: %+v %+v", d.Choice, d.Stake)
		}
		pl := p4offline.DerivePlacement(d, fp, validProof(fp))
		if pl.Status != p4offline.PlacementCounterfactualNotInheritable || !containsString(pl.Reasons, "CHOICE_DIFFERS") {
			t.Fatalf("%+v", pl)
		}
	})
	t.Run("an edited or decoded decision is not derived", func(t *testing.T) {
		edits := map[string]func(*p4offline.PolicyDecision){
			"another consistent choice": func(d *p4offline.PolicyDecision) {
				d.Choice = p4offline.PolicyChoice{Present: true, Index: 1, OutcomeID: "o2"}
			},
			"another stake":         func(d *p4offline.PolicyDecision) { d.Stake = p4offline.KnownInt64(50) },
			"another native action": func(d *p4offline.PolicyDecision) { d.Action.NativeAction = "ORDERED_RULES_ATTEMPT" },
			"another cutoff":        func(d *p4offline.PolicyDecision) { d.CutoffPosition++ },
		}
		for name, edit := range edits {
			d := other
			edit(&d)
			pl := p4offline.DerivePlacement(d, fp, validProof(fp))
			if pl.Status != p4offline.PlacementUnknown || !containsString(pl.Reasons, "DECISION_NOT_DERIVED") ||
				pl.AttributedCallObservationID != "" || pl.ContributesToBetOnlyDenominator {
				t.Fatalf("%s: %+v", name, pl)
			}
		}
		var decoded p4offline.PolicyDecision
		raw, _ := json.Marshal(same)
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if pl := p4offline.DerivePlacement(decoded, fp, validProof(fp)); !containsString(pl.Reasons, "DECISION_NOT_DERIVED") {
			t.Fatalf("a decision read back from storage must be re-derived, not trusted: %+v", pl)
		}
		forged := p4offline.PolicyDecision{Policy: p4offline.PolicyP3b, Attempt: fs.Attempt, FactsetDigest: fs.Digest, EventID: "e1",
			CutoffPosition: fs.CutoffPosition, OutcomeIDs: []string{"o1", "o2"},
			Action: p4offline.ActionMapping{MapVersion: p4offline.NativeActionMapVersion, Policy: p4offline.PolicyP3b,
				NativeAction: "ORDERED_RULES_ATTEMPT", Class: p4offline.ActionWouldAttempt, Legal: true},
			Choice: same.Choice, Stake: same.Stake}
		if pl := p4offline.DerivePlacement(forged, fp, validProof(fp)); pl.Status != p4offline.PlacementUnknown ||
			!containsString(pl.Reasons, "DECISION_NOT_DERIVED") {
			t.Fatalf("a hand-built P3b decision claiming the factual choice and stake inherits nothing: %+v", pl)
		}
	})
	t.Run("a choice whose index and identity disagree settles nothing", func(t *testing.T) {
		d := same
		d.Choice = p4offline.PolicyChoice{Present: true, Index: 0, OutcomeID: "o2"}
		pl := p4offline.DerivePlacement(d, fp, validProof(fp))
		if pl.Status != p4offline.PlacementUnknown || !containsString(pl.Reasons, "CHOICE_INDEX_ID_MISMATCH") ||
			pl.AttributedCallObservationID != "" || pl.ContributesToBetOnlyDenominator {
			t.Fatalf("slot 0 is o1; a decision naming o2 at slot 0 cannot inherit slot 0's call: %+v", pl)
		}
		d.Choice = p4offline.PolicyChoice{Present: true, Index: 7, OutcomeID: "o1"}
		if pl := p4offline.DerivePlacement(d, fp, nil); !containsString(pl.Reasons, "CHOICE_INDEX_ID_MISMATCH") {
			t.Fatalf("an index outside the vector: %+v", pl)
		}
	})
	t.Run("an edited, decoded or hand-built factual placement is not derived", func(t *testing.T) {
		edits := map[string]func(*p4offline.FactualPlacement){
			"another attempt's call": func(f *p4offline.FactualPlacement) {
				f.Attempt.AttemptID = 2
				f.CallStartedPosition = fp.CallStartedPosition + 10
				f.CallStartedObservationID = "call-of-attempt-2"
			},
			"a call moved before the cutoff": func(f *p4offline.FactualPlacement) { f.CallStartedPosition = fp.CutoffPosition },
			"another factset":                func(f *p4offline.FactualPlacement) { f.FactsetDigest = "other" },
			"another round":                  func(f *p4offline.FactualPlacement) { f.EventID = "e2" },
			"a local error erased":           func(f *p4offline.FactualPlacement) { f.LocalReasonOK = true; f.ErrorClass = "" },
		}
		for name, edit := range edits {
			edited := fp
			edit(&edited)
			pl := p4offline.DerivePlacement(same, edited, validProof(edited))
			if pl.Status != p4offline.PlacementUnknown || !containsString(pl.Reasons, "FACTUAL_PLACEMENT_NOT_DERIVED") ||
				pl.AttributedCallObservationID != "" {
				t.Fatalf("%s: %+v", name, pl)
			}
		}
		var decoded p4offline.FactualPlacement
		raw, _ := json.Marshal(fp)
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if pl := p4offline.DerivePlacement(same, decoded, validProof(decoded)); !containsString(pl.Reasons, "FACTUAL_PLACEMENT_NOT_DERIVED") {
			t.Fatalf("a factual placement read back from storage must be re-derived, not trusted: %+v", pl)
		}
		if pl := p4offline.DerivePlacement(same, p4offline.FactualPlacement{}, nil); !containsString(pl.Reasons, "FACTUAL_PLACEMENT_NOT_DERIVED") {
			t.Fatalf("%+v", pl)
		}
	})
	t.Run("a factual placement of another case", func(t *testing.T) {
		// Case B: the same public round in another collector session with an
		// identical, coherent call. Its factual placement is genuine — and it
		// is not case A's.
		s := newSynth()
		s.session = "p4-synth-session-b"
		s.source.CollectorSessionID = s.session
		s.placedAttempt("r1", "e1", 1)
		dsB := s.dataset()
		fsB, err := p4offline.BuildCommonFactset(dsB, singleEpisode(t, mustSelect(t, dsB)).Episode)
		if err != nil {
			t.Fatal(err)
		}
		fpB, err := p4offline.ProjectFactualPlacement(dsB, fsB)
		if err != nil {
			t.Fatal(err)
		}
		pl := p4offline.DerivePlacement(same, fpB, validProof(fpB))
		if pl.Status != p4offline.PlacementUnknown || !containsString(pl.Reasons, "CASE_BINDING_MISMATCH") {
			t.Fatalf("case B's call is not case A's, whatever it looks like: %+v", pl)
		}
	})
	t.Run("a P3b decision equal to the factual one inherits the factual call", func(t *testing.T) {
		// A Ge-50 rule admits o1 at 5% of 1000 = 50: the factual decision.
		rs := mustVerify(t, rulesetFrom(t, cfgWithRulePoints("same-as-factual", predictioneval.ComparatorGe, 50, 100, 5)))
		res, err := p4offline.EvaluateP3bCase(fs, rs, "key", 0)
		if err != nil {
			t.Fatal(err)
		}
		d := decisionOf(t, res, fs)
		if d.Choice.OutcomeID != "o1" || !d.Stake.Known() || d.Stake.Value != 50 {
			t.Fatalf("fixture: %+v %+v", d.Choice, d.Stake)
		}
		pl := p4offline.DerivePlacement(d, fp, nil)
		if pl.Status != p4offline.PlacementLocalOKPlatformUnproven || pl.Policy != p4offline.PolicyP3b ||
			pl.AttributedCallObservationID != fp.CallStartedObservationID {
			t.Fatalf("%+v", pl)
		}
	})
	t.Run("a decision that is not legal or not bound settles nothing", func(t *testing.T) {
		illegal := same
		illegal.Action.Legal = false
		illegal.Action.Class = p4offline.ActionUnsupportedShape
		if pl := p4offline.DerivePlacement(illegal, fp, validProof(fp)); pl.Status != p4offline.PlacementUnknown ||
			!containsString(pl.Reasons, "ILLEGAL_NATIVE_SHAPE") || pl.ContributesToBetOnlyDenominator {
			t.Fatalf("%+v", pl)
		}
		foreignMap := same
		foreignMap.Action.Policy = p4offline.PolicyP3b
		if pl := p4offline.DerivePlacement(foreignMap, fp, nil); !containsString(pl.Reasons, "POLICY_BINDING_MISMATCH") {
			t.Fatalf("%+v", pl)
		}
		unnamed := same
		unnamed.Policy = ""
		unnamed.Action.Policy = ""
		if pl := p4offline.DerivePlacement(unnamed, fp, nil); !containsString(pl.Reasons, "POLICY_BINDING_MISMATCH") {
			t.Fatalf("an empty policy is not a policy: %+v", pl)
		}
		if pl := p4offline.DerivePlacement(p4offline.PolicyDecision{}, fp, nil); pl.Status != p4offline.PlacementUnknown || len(pl.Reasons) == 0 {
			t.Fatalf("the zero decision settles nothing: %+v", pl)
		}
	})
}

// TestPolicyResultsBindOnlyToTheirOwnFactset pins Decision: a result carries
// the digest it was evaluated over, and a different factset — even one of
// the same attempt — is refused.
func TestPolicyResultsBindOnlyToTheirOwnFactset(t *testing.T) {
	_, fs, _, p2 := factualCase(t, coherentCall)
	_, _, other := selectedCase(t, func(e *predictioneval.SourceDecisionEnvelope) { e.Balance = ptrI64(2000) }, nil)
	if other.Attempt != fs.Attempt || other.Digest == fs.Digest {
		t.Fatalf("fixture: the two factsets must share an attempt and differ in digest")
	}
	if _, err := p2.Decision(other); !errors.Is(err, p4offline.ErrDecisionBinding) {
		t.Fatalf("got %v", err)
	}
	if _, err := p2.Decision(p4offline.CommonFactset{}); !errors.Is(err, p4offline.ErrFactsetDigest) {
		t.Fatalf("got %v", err)
	}
	rs := mustVerify(t, rulesetFrom(t, cfgWithRule("bind", predictioneval.ComparatorGe, 50, 100)))
	p3b, err := p4offline.EvaluateP3bCase(fs, rs, "key", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p3b.Decision(other); !errors.Is(err, p4offline.ErrDecisionBinding) {
		t.Fatalf("got %v", err)
	}
	forged := p3b
	forged.Action.Policy = p4offline.PolicyP2
	if _, err := forged.Decision(fs); !errors.Is(err, p4offline.ErrDecisionBinding) {
		t.Fatalf("a mapping of the other policy: %v", err)
	}
}

// TestPolicySkipAndNoAttemptPrefixPlacementsAreDistinct pins NOT_APPLICABLE
// against UNKNOWN, and the exact zero a skip must carry.
func TestPolicySkipAndNoAttemptPrefixPlacementsAreDistinct(t *testing.T) {
	_, fs, fp, _ := factualCase(t, coherentCall)
	// A genuine P2 skip: 5% of a balance of 100 is 5, below the minimum of 10.
	dsS, fsS, fpS, p2S := skippedCase(t)
	skip := decisionOf(t, p2S, fsS)
	if skip.Action.Class != p4offline.ActionPolicySkip || !skip.Stake.Known() || skip.Stake.Value != 0 {
		t.Fatalf("fixture: %+v %+v", skip.Action, skip.Stake)
	}
	pl := p4offline.DerivePlacement(skip, fpS, nil)
	if pl.Status != p4offline.PlacementNotApplicable || pl.ContributesToBetOnlyDenominator ||
		!pl.PolicyStake.Known() || pl.PolicyStake.Value != 0 || pl.AttributedCallObservationID != "" {
		t.Fatalf("a skip has no placement and an exact zero stake: %+v", pl)
	}
	_ = dsS
	t.Run("a skip whose stake is not the exact zero settles nothing", func(t *testing.T) {
		wrong := skip
		wrong.Stake = p4offline.KnownInt64(50)
		if pl := p4offline.DerivePlacement(wrong, fpS, nil); pl.Status != p4offline.PlacementUnknown ||
			!containsString(pl.Reasons, "POLICY_SKIP_STAKE_NOT_EXACT_ZERO") {
			t.Fatalf("%+v", pl)
		}
		wrong.Stake = p4offline.UnknownInt64("nobody knows")
		if pl := p4offline.DerivePlacement(wrong, fpS, nil); pl.Status != p4offline.PlacementUnknown ||
			!containsString(pl.Reasons, "POLICY_SKIP_STAKE_NOT_EXACT_ZERO") {
			t.Fatalf("an UNKNOWN stake is not turned into a zero by the class name: %+v", pl)
		}
	})
	rs := mustVerify(t, rulesetFrom(t, cfgDefaultOnly("none", 95, 100)))
	p3b, err := p4offline.EvaluateP3bCase(fs, rs, "key", 0)
	if err != nil {
		t.Fatal(err)
	}
	none := decisionOf(t, p3b, fs)
	if none.Action.Class != p4offline.ActionNoAttemptInSuppliedPrefix {
		t.Fatalf("fixture: %+v", none)
	}
	pl = p4offline.DerivePlacement(none, fp, validProof(fp))
	if pl.Status != p4offline.PlacementUnknown || pl.PolicyStake.Presence != p4offline.PresenceUnknown ||
		!containsString(pl.Reasons, string(p4offline.ActionNoAttemptInSuppliedPrefix)) || pl.ContributesToBetOnlyDenominator {
		t.Fatalf("NO_ATTEMPT_IN_SUPPLIED_PREFIX is not NOT_APPLICABLE and carries no zero: %+v", pl)
	}
	// A genuine PARTICIPATION_ADMITTED_STAKE_UNKNOWN: a balance past u32.
	dsU, _, fsU := selectedCase(t, func(e *predictioneval.SourceDecisionEnvelope) { e.Balance = ptrI64(math.MaxUint32 + 1) }, nil)
	fpU, err := p4offline.ProjectFactualPlacement(dsU, fsU)
	if err != nil {
		t.Fatal(err)
	}
	one := mustVerify(t, rulesetFrom(t, cfgWithRule("one", predictioneval.ComparatorGe, 50, 100)))
	p3bU, err := p4offline.EvaluateP3bCase(fsU, one, "key", 0)
	if err != nil {
		t.Fatal(err)
	}
	unknown := decisionOf(t, p3bU, fsU)
	if unknown.Action.Class != p4offline.ActionParticipationAdmittedStakeUnknown || !unknown.Choice.Present {
		t.Fatalf("fixture: %+v", unknown)
	}
	pl = p4offline.DerivePlacement(unknown, fpU, nil)
	if pl.Status != p4offline.PlacementUnknown || pl.PolicyStake.Known() ||
		!containsString(pl.Reasons, string(p4offline.ActionParticipationAdmittedStakeUnknown)) {
		t.Fatalf("%+v", pl)
	}
	if _, err := p4offline.ProjectFactualPlacement(predictioneval.SourceDataset{}, fs); !errors.Is(err, p4offline.ErrEpisodeNotSelected) {
		t.Fatalf("got %v", err)
	}
}

// skippedCase builds a genuine P2 skip (balance 100, BELOW_MINIMUM_POINTS)
// and returns the dataset, its factset, the factual placement and the P2
// result.
func skippedCase(t *testing.T) (predictioneval.SourceDataset, p4offline.CommonFactset, p4offline.FactualPlacement, p4offline.P2CaseResult) {
	t.Helper()
	s := newSynth()
	s.skippedAttempt("r1", "e1", 1)
	ds := s.dataset()
	fs, err := p4offline.BuildCommonFactset(ds, singleEpisode(t, mustSelect(t, ds)).Episode)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := p4offline.ProjectFactualPlacement(ds, fs)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := p4offline.EvaluateP2Case(fs)
	if err != nil {
		t.Fatal(err)
	}
	return ds, fs, fp, p2
}
