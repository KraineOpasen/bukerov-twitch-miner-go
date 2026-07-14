package auth

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// newAuth returns a TwitchAuth with a token set, working in an isolated temp cwd
// so cookiesPath() ("cookies/<user>.json") writes under the test's directory.
func newAuth(t *testing.T, user, token string) *TwitchAuth {
	t.Helper()
	t.Chdir(t.TempDir())
	a := NewTwitchAuth(user, "device-xyz")
	a.token = token
	a.userID = "uid-1"
	return a
}

func readCookieFile(t *testing.T, a *TwitchAuth) string {
	t.Helper()
	b, err := os.ReadFile(a.cookiesPath())
	if err != nil {
		t.Fatalf("read cookie file: %v", err)
	}
	return string(b)
}

func fileMode(t *testing.T, a *TwitchAuth) os.FileMode {
	t.Helper()
	fi, err := os.Stat(a.cookiesPath())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	return fi.Mode().Perm()
}

// --- crypto primitives ---

func TestEncryptDecryptRoundTrip(t *testing.T) {
	env, err := encryptBlob([]byte("super-secret-token"), "passphrase")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if env.Version != envelopeVersion || env.KDF != kdfName {
		t.Fatalf("envelope metadata wrong: %+v", env)
	}
	got, err := decryptBlob(env, "passphrase")
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if string(got) != "super-secret-token" {
		t.Fatalf("round-trip mismatch: %q", got)
	}
}

func TestDecryptWrongSecretFails(t *testing.T) {
	env, _ := encryptBlob([]byte("tok"), "right")
	if _, err := decryptBlob(env, "wrong"); err == nil {
		t.Fatal("decrypt with wrong passphrase must fail")
	}
}

func TestDecryptNoSecretFails(t *testing.T) {
	env, _ := encryptBlob([]byte("tok"), "right")
	if _, err := decryptBlob(env, ""); err == nil {
		t.Fatal("decrypt with no passphrase must fail")
	}
}

func TestDecryptTamperedCiphertextFails(t *testing.T) {
	env, _ := encryptBlob([]byte("tok"), "pass")
	// Flip the envelope's ciphertext: mangle a character so GCM auth fails.
	if strings.HasPrefix(env.Ciphertext, "A") {
		env.Ciphertext = "B" + env.Ciphertext[1:]
	} else {
		env.Ciphertext = "A" + env.Ciphertext[1:]
	}
	if _, err := decryptBlob(env, "pass"); err == nil {
		t.Fatal("tampered ciphertext must fail the AEAD check")
	}
}

func TestEncryptUsesFreshSaltAndNonce(t *testing.T) {
	a, _ := encryptBlob([]byte("tok"), "pass")
	b, _ := encryptBlob([]byte("tok"), "pass")
	if a.Salt == b.Salt {
		t.Error("salt must be random per encryption")
	}
	if a.Nonce == b.Nonce {
		t.Error("nonce must be random per encryption")
	}
	if a.Ciphertext == b.Ciphertext {
		t.Error("ciphertext must differ across encryptions of the same plaintext")
	}
}

// --- save/load: plaintext (no secret) ---

func TestSaveLoadPlaintextNoSecret(t *testing.T) {
	t.Setenv(EnvEncryptionKey, "")
	a := newAuth(t, "alice", "plain-token-123")
	if err := a.SaveAuth(); err != nil {
		t.Fatalf("save: %v", err)
	}

	body := readCookieFile(t, a)
	if !strings.Contains(body, "auth_token") || !strings.Contains(body, "plain-token-123") {
		t.Fatalf("plaintext file must contain the raw token, got:\n%s", body)
	}

	b := NewTwitchAuth("alice", "d")
	if err := b.LoadStoredAuth(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if b.token != "plain-token-123" {
		t.Fatalf("loaded token = %q", b.token)
	}
	if fileMode(t, a) != 0600 {
		t.Errorf("file mode = %o, want 0600", fileMode(t, a))
	}
}

// Scenario 1: no secret + existing plaintext → stays plaintext, no re-login.
func TestNoSecretPlaintextStaysPlaintext(t *testing.T) {
	t.Setenv(EnvEncryptionKey, "")
	a := newAuth(t, "bob", "tok-bob")
	if err := a.SaveAuth(); err != nil {
		t.Fatalf("save: %v", err)
	}
	before := readCookieFile(t, a)

	if err := a.LoadStoredAuth(); err != nil {
		t.Fatalf("load: %v", err)
	}
	after := readCookieFile(t, a)
	if before != after {
		t.Fatal("plaintext file must be unchanged when no secret is set")
	}
	if strings.Contains(after, "version") {
		t.Fatal("file must not become an encrypted envelope without a secret")
	}
}

// --- save/load: encrypted (secret set) ---

func TestSaveLoadEncryptedWithSecret(t *testing.T) {
	t.Setenv(EnvEncryptionKey, "hunter2")
	a := newAuth(t, "carol", "secret-tok-xyz")
	if err := a.SaveAuth(); err != nil {
		t.Fatalf("save: %v", err)
	}

	body := readCookieFile(t, a)
	if strings.Contains(body, "secret-tok-xyz") {
		t.Fatalf("encrypted file must NOT contain the raw token, got:\n%s", body)
	}
	if !strings.Contains(body, "\"version\": 2") {
		t.Fatalf("encrypted file must be a versioned envelope, got:\n%s", body)
	}

	b := NewTwitchAuth("carol", "d")
	if err := b.LoadStoredAuth(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if b.token != "secret-tok-xyz" {
		t.Fatalf("decrypted token = %q", b.token)
	}
	if fileMode(t, a) != 0600 {
		t.Errorf("file mode = %o, want 0600", fileMode(t, a))
	}
}

// Scenario 2: secret set later + existing plaintext → migrate-on-load to
// encrypted, no forced re-login.
func TestMigrateOnLoadPlaintextToEncrypted(t *testing.T) {
	// First write plaintext with no secret.
	t.Setenv(EnvEncryptionKey, "")
	a := newAuth(t, "dave", "migrate-me")
	if err := a.SaveAuth(); err != nil {
		t.Fatalf("save plaintext: %v", err)
	}
	if !strings.Contains(readCookieFile(t, a), "migrate-me") {
		t.Fatal("precondition: file should be plaintext")
	}

	// Now a secret appears; loading must return the token AND re-encrypt the file.
	t.Setenv(EnvEncryptionKey, "new-pass")
	b := NewTwitchAuth("dave", "d")
	if err := b.LoadStoredAuth(); err != nil {
		t.Fatalf("load with secret: %v", err)
	}
	if b.token != "migrate-me" {
		t.Fatalf("token after migrate = %q, want migrate-me", b.token)
	}
	body := readCookieFile(t, a)
	if strings.Contains(body, "migrate-me") {
		t.Fatalf("after migration the raw token must be gone, got:\n%s", body)
	}
	if !strings.Contains(body, "\"version\": 2") {
		t.Fatalf("after migration the file must be an encrypted envelope, got:\n%s", body)
	}

	// And it decrypts back with the same secret.
	c := NewTwitchAuth("dave", "d")
	if err := c.LoadStoredAuth(); err != nil {
		t.Fatalf("reload encrypted: %v", err)
	}
	if c.token != "migrate-me" {
		t.Fatalf("reloaded token = %q", c.token)
	}
}

// Scenario 3: encrypted with an old/different secret → load fails → Login falls
// back to device login. This is the only case that forces a re-login.
func TestEncryptedFileWrongSecretLoadErrors(t *testing.T) {
	t.Setenv(EnvEncryptionKey, "original")
	a := newAuth(t, "erin", "tok-erin")
	if err := a.SaveAuth(); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Operator changed the passphrase.
	t.Setenv(EnvEncryptionKey, "changed")
	b := NewTwitchAuth("erin", "d")
	if err := b.LoadStoredAuth(); err == nil {
		t.Fatal("load with a changed passphrase must error (→ device login)")
	}
	if b.token != "" {
		t.Fatalf("no token must be loaded on decrypt failure, got %q", b.token)
	}
}

// Encrypted file but the secret was removed entirely → same class as scenario 3.
func TestEncryptedFileNoSecretLoadErrors(t *testing.T) {
	t.Setenv(EnvEncryptionKey, "keep")
	a := newAuth(t, "frank", "tok-frank")
	if err := a.SaveAuth(); err != nil {
		t.Fatalf("save: %v", err)
	}

	t.Setenv(EnvEncryptionKey, "")
	b := NewTwitchAuth("frank", "d")
	if err := b.LoadStoredAuth(); err == nil {
		t.Fatal("load of an encrypted file with no secret must error (→ device login)")
	}
}

// The persisted envelope must be valid JSON with no secret leakage.
func TestEncryptedEnvelopeShape(t *testing.T) {
	t.Setenv(EnvEncryptionKey, "pw")
	a := newAuth(t, "gina", "leaky-token-should-not-appear")
	if err := a.SaveAuth(); err != nil {
		t.Fatalf("save: %v", err)
	}
	var env encryptedEnvelope
	if err := json.Unmarshal([]byte(readCookieFile(t, a)), &env); err != nil {
		t.Fatalf("envelope is not valid JSON: %v", err)
	}
	if env.Salt == "" || env.Nonce == "" || env.Ciphertext == "" {
		t.Fatalf("envelope missing crypto material: %+v", env)
	}
	if env.KDF != "pbkdf2-sha256" || env.Iter != pbkdf2Iter {
		t.Errorf("envelope KDF params wrong: %+v", env)
	}
}
