package drops

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// fakeUpcomingNotifier records every campaign the tracker forwards for an alert,
// so tests can assert exactly which campaigns (and how many times) trigger a
// notification on the edge of a sync.
type fakeUpcomingNotifier struct {
	mu        sync.Mutex
	campaigns []*models.Campaign
}

func (f *fakeUpcomingNotifier) NotifyUpcomingCampaign(_ context.Context, c *models.Campaign) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.campaigns = append(f.campaigns, c)
}

func (f *fakeUpcomingNotifier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.campaigns)
}

func (f *fakeUpcomingNotifier) ids() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.campaigns))
	for _, c := range f.campaigns {
		out = append(out, c.ID)
	}
	return out
}

// campaignSummaryDetail builds a matching dashboard summary + details pair for a
// campaign with the given window and game identity.
func campaignSummaryDetail(id, name, status, gameID, gameName string, start, end time.Time) (summary, detail map[string]interface{}) {
	game := map[string]interface{}{"id": gameID, "name": gameName}
	summary = map[string]interface{}{
		"id": id, "name": name, "status": status,
		"startAt": rfc3339(start), "endAt": rfc3339(end), "game": game,
	}
	detail = map[string]interface{}{
		"id": id, "name": name, "status": status,
		"startAt": rfc3339(start), "endAt": rfc3339(end), "game": game,
		"timeBasedDrops": []interface{}{activeDrop(id+"-drop", name+" Reward", 60)},
	}
	return summary, detail
}

func futurePair(id, name, gameID, gameName string) (map[string]interface{}, map[string]interface{}) {
	return campaignSummaryDetail(id, name, "UPCOMING", gameID, gameName, nowPlusHours(48), nowPlusHours(96))
}
func activePair(id, name, gameID, gameName string) (map[string]interface{}, map[string]interface{}) {
	return campaignSummaryDetail(id, name, "ACTIVE", gameID, gameName, nowMinusHours(2), nowPlusHours(48))
}
func endedPair(id, name, gameID, gameName string) (map[string]interface{}, map[string]interface{}) {
	return campaignSummaryDetail(id, name, "EXPIRED", gameID, gameName, nowMinusHours(96), nowMinusHours(2))
}

// Tests 2 + 3: active goes to the active set (not upcoming); ended goes to
// neither the active nor the upcoming set.
func TestSyncClassifiesActiveEndedUpcoming(t *testing.T) {
	aS, aD := activePair("act", "Active", "27546", "World of Tanks")
	eS, eD := endedPair("end", "Ended", "27546", "World of Tanks")
	uS, uD := futurePair("fut", "Future", "27546", "World of Tanks")
	client := &fakeDropsClient{
		dashboard: dashboardResponse(aS, eS, uS),
		inventory: emptyInventoryResponse(),
		details:   map[string]map[string]interface{}{"act": aD, "end": eD, "fut": uD},
	}
	tr := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tr.syncCampaigns()

	active := keptIDs(tr.Campaigns())
	if !active["act"] || active["end"] || active["fut"] {
		t.Fatalf("active set must hold only the active campaign, got %v", active)
	}
	up := keptIDs(tr.UpcomingCampaigns())
	if !up["fut"] || up["act"] || up["end"] {
		t.Fatalf("upcoming set must hold only the future campaign, got %v", up)
	}
}

// Test 4: a future campaign never enters the active farm set and is never
// assigned to a streamer (nor, therefore, given watch priority or registered
// with the progress watchdog, which both read the active set).
func TestFutureCampaignNotAssignedToStreamer(t *testing.T) {
	uS, uD := futurePair("fut", "Future", "27546", "World of Tanks")
	streamer := models.NewStreamer("wot_streamer", models.StreamerSettings{ClaimDrops: true})
	streamer.ChannelID = "chan-1"
	streamer.SetConfirmedOnline()
	streamer.Stream.Game = &models.Game{ID: "27546", Name: "World of Tanks"}
	streamer.Stream.SetCampaignIDs([]string{"fut"})

	client := &fakeDropsClient{
		dashboard: dashboardResponse(uS),
		inventory: emptyInventoryResponse(),
		details:   map[string]map[string]interface{}{"fut": uD},
	}
	tr := NewDropsTracker(client, []*models.Streamer{streamer}, config.RateLimitSettings{}, nil)
	tr.syncCampaigns()

	if len(tr.Campaigns()) != 0 {
		t.Fatalf("future campaign must not enter the active farm set")
	}
	if streamerHasCampaign(streamer, "fut") {
		t.Fatalf("future campaign must never be assigned to a streamer")
	}
}

// Test 6: a full-sync error preserves the previous cached upcoming snapshot,
// records the error, and does not notify.
func TestUpcomingCachedOnSyncError(t *testing.T) {
	uS, uD := futurePair("fut", "Future", "27546", "World of Tanks")
	client := &fakeDropsClient{
		dashboard: dashboardResponse(uS),
		inventory: emptyInventoryResponse(),
		details:   map[string]map[string]interface{}{"fut": uD},
	}
	notifier := &fakeUpcomingNotifier{}
	tr := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tr.UpdateGameFilter([]string{"27546"}, nil)
	tr.SetUpcomingNotifier(notifier)

	tr.syncCampaigns() // success: populate upcoming + notify once
	if !keptIDs(tr.UpcomingCampaigns())["fut"] {
		t.Fatalf("precondition: upcoming must be populated")
	}
	if notifier.count() != 1 {
		t.Fatalf("precondition: exactly one notify, got %d", notifier.count())
	}

	// A stale persisted-query hash aborts the whole sync.
	client.detailsErr = api.ErrPersistedQueryNotFound
	tr.syncCampaigns()

	if !keptIDs(tr.UpcomingCampaigns())["fut"] {
		t.Fatalf("failed sync must preserve the cached upcoming snapshot")
	}
	if notifier.count() != 1 {
		t.Fatalf("failed sync must not notify, got %d", notifier.count())
	}
	if tr.SyncStatus().LastError == "" {
		t.Fatalf("failed sync must record a last error")
	}
}

// Test 7: a successful sync that returns nothing honestly clears the upcoming set.
func TestUpcomingClearedOnEmptySuccess(t *testing.T) {
	uS, uD := futurePair("fut", "Future", "27546", "World of Tanks")
	client := &fakeDropsClient{
		dashboard: dashboardResponse(uS),
		inventory: emptyInventoryResponse(),
		details:   map[string]map[string]interface{}{"fut": uD},
	}
	tr := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tr.syncCampaigns()
	if len(tr.UpcomingCampaigns()) != 1 {
		t.Fatalf("precondition: one upcoming campaign")
	}

	client.dashboard = dashboardResponse() // Twitch now reports nothing
	tr.syncCampaigns()
	if len(tr.UpcomingCampaigns()) != 0 {
		t.Fatalf("successful empty sync must honestly clear the upcoming set")
	}
}

// Tests 16 + 18: strict game ID surfaces the relevant upcoming campaign; a
// foreign game is hidden from the relevant list (but still catalogued).
func TestRelevantUpcomingByStrictGameID(t *testing.T) {
	uS, uD := futurePair("wot", "WoT Future", "27546", "World of Tanks")
	fS, fD := futurePair("foreign", "Foreign Future", "999", "War Thunder")
	client := &fakeDropsClient{
		dashboard: dashboardResponse(uS, fS),
		inventory: emptyInventoryResponse(),
		details:   map[string]map[string]interface{}{"wot": uD, "foreign": fD},
	}
	tr := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tr.UpdateGameFilter([]string{"27546"}, []string{"World of Tanks"})
	tr.syncCampaigns()

	rel := keptIDs(tr.RelevantUpcomingCampaigns())
	if !rel["wot"] {
		t.Fatalf("strict game ID 27546 must surface the WoT upcoming campaign, got %v", rel)
	}
	if rel["foreign"] {
		t.Fatalf("foreign game must be hidden from the relevant upcoming list, got %v", rel)
	}
	if !keptIDs(tr.UpcomingCampaigns())["foreign"] {
		t.Fatalf("the full upcoming set should still hold the foreign campaign")
	}
}

// Test 17: a configured game NAME that does not resolve this sync must not lose
// a campaign whose strict game ID matches.
func TestRelevantUpcomingUnresolvedNameStrictIDKept(t *testing.T) {
	// Candidate game name "WoT RU" won't resolve the filter name "World of Tanks",
	// but the strict ID 27546 matches.
	uS, uD := futurePair("wot", "WoT Future", "27546", "WoT RU")
	client := &fakeDropsClient{
		dashboard: dashboardResponse(uS),
		inventory: emptyInventoryResponse(),
		details:   map[string]map[string]interface{}{"wot": uD},
	}
	tr := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tr.UpdateGameFilter([]string{"27546"}, []string{"World of Tanks"})
	tr.syncCampaigns()

	if !keptIDs(tr.RelevantUpcomingCampaigns())["wot"] {
		t.Fatalf("a strict-ID match must keep the campaign even when the name does not resolve")
	}
}

// Test 19: a campaign with no game ID follows the fail-open policy — never
// silently dropped from the relevant list.
func TestRelevantUpcomingMissingGameIDFailOpen(t *testing.T) {
	uS, uD := futurePair("noid", "No Game ID", "", "")
	client := &fakeDropsClient{
		dashboard: dashboardResponse(uS),
		inventory: emptyInventoryResponse(),
		details:   map[string]map[string]interface{}{"noid": uD},
	}
	tr := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tr.UpdateGameFilter([]string{"27546"}, nil)
	tr.syncCampaigns()

	if !keptIDs(tr.RelevantUpcomingCampaigns())["noid"] {
		t.Fatalf("a campaign with no game ID must be kept (fail-open), not silently dropped")
	}
}

// Tests 21 + 27: a successful full sync notifies exactly once per relevant
// upcoming campaign, and never for a foreign one.
func TestNotifierCalledOncePerRelevantUpcoming(t *testing.T) {
	uS, uD := futurePair("wot", "WoT Future", "27546", "World of Tanks")
	fS, fD := futurePair("foreign", "Foreign", "999", "War Thunder")
	client := &fakeDropsClient{
		dashboard: dashboardResponse(uS, fS),
		inventory: emptyInventoryResponse(),
		details:   map[string]map[string]interface{}{"wot": uD, "foreign": fD},
	}
	notifier := &fakeUpcomingNotifier{}
	tr := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tr.UpdateGameFilter([]string{"27546"}, nil)
	tr.SetUpcomingNotifier(notifier)
	tr.syncCampaigns()

	if got := notifier.ids(); len(got) != 1 || got[0] != "wot" {
		t.Fatalf("notifier must fire once, only for the relevant upcoming campaign, got %v", got)
	}
}

// Test 6 (notification half): a failed full sync must not notify.
func TestNotifierNotCalledOnFailedSync(t *testing.T) {
	uS, uD := futurePair("wot", "WoT", "27546", "World of Tanks")
	client := &fakeDropsClient{
		dashboard:  dashboardResponse(uS),
		inventory:  emptyInventoryResponse(),
		details:    map[string]map[string]interface{}{"wot": uD},
		detailsErr: api.ErrPersistedQueryNotFound,
	}
	notifier := &fakeUpcomingNotifier{}
	tr := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tr.SetUpcomingNotifier(notifier)
	tr.syncCampaigns()

	if notifier.count() != 0 {
		t.Fatalf("a failed full sync must not notify, got %d", notifier.count())
	}
}

// Test 23 + 24: the lightweight progress sync must never notify, even with a
// populated upcoming set that a full sync already alerted on.
func TestNotifierNotCalledFromLightweightSync(t *testing.T) {
	uS, uD := futurePair("wot", "WoT", "27546", "World of Tanks")
	client := &fakeDropsClient{
		dashboard: dashboardResponse(uS),
		inventory: emptyInventoryResponse(),
		details:   map[string]map[string]interface{}{"wot": uD},
	}
	notifier := &fakeUpcomingNotifier{}
	tr := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tr.SetUpcomingNotifier(notifier)

	tr.syncCampaigns() // full sync populates upcoming and notifies once
	before := notifier.count()
	if before != 1 {
		t.Fatalf("precondition: the full sync must notify once, got %d", before)
	}

	tr.syncProgress() // lightweight, inventory-only refresh
	if notifier.count() != before {
		t.Fatalf("the lightweight progress sync must never notify, got %d (was %d)", notifier.count(), before)
	}
}

// Test 28: active or ended campaigns never notify.
func TestNotifierNotCalledForActiveOrEnded(t *testing.T) {
	aS, aD := activePair("act", "Active", "27546", "World of Tanks")
	eS, eD := endedPair("end", "Ended", "27546", "World of Tanks")
	client := &fakeDropsClient{
		dashboard: dashboardResponse(aS, eS),
		inventory: emptyInventoryResponse(),
		details:   map[string]map[string]interface{}{"act": aD, "end": eD},
	}
	notifier := &fakeUpcomingNotifier{}
	tr := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tr.UpdateGameFilter([]string{"27546"}, nil)
	tr.SetUpcomingNotifier(notifier)
	tr.syncCampaigns()

	if notifier.count() != 0 {
		t.Fatalf("active/ended campaigns must not notify, got %d", notifier.count())
	}
}
