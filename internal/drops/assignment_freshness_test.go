package drops

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// F-D2: a lightweight progress sync that publishes no changed tracked state
// must still let the existing assignment owner reconsider broker-facing Drops
// assignments, because assignment inputs (independent channel availability,
// the UNKNOWN continuity grace, and the evaluator clock against campaign and
// reward windows) change without watched minutes changing. Every test here
// drives the production syncProgress entry point; updateStreamerCampaigns is
// only used to establish the starting assignment, exactly as the full sync
// would.
//
// Fixtures are anchored at the wall clock (like the original audit test):
// the light-sync merge's ClearClaimedDrops judges drop windows against real
// time, so a fixed historical date would turn an "unchanged" response into a
// changed (LS1) publication. The injectable evaluator clock is then advanced
// to expire a window or the UNKNOWN grace without any watched-minute change.

// countingInventoryClient counts calls at the existing Inventory seam
// (twitchClient.PostGQL) and otherwise answers exactly like fakeDropsClient.
type countingInventoryClient struct {
	*fakeDropsClient
	mu             sync.Mutex
	inventoryCalls int
}

func (c *countingInventoryClient) PostGQL(op constants.GQLOperation) (map[string]interface{}, error) {
	if op.OperationName == constants.Inventory.OperationName {
		c.mu.Lock()
		c.inventoryCalls++
		c.mu.Unlock()
	}
	return c.fakeDropsClient.PostGQL(op)
}

func (c *countingInventoryClient) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inventoryCalls
}

// lightSyncResponse is one Inventory outcome a no-change light sync can see.
// row names the contract outcome class: LS2 (accepted, usable exact
// authority), LS3 (admitted, authority error) or LS4 (acquisition failure).
type lightSyncResponse struct {
	name      string
	row       string
	inventory map[string]interface{}
	err       error
	// reportsTrackedDrop marks a response carrying camp-1/d1 progress. Real
	// reward-window expiry is then handled by the historical LS1 path (the
	// merge strips the out-of-window drop and republishes), so the reward-window
	// case is exercised only by responses with no relevant progress entry.
	reportsTrackedDrop bool
}

func unchangedCamp1Inventory() map[string]interface{} {
	return inventoryWithInProgress(map[string]interface{}{
		"id": "camp-1", "timeBasedDrops": []interface{}{invEntry("d1", 60, 10, nil)},
	})
}

func inventoryWithRawList(list interface{}, present bool) map[string]interface{} {
	inv := map[string]interface{}{}
	if present {
		inv["dropCampaignsInProgress"] = list
	}
	return map[string]interface{}{
		"data": map[string]interface{}{
			"currentUser": map[string]interface{}{"inventory": inv},
		},
	}
}

func freshnessResponses() []lightSyncResponse {
	partial := unchangedCamp1Inventory()
	partial["errors"] = []interface{}{map[string]interface{}{"message": "partial failure"}}
	return []lightSyncResponse{
		{name: "unchanged", row: "LS2", inventory: unchangedCamp1Inventory(), reportsTrackedDrop: true},
		{name: "valid-empty-array", row: "LS2", inventory: inventoryWithInProgress()},
		{name: "tracked-campaign-absent", row: "LS2", inventory: inventoryWithInProgress(map[string]interface{}{
			"id": "camp-other", "timeBasedDrops": []interface{}{invEntry("dx", 60, 5, nil)},
		})},
		{name: "list-missing", row: "LS3", inventory: inventoryWithRawList(nil, false)},
		{name: "list-null", row: "LS3", inventory: inventoryWithRawList(nil, true)},
		{name: "list-wrong-type", row: "LS3", inventory: inventoryWithRawList(map[string]interface{}{"id": "camp-1"}, true)},
		{name: "malformed-entry", row: "LS3", inventory: inventoryWithRawList([]interface{}{"not-an-object"}, true)},
		{name: "malformed-drops-list", row: "LS3", inventory: inventoryWithRawList([]interface{}{
			map[string]interface{}{"id": "camp-1", "timeBasedDrops": "not-a-list"},
		}, true)},
		{name: "partial-graphql-errors", row: "LS3", inventory: partial, reportsTrackedDrop: true},
		{name: "transport-error", row: "LS4", err: errors.New("inventory transport failure")},
		{name: "nil-response", row: "LS4"},
		{name: "graphql-errors-without-data", row: "LS4", inventory: map[string]interface{}{
			"errors": []interface{}{map[string]interface{}{"message": "service unavailable"}},
		}},
	}
}

func clientFor(resp lightSyncResponse) *countingInventoryClient {
	return &countingInventoryClient{fakeDropsClient: &fakeDropsClient{inventory: resp.inventory, inventoryErr: resp.err}}
}

// assertLightSyncBookkeeping proves the historical observation bookkeeping of
// each outcome class is preserved: an admitted response is a successful
// acquisition (ProgressLastError empty), only LS2 carries exact authority, and
// an acquisition failure is recorded as a failed attempt.
func assertLightSyncBookkeeping(t *testing.T, d *DropsTracker, row string, wantRuns int) {
	t.Helper()
	st := d.SyncStatus()
	obs := d.ProgressObservation("camp-1", "d1")
	if st.ProgressRuns != wantRuns {
		t.Errorf("%s: ProgressRuns=%d, want %d", row, st.ProgressRuns, wantRuns)
	}
	switch row {
	case "LS2":
		if st.ProgressLastError != "" || obs.Error != "" || !obs.Complete {
			t.Errorf("LS2 bookkeeping changed: lastErr=%q obsErr=%q complete=%v", st.ProgressLastError, obs.Error, obs.Complete)
		}
	case "LS3":
		if st.ProgressLastError != "" || obs.Error == "" || obs.Complete {
			t.Errorf("LS3 bookkeeping changed: lastErr=%q obsErr=%q complete=%v", st.ProgressLastError, obs.Error, obs.Complete)
		}
	case "LS4":
		if st.ProgressLastError == "" || obs.Error == "" || obs.Complete || obs.Found || obs.AuthoritativeAbsent {
			t.Errorf("LS4 bookkeeping changed: lastErr=%q obs=%+v", st.ProgressLastError, obs)
		}
	default:
		t.Fatalf("unknown outcome row %q", row)
	}
}

// freshnessInvalidation makes a previously valid assignment invalid without
// any watched-minute change.
type freshnessInvalidation struct {
	name    string
	prepare func(c *models.Campaign, now time.Time)
	apply   func(s *models.Streamer, now *time.Time)
}

func freshnessInvalidations() []freshnessInvalidation {
	return []freshnessInvalidation{
		{name: "known-empty", apply: func(s *models.Streamer, now *time.Time) {
			setAvailability(s, true, nil, *now)
		}},
		{name: "known-no", apply: func(s *models.Streamer, now *time.Time) {
			setAvailability(s, true, []string{"camp-other"}, *now)
		}},
		{name: "unknown-grace-expired", apply: func(s *models.Streamer, now *time.Time) {
			setAvailability(s, false, nil, *now)
			*now = now.Add(models.CampaignAvailabilityGrace)
		}},
		{name: "campaign-only-deadline", prepare: func(c *models.Campaign, now time.Time) {
			c.EndAt = now.Add(time.Hour)
			c.Drops[0].StartAt, c.Drops[0].EndAt = time.Time{}, time.Time{}
		}, apply: func(_ *models.Streamer, now *time.Time) {
			*now = now.Add(2 * time.Hour)
		}},
		{name: "reward-window-expired", prepare: func(c *models.Campaign, now time.Time) {
			c.Drops[0].EndAt = now.Add(time.Hour)
		}, apply: func(_ *models.Streamer, now *time.Time) {
			*now = now.Add(2 * time.Hour)
		}},
	}
}

func TestFreshnessFixtureShapes(t *testing.T) {
	inv := inventoryFromGQLResponse(inventoryWithInProgress())
	list, ok := inv["dropCampaignsInProgress"].([]interface{})
	if !ok || list == nil || len(list) != 0 {
		t.Fatalf("valid-empty fixture must be an explicit non-nil empty array, got %#v (ok=%v)", inv["dropCampaignsInProgress"], ok)
	}
	if _, present := inventoryFromGQLResponse(inventoryWithRawList(nil, false))["dropCampaignsInProgress"]; present {
		t.Fatal("list-missing fixture must omit dropCampaignsInProgress")
	}
	raw, present := inventoryFromGQLResponse(inventoryWithRawList(nil, true))["dropCampaignsInProgress"]
	if !present || raw != nil {
		t.Fatalf("list-null fixture must carry an explicit null, got %#v present=%v", raw, present)
	}
	if inventoryFromGQLResponse(nil) != nil {
		t.Fatal("nil-response fixture must not decode to an inventory")
	}
}

// (a), (b), (c): every applicable no-change light-sync outcome removes an
// assignment invalidated by availability, the UNKNOWN grace or a deadline.
func TestFreshnessNoChangeLightSyncRemovesInvalidatedAssignment(t *testing.T) {
	for _, resp := range freshnessResponses() {
		for _, inv := range freshnessInvalidations() {
			if resp.reportsTrackedDrop && inv.name == "reward-window-expired" {
				continue
			}
			t.Run(resp.name+"/"+inv.name, func(t *testing.T) {
				now := time.Now()
				d, s, c := continuitySetup(&now)
				if inv.prepare != nil {
					inv.prepare(c, now)
				}
				client := clientFor(resp)
				d.client = client
				setAvailability(s, true, []string{"camp-1"}, now)
				d.updateStreamerCampaigns()
				if !s.HasEligibleAssignedDropCampaign() {
					t.Fatal("precondition: initial eligible assignment missing")
				}
				revision := d.Revision()

				inv.apply(s, &now)
				d.syncProgress()

				if got := client.calls(); got != 1 {
					t.Fatalf("Inventory seam calls=%d, want exactly 1", got)
				}
				assertLightSyncBookkeeping(t, d, resp.row, 1)
				if d.Revision() != revision {
					t.Fatalf("no-change light sync must not bump Revision: %d -> %d", revision, d.Revision())
				}
				if cs := d.Campaigns(); len(cs) != 1 || cs[0] != c {
					t.Fatalf("tracked pool must stay the same published object, got %v", cs)
				}
				if s.HasEligibleAssignedDropCampaign() || len(s.Stream.GetCampaigns()) != 0 {
					t.Fatalf("%s no-change light sync kept an invalidated Drops assignment", resp.row)
				}
			})
		}
	}
}

// Independent Known+Yes may reinstate an already-tracked campaign through
// every applicable outcome (NORMAL_EXISTING_RULES), without the Inventory
// result supplying any new campaign or progress authority.
func TestFreshnessKnownYesReinstatesTrackedCampaign(t *testing.T) {
	for _, resp := range freshnessResponses() {
		t.Run(resp.name, func(t *testing.T) {
			now := time.Now()
			d, s, c := continuitySetup(&now)
			d.client = clientFor(resp)
			setAvailability(s, true, []string{"camp-1"}, now)
			d.updateStreamerCampaigns()
			setAvailability(s, false, nil, now)
			now = now.Add(models.CampaignAvailabilityGrace)
			d.updateStreamerCampaigns()
			if hasAssignment(s) {
				t.Fatal("precondition: the grace-expired assignment should have been released")
			}
			revision := d.Revision()
			broker := d.BrokerCampaignSnapshot()

			setAvailability(s, true, []string{"camp-1", "camp-other"}, now)
			d.syncProgress()

			assertLightSyncBookkeeping(t, d, resp.row, 1)
			// The broker view itself did not change (the campaign stayed tracked
			// and unfiltered), so the recovery is a pure re-point: no new broker
			// envelope that discovery could mistake for fresh Inventory authority.
			if after := d.BrokerCampaignSnapshot(); after.Generation != broker.Generation ||
				after.SourceRevision != after.CurrentRevision || len(after.Campaigns) != 1 || after.Campaigns[0] != broker.Campaigns[0] {
				t.Fatalf("recovery must not republish an unchanged broker view: before=%+v after=%+v", broker, after)
			}
			if obs := d.ProgressObservation("camp-1", "d1"); resp.row != "LS2" && (obs.Found || obs.Complete) {
				t.Fatalf("%s recovery must not invent exact progress authority, got %+v", resp.row, obs)
			}
			if d.Revision() != revision {
				t.Fatalf("recovery must not bump Revision: %d -> %d", revision, d.Revision())
			}
			if cs := d.Campaigns(); len(cs) != 1 || cs[0] != c {
				t.Fatalf("recovery must not discover or rebuild campaigns, got %v", cs)
			}
			got := s.Stream.GetCampaigns()
			if len(got) != 1 || got[0].ID != "camp-1" {
				t.Fatalf("%s: Known+Yes must reinstate the already-tracked campaign, got %v", resp.row, assignedIDs(s))
			}
		})
	}
}

// Healthy assignments are retained by every outcome, with no observable
// republication: element pointers, broker Generation and the tracked
// revision tuple all stay exactly as published.
func TestFreshnessHealthyAssignmentRetainedWithoutChurn(t *testing.T) {
	for _, resp := range freshnessResponses() {
		t.Run(resp.name, func(t *testing.T) {
			now := time.Now()
			d, s, c := continuitySetup(&now)
			d.client = clientFor(resp)
			setAvailability(s, true, []string{"camp-1"}, now)
			d.updateStreamerCampaigns()
			before := d.BrokerCampaignSnapshot()
			assigned := s.Stream.GetCampaigns()
			st := d.SyncStatus()
			if len(assigned) != 1 || len(before.Campaigns) != 1 {
				t.Fatal("precondition: initial assignment missing")
			}

			for pass := 0; pass < 3; pass++ {
				now = now.Add(time.Minute)
				d.syncProgress()
			}

			after := d.BrokerCampaignSnapshot()
			if after.Generation != before.Generation || after.SourceRevision != before.SourceRevision ||
				len(after.Campaigns) != 1 || after.Campaigns[0] != before.Campaigns[0] {
				t.Fatalf("broker view churned: before=%+v after=%+v", before, after)
			}
			got := s.Stream.GetCampaigns()
			if len(got) != 1 || got[0] != assigned[0] {
				t.Fatalf("streamer assignment pointer churned or lost: before=%p after=%v", assigned[0], got)
			}
			now2 := d.SyncStatus()
			if now2.Revision != st.Revision || now2.UpdateSource != st.UpdateSource || !now2.BackendUpdatedAt.Equal(st.BackendUpdatedAt) {
				t.Fatalf("tracked revision tuple churned: before=%v/%q/%v after=%v/%q/%v",
					st.Revision, st.UpdateSource, st.BackendUpdatedAt, now2.Revision, now2.UpdateSource, now2.BackendUpdatedAt)
			}
			if cs := d.Campaigns(); len(cs) != 1 || cs[0] != c {
				t.Fatalf("tracked pool element identity changed, got %v", cs)
			}
		})
	}
}

// UNKNOWN within the grace retains, and UNKNOWN never creates, through every
// outcome; repeated UNKNOWN does not extend the grace.
func TestFreshnessUnknownContinuityThroughNoChangeSync(t *testing.T) {
	for _, resp := range freshnessResponses() {
		t.Run(resp.name+"/within-grace-retains", func(t *testing.T) {
			now := time.Now()
			d, s, _ := continuitySetup(&now)
			d.client = clientFor(resp)
			setAvailability(s, true, []string{"camp-1"}, now)
			d.updateStreamerCampaigns()
			setAvailability(s, false, nil, now)
			now = now.Add(models.CampaignAvailabilityGrace - time.Second)
			d.syncProgress()
			if !s.HasEligibleAssignedDropCampaign() {
				t.Fatal("UNKNOWN within the grace must retain the eligible assignment")
			}
		})
		t.Run(resp.name+"/never-creates", func(t *testing.T) {
			now := time.Now()
			d, s, _ := continuitySetup(&now)
			d.client = clientFor(resp)
			setAvailability(s, true, []string{"camp-1"}, now)
			setAvailability(s, false, nil, now)
			d.syncProgress()
			if hasAssignment(s) {
				t.Fatal("UNKNOWN availability must never create a new assignment")
			}
		})
		t.Run(resp.name+"/repeated-unknown-does-not-extend", func(t *testing.T) {
			start := time.Now()
			now := start
			d, s, _ := continuitySetup(&now)
			d.client = clientFor(resp)
			setAvailability(s, true, []string{"camp-1"}, now)
			d.updateStreamerCampaigns()
			setAvailability(s, false, nil, now)
			now = now.Add(models.CampaignAvailabilityGrace / 2)
			setAvailability(s, false, nil, now)
			d.syncProgress()
			if !hasAssignment(s) {
				t.Fatal("precondition: still within the grace measured from the FIRST Unknown")
			}
			now = start.Add(models.CampaignAvailabilityGrace)
			d.syncProgress()
			if hasAssignment(s) {
				t.Fatal("grace is measured from the first Unknown; a repeated Unknown must not extend it")
			}
		})
	}
}

// An offline or unknown-liveness streamer keeps its assignment (BKM-002
// continuity); only a confirmed-online streamer is re-evaluated.
func TestFreshnessOfflineOrUnknownLivenessRetains(t *testing.T) {
	for _, mode := range []string{"offline", "unknown-liveness"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now()
			d, s, _ := continuitySetup(&now)
			d.client = clientFor(freshnessResponses()[0])
			setAvailability(s, true, []string{"camp-1"}, now)
			d.updateStreamerCampaigns()
			assigned := s.Stream.GetCampaigns()
			setAvailability(s, true, nil, now)
			if mode == "offline" {
				s.SetConfirmedOffline()
			} else {
				s.SetUnknown(models.ReasonTransportError)
			}
			d.syncProgress()
			if got := s.Stream.GetCampaigns(); len(got) != 1 || got[0] != assigned[0] {
				t.Fatalf("%s streamer must retain its assignment untouched, got %v", mode, assignedIDs(s))
			}
		})
	}
}

// LS0: no tracked campaigns means no Inventory call and no assignment work.
func TestFreshnessNoTrackedCampaignsDoesNothing(t *testing.T) {
	now := time.Now()
	d, s, c := continuitySetup(&now)
	client := clientFor(freshnessResponses()[0])
	d.client = client
	setAvailability(s, true, []string{"camp-1"}, now)
	d.updateStreamerCampaigns()
	before := d.BrokerCampaignSnapshot()
	d.mu.Lock()
	d.campaigns = nil
	d.mu.Unlock()
	setAvailability(s, true, nil, now)

	d.syncProgress()

	if client.calls() != 0 {
		t.Fatalf("LS0 must not call Inventory, got %d calls", client.calls())
	}
	if got := s.Stream.GetCampaigns(); len(got) != 1 || got[0] != c {
		t.Fatalf("LS0 must not touch assignments, got %v", assignedIDs(s))
	}
	if after := d.BrokerCampaignSnapshot(); after.Generation != before.Generation {
		t.Fatalf("LS0 must not publish broker views: %d -> %d", before.Generation, after.Generation)
	}
	if st := d.SyncStatus(); st.ProgressRuns != 0 {
		t.Fatalf("LS0 must not record an observation, got %d runs", st.ProgressRuns)
	}
}

// The light sync never discovers a campaign: an untracked in-progress entry
// and channel availability naming it do not create an assignment for it.
func TestFreshnessNeverDiscoversUntrackedCampaign(t *testing.T) {
	now := time.Now()
	d, s, c := continuitySetup(&now)
	d.client = clientFor(freshnessResponses()[2]) // reports only camp-other
	setAvailability(s, true, []string{"camp-1", "camp-other"}, now)
	d.updateStreamerCampaigns()

	d.syncProgress()

	if cs := d.Campaigns(); len(cs) != 1 || cs[0] != c {
		t.Fatalf("light sync must not add campaigns, got %v", cs)
	}
	if ids := assignedIDs(s); len(ids) != 1 || ids[0] != "camp-1" {
		t.Fatalf("only the tracked campaign may be assigned, got %v", ids)
	}
}

// LS4 when a newer pool was published while the failed request was in
// flight: the failure stays a failure (historical bookkeeping, which stamps
// the then-current revision without the successful-observation check), and
// the assignment re-evaluation uses its own fresh view of the CURRENT pool.
func TestFreshnessFailedReadReevaluatesNewerPool(t *testing.T) {
	now := time.Now()
	d, s, _ := continuitySetup(&now)
	client := &fakeDropsClient{}
	d.client = client
	setAvailability(s, true, []string{"camp-1", "camp-2"}, now)
	d.updateStreamerCampaigns()

	gate := client.armInventoryGate(nil, errors.New("inventory transport failure"))
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.syncProgress()
	}()
	<-gate.entered

	// A full sync publishes a newer pool (camp-1 ended, camp-2 discovered)
	// and re-points the streamer while the light sync's request is blocked.
	camp2 := &models.Campaign{
		ID: "camp-2", Name: "C2", Game: &models.Game{ID: "g1"}, ACL: unrestrictedACL(),
		StartAt: now.Add(-time.Hour), EndAt: now.Add(100 * time.Hour), ClaimStatus: models.CampaignClaimStatusInProgress,
		Drops: []*models.Drop{{ID: "d2", Name: "R2", MinutesRequired: 60, CurrentMinutesWatched: 5,
			StartAt: now.Add(-time.Hour), EndAt: now.Add(100 * time.Hour)}},
	}
	d.mu.Lock()
	d.campaigns = []*models.Campaign{camp2}
	d.bumpRevisionLocked(updateSourceFullSync)
	d.mu.Unlock()
	d.updateStreamerCampaigns()
	if ids := assignedIDs(s); len(ids) != 1 || ids[0] != "camp-2" {
		t.Fatalf("precondition: full sync should have assigned camp-2, got %v", ids)
	}
	newRevision := d.Revision()
	// Channel availability then becomes authoritatively empty.
	setAvailability(s, true, nil, now)

	close(gate.release)
	<-done

	st := d.SyncStatus()
	if st.ProgressLastError == "" {
		t.Fatal("the failed read must still be recorded as a failed attempt")
	}
	obs := d.ProgressObservation("camp-2", "d2")
	if obs.Error == "" || obs.Revision != newRevision || obs.Complete || obs.Found {
		t.Fatalf("failure bookkeeping must stay historical (no exact authority over the new pool), got %+v", obs)
	}
	if d.Revision() != newRevision {
		t.Fatalf("a failed read must not publish: revision %d -> %d", newRevision, d.Revision())
	}
	if hasAssignment(s) {
		t.Fatalf("the failure-path re-evaluation must judge the CURRENT pool against current availability, got %v", assignedIDs(s))
	}
}

// openFreshnessLedger opens a private single-connection SQLite ledger so a
// test can make its Snapshot read block or fail without touching the shared
// package-wide database.
func openFreshnessLedger(t *testing.T) (*SkipLedger, *database.DB) {
	t.Helper()
	db := openPrivateSkipLedgerDB(t, filepath.Join(t.TempDir(), "freshness-ledger.db"))
	t.Cleanup(func() { _ = db.Close() })
	ledger, err := NewSkipLedger(db, uniqueAccountKey(t))
	if err != nil {
		t.Fatalf("new skip ledger: %v", err)
	}
	return ledger, db
}

// holdLedgerConnection takes the private ledger database's only connection,
// so a Snapshot read blocks until the returned release is called.
func holdLedgerConnection(t *testing.T, db *database.DB) func() {
	t.Helper()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("hold ledger connection: %v", err)
	}
	return func() { _ = conn.Close() }
}

// HOLD_NEW_PASS: an unusable WIRED ledger snapshot abandons the new pass
// entirely (no broker publication, no assignment change, no Generation
// advance) while ordinary observation bookkeeping stays independent; a later
// pass with a usable snapshot completes the freshness repair.
func TestFreshnessUnusableLedgerHoldsNewPassThenRecovers(t *testing.T) {
	prevTimeout := skipLedgerOpTimeout
	skipLedgerOpTimeout = 200 * time.Millisecond
	t.Cleanup(func() { skipLedgerOpTimeout = prevTimeout })

	now := time.Now()
	d, s, _ := continuitySetup(&now)
	ledger, db := openFreshnessLedger(t)
	d.skipLedger = ledger
	d.client = clientFor(freshnessResponses()[0])
	setAvailability(s, true, []string{"camp-1"}, now)
	d.updateStreamerCampaigns()
	before := d.BrokerCampaignSnapshot()
	assigned := s.Stream.GetCampaigns()
	if len(assigned) != 1 || len(before.Campaigns) != 1 {
		t.Fatal("precondition: initial assignment missing")
	}

	setAvailability(s, true, nil, now)
	release := holdLedgerConnection(t, db)
	d.syncProgress() // ledger read times out -> HOLD
	release()

	after := d.BrokerCampaignSnapshot()
	if after.Generation != before.Generation || len(after.Campaigns) != 1 || after.Campaigns[0] != before.Campaigns[0] {
		t.Fatalf("an unusable wired ledger must not publish broker views: before=%+v after=%+v", before, after)
	}
	if got := s.Stream.GetCampaigns(); len(got) != 1 || got[0] != assigned[0] {
		t.Fatalf("an unusable wired ledger must keep the previous assignment untouched, got %v", assignedIDs(s))
	}
	assertLightSyncBookkeeping(t, d, "LS2", 1)

	d.syncProgress() // usable snapshot -> the deferred freshness repair completes
	if hasAssignment(s) {
		t.Fatalf("a later pass with a usable ledger snapshot must apply the deferred removal, got %v", assignedIDs(s))
	}
	assertLightSyncBookkeeping(t, d, "LS2", 2)
}

// LS1 keeps its historical fail-open handling of a failed ledger read while
// the new no-change pass holds on the same failure.
func TestFreshnessChangedPublicationKeepsLedgerFailOpen(t *testing.T) {
	now := time.Now()
	d, s, _ := continuitySetup(&now)
	ledger, db := openFreshnessLedger(t)
	must(t, ledger.Observe(context.Background(), skipEvidence{
		class: evidenceClaimAccepted, gameID: "g1", campaignID: "camp-1", dropID: "d1",
	}))
	d.skipLedger = ledger
	setAvailability(s, true, []string{"camp-1"}, now)
	d.updateStreamerCampaigns()
	if hasAssignment(s) {
		t.Fatal("precondition: the ledger row should suppress camp-1 while the snapshot loads")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close ledger db: %v", err)
	}

	d.client = clientFor(lightSyncResponse{inventory: inventoryWithInProgress(map[string]interface{}{
		"id": "camp-1", "timeBasedDrops": []interface{}{invEntry("d1", 60, 20, nil)},
	})})
	d.syncProgress() // LS1: changed progress -> historical re-point fails open
	if !hasAssignment(s) {
		t.Fatal("LS1 must keep its historical fail-open re-point on a failed ledger read")
	}
	generation := d.BrokerCampaignSnapshot().Generation
	assigned := s.Stream.GetCampaigns()

	setAvailability(s, true, nil, now)
	d.syncProgress() // LS2 (minutes unchanged at 20) with the same failed ledger -> HOLD
	if got := s.Stream.GetCampaigns(); len(got) != 1 || got[0] != assigned[0] {
		t.Fatalf("the new no-change pass must hold on a failed wired ledger, got %v", assignedIDs(s))
	}
	if g := d.BrokerCampaignSnapshot().Generation; g != generation {
		t.Fatalf("held pass must not advance Generation: %d -> %d", generation, g)
	}
}

// LS5: a successful observation rejected by the captured-revision guard runs
// no assignment pass at all and records no observation. The wired-ledger case
// matters because the refresh would read the ledger before its own revision
// fence: entering it after a rejection would cost a SQLite read and, with a
// failing ledger, misreport the stale rejection as a held pass.
func TestFreshnessStaleObservationRunsNoAssignmentPass(t *testing.T) {
	responses := []struct {
		name      string
		inventory map[string]interface{}
	}{
		{"unchanged", unchangedCamp1Inventory()},       // unchanged-progress call site
		{"valid-empty", inventoryWithInProgress()},     // empty-list call site
		{"list-null", inventoryWithRawList(nil, true)}, // empty-list call site, authority error
	}
	for _, resp := range responses {
		for _, mode := range []string{"no-ledger", "failed-wired-ledger"} {
			t.Run(resp.name+"/"+mode, func(t *testing.T) {
				now := time.Now()
				d, s, _ := continuitySetup(&now)
				client := &fakeDropsClient{}
				d.client = client
				var db *database.DB
				if mode == "failed-wired-ledger" {
					var ledger *SkipLedger
					ledger, db = openFreshnessLedger(t)
					d.skipLedger = ledger
				}
				setAvailability(s, true, []string{"camp-1"}, now)
				d.updateStreamerCampaigns()
				generation := d.BrokerCampaignSnapshot().Generation
				if db != nil {
					if err := db.Close(); err != nil {
						t.Fatalf("close ledger db: %v", err)
					}
				}
				h := installPauseOnMessage(t, "")

				gate := client.armInventoryGate(resp.inventory, nil)
				done := make(chan struct{})
				go func() {
					defer close(done)
					d.syncProgress()
				}()
				select {
				case <-gate.entered:
				case <-time.After(5 * time.Second):
					t.Fatal("the light sync never requested Inventory")
				}
				// A newer publication lands (its own re-point not yet run) and the
				// channel becomes authoritatively empty.
				d.mu.Lock()
				d.campaigns = append([]*models.Campaign(nil), d.campaigns...)
				d.bumpRevisionLocked(updateSourceFullSync)
				d.mu.Unlock()
				setAvailability(s, true, nil, now)
				close(gate.release)
				<-done

				if st := d.SyncStatus(); st.ProgressRuns != 0 || !st.ProgressLastSyncAt.IsZero() {
					t.Fatalf("a rejected stale observation must not be recorded, got runs=%d at=%v", st.ProgressRuns, st.ProgressLastSyncAt)
				}
				if g := d.BrokerCampaignSnapshot().Generation; g != generation {
					t.Fatalf("a rejected stale observation must not publish broker views: %d -> %d", generation, g)
				}
				if !hasAssignment(s) {
					t.Fatal("a rejected stale observation must not re-point streamers")
				}
				if h.loggedContaining("Drops assignment refresh") {
					t.Fatalf("a rejected stale observation must not enter the assignment refresh, got %v", h.records)
				}
			})
		}
	}
}

// pauseOnMessage is an slog handler that records every message and parks the
// FIRST record carrying msg until released, so a test can pause a pass at a
// known point of view building without a production hook. An empty msg never
// parks.
type pauseOnMessage struct {
	msg     string
	entered chan struct{}
	release chan struct{}
	once    sync.Once

	mu      sync.Mutex
	records []string
}

func (h *pauseOnMessage) Enabled(context.Context, slog.Level) bool { return true }

func (h *pauseOnMessage) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.records = append(h.records, r.Level.String()+" "+r.Message)
	h.mu.Unlock()
	if h.msg != "" && r.Message == h.msg {
		h.once.Do(func() {
			close(h.entered)
			<-h.release
		})
	}
	return nil
}

func (h *pauseOnMessage) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *pauseOnMessage) WithGroup(string) slog.Handler      { return h }

func (h *pauseOnMessage) loggedContaining(substr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if strings.Contains(r, substr) {
			return true
		}
	}
	return false
}

func (h *pauseOnMessage) logged(prefix string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if strings.HasPrefix(r, prefix) {
			return true
		}
	}
	return false
}

// refreshParkedOnLogMu reports whether some goroutine is inside
// refreshAssignments and blocked acquiring a sync.Mutex past view building,
// i.e. parked on the assignment serialization (logMu).
func refreshParkedOnLogMu() bool {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	for _, g := range strings.Split(string(buf[:n]), "\n\n") {
		if strings.Contains(g, "(*DropsTracker).refreshAssignments") &&
			strings.Contains(g, "sync.(*Mutex).Lock") &&
			!strings.Contains(g, "buildBrokerViews") {
			return true
		}
	}
	return false
}

// waitForPause waits, bounded, until the pass running in the background
// reaches h's pause point. A pass that finishes without reaching it, or never
// reaches it, fails the test instead of hanging the package.
func waitForPause(t *testing.T, h *pauseOnMessage, done <-chan struct{}) {
	t.Helper()
	select {
	case <-h.entered:
	case <-done:
		t.Fatal("the pass finished without reaching the pause point (no refresh view building)")
	case <-time.After(5 * time.Second):
		t.Fatal("the pass never reached the pause point")
	}
}

func installPauseOnMessage(t *testing.T, msg string) *pauseOnMessage {
	t.Helper()
	h := &pauseOnMessage{msg: msg, entered: make(chan struct{}), release: make(chan struct{})}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h
}

const (
	logRefreshNotStarted      = "DEBUG Drops assignment refresh not started: tracker lifecycle cancelled"
	logRefreshCancelledInRead = "DEBUG Drops assignment refresh abandoned: tracker lifecycle cancelled during skip ledger read"
	logRefreshCancelledAdmit  = "DEBUG Drops assignment refresh abandoned: tracker lifecycle cancelled before admission"
	logRefreshLedgerDeferred  = "WARN Drops assignment refresh deferred: skip ledger snapshot unavailable"
	logOperatorSkipWithheld   = "Drop campaign not assigned: reward skipped by operator rule"
)

// addOperatorSkippedCampaign tracks a second campaign whose current reward the
// operator skipped. View building logs its withholding at DEBUG on every
// pass, which pauseOnMessage uses as a deterministic pause point.
func addOperatorSkippedCampaign(d *DropsTracker, now time.Time) *models.Campaign {
	skipped := &models.Campaign{
		ID: "camp-skip", Name: "CS", Game: &models.Game{ID: "g1"}, ACL: unrestrictedACL(),
		StartAt: now.Add(-time.Hour), EndAt: now.Add(100 * time.Hour), ClaimStatus: models.CampaignClaimStatusInProgress,
		Drops: []*models.Drop{{ID: "ds", Name: "S", MinutesRequired: 60, CurrentMinutesWatched: 5,
			StartAt: now.Add(-time.Hour), EndAt: now.Add(100 * time.Hour)}},
	}
	d.campaigns = append(d.campaigns, skipped)
	d.rewardSkips = models.NewRewardSkips([]string{models.NormalizeRewardKey("g1", "S")})
	return skipped
}

func setLifecycle(d *DropsTracker, ctx context.Context) {
	d.mu.Lock()
	d.ctx = ctx
	d.mu.Unlock()
}

// R2 with a real SQLite ledger: views are fresh clones on every pass, yet an
// unchanged result keeps the published objects, Generation and assignment
// pointers, and SetCampaigns is not re-run (no re-stamped broadcast fact).
func TestFreshnessRealLedgerNoChurnAcrossPasses(t *testing.T) {
	now := time.Now()
	d, s, c := continuitySetup(&now)
	d.skipLedger = newTestSkipLedger(t, uniqueAccountKey(t))
	d.client = clientFor(freshnessResponses()[0])
	setAvailability(s, true, []string{"camp-1"}, now)
	d.updateStreamerCampaigns()
	before := d.BrokerCampaignSnapshot()
	assigned := s.Stream.GetCampaigns()
	st := d.SyncStatus()
	if len(before.Campaigns) != 1 || before.Campaigns[0] == c || len(assigned) != 1 || assigned[0] != before.Campaigns[0] {
		t.Fatalf("precondition: a wired ledger must publish a filtered clone and assign it, got %+v / %v", before, assigned)
	}
	s.Stream.BroadcastID = "broadcast-2" // a later broadcast; the unchanged passes must not re-stamp it

	for pass := 0; pass < 3; pass++ {
		now = now.Add(time.Minute)
		d.syncProgress()
		after := d.BrokerCampaignSnapshot()
		if after.Generation != before.Generation || after.SourceRevision != before.SourceRevision ||
			len(after.Campaigns) != 1 || after.Campaigns[0] != before.Campaigns[0] {
			t.Fatalf("pass %d: unchanged ledger-filtered view was republished: before=%+v after=%+v", pass, before, after)
		}
		if got := s.Stream.GetCampaigns(); len(got) != 1 || got[0] != assigned[0] {
			t.Fatalf("pass %d: unchanged assignment was re-set, got %v", pass, got)
		}
	}
	if s.Stream.ProvisionalDropSnapshot().HasConfirmedCampaign("camp-1") {
		t.Fatal("an unchanged pass must not call SetCampaigns (it re-stamped the confirmed-campaign fact)")
	}
	if now2 := d.SyncStatus(); now2.Revision != st.Revision || now2.UpdateSource != st.UpdateSource ||
		!now2.BackendUpdatedAt.Equal(st.BackendUpdatedAt) || now2.ProgressRuns != 3 {
		t.Fatalf("tracked publication tuple churned or observations missing: before=%+v after=%+v", st, now2)
	}
	if cs := d.Campaigns(); len(cs) != 1 || cs[0] != c {
		t.Fatalf("tracked pool element identity changed, got %v", cs)
	}
}

// A usable ledger keeps suppressed work suppressed across unchanged passes:
// the refresh never revives a ghost-skipped campaign.
func TestFreshnessLedgerSuppressedWorkNotRevived(t *testing.T) {
	now := time.Now()
	d, s, _ := continuitySetup(&now)
	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	must(t, ledger.Observe(context.Background(), skipEvidence{
		class: evidenceClaimAccepted, gameID: "g1", campaignID: "camp-1", dropID: "d1",
	}))
	d.skipLedger = ledger
	d.client = clientFor(freshnessResponses()[0])
	setAvailability(s, true, []string{"camp-1"}, now)
	d.updateStreamerCampaigns()
	generation := d.BrokerCampaignSnapshot().Generation
	if hasAssignment(s) {
		t.Fatal("precondition: the ledger row must suppress camp-1")
	}
	for pass := 0; pass < 3; pass++ {
		d.syncProgress()
	}
	if hasAssignment(s) || s.HasEligibleAssignedDropCampaign() {
		t.Fatalf("a usable ledger must keep the ghost suppressed, got %v", assignedIDs(s))
	}
	if g := d.BrokerCampaignSnapshot().Generation; g != generation {
		t.Fatalf("unchanged suppressed result must not republish: %d -> %d", generation, g)
	}
}

// HOLD on a failed (closed) wired ledger versus the nil ledger's fail-open:
// the same invalidation is held with a wired-but-failing ledger and applied
// with no ledger wired.
func TestFreshnessFailedWiredLedgerHoldsNilLedgerApplies(t *testing.T) {
	for _, wired := range []bool{true, false} {
		name := "nil-ledger-applies"
		if wired {
			name = "failed-wired-ledger-holds"
		}
		t.Run(name, func(t *testing.T) {
			now := time.Now()
			d, s, _ := continuitySetup(&now)
			var db *database.DB
			if wired {
				var ledger *SkipLedger
				ledger, db = openFreshnessLedger(t)
				d.skipLedger = ledger
			}
			d.client = clientFor(freshnessResponses()[0])
			setAvailability(s, true, []string{"camp-1"}, now)
			d.updateStreamerCampaigns()
			before := d.BrokerCampaignSnapshot()
			assigned := s.Stream.GetCampaigns()
			if wired {
				if err := db.Close(); err != nil {
					t.Fatalf("close ledger db: %v", err)
				}
			}
			setAvailability(s, true, nil, now)
			h := installPauseOnMessage(t, "")

			d.syncProgress()

			after := d.BrokerCampaignSnapshot()
			if wired {
				if got := s.Stream.GetCampaigns(); len(got) != 1 || got[0] != assigned[0] {
					t.Fatalf("failed wired ledger must hold the previous assignment, got %v", assignedIDs(s))
				}
				if after.Generation != before.Generation || len(after.Campaigns) != 1 || after.Campaigns[0] != before.Campaigns[0] {
					t.Fatalf("held pass must not publish: before=%+v after=%+v", before, after)
				}
				if !h.logged(logRefreshLedgerDeferred) {
					t.Fatal("the held pass must report its deferral distinctly from an Inventory failure")
				}
			} else if hasAssignment(s) {
				t.Fatalf("a nil (not wired) ledger is not a failed read; the removal must apply, got %v", assignedIDs(s))
			}
			assertLightSyncBookkeeping(t, d, "LS2", 1)
		})
	}
}

// cancellationFixture prepares an assigned camp-1 (plus an operator-skipped
// camp-skip whose withholding is logged during view building) and then two
// pending changes a live refresh would apply: authoritative Known-empty
// availability (clears the assignment) and a new operator Skip on camp-1's
// current reward (removes camp-1 from the broker views, so the refresh would
// publish Generation+1). Cancellation tests prove neither happens;
// TestFreshnessCancellationFixturePublishesWhenLive is the live control.
func cancellationFixture(t *testing.T, withLedger bool) (*DropsTracker, *models.Streamer, *database.DB, uint64) {
	t.Helper()
	now := time.Now()
	d, s, _ := continuitySetup(&now)
	addOperatorSkippedCampaign(d, now)
	var db *database.DB
	if withLedger {
		var ledger *SkipLedger
		ledger, db = openFreshnessLedger(t)
		d.skipLedger = ledger
	}
	d.client = clientFor(freshnessResponses()[0])
	setAvailability(s, true, []string{"camp-1"}, now)
	d.updateStreamerCampaigns()
	if !hasAssignment(s) {
		t.Fatal("precondition: initial assignment missing")
	}
	generation := d.BrokerCampaignSnapshot().Generation
	setAvailability(s, true, nil, now)
	d.UpdateRewardSkips(models.NewRewardSkips([]string{
		models.NormalizeRewardKey("g1", "S"), models.NormalizeRewardKey("g1", "R"),
	}))
	return d, s, db, generation
}

// Live control for the cancellation tests: the same fixture with a live
// lifecycle publishes the pending broker-view change and clears the
// assignment, so their "nothing published" assertions are not vacuous.
func TestFreshnessCancellationFixturePublishesWhenLive(t *testing.T) {
	for _, withLedger := range []bool{false, true} {
		name := "no-ledger"
		if withLedger {
			name = "private-ledger"
		}
		t.Run(name, func(t *testing.T) {
			d, s, _, generation := cancellationFixture(t, withLedger)
			setLifecycle(d, context.Background())

			d.syncProgress()

			after := d.BrokerCampaignSnapshot()
			if after.Generation == generation {
				t.Fatal("control: a live refresh must publish the pending broker-view change")
			}
			for _, c := range after.Campaigns {
				if c.ID == "camp-1" {
					t.Fatal("control: the published views must withhold the newly skipped campaign")
				}
			}
			if hasAssignment(s) {
				t.Fatal("control: a live refresh must clear the assignment")
			}
		})
	}
}

// Cancellation already visible: the refresh starts nothing (no ledger read,
// no publication, no SetCampaigns); the historical observation is unchanged.
func TestFreshnessAlreadyCancelledStartsNothing(t *testing.T) {
	d, s, _, generation := cancellationFixture(t, true)
	client := d.client.(*countingInventoryClient)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	setLifecycle(d, ctx)
	h := installPauseOnMessage(t, "")

	d.syncProgress()

	if !hasAssignment(s) {
		t.Fatal("a cancelled lifecycle must not start the refresh")
	}
	if g := d.BrokerCampaignSnapshot().Generation; g != generation {
		t.Fatalf("a cancelled lifecycle must not publish: %d -> %d", generation, g)
	}
	if !h.logged(logRefreshNotStarted) || h.logged(logRefreshCancelledInRead) || h.logged(logRefreshLedgerDeferred) {
		t.Fatalf("the refresh must stop before any ledger read, got %v", h.records)
	}
	if client.calls() != 1 {
		t.Fatalf("historical Inventory read unchanged, got %d calls", client.calls())
	}
	assertLightSyncBookkeeping(t, d, "LS2", 1)
}

// Cancellation while the pass waits for assignment serialization: the
// cancellation lands only once the refresh is parked on logMu, so only the
// admission re-check UNDER that serialization can observe it, and it must do
// so before any broker publication or SetCampaigns.
func TestFreshnessCancelledWhileWaitingForAdmission(t *testing.T) {
	d, s, _, generation := cancellationFixture(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	setLifecycle(d, ctx)
	h := installPauseOnMessage(t, logOperatorSkipWithheld)

	done := make(chan struct{})
	go func() {
		defer close(done)
		d.syncProgress()
	}()
	waitForPause(t, h, done) // the refresh started and is building its views
	d.logMu.Lock()
	close(h.release)
	deadline := time.Now().Add(5 * time.Second)
	for !refreshParkedOnLogMu() {
		if time.Now().After(deadline) {
			d.logMu.Unlock()
			<-done
			t.Fatal("the refresh never parked on the assignment serialization")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	d.logMu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the pass did not finish after admission was released")
	}

	if !hasAssignment(s) {
		t.Fatal("a pass cancelled before admission must not apply")
	}
	if g := d.BrokerCampaignSnapshot().Generation; g != generation {
		t.Fatalf("a pass cancelled before admission must not publish: %d -> %d", generation, g)
	}
	if !h.logged(logRefreshCancelledAdmit) {
		t.Fatalf("expected the admission-boundary cancellation, got %v", h.records)
	}
}

// Same-revision interleaving with the historical pass: a full-sync re-point
// that landed after this refresh captured its inputs (here applying a new
// operator Skip) is never overwritten by the refresh's older capture.
func TestFreshnessGenerationFenceKeepsNewerSameRevisionPublication(t *testing.T) {
	now := time.Now()
	d, s, _ := continuitySetup(&now)
	client := &fakeDropsClient{inventory: unchangedCamp1Inventory()}
	d.client = client
	setAvailability(s, true, []string{"camp-1"}, now)
	d.updateStreamerCampaigns()

	gate := client.armInventoryGate(unchangedCamp1Inventory(), nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.syncProgress()
	}()
	<-gate.entered
	revision := d.Revision()
	d.UpdateRewardSkips(models.NewRewardSkips([]string{models.NormalizeRewardKey("g1", "R")}))
	d.updateStreamerCampaigns() // same campaign Revision, newer skip input
	if hasAssignment(s) || d.Revision() != revision {
		t.Fatal("precondition: the historical pass should withhold the skipped reward at the same revision")
	}
	generation := d.BrokerCampaignSnapshot().Generation
	close(gate.release)
	<-done

	if hasAssignment(s) {
		t.Fatalf("the refresh overwrote a newer same-revision publication with older inputs, got %v", assignedIDs(s))
	}
	if g := d.BrokerCampaignSnapshot().Generation; g != generation {
		t.Fatalf("the stale refresh must not publish: %d -> %d", generation, g)
	}
}

// Two refreshes at the same campaign Revision: the newer-started pass that
// applied first is not overwritten by the older-started one.
func TestFreshnessConcurrentSameRevisionRefreshesKeepNewest(t *testing.T) {
	now := time.Now()
	d, s, _ := continuitySetup(&now)
	client := &fakeDropsClient{inventory: unchangedCamp1Inventory()}
	d.client = client
	setAvailability(s, true, []string{"camp-1"}, now)
	d.updateStreamerCampaigns()

	gate := client.armInventoryGate(unchangedCamp1Inventory(), nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.syncProgress() // pass A: captured before the Skip rule
	}()
	<-gate.entered
	d.UpdateRewardSkips(models.NewRewardSkips([]string{models.NormalizeRewardKey("g1", "R")}))
	d.syncProgress() // pass B: newer inputs, applies first
	if hasAssignment(s) {
		t.Fatal("precondition: the newer refresh should withhold the skipped reward")
	}
	generation := d.BrokerCampaignSnapshot().Generation
	close(gate.release)
	<-done

	if hasAssignment(s) {
		t.Fatalf("the older same-revision refresh overwrote the newer result, got %v", assignedIDs(s))
	}
	if g := d.BrokerCampaignSnapshot().Generation; g != generation {
		t.Fatalf("the older refresh must not publish: %d -> %d", generation, g)
	}
	if st := d.SyncStatus(); st.ProgressRuns != 2 {
		t.Fatalf("both observations stay recorded, got %d runs", st.ProgressRuns)
	}
}

// The refresh's revision fence: a newer pool published while the refresh was
// building its views (its own re-point not yet run) is never re-pointed from
// the superseded capture.
func TestFreshnessRevisionFenceRejectsSupersededPool(t *testing.T) {
	now := time.Now()
	d, s, _ := continuitySetup(&now)
	skipped := addOperatorSkippedCampaign(d, now)
	d.client = clientFor(lightSyncResponse{err: errors.New("inventory transport failure")})
	setAvailability(s, true, []string{"camp-1"}, now)
	d.updateStreamerCampaigns()
	setAvailability(s, false, nil, now)
	now = now.Add(models.CampaignAvailabilityGrace)
	d.updateStreamerCampaigns()
	if hasAssignment(s) {
		t.Fatal("precondition: the grace-expired assignment should have been released")
	}
	generation := d.BrokerCampaignSnapshot().Generation
	setAvailability(s, true, []string{"camp-1"}, now) // the refresh would reinstate camp-1
	h := installPauseOnMessage(t, logOperatorSkipWithheld)

	done := make(chan struct{})
	go func() {
		defer close(done)
		d.syncProgress() // LS4: fresh capture of the current pool, then pauses
	}()
	waitForPause(t, h, done)
	d.mu.Lock()
	d.campaigns = []*models.Campaign{skipped} // a full sync ended camp-1
	d.bumpRevisionLocked(updateSourceFullSync)
	d.mu.Unlock()
	close(h.release)
	<-done

	if hasAssignment(s) {
		t.Fatalf("the refresh re-pointed from a superseded pool, got %v", assignedIDs(s))
	}
	if g := d.BrokerCampaignSnapshot().Generation; g != generation {
		t.Fatalf("the superseded refresh must not publish: %d -> %d", generation, g)
	}
}

// A legitimate LS1 publication (here a valid lower progress value) keeps its
// historical re-point, and the following unchanged pass does not churn it.
func TestFreshnessChangedPublicationThenNoChurn(t *testing.T) {
	now := time.Now()
	d, s, c := continuitySetup(&now)
	setAvailability(s, true, []string{"camp-1"}, now)
	d.updateStreamerCampaigns()
	generation := d.BrokerCampaignSnapshot().Generation
	lower := inventoryWithInProgress(map[string]interface{}{
		"id": "camp-1", "timeBasedDrops": []interface{}{invEntry("d1", 60, 5, nil)},
	})
	d.client = clientFor(lightSyncResponse{inventory: lower})

	d.syncProgress() // LS1
	published := d.Campaigns()
	if len(published) != 1 || published[0] == c || published[0].Drops[0].CurrentMinutesWatched != 5 {
		t.Fatalf("LS1 must publish the lower progress value, got %v", published)
	}
	after := d.BrokerCampaignSnapshot()
	if after.Generation == generation || after.Campaigns[0] != published[0] {
		t.Fatalf("LS1 must keep its historical re-point and broker publication: before=%d after=%+v", generation, after)
	}
	assigned := s.Stream.GetCampaigns()
	if len(assigned) != 1 || assigned[0] != published[0] {
		t.Fatalf("LS1 must re-point the streamer at the published campaign, got %v", assigned)
	}

	d.syncProgress() // same lower value -> LS2 refresh, nothing to change
	if again := d.BrokerCampaignSnapshot(); again.Generation != after.Generation || again.Campaigns[0] != after.Campaigns[0] {
		t.Fatalf("the following unchanged pass churned: %+v -> %+v", after, again)
	}
	if got := s.Stream.GetCampaigns(); len(got) != 1 || got[0] != assigned[0] {
		t.Fatalf("the following unchanged pass re-set the assignment, got %v", got)
	}
}

// Repeated identical outcomes add no routine INFO: the refresh evaluates
// (its steady-state restricted-assignment line is DEBUG) but announces
// nothing new.
func TestFreshnessRepeatedIdenticalOutcomeAddsNoINFO(t *testing.T) {
	d := restrictedTracker(55, 100)
	d.client = clientFor(lightSyncResponse{inventory: inventoryWithInProgress(map[string]interface{}{
		"id": "campaign-amd", "timeBasedDrops": []interface{}{invEntry("drop-1", 100, 55, nil)},
	})})
	d.updateStreamerCampaigns()

	logs := captureLogs(t, func() {
		for pass := 0; pass < 3; pass++ {
			d.syncProgress()
		}
	})

	if n := countINFO(logs); n != 0 {
		t.Fatalf("repeated identical no-change passes must not log at INFO, got %d:\n%s", n, logs)
	}
	if strings.Count(logs, "Channel-restricted drop campaign still assigned to streamer") != 3 {
		t.Fatalf("expected each refresh to re-evaluate the assignment at DEBUG, got:\n%s", logs)
	}
}

// A usable-ledger change that withholds ONE drop of an assigned campaign
// (campaign membership unchanged) is a semantic broker-view change: the
// refresh publishes it and re-points the streamer, so campaign-ID equality
// cannot hide it.
func TestFreshnessLedgerDropSubsetChangeIsPublished(t *testing.T) {
	now := time.Now()
	d, s, c := continuitySetup(&now)
	c.Drops = append(c.Drops, &models.Drop{ID: "d2", Name: "R2", MinutesRequired: 60, CurrentMinutesWatched: 0,
		StartAt: now.Add(-time.Hour), EndAt: now.Add(100 * time.Hour)})
	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	d.skipLedger = ledger
	d.client = clientFor(freshnessResponses()[2]) // LS2: tracked campaign absent
	setAvailability(s, true, []string{"camp-1"}, now)
	d.updateStreamerCampaigns()
	before := d.BrokerCampaignSnapshot()
	if len(before.Campaigns) != 1 || len(before.Campaigns[0].Drops) != 2 {
		t.Fatalf("precondition: expected a 2-drop view, got %+v", before)
	}
	if got := s.Stream.GetCampaigns(); len(got) != 1 || got[0].CurrentDrop().ID != "d1" {
		t.Fatalf("precondition: expected d1 current, got %v", got)
	}
	must(t, ledger.Observe(context.Background(), skipEvidence{
		class: evidenceClaimAccepted, gameID: "g1", campaignID: "camp-1", dropID: "d1",
	}))

	d.syncProgress()

	after := d.BrokerCampaignSnapshot()
	if after.Generation == before.Generation {
		t.Fatalf("a drop-subset ledger change must publish new broker views: before=%d after=%d", before.Generation, after.Generation)
	}
	if len(after.Campaigns) != 1 || len(after.Campaigns[0].Drops) != 1 || after.Campaigns[0].Drops[0].ID != "d2" {
		t.Fatalf("the published view must withhold d1, got %+v", after.Campaigns)
	}
	got := s.Stream.GetCampaigns()
	if len(got) != 1 || got[0] != after.Campaigns[0] || got[0].CurrentDrop().ID != "d2" {
		t.Fatalf("the streamer must be re-pointed at the filtered view, got %v", got)
	}
}

// Broker views lagging the pool revision (a newer pool published before its
// own re-point ran) are never reused by the no-churn check, even when the
// campaign and drop IDs match: the refresh publishes views of the CURRENT
// revision so SourceRevision catches up.
func TestFreshnessLaggingBrokerRevisionIsNotReused(t *testing.T) {
	now := time.Now()
	d, s, c := continuitySetup(&now)
	d.client = clientFor(freshnessResponses()[2]) // LS2: tracked campaign absent
	setAvailability(s, true, []string{"camp-1"}, now)
	d.updateStreamerCampaigns()
	newer := c.Clone()
	newer.Drops[0].CurrentMinutesWatched = 30
	d.mu.Lock()
	d.campaigns = []*models.Campaign{newer}
	d.bumpRevisionLocked(updateSourceFullSync)
	d.mu.Unlock()
	before := d.BrokerCampaignSnapshot()
	if before.SourceRevision == before.CurrentRevision {
		t.Fatal("precondition: broker views must lag the pool")
	}

	d.syncProgress()

	after := d.BrokerCampaignSnapshot()
	if after.SourceRevision != after.CurrentRevision || after.Generation == before.Generation {
		t.Fatalf("lagging broker views must be republished for the current revision: before=%+v after=%+v", before, after)
	}
	if got := s.Stream.GetCampaigns(); len(got) != 1 || got[0] != newer {
		t.Fatalf("the streamer must be re-pointed at the current pool object, got %v", got)
	}
}

// LS1 and the full sync keep their historical re-point: updateStreamerCampaigns
// always republishes the broker views and re-sets assignments even when
// nothing changed. Only the F-D2 refresh suppresses that churn.
func TestFreshnessHistoricalPassAlwaysRepublishes(t *testing.T) {
	now := time.Now()
	d, s, _ := continuitySetup(&now)
	setAvailability(s, true, []string{"camp-1"}, now)
	d.updateStreamerCampaigns()
	generation := d.BrokerCampaignSnapshot().Generation
	s.Stream.BroadcastID = "broadcast-2"

	d.updateStreamerCampaigns()

	if g := d.BrokerCampaignSnapshot().Generation; g != generation+1 {
		t.Fatalf("the historical pass must always republish broker views: %d -> %d", generation, g)
	}
	if !s.Stream.ProvisionalDropSnapshot().HasConfirmedCampaign("camp-1") {
		t.Fatal("the historical pass must still re-set an unchanged assignment (SetCampaigns re-stamps the broadcast fact)")
	}
}
