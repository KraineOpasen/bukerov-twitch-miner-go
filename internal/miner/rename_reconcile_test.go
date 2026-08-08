package miner

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/pubsub"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamer"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/watcher"
)

// renameCapableAPI is a twitchClient fake whose login->ChannelID mapping is
// mutable at runtime, so a test can simulate Twitch reporting that a login
// was renamed (two different logins resolving to the SAME stable ID) between
// two ApplySettings calls. Falls back to "chan-"+login for any login not
// explicitly mapped, matching the package's other fakes.
type renameCapableAPI struct {
	mu   sync.Mutex
	ids  map[string]string
	fail map[string]bool
}

func newRenameCapableAPI() *renameCapableAPI {
	return &renameCapableAPI{ids: map[string]string{}, fail: map[string]bool{}}
}

func (f *renameCapableAPI) set(login, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ids[login] = id
}

// breakLogin makes every LATER resolution of login fail, the way a transient
// Twitch outage would. Seeding happens before it is called, so the roster
// still holds the ChannelID resolved back then while the config entry — as
// newRenameTestMiner writes it, and as a hand-written config.json has it —
// carries none. srap_test.go's unresolvableAPI offers the same seam but
// cannot also map two logins onto ONE stable id, which is what makes an
// apply a rename rather than an add+remove.
func (f *renameCapableAPI) breakLogin(login string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[login] = true
}

func (f *renameCapableAPI) GetChannelID(username string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail[username] {
		return "", errors.New("resolution unavailable this cycle")
	}
	if id, ok := f.ids[username]; ok {
		return id, nil
	}
	return "chan-" + username, nil
}
func (*renameCapableAPI) LoadChannelPointsContext(*models.Streamer) error { return nil }
func (*renameCapableAPI) CheckStreamerOnline(*models.Streamer) models.StatusTransition {
	return models.StatusTransition{}
}

// idTopic builds the topic a channelID (not a login) would be keyed under —
// renameCapableAPI's IDs are explicit, not derived from the login, so the
// capability_reconcile_test.go channelTopic helper (which assumes
// "chan-"+username) does not apply here.
func idTopic(tt pubsub.TopicType, channelID string) pubsub.Topic {
	return pubsub.NewTopic(tt, channelID)
}

// newRenameTestMiner builds a Miner over a seeded manager using
// renameCapableAPI, with the runtime capability reconciliation seams
// injected — no network, no pool, no IRC.
func newRenameTestMiner(t *testing.T, client *renameCapableAPI, seedLogins ...string) (*Miner, *fakeTopicReconciler, *fakeChatReconciler) {
	t.Helper()
	cfg := &config.Config{
		Username:         "tester",
		StreamerSettings: models.DefaultStreamerSettings(),
		Priority:         []config.Priority{config.PriorityOrder},
		RateLimits:       config.RateLimitSettings{MinuteWatchedInterval: 60},
	}
	for _, u := range seedLogins {
		cfg.Streamers = append(cfg.Streamers, config.StreamerConfig{Username: u})
	}

	mgr := streamer.NewManager(client, cfg.StreamerSettings)
	if added, _, _, _ := mgr.ApplySettings(cfg.Streamers, cfg.StreamerSettings); len(added) != len(seedLogins) {
		t.Fatalf("seeding failed: added=%d, want %d", len(added), len(seedLogins))
	}

	topics := newFakeTopicReconciler()
	chatRec := newFakeChatReconciler()
	m := &Miner{
		config:             cfg,
		streamers:          mgr,
		capabilityTopics:   topics,
		chatPresence:       chatRec,
		streamCheckTrigger: make(chan struct{}, 1),
		autoRedeemState:    make(map[string]*autoRedeemRuntime),
	}
	return m, topics, chatRec
}

// renameRuntimeStreamers builds a RuntimeSettings DTO from the miner's
// current config with its ONE streamer entry's login replaced, preserving
// any existing per-streamer settings override — mirroring how the Settings
// page posts a rename (the operator edits the login field, not the whole
// object).
func renameRuntimeStreamers(m *Miner, oldLogin, newLogin string) settings.RuntimeSettings {
	rs := m.GetRuntimeSettings()
	for i := range rs.Streamers {
		if rs.Streamers[i].Username == oldLogin {
			rs.Streamers[i].Username = newLogin
		}
	}
	return rs
}

// TestApplySettings_Rename_PubSubZeroChurn covers invariant H: a config-driven
// rename must produce ZERO PubSub churn — every per-channel topic (keyed by
// the immutable ChannelID) stays subscribed with no new LISTEN transition,
// since the rename never changes the ChannelID the topics are keyed by.
func TestApplySettings_Rename_PubSubZeroChurn(t *testing.T) {
	client := newRenameCapableAPI()
	client.set("oldlogin", "id-h")
	m, topics, _ := newRenameTestMiner(t, client, "oldlogin")

	_ = m.ApplySettings(context.Background(), m.GetRuntimeSettings()) // establish the initial topic set

	channelTopics := []pubsub.TopicType{
		pubsub.TopicVideoPlaybackByID,
		pubsub.TopicRaid,
		pubsub.TopicPredictionsChannel,
		pubsub.TopicCommunityMomentsChannel,
	}
	before := make(map[pubsub.TopicType]int, len(channelTopics))
	for _, tt := range channelTopics {
		before[tt] = topics.listenCount(idTopic(tt, "id-h"))
		if before[tt] == 0 {
			t.Fatalf("setup: topic %s never subscribed", tt)
		}
	}

	client.set("newlogin", "id-h")
	_ = m.ApplySettings(context.Background(), renameRuntimeStreamers(m, "oldlogin", "newlogin"))

	for _, tt := range channelTopics {
		if got := topics.listenCount(idTopic(tt, "id-h")); got != before[tt] {
			t.Errorf("topic %s LISTEN count changed by the rename: %d -> %d, want zero churn", tt, before[tt], got)
		}
		if !topics.has(idTopic(tt, "id-h")) {
			t.Errorf("topic %s missing after rename", tt)
		}
	}
}

// TestApplySettings_Rename_WatcherSlotContinuity covers invariant I at the
// miner-wiring level: a PURE rename (no added/removed roster members) must
// retain the exact same *models.Streamer pointer the watch loop already
// holds, and ApplySettings must not treat it as an add/remove (which is what
// would otherwise trigger watcher.UpdateStreamers and an index remap). The
// watcher's own remap logic — used defensively even when UpdateStreamers IS
// invoked — is covered separately in
// internal/watcher/rename_test.go(TestApplyStreamerList_RenameInPlacePreservesRotationState),
// since driving a REAL tick here would need a live Twitch client the fakes in
// this package don't provide.
func TestApplySettings_Rename_WatcherSlotContinuity(t *testing.T) {
	client := newRenameCapableAPI()
	client.set("oldlogin", "id-i")
	m, _, _ := newRenameTestMiner(t, client, "oldlogin")

	w := watcher.NewMinuteWatcher(nil, m.streamers.All(), m.config.Priority, m.config.RateLimits, nil)
	m.watcher = w

	before := m.streamers.Get("oldlogin")
	before.SetConfirmedOnline()
	before.Stream.Update("bid-continuity", "t", nil, nil, 5)

	client.set("newlogin", "id-i")
	added, removed, _, renamed := m.streamers.ApplySettings(
		[]config.StreamerConfig{{Username: "newlogin"}}, m.config.StreamerSettings)
	if len(added) != 0 || len(removed) != 0 {
		t.Fatalf("a pure rename must report NO added/removed roster members: added=%d removed=%d (this is what keeps watcher.UpdateStreamers from being called at all)",
			len(added), len(removed))
	}
	if len(renamed) != 1 {
		t.Fatalf("renamed = %d, want 1", len(renamed))
	}

	after := m.streamers.Get("newlogin")
	if after != before {
		t.Fatal("rename must retain the SAME *models.Streamer pointer (slot/status/history continuity)")
	}
	if m.streamers.Get("oldlogin") != nil {
		t.Fatal("the old login must no longer resolve to a streamer")
	}
	// The watch loop's own slice still holds the identical pointer (it was
	// never touched by UpdateStreamers), so its broadcast/status/history are
	// exactly what they were before the rename — no gap.
	if after.Stream.GetBroadcastID() != "bid-continuity" {
		t.Fatalf("broadcast continuity lost across the rename: got %q", after.Stream.GetBroadcastID())
	}
	if !after.GetIsOnline() {
		t.Fatal("online status must survive the rename untouched")
	}
}

// TestApplySettings_Rename_LeavesOldChatChannelOnce covers invariant K/I7 at the
// miner-wiring level: a config-driven rename must make reconcileRuntimeCapabilities
// leave the OLD login's IRC channel exactly once (the new login is (re)joined by
// the normal ToggleChat sweep, keyed by the retained streamer pointer). A baseline
// apply with no rename leaves nothing, and a repeated apply with no further rename
// adds no additional leave. Without this the miner-level chat-leave wiring would be
// untested (the chat-package test drives Leave manually).
func TestApplySettings_Rename_LeavesOldChatChannelOnce(t *testing.T) {
	client := newRenameCapableAPI()
	client.set("oldlogin", "id-k")
	m, _, chatRec := newRenameTestMiner(t, client, "oldlogin")

	_ = m.ApplySettings(context.Background(), m.GetRuntimeSettings()) // baseline: no rename yet
	if got := chatRec.leaveCount("oldlogin"); got != 0 {
		t.Fatalf("baseline apply left oldlogin %d times, want 0", got)
	}

	client.set("newlogin", "id-k")
	_ = m.ApplySettings(context.Background(), renameRuntimeStreamers(m, "oldlogin", "newlogin"))
	if got := chatRec.leaveCount("oldlogin"); got != 1 {
		t.Fatalf("rename must Leave(oldlogin) exactly once, got %d", got)
	}

	// Repeated apply with no further rename: no additional leave (renamed is empty).
	_ = m.ApplySettings(context.Background(), renameRuntimeStreamers(m, "oldlogin", "newlogin"))
	if got := chatRec.leaveCount("oldlogin"); got != 1 {
		t.Fatalf("repeated apply changed oldlogin leave count to %d, want 1 (no new rename)", got)
	}
}

// TestApplySettings_Rename_ConfigSurgeryAndAnalyticsIntegration is the
// end-to-end wiring test for BKM-006 item 5: after a rename, config.json's
// streamer entry is updated in place (login + ChannelID, settings pointer
// preserved), AutoRedeem is migrated, and the analytics history is migrated
// to the new login through the REAL analytics.Service/SQLite path (M: one
// streamer, new login, same channel identity end-to-end).
func TestApplySettings_Rename_ConfigSurgeryAndAnalyticsIntegration(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	svc, err := analytics.NewService(db, t.TempDir(), 0)
	if err != nil {
		t.Fatalf("new analytics service: %v", err)
	}
	if err := svc.Repository().RecordPoints("oldlogin", 1234, "WATCH"); err != nil {
		t.Fatalf("seed analytics points: %v", err)
	}

	client := newRenameCapableAPI()
	client.set("oldlogin", "id-wiring")
	m, _, _ := newRenameTestMiner(t, client, "oldlogin")
	m.analyticsSvc = svc
	m.config.AutoRedeem = map[string]config.AutoRedeemConfig{
		"oldlogin": {Enabled: true, Budget: 500, RewardIDs: []string{"reward-1"}},
	}

	client.set("newlogin", "id-wiring")
	_ = m.ApplySettings(context.Background(), renameRuntimeStreamers(m, "oldlogin", "newlogin"))

	// Config surgery: exactly one entry, new login, ChannelID stamped.
	if len(m.config.Streamers) != 1 {
		t.Fatalf("cfg.Streamers = %d entries, want 1", len(m.config.Streamers))
	}
	sc := m.config.Streamers[0]
	if sc.Username != "newlogin" {
		t.Errorf("cfg.Streamers[0].Username = %q, want newlogin", sc.Username)
	}
	if sc.ChannelID != "id-wiring" {
		t.Errorf("cfg.Streamers[0].ChannelID = %q, want id-wiring", sc.ChannelID)
	}

	// AutoRedeem migrated.
	if _, stillOld := m.config.AutoRedeem["oldlogin"]; stillOld {
		t.Error("AutoRedeem entry still present under the old login")
	}
	newAR, ok := m.config.AutoRedeem["newlogin"]
	if !ok || newAR.Budget != 500 || len(newAR.RewardIDs) != 1 {
		t.Errorf("AutoRedeem not migrated correctly: %+v (ok=%v)", newAR, ok)
	}

	// Analytics history followed the rename (same underlying row).
	data, err := svc.Repository().GetStreamerData("newlogin")
	if err != nil {
		t.Fatalf("get streamer data: %v", err)
	}
	if len(data.Series) != 1 {
		t.Fatalf("analytics points under new login = %d, want 1 (history must follow the rename)", len(data.Series))
	}
	oldData, _ := svc.Repository().GetStreamerData("oldlogin")
	if len(oldData.Series) != 0 {
		t.Fatalf("analytics points still under old login: %d, want 0", len(oldData.Series))
	}

	// Repeated identical apply is a no-op: still one entry, no error, no
	// duplicate migration attempt (RenameStreamer is idempotent by contract).
	_ = m.ApplySettings(context.Background(), renameRuntimeStreamers(m, "oldlogin", "newlogin"))
	if len(m.config.Streamers) != 1 {
		t.Fatalf("repeated apply changed entry count: %d, want 1", len(m.config.Streamers))
	}
}

// TestApplySettings_Rename_ConfigRestart_IDFirstReconstructsOneStreamer
// covers invariant G: after the rename's config surgery persists channelId
// and the new login, a full restart (a FRESH streamer.Manager loading that
// same persisted config) reconstructs exactly ONE streamer under the new
// login with the SAME settings — the old login is never seen again.
func TestApplySettings_Rename_ConfigRestart_IDFirstReconstructsOneStreamer(t *testing.T) {
	client := newRenameCapableAPI()
	client.set("oldlogin", "id-g")
	m, _, _ := newRenameTestMiner(t, client, "oldlogin")

	custom := false
	rs := m.GetRuntimeSettings()
	for i := range rs.Streamers {
		if rs.Streamers[i].Username == "oldlogin" {
			rs.Streamers[i].Settings = &settings.StreamerSettingsConfig{FollowRaid: &custom}
		}
	}
	_ = m.ApplySettings(context.Background(), rs)
	if got := m.streamers.Get("oldlogin").GetSettings().FollowRaid; got {
		t.Fatal("setup: custom FollowRaid=false did not apply")
	}

	client.set("newlogin", "id-g")
	_ = m.ApplySettings(context.Background(), renameRuntimeStreamers(m, "oldlogin", "newlogin"))

	persisted := m.config.Streamers
	if len(persisted) != 1 || persisted[0].Username != "newlogin" || persisted[0].ChannelID != "id-g" {
		t.Fatalf("persisted config wrong before restart: %+v", persisted)
	}

	// "Restart": a brand-new manager loads the SAME persisted config.
	fresh := streamer.NewManager(client, m.config.StreamerSettings)
	if err := fresh.LoadFromConfig(persisted, nil); err != nil {
		t.Fatalf("LoadFromConfig after restart: %v", err)
	}
	if fresh.Count() != 1 {
		t.Fatalf("post-restart count = %d, want 1", fresh.Count())
	}
	if fresh.Get("oldlogin") != nil {
		t.Fatal("post-restart: the old login must not resolve to anything")
	}
	restarted := fresh.Get("newlogin")
	if restarted == nil {
		t.Fatal("post-restart: new login missing")
	}
	if restarted.ChannelID != "id-g" {
		t.Errorf("post-restart ChannelID = %q, want id-g", restarted.ChannelID)
	}
	if restarted.GetSettings().FollowRaid {
		t.Error("post-restart settings did not survive (FollowRaid should still be false)")
	}

	// Applying the SAME persisted config again is a no-op (no renamed events,
	// roster unchanged).
	_, removed, changed, renamed := fresh.ApplySettings(persisted, m.config.StreamerSettings)
	if len(removed) != 0 || len(changed) != 0 || len(renamed) != 0 {
		t.Fatalf("repeated post-restart apply was not a no-op: removed=%d changed=%d renamed=%d",
			len(removed), len(changed), len(renamed))
	}
}

// TestApplySettings_Rename_PersistsRosterChannelIDWhenResolutionFails covers
// the SECOND stamping source on the rename path — the one the rename path,
// alone among the three, did not have. applySettingsWithRename stamps the
// candidate from this plan's OWN resolution only; a RETAINED streamer whose
// resolution fails this cycle is absent from that resolution, so the
// already-committed roster is the only thing that still knows its ChannelID.
// Without the roster stamp a transient Twitch hiccup, riding along with an
// unrelated rename, permanently erases from config.json an identity the
// process already held: finishApply's backfill repairs only memory, and it
// runs AFTER the write, so a cold restart reads the gap.
//
// This is the rename-path twin of
// TestApplySettingsRemovalPersistsRosterChannelIDWhenResolutionFails
// (srap_test.go), and the same coverage split applySettingsNoRename's step-2
// comment describes for its own two backfills.
func TestApplySettings_Rename_PersistsRosterChannelIDWhenResolutionFails(t *testing.T) {
	const oldLogin, newLogin, stale = "renameidold", "renameidnew", "renameidstale"
	const stableID = "id-rename-roster"

	client := newRenameCapableAPI()
	client.set(oldLogin, stableID)
	m, _, _ := newRenameTestMiner(t, client, oldLogin, stale)

	configPath := filepath.Join(t.TempDir(), "config.json")
	m.configPath = configPath
	if err := config.SaveConfig(configPath, m.config); err != nil {
		t.Fatalf("seed config file: %v", err)
	}

	// The roster resolved `stale` at seed time; the config never recorded it.
	// Both halves are preconditions — without them the test cannot tell the
	// roster source apart from the plan source.
	tracked := m.streamers.Get(stale)
	if tracked == nil || tracked.ChannelID != "chan-"+stale {
		t.Fatalf("seed precondition: roster ChannelID for %q = %+v, want %q", stale, tracked, "chan-"+stale)
	}
	for _, sc := range m.config.Streamers {
		if sc.Username == stale && sc.ChannelID != "" {
			t.Fatalf("seed precondition: config entry for %q already carries a ChannelID (%q); the test would not discriminate", stale, sc.ChannelID)
		}
	}

	// The rename resolves to the SAME stable id (that is what makes it a
	// rename rather than an add+remove); `stale` stops resolving entirely.
	client.set(newLogin, stableID)
	client.breakLogin(stale)

	rs := renameRuntimeStreamers(m, oldLogin, newLogin)

	// Path proof, static half: exactly one planned rename and NO removals, and
	// applySettings' switch is a pure function of those two lengths — so this
	// is applySettingsWithRename and cannot be either sibling. The plan
	// genuinely carries no id for `stale`, leaving the roster the only source.
	probe := m.streamers.PlanReconcile(
		settings.StreamersFromDTO(rs.Streamers),
		settings.StreamerSettingsFromDTO(rs.DefaultSettings),
		nil,
	)
	renames := probe.PlannedRenames()
	if len(renames) != 1 || renames[0].OldLogin != oldLogin || renames[0].NewLogin != newLogin {
		t.Fatalf("scenario does not exercise the rename path: PlannedRenames = %v, want exactly [%s -> %s]", renames, oldLogin, newLogin)
	}
	if got := probe.PlannedRemovals(m.streamers); len(got) != 0 {
		t.Fatalf("scenario is not a pure rename: PlannedRemovals = %v, want none", got)
	}
	if got, ok := probe.ResolvedChannelIDs()[stale]; ok && got != "" {
		t.Fatalf("precondition: the plan still resolved %q as %q; the roster half would not be discriminated", stale, got)
	}

	// Path proof, dynamic half: the barrier fires only on the removal and
	// rename commit points (applySettingsNoRename deliberately has no
	// bracket), so together with the static half the apply provably took the
	// rename path rather than merely being planned onto it.
	var phases []applyCommitPhase
	m.applyCommitBarrier = func(phase applyCommitPhase) { phases = append(phases, phase) }

	if err := m.ApplySettings(context.Background(), rs); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	if len(phases) != 2 || phases[0] != applyPreCommit || phases[1] != applyPostCommit {
		t.Fatalf("apply did not take a barrier-bracketed commit path (removal/rename): phases = %v", phases)
	}

	// The unresolvable streamer is kept, never dropped (fail-closed), and the
	// runtime still holds the identity it resolved at seed time.
	runtimeStale := m.streamers.Get(stale)
	if runtimeStale == nil {
		t.Fatalf("%q was dropped from the roster on a failed resolution", stale)
	}
	if runtimeStale.ChannelID != "chan-"+stale {
		t.Fatalf("runtime roster lost %q's ChannelID: got %q, want %q", stale, runtimeStale.ChannelID, "chan-"+stale)
	}

	loaded, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	// No old-login resurrection and no extra entry: exactly the two identities
	// that went in come out.
	if len(loaded.Streamers) != 2 {
		t.Fatalf("persisted config has %d entries, want exactly 2: %+v", len(loaded.Streamers), loaded.Streamers)
	}
	byLogin := map[string]config.StreamerConfig{}
	for _, sc := range loaded.Streamers {
		byLogin[sc.Username] = sc
	}
	if _, resurrected := byLogin[oldLogin]; resurrected {
		t.Errorf("persisted config still carries the OLD login %q: %+v", oldLogin, loaded.Streamers)
	}

	// The renamed identity keeps persisting its resolved stable id, exactly as
	// before — the plan-source stamp must not be traded away for the roster one.
	renamedEntry, ok := byLogin[newLogin]
	if !ok {
		t.Fatalf("persisted config missing the renamed streamer %q: %+v", newLogin, loaded.Streamers)
	}
	if renamedEntry.ChannelID != stableID {
		t.Errorf("persisted config lost the renamed entry's resolved ChannelID for %q: got %q, want %q", newLogin, renamedEntry.ChannelID, stableID)
	}

	// The defect, stated as the divergence itself: the process holds the id in
	// memory at the very moment the file it just wrote does not.
	staleEntry, ok := byLogin[stale]
	if !ok {
		t.Fatalf("persisted config missing retained streamer %q: %+v", stale, loaded.Streamers)
	}
	if staleEntry.ChannelID != runtimeStale.ChannelID {
		t.Errorf("runtime and config.json disagree about %q's identity after a rename-carrying apply: runtime has %q, the file has %q — a cold restart reads the file, so the identity the process already held is erased",
			stale, runtimeStale.ChannelID, staleEntry.ChannelID)
	}
}
