package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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

	// ErrAccessDenied is the terminal device-flow outcome when the user
	// declined the authorization request.
	ErrAccessDenied = errors.New("device authorization denied")

	// ErrInvalidDeviceCode is the terminal device-flow outcome when the device
	// code was already used or is otherwise rejected as invalid by Twitch.
	ErrInvalidDeviceCode = errors.New("invalid device code")

	// ErrAuthTransient classifies a token-endpoint or validate-endpoint failure
	// that proves nothing about the credentials: transport errors, timeouts,
	// HTTP 429 and 5xx. Callers must keep the current credentials and retry
	// later — a transient failure never triggers a device-code login.
	ErrAuthTransient = errors.New("transient twitch auth endpoint failure")

	// ErrAuthProtocol classifies a structurally invalid response from a Twitch
	// auth endpoint (malformed success payload, undocumented rejection). It is
	// fail-closed: the response is not trusted, but it is also not proof the
	// stored credentials are invalid, so they are never destroyed over it.
	ErrAuthProtocol = errors.New("twitch auth protocol error")

	// ErrIdentityMismatch means the validated token belongs to a different
	// Twitch account than the one this miner profile is bound to. The stored
	// credentials are not adopted (and not deleted); a fresh device login for
	// the configured profile is required.
	ErrIdentityMismatch = errors.New("stored credentials belong to a different twitch account")

	// errSlowDown is the RFC 8628 slow_down polling hint. Handled internally by
	// pollForToken (interval increase); never returned to callers.
	errSlowDown = errors.New("device token polling told to slow down")
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

// StoredAuth is the persisted on-disk auth record (inner JSON of the encrypted
// envelope, or the whole plaintext file). The refresh/metadata fields are
// omitempty additions on top of the legacy three-field record, so an old file
// loads unchanged and an old binary reading a new file simply ignores them.
type StoredAuth struct {
	AuthToken    string   `json:"auth_token"`
	UserID       string   `json:"user_id"`
	Username     string   `json:"username"`
	RefreshToken string   `json:"refresh_token,omitempty"`
	TokenType    string   `json:"token_type,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
	// ExpiresAt is the RFC3339 UTC time the access token expires (informational
	// metadata; the authoritative expiry is re-learned from /oauth2/validate).
	ExpiresAt string `json:"expires_at,omitempty"`
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

// Snapshot is an immutable, mutually consistent view of the current credential
// set for request signing. Generation identifies the credential set the token
// belongs to: Recover(rejectedGeneration) uses it to tell a stale rejection of
// an already-rotated token from a rejection of the current one. It never
// carries the refresh token.
type Snapshot struct {
	AccessToken string
	UserID      string
	Username    string
	Generation  uint64
}

// TwitchAuth owns the OAuth credential lifecycle: device-code login, startup
// and hourly /oauth2/validate, single-flight refresh/device-flow recovery, and
// crash-safe persistence. All credential state is guarded by mu; network and
// filesystem I/O never run under it. Lock order: mu is a leaf lock (nothing
// else is acquired while holding it, and callbacks are invoked with it
// released); saveMu serializes whole persistence cycles and may acquire mu
// briefly for state snapshots, never the reverse.
type TwitchAuth struct {
	clientID string
	deviceID string
	client   *http.Client

	// Endpoint URLs default to the constants and are overridden only by tests
	// (same pattern as api.TwitchClient.setGQLEndpoint).
	deviceURL   string
	tokenURL    string
	validateURL string

	mu           sync.Mutex
	username     string
	token        string
	refreshToken string
	userID       string
	tokenType    string
	scopes       []string
	expiresAt    time.Time
	// generation is the monotonic credential-set revision: bumped exactly once
	// per published token pair (device login or refresh), never on
	// metadata-only validation updates.
	generation uint64
	// validationState is diagnostics metadata: "unknown" until the first
	// /oauth2/validate outcome, then "valid" or "degraded:<reason>". Never
	// carries token material.
	validationState string
	validatedAt     time.Time
	// persistDirty marks credentials that are authoritative in memory but not
	// yet safely on disk (a rotation whose persistence failed). Safe
	// checkpoints (hourly validation, the next rotation, explicit SaveAuth)
	// retry until it clears.
	persistDirty bool

	eventCallback    AuthEventCallback
	rotationCallback func(generation uint64)

	// lifecycleCtx bounds the recovery OWNER's work (refresh HTTP, device-flow
	// polling). It is deliberately decoupled from any single caller's context:
	// a caller that stops waiting (bounded GQL wait, cancelled waiter) must
	// not abort the shared flight — only process shutdown does. Defaults to
	// context.Background(); the miner binds it to the run context.
	lifecycleCtx context.Context

	// Single-flight recovery state: exactly one owner per credential
	// generation runs refresh/device-flow; concurrent callers wait on
	// recoverDone and re-check the generation.
	recovering  bool
	recoverDone chan struct{}
	recoverErr  error

	validatorRunning bool

	// saveMu serializes whole SaveAuth cycles (snapshot, marshal, atomic
	// replace) so concurrent saves cannot interleave temp files or write an
	// older snapshot after a newer one.
	saveMu sync.Mutex

	// Deterministic test seams. now/timerAfter drive polling and expiry
	// without wall-clock sleeps; the fs hooks inject write/sync/rename
	// failures into the atomic persistence path. All default to the real
	// implementations in NewTwitchAuth.
	now        func() time.Time
	timerAfter func(d time.Duration) <-chan time.Time
	fsWrite    func(f *os.File, data []byte) error
	fsSync     func(f *os.File) error
	fsRename   func(oldPath, newPath string) error
}

func NewTwitchAuth(username, deviceID string) *TwitchAuth {
	return &TwitchAuth{
		clientID:        constants.ClientIDTV,
		deviceID:        deviceID,
		username:        strings.ToLower(strings.TrimSpace(username)),
		client:          &http.Client{Timeout: 30 * time.Second},
		deviceURL:       constants.OAuthDeviceURL,
		tokenURL:        constants.OAuthTokenURL,
		validateURL:     constants.OAuthValidateURL,
		validationState: "unknown",
		now:             time.Now,
		timerAfter:      func(d time.Duration) <-chan time.Time { return time.After(d) },
		fsWrite:         func(f *os.File, data []byte) error { _, err := f.Write(data); return err },
		fsSync:          func(f *os.File) error { return f.Sync() },
		fsRename:        os.Rename,
	}
}

func (a *TwitchAuth) GetAuthToken() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.token
}

func (a *TwitchAuth) GetUserID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.userID
}

func (a *TwitchAuth) GetUsername() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.username
}

func (a *TwitchAuth) SetToken(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.token = token
}

func (a *TwitchAuth) SetUserID(userID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.userID = userID
}

// Snapshot returns a mutually consistent view of the current credential set.
// Callers that sign a request MUST capture it once and pass Generation to
// Recover if the request is authoritatively rejected.
func (a *TwitchAuth) Snapshot() Snapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.snapshotLocked()
}

func (a *TwitchAuth) snapshotLocked() Snapshot {
	return Snapshot{
		AccessToken: a.token,
		UserID:      a.userID,
		Username:    a.username,
		Generation:  a.generation,
	}
}

// Generation returns the current credential-set revision.
func (a *TwitchAuth) Generation() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.generation
}

func (a *TwitchAuth) SetEventCallback(callback AuthEventCallback) {
	a.mu.Lock()
	a.eventCallback = callback
	a.mu.Unlock()
}

// SetLifecycleContext binds the context that bounds recovery-owner work
// (refresh requests, device-flow polling). Cancelling it — process shutdown —
// aborts any in-flight recovery; individual callers' contexts only bound their
// own wait. Call before Login/goroutines start.
func (a *TwitchAuth) SetLifecycleContext(ctx context.Context) {
	a.mu.Lock()
	a.lifecycleCtx = ctx
	a.mu.Unlock()
}

// recoveryContext returns the owner-work context (nil-safe).
func (a *TwitchAuth) recoveryContext() context.Context {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lifecycleCtx != nil {
		return a.lifecycleCtx
	}
	return context.Background()
}

// SetRotationCallback registers a callback invoked (outside every auth lock)
// each time a new credential generation is published, so long-lived consumers
// (PubSub user topics) can re-authorize. It receives only the generation
// number, never token material.
func (a *TwitchAuth) SetRotationCallback(callback func(generation uint64)) {
	a.mu.Lock()
	a.rotationCallback = callback
	a.mu.Unlock()
}

func (a *TwitchAuth) emitEvent(event AuthEvent) {
	a.mu.Lock()
	cb := a.eventCallback
	a.mu.Unlock()
	if cb != nil {
		cb(event)
	}
}

func (a *TwitchAuth) cookiesPath() string {
	a.mu.Lock()
	username := a.username
	a.mu.Unlock()
	return filepath.Join("cookies", fmt.Sprintf("%s.json", username))
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
//
// Both formats accept the legacy three-field record and the extended record
// carrying the refresh token and expiry metadata.
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
	var expiresAt time.Time
	if stored.ExpiresAt != "" {
		// Informational metadata: a malformed timestamp is tolerated (zero
		// value) — /oauth2/validate re-learns the authoritative expiry.
		expiresAt, _ = time.Parse(time.RFC3339, stored.ExpiresAt)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.token = stored.AuthToken
	a.userID = stored.UserID
	// The CONFIGURED profile name stays authoritative: adopting the file's
	// username would silently re-key cookiesPath (and thus every later save)
	// to whatever identity the record claims — a foreign record could then
	// contaminate another profile's file. Only an empty configured name
	// (library use) falls back to the stored one.
	if a.username == "" {
		a.username = strings.ToLower(strings.TrimSpace(stored.Username))
	}
	a.refreshToken = stored.RefreshToken
	a.tokenType = stored.TokenType
	a.scopes = slices.Clone(stored.Scopes)
	a.expiresAt = expiresAt
}

func (a *TwitchAuth) DeleteStoredAuth() error {
	return os.Remove(a.cookiesPath())
}

func (a *TwitchAuth) HasStoredAuth() bool {
	_, err := os.Stat(a.cookiesPath())
	return err == nil
}

// Login establishes a working credential set, in order of preference:
//
//  1. No stored auth (or an unreadable/undecryptable record) — fresh device
//     login. The unreadable file is never overwritten with an empty record; it
//     is replaced only by a successful login's real credentials.
//  2. Stored auth loads — the token is validated against /oauth2/validate
//     (Twitch requires validation on startup and hourly thereafter):
//     - 200 with the expected client ID, required scopes, and matching
//     identity: startup continues, no device flow.
//     - authoritative 401: the shared single-flight recovery runs (refresh
//     first when a refresh token is stored, device flow otherwise).
//     - transport failure / 429 / 5xx / malformed 200 / client-ID or scope
//     anomalies: the loaded token is KEPT and startup continues in a
//     degraded validation state — a transient or inconclusive validation
//     outcome never destroys persisted credentials and never forces a
//     device login. The hourly validator (or a real 401) settles it.
//     - identity mismatch (the token belongs to a different Twitch account
//     than this profile): the stored credentials are not adopted and a
//     fresh device login runs for the configured profile.
func (a *TwitchAuth) Login(ctx context.Context) error {
	// Best-effort sweep of temp files orphaned by a crash mid-save (they can
	// hold a superseded plaintext record). Startup, before any save runs, is
	// the one point no save can be in flight.
	a.sweepStaleTempFiles()

	if a.HasStoredAuth() {
		if err := a.LoadStoredAuth(); err == nil && a.GetAuthToken() != "" {
			status, rejectedGen, verr := a.validateAndApplyCurrent(ctx)
			switch status {
			case ValidateStatusValid:
				return nil
			case ValidateStatusUnauthorized:
				_, rerr := a.Recover(ctx, rejectedGen)
				return rerr
			case ValidateStatusIdentityMismatch:
				// Fall through to a fresh device login for this profile; the
				// foreign credentials are neither adopted nor deleted. The
				// foreign user ID loaded from the record must not leak into
				// the fresh session either.
				slog.Warn("Stored Twitch credentials belong to a different account; starting a fresh device login for this profile")
				a.SetUserID("")
			default:
				// Transient / protocol / client-ID / scope anomalies: keep the
				// loaded token; continue degraded (see doc comment).
				slog.Warn("Could not conclusively validate the stored Twitch token; continuing with it in a degraded state",
					"status", string(status), "error", verr)
				return nil
			}
		}
	}

	return a.DeviceFlowLogin(ctx)
}

// DeviceFlowLogin runs one complete device-code authorization and publishes
// the resulting token pair. Auth events (Started/Code/Completed/Error) are
// emitted exactly once per flow.
func (a *TwitchAuth) DeviceFlowLogin(ctx context.Context) error {
	token, err := a.deviceFlowAuthenticate(ctx)
	if err != nil {
		return err
	}

	a.adoptTokenPair(token)

	a.emitEvent(AuthEvent{Type: AuthEventCompleted})
	return nil
}

// deviceFlowAuthenticate performs the device-code request + polling and
// returns the granted token pair WITHOUT publishing it. It emits the
// Started/Code events on begin and the Error event on failure; the caller owns
// publication and the Completed event.
func (a *TwitchAuth) deviceFlowAuthenticate(ctx context.Context) (*TokenResponse, error) {
	a.emitEvent(AuthEvent{Type: AuthEventStarted})

	deviceCode, err := a.requestDeviceCode(ctx)
	if err != nil {
		a.emitEvent(AuthEvent{Type: AuthEventError, Error: err})
		return nil, fmt.Errorf("failed to get device code: %w", err)
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

	token, err := a.pollForToken(ctx, deviceCode)
	if err != nil {
		a.emitEvent(AuthEvent{Type: AuthEventError, Error: err})
		return nil, fmt.Errorf("failed to get token: %w", err)
	}

	return token, nil
}

// adoptTokenPair publishes a granted token pair as the new authoritative
// credential generation, persists it, and only THEN notifies rotation
// consumers. Persisting first keeps the crash window for the freshly rotated
// one-time refresh token as narrow as the disk write itself — a consumer sweep
// (PubSub re-LISTENs, network I/O of unbounded duration) must never sit
// between Twitch consuming the old token and the new pair reaching disk. A
// persistence failure never discards the pair: it stays authoritative in
// memory, is marked persist-pending, and is retried at the next safe
// checkpoint (hourly validation, next rotation, explicit SaveAuth).
//
// The rotation callback runs on its own goroutine so a slow consumer sweep
// cannot block the recovery flight's completion (waiters are released by the
// flight, not by the sweep).
func (a *TwitchAuth) adoptTokenPair(token *TokenResponse) {
	gen := a.publishTokenPair(token)

	if err := a.SaveAuth(); err != nil {
		slog.Warn("Failed to persist rotated Twitch credentials; keeping the new pair authoritative in memory and retrying at the next checkpoint",
			"generation", gen, "error", err)
	}

	a.mu.Lock()
	cb := a.rotationCallback
	a.mu.Unlock()
	if cb != nil {
		go cb(gen)
	}
}

// publishTokenPair installs the complete pair under the state lock and bumps
// the generation exactly once. Old and new fields are never observable
// interleaved: every reader goes through the same mutex. Notification and
// persistence are owned by adoptTokenPair.
func (a *TwitchAuth) publishTokenPair(token *TokenResponse) uint64 {
	a.mu.Lock()
	a.token = token.AccessToken
	a.refreshToken = token.RefreshToken
	a.tokenType = token.TokenType
	a.scopes = slices.Clone(token.Scope)
	if token.ExpiresIn > 0 {
		a.expiresAt = a.now().Add(time.Duration(token.ExpiresIn) * time.Second)
	} else {
		a.expiresAt = time.Time{}
	}
	a.generation++
	gen := a.generation
	a.validationState = "valid"
	a.validatedAt = a.now()
	a.persistDirty = true
	a.mu.Unlock()

	slog.Info("Published rotated Twitch credentials", "generation", gen)
	return gen
}

func (a *TwitchAuth) requestDeviceCode(ctx context.Context) (*DeviceCodeResponse, error) {
	data := url.Values{
		"client_id": {a.clientID},
		"scopes":    {constants.OAuthScopes},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", a.deviceURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	a.setOAuthHeaders(req)

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: device code request failed", ErrAuthTransient)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, classifyOAuthFailure(resp)
	}

	var deviceCode DeviceCodeResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxOAuthBodyBytes)).Decode(&deviceCode); err != nil {
		return nil, fmt.Errorf("%w: undecodable device code response", ErrAuthProtocol)
	}
	if deviceCode.DeviceCode == "" || deviceCode.UserCode == "" || deviceCode.VerificationURI == "" || deviceCode.ExpiresIn <= 0 {
		return nil, fmt.Errorf("%w: incomplete device code response", ErrAuthProtocol)
	}

	return &deviceCode, nil
}

// pollForToken polls the token endpoint until the user authorizes, the device
// code expires, the flow is terminally rejected, or ctx is cancelled. The
// server-provided interval is respected (and grown by 5s on an RFC 8628
// slow_down); transient endpoint failures keep polling within the deadline.
func (a *TwitchAuth) pollForToken(ctx context.Context, deviceCode *DeviceCodeResponse) (*TokenResponse, error) {
	deadline := a.now().Add(time.Duration(deviceCode.ExpiresIn) * time.Second)
	interval := time.Duration(deviceCode.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}

	for a.now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-a.timerAfter(interval):
		}

		token, err := a.requestToken(ctx, deviceCode.DeviceCode)
		switch {
		case err == nil:
			return token, nil
		case errors.Is(err, ErrAuthorizationPending):
			continue
		case errors.Is(err, errSlowDown):
			interval += 5 * time.Second
			continue
		case errors.Is(err, ErrAuthTransient):
			// Endpoint hiccup proves nothing; keep polling within the deadline.
			continue
		default:
			return nil, err
		}
	}

	return nil, ErrExpiredCode
}

// maxOAuthBodyBytes bounds how much of an OAuth endpoint response is ever
// read, so a hostile/broken response cannot balloon memory. OAuth payloads are
// tiny; 64 KiB is far above any legitimate size.
const maxOAuthBodyBytes = 64 * 1024

// oauthErrorBody is the documented Twitch OAuth error shape
// ({"status": 400, "message": "authorization_pending"}). Only the message is
// used, as a classification discriminator — it is never logged and never
// included in returned errors.
type oauthErrorBody struct {
	Status  int    `json:"status"`
	Message string `json:"message"`
}

// decodeOAuthError best-effort parses an OAuth error body. A missing or
// undecodable body yields an empty message, which classifies as an
// unrecognized rejection.
func decodeOAuthError(body io.Reader) oauthErrorBody {
	var parsed oauthErrorBody
	_ = json.NewDecoder(io.LimitReader(body, maxOAuthBodyBytes)).Decode(&parsed)
	return parsed
}

// classifyOAuthFailure maps a non-200 OAuth token/device endpoint response to
// a stable sentinel error. Per the official contract, grant rejections arrive
// as HTTP 400 with a discriminating message ("authorization_pending",
// "invalid device code", ...); 429/5xx are transient; anything else is a
// fail-closed protocol error. The raw body/message never leaves this function.
func classifyOAuthFailure(resp *http.Response) error {
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError {
		return fmt.Errorf("%w: token endpoint status class %dxx", ErrAuthTransient, resp.StatusCode/100)
	}

	if resp.StatusCode == http.StatusBadRequest {
		msg := strings.ToLower(decodeOAuthError(resp.Body).Message)
		switch {
		case strings.Contains(msg, "authorization_pending"):
			return ErrAuthorizationPending
		case strings.Contains(msg, "slow_down"):
			return errSlowDown
		case strings.Contains(msg, "expired"):
			return ErrExpiredCode
		case strings.Contains(msg, "denied"):
			return ErrAccessDenied
		case strings.Contains(msg, "invalid device code"):
			return ErrInvalidDeviceCode
		default:
			return fmt.Errorf("%w: unrecognized device grant rejection", ErrAuthProtocol)
		}
	}

	return fmt.Errorf("%w: unexpected token endpoint status %d", ErrAuthProtocol, resp.StatusCode)
}

func (a *TwitchAuth) requestToken(ctx context.Context, deviceCode string) (*TokenResponse, error) {
	data := url.Values{
		"client_id":   {a.clientID},
		"device_code": {deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", a.tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	a.setOAuthHeaders(req)

	resp, err := a.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: token request failed", ErrAuthTransient)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, classifyOAuthFailure(resp)
	}

	var token TokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxOAuthBodyBytes)).Decode(&token); err != nil {
		return nil, fmt.Errorf("%w: undecodable token response", ErrAuthProtocol)
	}
	// A device grant must return the COMPLETE pair — publishing a partial
	// credential set (no refresh token) would silently disable future
	// recovery, so a structurally incomplete success is fail-closed.
	if token.AccessToken == "" || token.RefreshToken == "" {
		return nil, fmt.Errorf("%w: incomplete token response", ErrAuthProtocol)
	}

	return &token, nil
}

func (a *TwitchAuth) setOAuthHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Client-Id", a.clientID)
	req.Header.Set("X-Device-Id", a.deviceID)
	req.Header.Set("User-Agent", constants.TVUserAgent)
}
