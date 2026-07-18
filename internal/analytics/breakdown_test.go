package analytics

import (
	"reflect"
	"testing"
)

// TestCanonicalPointReason pins the exact canonicalization table against the
// event_type values observed in a production database (Service.RecordPoints
// replaces underscores with spaces before persisting, so the stored streak
// reason is "WATCH STREAK"). The mapping must be exact — in particular,
// "WATCH STREAK" must never collapse into WATCH via any prefix/substring
// shortcut.
func TestCanonicalPointReason(t *testing.T) {
	cases := map[string]string{
		// Real production rows (space forms come from RecordPoints).
		"WATCH":          "WATCH",
		"CLAIM":          "CLAIM",
		"RAID":           "RAID",
		"WATCH STREAK":   "WATCH_STREAK",
		"WEEKLY REWARDS": "OTHER",
		"Spent":          "OTHER",
		// Prediction winnings are a first-class earnings category, not OTHER.
		"PREDICTION": "PREDICTION",
		"prediction": "PREDICTION", // case-folded key
		// Compatibility/robustness forms.
		"WATCH_STREAK":     "WATCH_STREAK",
		"  watch streak  ": "WATCH_STREAK", // trim + case apply to the key only
		"watch":            "WATCH",
		"":                 "OTHER",
		"SOMETHING_NEW":    "OTHER",
	}
	for raw, want := range cases {
		if got := canonicalPointReason(raw); got != want {
			t.Errorf("canonicalPointReason(%q) = %q, want %q", raw, got, want)
		}
	}

	// Anti-collision guard: the streak reason must never be absorbed by
	// WATCH (the trap a HasPrefix/Contains implementation would fall into).
	if got := canonicalPointReason("WATCH STREAK"); got == "WATCH" {
		t.Fatalf(`canonicalPointReason("WATCH STREAK") = WATCH — streak swallowed by WATCH`)
	}
}

func TestBreakdownFromSamples(t *testing.T) {
	cases := []struct {
		name    string
		samples []PointSample
		want    []ReasonShare
	}{
		{
			name:    "empty series",
			samples: nil,
			want:    nil,
		},
		{
			name:    "single sample is baseline only",
			samples: []PointSample{{Balance: 1000, Reason: "WATCH"}},
			want:    nil,
		},
		{
			name: "mixed reasons aggregate and sort by gained",
			samples: []PointSample{
				{Balance: 1000, Reason: "WATCH"}, // baseline
				{Balance: 1010, Reason: "WATCH"},
				{Balance: 1510, Reason: "CLAIM"},
				{Balance: 1760, Reason: "RAID"},
				{Balance: 1770, Reason: "WATCH"},
			},
			want: []ReasonShare{
				{Reason: "CLAIM", Gained: 500, Count: 1},
				{Reason: "RAID", Gained: 250, Count: 1},
				{Reason: "WATCH", Gained: 20, Count: 2},
			},
		},
		{
			name: "spent and flat deltas are ignored",
			samples: []PointSample{
				{Balance: 1000, Reason: "WATCH"},
				{Balance: 500, Reason: "Spent"}, // bet placed: negative
				{Balance: 500, Reason: "WATCH"}, // flat
				{Balance: 950, Reason: "WATCH_STREAK"},
			},
			want: []ReasonShare{
				{Reason: "WATCH_STREAK", Gained: 450, Count: 1},
			},
		},
		{
			name: "reasonless gain buckets as OTHER",
			samples: []PointSample{
				{Balance: 100},
				{Balance: 150},
				{Balance: 250, Reason: "WATCH"},
			},
			want: []ReasonShare{
				{Reason: "WATCH", Gained: 100, Count: 1},
				{Reason: "OTHER", Gained: 50, Count: 1},
			},
		},
		{
			name: "equal gains tie-break by reason for determinism",
			samples: []PointSample{
				{Balance: 0},
				{Balance: 10, Reason: "RAID"},
				{Balance: 20, Reason: "CLAIM"},
			},
			want: []ReasonShare{
				{Reason: "CLAIM", Gained: 10, Count: 1},
				{Reason: "RAID", Gained: 10, Count: 1},
			},
		},
		{
			name: "production reason strings canonicalize",
			samples: []PointSample{
				{Balance: 1000, Reason: "WATCH"},          // baseline
				{Balance: 1450, Reason: "WATCH STREAK"},   // prod space form
				{Balance: 1500, Reason: "WEEKLY REWARDS"}, // deliberately OTHER
				{Balance: 1510, Reason: "PREDICTION"},     // now its own category
			},
			want: []ReasonShare{
				{Reason: "WATCH_STREAK", Gained: 450, Count: 1},
				{Reason: "OTHER", Gained: 50, Count: 1},
				{Reason: "PREDICTION", Gained: 10, Count: 1},
			},
		},
		{
			// A prediction LOSS is a non-positive delta (points-spent at bet
			// time, no credit on loss), so it must never surface as a positive
			// PREDICTION slice; only the win credit does.
			name: "prediction win is categorized, loss is not a positive slice",
			samples: []PointSample{
				{Balance: 1000, Reason: "WATCH"},      // baseline
				{Balance: 800, Reason: "Spent"},       // bet placed: -200
				{Balance: 800, Reason: "PREDICTION"},  // lost round: flat/no credit
				{Balance: 1300, Reason: "PREDICTION"}, // won round: +500 credit
			},
			want: []ReasonShare{
				{Reason: "PREDICTION", Gained: 500, Count: 1},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BreakdownFromSamples(tc.samples)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("BreakdownFromSamples() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestBreakdownFirstSampleIsBaseline pins the documented window-edge
// limitation: the first in-window sample only establishes the baseline — its
// own delta cannot be known without a pre-window sample, which is
// deliberately not fetched (deferred data-quality work). A lone streak event
// at the window edge therefore yields no breakdown rather than a guessed one.
func TestBreakdownFirstSampleIsBaseline(t *testing.T) {
	// Single in-window sample: its own event is unattributable.
	if got := BreakdownFromSamples([]PointSample{{Balance: 1450, Reason: "WATCH STREAK"}}); got != nil {
		t.Errorf("single-sample window should yield nil breakdown, got %+v", got)
	}

	// With a baseline present, only deltas from the second sample onward are
	// attributed; the first sample's own event never contributes.
	got := BreakdownFromSamples([]PointSample{
		{Balance: 1000, Reason: "WATCH"},        // baseline: its event is not counted
		{Balance: 1450, Reason: "WATCH STREAK"}, // attributed
	})
	want := []ReasonShare{{Reason: "WATCH_STREAK", Gained: 450, Count: 1}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("breakdown = %+v, want %+v (baseline sample must not contribute)", got, want)
	}
}

// TestBreakdownWatchStreakSums pins the four watch-streak grant sizes Twitch
// awards (300/350/400/450) and proves they aggregate into a single WATCH_STREAK
// category regardless of the on-disk spelling ("WATCH STREAK" from
// Service.RecordPoints' underscore→space rewrite, or the underscore form from
// legacy rows). Each grant is counted exactly once (no double-counting), and
// nothing bleeds into WATCH or CLAIM.
func TestBreakdownWatchStreakSums(t *testing.T) {
	samples := []PointSample{
		{Balance: 10000, Reason: "WATCH"},           // baseline
		{Balance: 10010, Reason: "WATCH"},           // +10 passive watch
		{Balance: 10310, Reason: "WATCH STREAK"},    // +300 (space form)
		{Balance: 10660, Reason: "WATCH_STREAK"},    // +350 (legacy underscore row)
		{Balance: 11060, Reason: "WATCH STREAK"},    // +400
		{Balance: 11510, Reason: "  watch streak "}, // +450 (untrimmed/lowercased)
		{Balance: 12010, Reason: "CLAIM"},           // +500 bonus claim
	}
	got := BreakdownFromSamples(samples)

	byReason := make(map[string]ReasonShare, len(got))
	for _, s := range got {
		byReason[s.Reason] = s
	}

	streak, ok := byReason["WATCH_STREAK"]
	if !ok {
		t.Fatalf("expected a WATCH_STREAK category, got %+v", got)
	}
	// 300+350+400+450, each grant counted exactly once.
	if streak.Gained != 1500 {
		t.Errorf("WATCH_STREAK gained = %d, want 1500 (300+350+400+450)", streak.Gained)
	}
	if streak.Count != 4 {
		t.Errorf("WATCH_STREAK count = %d, want 4 (no double-counting, no merges)", streak.Count)
	}

	if w := byReason["WATCH"]; w.Gained != 10 || w.Count != 1 {
		t.Errorf("WATCH share = %+v, want gained 10 count 1 (streaks must not bleed into WATCH)", w)
	}
	if c := byReason["CLAIM"]; c.Gained != 500 || c.Count != 1 {
		t.Errorf("CLAIM share = %+v, want gained 500 count 1", c)
	}
	if _, isOther := byReason["OTHER"]; isOther {
		t.Errorf("no OTHER slice expected for these known reasons, got %+v", got)
	}
}
