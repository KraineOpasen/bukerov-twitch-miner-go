package pubsub

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	mathrand "math/rand"
	"sync"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/gorilla/websocket"
)

// pongTimeoutDefault bounds how long a connection may stay silent after a PING
// was successfully written before it is declared deaf. The deadline is armed
// only after a successful write (never from a scheduled tick), so the ping
// scheduler's own delay can never manufacture a timeout. Replaces the old
// coarse 5-minute idle watchdog with a tight, generation-scoped deadline.
const pongTimeoutDefault = 15 * time.Second

// errPongTimeout and errPingWrite are the typed lifecycle reasons surfaced to
// the reconnect owner so a deaf socket or a failed keepalive write is
// distinguishable in logs from a read-side transport error.
var (
	errPongTimeout = errors.New("pubsub: pong deadline exceeded")
	errPingWrite   = errors.New("pubsub: ping write failed")
)

// listenAttempt records the topic and connection generation a LISTEN frame was
// written under, keyed by the frame's nonce, so a permanent-invalid RESPONSE
// (ERR_BADTOPIC) can be correlated back to the exact topic it rejects and
// guarded against stale/superseded generations before any eviction.
type listenAttempt struct {
	topic Topic
	gen   uint64
}

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

	// reconnectRequestHook, when set (tests only), replaces the real
	// go-reconnect dispatch of requestAuthReconnect so the bounded
	// failed-relisten recovery path is observable without a network.
	reconnectRequestHook func()

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

	// listenNonces correlates an outstanding LISTEN frame's nonce to the topic
	// and connection generation it was written under, so an authoritative
	// ERR_BADTOPIC RESPONSE names the exact topic to evict. At most one entry
	// per topic is retained (a re-LISTEN drops the topic's prior nonce), which
	// is what prevents a stale rejection of a superseded attempt from evicting a
	// re-desired topic. Bounded and reset per generation, like userFrameGens.
	listenNonces map[string]listenAttempt

	// pongDeadline* form the single outstanding PONG deadline for the CURRENT
	// generation. Armed ONLY after a PING write succeeds (pongDeadlineArmed
	// stays false until then, so no successful write means no timeout can fire);
	// cleared by a matching PONG on the same generation. All guarded by ws.mu.
	pongDeadlineArmed bool
	pongDeadlineGen   uint64
	pongDeadlineAt    time.Time
	// pongSeq counts PONGs received on the CURRENT generation; pongDeadlineBase
	// snapshots it just before the PING write. A deadline is satisfied if either
	// a PONG cleared the armed flag (the common case) OR pongSeq advanced past
	// the base — which credits a PONG processed in the tiny window between the
	// successful write and armPongDeadline, so an answered ping can never time
	// out even under adverse goroutine scheduling.
	pongSeq          uint64
	pongDeadlineBase uint64

	// pongTimeout is how long after a successful PING write a PONG must arrive.
	// Immutable after construction (a test seam like delayUnit).
	pongTimeout time.Duration
	// pingUnit scales the (jittered) ping interval; seconds in production,
	// milliseconds in tests. Immutable after construction.
	pingUnit time.Duration

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
	// onInvalidTopic is fired (with no client lock held) when an authoritative
	// ERR_BADTOPIC evicts a topic, so the pool can quarantine it from effective
	// desired state. Wired once (before Connect) by the pool, captured under
	// ws.mu, invoked with the lock released.
	onInvalidTopic func(Topic)

	// writePingHook, when set (tests only), replaces the real PING frame write
	// so a keepalive write failure can be injected deterministically without a
	// network.
	writePingHook func() error

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
		pongTimeout:    pongTimeoutDefault,
		pingUnit:       time.Second,
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
	// The old socket's outstanding LISTEN correlations and PONG deadline are
	// void with its generation: a late ERR_BADTOPIC or PONG for an old frame
	// must never touch the new connection.
	ws.listenNonces = nil
	ws.pongDeadlineArmed = false
	gen := ws.connGen
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

	// The loops are bound to THIS generation: gen fences the PONG deadline and
	// invalid-topic correlation, and (stop, conn) fence the I/O, so an old loop
	// outliving a reconnect can neither read the new conn nor heal/void the new
	// generation's state.
	go ws.readLoop(stop, conn, gen)
	go ws.pingLoop(stop, conn, gen)

	return nil
}

// SetInvalidTopicHandler registers a callback fired once when an authoritative
// ERR_BADTOPIC evicts a topic from this connection, so the pool can quarantine
// it from effective desired state. Wired once (before Connect) by the pool. The
// handler is captured under ws.mu and invoked with the lock released, so it may
// safely acquire pool/other locks.
func (ws *WebSocketClient) SetInvalidTopicHandler(h func(Topic)) {
	ws.mu.Lock()
	ws.onInvalidTopic = h
	ws.mu.Unlock()
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

	failed := false
	for _, t := range userTopics {
		ws.mu.RLock()
		stillCurrent := gen == ws.connGen
		ws.mu.RUnlock()
		if !stillCurrent {
			return
		}
		if err := ws.writeTopicFrame(conn, "LISTEN", t); err != nil {
			// Broken socket. Privacy-safe log, then an EXPLICIT bounded
			// recovery request instead of hoping the read loop notices: the
			// reconnect's replay re-LISTENs the ledger's desired topics with
			// the current token. (Requested while writeMu is still held —
			// see requestAuthReconnect: it only takes a brief ws.mu read and
			// dispatches on its own goroutine.)
			slog.Warn("Re-authorizing user topic after token rotation failed; requesting one reconnect", "index", ws.index, "error", err)
			failed = true
			break
		}
	}
	if failed {
		// Safe under the held writeMu: requestAuthReconnect takes only a
		// brief ws.mu read (the documented writeMu -> ws.mu order) and
		// dispatches the actual reconnect on its own goroutine.
		ws.requestAuthReconnect()
	}
}

// requestAuthReconnect asks for exactly one reconnect of this connection after
// a failed post-rotation re-authorization write. Collapsed with any reconnect
// already in flight (isReconnecting) and suppressed on shutdown (forcedClose/
// isClosed); the reconnect replay re-subscribes the desired topic ledger with
// the CURRENT token. Tests observe the request via reconnectRequestHook.
func (ws *WebSocketClient) requestAuthReconnect() {
	ws.mu.RLock()
	skip := ws.isReconnecting || ws.forcedClose || ws.isClosed
	ws.mu.RUnlock()
	if skip {
		return
	}
	if hook := ws.reconnectRequestHook; hook != nil {
		hook()
		return
	}
	go ws.reconnect()
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

	// Correlate this LISTEN's nonce to its topic and the generation it is
	// written under, so an authoritative ERR_BADTOPIC can name the exact topic
	// to evict. Recorded BEFORE the test hook so the correlation is exercised
	// with fake transports too. Only the latest attempt per topic is kept: a
	// re-LISTEN drops the topic's prior nonce, so a stale rejection of a
	// superseded attempt can never evict a re-desired topic (P10).
	if frameType == "LISTEN" {
		ws.mu.Lock()
		if ws.listenNonces == nil || len(ws.listenNonces) > maxTrackedAuthNonces {
			ws.listenNonces = make(map[string]listenAttempt)
		}
		key := topic.String()
		for n, a := range ws.listenNonces {
			if a.topic.String() == key {
				delete(ws.listenNonces, n)
			}
		}
		ws.listenNonces[nonce] = listenAttempt{topic: topic, gen: ws.connGen}
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

// writePing writes a PING frame to a SPECIFIC generation's conn under writeMu
// (serialized against topic frames) and returns the write error so the ping
// loop can react to a failed keepalive instead of silently dropping it. Unlike
// send(), it writes to the conn the ping loop captured for its generation, not
// a fresh snapshot, so a ping can never land on a newer connection.
func (ws *WebSocketClient) writePing(conn *websocket.Conn) error {
	if hook := ws.writePingHook; hook != nil {
		return hook()
	}
	ws.writeMu.Lock()
	defer ws.writeMu.Unlock()
	if conn == nil {
		return nil
	}
	data, err := json.Marshal(WSMessage{Type: "PING"})
	if err != nil {
		return err
	}
	slog.Debug("WebSocket send", "index", ws.index, "type", "PING")
	return conn.WriteMessage(websocket.TextMessage, data)
}

// ping writes a PING using the current connection snapshot, discarding the
// error. Retained only for the concurrency stress test; the ping loop uses
// writePing with its captured generation conn and never ignores the error.
func (ws *WebSocketClient) ping() {
	ws.mu.RLock()
	conn := ws.conn
	ws.mu.RUnlock()
	_ = ws.writePing(conn)
}

// armPongDeadline publishes the single PONG deadline for gen. Called ONLY after
// a PING write succeeds, so pongDeadlineArmed stays false until a ping is
// genuinely on the wire (P2/P3): a scheduler delay before the write, or a
// failed write, can never make a timeout fire. A stale generation arms nothing.
func (ws *WebSocketClient) armPongDeadline(gen uint64, at time.Time, baseSeq uint64) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if gen != ws.connGen {
		return
	}
	ws.pongDeadlineArmed = true
	ws.pongDeadlineGen = gen
	ws.pongDeadlineAt = at
	ws.pongDeadlineBase = baseSeq
}

// pongSeqSnapshot returns the current PONG counter. The ping loop captures it
// immediately before the PING write so a PONG processed before the deadline is
// armed is still credited to this ping (see pongDeadlineExpired).
func (ws *WebSocketClient) pongSeqSnapshot() uint64 {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	return ws.pongSeq
}

// recordPong records a received PONG for the reader's generation. A PONG from a
// superseded generation (gen != connGen) is ignored, so a late frame drained by
// an old reader can neither refresh liveness nor clear the new connection's
// deadline (P4/P5). A matching PONG clears the current outstanding deadline.
func (ws *WebSocketClient) recordPong(gen uint64, now time.Time) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if gen != ws.connGen {
		return
	}
	ws.lastPong = now
	// Count every current-generation PONG, even one arriving before its ping's
	// deadline is armed, so the arm-window is credited via pongDeadlineBase.
	ws.pongSeq++
	if ws.pongDeadlineArmed && ws.pongDeadlineGen == gen {
		ws.pongDeadlineArmed = false
	}
}

// pongDeadlineExpired reports whether gen's PONG deadline is still armed and has
// passed. Returns false when no deadline was armed (no successful write, P3),
// when a matching PONG already cleared it, or when the generation has been
// superseded — so only a genuine, current, unanswered PING can time out.
func (ws *WebSocketClient) pongDeadlineExpired(gen uint64, now time.Time) bool {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	if !ws.pongDeadlineArmed || ws.pongDeadlineGen != gen || gen != ws.connGen {
		return false
	}
	// A PONG observed since the ping was initiated (including one processed in
	// the write→arm window) satisfies the deadline even if it did not clear the
	// armed flag.
	if ws.pongSeq != ws.pongDeadlineBase {
		return false
	}
	return !now.Before(ws.pongDeadlineAt)
}

// failConnection converges a ping-write or PONG-deadline failure for a specific
// generation onto the single reconnect owner (reconnectAfter's isReconnecting
// guard). A failure observed on an already-superseded generation, or during a
// deliberate shutdown, reconnects nothing (P6/P7/P9). Holds no lock across the
// callback or the redial dispatch.
func (ws *WebSocketClient) failConnection(gen uint64, reason error) {
	ws.mu.RLock()
	stale := gen != ws.connGen
	shuttingDown := ws.forcedClose
	delay := ws.readErrorReconnectDelayLocked()
	ws.mu.RUnlock()
	if stale || shuttingDown {
		return
	}
	slog.Warn("WebSocket connection lifecycle failure; reconnecting",
		"index", ws.index, "reason", reason.Error(), "delay", delay)
	if ws.onError != nil {
		ws.onError(reason)
	}
	go ws.reconnectAfter(delay)
}

// readLoop reads frames from ONE connection generation: stop, conn and gen are
// snapshotted by Connect under mu, so a loop outliving a reconnect can never
// adopt the replacement channel/conn from the fields (which would leave it
// unstoppable, or reading the new conn concurrently with its own reader) and
// its PONG/ERR_BADTOPIC handling is fenced to its own generation.
func (ws *WebSocketClient) readLoop(stop chan struct{}, conn *websocket.Conn, gen uint64) {
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

		ws.handleMessageForGen(wsMsg, gen)
	}
}

// handleMessage dispatches a frame attributed to the CURRENT generation. It is
// the entry point for direct (test) invocation; the read loop uses
// handleMessageForGen with its own captured generation so a superseded reader's
// frames are correctly fenced.
func (ws *WebSocketClient) handleMessage(msg WSMessage) {
	ws.mu.RLock()
	gen := ws.connGen
	ws.mu.RUnlock()
	ws.handleMessageForGen(msg, gen)
}

func (ws *WebSocketClient) handleMessageForGen(msg WSMessage, readerGen uint64) {
	slog.Debug("WebSocket received", "index", ws.index, "type", msg.Type)

	switch msg.Type {
	case "PONG":
		ws.recordPong(readerGen, time.Now())

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
		ws.handleListenResponse(msg, readerGen)

	case "RECONNECT":
		slog.Info("WebSocket reconnect requested", "index", ws.index)
		go ws.reconnect()
	}
}

// listenResponseClass is the typed classification of a Twitch PubSub LISTEN
// RESPONSE. Only an exact ERR_BADTOPIC is authoritative-permanent; every other
// non-empty, non-auth error is transient/retryable, so no topic is ever
// permanently evicted on an ambiguous or unknown string.
type listenResponseClass int

const (
	respAccepted     listenResponseClass = iota // error == "": subscription accepted
	respBadAuth                                 // ERR_BADAUTH: routed to BKM-005 recovery
	respInvalidTopic                            // ERR_BADTOPIC: authoritative permanent
	respTransient                               // ERR_SERVER / anything else: retryable
)

// classifyListenResponse maps a RESPONSE error string to its class. The mapping
// is deliberately narrow: only the exact ERR_BADTOPIC is permanent.
func classifyListenResponse(errStr string) listenResponseClass {
	switch errStr {
	case "":
		return respAccepted
	case "ERR_BADAUTH":
		return respBadAuth
	case "ERR_BADTOPIC":
		return respInvalidTopic
	default:
		return respTransient
	}
}

// handleListenResponse classifies one RESPONSE and applies exactly one outcome,
// always settling the frame's nonce attribution:
//   - accepted / transient: settle the nonce; the topic stays desired (a
//     transient rejection is retried by the next reconnect replay, P11).
//   - ERR_BADAUTH: the pre-BKM-012 path, unchanged — attribute the rejection to
//     the exact frame's credential generation and route it through onError to
//     the shared OAuth recovery (BKM-005, P12); never an eviction.
//   - ERR_BADTOPIC: correlate to the exact topic by nonce, guarded by the
//     reader's connection generation; on a match evict it from the ledger and
//     quarantine it (P10). A stale/unknown nonce or a superseded generation
//     evicts nothing (P5/T12).
func (ws *WebSocketClient) handleListenResponse(msg WSMessage, readerGen uint64) {
	switch classifyListenResponse(msg.Error) {
	case respAccepted:
		ws.mu.Lock()
		delete(ws.userFrameGens, msg.Nonce)
		delete(ws.listenNonces, msg.Nonce)
		ws.mu.Unlock()

	case respBadAuth:
		slog.Error("WebSocket response error", "index", ws.index, "error", msg.Error)
		// Attribute the rejection to the exact frame it answers (by nonce);
		// fall back to the last-written LISTEN generation for untracked nonces.
		ws.mu.Lock()
		gen := ws.lastAuthGen
		if g, ok := ws.userFrameGens[msg.Nonce]; ok {
			gen = g
		}
		delete(ws.userFrameGens, msg.Nonce)
		delete(ws.listenNonces, msg.Nonce)
		ws.mu.Unlock()
		if ws.onError != nil {
			ws.onError(&AuthError{Message: "ERR_BADAUTH", Generation: gen})
		}

	case respTransient:
		slog.Warn("WebSocket transient LISTEN rejection; topic stays desired and retryable",
			"index", ws.index, "error", msg.Error)
		ws.mu.Lock()
		delete(ws.userFrameGens, msg.Nonce)
		delete(ws.listenNonces, msg.Nonce)
		ws.mu.Unlock()

	case respInvalidTopic:
		slog.Error("WebSocket response error", "index", ws.index, "error", msg.Error)
		ws.mu.Lock()
		attempt, known := ws.listenNonces[msg.Nonce]
		delete(ws.listenNonces, msg.Nonce)
		delete(ws.userFrameGens, msg.Nonce)
		// Evict only when the rejection names a known frame written on THIS live
		// generation: current reader, current connection, current attempt.
		current := known && attempt.gen == readerGen && readerGen == ws.connGen
		ws.mu.Unlock()
		if !current {
			slog.Warn("WebSocket ignoring invalid-topic rejection (stale/unknown nonce or superseded generation)",
				"index", ws.index, "nonce_known", known)
			return
		}
		ws.evictTopicFromLedger(attempt.topic)
		ws.mu.RLock()
		onInvalid := ws.onInvalidTopic
		ws.mu.RUnlock()
		if onInvalid != nil {
			onInvalid(attempt.topic)
		}
		slog.Warn("WebSocket permanent-invalid topic evicted from desired state",
			"index", ws.index, "topic", attempt.topic.String())
	}
}

// evictTopicFromLedger removes a permanently-invalid topic from every wire
// bucket so it is never re-LISTENed (on this generation or via reconnect
// replay). No frame is written: an ERR_BADTOPIC subscription was never
// established on the wire. Runs under writeMu -> ws.mu (the ledger's lock order,
// caller must hold neither) so it linearizes against Listen/Unlisten/replay.
func (ws *WebSocketClient) evictTopicFromLedger(topic Topic) {
	ws.writeMu.Lock()
	defer ws.writeMu.Unlock()
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.topics = topicRemove(ws.topics, topic)
	ws.pendingTopics = topicRemove(ws.pendingTopics, topic)
	ws.unlistenRetry = topicRemove(ws.unlistenRetry, topic)
	key := topic.String()
	for n, a := range ws.listenNonces {
		if a.topic.String() == key {
			delete(ws.listenNonces, n)
		}
	}
}

func (ws *WebSocketClient) randomPingInterval() time.Duration {
	base := float64(ws.pingInterval)
	jitter := (mathrand.Float64() - 0.5) * 5.0
	d := base + jitter
	if d < 0 {
		d = 0
	}
	return time.Duration(d * float64(ws.pingUnit))
}

// pingLoop drives the keepalive PING and its PONG deadline for ONE connection
// generation (see readLoop on why stop/conn/gen are parameters, not fields).
//
// Each cycle: wait a jittered interval, write a PING, and only on a SUCCESSFUL
// write arm a single PONG deadline for this generation. A matching PONG
// (recorded by this generation's read loop) clears it; if the deadline passes
// still armed, the socket is deaf and control converges to the reconnect owner.
// A ping-write failure arms nothing and converges immediately, so a half-open
// socket (writes fail, reads block) can never outlive its ping loop — closing
// the socket via the reconnect owner unblocks the read loop too. The scheduler
// delay before the write is never part of any deadline (P1-P4/P7, T1-T6).
func (ws *WebSocketClient) pingLoop(stop chan struct{}, conn *websocket.Conn, gen uint64) {
	for {
		select {
		case <-stop:
			return
		case <-time.After(ws.randomPingInterval()):
		}

		// Skip pinging into a socket a reconnect is already tearing down; the
		// generation guard is the real fence, this just avoids a pointless write.
		ws.mu.RLock()
		reconnecting := ws.isReconnecting
		ws.mu.RUnlock()
		if reconnecting {
			continue
		}

		// Snapshot the PONG counter BEFORE the write so a PONG processed in the
		// gap between the successful write and armPongDeadline is still credited
		// to this ping and cannot cause a false timeout.
		baseSeq := ws.pongSeqSnapshot()
		if err := ws.writePing(conn); err != nil {
			ws.failConnection(gen, fmt.Errorf("%w: %v", errPingWrite, err))
			return
		}

		// The PING is on the wire: arm exactly one deadline, measured from AFTER
		// the successful write.
		ws.armPongDeadline(gen, time.Now().Add(ws.pongTimeout), baseSeq)

		select {
		case <-stop:
			return
		case <-time.After(ws.pongTimeout):
		}

		if ws.pongDeadlineExpired(gen, time.Now()) {
			ws.failConnection(gen, errPongTimeout)
			return
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
