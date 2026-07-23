package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// writeRawCookie writes raw bytes as the stored auth file for user "tester".
func writeRawCookie(t *testing.T, content string) {
	t.Helper()
	if err := os.MkdirAll("cookies", 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join("cookies", "tester.json"), []byte(content), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// A1: the legacy three-field plaintext record still loads.
func TestLegacyPlaintextRecordLoads(t *testing.T) {
	t.Chdir(t.TempDir())
	writeRawCookie(t, `{"auth_token":"legacy-access","user_id":"uid-9","username":"tester"}`)

	a := NewTwitchAuth("tester", "device-xyz")
	if err := a.LoadStoredAuth(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if a.GetAuthToken() != "legacy-access" || a.GetUserID() != "uid-9" {
		t.Fatalf("legacy fields not loaded")
	}
	if a.Health().HasRefreshToken {
		t.Fatalf("legacy record must load with no refresh token")
	}
}

// A2: the existing encrypted envelope v2 wrapping a LEGACY inner record loads.
func TestLegacyEncryptedEnvelopeLoads(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv(EnvEncryptionKey, "test-passphrase")

	env, err := encryptBlob([]byte(`{"auth_token":"legacy-access","user_id":"uid-9","username":"tester"}`), "test-passphrase")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	raw, _ := json.MarshalIndent(env, "", "  ")
	writeRawCookie(t, string(raw))

	a := NewTwitchAuth("tester", "device-xyz")
	if err := a.LoadStoredAuth(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if a.GetAuthToken() != "legacy-access" || a.GetUserID() != "uid-9" {
		t.Fatalf("legacy encrypted fields not loaded")
	}
}

// A3: the extended plaintext record round-trips access+refresh+metadata.
func TestExtendedPlaintextRoundTrip(t *testing.T) {
	a := newAuth(t, "tester", "test-access-1")
	a.refreshToken = "test-refresh-1"
	a.tokenType = "bearer"
	a.scopes = requiredScopes()
	a.expiresAt = time.Now().Add(4 * time.Hour)
	if err := a.SaveAuth(); err != nil {
		t.Fatalf("save: %v", err)
	}

	b := NewTwitchAuth("tester", "device-xyz")
	if err := b.LoadStoredAuth(); err != nil {
		t.Fatalf("load: %v", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.token != "test-access-1" || b.refreshToken != "test-refresh-1" || b.tokenType != "bearer" {
		t.Fatalf("extended fields did not round-trip")
	}
	if len(b.scopes) != len(requiredScopes()) || b.expiresAt.IsZero() {
		t.Fatalf("metadata did not round-trip")
	}
}

// A4: with encryption enabled, neither the access nor the refresh token is in
// cleartext on disk (and the envelope version stays 2).
func TestEncryptedRecordCarriesNoCleartextTokens(t *testing.T) {
	a := newAuth(t, "tester", "cleartext-access-token")
	a.refreshToken = "cleartext-refresh-token"
	t.Setenv(EnvEncryptionKey, "test-passphrase")
	if err := a.SaveAuth(); err != nil {
		t.Fatalf("save: %v", err)
	}
	body := readCookieFile(t, a)
	if strings.Contains(body, "cleartext-access-token") || strings.Contains(body, "cleartext-refresh-token") {
		t.Fatalf("encrypted record leaks a token in cleartext")
	}
	if !strings.Contains(body, `"version": 2`) {
		t.Fatalf("envelope version changed unnecessarily")
	}
}

// A5: plaintext -> encrypted migration on load preserves the whole extended
// snapshot.
func TestPlaintextToEncryptedMigrationKeepsExtendedFields(t *testing.T) {
	a := newAuth(t, "tester", "test-access-1")
	a.refreshToken = "test-refresh-1"
	a.scopes = requiredScopes()
	if err := a.SaveAuth(); err != nil { // plaintext (no key yet)
		t.Fatalf("save plaintext: %v", err)
	}

	t.Setenv(EnvEncryptionKey, "test-passphrase")
	b := NewTwitchAuth("tester", "device-xyz")
	if err := b.LoadStoredAuth(); err != nil { // migrates in place
		t.Fatalf("load+migrate: %v", err)
	}
	body := readCookieFile(t, b)
	if !strings.Contains(body, `"version": 2`) || strings.Contains(body, "test-refresh-1") {
		t.Fatalf("file not migrated to the encrypted envelope")
	}

	c := NewTwitchAuth("tester", "device-xyz")
	if err := c.LoadStoredAuth(); err != nil {
		t.Fatalf("reload migrated: %v", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "test-access-1" || c.refreshToken != "test-refresh-1" || len(c.scopes) == 0 {
		t.Fatalf("migration dropped extended fields")
	}
}

// A6 is asserted in TestAtomicSaveSuccess (mode 0600).

// A7: concurrent load/snapshot/save is race-free (meaningful under -race).
func TestConcurrentLoadSnapshotSave(t *testing.T) {
	a := newAuth(t, "tester", "test-access-1")
	a.refreshToken = "test-refresh-1"
	if err := a.SaveAuth(); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 25 {
				_ = a.Snapshot()
				_ = a.GetAuthToken()
				_ = a.Health()
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				_ = a.SaveAuth()
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				_ = a.LoadStoredAuth()
			}
		}()
	}
	wg.Wait()
}
