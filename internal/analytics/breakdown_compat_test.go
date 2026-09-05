package analytics

import (
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The compatibility contract of the public `breakdown` field is "whatever the
// first Statistics release computed". The oracle below is that release's
// internal/analytics/breakdown.go — base commit
// dc5566049f1de1909d66c0f190338d54af863402 — copied verbatim, reason table
// included, so it shares no code with production and cannot drift with it.

func baseCanonicalPointReason(raw string) string {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "WATCH STREAK", "WATCH_STREAK":
		return "WATCH_STREAK"
	case "WATCH":
		return "WATCH"
	case "CLAIM":
		return "CLAIM"
	case "RAID":
		return "RAID"
	case "PREDICTION":
		return "PREDICTION"
	default:
		return "OTHER"
	}
}

func baseBreakdownFromSamples(samples []PointSample) []ReasonShare {
	if len(samples) < 2 {
		return nil
	}

	gained := make(map[string]*ReasonShare)
	for i := 1; i < len(samples); i++ {
		diff := samples[i].Balance - samples[i-1].Balance
		if diff <= 0 {
			continue
		}
		reason := baseCanonicalPointReason(samples[i].Reason)
		share, ok := gained[reason]
		if !ok {
			share = &ReasonShare{Reason: reason}
			gained[reason] = share
		}
		share.Gained += diff
		share.Count++
	}

	out := make([]ReasonShare, 0, len(gained))
	for _, share := range gained {
		out = append(out, *share)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Gained != out[j].Gained {
			return out[i].Gained > out[j].Gained
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}

// productionTimeline is the talkto_megoose balance timeline behind PR #303:
// three accepted 450-point streak grants recorded as +462/+450/+462 balance
// deltas, plus the surrounding WATCH snapshots. The first release reported
// WATCH_STREAK 1374 over 3 for it; the ledger reports 1350 over 3.
var productionTimeline = []PointSample{
	{Balance: 11310, Reason: "WATCH"},
	{Balance: 11772, Reason: "WATCH STREAK"},
	{Balance: 11322, Reason: "WATCH"},
	{Balance: 11784, Reason: "WATCH"},
	{Balance: 14338, Reason: "WATCH"},
	{Balance: 14788, Reason: "WATCH STREAK"},
	{Balance: 17690, Reason: "WATCH"},
	{Balance: 18152, Reason: "WATCH STREAK"},
	{Balance: 17702, Reason: "WATCH"},
	{Balance: 18164, Reason: "WATCH"},
}

// TestBreakdownFromSamplesIsTheBaseAlgorithm proves the compatibility
// attribution behaviourally identical to the base commit — reason, gained,
// count and order — on hand-picked timelines and on a deterministic sweep of
// random ones, and shows it is a different function from the legacy
// estimator (so no mutant can satisfy both contracts with one algorithm).
func TestBreakdownFromSamplesIsTheBaseAlgorithm(t *testing.T) {
	mixedWithSpend := []PointSample{
		{Balance: 1000, Reason: "WATCH"},
		{Balance: 1012, Reason: "WATCH"},
		{Balance: 1462, Reason: "WATCH STREAK", Exact: true},
		{Balance: 1474, Reason: "WATCH", Exact: true},
		{Balance: 1424, Reason: "Spent"},
		{Balance: 1436, Reason: "WATCH"},
	}
	staleSpend := []PointSample{
		{Balance: 1012, Reason: "WATCH", Exact: true},
		{Balance: 900, Reason: "Spent"},
		{Balance: 1300, Reason: "Spent"}, // stale previous snapshot: +400 into a spend
		{Balance: 1312, Reason: "WATCH", Exact: true},
	}
	cases := []struct {
		name     string
		samples  []PointSample
		want     []ReasonShare // worked by hand from the base algorithm
		estimate []ReasonShare // what the legacy estimator says instead
	}{
		{"nil", nil, nil, nil},
		{"one sample is a baseline", []PointSample{{Balance: 1450, Reason: "WATCH STREAK"}}, nil, nil},
		{"production repro", productionTimeline,
			[]ReasonShare{{Reason: "WATCH", Gained: 6380, Count: 4}, {Reason: "WATCH_STREAK", Gained: 1374, Count: 3}},
			[]ReasonShare{{Reason: "WATCH", Gained: 6380, Count: 4}, {Reason: "WATCH_STREAK", Gained: 1374, Count: 3}}},
		{"mixed window with a spend", mixedWithSpend,
			[]ReasonShare{{Reason: "WATCH_STREAK", Gained: 450, Count: 1}, {Reason: "WATCH", Gained: 36, Count: 3}},
			[]ReasonShare{{Reason: "WATCH", Gained: 24, Count: 2}}},
		{"stale spent snapshot", staleSpend,
			[]ReasonShare{{Reason: "OTHER", Gained: 400, Count: 1}, {Reason: "WATCH", Gained: 12, Count: 1}},
			nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BreakdownFromSamples(tc.samples)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("BreakdownFromSamples() = %+v, want the base figure %+v", got, tc.want)
			}
			if oracle := baseBreakdownFromSamples(tc.samples); !reflect.DeepEqual(got, oracle) {
				t.Fatalf("BreakdownFromSamples() = %+v, base oracle = %+v", got, oracle)
			}
			if est := EstimateLegacyBreakdown(tc.samples).Breakdown; !reflect.DeepEqual(est, tc.estimate) {
				t.Fatalf("EstimateLegacyBreakdown() = %+v, want %+v", est, tc.estimate)
			}
		})
	}

	// Deterministic sweep: random walks with every reason form the timeline
	// can carry, exact flags and spends included. Identity with the oracle
	// on every one; divergence from the estimator on a good share of them,
	// so the identity is not the trivial one.
	rng := rand.New(rand.NewSource(20260905))
	reasons := []string{"WATCH", "WATCH STREAK", "WATCH_STREAK", "CLAIM", "RAID", "PREDICTION", "Spent", "spent", "WEEKLY REWARDS", "", "unknown", " watch "}
	divergent := 0
	const sweeps = 500
	for i := 0; i < sweeps; i++ {
		n := rng.Intn(41)
		samples := make([]PointSample, n)
		balance := rng.Intn(5000)
		for j := range samples {
			balance += rng.Intn(1201) - 600
			samples[j] = PointSample{T: int64(j), Balance: balance, Reason: reasons[rng.Intn(len(reasons))], Exact: rng.Intn(3) == 0}
		}
		got, oracle := BreakdownFromSamples(samples), baseBreakdownFromSamples(samples)
		if !reflect.DeepEqual(got, oracle) {
			t.Fatalf("sweep %d: BreakdownFromSamples() = %+v, base oracle = %+v, samples %+v", i, got, oracle, samples)
		}
		if !reflect.DeepEqual(got, EstimateLegacyBreakdown(samples).Breakdown) {
			divergent++
		}
	}
	if divergent < sweeps/2 {
		t.Fatalf("the compatibility attribution agreed with the legacy estimator on %d of %d random timelines; the sweep is not exercising the difference", sweeps-divergent, sweeps)
	}
}
