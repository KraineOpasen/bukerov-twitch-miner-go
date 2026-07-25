package drops

import (
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// BKM-026 requirement traceability for the requirements exercised at the drops
// (pipeline) layer. The model/decode requirements (E1-E9, E15) live in
// internal/models/account_link_test.go and the eligibility truth table (E10-E15,
// E20, E21) in internal/eligibility/account_link_test.go.
//
//	E16/E17  TestAccountLinkPartialDataFailsOpenPerCampaign, TestAccountLinkBackfillFromSummary
//	E18/I5/I14 TestAccountLinkMixedCampaignStaysTrackable, TestAccountLinkDashboardCountReflectsTrackableSet
//	E19       TestAccountLinkAllIneligibleCampaignDropped
//	E22/I10   TestAccountLinkAddsNoNewRequest
//	E23/I11   TestPersistedDropsQueriesUnchanged
//	E24       TestAccountLinkNoOpOnLegacyShape, TestAccountLinkComposesWithGameAndBlacklist
//	E25/I13   TestAccountLinkDashboardCountReflectsTrackableSet
//	E26       TestAccountConnectionCloneAndReparseStable (models), backfill no-downgrade below
//	E27       whole suite is race-clean (run via `go test -race`)
//	I1-I8     TestAccountLinkPipelineTruthTable (connected/disconnected/no-self/self:null/mixed/badge/emote/new-type)
//	I9        TestAccountLinkComposesWithGameAndBlacklist (existing filter order preserved)
//	I12       TestAccountLinkProgressSyncDoesNotRefilter (+ full suite: no watch/PubSub regressions)

// benefitDrop builds a details-shape timeBasedDrops entry (active, unclaimed)
// whose single benefit carries the given distributionType, so ClearClaimedDrops
// keeps it and NewDropFromGQL classifies its BenefitType.
func benefitDrop(id, name, distributionType string) map[string]interface{} {
	now := time.Now()
	return map[string]interface{}{
		"id":                     id,
		"name":                   name,
		"requiredMinutesWatched": float64(60),
		"startAt":                rfc3339(now.Add(-time.Hour)),
		"endAt":                  rfc3339(now.Add(24 * time.Hour)),
		"benefitEdges": []interface{}{
			map[string]interface{}{"benefit": map[string]interface{}{
				"id": "b-" + id, "name": name, "distributionType": distributionType,
			}},
		},
	}
}

// linkCampaign builds a ViewerDropsDashboard summary + DropCampaignDetails pair
// for a campaign whose self.isAccountConnected is set per conn:
//
//	"true"   -> self.isAccountConnected = true  (Connected)
//	"false"  -> self.isAccountConnected = false (Disconnected)
//	"null"   -> self.isAccountConnected = nil   (Unknown)
//	"absent" -> no self object at all           (Unknown)
//
// The drops (details-shape) go on the detail only, exactly as the real API
// returns them (the summary carries no timeBasedDrops).
func linkCampaign(id, name, gameID, gameName, conn string, drops ...map[string]interface{}) (summary, detail map[string]interface{}) {
	now := time.Now()
	game := map[string]interface{}{"id": gameID, "name": gameName}
	base := func() map[string]interface{} {
		m := map[string]interface{}{
			"id": id, "name": name, "status": "ACTIVE",
			"startAt": rfc3339(now.Add(-2 * time.Hour)),
			"endAt":   rfc3339(now.Add(48 * time.Hour)),
			"game":    game,
		}
		switch conn {
		case "true":
			m["self"] = map[string]interface{}{"isAccountConnected": true}
		case "false":
			m["self"] = map[string]interface{}{"isAccountConnected": false}
		case "null":
			m["self"] = map[string]interface{}{"isAccountConnected": nil}
		case "absent":
			// no self object
		}
		return m
	}
	summary = base()
	detail = base()
	list := make([]interface{}, 0, len(drops))
	for _, d := range drops {
		list = append(list, d)
	}
	detail["timeBasedDrops"] = list
	return summary, detail
}

func campaignByID(cs []*models.Campaign, id string) *models.Campaign {
	for _, c := range cs {
		if c.ID == id {
			return c
		}
	}
	return nil
}

func dropNames(c *models.Campaign) []string {
	if c == nil {
		return nil
	}
	names := make([]string, 0, len(c.Drops))
	for _, d := range c.Drops {
		names = append(names, d.Name)
	}
	return names
}

// syncOne runs a full sync over a single campaign (summary+detail) with an empty
// inventory and returns the tracked campaign (nil if it was excluded).
func syncOne(t *testing.T, summary, detail map[string]interface{}) *models.Campaign {
	t.Helper()
	client := &fakeDropsClient{
		dashboard: dashboardResponse(summary),
		inventory: emptyInventoryResponse(),
		details:   map[string]map[string]interface{}{summary["id"].(string): detail},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tracker.syncCampaigns()
	return campaignByID(tracker.Campaigns(), summary["id"].(string))
}

// --- Pipeline truth table (BKM-026 E10-E14, I1-I8) -------------------------

func TestAccountLinkPipelineTruthTable(t *testing.T) {
	const (
		DE    = "DIRECT_ENTITLEMENT"
		BADGE = "BADGE"
		EMOTE = "EMOTE"
	)
	cases := []struct {
		name       string
		conn       string
		dist       string
		wantExists bool // campaign remains trackable with its drop
	}{
		{"connected+linked (E10,I1)", "true", DE, true},
		{"connected+badge (E2)", "true", BADGE, true},
		{"connected+emote (E3)", "true", EMOTE, true},
		{"disconnected+linked (E11,I2)", "false", DE, false}, // excluded -> campaign gone
		{"disconnected+badge (E13,I6)", "false", BADGE, true},
		{"disconnected+emote (E14,I7)", "false", EMOTE, true},
		{"unknown-absent+linked (E12,I3)", "absent", DE, true},
		{"unknown-null+linked (I4)", "null", DE, true},
		{"unknown-absent+badge (E8)", "absent", BADGE, true},
		{"unknown+newtype (E9,I8)", "absent", "IN_GAME_ITEM_V2", true},
		{"disconnected+newtype fail-open (E9,I8)", "false", "SOMETHING_NEW", true},
		{"disconnected+missingtype fail-open (E15)", "false", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var drop map[string]interface{}
			if tc.dist == "" {
				drop = activeDrop("d1", "Reward", 60) // no benefitEdges -> Unknown type
			} else {
				drop = benefitDrop("d1", "Reward", tc.dist)
			}
			s, d := linkCampaign("c1", "Camp", "g1", "Game", tc.conn, drop)
			got := syncOne(t, s, d)
			if tc.wantExists {
				if got == nil {
					t.Fatalf("campaign must remain trackable, but it was excluded")
				}
				if len(got.Drops) != 1 {
					t.Fatalf("expected the reward to remain, got drops=%v", dropNames(got))
				}
			} else if got != nil {
				t.Fatalf("campaign must be excluded (not trackable), but it is tracked with drops=%v", dropNames(got))
			}
		})
	}
}

// E18 + I5 + I14: a mixed campaign (disconnected) with one DIRECT_ENTITLEMENT and
// one BADGE keeps the BADGE reward and stays trackable — a campaign is never
// hidden while one eligible reward remains.
func TestAccountLinkMixedCampaignStaysTrackable(t *testing.T) {
	s, d := linkCampaign("cmix", "Mixed", "g1", "Game", "false",
		benefitDrop("d-item", "Rare Skin", "DIRECT_ENTITLEMENT"),
		benefitDrop("d-badge", "Shiny Badge", "BADGE"),
	)
	got := syncOne(t, s, d)
	if got == nil {
		t.Fatal("mixed campaign must remain trackable")
	}
	if names := dropNames(got); len(names) != 1 || names[0] != "Shiny Badge" {
		t.Fatalf("only the badge reward must remain, got %v", names)
	}
}

// E19: a campaign whose only rewards require the link, while disconnected, is not
// trackable — it is removed from the published set (so it drives no watch slot
// and is not counted).
func TestAccountLinkAllIneligibleCampaignDropped(t *testing.T) {
	s, d := linkCampaign("conly", "LinkOnly", "g1", "Game", "false",
		benefitDrop("d1", "Skin A", "DIRECT_ENTITLEMENT"),
		benefitDrop("d2", "Skin B", "DIRECT_ENTITLEMENT"),
	)
	if got := syncOne(t, s, d); got != nil {
		t.Fatalf("campaign with only link-required rewards (disconnected) must be dropped, got drops=%v", dropNames(got))
	}
}

// E25 + I13: after a mixed sync the dashboard pool (Campaigns()) reflects exactly
// the trackable set — an all-ineligible campaign is absent, a mixed one present
// once (no duplicates), an unknown/connected one present.
func TestAccountLinkDashboardCountReflectsTrackableSet(t *testing.T) {
	keepS, keepD := linkCampaign("keep", "Connected", "g1", "Game1", "true",
		benefitDrop("k1", "Item", "DIRECT_ENTITLEMENT"))
	mixS, mixD := linkCampaign("mix", "Mixed", "g2", "Game2", "false",
		benefitDrop("m1", "Item", "DIRECT_ENTITLEMENT"), benefitDrop("m2", "Emote", "EMOTE"))
	dropS, dropD := linkCampaign("gone", "AllLinked", "g3", "Game3", "false",
		benefitDrop("x1", "Item", "DIRECT_ENTITLEMENT"))
	unkS, unkD := linkCampaign("unk", "Unknown", "g4", "Game4", "absent",
		benefitDrop("u1", "Item", "DIRECT_ENTITLEMENT"))

	client := &fakeDropsClient{
		dashboard: dashboardResponse(keepS, mixS, dropS, unkS),
		inventory: emptyInventoryResponse(),
		details: map[string]map[string]interface{}{
			"keep": keepD, "mix": mixD, "gone": dropD, "unk": unkD,
		},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tracker.syncCampaigns()

	tracked := tracker.Campaigns()
	got := keptIDs(tracked)
	if !got["keep"] || !got["mix"] || !got["unk"] || got["gone"] {
		t.Fatalf("tracked set wrong: %v (want keep,mix,unk; not gone)", got)
	}
	if len(tracked) != 3 {
		t.Fatalf("dashboard count must reflect trackable set (3), got %d: %v", len(tracked), got)
	}
	// No duplicate campaigns/rewards.
	if seen := map[string]int{}; func() bool {
		for _, c := range tracked {
			seen[c.ID]++
		}
		for _, n := range seen {
			if n != 1 {
				return true
			}
		}
		return false
	}() {
		t.Fatal("duplicate campaign in tracked set")
	}
	if mix := campaignByID(tracked, "mix"); mix == nil || len(mix.Drops) != 1 || mix.Drops[0].Name != "Emote" {
		t.Fatalf("mixed campaign must keep only the emote reward, got %v", dropNames(mix))
	}
}

// E16/E17: partial data (some campaigns missing self) fails open per-campaign —
// an authoritative disconnected campaign is filtered while a same-response
// campaign with no self stays trackable; a validly-present false is still honored.
func TestAccountLinkPartialDataFailsOpenPerCampaign(t *testing.T) {
	discS, discD := linkCampaign("disc", "Disconnected", "g1", "Game1", "false",
		benefitDrop("a", "Item", "DIRECT_ENTITLEMENT"))
	partialS, partialD := linkCampaign("partial", "NoSelf", "g2", "Game2", "absent",
		benefitDrop("b", "Item", "DIRECT_ENTITLEMENT"))
	client := &fakeDropsClient{
		dashboard: dashboardResponse(discS, partialS),
		inventory: emptyInventoryResponse(),
		details:   map[string]map[string]interface{}{"disc": discD, "partial": partialD},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tracker.syncCampaigns()
	got := keptIDs(tracker.Campaigns())
	if got["disc"] {
		t.Error("authoritative disconnected + linked-only campaign must be excluded")
	}
	if !got["partial"] {
		t.Error("campaign with no self (unknown) must fail open and stay trackable")
	}
}

// E24: a campaign in the old response shape (no self, no distributionType) is
// completely unaffected by the account-link pass — every existing filter observes
// a byte-identical drop set, and the campaign is tracked exactly as before.
func TestAccountLinkNoOpOnLegacyShape(t *testing.T) {
	s, d := dashCampaign("legacy", "Legacy WoT", "game-wot", "World of Tanks", "d1", "Garage Slot")
	got := syncOne(t, s, d)
	if got == nil || len(got.Drops) != 1 || got.Drops[0].Name != "Garage Slot" {
		t.Fatalf("legacy-shape campaign must be tracked unchanged, got %v", dropNames(got))
	}
	if got.AccountConnection != models.AccountConnectionUnknown {
		t.Fatalf("legacy campaign must decode as Unknown connection, got %v", got.AccountConnection)
	}
}

// --- No new network behavior (BKM-026 E22/E23, I10/I11) --------------------

// recordingDropsClient records every GQL operation name and DropCampaignDetails
// call so a test can prove the account-link decode adds NO request and uses NO
// new operation.
type recordingDropsClient struct {
	*fakeDropsClient
	mu        sync.Mutex
	ops       []string
	detailIDs []string
}

func (r *recordingDropsClient) PostGQL(op constants.GQLOperation) (map[string]interface{}, error) {
	r.mu.Lock()
	r.ops = append(r.ops, op.OperationName)
	r.mu.Unlock()
	return r.fakeDropsClient.PostGQL(op)
}

func (r *recordingDropsClient) GetDropCampaignDetails(id string) (map[string]interface{}, error) {
	r.mu.Lock()
	r.detailIDs = append(r.detailIDs, id)
	r.mu.Unlock()
	return r.fakeDropsClient.GetDropCampaignDetails(id)
}

// E22 + I10: a full sync issues only the pre-existing drops operations
// (ViewerDropsDashboard + Inventory) plus one DropCampaignDetails per dashboard
// campaign. The account-link feature introduces NO new operation and NO extra
// request — it only decodes fields already present in these responses.
func TestAccountLinkAddsNoNewRequest(t *testing.T) {
	s1, d1 := linkCampaign("c1", "A", "g1", "GameA", "false", benefitDrop("a", "Item", "DIRECT_ENTITLEMENT"))
	s2, d2 := linkCampaign("c2", "B", "g2", "GameB", "true", benefitDrop("b", "Emote", "EMOTE"))
	rec := &recordingDropsClient{fakeDropsClient: &fakeDropsClient{
		dashboard: dashboardResponse(s1, s2),
		inventory: emptyInventoryResponse(),
		details:   map[string]map[string]interface{}{"c1": d1, "c2": d2},
	}}
	tracker := NewDropsTracker(rec, nil, config.RateLimitSettings{}, nil)
	tracker.syncCampaigns()

	allowed := map[string]bool{"ViewerDropsDashboard": true, "Inventory": true}
	for _, op := range rec.ops {
		if !allowed[op] {
			t.Fatalf("unexpected GQL operation %q — account-link must introduce no new operation", op)
		}
	}
	// Exactly one details fetch per dashboard campaign — no extra fetch added.
	if len(rec.detailIDs) != 2 {
		t.Fatalf("expected 2 DropCampaignDetails fetches (one per campaign), got %d: %v", len(rec.detailIDs), rec.detailIDs)
	}
}

// E23 + I11: the persisted-query operation names, sha256 hashes, and variables of
// every drops operation are byte-identical to the values in effect before this
// change. This pins the wire contract so decoding extra response fields can never
// silently ride along with a hash/variable change.
func TestPersistedDropsQueriesUnchanged(t *testing.T) {
	cases := []struct {
		op   constants.GQLOperation
		name string
		hash string
		vars map[string]interface{}
	}{
		{constants.Inventory, "Inventory",
			"d86775d0ef16a63a33ad52e80eaff963b2d5b72fada7c991504a57496e1d8e4b",
			map[string]interface{}{"fetchRewardCampaigns": true}},
		{constants.ViewerDropsDashboard, "ViewerDropsDashboard",
			"5a4da2ab3d5b47c9f9ce864e727b2cb346af1e3ea8b897fe8f704a97ff017619",
			map[string]interface{}{"fetchRewardCampaigns": true}},
		{constants.DropCampaignDetails, "DropCampaignDetails",
			"039277bf98f3130929262cc7c6efd9c141ca3749cb6dca442fc8ead9a53f77c1",
			nil},
	}
	for _, tc := range cases {
		if tc.op.OperationName != tc.name {
			t.Errorf("operation name drifted: got %q want %q", tc.op.OperationName, tc.name)
		}
		if tc.op.Extensions.PersistedQuery.SHA256Hash != tc.hash {
			t.Errorf("%s hash drifted: got %q want %q", tc.name, tc.op.Extensions.PersistedQuery.SHA256Hash, tc.hash)
		}
		if !reflect.DeepEqual(tc.op.Variables, tc.vars) {
			t.Errorf("%s variables drifted: got %#v want %#v", tc.name, tc.op.Variables, tc.vars)
		}
	}
}

// --- applyAccountLinkFilter unit precision + observability -----------------

func TestApplyAccountLinkFilterUnit(t *testing.T) {
	mk := func(id string, conn models.AccountConnection, dist ...string) *models.Campaign {
		c := &models.Campaign{ID: id, Name: id, AccountConnection: conn}
		for i, dt := range dist {
			c.Drops = append(c.Drops, &models.Drop{ID: id + string(rune('a'+i)), Name: dt, BenefitType: models.ParseBenefitType(dt)})
		}
		return c
	}
	d := &DropsTracker{}
	campaigns := []*models.Campaign{
		mk("connected", models.AccountConnectionConnected, "DIRECT_ENTITLEMENT"),
		mk("unknown", models.AccountConnectionUnknown, "DIRECT_ENTITLEMENT"),
		mk("disc-badge", models.AccountConnectionDisconnected, "BADGE"),
		mk("disc-mixed", models.AccountConnectionDisconnected, "DIRECT_ENTITLEMENT", "EMOTE"),
		mk("disc-linked", models.AccountConnectionDisconnected, "DIRECT_ENTITLEMENT", "DIRECT_ENTITLEMENT"),
	}
	kept, excluded := d.applyAccountLinkFilter(campaigns)
	ids := keptIDs(kept)
	if !ids["connected"] || !ids["unknown"] || !ids["disc-badge"] || !ids["disc-mixed"] {
		t.Fatalf("wrong survivors: %v", ids)
	}
	if ids["disc-linked"] {
		t.Fatal("all-linked disconnected campaign must be removed")
	}
	if excluded != 3 { // disc-mixed: 1 DE, disc-linked: 2 DE
		t.Fatalf("excluded rewards = %d, want 3", excluded)
	}
	if mix := campaignByID(kept, "disc-mixed"); mix == nil || len(mix.Drops) != 1 || mix.Drops[0].Name != "EMOTE" {
		t.Fatalf("disc-mixed must keep only the emote, got %v", dropNames(mix))
	}
	// A campaign that arrives already empty (e.g. already-claimed) is left as-is.
	empty := &models.Campaign{ID: "empty", AccountConnection: models.AccountConnectionDisconnected}
	kept2, ex2 := d.applyAccountLinkFilter([]*models.Campaign{empty})
	if len(kept2) != 1 || ex2 != 0 {
		t.Fatalf("empty campaign must be kept untouched, got kept=%d excluded=%d", len(kept2), ex2)
	}
}

// The sync summary reports the aggregate filteredByAccountLink count (privacy-safe:
// campaign name/ID + typed reason only, never account/token/payload data).
func TestAccountLinkFilterLogsAggregateAndReason(t *testing.T) {
	s, d := linkCampaign("cmix", "Mixed", "g1", "Game", "false",
		benefitDrop("d-item", "Rare Skin", "DIRECT_ENTITLEMENT"),
		benefitDrop("d-badge", "Shiny Badge", "BADGE"))
	buf := captureSlog(t)
	client := &fakeDropsClient{
		dashboard: dashboardResponse(s),
		inventory: emptyInventoryResponse(),
		details:   map[string]map[string]interface{}{"cmix": d},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tracker.syncCampaigns()

	log := buf.String()
	if !strings.Contains(log, "filteredRewardsByAccountLink=1") {
		t.Errorf("sync summary must report filteredRewardsByAccountLink=1; log:\n%s", log)
	}
	if !strings.Contains(log, "Excluding account-link-required reward(s)") {
		t.Errorf("expected the mixed-campaign exclusion debug line; log:\n%s", log)
	}
	if !strings.Contains(log, "account_link_required") {
		t.Errorf("expected the typed account_link_required reason in the debug log")
	}
	// Privacy: the log must not leak token/oauth/raw-payload markers.
	for _, banned := range []string{"isAccountConnected", "oauth", "bearer", "token=", "accountLinkURL"} {
		if strings.Contains(strings.ToLower(log), strings.ToLower(banned)) {
			t.Errorf("account-link log leaked %q", banned)
		}
	}
}

// The all-excluded path emits its own distinct DEBUG line (a whole campaign is
// dropped as untrackable) with the typed reason and the correct reward count.
func TestAccountLinkAllExcludedLogsWholeCampaignSkip(t *testing.T) {
	s, d := linkCampaign("conly", "LinkOnly", "g1", "Game", "false",
		benefitDrop("d1", "Skin A", "DIRECT_ENTITLEMENT"),
		benefitDrop("d2", "Skin B", "DIRECT_ENTITLEMENT"))
	buf := captureSlog(t)
	client := &fakeDropsClient{
		dashboard: dashboardResponse(s),
		inventory: emptyInventoryResponse(),
		details:   map[string]map[string]interface{}{"conly": d},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tracker.syncCampaigns()

	log := buf.String()
	if !strings.Contains(log, "Skipping drop campaign: all rewards require a linked publisher account") {
		t.Errorf("expected the all-excluded whole-campaign skip line; log:\n%s", log)
	}
	if !strings.Contains(log, "account_link_required") || !strings.Contains(log, "excludedRewards=2") {
		t.Errorf("all-excluded log must carry the typed reason and excludedRewards=2; log:\n%s", log)
	}
	if len(tracker.Campaigns()) != 0 {
		t.Fatalf("all-linked disconnected campaign must be dropped, got %d", len(tracker.Campaigns()))
	}
}

// asymCampaign builds a summary/detail pair where self.isAccountConnected can be
// placed on the summary and/or the detail independently ("true"/"false"/"" for
// absent), to exercise the buildTrackedCampaign connection backfill. Real Twitch
// responses can be asymmetric (the summary carries no timeBasedDrops), so which
// response carries self is not guaranteed — the backfill must cover both.
func asymCampaign(id, summarySelf, detailSelf string, drops ...map[string]interface{}) (summary, detail map[string]interface{}) {
	now := time.Now()
	game := map[string]interface{}{"id": "g-" + id, "name": "Game " + id}
	withSelf := func(m map[string]interface{}, sel string) {
		switch sel {
		case "true":
			m["self"] = map[string]interface{}{"isAccountConnected": true}
		case "false":
			m["self"] = map[string]interface{}{"isAccountConnected": false}
		}
	}
	summary = map[string]interface{}{
		"id": id, "name": id, "status": "ACTIVE",
		"startAt": rfc3339(now.Add(-2 * time.Hour)), "endAt": rfc3339(now.Add(48 * time.Hour)),
		"game": game,
	}
	withSelf(summary, summarySelf)
	detail = map[string]interface{}{
		"id": id, "name": id, "status": "ACTIVE",
		"startAt": rfc3339(now.Add(-2 * time.Hour)), "endAt": rfc3339(now.Add(48 * time.Hour)),
		"game": game,
	}
	withSelf(detail, detailSelf)
	list := make([]interface{}, 0, len(drops))
	for _, dd := range drops {
		list = append(list, dd)
	}
	detail["timeBasedDrops"] = list
	return summary, detail
}

// TestAccountLinkBackfillFromSummary covers the load-bearing production path: when
// only the dashboard summary carries self.isAccountConnected (the details response
// omits it), buildTrackedCampaign backfills the connection so the filter still
// works — and it never downgrades a value the details response DID carry.
func TestAccountLinkBackfillFromSummary(t *testing.T) {
	// A) details omit self; summary says disconnected -> backfill -> excluded.
	// (Deleting the backfill leaves the campaign Unknown -> tracked, failing this.)
	s, d := asymCampaign("bf-disc", "false", "", benefitDrop("d1", "Skin", "DIRECT_ENTITLEMENT"))
	if got := syncOne(t, s, d); got != nil {
		t.Fatalf("summary-sourced disconnected campaign must be excluded via backfill, got %v", dropNames(got))
	}

	// B) details say connected; summary says disconnected -> details win, no
	// downgrade -> reward kept and connection stays Connected.
	s, d = asymCampaign("bf-conn", "false", "true", benefitDrop("d1", "Skin", "DIRECT_ENTITLEMENT"))
	got := syncOne(t, s, d)
	if got == nil || got.AccountConnection != models.AccountConnectionConnected {
		t.Fatalf("details Connected must win over summary Disconnected (no downgrade), got %v", got)
	}

	// C) details say disconnected; summary absent -> details path works with no backfill.
	s, d = asymCampaign("bf-detail", "", "false", benefitDrop("d1", "Skin", "DIRECT_ENTITLEMENT"))
	if got := syncOne(t, s, d); got != nil {
		t.Fatalf("details-sourced disconnected campaign must be excluded, got %v", dropNames(got))
	}

	// D) neither carries self -> Unknown -> fail open (reward kept).
	s, d = asymCampaign("bf-unknown", "", "", benefitDrop("d1", "Skin", "DIRECT_ENTITLEMENT"))
	if got := syncOne(t, s, d); got == nil || len(got.Drops) != 1 {
		t.Fatalf("campaign with no self anywhere must fail open, got %v", dropNames(got))
	}
}

// I9/I12: the lightweight progress sync never re-filters and never reintroduces a
// reward the full sync already excluded. After a full sync strips a disconnected
// campaign's DIRECT_ENTITLEMENT reward (keeping its badge), a progress sync whose
// inventory lists BOTH rewards must advance only the kept reward's progress and
// must not resurrect the stripped one.
func TestAccountLinkProgressSyncDoesNotRefilter(t *testing.T) {
	s, d := linkCampaign("cmix", "Mixed", "g1", "Game", "false",
		benefitDrop("d-item", "Rare Skin", "DIRECT_ENTITLEMENT"),
		benefitDrop("d-badge", "Shiny Badge", "BADGE"))
	client := &fakeDropsClient{
		dashboard: dashboardResponse(s),
		inventory: emptyInventoryResponse(),
		details:   map[string]map[string]interface{}{"cmix": d},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tracker.syncCampaigns()
	if mix := campaignByID(tracker.Campaigns(), "cmix"); mix == nil || len(mix.Drops) != 1 || mix.Drops[0].Name != "Shiny Badge" {
		t.Fatalf("precondition: mixed campaign must keep only the badge, got %v", dropNames(mix))
	}

	client.inventory = inventoryWithInProgress(map[string]interface{}{
		"id": "cmix",
		"timeBasedDrops": []interface{}{
			inProgressDrop("d-item", "Rare Skin", 60, 30, false),    // stripped DE — must NOT reappear
			inProgressDrop("d-badge", "Shiny Badge", 60, 45, false), // kept badge — progress advances
		},
	})
	tracker.syncProgress()

	mix := campaignByID(tracker.Campaigns(), "cmix")
	if mix == nil || len(mix.Drops) != 1 || mix.Drops[0].Name != "Shiny Badge" {
		t.Fatalf("progress sync must not reintroduce the stripped reward, got %v", dropNames(mix))
	}
	if mix.Drops[0].CurrentMinutesWatched != 45 {
		t.Errorf("kept reward progress must still advance via progress sync, got %d", mix.Drops[0].CurrentMinutesWatched)
	}
}

// Catalog retention (an excluded campaign still lands in the durable "Past"
// catalog) is guaranteed structurally by the SHARED recordCatalog stage, which
// runs once before ALL reward/campaign filters (drops.go: recordCatalog precedes
// applyBlacklist/applyGameFilter/applyAccountLinkFilter). That ordering is already
// guarded by TestForeignCampaignStillCatalogued (a sibling filter at the same
// stage); a reorder that dropped an account-link-excluded campaign from the
// catalog would break that test too. A dedicated account-link catalog test is
// intentionally omitted because database.Open shares one process-wide connection
// across the package's tests, so writing a catalog row from this (alphabetically
// earliest) test file would pollute the isolated catalog_test.go expectations.

// I9: the account-link filter composes with the existing game and blacklist
// filters (AND) without changing either — a foreign campaign is still dropped by
// the game filter, a blacklisted one by the blacklist, a disconnected linked-only
// one by account-link, and an allowed/connected one survives.
func TestAccountLinkComposesWithGameAndBlacklist(t *testing.T) {
	keepS, keepD := linkCampaign("keep", "Keep", "game-wot", "World of Tanks", "true",
		benefitDrop("k1", "Garage Slot", "DIRECT_ENTITLEMENT"))
	blackS, blackD := linkCampaign("black", "EWC WoT", "game-wot", "World of Tanks", "true",
		benefitDrop("b1", "EWC 2026 Bronze", "DIRECT_ENTITLEMENT"))
	forS, forD := linkCampaign("foreign", "War Thunder", "game-wt", "War Thunder", "true",
		benefitDrop("f1", "Rustic Sword", "DIRECT_ENTITLEMENT"))
	acctS, acctD := linkCampaign("acct", "Linked WoT", "game-wot", "World of Tanks", "false",
		benefitDrop("a1", "Premium Tank", "DIRECT_ENTITLEMENT"))

	client := &fakeDropsClient{
		dashboard: dashboardResponse(keepS, blackS, forS, acctS),
		inventory: emptyInventoryResponse(),
		details: map[string]map[string]interface{}{
			"keep": keepD, "black": blackD, "foreign": forD, "acct": acctD,
		},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, []string{"ewc 2026"})
	tracker.UpdateGameFilter([]string{"game-wot"}, nil)
	tracker.syncCampaigns()

	got := keptIDs(tracker.Campaigns())
	if !got["keep"] || got["black"] || got["foreign"] || got["acct"] {
		t.Fatalf("compose wrong: tracked=%v (want only keep)", got)
	}
}
