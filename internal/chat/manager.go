package chat

import (
	"errors"
	"log/slog"
	"net"
	"sync"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

type ChatManager struct {
	username string
	// tokenFn supplies the CURRENT OAuth credential snapshot to every IRC
	// client this manager creates, so joins and reconnects after a token
	// rotation authenticate with the rotated token. Set once, immutable.
	tokenFn TokenProvider
	// authErrorHandler, when set (before any join), receives the rejected
	// credential generation whenever a client observes the documented IRC
	// login-authentication-failed NOTICE. Wired by the miner into the shared
	// single-flight recovery.
	authErrorHandler func(rejectedGeneration uint64)
	clients          map[string]*IRCClient
	logger           ChatLogger
	globalChatLogsOn bool
	mentionHandler   MentionHandler
	// logGate is shared by every client generation this manager creates. Its
	// writer is the ApplySettings structured-write barrier; logGeneration is
	// the manager-locked mirror used to construct candidates without taking the
	// gate in the inverse lock order.
	logGate       *chatLogGate
	logGeneration uint64

	// loggingReconcileMu serializes direct ReconcileLogging callers (the miner
	// already serializes production calls with reconcileMu). pending reserves a
	// login while any IRC candidate connects outside all manager/gate locks, so
	// ToggleChat cannot start a duplicate and an apply can fence an in-flight
	// pre-apply join before it returns.
	loggingReconcileMu sync.Mutex
	pending            map[string]*clientReservation
	retirements        map[*IRCClient]*clientRetirement

	// rosterMembership, when set, gates joinChat: a streamer pointer is only
	// joined if this reports true for it. See SetRosterMembership.
	rosterMembership func(*models.Streamer) bool

	// dialFn, when set, is handed to every IRC client this manager creates. It
	// is nil in production (clients dial Twitch IRC over TLS); tests inject an
	// in-memory transport so presence transitions run without a network.
	dialFn func() (net.Conn, error)

	// closed marks the manager as shut down (S1): joinChat must not create a
	// fresh IRC client — whose read loop would outlive the shutdown drain —
	// once Close has run. Guarded by mu.
	closed bool

	mu sync.RWMutex
}

// clientReservation is the exact-pointer CAS ticket for an ordinary join or
// an existing IRC session being replaced after a logging generation change.
// candidate remains unauthorized until its install; Leave/Close/apply cancel
// the ticket and stop both pointers, preventing a late dial from resurrecting
// the login. preservePresence distinguishes an established session (UNKNOWN
// keeps it) from an absent ordinary join (UNKNOWN never invents it).
type clientReservation struct {
	generation       uint64
	login            string
	streamer         *models.Streamer
	old              *IRCClient
	candidate        *IRCClient
	preservePresence bool
	retryOnFailure   bool
}

// clientRetirement is the single Stop owner for a detached client. Keeping it
// registered until Stop completes lets a racing Close join the same bounded
// retirement instead of missing the detached read loop or taking Stop's
// idempotent fast path without joining the first caller.
type clientRetirement struct {
	client *IRCClient
	start  sync.Once
	done   chan struct{}
	err    error
}

func NewChatManager(username string, tokenFn TokenProvider, logger ChatLogger, globalChatLogsOn bool, mentionHandler MentionHandler) *ChatManager {
	const initialLoggingGeneration = 1
	return &ChatManager{
		username:         username,
		tokenFn:          tokenFn,
		clients:          make(map[string]*IRCClient),
		logger:           logger,
		globalChatLogsOn: globalChatLogsOn,
		mentionHandler:   mentionHandler,
		logGate: &chatLogGate{
			generation: initialLoggingGeneration,
			logger:     logger,
		},
		logGeneration: initialLoggingGeneration,
		pending:       make(map[string]*clientReservation),
		retirements:   make(map[*IRCClient]*clientRetirement),
	}
}

// SetAuthErrorHandler registers the sink for documented IRC authentication
// rejections (rejected credential generation only — never token material).
// Set once at wiring time, before any join.
func (m *ChatManager) SetAuthErrorHandler(handler func(rejectedGeneration uint64)) {
	m.mu.Lock()
	m.authErrorHandler = handler
	m.mu.Unlock()
}

// SetRosterMembership registers the predicate joinChat uses to reject a
// streamer pointer that is no longer the roster's live tracked object for its
// login (a stale pointer surviving a removal, or one that never belonged to
// the roster at all). Set ONCE at wiring time, before any join; nil preserves
// standalone behavior, where every join is permitted (the pre-M3 behavior,
// and the default for library/test use with no roster of its own).
//
// The predicate runs with ChatManager.mu HELD: it must never call back into
// ChatManager (that would deadlock, sync.RWMutex is not reentrant), and may
// only acquire locks BELOW ChatManager.mu in the documented lock order
// (coordinatorMu -> {miner.mu | reconcileMu} -> ChatManager.mu ->
// Manager.mu -> Streamer.mu, IRCClient.mu leaf) — i.e. streamer.Manager's
// lock and/or a models.Streamer's own lock, never anything above.
func (m *ChatManager) SetRosterMembership(fn func(*models.Streamer) bool) {
	m.mu.Lock()
	m.rosterMembership = fn
	m.mu.Unlock()
}

// ToggleChat reconciles the streamer's IRC presence with its CURRENT Chat
// setting, idempotently: joining when already joined and leaving when already
// left are no-ops, so it is safe to invoke immediately on every runtime
// settings apply as well as from the periodic stream-check loop.
func (m *ChatManager) ToggleChat(streamer *models.Streamer) {
	// Take the manager lock BEFORE the streamer snapshots. This makes the
	// presence decision and its map action indivisible with manager-owned
	// logging publication: a periodic ToggleChat cannot snapshot pre-apply
	// settings and perform that stale action after logging reconciliation.
	var reservation *clientReservation
	var retirements []*clientRetirement
	m.mu.Lock()
	switch streamer.GetSettings().Chat {
	case models.ChatAlways:
		reservation, retirements = m.reserveJoinLocked(streamer)
	case models.ChatNever:
		retirements = m.detachLoginLocked(streamer.GetUsername())
	case models.ChatOnline:
		// Join only on CONFIRMED online; leave only on CONFIRMED offline. On
		// UNKNOWN take no action, so an online→unknown blip does not tear down an
		// already-established IRC connection over transient uncertainty.
		switch streamer.GetStatus() {
		case models.StatusOnline:
			reservation, retirements = m.reserveJoinLocked(streamer)
		case models.StatusOffline:
			retirements = m.detachLoginLocked(streamer.GetUsername())
		}
	case models.ChatOffline:
		// Join only on CONFIRMED offline; leave on CONFIRMED online. UNKNOWN is
		// NOT treated as offline, so no offline-specific join happens on it.
		switch streamer.GetStatus() {
		case models.StatusOffline:
			reservation, retirements = m.reserveJoinLocked(streamer)
		case models.StatusOnline:
			retirements = m.detachLoginLocked(streamer.GetUsername())
		}
	}
	m.mu.Unlock()
	m.waitClientRetirements(retirements)
	if reservation != nil {
		m.connectReservation(reservation)
	}
}

// shouldLogChatLocked resolves the tri-state override against the current
// manager global. Caller holds m.mu.
func (m *ChatManager) shouldLogChatLocked(streamer *models.Streamer) bool {
	if logs := streamer.GetSettings().ChatLogs; logs != nil {
		return *logs
	}
	return m.globalChatLogsOn
}

// reserveJoinLocked performs the registry half of an ordinary idempotent join.
// Caller holds m.mu. The candidate's dial/auth/JOIN runs only after that lock
// is released, and the exact reservation/generation CAS below decides whether
// it may become authoritative.
func (m *ChatManager) reserveJoinLocked(streamer *models.Streamer) (*clientReservation, []*clientRetirement) {
	login := streamer.GetUsername()

	if m.closed {
		slog.Debug("Rejected joinChat: chat manager is shutting down", "channel", login)
		return nil, nil
	}

	if m.rosterMembership != nil && !m.rosterMembership(streamer) {
		// A stale or foreign streamer pointer: the roster no longer tracks
		// THIS object under its login (removed, or it never belonged), so
		// joining here would create a ghost IRC client nothing will ever
		// leave. Reject before any client lookup/creation. Login only —
		// never any other streamer field — stays in the log line.
		slog.Debug("Rejected joinChat for non-roster streamer", "channel", login)
		return nil, nil
	}
	if _, replacing := m.pending[login]; replacing {
		return nil, nil
	}

	var retirements []*clientRetirement
	if client, exists := m.clients[login]; exists {
		if client.IsRunning() {
			return nil, nil
		}
		client.revoke()
		delete(m.clients, login)
		retirements = m.retireClientsLocked([]*IRCClient{client})
	}

	reservation := m.newReservationLocked(login, streamer, nil, false, false)
	return reservation, retirements
}

// newReservationLocked snapshots the current immutable client logging/session
// policy and publishes one per-login reservation. Caller holds m.mu and has
// already checked that the login has no reservation.
func (m *ChatManager) newReservationLocked(login string, streamer *models.Streamer, old *IRCClient, preservePresence, retryOnFailure bool) *clientReservation {
	logChat := m.shouldLogChatLocked(streamer)
	reservation := &clientReservation{
		generation:       m.logGeneration,
		login:            login,
		streamer:         streamer,
		old:              old,
		candidate:        m.newClientLocked(streamer, logChat),
		preservePresence: preservePresence,
		retryOnFailure:   retryOnFailure,
	}
	m.pending[login] = reservation
	return reservation
}

// newClientLocked constructs an unauthorized manager-owned candidate for the
// current logging generation. Caller holds m.mu; authority is granted only
// after the client is installed in clients by the owning path.
func (m *ChatManager) newClientLocked(streamer *models.Streamer, logChat bool) *IRCClient {
	client := newIRCClient(m.username, m.tokenFn, streamer, m.logger, logChat, m.mentionHandler, m.logGate, m.logGeneration, false)
	client.authErrorFn = m.authErrorHandler
	if m.dialFn != nil {
		client.dialFn = m.dialFn
	}
	return client
}

func (m *ChatManager) Leave(username string) {
	m.mu.Lock()
	retirements := m.detachLoginLocked(username)
	m.mu.Unlock()
	m.waitClientRetirements(retirements)
}

// detachLoginLocked atomically removes the installed generation and cancels
// any join/replacement reservation. Caller holds m.mu; retirements are only
// started after unlock, so bounded Stop/network work never owns the manager
// registry lock.
func (m *ChatManager) detachLoginLocked(login string) []*clientRetirement {
	var clients []*IRCClient
	if replacement := m.pending[login]; replacement != nil {
		delete(m.pending, login)
		if replacement.old != nil {
			clients = append(clients, replacement.old)
		}
		if replacement.candidate != nil {
			clients = append(clients, replacement.candidate)
		}
	}
	if client := m.clients[login]; client != nil {
		delete(m.clients, login)
		clients = append(clients, client)
	}
	return m.retireClientsLocked(clients)
}

// retireClientsLocked registers exactly one bounded Stop owner for each client
// before m.mu is released. The caller starts it after releasing every manager
// and gate lock; a racing Close can find the registration and start/join that
// same retirement first. Caller holds m.mu.
func (m *ChatManager) retireClientsLocked(clients []*IRCClient) []*clientRetirement {
	unique := make(map[*IRCClient]bool, len(clients))
	retirements := make([]*clientRetirement, 0, len(clients))
	for _, client := range clients {
		if client == nil || unique[client] {
			continue
		}
		unique[client] = true
		if retirement := m.retirements[client]; retirement != nil {
			retirements = append(retirements, retirement)
			continue
		}
		client.revoke()
		retirement := &clientRetirement{client: client, done: make(chan struct{})}
		m.retirements[client] = retirement
		retirements = append(retirements, retirement)
	}
	return retirements
}

func (m *ChatManager) startClientRetirement(retirement *clientRetirement) {
	if retirement == nil {
		return
	}
	retirement.start.Do(func() {
		go func() {
			retirement.err = retirement.client.Stop()
			close(retirement.done)
			m.mu.Lock()
			if m.retirements[retirement.client] == retirement {
				delete(m.retirements, retirement.client)
			}
			m.mu.Unlock()
		}()
	})
}

func (m *ChatManager) waitClientRetirements(retirements []*clientRetirement) {
	seen := make(map[*clientRetirement]bool, len(retirements))
	for _, retirement := range retirements {
		if retirement == nil || seen[retirement] {
			continue
		}
		seen[retirement] = true
		m.startClientRetirement(retirement)
	}
	for retirement := range seen {
		<-retirement.done
	}
}

// ReconcileLogging publishes the current inherited-global/sink state and
// generation-fences every installed IRC session whose immutable CAP/write
// policy is no longer authoritative. It is intentionally independent of
// presence: an existing ONLINE/OFFLINE+UNKNOWN connection is replaced, while
// an absent UNKNOWN connection is never invented.
//
// The logGate writer is the successful-return boundary. It is acquired with no
// manager/client lock held and kept through the short authority publication,
// so every previously admitted structured write completes first. Ordinary
// joins reserve under m.mu but perform network I/O after releasing it; therefore
// acquiring m.mu here cannot turn a slow dial/auth/JOIN into a gate-held delay.
// Stop and Connect run only after both locks are released. A permanently wedged
// sink can still delay ApplySettings (the sink has no cancellation API), while
// Close remains bounded because Close never takes logGate.
func (m *ChatManager) ReconcileLogging(globalEnabled bool, logger ChatLogger) {
	m.loggingReconcileMu.Lock()
	defer m.loggingReconcileMu.Unlock()

	gate := m.logGate
	gate.mu.Lock()
	m.mu.Lock()

	targetLogger := m.logger
	if logger != nil {
		targetLogger = logger
	}
	semanticChange := globalEnabled != m.globalChatLogsOn ||
		(targetLogger == nil) != (m.logger == nil)
	m.globalChatLogsOn = globalEnabled
	m.logger = targetLogger
	gate.logger = targetLogger

	for _, client := range m.clients {
		desired := m.shouldLogChatLocked(client.streamer)
		if client.logChat != desired ||
			(client.logChat && (client.logGate != gate || client.logGeneration != m.logGeneration)) {
			semanticChange = true
			break
		}
	}
	if !semanticChange {
		for _, reservation := range m.pending {
			desired := m.shouldLogChatLocked(reservation.streamer)
			if reservation.generation != m.logGeneration || reservation.candidate.logChat != desired {
				semanticChange = true
				break
			}
		}
	}

	var reservations []*clientReservation
	var retirements []*clientRetirement
	if semanticChange {
		m.logGeneration++
		gate.generation = m.logGeneration

		// Cancel every in-flight pre-publication candidate. A still-desired
		// ordinary join is immediately re-reserved with the current generation;
		// its stale late dial can only close, never install.
		pending := make([]*clientReservation, 0, len(m.pending))
		for _, reservation := range m.pending {
			pending = append(pending, reservation)
		}
		for _, reservation := range pending {
			if m.pending[reservation.login] != reservation {
				continue
			}
			delete(m.pending, reservation.login)
			retirements = append(retirements, m.retireClientsLocked([]*IRCClient{reservation.old, reservation.candidate})...)

			rosterCurrent := reservation.login == reservation.streamer.GetUsername() &&
				(m.rosterMembership == nil || m.rosterMembership(reservation.streamer))
			if rosterCurrent && presenceDesired(reservation.streamer, reservation.preservePresence) {
				fresh := m.newReservationLocked(
					reservation.login,
					reservation.streamer,
					reservation.old,
					reservation.preservePresence,
					reservation.retryOnFailure,
				)
				reservations = append(reservations, fresh)
			}
		}

		for login, client := range m.clients {
			desired := m.shouldLogChatLocked(client.streamer)
			currentLogin := client.streamer.GetUsername()
			rosterCurrent := login == currentLogin &&
				(m.rosterMembership == nil || m.rosterMembership(client.streamer))

			// false->false sessions own neither CAP nor structured-write
			// authority, so they remain valid across a generation bump. Every
			// session that logged before or must log now gets a fresh immutable
			// generation. Invalid roster/login generations are detached and
			// retired without replacement.
			if rosterCurrent && !client.logChat && !desired {
				continue
			}

			client.revoke()
			if !rosterCurrent {
				delete(m.clients, login)
				retirements = append(retirements, m.retireClientsLocked([]*IRCClient{client})...)
				continue
			}
			delete(m.clients, login)
			replacement := m.newReservationLocked(login, client.streamer, client, true, true)
			reservations = append(reservations, replacement)
			retirements = append(retirements, m.retireClientsLocked([]*IRCClient{client})...)
		}
	}

	m.mu.Unlock()
	gate.mu.Unlock()

	// All stale write authority was revoked under the drained gate. The
	// bounded transport/reconnect stop can now run concurrently without
	// weakening the structured-write boundary.
	m.waitClientRetirements(retirements)
	for _, reservation := range reservations {
		m.connectReservation(reservation)
	}
}

// connectReservation performs dial/auth/JOIN outside every manager/gate lock.
// The candidate remains unable to write until the final exact-ticket/current-
// generation CAS installs it. A logging replacement whose first Connect fails
// keeps a desired-generation owner and starts the existing reconnect loop; the
// external transport can remain unavailable, but stale authority cannot return.
func (m *ChatManager) connectReservation(reservation *clientReservation) {
	m.mu.Lock()
	current := m.pending[reservation.login]
	if current != reservation || m.closed || reservation.generation != m.logGeneration ||
		reservation.streamer.GetUsername() != reservation.login ||
		(m.rosterMembership != nil && !m.rosterMembership(reservation.streamer)) ||
		!presenceDesired(reservation.streamer, reservation.preservePresence) {
		if current == reservation {
			delete(m.pending, reservation.login)
		}
		m.mu.Unlock()
		return
	}
	candidate := reservation.candidate
	m.mu.Unlock()

	err := candidate.Connect()

	m.mu.Lock()
	current = m.pending[reservation.login]
	ownerValid := current == reservation && !m.closed &&
		reservation.generation == m.logGeneration && m.clients[reservation.login] == nil &&
		reservation.streamer.GetUsername() == reservation.login &&
		(m.rosterMembership == nil || m.rosterMembership(reservation.streamer)) &&
		presenceDesired(reservation.streamer, reservation.preservePresence) &&
		m.shouldLogChatLocked(reservation.streamer) == candidate.logChat
	if current == reservation {
		delete(m.pending, reservation.login)
	}
	var starter func()
	install := false
	if ownerValid && (err == nil || reservation.retryOnFailure) {
		// Publish the exact pointer before granting lifecycle authority. activate
		// only admits WaitGroup work and returns a starter; actual read/reconnect
		// work begins after m.mu is released below.
		m.clients[reservation.login] = candidate
		starter, install = candidate.activate(err != nil)
		if !install {
			delete(m.clients, reservation.login)
		}
	}
	var retirements []*clientRetirement
	if !install {
		retirements = m.retireClientsLocked([]*IRCClient{candidate})
	}
	m.mu.Unlock()
	if starter != nil {
		starter()
	}

	if err != nil {
		slog.Error("Failed to establish IRC chat generation", "channel", reservation.login, "reconnecting", install, "error", err)
	}
	m.waitClientRetirements(retirements)
}

// presenceDesired resolves the existing tri-state presence contract. The
// existing argument is true for a logging replacement, so UNKNOWN preserves
// that already-established session; callers never use it to create an absent
// UNKNOWN join.
func presenceDesired(streamer *models.Streamer, existing bool) bool {
	switch streamer.GetSettings().Chat {
	case models.ChatAlways:
		return true
	case models.ChatNever:
		return false
	case models.ChatOnline:
		switch streamer.GetStatus() {
		case models.StatusOnline:
			return true
		case models.StatusOffline:
			return false
		default:
			return existing
		}
	case models.ChatOffline:
		switch streamer.GetStatus() {
		case models.StatusOffline:
			return true
		case models.StatusOnline:
			return false
		default:
			return existing
		}
	default:
		return false
	}
}

// Close stops every IRC client and JOINS their read loops (S1), so an
// in-flight chat-message analytics write finishes before the caller proceeds
// toward closing the shared database handle. The joins run with mu RELEASED —
// a read loop's handler path never takes ChatManager.mu today, but holding a
// manager-wide lock across N bounded waits would still stall every concurrent
// ToggleChat for up to the join bound — and CONCURRENTLY, so the manager's
// worst case is one per-client stopJoinTimeout, not len(clients) times it.
// Join timeouts are aggregated into the returned explicit shutdown error;
// Close itself never hangs. Idempotent (a repeated Close is a nil no-op);
// joinChat refuses new clients once closed.
func (m *ChatManager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	clients := make([]*IRCClient, 0, len(m.clients))
	for _, client := range m.clients {
		clients = append(clients, client)
	}
	for _, replacement := range m.pending {
		if replacement.old != nil {
			clients = append(clients, replacement.old)
		}
		if replacement.candidate != nil {
			clients = append(clients, replacement.candidate)
		}
	}
	m.clients = make(map[string]*IRCClient)
	m.pending = make(map[string]*clientReservation)
	retirements := make([]*clientRetirement, 0, len(m.retirements)+len(clients))
	for _, retirement := range m.retirements {
		retirements = append(retirements, retirement)
	}
	retirements = append(retirements, m.retireClientsLocked(clients)...)
	m.mu.Unlock()

	m.waitClientRetirements(retirements)
	seen := make(map[*clientRetirement]bool, len(retirements))
	errs := make([]error, 0, len(retirements))
	for _, retirement := range retirements {
		if retirement != nil && !seen[retirement] {
			seen[retirement] = true
			errs = append(errs, retirement.err)
		}
	}
	return errors.Join(errs...)
}
