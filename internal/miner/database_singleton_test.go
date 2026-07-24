package miner

import (
	"os"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
)

// TestMain opens the process-wide DB singleton (database.Open) against a
// durable, whole-package-lifetime directory before any test runs — mirroring
// internal/analytics/history_test.go's TestMain, which documents the exact
// same underlying constraint: database.Open is a process-global singleton (by
// design — see internal/database/database.go), so if the FIRST test in this
// binary to call it used a per-test t.TempDir(), that directory would be
// removed at THAT test's end and leave the shared handle pointing at deleted
// files for every later test in the package ("attempt to write a readonly
// database"). Several tests in this package (BKM-006 Corrective Pass 1's C2
// coordinator tests among them) exercise a REAL analytics.Service/SQLite path
// and each pass their own t.TempDir() to database.Open; opening the singleton
// once here, first, against a directory that outlives every individual test
// makes those calls resolve to the SAME durable handle instead of racing
// their own temp dirs' cleanup.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "miner-test-*")
	if err != nil {
		panic(err)
	}
	if _, err := database.Open(dir); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
