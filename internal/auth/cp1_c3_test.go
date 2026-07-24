package auth

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"
)

// newLifecycleAuthWithUsername is newLifecycleAuth (lifecycle_helper_test.go)
// generalized to a caller-chosen configured username — the BKM-006
// Corrective Pass 1 C3 tests need a profile name distinct from "tester" so a
// validated LOGIN RENAME is observable against it.
func newLifecycleAuthWithUsername(t *testing.T, f *fakeOAuth, username string) *TwitchAuth {
	t.Helper()
	t.Chdir(t.TempDir())
	a := NewTwitchAuth(username, "device-xyz")
	a.deviceURL = f.srv.URL + "/device"
	a.tokenURL = f.srv.URL + "/token"
	a.validateURL = f.srv.URL + "/validate"
	a.timerAfter = immediateTimer
	return a
}

// TestPinnedOwnerID_FullLoginPath_RenameTolerated_C3A pins BKM-006
// Corrective Pass 1 test matrix item C3-A: the FULL auth.Login path (not a
// pure helper) with a pinned expectedUserID. The stored token validates to
// the pinned user ID under a DIFFERENT login than the configured profile —
// Login must succeed with NO Device Flow, the token must not be classified
// foreign, the rename must be recognized (tolerated), and config.Username
// (the stable storage key) must stay exactly as configured.
func TestPinnedOwnerID_FullLoginPath_RenameTolerated_C3A(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuthWithUsername(t, f, "oldlogin")
	a.token = "test-access-1"
	// A disk-loaded userID alone confers nothing (BKM-005); seeded only for
	// shape realism, not because it drives the outcome — the pin does.
	a.userID = "uid-123"
	seedStoredAuth(t, a)

	a.SetExpectedUserID("uid-123")

	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) {
		f.writeJSON(w, 200, validateResponse{
			ClientID: a.clientID, Login: "newlogin",
			Scopes: requiredScopes(), UserID: "uid-123", ExpiresIn: 5000,
		})
	}
	f.mu.Unlock()

	if err := a.Login(context.Background()); err != nil {
		t.Fatalf("login: %v", err)
	}
	if device, _, _, _ := f.counts(); device != 0 {
		t.Fatalf("a pinned identity match with a renamed login must not trigger a device flow, got %d", device)
	}
	if a.GetUserID() != "uid-123" {
		t.Fatalf("confirmed user id = %q, want uid-123", a.GetUserID())
	}
	if !a.IsUserIDConfirmed() {
		t.Fatal("identity must be runtime-confirmed after a valid pinned validate")
	}
	// config.Username (the stable profile/cookie/db/log storage key) is NEVER
	// re-keyed by a tolerated rename.
	if a.GetUsername() != "oldlogin" {
		t.Fatalf("stable storage key changed to %q, want oldlogin unchanged", a.GetUsername())
	}
}

// TestPinnedOwnerID_ForeignUserID_FailsClosed_C3B pins BKM-006 Corrective
// Pass 1 test matrix item C3-B: the stored token validates to a DIFFERENT
// user ID than the pin. It must fail closed — never adopted, never
// published — and (with the fresh Device Flow fallback ALSO failing here, to
// isolate the assertion) the pin, the configured username, and the on-disk
// cookie must all stay byte-for-byte exactly as they were before Login ran.
func TestPinnedOwnerID_ForeignUserID_FailsClosed_C3B(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuthWithUsername(t, f, "oldlogin")
	a.token = "test-access-1"
	a.userID = "uid-123"
	before := seedStoredAuth(t, a)

	a.SetExpectedUserID("uid-123")

	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) {
		f.writeJSON(w, 200, validateResponse{
			ClientID: a.clientID, Login: "newlogin",
			Scopes: requiredScopes(), UserID: "uid-999", ExpiresIn: 5000,
		})
	}
	// Isolate the assertion: the fallback fresh Device Flow also fails, so
	// NO publication of any kind happens during this Login call.
	f.tokenHandler = func(w http.ResponseWriter, r *http.Request) { f.oauthError(w, 400, "access_denied") }
	f.mu.Unlock()

	if err := a.Login(context.Background()); err == nil {
		t.Fatal("expected Login to fail: pinned identity mismatch, and the fallback device flow also fails")
	}
	if a.GetUserID() == "uid-999" {
		t.Fatal("foreign user ID was adopted")
	}
	// The original (rejected) token is never destroyed on an identity
	// mismatch — only ever replaced by a SUCCESSFUL fresh login, which did
	// not happen here (the fallback device flow was made to fail too).
	if got := a.GetAuthToken(); got != "test-access-1" {
		t.Fatalf("original token unexpectedly changed to %q", got)
	}
	a.mu.Lock()
	pin := a.expectedUserID
	a.mu.Unlock()
	if pin != "uid-123" {
		t.Errorf("pin mutated to %q, want uid-123 unchanged", pin)
	}
	if a.GetUsername() != "oldlogin" {
		t.Errorf("configured username mutated to %q", a.GetUsername())
	}
	after, err := os.ReadFile(a.cookiesPath())
	if err != nil {
		t.Fatalf("read cookie file: %v", err)
	}
	if string(after) != string(before) {
		t.Error("cookie file was rewritten despite the whole login failing (no successful publish occurred)")
	}
}

// TestCredentialFileUserID_NoPin_DoesNotConferAuthority_C3C pins BKM-006
// Corrective Pass 1 test matrix item C3-C: with NO pin configured
// (config.Config.OwnerUserID empty — the default), a disk-loaded userID
// alone must NOT confer authority to a login that differs from the
// configured profile — the exact BKM-005 invariant this pass must leave
// untouched. The pin itself must never be silently populated from the
// credential file either.
func TestCredentialFileUserID_NoPin_DoesNotConferAuthority_C3C(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuthWithUsername(t, f, "oldlogin")
	a.token = "test-access-1"
	// Simulate the load path: a self-consistent (but foreign-shaped once the
	// login differs below) record on disk. NO SetExpectedUserID call — the
	// pin stays empty for this whole test.
	a.userID = "uid-123"
	seedStoredAuth(t, a)

	a.mu.Lock()
	pinBefore := a.expectedUserID
	a.mu.Unlock()
	if pinBefore != "" {
		t.Fatalf("setup: pin must start empty, got %q", pinBefore)
	}

	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) {
		if authHeaderToken(r) == "test-access-1" {
			// Same disk-loaded userID, but a DIFFERENT login — the disk
			// userID alone must not authorize this (BKM-005).
			f.writeJSON(w, 200, validateResponse{
				ClientID: a.clientID, Login: "renamed-login",
				Scopes: requiredScopes(), UserID: "uid-123", ExpiresIn: 5000,
			})
			return
		}
		// The fresh device-flow candidate belongs to THIS profile (login
		// matches the configured "oldlogin", unlike validFor's hardcoded
		// "tester").
		f.writeJSON(w, 200, validateResponse{
			ClientID: a.clientID, Login: "oldlogin",
			Scopes: requiredScopes(), UserID: "uid-fresh", ExpiresIn: 5000,
		})
	}
	f.mu.Unlock()

	if err := a.Login(context.Background()); err != nil {
		t.Fatalf("login: %v", err)
	}
	if device, _, _, _ := f.counts(); device != 1 {
		t.Fatalf("no pin configured: a disk-loaded-userID rename shape must fail closed to exactly one device flow, got %d", device)
	}

	a.mu.Lock()
	pinAfter := a.expectedUserID
	a.mu.Unlock()
	if pinAfter != "" {
		t.Errorf("pin was silently populated from the credential file / a runtime event: %q, want still empty (only the miner's explicit config-sourced backfill may ever set it)", pinAfter)
	}
}

// TestPinnedOwnerID_TwoConsecutiveRestarts_NoDeviceFlow_C3D pins BKM-006
// Corrective Pass 1 test matrix item C3-D: two consecutive "restarts" (two
// SEPARATE TwitchAuth instances, each loading the SAME stable stored-auth
// file and pinned with the SAME owner user ID) after an owner rename both
// authenticate via the stable key + pin — no fresh Device Flow on EITHER
// restart.
func TestPinnedOwnerID_TwoConsecutiveRestarts_NoDeviceFlow_C3D(t *testing.T) {
	f := newFakeOAuth(t)

	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) {
		f.writeJSON(w, 200, validateResponse{
			ClientID: "ue6666qo983tsx6so1t0vnawi233wa", Login: "renamed-login",
			Scopes: requiredScopes(), UserID: "uid-123", ExpiresIn: 5000,
		})
	}
	f.mu.Unlock()

	// Restart 1.
	a1 := newLifecycleAuthWithUsername(t, f, "oldlogin")
	a1.token = "test-access-1"
	a1.userID = "uid-123"
	seedStoredAuth(t, a1)
	a1.SetExpectedUserID("uid-123")
	if err := a1.Login(context.Background()); err != nil {
		t.Fatalf("restart 1 login: %v", err)
	}
	if device, _, _, _ := f.counts(); device != 0 {
		t.Fatalf("restart 1: unexpected device flow, count=%d", device)
	}
	if err := a1.SaveAuth(); err != nil {
		t.Fatalf("restart 1 save: %v", err)
	}

	// Restart 2: a FRESH instance loading the SAME stable stored file (same
	// configured username -> same cookiesPath), re-pinned the same way the
	// miner would do on every startup from config.Config.OwnerUserID.
	a2 := NewTwitchAuth("oldlogin", "device-xyz")
	a2.deviceURL, a2.tokenURL, a2.validateURL = a1.deviceURL, a1.tokenURL, a1.validateURL
	a2.timerAfter = immediateTimer
	a2.SetExpectedUserID("uid-123")
	if err := a2.Login(context.Background()); err != nil {
		t.Fatalf("restart 2 login: %v", err)
	}
	if device, _, _, _ := f.counts(); device != 0 {
		t.Fatalf("restart 2: unexpected device flow, cumulative count=%d", device)
	}
	if a2.GetUserID() != "uid-123" {
		t.Fatalf("restart 2 user id = %q, want uid-123", a2.GetUserID())
	}
	if a2.GetUsername() != "oldlogin" {
		t.Fatalf("restart 2 stable key = %q, want oldlogin unchanged", a2.GetUsername())
	}
}

// TestPinnedOwnerID_CandidatePath_C3E pins BKM-006 Corrective Pass 1 test
// matrix item C3-E: a fresh Device-Flow candidate is checked against the
// pinned user ID exactly like the authoritative validate path — a match
// (including a login rename) is accepted and published; a mismatch is
// rejected and never published, leaving the credential set completely
// untouched.
func TestPinnedOwnerID_CandidatePath_C3E(t *testing.T) {
	t.Run("accepted_with_rename", func(t *testing.T) {
		f := newFakeOAuth(t)
		a := newLifecycleAuthWithUsername(t, f, "oldlogin")
		a.SetExpectedUserID("uid-123")

		f.mu.Lock()
		f.validateHandler = func(w http.ResponseWriter, r *http.Request) {
			f.writeJSON(w, 200, validateResponse{
				ClientID: a.clientID, Login: "renamed-login",
				Scopes: requiredScopes(), UserID: "uid-123", ExpiresIn: 5000,
			})
		}
		f.mu.Unlock()

		if a.GetAuthToken() != "" || a.Generation() != 0 {
			t.Fatal("setup: credentials must start empty")
		}
		if err := a.DeviceFlowLogin(context.Background()); err != nil {
			t.Fatalf("device flow: %v", err)
		}
		if a.GetUserID() != "uid-123" || a.Generation() != 1 {
			t.Fatalf("pinned candidate with a matching id was not promoted: userID=%q generation=%d", a.GetUserID(), a.Generation())
		}
		if a.GetUsername() != "oldlogin" {
			t.Errorf("stable storage key changed to %q despite the candidate's login differing (tolerated rename)", a.GetUsername())
		}
	})

	t.Run("rejected_foreign", func(t *testing.T) {
		f := newFakeOAuth(t)
		a := newLifecycleAuthWithUsername(t, f, "oldlogin")
		a.SetExpectedUserID("uid-123")

		f.mu.Lock()
		f.validateHandler = func(w http.ResponseWriter, r *http.Request) {
			f.writeJSON(w, 200, validateResponse{
				ClientID: a.clientID, Login: "someoneelse",
				Scopes: requiredScopes(), UserID: "uid-OTHER", ExpiresIn: 5000,
			})
		}
		f.mu.Unlock()

		err := a.DeviceFlowLogin(context.Background())
		if err == nil {
			t.Fatal("expected the candidate to be rejected as foreign against the pin")
		}
		if !errors.Is(err, ErrIdentityMismatch) {
			t.Errorf("error = %v, want ErrIdentityMismatch", err)
		}
		// Candidate privacy: never published, credential set stays empty.
		if a.GetAuthToken() != "" || a.Generation() != 0 {
			t.Fatal("rejected candidate leaked into published credentials")
		}
	})
}
