package chat

import (
	"fmt"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

func c4OptionalBool(value *bool) string {
	if value == nil {
		return "inherit"
	}
	return fmt.Sprintf("explicit_%t", *value)
}

func c4Bool(value bool) *bool {
	return &value
}

func c4LoggingFixture(t *testing.T, global bool, override *bool) (*ChatManager, *fakeTransport, *models.Streamer, *IRCClient, []string, *c2RecordingChatLogger) {
	t.Helper()
	logger := &c2RecordingChatLogger{}
	transport := newFakeTransport()
	manager := NewChatManager("miner", StaticToken("synthetic-token"), logger, global, nil)
	manager.dialFn = transport.dial
	t.Cleanup(func() { _ = manager.Close() })

	streamer := streamerWithChat("somechannel", models.ChatAlways)
	settings := streamer.GetSettings()
	settings.ChatLogs = override
	streamer.SetSettings(settings)
	manager.ToggleChat(streamer)
	_ = recvServer(t, transport)
	handshake := c2HandshakeUntilJoin(t, transport)
	client := manager.clientPtr("somechannel")
	if client == nil {
		t.Fatal("chat client was not installed")
	}
	return manager, transport, streamer, client, handshake, logger
}

func TestC4ChatLoggingSixStatePolicyMatrixWithAvailableSink(t *testing.T) {
	tests := []struct {
		global   bool
		override *bool
		wantLog  bool
	}{
		{global: false, override: nil, wantLog: false},
		{global: false, override: c4Bool(false), wantLog: false},
		{global: false, override: c4Bool(true), wantLog: true},
		{global: true, override: nil, wantLog: true},
		{global: true, override: c4Bool(false), wantLog: false},
		{global: true, override: c4Bool(true), wantLog: true},
	}

	for _, tc := range tests {
		name := fmt.Sprintf("global_%t/%s", tc.global, c4OptionalBool(tc.override))
		t.Run(name, func(t *testing.T) {
			_, _, _, client, handshake, logger := c4LoggingFixture(t, tc.global, tc.override)
			if client.logChat != tc.wantLog {
				t.Fatalf("client.logChat = %t, want %t", client.logChat, tc.wantLog)
			}
			if got := c2HasCAP(handshake); got != tc.wantLog {
				t.Fatalf("logging CAP present = %t, want %t; handshake=%v", got, tc.wantLog, handshake)
			}

			client.handleMessage(":alice!u@h PRIVMSG #somechannel :matrix-message")
			wantWrites := 0
			if tc.wantLog {
				wantWrites = 1
			}
			if got := logger.count(); got != wantWrites {
				t.Fatalf("structured writes = %d, want %d", got, wantWrites)
			}
		})
	}
}

func TestC4ExplicitEnableIsolatedAndDisableFenced(t *testing.T) {
	logger := &c2RecordingChatLogger{}
	transport := newFakeTransport()
	manager := NewChatManager("miner", StaticToken("synthetic-token"), logger, false, nil)
	manager.dialFn = transport.dial
	t.Cleanup(func() { _ = manager.Close() })

	enabled := true
	streamerA := streamerWithChat("alpha", models.ChatAlways)
	settingsA := streamerA.GetSettings()
	settingsA.ChatLogs = &enabled
	streamerA.SetSettings(settingsA)
	streamerB := streamerWithChat("beta", models.ChatAlways)

	manager.ToggleChat(streamerA)
	_ = recvServer(t, transport)
	handshakeA := c2HandshakeUntilJoin(t, transport)
	manager.ToggleChat(streamerB)
	_ = recvServer(t, transport)
	handshakeB := c2HandshakeUntilJoin(t, transport)
	if !c2HasCAP(handshakeA) || c2HasCAP(handshakeB) {
		t.Fatalf("initial CAP isolation failed: alpha=%v beta=%v", handshakeA, handshakeB)
	}

	oldA := manager.clientPtr("alpha")
	oldB := manager.clientPtr("beta")
	oldA.handleMessage(":alice!u@h PRIVMSG #alpha :enabled-only")
	oldB.handleMessage(":bob!u@h PRIVMSG #beta :inherited-off")
	if got := logger.count(); got != 1 {
		t.Fatalf("isolated writes = %d, want 1", got)
	}

	dials := transport.dials()
	c2ApplyLogging(manager, false, logger, streamerA, streamerB)
	if got := transport.dials(); got != dials {
		t.Fatalf("identical policy reconciliation dials = %d, want %d", got, dials)
	}
	if manager.clientPtr("alpha") != oldA || manager.clientPtr("beta") != oldB {
		t.Fatal("identical policy reconciliation replaced a current client")
	}

	disabled := false
	settingsA = streamerA.GetSettings()
	settingsA.ChatLogs = &disabled
	streamerA.SetSettings(settingsA)
	c2ApplyLogging(manager, false, logger, streamerA, streamerB)
	if got := transport.dials(); got != dials+1 {
		t.Fatalf("explicit true->false replacement dials = %d, want %d", got, dials+1)
	}
	_ = recvServer(t, transport)
	disabledHandshake := c2HandshakeUntilJoin(t, transport)
	if c2HasCAP(disabledHandshake) {
		t.Fatalf("disabled replacement retained logging CAP: %v", disabledHandshake)
	}
	currentA := manager.clientPtr("alpha")
	if currentA == nil || currentA == oldA {
		t.Fatal("explicit true->false did not install a fresh fenced generation")
	}

	oldA.handleMessage(":alice!u@h PRIVMSG #alpha :stale-after-disable")
	currentA.handleMessage(":alice!u@h PRIVMSG #alpha :current-after-disable")
	manager.clientPtr("beta").handleMessage(":bob!u@h PRIVMSG #beta :still-inherited-off")
	if got := logger.count(); got != 1 {
		t.Fatalf("post-disable structured writes = %d, want the original 1 only", got)
	}
}
