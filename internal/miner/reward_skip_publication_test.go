package miner

// End-to-end wiring guard for the operator farming exclusion: the miner is
// the ONLY bridge from config.DropRules to the side-effect owners, so a rule
// set through the production SetDropRule path (or present at wiring time)
// must actually suppress the drops tracker's auto-claim, and clearing it must
// restore claiming. Reverting the publishRewardSkipsLocked wiring makes this
// fail while every package-local gate test still passes.

import (
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/drops"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/twitch"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/watcher"
)

// claimableDropsGQL mirrors fakeDropsGQL with a CLAIMABLE in-progress reward
// (instance minted, threshold met) and records every ClaimDrop call.
type claimableDropsGQL struct {
	mu      sync.Mutex
	claimed []string
}

func (c *claimableDropsGQL) PostGQL(op constants.GQLOperation) (map[string]interface{}, error) {
	switch op.OperationName {
	case "ViewerDropsDashboard":
		return map[string]interface{}{
			"data": map[string]interface{}{
				"currentUser": map[string]interface{}{
					"dropCampaigns": []interface{}{},
				},
			},
		}, nil
	case "Inventory":
		return map[string]interface{}{
			"data": map[string]interface{}{
				"currentUser": map[string]interface{}{
					"inventory": map[string]interface{}{
						"dropCampaignsInProgress": []interface{}{
							map[string]interface{}{
								"id":   "campaign-wot",
								"name": "World of Tanks Drops",
								"game": map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
								"timeBasedDrops": []interface{}{
									map[string]interface{}{
										"id":                     "drop-1",
										"name":                   "Garage Slot",
										"requiredMinutesWatched": float64(120),
										"self": map[string]interface{}{
											"currentMinutesWatched": float64(120),
											"dropInstanceID":        "inst-1",
											"isClaimed":             false,
										},
									},
								},
							},
						},
					},
				},
			},
		}, nil
	}
	return map[string]interface{}{}, nil
}

func (c *claimableDropsGQL) GetDropCampaignDetails(string) (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}

func (c *claimableDropsGQL) ClaimDrop(d *models.Drop) (twitch.ClaimStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.claimed = append(c.claimed, d.Name)
	return twitch.ClaimStatusAccepted, nil
}

func (c *claimableDropsGQL) claimCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.claimed)
}

// A Skip rule persisted via the production SetDropRule path suppresses the
// tracker's auto-claim end to end; resetting the rule (zero value) restores
// it. (Slow: the restored claim runs the production 5s post-claim sleep.)
func TestSetDropRulePublishesFarmingExclusionToDropsTracker(t *testing.T) {
	m, _ := newPersistedDropRuleMiner(t, nil, false)
	client := &claimableDropsGQL{}
	m.dropsTracker = drops.NewDropsTracker(client, nil, m.config.RateLimits, nil)
	m.watcher = &watcher.MinuteWatcher{}

	key := models.NormalizeRewardKey("game-wot", "Garage Slot")
	if err := m.SetDropRule(key, config.DropRule{Skip: true}); err != nil {
		t.Fatalf("SetDropRule: %v", err)
	}

	m.dropsTracker.SyncNow()
	if got := client.claimCount(); got != 0 {
		t.Fatalf("rule active: the skipped reward must not be auto-claimed, got %d claims (%v)", got, client.claimed)
	}

	// Reset (zero value clears the rule) → the reward claims again.
	if err := m.SetDropRule(key, config.DropRule{}); err != nil {
		t.Fatalf("SetDropRule reset: %v", err)
	}
	m.dropsTracker.SyncNow()
	if got := client.claimCount(); got == 0 {
		t.Fatal("rule cleared: the reward must be auto-claimed again")
	}
}

// Wiring-time publication: rules already present in the config reach the
// tracker through refreshPolicy (the same call setupComponents seeds and the
// watchdog repeats) without any SetDropRule call this session.
func TestRefreshPolicyPublishesPreexistingRules(t *testing.T) {
	key := models.NormalizeRewardKey("game-wot", "Garage Slot")
	m, _ := newPersistedDropRuleMiner(t, map[string]config.DropRule{key: {Skip: true}}, false)
	client := &claimableDropsGQL{}
	m.dropsTracker = drops.NewDropsTracker(client, nil, m.config.RateLimits, nil)
	m.watcher = &watcher.MinuteWatcher{}

	m.refreshPolicy(time.Unix(1_700_000_000, 0))
	m.dropsTracker.SyncNow()
	if got := client.claimCount(); got != 0 {
		t.Fatalf("preexisting rule must gate the very first sync, got %d claims (%v)", got, client.claimed)
	}
}

// rewardSkipKeys derives exactly the Skip=true keys, verbatim.
func TestRewardSkipKeys(t *testing.T) {
	rules := map[string]config.DropRule{
		"g1::skipped":     {Skip: true},
		"g1::prioritized": {HighPriority: true},
		"Legacy::Key":     {Skip: true, AlwaysFinishStarted: true},
	}
	keys := rewardSkipKeys(rules)
	if len(keys) != 2 {
		t.Fatalf("expected the two Skip=true keys, got %v", keys)
	}
	set := map[string]bool{}
	for _, k := range keys {
		set[k] = true
	}
	if !set["g1::skipped"] || !set["Legacy::Key"] {
		t.Fatalf("keys must be carried verbatim, got %v", keys)
	}
	if rewardSkipKeys(nil) != nil {
		t.Fatal("no rules yields no keys")
	}
}
