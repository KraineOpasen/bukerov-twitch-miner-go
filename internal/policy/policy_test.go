package policy

import (
	"strings"
	"testing"
	"time"
)

var base = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// chain builds a simple drop chain from (required, watched) pairs.
func chain(pairs ...[2]int) []DropStep {
	steps := make([]DropStep, 0, len(pairs))
	for _, p := range pairs {
		steps = append(steps, DropStep{MinutesRequired: p[0], CurrentMinutesWatched: p[1]})
	}
	return steps
}

func factorPoints(d Decision, substr string) (int, bool) {
	for _, f := range d.Factors {
		if strings.Contains(f.Label, substr) {
			return f.Points, true
		}
	}
	return 0, false
}

// --- feasibility ---

func TestFeasibilityStatuses(t *testing.T) {
	cases := []struct {
		name string
		in   CampaignInput
		want FeasStatus
	}{
		{
			name: "safe: lots of time, short chain",
			in:   CampaignInput{EndAt: base.Add(48 * time.Hour), Drops: chain([2]int{60, 10}, [2]int{120, 10})},
			want: StatusSafe,
		},
		{
			name: "at risk: can finish all but margin under 30m",
			// completeAll=100 remaining, avail = 125 - reserve(10) = 115; slack 15 < 30
			in:   CampaignInput{EndAt: base.Add(125 * time.Minute), Drops: chain([2]int{100, 0})},
			want: StatusAtRisk,
		},
		{
			name: "next reward only: can reach 60 but not 600",
			in:   CampaignInput{EndAt: base.Add(90 * time.Minute), Drops: chain([2]int{60, 0}, [2]int{600, 0})},
			want: StatusNextRewardOnly,
		},
		{
			name: "impossible: not even the next reward",
			in:   CampaignInput{EndAt: base.Add(30 * time.Minute), Drops: chain([2]int{120, 0})},
			want: StatusImpossible,
		},
		{
			name: "impossible: already ended",
			in:   CampaignInput{EndAt: base.Add(-time.Minute), Drops: chain([2]int{60, 0})},
			want: StatusImpossible,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ComputeFeasibility(tc.in, base).Status; got != tc.want {
				t.Fatalf("status = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestFeasibilityNextRewardOnlyRuleShrinksGoal(t *testing.T) {
	// 120 min available (110 after reserve): reaches the 60-min next reward with
	// a comfortable 50-min margin, but nowhere near the 600-min chain.
	in := CampaignInput{EndAt: base.Add(120 * time.Minute), Drops: chain([2]int{60, 0}, [2]int{600, 0})}
	if got := ComputeFeasibility(in, base).Status; got != StatusNextRewardOnly {
		t.Fatalf("baseline should be NEXT_REWARD_ONLY, got %s", got)
	}
	in.NextRewardOnly = true
	// Goal shrinks to the next reward (60), reachable with margin → SAFE.
	if got := ComputeFeasibility(in, base).Status; got != StatusSafe {
		t.Fatalf("with NextRewardOnly the reduced goal should read SAFE, got %s", got)
	}
}

func TestFeasibilityMinutes(t *testing.T) {
	in := CampaignInput{EndAt: base.Add(10 * time.Hour), Drops: chain([2]int{60, 22}, [2]int{300, 22})}
	f := ComputeFeasibility(in, base)
	if f.MinutesToNextReward != 38 { // 60-22
		t.Errorf("next reward remaining = %d, want 38", f.MinutesToNextReward)
	}
	if f.MinutesToCompleteAll != 278 { // 300-22 (furthest milestone)
		t.Errorf("complete-all remaining = %d, want 278", f.MinutesToCompleteAll)
	}
}

// --- SMART breakdown: reproduce the spec example exactly ---

// TestSmartBreakdownMatchesSpecExample builds the exact scenario from the
// master plan and asserts the breakdown and the total of 220:
//
//	+100 restricted campaign
//	+80  ends in less than 6 hours
//	+60  next reward requires 22 minutes
//	+30  only one eligible live channel
//	-50  unstable channel
//	Total: 220
func TestSmartBreakdownMatchesSpecExample(t *testing.T) {
	in := CampaignInput{
		CampaignID:           "c1",
		Restricted:           true,
		EndAt:                base.Add(5 * time.Hour), // < 6h
		Drops:                chain([2]int{22, 0}),    // next reward in 22 min
		EligibleLiveChannels: 1,
		ChannelStability:     0.0, // fully unstable
		StabilitySamples:     20,  // above the gate, so the penalty applies
	}
	d := Decide(ModeSmart, in, base)

	if d.Total != 220 {
		t.Fatalf("total = %d, want 220\nbreakdown:\n%s", d.Total, d.Breakdown())
	}
	checks := []struct {
		substr string
		points int
	}{
		{"channel-restricted campaign", 100},
		{"ends in under 6h", 80},
		{"next reward in 22 min", 60},
		{"only one eligible live channel", 30},
		{"unstable channel", -50},
	}
	for _, c := range checks {
		if pts, ok := factorPoints(d, c.substr); !ok || pts != c.points {
			t.Errorf("factor %q: got (%d, present=%v), want %d", c.substr, pts, ok, c.points)
		}
	}
}

// --- channel-stability sample-size gate (cold-start guard) ---

func TestStabilityInsufficientDataIsNeutralNotExtreme(t *testing.T) {
	// A channel with a terrible-looking 0.0 stability but only 2 observations
	// must NOT incur the -50 penalty; the factor is present but neutral and
	// labeled, so a 1-2 sample window never masquerades as a confident extreme.
	in := CampaignInput{
		CampaignID: "c1", Restricted: true, EndAt: base.Add(48 * time.Hour),
		Drops:            chain([2]int{200, 0}),    // >120 min → no reward-closeness bonus
		ChannelStability: 0.0, StabilitySamples: 2, // below minStabilitySamples
	}
	d := Decide(ModeSmart, in, base)

	if _, ok := factorPoints(d, "unstable channel"); ok {
		t.Fatal("insufficient-sample stability must not apply the instability penalty")
	}
	pts, ok := factorPoints(d, "insufficient data")
	if !ok {
		t.Fatal("expected an explicit 'insufficient data' stability factor")
	}
	if pts != 0 {
		t.Fatalf("insufficient-data factor must be neutral, got %d points", pts)
	}
	// Only the restricted bonus counts toward the total.
	if d.Total != smartRestricted {
		t.Fatalf("total = %d, want %d (stability must contribute nothing)", d.Total, smartRestricted)
	}
}

func TestStabilityPenaltyScalesOnceEnoughSamples(t *testing.T) {
	mk := func(stability float64, samples int) Decision {
		return Decide(ModeSmart, CampaignInput{
			CampaignID: "c", EndAt: base.Add(48 * time.Hour), Drops: chain([2]int{60, 0}),
			ChannelStability: stability, StabilitySamples: samples,
		}, base)
	}
	// Exactly at the threshold, half-stable → -25.
	if pts, ok := factorPoints(mk(0.5, minStabilitySamples), "unstable channel"); !ok || pts != -25 {
		t.Errorf("half-stable penalty = %d (present=%v), want -25", pts, ok)
	}
	// Perfectly stable → no penalty factor at all.
	if _, ok := factorPoints(mk(1.0, 50), "unstable channel"); ok {
		t.Error("a fully stable channel must incur no penalty")
	}
}

// --- per-drop rules ---

func TestSkipRuleExcludes(t *testing.T) {
	d := Decide(ModeSmart, CampaignInput{CampaignID: "c", Skip: true, EndAt: base.Add(48 * time.Hour), Drops: chain([2]int{60, 0})}, base)
	if !d.Excluded || !strings.Contains(d.ExcludeReason, "Skip") {
		t.Fatalf("Skip must exclude the campaign, got %+v", d)
	}
}

func TestHighPriorityFloatsToTopInEveryMode(t *testing.T) {
	normal := CampaignInput{CampaignID: "normal", EndAt: base.Add(1 * time.Hour), Drops: chain([2]int{30, 0}), GameOrderIndex: 0, EligibleLiveChannels: 1}
	hp := CampaignInput{CampaignID: "hp", HighPriority: true, EndAt: base.Add(48 * time.Hour), Drops: chain([2]int{120, 0}), GameOrderIndex: 9, EligibleLiveChannels: 9}
	for _, mode := range []Mode{ModeGameOrder, ModeEndingSoonest, ModeClosestToReward, ModeLowAvailability, ModeSmart} {
		ranked := Rank(mode, []CampaignInput{normal, hp}, base)
		if ranked[0].CampaignID != "hp" {
			t.Errorf("mode %s: high-priority campaign must rank first, got %s", mode, ranked[0].CampaignID)
		}
	}
}

func TestAlwaysFinishStartedLabelsAndScores(t *testing.T) {
	in := CampaignInput{CampaignID: "c", Started: true, AlwaysFinishStarted: true, EndAt: base.Add(48 * time.Hour), Drops: chain([2]int{60, 30})}
	d := Decide(ModeSmart, in, base)
	if pts, ok := factorPoints(d, "finish-started rule"); !ok || pts != smartStartedBonus {
		t.Fatalf("started+finish rule factor = %d (present=%v), want %d", pts, ok, smartStartedBonus)
	}
}

// --- modes / ranking ---

func TestModeOrderings(t *testing.T) {
	// Three campaigns with distinct game order, end time, next-reward distance,
	// and eligible-channel counts, so each mode's ordering is unambiguous.
	a := CampaignInput{CampaignID: "A", Game: "GA", GameOrderIndex: 0, EndAt: base.Add(50 * time.Hour), Drops: chain([2]int{100, 10}), EligibleLiveChannels: 5}
	b := CampaignInput{CampaignID: "B", Game: "GB", GameOrderIndex: 1, EndAt: base.Add(3 * time.Hour), Drops: chain([2]int{100, 95}), EligibleLiveChannels: 1}
	c := CampaignInput{CampaignID: "C", Game: "GC", GameOrderIndex: 2, EndAt: base.Add(20 * time.Hour), Drops: chain([2]int{100, 60}), EligibleLiveChannels: 3}
	in := []CampaignInput{c, b, a} // deliberately unsorted

	order := func(mode Mode) []string {
		ds := Rank(mode, in, base)
		ids := make([]string, len(ds))
		for i, d := range ds {
			ids[i] = d.CampaignID
		}
		return ids
	}

	if got := order(ModeGameOrder); got[0] != "A" || got[1] != "B" || got[2] != "C" {
		t.Errorf("GAME_ORDER = %v, want [A B C]", got)
	}
	if got := order(ModeEndingSoonest); got[0] != "B" { // ends in 3h
		t.Errorf("ENDING_SOONEST first = %s, want B", got[0])
	}
	if got := order(ModeClosestToReward); got[0] != "B" { // 5 min to next reward
		t.Errorf("CLOSEST_TO_REWARD first = %s, want B", got[0])
	}
	if got := order(ModeLowAvailability); got[0] != "B" { // only 1 eligible channel
		t.Errorf("LOW_AVAILABILITY first = %s, want B", got[0])
	}
}

// TestGameOrderBitIdenticalToConfiguredOrder is the backward-compat guard: the
// default mode must order campaigns purely by the configured game index, so
// enabling the engine changes nothing for existing users.
func TestGameOrderBitIdenticalToConfiguredOrder(t *testing.T) {
	inputs := []CampaignInput{
		{CampaignID: "z", GameOrderIndex: 3, EndAt: base.Add(1 * time.Hour), Drops: chain([2]int{10, 0})},
		{CampaignID: "y", GameOrderIndex: 1, EndAt: base.Add(2 * time.Hour), Drops: chain([2]int{10, 0})},
		{CampaignID: "x", GameOrderIndex: 0, EndAt: base.Add(99 * time.Hour), Drops: chain([2]int{10, 0})},
		{CampaignID: "w", GameOrderIndex: -1, EndAt: base.Add(1 * time.Hour), Drops: chain([2]int{10, 0})}, // unconfigured → last
	}
	ds := Rank(ModeGameOrder, inputs, base)
	want := []string{"x", "y", "z", "w"}
	for i, id := range want {
		if ds[i].CampaignID != id {
			t.Fatalf("GAME_ORDER position %d = %s, want %s (full: %v)", i, ds[i].CampaignID, id, ids(ds))
		}
	}
}

func TestRankExcludesImpossibleAndSkippedLast(t *testing.T) {
	good := CampaignInput{CampaignID: "good", GameOrderIndex: 5, EndAt: base.Add(48 * time.Hour), Drops: chain([2]int{60, 0})}
	impossible := CampaignInput{CampaignID: "imp", GameOrderIndex: 0, EndAt: base.Add(10 * time.Minute), Drops: chain([2]int{120, 0})}
	skipped := CampaignInput{CampaignID: "skip", GameOrderIndex: 1, Skip: true, EndAt: base.Add(48 * time.Hour), Drops: chain([2]int{60, 0})}

	ds := Rank(ModeGameOrder, []CampaignInput{impossible, skipped, good}, base)
	if ds[0].CampaignID != "good" {
		t.Fatalf("a trackable campaign must outrank excluded ones, got %v", ids(ds))
	}
	for _, d := range ds[1:] {
		if !d.Excluded {
			t.Fatalf("excluded campaigns must sort last, got %v", ids(ds))
		}
	}
}

func TestRankDeterministicTieBreak(t *testing.T) {
	// Two identical campaigns except ID: order must be stable by ID.
	mk := func(id string) CampaignInput {
		return CampaignInput{CampaignID: id, Restricted: true, EndAt: base.Add(48 * time.Hour), Drops: chain([2]int{60, 0})}
	}
	for i := 0; i < 5; i++ {
		ds := Rank(ModeSmart, []CampaignInput{mk("b"), mk("a")}, base)
		if ds[0].CampaignID != "a" || ds[1].CampaignID != "b" {
			t.Fatalf("tie-break not deterministic: %v", ids(ds))
		}
	}
}

func TestNormalizeMode(t *testing.T) {
	if Normalize("smart") != ModeSmart {
		t.Error("lowercase mode must normalize")
	}
	if Normalize("  ENDING_SOONEST ") != ModeEndingSoonest {
		t.Error("whitespace must be trimmed")
	}
	if Normalize("nonsense") != DefaultMode {
		t.Errorf("unknown mode must fall back to %s", DefaultMode)
	}
	if Normalize("") != ModeGameOrder {
		t.Error("empty mode must default to GAME_ORDER")
	}
}

func TestBreakdownRendering(t *testing.T) {
	d := Decision{Total: 130, Factors: []Factor{{"channel-restricted campaign", 100}, {"only one eligible live channel", 30}}}
	got := d.Breakdown()
	if !strings.Contains(got, "+100 channel-restricted campaign") ||
		!strings.Contains(got, "+30 only one eligible live channel") ||
		!strings.Contains(got, "Total: 130") {
		t.Fatalf("unexpected breakdown:\n%s", got)
	}
}

func ids(ds []Decision) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.CampaignID
	}
	return out
}
