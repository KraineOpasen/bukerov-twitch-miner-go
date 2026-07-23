package auth

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
)

// This file owns the candidate-token lifecycle. The core invariant of the
// credential state machine:
//
//	NO token pair becomes active credentials merely because the token
//	endpoint returned HTTP 200.
//
// Every granted pair — fresh device flow, device-flow fallback, refresh
// grant — is a PRIVATE CANDIDATE first. It is unavailable to GQL signing,
// PubSub LISTEN frames, IRC PASS, Snapshot(), GetAuthToken(), the rotation
// callback, and the persisted record until the authoritative /oauth2/validate
// confirms, for exactly that access token: structural validity, the expected
// client ID, the configured profile's identity, and the required scopes.
// Promotion is a single compare-and-swap on the credential generation, so a
// stale flight can never publish over a newer set.

// tokenCandidate is one granted-but-unpublished pair on its way through
// validation. It lives only in memory (never serialized) and never appears in
// logs, events, or errors.
type tokenCandidate struct {
	pair *TokenResponse
	// forGeneration is the credential generation this candidate is meant to
	// replace. Promotion happens only while the live generation still equals
	// it; any other publication in between makes the candidate stale.
	forGeneration uint64
	// source is diagnostics-only: "device" or "refresh".
	source string
	// consumedRefreshToken is the one-time refresh token that was spent to
	// obtain this candidate (empty for device grants). If the candidate is
	// definitively rejected, that stored token is dropped so it can never be
	// presented again.
	consumedRefreshToken string
}

// candidateOutcome classifies one candidate validation round trip.
type candidateOutcome int

const (
	// candidatePromote: structurally valid 200 with the expected client ID,
	// the configured profile's identity, and all required scopes.
	candidatePromote candidateOutcome = iota
	// candidateRejected: authoritative 401 for the candidate token.
	candidateRejected
	// candidateForeign: the candidate belongs to a different Twitch account
	// than this profile.
	candidateForeign
	// candidateAnomalous: valid 200 but issued for an unexpected client ID or
	// missing required scopes — deterministic properties a re-validation
	// cannot change, so the candidate is unusable.
	candidateAnomalous
	// candidateUnverified: the validation itself failed (transport / 429 /
	// 5xx / malformed response). Proves nothing about the candidate: it is
	// staged privately and re-validated by the next recovery attempt.
	candidateUnverified
)

// validateCandidate runs one /oauth2/validate round trip for the CANDIDATE's
// access token and classifies the result against this profile's expectations.
// It never touches published credential state.
func (a *TwitchAuth) validateCandidate(ctx context.Context, cand *tokenCandidate) (validateResponse, candidateOutcome, error) {
	res, status, err := a.validateOnce(ctx, cand.pair.AccessToken)
	switch status {
	case ValidateStatusValid:
	case ValidateStatusUnauthorized:
		return res, candidateRejected, nil
	case ValidateStatusTransient:
		return res, candidateUnverified, fmt.Errorf("%w: candidate validation unavailable", ErrAuthTransient)
	default:
		// Malformed 200 / undocumented status: the response is not trusted,
		// but it is also not proof the candidate is bad — fail closed,
		// re-validate later. Distinctly inconclusive so consumers never
		// escalate the operator path over it.
		return res, candidateUnverified, fmt.Errorf("%w: %w", ErrRecoveryInconclusive, err)
	}

	a.mu.Lock()
	username := a.username
	userID := a.userID
	confirmed := a.userIDAuthoritative
	a.mu.Unlock()

	// Identity first (a foreign candidate must never hide behind a lesser
	// anomaly), mirroring applyValidation's security ordering: a
	// runtime-confirmed user ID anchors the account (login difference = rename
	// observation); otherwise the configured profile name is the anchor.
	foreign := false
	switch {
	case confirmed && userID != "":
		foreign = res.UserID != userID
	default:
		foreign = username != "" && !strings.EqualFold(username, res.Login)
	}
	if foreign {
		return res, candidateForeign, ErrIdentityMismatch
	}

	if res.ClientID != a.clientID {
		return res, candidateAnomalous, fmt.Errorf("%w: candidate token issued for an unexpected client ID", ErrAuthProtocol)
	}
	if !hasAllScopes(res.Scopes, requiredScopes()) {
		return res, candidateAnomalous, fmt.Errorf("%w: candidate token is missing required scopes", ErrAuthProtocol)
	}
	return res, candidatePromote, nil
}

// resolveCandidate validates a candidate and routes the outcome:
//
//   - promote: the pair is published (compare-and-promote), persisted, and
//     rotation consumers are notified — the single success path. Returns
//     (true, nil); a candidate superseded by a concurrent publication returns
//     (false, nil) — valid credentials exist, they are just not this flight's.
//   - rejected/foreign/anomalous: the candidate is discarded; a consumed
//     one-time refresh token is durably dropped so it is never re-presented.
//     Nothing is published and the previous record is never deleted.
//   - unverified: the candidate is STAGED privately (the grant may be
//     unrepeatable) and the error is retryable; the next recovery for the
//     same generation re-validates the staged pair instead of re-contacting
//     the token endpoint.
func (a *TwitchAuth) resolveCandidate(ctx context.Context, cand *tokenCandidate) (bool, error) {
	backoff := candidateValidateBackoffBase
	for {
		// Re-check the generation before every validation round trip and before
		// any publication: an external replacement while we waited makes this
		// candidate stale — discard it and publish nothing.
		a.mu.Lock()
		stale := a.generation != cand.forGeneration
		if stale && a.pendingCandidate == cand {
			a.pendingCandidate = nil
		}
		a.mu.Unlock()
		if stale {
			return false, nil
		}

		res, outcome, verr := a.validateCandidate(ctx, cand)
		switch outcome {
		case candidatePromote:
			return a.promoteCandidate(cand, res), nil
		case candidateForeign:
			a.discardCandidate(cand)
			slog.Warn("Freshly granted Twitch credentials belong to a different account than this profile; not adopted")
			return false, fmt.Errorf("%w: %w", ErrRecoveryFailed, ErrIdentityMismatch)
		case candidateRejected:
			a.discardCandidate(cand)
			return false, fmt.Errorf("%w: %w: freshly granted token failed authoritative validation", ErrRecoveryFailed, ErrAuthProtocol)
		case candidateAnomalous:
			a.discardCandidate(cand)
			return false, fmt.Errorf("%w: %w", ErrRecoveryFailed, verr)
		default: // candidateUnverified — transient/inconclusive, retried below
		}

		// Transient/inconclusive: keep the candidate privately staged (never
		// serialized, never visible to credential readers) and wait, paced,
		// before revalidating the SAME access token. The attempt count is
		// unbounded (only the delay is capped), so a long /oauth2/validate
		// outage never abandons a freshly granted pair or aborts startup — only
		// a stale generation or ctx cancellation ends the loop.
		a.stageCandidate(cand)
		select {
		case <-ctx.Done():
			// Shutdown / owner abort: publish nothing; the candidate stays
			// staged in memory only.
			return false, ctx.Err()
		case <-a.timerAfter(backoff):
		}
		if backoff < candidateValidateBackoffCap {
			backoff *= 2
			if backoff > candidateValidateBackoffCap {
				backoff = candidateValidateBackoffCap
			}
		}
	}
}

// promoteCandidate is the single publication point for validated candidates:
// one compare-and-promote under the state lock (stale flights publish
// nothing), then persistence, then — only then — the rotation notification.
// Persisting before notifying keeps the crash window for a freshly rotated
// one-time refresh token as narrow as the disk write itself. A persistence
// failure never discards the pair: it stays authoritative in memory, marked
// persist-pending, and is retried at the next safe checkpoint. The rotation
// callback runs on its own goroutine so a slow consumer sweep cannot block
// the recovery flight's completion.
func (a *TwitchAuth) promoteCandidate(cand *tokenCandidate, res validateResponse) bool {
	a.mu.Lock()
	if a.generation != cand.forGeneration {
		// A concurrent publication superseded this flight: its result must
		// not be published, persisted, or announced.
		if a.pendingCandidate == cand {
			a.pendingCandidate = nil
		}
		a.mu.Unlock()
		return false
	}
	a.token = cand.pair.AccessToken
	a.refreshToken = cand.pair.RefreshToken
	a.tokenType = cand.pair.TokenType
	// The validate response is the authoritative scope/identity/expiry
	// snapshot for this token — adopted at publication, so the active set is
	// runtime-confirmed from its first instant.
	a.scopes = slices.Clone(res.Scopes)
	a.userID = res.UserID
	a.userIDAuthoritative = true
	a.expiresAt = a.now().Add(time.Duration(res.ExpiresIn) * time.Second)
	a.generation++
	gen := a.generation
	a.validationState = "valid"
	a.validatedAt = a.now()
	a.persistDirty = true
	a.pendingCandidate = nil
	a.clearRecoveryGateLocked()
	cb := a.rotationCallback
	a.mu.Unlock()

	slog.Info("Published rotated Twitch credentials", "generation", gen)

	if err := a.SaveAuth(); err != nil {
		slog.Warn("Failed to persist rotated Twitch credentials; keeping the new pair authoritative in memory and retrying at the next checkpoint",
			"generation", gen, "error", err)
	}
	if cb != nil {
		go cb(gen)
	}
	return true
}

// stageCandidate parks an unverified candidate in the private pending slot —
// but only while the generation it targets is still current (otherwise it is
// already stale and simply dropped).
func (a *TwitchAuth) stageCandidate(cand *tokenCandidate) {
	a.mu.Lock()
	if a.generation == cand.forGeneration {
		a.pendingCandidate = cand
	}
	a.mu.Unlock()
}

// discardCandidate drops a definitively unusable candidate. If a one-time
// refresh token was consumed to obtain it and is still the stored one, that
// token is dropped and the drop persisted (best-effort, retried at
// checkpoints) — a restart must not reload and re-present a consumed token.
func (a *TwitchAuth) discardCandidate(cand *tokenCandidate) {
	a.mu.Lock()
	if a.pendingCandidate == cand {
		a.pendingCandidate = nil
	}
	dropRefresh := cand.consumedRefreshToken != "" && a.refreshToken == cand.consumedRefreshToken
	if dropRefresh {
		a.refreshToken = ""
		a.persistDirty = true
	}
	a.mu.Unlock()
	if dropRefresh {
		if err := a.SaveAuth(); err != nil {
			slog.Warn("Failed to persist the dropped (consumed) refresh token; will retry at the next checkpoint", "error", err)
		}
	}
}

// Candidate-validation pacing. A candidate's /oauth2/validate spends no grant
// and is idempotent, so a transient/inconclusive outcome is retried against the
// SAME candidate with capped exponential backoff — the DELAY is bounded, the
// NUMBER of attempts is not — until an authoritative outcome, a stale
// generation, or context cancellation. A long validate outage therefore paces
// network traffic without ever abandoning a freshly granted (possibly already
// spent) pair or aborting startup.
const (
	candidateValidateBackoffBase = 2 * time.Second
	candidateValidateBackoffCap  = 60 * time.Second
)

// --- Sequential-recovery backoff gate ---

// Deterministic capped exponential backoff between failed recovery OWNER
// flights for the same credential generation. Deliberately not configurable.
const (
	recoveryBackoffBase = 30 * time.Second
	recoveryBackoffCap  = 10 * time.Minute
)

// recoveryBackoff returns the wait imposed after the n-th consecutive failed
// flight (n >= 1): base * 2^(n-1), capped.
func recoveryBackoff(failures int) time.Duration {
	d := recoveryBackoffBase
	for i := 1; i < failures; i++ {
		d *= 2
		if d >= recoveryBackoffCap {
			return recoveryBackoffCap
		}
	}
	return d
}

// clearRecoveryGateLocked resets the backoff gate. Callers hold mu.
func (a *TwitchAuth) clearRecoveryGateLocked() {
	a.gateGen = 0
	a.gateFailures = 0
	a.gateClass = ""
	a.gateNextAllowed = time.Time{}
}

// noteRecoveryOutcomeLocked updates the backoff gate after an owner flight
// for ownerGen completes. Callers hold mu. A successful flight (or one whose
// generation already advanced) clears the gate; a shutdown-cancelled flight
// arms nothing (the process is exiting — the gate exists to stop endpoint
// storms, not shutdowns); any other failure arms/extends the gate for exactly
// ownerGen with the deterministic capped exponential backoff.
func (a *TwitchAuth) noteRecoveryOutcomeLocked(ownerGen uint64, err error, classify func(error) string) {
	if err == nil || a.generation > ownerGen {
		a.clearRecoveryGateLocked()
		return
	}
	if ctxErr := a.lifecycleContextErrLocked(); ctxErr != nil {
		return
	}
	if a.gateGen == ownerGen {
		a.gateFailures++
	} else {
		a.gateGen = ownerGen
		a.gateFailures = 1
	}
	a.gateClass = classify(err)
	a.gateNextAllowed = a.now().Add(recoveryBackoff(a.gateFailures))
}

// lifecycleContextErrLocked reports whether the lifecycle context is already
// cancelled (process shutdown). Callers hold mu.
func (a *TwitchAuth) lifecycleContextErrLocked() error {
	if a.lifecycleCtx == nil {
		return nil
	}
	return a.lifecycleCtx.Err()
}
