package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// TestClassifyCheck is the pure classifier that enforces the core policy: only
// nil is online, only ErrStreamerIsOffline is offline, and every other error is
// UNKNOWN with a specific reason (never a false offline).
func TestClassifyCheck(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus models.StreamerStatus
		wantReason models.StatusReason
	}{
		{"nil -> online", nil, models.StatusOnline, ""},
		{"offline sentinel -> offline", ErrStreamerIsOffline, models.StatusOffline, ""},
		{"wrapped offline -> offline", fmt.Errorf("ctx: %w", ErrStreamerIsOffline), models.StatusOffline, ""},
		{"PQNF -> unknown/pqnf", ErrPersistedQueryNotFound, models.StatusUnknown, models.ReasonPersistedQueryMissing},
		{"unauthorized -> unknown/unauth", ErrUnauthorized, models.StatusUnknown, models.ReasonUnauthorized},
		{"context canceled -> unknown/timeout", context.Canceled, models.StatusUnknown, models.ReasonTimeout},
		{"deadline exceeded -> unknown/timeout", context.DeadlineExceeded, models.StatusUnknown, models.ReasonTimeout},
		{"malformed StreamCheckError -> unknown/malformed", &StreamCheckError{Reason: models.ReasonMalformedResponse}, models.StatusUnknown, models.ReasonMalformedResponse},
		{"graphql StreamCheckError -> unknown/graphql", &StreamCheckError{Reason: models.ReasonGraphQLError}, models.StatusUnknown, models.ReasonGraphQLError},
		{"net timeout -> unknown/timeout", &net.DNSError{IsTimeout: true}, models.StatusUnknown, models.ReasonTimeout},
		{"generic transport -> unknown/transport", errors.New("connection refused"), models.StatusUnknown, models.ReasonTransportError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotStatus, gotReason := classifyCheck(tc.err)
			if gotStatus != tc.wantStatus {
				t.Errorf("status = %v, want %v", gotStatus, tc.wantStatus)
			}
			if gotReason != tc.wantReason {
				t.Errorf("reason = %q, want %q", gotReason, tc.wantReason)
			}
		})
	}
}

// TestGetStreamInfoClassification drives GetStreamInfo with fixture GQL bodies and
// asserts the tri-state classification of its error via classifyCheck. This is the
// crux of BKM-002: only an explicit "stream": null is offline; every malformed or
// absent structural field is UNKNOWN, never a false offline.
func TestGetStreamInfoClassification(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus models.StreamerStatus
		wantReason models.StatusReason // only checked when unknown
	}{
		{"valid stream object -> online", `{"data":{"user":{"stream":{"id":"b1","viewersCount":10}}}}`, models.StatusOnline, ""},
		{"explicit stream null -> offline", `{"data":{"user":{"stream":null}}}`, models.StatusOffline, ""},
		{"missing data -> unknown", `{}`, models.StatusUnknown, models.ReasonMalformedResponse},
		{"data null -> unknown", `{"data":null}`, models.StatusUnknown, models.ReasonMalformedResponse},
		{"malformed data type -> unknown", `{"data":"oops"}`, models.StatusUnknown, models.ReasonMalformedResponse},
		{"missing user -> unknown", `{"data":{}}`, models.StatusUnknown, models.ReasonMalformedResponse},
		{"user null -> unknown", `{"data":{"user":null}}`, models.StatusUnknown, models.ReasonMalformedResponse},
		{"malformed user type -> unknown", `{"data":{"user":"x"}}`, models.StatusUnknown, models.ReasonMalformedResponse},
		{"missing stream field -> unknown", `{"data":{"user":{"id":"1"}}}`, models.StatusUnknown, models.ReasonMalformedResponse},
		{"malformed stream type -> unknown", `{"data":{"user":{"stream":"x"}}}`, models.StatusUnknown, models.ReasonMalformedResponse},
		{"top-level graphql errors -> unknown", `{"errors":[{"message":"boom"}],"data":null}`, models.StatusUnknown, models.ReasonGraphQLError},
		{"empty body -> unknown", ``, models.StatusUnknown, models.ReasonTransportError},
		{"invalid json -> unknown", `{not json`, models.StatusUnknown, models.ReasonTransportError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tc.body))
			})
			s := newTestStreamer("classify")

			_, err := c.GetStreamInfo(s)
			gotStatus, gotReason := classifyCheck(err)
			if gotStatus != tc.wantStatus {
				t.Fatalf("status = %v, want %v (err=%v)", gotStatus, tc.wantStatus, err)
			}
			if tc.wantStatus == models.StatusUnknown && gotReason != tc.wantReason {
				t.Errorf("reason = %q, want %q (err=%v)", gotReason, tc.wantReason, err)
			}
			// The authoritative-offline sentinel must be reserved for present-null.
			if tc.wantStatus == models.StatusOffline && !errors.Is(err, ErrStreamerIsOffline) {
				t.Errorf("offline case must return ErrStreamerIsOffline, got %v", err)
			}
			if tc.wantStatus != models.StatusOffline && errors.Is(err, ErrStreamerIsOffline) {
				t.Errorf("non-offline case must NOT return ErrStreamerIsOffline, got %v", err)
			}
		})
	}
}

// TestUnauthorizedClassifiesUnknown drives a 401 through GetStreamInfo and asserts
// it maps to UNKNOWN/unauthorized, never offline.
func TestUnauthorizedClassifiesUnknown(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"Unauthorized"}`))
	})
	s := newTestStreamer("unauth")
	_, err := c.GetStreamInfo(s)
	status, reason := classifyCheck(err)
	if status != models.StatusUnknown {
		t.Fatalf("status = %v, want unknown (err=%v)", status, err)
	}
	if reason != models.ReasonUnauthorized {
		t.Errorf("reason = %q, want unauthorized", reason)
	}
}
