package chat

import (
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

	// dialFn, when set, is handed to every IRC client this manager creates. It
	// is nil in production (clients dial Twitch IRC over TLS); tests inject an
	// in-memory transport so presence transitions run without a network.
	dialFn func() (net.Conn, error)

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

	if client, exists := m.clients[streamer.Username]; exists {
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
		slog.Error("Failed to join IRC chat", "channel", streamer.Username, "error", err)
		return
	}

	m.clients[streamer.Username] = client
}

func (m *ChatManager) leaveChat(streamer *models.Streamer) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if client, exists := m.clients[streamer.Username]; exists {
		client.Stop()
		delete(m.clients, streamer.Username)
	}
}

func (m *ChatManager) Leave(username string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if client, exists := m.clients[username]; exists {
		client.Stop()
		delete(m.clients, username)
	}
}

func (m *ChatManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, client := range m.clients {
		client.Stop()
	}
	m.clients = make(map[string]*IRCClient)
}
