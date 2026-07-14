package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// EnvEncryptionKey is the environment variable that, when set to a non-empty
// passphrase, enables AES-256-GCM encryption of the stored auth token at rest.
// It is deliberately an env var, never a config-file field: config.json is itself
// plaintext on disk, so putting the passphrase there would defeat the purpose.
// When unset, the token is stored in plaintext (with a one-time warning) so that
// existing installs and headless auto-restart keep working unchanged.
const EnvEncryptionKey = "TWITCH_AUTH_ENCRYPTION_KEY"

const (
	// envelopeVersion tags the encrypted on-disk format so it can be told apart
	// from the legacy plaintext file (which has no "version" field) and evolved
	// later without ambiguity.
	envelopeVersion = 2

	// pbkdf2Iter is the PBKDF2-HMAC-SHA256 iteration count (OWASP 2023 guidance
	// for that PRF). Stored in the envelope so a future bump stays readable.
	pbkdf2Iter = 600000

	kdfName  = "pbkdf2-sha256"
	keyLen   = 32 // AES-256
	saltLen  = 16
	nonceLen = 12 // AES-GCM standard nonce size
)

// errNoSecret means an encrypted file was found but no passphrase is available to
// decrypt it. The caller treats this like any load failure: fall back to device
// login rather than crash.
var errNoSecret = errors.New("auth token is encrypted but no encryption key is set")

// encryptedEnvelope is the versioned on-disk format when encryption is enabled.
// It carries no secret in the clear — only the KDF parameters and the AEAD
// ciphertext of the inner StoredAuth JSON.
type encryptedEnvelope struct {
	Version    int    `json:"version"`
	KDF        string `json:"kdf"`
	Iter       int    `json:"iter"`
	Salt       string `json:"salt"`       // base64
	Nonce      string `json:"nonce"`      // base64
	Ciphertext string `json:"ciphertext"` // base64
}

// encryptionSecret returns the trimmed passphrase from the environment, or "" if
// encryption is not configured. The value itself is never logged.
func encryptionSecret() string {
	return strings.TrimSpace(os.Getenv(EnvEncryptionKey))
}

// deriveKey stretches the passphrase into a 32-byte AES key with PBKDF2-SHA256.
func deriveKey(secret string, salt []byte, iter int) ([]byte, error) {
	return pbkdf2.Key(sha256.New, secret, salt, iter, keyLen)
}

// encryptBlob seals plaintext under a fresh random salt+nonce and returns the
// envelope. The derived key is zeroed before returning.
func encryptBlob(plaintext []byte, secret string) (encryptedEnvelope, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return encryptedEnvelope{}, err
	}
	key, err := deriveKey(secret, salt, pbkdf2Iter)
	if err != nil {
		return encryptedEnvelope{}, err
	}
	defer zero(key)

	block, err := aes.NewCipher(key)
	if err != nil {
		return encryptedEnvelope{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return encryptedEnvelope{}, err
	}
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return encryptedEnvelope{}, err
	}
	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)

	return encryptedEnvelope{
		Version:    envelopeVersion,
		KDF:        kdfName,
		Iter:       pbkdf2Iter,
		Salt:       base64.StdEncoding.EncodeToString(salt),
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(ciphertext),
	}, nil
}

// decryptBlob opens an envelope with the given passphrase. A wrong/changed
// passphrase or tampered ciphertext fails the AEAD check and returns an error
// (never a panic), which the caller turns into a device-login fallback.
func decryptBlob(env encryptedEnvelope, secret string) ([]byte, error) {
	if secret == "" {
		return nil, errNoSecret
	}
	salt, err := base64.StdEncoding.DecodeString(env.Salt)
	if err != nil {
		return nil, fmt.Errorf("decode salt: %w", err)
	}
	nonce, err := base64.StdEncoding.DecodeString(env.Nonce)
	if err != nil {
		return nil, fmt.Errorf("decode nonce: %w", err)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(env.Ciphertext)
	if err != nil {
		return nil, fmt.Errorf("decode ciphertext: %w", err)
	}

	iter := env.Iter
	if iter <= 0 {
		iter = pbkdf2Iter
	}
	key, err := deriveKey(secret, salt, iter)
	if err != nil {
		return nil, err
	}
	defer zero(key)

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		// Wrong passphrase or tampered file — indistinguishable by design.
		return nil, fmt.Errorf("decrypt auth token: %w", err)
	}
	return plaintext, nil
}

// zero best-effort wipes sensitive key bytes after use.
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
