package analytics

import (
	"context"
	"errors"
)

// errNonPositiveCandidateBound refuses a measurement with no bound, which
// would defeat the point of asking for one.
var errNonPositiveCandidateBound = errors.New(
	"analytics: observation session size needs a positive candidate-row bound")

// This file adds read-only measurement and a bounded read used by the offline
// replay reader. It performs no write, no migration and no schema change, and
// nothing in the miner's runtime path calls it.
//
// It exists because MaxObservationPayloadBytes is enforced by the WRITER. That
// is the right place for it while the store is the only thing filling the
// table, but it says nothing about a store that was tampered with or copied in
// from elsewhere. A reader that trusts the writer's ceiling has no ceiling at
// all against exactly the input it is most important to bound.

// observationRowWidthBytes is the ACTUAL stored byte width of every column a
// read of observationSelectColumns materializes. Every column, without
// exception — the choice of which to measure is not a judgement call.
//
// payload_json alone is not the bound: the row carries a dozen further TEXT
// columns whose length the schema does not constrain. Neither is "the
// variable-width ones", which is where an earlier version of this expression
// stopped, on the reasoning that integer and REAL columns are bounded by their
// declared type. That reasoning is FALSE here, and measurably so.
//
// prediction_observations is not a STRICT table, and outside a STRICT table
// SQLite treats a column's declared type as an affinity, not a constraint: an
// INTEGER-affinity column will hold an arbitrarily large TEXT or BLOB. Measured
// directly against modernc.org/sqlite, a 300 KB string stored in an
// INTEGER-affinity column yields typeof() = "text" and a stored width of
// 300,000 bytes, while an expression covering only the "variable-width" columns
// reports 2. The guarded SELECT would then hand that value across the driver
// before Scan rejected its type — past both the per-row and the session bound.
//
// So affinity is not consulted at all. Every selected column is measured by its
// real stored length, and the structural test in this package fails if the
// SELECT list and this expression ever disagree about which columns exist.
//
// LENGTH is applied to each value CAST to BLOB because LENGTH on TEXT counts
// CHARACTERS: a value of multi-byte runes would otherwise measure smaller than
// the memory it occupies. COALESCE covers NULL columns, since LENGTH(NULL) is
// NULL and one NULL would poison the whole SUM.
const observationRowWidthBytes = `(
	COALESCE(LENGTH(CAST(id AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(observation_id AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(collector_session_id AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(collector_epoch AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(collector_sequence AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(pool_instance_id AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(round_incarnation_id AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(round_capture_origin AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(round_capture_gap_cause AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(routed_streamer_id AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(routed_channel_id AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(round_owner_streamer_id AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(round_owner_channel_id AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(retention_group_owner_streamer_id AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(retention_group_owner_channel_id AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(event_id AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(kind AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(source_topic_type AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(source_message_type AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(source_fingerprint AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(producer_at_ms AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(producer_time_source AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(received_at_ms AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(connection_index AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(connection_generation AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(connection_sequence AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(payload_version AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(payload_json AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(observation_sha256 AS BLOB)), 0)
)`

// ObservationSessionSize is the measured cost of loading one session's facts,
// obtained without materializing any of them.
type ObservationSessionSize struct {
	// Rows is the number of facts stored for the session.
	Rows int64
	// TotalBytes is the summed width of every variable-width column those rows
	// would materialize.
	//
	// It is the AGGREGATE, deliberately. A per-row ceiling alone leaves the
	// interesting case open: many facts each just under the limit cost their
	// sum, and the sum is what a caller has to hold.
	TotalBytes int64
	// WidestRowBytes is the largest single row's width. It is carried
	// separately because an aggregate bound cannot be enforced by scanning:
	// aborting a scan part-way still materializes the row that broke it, so a
	// single row needs a bound of its own.
	WidestRowBytes int64
}

// ObservationSessionSizeBySession measures one session's stored facts without
// reading their content.
//
// SQLite evaluates the aggregate row by row and never hands a value across the
// driver boundary, so the cost of asking is bounded whatever the table holds —
// which is the entire point of asking before the load rather than discovering
// the size by allocating it.
func (r *SQLiteRepository) ObservationSessionSizeBySession(
	ctx context.Context, sessionID string, candidateRows int,
) (ObservationSessionSize, error) {
	if candidateRows <= 0 {
		return ObservationSessionSize{}, errNonPositiveCandidateBound
	}
	var out ObservationSessionSize
	// The aggregate runs over a BOUNDED candidate set, not the whole session.
	// Measuring every row first would let a store holding millions of small
	// rows spend unbounded database CPU and I/O before the row-count limit was
	// consulted — the returned allocation would be bounded and the work to
	// reach it would not.
	//
	// The caller passes limit+1, so a session AT the limit and one OVER it stay
	// distinguishable: COUNT reaching limit+1 is the refusal signal, and the
	// byte aggregates describe exactly the rows a load would have returned.
	//
	// COALESCE because SUM and MAX over no rows are NULL, and a session with
	// no facts is an ordinary answer rather than a scan error.
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(w), 0), COALESCE(MAX(w), 0)
		FROM (
			SELECT `+observationRowWidthBytes+` AS w
			FROM prediction_observations
			WHERE collector_session_id = ?
			ORDER BY collector_sequence ASC
			LIMIT ?
		)`, sessionID, candidateRows).
		Scan(&out.Rows, &out.TotalBytes, &out.WidestRowBytes)
	if err != nil {
		return ObservationSessionSize{}, err
	}
	return out, nil
}

// observationSessionRowWidthBytes is the actual stored byte width of EVERY
// column ReadObservationSession materializes.
//
// It is applied as a predicate INSIDE that read (see
// ReadObservationSessionWithinBudget) rather than by a measurement taken
// beforehand. A separate measuring statement leaves a window another
// connection can commit into, after which the read materializes a value no
// bound ever covered.
//
// Every column again, and for the same reason as observationRowWidthBytes:
// prediction_observation_sessions is not STRICT either, so a declared type is
// an affinity and not a constraint. An earlier version of this expression
// measured three columns — the ones that look like text — and left nine
// INTEGER-affinity ones unmeasured, which is precisely the mistake the
// observation-row expression already exists to document.
//
// The CHECK (col >= 0) constraints on five of those columns do NOT close it.
// SQLite's storage-class ordering ranks TEXT above INTEGER, so comparing a TEXT
// value against the integer literal 0 with >= is true whatever the text
// contains. Measured directly against modernc.org/sqlite: inserting a 250 KB
// string into an INTEGER NOT NULL CHECK (col >= 0) column succeeds, and the
// column then reports typeof() = "text" with a stored width of 250 000 bytes.
// Three of the columns carry no CHECK at all.
const observationSessionRowWidthBytes = `(
	COALESCE(LENGTH(CAST(collector_epoch AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(collector_session_id AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(producer_revision AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(started_at_ms AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(closed_at_ms AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(close_state AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(last_assigned_sequence AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(committed_count AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(dropped_count AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(unsettled_obligation_count AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(post_fence_producer_count AS BLOB)), 0) +
	COALESCE(LENGTH(CAST(producer_shutdown_uncertain_count AS BLOB)), 0)
)`

// ObservationEpochSize is the measured cost of the fact rows one COLLECTOR
// EPOCH holds, obtained without materializing any of them.
//
// It is keyed on the epoch rather than the session id because it answers a
// question asked BEFORE the session id is known — see
// ObservationEpochRowBounds.
type ObservationEpochSize struct {
	// Rows is how many facts the epoch holds, counted over the candidate
	// window only. A count equal to the window means the window truncated and
	// the measurement describes a prefix, not the epoch.
	Rows int64
	// WidestRowBytes is the largest single row's stored width within that
	// window.
	WidestRowBytes int64
}

// ObservationEpochRowBounds measures the fact rows an epoch holds, bounded to
// a candidate window, without materializing any of them.
//
// It exists for one specific allocation, which no other bound in this file
// covers. ReadObservationSession does not only read the session row: inside
// the same transaction it recomputes the stored witness of a bounded PREFIX of
// that session's facts, and doing so scans payload_json and a dozen other
// columns of each row into memory one row at a time. Those rows are read
// before LoadSession has any session id to measure against, so every per-row
// and aggregate bound in this file applied strictly after the store had
// already materialized them. Measured directly: a payload_json tampered to
// 4 MiB — four times the reader's per-row ceiling — was scanned and hashed in
// full, and the load then returned no facts and no error, having already paid
// for it.
//
// The window is keyed on collector_epoch alone, because that is the only key
// the caller holds at that point. That makes the measured set a SUPERSET of
// the rows the witness sweep can touch, which is what soundness needs here: the
// sweep is scoped to (epoch, session id) and this covers the epoch entire.
//
// The window also makes the work bounded, and the two properties only hold
// together. A truncated window would describe a prefix rather than the epoch,
// so the caller must refuse any epoch whose count reaches the window rather
// than read on — at which point the window provably covered every row the
// sweep could reach. Ordering is deliberately absent: any ordering would make
// the window a particular prefix of the epoch instead of a set the caller can
// only accept when it is complete.
func (r *SQLiteRepository) ObservationEpochRowBounds(
	ctx context.Context, epoch int64, candidateRows int,
) (ObservationEpochSize, error) {
	if candidateRows <= 0 {
		return ObservationEpochSize{}, errNonPositiveCandidateBound
	}
	var out ObservationEpochSize
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(MAX(w), 0)
		FROM (
			SELECT `+observationRowWidthBytes+` AS w
			FROM prediction_observations
			WHERE collector_epoch = ?
			LIMIT ?
		)`, epoch, candidateRows).Scan(&out.Rows, &out.WidestRowBytes)
	if err != nil {
		return ObservationEpochSize{}, err
	}
	return out, nil
}

// ObservationsBySessionWithinBudget reads a session's facts ONLY if they fit
// the given bounds, with the bounds evaluated in the same statement as the
// read.
//
// That co-location is the whole point and is not an optimization. A budget
// checked by an earlier, separate query is a time-of-check/time-of-use gap:
// another connection can enlarge a value in between, the row count is
// unchanged so a count re-check still passes, and the enlarged value is
// materialized having never been covered by any bound. Here the guard travels
// WITH the data, so it cannot be stale by the time the rows are produced.
//
// Both bounds are enforced, for different reasons. maxTotalBytes bounds what
// the caller ends up holding. maxRowBytes bounds what a single row can cost,
// which an aggregate cannot: a scan aborted on exceeding a running total has
// already materialized the row that exceeded it.
//
// It returns withinBudget=false and no rows when either bound is exceeded. A
// caller that measured a non-empty session and then gets no rows has learned
// that the store changed underneath it, which is a different fact from an
// empty session and one the reader reports as such.
func (r *SQLiteRepository) ObservationsBySessionWithinBudget(
	ctx context.Context, sessionID string, limit int, maxRowBytes, maxTotalBytes int64,
) (records []ObservationRecord, withinBudget bool, err error) {
	// The two guard subqueries are uncorrelated, so SQLite evaluates each once
	// for the statement — over the same snapshot that produces the rows.
	query := `SELECT ` + observationSelectColumns + `
		FROM prediction_observations
		WHERE collector_session_id = ?
		  AND (SELECT COALESCE(SUM(w), 0) FROM (
		         SELECT ` + observationRowWidthBytes + ` AS w FROM prediction_observations
		         WHERE collector_session_id = ? ORDER BY collector_sequence ASC LIMIT ?)) <= ?
		  AND (SELECT COALESCE(MAX(w), 0) FROM (
		         SELECT ` + observationRowWidthBytes + ` AS w FROM prediction_observations
		         WHERE collector_session_id = ? ORDER BY collector_sequence ASC LIMIT ?)) <= ?
		ORDER BY collector_sequence ASC`
	// The guard subqueries are bounded to the same candidate set the read
	// returns, so an over-limit session is refused after bounded work rather
	// than after aggregating every row a tampered store cares to insert.
	args := []interface{}{
		sessionID,
		sessionID, limit, maxTotalBytes,
		sessionID, limit, maxRowBytes,
	}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()

	records, err = scanObservationRows(rows)
	if err != nil {
		return nil, false, err
	}
	return records, len(records) > 0, nil
}
