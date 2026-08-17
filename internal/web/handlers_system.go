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
	"strings"
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
	// ClockAt is the raw instant ClockText was formatted from, kept so the
	// freshness cell can print the absolute wall clock next to the relative
	// age without re-deriving it. Zero means "no reading".
	ClockAt time.Time

	Clock2Label string
	Clock2Text  string
	Clock2At    time.Time

	Secondary string
	Detail    string

	// StatusBadge is the C10 badge encoding of StatusText at Sev's tier, so
	// the status reads as icon + text + colour and never colour alone
	// (Stage 4 §3 P4). Derived by systemRowBadge — never set by a row
	// builder, so no builder can pick a tier that disagrees with Sev.
	//
	// Filled by systemFinishRow, which only /system/status calls, so this
	// field is deliberately zero on /system/diagnostics — that page renders
	// the older health-card markup and must not be given a half-built badge.
	StatusBadge BadgeData

	// Freshness is the row's provenance, ALWAYS at least one entry once
	// systemFinishRow has run. /system/status renders freshness on EVERY row
	// (frozen V3 decision 10 and docs/dashboard/stage-4-visual-design-system.md
	// §11 route 23 both make per-row freshness mandatory), so a row with no
	// reading still gets an entry that says so — eliding the line would make
	// "never checked" indistinguishable from "this row has no clock concept".
	//
	// Like StatusBadge, only /system/status calls systemFinishRow, so this is
	// deliberately empty on /system/diagnostics.
	Freshness []SystemFreshnessView
}

// SystemFreshnessView is one clock's provenance as route 23 requires it:
// the C0 chip carrying the relative age (or its S-UNK variant when there is
// no reading at all), plus the "what was measured, and when" stamp beneath
// it. Rows with two independent clocks — drops sync's attempt and success —
// get two entries; they are never merged.
type SystemFreshnessView struct {
	Chip  ProvenanceChipData
	Kind  string
	Stamp string
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
	// Freshness stamps the block itself, not just the table row that points
	// at it: it is a live data region, and Stage 4 §7 S-READY requires every
	// live region to carry its own provenance. Empty only when the sampler
	// has produced no timestamp at all.
	Freshness []SystemFreshnessView
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

// buildSystemStatusPageData assembles /system/status's view: the read-only
// lifecycle echo (deliberately NOT one of the table rows), the five
// subsystem rows the table renders — OAuth, GQL-API, PubSub, drops sync and
// process/container resources — and the resource detail block those last
// two share a single sampler snapshot with.
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

	resourceSnap := resources.UnavailableSnapshot()
	if resourceFn != nil {
		resourceSnap = safeResourceSnapshot(resourceFn)
	}

	// Signals is the subsystem table's body. The lifecycle row is NOT in it
	// any more: the approved V3 composition puts a read-only lifecycle echo
	// above the table as its own band, leaving the table to the five
	// tracked subsystems (OAuth, GQL, PubSub, drops sync, resources).
	signals := []SystemStatusRowView{
		systemSignalRow(oauthSig, tr("system.status.oauth.label"), checkLabel, tr),
		buildSystemGQLRow(healthSnap, checkLabel, tr),
		systemSignalRow(pubsubSig, tr("system.status.pubsub.label"), checkLabel, tr),
		s.buildSystemDropsSyncRow(tr),
		buildSystemResourcesRow(resourceSnap, tr),
	}
	for i := range signals {
		signals[i] = systemFinishRow(signals[i], tr)
	}

	return SystemStatusPageData{
		Username:       s.username,
		RefreshMinutes: refresh,
		Version:        version.Version,
		DiscordEnabled: discordEnabled,
		DebugURL:       debugURL,

		Lifecycle: systemFinishRow(s.buildSystemLifecycleRow(tr), tr),
		Signals:   signals,
		Resources: buildSystemResourcesView(resourceSnap, tr),
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
		row.ClockText = systemAgo(sig.CheckedAt, tr)
		row.ClockAt = sig.CheckedAt
	}
	row.Detail = systemSignalDetail(sig, tr)
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

// systemRowBadge maps an already-decided health-sev-* class to the C10
// badge's (tier, icon) pair, reusing health.html's established glyph
// vocabulary (✓ / ✕ / ! / •) so the same severity reads identically on the
// legacy Health Center and here. It deliberately derives from Sev rather
// than from a raw status string: Sev is where every "unknown is never ok"
// decision has already been made (systemHealthSeverity,
// systemLifecycleSeverity, the drops-sync switch and buildSystemResourcesRow),
// so a new caller cannot accidentally reintroduce a green unknown by picking
// its own tier.
func systemRowBadge(sev, label string) BadgeData {
	switch sev {
	case "health-sev-ok":
		return BadgeData{Tier: "ok", Icon: "✓", Label: label}
	case "health-sev-bad":
		return BadgeData{Tier: "danger", Icon: "✕", Label: label}
	case "health-sev-warn":
		return BadgeData{Tier: "caution", Icon: "!", Label: label}
	default:
		return BadgeData{Tier: "neutral", Icon: "?", Label: label}
	}
}

// systemFinishRow fills the presentation-only fields every /system/status
// row needs but no row builder should have to remember: the C10 badge
// encoding and the provenance entries that keep the freshness column
// populated on every single row. It derives both from evidence the builder
// already established (StatusText, Sev, the clock fields) and never reads
// or alters that evidence, so it is idempotent.
func systemFinishRow(row SystemStatusRowView, tr func(string) string) SystemStatusRowView {
	row.StatusBadge = systemRowBadge(row.Sev, row.StatusText)

	row.Freshness = nil
	if row.ClockText != "" {
		row.Freshness = append(row.Freshness, systemFreshness(row.ClockLabel, row.ClockText, row.ClockAt))
	}
	if row.Clock2Text != "" {
		row.Freshness = append(row.Freshness, systemFreshness(row.Clock2Label, row.Clock2Text, row.Clock2At))
	}
	if len(row.Freshness) == 0 {
		// No reading at all: the C0 chip takes its S-UNK variant and the
		// stamp line says so in real text, never an empty cell.
		row.Freshness = []SystemFreshnessView{{
			Chip: ProvenanceChipData{Unknown: true},
			Kind: tr("system.status.freshness.none"),
		}}
	}
	return row
}

// systemFreshness builds one provenance entry: a live C0 chip carrying the
// relative age, the localized kind label, and the absolute wall clock the
// age was measured from. The absolute stamp is deliberately printed next to
// the age — "43s ago" alone cannot be checked against anything, whereas
// "43s ago · CHECKED 12:40:20" can.
//
// Aged is deliberately left false: flipping the chip to its S-STALE variant
// needs a staleness threshold, and this repository has an explicit
// precedent against inventing one (viewmodels_slots.go's c12PairProvenance
// refuses the same invention for watch-slot evidence). A threshold is an
// owner decision, not a rendering detail.
func systemFreshness(kind, age string, at time.Time) SystemFreshnessView {
	v := SystemFreshnessView{
		Chip: ProvenanceChipData{AgeLabel: age},
		Kind: kind,
	}
	if !at.IsZero() {
		v.Stamp = at.Format("15:04:05")
	}
	return v
}

// systemSignalDetail renders the evidence a health.Signal already carries
// but the System pages used to discard: its stage, its short human detail
// and its stable error code (the legacy /health page has always rendered all
// three — see partials/health.html). Everything crosses the boundary
// through supportbundle.Redact, matching the lifecycle/drops-sync rows;
// the health package stores no credentials, so redaction here is
// belt-and-braces rather than a correction.
//
// It is called from systemSignalRow, which BOTH System pages use, so this
// also gives /system/diagnostics' watch-transport canary row its own
// evidence line — the same discarded-evidence defect, fixed in one place
// rather than two. TestSystemDiagnosticsCanarySurfacesEvidence pins it.
func systemSignalDetail(sig health.Signal, tr func(string) string) string {
	parts := make([]string, 0, 3)
	if sig.Stage != "" {
		parts = append(parts, tr("health.card.stage")+" "+supportbundle.Redact(sig.Stage))
	}
	if sig.Detail != "" {
		parts = append(parts, supportbundle.Redact(sig.Detail))
	}
	if sig.ErrorCode != "" {
		parts = append(parts, tr("health.card.error_code")+" "+supportbundle.Redact(sig.ErrorCode))
	}
	return strings.Join(parts, " · ")
}

// buildSystemResourcesRow is the process/container resources row inside the
// subsystem table. It exists so the table's freshness column is genuinely
// total: the sampler publishes SampledAt (already part of /api/resources'
// public contract) and this page simply never read it. An unavailable
// sampler is "unknown"/neutral — never ok. An unparseable or absent
// SampledAt simply leaves the clock unset here; systemFinishRow is what
// turns that into the shared "no reading" entry.
func buildSystemResourcesRow(snap resources.Snapshot, tr func(string) string) SystemStatusRowView {
	row := SystemStatusRowView{Label: tr("system.status.resources.heading")}
	if !snap.Available {
		row.StatusText = tr("health.status.unknown")
		row.Sev = "health-sev-neutral"
		return row
	}

	// NOT a health verdict. No health provider judges the sampler, and
	// snap.Available flips true on the FIRST sample while the per-section
	// rates still need a second one — so "available" would render a green
	// OK above four em-dashes. The approved composition makes this row a
	// pointer to the values below it, which is what the evidence supports.
	row.StatusText = tr("system.status.resources.pointer")
	row.Sev = "health-sev-neutral"
	row.Detail = tr("system.status.resources.metrics")
	if sampled, err := time.Parse(time.RFC3339, snap.SampledAt); err == nil {
		row.ClockLabel = tr("system.status.resources.sampled_label")
		row.ClockText = systemAgo(sampled, tr)
		row.ClockAt = sampled
	}
	return row
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
	if snap.Desired != "" {
		row.Secondary = tr("system.status.lifecycle.desired_label") + " " + tr("lc.state."+string(snap.Desired))
	}
	if !snap.StartedAt.IsZero() {
		row.ClockLabel = tr("system.status.lifecycle.started_label")
		row.ClockText = systemAgo(snap.StartedAt, tr)
		row.ClockAt = snap.StartedAt
	}
	if !snap.TransitionStartedAt.IsZero() {
		row.Clock2Label = tr("system.status.lifecycle.transition_started_label")
		row.Clock2Text = systemAgo(snap.TransitionStartedAt, tr)
		row.Clock2At = snap.TransitionStartedAt
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
		row.ClockText = systemAgo(sync.LastSyncAt, tr)
		row.ClockAt = sync.LastSyncAt
	}
	if !sync.LastSuccessAt.IsZero() {
		row.Clock2Label = tr("system.status.drops_sync.success_label")
		row.Clock2Text = systemAgo(sync.LastSuccessAt, tr)
		row.Clock2At = sync.LastSuccessAt
	}
	if sync.LastError != "" {
		row.Detail = tr("lc.last_error_label") + ": " + supportbundle.Redact(sync.LastError)
	}
	return row
}

// buildSystemResourcesView renders the process/container resource detail
// block. Every row is systemDash ("—") — never a fabricated 0 — whenever the
// whole snapshot, or that specific section, is unavailable. Labels reuse the
// existing rw.cpu/rw.memory/rw.network/rw.disk keys (the Overview
// mini-widgets), so the metric names read identically everywhere in the
// dashboard.
//
// It takes the snapshot the caller already read rather than the provider
// func: /system/status samples ONCE per request (through the same
// safeResourceSnapshot the /api/resources endpoint uses, degrading a nil
// sampler to resources.UnavailableSnapshot()) and feeds both the subsystem
// table's resources row and this detail block from that single snapshot, so
// the freshness stamp in the table can never disagree with the values
// underneath it.
func buildSystemResourcesView(snap resources.Snapshot, tr func(string) string) SystemResourcesView {
	var view SystemResourcesView
	rows := []SystemResourceRowView{
		{Label: tr("rw.cpu"), Primary: systemDash},
		{Label: tr("rw.memory"), Primary: systemDash},
		{Label: tr("rw.network"), Primary: systemDash},
		{Label: tr("rw.disk"), Primary: systemDash},
	}
	if sampled, err := time.Parse(time.RFC3339, snap.SampledAt); err == nil {
		view.Freshness = []SystemFreshnessView{
			systemFreshness(tr("system.status.resources.sampled_label"), systemAgo(sampled, tr), sampled),
		}
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
	view.Rows = rows
	return view
}

// formatSystemBytes renders a byte count in compact IEEE-ish units
// (B/K/M/G/T/P/E), independent of (and deliberately not shared with)
// overview.html's client-side fmtBytes — this is a server-side render with
// no JS counterpart to stay in sync with.
//
// The unit table must cover every exponent the divisor loop below can reach.
// n >= 1<<60 drives exp to 5 (and no uint64 can reach 6, since 1<<70 overflows
// the type), so "KMGTPE" — indices 0..5 — makes this a TOTAL function over
// uint64 with no clamping needed. A value that large is not hypothetical: a
// cgroup-v2 memory limit arrives as an ordinary uint64 the sampler surfaces
// verbatim, and an out-of-range index here would panic mid-render.
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
	units := "KMGTPE"
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
//
// The elapsed suffix comes from the request's own translator via the existing
// repo-wide common.ago key (the same key handlers_overview.go:698,927 already
// uses for its event clocks) — no new locale key, and no English literal
// stranded on a Russian page. The numeric part is unchanged, so EN output is
// byte-for-byte what it was.
func systemAgo(t time.Time, tr func(string) string) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	ago := " " + tr("common.ago")
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds())) + ago
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes())) + ago
	default:
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		return fmt.Sprintf("%dh %dm", h, m) + ago
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
		view.ClockText = systemAgo(snap.EvaluatedAt, tr)
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
