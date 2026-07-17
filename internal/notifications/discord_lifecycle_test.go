package notifications

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/bwmarrin/discordgo"
)

// --- Disconnect error on a credential change must not commit a false success ---

func TestDiscordConfigDisconnectFailureDoesNotCommitChange(t *testing.T) {
	start := config.DiscordSettings{Enabled: true, BotToken: "old-token", GuildID: "guild"}
	m, ff := newManager(t, start)

	fake := &fakeDiscord{connected: true, botToken: "old-token", guildID: "guild"}
	// The first Disconnect (triggered by the token change) fails; the second
	// succeeds.
	fake.disconnectErrs = []error{errors.New("disconnect boom")}
	m.discord = fake

	buf := captureLogs(t)
	changed := config.DiscordSettings{Enabled: true, BotToken: "new-token", GuildID: "guild"}

	// First apply: Disconnect fails -> reconcile aborts before UpdateConfig/Connect.
	if err := m.UpdateDiscordConfig(&changed); err == nil {
		t.Fatalf("expected the Disconnect error to surface")
	}
	connect, disconnect, updateCfg, connected := fake.counts()
	if disconnect != 1 || updateCfg != 0 || connect != 0 {
		t.Fatalf("after failed Disconnect want disc=1,upd=0,conn=0, got disc=%d upd=%d conn=%d",
			disconnect, updateCfg, connect)
	}
	if connected {
		t.Fatalf("provider must be detached (not connected) after Disconnect")
	}
	if strings.Contains(buf.String(), "updated and reconnected") {
		t.Fatalf("a failed reconcile must not log a reconnect success: %q", buf.String())
	}

	// Second apply of the same new-token config: reconcile runs again (not a
	// no-op), Disconnect now succeeds, then UpdateConfig + Connect.
	if err := m.UpdateDiscordConfig(&changed); err != nil {
		t.Fatalf("retry should succeed: %v", err)
	}
	connect, disconnect, updateCfg, connected = fake.counts()
	if disconnect != 2 || updateCfg != 1 || connect != 1 {
		t.Fatalf("retry want disc=2,upd=1,conn=1, got disc=%d upd=%d conn=%d",
			disconnect, updateCfg, connect)
	}
	if !connected {
		t.Fatalf("provider should be connected after the successful retry")
	}
	if ff.creationCount() != 0 {
		t.Fatalf("existing provider should be reused, got %d creations", ff.creationCount())
	}
	if strings.Contains(buf.String(), "old-token") || strings.Contains(buf.String(), "new-token") {
		t.Fatalf("bot token must never be logged; found it in: %q", buf.String())
	}
}

// --- Disconnect failure then Connect failure both stay retryable ---

func TestDiscordConfigDisconnectAndConnectFailuresRemainRetryable(t *testing.T) {
	start := config.DiscordSettings{Enabled: true, BotToken: "old-token", GuildID: "guild"}
	m, _ := newManager(t, start)

	fake := &fakeDiscord{connected: true, botToken: "old-token", guildID: "guild"}
	fake.disconnectErrs = []error{errors.New("disconnect boom")} // first Disconnect fails
	fake.connectErrs = []error{errors.New("connect boom"), nil}  // first Connect fails, second ok
	m.discord = fake

	changed := config.DiscordSettings{Enabled: true, BotToken: "new-token", GuildID: "guild"}

	// 1) Disconnect fails.
	if err := m.UpdateDiscordConfig(&changed); err == nil {
		t.Fatalf("apply 1: expected Disconnect error")
	}
	if c, d, u, _ := fake.counts(); c != 0 || d != 1 || u != 0 {
		t.Fatalf("apply 1 want conn=0,disc=1,upd=0, got conn=%d disc=%d upd=%d", c, d, u)
	}

	// 2) Disconnect succeeds, Connect fails.
	if err := m.UpdateDiscordConfig(&changed); err == nil {
		t.Fatalf("apply 2: expected Connect error")
	}
	if c, d, u, conn := fake.counts(); c != 1 || d != 2 || u != 1 || conn {
		t.Fatalf("apply 2 want conn=1,disc=2,upd=1,connected=false, got conn=%d disc=%d upd=%d connected=%v", c, d, u, conn)
	}

	// 3) Same config again: unchanged (already stored) + disconnected -> Connect
	// recovery only, and it succeeds. No false no-op.
	if err := m.UpdateDiscordConfig(&changed); err != nil {
		t.Fatalf("apply 3 should succeed: %v", err)
	}
	if c, d, u, conn := fake.counts(); c != 2 || d != 2 || u != 1 || !conn {
		t.Fatalf("apply 3 want conn=2,disc=2,upd=1,connected=true, got conn=%d disc=%d upd=%d connected=%v", c, d, u, conn)
	}
}

// --- Disable with a Disconnect error keeps a retryable provider ---

func TestDiscordDisableDisconnectFailureRetainsRetryableProvider(t *testing.T) {
	start := config.DiscordSettings{Enabled: true, BotToken: "tok", GuildID: "guild"}
	m, ff := newManager(t, start)

	fake := &fakeDiscord{connected: true, botToken: "tok", guildID: "guild"}
	fake.disconnectErrs = []error{errors.New("disconnect boom")} // first disable fails
	m.discord = fake

	buf := captureLogs(t)
	disabled := config.DiscordSettings{Enabled: false}

	// First disable: Disconnect fails -> error, provider kept, nothing logged.
	if err := m.UpdateDiscordConfig(&disabled); err == nil {
		t.Fatalf("expected the disable Disconnect error to surface")
	}
	if m.discord == nil {
		t.Fatalf("provider must be retained after a failed disable")
	}
	if _, d, _, _ := fake.counts(); d != 1 {
		t.Fatalf("want 1 Disconnect, got %d", d)
	}

	// Second disable: Disconnect succeeds -> provider cleared, logged once.
	if err := m.UpdateDiscordConfig(&disabled); err != nil {
		t.Fatalf("second disable should succeed: %v", err)
	}
	if m.discord != nil {
		t.Fatalf("provider must be cleared after a successful disable")
	}
	if _, d, _, _ := fake.counts(); d != 2 {
		t.Fatalf("want 2 Disconnect, got %d", d)
	}
	if n := strings.Count(buf.String(), "Discord notifications disabled"); n != 1 {
		t.Fatalf("expected exactly one disabled log, got %d in %q", n, buf.String())
	}
	if ff.creationCount() != 0 {
		t.Fatalf("disable must not create a provider, got %d", ff.creationCount())
	}
}

// --- Provider Disconnect detaches state before the (failing) Close ---

func TestDiscordProviderDisconnectClearsStateBeforeClose(t *testing.T) {
	p := NewDiscordProvider("tok", "guild")

	// Simulate an established session.
	p.mu.Lock()
	p.session = &discordgo.Session{}
	p.mu.Unlock()

	closeErr := errors.New("close failed")
	muFreeDuringClose := false
	p.closeSession = func(_ *discordgo.Session) error {
		// The provider lock must NOT be held while the network Close runs.
		if p.mu.TryLock() {
			muFreeDuringClose = true
			p.mu.Unlock()
		}
		return closeErr
	}

	if !p.IsConnected() {
		t.Fatalf("provider should report connected before Disconnect")
	}

	err := p.Disconnect()
	if !errors.Is(err, closeErr) {
		t.Fatalf("Disconnect should return the Close error, got %v", err)
	}
	if !muFreeDuringClose {
		t.Fatalf("d.mu must not be held while closeSession runs")
	}
	if p.IsConnected() {
		t.Fatalf("provider must report NOT connected after Disconnect, even on Close error")
	}
}

// --- Discord network lifecycle never runs under Manager.mu ---

func TestDiscordLifecycleRunsWithoutManagerLock(t *testing.T) {
	// probe records whether Manager.mu was held when a provider network op ran.
	// TryLock on a RWMutex is non-reentrant: if the calling goroutine already
	// holds m.mu, TryLock fails, so `held` becomes true.
	newProbe := func(m *Manager, held *bool) func() {
		return func() {
			if m.mu.TryLock() {
				m.mu.Unlock()
			} else {
				*held = true
			}
		}
	}

	base := config.DiscordSettings{Enabled: true, BotToken: "tok", GuildID: "guild"}

	t.Run("UpdateDiscordConfig Connect", func(t *testing.T) {
		m, _ := newManager(t, base)
		var held bool
		fake := &fakeDiscord{connected: false, botToken: "tok", guildID: "guild"}
		fake.onLifecycle = newProbe(m, &held)
		m.discord = fake
		mustUpdate(t, m, base) // unchanged + disconnected -> Connect
		if held {
			t.Fatalf("Manager.mu was held during UpdateDiscordConfig Connect")
		}
	})

	t.Run("UpdateDiscordConfig Disconnect", func(t *testing.T) {
		m, _ := newManager(t, base)
		var held bool
		fake := &fakeDiscord{connected: true, botToken: "tok", guildID: "guild"}
		fake.onLifecycle = newProbe(m, &held)
		m.discord = fake
		// Token change -> Disconnect then Connect, both probed.
		mustUpdate(t, m, config.DiscordSettings{Enabled: true, BotToken: "tok2", GuildID: "guild"})
		if held {
			t.Fatalf("Manager.mu was held during a provider network call")
		}
	})

	t.Run("Start Connect", func(t *testing.T) {
		m, _ := newManager(t, base)
		var held bool
		fake := &fakeDiscord{connected: false, botToken: "tok", guildID: "guild"}
		fake.onLifecycle = newProbe(m, &held)
		m.discord = fake
		if err := m.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if held {
			t.Fatalf("Manager.mu was held during Start Connect")
		}
	})

	t.Run("Stop Disconnect", func(t *testing.T) {
		m, _ := newManager(t, base)
		var held bool
		fake := &fakeDiscord{connected: true, botToken: "tok", guildID: "guild"}
		fake.onLifecycle = newProbe(m, &held)
		m.discord = fake
		m.Stop()
		if held {
			t.Fatalf("Manager.mu was held during Stop Disconnect")
		}
	})
}

// --- UpdateConfig invalidates the channel cache on a credential change ---

func seedChannelCache(p *DiscordProvider) {
	p.mu.Lock()
	p.channelCache = []Channel{{ID: "1", Name: "general", Type: "text"}}
	p.channelCacheTime = time.Unix(1_000_000, 0)
	p.mu.Unlock()
}

func cacheState(p *DiscordProvider) (cache []Channel, ts time.Time) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.channelCache, p.channelCacheTime
}

func TestDiscordUpdateConfigInvalidatesCacheOnGuildChange(t *testing.T) {
	p := NewDiscordProvider("tok", "guild-old")
	seedChannelCache(p)
	p.UpdateConfig("tok", "guild-new")
	cache, ts := cacheState(p)
	if cache != nil {
		t.Fatalf("channel cache must be cleared on guild change, got %v", cache)
	}
	if !ts.IsZero() {
		t.Fatalf("cache timestamp must be reset on guild change, got %v", ts)
	}
}

func TestDiscordUpdateConfigInvalidatesCacheOnTokenChange(t *testing.T) {
	p := NewDiscordProvider("tok-old", "guild")
	seedChannelCache(p)
	p.UpdateConfig("tok-new", "guild")
	cache, ts := cacheState(p)
	if cache != nil {
		t.Fatalf("channel cache must be cleared on token change, got %v", cache)
	}
	if !ts.IsZero() {
		t.Fatalf("cache timestamp must be reset on token change, got %v", ts)
	}
}

func TestDiscordUpdateConfigUnchangedPreservesCache(t *testing.T) {
	p := NewDiscordProvider("tok", "guild")
	seedChannelCache(p)
	p.UpdateConfig("tok", "guild") // identical credentials
	cache, ts := cacheState(p)
	if cache == nil {
		t.Fatalf("channel cache must be preserved when credentials are unchanged")
	}
	if ts.IsZero() {
		t.Fatalf("cache timestamp must be preserved when credentials are unchanged")
	}
}

// --- Static secret-safety gate: no struct-dump format verbs in the package ---

func TestNoSecretLeakingFormatDirectives(t *testing.T) {
	// The plus-v and hash-v format verbs serialize every struct field;
	// DiscordSettings carries BotToken, so they must never appear in this
	// package (production or tests). The forbidden literals are built by
	// concatenation so this file does not trip its own check.
	forbidden := []string{"%" + "+v", "%" + "#v"}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		src, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		text := string(src)
		for _, f := range forbidden {
			if strings.Contains(text, f) {
				t.Errorf("%s: forbidden secret-unsafe format verb %q (DiscordSettings carries BotToken)", e.Name(), f)
			}
		}
	}
}
