package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestOverviewStreakBadgeHonorsBroadcastBinding pins the render semantics of
// the R2 fix: after a grant on the CURRENT broadcast, a re-armed pursuit
// (blip/restart) must NOT show the "streak pending" badge — the watcher is
// silent, and the dashboard must agree with it. A genuinely new broadcast
// shows the badge again.
func TestOverviewStreakBadgeHonorsBroadcastBinding(t *testing.T) {
	srv, online, _ := newOverviewTestServer(t)

	fetch := func() string {
		rec := httptest.NewRecorder()
		srv.handleAPIOverview(rec, httptest.NewRequest(http.MethodGet, "/api/overview", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		return rec.Body.String()
	}

	// Sanity: no grant yet -> the pursuit badge is shown.
	if !strings.Contains(fetch(), `bar-streak`) {
		t.Fatal("precondition: pending streak must render the badge before any grant")
	}

	// Grant on the current broadcast, then a same-broadcast re-arm
	// (blip/restart): the badge must disappear even though the raw missing
	// flag is armed again.
	online.UpdateHistory("WATCH_STREAK", 300)
	online.Stream.InitWatchStreak()
	if strings.Contains(fetch(), `bar-streak`) {
		t.Fatal("badge shown for a broadcast whose streak is already granted (phantom moved from logs to UI)")
	}

	// A new broadcast re-arms for real: the badge returns.
	online.Stream.Update("b2", "Ranked", nil, nil, 40000)
	if !strings.Contains(fetch(), `bar-streak`) {
		t.Fatal("badge must return on a genuinely new broadcast")
	}
}
