package chat

import (
	"log/slog"
	"sync"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

type ChatManager struct {
	username         string
	token            string
	clients          map[string]*IRCClient
	logger           ChatLogger
	globalChatLogsOn bool
	mentionHandler   MentionHandler

	mu sync.RWMutex
}

func NewChatManager(username, token string, logger ChatLogger, globalChatLogsOn bool, mentionHandler MentionHandler) *ChatManager {
	return &ChatManager{
		username:         username,
		token:            token,
		clients:          make(map[string]*IRCClient),
		logger:           logger,
		globalChatLogsOn: globalChatLogsOn,
		mentionHandler:   mentionHandler,
	}
}

func (m *ChatManager) ToggleChat(streamer *models.Streamer) {
	switch streamer.Settings.Chat {
	case models.ChatAlways:
		m.joinChat(streamer)
	case models.ChatNever:
		m.leaveChat(streamer)
	case models.ChatOnline:
		if streamer.GetIsOnline() {
			m.joinChat(streamer)
		} else {
			m.leaveChat(streamer)
		}
	case models.ChatOffline:
		if streamer.GetIsOnline() {
			m.leaveChat(streamer)
		} else {
			m.joinChat(streamer)
		}
	}
}

func (m *ChatManager) shouldLogChat(streamer *models.Streamer) bool {
	if streamer.Settings.ChatLogs != nil {
		return *streamer.Settings.ChatLogs
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
	client := NewIRCClient(m.username, m.token, streamer, m.logger, logChat, m.mentionHandler)
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
