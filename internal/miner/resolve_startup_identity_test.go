package miner

import (
	"errors"
	"fmt"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/auth"
)

// TestResolveStartupIdentity_SuccessfulLookupDelegatesToVerify covers the
// nil-lookupErr branch: a normal successful GetChannelID resolution is
// cross-checked against the session identity exactly as before BKM-006.
func TestResolveStartupIdentity_SuccessfulLookupDelegatesToVerify(t *testing.T) {
	userID, stale, err := resolveStartupIdentity("uid-1", "uid-1", nil)
	if err != nil {
		t.Fatalf("matching IDs must not error: %v", err)
	}
	if stale {
		t.Error("a successful lookup must never be reported as a stale login")
	}
	if userID != "uid-1" {
		t.Errorf("userID = %q, want uid-1", userID)
	}
}

// TestResolveStartupIdentity_SuccessfulLookupMismatchFailsClosed: a
// successful lookup that resolves to a DIFFERENT account than the session
// still fails closed exactly as verifyIdentityBinding always has — BKM-006
// only adds a fallback for ErrStreamerDoesNotExist, nothing else.
func TestResolveStartupIdentity_SuccessfulLookupMismatchFailsClosed(t *testing.T) {
	_, stale, err := resolveStartupIdentity("uid-session", "uid-someone-else", nil)
	if !errors.Is(err, auth.ErrIdentityMismatch) {
		t.Fatalf("error = %v, want ErrIdentityMismatch", err)
	}
	if stale {
		t.Error("a genuine identity mismatch is not a 'stale login', it must not be reported as one")
	}
}

// TestResolveStartupIdentity_RenamedOwnerLoginProceedsWithSession is the core
// new behavior (BKM-006 P): the configured username no longer resolves
// (Twitch reports it as not-found — most likely because it was renamed), but
// the OAuth session already carries a validated identity. Startup must
// proceed with that session identity instead of aborting.
func TestResolveStartupIdentity_RenamedOwnerLoginProceedsWithSession(t *testing.T) {
	lookupErr := fmt.Errorf("wrapped: %w", api.ErrStreamerDoesNotExist)
	userID, stale, err := resolveStartupIdentity("uid-session", "", lookupErr)
	if err != nil {
		t.Fatalf("a renamed owner login with a valid session must not error: %v", err)
	}
	if !stale {
		t.Error("staleLogin must be true so the caller logs exactly one warning")
	}
	if userID != "uid-session" {
		t.Errorf("userID = %q, want the session identity uid-session", userID)
	}
}

// TestResolveStartupIdentity_EmptySessionFailsClosedRegardlessOfLookupError
// covers invariant P's fail-closed branch: an empty session user ID is an
// invariant violation on its own (a successful Login always leaves one) and
// must fail closed even when the lookup error is the "renamed" sentinel —
// there is no authoritative identity to fall back to.
func TestResolveStartupIdentity_EmptySessionFailsClosedRegardlessOfLookupError(t *testing.T) {
	cases := []error{
		fmt.Errorf("wrapped: %w", api.ErrStreamerDoesNotExist),
		api.ErrUnauthorized,
		errors.New("transport exhausted"),
	}
	for _, lookupErr := range cases {
		t.Run(lookupErr.Error(), func(t *testing.T) {
			_, stale, err := resolveStartupIdentity("", "", lookupErr)
			if !errors.Is(err, auth.ErrIdentityMismatch) {
				t.Fatalf("error = %v, want ErrIdentityMismatch", err)
			}
			if stale {
				t.Error("an empty session must never be reported as a mere stale-login proceed-anyway case")
			}
		})
	}
}

// TestResolveStartupIdentity_OtherErrorsPassThrough covers the "otherwise"
// branch: a fail-fast error that is NOT ErrStreamerDoesNotExist (token
// rejection, or an unknown error the retry loop gave up on after context
// cancellation) is returned verbatim — no owner-rename fallback applies.
func TestResolveStartupIdentity_OtherErrorsPassThrough(t *testing.T) {
	for _, lookupErr := range []error{api.ErrUnauthorized, errors.New("boom")} {
		t.Run(lookupErr.Error(), func(t *testing.T) {
			_, stale, err := resolveStartupIdentity("uid-session", "", lookupErr)
			if !errors.Is(err, lookupErr) {
				t.Fatalf("error = %v, want %v verbatim", err, lookupErr)
			}
			if stale {
				t.Error("a non-ErrStreamerDoesNotExist failure must not be reported as a stale login")
			}
		})
	}
}
