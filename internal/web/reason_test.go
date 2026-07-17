package web

import "testing"

// T6 (web Logs classifier): the pool emits reason=<canonical> plus, for the
// space-form, reason_raw=<raw>. The exact-match streak class must key off the
// first reason= token and ignore the trailing reason_raw attribute (which is
// quoted because of its space). Regression test only; logclass.go is unchanged.
func TestPointsEarnedStreakClassWithRawAttr(t *testing.T) {
	// Space-form log line: canonical reason + forensic reason_raw.
	space := `time=2026-07-17T10:00:00Z level=INFO msg="Points earned" streamer=skill4ltu points=450 reason=WATCH_STREAK reason_raw="WATCH STREAK"`
	if p := classifyLogLine(space); p.Class != "log-points-streak" || p.Emoji != "🔥" {
		t.Errorf("space-form line classified as {%q, %q}, want {log-points-streak, 🔥}", p.Class, p.Emoji)
	}

	// Underscore-form log line: canonical reason, no reason_raw.
	under := `time=2026-07-17T10:00:00Z level=INFO msg="Points earned" streamer=skill4ltu points=450 reason=WATCH_STREAK`
	if p := classifyLogLine(under); p.Class != "log-points-streak" || p.Emoji != "🔥" {
		t.Errorf("underscore-form line classified as {%q, %q}, want {log-points-streak, 🔥}", p.Class, p.Emoji)
	}
}
