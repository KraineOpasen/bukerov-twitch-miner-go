package web

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/runtimeconfig"
)

// The security layer no longer reads the process environment: every decision
// takes a runtimeconfig.Dashboard snapshot, so these tests construct the
// snapshot explicitly and are hermetic with no t.Setenv.

func TestResolveBindHost(t *testing.T) {
	host, source := resolveBindHost(runtimeconfig.Dashboard{}, "127.0.0.1")
	if host != "127.0.0.1" || source != "config analytics.host" {
		t.Fatalf("expected config host, got %q from %q", host, source)
	}

	host, source = resolveBindHost(runtimeconfig.Dashboard{HostOverride: "0.0.0.0"}, "127.0.0.1")
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
	// Loopback: always fine, no credentials needed.
	if err := validateBindSecurity(runtimeconfig.Dashboard{}, "127.0.0.1"); err != nil {
		t.Fatalf("loopback bind should not require auth: %v", err)
	}

	// Non-loopback without credentials: fail-closed with an actionable message.
	err := validateBindSecurity(runtimeconfig.Dashboard{}, "0.0.0.0")
	if err == nil {
		t.Fatal("non-loopback bind without auth must be rejected")
	}
	for _, want := range []string{"DASHBOARD_USERNAME", "DASHBOARD_PASSWORD", "DASHBOARD_INSECURE_NO_AUTH", "127.0.0.1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("startup error should mention %s, got: %v", want, err)
		}
	}

	// Non-loopback with credentials: allowed.
	withAuth := runtimeconfig.Dashboard{Username: "admin", Password: runtimeconfig.NewSecret("secret")}
	if err := validateBindSecurity(withAuth, "0.0.0.0"); err != nil {
		t.Fatalf("non-loopback bind with auth should be allowed: %v", err)
	}

	// Non-loopback with the explicit opt-out: allowed.
	insecure := runtimeconfig.Dashboard{InsecureNoAuth: true}
	if err := validateBindSecurity(insecure, "0.0.0.0"); err != nil {
		t.Fatalf("explicit insecure opt-out should be allowed: %v", err)
	}
}

// TestValidateBindSecurityInvalidTrustedLANCIDRs is the Ф4d startup gate: an
// invalid DASHBOARD_TRUSTED_LAN_CIDRS value fails Start/validate regardless
// of bind host or auth mode (checked BEFORE the loopback short-circuit), and
// a valid allowlist alongside a loopback bind is unaffected.
func TestValidateBindSecurityInvalidTrustedLANCIDRs(t *testing.T) {
	_, parseErr := runtimeconfig.ParseTrustedLANCIDRs("not-a-cidr")
	if parseErr == nil {
		t.Fatal("test fixture: expected ParseTrustedLANCIDRs to fail for \"not-a-cidr\"")
	}

	invalid := runtimeconfig.Dashboard{TrustedLANCIDRsErr: parseErr.Error()}

	// Even a loopback bind (which every other check exempts) must be
	// rejected: the invalid-CIDR check runs BEFORE the loopback
	// short-circuit.
	err := validateBindSecurity(invalid, "127.0.0.1")
	if err == nil {
		t.Fatal("invalid DASHBOARD_TRUSTED_LAN_CIDRS must fail startup even for a loopback bind")
	}
	if !strings.Contains(err.Error(), "DASHBOARD_TRUSTED_LAN_CIDRS") {
		t.Errorf("startup error should name DASHBOARD_TRUSTED_LAN_CIDRS, got: %v", err)
	}

	// Also rejected for a non-loopback bind, auth configured or not.
	withAuth := runtimeconfig.Dashboard{
		TrustedLANCIDRsErr: parseErr.Error(),
		Username:           "admin",
		Password:           runtimeconfig.NewSecret("secret"),
	}
	if err := validateBindSecurity(withAuth, "0.0.0.0"); err == nil {
		t.Fatal("invalid DASHBOARD_TRUSTED_LAN_CIDRS must fail startup even with Basic Auth configured")
	}

	// A VALID allowlist alongside a loopback bind must not be affected.
	valid, err := runtimeconfig.ParseTrustedLANCIDRs("10.0.0.0/8")
	if err != nil {
		t.Fatalf("test fixture: %v", err)
	}
	ok := runtimeconfig.Dashboard{TrustedLANCIDRs: valid}
	if err := validateBindSecurity(ok, "127.0.0.1"); err != nil {
		t.Fatalf("a valid trusted-LAN allowlist must not block a loopback bind: %v", err)
	}
}

// TestLifecycleLANTrust is the seam-2 classifier matrix for
// lifecycleLANTrust: not-configured, allowed/denied across IPv4 and IPv6,
// multiple CIDRs, an unparseable/empty RemoteAddr (fail closed), and a
// zoned IPv6 address (never matches, per Prefix.Contains).
func TestLifecycleLANTrust(t *testing.T) {
	mustCIDRs := func(t *testing.T, raw string) []netip.Prefix {
		t.Helper()
		p, err := runtimeconfig.ParseTrustedLANCIDRs(raw)
		if err != nil {
			t.Fatalf("ParseTrustedLANCIDRs(%q): %v", raw, err)
		}
		return p
	}

	cases := []struct {
		name       string
		cidrs      string
		remoteAddr string
		want       lanTrust
	}{
		{"not configured, no CIDRs at all", "", "10.1.2.3:5555", lanTrustNotConfigured},
		{"IPv4 allowed", "10.0.0.0/8", "10.1.2.3:5555", lanTrustAllowed},
		{"IPv4 denied outside range", "10.0.0.0/8", "192.168.1.5:5555", lanTrustDenied},
		{"IPv6 allowed", "fd00::/8", "[fd12::1]:4242", lanTrustAllowed},
		{"IPv6 denied outside range", "fd00::/8", "[2001:db8::1]:4242", lanTrustDenied},
		{"multiple CIDRs, match in the second", "10.0.0.0/8,192.168.0.0/16", "192.168.5.5:1111", lanTrustAllowed},
		{"multiple CIDRs, match in neither", "10.0.0.0/8,192.168.0.0/16", "203.0.113.5:1111", lanTrustDenied},
		{"unparseable RemoteAddr denied (fail closed)", "10.0.0.0/8", "not-an-address", lanTrustDenied},
		{"empty RemoteAddr denied (fail closed)", "10.0.0.0/8", "", lanTrustDenied},
		{"zoned IPv6 never matches", "fe80::/10", "[fe80::1%eth0]:1234", lanTrustDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg runtimeconfig.Dashboard
			if tc.cidrs != "" {
				cfg.TrustedLANCIDRs = mustCIDRs(t, tc.cidrs)
			}
			if got := lifecycleLANTrust(cfg, tc.remoteAddr); got != tc.want {
				t.Errorf("lifecycleLANTrust(%v, %q) = %v, want %v", cfg.TrustedLANCIDRs, tc.remoteAddr, got, tc.want)
			}
		})
	}
}

// TestNonPrivateTrustedLANPrefixes is the A3 corrective-pass classifier
// test: entries fully inside a private/loopback/link-local/ULA range are
// never flagged; anything broader (including the degenerate 0.0.0.0/0) is.
func TestNonPrivateTrustedLANPrefixes(t *testing.T) {
	mustCIDRs := func(t *testing.T, raw string) []netip.Prefix {
		t.Helper()
		p, err := runtimeconfig.ParseTrustedLANCIDRs(raw)
		if err != nil {
			t.Fatalf("ParseTrustedLANCIDRs(%q): %v", raw, err)
		}
		return p
	}

	cases := []struct {
		name        string
		cidrs       string
		wantFlagged []string
	}{
		{"private RFC1918 /24 not flagged", "192.168.1.0/24", nil},
		{"ULA /8 not flagged", "fd00::/8", nil},
		{"loopback /8 not flagged", "127.0.0.0/8", nil},
		{"default route flagged", "0.0.0.0/0", []string{"0.0.0.0/0"}},
		{"public /24 flagged", "8.8.8.0/24", []string{"8.8.8.0/24"}},
		{
			"mixed list flags only the public entry",
			"192.168.1.0/24,8.8.8.0/24,fd00::/8",
			[]string{"8.8.8.0/24"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := runtimeconfig.Dashboard{TrustedLANCIDRs: mustCIDRs(t, tc.cidrs)}
			got := nonPrivateTrustedLANPrefixes(cfg)
			gotStrs := make([]string, len(got))
			for i, p := range got {
				gotStrs[i] = p.String()
			}
			if len(gotStrs) != len(tc.wantFlagged) {
				t.Fatalf("nonPrivateTrustedLANPrefixes(%q) = %v, want %v", tc.cidrs, gotStrs, tc.wantFlagged)
			}
			for i, want := range tc.wantFlagged {
				if gotStrs[i] != want {
					t.Errorf("entry %d = %q, want %q", i, gotStrs[i], want)
				}
			}
		})
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
			handler := csrfProtectMiddleware(runtimeconfig.Dashboard{}, okHandler(&reached))

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
	// The trusted origins come pre-parsed in the snapshot (parsing itself is
	// covered by runtimeconfig.TestParseTrustedOrigins).
	dash := runtimeconfig.Dashboard{
		TrustedOrigins: runtimeconfig.ParseTrustedOrigins("https://miner.example.com, proxy.lan:8443"),
	}

	reached := false
	handler := csrfProtectMiddleware(dash, okHandler(&reached))

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
	dash := runtimeconfig.Dashboard{Username: "admin", Password: runtimeconfig.NewSecret("secret")}

	reached := false
	handler := basicAuthMiddleware(dash, okHandler(&reached))

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
	s := newRenderServer(t)
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
