package analytics

import "context"

// This file adds ONE read-only measurement used by the offline replay reader.
// It performs no write, no migration and no schema change, and nothing in the
// miner's runtime path calls it.
//
// It exists because MaxObservationPayloadBytes is enforced by the WRITER. That
// is the right place for it while the store is the only thing filling the
// table, but it says nothing about a store that was tampered with or copied in
// from elsewhere. A reader that trusts the writer's ceiling has no ceiling at
// all against exactly the input it is most important to bound.

// ObservationSessionSize is the measured cost of loading one session's facts,
// obtained without materializing any of them.
type ObservationSessionSize struct {
	// Rows is the number of facts stored for the session.
	Rows int64
	// PayloadBytes is the total size of their payload_json columns, in BYTES.
	//
	// It is the AGGREGATE, deliberately. A per-payload ceiling would leave the
	// interesting case open: many facts each just under the limit cost their
	// sum, and the sum is what a caller has to hold.
	PayloadBytes int64
}

// ObservationSessionSizeBySession measures one session's stored facts without
// reading their content.
//
// SQLite evaluates the aggregate row by row and never hands a payload across
// the driver boundary, so the cost of asking is bounded whatever the table
// holds — which is the entire point of asking before the load rather than
// discovering the size by allocating it.
//
// LENGTH is applied to the value CAST to BLOB because LENGTH on TEXT counts
// CHARACTERS. A payload of multi-byte runes would otherwise measure smaller
// than the memory it occupies, and a bound computed from it would be an
// underestimate in precisely the case that matters.
func (r *SQLiteRepository) ObservationSessionSizeBySession(ctx context.Context, sessionID string) (ObservationSessionSize, error) {
	var out ObservationSessionSize
	// COALESCE because SUM over no rows is NULL, and a session with no facts
	// is an ordinary answer rather than a scan error.
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(LENGTH(CAST(payload_json AS BLOB))), 0)
		FROM prediction_observations
		WHERE collector_session_id = ?`, sessionID).Scan(&out.Rows, &out.PayloadBytes)
	if err != nil {
		return ObservationSessionSize{}, err
	}
	return out, nil
}
