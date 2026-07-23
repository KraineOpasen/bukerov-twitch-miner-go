package chat

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// passLine pulls lines off the transport's sent channel until a PASS line
// appears, skipping unrelated lines (NICK/JOIN/PART), so ordering between a
// leave's PART and the next dial's handshake never matters.
func passLine(t *testing.T, f *fakeTransport) string {
	t.Helper()
	for i := 0; i < 8; i++ {
		if l := recvLine(t, f.sent); strings.HasPrefix(l, "PASS ") {
			return l
		}
	}
	t.Fatal("no PASS line observed on the transport")
	return ""
}

// TestNextDialUsesCurrentToken — I6: an IRC client created after a token
// rotation authenticates with the CURRENT token, not a construction-time copy:
// join → leave → rotate → join sends a PASS line carrying the new token only.
func TestNextDialUsesCurrentToken(t *testing.T) {
	var tok atomic.Value
	tok.Store("tok-old")
	tokenFn := func() string { return tok.Load().(string) }

	f := newFakeTransport()
	m := NewChatManager("bot", tokenFn, nil, false, nil)
	m.dialFn = f.dial

	s := streamerWithChat("somechannel", models.ChatAlways)

	// First join: PASS must carry the pre-rotation token.
	m.ToggleChat(s)
	if !m.hasConnection(s.Username) {
		t.Fatal("first join: ALWAYS must join chat")
	}
	pass1 := passLine(t, f)
	if !strings.Contains(pass1, "tok-old") {
		t.Fatalf("first PASS = %q, want it to carry tok-old", pass1)
	}

	// Leave, rotate, rejoin.
	m.Leave(s.Username)
	if m.hasConnection(s.Username) {
		t.Fatal("Leave must remove the IRC client")
	}
	tok.Store("tok-new")
	m.ToggleChat(s)
	if !m.hasConnection(s.Username) {
		t.Fatal("rejoin after rotation: ALWAYS must join chat")
	}

	pass2 := passLine(t, f)
	if !strings.Contains(pass2, "tok-new") {
		t.Fatalf("post-rotation PASS = %q, want it to carry tok-new", pass2)
	}
	if strings.Contains(pass2, "tok-old") {
		t.Fatalf("post-rotation PASS = %q, must NOT carry the rotated-away tok-old", pass2)
	}
	if got := f.dials(); got != 2 {
		t.Fatalf("dials = %d, want 2 (one per join)", got)
	}

	// Tear down: the transport's drain goroutine consumes the PART write.
	m.Leave(s.Username)
}

// TestConcurrentJoinsDoNotDuplicateClients — I7: ten concurrent ToggleChat
// calls for the same ALWAYS streamer converge to exactly one IRC client in the
// manager map and exactly one dial.
func TestConcurrentJoinsDoNotDuplicateClients(t *testing.T) {
	m, dials := newRuntimeChatManager()
	s := streamerWithChat("chan", models.ChatAlways)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.ToggleChat(s)
		}()
	}
	wg.Wait()

	m.mu.RLock()
	clients := len(m.clients)
	m.mu.RUnlock()
	if clients != 1 {
		t.Fatalf("clients = %d, want exactly 1 (no duplicate IRC client)", clients)
	}
	if !m.hasConnection(s.Username) {
		t.Fatal("the single client must be the streamer's connection")
	}
	if got := atomic.LoadInt32(dials); got != 1 {
		t.Fatalf("dials = %d, want exactly 1 (concurrent joins must be idempotent-guarded)", got)
	}
}
