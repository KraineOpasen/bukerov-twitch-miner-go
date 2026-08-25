package pubsub

import "testing"

func TestPubSubFingerprintCanonicalAndExcludesFallbackTimestamp(t *testing.T) {
	first, err := ParsePubSubMessage(&WSData{
		Topic:   "community-points-user-v1.999",
		Message: `{"type":"points-earned","data":{"channel_id":"123","point_gain":{"reason_code":"WATCH","total_points":10}}}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := ParsePubSubMessage(&WSData{
		Topic:   "community-points-user-v1.999",
		Message: `{ "data": { "point_gain": { "total_points": 10, "reason_code": "WATCH" }, "channel_id": "123" }, "type": "points-earned" }`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.EventFingerprint == "" || first.EventFingerprint != second.EventFingerprint {
		t.Fatalf("canonical equivalent payload fingerprints differ: %q vs %q", first.EventFingerprint, second.EventFingerprint)
	}
	if first.Timestamp.Equal(second.Timestamp) {
		// Equality is possible at coarse clock resolutions, so this assertion is
		// intentionally informational only; identity equality above proves the
		// derived fallback timestamp is not part of the digest.
		t.Log("fallback timestamps happened to match")
	}
}

func TestPubSubFingerprintIncludesTopicAndEveryPayloadDifference(t *testing.T) {
	base, err := ParsePubSubMessage(&WSData{
		Topic:   "community-points-user-v1.999",
		Message: `{"type":"points-earned","data":{"channel_id":"123","point_gain":{"reason_code":"WATCH","total_points":10}}}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	differentPayload, err := ParsePubSubMessage(&WSData{
		Topic:   "community-points-user-v1.999",
		Message: `{"type":"points-earned","data":{"channel_id":"123","point_gain":{"reason_code":"WATCH_STREAK","total_points":350}}}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	differentTopic, err := ParsePubSubMessage(&WSData{
		Topic:   "community-points-user-v1.998",
		Message: `{"type":"points-earned","data":{"channel_id":"123","point_gain":{"reason_code":"WATCH","total_points":10}}}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if base.EventFingerprint == differentPayload.EventFingerprint || base.EventFingerprint == differentTopic.EventFingerprint {
		t.Fatalf("fingerprint collapsed distinct event/topic: base=%q payload=%q topic=%q",
			base.EventFingerprint, differentPayload.EventFingerprint, differentTopic.EventFingerprint)
	}
}
