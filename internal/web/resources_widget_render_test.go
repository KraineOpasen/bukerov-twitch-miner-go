package web

import (
	"net/http"
	"net/http/httptest"
	"regexp"
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
// It goes through the registered mux rather than calling handleSystemStatusPage
// directly, so the ownership assertions below cover ROUTE registration too: a
// /system/status route that is renamed or dropped would otherwise keep passing
// them, since a direct handler call ignores the request path entirely.
func renderSystemStatus(t *testing.T, lang string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/system/status", nil)
	if lang != "" {
		req.AddCookie(&http.Cookie{Name: langCookieName, Value: lang})
	}
	rec := httptest.NewRecorder()
	newRenderServer(t).handler().ServeHTTP(rec, req)
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

// topBarLangGroupRe matches the actual RU/EN language switcher: one role="group"
// element holding the two /api/lang buttons with their visible labels. \s* keeps
// whitespace out of the contract, so this is a structural assertion rather than
// a byte-exact snapshot of the rendered markup.
var topBarLangGroupRe = regexp.MustCompile(
	`<div[^>]*role="group"[^>]*>\s*` +
		`<button[^>]*hx-vals='\{"lang":"ru"\}'[^>]*>RU</button>\s*` +
		`<button[^>]*hx-vals='\{"lang":"en"\}'[^>]*>EN</button>\s*` +
		`</div>`)

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
		end := strings.Index(body[i:], "</header>")
		if end < 0 {
			t.Fatalf("%s page: the top-bar row is not inside a closed <header>", name)
		}
		inner := body[i+len(row) : i+end]

		// The widget block renders EMPTY, so the first thing inside the row is
		// base.html's OWN theme-toggle group — identified by its id, not by
		// "some <div>", which any injected widget would satisfy just as well.
		lt := strings.IndexByte(inner, '<')
		if lt < 0 {
			t.Fatalf("%s page: the top-bar row contains no elements at all; inner=%q", name, inner)
		}
		if pre := strings.TrimSpace(inner[:lt]); pre != "" {
			t.Errorf("%s page injects the text %q into the top-bar widget block", name, pre)
		}
		gt := strings.IndexByte(inner[lt:], '>')
		if gt < 0 {
			t.Fatalf("%s page: the first element inside the top-bar row is never closed; inner=%q", name, inner[lt:])
		}
		if boundary := inner[lt : lt+gt+1]; !strings.Contains(boundary, `id="theme-toggle"`) {
			t.Errorf("%s page: the first element after the top-bar widget block is %s, want base.html's own #theme-toggle group — an injected widget would take exactly this position", name, boundary)
		}

		// ...and the RU/EN switcher itself is the expected language group, so
		// this can never pass on a row that renders arbitrary markup instead.
		if !topBarLangGroupRe.MatchString(inner) {
			t.Errorf("%s page: the top-bar row does not render the expected RU/EN language group; inner=%s", name, inner)
		}
	}
}
