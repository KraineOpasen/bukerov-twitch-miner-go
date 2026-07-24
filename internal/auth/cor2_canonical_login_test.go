package auth

import (
	"context"
	"net/http"
	"testing"
)

// TestGetCanonicalLogin_SurfacesRenamedLogin_COR2 pins BKM-006 Corrective Pass
// 1, COR-2 at the auth layer: after a pinned-identity TOLERATED rename through
// the full Login path, the account's NEW canonical login is exposed via
// GetCanonicalLogin (which the miner adopts into config.Username for every
// Twitch-/user-facing operation), while GetUsername (the stable storage key
// cookies/db/logs are dialed under) is left unchanged.
func TestGetCanonicalLogin_SurfacesRenamedLogin_COR2(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuthWithUsername(t, f, "oldlogin")
	a.token = "test-access-1"
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
		t.Fatalf("a pinned rename must not trigger a device flow, got %d", device)
	}
	if got := a.GetCanonicalLogin(); got != "newlogin" {
		t.Fatalf("GetCanonicalLogin()=%q, want newlogin (the miner adopts this as config.Username)", got)
	}
	if got := a.GetUsername(); got != "oldlogin" {
		t.Fatalf("stable storage key GetUsername()=%q, want oldlogin unchanged (storage must not follow the login)", got)
	}
}

// TestGetCanonicalLogin_NonRenamedStartup_MatchesConfigured_COR2: a normal
// (non-renamed) startup surfaces the configured login as canonical, so the
// miner's reconcileOwnerIdentity is a no-op (no spurious config rewrite).
func TestGetCanonicalLogin_NonRenamedStartup_MatchesConfigured_COR2(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuthWithUsername(t, f, "steady")
	a.token = "test-access-1"
	a.userID = "uid-7"
	seedStoredAuth(t, a)
	a.SetExpectedUserID("uid-7")

	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) {
		f.writeJSON(w, 200, validateResponse{
			ClientID: a.clientID, Login: "steady",
			Scopes: requiredScopes(), UserID: "uid-7", ExpiresIn: 5000,
		})
	}
	f.mu.Unlock()

	if err := a.Login(context.Background()); err != nil {
		t.Fatalf("login: %v", err)
	}
	if got := a.GetCanonicalLogin(); got != "steady" {
		t.Fatalf("GetCanonicalLogin()=%q, want steady", got)
	}
}
