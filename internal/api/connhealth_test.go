package api

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/auth"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
)

// fakeRoundTripper returns a fixed status + body on every round trip, handing
// back a fresh body reader each call and counting calls under a mutex so it is
// safe to share across concurrent requests (the -race accounting test).
type fakeRoundTripper struct {
	status int
	body   string
	mu     sync.Mutex
	calls  int
}

func (f *fakeRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return &http.Response{
		StatusCode: f.status,
		Body:       io.NopCloser(strings.NewReader(f.body)),
		Header:     make(http.Header),
	}, nil
}

func newConnHealthClient(status int, body string) *TwitchClient {
	c := NewTwitchClient(auth.NewTwitchAuth("tester", "device"), "device")
	c.client = &http.Client{Transport: &fakeRoundTripper{status: status, body: body}}
	return c
}

// A genuine success records an attempt and a useful success, and no failures.
func TestConnHealthRecordsSuccessAndAttempt(t *testing.T) {
	c := newConnHealthClient(http.StatusOK, `{"data":{"user":{"id":"1"}}}`)
	c.lastSuccess = time.Now().Add(-time.Hour)

	if _, err := c.postGQLRequest(constants.Inventory); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	now := time.Now()
	h := c.ConnHealth(now, 5*time.Minute)
	if h.LastAttempt.IsZero() || now.Sub(h.LastAttempt) > time.Minute {
		t.Fatalf("attempt should be recorded recently, got %v", h.LastAttempt)
	}
	if now.Sub(h.LastSuccess) > time.Minute {
		t.Fatalf("useful success should be recorded recently, got %v", h.LastSuccess)
	}
	if h.RecentTransportFailures != 0 || h.RecentFunctionalFailures != 0 {
		t.Fatalf("a success must record no failures, got transport=%d functional=%d",
			h.RecentTransportFailures, h.RecentFunctionalFailures)
	}
}

// A top-level GQL error (HTTP 200, reachable, no useful data) is a FUNCTIONAL
// failure, not a transport/connectivity one, and must not refresh lastSuccess.
func TestConnHealthRecordsFunctionalFailureOnTopLevelErrors(t *testing.T) {
	c := newConnHealthClient(http.StatusOK, `{"errors":[{"message":"service error"}]}`)
	sentinel := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	c.lastSuccess = sentinel

	_, _ = c.postGQLRequest(constants.Inventory)

	now := time.Now()
	h := c.ConnHealth(now, 5*time.Minute)
	if h.RecentFunctionalFailures == 0 {
		t.Fatalf("a top-level GQL error must record a functional failure")
	}
	if h.RecentTransportFailures != 0 {
		t.Fatalf("a reachable functional error must NOT record a transport failure")
	}
	if !h.LastSuccess.Equal(sentinel) {
		t.Fatalf("functional failure must not refresh lastSuccess")
	}
	if h.LastAttempt.IsZero() {
		t.Fatalf("an attempt must still be recorded")
	}
}

// PersistedQueryNotFound across all client IDs is a protocol/hash degradation:
// reachable, functional — not a connectivity blackout.
func TestConnHealthRecordsFunctionalFailureOnPQNF(t *testing.T) {
	c := newConnHealthClient(http.StatusOK, `{"errors":[{"message":"PersistedQueryNotFound"}]}`)
	sentinel := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	c.lastSuccess = sentinel

	_, _ = c.postGQLRequest(constants.Inventory)

	now := time.Now()
	h := c.ConnHealth(now, 5*time.Minute)
	if h.RecentFunctionalFailures == 0 {
		t.Fatalf("PQNF must record a functional failure")
	}
	if h.RecentTransportFailures != 0 {
		t.Fatalf("PQNF must NOT record a transport failure")
	}
	if !h.LastSuccess.Equal(sentinel) {
		t.Fatalf("PQNF must not refresh lastSuccess")
	}
}

// K — an HTTP 401 goes through the auth lifecycle: it records an attempt but no
// transport failure and no functional failure, so it can never be mistaken for a
// network connectivity loss.
func TestConnHealthAuthErrorIsNotConnectivityFailure(t *testing.T) {
	c := newConnHealthClient(http.StatusUnauthorized, `{"error":"Unauthorized"}`)
	// Deterministic, network-free recovery that fails: no real OAuth endpoint.
	c.recoverFn = func(uint64) (auth.Snapshot, error) {
		return auth.Snapshot{}, errors.New("recovery unavailable in test")
	}
	c.lastSuccess = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	_, _ = c.postGQLRequest(constants.Inventory) // expected to fail with ErrUnauthorized

	now := time.Now()
	h := c.ConnHealth(now, 5*time.Minute)
	if h.RecentTransportFailures != 0 {
		t.Fatalf("HTTP 401 must not record a transport/connectivity failure, got %d", h.RecentTransportFailures)
	}
	if h.RecentFunctionalFailures != 0 {
		t.Fatalf("HTTP 401 must not record a functional failure (it is an auth-lifecycle event), got %d", h.RecentFunctionalFailures)
	}
	if h.LastAttempt.IsZero() {
		t.Fatalf("an attempt must still be recorded for a 401")
	}
}

// O — the API health accounting is concurrency-safe: many requests updating the
// snapshot while the watchdog reads it must be race-free under `go test -race`.
func TestConnHealthConcurrentAccess(t *testing.T) {
	c := newConnHealthClient(http.StatusOK, `{"data":{"user":{"id":"1"}}}`)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writers: request path updating attempt/success accounting.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_, _ = c.postGQLRequest(constants.Inventory)
				}
			}
		}()
	}
	// Readers: watchdog snapshotting the accounting.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = c.ConnHealth(time.Now(), 5*time.Minute)
				}
			}
		}()
	}

	time.AfterFunc(50*time.Millisecond, func() { close(stop) })
	wg.Wait()
}
