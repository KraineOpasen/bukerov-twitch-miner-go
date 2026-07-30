package miner

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/notifications"
)

// TestUpdateNotifyFuncsNilManagerIsSafe (design v6 §7 notify-adapter
// call-safety): current() returning nil — no generation live, or its
// manager was never built (e.g. no database configured) — must never panic
// and must still record the events-ring entry unconditionally, exactly like
// the pre-b2 Miner methods did.
func TestUpdateNotifyFuncsNilManagerIsSafe(t *testing.T) {
	notifyAvailable, notifyFailed := UpdateNotifyFuncs(func() *notifications.Manager { return nil })

	before := len(events.Recent(200))

	notifyAvailable("v1.0.0", "v1.1.0", "https://example.test/release")
	notifyFailed("v1.0.0", "v1.1.0", "checksum mismatch")

	after := events.Recent(200)
	if len(after) < before+2 {
		t.Fatalf("expected at least 2 new ring events with a nil manager, got %d new (before=%d after=%d)",
			len(after)-before, before, len(after))
	}

	foundAvailable, foundFailed := false, false
	for _, e := range after {
		if e.Type == events.TypeUpdateAvailable {
			foundAvailable = true
		}
		if e.Type == events.TypeUpdateFailed {
			foundFailed = true
		}
	}
	if !foundAvailable {
		t.Error("expected an update_available ring event even with a nil manager")
	}
	if !foundFailed {
		t.Error("expected an update_failed ring event even with a nil manager")
	}
}

// TestUpdateNotifyFuncsStoppedManagerIsSafe (design v6 §7 notify-adapter
// call-safety): current() returning an ALREADY-STOPPED manager must also
// never panic or hang — Manager.Stop closes the dispatch admission gate, so
// NotifyUpdateAvailable/NotifyUpdateFailed on a stopped manager are
// themselves safe no-ops. This uses a real, Discord-disabled manager (built
// the same way the rest of this package's tests do, via
// initNotificationManager over a throwaway sqlite db) — internal/miner has
// no seam to inject a fake Discord provider from outside the notifications
// package, so a Discord-ENABLED-then-stopped manager isn't constructible
// here; NotifyUpdateAvailable/Failed already early-return before touching
// anything when Discord is disabled, so this exercises "stopped manager,
// call is safe" for the reachable-from-here case, not the Discord-dispatch
// path specifically.
func TestUpdateNotifyFuncsStoppedManagerIsSafe(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "alpha")
	dbPath := filepath.Join(t.TempDir(), "update-notify-stopped.db")
	db := openRawMinerDB(t, dbPath)
	defer func() { _ = db.Close() }()
	m.db = db

	m.initNotificationManager(context.Background())
	mgr := m.notificationManager()
	if mgr == nil {
		t.Fatal("setup: initNotificationManager did not publish a manager")
	}
	if err := mgr.Stop(); err != nil {
		t.Fatalf("setup: mgr.Stop() = %v, want nil", err)
	}

	notifyAvailable, notifyFailed := UpdateNotifyFuncs(func() *notifications.Manager { return mgr })

	// Must not panic and must not block.
	notifyAvailable("v1.0.0", "v1.1.0", "https://example.test/release")
	notifyFailed("v1.0.0", "v1.1.0", "checksum mismatch")
}
