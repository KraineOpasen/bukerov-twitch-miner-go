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
	"strings"
)

// ErrRecoveryFailed wraps a recovery attempt that completed without publishing
// a new credential generation. Whether it is worth retrying is carried by the
// wrapped cause (ErrAuthTransient => retryable).
var ErrRecoveryFailed = errors.New("twitch auth recovery failed")

// ErrRecoveryInconclusive marks a recovery outcome that neither proved nor
// disproved the credentials (an undocumented refresh 400): fail-closed, no
// device flow, nothing destroyed — and, unlike a definitive failure, it must
// not escalate the operator reauth path. Deliberately distinct from
// ErrAuthProtocol so a MALFORMED refresh success (which consumes the refresh
// token and therefore definitively needs the operator soon) still escalates.
var ErrRecoveryInconclusive = errors.New("inconclusive twitch auth recovery outcome")

// refreshClass classifies one refresh-grant round trip.
type refreshClass int

const (
	// refreshOK: a structurally complete rotated pair was granted.
	refreshOK refreshClass = iota
	// refreshTransient: transport / 429 / 5xx / undocumented status — proves
	// nothing about the refresh token; retry later, never device flow.
	refreshTransient
	// refreshInvalid: the documented authoritative rejections (HTTP 400 whose
	// parsed message is exactly the documented "Invalid refresh token"
	// discriminator, and HTTP 401) — the refresh token is dead and device
	// flow is the only recovery.
	refreshInvalid
	// refreshMalformed: HTTP 200 whose payload is structurally incomplete.
	// Twitch may have consumed the one-time refresh token, so it must not be
	// reused — but no partial credentials are published either.
	refreshMalformed
	// refreshInconclusive: an HTTP 400 WITHOUT the documented discriminator
	// (unrelated message, malformed or empty body). It proves nothing about
	// the refresh token, so it must not trigger a device flow and must not
	// destroy state — but unlike a plain transient it is a fail-closed
	// protocol outcome (ErrAuthProtocol). A later independent authoritative
	// rejection may retry recovery.
	refreshInconclusive
)

// invalidRefreshTokenMessage is the documented discriminator of the
// authoritative refresh rejection ({"status":400,"message":"Invalid refresh
// token"}). Matched by normalized EXACT equality — never by substring — so an
// unrelated 400 can never be misread as a dead refresh token.
const invalidRefreshTokenMessage = "invalid refresh token"

// Recover is the single entry point for reacting to an authoritative
// credential rejection (GQL 401, PubSub ERR_BADAUTH, validate 401).
// rejectedGeneration must be the Snapshot.Generation the rejected request was
// signed with.
//
// Guarantees:
//
//   - If the current generation is already newer than rejectedGeneration,
//     another caller has recovered: the current snapshot is returned with no
//     network I/O (a stale rejection never triggers a second refresh).
//   - If a recovery for this generation is already in flight, the caller
//     waits for that owner's result (or its own ctx cancellation, which
//     releases only the waiter — the owner keeps going).
//   - Otherwise the caller becomes the single owner: refresh first when a
//     refresh token is available; device flow only on the documented
//     authoritative refresh rejections (never on transient failures).
//   - The owner publishes at most one new generation and wakes all waiters
//     with one result. After a failed recovery the flight is cleared, so a
//     later independent rejection may start a fresh owner.
func (a *TwitchAuth) Recover(ctx context.Context, rejectedGeneration uint64) (Snapshot, error) {
	a.mu.Lock()
	if a.generation > rejectedGeneration {
		snap := a.snapshotLocked()
		a.mu.Unlock()
		return snap, nil
	}

	var done chan struct{}
	if a.recovering {
		done = a.recoverDone
		a.mu.Unlock()
	} else {
		// Become the owner: start exactly one flight. The refresh token is
		// captured under the lock so it is used by exactly one flight. The
		// flight runs on the LIFECYCLE context, detached from every caller's
		// context: a caller with a bounded wait (GQL replay) or a cancelled
		// waiter must not abort the shared flight mid-refresh or
		// mid-device-flow — otherwise every rejected request would spawn (and
		// kill) its own device code. Only process shutdown cancels it.
		a.recovering = true
		a.recoverDone = make(chan struct{})
		done = a.recoverDone
		refreshToken := a.refreshToken
		a.mu.Unlock()

		go func() {
			err := a.runRecovery(a.recoveryContext(), refreshToken)
			a.mu.Lock()
			a.recovering = false
			a.recoverErr = err
			close(done)
			a.mu.Unlock()
		}()
	}

	select {
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	case <-done:
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.generation > rejectedGeneration {
		return a.snapshotLocked(), nil
	}
	if err := a.recoverErr; err != nil {
		return Snapshot{}, err
	}
	return Snapshot{}, ErrRecoveryFailed
}

// runRecovery executes one owned recovery flight: refresh when possible,
// device flow only on an authoritative refresh rejection (or when no refresh
// token exists). On success the new pair is published (one generation bump)
// and persisted via the pending-persistence policy. All I/O runs outside the
// state lock.
func (a *TwitchAuth) runRecovery(ctx context.Context, refreshToken string) error {
	if refreshToken != "" {
		token, class, err := a.requestRefresh(ctx, refreshToken)
		switch class {
		case refreshOK:
			slog.Info("Recovered Twitch session via refresh token")
			a.adoptTokenPair(token)
			return nil
		case refreshTransient, refreshInconclusive:
			// Nothing proved; the stored pair (and the refresh token) stays
			// authoritative and a later rejection or validation retries.
			// Never device flow here.
			return fmt.Errorf("%w: %w", ErrRecoveryFailed, err)
		case refreshMalformed:
			// Twitch may have accepted (and thus consumed) the one-time
			// refresh token even though the response was unusable. Reusing it
			// could double-spend a rotated grant, so it is dropped from the
			// runtime state AND the drop is persisted (best-effort, retried at
			// checkpoints) — otherwise a restart would reload the consumed
			// token from disk and present it again. The next recovery goes
			// straight to device flow.
			a.mu.Lock()
			if a.refreshToken == refreshToken {
				a.refreshToken = ""
				a.persistDirty = true
			}
			a.mu.Unlock()
			if serr := a.SaveAuth(); serr != nil {
				slog.Warn("Failed to persist the dropped (possibly consumed) refresh token; will retry at the next checkpoint", "error", serr)
			}
			return fmt.Errorf("%w: %w", ErrRecoveryFailed, err)
		case refreshInvalid:
			slog.Warn("Twitch refresh token authoritatively rejected; falling back to device login")
			// Fall through to device flow below.
		}
	}

	token, err := a.deviceFlowAuthenticate(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRecoveryFailed, err)
	}
	a.adoptTokenPair(token)
	a.emitEvent(AuthEvent{Type: AuthEventCompleted})
	return nil
}

// requestRefresh performs one refresh grant (public client — no client
// secret) and classifies the outcome. The refresh token is form-encoded per
// the documented contract; neither it nor any response body ever appears in
// logs or returned errors.
func (a *TwitchAuth) requestRefresh(ctx context.Context, refreshToken string) (*TokenResponse, refreshClass, error) {
	data := url.Values{
		"client_id":     {a.clientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", a.tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, refreshTransient, err
	}
	a.setOAuthHeaders(req)

	resp, err := a.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, refreshTransient, ctx.Err()
		}
		return nil, refreshTransient, fmt.Errorf("%w: token refresh request failed", ErrAuthTransient)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusOK:
		var token TokenResponse
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxOAuthBodyBytes)).Decode(&token); err != nil {
			return nil, refreshMalformed, fmt.Errorf("%w: undecodable refresh response", ErrAuthProtocol)
		}
		// A successful refresh must rotate BOTH halves: publishing an empty
		// access token is useless, and continuing without a replacement
		// refresh token would silently disable all future refreshes while the
		// old one-time token is already spent.
		if token.AccessToken == "" || token.RefreshToken == "" {
			return nil, refreshMalformed, fmt.Errorf("%w: incomplete refresh response", ErrAuthProtocol)
		}
		return &token, refreshOK, nil
	case resp.StatusCode == http.StatusUnauthorized:
		// Documented authoritative rejection: re-consent required.
		return nil, refreshInvalid, fmt.Errorf("refresh token authoritatively rejected (status %d)", resp.StatusCode)
	case resp.StatusCode == http.StatusBadRequest:
		// A 400 is authoritative ONLY with the documented discriminator
		// message; the parsed message is used purely as an in-classifier
		// discriminator (bounded read, normalized exact match) and never
		// propagated. Any other/empty/malformed 400 body proves nothing about
		// the refresh token: fail-closed inconclusive, no device flow, no
		// state destruction.
		msg := strings.ToLower(strings.TrimSpace(decodeOAuthError(resp.Body).Message))
		if msg == invalidRefreshTokenMessage {
			return nil, refreshInvalid, fmt.Errorf("refresh token authoritatively rejected (status %d)", resp.StatusCode)
		}
		return nil, refreshInconclusive, fmt.Errorf("%w: %w: unrecognized refresh rejection (status 400)", ErrRecoveryInconclusive, ErrAuthProtocol)
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError:
		return nil, refreshTransient, fmt.Errorf("%w: refresh endpoint status class %dxx", ErrAuthTransient, resp.StatusCode/100)
	default:
		// Undocumented status: fail-closed as transient — not proof the
		// refresh token is dead, so no device-flow storm.
		return nil, refreshTransient, fmt.Errorf("%w: unexpected refresh endpoint status %d", ErrAuthTransient, resp.StatusCode)
	}
}
