package predictioneval_test

// THE DONOR'S OWN CONFIGURATION, DRIVEN THROUGH THE MODEL.
//
// Every other test in this suite uses a configuration written to isolate one
// decision. This one uses the donor's shipped example verbatim, because a
// hand-built config can accidentally agree with a wrong normalization while a
// real one — with four overlapping rules at 90/10/70/30 and rates of 100, 1,
// 100 and 3 percent — cannot.
//
// It also closes the loop on the raw-percentage question with the donor's own
// words. The example annotates its second rule "bets placed when odds <= 10%,
// 1% of the time" beside `attempt_rate: 1.0`. A model that read that 1.0 as a
// probability of one would participate every time on that rule.

import (
	"encoding/json"
	"math"
	"os"
	"strconv"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
)

type donorFixture struct {
	FixtureID string `json:"fixtureId"`
	Source    struct {
		Repository string `json:"repository"`
		Commit     string `json:"commit"`
		Tree       string `json:"tree"`
		Path       string `json:"path"`
		BlobSha1   string `json:"blobSha1"`
	} `json:"source"`
	Cases []struct {
		Name      string                            `json:"name"`
		RawConfig predictioneval.OrderedRulesConfig `json:"rawConfig"`
		Expect    struct {
			Detailed []struct {
				ThresholdBits     string `json:"thresholdBits"`
				AttemptRateBits   string `json:"attemptRateBits"`
				BernoulliWord     string `json:"bernoulliWord"`
				PointsPercentBits string `json:"pointsPercentBits"`
			} `json:"detailed"`
			Default struct {
				MinBits           string `json:"minBits"`
				MaxBits           string `json:"maxBits"`
				PointsPercentBits string `json:"pointsPercentBits"`
			} `json:"default"`
		} `json:"normalizedExpectations"`
	} `json:"cases"`
}

func loadDonorFixture(t *testing.T) donorFixture {
	t.Helper()
	body, err := os.ReadFile("testdata/ordered_rules/donor_example_config.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var f donorFixture
	if err := json.Unmarshal(body, &f); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if len(f.Cases) == 0 {
		t.Fatal("the fixture carries no cases; these checks would pass vacuously")
	}
	if f.Source.Commit == "" || f.Source.BlobSha1 == "" {
		t.Fatal("the fixture must name the exact donor revision and blob it was transcribed from")
	}
	return f
}

// wantBits parses a recorded 0x-prefixed hex value.
//
// The prefix and length are checked before the slice rather than after: a
// fixture edited down to an empty or unprefixed string would otherwise panic on
// the slice bounds, and an opaque runtime error tells a reader nothing about
// which fixture field is wrong.
func wantBits(t *testing.T, hex string) uint64 {
	t.Helper()
	if len(hex) < 3 || hex[:2] != "0x" {
		t.Fatalf("recorded bits %q are not a 0x-prefixed hex value; the fixture is malformed", hex)
	}
	v, err := strconv.ParseUint(hex[2:], 16, 64)
	if err != nil {
		t.Fatalf("bad recorded bits %q: %v", hex, err)
	}
	return v
}

// donorCase returns one fixture case by name, with the rule count it must carry.
//
// Indexing a trimmed fixture directly panics on the bounds instead of saying
// which case lost which rule, so the shape is asserted once here.
func donorCase(t *testing.T, f donorFixture, name string, wantRules int) int {
	t.Helper()
	for i, c := range f.Cases {
		if c.Name != name {
			continue
		}
		if len(c.RawConfig.Detailed) != wantRules || len(c.Expect.Detailed) != wantRules {
			t.Fatalf("fixture case %q carries %d raw rules and %d recorded expectations, want %d of "+
				"each; the fixture is malformed", name, len(c.RawConfig.Detailed),
				len(c.Expect.Detailed), wantRules)
		}
		return i
	}
	t.Fatalf("the fixture no longer carries a case named %q", name)
	return 0
}

// TestOrderedRulesDonorExampleConfigNormalizesAsRecorded pins the single
// division against the fixture's recorded bits.
//
// The expectations are compared as float64 BITS. A decimal comparison would
// accept a value that merely prints the same, and every threshold here — 0.9,
// 0.1, 0.7, 0.3, 0.55, 0.45 — is inexact in binary64.
func TestOrderedRulesDonorExampleConfigNormalizesAsRecorded(t *testing.T) {
	f := loadDonorFixture(t)
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			if len(c.RawConfig.Detailed) != len(c.Expect.Detailed) {
				t.Fatalf("%d rules but %d recorded expectations", len(c.RawConfig.Detailed),
					len(c.Expect.Detailed))
			}
			for i, r := range c.RawConfig.Detailed {
				want := c.Expect.Detailed[i]
				if got := math.Float64bits(r.RawThresholdPercent / 100.0); got != wantBits(t, want.ThresholdBits) {
					t.Errorf("rule %d threshold: raw %v normalizes to %#016x, fixture records %s",
						i, r.RawThresholdPercent, got, want.ThresholdBits)
				}
				if got := math.Float64bits(r.RawAttemptRatePercent / 100.0); got != wantBits(t, want.AttemptRateBits) {
					t.Errorf("rule %d attempt rate: raw %v normalizes to %#016x, fixture records %s",
						i, r.RawAttemptRatePercent, got, want.AttemptRateBits)
				}
				if got := math.Float64bits(r.Points.RawPercent / 100.0); got != wantBits(t, want.PointsPercentBits) {
					t.Errorf("rule %d points percent: raw %v normalizes to %#016x, fixture records %s",
						i, r.Points.RawPercent, got, want.PointsPercentBits)
				}
			}
			if got := math.Float64bits(c.RawConfig.Default.RawMinPercent / 100.0); got !=
				wantBits(t, c.Expect.Default.MinBits) {
				t.Errorf("default min normalizes to %#016x, fixture records %s", got, c.Expect.Default.MinBits)
			}
			if got := math.Float64bits(c.RawConfig.Default.RawMaxPercent / 100.0); got !=
				wantBits(t, c.Expect.Default.MaxBits) {
				t.Errorf("default max normalizes to %#016x, fixture records %s", got, c.Expect.Default.MaxBits)
			}
			// The default's STAKE percentage, not only its bounds. It was
			// recorded for both cases and compared for neither, so a wrong
			// value could sit in the fixture indefinitely — and this file is
			// the only thing that says the recorded column is the correct
			// single division.
			if got := math.Float64bits(c.RawConfig.Default.Points.RawPercent / 100.0); got !=
				wantBits(t, c.Expect.Default.PointsPercentBits) {
				t.Errorf("default points percent normalizes to %#016x, fixture records %s",
					got, c.Expect.Default.PointsPercentBits)
			}
		})
	}
}

// TestOrderedRulesDonorExampleConfigDrivesTheMechanism runs the donor's real
// four-rule configuration over the pool [A:1, B:9].
//
// The three cases below walk deeper into the same config as the entropy turns
// against them, and between them exercise the whole mechanism at once:
//
//  1. rule 1 (Le 10 at ONE percent) admits outcome A;
//  2. rule 1's draw fails, and rule 3 (Le 30 at THREE percent) admits the SAME
//     outcome — the fall-through, at two different real rates;
//  3. both fail, outcome A's default misses, and outcome B is reached, where
//     rule 0 (Ge 90) does NOT match its 1/(10/9) share and rule 2 (Ge 70) does,
//     at one hundred percent, consuming no further word.
//
// Case 3 is the donor's own configuration reproducing the [9,1] falsifier: if
// the pool share were computed as a direct ratio, rule 0 would match and the
// stake would be sized from a different rule entirely.
func TestOrderedRulesDonorExampleConfigDrivesTheMechanism(t *testing.T) {
	f := loadDonorFixture(t)
	i := donorCase(t, f, "streamer_a", 4)
	cfg := f.Cases[i].RawConfig
	rule1Word := wantBits(t, f.Cases[i].Expect.Detailed[1].BernoulliWord)
	rule3Word := wantBits(t, f.Cases[i].Expect.Detailed[3].BernoulliWord)

	cs := []predictioneval.OrderedRulesCandidate{
		orCandidate("c1", 10, orKnownBalance(100000), orOutcome("A", 1), orOutcome("B", 9)),
	}

	t.Run("rule 1 admits at one percent", func(t *testing.T) {
		ev := orEval(t, cs, cfg, rule1Word-1)
		sel := orMustAdmit(t, ev)
		switch {
		case sel.OutcomeIdentity != "A" || sel.RuleIndex != 1:
			t.Fatalf("selected %q by rule %d, want \"A\" by rule 1", sel.OutcomeIdentity, sel.RuleIndex)
		case ev.RawWordsConsumed != 1:
			t.Fatalf("consumed %d words, want 1", ev.RawWordsConsumed)
		// 1% of 100000 is 1000.0, and the cap is also 1000, so the cap wins.
		case ev.Stake.Presence != predictioneval.SuppliedKnown || ev.Stake.Value != 1000:
			t.Fatalf("stake = %v(%d), want KNOWN(1000)", ev.Stake.Presence, ev.Stake.Value)
		}
	})

	t.Run("rule 1 fails and rule 3 admits the same outcome at three percent", func(t *testing.T) {
		ev := orEval(t, cs, cfg, rule1Word, rule3Word-1)
		sel := orMustAdmit(t, ev)
		switch {
		case sel.OutcomeIdentity != "A" || sel.RuleIndex != 3:
			t.Fatalf("selected %q by rule %d, want \"A\" by rule 3 — the fall-through must reach the "+
				"later overlapping rule", sel.OutcomeIdentity, sel.RuleIndex)
		case ev.RawWordsConsumed != 2:
			t.Fatalf("consumed %d words, want 2: rules 0 and 2 are Ge rules that never match a share of "+
				"0.1, so only rules 1 and 3 draw", ev.RawWordsConsumed)
		// 5% of 100000 is 5000.0 and the cap is 5000, so the cap wins again.
		case ev.Stake.Value != 5000:
			t.Fatalf("stake = %d, want 5000", ev.Stake.Value)
		}
	})

	t.Run("outcome B is reached and rule 0 does not match its reciprocal share", func(t *testing.T) {
		ev := orEval(t, cs, cfg, rule1Word, rule3Word)
		sel := orMustAdmit(t, ev)
		switch {
		case sel.OutcomeIdentity != "B":
			t.Fatalf("selected %q, want \"B\": both of outcome A's rules failed and its default missed",
				sel.OutcomeIdentity)
		case sel.RuleIndex != 2:
			t.Fatalf("selected by rule %d, want rule 2 (Ge 70). Rule 0 (Ge 90) matching here would mean "+
				"the pool share was computed as a direct ratio rather than the donor's reciprocal.",
				sel.RuleIndex)
		case ev.RawWordsConsumed != 2:
			t.Fatalf("consumed %d words, want 2: rule 2 participates at one hundred percent and draws "+
				"nothing", ev.RawWordsConsumed)
		}
		// float64 VARIABLES. As an untyped constant expression Go evaluates
		// 1/(10/9) exactly and rounds once, producing 0x3feccccccccccccd — the
		// SHORTCUT's value. Writing the expectation that way would assert the
		// very collapse this case exists to detect.
		var total, points float64 = 10, 9
		if got := uint64(sel.ShareBits); got != math.Float64bits(1.0/(total/points)) {
			t.Fatalf("outcome B's recorded share is %#016x, want the donor's reciprocal %#016x",
				got, math.Float64bits(1.0/(total/points)))
		}
	})
}

// TestOrderedRulesDonorPresetSmallKeepsItsExplicitZeros pins the donor's own
// counterexample to substituting the 40/60 serde defaults.
//
// The shipped `small` preset writes every default field as an explicit zero. It
// therefore admits ONLY an outcome holding no points at all, and stakes nothing
// when it does. A model that filled in 40 and 60 would give this preset a
// betting range its author never wrote.
func TestOrderedRulesDonorPresetSmallKeepsItsExplicitZeros(t *testing.T) {
	f := loadDonorFixture(t)
	// Through donorCase rather than a hand-rolled scan: the scan kept the LAST
	// match rather than the first, and probed presence by an empty ConfigID, so
	// a duplicated case would be silently resolved and a case present but blank
	// would be reported as absent. The rule count asserted here is zero, which
	// is the preset's own defining shape — it ships no detailed rules at all,
	// which is half of why it admits only a zero-share outcome.
	cfg := f.Cases[donorCase(t, f, "preset_small", 0)].RawConfig

	ordinary := []predictioneval.OrderedRulesCandidate{
		orCandidate("c1", 10, orKnownBalance(100000), orOutcome("A", 4), orOutcome("B", 6)),
	}
	if ev := orEval(t, ordinary, cfg); ev.Status != predictioneval.StatusNoAttemptInSuppliedPrefix {
		t.Fatalf("an all-zero default admits neither a 0.4 nor a 0.6 share: got %q selecting %+v",
			ev.Status, ev.Selected)
	}

	empty := []predictioneval.OrderedRulesCandidate{
		orCandidate("c1", 10, orKnownBalance(100000), orOutcome("A", 0), orOutcome("B", 10)),
	}
	ev := orEval(t, empty, cfg)
	sel := orMustAdmit(t, ev)
	switch {
	case sel.OutcomeIdentity != "A":
		t.Fatalf("only the zero-share outcome may match [0,0]: selected %q", sel.OutcomeIdentity)
	case ev.Status != predictioneval.StatusWouldAttempt:
		t.Fatalf("status = %q, want WOULD_ATTEMPT", ev.Status)
	case ev.Stake.Presence != predictioneval.SuppliedKnown || ev.Stake.Value != 0:
		t.Fatalf("stake = %v(%d), want KNOWN(0): a zero percentage stakes zero and stays an attempt",
			ev.Stake.Presence, ev.Stake.Value)
	}
}
