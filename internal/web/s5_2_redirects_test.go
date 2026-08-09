package web

// S5-2 compatibility route matrix tests (task §K.4, K.5, K.6, K.14): every
// additive temporary redirect (§E) round-trips GET/HEAD -> 302 with the
// exact target and query string preserved, rejects every other method with
// 405, never loops; the three new canonical direct-render routes return
// 200; every deferred (not-yet-built) route keeps its honest 404; and
// neither the compatibility routes nor this slice's chrome changes touch
// any existing API/JSON/POST endpoint or legacy page route.

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// wantCompatibilityRedirects hardcodes the exact route matrix from task §E,
// independent of the production compatibilityRedirects map — so a mutation
// to that map's target values (not just its cardinality) is actually
// caught, rather than the test tautologically re-deriving its own
// expectation from the very map under test.
//
// task S5-4 removed /drops/current, /drops/upcoming and /drops/past from
// this matrix: each is now its own direct-render route (handlers_drops.go),
// not a redirect to /drops. See TestS5_4DropsDirectRoutes for their 200/
// GET-HEAD/no-redirect coverage.
//
// task S5-5 removed /system/status, /system/diagnostics and /system/logs
// from this matrix: each is now its own direct-render route
// (handlers_system.go), not a redirect to /health or /logs. See
// s5_5_system_test.go for their 200/no-redirect coverage and the rest of the
// S5-5 System-page contract.
//
// task S5-6 removed all ten /settings/* entries from this matrix: each is
// now its own direct-render route (handlers_settings_categories.go), not a
// redirect to /settings. See s5_6_settings_test.go for their 200/no-redirect
// coverage and the rest of the S5-6 category-page contract.
var wantCompatibilityRedirects = map[string]string{
	"/analytics/points": "/statistics",
	"/analytics/roi":    "/statistics",
	"/help":             "/help/getting-started",
}

// TestS5_2CompatibilityRedirectsMapMatchesSpec proves the production map
// exactly matches the hardcoded route matrix — no missing, extra, or
// wrong-target entry.
func TestS5_2CompatibilityRedirectsMapMatchesSpec(t *testing.T) {
	if !reflect.DeepEqual(compatibilityRedirects, wantCompatibilityRedirects) {
		t.Fatalf("compatibilityRedirects does not match the task §E route matrix:\n got=%v\nwant=%v", compatibilityRedirects, wantCompatibilityRedirects)
	}
}

// TestS5_2RedirectMatrix table-drives every entry in the hardcoded expected
// route matrix: status, Location, query preservation, GET/HEAD-only, POST
// rejection. Deliberately iterates wantCompatibilityRedirects (not the
// production map) so a mutated production target is caught as a Location
// mismatch, not silently re-validated against itself.
func TestS5_2RedirectMatrix(t *testing.T) {
	srv := buildF3PageServer(t)
	h := srv.handler()

	if len(wantCompatibilityRedirects) != 3 {
		t.Fatalf("expected exactly 3 compatibility redirects, found %d", len(wantCompatibilityRedirects))
	}

	for route, target := range wantCompatibilityRedirects {
		t.Run(route, func(t *testing.T) {
			// GET, no query.
			rec, _ := httpGetBody(t, h, route)
			if rec.Code != http.StatusFound {
				t.Fatalf("GET %s = %d, want %d", route, rec.Code, http.StatusFound)
			}
			if loc := rec.Header().Get("Location"); loc != target {
				t.Errorf("GET %s Location = %q, want %q", route, loc, target)
			}

			// GET with a query string: preserved exactly, appended verbatim.
			req := httptest.NewRequest(http.MethodGet, route+"?foo=bar&baz=qux", nil)
			rec2 := httptest.NewRecorder()
			h.ServeHTTP(rec2, req)
			if rec2.Code != http.StatusFound {
				t.Fatalf("GET %s?... = %d, want %d", route, rec2.Code, http.StatusFound)
			}
			wantLoc := target + "?foo=bar&baz=qux"
			if loc := rec2.Header().Get("Location"); loc != wantLoc {
				t.Errorf("GET %s?... Location = %q, want %q", route, loc, wantLoc)
			}

			// HEAD: same redirect.
			reqHead := httptest.NewRequest(http.MethodHead, route, nil)
			recHead := httptest.NewRecorder()
			h.ServeHTTP(recHead, reqHead)
			if recHead.Code != http.StatusFound {
				t.Errorf("HEAD %s = %d, want %d", route, recHead.Code, http.StatusFound)
			}
			if loc := recHead.Header().Get("Location"); loc != target {
				t.Errorf("HEAD %s Location = %q, want %q", route, loc, target)
			}

			// POST: rejected, never redirected.
			reqPost := httptest.NewRequest(http.MethodPost, route, nil)
			recPost := httptest.NewRecorder()
			h.ServeHTTP(recPost, reqPost)
			if recPost.Code != http.StatusMethodNotAllowed {
				t.Errorf("POST %s = %d, want %d", route, recPost.Code, http.StatusMethodNotAllowed)
			}
			if allow := recPost.Header().Get("Allow"); allow != "GET, HEAD" {
				t.Errorf("POST %s Allow header = %q, want %q", route, allow, "GET, HEAD")
			}
		})
	}
}

// TestS5_2RedirectTargetsNeverLoop proves every redirect target is a
// canonical, non-redirected destination (never another key in the same
// map), so no chain can loop.
func TestS5_2RedirectTargetsNeverLoop(t *testing.T) {
	for route, target := range compatibilityRedirects {
		if _, isAlsoARedirect := compatibilityRedirects[target]; isAlsoARedirect {
			t.Errorf("redirect %s -> %s targets another redirect entry — potential loop", route, target)
		}
		if target == route {
			t.Errorf("redirect %s targets itself", route)
		}
	}
}

// TestS5_2RedirectTargetsRenderDirectly proves every redirect's destination
// still renders directly (200) — the canonical pages are untouched, never
// themselves redirected.
func TestS5_2RedirectTargetsRenderDirectly(t *testing.T) {
	srv := buildF3PageServer(t)
	h := srv.handler()

	targets := map[string]bool{}
	for _, target := range compatibilityRedirects {
		targets[target] = true
	}
	for target := range targets {
		rec, body := httpGetBody(t, h, target)
		if rec.Code != http.StatusOK {
			t.Errorf("canonical target %s = %d, want 200; body=%s", target, rec.Code, body)
		}
	}
}

// TestS5_2CanonicalDirectRoutes proves the additive direct-render routes
// return 200. task S5-5 extended this list with the three System routes
// (/system/status, /system/diagnostics, /system/logs), which replaced their
// former /system/* compatibility-redirect entries with real direct renders.
// task S5-6 extended it again with the ten Settings category routes, which
// replaced their former /settings/* compatibility-redirect entries.
func TestS5_2CanonicalDirectRoutes(t *testing.T) {
	srv := buildF3PageServer(t)
	for _, path := range []string{
		"/overview", "/events", "/help/getting-started",
		"/system/status", "/system/diagnostics", "/system/logs",
		"/settings/streamers", "/settings/rotation", "/settings/drops", "/settings/predictions",
		"/settings/chat-raids", "/settings/transport", "/settings/analytics-logging",
		"/settings/events-notifications", "/settings/discord", "/settings/system",
	} {
		body := f3GetPage(t, srv, path, "en")
		if body == "" {
			t.Errorf("%s rendered an empty body", path)
		}
	}
}

// TestS5_2ExistingLegacyRoutesUnchanged proves every pre-existing canonical
// page route still renders directly (never redirected) after this slice's
// chrome/route additions.
func TestS5_2ExistingLegacyRoutesUnchanged(t *testing.T) {
	srv := buildF3PageServer(t)
	for _, path := range []string{"/", "/drops", "/settings", "/statistics", "/health", "/logs", "/notifications"} {
		body := f3GetPage(t, srv, path, "en")
		if body == "" {
			t.Errorf("legacy route %s rendered an empty body", path)
		}
	}
}

// TestS5_2DeferredRoutesRemain404 proves every route explicitly deferred by
// task §E (not yet built — no fake placeholder) keeps its honest 404,
// falling through to the existing "/" catch-all exactly like any other
// unregistered path. S5-3 removed /overview/queue from this list: it is now
// a real direct-render route (handlers_queue.go) - see
// TestS5_3QueueRouteReturns200 and TestS5_3RemainingDeferredRoutesStill404.
// task S5-4 removed /drops/claims from this list: it is now a real
// direct-render route (handlers_drops.go) - see s5_4_drops_test.go.
func TestS5_2DeferredRoutesRemain404(t *testing.T) {
	srv := buildF3PageServer(t)
	h := srv.handler()
	deferred := []string{
		"/events/browser",
		"/events/sound",
		"/events/discord",
		"/help/glossary",
		"/help/troubleshooting",
		"/help/notifications-audio",
		"/help/diagnostics-support",
	}
	for _, path := range deferred {
		rec, _ := httpGetBody(t, h, path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("deferred route %s = %d, want 404 (no placeholder should exist yet)", path, rec.Code)
		}
	}
}

// TestS5_2APIAndJSONRoutesUntouched spot-checks that existing JSON/API/POST
// endpoints still resolve exactly as before — the new compatibility routes
// and chrome additions never shadow them.
func TestS5_2APIAndJSONRoutesUntouched(t *testing.T) {
	srv := buildF3PageServer(t)
	h := srv.handler()

	rec, body := httpGetBody(t, h, "/api/settings")
	if rec.Code != http.StatusOK {
		t.Errorf("GET /api/settings = %d, want 200; body=%s", rec.Code, body)
	}

	// /api/lang remains POST-only and state-changing (sets the language
	// cookie), untouched by the new /api-adjacent-looking page routes.
	reqLang := httptest.NewRequest(http.MethodPost, "/api/lang", nil)
	reqLang.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recLang := httptest.NewRecorder()
	h.ServeHTTP(recLang, reqLang)
	if recLang.Code != http.StatusNoContent {
		t.Errorf("POST /api/lang = %d, want %d", recLang.Code, http.StatusNoContent)
	}

	recStatus, _ := httpGetBody(t, h, "/api/status")
	if recStatus.Code != http.StatusOK {
		t.Errorf("GET /api/status = %d, want 200", recStatus.Code)
	}
}
