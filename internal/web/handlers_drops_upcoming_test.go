package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/drops"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

func getUpcomingBody(t *testing.T, srv *Server, lang string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/drops/upcoming", nil)
	if lang != "" {
		req.AddCookie(&http.Cookie{Name: langCookieName, Value: lang})
	}
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

func upcomingModel(id, name, gameName string, start, end time.Time, rewards ...string) *models.Campaign {
	c := &models.Campaign{ID: id, Name: name, StartAt: start, EndAt: end}
	if gameName != "" {
		c.Game = &models.Game{Name: gameName}
	}
	for i, r := range rewards {
		c.Drops = append(c.Drops, &models.Drop{Name: fmt.Sprintf("drop-%d", i), Benefit: r})
	}
	return c
}

// Test 8: the never-synced state has its own RU/EN text, not the active-tab
// empty message.
func TestUpcomingNeverSyncedState(t *testing.T) {
	srv := newStatsTestServer(t)
	srv.SetDropCatalogProvider(&fakeCatalogProvider{}) // zero sync status

	en := getUpcomingBody(t, srv, "en")
	if !strings.Contains(en, "has not been synchronized yet") {
		t.Errorf("EN never-synced text missing:\n%s", en)
	}
	if strings.Contains(en, "No active drop campaigns") {
		t.Errorf("upcoming tab must not reuse the active empty message")
	}
	ru := getUpcomingBody(t, srv, "ru")
	if !strings.Contains(ru, "ещё не синхронизированы") {
		t.Errorf("RU never-synced text missing:\n%s", ru)
	}
}

// Test 9: the successful-empty state has Upcoming-specific RU/EN text plus the
// last successful sync time.
func TestUpcomingEmptyState(t *testing.T) {
	srv := newStatsTestServer(t)
	now := time.Now()
	srv.SetDropCatalogProvider(&fakeCatalogProvider{
		syncStatus: drops.SyncStatus{LastSyncAt: now, LastSuccessAt: now},
	})

	en := getUpcomingBody(t, srv, "en")
	if !strings.Contains(en, "no upcoming campaigns at the last synchronization") {
		t.Errorf("EN empty text missing:\n%s", en)
	}
	if !strings.Contains(en, "Last successful synchronization") {
		t.Errorf("empty state must show the last successful sync time")
	}
	ru := getUpcomingBody(t, srv, "ru")
	if !strings.Contains(ru, "не сообщил о предстоящих кампаниях") {
		t.Errorf("RU empty text missing:\n%s", ru)
	}
}

// Test 10: a failed refresh with cached campaigns keeps the cards and shows a
// stale/error note.
func TestUpcomingStaleWithCache(t *testing.T) {
	srv := newStatsTestServer(t)
	now := time.Now()
	c := upcomingModel("wot", "WoT Future", "World of Tanks", now.Add(48*time.Hour), now.Add(96*time.Hour), "Premium Tank")
	srv.SetDropCatalogProvider(&fakeCatalogProvider{
		relevantUpcoming: []*models.Campaign{c},
		syncStatus:       drops.SyncStatus{LastSyncAt: now, LastSuccessAt: now.Add(-time.Hour), LastError: "boom"},
	})

	en := getUpcomingBody(t, srv, "en")
	if !strings.Contains(en, "WoT Future") {
		t.Errorf("stale state must keep the cached cards:\n%s", en)
	}
	if !strings.Contains(en, "Could not refresh") {
		t.Errorf("stale state must show the refresh-failed note:\n%s", en)
	}
}

// Test 10b: a failed refresh with NO cache shows the no-cache error message.
func TestUpcomingStaleNoCache(t *testing.T) {
	srv := newStatsTestServer(t)
	now := time.Now()
	srv.SetDropCatalogProvider(&fakeCatalogProvider{
		syncStatus: drops.SyncStatus{LastSyncAt: now, LastSuccessAt: now.Add(-time.Hour), LastError: "boom"},
	})

	en := getUpcomingBody(t, srv, "en")
	if !strings.Contains(en, "could not be refreshed") {
		t.Errorf("no-cache error state text missing:\n%s", en)
	}
}

// Test 11 + 12: a populated card shows campaign/game/local start/starts-in/end/
// rewards and the Upcoming badge, and does NOT show any active-farming UI
// (progress bar, watched minutes, health).
func TestUpcomingPopulatedCardContent(t *testing.T) {
	srv := newStatsTestServer(t)
	now := time.Now()
	// A <24h start so the relative label is "starts in Xh" (the far-future branch
	// renders a calendar date instead).
	c := upcomingModel("wot", "WoT Future", "World of Tanks",
		now.Add(5*time.Hour), now.Add(48*time.Hour), "Premium Tank")
	srv.SetDropCatalogProvider(&fakeCatalogProvider{
		relevantUpcoming: []*models.Campaign{c},
		syncStatus:       drops.SyncStatus{LastSyncAt: now, LastSuccessAt: now},
	})

	en := getUpcomingBody(t, srv, "en")
	for _, want := range []string{"WoT Future", "World of Tanks", "Starts:", "Ends:", "Premium Tank", "Upcoming", "starts in"} {
		if !strings.Contains(en, want) {
			t.Errorf("populated card missing %q:\n%s", want, en)
		}
	}
	// No active-farming presentation on an upcoming card.
	for _, forbidden := range []string{"progress-track", "progress-fill", "min watched", "HEALTHY", "RECOVERING", "STALLED"} {
		if strings.Contains(en, forbidden) {
			t.Errorf("upcoming card must not show active-farming UI (%q):\n%s", forbidden, en)
		}
	}
}

// Test 15: the endpoint re-reads the backend snapshot each time, so a manual
// full sync's result appears on the next refresh without a page reload.
func TestUpcomingReflectsBackendChange(t *testing.T) {
	srv := newStatsTestServer(t)
	now := time.Now()
	fake := &fakeCatalogProvider{
		relevantUpcoming: []*models.Campaign{upcomingModel("first", "First Campaign", "World of Tanks", now.Add(24*time.Hour), now.Add(48*time.Hour))},
		syncStatus:       drops.SyncStatus{LastSyncAt: now, LastSuccessAt: now},
	}
	srv.SetDropCatalogProvider(fake)

	if body := getUpcomingBody(t, srv, "en"); !strings.Contains(body, "First Campaign") {
		t.Fatalf("first snapshot must render:\n%s", body)
	}

	// Simulate a fresh full sync publishing a different campaign.
	fake.relevantUpcoming = []*models.Campaign{upcomingModel("second", "Second Campaign", "World of Tanks", now.Add(24*time.Hour), now.Add(48*time.Hour))}
	body := getUpcomingBody(t, srv, "en")
	if !strings.Contains(body, "Second Campaign") || strings.Contains(body, "First Campaign") {
		t.Fatalf("refresh must reflect the new backend snapshot:\n%s", body)
	}
}

// Tests 14 + 16-18 (UI level): the handler reads the RELEVANT (game-filtered)
// upcoming list, not the raw one — a foreign campaign present only in the raw
// list never renders, and no Twitch call is made (the tab only reads the
// provider's in-memory snapshot).
func TestUpcomingUsesRelevantList(t *testing.T) {
	srv := newStatsTestServer(t)
	now := time.Now()
	fake := &fakeCatalogProvider{
		// The raw list carries a foreign campaign that must NEVER surface.
		upcoming:         []*models.Campaign{upcomingModel("foreign", "FOREIGN_SHOULD_NOT_APPEAR", "War Thunder", now.Add(24*time.Hour), now.Add(48*time.Hour))},
		relevantUpcoming: []*models.Campaign{upcomingModel("wot", "WoT Future", "World of Tanks", now.Add(24*time.Hour), now.Add(48*time.Hour))},
		syncStatus:       drops.SyncStatus{LastSyncAt: now, LastSuccessAt: now},
	}
	srv.SetDropCatalogProvider(fake)

	body := getUpcomingBody(t, srv, "en")
	if !strings.Contains(body, "WoT Future") {
		t.Errorf("relevant upcoming campaign must render:\n%s", body)
	}
	if strings.Contains(body, "FOREIGN_SHOULD_NOT_APPEAR") {
		t.Errorf("the upcoming tab must use the relevant (filtered) list, not the raw one")
	}
}

// Timezone: absolute times render in the configured display location, not UTC.
func TestUpcomingRendersConfiguredTimezone(t *testing.T) {
	srv := newStatsTestServer(t)
	loc := time.FixedZone("TST", 2*60*60) // UTC+02:00
	srv.SetDisplayLocation(loc)

	start := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC) // 14:00 in TST
	c := upcomingModel("wot", "WoT Future", "World of Tanks", start, start.Add(48*time.Hour))
	srv.SetDropCatalogProvider(&fakeCatalogProvider{
		relevantUpcoming: []*models.Campaign{c},
		syncStatus:       drops.SyncStatus{LastSyncAt: time.Now(), LastSuccessAt: time.Now()},
	})

	body := getUpcomingBody(t, srv, "en")
	if !strings.Contains(body, "14:00 TST") {
		t.Errorf("start time must render in the configured time zone (expected 14:00 TST):\n%s", body)
	}
}
