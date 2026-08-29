package miner

import (
	"context"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/chat"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamer"
)

func startC4ColdGeneration(
	t *testing.T,
	cfg *config.Config,
	configPath string,
	db *database.DB,
	svc *analytics.Service,
) (*Miner, *fakeChatReconciler, func()) {
	t.Helper()

	m := New(cfg, configPath)
	m.SetDatabase(db)
	m.SetAnalyticsService(svc)
	m.capabilityTopics = newFakeTopicReconciler()
	chatRec := newFakeChatReconciler()
	m.chatPresence = chatRec

	stubAuthenticate(m)
	m.loadStreamersFn = func() error {
		manager := streamer.NewManager(fakeStreamerAPI{}, m.config.StreamerSettings)
		added, _, _, _ := manager.ApplySettings(m.config.Streamers, m.config.StreamerSettings)
		if len(added) != len(m.config.Streamers) {
			t.Fatalf("cold roster seed added %d streamers, want %d", len(added), len(m.config.Streamers))
		}
		m.streamers = manager
		return nil
	}
	m.subscribeTopicsFn = func() error { return nil }

	started := make(chan struct{})
	m.startMiningFn = func(context.Context) { close(started) }
	runCtx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- m.Run(runCtx) }()

	select {
	case <-started:
	case err := <-runErr:
		cancel()
		t.Fatalf("cold generation exited before startup completed: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("cold generation startup did not complete")
	}

	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			select {
			case err := <-runErr:
				if err != nil {
					t.Fatalf("cold generation shutdown = %v, want nil", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("cold generation did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return m, chatRec, stop
}

func TestC4ColdExplicitTrueProvisionsStableCanonicalSinkAcrossRestart(t *testing.T) {
	t.Chdir(t.TempDir())

	db := openRawMinerDB(t, filepath.Join(t.TempDir(), "miner.db"))
	t.Cleanup(func() { _ = db.Close() })
	svc, err := analytics.NewService(db, t.TempDir(), 0)
	if err != nil {
		t.Fatalf("analytics.NewService: %v", err)
	}

	explicitTrue := true
	alphaSettings := models.DefaultStreamerSettings()
	alphaSettings.ChatLogs = &explicitTrue
	cfg := config.DefaultConfig()
	cfg.Username = "c4_cold_tester"
	cfg.EnableAnalytics = true
	cfg.Analytics.EnableChatLogs = false
	cfg.Discord.Enabled = false
	cfg.Debug.Enabled = false
	cfg.Streamers = []config.StreamerConfig{{
		Username:  "alpha",
		ChannelID: "chan-alpha",
		Settings:  &alphaSettings,
	}}
	configPath := filepath.Join(t.TempDir(), "config.json")

	generationA, chatA, stopA := startC4ColdGeneration(t, &cfg, configPath, db, svc)
	if generationA.db != db || generationA.analyticsSvc != svc {
		t.Fatal("generation A did not retain the App-owned database/service")
	}

	runtimeA := generationA.GetRuntimeSettings()
	if runtimeA.Analytics.EnableChatLogs {
		t.Fatal("nested global chat logging unexpectedly enabled")
	}
	if len(runtimeA.Streamers) != 1 || runtimeA.Streamers[0].Settings == nil ||
		runtimeA.Streamers[0].Settings.ChatLogs == nil || !*runtimeA.Streamers[0].Settings.ChatLogs {
		t.Fatalf("C3 explicit-true pointer was not preserved in runtime DTO: %+v", runtimeA.Streamers)
	}
	if _, ok := generationA.chatLogger.(*analytics.ChatLoggerAdapter); !ok {
		t.Fatalf("cold canonical sink = %T, want *analytics.ChatLoggerAdapter", generationA.chatLogger)
	}
	if global, hasLogger := c2ManagerLoggingState(t, generationA.chatManager); global || !hasLogger {
		t.Fatalf("generation A cold ChatManager target = global:%t logger:%t, want false/non-nil", global, hasLogger)
	}

	if err := generationA.ApplySettings(context.Background(), runtimeA); err != nil {
		t.Fatalf("generation A ApplySettings: %v", err)
	}
	callsA := chatA.loggingCalls()
	if len(callsA) != 1 || callsA[0].global || callsA[0].logger == nil {
		t.Fatalf("generation A C2 target = %+v, want global=false/non-nil", callsA)
	}
	if got := chatA.toggleCount("alpha"); got != 1 {
		t.Fatalf("generation A ToggleChat calls = %d, want 1", got)
	}
	sinkA := callsA[0].logger
	if sinkA != generationA.chatLogger {
		t.Fatal("cold and runtime paths did not share the canonical sink")
	}

	if err := generationA.ApplySettings(context.Background(), runtimeA); err != nil {
		t.Fatalf("generation A repeated ApplySettings: %v", err)
	}
	callsA = chatA.loggingCalls()
	if len(callsA) != 2 || callsA[1].global || callsA[1].logger != sinkA {
		t.Fatalf("generation A repeated target = %+v, want same false/non-nil sink", callsA)
	}
	if got := chatA.toggleCount("alpha"); got != 2 {
		t.Fatalf("generation A repeated ToggleChat calls = %d, want 2", got)
	}

	first := chat.ChatMessageData{
		Username: "alice", DisplayName: "Alice", Message: "generation-a",
		Emotes: "25:0-4", Badges: "subscriber/1", Color: "#123456",
	}
	if err := sinkA.RecordChatMessage("alpha", first); err != nil {
		t.Fatalf("generation A chat write: %v", err)
	}

	stopA()
	requireExternalDBAlive(t, db)
	nextConfig := generationA.ConfigSnapshot()
	if nextConfig.Streamers[0].Settings == nil || nextConfig.Streamers[0].Settings.ChatLogs == nil ||
		!*nextConfig.Streamers[0].Settings.ChatLogs {
		t.Fatal("restart snapshot lost explicit ChatLogs=true")
	}

	generationB, chatB, stopB := startC4ColdGeneration(t, nextConfig, configPath, db, svc)
	if generationB.db != db || generationB.analyticsSvc != svc {
		t.Fatal("generation B did not reuse the App-owned database/service")
	}
	sinkB := generationB.chatLogger
	if _, ok := sinkB.(*analytics.ChatLoggerAdapter); !ok {
		t.Fatalf("generation B cold sink = %T, want *analytics.ChatLoggerAdapter", sinkB)
	}
	if sinkB == sinkA {
		t.Fatal("fresh Miner generation reused the prior generation's adapter")
	}
	if global, hasLogger := c2ManagerLoggingState(t, generationB.chatManager); global || !hasLogger {
		t.Fatalf("generation B cold ChatManager target = global:%t logger:%t, want false/non-nil", global, hasLogger)
	}

	runtimeB := generationB.GetRuntimeSettings()
	if err := generationB.ApplySettings(context.Background(), runtimeB); err != nil {
		t.Fatalf("generation B ApplySettings: %v", err)
	}
	callsB := chatB.loggingCalls()
	if len(callsB) != 1 || callsB[0].global || callsB[0].logger != sinkB {
		t.Fatalf("generation B C2 target = %+v, want global=false/new canonical sink", callsB)
	}
	if got := chatB.toggleCount("alpha"); got != 1 {
		t.Fatalf("generation B ToggleChat calls = %d, want 1", got)
	}

	second := chat.ChatMessageData{
		Username: "bob", DisplayName: "Bob", Message: "generation-b",
		Emotes: "", Badges: "moderator/1", Color: "#654321",
	}
	if err := sinkB.RecordChatMessage("alpha", second); err != nil {
		t.Fatalf("generation B chat write: %v", err)
	}
	stopB()
	requireExternalDBAlive(t, db)

	type chatRow struct {
		Streamer, Username, DisplayName, Message, Emotes, Badges, Color string
	}
	rows, err := db.Query(`
		SELECT s.name, c.username, c.display_name, c.message,
		       COALESCE(c.emotes, ''), COALESCE(c.badges, ''), COALESCE(c.color, '')
		  FROM chat_messages c
		  JOIN streamers s ON s.id = c.streamer_id
		 ORDER BY c.id`)
	if err != nil {
		t.Fatalf("query chat_messages: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var got []chatRow
	for rows.Next() {
		var row chatRow
		if err := rows.Scan(&row.Streamer, &row.Username, &row.DisplayName, &row.Message,
			&row.Emotes, &row.Badges, &row.Color); err != nil {
			t.Fatalf("scan chat_messages: %v", err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate chat_messages: %v", err)
	}
	want := []chatRow{
		{"alpha", "alice", "Alice", "generation-a", "25:0-4", "subscriber/1", "#123456"},
		{"alpha", "bob", "Bob", "generation-b", "", "moderator/1", "#654321"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("chat_messages = %+v, want exactly %+v (no missing/ghost rows)", got, want)
	}
}

func TestC4TopLevelAnalyticsDisableDoesNotProvisionSink(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.EnableAnalytics = false
	cfg.Analytics.EnableChatLogs = true
	m := New(&cfg, "")
	m.SetAnalyticsService(new(analytics.Service))

	m.mu.Lock()
	enabled, sink := m.chatLoggingTargetLocked()
	m.mu.Unlock()
	if enabled || sink != nil || m.chatLogger != nil {
		t.Fatalf("top-level analytics disable target = enabled=%v sink=%T stored=%T, want false/nil/nil", enabled, sink, m.chatLogger)
	}
}
