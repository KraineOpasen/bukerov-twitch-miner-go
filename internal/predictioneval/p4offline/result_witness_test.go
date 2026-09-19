package p4offline_test

// POLICY RESULTS carry their own witness: a PolicyDecision can be minted
// only from a result this package's evaluators produced, never from a
// hand-built, edited or stored-and-reloaded P2CaseResult / P3bCaseResult.
// Without this, the decision's witness would attest only that a Decision
// method ran — over anything — and a fabricated P3b result could enter the
// primary denominator with a fabricated derivation.

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval/p4offline"
)

func TestPolicyResultsCarryTheirOwnWitness(t *testing.T) {
	ds, fs, fp, p2 := factualCase(t, coherentCall)
	p2dec := decisionOf(t, p2, fs)
	rs := mustVerify(t, rulesetFrom(t, cfgWithRule("one", predictioneval.ComparatorGe, 50, 100)))
	p3b, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, 0))
	if err != nil {
		t.Fatal(err)
	}
	res := winnerArtifact("o1")

	t.Run("a hand-built P3b result mints no decision", func(t *testing.T) {
		// Every field a Decision reads, filled with genuine-looking values:
		// the genuine factset digest, a legal WOULD_ATTEMPT on the winner,
		// a plausible stake, and a fabricated provenance.
		fake := p4offline.P3bCaseResult{
			Policy: p4offline.PolicyP3b, FactsetDigest: fs.Digest,
			RulesetID: "donor-example-streamer-a", RulesetRawSHA256: rs.RawSHA256, NativeConfigDigest: rs.NativeConfigDigest,
			Trace:  p4offline.EntropyTraceBinding{Coordinates: synthCoords(fs, 0), RunID: p3b.Trace.RunID, EntropyDigest: p3b.Trace.EntropyDigest},
			Action: legalMapping(p4offline.PolicyP3b, string(predictioneval.StatusWouldAttempt), p4offline.ActionWouldAttempt),
			Choice: p4offline.PolicyChoice{Present: true, Index: 0, OutcomeID: "o1"},
			Stake:  p4offline.KnownInt64(50),
		}
		dec, err := fake.Decision(fs)
		if !errors.Is(err, p4offline.ErrResultNotDerived) {
			t.Fatalf("a result no evaluator produced must mint no decision: err=%v decision=%+v", err, dec)
		}
	})
	t.Run("an edited genuine P3b result mints no decision", func(t *testing.T) {
		for name, edit := range map[string]func(*p4offline.P3bCaseResult){
			"choice": func(r *p4offline.P3bCaseResult) {
				r.Choice = p4offline.PolicyChoice{Present: true, Index: 1, OutcomeID: "o2"}
			},
			"stake": func(r *p4offline.P3bCaseResult) { r.Stake = p4offline.KnownInt64(51) },
			"class": func(r *p4offline.P3bCaseResult) {
				r.Action.Class = p4offline.ActionPolicySkip
				r.Stake = p4offline.KnownInt64(0)
			},
			"ruleset":     func(r *p4offline.P3bCaseResult) { r.RulesetID = "other" },
			"raw hash":    func(r *p4offline.P3bCaseResult) { r.RulesetRawSHA256 = rs.NativeConfigDigest },
			"native":      func(r *p4offline.P3bCaseResult) { r.NativeConfigDigest = rs.RawSHA256 },
			"run":         func(r *p4offline.P3bCaseResult) { r.Trace.RunID = "forged" },
			"trajectory":  func(r *p4offline.P3bCaseResult) { r.Trace.Coordinates.Trajectory = 7 },
			"entropy":     func(r *p4offline.P3bCaseResult) { r.Trace.EntropyDigest = "" },
			"legality":    func(r *p4offline.P3bCaseResult) { r.Action.Legal = false },
			"native name": func(r *p4offline.P3bCaseResult) { r.Action.NativeAction = "X" },
			"illegality":  func(r *p4offline.P3bCaseResult) { r.Action.Illegality = []string{"INVENTED"} },
			"class only":  func(r *p4offline.P3bCaseResult) { r.Action.Class = p4offline.ActionUnsupportedShape },
			"policy":      func(r *p4offline.P3bCaseResult) { r.Policy = p4offline.PolicyP2 },
			"refusal":     func(r *p4offline.P3bCaseResult) { r.ProjectionRefusal = "p4offline: invented" },
			"dataset":     func(r *p4offline.P3bCaseResult) { r.Trace.Coordinates.DatasetID = "other" },
			"version":     func(r *p4offline.P3bCaseResult) { r.Trace.Coordinates.DatasetVersion = "v2" },
			"digest ref": func(r *p4offline.P3bCaseResult) {
				r.Trace.Coordinates.CommonFactsetDigest = p4offline.DigestReference("0000000000000000000000000000000000000000000000000000000000000000")
			},
			"opportunity": func(r *p4offline.P3bCaseResult) { r.Trace.Coordinates.PairedOpportunityID = "e2" },
			"words":       func(r *p4offline.P3bCaseResult) { r.Trace.WordsSupplied++ },
			"consumed":    func(r *p4offline.P3bCaseResult) { r.Trace.WordsConsumed++ },
			"selection":   func(r *p4offline.P3bCaseResult) { r.Projection.Stream.SelectionDigest = "" },
			"candidate":   func(r *p4offline.P3bCaseResult) { r.Projection.CandidateIdentity = "attempt-2" },
			"status":      func(r *p4offline.P3bCaseResult) { r.Evaluation.Status = "" },
			"reason":      func(r *p4offline.P3bCaseResult) { r.Evaluation.Reason = "invented" },
			"ev entropy":  func(r *p4offline.P3bCaseResult) { r.Evaluation.EntropyDigest = "" },
			"raw words":   func(r *p4offline.P3bCaseResult) { r.Evaluation.RawWordsConsumed++ },
			"bernoulli":   func(r *p4offline.P3bCaseResult) { r.Evaluation.BernoulliEvaluations++ },
			"mode":        func(r *p4offline.P3bCaseResult) { r.Projection.Mode = "OTHER" },
			"proj digest": func(r *p4offline.P3bCaseResult) { r.Projection.FactsetDigest = "" },
			"proj event":  func(r *p4offline.P3bCaseResult) { r.Projection.EventID = "e2" },
			"proj cutoff": func(r *p4offline.P3bCaseResult) { r.Projection.CutoffPosition++ },
			"outcomes":    func(r *p4offline.P3bCaseResult) { r.Projection.OutcomeCount++ },
			"stream":      func(r *p4offline.P3bCaseResult) { r.Evaluation.StreamDigest = "" },
			"config":      func(r *p4offline.P3bCaseResult) { r.Evaluation.ConfigDigest = "" },
			"input":       func(r *p4offline.P3bCaseResult) { r.Evaluation.ConsumedInputDigest = "" },
		} {
			edited := p3b
			edit(&edited)
			if _, err := edited.Decision(fs); !errors.Is(err, p4offline.ErrResultNotDerived) {
				t.Fatalf("%s: an edited result must mint no decision: %v", name, err)
			}
		}
		var decoded p4offline.P3bCaseResult
		raw := mustMarshal(t, p3b)
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if _, err := decoded.Decision(fs); !errors.Is(err, p4offline.ErrResultNotDerived) {
			t.Fatalf("a result read back from storage must be re-evaluated, not trusted: %v", err)
		}
		// The genuine result still decides, and a refused projection is a
		// genuine result too.
		if _, err := p3b.Decision(fs); err != nil {
			t.Fatalf("the genuine result must decide: %v", err)
		}
		_, refusedFs := selectedFactset(t, func(e *predictioneval.SourceDecisionEnvelope) { e.Outcomes[1].ID = "" }, nil)
		refused, err := p4offline.EvaluateP3bCase(refusedFs, rs, synthCoords(refusedFs, 0))
		if err != nil {
			t.Fatal(err)
		}
		if refused.Action.Class != p4offline.ActionRefused {
			t.Fatalf("fixture: %+v", refused.Action)
		}
		if _, err := refused.Decision(refusedFs); err != nil {
			t.Fatalf("a refused projection is the evaluator's own result: %v", err)
		}
	})
	t.Run("a hand-built or edited P2 result mints no decision", func(t *testing.T) {
		fake := p4offline.P2CaseResult{Policy: p4offline.PolicyP2, FactsetDigest: fs.Digest, Binding: p2.Binding,
			Action: p2.Action, Choice: p2.Choice, Stake: p2.Stake}
		if _, err := fake.Decision(fs); !errors.Is(err, p4offline.ErrResultNotDerived) {
			t.Fatalf("got %v", err)
		}
		for name, edit := range map[string]func(*p4offline.P2CaseResult){
			"binding digest": func(r *p4offline.P2CaseResult) { r.Binding.Digest = fs.Digest },
			"contract":       func(r *p4offline.P2CaseResult) { r.Binding.ContractVersion = "p4-p2-config-binding/v2" },
			"policy":         func(r *p4offline.P2CaseResult) { r.Policy = p4offline.PolicyP3b },
			"action":         func(r *p4offline.P2CaseResult) { r.Action.NativeAction = "X" },
			"native action":  func(r *p4offline.P2CaseResult) { r.Evaluation.Action = "X" },
			"action reason":  func(r *p4offline.P2CaseResult) { r.Evaluation.ActionReason = "invented" },
			"input digest":   func(r *p4offline.P2CaseResult) { r.Evaluation.CommonInputDigest = "" },
			"stake":          func(r *p4offline.P2CaseResult) { r.Stake = p4offline.KnownInt64(51) },
			"choice":         func(r *p4offline.P2CaseResult) { r.Choice.OutcomeID = "o2"; r.Choice.Index = 1 },
		} {
			edited := p2
			edit(&edited)
			if _, err := edited.Decision(fs); !errors.Is(err, p4offline.ErrResultNotDerived) {
				t.Fatalf("%s: got %v", name, err)
			}
		}
		var decoded p4offline.P2CaseResult
		raw := mustMarshal(t, p2)
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if _, err := decoded.Decision(fs); !errors.Is(err, p4offline.ErrResultNotDerived) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a genuine result of one case cannot be replayed as another case's", func(t *testing.T) {
		// The same shapes with another balance: a different, genuine factset.
		_, other := selectedFactset(t, func(e *predictioneval.SourceDecisionEnvelope) { e.Balance = ptrI64(2000) }, nil)
		if other.Digest == fs.Digest {
			t.Fatalf("fixture: the two factsets coincide")
		}
		replay := p3b
		replay.FactsetDigest = other.Digest
		if d, err := replay.Decision(other); !errors.Is(err, p4offline.ErrResultNotDerived) || d.Policy != "" || d.FactsetDigest != "" {
			t.Fatalf("a P3b result relabelled to another factset must mint no decision: %v %+v", err, d)
		}
		replayP2 := p2
		replayP2.FactsetDigest = other.Digest
		if d, err := replayP2.Decision(other); !errors.Is(err, p4offline.ErrResultNotDerived) || d.Policy != "" || d.FactsetDigest != "" {
			t.Fatalf("a P2 result relabelled to another factset must mint no decision: %v %+v", err, d)
		}
	})
	t.Run("a binding contradiction is still named before the witness", func(t *testing.T) {
		forged := p3b
		forged.Action.Policy = p4offline.PolicyP2
		if _, err := forged.Decision(fs); !errors.Is(err, p4offline.ErrDecisionBinding) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("the primary denominator cannot be entered through a minted decision", func(t *testing.T) {
		// The chain a forged P3b result would need: decision, placement,
		// payout, composed verdict. It stops at the first link.
		fake := p3b
		fake.Choice = p4offline.PolicyChoice{Present: true, Index: 0, OutcomeID: "o1"}
		fake.Stake = p4offline.KnownInt64(50)
		if _, err := fake.Decision(fs); err == nil {
			t.Fatalf("an edited result minted a decision")
		}
		// And a decision minted before the edit is still bound to what it
		// was minted from: the genuine chain counts, exactly once.
		genuine := decisionOf(t, p3b, fs)
		pe := p4offline.DerivePayout(genuine, p4offline.DerivePlacement(genuine, fp, nil), res, nil)
		if m := p4offline.AssessDenominatorMembership(ds, registryOf(ds, fs), fs, p2dec, genuine, res, pe); !m.Primary {
			t.Fatalf("%+v", m)
		}
	})
}
