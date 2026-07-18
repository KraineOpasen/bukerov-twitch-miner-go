package notifications

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// upcomingDiscord is a network-free discordProvider whose Send outcome the test
// controls, so the upcoming-campaign dedupe/retry semantics can be asserted
// without any gateway I/O.
type upcomingDiscord struct {
	mu        sync.Mutex
	sendErr   error
	sendCalls int
	last      Notification
}

func (f *upcomingDiscord) Connect(context.Context) error { return nil }
func (f *upcomingDiscord) Disconnect() error             { return nil }
func (f *upcomingDiscord) UpdateConfig(string, string)   {}
func (f *upcomingDiscord) IsConnected() bool             { return true }
func (f *upcomingDiscord) Send(_ context.Context, n Notification) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendCalls++
	f.last = n
	return f.sendErr
}
func (f *upcomingDiscord) GetChannels(context.Context, bool) ([]Channel, error) { return nil, nil }

func (f *upcomingDiscord) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sendCalls
}
func (f *upcomingDiscord) setErr(e error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendErr = e
}
func (f *upcomingDiscord) lastMessage() Notification {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

// newUpcomingManagerRaw builds a Manager wired with a controllable fake Discord
// provider and a system channel, opting the upcoming event in or out. It does
// NOT clear the shared dedupe table, so tests that deliberately rely on durable
// state across managers (restart) can use it for the second manager.
func newUpcomingManagerRaw(t *testing.T, eventEnabled bool) (*Manager, *upcomingDiscord) {
	t.Helper()
	m, _ := newManager(t, config.DiscordSettings{Enabled: true})
	fake := &upcomingDiscord{}
	m.discord = fake

	cfg := DefaultNotificationConfig()
	cfg.SystemEnabled = true
	cfg.SystemChannelID = "chan-1"
	cfg.UpcomingDropsEnabled = eventEnabled
	if err := m.SaveConfig(&cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	t.Cleanup(func() {
		d := DefaultNotificationConfig()
		_ = m.SaveConfig(&d)
	})
	return m, fake
}

// newUpcomingManager builds a manager and clears the shared, process-wide dedupe
// table so each test (and each -count iteration) starts from a clean slate —
// the singleton test DB persists rows across tests and repeated runs otherwise.
// The migration has already run by now (NewManager registered the module), so
// the table exists.
func newUpcomingManager(t *testing.T, eventEnabled bool) (*Manager, *upcomingDiscord) {
	t.Helper()
	m, fake := newUpcomingManagerRaw(t, eventEnabled)
	if _, err := testDBHandle.Exec("DELETE FROM upcoming_campaign_notifications"); err != nil {
		t.Fatalf("reset dedupe table: %v", err)
	}
	return m, fake
}

func upcomingCampaignModel(id string) *models.Campaign {
	return &models.Campaign{
		ID:      id,
		Name:    "Campaign " + id,
		Game:    &models.Game{ID: "27546", Name: "World of Tanks"},
		StartAt: time.Now().Add(48 * time.Hour),
		EndAt:   time.Now().Add(96 * time.Hour),
		Drops:   []*models.Drop{{Name: "Reward A", Benefit: "Premium Tank"}},
	}
}

func upcomingState(t *testing.T, m *Manager, id string) UpcomingNotifyRecord {
	t.Helper()
	rec, err := m.repo.GetUpcomingNotifyState(id, string(NotificationTypeUpcomingCampaign))
	if err != nil {
		t.Fatalf("GetUpcomingNotifyState: %v", err)
	}
	return rec
}

// Test 20 (default half) / M6: the upcoming-drops opt-in is OFF by default, so
// an update never starts sending an event nobody enabled.
func TestUpcomingDefaultDisabled(t *testing.T) {
	if DefaultNotificationConfig().UpcomingDropsEnabled {
		t.Fatal("upcoming-drops notifications must be disabled by default")
	}
}

// Test 20: disabled by default -> no notification, recorded suppressed.
func TestUpcomingDisabledDoesNotNotify(t *testing.T) {
	m, fake := newUpcomingManager(t, false)
	m.NotifyUpcomingDropCampaign(context.Background(), upcomingCampaignModel("disabled-1"))

	if fake.calls() != 0 {
		t.Fatalf("event disabled must not send, got %d sends", fake.calls())
	}
	if rec := upcomingState(t, m, "disabled-1"); rec.Status != UpcomingStatusSuppressed {
		t.Fatalf("disabled campaign must be recorded suppressed, got %q", rec.Status)
	}
}

// Test 21 + 22: enabled + first new campaign -> exactly one; identical repeat -> zero more.
func TestUpcomingEnabledNotifiesOnceThenDedupes(t *testing.T) {
	m, fake := newUpcomingManager(t, true)
	c := upcomingCampaignModel("enabled-1")

	m.NotifyUpcomingDropCampaign(context.Background(), c)
	if fake.calls() != 1 {
		t.Fatalf("first new campaign must send exactly once, got %d", fake.calls())
	}
	if rec := upcomingState(t, m, "enabled-1"); rec.Status != UpcomingStatusNotified {
		t.Fatalf("delivered campaign must be recorded notified, got %q", rec.Status)
	}
	// Content sanity: title + campaign id present, routed to the system channel.
	msg := fake.lastMessage()
	if msg.Title == "" || msg.ChannelID != "chan-1" {
		t.Fatalf("unexpected notification: title=%q channel=%q", msg.Title, msg.ChannelID)
	}
	if !strings.Contains(msg.Message, "enabled-1") {
		t.Fatalf("notification should carry the campaign id in its body")
	}

	// Test 22: identical repeat sync -> zero additional.
	m.NotifyUpcomingDropCampaign(context.Background(), c)
	if fake.calls() != 1 {
		t.Fatalf("identical repeat must not re-notify, got %d sends", fake.calls())
	}
}

// Test 25: restart with the same DB -> not re-notified (durable dedupe).
func TestUpcomingSurvivesRestart(t *testing.T) {
	m1, fake1 := newUpcomingManager(t, true)
	c := upcomingCampaignModel("restart-1")
	m1.NotifyUpcomingDropCampaign(context.Background(), c)
	if fake1.calls() != 1 {
		t.Fatalf("first manager must send once, got %d", fake1.calls())
	}

	// A fresh manager over the SAME shared DB simulates a process restart. Use the
	// no-clear variant so the durable dedupe row from m1 survives, exactly as it
	// would across a real restart.
	m2, fake2 := newUpcomingManagerRaw(t, true)
	m2.NotifyUpcomingDropCampaign(context.Background(), c)
	if fake2.calls() != 0 {
		t.Fatalf("after restart the same campaign must not re-notify, got %d", fake2.calls())
	}
}

// Test 26: a new campaign ID is a new event.
func TestUpcomingNewCampaignIsNewEvent(t *testing.T) {
	m, fake := newUpcomingManager(t, true)
	m.NotifyUpcomingDropCampaign(context.Background(), upcomingCampaignModel("newid-a"))
	m.NotifyUpcomingDropCampaign(context.Background(), upcomingCampaignModel("newid-b"))
	if fake.calls() != 2 {
		t.Fatalf("two distinct campaign IDs must produce two events, got %d", fake.calls())
	}
}

// Test 30 + 31: provider failure -> pending + retry next sync -> one success, no repeat.
func TestUpcomingRetriesAfterProviderFailure(t *testing.T) {
	m, fake := newUpcomingManager(t, true)
	c := upcomingCampaignModel("retry-1")

	fake.setErr(errors.New("provider boom"))
	m.NotifyUpcomingDropCampaign(context.Background(), c)
	if fake.calls() != 1 {
		t.Fatalf("failed attempt must still call the provider once, got %d", fake.calls())
	}
	if rec := upcomingState(t, m, "retry-1"); rec.Status != UpcomingStatusPending || rec.Attempts != 1 {
		t.Fatalf("failed delivery must be pending with attempts=1, got %q attempts=%d", rec.Status, rec.Attempts)
	}

	// Next full sync: provider recovers -> one successful delivery.
	fake.setErr(nil)
	m.NotifyUpcomingDropCampaign(context.Background(), c)
	if fake.calls() != 2 {
		t.Fatalf("recovered provider must retry once, got %d total sends", fake.calls())
	}
	if rec := upcomingState(t, m, "retry-1"); rec.Status != UpcomingStatusNotified {
		t.Fatalf("successful retry must be notified, got %q", rec.Status)
	}

	// Further syncs must not repeat.
	m.NotifyUpcomingDropCampaign(context.Background(), c)
	if fake.calls() != 2 {
		t.Fatalf("delivered campaign must not resend, got %d", fake.calls())
	}
}

// Bounded retry: after maxUpcomingAttempts failures the alert is given up.
func TestUpcomingRetryIsBounded(t *testing.T) {
	orig := maxUpcomingAttempts
	maxUpcomingAttempts = 2
	t.Cleanup(func() { maxUpcomingAttempts = orig })

	m, fake := newUpcomingManager(t, true)
	fake.setErr(errors.New("always fails"))
	c := upcomingCampaignModel("bounded-1")

	for i := 0; i < 5; i++ {
		m.NotifyUpcomingDropCampaign(context.Background(), c)
	}
	if fake.calls() != 2 {
		t.Fatalf("retry must be bounded to %d attempts, got %d sends", maxUpcomingAttempts, fake.calls())
	}
}

// Test 29: channel missing / Discord unavailable -> suppressed, no send, no error.
func TestUpcomingNoDestinationSuppresses(t *testing.T) {
	// Sub-case: event enabled but no system channel configured.
	m, fake := newUpcomingManager(t, true)
	cfg := DefaultNotificationConfig()
	cfg.SystemEnabled = true
	cfg.SystemChannelID = "" // no destination
	cfg.UpcomingDropsEnabled = true
	if err := m.SaveConfig(&cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	m.NotifyUpcomingDropCampaign(context.Background(), upcomingCampaignModel("nochan-1"))
	if fake.calls() != 0 {
		t.Fatalf("missing channel must not send, got %d", fake.calls())
	}
	if rec := upcomingState(t, m, "nochan-1"); rec.Status != UpcomingStatusSuppressed {
		t.Fatalf("missing channel must record suppressed, got %q", rec.Status)
	}

	// Sub-case: no Discord provider at all.
	m2, _ := newUpcomingManager(t, true)
	m2.discord = nil
	m2.NotifyUpcomingDropCampaign(context.Background(), upcomingCampaignModel("noprov-1"))
	if rec := upcomingState(t, m2, "noprov-1"); rec.Status != UpcomingStatusSuppressed {
		t.Fatalf("no provider must record suppressed, got %q", rec.Status)
	}
}

// Section 10.1: a campaign first seen while the event was OFF is recorded seen
// (suppressed) and is NEVER back-filled after the event is later enabled.
func TestUpcomingNoBackfillAfterEnable(t *testing.T) {
	m, fake := newUpcomingManager(t, false) // disabled
	c := upcomingCampaignModel("backfill-1")
	m.NotifyUpcomingDropCampaign(context.Background(), c)
	if fake.calls() != 0 {
		t.Fatalf("disabled must not send, got %d", fake.calls())
	}

	// Operator enables the event later.
	cfg := DefaultNotificationConfig()
	cfg.SystemEnabled = true
	cfg.SystemChannelID = "chan-1"
	cfg.UpcomingDropsEnabled = true
	if err := m.SaveConfig(&cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	m.NotifyUpcomingDropCampaign(context.Background(), c)
	if fake.calls() != 0 {
		t.Fatalf("enabling the event must not back-fill an already-seen campaign, got %d sends", fake.calls())
	}
	if rec := upcomingState(t, m, "backfill-1"); rec.Status != UpcomingStatusSuppressed {
		t.Fatalf("previously-suppressed campaign must stay suppressed, got %q", rec.Status)
	}

	// But a brand-new campaign after enabling DOES notify.
	m.NotifyUpcomingDropCampaign(context.Background(), upcomingCampaignModel("backfill-new"))
	if fake.calls() != 1 {
		t.Fatalf("a new campaign after enabling must notify, got %d sends", fake.calls())
	}
}

// Test 32 + 33: concurrent identical processing -> exactly one notification.
func TestUpcomingConcurrentDeliversOnce(t *testing.T) {
	m, fake := newUpcomingManager(t, true)
	c := upcomingCampaignModel("concurrent-1")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.NotifyUpcomingDropCampaign(context.Background(), c)
		}()
	}
	wg.Wait()

	if fake.calls() != 1 {
		t.Fatalf("concurrent identical processing must deliver exactly once, got %d", fake.calls())
	}
	if rec := upcomingState(t, m, "concurrent-1"); rec.Status != UpcomingStatusNotified {
		t.Fatalf("campaign must end notified, got %q", rec.Status)
	}
}

// Test 34-36: migrations are idempotent and the dedupe key rejects duplicates.
func TestUpcomingMigrationIdempotentAndUniqueKey(t *testing.T) {
	// Re-registering the module on the already-populated shared DB is a no-op
	// (version gating) and must not error.
	if _, err := NewRepository(testDBHandle); err != nil {
		t.Fatalf("re-registering notifications module must be idempotent: %v", err)
	}
	repo, err := NewRepository(testDBHandle)
	if err != nil {
		t.Fatalf("NewRepository: %v", err)
	}
	// The table exists now; clear it so repeated -count runs start clean.
	if _, err := testDBHandle.Exec("DELETE FROM upcoming_campaign_notifications"); err != nil {
		t.Fatalf("reset dedupe table: %v", err)
	}

	notifType := string(NotificationTypeUpcomingCampaign)
	// Duplicate suppressed inserts coalesce onto one row.
	if err := repo.InsertUpcomingSuppressedIfAbsent("uniq-1", notifType, nowMillis()); err != nil {
		t.Fatalf("insert suppressed: %v", err)
	}
	if err := repo.InsertUpcomingSuppressedIfAbsent("uniq-1", notifType, nowMillis()); err != nil {
		t.Fatalf("duplicate suppressed insert must be safe: %v", err)
	}
	rec, err := repo.GetUpcomingNotifyState("uniq-1", notifType)
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if !rec.Found || rec.Status != UpcomingStatusSuppressed {
		t.Fatalf("unexpected row after duplicate insert: found=%v status=%q", rec.Found, rec.Status)
	}

	// Notified is terminal: a later suppressed-insert must not downgrade it.
	if err := repo.MarkUpcomingNotified("uniq-2", notifType, nowMillis()); err != nil {
		t.Fatalf("mark notified: %v", err)
	}
	if err := repo.InsertUpcomingSuppressedIfAbsent("uniq-2", notifType, nowMillis()); err != nil {
		t.Fatalf("insert-if-absent over notified must be safe: %v", err)
	}
	rec2, err := repo.GetUpcomingNotifyState("uniq-2", notifType)
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if rec2.Status != UpcomingStatusNotified {
		t.Fatalf("notified row must stay notified, got %q", rec2.Status)
	}
}
