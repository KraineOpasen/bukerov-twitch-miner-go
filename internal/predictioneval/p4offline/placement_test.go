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
	"fmt"
	"math"
	"runtime"
	"strings"
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
	fs, err := p4offline.BuildCommonFactset(preparedDS(ds), ep.Episode)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := p4offline.ProjectFactualPlacement(preparedDS(ds), fs)
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
			pl.AttributedCallObservationID != fp.CallStartedObservationID {
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
	t.Run("a started call that never returned is refused before a placement exists", func(t *testing.T) {
		// OWNER DISPOSITION (D16). PLACEMENT_NOT_RETURNED is NOT an admissible
		// reachable derived outcome of this pipeline. P4 admits only
		// COMPLETE + AS_FINALIZED sources, and such a source cannot carry a
		// factual automatic CALL_STARTED without its CALL_RETURNED, so an
		// unspent start is a contradiction in the EVIDENCE rather than a
		// placement result to report.
		//
		// This used to be the positive fixture for that status: it built a
		// started-only case, asserted it was scorable, and asserted the seam
		// answered NOT_RETURNED. It is now the negative proof along the trusted
		// path, which is the whole point of the disposition — the shape never
		// reaches the placement seam at all.
		s := newSynth()
		s.due("r1", "e1", 1)
		env := synthPlacedEnvelope()
		s.terminal("r1", "e1", 1, predictioneval.PhaseAutoDecided, "OK", env)
		s.placement("r1", "e1", 1, predictioneval.PhaseCallStarted, *env.FinalAmount, *env.ChoiceIndex, "OK", "NONE")
		ds := s.dataset()

		ep := singleEpisode(t, mustSelect(t, ds))
		if !ep.Excluded {
			t.Fatalf("an unspent automatic start must exclude the episode: %+v", ep)
		}
		if ep.Boundary.NoCallCoverage.Proven || ep.Boundary.Proven {
			t.Fatalf("the boundary cannot be proven over contradicted evidence: %+v", ep.Boundary)
		}
		// And nothing downstream can mint a factual placement from it. WHICH
		// seam refuses is asserted rather than swallowed: the bare `return`
		// that used to stand here made the ProjectFactualPlacement assertion
		// below unreachable on every run -- an independent lane instrumented
		// both branches and only the early one ever printed -- so no change to
		// ProjectFactualPlacement could have failed this subtest, and a future
		// change making BuildCommonFactset succeed here would have passed
		// silently.
		//
		// The factset seam is the one that refuses today, because an excluded
		// episode is not a selected opportunity. If that ever stops being
		// true, this fails and names it rather than skipping on.
		fs, err := p4offline.BuildCommonFactset(preparedDS(ds), ep.Episode)
		if err != nil {
			if !errors.Is(err, p4offline.ErrEpisodeNotSelected) {
				t.Fatalf("the factset seam must refuse an excluded episode as unselected, got %v", err)
			}
			return
		}
		t.Fatalf("an excluded episode must not yield a factset; the refusal moved and this test's own assertion has gone stale: %+v", fs)
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
	p3b, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, 0))
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
		res, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, 0))
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
				pl.AttributedCallObservationID != "" {
				t.Fatalf("%s: %+v", name, pl)
			}
		}
		var decoded p4offline.PolicyDecision
		raw := mustMarshal(t, same)
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
			pl.AttributedCallObservationID != "" {
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
			"a call moved before the cutoff":  func(f *p4offline.FactualPlacement) { f.CallStartedPosition = fp.CutoffPosition },
			"another factset":                 func(f *p4offline.FactualPlacement) { f.FactsetDigest = "other" },
			"another round":                   func(f *p4offline.FactualPlacement) { f.EventID = "e2" },
			"a local error erased":            func(f *p4offline.FactualPlacement) { f.LocalReasonOK = true; f.ErrorClass = "" },
			"the recorded identity rewritten": func(f *p4offline.FactualPlacement) { f.RecordedChoiceOutcomeID = "o2" },
			"the terminal slot rewritten":     func(f *p4offline.FactualPlacement) { f.RecordedTerminalSlot = ptrInt(1) },
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
		raw := mustMarshal(t, fp)
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
		fsB, err := p4offline.BuildCommonFactset(preparedDS(dsB), singleEpisode(t, mustSelect(t, dsB)).Episode)
		if err != nil {
			t.Fatal(err)
		}
		fpB, err := p4offline.ProjectFactualPlacement(preparedDS(dsB), fsB)
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
		res, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, 0))
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
			!containsString(pl.Reasons, "ILLEGAL_NATIVE_SHAPE") {
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
	p3b, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, 0))
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
	if pl.Status != p4offline.PlacementNotApplicable ||
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
	p3b, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, 0))
	if err != nil {
		t.Fatal(err)
	}
	none := decisionOf(t, p3b, fs)
	if none.Action.Class != p4offline.ActionNoAttemptInSuppliedPrefix {
		t.Fatalf("fixture: %+v", none)
	}
	pl = p4offline.DerivePlacement(none, fp, validProof(fp))
	if pl.Status != p4offline.PlacementUnknown || pl.PolicyStake.Presence != p4offline.PresenceUnknown ||
		!containsString(pl.Reasons, string(p4offline.ActionNoAttemptInSuppliedPrefix)) {
		t.Fatalf("NO_ATTEMPT_IN_SUPPLIED_PREFIX is not NOT_APPLICABLE and carries no zero: %+v", pl)
	}
	// A genuine PARTICIPATION_ADMITTED_STAKE_UNKNOWN: a balance past u32.
	dsU, _, fsU := selectedCase(t, func(e *predictioneval.SourceDecisionEnvelope) { e.Balance = ptrI64(math.MaxUint32 + 1) }, nil)
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
	if unknown.Action.Class != p4offline.ActionParticipationAdmittedStakeUnknown || !unknown.Choice.Present {
		t.Fatalf("fixture: %+v", unknown)
	}
	pl = p4offline.DerivePlacement(unknown, fpU, nil)
	if pl.Status != p4offline.PlacementUnknown || pl.PolicyStake.Known() ||
		!containsString(pl.Reasons, string(p4offline.ActionParticipationAdmittedStakeUnknown)) {
		t.Fatalf("%+v", pl)
	}
	if _, err := p4offline.ProjectFactualPlacement(preparedDS(predictioneval.SourceDataset{}), fs); !errors.Is(err, p4offline.ErrEpisodeNotSelected) {
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
	fs, err := p4offline.BuildCommonFactset(preparedDS(ds), singleEpisode(t, mustSelect(t, ds)).Episode)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := p4offline.ProjectFactualPlacement(preparedDS(ds), fs)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := p4offline.EvaluateP2Case(fs)
	if err != nil {
		t.Fatal(err)
	}
	return ds, fs, fp, p2
}

// TestRecordedChoiceIdentityMustMatchBeforeInheritance pins the factual
// decision's identity: when the store recorded which outcome the factual
// choice named, the policy's choice must name the same one, index and
// identity alike, to be THE factual decision. A recorded decision whose index
// agrees with the policy's but whose outcome does not is another decision,
// and the platform-proven call is not inherited across it.
func TestRecordedChoiceIdentityMustMatchBeforeInheritance(t *testing.T) {
	build := func(t *testing.T, recorded string) (p4offline.FactualPlacement, p4offline.PolicyDecision) {
		t.Helper()
		s := newSynth()
		s.due("r1", "e1", 1)
		env := synthPlacedEnvelope()
		env.ChoiceOutcomeID = recorded
		s.terminal("r1", "e1", 1, predictioneval.PhaseAutoDecided, "OK", env)
		s.call("r1", "e1", 1, *env.FinalAmount, *env.ChoiceIndex)
		ds := s.dataset()
		ep := singleEpisode(t, mustSelect(t, ds))
		if ep.Excluded {
			t.Fatalf("%v", ep.ExclusionReasons)
		}
		fs, err := p4offline.BuildCommonFactset(preparedDS(ds), ep.Episode)
		if err != nil {
			t.Fatal(err)
		}
		fp, err := p4offline.ProjectFactualPlacement(preparedDS(ds), fs)
		if err != nil {
			t.Fatal(err)
		}
		p2, err := p4offline.EvaluateP2Case(fs)
		if err != nil {
			t.Fatal(err)
		}
		dec := decisionOf(t, p2, fs)
		if !dec.Choice.Present || dec.Choice.Index != 0 || dec.Choice.OutcomeID != "o1" {
			t.Fatalf("fixture: %+v", dec.Choice)
		}
		if fp.RecordedChoiceOutcomeID != recorded {
			t.Fatalf("the factual placement carries the recorded identity %q, got %q", recorded, fp.RecordedChoiceOutcomeID)
		}
		return fp, dec
	}
	t.Run("a recorded identity that names another outcome is another decision", func(t *testing.T) {
		fp, dec := build(t, "o2")
		pl := p4offline.DerivePlacement(dec, fp, validProof(fp))
		if pl.Status != p4offline.PlacementCounterfactualNotInheritable || !containsString(pl.Reasons, "CHOICE_IDENTITY_DIFFERS") ||
			pl.AttributedCallObservationID != "" {
			t.Fatalf("the store says o2 at index 0; the policy's o1 at index 0 is not that decision and inherits nothing: %+v", pl)
		}
	})
	t.Run("the recorded identity that names the policy's outcome inherits", func(t *testing.T) {
		fp, dec := build(t, "o1")
		if pl := p4offline.DerivePlacement(dec, fp, validProof(fp)); pl.Status != p4offline.PlacementAcceptedPlatformProven {
			t.Fatalf("%+v", pl)
		}
	})
	t.Run("a store that recorded no identity settles on the index alone", func(t *testing.T) {
		fp, dec := build(t, "")
		if pl := p4offline.DerivePlacement(dec, fp, validProof(fp)); pl.Status != p4offline.PlacementAcceptedPlatformProven {
			t.Fatalf("%+v", pl)
		}
	})
}

// TestTerminalSlotMustMatchBeforeInheritance pins the second recorded
// identity of the factual choice: the slot the terminal fact itself named.
// The pinned producer writes it on every placing terminal fact, so a PLACE
// whose terminal fact names another slot — or none — is not the policy's
// decision, whatever the envelope's index says.
func TestTerminalSlotMustMatchBeforeInheritance(t *testing.T) {
	build := func(t *testing.T, terminalSlot *int) (p4offline.FactualPlacement, p4offline.PolicyDecision) {
		t.Helper()
		s := newSynth()
		s.due("r1", "e1", 1)
		env := synthPlacedEnvelope()
		s.terminal("r1", "e1", 1, predictioneval.PhaseAutoDecided, "OK", env)
		s.records[len(s.records)-1].Payload.OutcomeSlot = terminalSlot
		s.call("r1", "e1", 1, *env.FinalAmount, *env.ChoiceIndex)
		ds := s.dataset()
		ep := singleEpisode(t, mustSelect(t, ds))
		if ep.Excluded {
			t.Fatalf("%v", ep.ExclusionReasons)
		}
		fs, err := p4offline.BuildCommonFactset(preparedDS(ds), ep.Episode)
		if err != nil {
			t.Fatal(err)
		}
		fp, err := p4offline.ProjectFactualPlacement(preparedDS(ds), fs)
		if err != nil {
			t.Fatal(err)
		}
		p2, err := p4offline.EvaluateP2Case(fs)
		if err != nil {
			t.Fatal(err)
		}
		dec := decisionOf(t, p2, fs)
		if !dec.Choice.Present || dec.Choice.Index != 0 {
			t.Fatalf("fixture: %+v", dec.Choice)
		}
		if (fp.RecordedTerminalSlot == nil) != (terminalSlot == nil) || (terminalSlot != nil && *fp.RecordedTerminalSlot != *terminalSlot) {
			t.Fatalf("the factual placement carries the terminal fact's own slot %v, got %v", terminalSlot, fp.RecordedTerminalSlot)
		}
		return fp, dec
	}
	t.Run("a terminal fact naming another slot is another decision", func(t *testing.T) {
		fp, dec := build(t, ptrInt(1))
		pl := p4offline.DerivePlacement(dec, fp, validProof(fp))
		if pl.Status != p4offline.PlacementCounterfactualNotInheritable || !containsString(pl.Reasons, "TERMINAL_SLOT_DIFFERS") ||
			pl.AttributedCallObservationID != "" {
			t.Fatalf("the terminal fact names slot 1; the policy's slot 0 is not that decision and inherits nothing: %+v", pl)
		}
	})
	t.Run("a placing terminal fact without its slot is missing evidence", func(t *testing.T) {
		fp, dec := build(t, nil)
		pl := p4offline.DerivePlacement(dec, fp, validProof(fp))
		if pl.Status != p4offline.PlacementCounterfactualNotInheritable || !containsString(pl.Reasons, "TERMINAL_SLOT_DIFFERS") {
			t.Fatalf("%+v", pl)
		}
	})
	t.Run("the terminal fact naming the policy's slot inherits", func(t *testing.T) {
		fp, dec := build(t, ptrInt(0))
		if pl := p4offline.DerivePlacement(dec, fp, validProof(fp)); pl.Status != p4offline.PlacementAcceptedPlatformProven {
			t.Fatalf("%+v", pl)
		}
	})
}

// TestARefusedDecisionNamesItsExtentAndNotItsText is obligation D's proof, and
// it covers BOTH evidence seams from one place because they share one rule,
// one helper and one refusal: derivePlacement states the rule in full and
// derivePayout defers to it, so two tests would be two copies of one table to
// drift apart.
//
// WHAT THE DEFECT WAS. A decision decisionRefusal rejects has never been shown
// to be this package's, so every string on it is the caller's and nothing
// bounds it. Both seams copied those strings into the artifact and then hashed
// the artifact into its witness, so a refusal decided by ONE comparison
// allocated 18,301,274 B/op on a decision carrying five 1 MiB strings --
// about 3.49x what the caller supplied -- against 872 B/op for the same
// refusal at one byte. DerivePayout read 18,301,246 against 1,576.
//
// WHAT IS ASSERTED IS FLATNESS, not a budget. A budget alone passes for a
// repair that shrinks the multiple and leaves the shape, and doc.go's own
// entry on this finding warns that the multiple is not even a constant: the
// same path with three of the five strings wide read 2.19x where five read
// 3.49x, because what moves between them is the canonical buffer's growth
// series. A one-byte control measured on the same fixture cannot be satisfied
// that way. It now reads 1,712 -> 1,792 bytes for DerivePlacement and
// 1,840 -> 1,952 for DerivePayout across the same span.
//
// THE ONE-BYTE REFUSAL GOT DEARER AND THAT IS PART OF THE TRADE, recorded here
// rather than absorbed: the extent sentence costs about 840 bytes at the
// placement seam and about 260 at the payout seam that a refusal naming its
// case in full did not pay. What it buys is 18.3 MB at 1 MiB and an artifact
// that echoes no unverified text at any width.
func TestARefusedDecisionNamesItsExtentAndNotItsText(t *testing.T) {
	measure := func(f func()) uint64 {
		runtime.GC()
		var a, b runtime.MemStats
		runtime.ReadMemStats(&a)
		f()
		runtime.ReadMemStats(&b)
		return b.TotalAlloc - a.TotalAlloc
	}
	// The FIRST clause of decisionRefusal refuses a policy name that is
	// neither P2 nor P3b, so nothing below that clause runs and every byte
	// measured is the artifact's own.
	decisionAt := func(width int) p4offline.PolicyDecision {
		v := strings.Repeat("x", width)
		return p4offline.PolicyDecision{
			Policy: v,
			Attempt: predictioneval.AttemptKey{CollectorEpoch: 20260901, CollectorSessionID: v,
				PoolInstanceID: v, AttemptID: 7},
			FactsetDigest: v, EventID: v, Derivation: v,
			OutcomeIDs: []string{"o1", "o2"}, Stake: p4offline.UnknownInt64(v),
		}
	}
	const wide = 1 << 20
	narrowDec, wideDec := decisionAt(1), decisionAt(wide)
	wideText := strings.Repeat("x", wide)

	// withheld holds one refused artifact's reasons to the whole rule: the
	// category survives, the extent is stated, and no byte of the text is.
	withheld := func(t *testing.T, seam string, reasons []string, wantExtent string) {
		t.Helper()
		if !containsString(reasons, "POLICY_BINDING_MISMATCH") {
			t.Fatalf("%s: the refusal category did not survive the withholding: %v", seam, reasons)
		}
		var extent string
		for _, r := range reasons {
			if len(r) > 512 {
				t.Fatalf("%s: a reason of %d bytes is carrying its input, not describing it", seam, len(r))
			}
			if strings.Contains(r, wideText) {
				t.Fatalf("%s: a reason re-exported the text it withheld", seam)
			}
			if strings.HasPrefix(r, p4offline.PlacementReasonIdentityWithheldPrefix) {
				extent = r
			}
		}
		if extent == "" {
			t.Fatalf("%s: a refused artifact that names no extent has lost the case in silence: %v", seam, reasons)
		}
		if extent != wantExtent {
			t.Fatalf("%s: the extent report is\n got %q\nwant %q", seam, extent, wantExtent)
		}
	}
	const wideExtent = "1048576 bytes"
	wantPlacement := p4offline.PlacementReasonIdentityWithheldPrefix + "policy " + wideExtent +
		", session " + wideExtent + ", pool " + wideExtent + ", factset digest " + wideExtent +
		", event " + wideExtent
	wantPayout := wantPlacement + ", derivation " + wideExtent

	t.Run("the placement seam withholds the identity and states its extent", func(t *testing.T) {
		var narrow, wideOut p4offline.PlacementEvidence
		narrowCost := measure(func() { narrow = p4offline.DerivePlacement(narrowDec, p4offline.FactualPlacement{}, nil) })
		wideCost := measure(func() { wideOut = p4offline.DerivePlacement(wideDec, p4offline.FactualPlacement{}, nil) })
		t.Logf("DerivePlacement refused: %d bytes at one byte supplied, %d at 1 MiB", narrowCost, wideCost)

		if wideOut.Status != p4offline.PlacementUnknown {
			t.Fatalf("a refusal must not become a verdict: status %s, %s", wideOut.Status, placementExtents(wideOut))
		}
		withheld(t, "DerivePlacement", wideOut.Reasons, wantPlacement)
		if wideOut.Policy != "" || wideOut.FactsetDigest != "" || wideOut.EventID != "" ||
			wideOut.Attempt.CollectorSessionID != "" || wideOut.Attempt.PoolInstanceID != "" {
			t.Fatalf("the artifact echoed identity the refusal never verified: %s", placementExtents(wideOut))
		}
		// The case is withheld, not lost: the two fixed-width numbers are the
		// caller's own and cost nothing to carry.
		if wideOut.Attempt.CollectorEpoch != 20260901 || wideOut.Attempt.AttemptID != 7 {
			t.Fatalf("the refused artifact does not name its attempt at all: %s", placementExtents(wideOut))
		}
		if wideOut.PolicyStake.Known() || wideOut.PolicyStake.Reason != "POLICY_BINDING_MISMATCH" {
			t.Fatalf("a refused decision has no established policy stake: %s", placementExtents(wideOut))
		}
		if !wideOut.PolicyStake.Known() && strings.Contains(wideOut.PolicyStake.Reason, wideText) {
			t.Fatalf("the stake's reason carried the caller's text")
		}
		if narrow.Policy != "" || narrow.Attempt.CollectorEpoch != 20260901 {
			t.Fatalf("the one-byte control must be the same shape: %+v", narrow)
		}
		if wideCost > narrowCost+refusedArtifactSlack {
			t.Fatalf("the refusal grew by %d bytes as its input grew by %d: it is still framing what it refused",
				wideCost-narrowCost, wide-1)
		}
	})

	t.Run("the payout seam withholds one field more", func(t *testing.T) {
		var narrow, wideOut p4offline.PayoutEvidence
		var res p4offline.ResolutionArtifact
		narrowCost := measure(func() {
			narrow = p4offline.DerivePayout(narrowDec, p4offline.PlacementEvidence{}, res, nil)
		})
		wideCost := measure(func() {
			wideOut = p4offline.DerivePayout(wideDec, p4offline.PlacementEvidence{}, res, nil)
		})
		t.Logf("DerivePayout refused: %d bytes at one byte supplied, %d at 1 MiB", narrowCost, wideCost)

		if wideOut.Outcome != p4offline.PayoutUnknown || wideOut.ChoiceCorrect != p4offline.ChoiceUnknown ||
			wideOut.PrimaryDenominatorMember || wideOut.PlacedBetDenominatorMember {
			t.Fatalf("a refusal must not become a settlement or a denominator member: outcome %s, %s", wideOut.Outcome, payoutExtents(wideOut))
		}
		withheld(t, "DerivePayout", wideOut.Reasons, wantPayout)
		if wideOut.Policy != "" || wideOut.FactsetDigest != "" || wideOut.EventID != "" ||
			wideOut.Derivation != "" || wideOut.ResolutionFactsDigest != "" ||
			wideOut.Attempt.CollectorSessionID != "" || wideOut.Attempt.PoolInstanceID != "" {
			t.Fatalf("the artifact echoed identity the refusal never verified: %s", payoutExtents(wideOut))
		}
		if wideOut.Stake.Known() || wideOut.Payout.Known() || wideOut.Net.Known() {
			t.Fatalf("a refused decision is paid nothing and is owed nothing: %s", payoutExtents(wideOut))
		}
		if narrow.Policy != "" || narrow.Attempt.AttemptID != 7 {
			t.Fatalf("the one-byte control must be the same shape: %+v", narrow)
		}
		if wideCost > narrowCost+refusedArtifactSlack {
			t.Fatalf("the refusal grew by %d bytes as its input grew by %d: it is still framing what it refused",
				wideCost-narrowCost, wide-1)
		}
	})

	t.Run("a recognized policy name survives, and only a recognized one", func(t *testing.T) {
		_, fs, fp, p2 := factualCase(t, coherentCall)
		dec := decisionOf(t, p2, fs)
		negative := dec
		negative.Stake = p4offline.KnownInt64(-1) // refused below the policy clause
		pl := p4offline.DerivePlacement(negative, fp, validProof(fp))
		if !containsString(pl.Reasons, "STAKE_NEGATIVE") {
			t.Fatalf("fixture: this decision must be refused for its stake: %+v", pl)
		}
		// THE NAME IS THIS PACKAGE'S CONSTANT MATCHED BY VALUE, so carrying it
		// echoes nothing unbounded -- and it is what keeps a refused placement
		// being refused downstream for the CASE it does not name rather than
		// for the policy it does.
		if pl.Policy != p4offline.PolicyP2 {
			t.Fatalf("a recognized policy name is not unverified text and must survive: %s", placementExtents(pl))
		}
		if pl.FactsetDigest != "" || pl.EventID != "" || pl.Attempt.CollectorSessionID != "" {
			t.Fatalf("everything else is still withheld: %s", placementExtents(pl))
		}
		if pe := p4offline.DerivePayout(dec, pl, winnerArtifact("o1"), nil); pe.Outcome != p4offline.PayoutUnknown ||
			!containsString(pe.Reasons, "PLACEMENT_BINDING_MISMATCH") {
			t.Fatalf("a refused placement settles nothing, and says so as a CASE mismatch: %+v", pe)
		}
	})

	t.Run("an accepted decision still carries its whole identity", func(t *testing.T) {
		_, fs, fp, p2 := factualCase(t, coherentCall)
		dec := decisionOf(t, p2, fs)
		pl := p4offline.DerivePlacement(dec, fp, validProof(fp))
		if pl.Policy != dec.Policy || pl.Attempt != dec.Attempt || pl.FactsetDigest != dec.FactsetDigest ||
			pl.EventID != dec.EventID || pl.PolicyStake != dec.Stake {
			t.Fatalf("the accepted path lost identity the withholding was never meant to touch: %s", placementExtents(pl))
		}
		for _, r := range pl.Reasons {
			if strings.HasPrefix(r, p4offline.PlacementReasonIdentityWithheldPrefix) {
				t.Fatalf("an accepted artifact reports no withholding: %v", pl.Reasons)
			}
		}
		pe := p4offline.DerivePayout(dec, pl, winnerArtifact("o1"), nil)
		if pe.Policy != dec.Policy || pe.FactsetDigest != dec.FactsetDigest || pe.EventID != dec.EventID ||
			pe.Derivation != dec.Derivation {
			t.Fatalf("the accepted payout path lost identity: %s", payoutExtents(pe))
		}
	})
}

// refusedArtifactSlack is how much wider a refused artifact may be at a 1 MiB
// identity than at a one-byte one.
//
// IT IS NOT A BUDGET FOR THE PAYLOAD, it is room for the EXTENT SENTENCE: six
// numbers grow from one digit to seven, and the canonical buffer that frames
// the reasons takes its growth in steps rather than by the byte. Measured on
// this tree the two seams grow by 80 and 112 bytes across that span, so the
// ceiling is an order of magnitude above what the sentence costs and six
// orders below what one reinstated framing would.
const refusedArtifactSlack = 1024

// placementExtents and payoutExtents render an artifact's caller-supplied
// strings BY LENGTH.
//
// THEY EXIST BECAUSE A FAILURE MESSAGE IS AN ARTIFACT TOO. The first build of
// the test above printed the whole evidence with %+v, so the one mutant that
// reinstated a 1 MiB field made the test emit six megabytes of x. A test that
// proves a refusal does not carry its input must not carry it either.
func placementExtents(p p4offline.PlacementEvidence) string {
	return fmt.Sprintf("policy=%d session=%d pool=%d factset=%d event=%d epoch=%d attempt=%d stake=%s/%d/reason=%d reasons=%d",
		len(p.Policy), len(p.Attempt.CollectorSessionID), len(p.Attempt.PoolInstanceID),
		len(p.FactsetDigest), len(p.EventID), p.Attempt.CollectorEpoch, p.Attempt.AttemptID,
		p.PolicyStake.Presence, p.PolicyStake.Value, len(p.PolicyStake.Reason), len(p.Reasons))
}

func payoutExtents(p p4offline.PayoutEvidence) string {
	return fmt.Sprintf("policy=%d session=%d pool=%d factset=%d event=%d derivation=%d resolution=%d epoch=%d attempt=%d reasons=%d",
		len(p.Policy), len(p.Attempt.CollectorSessionID), len(p.Attempt.PoolInstanceID),
		len(p.FactsetDigest), len(p.EventID), len(p.Derivation), len(p.ResolutionFactsDigest),
		p.Attempt.CollectorEpoch, p.Attempt.AttemptID, len(p.Reasons))
}

// TestALocalErrorClassIsNamedByItsExtentAndNotEchoed is the receipt for the
// local-error arm, reported by a review lane on the published head.
//
// THE DEFECT. placementStatusCoherent admits ANY error class other than NONE
// whenever the reason code is not OK, so a supplied dataset can carry an
// otherwise coherent failed CALL_RETURNED whose class is arbitrarily wide. The
// arm copied that class into Reasons, and placementEvidenceWitness then framed
// the copy -- so a fail-closed placement returned attacker-sized diagnostic text
// and paid payload-sized allocations to produce it.
//
// THE REPAIR IS THE RULE THE REFUSED-DECISION SEAMS ALREADY FOLLOW, applied
// without an exception: a refusal names the fault and the EXTENT, never the
// text. There is no vocabulary to recognize the class against -- this
// repository names exactly one class constant, NONE, which this arm cannot see
// -- so unlike a policy name the class cannot be carried-when-recognized. It is
// withheld outright, and the reporting loss is recorded rather than absorbed.
//
// WHAT IS ASSERTED IS A ONE-BYTE CONTROL, not a budget, for the reason
// obligation D's own test records: a budget is satisfied by shrinking a
// multiple, where a control measured on the same fixture is not.
func TestALocalErrorClassIsNamedByItsExtentAndNotEchoed(t *testing.T) {
	failedCall := func(class string) func(*synth, *predictioneval.SourceDecisionEnvelope) {
		return func(s *synth, env *predictioneval.SourceDecisionEnvelope) {
			s.placement("r1", "e1", 1, predictioneval.PhaseCallStarted,
				*env.FinalAmount, *env.ChoiceIndex, "OK", "NONE")
			// Coherent BY THE PRODUCER'S OWN RULE: the reason code is not OK
			// and the class is not NONE, so the pair agrees and the record is
			// admitted. Only the class's WIDTH is the caller's to choose.
			s.placement("r1", "e1", 1, predictioneval.PhaseCallReturned,
				*env.FinalAmount, *env.ChoiceIndex, "LOCAL_FAILURE", class)
		}
	}

	derive := func(t *testing.T, class string) (p4offline.PlacementEvidence, uint64) {
		t.Helper()
		_, fs, fp, p2 := factualCase(t, failedCall(class))
		if fp.LocalReasonOK {
			t.Fatal("the fixture must reach the local-error arm")
		}
		if fp.ErrorClass != class {
			t.Fatalf("the factual placement must carry the supplied class: got %d bytes, want %d",
				len(fp.ErrorClass), len(class))
		}
		dec := decisionOf(t, p2, fs)
		runtime.GC()
		var a, b runtime.MemStats
		runtime.ReadMemStats(&a)
		pl := p4offline.DerivePlacement(dec, fp, nil)
		runtime.ReadMemStats(&b)
		return pl, b.TotalAlloc - a.TotalAlloc
	}

	const wide = 1 << 20
	narrow, narrowCost := derive(t, "X")
	huge, hugeCost := derive(t, strings.Repeat("X", wide))
	t.Logf("one byte -> %d B/op; %d bytes -> %d B/op", narrowCost, wide, hugeCost)

	// THE ASSERTION IS ONE FRAMING, NOT A CONSTANT, and the difference is the
	// whole of what this seam can honestly promise. The class here is a field of
	// a GENUINE FactualPlacement -- one this package projected from the dataset
	// -- so factualPlacementWitness frames it to answer whether that placement
	// is derived. That is work proportional to an artifact the dataset really
	// carries, and removing it would mean a witness that does not cover the
	// field. What the repair removes is every FURTHER framing: the
	// concatenation into Reasons and placementEvidenceWitness framing that
	// copy. Measured on this tree: 3,174,320 B/op before, which is 3.03x the
	// supplied class, against 1,061,552 after, which is 1.012x.
	//
	// AN EDITED PLACEMENT IS A DIFFERENT QUESTION AND IS NOT THIS TEST'S. A Q3
	// lane pointed out that a copied-and-widened placement is NOT an artifact
	// the dataset carries, so the framing there is removable and was removed:
	// FactualPlacement carries a framed width now, like the two result types.
	// TestAnEditedResultIsRefusedWithoutFramingTheEdit covers that seam.
	if over := grew(narrowCost, hugeCost, wide); over > 1.5 {
		t.Fatalf("a %d-byte error class cost %.2fx its own width above the one-byte control (%d against %d B/op): it is framed more than once",
			wide, over, hugeCost, narrowCost)
	}

	// THE ARTIFACT MUST CARRY THE EXTENT AND NOT ONE BYTE OF THE CLASS. The
	// second half is checked by searching every reason for the class's own
	// text, because a reason that merely LOOKS like an extent while another
	// carries the bytes would satisfy the first half alone.
	for _, c := range []struct {
		name string
		pl   p4offline.PlacementEvidence
		want string
	}{
		{"one byte", narrow, p4offline.PlacementReasonLocalErrorClassWithheldPrefix + "1 bytes"},
		{"1 MiB", huge, p4offline.PlacementReasonLocalErrorClassWithheldPrefix + "1048576 bytes"},
	} {
		if !containsString(c.pl.Reasons, c.want) {
			t.Fatalf("%s: the artifact must name the class's extent, got %v", c.name, c.pl.Reasons)
		}
		for _, r := range c.pl.Reasons {
			if strings.Contains(r, strings.Repeat("X", 2)) {
				t.Fatalf("%s: a reason carries the class's own text: %d bytes", c.name, len(r))
			}
		}
		if c.pl.Status != p4offline.PlacementLocalErrorPlatformUnknown {
			t.Fatalf("%s: the verdict must not move, got %v", c.name, c.pl.Status)
		}
	}

	// AND THE WITNESS IS UNCHANGED BY THE WIDTH, which is the half the reason
	// text alone cannot show: the witness frames Reasons, so a class echoed
	// anywhere would make these two differ.
	if narrow.Status != huge.Status {
		t.Fatalf("the two widths reached different arms: %v against %v", narrow.Status, huge.Status)
	}
}
