package notifications

import "testing"

// Historical migration-3 storage is intentionally preserved. Ordinary config
// saves must neither rewrite its retired opt-in column nor delete durable rows
// from the retired dedupe table.
func TestLegacyDropsNotificationStorageIsPreservedButNotWritten(t *testing.T) {
	repo, err := NewRepository(testDBHandle)
	if err != nil {
		t.Fatalf("new repository: %v", err)
	}
	const campaignID = "legacy-preservation-proof"
	t.Cleanup(func() {
		_, _ = testDBHandle.Exec("UPDATE notification_config SET upcoming_drops_enabled = 0 WHERE id = 1")
		_, _ = testDBHandle.Exec("DELETE FROM upcoming_campaign_notifications WHERE campaign_id = ?", campaignID)
	})

	if _, err := testDBHandle.Exec("UPDATE notification_config SET upcoming_drops_enabled = 1 WHERE id = 1"); err != nil {
		t.Fatalf("seed legacy column: %v", err)
	}
	if _, err := testDBHandle.Exec(`
		INSERT OR REPLACE INTO upcoming_campaign_notifications
			(campaign_id, notification_type, status, first_seen_at, attempts)
		VALUES (?, 'upcoming_drop_campaign', 'notified', 123, 0)`, campaignID); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	cfg, err := repo.GetConfig()
	if err != nil {
		t.Fatalf("get active config: %v", err)
	}
	if err := repo.SaveConfig(cfg); err != nil {
		t.Fatalf("save active config: %v", err)
	}

	var legacyOptIn, legacyRows int
	if err := testDBHandle.QueryRow("SELECT upcoming_drops_enabled FROM notification_config WHERE id = 1").Scan(&legacyOptIn); err != nil {
		t.Fatalf("read legacy column: %v", err)
	}
	if err := testDBHandle.QueryRow("SELECT COUNT(*) FROM upcoming_campaign_notifications WHERE campaign_id = ?", campaignID).Scan(&legacyRows); err != nil {
		t.Fatalf("count legacy rows: %v", err)
	}
	if legacyOptIn != 1 || legacyRows != 1 {
		t.Fatalf("ordinary config save changed historical storage: opt-in=%d rows=%d, want 1/1", legacyOptIn, legacyRows)
	}
}
