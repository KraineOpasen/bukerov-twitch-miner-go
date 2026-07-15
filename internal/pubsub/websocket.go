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
	index         int
	conn          *websocket.Conn
	topics        []Topic
	pendingTopics []Topic
	authToken     string
	pingInterval  int

	isOpened       bool
	isClosed       bool
	isReconnecting bool
	forcedClose    bool

	lastPong    time.Time
	lastPing    time.Time
	lastMsgTime time.Time
	lastMsgID   string

	onMessage   func(*PubSubMessage)
	onError     func(error)
	onReconnect func()

	mu       sync.RWMutex
	writeMu  sync.Mutex
	stopChan chan struct{}
}

func NewWebSocketClient(index int, authToken string, pingInterval int, onMessage func(*PubSubMessage), onError func(error)) *WebSocketClient {
	return &WebSocketClient{
		index:         index,
		authToken:     authToken,
		pingInterval:  pingInterval,
		onMessage:     onMessage,
		onError:       onError,
		stopChan:      make(chan struct{}),
		topics:        make([]Topic, 0),
		pendingTopics: make([]Topic, 0),
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
	conn, _, err := dialer.Dial(constants.PubSubURL, nil)
	if err != nil {
		return err
	}

	ws.mu.Lock()
	ws.conn = conn
	ws.isOpened = true
	ws.isReconnecting = false
	ws.isClosed = false
	ws.lastPong = time.Now()
	pending := ws.pendingTopics
	ws.mu.Unlock()

	// Resubscribe outside the lock (Listen takes ws.mu itself). isOpened is now
	// true, so each Listen sends its LISTEN frame immediately rather than
	// re-parking the topic in pendingTopics.
	for _, topic := range pending {
		ws.Listen(topic)
	}

	ws.mu.Lock()
	ws.pendingTopics = nil
	ws.mu.Unlock()

	slog.Info("WebSocket connected", "index", ws.index, "resubscribed", len(pending))

	go ws.readLoop()
	go ws.pingLoop()

	return nil
}

func (ws *WebSocketClient) Close() {
	ws.mu.Lock()
	ws.forcedClose = true
	ws.isClosed = true
	ws.mu.Unlock()

	close(ws.stopChan)
	if ws.conn != nil {
		_ = ws.conn.Close()
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

func (ws *WebSocketClient) TopicCount() int {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	return len(ws.topics)
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
	ws.topics = append(ws.topics, topic)

	if !ws.isOpened {
		ws.pendingTopics = append(ws.pendingTopics, topic)
		ws.mu.Unlock()
		return
	}
	ws.mu.Unlock()

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

func (ws *WebSocketClient) HasTopic(topic Topic) bool {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	for _, t := range ws.topics {
		if t.String() == topic.String() {
			return true
		}
	}
	return false
}

func (ws *WebSocketClient) send(msg WSMessage) error {
	ws.writeMu.Lock()
	defer ws.writeMu.Unlock()

	if ws.conn == nil {
		return nil
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	slog.Debug("WebSocket send", "index", ws.index, "type", msg.Type)
	return ws.conn.WriteMessage(websocket.TextMessage, data)
}

func (ws *WebSocketClient) ping() {
	msg := WSMessage{Type: "PING"}
	_ = ws.send(msg)

	ws.mu.Lock()
	ws.lastPing = time.Now()
	ws.mu.Unlock()
}

func (ws *WebSocketClient) readLoop() {
	for {
		select {
		case <-ws.stopChan:
			return
		default:
		}

		ws.mu.RLock()
		conn := ws.conn
		ws.mu.RUnlock()

		if conn == nil {
			return
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			ws.mu.RLock()
			forcedClose := ws.forcedClose
			ws.mu.RUnlock()

			if !forcedClose {
				slog.Error("WebSocket read error", "index", ws.index, "error", err)
				if ws.onError != nil {
					ws.onError(err)
				}
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

func (ws *WebSocketClient) pingLoop() {
	checkTicker := time.NewTicker(time.Minute)
	defer checkTicker.Stop()

	for {
		pingWait := ws.randomPingInterval()

		select {
		case <-ws.stopChan:
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

func (ws *WebSocketClient) reconnect() {
	ws.mu.Lock()
	if ws.isReconnecting || ws.forcedClose {
		ws.mu.Unlock()
		return
	}
	ws.isReconnecting = true
	ws.isClosed = true
	onReconnect := ws.onReconnect
	ws.mu.Unlock()

	// Report the reconnect (lock released) so the pool can count churn. Fired
	// once per reconnect entry, including the self-retry path on a failed dial —
	// that is intentional: repeated retries are exactly the "flapping" the
	// degraded signal is meant to catch.
	if onReconnect != nil {
		onReconnect()
	}

	if ws.conn != nil {
		_ = ws.conn.Close()
	}

	slog.Info("Reconnecting WebSocket in 60 seconds", "index", ws.index)
	time.Sleep(60 * time.Second)

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
	ws.stopChan = make(chan struct{})
	ws.pendingTopics = restore
	ws.topics = nil
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
