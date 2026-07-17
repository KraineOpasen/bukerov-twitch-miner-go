package logger

import (
	"log/slog"
	"testing"
	"time"
)

// pointsEarnedRecord builds a "Points earned" INFO record with the given
// attributes, matching what the pubsub pool emits.
func pointsEarnedRecord(attrs ...any) slog.Record {
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "Points earned", 0)
	r.Add(attrs...)
	return r
}

// T6 (console classifier): a canonical reason=WATCH_STREAK attribute — which the
// pool now emits for BOTH the space- and underscore-form payloads — selects the
// existing streak style (gold + 🔥). The reason_raw attribute the pool adds for
// the space-form must not disturb it. This is a regression test only; console.go
// is unchanged.
func TestPointsEarnedCanonicalStreakStyle(t *testing.T) {
	// Underscore/canonical reason alone.
	if s := styleForRecord(pointsEarnedRecord("reason", "WATCH_STREAK")); s.color != ansiGold || s.emoji != "🔥" {
		t.Errorf("reason=WATCH_STREAK style = %+v, want gold 🔥", s)
	}

	// Space-form drives reason=WATCH_STREAK + reason_raw="WATCH STREAK"; the extra
	// attribute must not change the streak style.
	if s := styleForRecord(pointsEarnedRecord("reason", "WATCH_STREAK", "reason_raw", "WATCH STREAK")); s.color != ansiGold || s.emoji != "🔥" {
		t.Errorf("reason=WATCH_STREAK + reason_raw style = %+v, want gold 🔥", s)
	}

	// Control: a plain WATCH must NOT be streak-styled.
	if s := styleForRecord(pointsEarnedRecord("reason", "WATCH")); s.color == ansiGold {
		t.Errorf("plain WATCH must not use the streak (gold) style: %+v", s)
	}
}
