package miner

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/chat"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
)

func newC2ApplyLoggingMiner(t *testing.T, enabled bool) (*Miner, *chat.ChatManager) {
	t.Helper()
	m, _, _ := newCapabilityMiner(t, "alpha")
	m.config.EnableAnalytics = true
	m.config.Analytics.EnableChatLogs = enabled
	m.analyticsSvc = new(analytics.Service)

	var logger chat.ChatLogger
	if enabled {
		logger = analytics.NewChatLoggerAdapter(m.analyticsSvc)
	}
	manager := chat.NewChatManager("tester", chat.StaticToken("tok"), logger, enabled, nil)
	t.Cleanup(func() { _ = manager.Close() })

	// The seeded streamer is ChatOnline+UNKNOWN, so the genuine whole-roster
	// presence sweep performs no network dial while this test observes the real
	// ApplySettings -> ChatManager owner handoff.
	m.chatPresence = nil
	m.chatManager = manager
	return m, manager
}

func TestApplySettingsChatLoggingUsesOneStableRuntimeSink(t *testing.T) {
	m, _, chatRec := newCapabilityMiner(t, "alpha")
	m.config.EnableAnalytics = true
	m.analyticsSvc = new(analytics.Service)

	initial := m.GetRuntimeSettings()
	initial.Analytics.EnableChatLogs = false
	if err := m.ApplySettings(context.Background(), initial); err != nil {
		t.Fatalf("initial false ApplySettings: %v", err)
	}
	calls := chatRec.loggingCalls()
	if len(calls) != 1 || calls[0].global || calls[0].logger == nil {
		t.Fatalf("initial false logging target = %+v, want false/non-nil", calls)
	}
	firstSink := calls[0].logger

	enabled := m.GetRuntimeSettings()
	enabled.Analytics.EnableChatLogs = true
	if err := m.ApplySettings(context.Background(), enabled); err != nil {
		t.Fatalf("enable ApplySettings: %v", err)
	}
	calls = chatRec.loggingCalls()
	if !calls[len(calls)-1].global || calls[len(calls)-1].logger != firstSink {
		t.Fatalf("enabled logging target = %+v, want true/non-nil", calls[len(calls)-1])
	}

	if err := m.ApplySettings(context.Background(), enabled); err != nil {
		t.Fatalf("identical enabled ApplySettings: %v", err)
	}
	disabled := enabled
	disabled.Analytics.EnableChatLogs = false
	if err := m.ApplySettings(context.Background(), disabled); err != nil {
		t.Fatalf("disable ApplySettings: %v", err)
	}
	calls = chatRec.loggingCalls()
	for i, call := range calls[1:] {
		if call.logger != firstSink {
			t.Fatalf("logging call %d sink identity changed: got %T, want the original %T instance", i+2, call.logger, firstSink)
		}
	}
	if calls[len(calls)-1].global {
		t.Fatal("final logging target stayed globally enabled")
	}
}

type c2OrderedChatReconciler struct {
	record func(string)
	global bool
}

func (c *c2OrderedChatReconciler) ToggleChat(*models.Streamer) {}
func (c *c2OrderedChatReconciler) Leave(string)                {}
func (c *c2OrderedChatReconciler) ReconcileLogging(globalEnabled bool, _ chat.ChatLogger) {
	c.global = globalEnabled
	c.record("reconcile")
}

func TestApplySettingsKeepsRuntimeBeforeNonFatalSaveConfig(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "alpha")
	m.config.EnableAnalytics = true
	m.analyticsSvc = new(analytics.Service)

	var mu sync.Mutex
	var events []string
	record := func(event string) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	}
	chatRec := &c2OrderedChatReconciler{record: record}
	m.chatPresence = chatRec
	m.configPath = "c2-order-config.json"
	saveFailure := errors.New("c2 deterministic save failure")
	m.saveConfigFn = func(string, *config.Config) error {
		record("save")
		return saveFailure
	}

	runtimeSettings := m.GetRuntimeSettings()
	runtimeSettings.Analytics.EnableChatLogs = true
	if err := m.ApplySettings(context.Background(), runtimeSettings); err != nil {
		t.Fatalf("generic SaveConfig failure became fatal: %v", err)
	}

	mu.Lock()
	gotEvents := append([]string(nil), events...)
	mu.Unlock()
	if !reflect.DeepEqual(gotEvents, []string{"reconcile", "save"}) {
		t.Fatalf("generic apply ordering = %v, want [reconcile save]", gotEvents)
	}
	if !chatRec.global || !m.config.Analytics.EnableChatLogs {
		t.Fatal("runtime/in-memory logging target did not remain applied after non-fatal SaveConfig failure")
	}
}

type c2SerializedChatReconciler struct {
	mu        sync.Mutex
	active    int
	maxActive int
	calls     []bool
	loggers   []chat.ChatLogger
	entered   chan bool
	release   chan struct{}
}

func newC2SerializedChatReconciler() *c2SerializedChatReconciler {
	return &c2SerializedChatReconciler{
		entered: make(chan bool),
		release: make(chan struct{}),
	}
}

func (c *c2SerializedChatReconciler) ToggleChat(*models.Streamer) {}
func (c *c2SerializedChatReconciler) Leave(string)                {}
func (c *c2SerializedChatReconciler) ReconcileLogging(globalEnabled bool, logger chat.ChatLogger) {
	c.mu.Lock()
	c.active++
	if c.active > c.maxActive {
		c.maxActive = c.active
	}
	c.calls = append(c.calls, globalEnabled)
	c.loggers = append(c.loggers, logger)
	c.mu.Unlock()

	c.entered <- globalEnabled
	<-c.release

	c.mu.Lock()
	c.active--
	c.mu.Unlock()
}

func receiveC2LoggingApply(t *testing.T, entered <-chan bool) bool {
	t.Helper()
	select {
	case global := <-entered:
		return global
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for serialized logging apply")
		return false
	}
}

func TestConcurrentApplySettingsSerializesChatLoggingToFinalApply(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "alpha")
	m.config.EnableAnalytics = true
	m.analyticsSvc = new(analytics.Service)
	chatRec := newC2SerializedChatReconciler()
	m.chatPresence = chatRec

	on := m.GetRuntimeSettings()
	on.Analytics.EnableChatLogs = true
	off := on
	off.Analytics.EnableChatLogs = false

	apply := func(settingsToApply settings.RuntimeSettings) <-chan error {
		done := make(chan error, 1)
		go func() { done <- m.ApplySettings(context.Background(), settingsToApply) }()
		return done
	}

	firstDone := apply(on)
	if global := receiveC2LoggingApply(t, chatRec.entered); !global {
		t.Fatal("first serialized logging apply = false, want true")
	}
	secondStarted := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		close(secondStarted)
		secondDone <- m.ApplySettings(context.Background(), off)
	}()
	<-secondStarted
	chatRec.release <- struct{}{}
	if err := <-firstDone; err != nil {
		t.Fatalf("first ApplySettings: %v", err)
	}
	if global := receiveC2LoggingApply(t, chatRec.entered); global {
		t.Fatal("second serialized logging apply = true, want false")
	}

	thirdDone := apply(on)
	chatRec.release <- struct{}{}
	if err := <-secondDone; err != nil {
		t.Fatalf("second ApplySettings: %v", err)
	}
	if global := receiveC2LoggingApply(t, chatRec.entered); !global {
		t.Fatal("third serialized logging apply = false, want true")
	}
	chatRec.release <- struct{}{}
	if err := <-thirdDone; err != nil {
		t.Fatalf("third ApplySettings: %v", err)
	}

	chatRec.mu.Lock()
	calls := append([]bool(nil), chatRec.calls...)
	loggers := append([]chat.ChatLogger(nil), chatRec.loggers...)
	maxActive := chatRec.maxActive
	chatRec.mu.Unlock()
	if !reflect.DeepEqual(calls, []bool{true, false, true}) {
		t.Fatalf("serialized logging calls = %v, want [true false true]", calls)
	}
	if maxActive != 1 {
		t.Fatalf("maximum concurrent logging reconciliations = %d, want 1", maxActive)
	}
	if len(loggers) != 3 || loggers[0] == nil || loggers[1] != loggers[0] || loggers[2] != loggers[0] {
		t.Fatalf("serialized applies did not reuse one stable sink: %v", loggers)
	}
	if !m.config.Analytics.EnableChatLogs {
		t.Fatal("final in-memory global logging state did not converge to true")
	}
}

func c2ManagerLoggingState(t *testing.T, manager *chat.ChatManager) (global, hasLogger bool) {
	t.Helper()
	value := reflect.ValueOf(manager).Elem()
	globalField := value.FieldByName("globalChatLogsOn")
	loggerField := value.FieldByName("logger")
	if !globalField.IsValid() || !loggerField.IsValid() {
		t.Fatal("ChatManager logging owner fields missing")
	}
	return globalField.Bool(), !loggerField.IsNil()
}

func TestC2REDApplySettingsReconcilesGlobalChatLoggingBeforeReturn(t *testing.T) {
	t.Run("false_to_true_updates_future_join_state_and_sink", func(t *testing.T) {
		m, manager := newC2ApplyLoggingMiner(t, false)
		runtimeSettings := m.GetRuntimeSettings()
		runtimeSettings.Analytics.EnableChatLogs = true

		if err := m.ApplySettings(context.Background(), runtimeSettings); err != nil {
			t.Fatalf("ApplySettings: %v", err)
		}
		global, hasLogger := c2ManagerLoggingState(t, manager)
		if !global {
			t.Error("ApplySettings returned while ChatManager still inherited global Chat Logs=false")
		}
		if !hasLogger {
			t.Error("ApplySettings returned without provisioning ChatLogger although analyticsSvc exists")
		}
	})

	t.Run("true_to_false_revokes_manager_state_before_return", func(t *testing.T) {
		m, manager := newC2ApplyLoggingMiner(t, true)
		runtimeSettings := m.GetRuntimeSettings()
		runtimeSettings.Analytics.EnableChatLogs = false

		if err := m.ApplySettings(context.Background(), runtimeSettings); err != nil {
			t.Fatalf("ApplySettings: %v", err)
		}
		global, _ := c2ManagerLoggingState(t, manager)
		if global {
			t.Error("ApplySettings returned while ChatManager still inherited global Chat Logs=true")
		}
	})
}
