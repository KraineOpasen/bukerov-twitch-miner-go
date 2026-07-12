package pubsub

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	mathrand "math/rand"
	"sync"
	"time"

	"github.com/PatrickWalther/twitch-miner-go/internal/constants"
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

	onMessage func(*PubSubMessage)
	onError   func(error)

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
	ws.mu.Lock()
	ws.isReconnecting = false
	ws.isClosed = false
	ws.mu.Unlock()

	dialer := websocket.Dialer{
		HandshakeTimeout: 30 * time.Second,
	}

	conn, _, err := dialer.Dial(constants.PubSubURL, nil)
	if err != nil {
		return err
	}

	ws.mu.Lock()
	ws.conn = conn
	ws.isOpened = true
	ws.lastPong = time.Now()
	ws.mu.Unlock()

	for _, topic := range ws.pendingTopics {
		ws.Listen(topic)
	}
	ws.pendingTopics = nil

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

func (ws *WebSocketClient) TopicCount() int {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	return len(ws.topics)
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
	ws.mu.Unlock()

	if ws.conn != nil {
		_ = ws.conn.Close()
	}

	slog.Info("Reconnecting WebSocket in 60 seconds", "index", ws.index)
	time.Sleep(60 * time.Second)

	ws.mu.RLock()
	forcedClose := ws.forcedClose
	topics := make([]Topic, len(ws.topics))
	copy(topics, ws.topics)
	ws.mu.RUnlock()

	if forcedClose {
		return
	}

	ws.mu.Lock()
	ws.stopChan = make(chan struct{})
	ws.pendingTopics = topics
	ws.topics = nil
	ws.mu.Unlock()

	if err := ws.Connect(); err != nil {
		slog.Error("Failed to reconnect", "index", ws.index, "error", err)
		go ws.reconnect()
	}
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
