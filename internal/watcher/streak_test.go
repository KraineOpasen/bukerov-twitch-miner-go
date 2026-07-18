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

// TestNoteStreakProgressLogsPursuitOnceAndReleaseOnce exercises the diagnostics
// state machine for the event-driven model: exactly one "Pursuing watch streak"
// INFO while the streak is pending and the pursuit is not exhausted, and — this
// is the key regression vs the old 7-minute timer — being watched PAST seven
// minutes must NOT trigger a release. The single "Releasing the boost slot" line
// fires exactly once when the bounded continuous-watch window elapses, and it is
// OUTCOME-NEUTRAL: releaseReason=bounded_timeout, outcome=unknown, asserting
// neither that the streak was granted nor that it was not.
func TestNoteStreakProgressLogsPursuitOnceAndReleaseOnce(t *testing.T) {
	w, _ := newTestWatcher(1)
	s := w.streamers[0]

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// Early ticks, pending and not exhausted — including PAST the old 7-minute
	// cutoff AND with WATCH credits present: exactly one "Pursuing" line, and NO
	// release. Neither a bare timer nor WATCH evidence may end the pursuit; only
	// the grant or the bounded window does. This fails if the 7-minute cutoff or
	// an evidence-based early release is reintroduced.
	s.Stream.MinuteWatched = 1
	w.noteStreakProgress(0)
	s.Stream.NoteWatchPointsEvent()
	s.Stream.NoteWatchPointsEvent()
	s.Stream.MinuteWatched = watchStreakThresholdMinutes + 1 // 8 min: past 7, still pursuing
	w.noteStreakProgress(0)

	if got := strings.Count(buf.String(), "Pursuing watch streak"); got != 1 {
		t.Errorf("Pursuing logged %d times, want exactly 1:\n%s", got, buf.String())
	}
	if strings.Contains(buf.String(), "Releasing the watch-streak boost slot") {
		t.Errorf("pursuit released before the bounded window (past 7 min / on WATCH evidence) — early cutoff leaked back in:\n%s", buf.String())
	}

	// Reach the bounded continuous-watch window with the streak still missing:
	// exactly one release line, OUTCOME-NEUTRAL (releaseReason=bounded_timeout,
	// outcome=unknown), asserting neither "granted" nor "not granted". With WATCH
	// credits present (evidence>0) it is the plain variant, without the zero-credit
	// transport/authorization note.
	s.Stream.MinuteWatched = streakPursuitCapMinutes
	w.noteStreakProgress(0)
	w.noteStreakProgress(0)

	rel := buf.String()
	if got := strings.Count(rel, "Releasing the watch-streak boost slot"); got != 1 {
		t.Errorf("release logged %d times, want exactly 1:\n%s", got, rel)
	}
	if !strings.Contains(rel, "releaseReason=bounded_timeout") || !strings.Contains(rel, "outcome=unknown") {
		t.Errorf("release must be tagged releaseReason=bounded_timeout outcome=unknown:\n%s", rel)
	}
	// Must not assert an outcome the release cannot know: the grant may still land,
	// or may have been dropped by the best-effort PubSub transport.
	for _, banned := range []string{"granted no streak", "could not be earned", "benign", "view confirmed counted"} {
		if strings.Contains(rel, banned) {
			t.Errorf("release log must not assert a streak outcome (%q):\n%s", banned, rel)
		}
	}
	// Evidence is present (2 WATCH credits), so no zero-credit transport note.
	if strings.Contains(rel, "check authorization/transport") {
		t.Errorf("with WATCH credits present the release must not emit the zero-credit transport note:\n%s", rel)
	}

	// Earning the streak clears the pursuit state so the next fresh broadcast
	// reports again from scratch.
	s.Stream.WatchStreakMissing = false
	w.noteStreakProgress(0)
	if _, ok := w.streakDiag[0]; ok {
		t.Errorf("streak diagnostics state should be cleared once the streak is earned")
	}
}

// TestBoostStaysEligiblePastSevenMinutes (§5.4) is the core regression + the
// mutation anchor: a pending-streak streamer that has watched PAST seven minutes
// with no confirming WATCH credit yet must STAY boost-eligible, so it keeps its
// slot until Twitch actually grants the streak (which commonly lands after seven
// minutes). Restoring the `MinuteWatched < 7` cutoff makes this fail.
func TestBoostStaysEligiblePastSevenMinutes(t *testing.T) {
	w, _ := newTestWatcher(1)
	s := w.streamers[0]
	s.Stream.Update("b1", "t", nil, nil, 10)
	s.Stream.MinuteWatched = 10 // well past 7, no WATCH evidence, no grant

	if !w.isBoostEligible(0) {
		t.Fatal("a pending streak past 7 minutes must stay boost-eligible (the 7-minute cutoff must be gone)")
	}
	if !w.streakInProgress(0) {
		t.Error("a streamer watched 10 min with a pending streak is still in progress")
	}

	// The real grant ends eligibility (StreakPending -> false), not a timer.
	s.Stream.MarkStreakEarned("b1")
	if w.isBoostEligible(0) {
		t.Error("once the WATCH_STREAK grant lands, the streamer is no longer boost-eligible")
	}
}

// TestBoostNotReleasedByWatchEventsAlone (§5.5, Q1) is the core safety guard for
// the conservative pursuit: WATCH points-credits — however many — must NOT release
// the boost seat. Twitch pays a watch streak only while the channel is watched,
// and nothing proves the grant lands at/before the second WATCH credit; on top of
// that the points-earned stream is account-wide, so a WATCH credit can come from
// an external browser tab or a prior broadcast. The seat is held until the
// authoritative WATCH_STREAK grant or the bounded continuous window — never on
// WATCH evidence. The evidence still must never fabricate a 300-450 grant.
func TestBoostNotReleasedByWatchEventsAlone(t *testing.T) {
	w, _ := newTestWatcher(1)
	s := w.streamers[0]
	s.Stream.Update("b1", "t", nil, nil, 10)
	s.Stream.MinuteWatched = 8 // past 7, well under the bounded cap

	if !w.isBoostEligible(0) {
		t.Fatal("setup: streamer should start boost-eligible")
	}
	// Many WATCH credits must not release the seat: only the grant or the bounded
	// window may. This is the regression guard against re-adding an evidence-based
	// (or any premature) early release.
	for i := 0; i < 5; i++ {
		s.Stream.NoteWatchPointsEvent()
		if !w.isBoostEligible(0) {
			t.Fatalf("WATCH credit #%d released the seat — WATCH evidence must never trigger release", i+1)
		}
	}
	// Still pending, still no fabricated grant.
	if !s.Stream.StreakPending() {
		t.Error("WATCH evidence must never mark the streak earned")
	}
	if e := s.History["WATCH_STREAK"]; e != nil {
		t.Errorf("no WATCH_STREAK must be recorded from WATCH evidence, got %+v", e)
	}
}

// TestDelayedWatchAfterReArmIsSafetyNeutral (Q2) documents the WATCH-evidence
// binding limitation and proves it is safety-neutral. The points-earned payload
// carries no broadcastID, so a WATCH credit that belongs to a prior broadcast
// (or an external browser tab watching the same channel) and arrives after a
// re-arm is counted toward the current attempt. Because release is bounded-minute
// based and NOT evidence based, such a mis-attributed credit can never release the
// boost seat early — it only tints a diagnostic log line.
func TestDelayedWatchAfterReArmIsSafetyNeutral(t *testing.T) {
	w, _ := newTestWatcher(1)
	s := w.streamers[0]
	s.Stream.Update("b1", "t", nil, nil, 10)
	s.Stream.NoteWatchPointsEvent() // belongs to b1

	// A genuinely new broadcast re-arms and resets the evidence counter.
	s.Stream.Update("b2", "t", nil, nil, 10)
	if s.Stream.StreakWatchEvidence() != 0 {
		t.Fatalf("re-arm must reset the evidence counter, got %d", s.Stream.StreakWatchEvidence())
	}

	// Late/foreign WATCH credits now land against b2 (no broadcastID to reject them).
	s.Stream.NoteWatchPointsEvent()
	s.Stream.NoteWatchPointsEvent()
	if s.Stream.StreakWatchEvidence() != 2 {
		t.Fatalf("late WATCH credits are counted toward the current attempt, got %d", s.Stream.StreakWatchEvidence())
	}

	// Safety-critical: even two (possibly mis-attributed) WATCH credits must NOT
	// release the seat while under the bounded window — evidence is diagnostic-only.
	s.Stream.MinuteWatched = 5
	if !w.isBoostEligible(0) {
		t.Error("mis-attributed WATCH credits must never release the boost seat (release is minute-bounded, not evidence-based)")
	}
}

// TestBoostReleasedAtBoundedCap (§5.12) proves the bounded fallback: a pending
// streak holds the seat until the CONTINUOUSLY-watched minutes reach the cap, so
// a channel can't hold the boost forever. WATCH evidence must not move that point
// earlier — the cap is the sole non-grant release.
func TestBoostReleasedAtBoundedCap(t *testing.T) {
	w, _ := newTestWatcher(1)
	s := w.streamers[0]
	s.Stream.Update("b1", "t", nil, nil, 10)
	// Two WATCH credits present: they must NOT bring the release forward.
	s.Stream.NoteWatchPointsEvent()
	s.Stream.NoteWatchPointsEvent()

	s.Stream.MinuteWatched = streakPursuitCapMinutes - 1
	if !w.isBoostEligible(0) {
		t.Error("just under the bounded cap with a pending streak: must stay eligible even with WATCH evidence")
	}
	s.Stream.MinuteWatched = streakPursuitCapMinutes
	if w.isBoostEligible(0) {
		t.Error("at the bounded cap the seat must be released")
	}
}

// TestSatisfiedBroadcastNotBoostEligible (§5.6) proves a broadcast whose streak
// was already granted — including a grant restored from the persistence cache on
// restart — does not re-hold the boost seat. StreakPending is false because the
// remembered grant matches the current broadcast.
func TestSatisfiedBroadcastNotBoostEligible(t *testing.T) {
	w, _ := newTestWatcher(1)
	s := w.streamers[0]
	// Restart: the cache hydrated a grant for broadcast b1; the streamer comes
	// online on that same broadcast.
	s.Stream.HydrateStreakGrant("b1", time.Now())
	s.Stream.Update("b1", "t", nil, nil, 10)

	if s.Stream.StreakPending() {
		t.Fatal("a broadcast whose streak was already granted must not be pending")
	}
	if w.isBoostEligible(0) {
		t.Error("an already-satisfied broadcast must not re-hold the streak boost seat after restart")
	}
}

// TestNoStreakStarvationWithThreeCandidates (§5.9) proves the single boost seat
// hands off so no pending candidate starves: with three off-pair pending-streak
// streamers, the seat goes to one; when that one's streak is granted it releases,
// and the seat moves to another pending candidate rather than staying stuck.
func TestNoStreakStarvationWithThreeCandidates(t *testing.T) {
	w, online := newTestWatcher(5)
	// Base pair {0,1}; 2,3,4 off-pair, all pending-streak with banked minutes so
	// they are "in progress" candidates.
	for _, i := range []int{2, 3, 4} {
		w.streamers[i].Stream.Update("b"+string(rune('0'+i)), "t", nil, nil, 10)
		w.streamers[i].Stream.MinuteWatched = 2
	}
	// Deterministic LRU order: 2 least-recently-watched.
	now := time.Now()
	w.rotation.lastWatched = map[int]time.Time{
		0: now, 1: now,
		2: now.Add(-3 * time.Hour), 3: now.Add(-2 * time.Hour), 4: now.Add(-1 * time.Hour),
	}

	first := w.selectBoostTarget([2]int{0, 1}, online)
	if first != 2 && first != 3 && first != 4 {
		t.Fatalf("boost seat must go to a pending candidate, got %d", first)
	}

	// The chosen candidate's streak is granted → it releases the seat.
	w.streamers[first].Stream.MarkStreakEarned("b" + string(rune('0'+first)))
	if w.isBoostEligible(first) {
		t.Fatalf("granted streamer %d must release the seat", first)
	}

	second := w.selectBoostTarget([2]int{0, 1}, online)
	if second == -1 || second == first {
		t.Fatalf("the seat must hand off to a DIFFERENT pending candidate (no starvation), got %d after %d", second, first)
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
