package streamerlifecycle_test

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamerlifecycle"
)

// recordingPurger implements both the login-only and the identity-aware purge
// contracts, so a test can see WHICH one the coordinator chose and with what
// identity — the point being that a store that can key by channel must be
// handed the ledger's proven channel id, not just the login.
type recordingPurger struct {
	mu       sync.Mutex
	loginTx  []string
	identity [][2]string
}

func (r *recordingPurger) DeleteStreamerTx(tx *sql.Tx, login string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loginTx = append(r.loginTx, login)
	return false, nil
}

func (r *recordingPurger) DeleteStreamerIdentityTx(tx *sql.Tx, channelID, login string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.identity = append(r.identity, [2]string{channelID, login})
	return false, nil
}

func (r *recordingPurger) snapshot() ([]string, [][2]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.loginTx...), append([][2]string(nil), r.identity...)
}

// loginOnlyPurger implements ONLY the original contract, proving a store that
// cannot key by channel keeps the exact behaviour it always had.
type loginOnlyPurger struct {
	mu     sync.Mutex
	logins []string
}

func (l *loginOnlyPurger) DeleteStreamerTx(tx *sql.Tx, login string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.logins = append(l.logins, login)
	return false, nil
}

func (l *loginOnlyPurger) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.logins...)
}

// recordingFencer records tombstones and identity invalidations, and the ORDER
// they happened in relative to the purge.
type recordingFencer struct {
	mu          sync.Mutex
	events      []string
	purgeMarker func() string
}

func (f *recordingFencer) Tombstone(login string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "tombstone:"+login)
}

func (f *recordingFencer) Reinstate(login string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "reinstate:"+login)
}

func (f *recordingFencer) InvalidateIdentity(channelID, login string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "invalidate:"+channelID+"/"+login)
}

func (f *recordingFencer) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

// countingPriority is the outside-transaction priority hook. It records how
// many times it was claimed and asserts every claim is released.
type countingPriority struct {
	claims   atomic.Int64
	released atomic.Int64
	inFlight atomic.Int64
	maxDepth atomic.Int64
}

func (c *countingPriority) Claim() func() {
	c.claims.Add(1)
	depth := c.inFlight.Add(1)
	for {
		max := c.maxDepth.Load()
		if depth <= max || c.maxDepth.CompareAndSwap(max, depth) {
			break
		}
	}
	return func() {
		c.inFlight.Add(-1)
		c.released.Add(1)
	}
}

func newIdentityCoordinator(t *testing.T, purgers []streamerlifecycle.Purger, fencers []streamerlifecycle.Fencer, priority streamerlifecycle.TxPriority) (*streamerlifecycle.Coordinator, *database.DB) {
	t.Helper()
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	coord, err := streamerlifecycle.NewWithPriority(db, purgers, fencers, nil, priority)
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	return coord, db
}

// TestIdentityPurgerReceivesTheLedgerProvenChannel proves the whole point of
// the identity plumbing: a store that can key by channel is handed the
// ledger's OWN channel id, end to end, through the committed-removal path.
func TestIdentityPurgerReceivesTheLedgerProvenChannel(t *testing.T) {
	idp := &recordingPurger{}
	lop := &loginOnlyPurger{}
	fencer := &recordingFencer{}
	coord, _ := newIdentityCoordinator(t,
		[]streamerlifecycle.Purger{idp, lop},
		[]streamerlifecycle.Fencer{fencer}, nil)

	ctx := context.Background()
	if _, err := coord.Delete(ctx, "chan-777", "identity-user"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	loginCalls, identityCalls := idp.snapshot()
	if len(identityCalls) != 1 || identityCalls[0] != [2]string{"chan-777", "identity-user"} {
		t.Fatalf("identity purger got %v, want one call with the proven (chan-777, identity-user)", identityCalls)
	}
	if len(loginCalls) != 0 {
		t.Fatalf("identity purger ALSO got the login-only call %v; it must be called exactly once", loginCalls)
	}
	// A store that cannot key by channel keeps its original contract exactly.
	if got := lop.snapshot(); len(got) != 1 || got[0] != "identity-user" {
		t.Fatalf("login-only purger got %v, want [identity-user]", got)
	}
}

// TestIdentityFenceRunsBeforeThePurge proves in-flight work is invalidated
// OUTSIDE and strictly BEFORE the purge transaction, so nothing already
// accepted for the identity can commit after the erasure.
func TestIdentityFenceRunsBeforeThePurge(t *testing.T) {
	var order []string
	var mu sync.Mutex
	note := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }

	fencer := &recordingFencer{}
	purger := purgeFunc(func(tx *sql.Tx, channelID, login string) (bool, error) {
		note("purge")
		return false, nil
	})
	coord, _ := newIdentityCoordinator(t,
		[]streamerlifecycle.Purger{purger},
		[]streamerlifecycle.Fencer{fencerNoting{fencer, note}}, nil)

	if _, err := coord.Delete(context.Background(), "chan-1", "fenced"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := append([]string(nil), order...)
	mu.Unlock()

	invalidateAt, purgeAt := -1, -1
	for i, s := range got {
		if s == "invalidate" && invalidateAt < 0 {
			invalidateAt = i
		}
		if s == "purge" && purgeAt < 0 {
			purgeAt = i
		}
	}
	if invalidateAt < 0 || purgeAt < 0 || invalidateAt > purgeAt {
		t.Fatalf("order = %v, want the identity invalidation strictly before the purge", got)
	}
	events := fencer.snapshot()
	found := false
	for _, e := range events {
		if e == "invalidate:chan-1/fenced" {
			found = true
		}
	}
	if !found {
		t.Fatalf("fencer events = %v, want the proven identity", events)
	}
}

// purgeFunc adapts a func to both purge contracts.
type purgeFunc func(tx *sql.Tx, channelID, login string) (bool, error)

func (f purgeFunc) DeleteStreamerTx(tx *sql.Tx, login string) (bool, error) {
	return f(tx, "", login)
}
func (f purgeFunc) DeleteStreamerIdentityTx(tx *sql.Tx, channelID, login string) (bool, error) {
	return f(tx, channelID, login)
}

// fencerNoting wraps a fencer so the ordering test can see when the identity
// invalidation happened relative to the purge.
type fencerNoting struct {
	inner *recordingFencer
	note  func(string)
}

func (f fencerNoting) Tombstone(login string) { f.inner.Tombstone(login) }
func (f fencerNoting) Reinstate(login string) { f.inner.Reinstate(login) }
func (f fencerNoting) InvalidateIdentity(channelID, login string) {
	f.note("invalidate")
	f.inner.InvalidateIdentity(channelID, login)
}

// TestReconcileLoginCarriesTheLedgerChannel is the proof that ReconcileLogin
// was NOT collapsed to a boolean: a re-added login whose purge is still owed
// resolves the ledger's own channel id and hands it to the identity purger.
func TestReconcileLoginCarriesTheLedgerChannel(t *testing.T) {
	idp := &recordingPurger{}
	coord, db := newIdentityCoordinator(t,
		[]streamerlifecycle.Purger{idp},
		[]streamerlifecycle.Fencer{&recordingFencer{}}, nil)
	ctx := context.Background()

	// Leave a pending purge behind, exactly as an unclean shutdown would.
	if _, err := db.Exec(`INSERT INTO pending_streamer_deletions (login, channel_id, requested_at, attempts)
		VALUES ('readded', 'chan-ledger', 1, 0)`); err != nil {
		t.Fatal(err)
	}
	had, err := coord.ReconcileLogin(ctx, "readded")
	if err != nil || !had {
		t.Fatalf("ReconcileLogin = %v, %v; want (true, nil)", had, err)
	}
	_, identityCalls := idp.snapshot()
	if len(identityCalls) != 1 || identityCalls[0] != [2]string{"chan-ledger", "readded"} {
		t.Fatalf("identity purger got %v, want the ledger-proven (chan-ledger, readded)", identityCalls)
	}
}

// TestReconcileCarriesTheLedgerChannel proves the same for the startup sweep.
func TestReconcileCarriesTheLedgerChannel(t *testing.T) {
	idp := &recordingPurger{}
	coord, db := newIdentityCoordinator(t,
		[]streamerlifecycle.Purger{idp},
		[]streamerlifecycle.Fencer{&recordingFencer{}}, nil)

	if _, err := db.Exec(`INSERT INTO pending_streamer_deletions (login, channel_id, requested_at, attempts)
		VALUES ('swept', 'chan-swept', 1, 0)`); err != nil {
		t.Fatal(err)
	}
	// This package shares the process-wide database singleton, so the sweep
	// may legitimately also reconcile rows other tests left behind. Assert on
	// OUR row's identity, not on a global count.
	if n, err := coord.Reconcile(context.Background()); err != nil || n < 1 {
		t.Fatalf("Reconcile = %d, %v; want at least our one row and no error", n, err)
	}
	_, identityCalls := idp.snapshot()
	found := false
	for _, call := range identityCalls {
		if call == [2]string{"chan-swept", "swept"} {
			found = true
		}
		if call[1] == "swept" && call[0] != "chan-swept" {
			t.Fatalf("the sweep purged %q under channel %q, want chan-swept", call[1], call[0])
		}
	}
	if !found {
		t.Fatalf("identity purger got %v, want a call with (chan-swept, swept)", identityCalls)
	}
}

// TestHasPendingContractUnchanged proves folding HasPending onto the identity
// lookup preserved its exact behaviour across BOTH ledgers.
func TestHasPendingContractUnchanged(t *testing.T) {
	coord, db := newIdentityCoordinator(t, nil, nil, nil)
	ctx := context.Background()

	if has, err := coord.HasPending(ctx, "nobody"); err != nil || has {
		t.Fatalf("HasPending(unknown) = %v, %v; want (false, nil)", has, err)
	}
	if has, err := coord.HasPending(ctx, ""); err != nil || has {
		t.Fatalf("HasPending(empty) = %v, %v; want (false, nil)", has, err)
	}
	if _, err := db.Exec(`INSERT INTO streamer_deletion_admissions (login, channel_id, requested_at)
		VALUES ('prepared', 'chan-p', 1)`); err != nil {
		t.Fatal(err)
	}
	if has, err := coord.HasPending(ctx, "PREPARED"); err != nil || !has {
		t.Fatalf("HasPending(prepared, uppercased) = %v, %v; want (true, nil)", has, err)
	}
	if _, err := db.Exec(`INSERT INTO pending_streamer_deletions (login, channel_id, requested_at, attempts)
		VALUES ('owed', 'chan-o', 1, 0)`); err != nil {
		t.Fatal(err)
	}
	if has, err := coord.HasPending(ctx, "owed"); err != nil || !has {
		t.Fatalf("HasPending(owed) = %v, %v; want (true, nil)", has, err)
	}
}

// TestPendingRowWinsOverAdmissionRow proves the identity lookup prefers the
// pending-purge ledger — the record of a removal already committed — when both
// tables hold a row for the login.
func TestPendingRowWinsOverAdmissionRow(t *testing.T) {
	idp := &recordingPurger{}
	coord, db := newIdentityCoordinator(t,
		[]streamerlifecycle.Purger{idp},
		[]streamerlifecycle.Fencer{&recordingFencer{}}, nil)

	if _, err := db.Exec(`INSERT INTO streamer_deletion_admissions (login, channel_id, requested_at)
		VALUES ('both', 'chan-prepared', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO pending_streamer_deletions (login, channel_id, requested_at, attempts)
		VALUES ('both', 'chan-committed', 1, 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.ReconcileLogin(context.Background(), "both"); err != nil {
		t.Fatal(err)
	}
	_, identityCalls := idp.snapshot()
	if len(identityCalls) != 1 || identityCalls[0][0] != "chan-committed" {
		t.Fatalf("identity purger got %v, want the committed removal's channel", identityCalls)
	}
}

// TestTxPriorityWrapsEveryTransactionExactlyOnce proves the hook is claimed
// around every coordinator transaction, always released, and NEVER nested —
// a nested claim would mean a claim was taken inside a transaction.
func TestTxPriorityWrapsEveryTransactionExactlyOnce(t *testing.T) {
	priority := &countingPriority{}
	idp := &recordingPurger{}
	coord, db := newIdentityCoordinator(t,
		[]streamerlifecycle.Purger{idp},
		[]streamerlifecycle.Fencer{&recordingFencer{}}, priority)

	// The constructor's own RegisterModule is covered too.
	if priority.claims.Load() < 1 {
		t.Fatal("the constructor's RegisterModule was not covered by the priority hook")
	}
	ctx := context.Background()
	before := priority.claims.Load()

	if err := coord.AdmitRemovals(ctx, []streamerlifecycle.Removal{{ChannelID: "c", Login: "l"}}); err != nil {
		t.Fatal(err)
	}
	if err := coord.AbortAdmission(ctx, []string{"l"}); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.Delete(ctx, "c", "l"); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.HasPending(ctx, "l"); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := coord.ArbitratePrepared(ctx, func(string, string) (bool, string) { return false, "" }); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.ReconcileLogin(ctx, "l"); err != nil {
		t.Fatal(err)
	}

	if got := priority.claims.Load(); got <= before {
		t.Fatalf("claims did not advance past %d (now %d)", before, got)
	}
	if c, r := priority.claims.Load(), priority.released.Load(); c != r {
		t.Fatalf("%d claims but %d releases: a claim leaked", c, r)
	}
	if d := priority.maxDepth.Load(); d != 1 {
		t.Fatalf("max claim depth = %d, want 1 — a nested claim means one was taken inside a transaction", d)
	}
	_ = db
}

// TestCoordinatorHasExactlyNineTransactions is the source invariant. The
// coordinator opens transactions through exactly one wrapper, and that wrapper
// is used at exactly nine sites — the number this package's design fixes.
func TestCoordinatorHasExactlyNineTransactions(t *testing.T) {
	src, err := os.ReadFile("lifecycle.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if n := strings.Count(body, "c.withTx("); n != 9 {
		t.Fatalf("coordinator has %d transaction sites, want exactly 9", n)
	}
	// Exactly one of them is the wrapper's own call to the real primitive.
	if n := strings.Count(body, "c.db.WithTx("); n != 1 {
		t.Fatalf("found %d raw WithTx calls, want exactly 1 (inside the wrapper) — a raw bypass skips the priority hook", n)
	}
	// The claim is taken OUTSIDE the transaction: the wrapper defers the
	// release before it opens one.
	wrapper := body[strings.Index(body, "func (c *Coordinator) withTx("):]
	wrapper = wrapper[:strings.Index(wrapper, "\n}\n")]
	claimAt := strings.Index(wrapper, "c.priority.Claim()")
	txAt := strings.Index(wrapper, "c.db.WithTx(")
	if claimAt < 0 || txAt < 0 || claimAt > txAt {
		t.Fatalf("the priority claim is not taken before the transaction opens:\n%s", wrapper)
	}
}

// TestAnalyticsRepositorySatisfiesTheIdentityPurger proves the real analytics
// repository is what the coordinator will actually route through, end to end,
// erasing observations in the SAME transaction as the rest of the purge.
func TestAnalyticsRepositorySatisfiesTheIdentityPurger(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := analytics.NewSQLiteRepository(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := interface{}(repo).(streamerlifecycle.IdentityPurger); !ok {
		t.Fatal("analytics.SQLiteRepository no longer satisfies streamerlifecycle.IdentityPurger")
	}
	if _, ok := interface{}(repo).(streamerlifecycle.IdentityFencer); !ok {
		t.Fatal("analytics.SQLiteRepository no longer satisfies streamerlifecycle.IdentityFencer")
	}

	coord, err := streamerlifecycle.New(db,
		[]streamerlifecycle.Purger{repo},
		[]streamerlifecycle.Fencer{repo},
		[]streamerlifecycle.Renamer{repo})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// A streamer with both ordinary history and an observation fact.
	if err := repo.RecordPoints("purged-user", 100, "WATCH"); err != nil {
		t.Fatal(err)
	}
	var streamerID int64
	if err := db.QueryRow(`SELECT id FROM streamers WHERE name = ?`, "purged-user").Scan(&streamerID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO prediction_observations
		(observation_id, collector_session_id, collector_epoch, collector_sequence,
		 pool_instance_id, round_incarnation_id, retention_group_owner_channel_id,
		 retention_group_owner_streamer_id, routed_channel_id, routed_streamer_id,
		 kind, producer_time_source, received_at_ms, payload_version, payload_json, observation_sha256)
		VALUES ('o-purge', 's', 1, 1, 'pool', 'round:x', 'chan-purge', ?, 'chan-purge', ?,
		        'channel_event', 'RECEIVER', 1, 1, '{"phase":"ROUND_CREATED"}', 'sha256:x')`,
		streamerID, streamerID); err != nil {
		t.Fatal(err)
	}
	// A bystander channel's fact that must survive.
	if _, err := db.Exec(`INSERT INTO prediction_observations
		(observation_id, collector_session_id, collector_epoch, collector_sequence,
		 pool_instance_id, routed_channel_id, kind, producer_time_source, received_at_ms,
		 payload_version, payload_json, observation_sha256)
		VALUES ('o-keep', 's', 1, 2, 'pool', 'chan-other',
		        'channel_event', 'RECEIVER', 1, 1, '{"phase":"ROUND_CREATED"}', 'sha256:x')`); err != nil {
		t.Fatal(err)
	}

	if _, err := coord.Delete(ctx, "chan-purge", "purged-user"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	var left int
	if err := db.QueryRow(`SELECT COUNT(*) FROM prediction_observations WHERE routed_channel_id = 'chan-purge'`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("%d observation facts survived the identity purge", left)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM prediction_observations WHERE routed_channel_id = 'chan-other'`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 1 {
		t.Fatal("the purge removed a bystander channel's observation fact")
	}
	// And the ordinary history is gone too, in the same transaction.
	var streamers int
	if err := db.QueryRow(`SELECT COUNT(*) FROM streamers WHERE name = ?`, "purged-user").Scan(&streamers); err != nil {
		t.Fatal(err)
	}
	if streamers != 0 {
		t.Fatal("the streamers row survived the purge")
	}
}

// TestRenamePerformsZeroUpdatesOnObservations proves a rename never mutates an
// immutable fact: it moves the `streamers` row and leaves every observation
// byte-for-byte where it was.
func TestRenamePerformsZeroUpdatesOnObservations(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := analytics.NewSQLiteRepository(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	coord, err := streamerlifecycle.New(db,
		[]streamerlifecycle.Purger{repo},
		[]streamerlifecycle.Fencer{repo},
		[]streamerlifecycle.Renamer{repo})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.RecordPoints("oldlogin", 100, "WATCH"); err != nil {
		t.Fatal(err)
	}
	var streamerID int64
	if err := db.QueryRow(`SELECT id FROM streamers WHERE name = ?`, "oldlogin").Scan(&streamerID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO prediction_observations
		(observation_id, collector_session_id, collector_epoch, collector_sequence,
		 pool_instance_id, routed_channel_id, routed_streamer_id,
		 kind, producer_time_source, received_at_ms, payload_version, payload_json, observation_sha256)
		VALUES ('o-rename', 's', 1, 1, 'pool', 'chan-rename', ?,
		        'channel_event', 'RECEIVER', 1, 1, '{"phase":"ROUND_CREATED"}', 'sha256:frozen')`,
		streamerID); err != nil {
		t.Fatal(err)
	}
	before := dumpObservation(t, db, "o-rename")

	if err := coord.RenameStreamer("oldlogin", "newlogin"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	var name string
	if err := db.QueryRow(`SELECT name FROM streamers WHERE id = ?`, streamerID).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "newlogin" {
		t.Fatalf("streamers row = %q, want newlogin", name)
	}
	if after := dumpObservation(t, db, "o-rename"); after != before {
		t.Fatalf("the rename mutated an immutable observation:\nbefore=%s\n after=%s", before, after)
	}
}

func dumpObservation(t *testing.T, db *database.DB, observationID string) string {
	t.Helper()
	var epoch, seq, received int64
	var parent sql.NullInt64
	var channel, kind, payload, digest string
	if err := db.QueryRow(`SELECT collector_epoch, collector_sequence, routed_streamer_id,
		COALESCE(routed_channel_id,''), kind, received_at_ms, payload_json, observation_sha256
		FROM prediction_observations WHERE observation_id = ?`, observationID).
		Scan(&epoch, &seq, &parent, &channel, &kind, &received, &payload, &digest); err != nil {
		t.Fatal(err)
	}
	return strings.Join([]string{
		observationID, itoa64(epoch), itoa64(seq), itoa64(parent.Int64), channel, kind,
		itoa64(received), payload, digest,
	}, "|")
}

func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
