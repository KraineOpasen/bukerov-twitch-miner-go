package watcher

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// TestBoostFinishesStreakInProgressInsteadOfThrashing covers the anti-thrash
// rule: when several off-pair streamers all have a pending watch streak, the
// single boost seat must keep going to the one already part-way through (most
// watch time banked) so it actually completes, instead of alternating between
// fresh candidates and finishing none.
func TestBoostFinishesStreakInProgressInsteadOfThrashing(t *testing.T) {
	w, online := newTestWatcher(4)
	// Base pair is {0,1}; 2 and 3 are off-pair, both streak-eligible.
	// Streamer 2 is part-way through its streak; streamer 3 just started.
	w.streamers[2].Stream.MinuteWatched = 4 // in progress
	w.streamers[3].Stream.MinuteWatched = 0 // fresh

	// Make the fresh candidate (3) look least-recently-watched, which under the
	// old recency-only rule would have won the seat and starved 2's streak.
	w.rotation.lastWatched = map[int]time.Time{
		0: time.Now(),
		1: time.Now(),
		2: time.Now(), // watched most recently, yet should still win: it's mid-streak
		3: time.Now().Add(-time.Hour),
	}

	boosted := w.applyPriorityBoost([2]int{0, 1}, online)
	if boosted[0] != 2 && boosted[1] != 2 {
		t.Fatalf("expected the in-progress-streak streamer 2 to keep the boost seat, got %v", boosted)
	}
}

// TestBoostRestrictedDropStillOutranksStreak keeps the existing contract: a
// channel-restricted drop campaign still wins the boost seat over a streak in
// progress, because that drop progress can only ever be earned here.
func TestBoostRestrictedDropStillOutranksStreak(t *testing.T) {
	w, online := newTestWatcher(4)
	w.streamers[2].Stream.MinuteWatched = 6 // streak nearly done
	w.streamers[3].Stream.CampaignIDs = []string{"restricted"}
	w.streamers[3].Stream.Campaigns = []*models.Campaign{
		{ID: "restricted", Channels: []string{w.streamers[3].ChannelID}},
	}

	w.rotation.lastWatched = map[int]time.Time{
		0: time.Now(), 1: time.Now(),
		2: time.Now().Add(-time.Hour),
		3: time.Now(),
	}

	boosted := w.applyPriorityBoost([2]int{0, 1}, online)
	if boosted[0] != 3 && boosted[1] != 3 {
		t.Fatalf("expected channel-restricted-drop streamer 3 to win the boost seat over the mid-streak streamer 2, got %v", boosted)
	}
}

// TestNoteStreakProgressLogsPursuitOnceAndStallOnce exercises the diagnostics
// state machine: exactly one "Pursuing watch streak" INFO while the streak is
// pending, then exactly one "past the threshold" WARN once enough minutes are
// banked with the streak still missing.
func TestNoteStreakProgressLogsPursuitOnceAndStallOnce(t *testing.T) {
	w, _ := newTestWatcher(1)
	s := w.streamers[0]

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// Two early ticks with the streak pending and under the threshold: exactly
	// one "Pursuing" line, no stall warning yet.
	s.Stream.MinuteWatched = 1
	w.noteStreakProgress(0)
	s.Stream.MinuteWatched = 3
	w.noteStreakProgress(0)

	if got := strings.Count(buf.String(), "Pursuing watch streak"); got != 1 {
		t.Errorf("Pursuing logged %d times, want exactly 1:\n%s", got, buf.String())
	}
	if strings.Contains(buf.String(), "past the watch-streak threshold") {
		t.Errorf("stall warning fired before the threshold was reached:\n%s", buf.String())
	}

	// Cross the threshold, still missing: exactly one stall warning, and it must
	// not repeat on subsequent ticks.
	s.Stream.MinuteWatched = watchStreakThresholdMinutes + 1
	w.noteStreakProgress(0)
	w.noteStreakProgress(0)

	if got := strings.Count(buf.String(), "past the watch-streak threshold"); got != 1 {
		t.Errorf("stall warning logged %d times, want exactly 1:\n%s", got, buf.String())
	}

	// Earning the streak clears the pursuit state so the next fresh broadcast
	// reports again from scratch.
	s.Stream.WatchStreakMissing = false
	w.noteStreakProgress(0)
	if _, ok := w.streakDiag[0]; ok {
		t.Errorf("streak diagnostics state should be cleared once the streak is earned")
	}
}

// TestNoteStreakProgressSilentWhenDisabled: a streamer with WatchStreak off
// must produce no pursuit logging at all.
func TestNoteStreakProgressSilentWhenDisabled(t *testing.T) {
	w, _ := newTestWatcher(1)
	w.streamers[0].Settings.WatchStreak = false
	w.streamers[0].Stream.MinuteWatched = 10

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	w.noteStreakProgress(0)

	if strings.Contains(buf.String(), "watch streak") || strings.Contains(buf.String(), "watch-streak") {
		t.Errorf("expected no streak logging when WatchStreak is disabled:\n%s", buf.String())
	}
}
