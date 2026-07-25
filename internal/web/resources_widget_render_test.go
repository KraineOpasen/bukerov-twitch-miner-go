package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// renderOverview renders the full Overview page (base.html + overview.html) in
// the given language and returns the HTML.
func renderOverview(t *testing.T, lang string) string {
	t.Helper()
	srv, _, _ := newOverviewTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if lang != "" {
		req.AddCookie(&http.Cookie{Name: langCookieName, Value: lang})
	}
	rec := httptest.NewRecorder()
	srv.handleDashboard(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

func TestOverviewRendersFourResourceWidgets(t *testing.T) {
	html := renderOverview(t, "")
	if !strings.Contains(html, `id="rw-strip"`) {
		t.Fatal("overview missing the resource-widget strip")
	}
	for _, m := range []string{"cpu", "memory", "network", "disk"} {
		if !strings.Contains(html, `data-rw="`+m+`"`) {
			t.Errorf("overview missing the %q resource widget", m)
		}
	}
	// Exactly four decorative icons, each aria-hidden.
	if n := strings.Count(html, `<span class="rw-ico" aria-hidden="true">`); n != 4 {
		t.Errorf("found %d decorative widget icons, want 4", n)
	}
	if n := strings.Count(html, `class="rw-spark"`); n != 4 {
		t.Errorf("found %d spark containers, want 4", n)
	}
	// The poller targets the read-only endpoint.
	if !strings.Contains(html, "/api/resources") {
		t.Error("overview poller does not reference /api/resources")
	}
}

func TestResourceWidgetsRussianLabels(t *testing.T) {
	html := renderOverview(t, "ru")
	for _, want := range []string{"Системные ресурсы", "Память", "Сеть", "Диск"} {
		if !strings.Contains(html, want) {
			t.Errorf("RU overview missing widget label %q", want)
		}
	}
	// English memory label must not appear in the RU render's widgets.
	if strings.Contains(html, ">Memory<") {
		t.Error("RU render leaked the English 'Memory' label")
	}
}

func TestResourceWidgetsEnglishLabels(t *testing.T) {
	html := renderOverview(t, "en")
	for _, want := range []string{"System resources", "Memory", "Network", "Disk I/O"} {
		if !strings.Contains(html, want) {
			t.Errorf("EN overview missing widget label %q", want)
		}
	}
	if strings.Contains(html, ">Память<") {
		t.Error("EN render leaked the Russian 'Память' label")
	}
}

// TestResourceWidgetsAbsentOnOtherPages proves the topbar_widgets block is
// overview-only: a non-overview page leaves the base default (empty), so the
// widgets and their poller never appear there.
func TestResourceWidgetsAbsentOnOtherPages(t *testing.T) {
	s := newRenderServer(t)
	rec := httptest.NewRecorder()
	s.handleLogsPage(rec, httptest.NewRequest(http.MethodGet, "/logs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/logs = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, banned := range []string{`id="rw-strip"`, `data-rw="cpu"`, "/api/resources"} {
		if strings.Contains(body, banned) {
			t.Errorf("logs page unexpectedly contains %q (widgets must be overview-only)", banned)
		}
	}
}

// TestTopBarByteIdenticalForOtherPages guards the byte-identity requirement:
// on a non-overview page the top-bar language-switcher row is unchanged by the
// added block (empty default renders nothing between `>` and the RU/EN group).
func TestTopBarByteIdenticalForOtherPages(t *testing.T) {
	s := newRenderServer(t)
	rec := httptest.NewRecorder()
	s.handleLogsPage(rec, httptest.NewRequest(http.MethodGet, "/logs", nil))
	body := rec.Body.String()
	// The empty block leaves the exact original markup: the flex row's `>` is
	// immediately followed by the newline + the RU/EN group's opening div.
	if !strings.Contains(body, `<div class="flex items-center justify-end mb-3">`+"\n") {
		t.Error("top-bar row markup changed on a non-overview page (block must render empty)")
	}
}
