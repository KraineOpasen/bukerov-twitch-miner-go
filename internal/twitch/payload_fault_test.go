package twitch

import (
	"io"
	"net/http"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/auth"
)

// --- production wiring of an unbuildable minute-watched payload -------------
//
// doRefreshPlaybackSession is the ONLY production constructor of the beacon
// payload. When it cannot build one (the authenticated viewer identity is not a
// usable numeric id, or the observed watch identity is incomplete) the refresh
// must publish the rest of the session, publish NO payload, and CLEAR any payload
// the session was still carrying — so the minute sender fails closed at its
// session-snapshot gate instead of beaconing an identity Twitch will not credit.

const faultStreamInfoBody = `{"data":{"user":{"stream":{"id":"b-new","viewersCount":3},"broadcastSettings":{"title":"t","game":{"id":"g1","name":"GameX"}}}}}`

// clientWithUserID builds a test client whose auth carries the given user id.
// An empty id models an account whose identity was never confirmed.
func clientWithUserID(t *testing.T, userID string, handler http.HandlerFunc) *TwitchClient {
	t.Helper()
	c := newTestClient(t, handler)
	a := auth.NewTwitchAuth("tester", "device-id")
	a.ReplaceCredentials(auth.TokenResponse{AccessToken: "dummy-token"})
	if userID != "" {
		a.SetUserID(userID)
	}
	c.auth = a
	return c
}

func faultStreamInfoHandler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, faultStreamInfoBody)
	}
}

// TestRefreshPublishesNoPayloadWithoutAUsableUserID: with no confirmed viewer
// identity the refresh still succeeds and publishes the broadcast, but no beacon
// payload is published — a beacon with a coerced user_id is never possible.
func TestRefreshPublishesNoPayloadWithoutAUsableUserID(t *testing.T) {
	c := clientWithUserID(t, "", faultStreamInfoHandler(t))
	s := newTestStreamer("cyganzor")
	s.ChannelID = "123456"

	if err := c.UpdateStream(s); err != nil {
		t.Fatalf("an unusable viewer identity must not fail the stream refresh: %v", err)
	}
	if got := s.Stream.GetBroadcastID(); got != "b-new" {
		t.Fatalf("the observed broadcast must still be published, got %q", got)
	}
	if s.Stream.SessionSnapshot().HasPayload() {
		t.Fatal("no beacon payload may be published without a usable viewer identity")
	}
}

// TestRefreshClearsAStalePayloadWhenTheIdentityBecomesUnusable is the regression
// that matters most: a session that ALREADY had a good payload must not keep
// replaying it once the refresh proves the identity can no longer be encoded.
func TestRefreshClearsAStalePayloadWhenTheIdentityBecomesUnusable(t *testing.T) {
	c := clientWithUserID(t, "", faultStreamInfoHandler(t))
	s := newTestStreamer("cyganzor")
	s.ChannelID = "123456"

	// Seed a perfectly good payload from an earlier, healthy session.
	if err := s.Stream.SetPayload("123456", "b-old", "44322889", "cyganzor", nil, nil); err != nil {
		t.Fatalf("seeding a valid payload must succeed: %v", err)
	}
	if !s.Stream.SessionSnapshot().HasPayload() {
		t.Fatal("fixture: the seeded payload must be present")
	}

	if err := c.UpdateStream(s); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if s.Stream.SessionSnapshot().HasPayload() {
		t.Fatal("a refresh that cannot build a payload must CLEAR the old one, not keep beaconing it")
	}
}

// TestRefreshPublishesAPayloadWithAUsableUserID is the positive control: the
// same path with a valid numeric identity publishes a payload carrying the newly
// observed broadcast.
func TestRefreshPublishesAPayloadWithAUsableUserID(t *testing.T) {
	c := clientWithUserID(t, "44322889", faultStreamInfoHandler(t))
	s := newTestStreamer("cyganzor")
	s.ChannelID = "123456"

	if err := c.UpdateStream(s); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	snap := s.Stream.SessionSnapshot()
	if !snap.HasPayload() {
		t.Fatal("a healthy identity must publish a beacon payload")
	}
	if got := payloadBroadcastID(t, snap); got != "b-new" {
		t.Fatalf("the published payload must carry the observed broadcast, got %q", got)
	}
}

// TestRefreshPublishesNoPayloadWithoutAChannelID covers the watched-identity half
// of the same rule: an unresolved channel id cannot produce a creditable beacon.
func TestRefreshPublishesNoPayloadWithoutAChannelID(t *testing.T) {
	c := clientWithUserID(t, "44322889", faultStreamInfoHandler(t))
	s := newTestStreamer("cyganzor")
	s.ChannelID = "" // never resolved

	if err := c.UpdateStream(s); err != nil {
		t.Fatalf("an unresolved channel id must not fail the stream refresh: %v", err)
	}
	if s.Stream.SessionSnapshot().HasPayload() {
		t.Fatal("no beacon payload may be published without a channel id")
	}
	if got := s.Stream.GetBroadcastID(); got != "b-new" {
		t.Fatalf("the observed broadcast must still be published, got %q", got)
	}
}
