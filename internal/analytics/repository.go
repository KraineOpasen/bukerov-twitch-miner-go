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
		"INSERT INTO annotations (streamer_id, timestamp, text, color) VALUES (?, ?, ?, ?)",
		streamerID, time.Now().UnixMilli(), text, color,
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
	defer rows.Close()

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
	defer rows.Close()

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
	defer rows.Close()

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
	defer rows.Close()

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
	defer rows.Close()

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
