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
	"reflect"
	"runtime"
	"sort"
	"strconv"
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
		}, []string{"VISIT_COUNT_CONTRADICTS_CANDIDATES_CONSUMED", "SELECTION_CONTRADICTS_CANDIDATES_CONSUMED"}},
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
		}, []string{"VISIT_COUNT_CONTRADICTS_CANDIDATES_CONSUMED", "SELECTION_CONTRADICTS_VISIT_COUNT", "SELECTION_WITHOUT_ADMITTING_VISIT"}},
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
		}, []string{"VISIT_COUNT_CONTRADICTS_CANDIDATES_CONSUMED", "SELECTION_CONTRADICTS_VISIT_COUNT"}},
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
				// Clearing the stop also contradicts the traversal state the
				// producer wrote beside it, which is now named on every status
				// rather than only where a selection carries it.
				if m.Legal || !sameStrings(m.Illegality, []string{
					"STOP_POSITION_CONTRADICTS_CANDIDATES_CONSUMED",
					"STOP_FIELDS_WITHOUT_STOP_POSITION",
					"SELECTION_WITHOUT_STOP_POSITION"}) {
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
	t.Run("the admitting step must be shaped like the half it claims", func(t *testing.T) {
		// The step is the witness, so its own fields have to be the ones the
		// producer writes for that half. A RULE_DRAW that admits took the draw
		// and won it; a DEFAULT_BOUNDS that admits took no draw at all and
		// consumed no word. Neither pairing is one a forger can leave alone
		// while editing the rest of the entry.
		edit := func(ev *predictioneval.OrderedRulesEvaluation, f func(*predictioneval.OrderedRulesTraceEntry)) {
			trace := append([]predictioneval.OrderedRulesTraceEntry(nil), ev.Trace...)
			f(&trace[len(trace)-1])
			ev.Trace = trace
		}
		t.Run("a rule draw that admitted without matching or drawing", func(t *testing.T) {
			for _, tc := range []struct {
				name string
				on   func(*predictioneval.OrderedRulesTraceEntry)
				want string
			}{
				{"a comparator that did not match", func(e *predictioneval.OrderedRulesTraceEntry) {
					e.ComparatorMatched = false
				}, "ADMITTING_STEP_CONTRADICTS_ITS_KIND"},
				{"a draw that was never evaluated", func(e *predictioneval.OrderedRulesTraceEntry) {
					e.BernoulliEvaluated = false
				}, "ADMITTING_STEP_CONTRADICTS_ITS_KIND"},
				{"a draw that failed", func(e *predictioneval.OrderedRulesTraceEntry) {
					e.BernoulliResult = false
				}, "ADMITTING_STEP_CONTRADICTS_ITS_KIND"},
				{"a word past everything the run consumed", func(e *predictioneval.OrderedRulesTraceEntry) {
					e.RawWordIndex = 1 << 30
				}, "ADMITTING_STEP_RAW_WORD_OVER_CEILING"},
				{"an index below the no-word sentinel", func(e *predictioneval.OrderedRulesTraceEntry) {
					// -1 is the producer's "no word". Nothing below it means
					// anything, and the lower bound is the only guard that
					// says so — the two upper bounds are scoped to a
					// NON-NEGATIVE index and never see this shape.
					e.RawWordIndex = -2
				}, "ADMITTING_STEP_RAW_WORD_BELOW_NO_WORD"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					ev := predictioneval.EvaluateOrderedRules(proj.Stream, cfgWithRule("a", predictioneval.ComparatorGe, 50, 100), trace)
					if ev.Selected == nil || ev.Trace[len(ev.Trace)-1].Step != predictioneval.TraceStepRuleDraw {
						t.Fatalf("fixture must admit on a rule draw: %+v", ev.Trace)
					}
					edit(&ev, tc.on)
					if m := p4offline.MapP3bAction(ev); m.Legal || !containsString(m.Illegality, tc.want) {
						t.Fatalf("want %s, got %+v", tc.want, m)
					}
				})
			}
		})
		t.Run("each bound on the word index is pinned on its own", func(t *testing.T) {
			// The two bounds mask each other on a tamper both catch, so each
			// needs a shape only IT refuses: a counter co-forged to match the
			// index leaves the declared ceiling as the only check, and an index
			// inside the ceiling but past the count leaves the counter as the
			// only one.
			for _, tc := range []struct {
				name            string
				index, consumed int
				want            string
			}{
				{"a counter co-forged to match an index past the ceiling", 1 << 30, 1<<30 + 1, "ADMITTING_STEP_RAW_WORD_OVER_CEILING"},
				{"an index inside the ceiling but past what the run consumed", 5, 1, "ADMITTING_STEP_RAW_WORD_NOT_CONSUMED"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					ev := predictioneval.EvaluateOrderedRules(proj.Stream, cfgWithRule("a", predictioneval.ComparatorGe, 50, 100), trace)
					if ev.Selected == nil {
						t.Fatalf("fixture must admit: %q/%q", ev.Status, ev.Reason)
					}
					entries := append([]predictioneval.OrderedRulesTraceEntry(nil), ev.Trace...)
					entries[len(entries)-1].RawWordIndex = tc.index
					ev.Trace = entries
					ev.RawWordsConsumed = tc.consumed
					// Each bound now reports under its own name, so this also
					// pins that the OTHER bound is not the one answering.
					if m := p4offline.MapP3bAction(ev); m.Legal || !containsString(m.Illegality, tc.want) {
						t.Fatalf("want %s: %+v", tc.want, m)
					}
				})
			}
		})
		t.Run("a default admission that took a draw", func(t *testing.T) {
			for _, tc := range []struct {
				name string
				on   func(*predictioneval.OrderedRulesTraceEntry)
				want string
			}{
				{"a draw the default half cannot take", func(e *predictioneval.OrderedRulesTraceEntry) {
					e.BernoulliEvaluated = true
				}, "ADMITTING_STEP_CONTRADICTS_ITS_KIND"},
				{"a draw the default half cannot win", func(e *predictioneval.OrderedRulesTraceEntry) {
					e.BernoulliResult = true
				}, "ADMITTING_STEP_CONTRADICTS_ITS_KIND"},
				{"a word the default half cannot spend", func(e *predictioneval.OrderedRulesTraceEntry) {
					e.RawWordIndex = 3
				}, "ADMITTING_STEP_DEFAULT_SPENT_A_WORD"},
				{"a raw value the default half cannot carry", func(e *predictioneval.OrderedRulesTraceEntry) {
					e.RawWordValue = 7
				}, "ADMITTING_STEP_RAW_WORD_VALUE_WITHOUT_WORD"},
				{"bounds it admitted outside of", func(e *predictioneval.OrderedRulesTraceEntry) {
					e.ComparatorMatched = false
				}, "ADMITTING_STEP_CONTRADICTS_ITS_KIND"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					ev := predictioneval.EvaluateOrderedRules(proj.Stream, cfgDefaultOnly("a", 0, 100), trace)
					if ev.Selected == nil || ev.Trace[len(ev.Trace)-1].Step != predictioneval.TraceStepDefaultBounds {
						t.Fatalf("fixture must admit by default: %+v", ev.Trace)
					}
					edit(&ev, tc.on)
					if m := p4offline.MapP3bAction(ev); m.Legal || !containsString(m.Illegality, tc.want) {
						t.Fatalf("want %s, got %+v", tc.want, m)
					}
				})
			}
		})
	})
	t.Run("participation and stake presence are held to their vocabularies", func(t *testing.T) {
		// The arms derive booleans by EQUALITY — admitted, stakeKnown — so a
		// value outside the vocabulary silently reads as the negative state and
		// satisfies every `!admitted` / `!stakeKnown` requirement. Bounding the
		// two vocabularies is what makes those requirements mean what they say.
		for _, c := range carriers {
			t.Run(c.name, func(t *testing.T) {
				base := predictioneval.EvaluateOrderedRules(c.stream, cfgWithRule("a", predictioneval.ComparatorGe, 50, 100), trace)
				if base.Status != c.native {
					t.Fatalf("fixture: %q/%q", base.Status, base.Reason)
				}
				t.Run("a participation outside the vocabulary", func(t *testing.T) {
					ev := base
					ev.Participation = "FORGED"
					if m := p4offline.MapP3bAction(ev); m.Legal ||
						!containsString(m.Illegality, "PARTICIPATION_OUTSIDE_VOCABULARY") {
						t.Fatalf("a foreign participation must fail closed: %+v", m)
					}
				})
				t.Run("a stake presence outside the vocabulary", func(t *testing.T) {
					ev := base
					ev.Stake.Presence = "FORGED"
					if m := p4offline.MapP3bAction(ev); m.Legal ||
						!containsString(m.Illegality, "STAKE_PRESENCE_OUTSIDE_VOCABULARY") {
						t.Fatalf("a foreign stake presence must fail closed: %+v", m)
					}
				})
			})
		}
		t.Run("a presence no admission explains", func(t *testing.T) {
			// The producer initialises Stake to {MISSING, NOT_EVALUATED} at both
			// construction sites and overwrites it ONLY when it admits, so every
			// status but the two admitting ones carries MISSING.
			ev := predictioneval.EvaluateOrderedRules(proj.Stream, cfgDefaultOnly("a", 90, 100), trace)
			if ev.Status == predictioneval.StatusWouldAttempt ||
				ev.Status == predictioneval.StatusParticipationAdmittedStakeUnknown {
				t.Fatalf("fixture must not admit: %q", ev.Status)
			}
			if ev.Stake.Presence != predictioneval.SuppliedMissing {
				t.Fatalf("a non-admitting result carries MISSING: %+v", ev.Stake)
			}
			ev.Stake.Presence = predictioneval.SuppliedInvalid
			if m := p4offline.MapP3bAction(ev); m.Legal ||
				!containsString(m.Illegality, "STAKE_PRESENCE_CONTRADICTS_STATUS") {
				t.Fatalf("only an admission explains a non-MISSING presence: %+v", m)
			}
		})
		t.Run("a rate of exactly one still admits without spending a word", func(t *testing.T) {
			// Names the false-refusal risk the admitting-step requirements
			// introduce. A rate of exactly one succeeds WITHOUT drawing, so the
			// step carries BernoulliEvaluated true and RawWordIndex -1 together
			// — a pairing a naive "evaluated implies a word was spent" reading
			// would refuse.
			ev := predictioneval.EvaluateOrderedRules(proj.Stream, cfgWithRule("a", predictioneval.ComparatorGe, 50, 100), trace)
			last := ev.Trace[len(ev.Trace)-1]
			if last.Step != predictioneval.TraceStepRuleDraw || !last.BernoulliEvaluated || last.RawWordIndex != -1 {
				t.Fatalf("fixture must admit on a draw that spent no word: %+v", last)
			}
			if m := p4offline.MapP3bAction(ev); !m.Legal || len(m.Illegality) != 0 {
				t.Fatalf("an honest no-word admission must stay legal: %+v", m)
			}
		})
		t.Run("a presence the reason contradicts", func(t *testing.T) {
			// The producer writes the two together: a balance that was not
			// supplied is MISSING, one invalid or out of the u32 domain is
			// INVALID. It never writes NOT_SUPPLIED beside INVALID.
			ev := predictioneval.EvaluateOrderedRules(carriers[1].stream, cfgWithRule("a", predictioneval.ComparatorGe, 50, 100), trace)
			if ev.Status != predictioneval.StatusParticipationAdmittedStakeUnknown ||
				ev.Reason != predictioneval.ReasonBalanceNotSupplied {
				t.Fatalf("fixture must be the not-supplied arm: %q/%q", ev.Status, ev.Reason)
			}
			if ev.Stake.Presence != predictioneval.SuppliedMissing {
				t.Fatalf("the producer pairs NOT_SUPPLIED with MISSING: %+v", ev.Stake)
			}
			ev.Stake.Presence = predictioneval.SuppliedInvalid
			if m := p4offline.MapP3bAction(ev); m.Legal ||
				!containsString(m.Illegality, "STAKE_PRESENCE_CONTRADICTS_REASON") {
				t.Fatalf("a presence its reason contradicts must fail closed: %+v", m)
			}
		})
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
		if m.Legal || !sameStrings(m.Illegality, []string{
			"STOP_POSITION_CONTRADICTS_CANDIDATES_CONSUMED",
			"STOP_FIELDS_WITHOUT_STOP_POSITION",
			"NO_SELECTION", "NO_CANDIDATE_REACHED"}) {
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

// TestP3bEvaluationMustNotContradictItsOwnBookkeeping covers four shapes an
// external review reproduced against the previous head. Each is a value the
// producer fixes on a path this map already reads, left unread so that an
// evaluation could contradict itself and still map legal. None of them asks the
// map to re-run the evaluator: every relation below compares the evaluation
// against another field of the same evaluation.
func TestP3bEvaluationMustNotContradictItsOwnBookkeeping(t *testing.T) {
	_, fs := selectedFactset(t, nil, nil)
	proj, err := p4offline.ProjectP3bSingleCandidate(fs)
	if err != nil {
		t.Fatal(err)
	}
	coords := synthCoords(fs, 0)
	trace := mustDrawTrace(t, coords, 4)
	ruleCfg := cfgWithRule("a", predictioneval.ComparatorGe, 50, 100)

	// A rate of exactly one is the producer's "always true" threshold: it
	// answers WITHOUT drawing, so the admitting entry carries the no-word
	// sentinel and a zero raw value, and BernoulliEvaluations still advances
	// because the counter sits below the branch rather than inside it. Both
	// facts are pinned here because the cases below rest on them.
	admitting := func(t *testing.T) predictioneval.OrderedRulesEvaluation {
		t.Helper()
		ev := predictioneval.EvaluateOrderedRules(proj.Stream, ruleCfg, trace)
		if ev.Status != predictioneval.StatusWouldAttempt || ev.Selected == nil {
			t.Fatalf("fixture must admit: %q/%q", ev.Status, ev.Reason)
		}
		if ev.Selected.Basis != predictioneval.SelectionDetailedRule {
			t.Fatalf("fixture must admit on the detailed-rule half: %+v", ev.Selected)
		}
		last := ev.Trace[len(ev.Trace)-1]
		if last.Step != predictioneval.TraceStepRuleDraw || last.RawWordIndex != -1 || last.RawWordValue != 0 {
			t.Fatalf("fixture must admit on an UNDRAWN rule draw: %+v", last)
		}
		if ev.BernoulliEvaluations < 1 {
			t.Fatalf("a detailed-rule admission always evaluated a draw: %+v", ev)
		}
		if m := p4offline.MapP3bAction(ev); !m.Legal {
			t.Fatalf("the untouched fixture must stay legal: %+v", m)
		}
		return ev
	}
	editLast := func(ev *predictioneval.OrderedRulesEvaluation, f func(*predictioneval.OrderedRulesTraceEntry)) {
		entries := append([]predictioneval.OrderedRulesTraceEntry(nil), ev.Trace...)
		f(&entries[len(entries)-1])
		ev.Trace = entries
	}
	editVisit := func(ev *predictioneval.OrderedRulesEvaluation, f func(*predictioneval.OrderedRulesCandidateVisit)) {
		visits := append([]predictioneval.OrderedRulesCandidateVisit(nil), ev.Visits...)
		f(&visits[len(visits)-1])
		ev.Visits = visits
	}

	t.Run("a raw value beside the no-word sentinel", func(t *testing.T) {
		// The rule-draw half leaves RawWordValue unread BECAUSE a drawn word's
		// value is not something the evaluation attests to. That reasoning does
		// not reach the case where the entry itself says no word was drawn:
		// there the producer fixes the value at zero, exactly as on the default
		// half, so a nonzero value is a self-contradicting witness.
		ev := admitting(t)
		editLast(&ev, func(e *predictioneval.OrderedRulesTraceEntry) { e.RawWordValue = 1 })
		// Its own identifier, not the kind check's: the two can fail on one
		// entry, and a shared name would report one shape twice and say which
		// guard answered neither time — the masking defect this diff repairs
		// for the raw-word bounds a few lines below.
		if m := p4offline.MapP3bAction(ev); m.Legal ||
			!containsString(m.Illegality, "ADMITTING_STEP_RAW_WORD_VALUE_WITHOUT_WORD") {
			t.Fatalf("a raw value with no word drawn must fail closed: %+v", m)
		}
	})
	t.Run("a drawn word's value stays unread", func(t *testing.T) {
		// The guard above must NOT become "RawWordValue is always zero": on a
		// rule that really drew, the value is whatever the trace supplied and
		// the evaluation never attests to it.
		ev := predictioneval.EvaluateOrderedRules(proj.Stream, cfgWithRule("a", predictioneval.ComparatorGe, 50, 50), trace)
		if ev.Status != predictioneval.StatusWouldAttempt || ev.Selected == nil {
			// FATAL, not SKIP. This subtest is the control that stops the guard
			// above being widened into "RawWordValue is always zero". Its whole
			// body is conditional on the fixture admitting, so a skip here
			// would delete the control and report a pass -- exactly hiding the
			// regression it was written for. Everywhere else in this package a
			// fixture precondition is fatal; this was the one exception.
			t.Fatalf("fixture must admit at 50%%: %q/%q", ev.Status, ev.Reason)
		}
		last := ev.Trace[len(ev.Trace)-1]
		if last.RawWordIndex < 0 {
			t.Fatalf("fixture must have SPENT a word: %+v", last)
		}
		if m := p4offline.MapP3bAction(ev); !m.Legal {
			t.Fatalf("a genuine drawn word must stay legal: %+v", m)
		}
	})
	t.Run("a detailed-rule admission that evaluated no draw", func(t *testing.T) {
		// BernoulliEvaluations is incremented on the common path below the
		// rate-one branch, so EVERY detailed-rule admission has passed it at
		// least once, including one that drew no word.
		ev := admitting(t)
		ev.BernoulliEvaluations = 0
		if m := p4offline.MapP3bAction(ev); m.Legal ||
			!containsString(m.Illegality, "SELECTION_BASIS_CONTRADICTS_BERNOULLI_COUNT") {
			t.Fatalf("a detailed-rule admission with no evaluated draw must fail closed: %+v", m)
		}
	})
	t.Run("a default admission is held to no such count", func(t *testing.T) {
		// The converse does NOT hold and must not be asserted: detailed rules
		// that failed on an earlier outcome advance the counter and the default
		// still admits, so a default admission may carry any count inside the
		// range the header bounds every status to.
		ev := predictioneval.EvaluateOrderedRules(proj.Stream, cfgDefaultOnly("a", 0, 100), trace)
		if ev.Selected == nil || ev.Selected.Basis != predictioneval.SelectionDefault {
			t.Fatalf("fixture must admit by default: %+v", ev.Selected)
		}
		if ev.BernoulliEvaluations != 0 {
			t.Fatalf("this fixture reaches no rule: %+v", ev)
		}
		if m := p4offline.MapP3bAction(ev); !m.Legal {
			t.Fatalf("a default admission with a zero count is legal: %+v", m)
		}
	})
	t.Run("an admitting visit whose balance use the status contradicts", func(t *testing.T) {
		// admitOrderedRules writes the visit's BalanceUse and the status in ONE
		// switch, so on an admitting visit it is an exact function of the
		// status and reason. NOT_EVALUATED is what a visit carries before that
		// switch runs, and the admitting visit is appended after it.
		for _, tc := range []struct {
			name string
			use  predictioneval.OrderedRulesBalanceUse
		}{
			{"a balance the admission never evaluated", predictioneval.BalanceNotEvaluated},
			{"a balance use outside the vocabulary", "FORGED"},
			{"a balance the admission could not size with", predictioneval.BalanceRequiredButMissing},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ev := admitting(t)
				editVisit(&ev, func(v *predictioneval.OrderedRulesCandidateVisit) { v.BalanceUse = tc.use })
				if m := p4offline.MapP3bAction(ev); m.Legal ||
					!containsString(m.Illegality, "SELECTION_VISIT_CONTRADICTS_BALANCE_USE") {
					t.Fatalf("want the visit refused: %+v", m)
				}
			})
		}
	})
	t.Run("each balance-use arm is pinned on its own", func(t *testing.T) {
		// One arm per admitting outcome. WOULD_ATTEMPT is covered above; this
		// covers the stake-unknown one, so that swapping either arm's answer
		// refuses genuine producer output at a case named for it rather than
		// being caught only by some other test's fixture check.
		cands := append([]predictioneval.OrderedRulesCandidate(nil), proj.Stream.Candidates...)
		cands[0].Balance = predictioneval.SuppliedInt64{Presence: predictioneval.SuppliedMissing, Reason: "test"}
		missing, err := predictioneval.ProjectOrderedRulesStream(
			predictioneval.OrderedRulesSource{Scope: proj.Stream.Scope, Candidates: cands}, proj.Stream.Admission)
		if err != nil {
			t.Fatal(err)
		}
		ev := predictioneval.EvaluateOrderedRules(missing, ruleCfg, trace)
		if ev.Status != predictioneval.StatusParticipationAdmittedStakeUnknown ||
			ev.Reason != predictioneval.ReasonBalanceNotSupplied || ev.Selected == nil {
			t.Fatalf("fixture must admit with an unsupplied balance: %q/%q", ev.Status, ev.Reason)
		}
		last := ev.Visits[len(ev.Visits)-1]
		if last.BalanceUse != predictioneval.BalanceRequiredButMissing {
			t.Fatalf("the producer writes REQUIRED_BUT_MISSING on this arm: %+v", last)
		}
		if m := p4offline.MapP3bAction(ev); !m.Legal {
			t.Fatalf("genuine output on this arm must stay legal: %+v", m)
		}
		// And the arm is not interchangeable with the other one.
		editVisit(&ev, func(v *predictioneval.OrderedRulesCandidateVisit) { v.BalanceUse = predictioneval.BalanceUsed })
		if m := p4offline.MapP3bAction(ev); m.Legal ||
			!containsString(m.Illegality, "SELECTION_VISIT_CONTRADICTS_BALANCE_USE") {
			t.Fatalf("a used balance cannot witness an unsupplied one: %+v", m)
		}
	})
	t.Run("a status naming no balance use is not held to one", func(t *testing.T) {
		// The pairing is checked only where the status names one. A reason the
		// arm above has already refused names none, and reporting the visit as
		// contradicting an empty expectation would describe ONE contradiction
		// under two names. The exact set is what pins that.
		cands := append([]predictioneval.OrderedRulesCandidate(nil), proj.Stream.Candidates...)
		cands[0].Balance = predictioneval.SuppliedInt64{Presence: predictioneval.SuppliedMissing, Reason: "test"}
		reproj, err := predictioneval.ProjectOrderedRulesStream(
			predictioneval.OrderedRulesSource{Scope: proj.Stream.Scope, Candidates: cands}, proj.Stream.Admission)
		if err != nil {
			t.Fatal(err)
		}
		ev := predictioneval.EvaluateOrderedRules(reproj, ruleCfg, trace)
		if ev.Status != predictioneval.StatusParticipationAdmittedStakeUnknown || ev.Selected == nil {
			t.Fatalf("fixture: %q/%q", ev.Status, ev.Reason)
		}
		ev.Reason = "FORGED"
		m := p4offline.MapP3bAction(ev)
		if m.Legal || !sameStrings(m.Illegality, []string{"STAKE_UNKNOWN_REASON_FOREIGN"}) {
			t.Fatalf("want exactly the foreign reason named: %+v", m)
		}
	})
	t.Run("terminal reasons are held to their status vocabulary", func(t *testing.T) {
		// Both arms tested only for a NONEMPTY reason, so any string passed.
		// The producer emits a closed, status-specific set at every site.
		for _, tc := range []struct {
			name   string
			cfg    predictioneval.OrderedRulesConfig
			trace  predictioneval.SuppliedDrawTrace
			native predictioneval.OrderedRulesStatus
			want   string
		}{
			{"unknown input", cfgWithRule("a", predictioneval.ComparatorGe, 50, 50),
				predictioneval.SuppliedDrawTrace{RunID: "r", EntropySemanticsVersion: predictioneval.OrderedRulesEntropySemanticsVersion},
				predictioneval.StatusUnknownInput, "UNKNOWN_INPUT_REASON_FOREIGN"},
			{"refused", predictioneval.OrderedRulesConfig{ConfigID: "nodefault"}, trace,
				predictioneval.StatusRefused, "REFUSAL_REASON_FOREIGN"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ev := predictioneval.EvaluateOrderedRules(proj.Stream, tc.cfg, tc.trace)
				if ev.Status != tc.native {
					t.Fatalf("fixture produced %q/%q, want %q", ev.Status, ev.Reason, tc.native)
				}
				if m := p4offline.MapP3bAction(ev); !m.Legal {
					t.Fatalf("the producer's own reason %q must stay legal: %+v", ev.Reason, m)
				}
				ev.Reason = "FORGED"
				if m := p4offline.MapP3bAction(ev); m.Legal || !containsString(m.Illegality, tc.want) {
					t.Fatalf("a foreign %q reason must fail closed: %+v", tc.native, m)
				}
			})
		}
	})
	t.Run("every cumulative counter is bounded on its own range", func(t *testing.T) {
		// The same defect as the consumed-word counter below, in its three
		// siblings, found only because that repair prompted the question
		// "which OTHER class-A field is read on some paths and not others?".
		// All four are read exclusively inside requireCoherentSelection, so on
		// every terminal status -- which carries no selection -- and on the
		// DEFAULT half -- where the detailed-rule arm never runs -- they went
		// entirely unread while the matrix classified them load-bearing.
		//
		// Every ceiling is the producer's own, read off the writer:
		//   CandidatesConsumed  = ci+1 over a candidate list the input gate
		//                         refuses past MaxOrderedRulesCandidates.
		//   OutcomesConsidered  one per outcome of one candidate, and the same
		//                         gate refuses any candidate past
		//                         MaxOrderedRulesOutcomes, so the product bounds it.
		//   RulesConsidered     incremented immediately AFTER the work++ that
		//                         refuses past MaxOrderedRulesWork.
		//   BernoulliEvaluations in that same work-guarded rule loop.
		terminal := func(t *testing.T) predictioneval.OrderedRulesEvaluation {
			t.Helper()
			ev := predictioneval.EvaluateOrderedRules(proj.Stream,
				predictioneval.OrderedRulesConfig{ConfigID: "nodefault"}, trace)
			if ev.Status != predictioneval.StatusRefused || ev.Selected != nil {
				t.Fatalf("fixture must refuse without a selection: %q/%q", ev.Status, ev.Reason)
			}
			if m := p4offline.MapP3bAction(ev); !m.Legal {
				t.Fatalf("the untouched refusal must stay legal: %+v", m)
			}
			return ev
		}
		midTraversal := func(t *testing.T) predictioneval.OrderedRulesEvaluation {
			// A mid-traversal refusal: it carries a stop and one visit per
			// candidate consumed, which is the shape the producer writes when
			// the work budget stops it. This package's single-candidate
			// projection cannot reach that budget, so the status/reason pair is
			// set on traversal state the producer wrote.
			t.Helper()
			ev := predictioneval.EvaluateOrderedRules(proj.Stream, cfgDefaultOnly("a", 95, 100), trace)
			if ev.Status != predictioneval.StatusNoAttemptInSuppliedPrefix || ev.CandidatesConsumed == 0 {
				t.Fatalf("fixture must have walked candidates: %q/%q %+v", ev.Status, ev.Reason, ev)
			}
			ev.Status = predictioneval.StatusRefused
			ev.Reason = predictioneval.ReasonWorkBudgetExceeded
			if m := p4offline.MapP3bAction(ev); !m.Legal {
				t.Fatalf("the untouched mid-traversal shape must stay legal: %+v", m)
			}
			return ev
		}
		defaultAdmission := func(t *testing.T) predictioneval.OrderedRulesEvaluation {
			t.Helper()
			ev := predictioneval.EvaluateOrderedRules(proj.Stream, cfgDefaultOnly("a", 0, 100), trace)
			if ev.Status != predictioneval.StatusWouldAttempt || ev.Selected == nil ||
				ev.Selected.Basis != predictioneval.SelectionDefault {
				t.Fatalf("fixture must admit by default: %q/%q", ev.Status, ev.Reason)
			}
			return ev
		}
		// The literals above are the contract's numbers. If the contract ever
		// moves one, this fails here rather than silently re-pinning the guard
		// to whatever the code now says.
		if predictioneval.MaxOrderedRulesCandidates != 128 || predictioneval.MaxOrderedRulesOutcomes != 64 ||
			predictioneval.MaxOrderedRulesWork != 1<<18 {
			t.Fatalf("the declared ceilings moved: candidates %d, outcomes %d, work %d",
				predictioneval.MaxOrderedRulesCandidates, predictioneval.MaxOrderedRulesOutcomes,
				predictioneval.MaxOrderedRulesWork)
		}
		for _, c := range []struct {
			name string
			set  func(*predictioneval.OrderedRulesEvaluation, int)
			max  int
			want string
		}{
			// The ceilings are LITERALS, not the constant expressions the code
			// uses. Written as predictioneval.MaxOrderedRulesCandidates the
			// expectation would agree with the code by construction, and a
			// substitution between two constants that happen to share a value
			// -- Candidates and Rules are both 128 -- would be invisible.
			{"candidates consumed", func(e *predictioneval.OrderedRulesEvaluation, v int) { e.CandidatesConsumed = v },
				128, "EVALUATION_CANDIDATES_CONSUMED_OUT_OF_RANGE"},
			{"outcomes considered", func(e *predictioneval.OrderedRulesEvaluation, v int) { e.OutcomesConsidered = v },
				128 * 64, "EVALUATION_OUTCOMES_CONSIDERED_OUT_OF_RANGE"},
			{"rules considered", func(e *predictioneval.OrderedRulesEvaluation, v int) { e.RulesConsidered = v },
				1 << 18, "EVALUATION_RULES_CONSIDERED_OUT_OF_RANGE"},
			{"bernoulli evaluations", func(e *predictioneval.OrderedRulesEvaluation, v int) { e.BernoulliEvaluations = v },
				1 << 18, "EVALUATION_BERNOULLI_EVALUATIONS_OUT_OF_RANGE"},
		} {
			t.Run(c.name, func(t *testing.T) {
				for _, base := range []struct {
					name string
					make func(*testing.T) predictioneval.OrderedRulesEvaluation
				}{
					{"on a terminal status", terminal},
					{"on a default admission", defaultAdmission},
				} {
					t.Run(base.name, func(t *testing.T) {
						for _, bad := range []struct {
							name string
							v    int
						}{
							{"negative", -1},
							{"past its ceiling", c.max + 1},
						} {
							t.Run(bad.name, func(t *testing.T) {
								ev := base.make(t)
								c.set(&ev, bad.v)
								if m := p4offline.MapP3bAction(ev); m.Legal || !containsString(m.Illegality, c.want) {
									t.Fatalf("want %s: %+v", c.want, m)
								}
							})
						}
					})
				}
				t.Run("both ends of the declared range are admitted", func(t *testing.T) {
					// The control that stops a bound being narrowed, and it is
					// named for what it does rather than for what an earlier
					// version claimed. That version asserted "the extremes the
					// producer can write stay legal" on a PRE-TRAVERSAL refusal,
					// where the producer writes zero and nothing else -- so its
					// ceiling half asserted that a shape the producer cannot
					// write stays legal, which is a gap, not a control.
					//
					// The base here is a shape the producer really emits: a
					// mid-traversal refusal carries a stop and one visit per
					// candidate consumed. The ceiling case keeps that accounting
					// true by padding the visits with it, so the only predicate
					// left to answer is the range itself.
					zero := terminal(t)
					c.set(&zero, 0)
					if m := p4offline.MapP3bAction(zero); !m.Legal {
						t.Fatalf("zero is inside the producer's range: %+v", m)
					}
					atCeiling := midTraversal(t)
					c.set(&atCeiling, c.max)
					if c.want == "EVALUATION_CANDIDATES_CONSUMED_OUT_OF_RANGE" {
						visits := make([]predictioneval.OrderedRulesCandidateVisit, c.max)
						copy(visits, atCeiling.Visits)
						atCeiling.Visits = visits
					}
					if m := p4offline.MapP3bAction(atCeiling); !m.Legal {
						t.Fatalf("the declared ceiling %d must be admitted: %+v", c.max, m)
					}
				})
			})
		}
	})
	t.Run("the consumed-word counter is bounded on its own", func(t *testing.T) {
		// The counter was read ONLY as the second bound on the admitting word
		// index, inside `if last.RawWordIndex >= 0`. Every admission whose final
		// step consumed no word never reached it: the whole default half, and a
		// detailed rule at a rate of exactly one, which is this fixture. The
		// matrix classified the field A while a whole reachable path left it
		// unread, which is the defect the matrix exists to make impossible.
		//
		// The range is the producer's own: `cursor` starts at zero, is only ever
		// incremented, never passes len(words), and a trace longer than
		// MaxOrderedRulesDrawWords is refused before the traversal. So both
		// shapes below are producer-impossible, and the bound holds on EVERY
		// status, not only on an admission. The terminal case below refuses
		// PRE-traversal, where the counter is never assigned at all and carries
		// Go's zero; only the mid-traversal refusals write it from the cursor.
		for _, tc := range []struct {
			name  string
			count int
		}{
			{"a negative count", -1},
			{"a count past the declared ceiling", predictioneval.MaxOrderedRulesDrawWords + 1},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ev := admitting(t)
				ev.RawWordsConsumed = tc.count
				if m := p4offline.MapP3bAction(ev); m.Legal ||
					!containsString(m.Illegality, "EVALUATION_RAW_WORDS_CONSUMED_OUT_OF_RANGE") {
					t.Fatalf("want EVALUATION_RAW_WORDS_CONSUMED_OUT_OF_RANGE: %+v", m)
				}
			})
		}
		t.Run("the default half is held to it too", func(t *testing.T) {
			// The other half of the same gap, and the one with no draw at all.
			ev := predictioneval.EvaluateOrderedRules(proj.Stream, cfgDefaultOnly("a", 0, 100), trace)
			if ev.Status != predictioneval.StatusWouldAttempt || ev.Selected == nil ||
				ev.Selected.Basis != predictioneval.SelectionDefault {
				t.Fatalf("fixture must admit by default: %q/%q %+v", ev.Status, ev.Reason, ev.Selected)
			}
			if m := p4offline.MapP3bAction(ev); !m.Legal {
				t.Fatalf("the untouched default admission must stay legal: %+v", m)
			}
			ev.RawWordsConsumed = -1
			if m := p4offline.MapP3bAction(ev); m.Legal ||
				!containsString(m.Illegality, "EVALUATION_RAW_WORDS_CONSUMED_OUT_OF_RANGE") {
				t.Fatalf("want EVALUATION_RAW_WORDS_CONSUMED_OUT_OF_RANGE: %+v", m)
			}
		})
		t.Run("a terminal status carries the counter too", func(t *testing.T) {
			// The bound is not scoped to admissions, so a refusal is held to it.
			ev := predictioneval.EvaluateOrderedRules(proj.Stream,
				predictioneval.OrderedRulesConfig{ConfigID: "nodefault"}, trace)
			if ev.Status != predictioneval.StatusRefused {
				t.Fatalf("fixture must refuse: %q/%q", ev.Status, ev.Reason)
			}
			if m := p4offline.MapP3bAction(ev); !m.Legal {
				t.Fatalf("the producer's own refusal must stay legal: %+v", m)
			}
			ev.RawWordsConsumed = predictioneval.MaxOrderedRulesDrawWords + 1
			if m := p4offline.MapP3bAction(ev); m.Legal ||
				!containsString(m.Illegality, "EVALUATION_RAW_WORDS_CONSUMED_OUT_OF_RANGE") {
				t.Fatalf("want EVALUATION_RAW_WORDS_CONSUMED_OUT_OF_RANGE: %+v", m)
			}
		})
		t.Run("every count the producer can write stays legal", func(t *testing.T) {
			// The false-refusal control, at both ends of the admitted range.
			for _, count := range []int{0, 1, predictioneval.MaxOrderedRulesDrawWords} {
				ev := admitting(t)
				ev.RawWordsConsumed = count
				if m := p4offline.MapP3bAction(ev); !m.Legal {
					t.Fatalf("a count of %d is inside the producer's range: %+v", count, m)
				}
			}
		})
	})
}

// TestP3bMapReadsEveryLoadBearingHeaderAndStakeField closes the two fields the
// field/status matrix classified as load-bearing but accidentally unread.
func TestP3bMapReadsEveryLoadBearingHeaderAndStakeField(t *testing.T) {
	_, fs := selectedFactset(t, nil, nil)
	proj, err := p4offline.ProjectP3bSingleCandidate(fs)
	if err != nil {
		t.Fatal(err)
	}
	coords := synthCoords(fs, 0)
	trace := mustDrawTrace(t, coords, 4)
	ruleCfg := cfgWithRule("a", predictioneval.ComparatorGe, 50, 100)

	t.Run("a foreign donor revision", func(t *testing.T) {
		// The fourth pinned header string. EvidenceLabel, ModelVersion and
		// EntropySemanticsVersion are all held to their constants; this one was
		// unchecked beside them, which was an omission rather than a decision.
		ev := predictioneval.EvaluateOrderedRules(proj.Stream, ruleCfg, trace)
		if m := p4offline.MapP3bAction(ev); !m.Legal {
			t.Fatalf("fixture must be legal: %+v", m)
		}
		ev.DonorRevision = "someone-elses/fork@0000000"
		if m := p4offline.MapP3bAction(ev); m.Legal ||
			!containsString(m.Illegality, "DONOR_REVISION_FOREIGN") {
			t.Fatalf("a foreign donor revision must fail closed: %+v", m)
		}
	})
	t.Run("a stake reason no non-admitting status can carry", func(t *testing.T) {
		// The producer initialises Stake to {MISSING, NOT_EVALUATED} at both
		// construction sites and overwrites it ONLY where it admits, so on
		// every non-admitting status the REASON is fixed exactly as the
		// presence is. The presence was held to that and the reason was not.
		for _, tc := range []struct {
			name   string
			cfg    predictioneval.OrderedRulesConfig
			trace  predictioneval.SuppliedDrawTrace
			native predictioneval.OrderedRulesStatus
		}{
			{"no attempt in prefix", cfgDefaultOnly("a", 95, 100), trace, predictioneval.StatusNoAttemptInSuppliedPrefix},
			{"unknown input", cfgWithRule("a", predictioneval.ComparatorGe, 50, 50),
				predictioneval.SuppliedDrawTrace{RunID: "r", EntropySemanticsVersion: predictioneval.OrderedRulesEntropySemanticsVersion},
				predictioneval.StatusUnknownInput},
			{"refused", predictioneval.OrderedRulesConfig{ConfigID: "nodefault"}, trace, predictioneval.StatusRefused},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ev := predictioneval.EvaluateOrderedRules(proj.Stream, tc.cfg, tc.trace)
				if ev.Status != tc.native {
					t.Fatalf("fixture produced %q/%q", ev.Status, ev.Reason)
				}
				if ev.Stake.Reason != predictioneval.ReasonBalanceNotEvaluated {
					t.Fatalf("the producer fixes this arm's stake reason: %+v", ev.Stake)
				}
				if m := p4offline.MapP3bAction(ev); !m.Legal {
					t.Fatalf("genuine output must stay legal: %+v", m)
				}
				ev.Stake.Reason = "BALANCE_INVALID"
				if m := p4offline.MapP3bAction(ev); m.Legal ||
					!containsString(m.Illegality, "STAKE_REASON_CONTRADICTS_STATUS") {
					t.Fatalf("a stake reason no non-admitting status writes must fail closed: %+v", m)
				}
			})
		}
	})
	t.Run("an admitting arm's stake reason stays unread", func(t *testing.T) {
		// It must NOT become "the reason is always NOT_EVALUATED": where the
		// model admits, balanceReason carries the CALLER's own balance reason
		// string, which is arbitrary text and not a vocabulary this package
		// can bound.
		cands := append([]predictioneval.OrderedRulesCandidate(nil), proj.Stream.Candidates...)
		cands[0].Balance = predictioneval.SuppliedInt64{Presence: predictioneval.SuppliedMissing, Reason: "anything the caller wrote"}
		reproj, err := predictioneval.ProjectOrderedRulesStream(
			predictioneval.OrderedRulesSource{Scope: proj.Stream.Scope, Candidates: cands}, proj.Stream.Admission)
		if err != nil {
			t.Fatal(err)
		}
		ev := predictioneval.EvaluateOrderedRules(reproj, ruleCfg, trace)
		if ev.Status != predictioneval.StatusParticipationAdmittedStakeUnknown {
			t.Fatalf("fixture: %q/%q", ev.Status, ev.Reason)
		}
		if ev.Stake.Reason != "anything the caller wrote" {
			t.Fatalf("this arm carries the caller's own text: %+v", ev.Stake)
		}
		if m := p4offline.MapP3bAction(ev); !m.Legal {
			t.Fatalf("caller text on an admitting arm is not a contradiction: %+v", m)
		}
	})
}

// TestP3bFieldStatusMatrixIsComplete pins the census the seam-C field/status
// matrix in actionmap.go is written against.
//
// The matrix classifies every field of every native structure MapP3bAction
// reads as VALIDATED_LOAD_BEARING or INTENTIONALLY_NON_AUTHORITATIVE. That
// classification is only trustworthy while the census it was taken over is
// still the whole census: if the native core gains a field, the matrix would
// silently stop covering it and the new field would be accidentally unread —
// exactly the condition this class of repair exists to remove.
//
// So this test does not check behaviour. It checks that the set of fields has
// not changed, and fails with the new name when it has, which forces the field
// to be classified in the matrix before the suite can go green again.
func TestP3bFieldStatusMatrixIsComplete(t *testing.T) {
	census := map[string][]string{
		"OrderedRulesEvaluation": {
			"EvidenceLabel", "ModelVersion", "DonorRevision", "EntropySemanticsVersion",
			"StreamDigest", "ConfigDigest", "EntropyDigest", "ConsumedInputDigest",
			"Status", "Reason", "Participation", "Stake", "Selected",
			"HasStopPosition", "StoppedAtCandidate", "StoppedAtPosition",
			"CandidatesConsumed", "OutcomesConsidered", "RulesConsidered",
			"BernoulliEvaluations", "RawWordsConsumed",
			"Cutoff", "Qualifications", "Trace", "Visits",
		},
		"OrderedRulesSelection": {
			"CandidateIdentity", "CandidatePosition", "CandidateIndex",
			"OutcomeIndex", "OutcomeIdentity", "Basis", "RuleIndex", "ShareBits",
		},
		"OrderedRulesTraceEntry": {
			"Step", "CandidateIndex", "CandidatePosition", "OutcomeIndex", "ShareBits",
			"RuleIndex", "ComparatorMatched", "BernoulliEvaluated", "BernoulliResult",
			"RawWordIndex", "RawWordValue", "Admitted",
		},
		"OrderedRulesCandidateVisit": {
			"CandidateIndex", "CandidateIdentity", "CandidatePosition", "Verdict",
			"BalanceUse", "PoolTotalKnown", "PoolTotal", "RawWordsConsumedHere",
		},
		"SuppliedUint32": {"Presence", "Value", "Reason"},
	}
	samples := map[string]any{
		"OrderedRulesEvaluation":     predictioneval.OrderedRulesEvaluation{},
		"OrderedRulesSelection":      predictioneval.OrderedRulesSelection{},
		"OrderedRulesTraceEntry":     predictioneval.OrderedRulesTraceEntry{},
		"OrderedRulesCandidateVisit": predictioneval.OrderedRulesCandidateVisit{},
		"SuppliedUint32":             predictioneval.SuppliedUint32{},
	}
	total := 0
	for name, want := range census {
		ty := reflect.TypeOf(samples[name])
		got := map[string]bool{}
		for i := 0; i < ty.NumField(); i++ {
			if f := ty.Field(i); f.IsExported() {
				got[f.Name] = true
			}
		}
		total += len(want)
		classified := map[string]bool{}
		for _, f := range want {
			classified[f] = true
			if !got[f] {
				t.Errorf("%s: the matrix classifies %q, which the native structure no longer has", name, f)
			}
		}
		for f := range got {
			if !classified[f] {
				t.Errorf("%s: field %q is NOT classified in the seam-C field/status matrix. "+
					"Classify it as VALIDATED_LOAD_BEARING or INTENTIONALLY_NON_AUTHORITATIVE "+
					"in actionmap.go before relying on the map's closure.", name, f)
			}
		}
		if len(got) != len(want) {
			t.Errorf("%s: native structure has %d exported fields, matrix classifies %d", name, len(got), len(want))
		}
	}
	if total != 56 {
		t.Errorf("the matrix documents 56 classified fields, this census counts %d", total)
	}
}

// TestP3bAdmittingArmsBindTheirOwnProducerFixedFields closes two gaps an
// independent review lane found in the field/status matrix: a justification
// that was true of some producer arms and applied to all of them, and a
// presence flag classified with the value it accompanies.
func TestP3bAdmittingArmsBindTheirOwnProducerFixedFields(t *testing.T) {
	_, fs := selectedFactset(t, nil, nil)
	proj, err := p4offline.ProjectP3bSingleCandidate(fs)
	if err != nil {
		t.Fatal(err)
	}
	coords := synthCoords(fs, 0)
	trace := mustDrawTrace(t, coords, 4)
	ruleCfg := cfgWithRule("a", predictioneval.ComparatorGe, 50, 100)
	streamWith := func(t *testing.T, bal predictioneval.SuppliedInt64) predictioneval.OrderedRulesStream {
		t.Helper()
		cands := append([]predictioneval.OrderedRulesCandidate(nil), proj.Stream.Candidates...)
		cands[0].Balance = bal
		re, err := predictioneval.ProjectOrderedRulesStream(
			predictioneval.OrderedRulesSource{Scope: proj.Stream.Scope, Candidates: cands}, proj.Stream.Admission)
		if err != nil {
			t.Fatal(err)
		}
		return re
	}

	t.Run("a sized stake carries no reason", func(t *testing.T) {
		// The admitting default arm writes SuppliedUint32{KNOWN, Value} and
		// never sets Reason, so a KNOWN stake carrying one is a shape the
		// producer cannot write. balanceReason — the caller-text path the
		// exemption was written for — is not reached on this arm at all.
		ev := predictioneval.EvaluateOrderedRules(proj.Stream, ruleCfg, trace)
		if ev.Status != predictioneval.StatusWouldAttempt || ev.Stake.Reason != "" {
			t.Fatalf("fixture: %q stake=%+v", ev.Status, ev.Stake)
		}
		if m := p4offline.MapP3bAction(ev); !m.Legal {
			t.Fatalf("genuine output must stay legal: %+v", m)
		}
		ev.Stake.Reason = predictioneval.ReasonBalanceInvalid
		if m := p4offline.MapP3bAction(ev); m.Legal ||
			!containsString(m.Illegality, "STAKE_REASON_CONTRADICTS_STATUS") {
			t.Fatalf("a sized stake with a balance failure reason must fail closed: %+v", m)
		}
	})
	t.Run("the out-of-domain arm writes a constant, not caller text", func(t *testing.T) {
		// This arm builds SuppliedUint32 directly with the constant, so unlike
		// the two balanceReason arms it IS bounded.
		bal := proj.Stream.Candidates[0].Balance
		bal.Value = 1 << 33
		ev := predictioneval.EvaluateOrderedRules(streamWith(t, bal), ruleCfg, trace)
		if ev.Reason != predictioneval.ReasonBalanceOutOfDomain ||
			ev.Stake.Reason != predictioneval.ReasonBalanceOutOfDomain {
			t.Fatalf("fixture: %q/%q stake=%+v", ev.Status, ev.Reason, ev.Stake)
		}
		if m := p4offline.MapP3bAction(ev); !m.Legal {
			t.Fatalf("genuine output must stay legal: %+v", m)
		}
		ev.Stake.Reason = predictioneval.ReasonBalanceNotEvaluated
		if m := p4offline.MapP3bAction(ev); m.Legal ||
			!containsString(m.Illegality, "STAKE_REASON_CONTRADICTS_REASON") {
			t.Fatalf("this arm's stake reason is a constant: %+v", m)
		}
	})
	t.Run("the two caller-text arms stay unread", func(t *testing.T) {
		// The exemption must survive where it is actually justified.
		for _, tc := range []struct {
			name string
			bal  predictioneval.SuppliedInt64
		}{
			{"an unsupplied balance", predictioneval.SuppliedInt64{Presence: predictioneval.SuppliedMissing, Reason: "caller wrote this"}},
			{"an invalid balance", predictioneval.SuppliedInt64{Presence: predictioneval.SuppliedInvalid, Reason: "and this"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ev := predictioneval.EvaluateOrderedRules(streamWith(t, tc.bal), ruleCfg, trace)
				if ev.Status != predictioneval.StatusParticipationAdmittedStakeUnknown {
					t.Fatalf("fixture: %q/%q", ev.Status, ev.Reason)
				}
				if ev.Stake.Reason != tc.bal.Reason {
					t.Fatalf("this arm passes the caller's text through: %+v", ev.Stake)
				}
				if m := p4offline.MapP3bAction(ev); !m.Legal {
					t.Fatalf("caller text on this arm is not a contradiction: %+v", m)
				}
			})
		}
	})
	t.Run("an admitting visit whose pool was never summed", func(t *testing.T) {
		// checkedPoolSum runs BEFORE the outcome loop and returns early on
		// failure, so PoolTotalKnown is true on every visit that can go on to
		// admit. A false flag on an admitting visit is the evaluation's own
		// record saying the pool could not be summed — which the producer emits
		// as UNKNOWN_INPUT, never as an admission.
		ev := predictioneval.EvaluateOrderedRules(proj.Stream, ruleCfg, trace)
		if ev.Selected == nil || !ev.Visits[len(ev.Visits)-1].PoolTotalKnown {
			t.Fatalf("fixture must admit with a summed pool: %+v", ev.Visits)
		}
		if m := p4offline.MapP3bAction(ev); !m.Legal {
			t.Fatalf("genuine output must stay legal: %+v", m)
		}
		for _, tc := range []struct {
			name string
			on   func(*predictioneval.OrderedRulesCandidateVisit)
			want string
		}{
			{"a pool the visit says was never summed", func(v *predictioneval.OrderedRulesCandidateVisit) {
				v.PoolTotalKnown, v.PoolTotal = false, 0
			}, "SELECTION_VISIT_POOL_NOT_SUMMED"},
			{"a negative pool total", func(v *predictioneval.OrderedRulesCandidateVisit) {
				v.PoolTotal = -999
			}, "SELECTION_VISIT_POOL_TOTAL_NEGATIVE"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ev := predictioneval.EvaluateOrderedRules(proj.Stream, ruleCfg, trace)
				visits := append([]predictioneval.OrderedRulesCandidateVisit(nil), ev.Visits...)
				tc.on(&visits[len(visits)-1])
				ev.Visits = visits
				if m := p4offline.MapP3bAction(ev); m.Legal || !containsString(m.Illegality, tc.want) {
					t.Fatalf("want %s: %+v", tc.want, m)
				}
			})
		}
	})
}

// TestP3bStakeAmountIsZeroWhereverNoStakeWasSized closes the last field an
// independent lane found classified on a justification true of one status.
//
// Only the admitting default arm sizes a stake, through pointsValue. Every
// other status writes a struct literal that leaves Value at its zero, so a
// non-zero amount outside WOULD_ATTEMPT is producer-impossible — and reading
// that costs nothing and reimplements no policy. The AMOUNT on WOULD_ATTEMPT
// stays unread, which is where the recomputation argument actually applies.
func TestP3bStakeAmountIsZeroWhereverNoStakeWasSized(t *testing.T) {
	_, fs := selectedFactset(t, nil, nil)
	proj, err := p4offline.ProjectP3bSingleCandidate(fs)
	if err != nil {
		t.Fatal(err)
	}
	coords := synthCoords(fs, 0)
	trace := mustDrawTrace(t, coords, 4)
	ruleCfg := cfgWithRule("a", predictioneval.ComparatorGe, 50, 100)
	streamWith := func(t *testing.T, bal predictioneval.SuppliedInt64) predictioneval.OrderedRulesStream {
		t.Helper()
		cands := append([]predictioneval.OrderedRulesCandidate(nil), proj.Stream.Candidates...)
		cands[0].Balance = bal
		re, err := predictioneval.ProjectOrderedRulesStream(
			predictioneval.OrderedRulesSource{Scope: proj.Stream.Scope, Candidates: cands}, proj.Stream.Admission)
		if err != nil {
			t.Fatal(err)
		}
		return re
	}
	missing := streamWith(t, predictioneval.SuppliedInt64{Presence: predictioneval.SuppliedMissing, Reason: "caller text"})

	for _, tc := range []struct {
		name   string
		stream predictioneval.OrderedRulesStream
		cfg    predictioneval.OrderedRulesConfig
		trace  predictioneval.SuppliedDrawTrace
		native predictioneval.OrderedRulesStatus
	}{
		{"no attempt in prefix", proj.Stream, cfgDefaultOnly("a", 95, 100), trace, predictioneval.StatusNoAttemptInSuppliedPrefix},
		{"unknown input", proj.Stream, cfgWithRule("a", predictioneval.ComparatorGe, 50, 50),
			predictioneval.SuppliedDrawTrace{RunID: "r", EntropySemanticsVersion: predictioneval.OrderedRulesEntropySemanticsVersion},
			predictioneval.StatusUnknownInput},
		{"refused", proj.Stream, predictioneval.OrderedRulesConfig{ConfigID: "nodefault"}, trace, predictioneval.StatusRefused},
		{"stake unknown", missing, ruleCfg, trace, predictioneval.StatusParticipationAdmittedStakeUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := predictioneval.EvaluateOrderedRules(tc.stream, tc.cfg, tc.trace)
			if ev.Status != tc.native {
				t.Fatalf("fixture produced %q/%q", ev.Status, ev.Reason)
			}
			if ev.Stake.Value != 0 {
				t.Fatalf("this arm sizes no stake: %+v", ev.Stake)
			}
			if m := p4offline.MapP3bAction(ev); !m.Legal {
				t.Fatalf("genuine output must stay legal: %+v", m)
			}
			ev.Stake.Value = 123456
			if m := p4offline.MapP3bAction(ev); m.Legal ||
				!containsString(m.Illegality, "STAKE_VALUE_CONTRADICTS_STATUS") {
				t.Fatalf("an amount no arm sized must fail closed: %+v", m)
			}
		})
	}
	t.Run("the sized amount itself stays unread", func(t *testing.T) {
		// The guard must NOT become "Value is always zero": on WOULD_ATTEMPT
		// the producer sizes a real stake and this package does not recompute
		// it.
		ev := predictioneval.EvaluateOrderedRules(proj.Stream, ruleCfg, trace)
		if ev.Status != predictioneval.StatusWouldAttempt || ev.Stake.Value == 0 {
			t.Fatalf("fixture must size a stake: %q %+v", ev.Status, ev.Stake)
		}
		ev.Stake.Value = 999999
		if m := p4offline.MapP3bAction(ev); !m.Legal {
			t.Fatalf("the amount is bound downstream, not here: %+v", m)
		}
	})
	t.Run("an admitting arm's caller text is never empty", func(t *testing.T) {
		ev := predictioneval.EvaluateOrderedRules(missing, ruleCfg, trace)
		if ev.Status != predictioneval.StatusParticipationAdmittedStakeUnknown {
			t.Fatalf("fixture: %q/%q", ev.Status, ev.Reason)
		}
		if m := p4offline.MapP3bAction(ev); !m.Legal {
			t.Fatalf("caller text is legal: %+v", m)
		}
		// balanceReason falls back to a non-empty constant, so empty is a shape
		// no admitting arm writes.
		ev.Stake.Reason = ""
		if m := p4offline.MapP3bAction(ev); m.Legal ||
			!containsString(m.Illegality, "STAKE_REASON_EMPTY_ON_ADMISSION") {
			t.Fatalf("an empty stake reason must fail closed: %+v", m)
		}
	})
}

// TestP3bTerminalReasonsTheProducerEmitsStayLegal is the test actionmap.go's
// vocabulary note points at, and it exists so that note claims exactly what is
// pinned and no more.
//
// Bounding a closed vocabulary refuses honest output as easily as forged, so
// the risk that matters is a reason the producer really emits being missing
// from one of the two maps. Membership was established by enumerating every
// emitting site; this drives the subset reachable through the exported
// evaluator and asserts each one maps LEGAL. It deliberately reports the set it
// reached, so the coverage claim cannot drift from the coverage.
func TestP3bTerminalReasonsTheProducerEmitsStayLegal(t *testing.T) {
	_, fs := selectedFactset(t, nil, nil)
	proj, err := p4offline.ProjectP3bSingleCandidate(fs)
	if err != nil {
		t.Fatal(err)
	}
	coords := synthCoords(fs, 0)
	trace := mustDrawTrace(t, coords, 4)
	good := cfgWithRule("a", predictioneval.ComparatorGe, 50, 100)
	tamper := func(f func(*predictioneval.OrderedRulesStream)) predictioneval.OrderedRulesStream {
		st := proj.Stream
		st.Candidates = append([]predictioneval.OrderedRulesCandidate(nil), proj.Stream.Candidates...)
		f(&st)
		return st
	}
	cases := []struct {
		name   string
		stream predictioneval.OrderedRulesStream
		cfg    predictioneval.OrderedRulesConfig
		trace  predictioneval.SuppliedDrawTrace
	}{
		{"config default not supplied", proj.Stream, predictioneval.OrderedRulesConfig{ConfigID: "nodefault"}, trace},
		{"config out of domain", proj.Stream, cfgWithRule("a", predictioneval.ComparatorGe, 50, 1000), trace},
		{"entropy exhausted", proj.Stream, cfgWithRule("a", predictioneval.ComparatorGe, 50, 50),
			predictioneval.SuppliedDrawTrace{RunID: "r", EntropySemanticsVersion: predictioneval.OrderedRulesEntropySemanticsVersion}},
		{"entropy semantics mismatch", proj.Stream, good,
			predictioneval.SuppliedDrawTrace{RunID: "r", EntropySemanticsVersion: "other/v0", Words: trace.Words}},
		{"stream contract version", tamper(func(s *predictioneval.OrderedRulesStream) {
			s.Scope.SourceContractVersion = "wrong/v0"
		}), good, trace},
		{"stream invariant violated", tamper(func(s *predictioneval.OrderedRulesStream) {
			s.Candidates[0].Position = 99
		}), good, trace},
		{"stream selection digest mismatch", tamper(func(s *predictioneval.OrderedRulesStream) {
			s.SelectionDigest = "0000000000000000000000000000000000000000000000000000000000000000"
		}), good, trace},
		{"supplied text not encodable", proj.Stream, func() predictioneval.OrderedRulesConfig {
			c := good
			c.ConfigID = string([]byte{0xff, 0xfe})
			return c
		}(), trace},
	}
	reached := map[string]bool{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := predictioneval.EvaluateOrderedRules(tc.stream, tc.cfg, tc.trace)
			if ev.Reason == "" {
				t.Fatalf("this fixture must produce a terminal reason: %q", ev.Status)
			}
			reached[string(ev.Status)+"/"+ev.Reason] = true
			if m := p4offline.MapP3bAction(ev); !m.Legal {
				t.Fatalf("the producer's own %s/%s must stay legal: %+v", ev.Status, ev.Reason, m)
			}
		})
	}
	// The number is asserted so that losing a case is a failure rather than a
	// silent narrowing of what the vocabulary note claims.
	if len(reached) < 7 {
		t.Fatalf("expected at least 7 distinct status/reason pairs, reached %d: %v", len(reached), reached)
	}
	t.Logf("reached %d distinct producer status/reason pairs: %v", len(reached), reached)
}

// TestRulesetRawDocumentIsCappedBeforeItIsHashed pins the ceiling on the raw
// ruleset document, from BOTH sides.
//
// The only size test was "not empty". A tiny valid config followed by an
// arbitrarily long whitespace tail decoded cleanly, trimmed to nothing,
// satisfied every native ceiling, and still cost a full-buffer SHA-256 and a
// full-buffer trailing scan.
//
// THE EXPECTED CEILING IS STATED HERE, as literals, and not read back from the
// code. An earlier version exposed the code's own rulesetRawCeiling to the test
// binary and computed the boundary from it, on the rationale that restating the
// formula would be tautological. That was exactly backwards: a restated formula
// is an independent expectation that fails when the code's changes, while a
// shim calling the function under test can never disagree with it. Nothing then
// failed if the allowance grew to 1<<24 or the sixfold became six hundred --
// the direction that loses the protection the ceiling exists for.
//
// The ceiling is derived from the identifier the document declares. A flat
// mebibyte was tried first and refused a document the contract admits: ConfigID
// carries no per-string length bound, and the producer says so and charges it
// only against its aggregate budget. "A large identifier raises the ceiling with
// it" is the regression control for that over-refusal, which is the worse
// direction here.
func TestRulesetRawDocumentIsCappedBeforeItIsHashed(t *testing.T) {
	// The expectation, independent of the implementation: one mebibyte of
	// structure, plus JSON escaping's worst case of six bytes per identifier
	// byte. Both numbers come from the contract, not from p3b.go.
	const allowance = 1 << 20
	const perIdentifierByte = 6
	wantCeiling := func(cfg predictioneval.OrderedRulesConfig) int {
		return allowance + perIdentifierByte*len(cfg.ConfigID)
	}

	cfg := cfgDefaultOnly("x", 95, 100)
	base := rulesetFrom(t, cfg)
	mustVerify(t, base)
	ceiling := wantCeiling(cfg)

	rehash := func(raw []byte) p4offline.P3bRuleset {
		sum := sha256.Sum256(raw)
		r := base
		r.RawBytes = raw
		r.RawSHA256 = hex.EncodeToString(sum[:])
		return r
	}
	pad := func(to int) []byte {
		raw := append([]byte(nil), base.RawBytes...)
		return append(raw, []byte(strings.Repeat(" ", to-len(raw)))...)
	}

	t.Run("a whitespace tail one byte past the stated ceiling is refused", func(t *testing.T) {
		// Fails if the allowance or the per-byte factor grows.
		if _, err := p4offline.VerifyP3bRuleset(rehash(pad(ceiling + 1))); !errors.Is(err, p4offline.ErrRulesetRawSize) {
			t.Fatalf("want ErrRulesetRawSize at %d bytes, got %v", ceiling+1, err)
		}
	})
	t.Run("a document at exactly the stated ceiling is admitted", func(t *testing.T) {
		// Fails if either shrinks, and if the comparison becomes exclusive.
		if _, err := p4offline.VerifyP3bRuleset(rehash(pad(ceiling))); err != nil {
			t.Fatalf("a document at %d bytes must be admitted: %v", ceiling, err)
		}
	})
	t.Run("the size is judged before the hash", func(t *testing.T) {
		// Without this the ordering is only claimed: every other case rehashes
		// correctly, so all of them would pass identically with the size test
		// moved below the SHA-256. An oversized document carrying a DELIBERATELY
		// WRONG hash must report the size, because the hash is never spent.
		r := rehash(pad(ceiling + 1))
		r.RawSHA256 = strings.Repeat("0", 64)
		_, err := p4offline.VerifyP3bRuleset(r)
		if !errors.Is(err, p4offline.ErrRulesetRawSize) {
			t.Fatalf("want ErrRulesetRawSize, got %v", err)
		}
		if errors.Is(err, p4offline.ErrRulesetRawHash) {
			t.Fatalf("the hash was spent on bytes the size already refused: %v", err)
		}
	})
	t.Run("a large identifier raises the ceiling with it", func(t *testing.T) {
		// THE REGRESSION CONTROL. ConfigID carries no per-string length bound:
		// the producer declines to invent one and charges it only against its
		// aggregate budget, and its own suite pins a mebibyte identifier as
		// legal. A ceiling that did not scale refused exactly this document.
		big := strings.Repeat("i", 1<<20)
		bigCfg := cfgDefaultOnly(big, 95, 100)
		r := rulesetFrom(t, bigCfg)
		if len(r.RawBytes) <= ceiling {
			t.Fatalf("fixture is not larger than a small ruleset's ceiling (%d bytes)", len(r.RawBytes))
		}
		if _, err := p4offline.VerifyP3bRuleset(r); err != nil {
			t.Fatalf("an identifier the contract admits must not be refused: %v", err)
		}
		t.Logf("a %d-byte identifier carries a %d-byte document against a stated ceiling of %d",
			len(big), len(r.RawBytes), wantCeiling(bigCfg))
	})
	t.Run("the allowance covers the widest body the contract permits", func(t *testing.T) {
		// Measured at the real worst case, not at json.Marshal's. The contract
		// admits any float in range, so the widest rendering is a full-precision
		// exponent form at 23 bytes -- not the 18-byte value a convenient
		// fixture produces -- and it admits every key and string token written
		// as \u escapes, which json.Marshal never emits. An earlier version
		// measured neither and understated the widest body several-fold.
		widest := cfgWithRule("x", predictioneval.ComparatorGe, 1.2345678901234567e-308, 100)
		widest.Detailed[0].Points = predictioneval.OrderedRulesPoints{
			MaxValue: 4294967295, RawPercent: 1.2345678901234567e-308}
		widest.Default = predictioneval.OrderedRulesDefault{
			RawMinPercent: 1.2345678901234567e-308, RawMaxPercent: 1.2345678901234567e-308,
			Points: predictioneval.OrderedRulesPoints{MaxValue: 4294967295, RawPercent: 1.2345678901234567e-308}}
		rule := widest.Detailed[0]
		for len(widest.Detailed) < predictioneval.MaxOrderedRulesRules {
			widest.Detailed = append(widest.Detailed, rule)
		}
		compact := mustMarshal(t, widest)
		indented, err := json.MarshalIndent(widest, "", "        ")
		if err != nil {
			t.Fatal(err)
		}
		// Every key and enum token escaped six-fold is the widest a conformant
		// encoder can write the same document; measured rather than assumed by
		// charging each of them at its escaped width.
		escaped := len(indented) + 5*countRulesetTokenBytes(indented)
		body := escaped - len(widest.ConfigID)
		if body >= allowance {
			t.Fatalf("the stated allowance %d does not cover a %d-byte body", allowance, body)
		}
		t.Logf("widest body: compact %d, indented %d, indented+escaped %d, against an allowance of %d (%.1fx)",
			len(compact)-len(widest.ConfigID), len(indented)-len(widest.ConfigID), body,
			allowance, float64(allowance)/float64(body))
		// And it must still verify: an allowance that covered only documents the
		// contract refuses would prove nothing.
		if _, err := p4offline.VerifyP3bRuleset(rulesetFrom(t, widest)); err != nil {
			t.Fatalf("the widest document the contract permits must verify: %v", err)
		}
	})
}

// countRulesetTokenBytes counts the bytes inside JSON string tokens, which are
// the ones a conformant encoder may write as \uXXXX escapes at six bytes each.
func countRulesetTokenBytes(doc []byte) int {
	n, inString, escaped := 0, false, false
	for _, b := range doc {
		switch {
		case escaped:
			escaped = false
		case b == '\\' && inString:
			escaped = true
		case b == '"':
			inString = !inString
		case inString:
			n++
		}
	}
	return n
}

// TestRulesetIdentityRefusalDoesNotMaterializeSuppliedText pins that the FIRST
// gate of VerifyP3bRuleset refuses without re-exporting the text it refuses.
//
// The gate compared two identifiers and, on a mismatch, built its error by
// quoting BOTH of them. That happens before the raw-byte ceiling is evaluated
// and before the native core is ever called, so on that path no bound of any
// kind governs the work: a 64 MiB identifier cost 476 ms and 235 MB of
// allocation to report that two strings differ.
//
// The expectation is not this package's invention. The native producer found
// and repaired the same defect in its own gate and wrote the rule down
// (ordered_rules.go, on a 125 MiB config identifier beside a wrong contract
// version: "1.23 seconds and allocated 251,662,352 bytes -- to compare two
// short strings and find them different. It is 3.8 microseconds and nothing
// now"), which is why orderedRulesUnreadRefusal re-exports none of the
// supplied text. This gate now follows the same rule: it reports the FAULT and
// the lengths, never the values.
func TestRulesetIdentityRefusalDoesNotMaterializeSuppliedText(t *testing.T) {
	// A mebibyte is far below the sizes that made this expensive, and far above
	// any bound an error message should carry. The assertion is a fixed budget,
	// stated here and not read from the code under test.
	const budget = 1024
	big := strings.Repeat("a", 1<<20)

	for _, tc := range []struct {
		name string
		r    p4offline.P3bRuleset
	}{
		{"an over-long config id that does not match the ruleset id", p4offline.P3bRuleset{
			RulesetID: "wanted-id",
			Config:    predictioneval.OrderedRulesConfig{ConfigID: big},
			RawBytes:  []byte("{}"),
		}},
		{"an over-long ruleset id that does not match the config id", p4offline.P3bRuleset{
			RulesetID: big,
			Config:    predictioneval.OrderedRulesConfig{ConfigID: "wanted-id"},
			RawBytes:  []byte("{}"),
		}},
		{"an over-long ruleset id with no config id at all", p4offline.P3bRuleset{
			RulesetID: big,
			RawBytes:  []byte("{}"),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p4offline.VerifyP3bRuleset(tc.r)
			if !errors.Is(err, p4offline.ErrRulesetIdentity) {
				t.Fatalf("want ErrRulesetIdentity, got %v", err)
			}
			if n := len(err.Error()); n > budget {
				t.Fatalf("the refusal carries %d bytes; a refusal must name the fault, not the input (budget %d)", n, budget)
			}
		})
	}

	t.Run("the refusal still says which side is wrong", func(t *testing.T) {
		// Declining to quote the values must not cost the diagnosis: the reader
		// of the error still has to be able to tell the two faults apart.
		empty := p4offline.P3bRuleset{Config: predictioneval.OrderedRulesConfig{ConfigID: "x"}, RawBytes: []byte("{}")}
		mismatch := p4offline.P3bRuleset{RulesetID: "a", Config: predictioneval.OrderedRulesConfig{ConfigID: "b"}, RawBytes: []byte("{}")}
		_, e1 := p4offline.VerifyP3bRuleset(empty)
		_, e2 := p4offline.VerifyP3bRuleset(mismatch)
		if e1 == nil || e2 == nil || e1.Error() == e2.Error() {
			t.Fatalf("an empty identity and a mismatched one must be distinguishable: %v / %v", e1, e2)
		}
	})
}

// TestP3bTerminalStatusesCarryTheTraversalStateTheProducerWrote closes the last
// open half of the seam-C matrix: five fields classified VALIDATED_LOAD_BEARING
// that were read only inside requireCoherentSelection, which returns at once
// when no selection is carried. On NO_ATTEMPT_IN_SUPPLIED_PREFIX, UNKNOWN_INPUT
// and REFUSED they were therefore unread entirely.
//
// Every relation below is the producer's, read off its writers. The four stop
// and count fields are written in one adjacent block at the top of the candidate
// loop (CandidatesConsumed = ci+1, StoppedAtCandidate, StoppedAtPosition,
// HasStopPosition = true), and every one of that loop's exit paths appends
// exactly one visit -- which is what makes len(Visits) == CandidatesConsumed an
// invariant on EVERY terminal path, not only some.
//
// The REFUSED split is keyed on the REASON, not on a tier name, because the
// reason is what distinguishes the two writers: refuse() is called at exactly
// three sites with exactly two reasons (WORK_BUDGET_EXCEEDED twice,
// ATTEMPT_RATE_OUT_OF_DOMAIN once); every other refusal reason is emitted only
// before the loop, from a constructor that builds a fresh result. That is three
// call sites in one producer function, so a future mid-traversal refusal reusing
// one of the twelve pre-traversal reason names would break this -- named here
// so the next person to add one sees what their reason choice now means.
func TestP3bTerminalStatusesCarryTheTraversalStateTheProducerWrote(t *testing.T) {
	_, fs := selectedFactset(t, nil, nil)
	proj, err := p4offline.ProjectP3bSingleCandidate(fs)
	if err != nil {
		t.Fatal(err)
	}
	coords := synthCoords(fs, 0)
	trace := mustDrawTrace(t, coords, 4)

	native := func(t *testing.T, cfg predictioneval.OrderedRulesConfig, tr predictioneval.SuppliedDrawTrace,
		want predictioneval.OrderedRulesStatus) predictioneval.OrderedRulesEvaluation {
		t.Helper()
		ev := predictioneval.EvaluateOrderedRules(proj.Stream, cfg, tr)
		if ev.Status != want {
			t.Fatalf("fixture must be %q, got %q/%q", want, ev.Status, ev.Reason)
		}
		if m := p4offline.MapP3bAction(ev); !m.Legal {
			t.Fatalf("the untouched producer output must stay legal: %+v", m)
		}
		return ev
	}
	noDefault := predictioneval.OrderedRulesConfig{ConfigID: "nodefault"}
	noEntropy := predictioneval.SuppliedDrawTrace{RunID: "r",
		EntropySemanticsVersion: predictioneval.OrderedRulesEntropySemanticsVersion}

	t.Run("an unread refusal cannot carry traversal state", func(t *testing.T) {
		// Counterexample 1: a refusal that declined to read the input, forged to
		// carry a ghost stop, an admitting trace entry and an ADMITTED visit.
		ev := native(t, noDefault, trace, predictioneval.StatusRefused)
		if ev.HasStopPosition || len(ev.Trace) != 0 || len(ev.Visits) != 0 {
			t.Fatalf("fixture must refuse before the traversal: %+v", ev)
		}
		ev.HasStopPosition = true
		ev.StoppedAtCandidate = "GHOST-CANDIDATE"
		ev.StoppedAtPosition = 424242
		ev.Trace = []predictioneval.OrderedRulesTraceEntry{{Step: predictioneval.TraceStepRuleDraw,
			Admitted: true, RawWordIndex: 999999}}
		ev.Visits = []predictioneval.OrderedRulesCandidateVisit{{Verdict: predictioneval.CandidateAdmitted}}
		if m := p4offline.MapP3bAction(ev); m.Legal ||
			!containsString(m.Illegality, "UNREAD_REFUSAL_CARRIES_TRAVERSAL_STATE") {
			t.Fatalf("want UNREAD_REFUSAL_CARRIES_TRAVERSAL_STATE: %+v", m)
		}
	})

	t.Run("each clause of the unread-refusal guard is pinned on its own", func(t *testing.T) {
		// ONE CLAUSE AT A TIME, which the case above does not do. Tampering all
		// five fields together cannot tell the guard from a strictly weaker
		// one: an independent lane deleted `!ev.HasStopPosition &&` -- a clause
		// THIS branch added -- and the whole package suite stayed green,
		// because the four remaining clauses still caught the all-five forgery.
		// A defence-in-depth clause whose removal a sibling clause masks is an
		// unprotected barrier, not an equivalent mutant.
		for _, tc := range []struct {
			name string
			edit func(*predictioneval.OrderedRulesEvaluation)
		}{
			{"a stop position alone", func(e *predictioneval.OrderedRulesEvaluation) {
				e.HasStopPosition = true
			}},
			{"a stopped candidate alone", func(e *predictioneval.OrderedRulesEvaluation) {
				e.StoppedAtCandidate = "GHOST-CANDIDATE"
			}},
			{"a stopped position value alone", func(e *predictioneval.OrderedRulesEvaluation) {
				e.StoppedAtPosition = 424242
			}},
			{"a trace entry alone", func(e *predictioneval.OrderedRulesEvaluation) {
				e.Trace = []predictioneval.OrderedRulesTraceEntry{{Step: predictioneval.TraceStepDefaultBounds,
					RawWordIndex: -1}}
			}},
			{"a visit alone", func(e *predictioneval.OrderedRulesEvaluation) {
				e.Visits = []predictioneval.OrderedRulesCandidateVisit{{Verdict: predictioneval.CandidateAdmitted}}
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ev := native(t, noDefault, trace, predictioneval.StatusRefused)
				tc.edit(&ev)
				m := p4offline.MapP3bAction(ev)
				// The arm's OWN identifier must be named. Other relations may
				// fire beside it -- a lone stop also contradicts the candidate
				// count -- and that is fine; what must not happen is this
				// clause going unnamed.
				if m.Legal || !containsString(m.Illegality, "UNREAD_REFUSAL_CARRIES_TRAVERSAL_STATE") {
					t.Fatalf("want UNREAD_REFUSAL_CARRIES_TRAVERSAL_STATE for %s: %+v", tc.name, m)
				}
			})
		}
	})

	t.Run("a visit count that contradicts the candidates consumed", func(t *testing.T) {
		// Counterexample 2, in the direction the producer can never write.
		ev := native(t, cfgDefaultOnly("a", 95, 100), trace, predictioneval.StatusNoAttemptInSuppliedPrefix)
		if ev.CandidatesConsumed == 0 || len(ev.Visits) != ev.CandidatesConsumed {
			t.Fatalf("fixture must have walked candidates: %+v", ev)
		}
		ev.Visits = nil
		ev.Trace = nil
		if m := p4offline.MapP3bAction(ev); m.Legal ||
			!containsString(m.Illegality, "VISIT_COUNT_CONTRADICTS_CANDIDATES_CONSUMED") {
			t.Fatalf("want VISIT_COUNT_CONTRADICTS_CANDIDATES_CONSUMED: %+v", m)
		}
	})

	t.Run("a cleared stop beside candidates the traversal consumed", func(t *testing.T) {
		ev := native(t, cfgDefaultOnly("a", 95, 100), trace, predictioneval.StatusNoAttemptInSuppliedPrefix)
		ev.HasStopPosition = false
		if m := p4offline.MapP3bAction(ev); m.Legal ||
			!containsString(m.Illegality, "STOP_POSITION_CONTRADICTS_CANDIDATES_CONSUMED") {
			t.Fatalf("want STOP_POSITION_CONTRADICTS_CANDIDATES_CONSUMED: %+v", m)
		}
	})

	t.Run("a stop that names no candidate", func(t *testing.T) {
		// The other half of the same entry, and the half that was asserted in
		// the matrix and enforced nowhere. "Blank together when none was" was
		// held; "present exactly when a candidate was consumed" was held only
		// where a SELECTION names the candidate, i.e. on the two admitting
		// statuses, inside requireCoherentSelection -- which returns at once
		// when Selected is nil. On every terminal status the non-blank half was
		// unread.
		//
		// The producer cannot write this. orderedRulesStreamInvariantsBroken
		// refuses any candidate whose Identity is empty
		// (ordered_rules.go:799, checkIdentifierPresent) BEFORE the traversal
		// begins, and the traversal writes StoppedAtCandidate = c.Identity in
		// the same adjacent block that sets HasStopPosition = true
		// (ordered_rules.go:1544-1547). So a stop with no candidate is a
		// contradiction in the evidence, on every status.
		for _, tc := range []struct {
			name string
			ev   func(t *testing.T) predictioneval.OrderedRulesEvaluation
		}{
			{"no attempt in the supplied prefix", func(t *testing.T) predictioneval.OrderedRulesEvaluation {
				return native(t, cfgDefaultOnly("a", 95, 100), trace, predictioneval.StatusNoAttemptInSuppliedPrefix)
			}},
			{"unknown input", func(t *testing.T) predictioneval.OrderedRulesEvaluation {
				return native(t, cfgWithRule("a", predictioneval.ComparatorGe, 50, 50), noEntropy,
					predictioneval.StatusUnknownInput)
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ev := tc.ev(t)
				if !ev.HasStopPosition || ev.StoppedAtCandidate == "" {
					t.Fatalf("fixture must carry a named stop: %+v", ev)
				}
				ev.StoppedAtCandidate = ""
				if m := p4offline.MapP3bAction(ev); m.Legal ||
					!containsString(m.Illegality, "STOP_CANDIDATE_MISSING_WITH_STOP_POSITION") {
					t.Fatalf("want STOP_CANDIDATE_MISSING_WITH_STOP_POSITION: %+v", m)
				}
			})
		}
	})

	t.Run("stop fields beside no stop at all", func(t *testing.T) {
		// Counterexample 3's other half: the ghost stop on a shape that really
		// did stop nowhere.
		//
		// ONE CLAUSE AT A TIME. Every case in this package naming this tag used
		// to violate BOTH conjuncts at once, so replacing either with `true`
		// left the whole suite green -- the sibling half of the very guard this
		// branch split for the unread-refusal arm, and found by the same kind
		// of sweep one round later. A build in which an evaluation declares no
		// stop yet names a stopped-at candidate, or carries a non-zero stop
		// position, would have mapped legal with nothing saying so.
		for _, tc := range []struct {
			name string
			edit func(*predictioneval.OrderedRulesEvaluation)
		}{
			{"a stopped candidate alone", func(e *predictioneval.OrderedRulesEvaluation) {
				e.StoppedAtCandidate = "GHOST"
			}},
			{"a stopped position alone", func(e *predictioneval.OrderedRulesEvaluation) {
				e.StoppedAtPosition = -99
			}},
			{"both together", func(e *predictioneval.OrderedRulesEvaluation) {
				e.StoppedAtCandidate = "GHOST"
				e.StoppedAtPosition = -99
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ev := native(t, noDefault, trace, predictioneval.StatusRefused)
				if ev.HasStopPosition {
					t.Fatalf("fixture must have stopped nowhere: %+v", ev)
				}
				tc.edit(&ev)
				if m := p4offline.MapP3bAction(ev); m.Legal ||
					!containsString(m.Illegality, "STOP_FIELDS_WITHOUT_STOP_POSITION") {
					t.Fatalf("want STOP_FIELDS_WITHOUT_STOP_POSITION for %s: %+v", tc.name, m)
				}
			})
		}
	})

	t.Run("an admission that claims no candidate at all", func(t *testing.T) {
		// NO_CANDIDATE_REACHED is a disjunction beside a count, and its SECOND
		// conjunct was unpinned: no case cleared the counter while leaving the
		// selection and the stop in place. The producer cannot write that --
		// CandidatesConsumed, the stop and HasStopPosition are the same three
		// adjacent lines -- so it is a contradiction in the evidence. Other
		// identifiers fire beside it here, which is expected; what matters is
		// that this one is named.
		ev := native(t, cfgWithRule("a", predictioneval.ComparatorGe, 50, 100), trace,
			predictioneval.StatusWouldAttempt)
		if ev.Selected == nil || !ev.HasStopPosition || ev.CandidatesConsumed < 1 {
			t.Fatalf("fixture must admit, carrying a stop and a consumed candidate: %+v", ev)
		}
		ev.CandidatesConsumed = 0
		if m := p4offline.MapP3bAction(ev); m.Legal ||
			!containsString(m.Illegality, "NO_CANDIDATE_REACHED") {
			t.Fatalf("want NO_CANDIDATE_REACHED: %+v", m)
		}
	})

	t.Run("an unknown input that claims it never entered the loop", func(t *testing.T) {
		// Counterexample 3: every UNKNOWN_INPUT site is below the stop block, so
		// the producer sets the stop before any of them can be reached.
		ev := native(t, cfgWithRule("a", predictioneval.ComparatorGe, 50, 50), noEntropy,
			predictioneval.StatusUnknownInput)
		if !ev.HasStopPosition {
			t.Fatalf("fixture must carry a stop: %+v", ev)
		}
		ev.HasStopPosition = false
		ev.StoppedAtCandidate = ""
		ev.StoppedAtPosition = 0
		ev.CandidatesConsumed = 0
		ev.Visits = nil
		if m := p4offline.MapP3bAction(ev); m.Legal ||
			!containsString(m.Illegality, "UNKNOWN_INPUT_WITHOUT_STOP_POSITION") {
			t.Fatalf("want UNKNOWN_INPUT_WITHOUT_STOP_POSITION: %+v", m)
		}
	})

	t.Run("a mid-traversal refusal that claims it never entered the loop", func(t *testing.T) {
		// WORK_BUDGET_EXCEEDED is the only REACHABLE mid-traversal refusal, and it
		// carries a stop and a visit per candidate consumed -- driven for real at
		// 32 candidates x 64 outcomes x 128 rules it yields hasStop=true,
		// stopCand="c31", stopPos=32, trace=262144, visits=32, cand=32.
		//
		// THIS PACKAGE CANNOT REACH IT, and the reason is the protocol, not an
		// omission: the P3b seam projects exactly ONE candidate, so the most work a
		// projection can buy is MaxOrderedRulesOutcomes x MaxOrderedRulesRules,
		// about thirty times under the budget. The status/reason pair is therefore
		// set here, on top of traversal state the producer wrote, and the control
		// below pins that the untouched shape is admitted.
		ev := native(t, cfgDefaultOnly("a", 95, 100), trace, predictioneval.StatusNoAttemptInSuppliedPrefix)
		ev.Status = predictioneval.StatusRefused
		ev.Reason = predictioneval.ReasonWorkBudgetExceeded
		if m := p4offline.MapP3bAction(ev); !m.Legal {
			t.Fatalf("a mid-traversal refusal carrying its traversal state is admitted: %+v", m)
		}
		ev.HasStopPosition = false
		ev.StoppedAtCandidate = ""
		ev.StoppedAtPosition = 0
		ev.CandidatesConsumed = 0
		ev.Visits = nil
		if m := p4offline.MapP3bAction(ev); m.Legal ||
			!containsString(m.Illegality, "MID_TRAVERSAL_REFUSAL_WITHOUT_STOP") {
			t.Fatalf("want MID_TRAVERSAL_REFUSAL_WITHOUT_STOP: %+v", m)
		}
	})

	t.Run("the two tiers are told apart by the reason, not by the shape", func(t *testing.T) {
		// The discriminator: the SAME traversal state is admitted under a
		// mid-traversal reason and refused under a pre-traversal one. Without this
		// the two arms could both be satisfied by a single weaker predicate.
		ev := native(t, cfgDefaultOnly("a", 95, 100), trace, predictioneval.StatusNoAttemptInSuppliedPrefix)
		ev.Status = predictioneval.StatusRefused
		ev.Reason = predictioneval.ReasonWorkBudgetExceeded
		if m := p4offline.MapP3bAction(ev); !m.Legal {
			t.Fatalf("mid-traversal reason must admit this shape: %+v", m)
		}
		ev.Reason = predictioneval.ReasonConfigDefaultNotSupplied
		if m := p4offline.MapP3bAction(ev); m.Legal ||
			!containsString(m.Illegality, "UNREAD_REFUSAL_CARRIES_TRAVERSAL_STATE") {
			t.Fatalf("a pre-traversal reason must refuse the same shape: %+v", m)
		}
	})

	t.Run("more visits than the traversal consumed candidates", func(t *testing.T) {
		// The other direction of the same accounting. Without it the relation
		// could be weakened from an equality to a lower bound and nothing on a
		// terminal status would notice.
		ev := native(t, cfgDefaultOnly("a", 95, 100), trace, predictioneval.StatusNoAttemptInSuppliedPrefix)
		ev.Visits = append(append([]predictioneval.OrderedRulesCandidateVisit(nil), ev.Visits...), ev.Visits[0])
		if m := p4offline.MapP3bAction(ev); m.Legal ||
			!containsString(m.Illegality, "VISIT_COUNT_CONTRADICTS_CANDIDATES_CONSUMED") {
			t.Fatalf("want VISIT_COUNT_CONTRADICTS_CANDIDATES_CONSUMED: %+v", m)
		}
	})

	t.Run("a refusal reason outside the vocabulary is named once, not twice", func(t *testing.T) {
		// The tier split is keyed on the reason, so a reason in NEITHER tier
		// must fall through rather than be judged by the pre-traversal rule.
		// That is a deliberate fail-open: such a shape is already named by
		// REFUSAL_REASON_FOREIGN, and naming it again here would report one
		// contradiction as two. Pinned because nothing else holds the choice.
		ev := native(t, cfgDefaultOnly("a", 95, 100), trace, predictioneval.StatusNoAttemptInSuppliedPrefix)
		ev.Status = predictioneval.StatusRefused
		ev.Reason = "FORGED"
		m := p4offline.MapP3bAction(ev)
		if m.Legal || !containsString(m.Illegality, "REFUSAL_REASON_FOREIGN") {
			t.Fatalf("a foreign refusal reason must fail closed: %+v", m)
		}
		if containsString(m.Illegality, "UNREAD_REFUSAL_CARRIES_TRAVERSAL_STATE") {
			t.Fatalf("a reason in neither tier must not be judged by the pre-traversal rule: %+v", m)
		}
	})

	t.Run("every terminal shape the producer really emits stays legal", func(t *testing.T) {
		// The false-refusal control, and the one that matters most here: these
		// relations are the easiest place in the package to refuse honest output.
		for _, tc := range []struct {
			name  string
			cfg   predictioneval.OrderedRulesConfig
			trace predictioneval.SuppliedDrawTrace
		}{
			{"no attempt in prefix", cfgDefaultOnly("a", 95, 100), trace},
			{"entropy exhausted", cfgWithRule("a", predictioneval.ComparatorGe, 50, 50), noEntropy},
			{"config default not supplied", noDefault, trace},
			{"config out of domain", cfgWithRule("a", predictioneval.ComparatorGe, 50, 1000), trace},
			{"entropy semantics mismatch", cfgWithRule("a", predictioneval.ComparatorGe, 50, 100),
				predictioneval.SuppliedDrawTrace{RunID: "r", EntropySemanticsVersion: "other/v0", Words: trace.Words}},
			{"a default admission", cfgDefaultOnly("a", 0, 100), trace},
			{"a detailed-rule admission", cfgWithRule("a", predictioneval.ComparatorGe, 50, 100), trace},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ev := predictioneval.EvaluateOrderedRules(proj.Stream, tc.cfg, tc.trace)
				if m := p4offline.MapP3bAction(ev); !m.Legal {
					t.Fatalf("producer %q/%q must map legal: %+v", ev.Status, ev.Reason, m)
				}
			})
		}
	})
}

// TestEntropyBindingRefusalDoesNotMaterializeTheFactsetRound is the third
// sibling of the same defect class as
// TestRulesetIdentityRefusalDoesNotMaterializeSuppliedText. bindEntropyCoordinates
// compares the coordinates' opportunity against the FACTSET's round. The
// coordinate side is bounded at MaxOrderedRulesIdentifierBytes by
// checkEntropyCoordinates, which runs first; the factset side is bounded by
// nothing in this package -- VerifyCommonFactset checks the two contract
// strings, the digest and the label consistency, and no length. So every
// factset whose round exceeds the coordinate bound NECESSARILY fails this
// equality and, before the repair, necessarily paid to quote the whole thing.
//
// The path is the exported one: EvaluateP3bCase takes the projection-refused
// branch and binds the coordinates there.
func TestEntropyBindingRefusalDoesNotMaterializeTheFactsetRound(t *testing.T) {
	const budget = 1024
	big := strings.Repeat("a", 1<<20)

	_, fs := selectedFactset(t, nil, nil)
	fs.Episode.EventID = big
	fs.Digest = digestOf(p4offline.SerializeCommonFactset(fs))
	if err := p4offline.VerifyCommonFactset(fs); err != nil {
		t.Fatalf("the fixture must be a factset the package accepts: %v", err)
	}

	coords := p4offline.EntropyCoordinates{
		DatasetID:           "d",
		DatasetVersion:      "v1",
		CommonFactsetDigest: p4offline.DigestReference(fs.Digest),
		PairedOpportunityID: "round",
	}
	rs := mustVerify(t, rulesetFrom(t, cfgDefaultOnly("c1", 0, 1)))

	_, err := p4offline.EvaluateP3bCase(fs, rs, coords)
	if !errors.Is(err, p4offline.ErrP3bBinding) {
		t.Fatalf("want ErrP3bBinding, got %v", err)
	}
	if n := len(err.Error()); n > budget {
		t.Fatalf("the refusal carries %d bytes; a refusal must name the fault, not the factset's round (budget %d)", n, budget)
	}

	t.Run("each binding fault still names its own side", func(t *testing.T) {
		// BOTH FAULTS THROUGH THE SAME SEAM, and each one asserted on its own
		// CONTENT. A first version of this subtest produced its "other" error
		// from ValidateDrawTrace with a zero-value trace, which returns at its
		// first gate with a SEMANTICS fault and never reaches
		// bindEntropyCoordinates at all -- so it compared a trace fault against
		// a binding fault, two different sentinels, and could not fail for any
		// implementation of either message. Two independent lanes found that,
		// and both demonstrated it the same way: collapsing the two binding
		// messages into one identical string left the whole suite green. Which
		// is the direction this round was pushing in.
		_, small := selectedFactset(t, nil, nil)

		wrongDigest := synthCoords(small, 0)
		wrongDigest.CommonFactsetDigest = p4offline.DigestReference(strings.Repeat("ab", 32))
		_, e1 := p4offline.EvaluateP3bCase(small, rs, wrongDigest)
		if !errors.Is(e1, p4offline.ErrP3bBinding) {
			t.Fatalf("a foreign factset digest must be refused as a binding fault: %v", e1)
		}
		// The digest side IS quoted, deliberately: isDigestReference holds both
		// values to 71 bytes and naming them is the diagnosis.
		if !strings.Contains(e1.Error(), "entropy factset digest") ||
			!strings.Contains(e1.Error(), strings.Repeat("ab", 32)) {
			t.Fatalf("the digest fault must name itself and the digest it was given: %v", e1)
		}

		wrongRound := synthCoords(small, 0)
		wrongRound.PairedOpportunityID = "not-this-round"
		_, e2 := p4offline.EvaluateP3bCase(small, rs, wrongRound)
		if !errors.Is(e2, p4offline.ErrP3bBinding) {
			t.Fatalf("a foreign round must be refused as a binding fault: %v", e2)
		}
		// The round side is NOT quoted -- the factset's round is unbounded --
		// so what it must carry is its own name and both byte counts.
		if !strings.Contains(e2.Error(), "paired opportunity") ||
			!strings.Contains(e2.Error(), strconv.Itoa(len(wrongRound.PairedOpportunityID))+" bytes") ||
			!strings.Contains(e2.Error(), strconv.Itoa(len(small.Episode.EventID))+" bytes") {
			t.Fatalf("the round fault must name itself and both lengths: %v", e2)
		}
		if strings.Contains(e2.Error(), wrongRound.PairedOpportunityID) ||
			strings.Contains(e2.Error(), small.Episode.EventID) {
			t.Fatalf("the round fault must not re-export either identity: %v", e2)
		}
		if e1.Error() == e2.Error() {
			t.Fatalf("the two binding faults must be distinguishable: %v / %v", e1, e2)
		}
	})
}

// TestOverLongTraceIsRefusedBeforeItIsCopied pins the ORDER of two steps that
// both have to happen. evaluateProjected must detach the caller's word array
// before validating it, because the native core consumes the words it is
// handed without copying -- so the array that is checked has to be the array
// that is read. But the detachment used to happen before the gate whose only
// job is to refuse an over-long array, so a slice far past the declared
// ceiling was duplicated in full on its way to being refused.
//
// The assertion is an allocation RATIO against the supplied array, never a
// wall-clock threshold: a time bound on shared CI proves nothing and flakes.
func TestOverLongTraceIsRefusedBeforeItIsCopied(t *testing.T) {
	_, fs := selectedFactset(t, nil, nil)
	rs := mustVerify(t, rulesetFrom(t, cfgDefaultOnly("c1", 0, 1)))
	coords := synthCoords(fs, 0)

	// Four times the declared ceiling: big enough that a full copy is
	// unmistakable against allocator noise, small enough for shared CI.
	const words = 4 * predictioneval.MaxOrderedRulesDrawWords
	const suppliedBytes = words * 8
	trace := predictioneval.SuppliedDrawTrace{
		EntropySemanticsVersion: predictioneval.OrderedRulesEntropySemanticsVersion,
		Words:                   make([]predictioneval.OrderedRulesHex64, words),
	}

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := p4offline.EvaluateP3bWithTrace(fs, rs, coords, trace)
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("supplied %d words (%d bytes); the refusing call allocated %d bytes", words, suppliedBytes, allocated)

	if !errors.Is(err, p4offline.ErrEntropyCount) {
		t.Fatalf("an array past the ceiling must be refused as a count fault, got %v", err)
	}
	// Half the supplied array is far above anything the refusal legitimately
	// needs and far below the full copy the defect made.
	if allocated > suppliedBytes/2 {
		t.Fatalf("the refusing call allocated %d bytes against a %d-byte supplied array: the array was copied before it was judged",
			allocated, suppliedBytes)
	}

	t.Run("a trace the package itself draws is still admitted", func(t *testing.T) {
		// The gate must not become the reason legal input is refused. This is
		// the false-refusal control, and it runs on the trace this very
		// projection needs rather than on a hand-picked number.
		if _, err := p4offline.EvaluateP3bCase(fs, rs, coords); err != nil {
			t.Fatalf("the projection's own trace must be admitted: %v", err)
		}
	})

	t.Run("the gate's own boundary, not the ceiling constant's", func(t *testing.T) {
		// THIS CALL SITE'S ARGUMENT, not checkEntropyCount itself. A lane
		// showed the difference: entropy_test.go's count table pins the
		// helper, and the two mutants that delete this gate or move it back
		// after the copy are killed above -- but `len(trace.Words) + 1`
		// survived all of them, falsely refusing a legal ceiling-sized trace
		// with "1048577 words requested". Nothing distinguished the helper's
		// boundary from this expression's until here.
		//
		// The word VALUES are irrelevant to a count gate, so zero-valued words
		// are used and no HMAC work is done. The RunID is deliberately not the
		// one this count would have: the assertion is on WHICH fault is
		// reported, and the count gate runs before the trace is validated.
		words := func(n int) predictioneval.SuppliedDrawTrace {
			return predictioneval.SuppliedDrawTrace{
				EntropySemanticsVersion: predictioneval.OrderedRulesEntropySemanticsVersion,
				Words:                   make([]predictioneval.OrderedRulesHex64, n),
			}
		}
		if _, err := p4offline.EvaluateP3bWithTrace(fs, rs, coords,
			words(predictioneval.MaxOrderedRulesDrawWords+1)); !errors.Is(err, p4offline.ErrEntropyCount) {
			t.Fatalf("one word past the ceiling must be a count fault, got %v", err)
		}
		// AT the ceiling the count gate must let it through. What it is then
		// refused for is a run-identity fault, because these words were never
		// drawn -- and that is the point: the count gate is no longer the one
		// answering.
		if _, err := p4offline.EvaluateP3bWithTrace(fs, rs, coords,
			words(predictioneval.MaxOrderedRulesDrawWords)); errors.Is(err, p4offline.ErrEntropyCount) {
			t.Fatalf("a trace AT the ceiling must pass the count gate, got %v", err)
		}
	})
}

// TestAnUnreadRefusalStillCarriesWhatTheProducerEarned is the control for a
// correction, not for a repair: the seam-C matrix used to assert that the
// producer withholds its digests, cutoff and qualifications on every unread
// refusal, so a refusal carrying them was producer-impossible. An independent
// lane executed the producer and showed otherwise, and this test keeps the
// corrected reading honest by driving the shape the matrix called impossible.
//
// It also pins the property the REFUSED arm actually depends on, which is the
// narrower one that survived: neither pre-traversal helper can carry TRAVERSAL
// state, because every site of both sits above the candidate loop's first
// write.
func TestAnUnreadRefusalStillCarriesWhatTheProducerEarned(t *testing.T) {
	_, fs := selectedFactset(t, nil, nil)
	proj, err := p4offline.ProjectP3bSingleCandidate(fs)
	if err != nil {
		t.Fatal(err)
	}
	coords := synthCoords(fs, 0)
	trace := mustDrawTrace(t, coords, 4)

	// An unencodable config id: the producer refuses with
	// SUPPLIED_TEXT_NOT_ENCODABLE from the LOCAL closure, below the
	// derived-field assignment.
	cfg := cfgDefaultOnly("a", 0, 1)
	cfg.ConfigID = "cfg-\xff\xfe"
	ev := predictioneval.EvaluateOrderedRules(proj.Stream, cfg, trace)
	if ev.Status != predictioneval.StatusRefused || ev.Reason != predictioneval.ReasonSuppliedTextNotEncodable {
		t.Fatalf("fixture must be the unencodable-text refusal, got %q/%q", ev.Status, ev.Reason)
	}
	if !refusalReasonIsInP4Vocabulary(ev.Reason) {
		t.Fatalf("the reason must be one p4offline recognises: %q", ev.Reason)
	}

	// The shape the matrix called producer-impossible.
	if ev.StreamDigest == "" {
		t.Fatalf("this refusal carries the stream digest it computed; the matrix said it does not")
	}
	if len(ev.Qualifications) == 0 {
		t.Fatalf("this refusal carries the qualifications it derived; the matrix said it does not")
	}
	// And it maps legal, because none of those three can make an admitted
	// shape unadmitted or the reverse -- which is the reason these fields are
	// class B, and the only reason that was ever true.
	if m := p4offline.MapP3bAction(ev); !m.Legal {
		t.Fatalf("ordinary producer output must map legal: %+v", m)
	}

	t.Run("and an established cutoff, which is the limb the matrix reversed", func(t *testing.T) {
		// THE THIRD FIELD THE MATRIX ENTRY NAMES. The fixture above comes from
		// ProjectP3bSingleCandidate, which supplies no interventions, so its
		// cutoff is always Established:false -- and a lane pointed out that the
		// one limb reversing the previous round's producer claim about DERIVED
		// fields was the limb the control could not see. If a future producer
		// change started clearing Cutoff on the refuseUnread path, restoring
		// the old and wrong matrix reading, the control above would still pass.
		//
		// The stream is rebuilt on the producer's own projection seam from the
		// candidate this very projection produced -- so the balance keeps its
		// provenance -- with the declared interval widened by one and a single
		// PROVEN_RELEVANT intervention placed in the gap.
		cand := proj.Stream.Candidates[0]
		scope := proj.Stream.Scope
		scope.IntervalToPosition = cand.Position + 1
		src := predictioneval.OrderedRulesSource{
			Scope:      scope,
			Candidates: []predictioneval.OrderedRulesCandidate{cand},
			Interventions: []predictioneval.OrderedRulesIntervention{{
				Identity: "obs-x", Position: cand.Position + 1, HasPosition: true,
				Kind: predictioneval.InterventionAutoCallStarted, Relevance: predictioneval.RelevanceProven,
			}},
		}
		stream, err := predictioneval.ProjectOrderedRulesStream(src, proj.Stream.Admission)
		if err != nil {
			t.Fatalf("the fixture stream must project: %v", err)
		}
		if !stream.Cutoff.Established {
			t.Fatalf("the fixture must establish a cutoff: %+v", stream.Cutoff)
		}
		cut := predictioneval.EvaluateOrderedRules(stream, cfg, trace)
		if cut.Status != predictioneval.StatusRefused || cut.Reason != predictioneval.ReasonSuppliedTextNotEncodable {
			t.Fatalf("fixture must be the unencodable-text refusal, got %q/%q", cut.Status, cut.Reason)
		}
		if !cut.Cutoff.Established {
			t.Fatalf("this refusal carries the cutoff it established; the matrix said it does not: %+v", cut.Cutoff)
		}
		if cut.StreamDigest == "" || len(cut.Qualifications) == 0 {
			t.Fatalf("and the other two fields with it: %+v", cut)
		}
		if m := p4offline.MapP3bAction(cut); !m.Legal {
			t.Fatalf("ordinary producer output must map legal: %+v", m)
		}
	})

	t.Run("and still carries no traversal state", func(t *testing.T) {
		// The property the REFUSED arm does depend on. Both pre-traversal
		// helpers are unreachable once the loop has begun.
		if ev.HasStopPosition || ev.StoppedAtCandidate != "" || ev.StoppedAtPosition != 0 ||
			len(ev.Trace) != 0 || len(ev.Visits) != 0 || ev.CandidatesConsumed != 0 {
			t.Fatalf("a refusal decided before the traversal carries no traversal state: %+v", ev)
		}
		ev.HasStopPosition = true
		ev.StoppedAtCandidate = "GHOST"
		if m := p4offline.MapP3bAction(ev); m.Legal ||
			!containsString(m.Illegality, "UNREAD_REFUSAL_CARRIES_TRAVERSAL_STATE") {
			t.Fatalf("want UNREAD_REFUSAL_CARRIES_TRAVERSAL_STATE: %+v", m)
		}
	})
}

// refusalReasonIsInP4Vocabulary asks the map itself whether a reason is one it
// recognises, without reaching into the package: an unrecognised reason is
// named REFUSAL_REASON_FOREIGN.
func refusalReasonIsInP4Vocabulary(reason string) bool {
	ev := predictioneval.OrderedRulesEvaluation{Status: predictioneval.StatusRefused, Reason: reason}
	return !containsString(p4offline.MapP3bAction(ev).Illegality, "REFUSAL_REASON_FOREIGN")
}

// TestDecisionDoesNotBuildItsDerivationBeforeItsGates is the receipt for the
// FIFTH instance of the refusal-cost class, and for the rule's third and
// final scoping.
//
// P3bCaseResult and P2CaseResult have every field exported but their witness,
// so any caller constructs one -- and doc.go's own replay path says a stored
// result is re-evaluated rather than trusted. Decision built its derivation
// string from those fields as a CALL ARGUMENT, and Go evaluates arguments
// before the call, so it was fully materialized before decisionOf's first gate
// ran. Measured by an independent sweep: 3,686,696 bytes allocated on a 1 MiB
// ruleset id, on a call that then refuses.
//
// Neither of the rule's earlier scopings covered it: it is not the first
// statement of an exported function, and it is not a gate. decisionOf takes a
// thunk now, so the derivation is built only once the gates have passed.
//
// BOTH callers are driven here. decisionOf has exactly two in production --
// P2CaseResult.Decision and P3bCaseResult.Decision -- and an earlier draft of
// this test pinned only P3b's, which left the sibling repair free to regress
// silently. A repair that lives at three places needs a case per caller, not
// a case per repair.
func TestDecisionDoesNotBuildItsDerivationBeforeItsGates(t *testing.T) {
	_, fs := selectedFactset(t, nil, nil)
	rs := mustVerify(t, rulesetFrom(t, cfgDefaultOnly("c1", 0, 1)))
	res, err := p4offline.EvaluateP3bCase(fs, rs, synthCoords(fs, 0))
	if err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("a", 1<<20)

	for _, tc := range []struct {
		name string
		edit func(*p4offline.P3bCaseResult)
	}{
		{"an over-long ruleset id", func(r *p4offline.P3bCaseResult) { r.RulesetID = big }},
		{"an over-long run identity", func(r *p4offline.P3bCaseResult) { r.Trace.RunID = big }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := res
			tc.edit(&bad)
			// The factset side is deliberately made to disagree, so the call
			// refuses at decisionOf's binding gate -- the point being that it
			// refuses WITHOUT having built the derivation first.
			bad.FactsetDigest = strings.Repeat("ab", 32)

			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			_, derr := bad.Decision(fs)
			runtime.ReadMemStats(&after)
			allocated := after.TotalAlloc - before.TotalAlloc
			t.Logf("1 MiB supplied; the refusing call allocated %d bytes", allocated)

			if !errors.Is(derr, p4offline.ErrDecisionBinding) {
				t.Fatalf("want ErrDecisionBinding, got %v", derr)
			}
			if allocated > uint64(len(big))/2 {
				t.Fatalf("the refusing call allocated %d bytes against a %d-byte supplied field: the derivation was built before the gates",
					allocated, len(big))
			}
		})
	}

	t.Run("and the P2 caller is held to the same rule", func(t *testing.T) {
		// THE REPAIR LIVES AT THREE PLACES AND ONLY ONE OF THEM WAS PINNED.
		// decisionOf has exactly two production callers, and the table above
		// drives only P3b's. An independent lane reverted P2's thunk to an
		// eager string, left every other file at these bytes, and the whole
		// suite passed -- at 67,121,512 bytes allocated on a 64 MiB
		// Binding.Digest, for a call that returns a 210-byte refusal. That is
		// the identical fifth-instance defect on the sibling caller, reachable
		// through an exported method, with no test pressure holding the repair
		// in place. P2CaseResult has every field exported but its witness, so
		// Binding.Digest carries exactly the provenance the P3b rows do.
		p2, err := p4offline.EvaluateP2Case(fs)
		if err != nil {
			t.Fatal(err)
		}
		bad := p2
		bad.Binding.Digest = big
		bad.FactsetDigest = strings.Repeat("ab", 32)

		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		_, derr := bad.Decision(fs)
		runtime.ReadMemStats(&after)
		allocated := after.TotalAlloc - before.TotalAlloc
		t.Logf("1 MiB supplied; the refusing P2 call allocated %d bytes", allocated)

		if !errors.Is(derr, p4offline.ErrDecisionBinding) {
			t.Fatalf("want ErrDecisionBinding, got %v", derr)
		}
		if allocated > uint64(len(big))/2 {
			t.Fatalf("the refusing call allocated %d bytes against a %d-byte supplied field: the derivation was built before the gates",
				allocated, len(big))
		}
	})

	t.Run("and a decision that passes its gates still carries its derivation", func(t *testing.T) {
		// The thunk must not cost the derivation itself: a legitimate decision
		// still names the ruleset and the run it came from.
		d, err := res.Decision(fs)
		if err != nil {
			t.Fatalf("the result must bind to its own factset: %v", err)
		}
		if !strings.Contains(d.Derivation, "ruleset=") || !strings.Contains(d.Derivation, "run=") {
			t.Fatalf("the derivation must still name the ruleset and the run: %q", d.Derivation)
		}
	})
}
