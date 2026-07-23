package miner

import (
	"errors"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/auth"
)

// Regression: the reauth-required state is per-outage, not per-process — a
// successful rotation clears it, and a later separate outage notifies again.
func TestReauthRequiredClearsOnRotationAndCanRelatch(t *testing.T) {
	m := &Miner{}

	m.handleAuthError()
	m.handleAuthError() // dedupe within one outage
	m.mu.RLock()
	required, notified := m.reauthRequired, m.reauthNotified
	m.mu.RUnlock()
	if !required || !notified {
		t.Fatalf("first definitive failure did not latch the reauth state")
	}

	m.clearReauthRequired()
	m.mu.RLock()
	required, notified = m.reauthRequired, m.reauthNotified
	m.mu.RUnlock()
	if required || notified {
		t.Fatalf("successful rotation did not clear the reauth state")
	}

	// A later, separate outage must be able to notify again.
	m.handleAuthError()
	m.mu.RLock()
	required = m.reauthRequired
	m.mu.RUnlock()
	if !required {
		t.Fatalf("a later outage could not re-latch after a recovery")
	}
}

// Regression: credentials confirmed for a different account than the
// configured username resolves to are never silently bound to this profile.
func TestVerifyIdentityBinding(t *testing.T) {
	if err := verifyIdentityBinding("", "uid-1"); err != nil {
		t.Fatalf("fresh session (no bound ID) must pass: %v", err)
	}
	if err := verifyIdentityBinding("uid-1", "uid-1"); err != nil {
		t.Fatalf("matching identity must pass: %v", err)
	}
	if err := verifyIdentityBinding("uid-bob", "uid-alice"); !errors.Is(err, auth.ErrIdentityMismatch) {
		t.Fatalf("foreign identity must be classified as ErrIdentityMismatch, got %v", err)
	}
}
