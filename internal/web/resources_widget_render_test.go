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

// renderSystemStatus renders /system/status, the sole owner of host resources.
func renderSystemStatus(t *testing.T, lang string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/system/status", nil)
	if lang != "" {
		req.AddCookie(&http.Cookie{Name: langCookieName, Value: lang})
	}
	rec := httptest.NewRecorder()
	newRenderServer(t).handleSystemStatusPage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /system/status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

// TestResourceWidgetsAbsentFromOverview: CPU / RAM / network / disk are owned
// exclusively by /system/status. The Overview renders none of them and never
// reaches for the sampler endpoint that feeds them.
func TestResourceWidgetsAbsentFromOverview(t *testing.T) {
	html := renderOverview(t, "")
	for _, banned := range []string{
		`id="rw-strip"`, `class="rw-card"`, `class="rw-ico"`, `class="rw-spark"`,
		`data-rw="cpu"`, `data-rw="memory"`, `data-rw="network"`, `data-rw="disk"`,
		"data-rw-primary", "data-rw-spark", "/api/resources", "__rwPoller",
	} {
		if strings.Contains(html, banned) {
			t.Errorf("/overview still carries host-resource surface %q", banned)
		}
	}
}

// TestHostResourcesStillOwnedBySystemStatus is the non-vacuity half: the
// widgets did not disappear from the product, they moved. Without this, the
// bans above could pass simply because resource rendering broke everywhere.
func TestHostResourcesStillOwnedBySystemStatus(t *testing.T) {
	html := renderSystemStatus(t, "en")
	for _, want := range []string{"CPU", "Memory", "Network", "Disk"} {
		if !strings.Contains(html, want) {
			t.Errorf("/system/status missing host-resource label %q", want)
		}
	}
}

// TestResourceLabelsLocalizeOnTheirOwnerPage keeps the localization coverage the
// Overview widgets used to provide, on the page that now owns them.
func TestResourceLabelsLocalizeOnTheirOwnerPage(t *testing.T) {
	ru := renderSystemStatus(t, "ru")
	for _, want := range []string{"Память", "Сеть", "Диск"} {
		if !strings.Contains(ru, want) {
			t.Errorf("RU /system/status missing resource label %q", want)
		}
	}
	if strings.Contains(ru, ">Memory<") {
		t.Error("RU render leaked the English 'Memory' label")
	}

	en := renderSystemStatus(t, "en")
	if strings.Contains(en, ">Память<") {
		t.Error("EN render leaked the Russian 'Память' label")
	}
}

// TestTopBarWidgetBlockEmptyOnEveryPage: with the Overview's topbar_widgets
// override gone, EVERY page (Overview included) now falls back to base.html's
// empty default, so the top-bar language-switcher row is identical everywhere.
func TestTopBarWidgetBlockEmptyOnEveryPage(t *testing.T) {
	s := newRenderServer(t)
	rec := httptest.NewRecorder()
	s.handleLogsPage(rec, httptest.NewRequest(http.MethodGet, "/logs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/logs = %d, want 200", rec.Code)
	}
	logs := rec.Body.String()
	overview := renderOverview(t, "")

	const row = `<div class="flex items-center justify-end mb-3">`
	for name, body := range map[string]string{"logs": logs, "overview": overview} {
		i := strings.Index(body, row)
		if i < 0 {
			t.Fatalf("%s page missing the top-bar row", name)
		}
		// Nothing is injected between the row's opening tag and the RU/EN
		// group that follows it: the block renders empty on both pages.
		rest := strings.TrimLeft(body[i+len(row):], " \t\r\n")
		if !strings.HasPrefix(rest, "<div") {
			t.Errorf("%s page injects content into the top-bar widget block", name)
		}
	}
}
