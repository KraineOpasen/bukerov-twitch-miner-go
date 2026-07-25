package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/runtimeconfig"
)

// The healthcheck no longer reads the process environment: the dashboard
// exposure/auth values are passed in as a resolved runtimeconfig.Dashboard, so
// these tests construct it explicitly and set no env.

func writeConfig(t *testing.T, enableAnalytics bool, host string, port int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := fmt.Sprintf(`{"username":"u","enableAnalytics":%v,"streamers":[{"username":"s"}],"analytics":{"host":%q,"port":%d}}`,
		enableAnalytics, host, port)
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func serverPort(t *testing.T, srv *httptest.Server) (string, int) {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return u.Hostname(), port
}

func TestRunHealthcheckHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/status" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	host, port := serverPort(t, srv)
	cfgPath := writeConfig(t, true, host, port)

	if code := runHealthcheck(cfgPath, runtimeconfig.Dashboard{}); code != 0 {
		t.Fatalf("healthy server: exit code = %d, want 0", code)
	}
}

func TestRunHealthcheckUnhealthyStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	host, port := serverPort(t, srv)
	cfgPath := writeConfig(t, true, host, port)

	if code := runHealthcheck(cfgPath, runtimeconfig.Dashboard{}); code != 1 {
		t.Fatalf("500 server: exit code = %d, want 1", code)
	}
}

func TestRunHealthcheckUnreachable(t *testing.T) {
	// A closed server: connection refused.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	host, port := serverPort(t, srv)
	srv.Close()

	cfgPath := writeConfig(t, true, host, port)
	if code := runHealthcheck(cfgPath, runtimeconfig.Dashboard{}); code != 1 {
		t.Fatalf("unreachable server: exit code = %d, want 1", code)
	}
}

func TestRunHealthcheckAnalyticsDisabled(t *testing.T) {
	cfgPath := writeConfig(t, false, "127.0.0.1", 5000)
	if code := runHealthcheck(cfgPath, runtimeconfig.Dashboard{}); code != 0 {
		t.Fatalf("analytics disabled: exit code = %d, want 0", code)
	}
}

func TestRunHealthcheckMissingConfig(t *testing.T) {
	if code := runHealthcheck(filepath.Join(t.TempDir(), "nope.json"), runtimeconfig.Dashboard{}); code != 1 {
		t.Fatal("missing config must be unhealthy (exit 1)")
	}
}

func TestRunHealthcheckSendsBasicAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "admin" || pass != "secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	host, port := serverPort(t, srv)
	cfgPath := writeConfig(t, true, host, port)

	dash := runtimeconfig.Dashboard{Username: "admin", Password: runtimeconfig.NewSecret("secret")}
	if code := runHealthcheck(cfgPath, dash); code != 0 {
		t.Fatalf("auth-protected server with resolved creds: exit code = %d, want 0", code)
	}
}

func TestHealthcheckURL(t *testing.T) {
	cases := []struct {
		host         string
		hostOverride string
		want         string
	}{
		{"0.0.0.0", "", "http://127.0.0.1:5000/api/status"},
		{"", "", "http://127.0.0.1:5000/api/status"},
		{"127.0.0.1", "", "http://127.0.0.1:5000/api/status"},
		{"192.168.1.20", "", "http://192.168.1.20:5000/api/status"},
		{"127.0.0.1", "0.0.0.0", "http://127.0.0.1:5000/api/status"},
		{"192.168.1.20", "127.0.0.1", "http://127.0.0.1:5000/api/status"},
	}
	for _, tc := range cases {
		if got := healthcheckURL(tc.hostOverride, tc.host, 5000); got != tc.want {
			t.Errorf("healthcheckURL(override=%q, config=%q) = %q, want %q", tc.hostOverride, tc.host, got, tc.want)
		}
	}
}
