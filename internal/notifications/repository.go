package notifications

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
)

type Repository struct {
	db *database.DB
	mu sync.RWMutex
}

type NotificationsModule struct{}

func (m *NotificationsModule) Name() string {
	return "notifications"
}

func (m *NotificationsModule) Migrations() []database.Migration {
	return []database.Migration{
		{
			Version:     1,
			Description: "Create notification_config and point_rules tables",
			SQL: `
				CREATE TABLE IF NOT EXISTS notification_config (
					id INTEGER PRIMARY KEY CHECK (id = 1),
					mentions_channel_id TEXT DEFAULT '',
					points_channel_id TEXT DEFAULT '',
					online_channel_id TEXT DEFAULT '',
					offline_channel_id TEXT DEFAULT '',
					mentions_enabled INTEGER DEFAULT 0,
					mentions_all_chats INTEGER DEFAULT 1,
					mentions_streamers TEXT DEFAULT '[]',
					online_enabled INTEGER DEFAULT 0,
					online_all_streamers INTEGER DEFAULT 1,
					online_streamers TEXT DEFAULT '[]',
					offline_enabled INTEGER DEFAULT 0,
					offline_all_streamers INTEGER DEFAULT 1,
					offline_streamers TEXT DEFAULT '[]'
				);

				CREATE TABLE IF NOT EXISTS point_rules (
					id INTEGER PRIMARY KEY AUTOINCREMENT,
					streamer TEXT NOT NULL,
					threshold INTEGER NOT NULL,
					delete_on_trigger INTEGER DEFAULT 0,
					triggered INTEGER DEFAULT 0
				);

				INSERT OR IGNORE INTO notification_config (id) VALUES (1);
			`,
		},
		{
			Version:     2,
			Description: "Add system channel columns for reauth/connection-health notifications",
			// Run (not SQL): two non-idempotent ALTERs. Each column is
			// guarded independently so a DB where only the first ALTER
			// landed (pre-transactional-migrations crash window between the
			// two statements) self-heals by adding just the missing column.
			Run: func(tx *sql.Tx) error {
				if err := database.AddColumnIfMissing(tx, "notification_config", "system_channel_id", "TEXT DEFAULT ''"); err != nil {
					return err
				}
				return database.AddColumnIfMissing(tx, "notification_config", "system_enabled", "INTEGER DEFAULT 1")
			},
		},
		{
			Version:     3,
			Description: "Add upcoming-drops opt-in column and durable upcoming-campaign notification dedupe table",
			// Additive and idempotent: AddColumnIfMissing self-heals a
			// half-applied schema, and CREATE TABLE IF NOT EXISTS is a no-op on
			// re-run. Default 0 keeps the opt-in OFF for existing installs. The
			// dedupe table's (campaign_id, notification_type) PRIMARY KEY is the
			// idempotency key that makes "notify once per campaign" survive
			// restarts and reject concurrent duplicate inserts.
			Run: func(tx *sql.Tx) error {
				if err := database.AddColumnIfMissing(tx, "notification_config", "upcoming_drops_enabled", "INTEGER DEFAULT 0"); err != nil {
					return err
				}
				_, err := tx.Exec(`
					CREATE TABLE IF NOT EXISTS upcoming_campaign_notifications (
						campaign_id       TEXT NOT NULL,
						notification_type TEXT NOT NULL,
						status            TEXT NOT NULL,
						first_seen_at     INTEGER NOT NULL,
						notified_at       INTEGER,
						last_error_at     INTEGER,
						attempts          INTEGER NOT NULL DEFAULT 0,
						PRIMARY KEY (campaign_id, notification_type)
					);
				`)
				return err
			},
		},
	}
}

func NewRepository(db *database.DB) (*Repository, error) {
	module := &NotificationsModule{}
	if err := db.RegisterModule(module); err != nil {
		return nil, fmt.Errorf("failed to register notifications module: %w", err)
	}

	return &Repository{db: db}, nil
}

func (r *Repository) Close() error {
	return nil
}

func (r *Repository) GetConfig() (*NotificationConfig, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	row := r.db.QueryRow(`
		SELECT
			mentions_channel_id, points_channel_id, online_channel_id, offline_channel_id,
			mentions_enabled, mentions_all_chats, mentions_streamers,
			online_enabled, online_all_streamers, online_streamers,
			offline_enabled, offline_all_streamers, offline_streamers,
			system_channel_id, system_enabled, upcoming_drops_enabled
		FROM notification_config WHERE id = 1
	`)

	var cfg NotificationConfig
	var mentionsStreamersJSON, onlineStreamersJSON, offlineStreamersJSON string

	err := row.Scan(
		&cfg.MentionsChannelID, &cfg.PointsChannelID, &cfg.OnlineChannelID, &cfg.OfflineChannelID,
		&cfg.MentionsEnabled, &cfg.MentionsAllChats, &mentionsStreamersJSON,
		&cfg.OnlineEnabled, &cfg.OnlineAllStreamers, &onlineStreamersJSON,
		&cfg.OfflineEnabled, &cfg.OfflineAllStreamers, &offlineStreamersJSON,
		&cfg.SystemChannelID, &cfg.SystemEnabled, &cfg.UpcomingDropsEnabled,
	)
	if err != nil {
		return nil, err
	}

	_ = json.Unmarshal([]byte(mentionsStreamersJSON), &cfg.MentionsStreamers)
	_ = json.Unmarshal([]byte(onlineStreamersJSON), &cfg.OnlineStreamers)
	_ = json.Unmarshal([]byte(offlineStreamersJSON), &cfg.OfflineStreamers)

	if cfg.MentionsStreamers == nil {
		cfg.MentionsStreamers = []string{}
	}
	if cfg.OnlineStreamers == nil {
		cfg.OnlineStreamers = []string{}
	}
	if cfg.OfflineStreamers == nil {
		cfg.OfflineStreamers = []string{}
	}

	return &cfg, nil
}

func (r *Repository) SaveConfig(cfg *NotificationConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	mentionsStreamersJSON, _ := json.Marshal(cfg.MentionsStreamers)
	onlineStreamersJSON, _ := json.Marshal(cfg.OnlineStreamers)
	offlineStreamersJSON, _ := json.Marshal(cfg.OfflineStreamers)

	_, err := r.db.Exec(`
		UPDATE notification_config SET
			mentions_channel_id = ?,
			points_channel_id = ?,
			online_channel_id = ?,
			offline_channel_id = ?,
			mentions_enabled = ?,
			mentions_all_chats = ?,
			mentions_streamers = ?,
			online_enabled = ?,
			online_all_streamers = ?,
			online_streamers = ?,
			offline_enabled = ?,
			offline_all_streamers = ?,
			offline_streamers = ?,
			system_channel_id = ?,
			system_enabled = ?,
			upcoming_drops_enabled = ?
		WHERE id = 1
	`,
		cfg.MentionsChannelID, cfg.PointsChannelID, cfg.OnlineChannelID, cfg.OfflineChannelID,
		cfg.MentionsEnabled, cfg.MentionsAllChats, string(mentionsStreamersJSON),
		cfg.OnlineEnabled, cfg.OnlineAllStreamers, string(onlineStreamersJSON),
		cfg.OfflineEnabled, cfg.OfflineAllStreamers, string(offlineStreamersJSON),
		cfg.SystemChannelID, cfg.SystemEnabled, cfg.UpcomingDropsEnabled,
	)

	return err
}

func (r *Repository) GetPointRules() ([]PointRule, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	rows, err := r.db.Query(`
		SELECT id, streamer, threshold, delete_on_trigger, triggered
		FROM point_rules ORDER BY streamer, threshold
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var rules []PointRule
	for rows.Next() {
		var rule PointRule
		if err := rows.Scan(&rule.ID, &rule.Streamer, &rule.Threshold, &rule.DeleteOnTrigger, &rule.Triggered); err != nil {
			return nil, err
		}
		rules = append(rules, rule)
	}

	return rules, rows.Err()
}

func (r *Repository) AddPointRule(rule *PointRule) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	result, err := r.db.Exec(`
		INSERT INTO point_rules (streamer, threshold, delete_on_trigger, triggered)
		VALUES (?, ?, ?, 0)
	`, rule.Streamer, rule.Threshold, rule.DeleteOnTrigger)
	if err != nil {
		return err
	}

	id, err := result.LastInsertId()
	if err != nil {
		return err
	}
	rule.ID = id

	return nil
}

func (r *Repository) UpdatePointRule(rule *PointRule) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	_, err := r.db.Exec(`
		UPDATE point_rules SET
			streamer = ?,
			threshold = ?,
			delete_on_trigger = ?,
			triggered = ?
		WHERE id = ?
	`, rule.Streamer, rule.Threshold, rule.DeleteOnTrigger, rule.Triggered, rule.ID)

	return err
}

func (r *Repository) DeletePointRule(id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	_, err := r.db.Exec(`DELETE FROM point_rules WHERE id = ?`, id)
	return err
}

func (r *Repository) MarkPointRuleTriggered(id int64, triggered bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	_, err := r.db.Exec(`UPDATE point_rules SET triggered = ? WHERE id = ?`, triggered, id)
	return err
}

func (r *Repository) ResetPointRuleIfBelow(streamer string, points int) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	_, err := r.db.Exec(`
		UPDATE point_rules
		SET triggered = 0
		WHERE streamer = ? AND threshold > ? AND triggered = 1 AND delete_on_trigger = 0
	`, streamer, points)

	return err
}

// UpcomingNotifyStatus is the durable disposition of one (campaign_id,
// notification_type) pair in the upcoming-campaign dedupe table.
type UpcomingNotifyStatus string

const (
	// UpcomingStatusSuppressed: first observed while the opt-in event was off (or
	// no destination configured). Terminal — enabling the event later never
	// backfills an already-seen campaign.
	UpcomingStatusSuppressed UpcomingNotifyStatus = "suppressed"
	// UpcomingStatusPending: observed while enabled but delivery has not yet
	// succeeded. Eligible for bounded retry on the next full sync.
	UpcomingStatusPending UpcomingNotifyStatus = "pending"
	// UpcomingStatusNotified: delivered/accepted. Terminal.
	UpcomingStatusNotified UpcomingNotifyStatus = "notified"
)

// UpcomingNotifyRecord is one row of the upcoming-campaign notification dedupe
// table (Found is false when no row exists yet for the key).
type UpcomingNotifyRecord struct {
	CampaignID string
	Type       string
	Status     UpcomingNotifyStatus
	Attempts   int
	Found      bool
}

// GetUpcomingNotifyState returns the durable dedupe row for a
// (campaign_id, notification_type) pair, or Found=false when none exists.
func (r *Repository) GetUpcomingNotifyState(campaignID, notifType string) (UpcomingNotifyRecord, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	rec := UpcomingNotifyRecord{CampaignID: campaignID, Type: notifType}
	var status string
	err := r.db.QueryRow(`
		SELECT status, attempts FROM upcoming_campaign_notifications
		WHERE campaign_id = ? AND notification_type = ?`,
		campaignID, notifType,
	).Scan(&status, &rec.Attempts)
	if err == sql.ErrNoRows {
		return rec, nil
	}
	if err != nil {
		return rec, err
	}
	rec.Status = UpcomingNotifyStatus(status)
	rec.Found = true
	return rec, nil
}

// InsertUpcomingSuppressedIfAbsent records a campaign as seen-but-suppressed
// (opt-in off / no destination) only when no row exists yet, so a later
// enable never resurrects an already-seen campaign. A no-op when a row is
// already present (INSERT OR IGNORE on the PK).
func (r *Repository) InsertUpcomingSuppressedIfAbsent(campaignID, notifType string, nowMs int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	_, err := r.db.Exec(`
		INSERT OR IGNORE INTO upcoming_campaign_notifications
			(campaign_id, notification_type, status, first_seen_at, attempts)
		VALUES (?, ?, ?, ?, 0)`,
		campaignID, notifType, string(UpcomingStatusSuppressed), nowMs,
	)
	return err
}

// MarkUpcomingNotified records a successful delivery (terminal). first_seen_at
// is preserved on an existing row and set to now on a first insert.
func (r *Repository) MarkUpcomingNotified(campaignID, notifType string, nowMs int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	_, err := r.db.Exec(`
		INSERT INTO upcoming_campaign_notifications
			(campaign_id, notification_type, status, first_seen_at, notified_at, attempts)
		VALUES (?, ?, ?, ?, ?, 0)
		ON CONFLICT(campaign_id, notification_type) DO UPDATE SET
			status      = excluded.status,
			notified_at = excluded.notified_at`,
		campaignID, notifType, string(UpcomingStatusNotified), nowMs, nowMs,
	)
	return err
}

// MarkUpcomingFailed records a failed delivery attempt: the row becomes/stays
// pending, attempts is incremented, and last_error_at is stamped, so the next
// full sync can bounded-retry.
func (r *Repository) MarkUpcomingFailed(campaignID, notifType string, nowMs int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	_, err := r.db.Exec(`
		INSERT INTO upcoming_campaign_notifications
			(campaign_id, notification_type, status, first_seen_at, last_error_at, attempts)
		VALUES (?, ?, ?, ?, ?, 1)
		ON CONFLICT(campaign_id, notification_type) DO UPDATE SET
			status        = excluded.status,
			last_error_at = excluded.last_error_at,
			attempts      = attempts + 1`,
		campaignID, notifType, string(UpcomingStatusPending), nowMs, nowMs,
	)
	return err
}
