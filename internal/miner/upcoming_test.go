package miner

import (
	"testing"
	"time"
)

// resolveDisplayLocation loads the configured zone, and falls back to the
// server local time (never failing) for an empty or unloadable name.
func TestResolveDisplayLocation(t *testing.T) {
	if got := resolveDisplayLocation(""); got != time.Local {
		t.Errorf("empty time zone must fall back to time.Local, got %v", got)
	}
	if got := resolveDisplayLocation("this/is-not-a-zone"); got != time.Local {
		t.Errorf("invalid time zone must fall back to time.Local, got %v", got)
	}
	loc := resolveDisplayLocation("Asia/Jerusalem")
	if loc == nil || loc.String() != "Asia/Jerusalem" {
		t.Fatalf("valid zone must load, got %v", loc)
	}
	// The loaded zone must be a real, DST-aware location (offset differs between
	// winter and summer for Asia/Jerusalem).
	winter := time.Date(2026, 1, 15, 12, 0, 0, 0, loc)
	summer := time.Date(2026, 7, 15, 12, 0, 0, 0, loc)
	_, wOff := winter.Zone()
	_, sOff := summer.Zone()
	if wOff == sOff {
		t.Errorf("expected DST-aware offsets to differ, winter=%d summer=%d", wOff, sOff)
	}
}
