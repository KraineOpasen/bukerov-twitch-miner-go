package drops

import (
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"testing"
	"time"
)

// RED_PROPOSED. Existing unit tests call updateStreamerCampaigns directly;
// this test exercises the production no-change progress-sync entry point.
func TestAuditUnchangedProgressRevalidatesAvailability(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		name := "known-empty"
		if unknown {
			name = "unknown-grace-expired"
		}
		t.Run(name, func(t *testing.T) {
			now := time.Now()
			d, s, _ := continuitySetup(&now)
			d.client = &fakeDropsClient{inventory: inventoryWithInProgress(map[string]interface{}{
				"id": "camp-1", "timeBasedDrops": []interface{}{invEntry("d1", 60, 10, nil)},
			})}
			setAvailability(s, true, []string{"camp-1"}, now)
			d.updateStreamerCampaigns()
			if !hasAssignment(s) {
				t.Fatal("initial eligible assignment missing")
			}
			setAvailability(s, !unknown, nil, now)
			if unknown {
				now = now.Add(models.CampaignAvailabilityGrace)
			}
			d.syncProgress()
			if s.HasEligibleAssignedDropCampaign() {
				t.Fatal("unchanged inventory kept an invalid Drops assignment after a completed sync")
			}
		})
	}
}

// Campaign expiry with date-less inventory rewards must not depend on minute changes.
func TestAuditCampaignOnlyDeadlineRevalidatesWithoutProgress(t *testing.T) {
	now := time.Now()
	d, s, c := continuitySetup(&now)
	c.EndAt = now.Add(time.Hour)
	c.Drops[0].StartAt, c.Drops[0].EndAt = time.Time{}, time.Time{}
	d.client = &fakeDropsClient{inventory: inventoryWithInProgress(map[string]interface{}{
		"id": "camp-1", "timeBasedDrops": []interface{}{invEntry("d1", 60, 10, nil)},
	})}
	setAvailability(s, true, []string{"camp-1"}, now)
	d.updateStreamerCampaigns()
	if !s.HasEligibleAssignedDropCampaign() {
		t.Fatal("initial assignment missing")
	}
	now = now.Add(2 * time.Hour)
	d.syncProgress()
	stale := s.HasEligibleAssignedDropCampaign()
	d.updateStreamerCampaigns()
	if s.HasEligibleAssignedDropCampaign() {
		t.Fatal("direct expiry revalidation control failed")
	}
	if stale {
		t.Fatal("campaign-only deadline passed, unchanged progress retained expired authority")
	}
}
