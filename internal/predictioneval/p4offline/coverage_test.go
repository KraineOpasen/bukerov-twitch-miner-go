package p4offline_test

// Remaining acceptance seams: a missing outcome vector, a pre-decision exit,
// and the serialization round trip of every digested artifact.

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval/p4offline"
)

// TestMissingOutcomeVectorIsIncompleteNotEmpty pins vector presence: a
// factset whose vector was not recovered is INCOMPLETE, evaluable by neither
// policy, and distinct from an empty vector.
func TestMissingOutcomeVectorIsIncompleteNotEmpty(t *testing.T) {
	_, fs := selectedFactset(t, nil, nil)
	missing := fs
	missing.Outcomes = nil
	missing.OutcomesPresent = false
	missing.Completeness = p4offline.FactsetIncomplete
	missing.IncompleteReasons = []string{predictioneval.IneligibleOutcomeUnrepresentable}
	missing.Digest = ""
	// The digest is recomputed over the values so the factset is internally
	// consistent; this constructs an INPUT, not an expectation.
	sum := p4offline.SerializeCommonFactset(missing)
	if len(sum) == 0 {
		t.Fatal("serialization empty")
	}
	missing.Digest = digestOf(sum)
	if err := p4offline.VerifyCommonFactset(missing); err != nil {
		t.Fatal(err)
	}
	if _, err := p4offline.EvaluateP2Case(missing); !errors.Is(err, p4offline.ErrFactsetNotEvaluable) {
		t.Fatalf("P2: %v", err)
	}
	if _, err := p4offline.ProjectP3bSingleCandidate(missing); !errors.Is(err, p4offline.ErrFactsetNotEvaluable) {
		t.Fatalf("P3b: %v", err)
	}
	if missing.Digest == fs.Digest {
		t.Fatal("a missing vector and a present one share a digest")
	}
}

// TestPreDecisionExitIsCoverageOnly pins the factset of an attempt that
// ended before the policy ran: no inputs, no evaluation, descriptive only.
func TestPreDecisionExitIsCoverageOnly(t *testing.T) {
	s := newSynth()
	s.due("r1", "e1", 1)
	env := &predictioneval.SourceDecisionEnvelope{
		SettingsStage:  predictioneval.StageNotReached,
		CalculateStage: predictioneval.StageNotReached,
		SkipStage:      predictioneval.StageNotReached,
		HealthStage:    predictioneval.HealthNotReached,
		StakeStage:     predictioneval.StageNotReached,
	}
	s.terminal("r1", "e1", 1, predictioneval.PhaseAutoSkipped, "NOT_ELIGIBLE", env)
	ds := s.dataset()
	ep := singleEpisode(t, mustSelect(t, ds))
	if ep.Excluded {
		t.Fatalf("%v", ep.ExclusionReasons)
	}
	fs, err := p4offline.BuildCommonFactset(ds, ep.Episode)
	if err != nil {
		t.Fatal(err)
	}
	if fs.Completeness != p4offline.FactsetPreDecisionExit || fs.ReachedDecision || fs.PreDecisionExit != "NOT_ELIGIBLE" ||
		fs.StealthProof != p4offline.StealthProofUnknown || fs.Settings != nil || fs.BalancePresent || fs.OutcomesPresent {
		t.Fatalf("%+v", fs)
	}
	if err := p4offline.VerifyCommonFactset(fs); err != nil {
		t.Fatalf("a pre-decision factset verifies as what it is: %v", err)
	}
	if _, err := p4offline.EvaluateP2Case(fs); !errors.Is(err, p4offline.ErrFactsetNotEvaluable) {
		t.Fatalf("%v", err)
	}
	// The native evaluator's own COVERAGE_ONLY mapping is what such an attempt
	// yields when replayed directly (under the minimum the projection pins on
	// every path).
	m := p4offline.MapP2Action(predictioneval.Evaluate(
		predictioneval.DecisionInputs{PreDecisionExit: "NOT_ELIGIBLE", MinimumStake: predictioneval.PinnedMinimumStake},
		predictioneval.ObservedRealization{}))
	if !m.Legal || m.Class != p4offline.ActionCoverageOnly {
		t.Fatalf("%+v", m)
	}
	// Neither policy can produce a decision for a factset that is not
	// COMPLETE, so the case is assessed with none: coverage only.
	q := p4offline.AssessCaseQuality(ds, fs, p4offline.PolicyDecision{}, p4offline.PolicyDecision{}, winnerArtifact("o1"))
	if q.Quality != p4offline.QualityDescriptiveOnly || !containsString(q.Reasons, "FACTSET_PRE_DECISION_EXIT") ||
		containsString(q.Reasons, "POLICY_DECISION_ON_NON_EVALUABLE_FACTSET") {
		t.Fatalf("%+v", q)
	}
	// A decision handed in anyway for such a factset is named, not read.
	fabricated := p4offline.PolicyDecision{Policy: p4offline.PolicyP2, Attempt: fs.Attempt, FactsetDigest: fs.Digest, EventID: "e1",
		CutoffPosition: fs.CutoffPosition, Action: m, Stake: p4offline.KnownInt64(4242)}
	if q := p4offline.AssessCaseQuality(ds, fs, fabricated, p4offline.PolicyDecision{}, winnerArtifact("o1")); q.Quality != p4offline.QualityDescriptiveOnly ||
		!containsString(q.Reasons, "POLICY_DECISION_ON_NON_EVALUABLE_FACTSET") {
		t.Fatalf("%+v", q)
	}
}

// TestDigestedArtifactsSurviveTheirOwnJSONRoundTrip pins serialization: the
// factset, the resolution artifact and the P3b stream come back from JSON
// with digests that still verify, and a round-tripped factset evaluates
// exactly as the original.
func TestDigestedArtifactsSurviveTheirOwnJSONRoundTrip(t *testing.T) {
	_, fs := selectedFactset(t, func(e *predictioneval.SourceDecisionEnvelope) {
		e.Outcomes[0].Odds = 1.6600000000000001 // a value whose shortest rendering must survive
		e.Settings.FilterCondition = &predictioneval.SourceFilterCondition{By: "odds", Where: "GT", Value: 0.1}
	}, nil)
	var fs2 p4offline.CommonFactset
	raw, err := json.Marshal(fs)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &fs2); err != nil {
		t.Fatal(err)
	}
	if err := p4offline.VerifyCommonFactset(fs2); err != nil {
		t.Fatalf("factset after round trip: %v", err)
	}
	res := winnerArtifact("o1")
	var res2 p4offline.ResolutionArtifact
	raw, _ = json.Marshal(res)
	if err := json.Unmarshal(raw, &res2); err != nil {
		t.Fatal(err)
	}
	if err := p4offline.VerifyResolutionArtifact(res2); err != nil {
		t.Fatalf("resolution after round trip: %v", err)
	}
	rs := mustVerify(t, rulesetFrom(t, cfgWithRule("rt", predictioneval.ComparatorGe, 50, 100)))
	coords := synthCoords(fs, 0)
	trace, _ := p4offline.BuildDrawTrace(coords, 2)
	a, err := p4offline.EvaluateP3bWithTrace(fs, rs, coords, trace)
	if err != nil {
		t.Fatal(err)
	}
	b, err := p4offline.EvaluateP3bWithTrace(fs2, rs, coords, trace)
	if err != nil {
		t.Fatalf("the round-tripped factset must still be projected and accepted by the core: %v", err)
	}
	if a.Evaluation.ConsumedInputDigest != b.Evaluation.ConsumedInputDigest || a.Action.Class != b.Action.Class ||
		a.Projection.Stream.SelectionDigest != b.Projection.Stream.SelectionDigest {
		t.Fatalf("round trip changed the evaluation: %s vs %s", a.Evaluation.ConsumedInputDigest, b.Evaluation.ConsumedInputDigest)
	}
	// The projected stream itself survives JSON and is still the same
	// stream to the native core.
	var proj2 p4offline.P3bProjection
	raw, _ = json.Marshal(a.Projection)
	if err := json.Unmarshal(raw, &proj2); err != nil {
		t.Fatal(err)
	}
	native := predictioneval.EvaluateOrderedRules(proj2.Stream, rs.Config, trace)
	if native.ConsumedInputDigest != a.Evaluation.ConsumedInputDigest || native.StreamDigest != a.Projection.Stream.SelectionDigest {
		t.Fatalf("the round-tripped stream is not the projected stream: %s vs %s", native.ConsumedInputDigest, a.Evaluation.ConsumedInputDigest)
	}
}
