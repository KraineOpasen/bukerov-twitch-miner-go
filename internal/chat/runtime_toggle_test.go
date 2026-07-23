package chat

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// fakeChatConn is an in-memory IRC transport: writes are swallowed, reads
// block until Close so the client's readLoop parks instead of spinning, and
// Close is idempotent.
type fakeChatConn struct {
	closed chan struct{}
	once   sync.Once
}

func newFakeChatConn() *fakeChatConn { return &fakeChatConn{closed: make(chan struct{})} }

func (c *fakeChatConn) Read([]byte) (int, error) {
	<-c.closed
	return 0, net.ErrClosed
}
func (c *fakeChatConn) Write(b []byte) (int, error) { return len(b), nil }
func (c *fakeChatConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}
func (c *fakeChatConn) LocalAddr() net.Addr              { return fakeAddr{} }
func (c *fakeChatConn) RemoteAddr() net.Addr             { return fakeAddr{} }
func (c *fakeChatConn) SetDeadline(time.Time) error      { return nil }
func (c *fakeChatConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeChatConn) SetWriteDeadline(time.Time) error { return nil }

type fakeAddr struct{}

func (fakeAddr) Network() string { return "fake" }
func (fakeAddr) String() string  { return "fake" }

// newRuntimeChatManager returns a manager whose IRC clients use the in-memory
// transport, plus a dial counter proving how many real join attempts happened.
func newRuntimeChatManager() (*ChatManager, *int32) {
	m := NewChatManager("bot", "tok", nil, false, nil)
	var dials int32
	m.dialFn = func() (net.Conn, error) {
		atomic.AddInt32(&dials, 1)
		return newFakeChatConn(), nil
	}
	return m, &dials
}

func setChatMode(s *models.Streamer, mode models.ChatPresence) {
	next := s.GetSettings()
	next.Chat = mode
	s.SetSettings(next)
}

// TestToggleChatRuntimeNeverToAlwaysJoins: NEVER -> ALWAYS joins immediately
// on the next ToggleChat, with exactly one IRC connection.
func TestToggleChatRuntimeNeverToAlwaysJoins(t *testing.T) {
	m, dials := newRuntimeChatManager()
	s := streamerWithChat("chan", models.ChatNever)

	m.ToggleChat(s)
	if m.hasConnection(s.Username) {
		t.Fatal("NEVER must not join")
	}

	setChatMode(s, models.ChatAlways)
	m.ToggleChat(s)
	if !m.hasConnection(s.Username) {
		t.Fatal("NEVER -> ALWAYS must join chat")
	}
	if got := atomic.LoadInt32(dials); got != 1 {
		t.Fatalf("dials = %d, want 1", got)
	}
}

// TestToggleChatRuntimeAlwaysToNeverLeaves: ALWAYS -> NEVER leaves immediately.
func TestToggleChatRuntimeAlwaysToNeverLeaves(t *testing.T) {
	m, _ := newRuntimeChatManager()
	s := streamerWithChat("chan", models.ChatAlways)

	m.ToggleChat(s)
	if !m.hasConnection(s.Username) {
		t.Fatal("setup: ALWAYS must join")
	}

	setChatMode(s, models.ChatNever)
	m.ToggleChat(s)
	if m.hasConnection(s.Username) {
		t.Fatal("ALWAYS -> NEVER must leave chat")
	}
}

// TestToggleChatRuntimeNeverToOnlineJoinsWhenConfirmedOnline: NEVER -> ONLINE
// with a CONFIRMED-online streamer joins.
func TestToggleChatRuntimeNeverToOnlineJoinsWhenConfirmedOnline(t *testing.T) {
	m, _ := newRuntimeChatManager()
	s := streamerWithChat("chan", models.ChatNever)
	s.SetConfirmedOnline()

	m.ToggleChat(s)
	if m.hasConnection(s.Username) {
		t.Fatal("NEVER must not join even when online")
	}

	setChatMode(s, models.ChatOnline)
	m.ToggleChat(s)
	if !m.hasConnection(s.Username) {
		t.Fatal("NEVER -> ONLINE with confirmed online must join")
	}
}

// TestToggleChatRuntimeAlwaysToOnlineLeavesWhenConfirmedOffline: ALWAYS ->
// ONLINE with a CONFIRMED-offline streamer leaves.
func TestToggleChatRuntimeAlwaysToOnlineLeavesWhenConfirmedOffline(t *testing.T) {
	m, _ := newRuntimeChatManager()
	s := streamerWithChat("chan", models.ChatAlways)
	s.SetConfirmedOffline()

	m.ToggleChat(s)
	if !m.hasConnection(s.Username) {
		t.Fatal("setup: ALWAYS must join regardless of liveness")
	}

	setChatMode(s, models.ChatOnline)
	m.ToggleChat(s)
	if m.hasConnection(s.Username) {
		t.Fatal("ALWAYS -> ONLINE with confirmed offline must leave")
	}
}

// TestToggleChatRuntimeAlwaysToOnlineUnknownKeepsPresence: ALWAYS -> ONLINE
// while liveness is UNKNOWN performs no destructive transition — the
// established connection is retained.
func TestToggleChatRuntimeAlwaysToOnlineUnknownKeepsPresence(t *testing.T) {
	m, dials := newRuntimeChatManager()
	s := streamerWithChat("chan", models.ChatAlways) // fresh streamer: status UNKNOWN

	m.ToggleChat(s)
	if !m.hasConnection(s.Username) {
		t.Fatal("setup: ALWAYS must join")
	}

	setChatMode(s, models.ChatOnline)
	m.ToggleChat(s)
	if !m.hasConnection(s.Username) {
		t.Fatal("ALWAYS -> ONLINE on UNKNOWN must not tear down the connection")
	}
	if got := atomic.LoadInt32(dials); got != 1 {
		t.Fatalf("dials = %d, want 1 (no reconnect churn)", got)
	}
}

// TestToggleChatRuntimeNeverToOfflineJoinsWhenConfirmedOffline: NEVER ->
// OFFLINE with a CONFIRMED-offline streamer joins (offline-lurk mode).
func TestToggleChatRuntimeNeverToOfflineJoinsWhenConfirmedOffline(t *testing.T) {
	m, _ := newRuntimeChatManager()
	s := streamerWithChat("chan", models.ChatNever)
	s.SetConfirmedOffline()

	setChatMode(s, models.ChatOffline)
	m.ToggleChat(s)
	if !m.hasConnection(s.Username) {
		t.Fatal("NEVER -> OFFLINE with confirmed offline must join")
	}
}

// TestToggleChatRuntimeAlwaysToOfflineLeavesWhenConfirmedOnline: ALWAYS ->
// OFFLINE with a CONFIRMED-online streamer leaves.
func TestToggleChatRuntimeAlwaysToOfflineLeavesWhenConfirmedOnline(t *testing.T) {
	m, _ := newRuntimeChatManager()
	s := streamerWithChat("chan", models.ChatAlways)
	s.SetConfirmedOnline()

	m.ToggleChat(s)
	if !m.hasConnection(s.Username) {
		t.Fatal("setup: ALWAYS must join")
	}

	setChatMode(s, models.ChatOffline)
	m.ToggleChat(s)
	if m.hasConnection(s.Username) {
		t.Fatal("ALWAYS -> OFFLINE with confirmed online must leave")
	}
}

// TestToggleChatRuntimeOfflineModeUnknownNotTreatedAsOffline: OFFLINE mode
// never treats UNKNOWN as offline — no join happens on an unconfirmed status.
func TestToggleChatRuntimeOfflineModeUnknownNotTreatedAsOffline(t *testing.T) {
	m, dials := newRuntimeChatManager()
	s := streamerWithChat("chan", models.ChatOffline) // status UNKNOWN

	m.ToggleChat(s)
	if m.hasConnection(s.Username) {
		t.Fatal("OFFLINE mode joined on UNKNOWN status — unknown must not count as offline")
	}
	if got := atomic.LoadInt32(dials); got != 0 {
		t.Fatalf("dials = %d, want 0", got)
	}
}

// TestToggleChatRuntimeRepeatedIdempotentNoDuplicateClient: repeating the same
// ToggleChat never creates a second IRC client or re-dials.
func TestToggleChatRuntimeRepeatedIdempotentNoDuplicateClient(t *testing.T) {
	m, dials := newRuntimeChatManager()
	s := streamerWithChat("chan", models.ChatAlways)

	for i := 0; i < 3; i++ {
		m.ToggleChat(s)
	}
	if !m.hasConnection(s.Username) {
		t.Fatal("ALWAYS must be joined")
	}
	if got := atomic.LoadInt32(dials); got != 1 {
		t.Fatalf("dials = %d, want exactly 1 (idempotent joins)", got)
	}
	m.mu.RLock()
	clients := len(m.clients)
	m.mu.RUnlock()
	if clients != 1 {
		t.Fatalf("clients = %d, want 1 (no duplicate IRC client)", clients)
	}
}

// TestToggleChatRuntimeRemovedStreamerAlwaysLeaves: Leave tears down the
// connection for a removed streamer regardless of its Chat mode, and is a safe
// no-op when repeated or never joined.
func TestToggleChatRuntimeRemovedStreamerAlwaysLeaves(t *testing.T) {
	m, _ := newRuntimeChatManager()
	s := streamerWithChat("chan", models.ChatAlways)

	m.ToggleChat(s)
	if !m.hasConnection(s.Username) {
		t.Fatal("setup: ALWAYS must join")
	}

	m.Leave(s.Username)
	if m.hasConnection(s.Username) {
		t.Fatal("removed streamer must leave chat")
	}
	m.Leave(s.Username) // repeated: no-op
	m.Leave("never-joined")
}

// TestToggleChatRuntimeConcurrentTogglesAndSettings: concurrent ToggleChat and
// settings writes race cleanly (-race) and converge to the final desired mode.
func TestToggleChatRuntimeConcurrentTogglesAndSettings(t *testing.T) {
	m, _ := newRuntimeChatManager()
	s := streamerWithChat("chan", models.ChatAlways)

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				if (g+i)%2 == 0 {
					setChatMode(s, models.ChatAlways)
				} else {
					setChatMode(s, models.ChatNever)
				}
				m.ToggleChat(s)
			}
		}(g)
	}
	wg.Wait()

	// Converge deterministically.
	setChatMode(s, models.ChatNever)
	m.ToggleChat(s)
	if m.hasConnection(s.Username) {
		t.Fatal("final NEVER must leave chat")
	}
	setChatMode(s, models.ChatAlways)
	m.ToggleChat(s)
	if !m.hasConnection(s.Username) {
		t.Fatal("final ALWAYS must join chat")
	}
}
