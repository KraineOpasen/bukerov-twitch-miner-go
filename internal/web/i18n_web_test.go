package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// postForm builds a same-origin form POST unless a foreign origin is given.
func langPost(origin string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:5000/api/lang", strings.NewReader("lang=en"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	return req
}

// TestLangEndpointSetsCookieAndRefresh verifies a same-origin POST persists the
// language cookie and asks the client to refresh.
func TestLangEndpointSetsCookieAndRefresh(t *testing.T) {
	clearSecurityEnv(t)
	s := newRenderServer(t)
	handler := s.handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, langPost("http://127.0.0.1:5000"))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if rec.Header().Get("HX-Refresh") != "true" {
		t.Errorf("missing HX-Refresh: true header")
	}
	var langCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == langCookieName {
			langCookie = c
		}
	}
	if langCookie == nil || langCookie.Value != "en" {
		t.Fatalf("lang cookie = %+v, want value en", langCookie)
	}
	if !langCookie.HttpOnly {
		t.Errorf("lang cookie should be HttpOnly")
	}
}

// TestLangEndpointCSRF proves /api/lang is inside the shared csrfProtectMiddleware
// chain: a cross-origin POST is rejected, a same-origin POST passes.
func TestLangEndpointCSRF(t *testing.T) {
	clearSecurityEnv(t)
	handler := newRenderServer(t).handler()

	// Cross-origin POST: blocked by the CSRF layer before reaching the handler.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, langPost("http://evil.example"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin /api/lang = %d, want 403 (must be CSRF-protected)", rec.Code)
	}

	// Same-origin POST: passes the CSRF layer and is handled (204).
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, langPost("http://127.0.0.1:5000"))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("same-origin /api/lang = %d, want 204", rec.Code)
	}
}

// TestLangEndpointRejectsNonPost verifies the endpoint is POST-only.
func TestLangEndpointRejectsNonPost(t *testing.T) {
	clearSecurityEnv(t)
	handler := newRenderServer(t).handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:5000/api/lang", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/lang = %d, want 405", rec.Code)
	}
}

// TestPageRespectsLangCookie verifies the rendered page switches language based
// on the cookie: English nav for lang=en, Russian (default) otherwise.
func TestPageRespectsLangCookie(t *testing.T) {
	s := newRenderServer(t)

	// Default (no cookie) -> Russian nav.
	recRU := httptest.NewRecorder()
	s.renderPage(recRU, httptest.NewRequest(http.MethodGet, "/drops", nil), "drops.html", DropsPageData{Username: "tester"})
	ru := recRU.Body.String()
	if !strings.Contains(ru, "Обзор") || !strings.Contains(ru, `lang="ru"`) {
		t.Errorf("default render should be Russian (nav 'Обзор', html lang=ru)")
	}

	// lang=en cookie -> English nav.
	reqEN := httptest.NewRequest(http.MethodGet, "/drops", nil)
	reqEN.AddCookie(&http.Cookie{Name: langCookieName, Value: "en"})
	recEN := httptest.NewRecorder()
	s.renderPage(recEN, reqEN, "drops.html", DropsPageData{Username: "tester"})
	en := recEN.Body.String()
	if !strings.Contains(en, ">\n                    Overview") && !strings.Contains(en, "Overview") {
		t.Errorf("lang=en render should show English nav 'Overview'")
	}
	if !strings.Contains(en, `lang="en"`) {
		t.Errorf("lang=en render should set <html lang=\"en\">")
	}
	if strings.Contains(en, "Обзор") {
		t.Errorf("lang=en render must not contain Russian nav")
	}
}

// TestJSCatalogInjected verifies the js.* catalog is injected for client-side use.
func TestJSCatalogInjected(t *testing.T) {
	s := newRenderServer(t)
	rec := httptest.NewRecorder()
	s.renderPage(rec, httptest.NewRequest(http.MethodGet, "/drops", nil), "drops.html", DropsPageData{Username: "tester"})
	body := rec.Body.String()
	if !strings.Contains(body, "window.I18N =") {
		t.Errorf("page should inject window.I18N catalog")
	}
	if !strings.Contains(body, "js.overlay.initializing") {
		t.Errorf("injected catalog should include js.* keys")
	}
}
