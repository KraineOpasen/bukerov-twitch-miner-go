package drops

import (
	"context"
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

// End-to-end at the campaign level: because the proven gameEventDrops shape
// carries NO entitlement window, BOTH a name-only and a benefit-id claim record
// yield an Ambiguous match — the drop stays farmable (fail open). A benefit id
// alone identifies a reward family, not a specific occurrence, so without window
// evidence it can never confirm a claim from claim history. This is the
// corrected B1 behavior: a repeatable reward is never falsely marked claimed.
func TestClaimHistoryFailOpenNoWindow(t *testing.T) {
	nameOnlyInv := map[string]interface{}{
		"gameEventDrops": []interface{}{
			map[string]interface{}{"name": "Coin", "gameId": "g1"},
		},
	}
	nameOnly := &models.Campaign{ID: "c1", Game: &models.Game{ID: "g1"},
		Drops: []*models.Drop{{ID: "d1", Name: "Coin", MinutesRequired: 60}}}
	nameOnly.ApplyClaimHistoryRecords(extractClaimedRewards(nameOnlyInv), nil)
	if len(nameOnly.Drops) != 1 {
		t.Fatal("name-only claim history must keep the drop farmable (fail open)")
	}

	benefitInv := map[string]interface{}{
		"gameEventDrops": []interface{}{
			map[string]interface{}{"name": "Coin", "gameId": "g1", "benefit": map[string]interface{}{"id": "ben-1"}},
		},
	}
	withBenefit := &models.Campaign{ID: "c2", Game: &models.Game{ID: "g1"},
		Drops: []*models.Drop{{ID: "d1", Name: "Coin", BenefitID: "ben-1", MinutesRequired: 60}}}
	out := withBenefit.ApplyClaimHistoryRecords(extractClaimedRewards(benefitInv), nil)
	if len(withBenefit.Drops) != 1 {
		t.Fatal("benefit-id claim history WITHOUT a window must stay ambiguous (fail open), not confirm")
	}
	if len(out.AmbiguousNames) != 1 || len(out.ConfirmedNames) != 0 {
		t.Fatalf("expected ambiguous, got confirmed=%v ambiguous=%v", out.ConfirmedNames, out.AmbiguousNames)
	}
}

// TestInventoryClaimedSurvivesClaimHistoryFailOpen closes the loop the rest of
// this file documents: claim history's benefit-id-without-window signal can
// never CONFIRM a claim (TestClaimHistoryFailOpenNoWindow above, proven and
// left exactly as-is -- that data shape genuinely cannot support it, see
// skipledger.go's file-level doc comment). But a raw self.isClaimed=true
// sighting for the SAME reward independently reaches the skip ledger (E3,
// S4) and gates future broker assignment on its own -- this is the actual
// fix for the structural gap the rest of this file proves exists.
func TestInventoryClaimedSurvivesClaimHistoryFailOpen(t *testing.T) {
	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	d := &DropsTracker{skipLedger: ledger}

	// The exact reward TestClaimHistoryFailOpenNoWindow proves claim history
	// can never confirm: a benefit id is present, but no entitlement window.
	prog := []interface{}{
		map[string]interface{}{
			"id":   "c2",
			"name": "Camp",
			"game": map[string]interface{}{"id": "g1", "name": "Game"},
			"timeBasedDrops": []interface{}{
				claimedInProgressDrop("d1", "Coin", 60, 60, "inst-ch1", "ben-1"),
			},
		},
	}
	d.observeClaimedFromInventory(prog)

	snap, err := ledger.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	skip, reason := snap.decide(models.RewardIdentity{
		GameID: "g1", BenefitID: "ben-1", InstanceID: "inst-ch1", DropID: "d1", CampaignID: "c2",
	}, false)
	if !skip {
		t.Fatalf("expected the ghost-skip ledger to SKIP a re-offer of the same reward (reason=%q) even though claim history itself stays fail-open", reason)
	}
}
