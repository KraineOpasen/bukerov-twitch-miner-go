package watcher

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
)

// ErrStreamerDeleted is returned by RecordMinutes for a login whose lifecycle is
// being deleted, so a late watch tick cannot resurrect rotation-fairness rows
// for a removed streamer.
var ErrStreamerDeleted = errors.New("watch_time: streamer deleted")

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

// watchTimeCreditTimeout ceilings how long one watch_time credit may wait on the
// shared handle before giving up, so a credit can never hold a shutdown's
// database Close open indefinitely. It is not a generation deadline: a credit is
// deliberately not cancellable by the watch teardown that is draining it. Sized
// for the pathological case only — a single-row insert on this handle completes
// in well under a millisecond — and matching the other bounded teardown joins.
// Package variable so tests can shrink it, like stopJoinTimeout.
var watchTimeCreditTimeout = 5 * time.Second

// WatchTimeStore persists accumulated per-streamer watch minutes in the
// shared SQLite database (under the /database volume), so the fair-rotation
// ranking survives container restarts instead of resetting to all-zero.
type WatchTimeStore struct {
	db *database.DB

	// mu serializes RecordMinutes against the resurrection fence; deleted holds
	// tombstoned lowercase logins. RecordMinutes holds mu across its check+insert
	// and Tombstone takes mu, so once Tombstone returns no later write inserts a
	// row for the login (the barrier contract shared by every store here).
	mu      sync.Mutex
	deleted map[string]struct{}
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
	return &WatchTimeStore{db: db, deleted: make(map[string]struct{})}, nil
}

// Tombstone arms the resurrection fence for login; RecordMinutes then returns
// ErrStreamerDeleted instead of writing a new rotation-fairness row. Idempotent.
func (s *WatchTimeStore) Tombstone(login string) {
	login = strings.ToLower(login)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted[login] = struct{}{}
}

// Reinstate clears the fence for login so a re-added streamer accrues fresh
// watch time. Idempotent.
func (s *WatchTimeStore) Reinstate(login string) {
	login = strings.ToLower(login)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.deleted, login)
}

func (s *WatchTimeStore) tombstonedLocked(login string) bool {
	_, ok := s.deleted[strings.ToLower(login)]
	return ok
}

// RenameStreamer repoints one login's watch-time rows from oldLogin to newLogin
// so a config-driven rename keeps rotation-fairness history attached to the
// streamer's CURRENT login (matching analytics/notifications), which in turn
// keeps the rows deletable by that login (BKM-018A D15/D16). Best-effort single
// UPDATE; no-op when the logins match or either is empty. Case-insensitive.
func (s *WatchTimeStore) RenameStreamer(oldLogin, newLogin string) error {
	oldLogin = strings.ToLower(oldLogin)
	newLogin = strings.ToLower(newLogin)
	if oldLogin == "" || newLogin == "" || oldLogin == newLogin {
		return nil
	}
	_, err := s.db.Exec("UPDATE watch_time_events SET streamer = ? WHERE streamer = ? COLLATE NOCASE", newLogin, oldLogin)
	return err
}

// RenameStreamerTx is the transaction body of RenameStreamer, exposed so it can
// join an atomic multi-store rename. No-op when the logins match or either is
// empty. Case-insensitive.
func (s *WatchTimeStore) RenameStreamerTx(tx *sql.Tx, oldLogin, newLogin string) error {
	oldLogin = strings.ToLower(oldLogin)
	newLogin = strings.ToLower(newLogin)
	if oldLogin == "" || newLogin == "" || oldLogin == newLogin {
		return nil
	}
	_, err := tx.Exec("UPDATE watch_time_events SET streamer = ? WHERE streamer = ? COLLATE NOCASE", newLogin, oldLogin)
	return err
}

// DeleteStreamerTx removes all of one login's watch-time rows within the
// caller's transaction, joining the atomic multi-store streamer purge. Returns
// true when any row existed. Idempotent. The caller is expected to have
// Tombstone()d the login first.
func (s *WatchTimeStore) DeleteStreamerTx(tx *sql.Tx, login string) (bool, error) {
	login = strings.ToLower(login)
	if login == "" {
		return false, nil
	}
	res, err := tx.Exec("DELETE FROM watch_time_events WHERE streamer = ? COLLATE NOCASE", login)
	if err != nil {
		return false, fmt.Errorf("delete watch_time_events for %q: %w", login, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, nil
	}
	return n > 0, nil
}

// RecordMinutes credits minutes of watch time to streamer at "at", and
// opportunistically prunes events older than 2x watchTimeWindow so the
// table doesn't grow unbounded over long uptimes.
func (s *WatchTimeStore) RecordMinutes(streamer string, minutes float64, at time.Time) error {
	if minutes <= 0 {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tombstonedLocked(streamer) {
		return ErrStreamerDeleted
	}

	// The credit goes through database.DB's own closed-barrier (WithTx), not
	// the embedded *sql.DB, because this is the one watch-generation write that
	// is deliberately NOT bound to the generation context: the bounded join in
	// MinuteWatcher.Stop exists so it drains before the owner closes the
	// database, and when that join expires the owner closes anyway. Through the
	// barrier the two outcomes of that race are both correct — a credit that
	// reaches it first makes Close wait for this ONE statement, and a credit
	// that arrives after is refused with database.ErrClosed, which a caller can
	// actually recognise. Through the embedded handle neither held: Close ran
	// straight past a parked credit and killed it, and the refusal was an
	// unmatchable driver string. The wait this adds is one INSERT long and is
	// the same bounded wait every other store here already imposes on Close; it
	// is NOT a wait for the watch generation to quiesce.
	//
	// Lock order is s.mu -> database.DB.mu (RLock). Nothing may take them the
	// other way round: a WithTx body that called Tombstone/Reinstate/
	// RecordMinutes would deadlock against a pending Close under Go's
	// writer-preferring RWMutex. Today no such call site exists — the purge
	// coordinator tombstones strictly BEFORE it opens its transaction, and
	// RenameStreamerTx/DeleteStreamerTx take no store lock at all.
	//
	// The credit is bounded but NOT by the generation context: a teardown must
	// never cancel it (that is what the bounded join is for), yet it must not be
	// able to hold the shutdown owner's Close open forever either. Waiting on
	// the shared handle's single connection is the one step here with no
	// intrinsic bound, and before this write took the barrier a stuck connection
	// only cost the credit — Close could still proceed and wake it. Now Close
	// waits behind it, so the wait needs its own ceiling: without one, one
	// wedged connection holder would wedge shutdown itself, which is exactly the
	// "wait forever" this whole teardown design refuses.
	writeCtx, cancelWrite := context.WithTimeout(context.Background(), watchTimeCreditTimeout)
	defer cancelWrite()
	if err := s.db.WithTx(writeCtx, func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO watch_time_events (streamer, timestamp, minutes) VALUES (?, ?, ?)`,
			streamer, at.Unix(), minutes,
		)
		return err
	}); err != nil {
		return err
	}

	// Opportunistic housekeeping, deliberately outside the credit's transaction
	// AND outside its verdict. Inside the transaction a prune failure would roll
	// back credited watch time — strictly worse than the loss being repaired.
	// Reported as the caller's error it would be worse still: the row is already
	// committed, so a prune that lost its own race against Close would make a
	// landed credit look like a failed one.
	cutoff := at.Add(-2 * watchTimeWindow).Unix()
	if _, err := s.db.Exec(`DELETE FROM watch_time_events WHERE timestamp < ?`, cutoff); err != nil {
		slog.Debug("Failed to prune old watch-time events", "error", err)
	}
	return nil
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
