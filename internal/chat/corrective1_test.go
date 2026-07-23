package chat

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// C8.6: the OFFICIALLY documented login-failed NOTICE (and only that exact
// line) reports an auth rejection carrying the generation the connection
// authenticated with.
func TestLoginFailedNoticeReportsRejectedGeneration(t *testing.T) {
	gen := atomic.Uint64{}
	gen.Store(7)
	c := NewIRCClient("miner", func() TokenSnapshot {
		return TokenSnapshot{Token: "fake-token", Generation: gen.Load()}
	}, models.NewStreamer("chan", models.StreamerSettings{}), nil, false, nil)

	got := make(chan uint64, 4)
	c.authErrorFn = func(rejected uint64) { got <- rejected }

	// The PASS line records which generation was presented.
	_ = c.currentToken()

	// A rotation AFTER authentication must not change the attribution.
	gen.Store(9)

	c.handleMessage(":tmi.twitch.tv NOTICE * :Login authentication failed")
	select {
	case rejected := <-got:
		if rejected != 7 {
			t.Fatalf("rejection attributed to generation %d, want 7 (the one presented at PASS time)", rejected)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("documented login-failed NOTICE did not report an auth rejection")
	}

	// Non-matching NOTICEs are never classified as credential rejections —
	// asserted synchronously against the classifier itself (the async handler
	// path cannot prove an absence deterministically).
	for _, line := range []string{
		":tmi.twitch.tv NOTICE * :Improperly formatted auth",
		":tmi.twitch.tv NOTICE #chan :This room is now in followers-only mode",
		"random line mentioning Login authentication failed somewhere",
	} {
		if isLoginAuthFailedNotice(line) {
			t.Fatalf("non-documented line misclassified as a credential rejection: %q", line)
		}
	}
	if !isLoginAuthFailedNotice("  " + loginAuthFailedNotice + "  ") {
		t.Fatalf("documented line with surrounding whitespace not recognized")
	}
}

// C8.5-supporting: the manager wires the handler into every client it creates.
func TestManagerWiresAuthErrorHandlerIntoClients(t *testing.T) {
	m := NewChatManager("bot", StaticToken("tok"), nil, false, nil)
	var fired atomic.Int64
	m.SetAuthErrorHandler(func(uint64) { fired.Add(1) })
	m.dialFn = newFakeTransport().dial

	s := models.NewStreamer("chan", models.StreamerSettings{Chat: models.ChatAlways})
	m.ToggleChat(s)

	m.mu.RLock()
	client := m.clients["chan"]
	m.mu.RUnlock()
	if client == nil {
		t.Fatalf("client not joined")
	}
	if client.authErrorFn == nil {
		t.Fatalf("manager did not wire the auth-error handler into the client")
	}
	client.handleMessage(":tmi.twitch.tv NOTICE * :Login authentication failed")
	deadline := time.Now().Add(5 * time.Second)
	for fired.Load() == 0 && time.Now().Before(deadline) {
	}
	if fired.Load() != 1 {
		t.Fatalf("handler fired %d times, want 1", fired.Load())
	}
}
