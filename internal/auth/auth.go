package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
)

// plaintextWarnOnce ensures the "token stored unencrypted" warning is logged at
// most once per process, whether it is first hit on save or on load.
var plaintextWarnOnce sync.Once

func warnPlaintextOnce() {
	plaintextWarnOnce.Do(func() {
		slog.Warn("Twitch auth token is stored UNENCRYPTED at rest; set "+
			EnvEncryptionKey+" to a passphrase to encrypt it (AES-256-GCM)",
			"file", "cookies/*.json")
	})
}

var (
	ErrBadCredentials       = errors.New("bad credentials")
	ErrExpiredCode          = errors.New("device code expired")
	ErrAuthorizationPending = errors.New("authorization pending")
)

type DeviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
}

type TokenResponse struct {
	AccessToken  string   `json:"access_token"`
	RefreshToken string   `json:"refresh_token"`
	ExpiresIn    int      `json:"expires_in"`
	Scope        []string `json:"scope"`
	TokenType    string   `json:"token_type"`
}

type StoredAuth struct {
	AuthToken string `json:"auth_token"`
	UserID    string `json:"user_id"`
	Username  string `json:"username"`
}

type AuthEventCallback func(event AuthEvent)

type AuthEventType string

const (
	AuthEventStarted   AuthEventType = "started"
	AuthEventCode      AuthEventType = "code"
	AuthEventCompleted AuthEventType = "completed"
	AuthEventError     AuthEventType = "error"
)

type AuthEvent struct {
	Type            AuthEventType
	VerificationURI string
	UserCode        string
	ExpiresIn       int
	Error           error
}

type TwitchAuth struct {
	clientID      string
	deviceID      string
	username      string
	token         string
	userID        string
	client        *http.Client
	eventCallback AuthEventCallback
}

func NewTwitchAuth(username, deviceID string) *TwitchAuth {
	return &TwitchAuth{
		clientID: constants.ClientIDTV,
		deviceID: deviceID,
		username: strings.ToLower(strings.TrimSpace(username)),
		client:   &http.Client{Timeout: 30 * time.Second},
	}
}

func (a *TwitchAuth) GetAuthToken() string {
	return a.token
}

func (a *TwitchAuth) GetUserID() string {
	return a.userID
}

func (a *TwitchAuth) GetUsername() string {
	return a.username
}

func (a *TwitchAuth) SetToken(token string) {
	a.token = token
}

func (a *TwitchAuth) SetUserID(userID string) {
	a.userID = userID
}

func (a *TwitchAuth) SetEventCallback(callback AuthEventCallback) {
	a.eventCallback = callback
}

func (a *TwitchAuth) emitEvent(event AuthEvent) {
	if a.eventCallback != nil {
		a.eventCallback(event)
	}
}

func (a *TwitchAuth) cookiesPath() string {
	return filepath.Join("cookies", fmt.Sprintf("%s.json", a.username))
}

// LoadStoredAuth reads the persisted auth for this user. It transparently
// handles both on-disk formats:
//
//   - Encrypted envelope (version >= 2): decrypted with the passphrase from
//     TWITCH_AUTH_ENCRYPTION_KEY. If the passphrase is missing, changed, or the
//     file was tampered with, decryption fails and this returns an error — the
//     caller (Login) then falls back to a fresh device login. This is the only
//     situation that forces a re-login.
//   - Legacy plaintext (no version field): loaded as-is. If a passphrase is now
//     set, the file is migrated in place to the encrypted format on load (no
//     re-login needed); if not, a one-time warning is logged and it stays
//     plaintext.
func (a *TwitchAuth) LoadStoredAuth() error {
	data, err := os.ReadFile(a.cookiesPath())
	if err != nil {
		return err
	}

	// Detect the format: the encrypted envelope carries a "version" field, the
	// legacy plaintext record does not.
	var probe struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}

	secret := encryptionSecret()

	if probe.Version >= envelopeVersion {
		var env encryptedEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			return err
		}
		plaintext, err := decryptBlob(env, secret)
		if err != nil {
			return err
		}
		var stored StoredAuth
		if err := json.Unmarshal(plaintext, &stored); err != nil {
			return err
		}
		a.applyStored(stored)
		return nil
	}

	// Legacy plaintext.
	var stored StoredAuth
	if err := json.Unmarshal(data, &stored); err != nil {
		return err
	}
	a.applyStored(stored)

	if secret != "" {
		// A passphrase is now configured but the file is still plaintext:
		// migrate it in place to the encrypted format without forcing a login.
		if err := a.SaveAuth(); err != nil {
			slog.Warn("Failed to migrate plaintext auth token to encrypted form", "error", err)
		} else {
			slog.Info("Migrated stored auth token to encrypted form (AES-256-GCM)")
		}
	} else {
		warnPlaintextOnce()
	}
	return nil
}

func (a *TwitchAuth) applyStored(stored StoredAuth) {
	a.token = stored.AuthToken
	a.userID = stored.UserID
	a.username = stored.Username
}

// SaveAuth persists the current auth for this user. When
// TWITCH_AUTH_ENCRYPTION_KEY is set, the record is AES-256-GCM encrypted at rest;
// otherwise it is written in plaintext (with a one-time warning). The file is
// always mode 0600 regardless of format.
func (a *TwitchAuth) SaveAuth() error {
	if err := os.MkdirAll("cookies", 0755); err != nil {
		return err
	}

	stored := StoredAuth{
		AuthToken: a.token,
		UserID:    a.userID,
		Username:  a.username,
	}

	inner, err := json.Marshal(stored)
	if err != nil {
		return err
	}

	secret := encryptionSecret()

	var data []byte
	if secret != "" {
		env, err := encryptBlob(inner, secret)
		if err != nil {
			return err
		}
		data, err = json.MarshalIndent(env, "", "  ")
		if err != nil {
			return err
		}
	} else {
		warnPlaintextOnce()
		// Preserve the historical human-readable plaintext layout.
		data, err = json.MarshalIndent(stored, "", "  ")
		if err != nil {
			return err
		}
	}

	return os.WriteFile(a.cookiesPath(), data, 0600)
}

func (a *TwitchAuth) DeleteStoredAuth() error {
	return os.Remove(a.cookiesPath())
}

func (a *TwitchAuth) HasStoredAuth() bool {
	_, err := os.Stat(a.cookiesPath())
	return err == nil
}

func (a *TwitchAuth) Login() error {
	if a.HasStoredAuth() {
		if err := a.LoadStoredAuth(); err == nil && a.token != "" {
			return nil
		}
	}

	return a.DeviceFlowLogin()
}

func (a *TwitchAuth) DeviceFlowLogin() error {
	a.emitEvent(AuthEvent{Type: AuthEventStarted})

	deviceCode, err := a.requestDeviceCode()
	if err != nil {
		a.emitEvent(AuthEvent{Type: AuthEventError, Error: err})
		return fmt.Errorf("failed to get device code: %w", err)
	}

	fmt.Println("\n=== Twitch Login Required ===")
	fmt.Printf("Open: %s\n", deviceCode.VerificationURI)
	fmt.Printf("Enter code: %s\n", deviceCode.UserCode)
	fmt.Printf("Code expires in %d minutes\n", deviceCode.ExpiresIn/60)
	fmt.Println("Waiting for authorization...")

	a.emitEvent(AuthEvent{
		Type:            AuthEventCode,
		VerificationURI: deviceCode.VerificationURI,
		UserCode:        deviceCode.UserCode,
		ExpiresIn:       deviceCode.ExpiresIn,
	})

	token, err := a.pollForToken(deviceCode)
	if err != nil {
		a.emitEvent(AuthEvent{Type: AuthEventError, Error: err})
		return fmt.Errorf("failed to get token: %w", err)
	}

	a.token = token.AccessToken

	if err := a.SaveAuth(); err != nil {
		a.emitEvent(AuthEvent{Type: AuthEventError, Error: err})
		return fmt.Errorf("failed to save auth: %w", err)
	}

	a.emitEvent(AuthEvent{Type: AuthEventCompleted})
	return nil
}

func (a *TwitchAuth) requestDeviceCode() (*DeviceCodeResponse, error) {
	data := url.Values{
		"client_id": {a.clientID},
		"scopes":    {constants.OAuthScopes},
	}

	req, err := http.NewRequest("POST", constants.OAuthDeviceURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Client-Id", a.clientID)
	req.Header.Set("X-Device-Id", a.deviceID)
	req.Header.Set("User-Agent", constants.TVUserAgent)

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var deviceCode DeviceCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&deviceCode); err != nil {
		return nil, err
	}

	return &deviceCode, nil
}

func (a *TwitchAuth) pollForToken(deviceCode *DeviceCodeResponse) (*TokenResponse, error) {
	deadline := time.Now().Add(time.Duration(deviceCode.ExpiresIn) * time.Second)
	interval := time.Duration(deviceCode.Interval) * time.Second

	for time.Now().Before(deadline) {
		time.Sleep(interval)

		token, err := a.requestToken(deviceCode.DeviceCode)
		if err == ErrAuthorizationPending {
			continue
		}
		if err != nil {
			return nil, err
		}

		return token, nil
	}

	return nil, ErrExpiredCode
}

func (a *TwitchAuth) requestToken(deviceCode string) (*TokenResponse, error) {
	data := url.Values{
		"client_id":   {a.clientID},
		"device_code": {deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}

	req, err := http.NewRequest("POST", constants.OAuthTokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Client-Id", a.clientID)
	req.Header.Set("X-Device-Id", a.deviceID)
	req.Header.Set("User-Agent", constants.TVUserAgent)

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusBadRequest {
		return nil, ErrAuthorizationPending
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var token TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&token); err != nil {
		return nil, err
	}

	return &token, nil
}
