package pubsub

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// TestPointsEarnedWatchStreakCreditedAndLoggedUnbound feeds a realistic
// community-points-user-v1 points-earned message carrying
// point_gain.reason_code = WATCH_STREAK through the real parse -> route ->
// handle path and asserts it is credited and classified. This is the shape the
// reference Python miners (Tkd-Alex / rdavydov) read - message.data
// ["point_gain"]["reason_code"] and ["total_points"] - so this test locks in
// that the Go handler processes an identical payload correctly. A missing
// WATCH_STREAK in production therefore means the event never arrived (the
// streak was never earned), not that it arrived and was dropped here.
func TestPointsEarnedWatchStreakCreditedAndLoggedUnbound(t *testing.T) {
	streamer := models.NewStreamer("skill4ltu", models.DefaultStreamerSettings())
	streamer.ChannelID = "123456"
	// A fresh broadcast starts with the streak pending.
	if !streamer.Stream.GetWatchStreakMissing() {
		t.Fatalf("precondition: expected a fresh stream to have the streak pending")
	}

	pool := &WebSocketPool{streamers: []*models.Streamer{streamer}}

	// The topic key is the *user* id; the channel the points were earned on
	// lives in data.channel_id and must drive routing to the streamer.
	raw := `{
		"type": "points-earned",
		"data": {
			"timestamp": "2026-07-12T10:00:00.000000000Z",
			"channel_id": "123456",
			"point_gain": {
				"user_id": "999",
				"channel_id": "123456",
				"total_points": 450,
				"baseline_points": 450,
				"reason_code": "WATCH_STREAK",
				"multipliers": []
			},
			"balance": {
				"user_id": "999",
				"channel_id": "123456",
				"balance": 231450
			}
		}
	}`

	msg, err := ParsePubSubMessage(&WSData{
		Topic:   "community-points-user-v1.999",
		Message: raw,
	})
	if err != nil {
		t.Fatalf("ParsePubSubMessage: %v", err)
	}
	if msg.Type != "points-earned" {
		t.Fatalf("message type = %q, want points-earned", msg.Type)
	}
	if msg.ChannelID != "123456" {
		t.Fatalf("routed channel id = %q, want 123456 (data.channel_id, not the user-id topic key)", msg.ChannelID)
	}

	pool.handleMessage(msg)

	if got := streamer.GetChannelPoints(); got != 231450 {
		t.Errorf("channel points = %d, want 231450 (balance not applied)", got)
	}
	entry, ok := streamer.History["WATCH_STREAK"]
	if !ok {
		t.Fatalf("WATCH_STREAK not recorded in history: %#v", streamer.History)
	}
	if entry.Amount != 450 || entry.Counter != 1 {
		t.Errorf("WATCH_STREAK history = %+v, want {Counter:1 Amount:450}", entry)
	}
	if !streamer.Stream.GetWatchStreakMissing() {
		t.Error("an unbound WATCH_STREAK must not be guessed onto an unidentified/current broadcast")
	}
	persisted := streamer.Stream.WatchStreakPersistence()
	if len(persisted.Grants) != 1 || persisted.Grants[0].Binding != models.WatchStreakGrantUnbound || persisted.Grants[0].BroadcastID != "" {
		t.Fatalf("grant ledger = %+v, want one explicit GRANTED_UNBOUND fact", persisted.Grants)
	}
}

func parsedWatch(t *testing.T, timestamp string, balance int) *PubSubMessage {
	t.Helper()
	raw := fmt.Sprintf(`{
		"type":"points-earned",
		"data":{
			"timestamp":%q,
			"channel_id":"123456",
			"point_gain":{"total_points":10,"reason_code":"WATCH"},
			"balance":{"channel_id":"123456","balance":%d}
		}
	}`, timestamp, balance)
	msg, err := ParsePubSubMessage(&WSData{Topic: "community-points-user-v1.999", Message: raw})
	if err != nil {
		t.Fatalf("ParsePubSubMessage: %v", err)
	}
	return msg
}

// TestPointsEarnedPlainWatchStillCredited guards that ordinary passive WATCH
// gains (the dominant reason code) are still credited to the balance and
// history, so the WATCH_STREAK-specific handling above didn't regress the
// common path.
func TestPointsEarnedPlainWatchStillCredited(t *testing.T) {
	streamer := models.NewStreamer("skill4ltu", models.DefaultStreamerSettings())
	streamer.ChannelID = "123456"

	pool := &WebSocketPool{streamers: []*models.Streamer{streamer}}

	raw := `{
		"type": "points-earned",
		"data": {
			"channel_id": "123456",
			"point_gain": {"total_points": 10, "reason_code": "WATCH"},
			"balance": {"channel_id": "123456", "balance": 500}
		}
	}`

	msg, err := ParsePubSubMessage(&WSData{Topic: "community-points-user-v1.999", Message: raw})
	if err != nil {
		t.Fatalf("ParsePubSubMessage: %v", err)
	}
	pool.handleMessage(msg)

	if got := streamer.GetChannelPoints(); got != 500 {
		t.Errorf("channel points = %d, want 500", got)
	}
	if entry := streamer.History["WATCH"]; entry == nil || entry.Amount != 10 {
		t.Errorf("WATCH history = %+v, want amount 10", entry)
	}
	// A plain WATCH must not clear the streak-pending flag.
	if !streamer.Stream.GetWatchStreakMissing() {
		t.Errorf("plain WATCH should not clear the pending watch streak")
	}
}

func parsedWatchStreak(t *testing.T, timestamp string, balance int) *PubSubMessage {
	t.Helper()
	raw := fmt.Sprintf(`{
		"type":"points-earned",
		"data":{
			"timestamp":%q,
			"channel_id":"123456",
			"point_gain":{"total_points":350,"reason_code":"WATCH_STREAK"},
			"balance":{"channel_id":"123456","balance":%d}
		}
	}`, timestamp, balance)
	msg, err := ParsePubSubMessage(&WSData{Topic: "community-points-user-v1.999", Message: raw})
	if err != nil {
		t.Fatalf("ParsePubSubMessage: %v", err)
	}
	return msg
}

// Domain idempotency is independent of the transport replay window: an exact
// WATCH_STREAK replay reaching the pool must not count history twice.
func TestWatchStreakExactReplayCountsHistoryOnce(t *testing.T) {
	streamer := models.NewStreamer("skill4ltu", models.DefaultStreamerSettings())
	streamer.ChannelID = "123456"
	streamer.Stream.Update("broadcast-1", "title", nil, nil, 1)
	pool := &WebSocketPool{streamers: []*models.Streamer{streamer}}
	msg := parsedWatchStreak(t, "2026-08-25T09:00:00Z", 1000)

	pool.handleMessage(msg)
	pool.handleMessage(msg)

	entry := streamer.History["WATCH_STREAK"]
	if entry == nil || entry.Counter != 1 || entry.Amount != 350 {
		t.Fatalf("exact replay history = %+v, want one +350 grant", entry)
	}
}

func TestWatchStreakReplayCannotRollbackNewerBalance(t *testing.T) {
	streamer := models.NewStreamer("skill4ltu", models.DefaultStreamerSettings())
	streamer.ChannelID = "123456"
	pool := &WebSocketPool{streamers: []*models.Streamer{streamer}}
	streak := parsedWatchStreak(t, "2026-08-25T09:00:00Z", 1000)

	pool.handleMessage(streak)
	pool.handleMessage(parsedWatch(t, "2026-08-25T09:01:00Z", 1010))
	pool.handleMessage(streak)

	if got := streamer.GetChannelPoints(); got != 1010 {
		t.Fatalf("stale streak replay rolled balance back to %d, want 1010", got)
	}
	if entry := streamer.History["WATCH_STREAK"]; entry == nil || entry.Counter != 1 {
		t.Fatalf("streak history after replay=%+v, want one", entry)
	}
}

func TestWatchStreakConcurrentExactReplayCountsOnce(t *testing.T) {
	streamer := models.NewStreamer("skill4ltu", models.DefaultStreamerSettings())
	streamer.ChannelID = "123456"
	pool := &WebSocketPool{streamers: []*models.Streamer{streamer}}
	msg := parsedWatchStreak(t, "2026-08-25T09:00:00Z", 1000)

	const workers = 32
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			pool.handleMessage(msg)
		}()
	}
	wg.Wait()

	if entry := streamer.History["WATCH_STREAK"]; entry == nil || entry.Counter != 1 || entry.Amount != 350 {
		t.Fatalf("concurrent replay history=%+v, want one +350", entry)
	}
	if got := streamer.Stream.WatchStreakPersistence(); got.Revision != 1 || len(got.Grants) != 1 {
		t.Fatalf("concurrent replay ledger=%+v, want one revision/fact", got)
	}
}

func TestOrdinaryWatchCannotCreateGrantFact(t *testing.T) {
	streamer := models.NewStreamer("skill4ltu", models.DefaultStreamerSettings())
	streamer.ChannelID = "123456"
	streamer.Stream.Update("b1", "t", nil, nil, 1)
	pool := &WebSocketPool{streamers: []*models.Streamer{streamer}}

	pool.handleMessage(parsedWatch(t, "2026-08-25T09:00:00Z", 1000))

	if state := streamer.Stream.WatchStreakPersistence(); state.Revision != 0 || len(state.Grants) != 0 || state.Timeout != nil {
		t.Fatalf("ordinary WATCH manufactured streak terminal state: %+v", state)
	}
	if decision := streamer.Stream.EvaluateWatchStreak(time.Now()); !decision.PursuitEligible || decision.State != models.WatchStreakEligible {
		t.Fatalf("ordinary WATCH ended current pursuit: %+v", decision)
	}
}

// Timestamp/arrival order cannot bind a delayed WATCH_STREAK to whichever
// broadcast happens to be current when it arrives. It is a real unbound grant,
// while the newly observed broadcast remains independently pursuable.
func TestDelayedUnboundWatchStreakDoesNotBindCurrentBroadcast(t *testing.T) {
	streamer := models.NewStreamer("skill4ltu", models.DefaultStreamerSettings())
	streamer.ChannelID = "123456"
	streamer.Stream.Update("new-broadcast", "title", nil, nil, 1)
	pool := &WebSocketPool{streamers: []*models.Streamer{streamer}}

	pool.handleMessage(parsedWatchStreak(t, "2026-08-24T09:00:00Z", 1000))

	if !streamer.Stream.StreakPending() {
		t.Fatal("unbound delayed grant was guessed onto the current broadcast and ended its pursuit")
	}
	if bid, _ := streamer.Stream.StreakEarnedGrant(); bid != "" {
		t.Fatalf("unbound delayed grant acquired invented BroadcastID %q", bid)
	}
	entry := streamer.History["WATCH_STREAK"]
	if entry == nil || entry.Counter != 1 {
		t.Fatalf("authoritative unbound grant must still be counted once, got %+v", entry)
	}
}
