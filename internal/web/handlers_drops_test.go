package web

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/health"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
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
	}, nil)

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

func TestBuildDropDetailViews(t *testing.T) {
	c := &models.Campaign{
		Name: "Cool Campaign",
		Drops: []*models.Drop{
			{Name: "Tier 2", Benefit: "Emote", ImageURL: "img2", MinutesRequired: 120, CurrentMinutesWatched: 30},
			{Name: "Tier 1", Benefit: "Badge", ImageURL: "img1", MinutesRequired: 60, CurrentMinutesWatched: 60},
		},
		ClaimedDropNames: []string{"Tier 0 (already got it)"},
	}

	views := buildDropDetailViews(c)
	if len(views) != 3 {
		t.Fatalf("expected 3 detail views (2 in-progress + 1 claimed), got %d", len(views))
	}

	// In-progress drops are ordered by watch requirement (Tier 1 before Tier 2).
	if views[0].Name != "Tier 1" || views[1].Name != "Tier 2" {
		t.Errorf("expected drops ordered by requirement, got %q then %q", views[0].Name, views[1].Name)
	}
	if views[0].Claimed || views[0].StatusLabel != "In progress" {
		t.Errorf("expected first drop in progress, got %+v", views[0])
	}
	if views[0].Percent != 100 || !views[0].HasMinuteProgress || views[0].MinutesWatched != 60 || views[0].MinutesRequired != 60 {
		t.Errorf("unexpected progress on first drop: %+v", views[0])
	}
	if views[1].Percent != 25 {
		t.Errorf("expected 25%% on Tier 2, got %d", views[1].Percent)
	}

	// Already-claimed rewards (from claim history) come last, marked claimed.
	claimed := views[2]
	if !claimed.Claimed || claimed.StatusLabel != "Already claimed" || claimed.Percent != 100 || claimed.Name != "Tier 0 (already got it)" {
		t.Errorf("unexpected claimed detail view: %+v", claimed)
	}
}

// TestDropHealthBadgeViews pins the Drops-page watchdog badge content for all
// three states, matching the Stage 3 UI spec: HEALTHY shows last progress +
// channel, RECOVERING shows the flat spell + delivered reports + stage,
// STALLED shows the exhausted-recovery message + last attempt.
func TestDropHealthBadgeViews(t *testing.T) {
	tenMinAgo := time.Now().Add(-10 * time.Minute)

	healthy := buildDropHealthView(health.DropProgress{
		Status: health.ProgressHealthy, Channel: "streamer-name", LastProgressAt: tenMinAgo,
	})
	if healthy.Label != "HEALTHY" || len(healthy.Lines) != 2 {
		t.Fatalf("unexpected healthy badge: %+v", healthy)
	}
	if !strings.Contains(healthy.Lines[0], "Last progress:") || !strings.Contains(healthy.Lines[1], "streamer-name") {
		t.Fatalf("healthy badge lines wrong: %+v", healthy.Lines)
	}

	recovering := buildDropHealthView(health.DropProgress{
		Status: health.ProgressRecovering, Channel: "chan", LastProgressAt: tenMinAgo,
		ReportsSinceProgress: 17, RecoveryStageName: "session_recreate",
	})
	if recovering.Label != "RECOVERING" || len(recovering.Lines) != 3 {
		t.Fatalf("unexpected recovering badge: %+v", recovering)
	}
	if !strings.Contains(recovering.Lines[0], "No progress for") ||
		!strings.Contains(recovering.Lines[1], "17 delivered") ||
		!strings.Contains(recovering.Lines[2], "watch session recreate") {
		t.Fatalf("recovering badge lines wrong: %+v", recovering.Lines)
	}

	stalled := buildDropHealthView(health.DropProgress{
		Status: health.ProgressStalled, LastProgressAt: tenMinAgo, RecoveryStageName: "channel_switch",
	})
	if stalled.Label != "STALLED" || len(stalled.Lines) != 3 {
		t.Fatalf("unexpected stalled badge: %+v", stalled)
	}
	if stalled.Lines[0] != "Automatic recovery did not help" || !strings.Contains(stalled.Lines[1], "channel switch") {
		t.Fatalf("stalled badge lines wrong: %+v", stalled.Lines)
	}
}

// TestBuildDropCampaignViewsMergesHealth verifies the watchdog snapshot is
// merged onto the right campaign card by ID, and campaigns without watchdog
// state keep a nil badge.
func TestBuildDropCampaignViewsMergesHealth(t *testing.T) {
	tracked := &models.Campaign{ID: "camp-1", Name: "Tracked",
		Drops: []*models.Drop{{Name: "A", MinutesRequired: 100, CurrentMinutesWatched: 50}}}
	other := &models.Campaign{ID: "camp-2", Name: "Other",
		Drops: []*models.Drop{{Name: "B", MinutesRequired: 100, CurrentMinutesWatched: 90}}}

	snap := health.ProgressSnapshot{Enabled: true, Drops: []health.DropProgress{
		{CampaignID: "camp-1", DropID: "d1", Status: health.ProgressRecovering, RecoveryStageName: "full_resync"},
	}}
	views := buildDropCampaignViews([]*models.Campaign{tracked, other}, dropHealthByCampaign(snap))

	byName := map[string]DropCampaignView{}
	for _, v := range views {
		byName[v.Name] = v
	}
	if byName["Tracked"].Health == nil || byName["Tracked"].Health.Label != "RECOVERING" {
		t.Fatalf("expected a RECOVERING badge on the tracked campaign, got %+v", byName["Tracked"].Health)
	}
	if byName["Other"].Health != nil {
		t.Fatalf("expected no badge on the untracked campaign, got %+v", byName["Other"].Health)
	}

	// Disabled watchdog: no badges at all.
	if m := dropHealthByCampaign(health.ProgressSnapshot{Enabled: false, Drops: snap.Drops}); m != nil {
		t.Fatalf("disabled watchdog must yield no badges, got %+v", m)
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
		{ID: "camp-1", Name: "C", GameName: "Rust", BoxArtURL: "x", DropName: "Skin", ChannelRestricted: true,
			StatusLabel: "In progress", OverallPercent: 25, HasMinuteProgress: true,
			MinutesWatched: 30, MinutesRequired: 120, MinutesRemaining: 90, MinutePercent: 25,
			Health: &DropHealthView{Status: health.ProgressRecovering, Label: "RECOVERING", BadgeColor: "#f59e0b",
				Lines: []string{"No progress for 18m", "Watch reports: 17 delivered", "Stage: watch session recreate"}},
			Drops: []DropDetailView{
				{Name: "Emote Pack", Benefit: "5 Emotes", StatusLabel: "In progress", Percent: 25,
					HasMinuteProgress: true, MinutesWatched: 30, MinutesRequired: 120},
				{Name: "Old Badge", StatusLabel: "Already claimed", Claimed: true, Percent: 100},
			}},
		{ID: "camp-2", Name: "Done", StatusLabel: "Already claimed", Claimed: true, OverallPercent: 100},
	}}
	if err := partials.ExecuteTemplate(&buf, "drops_list", dropsData); err != nil {
		t.Fatalf("drops_list render failed: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Channel-only drop") || !strings.Contains(out, "90 min remaining") {
		t.Errorf("drops_list output missing expected content:\n%s", out)
	}
	// The watchdog badge renders with its label, explanation lines, and the
	// inline color surviving html/template's CSS sanitizer.
	if !strings.Contains(out, "RECOVERING") || !strings.Contains(out, "Watch reports: 17 delivered") {
		t.Errorf("drops_list output missing the watchdog badge:\n%s", out)
	}
	if !strings.Contains(out, "#f59e0b") || strings.Contains(out, "ZgotmplZ") {
		t.Errorf("badge color did not survive CSS sanitization:\n%s", out)
	}
	// The per-campaign modal and its individual drops must render.
	if !strings.Contains(out, `id="drop-modal-0"`) || !strings.Contains(out, `data-drop-modal="drop-modal-0"`) {
		t.Errorf("drops_list output missing modal wiring:\n%s", out)
	}
	if !strings.Contains(out, "Emote Pack") || !strings.Contains(out, "Old Badge") {
		t.Errorf("drops_list output missing per-drop detail rows:\n%s", out)
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
