package notifications

import (
	"testing"

	"github.com/bwmarrin/discordgo"
)

// TestDiscordProviderIsConnected exercises the lock-safe connection-state check
// that the manager relies on for idempotent config updates. It never opens a
// real Discord connection: the "connected" state is simulated by attaching a
// bare *discordgo.Session (a non-nil session with no live websocket), which is
// exactly what IsConnected inspects.
func TestDiscordProviderIsConnected(t *testing.T) {
	p := NewDiscordProvider("tok", "guild")

	if p.IsConnected() {
		t.Fatalf("a freshly constructed provider must report not connected")
	}

	// Simulate an established session. A zero-value session has no open
	// websocket, so Disconnect's Close is a no-op that still clears it.
	p.mu.Lock()
	p.session = &discordgo.Session{}
	p.mu.Unlock()

	if !p.IsConnected() {
		t.Fatalf("provider with a live session must report connected")
	}

	if err := p.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}

	if p.IsConnected() {
		t.Fatalf("provider must report not connected after Disconnect")
	}
}
