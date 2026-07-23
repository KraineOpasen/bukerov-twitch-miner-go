package auth

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// Review regression: a refresh success proves liveness, not identity — a
// foreign record whose stored user_id was manipulated must still fail closed
// AFTER a refresh-based startup recovery (the rotated credentials are
// re-validated with the identity-first checks).
func TestLoginRefreshRecoveredForeignIdentityFailsClosed(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.applyStored(StoredAuth{
		AuthToken: "test-access-old", RefreshToken: "test-refresh-1",
		UserID: "uid-1", Username: "tester",
	})
	a.username = "tester"
	seedStoredAuth(t, a)

	var validations atomic.Int64
	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) {
		if validations.Add(1) == 1 {
			f.oauthError(w, 401, "invalid access token")
			return
		}
		// The refresh handed out a FOREIGN account's credentials.
		f.writeJSON(w, 200, validateResponse{
			ClientID: a.clientID, Login: "someoneelse",
			Scopes: requiredScopes(), UserID: "uid-FOREIGN", ExpiresIn: 5000,
		})
	}
	f.mu.Unlock()

	if err := a.Login(context.Background()); err != nil {
		t.Fatalf("login: %v", err)
	}
	device, _, refresh, _ := f.counts()
	if refresh != 1 {
		t.Fatalf("refresh attempts = %d, want 1", refresh)
	}
	if device != 1 {
		t.Fatalf("foreign refresh-recovered identity must fail closed to exactly one device flow, got %d", device)
	}
	if a.GetUserID() == "uid-FOREIGN" {
		t.Fatalf("foreign user ID adopted after refresh recovery")
	}
}

// Review regression: an hourly identity-mismatch detection is loudly surfaced
// (error event), never filed under "inconclusive".
func TestHourlyIdentityMismatchSurfacesErrorEvent(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.userID = "uid-1"
	a.SetUserID("uid-1") // runtime-confirmed binding

	var identityErrors atomic.Int64
	a.SetEventCallback(func(ev AuthEvent) {
		if ev.Type == AuthEventError && errors.Is(ev.Error, ErrIdentityMismatch) {
			identityErrors.Add(1)
		}
	})

	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) {
		f.writeJSON(w, 200, validateResponse{
			ClientID: a.clientID, Login: "someoneelse",
			Scopes: requiredScopes(), UserID: "uid-FOREIGN", ExpiresIn: 5000,
		})
	}
	f.mu.Unlock()

	a.hourlyTick(context.Background())

	if identityErrors.Load() != 1 {
		t.Fatalf("hourly identity mismatch did not surface an error event (got %d)", identityErrors.Load())
	}
	if device, _, refresh, _ := f.counts(); device != 0 || refresh != 0 {
		t.Fatalf("hourly identity mismatch must not start recovery flows (device=%d refresh=%d)", device, refresh)
	}
}

// Review regression: a username carrying glob metacharacters can neither
// break temp creation nor widen the sweep onto another profile's files.
func TestTempSweepMetacharacterUsernameSafe(t *testing.T) {
	t.Chdir(t.TempDir())
	weird := NewTwitchAuth("b?b", "device-w")
	weird.token = "test-access-weird"
	if err := weird.SaveAuth(); err != nil {
		t.Fatalf("save with metacharacter username: %v", err)
	}

	victim := filepath.Join("cookies", ".bob.json.auth-live.tmp")
	if err := os.WriteFile(victim, []byte(`{"auth_token":"fake"}`), 0600); err != nil {
		t.Fatalf("plant: %v", err)
	}

	weird.sweepStaleTempFiles()

	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("metacharacter username's sweep deleted another profile's temp")
	}
	if !strings.Contains(ownTempPrefix(weird.cookiesPath()), "b_b") {
		t.Fatalf("metacharacters not neutralized in the temp prefix: %q", ownTempPrefix(weird.cookiesPath()))
	}
}

// Review regression: the unknown-400 refresh outcome is inconclusive (never
// escalates the operator path), while a malformed refresh success — which
// consumed the one-time token — is NOT inconclusive.
func TestInconclusiveVsMalformedSentinels(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"
	f.mu.Lock()
	f.refreshHandler = func(w http.ResponseWriter, r *http.Request) { f.oauthError(w, 400, "invalid client") }
	f.mu.Unlock()
	_, err := a.Recover(context.Background(), 0)
	if !errors.Is(err, ErrRecoveryInconclusive) {
		t.Fatalf("unknown 400 must carry the inconclusive sentinel: %v", err)
	}

	f2 := newFakeOAuth(t)
	b := newLifecycleAuth(t, f2)
	b.token = "test-access-1"
	b.refreshToken = "test-refresh-1"
	f2.mu.Lock()
	f2.refreshHandler = func(w http.ResponseWriter, r *http.Request) {
		f2.writeJSON(w, 200, TokenResponse{AccessToken: "test-access-2"})
	}
	f2.mu.Unlock()
	_, err = b.Recover(context.Background(), 0)
	if errors.Is(err, ErrRecoveryInconclusive) {
		t.Fatalf("malformed refresh success must NOT be inconclusive (it consumed the token): %v", err)
	}
	if !errors.Is(err, ErrAuthProtocol) {
		t.Fatalf("malformed refresh success lost its protocol classification: %v", err)
	}
}
