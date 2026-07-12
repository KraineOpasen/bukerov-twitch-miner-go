package pubsub

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// TestPointsEarnedWatchStreakCreditedAndLogged feeds a realistic
// community-points-user-v1 points-earned message carrying
// point_gain.reason_code = WATCH_STREAK through the real parse -> route ->
// handle path and asserts it is credited and classified. This is the shape the
// reference Python miners (Tkd-Alex / rdavydov) read - message.data
// ["point_gain"]["reason_code"] and ["total_points"] - so this test locks in
// that the Go handler processes an identical payload correctly. A missing
// WATCH_STREAK in production therefore means the event never arrived (the
// streak was never earned), not that it arrived and was dropped here.
func TestPointsEarnedWatchStreakCreditedAndLogged(t *testing.T) {
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
	if streamer.Stream.GetWatchStreakMissing() {
		t.Errorf("WatchStreakMissing should be cleared once the streak is credited")
	}
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
