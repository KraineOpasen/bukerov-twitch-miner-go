package policy

import (
	"encoding/json"
	"fmt"
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

func TestUnknownDeadlineWithRemainingWorkIsNotImpossibleOrExcluded(t *testing.T) {
	in := CampaignInput{
		CampaignID: "unknown-deadline",
		Drops:      chain([2]int{60, 0}),
	}

	f := ComputeFeasibility(in, base)
	if f.Status != StatusUnknown {
		t.Errorf("unknown deadline status = %s, want UNKNOWN", f.Status)
	}
	if f.DeadlineKnown {
		t.Error("zero EndAt must leave DeadlineKnown false")
	}
	if f.TimeUntilEnd != 0 || f.CanCompleteNextReward || f.CanCompleteAll {
		t.Fatalf("unknown deadline manufactured certainty: %+v", f)
	}

	d := Decide(ModeSmart, in, base)
	if d.Excluded {
		t.Fatalf("unknown deadline decision: status=%s excluded=%v reason=%q", d.Status, d.Excluded, d.ExcludeReason)
	}
	if d.ExcludeReason != "" {
		t.Fatalf("non-excluded unknown deadline has reason %q", d.ExcludeReason)
	}
}

func TestFeasibilityDeadlineBoundaryMatrix(t *testing.T) {
	cases := []struct {
		name         string
		endAt        time.Time
		drops        []DropStep
		nextOnly     bool
		wantStatus   FeasStatus
		wantKnown    bool
		wantNext     bool
		wantAll      bool
		wantUntil    time.Duration
		wantExcluded bool
	}{
		{
			name: "unknown with work remaining", drops: chain([2]int{60, 0}),
			wantStatus: StatusUnknown,
		},
		{
			name: "known past", endAt: base.Add(-time.Minute), drops: chain([2]int{60, 0}),
			wantStatus: StatusImpossible, wantKnown: true, wantExcluded: true,
		},
		{
			name: "known exact now", endAt: base, drops: chain([2]int{60, 0}),
			wantStatus: StatusImpossible, wantKnown: true, wantExcluded: true,
		},
		{
			name: "known future safe", endAt: base.Add(48 * time.Hour), drops: chain([2]int{60, 10}, [2]int{120, 10}),
			wantStatus: StatusSafe, wantKnown: true, wantNext: true, wantAll: true, wantUntil: 48 * time.Hour,
		},
		{
			name: "known future at risk", endAt: base.Add(125 * time.Minute), drops: chain([2]int{100, 0}),
			wantStatus: StatusAtRisk, wantKnown: true, wantNext: true, wantAll: true, wantUntil: 125 * time.Minute,
		},
		{
			name: "known future next reward only", endAt: base.Add(90 * time.Minute), drops: chain([2]int{60, 0}, [2]int{600, 0}),
			wantStatus: StatusNextRewardOnly, wantKnown: true, wantNext: true, wantUntil: 90 * time.Minute,
		},
		{
			name: "known future impossible", endAt: base.Add(30 * time.Minute), drops: chain([2]int{120, 0}),
			wantStatus: StatusImpossible, wantKnown: true, wantUntil: 30 * time.Minute, wantExcluded: true,
		},
		{
			name: "unix epoch is a known past deadline", endAt: time.Unix(0, 0).UTC(), drops: chain([2]int{60, 0}),
			wantStatus: StatusImpossible, wantKnown: true, wantExcluded: true,
		},
		{
			name: "unknown remains unknown under next reward only rule", drops: chain([2]int{60, 0}, [2]int{600, 0}), nextOnly: true,
			wantStatus: StatusUnknown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := CampaignInput{
				CampaignID:     tc.name,
				EndAt:          tc.endAt,
				Drops:          tc.drops,
				NextRewardOnly: tc.nextOnly,
			}
			f := ComputeFeasibility(in, base)
			if f.Status != tc.wantStatus || f.DeadlineKnown != tc.wantKnown ||
				f.CanCompleteNextReward != tc.wantNext || f.CanCompleteAll != tc.wantAll ||
				f.TimeUntilEnd != tc.wantUntil {
				t.Fatalf("feasibility = %+v, want status=%s known=%v next=%v all=%v until=%s",
					f, tc.wantStatus, tc.wantKnown, tc.wantNext, tc.wantAll, tc.wantUntil)
			}

			d := Decide(ModeSmart, in, base)
			if d.Excluded != tc.wantExcluded {
				t.Fatalf("decision excluded=%v reason=%q, want excluded=%v", d.Excluded, d.ExcludeReason, tc.wantExcluded)
			}
			if tc.wantExcluded && d.ExcludeReason != "cannot finish the next reward before the campaign ends" {
				t.Fatalf("impossible exclusion reason = %q", d.ExcludeReason)
			}
		})
	}
}

func TestUnknownDeadlineWithNoRemainingWorkKeepsCompleteSemantics(t *testing.T) {
	in := CampaignInput{
		CampaignID: "complete-unknown-deadline",
		Drops:      chain([2]int{60, 60}, [2]int{120, 120}),
	}
	f := ComputeFeasibility(in, base)
	if f.DeadlineKnown || f.Status != StatusSafe {
		t.Fatalf("complete campaign feasibility = %+v, want deadline irrelevant and SAFE", f)
	}
	d := Decide(ModeSmart, in, base)
	if d.Excluded {
		t.Fatalf("complete campaign with unknown deadline must not be excluded: %+v", d)
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

func TestSmartUnknownDeadlineIsNeutralAndExplicit(t *testing.T) {
	in := CampaignInput{
		CampaignID:           "unknown-smart",
		Restricted:           true,
		Started:              true,
		Drops:                chain([2]int{22, 0}),
		EligibleLiveChannels: 1,
		ChannelStability:     1,
		StabilitySamples:     minStabilitySamples,
	}
	unknown := Decide(ModeSmart, in, base)
	if unknown.Status != StatusUnknown || unknown.Excluded {
		t.Fatalf("unknown SMART decision = %+v", unknown)
	}
	if _, ok := factorPoints(unknown, "ends in under"); ok {
		t.Fatal("unknown deadline must not receive an ending-soon bonus")
	}
	if pts, ok := factorPoints(unknown, "deadline unknown"); !ok || pts != 0 {
		t.Fatalf("unknown-deadline explanation = (%d, present=%v), want neutral explicit factor", pts, ok)
	}

	knownInput := in
	knownInput.EndAt = base.Add(48 * time.Hour)
	known := Decide(ModeSmart, knownInput, base)
	if unknown.Total != known.Total {
		t.Fatalf("unknown deadline changed unrelated SMART factors: unknown=%d known-far=%d", unknown.Total, known.Total)
	}
	for _, label := range []string{"High priority", "channel-restricted", "next reward", "only one eligible", "campaign already started"} {
		unknownPoints, unknownOK := factorPoints(unknown, label)
		knownPoints, knownOK := factorPoints(known, label)
		if unknownPoints != knownPoints || unknownOK != knownOK {
			t.Errorf("factor %q changed: unknown=(%d,%v) known=(%d,%v)", label, unknownPoints, unknownOK, knownPoints, knownOK)
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
		if ranked[0].SemanticClass != 0 || ranked[1].SemanticClass != 1 {
			t.Errorf("mode %s: HighPriority semantic classes = [%d %d], want [0 1]", mode, ranked[0].SemanticClass, ranked[1].SemanticClass)
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
		for i, d := range ds {
			if d.SemanticClass != SemanticClass(i) {
				t.Fatalf("mode %s position %d semantic class = %d, want %d for distinct policy facts", mode, i, d.SemanticClass, i)
			}
		}
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
	if got := order(ModeSmart); got[0] != "B" {
		t.Errorf("SMART first = %s, want B", got[0])
	}
}

func TestEqualPolicyFactsShareSemanticClassInEveryMode(t *testing.T) {
	mk := func(id string) CampaignInput {
		return CampaignInput{
			CampaignID:           id,
			GameOrderIndex:       1,
			EndAt:                base.Add(12 * time.Hour),
			Drops:                chain([2]int{60, 20}),
			EligibleLiveChannels: 2,
			ChannelStability:     1,
			StabilitySamples:     minStabilitySamples,
		}
	}
	for _, mode := range []Mode{ModeGameOrder, ModeEndingSoonest, ModeClosestToReward, ModeLowAvailability, ModeSmart} {
		ds := Rank(mode, []CampaignInput{mk("c"), mk("a"), mk("b")}, base)
		if got := strings.Join(ids(ds), ","); got != "a,b,c" {
			t.Fatalf("mode %s deterministic presentation order = %s, want a,b,c", mode, got)
		}
		for _, d := range ds {
			if d.SemanticClass != 0 {
				t.Fatalf("mode %s equal facts split by campaign ID: %s class=%d", mode, d.CampaignID, d.SemanticClass)
			}
		}
	}
}

func TestEndingSoonestRanksKnownDeadlineBeforeUnknownWithinPriorityClass(t *testing.T) {
	known := CampaignInput{CampaignID: "known", EndAt: base.Add(3 * time.Hour), Drops: chain([2]int{30, 0})}
	unknown := CampaignInput{CampaignID: "unknown", Drops: chain([2]int{30, 0})}

	for _, inputs := range [][]CampaignInput{{unknown, known}, {known, unknown}} {
		ds := Rank(ModeEndingSoonest, inputs, base)
		if got := ids(ds); got[0] != "known" || got[1] != "unknown" {
			t.Fatalf("ENDING_SOONEST order = %v, want [known unknown]", got)
		}
		if ds[0].SemanticClass != 0 || ds[1].SemanticClass != 1 {
			t.Fatalf("ENDING_SOONEST known/unknown semantic classes = [%d %d], want [0 1]", ds[0].SemanticClass, ds[1].SemanticClass)
		}
		if ds[1].Status != StatusUnknown || ds[1].Excluded {
			t.Fatalf("unknown decision = %+v", ds[1])
		}
		if pts, ok := factorPoints(ds[1], "deadline unknown"); !ok || pts != 0 {
			t.Fatalf("unknown ENDING_SOONEST explanation = (%d, present=%v)", pts, ok)
		}
		if _, ok := factorPoints(ds[1], "ends in 0s"); ok {
			t.Fatal("unknown deadline masqueraded as an immediate expiry")
		}
	}

	unknown.HighPriority = true
	ds := Rank(ModeEndingSoonest, []CampaignInput{known, unknown}, base)
	if ds[0].CampaignID != "unknown" {
		t.Fatalf("global HighPriority invariant changed: %v", ids(ds))
	}
}

func TestUnknownDeadlinePreservesNonDeadlineModeSemantics(t *testing.T) {
	unknown := CampaignInput{
		CampaignID:           "unknown",
		GameOrderIndex:       0,
		Drops:                chain([2]int{30, 25}),
		EligibleLiveChannels: 1,
	}
	known := CampaignInput{
		CampaignID:           "known",
		GameOrderIndex:       1,
		EndAt:                base.Add(48 * time.Hour),
		Drops:                chain([2]int{30, 10}),
		EligibleLiveChannels: 2,
	}

	for _, mode := range []Mode{ModeGameOrder, ModeClosestToReward, ModeLowAvailability} {
		ds := Rank(mode, []CampaignInput{known, unknown}, base)
		if ds[0].CampaignID != "unknown" {
			t.Fatalf("mode %s ignored its non-deadline ordering fact: %v", mode, ids(ds))
		}
		if ds[0].Status != StatusUnknown || ds[0].Excluded {
			t.Fatalf("mode %s turned unknown deadline into impossibility: %+v", mode, ds[0])
		}
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

func TestRankPermutationDeterminismWithUnknownDeadlines(t *testing.T) {
	mk := func(id string) CampaignInput {
		return CampaignInput{
			CampaignID:           id,
			GameOrderIndex:       2,
			Drops:                chain([2]int{60, 10}),
			EligibleLiveChannels: 2,
			ChannelStability:     1,
			StabilitySamples:     minStabilitySamples,
		}
	}
	a, b, c := mk("a"), mk("b"), mk("c")
	permutations := [][]CampaignInput{
		{a, b, c}, {a, c, b}, {b, a, c}, {b, c, a}, {c, a, b}, {c, b, a},
	}
	for _, mode := range []Mode{ModeGameOrder, ModeEndingSoonest, ModeClosestToReward, ModeLowAvailability, ModeSmart} {
		for i, inputs := range permutations {
			if got := strings.Join(ids(Rank(mode, inputs, base)), ","); got != "a,b,c" {
				t.Fatalf("mode %s permutation %d = %s, want a,b,c", mode, i, got)
			}
		}
	}
}

func TestSemanticUtilityUsesOneBoundedDistinctSecondary(t *testing.T) {
	facts := map[string]CampaignSemantic{
		"primary":   {SemanticClass: 0, SecondaryEligible: true},
		"secondary": {SemanticClass: 4, SecondaryEligible: true},
	}
	one, ok := BuildSemanticUtility([]string{"primary"}, facts)
	if !ok {
		t.Fatal("single primary campaign was not ranked")
	}
	overlap, ok := BuildSemanticUtility([]string{"primary", "secondary"}, facts)
	if !ok || !overlap.HasSecondary || overlap.SecondarySemanticClass != 4 {
		t.Fatalf("overlap utility = %+v, ranked=%v", overlap, ok)
	}
	if CompareSemanticUtility(overlap, one) <= 0 {
		t.Fatalf("bounded overlap utility %+v did not beat equal primary %+v", overlap, one)
	}

	for _, additional := range []int{2, 5, 20} {
		ids := []string{"primary", "secondary"}
		manyFacts := map[string]CampaignSemantic{
			"primary":   facts["primary"],
			"secondary": facts["secondary"],
		}
		for i := 1; i < additional; i++ {
			id := fmt.Sprintf("weak-%02d", i)
			ids = append(ids, id)
			manyFacts[id] = CampaignSemantic{SemanticClass: SemanticClass(10 + i), SecondaryEligible: true}
		}
		many, ranked := BuildSemanticUtility(ids, manyFacts)
		if !ranked {
			t.Fatalf("%d additional campaigns were not ranked", additional)
		}
		if cmp := CompareSemanticUtility(many, overlap); cmp != 0 {
			t.Fatalf("%d additional campaigns accumulated utility: many=%+v one=%+v cmp=%d", additional, many, overlap, cmp)
		}
	}
}

func TestSemanticUtilityPrimaryAlwaysPrecedesSecondary(t *testing.T) {
	strong, ok := BuildSemanticUtility([]string{"strong"}, map[string]CampaignSemantic{
		"strong": {SemanticClass: 0, SecondaryEligible: true},
	})
	if !ok {
		t.Fatal("strong primary campaign was not ranked")
	}
	for _, additional := range []int{2, 5, 20} {
		facts := map[string]CampaignSemantic{
			"weak-primary": {SemanticClass: 1, SecondaryEligible: true},
		}
		ids := []string{"weak-primary"}
		for i := 0; i < additional; i++ {
			id := fmt.Sprintf("weak-extra-%02d", i)
			ids = append(ids, id)
			facts[id] = CampaignSemantic{SemanticClass: SemanticClass(2 + i), SecondaryEligible: true}
		}
		weak, ranked := BuildSemanticUtility(ids, facts)
		if !ranked {
			t.Fatalf("weak overlap with %d additions was not ranked", additional)
		}
		if cmp := CompareSemanticUtility(strong, weak); cmp <= 0 {
			t.Fatalf("strong primary %+v lost to %d weaker campaigns %+v (cmp=%d)", strong, additional, weak, cmp)
		}
	}
}

func TestSemanticUtilityPreservesPrimaryOrderingInEveryMode(t *testing.T) {
	for _, mode := range []Mode{ModeGameOrder, ModeEndingSoonest, ModeClosestToReward, ModeLowAvailability, ModeSmart} {
		t.Run(string(mode), func(t *testing.T) {
			decisions := Rank(mode, []CampaignInput{
				{
					CampaignID: "strong", HighPriority: true, GameOrderIndex: 9,
					EndAt: base.Add(48 * time.Hour), Drops: chain([2]int{120, 0}), EligibleLiveChannels: 9,
				},
				{
					CampaignID: "weak", GameOrderIndex: 0,
					EndAt: base.Add(2 * time.Hour), Drops: chain([2]int{30, 29}), EligibleLiveChannels: 1,
				},
				{
					CampaignID: "extra", GameOrderIndex: 5,
					EndAt: base.Add(10 * time.Hour), Drops: chain([2]int{60, 0}), EligibleLiveChannels: 5,
				},
			}, base)
			facts := make(map[string]CampaignSemantic, len(decisions))
			for _, decision := range decisions {
				fact, ok := CampaignSemanticFromDecision(decision)
				if !ok {
					t.Fatalf("mode %s decision unexpectedly unpublished: %+v", mode, decision)
				}
				facts[decision.CampaignID] = fact
			}
			strong, strongOK := BuildSemanticUtility([]string{"strong"}, facts)
			weakOverlap, weakOK := BuildSemanticUtility([]string{"weak", "extra"}, facts)
			if !strongOK || !weakOK || CompareSemanticUtility(strong, weakOverlap) <= 0 {
				t.Fatalf("mode %s primary ordering lost: strong=%+v (%v) weak overlap=%+v (%v)", mode, strong, strongOK, weakOverlap, weakOK)
			}
		})
	}
}

func TestSemanticUtilityDeduplicatesCampaignIDAndIgnoresIneligibleSecondary(t *testing.T) {
	facts := map[string]CampaignSemantic{
		"primary":    {SemanticClass: 0, SecondaryEligible: true},
		"eligible":   {SemanticClass: 3, SecondaryEligible: true},
		"unknown":    {SemanticClass: 1},
		"impossible": {SemanticClass: 1},
		"skipped":    {SemanticClass: 1},
		"completed":  {SemanticClass: 1},
	}
	single, ok := BuildSemanticUtility([]string{"primary"}, facts)
	if !ok {
		t.Fatal("single campaign was not ranked")
	}
	duplicates, ok := BuildSemanticUtility([]string{"primary", "primary", "primary"}, facts)
	if !ok || duplicates.HasSecondary || CompareSemanticUtility(duplicates, single) != 0 {
		t.Fatalf("duplicate CampaignID manufactured secondary utility: single=%+v duplicates=%+v", single, duplicates)
	}

	failClosed, ok := BuildSemanticUtility(
		[]string{"primary", "unknown", "impossible", "skipped", "completed"},
		facts,
	)
	if !ok || failClosed.HasSecondary || CompareSemanticUtility(failClosed, single) != 0 {
		t.Fatalf("ineligible campaigns manufactured secondary utility: single=%+v failClosed=%+v", single, failClosed)
	}

	eligible, ok := BuildSemanticUtility([]string{"primary", "eligible"}, facts)
	if !ok || !eligible.HasSecondary || CompareSemanticUtility(eligible, single) <= 0 {
		t.Fatalf("distinct eligible campaign did not provide bounded secondary utility: %+v", eligible)
	}
}

func TestSemanticUtilityIntersectsCurrentRemainingWork(t *testing.T) {
	facts := map[string]CampaignSemantic{
		"primary":   {SemanticClass: 0, SecondaryEligible: true},
		"secondary": {SemanticClass: 2, SecondaryEligible: true},
	}
	live, ok := BuildSemanticUtilityWithRemainingWork(
		[]string{"primary", "secondary"},
		[]string{"primary", "secondary"},
		facts,
	)
	if !ok || !live.HasSecondary {
		t.Fatalf("live secondary utility = %+v, ranked=%v", live, ok)
	}

	completed, ok := BuildSemanticUtilityWithRemainingWork(
		[]string{"primary", "secondary"},
		[]string{"primary"},
		facts,
	)
	if !ok || completed.SemanticClass != live.SemanticClass || completed.HasSecondary {
		t.Fatalf("completed secondary was not downgraded without changing primary: live=%+v completed=%+v ranked=%v", live, completed, ok)
	}
}

func TestCampaignSemanticFromDecisionFailsClosedForSecondary(t *testing.T) {
	cases := []struct {
		name          string
		decision      Decision
		wantPublished bool
		wantEligible  bool
	}{
		{name: "safe", decision: Decision{CampaignID: "safe", Status: StatusSafe, Feasibility: Feasibility{MinutesToNextReward: 10}}, wantPublished: true, wantEligible: true},
		{name: "at risk", decision: Decision{CampaignID: "risk", Status: StatusAtRisk, Feasibility: Feasibility{MinutesToNextReward: 10}}, wantPublished: true, wantEligible: true},
		{name: "next reward only", decision: Decision{CampaignID: "next", Status: StatusNextRewardOnly, Feasibility: Feasibility{MinutesToNextReward: 10}}, wantPublished: true, wantEligible: true},
		{name: "unknown", decision: Decision{CampaignID: "unknown", Status: StatusUnknown, Feasibility: Feasibility{MinutesToNextReward: 10}}, wantPublished: true},
		{name: "impossible", decision: Decision{CampaignID: "impossible", Status: StatusImpossible, Feasibility: Feasibility{MinutesToNextReward: 10}}},
		{name: "completed", decision: Decision{CampaignID: "completed", Status: StatusSafe, Feasibility: Feasibility{MinutesToNextReward: 0}}, wantPublished: true},
		{name: "skip", decision: Decision{CampaignID: "skip", Status: StatusSafe, Excluded: true, ExcludeReason: "per-drop rule: Skip"}},
		{name: "empty ID", decision: Decision{Status: StatusSafe, Feasibility: Feasibility{MinutesToNextReward: 10}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fact, published := CampaignSemanticFromDecision(tc.decision)
			if published != tc.wantPublished {
				t.Fatalf("published=%v, want %v (fact=%+v)", published, tc.wantPublished, fact)
			}
			if fact.SecondaryEligible != tc.wantEligible {
				t.Fatalf("secondary eligible=%v, want %v (fact=%+v)", fact.SecondaryEligible, tc.wantEligible, fact)
			}
		})
	}
}

func TestSemanticUtilityEqualPrimaryClassDoesNotLetUnknownManufactureOverlap(t *testing.T) {
	oneEligible := map[string]CampaignSemantic{
		"eligible": {SemanticClass: 0, SecondaryEligible: true},
		"unknown":  {SemanticClass: 0},
	}
	utility, ok := BuildSemanticUtility([]string{"unknown", "eligible"}, oneEligible)
	if !ok || utility.PrimaryCampaignID != "eligible" || utility.HasSecondary {
		t.Fatalf("equal-class UNKNOWN manufactured overlap: utility=%+v ranked=%v", utility, ok)
	}

	twoEligible := map[string]CampaignSemantic{
		"eligible-a": {SemanticClass: 0, SecondaryEligible: true},
		"eligible-b": {SemanticClass: 0, SecondaryEligible: true},
	}
	utility, ok = BuildSemanticUtility([]string{"eligible-b", "eligible-a"}, twoEligible)
	if !ok || utility.PrimaryCampaignID != "eligible-a" || !utility.HasSecondary ||
		utility.SecondaryCampaignID != "eligible-b" || utility.SecondarySemanticClass != 0 {
		t.Fatalf("two equal-class feasible campaigns did not form one bounded overlap: utility=%+v ranked=%v", utility, ok)
	}
}

func TestSemanticUtilityIsDeterministicUnderInputPermutations(t *testing.T) {
	facts := map[string]CampaignSemantic{
		"primary-b": {SemanticClass: 0, SecondaryEligible: true},
		"weak":      {SemanticClass: 3, SecondaryEligible: true},
		"primary-a": {SemanticClass: 0, SecondaryEligible: true},
	}
	permutations := [][]string{
		{"primary-b", "weak", "primary-a"},
		{"primary-b", "primary-a", "weak"},
		{"weak", "primary-b", "primary-a"},
		{"weak", "primary-a", "primary-b"},
		{"primary-a", "primary-b", "weak"},
		{"primary-a", "weak", "primary-b"},
	}
	want, ok := BuildSemanticUtility(permutations[0], facts)
	if !ok {
		t.Fatal("reference permutation was not ranked")
	}
	for i, ids := range permutations[1:] {
		got, ranked := BuildSemanticUtility(ids, facts)
		if !ranked || got != want {
			t.Fatalf("permutation %d utility = %+v, ranked=%v, want %+v", i+1, got, ranked, want)
		}
	}
}

func TestUnknownDeadlineDecisionJSONIsExplicit(t *testing.T) {
	d := Decide(ModeEndingSoonest, CampaignInput{
		CampaignID: "unknown-json",
		Drops:      chain([2]int{60, 0}),
	}, base)
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Status      FeasStatus `json:"status"`
		Feasibility struct {
			DeadlineKnown         *bool         `json:"deadlineKnown"`
			TimeUntilEnd          time.Duration `json:"timeUntilEnd"`
			CanCompleteNextReward bool          `json:"canCompleteNextReward"`
			CanCompleteAll        bool          `json:"canCompleteAll"`
			Status                FeasStatus    `json:"status"`
		} `json:"feasibility"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusUnknown || got.Feasibility.Status != StatusUnknown {
		t.Fatalf("serialized status = decision:%s feasibility:%s, want UNKNOWN", got.Status, got.Feasibility.Status)
	}
	if got.Feasibility.DeadlineKnown == nil || *got.Feasibility.DeadlineKnown {
		t.Fatalf("serialized deadlineKnown = %v, want explicit false", got.Feasibility.DeadlineKnown)
	}
	if got.Feasibility.TimeUntilEnd != 0 || got.Feasibility.CanCompleteNextReward || got.Feasibility.CanCompleteAll {
		t.Fatalf("serialized unknown deadline manufactured certainty: %s", b)
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
