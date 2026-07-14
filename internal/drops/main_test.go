package drops

import (
	"os"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
)

// TestMain opens the process-wide DB singleton against a durable directory so
// catalog tests that need SQLite share one handle whose backing file outlives
// any single test's t.TempDir(). Tests isolate via unique campaign ids.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "drops-test-*")
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
