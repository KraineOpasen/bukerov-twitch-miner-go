package notifications

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
)

// testDBHandle is the shared, process-wide database opened once for the whole
// package test run. database.Open is a singleton, so opening it under a
// package-lifetime temp dir (rather than a per-test t.TempDir that is deleted
// while the singleton still points at it) keeps the handle valid for every
// test. UpdateDiscordConfig never touches the repository; the DB exists only so
// NewManager — the production constructor these tests deliberately exercise —
// can build its repository.
var testDBHandle *database.DB

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "notif-discord-test-*")
	if err != nil {
		panic("mkdtemp: " + err.Error())
	}
	db, err := database.Open(dir)
	if err != nil {
		panic("open db: " + err.Error())
	}
	testDBHandle = db
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// fakeDiscord is a network-free stand-in for *DiscordProvider. It records how
// the manager drives the provider lifecycle and reports a purely local
// "connected" flag, so the idempotency behaviour can be asserted deterministically
// without any Discord gateway I/O. Every field is guarded by mu so the race
// detector stays clean.
type fakeDiscord struct {
	mu sync.Mutex

	botToken string
	guildID  string

	connectCalls    int
	disconnectCalls int
	updateCfgCalls  int
	connected       bool

	// connectErrs is consumed one entry per Connect call, front to back. A nil
	// entry — or an exhausted slice — means that Connect succeeds and marks the
	// provider connected; a non-nil entry fails and leaves it disconnected.
	connectErrs []error

	// disconnectErrs is consumed one entry per Disconnect call. Modelling the
	// real provider's detach-before-close, Disconnect always leaves the fake
	// disconnected; a non-nil entry is merely returned to the caller.
	disconnectErrs []error

	// onLifecycle, if set, is invoked at the very start of Connect and
	// Disconnect (the provider "network" operations) so a test can assert the
	// Manager lock is not held while they run.
	onLifecycle func()
}

func (f *fakeDiscord) Connect(_ context.Context) error {
	if f.onLifecycle != nil {
		f.onLifecycle()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connectCalls++
	if len(f.connectErrs) > 0 {
		err := f.connectErrs[0]
		f.connectErrs = f.connectErrs[1:]
		if err != nil {
			f.connected = false
			return err
		}
	}
	f.connected = true
	return nil
}

func (f *fakeDiscord) Disconnect() error {
	if f.onLifecycle != nil {
		f.onLifecycle()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconnectCalls++
	// Detach-before-close: the fake is never connected once Disconnect returns,
	// even when it reports a Close error.
	f.connected = false
	if len(f.disconnectErrs) > 0 {
		err := f.disconnectErrs[0]
		f.disconnectErrs = f.disconnectErrs[1:]
		return err
	}
	return nil
}

func (f *fakeDiscord) UpdateConfig(botToken, guildID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateCfgCalls++
	f.botToken = botToken
	f.guildID = guildID
}

func (f *fakeDiscord) IsConnected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}

func (f *fakeDiscord) Send(_ context.Context, _ Notification) error { return nil }

func (f *fakeDiscord) GetChannels(_ context.Context, _ bool) ([]Channel, error) {
	return nil, nil
}

// counts returns the lifecycle counters and connected state atomically.
func (f *fakeDiscord) counts() (connect, disconnect, updateCfg int, connected bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connectCalls, f.disconnectCalls, f.updateCfgCalls, f.connected
}

// token returns the currently configured bot token (as last set via the
// literal or UpdateConfig).
func (f *fakeDiscord) token() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.botToken
}

// fakeFactory records how many providers the manager built through the
// injected factory seam and lets a test pre-seed the connect outcome of each
// provider it will create.
type fakeFactory struct {
	mu        sync.Mutex
	creations int
	created   []*fakeDiscord

	// connectErrsByCreation[i] is applied to the i-th provider this factory
	// builds. A missing index means "no injected errors".
	connectErrsByCreation [][]error
}

func (ff *fakeFactory) make(botToken, guildID string) discordProvider {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	f := &fakeDiscord{botToken: botToken, guildID: guildID}
	if ff.creations < len(ff.connectErrsByCreation) {
		f.connectErrs = ff.connectErrsByCreation[ff.creations]
	}
	ff.creations++
	ff.created = append(ff.created, f)
	return f
}

func (ff *fakeFactory) creationCount() int {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	return ff.creations
}

// newManager builds a Manager through the production NewManager constructor
// (so the by-value config copy is exercised exactly as in production), then
// routes all subsequent provider creation through the counting fake factory and
// detaches any provider NewManager may have eagerly built. The caller attaches
// whatever provider state the case requires via m.discord. No test ever touches
// m.discordConfig directly, which keeps this file compiling regardless of that
// field's type.
func newManager(t *testing.T, initial config.DiscordSettings) (*Manager, *fakeFactory) {
	t.Helper()
	ff := &fakeFactory{}
	m, err := NewManager(&initial, nil, testDBHandle, nil, "")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	m.discord = nil
	m.newDiscord = ff.make
	return m, ff
}

// captureLogs redirects the default slog logger to a buffer for the duration of
// the test so secret-leak assertions can inspect everything that was logged.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func mustUpdate(t *testing.T, m *Manager, cfg config.DiscordSettings) {
	t.Helper()
	// Never format the whole DiscordSettings (it carries BotToken); log only the
	// non-secret fields.
	if err := m.UpdateDiscordConfig(&cfg); err != nil {
		t.Fatalf("UpdateDiscordConfig(enabled=%v guildID=%q): %v", cfg.Enabled, cfg.GuildID, err)
	}
}

// --- Test 1: CASE D — unchanged config + connected provider is a true no-op ---

func TestDiscordConfigUnchangedConnectedIsNoop(t *testing.T) {
	cfg := config.DiscordSettings{Enabled: true, BotToken: "tok", GuildID: "guild"}
	m, ff := newManager(t, cfg)

	fake := &fakeDiscord{connected: true, botToken: "tok", guildID: "guild"}
	m.discord = fake

	buf := captureLogs(t)
	mustUpdate(t, m, cfg) // identical config

	connect, disconnect, updateCfg, _ := fake.counts()
	if connect != 0 || disconnect != 0 || updateCfg != 0 {
		t.Fatalf("expected no lifecycle calls, got connect=%d disconnect=%d updateCfg=%d",
			connect, disconnect, updateCfg)
	}
	if ff.creationCount() != 0 {
		t.Fatalf("expected 0 provider creations, got %d", ff.creationCount())
	}
	if m.discord != discordProvider(fake) {
		t.Fatalf("provider object must not be replaced on a no-op")
	}
	if strings.Contains(buf.String(), "updated and reconnected") {
		t.Fatalf("no-op must not log a reconnect: %q", buf.String())
	}
}

// --- Test 2: CASE E — unchanged config + disconnected provider reconnects ---

func TestDiscordConfigUnchangedDisconnectedReconnects(t *testing.T) {
	cfg := config.DiscordSettings{Enabled: true, BotToken: "tok", GuildID: "guild"}
	m, ff := newManager(t, cfg)

	fake := &fakeDiscord{connected: false, botToken: "tok", guildID: "guild"}
	m.discord = fake

	mustUpdate(t, m, cfg) // identical config, but session is down

	connect, disconnect, _, connected := fake.counts()
	if connect != 1 {
		t.Fatalf("expected exactly 1 Connect (recovery), got %d", connect)
	}
	if disconnect != 0 {
		t.Fatalf("expected 0 Disconnect (nothing to tear down), got %d", disconnect)
	}
	if !connected {
		t.Fatalf("provider should be connected after recovery")
	}
	if ff.creationCount() != 0 {
		t.Fatalf("existing provider should be reused, got %d creations", ff.creationCount())
	}
}

// --- Test 3: CASE F — unchanged config + nil provider creates and connects ---

func TestDiscordConfigUnchangedProviderNilConnects(t *testing.T) {
	cfg := config.DiscordSettings{Enabled: true, BotToken: "tok", GuildID: "guild"}
	m, ff := newManager(t, cfg) // helper leaves m.discord == nil

	mustUpdate(t, m, cfg) // stored config already enabled; provider missing

	if ff.creationCount() != 1 {
		t.Fatalf("expected 1 provider creation, got %d", ff.creationCount())
	}
	created := ff.created[0]
	connect, disconnect, _, connected := created.counts()
	if connect != 1 {
		t.Fatalf("expected exactly 1 Connect, got %d", connect)
	}
	if disconnect != 0 {
		t.Fatalf("expected 0 Disconnect, got %d", disconnect)
	}
	if !connected {
		t.Fatalf("newly created provider should be connected")
	}
}

// --- Test 4: CASE G — bot token change disconnects and reconnects ---

func TestDiscordConfigTokenChangeReconnects(t *testing.T) {
	const secret = "brand-new-secret-token"
	start := config.DiscordSettings{Enabled: true, BotToken: "old-token", GuildID: "guild"}
	m, ff := newManager(t, start)

	fake := &fakeDiscord{connected: true, botToken: "old-token", guildID: "guild"}
	m.discord = fake

	buf := captureLogs(t)
	mustUpdate(t, m, config.DiscordSettings{Enabled: true, BotToken: secret, GuildID: "guild"})

	connect, disconnect, updateCfg, connected := fake.counts()
	if disconnect != 1 || updateCfg != 1 || connect != 1 {
		t.Fatalf("expected reconnect (disc=1,upd=1,conn=1), got disc=%d upd=%d conn=%d",
			disconnect, updateCfg, connect)
	}
	if !connected {
		t.Fatalf("provider should be connected after token change")
	}
	if ff.creationCount() != 0 {
		t.Fatalf("existing provider should be reused, got %d creations", ff.creationCount())
	}
	if got := fake.token(); got != secret {
		t.Fatalf("provider not re-pointed at new token: got %q", got)
	}
	if strings.Contains(buf.String(), secret) {
		t.Fatalf("bot token must never be logged; found it in: %q", buf.String())
	}
}

// --- Test 5: CASE H — guild ID change disconnects and reconnects ---

func TestDiscordConfigGuildChangeReconnects(t *testing.T) {
	const secret = "sensitive-bot-token"
	start := config.DiscordSettings{Enabled: true, BotToken: secret, GuildID: "old-guild"}
	m, ff := newManager(t, start)

	fake := &fakeDiscord{connected: true, botToken: secret, guildID: "old-guild"}
	m.discord = fake

	buf := captureLogs(t)
	mustUpdate(t, m, config.DiscordSettings{Enabled: true, BotToken: secret, GuildID: "new-guild"})

	connect, disconnect, updateCfg, connected := fake.counts()
	if disconnect != 1 || updateCfg != 1 || connect != 1 {
		t.Fatalf("expected reconnect (disc=1,upd=1,conn=1), got disc=%d upd=%d conn=%d",
			disconnect, updateCfg, connect)
	}
	if !connected {
		t.Fatalf("provider should be connected after guild change")
	}
	if ff.creationCount() != 0 {
		t.Fatalf("existing provider should be reused, got %d creations", ff.creationCount())
	}
	if strings.Contains(buf.String(), secret) {
		t.Fatalf("bot token must never be logged; found it in: %q", buf.String())
	}
}

// --- Test 6: CASE C — enabling (false -> true) creates and connects, no pre-disconnect ---

func TestDiscordConfigEnableCreatesAndConnects(t *testing.T) {
	m, ff := newManager(t, config.DiscordSettings{Enabled: false})

	buf := captureLogs(t)
	mustUpdate(t, m, config.DiscordSettings{Enabled: true, BotToken: "tok", GuildID: "guild"})

	if ff.creationCount() != 1 {
		t.Fatalf("expected 1 provider creation on enable, got %d", ff.creationCount())
	}
	created := ff.created[0]
	connect, disconnect, _, connected := created.counts()
	if connect != 1 {
		t.Fatalf("expected exactly 1 Connect, got %d", connect)
	}
	if disconnect != 0 {
		t.Fatalf("enable must not pre-disconnect, got %d Disconnect", disconnect)
	}
	if !connected {
		t.Fatalf("provider should be connected after enable")
	}
	if !strings.Contains(buf.String(), "Discord notifications enabled") {
		t.Fatalf("enable should log the enabled transition: %q", buf.String())
	}
}

// --- Test 7: CASE B — disabling disconnects, clears provider, and repeat is a no-op ---

func TestDiscordConfigDisableDisconnectsAndClearsProvider(t *testing.T) {
	start := config.DiscordSettings{Enabled: true, BotToken: "tok", GuildID: "guild"}
	m, ff := newManager(t, start)

	fake := &fakeDiscord{connected: true, botToken: "tok", guildID: "guild"}
	m.discord = fake

	mustUpdate(t, m, config.DiscordSettings{Enabled: false})

	_, disconnect, _, _ := fake.counts()
	if disconnect != 1 {
		t.Fatalf("expected 1 Disconnect on disable, got %d", disconnect)
	}
	if m.discord != nil {
		t.Fatalf("provider must be cleared to nil on disable")
	}

	// Re-applying the disabled config must be a pure no-op: nothing to
	// disconnect, no provider created.
	buf := captureLogs(t)
	mustUpdate(t, m, config.DiscordSettings{Enabled: false})
	if m.discord != nil {
		t.Fatalf("provider must remain nil after a repeated disabled apply")
	}
	if ff.creationCount() != 0 {
		t.Fatalf("repeated disable must not create a provider, got %d creations", ff.creationCount())
	}
	if strings.Contains(buf.String(), "disabled") {
		t.Fatalf("repeated disable must not re-log the disable transition: %q", buf.String())
	}
}

// --- Test 8: pointer-alias regression — mutating the original config struct in
// place is still detected as a change and triggers a reconnect. A manager that
// retained the caller's pointer as its authoritative config would treat this as
// a no-op. ---

func TestDiscordConfigPointerAliasMutationReconnects(t *testing.T) {
	// One shared pointer handed to NewManager and later mutated in place.
	cfg := &config.DiscordSettings{Enabled: true, BotToken: "old-token", GuildID: "guild"}

	ff := &fakeFactory{}
	m, err := NewManager(cfg, nil, testDBHandle, nil, "")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	m.newDiscord = ff.make

	// Attach a connected fake reflecting the ORIGINAL credentials.
	fake := &fakeDiscord{connected: true, botToken: "old-token", guildID: "guild"}
	m.discord = fake

	// Mutate the very object NewManager received. If the manager kept this
	// pointer as its stored config, its "old" snapshot now also reads
	// "changed-token" and the change goes undetected.
	cfg.BotToken = "changed-token"

	if err := m.UpdateDiscordConfig(cfg); err != nil {
		t.Fatalf("UpdateDiscordConfig: %v", err)
	}

	connect, disconnect, updateCfg, connected := fake.counts()
	if disconnect != 1 || updateCfg != 1 || connect != 1 {
		t.Fatalf("in-place token mutation must reconnect (disc=1,upd=1,conn=1), got disc=%d upd=%d conn=%d",
			disconnect, updateCfg, connect)
	}
	if !connected {
		t.Fatalf("provider should be connected after reconnect")
	}
	if ff.creationCount() != 0 {
		t.Fatalf("existing provider should be reused, got %d creations", ff.creationCount())
	}
	if got := fake.token(); got != "changed-token" {
		t.Fatalf("provider not re-pointed at the mutated token: got %q", got)
	}
}

// --- Test 9: CASE I — a failed Connect stays retryable; the next identical
// apply retries instead of becoming a no-op or re-tearing-down. ---

func TestDiscordConfigConnectFailureRemainsRetryable(t *testing.T) {
	start := config.DiscordSettings{Enabled: true, BotToken: "old-token", GuildID: "guild"}
	m, ff := newManager(t, start)

	// Provider currently connected on the OLD credentials.
	fake := &fakeDiscord{connected: true, botToken: "old-token", guildID: "guild"}
	// First Connect (after the token change) fails; the second succeeds.
	fake.connectErrs = []error{errors.New("gateway unavailable"), nil}
	m.discord = fake

	buf := captureLogs(t)
	changed := config.DiscordSettings{Enabled: true, BotToken: "new-token", GuildID: "guild"}

	// First apply: token changed, Connect fails -> error surfaced, not connected.
	if err := m.UpdateDiscordConfig(&changed); err == nil {
		t.Fatalf("expected the first apply to surface the Connect error")
	}
	connect, disconnect, updateCfg, connected := fake.counts()
	if connect != 1 || disconnect != 1 || updateCfg != 1 {
		t.Fatalf("after failed change apply expected disc=1,upd=1,conn=1, got disc=%d upd=%d conn=%d",
			disconnect, updateCfg, connect)
	}
	if connected {
		t.Fatalf("provider must not be considered connected after a failed Connect")
	}

	// Second apply: same (already-stored) config. Because the desired config was
	// recorded before the failed Connect, this is unchanged config + a down
	// session -> a clean reconnect (no extra Disconnect/UpdateConfig), NOT a
	// no-op.
	if err := m.UpdateDiscordConfig(&changed); err != nil {
		t.Fatalf("retry apply should succeed: %v", err)
	}
	connect, disconnect, updateCfg, connected = fake.counts()
	if connect != 2 {
		t.Fatalf("retry must call Connect again (want 2), got %d", connect)
	}
	if disconnect != 1 || updateCfg != 1 {
		t.Fatalf("retry of an unchanged (already-stored) config must not re-tear-down: got disc=%d upd=%d",
			disconnect, updateCfg)
	}
	if !connected {
		t.Fatalf("provider should be connected after a successful retry")
	}
	if ff.creationCount() != 0 {
		t.Fatalf("existing provider should be reused, got %d creations", ff.creationCount())
	}
	if strings.Contains(buf.String(), "new-token") || strings.Contains(buf.String(), "old-token") {
		t.Fatalf("bot token must never be logged; found it in: %q", buf.String())
	}
}

// --- Test 11: saving an unrelated (non-Discord) setting re-applies the
// unchanged Discord config; while connected this must not reconnect.
//
// Boundary note: Miner.ApplySettings copies m.config.Discord into a fresh local
// value and calls notifMgr.UpdateDiscordConfig(&copy) on EVERY settings save,
// Discord-related or not. A full Miner fixture (watcher, pubsub pool, streamer
// manager, web server, ...) is far heavier than needed to prove the fix, so this
// exercises the exact boundary that ApplySettings hits — repeated identical
// UpdateDiscordConfig calls with a connected provider — and asserts zero
// reconnect churn across several such saves. ---

func TestNonDiscordSettingsSaveDoesNotReconnectDiscord(t *testing.T) {
	cfg := config.DiscordSettings{Enabled: true, BotToken: "tok", GuildID: "guild"}
	m, ff := newManager(t, cfg)

	fake := &fakeDiscord{connected: true, botToken: "tok", guildID: "guild"}
	m.discord = fake

	buf := captureLogs(t)

	// Simulate several non-Discord settings saves. Each mirrors ApplySettings
	// handing over a fresh copy of the unchanged Discord config.
	for i := 0; i < 5; i++ {
		mustUpdate(t, m, config.DiscordSettings{Enabled: true, BotToken: "tok", GuildID: "guild"})
	}

	connect, disconnect, updateCfg, connected := fake.counts()
	if connect != 0 || disconnect != 0 || updateCfg != 0 {
		t.Fatalf("unrelated settings saves must not touch Discord (conn=0,disc=0,upd=0), got conn=%d disc=%d upd=%d",
			connect, disconnect, updateCfg)
	}
	if !connected {
		t.Fatalf("provider should remain connected throughout")
	}
	if ff.creationCount() != 0 {
		t.Fatalf("no provider should be created on unrelated saves, got %d", ff.creationCount())
	}
	if m.discord != discordProvider(fake) {
		t.Fatalf("provider object must be preserved across unrelated saves")
	}
	if strings.Contains(buf.String(), "updated and reconnected") {
		t.Fatalf("unrelated saves must not log Discord reconnects: %q", buf.String())
	}
}
