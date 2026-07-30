package miner

import (
	"fmt"
	"log/slog"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/notifications"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/updater"
)

// UpdateNotifyFuncs builds the updater's Notify/NotifyFailure callbacks as
// PACKAGE-LEVEL functions rather than Miner methods (design v6 §7: "Notify-
// адаптер generation-независим"). The auto-updater now lives at the process
// level (internal/lifecycle), outliving any single Miner generation — a
// pause/resume/restart cycle tears down and rebuilds the generation (and
// whatever notifications.Manager it owns) while the SAME process-level
// updater loop keeps running underneath it, so these callbacks must not be
// bound to one generation's methods the way the pre-b2 Miner.
// notifyUpdateAvailable/notifyUpdateFailed methods were.
//
// current is invoked fresh on every call to resolve "the live generation's
// notifications manager, if any" — b3 wires it to the current generation's
// accessor; a caller with no generation concept at all can pass a func that
// always returns nil. Both returned funcs ALWAYS record the events ring
// entry first, unconditionally, exactly like the pre-b2 methods did, then
// best-effort forward to Discord through whatever manager current()
// resolves to right now:
//
//   - current() == nil (no generation live, or its manager was never built —
//     e.g. no database configured): slog only, no Discord. Safe: never
//     touches a nil pointer.
//   - current() returns a manager that has already had Stop called on it:
//     Stop closes the Manager's dispatch admission gate, so calling
//     NotifyUpdateAvailable/NotifyUpdateFailed on it is itself a safe no-op
//     (see notifications.Manager.Stop's doc comment) — never panics, never
//     blocks waiting on anything the stopped manager has already torn down.
func UpdateNotifyFuncs(current func() *notifications.Manager) (updater.NotifyFunc, updater.NotifyFailureFunc) {
	notifyAvailable := func(cur, latest, releaseURL string) {
		events.Record(events.TypeUpdateAvailable, "", fmt.Sprintf("%s -> %s", cur, latest))

		mgr := current()
		if mgr == nil {
			slog.Info("Auto-update: newer release available (no live notifications manager)",
				"current", cur, "latest", latest)
			return
		}
		mgr.NotifyUpdateAvailable(cur, latest, releaseURL)
	}

	notifyFailed := func(cur, latest, reason string) {
		events.Record(events.TypeUpdateFailed, "", fmt.Sprintf("%s -> %s: %s", cur, latest, reason))

		mgr := current()
		if mgr == nil {
			slog.Info("Auto-update: applying update failed (no live notifications manager)",
				"current", cur, "latest", latest, "reason", reason)
			return
		}
		mgr.NotifyUpdateFailed(cur, latest, reason)
	}

	return notifyAvailable, notifyFailed
}
