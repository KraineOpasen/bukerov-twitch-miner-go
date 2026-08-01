package miner

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/drops"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
)

// TestSetupComponentsWiresDropSkipLedger (S1): a normal, successful startup
// with a real database registers the drop_skip_ledger module (schema_versions
// carries it at version 1) -- the same guarded, fail-open block shape as the
// pre-existing drop-campaign catalog (drop_catalog) immediately above it in
// setupComponents (see miner.go). setupComponents itself returns no error, so
// a DB-less startup (the `if m.db != nil` guard, identical in shape to the
// catalog's own) can never prevent the miner from starting either way -- this
// test only needs to prove the SUCCESS path actually wires the module during
// a real Run(), mirroring how the catalog's identical init block has no
// dedicated failure-injection test either (RegisterModule's own error
// handling is covered by internal/database's tests, which this contract
// treats as frozen).
//
// The schema_versions row alone is NOT sufficient: drops.NewSkipLedger
// registers the module as a side effect of construction, so that row would
// still land even if the very next line -- m.dropsTracker.SetSkipLedger(ledger)
// -- were silently deleted from miner.go, which would leave ghost-skip
// permanently disabled in production with the whole test suite still green.
// The m.dropsTracker.SkipLedgerEnabled() assertion below is what actually
// catches that regression: it can only be true if SetSkipLedger was called
// with a non-nil ledger.
func TestSetupComponentsWiresDropSkipLedger(t *testing.T) {
	m, db := newStartupCleanupMiner(t)
	runToNormalCompletion(t, m)

	// For symmetry with the two failure-path tests below (which both call
	// this): the success path must reach the end of setupComponents too, not
	// just wire the ledger and stop early. Under the bare-return-after-the-
	// skip-ledger-block mutant this test stayed green on its own (nothing
	// below depended on setup continuing past that point); only the failure
	// tests caught it.
	requireComponentSetupCompleted(t, m)

	var version int
	if err := db.QueryRow(`SELECT version FROM schema_versions WHERE module = ?`, "drop_skip_ledger").Scan(&version); err != nil {
		t.Fatalf("drop_skip_ledger module was not registered during startup: %v", err)
	}
	if version != 1 {
		t.Errorf("drop_skip_ledger schema version = %d, want 1", version)
	}
	if !m.dropsTracker.SkipLedgerEnabled() {
		t.Fatal("m.dropsTracker.SetSkipLedger was not called during startup -- ghost-skip is silently disabled")
	}
	requireExternalDBAlive(t, db)
}

// runToNormalCompletion drives m.Run to a successful, normally-cancelled
// completion via the SAME offline-stub + channel-synchronized start/cancel
// dance TestSetupComponentsWiresDropSkipLedger originally inlined (no sleep
// ordering anywhere): stub authenticate/loadStreamers/subscribeTopics so the
// whole sequence runs offline, let setupComponents run for real (unstubbed),
// signal on startMiningFn, then cancel and wait for Run to return. Shared by
// every test in this file that needs a real, completed startup pass.
func runToNormalCompletion(t *testing.T, m *Miner) {
	t.Helper()
	stubAuthenticate(m)
	stubLoadStreamers(m)
	m.subscribeTopicsFn = func() error { return nil }

	started := make(chan struct{})
	m.startMiningFn = func(ctx context.Context) { close(started) }

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- m.Run(ctx) }()

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("startup did not complete")
	}
	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run after normal cancellation = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

// requireComponentSetupCompleted asserts that setupComponents ran all the way
// through to its end, past the skip-ledger block, rather than bailing out
// early from inside (or immediately after) it. m.discovery, m.healthCenter,
// m.canary, m.avoidList and m.progressWatchdog are all constructed in
// setupComponents STRICTLY AFTER the skip-ledger block (see miner.go) and are
// never touched anywhere else before that point, so every one of them
// staying nil is exactly the signature of "a skip-ledger init failure
// aborted the rest of component wiring" rather than failing open past it.
// Checking five independently-constructed fields (not just one) means this
// cannot be satisfied by accident -- a mutant that returns early right after
// the skip-ledger block, or anywhere before these are set, leaves ALL five
// nil, and a mutant that returns early somewhere later would still be caught
// by whichever of these five fields it landed before.
func requireComponentSetupCompleted(t *testing.T, m *Miner) {
	t.Helper()
	if m.discovery == nil {
		t.Error("component setup did not complete: m.discovery is nil (a skip-ledger init failure must not abort setupComponents)")
	}
	if m.healthCenter == nil {
		t.Error("component setup did not complete: m.healthCenter is nil (a skip-ledger init failure must not abort setupComponents)")
	}
	if m.canary == nil {
		t.Error("component setup did not complete: m.canary is nil (a skip-ledger init failure must not abort setupComponents)")
	}
	if m.avoidList == nil {
		t.Error("component setup did not complete: m.avoidList is nil (a skip-ledger init failure must not abort setupComponents)")
	}
	if m.progressWatchdog == nil {
		t.Error("component setup did not complete: m.progressWatchdog is nil (a skip-ledger init failure must not abort setupComponents)")
	}
}

// newStartupCleanupMinerWithDB mirrors newStartupCleanupMiner
// (startup_cleanup_test.go) exactly, except the caller supplies db directly
// instead of letting it resolve through the process-wide database.Open
// singleton every other test in this binary shares. It exists so a test can
// drive a REAL migration/registration failure -- a pre-poisoned, private
// *database.DB (see openRawMinerDB, srap_test.go) -- through setupComponents
// via a genuine Run(), rather than injecting a fabricated error at
// newSkipLedgerFn.
func newStartupCleanupMinerWithDB(t *testing.T, db *database.DB) *Miner {
	t.Helper()
	t.Chdir(t.TempDir())

	cfg := config.DefaultConfig()
	cfg.Username = "startup_cleanup_tester"
	cfg.Streamers = nil
	cfg.EnableAnalytics = false
	cfg.Discord.Enabled = false
	cfg.Debug.Enabled = false

	m := New(&cfg, "")
	m.SetDatabase(db)
	return m
}

// newEventsSince returns only the events recorded after the given "before"
// snapshot (a prior events.Recent(...) call, newest first), by walking the
// current list from the front until it reaches the event that used to be
// newest. A plain length delta does not work here: events.Recent(200) draws
// from a process-wide, fixed-capacity (200) ring that EVERY test in this
// binary shares, so once it fills up -- which it reliably has by the time
// this package's later tests run -- Recent(200) always returns exactly 200
// events regardless of how many new ones were recorded since. And scanning
// the WHOLE current list (rather than just the delta) risks a false pass
// from an unrelated earlier test that happened to record an event of the
// same shape before this test even started.
func newEventsSince(before []events.Event) []events.Event {
	after := events.Recent(200)
	if len(before) == 0 {
		return after
	}
	marker := before[0]
	for i, e := range after {
		if e == marker {
			return after[:i]
		}
	}
	// The marker itself fell off the ring (200+ events recorded since) --
	// every currently retained event is new.
	return after
}

// TestSetupComponentsSkipLedgerMigrationFailureFailsOpen (FIX 2 scenario 1,
// miner-package half): drives a REAL drop_skip_ledger migration/registration
// failure through m.runNewSkipLedger's real drops.NewSkipLedger path -- no
// seam is needed to reach it (see newSkipLedgerFn's own doc comment in
// miner.go). A private, pre-poisoned *database.DB (never the shared
// database.Open singleton -- see newStartupCleanupMinerWithDB) is seeded
// with a conflicting drop_reward_skips table BEFORE Run ever starts, so
// setupComponents hits the genuine SQLite error drops.NewSkipLedger's
// migration produces. This mirrors internal/drops's own
// TestNewSkipLedgerMigrationFailureNoPartialState, reached here through the
// miner's real startup sequence instead of calling drops.NewSkipLedger
// directly.
func TestSetupComponentsSkipLedgerMigrationFailureFailsOpen(t *testing.T) {
	poisonedDB := openRawMinerDB(t, filepath.Join(t.TempDir(), "miner.db"))
	t.Cleanup(func() { _ = poisonedDB.Close() })
	if _, err := poisonedDB.Exec(`CREATE TABLE drop_reward_skips (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("seed conflicting table: %v", err)
	}

	logCap := &captureHandler{}
	prevLog := slog.Default()
	slog.SetDefault(slog.New(logCap))
	defer slog.SetDefault(prevLog)

	eventsBefore := events.Recent(200)

	m := newStartupCleanupMinerWithDB(t, poisonedDB)
	runToNormalCompletion(t, m)

	// The contract's requirement (1): a skip-ledger init failure must not
	// prevent miner startup / component setup. Checked FIRST and separately
	// from the ledger-specific assertions below -- those alone stay true even
	// if setupComponents aborted immediately after the ledger block, so they
	// do not by themselves prove startup continued.
	requireComponentSetupCompleted(t, m)

	// Proves the failure is scoped to only the skip-ledger module: the
	// drop-campaign catalog -- a SEPARATE module registered on this exact
	// SAME poisoned db, immediately before the skip-ledger block in
	// setupComponents -- still initializes normally.
	if m.dropCatalog == nil {
		t.Fatal("expected the drop-campaign catalog (an unrelated module sharing the same poisoned db) to still initialize")
	}

	if m.dropsTracker.SkipLedgerEnabled() {
		t.Fatal("no ledger must be attached after a migration failure")
	}

	newEvents := newEventsSince(eventsBefore)
	found := false
	for _, e := range newEvents {
		if e.Type == events.TypeModuleInitFailed && strings.Contains(e.Detail, "drop_skip_ledger") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a TypeModuleInitFailed event mentioning drop_skip_ledger among %d new events: %+v", len(newEvents), newEvents)
	}

	if !logCap.has("Failed to initialize drop skip ledger; ghost-skip stays DISABLED") {
		t.Error("expected the ghost-skip-disabled ERROR log line")
	}

	requireExternalDBAlive(t, poisonedDB)
}

// TestSetupComponentsSkipLedgerConstructorFailureNoPartialLedgerEscapes (FIX
// 2 scenario 2): injects at m.newSkipLedgerFn a constructor that returns a
// GENUINE, fully-usable *drops.SkipLedger (built via a real, separate
// drops.NewSkipLedger call) ALONGSIDE a non-nil error -- the ONE shape the
// real, unmodified drops.NewSkipLedger can never produce (a real failure
// there always returns a clean (nil, error) -- see
// TestSetupComponentsSkipLedgerMigrationFailureFailsOpen above and
// internal/drops's TestNewSkipLedgerMigrationFailureNoPartialState for what
// a genuine failure actually looks like). This seam exists solely to
// fabricate that unreachable partial-object shape as a defensive guard: it
// proves the miner's error check gates on err, never on ledger != nil --
// SetSkipLedger must never be called with that ledger even though it is a
// real, working object. It also proves the tracker stays fully usable
// afterward -- no partial state anywhere.
func TestSetupComponentsSkipLedgerConstructorFailureNoPartialLedgerEscapes(t *testing.T) {
	scratchDB, err := database.Open(t.TempDir()) // process-wide singleton; see database_singleton_test.go
	if err != nil {
		t.Fatalf("open scratch db: %v", err)
	}
	partial, err := drops.NewSkipLedger(scratchDB, "partial-account")
	if err != nil {
		t.Fatalf("build the genuine-but-should-be-discarded ledger: %v", err)
	}

	sentinel := errors.New("simulated constructor failure (unrelated to migration)")
	m, db := newStartupCleanupMiner(t)
	m.newSkipLedgerFn = func(*database.DB, string) (*drops.SkipLedger, error) {
		return partial, sentinel // non-nil ledger AND non-nil error
	}

	runToNormalCompletion(t, m)

	// The contract's requirement: startup continues past this failure too --
	// see requireComponentSetupCompleted's doc comment.
	requireComponentSetupCompleted(t, m)

	if m.dropsTracker.SkipLedgerEnabled() {
		t.Fatal("a non-nil ledger returned alongside a non-nil error must never be attached (partial-object escape)")
	}

	// The tracker stays genuinely usable afterward: this call must not panic
	// on the discarded-ledger state. It cannot itself distinguish a
	// skip-ledger regression (no campaigns were ever synced in this offline
	// test regardless of ledger state, so a len check here would be
	// tautological the same way the SuppressedDrops() one above was) -- see
	// this file's other tests for the actual ledger-attachment assertions.
	_ = m.dropsTracker.Campaigns()

	requireExternalDBAlive(t, db)
}
