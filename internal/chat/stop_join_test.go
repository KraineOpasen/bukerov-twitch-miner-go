package chat

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// blockingChatLogger blocks RecordChatMessage until the test releases it, so
// "a chat-log analytics write is in flight on the read loop" is a
// deterministic, channel-synchronized state.
type blockingChatLogger struct {
	entered chan struct{}
	release chan struct{}
	wrote   chan struct{}
}

func newBlockingChatLogger() *blockingChatLogger {
	return &blockingChatLogger{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
		wrote:   make(chan struct{}),
	}
}

func (l *blockingChatLogger) RecordChatMessage(string, ChatMessageData) error {
	l.entered <- struct{}{}
	<-l.release
	close(l.wrote)
	return nil
}

// newLoggingTestClient builds a chat-logging client over the in-memory
// transport and pushes one PRIVMSG so the read loop enters the logger.
func newLoggingTestClient(t *testing.T, f *fakeTransport, logger ChatLogger) (*IRCClient, net.Conn) {
	t.Helper()
	streamer := &models.Streamer{Username: "somechannel"}
	c := NewIRCClient("miner", StaticToken("sometoken"), streamer, logger, true, nil)
	c.dialFn = f.dial
	if err := c.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	// A chat-logging client sends CAP REQ, PASS, NICK, JOIN — four lines.
	for i := 0; i < 4; i++ {
		recvLine(t, f.sent)
	}
	serverConn := <-f.serverConns
	return c, serverConn
}

// TestStopJoinsInFlightChatLogWrite (S1): Stop must not return while the read
// loop is still inside the chat-log analytics write — Stop returning early is
// what let that SQLite write race the database close.
func TestStopJoinsInFlightChatLogWrite(t *testing.T) {
	f := newFakeTransport()
	logger := newBlockingChatLogger()
	c, serverConn := newLoggingTestClient(t, f, logger)

	if _, err := serverConn.Write([]byte(":someuser!u@h PRIVMSG #somechannel :hello there\r\n")); err != nil {
		t.Fatalf("push PRIVMSG: %v", err)
	}
	<-logger.entered // the read loop is inside RecordChatMessage

	stopDone := make(chan struct{})
	go func() {
		_ = c.Stop()
		close(stopDone)
	}()

	// Negative assertion (stop_join_test precedent): a joining Stop cannot
	// return while the write is still blocked.
	select {
	case <-stopDone:
		t.Fatal("Stop returned while a chat-log write was still in flight")
	case <-time.After(300 * time.Millisecond):
	}

	close(logger.release)

	select {
	case <-stopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after the chat-log write finished")
	}
	select {
	case <-logger.wrote:
	default:
		t.Fatal("Stop returned before the in-flight chat-log write completed")
	}
}

// TestStopReturnsDespiteWedgedChatLogger (S1): the join is bounded — a logger
// that never returns cannot hang shutdown past stopJoinTimeout.
func TestStopReturnsDespiteWedgedChatLogger(t *testing.T) {
	old := stopJoinTimeout
	stopJoinTimeout = 100 * time.Millisecond
	defer func() { stopJoinTimeout = old }()

	f := newFakeTransport()
	logger := newBlockingChatLogger()
	c, serverConn := newLoggingTestClient(t, f, logger)

	if _, err := serverConn.Write([]byte(":someuser!u@h PRIVMSG #somechannel :hello there\r\n")); err != nil {
		t.Fatalf("push PRIVMSG: %v", err)
	}
	<-logger.entered
	defer close(logger.release) // let the wedged goroutine exit after the test

	start := time.Now()
	done := make(chan struct{})
	go func() {
		_ = c.Stop()
		close(done)
	}()

	select {
	case <-done:
		if elapsed := time.Since(start); elapsed < stopJoinTimeout {
			t.Fatalf("Stop returned before the join timeout (%v) — did it wait at all?", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop blocked far beyond the join timeout — wedged-logger protection missing")
	}
}

// TestStopTwiceIsSafe (S1 idempotency): before S1 a second Stop panicked on
// re-closing the stop channel; repeated shutdown must be a defined no-op.
func TestStopTwiceIsSafe(t *testing.T) {
	f := newFakeTransport()
	c := newTestClient(f)
	if err := c.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	drainHandshake(t, f)

	_ = c.Stop()
	_ = c.Stop() // must not panic and must not block
}

// TestConnectAfterStopIsRefused (S1): a stopped client must never spawn a new
// read loop — it would outlive the shutdown drain.
func TestConnectAfterStopIsRefused(t *testing.T) {
	f := newFakeTransport()
	c := newTestClient(f)

	_ = c.Stop()

	if err := c.Connect(); !errors.Is(err, ErrClientStopped) {
		t.Fatalf("Connect after Stop = %v, want ErrClientStopped", err)
	}
}

// TestManagerCloseRejectsLateJoin (S1 admission): once the manager is closed,
// a stream-check racing shutdown must not create a fresh IRC client whose
// read loop would outlive the drain.
func TestManagerCloseRejectsLateJoin(t *testing.T) {
	f := newFakeTransport()
	m := NewChatManager("miner", StaticToken("sometoken"), nil, false, nil)
	m.dialFn = f.dial

	_ = m.Close()

	streamer := models.NewStreamer("somechannel", models.StreamerSettings{Chat: models.ChatAlways})
	m.ToggleChat(streamer)

	if got := f.dials(); got != 0 {
		t.Fatalf("dials after Close = %d, want 0 — a late join created a client", got)
	}
}
