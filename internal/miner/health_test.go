package miner

import (
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/drops"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/health"
)

// TestRefreshHealthCenterDisabledWatchdogFallsBack guards the drops_progress
// signal semantics when the operator disables the watchdog: the signal must
// fall back to the passive tracked-campaigns view (OK, "N tracked"), never
// misreport IDLE "no active drop campaign" while campaigns are tracked and
// progressing — that was pre-watchdog behavior and disabling the watchdog must
// not regress it.
func TestRefreshHealthCenterDisabledWatchdogFallsBack(t *testing.T) {
	tracker := drops.NewDropsTracker(snapshotDropsClient{}, nil, config.RateLimitSettings{}, nil)
	tracker.SyncNow() // one tracked campaign

	center := health.NewCenter()
	watchdog := health.NewProgressWatchdog(center, tracker, nil, nil, nil, nil, nil,
		health.WatchdogConfig{Enabled: false})

	m := &Miner{
		config:           &config.Config{Username: "tester"},
		dropsTracker:     tracker,
		healthCenter:     center,
		progressWatchdog: watchdog,
	}
	m.refreshHealthCenter(time.Now())

	sig, ok := center.Snapshot().Signal(health.SignalDropsProgress)
	if !ok {
		t.Fatal("expected a drops_progress signal to be recorded")
	}
	if sig.Status != health.StatusOK || !strings.Contains(sig.Detail, "watchdog disabled") {
		t.Fatalf("disabled watchdog must fall back to the tracked-campaigns view, got %+v", sig)
	}
}

// TestStalenessSignal covers the four branches, especially the new degraded
// tier: full staleness (failed) must win over the failCount (degraded), and a
// zero degradeThreshold must never produce degraded.
func TestStalenessSignal(t *testing.T) {
	now := time.Now()
	threshold := 15 * time.Minute
	fresh := now.Add(-time.Minute)
	stale := now.Add(-30 * time.Minute)

	cases := []struct {
		name       string
		last       time.Time
		failCount  int
		degradeMin int
		want       string
	}{
		{"never checked", time.Time{}, 5, 2, health.StatusUnknown},
		{"fresh, no failures", fresh, 0, 2, health.StatusOK},
		{"fresh, below degrade threshold", fresh, 1, 2, health.StatusOK},
		{"fresh, at degrade threshold", fresh, 2, 2, health.StatusDegraded},
		{"stale beats degrade", stale, 9, 2, health.StatusFailed},
		{"zero threshold disables degrade", fresh, 9, 0, health.StatusOK},
	}
	for _, c := range cases {
		sig := stalenessSignal("test", c.last, now, threshold, "stale detail", c.failCount, c.degradeMin, "degrade detail")
		if sig.Status != c.want {
			t.Errorf("%s: status = %q, want %q", c.name, sig.Status, c.want)
		}
	}
}

// TestDropsInventorySignal pins the §12 Status-Center freshness contract for the
// Drops Inventory Sync signal: unknown before the first run; failed on a last
// error; OK while within the configured interval (with a next-sync ETA and, for
// a long interval, the honest new-campaign delay); DEGRADED once the last
// SUCCESS is older than interval+grace even if a later attempt was recent.
func TestDropsInventorySignal(t *testing.T) {
	now := time.Now()

	t.Run("never run is unknown", func(t *testing.T) {
		sig := dropsInventorySignal(drops.SyncStatus{IntervalMinutes: 60}, now)
		if sig.Status != health.StatusUnknown {
			t.Fatalf("status = %q, want unknown", sig.Status)
		}
	})

	t.Run("last error is failed", func(t *testing.T) {
		sig := dropsInventorySignal(drops.SyncStatus{
			IntervalMinutes: 60, LastSyncAt: now.Add(-time.Minute), LastError: "boom",
		}, now)
		if sig.Status != health.StatusFailed || sig.ErrorCode != "sync_error" {
			t.Fatalf("got %q/%q, want failed/sync_error", sig.Status, sig.ErrorCode)
		}
	})

	t.Run("fresh within interval is ok with ETA and delay note", func(t *testing.T) {
		last := now.Add(-36 * time.Minute) // 36m ago at a 60m interval: still healthy
		sig := dropsInventorySignal(drops.SyncStatus{
			IntervalMinutes: 60, LastSyncAt: last, LastSuccessAt: last, TrackedCampaigns: 1,
		}, now)
		if sig.Status != health.StatusOK {
			t.Fatalf("status = %q, want ok", sig.Status)
		}
		// next sync ~24m away, and the long-interval delay caveat present.
		if !strings.Contains(sig.Detail, "next sync in ~24m") {
			t.Errorf("detail missing next-sync ETA: %q", sig.Detail)
		}
		if !strings.Contains(sig.Detail, "up to 1h0m0s to appear") && !strings.Contains(sig.Detail, "up to 1h") {
			t.Errorf("detail missing long-interval new-campaign caveat: %q", sig.Detail)
		}
	})

	t.Run("overdue success is degraded even if attempted recently", func(t *testing.T) {
		sig := dropsInventorySignal(drops.SyncStatus{
			IntervalMinutes: 60,
			LastSyncAt:      now.Add(-time.Minute),      // attempted recently...
			LastSuccessAt:   now.Add(-90 * time.Minute), // ...but last SUCCESS is 90m ago (> 60m + 15m grace)
		}, now)
		if sig.Status != health.StatusDegraded || sig.ErrorCode != "sync_overdue" {
			t.Fatalf("got %q/%q, want degraded/sync_overdue; detail=%q", sig.Status, sig.ErrorCode, sig.Detail)
		}
	})
}
