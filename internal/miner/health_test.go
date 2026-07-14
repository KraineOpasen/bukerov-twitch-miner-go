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
