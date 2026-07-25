package analytics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/util"
)

// ErrStreamerDeleted is returned by the write paths (RecordPoints, RecordAnnotation,
// RecordChatMessage, RecordBet) for a streamer whose lifecycle is being deleted:
// a late event that lost the race with a DeleteStreamer must NOT resurrect the
// streamer's row via getOrCreateStreamer. Callers treat it as a benign drop.
var ErrStreamerDeleted = errors.New("analytics: streamer deleted")

type Repository interface {
	RecordPoints(streamer string, points int, eventType string) error
	RecordAnnotation(streamer string, eventType, text, color string) error
	GetStreamerData(streamer string) (*StreamerData, error)
	GetStreamerDataFiltered(streamer string, startTime, endTime time.Time) (*StreamerData, error)
	GetPointSamples(streamer string, startTime, endTime time.Time, limit int) ([]PointSample, error)
	GetAnnotationRecords(streamer string, startTime, endTime time.Time) ([]AnnotationRecord, error)
	PruneBefore(cutoff time.Time) (int64, error)
	ListStreamers() ([]StreamerInfo, error)
	RecordChatMessage(streamer string, msg ChatMessage) error
	GetChatMessages(streamer string, limit, offset int) (*ChatLogData, error)
	SearchChatMessages(streamer string, query string, limit, offset int) (*ChatLogData, error)
	RecordBet(b BetRecord) error
	GetBets(streamer, strategy string, startTime, endTime time.Time) ([]BetRecord, error)
	DistinctBetStrategies() ([]string, error)
	EarnedPointsBetween(start, end time.Time) (int, error)
	CountAnnotationsByType(eventType string, start, end time.Time) (int, error)
	RenameStreamer(oldName, newName string) error
	// RenameStreamerTx renames within the caller's transaction (for an atomic
	// multi-store rename); same idempotent + fail-closed-conflict contract as
	// RenameStreamer.
	RenameStreamerTx(tx *sql.Tx, oldName, newName string) error
	// DeleteStreamerTx deletes every row of one login's analytics history
	// (points, annotations, chat messages, prediction bets, and the streamers
	// row itself) within the caller's transaction, so a multi-store purge is
	// atomic. Returns true when a streamers row existed. Idempotent: an unknown
	// or already-deleted login is (false, nil). The shared hidden drops bucket is
	// never touched.
	DeleteStreamerTx(tx *sql.Tx, login string) (bool, error)
	// DeleteStreamer runs DeleteStreamerTx in its own transaction (convenience
	// for standalone callers/tests).
	DeleteStreamer(ctx context.Context, login string) (bool, error)
	// Tombstone / Reinstate arm and clear the in-memory resurrection fence for a
	// login. While tombstoned, the write paths return ErrStreamerDeleted instead
	// of recreating the streamer row.
	Tombstone(login string)
	Reinstate(login string)
	Close() error
}

// StreamerRenameConflictError means both the old and new analytics streamer
// rows already exist independently, so RenameStreamer refuses to silently
// merge two separate histories together. Privacy-safe: it carries only the
// two login names involved, never a token, URL, header, or payload.
type StreamerRenameConflictError struct {
	OldName, NewName string
}

func (e *StreamerRenameConflictError) Error() string {
	return fmt.Sprintf("analytics: cannot rename streamer %q to %q: both already have recorded history", e.OldName, e.NewName)
}

type SQLiteRepository struct {
	db       *database.DB
	basePath string

	// mu serializes the write paths (RecordPoints/Annotation/ChatMessage/Bet)
	// against the resurrection fence. A write holds mu across its check+insert;
	// Tombstone takes mu too, so once Tombstone returns every in-flight write has
	// finished (its row now exists and is deleted by the purge) and every later
	// write observes the tombstone. deleted holds the tombstoned lowercase
	// logins.
	mu      sync.Mutex
	deleted map[string]struct{}
}

type AnalyticsModule struct{}

func (m *AnalyticsModule) Name() string {
	return "analytics"
}

func (m *AnalyticsModule) Migrations() []database.Migration {
	return []database.Migration{
		{
			Version:     1,
			Description: "Create streamers, points, and annotations tables",
			SQL: `
				CREATE TABLE IF NOT EXISTS streamers (
					id INTEGER PRIMARY KEY AUTOINCREMENT,
					name TEXT UNIQUE NOT NULL,
					created_at INTEGER NOT NULL
				);

				CREATE TABLE IF NOT EXISTS points (
					id INTEGER PRIMARY KEY AUTOINCREMENT,
					streamer_id INTEGER NOT NULL,
					timestamp INTEGER NOT NULL,
					points INTEGER NOT NULL,
					event_type TEXT,
					FOREIGN KEY (streamer_id) REFERENCES streamers(id)
				);

				CREATE TABLE IF NOT EXISTS annotations (
					id INTEGER PRIMARY KEY AUTOINCREMENT,
					streamer_id INTEGER NOT NULL,
					timestamp INTEGER NOT NULL,
					text TEXT NOT NULL,
					color TEXT NOT NULL,
					FOREIGN KEY (streamer_id) REFERENCES streamers(id)
				);

				CREATE INDEX IF NOT EXISTS idx_points_streamer_time ON points(streamer_id, timestamp);
				CREATE INDEX IF NOT EXISTS idx_annotations_streamer_time ON annotations(streamer_id, timestamp);
			`,
		},
		{
			Version:     2,
			Description: "Create chat_messages table",
			SQL: `
				CREATE TABLE IF NOT EXISTS chat_messages (
					id INTEGER PRIMARY KEY AUTOINCREMENT,
					streamer_id INTEGER NOT NULL,
					timestamp INTEGER NOT NULL,
					username TEXT NOT NULL,
					display_name TEXT NOT NULL,
					message TEXT NOT NULL,
					emotes TEXT,
					badges TEXT,
					color TEXT,
					FOREIGN KEY (streamer_id) REFERENCES streamers(id)
				);

				CREATE INDEX IF NOT EXISTS idx_chat_streamer_time ON chat_messages(streamer_id, timestamp);
			`,
		},
		{
			Version:     3,
			Description: "Add machine-readable event_type to annotations",
			// Run (not SQL): ALTER TABLE ADD COLUMN is not idempotent in
			// SQLite, and DBs that crashed between this migration and its
			// version bump (pre-transactional-migrations builds) already have
			// the column with a stale version. The per-column guard lets such
			// a DB self-heal instead of failing with "duplicate column name"
			// on every startup.
			Run: func(tx *sql.Tx) error {
				return database.AddColumnIfMissing(tx, "annotations", "event_type", "TEXT")
			},
		},
		{
			Version:     4,
			Description: "Create prediction_bets table for ROI analytics",
			// Additive only: a new table, no ALTER of points/annotations/chat_messages,
			// so existing statistics history is untouched and this migration is safe on
			// a populated database. UNIQUE(event_id) makes RecordBet idempotent against
			// a re-delivered prediction-result (PubSub reconnect). No FOREIGN KEY clause:
			// this codebase never enables PRAGMA foreign_keys, so an FK would be
			// decorative and misleading — integrity of streamer_id is instead guaranteed
			// by RecordBet always resolving the parent row via getOrCreateStreamer first,
			// exactly as every other table here already relies on. This table is
			// deliberately excluded from the retention sweep (PruneBefore) so lifetime
			// ROI stays exact; it grows by one row per resolved prediction.
			SQL: `
				CREATE TABLE IF NOT EXISTS prediction_bets (
					id           INTEGER PRIMARY KEY AUTOINCREMENT,
					streamer_id  INTEGER NOT NULL,
					event_id     TEXT NOT NULL UNIQUE,
					timestamp    INTEGER NOT NULL,
					strategy     TEXT NOT NULL,
					result_type  TEXT NOT NULL,
					placed       INTEGER NOT NULL,
					won          INTEGER NOT NULL,
					gained       INTEGER NOT NULL,
					odds         REAL NOT NULL,
					manual       INTEGER NOT NULL DEFAULT 0
				);

				CREATE INDEX IF NOT EXISTS idx_predbets_streamer_time ON prediction_bets(streamer_id, timestamp);
			`,
		},
	}
}

func NewSQLiteRepository(db *database.DB, basePath string) (*SQLiteRepository, error) {
	module := &AnalyticsModule{}
	if err := db.RegisterModule(module); err != nil {
		return nil, fmt.Errorf("failed to register analytics module: %w", err)
	}

	repo := &SQLiteRepository{
		db:       db,
		basePath: basePath,
		deleted:  make(map[string]struct{}),
	}

	return repo, nil
}

// Tombstone arms the resurrection fence for login: subsequent write paths
// (RecordPoints/Annotation/ChatMessage/Bet) return ErrStreamerDeleted instead
// of recreating the streamers row. Because it takes mu — the same lock every
// write holds across its check+insert — once Tombstone returns, every in-flight
// write has finished (its row now exists, to be removed by the purge that
// follows) and every later write observes the tombstone: an airtight barrier
// with no window for a late event to slip a row past the delete. Idempotent.
func (r *SQLiteRepository) Tombstone(login string) {
	login = strings.ToLower(login)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleted[login] = struct{}{}
}

// Reinstate clears the fence for login so a re-added streamer of the same login
// can record fresh history again. Idempotent.
func (r *SQLiteRepository) Reinstate(login string) {
	login = strings.ToLower(login)
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.deleted, login)
}

// tombstonedLocked reports whether login is fenced. Caller holds mu.
func (r *SQLiteRepository) tombstonedLocked(login string) bool {
	_, ok := r.deleted[strings.ToLower(login)]
	return ok
}

func (r *SQLiteRepository) getOrCreateStreamer(name string) (int64, error) {
	tx, err := r.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	id, err := r.getOrCreateStreamerTx(tx, name)
	if err != nil {
		return 0, err
	}

	return id, tx.Commit()
}

func (r *SQLiteRepository) getOrCreateStreamerTx(tx *sql.Tx, name string) (int64, error) {
	var id int64
	err := tx.QueryRow("SELECT id FROM streamers WHERE name = ?", name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, err
	}

	result, err := tx.Exec("INSERT INTO streamers (name, created_at) VALUES (?, ?)", name, time.Now().UnixMilli())
	if err != nil {
		return 0, err
	}

	return result.LastInsertId()
}

// RenameStreamer migrates the streamers row from oldName to newName within
// ONE transaction, preserving its internal autoincrement id so every table
// keyed by streamer_id (points, annotations, chat_messages, prediction_bets)
// stays attached to the SAME history after the rename — no schema migration,
// no new column, no data duplication (BKM-006 I8). It is idempotent: if
// oldName has no recorded row this is a no-op (nil error), so a repeated
// settings apply never errors. It fails CLOSED with a typed
// *StreamerRenameConflictError — no merge, no mutation — when BOTH oldName
// and newName already have their own independent row: two histories are
// never silently combined, the caller decides.
func (r *SQLiteRepository) RenameStreamer(oldName, newName string) error {
	if oldName == newName {
		return nil
	}
	return r.db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return r.RenameStreamerTx(tx, oldName, newName)
	})
}

// RenameStreamerTx is the transaction body of RenameStreamer, exposed so a
// multi-store rename can run analytics + notifications + watch-time renames in
// ONE atomic transaction (all move together or none do). It preserves the same
// idempotent no-op (unknown old login) and fail-closed *StreamerRenameConflictError
// (both logins already have independent history) contract as RenameStreamer, on
// the caller's tx. Names are matched exactly; callers pass canonical (lowercase)
// logins, as every write path here already stores them.
func (r *SQLiteRepository) RenameStreamerTx(tx *sql.Tx, oldName, newName string) error {
	if oldName == newName {
		return nil
	}
	var oldID int64
	err := tx.QueryRow("SELECT id FROM streamers WHERE name = ?", oldName).Scan(&oldID)
	if err == sql.ErrNoRows {
		return nil // nothing recorded under the old login: idempotent no-op
	}
	if err != nil {
		return err
	}
	var newID int64
	err = tx.QueryRow("SELECT id FROM streamers WHERE name = ?", newName).Scan(&newID)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil {
		// Both rows exist independently: fail closed rather than guess which
		// history should win or silently combine them.
		return &StreamerRenameConflictError{OldName: oldName, NewName: newName}
	}
	_, err = tx.Exec("UPDATE streamers SET name = ? WHERE id = ?", newName, oldID)
	return err
}

// DeleteStreamerTx removes every trace of one login's analytics history within
// the caller's transaction: the child rows (points, annotations, chat_messages,
// prediction_bets) keyed by the resolved streamers.id, then the streamers row
// itself. It runs on the passed *sql.Tx so a full multi-store streamer purge is
// one atomic transaction (foreign keys are not enforced in this codebase, so the
// children are deleted explicitly rather than via ON DELETE CASCADE). Returns
// true when a streamers row existed. Idempotent: an unknown login is (false,
// nil). The shared hidden drops bucket ("(drops)") is never deleted — it is
// global claim-accounting, not a streamer. The caller is expected to have
// Tombstone()d the login first so no concurrent write recreates the row.
func (r *SQLiteRepository) DeleteStreamerTx(tx *sql.Tx, login string) (bool, error) {
	login = strings.ToLower(login)
	if login == "" || login == DropsBucket {
		return false, nil
	}

	var id int64
	err := tx.QueryRow("SELECT id FROM streamers WHERE name = ?", login).Scan(&id)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	for _, table := range []string{"points", "annotations", "chat_messages", "prediction_bets"} {
		if _, err := tx.Exec("DELETE FROM "+table+" WHERE streamer_id = ?", id); err != nil {
			return false, fmt.Errorf("delete %s for streamer_id %d: %w", table, id, err)
		}
	}
	if _, err := tx.Exec("DELETE FROM streamers WHERE id = ?", id); err != nil {
		return false, fmt.Errorf("delete streamers row %d: %w", id, err)
	}
	return true, nil
}

// DeleteStreamer runs DeleteStreamerTx in its own transaction on the shared
// handle. Convenience for standalone callers/tests; the miner's lifecycle
// coordinator uses DeleteStreamerTx to purge several stores in one transaction.
func (r *SQLiteRepository) DeleteStreamer(ctx context.Context, login string) (bool, error) {
	var existed bool
	err := r.db.WithTx(ctx, func(tx *sql.Tx) error {
		var e error
		existed, e = r.DeleteStreamerTx(tx, login)
		return e
	})
	return existed, err
}

func (r *SQLiteRepository) RecordPoints(streamer string, points int, eventType string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tombstonedLocked(streamer) {
		return ErrStreamerDeleted
	}

	streamerID, err := r.getOrCreateStreamer(streamer)
	if err != nil {
		return err
	}

	_, err = r.db.Exec(
		"INSERT INTO points (streamer_id, timestamp, points, event_type) VALUES (?, ?, ?, ?)",
		streamerID, time.Now().UnixMilli(), points, eventType,
	)
	return err
}

func (r *SQLiteRepository) RecordAnnotation(streamer string, eventType, text, color string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tombstonedLocked(streamer) {
		return ErrStreamerDeleted
	}

	streamerID, err := r.getOrCreateStreamer(streamer)
	if err != nil {
		return err
	}

	_, err = r.db.Exec(
		"INSERT INTO annotations (streamer_id, timestamp, text, color, event_type) VALUES (?, ?, ?, ?, ?)",
		streamerID, time.Now().UnixMilli(), text, color, eventType,
	)
	return err
}

func (r *SQLiteRepository) GetStreamerData(streamer string) (*StreamerData, error) {
	return r.GetStreamerDataFiltered(streamer, time.Time{}, time.Time{})
}

func (r *SQLiteRepository) GetStreamerDataFiltered(streamer string, startTime, endTime time.Time) (*StreamerData, error) {
	var streamerID int64
	err := r.db.QueryRow("SELECT id FROM streamers WHERE name = ?", streamer).Scan(&streamerID)
	if err == sql.ErrNoRows {
		return &StreamerData{}, nil
	}
	if err != nil {
		return nil, err
	}

	data := &StreamerData{}

	pointsQuery := "SELECT timestamp, points, COALESCE(event_type, '') FROM points WHERE streamer_id = ?"
	var args []interface{}
	args = append(args, streamerID)

	if !startTime.IsZero() {
		pointsQuery += " AND timestamp >= ?"
		args = append(args, startTime.UnixMilli())
	}
	if !endTime.IsZero() {
		pointsQuery += " AND timestamp <= ?"
		args = append(args, endTime.UnixMilli())
	}
	pointsQuery += " ORDER BY timestamp ASC"

	rows, err := r.db.Query(pointsQuery, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var p SeriesPoint
		if err := rows.Scan(&p.X, &p.Y, &p.Z); err != nil {
			return nil, err
		}
		data.Series = append(data.Series, p)
	}

	annotationsQuery := "SELECT timestamp, text, color FROM annotations WHERE streamer_id = ?"
	args = []interface{}{streamerID}

	if !startTime.IsZero() {
		annotationsQuery += " AND timestamp >= ?"
		args = append(args, startTime.UnixMilli())
	}
	if !endTime.IsZero() {
		annotationsQuery += " AND timestamp <= ?"
		args = append(args, endTime.UnixMilli())
	}
	annotationsQuery += " ORDER BY timestamp ASC"

	rows, err = r.db.Query(annotationsQuery, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var a Annotation
		var text, color string
		if err := rows.Scan(&a.X, &text, &color); err != nil {
			return nil, err
		}
		a.BorderColor = color
		a.Label = AnnotationLabel{
			Style: map[string]string{"color": "#000", "background": color},
			Text:  text,
		}
		data.Annotations = append(data.Annotations, a)
	}

	return data, nil
}

// GetPointSamples returns the balance-over-time readings for a streamer within
// [startTime, endTime] (zero bounds are open-ended), ordered oldest-first. When
// limit > 0 it caps the number of rows fetched (a memory/timeout guard); the
// caller downsamples the result for display. An unknown streamer yields nil.
func (r *SQLiteRepository) GetPointSamples(streamer string, startTime, endTime time.Time, limit int) ([]PointSample, error) {
	var streamerID int64
	err := r.db.QueryRow("SELECT id FROM streamers WHERE name = ?", streamer).Scan(&streamerID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	query := "SELECT timestamp, points, COALESCE(event_type, '') FROM points WHERE streamer_id = ?"
	args := []interface{}{streamerID}
	if !startTime.IsZero() {
		query += " AND timestamp >= ?"
		args = append(args, startTime.UnixMilli())
	}
	if !endTime.IsZero() {
		query += " AND timestamp <= ?"
		args = append(args, endTime.UnixMilli())
	}
	query += " ORDER BY timestamp ASC"
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var samples []PointSample
	for rows.Next() {
		var s PointSample
		if err := rows.Scan(&s.T, &s.Balance, &s.Reason); err != nil {
			return nil, err
		}
		samples = append(samples, s)
	}
	return samples, rows.Err()
}

// GetAnnotationRecords returns the event markers for a streamer within
// [startTime, endTime] (zero bounds are open-ended), ordered oldest-first. The
// event type falls back to the label text for rows written before the
// event_type column existed; the per-type colour is carried through so the
// chart can render each marker distinctly. An unknown streamer yields nil.
func (r *SQLiteRepository) GetAnnotationRecords(streamer string, startTime, endTime time.Time) ([]AnnotationRecord, error) {
	var streamerID int64
	err := r.db.QueryRow("SELECT id FROM streamers WHERE name = ?", streamer).Scan(&streamerID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	query := "SELECT timestamp, COALESCE(event_type, ''), text, color FROM annotations WHERE streamer_id = ?"
	args := []interface{}{streamerID}
	if !startTime.IsZero() {
		query += " AND timestamp >= ?"
		args = append(args, startTime.UnixMilli())
	}
	if !endTime.IsZero() {
		query += " AND timestamp <= ?"
		args = append(args, endTime.UnixMilli())
	}
	query += " ORDER BY timestamp ASC"

	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var records []AnnotationRecord
	for rows.Next() {
		var rec AnnotationRecord
		if err := rows.Scan(&rec.T, &rec.Type, &rec.Reason, &rec.Color); err != nil {
			return nil, err
		}
		if rec.Type == "" {
			rec.Type = rec.Reason
		}
		records = append(records, rec)
	}
	return records, rows.Err()
}

// PruneBefore deletes points and annotation rows older than cutoff, returning
// the total number of rows removed. Used by the retention sweep; the single-
// connection DB serializes it against concurrent writes.
func (r *SQLiteRepository) PruneBefore(cutoff time.Time) (int64, error) {
	c := cutoff.UnixMilli()
	var total int64
	for _, table := range []string{"points", "annotations"} {
		res, err := r.db.Exec("DELETE FROM "+table+" WHERE timestamp < ?", c)
		if err != nil {
			return total, err
		}
		if n, err := res.RowsAffected(); err == nil {
			total += n
		}
	}
	return total, nil
}

// Downsample uniformly reduces samples to at most max points, always keeping
// the first and last reading so the chart's endpoints stay accurate. It returns
// the input unchanged when max <= 0 or the series is already within budget.
func Downsample(samples []PointSample, max int) []PointSample {
	if max <= 0 || len(samples) <= max {
		return samples
	}
	if max == 1 {
		return samples[len(samples)-1:]
	}
	out := make([]PointSample, 0, max)
	step := float64(len(samples)-1) / float64(max-1)
	for i := 0; i < max-1; i++ {
		out = append(out, samples[int(float64(i)*step)])
	}
	return append(out, samples[len(samples)-1])
}

func (r *SQLiteRepository) ListStreamers() ([]StreamerInfo, error) {
	query := `
		SELECT s.name,
			COALESCE((SELECT points FROM points WHERE streamer_id = s.id ORDER BY timestamp DESC LIMIT 1), 0) as points,
			COALESCE((SELECT timestamp FROM points WHERE streamer_id = s.id ORDER BY timestamp DESC LIMIT 1), 0) as last_activity
		FROM streamers s
		WHERE s.name != ?
		ORDER BY points DESC
	`

	rows, err := r.db.Query(query, DropsBucket)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var streamers []StreamerInfo
	for rows.Next() {
		var info StreamerInfo
		if err := rows.Scan(&info.Name, &info.Points, &info.LastActivity); err != nil {
			return nil, err
		}
		info.PointsFormatted = util.FormatNumber(info.Points)
		info.LastActivityFormatted = util.FormatTimeAgo(info.LastActivity)
		streamers = append(streamers, info)
	}

	return streamers, nil
}

func (r *SQLiteRepository) RecordChatMessage(streamer string, msg ChatMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tombstonedLocked(streamer) {
		return ErrStreamerDeleted
	}

	streamerID, err := r.getOrCreateStreamer(streamer)
	if err != nil {
		return err
	}

	_, err = r.db.Exec(
		`INSERT INTO chat_messages (streamer_id, timestamp, username, display_name, message, emotes, badges, color)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		streamerID, time.Now().UnixMilli(), msg.Username, msg.DisplayName, msg.Message, msg.Emotes, msg.Badges, msg.Color,
	)
	return err
}

func (r *SQLiteRepository) GetChatMessages(streamer string, limit, offset int) (*ChatLogData, error) {
	var streamerID int64
	err := r.db.QueryRow("SELECT id FROM streamers WHERE name = ?", streamer).Scan(&streamerID)
	if err == sql.ErrNoRows {
		return &ChatLogData{Messages: []ChatMessage{}}, nil
	}
	if err != nil {
		return nil, err
	}

	var totalCount int
	err = r.db.QueryRow("SELECT COUNT(*) FROM chat_messages WHERE streamer_id = ?", streamerID).Scan(&totalCount)
	if err != nil {
		return nil, err
	}

	if limit <= 0 {
		limit = 100
	}

	rows, err := r.db.Query(
		`SELECT id, timestamp, username, display_name, message, COALESCE(emotes, ''), COALESCE(badges, ''), COALESCE(color, '')
		 FROM chat_messages 
		 WHERE streamer_id = ? 
		 ORDER BY timestamp DESC 
		 LIMIT ? OFFSET ?`,
		streamerID, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var messages []ChatMessage
	for rows.Next() {
		var msg ChatMessage
		if err := rows.Scan(&msg.ID, &msg.Timestamp, &msg.Username, &msg.DisplayName, &msg.Message, &msg.Emotes, &msg.Badges, &msg.Color); err != nil {
			return nil, err
		}
		messages = append(messages, msg)
	}

	if messages == nil {
		messages = []ChatMessage{}
	}

	return &ChatLogData{
		Messages:   messages,
		TotalCount: totalCount,
		HasMore:    offset+len(messages) < totalCount,
	}, nil
}

func (r *SQLiteRepository) SearchChatMessages(streamer string, query string, limit, offset int) (*ChatLogData, error) {
	var streamerID int64
	err := r.db.QueryRow("SELECT id FROM streamers WHERE name = ?", streamer).Scan(&streamerID)
	if err == sql.ErrNoRows {
		return &ChatLogData{Messages: []ChatMessage{}}, nil
	}
	if err != nil {
		return nil, err
	}

	searchPattern := "%" + query + "%"

	var totalCount int
	err = r.db.QueryRow(
		"SELECT COUNT(*) FROM chat_messages WHERE streamer_id = ? AND (message LIKE ? OR username LIKE ? OR display_name LIKE ?)",
		streamerID, searchPattern, searchPattern, searchPattern,
	).Scan(&totalCount)
	if err != nil {
		return nil, err
	}

	if limit <= 0 {
		limit = 100
	}

	rows, err := r.db.Query(
		`SELECT id, timestamp, username, display_name, message, COALESCE(emotes, ''), COALESCE(badges, ''), COALESCE(color, '')
		 FROM chat_messages 
		 WHERE streamer_id = ? AND (message LIKE ? OR username LIKE ? OR display_name LIKE ?)
		 ORDER BY timestamp DESC 
		 LIMIT ? OFFSET ?`,
		streamerID, searchPattern, searchPattern, searchPattern, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var messages []ChatMessage
	for rows.Next() {
		var msg ChatMessage
		if err := rows.Scan(&msg.ID, &msg.Timestamp, &msg.Username, &msg.DisplayName, &msg.Message, &msg.Emotes, &msg.Badges, &msg.Color); err != nil {
			return nil, err
		}
		messages = append(messages, msg)
	}

	if messages == nil {
		messages = []ChatMessage{}
	}

	return &ChatLogData{
		Messages:   messages,
		TotalCount: totalCount,
		HasMore:    offset+len(messages) < totalCount,
	}, nil
}

// RecordBet persists one resolved prediction bet. It is idempotent: UNIQUE(event_id)
// plus INSERT OR IGNORE means a re-delivered prediction-result (PubSub reconnect,
// duplicate push) neither errors nor double-counts — the second write is a no-op
// that is logged, not silently swallowed. streamer_id integrity is guaranteed by
// resolving/creating the parent streamer row first.
func (r *SQLiteRepository) RecordBet(b BetRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tombstonedLocked(b.Streamer) {
		return ErrStreamerDeleted
	}

	streamerID, err := r.getOrCreateStreamer(b.Streamer)
	if err != nil {
		return err
	}

	manual := 0
	if b.Manual {
		manual = 1
	}

	res, err := r.db.Exec(
		`INSERT OR IGNORE INTO prediction_bets
		   (streamer_id, event_id, timestamp, strategy, result_type, placed, won, gained, odds, manual)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		streamerID, b.EventID, b.Timestamp, b.Strategy, b.ResultType,
		b.Placed, b.Won, b.Gained, b.Odds, manual,
	)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// UNIQUE(event_id) rejected the row: this exact prediction result was
		// already recorded. Expected on a PubSub reconnect; log so it is visible
		// but never treat it as an error or a second bet.
		slog.Info("Duplicate prediction result ignored", "event", b.EventID, "streamer", b.Streamer)
	}
	return nil
}

// GetBets returns resolved bets for the given filters ordered oldest-first (the
// order the ROI aggregator needs for its drawdown curve). An empty streamer or
// strategy means "no filter on that field"; zero start/end are open-ended (used
// for the lifetime period). An unknown streamer name yields nil, not an error —
// mirroring GetPointSamples.
func (r *SQLiteRepository) GetBets(streamer, strategy string, startTime, endTime time.Time) ([]BetRecord, error) {
	query := `SELECT s.name, b.event_id, b.timestamp, b.strategy, b.result_type,
	                 b.placed, b.won, b.gained, b.odds, b.manual
	          FROM prediction_bets b
	          JOIN streamers s ON s.id = b.streamer_id
	          WHERE 1=1`
	var args []interface{}

	if streamer != "" {
		var streamerID int64
		err := r.db.QueryRow("SELECT id FROM streamers WHERE name = ?", streamer).Scan(&streamerID)
		if err == sql.ErrNoRows {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		query += " AND b.streamer_id = ?"
		args = append(args, streamerID)
	}
	if strategy != "" {
		query += " AND b.strategy = ?"
		args = append(args, strategy)
	}
	if !startTime.IsZero() {
		query += " AND b.timestamp >= ?"
		args = append(args, startTime.UnixMilli())
	}
	if !endTime.IsZero() {
		query += " AND b.timestamp <= ?"
		args = append(args, endTime.UnixMilli())
	}
	query += " ORDER BY b.timestamp ASC"

	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var bets []BetRecord
	for rows.Next() {
		var b BetRecord
		var manual int
		if err := rows.Scan(&b.Streamer, &b.EventID, &b.Timestamp, &b.Strategy, &b.ResultType,
			&b.Placed, &b.Won, &b.Gained, &b.Odds, &manual); err != nil {
			return nil, err
		}
		b.Manual = manual != 0
		bets = append(bets, b)
	}
	return bets, rows.Err()
}

// DropsBucket is a synthetic streamer name under which drop-claim annotations
// are recorded (drop claims are not tied to a single watched channel). It is
// hidden from ListStreamers so it never shows up in the Statistics selector; it
// exists only so the daily summary can count DROP_CLAIMED annotations durably.
// The parenthesis makes it an impossible real Twitch login.
const DropsBucket = "(drops)"

// EarnedPointsBetween returns the net channel-point change across all streamers
// within [start, end]: the sum over streamers of (last balance − first balance)
// in the window. Because the points table stores absolute balance snapshots,
// this is the honest "net points change" (it includes claims, watch gains, and
// betting outcomes alike). A window with no samples yields 0.
func (r *SQLiteRepository) EarnedPointsBetween(start, end time.Time) (int, error) {
	rows, err := r.db.Query(
		`SELECT streamer_id, points FROM points
		 WHERE timestamp >= ? AND timestamp <= ?
		 ORDER BY streamer_id, timestamp ASC`,
		start.UnixMilli(), end.UnixMilli(),
	)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()

	type span struct {
		first, last int
		seen        bool
	}
	perStreamer := map[int64]*span{}
	for rows.Next() {
		var sid int64
		var pts int
		if err := rows.Scan(&sid, &pts); err != nil {
			return 0, err
		}
		s := perStreamer[sid]
		if s == nil {
			s = &span{}
			perStreamer[sid] = s
		}
		if !s.seen {
			s.first = pts
			s.seen = true
		}
		s.last = pts // rows are timestamp-ascending, so this ends on the latest
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	total := 0
	for _, s := range perStreamer {
		total += s.last - s.first
	}
	return total, nil
}

// CountAnnotationsByType counts annotations of the given event type across all
// streamers within [start, end]. Used by the daily summary for durable counts
// of typed events (WATCH_STREAK streaks, DROP_CLAIMED drop claims).
func (r *SQLiteRepository) CountAnnotationsByType(eventType string, start, end time.Time) (int, error) {
	var n int
	err := r.db.QueryRow(
		`SELECT COUNT(*) FROM annotations
		 WHERE event_type = ? AND timestamp >= ? AND timestamp <= ?`,
		eventType, start.UnixMilli(), end.UnixMilli(),
	).Scan(&n)
	return n, err
}

// DistinctBetStrategies returns the strategies that actually appear in recorded
// bets, sorted, so the ROI filter only offers strategies that have data.
func (r *SQLiteRepository) DistinctBetStrategies() ([]string, error) {
	rows, err := r.db.Query("SELECT DISTINCT strategy FROM prediction_bets ORDER BY strategy ASC")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *SQLiteRepository) Close() error {
	return nil
}
