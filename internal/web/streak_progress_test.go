package web

import (
	"bytes"
	"strings"
	"testing"
)

// This file pins the v0.13.7 watch-streak UI hotfix (§5): the progress-bar
// denominator is the watcher's bounded 20-minute pursuit cap, not the obsolete
// fixed 7. streakProgressPercent maps continuously-watched minutes to a clamped
// 0..100 bar, and the rendered UI shows "/20", never "/7".

func TestStreakCapIsPursuitWindow(t *testing.T) {
	if streakCapMinutes != 20 {
		t.Fatalf("streak progress denominator must be the 20-minute pursuit cap, got %d", streakCapMinutes)
	}
}

// §8.13-8.17: the percent is computed against the 20-minute cap and clamps past it.
func TestStreakProgressPercent(t *testing.T) {
	cases := []struct {
		mins int
		want int
	}{
		{0, 0},    // §8.13  0/20
		{7, 35},   // §8.14  7/20
		{12, 60},  // §8.15  12/20
		{20, 100}, // §8.16  20/20
		{23, 100}, // §8.17  >20 clamps
		{45, 100},
	}
	for _, c := range cases {
		if got := streakProgressPercent(c.mins); got != c.want {
			t.Errorf("streakProgressPercent(%d) = %d, want %d (denominator %d)", c.mins, got, c.want, streakCapMinutes)
		}
	}
}

// §8.18: the rendered overview + now-watching partials show the streak
// denominator as /20 and never the obsolete /7.
func TestStreakRenderedDenominatorIsCap(t *testing.T) {
	partials := testPartials(t)

	var overview bytes.Buffer
	if err := partials.ExecuteTemplate(&overview, "overview_live", sampleOverview()); err != nil {
		t.Fatalf("render overview_live: %v", err)
	}
	out := overview.String()
	// sampleOverview's live card has StreakMinutes:5, StreakCapMinutes:20.
	if !strings.Contains(out, "5/20") {
		t.Errorf("overview_live must render the streak as 5/20, got:\n%s", out)
	}
	if strings.Contains(out, "5/7") {
		t.Errorf("overview_live must not render the obsolete 5/7 streak denominator:\n%s", out)
	}

	nw := NowWatchingView{
		Slots: []WatchSlotView{
			{Name: "shroud", Points: "100,000", Game: "VALORANT", StreakPending: true, StreakMinutes: 12, StreakCapMinutes: 20, StreakPercent: 60},
		},
	}
	var side bytes.Buffer
	if err := partials.ExecuteTemplate(&side, "now_watching", nw); err != nil {
		t.Fatalf("render now_watching: %v", err)
	}
	sideOut := side.String()
	if !strings.Contains(sideOut, "12/20") {
		t.Errorf("now_watching must render the streak as 12/20, got:\n%s", sideOut)
	}
	if strings.Contains(sideOut, "12/7") {
		t.Errorf("now_watching must not render the obsolete /7 streak denominator:\n%s", sideOut)
	}
}
