package analytics

import (
	"reflect"
	"testing"
)

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
