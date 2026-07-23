package auth

import (
	"context"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
)

// --- C1: the device-code token exchange must send the scopes parameter ---

// C1.1/C1.2/C1.4: both the device-code request and the token exchange send the
// exact configured scope set, form-encoded.
func TestDeviceFlowSendsScopesOnBothRequests(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)

	var deviceScopes, exchangeScopes string
	f.mu.Lock()
	f.deviceHandler = func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		deviceScopes = r.PostForm.Get("scopes")
		f.writeJSON(w, 200, DeviceCodeResponse{
			DeviceCode: "test-device-code", ExpiresIn: 1800, Interval: 5,
			UserCode: "TESTCODE", VerificationURI: "https://example.invalid/activate",
		})
	}
	f.tokenHandler = func(w http.ResponseWriter, r *http.Request) {
		exchangeScopes = r.PostForm.Get("scopes")
		f.writeJSON(w, 200, TokenResponse{
			AccessToken: "test-access-df", RefreshToken: "test-refresh-df",
			ExpiresIn: 14000, Scope: requiredScopes(), TokenType: "bearer",
		})
	}
	f.mu.Unlock()

	if err := a.DeviceFlowLogin(context.Background()); err != nil {
		t.Fatalf("device flow: %v", err)
	}
	if deviceScopes == "" || !slices.Equal(strings.Fields(deviceScopes), requiredScopes()) {
		t.Fatalf("device-code request scope set does not match the configured scopes")
	}
	if exchangeScopes == "" {
		t.Fatalf("token exchange sent NO scopes parameter (official DCF contract requires it)")
	}
	if !slices.Equal(strings.Fields(exchangeScopes), strings.Fields(deviceScopes)) {
		t.Fatalf("token exchange scope set differs from the device-code request's")
	}
}

// C1.3/C1.5: a server enforcing the scopes parameter rejects a scope-less
// exchange, and no pair is published until a scoped exchange is accepted.
func TestScopedExchangeRequiredForPublication(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)

	f.mu.Lock()
	f.tokenHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.PostForm.Get("scopes") == "" {
			// Strict fake: enforce the documented required parameter.
			f.oauthError(w, 400, "missing scopes")
			return
		}
		f.writeJSON(w, 200, TokenResponse{
			AccessToken: "test-access-df", RefreshToken: "test-refresh-df",
			ExpiresIn: 14000, Scope: requiredScopes(), TokenType: "bearer",
		})
	}
	f.mu.Unlock()

	if err := a.DeviceFlowLogin(context.Background()); err != nil {
		t.Fatalf("scoped exchange rejected: %v", err)
	}
	if a.GetAuthToken() != "test-access-df" || a.Generation() != 1 {
		t.Fatalf("pair not published after accepted scoped exchange")
	}
}

// --- C2: only the documented discriminator makes a refresh 400 authoritative ---

// C2.1/C2.2: the documented 400 "Invalid refresh token" (case/whitespace
// normalized) is authoritative -> exactly one device flow.
func TestRefreshInvalidDiscriminatorTriggersOneDeviceFlow(t *testing.T) {
	for _, msg := range []string{"Invalid refresh token", "  invalid REFRESH token  "} {
		f := newFakeOAuth(t)
		a := newLifecycleAuth(t, f)
		a.token = "test-access-1"
		a.refreshToken = "test-refresh-dead"

		f.mu.Lock()
		f.refreshHandler = func(w http.ResponseWriter, r *http.Request) { f.oauthError(w, 400, msg) }
		f.mu.Unlock()

		if _, err := a.Recover(context.Background(), 0); err != nil {
			t.Fatalf("message %q: recover via device flow: %v", msg, err)
		}
		if device, _, _, _ := f.counts(); device != 1 {
			t.Fatalf("message %q: device flows = %d, want exactly 1", msg, device)
		}
	}
}

// C2.3/C2.4/C2.5: a 400 with an unrelated, malformed, or empty body proves
// nothing about the refresh token — no device flow, credentials and refresh
// token kept.
func TestRefreshUnknown400IsInconclusiveNoDeviceFlow(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"unrelated-message": nil, // filled below with oauthError writer
		"malformed-json": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{not json`))
		},
		"empty-body": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(400)
		},
	}
	for name, handler := range cases {
		f := newFakeOAuth(t)
		a := newLifecycleAuth(t, f)
		a.token = "test-access-1"
		a.refreshToken = "test-refresh-1"

		h := handler
		if name == "unrelated-message" {
			h = func(w http.ResponseWriter, r *http.Request) { f.oauthError(w, 400, "invalid client") }
		}
		f.mu.Lock()
		f.refreshHandler = h
		f.mu.Unlock()

		_, err := a.Recover(context.Background(), 0)
		if err == nil {
			t.Fatalf("%s: expected an inconclusive recovery error", name)
		}
		if !errors.Is(err, ErrAuthProtocol) {
			t.Fatalf("%s: unknown 400 must classify as a fail-closed protocol outcome, got %v", name, err)
		}
		if device, _, _, _ := f.counts(); device != 0 {
			t.Fatalf("%s: unknown 400 started a device flow", name)
		}
		if a.GetAuthToken() != "test-access-1" || !a.Health().HasRefreshToken {
			t.Fatalf("%s: unknown 400 destroyed credential state", name)
		}
	}
}

// C2.9: undocumented refresh statuses stay inconclusive/transient — never
// device flow (429/5xx/transport are covered by the pass-1 suite).
func TestRefreshUndocumentedStatusesNoDeviceFlow(t *testing.T) {
	for _, status := range []int{403, 409, 422} {
		f := newFakeOAuth(t)
		a := newLifecycleAuth(t, f)
		a.token = "test-access-1"
		a.refreshToken = "test-refresh-1"

		f.mu.Lock()
		f.refreshHandler = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }
		f.mu.Unlock()

		if _, err := a.Recover(context.Background(), 0); err == nil {
			t.Fatalf("status %d: expected a retryable error", status)
		}
		if device, _, _, _ := f.counts(); device != 0 {
			t.Fatalf("status %d: undocumented status started a device flow", status)
		}
	}
}

// C2.10: no raw body or token material in errors for any refresh outcome.
func TestRefreshClassifierErrorsCarryNoBodyMaterial(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-secret-material"

	f.mu.Lock()
	f.refreshHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"status":400,"message":"SECRET-BODY-MARKER something"}`))
	}
	f.mu.Unlock()

	_, err := a.Recover(context.Background(), 0)
	if err == nil {
		t.Fatalf("expected error")
	}
	for _, leak := range []string{"SECRET-BODY-MARKER", "test-refresh-secret-material"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("classifier error leaks body/token material: %q", err.Error())
		}
	}
}

// --- C3: ALL validation outcomes are generation-aware ---

// rotateDuringValidate installs a validate handler that publishes a new
// generation BEFORE answering, then responds via respond — the deterministic
// mid-flight-rotation barrier.
func rotateDuringValidate(f *fakeOAuth, a *TwitchAuth, respond http.HandlerFunc) {
	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) {
		a.publishTokenPair(&TokenResponse{
			AccessToken: "test-access-new", RefreshToken: "test-refresh-new",
			ExpiresIn: 14000, Scope: requiredScopes(), TokenType: "bearer",
		})
		respond(w, r)
	}
	f.mu.Unlock()
}

// C3.2-C3.6: a stale non-200 (and anomalous 200) outcome for the OLD
// generation must not degrade the NEW generation's health.
func TestStaleValidationOutcomesDoNotDegradeNewGeneration(t *testing.T) {
	cases := map[string]func(f *fakeOAuth) http.HandlerFunc{
		"401": func(f *fakeOAuth) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) { f.oauthError(w, 401, "invalid access token") }
		},
		"transient-503": func(f *fakeOAuth) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }
		},
		"malformed-200": func(f *fakeOAuth) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte(`{not json`)) }
		},
		"client-id-mismatch": func(f *fakeOAuth) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				f.writeJSON(w, 200, validateResponse{
					ClientID: "some-other-client", Login: "tester",
					Scopes: requiredScopes(), UserID: "uid-1", ExpiresIn: 5000,
				})
			}
		},
		"missing-scopes": func(f *fakeOAuth) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				f.writeJSON(w, 200, validateResponse{
					ClientID: "ue6666qo983tsx6so1t0vnawi233wa", Login: "tester",
					Scopes: []string{"chat:read"}, UserID: "uid-1", ExpiresIn: 5000,
				})
			}
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeOAuth(t)
			a := newLifecycleAuth(t, f)
			a.token = "test-access-old"
			a.userID = "uid-1"
			rotateDuringValidate(f, a, mk(f))

			_, _ = a.ValidateAndApply(context.Background())

			h := a.Health()
			if h.Generation != 1 {
				t.Fatalf("setup: rotation did not publish generation 1")
			}
			if h.ValidationState != "valid" {
				t.Fatalf("stale %s outcome degraded the NEW generation: state = %q", name, h.ValidationState)
			}
		})
	}
}

// C3.7: a stale 401 keys recovery on the OLD generation — the fast path
// returns the new credentials with zero refresh traffic.
func TestStale401DoesNotRefreshFreshGeneration(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-old"
	a.refreshToken = "test-refresh-old"
	a.userID = "uid-1"
	rotateDuringValidate(f, a, func(w http.ResponseWriter, r *http.Request) {
		f.oauthError(w, 401, "invalid access token")
	})

	a.hourlyTick(context.Background())

	if _, _, refresh, _ := f.counts(); refresh != 0 {
		t.Fatalf("stale 401 refreshed the freshly rotated generation: %d refreshes", refresh)
	}
	if a.Generation() != 1 {
		t.Fatalf("generation churned: %d", a.Generation())
	}
}

// C3.8: a CURRENT-generation failure still updates health as designed.
func TestCurrentGenerationFailureStillDegrades(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.userID = "uid-1"
	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }
	f.mu.Unlock()

	_, _ = a.ValidateAndApply(context.Background())
	if got := a.Health().ValidationState; !strings.Contains(got, "degraded") {
		t.Fatalf("current-generation transient failure did not degrade health: %q", got)
	}
}

// --- C4: authoritative identity outranks client-ID/scope anomalies ---

func foreignValidateHandler(f *fakeOAuth, clientID string, scopes []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.writeJSON(w, 200, validateResponse{
			ClientID: clientID, Login: "someoneelse",
			Scopes: scopes, UserID: "uid-FOREIGN", ExpiresIn: 5000,
		})
	}
}

// C4.1-C4.4: a foreign identity wins over every combination of client-ID and
// scope anomalies.
func TestForeignIdentityOutranksLesserAnomalies(t *testing.T) {
	const goodClient = "ue6666qo983tsx6so1t0vnawi233wa"
	cases := map[string]struct {
		clientID string
		scopes   []string
	}{
		"clean":           {goodClient, requiredScopes()},
		"client-mismatch": {"some-other-client", requiredScopes()},
		"missing-scopes":  {goodClient, []string{"chat:read"}},
		"both-anomalies":  {"some-other-client", []string{"chat:read"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeOAuth(t)
			a := newLifecycleAuth(t, f)
			a.token = "test-access-1"
			a.userID = "uid-1" // disk-loaded, NOT runtime-confirmed
			f.mu.Lock()
			f.validateHandler = foreignValidateHandler(f, tc.clientID, tc.scopes)
			f.mu.Unlock()

			status, err := a.ValidateAndApply(context.Background())
			if status != ValidateStatusIdentityMismatch {
				t.Fatalf("status = %q, want identity_mismatch to win", status)
			}
			if !errors.Is(err, ErrIdentityMismatch) {
				t.Fatalf("error = %v, want ErrIdentityMismatch", err)
			}
			if a.GetUserID() == "uid-FOREIGN" {
				t.Fatalf("foreign user ID adopted")
			}
		})
	}
}

// C4.5: a manipulated local userID that matches the foreign token's user_id
// must not mask the foreign identity — the disk-loaded userID is not trusted
// to authorize a login that differs from the configured profile.
func TestManipulatedLocalUserIDDoesNotMaskForeignIdentity(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	// Simulate the load path: a self-consistent foreign record on disk.
	a.applyStored(StoredAuth{AuthToken: "test-access-1", UserID: "uid-FOREIGN", Username: "someoneelse"})

	f.mu.Lock()
	f.validateHandler = foreignValidateHandler(f, a.clientID, requiredScopes())
	f.mu.Unlock()

	status, _ := a.ValidateAndApply(context.Background())
	if status != ValidateStatusIdentityMismatch {
		t.Fatalf("status = %q — a disk-loaded userID masked an authoritative foreign identity", status)
	}
}

// C4.6: a runtime-CONFIRMED userID with a changed login is a rename
// observation, not a foreign identity.
func TestConfirmedUserIDRenameIsNotForeign(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	// Runtime-confirmed binding. P2: only an authoritative validate
	// application (or candidate promotion) confers confirmation — SetUserID
	// deliberately cannot — so the provenance is seeded directly here.
	a.userID = "uid-1"
	a.userIDAuthoritative = true

	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) {
		f.writeJSON(w, 200, validateResponse{
			ClientID: a.clientID, Login: "renamed-login",
			Scopes: requiredScopes(), UserID: "uid-1", ExpiresIn: 5000,
		})
	}
	f.mu.Unlock()

	status, err := a.ValidateAndApply(context.Background())
	if status != ValidateStatusValid || err != nil {
		t.Fatalf("rename with confirmed userID misclassified: %v %v", status, err)
	}
}

// C4.7/C4.8: identity mismatch never deletes the original file, and the fresh
// device login persists a clean record with no foreign identity material.
func TestIdentityMismatchPreservesFileAndFreshLoginIsClean(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.userID = "uid-FOREIGN"
	a.username = "tester"
	before := seedStoredAuth(t, a)

	f.mu.Lock()
	foreign := foreignValidateHandler(f, a.clientID, requiredScopes())
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) {
		if authHeaderToken(r) == "test-access-1" {
			foreign(w, r)
			return
		}
		// The fresh device-flow candidate belongs to this profile.
		validFor(f, a, w)
	}
	f.mu.Unlock()

	// Phase 1: device flow fails -> original file untouched.
	f.mu.Lock()
	f.tokenHandler = func(w http.ResponseWriter, r *http.Request) { f.oauthError(w, 400, "access_denied") }
	f.mu.Unlock()
	if err := a.Login(context.Background()); err == nil {
		t.Fatalf("expected failed device flow")
	}
	after, _ := os.ReadFile(a.cookiesPath())
	if string(after) != string(before) {
		t.Fatalf("identity mismatch path modified the original auth file before a successful login")
	}

	// Phase 2: device flow succeeds -> record is clean of foreign identity.
	f.mu.Lock()
	f.tokenHandler = nil
	f.mu.Unlock()
	if err := a.Login(context.Background()); err != nil {
		t.Fatalf("fresh login: %v", err)
	}
	body := readCookieFile(t, a)
	if strings.Contains(body, "uid-FOREIGN") || strings.Contains(body, "someoneelse") {
		t.Fatalf("fresh login persisted foreign identity material")
	}
}

// --- C6: authoritative validated scopes are applied without churn ---

func (a *TwitchAuth) scopesSnapshotForTest() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.scopes)
}

// C6.1/C6.2: a valid current-generation validate populates/updates runtime
// scopes from the authoritative response.
func TestValidateAppliesAuthoritativeScopes(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.userID = "uid-1"
	// Legacy record: no scopes metadata.
	if len(a.scopesSnapshotForTest()) != 0 {
		t.Fatalf("setup: expected empty legacy scopes")
	}

	if st, err := a.ValidateAndApply(context.Background()); st != ValidateStatusValid || err != nil {
		t.Fatalf("validate: %v %v", st, err)
	}
	got := a.scopesSnapshotForTest()
	if !slices.Equal(got, requiredScopes()) {
		t.Fatalf("runtime scopes not populated from the authoritative validate response: %v", got)
	}
}

// C6.3: the same scope SET in a different order causes no file rewrite churn.
func TestSameScopeSetDifferentOrderNoFileChurn(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.userID = "uid-1"
	a.scopes = requiredScopes()
	if err := a.SaveAuth(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	before, _ := os.ReadFile(a.cookiesPath())

	shuffled := slices.Clone(requiredScopes())
	slices.Reverse(shuffled)
	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) {
		f.writeJSON(w, 200, validateResponse{
			ClientID: a.clientID, Login: "tester",
			Scopes: shuffled, UserID: "uid-1", ExpiresIn: 5000,
		})
	}
	f.mu.Unlock()

	for range 3 {
		if st, err := a.ValidateAndApply(context.Background()); st != ValidateStatusValid || err != nil {
			t.Fatalf("validate: %v %v", st, err)
		}
	}
	after, _ := os.ReadFile(a.cookiesPath())
	if string(after) != string(before) {
		t.Fatalf("order-only scope difference rewrote the auth file")
	}
	if a.Health().PersistPending {
		t.Fatalf("order-only scope difference marked the record dirty")
	}
}

// C10 (profile-scoped temp cleanup) regression tests live in
// corrective1_c10_test.go — they exercise the profile-scoped naming helper
// introduced by the corrective commit.
