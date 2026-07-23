package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- D. Single-flight refresh ---

// D1-D6: many concurrent callers rejected on the same generation produce
// exactly one refresh HTTP request; all observe the same new generation and
// token; the old one-time refresh token is presented exactly once and the
// rotated one is persisted.
func TestConcurrentRecoverSingleRefresh(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"
	a.userID = "uid-1"

	const callers = 100
	var wg sync.WaitGroup
	snaps := make([]Snapshot, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			snaps[i], errs[i] = a.Recover(context.Background(), 0)
		}()
	}
	close(start)
	wg.Wait()

	_, _, refreshHits, _ := f.counts()
	if refreshHits != 1 {
		t.Fatalf("refresh HTTP requests = %d, want exactly 1", refreshHits)
	}
	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: unexpected error %v", i, errs[i])
		}
		if snaps[i].Generation != 1 {
			t.Fatalf("caller %d: generation = %d, want 1", i, snaps[i].Generation)
		}
		if snaps[i].AccessToken != "test-access-2" {
			t.Fatalf("caller %d saw a different access token snapshot", i)
		}
	}

	f.mu.Lock()
	seen := append([]string(nil), f.refreshTokensSeen...)
	f.mu.Unlock()
	if len(seen) != 1 || seen[0] != "test-refresh-1" {
		t.Fatalf("old refresh token not used exactly once: %d uses", len(seen))
	}

	// D6: the rotated refresh token is the one persisted.
	if body := readCookieFile(t, a); !strings.Contains(body, "test-refresh-2") {
		t.Fatalf("rotated refresh token not persisted")
	}
	if a.Health().HasRefreshToken != true {
		t.Fatalf("runtime state lost the rotated refresh token")
	}
}

// D7 / M3: a stale rejection carrying an older generation after a rotation
// returns the current credentials without any second refresh.
func TestStale401AfterRotationDoesNotRefreshAgain(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"

	if _, err := a.Recover(context.Background(), 0); err != nil {
		t.Fatalf("first recover: %v", err)
	}

	snap, err := a.Recover(context.Background(), 0) // stale: gen 0 already rotated to 1
	if err != nil {
		t.Fatalf("stale recover: %v", err)
	}
	if snap.Generation != 1 || snap.AccessToken != "test-access-2" {
		t.Fatalf("stale recover did not return current credentials: %+v", snap.Generation)
	}
	if _, _, refreshHits, _ := f.counts(); refreshHits != 1 {
		t.Fatalf("stale 401 triggered a second refresh: %d", refreshHits)
	}
}

// D8: a context-cancelled waiter exits while the owner keeps recovering.
func TestCancelledWaiterExitsOwnerFinishes(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"

	release := make(chan struct{})
	f.mu.Lock()
	f.refreshHandler = func(w http.ResponseWriter, r *http.Request) {
		<-release
		f.writeJSON(w, 200, TokenResponse{
			AccessToken: "test-access-2", RefreshToken: "test-refresh-2",
			ExpiresIn: 14000, Scope: requiredScopes(), TokenType: "bearer",
		})
	}
	f.mu.Unlock()

	ownerDone := make(chan error, 1)
	go func() {
		_, err := a.Recover(context.Background(), 0)
		ownerDone <- err
	}()

	// Wait until the owner's flight is registered so the waiter joins it.
	for {
		a.mu.Lock()
		inFlight := a.recovering
		a.mu.Unlock()
		if inFlight {
			break
		}
	}

	waiterCtx, cancel := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() {
		_, err := a.Recover(waiterCtx, 0)
		waiterDone <- err
	}()
	cancel()
	if err := <-waiterDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter error = %v, want context.Canceled", err)
	}

	close(release)
	if err := <-ownerDone; err != nil {
		t.Fatalf("owner failed after waiter cancellation: %v", err)
	}
	if a.Generation() != 1 {
		t.Fatalf("owner did not publish after waiter cancellation")
	}
}

// D9-D10: an owner failure wakes all waiters with one error and clears the
// flight, so a later independent attempt becomes a fresh owner (once the
// P2-C3 backoff window for the failed generation has elapsed).
func TestOwnerFailureWakesWaitersAndAllowsNewOwner(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	clock := newTestClock()
	a.now = clock.Now
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"

	f.mu.Lock()
	f.refreshHandler = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }
	f.mu.Unlock()

	const callers = 20
	var wg sync.WaitGroup
	var failures atomic.Int64
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.Recover(context.Background(), 0); err != nil {
				failures.Add(1)
			}
		}()
	}
	wg.Wait()
	if failures.Load() != callers {
		t.Fatalf("failures = %d, want all %d waiters to observe the owner error", failures.Load(), callers)
	}
	if _, _, _, _ = f.counts(); a.Generation() != 0 {
		t.Fatalf("failed recovery must not publish a generation")
	}

	// Past the backoff window, the next independent attempt becomes a new
	// owner and succeeds.
	clock.Advance(time.Hour)
	f.mu.Lock()
	f.refreshHandler = nil
	f.mu.Unlock()
	if _, err := a.Recover(context.Background(), 0); err != nil {
		t.Fatalf("new owner after failure: %v", err)
	}
	if a.Generation() != 1 {
		t.Fatalf("new owner did not publish")
	}
}

// --- E. Refresh classification ---

// E2/E3 / M5: transient refresh failures never fall back to device flow and
// never destroy stored credentials.
func TestRefreshTransientFailureNoDeviceFlow(t *testing.T) {
	for _, status := range []int{429, 500, 503} {
		f := newFakeOAuth(t)
		a := newLifecycleAuth(t, f)
		a.token = "test-access-1"
		a.refreshToken = "test-refresh-1"
		if err := a.SaveAuth(); err != nil {
			t.Fatalf("seed save: %v", err)
		}

		f.mu.Lock()
		f.refreshHandler = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }
		f.mu.Unlock()

		_, err := a.Recover(context.Background(), 0)
		if err == nil {
			t.Fatalf("status %d: expected a retryable recovery error", status)
		}
		if !errors.Is(err, ErrAuthTransient) {
			t.Fatalf("status %d: error not classified transient: %v", status, err)
		}
		if device, _, _, _ := f.counts(); device != 0 {
			t.Fatalf("status %d: transient refresh failure started a device flow", status)
		}
		if a.GetAuthToken() != "test-access-1" || a.Health().HasRefreshToken != true {
			t.Fatalf("status %d: transient failure destroyed in-memory credentials", status)
		}
		if body := readCookieFile(t, a); !strings.Contains(body, "test-refresh-1") {
			t.Fatalf("status %d: transient failure destroyed stored credentials", status)
		}
	}
}

// TestRefreshTransportFailureNoDeviceFlow covers the connection-level variant
// of E2 (endpoint unreachable).
func TestRefreshTransportFailureNoDeviceFlow(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"
	a.tokenURL = "http://127.0.0.1:1/token" // nothing listens here

	_, err := a.Recover(context.Background(), 0)
	if !errors.Is(err, ErrAuthTransient) {
		t.Fatalf("transport failure not classified transient: %v", err)
	}
	if device, _, _, _ := f.counts(); device != 0 {
		t.Fatalf("transport failure started a device flow")
	}
}

// E4 / M5-inverse: only the documented authoritative rejection falls back to
// device flow — and exactly one device flow runs.
func TestRefreshAuthoritativeInvalidFallsBackToOneDeviceFlow(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-dead"

	f.mu.Lock()
	f.refreshHandler = func(w http.ResponseWriter, r *http.Request) {
		f.oauthError(w, 400, "Invalid refresh token")
	}
	f.mu.Unlock()

	snap, err := a.Recover(context.Background(), 0)
	if err != nil {
		t.Fatalf("recover via device flow: %v", err)
	}
	if snap.AccessToken != "test-access-df" {
		t.Fatalf("device-flow credentials not published")
	}
	device, deviceGrant, refresh, _ := f.counts()
	if device != 1 {
		t.Fatalf("device flows = %d, want exactly 1", device)
	}
	if refresh != 1 || deviceGrant < 1 {
		t.Fatalf("unexpected endpoint traffic: refresh=%d deviceGrant=%d", refresh, deviceGrant)
	}
}

// E5: a malformed refresh success without an access token publishes nothing.
func TestRefreshMalformedNoAccessTokenNotPublished(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"

	f.mu.Lock()
	f.refreshHandler = func(w http.ResponseWriter, r *http.Request) {
		f.writeJSON(w, 200, TokenResponse{RefreshToken: "test-refresh-2"})
	}
	f.mu.Unlock()

	_, err := a.Recover(context.Background(), 0)
	if !errors.Is(err, ErrAuthProtocol) {
		t.Fatalf("malformed success not a protocol error: %v", err)
	}
	if a.Generation() != 0 || a.GetAuthToken() != "test-access-1" {
		t.Fatalf("partial credentials were published")
	}
}

// E6: a malformed success without a replacement refresh token must not reuse
// the (possibly consumed) old one-time token: the next recovery goes straight
// to device flow.
func TestRefreshMalformedNoReplacementDropsOldOneTimeToken(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	clock := newTestClock()
	a.now = clock.Now
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"

	f.mu.Lock()
	f.refreshHandler = func(w http.ResponseWriter, r *http.Request) {
		f.writeJSON(w, 200, TokenResponse{AccessToken: "test-access-2"})
	}
	f.mu.Unlock()

	if _, err := a.Recover(context.Background(), 0); !errors.Is(err, ErrAuthProtocol) {
		t.Fatalf("malformed success not a protocol error: %v", err)
	}
	if a.Health().HasRefreshToken {
		t.Fatalf("possibly-consumed one-time refresh token retained")
	}

	clock.Advance(time.Hour) // past the P2-C3 backoff window
	if _, err := a.Recover(context.Background(), 0); err != nil {
		t.Fatalf("second recover: %v", err)
	}
	_, _, refreshHits, _ := f.counts()
	if refreshHits != 1 {
		t.Fatalf("old one-time refresh token was presented again: %d refresh calls", refreshHits)
	}
	if device, _, _, _ := f.counts(); device != 1 {
		t.Fatalf("second recovery should run exactly one device flow, got %d", device)
	}
}

// E7: no refresh token, access token, or response body ever appears in
// recovery errors.
func TestRefreshErrorsCarryNoSecrets(t *testing.T) {
	secretBody := `{"access_token":"SECRET-ACCESS","refresh_token":"SECRET-REFRESH"}`
	for name, handler := range map[string]http.HandlerFunc{
		"transient500": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(secretBody))
		},
		"invalid400": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(secretBody))
		},
		"malformed200": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"access_token":""}`))
		},
	} {
		f := newFakeOAuth(t)
		a := newLifecycleAuth(t, f)
		a.token = "runtime-access-token-secret"
		a.refreshToken = "runtime-refresh-token-secret"
		f.mu.Lock()
		f.refreshHandler = handler
		// Terminal device-flow failure keeps the invalid400 case's error local.
		f.tokenHandler = func(w http.ResponseWriter, r *http.Request) { f.oauthError(w, 400, "access_denied") }
		f.mu.Unlock()

		_, err := a.Recover(context.Background(), 0)
		if err == nil {
			t.Fatalf("%s: expected error", name)
		}
		msg := err.Error()
		for _, secret := range []string{"SECRET-ACCESS", "SECRET-REFRESH", "runtime-access-token-secret", "runtime-refresh-token-secret"} {
			if strings.Contains(msg, secret) {
				t.Fatalf("%s: error leaks secret material: %q", name, msg)
			}
		}
	}
}

// E1 is the plain success path exercised standalone (also via D1).
func TestRefreshSuccessRotatesPair(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"

	snap, err := a.Recover(context.Background(), 0)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if snap.AccessToken != "test-access-2" || snap.Generation != 1 {
		t.Fatalf("pair not rotated: %+v", snap)
	}
	if device, _, _, _ := f.counts(); device != 0 {
		t.Fatalf("refresh success must not run a device flow")
	}
}
