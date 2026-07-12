package watcher

import (
	"fmt"
	"strings"
	"time"

	"github.com/PatrickWalther/twitch-miner-go/internal/database"
)

// watchTimeWindow is the trailing period over which accumulated watch
// minutes are considered when ranking streamers for rotation fairness.
//
// 8 hours was chosen as a middle ground within the requested 6-12h range:
// it comfortably covers a single mining/streaming session, so a brief
// offline blip or game switch doesn't erase a channel's "already watched a
// lot recently" standing (it keeps yielding its slot to less-watched
// channels through short interruptions), while a session from more than 8
// hours ago no longer counts against a channel indefinitely, so yesterday's
// long watch doesn't permanently deprioritize it today.
const watchTimeWindow = 8 * time.Hour

// WatchTimeStore persists accumulated per-streamer watch minutes in the
// shared SQLite database (under the /database volume), so the fair-rotation
// ranking survives container restarts instead of resetting to all-zero.
type WatchTimeStore struct {
	db *database.DB
}

type watchTimeModule struct{}

func (watchTimeModule) Name() string { return "watch_time" }

func (watchTimeModule) Migrations() []database.Migration {
	return []database.Migration{
		{
			Version:     1,
			Description: "Create watch_time_events table",
			SQL: `
				CREATE TABLE IF NOT EXISTS watch_time_events (
					id INTEGER PRIMARY KEY AUTOINCREMENT,
					streamer TEXT NOT NULL,
					timestamp INTEGER NOT NULL,
					minutes REAL NOT NULL
				);

				CREATE INDEX IF NOT EXISTS idx_watch_time_streamer_time ON watch_time_events(streamer, timestamp);
			`,
		},
	}
}

// NewWatchTimeStore registers the watch_time module's schema against db and
// returns a store for recording and querying rotation-fairness data.
func NewWatchTimeStore(db *database.DB) (*WatchTimeStore, error) {
	if err := db.RegisterModule(watchTimeModule{}); err != nil {
		return nil, fmt.Errorf("failed to register watch_time module: %w", err)
	}
	return &WatchTimeStore{db: db}, nil
}

// RecordMinutes credits minutes of watch time to streamer at "at", and
// opportunistically prunes events older than 2x watchTimeWindow so the
// table doesn't grow unbounded over long uptimes.
func (s *WatchTimeStore) RecordMinutes(streamer string, minutes float64, at time.Time) error {
	if minutes <= 0 {
		return nil
	}

	if _, err := s.db.Exec(
		`INSERT INTO watch_time_events (streamer, timestamp, minutes) VALUES (?, ?, ?)`,
		streamer, at.Unix(), minutes,
	); err != nil {
		return err
	}

	cutoff := at.Add(-2 * watchTimeWindow).Unix()
	_, err := s.db.Exec(`DELETE FROM watch_time_events WHERE timestamp < ?`, cutoff)
	return err
}

// WindowMinutes returns each requested streamer's accumulated watch minutes
// within the trailing watchTimeWindow ending at "at". Streamers with no
// events in the window are omitted from the result; callers should treat a
// missing entry as zero.
func (s *WatchTimeStore) WindowMinutes(usernames []string, at time.Time) (map[string]float64, error) {
	result := make(map[string]float64, len(usernames))
	if len(usernames) == 0 {
		return result, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(usernames)), ",")
	args := make([]interface{}, 0, len(usernames)+1)
	args = append(args, at.Add(-watchTimeWindow).Unix())
	for _, u := range usernames {
		args = append(args, u)
	}

	rows, err := s.db.Query(fmt.Sprintf(`
		SELECT streamer, SUM(minutes)
		FROM watch_time_events
		WHERE timestamp >= ? AND streamer IN (%s)
		GROUP BY streamer
	`, placeholders), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var streamer string
		var total float64
		if err := rows.Scan(&streamer, &total); err != nil {
			return nil, err
		}
		result[streamer] = total
	}
	return result, rows.Err()
}
