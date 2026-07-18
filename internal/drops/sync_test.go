package drops

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

func nowMinusHours(h int) time.Time { return time.Now().Add(-time.Duration(h) * time.Hour) }
func nowPlusHours(h int) time.Time  { return time.Now().Add(time.Duration(h) * time.Hour) }

// fakeDropsClient is a scripted twitchClient for exercising the whole
// syncCampaigns pipeline without a live Twitch connection. PostGQL is
// dispatched by operation name so ViewerDropsDashboard and Inventory can be
// answered independently, and GetDropCampaignDetails is keyed by campaign ID.
type fakeDropsClient struct {
	dashboard    map[string]interface{}
	inventory    map[string]interface{}
	inventoryErr error
	details      map[string]map[string]interface{}
	// detailsErr, when set, fails EVERY GetDropCampaignDetails call (an
	// operation-wide outage such as a stale persisted-query hash);
	// detailErrByID fails only the given campaign (a campaign-specific error).
	detailsErr    error
	detailErrByID map[string]error

	// fullSyncSignal, when non-nil, receives one non-blocking signal per full
	// sync (each ViewerDropsDashboard call), letting a test observe the
	// background loop's cadence. Set before Start; the maps are read-only after
	// construction, so the loop goroutine touches no mutable shared state.
	fullSyncSignal chan struct{}
}

func (f *fakeDropsClient) PostGQL(op constants.GQLOperation) (map[string]interface{}, error) {
	switch op.OperationName {
	case "ViewerDropsDashboard":
		if f.fullSyncSignal != nil {
			select {
			case f.fullSyncSignal <- struct{}{}:
			default:
			}
		}
		return f.dashboard, nil
	case "Inventory":
		if f.inventoryErr != nil {
			return nil, f.inventoryErr
		}
		return f.inventory, nil
	default:
		return map[string]interface{}{}, nil
	}
}

func (f *fakeDropsClient) GetDropCampaignDetails(campaignID string) (map[string]interface{}, error) {
	if f.detailsErr != nil {
		return nil, f.detailsErr
	}
	if err := f.detailErrByID[campaignID]; err != nil {
		return nil, err
	}
	return f.details[campaignID], nil
}

func (f *fakeDropsClient) ClaimDrop(*models.Drop) (bool, error) { return false, nil }

// dashboardResponse wraps campaign summaries the way ViewerDropsDashboard does.
func dashboardResponse(campaigns ...map[string]interface{}) map[string]interface{} {
	list := make([]interface{}, 0, len(campaigns))
	for _, c := range campaigns {
		list = append(list, c)
	}
	return map[string]interface{}{
		"data": map[string]interface{}{
			"currentUser": map[string]interface{}{
				"dropCampaigns": list,
			},
		},
	}
}

// emptyInventoryResponse is an Inventory response with no in-progress
// campaigns and no claim history, so syncWithInventory/applyClaimHistory are
// no-ops and the test isolates the dashboard+details path.
func emptyInventoryResponse() map[string]interface{} {
	return map[string]interface{}{
		"data": map[string]interface{}{
			"currentUser": map[string]interface{}{
				"inventory": map[string]interface{}{},
			},
		},
	}
}

// inventoryWithInProgress is an Inventory response carrying the given
// dropCampaignsInProgress entries, used to exercise the inventory-recovery path.
func inventoryWithInProgress(campaigns ...map[string]interface{}) map[string]interface{} {
	list := make([]interface{}, 0, len(campaigns))
	for _, c := range campaigns {
		list = append(list, c)
	}
	return map[string]interface{}{
		"data": map[string]interface{}{
			"currentUser": map[string]interface{}{
				"inventory": map[string]interface{}{
					"dropCampaignsInProgress": list,
				},
			},
		},
	}
}

// TestLoopAdoptsRuntimeCampaignSyncInterval is the regression guard for the
// dead runtime-interval bug: the full-sync loop used to create a time.Ticker
// once at startup, so a CampaignSyncInterval change via UpdateSettings (the
// Settings page path) never reached it — contradicting UpdateSettings' own doc
// contract. The loop must re-read the interval each cycle. Uses a sub-second
// intervalUnit so the cadence is observable without waiting real minutes.
func TestLoopAdoptsRuntimeCampaignSyncInterval(t *testing.T) {
	signal := make(chan struct{}, 64)
	client := &fakeDropsClient{
		dashboard:      dashboardResponse(),
		inventory:      emptyInventoryResponse(),
		details:        map[string]map[string]interface{}{},
		fullSyncSignal: signal,
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{
		CampaignSyncInterval:     50,      // × ms unit below = 50ms fast startup cadence
		DropProgressSyncInterval: 100_000, // keep the lightweight loop quiet during the test
	}, nil)
	tracker.intervalUnit = time.Millisecond // set before Start (happens-before the loop goroutine)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tracker.Start(ctx)

	// The loop is cycling at the fast startup interval: several full syncs land
	// quickly (proves the loop is actually running before we change anything).
	for i := 0; i < 3; i++ {
		select {
		case <-signal:
		case <-time.After(2 * time.Second):
			t.Fatalf("startup interval did not drive repeated full syncs (saw %d)", i)
		}
	}

	// Runtime change to a very slow cadence via the same path the Settings page
	// uses. A fixed-ticker loop would ignore this entirely.
	tracker.UpdateSettings(config.RateLimitSettings{
		CampaignSyncInterval:     100_000, // 100s in ms units
		DropProgressSyncInterval: 100_000,
	})

	// The fixed loop re-reads the interval each cycle, so after the single
	// already-in-flight old-interval sync it adopts the slow cadence. Absorb the
	// in-flight sync plus any backlog for a bounded window, then drain.
	drainDeadline := time.After(250 * time.Millisecond)
drain:
	for {
		select {
		case <-signal:
		case <-drainDeadline:
			break drain
		}
	}

	// The slow interval must now be in effect: no further full sync for a window
	// spanning many old intervals. The buggy fixed-ticker loop would fire ~12
	// times here.
	select {
	case <-signal:
		t.Fatal("full-sync loop kept firing at the startup interval — the runtime CampaignSyncInterval change was ignored")
	case <-time.After(600 * time.Millisecond):
	}
}

// TestSyncProgressImmediateRevisionBump pins §9: a confirmed progress update
// republishes the shared campaign snapshot immediately (bumping the revision via
// light_sync) rather than waiting out the full campaign sync, so Overview and
// Drops — both reading the pool the same revision tags — never lag behind
// Twitch-confirmed progress. An unchanged observation must not bump the revision
// (no spurious churn). The full sync tags the pool full_sync.
func TestSyncProgressImmediateRevisionBump(t *testing.T) {
	invAt := func(minutes float64) map[string]interface{} {
		return inventoryWithInProgress(map[string]interface{}{
			"id":             "campaign-wot",
			"name":           "WoT Drops",
			"game":           map[string]interface{}{"id": "game-wot", "displayName": "World of Tanks"},
			"timeBasedDrops": []interface{}{inProgressDrop("drop-1", "Garage Slot", 120, minutes, false)},
		})
	}
	client := &fakeDropsClient{
		dashboard: dashboardResponse(),
		inventory: invAt(45),
		details:   map[string]map[string]interface{}{},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tracker.syncCampaigns()

	st := tracker.SyncStatus()
	if st.Revision == 0 || st.UpdateSource != "full_sync" {
		t.Fatalf("after full sync: revision=%d source=%q, want >0 / full_sync", st.Revision, st.UpdateSource)
	}
	rev1 := st.Revision
	if got := tracker.Campaigns(); len(got) != 1 || got[0].Drops[0].CurrentMinutesWatched != 45 {
		t.Fatalf("tracked drop should start at 45 min, got %+v", got)
	}

	// Confirmed progress advances to 60 minutes in the inventory.
	client.inventory = invAt(60)
	tracker.syncProgress()

	st = tracker.SyncStatus()
	if st.Revision != rev1+1 || st.UpdateSource != "light_sync" {
		t.Fatalf("progress sync should bump revision to %d via light_sync, got %d/%q", rev1+1, st.Revision, st.UpdateSource)
	}
	if got := tracker.Campaigns(); got[0].Drops[0].CurrentMinutesWatched != 60 {
		t.Errorf("shared snapshot must reflect confirmed progress immediately: got %d, want 60", got[0].Drops[0].CurrentMinutesWatched)
	}

	// An unchanged observation must not bump the revision.
	tracker.syncProgress()
	if r := tracker.Revision(); r != rev1+1 {
		t.Errorf("unchanged progress must not bump the revision: got %d, want %d", r, rev1+1)
	}
}

// TestSyncProgressDoesNotDropWindowlessRecoveredCampaign is the direct
// regression guard for the intermittent 3 MoE loss: a campaign recovered from
// the inventory whose drops have NO per-drop date window (the common inventory
// shape) must SURVIVE the lightweight progress sync intact — not be emptied by
// ClearClaimedDrops and dropped out of the tracked set (and thus out of
// activeCampaignGames / directory discovery) two minutes after every full sync.
func TestSyncProgressDoesNotDropWindowlessRecoveredCampaign(t *testing.T) {
	prog := map[string]interface{}{
		"id":             "campaign-3moe",
		"name":           "3 MoE Challenge",
		"game":           map[string]interface{}{"id": "game-wot", "displayName": "World of Tanks"},
		"timeBasedDrops": []interface{}{inProgressDrop("drop-1", "3 MoE Reward", 120, 45, false)}, // no startAt/endAt
	}
	client := &fakeDropsClient{
		dashboard: dashboardResponse(),
		inventory: inventoryWithInProgress(prog),
		details:   map[string]map[string]interface{}{},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tracker.syncCampaigns()

	if got := tracker.Campaigns(); len(got) != 1 || len(got[0].Drops) != 1 {
		t.Fatalf("full sync should recover 1 campaign with 1 drop, got %d campaigns", len(got))
	}
	revAfterFull := tracker.Revision()

	// A progress sync over the SAME inventory (no advance) must not strip the
	// date-less drop and must not churn the campaign out of the tracked set.
	tracker.syncProgress()

	got := tracker.Campaigns()
	if len(got) != 1 {
		t.Fatalf("progress sync dropped the recovered campaign: %d campaigns tracked", len(got))
	}
	if len(got[0].Drops) != 1 {
		t.Fatalf("progress sync stripped the recovered campaign's date-less drops: %d drops left", len(got[0].Drops))
	}
	if got[0].Game == nil || got[0].Game.DisplayName != "World of Tanks" {
		t.Errorf("game identity lost through progress sync: %+v", got[0].Game)
	}
	// Nothing changed, so no spurious republish.
	if r := tracker.Revision(); r != revAfterFull {
		t.Errorf("unchanged progress sync must not bump the revision: got %d, want %d", r, revAfterFull)
	}
}

// TestStartRunsImmediateInitialSync pins §8.1/§8.2: Start runs a full campaign
// sync immediately, before waiting out the first CampaignSyncInterval — so a
// campaign already live when the miner starts is discovered at once, not up to a
// full interval later. The interval here is effectively infinite, so the sync
// observed can only be the immediate startup one.
func TestStartRunsImmediateInitialSync(t *testing.T) {
	signal := make(chan struct{}, 4)
	client := &fakeDropsClient{
		dashboard:      dashboardResponse(),
		inventory:      emptyInventoryResponse(),
		details:        map[string]map[string]interface{}{},
		fullSyncSignal: signal,
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{
		CampaignSyncInterval:     100_000, // × ms unit = effectively never on its own
		DropProgressSyncInterval: 100_000,
	}, nil)
	tracker.intervalUnit = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tracker.Start(ctx)

	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not run an immediate initial full sync before the first interval")
	}
}

// TestUpdateGameFilterTriggersImmediateResync pins §8.3: a Drops game-filter
// change forces an immediate full campaign resync instead of waiting out the
// (here effectively infinite) CampaignSyncInterval, so a re-filter — or a game
// newly allowed — takes effect within seconds. The coalescing campaignResync
// channel + fullSyncMu guarantee no parallel sync.
func TestUpdateGameFilterTriggersImmediateResync(t *testing.T) {
	signal := make(chan struct{}, 64)
	client := &fakeDropsClient{
		dashboard:      dashboardResponse(),
		inventory:      emptyInventoryResponse(),
		details:        map[string]map[string]interface{}{},
		fullSyncSignal: signal,
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{
		CampaignSyncInterval:     100_000,
		DropProgressSyncInterval: 100_000,
	}, nil)
	tracker.intervalUnit = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tracker.Start(ctx)

	// Absorb the startup sync.
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("no initial full sync at startup")
	}
	// Drain any buffered startup signals so the next receive is the resync.
	for len(signal) > 0 {
		<-signal
	}

	// On the huge interval nothing more would fire on its own; a filter change
	// must force a sync promptly.
	tracker.UpdateGameFilter([]string{"game-wot"}, nil)
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("game-filter change did not trigger an immediate campaign resync")
	}
}

// TestRequestManualSyncCooldown pins §8.11/§13.14: the manual "Sync Drops now"
// action triggers a resync, then is cooldown-gated so it can't storm, and never
// queues more than one pending resync (coalesced).
func TestRequestManualSyncCooldown(t *testing.T) {
	old := manualSyncCooldown
	manualSyncCooldown = 50 * time.Millisecond
	defer func() { manualSyncCooldown = old }()

	tracker := NewDropsTracker(&fakeDropsClient{}, nil, config.RateLimitSettings{}, nil)

	if r := tracker.RequestManualSync(); !r.Triggered {
		t.Fatal("first manual sync must trigger")
	}
	// Exactly one resync queued (no storm).
	select {
	case <-tracker.campaignResync:
	default:
		t.Error("first manual sync should have queued a resync")
	}

	r2 := tracker.RequestManualSync()
	if r2.Triggered {
		t.Error("manual sync within cooldown must not trigger")
	}
	if r2.RetryAfter <= 0 || r2.RetryAfter > manualSyncCooldown {
		t.Errorf("rejected manual sync RetryAfter = %v, want in (0, %v]", r2.RetryAfter, manualSyncCooldown)
	}
	select {
	case <-tracker.campaignResync:
		t.Error("cooldown-rejected sync must not queue another resync")
	default:
	}

	time.Sleep(60 * time.Millisecond)
	if r := tracker.RequestManualSync(); !r.Triggered {
		t.Error("manual sync after cooldown must trigger again")
	}
}

// TestSyncCampaignsRecoversInventoryCampaignAndReportsIt is the composition
// guard for the two fixes landing together: the inventory-recovery path (a
// campaign live in dropCampaignsInProgress that the dashboard/details path
// never produced) must fold into the tracked set, AND the observability layer
// must attribute it to recovery (dashboardCampaigns=0, recovered=1) rather than
// to the dashboard. This proves the two approaches reinforce each other instead
// of duplicating or masking one another.
func TestSyncCampaignsRecoversInventoryCampaignAndReportsIt(t *testing.T) {
	prog := map[string]interface{}{
		"id":   "campaign-wot",
		"name": "World of Tanks AMD Summer Arena Drops#2",
		"game": map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
		"timeBasedDrops": []interface{}{
			// 118/120 min: ~99% done, not yet claimable (no instance ID).
			inProgressDrop("drop-1", "Garage Slot", 120, 118, false),
		},
	}

	client := &fakeDropsClient{
		dashboard: dashboardResponse(), // dashboard yields nothing
		inventory: inventoryWithInProgress(prog),
		details:   map[string]map[string]interface{}{},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tracker.syncCampaigns()

	got := tracker.Campaigns()
	if len(got) != 1 {
		t.Fatalf("expected 1 recovered campaign, got %d", len(got))
	}
	if !got[0].InInventory {
		t.Error("expected the recovered campaign to be marked InInventory")
	}
	if got[0].Name != "World of Tanks AMD Summer Arena Drops#2" {
		t.Errorf("unexpected recovered campaign name %q", got[0].Name)
	}

	status := tracker.SyncStatus()
	if status.DashboardCampaigns != 0 {
		t.Errorf("expected dashboardCampaigns=0 (dashboard was empty), got %d", status.DashboardCampaigns)
	}
	if status.RecoveredCampaigns != 1 {
		t.Errorf("expected recoveredCampaigns=1 (recovered from inventory), got %d", status.RecoveredCampaigns)
	}
	if status.TrackedCampaigns != 1 {
		t.Errorf("expected trackedCampaigns=1, got %d", status.TrackedCampaigns)
	}
}

// channelRestrictedInProgress builds an inventory dropCampaignsInProgress entry
// for a CHANNEL-RESTRICTED campaign (Twitch's allow.channels) whose game node
// carries only `displayName` — the exact shape the real Inventory query returns
// (game: { id, displayName }, no `name`). Using displayName-only here is
// deliberate: it proves the recovery path preserves the game identity that
// activeCampaignGames keys on even without a `name` field.
func channelRestrictedInProgress(id, name, gameID, gameDisplayName, dropName string, channelIDs ...string) map[string]interface{} {
	chans := make([]interface{}, 0, len(channelIDs))
	for _, c := range channelIDs {
		chans = append(chans, map[string]interface{}{"id": c})
	}
	return map[string]interface{}{
		"id":             id,
		"name":           name,
		"game":           map[string]interface{}{"id": gameID, "displayName": gameDisplayName},
		"allow":          map[string]interface{}{"channels": chans},
		"timeBasedDrops": []interface{}{inProgressDrop("d-"+id, dropName, 120, 45, false)},
	}
}

// TestSyncRecoversAllowedInventoryCampaignAmidBlacklistAndForeign is the §6/§7
// composite regression guard for the 3 MoE Challenge loss chain. It proves the
// deterministic pipeline Inventory response -> parsing -> recovery -> d.campaigns
// survives the realistic mix Twitch returns in one inventory:
//
//   - an allowed, channel-restricted, inventory-only campaign (3 MoE Challenge,
//     World of Tanks) the dashboard/details path never produced — must be
//     recovered, keep its game identity (id + displayName) AND its channel
//     restrictions, and end up the single tracked campaign;
//   - a blacklisted campaign (EWC 2026) — recovered raw but dropped by the
//     blacklist, never by the game filter;
//   - a foreign-game campaign (War Thunder) — recovered raw but dropped by the
//     strict game filter.
//
// The preserved game identity is what lets discovery.activeCampaignGames key on
// "world of tanks" (see discovery.TestActiveCampaignGamesKeysOnDisplayNameOnly),
// so directory discovery queries WoT channels — the step whose absence the
// original report observed as a *consequence* of the lost campaign.
func TestSyncRecoversAllowedInventoryCampaignAmidBlacklistAndForeign(t *testing.T) {
	moe := channelRestrictedInProgress(
		"campaign-3moe", "3 MoE Challenge", "game-wot", "World of Tanks",
		"3 Marks of Excellence Reward", "chan-mouzakrobat", "chan-skill4ltu",
	)
	ewc := map[string]interface{}{
		"id":             "campaign-ewc",
		"name":           "EWC 2026",
		"game":           map[string]interface{}{"id": "game-wot", "displayName": "World of Tanks"},
		"timeBasedDrops": []interface{}{inProgressDrop("d-ewc", "EWC 2026 Sticker", 120, 30, false)},
	}
	foreign := foreignInProgress("campaign-foreign", "War Thunder Drops", "game-warthunder", "War Thunder")

	client := &fakeDropsClient{
		dashboard: dashboardResponse(), // dashboard empty: dashboardCampaigns must be 0
		inventory: inventoryWithInProgress(moe, ewc, foreign),
		details:   map[string]map[string]interface{}{},
	}
	// Blacklist "EWC"; strict game filter = World of Tanks only.
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, []string{"EWC"})
	tracker.UpdateGameFilter([]string{"game-wot"}, nil)
	tracker.syncCampaigns()

	got := tracker.Campaigns()
	if len(got) != 1 {
		t.Fatalf("expected only 3 MoE tracked, got %d: %v", len(got), keptIDs(got))
	}
	c := got[0]
	if c.ID != "campaign-3moe" || c.Name != "3 MoE Challenge" {
		t.Errorf("wrong tracked campaign: %s / %q", c.ID, c.Name)
	}
	if !c.InInventory {
		t.Error("recovered campaign must be marked InInventory")
	}
	// Game identity survives recovery (the activeCampaignGames link).
	if c.Game == nil || c.Game.ID != "game-wot" || c.Game.DisplayName != "World of Tanks" {
		t.Errorf("game identity not preserved through recovery: %+v", c.Game)
	}
	// Channel restrictions survive recovery.
	if !c.IsChannelRestricted() {
		t.Error("channel restrictions must be preserved on the recovered campaign")
	}
	if !c.AllowsChannel("chan-mouzakrobat") || c.AllowsChannel("chan-not-allowed") {
		t.Errorf("allowed-channel set not preserved: %v", c.Channels)
	}

	st := tracker.SyncStatus()
	if st.DashboardCampaigns != 0 {
		t.Errorf("dashboardCampaigns = %d, want 0 (dashboard was empty)", st.DashboardCampaigns)
	}
	if st.RecoveredCampaigns != 3 {
		t.Errorf("recoveredCampaigns = %d, want 3 (raw pre-filter inventory recovery)", st.RecoveredCampaigns)
	}
	if st.TrackedCampaigns != 1 {
		t.Errorf("trackedCampaigns = %d, want 1", st.TrackedCampaigns)
	}
}

// TestSyncCampaignsTracksActiveCampaign is the end-to-end regression guard for
// the empty-Drops-page bug: an account with a live, in-progress campaign
// (mirroring "World of Tanks AMD Summer Arena Drops#2") must end up in the
// tracker's Campaigns() pool and be reflected in SyncStatus. It exercises the
// real syncCampaigns pipeline (dashboard listing -> per-campaign details fetch
// -> merge/filter -> publish), not just the pure buildTrackedCampaign helper,
// so a future change that breaks the live path - in drops.go or in how the
// details fetch is wired - is caught here instead of silently emptying the
// page in production.
func TestSyncCampaignsTracksActiveCampaign(t *testing.T) {
	summary := map[string]interface{}{
		"id":     "campaign-amd",
		"name":   "AMD Summer Arena Drops#2",
		"status": "ACTIVE",
		"game":   map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
	}
	detail := map[string]interface{}{
		"id":      "campaign-amd",
		"name":    "AMD Summer Arena Drops#2",
		"status":  "ACTIVE",
		"startAt": rfc3339(nowMinusHours(2)),
		"endAt":   rfc3339(nowPlusHours(48)),
		"game":    map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
		"timeBasedDrops": []interface{}{
			activeDrop("drop-1", "Garage Slot", 60),
		},
	}

	client := &fakeDropsClient{
		dashboard: dashboardResponse(summary),
		inventory: emptyInventoryResponse(),
		details:   map[string]map[string]interface{}{"campaign-amd": detail},
	}

	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tracker.syncCampaigns()

	got := tracker.Campaigns()
	if len(got) != 1 {
		t.Fatalf("expected 1 tracked campaign, got %d", len(got))
	}
	if got[0].Name != "AMD Summer Arena Drops#2" {
		t.Errorf("unexpected campaign name %q", got[0].Name)
	}
	if len(got[0].Drops) != 1 {
		t.Errorf("expected 1 tracked drop, got %d", len(got[0].Drops))
	}

	status := tracker.SyncStatus()
	if status.Runs != 1 {
		t.Errorf("expected 1 sync run, got %d", status.Runs)
	}
	if status.DashboardCampaigns != 1 {
		t.Errorf("expected dashboardCampaigns=1, got %d", status.DashboardCampaigns)
	}
	if status.TrackedCampaigns != 1 {
		t.Errorf("expected trackedCampaigns=1, got %d", status.TrackedCampaigns)
	}
	if status.LastError != "" {
		t.Errorf("expected no sync error, got %q", status.LastError)
	}
	if status.LastSyncAt.IsZero() {
		t.Error("expected lastSyncAt to be set")
	}
}

// TestSyncProgressAdvancesTrackedProgress verifies the lightweight,
// inventory-only progress sync updates the watched-minute counters of an
// already-tracked campaign without a full re-sync -- the fix for the Drops
// page lagging up to a full CampaignSyncInterval behind Twitch's real
// progress. A campaign is first tracked at 140/240 min, Twitch then advances
// it to 180/240, and syncProgress (a single Inventory read, no
// dashboard/details fetch) must surface the new value.
func TestSyncProgressAdvancesTrackedProgress(t *testing.T) {
	summary := map[string]interface{}{
		"id":     "campaign-amd",
		"name":   "AMD Summer Arena Drops#2",
		"status": "ACTIVE",
		"game":   map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
	}
	detail := map[string]interface{}{
		"id":      "campaign-amd",
		"name":    "AMD Summer Arena Drops#2",
		"status":  "ACTIVE",
		"startAt": rfc3339(nowMinusHours(2)),
		"endAt":   rfc3339(nowPlusHours(48)),
		"game":    map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
		"timeBasedDrops": []interface{}{
			activeDrop("drop-1", "Alienware Mystery Drop", 240),
		},
	}

	progressAt := func(watched float64) map[string]interface{} {
		return map[string]interface{}{
			"id":   "campaign-amd",
			"name": "AMD Summer Arena Drops#2",
			"game": map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
			"timeBasedDrops": []interface{}{
				inProgressDrop("drop-1", "Alienware Mystery Drop", 240, watched, false),
			},
		}
	}

	client := &fakeDropsClient{
		dashboard: dashboardResponse(summary),
		inventory: inventoryWithInProgress(progressAt(140)),
		details:   map[string]map[string]interface{}{"campaign-amd": detail},
	}

	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tracker.syncCampaigns()

	got := tracker.Campaigns()
	if len(got) != 1 || len(got[0].Drops) != 1 {
		t.Fatalf("expected 1 tracked campaign with 1 drop, got %+v", got)
	}
	if w := got[0].Drops[0].CurrentMinutesWatched; w != 140 {
		t.Fatalf("expected initial tracked progress 140, got %d", w)
	}

	// Twitch advances the drop; the lightweight progress sync must pick it up
	// without going through the dashboard/details discovery path again.
	client.inventory = inventoryWithInProgress(progressAt(180))
	tracker.syncProgress()

	got = tracker.Campaigns()
	if len(got) != 1 || len(got[0].Drops) != 1 {
		t.Fatalf("expected the campaign to remain tracked after progress sync, got %+v", got)
	}
	if w := got[0].Drops[0].CurrentMinutesWatched; w != 180 {
		t.Errorf("expected progress advanced to 180 after syncProgress, got %d", w)
	}
}

// TestSyncProgressNoTrackedCampaignsIsSafe guards that a progress sync run
// before the full sync has populated any campaigns is a harmless no-op (it must
// not panic or fabricate campaigns from the inventory -- discovery stays with
// the full sync).
func TestSyncProgressNoTrackedCampaignsIsSafe(t *testing.T) {
	client := &fakeDropsClient{
		dashboard: dashboardResponse(),
		inventory: inventoryWithInProgress(map[string]interface{}{
			"id":   "campaign-amd",
			"name": "AMD Summer Arena Drops#2",
			"game": map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
			"timeBasedDrops": []interface{}{
				inProgressDrop("drop-1", "Alienware Mystery Drop", 240, 180, false),
			},
		}),
		details: map[string]map[string]interface{}{},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)

	tracker.syncProgress()

	if got := len(tracker.Campaigns()); got != 0 {
		t.Fatalf("expected progress sync to add no campaigns, got %d", got)
	}
}

// TestSyncProgressRecordsObservations pins the Stage 3 observation contract the
// progress watchdog builds on: a completed inventory read counts as an
// observation even when nothing moved ("checked and unchanged" is exactly the
// stall evidence), an inventory failure is recorded instead of being swallowed
// silently, and the full sync never stamps the progress-observation fields.
func TestSyncProgressRecordsObservations(t *testing.T) {
	summary := map[string]interface{}{
		"id":     "campaign-amd",
		"name":   "AMD Summer Arena Drops#2",
		"status": "ACTIVE",
		"game":   map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
	}
	detail := map[string]interface{}{
		"id":      "campaign-amd",
		"name":    "AMD Summer Arena Drops#2",
		"status":  "ACTIVE",
		"startAt": rfc3339(nowMinusHours(2)),
		"endAt":   rfc3339(nowPlusHours(48)),
		"game":    map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
		"timeBasedDrops": []interface{}{
			activeDrop("drop-1", "Alienware Mystery Drop", 240),
		},
	}
	prog := map[string]interface{}{
		"id":   "campaign-amd",
		"name": "AMD Summer Arena Drops#2",
		"game": map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
		"timeBasedDrops": []interface{}{
			inProgressDrop("drop-1", "Alienware Mystery Drop", 240, 140, false),
		},
	}

	client := &fakeDropsClient{
		dashboard: dashboardResponse(summary),
		inventory: inventoryWithInProgress(prog),
		details:   map[string]map[string]interface{}{"campaign-amd": detail},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tracker.syncCampaigns()

	if s := tracker.SyncStatus(); s.ProgressRuns != 0 || !s.ProgressLastSyncAt.IsZero() {
		t.Fatalf("full sync must not stamp progress observations, got %+v", s)
	}

	// Unchanged progress: still a completed observation.
	tracker.syncProgress()
	s := tracker.SyncStatus()
	if s.ProgressRuns != 1 || s.ProgressLastSyncAt.IsZero() || s.ProgressLastError != "" {
		t.Fatalf("expected one clean observation after an unchanged progress sync, got %+v", s)
	}

	// Inventory outage: recorded, not swallowed.
	client.inventoryErr = fmt.Errorf("inventory 502")
	tracker.syncProgress()
	s = tracker.SyncStatus()
	if s.ProgressRuns != 2 || s.ProgressLastError == "" {
		t.Fatalf("expected the inventory failure to be recorded, got %+v", s)
	}

	// Recovery: the next successful read clears the error.
	client.inventoryErr = nil
	tracker.syncProgress()
	s = tracker.SyncStatus()
	if s.ProgressRuns != 3 || s.ProgressLastError != "" {
		t.Fatalf("expected the observation error to clear on recovery, got %+v", s)
	}
}

// TestSyncCampaignsDistinguishesEmptyFromFiltered verifies SyncStatus makes the
// two silent-failure modes distinguishable: Twitch returning no active
// campaigns at all vs returning campaigns that all get filtered out (here the
// details response carries no drops, so the campaign is skipped). Before this,
// both looked identical to an operator - an empty page and no INFO logs.
func TestSyncCampaignsDistinguishesEmptyFromFiltered(t *testing.T) {
	// Dashboard advertises a campaign, but its details have no drops, so it is
	// filtered out: dashboardCount stays 1 while tracked drops to 0.
	summary := map[string]interface{}{
		"id":     "campaign-empty",
		"name":   "Campaign Without Drops",
		"status": "ACTIVE",
		"game":   map[string]interface{}{"id": "game-x", "name": "Game X"},
	}
	detail := map[string]interface{}{
		"id":      "campaign-empty",
		"name":    "Campaign Without Drops",
		"status":  "ACTIVE",
		"startAt": rfc3339(nowMinusHours(2)),
		"endAt":   rfc3339(nowPlusHours(48)),
		"game":    map[string]interface{}{"id": "game-x", "name": "Game X"},
		// no timeBasedDrops
	}

	client := &fakeDropsClient{
		dashboard: dashboardResponse(summary),
		inventory: emptyInventoryResponse(),
		details:   map[string]map[string]interface{}{"campaign-empty": detail},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tracker.syncCampaigns()

	if got := len(tracker.Campaigns()); got != 0 {
		t.Fatalf("expected 0 tracked campaigns, got %d", got)
	}
	status := tracker.SyncStatus()
	if status.DashboardCampaigns != 1 {
		t.Errorf("expected dashboardCampaigns=1 (Twitch had a campaign), got %d", status.DashboardCampaigns)
	}
	if status.TrackedCampaigns != 0 {
		t.Errorf("expected trackedCampaigns=0 (all filtered), got %d", status.TrackedCampaigns)
	}

	// The genuinely-empty case: dashboard returns nothing.
	client2 := &fakeDropsClient{
		dashboard: dashboardResponse(),
		inventory: emptyInventoryResponse(),
		details:   map[string]map[string]interface{}{},
	}
	tracker2 := NewDropsTracker(client2, nil, config.RateLimitSettings{}, nil)
	tracker2.syncCampaigns()
	if status := tracker2.SyncStatus(); status.DashboardCampaigns != 0 {
		t.Errorf("expected dashboardCampaigns=0 for an account with no campaigns, got %d", status.DashboardCampaigns)
	}
}
