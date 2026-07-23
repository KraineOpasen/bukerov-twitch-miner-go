package miner

import (
	"errors"
	"sync"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamer"
)

// TestApplyConfigRenames_UpdatesUsernameKeepsSettingsPointer proves the
// common case: renaming a config entry updates its Username in place and
// leaves its *models.StreamerSettings pointer (and therefore whatever
// override it carries) completely untouched.
func TestApplyConfigRenames_UpdatesUsernameKeepsSettingsPointer(t *testing.T) {
	custom := &models.StreamerSettings{FollowRaid: false}
	cfg := &config.Config{Streamers: []config.StreamerConfig{
		{Username: "oldlogin", Settings: custom},
		{Username: "untouched"},
	}}

	applyConfigRenames(cfg, []streamer.RenameEvent{{OldLogin: "oldlogin", NewLogin: "newlogin", ChannelID: "id-1"}})

	if len(cfg.Streamers) != 2 {
		t.Fatalf("entry count changed: %d, want 2", len(cfg.Streamers))
	}
	if cfg.Streamers[0].Username != "newlogin" {
		t.Errorf("Username = %q, want newlogin", cfg.Streamers[0].Username)
	}
	if cfg.Streamers[0].ChannelID != "id-1" {
		t.Errorf("ChannelID = %q, want id-1", cfg.Streamers[0].ChannelID)
	}
	if cfg.Streamers[0].Settings != custom {
		t.Error("Settings pointer was replaced, not preserved")
	}
	if cfg.Streamers[1].Username != "untouched" {
		t.Error("an unrelated entry was mutated by the rename")
	}
}

// TestApplyConfigRenames_CoalescesWhenBothEntriesExist covers the case where
// the operator's config already lists BOTH the old and new login (or the
// manager's own duplicate-config coalesce produced this at the runtime
// level): the stale old-login entry is dropped, the surviving new-login
// entry's OWN settings win, and ChannelID is stamped onto it.
func TestApplyConfigRenames_CoalescesWhenBothEntriesExist(t *testing.T) {
	newSettings := &models.StreamerSettings{FollowRaid: true}
	cfg := &config.Config{Streamers: []config.StreamerConfig{
		{Username: "oldlogin", Settings: &models.StreamerSettings{FollowRaid: false}},
		{Username: "newlogin", Settings: newSettings},
	}}

	applyConfigRenames(cfg, []streamer.RenameEvent{{OldLogin: "oldlogin", NewLogin: "newlogin", ChannelID: "id-2"}})

	if len(cfg.Streamers) != 1 {
		t.Fatalf("entry count = %d, want 1 (coalesced)", len(cfg.Streamers))
	}
	if cfg.Streamers[0].Username != "newlogin" {
		t.Errorf("Username = %q, want newlogin", cfg.Streamers[0].Username)
	}
	if cfg.Streamers[0].Settings != newSettings {
		t.Error("the surviving entry's OWN settings must win, not the dropped old entry's")
	}
	if cfg.Streamers[0].ChannelID != "id-2" {
		t.Errorf("ChannelID = %q, want id-2", cfg.Streamers[0].ChannelID)
	}
}

// TestApplyConfigRenames_CaseInsensitiveMatch: the DTO round-trip can carry
// whatever case the operator typed; matching against OldLogin/NewLogin
// (always lowercase, from the manager) must be case-insensitive.
func TestApplyConfigRenames_CaseInsensitiveMatch(t *testing.T) {
	cfg := &config.Config{Streamers: []config.StreamerConfig{{Username: "OldLogin"}}}
	applyConfigRenames(cfg, []streamer.RenameEvent{{OldLogin: "oldlogin", NewLogin: "newlogin", ChannelID: "id-3"}})
	if len(cfg.Streamers) != 1 || cfg.Streamers[0].Username != "newlogin" {
		t.Fatalf("case-insensitive rename match failed: %+v", cfg.Streamers)
	}
}

// TestMigrateAutoRedeem_MovesEntry proves the normal migration path.
func TestMigrateAutoRedeem_MovesEntry(t *testing.T) {
	cfg := &config.Config{AutoRedeem: map[string]config.AutoRedeemConfig{
		"oldlogin": {Enabled: true, Budget: 250, RewardIDs: []string{"r1"}},
	}}
	migrateAutoRedeem(cfg, "oldlogin", "newlogin")

	if _, ok := cfg.AutoRedeem["oldlogin"]; ok {
		t.Error("old entry still present after migration")
	}
	got, ok := cfg.AutoRedeem["newlogin"]
	if !ok || got.Budget != 250 || len(got.RewardIDs) != 1 {
		t.Errorf("new entry wrong: %+v (ok=%v)", got, ok)
	}
}

// TestMigrateAutoRedeem_DoesNotClobberExistingDestination_RemovesOldKey: if
// the new login already has its own AutoRedeem entry, migration must leave
// that destination's configuration untouched (never silently overwritten by
// the old entry) AND must delete the now-orphaned old-login key — oldLogin no
// longer identifies any tracked streamer after the rename, so leaving it
// behind would be dead config that could never be reached or cleaned up again.
func TestMigrateAutoRedeem_DoesNotClobberExistingDestination_RemovesOldKey(t *testing.T) {
	cfg := &config.Config{AutoRedeem: map[string]config.AutoRedeemConfig{
		"oldlogin": {Enabled: true, Budget: 100},
		"newlogin": {Enabled: true, Budget: 999},
	}}
	migrateAutoRedeem(cfg, "oldlogin", "newlogin")

	if got := cfg.AutoRedeem["newlogin"].Budget; got != 999 {
		t.Errorf("destination AutoRedeem entry was clobbered: budget=%d, want 999 (untouched)", got)
	}
	if _, ok := cfg.AutoRedeem["oldlogin"]; ok {
		t.Error("orphaned old-login entry must be removed once the destination's own entry wins")
	}
	if len(cfg.AutoRedeem) != 1 {
		t.Errorf("AutoRedeem has %d entries, want 1 (only the surviving new-login entry)", len(cfg.AutoRedeem))
	}
}

// TestBackfillChannelIDs_MatchesByCurrentLoginCaseInsensitive proves the
// backfill stamps ChannelID onto every cfg.Streamers entry by matching the
// RECONCILED ROSTER's current login, case-insensitively, and never touches
// an entry it cannot match.
func TestBackfillChannelIDs_MatchesByCurrentLoginCaseInsensitive(t *testing.T) {
	cfg := &config.Config{Streamers: []config.StreamerConfig{
		{Username: "Alpha"},
		{Username: "unresolved"},
	}}
	roster := []*models.Streamer{
		models.NewStreamer("alpha", models.DefaultStreamerSettings()),
	}
	roster[0].ChannelID = "id-alpha"

	backfillChannelIDs(cfg, roster)

	if cfg.Streamers[0].ChannelID != "id-alpha" {
		t.Errorf("ChannelID = %q, want id-alpha", cfg.Streamers[0].ChannelID)
	}
	if cfg.Streamers[1].ChannelID != "" {
		t.Errorf("unresolved entry got a ChannelID it shouldn't have: %q", cfg.Streamers[1].ChannelID)
	}
}

// fakeRenameAnalytics records RenameStreamer calls for
// migrateRenamesToPersistence's unit tests, optionally returning a
// pre-programmed error to exercise the conflict-tolerant path.
type fakeRenameAnalytics struct {
	mu    sync.Mutex
	calls []struct{ old, new string }
	err   error
}

func (f *fakeRenameAnalytics) RenameStreamer(oldName, newName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, struct{ old, new string }{oldName, newName})
	return f.err
}

// TestMigrateRenamesToPersistence_CallsAnalyticsOncePerEvent proves the
// straightforward wiring: exactly one RenameStreamer call per RenameEvent,
// with the correct old/new logins.
func TestMigrateRenamesToPersistence_CallsAnalyticsOncePerEvent(t *testing.T) {
	fa := &fakeRenameAnalytics{}
	m := &Miner{}
	m.migrateRenamesToPersistence([]streamer.RenameEvent{
		{OldLogin: "a-old", NewLogin: "a-new", ChannelID: "id-a"},
		{OldLogin: "b-old", NewLogin: "b-new", ChannelID: "id-b"},
	}, fa)

	if len(fa.calls) != 2 {
		t.Fatalf("RenameStreamer called %d times, want 2", len(fa.calls))
	}
	if fa.calls[0].old != "a-old" || fa.calls[0].new != "a-new" {
		t.Errorf("call 0 = %+v", fa.calls[0])
	}
	if fa.calls[1].old != "b-old" || fa.calls[1].new != "b-new" {
		t.Errorf("call 1 = %+v", fa.calls[1])
	}
}

// TestMigrateRenamesToPersistence_ConflictIsNonFatal: an analytics rename
// conflict must be tolerated (logged, not propagated/panicked) — it must
// never abort processing of the remaining rename events.
func TestMigrateRenamesToPersistence_ConflictIsNonFatal(t *testing.T) {
	fa := &fakeRenameAnalytics{err: errors.New("simulated conflict")}
	m := &Miner{}

	// Must not panic and must still process both events.
	m.migrateRenamesToPersistence([]streamer.RenameEvent{
		{OldLogin: "a-old", NewLogin: "a-new", ChannelID: "id-a"},
		{OldLogin: "b-old", NewLogin: "b-new", ChannelID: "id-b"},
	}, fa)

	if len(fa.calls) != 2 {
		t.Fatalf("RenameStreamer called %d times despite the first conflict, want 2 (never aborts the batch)", len(fa.calls))
	}
}

// TestMigrateRenamesToPersistence_NilAnalyticsService: a nil analytics
// service (analytics disabled/unavailable) must not panic — only the
// privacy-safe log line is emitted.
func TestMigrateRenamesToPersistence_NilAnalyticsService(t *testing.T) {
	m := &Miner{}
	m.migrateRenamesToPersistence([]streamer.RenameEvent{
		{OldLogin: "a-old", NewLogin: "a-new", ChannelID: "id-a"},
	}, nil)
}
