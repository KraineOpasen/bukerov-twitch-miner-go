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

// TestCountdownTimerIsSharedAcrossPages guards the fix that moved the
// client-side countdown (pad/fmtDuration/tickCountdowns + setInterval + the
// htmx:afterSwap re-run) out of the Overview-only {{define "scripts"}} block and
// into base.html, which wraps every page. Before the move, the pinned sidebar
// "Next rotation" timer ticked only on Overview and stayed frozen at "--:--" on
// Drops/Logs/Statistics/Health/Settings, because those pages never loaded the
// updater JS.
func TestCountdownTimerIsSharedAcrossPages(t *testing.T) {
	base := readEmbeddedTemplate(t, "templates/base.html")
	overview := readEmbeddedTemplate(t, "templates/overview.html")

	// base.html renders into EVERY page, so it must define and start the
	// countdown (and re-run it after each htmx swap so a freshly-loaded sidebar
	// updates at once).
	for _, want := range []string{
		"function tickCountdowns()",
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

	// The sidebar timer element the shared JS drives still ships in the shared
	// partial (its markup is untouched), so there is something to update.
	partial := readEmbeddedTemplate(t, "templates/partials/now_watching.html")
	if !strings.Contains(partial, "data-countdown-to") {
		t.Error("now_watching partial must still emit data-countdown-to for the shared timer to update")
	}
}

// TestCountdownJSDeliveredOnNonOverviewPages renders full pages through the real
// server (base layout + content) and asserts the shared countdown updater ships
// in the HTML of pages OTHER than Overview. This is the end-to-end half of the
// fix: the sidebar timer element arrives via the htmx-loaded /api/now-watching
// partial (covered by the now_watching render tests), and the JS that keeps it
// ticking must arrive on every page — which before the fix it did not.
func TestCountdownJSDeliveredOnNonOverviewPages(t *testing.T) {
	srv := newStatsTestServer(t)
	for _, path := range []string{"/health", "/logs", "/statistics"} {
		rec := httptest.NewRecorder()
		srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200; body=%q", path, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "setInterval(tickCountdowns, 1000)") {
			t.Errorf("GET %s: shared countdown JS not delivered; the sidebar \"Next rotation\" timer would stay frozen at --:--", path)
		}
	}
}
