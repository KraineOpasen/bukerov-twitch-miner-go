package drops

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/twitch"
)

func nowMinusHours(h int) time.Time { return time.Now().Add(-time.Duration(h) * time.Hour) }
func nowPlusHours(h int) time.Time  { return time.Now().Add(time.Duration(h) * time.Hour) }

type blockingSuppressionHandler struct {
	entered     chan struct{}
	release     chan struct{}
	blockOnce   sync.Once
	releaseOnce sync.Once
}

func (h *blockingSuppressionHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *blockingSuppressionHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "Drop suppressed by ghost-skip ledger" {
		h.blockOnce.Do(func() {
			close(h.entered)
			<-h.release
		})
	}
	return nil
}

func (h *blockingSuppressionHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *blockingSuppressionHandler) WithGroup(string) slog.Handler      { return h }

func (h *blockingSuppressionHandler) unblock() {
	h.releaseOnce.Do(func() { close(h.release) })
}

// fakeDropsClient is a scripted twitchClient for exercising the whole
// syncCampaigns pipeline without a live Twitch connection. PostGQL is
// dispatched by operation name so ViewerDropsDashboard and Inventory can be
// answered independently, and GetDropCampaignDetails is keyed by campaign ID.
type fakeDropsClient struct {
	dashboard    map[string]interface{}
	dashboardErr error
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
	// background loop's cadence. Set before Start. dashboard/details/inventory
	// may be swapped after construction (e.g. publishCampaignB), but only
	// while no concurrent caller can re-enter PostGQL: either parked in the
	// inventoryGate, or at full quiescence (after wg.Wait for every goroutine
	// of one phase, before the next phase's goroutines start) -- swapping
	// them at any other time is a data race.
	fullSyncSignal chan struct{}

	// gateMu guards inventoryGate: armInventoryGate (the test goroutine) and
	// PostGQL's "Inventory" case (whichever goroutine reaches it) touch it
	// concurrently once a light sync and a full sync race, so a plain field
	// would be a data race under -race.
	gateMu        sync.Mutex
	inventoryGate *inventoryGate
}

// cadenceActivationClient changes only its dashboard's current-state response,
// under a mutex, so the background full-sync loop can prove it discovers a
// campaign after Twitch begins reporting it ACTIVE without any auxiliary wake.
type cadenceActivationClient struct {
	mu     sync.RWMutex
	active bool
	now    time.Time
}

func (c *cadenceActivationClient) setActive() {
	c.mu.Lock()
	c.active = true
	c.mu.Unlock()
}

func (c *cadenceActivationClient) PostGQL(op constants.GQLOperation) (map[string]interface{}, error) {
	switch op.OperationName {
	case "ViewerDropsDashboard":
		c.mu.RLock()
		active := c.active
		c.mu.RUnlock()
		status := "UPCOMING"
		startAt := c.now.Add(time.Hour)
		if active {
			status = "ACTIVE"
			startAt = c.now.Add(-time.Hour)
		}
		return dashboardResponse(map[string]interface{}{
			"id": "cadence-active", "name": "Cadence Active", "status": status,
			"startAt": rfc3339(startAt), "endAt": rfc3339(c.now.Add(2 * time.Hour)),
			"game": map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
		}), nil
	case "Inventory":
		return emptyInventoryResponse(), nil
	default:
		return map[string]interface{}{}, nil
	}
}

func (c *cadenceActivationClient) GetDropCampaignDetails(string) (map[string]interface{}, error) {
	return map[string]interface{}{
		"id": "cadence-active", "name": "Cadence Active", "status": "ACTIVE",
		"startAt": rfc3339(c.now.Add(-time.Hour)), "endAt": rfc3339(c.now.Add(2 * time.Hour)),
		"game":           map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
		"timeBasedDrops": []interface{}{activeDrop("cadence-drop", "Cadence Reward", 60)},
	}, nil
}

func (*cadenceActivationClient) ClaimDrop(*models.Drop) (twitch.ClaimStatus, error) {
	return twitch.ClaimStatusRejected, nil
}

// inventoryGate lets a test deterministically hold ONE in-flight Inventory
// PostGQL call open while a second, concurrent caller (a full sync) runs to
// completion unblocked -- the exact interleaving the F1 staleness tests need
// to prove a stale light-sync response is discarded rather than published
// over a newer campaign pool.
type inventoryGate struct {
	entered chan struct{} // closed once the gated call is reached
	release chan struct{} // the test closes this to let the gated call return
	resp    map[string]interface{}
	err     error
}

// armInventoryGate arms the fake to intercept exactly the NEXT "Inventory"
// PostGQL call: that call closes the returned gate's entered channel, blocks
// until the test closes release, then returns resp/err instead of the normal
// f.inventory/f.inventoryErr. It is single-shot -- every Inventory call after
// the gated one (including further calls within the same sync that triggered
// it) proceeds normally. Must be called before starting the goroutine whose
// Inventory call is to be gated.
func (f *fakeDropsClient) armInventoryGate(resp map[string]interface{}, err error) *inventoryGate {
	g := &inventoryGate{entered: make(chan struct{}), release: make(chan struct{}), resp: resp, err: err}
	f.gateMu.Lock()
	f.inventoryGate = g
	f.gateMu.Unlock()
	return g
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
		if f.dashboardErr != nil {
			return nil, f.dashboardErr
		}
		return f.dashboard, nil
	case "Inventory":
		f.gateMu.Lock()
		g := f.inventoryGate
		f.inventoryGate = nil // single-shot: only the first call after arming is gated
		f.gateMu.Unlock()
		if g != nil {
			close(g.entered)
			<-g.release
			return g.resp, g.err
		}
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

func (f *fakeDropsClient) ClaimDrop(*models.Drop) (twitch.ClaimStatus, error) {
	return twitch.ClaimStatusRejected, nil
}

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

type testPreconditionState struct {
	present bool
	value   bool
}

func testPreconditionPtr(state testPreconditionState) *bool {
	if !state.present {
		return nil
	}
	value := state.value
	return &value
}

func inProgressDropWithPrecondition(
	id, name string,
	required, watched float64,
	claimed bool,
	state testPreconditionState,
) map[string]interface{} {
	drop := inProgressDrop(id, name, required, watched, claimed)
	if state.present {
		drop["self"].(map[string]interface{})["hasPreconditionsMet"] = state.value
	}
	return drop
}

func assertPreconditionState(t *testing.T, got *bool, want testPreconditionState) {
	t.Helper()
	if !want.present {
		if got != nil {
			t.Fatalf("HasPreconditionsMet=%v, want nil", *got)
		}
		return
	}
	if got == nil {
		t.Fatalf("HasPreconditionsMet=nil, want %v", want.value)
	}
	if *got != want.value {
		t.Fatalf("HasPreconditionsMet=%v, want %v", *got, want.value)
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

func TestProgressDiffersSemanticSnapshotMatrix(t *testing.T) {
	states := []struct {
		name  string
		state testPreconditionState
	}{
		{name: "nil", state: testPreconditionState{}},
		{name: "false", state: testPreconditionState{present: true, value: false}},
		{name: "true", state: testPreconditionState{present: true, value: true}},
	}

	campaign := func(drops ...*models.Drop) *models.Campaign {
		return &models.Campaign{Drops: drops}
	}
	drop := func(id string, minutes int, state testPreconditionState) *models.Drop {
		return &models.Drop{
			ID:                    id,
			CurrentMinutesWatched: minutes,
			HasPreconditionsMet:   testPreconditionPtr(state),
		}
	}

	for _, before := range states {
		for _, after := range states {
			name := before.name + "_to_" + after.name
			t.Run(name, func(t *testing.T) {
				wantChanged := before.state.present != after.state.present ||
					(before.state.present && before.state.value != after.state.value)
				got := progressDiffers(
					campaign(drop("drop-a", 45, before.state)),
					campaign(drop("drop-a", 45, after.state)),
				)
				if got != wantChanged {
					t.Fatalf("progressDiffers=%v, want %v", got, wantChanged)
				}
			})
		}
	}

	controls := []struct {
		name          string
		before, after *models.Campaign
		wantChanged   bool
	}{
		{
			name:        "minutes changed",
			before:      campaign(drop("drop-a", 45, testPreconditionState{})),
			after:       campaign(drop("drop-a", 46, testPreconditionState{})),
			wantChanged: true,
		},
		{
			name:        "ID changed with same count and minutes",
			before:      campaign(drop("drop-a", 45, testPreconditionState{})),
			after:       campaign(drop("drop-b", 45, testPreconditionState{})),
			wantChanged: true,
		},
		{
			name:        "drop added",
			before:      campaign(drop("drop-a", 45, testPreconditionState{})),
			after:       campaign(drop("drop-a", 45, testPreconditionState{}), drop("drop-b", 45, testPreconditionState{})),
			wantChanged: true,
		},
		{
			name:        "drop removed",
			before:      campaign(drop("drop-a", 45, testPreconditionState{}), drop("drop-b", 45, testPreconditionState{})),
			after:       campaign(drop("drop-a", 45, testPreconditionState{})),
			wantChanged: true,
		},
		{
			name: "order changed only",
			before: campaign(
				drop("drop-a", 45, testPreconditionState{present: true, value: false}),
				drop("drop-b", 30, testPreconditionState{present: true, value: true}),
			),
			after: campaign(
				drop("drop-b", 30, testPreconditionState{present: true, value: true}),
				drop("drop-a", 45, testPreconditionState{present: true, value: false}),
			),
			wantChanged: false,
		},
	}

	for _, tc := range controls {
		t.Run(tc.name, func(t *testing.T) {
			if got := progressDiffers(tc.before, tc.after); got != tc.wantChanged {
				t.Fatalf("progressDiffers=%v, want %v", got, tc.wantChanged)
			}
		})
	}
}

func TestSyncProgressPublishesPreconditionStateTransitions(t *testing.T) {
	nilState := testPreconditionState{}
	falseState := testPreconditionState{present: true, value: false}
	trueState := testPreconditionState{present: true, value: true}

	cases := []struct {
		name        string
		initial     testPreconditionState
		observed    testPreconditionState
		want        testPreconditionState
		wantPublish bool
	}{
		{name: "nil to false", initial: nilState, observed: falseState, want: falseState, wantPublish: true},
		{name: "false to true", initial: falseState, observed: trueState, want: trueState, wantPublish: true},
		{name: "true to false", initial: trueState, observed: falseState, want: falseState, wantPublish: true},
		{name: "nil to nil", initial: nilState, observed: nilState, want: nilState, wantPublish: false},
		{name: "false to false", initial: falseState, observed: falseState, want: falseState, wantPublish: false},
		{name: "true to true", initial: trueState, observed: trueState, want: trueState, wantPublish: false},
		{name: "false then absent", initial: falseState, observed: nilState, want: falseState, wantPublish: false},
		{name: "true then absent", initial: trueState, observed: nilState, want: trueState, wantPublish: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			summary, detail, _, _ := campaignAAndB()
			inventoryDrop := func(state testPreconditionState) map[string]interface{} {
				return inProgressDropWithPrecondition("drop-a", "Reward A", 120, 45, false, state)
			}
			inventory := func(state testPreconditionState) map[string]interface{} {
				return inventoryWithInProgress(map[string]interface{}{
					"id":             "campaign-a",
					"name":           "Campaign A",
					"game":           map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
					"timeBasedDrops": []interface{}{inventoryDrop(state)},
				})
			}

			client := &fakeDropsClient{
				dashboard: dashboardResponse(summary),
				inventory: inventory(tc.initial),
				details:   map[string]map[string]interface{}{"campaign-a": detail},
			}
			streamer := models.NewStreamer("streamer1", models.StreamerSettings{ClaimDrops: true})
			streamer.SetConfirmedOnline()
			streamer.Stream.SetCampaignIDs([]string{"campaign-a"})

			tracker := NewDropsTracker(client, []*models.Streamer{streamer}, config.RateLimitSettings{}, nil)
			tracker.syncCampaigns()

			publishedBefore := tracker.Campaigns()
			if len(publishedBefore) != 1 || len(publishedBefore[0].Drops) != 1 {
				t.Fatalf("fixture setup: expected one campaign/drop, got %+v", publishedBefore)
			}
			beforeCampaign := publishedBefore[0]
			beforeDrop := beforeCampaign.Drops[0]
			assertPreconditionState(t, beforeDrop.HasPreconditionsMet, tc.initial)
			assertAssigned(t, streamer, "campaign-a")
			assignedBefore := streamer.Stream.GetCampaigns()[0]
			if assignedBefore != beforeCampaign {
				t.Fatal("fixture setup: streamer must reference the shared published campaign")
			}

			freshDrop := inventoryDrop(tc.observed)
			freshSelf := freshDrop["self"].(map[string]interface{})
			freshValue, freshPresent := freshSelf["hasPreconditionsMet"]
			if freshPresent != tc.observed.present {
				t.Fatalf("fresh Inventory field presence=%v, want %v", freshPresent, tc.observed.present)
			}
			if tc.observed.present {
				value, ok := freshValue.(bool)
				if !ok || value != tc.observed.value {
					t.Fatalf("fresh Inventory value=%T(%v), want bool(%v)", freshValue, freshValue, tc.observed.value)
				}
			}

			clone := beforeCampaign.Clone()
			clone.SyncDrops([]interface{}{freshDrop}, nil)
			clone.ClearClaimedDrops()
			assertPreconditionState(t, clone.Drops[0].HasPreconditionsMet, tc.want)
			if differs := progressDiffers(beforeCampaign, clone); differs != tc.wantPublish {
				t.Errorf("progressDiffers=%v, want %v for semantic transition", differs, tc.wantPublish)
			}

			statusBefore := tracker.SyncStatus()
			client.inventory = inventory(tc.observed)
			tracker.syncProgress()

			statusAfter := tracker.SyncStatus()
			publishedAfter := tracker.Campaigns()
			if len(publishedAfter) != 1 || len(publishedAfter[0].Drops) != 1 {
				t.Fatalf("expected one published campaign/drop after light sync, got %+v", publishedAfter)
			}
			afterCampaign := publishedAfter[0]
			if statusAfter.ProgressRuns != statusBefore.ProgressRuns+1 {
				t.Errorf("ProgressRuns=%d, want %d", statusAfter.ProgressRuns, statusBefore.ProgressRuns+1)
			}

			assignedAfter := streamer.Stream.GetCampaigns()
			if len(assignedAfter) != 1 || assignedAfter[0].ID != "campaign-a" {
				t.Fatalf("streamer assignment lost after light sync: %+v", assignedAfter)
			}
			if tc.wantPublish {
				if statusAfter.Revision != statusBefore.Revision+1 {
					t.Errorf("Revision=%d, want exactly %d", statusAfter.Revision, statusBefore.Revision+1)
				}
				if statusAfter.UpdateSource != updateSourceLightSync {
					t.Errorf("UpdateSource=%q, want %q", statusAfter.UpdateSource, updateSourceLightSync)
				}
				if afterCampaign == beforeCampaign {
					t.Error("changed semantic state must publish a new immutable campaign snapshot")
				}
				if assignedAfter[0] != afterCampaign || assignedAfter[0] == assignedBefore {
					t.Error("streamer must be re-pointed to the newly published campaign snapshot")
				}
			} else {
				if statusAfter.Revision != statusBefore.Revision {
					t.Errorf("unchanged semantic state must not bump Revision: got %d, want %d", statusAfter.Revision, statusBefore.Revision)
				}
				if statusAfter.UpdateSource != statusBefore.UpdateSource {
					t.Errorf("unchanged semantic state changed UpdateSource from %q to %q", statusBefore.UpdateSource, statusAfter.UpdateSource)
				}
				if afterCampaign != beforeCampaign || assignedAfter[0] != assignedBefore {
					t.Error("unchanged semantic state must not churn published or assigned campaign pointers")
				}
			}
			assertPreconditionState(t, afterCampaign.Drops[0].HasPreconditionsMet, tc.want)
			assertPreconditionState(t, beforeDrop.HasPreconditionsMet, tc.initial)

			stableRevision := tracker.Revision()
			stableCampaign := tracker.Campaigns()[0]
			tracker.syncProgress()
			if got := tracker.Revision(); got != stableRevision {
				t.Errorf("repeated identical Inventory observation churned Revision: got %d, want %d", got, stableRevision)
			}
			if got := tracker.Campaigns()[0]; got != stableCampaign {
				t.Error("repeated identical Inventory observation churned the shared campaign pointer")
			}
		})
	}
}

// TestFullSyncDropsRecoveredCampaignGoneFromInventory is the §6 preflight guard
// for the other edge: keeping date-less inventory drops must NOT make a recovered
// campaign linger forever. Because each full sync REBUILDS the tracked pool from
// scratch and replaces it (never merges), a campaign that disappears from both
// the dashboard and the inventory is simply not re-added on the next full sync.
func TestFullSyncDropsRecoveredCampaignGoneFromInventory(t *testing.T) {
	prog := map[string]interface{}{
		"id":             "campaign-3moe",
		"name":           "3 MoE Challenge",
		"game":           map[string]interface{}{"id": "game-wot", "displayName": "World of Tanks"},
		"timeBasedDrops": []interface{}{inProgressDrop("drop-1", "3 MoE Reward", 120, 45, false)}, // no dates
	}
	client := &fakeDropsClient{
		dashboard: dashboardResponse(),
		inventory: inventoryWithInProgress(prog),
		details:   map[string]map[string]interface{}{},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tracker.syncCampaigns()
	if len(tracker.Campaigns()) != 1 {
		t.Fatalf("first sync should recover the date-less campaign, got %d", len(tracker.Campaigns()))
	}

	// Twitch stops advertising it (gone from dashboard AND inventory).
	client.inventory = emptyInventoryResponse()
	tracker.syncCampaigns()
	if got := tracker.Campaigns(); len(got) != 0 {
		t.Fatalf("a date-less campaign gone from Twitch must not linger; got %d tracked", len(got))
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

func TestOrdinaryCampaignCadenceDiscoversNewlyActiveCampaign(t *testing.T) {
	client := &cadenceActivationClient{now: time.Now()}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{
		CampaignSyncInterval:     40,
		DropProgressSyncInterval: 100_000,
	}, nil)
	tracker.intervalUnit = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	tracker.Start(ctx)
	t.Cleanup(func() {
		cancel()
		tracker.Stop()
	})

	deadline := time.Now().Add(2 * time.Second)
	for tracker.SyncStatus().Runs < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if runs := tracker.SyncStatus().Runs; runs < 1 {
		t.Fatalf("initial full sync did not complete: runs=%d", runs)
	}
	if got := tracker.Campaigns(); len(got) != 0 {
		t.Fatalf("non-active dashboard response entered the Current set: %+v", got)
	}

	client.setActive()
	for time.Now().Before(deadline) {
		got := tracker.Campaigns()
		if len(got) == 1 && got[0].ID == "cadence-active" && tracker.SyncStatus().Runs >= 2 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("ordinary CampaignSyncInterval did not discover newly active campaign: status=%+v campaigns=%+v",
		tracker.SyncStatus(), tracker.Campaigns())
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

// ---------------------------------------------------------------------------
// F1: a lightweight progress sync captured against campaign revision R must
// never publish (or record a successful observation) over a pool a full sync
// has already replaced with revision R+n. syncProgress performs its Inventory
// read without holding d.mu, so the interleaving below is a real race the
// production code must resolve, not just a sequencing convenience.
// ---------------------------------------------------------------------------

// campaignAAndB returns matching dashboard summary + campaign detail for two
// distinct, equally-eligible campaigns sharing one game, used to prove a
// stale light-sync response for campaign A is discarded rather than merged
// over a newer pool that has moved on to campaign B.
func campaignAAndB() (summaryA, detailA, summaryB, detailB map[string]interface{}) {
	game := map[string]interface{}{"id": "game-wot", "name": "World of Tanks"}
	summaryA = map[string]interface{}{"id": "campaign-a", "name": "Campaign A", "status": "ACTIVE", "game": game}
	detailA = map[string]interface{}{
		"id": "campaign-a", "name": "Campaign A", "status": "ACTIVE",
		"startAt": rfc3339(nowMinusHours(2)), "endAt": rfc3339(nowPlusHours(48)), "game": game,
		"timeBasedDrops": []interface{}{activeDrop("drop-a", "Reward A", 120)},
	}
	summaryB = map[string]interface{}{"id": "campaign-b", "name": "Campaign B", "status": "ACTIVE", "game": game}
	detailB = map[string]interface{}{
		"id": "campaign-b", "name": "Campaign B", "status": "ACTIVE",
		"startAt": rfc3339(nowMinusHours(2)), "endAt": rfc3339(nowPlusHours(48)), "game": game,
		"timeBasedDrops": []interface{}{activeDrop("drop-b", "Reward B", 120)},
	}
	return
}

// staleProgressFixture is a tracker with one online, drops-enabled streamer
// and campaign A already published (via a real syncCampaigns) and assigned to
// the streamer, ready for a test to interleave a light sync's in-flight
// Inventory read with a concurrent full sync that publishes campaign B.
type staleProgressFixture struct {
	tracker  *DropsTracker
	client   *fakeDropsClient
	streamer *models.Streamer
}

func newStaleProgressFixture(t *testing.T) *staleProgressFixture {
	t.Helper()
	summaryA, detailA, _, _ := campaignAAndB()

	client := &fakeDropsClient{
		dashboard: dashboardResponse(summaryA),
		inventory: inventoryWithInProgress(map[string]interface{}{
			"id":             "campaign-a",
			"name":           "Campaign A",
			"game":           map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
			"timeBasedDrops": []interface{}{inProgressDrop("drop-a", "Reward A", 120, 45, false)},
		}),
		details: map[string]map[string]interface{}{"campaign-a": detailA},
	}

	s := models.NewStreamer("streamer1", models.StreamerSettings{ClaimDrops: true})
	s.SetConfirmedOnline()
	s.Stream.SetCampaignIDs([]string{"campaign-a"}) // resolved Known availability -> AvailabilityYes

	tracker := NewDropsTracker(client, []*models.Streamer{s}, config.RateLimitSettings{}, nil)
	tracker.syncCampaigns()

	got := tracker.Campaigns()
	if len(got) != 1 || got[0].ID != "campaign-a" || got[0].Drops[0].CurrentMinutesWatched != 45 {
		t.Fatalf("fixture setup: expected campaign-a tracked at 45 min, got %+v", got)
	}
	assertAssigned(t, s, "campaign-a")

	return &staleProgressFixture{tracker: tracker, client: client, streamer: s}
}

// publishCampaignB runs a full sync that replaces the tracked pool with
// campaign B and re-points the streamer's advertised availability at it --
// the "newer full sync" a concurrent stale light sync must never be
// published over.
func (f *staleProgressFixture) publishCampaignB(t *testing.T) {
	t.Helper()
	_, _, summaryB, detailB := campaignAAndB()
	f.client.dashboard = dashboardResponse(summaryB)
	f.client.details = map[string]map[string]interface{}{"campaign-b": detailB}
	f.client.inventory = emptyInventoryResponse()
	f.streamer.Stream.SetCampaignIDs([]string{"campaign-b"})

	f.tracker.syncCampaigns()

	got := f.tracker.Campaigns()
	if len(got) != 1 || got[0].ID != "campaign-b" {
		t.Fatalf("fixture setup: expected campaign-b published, got %+v", got)
	}
}

// TestSyncProgressStaleChangedResultDiscarded is F1-T1: a light sync captures
// campaign-a at revision R, blocks in flight on its Inventory read, and a full
// sync publishes campaign-b at R+1 while it waits. Releasing the light sync's
// response -- reporting CHANGED progress for the now-superseded campaign-a --
// must be discarded entirely: campaign-b must remain the published pool,
// untouched by the stale result, and the streamer must stay assigned to it.
func TestSyncProgressStaleChangedResultDiscarded(t *testing.T) {
	f := newStaleProgressFixture(t)

	gate := f.client.armInventoryGate(inventoryWithInProgress(map[string]interface{}{
		"id":             "campaign-a",
		"name":           "Campaign A",
		"game":           map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
		"timeBasedDrops": []interface{}{inProgressDrop("drop-a", "Reward A", 120, 90, false)}, // changed: 45 -> 90
	}), nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		f.tracker.syncProgress()
	}()

	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("syncProgress did not reach the gated Inventory call")
	}

	// The newer full sync lands while the light sync's Inventory read is in
	// flight: publishes campaign-b at a bumped revision and re-points the
	// streamer, exactly as a real interleaved full sync would.
	f.publishCampaignB(t)
	revAfterFull := f.tracker.Revision()
	statusAfterFull := f.tracker.SyncStatus()

	close(gate.release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("syncProgress did not return after the gated Inventory call was released")
	}

	got := f.tracker.Campaigns()
	if len(got) != 1 || got[0].ID != "campaign-b" {
		t.Fatalf("stale light-sync result must not resurrect campaign-a or lose campaign-b, got %v", keptIDs(got))
	}
	if r := f.tracker.Revision(); r != revAfterFull {
		t.Errorf("stale light-sync result must not bump the revision, got %d, want %d (unchanged since the full sync)", r, revAfterFull)
	}
	status := f.tracker.SyncStatus()
	if status.UpdateSource != "full_sync" {
		t.Errorf("stale light-sync result must not overwrite UpdateSource, got %q, want %q", status.UpdateSource, "full_sync")
	}
	if !status.BackendUpdatedAt.Equal(statusAfterFull.BackendUpdatedAt) {
		t.Errorf("stale light-sync result must not stamp BackendUpdatedAt, got %v, want %v", status.BackendUpdatedAt, statusAfterFull.BackendUpdatedAt)
	}
	if status.ProgressRuns != 0 || !status.ProgressLastSyncAt.IsZero() {
		t.Errorf("stale light-sync result must not record a progress observation, got runs=%d lastSyncAt=%v", status.ProgressRuns, status.ProgressLastSyncAt)
	}
	assertAssigned(t, f.streamer, "campaign-b")
}

// TestSyncProgressStaleUnchangedOrEmptyResultDiscarded is F1-T2: the same
// stale interleaving as F1-T1, but the light sync's released response reports
// either unchanged progress or a valid empty/no-in-progress observation for
// the superseded pool. Both must be discarded exactly like a changed result:
// the progress-observation bookkeeping (ProgressLastSyncAt/ProgressLastError)
// must not be stamped against the newer pool, and the streamer assignment
// must not be touched.
func TestSyncProgressStaleUnchangedOrEmptyResultDiscarded(t *testing.T) {
	cases := []struct {
		name  string
		build func() map[string]interface{}
	}{
		{
			name: "unchanged",
			build: func() map[string]interface{} {
				return inventoryWithInProgress(map[string]interface{}{
					"id":             "campaign-a",
					"name":           "Campaign A",
					"game":           map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
					"timeBasedDrops": []interface{}{inProgressDrop("drop-a", "Reward A", 120, 45, false)}, // unchanged: still 45
				})
			},
		},
		{
			name:  "empty",
			build: func() map[string]interface{} { return emptyInventoryResponse() },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newStaleProgressFixture(t)

			gate := f.client.armInventoryGate(tc.build(), nil)

			done := make(chan struct{})
			go func() {
				defer close(done)
				f.tracker.syncProgress()
			}()

			select {
			case <-gate.entered:
			case <-time.After(2 * time.Second):
				t.Fatal("syncProgress did not reach the gated Inventory call")
			}

			f.publishCampaignB(t)
			revAfterFull := f.tracker.Revision()

			close(gate.release)
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("syncProgress did not return after the gated Inventory call was released")
			}

			status := f.tracker.SyncStatus()
			if status.ProgressRuns != 0 || !status.ProgressLastSyncAt.IsZero() {
				t.Errorf("a stale %s observation must not advance ProgressLastSyncAt/ProgressRuns for the newer pool, got runs=%d lastSyncAt=%v",
					tc.name, status.ProgressRuns, status.ProgressLastSyncAt)
			}
			if status.ProgressLastError != "" {
				t.Errorf("a stale %s observation must not set/clear ProgressLastError, got %q", tc.name, status.ProgressLastError)
			}
			if r := f.tracker.Revision(); r != revAfterFull {
				t.Errorf("stale %s observation must not bump the revision, got %d, want %d", tc.name, r, revAfterFull)
			}
			assertAssigned(t, f.streamer, "campaign-b")
		})
	}
}

func TestSyncProgressPublishedLightRepointCannotOverwriteNewerFullRepoint(t *testing.T) {
	f := newStaleProgressFixture(t)

	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	if err := ledger.Observe(context.Background(), skipEvidence{
		class: evidenceClaimAccepted, gameID: "game-wot", campaignID: "campaign-a", dropID: "drop-a",
	}); err != nil {
		t.Fatalf("seed skip ledger: %v", err)
	}
	f.tracker.skipLedger = ledger

	// Make the light observation a precondition-only nil -> false transition at
	// unchanged IDs/count/minutes, so this exercises the exact concern.
	f.client.inventory = inventoryWithInProgress(map[string]interface{}{
		"id":             "campaign-a",
		"name":           "Campaign A",
		"game":           map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
		"timeBasedDrops": []interface{}{inProgressDropWithPrecondition("drop-a", "Reward A", 120, 45, false, testPreconditionState{present: true, value: false})},
	})

	h := &blockingSuppressionHandler{entered: make(chan struct{}), release: make(chan struct{})}
	defer h.unblock()
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(previousLogger)

	lightDone := make(chan struct{})
	go func() {
		defer close(lightDone)
		f.tracker.syncProgress()
	}()

	select {
	case <-h.entered:
		// The light sync already published A at R+1 and updateStreamerCampaigns
		// captured that old pool; it is now deterministically paused before logMu.
	case <-time.After(2 * time.Second):
		t.Fatal("light repoint did not reach the post-snapshot gate")
	}

	lightRevision := f.tracker.Revision()
	lightObservation := f.tracker.ProgressObservation("campaign-a", "drop-a")
	if lightObservation.Error != "" || !lightObservation.Found || lightObservation.Revision != lightRevision {
		t.Fatalf("light exact observation was not published at its final revision: %+v (revision=%d)", lightObservation, lightRevision)
	}
	preRepointBroker := f.tracker.BrokerCampaignSnapshot()
	if preRepointBroker.CurrentRevision != lightRevision ||
		preRepointBroker.SourceRevision == preRepointBroker.CurrentRevision {
		t.Fatalf("post-light/pre-repoint window was not represented by a stale broker source fence: %+v", preRepointBroker)
	}
	got := f.tracker.Campaigns()
	if len(got) != 1 || got[0].ID != "campaign-a" || got[0].Drops[0].CurrentMinutesWatched != 45 ||
		got[0].Drops[0].HasPreconditionsMet == nil || *got[0].Drops[0].HasPreconditionsMet {
		t.Fatalf("light publication did not land before repoint gate: %+v", got)
	}

	// A full sync now publishes B at R+2 and completes its own repoint first.
	f.publishCampaignB(t)
	if f.tracker.Revision() != lightRevision+1 {
		t.Fatalf("full publication revision=%d, want %d", f.tracker.Revision(), lightRevision+1)
	}
	assertAssigned(t, f.streamer, "campaign-b")
	postFullBroker := f.tracker.BrokerCampaignSnapshot()
	if postFullBroker.SourceRevision != postFullBroker.CurrentRevision ||
		postFullBroker.CurrentRevision != f.tracker.Revision() {
		t.Fatalf("newer full re-point did not close the broker source fence: %+v", postFullBroker)
	}

	// Let the older light repoint continue after the newer full repoint.
	h.unblock()
	select {
	case <-lightDone:
	case <-time.After(2 * time.Second):
		t.Fatal("light repoint did not return after release")
	}

	// The published pool remains B, and the assignment must match it. Without
	// a revision fence, the old A pass clears the fresh B assignment here.
	got = f.tracker.Campaigns()
	if len(got) != 1 || got[0].ID != "campaign-b" {
		t.Fatalf("published pool changed unexpectedly: %v", keptIDs(got))
	}
	assertAssigned(t, f.streamer, "campaign-b")
}

// TestSyncProgressChangedObservationPublishesOnceAfterFreshRevision is F1-T3:
// with no concurrent revision movement, a changed light-sync observation
// still publishes normally -- the revision increments exactly once,
// UpdateSource is light_sync, and the observation timestamp is recorded no
// earlier than the refreshed campaign data it describes (both are set
// together under the same lock in publishProgressObservation, so a reader can
// never observe one without the other).
func TestSyncProgressChangedObservationPublishesOnceAfterFreshRevision(t *testing.T) {
	invAt := func(minutes float64) map[string]interface{} {
		return inventoryWithInProgress(map[string]interface{}{
			"id":             "campaign-a",
			"name":           "Campaign A",
			"game":           map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
			"timeBasedDrops": []interface{}{inProgressDrop("drop-a", "Reward A", 120, minutes, false)},
		})
	}
	summaryA, detailA, _, _ := campaignAAndB()
	client := &fakeDropsClient{
		dashboard: dashboardResponse(summaryA),
		inventory: invAt(45),
		details:   map[string]map[string]interface{}{"campaign-a": detailA},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tracker.syncCampaigns()
	revBefore := tracker.Revision()

	before := time.Now()
	client.inventory = invAt(90)
	tracker.syncProgress()

	status := tracker.SyncStatus()
	if status.Revision != revBefore+1 {
		t.Fatalf("revision should increment exactly once, got %d, want %d", status.Revision, revBefore+1)
	}
	if status.UpdateSource != "light_sync" {
		t.Fatalf("UpdateSource should be light_sync, got %q", status.UpdateSource)
	}
	if status.ProgressLastSyncAt.Before(before) {
		t.Errorf("ProgressLastSyncAt should be recorded at/after the sync started, got %v, want >= %v", status.ProgressLastSyncAt, before)
	}
	got := tracker.Campaigns()
	if len(got) != 1 || got[0].Drops[0].CurrentMinutesWatched != 90 {
		t.Fatalf("expected refreshed progress 90, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// F2: when the Inventory acquisition syncWithInventory depends on fails,
// returns nil, or is structurally unusable, the full sync must abort and keep
// the last-known-good published pool rather than publish freshly-built
// campaign/drop objects that never received live progress. A successfully
// decoded Inventory response that simply reports no in-progress campaigns
// remains a legitimate observation, not a failure.
// ---------------------------------------------------------------------------

// f2Fixture builds a dashboard summary/detail pair and an Inventory response
// generator for one campaign, shared by the F2 acquisition-failure tests.
func f2Fixture() (summary, detail map[string]interface{}, progAt func(watched float64) map[string]interface{}) {
	summary = map[string]interface{}{
		"id": "campaign-amd", "name": "AMD Summer Arena Drops#2", "status": "ACTIVE",
		"game": map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
	}
	detail = map[string]interface{}{
		"id": "campaign-amd", "name": "AMD Summer Arena Drops#2", "status": "ACTIVE",
		"startAt": rfc3339(nowMinusHours(2)), "endAt": rfc3339(nowPlusHours(48)),
		"game":           map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
		"timeBasedDrops": []interface{}{activeDrop("drop-1", "Alienware Mystery Drop", 240)},
	}
	progAt = func(watched float64) map[string]interface{} {
		return inventoryWithInProgress(map[string]interface{}{
			"id":             "campaign-amd",
			"name":           "AMD Summer Arena Drops#2",
			"game":           map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
			"timeBasedDrops": []interface{}{inProgressDrop("drop-1", "Alienware Mystery Drop", 240, watched, false)},
		})
	}
	return
}

// TestSyncCampaignsInventoryAcquisitionFailurePreservesLastKnownGood covers
// F2-T1 (a request-error Inventory acquisition aborts the full sync and keeps
// the previous pool) and F2-T4 (a later successful sync recovers normally).
// syncWithInventory's own Inventory acquisition is the sole source of live
// per-drop progress (Drop.Update(selfData)); when it fails, the campaigns
// built from GetDropCampaignDetails have never received it, so publishing
// them would silently replace known progress with zero/unknown data.
func TestSyncCampaignsInventoryAcquisitionFailurePreservesLastKnownGood(t *testing.T) {
	summary, detail, progAt := f2Fixture()
	client := &fakeDropsClient{
		dashboard: dashboardResponse(summary),
		inventory: progAt(140),
		details:   map[string]map[string]interface{}{"campaign-amd": detail},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tracker.syncCampaigns()

	got := tracker.Campaigns()
	if len(got) != 1 || got[0].Drops[0].CurrentMinutesWatched != 140 {
		t.Fatalf("expected the last-known-good pool at 140 min, got %+v", got)
	}
	revBefore := tracker.Revision()

	// The Inventory acquisition syncWithInventory depends on now fails.
	client.inventoryErr = fmt.Errorf("inventory 502")
	tracker.syncCampaigns()

	got = tracker.Campaigns()
	if len(got) != 1 || got[0].ID != "campaign-amd" || got[0].Drops[0].CurrentMinutesWatched != 140 {
		t.Fatalf("an inventory acquisition failure must preserve the last-known-good pool/progress, got %+v", got)
	}
	if r := tracker.Revision(); r != revBefore {
		t.Errorf("a failed inventory merge must not publish a new revision, got %d, want %d", r, revBefore)
	}
	status := tracker.SyncStatus()
	if status.LastError == "" {
		t.Error("expected the inventory acquisition failure to be visible via SyncStatus.LastError")
	}
	if status.DashboardCampaigns != 1 {
		t.Errorf("expected dashboardCampaigns=1 recorded for the failed run (the listing succeeded), got %d", status.DashboardCampaigns)
	}

	// F2-T4: recovery -- a later successful sync updates progress normally.
	client.inventoryErr = nil
	client.inventory = progAt(200)
	tracker.syncCampaigns()

	got = tracker.Campaigns()
	if len(got) != 1 || got[0].Drops[0].CurrentMinutesWatched != 200 {
		t.Fatalf("recovery sync should update progress normally, got %+v", got)
	}
	if status := tracker.SyncStatus(); status.LastError != "" {
		t.Errorf("expected the sync error to clear on recovery, got %q", status.LastError)
	}
}

// TestSyncCampaignsUnusableInventoryResponsePreservesLastKnownGood is F2-T2:
// the same last-known-good preservation as F2-T1, but for a nil-error
// response that simply does not decode to a usable inventory object -- the
// other half of syncWithInventory's `err != nil || inventory == nil` guard,
// distinct from a request error.
func TestSyncCampaignsUnusableInventoryResponsePreservesLastKnownGood(t *testing.T) {
	cases := []struct {
		name      string
		inventory map[string]interface{}
	}{
		{name: "nil response", inventory: nil},
		{name: "malformed shape (no currentUser)", inventory: map[string]interface{}{"data": map[string]interface{}{}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			summary, detail, progAt := f2Fixture()
			client := &fakeDropsClient{
				dashboard: dashboardResponse(summary),
				inventory: progAt(140),
				details:   map[string]map[string]interface{}{"campaign-amd": detail},
			}
			tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
			tracker.syncCampaigns()
			if got := tracker.Campaigns(); len(got) != 1 || got[0].Drops[0].CurrentMinutesWatched != 140 {
				t.Fatalf("expected the last-known-good pool at 140 min, got %+v", got)
			}
			revBefore := tracker.Revision()

			client.inventory = tc.inventory // no error, but getInventory() cannot produce a usable map
			tracker.syncCampaigns()

			got := tracker.Campaigns()
			if len(got) != 1 || got[0].Drops[0].CurrentMinutesWatched != 140 {
				t.Fatalf("an unusable inventory response (%s) must preserve the last-known-good pool/progress, got %+v", tc.name, got)
			}
			if r := tracker.Revision(); r != revBefore {
				t.Errorf("an unusable inventory response (%s) must not publish a new revision, got %d, want %d", tc.name, r, revBefore)
			}
			if status := tracker.SyncStatus(); status.LastError == "" {
				t.Errorf("expected the unusable inventory response (%s) to surface via SyncStatus.LastError", tc.name)
			}
		})
	}
}

// TestSyncCampaignsValidEmptyInventoryIsNotAcquisitionFailure is F2-T3: a
// successfully decoded Inventory response that simply reports no
// dropCampaignsInProgress (I6) must remain distinguishable from a request
// error or a malformed response -- it is a legitimate observation, and the
// sync must publish normally rather than aborting.
func TestSyncCampaignsValidEmptyInventoryIsNotAcquisitionFailure(t *testing.T) {
	summary, detail, _ := f2Fixture()
	client := &fakeDropsClient{
		dashboard: dashboardResponse(summary),
		inventory: emptyInventoryResponse(), // valid, decoded, but no dropCampaignsInProgress
		details:   map[string]map[string]interface{}{"campaign-amd": detail},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tracker.syncCampaigns()

	got := tracker.Campaigns()
	if len(got) != 1 {
		t.Fatalf("a valid empty inventory must not be treated as an acquisition failure, got %d tracked", len(got))
	}
	status := tracker.SyncStatus()
	if status.LastError != "" {
		t.Errorf("a valid empty inventory must not surface as a sync error, got %q", status.LastError)
	}
	if status.Revision == 0 || status.UpdateSource != "full_sync" {
		t.Errorf("expected a normal full-sync publication, got revision=%d source=%q", status.Revision, status.UpdateSource)
	}
}
