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

// observationRowWidthBytes is the byte width of every VARIABLE-WIDTH column a
// read of observationSelectColumns materializes.
//
// payload_json alone is not the bound. The row also carries a dozen TEXT
// columns, and the schema constrains the length of NONE of them: a store that
// kept payload_json small while putting a very large value in, say,
// pool_instance_id or source_fingerprint would measure as tiny and still be
// scanned into Go strings in full. The integer, REAL and fixed-vocabulary
// columns are omitted because their width is bounded by their type.
//
// LENGTH is applied to each value CAST to BLOB because LENGTH on TEXT counts
// CHARACTERS. A value of multi-byte runes would otherwise measure smaller than
// the memory it occupies, and a bound computed from it would be an
// underestimate in precisely the case that matters.
const observationRowWidthBytes = `(
	LENGTH(CAST(observation_id AS BLOB)) +
	LENGTH(CAST(collector_session_id AS BLOB)) +
	LENGTH(CAST(pool_instance_id AS BLOB)) +
	LENGTH(CAST(COALESCE(round_incarnation_id, '') AS BLOB)) +
	LENGTH(CAST(COALESCE(round_capture_origin, '') AS BLOB)) +
	LENGTH(CAST(COALESCE(round_capture_gap_cause, '') AS BLOB)) +
	LENGTH(CAST(COALESCE(routed_channel_id, '') AS BLOB)) +
	LENGTH(CAST(COALESCE(round_owner_channel_id, '') AS BLOB)) +
	LENGTH(CAST(COALESCE(retention_group_owner_channel_id, '') AS BLOB)) +
	LENGTH(CAST(COALESCE(event_id, '') AS BLOB)) +
	LENGTH(CAST(kind AS BLOB)) +
	LENGTH(CAST(COALESCE(source_topic_type, '') AS BLOB)) +
	LENGTH(CAST(COALESCE(source_message_type, '') AS BLOB)) +
	LENGTH(CAST(COALESCE(source_fingerprint, '') AS BLOB)) +
	LENGTH(CAST(producer_time_source AS BLOB)) +
	LENGTH(CAST(payload_json AS BLOB)) +
	LENGTH(CAST(observation_sha256 AS BLOB))
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
