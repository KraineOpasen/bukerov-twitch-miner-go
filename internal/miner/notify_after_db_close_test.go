package miner

import (
	"path/filepath"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/notifications"
)

// MINOR 17 (F4b Q3 consolidated corrective), design v6 §7/§12 test 2
// extension: "requirement to the notify-path: a call after the database is
// closed must not panic (repository returns a driver error internally,
// manager after Stop is a no-op)". The updater-join-before-db-close
// ORDERING itself is already covered at the App/controller level
// (internal/app's own lifecycle tests); this test isolates the notify path
// itself, using ONLY internal/notifications' public API (NewManager,
// NotifyUpdateAvailable, NotifyUpdateFailed) plus a raw, private
// *database.DB handle (openRawMinerDB, srap_test.go — the SAME
// non-singleton-database technique this package's own tests already use
// for "deliberately close a handle without breaking every other test in
// this binary", built entirely from database.DB's exported DB field, no
// unexported internals touched).
//
// Discord is configured "enabled" with a bot token that is never dialed
// (NewDiscordProvider does no network I/O at construction — see
// discord.go) specifically so the call reaches the repository's raw
// db.QueryRow/Exec calls (repository.go bypasses the WithTx closed-guard in
// several places — design v6 F11) instead of early-returning on Discord
// being disabled. Go's database/sql package is documented to return an
// error from a closed *sql.DB, never panic, so the repository surfacing
// that as a plain error (logged, not propagated) is exactly what this
// guards against regressing.
func TestNotifyAfterDBCloseDoesNotPanic(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "notify-after-close.db")
	db := openRawMinerDB(t, dbPath)

	mgr, err := notifications.NewManager(
		&config.DiscordSettings{Enabled: true, BotToken: "fake-token-never-dialed", GuildID: "fake-guild"},
		nil,
		db,
		nil,
		"testuser",
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("db.Close(): %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a Notify call panicked after the database was closed: %v", r)
		}
	}()

	mgr.NotifyUpdateAvailable("v1.0.0", "v1.1.0", "https://example.test/release")
	mgr.NotifyUpdateFailed("v1.0.0", "v1.1.0", "checksum mismatch")
}

// The symmetric case named in the same §7 requirement: a Stop()'d manager
// (dispatch admission closed) must ALSO be a safe no-op after the database
// is closed — belt-and-suspenders against the two conditions (stopped,
// db-closed) ever combining into a panic.
func TestNotifyAfterStopAndDBCloseDoesNotPanic(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "notify-after-stop-and-close.db")
	db := openRawMinerDB(t, dbPath)

	mgr, err := notifications.NewManager(
		&config.DiscordSettings{Enabled: true, BotToken: "fake-token-never-dialed", GuildID: "fake-guild"},
		nil,
		db,
		nil,
		"testuser",
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	if err := mgr.Stop(); err != nil {
		t.Fatalf("mgr.Stop(): %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close(): %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a Notify call panicked after Stop + database close: %v", r)
		}
	}()

	mgr.NotifyUpdateAvailable("v1.0.0", "v1.1.0", "https://example.test/release")
	mgr.NotifyUpdateFailed("v1.0.0", "v1.1.0", "checksum mismatch")
}
