package notifications

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
)

// ErrStreamerDeleted is returned by AddPointRule for a login whose lifecycle is
// being deleted, so a rule creation that raced a streamer deletion cannot
// recreate a notification record for the removed streamer.
var ErrStreamerDeleted = errors.New("notifications: streamer deleted")

type Repository struct {
	db *database.DB
	mu sync.RWMutex
	// deleted is the resurrection fence: tombstoned lowercase logins whose
	// AddPointRule is refused while a deletion is in flight. Guarded by mu (the
	// same lock AddPointRule takes), so once Tombstone returns every in-flight
	// rule insert has completed and every later one observes the tombstone.
	deleted map[string]struct{}
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

	return &Repository{db: db, deleted: make(map[string]struct{})}, nil
}

func (r *Repository) Close() error {
	return nil
}

// Tombstone arms the resurrection fence for login: AddPointRule returns
// ErrStreamerDeleted while the tombstone is set. Idempotent; see the fence
// contract on the analytics repository for why the shared mu makes it airtight.
func (r *Repository) Tombstone(login string) {
	login = strings.ToLower(login)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleted[login] = struct{}{}
}

// Reinstate clears the fence for login so a re-added streamer can hold rules
// again. Idempotent.
func (r *Repository) Reinstate(login string) {
	login = strings.ToLower(login)
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.deleted, login)
}

func (r *Repository) tombstonedLocked(login string) bool {
	_, ok := r.deleted[strings.ToLower(login)]
	return ok
}

// DeleteStreamerTx scrubs one login's notification state within the caller's
// transaction: it deletes every point_rules row for the login (case-insensitive,
// since rules may have been entered in any case) and strips the login from the
// three notification_config login lists (mentions/online/offline). It runs on
// the passed *sql.Tx so it joins the atomic multi-store streamer purge. Returns
// true when anything was actually removed. Idempotent. upcoming_campaign_
// notifications is keyed by campaign, not streamer, and is deliberately left
// alone. The caller is expected to have Tombstone()d the login first.
func (r *Repository) DeleteStreamerTx(tx *sql.Tx, login string) (bool, error) {
	login = strings.ToLower(login)
	if login == "" {
		return false, nil
	}

	changed := false

	res, err := tx.Exec("DELETE FROM point_rules WHERE streamer = ? COLLATE NOCASE", login)
	if err != nil {
		return false, fmt.Errorf("delete point_rules for %q: %w", login, err)
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		changed = true
	}

	// Strip the login from the three config login-lists (read-modify-write of the
	// single config row, inside the same tx).
	var mentionsJSON, onlineJSON, offlineJSON string
	err = tx.QueryRow(`SELECT mentions_streamers, online_streamers, offline_streamers
		FROM notification_config WHERE id = 1`).Scan(&mentionsJSON, &onlineJSON, &offlineJSON)
	if err == sql.ErrNoRows {
		return changed, nil
	}
	if err != nil {
		return false, fmt.Errorf("read notification_config lists: %w", err)
	}

	newMentions, m1 := removeLoginFromJSONList(mentionsJSON, login)
	newOnline, m2 := removeLoginFromJSONList(onlineJSON, login)
	newOffline, m3 := removeLoginFromJSONList(offlineJSON, login)
	if m1 || m2 || m3 {
		if _, err := tx.Exec(`UPDATE notification_config
			SET mentions_streamers = ?, online_streamers = ?, offline_streamers = ?
			WHERE id = 1`, newMentions, newOnline, newOffline); err != nil {
			return false, fmt.Errorf("rewrite notification_config lists: %w", err)
		}
		changed = true
	}

	return changed, nil
}

// DeleteStreamer runs DeleteStreamerTx in its own transaction. Convenience for
// standalone callers/tests.
func (r *Repository) DeleteStreamer(ctx context.Context, login string) (bool, error) {
	var existed bool
	err := r.db.WithTx(ctx, func(tx *sql.Tx) error {
		var e error
		existed, e = r.DeleteStreamerTx(tx, login)
		return e
	})
	return existed, err
}

// RenameStreamer repoints one login's notification state (point rules + the
// three config login-lists) from oldLogin to newLogin within one transaction,
// so a config-driven rename keeps notification state attached to the streamer's
// CURRENT login — mirroring analytics.RenameStreamer and keeping the store
// deletable by the current login (BKM-018A D15/D16). Best-effort by design:
// there is no unique constraint to collide on, so a rename onto an existing
// login simply merges (de-duplicated). No-op when the logins match or either is
// empty. Case-insensitive.
func (r *Repository) RenameStreamer(oldLogin, newLogin string) error {
	oldLogin = strings.ToLower(oldLogin)
	newLogin = strings.ToLower(newLogin)
	if oldLogin == "" || newLogin == "" || oldLogin == newLogin {
		return nil
	}
	return r.db.WithTx(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec("UPDATE point_rules SET streamer = ? WHERE streamer = ? COLLATE NOCASE", newLogin, oldLogin); err != nil {
			return fmt.Errorf("rename point_rules %q->%q: %w", oldLogin, newLogin, err)
		}
		var mentionsJSON, onlineJSON, offlineJSON string
		err := tx.QueryRow(`SELECT mentions_streamers, online_streamers, offline_streamers
			FROM notification_config WHERE id = 1`).Scan(&mentionsJSON, &onlineJSON, &offlineJSON)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read notification_config lists: %w", err)
		}
		nm, c1 := renameLoginInJSONList(mentionsJSON, oldLogin, newLogin)
		no, c2 := renameLoginInJSONList(onlineJSON, oldLogin, newLogin)
		nf, c3 := renameLoginInJSONList(offlineJSON, oldLogin, newLogin)
		if c1 || c2 || c3 {
			if _, err := tx.Exec(`UPDATE notification_config
				SET mentions_streamers = ?, online_streamers = ?, offline_streamers = ?
				WHERE id = 1`, nm, no, nf); err != nil {
				return fmt.Errorf("rewrite notification_config lists: %w", err)
			}
		}
		return nil
	})
}

// renameLoginInJSONList replaces every case-insensitive occurrence of oldLogin
// with newLogin in a JSON string array, de-duplicating (case-insensitively) so a
// rename onto a login already present does not create a duplicate entry. Returns
// the re-encoded array and whether anything changed.
func renameLoginInJSONList(raw, oldLogin, newLogin string) (string, bool) {
	var list []string
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &list)
	}
	changed := false
	seen := make(map[string]bool, len(list))
	out := make([]string, 0, len(list))
	for _, s := range list {
		if strings.EqualFold(s, oldLogin) {
			s = newLogin
			changed = true
		}
		key := strings.ToLower(s)
		if seen[key] {
			changed = true // collapsed a duplicate created by the rename
			continue
		}
		seen[key] = true
		out = append(out, s)
	}
	if !changed {
		return raw, false
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return raw, false
	}
	return string(encoded), true
}

// removeLoginFromJSONList parses a JSON string array, drops every element that
// equals login case-insensitively, and returns the re-encoded array plus whether
// anything was removed. A malformed/empty array yields an empty array unchanged.
func removeLoginFromJSONList(raw, login string) (string, bool) {
	var list []string
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &list)
	}
	out := make([]string, 0, len(list))
	removed := false
	for _, s := range list {
		if strings.EqualFold(s, login) {
			removed = true
			continue
		}
		out = append(out, s)
	}
	if !removed {
		return raw, false
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return raw, false
	}
	return string(encoded), true
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
	if r.tombstonedLocked(rule.Streamer) {
		return ErrStreamerDeleted
	}

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
