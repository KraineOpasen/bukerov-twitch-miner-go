package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/runtimeconfig"
)

// healthcheckTimeout bounds the whole probe; Docker's HEALTHCHECK has its own
// --timeout on top, this just keeps the process from hanging on its own.
const healthcheckTimeout = 5 * time.Second

// runHealthcheck implements the -healthcheck flag: a self-contained liveness
// probe for container HEALTHCHECKs (the scratch image has no shell or curl).
// It loads the same config the miner runs with and probes the dashboard's
// /api/status endpoint. Returns a process exit code: 0 healthy, 1 unhealthy.
//
// With analytics disabled there is no HTTP surface to probe, so the check
// degrades to "the container's main process is running" — which is exactly
// what being able to execute this subcommand demonstrates — and reports
// healthy.
//
// The dashboard exposure/auth values come from the same immutable snapshot the
// server binds with (resolved once at bootstrap), so the probe targets exactly
// the host the server binds and presents the same Basic Auth credentials.
func runHealthcheck(configPath string, dash runtimeconfig.Dashboard) int {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: cannot read config %s: %v\n", configPath, err)
		return 1
	}
	if !cfg.EnableAnalytics {
		fmt.Println("healthcheck: analytics disabled, nothing to probe")
		return 0
	}

	url := healthcheckURL(dash.HostOverride, cfg.Analytics.Host, cfg.Analytics.Port)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	// The dashboard may require Basic Auth; present the resolved credentials.
	if dash.AuthEnabled() {
		req.SetBasicAuth(dash.Username, dash.Password)
	}

	client := &http.Client{Timeout: healthcheckTimeout}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %s unreachable: %v\n", url, err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: %s returned %d\n", url, resp.StatusCode)
		return 1
	}
	fmt.Printf("healthcheck: %s OK\n", url)
	return 0
}

// healthcheckURL picks the address to probe. Wildcard and loopback binds are
// reachable via 127.0.0.1; a bind to one specific address is probed directly.
// The DASHBOARD_HOST override (already resolved into hostOverride) participates
// with the same precedence the server applies (env over config).
func healthcheckURL(hostOverride, configHost string, port int) string {
	host := configHost
	if hostOverride != "" {
		host = hostOverride
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/api/status"
}
