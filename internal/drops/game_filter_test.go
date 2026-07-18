package drops

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// ---- helpers -------------------------------------------------------------

// gameCampaign builds a minimal campaign carrying only the identity the game
// filter inspects (campaign ID + game). A blank gameID/gameName leaves Game nil.
func gameCampaign(id, gameID, gameName string) *models.Campaign {
	c := &models.Campaign{ID: id, Name: id + "-camp"}
	if gameID != "" || gameName != "" {
		c.Game = &models.Game{ID: gameID, Name: gameName}
	}
	return c
}

func withDisplayName(c *models.Campaign, displayName string) *models.Campaign {
	if c.Game == nil {
		c.Game = &models.Game{}
	}
	c.Game.DisplayName = displayName
	return c
}

func keptIDs(cs []*models.Campaign) map[string]bool {
	m := make(map[string]bool, len(cs))
	for _, c := range cs {
		m[c.ID] = true
	}
	return m
}

// captureSlog redirects the default slog logger to a buffer for the duration of
// the test so a test can assert a specific WARN/INFO line was emitted.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	prev := slog.Default()
	buf := &bytes.Buffer{}
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// claimRecordingClient records every ClaimDrop call and reports success, so a
// test can prove the raw-inventory claim sweep ran regardless of the game
// filter. It reuses fakeDropsClient for PostGQL/GetDropCampaignDetails.
type claimRecordingClient struct {
	*fakeDropsClient
	claimed []string
}

func (c *claimRecordingClient) ClaimDrop(d *models.Drop) (bool, error) {
	c.claimed = append(c.claimed, d.Name)
	return true, nil
}

// ---- T1..T11 : applyGameFilter unit behaviour ---------------------------

// T1: both lists empty -> track all (strict backward-compatible no-op).
func TestGameFilterBothEmptyTracksAll(t *testing.T) {
	d := &DropsTracker{}
	cs := []*models.Campaign{
		gameCampaign("a", "game-wot", "World of Tanks"),
		gameCampaign("b", "game-wt", "War Thunder"),
	}
	kept, filtered := d.applyGameFilter(cs)
	if len(kept) != 2 || filtered != 0 {
		t.Fatalf("empty filter must track all: kept=%d filtered=%d", len(kept), filtered)
	}
}

// T2: strict game-ID allowlist keeps only campaigns with that exact game ID.
func TestGameFilterStrictIDKeepsOnlyMatch(t *testing.T) {
	d := &DropsTracker{}
	d.UpdateGameFilter([]string{"game-wot"}, nil)
	cs := []*models.Campaign{
		gameCampaign("a", "game-wot", "World of Tanks"),
		gameCampaign("b", "game-wt", "War Thunder"),
	}
	kept, filtered := d.applyGameFilter(cs)
	got := keptIDs(kept)
	if !got["a"] || got["b"] || filtered != 1 {
		t.Fatalf("strict ID filter wrong: kept=%v filtered=%d", got, filtered)
	}
}

// T3: game IDs are opaque and compared case-sensitively (no numeric assumption,
// no lowercasing). A non-numeric ID works; a case-mismatched ID does not match.
func TestGameFilterIDIsOpaqueAndCaseSensitive(t *testing.T) {
	d := &DropsTracker{}
	d.UpdateGameFilter([]string{"game-wot"}, nil)
	cs := []*models.Campaign{
		gameCampaign("exact", "game-wot", "World of Tanks"),
		gameCampaign("upper", "GAME-WOT", "World of Tanks"),
	}
	kept, _ := d.applyGameFilter(cs)
	got := keptIDs(kept)
	if !got["exact"] || got["upper"] {
		t.Fatalf("opaque case-sensitive ID match wrong: kept=%v", got)
	}
}

// T4: a configured game name resolves to exactly one ID (from this sync's
// candidates), then filtering is by that ID.
func TestGameFilterNameResolvesToID(t *testing.T) {
	d := &DropsTracker{}
	d.UpdateGameFilter(nil, []string{"World of Tanks"})
	cs := []*models.Campaign{
		gameCampaign("a", "game-wot", "World of Tanks"),
		gameCampaign("b", "game-wt", "War Thunder"),
	}
	kept, filtered := d.applyGameFilter(cs)
	got := keptIDs(kept)
	if !got["a"] || got["b"] || filtered != 1 {
		t.Fatalf("name resolution wrong: kept=%v filtered=%d", got, filtered)
	}
}

// T5: a configured displayName resolves to the same ID as the name.
func TestGameFilterDisplayNameResolvesToID(t *testing.T) {
	d := &DropsTracker{}
	d.UpdateGameFilter(nil, []string{"WoT (display)"})
	wot := withDisplayName(gameCampaign("a", "game-wot", "World of Tanks"), "WoT (display)")
	cs := []*models.Campaign{wot, gameCampaign("b", "game-wt", "War Thunder")}
	kept, _ := d.applyGameFilter(cs)
	got := keptIDs(kept)
	if !got["a"] || got["b"] {
		t.Fatalf("displayName resolution wrong: kept=%v", got)
	}
}

// T6: an ambiguous name (two distinct game IDs share it this sync) adds ALL
// matching IDs (fail-open for the affected games) and logs one WARN. It never
// becomes a global track-all and never picks one ID.
func TestGameFilterAmbiguousNameKeepsAllMatchingIDs(t *testing.T) {
	d := &DropsTracker{}
	d.UpdateGameFilter(nil, []string{"Example Game"})
	buf := captureSlog(t)
	cs := []*models.Campaign{
		gameCampaign("a", "100", "Example Game"),
		gameCampaign("b", "200", "Example Game"),
		gameCampaign("c", "300", "Other Game"),
	}
	kept, filtered := d.applyGameFilter(cs)
	got := keptIDs(kept)
	if !got["a"] || !got["b"] || got["c"] || filtered != 1 {
		t.Fatalf("ambiguous name must keep both matching IDs and drop the other: kept=%v filtered=%d", got, filtered)
	}
	log := buf.String()
	if !strings.Contains(log, "ambiguous") || !strings.Contains(log, "100") || !strings.Contains(log, "200") {
		t.Fatalf("expected one ambiguous WARN naming both IDs, got: %s", log)
	}
}

// T7: an unresolved name does NOT disable an active strict-ID filter.
func TestGameFilterUnresolvedNameDoesNotDisableStrictID(t *testing.T) {
	d := &DropsTracker{}
	d.UpdateGameFilter([]string{"game-wot"}, []string{"Typo Game"})
	cs := []*models.Campaign{
		gameCampaign("a", "game-wot", "World of Tanks"),
		gameCampaign("b", "game-wt", "War Thunder"),
	}
	kept, filtered := d.applyGameFilter(cs)
	got := keptIDs(kept)
	if !got["a"] || got["b"] || filtered != 1 {
		t.Fatalf("unresolved name must not disable strict ID filter: kept=%v filtered=%d", got, filtered)
	}
}

// T8: a name-only configuration that resolves to nothing fails open to track-all
// this cycle, with one WARN — never a blind deny-all.
func TestGameFilterNameOnlyUnresolvedTracksAllWithWarn(t *testing.T) {
	d := &DropsTracker{}
	d.UpdateGameFilter(nil, []string{"Nonexistent Game"})
	buf := captureSlog(t)
	cs := []*models.Campaign{
		gameCampaign("a", "game-wot", "World of Tanks"),
		gameCampaign("b", "game-wt", "War Thunder"),
	}
	kept, filtered := d.applyGameFilter(cs)
	if len(kept) != 2 || filtered != 0 {
		t.Fatalf("name-only-unresolved must track all: kept=%d filtered=%d", len(kept), filtered)
	}
	if !strings.Contains(buf.String(), "no game names resolved") {
		t.Fatalf("expected a name-only fail-open WARN, got: %s", buf.String())
	}
}

// T9: two distinct game IDs whose names would collapse to the same string under
// whitespace-collapsing are NOT merged. Name matching is whole-string (case-fold
// only), so "Example Game" resolves to id 200 alone, never id 100.
func TestGameFilterDistinctIDsNotMergedByCollapsedName(t *testing.T) {
	d := &DropsTracker{}
	d.UpdateGameFilter(nil, []string{"Example Game"})
	cs := []*models.Campaign{
		gameCampaign("dbl", "100", "Example  Game"), // double space
		gameCampaign("sgl", "200", "Example Game"),  // single space
	}
	kept, _ := d.applyGameFilter(cs)
	got := keptIDs(kept)
	if got["dbl"] || !got["sgl"] {
		t.Fatalf("collapsed-name merge must not happen: kept=%v (want only sgl/200)", got)
	}
}

// T10: with a strict ID of "100", a campaign whose game ID is "200" never
// passes, even though the two games' names collapse to the same string.
func TestGameFilterStrictIDIgnoresNameCollision(t *testing.T) {
	d := &DropsTracker{}
	d.UpdateGameFilter([]string{"100"}, nil)
	cs := []*models.Campaign{
		gameCampaign("a", "100", "Example  Game"),
		gameCampaign("b", "200", "Example Game"),
	}
	kept, filtered := d.applyGameFilter(cs)
	got := keptIDs(kept)
	if !got["a"] || got["b"] || filtered != 1 {
		t.Fatalf("strict ID 100 must not admit ID 200: kept=%v filtered=%d", got, filtered)
	}
}

// T11: "World of Tanks" must not admit "World of Tanks Blitz" (a different game
// with a different ID) — whole-string match, no prefix/substring.
func TestGameFilterNameDoesNotSwallowLongerName(t *testing.T) {
	d := &DropsTracker{}
	d.UpdateGameFilter(nil, []string{"World of Tanks"})
	cs := []*models.Campaign{
		gameCampaign("wot", "game-wot", "World of Tanks"),
		gameCampaign("blitz", "game-blitz", "World of Tanks Blitz"),
	}
	kept, filtered := d.applyGameFilter(cs)
	got := keptIDs(kept)
	if !got["wot"] || got["blitz"] || filtered != 1 {
		t.Fatalf("name must be whole-string: kept=%v filtered=%d", got, filtered)
	}
}

// T(missing-id): a campaign with no game ID is kept (fail-open) even under an
// active filter — its identity is unknown, never proof it is foreign.
func TestGameFilterMissingGameIDKept(t *testing.T) {
	d := &DropsTracker{}
	d.UpdateGameFilter([]string{"game-wot"}, nil)
	buf := captureSlog(t)
	cs := []*models.Campaign{
		gameCampaign("known", "game-wot", "World of Tanks"),
		gameCampaign("nogame", "", ""), // Game nil
		{ID: "emptyid", Name: "x", Game: &models.Game{ID: "", Name: "Mystery"}},
	}
	kept, _ := d.applyGameFilter(cs)
	got := keptIDs(kept)
	if !got["known"] || !got["nogame"] || !got["emptyid"] {
		t.Fatalf("campaigns with no game ID must be kept: kept=%v", got)
	}
	if !strings.Contains(buf.String(), "missing_game_id") {
		t.Fatalf("expected a missing_game_id diagnostic, got: %s", buf.String())
	}
}

// ---- T12..T15, T20, T21 : pipeline behaviour ----------------------------

// dashCampaign returns a ViewerDropsDashboard summary and its matching
// DropCampaignDetails response for one active campaign with a single in-window,
// unclaimed drop — enough for buildTrackedCampaign to track it.
func dashCampaign(id, name, gameID, gameName, dropID, dropName string) (summary, detail map[string]interface{}) {
	now := time.Now()
	game := map[string]interface{}{"id": gameID, "name": gameName}
	summary = map[string]interface{}{
		"id":      id,
		"name":    name,
		"status":  "ACTIVE",
		"startAt": rfc3339(now.Add(-2 * time.Hour)),
		"endAt":   rfc3339(now.Add(48 * time.Hour)),
		"game":    game,
	}
	detail = map[string]interface{}{
		"id":             id,
		"name":           name,
		"status":         "ACTIVE",
		"startAt":        rfc3339(now.Add(-2 * time.Hour)),
		"endAt":          rfc3339(now.Add(48 * time.Hour)),
		"game":           game,
		"timeBasedDrops": []interface{}{activeDrop(dropID, dropName, 60)},
	}
	return summary, detail
}

// foreignInProgress builds an inventory dropCampaignsInProgress entry for a
// foreign game with one in-progress (not-yet-claimable) drop.
func foreignInProgress(id, name, gameID, gameName string) map[string]interface{} {
	return map[string]interface{}{
		"id":   id,
		"name": name,
		"game": map[string]interface{}{"id": gameID, "name": gameName},
		"timeBasedDrops": []interface{}{
			inProgressDrop("d-"+id, name+" Reward", 120, 60, false),
		},
	}
}

// T12 + T20 (prod scenario 18.07): dashboard empty, a foreign campaign recovered
// from inventory, strict filter = WoT only. The foreign campaign is excluded
// from the tracked set / Current, while recovered stays raw (pre-filter).
func TestSyncFiltersForeignRecoveredCampaign(t *testing.T) {
	client := &fakeDropsClient{
		dashboard: dashboardResponse(),
		inventory: inventoryWithInProgress(
			foreignInProgress("campaign-wt", "Challenger League Major", "game-warthunder", "War Thunder"),
		),
		details: map[string]map[string]interface{}{},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tracker.UpdateGameFilter([]string{"game-wot"}, nil)
	tracker.syncCampaigns()

	if got := tracker.Campaigns(); len(got) != 0 {
		t.Fatalf("foreign recovered campaign must not be tracked, got %d", len(got))
	}
	st := tracker.SyncStatus()
	if st.TrackedCampaigns != 0 {
		t.Errorf("expected tracked=0, got %d", st.TrackedCampaigns)
	}
	if st.RecoveredCampaigns != 1 {
		t.Errorf("recovered count is pre-filter raw inventory recovery; expected 1, got %d", st.RecoveredCampaigns)
	}
}

// T13: dashboard-origin and inventory-recovered campaigns both pass through the
// single applyGameFilter; only the allowed game survives from either source.
func TestSyncFilterAppliesToDashboardAndRecovery(t *testing.T) {
	wotSummary, wotDetail := dashCampaign("campaign-wot", "WoT AMD Drops", "game-wot", "World of Tanks", "wd", "Garage Slot")
	xSummary, xDetail := dashCampaign("campaign-x", "Foreign Dashboard", "game-x", "Assassin's Creed", "xd", "Rustic Sword")

	client := &fakeDropsClient{
		dashboard: dashboardResponse(wotSummary, xSummary),
		inventory: inventoryWithInProgress(
			foreignInProgress("campaign-y", "Foreign Recovered", "game-y", "World of Warships"),
		),
		details: map[string]map[string]interface{}{
			"campaign-wot": wotDetail,
			"campaign-x":   xDetail,
		},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tracker.UpdateGameFilter([]string{"game-wot"}, nil)
	tracker.syncCampaigns()

	got := keptIDs(tracker.Campaigns())
	if !got["campaign-wot"] || got["campaign-x"] || got["campaign-y"] {
		t.Fatalf("single filter must gate both sources: tracked=%v (want only campaign-wot)", got)
	}
}

// T14 + M5 guard: the raw-inventory claim sweep (claimAllDropsFromInventory) is
// never gated by the game filter — a claimable reward for a filtered-out foreign
// game is still claimed. Exercised directly so exactly one claim path runs
// (via syncCampaigns the recovery SyncDrops would also claim it). (Slow: the
// production sweep sleeps 5s after a claim, so this is excluded from -count runs.
// Foreign campaigns being excluded from the tracked set is covered by T12.)
func TestClaimSweepIgnoresGameFilter(t *testing.T) {
	foreign := map[string]interface{}{
		"id":   "campaign-wt",
		"name": "Challenger League Major",
		"game": map[string]interface{}{"id": "game-warthunder", "name": "War Thunder"},
		"timeBasedDrops": []interface{}{
			// dropInstanceID present + watched>=required + unclaimed => claimable.
			map[string]interface{}{
				"id":                     "d-claim",
				"name":                   "Rustic Sword",
				"requiredMinutesWatched": float64(120),
				"self": map[string]interface{}{
					"currentMinutesWatched": float64(120),
					"dropInstanceID":        "inst-1",
					"isClaimed":             false,
				},
			},
		},
	}
	client := &claimRecordingClient{fakeDropsClient: &fakeDropsClient{
		inventory: inventoryWithInProgress(foreign),
	}}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tracker.UpdateGameFilter([]string{"game-wot"}, nil) // foreign game excluded from tracking
	tracker.claimAllDropsFromInventory()                // the raw-inventory sweep, run in isolation

	if len(client.claimed) != 1 || client.claimed[0] != "Rustic Sword" {
		t.Fatalf("foreign claimable reward must still be claimed regardless of filter, got %v", client.claimed)
	}
}

// T15: the blacklist and the game filter compose (AND) without either changing
// the other — a blacklisted WoT campaign is dropped by the blacklist, a foreign
// campaign by the game filter, an allowed non-blacklisted WoT campaign survives.
func TestBlacklistAndGameFilterCompose(t *testing.T) {
	keepS, keepD := dashCampaign("keep", "Keep WoT", "game-wot", "World of Tanks", "kd", "Garage Slot")
	blackS, blackD := dashCampaign("black", "EWC WoT", "game-wot", "World of Tanks", "bd", "EWC 2026 Bronze")
	forS, forD := dashCampaign("foreign", "War Thunder Camp", "game-wt", "War Thunder", "fd", "Rustic Sword")

	client := &fakeDropsClient{
		dashboard: dashboardResponse(keepS, blackS, forS),
		inventory: emptyInventoryResponse(),
		details: map[string]map[string]interface{}{
			"keep": keepD, "black": blackD, "foreign": forD,
		},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, []string{"ewc 2026"})
	tracker.UpdateGameFilter([]string{"game-wot"}, nil)
	buf := captureSlog(t)
	tracker.syncCampaigns()

	got := keptIDs(tracker.Campaigns())
	if !got["keep"] || got["black"] || got["foreign"] {
		t.Fatalf("compose wrong: tracked=%v (want only keep)", got)
	}
	log := buf.String()
	if !strings.Contains(log, "matched drop-name blacklist") {
		t.Errorf("expected blacklist skip log for the EWC campaign")
	}
	if !strings.Contains(log, "game not in configured drop-campaign game list") {
		t.Errorf("expected game-filter skip log for the foreign campaign")
	}
}

// T21: a foreign campaign is still recorded in the durable catalog (the "Past"
// data source) even though the game filter excludes it from the tracked set —
// recordCatalog runs before the filter.
func TestForeignCampaignStillCatalogued(t *testing.T) {
	cat := newTestCatalog(t)
	client := &fakeDropsClient{
		dashboard: dashboardResponse(),
		inventory: inventoryWithInProgress(
			foreignInProgress("campaign-wt", "Challenger League Major", "game-warthunder", "War Thunder"),
		),
		details: map[string]map[string]interface{}{},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tracker.SetCatalog(cat)
	tracker.UpdateGameFilter([]string{"game-wot"}, nil)
	tracker.syncCampaigns()

	if got := tracker.Campaigns(); len(got) != 0 {
		t.Fatalf("foreign campaign must be excluded from Current, got %d", len(got))
	}
	var name, game string
	err := cat.db.QueryRow("SELECT name, game FROM drop_campaigns WHERE campaign_id = ?", "campaign-wt").Scan(&name, &game)
	if err != nil {
		t.Fatalf("foreign campaign must survive in the catalog, query err: %v", err)
	}
	if name != "Challenger League Major" || game != "War Thunder" {
		t.Fatalf("catalog row wrong: name=%q game=%q", name, game)
	}
}

func streamerHasCampaign(s *models.Streamer, id string) bool {
	for _, c := range s.Stream.GetCampaigns() {
		if c.ID == id {
			return true
		}
	}
	return false
}

// M1: a runtime filter change must clear a stale channel-restricted assignment
// from the STREAMER (not just d.campaigns), so the broker stops classifying the
// slot as restricted_drop. Drives the real updateStreamerCampaigns via
// syncCampaigns and asserts Stream.GetCampaigns() and HasChannelRestrictedCampaign()
// — broker.classify (internal/watcher/broker.go) keys reason=restricted_drop off
// exactly that method, so clearing it removes the restricted_drop reason without
// importing the watcher.
func TestGameFilterRuntimeUpdateClearsRestrictedDropAssignment(t *testing.T) {
	game := &models.Game{ID: "game-foreign", Name: "War Thunder"}
	streamer := models.NewStreamer("wt_streamer", models.StreamerSettings{ClaimDrops: true})
	streamer.ChannelID = "chan-foreign"
	streamer.IsOnline = true
	streamer.Stream.Game = game
	streamer.Stream.CampaignIDs = []string{"campaign-wt"}

	// Foreign channel-restricted campaign recovered from inventory, restricted to
	// this streamer's channel.
	prog := map[string]interface{}{
		"id":    "campaign-wt",
		"name":  "Challenger League Major",
		"game":  map[string]interface{}{"id": "game-foreign", "name": "War Thunder"},
		"allow": map[string]interface{}{"channels": []interface{}{map[string]interface{}{"id": "chan-foreign"}}},
		"timeBasedDrops": []interface{}{
			inProgressDrop("d-wt", "Challenger Reward", 120, 60, false),
		},
	}
	client := &fakeDropsClient{
		dashboard: dashboardResponse(),
		inventory: inventoryWithInProgress(prog),
		details:   map[string]map[string]interface{}{},
	}
	tracker := NewDropsTracker(client, []*models.Streamer{streamer}, config.RateLimitSettings{}, nil)

	// No filter: foreign restricted campaign is tracked AND assigned -> the broker
	// would classify this streamer's slot as restricted_drop.
	tracker.syncCampaigns()
	if got := keptIDs(tracker.Campaigns()); !got["campaign-wt"] {
		t.Fatalf("precondition: foreign campaign must be tracked without a filter, got %v", got)
	}
	if !streamerHasCampaign(streamer, "campaign-wt") {
		t.Fatal("precondition: foreign campaign must be assigned to the streamer's stream")
	}
	if !streamer.HasChannelRestrictedCampaign() {
		t.Fatal("precondition: streamer must hold a channel-restricted campaign (broker restricted_drop input)")
	}

	// Runtime filter change to WoT-only, then a normal sync.
	tracker.UpdateGameFilter([]string{"game-wot"}, nil)
	tracker.syncCampaigns()

	if got := keptIDs(tracker.Campaigns()); got["campaign-wt"] {
		t.Fatalf("foreign campaign must leave d.campaigns after the filter, got %v", got)
	}
	if streamerHasCampaign(streamer, "campaign-wt") {
		t.Fatal("stale foreign campaign must be cleared from the streamer's stream after the filter")
	}
	if streamer.HasChannelRestrictedCampaign() {
		t.Fatal("restricted_drop must clear: HasChannelRestrictedCampaign() still true after the filter update")
	}
}

// claimEvent records one ClaimDrop call's identity.
type claimEvent struct{ DropID, InstanceID string }

// orderedClaimClient records the ORDER of GQL operations and claims, and — after
// the first successful claim — advances the inventory it returns to
// isClaimed=true, mimicking Twitch propagating the claimed state on the next
// fetch. This lets the test pin that the raw-inventory claim sweep runs BEFORE
// the dashboard/filter, WITHOUT asserting a second (recovery) claim of the same
// reward: a future dedup that stops re-claiming an already-claimed drop must not
// break this test.
type orderedClaimClient struct {
	mu        sync.Mutex
	events    []string
	claims    []claimEvent
	claimed   bool
	invBefore map[string]interface{}
	invAfter  map[string]interface{}
	dashboard map[string]interface{}
	details   map[string]map[string]interface{}
}

func (c *orderedClaimClient) PostGQL(op constants.GQLOperation) (map[string]interface{}, error) {
	c.mu.Lock()
	c.events = append(c.events, op.OperationName)
	claimed := c.claimed
	c.mu.Unlock()
	switch op.OperationName {
	case "ViewerDropsDashboard":
		return c.dashboard, nil
	case "Inventory":
		if claimed {
			return c.invAfter, nil // Twitch now reports the reward as claimed
		}
		return c.invBefore, nil
	default:
		return map[string]interface{}{}, nil
	}
}

func (c *orderedClaimClient) GetDropCampaignDetails(id string) (map[string]interface{}, error) {
	c.mu.Lock()
	c.events = append(c.events, "Details")
	c.mu.Unlock()
	return c.details[id], nil
}

func (c *orderedClaimClient) ClaimDrop(drop *models.Drop) (bool, error) {
	c.mu.Lock()
	c.events = append(c.events, "Claim")
	c.claims = append(c.claims, claimEvent{DropID: drop.ID, InstanceID: drop.DropInstanceID})
	c.claimed = true
	c.mu.Unlock()
	return true, nil
}

func firstIndex(events []string, s string) int {
	for i, e := range events {
		if e == s {
			return i
		}
	}
	return -1
}

// foreignInProgressWithClaim builds the inventory entry for the foreign campaign
// with a claimable reward (claimed toggles its isClaimed) plus a still-in-progress
// drop that keeps the campaign recoverable.
func foreignInProgressWithClaim(claimed bool) map[string]interface{} {
	return map[string]interface{}{
		"id":   "campaign-wt",
		"name": "Challenger League Major",
		"game": map[string]interface{}{"id": "game-foreign", "name": "War Thunder"},
		"timeBasedDrops": []interface{}{
			map[string]interface{}{
				"id":                     "d-claim",
				"name":                   "Rustic Sword",
				"requiredMinutesWatched": float64(120),
				"self": map[string]interface{}{
					"currentMinutesWatched": float64(120),
					"dropInstanceID":        "inst-1",
					"isClaimed":             claimed,
				},
			},
			inProgressDrop("d-prog", "Longbow", 120, 60, false),
		},
	}
}

// M2: claim-before-filter through the REAL syncCampaigns pipeline. The
// raw-inventory claim sweep claims a foreign reward BEFORE the dashboard listing
// (before candidates are built and the game filter runs); exactly one successful
// claim is required (state propagation makes the later inventory fetch report
// it claimed, so recovery does not re-claim), and the foreign campaign is still
// filtered out of the tracked set. Fails if the sweep is skipped, moved after
// the dashboard/filter, or gated by allowed game IDs. (Slow: the sweep sleeps 5s
// after a claim.)
func TestSyncClaimsForeignRewardBeforeGameFilter(t *testing.T) {
	client := &orderedClaimClient{
		invBefore: inventoryWithInProgress(foreignInProgressWithClaim(false)),
		invAfter:  inventoryWithInProgress(foreignInProgressWithClaim(true)),
		dashboard: dashboardResponse(),
		details:   map[string]map[string]interface{}{},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tracker.UpdateGameFilter([]string{"game-wot"}, nil) // foreign game excluded from tracking
	tracker.syncCampaigns()

	// Exactly one successful claim, of the claimable reward (never the
	// in-progress drop, never a second claim of the same reward).
	if len(client.claims) != 1 {
		t.Fatalf("expected exactly one claim, got %d (%+v)", len(client.claims), client.claims)
	}
	if client.claims[0].DropID != "d-claim" || client.claims[0].InstanceID != "inst-1" {
		t.Fatalf("unexpected claimed drop: %+v", client.claims[0])
	}

	// The claim happened in the raw-inventory sweep — the first inventory fetch
	// precedes the claim, and the claim precedes the first dashboard listing
	// (i.e. before candidates are built and the game filter runs).
	invAt := firstIndex(client.events, "Inventory")
	claimAt := firstIndex(client.events, "Claim")
	dashAt := firstIndex(client.events, "ViewerDropsDashboard")
	if invAt < 0 || claimAt < 0 || dashAt < 0 || invAt > claimAt || claimAt >= dashAt {
		t.Fatalf("claim must occur in the raw inventory sweep, before the first ViewerDropsDashboard; events=%v", client.events)
	}

	// ...and the foreign campaign is still filtered out of the tracked set.
	if got := tracker.Campaigns(); len(got) != 0 {
		t.Fatalf("foreign campaign must be filtered from d.campaigns, got %d", len(got))
	}
	if tracker.SyncStatus().TrackedCampaigns != 0 {
		t.Fatalf("tracked must be 0 after filtering the foreign campaign")
	}
}

// T19: a runtime UpdateGameFilter (the Settings-page path) takes effect on the
// next full sync without a restart — no new goroutine, no restart.
func TestGameFilterRuntimeUpdateChangesTrackedSet(t *testing.T) {
	wotS, wotD := dashCampaign("campaign-wot", "WoT Drops", "game-wot", "World of Tanks", "wd", "Garage Slot")
	forS, forD := dashCampaign("campaign-x", "Foreign", "game-x", "War Thunder", "fd", "Rustic Sword")
	client := &fakeDropsClient{
		dashboard: dashboardResponse(wotS, forS),
		inventory: emptyInventoryResponse(),
		details:   map[string]map[string]interface{}{"campaign-wot": wotD, "campaign-x": forD},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)

	// No filter yet: both games tracked.
	tracker.syncCampaigns()
	if got := keptIDs(tracker.Campaigns()); !got["campaign-wot"] || !got["campaign-x"] {
		t.Fatalf("no filter must track both, got %v", got)
	}

	// Runtime change to WoT-only: the next sync drops the foreign campaign.
	tracker.UpdateGameFilter([]string{"game-wot"}, nil)
	tracker.syncCampaigns()
	got := keptIDs(tracker.Campaigns())
	if !got["campaign-wot"] || got["campaign-x"] {
		t.Fatalf("runtime filter update must drop foreign on next sync, got %v", got)
	}
}
