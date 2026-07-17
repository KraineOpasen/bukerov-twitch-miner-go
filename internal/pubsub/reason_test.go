package pubsub

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// pointsEarnedMsg builds a realistic community-points-user-v1 points-earned
// message through the real parse path, carrying the given raw reason_code so
// tests exercise the exact shape production receives (NOT a pre-canonicalized
// constant — that was the PR #96 fixture mistake).
func pointsEarnedMsg(t *testing.T, channelID, reasonCode string, totalPoints, balance int) *PubSubMessage {
	t.Helper()
	raw := fmt.Sprintf(`{
		"type": "points-earned",
		"data": {
			"timestamp": "2026-07-17T10:00:00.000000000Z",
			"channel_id": %q,
			"point_gain": {"user_id":"999","channel_id":%q,"total_points":%d,"reason_code":%q},
			"balance": {"user_id":"999","channel_id":%q,"balance":%d}
		}
	}`, channelID, channelID, totalPoints, reasonCode, channelID, balance)
	msg, err := ParsePubSubMessage(&WSData{Topic: "community-points-user-v1.999", Message: raw})
	if err != nil {
		t.Fatalf("ParsePubSubMessage: %v", err)
	}
	return msg
}

// T7 + T8: the canonicalization contract is an EXACT, case-insensitive,
// space-trimmed table. WATCH must never swallow WATCH STREAK, both streak forms
// collapse to the single canonical, and an unknown reason passes through trimmed
// (never coerced to WATCH / OTHER).
func TestCanonicalReasonCode(t *testing.T) {
	cases := []struct {
		raw, want string
	}{
		{"WATCH STREAK", "WATCH_STREAK"}, // production space-form
		{"WATCH_STREAK", "WATCH_STREAK"}, // underscore-form
		{"WATCH", "WATCH"},
		{"CLAIM", "CLAIM"},
		{"RAID", "RAID"},
		{"watch streak", "WATCH_STREAK"},                     // case-insensitive lookup
		{"  WATCH STREAK  ", "WATCH_STREAK"},                 // trimmed lookup
		{"WATCH_STREAK ", "WATCH_STREAK"},                    // trailing space still matches
		{"SOME NEW TWITCH REASON", "SOME NEW TWITCH REASON"}, // unknown passthrough
		{"  weird  ", "weird"},                               // unknown trimmed, case preserved
		{"", ""},
	}
	for _, c := range cases {
		if got := CanonicalReasonCode(c.raw); got != c.want {
			t.Errorf("CanonicalReasonCode(%q) = %q, want %q", c.raw, got, c.want)
		}
	}

	// Explicit anti-collision assertion (T7): WATCH and WATCH STREAK are distinct
	// canonical values — no HasPrefix/Contains fallout.
	if CanonicalReasonCode("WATCH") == CanonicalReasonCode("WATCH STREAK") {
		t.Fatal("WATCH must not canonicalize to the same value as WATCH STREAK")
	}
}

// PointReason is the single shared extraction point; it must return the raw
// payload string untouched, the canonical form, and must never mutate msg.Data.
func TestPointReasonExtractsRawAndCanonical(t *testing.T) {
	msg := pointsEarnedMsg(t, "123456", "WATCH STREAK", 450, 231450)

	raw, canonical, ok := PointReason(msg)
	if !ok {
		t.Fatal("PointReason ok = false, want true")
	}
	if raw != "WATCH STREAK" {
		t.Errorf("raw = %q, want %q (unchanged wire form)", raw, "WATCH STREAK")
	}
	if canonical != ReasonWatchStreak {
		t.Errorf("canonical = %q, want %q", canonical, ReasonWatchStreak)
	}

	// msg.Data must be pristine — the raw reason_code is still the space-form.
	pg := msg.Data["point_gain"].(map[string]interface{})
	if pg["reason_code"] != "WATCH STREAK" {
		t.Errorf("PointReason mutated the payload: reason_code = %v", pg["reason_code"])
	}

	// No point_gain -> not ok.
	empty := &PubSubMessage{Data: map[string]interface{}{}}
	if _, _, ok := PointReason(empty); ok {
		t.Error("PointReason ok = true for a message with no point_gain")
	}
}

// callbackState captures streamer state observed FROM INSIDE the onMessage
// callback, to prove the pool's own handleCommunityPointsUser (UpdateHistory ->
// MarkStreakEarned) has already run before onMessage fires.
type callbackState struct {
	called        bool
	streakEntry   *models.HistoryEntry
	hasSpaceKey   bool
	streakMissing bool
	boundBID      string
	pending       bool
}

// T1: a production-shaped space-form ("WATCH STREAK") message driven through the
// REAL pool dispatcher must, BY THE TIME onMessage fires, have canonicalized the
// reason, credited History under the canonical key, cleared the streak-missing
// flag, and bound the grant to the current broadcast. This proves both the
// canonicalization AND the dispatcher ordering (handleCommunityPointsUser before
// onMessage).
func TestPoolSpaceFormWatchStreakBindsBeforeOnMessage(t *testing.T) {
	streamer := models.NewStreamer("skill4ltu", models.DefaultStreamerSettings())
	streamer.ChannelID = "123456"
	streamer.Stream.Update("bcast-1", "title", nil, nil, 5) // identify the broadcast
	if !streamer.Stream.StreakPending() {
		t.Fatal("precondition: a fresh identified broadcast should have the streak pending")
	}

	var cb callbackState
	pool := &WebSocketPool{streamers: []*models.Streamer{streamer}}
	pool.SetMessageHandler(func(_ *PubSubMessage, s *models.Streamer) {
		cb.called = true
		cb.streakEntry = s.History[ReasonWatchStreak]
		_, cb.hasSpaceKey = s.History["WATCH STREAK"]
		cb.streakMissing = s.Stream.GetWatchStreakMissing()
		cb.boundBID, _ = s.Stream.StreakEarnedGrant()
		cb.pending = s.Stream.StreakPending()
	})

	pool.handleMessage(pointsEarnedMsg(t, "123456", "WATCH STREAK", 450, 231450))

	if !cb.called {
		t.Fatal("onMessage callback was never invoked")
	}
	// Observed from inside the callback — the binding is already set.
	if cb.streakEntry == nil || cb.streakEntry.Amount != 450 || cb.streakEntry.Counter != 1 {
		t.Errorf("callback saw History[WATCH_STREAK] = %+v, want {Counter:1 Amount:450}", cb.streakEntry)
	}
	if cb.hasSpaceKey {
		t.Error("History must not contain the raw space-form key \"WATCH STREAK\"")
	}
	if cb.streakMissing {
		t.Error("callback saw WatchStreakMissing = true; MarkStreakEarned should have cleared it first")
	}
	if cb.boundBID != "bcast-1" {
		t.Errorf("callback saw streakEarnedBroadcastID = %q, want bcast-1", cb.boundBID)
	}
	if cb.pending {
		t.Error("callback saw StreakPending = true; the grant should already suppress pursuit")
	}

	// Final state after dispatch matches.
	if streamer.Stream.GetWatchStreakMissing() {
		t.Error("WatchStreakMissing should stay cleared after dispatch")
	}
	if _, ok := streamer.History["WATCH STREAK"]; ok {
		t.Error("no space-form History key should ever be created")
	}
}

// T3: the underscore-form must produce the identical runtime result.
func TestPoolUnderscoreFormWatchStreak(t *testing.T) {
	streamer := models.NewStreamer("skill4ltu", models.DefaultStreamerSettings())
	streamer.ChannelID = "123456"
	streamer.Stream.Update("bcast-2", "title", nil, nil, 5)

	pool := &WebSocketPool{streamers: []*models.Streamer{streamer}}
	pool.handleMessage(pointsEarnedMsg(t, "123456", "WATCH_STREAK", 450, 231450))

	if streamer.History[ReasonWatchStreak] == nil {
		t.Fatalf("WATCH_STREAK not recorded: %#v", streamer.History)
	}
	if streamer.Stream.GetWatchStreakMissing() {
		t.Error("WatchStreakMissing should be cleared for the underscore-form too")
	}
	bid, _ := streamer.Stream.StreakEarnedGrant()
	if bid != "bcast-2" {
		t.Errorf("streakEarnedBroadcastID = %q, want bcast-2", bid)
	}
}

// T7 (pool level): a plain WATCH must credit history but never earn the streak —
// no substring collision with WATCH STREAK.
func TestPoolPlainWatchDoesNotEarnStreak(t *testing.T) {
	streamer := models.NewStreamer("skill4ltu", models.DefaultStreamerSettings())
	streamer.ChannelID = "123456"
	streamer.Stream.Update("bcast-3", "title", nil, nil, 5)

	pool := &WebSocketPool{streamers: []*models.Streamer{streamer}}
	pool.handleMessage(pointsEarnedMsg(t, "123456", "WATCH", 10, 500))

	if streamer.History[ReasonWatch] == nil || streamer.History[ReasonWatch].Amount != 10 {
		t.Errorf("WATCH history = %+v, want amount 10", streamer.History[ReasonWatch])
	}
	if streamer.History[ReasonWatchStreak] != nil {
		t.Error("a plain WATCH must not create a WATCH_STREAK history entry")
	}
	if !streamer.Stream.GetWatchStreakMissing() {
		t.Error("a plain WATCH must not clear the pending watch streak")
	}
}

// T8: an unknown reason must flow through the runtime without panic and without
// any streak side effect, recorded under its own (trimmed) history key.
func TestPoolUnknownReasonPassesThrough(t *testing.T) {
	streamer := models.NewStreamer("skill4ltu", models.DefaultStreamerSettings())
	streamer.ChannelID = "123456"
	streamer.Stream.Update("bcast-4", "title", nil, nil, 5)

	pool := &WebSocketPool{streamers: []*models.Streamer{streamer}}
	pool.handleMessage(pointsEarnedMsg(t, "123456", "SOME NEW TWITCH REASON", 77, 900))

	if streamer.History["SOME NEW TWITCH REASON"] == nil {
		t.Errorf("unknown reason should get its own history key: %#v", streamer.History)
	}
	if streamer.History[ReasonWatch] != nil || streamer.History[ReasonWatchStreak] != nil {
		t.Error("an unknown reason must not be classified as WATCH or WATCH_STREAK")
	}
	if !streamer.Stream.GetWatchStreakMissing() {
		t.Error("an unknown reason must not clear the pending watch streak")
	}
	if bid, _ := streamer.Stream.StreakEarnedGrant(); bid != "" {
		t.Errorf("an unknown reason must not bind a streak grant; bid = %q", bid)
	}
}

// T6 (emitter side): the "Points earned" log carries the canonical reason, plus
// reason_raw ONLY when the wire form differed. The exact-match console/web-log
// classifiers key off reason alone, so this is what re-enables streak styling.
func TestPoolWatchStreakLogAttributes(t *testing.T) {
	logOf := func(reasonCode string) string {
		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
		defer slog.SetDefault(prev)

		streamer := models.NewStreamer("skill4ltu", models.DefaultStreamerSettings())
		streamer.ChannelID = "123456"
		streamer.Stream.Update("bcast-x", "title", nil, nil, 5)
		pool := &WebSocketPool{streamers: []*models.Streamer{streamer}}
		pool.handleMessage(pointsEarnedMsg(t, "123456", reasonCode, 450, 231450))
		return buf.String()
	}

	// Space-form: canonical reason + reason_raw carrying the wire form.
	space := logOf("WATCH STREAK")
	if !strings.Contains(space, "reason=WATCH_STREAK") {
		t.Errorf("space-form log missing canonical reason=WATCH_STREAK:\n%s", space)
	}
	if !strings.Contains(space, `reason_raw="WATCH STREAK"`) {
		t.Errorf("space-form log missing reason_raw=\"WATCH STREAK\":\n%s", space)
	}

	// Underscore-form: canonical reason, and NO redundant reason_raw.
	under := logOf("WATCH_STREAK")
	if !strings.Contains(under, "reason=WATCH_STREAK") {
		t.Errorf("underscore-form log missing reason=WATCH_STREAK:\n%s", under)
	}
	if strings.Contains(under, "reason_raw") {
		t.Errorf("underscore-form log should not emit reason_raw (raw == canonical):\n%s", under)
	}

	// Plain WATCH: reason=WATCH, no reason_raw.
	watch := logOf("WATCH")
	if !strings.Contains(watch, "reason=WATCH") || strings.Contains(watch, "reason_raw") {
		t.Errorf("plain WATCH log wrong:\n%s", watch)
	}
}
