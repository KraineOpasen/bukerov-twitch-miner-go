package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/debug"
)

func testSnapshot() debug.Snapshot {
	return debug.Snapshot{
		GeneratedAt: time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC),
		Version:     "test-version",
		Username:    "tester",
		Status:      debug.StatusRunning,
	}
}

// TestDebugSnapshotRoute covers the happy path through the full middleware
// chain: 200, JSON content type, no-store caching, a valid document with the
// snapshot's top-level keys.
func TestDebugSnapshotRoute(t *testing.T) {
	s := newRenderServer(t)
	s.SetDebugSnapshotProvider(testSnapshot)
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, DebugSnapshotPath, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", DebugSnapshotPath, rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	// The route sits behind the same outermost middleware as every page.
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("security headers missing on snapshot route")
	}

	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	for _, key := range []string{"generatedAt", "version", "status"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("snapshot JSON missing top-level key %q", key)
		}
	}
	if doc["version"] != "test-version" {
		t.Errorf("version = %v, want the provider's value (no second snapshot implementation)", doc["version"])
	}
}

// TestDebugSnapshotRouteDisabled: with no provider wired (Debug.Enabled
// false), the route fails closed with 404 and leaks nothing.
func TestDebugSnapshotRouteDisabled(t *testing.T) {
	s := newRenderServer(t)
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, DebugSnapshotPath, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET %s without provider = %d, want 404", DebugSnapshotPath, rec.Code)
	}
	if strings.Contains(rec.Body.String(), "generatedAt") {
		t.Errorf("disabled route leaked snapshot content")
	}
}

// TestDebugSnapshotRouteMethods: the route is GET-only.
func TestDebugSnapshotRouteMethods(t *testing.T) {
	s := newRenderServer(t)
	s.SetDebugSnapshotProvider(testSnapshot)
	h := s.handler()

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, DebugSnapshotPath, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", method, DebugSnapshotPath, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "generatedAt") {
			t.Errorf("%s must not serve the snapshot", method)
		}
	}
}

// TestDebugSnapshotRouteAuth: the route inherits the dashboard's Basic Auth
// middleware — 401 without credentials, 200 with them.
func TestDebugSnapshotRouteAuth(t *testing.T) {
	t.Setenv("DASHBOARD_USERNAME", "admin")
	t.Setenv("DASHBOARD_PASSWORD", "hunter2")

	s := newRenderServer(t)
	s.SetDebugSnapshotProvider(testSnapshot)
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, DebugSnapshotPath, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET without credentials = %d, want 401", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, DebugSnapshotPath, nil)
	req.SetBasicAuth("admin", "hunter2")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET with credentials = %d, want 200", rec.Code)
	}
}

// TestDebugSnapshotRouteProviderPanic: a broken provider yields a clean 500
// with no internals in the body, and the server keeps serving.
func TestDebugSnapshotRouteProviderPanic(t *testing.T) {
	s := newRenderServer(t)
	s.SetDebugSnapshotProvider(func() debug.Snapshot { panic("internal detail: secret state") })
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, DebugSnapshotPath, nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("GET with panicking provider = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	for _, leak := range []string{"goroutine", "runtime", "internal detail", "secret"} {
		if strings.Contains(body, leak) {
			t.Errorf("500 body leaks internals (%q):\n%s", leak, body)
		}
	}

	// The server must still answer the next request.
	s.SetDebugSnapshotProvider(testSnapshot)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, DebugSnapshotPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("request after panic = %d, want 200", rec.Code)
	}
}

// TestLogsPageSnapshotButton: with debug enabled the Logs page links the
// relative dashboard route (never a localhost URL) in a new tab; with debug
// disabled the button is absent.
func TestLogsPageSnapshotButton(t *testing.T) {
	s := newRenderServer(t)
	s.SetDebugURL(DebugSnapshotPath)

	rec := httptest.NewRecorder()
	s.handleLogsPage(rec, httptest.NewRequest(http.MethodGet, "/logs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/logs = %d, want 200", rec.Code)
	}
	html := rec.Body.String()

	if !strings.HasPrefix(DebugSnapshotPath, "/") {
		t.Errorf("DebugSnapshotPath %q must be relative (start with /)", DebugSnapshotPath)
	}
	if !strings.Contains(html, `href="`+DebugSnapshotPath+`"`) {
		t.Errorf("logs page missing relative snapshot link %q", DebugSnapshotPath)
	}
	if !strings.Contains(html, `target="_blank"`) || !strings.Contains(html, `rel="noopener"`) {
		t.Errorf("snapshot button lost target=_blank / rel=noopener")
	}
	for _, banned := range []string{"localhost:5757", "127.0.0.1:5757"} {
		if strings.Contains(html, banned) {
			t.Errorf("logs page must not embed %q", banned)
		}
	}

	// Debug disabled: no URL published, no button.
	s2 := newRenderServer(t)
	rec2 := httptest.NewRecorder()
	s2.handleLogsPage(rec2, httptest.NewRequest(http.MethodGet, "/logs", nil))
	if strings.Contains(rec2.Body.String(), DebugSnapshotPath) {
		t.Errorf("logs page shows the snapshot button while debug is disabled")
	}
}
