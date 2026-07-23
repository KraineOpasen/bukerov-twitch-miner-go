package miner

import "testing"

// P2 identity invariant: a successful Login must leave an authoritative
// session user ID — an EMPTY session ID at binding time is an invariant
// violation, not a pass. GetChannelID's resolution is only a cross-check; it
// must never substitute for the token session's own validated identity.
func TestVerifyIdentityBindingEmptySessionFailsClosed(t *testing.T) {
	if err := verifyIdentityBinding("", "123"); err == nil {
		t.Fatalf("empty session user ID passed the identity binding (fabricated authority)")
	}
	if err := verifyIdentityBinding("123", "123"); err != nil {
		t.Fatalf("matching IDs rejected: %v", err)
	}
	if err := verifyIdentityBinding("123", "456"); err == nil {
		t.Fatalf("mismatched IDs passed the identity binding")
	}
}
