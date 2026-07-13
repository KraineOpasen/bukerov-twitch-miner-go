package analytics

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/util"
)

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
	Close() error
}

type SQLiteRepository struct {
	db       *database.DB
	basePath string
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
			SQL: `
				ALTER TABLE annotations ADD COLUMN event_type TEXT;
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
	}

	return repo, nil
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

func (r *SQLiteRepository) RecordPoints(streamer string, points int, eventType string) error {
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
// event_type column existed. An unknown streamer yields nil.
func (r *SQLiteRepository) GetAnnotationRecords(streamer string, startTime, endTime time.Time) ([]AnnotationRecord, error) {
	var streamerID int64
	err := r.db.QueryRow("SELECT id FROM streamers WHERE name = ?", streamer).Scan(&streamerID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	query := "SELECT timestamp, COALESCE(event_type, ''), text FROM annotations WHERE streamer_id = ?"
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
		if err := rows.Scan(&rec.T, &rec.Type, &rec.Reason); err != nil {
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
		ORDER BY points DESC
	`

	rows, err := r.db.Query(query)
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

func (r *SQLiteRepository) Close() error {
	return nil
}
