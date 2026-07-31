package app

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/miner"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/runtimeconfig"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/updater"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/web"

	_ "modernc.org/sqlite"
)

// These tests simulate a process RESTART across a self-update: unlike
// build_test.go's testFactories (a fresh, isolated *database.DB per Build
// call, one per independent test), the factories here always reopen the
// SAME on-disk sqlite file, so a durable handoff row written by one Build
// (one "process generation") is actually still there for the NEXT Build to
// consume - exactly like a real restart onto a freshly swapped binary.
//
// version.Version is a package var (internal/version), read by buildWith to
// resolve CurrentVersion/ClassifyBoot; these tests save and restore it and
// deliberately do NOT run in parallel with each other or with anything else
// that reads it.

// handoffRingMarkerSeq gives each test invocation (including reruns under
// `go test -count=N`) distinct from/to version numbers, so assertions against
// the process-wide events ring can find the exact event this run produced
// rather than a stale one from another run. Stepped by 10 (not 1) per call so
// consecutive calls' (from, to) pairs never overlap - e.g. call N's `to`
// substring-matching call N+1's `from` would make one test's assertion see
// an event actually recorded by an EARLIER test.
var handoffRingMarkerSeq atomic.Int64

func nextHandoffVersions() (from, to string) {
	n := 300000 + handoffRingMarkerSeq.Add(1)*10
	return fmt.Sprintf("1.2.%d", n), fmt.Sprintf("1.2.%d", n+1)
}

// restartFactories always reopens the SAME dbPath (simulating the durable
// file surviving a process restart), unlike testFactories/freshDB's
// per-Build t.TempDir() isolation.
func restartFactories(t *testing.T, dbPath string) factories {
	return factories{
		openDB: func(string) (*database.DB, error) {
			sqlDB, err := sql.Open("sqlite", dbPath)
			if err != nil {
				return nil, err
			}
			sqlDB.SetMaxOpenConns(1)
			return &database.DB{DB: sqlDB}, nil
		},
		newAnalytics: func(db *database.DB, _ string, r int) (*analytics.Service, error) {
			return analytics.NewService(db, t.TempDir(), r)
		},
		newWeb:   web.NewServerEarly,
		newMiner: miner.New,
	}
}

// countRecentUpdateSucceeded counts TypeUpdateSucceeded ring events whose
// Detail contains marker.
func countRecentUpdateSucceeded(marker string) int {
	n := 0
	for _, e := range events.Recent(500) {
		if e.Type == events.TypeUpdateSucceeded && strings.Contains(e.Detail, marker) {
			n++
		}
	}
	return n
}

func countRecentUpdateFailed(marker string) int {
	n := 0
	for _, e := range events.Recent(500) {
		if e.Type == events.TypeUpdateFailed && strings.Contains(e.Detail, marker) {
			n++
		}
	}
	return n
}

// Restart simulation, the happy path: a handoff row {from, to, phase
// applying} written by the PREVIOUS generation is consumed by the NEXT
// Build once the running version equals "to" - recording update_succeeded
// exactly once (idempotent across a THIRD boot that finds no row left).
func TestBuildConsumesHandoffAndRecordsUpdateSucceeded(t *testing.T) {
	origVersion := version.Version
	t.Cleanup(func() { version.Version = origVersion })

	dbPath := filepath.Join(t.TempDir(), "miner.db")
	from, to := nextHandoffVersions()
	marker := from + " -> " + to

	// Simulate the PREVIOUS generation: a durable store write directly
	// against dbPath, with no App involved at all (the simplest possible
	// stand-in for "checkAndMaybeUpdate wrote this before restarting").
	seedDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	seedDB.SetMaxOpenConns(1)
	store, err := updater.NewStore(&database.DB{DB: seedDB})
	if err != nil {
		t.Fatalf("updater.NewStore (seed): %v", err)
	}
	if err := store.RecordApplying(context.Background(), from, to, "https://example.test/releases/"+to); err != nil {
		t.Fatalf("seed RecordApplying: %v", err)
	}
	if err := seedDB.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}

	// "Restart onto the new binary": the running version is now `to`.
	version.Version = to

	f := restartFactories(t, dbPath)

	app2, err := buildWith(context.Background(), testConfig(), runtimeconfig.RuntimeConfig{}, f)
	if err != nil {
		t.Fatalf("Build #2: %v", err)
	}
	if got := countRecentUpdateSucceeded(marker); got != 1 {
		t.Errorf("update_succeeded events for %q after Build #2 = %d, want 1", marker, got)
	}
	if err := app2.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown app2: %v", err)
	}

	// The row must have been consumed: a fresh store against the same file
	// finds nothing left.
	checkDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open check db: %v", err)
	}
	checkDB.SetMaxOpenConns(1)
	checkStore := &database.DB{DB: checkDB}
	cs, err := updater.NewStore(checkStore)
	if err != nil {
		t.Fatalf("updater.NewStore (check): %v", err)
	}
	if _, found, err := cs.ConsumeHandoff(context.Background()); err != nil || found {
		t.Fatalf("handoff row still present after Build #2 consumed it: found=%v err=%v", found, err)
	}
	if err := checkDB.Close(); err != nil {
		t.Fatalf("close check db: %v", err)
	}

	// Build #3 (same file, same running version, no new row): no additional
	// update_succeeded event for this marker - idempotent.
	app3, err := buildWith(context.Background(), testConfig(), runtimeconfig.RuntimeConfig{}, restartFactories(t, dbPath))
	if err != nil {
		t.Fatalf("Build #3: %v", err)
	}
	t.Cleanup(func() { _ = app3.Shutdown(context.Background()) })
	if got := countRecentUpdateSucceeded(marker); got != 1 {
		t.Errorf("update_succeeded events for %q after Build #3 = %d, want still 1 (idempotent)", marker, got)
	}
}

// Restart simulation, the "not effective" mismatch: a handoff row reached
// phase 'applied' (the swap reported success), but the process that comes
// back up is still running FromVersion - Build must record update_failed,
// not update_succeeded, and still consume the row.
func TestBuildConsumesHandoffMismatchRecordsUpdateFailed(t *testing.T) {
	origVersion := version.Version
	t.Cleanup(func() { version.Version = origVersion })

	dbPath := filepath.Join(t.TempDir(), "miner.db")
	from, to := nextHandoffVersions()
	marker := from + " -> " + to

	seedDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	seedDB.SetMaxOpenConns(1)
	store, err := updater.NewStore(&database.DB{DB: seedDB})
	if err != nil {
		t.Fatalf("updater.NewStore (seed): %v", err)
	}
	if err := store.RecordApplying(context.Background(), from, to, "https://example.test/releases/"+to); err != nil {
		t.Fatalf("seed RecordApplying: %v", err)
	}
	if err := store.RecordApplied(context.Background(), from, to); err != nil {
		t.Fatalf("seed RecordApplied: %v", err)
	}
	if err := seedDB.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}

	// The restarted process is somehow still running the OLD version: the
	// swap did not take effect.
	version.Version = from

	app, err := buildWith(context.Background(), testConfig(), runtimeconfig.RuntimeConfig{}, restartFactories(t, dbPath))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })

	if got := countRecentUpdateSucceeded(marker); got != 0 {
		t.Errorf("update_succeeded events for %q = %d, want 0 (swap did not take effect)", marker, got)
	}
	if got := countRecentUpdateFailed(marker); got != 1 {
		t.Errorf("update_failed events for %q = %d, want 1", marker, got)
	}

	checkDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open check db: %v", err)
	}
	checkDB.SetMaxOpenConns(1)
	cs, err := updater.NewStore(&database.DB{DB: checkDB})
	if err != nil {
		t.Fatalf("updater.NewStore (check): %v", err)
	}
	if _, found, err := cs.ConsumeHandoff(context.Background()); err != nil || found {
		t.Fatalf("handoff row still present after mismatch Build consumed it: found=%v err=%v", found, err)
	}
	if err := checkDB.Close(); err != nil {
		t.Fatalf("close check db: %v", err)
	}
}

// A boot with no pending handoff row (the overwhelming common case) must
// build successfully and record nothing - Build's boot-consumption step is
// best-effort and silent on the "nothing to consume" path. version.Version is
// set to a marker unique to this test run (a fresh, never-before-seen
// version string), so a subsequent absence of any update_succeeded/
// update_failed event carrying that marker actually pins "nothing was
// recorded", rather than merely "the process didn't crash".
func TestBuildNoHandoffRowIsSilentAndSucceeds(t *testing.T) {
	origVersion := version.Version
	t.Cleanup(func() { version.Version = origVersion })

	marker, _ := nextHandoffVersions()
	version.Version = marker

	app, err := buildWith(context.Background(), testConfig(), runtimeconfig.RuntimeConfig{}, testFactories(t, nil))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })

	if got := countRecentUpdateSucceeded(marker); got != 0 {
		t.Errorf("update_succeeded events mentioning %q = %d, want 0 (no handoff row was ever written)", marker, got)
	}
	if got := countRecentUpdateFailed(marker); got != 0 {
		t.Errorf("update_failed events mentioning %q = %d, want 0 (no handoff row was ever written)", marker, got)
	}
}
