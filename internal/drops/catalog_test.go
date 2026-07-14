package drops

import (
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
)

func newTestCatalog(t *testing.T) *CampaignCatalog {
	t.Helper()
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	cat, err := NewCampaignCatalog(db)
	if err != nil {
		t.Fatalf("new catalog: %v", err)
	}
	return cat
}

// catalogRow reads the raw persisted columns for a campaign_id.
func catalogRow(t *testing.T, cat *CampaignCatalog, id string) (startMs, endMs, firstMs, lastMs int64, status string, claimed int) {
	t.Helper()
	err := cat.db.QueryRow(
		"SELECT start_at, end_at, first_seen_at, last_seen_at, status, claimed FROM drop_campaigns WHERE campaign_id = ?", id,
	).Scan(&startMs, &endMs, &firstMs, &lastMs, &status, &claimed)
	if err != nil {
		t.Fatalf("read row %s: %v", id, err)
	}
	return
}

func TestCatalogUpsertAndPastFiltersByEnd(t *testing.T) {
	cat := newTestCatalog(t)
	base := time.Date(2031, 6, 1, 12, 0, 0, 0, time.UTC)
	cat.now = func() time.Time { return base }

	// One ended campaign, one still-running (end in the future).
	must(t, cat.Upsert(CatalogRecord{CampaignID: "cat-past", CampaignKey: "g::past", Name: "Past One",
		StartAt: base.Add(-72 * time.Hour), EndAt: base.Add(-24 * time.Hour), Status: "EXPIRED"}))
	must(t, cat.Upsert(CatalogRecord{CampaignID: "cat-live", CampaignKey: "g::live", Name: "Live One",
		StartAt: base.Add(-24 * time.Hour), EndAt: base.Add(24 * time.Hour), Status: "ACTIVE"}))

	past, err := cat.Past()
	if err != nil {
		t.Fatalf("past: %v", err)
	}
	if len(past) != 1 || past[0].CampaignID != "cat-past" {
		t.Fatalf("past must contain only the ended campaign, got %+v", past)
	}
}

func TestCatalogFirstSeenImmutableLastSeenUpdates(t *testing.T) {
	cat := newTestCatalog(t)
	t1 := time.Date(2031, 1, 1, 8, 0, 0, 0, time.UTC)
	t2 := t1.Add(7 * 24 * time.Hour)

	cat.now = func() time.Time { return t1 }
	must(t, cat.Upsert(CatalogRecord{CampaignID: "fs-1", CampaignKey: "g::fs", Name: "N", Status: "ACTIVE",
		StartAt: t1, EndAt: t1.Add(48 * time.Hour)}))

	// Later observation of the SAME campaign_id with a new status.
	cat.now = func() time.Time { return t2 }
	must(t, cat.Upsert(CatalogRecord{CampaignID: "fs-1", CampaignKey: "g::fs", Name: "N", Status: "EXPIRED",
		StartAt: t1, EndAt: t1.Add(48 * time.Hour), Claimed: true}))

	_, _, firstMs, lastMs, status, claimed := catalogRow(t, cat, "fs-1")
	if firstMs != t1.UnixMilli() {
		t.Errorf("first_seen_at moved: got %d, want %d (must stay first observation)", firstMs, t1.UnixMilli())
	}
	if lastMs != t2.UnixMilli() {
		t.Errorf("last_seen_at not updated: got %d, want %d", lastMs, t2.UnixMilli())
	}
	if status != "EXPIRED" {
		t.Errorf("status not updated: %q", status)
	}
	if claimed != 1 {
		t.Errorf("claimed not updated: %d", claimed)
	}
}

func TestCatalogCoalesceGuardKeepsDates(t *testing.T) {
	cat := newTestCatalog(t)
	base := time.Date(2031, 3, 1, 0, 0, 0, 0, time.UTC)
	cat.now = func() time.Time { return base }

	start := base.Add(-48 * time.Hour)
	end := base.Add(-1 * time.Hour)
	must(t, cat.Upsert(CatalogRecord{CampaignID: "cg-1", CampaignKey: "g::cg", Name: "N", Status: "EXPIRED",
		StartAt: start, EndAt: end}))

	// A later date-less observation (Twitch omitted dates) must NOT zero them.
	must(t, cat.Upsert(CatalogRecord{CampaignID: "cg-1", CampaignKey: "g::cg", Name: "N", Status: "EXPIRED"}))

	startMs, endMs, _, _, _, _ := catalogRow(t, cat, "cg-1")
	if startMs != start.UnixMilli() {
		t.Errorf("start_at was clobbered by a date-less update: got %d, want %d", startMs, start.UnixMilli())
	}
	if endMs != end.UnixMilli() {
		t.Errorf("end_at was clobbered by a date-less update: got %d, want %d", endMs, end.UnixMilli())
	}
}

func TestCatalogClaimedRoundTrips(t *testing.T) {
	cat := newTestCatalog(t)
	base := time.Date(2031, 4, 1, 0, 0, 0, 0, time.UTC)
	cat.now = func() time.Time { return base }
	must(t, cat.Upsert(CatalogRecord{CampaignID: "cl-1", CampaignKey: "g::cl", Name: "N", Status: "EXPIRED",
		StartAt: base.Add(-48 * time.Hour), EndAt: base.Add(-1 * time.Hour), Claimed: true}))

	past, err := cat.Past()
	if err != nil {
		t.Fatalf("past: %v", err)
	}
	var found *CatalogRecord
	for i := range past {
		if past[i].CampaignID == "cl-1" {
			found = &past[i]
		}
	}
	if found == nil || !found.Claimed {
		t.Fatalf("claimed flag did not round-trip: %+v", found)
	}
}

func TestCatalogRecurringInstancesGroupedByKeyNewestFirst(t *testing.T) {
	cat := newTestCatalog(t)
	base := time.Date(2031, 5, 1, 0, 0, 0, 0, time.UTC)
	cat.now = func() time.Time { return base }

	// Two instances of the same recurring campaign (same key, different ids).
	must(t, cat.Upsert(CatalogRecord{CampaignID: "wk-1", CampaignKey: "g::weekly", Name: "Weekly",
		StartAt: base.Add(-14 * 24 * time.Hour), EndAt: base.Add(-13 * 24 * time.Hour), Status: "EXPIRED"}))
	must(t, cat.Upsert(CatalogRecord{CampaignID: "wk-2", CampaignKey: "g::weekly", Name: "Weekly",
		StartAt: base.Add(-7 * 24 * time.Hour), EndAt: base.Add(-6 * 24 * time.Hour), Status: "EXPIRED"}))

	past, err := cat.Past()
	if err != nil {
		t.Fatalf("past: %v", err)
	}
	// Both present, adjacent, newest-ended first (wk-2 before wk-1).
	var idx1, idx2 = -1, -1
	for i, r := range past {
		if r.CampaignID == "wk-1" {
			idx1 = i
		}
		if r.CampaignID == "wk-2" {
			idx2 = i
		}
	}
	if idx1 < 0 || idx2 < 0 {
		t.Fatalf("both instances must be present: idx1=%d idx2=%d", idx1, idx2)
	}
	if idx2 > idx1 {
		t.Errorf("newest-ended instance (wk-2) must come first: idx2=%d idx1=%d", idx2, idx1)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
