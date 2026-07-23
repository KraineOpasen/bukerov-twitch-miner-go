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
	index int
	conn  *websocket.Conn

	// Per-topic wire ledger. The three buckets are disjoint per topic:
	//   - topics: the LISTEN frame was successfully written on the CURRENT
	//     connection generation (wire-applied).
	//   - pendingTopics: desired here, but no LISTEN has succeeded on the
	//     current generation (parked during dial/reconnect, or the last write
	//     failed and must be retried).
	//   - unlistenRetry: NOT desired, but the UNLISTEN write failed on the
	//     current generation — a debt that must be re-sent before the local
	//     absent state can be trusted against the wire.
	// All transitions run under writeMu (see Listen/Unlisten), which is what
	// linearizes LISTEN/UNLISTEN wire order per topic.
	topics        []Topic
	pendingTopics []Topic
	unlistenRetry []Topic
	// connGen counts connection generations: bumped on every reconnect swap
	// and every successful Connect. A topic write whose captured generation no
	// longer matches commits nothing — the old socket (and everything written
	// to it) is void, and the parked set carried by the swap owns the topic.
	connGen uint64

	// writeTopicFrameHook, when set (tests only, before any concurrency),
	// replaces the real LISTEN/UNLISTEN frame write so wire order can be
	// recorded and write failures injected deterministically.
	writeTopicFrameHook func(frameType string, topic Topic) error

	// tokenFn supplies the CURRENT auth snapshot at every user-topic frame
	// write, so a LISTEN after a token rotation (including one written by a
	// reconnect replay) always carries the rotated token instead of a
	// startup-time copy. Set once at construction, immutable afterwards.
	tokenFn AuthTokenProvider
	// lastAuthGen is the credential generation whose token was last written in
	// a user-topic LISTEN on this connection; userFrameGens attributes each
	// outstanding user-topic frame's nonce to the exact generation it
	// presented, so a queued ERR_BADAUTH for an OLD frame arriving after a
	// rotation sweep is never misattributed to the new credentials (which
	// would trigger a spurious refresh). Both guarded by ws.mu; the map is
	// reset on every reconnect (the old socket's frames are void).
	lastAuthGen    uint64
	userFrameGens  map[string]uint64
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

func NewWebSocketClient(index int, tokenFn AuthTokenProvider, pingInterval int, reconnectDelay int, onMessage func(*PubSubMessage), onError func(error)) *WebSocketClient {
	return &WebSocketClient{
		index:          index,
		tokenFn:        tokenFn,
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
	// A fresh usable generation: writes captured against an older generation
	// commit nothing (see connGen), and the old socket's outstanding frame
	// attributions are void with it.
	ws.connGen++
	ws.userFrameGens = nil
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
// counting parked pendingTopics (during a reconnect the live set sits there)
// and unlistenRetry debts (still occupying a wire slot on Twitch's side until
// the UNLISTEN lands): ignoring either would let the pool pack new topics
// onto an effectively full connection past MaxTopicsPerConnection.
func (ws *WebSocketClient) TopicCount() int {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	return len(ws.topics) + len(ws.pendingTopics) + len(ws.unlistenRetry)
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

// topicIn reports whether the list contains the topic (by wire string).
func topicIn(list []Topic, topic Topic) bool {
	for _, t := range list {
		if t.String() == topic.String() {
			return true
		}
	}
	return false
}

// topicRemove returns the list without the topic.
func topicRemove(list []Topic, topic Topic) []Topic {
	var out []Topic
	for _, t := range list {
		if t.String() != topic.String() {
			out = append(out, t)
		}
	}
	return out
}

// topicAppendUnique appends the topic unless already present.
func topicAppendUnique(list []Topic, topic Topic) []Topic {
	if topicIn(list, topic) {
		return list
	}
	return append(list, topic)
}

// Listen reconciles ONE topic to desired=true on this connection. The whole
// read-decide-write-commit sequence runs under writeMu — the same lock every
// topic frame write holds — so per-topic LISTEN/UNLISTEN transitions are
// linearized against the wire: a stale LISTEN can never be the final frame
// after a newer desired=false (the disable serializes behind the in-flight
// write and follows with its compensating UNLISTEN), and vice versa.
//
// The write error, if any, is returned; on error the topic stays in
// pendingTopics (desired, retryable by an identical EnsureTopic or the next
// reconnect replay) and is never falsely marked wire-applied.
// AuthSnapshot is the minimal credential view a PubSub connection needs to
// sign user-topic LISTEN frames: the current access token and the generation
// it belongs to. It never carries the refresh token.
type AuthSnapshot struct {
	Token      string
	Generation uint64
}

// AuthTokenProvider supplies the current AuthSnapshot at frame-write time. In
// production the miner adapts auth.TwitchAuth.Snapshot; tests may pass nil
// (empty snapshot) or a static provider.
type AuthTokenProvider func() AuthSnapshot

// StaticAuthToken wraps a fixed token as a provider, for tests and library use
// where no rotation exists.
func StaticAuthToken(token string) AuthTokenProvider {
	return func() AuthSnapshot { return AuthSnapshot{Token: token} }
}

// authSnapshot resolves the provider (nil-safe).
func (ws *WebSocketClient) authSnapshot() AuthSnapshot {
	if ws.tokenFn == nil {
		return AuthSnapshot{}
	}
	return ws.tokenFn()
}

// RelistenUserTopics re-sends a LISTEN for every user topic currently
// wire-applied on this connection, carrying the CURRENT auth snapshot. It is
// the bounded post-rotation sweep: a Twitch LISTEN for an already-subscribed
// topic is idempotent, so this re-authorizes the subscription without tearing
// the connection or touching the topic ledger. Runs under writeMu (the same
// linearization as Listen/Unlisten; lock order writeMu -> ws.mu); a mid-sweep
// reconnect voids the remaining writes — the reconnect replay re-LISTENs the
// parked topics with the current token anyway.
func (ws *WebSocketClient) RelistenUserTopics() {
	ws.writeMu.Lock()
	defer ws.writeMu.Unlock()

	ws.mu.Lock()
	if !ws.isOpened {
		ws.mu.Unlock()
		return
	}
	conn := ws.conn
	gen := ws.connGen
	var userTopics []Topic
	for _, t := range ws.topics {
		if t.IsUserTopic() {
			userTopics = append(userTopics, t)
		}
	}
	ws.mu.Unlock()

	for _, t := range userTopics {
		ws.mu.RLock()
		stillCurrent := gen == ws.connGen
		ws.mu.RUnlock()
		if !stillCurrent {
			return
		}
		if err := ws.writeTopicFrame(conn, "LISTEN", t); err != nil {
			// Broken socket: the read-loop reconnect owns recovery, and its
			// replay re-LISTENs with the current token. Privacy-safe log only.
			slog.Warn("Re-authorizing user topic after token rotation failed; reconnect replay will retry", "index", ws.index, "error", err)
			return
		}
	}
}

func (ws *WebSocketClient) Listen(topic Topic) error {
	ws.writeMu.Lock()
	defer ws.writeMu.Unlock()
	return ws.listenUnderWriteLock(topic)
}

// listenUnderWriteLock is Listen's body; caller must hold writeMu (never
// ws.mu).
func (ws *WebSocketClient) listenUnderWriteLock(topic Topic) error {
	ws.mu.Lock()
	if topicIn(ws.topics, topic) {
		ws.mu.Unlock()
		return nil // already wire-applied on the current generation
	}
	if !ws.isOpened {
		// Not connected (initial dial pending, or a reconnect swapped the
		// generation): PARK the topic only — the post-connect replay owns the
		// write. Writing here would push the frame into a dead conn and strand
		// the topic as tracked-but-never-subscribed.
		ws.pendingTopics = topicAppendUnique(ws.pendingTopics, topic)
		ws.mu.Unlock()
		return nil
	}
	// The topic stays parked in pendingTopics ACROSS the write: if a reconnect
	// swaps the generation mid-write, the swap's merge preserves it and the
	// next replay re-sends it — it can never be lost in limbo.
	ws.pendingTopics = topicAppendUnique(ws.pendingTopics, topic)
	conn := ws.conn
	gen := ws.connGen
	ws.mu.Unlock()

	err := ws.writeTopicFrame(conn, "LISTEN", topic)

	ws.mu.Lock()
	defer ws.mu.Unlock()
	if gen != ws.connGen {
		// The connection died and was swapped while writing: whatever reached
		// the old socket is void. The topic is still parked (the swap's merge
		// kept it); the new generation's replay owns it. Not an error — the
		// retry obligation is already recorded in pendingTopics.
		return nil
	}
	if err != nil {
		// Stays in pendingTopics: desired, NOT applied, retryable.
		return err
	}
	ws.pendingTopics = topicRemove(ws.pendingTopics, topic)
	ws.topics = topicAppendUnique(ws.topics, topic)
	// A successful LISTEN settles any failed-UNLISTEN debt: the last confirmed
	// wire command now matches desired=true.
	ws.unlistenRetry = topicRemove(ws.unlistenRetry, topic)
	return nil
}

// Unlisten reconciles ONE topic to desired=false on this connection, under
// the same writeMu linearization as Listen (see there). The UNLISTEN write
// error, if any, is returned; on error the topic is held in unlistenRetry so
// an identical EnsureTopic(desired=false) re-sends the frame instead of
// no-opping — the local absent state never silently outruns a known-failed
// wire operation.
func (ws *WebSocketClient) Unlisten(topic Topic) error {
	ws.writeMu.Lock()
	defer ws.writeMu.Unlock()

	ws.mu.Lock()
	// Parked-but-never-confirmed: no LISTEN succeeded on this generation, so
	// dropping the desire is the whole transition — no compensating frame. (A
	// FAILED earlier LISTEN also lands here: its socket takes no more frames
	// and dies to a reconnect, which replays desired topics only.)
	ws.pendingTopics = topicRemove(ws.pendingTopics, topic)

	needWrite := false
	if topicIn(ws.topics, topic) {
		ws.topics = topicRemove(ws.topics, topic)
		needWrite = true
	} else if topicIn(ws.unlistenRetry, topic) {
		needWrite = true // previous UNLISTEN write failed — retry the frame
	}
	if !needWrite {
		ws.mu.Unlock()
		return nil
	}
	if !ws.isOpened {
		// Generation already swapped: the old socket and its subscriptions are
		// dead, so there is nothing left to compensate on the wire.
		ws.unlistenRetry = topicRemove(ws.unlistenRetry, topic)
		ws.mu.Unlock()
		return nil
	}
	// Record the debt BEFORE the write, so no interleaving can leave a
	// confirmed LISTEN as the final command without a retry obligation.
	ws.unlistenRetry = topicAppendUnique(ws.unlistenRetry, topic)
	conn := ws.conn
	gen := ws.connGen
	ws.mu.Unlock()

	err := ws.writeTopicFrame(conn, "UNLISTEN", topic)

	ws.mu.Lock()
	defer ws.mu.Unlock()
	if gen != ws.connGen {
		// Old socket died mid-write; its wire subscriptions die with it and
		// the swap cleared the debt ledger.
		return nil
	}
	if err != nil {
		// Debt stays in unlistenRetry: local state is absent AND the failed
		// UNLISTEN is explicitly recorded for the next identical apply.
		return err
	}
	ws.unlistenRetry = topicRemove(ws.unlistenRetry, topic)
	return nil
}

// maxTrackedAuthNonces bounds the per-connection nonce->generation attribution
// ledger; beyond it the oldest information is discarded wholesale (attribution
// then falls back to lastAuthGen). User-topic frames are rare, so this is a
// defensive cap, not a working limit.
const maxTrackedAuthNonces = 128

// writeTopicFrame writes one LISTEN/UNLISTEN frame for the topic. Caller must
// hold writeMu and must NOT hold ws.mu. Only the error ever surfaces to
// callers/logs — never the frame, token, or payload.
func (ws *WebSocketClient) writeTopicFrame(conn *websocket.Conn, frameType string, topic Topic) error {
	var authToken string
	nonce := generateNonce()
	if topic.IsUserTopic() {
		// Read the CURRENT credential snapshot at write time and record which
		// generation THIS frame (by nonce) presented, so a later ERR_BADAUTH
		// RESPONSE is attributed to the exact credential set of the frame it
		// answers — not to whatever was written most recently. lastAuthGen is
		// the LISTEN-only fallback for responses without a tracked nonce.
		snap := ws.authSnapshot()
		authToken = snap.Token
		ws.mu.Lock()
		if ws.userFrameGens == nil || len(ws.userFrameGens) > maxTrackedAuthNonces {
			ws.userFrameGens = make(map[string]uint64)
		}
		ws.userFrameGens[nonce] = snap.Generation
		if frameType == "LISTEN" {
			ws.lastAuthGen = snap.Generation
		}
		ws.mu.Unlock()
	}

	if hook := ws.writeTopicFrameHook; hook != nil {
		return hook(frameType, topic)
	}
	if conn == nil {
		// No transport (library/test construction): matches send()'s
		// historical nil-conn no-op.
		return nil
	}

	data := &WSData{
		Topics: []string{topic.String()},
	}
	if topic.IsUserTopic() {
		data.AuthToken = authToken
	}
	msg := WSMessage{
		Type:  frameType,
		Nonce: nonce,
		Data:  data,
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	slog.Debug("WebSocket send", "index", ws.index, "type", frameType)
	return conn.WriteMessage(websocket.TextMessage, payload)
}

// replayPendingTopics promotes parked topics onto the live connection. Each
// iteration runs its whole read-decide-write-commit under writeMu — PEEKING
// the head rather than popping, so a mid-write reconnect merge can never lose
// the topic — which linearizes the replayed LISTEN against concurrent runtime
// Listen/Unlisten calls: a disable issued during a replay write serializes
// behind it and follows with the compensating UNLISTEN, and a disable issued
// before the topic's iteration removes it from the parked set so it is never
// written at all. A write failure leaves the topic parked (retryable by an
// identical EnsureTopic or the next replay) and stops this replay: the socket
// is broken and the read-loop reconnect owns recovery.
func (ws *WebSocketClient) replayPendingTopics() {
	for {
		ws.writeMu.Lock()
		ws.mu.Lock()
		if !ws.isOpened || len(ws.pendingTopics) == 0 {
			ws.mu.Unlock()
			ws.writeMu.Unlock()
			return
		}
		topic := ws.pendingTopics[0]
		conn := ws.conn
		gen := ws.connGen
		ws.mu.Unlock()

		err := ws.writeTopicFrame(conn, "LISTEN", topic)

		ws.mu.Lock()
		stale := gen != ws.connGen
		if !stale && err == nil {
			ws.pendingTopics = topicRemove(ws.pendingTopics, topic)
			ws.topics = topicAppendUnique(ws.topics, topic)
			ws.unlistenRetry = topicRemove(ws.unlistenRetry, topic)
		}
		ws.mu.Unlock()
		ws.writeMu.Unlock()

		if stale {
			return // a newer generation owns the parked set now
		}
		if err != nil {
			slog.Warn("WebSocket topic replay write failed; topics stay parked for the next replay",
				"index", ws.index, "error", err)
			return
		}
	}
}

// HasTopic reports whether this connection DESIRES the topic — wire-applied
// or parked in pendingTopics (a reconnect in flight moves the live set there)
// — so a pool-wide dedup scan cannot double-subscribe a topic mid-reconnect.
// A topic owing only a failed-UNLISTEN retry is NOT desired and not counted.
func (ws *WebSocketClient) HasTopic(topic Topic) bool {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	return topicIn(ws.topics, topic) || topicIn(ws.pendingTopics, topic)
}

// HasTopicApplied reports whether the topic's LISTEN frame was successfully
// written on this connection's CURRENT generation — the only state in which a
// desired topic needs no further wire action.
func (ws *WebSocketClient) HasTopicApplied(topic Topic) bool {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	return topicIn(ws.topics, topic)
}

// HasUnlistenDebt reports whether this connection owes a failed-UNLISTEN retry
// for the topic. While the debt stands, the topic's wire subscription may
// still exist HERE — so a pool-level re-enable must stay on this connection
// (its Listen settles the debt) instead of subscribing elsewhere and leaving a
// duplicate plus an eternal debt behind.
func (ws *WebSocketClient) HasUnlistenDebt(topic Topic) bool {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	return topicIn(ws.unlistenRetry, topic)
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
				// Attribute the rejection to the exact frame it answers (by
				// nonce); fall back to the last-written LISTEN generation for
				// untracked nonces.
				ws.mu.Lock()
				gen := ws.lastAuthGen
				if g, ok := ws.userFrameGens[msg.Nonce]; ok {
					gen = g
				}
				delete(ws.userFrameGens, msg.Nonce)
				ws.mu.Unlock()
				ws.onError(&AuthError{Message: "ERR_BADAUTH", Generation: gen})
			} else {
				ws.mu.Lock()
				delete(ws.userFrameGens, msg.Nonce)
				ws.mu.Unlock()
			}
		} else {
			// Successful RESPONSE settles its nonce's attribution entry.
			ws.mu.Lock()
			delete(ws.userFrameGens, msg.Nonce)
			ws.mu.Unlock()
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
	// The old socket's wire subscriptions die with it, so any failed-UNLISTEN
	// debt is settled by the socket's death; the new generation replays only
	// the desired set.
	ws.unlistenRetry = nil
	// The connection is no longer usable from this point: a Listen arriving
	// during the redial must PARK its topic (picked up by the post-connect
	// replay) instead of writing a LISTEN frame into the dead old conn and
	// stranding the topic as tracked-but-never-subscribed. Bumping the
	// generation voids the commit of any topic write already in flight on the
	// dying socket.
	ws.isOpened = false
	ws.connGen++
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

// AuthError reports a Twitch PubSub auth rejection (ERR_BADAUTH). Generation
// is the credential generation whose token was last written in a user-topic
// LISTEN on the failing connection — the recovery layer uses it to tell a
// stale rejection of an already-rotated token from one of the current
// credentials. It never carries token material.
type AuthError struct {
	Message    string
	Generation uint64
}

func (e *AuthError) Error() string {
	return e.Message
}

// Is matches any AuthError with the same message, so
// errors.Is(err, ErrBadAuth) keeps working for instances that carry a
// generation.
func (e *AuthError) Is(target error) bool {
	t, ok := target.(*AuthError)
	return ok && t.Message == e.Message
}
