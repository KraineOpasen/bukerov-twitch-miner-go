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

func NewChatManager(username string, tokenFn TokenProvider, logger ChatLogger, globalChatLogsOn bool, mentionHandler MentionHandler) *ChatManager {
	return &ChatManager{
		username:         username,
		tokenFn:          tokenFn,
		clients:          make(map[string]*IRCClient),
		logger:           logger,
		globalChatLogsOn: globalChatLogsOn,
		mentionHandler:   mentionHandler,
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
	// Snapshot under the streamer lock: Settings is replaced wholesale by a
	// runtime settings apply, concurrent with this reconciliation.
	switch streamer.GetSettings().Chat {
	case models.ChatAlways:
		m.joinChat(streamer)
	case models.ChatNever:
		m.leaveChat(streamer)
	case models.ChatOnline:
		// Join only on CONFIRMED online; leave only on CONFIRMED offline. On
		// UNKNOWN take no action, so an online→unknown blip does not tear down an
		// already-established IRC connection over transient uncertainty.
		switch streamer.GetStatus() {
		case models.StatusOnline:
			m.joinChat(streamer)
		case models.StatusOffline:
			m.leaveChat(streamer)
		}
	case models.ChatOffline:
		// Join only on CONFIRMED offline; leave on CONFIRMED online. UNKNOWN is
		// NOT treated as offline, so no offline-specific join happens on it.
		switch streamer.GetStatus() {
		case models.StatusOffline:
			m.joinChat(streamer)
		case models.StatusOnline:
			m.leaveChat(streamer)
		}
	}
}

func (m *ChatManager) shouldLogChat(streamer *models.Streamer) bool {
	if logs := streamer.GetSettings().ChatLogs; logs != nil {
		return *logs
	}
	return m.globalChatLogsOn
}

func (m *ChatManager) joinChat(streamer *models.Streamer) {
	m.mu.Lock()
	defer m.mu.Unlock()

	login := streamer.GetUsername()

	if m.closed {
		slog.Debug("Rejected joinChat: chat manager is shutting down", "channel", login)
		return
	}

	if m.rosterMembership != nil && !m.rosterMembership(streamer) {
		// A stale or foreign streamer pointer: the roster no longer tracks
		// THIS object under its login (removed, or it never belonged), so
		// joining here would create a ghost IRC client nothing will ever
		// leave. Reject before any client lookup/creation. Login only —
		// never any other streamer field — stays in the log line.
		slog.Debug("Rejected joinChat for non-roster streamer", "channel", login)
		return
	}

	if client, exists := m.clients[login]; exists {
		if client.IsRunning() {
			return
		}
	}

	logChat := m.shouldLogChat(streamer)
	client := NewIRCClient(m.username, m.tokenFn, streamer, m.logger, logChat, m.mentionHandler)
	client.authErrorFn = m.authErrorHandler
	if m.dialFn != nil {
		client.dialFn = m.dialFn
	}
	if err := client.Connect(); err != nil {
		slog.Error("Failed to join IRC chat", "channel", login, "error", err)
		return
	}

	m.clients[login] = client
}

func (m *ChatManager) leaveChat(streamer *models.Streamer) {
	login := streamer.GetUsername()

	m.mu.Lock()
	defer m.mu.Unlock()

	if client, exists := m.clients[login]; exists {
		// Runtime leave: the join outcome is advisory here (the client logs
		// its own timeout); only the shutdown path aggregates join errors.
		_ = client.Stop()
		delete(m.clients, login)
	}
}

func (m *ChatManager) Leave(username string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if client, exists := m.clients[username]; exists {
		_ = client.Stop()
		delete(m.clients, username)
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
	m.clients = make(map[string]*IRCClient)
	m.mu.Unlock()

	errs := make([]error, len(clients))
	var wg sync.WaitGroup
	for i, client := range clients {
		i, client := i, client
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = client.Stop()
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}
