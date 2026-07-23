package pubsub

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	mathrand "math/rand"
	"sync"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/gorilla/websocket"
)

type WebSocketClient struct {
	index          int
	conn           *websocket.Conn
	topics         []Topic
	pendingTopics  []Topic
	authToken      string
	pingInterval   int
	reconnectDelay int

	// url is the WebSocket endpoint to dial. Defaults to constants.PubSubURL;
	// overridable so tests can point the client at a local server, and a seam
	// for a future transport abstraction (PubSub -> Hermes).
	url string

	// delayUnit scales reconnectDelay (seconds in production). A test seam in
	// the spirit of drops.intervalUnit, so reconnect pacing is observable in
	// milliseconds instead of real seconds.
	delayUnit time.Duration

	isOpened       bool
	isClosed       bool
	isReconnecting bool
	forcedClose    bool

	lastPong time.Time
	lastPing time.Time
	// lastConnectedAt is when the current connection was established. The
	// read-error reconnect path uses it as an anti-flap guard: only a link
	// that had been up for at least one reconnectDelay earns an immediate
	// redial (see readErrorReconnectDelay).
	lastConnectedAt time.Time
	lastMsgTime     time.Time
	lastMsgID       string

	onMessage   func(*PubSubMessage)
	onError     func(error)
	onReconnect func()

	mu       sync.RWMutex
	writeMu  sync.Mutex
	stopChan chan struct{}
}

func NewWebSocketClient(index int, authToken string, pingInterval int, reconnectDelay int, onMessage func(*PubSubMessage), onError func(error)) *WebSocketClient {
	return &WebSocketClient{
		index:          index,
		authToken:      authToken,
		pingInterval:   pingInterval,
		reconnectDelay: reconnectDelay,
		url:            constants.PubSubURL,
		delayUnit:      time.Second,
		onMessage:      onMessage,
		onError:        onError,
		stopChan:       make(chan struct{}),
		topics:         make([]Topic, 0),
		pendingTopics:  make([]Topic, 0),
	}
}

func (ws *WebSocketClient) Connect() error {
	dialer := websocket.Dialer{
		HandshakeTimeout: 30 * time.Second,
	}

	// isReconnecting/isClosed are deliberately NOT cleared here: on the reconnect
	// path they must stay set for the entire Dial (up to HandshakeTimeout) so the
	// reconnect guard and the ping-loop watchdog cannot spawn a second, racing
	// reconnect while this one is still dialing. They are cleared only once the
	// connection is actually established, below.
	conn, _, err := dialer.Dial(ws.url, nil)
	if err != nil {
		return err
	}

	ws.mu.Lock()
	ws.conn = conn
	ws.isOpened = true
	ws.isReconnecting = false
	ws.isClosed = false
	ws.lastPong = time.Now()
	ws.lastConnectedAt = time.Now()
	resubscribed := len(ws.pendingTopics)
	// The loops below get THIS generation's stop channel and conn as
	// parameters, snapshotted under mu. Selecting on the ws.stopChan field
	// would race with reconnect() replacing it — and worse, an old loop
	// iterating after the swap would silently adopt the NEW channel/conn,
	// leaving it unstoppable (the leaked-pingLoop bug) or reading the new
	// conn concurrently with its own reader.
	stop := ws.stopChan
	ws.mu.Unlock()

	// Drain the parked set one topic at a time, re-checking pendingTopics
	// membership under the lock per topic. Replaying a pre-unlock snapshot
	// instead would resurrect a topic a concurrent Unlisten (runtime capability
	// disable) had just removed from the field.
	ws.replayPendingTopics()

	slog.Info("WebSocket connected", "index", ws.index, "resubscribed", resubscribed)

	go ws.readLoop(stop, conn)
	go ws.pingLoop(stop)

	return nil
}

func (ws *WebSocketClient) Close() {
	ws.mu.Lock()
	// Idempotent: a second Close must not re-close the stop channel.
	if ws.forcedClose {
		ws.mu.Unlock()
		return
	}
	ws.forcedClose = true
	ws.isClosed = true
	// Closing the CURRENT stop channel under mu is what makes this safe
	// against a concurrent reconnect: reconnect's close-and-replace runs
	// under the same lock and checks forcedClose first, so each channel
	// value is closed exactly once.
	close(ws.stopChan)
	conn := ws.conn
	ws.mu.Unlock()

	if conn != nil {
		_ = conn.Close()
	}
}

// LastPong returns the last time this connection received a PONG, i.e. the
// last confirmed sign of life. Used by the connection-health watchdog.
func (ws *WebSocketClient) LastPong() time.Time {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	return ws.lastPong
}

func (ws *WebSocketClient) IsClosed() bool {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	return ws.isClosed
}

// SetReconnectHandler registers a callback fired once each time this connection
// begins a reconnect. It is wired once (before Connect) by the pool so it can
// track reconnect churn. The handler is captured under ws.mu and invoked with
// the lock released, so it may safely acquire other locks.
func (ws *WebSocketClient) SetReconnectHandler(h func()) {
	ws.mu.Lock()
	ws.onReconnect = h
	ws.mu.Unlock()
}

// TopicCount reports how many topics this connection is responsible for,
// counting parked pendingTopics too: during a reconnect the live set sits in
// pendingTopics, and ignoring it would let the pool pack new topics onto an
// effectively full connection past MaxTopicsPerConnection.
func (ws *WebSocketClient) TopicCount() int {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	return len(ws.topics) + len(ws.pendingTopics)
}

// state returns an atomic read-only view of this connection for the pool's
// per-index health/debug snapshot. Taken under a single RLock so the fields are
// mutually consistent (e.g. Topics and Reconnecting cannot straddle a reconnect).
func (ws *WebSocketClient) state() ConnState {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	return ConnState{
		Index:        ws.index,
		Topics:       len(ws.topics),
		LastPong:     ws.lastPong,
		Reconnecting: ws.isReconnecting,
		Closed:       ws.isClosed,
	}
}

func (ws *WebSocketClient) Listen(topic Topic) {
	ws.mu.Lock()
	for _, t := range ws.topics {
		if t.String() == topic.String() {
			ws.mu.Unlock()
			return
		}
	}

	if !ws.isOpened {
		// Not connected (initial dial pending, or a reconnect swapped the
		// generation): PARK the topic only — the post-connect replay sends its
		// LISTEN. Appending to the live set and writing here would push the
		// frame into a dead conn, silently stranding the topic as
		// tracked-but-never-subscribed (which a desired-state sweep could then
		// never heal, since HasTopic would claim ownership).
		for _, t := range ws.pendingTopics {
			if t.String() == topic.String() {
				ws.mu.Unlock()
				return
			}
		}
		ws.pendingTopics = append(ws.pendingTopics, topic)
		ws.mu.Unlock()
		return
	}
	ws.topics = append(ws.topics, topic)
	ws.mu.Unlock()

	ws.sendListen(topic)
}

// sendListen writes one LISTEN frame for the topic on the current connection.
func (ws *WebSocketClient) sendListen(topic Topic) {
	data := &WSData{
		Topics: []string{topic.String()},
	}
	if topic.IsUserTopic() {
		data.AuthToken = ws.authToken
	}

	msg := WSMessage{
		Type:  "LISTEN",
		Nonce: generateNonce(),
		Data:  data,
	}

	_ = ws.send(msg)
}

// replayPendingTopics promotes parked topics onto the live connection one at a
// time. Each iteration pops the head and promotes it into ws.topics under a
// SINGLE lock hold, so (a) a topic a concurrent Unlisten already removed from
// pendingTopics is never replayed, and (b) from the instant of promotion a
// later Unlisten finds the topic in the live set and issues its own UNLISTEN.
func (ws *WebSocketClient) replayPendingTopics() {
	for {
		ws.mu.Lock()
		if !ws.isOpened || len(ws.pendingTopics) == 0 {
			ws.mu.Unlock()
			return
		}
		topic := ws.pendingTopics[0]
		ws.pendingTopics = ws.pendingTopics[1:]
		ws.topics = append(ws.topics, topic)
		ws.mu.Unlock()

		ws.sendListen(topic)
	}
}

func (ws *WebSocketClient) Unlisten(topic Topic) bool {
	ws.mu.Lock()
	found := false
	var remaining []Topic
	for _, t := range ws.topics {
		if t.String() == topic.String() {
			found = true
		} else {
			remaining = append(remaining, t)
		}
	}
	ws.topics = remaining

	var remainingPending []Topic
	for _, t := range ws.pendingTopics {
		if t.String() != topic.String() {
			remainingPending = append(remainingPending, t)
		}
	}
	ws.pendingTopics = remainingPending

	isOpened := ws.isOpened
	ws.mu.Unlock()

	if found && isOpened {
		data := &WSData{
			Topics: []string{topic.String()},
		}
		if topic.IsUserTopic() {
			data.AuthToken = ws.authToken
		}

		msg := WSMessage{
			Type:  "UNLISTEN",
			Nonce: generateNonce(),
			Data:  data,
		}

		_ = ws.send(msg)
	}

	return found
}

// HasTopic reports whether this connection owns the topic, counting topics
// parked in pendingTopics (a reconnect in flight moves the live set there) so
// a pool-wide dedup scan cannot double-subscribe a topic mid-reconnect.
func (ws *WebSocketClient) HasTopic(topic Topic) bool {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	for _, t := range ws.topics {
		if t.String() == topic.String() {
			return true
		}
	}
	for _, t := range ws.pendingTopics {
		if t.String() == topic.String() {
			return true
		}
	}
	return false
}

func (ws *WebSocketClient) send(msg WSMessage) error {
	ws.writeMu.Lock()
	defer ws.writeMu.Unlock()

	// Snapshot the conn under mu: Connect/reconnect swap it under mu, and
	// writeMu alone does not order this read against that write.
	ws.mu.RLock()
	conn := ws.conn
	ws.mu.RUnlock()

	if conn == nil {
		return nil
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	slog.Debug("WebSocket send", "index", ws.index, "type", msg.Type)
	return conn.WriteMessage(websocket.TextMessage, data)
}

func (ws *WebSocketClient) ping() {
	msg := WSMessage{Type: "PING"}
	_ = ws.send(msg)

	ws.mu.Lock()
	ws.lastPing = time.Now()
	ws.mu.Unlock()
}

// readLoop reads frames from ONE connection generation: stop and conn are
// snapshotted by Connect under mu, so a loop outliving a reconnect can never
// adopt the replacement channel/conn from the fields (which would leave it
// unstoppable, or reading the new conn concurrently with its own reader).
func (ws *WebSocketClient) readLoop(stop chan struct{}, conn *websocket.Conn) {
	for {
		select {
		case <-stop:
			return
		default:
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			// Read errors on a gorilla connection are permanent (and repeated
			// reads on a failed connection eventually panic), so this loop
			// always exits here — exactly once. Classification is by our own
			// state flags, never by the error's contents: transport failures
			// aren't CloseErrors, close-code matching is toolchain-fragile,
			// and both deliberate paths already flag themselves before
			// closing the conn (Close sets forcedClose; reconnect sets
			// isReconnecting under mu before Close on the old conn).
			ws.mu.RLock()
			forcedClose := ws.forcedClose
			reconnecting := ws.isReconnecting
			delay := ws.readErrorReconnectDelayLocked()
			ws.mu.RUnlock()

			switch {
			case forcedClose:
				// Deliberate shutdown: silent, no reconnect.
			case reconnecting:
				// Our own reconnect closed the conn under this reader — an
				// expected artifact, not an error, and the reconnect is
				// already in flight.
				slog.Debug("WebSocket read loop ended during reconnect", "index", ws.index)
			default:
				// Unexpected death (reset by peer, server close frame such as
				// 4100, abnormal EOF): without this trigger the connection
				// stayed deaf until the 5-minute PONG watchdog noticed.
				slog.Error("WebSocket read error; reconnecting", "index", ws.index, "error", err, "delay", delay)
				if ws.onError != nil {
					ws.onError(err)
				}
				go ws.reconnectAfter(delay)
			}
			return
		}

		var wsMsg WSMessage
		if err := json.Unmarshal(message, &wsMsg); err != nil {
			slog.Error("Failed to parse WebSocket message", "error", err)
			continue
		}

		ws.handleMessage(wsMsg)
	}
}

func (ws *WebSocketClient) handleMessage(msg WSMessage) {
	slog.Debug("WebSocket received", "index", ws.index, "type", msg.Type)

	switch msg.Type {
	case "PONG":
		ws.mu.Lock()
		ws.lastPong = time.Now()
		ws.mu.Unlock()

	case "MESSAGE":
		if msg.Data == nil {
			return
		}

		pubsubMsg, err := ParsePubSubMessage(msg.Data)
		if err != nil {
			slog.Error("Failed to parse PubSub message", "error", err)
			return
		}

		msgID := pubsubMsg.Type + "." + pubsubMsg.Topic.String() + "." + pubsubMsg.ChannelID

		ws.mu.Lock()
		if ws.lastMsgID == msgID && time.Since(ws.lastMsgTime) < time.Second {
			ws.mu.Unlock()
			return
		}
		ws.lastMsgID = msgID
		ws.lastMsgTime = time.Now()
		ws.mu.Unlock()

		if ws.onMessage != nil {
			ws.onMessage(pubsubMsg)
		}

	case "RESPONSE":
		if msg.Error != "" {
			slog.Error("WebSocket response error", "index", ws.index, "error", msg.Error)
			if ws.onError != nil && msg.Error == "ERR_BADAUTH" {
				ws.onError(ErrBadAuth)
			}
		}

	case "RECONNECT":
		slog.Info("WebSocket reconnect requested", "index", ws.index)
		go ws.reconnect()
	}
}

func (ws *WebSocketClient) randomPingInterval() time.Duration {
	base := float64(ws.pingInterval)
	jitter := (mathrand.Float64() - 0.5) * 5.0
	return time.Duration(base+jitter) * time.Second
}

// pingLoop drives PINGs and the PONG watchdog for ONE connection generation
// (see readLoop on why stop is a parameter, not the field).
func (ws *WebSocketClient) pingLoop(stop chan struct{}) {
	checkTicker := time.NewTicker(time.Minute)
	defer checkTicker.Stop()

	for {
		pingWait := ws.randomPingInterval()

		select {
		case <-stop:
			return
		case <-time.After(pingWait):
			ws.mu.RLock()
			isReconnecting := ws.isReconnecting
			ws.mu.RUnlock()

			if !isReconnecting {
				ws.ping()
			}
		case <-checkTicker.C:
			ws.mu.RLock()
			elapsed := time.Since(ws.lastPong)
			isReconnecting := ws.isReconnecting
			ws.mu.RUnlock()

			if !isReconnecting && elapsed > 5*time.Minute {
				slog.Warn("No PONG received for 5 minutes, reconnecting", "index", ws.index)
				go ws.reconnect()
			}
		}
	}
}

// configuredReconnectDelay is the operator-configured pause before a redial
// (ReconnectDelay seconds in production; delayUnit is a test seam).
func (ws *WebSocketClient) configuredReconnectDelay() time.Duration {
	return time.Duration(ws.reconnectDelay) * ws.delayUnit
}

// readErrorReconnectDelayLocked picks the redial pause for the unexpected
// read-error path. A connection that had been up for at least one configured
// reconnectDelay earns an immediate first dial — that is what closes the
// multi-minute deaf window between a socket dying and the PONG watchdog
// noticing. A connection that dies right after connecting is flapping and
// waits the full configured delay, so "dial succeeds -> dies instantly"
// cannot hot-loop at ~1s. This is an anti-flap guard, not a backoff: the
// self-retry pacing below is unchanged. Caller must hold ws.mu (read or
// write).
func (ws *WebSocketClient) readErrorReconnectDelayLocked() time.Duration {
	delay := ws.configuredReconnectDelay()
	if time.Since(ws.lastConnectedAt) >= delay {
		return 0
	}
	return delay
}

// reconnect re-establishes the connection after the operator-configured
// delay. Used by the server RECONNECT frame, the PONG watchdog, and the
// failed-dial self-retry; the unexpected read-error path calls
// reconnectAfter directly with the anti-flap delay.
func (ws *WebSocketClient) reconnect() {
	ws.reconnectAfter(ws.configuredReconnectDelay())
}

func (ws *WebSocketClient) reconnectAfter(delay time.Duration) {
	ws.mu.Lock()
	if ws.isReconnecting || ws.forcedClose {
		ws.mu.Unlock()
		return
	}
	ws.isReconnecting = true
	ws.isClosed = true
	onReconnect := ws.onReconnect
	conn := ws.conn
	ws.mu.Unlock()

	// Report the reconnect (lock released) so the pool can count churn. Fired
	// once per reconnect entry, including the self-retry path on a failed dial —
	// that is intentional: repeated retries are exactly the "flapping" the
	// degraded signal is meant to catch.
	if onReconnect != nil {
		onReconnect()
	}

	if conn != nil {
		_ = conn.Close()
	}

	slog.Info("Reconnecting WebSocket", "index", ws.index, "delay", delay)
	time.Sleep(delay)

	ws.mu.Lock()
	if ws.forcedClose {
		ws.mu.Unlock()
		return
	}
	// The set to resubscribe is the union of the currently-live topics and
	// anything already parked in pendingTopics. The pendingTopics term is what
	// makes this safe on a retry: when a previous Connect() failed at Dial it
	// left topics=nil with the real set stranded in pendingTopics, so snapshotting
	// topics alone (as the old code did) would clobber it with an empty slice and
	// silently drop every subscription. Union never loses a parked topic.
	restore := mergeTopics(ws.topics, ws.pendingTopics)
	// Release the previous generation's loops BEFORE handing out a fresh stop
	// channel. Without this close the old pingLoop lived forever (its next
	// select silently adopted the replacement channel), so every reconnect
	// leaked one more pingLoop — multiplying PINGs on the new connection and
	// pushing it toward Twitch's 4100 "ping pong failed" close. Closing here
	// is single-close-safe: Close() and this block both run under mu, and
	// this block never runs once forcedClose is set (checked above).
	close(ws.stopChan)
	ws.stopChan = make(chan struct{})
	ws.pendingTopics = restore
	ws.topics = nil
	// The connection is no longer usable from this point: a Listen arriving
	// during the redial must PARK its topic (picked up by the post-connect
	// replay) instead of writing a LISTEN frame into the dead old conn and
	// stranding the topic as tracked-but-never-subscribed.
	ws.isOpened = false
	ws.mu.Unlock()

	if err := ws.Connect(); err != nil {
		slog.Error("Failed to reconnect", "index", ws.index, "error", err)
		// Connect() clears isReconnecting only on success, so reopen the guard
		// here before retrying — otherwise the retry would be swallowed by the
		// isReconnecting check at the top of reconnect() and the connection would
		// be stranded forever. The topics stay safe in pendingTopics across the
		// retry (see mergeTopics above).
		ws.mu.Lock()
		ws.isReconnecting = false
		ws.mu.Unlock()
		go ws.reconnect()
	}
}

// mergeTopics returns the union of two topic slices, de-duplicated by their
// wire string (Topic.String()). Order is deterministic: every topic from a in
// order, then any topic from b not already present.
func mergeTopics(a, b []Topic) []Topic {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]Topic, 0, len(a)+len(b))
	for _, src := range [][]Topic{a, b} {
		for _, t := range src {
			key := t.String()
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, t)
		}
	}
	return out
}

func generateNonce() string {
	b := make([]byte, 15)
	if _, err := rand.Read(b); err != nil {
		return "000000000000000000000000000000"
	}
	return hex.EncodeToString(b)
}

var ErrBadAuth = &AuthError{Message: "ERR_BADAUTH"}

type AuthError struct {
	Message string
}

func (e *AuthError) Error() string {
	return e.Message
}
