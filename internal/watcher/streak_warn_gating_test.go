package watcher

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// T7: after a grant on THIS broadcast, a re-armed pursuit (blip/restart) must
// stay silent — no boost eligibility, no "Pursuing" INFO and, crucially, no
// "past the threshold" WARN masking real non-grants. A genuinely NEW
// broadcast resumes both the pursuit and its diagnostics.
func TestNoteStreakProgressSilentAfterGrantOnSameBroadcast(t *testing.T) {
	w, _ := newTestWatcher(1)
	s := w.streamers[0]
	s.IsOnline = true

	s.Stream.Update("bid-1", "t", nil, nil, 1)
	s.UpdateHistory("WATCH_STREAK", 300) // grant lands on bid-1
	s.Stream.InitWatchStreak()           // blip/restart re-arm on the SAME broadcast

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	s.Stream.MinuteWatched = watchStreakThresholdMinutes + 1
	w.noteStreakProgress(0)

	if strings.Contains(buf.String(), "Pursuing watch streak") {
		t.Errorf("pursuit logged for a broadcast whose streak is already granted:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "past the watch-streak threshold") {
		t.Errorf("threshold WARN fired for an already-granted broadcast (the orzanel noise):\n%s", buf.String())
	}
	if _, ok := w.streakDiag[0]; ok {
		t.Error("pursuit diagnostics must be cleared for a granted broadcast")
	}
	if w.isBoostEligible(0) {
		t.Error("granted broadcast must not be boost-eligible")
	}

	// A NEW broadcast re-arms via Update and diagnostics resume.
	s.Stream.Update("bid-2", "t", nil, nil, 1)
	s.Stream.MinuteWatched = 1
	w.noteStreakProgress(0)
	if !strings.Contains(buf.String(), "Pursuing watch streak") {
		t.Errorf("new broadcast must resume the pursuit logging:\n%s", buf.String())
	}
}
