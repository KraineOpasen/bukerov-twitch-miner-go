package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// readEmbeddedTemplate returns the raw bytes of a template as they ship in the
// binary (from the same embed.FS the server serves), so these assertions guard
// what actually reaches the browser.
func readEmbeddedTemplate(t *testing.T, name string) string {
	t.Helper()
	b, err := templatesFS.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// TestPredictionCountdownTimerRemainsSharedAcrossPages proves retiring the
// scheduler projection did not remove the unrelated prediction-window timer.
func TestPredictionCountdownTimerRemainsSharedAcrossPages(t *testing.T) {
	base := readEmbeddedTemplate(t, "templates/base.html")
	overview := readEmbeddedTemplate(t, "templates/overview.html")

	// base.html renders into every page, so it defines and starts the shared
	// prediction countdown and re-runs it after htmx swaps.
	for _, want := range []string{
		"function tickCountdowns()",
		"[data-window-end]",
		"setInterval(tickCountdowns, 1000)",
		"addEventListener('htmx:afterSwap', tickCountdowns)",
	} {
		if !strings.Contains(base, want) {
			t.Errorf("base.html must carry the shared countdown so it runs on every page; missing %q", want)
		}
	}

	// overview.html must NOT re-declare or re-start the countdown, otherwise the
	// Overview page would run two intervals against the same elements — the
	// double-setInterval regression this move must avoid.
	for _, bad := range []string{
		"function tickCountdowns()",
		"setInterval(tickCountdowns",
	} {
		if strings.Contains(overview, bad) {
			t.Errorf("overview.html must not re-declare the countdown (would double-run it); found %q", bad)
		}
	}

	// Scheduler timing is no longer projected into the sidebar.
	partial := readEmbeddedTemplate(t, "templates/partials/now_watching.html")
	if strings.Contains(partial, "data-countdown-to") {
		t.Error("now_watching partial still emits the retired scheduler countdown")
	}
}

// TestCountdownJSDeliveredOnNonOverviewPages renders full pages through the real
// server and asserts the unrelated prediction countdown helper remains part of
// the shared base layout.
func TestPredictionCountdownJSDeliveredOnNonOverviewPages(t *testing.T) {
	srv := newStatsTestServer(t)
	for _, path := range []string{"/health", "/logs", "/statistics"} {
		rec := httptest.NewRecorder()
		srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200; body=%q", path, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "setInterval(tickCountdowns, 1000)") {
			t.Errorf("GET %s: shared prediction countdown JS not delivered", path)
		}
	}
}
