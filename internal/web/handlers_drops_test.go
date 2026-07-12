package web

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/PatrickWalther/twitch-miner-go/internal/models"
)

func TestBuildDropCampaignViewsOrdering(t *testing.T) {
	soon := time.Now().Add(24 * time.Hour)
	later := time.Now().Add(72 * time.Hour)

	claimed := &models.Campaign{Name: "Claimed", ClaimStatus: models.CampaignClaimStatusAlreadyClaimed, EndAt: soon}
	restricted := &models.Campaign{
		Name:     "Restricted",
		Channels: []string{"chan-1"},
		EndAt:    later,
		Drops:    []*models.Drop{{Name: "R", MinutesRequired: 100, CurrentMinutesWatched: 10}},
	}
	aheadUnrestricted := &models.Campaign{
		Name:  "AheadUnrestricted",
		EndAt: later,
		Drops: []*models.Drop{{Name: "A", MinutesRequired: 100, CurrentMinutesWatched: 90}},
	}
	behindUnrestricted := &models.Campaign{
		Name:  "BehindUnrestricted",
		EndAt: soon,
		Drops: []*models.Drop{{Name: "B", MinutesRequired: 100, CurrentMinutesWatched: 20}},
	}

	views := buildDropCampaignViews([]*models.Campaign{
		claimed, behindUnrestricted, aheadUnrestricted, restricted,
	})

	got := make([]string, len(views))
	for i, v := range views {
		got[i] = v.Name
	}

	want := []string{"Restricted", "AheadUnrestricted", "BehindUnrestricted", "Claimed"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unexpected order: got %v, want %v", got, want)
		}
	}
}

func TestBuildDropCampaignViewFields(t *testing.T) {
	c := &models.Campaign{
		Name:     "Cool Campaign",
		Game:     &models.Game{Name: "Rust"},
		Channels: []string{"chan-1"},
		Drops: []*models.Drop{
			{Name: "Skin", Benefit: "Legendary Skin", MinutesRequired: 120, CurrentMinutesWatched: 30},
		},
	}

	v := buildDropCampaignView(c)
	if !v.ChannelRestricted {
		t.Error("expected channel-restricted")
	}
	if v.Claimed || v.StatusLabel != "In progress" {
		t.Errorf("expected in-progress status, got %q claimed=%v", v.StatusLabel, v.Claimed)
	}
	if v.DropName != "Skin" || v.DropBenefit != "Legendary Skin" {
		t.Errorf("unexpected drop fields: %+v", v)
	}
	if !v.HasMinuteProgress || v.MinutesWatched != 30 || v.MinutesRequired != 120 || v.MinutesRemaining != 90 {
		t.Errorf("unexpected minute progress: %+v", v)
	}
	if v.MinutePercent != 25 || v.OverallPercent != 25 {
		t.Errorf("expected 25%% progress, got minute=%d overall=%d", v.MinutePercent, v.OverallPercent)
	}
	if !strings.Contains(v.BoxArtURL, "Rust") {
		t.Errorf("expected box art URL to reference the game, got %q", v.BoxArtURL)
	}
}

// TestTemplatesRenderDropsAndCards ensures the new templates parse and execute
// against their view models (embedded via the same globs the server uses).
func TestTemplatesRenderDropsAndCards(t *testing.T) {
	templates := loadTemplates()

	partials := templates["partials"]
	if partials == nil {
		t.Fatal("partials template not loaded")
	}

	var buf bytes.Buffer
	dropsData := DropsListData{Campaigns: []DropCampaignView{
		{Name: "C", GameName: "Rust", BoxArtURL: "x", DropName: "Skin", ChannelRestricted: true,
			StatusLabel: "In progress", OverallPercent: 25, HasMinuteProgress: true,
			MinutesWatched: 30, MinutesRequired: 120, MinutesRemaining: 90, MinutePercent: 25},
		{Name: "Done", StatusLabel: "Already claimed", Claimed: true, OverallPercent: 100},
	}}
	if err := partials.ExecuteTemplate(&buf, "drops_list", dropsData); err != nil {
		t.Fatalf("drops_list render failed: %v", err)
	}
	if !strings.Contains(buf.String(), "Channel-only drop") || !strings.Contains(buf.String(), "90 min remaining") {
		t.Errorf("drops_list output missing expected content:\n%s", buf.String())
	}

	buf.Reset()
	gridData := StreamerGridData{TrackedLive: []StreamerInfo{{
		Name: "streamer1", IsLive: true, PointsFormatted: "1,000",
		HasCampaign: true, CampaignName: "Camp", CampaignDropName: "Drop", CampaignPercent: 42, CampaignMinutesInfo: "42/100 min",
	}}}
	if err := partials.ExecuteTemplate(&buf, "streamer_grid", gridData); err != nil {
		t.Fatalf("streamer_grid render failed: %v", err)
	}
	if !strings.Contains(buf.String(), "42%") {
		t.Errorf("streamer card mini progress bar missing:\n%s", buf.String())
	}

	// Drops page must parse against its base layout too.
	if templates["drops.html"] == nil {
		t.Fatal("drops.html page template not loaded")
	}
}
