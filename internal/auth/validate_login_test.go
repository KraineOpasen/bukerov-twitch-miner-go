package auth

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
)

// seedStoredAuth persists a stored record for the given auth and returns the
// on-disk bytes for later comparison.
func seedStoredAuth(t *testing.T, a *TwitchAuth) []byte {
	t.Helper()
	if err := a.SaveAuth(); err != nil {
		t.Fatalf("seed save: %v", err)
	}
	b, err := os.ReadFile(a.cookiesPath())
	if err != nil {
		t.Fatalf("read seeded file: %v", err)
	}
	return b
}

// C1/C2: a valid 200 lets startup continue without any device flow and
// refreshes the expiry metadata.
func TestLoginValid200NoDeviceFlow(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.userID = "uid-1"
	seedStoredAuth(t, a)

	if err := a.Login(context.Background()); err != nil {
		t.Fatalf("login: %v", err)
	}
	device, deviceGrant, refresh, validate := f.counts()
	if device != 0 || deviceGrant != 0 || refresh != 0 {
		t.Fatalf("valid stored token triggered auth traffic: device=%d grant=%d refresh=%d", device, deviceGrant, refresh)
	}
	if validate != 1 {
		t.Fatalf("startup validations = %d, want 1", validate)
	}

	h := a.Health()
	if h.ValidationState != "valid" {
		t.Fatalf("validation state = %q", h.ValidationState)
	}
	if h.ExpiresAt.IsZero() {
		t.Fatalf("expiry metadata not updated from validate response")
	}
	if a.Generation() != 0 {
		t.Fatalf("healthy validation must not bump the credential generation")
	}
}

// C3: a client-ID mismatch is classified safely — no destruction, no device
// flow, startup continues degraded.
func TestLoginClientIDMismatchDegradedNotDestroyed(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.userID = "uid-1"
	before := seedStoredAuth(t, a)

	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) {
		f.writeJSON(w, 200, validateResponse{
			ClientID: "some-other-client-id", Login: "tester",
			Scopes: requiredScopes(), UserID: "uid-1", ExpiresIn: 5000,
		})
	}
	f.mu.Unlock()

	if err := a.Login(context.Background()); err != nil {
		t.Fatalf("login: %v", err)
	}
	if device, _, _, _ := f.counts(); device != 0 {
		t.Fatalf("client-ID mismatch started a device flow")
	}
	if got := a.Health().ValidationState; !strings.Contains(got, string(ValidateStatusClientIDMismatch)) {
		t.Fatalf("validation state = %q, want client_id_mismatch classification", got)
	}
	after, _ := os.ReadFile(a.cookiesPath())
	if string(after) != string(before) {
		t.Fatalf("client-ID mismatch rewrote the auth file")
	}
}

// C4: missing required scopes is not healthy — degraded classification, but no
// device flow and no destruction.
func TestLoginMissingScopesDegraded(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.userID = "uid-1"
	seedStoredAuth(t, a)

	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) {
		f.writeJSON(w, 200, validateResponse{
			ClientID: a.clientID, Login: "tester",
			Scopes: []string{"chat:read"}, UserID: "uid-1", ExpiresIn: 5000,
		})
	}
	f.mu.Unlock()

	if err := a.Login(context.Background()); err != nil {
		t.Fatalf("login: %v", err)
	}
	if device, _, _, _ := f.counts(); device != 0 {
		t.Fatalf("missing scopes started a device flow")
	}
	if got := a.Health().ValidationState; !strings.Contains(got, string(ValidateStatusMissingScopes)) {
		t.Fatalf("validation state = %q, want missing_scopes classification", got)
	}
}

// C5 / M1: validate 401 + refresh success -> startup continues on the rotated
// pair with no device flow.
func TestLoginValidate401RefreshSuccess(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"
	a.userID = "uid-1"
	seedStoredAuth(t, a)

	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) { f.oauthError(w, 401, "invalid access token") }
	f.mu.Unlock()

	if err := a.Login(context.Background()); err != nil {
		t.Fatalf("login: %v", err)
	}
	if a.GetAuthToken() != "test-access-2" || a.Generation() != 1 {
		t.Fatalf("refresh recovery did not rotate the pair")
	}
	device, _, refresh, _ := f.counts()
	if device != 0 || refresh != 1 {
		t.Fatalf("unexpected recovery traffic: device=%d refresh=%d", device, refresh)
	}
}

// C6: validate 401 with no refresh token -> exactly one device flow.
func TestLoginValidate401NoRefreshTokenOneDeviceFlow(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.userID = "uid-1"
	seedStoredAuth(t, a)

	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) { f.oauthError(w, 401, "invalid access token") }
	f.mu.Unlock()

	if err := a.Login(context.Background()); err != nil {
		t.Fatalf("login: %v", err)
	}
	device, _, refresh, _ := f.counts()
	if device != 1 || refresh != 0 {
		t.Fatalf("want exactly one device flow and no refresh: device=%d refresh=%d", device, refresh)
	}
	if a.GetAuthToken() != "test-access-df" {
		t.Fatalf("device-flow credentials not adopted")
	}
}

// C7/C8: transient validate failures (transport, 5xx) keep the loaded token
// and never start a device flow.
func TestLoginValidateTransientKeepsToken(t *testing.T) {
	t.Run("transport", func(t *testing.T) {
		f := newFakeOAuth(t)
		a := newLifecycleAuth(t, f)
		a.token = "test-access-1"
		a.userID = "uid-1"
		before := seedStoredAuth(t, a)
		a.validateURL = "http://127.0.0.1:1/validate" // unreachable

		if err := a.Login(context.Background()); err != nil {
			t.Fatalf("login: %v", err)
		}
		if device, _, refresh, _ := f.counts(); device != 0 || refresh != 0 {
			t.Fatalf("transient validate failure triggered recovery traffic")
		}
		if a.GetAuthToken() != "test-access-1" {
			t.Fatalf("loaded token was dropped on a transient failure")
		}
		after, _ := os.ReadFile(a.cookiesPath())
		if string(after) != string(before) {
			t.Fatalf("transient validate failure rewrote the auth file")
		}
	})
	t.Run("http500", func(t *testing.T) {
		f := newFakeOAuth(t)
		a := newLifecycleAuth(t, f)
		a.token = "test-access-1"
		a.userID = "uid-1"
		seedStoredAuth(t, a)
		f.mu.Lock()
		f.validateHandler = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }
		f.mu.Unlock()

		if err := a.Login(context.Background()); err != nil {
			t.Fatalf("login: %v", err)
		}
		if device, _, refresh, _ := f.counts(); device != 0 || refresh != 0 {
			t.Fatalf("500 validate triggered recovery traffic")
		}
		if a.GetAuthToken() != "test-access-1" {
			t.Fatalf("loaded token was dropped on a 500")
		}
	})
}

// C9: a malformed 200 is a fail-closed protocol error — credentials survive,
// no device flow, no endless loop.
func TestLoginMalformed200ProtocolErrorKeepsCredentials(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.userID = "uid-1"
	seedStoredAuth(t, a)

	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{not json`))
	}
	f.mu.Unlock()

	if err := a.Login(context.Background()); err != nil {
		t.Fatalf("login: %v", err)
	}
	if device, _, refresh, _ := f.counts(); device != 0 || refresh != 0 {
		t.Fatalf("malformed 200 triggered recovery traffic")
	}
	if a.GetAuthToken() != "test-access-1" {
		t.Fatalf("credentials destroyed on malformed validate response")
	}
	if got := a.Health().ValidationState; !strings.Contains(got, string(ValidateStatusProtocolError)) {
		t.Fatalf("validation state = %q, want protocol_error", got)
	}
}

// C10: an unreadable encrypted record is never overwritten with an empty
// record — a failed follow-up device flow leaves the original bytes intact.
func TestUnreadableEncryptedFileNotOverwritten(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.userID = "uid-1"

	t.Setenv(EnvEncryptionKey, "original-key")
	before := seedStoredAuth(t, a)

	// Key changed: the record can no longer be decrypted.
	t.Setenv(EnvEncryptionKey, "different-key")

	// Make the device flow fail terminally so Login errors out.
	f.mu.Lock()
	f.tokenHandler = func(w http.ResponseWriter, r *http.Request) { f.oauthError(w, 400, "access_denied") }
	f.mu.Unlock()

	fresh := NewTwitchAuth("tester", "device-xyz")
	fresh.deviceURL = a.deviceURL
	fresh.tokenURL = a.tokenURL
	fresh.validateURL = a.validateURL
	fresh.timerAfter = immediateTimer

	if err := fresh.Login(context.Background()); err == nil {
		t.Fatalf("expected login failure when decrypt and device flow both fail")
	}
	after, err := os.ReadFile(fresh.cookiesPath())
	if err != nil {
		t.Fatalf("original encrypted file is gone: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("unreadable encrypted record was overwritten")
	}
}

// Identity mismatch: a token for a different account is never adopted; a fresh
// device login runs for this profile.
func TestLoginIdentityMismatchStartsFreshDeviceFlow(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.userID = "uid-1"
	seedStoredAuth(t, a)

	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) {
		f.writeJSON(w, 200, validateResponse{
			ClientID: a.clientID, Login: "someoneelse",
			Scopes: requiredScopes(), UserID: "uid-OTHER", ExpiresIn: 5000,
		})
	}
	f.mu.Unlock()

	if err := a.Login(context.Background()); err != nil {
		t.Fatalf("login: %v", err)
	}
	if device, _, _, _ := f.counts(); device != 1 {
		t.Fatalf("identity mismatch must run exactly one fresh device flow, got %d", device)
	}
	if a.GetUserID() == "uid-OTHER" {
		t.Fatalf("foreign user ID was adopted")
	}
}

// Rename (same user ID, different login) is NOT an identity mismatch — BKM-006
// territory, surfaced but not acted on.
func TestLoginRenameSameUserIDIsNotMismatch(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.userID = "uid-1"
	seedStoredAuth(t, a)

	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) {
		f.writeJSON(w, 200, validateResponse{
			ClientID: a.clientID, Login: "renamed-login",
			Scopes: requiredScopes(), UserID: "uid-1", ExpiresIn: 5000,
		})
	}
	f.mu.Unlock()

	if err := a.Login(context.Background()); err != nil {
		t.Fatalf("login: %v", err)
	}
	if device, _, _, _ := f.counts(); device != 0 {
		t.Fatalf("rename treated as mismatch (device flow started)")
	}
	if a.Health().ValidationState != "valid" {
		t.Fatalf("rename should still validate as healthy, got %q", a.Health().ValidationState)
	}
}
