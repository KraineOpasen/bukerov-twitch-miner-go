// Package runtimeconfig is the single, deterministic boundary where the
// process resolves its startup inputs — CLI flags and environment variables —
// into an immutable, typed runtime configuration snapshot (BKM-021).
//
// # Three configurations, deliberately kept apart
//
//   - Persisted user configuration lives in internal/config (config.json plus
//     the settings the dashboard edits at runtime). It is legitimately mutable
//     and persisted, and it is NOT this package's concern.
//   - Startup inputs are the raw CLI flags and environment variables the
//     process is launched with. They are captured by Flags and read through a
//     Lookup.
//   - The runtime configuration snapshot (RuntimeConfig) is the normalized,
//     validated, typed, immutable value produced from those inputs by Resolve.
//     After Resolve it never changes: services receive it by value and can
//     read it concurrently without synchronization.
//
// # Why this exists
//
// Before BKM-021 the process-level environment was read ad hoc from deep
// inside runtime components — the web package re-read DASHBOARD_USERNAME /
// DASHBOARD_PASSWORD on every HTTP request, re-parsed DASHBOARD_TRUSTED_ORIGINS
// on every state-changing request, and main.go / the web / security layers each
// reached for os.Getenv independently. That scattered reading made the config
// boundary non-deterministic (a service could, in principle, observe a changed
// process environment mid-run) and hard to test (every test had to mutate the
// real process environment with t.Setenv).
//
// Resolve reads the environment exactly once, at the cmd/miner bootstrap, and
// hands every downstream service a value snapshot. No production runtime
// package reaches for os.Getenv for these inputs any more; the environment is
// read only here (through the injected Lookup) and, for the persisted config
// file's own env overlay, in internal/config — the two sanctioned boundaries.
//
// # Not in scope
//
// Two subsystem-local secret reads deliberately stay at their own construction
// boundaries because relocating them would change observable behavior and is a
// separate, behavior-sensitive change:
//
//   - internal/auth reads TWITCH_AUTH_ENCRYPTION_KEY at token-at-rest I/O. It is
//     a global secret consumed only inside the auth persistence path.
//   - internal/notifications reads the per-provider push secrets
//     (MATRIX_*/PUSHOVER_*/GOTIFY_*/WEBHOOK_*, with a "<BASE>_<USERNAME>"
//     per-account override). The override binds to the RUNTIME-RECONCILED owner
//     login, which an owner-rename can change during start-up before the
//     notification manager is built — so its resolution point cannot move to
//     bootstrap without altering behavior for renamed owners.
//
// internal/config keeps reading DISCORD_BOT_TOKEN in LoadConfig: that IS the
// config-file boundary (it produces the persisted config, and SaveConfig's
// never-persist handling is coupled to it), which this package's contract
// explicitly permits.
package runtimeconfig

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/updater"
)

// Environment variable names read by Resolve. They are collected here so the
// full set of process-level inputs this boundary owns is visible in one place.
const (
	envAutoUpdate            = "AUTO_UPDATE"
	envAutoUpdateInterval    = "AUTO_UPDATE_CHECK_INTERVAL"
	envDashboardHost         = "DASHBOARD_HOST"
	envDashboardUsername     = "DASHBOARD_USERNAME"
	envDashboardPassword     = "DASHBOARD_PASSWORD"
	envDashboardInsecure     = "DASHBOARD_INSECURE_NO_AUTH"
	envDashboardTrustedOrigs = "DASHBOARD_TRUSTED_ORIGINS"
	envDevPredictions        = "MINER_DEV_PREDICTIONS"
)

// Lookup abstracts reading a single environment variable. It mirrors
// os.LookupEnv's (value, present) contract so Resolve can be unit-tested with a
// map instead of mutating the real process environment. Production code passes
// OSLookup.
type Lookup func(key string) (value string, ok bool)

// OSLookup is the production Lookup backed by the process environment.
func OSLookup(key string) (string, bool) {
	return os.LookupEnv(key)
}

// get returns the environment value for key or "" when unset, matching the
// os.Getenv semantics the pre-BKM-021 call sites relied on.
func (l Lookup) get(key string) string {
	v, _ := l(key)
	return v
}

// Flags carries the parsed CLI flags. main owns flag definition and parsing;
// this struct is the typed hand-off into Resolve so flag/env precedence lives
// in exactly one place.
type Flags struct {
	// ConfigPath is the -config value (path to config.json).
	ConfigPath string
	// Debug is the -debug value (force DEBUG log levels).
	Debug bool
	// AutoUpdate is the -auto-update value. It participates in the auto-update
	// precedence resolved below.
	AutoUpdate bool
	// GenerateConfig is the -generate-config mode flag.
	GenerateConfig bool
	// Healthcheck is the -healthcheck mode flag.
	Healthcheck bool
}

// Dashboard is the immutable, environment-derived slice of the web dashboard's
// exposure and authentication configuration. Every field is fully resolved at
// bootstrap: strings are captured verbatim (or trimmed where the pre-BKM-021
// code trimmed), the trusted-origin list is already parsed, and the booleans
// are already decided — so no web request path re-reads the environment or
// re-parses a string.
type Dashboard struct {
	// HostOverride is the trimmed DASHBOARD_HOST value, or "" when unset. It
	// overrides config.analytics.host for the effective bind address but is
	// never persisted back to config.json.
	HostOverride string
	// Username / Password are the DASHBOARD_USERNAME / DASHBOARD_PASSWORD Basic
	// Auth credentials, captured verbatim (no trimming — a credential may be
	// intentionally surrounded by spaces). Password is a secret and is redacted
	// by LogValue / String.
	Username string
	Password string
	// InsecureNoAuth is the resolved DASHBOARD_INSECURE_NO_AUTH opt-out: true
	// only when the value parses as a truthy bool, matching strconv.ParseBool.
	InsecureNoAuth bool
	// TrustedOrigins is the parsed DASHBOARD_TRUSTED_ORIGINS allowlist (bare
	// host[:port] values). It is stored as an already-parsed slice so the CSRF
	// check never re-parses per request; treat it as read-only.
	TrustedOrigins []string
	// DevPredictions is the resolved MINER_DEV_PREDICTIONS switch enabling the
	// local prediction simulator. Off unless explicitly set truthy.
	DevPredictions bool
}

// AuthEnabled reports whether Basic Auth is configured (both credentials set).
func (d Dashboard) AuthEnabled() bool {
	return d.Username != "" && d.Password != ""
}

// TrustedOriginHosts returns a defensive copy of the parsed trusted-origin
// hosts, so a caller can never mutate the snapshot's internal slice.
func (d Dashboard) TrustedOriginHosts() []string {
	if len(d.TrustedOrigins) == 0 {
		return nil
	}
	out := make([]string, len(d.TrustedOrigins))
	copy(out, d.TrustedOrigins)
	return out
}

// LogValue redacts the Basic Auth password so a Dashboard is safe to log.
func (d Dashboard) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("hostOverride", d.HostOverride),
		slog.String("username", d.Username),
		slog.String("password", redact(d.Password)),
		slog.Bool("insecureNoAuth", d.InsecureNoAuth),
		slog.Int("trustedOrigins", len(d.TrustedOrigins)),
		slog.Bool("devPredictions", d.DevPredictions),
	)
}

// RuntimeConfig is the immutable, typed process-level runtime configuration
// snapshot. It is produced once by Resolve and then only read. It is a value
// type with no exported setters and no shared mutable maps: copying it is
// enough to hand a service its own safe view, and its only slice
// (Dashboard.TrustedOrigins) is exposed for mutation only through the
// defensive-copying accessor.
type RuntimeConfig struct {
	// ConfigPath is the resolved path to config.json (from -config).
	ConfigPath string
	// Debug forces DEBUG log levels (from -debug).
	Debug bool
	// AutoUpdateEnabled is the resolved self-update switch: the -auto-update
	// flag OR a truthy AUTO_UPDATE environment value (flag wins when set).
	AutoUpdateEnabled bool
	// AutoUpdateInterval is the resolved release-check cadence, already parsed
	// and clamped by updater.ParseCheckInterval from AUTO_UPDATE_CHECK_INTERVAL.
	AutoUpdateInterval time.Duration
	// Dashboard is the resolved web dashboard exposure/auth configuration.
	Dashboard Dashboard
}

// LogValue redacts nested secrets (the dashboard password) so a RuntimeConfig
// is safe to pass to slog.
func (rc RuntimeConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("configPath", rc.ConfigPath),
		slog.Bool("debug", rc.Debug),
		slog.Bool("autoUpdateEnabled", rc.AutoUpdateEnabled),
		slog.Duration("autoUpdateInterval", rc.AutoUpdateInterval),
		// Embed the already-redacted dashboard group directly (rather than
		// slog.Any of the struct) so the secret is redacted even when the value
		// is rendered without a LogValuer-resolving handler.
		slog.Attr{Key: "dashboard", Value: rc.Dashboard.LogValue()},
	)
}

// String redacts secrets so a RuntimeConfig printed with %v / %s never leaks
// the dashboard password.
func (rc RuntimeConfig) String() string {
	return fmt.Sprintf(
		"RuntimeConfig{configPath:%q debug:%t autoUpdate:%t interval:%s "+
			"dashboard:{hostOverride:%q username:%q password:%s insecureNoAuth:%t trustedOrigins:%d devPredictions:%t}}",
		rc.ConfigPath, rc.Debug, rc.AutoUpdateEnabled, rc.AutoUpdateInterval,
		rc.Dashboard.HostOverride, rc.Dashboard.Username, redact(rc.Dashboard.Password),
		rc.Dashboard.InsecureNoAuth, len(rc.Dashboard.TrustedOrigins), rc.Dashboard.DevPredictions,
	)
}

// Resolve normalizes the CLI flags and environment (read through env) into the
// immutable RuntimeConfig snapshot. It is pure and total: every input has a
// defined fallback, so there is nothing left to fail — the returned snapshot is
// valid by construction. The environment is read ONLY here, and only through
// the injected Lookup.
//
// Precedence is preserved exactly as it was before BKM-021:
//
//   - AutoUpdateEnabled: -auto-update flag OR a truthy AUTO_UPDATE env value.
//   - AutoUpdateInterval: AUTO_UPDATE_CHECK_INTERVAL via updater.ParseCheckInterval.
//   - Dashboard.HostOverride: DASHBOARD_HOST (trimmed); overrides config.analytics.host
//     at the web boundary, never persisted.
//   - Dashboard.Username/Password: DASHBOARD_USERNAME/DASHBOARD_PASSWORD (verbatim).
//   - Dashboard.InsecureNoAuth: DASHBOARD_INSECURE_NO_AUTH (strconv.ParseBool).
//   - Dashboard.TrustedOrigins: DASHBOARD_TRUSTED_ORIGINS (parsed here).
//   - Dashboard.DevPredictions: MINER_DEV_PREDICTIONS truthy set.
func Resolve(flags Flags, env Lookup) RuntimeConfig {
	return RuntimeConfig{
		ConfigPath:         flags.ConfigPath,
		Debug:              flags.Debug,
		AutoUpdateEnabled:  resolveAutoUpdateEnabled(flags.AutoUpdate, env.get(envAutoUpdate)),
		AutoUpdateInterval: updater.ParseCheckInterval(env.get(envAutoUpdateInterval)),
		Dashboard: Dashboard{
			HostOverride:   strings.TrimSpace(env.get(envDashboardHost)),
			Username:       env.get(envDashboardUsername),
			Password:       env.get(envDashboardPassword),
			InsecureNoAuth: parseBool(env.get(envDashboardInsecure)),
			TrustedOrigins: ParseTrustedOrigins(env.get(envDashboardTrustedOrigs)),
			DevPredictions: parseDevPredictions(env.get(envDevPredictions)),
		},
	}
}

// resolveAutoUpdateEnabled preserves the pre-BKM-021 precedence from main's
// autoUpdateEnabled: the flag forces it on; otherwise a parseable AUTO_UPDATE
// bool decides (an unset or unparseable value leaves it off).
func resolveAutoUpdateEnabled(flag bool, envValue string) bool {
	if flag {
		return true
	}
	if v, err := strconv.ParseBool(envValue); err == nil {
		return v
	}
	return false
}

// parseBool mirrors the web layer's insecureNoAuthAllowed: true only when the
// value parses as a truthy bool; any parse error (including an empty value)
// yields false.
func parseBool(v string) bool {
	b, err := strconv.ParseBool(v)
	return err == nil && b
}

// parseDevPredictions mirrors the web layer's devPredictionsEnabled truthy set
// exactly, so the dev simulator activates under precisely the same values.
func parseDevPredictions(v string) bool {
	switch v {
	case "1", "true", "TRUE", "yes", "on":
		return true
	default:
		return false
	}
}

// ParseTrustedOrigins parses a DASHBOARD_TRUSTED_ORIGINS value: a
// comma-separated list of extra allowed origins for setups where a reverse
// proxy rewrites the Host header. Entries may be full origins
// ("https://miner.example.com") or bare host[:port] values; each is reduced to
// its host[:port]. This is the single parser for that variable (previously in
// web.trustedOriginHosts), so the CSRF check consumes an already-parsed slice.
func ParseTrustedOrigins(raw string) []string {
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

// redact hides a secret's value while still signalling whether one was set, so
// logs distinguish "no password configured" from "password configured".
func redact(secret string) string {
	if secret == "" {
		return ""
	}
	return "***"
}
