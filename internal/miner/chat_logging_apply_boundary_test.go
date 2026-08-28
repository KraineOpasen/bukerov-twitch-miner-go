package miner

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/chat"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// c2ApplyBoundaryChat models the one bit of immutable client-generation
// authority relevant at the Miner boundary. It deliberately implements the
// production ReconcileLogging method as an extra method, so this test compiles
// on the untouched base (where the old chatToggler slice never invokes it) and
// fails there when a post-ApplySettings write remains authorized.
type c2ApplyBoundaryChat struct {
	authorized atomic.Bool
	writes     atomic.Int32
}

func (c *c2ApplyBoundaryChat) ToggleChat(*models.Streamer) {}
func (c *c2ApplyBoundaryChat) Leave(string)                {}
func (c *c2ApplyBoundaryChat) ReconcileLogging(globalEnabled bool, _ chat.ChatLogger) {
	c.authorized.Store(globalEnabled)
}
func (c *c2ApplyBoundaryChat) attemptStructuredWrite() {
	if c.authorized.Load() {
		c.writes.Add(1)
	}
}

func TestApplySettingsReturnRevokesPostBoundaryStructuredWrite(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "alpha")
	m.config.EnableAnalytics = true
	m.config.Analytics.EnableChatLogs = true
	authority := &c2ApplyBoundaryChat{}
	authority.authorized.Store(true)
	m.chatPresence = authority

	runtimeSettings := m.GetRuntimeSettings()
	runtimeSettings.Analytics.EnableChatLogs = false
	if err := m.ApplySettings(context.Background(), runtimeSettings); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}

	authority.attemptStructuredWrite() // explicitly after successful return
	if got := authority.writes.Load(); got != 0 {
		t.Fatalf("post-ApplySettings stale structured writes = %d, want 0", got)
	}
}
