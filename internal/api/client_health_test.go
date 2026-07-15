package api

import (
	"bytes"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/auth"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
)

// stubRoundTripper returns a fixed HTTP 200 response with the given body on
// every round trip, handing back a fresh body reader each call so the
// client-ID fallback loop (which may re-send the request under several client
// IDs on PersistedQueryNotFound) always reads a readable body.
type stubRoundTripper struct {
	body  string
	calls int
}

func (s *stubRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	s.calls++
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewBufferString(s.body)),
		Header:     make(http.Header),
	}, nil
}

// newHealthTestClient builds a TwitchClient (via the real constructor, so its
// client ID and other fields are populated as in production) whose HTTP
// transport always returns body with HTTP 200. That is enough to exercise
// postGQLRequest end-to-end (header building, GQL decode, the markSuccess
// decision) without touching the network.
func newHealthTestClient(body string) *TwitchClient {
	c := NewTwitchClient(auth.NewTwitchAuth("tester", "device"), "device")
	c.client = &http.Client{Transport: &stubRoundTripper{body: body}}
	return c
}

const healthSentinelUnchanged = "LastSuccessAt was updated, but the response carried top-level GQL errors and must not refresh the health timestamp: got %v, want %v (unchanged)"

// A GQL-layer error body (HTTP 200, top-level "errors", no data) must not
// refresh the connection-health timestamp — otherwise the watchdog/canary
// reads a GQL outage as healthy.
func TestPostGQLRequestDoesNotMarkSuccessOnTopLevelErrors(t *testing.T) {
	c := newHealthTestClient(`{"errors":[{"message":"service error"}]}`)
	sentinel := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	c.lastSuccess = sentinel

	_, _ = c.postGQLRequest(constants.Inventory)

	if got := c.LastSuccessAt(); !got.Equal(sentinel) {
		t.Fatalf(healthSentinelUnchanged, got, sentinel)
	}
}

// PersistedQueryNotFound is the specific top-level-errors case that survives
// the client-ID fallback (returned with a nil error). It must likewise not
// refresh the health timestamp.
func TestPostGQLRequestDoesNotMarkSuccessOnPQNF(t *testing.T) {
	c := newHealthTestClient(`{"errors":[{"message":"PersistedQueryNotFound"}]}`)
	sentinel := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	c.lastSuccess = sentinel

	_, _ = c.postGQLRequest(constants.Inventory)

	if got := c.LastSuccessAt(); !got.Equal(sentinel) {
		t.Fatalf(healthSentinelUnchanged, got, sentinel)
	}
}

// Positive path: a real success (data, no top-level errors) must still refresh
// the health timestamp — the fix must not break the happy path.
func TestPostGQLRequestMarksSuccessOnRealData(t *testing.T) {
	c := newHealthTestClient(`{"data":{"user":{"id":"123"}}}`)
	sentinel := time.Now().Add(-time.Hour)
	c.lastSuccess = sentinel

	if _, err := c.postGQLRequest(constants.Inventory); err != nil {
		t.Fatalf("unexpected error on a real data response: %v", err)
	}
	if got := c.LastSuccessAt(); !got.After(sentinel) {
		t.Fatalf("LastSuccessAt was not advanced on a genuine success: got %v, want after %v", got, sentinel)
	}
}

// The batch path shares the same gate; a batch entry with top-level errors must
// not refresh the health timestamp.
func TestPostGQLBatchRequestDoesNotMarkSuccessOnTopLevelErrors(t *testing.T) {
	c := newHealthTestClient(`[{"errors":[{"message":"service error"}]}]`)
	sentinel := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	c.lastSuccess = sentinel

	_, _ = c.postGQLBatchRequest([]constants.GQLOperation{constants.Inventory})

	if got := c.LastSuccessAt(); !got.Equal(sentinel) {
		t.Fatalf(healthSentinelUnchanged, got, sentinel)
	}
}

// Batch positive path: real data with no errors still refreshes the timestamp.
func TestPostGQLBatchRequestMarksSuccessOnRealData(t *testing.T) {
	c := newHealthTestClient(`[{"data":{"user":{"id":"123"}}}]`)
	sentinel := time.Now().Add(-time.Hour)
	c.lastSuccess = sentinel

	if _, err := c.postGQLBatchRequest([]constants.GQLOperation{constants.Inventory}); err != nil {
		t.Fatalf("unexpected error on a real batch data response: %v", err)
	}
	if got := c.LastSuccessAt(); !got.After(sentinel) {
		t.Fatalf("LastSuccessAt was not advanced on a genuine batch success: got %v, want after %v", got, sentinel)
	}
}
