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
	"sort"
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

// mustMarshal serializes v or fails the test. Serialization is setup for the
// round-trip and digest tests that follow it: a discarded error there would
// leave a nil document whose later assertion could pass for the wrong reason.
func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	return raw
}

// mustDrawTrace builds the trace or fails the test. The refusal tests below
// assert a LATER error (a binding or an unverified ruleset); a trace that was
// never built would satisfy some of them without exercising the path at all,
// so a construction failure must surface as itself, here.
func mustDrawTrace(t *testing.T, coords p4offline.EntropyCoordinates, count int) predictioneval.SuppliedDrawTrace {
	t.Helper()
	trace, err := p4offline.BuildDrawTrace(coords, count)
	if err != nil {
		t.Fatalf("BuildDrawTrace(%d words): %v", count, err)
	}
	return trace
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
		if _, err := p4offline.EvaluateP3bCase(fs, forged, synthCoords(fs, 0)); !errors.Is(err, p4offline.ErrRulesetNotVerified) {
			t.Fatalf("got %v", err)
		}
		coords := synthCoords(fs, 0)
		trace := mustDrawTrace(t, coords, 2)
		if _, err := p4offline.EvaluateP3bWithTrace(fs, forged, coords, trace); !errors.Is(err, p4offline.ErrRulesetNotVerified) {
			t.Fatalf("got %v", err)
		}
		altered := rs
		altered.RulesetID = "renamed-after-verification"
		if _, err := p4offline.EvaluateP3bCase(fs, altered, synthCoords(fs, 0)); !errors.Is(err, p4offline.ErrRulesetNotVerified) {
			t.Fatalf("got %v", err)
		}
		altered = rs
		altered.RawSHA256 = strings.Repeat("0", 64)
		if _, err := p4offline.EvaluateP3bCase(fs, altered, synthCoords(fs, 0)); !errors.Is(err, p4offline.ErrRulesetNotVerified) {
			t.Fatalf("got %v", err)
		}
		retuned := rs
		retuned.Config.Detailed = append([]predictioneval.OrderedRule(nil), rs.Config.Detailed...)
		retuned.Config.Detailed[0].RawAttemptRatePercent = 0
		if _, err := p4offline.EvaluateP3bCase(fs, retuned, synthCoords(fs, 0)); !errors.Is(err, p4offline.ErrRulesetNotVerified) {
			t.Fatalf("a config altered after verification no longer digests to the verified native digest: %v", err)
		}
		broken := rs
		broken.Config.Detailed = append([]predictioneval.OrderedRule(nil), rs.Config.Detailed...)
		broken.Config.Detailed[0].RawThresholdPercent = 500 // a shape the core refuses outright
		if _, err := p4offline.EvaluateP3bCase(fs, broken, synthCoords(fs, 0)); !errors.Is(err, p4offline.ErrRulesetNotVerified) {
			t.Fatalf("a config the core refuses is not a typed REFUSED under the honest identity; it is an unverified ruleset: %v", err)
		}
		var decoded p4offline.VerifiedP3bRuleset
		raw := mustMarshal(t, rs)
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if _, err := p4offline.EvaluateP3bCase(fs, decoded, synthCoords(fs, 0)); !errors.Is(err, p4offline.ErrRulesetNotVerified) {
			t.Fatalf("a verified ruleset does not survive a round trip; the raw bytes must be re-verified: %v", err)
		}
	})
	t.Run("a config the core refuses is unusable", func(t *testing.T) {
		bad := cfgWithRule("bad", predictioneval.ComparatorGe, 150, 100) // threshold out of domain
		raw := mustMarshal(t, bad)
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
	a, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, 0))
	if err != nil {
		t.Fatal(err)
	}
	_, plain := selectedFactset(t, nil, nil)
	b, err := p4offline.EvaluateP3bCase(plain, rs, synthCoords(plain, 0))
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
	if a.Trace.Coordinates != synthCoords(fs, 0) {
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
		res, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, 5))
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
		if err := p4offline.ValidateEntropyWords(res.Trace.Coordinates, []predictioneval.OrderedRulesHex64{res.Evaluation.Trace[0].RawWordValue}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a matched rule at 100% consumes no word", func(t *testing.T) {
		rs := mustVerify(t, rulesetFrom(t, cfgWithRule("one", predictioneval.ComparatorGe, 50, 100)))
		res, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, 5))
		if err != nil {
			t.Fatal(err)
		}
		if res.Action.Class != p4offline.ActionWouldAttempt || res.Trace.WordsConsumed != 0 || res.Evaluation.BernoulliEvaluations != 1 {
			t.Fatalf("%+v consumed=%d", res.Action, res.Trace.WordsConsumed)
		}
	})
	t.Run("a default admission consumes no word", func(t *testing.T) {
		rs := mustVerify(t, rulesetFrom(t, cfgDefaultOnly("dflt", 55, 65)))
		res, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, 5))
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
		coords := synthCoords(fs, 1)
		empty := mustDrawTrace(t, coords, 0)
		res, err := p4offline.EvaluateP3bWithTrace(fs, rs, coords, empty)
		if err != nil {
			t.Fatal(err)
		}
		if res.Action.Class != p4offline.ActionUnknownInput || res.Evaluation.Reason != predictioneval.ReasonEntropyExhausted ||
			res.Stake.Known() || res.Choice.Present {
			t.Fatalf("%+v %+v %+v", res.Action, res.Stake, res.Evaluation.Reason)
		}
		// A trace whose words are not the algorithm's for the coordinates is
		// refused before the core sees it.
		wrong := mustDrawTrace(t, coords, 2)
		wrong.Words = append([]predictioneval.OrderedRulesHex64(nil), wrong.Words...)
		wrong.Words[0] ^= 1
		if _, err := p4offline.EvaluateP3bWithTrace(fs, rs, coords, wrong); !errors.Is(err, p4offline.ErrEntropyWordMismatch) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a trace drawn for another opportunity or another factset is refused", func(t *testing.T) {
		rs := mustVerify(t, rulesetFrom(t, cfgWithRule("half", predictioneval.ComparatorGe, 50, 50)))
		foreignRound := synthCoords(fs, 3)
		foreignRound.PairedOpportunityID = "SOME-OTHER-ROUND"
		trace := mustDrawTrace(t, foreignRound, 2)
		if _, err := p4offline.EvaluateP3bWithTrace(fs, rs, foreignRound, trace); !errors.Is(err, p4offline.ErrP3bBinding) {
			t.Fatalf("round e1 must not be drawn under another opportunity's words: %v", err)
		}
		foreignFactset := synthCoords(fs, 3)
		foreignFactset.CommonFactsetDigest = p4offline.DigestReference(strings.Repeat("0", 64))
		trace = mustDrawTrace(t, foreignFactset, 2)
		if _, err := p4offline.EvaluateP3bWithTrace(fs, rs, foreignFactset, trace); !errors.Is(err, p4offline.ErrP3bBinding) {
			t.Fatalf("a factset must not be drawn under another factset's words: %v", err)
		}
		// The digest must be spelled as the protocol spells it; the bare
		// hex is outside the protocol, not a second spelling of the same
		// coordinates.
		bare := synthCoords(fs, 3)
		bare.CommonFactsetDigest = fs.Digest
		if _, err := p4offline.BuildDrawTrace(bare, 2); !errors.Is(err, p4offline.ErrEntropyCoordinates) {
			t.Fatalf("got %v", err)
		}
		// The in-package path carries the coordinates it was given, bound.
		res, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, 3))
		if err != nil {
			t.Fatal(err)
		}
		if res.Trace.Coordinates != synthCoords(fs, 3) {
			t.Fatalf("%+v", res.Trace.Coordinates)
		}
		if _, err := p4offline.EvaluateP3bCase(fs, rs, foreignRound); !errors.Is(err, p4offline.ErrP3bBinding) {
			t.Fatalf("got %v", err)
		}
		if _, err := p4offline.EvaluateP3bCase(fs, rs, foreignFactset); !errors.Is(err, p4offline.ErrP3bBinding) {
			t.Fatalf("got %v", err)
		}
		if _, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, p4offline.TrajectoryCount)); !errors.Is(err, p4offline.ErrEntropyCoordinates) {
			t.Fatalf("a trajectory outside the protocol: %v", err)
		}
		// Two rulesets over the same opportunity and run draw the SAME words:
		// the approved framing has no ruleset coordinate.
		other := mustVerify(t, rulesetFrom(t, cfgWithRule("other-half", predictioneval.ComparatorGe, 50, 50)))
		a, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, 3))
		if err != nil {
			t.Fatal(err)
		}
		b, err := p4offline.EvaluateP3bCase(fs, other, synthCoords(fs, 3))
		if err != nil {
			t.Fatal(err)
		}
		if a.Trace.RunID != b.Trace.RunID || a.Evaluation.Trace[0].RawWordValue != b.Evaluation.Trace[0].RawWordValue {
			t.Fatalf("candidate rulesets must see common words: %+v / %+v", a.Trace, b.Trace)
		}
	})
	t.Run("two trajectories draw different words", func(t *testing.T) {
		rs := mustVerify(t, rulesetFrom(t, cfgWithRule("half", predictioneval.ComparatorGe, 50, 50)))
		a, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, 0))
		if err != nil {
			t.Fatalf("trajectory 0: %v", err)
		}
		b, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, 1))
		if err != nil {
			t.Fatalf("trajectory 1: %v", err)
		}
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
	res, err := p4offline.EvaluateP3bCase(zero, rs, synthCoords(zero, 0))
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
	res, err = p4offline.EvaluateP3bCase(fs, none, synthCoords(fs, 0))
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
		res, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, 0))
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
		res, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, 0))
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
		res, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, 0))
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
		res, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, 0))
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
		res, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, 0))
		if err != nil {
			t.Fatal(err)
		}
		if res.Action.Class != p4offline.ActionRefused || res.Action.NativeAction != "P4_PROJECTION_REFUSED" || res.Stake.Known() {
			t.Fatalf("a refused projection is a typed REFUSED, never a skip: %+v", res.Action)
		}
		// Even a refused projection is bound to its coordinates and inside
		// the protocol: another opportunity's coordinates, a trajectory at
		// the count or an empty dataset identity yield no result at all,
		// exactly as on an evaluable factset; and the refused result names
		// the run it was produced under.
		if res.Trace.Coordinates != synthCoords(fs, 0) {
			t.Fatalf("a refused result must name its coordinates: %+v", res.Trace)
		}
		empty := mustDrawTrace(t, synthCoords(fs, 0), 0)
		if res.Trace.RunID == "" || res.Trace.RunID != empty.RunID {
			t.Fatalf("a refused result names the run by the empty trace's identity: %q", res.Trace.RunID)
		}
		foreign := synthCoords(fs, 0)
		foreign.PairedOpportunityID = "SOME-OTHER-ROUND"
		if _, err := p4offline.EvaluateP3bCase(fs, rs, foreign); !errors.Is(err, p4offline.ErrP3bBinding) {
			t.Fatalf("got %v", err)
		}
		if _, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, p4offline.TrajectoryCount)); !errors.Is(err, p4offline.ErrEntropyCoordinates) {
			t.Fatalf("a trajectory outside the protocol on the refused path: %v", err)
		}
		noDataset := synthCoords(fs, 0)
		noDataset.DatasetID = ""
		if _, err := p4offline.EvaluateP3bCase(fs, rs, noDataset); !errors.Is(err, p4offline.ErrEntropyCoordinates) {
			t.Fatalf("an empty dataset identity on the refused path: %v", err)
		}
		// The supplied-trace entry point refuses the same coordinates the
		// same way, before it projects anything.
		out := mustDrawTrace(t, synthCoords(fs, 0), 0)
		if _, err := p4offline.EvaluateP3bWithTrace(fs, rs, synthCoords(fs, p4offline.TrajectoryCount), out); !errors.Is(err, p4offline.ErrEntropyCoordinates) {
			t.Fatalf("the supplied-trace path must refuse an out-of-protocol trajectory as such: %v", err)
		}
		if _, err := p4offline.EvaluateP3bWithTrace(fs, rs, noDataset, out); !errors.Is(err, p4offline.ErrEntropyCoordinates) {
			t.Fatalf("got %v", err)
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
	coords := synthCoords(fs, 0)
	trace := mustDrawTrace(t, coords, 4)
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

// TestRulesetDocumentMustSpellEveryMandatoryKey pins the raw document's
// completeness: a mandatory key the document omits, or spells with a null,
// would decode to a zero the core cannot tell from an explicit one — a
// document without its default would pass as a resolved configuration whose
// default is [0,0] — so it is refused as a decode failure before any digest
// is read. Only "detailed" may be absent or null.
func TestRulesetDocumentMustSpellEveryMandatoryKey(t *testing.T) {
	dummy := strings.Repeat("a", 64)
	refused := map[string]string{
		"the default omitted":               `{"configId":"x","hasDefault":true}`,
		"the default null":                  `{"configId":"x","hasDefault":true,"default":null}`,
		"the default's points omitted":      `{"configId":"x","hasDefault":true,"default":{"rawMinPercent":95,"rawMaxPercent":100}}`,
		"a bound of the default omitted":    `{"configId":"x","hasDefault":true,"default":{"rawMinPercent":95,"points":{"maxValue":0,"rawPercent":1}}}`,
		"hasDefault omitted":                `{"configId":"x","default":{"rawMinPercent":95,"rawMaxPercent":100,"points":{"maxValue":0,"rawPercent":1}}}`,
		"configId null":                     `{"configId":null,"hasDefault":true,"default":{"rawMinPercent":95,"rawMaxPercent":100,"points":{"maxValue":0,"rawPercent":1}}}`,
		"a rule without its attempt rate":   `{"configId":"x","hasDefault":true,"detailed":[{"comparator":"Ge","rawThresholdPercent":50,"points":{"maxValue":0,"rawPercent":10}}],"default":{"rawMinPercent":95,"rawMaxPercent":100,"points":{"maxValue":0,"rawPercent":1}}}`,
		"a rule's points without a percent": `{"configId":"x","hasDefault":true,"detailed":[{"comparator":"Ge","rawThresholdPercent":50,"rawAttemptRatePercent":100,"points":{"maxValue":0}}],"default":{"rawMinPercent":95,"rawMaxPercent":100,"points":{"maxValue":0,"rawPercent":1}}}`,
		"a null rule":                       `{"configId":"x","hasDefault":true,"detailed":[null],"default":{"rawMinPercent":95,"rawMaxPercent":100,"points":{"maxValue":0,"rawPercent":1}}}`,
	}
	for name, doc := range refused {
		raw := []byte(doc)
		sum := sha256.Sum256(raw)
		r := p4offline.P3bRuleset{RulesetID: "x", RawBytes: raw, RawSHA256: hex.EncodeToString(sum[:]),
			Config: predictioneval.OrderedRulesConfig{ConfigID: "x", HasDefault: true}, NativeConfigDigest: dummy}
		if _, err := p4offline.VerifyP3bRuleset(r); !errors.Is(err, p4offline.ErrRulesetRawDecode) {
			t.Errorf("%s: must be refused as a decode failure, got %v", name, err)
		}
	}
	// The one omission the contract admits: no detailed rules, absent or null.
	nullDetailed := []byte(`{"configId":"x","hasDefault":true,"detailed":null,"default":{"rawMinPercent":95,"rawMaxPercent":100,"points":{"maxValue":0,"rawPercent":1}}}`)
	cfg := cfgDefaultOnly("x", 95, 100)
	sum := sha256.Sum256(nullDetailed)
	if _, err := p4offline.VerifyP3bRuleset(p4offline.P3bRuleset{RulesetID: "x", RawBytes: nullDetailed,
		RawSHA256: hex.EncodeToString(sum[:]), Config: cfg, NativeConfigDigest: nativeConfigDigest(t, cfg)}); err != nil {
		t.Fatalf("a null detailed list means no detailed rules: %v", err)
	}
	mustVerify(t, rulesetFrom(t, cfg)) // absent detailed, the same thing
}

// sameStrings reports whether two name sets hold the same names, in any order.
// Illegality is documented as naming EVERY contradiction found, so a shape that
// contradicts two independent records of the same fact is expected to name both
// — the assertion is an exact set, not a containment.
func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	for i := range g {
		if g[i] != w[i] {
			return false
		}
	}
	return true
}

// TestP3bSelectionMustBeIntrinsicallyConsistent pins the selection half of the
// P3b mapping seam. A status that carries a selection is legal only if that
// selection is one the native mechanism could have produced, judged by the
// selection's OWN fields — the evaluator is not re-run and no other exported
// field is re-derived here. The invariants come from the native contract:
// candidate and outcome are slice indices with identities, the basis is one of
// two named halves, and RuleIndex is the admitting rule's index or -1 for a
// default admission (ordered_rules_types.go), which the producer pairs exactly
// (ordered_rules.go admits under DETAILED_RULE only with an index >= 0 and
// under DEFAULT only with -1).
func TestP3bSelectionMustBeIntrinsicallyConsistent(t *testing.T) {
	_, fs := selectedFactset(t, nil, nil)
	proj, err := p4offline.ProjectP3bSingleCandidate(fs)
	if err != nil {
		t.Fatal(err)
	}
	coords := synthCoords(fs, 0)
	trace := mustDrawTrace(t, coords, 4)

	missingBalance := proj.Stream
	missingBalance.Candidates = append([]predictioneval.OrderedRulesCandidate(nil), proj.Stream.Candidates...)
	missingBalance.Candidates[0].Balance = predictioneval.SuppliedInt64{Presence: predictioneval.SuppliedMissing, Reason: "test"}
	reproj, err := predictioneval.ProjectOrderedRulesStream(
		predictioneval.OrderedRulesSource{Scope: proj.Stream.Scope, Candidates: missingBalance.Candidates},
		proj.Stream.Admission)
	if err != nil {
		t.Fatal(err)
	}
	missingBalance = reproj

	carriers := []struct {
		name   string
		stream predictioneval.OrderedRulesStream
		native predictioneval.OrderedRulesStatus
	}{
		{"would attempt", proj.Stream, predictioneval.StatusWouldAttempt},
		{"stake unknown", missingBalance, predictioneval.StatusParticipationAdmittedStakeUnknown},
	}
	// A tamper may touch the evaluation as well as the selection, because these
	// invariants relate one to the other. want is the EXACT set of illegalities
	// the shape must produce: most fields are witnessed more than once — by a
	// counter, by the admitting trace entry and by the admitting visit — and
	// naming every one of them is the documented behaviour of Illegality rather
	// than a defect. Each set is derived from which witnesses carry the tampered
	// field AND from the suppression the guard applies — a field already named
	// malformed is not named a second time by a relation that would compare it
	// — rather than read back off a run.
	tampers := []struct {
		name string
		on   func(*predictioneval.OrderedRulesEvaluation, *predictioneval.OrderedRulesSelection)
		want []string
	}{
		{"a negative candidate index", func(_ *predictioneval.OrderedRulesEvaluation, sel *predictioneval.OrderedRulesSelection) {
			sel.CandidateIndex = -1
		}, []string{"SELECTION_CANDIDATE_INDEX_NEGATIVE", "SELECTION_CONTRADICTS_ADMITTING_TRACE", "SELECTION_CONTRADICTS_ADMITTING_VISIT"}},
		{"a candidate index past the declared ceiling", func(_ *predictioneval.OrderedRulesEvaluation, sel *predictioneval.OrderedRulesSelection) {
			sel.CandidateIndex = predictioneval.MaxOrderedRulesCandidates
		}, []string{"SELECTION_CANDIDATE_INDEX_OVER_CEILING", "SELECTION_CONTRADICTS_ADMITTING_TRACE", "SELECTION_CONTRADICTS_ADMITTING_VISIT"}},
		{"a negative outcome index", func(_ *predictioneval.OrderedRulesEvaluation, sel *predictioneval.OrderedRulesSelection) {
			sel.OutcomeIndex = -1
		}, []string{"SELECTION_OUTCOME_INDEX_NEGATIVE", "SELECTION_CONTRADICTS_ADMITTING_TRACE"}},
		{"an outcome index past the declared ceiling", func(_ *predictioneval.OrderedRulesEvaluation, sel *predictioneval.OrderedRulesSelection) {
			sel.OutcomeIndex = predictioneval.MaxOrderedRulesOutcomes
		}, []string{"SELECTION_OUTCOME_INDEX_OVER_CEILING", "SELECTION_CONTRADICTS_ADMITTING_TRACE"}},
		{"a candidate with no identity", func(_ *predictioneval.OrderedRulesEvaluation, sel *predictioneval.OrderedRulesSelection) {
			sel.CandidateIdentity = ""
		}, []string{"SELECTION_CANDIDATE_IDENTITY_EMPTY", "SELECTION_CONTRADICTS_ADMITTING_VISIT"}},
		{"an outcome with no identity", func(_ *predictioneval.OrderedRulesEvaluation, sel *predictioneval.OrderedRulesSelection) {
			sel.OutcomeIdentity = ""
		}, []string{"SELECTION_OUTCOME_IDENTITY_EMPTY"}},
		{"a basis outside the vocabulary", func(_ *predictioneval.OrderedRulesEvaluation, sel *predictioneval.OrderedRulesSelection) {
			sel.Basis = "SOME_OTHER_BASIS"
		}, []string{"SELECTION_BASIS_OUTSIDE_VOCABULARY"}},
		{"a default admission naming a rule", func(_ *predictioneval.OrderedRulesEvaluation, sel *predictioneval.OrderedRulesSelection) {
			sel.Basis, sel.RuleIndex = predictioneval.SelectionDefault, 0
		}, []string{"SELECTION_BASIS_CONTRADICTS_RULE_INDEX", "SELECTION_CONTRADICTS_ADMITTING_STEP"}},
		{"a detailed-rule admission naming no rule", func(_ *predictioneval.OrderedRulesEvaluation, sel *predictioneval.OrderedRulesSelection) {
			sel.Basis, sel.RuleIndex = predictioneval.SelectionDetailedRule, -1
		}, []string{"SELECTION_BASIS_CONTRADICTS_RULE_INDEX", "SELECTION_CONTRADICTS_ADMITTING_TRACE"}},
		{"a detailed-rule admission naming an impossible rule", func(_ *predictioneval.OrderedRulesEvaluation, sel *predictioneval.OrderedRulesSelection) {
			sel.Basis, sel.RuleIndex = predictioneval.SelectionDetailedRule, -5
		}, []string{"SELECTION_BASIS_CONTRADICTS_RULE_INDEX", "SELECTION_CONTRADICTS_ADMITTING_TRACE"}},
		{"a rule index past the declared ceiling", func(_ *predictioneval.OrderedRulesEvaluation, sel *predictioneval.OrderedRulesSelection) {
			sel.Basis, sel.RuleIndex = predictioneval.SelectionDetailedRule, predictioneval.MaxOrderedRulesRules
		}, []string{"SELECTION_RULE_INDEX_OVER_CEILING", "SELECTION_CONTRADICTS_ADMITTING_TRACE"}},
		{"a candidate the traversal never reached", func(_ *predictioneval.OrderedRulesEvaluation, sel *predictioneval.OrderedRulesSelection) {
			sel.CandidateIndex = 5
		}, []string{"SELECTION_CONTRADICTS_CANDIDATES_CONSUMED", "SELECTION_CONTRADICTS_VISIT_COUNT", "SELECTION_CONTRADICTS_ADMITTING_TRACE", "SELECTION_CONTRADICTS_ADMITTING_VISIT"}},
		{"a candidate earlier than the one stopped on", func(ev *predictioneval.OrderedRulesEvaluation, _ *predictioneval.OrderedRulesSelection) {
			// The relation is an EQUALITY, so a candidate short of the stop is
			// as contradicted as one past it. Nothing else moves: the selection
			// still names the candidate the trace and the visit admitted on.
			ev.CandidatesConsumed = 3
		}, []string{"SELECTION_CONTRADICTS_CANDIDATES_CONSUMED"}},
		{"a candidate other than the one stopped on", func(_ *predictioneval.OrderedRulesEvaluation, sel *predictioneval.OrderedRulesSelection) {
			sel.CandidateIdentity = "cand-forged"
		}, []string{"SELECTION_CONTRADICTS_STOPPED_CANDIDATE", "SELECTION_CONTRADICTS_ADMITTING_VISIT"}},
		{"a position other than the one stopped at", func(_ *predictioneval.OrderedRulesEvaluation, sel *predictioneval.OrderedRulesSelection) {
			sel.CandidatePosition += 999
		}, []string{"SELECTION_CONTRADICTS_STOPPED_POSITION", "SELECTION_CONTRADICTS_TRACE_POSITION", "SELECTION_CONTRADICTS_ADMITTING_VISIT"}},
		{"a rule the traversal never considered", func(_ *predictioneval.OrderedRulesEvaluation, sel *predictioneval.OrderedRulesSelection) {
			sel.Basis, sel.RuleIndex = predictioneval.SelectionDetailedRule, 5
		}, []string{"SELECTION_CONTRADICTS_RULES_CONSIDERED", "SELECTION_CONTRADICTS_ADMITTING_TRACE"}},
		{"a rule exactly one past what the traversal counted", func(ev *predictioneval.OrderedRulesEvaluation, sel *predictioneval.OrderedRulesSelection) {
			// The boundary form: the relation is strict, so the index EQUAL to
			// the count is already one the traversal never walked.
			sel.Basis, sel.RuleIndex = predictioneval.SelectionDetailedRule, ev.RulesConsidered
		}, []string{"SELECTION_CONTRADICTS_RULES_CONSIDERED", "SELECTION_CONTRADICTS_ADMITTING_TRACE"}},
		{"an outcome the traversal never considered", func(_ *predictioneval.OrderedRulesEvaluation, sel *predictioneval.OrderedRulesSelection) {
			sel.OutcomeIndex = 5
		}, []string{"SELECTION_CONTRADICTS_OUTCOMES_CONSIDERED", "SELECTION_CONTRADICTS_ADMITTING_TRACE"}},
		{"an outcome exactly one past what the traversal counted", func(ev *predictioneval.OrderedRulesEvaluation, sel *predictioneval.OrderedRulesSelection) {
			sel.OutcomeIndex = ev.OutcomesConsidered
		}, []string{"SELECTION_CONTRADICTS_OUTCOMES_CONSIDERED", "SELECTION_CONTRADICTS_ADMITTING_TRACE"}},
		{"a share the admitting step contradicts", func(_ *predictioneval.OrderedRulesEvaluation, sel *predictioneval.OrderedRulesSelection) {
			sel.ShareBits ^= 1
		}, []string{"SELECTION_CONTRADICTS_TRACE_SHARE"}},
		{"an admission the evaluation recorded no step for", func(ev *predictioneval.OrderedRulesEvaluation, _ *predictioneval.OrderedRulesSelection) {
			ev.Trace = nil
		}, []string{"SELECTION_WITHOUT_ADMITTING_TRACE"}},
		{"a last recorded step that admitted nothing", func(ev *predictioneval.OrderedRulesEvaluation, _ *predictioneval.OrderedRulesSelection) {
			trace := append([]predictioneval.OrderedRulesTraceEntry(nil), ev.Trace...)
			trace[len(trace)-1].Admitted = false
			ev.Trace = trace
		}, []string{"SELECTION_TRACE_STEP_NOT_ADMITTING"}},
		{"a step the selection's basis contradicts", func(ev *predictioneval.OrderedRulesEvaluation, sel *predictioneval.OrderedRulesSelection) {
			// The default half is documented to take NO draw, so a DEFAULT
			// admission sitting on a RULE_DRAW step is a shape the mechanism
			// cannot emit. (This fixture admits at rate 100%, which the donor
			// answers without consuming a word, so the step it sits on took a
			// draw without spending entropy — the step's own draw fields are
			// deliberately not read here.) Every other compared field is
			// co-edited to agree, so only the step is left to catch it.
			sel.Basis, sel.RuleIndex = predictioneval.SelectionDefault, -1
			trace := append([]predictioneval.OrderedRulesTraceEntry(nil), ev.Trace...)
			trace[len(trace)-1].RuleIndex = -1
			ev.Trace = trace
		}, []string{"SELECTION_CONTRADICTS_ADMITTING_STEP"}},
		{"a trace entry that alone contradicts the selection", func(ev *predictioneval.OrderedRulesEvaluation, _ *predictioneval.OrderedRulesSelection) {
			trace := append([]predictioneval.OrderedRulesTraceEntry(nil), ev.Trace...)
			trace[len(trace)-1].CandidateIndex += 7
			ev.Trace = trace
		}, []string{"SELECTION_CONTRADICTS_ADMITTING_TRACE"}},
		{"an admission the evaluation recorded no visit for", func(ev *predictioneval.OrderedRulesEvaluation, _ *predictioneval.OrderedRulesSelection) {
			// Both relations are contradicted and both are named: an evaluation
			// with no visits at all has no admitting visit AND a visit count
			// that cannot match the candidate the selection names.
			ev.Visits = nil
		}, []string{"SELECTION_CONTRADICTS_VISIT_COUNT", "SELECTION_WITHOUT_ADMITTING_VISIT"}},
		{"a last visit that admitted nothing", func(ev *predictioneval.OrderedRulesEvaluation, _ *predictioneval.OrderedRulesSelection) {
			visits := append([]predictioneval.OrderedRulesCandidateVisit(nil), ev.Visits...)
			visits[len(visits)-1].Verdict = predictioneval.CandidateNoMatch
			ev.Visits = visits
		}, []string{"SELECTION_VISIT_NOT_ADMITTED"}},
		{"no recorded step and a visit that admitted nothing", func(ev *predictioneval.OrderedRulesEvaluation, _ *predictioneval.OrderedRulesSelection) {
			// The two witnesses are independent: a missing trace must not stop
			// the visit from being read, which is what an early return here
			// would do.
			ev.Trace = nil
			visits := append([]predictioneval.OrderedRulesCandidateVisit(nil), ev.Visits...)
			visits[len(visits)-1].Verdict = predictioneval.CandidateNoMatch
			ev.Visits = visits
		}, []string{"SELECTION_WITHOUT_ADMITTING_TRACE", "SELECTION_VISIT_NOT_ADMITTED"}},
		{"more visits recorded than the selection's candidate accounts for", func(ev *predictioneval.OrderedRulesEvaluation, _ *predictioneval.OrderedRulesSelection) {
			// The boundary form: the relation is an EQUALITY, so a history
			// LONGER than the admitting index is as contradicted as a shorter
			// one. The extra visit is a copy of the admitting one, so nothing
			// but the count can notice it.
			visits := append([]predictioneval.OrderedRulesCandidateVisit(nil), ev.Visits...)
			ev.Visits = append(visits, visits[len(visits)-1])
		}, []string{"SELECTION_CONTRADICTS_VISIT_COUNT"}},
		{"a visit naming a candidate the selection does not", func(ev *predictioneval.OrderedRulesEvaluation, _ *predictioneval.OrderedRulesSelection) {
			visits := append([]predictioneval.OrderedRulesCandidateVisit(nil), ev.Visits...)
			visits[len(visits)-1].CandidateIdentity = "visit-forged"
			ev.Visits = visits
		}, []string{"SELECTION_CONTRADICTS_ADMITTING_VISIT"}},
	}

	for _, c := range carriers {
		t.Run(c.name, func(t *testing.T) {
			base := predictioneval.EvaluateOrderedRules(c.stream, cfgWithRule("a", predictioneval.ComparatorGe, 50, 100), trace)
			if base.Status != c.native {
				t.Fatalf("fixture produced %q/%q, want %q", base.Status, base.Reason, c.native)
			}
			if base.Selected == nil {
				t.Fatalf("this status must carry a selection: %+v", base)
			}
			if m := p4offline.MapP3bAction(base); !m.Legal {
				t.Fatalf("the native selection must stay legal: %+v", m)
			}
			for _, tc := range tampers {
				t.Run(tc.name, func(t *testing.T) {
					ev := base
					sel := *base.Selected
					ev.Selected = &sel
					tc.on(&ev, &sel)
					m := p4offline.MapP3bAction(ev)
					if m.Legal || m.Class != p4offline.ActionUnsupportedShape {
						t.Fatalf("a selection the mechanism cannot produce must fail closed: %+v", m)
					}
					if !sameStrings(m.Illegality, tc.want) {
						t.Fatalf("want exactly %v, got %v", tc.want, m.Illegality)
					}
				})
			}
		})
	}

	t.Run("a selection beside an evaluation that never stopped is refused", func(t *testing.T) {
		// The producer records the stop before it mints a selection, so the two
		// cannot disagree. Only the WOULD_ATTEMPT arm required a stop position
		// of its own; a carried selection now requires it on either arm.
		for _, c := range carriers {
			t.Run(c.name, func(t *testing.T) {
				ev := predictioneval.EvaluateOrderedRules(c.stream, cfgWithRule("a", predictioneval.ComparatorGe, 50, 100), trace)
				if ev.Status != c.native || ev.Selected == nil {
					t.Fatalf("fixture: %q/%q %+v", ev.Status, ev.Reason, ev.Selected)
				}
				sel := *ev.Selected
				ev.Selected = &sel
				ev.HasStopPosition = false
				ev.StoppedAtPosition = sel.CandidatePosition
				m := p4offline.MapP3bAction(ev)
				if m.Legal || !sameStrings(m.Illegality, []string{"SELECTION_WITHOUT_STOP_POSITION"}) {
					t.Fatalf("a selection without a stop position must fail closed: %+v", m)
				}
			})
		}
	})
	t.Run("a legal default admission is untouched", func(t *testing.T) {
		ev := predictioneval.EvaluateOrderedRules(proj.Stream, cfgDefaultOnly("a", 0, 100), trace)
		if ev.Status != predictioneval.StatusWouldAttempt || ev.Selected == nil {
			t.Fatalf("fixture must admit by default: %q/%q %+v", ev.Status, ev.Reason, ev.Selected)
		}
		if ev.Selected.Basis != predictioneval.SelectionDefault || ev.Selected.RuleIndex != -1 {
			t.Fatalf("a default admission consumes no rule: %+v", ev.Selected)
		}
		if m := p4offline.MapP3bAction(ev); !m.Legal || m.Class != p4offline.ActionWouldAttempt {
			t.Fatalf("the default half of the mechanism stays legal: %+v", m)
		}
	})
	t.Run("a stop position is still named when no selection is carried", func(t *testing.T) {
		// requireCoherentSelection returns early with no selection to judge, so
		// the arm itself has to keep naming the missing stop position on that
		// shape. Without the guard the contradiction goes unnamed entirely.
		ev := predictioneval.EvaluateOrderedRules(proj.Stream, cfgWithRule("a", predictioneval.ComparatorGe, 50, 100), trace)
		if ev.Status != predictioneval.StatusWouldAttempt {
			t.Fatalf("fixture: %q/%q", ev.Status, ev.Reason)
		}
		ev.Selected = nil
		ev.HasStopPosition = false
		m := p4offline.MapP3bAction(ev)
		if m.Legal || !sameStrings(m.Illegality, []string{"NO_SELECTION", "NO_CANDIDATE_REACHED"}) {
			t.Fatalf("both contradictions must be named: %+v", m)
		}
	})
	t.Run("a legitimate admission survives the wire", func(t *testing.T) {
		// The invariants now relate the selection to fields that TRAVEL - the
		// stop fields, the counters and the admitting trace entry, whose share
		// and position both use the hex-word transport. A native admission that
		// went out as JSON and came back must still be legal, on both carriers
		// and on both halves of the mechanism: an invariant that only holds
		// in-process would refuse honest evidence.
		for _, c := range carriers {
			for _, cfg := range []struct {
				name  string
				rules predictioneval.OrderedRulesConfig
			}{
				{"admitted by a rule", cfgWithRule("a", predictioneval.ComparatorGe, 50, 100)},
				{"admitted by the default", cfgDefaultOnly("a", 0, 100)},
			} {
				t.Run(c.name+"/"+cfg.name, func(t *testing.T) {
					ev := predictioneval.EvaluateOrderedRules(c.stream, cfg.rules, trace)
					if ev.Selected == nil {
						t.Fatalf("fixture must admit: %q/%q", ev.Status, ev.Reason)
					}
					var back predictioneval.OrderedRulesEvaluation
					if err := json.Unmarshal(mustMarshal(t, ev), &back); err != nil {
						t.Fatal(err)
					}
					m := p4offline.MapP3bAction(back)
					if !m.Legal || len(m.Illegality) != 0 {
						t.Fatalf("a round-tripped native admission must stay legal: %+v", m)
					}
				})
			}
		}
	})
}
