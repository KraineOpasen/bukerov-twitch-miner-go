package chat

import (
	"bufio"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// fakeTransport is an in-memory replacement for the TLS dialer. Each dial hands
// the client one end of a net.Pipe; a drain goroutine reads everything the
// client writes (net.Pipe is synchronous, so unread writes would block auth)
// and forwards each line to sent. The server end of the most recent pipe is
// exposed on serverConns so a test can push inbound lines to the client.
type fakeTransport struct {
	dialCount   int32
	sent        chan string   // lines written by the client (PASS/NICK/JOIN/...)
	serverConns chan net.Conn // server side of each pipe, one per dial
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{
		sent:        make(chan string, 64),
		serverConns: make(chan net.Conn, 8),
	}
}

func (f *fakeTransport) dial() (net.Conn, error) {
	clientConn, serverConn := net.Pipe()
	atomic.AddInt32(&f.dialCount, 1)
	f.serverConns <- serverConn

	go func() {
		r := bufio.NewReader(serverConn)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			f.sent <- strings.TrimSpace(line)
		}
	}()

	return clientConn, nil
}

func (f *fakeTransport) dials() int32 { return atomic.LoadInt32(&f.dialCount) }

func newTestClient(f *fakeTransport) *IRCClient {
	streamer := &models.Streamer{Username: "somechannel"}
	c := NewIRCClient("miner", func() string { return "sometoken" }, streamer, nil, false, nil)
	c.dialFn = f.dial
	return c
}

func recvLine(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case l := <-ch:
		return l
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for client to send a line")
		return ""
	}
}

// drainHandshake reads the PASS/NICK/JOIN lines a (re)connect sends.
func drainHandshake(t *testing.T, f *fakeTransport) {
	t.Helper()
	var got []string
	for i := 0; i < 3; i++ {
		got = append(got, recvLine(t, f.sent))
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{"PASS oauth:sometoken", "NICK miner", "JOIN #somechannel"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("handshake missing %q, got:\n%s", want, joined)
		}
	}
}

func waitDials(t *testing.T, f *fakeTransport, want int32) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if f.dials() >= want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("dial count did not reach %d (got %d)", want, f.dials())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func recvServer(t *testing.T, f *fakeTransport) net.Conn {
	t.Helper()
	select {
	case sc := <-f.serverConns:
		return sc
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for a server-side connection")
		return nil
	}
}

// TestReconnectOnReadError proves the dead-client bug is fixed: an unexpected
// connection drop triggers a re-dial with a fresh handshake instead of leaving
// the client wedged with IsRunning() stuck true.
func TestReconnectOnReadError(t *testing.T) {
	f := newFakeTransport()
	c := newTestClient(f)

	if err := c.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Stop()

	server := recvServer(t, f)
	drainHandshake(t, f)

	// Simulate an unexpected drop by closing the server side.
	_ = server.Close()

	waitDials(t, f, 2)
	recvServer(t, f)
	drainHandshake(t, f) // reconnect re-authenticates on the new connection

	if !c.IsRunning() {
		t.Fatal("client should still report running after a recovered drop")
	}
}

// TestReconnectOnServerCommand verifies the Twitch RECONNECT command is honoured.
func TestReconnectOnServerCommand(t *testing.T) {
	f := newFakeTransport()
	c := newTestClient(f)

	if err := c.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Stop()

	server := recvServer(t, f)
	drainHandshake(t, f)

	// The server asks the client to reconnect.
	if _, err := server.Write([]byte(":tmi.twitch.tv RECONNECT\r\n")); err != nil {
		t.Fatalf("write RECONNECT: %v", err)
	}

	waitDials(t, f, 2)
	recvServer(t, f)
	drainHandshake(t, f)
}

// TestStopPreventsReconnect ensures a deliberate Stop() does not spawn a
// reconnect loop when the read fails as a result of the close.
func TestStopPreventsReconnect(t *testing.T) {
	f := newFakeTransport()
	c := newTestClient(f)

	if err := c.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	recvServer(t, f)
	drainHandshake(t, f)

	// Drain any post-stop writes (PART) so Stop() doesn't block on net.Pipe.
	var drain sync.WaitGroup
	drain.Add(1)
	go func() {
		defer drain.Done()
		for {
			select {
			case <-f.sent:
			case <-time.After(500 * time.Millisecond):
				return
			}
		}
	}()

	c.Stop()

	if c.IsRunning() {
		t.Fatal("client should not report running after Stop")
	}

	// Give any (incorrect) reconnect attempt time to fire.
	time.Sleep(1500 * time.Millisecond)
	if got := f.dials(); got != 1 {
		t.Fatalf("expected no reconnect after Stop, dial count = %d", got)
	}
	drain.Wait()
}

func TestWithJitterBounds(t *testing.T) {
	base := time.Second
	for i := 0; i < 1000; i++ {
		d := withJitter(base)
		if d < 800*time.Millisecond || d > 1200*time.Millisecond {
			t.Fatalf("jittered delay %v out of ±20%% bounds", d)
		}
	}
}
