package miner

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/journal"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/pubsub"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
)

// newDiscordTogglingMiner builds a capability miner with a real,
// write-once-published notifications manager (a private raw DB, closed by
// the returned cleanup) plus the two RuntimeSettings variants (Discord
// enabled/disabled) every T5-T10 concurrency test races a reader against.
func newDiscordTogglingMiner(t *testing.T, usernames ...string) (m *Miner, rsOn, rsOff settings.RuntimeSettings) {
	t.Helper()
	m, _, _ = newCapabilityMiner(t, usernames...)
	dbPath := filepath.Join(t.TempDir(), "concurrency.db")
	db := openRawMinerDB(t, dbPath)
	t.Cleanup(func() { _ = db.Close() })
	m.db = db
	m.initNotificationManager(context.Background())

	rsOn = m.GetRuntimeSettings()
	rsOn.Discord.Enabled = true
	rsOff = m.GetRuntimeSettings()
	rsOff.Discord.Enabled = false
	return m, rsOn, rsOff
}

// raceApplyAgainst hammers `reader` on its own goroutine while driving
// several real ApplySettings calls that alternate rs.Discord.Enabled on the
// miner's own goroutine, then joins the reader. The race detector is the
// actual assertion for every T5-T10 test built on this helper: it proves the
// given reader function — one of the raw call sites converted to the
// notificationManager() accessor — never races finishApply's config swap or
// its (accessor-based) Discord reconcile.
func raceApplyAgainst(t *testing.T, m *Miner, rsOn, rsOff settings.RuntimeSettings, reader func()) {
	t.Helper()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				reader()
			}
		}
	}()

	for i := 0; i < 25; i++ {
		rs := rsOn
		if i%2 == 1 {
			rs = rsOff
		}
		if err := m.ApplySettings(context.Background(), rs); err != nil {
			t.Errorf("ApplySettings: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}

// T5: handleStatusChange (pubsub/status goroutine in production).
func TestHandleStatusChangeConcurrentWithDiscordToggle(t *testing.T) {
	m, rsOn, rsOff := newDiscordTogglingMiner(t, "alpha")
	raceApplyAgainst(t, m, rsOn, rsOff, func() {
		m.handleStatusChange("alpha", models.StatusOnline)
		m.handleStatusChange("alpha", models.StatusOffline)
	})
}

// T6: evaluateConnectionHealth (health-watchdog goroutine in production).
func TestEvaluateConnectionHealthConcurrentWithDiscordToggle(t *testing.T) {
	m, rsOn, rsOff := newDiscordTogglingMiner(t, "alpha")
	m.healthJournal = journal.New[journal.HealthEvent](healthJournalCapacity, nil)

	var state connHealthState
	raceApplyAgainst(t, m, rsOn, rsOff, func() {
		m.evaluateConnectionHealth(time.Now(), &state)
	})
}

// T7: sendDailySummary (daily-summary goroutine in production), wired with a
// real analytics service so the reader exercises the actual query path, not
// just the notification-manager accessor.
func TestSendDailySummaryConcurrentWithDiscordToggle(t *testing.T) {
	m, rsOn, rsOff := newDiscordTogglingMiner(t, "alpha")

	svc, err := analytics.NewService(m.db, t.TempDir(), 0)
	if err != nil {
		t.Fatalf("analytics service: %v", err)
	}
	m.analyticsSvc = svc

	start, end := time.Now().Add(-24*time.Hour), time.Now()
	raceApplyAgainst(t, m, rsOn, rsOff, func() {
		m.sendDailySummary(start, end)
	})
}

// T8: handlePubSubMessage's points-earned branch (pubsub goroutine in
// production).
func TestHandlePubSubMessageConcurrentWithDiscordToggle(t *testing.T) {
	m, rsOn, rsOff := newDiscordTogglingMiner(t, "alpha")
	s := m.streamers.Get("alpha")
	if s == nil {
		t.Fatal("setup: streamer alpha not found")
	}

	msg := &pubsub.PubSubMessage{
		Topic: pubsub.NewTopic(pubsub.TopicCommunityPointsUser, "u1"),
		Type:  "points-earned",
		Data: map[string]interface{}{
			"point_gain": map[string]interface{}{
				"reason_code":  "CLAIM",
				"total_points": float64(10),
			},
		},
	}

	raceApplyAgainst(t, m, rsOn, rsOff, func() {
		m.handlePubSubMessage(msg, s)
	})
}

// T9: handleAuthError (recovery goroutine in production). The dedupe flag
// (reauthNotified) is reset before each call so the reader actually
// re-executes the notificationManager()-reading branch every iteration
// rather than short-circuiting on the guard after the first call.
func TestHandleAuthErrorConcurrentWithDiscordToggle(t *testing.T) {
	m, rsOn, rsOff := newDiscordTogglingMiner(t, "alpha")

	raceApplyAgainst(t, m, rsOn, rsOff, func() {
		m.mu.Lock()
		m.reauthNotified = false
		m.mu.Unlock()
		m.handleAuthError()
	})
}

// T10: recordHealthTransition (watchdog goroutine in production), driven
// directly with a synthetic "meaningful" transition so every call actually
// reaches the notificationManager() read at health_journal.go's
// NotificationRequested line, rather than being deduped as a repeat.
func TestRecordHealthTransitionConcurrentWithDiscordToggle(t *testing.T) {
	m, rsOn, rsOff := newDiscordTogglingMiner(t, "alpha")
	m.healthJournal = journal.New[journal.HealthEvent](healthJournalCapacity, nil)

	out := connOutcome{level: connLost, apiState: apiConnDown, lostDetail: "down"}
	tr := connTransition{notifyLost: true, enteredLost: true}
	raceApplyAgainst(t, m, rsOn, rsOff, func() {
		m.recordHealthTransition(connHealthy, connLost, out, tr)
	})
}

// T16: dailySummaryTimeSnapshot (I12) racing the real ApplySettings config
// swap. This targets the SEPARATE, adjacent m.config-pointer race the design
// dossier flagged (daily_summary.go's old raw `m.config.DailySummary.Time`
// read vs. finishApply's `m.config = newConfig` swap) rather than the
// notifications field — dailySummaryTimeSnapshot is the fix.
func TestDailySummaryTimeSnapshotRaceFreeWithConcurrentApply(t *testing.T) {
	m, rsOn, rsOff := newDiscordTogglingMiner(t, "alpha")
	m.config.DailySummary.Time = "09:00"

	raceApplyAgainst(t, m, rsOn, rsOff, func() {
		_ = m.dailySummaryTimeSnapshot()
	})
}
