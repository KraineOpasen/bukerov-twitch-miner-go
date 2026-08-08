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

// §8.18: the rendered now-watching partial — the one surface that still shows
// streak progress, now that /overview carries no per-streamer cards — uses /20
// as the denominator and never the obsolete /7.
func TestStreakRenderedDenominatorIsCap(t *testing.T) {
	partials := testPartials(t)

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

// TestStreakRenderedSevenOfTwentyIsThirtyFivePercent is the end-to-end pin for
// ONE known input: 7 continuously-watched minutes against the 20-minute pursuit
// cap renders "7/20" as text and a bar exactly 35% wide.
//
// Both figures come from production code rather than from the fixture — the
// denominator is streakCapMinutes and the width is streakProgressPercent(7) —
// so a changed cap or a changed percent formula fails here instead of silently
// rendering a bar that disagrees with its own label. §8.14 pins 7 -> 35 as
// arithmetic; this pins that the RENDERED page actually says so.
//
// Fully deterministic: no sleep, no wall-clock reads, no dependence on any
// other test having run first.
func TestStreakRenderedSevenOfTwentyIsThirtyFivePercent(t *testing.T) {
	const mins = 7

	// Pin the two numbers as literals first. Without this the render
	// assertions below would happily follow a changed formula wherever it went.
	if streakCapMinutes != 20 {
		t.Fatalf("streakCapMinutes = %d, want 20", streakCapMinutes)
	}
	pct := streakProgressPercent(mins)
	if pct != 35 {
		t.Fatalf("streakProgressPercent(%d) = %d, want 35", mins, pct)
	}

	partials := testPartials(t)
	nw := NowWatchingView{
		Slots: []WatchSlotView{{
			Name: "shroud", Points: "100,000", Game: "VALORANT",
			StreakPending:    true,
			StreakMinutes:    mins,
			StreakCapMinutes: streakCapMinutes,
			StreakPercent:    pct,
		}},
	}
	var buf bytes.Buffer
	if err := partials.ExecuteTemplate(&buf, "now_watching", nw); err != nil {
		t.Fatalf("render now_watching: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "7/20") {
		t.Errorf("rendered streak must read 7/20; out=\n%s", out)
	}
	if !strings.Contains(out, "width: 35%") {
		t.Errorf("rendered streak bar must be 35%% wide; out=\n%s", out)
	}
	// The label and the bar describe the same progress, so a denominator the
	// bar does not agree with is a rendered contradiction, not a rounding
	// difference.
	if strings.Contains(out, "7/7") {
		t.Errorf("rendered streak used the obsolete /7 denominator; out=\n%s", out)
	}
}
