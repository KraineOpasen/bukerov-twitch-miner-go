package miner

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/policy"
)

// TestPolicyGameRanks maps the ranked decision order onto per-game ranks
// (first appearance wins, excluded decisions ignored) for the discovery
// cross-game ordering.
func TestPolicyGameRanks(t *testing.T) {
	campaigns := []*models.Campaign{
		{ID: "c1", Game: &models.Game{Name: "Alpha"}},
		{ID: "c2", Game: &models.Game{Name: "Bravo"}},
		{ID: "c3", Game: &models.Game{Name: "Alpha"}}, // same game as c1
		{ID: "c4", Game: &models.Game{Name: "Charlie"}},
	}
	decisions := []policy.Decision{
		{CampaignID: "c2"},                 // Bravo first
		{CampaignID: "c4", Excluded: true}, // excluded → ignored
		{CampaignID: "c3"},                 // Alpha
		{CampaignID: "c1"},                 // Alpha again → no new rank
	}
	ranks := policyGameRanks(decisions, campaigns)

	if ranks["bravo"] != 0 {
		t.Errorf("bravo rank = %d, want 0", ranks["bravo"])
	}
	if ranks["alpha"] != 1 {
		t.Errorf("alpha rank = %d, want 1", ranks["alpha"])
	}
	if _, ok := ranks["charlie"]; ok {
		t.Error("excluded campaign's game must not be ranked")
	}
}

func TestGameOrderIndexLookup(t *testing.T) {
	idx := map[string]int{"world of tanks": 0, "rust": 1}
	if got := gameOrderIndex(idx, "Rust"); got != 1 {
		t.Errorf("case-insensitive lookup = %d, want 1", got)
	}
	if got := gameOrderIndex(idx, "Unlisted Game"); got != -1 {
		t.Errorf("unconfigured game = %d, want -1", got)
	}
}
