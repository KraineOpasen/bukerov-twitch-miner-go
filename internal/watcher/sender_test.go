package watcher

import (
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
)

// TestSpadeFormBodyPercentEncodesPayload guards against the form-corruption bug:
// the base64 minute-watched payload must be percent-encoded so a '+' survives
// transit instead of being decoded as a space by the spade endpoint's form
// parser. It checks a payload that deliberately contains every base64 special
// character ('+', '/', '=') round-trips exactly through a standard form parse.
func TestSpadeFormBodyPercentEncodesPayload(t *testing.T) {
	// A base64 blob exercising the characters a raw "data="+payload body would
	// mangle: '+' (space), plus '/' and '=' for good measure.
	payload := "aGVsbG8+d29ybGQ/Zm9v==" // arbitrary, contains + / =

	body := spadeFormBody(payload)

	if strings.Contains(body, "+") {
		t.Fatalf("form body must not contain a raw '+', got %q", body)
	}

	parsed, err := url.ParseQuery(body)
	if err != nil {
		t.Fatalf("form body did not parse: %v", err)
	}
	if got := parsed.Get("data"); got != payload {
		t.Fatalf("payload did not survive a form round-trip: sent %q, server sees %q", payload, got)
	}
}

// TestSpadeFormBodyRoundTripsRealPayload confirms a realistic base64-encoded
// minute-watched JSON blob decodes back to identical bytes after the form
// encode/parse cycle a real spade request goes through.
func TestSpadeFormBodyRoundTripsRealPayload(t *testing.T) {
	original := []byte(`[{"event":"minute-watched","properties":{"channel_id":"123","broadcast_id":"456","player":"site","user_id":"789","live":true,"channel":"somestreamer"}}]`)
	payload := base64.StdEncoding.EncodeToString(original)

	body := spadeFormBody(payload)

	parsed, err := url.ParseQuery(body)
	if err != nil {
		t.Fatalf("form body did not parse: %v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(parsed.Get("data"))
	if err != nil {
		t.Fatalf("server-side base64 decode failed: %v", err)
	}
	if string(decoded) != string(original) {
		t.Fatalf("payload corrupted in transit:\n sent: %s\n  got: %s", original, decoded)
	}
}
