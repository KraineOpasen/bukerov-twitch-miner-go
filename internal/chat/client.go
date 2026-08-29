package chat

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	mathrand "math/rand"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

type ChatLogger interface {
	RecordChatMessage(streamer string, msg ChatMessageData) error
}

// chatLogGate is the manager-wide structured-write linearization point. Every
// actual ChatLogger call holds mu for reading for the entire sink call. Runtime
// settings reconciliation takes it for writing, which drains every previously
// admitted write -- including one from a client already detached after a
// bounded Stop timeout -- before it changes logging authority.
type chatLogGate struct {
	mu         sync.RWMutex
	generation uint64
	logger     ChatLogger
}

// MentionHandler is called when the user is mentioned in chat.
type MentionHandler func(streamer, fromUser, message string)

type ChatMessageData struct {
	Username    string
	DisplayName string
	Message     string
	Emotes      string
	Badges      string
	Color       string
}

// TokenSnapshot is the credential view an IRC client needs at authentication
// time: the current access token and the credential generation it belongs to,
// so an authentication rejection can be attributed to the exact credential
// set that was presented. Never carries the refresh token.
type TokenSnapshot struct {
	Token      string
	Generation uint64
}

// TokenProvider supplies the current TokenSnapshot at dial/reconnect time.
type TokenProvider func() TokenSnapshot

// StaticToken wraps a fixed token as a provider, for tests and library use.
func StaticToken(token string) TokenProvider {
	return func() TokenSnapshot { return TokenSnapshot{Token: token} }
}

// loginAuthFailedNotice is the OFFICIALLY documented Twitch IRC response to a
// failed login authentication (dev.twitch.tv/docs/irc/authenticate-bot). It
// is matched by exact equality of the trimmed line — never by substring — so
// no other NOTICE can be misread as a credential rejection.
const loginAuthFailedNotice = ":tmi.twitch.tv NOTICE * :Login authentication failed"

// isLoginAuthFailedNotice classifies one raw IRC line against the documented
// authentication-rejection shape (exact trimmed equality only).
func isLoginAuthFailedNotice(line string) bool {
	return strings.TrimSpace(line) == loginAuthFailedNotice
}

type IRCClient struct {
	username string
	// tokenFn supplies the CURRENT OAuth credential snapshot at
	// authentication time, so a dial or internal reconnect after a token
	// rotation presents the rotated token instead of a construction-time
	// copy. Set once, immutable.
	tokenFn TokenProvider
	// authErrorFn, when set, is invoked (on its own goroutine, never blocking
	// the read loop) with the credential generation whose token this
	// connection last presented, when the server answers with the documented
	// login-authentication-failed NOTICE. Set once at construction.
	authErrorFn func(rejectedGeneration uint64)
	// lastAuthGen is the generation of the token last sent in a PASS line
	// (guarded by mu).
	lastAuthGen    uint64
	channel        string
	streamer       *models.Streamer
	logGate        *chatLogGate
	logGeneration  uint64
	logAuthorized  atomic.Bool
	logChat        bool
	mentionHandler MentionHandler
	// lifecycleAuthorized is the manager/session ownership bit. Manager-owned
	// candidates complete dial/auth/JOIN while false and cannot read, reconnect,
	// report auth failures, or emit mentions until their exact reservation CAS
	// activates them. Guarded by mu; standalone clients start true.
	lifecycleAuthorized bool

	conn   net.Conn
	reader *bufio.Reader
	// connEpoch is incremented whenever a freshly dialed transport becomes the
	// current connection. Every read loop carries its immutable epoch, so a
	// delayed error or RECONNECT line from an old reader cannot close or replace
	// the fresh transport. Guarded by mu.
	connEpoch uint64
	// sessionReady means the current reader completed auth/JOIN and may start a
	// read loop once lifecycle authority is granted. Guarded by mu.
	sessionReady bool
	running      bool
	stopChan     chan struct{}

	// forcedClose is set by Stop() and permanently disables reconnection.
	// reconnecting guards against overlapping reconnect loops (a read error and
	// a server RECONNECT command can race).
	forcedClose  bool
	reconnecting bool

	// dialFn opens a new connection. It is nil in production (dial() is used);
	// tests inject an in-memory transport here.
	dialFn func() (net.Conn, error)

	// loopWG counts this client's read loops (every generation, including
	// reconnect-spawned ones) and any in-flight reconnect goroutine (S1).
	// Every Add happens under mu immediately after a forcedClose check, and
	// Stop sets forcedClose under the same mu before it waits — so an Add can
	// never race a Wait that already observed zero. Joining is what lets Stop
	// guarantee an in-flight chat-message analytics write has finished before
	// the caller proceeds toward closing the shared database handle.
	loopWG sync.WaitGroup

	mu sync.RWMutex
}

// ErrClientStopped is returned by Connect once Stop has been called: a
// stopped client must never spawn a new read loop (it would outlive the
// shutdown drain).
var ErrClientStopped = errors.New("chat: client stopped")

// errStaleConnection is internal control flow: a delayed reader/handshake from
// an old transport epoch must never send on or claim the fresh connection.
var errStaleConnection = errors.New("chat: stale connection generation")

// stopJoinTimeout bounds how long Stop waits for the read loop to drain an
// in-flight message (which may be writing a chat row) before giving up, so a
// wedged handler can never block shutdown indefinitely. Package variable so
// tests can shrink it (the watcher/drops precedent). 3s keeps the aggregate
// shutdown drain budget documented in miner.stop() under the process-level
// shutdown deadline; clients are joined concurrently by ChatManager.Close,
// so this is the manager's worst case too.
var stopJoinTimeout = 3 * time.Second

// errLoopJoinTimeout is the explicit shutdown error Stop returns when this
// client's loops did not finish inside stopJoinTimeout.
var errLoopJoinTimeout = errors.New("chat: client loop join timed out")

// partWriteTimeout bounds the courtesy PART write in Stop. Without a
// deadline that blocking write could wedge Stop before the join even starts
// (a full send buffer, or an unread in-memory pipe in tests), making the
// join bound meaningless.
var partWriteTimeout = time.Second

func NewIRCClient(username string, tokenFn TokenProvider, streamer *models.Streamer, logger ChatLogger, logChat bool, mentionHandler MentionHandler) *IRCClient {
	gate := &chatLogGate{generation: 1, logger: logger}
	return newIRCClient(username, tokenFn, streamer, logger, logChat, mentionHandler, gate, 1, true)
}

// newIRCClient constructs one immutable IRC/session logging generation.
// Manager-owned candidates start unauthorized and gain authority only after
// their exact-pointer install succeeds; standalone clients returned by
// NewIRCClient are authoritative immediately.
func newIRCClient(username string, tokenFn TokenProvider, streamer *models.Streamer, logger ChatLogger, logChat bool, mentionHandler MentionHandler, gate *chatLogGate, generation uint64, authorized bool) *IRCClient {
	// Captured once, race-safe (Username can be mutated in place by a
	// concurrent rename): this connection's IRC channel is fixed for its
	// lifetime, exactly the login it was joined under. A later rename does
	// not retarget an already-open connection — the caller (ChatManager)
	// leaves this login's client and joins a fresh one under the new login.
	login := streamer.GetUsername()
	slog.Debug("Creating IRC client", "channel", login, "logChat", logChat, "hasLogger", logger != nil)
	client := &IRCClient{
		username:            username,
		tokenFn:             tokenFn,
		channel:             "#" + strings.ToLower(login),
		streamer:            streamer,
		logGate:             gate,
		logGeneration:       generation,
		logChat:             logChat,
		mentionHandler:      mentionHandler,
		lifecycleAuthorized: authorized,
		stopChan:            make(chan struct{}),
	}
	client.logAuthorized.Store(authorized)
	return client
}

// dial opens a TLS connection to Twitch IRC. The OAuth token is sent as the
// PASS line during authentication, so plaintext (port 6667) would leak it on
// the wire; we always use the TLS port. tls.DialWithDialer applies the dialer's
// timeout to both the TCP connect and the TLS handshake as a whole.
func (c *IRCClient) dial() (net.Conn, error) {
	if c.dialFn != nil {
		return c.dialFn()
	}
	addr := net.JoinHostPort(constants.IRCURL, fmt.Sprintf("%d", constants.IRCPortTLS))
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	return tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: constants.IRCURL,
	})
}

func (c *IRCClient) Connect() error {
	conn, err := c.dial()
	if err != nil {
		return fmt.Errorf("failed to connect to IRC: %w", err)
	}

	c.mu.Lock()
	if c.forcedClose {
		// Stop landed while this dial was in flight; never bind the fresh
		// connection to a stopped client.
		c.mu.Unlock()
		_ = conn.Close()
		return ErrClientStopped
	}
	reader := bufio.NewReader(conn)
	c.conn = conn
	c.reader = reader
	c.connEpoch++
	epoch := c.connEpoch
	c.running = true
	c.mu.Unlock()

	authGeneration, err := c.authenticateAt(epoch)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("failed to authenticate: %w", err)
	}

	if err := c.joinAt(epoch); err != nil {
		_ = conn.Close()
		return fmt.Errorf("failed to join channel: %w", err)
	}

	var startReadLoop func()
	c.mu.Lock()
	if c.forcedClose || c.connEpoch != epoch {
		// Stop raced the handshake; it already closed the conn it saw. Do not
		// spawn a read loop for a stopped client.
		c.mu.Unlock()
		_ = conn.Close()
		return ErrClientStopped
	}
	c.sessionReady = true
	if c.lifecycleAuthorized {
		// Registered under the same mu that checked forcedClose — the
		// no-Add-after-Wait pairing with Stop (see loopWG).
		c.loopWG.Add(1)
		startReadLoop = func() {
			go func() {
				defer c.loopWG.Done()
				c.readLoop(reader, epoch, authGeneration)
			}()
		}
	}
	c.mu.Unlock()
	if startReadLoop != nil {
		startReadLoop()
	}

	slog.Info("Joined IRC chat", "channel", c.channel)
	return nil
}

// activate grants a manager candidate lifecycle/write authority after its
// exact reservation is published. It returns a starter so ChatManager can
// release m.mu before any read/reconnect goroutine begins. WaitGroup admission
// happens here under c.mu, paired with Stop's forcedClose transition.
func (c *IRCClient) activate(reconnectIfUnavailable bool) (func(), bool) {
	c.mu.Lock()
	if c.forcedClose || c.lifecycleAuthorized {
		c.mu.Unlock()
		return nil, false
	}
	c.lifecycleAuthorized = true
	c.logAuthorized.Store(true)

	if c.sessionReady && c.reader != nil {
		reader := c.reader
		epoch := c.connEpoch
		authGeneration := c.lastAuthGen
		c.loopWG.Add(1)
		c.mu.Unlock()
		return func() {
			go func() {
				defer c.loopWG.Done()
				c.readLoop(reader, epoch, authGeneration)
			}()
		}, true
	}
	if !reconnectIfUnavailable || c.reconnecting {
		c.lifecycleAuthorized = false
		c.logAuthorized.Store(false)
		c.mu.Unlock()
		return nil, false
	}

	c.reconnecting = true
	c.running = true
	c.sessionReady = false
	c.loopWG.Add(1)
	old := c.conn
	c.mu.Unlock()
	return func() { go c.reconnectLoop(old) }, true
}

// revoke synchronously removes lifecycle and structured-write authority with
// no network I/O. ChatManager calls it while it still owns the registry/gate
// publication, closing the handoff window in which a detached reader could
// otherwise acquire fresh reconnect authority before asynchronous Stop starts.
func (c *IRCClient) revoke() {
	c.logAuthorized.Store(false)
	c.mu.Lock()
	c.lifecycleAuthorized = false
	c.sessionReady = false
	c.mu.Unlock()
}

// currentToken resolves the token provider (nil-safe for library/test use)
// and records which credential generation is being presented, so a later
// login-failed NOTICE is attributed to the exact snapshot this connection
// authenticated with — not to whatever is current when the NOTICE arrives.
func (c *IRCClient) currentToken() string {
	snap := c.tokenSnapshot()
	c.mu.Lock()
	c.lastAuthGen = snap.Generation
	c.mu.Unlock()
	return snap.Token
}

func (c *IRCClient) tokenSnapshot() TokenSnapshot {
	if c.tokenFn == nil {
		return TokenSnapshot{}
	}
	return c.tokenFn()
}

func (c *IRCClient) authenticateAt(epoch uint64) (uint64, error) {
	if c.logChat {
		if err := c.sendAt(epoch, "CAP REQ :twitch.tv/tags twitch.tv/commands"); err != nil {
			return 0, err
		}
	}
	snap := c.tokenSnapshot()
	c.mu.Lock()
	if c.forcedClose || c.connEpoch != epoch {
		c.mu.Unlock()
		return 0, errStaleConnection
	}
	c.lastAuthGen = snap.Generation
	c.mu.Unlock()
	if err := c.sendAt(epoch, fmt.Sprintf("PASS oauth:%s", snap.Token)); err != nil {
		return 0, err
	}
	return snap.Generation, c.sendAt(epoch, fmt.Sprintf("NICK %s", c.username))
}

func (c *IRCClient) joinAt(epoch uint64) error {
	return c.sendAt(epoch, fmt.Sprintf("JOIN %s", c.channel))
}

// sendAt snapshots the exact epoch's conn under c.mu, then releases the lock
// before network I/O. An epoch of zero is the direct/current helper path.
func (c *IRCClient) sendAt(epoch uint64, message string) error {
	c.mu.RLock()
	if c.forcedClose || (epoch != 0 && epoch != c.connEpoch) {
		c.mu.RUnlock()
		return errStaleConnection
	}
	conn := c.conn
	c.mu.RUnlock()

	if conn == nil {
		return fmt.Errorf("not connected")
	}

	_, err := conn.Write([]byte(message + "\r\n"))
	return err
}

func (c *IRCClient) readLoop(reader *bufio.Reader, epoch, authGeneration uint64) {
	for {
		select {
		case <-c.stopChan:
			return
		default:
		}

		c.mu.RLock()
		running := c.running
		current := c.lifecycleAuthorized && c.connEpoch == epoch
		c.mu.RUnlock()

		if !running || !current || reader == nil {
			return
		}

		line, err := reader.ReadString('\n')
		if err != nil {
			// Admission is synchronous and owned by this exact transport epoch.
			// Stop or a newer connection makes beginReconnect reject the stale
			// error without touching the current connection.
			if c.beginReconnect(epoch) {
				slog.Debug("IRC read error, reconnecting", "channel", c.channel, "error", err)
			}
			return
		}

		c.mu.RLock()
		current = !c.forcedClose && c.lifecycleAuthorized && c.connEpoch == epoch
		c.mu.RUnlock()
		if !current {
			return
		}
		line = strings.TrimSpace(line)
		c.handleMessageFromEpoch(line, epoch, authGeneration)
	}
}

// beginReconnect synchronously transfers reconnect authority from one exact
// transport epoch into the existing backoff loop. sourceEpoch==0 is the
// direct/current helper form. Manager recovery after an initial Connect failure
// is admitted by activate. No network I/O occurs in this admission section.
func (c *IRCClient) beginReconnect(sourceEpoch uint64) bool {
	c.mu.Lock()
	if c.forcedClose || !c.lifecycleAuthorized || c.reconnecting ||
		(sourceEpoch != 0 && sourceEpoch != c.connEpoch) {
		c.mu.Unlock()
		return false
	}
	c.reconnecting = true
	c.running = true
	c.sessionReady = false
	// The reconnect goroutine itself is tracked (admission-checked by the
	// forcedClose test above): it can sit in a dial for up to the 30s TLS
	// timeout, and Stop's bounded join is what makes that observable instead
	// of silently outliving shutdown. The single deferred Done below covers
	// every return path of the backoff loop.
	c.loopWG.Add(1)
	old := c.conn
	c.mu.Unlock()
	go c.reconnectLoop(old)
	return true
}

// reconnectLoop re-establishes a dropped IRC connection with the existing
// exponential-backoff/jitter policy. Only beginReconnect may start it.
func (c *IRCClient) reconnectLoop(old net.Conn) {
	defer c.loopWG.Done()

	// Closing the old connection unblocks any in-flight ReadString in the
	// previous readLoop so the stale goroutine exits instead of lingering.
	if old != nil {
		_ = old.Close()
	}

	delay := time.Second
	const maxDelay = 30 * time.Second

	for {
		c.mu.RLock()
		forced := c.forcedClose || !c.lifecycleAuthorized
		c.mu.RUnlock()
		if forced {
			return
		}

		select {
		case <-c.stopChan:
			return
		case <-time.After(withJitter(delay)):
		}

		conn, err := c.dial()
		if err != nil {
			slog.Debug("IRC reconnect dial failed, retrying", "channel", c.channel, "error", err, "delay", delay)
			if delay < maxDelay {
				delay *= 2
			}
			continue
		}

		c.mu.Lock()
		if c.forcedClose || !c.lifecycleAuthorized {
			c.mu.Unlock()
			_ = conn.Close()
			return
		}
		reader := bufio.NewReader(conn)
		c.conn = conn
		c.reader = reader
		c.connEpoch++
		epoch := c.connEpoch
		c.mu.Unlock()

		authGeneration, err := c.authenticateAt(epoch)
		if err != nil {
			slog.Debug("IRC reconnect auth failed, retrying", "channel", c.channel, "error", err)
			_ = conn.Close()
			if delay < maxDelay {
				delay *= 2
			}
			continue
		}
		if err := c.joinAt(epoch); err != nil {
			slog.Debug("IRC reconnect join failed, retrying", "channel", c.channel, "error", err)
			_ = conn.Close()
			if delay < maxDelay {
				delay *= 2
			}
			continue
		}

		c.mu.Lock()
		if c.forcedClose || !c.lifecycleAuthorized || c.connEpoch != epoch {
			// Stop raced the successful redial: never spawn a read loop for a
			// stopped client.
			c.mu.Unlock()
			_ = conn.Close()
			return
		}
		c.reconnecting = false
		c.sessionReady = true
		c.loopWG.Add(1)
		c.mu.Unlock()

		go func() {
			defer c.loopWG.Done()
			c.readLoop(reader, epoch, authGeneration)
		}()
		slog.Info("Reconnected to IRC chat", "channel", c.channel)
		return
	}
}

// withJitter applies ±20% random jitter to a backoff delay, matching the
// human-like timing conventions used elsewhere in the miner.
func withJitter(d time.Duration) time.Duration {
	jitter := (mathrand.Float64() - 0.5) * 0.4 // ±20%
	return time.Duration(float64(d) * (1 + jitter))
}

func (c *IRCClient) handleMessage(line string) {
	c.mu.RLock()
	epoch := c.connEpoch
	authGeneration := c.lastAuthGen
	c.mu.RUnlock()
	c.handleMessageFromEpoch(line, epoch, authGeneration)
}

// handleMessageFromEpoch carries the source transport's authority into
// RECONNECT admission. epoch==0 is the package/test direct-call form and uses
// whichever transport is current when beginReconnect takes c.mu.
func (c *IRCClient) handleMessageFromEpoch(line string, epoch, authGeneration uint64) {
	c.mu.RLock()
	current := !c.forcedClose && c.lifecycleAuthorized && (epoch == 0 || epoch == c.connEpoch)
	c.mu.RUnlock()
	if !current {
		return
	}
	slog.Debug("IRC message received", "channel", c.channel, "line", line)

	if strings.HasPrefix(line, "PING") {
		pongMsg := strings.Replace(line, "PING", "PONG", 1)
		_ = c.sendAt(epoch, pongMsg)
		return
	}

	// The officially documented authentication rejection. Attributed to the
	// generation this connection last presented and reported on a separate
	// goroutine — the read loop never blocks on recovery; the existing
	// backoff reconnect keeps redialing and picks up the rotated token.
	if isLoginAuthFailedNotice(line) {
		slog.Warn("IRC login authentication failed; reporting for credential recovery", "channel", c.channel)
		c.mu.RLock()
		handler := c.authErrorFn
		c.mu.RUnlock()
		if handler != nil {
			go handler(authGeneration)
		}
		return
	}

	// Twitch asks the client to reconnect (":tmi.twitch.tv RECONNECT") before it
	// tears the connection down for maintenance. Honour it proactively.
	if strings.Contains(line, "RECONNECT") {
		slog.Info("IRC reconnect requested by server", "channel", c.channel)
		c.beginReconnect(epoch)
		return
	}

	if strings.Contains(line, "PRIVMSG") {
		c.handlePrivMsg(line, epoch)
	}
}

func (c *IRCClient) handlePrivMsg(line string, epoch uint64) {
	var tags map[string]string
	remaining := line

	if strings.HasPrefix(line, "@") {
		spaceIdx := strings.Index(line, " ")
		if spaceIdx == -1 {
			return
		}
		tags = parseTags(line[1:spaceIdx])
		remaining = line[spaceIdx+1:]
	}

	parts := strings.SplitN(remaining, " ", 4)
	if len(parts) < 4 {
		return
	}

	prefix := parts[0]
	message := strings.TrimPrefix(parts[3], ":")

	nick := ""
	if strings.HasPrefix(prefix, ":") {
		prefix = prefix[1:]
		if idx := strings.Index(prefix, "!"); idx > 0 {
			nick = prefix[:idx]
		}
	}

	if c.logChat {
		displayName := nick
		if dn, ok := tags["display-name"]; ok && dn != "" {
			displayName = dn
		}

		msgData := ChatMessageData{
			Username:    nick,
			DisplayName: displayName,
			Message:     message,
			Emotes:      tags["emotes"],
			Badges:      tags["badges"],
			Color:       tags["color"],
		}

		if err := c.recordChatMessage(msgData, epoch); err != nil {
			slog.Debug("Failed to log chat message", "error", err)
		}
	}

	mention := "@" + strings.ToLower(c.username)
	if strings.Contains(strings.ToLower(message), mention) ||
		strings.Contains(strings.ToLower(message), strings.ToLower(c.username)) {
		slog.Info("Chat mention",
			"channel", c.channel,
			"from", nick,
			"message", message,
		)

		if c.mentionHandler != nil {
			c.mentionHandler(c.streamer.GetUsername(), nick, message)
		}
	}
}

// recordChatMessage admits a structured write only while this exact immutable
// client generation still owns authority. Holding the shared gate's RLock
// through RecordChatMessage is load-bearing: checking before the call and then
// unlocking would let an already-admitted stale write commit after a successful
// settings apply returned.
func (c *IRCClient) recordChatMessage(msg ChatMessageData, epoch uint64) error {
	gate := c.logGate
	if gate == nil {
		return nil
	}

	gate.mu.RLock()
	defer gate.mu.RUnlock()
	c.mu.RLock()
	current := !c.forcedClose && c.lifecycleAuthorized && (epoch == 0 || epoch == c.connEpoch)
	c.mu.RUnlock()
	if !current {
		return nil
	}
	if !c.logAuthorized.Load() || c.logGeneration != gate.generation || gate.logger == nil {
		return nil
	}
	return gate.logger.RecordChatMessage(c.streamer.GetUsername(), msg)
}

func parseTags(tagStr string) map[string]string {
	tags := make(map[string]string)
	for _, tag := range strings.Split(tagStr, ";") {
		parts := strings.SplitN(tag, "=", 2)
		if len(parts) == 2 {
			tags[parts[0]] = parts[1]
		}
	}
	return tags
}

// Stop leaves the channel, closes the connection, and joins the client's
// loops (bounded). On a join timeout it returns the explicit
// errLoopJoinTimeout and proceeds — it never hangs. A repeated Stop is a nil
// no-op.
func (c *IRCClient) Stop() error {
	// Authority is one-way: a stopped/replaced generation can never regain the
	// right to enter the structured sink, including through internal reconnect.
	c.logAuthorized.Store(false)

	c.mu.Lock()
	if c.forcedClose {
		// Idempotent: a second Stop must not re-close the stop channel (a
		// panic before S1) and must not re-join — the first Stop owns the
		// bounded wait.
		c.mu.Unlock()
		return nil
	}
	c.running = false
	c.lifecycleAuthorized = false
	c.sessionReady = false
	c.forcedClose = true
	conn := c.conn
	c.mu.Unlock()

	close(c.stopChan)

	if conn != nil {
		// PART is best-effort courtesy. Without a write deadline this
		// blocking write could wedge Stop before the join even starts,
		// making the bound below meaningless.
		_ = conn.SetWriteDeadline(time.Now().Add(partWriteTimeout))
		_, _ = conn.Write([]byte("PART " + c.channel + "\r\n"))
		_ = conn.Close() // unblocks the read loop's pending ReadString
	}

	// Join the read loop (and any in-flight reconnect) with NO lock held —
	// the loop's message handling may take c.mu — bounded so a wedged
	// chat-log write degrades to a warning instead of hanging shutdown (S1).
	done := make(chan struct{})
	go func() {
		c.loopWG.Wait()
		close(done)
	}()
	var joinErr error
	select {
	case <-done:
	case <-time.After(stopJoinTimeout):
		slog.Warn("IRC client loops did not finish within the stop timeout; proceeding with shutdown",
			"channel", c.channel, "timeout", stopJoinTimeout)
		joinErr = fmt.Errorf("%w (%s, %s)", errLoopJoinTimeout, c.channel, stopJoinTimeout)
	}

	slog.Info("Left IRC chat", "channel", c.channel)
	return joinErr
}

func (c *IRCClient) IsRunning() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.running
}
