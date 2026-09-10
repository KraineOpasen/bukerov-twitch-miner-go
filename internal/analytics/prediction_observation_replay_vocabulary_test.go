package analytics

import (
	"context"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
)

// This file is the producer-side half of the replay's vocabulary contract.
//
// internal/predictioneval deliberately RE-DECLARES the phases and counter keys
// it reads, because importing this package would drag database/sql and the
// whole write path across its purity fence. The reader's own vocabulary test
// pins the exported values — kinds, stage states, health states, readings — but
// the phases and counter keys live in UNEXPORTED allowlists here and cannot be
// reached from outside the package at all. They were therefore unpinned, and
// they are the values whose drift is hardest to notice.
//
// The failure mode is specific and quiet. If the producer renamed AUTO_DECIDED
// or autoAttemptId, the replay would not crash and would not report a mismatch:
// it would find no terminal fact and no attempt discriminator, exclude every
// fact as NO_TERMINAL_FACT or NO_ATTEMPT_ID, and hand back an empty result that
// looks exactly like a session in which nothing happened.
//
// This is an in-package test because that is the only place the allowlists are
// visible. It creates no import cycle: internal/predictioneval imports no
// first-party package, which its dependency fence enforces.

// TestTheReplayVocabularyIsAdmittedByTheProducersAllowlists pins every phase
// and counter key the replay re-declares against the store's closed
// vocabularies.
func TestTheReplayVocabularyIsAdmittedByTheProducersAllowlists(t *testing.T) {
	phases := map[string]bool{}
	for _, p := range observationPhases {
		phases[p] = true
	}
	if len(phases) < 20 {
		t.Fatalf("only %d phases in the store's allowlist; this check is not reading it",
			len(phases))
	}

	for _, tc := range []struct{ what, value string }{
		{"the due phase", predictioneval.PhaseAutoDue},
		{"the decided terminal phase", predictioneval.PhaseAutoDecided},
		{"the skipped terminal phase", predictioneval.PhaseAutoSkipped},
		{"the placement call start", predictioneval.PhaseCallStarted},
		{"the placement call return", predictioneval.PhaseCallReturned},
		{"the delivered terminal phase", predictioneval.PhaseTerminalDelivered},
		{"the admitted terminal phase", predictioneval.PhaseTerminalAdmitted},
	} {
		if !phases[tc.value] {
			t.Errorf("%s: the replay reads %q, which the producer's closed phase vocabulary "+
				"does not contain. The replay would not fail loudly — it would find no fact "+
				"with that phase and report an empty session, which is indistinguishable from "+
				"a session in which nothing happened.", tc.what, tc.value)
		}
	}

	counters := map[string]bool{}
	for _, k := range observationCounterKeys {
		counters[k] = true
	}
	if len(counters) < 10 {
		t.Fatalf("only %d counter keys in the store's allowlist; this check is not reading it",
			len(counters))
	}

	for _, tc := range []struct{ what, value string }{
		// The discriminator is the load-bearing one: it is what links an
		// attempt's facts to each other.
		{"the attempt discriminator", predictioneval.CounterAutoAttemptID},
		{"the stake", predictioneval.CounterStake},
		{"the balance", predictioneval.CounterBalance},
		{"the payout", predictioneval.CounterPayout},
		{"the returned stake", predictioneval.CounterReturnedStake},
	} {
		if !counters[tc.value] {
			t.Errorf("%s: the replay reads the counter %q, which the producer's closed key "+
				"vocabulary does not contain. Every fact would then be excluded as carrying no "+
				"attempt id, and the replay would report nothing rather than a mismatch.",
				tc.what, tc.value)
		}
	}
}

// TestTheProducerStillWritesTheDecisionKindsTheReplayGroups pins the fact
// kinds the replay groups on, from the producer's side.
//
// The reader's test pins these too, by comparing exported constants. This one
// checks the other property: that they are members of the store's own closed
// KIND vocabulary, so a kind that was renamed in the allowlist but left as a
// stale exported constant is still caught.
func TestTheProducerStillWritesTheDecisionKindsTheReplayGroups(t *testing.T) {
	kinds := map[string]bool{}
	for _, k := range observationKinds {
		kinds[k] = true
	}
	if len(kinds) < 5 {
		t.Fatalf("only %d kinds in the store's allowlist; this check is not reading it",
			len(kinds))
	}
	for _, tc := range []struct{ what, value string }{
		{"the automatic decision", predictioneval.KindAutoDecision},
		{"the placement call", predictioneval.KindPlacement},
		{"the round verdict", predictioneval.KindUserTerminal},
	} {
		if !kinds[tc.value] {
			t.Errorf("%s: the replay groups on kind %q, which the producer's closed kind "+
				"vocabulary does not contain", tc.what, tc.value)
		}
	}
}

// TestTheOrphanCheckStopsAtTheFirstMatch pins the bound on the one scan in the
// session read that no caller-side preflight can cover.
//
// The classification asks a yes/no question — does any fact match exactly one
// half of the (epoch, session id) pair? — and it used to ask it with COUNT(*).
// Counting produces a number nobody reads, and in the case the bound exists for
// that number is expensive: a tampered store holding many rows under this
// session id but OTHER epochs makes the counting form walk all of them. The
// caller's epoch preflight cannot cover those rows, because they are not in the
// epoch it counted. Measured by review at 500,000 such rows: 5,000,029 VM steps
// for the count against 24 for the preflight.
//
// EXISTS stops at the first match, and that bounds both directions. Either a
// row matches and the scan ends there, or none does — which means every row
// carrying this session id also carries this epoch, and the epoch preflight
// already refused the load if there were too many of those.
//
// The check is STRUCTURAL, because the bound is not observable in the result:
// a count and a presence flag are both "orphans exist" to every caller. A
// wall-clock assertion would be flaky and would not say why it passed. So the
// query's shape is pinned directly, and the classification's use of it is
// pinned behaviourally beside it.
func TestTheOrphanCheckStopsAtTheFirstMatch(t *testing.T) {
	q := observationOrphanExistsQuery
	if !strings.Contains(q, "EXISTS(") {
		t.Errorf("the orphan query is not EXISTS-shaped, so it does not stop at the first "+
			"match:\n%s", q)
	}
	if strings.Contains(strings.ToUpper(q), "COUNT(") {
		t.Errorf("the orphan query counts. The classification only asks whether an orphan "+
			"exists, so counting walks every orphan a tampered store inserts to produce a "+
			"number nobody reads:\n%s", q)
	}
	// Both halves of the pair must still be asked about; a bound that dropped
	// one would be cheap and wrong.
	for _, half := range []string{"collector_epoch =  ?", "collector_epoch <> ?",
		"collector_session_id <> ?", "collector_session_id =  ?"} {
		if !strings.Contains(q, half) {
			t.Errorf("the orphan query no longer tests %q, so it cannot detect that half of "+
				"the pair:\n%s", half, q)
		}
	}

	// And the classification treats the presence flag as the integrity failure
	// it is — the property the query exists to serve.
	base := ObservationSessionRecord{
		CloseState: SessionComplete, ClosedAtKnown: true,
		CommittedCount: 1, LastAssignedSequence: 1, LastAssignedSequenceKnown: true,
	}
	facts := observationSessionFacts{Present: 1, MinSequence: 1, MaxSequence: 1,
		DistinctSequences: 1}
	if got := classifyObservationSession(base, facts); got.Reading == ReadingIntegrityError {
		t.Fatalf("a session with no orphans read as INTEGRITY_ERROR (%q); the positive case "+
			"below would then prove nothing", got.Detail)
	}
	facts.HalfPairPresent = true
	if got := classifyObservationSession(base, facts); got.Reading != ReadingIntegrityError {
		t.Errorf("a session with an orphaned fact read as %q, want INTEGRITY_ERROR", got.Reading)
	}
}

// TestTheOrphanCheckIsServedByIndexSeeksRatherThanATableScan pins the half of
// the orphan bound that lives in the SCHEMA rather than in the SQL.
//
// Review suggested this instrument in answer to a direct question about the
// weakness of the shape check above, and it is a genuinely better one — for a
// different property than the one it was offered for. Both claims were
// measured rather than taken:
//
//	EXISTS: SCAN CONSTANT ROW
//	EXISTS: SCALAR SUBQUERY 1
//	EXISTS: MULTI-INDEX OR
//	EXISTS:   SEARCH prediction_observations USING COVERING INDEX idx_predobs_exact_pair (collector_epoch=?)
//	EXISTS:   SEARCH prediction_observations USING INDEX idx_predobs_session (collector_session_id=?)
//	COUNT:  MULTI-INDEX OR
//	COUNT:    SEARCH prediction_observations USING COVERING INDEX idx_predobs_exact_pair (collector_epoch=?)
//	COUNT:    SEARCH prediction_observations USING INDEX idx_predobs_session (collector_session_id=?)
//
// The suggestion came with the claim that swapping EXISTS for COUNT(*) would
// change the plan's shape and so make this a pin on early termination. It does
// change the shape — the EXISTS form carries the two scalar-subquery rows — but
// the ACCESS PATH is identical, so the plan says nothing about stopping at the
// first match. That property stays pinned by the shape check above, and this
// test does not claim it.
//
// What this test does pin is the other half, which the shape check cannot see:
// that both branches of the OR are served by index SEEKS. An index dropped or
// renamed would leave the SQL still saying EXISTS while the database walked the
// whole table for every load — the bound gone, with every string assertion
// still passing.
func TestTheOrphanCheckIsServedByIndexSeeksRatherThanATableScan(t *testing.T) {
	repo := newTestRepo(t)
	rows, err := repo.db.QueryContext(context.Background(),
		"EXPLAIN QUERY PLAN "+observationOrphanExistsQuery,
		int64(1), "some-session", int64(1), "some-session")
	if err != nil {
		t.Fatalf("explain query plan: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var steps []string
	for rows.Next() {
		var id, parent, notUsed int64
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		steps = append(steps, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate plan: %v", err)
	}
	if len(steps) == 0 {
		t.Fatal("the planner returned no steps, so this check is inspecting nothing")
	}

	var touches []string
	for _, step := range steps {
		if strings.Contains(step, "prediction_observations") {
			touches = append(touches, step)
		}
	}
	if len(touches) != 2 {
		t.Fatalf("the plan touches prediction_observations %d times, want 2 — one per branch "+
			"of the pair. A single access means one branch stopped being asked about:\n  %s",
			len(touches), strings.Join(steps, "\n  "))
	}
	for _, step := range touches {
		if !strings.HasPrefix(step, "SEARCH ") || !strings.Contains(step, "INDEX") {
			t.Errorf("the planner reads prediction_observations as %q rather than an indexed "+
				"SEARCH. A full scan here walks every row of the table on every load, and the "+
				"SQL would still say EXISTS while it did.\n  %s",
				step, strings.Join(steps, "\n  "))
		}
	}

	// Named, because these two indexes are what make each branch a seek. The
	// bound is a property of the schema and the query TOGETHER, so a test that
	// pinned only the query would be pinning half of it.
	plan := strings.Join(steps, "\n")
	for _, index := range []string{"idx_predobs_exact_pair", "idx_predobs_session"} {
		if !strings.Contains(plan, index) {
			t.Errorf("the plan does not use %s:\n%s", index, plan)
		}
	}
}
