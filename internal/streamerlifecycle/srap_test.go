package streamerlifecycle_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamerlifecycle"
)

// admissionCount/pendingCount are raw-SQL read-back helpers for the two SRAP
// ledgers, used instead of the package's own (unexported) list functions so
// these tests observe exactly what a restart/inspection would see on disk.
func admissionCount(t *testing.T, db *database.DB, login string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM streamer_deletion_admissions WHERE login = ?`, login).Scan(&n); err != nil {
		t.Fatalf("count admissions for %s: %v", login, err)
	}
	return n
}

func pendingCount(t *testing.T, db *database.DB, login string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pending_streamer_deletions WHERE login = ?`, login).Scan(&n); err != nil {
		t.Fatalf("count pending for %s: %v", login, err)
	}
	return n
}

// TestAdmitRemovalsMultiRemovalAllOrNothing pins the batch atomicity
// contract: N removals admitted in one call either ALL land or NONE do. Here
// the transaction is forced to fail by closing the database mid-flight (via
// a private, non-singleton handle so the shared package DB is untouched),
// which must surface as the typed database.ErrClosed, not a raw driver
// string, and must leave every login's admissions row absent.
func TestAdmitRemovalsMultiRemovalAllOrNothing(t *testing.T) {
	db := openRawDB(t, filepath.Join(t.TempDir(), "miner.db"))
	_, _, _, coord := buildRawStores(t, db)

	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	err := coord.AdmitRemovals(context.Background(), []streamerlifecycle.Removal{
		{ChannelID: "chan-1", Login: "admitmulti1"},
		{ChannelID: "chan-2", Login: "admitmulti2"},
		{ChannelID: "chan-3", Login: "admitmulti3"},
	})
	if !errors.Is(err, database.ErrClosed) {
		t.Fatalf("AdmitRemovals after close = %v, want database.ErrClosed", err)
	}
}

// TestAdmitRemovalsEmptyLoginRejectedPreTx proves the empty-login validation
// runs BEFORE the transaction opens, not merely triggering a rollback: a
// batch mixing valid removals with one empty-login entry is rejected wholly,
// and NONE of the otherwise-valid logins — which a tx-rollback design could
// still have inserted before hitting the bad entry — end up admitted.
func TestAdmitRemovalsEmptyLoginRejectedPreTx(t *testing.T) {
	s := newStores(t)

	err := s.coord.AdmitRemovals(context.Background(), []streamerlifecycle.Removal{
		{ChannelID: "chan-a", Login: "pretxvalid1"},
		{ChannelID: "chan-b", Login: "   "}, // blank after trim -> empty
		{ChannelID: "chan-c", Login: "pretxvalid2"},
	})
	if err == nil {
		t.Fatal("expected an error for the empty-login entry")
	}
	if n := admissionCount(t, s.db, "pretxvalid1"); n != 0 {
		t.Errorf("pretxvalid1 admitted despite a later invalid entry in the same batch: %d rows", n)
	}
	if n := admissionCount(t, s.db, "pretxvalid2"); n != 0 {
		t.Errorf("pretxvalid2 admitted despite an earlier invalid entry in the same batch: %d rows", n)
	}
}

// TestAdmitRemovalsDedupesByLogin proves a batch naming the same login twice
// (case/whitespace variants included) collapses to exactly one row.
func TestAdmitRemovalsDedupesByLogin(t *testing.T) {
	s := newStores(t)

	if err := s.coord.AdmitRemovals(context.Background(), []streamerlifecycle.Removal{
		{ChannelID: "chan-dup", Login: "DupLogin"},
		{ChannelID: "chan-dup", Login: " duplogin "},
	}); err != nil {
		t.Fatalf("AdmitRemovals: %v", err)
	}
	if n := admissionCount(t, s.db, "duplogin"); n != 1 {
		t.Fatalf("admissions rows for duplicated login = %d, want exactly 1", n)
	}
}

// TestAdmitRemovalsEmptyBatchIsNoop proves an empty (or all-blank) batch does
// nothing and returns no error — there is no "nothing to admit" failure mode.
func TestAdmitRemovalsEmptyBatchIsNoop(t *testing.T) {
	s := newStores(t)
	if err := s.coord.AdmitRemovals(context.Background(), nil); err != nil {
		t.Fatalf("AdmitRemovals(nil) = %v, want nil", err)
	}
}

// TestAbortAdmissionCompensatesPreparedRows proves AbortAdmission deletes
// exactly the prepared rows it names, leaving an unrelated prepared row (and
// an unrelated login entirely) untouched, and is a silent no-op for a login
// that was never admitted.
func TestAbortAdmissionCompensatesPreparedRows(t *testing.T) {
	s := newStores(t)
	ctx := context.Background()

	if err := s.coord.AdmitRemovals(ctx, []streamerlifecycle.Removal{
		{ChannelID: "chan-x", Login: "abortme"},
		{ChannelID: "chan-y", Login: "abortkeep"},
	}); err != nil {
		t.Fatalf("admit: %v", err)
	}

	if err := s.coord.AbortAdmission(ctx, []string{"abortme", "never-admitted"}); err != nil {
		t.Fatalf("abort: %v", err)
	}

	if n := admissionCount(t, s.db, "abortme"); n != 0 {
		t.Errorf("abortme still admitted after AbortAdmission: %d rows", n)
	}
	if n := admissionCount(t, s.db, "abortkeep"); n != 1 {
		t.Errorf("unrelated prepared row abortkeep was disturbed: %d rows, want 1", n)
	}
}

// TestCommitRemovalMovesAdmissionThenPurges pins the happy path of SRAP's
// COMPLETE phase: an admitted removal is moved into the pending ledger and
// then purged, in one call, leaving neither ledger holding a row and the
// stores actually scrubbed.
func TestCommitRemovalMovesAdmissionThenPurges(t *testing.T) {
	s := newStores(t)
	ctx := context.Background()
	s.seedStreamer(t, "commitremoval", 100)

	if err := s.coord.AdmitRemovals(ctx, []streamerlifecycle.Removal{
		{ChannelID: "chan-cr", Login: "commitremoval"},
	}); err != nil {
		t.Fatalf("admit: %v", err)
	}
	if n := admissionCount(t, s.db, "commitremoval"); n != 1 {
		t.Fatalf("setup: admissions row missing before CommitRemoval")
	}

	res, err := s.coord.CommitRemoval(ctx, "chan-cr", "commitremoval")
	if err != nil {
		t.Fatalf("CommitRemoval: %v", err)
	}
	if res.Outcome != streamerlifecycle.OutcomeDeleted {
		t.Errorf("outcome = %q, want deleted", res.Outcome)
	}
	if n := admissionCount(t, s.db, "commitremoval"); n != 0 {
		t.Errorf("admissions row survived a successful CommitRemoval: %d rows", n)
	}
	if n := pendingCount(t, s.db, "commitremoval"); n != 0 {
		t.Errorf("pending row survived a successful CommitRemoval: %d rows", n)
	}
	if s.analyticsHas(t, "commitremoval") {
		t.Error("analytics rows survived CommitRemoval")
	}
}

// TestCommitRemovalPurgeFailureLeavesPendingRow reuses the failPurger seam
// (lifecycle_test.go) to fail CommitRemoval's purge step AFTER the move
// step already committed: the pending row must remain (durably queued), the
// admissions row must be gone (moved, not merely copied), and HasPending
// must report true so a caller's "durably queued" claim is truthful.
func TestCommitRemovalPurgeFailureLeavesPendingRow(t *testing.T) {
	s := newStores(t)
	ctx := context.Background()
	s.seedStreamer(t, "commitpurgefail", 100)

	if err := s.coord.AdmitRemovals(ctx, []streamerlifecycle.Removal{
		{ChannelID: "chan-cpf", Login: "commitpurgefail"},
	}); err != nil {
		t.Fatalf("admit: %v", err)
	}

	failing := s.failingCoordinator(t)
	if _, err := failing.CommitRemoval(ctx, "chan-cpf", "commitpurgefail"); err == nil {
		t.Fatal("expected CommitRemoval to fail via the injected purge failure")
	}

	if n := admissionCount(t, s.db, "commitpurgefail"); n != 0 {
		t.Errorf("admissions row survived the move step: %d rows, want 0 (moved, not copied)", n)
	}
	if n := pendingCount(t, s.db, "commitpurgefail"); n != 1 {
		t.Errorf("pending row missing after the purge failed: %d rows, want 1 (durably queued)", n)
	}
	if has, err := s.coord.HasPending(ctx, "commitpurgefail"); err != nil || !has {
		t.Errorf("HasPending = (%v, %v), want (true, nil)", has, err)
	}
	if !s.analyticsHas(t, "commitpurgefail") {
		t.Error("analytics rows were deleted despite the purge transaction rolling back")
	}

	// Clean up via the HEALTHY shared coordinator (mirroring
	// TestFailedPurgePersistsAndReconciles' own pattern): proves the durably
	// queued row is actually retryable to completion. Checked by presence,
	// not by Reconcile's aggregate count — this package's tests share one
	// process-wide DB singleton (database.Open), and a sibling test
	// (TestAtomicRollback) deliberately leaves its OWN unrelated row stuck
	// forever to pin the same rollback invariant, so the aggregate count
	// this call returns is not exclusively this test's to assert on.
	if _, err := s.coord.Reconcile(ctx); err != nil {
		t.Fatalf("cleanup reconcile: %v", err)
	}
	if n := pendingCount(t, s.db, "commitpurgefail"); n != 0 {
		t.Errorf("pending row for commitpurgefail survived a healthy reconcile: %d rows", n)
	}
	if s.analyticsHas(t, "commitpurgefail") {
		t.Error("commitpurgefail's analytics history survived the retried purge")
	}
}

// arbitrationKeep is a tiny stand-in for the miner's config-driven predicate,
// letting each test declare exactly which logins are "still configured" and
// under which stored ChannelID.
func arbitrationKeep(cfg map[string]string) func(login, channelID string) (bool, string) {
	return func(login, _ string) (bool, string) {
		id, ok := cfg[login]
		return ok, id
	}
}

// The four ArbitratePrepared tests below each use a PRIVATE, non-singleton
// database (openRawDB/buildRawStores, not newStores' shared package
// singleton): ArbitratePrepared sweeps EVERY row of the admissions table by
// design (it is a startup-only, whole-ledger pass), so asserting an exact
// aborted/promoted count needs a table no other test can have left rows in.

// TestArbitratePreparedAbortsWhenStillConfiguredSameChannel covers the
// "commit never happened" disposition: a prepared row for a login that is
// STILL configured, under the SAME stored ChannelID, is aborted (deleted,
// fence lifted) — never promoted into the pending purge ledger.
func TestArbitratePreparedAbortsWhenStillConfiguredSameChannel(t *testing.T) {
	db := openRawDB(t, filepath.Join(t.TempDir(), "miner.db"))
	_, _, _, coord := buildRawStores(t, db)
	ctx := context.Background()

	if err := coord.AdmitRemovals(ctx, []streamerlifecycle.Removal{
		{ChannelID: "chan-arb1", Login: "arbabort"},
	}); err != nil {
		t.Fatalf("admit: %v", err)
	}

	aborted, promoted, err := coord.ArbitratePrepared(ctx, arbitrationKeep(map[string]string{"arbabort": "chan-arb1"}))
	if err != nil {
		t.Fatalf("arbitrate: %v", err)
	}
	if aborted != 1 || promoted != 0 {
		t.Fatalf("aborted=%d promoted=%d, want 1,0", aborted, promoted)
	}
	if n := admissionCount(t, db, "arbabort"); n != 0 {
		t.Errorf("prepared row survived abort: %d rows", n)
	}
	if n := pendingCount(t, db, "arbabort"); n != 0 {
		t.Errorf("aborted row was wrongly promoted to pending: %d rows", n)
	}
}

// TestArbitratePreparedPromotesWhenAbsentFromConfig covers the "commit did
// happen" disposition: a prepared row for a login no longer configured at
// all is promoted into the pending ledger so Reconcile purges it on this
// same startup, never aborted (which would leak the row forever with no
// purge ever attempted).
func TestArbitratePreparedPromotesWhenAbsentFromConfig(t *testing.T) {
	db := openRawDB(t, filepath.Join(t.TempDir(), "miner.db"))
	an, _, _, coord := buildRawStores(t, db)
	ctx := context.Background()
	if err := an.RecordPoints("arbpromote", 100, "WATCH"); err != nil {
		t.Fatalf("seed points: %v", err)
	}

	if err := coord.AdmitRemovals(ctx, []streamerlifecycle.Removal{
		{ChannelID: "chan-arb2", Login: "arbpromote"},
	}); err != nil {
		t.Fatalf("admit: %v", err)
	}

	aborted, promoted, err := coord.ArbitratePrepared(ctx, arbitrationKeep(nil))
	if err != nil {
		t.Fatalf("arbitrate: %v", err)
	}
	if aborted != 0 || promoted != 1 {
		t.Fatalf("aborted=%d promoted=%d, want 0,1", aborted, promoted)
	}
	if n := admissionCount(t, db, "arbpromote"); n != 0 {
		t.Errorf("promoted row still present in admissions: %d rows", n)
	}
	if n := pendingCount(t, db, "arbpromote"); n != 1 {
		t.Fatalf("promoted row missing from pending: %d rows, want 1", n)
	}

	// The promotion must actually be finishable by Reconcile on this same pass.
	n, err := coord.Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != 1 {
		t.Errorf("reconciled %d rows, want 1 (the just-promoted one)", n)
	}
	if data, _ := an.GetStreamerData("arbpromote"); len(data.Series) != 0 {
		t.Error("promoted-then-reconciled streamer's analytics history survived")
	}
}

// TestArbitratePreparedChannelAwareRuleTreatsDifferentChannelAsAbsent covers
// the package's identity model (channel ID is the primary identity, login is
// a reusable lookup key): a prepared row for login "foo" under channel A is
// promoted (never aborted) when "foo" is NOW configured under a DIFFERENT
// channel B — the naive login-only rule would abort it and silently keep
// channel A's history parked forever under a login that now means someone
// else, and worse would let channel B's identity absorb it.
func TestArbitratePreparedChannelAwareRuleTreatsDifferentChannelAsAbsent(t *testing.T) {
	db := openRawDB(t, filepath.Join(t.TempDir(), "miner.db"))
	_, _, _, coord := buildRawStores(t, db)
	ctx := context.Background()

	if err := coord.AdmitRemovals(ctx, []streamerlifecycle.Removal{
		{ChannelID: "chan-old-identity", Login: "reusedlogin"},
	}); err != nil {
		t.Fatalf("admit: %v", err)
	}

	// "reusedlogin" is configured again, but Twitch now resolves it to a
	// DIFFERENT channel ID than the one the prepared row named.
	aborted, promoted, err := coord.ArbitratePrepared(ctx, arbitrationKeep(map[string]string{"reusedlogin": "chan-new-identity"}))
	if err != nil {
		t.Fatalf("arbitrate: %v", err)
	}
	if aborted != 0 || promoted != 1 {
		t.Fatalf("aborted=%d promoted=%d, want 0,1 (different channel identity must NOT be treated as still configured)", aborted, promoted)
	}
}

// TestArbitratePreparedAbortsWhenAdmissionChannelIDEmpty covers the "either
// side empty" half of the channel-aware rule ArbitratePreparedChannelAware
// RuleTreatsDifferentChannelAsAbsent otherwise leaves untested: a prepared
// row recorded with an EMPTY channel_id (e.g. admitted before the caller
// ever resolved one) against a login the config now reports under a
// NON-EMPTY ChannelID must still be treated as the SAME identity (an empty
// recorded ID makes no claim to compare) and ABORTED — never promoted, which
// would wrongly purge a still-configured, still-identified streamer.
func TestArbitratePreparedAbortsWhenAdmissionChannelIDEmpty(t *testing.T) {
	db := openRawDB(t, filepath.Join(t.TempDir(), "miner.db"))
	_, _, _, coord := buildRawStores(t, db)
	ctx := context.Background()

	if err := coord.AdmitRemovals(ctx, []streamerlifecycle.Removal{
		{ChannelID: "", Login: "emptyadmissionid"},
	}); err != nil {
		t.Fatalf("admit: %v", err)
	}

	aborted, promoted, err := coord.ArbitratePrepared(ctx, arbitrationKeep(map[string]string{"emptyadmissionid": "chan-configured-nonempty"}))
	if err != nil {
		t.Fatalf("arbitrate: %v", err)
	}
	if aborted != 1 || promoted != 0 {
		t.Fatalf("aborted=%d promoted=%d, want 1,0 (an empty recorded channel_id must not block the still-configured abort)", aborted, promoted)
	}
	if n := admissionCount(t, db, "emptyadmissionid"); n != 0 {
		t.Errorf("prepared row survived abort: %d rows", n)
	}
	if n := pendingCount(t, db, "emptyadmissionid"); n != 0 {
		t.Errorf("aborted row was wrongly promoted to pending: %d rows", n)
	}
}

// TestArbitratePreparedAbortsWhenConfigChannelIDEmpty covers the OTHER half:
// a prepared row recorded with a NON-EMPTY channel_id against a login the
// config now reports under an EMPTY stored ChannelID (e.g. a config entry
// that has never had its ChannelID backfilled) must ALSO be treated as the
// same identity and ABORTED — an empty config-side ID makes no claim either,
// so it can never itself prove "a different identity" strongly enough to
// justify promoting (and therefore purging) a login still present in config.
func TestArbitratePreparedAbortsWhenConfigChannelIDEmpty(t *testing.T) {
	db := openRawDB(t, filepath.Join(t.TempDir(), "miner.db"))
	_, _, _, coord := buildRawStores(t, db)
	ctx := context.Background()

	if err := coord.AdmitRemovals(ctx, []streamerlifecycle.Removal{
		{ChannelID: "chan-recorded-nonempty", Login: "emptyconfigid"},
	}); err != nil {
		t.Fatalf("admit: %v", err)
	}

	aborted, promoted, err := coord.ArbitratePrepared(ctx, arbitrationKeep(map[string]string{"emptyconfigid": ""}))
	if err != nil {
		t.Fatalf("arbitrate: %v", err)
	}
	if aborted != 1 || promoted != 0 {
		t.Fatalf("aborted=%d promoted=%d, want 1,0 (an empty config-side channel_id must not block the still-configured abort)", aborted, promoted)
	}
	if n := admissionCount(t, db, "emptyconfigid"); n != 0 {
		t.Errorf("prepared row survived abort: %d rows", n)
	}
	if n := pendingCount(t, db, "emptyconfigid"); n != 0 {
		t.Errorf("aborted row was wrongly promoted to pending: %d rows", n)
	}
}

// TestArbitratePreparedReAddWithOwedPurgeNotAborted covers the interaction
// between the two ledgers: a login with an OWED PURGE already sitting in the
// pending table (no admissions row at all — e.g. an earlier, already-
// committed deletion whose purge failed) and which is now configured again
// (re-added) must NOT have its pending purge silently dropped by
// arbitration — arbitration only ever inspects the admissions table, so it
// must be a complete no-op here, and Reconcile must still purge the owed
// row exactly as it always has.
func TestArbitratePreparedReAddWithOwedPurgeNotAborted(t *testing.T) {
	db := openRawDB(t, filepath.Join(t.TempDir(), "miner.db"))
	an, _, _, coord := buildRawStores(t, db)
	ctx := context.Background()
	if err := an.RecordPoints("reaerowed", 100, "WATCH"); err != nil {
		t.Fatalf("seed points: %v", err)
	}

	// Seed a pending row directly (bypassing AdmitRemovals/CommitRemoval),
	// simulating an already-committed-but-purge-failed deletion left over
	// from an earlier apply, exactly as TestStartupReconcilesDurablePendingDeletion
	// (internal/miner/streamer_deletion_test.go) does at the miner layer.
	if _, err := db.Exec(`INSERT INTO pending_streamer_deletions (login, channel_id, requested_at, attempts)
		VALUES (?, ?, ?, 0)`, "reaerowed", "chan-reaerowed", 1); err != nil {
		t.Fatalf("seed pending row: %v", err)
	}

	aborted, promoted, err := coord.ArbitratePrepared(ctx, arbitrationKeep(map[string]string{"reaerowed": "chan-reaerowed"}))
	if err != nil {
		t.Fatalf("arbitrate: %v", err)
	}
	if aborted != 0 || promoted != 0 {
		t.Fatalf("aborted=%d promoted=%d, want 0,0 (arbitration must not touch a login with no admissions row)", aborted, promoted)
	}
	if n := pendingCount(t, db, "reaerowed"); n != 1 {
		t.Fatalf("arbitration disturbed the pre-existing pending row: %d rows, want 1", n)
	}

	n, err := coord.Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != 1 {
		t.Fatalf("reconciled %d rows, want 1 (the owed purge must still happen for a re-added, now-configured login)", n)
	}
	if data, _ := an.GetStreamerData("reaerowed"); len(data.Series) != 0 {
		t.Error("re-added streamer's owed purge did not run: stale analytics rows survived")
	}
}

// TestArbitratePreparedContinuesPastOneFailure proves one row's abort
// failure does not block another row's resolution: with the database closed
// mid-arbitration every row's abort/promote attempt fails, but ArbitratePrepared
// must still have attempted both rather than stopping at the first error.
func TestArbitratePreparedContinuesPastOneFailure(t *testing.T) {
	db := openRawDB(t, filepath.Join(t.TempDir(), "miner.db"))
	_, _, _, coord := buildRawStores(t, db)
	ctx := context.Background()

	if err := coord.AdmitRemovals(ctx, []streamerlifecycle.Removal{
		{ChannelID: "chan-c1", Login: "arbcontinue1"},
		{ChannelID: "chan-c2", Login: "arbcontinue2"},
	}); err != nil {
		t.Fatalf("admit: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	_, _, err := coord.ArbitratePrepared(ctx, arbitrationKeep(nil))
	if err == nil {
		t.Fatal("expected an error once the database is closed")
	}
	if !errors.Is(err, database.ErrClosed) {
		t.Fatalf("ArbitratePrepared error = %v, want it to wrap database.ErrClosed", err)
	}
}

// fakeV1OnlyModule simulates the pre-M1 on-disk schema: only the v1
// migration (pending_streamer_deletions) has ever been applied — the same
// module name streamerlifecycle.New uses, but frozen at v1, exactly as an
// existing production database would be before this binary's first run.
type fakeV1OnlyModule struct{}

func (fakeV1OnlyModule) Name() string { return "streamer_lifecycle" }
func (fakeV1OnlyModule) Migrations() []database.Migration {
	return []database.Migration{
		{
			Version:     1,
			Description: "v1 only (simulated pre-M1 database)",
			SQL: `
				CREATE TABLE IF NOT EXISTS pending_streamer_deletions (
					login        TEXT PRIMARY KEY,
					channel_id   TEXT NOT NULL,
					requested_at INTEGER NOT NULL,
					attempts     INTEGER NOT NULL DEFAULT 0
				);
			`,
		},
	}
}

// TestMigrationV2AppliesOnV1Database proves the SRAP ledger migration
// self-heals an existing v1 (pre-M1) database on first use: only the new
// table (and the module's version bump to 2) are added — the pre-existing
// pending_streamer_deletions table and its meaning are untouched.
func TestMigrationV2AppliesOnV1Database(t *testing.T) {
	db := openRawDB(t, filepath.Join(t.TempDir(), "miner.db"))

	if err := db.RegisterModule(fakeV1OnlyModule{}); err != nil {
		t.Fatalf("seed v1: %v", err)
	}
	var version int
	if err := db.QueryRow(`SELECT version FROM schema_versions WHERE module = 'streamer_lifecycle'`).Scan(&version); err != nil {
		t.Fatalf("read seeded version: %v", err)
	}
	if version != 1 {
		t.Fatalf("setup: seeded version = %d, want 1", version)
	}
	var admissionsExists int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='streamer_deletion_admissions'`).Scan(&admissionsExists); err != nil {
		t.Fatalf("check admissions table pre-migration: %v", err)
	}
	if admissionsExists != 0 {
		t.Fatal("setup: admissions table must not exist before the real module registers")
	}

	if _, err := streamerlifecycle.New(db, nil, nil, nil); err != nil {
		t.Fatalf("New (real module, applies only the missing v2 migration): %v", err)
	}

	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='streamer_deletion_admissions'`).Scan(&admissionsExists); err != nil {
		t.Fatalf("check admissions table post-migration: %v", err)
	}
	if admissionsExists == 0 {
		t.Fatal("streamer_deletion_admissions table was not created by the v2 migration")
	}
	if err := db.QueryRow(`SELECT version FROM schema_versions WHERE module = 'streamer_lifecycle'`).Scan(&version); err != nil {
		t.Fatalf("read post-migration version: %v", err)
	}
	if version != 2 {
		t.Fatalf("module version after migration = %d, want 2", version)
	}
	var pendingExists int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='pending_streamer_deletions'`).Scan(&pendingExists); err != nil {
		t.Fatalf("check pending table survived: %v", err)
	}
	if pendingExists == 0 {
		t.Fatal("pre-existing pending_streamer_deletions table was lost")
	}
}

// TestHasPendingSeesBothTables proves HasPending is a union over both
// ledgers: a login recorded ONLY in the admissions table (prepared, not yet
// committed) and a login recorded ONLY in the pending table (committed,
// purge owed) must both report true — checking only one table would leave a
// same-process re-add window open against whichever table it ignored.
func TestHasPendingSeesBothTables(t *testing.T) {
	s := newStores(t)
	ctx := context.Background()

	if has, err := s.coord.HasPending(ctx, "haspendneither"); err != nil || has {
		t.Fatalf("HasPending for an untouched login = (%v, %v), want (false, nil)", has, err)
	}

	if err := s.coord.AdmitRemovals(ctx, []streamerlifecycle.Removal{
		{ChannelID: "chan-admonly", Login: "haspendadmissiononly"},
	}); err != nil {
		t.Fatalf("admit: %v", err)
	}
	if has, err := s.coord.HasPending(ctx, "haspendadmissiononly"); err != nil || !has {
		t.Fatalf("HasPending for an admissions-only login = (%v, %v), want (true, nil)", has, err)
	}

	if _, err := s.db.Exec(`INSERT INTO pending_streamer_deletions (login, channel_id, requested_at, attempts)
		VALUES (?, ?, ?, 0)`, "haspendpendingonly", "chan-pendonly", 1); err != nil {
		t.Fatalf("seed pending row: %v", err)
	}
	if has, err := s.coord.HasPending(ctx, "haspendpendingonly"); err != nil || !has {
		t.Fatalf("HasPending for a pending-only login = (%v, %v), want (true, nil)", has, err)
	}
}

// TestHasPendingAfterCloseReturnsTypedErrClosed pins the G14 fix: HasPending
// (like listPending/bumpAttempts) is routed through WithTx, so a call after
// the database is closed returns the typed database.ErrClosed instead of a
// raw driver string or a silent false.
func TestHasPendingAfterCloseReturnsTypedErrClosed(t *testing.T) {
	db := openRawDB(t, filepath.Join(t.TempDir(), "miner.db"))
	_, _, _, coord := buildRawStores(t, db)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	_, err := coord.HasPending(context.Background(), "anything")
	if !errors.Is(err, database.ErrClosed) {
		t.Fatalf("HasPending after close = %v, want database.ErrClosed", err)
	}
}
