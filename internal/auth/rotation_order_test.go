package auth

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Regression: the rotated pair must be ON DISK before rotation consumers are
// notified — a consumer sweep doing network I/O must never sit inside the
// crash window between Twitch consuming the old one-time refresh token and
// the new pair being persisted.
func TestRotationPersistsBeforeCallback(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"

	type observed struct {
		fileHasNewPair bool
	}
	seen := make(chan observed, 1)
	a.SetRotationCallback(func(uint64) {
		body, _ := os.ReadFile(a.cookiesPath())
		seen <- observed{fileHasNewPair: strings.Contains(string(body), "test-refresh-2")}
	})

	if _, err := a.Recover(context.Background(), 0); err != nil {
		t.Fatalf("recover: %v", err)
	}
	select {
	case got := <-seen:
		if !got.fileHasNewPair {
			t.Fatalf("rotation callback observed a disk state WITHOUT the rotated pair (persist must precede notification)")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("rotation callback never fired")
	}
}

// Regression: a slow rotation consumer must not block the recovery flight's
// completion (waiters are released by the flight, not by the consumer sweep).
func TestSlowRotationCallbackDoesNotBlockRecovery(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"

	release := make(chan struct{})
	a.SetRotationCallback(func(uint64) { <-release })
	defer close(release)

	done := make(chan error, 1)
	go func() {
		_, err := a.Recover(context.Background(), 0)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("recover: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("a blocked rotation consumer stalled the recovery flight")
	}
}

// Regression: dropping a possibly-consumed refresh token (malformed refresh
// success) must be durable — a reload from disk must not resurrect it.
func TestMalformedRefreshDropIsPersisted(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"
	if err := a.SaveAuth(); err != nil {
		t.Fatalf("seed: %v", err)
	}

	f.mu.Lock()
	f.refreshHandler = func(w http.ResponseWriter, r *http.Request) {
		f.writeJSON(w, 200, TokenResponse{AccessToken: "test-access-2"})
	}
	f.mu.Unlock()

	if _, err := a.Recover(context.Background(), 0); err == nil {
		t.Fatalf("expected malformed refresh to fail")
	}

	fresh := NewTwitchAuth("tester", "device-xyz")
	if err := fresh.LoadStoredAuth(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if fresh.Health().HasRefreshToken {
		t.Fatalf("possibly-consumed one-time refresh token resurrected from disk")
	}
}

// Regression: a validation that raced a rotation must not stamp the OLD
// token's metadata over the new credential set.
func TestStaleValidationDoesNotStampNewGeneration(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.userID = "uid-1"

	// Simulate: validation started at generation 0, rotation to generation 1
	// happened mid-flight, then the stale validation result applies.
	a.publishTokenPair(&TokenResponse{
		AccessToken: "test-access-2", RefreshToken: "test-refresh-2",
		ExpiresIn: 14000, TokenType: "bearer",
	})
	before := a.Health()

	st, err := a.applyValidation(validateResponse{
		ClientID: a.clientID, Login: "tester",
		Scopes: requiredScopes(), UserID: "uid-1", ExpiresIn: 1, // 1s — would visibly shrink expiry
	}, 0 /* the stale generation the validated token belonged to */)
	if err != nil || st != ValidateStatusValid {
		t.Fatalf("stale apply: %v %v", st, err)
	}

	after := a.Health()
	if !after.ExpiresAt.Equal(before.ExpiresAt) || !after.ValidatedAt.Equal(before.ValidatedAt) {
		t.Fatalf("stale validation stamped metadata over the newer generation")
	}
}

// Regression: a crash-orphaned temp file (with secret content) belonging to
// THIS profile is swept at the next startup.
func TestLoginSweepsStaleTempFiles(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.userID = "uid-1"
	if err := a.SaveAuth(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	stale := "cookies/" + ownTempPrefix(a.cookiesPath()) + "crashed123.tmp"
	if err := os.WriteFile(stale, []byte(`{"auth_token":"orphaned"}`), 0600); err != nil {
		t.Fatalf("plant stale temp: %v", err)
	}

	if err := a.Login(context.Background()); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale temp file survived startup")
	}
}
