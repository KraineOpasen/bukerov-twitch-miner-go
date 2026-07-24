package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
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
		// Generation-aware like every other outcome: a stale 401 for an
		// already-rotated token must not degrade the new generation's health
		// (the caller's Recover(gen) fast-paths on the same guard).
		a.setValidationStateForGeneration(gen, "degraded:unauthorized")
		return status, gen, nil
	default:
		a.setValidationStateForGeneration(gen, "degraded:"+string(status))
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

// applyValidation applies a structurally valid 200 to the auth state. The
// whole application runs in ONE mu section that first re-checks validatedGen —
// a validation that raced a rotation applies NOTHING (no metadata, no
// degradation) to the newer credential set. Check order within the current
// generation is security-ordered: authoritative IDENTITY first (a foreign
// token must never hide behind a lesser client-ID/scope anomaly), then client
// ID, then required scopes, then the metadata apply (authoritative user ID,
// expiry, and the authoritative scope snapshot — adopted only when the scope
// SET actually changed, so healthy hourly validations cause no churn). A
// pending persistence is retried afterwards as a safe checkpoint.
func (a *TwitchAuth) applyValidation(res validateResponse, validatedGen uint64) (ValidateStatus, error) {
	a.mu.Lock()
	if a.generation != validatedGen {
		// A rotation superseded this validation mid-flight; its outcome
		// belongs to the old token and must not touch the new set's state.
		a.mu.Unlock()
		return ValidateStatusValid, nil
	}

	// 1. Authoritative identity. A CONFIGURED pin (a.expectedUserID, BKM-006
	// Corrective Pass 1 C3 — trusted operator metadata from
	// config.Config.OwnerUserID, NEVER derived from the cookie/credential
	// file) is the STRONGEST anchor when set, and applies even on a session
	// whose disk-loaded userID has not yet been runtime-confirmed THIS
	// process: a differing user ID is foreign unconditionally; a matching
	// user ID with a different login is a TOLERATED rename. Without a pin,
	// the legacy anchor applies unchanged (BKM-005): a runtime-CONFIRMED
	// userID anchors the account (login change = rename observation); a
	// disk-loaded (unconfirmed) userID is NOT trusted to authorize a foreign
	// login — the configured profile name is the anchor then.
	foreign := false
	rename := false
	switch {
	case a.expectedUserID != "":
		if res.UserID != a.expectedUserID {
			foreign = true
		} else if a.username != "" && !strings.EqualFold(a.username, res.Login) {
			rename = true
		}
	case a.userIDAuthoritative && a.userID != "":
		if res.UserID != a.userID {
			foreign = true
		} else if a.username != "" && !strings.EqualFold(a.username, res.Login) {
			rename = true
		}
	default:
		if a.username != "" && !strings.EqualFold(a.username, res.Login) {
			foreign = true
		}
	}
	if foreign {
		a.validationState = "degraded:" + string(ValidateStatusIdentityMismatch)
		a.validatedAt = a.now()
		profile := a.username
		a.mu.Unlock()
		slog.Warn("Validated Twitch token belongs to a different account than this profile", "profile", profile)
		return ValidateStatusIdentityMismatch, ErrIdentityMismatch
	}

	// 2. Client ID: the token was issued to a different client ID than this
	// miner uses. Classified, degraded, nothing destroyed: GQL may still
	// work, and an authoritative 401 (not this anomaly) triggers recovery.
	if res.ClientID != a.clientID {
		a.validationState = "degraded:" + string(ValidateStatusClientIDMismatch)
		a.validatedAt = a.now()
		a.mu.Unlock()
		slog.Warn("Stored Twitch token was issued for an unexpected client ID; keeping it but flagging validation as degraded")
		return ValidateStatusClientIDMismatch, nil
	}

	// 3. Required scopes.
	if !hasAllScopes(res.Scopes, requiredScopes()) {
		a.validationState = "degraded:" + string(ValidateStatusMissingScopes)
		a.validatedAt = a.now()
		a.mu.Unlock()
		slog.Warn("Stored Twitch token is missing required scopes; keeping it but flagging validation as degraded")
		return ValidateStatusMissingScopes, nil
	}

	// 4. Metadata apply (current generation only, checked above).
	a.userID = res.UserID
	a.userIDAuthoritative = true
	// Record the account's current canonical login (COR-2). Reached only after
	// the identity checks above passed, so this is never a foreign login; the
	// miner adopts it for Twitch-/user-facing use while storage stays keyed by
	// a.username.
	a.canonicalLogin = res.Login
	a.expiresAt = a.now().Add(time.Duration(res.ExpiresIn) * time.Second)
	a.validatedAt = a.now()
	a.validationState = "valid"
	if !sameScopeSet(a.scopes, res.Scopes) {
		// The validate response is the authoritative scope snapshot. Adopted
		// (and marked for a one-time persist) only when the SET changed —
		// order-only differences must not churn memory or disk.
		a.scopes = slices.Clone(res.Scopes)
		a.persistDirty = true
	}
	dirty := a.persistDirty
	a.mu.Unlock()

	if rename {
		// Same confirmed user ID, different login: a Twitch rename. Full
		// reconciliation is BKM-006; here it is only surfaced.
		slog.Warn("Twitch login differs from the configured profile name (account rename?); continuing with the confirmed user ID",
			"profile", a.GetUsername())
	}

	if dirty {
		// Safe checkpoint: retry persisting a rotation whose save failed (or
		// persist a scope-metadata upgrade exactly once).
		if err := a.SaveAuth(); err != nil {
			slog.Warn("Retried persisting Twitch credentials at validation checkpoint; still failing", "error", err)
		}
	}
	return ValidateStatusValid, nil
}

// sameScopeSet compares two scope lists as SETS (order- and
// duplicate-insensitive), so authoritative order changes never register as a
// scope change.
func sameScopeSet(a, b []string) bool {
	return hasAllScopes(a, b) && hasAllScopes(b, a)
}

// setValidationStateForGeneration records a validation outcome's diagnostics
// ONLY if the credentials the outcome belongs to are still current. A stale
// outcome (its generation was rotated away mid-flight) applies nothing and
// returns false.
func (a *TwitchAuth) setValidationStateForGeneration(validatedGen uint64, state string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.generation != validatedGen {
		return false
	}
	a.validationState = state
	a.validatedAt = a.now()
	return true
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
	case ValidateStatusIdentityMismatch:
		// Authoritative foreign-identity detection at runtime: no automatic
		// interactive flow (that is a startup decision), but it must be
		// loudly surfaced, not filed under "inconclusive".
		slog.Error("Hourly token validation: credentials belong to a different Twitch account than this profile; operator action required")
		a.emitEvent(AuthEvent{Type: AuthEventError, Error: ErrIdentityMismatch})
	default:
		slog.Warn("Hourly token validation inconclusive; keeping the current session",
			"status", string(status), "error", err)
	}
}
