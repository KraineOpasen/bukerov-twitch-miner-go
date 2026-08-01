package drops

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/twitch"
)

// statusClient is a twitchClient whose ClaimDrop returns a scripted ClaimStatus
// (or error) and counts how many times it was actually invoked, so a test can
// prove both the claim-call count and the success-event count.
type statusClient struct {
	*fakeDropsClient
	mu     sync.Mutex
	status twitch.ClaimStatus
	err    error
	calls  int
}

func (c *statusClient) ClaimDrop(*models.Drop) (twitch.ClaimStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.status, c.err
}

func (c *statusClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// invEntry builds one inventory timeBasedDrops entry. extraSelf lets a test add
// dropInstanceID / isClaimable / isClaimed / hasPreconditionsMet to the `self`.
func invEntry(id string, required, watched float64, extraSelf map[string]interface{}) map[string]interface{} {
	sf := map[string]interface{}{"currentMinutesWatched": watched}
	for k, v := range extraSelf {
		sf[k] = v
	}
	return map[string]interface{}{
		"id":                     id,
		"name":                   "Reward " + id,
		"requiredMinutesWatched": required,
		"self":                   sf,
	}
}

// campaignWithDrop builds a campaign carrying a REAL identity -- a game ID, a
// campaign ID, and an entitlement window (StartAt/EndAt), plus a benefit ID
// on the drop -- so a test driving it through claimDropFnFor exercises the
// actual production identity-bundle path (campaignGameID/
// campaignFallbackWindow), not zero values that would pass even if that path
// were broken or bypassed.
func campaignWithDrop(dropID string, required int) *models.Campaign {
	now := time.Now()
	return &models.Campaign{
		ID:      "camp-1",
		Game:    &models.Game{ID: "game-1", Name: "Game One"},
		StartAt: now.Add(-time.Hour),
		EndAt:   now.Add(10 * time.Hour),
		Drops: []*models.Drop{
			{ID: dropID, Name: "Reward " + dropID, BenefitID: "ben-" + dropID, MinutesRequired: required},
		},
	}
}

func trackerWithHook(t *testing.T, client twitchClient) (*DropsTracker, *int) {
	t.Helper()
	tr := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	successes := 0
	tr.SetDropClaimedHook(func(string) { successes++ })
	return tr, &successes
}

// TestSyncDropsClaimGatingMatrix drives Campaign.SyncDrops through the tracker's
// real claim callback and asserts the number of claim calls, the number of
// user-facing success events, and the drop's reconciled IsClaimed state for each
// authoritative outcome. This is the exactly-once / no-false-success core.
//
// wantLedgerRow additionally pins THIS EXACT call site (`tr.claimDropFnFor(camp)`
// below) to the skip ledger: it is true only for the two cases that reach an
// authoritative Accepted() outcome (fresh accept / already-claimed), and a
// per-case *SkipLedger is wired so the assertion exercises the real
// production wiring, not a separate call elsewhere. Without this, silently
// dropping the campaign argument at this call site (e.g. reverting it to a
// nil/no-identity form) would leave every assertion in this table
// unchanged -- none of the others depend on campaign identity at all -- so
// this closes that exact gap.
func TestSyncDropsClaimGatingMatrix(t *testing.T) {
	cases := []struct {
		name           string
		extraSelf      map[string]interface{}
		status         twitch.ClaimStatus
		err            error
		wantCalls      int
		wantSuccesses  int
		wantClaimedSet bool
		wantLedgerRow  bool
	}{
		{
			name:      "no authoritative signal (no instance) -> never calls claim",
			extraSelf: map[string]interface{}{}, // local minutes complete, but no instance
			status:    twitch.ClaimStatusAccepted,
			wantCalls: 0, wantSuccesses: 0, wantClaimedSet: false,
		},
		{
			name:      "server isClaimable=false over local 100% -> never calls claim",
			extraSelf: map[string]interface{}{"dropInstanceID": "inst-1", "isClaimable": false},
			status:    twitch.ClaimStatusAccepted,
			wantCalls: 0, wantSuccesses: 0, wantClaimedSet: false,
		},
		{
			name:      "hasPreconditionsMet=false blocks -> never calls claim",
			extraSelf: map[string]interface{}{"dropInstanceID": "inst-1", "hasPreconditionsMet": false},
			status:    twitch.ClaimStatusAccepted,
			wantCalls: 0, wantSuccesses: 0, wantClaimedSet: false,
		},
		{
			name:      "fresh accept -> exactly one claim, one success, reconciled claimed",
			extraSelf: map[string]interface{}{"dropInstanceID": "inst-1"},
			status:    twitch.ClaimStatusAccepted,
			wantCalls: 1, wantSuccesses: 1, wantClaimedSet: true, wantLedgerRow: true,
		},
		{
			name:      "already-claimed -> one claim, NO success event, reconciled claimed",
			extraSelf: map[string]interface{}{"dropInstanceID": "inst-1"},
			status:    twitch.ClaimStatusAlreadyClaimed,
			wantCalls: 1, wantSuccesses: 0, wantClaimedSet: true, wantLedgerRow: true,
		},
		{
			name:      "rejected -> one claim, no success, NOT claimed (retryable)",
			extraSelf: map[string]interface{}{"dropInstanceID": "inst-1"},
			status:    twitch.ClaimStatusRejected,
			wantCalls: 1, wantSuccesses: 0, wantClaimedSet: false,
		},
		{
			name:      "transient error -> one claim attempt, no success, NOT claimed",
			extraSelf: map[string]interface{}{"dropInstanceID": "inst-1"},
			status:    twitch.ClaimStatus(""),
			err:       errors.New("boom"),
			wantCalls: 1, wantSuccesses: 0, wantClaimedSet: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &statusClient{fakeDropsClient: &fakeDropsClient{}, status: tc.status, err: tc.err}
			tr, successes := trackerWithHook(t, client)
			ledger := newTestSkipLedger(t, uniqueAccountKey(t))
			tr.SetSkipLedger(ledger)

			camp := campaignWithDrop("d1", 60)
			camp.SyncDrops([]interface{}{invEntry("d1", 60, 60, tc.extraSelf)}, tr.claimDropFnFor(camp))

			if got := client.callCount(); got != tc.wantCalls {
				t.Errorf("claim calls = %d, want %d", got, tc.wantCalls)
			}
			if *successes != tc.wantSuccesses {
				t.Errorf("success events = %d, want %d", *successes, tc.wantSuccesses)
			}
			if got := camp.Drops[0].IsClaimed; got != tc.wantClaimedSet {
				t.Errorf("IsClaimed = %v, want %v", got, tc.wantClaimedSet)
			}

			snap, err := ledger.Snapshot(context.Background())
			if err != nil {
				t.Fatalf("snapshot: %v", err)
			}
			_, gotRow := snap.byInstance["inst-1"]
			if gotRow != tc.wantLedgerRow {
				t.Errorf("ledger row present = %v, want %v (campaign identity must reach claimDropFnFor at this exact call site whenever a claim is authoritatively accepted)", gotRow, tc.wantLedgerRow)
			}
		})
	}
}

// TestRepeatedSyncNoDuplicateSuccess proves a repeated inventory sync does not
// re-claim or re-emit a success event once Twitch reports the drop claimed.
//
// The ledger assertion at the end additionally pins these THREE call sites to
// campaign identity: dropping it (e.g. reverting any of them to a
// no-identity form) would leave callCount/successes unchanged -- neither
// depends on the ledger -- so without this check that regression would be
// invisible here.
func TestRepeatedSyncNoDuplicateSuccess(t *testing.T) {
	client := &statusClient{fakeDropsClient: &fakeDropsClient{}, status: twitch.ClaimStatusAccepted}
	tr, successes := trackerWithHook(t, client)
	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	tr.SetSkipLedger(ledger)
	camp := campaignWithDrop("d1", 60)

	// Sync 1: minted instance, unclaimed -> one fresh claim + one success event.
	camp.SyncDrops([]interface{}{invEntry("d1", 60, 60, map[string]interface{}{"dropInstanceID": "inst-1"})}, tr.claimDropFnFor(camp))
	// Sync 2: Twitch now reports it claimed -> no second claim, no second event.
	camp.SyncDrops([]interface{}{invEntry("d1", 60, 60, map[string]interface{}{"dropInstanceID": "inst-1", "isClaimed": true})}, tr.claimDropFnFor(camp))
	// Sync 3 (repeat of the claimed state): still idempotent.
	camp.SyncDrops([]interface{}{invEntry("d1", 60, 60, map[string]interface{}{"dropInstanceID": "inst-1", "isClaimed": true})}, tr.claimDropFnFor(camp))

	if client.callCount() != 1 {
		t.Errorf("expected exactly one claim across repeated syncs, got %d", client.callCount())
	}
	if *successes != 1 {
		t.Errorf("expected exactly one success event across repeated syncs, got %d", *successes)
	}

	snap, err := ledger.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	row, ok := snap.byInstance["inst-1"]
	if !ok {
		t.Fatal("expected a ledger row for the fresh claim -- campaign identity did not reach claimDropFnFor at this call site")
	}
	if row.campaignID != camp.ID || row.gameID != camp.Game.ID {
		t.Errorf("incomplete identity persisted: %+v (want campaign=%s game=%s)", row, camp.ID, camp.Game.ID)
	}
}

// TestAlreadyClaimedReconciliationNoEvent isolates invariant: an authoritative
// already-claimed response reconciles local state to claimed but never emits a
// user-facing success event.
//
// The ledger assertion at the end additionally pins this call site to
// campaign identity (E2 evidence specifically) -- dropping it would leave
// callCount/successes/IsClaimed unchanged, so without this check that
// regression would be invisible here.
func TestAlreadyClaimedReconciliationNoEvent(t *testing.T) {
	client := &statusClient{fakeDropsClient: &fakeDropsClient{}, status: twitch.ClaimStatusAlreadyClaimed}
	tr, successes := trackerWithHook(t, client)
	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	tr.SetSkipLedger(ledger)
	camp := campaignWithDrop("d1", 60)

	camp.SyncDrops([]interface{}{invEntry("d1", 60, 60, map[string]interface{}{"dropInstanceID": "inst-1"})}, tr.claimDropFnFor(camp))

	if client.callCount() != 1 {
		t.Errorf("already-claimed must still issue exactly one mutation, got %d", client.callCount())
	}
	if *successes != 0 {
		t.Errorf("already-claimed reconciliation must not emit a success event, got %d", *successes)
	}
	if !camp.Drops[0].IsClaimed {
		t.Error("already-claimed must reconcile local IsClaimed to true")
	}

	snap, err := ledger.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	row, ok := snap.byInstance["inst-1"]
	if !ok {
		t.Fatal("expected a ledger row for the already-claimed reconciliation -- campaign identity did not reach claimDropFnFor at this call site")
	}
	if row.campaignID != camp.ID || row.gameID != camp.Game.ID {
		t.Errorf("incomplete identity persisted: %+v (want campaign=%s game=%s)", row, camp.ID, camp.Game.ID)
	}
}

// TestLightweightProgressSyncNeverClaims proves the hot progress path never
// issues a claim mutation, even for a drop Twitch reports as claimable — the
// only claiming paths are the full sync / inventory sweep, so concurrent
// progress syncs cannot trigger duplicate claims.
func TestLightweightProgressSyncNeverClaims(t *testing.T) {
	claimable := map[string]interface{}{
		"id":   "camp-1",
		"name": "Camp",
		"game": map[string]interface{}{"id": "g1", "name": "Game"},
		"timeBasedDrops": []interface{}{
			invEntry("d1", 60, 60, map[string]interface{}{"dropInstanceID": "inst-1"}),
		},
	}
	client := &statusClient{
		fakeDropsClient: &fakeDropsClient{inventory: inventoryWithInProgress(claimable)},
		status:          twitch.ClaimStatusAccepted,
	}
	tr, successes := trackerWithHook(t, client)
	// Seed a tracked campaign so syncProgress has something to refresh.
	tr.campaigns = []*models.Campaign{campaignWithDrop("d1", 60)}

	tr.syncProgress()

	if client.callCount() != 0 {
		t.Errorf("lightweight progress sync must never claim, got %d claim calls", client.callCount())
	}
	if *successes != 0 {
		t.Errorf("lightweight progress sync must not emit success events, got %d", *successes)
	}
}

// TestClaimPathBuildsFreshInstancelessDrops proves the starting point of every
// active-claim full-sync pass: a Drop built from DropCampaignDetails carries NO
// authoritative self-state (no dropInstanceID, Unknown claimability, not
// claimable) until the CURRENT inventory snapshot is applied. This is the
// architectural guarantee that makes Drop.Update's field-retention semantics
// safe: a stale dropInstanceID from a prior sync can never reach a claim on the
// full-sync path because the object is rebuilt fresh from details each sync.
func TestClaimPathBuildsFreshInstancelessDrops(t *testing.T) {
	summary, detail := dashCampaign("c1", "Camp", "g1", "Game", "d1", "Reward")
	campaign, _, skip := buildTrackedCampaign(summary, detail)
	if skip != skipNone {
		t.Fatalf("campaign should be tracked, got skip=%v", skip)
	}
	if len(campaign.Drops) == 0 {
		t.Fatal("expected drops built from details")
	}
	for _, dr := range campaign.Drops {
		if dr.DropInstanceID != "" {
			t.Errorf("fresh drop from details must have no dropInstanceID, got %q", dr.DropInstanceID)
		}
		if dr.Claimability != models.ClaimabilityUnknown {
			t.Errorf("fresh drop must be ClaimabilityUnknown, got %v", dr.Claimability)
		}
		if dr.CanClaim() {
			t.Error("fresh drop (no inventory self applied yet) must not be claimable")
		}
	}
}

// TestFullSyncNoClaimWithoutInstanceID drives the real full-sync claim path end
// to end (getActiveCampaigns -> details -> syncWithInventory + the raw-inventory
// sweep). The current inventory snapshot reports the drop's watch requirement met
// and unclaimed but supplies NO dropInstanceID, so there is no authoritative
// claim signal: ClaimDrop must never be called and no success event is emitted —
// proving the claim reflects the current snapshot with zero cross-sync carryover.
func TestFullSyncNoClaimWithoutInstanceID(t *testing.T) {
	summary, detail := dashCampaign("c1", "Camp", "g1", "Game", "d1", "Reward")
	invCampaign := map[string]interface{}{
		"id":   "c1",
		"name": "Camp",
		"game": map[string]interface{}{"id": "g1", "name": "Game"},
		"timeBasedDrops": []interface{}{
			// Watch requirement met and unclaimed, but NO dropInstanceID minted.
			invEntry("d1", 60, 60, map[string]interface{}{"isClaimed": false}),
		},
	}
	client := &statusClient{fakeDropsClient: &fakeDropsClient{
		dashboard: dashboardResponse(summary),
		details:   map[string]map[string]interface{}{"c1": detail},
		inventory: inventoryWithInProgress(invCampaign),
	}, status: twitch.ClaimStatusAccepted}
	tr, successes := trackerWithHook(t, client)

	tr.syncCampaigns()

	if client.callCount() != 0 {
		t.Fatalf("no dropInstanceID => no authoritative signal => ClaimDrop must not be called, got %d", client.callCount())
	}
	if *successes != 0 {
		t.Fatalf("no claim => no success event, got %d", *successes)
	}
	// The campaign is still tracked (its drop is farmable, just not claimable yet).
	if len(tr.Campaigns()) == 0 {
		t.Fatal("campaign should be tracked while its drop is still progressing")
	}
}

// TestClaimDropFnForPersistsFullLedgerIdentity closes the coverage gap this
// file's claim-callback tests left before this test was added: it drives
// BOTH authoritative outcomes that feed the skip ledger (a fresh accept ->
// E1, an already-claimed reconciliation -> E2) through the real
// Campaign.SyncDrops wiring against a campaign carrying a REAL identity
// (campaignWithDrop's game ID, campaign ID, and entitlement window -- not
// zero values), and asserts the ledger receives the COMPLETE bundle: game,
// campaign, drop, instance, and benefit ID, plus a Known, campaign-sourced
// window matching the campaign's own StartAt/EndAt, AND the correct evidence
// class/rank for the outcome that produced the row -- evidence_class/
// evidence_rank are not exposed via skipRow/skipRowColumns (decision-
// relevant columns only), so this reads them with a raw query. Swapping
// evidenceClaimAccepted <-> evidenceClaimAlready at the fresh-claim seam in
// claimDropFnFor would otherwise pass this whole test undetected: every
// other assertion here (row presence, identity bundle, window) is identical
// for E1 and E2. This is the haveIdentity branch of claimDropFnFor that,
// before this test, had exactly one direct exerciser
// (skipledger_test.go's TestClaimSeamPersistsCompleteIdentityBundle, which
// calls the returned closure directly rather than through SyncDrops) and zero
// coverage from this file's own boundary suite.
func TestClaimDropFnForPersistsFullLedgerIdentity(t *testing.T) {
	cases := []struct {
		name              string
		status            twitch.ClaimStatus
		instanceID        string
		wantEvidenceClass skipEvidenceClass
	}{
		{name: "fresh accept (E1)", status: twitch.ClaimStatusAccepted, instanceID: "inst-accept", wantEvidenceClass: evidenceClaimAccepted},
		{name: "already claimed (E2)", status: twitch.ClaimStatusAlreadyClaimed, instanceID: "inst-already", wantEvidenceClass: evidenceClaimAlready},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ledger := newTestSkipLedger(t, uniqueAccountKey(t))
			client := &statusClient{fakeDropsClient: &fakeDropsClient{}, status: tc.status}
			tr, _ := trackerWithHook(t, client)
			tr.SetSkipLedger(ledger)

			camp := campaignWithDrop("d1", 60)
			camp.SyncDrops(
				[]interface{}{invEntry("d1", 60, 60, map[string]interface{}{"dropInstanceID": tc.instanceID})},
				tr.claimDropFnFor(camp),
			)

			if client.callCount() != 1 {
				t.Fatalf("expected exactly one claim call, got %d", client.callCount())
			}

			snap, err := ledger.Snapshot(context.Background())
			if err != nil {
				t.Fatalf("snapshot: %v", err)
			}
			row, ok := snap.byInstance[tc.instanceID]
			if !ok {
				t.Fatal("expected a ledger row keyed by the claimed instance -- the haveIdentity branch did not fire (campaign context was dropped)")
			}
			if row.gameID != camp.Game.ID || row.campaignID != camp.ID || row.dropID != "d1" ||
				row.instanceID != tc.instanceID || row.benefitID != "ben-d1" {
				t.Fatalf("incomplete identity bundle persisted: %+v (want game=%s campaign=%s drop=d1 instance=%s benefit=ben-d1)",
					row, camp.Game.ID, camp.ID, tc.instanceID)
			}
			if !row.window.Known || row.window.Source != models.WindowSourceCampaign {
				t.Fatalf("expected a Known, campaign-sourced window, got %+v", row.window)
			}
			if row.window.Start.UnixMilli() != camp.StartAt.UnixMilli() || row.window.End.UnixMilli() != camp.EndAt.UnixMilli() {
				t.Fatalf("window bounds mismatch: got [%v,%v), want [%v,%v)", row.window.Start, row.window.End, camp.StartAt, camp.EndAt)
			}

			var gotClass string
			var gotRank int
			if err := ledger.db.QueryRow(
				`SELECT evidence_class, evidence_rank FROM drop_reward_skips WHERE account_key = ? AND instance_id = ?`,
				ledger.accountKey, tc.instanceID,
			).Scan(&gotClass, &gotRank); err != nil {
				t.Fatalf("read evidence_class/evidence_rank: %v", err)
			}
			if gotClass != string(tc.wantEvidenceClass) {
				t.Errorf("evidence_class = %q, want %q", gotClass, tc.wantEvidenceClass)
			}
			if gotRank != tc.wantEvidenceClass.rank() {
				t.Errorf("evidence_rank = %d, want %d", gotRank, tc.wantEvidenceClass.rank())
			}
		})
	}
}

// TestSyncWithInventoryPassesCampaignIdentityToClaimDropFnFor pins
// syncWithInventory's own claim call site (drops.go, inside the
// campaign-merge loop: `campaign.SyncDrops(drops, d.claimDropFnFor(campaign))`)
// to campaign identity actually reaching the skip ledger. Before this test,
// reverting that exact call site to claimDropFnFor(nil) (or otherwise
// dropping campaign) was caught by NO test in this package: every other
// syncWithInventory-driving test (skipledger_test.go's E3 tests, this
// package's game/blacklist/ACL suites) exercises it with either no claimable
// drop or an already-claimed one, never a genuine fresh claim: and
// claimDropFnFor's own identity-bundle test (above) exercises its OWN direct
// call site, not this production one. This drives the REAL syncWithInventory
// path end to end with a genuinely claimable, freshly-minted drop instance.
func TestSyncWithInventoryPassesCampaignIdentityToClaimDropFnFor(t *testing.T) {
	prog := map[string]interface{}{
		"id":   "camp-1",
		"name": "Camp One",
		"timeBasedDrops": []interface{}{
			invEntry("d1", 60, 60, map[string]interface{}{"dropInstanceID": "inst-swi"}),
		},
	}
	client := &statusClient{
		fakeDropsClient: &fakeDropsClient{inventory: inventoryWithInProgress(prog)},
		status:          twitch.ClaimStatusAccepted,
	}
	tr, _ := trackerWithHook(t, client)
	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	tr.SetSkipLedger(ledger)

	campaigns := []*models.Campaign{campaignWithDrop("d1", 60)} // ID "camp-1", Game "game-1"
	tr.syncWithInventory(campaigns)

	if client.callCount() != 1 {
		t.Fatalf("expected exactly one claim call through syncWithInventory, got %d", client.callCount())
	}

	snap, err := ledger.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	row, ok := snap.byInstance["inst-swi"]
	if !ok {
		t.Fatal("expected a ledger row for the claim reached through syncWithInventory -- campaign identity did not reach claimDropFnFor at this call site")
	}
	if row.gameID != "game-1" || row.campaignID != "camp-1" || row.dropID != "d1" {
		t.Fatalf("incomplete identity bundle persisted through syncWithInventory: %+v", row)
	}
}

// TestBuildInProgressCampaignPassesCampaignIdentityToClaimDropFnFor is
// TestSyncWithInventoryPassesCampaignIdentityToClaimDropFnFor's twin for
// claimDropFnFor's OTHER production call site: buildInProgressCampaign
// (drops.go: `campaign.SyncDrops(drops, d.claimDropFnFor(campaign))`).
// drops_test.go's existing buildInProgressCampaign tests (frozen; read-only
// here) drive either a not-yet-claimable drop or an already-claimed one,
// never a genuine claim, so reverting this call site the same way was caught
// by NO test either.
func TestBuildInProgressCampaignPassesCampaignIdentityToClaimDropFnFor(t *testing.T) {
	client := &statusClient{fakeDropsClient: &fakeDropsClient{}, status: twitch.ClaimStatusAccepted}
	tr, _ := trackerWithHook(t, client)
	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	tr.SetSkipLedger(ledger)

	prog := map[string]interface{}{
		"id":   "camp-bip",
		"name": "Camp BIP",
		"game": map[string]interface{}{"id": "game-bip", "name": "Game BIP"},
		"timeBasedDrops": []interface{}{
			invEntry("d1", 60, 60, map[string]interface{}{"dropInstanceID": "inst-bip"}),
		},
	}

	campaign := tr.buildInProgressCampaign(prog)
	if campaign == nil {
		t.Fatal("expected a recovered campaign, got nil")
	}

	if client.callCount() != 1 {
		t.Fatalf("expected exactly one claim call through buildInProgressCampaign, got %d", client.callCount())
	}

	snap, err := ledger.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	row, ok := snap.byInstance["inst-bip"]
	if !ok {
		t.Fatal("expected a ledger row for the claim reached through buildInProgressCampaign -- campaign identity did not reach claimDropFnFor at this call site")
	}
	if row.gameID != "game-bip" || row.campaignID != "camp-bip" || row.dropID != "d1" {
		t.Fatalf("incomplete identity bundle persisted through buildInProgressCampaign: %+v", row)
	}
}
