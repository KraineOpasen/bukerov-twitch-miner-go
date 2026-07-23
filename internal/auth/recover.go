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
	"time"
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

// ErrRecoveryBackoff marks a recovery attempt refused because a previous
// attempt for the SAME credential generation recently failed and the
// deterministic retry backoff has not yet elapsed. It is retryable and
// non-escalating (like a transient failure), carries no secret material, and
// causes zero network traffic.
var ErrRecoveryBackoff = errors.New("twitch auth recovery is backing off after a recent failure")

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
// credential rejection (GQL 401, PubSub ERR_BADAUTH, IRC login-failure
// NOTICE, validate 401). rejectedGeneration must be the Snapshot.Generation
// the rejected request was signed with. It is also the single retry
// authority: consumers never impose their own recovery pacing on top.
//
// Check order and guarantees:
//
//  1. Stale fast-path: if the current generation is already newer than
//     rejectedGeneration, another caller has recovered — the current
//     snapshot is returned with no network I/O, and the backoff gate is
//     untouched.
//  2. Join: if a recovery for this generation is already in flight, the
//     caller waits for that owner's result (or its own ctx cancellation,
//     which releases only the waiter — the owner keeps going). Joining is
//     never gated: it causes no new traffic.
//  3. Pending-candidate priority: a new owner whose generation has a
//     privately staged (granted-but-unverified) candidate re-VALIDATES that
//     candidate instead of contacting the token endpoint — a consumed
//     one-time refresh grant is never repeated.
//  4. Backoff gate: after a failed owner flight for this same generation, a
//     new owner is refused with ErrRecoveryBackoff (retryable,
//     non-escalating, zero network) until the deterministic capped
//     exponential backoff elapses. Success and new generations clear it.
//  5. Otherwise the caller becomes the single owner: refresh first when a
//     refresh token is available; device flow only on the documented
//     authoritative refresh rejections (never on transient failures). Every
//     granted pair is validated as a candidate before publication.
//
// The owner publishes at most one new generation and wakes all waiters with
// one result. After a failed recovery the flight is cleared, so a later
// independent rejection may start a fresh owner (subject to the gate).
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
		// Pending-candidate priority (see check order above): decide the
		// flight's work while still under the lock.
		pending := a.pendingCandidate
		if pending != nil && pending.forGeneration != a.generation {
			pending = nil
		}
		// Backoff gate for a fresh owner of a recently failed generation.
		if a.gateFailures > 0 && a.gateGen == a.generation {
			if wait := a.gateNextAllowed.Sub(a.now()); wait > 0 {
				a.mu.Unlock()
				return Snapshot{}, fmt.Errorf("%w: retry allowed in %s",
					ErrRecoveryBackoff, wait.Round(time.Second))
			}
		}
		// Become the owner: start exactly one flight. The refresh token (or
		// staged candidate) is captured under the lock so it is used by
		// exactly one flight. The flight runs on the LIFECYCLE context,
		// detached from every caller's context: a caller with a bounded wait
		// (GQL replay) or a cancelled waiter must not abort the shared flight
		// mid-refresh or mid-device-flow — otherwise every rejected request
		// would spawn (and kill) its own device code. Only process shutdown
		// cancels it.
		a.recovering = true
		a.recoverDone = make(chan struct{})
		done = a.recoverDone
		ownerGen := a.generation
		var refreshToken string
		if pending == nil {
			refreshToken = a.refreshToken
		}
		a.mu.Unlock()

		go func() {
			err := a.runRecovery(a.recoveryContext(), ownerGen, pending, refreshToken)
			a.mu.Lock()
			a.recovering = false
			a.recoverErr = err
			a.noteRecoveryOutcomeLocked(ownerGen, err, classifyGateFailure)
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

// classifyGateFailure maps a failed flight's error to a redacted diagnostics
// class for the backoff gate (never any secret or body material).
func classifyGateFailure(err error) string {
	switch {
	case errors.Is(err, ErrRecoveryBackoff):
		return "backoff"
	case errors.Is(err, ErrIdentityMismatch):
		return "identity_mismatch"
	case errors.Is(err, ErrRecoveryInconclusive):
		return "inconclusive"
	case errors.Is(err, ErrAuthTransient):
		return "transient"
	case errors.Is(err, ErrAuthProtocol):
		return "protocol"
	default:
		return "failed"
	}
}

// runRecovery executes one owned recovery flight. A staged pending candidate
// is re-validated FIRST (its grant is already spent and must not be
// repeated); otherwise refresh when possible, device flow only on an
// authoritative refresh rejection (or when no refresh token exists). EVERY
// granted pair is a private candidate: publication happens only inside
// resolveCandidate after the authoritative validation passes, via the single
// compare-and-promote point. All I/O runs outside the state lock.
func (a *TwitchAuth) runRecovery(ctx context.Context, ownerGen uint64, pending *tokenCandidate, refreshToken string) error {
	if pending != nil {
		promoted, err := a.resolveCandidate(ctx, pending)
		if err != nil {
			return err
		}
		if promoted {
			slog.Info("Recovered Twitch session by validating the staged candidate pair")
		}
		return nil
	}

	if refreshToken != "" {
		token, class, err := a.requestRefresh(ctx, refreshToken)
		switch class {
		case refreshOK:
			promoted, rerr := a.resolveCandidate(ctx, &tokenCandidate{
				pair: token, forGeneration: ownerGen,
				source: "refresh", consumedRefreshToken: refreshToken,
			})
			if rerr != nil {
				return rerr
			}
			if promoted {
				slog.Info("Recovered Twitch session via refresh token")
			}
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
	promoted, rerr := a.resolveCandidate(ctx, &tokenCandidate{
		pair: token, forGeneration: ownerGen, source: "device",
	})
	if rerr != nil {
		a.emitEvent(AuthEvent{Type: AuthEventError, Error: rerr})
		return rerr
	}
	if promoted {
		a.emitEvent(AuthEvent{Type: AuthEventCompleted})
	}
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
