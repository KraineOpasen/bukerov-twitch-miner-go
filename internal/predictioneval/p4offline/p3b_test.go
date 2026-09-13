package p4offline_test

// SEAM 6 and the P3b plumbing: the explicitly supplied, verified raw ruleset;
// the one-candidate common-data projection; the P4 trace consumed by the
// native core; and the P3b half of the action map.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval/p4offline"
)

// nativeConfigDigest obtains pe-ors-config/v1 from the NATIVE evaluator over a
// probe of the test's own, so the expectation does not come from p4offline.
func nativeConfigDigest(t *testing.T, cfg predictioneval.OrderedRulesConfig) string {
	t.Helper()
	known := func(v int64) predictioneval.SuppliedInt64 {
		return predictioneval.SuppliedInt64{Presence: predictioneval.SuppliedKnown, Value: predictioneval.OrderedRulesInt64(v),
			Provenance: "test-probe", HasAvailableAtPosition: true}
	}
	stream, err := predictioneval.ProjectOrderedRulesStream(predictioneval.OrderedRulesSource{
		Scope: predictioneval.OrderedRulesScope{Namespace: "t", EpisodeID: "t", AccountContext: "t", AssociationEvidence: "t",
			SourceContractVersion: predictioneval.OrderedRulesStreamContractVersion,
			Coverage:              predictioneval.CoverageCompleteDeclared, HasInterval: true},
		Candidates: []predictioneval.OrderedRulesCandidate{{
			Identity: "c", HasPosition: true, SourceKind: predictioneval.SourceKindCalculateSnapshot,
			EpisodeMembership: predictioneval.MembershipProven, OutcomesPresence: predictioneval.SuppliedKnown,
			Outcomes: []predictioneval.OrderedRulesOutcome{{Identity: "a", Points: known(1)}, {Identity: "b", Points: known(1)}},
			Balance:  known(0),
		}},
	}, predictioneval.CommonAdmission{ManifestID: "t", ViewKind: predictioneval.ViewCalculateOnly, Population: "t", OrderBasis: "t"})
	if err != nil {
		t.Fatalf("native probe projection: %v", err)
	}
	ev := predictioneval.EvaluateOrderedRules(stream, cfg, predictioneval.SuppliedDrawTrace{
		RunID: "probe", EntropySemanticsVersion: predictioneval.OrderedRulesEntropySemanticsVersion})
	if ev.ConfigDigest == "" {
		t.Fatalf("native probe produced no config digest: %s/%s", ev.Status, ev.Reason)
	}
	return ev.ConfigDigest
}

// rulesetFrom builds a supplied ruleset from a config: raw bytes are the
// config's JSON, the raw hash is computed here, the native digest comes from
// the native evaluator.
func rulesetFrom(t *testing.T, cfg predictioneval.OrderedRulesConfig) p4offline.P3bRuleset {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return p4offline.P3bRuleset{
		RulesetID: cfg.ConfigID, RawBytes: raw, RawSHA256: hex.EncodeToString(sum[:]),
		Config: cfg, NativeConfigDigest: nativeConfigDigest(t, cfg),
	}
}

func mustVerify(t *testing.T, r p4offline.P3bRuleset) p4offline.VerifiedP3bRuleset {
	t.Helper()
	rs, err := p4offline.VerifyP3bRuleset(r)
	if err != nil {
		t.Fatalf("VerifyP3bRuleset: %v", err)
	}
	return rs
}

func cfgWithRule(id string, comparator predictioneval.OrderedRuleComparator, threshold, rate float64) predictioneval.OrderedRulesConfig {
	return predictioneval.OrderedRulesConfig{
		ConfigID: id, HasDefault: true,
		Detailed: []predictioneval.OrderedRule{{Comparator: comparator, RawThresholdPercent: threshold,
			RawAttemptRatePercent: rate, Points: predictioneval.OrderedRulesPoints{MaxValue: 0, RawPercent: 10}}},
		Default: predictioneval.OrderedRulesDefault{RawMinPercent: 95, RawMaxPercent: 100,
			Points: predictioneval.OrderedRulesPoints{MaxValue: 0, RawPercent: 1}},
	}
}

// cfgWithRulePoints is cfgWithRule with the rule's stake percentage chosen.
func cfgWithRulePoints(id string, comparator predictioneval.OrderedRuleComparator, threshold, rate, pointsPercent float64) predictioneval.OrderedRulesConfig {
	cfg := cfgWithRule(id, comparator, threshold, rate)
	cfg.Detailed[0].Points.RawPercent = pointsPercent
	return cfg
}

func cfgDefaultOnly(id string, min, max float64) predictioneval.OrderedRulesConfig {
	return predictioneval.OrderedRulesConfig{
		ConfigID: id, HasDefault: true,
		Default: predictioneval.OrderedRulesDefault{RawMinPercent: min, RawMaxPercent: max,
			Points: predictioneval.OrderedRulesPoints{MaxValue: 0, RawPercent: 1}},
	}
}

// TestP3bRulesetIsSuppliedVerifiedAndBound pins that the ruleset is consumed
// from explicit raw bytes and hashes, with every mismatch refused by name.
func TestP3bRulesetIsSuppliedVerifiedAndBound(t *testing.T) {
	raw, err := os.ReadFile("testdata/synthetic/ruleset_synthetic.json")
	if err != nil {
		t.Fatal(err)
	}
	sidecar, err := os.ReadFile("testdata/synthetic/ruleset_synthetic.sha256")
	if err != nil {
		t.Fatal(err)
	}
	wantHash := strings.TrimSpace(string(sidecar))
	var cfg predictioneval.OrderedRulesConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	good := p4offline.P3bRuleset{RulesetID: "p4-synthetic-ruleset-a", RawBytes: raw, RawSHA256: wantHash,
		Config: cfg, NativeConfigDigest: nativeConfigDigest(t, cfg)}
	rs := mustVerify(t, good)
	if rs.RulesetID != "p4-synthetic-ruleset-a" || rs.RawSHA256 != wantHash || rs.Rules() != 1 ||
		rs.NativeConfigDigest != good.NativeConfigDigest || rs.Config.ConfigID != cfg.ConfigID {
		t.Fatalf("%+v", rs)
	}

	cases := []struct {
		name   string
		mutate func(*p4offline.P3bRuleset)
		want   error
	}{
		{"declared raw hash differs", func(r *p4offline.P3bRuleset) { r.RawSHA256 = strings.Repeat("0", 64) }, p4offline.ErrRulesetRawHash},
		{"declared raw hash not canonical", func(r *p4offline.P3bRuleset) { r.RawSHA256 = strings.ToUpper(r.RawSHA256) }, p4offline.ErrRulesetRawHash},
		{"raw bytes edited after hashing", func(r *p4offline.P3bRuleset) {
			r.RawBytes = append([]byte(nil), r.RawBytes...)
			r.RawBytes[len(r.RawBytes)-2] = ' '
		}, p4offline.ErrRulesetRawHash},
		{"raw bytes carry an unknown field", func(r *p4offline.P3bRuleset) {
			r.RawBytes = []byte(strings.Replace(string(r.RawBytes), `"hasDefault": true,`, `"hasDefault": true, "extra": 1,`, 1))
			sum := sha256.Sum256(r.RawBytes)
			r.RawSHA256 = hex.EncodeToString(sum[:])
		}, p4offline.ErrRulesetRawDecode},
		{"raw bytes spell a key twice", func(r *p4offline.P3bRuleset) {
			r.RawBytes = []byte(strings.Replace(string(r.RawBytes), `"hasDefault": true,`, `"hasDefault": true, "hasDefault": true,`, 1))
			sum := sha256.Sum256(r.RawBytes)
			r.RawSHA256 = hex.EncodeToString(sum[:])
		}, p4offline.ErrRulesetRawDecode},
		{"raw bytes spell a key in another case", func(r *p4offline.P3bRuleset) {
			r.RawBytes = []byte(strings.Replace(string(r.RawBytes), `"hasDefault": true,`, `"HASDEFAULT": true,`, 1))
			sum := sha256.Sum256(r.RawBytes)
			r.RawSHA256 = hex.EncodeToString(sum[:])
		}, p4offline.ErrRulesetRawDecode},
		{"raw bytes spell a nested key twice", func(r *p4offline.P3bRuleset) {
			r.RawBytes = []byte(strings.Replace(string(r.RawBytes), `"rawMinPercent":`, `"rawMinPercent": 1, "rawMinPercent":`, 1))
			sum := sha256.Sum256(r.RawBytes)
			r.RawSHA256 = hex.EncodeToString(sum[:])
		}, p4offline.ErrRulesetRawDecode},
		{"raw bytes hold an object where the contract has none", func(r *p4offline.P3bRuleset) {
			r.RawBytes = []byte(strings.Replace(string(r.RawBytes), `"hasDefault": true,`, `"hasDefault": true, "configId": {"x": 1},`, 1))
			sum := sha256.Sum256(r.RawBytes)
			r.RawSHA256 = hex.EncodeToString(sum[:])
		}, p4offline.ErrRulesetRawDecode},
		{"raw bytes hold two documents", func(r *p4offline.P3bRuleset) {
			r.RawBytes = append(append([]byte(nil), r.RawBytes...), r.RawBytes...)
			sum := sha256.Sum256(r.RawBytes)
			r.RawSHA256 = hex.EncodeToString(sum[:])
		}, p4offline.ErrRulesetRawDecode},
		{"typed config differs from the raw bytes", func(r *p4offline.P3bRuleset) {
			r.Config.Detailed = append([]predictioneval.OrderedRule(nil), r.Config.Detailed...)
			r.Config.Detailed[0].RawAttemptRatePercent = 99
		}, p4offline.ErrRulesetConfigMismatch},
		{"typed default differs by one ulp", func(r *p4offline.P3bRuleset) {
			r.Config.Default.RawMinPercent = math.Nextafter(95, 96)
		}, p4offline.ErrRulesetConfigMismatch},
		{"native digest differs", func(r *p4offline.P3bRuleset) { r.NativeConfigDigest = strings.Repeat("a", 64) }, p4offline.ErrRulesetNativeDigest},
		{"ruleset identity differs from the config id", func(r *p4offline.P3bRuleset) { r.RulesetID = "other" }, p4offline.ErrRulesetIdentity},
		{"empty ruleset identity", func(r *p4offline.P3bRuleset) { r.RulesetID = ""; r.Config.ConfigID = "" }, p4offline.ErrRulesetIdentity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := good
			tc.mutate(&r)
			if _, err := p4offline.VerifyP3bRuleset(r); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
	t.Run("a hand-built or altered verified ruleset is refused at use", func(t *testing.T) {
		_, fs := selectedFactset(t, nil, nil)
		forged := p4offline.VerifiedP3bRuleset{RulesetID: "impostor", RawSHA256: "not-a-hash",
			NativeConfigDigest: good.NativeConfigDigest, Config: cfg}
		if _, err := p4offline.EvaluateP3bCase(fs, forged, "key", 0); !errors.Is(err, p4offline.ErrRulesetNotVerified) {
			t.Fatalf("got %v", err)
		}
		coords := p4offline.EntropyCoordinates{Trajectory: 0, SourceRoundID: "e1", PolicyID: "impostor"}
		trace, _ := p4offline.BuildDrawTrace("key", coords, 2)
		if _, err := p4offline.EvaluateP3bWithTrace(fs, forged, "key", coords, trace); !errors.Is(err, p4offline.ErrRulesetNotVerified) {
			t.Fatalf("got %v", err)
		}
		altered := rs
		altered.RulesetID = "renamed-after-verification"
		if _, err := p4offline.EvaluateP3bCase(fs, altered, "key", 0); !errors.Is(err, p4offline.ErrRulesetNotVerified) {
			t.Fatalf("got %v", err)
		}
		altered = rs
		altered.RawSHA256 = strings.Repeat("0", 64)
		if _, err := p4offline.EvaluateP3bCase(fs, altered, "key", 0); !errors.Is(err, p4offline.ErrRulesetNotVerified) {
			t.Fatalf("got %v", err)
		}
		retuned := rs
		retuned.Config.Detailed = append([]predictioneval.OrderedRule(nil), rs.Config.Detailed...)
		retuned.Config.Detailed[0].RawAttemptRatePercent = 0
		if _, err := p4offline.EvaluateP3bCase(fs, retuned, "key", 0); !errors.Is(err, p4offline.ErrRulesetNotVerified) {
			t.Fatalf("a config altered after verification no longer digests to the verified native digest: %v", err)
		}
		broken := rs
		broken.Config.Detailed = append([]predictioneval.OrderedRule(nil), rs.Config.Detailed...)
		broken.Config.Detailed[0].RawThresholdPercent = 500 // a shape the core refuses outright
		if _, err := p4offline.EvaluateP3bCase(fs, broken, "key", 0); !errors.Is(err, p4offline.ErrRulesetNotVerified) {
			t.Fatalf("a config the core refuses is not a typed REFUSED under the honest identity; it is an unverified ruleset: %v", err)
		}
		var decoded p4offline.VerifiedP3bRuleset
		raw, _ := json.Marshal(rs)
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if _, err := p4offline.EvaluateP3bCase(fs, decoded, "key", 0); !errors.Is(err, p4offline.ErrRulesetNotVerified) {
			t.Fatalf("a verified ruleset does not survive a round trip; the raw bytes must be re-verified: %v", err)
		}
	})
	t.Run("a config the core refuses is unusable", func(t *testing.T) {
		bad := cfgWithRule("bad", predictioneval.ComparatorGe, 150, 100) // threshold out of domain
		raw, _ := json.Marshal(bad)
		sum := sha256.Sum256(raw)
		r := p4offline.P3bRuleset{RulesetID: "bad", RawBytes: raw, RawSHA256: hex.EncodeToString(sum[:]),
			Config: bad, NativeConfigDigest: strings.Repeat("b", 64)}
		if _, err := p4offline.VerifyP3bRuleset(r); !errors.Is(err, p4offline.ErrRulesetConfigUnusable) {
			t.Fatalf("got %v", err)
		}
	})
}

// TestP3bProjectionCarriesOnlyCommonDataAndBindsTheFactset pins seam 6.
func TestP3bProjectionCarriesOnlyCommonDataAndBindsTheFactset(t *testing.T) {
	markers := func(e *predictioneval.SourceDecisionEnvelope) {
		e.Settings.Strategy = "MARKER_STRATEGY"
		e.Settings.FilterCondition = &predictioneval.SourceFilterCondition{By: "marker_filter", Where: "GT", Value: 3.5}
		e.Outcomes[0].TopPoints = 987654
		e.Outcomes[0].Odds = 12.345
		e.Outcomes[0].PercentageUsers = 67.89
		e.Outcomes[0].TotalUsers = 4321
		e.HealthReason = "marker-health"
		e.RiskReservePoints = ptrInt(55555)
		e.ChoiceOutcomeID = "marker-choice"
	}
	ep, fs := selectedFactset(t, markers, nil)
	proj, err := p4offline.ProjectP3bSingleCandidate(fs)
	if err != nil {
		t.Fatalf("ProjectP3bSingleCandidate: %v", err)
	}
	if proj.Mode != p4offline.P3bProjectionMode || proj.FactsetDigest != fs.Digest || proj.OutcomeCount != 2 ||
		proj.EventID != "e1" || proj.CutoffPosition != fs.CutoffPosition {
		t.Fatalf("%+v", proj)
	}
	st := proj.Stream
	if len(st.Candidates) != 1 {
		t.Fatalf("CALCULATE_INPUT_SINGLE_CANDIDATE means one candidate, got %d", len(st.Candidates))
	}
	if proj.Stream.Candidates[0].Outcomes == nil {
		t.Fatal("projection carries no outcomes")
	}
	c := st.Candidates[0]
	if !c.HasPosition || int64(c.Position) != ep.Boundary.CutoffPosition || c.Identity != proj.CandidateIdentity {
		t.Fatalf("the candidate sits at C with its position DECLARED: %+v", c)
	}
	if c.SourceKind != predictioneval.SourceKindCalculateSnapshot || st.Admission.ViewKind != predictioneval.ViewCalculateOnly ||
		st.Admission.ManifestID != p4offline.P3bProjectionManifestID {
		t.Fatalf("a decision-time snapshot must be declared as such: %+v / %+v", c.SourceKind, st.Admission)
	}
	if c.OutcomesPresence != predictioneval.SuppliedKnown || len(c.Outcomes) != 2 ||
		c.Outcomes[0].Identity != "o1" || c.Outcomes[1].Identity != "o2" ||
		c.Outcomes[0].Points.Presence != predictioneval.SuppliedKnown || int64(c.Outcomes[0].Points.Value) != 600 ||
		int64(c.Outcomes[1].Points.Value) != 400 ||
		!c.Outcomes[0].Points.HasAvailableAtPosition || int64(c.Outcomes[0].Points.AvailableAtPosition) != ep.Boundary.CutoffPosition {
		t.Fatalf("outcomes must be the model vector's points in order, available at C: %+v", c.Outcomes)
	}
	if c.Balance.Presence != predictioneval.SuppliedKnown || int64(c.Balance.Value) != 1000 ||
		!c.Balance.HasAvailableAtPosition || int64(c.Balance.AvailableAtPosition) != ep.Boundary.CutoffPosition {
		t.Fatalf("balance: %+v", c.Balance)
	}
	if st.Scope.Coverage != predictioneval.CoverageCompleteDeclared || !st.Scope.HasInterval ||
		int64(st.Scope.IntervalFromPosition) != ep.Boundary.CutoffPosition || int64(st.Scope.IntervalToPosition) != ep.Boundary.CutoffPosition {
		t.Fatalf("scope: %+v", st.Scope)
	}
	// The boundary is proven upstream, so the native projection sees no
	// intervention in [C, C] and says so, beside the view qualification; it
	// establishes no cut of its own.
	if st.Cutoff.Established || len(st.Qualifications) != 2 ||
		!strings.HasPrefix(st.Qualifications[0], "NO_INTERVENTION_IN_DECLARED_INTERVAL") ||
		!strings.HasPrefix(st.Qualifications[1], "CALCULATE_ONLY_VIEW") {
		t.Fatalf("cutoff %+v qualifications %v", st.Cutoff, st.Qualifications)
	}
	encoded, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"MARKER_STRATEGY", "marker_filter", "987654", "12.345", "67.89", "4321", "marker-health", "55555", "marker-choice", "3.5"} {
		if strings.Contains(string(encoded), marker) {
			t.Fatalf("P2-only or recorded data %q leaked into the P3b stream", marker)
		}
	}
	// The mechanism's answer is invariant under P2-only changes.
	rs := mustVerify(t, rulesetFrom(t, cfgWithRule("inv", predictioneval.ComparatorGe, 50, 100)))
	a, err := p4offline.EvaluateP3bCase(fs, rs, "key", 0)
	if err != nil {
		t.Fatal(err)
	}
	_, plain := selectedFactset(t, nil, nil)
	b, err := p4offline.EvaluateP3bCase(plain, rs, "key", 0)
	if err != nil {
		t.Fatal(err)
	}
	if a.Action.Class != b.Action.Class || a.Choice != b.Choice || a.Stake != b.Stake || a.Trace.WordsConsumed != b.Trace.WordsConsumed {
		t.Fatalf("P2-only inputs changed the P3b answer:\n%+v\n%+v", a, b)
	}
	if a.Action.Class != p4offline.ActionWouldAttempt || a.Choice.OutcomeID != "o1" || !a.Stake.Known() || a.Stake.Value != 100 {
		t.Fatalf("o1 holds 60%% >= 50%% at a 100%% rate: 10%% of 1000 is 100: %+v %+v %+v", a.Action, a.Choice, a.Stake)
	}
	if a.Policy != p4offline.PolicyP3b || a.FactsetDigest != fs.Digest || a.RulesetID != "inv" ||
		a.NativeConfigDigest != rs.NativeConfigDigest || a.Evaluation.ConfigDigest != rs.NativeConfigDigest ||
		a.Evaluation.StreamDigest != a.Projection.Stream.SelectionDigest {
		t.Fatalf("the P3b result must be bound to its factset, ruleset and stream: %+v", a)
	}
	if a.Trace.Coordinates != (p4offline.EntropyCoordinates{Trajectory: 0, SourceRoundID: "e1", PolicyID: "inv"}) {
		t.Fatalf("trace coordinates: %+v", a.Trace.Coordinates)
	}
	if a.Trace.EntropyDigest == "" || a.Trace.EntropyDigest != a.Evaluation.EntropyDigest {
		t.Fatalf("the entropy binding must be the core's own: %+v", a.Trace)
	}
	if _, err := p4offline.ProjectP3bSingleCandidate(p4offline.CommonFactset{}); !errors.Is(err, p4offline.ErrFactsetDigest) {
		t.Fatalf("an unverifiable factset cannot be projected: %v", err)
	}
	_, incomplete := selectedFactset(t, func(e *predictioneval.SourceDecisionEnvelope) { e.Settings.StealthMode = true }, nil)
	if _, err := p4offline.ProjectP3bSingleCandidate(incomplete); !errors.Is(err, p4offline.ErrFactsetNotEvaluable) {
		t.Fatalf("both policies admit the same COMPLETE factsets: %v", err)
	}
}

// TestP3bEntropyConsumptionThroughTheP4Trace pins the four consumption
// facts through the P4 trace: 0% consumes one word and fails, 100% consumes
// none, the default consumes none, and an exhausted trace is an explicit
// UNKNOWN_INPUT — never a default draw.
func TestP3bEntropyConsumptionThroughTheP4Trace(t *testing.T) {
	_, fs := selectedFactset(t, nil, nil)
	t.Run("a matched rule at 0% consumes one word and admits nothing", func(t *testing.T) {
		rs := mustVerify(t, rulesetFrom(t, cfgWithRule("zero", predictioneval.ComparatorGe, 50, 0)))
		res, err := p4offline.EvaluateP3bCase(fs, rs, "key", 5)
		if err != nil {
			t.Fatal(err)
		}
		if res.Action.Class != p4offline.ActionNoAttemptInSuppliedPrefix || res.Trace.WordsConsumed != 1 ||
			res.Evaluation.BernoulliEvaluations != 1 {
			t.Fatalf("%+v consumed=%d", res.Action, res.Trace.WordsConsumed)
		}
		if res.Trace.WordsSupplied != 2 { // 1 rule x 2 outcomes
			t.Fatalf("the trace supplies one word per rule per outcome: %d", res.Trace.WordsSupplied)
		}
		if err := p4offline.ValidateEntropyWords("key", res.Trace.Coordinates, []predictioneval.OrderedRulesHex64{res.Evaluation.Trace[0].RawWordValue}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a matched rule at 100% consumes no word", func(t *testing.T) {
		rs := mustVerify(t, rulesetFrom(t, cfgWithRule("one", predictioneval.ComparatorGe, 50, 100)))
		res, err := p4offline.EvaluateP3bCase(fs, rs, "key", 5)
		if err != nil {
			t.Fatal(err)
		}
		if res.Action.Class != p4offline.ActionWouldAttempt || res.Trace.WordsConsumed != 0 || res.Evaluation.BernoulliEvaluations != 1 {
			t.Fatalf("%+v consumed=%d", res.Action, res.Trace.WordsConsumed)
		}
	})
	t.Run("a default admission consumes no word", func(t *testing.T) {
		rs := mustVerify(t, rulesetFrom(t, cfgDefaultOnly("dflt", 55, 65)))
		res, err := p4offline.EvaluateP3bCase(fs, rs, "key", 5)
		if err != nil {
			t.Fatal(err)
		}
		if res.Action.Class != p4offline.ActionWouldAttempt || res.Choice.OutcomeID != "o1" ||
			res.Trace.WordsConsumed != 0 || res.Trace.WordsSupplied != 0 || res.Evaluation.BernoulliEvaluations != 0 {
			t.Fatalf("%+v %+v consumed=%d supplied=%d", res.Action, res.Choice, res.Trace.WordsConsumed, res.Trace.WordsSupplied)
		}
	})
	t.Run("an exhausted trace is UNKNOWN_INPUT, not a default draw", func(t *testing.T) {
		rs := mustVerify(t, rulesetFrom(t, cfgWithRule("half", predictioneval.ComparatorGe, 50, 50)))
		coords := p4offline.EntropyCoordinates{Trajectory: 1, SourceRoundID: "e1", PolicyID: "half"}
		empty, _ := p4offline.BuildDrawTrace("key", coords, 0)
		res, err := p4offline.EvaluateP3bWithTrace(fs, rs, "key", coords, empty)
		if err != nil {
			t.Fatal(err)
		}
		if res.Action.Class != p4offline.ActionUnknownInput || res.Evaluation.Reason != predictioneval.ReasonEntropyExhausted ||
			res.Stake.Known() || res.Choice.Present {
			t.Fatalf("%+v %+v %+v", res.Action, res.Stake, res.Evaluation.Reason)
		}
		// A trace that does not belong to the coordinates is refused before
		// the core sees it.
		wrong, _ := p4offline.BuildDrawTrace("other-key", coords, 2)
		if _, err := p4offline.EvaluateP3bWithTrace(fs, rs, "key", coords, wrong); !errors.Is(err, p4offline.ErrEntropyWordMismatch) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a trace drawn for another round or another ruleset is refused", func(t *testing.T) {
		rs := mustVerify(t, rulesetFrom(t, cfgWithRule("half", predictioneval.ComparatorGe, 50, 50)))
		foreignRound := p4offline.EntropyCoordinates{Trajectory: 3, SourceRoundID: "SOME-OTHER-ROUND", PolicyID: "half"}
		trace, _ := p4offline.BuildDrawTrace("key", foreignRound, 2)
		if _, err := p4offline.EvaluateP3bWithTrace(fs, rs, "key", foreignRound, trace); !errors.Is(err, p4offline.ErrP3bBinding) {
			t.Fatalf("round e1 must not be drawn under another round's words: %v", err)
		}
		foreignPolicy := p4offline.EntropyCoordinates{Trajectory: 3, SourceRoundID: "e1", PolicyID: "other-ruleset"}
		trace, _ = p4offline.BuildDrawTrace("key", foreignPolicy, 2)
		if _, err := p4offline.EvaluateP3bWithTrace(fs, rs, "key", foreignPolicy, trace); !errors.Is(err, p4offline.ErrP3bBinding) {
			t.Fatalf("got %v", err)
		}
		// The in-package path names the coordinates itself.
		res, err := p4offline.EvaluateP3bCase(fs, rs, "key", 3)
		if err != nil {
			t.Fatal(err)
		}
		if res.Trace.Coordinates != (p4offline.EntropyCoordinates{Trajectory: 3, SourceRoundID: "e1", PolicyID: "half"}) {
			t.Fatalf("%+v", res.Trace.Coordinates)
		}
		if _, err := p4offline.EvaluateP3bCase(fs, rs, "key", p4offline.TrajectoryCount); !errors.Is(err, p4offline.ErrEntropyCoordinates) {
			t.Fatalf("a trajectory outside the protocol: %v", err)
		}
	})
	t.Run("two trajectories draw different words", func(t *testing.T) {
		rs := mustVerify(t, rulesetFrom(t, cfgWithRule("half", predictioneval.ComparatorGe, 50, 50)))
		a, _ := p4offline.EvaluateP3bCase(fs, rs, "key", 0)
		b, _ := p4offline.EvaluateP3bCase(fs, rs, "key", 1)
		if a.Trace.RunID == b.Trace.RunID || a.Trace.EntropyDigest == b.Trace.EntropyDigest {
			t.Fatalf("trajectories 0 and 1 share entropy: %+v / %+v", a.Trace, b.Trace)
		}
	})
}

// TestP3bKnownZeroStakeIsWouldAttemptAndNoAttemptPrefixIsNotASkip pins the
// three financial distinctions on the P3b side.
func TestP3bKnownZeroStakeIsWouldAttemptAndNoAttemptPrefixIsNotASkip(t *testing.T) {
	_, zero := selectedFactset(t, func(e *predictioneval.SourceDecisionEnvelope) {
		e.Balance = ptrI64(0)
		e.ChoiceAmount = ptrI64(0)
		e.StakeAllowed = ptrI64(0)
		e.FinalAmount = ptrI64(0)
	}, nil)
	rs := mustVerify(t, rulesetFrom(t, cfgWithRule("one", predictioneval.ComparatorGe, 50, 100)))
	res, err := p4offline.EvaluateP3bCase(zero, rs, "key", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action.Class != p4offline.ActionWouldAttempt || !res.Stake.Known() || res.Stake.Value != 0 || res.Choice.OutcomeID != "o1" {
		t.Fatalf("a KNOWN zero stake is still WOULD_ATTEMPT: %+v %+v", res.Action, res.Stake)
	}
	// The same factset under P2 is a POLICY_SKIP: the two classes are distinct
	// facts about one factset, and neither is rewritten into the other.
	p2, err := p4offline.EvaluateP2Case(zero)
	if err != nil {
		t.Fatal(err)
	}
	if p2.Action.Class != p4offline.ActionPolicySkip {
		t.Fatalf("%+v", p2.Action)
	}

	_, fs := selectedFactset(t, nil, nil)
	none := mustVerify(t, rulesetFrom(t, cfgDefaultOnly("none", 95, 100)))
	res, err = p4offline.EvaluateP3bCase(fs, none, "key", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action.Class != p4offline.ActionNoAttemptInSuppliedPrefix || res.Action.Class == p4offline.ActionPolicySkip {
		t.Fatalf("%+v", res.Action)
	}
	if res.Stake.Presence != p4offline.PresenceUnknown || res.Stake.Value != 0 || res.Choice.Present {
		t.Fatalf("NO_ATTEMPT_IN_SUPPLIED_PREFIX carries no financial zero: %+v", res.Stake)
	}
}

// TestP3bUint32AndVectorBoundariesAreRefusedNotClamped pins the u32 domain
// and the vector presence through the P4 projection.
func TestP3bUint32AndVectorBoundariesAreRefusedNotClamped(t *testing.T) {
	rs := mustVerify(t, rulesetFrom(t, cfgWithRule("one", predictioneval.ComparatorGe, 50, 100)))
	t.Run("a balance past u32 leaves the admission standing and the stake unknown", func(t *testing.T) {
		_, fs := selectedFactset(t, func(e *predictioneval.SourceDecisionEnvelope) {
			e.Balance = ptrI64(math.MaxUint32 + 1)
		}, nil)
		res, err := p4offline.EvaluateP3bCase(fs, rs, "key", 0)
		if err != nil {
			t.Fatal(err)
		}
		if res.Action.Class != p4offline.ActionParticipationAdmittedStakeUnknown || !res.Choice.Present ||
			res.Choice.OutcomeID != "o1" || res.Stake.Known() || res.Stake.Reason != predictioneval.ReasonBalanceOutOfDomain {
			t.Fatalf("%+v %+v %+v", res.Action, res.Choice, res.Stake)
		}
	})
	t.Run("a balance exactly at u32 sizes the stake", func(t *testing.T) {
		_, fs := selectedFactset(t, func(e *predictioneval.SourceDecisionEnvelope) {
			e.Balance = ptrI64(math.MaxUint32)
		}, nil)
		res, err := p4offline.EvaluateP3bCase(fs, rs, "key", 0)
		if err != nil {
			t.Fatal(err)
		}
		// 10% of 4294967295 is 429496729.5, truncated to 429496729.
		if res.Action.Class != p4offline.ActionWouldAttempt || !res.Stake.Known() || res.Stake.Value != 429496729 {
			t.Fatalf("%+v %+v", res.Action, res.Stake)
		}
	})
	t.Run("negative points are out of domain", func(t *testing.T) {
		_, fs := selectedFactset(t, func(e *predictioneval.SourceDecisionEnvelope) { e.Outcomes[1].TotalPoints = -1 }, nil)
		res, err := p4offline.EvaluateP3bCase(fs, rs, "key", 0)
		if err != nil {
			t.Fatal(err)
		}
		if res.Action.Class != p4offline.ActionUnknownInput || res.Evaluation.Reason != predictioneval.ReasonOutcomePointsOutOfDomain {
			t.Fatalf("%+v %s", res.Action, res.Evaluation.Reason)
		}
	})
	t.Run("a hole in the model vector is an unknown vector", func(t *testing.T) {
		_, fs := selectedFactset(t, func(e *predictioneval.SourceDecisionEnvelope) { e.Outcomes[1].Present = false }, nil)
		proj, err := p4offline.ProjectP3bSingleCandidate(fs)
		if err != nil {
			t.Fatal(err)
		}
		if len(proj.Stream.Candidates) != 1 {
			t.Fatalf("projection holds %d candidates", len(proj.Stream.Candidates))
		}
		if proj.Stream.Candidates[0].OutcomesPresence != predictioneval.SuppliedInvalid {
			t.Fatalf("a partially present vector is declared INVALID, never passed off as whole: %+v", proj.Stream.Candidates[0])
		}
		res, err := p4offline.EvaluateP3bCase(fs, rs, "key", 0)
		if err != nil {
			t.Fatal(err)
		}
		if res.Action.Class != p4offline.ActionUnknownInput || res.Evaluation.Reason != predictioneval.ReasonOutcomeVectorNotKnown {
			t.Fatalf("%+v %s", res.Action, res.Evaluation.Reason)
		}
	})
	t.Run("an outcome without an identity cannot be projected", func(t *testing.T) {
		_, fs := selectedFactset(t, func(e *predictioneval.SourceDecisionEnvelope) { e.Outcomes[1].ID = "" }, nil)
		if _, err := p4offline.ProjectP3bSingleCandidate(fs); !errors.Is(err, p4offline.ErrP3bProjectionRefused) {
			t.Fatalf("got %v", err)
		}
		res, err := p4offline.EvaluateP3bCase(fs, rs, "key", 0)
		if err != nil {
			t.Fatal(err)
		}
		if res.Action.Class != p4offline.ActionRefused || res.Action.NativeAction != "P4_PROJECTION_REFUSED" || res.Stake.Known() {
			t.Fatalf("a refused projection is a typed REFUSED, never a skip: %+v", res.Action)
		}
	})
}

// TestP3bNativeActionMapIsExhaustive drives every native P3b status through
// the map and fails closed on contradicted shapes.
func TestP3bNativeActionMapIsExhaustive(t *testing.T) {
	_, fs := selectedFactset(t, nil, nil)
	proj, err := p4offline.ProjectP3bSingleCandidate(fs)
	if err != nil {
		t.Fatal(err)
	}
	coords := p4offline.EntropyCoordinates{Trajectory: 0, SourceRoundID: "e1", PolicyID: "x"}
	trace, _ := p4offline.BuildDrawTrace("key", coords, 4)
	// A missing-balance stream, built natively, for the stake-unknown status.
	missingBalance := proj.Stream
	missingBalance.Candidates = append([]predictioneval.OrderedRulesCandidate(nil), proj.Stream.Candidates...)
	missingBalance.Candidates[0].Balance = predictioneval.SuppliedInt64{Presence: predictioneval.SuppliedMissing, Reason: "test"}
	// Re-project so the native selection digest matches the changed candidate.
	{
		src := predictioneval.OrderedRulesSource{Scope: proj.Stream.Scope, Candidates: missingBalance.Candidates}
		reproj, err := predictioneval.ProjectOrderedRulesStream(src, proj.Stream.Admission)
		if err != nil {
			t.Fatal(err)
		}
		missingBalance = reproj
	}

	legal := []struct {
		name   string
		stream predictioneval.OrderedRulesStream
		cfg    predictioneval.OrderedRulesConfig
		trace  predictioneval.SuppliedDrawTrace
		native predictioneval.OrderedRulesStatus
		class  p4offline.ActionClass
	}{
		{"would attempt", proj.Stream, cfgWithRule("a", predictioneval.ComparatorGe, 50, 100), trace, predictioneval.StatusWouldAttempt, p4offline.ActionWouldAttempt},
		{"stake unknown", missingBalance, cfgWithRule("a", predictioneval.ComparatorGe, 50, 100), trace, predictioneval.StatusParticipationAdmittedStakeUnknown, p4offline.ActionParticipationAdmittedStakeUnknown},
		{"no attempt in prefix", proj.Stream, cfgDefaultOnly("a", 95, 100), trace, predictioneval.StatusNoAttemptInSuppliedPrefix, p4offline.ActionNoAttemptInSuppliedPrefix},
		{"unknown input", proj.Stream, cfgWithRule("a", predictioneval.ComparatorGe, 50, 50), predictioneval.SuppliedDrawTrace{RunID: "r", EntropySemanticsVersion: predictioneval.OrderedRulesEntropySemanticsVersion}, predictioneval.StatusUnknownInput, p4offline.ActionUnknownInput},
		{"refused", proj.Stream, predictioneval.OrderedRulesConfig{ConfigID: "nodefault"}, trace, predictioneval.StatusRefused, p4offline.ActionRefused},
	}
	seen := map[predictioneval.OrderedRulesStatus]bool{}
	var placed predictioneval.OrderedRulesEvaluation
	for _, tc := range legal {
		t.Run(tc.name, func(t *testing.T) {
			ev := predictioneval.EvaluateOrderedRules(tc.stream, tc.cfg, tc.trace)
			if ev.Status != tc.native {
				t.Fatalf("fixture produced %q/%q, want %q", ev.Status, ev.Reason, tc.native)
			}
			seen[ev.Status] = true
			if tc.native == predictioneval.StatusWouldAttempt {
				placed = ev
			}
			m := p4offline.MapP3bAction(ev)
			if m.MapVersion != p4offline.NativeActionMapVersion || m.Policy != p4offline.PolicyP3b || m.NativeAction != string(tc.native) {
				t.Fatalf("%+v", m)
			}
			if !m.Legal || m.Class != tc.class {
				t.Fatalf("native %q must map LEGALLY to %s: %+v", tc.native, tc.class, m)
			}
			if m.Class == p4offline.ActionPolicySkip {
				t.Fatalf("no P3b status is ever a POLICY_SKIP: %+v", m)
			}
		})
	}
	for _, s := range []predictioneval.OrderedRulesStatus{predictioneval.StatusWouldAttempt, predictioneval.StatusParticipationAdmittedStakeUnknown,
		predictioneval.StatusNoAttemptInSuppliedPrefix, predictioneval.StatusUnknownInput, predictioneval.StatusRefused} {
		if !seen[s] {
			t.Errorf("status %q not exercised", s)
		}
	}
	illegal := []struct {
		name   string
		tamper func(*predictioneval.OrderedRulesEvaluation)
	}{
		{"attempt without a known stake", func(e *predictioneval.OrderedRulesEvaluation) { e.Stake.Presence = predictioneval.SuppliedMissing }},
		{"attempt without a selection", func(e *predictioneval.OrderedRulesEvaluation) { e.Selected = nil }},
		{"attempt with a reason", func(e *predictioneval.OrderedRulesEvaluation) { e.Reason = "X" }},
		{"attempt with participation not admitted", func(e *predictioneval.OrderedRulesEvaluation) {
			e.Participation = predictioneval.ParticipationNotAdmitted
		}},
		{"no-attempt name on an admitted shape", func(e *predictioneval.OrderedRulesEvaluation) {
			e.Status = predictioneval.StatusNoAttemptInSuppliedPrefix
		}},
		{"unknown-input name without a reason", func(e *predictioneval.OrderedRulesEvaluation) {
			e.Status = predictioneval.StatusUnknownInput
			e.Selected = nil
			e.Participation = predictioneval.ParticipationNotAdmitted
			e.Stake.Presence = predictioneval.SuppliedMissing
		}},
		{"a new status", func(e *predictioneval.OrderedRulesEvaluation) { e.Status = "SOMETHING_NEW" }},
		{"an empty status", func(e *predictioneval.OrderedRulesEvaluation) { e.Status = "" }},
		{"a foreign evidence label", func(e *predictioneval.OrderedRulesEvaluation) { e.EvidenceLabel = "OTHER" }},
		{"a foreign model version", func(e *predictioneval.OrderedRulesEvaluation) { e.ModelVersion = "other/v9" }},
	}
	for _, tc := range illegal {
		t.Run(tc.name, func(t *testing.T) {
			ev := placed
			if ev.Selected != nil {
				sel := *ev.Selected
				ev.Selected = &sel
			}
			tc.tamper(&ev)
			m := p4offline.MapP3bAction(ev)
			if m.Legal || m.Class != p4offline.ActionUnsupportedShape || len(m.Illegality) == 0 {
				t.Fatalf("must fail closed: %+v", m)
			}
		})
	}
}
