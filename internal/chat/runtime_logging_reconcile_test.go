package chat

import (
	"bufio"
	"errors"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

type c2DelayedReadConn struct {
	readEntered chan struct{}
	releaseRead chan struct{}
	closed      chan struct{}
	readOnce    sync.Once
	closeOnce   sync.Once
}

func newC2DelayedReadConn() *c2DelayedReadConn {
	return &c2DelayedReadConn{
		readEntered: make(chan struct{}),
		releaseRead: make(chan struct{}),
		closed:      make(chan struct{}),
	}
}

func (c *c2DelayedReadConn) Read([]byte) (int, error) {
	c.readOnce.Do(func() { close(c.readEntered) })
	<-c.releaseRead
	return 0, net.ErrClosed
}
func (c *c2DelayedReadConn) Write(b []byte) (int, error) { return len(b), nil }
func (c *c2DelayedReadConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}
func (c *c2DelayedReadConn) LocalAddr() net.Addr              { return fakeAddr{} }
func (c *c2DelayedReadConn) RemoteAddr() net.Addr             { return fakeAddr{} }
func (c *c2DelayedReadConn) SetDeadline(time.Time) error      { return nil }
func (c *c2DelayedReadConn) SetReadDeadline(time.Time) error  { return nil }
func (c *c2DelayedReadConn) SetWriteDeadline(time.Time) error { return nil }

type c2LockProbe struct {
	managerFree bool
	gateFree    bool
}

type c2RecordingChatLogger struct {
	mu     sync.Mutex
	writes int
}

func (l *c2RecordingChatLogger) RecordChatMessage(string, ChatMessageData) error {
	l.mu.Lock()
	l.writes++
	l.mu.Unlock()
	return nil
}

func (l *c2RecordingChatLogger) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.writes
}

// c2ApplyLogging models the existing settings-apply chat order while keeping
// this RED test compile-valid on the untouched base: presence is swept first;
// once ChatManager owns runtime logging reconciliation, the optional interface
// becomes present and its tail barrier is exercised by the same test.
func c2ApplyLogging(m *ChatManager, global bool, logger ChatLogger, streamers ...*models.Streamer) {
	for _, streamer := range streamers {
		m.ToggleChat(streamer)
	}
	if reconciler, ok := any(m).(interface {
		ReconcileLogging(bool, ChatLogger)
	}); ok {
		reconciler.ReconcileLogging(global, logger)
	}
}

func c2HandshakeUntilJoin(t *testing.T, f *fakeTransport) []string {
	t.Helper()
	got := make([]string, 0, 5)
	for i := 0; i < 8; i++ {
		line := recvLine(t, f.sent)
		got = append(got, line)
		if strings.HasPrefix(line, "JOIN ") {
			return got
		}
	}
	t.Fatalf("handshake never reached JOIN: %v", got)
	return nil
}

func c2HasCAP(lines []string) bool {
	for _, line := range lines {
		if line == "CAP REQ :twitch.tv/tags twitch.tv/commands" {
			return true
		}
	}
	return false
}

func newC2LoggingManager(t *testing.T, logger ChatLogger, global bool) (*ChatManager, *fakeTransport, *models.Streamer, *IRCClient) {
	return newC2LoggingManagerWithOverride(t, logger, global, nil)
}

func newC2LoggingManagerWithOverride(t *testing.T, logger ChatLogger, global bool, override *bool) (*ChatManager, *fakeTransport, *models.Streamer, *IRCClient) {
	t.Helper()
	f := newFakeTransport()
	m := NewChatManager("miner", StaticToken("sometoken"), logger, global, nil)
	m.dialFn = f.dial
	t.Cleanup(func() { _ = m.Close() })

	streamer := streamerWithChat("somechannel", models.ChatAlways)
	settings := streamer.GetSettings()
	settings.ChatLogs = override
	streamer.SetSettings(settings)
	m.ToggleChat(streamer)
	_ = recvServer(t, f)
	_ = c2HandshakeUntilJoin(t, f)
	return m, f, streamer, m.clientPtr("somechannel")
}

type c2BlockingOnceLogger struct {
	first     sync.Once
	entered   chan struct{}
	release   chan struct{}
	completed chan struct{}
	mu        sync.Mutex
	writes    int
}

func newC2BlockingOnceLogger() *c2BlockingOnceLogger {
	return &c2BlockingOnceLogger{
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
		completed: make(chan struct{}),
	}
}

func (l *c2BlockingOnceLogger) RecordChatMessage(string, ChatMessageData) error {
	l.first.Do(func() {
		close(l.entered)
		<-l.release
		close(l.completed)
	})
	l.mu.Lock()
	l.writes++
	l.mu.Unlock()
	return nil
}

func (l *c2BlockingOnceLogger) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.writes
}

func TestC2REDInheritedFalseToTrueReconcilesExistingClientOnUnknown(t *testing.T) {
	m, f, streamer, old := newC2LoggingManager(t, nil, false)
	logger := &c2RecordingChatLogger{}

	// Presence reconciliation deliberately does nothing for ONLINE+UNKNOWN;
	// logging reconciliation must nevertheless replace/fence the stale session.
	setChatMode(streamer, models.ChatOnline)
	before := f.dials()
	c2ApplyLogging(m, true, logger, streamer)
	after := f.dials()
	if after != before+1 {
		t.Fatalf("false->true replacement dials = %d, want exactly 1", after-before)
	}

	current := m.clientPtr("somechannel")
	if current == old {
		t.Fatal("false->true retained the stale client generation")
	}
	_ = recvServer(t, f)
	handshake := c2HandshakeUntilJoin(t, f)
	if !c2HasCAP(handshake) {
		t.Fatalf("false->true replacement handshake lacks logging CAP: %v", handshake)
	}
	current.handleMessage(":alice!u@h PRIVMSG #somechannel :after-apply")
	if got := logger.count(); got != 1 {
		t.Fatalf("writes after inherited false->true = %d, want 1; existing client stayed stale (old=%p current=%p)", got, old, current)
	}
}

func TestC2REDInheritedTrueToFalseFencesOldGenerationBeforeReturn(t *testing.T) {
	logger := &c2RecordingChatLogger{}
	m, _, streamer, old := newC2LoggingManager(t, logger, true)

	// OFFLINE+UNKNOWN is also a presence no-op. The logging transition itself
	// is the convergence boundary and must revoke both the installed session
	// and every stale pointer to its retired generation.
	setChatMode(streamer, models.ChatOffline)
	c2ApplyLogging(m, false, nil, streamer)

	current := m.clientPtr("somechannel")
	old.handleMessage(":alice!u@h PRIVMSG #somechannel :late-old-generation")
	current.handleMessage(":alice!u@h PRIVMSG #somechannel :late-current-generation")
	if got := logger.count(); got != 0 {
		t.Fatalf("post-return structured writes after inherited true->false = %d, want 0 (old=%p current=%p)", got, old, current)
	}
}

func TestC2REDFutureJoinUsesReconciledManagerLoggingState(t *testing.T) {
	f := newFakeTransport()
	m := NewChatManager("miner", StaticToken("sometoken"), nil, false, nil)
	m.dialFn = f.dial
	t.Cleanup(func() { _ = m.Close() })
	logger := &c2RecordingChatLogger{}

	// No client exists during apply. A later join must still inherit the newly
	// published manager state and negotiate the logging CAP set.
	c2ApplyLogging(m, true, logger)
	streamer := streamerWithChat("somechannel", models.ChatAlways)
	m.ToggleChat(streamer)
	_ = recvServer(t, f)
	handshake := c2HandshakeUntilJoin(t, f)
	if !c2HasCAP(handshake) {
		t.Fatalf("future join inherited stale no-CAP manager state: %v", handshake)
	}
	m.clientPtr("somechannel").handleMessage(":alice!u@h PRIVMSG #somechannel :future")
	if got := logger.count(); got != 1 {
		t.Fatalf("future join structured writes = %d, want 1", got)
	}
}

func TestC2REDReconnectCannotRestoreStaleLoggingCAPState(t *testing.T) {
	m, f, streamer, _ := newC2LoggingManager(t, nil, false)
	logger := &c2RecordingChatLogger{}

	setChatMode(streamer, models.ChatOnline) // UNKNOWN retains the existing session.
	before := f.dials()
	c2ApplyLogging(m, true, logger, streamer)
	after := f.dials()

	if after != before+1 {
		t.Fatalf("false->true replacement dials = %d, want exactly 1", after-before)
	}
	_ = recvServer(t, f)
	replacementHandshake := c2HandshakeUntilJoin(t, f)
	if !c2HasCAP(replacementHandshake) {
		t.Fatalf("replacement handshake lacks logging CAP: %v", replacementHandshake)
	}

	current := m.clientPtr("somechannel")
	current.handleMessage(":tmi.twitch.tv RECONNECT")
	_ = recvServer(t, f)
	reconnectHandshake := c2HandshakeUntilJoin(t, f)
	if !c2HasCAP(reconnectHandshake) {
		t.Fatalf("internal reconnect restored stale no-CAP semantics: %v", reconnectHandshake)
	}
}

func TestChatLoggingTrueToFalseReconnectKeepsLoggingCAPRevoked(t *testing.T) {
	logger := &c2RecordingChatLogger{}
	m, f, streamer, _ := newC2LoggingManager(t, logger, true)

	setChatMode(streamer, models.ChatOnline) // UNKNOWN retains the existing session.
	before := f.dials()
	c2ApplyLogging(m, false, nil, streamer)
	after := f.dials()
	for dial := before; dial < after; dial++ {
		_ = recvServer(t, f)
		lines := c2HandshakeUntilJoin(t, f)
		if c2HasCAP(lines) {
			t.Fatalf("true->false replacement retained logging CAP: %v", lines)
		}
	}

	current := m.clientPtr("somechannel")
	current.handleMessage(":tmi.twitch.tv RECONNECT")
	_ = recvServer(t, f)
	reconnectHandshake := c2HandshakeUntilJoin(t, f)
	if c2HasCAP(reconnectHandshake) {
		t.Fatalf("true->false reconnect resurrected logging CAP: %v", reconnectHandshake)
	}
	current.handleMessage(":alice!u@h PRIVMSG #somechannel :must-not-log")
	if got := logger.count(); got != 0 {
		t.Fatalf("writes after true->false reconnect = %d, want 0", got)
	}
}

func TestChatLoggingExplicitOverridesSurviveGlobalTransitions(t *testing.T) {
	t.Run("explicit_false_stays_false", func(t *testing.T) {
		override := false
		logger := &c2RecordingChatLogger{}
		m, f, streamer, old := newC2LoggingManagerWithOverride(t, nil, false, &override)
		before := f.dials()

		c2ApplyLogging(m, true, logger, streamer)
		current := m.clientPtr("somechannel")
		if current != old {
			t.Fatal("explicit-false client was needlessly replaced by global enable")
		}
		if got := f.dials(); got != before {
			t.Fatalf("explicit-false global enable dials = %d, want %d", got, before)
		}
		current.handleMessage(":alice!u@h PRIVMSG #somechannel :explicit-false")
		if got := logger.count(); got != 0 {
			t.Fatalf("explicit-false writes = %d, want 0", got)
		}
	})

	t.Run("global_true_and_explicit_true_stay_true", func(t *testing.T) {
		override := true
		logger := &c2RecordingChatLogger{}
		m, f, streamer, current := newC2LoggingManagerWithOverride(t, logger, true, &override)
		before := f.dials()

		c2ApplyLogging(m, true, logger, streamer)
		if got := f.dials(); got != before {
			t.Fatalf("global-true/explicit-true identical apply dials = %d, want %d", got, before)
		}
		if got := m.clientPtr("somechannel"); got != current {
			t.Fatal("global-true/explicit-true client was replaced by an identical apply")
		}
		current.handleMessage(":alice!u@h PRIVMSG #somechannel :explicit-true")
		if got := logger.count(); got != 1 {
			t.Fatalf("explicit-true writes = %d, want 1", got)
		}
	})
}

func TestChatLoggingRepeatedApplyIsIdempotent(t *testing.T) {
	m, f, streamer, _ := newC2LoggingManager(t, nil, false)
	logger := &c2RecordingChatLogger{}

	c2ApplyLogging(m, true, logger, streamer)
	current := m.clientPtr("somechannel")
	dials := f.dials()
	c2ApplyLogging(m, true, logger, streamer)

	if got := m.clientPtr("somechannel"); got != current {
		t.Fatal("identical logging apply replaced an already-current generation")
	}
	if got := f.dials(); got != dials {
		t.Fatalf("identical logging apply dials = %d, want %d", got, dials)
	}
}

func TestChatLoggingRapidTransitionsConvergeToFinalGeneration(t *testing.T) {
	m, _, streamer, generation0 := newC2LoggingManager(t, nil, false)
	logger := &c2RecordingChatLogger{}

	c2ApplyLogging(m, true, logger, streamer)
	generation1 := m.clientPtr("somechannel")
	c2ApplyLogging(m, false, nil, streamer)
	generation2 := m.clientPtr("somechannel")
	c2ApplyLogging(m, true, logger, streamer)
	generation3 := m.clientPtr("somechannel")

	for _, stale := range []*IRCClient{generation0, generation1, generation2} {
		stale.handleMessage(":alice!u@h PRIVMSG #somechannel :stale")
	}
	generation3.handleMessage(":alice!u@h PRIVMSG #somechannel :current")
	if got := logger.count(); got != 1 {
		t.Fatalf("rapid false->true->false->true writes = %d, want only final generation's 1", got)
	}
	m.mu.RLock()
	clients, pending := len(m.clients), len(m.pending)
	m.mu.RUnlock()
	if clients != 1 || pending != 0 {
		t.Fatalf("rapid transition registry = %d clients/%d pending, want 1/0", clients, pending)
	}
}

func TestChatLoggingBarrierDrainsInFlightStaleWrite(t *testing.T) {
	logger := newC2BlockingOnceLogger()
	m, _, streamer, old := newC2LoggingManager(t, logger, true)
	setChatMode(streamer, models.ChatOnline) // UNKNOWN must not bypass the barrier.

	writeDone := make(chan struct{})
	go func() {
		old.handleMessage(":alice!u@h PRIVMSG #somechannel :in-flight")
		close(writeDone)
	}()
	<-logger.entered

	reconcileDone := make(chan struct{})
	go func() {
		c2ApplyLogging(m, false, nil, streamer)
		close(reconcileDone)
	}()

	// Go's RWMutex excludes new readers once a writer is waiting. Observing
	// TryRLock fail proves ReconcileLogging reached the gate writer while the
	// synchronized sink call above still owns its read admission.
	deadline := time.After(3 * time.Second)
	for m.logGate.mu.TryRLock() {
		m.logGate.mu.RUnlock()
		select {
		case <-deadline:
			t.Fatal("logging reconciliation never reached the write barrier")
		default:
			runtime.Gosched()
		}
	}
	select {
	case <-reconcileDone:
		t.Fatal("logging reconciliation returned while a stale structured write was in flight")
	default:
	}

	close(logger.release)
	select {
	case <-writeDone:
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight structured write did not finish after release")
	}
	select {
	case <-reconcileDone:
	case <-time.After(3 * time.Second):
		t.Fatal("logging reconciliation did not finish after the admitted write drained")
	}
	select {
	case <-logger.completed:
	default:
		t.Fatal("reconciliation returned before the admitted sink call completed")
	}

	old.handleMessage(":alice!u@h PRIVMSG #somechannel :post-return-stale")
	if got := logger.count(); got != 1 {
		t.Fatalf("writes after barrier = %d, want only the pre-boundary admitted write", got)
	}
}

func TestChatLoggingPendingReplacementCannotDuplicateOrSurviveClose(t *testing.T) {
	m, _ := newRuntimeChatManager()
	streamer := streamerWithChat("chan", models.ChatAlways)
	m.ToggleChat(streamer)

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblockDial := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblockDial()
	var replacementDials atomic.Int32
	m.mu.Lock()
	m.dialFn = func() (net.Conn, error) {
		replacementDials.Add(1)
		entered <- struct{}{}
		<-release
		return newFakeChatConn(), nil
	}
	m.mu.Unlock()

	reconcileDone := make(chan struct{})
	go func() {
		m.ReconcileLogging(true, &c2RecordingChatLogger{})
		close(reconcileDone)
	}()
	<-entered // replacement dial is in flight and reserved.

	toggleDone := make(chan struct{})
	go func() {
		m.ToggleChat(streamer)
		close(toggleDone)
	}()
	select {
	case <-toggleDone:
	case <-time.After(3 * time.Second):
		t.Fatal("ToggleChat blocked behind a reserved replacement dial")
	}
	if got := replacementDials.Load(); got != 1 {
		t.Fatalf("ToggleChat started %d replacement dials while one was reserved, want 1", got)
	}
	closeResult := make(chan error, 1)
	go func() { closeResult <- m.Close() }()
	select {
	case err := <-closeResult:
		if err != nil {
			t.Fatalf("Close while replacement dial was pending: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close blocked on a pending replacement dial")
	}
	unblockDial()
	select {
	case <-reconcileDone:
	case <-time.After(3 * time.Second):
		t.Fatal("reconciliation did not release after Close cancelled its dial")
	}

	m.mu.RLock()
	clients, pending, closed := len(m.clients), len(m.pending), m.closed
	m.mu.RUnlock()
	if !closed || clients != 0 || pending != 0 {
		t.Fatalf("post-Close registry = closed:%v clients:%d pending:%d, want true/0/0", closed, clients, pending)
	}
	if got := replacementDials.Load(); got != 1 {
		t.Fatalf("post-Close replacement dials = %d, want exactly 1", got)
	}
}

func TestChatLoggingStaleReadEpochCannotReconnectFreshTransport(t *testing.T) {
	streamer := streamerWithChat("chan", models.ChatAlways)
	client := NewIRCClient("bot", StaticToken("tok"), streamer, nil, false, nil)
	oldConn := newC2DelayedReadConn()
	oldReader := bufio.NewReader(oldConn)
	freshConn := newFakeChatConn()
	freshReader := bufio.NewReader(freshConn)
	var releaseOnce sync.Once
	defer func() {
		releaseOnce.Do(func() { close(oldConn.releaseRead) })
		_ = client.Stop()
		_ = oldConn.Close()
	}()

	client.mu.Lock()
	client.conn = oldConn
	client.reader = oldReader
	client.connEpoch = 7
	client.running = true
	client.mu.Unlock()

	loopDone := make(chan struct{})
	go func() {
		client.readLoop(oldReader, 7, 1)
		close(loopDone)
	}()
	select {
	case <-oldConn.readEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("old read loop did not enter its controlled read")
	}

	client.mu.Lock()
	client.conn = freshConn
	client.reader = freshReader
	client.connEpoch = 8
	client.reconnecting = false
	client.mu.Unlock()
	releaseOnce.Do(func() { close(oldConn.releaseRead) })
	select {
	case <-loopDone:
	case <-time.After(3 * time.Second):
		t.Fatal("stale read loop did not return")
	}

	client.mu.RLock()
	gotConn := client.conn
	gotEpoch := client.connEpoch
	reconnecting := client.reconnecting
	client.mu.RUnlock()
	if gotConn != freshConn || gotEpoch != 8 || reconnecting {
		t.Fatalf("stale read error changed fresh authority: conn=%p epoch=%d reconnecting=%v", gotConn, gotEpoch, reconnecting)
	}
	select {
	case <-freshConn.closed:
		t.Fatal("stale read error closed the fresh transport")
	default:
	}
}

func TestChatLoggingRetirementRevokesReconnectBeforeAsyncStop(t *testing.T) {
	m, _, _, old := newC2LoggingManager(t, nil, false)
	old.mu.RLock()
	epoch := old.connEpoch
	old.mu.RUnlock()

	m.mu.Lock()
	retirements := m.detachLoginLocked("somechannel")
	// Stop has only been registered, not started. Reconnect authority must
	// already be gone at the registry publication boundary.
	admitted := old.beginReconnect(epoch)
	old.mu.RLock()
	lifecycleAuthorized := old.lifecycleAuthorized
	old.mu.RUnlock()
	m.mu.Unlock()
	if admitted || lifecycleAuthorized || old.logAuthorized.Load() {
		t.Fatalf("detached generation retained handoff authority: reconnect=%v lifecycle=%v log=%v",
			admitted, lifecycleAuthorized, old.logAuthorized.Load())
	}
	m.waitClientRetirements(retirements)
}

func TestChatLoggingApplySupersedesBlockedOrdinaryJoinOutsideLocks(t *testing.T) {
	m := NewChatManager("bot", StaticToken("tok"), nil, false, nil)
	streamer := streamerWithChat("chan", models.ChatAlways)
	firstConn := newFakeChatConn()
	probe := make(chan c2LockProbe, 1)
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	defer func() {
		unblock()
		_ = m.Close()
	}()
	var dials atomic.Int32
	m.dialFn = func() (net.Conn, error) {
		if dials.Add(1) == 1 {
			managerFree := m.mu.TryLock()
			if managerFree {
				m.mu.Unlock()
			}
			gateFree := m.logGate.mu.TryLock()
			if gateFree {
				m.logGate.mu.Unlock()
			}
			probe <- c2LockProbe{managerFree: managerFree, gateFree: gateFree}
			<-releaseFirst
			return firstConn, nil
		}
		return newFakeChatConn(), nil
	}

	firstToggleDone := make(chan struct{})
	go func() {
		m.ToggleChat(streamer)
		close(firstToggleDone)
	}()
	select {
	case got := <-probe:
		if !got.managerFree || !got.gateFree {
			t.Fatalf("ordinary dial lock probe = %+v, want manager/gate both free", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ordinary join did not reach controlled dial")
	}

	m.mu.RLock()
	firstReservation := m.pending["chan"]
	m.mu.RUnlock()
	if firstReservation == nil {
		t.Fatal("blocked ordinary join has no per-login reservation")
	}
	secondToggleDone := make(chan struct{})
	go func() {
		m.ToggleChat(streamer)
		close(secondToggleDone)
	}()
	select {
	case <-secondToggleDone:
	case <-time.After(3 * time.Second):
		t.Fatal("duplicate ToggleChat blocked behind the first dial")
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("duplicate ToggleChat dials = %d, want 1", got)
	}

	logger := &c2RecordingChatLogger{}
	reconcileDone := make(chan struct{})
	go func() {
		m.ReconcileLogging(true, logger)
		close(reconcileDone)
	}()
	select {
	case <-reconcileDone:
	case <-time.After(3 * time.Second):
		t.Fatal("logging apply waited behind an unrelated blocked ordinary dial")
	}

	current := m.clientPtr("chan")
	if current == nil || current == firstReservation.candidate {
		t.Fatal("logging apply did not install a distinct current-generation candidate")
	}
	if !current.logChat || current.logGeneration != m.logGeneration || !current.logAuthorized.Load() {
		t.Fatalf("current candidate authority = log:%v generation:%d/%d authorized:%v",
			current.logChat, current.logGeneration, m.logGeneration, current.logAuthorized.Load())
	}
	current.handleMessage(":alice!u@h PRIVMSG #chan :current")
	if got := logger.count(); got != 1 {
		t.Fatalf("current-generation writes = %d, want 1", got)
	}

	unblock()
	select {
	case <-firstToggleDone:
	case <-time.After(3 * time.Second):
		t.Fatal("superseded ordinary join did not finish after dial release")
	}
	select {
	case <-firstConn.closed:
	default:
		t.Fatal("superseded late dial was not closed")
	}
	if got := m.clientPtr("chan"); got != current {
		t.Fatal("superseded ordinary join replaced the current exact-ticket owner")
	}
	m.mu.RLock()
	clients, pending := len(m.clients), len(m.pending)
	m.mu.RUnlock()
	if clients != 1 || pending != 0 || dials.Load() != 2 {
		t.Fatalf("final registry/dials = %d clients/%d pending/%d dials, want 1/0/2", clients, pending, dials.Load())
	}
}

func TestChatLoggingCloseCancelsBlockedOrdinaryJoin(t *testing.T) {
	m := NewChatManager("bot", StaticToken("tok"), nil, false, nil)
	streamer := streamerWithChat("chan", models.ChatAlways)
	lateConn := newFakeChatConn()
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var dials atomic.Int32
	m.dialFn = func() (net.Conn, error) {
		dials.Add(1)
		close(entered)
		<-release
		return lateConn, nil
	}

	toggleDone := make(chan struct{})
	go func() {
		m.ToggleChat(streamer)
		close(toggleDone)
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("ordinary join did not reach controlled dial")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- m.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close with blocked ordinary join: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close waited for an unbound ordinary dial")
	}
	unblock()
	select {
	case <-toggleDone:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled ordinary join did not return after dial release")
	}
	select {
	case <-lateConn.closed:
	default:
		t.Fatal("post-Close dial result was not closed")
	}
	m.mu.RLock()
	closed, clients, pending := m.closed, len(m.clients), len(m.pending)
	m.mu.RUnlock()
	if !closed || clients != 0 || pending != 0 || dials.Load() != 1 {
		t.Fatalf("post-Close state = closed:%v clients:%d pending:%d dials:%d, want true/0/0/1", closed, clients, pending, dials.Load())
	}
}

func TestChatLoggingReplacementConnectFailureUsesCurrentGenerationReconnect(t *testing.T) {
	f := newFakeTransport()
	logger := &c2RecordingChatLogger{}
	m := NewChatManager("miner", StaticToken("sometoken"), nil, false, nil)
	defer func() { _ = m.Close() }()
	connectFailure := errors.New("deterministic replacement dial failure")
	var attempts atomic.Int32
	m.dialFn = func() (net.Conn, error) {
		if attempts.Add(1) == 2 {
			return nil, connectFailure
		}
		return f.dial()
	}
	streamer := streamerWithChat("somechannel", models.ChatAlways)
	m.ToggleChat(streamer)
	_ = recvServer(t, f)
	_ = c2HandshakeUntilJoin(t, f)

	m.ReconcileLogging(true, logger)
	current := m.clientPtr("somechannel")
	if current == nil || !current.logChat || !current.logAuthorized.Load() {
		t.Fatal("failed replacement did not retain an authoritative current-policy recovery owner")
	}
	current.mu.RLock()
	recovering := current.reconnecting && current.lifecycleAuthorized
	current.mu.RUnlock()
	if !recovering {
		t.Fatal("failed replacement did not enter the existing reconnect loop")
	}

	_ = recvServer(t, f)
	handshake := c2HandshakeUntilJoin(t, f)
	if !c2HasCAP(handshake) {
		t.Fatalf("replacement recovery handshake lacks current logging CAP: %v", handshake)
	}
	current.handleMessage(":alice!u@h PRIVMSG #somechannel :recovered")
	if got := logger.count(); got != 1 {
		t.Fatalf("recovered current-generation writes = %d, want 1", got)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("connect attempts = %d, want initial + failed replacement + one recovery", got)
	}
}

func TestChatLoggingReplacementPreservesRotatedTokenAndAuthGeneration(t *testing.T) {
	f := newFakeTransport()
	var tokenMu sync.Mutex
	token := TokenSnapshot{Token: "old-token", Generation: 11}
	provider := func() TokenSnapshot {
		tokenMu.Lock()
		defer tokenMu.Unlock()
		return token
	}
	m := NewChatManager("miner", provider, nil, false, nil)
	m.dialFn = f.dial
	defer func() { _ = m.Close() }()
	rejected := make(chan uint64, 1)
	m.SetAuthErrorHandler(func(generation uint64) { rejected <- generation })
	streamer := streamerWithChat("somechannel", models.ChatAlways)
	m.ToggleChat(streamer)
	_ = recvServer(t, f)
	initial := c2HandshakeUntilJoin(t, f)
	if !strings.Contains(strings.Join(initial, "\n"), "PASS oauth:old-token") {
		t.Fatalf("initial handshake did not use old token: %v", initial)
	}

	tokenMu.Lock()
	token = TokenSnapshot{Token: "rotated-token", Generation: 12}
	tokenMu.Unlock()
	m.ReconcileLogging(true, &c2RecordingChatLogger{})
	_ = recvServer(t, f)
	replacement := c2HandshakeUntilJoin(t, f)
	joined := strings.Join(replacement, "\n")
	if !c2HasCAP(replacement) || !strings.Contains(joined, "PASS oauth:rotated-token") {
		t.Fatalf("replacement handshake did not preserve CAP/current token semantics: %v", replacement)
	}

	m.clientPtr("somechannel").handleMessage(loginAuthFailedNotice)
	select {
	case generation := <-rejected:
		if generation != 12 {
			t.Fatalf("replacement auth rejection generation = %d, want 12", generation)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("replacement auth rejection was not reported")
	}
}
