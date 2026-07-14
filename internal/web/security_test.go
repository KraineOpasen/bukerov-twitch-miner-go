package web

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// clearSecurityEnv resets every env var the security layer reads so tests
// are hermetic regardless of the environment they run in.
func clearSecurityEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"DASHBOARD_HOST",
		"DASHBOARD_USERNAME",
		"DASHBOARD_PASSWORD",
		"DASHBOARD_INSECURE_NO_AUTH",
		"DASHBOARD_TRUSTED_ORIGINS",
	} {
		t.Setenv(key, "")
	}
}

func TestResolveBindHost(t *testing.T) {
	clearSecurityEnv(t)

	host, source := resolveBindHost("127.0.0.1")
	if host != "127.0.0.1" || source != "config analytics.host" {
		t.Fatalf("expected config host, got %q from %q", host, source)
	}

	t.Setenv("DASHBOARD_HOST", "0.0.0.0")
	host, source = resolveBindHost("127.0.0.1")
	if host != "0.0.0.0" || source != "DASHBOARD_HOST env" {
		t.Fatalf("expected env override, got %q from %q", host, source)
	}
}

func TestIsLoopbackHost(t *testing.T) {
	loopback := []string{"127.0.0.1", "127.0.0.53", "::1", "[::1]", "localhost", "LOCALHOST"}
	for _, h := range loopback {
		if !isLoopbackHost(h) {
			t.Errorf("expected %q to be loopback", h)
		}
	}
	nonLoopback := []string{"", "0.0.0.0", "::", "10.100.102.24", "192.168.1.5", "example.com"}
	for _, h := range nonLoopback {
		if isLoopbackHost(h) {
			t.Errorf("expected %q to NOT be loopback", h)
		}
	}
}

func TestValidateBindSecurity(t *testing.T) {
	clearSecurityEnv(t)

	// Loopback: always fine, no credentials needed.
	if err := validateBindSecurity("127.0.0.1"); err != nil {
		t.Fatalf("loopback bind should not require auth: %v", err)
	}

	// Non-loopback without credentials: fail-closed with an actionable message.
	err := validateBindSecurity("0.0.0.0")
	if err == nil {
		t.Fatal("non-loopback bind without auth must be rejected")
	}
	for _, want := range []string{"DASHBOARD_USERNAME", "DASHBOARD_PASSWORD", "DASHBOARD_INSECURE_NO_AUTH", "127.0.0.1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("startup error should mention %s, got: %v", want, err)
		}
	}

	// Non-loopback with credentials: allowed.
	t.Setenv("DASHBOARD_USERNAME", "admin")
	t.Setenv("DASHBOARD_PASSWORD", "secret")
	if err := validateBindSecurity("0.0.0.0"); err != nil {
		t.Fatalf("non-loopback bind with auth should be allowed: %v", err)
	}

	// Non-loopback with the explicit opt-out: allowed.
	t.Setenv("DASHBOARD_USERNAME", "")
	t.Setenv("DASHBOARD_PASSWORD", "")
	t.Setenv("DASHBOARD_INSECURE_NO_AUTH", "true")
	if err := validateBindSecurity("0.0.0.0"); err != nil {
		t.Fatalf("explicit insecure opt-out should be allowed: %v", err)
	}
}

// okHandler records whether the request made it through the middleware.
func okHandler(reached *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusOK)
	})
}

func TestCSRFProtectMiddleware(t *testing.T) {
	clearSecurityEnv(t)

	cases := []struct {
		name       string
		method     string
		headers    map[string]string
		wantStatus int
	}{
		{"GET always passes", http.MethodGet, map[string]string{"Origin": "https://evil.example"}, http.StatusOK},
		{"HEAD always passes", http.MethodHead, map[string]string{"Origin": "https://evil.example"}, http.StatusOK},
		{"POST without provenance headers passes (curl)", http.MethodPost, nil, http.StatusOK},
		{"POST same-origin Origin passes", http.MethodPost, map[string]string{"Origin": "http://10.100.102.24:5000"}, http.StatusOK},
		{"POST cross-origin Origin blocked", http.MethodPost, map[string]string{"Origin": "http://evil.example"}, http.StatusForbidden},
		{"POST same host different port blocked", http.MethodPost, map[string]string{"Origin": "http://10.100.102.24:8080"}, http.StatusForbidden},
		{"POST null Origin blocked", http.MethodPost, map[string]string{"Origin": "null"}, http.StatusForbidden},
		{"POST Sec-Fetch-Site same-origin passes", http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusOK},
		{"POST Sec-Fetch-Site none passes", http.MethodPost, map[string]string{"Sec-Fetch-Site": "none"}, http.StatusOK},
		{"POST Sec-Fetch-Site cross-site blocked", http.MethodPost, map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"POST Sec-Fetch-Site same-site blocked", http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"POST same-origin Referer passes", http.MethodPost, map[string]string{"Referer": "http://10.100.102.24:5000/settings"}, http.StatusOK},
		{"POST cross-origin Referer blocked", http.MethodPost, map[string]string{"Referer": "http://evil.example/attack.html"}, http.StatusForbidden},
		{"DELETE cross-origin blocked", http.MethodDelete, map[string]string{"Origin": "http://evil.example"}, http.StatusForbidden},
		{"PUT cross-origin blocked", http.MethodPut, map[string]string{"Origin": "http://evil.example"}, http.StatusForbidden},
		{"cross-site Sec-Fetch-Site wins over matching Origin", http.MethodPost, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "http://10.100.102.24:5000"}, http.StatusForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reached := false
			handler := csrfProtectMiddleware(okHandler(&reached))

			req := httptest.NewRequest(tc.method, "http://10.100.102.24:5000/api/settings", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantStatus == http.StatusOK && !reached {
				t.Fatal("request should have reached the handler")
			}
			if tc.wantStatus == http.StatusForbidden && reached {
				t.Fatal("blocked request must not reach the handler")
			}
		})
	}
}

func TestCSRFTrustedOrigins(t *testing.T) {
	clearSecurityEnv(t)
	t.Setenv("DASHBOARD_TRUSTED_ORIGINS", "https://miner.example.com, proxy.lan:8443")

	reached := false
	handler := csrfProtectMiddleware(okHandler(&reached))

	for _, origin := range []string{"https://miner.example.com", "https://proxy.lan:8443"} {
		reached = false
		req := httptest.NewRequest(http.MethodPost, "http://10.100.102.24:5000/api/settings", nil)
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !reached {
			t.Fatalf("trusted origin %q should pass, got %d", origin, rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "http://10.100.102.24:5000/api/settings", nil)
	req.Header.Set("Origin", "https://untrusted.example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("untrusted origin should still be blocked, got %d", rec.Code)
	}
}

func TestSecurityHeadersMiddleware(t *testing.T) {
	reached := false
	handler := securityHeadersMiddleware(okHandler(&reached))

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:5000/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "same-origin",
	}
	for header, value := range want {
		if got := rec.Header().Get(header); got != value {
			t.Errorf("%s = %q, want %q", header, got, value)
		}
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("Content-Security-Policy header missing")
	}
	for _, directive := range []string{"default-src 'self'", "frame-ancestors 'none'", "connect-src 'self'"} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP missing directive %q: %s", directive, csp)
		}
	}
	if !reached {
		t.Fatal("request should pass through the headers middleware")
	}
}

func TestBasicAuthMiddleware(t *testing.T) {
	clearSecurityEnv(t)
	t.Setenv("DASHBOARD_USERNAME", "admin")
	t.Setenv("DASHBOARD_PASSWORD", "secret")

	reached := false
	handler := basicAuthMiddleware(okHandler(&reached))

	// No credentials: 401 with a challenge.
	req := httptest.NewRequest(http.MethodGet, "http://10.100.102.24:5000/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no credentials: status = %d, want 401", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("401 must carry a WWW-Authenticate challenge")
	}

	// Wrong credentials: 401.
	req = httptest.NewRequest(http.MethodGet, "http://10.100.102.24:5000/", nil)
	req.SetBasicAuth("admin", "wrong")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || reached {
		t.Fatalf("wrong credentials: status = %d, reached = %v", rec.Code, reached)
	}

	// Correct credentials: pass.
	req = httptest.NewRequest(http.MethodGet, "http://10.100.102.24:5000/", nil)
	req.SetBasicAuth("admin", "secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !reached {
		t.Fatalf("correct credentials: status = %d, reached = %v", rec.Code, reached)
	}
}

// TestHandlerChainCSRFAndHeaders exercises the full middleware chain via
// Server.handler(): GETs stay reachable, a mutating route is blocked
// cross-origin but allowed same-origin, and headers are applied everywhere.
func TestHandlerChainCSRFAndHeaders(t *testing.T) {
	clearSecurityEnv(t)

	s := &Server{templates: map[string]*template.Template{}}
	handler := s.handler()

	// A plain GET (same method the SSE stream uses) passes untouched even
	// with a foreign Origin - the CSRF layer only guards unsafe methods.
	req0 := httptest.NewRequest(http.MethodGet, "http://10.100.102.24:5000/api/status", nil)
	req0.Header.Set("Origin", "http://evil.example")
	rec0 := httptest.NewRecorder()
	handler.ServeHTTP(rec0, req0)
	if rec0.Code != http.StatusOK {
		t.Fatalf("GET /api/status: status = %d, want 200", rec0.Code)
	}

	// Cross-origin POST to a mutating endpoint: blocked by the CSRF layer
	// before any handler logic runs.
	req := httptest.NewRequest(http.MethodPost, "http://10.100.102.24:5000/api/settings", strings.NewReader("{}"))
	req.Header.Set("Origin", "http://evil.example")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST: status = %d, want 403", rec.Code)
	}

	// Same-origin POST passes the CSRF layer (handler itself may then reply
	// 4xx/5xx for other reasons - anything but 403 proves the middleware let
	// it through).
	req = httptest.NewRequest(http.MethodPost, "http://10.100.102.24:5000/api/settings", strings.NewReader("{}"))
	req.Header.Set("Origin", "http://10.100.102.24:5000")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Fatal("same-origin POST must not be blocked by the CSRF layer")
	}

	// Security headers are applied to every response.
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("middleware chain must apply security headers")
	}
}
