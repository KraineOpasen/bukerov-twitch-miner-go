package web

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/health"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/policy"
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
	}, nil, nil, enTR(t))

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

func TestBuildDropCampaignViewsKnownDeadlineBeforeUnknown(t *testing.T) {
	known := &models.Campaign{
		ID:    "known",
		Name:  "Known",
		EndAt: time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC),
	}
	unknown := &models.Campaign{ID: "unknown", Name: "Unknown"}

	for _, tc := range []struct {
		name  string
		input []*models.Campaign
	}{
		{name: "known then unknown", input: []*models.Campaign{known, unknown}},
		{name: "unknown then known", input: []*models.Campaign{unknown, known}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			views := buildDropCampaignViews(tc.input, nil, nil, enTR(t))
			if got := []string{views[0].ID, views[1].ID}; got[0] != "known" || got[1] != "unknown" {
				t.Fatalf("deadline order = %v, want [known unknown]", got)
			}
		})
	}
}

func TestBuildDropCampaignViewsDeadlineTieBreaks(t *testing.T) {
	early := &models.Campaign{
		ID: "early", Name: "Zulu",
		EndAt: time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC),
	}
	later := &models.Campaign{
		ID: "later", Name: "Alpha",
		EndAt: early.EndAt.Add(time.Hour),
	}
	alphaUnknown := &models.Campaign{ID: "alpha-unknown", Name: "Alpha"}
	zuluUnknown := &models.Campaign{ID: "zulu-unknown", Name: "Zulu"}

	for _, tc := range []struct {
		name  string
		input []*models.Campaign
		want  []string
	}{
		{name: "known earlier then later", input: []*models.Campaign{early, later}, want: []string{"early", "later"}},
		{name: "known later then earlier", input: []*models.Campaign{later, early}, want: []string{"early", "later"}},
		{name: "unknown alpha then zulu", input: []*models.Campaign{alphaUnknown, zuluUnknown}, want: []string{"alpha-unknown", "zulu-unknown"}},
		{name: "unknown zulu then alpha", input: []*models.Campaign{zuluUnknown, alphaUnknown}, want: []string{"alpha-unknown", "zulu-unknown"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			views := buildDropCampaignViews(tc.input, nil, nil, enTR(t))
			got := []string{views[0].ID, views[1].ID}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("deadline tie-break order = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBuildDropCampaignViewsDeadlineDoesNotOverrideEarlierPriorities(t *testing.T) {
	knownEnd := time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		known     *models.Campaign
		unknown   *models.Campaign
		wantFirst string
	}{
		{
			name:      "unclaimed before claimed",
			known:     &models.Campaign{ID: "claimed-known", Name: "Claimed known", EndAt: knownEnd, ClaimStatus: models.CampaignClaimStatusAlreadyClaimed},
			unknown:   &models.Campaign{ID: "unclaimed-unknown", Name: "Unclaimed unknown"},
			wantFirst: "unclaimed-unknown",
		},
		{
			name:      "restricted before unrestricted",
			known:     &models.Campaign{ID: "unrestricted-known", Name: "Unrestricted known", EndAt: knownEnd},
			unknown:   &models.Campaign{ID: "restricted-unknown", Name: "Restricted unknown", Channels: []string{"channel"}},
			wantFirst: "restricted-unknown",
		},
		{
			name:      "higher progress before lower progress",
			known:     &models.Campaign{ID: "lower-known", Name: "Lower known", EndAt: knownEnd, Drops: []*models.Drop{{MinutesRequired: 100, CurrentMinutesWatched: 20}}},
			unknown:   &models.Campaign{ID: "higher-unknown", Name: "Higher unknown", Drops: []*models.Drop{{MinutesRequired: 100, CurrentMinutesWatched: 90}}},
			wantFirst: "higher-unknown",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, input := range [][]*models.Campaign{{tc.known, tc.unknown}, {tc.unknown, tc.known}} {
				views := buildDropCampaignViews(input, nil, nil, enTR(t))
				if views[0].ID != tc.wantFirst {
					t.Fatalf("first campaign = %q, want %q", views[0].ID, tc.wantFirst)
				}
			}
		})
	}
}

func TestBuildDropCampaignViewsPreservesUnknownPolicyAndSources(t *testing.T) {
	now := time.Date(2029, time.January, 2, 3, 4, 5, 0, time.UTC)
	unknown := &models.Campaign{
		ID: "unknown", Name: "Unknown",
		Drops: []*models.Drop{{Name: "Reward", MinutesRequired: 60}},
	}
	known := &models.Campaign{
		ID: "known", Name: "Known", EndAt: now.Add(24 * time.Hour),
		Drops: []*models.Drop{{Name: "Reward", MinutesRequired: 60}},
	}
	decision := policy.Decide(policy.ModeEndingSoonest, policy.CampaignInput{
		CampaignID: unknown.ID,
		Drops:      []policy.DropStep{{MinutesRequired: 60}},
	}, now)
	if decision.Status != policy.StatusUnknown || decision.Feasibility.DeadlineKnown || decision.Excluded {
		t.Fatalf("policy decision = %+v, want explicit non-excluded UNKNOWN", decision)
	}

	input := []*models.Campaign{unknown, known}
	inputBefore := append([]*models.Campaign(nil), input...)
	unknownBefore, knownBefore := *unknown, *known
	policyByID := buildDropPolicyByCampaign(input, []policy.Decision{decision}, nil, enTR(t))
	views := buildDropCampaignViews(input, nil, policyByID, enTR(t))

	if views[0].ID != "known" || views[1].ID != "unknown" {
		t.Fatalf("web order = [%s %s], want [known unknown]", views[0].ID, views[1].ID)
	}
	if views[1].Policy == nil || views[1].Policy.Status != string(policy.StatusUnknown) || views[1].Policy.Excluded {
		t.Fatalf("unknown policy projection = %+v", views[1].Policy)
	}
	if !reflect.DeepEqual(input, inputBefore) || !reflect.DeepEqual(*unknown, unknownBefore) || !reflect.DeepEqual(*known, knownBefore) {
		t.Fatal("sorting/rendering mutated the source campaigns or input order")
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

	v := buildDropCampaignView(c, enTR(t))
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

	views := buildDropDetailViews(c, enTR(t))
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
	}, enTR(t))
	if healthy.Label != "HEALTHY" || len(healthy.Lines) != 2 {
		t.Fatalf("unexpected healthy badge: %+v", healthy)
	}
	if !strings.Contains(healthy.Lines[0], "Last progress:") || !strings.Contains(healthy.Lines[1], "streamer-name") {
		t.Fatalf("healthy badge lines wrong: %+v", healthy.Lines)
	}

	recovering := buildDropHealthView(health.DropProgress{
		Status: health.ProgressRecovering, Channel: "chan", LastProgressAt: tenMinAgo,
		ReportsSinceProgress: 17, RecoveryStageName: "session_recreate",
	}, enTR(t))
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
	}, enTR(t))
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
	views := buildDropCampaignViews([]*models.Campaign{tracked, other}, dropHealthByCampaign(snap, enTR(t)), nil, enTR(t))

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
	if m := dropHealthByCampaign(health.ProgressSnapshot{Enabled: false, Drops: snap.Drops}, enTR(t)); m != nil {
		t.Fatalf("disabled watchdog must yield no badges, got %+v", m)
	}
}

// TestTemplatesRenderDropsAndCards ensures the new templates parse and execute
// against their view models (embedded via the same globs the server uses).
func TestTemplatesRenderDropsAndCards(t *testing.T) {
	partials := testPartials(t)

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
	if !strings.Contains(out, "Дроп только для канала") || !strings.Contains(out, "90 мин осталось") {
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
	pages, _ := testLoadTemplates(t)
	if pages["drops.html"] == nil {
		t.Fatal("drops.html page template not loaded")
	}
}

// TestBuildDropPolicyByCampaign verifies the policy decision + per-drop rule
// merge, including the reward key and the subscriber-only "known" flag that
// drives the "no effect" marker.
func TestBuildDropPolicyByCampaign(t *testing.T) {
	game := &models.Game{ID: "g1", Name: "Game One"}
	c := &models.Campaign{
		ID: "c1", Name: "Camp", Game: game,
		Drops: []*models.Drop{{Name: "Cool Skin", MinutesRequired: 60, CurrentMinutesWatched: 10}},
	}
	decisions := []policy.Decision{{
		CampaignID: "c1", Name: "Camp", Total: 180, Status: policy.StatusAtRisk,
		Factors: []policy.Factor{{Label: "channel-restricted campaign", Points: 100}},
	}}
	rules := map[string]config.DropRule{
		"g1::cool skin": {Skip: true},
	}
	views := buildDropPolicyByCampaign([]*models.Campaign{c}, decisions, rules, enTR(t))

	v := views["c1"]
	if v == nil {
		t.Fatal("expected a policy view for c1")
	}
	if v.Total != 180 || v.StatusLabel != "AT RISK" {
		t.Fatalf("unexpected badge: %+v", v)
	}
	if v.RewardKey != "g1::cool skin" || !v.Skip {
		t.Fatalf("expected the reward key and Skip rule merged, got key=%q skip=%v", v.RewardKey, v.Skip)
	}
	if v.SubscriberOnlyKnown {
		t.Error("subscriber-only should be unknown (Twitch reported no flag)")
	}
	if len(v.Factors) != 1 || v.Factors[0].Points != 100 {
		t.Fatalf("expected the breakdown factor merged, got %+v", v.Factors)
	}
}
