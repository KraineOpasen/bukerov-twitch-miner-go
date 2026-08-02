package drops

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/twitch"
)

// skipLedgerAccountSeq is a process-wide monotonic counter backing
// uniqueAccountKey. It exists because database.Open is a process-wide
// singleton (see main_test.go) that every test in this package shares: a
// -count=20 rerun of the SAME test function sees the SAME t.Name() each time,
// so t.Name() alone is not enough to keep repeated runs' rows apart. The
// counter guarantees a fresh account_key on every call, so repeated runs
// exercise entirely disjoint rows and can never collide on either unique
// index (account-key isolation is exactly what the schema uses to keep
// separate accounts' ledgers apart -- see TestAccountIsolation).
var skipLedgerAccountSeq atomic.Int64

func uniqueAccountKey(t *testing.T) string {
	t.Helper()
	return t.Name() + "#" + strconv.FormatInt(skipLedgerAccountSeq.Add(1), 10)
}

// newTestSkipLedger mirrors newTestCatalog (catalog_test.go): database.Open
// is a process-wide singleton, so this always resolves to the SAME shared
// handle TestMain opened, regardless of the t.TempDir() argument.
func newTestSkipLedger(t *testing.T, accountKey string) *SkipLedger {
	t.Helper()
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	ledger, err := NewSkipLedger(db, accountKey)
	if err != nil {
		t.Fatalf("new skip ledger: %v", err)
	}
	return ledger
}

// claimableDrop builds a Drop whose CanClaim() is true (a minted instance,
// unclaimed, no precondition block) and which sits inside an active,
// feasible window -- the shape eligibility.EvaluateDrops accepts (mirrors
// assignActiveDrop in eligible_assignment_test.go).
func claimableDrop(id, instanceID string) *models.Drop {
	d := &models.Drop{
		ID: id, Name: "Reward " + id, MinutesRequired: 60, CurrentMinutesWatched: 10,
		StartAt: time.Now().Add(-time.Hour), EndAt: time.Now().Add(10 * time.Hour),
	}
	d.Update(map[string]interface{}{"dropInstanceID": instanceID, "currentMinutesWatched": float64(10)})
	return d
}

// claimedInProgressDrop builds a raw inventory timeBasedDrops entry reporting
// self.isClaimed=true with a benefit id and a minted instance id -- the exact
// shape observeClaimedFromInventory (E3/S4) reads.
func claimedInProgressDrop(id, name string, required, watched float64, instanceID, benefitID string) map[string]interface{} {
	return map[string]interface{}{
		"id":                     id,
		"name":                   name,
		"requiredMinutesWatched": required,
		"benefitEdges": []interface{}{
			map[string]interface{}{"benefit": map[string]interface{}{"id": benefitID, "name": name}},
		},
		"self": map[string]interface{}{
			"currentMinutesWatched": watched,
			"isClaimed":             true,
			"dropInstanceID":        instanceID,
		},
	}
}

// skipRowByComposite reads one row directly by its composite key, for test
// assertions that need to inspect state/state_reason after a Reconcile pass.
func skipRowByComposite(t *testing.T, ledger *SkipLedger, campaignID, dropID, instanceID string) skipRow {
	t.Helper()
	row := ledger.db.QueryRow(`
		SELECT `+skipRowColumns+`
		FROM drop_reward_skips
		WHERE account_key = ? AND campaign_id = ? AND drop_id = ? AND instance_id = ?`,
		ledger.accountKey, campaignID, dropID, instanceID)
	r, err := scanSkipRow(row.Scan)
	if err != nil {
		t.Fatalf("read row campaign=%s drop=%s instance=%q: %v", campaignID, dropID, instanceID, err)
	}
	return r
}

// forceRowState directly sets a ledger row's state via raw SQL, bypassing
// Observe/Reconcile, so a test can set up a "row is currently
// conflicting/released" precondition without depending on the very
// transition logic under test.
func forceRowState(t *testing.T, ledger *SkipLedger, instanceID, state string) {
	t.Helper()
	if _, err := ledger.db.Exec(`UPDATE drop_reward_skips SET state = ? WHERE account_key = ? AND instance_id = ?`,
		state, ledger.accountKey, instanceID); err != nil {
		t.Fatalf("force row state: %v", err)
	}
}

func forceRowStateByComposite(t *testing.T, ledger *SkipLedger, campaignID, dropID, instanceID, state string) {
	t.Helper()
	if _, err := ledger.db.Exec(`UPDATE drop_reward_skips SET state = ? WHERE account_key = ? AND campaign_id = ? AND drop_id = ? AND instance_id = ?`,
		state, ledger.accountKey, campaignID, dropID, instanceID); err != nil {
		t.Fatalf("force row state: %v", err)
	}
}

// countRows counts ledger rows matching a WHERE fragment, for "exactly N
// rows" assertions (no unique-constraint violation, no duplicate inserts).
func countRows(t *testing.T, ledger *SkipLedger, where string, args ...any) int {
	t.Helper()
	var n int
	if err := ledger.db.QueryRow("SELECT COUNT(1) FROM drop_reward_skips WHERE "+where, args...).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

// ---------------------------------------------------------------------------
// T1: distinct instances under the same composite stay isolated.
// ---------------------------------------------------------------------------

func TestDistinctInstancesSameCompositeStayIsolated(t *testing.T) {
	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	ctx := context.Background()

	base := skipEvidence{class: evidenceClaimAccepted, gameID: "g1", campaignID: "camp-1", dropID: "drop-1"}
	ev1, ev2 := base, base
	ev1.instanceID, ev2.instanceID = "i1", "i2"

	must(t, ledger.Observe(ctx, ev1))
	must(t, ledger.Observe(ctx, ev2)) // must NOT unique-violate against ev1's row

	if n := countRows(t, ledger, "account_key = ? AND campaign_id = ? AND drop_id = ?",
		ledger.accountKey, "camp-1", "drop-1"); n != 2 {
		t.Fatalf("expected 2 distinct rows for i1/i2 under the same composite, got %d", n)
	}

	snap, err := ledger.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	idFor := func(instance string) models.RewardIdentity {
		return models.RewardIdentity{GameID: "g1", CampaignID: "camp-1", DropID: "drop-1", InstanceID: instance}
	}
	if skip, reason := snap.decide(idFor("i1"), false); !skip {
		t.Errorf("Decide(i1) = FARM (%s), want SKIP", reason)
	}
	if skip, reason := snap.decide(idFor("i2"), false); !skip {
		t.Errorf("Decide(i2) = FARM (%s), want SKIP", reason)
	}
	if skip, reason := snap.decide(idFor("i3"), true); skip {
		t.Errorf("Decide(i3, CanClaim) = SKIP (%s), want FARM: a new minted instance must never be blocked", reason)
	}
}

// ---------------------------------------------------------------------------
// T2: the claim seam persists the COMPLETE identity bundle.
// ---------------------------------------------------------------------------

func TestClaimSeamPersistsCompleteIdentityBundle(t *testing.T) {
	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	client := &statusClient{fakeDropsClient: &fakeDropsClient{}, status: twitch.ClaimStatusAccepted}
	tr := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tr.SetSkipLedger(ledger)

	start := time.Now().Add(-time.Hour)
	end := time.Now().Add(time.Hour)
	campaign := &models.Campaign{
		ID: "camp-t2", Game: &models.Game{ID: "game-t2"},
		StartAt: start, EndAt: end,
	}
	drop := &models.Drop{ID: "drop-t2", Name: "Reward", BenefitID: "ben-t2", MinutesRequired: 60}
	drop.Update(map[string]interface{}{"dropInstanceID": "inst-t2", "currentMinutesWatched": float64(60)})

	if ok := tr.claimDropFnFor(campaign)(drop); !ok {
		t.Fatal("expected the claim to be reported successful")
	}

	snap, err := ledger.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	row, ok := snap.byInstance["inst-t2"]
	if !ok {
		t.Fatal("expected a ledger row keyed by the claimed instance")
	}
	if row.gameID != "game-t2" || row.benefitID != "ben-t2" || row.campaignID != "camp-t2" || row.dropID != "drop-t2" || row.instanceID != "inst-t2" {
		t.Fatalf("incomplete identity bundle persisted: %+v", row)
	}
	if !row.window.Known || row.window.Source != models.WindowSourceCampaign {
		t.Fatalf("expected a Known, campaign-sourced window, got %+v", row.window)
	}
	if row.window.Start.UnixMilli() != start.UnixMilli() || row.window.End.UnixMilli() != end.UnixMilli() {
		t.Fatalf("window bounds mismatch: got [%v,%v), want [%v,%v)", row.window.Start, row.window.End, start, end)
	}
}

// ---------------------------------------------------------------------------
// T3: the E3 observation is sourced from the raw decoded inventory maps, NOT
// from campaign.Drops, and is idempotent, for BOTH syncWithInventory and
// syncProgress.
//
// NOTE on what these tests can and cannot prove: observeClaimedFromInventory
// structurally never reads campaign.Drops at all (only the raw progData/dd
// maps), so its OUTPUT cannot differ based on whether it happens to run
// before or after some campaign-merge step touches a *models.Campaign --
// there is no "stripped vs. not-yet-stripped" state for it to observe
// differently. A test named "...SurvivesClearClaimedDrops" would therefore
// stay green even if a future change moved the call to run AFTER the merge
// loop, which is not what such a name implies. What IS genuinely
// discriminating -- and what these tests assert -- is that the tracked
// campaign carries NO drops of its own at all: if a regression ever made
// observeClaimedFromInventory start reading from campaign.Drops instead of
// the raw maps, it would find nothing here and no row would ever appear.
// ---------------------------------------------------------------------------

func TestE3ObservedFromRawInventoryNotCampaignDropsInSyncWithInventory(t *testing.T) {
	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	prog := map[string]interface{}{
		"id":   "camp-e3a",
		"name": "Camp E3a",
		"game": map[string]interface{}{"id": "game-e3a", "name": "Game"},
		"timeBasedDrops": []interface{}{
			claimedInProgressDrop("drop-e3a", "Reward", 60, 60, "inst-e3a", "ben-e3a"),
		},
	}
	client := &fakeDropsClient{inventory: inventoryWithInProgress(prog)}
	tr := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tr.SetSkipLedger(ledger)
	// No Drops at all on the tracked campaign -- see the NOTE above.
	tracked := []*models.Campaign{{ID: "camp-e3a", Game: &models.Game{ID: "game-e3a"}, Drops: nil}}
	tr.campaigns = tracked

	got, err := tr.syncWithInventory(tracked)
	if err != nil {
		t.Fatalf("syncWithInventory: %v", err)
	}
	if len(got) != 1 || len(got[0].Drops) != 0 {
		t.Fatalf("expected the campaign to remain trackable with no drops of its own, got %+v", got)
	}

	snap, err := ledger.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if _, ok := snap.byInstance["inst-e3a"]; !ok {
		t.Fatal("E3 observation must be sourced from the raw inventory map, not campaign.Drops (which was empty here)")
	}

	// Idempotent: a repeat sync must not create a second row.
	if _, err := tr.syncWithInventory(tracked); err != nil {
		t.Fatalf("syncWithInventory: %v", err)
	}
	if n := countRows(t, ledger, "account_key = ? AND instance_id = ?", ledger.accountKey, "inst-e3a"); n != 1 {
		t.Fatalf("expected exactly one row after repeated syncs, got %d", n)
	}
}

func TestE3ObservedFromRawInventoryNotCampaignDropsInSyncProgress(t *testing.T) {
	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	prog := map[string]interface{}{
		"id":   "camp-e3b",
		"name": "Camp E3b",
		"game": map[string]interface{}{"id": "game-e3b", "name": "Game"},
		"timeBasedDrops": []interface{}{
			claimedInProgressDrop("drop-e3b", "Reward", 60, 60, "inst-e3b", "ben-e3b"),
		},
	}
	client := &fakeDropsClient{inventory: inventoryWithInProgress(prog)}
	tr := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tr.SetSkipLedger(ledger)
	// No Drops at all on the tracked campaign -- see the NOTE above.
	tr.campaigns = []*models.Campaign{{ID: "camp-e3b", Game: &models.Game{ID: "game-e3b"}, Drops: nil}}

	tr.syncProgress()

	snap, err := ledger.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if _, ok := snap.byInstance["inst-e3b"]; !ok {
		t.Fatal("E3 observation via syncProgress must be sourced from the raw inventory map, not campaign.Drops (which was empty here)")
	}

	tr.syncProgress()
	if n := countRows(t, ledger, "account_key = ? AND instance_id = ?", ledger.accountKey, "inst-e3b"); n != 1 {
		t.Fatalf("expected exactly one row after repeated progress syncs, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// T4: a composite-only active row never blocks a new minted claimable
// instance (Decide=FARM, Reconcile->conflicting via SH4); control: the SAME
// recorded instance offered claimable again stays conflicting (SH3).
// ---------------------------------------------------------------------------

func TestReconcileCompositeOnlyRowSupersededByMintedClaimableInstance(t *testing.T) {
	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	ctx := context.Background()

	must(t, ledger.Observe(ctx, skipEvidence{
		class: evidenceInventoryClaimed, gameID: "g1", campaignID: "camp-t4", dropID: "drop-t4",
	}))

	candidate := models.RewardIdentity{GameID: "g1", CampaignID: "camp-t4", DropID: "drop-t4", InstanceID: "inst-new"}
	snap, err := ledger.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if skip, reason := snap.decide(candidate, true); skip {
		t.Fatalf("Decide = SKIP (%s), want FARM: a composite-only row must never block a newly-minted claimable instance", reason)
	}

	campaign := &models.Campaign{
		ID: "camp-t4", Game: &models.Game{ID: "g1"},
		Drops: []*models.Drop{claimableDrop("drop-t4", "inst-new")},
	}
	must(t, ledger.Reconcile(ctx, []*models.Campaign{campaign}))

	row := skipRowByComposite(t, ledger, "camp-t4", "drop-t4", "")
	if row.state != skipStateConflicting {
		t.Fatalf("expected the composite-only row to move to conflicting (SH4), got %q", row.state)
	}
}

func TestReconcileSameInstanceClaimableAgainStaysConflicting(t *testing.T) {
	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	ctx := context.Background()

	must(t, ledger.Observe(ctx, skipEvidence{
		class: evidenceClaimAccepted, gameID: "g1", campaignID: "camp-t4b", dropID: "drop-t4b", instanceID: "inst-t4b",
	}))

	campaign := &models.Campaign{
		ID: "camp-t4b", Game: &models.Game{ID: "g1"},
		Drops: []*models.Drop{claimableDrop("drop-t4b", "inst-t4b")},
	}
	must(t, ledger.Reconcile(ctx, []*models.Campaign{campaign}))

	row := skipRowByComposite(t, ledger, "camp-t4b", "drop-t4b", "inst-t4b")
	if row.state != skipStateConflicting {
		t.Fatalf("expected the same-instance claimable row to move to conflicting (SH3), got %q", row.state)
	}
}

// ---------------------------------------------------------------------------
// T5: re-arm by the same instance and by adoption of an instance-less
// composite row; NEVER re-armed by a different instance.
// ---------------------------------------------------------------------------

func TestObserveRearmsConflictingRowBySameInstance(t *testing.T) {
	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	ctx := context.Background()

	must(t, ledger.Observe(ctx, skipEvidence{
		class: evidenceClaimAccepted, gameID: "g1", campaignID: "c1", dropID: "d1", instanceID: "inst-a",
	}))
	forceRowState(t, ledger, "inst-a", skipStateConflicting)

	must(t, ledger.Observe(ctx, skipEvidence{
		class: evidenceClaimAccepted, gameID: "g1", campaignID: "c1", dropID: "d1", instanceID: "inst-a",
	}))

	snap, err := ledger.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if row, ok := snap.byInstance["inst-a"]; !ok || row.state != skipStateActive {
		t.Fatalf("expected re-arm to active for the SAME instance, got %+v ok=%v", row, ok)
	}
}

func TestObserveRearmsByAdoptingInstancelessCompositeRow(t *testing.T) {
	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	ctx := context.Background()

	must(t, ledger.Observe(ctx, skipEvidence{class: evidenceInventoryClaimed, gameID: "g1", campaignID: "c2", dropID: "d2"}))
	forceRowStateByComposite(t, ledger, "c2", "d2", "", skipStateReleased)

	must(t, ledger.Observe(ctx, skipEvidence{
		class: evidenceClaimAccepted, gameID: "g1", campaignID: "c2", dropID: "d2", instanceID: "inst-b",
	}))

	snap, err := ledger.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if row, ok := snap.byInstance["inst-b"]; !ok || row.state != skipStateActive {
		t.Fatalf("expected the instance-less row to be adopted and re-armed to active, got %+v ok=%v", row, ok)
	}
	if n := countRows(t, ledger, "account_key = ? AND campaign_id = ? AND drop_id = ?", ledger.accountKey, "c2", "d2"); n != 1 {
		t.Fatalf("expected the composite-only row to be enriched IN PLACE, not duplicated: got %d rows", n)
	}
}

func TestObserveNeverRearmsByDifferentInstance(t *testing.T) {
	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	ctx := context.Background()

	must(t, ledger.Observe(ctx, skipEvidence{
		class: evidenceClaimAccepted, gameID: "g1", campaignID: "c3", dropID: "d3", instanceID: "inst-c",
	}))
	forceRowState(t, ledger, "inst-c", skipStateReleased)

	// A DIFFERENT instance for the same composite must never touch inst-c's row.
	must(t, ledger.Observe(ctx, skipEvidence{
		class: evidenceClaimAccepted, gameID: "g1", campaignID: "c3", dropID: "d3", instanceID: "inst-d",
	}))

	snap, err := ledger.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if row, ok := snap.byInstance["inst-c"]; !ok || row.state != skipStateReleased {
		t.Fatalf("a different instance must NEVER re-arm the original row, got %+v ok=%v", row, ok)
	}
	if row, ok := snap.byInstance["inst-d"]; !ok || row.state != skipStateActive {
		t.Fatalf("expected a brand new active row for the different instance, got %+v ok=%v", row, ok)
	}
}

// ---------------------------------------------------------------------------
// T6: different known game IDs never match (game conflict beats both
// composite and benefit fallback).
// ---------------------------------------------------------------------------

func TestGameConflictExcludesCompositeAndBenefitMatches(t *testing.T) {
	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	ctx := context.Background()

	must(t, ledger.Observe(ctx, skipEvidence{
		class: evidenceClaimAccepted, gameID: "game-A", benefitID: "ben-shared",
		campaignID: "camp-shared", dropID: "drop-shared", instanceID: "inst-shared",
	}))

	snap, err := ledger.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	candidate := models.RewardIdentity{
		GameID: "game-B", BenefitID: "ben-shared", CampaignID: "camp-shared", DropID: "drop-shared",
	}
	if skip, reason := snap.decide(candidate, true); skip {
		t.Fatalf("Decide = SKIP (%s), want FARM: a different KNOWN game must exclude the row from composite AND benefit matching", reason)
	}
}

// ---------------------------------------------------------------------------
// T7: restart persistence (a NEW SkipLedger built over the SAME db).
// ---------------------------------------------------------------------------

func TestRestartPersistsAcrossNewLedgerInstance(t *testing.T) {
	account := uniqueAccountKey(t)
	ledger1 := newTestSkipLedger(t, account)
	ctx := context.Background()
	must(t, ledger1.Observe(ctx, skipEvidence{
		class: evidenceClaimAccepted, gameID: "g1", campaignID: "c-r1", dropID: "d-r1", instanceID: "inst-r1",
	}))

	ledger2, err := NewSkipLedger(ledger1.db, account) // simulated restart: same db, fresh in-memory ledger
	if err != nil {
		t.Fatalf("new ledger over the same db: %v", err)
	}
	snap, err := ledger2.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if row, ok := snap.byInstance["inst-r1"]; !ok || row.state != skipStateActive {
		t.Fatalf("expected the row to persist across a simulated restart, got %+v ok=%v", row, ok)
	}
}

// ---------------------------------------------------------------------------
// T8: account isolation.
// ---------------------------------------------------------------------------

func TestAccountIsolation(t *testing.T) {
	base := uniqueAccountKey(t)
	ledgerA := newTestSkipLedger(t, base+"-A")
	ledgerB := newTestSkipLedger(t, base+"-B")
	ctx := context.Background()

	must(t, ledgerA.Observe(ctx, skipEvidence{
		class: evidenceClaimAccepted, gameID: "g1", campaignID: "c-iso", dropID: "d-iso", instanceID: "inst-iso",
	}))

	snapB, err := ledgerB.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot B: %v", err)
	}
	if _, ok := snapB.byInstance["inst-iso"]; ok {
		t.Fatal("account B must not see account A's ledger row")
	}

	snapA, err := ledgerA.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot A: %v", err)
	}
	if _, ok := snapA.byInstance["inst-iso"]; !ok {
		t.Fatal("account A must see its own row")
	}
}

// ---------------------------------------------------------------------------
// T9: fail-open -- nil ledger, nil snapshot, and a deterministic (context-
// cancelled, non-destructive) read/write failure all fail open to FARM.
// ---------------------------------------------------------------------------

func TestDecideNilSnapshotFailsOpen(t *testing.T) {
	var snap *skipSnapshot // no ledger wired, or the snapshot load failed
	id := models.RewardIdentity{GameID: "g1", CampaignID: "c1", DropID: "d1", InstanceID: "i1"}
	if skip, reason := snap.decide(id, true); skip {
		t.Errorf("a nil snapshot must fail open (FARM), got skip=%v reason=%q", skip, reason)
	}
}

func TestBrokerViewNilSnapshotReturnsCampaignUnchanged(t *testing.T) {
	campaign := &models.Campaign{ID: "c1", Drops: []*models.Drop{{ID: "d1", Name: "R"}}}
	got := brokerView(campaign, nil)
	if got != campaign {
		t.Error("brokerView with a nil snapshot must return the ORIGINAL campaign pointer unchanged, not a clone")
	}
}

func TestObserveContextCancelledFailsOpenNoPartialRow(t *testing.T) {
	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // deterministic, non-destructive failure injection: no DB content is touched

	ev := skipEvidence{class: evidenceClaimAccepted, campaignID: "c1", dropID: "d1", instanceID: "inst-fail"}
	if err := ledger.Observe(ctx, ev); err == nil {
		t.Fatal("Observe with an already-cancelled context should return an error")
	}

	snap, err := ledger.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if _, ok := snap.byInstance["inst-fail"]; ok {
		t.Error("a failed Observe must not have created a partial row")
	}
}

func TestSnapshotContextCancelledFailsOpen(t *testing.T) {
	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := ledger.Snapshot(ctx); err == nil {
		t.Fatal("Snapshot with an already-cancelled context should return an error")
	}
	// The caller (updateStreamerCampaigns) treats a Snapshot error identically
	// to "no ledger wired": brokerView(campaign, nil) returns the campaign
	// unchanged (see TestBrokerViewNilSnapshotReturnsCampaignUnchanged).
}

// TestReconcileContextCancelledFailsOpenNoTransition (A-3): Reconcile honors
// ctx exactly like Observe/Snapshot -- an already-cancelled context fails the
// whole (single) transaction deterministically, without touching any DB
// content, so no row is left partially transitioned.
func TestReconcileContextCancelledFailsOpenNoTransition(t *testing.T) {
	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	must(t, ledger.Observe(context.Background(), skipEvidence{
		class: evidenceInventoryClaimed, gameID: "g1", campaignID: "camp-a3", dropID: "drop-a3",
	}))

	campaign := &models.Campaign{
		ID: "camp-a3", Game: &models.Game{ID: "g1"},
		Drops: []*models.Drop{claimableDrop("drop-a3", "inst-a3-new")}, // would trigger SH4 if applied
	}

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ledger.Reconcile(cancelledCtx, []*models.Campaign{campaign}); err == nil {
		t.Fatal("Reconcile with an already-cancelled context should return an error")
	}

	row := skipRowByComposite(t, ledger, "camp-a3", "drop-a3", "")
	if row.state != skipStateActive {
		t.Fatalf("a failed Reconcile must leave the row untouched (active), got %q", row.state)
	}
}

// ---------------------------------------------------------------------------
// T10: the raw catalog + d.campaigns + published campaigns remain unchanged
// after a suppressing sync; only the broker-facing CLONE is filtered.
// ---------------------------------------------------------------------------

func TestBrokerFilterNeverMutatesSourceCampaigns(t *testing.T) {
	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	ctx := context.Background()
	must(t, ledger.Observe(ctx, skipEvidence{
		class: evidenceClaimAccepted, gameID: "g1", campaignID: "camp-t10", dropID: "drop-t10", instanceID: "inst-t10",
	}))

	ghost := claimableDrop("drop-t10", "inst-t10")
	campaign := campaignFor("camp-t10", unrestrictedACL(), ghost)

	s := models.NewStreamer("streamer-t10", models.StreamerSettings{ClaimDrops: true})
	s.ChannelID = "chan-t10"
	s.SetConfirmedOnline()
	s.Stream.SetCampaignIDs([]string{"camp-t10"})

	tr := &DropsTracker{streamers: []*models.Streamer{s}, campaigns: []*models.Campaign{campaign}, skipLedger: ledger}
	tr.updateStreamerCampaigns()

	got := tr.Campaigns()
	if len(got) != 1 || got[0] != campaign {
		t.Fatalf("Campaigns() must still return the SAME, unfiltered, byte-identical source object, got %+v", got)
	}
	if len(campaign.Drops) != 1 || campaign.Drops[0] != ghost {
		t.Fatalf("the source campaign's Drops slice/pointers must never be mutated in place, got %+v", campaign.Drops)
	}
	if campaign.Drops[0].IsClaimed {
		t.Fatal("the source drop must never be mutated by the broker filter")
	}

	// But the streamer's ASSIGNED view is filtered (the ghost, its only drop,
	// is gone) -- proving suppression really happened, on a clone only.
	if assigned := s.Stream.GetCampaigns(); len(assigned) != 0 {
		t.Fatalf("expected the streamer to lose its only (ghost) campaign, got %+v", assigned)
	}
}

// ---------------------------------------------------------------------------
// T11: broker eligibility is actually gated -- asserted on
// HasEligibleAssignedDropCampaign (the real slot gate), never DropsCondition.
// ---------------------------------------------------------------------------

func TestBrokerFilterGatesHasEligibleAssignedDropCampaign(t *testing.T) {
	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	ctx := context.Background()
	must(t, ledger.Observe(ctx, skipEvidence{
		class: evidenceClaimAccepted, gameID: "g1", campaignID: "camp-t11", dropID: "drop-t11", instanceID: "inst-t11",
	}))

	ghost := claimableDrop("drop-t11", "inst-t11")
	campaign := campaignFor("camp-t11", unrestrictedACL(), ghost)

	// Baseline: WITHOUT a ledger wired, the ghost is still assigned.
	trBaseline, sBaseline := assignmentTracker(models.CapabilityEnabled, campaign, func(s *models.Streamer) {
		s.Stream.SetCampaignIDs([]string{"camp-t11"})
	})
	trBaseline.updateStreamerCampaigns()
	if !sBaseline.HasEligibleAssignedDropCampaign() {
		t.Fatal("baseline: without a ledger, the ghost campaign should still be assigned")
	}

	// WITH the ledger wired: the ghost must be excluded and eligibility flips.
	trFiltered, sFiltered := assignmentTracker(models.CapabilityEnabled, campaign, func(s *models.Streamer) {
		s.Stream.SetCampaignIDs([]string{"camp-t11"})
	})
	trFiltered.skipLedger = ledger
	trFiltered.updateStreamerCampaigns()
	if sFiltered.HasEligibleAssignedDropCampaign() {
		t.Fatal("expected HasEligibleAssignedDropCampaign to flip false once the ghost is broker-filtered")
	}
}

// ---------------------------------------------------------------------------
// Defensive boundary hardening: a nil element in campaign.Drops must never
// panic brokerView. This is NOT a claim that a nil *models.Drop is currently
// reachable from production (models.NewDropFromGQL, the only production
// constructor, never returns one) -- it is that brokerView sits on the
// broker-assignment path and should not be the thing that panics if a nil
// ever appears there, and that it should agree with suppressedDrops/
// Reconcile (both in this file), which already guard against nil drops via
// their own `if drop == nil { continue }` checks.
//
// Without the guard, campaign.Clone() (models/campaign.go) does `dc := *d`
// for every element of c.Drops, so a nil element panics INSIDE Clone before
// brokerView's own filtering loop ever runs.
// ---------------------------------------------------------------------------

func TestBrokerViewNilDropInSourceDoesNotPanic(t *testing.T) {
	validDrop := claimableDrop("drop-nilguard", "inst-nilguard")
	campaign := &models.Campaign{
		ID: "camp-nilguard", Name: "C-nilguard", Game: &models.Game{ID: "game-nilguard"},
		StartAt: time.Now().Add(-time.Hour), EndAt: time.Now().Add(10 * time.Hour),
		Drops: []*models.Drop{nil, validDrop},
	}
	// A non-nil, empty snapshot -- no rule should suppress the valid drop --
	// so brokerView takes the real filtering path instead of the nil-snapshot
	// fail-open early return.
	snap := &skipSnapshot{
		byInstance:  make(map[string]skipRow),
		byComposite: make(map[compositeKey][]skipRow),
		byBenefit:   make(map[string][]skipRow),
	}

	view := brokerView(campaign, snap) // must NOT panic on the nil element

	if len(view.Drops) != 1 || view.Drops[0] == nil || view.Drops[0].ID != validDrop.ID {
		t.Fatalf("expected the returned view to keep only the valid drop, got %+v", view.Drops)
	}
	if view.Drops[0] == validDrop {
		t.Fatal("expected the returned view's drop to be a CLONE, not the source pointer (existing immutability guarantee)")
	}

	// The SOURCE campaign must be completely unaffected: same length, the
	// same nil element still in place, and the same *models.Drop pointer for
	// the valid entry (pointer identity, not just an equal value).
	if len(campaign.Drops) != 2 {
		t.Fatalf("source campaign.Drops length must be unchanged, got %d", len(campaign.Drops))
	}
	if campaign.Drops[0] != nil {
		t.Fatalf("source campaign.Drops[0] must remain nil, got %+v", campaign.Drops[0])
	}
	if campaign.Drops[1] != validDrop {
		t.Fatal("source campaign.Drops[1] must remain the SAME *models.Drop pointer, unmutated")
	}
}

// ---------------------------------------------------------------------------
// Retention: Prune only ever deletes RELEASED rows past the given horizon.
// ---------------------------------------------------------------------------

func TestPruneOnlyDeletesReleasedRowsPastHorizon(t *testing.T) {
	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	ctx := context.Background()
	base := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ledger.now = func() time.Time { return base }

	must(t, ledger.Observe(ctx, skipEvidence{class: evidenceClaimAccepted, campaignID: "c1", dropID: "d1", instanceID: "inst-active"}))
	must(t, ledger.Observe(ctx, skipEvidence{class: evidenceClaimAccepted, campaignID: "c2", dropID: "d2", instanceID: "inst-old-released"}))
	must(t, ledger.Observe(ctx, skipEvidence{class: evidenceClaimAccepted, campaignID: "c3", dropID: "d3", instanceID: "inst-recent-released"}))

	ledger.now = func() time.Time { return base.Add(-10 * 24 * time.Hour) }
	forceReleaseAt(t, ledger, "inst-old-released", ledger.now())
	ledger.now = func() time.Time { return base.Add(-1 * time.Hour) }
	forceReleaseAt(t, ledger, "inst-recent-released", ledger.now())

	horizon := base.Add(-24 * time.Hour)
	n, err := ledger.Prune(horizon)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 row pruned (the old released one), got %d", n)
	}

	snap, err := ledger.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if _, ok := snap.byInstance["inst-active"]; !ok {
		t.Error("Prune must never delete an active row")
	}
	if _, ok := snap.byInstance["inst-recent-released"]; !ok {
		t.Error("Prune must not delete a released row more recent than the horizon")
	}
	if _, ok := snap.byInstance["inst-old-released"]; ok {
		t.Error("Prune should have deleted the old released row")
	}
}

// forceReleaseAt directly sets a row to released with resolved_at_ms = at,
// bypassing the transition machinery (that machinery is exercised elsewhere;
// this test is about Prune's own WHERE clause).
func forceReleaseAt(t *testing.T, ledger *SkipLedger, instanceID string, at time.Time) {
	t.Helper()
	if _, err := ledger.db.Exec(
		`UPDATE drop_reward_skips SET state = ?, resolved_at_ms = ? WHERE account_key = ? AND instance_id = ?`,
		skipStateReleased, at.UnixMilli(), ledger.accountKey, instanceID,
	); err != nil {
		t.Fatalf("force release: %v", err)
	}
}

// TestPruneIsAccountScoped is the regression guard for the proven cross-
// account deletion bug: Prune's DELETE was missing an account_key predicate
// (present on every other statement in this file), so ledgerA.Prune() could
// delete ledgerB's released rows -- a live hazard because internal/drops
// shares one process-wide DB across every account's ledger. Account B's old
// released row must survive account A's Prune call untouched.
func TestPruneIsAccountScoped(t *testing.T) {
	base := uniqueAccountKey(t)
	ledgerA := newTestSkipLedger(t, base+"-A")
	ledgerB := newTestSkipLedger(t, base+"-B")
	ctx := context.Background()
	old := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	ledgerA.now = func() time.Time { return old }
	ledgerB.now = func() time.Time { return old }

	must(t, ledgerA.Observe(ctx, skipEvidence{class: evidenceClaimAccepted, campaignID: "c1", dropID: "d1", instanceID: "inst-a-old"}))
	must(t, ledgerB.Observe(ctx, skipEvidence{class: evidenceClaimAccepted, campaignID: "c1", dropID: "d1", instanceID: "inst-b-old"}))
	forceReleaseAt(t, ledgerA, "inst-a-old", old)
	forceReleaseAt(t, ledgerB, "inst-b-old", old)

	horizon := old.Add(24 * time.Hour) // well past both rows' resolved_at_ms
	n, err := ledgerA.Prune(horizon)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected ledgerA.Prune to delete exactly its own 1 row, got %d", n)
	}

	snapA, err := ledgerA.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot A: %v", err)
	}
	if _, ok := snapA.byInstance["inst-a-old"]; ok {
		t.Error("account A's own old released row should have been pruned")
	}

	snapB, err := ledgerB.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot B: %v", err)
	}
	if _, ok := snapB.byInstance["inst-b-old"]; !ok {
		t.Fatal("account B's row must survive account A's Prune call -- Prune must be account-scoped")
	}
}

// ---------------------------------------------------------------------------
// INVARIANT 3: benefit-only / name-only evidence never creates a row.
// ---------------------------------------------------------------------------

func TestObserveBenefitOnlyOrNameOnlyEvidenceNeverCreatesRow(t *testing.T) {
	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	ctx := context.Background()

	// Benefit-only: no instance ID, no campaign+drop composite.
	must(t, ledger.Observe(ctx, skipEvidence{class: evidenceClaimAlready, gameID: "g1", benefitID: "ben-only"}))
	// Name-only: nothing but a name (and a game).
	must(t, ledger.Observe(ctx, skipEvidence{class: evidenceInventoryClaimed, gameID: "g1", name: "Coin"}))
	// Benefit + name together, still no instance/composite.
	must(t, ledger.Observe(ctx, skipEvidence{class: evidenceClaimAccepted, gameID: "g1", benefitID: "ben-only", name: "Coin"}))

	if n := countRows(t, ledger, "account_key = ?", ledger.accountKey); n != 0 {
		t.Fatalf("benefit-only/name-only evidence must never create a row (INVARIANT 3), got %d rows", n)
	}
}

// ---------------------------------------------------------------------------
// A-7: state_reason must never claim a re-arm that never happened.
// ---------------------------------------------------------------------------

func TestEnrichRowDoesNotClaimRearmWhenAlreadyActive(t *testing.T) {
	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	ctx := context.Background()

	must(t, ledger.Observe(ctx, skipEvidence{
		class: evidenceClaimAccepted, gameID: "g1", campaignID: "c-a7", dropID: "d-a7", instanceID: "inst-a7",
	}))
	// A second, instance-bearing observation for the SAME already-active row:
	// the re-arm predicate (state = ... OR instance_id = ev.instance) is still
	// satisfied, but nothing is actually re-armed (the row was already
	// active), so state_reason must stay untouched (it started as "" from
	// insertRow).
	must(t, ledger.Observe(ctx, skipEvidence{
		class: evidenceClaimAlready, gameID: "g1", campaignID: "c-a7", dropID: "d-a7", instanceID: "inst-a7",
	}))

	row := skipRowByComposite(t, ledger, "c-a7", "d-a7", "inst-a7")
	if row.state != skipStateActive {
		t.Fatalf("expected the row to remain active, got %q", row.state)
	}
	var reason string
	if err := ledger.db.QueryRow(`SELECT state_reason FROM drop_reward_skips WHERE account_key = ? AND instance_id = ?`,
		ledger.accountKey, "inst-a7").Scan(&reason); err != nil {
		t.Fatalf("read state_reason: %v", err)
	}
	if reason != "" {
		t.Fatalf(`state_reason must not claim a re-arm ("rearmed_by_...") for a row that was already active, got %q`, reason)
	}
}

// ---------------------------------------------------------------------------
// B-5: a FAILED (not merely nil) snapshot load must compose correctly
// through the real updateStreamerCampaigns path -- campaigns stay
// unfiltered, exactly like the no-ledger-wired baseline.
// ---------------------------------------------------------------------------

func TestUpdateStreamerCampaignsFailsOpenWhenSnapshotLoadFails(t *testing.T) {
	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	ctx := context.Background()
	must(t, ledger.Observe(ctx, skipEvidence{
		class: evidenceClaimAccepted, gameID: "g1", campaignID: "camp-snapfail", dropID: "drop-snapfail", instanceID: "inst-snapfail",
	}))

	ghost := claimableDrop("drop-snapfail", "inst-snapfail")
	campaign := campaignFor("camp-snapfail", unrestrictedACL(), ghost)

	tr, s := assignmentTracker(models.CapabilityEnabled, campaign, func(s *models.Streamer) {
		s.Stream.SetCampaignIDs([]string{"camp-snapfail"})
	})
	tr.skipLedger = ledger

	// Sanity check first: with a WORKING snapshot load, this exact ghost is
	// suppressed (mirrors TestBrokerFilterGatesHasEligibleAssignedDropCampaign,
	// establishing the filtered baseline this test's fail-open case contrasts
	// against).
	tr.updateStreamerCampaigns()
	if s.HasEligibleAssignedDropCampaign() {
		t.Fatal("precondition failed: the ghost should be suppressed when the snapshot load succeeds")
	}

	// Force the NEXT Snapshot load to fail deterministically: an already-
	// cancelled tracker lifecycle ctx makes skipLedgerCtx's derived context
	// immediately Done, so d.skipLedger.Snapshot(ctx) returns a real error
	// without touching the DB's actual content (same non-destructive
	// technique as TestSnapshotContextCancelledFailsOpen, exercised here
	// through the production updateStreamerCampaigns path instead of calling
	// SkipLedger.Snapshot directly).
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	tr.ctx = cancelledCtx

	tr.updateStreamerCampaigns()

	// A FAILED (not nil) snapshot must fail open exactly like a nil ledger:
	// the ghost campaign is reinstated, not left suppressed from the prior
	// successful pass.
	if !s.HasEligibleAssignedDropCampaign() {
		t.Fatal("a failed Snapshot load must fail open (leave campaigns unfiltered), but the ghost stayed suppressed")
	}
}

// ---------------------------------------------------------------------------
// A-1: ghost-skip suppression is observable -- a DEBUG log per suppressed
// drop (via captureLogs, logging_test.go, same package), a suppressed-drop
// counter on the pipeline summary line, and a read-only diagnostics
// accessor. Before this an operator's only recourse was opening miner.db.
// ---------------------------------------------------------------------------

func TestBrokerViewLogsEachSuppressedDrop(t *testing.T) {
	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	ctx := context.Background()
	must(t, ledger.Observe(ctx, skipEvidence{
		class: evidenceClaimAccepted, gameID: "g1", campaignID: "camp-a1", dropID: "drop-a1", instanceID: "inst-a1",
	}))

	ghost := claimableDrop("drop-a1", "inst-a1")
	campaign := campaignFor("camp-a1", unrestrictedACL(), ghost)

	snap, err := ledger.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	logs := captureLogs(t, func() { brokerView(campaign, snap) })
	if !strings.Contains(logs, "Drop suppressed by ghost-skip ledger") {
		t.Fatalf("expected a suppression log line, got:\n%s", logs)
	}
	if !strings.Contains(logs, "same_instance") {
		t.Fatalf("expected the decide() reason in the log line, got:\n%s", logs)
	}
	if !strings.Contains(logs, "camp-a1") || !strings.Contains(logs, "drop-a1") {
		t.Fatalf("expected campaign/drop identity in the log line, got:\n%s", logs)
	}
}

func TestSuppressedDropsAccessorReportsGhosts(t *testing.T) {
	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	ctx := context.Background()
	must(t, ledger.Observe(ctx, skipEvidence{
		class: evidenceClaimAccepted, gameID: "g1", campaignID: "camp-a1b", dropID: "drop-a1b", instanceID: "inst-a1b",
	}))

	ghost := claimableDrop("drop-a1b", "inst-a1b")
	campaign := campaignFor("camp-a1b", unrestrictedACL(), ghost)

	tr := &DropsTracker{campaigns: []*models.Campaign{campaign}, skipLedger: ledger}

	got := tr.SuppressedDrops()
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 suppressed drop, got %d: %+v", len(got), got)
	}
	if got[0].CampaignID != "camp-a1b" || got[0].DropID != "drop-a1b" || got[0].Reason != "same_instance" {
		t.Fatalf("unexpected suppressed-drop diagnostic: %+v", got[0])
	}

	// A nil/unwired ledger must report nothing (fail open) rather than
	// panicking or requiring the caller to nil-check the ledger itself.
	trNoLedger := &DropsTracker{campaigns: []*models.Campaign{campaign}}
	if got := trNoLedger.SuppressedDrops(); got != nil {
		t.Fatalf("expected no suppressed drops without a ledger wired, got %+v", got)
	}
}

// TestSyncCampaignsLogsSuppressedDropCount drives the real full-sync pipeline
// end to end and asserts the "Drops sync: campaign counts through the
// pipeline" DEBUG line carries the suppressedByGhostSkipLedger counter --
// the operator-visible diagnostic A-1 adds to the pre-existing pipeline
// summary. The pre-seeded ledger row matches the synced campaign's
// campaign_id+drop_id (COMPOSITE tier); the drop's own dropInstanceID is
// never populated here (inventory is empty), which is exactly the point --
// suppression must not depend on the inventory sync step at all.
func TestSyncCampaignsLogsSuppressedDropCount(t *testing.T) {
	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	must(t, ledger.Observe(context.Background(), skipEvidence{
		class: evidenceClaimAccepted, gameID: "game-a1c", campaignID: "camp-a1c", dropID: "drop-a1c",
	}))

	summary := map[string]interface{}{
		"id": "camp-a1c", "name": "Camp A1c", "status": "ACTIVE",
		"game": map[string]interface{}{"id": "game-a1c", "name": "Game"},
	}
	detail := map[string]interface{}{
		"id": "camp-a1c", "name": "Camp A1c", "status": "ACTIVE",
		"startAt": rfc3339(time.Now().Add(-time.Hour)),
		"endAt":   rfc3339(time.Now().Add(10 * time.Hour)),
		"game":    map[string]interface{}{"id": "game-a1c", "name": "Game"},
		"timeBasedDrops": []interface{}{
			activeDrop("drop-a1c", "Ghost", 60),
		},
	}
	client := &fakeDropsClient{
		dashboard: dashboardResponse(summary),
		inventory: emptyInventoryResponse(),
		details:   map[string]map[string]interface{}{"camp-a1c": detail},
	}
	tr := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tr.SetSkipLedger(ledger)

	logs := captureLogs(t, tr.syncCampaigns)

	if !strings.Contains(logs, "suppressedByGhostSkipLedger=1") {
		t.Fatalf("expected the pipeline debug line to report 1 suppressed drop, got:\n%s", logs)
	}
}

// ---------------------------------------------------------------------------
// Migration/registration failure injection (miner FIX 2, scenario 1's
// drops-package half). NewSkipLedger carries NO injectable seam around
// db.RegisterModule -- none is needed. database.DB embeds an EXPORTED
// *sql.DB (internal/database/database.go), so a REAL RegisterModule/
// migration failure is reachable directly: open a private, non-singleton
// *database.DB over its own temp file (never database.Open, which would hit
// the process-wide singleton every other test in this binary shares -- see
// main_test.go) and seed it with a conflicting drop_reward_skips table
// BEFORE calling NewSkipLedger. This is the same pattern already used in 13+
// places in this repo (e.g. internal/miner/srap_test.go's openRawMinerDB,
// internal/streamerlifecycle's openRawDB).
//
// This proves NewSkipLedger's REAL contract, through its REAL, unmodified
// code path: a migration failure returns a clean (nil, error) carrying the
// genuine SQLite error, and the failure leaves no drop_skip_ledger row in
// schema_versions -- so a later RegisterModule call can never mistake the
// failed attempt for a completed migration and skip re-running it -- plus a
// genuine transactional rollback of an earlier statement that DID execute
// within the same migration transaction (see the poisoning comment inside
// the test for exactly which statement and why). The failure is also scoped
// to its own db: a completely separate, clean private db still registers and
// works normally. The miner-level consequences (startup continues,
// SetSkipLedger not called, SkipLedgerEnabled()==false, the failure is
// recorded via events.TypeModuleInitFailed + slog) are proved separately in
// internal/miner/miner_test.go's TestSetupComponentsSkipLedgerMigrationFailureFailsOpen,
// which reaches the SAME real mechanism (a pre-poisoned private db) through
// a real Run().
// ---------------------------------------------------------------------------

func TestNewSkipLedgerMigrationFailureNoPartialState(t *testing.T) {
	poisonedPath := filepath.Join(t.TempDir(), "miner.db")
	poisonedSQL, err := sql.Open("sqlite", poisonedPath)
	if err != nil {
		t.Fatalf("open private sqlite db: %v", err)
	}
	defer func() { _ = poisonedSQL.Close() }()
	poisonedSQL.SetMaxOpenConns(1)
	poisonedDB := &database.DB{DB: poisonedSQL}

	// Seed a table named after one of the migration's OWN indexes --
	// ux_drop_skips_composite -- BEFORE NewSkipLedger ever runs. This is
	// deliberately NOT a conflict on drop_reward_skips itself: that would
	// make `CREATE TABLE IF NOT EXISTS drop_reward_skips` a no-op (nothing
	// pre-existing there to fail on further down), meaning nothing in the
	// migration's own transaction would ever genuinely execute before the
	// failure -- leaving "no row afterward" true for the trivial reason that
	// nothing was ever written, not because anything was rolled back.
	// Instead: the real `CREATE TABLE IF NOT EXISTS drop_reward_skips` and
	// the real `CREATE UNIQUE INDEX ux_drop_skips_instance` BOTH genuinely
	// execute first (a real write, inside the migration's one transaction --
	// see applyMigration, internal/database/database.go), and only the
	// following `CREATE UNIQUE INDEX IF NOT EXISTS ux_drop_skips_composite`
	// statement fails: SQLite's IF NOT EXISTS only suppresses "an index by
	// this name already exists" -- it still errors on a NAME COLLISION with
	// an object of a DIFFERENT type (here, our pre-seeded TABLE). That failure
	// then rolls back the whole transaction, INCLUDING the drop_reward_skips
	// table the same transaction already created -- which the assertion
	// below actually observes.
	if _, err := poisonedDB.Exec(`CREATE TABLE ux_drop_skips_composite (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("seed conflicting table: %v", err)
	}

	ledger, err := NewSkipLedger(poisonedDB, uniqueAccountKey(t))
	if ledger != nil {
		t.Fatalf("expected a nil ledger on a real migration failure, got %+v", ledger)
	}
	if err == nil {
		t.Fatal("expected a real migration error")
	}
	if !strings.Contains(err.Error(), "failed to register drop_skip_ledger module") {
		t.Fatalf("expected NewSkipLedger's own wrap around the RegisterModule error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "ux_drop_skips_composite") {
		t.Fatalf("expected the genuine SQLite name-collision error to surface, got: %v", err)
	}

	// Real transactional rollback, observed directly: drop_reward_skips
	// itself was created by an earlier statement that genuinely executed
	// inside this same failed migration transaction (see the poisoning
	// comment above) -- it must not exist afterward either, proving the
	// whole transaction rolled back rather than merely "the failing
	// statement's own effect" never having happened.
	var tableName string
	tableErr := poisonedDB.QueryRow(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'drop_reward_skips'`,
	).Scan(&tableName)
	if !errors.Is(tableErr, sql.ErrNoRows) {
		t.Fatalf("expected drop_reward_skips to not exist after a rolled-back migration, got name=%q err=%v", tableName, tableErr)
	}

	// And, consistently, no schema_versions row either: a failed migration
	// leaves no version marker that could let a later RegisterModule call
	// mistake this attempt for a completed one and skip re-running it.
	var version int
	scanErr := poisonedDB.QueryRow(`SELECT version FROM schema_versions WHERE module = ?`, "drop_skip_ledger").Scan(&version)
	if !errors.Is(scanErr, sql.ErrNoRows) {
		t.Fatalf("expected no schema_versions row for drop_skip_ledger after a rolled-back migration, got version=%d err=%v", version, scanErr)
	}

	// Not poisoning anything beyond its own target db: a completely
	// separate, clean private db still registers and works normally.
	cleanPath := filepath.Join(t.TempDir(), "clean.db")
	cleanSQL, err := sql.Open("sqlite", cleanPath)
	if err != nil {
		t.Fatalf("open clean private sqlite db: %v", err)
	}
	defer func() { _ = cleanSQL.Close() }()
	cleanSQL.SetMaxOpenConns(1)
	cleanDB := &database.DB{DB: cleanSQL}

	ledger2, err := NewSkipLedger(cleanDB, uniqueAccountKey(t))
	if err != nil {
		t.Fatalf("expected a clean db to register successfully, got %v", err)
	}
	if ledger2 == nil {
		t.Fatal("expected a usable ledger on the clean db")
	}
	if _, err := ledger2.Snapshot(context.Background()); err != nil {
		t.Fatalf("the clean ledger must be genuinely usable, snapshot failed: %v", err)
	}
	var cleanVersion int
	if err := cleanDB.QueryRow(`SELECT version FROM schema_versions WHERE module = ?`, "drop_skip_ledger").Scan(&cleanVersion); err != nil {
		t.Fatalf("expected the clean db to have registered the module: %v", err)
	}
	if cleanVersion != 1 {
		t.Errorf("clean db drop_skip_ledger version = %d, want 1", cleanVersion)
	}
}

// ---------------------------------------------------------------------------
// Benefit-tier decide() branches: a Benefit ID identifies a reward FAMILY, not
// a specific occurrence, so tier 3 (BENEFIT) only ever returns SKIP when both
// windows are decidable AND overlapping (same_benefit_overlapping_window).
// Every other outcome in this tier -- either window unknown, provably
// disjoint windows, or a game conflict that excludes the row from
// activeBenefit before the window is ever consulted -- shares the same
// "benefit_window_undecidable" fallthrough (fail open by default). Tier 1
// (INSTANCE) separately consults activeBenefit, but only to prove a
// DIFFERENT already-recorded instance makes the candidate a new occurrence
// ("new_minted_instance") -- that guard must terminate BEFORE tier 3 ever
// runs, or two distinct grants of the same reward family would collapse onto
// one SKIP.
// ---------------------------------------------------------------------------

func TestSkipSnapshotDecideBenefitTierMatrix(t *testing.T) {
	windowEarly := models.EntitlementWindow{
		Start: time.Date(2031, 3, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2031, 3, 1, 6, 0, 0, 0, time.UTC),
		Source: models.WindowSourceCampaign, Known: true,
	}
	windowOverlapsEarly := models.EntitlementWindow{
		Start: time.Date(2031, 3, 1, 3, 0, 0, 0, time.UTC), End: time.Date(2031, 3, 1, 9, 0, 0, 0, time.UTC),
		Source: models.WindowSourceCampaign, Known: true,
	}
	windowLaterDisjoint := models.EntitlementWindow{
		Start: time.Date(2031, 3, 2, 0, 0, 0, 0, time.UTC), End: time.Date(2031, 3, 2, 6, 0, 0, 0, time.UTC),
		Source: models.WindowSourceCampaign, Known: true,
	}
	var windowUnknown models.EntitlementWindow // zero value: Known=false

	tests := []struct {
		name           string
		ledgerEvidence skipEvidence
		candidate      models.RewardIdentity
		wantSkip       bool
		wantReason     string
	}{
		{
			// B1: known, overlapping windows on both sides. The candidate
			// carries no CampaignID/DropID/InstanceID at all, so tiers 1 and 2
			// are structurally never entered (their guards require non-empty
			// fields the candidate doesn't have here) -- only tier 3 can
			// produce this result. The candidate's name is never set either,
			// confirming the match needs no name comparison.
			name: "B1_same_benefit_overlapping_window_is_skip",
			ledgerEvidence: skipEvidence{
				class: evidenceClaimAccepted, gameID: "game-b1", benefitID: "ben-b1",
				campaignID: "camp-b1", dropID: "drop-b1", window: windowEarly,
			},
			candidate:  models.RewardIdentity{GameID: "game-b1", BenefitID: "ben-b1", Window: windowOverlapsEarly},
			wantSkip:   true,
			wantReason: "same_benefit_overlapping_window",
		},
		{
			// B2: both windows decidable but the candidate is a LATER,
			// disjoint occurrence -- provably a different grant of the same
			// reward family, so this later occurrence must remain farmable.
			name: "B2_same_benefit_disjoint_window_is_farm",
			ledgerEvidence: skipEvidence{
				class: evidenceClaimAccepted, gameID: "game-b2", benefitID: "ben-b2",
				campaignID: "camp-b2", dropID: "drop-b2", window: windowEarly,
			},
			candidate:  models.RewardIdentity{GameID: "game-b2", BenefitID: "ben-b2", Window: windowLaterDisjoint},
			wantSkip:   false,
			wantReason: "benefit_window_undecidable",
		},
		{
			// B3: the candidate's own window is unknown -- sameness cannot be
			// proven, so this fails open to FARM exactly like B2.
			name: "B3_candidate_window_unknown_fails_open_to_farm",
			ledgerEvidence: skipEvidence{
				class: evidenceClaimAccepted, gameID: "game-b3", benefitID: "ben-b3",
				campaignID: "camp-b3", dropID: "drop-b3", window: windowEarly,
			},
			candidate:  models.RewardIdentity{GameID: "game-b3", BenefitID: "ben-b3", Window: windowUnknown},
			wantSkip:   false,
			wantReason: "benefit_window_undecidable",
		},
		{
			// B4: the same fail-open rule from the OTHER side -- the recorded
			// ledger row itself never had a known window (e.g. an E3
			// inventory sighting, which carries no dates of its own).
			name: "B4_ledger_row_window_unknown_fails_open_to_farm",
			ledgerEvidence: skipEvidence{
				class: evidenceClaimAccepted, gameID: "game-b4", benefitID: "ben-b4",
				campaignID: "camp-b4", dropID: "drop-b4", // window left zero-value (unknown)
			},
			candidate:  models.RewardIdentity{GameID: "game-b4", BenefitID: "ben-b4", Window: windowEarly},
			wantSkip:   false,
			wantReason: "benefit_window_undecidable",
		},
		{
			// B5: without the game conflict this candidate would SKIP (same
			// benefit, overlapping windows) -- the different KNOWN game must
			// exclude the row from activeBenefit before the window is ever
			// consulted, so the decision terminates as FARM.
			name: "B5_different_known_game_ids_is_farm_before_benefit_match",
			ledgerEvidence: skipEvidence{
				class: evidenceClaimAccepted, gameID: "game-b5-a", benefitID: "ben-b5",
				campaignID: "camp-b5", dropID: "drop-b5", window: windowEarly,
			},
			candidate:  models.RewardIdentity{GameID: "game-b5-b", BenefitID: "ben-b5", Window: windowOverlapsEarly},
			wantSkip:   false,
			wantReason: "benefit_window_undecidable",
		},
		{
			// B6: the ledger row already carries a DIFFERENT, older minted
			// instance under the same benefit ID. The candidate's brand-new
			// instance is unrecorded, so decide() enters tier 1's "no record
			// of this exact instance" branch; the benefit-backed guard there
			// must return FARM ("new_minted_instance") and terminate BEFORE
			// tier 3 ever runs, even though the windows overlap and tier 3
			// would otherwise SKIP. The candidate has no CampaignID/DropID, so
			// the sibling composite-backed guard in the same tier is
			// structurally unreachable here -- only the benefit-backed guard
			// can produce this result.
			name: "B6_different_minted_instance_same_benefit_is_farm_terminal",
			ledgerEvidence: skipEvidence{
				class: evidenceClaimAccepted, gameID: "game-b6", benefitID: "ben-b6",
				campaignID: "camp-b6-old", dropID: "drop-b6-old", instanceID: "inst-b6-old", window: windowEarly,
			},
			candidate: models.RewardIdentity{
				GameID: "game-b6", BenefitID: "ben-b6", InstanceID: "inst-b6-new", Window: windowOverlapsEarly,
			},
			wantSkip:   false,
			wantReason: "new_minted_instance",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ledger := newTestSkipLedger(t, uniqueAccountKey(t))
			ctx := context.Background()
			must(t, ledger.Observe(ctx, tt.ledgerEvidence))

			snap, err := ledger.Snapshot(ctx)
			if err != nil {
				t.Fatalf("snapshot: %v", err)
			}
			skip, reason := snap.decide(tt.candidate, false)
			if skip != tt.wantSkip {
				t.Fatalf("decide() = skip=%v reason=%q, want skip=%v", skip, reason, tt.wantSkip)
			}
			if reason != tt.wantReason {
				t.Fatalf("decide() reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// B7: Reconcile's SH2 rule releases an ACTIVE row once a freshly-observed
// candidate's entitlement window is provably disjoint from it -- a genuinely
// new occurrence of the same campaign+drop composite, regardless of instance
// IDs (both sides are instance-less here, so SH1 can never fire first).
// ---------------------------------------------------------------------------

func TestReconcileSH2DisjointOccurrenceReleasesRow(t *testing.T) {
	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	ctx := context.Background()

	earlyWindow := models.EntitlementWindow{
		Start: time.Date(2031, 4, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2031, 4, 1, 6, 0, 0, 0, time.UTC),
		Source: models.WindowSourceCampaign, Known: true,
	}
	laterDisjointWindow := models.EntitlementWindow{
		Start: time.Date(2031, 4, 2, 0, 0, 0, 0, time.UTC), End: time.Date(2031, 4, 2, 6, 0, 0, 0, time.UTC),
		Source: models.WindowSourceCampaign, Known: true,
	}

	must(t, ledger.Observe(ctx, skipEvidence{
		class: evidenceInventoryClaimed, gameID: "game-b7", campaignID: "camp-b7", dropID: "drop-b7",
		window: earlyWindow,
	}))

	// Precondition: before Reconcile runs, the SAME occurrence still SKIPs --
	// establishes the "before" state this test's transition is measured
	// against.
	preSnap, err := ledger.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	sameOccurrence := models.RewardIdentity{GameID: "game-b7", CampaignID: "camp-b7", DropID: "drop-b7", Window: earlyWindow}
	if skip, reason := preSnap.decide(sameOccurrence, false); !skip {
		t.Fatalf("precondition failed: expected the row to still SKIP the same occurrence before Reconcile, got FARM (%s)", reason)
	}

	campaign := &models.Campaign{
		ID: "camp-b7", Game: &models.Game{ID: "game-b7"},
		StartAt: laterDisjointWindow.Start, EndAt: laterDisjointWindow.End,
		Drops: []*models.Drop{{ID: "drop-b7", Name: "Reward", MinutesRequired: 60}},
	}
	must(t, ledger.Reconcile(ctx, []*models.Campaign{campaign}))

	row := skipRowByComposite(t, ledger, "camp-b7", "drop-b7", "")
	if row.state != skipStateReleased {
		t.Fatalf("expected SH2 to release the row for a provably disjoint occurrence, got state %q", row.state)
	}

	var reason string
	if err := ledger.db.QueryRow(
		`SELECT state_reason FROM drop_reward_skips WHERE account_key = ? AND campaign_id = ? AND drop_id = ? AND instance_id = ''`,
		ledger.accountKey, "camp-b7", "drop-b7",
	).Scan(&reason); err != nil {
		t.Fatalf("read state_reason: %v", err)
	}
	if reason != "disjoint_occurrence" {
		t.Fatalf("state_reason = %q, want %q", reason, "disjoint_occurrence")
	}

	// And the practical consequence: the later occurrence itself is now
	// decidable as farmable, since a released row is excluded from every
	// decide() tier.
	postSnap, err := ledger.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	laterOccurrence := models.RewardIdentity{GameID: "game-b7", CampaignID: "camp-b7", DropID: "drop-b7", Window: laterDisjointWindow}
	if skip, reason := postSnap.decide(laterOccurrence, false); skip {
		t.Fatalf("decide() = SKIP (%s), want FARM: the later disjoint occurrence must remain farmable after SH2", reason)
	}
}
