package web

// Task S5-5: three direct server-rendered System pages —
// /system/status, /system/diagnostics, /system/logs — replacing the three
// former /system/* compatibility redirects to /health and /logs
// (handlers_chrome.go). All three are pure server-render: no hx-get, no
// polling, no new API endpoint. /system/status and /system/diagnostics are
// STATUS-ONLY: they read existing in-memory providers (lifecycle, health,
// drops sync, drops-progress watchdog, resource sampler, updater state) and
// never render a settings form or a lifecycle mutation control — every
// existing config surface (Health Center's canary/watchdog forms, the
// lifecycle panel's Pause/Resume/Restart/Stop) stays exactly where it is
// today. /system/logs reuses the /logs transport and template wholesale via
// the shared buildLogsPageData builder in handlers_logs.go.
//
// Honesty rules threaded through every row builder below: an absent/never-
// checked/never-synced signal renders as "unknown"/"unavailable"
// (health-sev-neutral) — NEVER as healthy/ok (health-sev-ok). A freshness
// clock is rendered only when its source timestamp is non-zero. Any
// LastError surfaced from lifecycle or drops-sync crosses the boundary only
// through supportbundle.Redact — the same defense-in-depth sanitizer the
// support bundle already relies on; this file never reimplements or copies
// its regexes.

import (
	"fmt"
	"net/http"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/health"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/lifecycle"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/resources"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/supportbundle"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version"
)

// systemDash is the honest absence marker rendered whenever a value is not
// currently available — NEVER a fabricated 0.
const systemDash = "—" // "—"

// SystemStatusRowView is one status row shared by /system/status and
// /system/diagnostics: a label, a severity-classed status text, and up to
// two independent freshness clocks (a signal has at most one; the
// lifecycle and drops-sync rows each carry two DISTINCT clocks — e.g.
// attempt vs success — that must never be merged or substituted for one
// another). Secondary is an optional extra evidence line (the GQL row's
// "Active client ID"); Detail is an optional, already-redacted explanatory
// line (a lifecycle/drops-sync LastError).
type SystemStatusRowView struct {
	Label      string
	StatusText string
	// Sev is one of the four existing health-sev-{ok,bad,warn,neutral}
	// classes (already styled in app.css) — health-sev-ok is used ONLY for
	// genuinely OK evidence; every unknown/unavailable/never-synced state
	// uses health-sev-neutral, never ok.
	Sev string

	ClockLabel string
	ClockText  string

	Clock2Label string
	Clock2Text  string

	Secondary string
	Detail    string
}

// SystemResourceRowView is one process/container resource metric line.
// Primary/Secondary render as systemDash ("—") — never a fabricated 0 —
// when the metric (or the whole snapshot) is unavailable.
type SystemResourceRowView struct {
	Label     string
	Primary   string
	Secondary string
}

// SystemResourcesView is the process/container resource mini-table on
// /system/status. Always exactly 4 rows (CPU, Memory, Network, Disk), in
// that fixed order — this is explicitly a PROCESS/CONTAINER view (the same
// sampler backing the Overview mini-widgets), never framed as whole-host.
type SystemResourcesView struct {
	Rows []SystemResourceRowView
}

// SystemDropProgressView is the drops-progress watchdog section on
// /system/diagnostics. Available is false only when no DropProgressProvider
// is wired at all. HealthyCount/RecoveringCount/StalledCount are simple,
// independent per-status counts — deliberately not merged into any invented
// aggregate ratio.
type SystemDropProgressView struct {
	Available bool
	Enabled   bool

	ClockLabel string
	ClockText  string

	HasAnyDrops     bool
	HealthyCount    int
	RecoveringCount int
	StalledCount    int
}

// SystemUpdateView is the best-effort auto-updater evidence on
// /system/diagnostics. State is "" (nothing observed — render no positive
// row at all, never "up to date"), "available", "failed", or "applied" —
// mirrors LifecycleUpdateState exactly (handlers_lifecycle.go).
type SystemUpdateView struct {
	State   string
	Version string
}

// handleSystemStatusPage renders /system/status: STATUS-ONLY, no htmx, no
// polling, no new API endpoint.
func (s *Server) handleSystemStatusPage(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, r, "system_status.html", s.buildSystemStatusPageData(r))
}

// handleSystemDiagnosticsPage renders /system/diagnostics: pure
// server-render (no auto-refresh); the page copy instructs the user to
// reload for current status. The one exception to "no new request" is the
// Run Canary action, which POSTs to the existing, unconditionally-registered
// /api/health/canary/run with hx-swap="none" (its legacy partial response is
// discarded, never inserted).
func (s *Server) handleSystemDiagnosticsPage(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, r, "system_diagnostics.html", s.buildSystemDiagnosticsPageData(r))
}

// handleSystemLogsPage renders /system/logs: the exact same LogsPageData and
// "logs.html" template as the canonical /logs route (see
// handlers_logs.go's buildLogsPageData), so both stay byte-for-byte
// identical in every control they share.
func (s *Server) handleSystemLogsPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/system/logs" {
		http.NotFound(w, r)
		return
	}
	s.renderPage(w, r, "logs.html", s.buildLogsPageData())
}

// systemTr returns a translation closure bound to the request's language,
// the small tr() shim this file needs for every localized string below.
func (s *Server) systemTr(r *http.Request) func(string) string {
	lang := s.langFromRequest(r)
	return func(key string) string { return s.i18n.T(lang, key) }
}

// systemHealthSnapshot reads the Health Center's aggregated snapshot. A nil
// healthProvider (never wired) degrades to a zero Snapshot — every signal
// lookup below then honestly resolves to "unknown", exactly like a wired
// provider that has simply never recorded that signal yet.
func (s *Server) systemHealthSnapshot() health.Snapshot {
	s.mu.RLock()
	provider := s.healthProvider
	s.mu.RUnlock()
	if provider == nil {
		return health.Snapshot{}
	}
	return provider.HealthSnapshot()
}

// buildSystemStatusPageData assembles /system/status's view: the lifecycle,
// OAuth, GQL-API, PubSub and drops-sync rows, and the process/container
// resource mini-table.
func (s *Server) buildSystemStatusPageData(r *http.Request) SystemStatusPageData {
	tr := s.systemTr(r)

	s.mu.RLock()
	refresh := s.refresh
	discordEnabled := s.discordEnabled
	debugURL := s.debugURL
	resourceFn := s.resourceSnapshot
	s.mu.RUnlock()

	healthSnap := s.systemHealthSnapshot()
	checkLabel := tr("system.status.signal.last_check_label")
	oauthSig, _ := healthSnap.Signal(health.SignalOAuth)
	pubsubSig, _ := healthSnap.Signal(health.SignalPubSub)

	return SystemStatusPageData{
		Username:       s.username,
		RefreshMinutes: refresh,
		Version:        version.Version,
		DiscordEnabled: discordEnabled,
		DebugURL:       debugURL,

		Signals: []SystemStatusRowView{
			s.buildSystemLifecycleRow(tr),
			systemSignalRow(oauthSig, tr("system.status.oauth.label"), checkLabel, tr),
			buildSystemGQLRow(healthSnap, checkLabel, tr),
			systemSignalRow(pubsubSig, tr("system.status.pubsub.label"), checkLabel, tr),
			s.buildSystemDropsSyncRow(tr),
		},
		Resources: buildSystemResourcesView(resourceFn, tr),
	}
}

// buildSystemDiagnosticsPageData assembles /system/diagnostics's view: the
// watch-transport canary signal, the drops-progress watchdog counts, the
// current version, best-effort update evidence, and the support-bundle
// availability flag.
func (s *Server) buildSystemDiagnosticsPageData(r *http.Request) SystemDiagnosticsPageData {
	tr := s.systemTr(r)

	s.mu.RLock()
	refresh := s.refresh
	discordEnabled := s.discordEnabled
	debugURL := s.debugURL
	dropProgressProvider := s.dropProgressProvider
	updFn := s.lifecycleUpdateState
	authEnabled := s.dashboard.AuthEnabled()
	s.mu.RUnlock()

	healthSnap := s.systemHealthSnapshot()
	watchSig, _ := healthSnap.Signal(health.SignalWatchTransport)
	checkLabel := tr("system.status.signal.last_check_label")

	return SystemDiagnosticsPageData{
		Username:       s.username,
		RefreshMinutes: refresh,
		Version:        version.Version,
		DiscordEnabled: discordEnabled,
		DebugURL:       debugURL,

		Signals: []SystemStatusRowView{
			systemSignalRow(watchSig, tr("health.canary.title"), checkLabel, tr),
		},
		DropProgress: buildSystemDropProgressView(dropProgressProvider, tr),
		Update:       buildSystemUpdateView(updFn),

		SupportBundleAvailable: authEnabled,
	}
}

// systemSignalRow builds a status row from one health.Signal. sig.Status=="" —
// whether because no provider is wired, or a wired provider has never
// recorded this signal — always resolves to "unknown", never ok/healthy.
func systemSignalRow(sig health.Signal, label, clockLabel string, tr func(string) string) SystemStatusRowView {
	row := SystemStatusRowView{
		Label:      label,
		StatusText: systemHealthStatusText(sig.Status, tr),
		Sev:        systemHealthSeverity(sig.Status),
	}
	if !sig.CheckedAt.IsZero() {
		row.ClockLabel = clockLabel
		row.ClockText = systemAgo(sig.CheckedAt)
	}
	return row
}

// buildSystemGQLRow is systemSignalRow for the GQL-API signal specifically,
// plus its secondary evidence line (the active GQL client ID) when known.
func buildSystemGQLRow(snap health.Snapshot, clockLabel string, tr func(string) string) SystemStatusRowView {
	sig, _ := snap.Signal(health.SignalGQLAPI)
	row := systemSignalRow(sig, tr("system.status.gql.label"), clockLabel, tr)
	if snap.ActiveClientID != "" {
		row.Secondary = tr("health.active_client_id") + " " + snap.ActiveClientID
	}
	return row
}

// systemHealthStatusText mirrors healthStatusDisplay's status->text mapping
// (handlers_health.go:63-78) but is its own independent implementation, kept
// local to this file rather than calling into handlers_health.go: any
// status this file doesn't explicitly recognize (including "", the
// never-checked case) honestly resolves to "unknown" — never ok.
func systemHealthStatusText(status string, tr func(string) string) string {
	switch status {
	case health.StatusOK:
		return tr("health.status.ok")
	case health.StatusFailed:
		return tr("health.status.failed")
	case health.StatusDegraded:
		return tr("health.status.degraded")
	case health.StatusStalled:
		return tr("health.status.stalled")
	case health.StatusIdle:
		return tr("health.status.idle")
	default:
		return tr("health.status.unknown")
	}
}

// systemHealthSeverity maps a health.Status to one of the four existing
// health-sev-* CSS classes (already styled in app.css). Idle and Unknown
// both fall into the neutral bucket — textually distinct (via
// systemHealthStatusText) but never "ok", exactly mirroring health.html's
// own established precedent (see TestF3HealthCardsRenderStageDetailErrorCodeAndSeverity).
func systemHealthSeverity(status string) string {
	switch status {
	case health.StatusOK:
		return "health-sev-ok"
	case health.StatusFailed, health.StatusStalled:
		return "health-sev-bad"
	case health.StatusDegraded:
		return "health-sev-warn"
	default:
		return "health-sev-neutral"
	}
}

// buildSystemLifecycleRow reads the lifecycle controller's snapshot (nil ->
// an explicit unavailable/unknown row, never healthy/green). Freshness
// clocks (Started / Transition started) are independent and each rendered
// only when their source timestamp is non-zero; LastError crosses the
// boundary only through supportbundle.Redact.
func (s *Server) buildSystemLifecycleRow(tr func(string) string) SystemStatusRowView {
	s.mu.RLock()
	ctrl := s.lifecycleController
	s.mu.RUnlock()

	row := SystemStatusRowView{Label: tr("system.status.lifecycle.label")}
	if ctrl == nil {
		row.StatusText = tr("system.status.unavailable")
		row.Sev = "health-sev-neutral"
		return row
	}

	snap := ctrl.Snapshot()
	row.StatusText = tr("lc.state." + string(snap.Observed))
	row.Sev = systemLifecycleSeverity(snap.Observed)
	if !snap.StartedAt.IsZero() {
		row.ClockLabel = tr("system.status.lifecycle.started_label")
		row.ClockText = systemAgo(snap.StartedAt)
	}
	if !snap.TransitionStartedAt.IsZero() {
		row.Clock2Label = tr("system.status.lifecycle.transition_started_label")
		row.Clock2Text = systemAgo(snap.TransitionStartedAt)
	}
	if snap.LastError != "" {
		row.Detail = tr("lc.last_error_label") + ": " + supportbundle.Redact(snap.LastError)
	}
	return row
}

// systemLifecycleSeverity maps an ObservedState to a health-sev-* class,
// mirroring lifecycle_panel.html's own badge-severity precedent (running ->
// ok, failed -> bad, degraded -> warn, every transitional/steady state in
// between -> neutral).
func systemLifecycleSeverity(observed lifecycle.ObservedState) string {
	switch observed {
	case lifecycle.ObservedRunning:
		return "health-sev-ok"
	case lifecycle.ObservedFailed:
		return "health-sev-bad"
	case lifecycle.ObservedDegraded:
		return "health-sev-warn"
	default:
		return "health-sev-neutral"
	}
}

// buildSystemDropsSyncRow reads the drops tracker's sync bookkeeping (nil
// campaignsProvider -> unavailable row). Both clocks (attempt via
// LastSyncAt, success via LastSuccessAt) are rendered independently — never
// merged, never one substituted for the other; a zero clock is simply
// absent. LastError crosses the boundary only through supportbundle.Redact.
func (s *Server) buildSystemDropsSyncRow(tr func(string) string) SystemStatusRowView {
	s.mu.RLock()
	provider := s.campaignsProvider
	s.mu.RUnlock()

	row := SystemStatusRowView{Label: tr("system.status.drops_sync.label")}
	if provider == nil {
		row.StatusText = tr("system.status.unavailable")
		row.Sev = "health-sev-neutral"
		return row
	}

	sync := provider.SyncStatus()
	switch {
	case sync.LastSyncAt.IsZero() && sync.LastSuccessAt.IsZero():
		// Never synced at all: unknown, not merely "degraded" — there is no
		// evidence of any attempt yet.
		row.StatusText = tr("health.status.unknown")
		row.Sev = "health-sev-neutral"
	case sync.LastError != "":
		row.StatusText = tr("health.status.degraded")
		row.Sev = "health-sev-warn"
	default:
		row.StatusText = tr("health.status.ok")
		row.Sev = "health-sev-ok"
	}
	if !sync.LastSyncAt.IsZero() {
		row.ClockLabel = tr("system.status.drops_sync.attempt_label")
		row.ClockText = systemAgo(sync.LastSyncAt)
	}
	if !sync.LastSuccessAt.IsZero() {
		row.Clock2Label = tr("system.status.drops_sync.success_label")
		row.Clock2Text = systemAgo(sync.LastSuccessAt)
	}
	if sync.LastError != "" {
		row.Detail = tr("lc.last_error_label") + ": " + supportbundle.Redact(sync.LastError)
	}
	return row
}

// buildSystemResourcesView reads the resource sampler's latest snapshot
// through the same safeResourceSnapshot (handlers_resources.go) the
// /api/resources endpoint uses — a nil fn (sampler not wired yet) degrades
// to resources.UnavailableSnapshot(), exactly like that endpoint. Every row
// is systemDash ("—") — never a fabricated 0 — whenever the whole snapshot,
// or that specific section, is unavailable. Labels reuse the existing
// rw.cpu/rw.memory/rw.network/rw.disk keys (the Overview mini-widgets), so
// the metric names read identically everywhere in the dashboard.
func buildSystemResourcesView(fn func() resources.Snapshot, tr func(string) string) SystemResourcesView {
	snap := resources.UnavailableSnapshot()
	if fn != nil {
		snap = safeResourceSnapshot(fn)
	}

	rows := []SystemResourceRowView{
		{Label: tr("rw.cpu"), Primary: systemDash},
		{Label: tr("rw.memory"), Primary: systemDash},
		{Label: tr("rw.network"), Primary: systemDash},
		{Label: tr("rw.disk"), Primary: systemDash},
	}
	if snap.Available {
		if snap.CPU.Available {
			rows[0].Primary = fmt.Sprintf("%.1f%%", snap.CPU.Percent)
			rows[0].Secondary = "/ " + formatSystemCores(snap.CPU.LimitCores)
		}
		if snap.Memory.Available {
			rows[1].Primary = formatSystemBytes(snap.Memory.UsedBytes)
			if snap.Memory.LimitBytes > 0 {
				rows[1].Secondary = "/ " + formatSystemBytes(snap.Memory.LimitBytes)
			}
		}
		if snap.Network.Available {
			rows[2].Primary = formatSystemRate(snap.Network.RxBytesPerSec) + " ↓"
			rows[2].Secondary = formatSystemRate(snap.Network.TxBytesPerSec) + " ↑"
		}
		if snap.Disk.Available {
			rows[3].Primary = formatSystemRate(snap.Disk.ReadBytesPerSec) + " ↓"
			rows[3].Secondary = formatSystemRate(snap.Disk.WriteBytesPerSec) + " ↑"
		}
	}
	return SystemResourcesView{Rows: rows}
}

// formatSystemBytes renders a byte count in compact IEEE-ish units
// (B/K/M/G/T/P), independent of (and deliberately not shared with)
// overview.html's client-side fmtBytes — this is a server-side render with
// no JS counterpart to stay in sync with.
func formatSystemBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := uint64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	units := "KMGTP"
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), units[exp])
}

// formatSystemRate renders a bytes-per-second rate using formatSystemBytes.
func formatSystemRate(bytesPerSec float64) string {
	if bytesPerSec <= 0 {
		return "0B/s"
	}
	return formatSystemBytes(uint64(bytesPerSec)) + "/s"
}

// formatSystemCores renders a CPU core-count limit compactly (whole numbers
// unadorned, fractional limits to one decimal place).
func formatSystemCores(cores float64) string {
	if cores <= 0 {
		return "0"
	}
	if cores == float64(int64(cores)) {
		return fmt.Sprintf("%d", int64(cores))
	}
	return fmt.Sprintf("%.1f", cores)
}

// systemAgo renders how long ago t was, compactly — independent of (not a
// call into) handlers_health.go's own formatHealthAgo, kept local to this
// file by design. Zero t (never happened) renders as "" so the caller can
// omit the clock entirely rather than claim a false freshness.
func systemAgo(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		return fmt.Sprintf("%dh %dm ago", h, m)
	}
}

// buildSystemDropProgressView reads the drops-progress watchdog's published
// per-drop state (nil provider -> unavailable). HealthyCount/
// RecoveringCount/StalledCount are simple independent counts of
// DropProgress.Status values — never an invented aggregate.
func buildSystemDropProgressView(provider DropProgressProvider, tr func(string) string) SystemDropProgressView {
	if provider == nil {
		return SystemDropProgressView{}
	}
	view := SystemDropProgressView{Available: true}
	snap := provider.DropProgress()
	view.Enabled = snap.Enabled
	if !snap.EvaluatedAt.IsZero() {
		view.ClockLabel = tr("system.diagnostics.watchdog.evaluated_label")
		view.ClockText = systemAgo(snap.EvaluatedAt)
	}
	for _, d := range snap.Drops {
		switch d.Status {
		case health.ProgressHealthy:
			view.HealthyCount++
		case health.ProgressRecovering:
			view.RecoveringCount++
		case health.ProgressStalled:
			view.StalledCount++
		}
	}
	view.HasAnyDrops = len(snap.Drops) > 0
	return view
}

// systemUpdateStateOf is this file's own nil-safe reader of the injected
// updater-state closure — a small, deliberate duplicate of
// lifecycleUpdateStateOf (handlers_lifecycle.go:84-89) so this file depends
// only on the exported LifecycleUpdateState type and the Server field, never
// on that file's own unexported helper.
func systemUpdateStateOf(fn func() LifecycleUpdateState) LifecycleUpdateState {
	if fn == nil {
		return LifecycleUpdateState{}
	}
	return fn()
}

// buildSystemUpdateView converts the injected updater-state closure into the
// diagnostics page's view. State "" means "nothing observed" — the template
// renders no positive update row at all for that case (see
// system_diagnostics.html): absence of evidence is never presented as
// "up to date".
func buildSystemUpdateView(fn func() LifecycleUpdateState) SystemUpdateView {
	upd := systemUpdateStateOf(fn)
	return SystemUpdateView(upd)
}
