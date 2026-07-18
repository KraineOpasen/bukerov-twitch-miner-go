package drops

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// This file pins the v0.13.7 Drops log-spam hotfix (§4): the duplicate
// diagnostic INFO blocks emitted in pairs came from CASE A — two equivalent
// FULL syncs run for one logical trigger. The fixes are (1) UpdateBlacklist no
// longer self-triggers a resync (its sole caller pairs it with UpdateGameFilter,
// which triggers the single resync), (2) loop() drops any campaignResync buffered
// before it started (the construction-time UpdateGameFilter seed), (3) the stable
// per-campaign filter decisions moved to DEBUG, and (4) the "Drops sync complete"
// summary is deduped by a deterministic semantic fingerprint so an identical
// no-op result no longer republishes at INFO.

// countSummaryINFO / countSummaryDEBUG count how many "Drops sync complete"
// summary lines were emitted at each level in a captured log blob.
func countSummaryINFO(logs string) int {
	return strings.Count(logs, `level=INFO msg="Drops sync complete`)
}

func countSummaryDEBUG(logs string) int {
	return strings.Count(logs, `level=DEBUG msg="Drops sync complete`)
}

func emptySyncTracker(t *testing.T) (*DropsTracker, *fakeDropsClient) {
	t.Helper()
	client := &fakeDropsClient{
		dashboard: dashboardResponse(),
		inventory: emptyInventoryResponse(),
		details:   map[string]map[string]interface{}{},
	}
	return NewDropsTracker(client, nil, config.RateLimitSettings{}, nil), client
}

// §8.1: one logical startup trigger must run exactly one full sync (and thus one
// diagnostic block), not the pair the buffered construction-time seed caused.
func TestStartupRunsSingleFullSync(t *testing.T) {
	signal := make(chan struct{}, 64)
	client := &fakeDropsClient{
		dashboard:      dashboardResponse(),
		inventory:      emptyInventoryResponse(),
		details:        map[string]map[string]interface{}{},
		fullSyncSignal: signal,
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{
		CampaignSyncInterval:     100_000, // no scheduled sync during the test window
		DropProgressSyncInterval: 100_000,
	}, nil)
	tracker.intervalUnit = time.Millisecond
	// Mirror the miner's construction-time seed (miner.go:442): UpdateGameFilter
	// before Start buffers a campaignResync that must NOT fire a second sync.
	tracker.UpdateGameFilter([]string{"27546"}, []string{"World of Tanks"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tracker.Start(ctx)

	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("startup did not run its initial full sync")
	}
	select {
	case <-signal:
		t.Fatal("startup ran a second immediate full sync — the pre-Start seed trigger was not coalesced")
	case <-time.After(300 * time.Millisecond):
	}
}

// §8.2: one Settings save applies the blacklist and game filter together; that
// must schedule exactly one resync, not two back-to-back (the pair that defeated
// the buffered-to-1 coalescing).
func TestSettingsSaveTriggersSingleResync(t *testing.T) {
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

	// Absorb the single startup sync, then quiesce.
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("no startup full sync")
	}
	drainSignals(signal, 200*time.Millisecond)

	// One logical settings save (the two calls Miner.ApplySettings makes together).
	tracker.UpdateBlacklist([]string{"ewc 2026"})
	tracker.UpdateGameFilter([]string{"27546"}, []string{"World of Tanks"})

	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("settings save did not trigger a resync")
	}
	select {
	case <-signal:
		t.Fatal("settings save triggered a duplicate full sync")
	case <-time.After(300 * time.Millisecond):
	}
}

// §8.3: a streamer-list update alone must not trigger a full campaign resync (the
// roster propagation is not a re-filter), so it never republishes a block.
func TestUpdateStreamersDoesNotTriggerResync(t *testing.T) {
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

	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("no startup full sync")
	}
	drainSignals(signal, 200*time.Millisecond)

	tracker.UpdateStreamers([]*models.Streamer{models.NewStreamer("x", models.StreamerSettings{})})

	select {
	case <-signal:
		t.Fatal("UpdateStreamers must not trigger a full campaign sync")
	case <-time.After(300 * time.Millisecond):
	}
}

func drainSignals(ch <-chan struct{}, window time.Duration) {
	deadline := time.After(window)
	for {
		select {
		case <-ch:
		case <-deadline:
			return
		}
	}
}

// §8.4: the stable blacklist decision must be DEBUG, never INFO — but it must
// still be logged (the "-debug for per-campaign reasons" promise).
func TestBlacklistSkipIsDebugNotInfo(t *testing.T) {
	d := &DropsTracker{dropBlacklist: []string{"ewc 2026"}}
	cs := []*models.Campaign{
		{ID: "keep", Name: "Keep WoT", Drops: []*models.Drop{{Name: "Garage Slot"}}},
		{ID: "black", Name: "EWC WoT", Drops: []*models.Drop{{Name: "EWC 2026 Bronze"}}},
	}
	logs := captureLogs(t, func() { d.applyBlacklist(cs) })

	if !strings.Contains(logs, "matched drop-name blacklist") {
		t.Fatalf("blacklist decision must still be logged:\n%s", logs)
	}
	if strings.Contains(firstINFOLines(logs), "matched drop-name blacklist") {
		t.Fatalf("stable blacklist decision must be DEBUG, never INFO:\n%s", logs)
	}
	if !strings.Contains(logs, `level=DEBUG msg="Skipping drop campaign: matched drop-name blacklist`) {
		t.Fatalf("expected the blacklist decision at DEBUG:\n%s", logs)
	}
}

// §8.5: the stable game_not_allowed decision must be DEBUG, never INFO.
func TestGameNotAllowedIsDebugNotInfo(t *testing.T) {
	d := &DropsTracker{}
	d.UpdateGameFilter([]string{"game-wot"}, nil)
	cs := []*models.Campaign{
		gameCampaign("a", "game-wot", "World of Tanks"),
		gameCampaign("b", "game-wt", "War Thunder"),
	}
	logs := captureLogs(t, func() { d.applyGameFilter(cs) })

	if !strings.Contains(logs, "game not in configured drop-campaign game list") {
		t.Fatalf("game_not_allowed decision must still be logged:\n%s", logs)
	}
	if strings.Contains(firstINFOLines(logs), "game not in configured drop-campaign game list") {
		t.Fatalf("stable game_not_allowed decision must be DEBUG, never INFO:\n%s", logs)
	}
}

// §8.6: with a strict game ID configured, an unresolved best-effort NAME is a
// benign DEBUG diagnostic — never INFO, never WARN (strict ID already filters).
func TestUnresolvedNameWithStrictIDIsDebugOnly(t *testing.T) {
	d := &DropsTracker{}
	d.UpdateGameFilter([]string{"27546"}, []string{"World of Tanks"})
	cs := []*models.Campaign{
		gameCampaign("a", "27546", "World of Tanks HD"), // allowed by strict ID
		gameCampaign("b", "game-wt", "War Thunder"),     // foreign
	}
	logs := captureLogs(t, func() { d.applyGameFilter(cs) })

	if !strings.Contains(logs, "game_name_unresolved") {
		t.Fatalf("the unresolved-name note should still be logged (at DEBUG):\n%s", logs)
	}
	if strings.Contains(firstINFOLines(logs), "game_name_unresolved") {
		t.Fatalf("unresolved name under a strict ID must not be INFO:\n%s", logs)
	}
	if strings.Contains(logs, "level=WARN") {
		t.Fatalf("unresolved name under a strict ID must not WARN:\n%s", logs)
	}
}

// §8.7: a name-only configuration that resolves to nothing WARNs once, then stays
// quiet (DEBUG) while unchanged, and publishes one INFO transition on recovery.
func TestNameOnlyUnresolvedWarnsOnceThenDedupes(t *testing.T) {
	d := &DropsTracker{}
	d.UpdateGameFilter(nil, []string{"Nonexistent Game"})
	unresolved := []*models.Campaign{gameCampaign("a", "game-wot", "World of Tanks")}

	logs1 := captureLogs(t, func() { d.applyGameFilter(unresolved) })
	if strings.Count(logs1, "level=WARN") != 1 || !strings.Contains(logs1, "no game names resolved") {
		t.Fatalf("first name-only-unresolved sync must WARN exactly once:\n%s", logs1)
	}

	logs2 := captureLogs(t, func() { d.applyGameFilter(unresolved) })
	if strings.Contains(logs2, "level=WARN") {
		t.Fatalf("an unchanged name-only-unresolved condition must not repeat the WARN:\n%s", logs2)
	}
	if !strings.Contains(logs2, "level=DEBUG") {
		t.Fatalf("the unchanged fail-open condition should still be noted at DEBUG:\n%s", logs2)
	}

	// Recovery: a candidate now carries the configured game name, resolving it.
	resolved := []*models.Campaign{gameCampaign("a", "game-nx", "Nonexistent Game")}
	logs3 := captureLogs(t, func() { d.applyGameFilter(resolved) })
	if !strings.Contains(firstINFOLines(logs3), "fail-open condition cleared") {
		t.Fatalf("recovering resolution should publish one INFO transition:\n%s", logs3)
	}
}

// §8.8: the first completed full sync publishes exactly one INFO summary.
func TestFirstFullSyncPublishesOneInfoSummary(t *testing.T) {
	tracker, _ := emptySyncTracker(t)
	logs := captureLogs(t, tracker.syncCampaigns)
	if got := countSummaryINFO(logs); got != 1 {
		t.Fatalf("first full sync must publish exactly one INFO summary, got %d:\n%s", got, logs)
	}
}

// §8.9: an identical no-op result does not repeat the INFO summary — the second
// summary drops to DEBUG.
func TestIdenticalNoOpSyncDoesNotRepeatInfoSummary(t *testing.T) {
	tracker, _ := emptySyncTracker(t)
	logs := captureLogs(t, func() {
		tracker.syncCampaigns()
		tracker.syncCampaigns()
	})
	if got := countSummaryINFO(logs); got != 1 {
		t.Fatalf("identical no-op result must not repeat the INFO summary, got %d INFO:\n%s", got, logs)
	}
	if got := countSummaryDEBUG(logs); got != 1 {
		t.Fatalf("the repeated no-op summary must still be logged once at DEBUG, got %d DEBUG:\n%s", got, logs)
	}
}

// §8.10: a genuinely changed result publishes a fresh INFO summary.
func TestChangedResultPublishesNewInfoSummary(t *testing.T) {
	keepS, keepD := dashCampaign("keep", "Keep WoT", "game-wot", "World of Tanks", "kd", "Garage Slot")
	tracker, client := emptySyncTracker(t)
	client.details = map[string]map[string]interface{}{"keep": keepD}

	logs := captureLogs(t, func() {
		tracker.syncCampaigns()                     // empty -> "no active campaigns"
		client.dashboard = dashboardResponse(keepS) // a trackable campaign appears
		tracker.syncCampaigns()                     // tracked=1 -> new fingerprint
	})
	if got := countSummaryINFO(logs); got != 2 {
		t.Fatalf("a changed sync result must publish a fresh INFO summary, got %d INFO:\n%s", got, logs)
	}
}

// §8.11: a real API/GQL failure is never suppressed (logged at ERROR), and the
// recovery afterwards re-publishes the INFO summary even if the result matches a
// previously-fingerprinted success.
func TestSyncErrorNotSuppressedAndRecoveryReemitsInfo(t *testing.T) {
	tracker, client := emptySyncTracker(t)

	logs1 := captureLogs(t, tracker.syncCampaigns)
	if countSummaryINFO(logs1) != 1 {
		t.Fatalf("first sync must publish one INFO summary:\n%s", logs1)
	}

	client.dashboardErr = errors.New("gql boom")
	logs2 := captureLogs(t, tracker.syncCampaigns)
	if !strings.Contains(logs2, "level=ERROR") || !strings.Contains(logs2, "Drops sync failed") {
		t.Fatalf("API failure must be logged at ERROR, not suppressed:\n%s", logs2)
	}
	if countSummaryINFO(logs2) != 0 {
		t.Fatalf("a failed sync must not publish a completion summary:\n%s", logs2)
	}

	client.dashboardErr = nil
	logs3 := captureLogs(t, tracker.syncCampaigns)
	if countSummaryINFO(logs3) != 1 {
		t.Fatalf("recovery from a real error must re-publish the INFO summary even for an unchanged result, got %d:\n%s",
			countSummaryINFO(logs3), logs3)
	}
}

// §8.12: concurrent full syncs (serialized by fullSyncMu) racing SyncStatus reads
// must be race-safe over the new fingerprint/fail-open dedup state. Run under -race.
func TestConcurrentSyncAndStatusRaceSafe(t *testing.T) {
	tracker, _ := emptySyncTracker(t)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); tracker.syncCampaigns() }()
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = tracker.SyncStatus() }()
	}
	wg.Wait()
}
