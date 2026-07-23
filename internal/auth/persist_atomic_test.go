package auth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// noTempResidue asserts no .auth-*.tmp files remain in the cookies directory.
func noTempResidue(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir("cookies")
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("read cookies dir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".auth-") && strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file residue: %s", e.Name())
		}
	}
}

// B1: a successful save atomically lands the record with no temp residue.
func TestAtomicSaveSuccess(t *testing.T) {
	a := newAuth(t, "tester", "test-access-1")
	a.refreshToken = "test-refresh-1"
	if err := a.SaveAuth(); err != nil {
		t.Fatalf("save: %v", err)
	}
	body := readCookieFile(t, a)
	if !strings.Contains(body, "test-access-1") || !strings.Contains(body, "test-refresh-1") {
		t.Fatalf("saved record incomplete")
	}
	noTempResidue(t)
	if mode := fileMode(t, a); mode != 0600 {
		t.Fatalf("file mode = %o, want 0600", mode)
	}
}

// B2/B3/B4/B5: injected write/sync/rename failures leave the previous final
// file byte-identical and no temp residue behind.
func TestAtomicSaveFailuresPreserveOldFile(t *testing.T) {
	inject := map[string]func(a *TwitchAuth){
		"write": func(a *TwitchAuth) {
			a.fsWrite = func(f *os.File, data []byte) error { return errors.New("injected write failure") }
		},
		"sync": func(a *TwitchAuth) {
			a.fsSync = func(f *os.File) error { return errors.New("injected sync failure") }
		},
		"rename": func(a *TwitchAuth) {
			a.fsRename = func(oldPath, newPath string) error { return errors.New("injected rename failure") }
		},
	}
	for name, apply := range inject {
		t.Run(name, func(t *testing.T) {
			a := newAuth(t, "tester", "test-access-old")
			a.refreshToken = "test-refresh-old"
			if err := a.SaveAuth(); err != nil {
				t.Fatalf("seed save: %v", err)
			}
			before := readCookieFile(t, a)

			a.token = "test-access-new"
			a.refreshToken = "test-refresh-new"
			apply(a)
			if err := a.SaveAuth(); err == nil {
				t.Fatalf("expected injected %s failure to surface", name)
			}

			if after := readCookieFile(t, a); after != before {
				t.Fatalf("%s failure corrupted the previous final file", name)
			}
			noTempResidue(t)
		})
	}
}

// B6: after a rotation, a completed save always carries the NEW pair — an
// older in-flight save can never resurrect the consumed one-time token.
func TestSaveAlwaysCarriesNewestPair(t *testing.T) {
	a := newAuth(t, "tester", "test-access-1")
	a.refreshToken = "test-refresh-1"
	if err := a.SaveAuth(); err != nil {
		t.Fatalf("save1: %v", err)
	}

	a.publishTokenPair(&TokenResponse{
		AccessToken: "test-access-2", RefreshToken: "test-refresh-2",
		ExpiresIn: 14000, TokenType: "bearer",
	})
	// publishTokenPair -> adopt path normally saves; here we saved manually
	// before, then rotate and save again: the final file must hold pair 2.
	if err := a.SaveAuth(); err != nil {
		t.Fatalf("save2: %v", err)
	}
	body := readCookieFile(t, a)
	if strings.Contains(body, "test-refresh-1") || !strings.Contains(body, "test-refresh-2") {
		t.Fatalf("final file does not hold the newest rotated pair")
	}
}

// B7/B8 (unit level; the checkpoint integration lives in
// TestValidationCheckpointRetriesPendingPersistence): a persistence failure
// after a rotation keeps the new pair authoritative in memory, marks it
// pending, and the next explicit save lands exactly that pair.
func TestPersistFailureKeepsRotatedPairAuthoritative(t *testing.T) {
	a := newAuth(t, "tester", "test-access-1")
	a.refreshToken = "test-refresh-1"
	if err := a.SaveAuth(); err != nil {
		t.Fatalf("seed: %v", err)
	}

	failing := true
	a.fsRename = func(oldPath, newPath string) error {
		if failing {
			return errors.New("injected rename failure")
		}
		return os.Rename(oldPath, newPath)
	}

	a.adoptTokenPair(&TokenResponse{
		AccessToken: "test-access-2", RefreshToken: "test-refresh-2",
		ExpiresIn: 14000, TokenType: "bearer",
	})

	if a.GetAuthToken() != "test-access-2" {
		t.Fatalf("rotated pair lost from memory after persist failure")
	}
	h := a.Health()
	if !h.PersistPending || !h.HasRefreshToken {
		t.Fatalf("state not marked persist-pending with the new pair: %+v", h)
	}
	// Old pair still on disk (the crash window), never an empty/partial file.
	if body := readCookieFile(t, a); !strings.Contains(body, "test-refresh-1") {
		t.Fatalf("failed persist corrupted the previous record")
	}

	failing = false
	if err := a.SaveAuth(); err != nil {
		t.Fatalf("checkpoint save: %v", err)
	}
	if a.Health().PersistPending {
		t.Fatalf("pending flag not cleared after successful checkpoint")
	}
	body := readCookieFile(t, a)
	if strings.Contains(body, "test-refresh-1") || !strings.Contains(body, "test-refresh-2") {
		t.Fatalf("checkpoint persisted the wrong pair")
	}
	noTempResidue(t)
}

// The temp file name and injected error paths never leak token material.
func TestAtomicSaveErrorsCarryNoSecrets(t *testing.T) {
	a := newAuth(t, "tester", "very-secret-access-token")
	a.refreshToken = "very-secret-refresh-token"
	a.fsSync = func(f *os.File) error { return errors.New("disk full") }
	err := a.SaveAuth()
	if err == nil {
		t.Fatalf("expected failure")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("error leaks secret material: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Dir(a.cookiesPath()))
	for _, e := range entries {
		if strings.Contains(e.Name(), "secret") {
			t.Fatalf("temp file name leaks secret material: %s", e.Name())
		}
	}
}
