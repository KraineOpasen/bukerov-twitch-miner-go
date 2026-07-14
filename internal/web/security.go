package web

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// This file implements the dashboard's exposure model:
//
//   - The server binds to loopback by default; a non-loopback bind must be
//     requested explicitly (analytics.host in config.json, or the
//     DASHBOARD_HOST env var, which the Docker image sets to 0.0.0.0 so
//     published container ports keep working).
//   - A non-loopback bind requires Basic Auth credentials
//     (DASHBOARD_USERNAME/DASHBOARD_PASSWORD). Without them startup fails
//     with an actionable error instead of silently serving an open
//     dashboard; DASHBOARD_INSECURE_NO_AUTH=true is the explicit opt-out
//     for trusted-LAN setups.
//   - State-changing requests must be same-origin: browsers attach Basic
//     Auth credentials to cross-site requests automatically, so auth alone
//     does not stop CSRF. The check is stateless (no cookies/sessions
//     exist here): Sec-Fetch-Site when the browser sends it, otherwise
//     Origin/Referer matched against the request Host.
//     DASHBOARD_TRUSTED_ORIGINS extends the allowlist for reverse proxies
//     that rewrite Host.

// resolveBindHost returns the effective bind host and where it came from.
// The DASHBOARD_HOST env var takes precedence over the config value but is
// never written back to config.json, so a container-supplied override can't
// leak into the user's persisted settings.
func resolveBindHost(configHost string) (host, source string) {
	if env := strings.TrimSpace(os.Getenv("DASHBOARD_HOST")); env != "" {
		return env, "DASHBOARD_HOST env"
	}
	return configHost, "config analytics.host"
}

// isLoopbackHost reports whether the bind host is only reachable from the
// local machine. An empty host binds all interfaces, so it is not loopback.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func insecureNoAuthAllowed() bool {
	v, err := strconv.ParseBool(os.Getenv("DASHBOARD_INSECURE_NO_AUTH"))
	return err == nil && v
}

// validateBindSecurity is the fail-closed gate run before the dashboard
// starts listening: loopback binds are always fine, non-loopback binds need
// credentials or the explicit DASHBOARD_INSECURE_NO_AUTH opt-out.
func validateBindSecurity(host string) error {
	if isLoopbackHost(host) {
		return nil
	}
	if authEnabled() {
		return nil
	}
	if insecureNoAuthAllowed() {
		slog.Warn("DASHBOARD_INSECURE_NO_AUTH=true: dashboard is reachable on a non-loopback address WITHOUT authentication",
			"host", host)
		return nil
	}
	return fmt.Errorf("refusing to serve the dashboard on non-loopback address %q without authentication: "+
		"set the DASHBOARD_USERNAME and DASHBOARD_PASSWORD environment variables, "+
		"bind to 127.0.0.1 instead (analytics.host in config.json, or the DASHBOARD_HOST environment variable), "+
		"or explicitly accept an unauthenticated dashboard with DASHBOARD_INSECURE_NO_AUTH=true", host)
}

// contentSecurityPolicy allows only same-origin scripts/styles (plus inline,
// which the templates and vendored htmx/ApexCharts rely on), same-origin
// fetch/SSE, and https images (Twitch CDN box art, emotes, avatars).
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self' 'unsafe-inline'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob: https:; " +
	"connect-src 'self'; " +
	"frame-ancestors 'none'; " +
	"base-uri 'self'; " +
	"form-action 'self'; " +
	"object-src 'none'"

func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		next.ServeHTTP(w, r)
	})
}

// csrfProtectMiddleware rejects state-changing requests whose browser-supplied
// provenance (Sec-Fetch-Site, Origin, Referer) points at a different origin.
// Safe methods pass through untouched, so the SSE stream (a GET) is never
// affected. Requests carrying none of those headers pass too: they come from
// non-browser clients (curl, scripts), which are not CSRF vectors.
func csrfProtectMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if err := checkSameOrigin(r); err != nil {
			slog.Warn("Blocked cross-origin state-changing request",
				"method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr, "reason", err.Error())
			http.Error(w, "Forbidden: cross-origin request blocked", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func checkSameOrigin(r *http.Request) error {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		if site == "same-origin" || site == "none" {
			return nil
		}
		return fmt.Errorf("Sec-Fetch-Site is %q", site)
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		return matchesRequestHost(origin, r.Host)
	}
	if referer := r.Header.Get("Referer"); referer != "" {
		return matchesRequestHost(referer, r.Host)
	}
	return nil
}

func matchesRequestHost(rawURL, requestHost string) error {
	if rawURL == "null" {
		return fmt.Errorf(`opaque "null" origin`)
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("unparseable origin %q", rawURL)
	}
	if strings.EqualFold(u.Host, requestHost) {
		return nil
	}
	for _, trusted := range trustedOriginHosts() {
		if strings.EqualFold(u.Host, trusted) {
			return nil
		}
	}
	return fmt.Errorf("origin host %q does not match request host %q", u.Host, requestHost)
}

// trustedOriginHosts parses the DASHBOARD_TRUSTED_ORIGINS env var: a
// comma-separated list of extra allowed origins for setups where a reverse
// proxy rewrites the Host header. Entries may be full origins
// ("https://miner.example.com") or bare host[:port] values.
func trustedOriginHosts() []string {
	raw := os.Getenv("DASHBOARD_TRUSTED_ORIGINS")
	if raw == "" {
		return nil
	}
	var hosts []string
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if u, err := url.Parse(entry); err == nil && u.Host != "" {
			hosts = append(hosts, u.Host)
			continue
		}
		hosts = append(hosts, entry)
	}
	return hosts
}
