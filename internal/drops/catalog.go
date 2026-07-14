package drops

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// CampaignCatalog is the durable record of every drop campaign the miner has
// observed (current and upcoming), so the Drops page can show a "Past" tab of
// campaigns that have since expired and dropped off Twitch's dashboard (which
// only returns active + upcoming). One row per campaign INSTANCE, keyed by the
// Twitch campaign id; a recurring campaign therefore accumulates one row per
// occurrence, grouped in the UI by campaign_key (game + campaign name). The
// table is deliberately excluded from the retention sweep — the catalog's whole
// point is long memory, and it grows only one row per campaign instance.
type CampaignCatalog struct {
	db *database.DB
	// now is injectable so tests can control observation timestamps (to prove
	// first_seen_at is immutable across upserts and past filtering by end_at).
	now func() time.Time
}

type catalogModule struct{}

func (catalogModule) Name() string { return "drop_catalog" }

func (catalogModule) Migrations() []database.Migration {
	return []database.Migration{
		{
			Version:     1,
			Description: "Create drop_campaigns catalog table",
			// Additive, standalone table — touches nothing else, safe on a
			// populated DB. campaign_id is the PRIMARY KEY (unique per instance)
			// so the upsert's ON CONFLICT(campaign_id) collapses repeat
			// observations of the same instance onto one row. The (campaign_key,
			// end_at) index makes the grouped "past" query efficient.
			SQL: `
				CREATE TABLE IF NOT EXISTS drop_campaigns (
					campaign_id   TEXT PRIMARY KEY,
					campaign_key  TEXT NOT NULL,
					name          TEXT NOT NULL,
					game          TEXT,
					start_at      INTEGER NOT NULL DEFAULT 0,
					end_at        INTEGER NOT NULL DEFAULT 0,
					status        TEXT,
					claimed       INTEGER NOT NULL DEFAULT 0,
					first_seen_at INTEGER NOT NULL,
					last_seen_at  INTEGER NOT NULL
				);

				CREATE INDEX IF NOT EXISTS idx_drop_campaigns_key_end ON drop_campaigns(campaign_key, end_at);
			`,
		},
	}
}

// CatalogRecord is one observed campaign instance for the catalog.
type CatalogRecord struct {
	CampaignID  string
	CampaignKey string
	Name        string
	Game        string
	StartAt     time.Time
	EndAt       time.Time
	Status      string
	Claimed     bool
	FirstSeenAt time.Time
	LastSeenAt  time.Time
}

// NewCampaignCatalog registers the catalog module against db and returns a
// store for recording and querying observed campaigns.
func NewCampaignCatalog(db *database.DB) (*CampaignCatalog, error) {
	if err := db.RegisterModule(catalogModule{}); err != nil {
		return nil, fmt.Errorf("failed to register drop_catalog module: %w", err)
	}
	return &CampaignCatalog{db: db, now: time.Now}, nil
}

// Upsert records (or refreshes) one observed campaign instance. On first sight it
// inserts with first_seen_at = last_seen_at = now. On a repeat observation of the
// same campaign_id it updates last_seen_at, status, claimed, name, and game, and
// refreshes start_at/end_at ONLY when the new observation actually carries a date
// (the CASE guard) so a later date-less Twitch response can never zero out good
// dates. first_seen_at is never in the SET list, so it keeps the first-seen
// moment across every subsequent upsert.
func (c *CampaignCatalog) Upsert(rec CatalogRecord) error {
	now := c.now().UnixMilli()
	claimed := 0
	if rec.Claimed {
		claimed = 1
	}
	_, err := c.db.Exec(`
		INSERT INTO drop_campaigns
			(campaign_id, campaign_key, name, game, start_at, end_at, status, claimed, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(campaign_id) DO UPDATE SET
			last_seen_at = excluded.last_seen_at,
			status       = excluded.status,
			claimed      = excluded.claimed,
			name         = excluded.name,
			game         = excluded.game,
			start_at     = CASE WHEN excluded.start_at > 0 THEN excluded.start_at ELSE start_at END,
			end_at       = CASE WHEN excluded.end_at   > 0 THEN excluded.end_at   ELSE end_at   END`,
		rec.CampaignID, rec.CampaignKey, rec.Name, rec.Game,
		msOrZero(rec.StartAt), msOrZero(rec.EndAt), rec.Status, claimed,
		now, now,
	)
	return err
}

// Past returns campaign instances whose end has passed (end_at < now), ordered
// so recurring instances of the same campaign_key are adjacent (newest first)
// for grouped rendering. Instances with no recorded end date are omitted (they
// cannot be classified as past).
func (c *CampaignCatalog) Past() ([]CatalogRecord, error) {
	nowMs := c.now().UnixMilli()
	rows, err := c.db.Query(`
		SELECT campaign_id, campaign_key, name, COALESCE(game, ''), start_at, end_at,
		       COALESCE(status, ''), claimed, first_seen_at, last_seen_at
		FROM drop_campaigns
		WHERE end_at > 0 AND end_at < ?
		ORDER BY campaign_key ASC, end_at DESC`,
		nowMs,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []CatalogRecord
	for rows.Next() {
		var r CatalogRecord
		var startMs, endMs, firstMs, lastMs int64
		var claimed int
		if err := rows.Scan(&r.CampaignID, &r.CampaignKey, &r.Name, &r.Game,
			&startMs, &endMs, &r.Status, &claimed, &firstMs, &lastMs); err != nil {
			return nil, err
		}
		r.StartAt = msToTime(startMs)
		r.EndAt = msToTime(endMs)
		r.FirstSeenAt = msToTime(firstMs)
		r.LastSeenAt = msToTime(lastMs)
		r.Claimed = claimed != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// recordCatalog upserts every observed campaign (the claim-enriched active set
// plus the upcoming set) into the durable catalog. A nil catalog is a no-op;
// individual write errors are logged and never disrupt the sync.
func (d *DropsTracker) recordCatalog(active, upcoming []*models.Campaign) {
	if d.catalog == nil {
		return
	}
	record := func(campaigns []*models.Campaign) {
		for _, c := range campaigns {
			if c == nil || c.ID == "" {
				continue
			}
			if err := d.catalog.Upsert(catalogRecordFromCampaign(c)); err != nil {
				slog.Error("Failed to record campaign in catalog", "campaign", c.Name, "campaignID", c.ID, "error", err)
			}
		}
	}
	record(active)
	record(upcoming)
}

// catalogRecordFromCampaign builds a CatalogRecord from an observed campaign.
// The campaign_key groups recurring/regional instances that share a game and
// campaign name (a different campaign_id each time) under one heading in the UI.
func catalogRecordFromCampaign(c *models.Campaign) CatalogRecord {
	gameID, gameName := "", ""
	if c.Game != nil {
		gameID = c.Game.ID
		gameName = c.Game.DisplayName
		if gameName == "" {
			gameName = c.Game.Name
		}
	}
	return CatalogRecord{
		CampaignID:  c.ID,
		CampaignKey: models.NormalizeRewardKey(gameID, c.Name),
		Name:        c.Name,
		Game:        gameName,
		StartAt:     c.StartAt,
		EndAt:       c.EndAt,
		Status:      string(c.Status),
		Claimed:     c.ClaimStatus == models.CampaignClaimStatusAlreadyClaimed,
	}
}

// msOrZero maps a time to Unix millis, mapping the zero time to 0 (not the huge
// negative value time.Time{}.UnixMilli() would give) so the upsert's
// "> 0" date guard and the past query behave correctly.
func msOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// msToTime is the inverse: 0 (or negative) becomes the zero time.
func msToTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}
