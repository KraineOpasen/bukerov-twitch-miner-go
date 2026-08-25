package pubsub

import (
	"fmt"
	"testing"
)

func pointsEarnedWireMessage(reason string, points int, timestamp string) WSMessage {
	return WSMessage{Type: "MESSAGE", Data: &WSData{
		Topic: "community-points-user-v1.999",
		Message: fmt.Sprintf(`{"type":"points-earned","data":{"timestamp":%q,"channel_id":"123456","point_gain":{"reason_code":%q,"total_points":%d}}}`,
			timestamp, reason, points),
	}}
}

func admittedPointReasons(t *testing.T, messages ...WSMessage) []string {
	t.Helper()
	var reasons []string
	ws := NewWebSocketClient(0, nil, 3600, 1, func(msg *PubSubMessage) {
		gain, _ := msg.Data["point_gain"].(map[string]interface{})
		reason, _ := gain["reason_code"].(string)
		reasons = append(reasons, reason)
	}, nil)
	for _, msg := range messages {
		ws.handleMessage(msg)
	}
	return reasons
}

// A passive WATCH and an authoritative WATCH_STREAK are distinct domain
// events even when Twitch delivers them back-to-back on the same user topic.
func TestWebSocketAdmissionKeepsWatchThenWatchStreak(t *testing.T) {
	watch := pointsEarnedWireMessage("WATCH", 10, "2026-08-25T10:00:00Z")
	streak := pointsEarnedWireMessage("WATCH_STREAK", 350, "2026-08-25T10:00:00.100Z")

	got := admittedPointReasons(t, watch, streak)
	if len(got) != 2 || got[0] != "WATCH" || got[1] != "WATCH_STREAK" {
		t.Fatalf("admitted reasons = %v, want [WATCH WATCH_STREAK]", got)
	}
}

// An exact replay inside the transport window is still suppressed once.
func TestWebSocketAdmissionSuppressesExactReplay(t *testing.T) {
	watch := pointsEarnedWireMessage("WATCH", 10, "2026-08-25T10:00:00Z")

	got := admittedPointReasons(t, watch, watch)
	if len(got) != 1 || got[0] != "WATCH" {
		t.Fatalf("admitted reasons = %v, want one exact WATCH replay", got)
	}
}

// Replay suppression is window-scoped rather than merely adjacent: a distinct
// event between two identical frames must not make the replay new again.
func TestWebSocketAdmissionSuppressesExactReplayAcrossDifferentEvent(t *testing.T) {
	watch := pointsEarnedWireMessage("WATCH", 10, "2026-08-25T10:00:00Z")
	streak := pointsEarnedWireMessage("WATCH_STREAK", 350, "2026-08-25T10:00:00.100Z")

	got := admittedPointReasons(t, watch, streak, watch)
	if len(got) != 2 || got[0] != "WATCH" || got[1] != "WATCH_STREAK" {
		t.Fatalf("admitted reasons = %v, want [WATCH WATCH_STREAK] with replay suppressed", got)
	}
}

// Matching Type/Topic/ChannelID is not enough to prove replay: payload
// differences must reach the domain owner.
func TestWebSocketAdmissionKeepsDifferentWatchPayloads(t *testing.T) {
	first := pointsEarnedWireMessage("WATCH", 10, "2026-08-25T10:00:00Z")
	second := pointsEarnedWireMessage("WATCH", 20, "2026-08-25T10:00:00.100Z")

	got := admittedPointReasons(t, first, second)
	if len(got) != 2 {
		t.Fatalf("admitted reasons = %v, want two distinct WATCH payloads", got)
	}
}
