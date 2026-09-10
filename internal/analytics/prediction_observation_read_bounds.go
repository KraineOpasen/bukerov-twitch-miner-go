package analytics

import "context"

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
func (r *SQLiteRepository) ObservationSessionSizeBySession(ctx context.Context, sessionID string) (ObservationSessionSize, error) {
	var out ObservationSessionSize
	// COALESCE because SUM and MAX over no rows are NULL, and a session with
	// no facts is an ordinary answer rather than a scan error.
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(`+observationRowWidthBytes+`), 0),
		       COALESCE(MAX(`+observationRowWidthBytes+`), 0)
		FROM prediction_observations
		WHERE collector_session_id = ?`, sessionID).
		Scan(&out.Rows, &out.TotalBytes, &out.WidestRowBytes)
	if err != nil {
		return ObservationSessionSize{}, err
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
		  AND (SELECT COALESCE(SUM(` + observationRowWidthBytes + `), 0)
		       FROM prediction_observations WHERE collector_session_id = ?) <= ?
		  AND (SELECT COALESCE(MAX(` + observationRowWidthBytes + `), 0)
		       FROM prediction_observations WHERE collector_session_id = ?) <= ?
		ORDER BY collector_sequence ASC`
	args := []interface{}{sessionID, sessionID, maxTotalBytes, sessionID, maxRowBytes}
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
