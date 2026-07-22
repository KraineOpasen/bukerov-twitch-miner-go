package drops

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// gameEventDrops is absent from every other fixture in the repo; these tests are
// the first representative payloads for the account-wide claim-history path.

func TestExtractClaimedRewardsNameOnly(t *testing.T) {
	inv := map[string]interface{}{
		"gameEventDrops": []interface{}{
			map[string]interface{}{"name": "Legendary Skin", "gameId": "g1"},
			map[string]interface{}{"benefit": map[string]interface{}{"name": "Emote Pack"}, "game": map[string]interface{}{"id": "g1"}},
		},
	}
	recs := extractClaimedRewards(inv)
	if len(recs) != 2 {
		t.Fatalf("expected 2 records, got %d", len(recs))
	}
	// No benefit id in the proven shape -> name-only evidence (weak, fail-open).
	for _, r := range recs {
		if r.Identity.Evidence != models.EvidenceNameOnly {
			t.Errorf("expected name-only evidence, got %v", r.Identity.Evidence)
		}
	}
}

func TestExtractClaimedRewardsCapturesBenefitID(t *testing.T) {
	inv := map[string]interface{}{
		"gameEventDrops": []interface{}{
			map[string]interface{}{
				"name":    "Skin",
				"gameId":  "g1",
				"benefit": map[string]interface{}{"id": "ben-1", "name": "Skin"},
			},
		},
	}
	recs := extractClaimedRewards(inv)
	if len(recs) != 1 || recs[0].Identity.BenefitID != "ben-1" {
		t.Fatalf("benefit id should be captured additively, got %+v", recs)
	}
	if recs[0].Identity.Evidence != models.EvidenceBenefit {
		t.Fatalf("benefit id present should be benefit evidence, got %v", recs[0].Identity.Evidence)
	}
}

func TestExtractClaimedRewardsDedup(t *testing.T) {
	inv := map[string]interface{}{
		"gameEventDrops": []interface{}{
			map[string]interface{}{"name": "Skin", "gameId": "g1"},
			map[string]interface{}{"name": "Skin", "gameId": "g1"}, // dup
		},
	}
	if got := len(extractClaimedRewards(inv)); got != 1 {
		t.Fatalf("expected deduped to 1, got %d", got)
	}
}

func TestExtractClaimedRewardsEmpty(t *testing.T) {
	if recs := extractClaimedRewards(nil); recs != nil {
		t.Error("nil inventory should yield no records")
	}
	if recs := extractClaimedRewards(map[string]interface{}{}); recs != nil {
		t.Error("missing gameEventDrops should yield no records")
	}
}

// End-to-end at the campaign level: a name-only claim history keeps a same-named
// drop farmable (fail open) while a benefit-id match confirms it.
func TestClaimHistoryFailOpenVsConfirmed(t *testing.T) {
	inv := map[string]interface{}{
		"gameEventDrops": []interface{}{
			map[string]interface{}{"name": "Coin", "gameId": "g1"},
		},
	}
	recs := extractClaimedRewards(inv)

	nameOnly := &models.Campaign{ID: "c1", Game: &models.Game{ID: "g1"},
		Drops: []*models.Drop{{ID: "d1", Name: "Coin", MinutesRequired: 60}}}
	nameOnly.ApplyClaimHistoryRecords(recs, nil)
	if len(nameOnly.Drops) != 1 {
		t.Fatal("name-only claim history must keep the drop farmable (fail open)")
	}

	invBenefit := map[string]interface{}{
		"gameEventDrops": []interface{}{
			map[string]interface{}{"name": "Coin", "gameId": "g1", "benefit": map[string]interface{}{"id": "ben-1"}},
		},
	}
	withBenefit := &models.Campaign{ID: "c2", Game: &models.Game{ID: "g1"},
		Drops: []*models.Drop{{ID: "d1", Name: "Coin", BenefitID: "ben-1", MinutesRequired: 60}}}
	withBenefit.ApplyClaimHistoryRecords(extractClaimedRewards(invBenefit), nil)
	if len(withBenefit.Drops) != 0 {
		t.Fatal("benefit-id claim history should confirm and remove the drop")
	}
}
