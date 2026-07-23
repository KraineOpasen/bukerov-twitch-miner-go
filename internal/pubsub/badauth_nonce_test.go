package pubsub

import (
	"errors"
	"testing"
)

// Regression: a queued ERR_BADAUTH answering an OLD-generation LISTEN that is
// read AFTER the post-rotation sweep must be attributed to the old generation
// (by its frame nonce), not to whatever generation was written most recently —
// otherwise a stale rejection would trigger a spurious refresh of freshly
// rotated, valid credentials.
func TestStaleBadAuthAttributedByNonceNotLastWrite(t *testing.T) {
	provider := newRotatingAuth(AuthSnapshot{Token: "tok-1", Generation: 1})
	var got []*AuthError
	ws := NewWebSocketClient(0, provider.provider, 60, 60, nil, func(err error) {
		var ae *AuthError
		if errors.As(err, &ae) {
			got = append(got, ae)
		}
	})
	rec := &authRotRecorder{}
	ws.writeTopicFrameHook = rec.hook
	ws.mu.Lock()
	ws.isOpened = true
	ws.mu.Unlock()

	user := NewTopic(TopicCommunityPointsUser, "42")
	if err := ws.Listen(user); err != nil {
		t.Fatalf("listen: %v", err)
	}

	// Grab the gen-1 frame's nonce from the attribution ledger.
	ws.mu.RLock()
	var oldNonce string
	for n := range ws.userFrameGens {
		oldNonce = n
	}
	ws.mu.RUnlock()
	if oldNonce == "" {
		t.Fatalf("no attribution entry recorded for the gen-1 LISTEN")
	}

	// Rotation happens; the sweep re-LISTENs with generation 2.
	provider.rotate(AuthSnapshot{Token: "tok-2", Generation: 2})
	ws.RelistenUserTopics()
	if ws.lastAuthGeneration() != 2 {
		t.Fatalf("sweep did not record generation 2 as last-written")
	}

	// The delayed BADAUTH for the OLD frame arrives after the sweep.
	ws.handleMessage(WSMessage{Type: "RESPONSE", Nonce: oldNonce, Error: "ERR_BADAUTH"})
	if len(got) != 1 {
		t.Fatalf("auth errors observed = %d, want 1", len(got))
	}
	if got[0].Generation != 1 {
		t.Fatalf("stale BADAUTH attributed to generation %d, want 1 (its own frame's)", got[0].Generation)
	}

	// An untracked nonce falls back to the last-written LISTEN generation.
	ws.handleMessage(WSMessage{Type: "RESPONSE", Nonce: "unknown-nonce", Error: "ERR_BADAUTH"})
	if len(got) != 2 || got[1].Generation != 2 {
		t.Fatalf("untracked nonce fallback wrong: %+v", got)
	}
}

// A successful RESPONSE settles its nonce's attribution entry (no leak).
func TestResponseSettlesNonceAttribution(t *testing.T) {
	provider := newRotatingAuth(AuthSnapshot{Token: "tok-1", Generation: 1})
	ws := NewWebSocketClient(0, provider.provider, 60, 60, nil, nil)
	rec := &authRotRecorder{}
	ws.writeTopicFrameHook = rec.hook
	ws.mu.Lock()
	ws.isOpened = true
	ws.mu.Unlock()

	if err := ws.Listen(NewTopic(TopicCommunityPointsUser, "42")); err != nil {
		t.Fatalf("listen: %v", err)
	}
	ws.mu.RLock()
	var nonce string
	for n := range ws.userFrameGens {
		nonce = n
	}
	ws.mu.RUnlock()

	ws.handleMessage(WSMessage{Type: "RESPONSE", Nonce: nonce})
	ws.mu.RLock()
	remaining := len(ws.userFrameGens)
	ws.mu.RUnlock()
	if remaining != 0 {
		t.Fatalf("attribution entries remaining = %d, want 0", remaining)
	}
}
