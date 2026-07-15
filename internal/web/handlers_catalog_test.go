package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/drops"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

func TestStartsInLabel(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		start time.Time
		want  string
	}{
		{time.Time{}, ""},
		{now.Add(30 * time.Minute), "starts in 30m"},
		{now.Add(5 * time.Hour), "starts in 5h"},
		{now.Add(-time.Minute), "starting now"},
	}
	for _, tc := range cases {
		if got := startsInLabel(tc.start, now, enTR(t)); got != tc.want {
			t.Errorf("startsInLabel(%v) = %q, want %q", tc.start, got, tc.want)
		}
	}
	// A multi-day-out start renders a date.
	if got := startsInLabel(now.Add(72*time.Hour), now, enTR(t)); !strings.HasPrefix(got, "starts ") {
		t.Errorf("far-future start should render a date, got %q", got)
	}
}

func TestBuildPastGroupsGroupsByKey(t *testing.T) {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	// Records arrive ordered (campaign_key, end_at DESC) as the store returns them.
	records := []drops.CatalogRecord{
		{CampaignID: "wk-2", CampaignKey: "g::weekly", Name: "Weekly", Game: "Game", EndAt: base.Add(-6 * 24 * time.Hour), Claimed: true},
		{CampaignID: "wk-1", CampaignKey: "g::weekly", Name: "Weekly", Game: "Game", EndAt: base.Add(-13 * 24 * time.Hour), Claimed: false},
		{CampaignID: "one", CampaignKey: "g::once", Name: "Once", Game: "Game", EndAt: base.Add(-2 * 24 * time.Hour), Claimed: true},
	}
	groups := buildPastGroups(records, enTR(t))

	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d: %+v", len(groups), groups)
	}
	weekly := groups[0]
	if weekly.CampaignKey != "g::weekly" || weekly.Count != 2 {
		t.Fatalf("weekly group wrong: %+v", weekly)
	}
	if weekly.ClaimedCount != 1 {
		t.Errorf("weekly claimed count = %d, want 1", weekly.ClaimedCount)
	}
	if len(weekly.Instances) != 2 || weekly.Instances[0].CampaignID != "wk-2" {
		t.Errorf("weekly instances wrong (newest first expected): %+v", weekly.Instances)
	}
	once := groups[1]
	if once.Count != 1 || once.ClaimedCount != 1 {
		t.Errorf("once group wrong: %+v", once)
	}
}

// fakeCatalogProvider satisfies web.DropCatalogProvider for endpoint tests.
type fakeCatalogProvider struct {
	upcoming []*models.Campaign
	past     []drops.CatalogRecord
}

func (f *fakeCatalogProvider) UpcomingCampaigns() []*models.Campaign         { return f.upcoming }
func (f *fakeCatalogProvider) PastCampaigns() ([]drops.CatalogRecord, error) { return f.past, nil }

func TestDropsPastEndpointRenders(t *testing.T) {
	srv := newStatsTestServer(t)
	srv.SetDropCatalogProvider(&fakeCatalogProvider{
		past: []drops.CatalogRecord{
			{CampaignID: "p1", CampaignKey: "g::p", Name: "Past Campaign", Game: "Game",
				StartAt: time.Now().Add(-72 * time.Hour), EndAt: time.Now().Add(-24 * time.Hour), Claimed: true},
		},
	})

	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/drops/past", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Past Campaign") {
		t.Errorf("past tab must render the campaign name, got:\n%s", body)
	}
}

func TestDropsPastEndpointEmptyState(t *testing.T) {
	srv := newStatsTestServer(t)
	srv.SetDropCatalogProvider(&fakeCatalogProvider{})

	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/drops/past", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Прошедших кампаний пока нет") {
		t.Errorf("empty past tab must show the empty state, got:\n%s", rec.Body.String())
	}
}

func TestDropsUpcomingEndpointRenders(t *testing.T) {
	srv := newStatsTestServer(t)
	c := models.NewCampaignFromGQL(map[string]interface{}{
		"id": "u1", "name": "Upcoming Campaign",
		"game": map[string]interface{}{"id": "g", "name": "Game"},
	})
	c.StartAt = time.Now().Add(48 * time.Hour)
	srv.SetDropCatalogProvider(&fakeCatalogProvider{upcoming: []*models.Campaign{c}})

	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/drops/upcoming", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Upcoming Campaign") {
		t.Errorf("upcoming tab must render the campaign, got:\n%s", rec.Body.String())
	}
}
