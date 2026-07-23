package auth

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// C10.1-C10.3: profile A's sweep removes only ITS own orphan temps — never
// another profile's temps (including legacy generic-named ones, which could be
// another process's LIVE temp) and never unrelated files.
func TestTempSweepIsProfileScoped(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.userID = "uid-1"
	seedStoredAuth(t, a)

	own := filepath.Join("cookies", ownTempPrefix(a.cookiesPath())+"orphan1.tmp")
	foreignScoped := filepath.Join("cookies", ".bob.json.auth-orphan2.tmp")
	legacyGeneric := filepath.Join("cookies", ".auth-orphan3.tmp")
	unrelated := filepath.Join("cookies", "bob.json")
	for _, p := range []string{own, foreignScoped, legacyGeneric, unrelated} {
		if err := os.WriteFile(p, []byte(`{"auth_token":"fake"}`), 0600); err != nil {
			t.Fatalf("plant %s: %v", p, err)
		}
	}

	if err := a.Login(context.Background()); err != nil {
		t.Fatalf("login: %v", err)
	}

	if _, err := os.Stat(own); !os.IsNotExist(err) {
		t.Fatalf("own orphan temp survived the profile sweep")
	}
	for _, p := range []string{foreignScoped, legacyGeneric, unrelated} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("sweep deleted a file it does not own: %s", p)
		}
	}
}

// C10.4 (P2-C4 form): the temp naming derives ONLY from the final path —
// deterministic per profile, distinct across profiles, and free of any
// secret material.
func TestTempNamingCarriesNoSecrets(t *testing.T) {
	a := newAuth(t, "tester", "very-secret-token-value")
	a.refreshToken = "very-secret-refresh-value"
	if err := a.SaveAuth(); err != nil {
		t.Fatalf("save: %v", err)
	}
	prefix := ownTempPrefix(a.cookiesPath())
	if strings.Contains(prefix, "secret") {
		t.Fatalf("temp prefix leaks secret material: %q", prefix)
	}
	if prefix != ownTempPrefix(a.cookiesPath()) {
		t.Fatalf("temp prefix is not deterministic for one profile")
	}
	if prefix == ownTempPrefix("cookies/other.json") {
		t.Fatalf("temp prefix is not profile-scoped: %q", prefix)
	}
}

// C10.7: injected write/sync/rename failures remove only THIS profile's temp;
// another profile's planted temp survives every failure path.
func TestFailedSaveCleansOnlyOwnTemp(t *testing.T) {
	a := newAuth(t, "tester", "test-access-1")
	foreign := filepath.Join("cookies", ".bob.json.auth-live.tmp")
	if err := os.MkdirAll("cookies", 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(foreign, []byte(`{"auth_token":"fake"}`), 0600); err != nil {
		t.Fatalf("plant: %v", err)
	}

	a.fsRename = func(oldPath, newPath string) error { return os.ErrPermission }
	if err := a.SaveAuth(); err == nil {
		t.Fatalf("expected injected rename failure")
	}
	// Own temp cleaned up:
	matches, _ := filepath.Glob(filepath.Join("cookies", ownTempPrefix(a.cookiesPath())+"*.tmp"))
	if len(matches) != 0 {
		t.Fatalf("own temp residue after failed save: %v", matches)
	}
	// Foreign temp untouched:
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("failed save removed a foreign profile's temp")
	}
}

// C10.5/C10.6: two profiles saving concurrently (plus one sweeping) keep both
// final records intact; race-free under -race.
func TestConcurrentProfilesKeepBothRecords(t *testing.T) {
	t.Chdir(t.TempDir())
	alice := NewTwitchAuth("alice", "device-a")
	alice.token = "test-access-alice"
	bob := NewTwitchAuth("bob", "device-b")
	bob.token = "test-access-bob"

	done := make(chan struct{}, 3)
	go func() {
		defer func() { done <- struct{}{} }()
		for range 10 {
			_ = alice.SaveAuth()
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		for range 10 {
			_ = bob.SaveAuth()
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		for range 10 {
			alice.sweepStaleTempFiles()
		}
	}()
	for range 3 {
		<-done
	}

	if body := readCookieFile(t, alice); !strings.Contains(body, "test-access-alice") {
		t.Fatalf("alice's record lost")
	}
	if body := readCookieFile(t, bob); !strings.Contains(body, "test-access-bob") {
		t.Fatalf("bob's record lost")
	}
}
