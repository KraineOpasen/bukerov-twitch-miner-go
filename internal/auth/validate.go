package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
)

// ValidateStatus classifies one /oauth2/validate outcome. Only
// ValidateStatusUnauthorized is authoritative proof the token is invalid;
// every other non-valid status keeps the current credentials.
type ValidateStatus string

const (
	ValidateStatusValid            ValidateStatus = "valid"
	ValidateStatusUnauthorized     ValidateStatus = "unauthorized"
	ValidateStatusTransient        ValidateStatus = "transient"
	ValidateStatusProtocolError    ValidateStatus = "protocol_error"
	ValidateStatusClientIDMismatch ValidateStatus = "client_id_mismatch"
	ValidateStatusMissingScopes    ValidateStatus = "missing_scopes"
	ValidateStatusIdentityMismatch ValidateStatus = "identity_mismatch"
	ValidateStatusNoToken          ValidateStatus = "no_token"
)

// hourlyValidationInterval is Twitch's fixed contract: apps must validate
// their token on startup and hourly thereafter. Deliberately not configurable.
const hourlyValidationInterval = time.Hour

// validateResponse is the documented 200 shape of /oauth2/validate.
type validateResponse struct {
	ClientID  string   `json:"client_id"`
	Login     string   `json:"login"`
	Scopes    []string `json:"scopes"`
	UserID    string   `json:"user_id"`
	ExpiresIn int64    `json:"expires_in"`
}

// requiredScopes returns the scope set this miner was granted at login. It is
// defined solely by the existing constants.OAuthScopes value — this PR neither
// changes nor extends the scopes.
func requiredScopes() []string {
	return strings.Fields(constants.OAuthScopes)
}

// hasAllScopes reports whether granted contains every required scope.
func hasAllScopes(granted, required []string) bool {
	set := make(map[string]struct{}, len(granted))
	for _, s := range granted {
		set[strings.ToLower(strings.TrimSpace(s))] = struct{}{}
	}
	for _, r := range required {
		if _, ok := set[strings.ToLower(r)]; !ok {
			return false
		}
	}
	return true
}

// ValidateAndApply performs one /oauth2/validate round trip for the current
// access token and applies the outcome:
//
//   - 200 (structurally valid, expected client ID, required scopes, matching
//     identity): metadata (authoritative user_id, expiry, validation state) is
//     updated atomically; credentials are untouched (no generation bump) and
//     the auth file is rewritten only when a pending persistence needs
//     retrying.
//   - 401: the token is authoritatively invalid — reported to the caller,
//     which owns triggering the shared recovery. Nothing is destroyed here.
//   - transport error / 429 / 5xx: transient — validation state degrades but
//     credentials and the auth file are untouched.
//   - malformed 200 or an undocumented status: fail-closed protocol error —
//     the response is not trusted, and it is also NOT treated as proof of
//     invalidity, so no credentials are destroyed and no device flow starts.
//
// The HTTP round trip runs outside every lock.
func (a *TwitchAuth) ValidateAndApply(ctx context.Context) (ValidateStatus, error) {
	status, _, err := a.validateAndApplyCurrent(ctx)
	return status, err
}

// validateAndApplyCurrent additionally reports the credential generation the
// validated token belonged to, so a 401 outcome can be funneled into Recover
// keyed on exactly that generation (a rotation racing the validation must not
// key recovery on the newer set). Token and generation are captured in ONE
// lock acquisition.
func (a *TwitchAuth) validateAndApplyCurrent(ctx context.Context) (ValidateStatus, uint64, error) {
	a.mu.Lock()
	token := a.token
	gen := a.generation
	a.mu.Unlock()
	if token == "" {
		return ValidateStatusNoToken, gen, nil
	}

	res, status, err := a.validateOnce(ctx, token)
	switch status {
	case ValidateStatusValid:
		st, aerr := a.applyValidation(res, gen)
		return st, gen, aerr
	case ValidateStatusUnauthorized:
		a.setValidationState("degraded:unauthorized")
		return status, gen, nil
	default:
		a.setValidationState("degraded:" + string(status))
		return status, gen, err
	}
}

// validateOnce is the raw round trip + structural classification. It never
// touches auth state.
func (a *TwitchAuth) validateOnce(ctx context.Context, token string) (validateResponse, ValidateStatus, error) {
	var parsed validateResponse

	req, err := http.NewRequestWithContext(ctx, "GET", a.validateURL, nil)
	if err != nil {
		return parsed, ValidateStatusTransient, err
	}
	// The documented header form; "Bearer" is accepted interchangeably but
	// "OAuth" matches every other request this client signs.
	req.Header.Set("Authorization", "OAuth "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", constants.TVUserAgent)

	resp, err := a.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return parsed, ValidateStatusTransient, ctx.Err()
		}
		return parsed, ValidateStatusTransient, fmt.Errorf("%w: token validation transport failure", ErrAuthTransient)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusOK:
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxOAuthBodyBytes)).Decode(&parsed); err != nil {
			return parsed, ValidateStatusProtocolError, fmt.Errorf("%w: undecodable validate response", ErrAuthProtocol)
		}
		if parsed.ClientID == "" || parsed.Login == "" || parsed.UserID == "" || parsed.ExpiresIn < 0 {
			return parsed, ValidateStatusProtocolError, fmt.Errorf("%w: incomplete validate response", ErrAuthProtocol)
		}
		return parsed, ValidateStatusValid, nil
	case resp.StatusCode == http.StatusUnauthorized:
		return parsed, ValidateStatusUnauthorized, nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError:
		return parsed, ValidateStatusTransient, fmt.Errorf("%w: validate endpoint status class %dxx", ErrAuthTransient, resp.StatusCode/100)
	default:
		return parsed, ValidateStatusProtocolError, fmt.Errorf("%w: unexpected validate endpoint status %d", ErrAuthProtocol, resp.StatusCode)
	}
}

// applyValidation applies a structurally valid 200 to the auth state: client
// ID, scope, and identity checks first (each classified without destroying
// anything), then an atomic metadata update — guarded by validatedGen so a
// validation that raced a rotation can never stamp the OLD token's metadata
// over the newer credential set. A pending persistence is retried afterwards
// as a safe checkpoint.
func (a *TwitchAuth) applyValidation(res validateResponse, validatedGen uint64) (ValidateStatus, error) {
	if res.ClientID != a.clientID {
		// The token was issued to a different client ID than this miner uses.
		// Classified, degraded, nothing destroyed: GQL may still work, and an
		// authoritative 401 (not this anomaly) is what triggers recovery.
		a.setValidationState("degraded:" + string(ValidateStatusClientIDMismatch))
		slog.Warn("Stored Twitch token was issued for an unexpected client ID; keeping it but flagging validation as degraded")
		return ValidateStatusClientIDMismatch, nil
	}
	if !hasAllScopes(res.Scopes, requiredScopes()) {
		a.setValidationState("degraded:" + string(ValidateStatusMissingScopes))
		slog.Warn("Stored Twitch token is missing required scopes; keeping it but flagging validation as degraded")
		return ValidateStatusMissingScopes, nil
	}

	a.mu.Lock()
	if a.generation != validatedGen {
		// A rotation superseded this validation mid-flight; its metadata
		// belongs to the old token and must not be stamped over the new set.
		// The new set was validated fresh by its own publication.
		a.mu.Unlock()
		return ValidateStatusValid, nil
	}
	switch {
	case a.userID != "" && res.UserID != a.userID:
		// The validated token belongs to a different account than the one this
		// profile is bound to. Never adopt foreign credentials.
		a.mu.Unlock()
		a.setValidationState("degraded:" + string(ValidateStatusIdentityMismatch))
		slog.Warn("Validated Twitch token belongs to a different account than this profile", "profile", a.GetUsername())
		return ValidateStatusIdentityMismatch, ErrIdentityMismatch
	case a.userID == "" && a.username != "" && !strings.EqualFold(a.username, res.Login):
		// No user ID on record to compare, and the token's login is not the
		// configured profile: same foreign-credentials protection.
		a.mu.Unlock()
		a.setValidationState("degraded:" + string(ValidateStatusIdentityMismatch))
		slog.Warn("Validated Twitch token login does not match this profile", "profile", a.GetUsername())
		return ValidateStatusIdentityMismatch, ErrIdentityMismatch
	}
	if a.username != "" && !strings.EqualFold(a.username, res.Login) {
		// Same user ID, different login: a Twitch rename. Full reconciliation
		// is BKM-006; here it is only surfaced, never treated as a mismatch.
		slog.Warn("Twitch login differs from the configured profile name (account rename?); continuing with the confirmed user ID",
			"profile", a.username)
	}
	a.userID = res.UserID
	a.expiresAt = a.now().Add(time.Duration(res.ExpiresIn) * time.Second)
	a.validatedAt = a.now()
	a.validationState = "valid"
	dirty := a.persistDirty
	a.mu.Unlock()

	if dirty {
		// Safe checkpoint: retry persisting a rotation whose save failed.
		if err := a.SaveAuth(); err != nil {
			slog.Warn("Retried persisting rotated Twitch credentials at validation checkpoint; still failing", "error", err)
		}
	}
	return ValidateStatusValid, nil
}

func (a *TwitchAuth) setValidationState(state string) {
	a.mu.Lock()
	a.validationState = state
	a.validatedAt = a.now()
	a.mu.Unlock()
}

// AuthHealth is redacted validation/persistence metadata for diagnostics. It
// never carries token material.
type AuthHealth struct {
	Generation      uint64
	ValidationState string
	ValidatedAt     time.Time
	ExpiresAt       time.Time
	PersistPending  bool
	HasRefreshToken bool
}

// Health returns the current redacted auth diagnostics snapshot.
func (a *TwitchAuth) Health() AuthHealth {
	a.mu.Lock()
	defer a.mu.Unlock()
	return AuthHealth{
		Generation:      a.generation,
		ValidationState: a.validationState,
		ValidatedAt:     a.validatedAt,
		ExpiresAt:       a.expiresAt,
		PersistPending:  a.persistDirty,
		HasRefreshToken: a.refreshToken != "",
	}
}

// RunHourlyValidation is the lifecycle-managed hourly validator. Twitch
// requires validation at startup (done synchronously inside Login) and hourly
// thereafter; this loop owns the "hourly thereafter" half. It is guarded so a
// second concurrent call exits immediately (one validator per auth session),
// stops on ctx cancellation, and uses the timer seam so tests never wait a
// real hour.
//
// A healthy validation performs no credential churn: no generation bump, no
// rotation callback, no auth-file write (unless a pending persistence needs
// retrying). A 401 joins the shared single-flight recovery — never a second
// concurrent one. Transient failures keep the session untouched.
func (a *TwitchAuth) RunHourlyValidation(ctx context.Context) {
	a.mu.Lock()
	if a.validatorRunning {
		a.mu.Unlock()
		slog.Warn("Hourly Twitch token validator is already running; not starting a duplicate")
		return
	}
	a.validatorRunning = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.validatorRunning = false
		a.mu.Unlock()
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-a.timerAfter(hourlyValidationInterval):
		}
		a.hourlyTick(ctx)
	}
}

// hourlyTick runs one validation cycle and, on an authoritative 401, funnels
// into the shared single-flight recovery (joining one already in flight). The
// recovery is keyed on the generation the validated token belonged to —
// captured atomically with the token — so a rotation racing the validation is
// recognized as already-recovered instead of being rotated again.
func (a *TwitchAuth) hourlyTick(ctx context.Context) {
	status, rejectedGen, err := a.validateAndApplyCurrent(ctx)
	switch status {
	case ValidateStatusValid, ValidateStatusNoToken:
		return
	case ValidateStatusUnauthorized:
		if _, rerr := a.Recover(ctx, rejectedGen); rerr != nil {
			slog.Warn("Hourly token validation: recovery did not complete; will retry on the next validation or authoritative rejection",
				"error", rerr)
		}
	default:
		slog.Warn("Hourly token validation inconclusive; keeping the current session",
			"status", string(status), "error", err)
	}
}
