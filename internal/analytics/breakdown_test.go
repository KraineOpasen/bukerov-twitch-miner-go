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
		"PREDICTION":     "OTHER",
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
				{Balance: 1510, Reason: "PREDICTION"},     // deliberately OTHER
			},
			want: []ReasonShare{
				{Reason: "WATCH_STREAK", Gained: 450, Count: 1},
				{Reason: "OTHER", Gained: 60, Count: 2},
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
