package chat

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

func streamerWithChat(name string, mode models.ChatPresence) *models.Streamer {
	settings := models.DefaultStreamerSettings()
	settings.Chat = mode
	return models.NewStreamer(name, settings)
}

// seedConnection puts a client in the manager's map to stand in for an
// already-established IRC connection, so we can observe whether ToggleChat leaves
// it (removes it) or retains it.
func (m *ChatManager) seedConnection(s *models.Streamer) {
	m.clients[s.Username] = NewIRCClient(m.username, m.tokenFn, s, m.logger, false, m.mentionHandler)
}

func (m *ChatManager) hasConnection(username string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.clients[username]
	return ok
}

// TestChatOnlineUnknownKeepsConnection is the core continuity requirement: with
// ChatOnline, an online→unknown blip must NOT tear down an established IRC
// connection over transient uncertainty.
func TestChatOnlineUnknownKeepsConnection(t *testing.T) {
	m := NewChatManager("bot", func() string { return "tok" }, nil, false, nil)
	s := streamerWithChat("chan", models.ChatOnline)
	s.SetConfirmedOnline()
	m.seedConnection(s)

	s.SetUnknown(models.ReasonTransportError) // online -> unknown
	m.ToggleChat(s)

	if !m.hasConnection(s.Username) {
		t.Error("ChatOnline: online->unknown must retain the existing IRC connection")
	}
}

// TestChatOnlineOfflineLeaves confirms an authoritative offline still leaves chat.
func TestChatOnlineOfflineLeaves(t *testing.T) {
	m := NewChatManager("bot", func() string { return "tok" }, nil, false, nil)
	s := streamerWithChat("chan", models.ChatOnline)
	s.SetConfirmedOnline()
	m.seedConnection(s)

	s.SetConfirmedOffline()
	m.ToggleChat(s)

	if m.hasConnection(s.Username) {
		t.Error("ChatOnline: confirmed offline must leave chat")
	}
}

// TestChatOfflineUnknownNotTreatedAsOffline proves ChatOffline does not treat an
// unknown streamer as offline: it takes no join/leave action, so a connection
// established while confirmed-offline is retained through an unknown blip and no
// offline-specific action fires.
func TestChatOfflineUnknownNotTreatedAsOffline(t *testing.T) {
	m := NewChatManager("bot", func() string { return "tok" }, nil, false, nil)
	s := streamerWithChat("chan", models.ChatOffline)
	s.SetConfirmedOffline()
	m.seedConnection(s) // joined because it was confirmed offline

	s.SetUnknown(models.ReasonTransportError)
	m.ToggleChat(s)

	if !m.hasConnection(s.Username) {
		t.Error("ChatOffline: unknown must not be treated as offline (no leave), connection retained")
	}
}

// TestChatOfflineOnlineLeaves confirms ChatOffline leaves when confirmed online.
func TestChatOfflineOnlineLeaves(t *testing.T) {
	m := NewChatManager("bot", func() string { return "tok" }, nil, false, nil)
	s := streamerWithChat("chan", models.ChatOffline)
	s.SetConfirmedOffline()
	m.seedConnection(s)

	s.SetConfirmedOnline()
	m.ToggleChat(s)

	if m.hasConnection(s.Username) {
		t.Error("ChatOffline: confirmed online must leave chat")
	}
}
