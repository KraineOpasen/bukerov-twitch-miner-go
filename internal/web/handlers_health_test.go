package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHealthPageRendersThroughBaseLayout guards against the regression where
// HealthView omitted the base-layout fields (DiscordEnabled/DebugURL/Username/
// Version) that base.html references for every full-page view model, causing
// /health to fail with "can't evaluate field DiscordEnabled in type
// web.HealthView". This exercises the real render through base.html (not just
// template compilation): if base.html aborts on a missing field, the content
// block never runs and its heading is absent from the output.
func TestHealthPageRendersThroughBaseLayout(t *testing.T) {
	srv := newStatsTestServer(t) // username "tester", no health provider (Available=false)

	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /health = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// The content block is invoked by base.html *after* the sidebar that
	// references .DiscordEnabled/.DebugURL — so its presence proves base.html
	// executed the whole way through with a valid HealthView.
	if !strings.Contains(body, "Центр состояния") {
		t.Errorf("content block did not render through base.html (base likely aborted on a missing field); body=%d bytes", len(body))
	}
	// Base-layout markers that only appear once the sidebar/footer render fully.
	for _, marker := range []string{"app-sidebar", "Настройки", "tester"} {
		if !strings.Contains(body, marker) {
			t.Errorf("rendered /health missing base-layout marker %q", marker)
		}
	}
	// A Go template execution error would surface this fragment.
	if strings.Contains(body, "can't evaluate field") {
		t.Errorf("template execution error leaked into /health output")
	}
}

// TestBuildHealthViewPopulatesBaseFields verifies the base-layout fields the
// layout needs are set even when no health provider is attached.
func TestBuildHealthViewPopulatesBaseFields(t *testing.T) {
	srv := newStatsTestServer(t)
	v := srv.buildHealthView(enTR(t))

	if v.Username != "tester" {
		t.Errorf("Username = %q, want tester", v.Username)
	}
	if v.RefreshMinutes != 5 {
		t.Errorf("RefreshMinutes = %d, want 5", v.RefreshMinutes)
	}
}
