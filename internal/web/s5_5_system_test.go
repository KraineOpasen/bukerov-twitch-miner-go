package web

// S5-5 System routes tests: three direct server-rendered pages
// (/system/status, /system/diagnostics, /system/logs) replacing the three
// former compatibility redirects to /health and /logs. Every assertion here
// pins the honesty contract from the task brief: unknown/unavailable never
// renders as healthy, two distinct sync clocks never merge, no config forms
// or lifecycle mutation controls leak onto the read-only pages, the
// canonical support-bundle link has exactly one owner, and every sensitive
// LastError/log line is redacted before it ever reaches an HTTP response.

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/drops"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/health"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/i18n"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/lifecycle"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/resources"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/runtimeconfig"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
)

// ---------------------------------------------------------------------
// Local fakes (distinctly named to avoid colliding with fakeCampaignsProvider
// / fakeLifecycleController / f3Health / f3Progress already declared
// elsewhere in this package).
// ---------------------------------------------------------------------

// s55FakeLifecycleController is a minimal LifecycleController (see
// handlers_lifecycle.go:28-34) whose Snapshot is fully test-controlled; the
// four command methods are never exercised by the read-only System pages, so
// they return a zero SubmitResult.
type s55FakeLifecycleController struct {
	snap lifecycle.Snapshot
}

func (f *s55FakeLifecycleController) Snapshot() lifecycle.Snapshot { return f.snap }
func (f *s55FakeLifecycleController) Pause(context.Context) lifecycle.SubmitResult {
	return lifecycle.SubmitResult{}
}
func (f *s55FakeLifecycleController) Resume(context.Context) lifecycle.SubmitResult {
	return lifecycle.SubmitResult{}
}
func (f *s55FakeLifecycleController) Restart(context.Context) lifecycle.SubmitResult {
	return lifecycle.SubmitResult{}
}
func (f *s55FakeLifecycleController) Stop(context.Context) lifecycle.SubmitResult {
	return lifecycle.SubmitResult{}
}

// s55FakeHealthProvider is a minimal HealthProvider whose HealthSnapshot is
// fully test-controlled.
type s55FakeHealthProvider struct {
	snap health.Snapshot
}

func (f *s55FakeHealthProvider) HealthSnapshot() health.Snapshot { return f.snap }
func (f *s55FakeHealthProvider) RunCanaryNow()                   {}
func (f *s55FakeHealthProvider) CurrentHealthSettings() config.HealthSettings {
	return config.HealthSettings{}
}
func (f *s55FakeHealthProvider) ApplyHealthSettings(config.HealthSettings) error { return nil }

// s55FakeCampaignsProvider is a minimal CampaignsProvider whose SyncStatus is
// fully test-controlled (distinct from the package's own
// fakeCampaignsProvider in handlers_drops_sync_test.go). campaigns is
// optional (nil by default, exactly as before) — only the evidence harness
// below populates it, so /system/status's drops-sync row (which reads only
// SyncStatus) is unaffected either way.
type s55FakeCampaignsProvider struct {
	status    drops.SyncStatus
	campaigns []*models.Campaign
}

func (f *s55FakeCampaignsProvider) Campaigns() []*models.Campaign { return f.campaigns }
func (f *s55FakeCampaignsProvider) SyncStatus() drops.SyncStatus  { return f.status }
func (f *s55FakeCampaignsProvider) RequestManualSync() drops.ManualSyncResult {
	return drops.ManualSyncResult{Triggered: true, Status: f.status}
}

// s55FakeDropProgressProvider is a minimal DropProgressProvider whose
// DropProgress is fully test-controlled.
type s55FakeDropProgressProvider struct {
	snap health.ProgressSnapshot
}

func (f s55FakeDropProgressProvider) DropProgress() health.ProgressSnapshot { return f.snap }

// ruTR returns a Russian translation closure, the RU-language twin of the
// existing enTR helper (render_helpers_test.go), for tests that need to
// assert BOTH languages' rendered text rather than just structure.
func ruTR(t *testing.T) func(string) string {
	t.Helper()
	loc, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}
	return func(key string) string { return loc.T(i18n.LangRU, key) }
}

// ---------------------------------------------------------------------
// 1. Unknown/unavailable never renders as healthy.
// ---------------------------------------------------------------------

// TestS5_5StatusUnknownNeverHealthy proves that with a HealthProvider
// reporting zero signals and no lifecycle/campaigns providers wired at all,
// /system/status renders every row as unknown/unavailable — never as
// healthy/ok — and the resource mini-table renders the honest absence marker
// rather than a fabricated zero.
func TestS5_5StatusUnknownNeverHealthy(t *testing.T) {
	srv := newRenderServer(t)
	srv.SetHealthProvider(&s55FakeHealthProvider{snap: health.Snapshot{}})
	// lifecycleController, campaignsProvider, resourceSnapshot: left nil.

	req := httptest.NewRequest(http.MethodGet, "/system/status", nil)
	req.AddCookie(&http.Cookie{Name: langCookieName, Value: "en"})
	rec := httptest.NewRecorder()
	srv.handleSystemStatusPage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/system/status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	tr := enTR(t)

	if strings.Contains(body, "health-sev-ok") {
		t.Error("no signal is genuinely OK in this fixture; health-sev-ok must never appear")
	}

	// Row-scoped, not page-wide: with nothing wired every subsystem row must
	// say "unknown" in its own cell. A page-wide count tolerated one signal
	// regressing to ok once the resources row began contributing a fourth
	// occurrence of the same word.
	rows := sstRows(t, body)
	if len(rows) != 5 {
		t.Fatalf("expected 5 subsystem rows, got %d", len(rows))
	}
	for i, row := range rows {
		// Every row must name its own absence in words: the signal rows and
		// the resources row say "unknown", the drops row (no provider wired
		// at all) says "not available". Neither may be silently blank, and
		// neither may borrow a healthy word.
		if !strings.Contains(row, tr("health.status.unknown")) && !strings.Contains(row, tr("system.status.unavailable")) {
			t.Errorf("row %d must report unknown/unavailable when nothing is wired: %s", i, row)
		}
	}

	if !strings.Contains(body, tr("system.status.unavailable")) {
		t.Error("expected the lifecycle/drops-sync unavailable text (nil providers) to render")
	}

	// Scoped to the resource block via its own marker: base.html's embedded
	// js catalog already puts a dozen em-dashes on every page, so a
	// page-wide count proved nothing about these four rows, and the block
	// heading text also appears as the subsystem row's label.
	start := strings.Index(body, "data-system-resources")
	if start < 0 {
		t.Fatal("the resource block marker is missing")
	}
	block := body[start:]
	if end := strings.Index(block, "</div>"); end > 0 {
		if tail := strings.Index(block[end:], "data-system"); tail > 0 {
			block = block[:end+tail]
		}
	}
	if got := strings.Count(block, systemDash); got < 4 {
		t.Errorf("expected all 4 resource rows to render the absence marker %q, found %d in the resource block", systemDash, got)
	}
	if strings.Contains(body, `<span class="num">0</span>`) {
		t.Error("resources must never fabricate a literal 0 when the sampler is unavailable")
	}
}

// ---------------------------------------------------------------------
// 2. Drops sync: two distinct clocks, never merged.
// ---------------------------------------------------------------------

// s55FreshnessValue returns the age rendered by the subsystem table's
// freshness cell whose kind-label is exactly label, plus whether such a pair
// was rendered at all.
//
// Each clock renders as an ADJACENT pair — the age inside the C0 provenance
// chip's text span, immediately followed by the uppercase kind line (see
// system_status.html). This helper requires that exact adjacency (only
// inter-tag whitespace may sit between them), so an assertion built on it
// binds a label to ITS OWN value. A page-wide strings.Contains check for
// each label and each value independently cannot tell two correctly-paired
// clocks apart from two clocks whose values have been transposed — which is
// exactly the coverage gap this helper closes, and which neither the move to
// the dense table nor the move to the C0 chip may lose.
func s55FreshnessValue(body, label string) (string, bool) {
	const (
		ageOpen  = `<span class="c0-chip-text">`
		kindOpen = `<div class="type-micro text-text-muted uppercase">`
		spanEnd  = `</span>`
		divEnd   = `</div>`
		stampSep = `<span class="num">`
	)
	for rest := body; ; {
		i := strings.Index(rest, ageOpen)
		if i < 0 {
			return "", false
		}
		rest = rest[i+len(ageOpen):]

		end := strings.Index(rest, spanEnd)
		if end < 0 {
			return "", false
		}
		age := rest[:end]
		// Skip the chip's own closing </span> and any layout whitespace, then
		// require the kind line to be the very next element.
		after := rest[end+len(spanEnd):]
		if k := strings.Index(after, spanEnd); k >= 0 && strings.TrimSpace(after[:k]) == "" {
			after = after[k+len(spanEnd):]
		}
		after = strings.TrimLeft(after, " \t\r\n")
		rest = rest[end+len(spanEnd):]

		if !strings.HasPrefix(after, kindOpen) {
			continue
		}
		kind := after[len(kindOpen):]
		j := strings.Index(kind, divEnd)
		if j < 0 {
			continue
		}
		kind = kind[:j]
		if c := strings.Index(kind, stampSep); c >= 0 {
			kind = kind[:c]
		}
		if strings.TrimSpace(kind) != label {
			continue
		}
		return age, true
	}
}

// TestS5_5StatusDistinctAttemptAndSuccessClocks proves the drops-sync row
// renders the attempt clock (LastSyncAt) and the success clock
// (LastSuccessAt) as two separately labeled, separately valued lines — never
// merged into one, and never one substituted for the other.
//
// Each clock is asserted against its OWN adjacent kind-label via
// s55FreshnessValue, so transposing only the two values (leaving both
// labels in place) fails here.
func TestS5_5StatusDistinctAttemptAndSuccessClocks(t *testing.T) {
	srv := newRenderServer(t)
	srv.SetCampaignsProvider(&s55FakeCampaignsProvider{status: drops.SyncStatus{
		LastSyncAt:    time.Now().Add(-5 * time.Minute),
		LastSuccessAt: time.Now().Add(-3 * time.Hour),
		LastError:     "inventory: HTTP 500",
	}})

	req := httptest.NewRequest(http.MethodGet, "/system/status", nil)
	req.AddCookie(&http.Cookie{Name: langCookieName, Value: "en"})
	rec := httptest.NewRecorder()
	srv.handleSystemStatusPage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/system/status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	tr := enTR(t)

	for _, want := range []struct{ label, value string }{
		{tr("system.status.drops_sync.attempt_label"), "5m ago"},
		{tr("system.status.drops_sync.success_label"), "3h 0m ago"},
	} {
		got, ok := s55FreshnessValue(body, want.label)
		if !ok {
			t.Errorf("no freshness cell labeled %q was rendered", want.label)
			continue
		}
		if got != want.value {
			t.Errorf("row %q rendered value %q, want %q (label/value pair must not be transposed)", want.label, got, want.value)
		}
	}
}

// ---------------------------------------------------------------------
// 3. No config forms, no lifecycle mutation controls.
// ---------------------------------------------------------------------

// TestS5_5SystemPagesHaveNoConfigForms proves neither /system/status nor
// /system/diagnostics ever renders a settings form, the canary/watchdog
// field names, or a Save/Cancel/Discard control, and that /system/status
// additionally never renders a lifecycle mutation control (Pause/Resume/
// Restart/Stop) or the /api/lifecycle/ action endpoints — this is a
// STATUS-ONLY surface.
func TestS5_5SystemPagesHaveNoConfigForms(t *testing.T) {
	srv := buildF3PageServer(t)
	tr := enTR(t)

	// Note: a bare "Save"/"Cancel" substring check would false-positive on
	// EVERY page in this app — base.html unconditionally embeds the FULL
	// js.* client catalog as window.I18N in <head> (e.g. js.set.save =
	// "Save Settings", js.streamer.cancel = "Cancel"), regardless of which
	// page is rendering. The honest, page-specific check is the ACTUAL
	// settings-form button labels this page must never render.
	commonBanned := []string{
		"/api/health/settings", "canaryEnabled", "watchdogEnabled",
		"<form", "Discard",
		tr("health.canary.save"), tr("health.watchdog.save"),
	}
	for _, path := range []string{"/system/status", "/system/diagnostics"} {
		body := f3GetPage(t, srv, path, "en")
		for _, banned := range commonBanned {
			if strings.Contains(body, banned) {
				t.Errorf("%s must not contain %q", path, banned)
			}
		}
	}

	statusBody := f3GetPage(t, srv, "/system/status", "en")
	lifecycleBanned := []string{
		"/api/lifecycle/",
		tr("lc.btn.pause"), tr("lc.btn.resume"), tr("lc.btn.restart"), tr("lc.btn.stop"),
	}
	for _, banned := range lifecycleBanned {
		if strings.Contains(statusBody, banned) {
			t.Errorf("/system/status must not contain lifecycle action control %q", banned)
		}
	}
}

// ---------------------------------------------------------------------
// 4. Support bundle: single owner.
// ---------------------------------------------------------------------

// TestS5_5SupportBundleSingleOwner proves the canonical support-bundle
// download link (SupportBundlePath) is present on /system/diagnostics only —
// absent from /logs, /system/logs, and /system/status — when the dashboard
// has real authentication configured.
func TestS5_5SupportBundleSingleOwner(t *testing.T) {
	srv := newAuthedServer(t)
	h := srv.handler()

	want := map[string]bool{
		"/system/diagnostics": true,
		"/system/status":      false,
		"/logs":               false,
		"/system/logs":        false,
	}
	for path, wantPresent := range want {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.SetBasicAuth("admin", "hunter2")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, rec.Code)
		}
		got := strings.Contains(rec.Body.String(), SupportBundlePath)
		if got != wantPresent {
			t.Errorf("%s contains %q = %v, want %v", path, SupportBundlePath, got, wantPresent)
		}
	}
}

// ---------------------------------------------------------------------
// 5. Build/update evidence: honest absence, honest presence.
// ---------------------------------------------------------------------

// TestS5_5BuildAndUpdateEvidenceAbsence proves that with no lifecycleUpdateState
// wired, /system/diagnostics shows the honest "build information unavailable"
// line and renders no build SHA/digest/build-time value, no "up to date"
// claim, and no positive update row; wiring State="available"+Version makes
// the available-update row appear with that version.
func TestS5_5BuildAndUpdateEvidenceAbsence(t *testing.T) {
	srv := newRenderServer(t)
	tr := enTR(t)

	req := httptest.NewRequest(http.MethodGet, "/system/diagnostics", nil)
	req.AddCookie(&http.Cookie{Name: langCookieName, Value: "en"})
	rec := httptest.NewRecorder()
	srv.handleSystemDiagnosticsPage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/system/diagnostics = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	if !strings.Contains(body, tr("system.diagnostics.build_info_unavailable")) {
		t.Error("expected the honest build-information-unavailable line")
	}
	banned := []string{
		"up to date", "актуальная версия",
		tr("lc.update_available"), tr("lc.update_failed"), tr("lc.update_applied"),
	}
	for _, b := range banned {
		if strings.Contains(body, b) {
			t.Errorf("with no update state wired, diagnostics must not contain %q", b)
		}
	}

	srv.SetLifecycleUpdateState(func() LifecycleUpdateState {
		return LifecycleUpdateState{State: "available", Version: "v9.9.9"}
	})
	rec2 := httptest.NewRecorder()
	srv.handleSystemDiagnosticsPage(rec2, req)
	body2 := rec2.Body.String()
	if !strings.Contains(body2, tr("lc.update_available")) {
		t.Error("expected the available-update row to appear once State=\"available\"")
	}
	if !strings.Contains(body2, "v9.9.9") {
		t.Error("expected the available update's version to render")
	}
	// Build information remains honestly unavailable regardless of update state.
	if !strings.Contains(body2, tr("system.diagnostics.build_info_unavailable")) {
		t.Error("build-information-unavailable line must still render alongside update evidence")
	}
}

// ---------------------------------------------------------------------
// 6. /system/logs control-set parity with /logs.
// ---------------------------------------------------------------------

// TestS5_5SystemLogsParitySet proves /system/logs renders the full proven
// control set (ids, hx-get, the "every 10s" poll literal) present on /logs —
// a presence-set comparison, not whole-page byte equality (the two pages
// differ elsewhere, e.g. which nav child is marked current).
func TestS5_5SystemLogsParitySet(t *testing.T) {
	srv := buildF3PageServer(t)
	logsBody := f3GetPage(t, srv, "/logs", "en")
	sysLogsBody := f3GetPage(t, srv, "/system/logs", "en")

	controls := []string{
		`id="logs-filter-level"`, `id="logs-filter-subsystem"`, `id="logs-filter-search"`,
		`id="logs-filter-reconnect"`, `id="logs-copy-btn"`, `id="logs-copy-status"`,
		`id="logs-count"`, `id="logs-no-match"`, `id="logs-new-indicator"`,
		`id="logs-scroll"`, `id="logs-lines"`,
		`hx-get="/api/logs"`, "every 10s",
	}
	for _, c := range controls {
		if !strings.Contains(logsBody, c) {
			t.Errorf("/logs missing control %q", c)
		}
		if !strings.Contains(sysLogsBody, c) {
			t.Errorf("/system/logs missing control %q", c)
		}
	}
}

// ---------------------------------------------------------------------
// 7. Logs "technical journal" note + /events link.
// ---------------------------------------------------------------------

// TestS5_5LogsTechnicalJournalNote proves both /logs and /system/logs carry
// the honest explanatory note (logs are the miner's technical journal;
// product events live at /events) and a real <a href="/events"> link, in
// both languages.
func TestS5_5LogsTechnicalJournalNote(t *testing.T) {
	srv := buildF3PageServer(t)
	cases := []struct {
		lang string
		tr   func(string) string
	}{
		{"en", enTR(t)},
		{"ru", ruTR(t)},
	}
	for _, tc := range cases {
		for _, path := range []string{"/logs", "/system/logs"} {
			body := f3GetPage(t, srv, path, tc.lang)
			if !strings.Contains(body, tc.tr("logs.journal_note")) {
				t.Errorf("%s (lang=%s) missing the technical-journal note text", path, tc.lang)
			}
			if !strings.Contains(body, `<a href="/events">`) {
				t.Errorf("%s (lang=%s) missing the <a href=\"/events\"> link", path, tc.lang)
			}
		}
	}
}

// ---------------------------------------------------------------------
// 8. Lifecycle LastError redaction on /system/status.
// ---------------------------------------------------------------------

// TestS5_5StatusRedactsLifecycleLastError table-drives over the six S5-4
// sensitivity categories (shared fixture: s54SensitiveLastErrorCanaries,
// s5_4_drops_harness_test.go) as a fake lifecycle controller's
// Snapshot.LastError: the rendered /system/status must contain "[REDACTED]"
// and never the raw canary marker (nor, for the URL case, the bare host).
// The paired benign control (s54BenignLastError = "inventory: HTTP 500")
// must render verbatim, never redacted.
func TestS5_5StatusRedactsLifecycleLastError(t *testing.T) {
	for _, canary := range s54SensitiveLastErrorCanaries {
		t.Run(canary.name, func(t *testing.T) {
			srv := newRenderServer(t)
			srv.SetLifecycleController(&s55FakeLifecycleController{snap: lifecycle.Snapshot{
				Observed:  lifecycle.ObservedFailed,
				LastError: canary.value,
			}})
			req := httptest.NewRequest(http.MethodGet, "/system/status", nil)
			req.AddCookie(&http.Cookie{Name: langCookieName, Value: "en"})
			rec := httptest.NewRecorder()
			srv.handleSystemStatusPage(rec, req)
			body := rec.Body.String()

			if strings.Contains(body, canary.marker) {
				t.Errorf("rendered body leaked the raw canary marker %q", canary.marker)
			}
			for _, absent := range canary.alsoAbsent {
				if strings.Contains(body, absent) {
					t.Errorf("rendered body leaked %q from the raw LastError", absent)
				}
			}
			if !strings.Contains(body, "[REDACTED]") {
				t.Error("expected the redacted marker [REDACTED] in the rendered body")
			}
		})
	}

	// Paired benign control: renders verbatim, never redacted.
	srv := newRenderServer(t)
	srv.SetLifecycleController(&s55FakeLifecycleController{snap: lifecycle.Snapshot{
		Observed:  lifecycle.ObservedFailed,
		LastError: s54BenignLastError,
	}})
	req := httptest.NewRequest(http.MethodGet, "/system/status", nil)
	req.AddCookie(&http.Cookie{Name: langCookieName, Value: "en"})
	rec := httptest.NewRecorder()
	srv.handleSystemStatusPage(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, s54BenignLastError) {
		t.Errorf("expected the benign LastError %q to render verbatim", s54BenignLastError)
	}
	if strings.Contains(body, "[REDACTED]") {
		t.Error("a genuinely benign LastError must not trigger redaction")
	}
}

// ---------------------------------------------------------------------
// 9. Log-line redaction end to end: /logs, /api/logs AND /system/logs.
// ---------------------------------------------------------------------

// TestS5_5LogsRedactSensitiveRenderedContent writes a temp log file (the
// same fixture shape as TestReadLogTailClassifies) containing one raw line
// per S5-4 sensitivity category plus a benign control line, then exercises
// the REAL handler/template chain for all THREE rendered log surfaces —
// GET /logs, GET /api/logs and GET /system/logs: every sensitive line's raw
// canary marker must be absent from the response and replaced by
// "[REDACTED]"; the benign line must render verbatim; and EVERY redacted
// line — not just one of them — must still carry its correct log-error
// classification (the class attribute, derived from the RAW line, is
// unaffected by the later redaction of the display text).
//
// /system/logs is exercised through its own real handler rather than trusted
// to the shared buildLogsPageData builder: giving that one route a raw,
// unsanitized builder is a mutation the /logs + /api/logs pair alone cannot
// see, so the privacy boundary is proven per-surface here.
func TestS5_5LogsRedactSensitiveRenderedContent(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("logs", 0o755); err != nil {
		t.Fatal(err)
	}

	type canaryLine struct {
		name   string
		line   string
		marker string
	}
	// Each value below is whitespace-separated from its surrounding text (the
	// url/entropy shapes are detected token-by-token via strings.Fields in
	// supportbundle.Redact — see looksSensitive — so a canary glued directly
	// onto a key= prefix with no separating space would not tokenize as a
	// bare URL/entropy run; the bearer/cookie/secret shapes are instead
	// matched as whole-string substrings/regexes and are insensitive to
	// spacing). Mirrors s54SensitiveLastErrorCanaries' own working shapes.
	lines := []canaryLine{
		{"bearer", `time=2026-08-06T10:00:00Z level=ERROR msg="request failed" detail="Authorization: Bearer S5_5_LOG_CANARY_BEARER_AB12CD34"`, "S5_5_LOG_CANARY_BEARER_AB12CD34"},
		{"cookie", `time=2026-08-06T10:00:01Z level=ERROR msg="request failed" detail="Set-Cookie: session=S5_5_LOG_CANARY_COOKIE_AB12CD34"`, "S5_5_LOG_CANARY_COOKIE_AB12CD34"},
		{"secret", `time=2026-08-06T10:00:02Z level=ERROR msg="request failed" detail="client_secret=S5_5_LOG_CANARY_SECRET_AB12CD34"`, "S5_5_LOG_CANARY_SECRET_AB12CD34"},
		{"url", `time=2026-08-06T10:00:03Z level=ERROR msg="request failed: Post "https://gql.twitch.tv/gql?sig=S5_5_LOG_CANARY_SIG_AB12CD34": context deadline exceeded"`, "S5_5_LOG_CANARY_SIG_AB12CD34"},
		{"multiline", "time=2026-08-06T10:00:04Z level=ERROR msg=\"trace: S5_5_LOG_CANARY_MULTILINE_AB12CD34\rsecond frame\"", "S5_5_LOG_CANARY_MULTILINE_AB12CD34"},
		{"entropy", `time=2026-08-06T10:00:05Z level=ERROR msg="token seen: Xk9Qm2Pv7Rt4Ws8Zc6Fh0Jd5Lg8Nn3Ss7Uu"`, "Xk9Qm2Pv7Rt4Ws8Zc6Fh0Jd5Lg8Nn3Ss7Uu"},
	}
	const benign = "inventory: HTTP 500"

	var buf strings.Builder
	for _, l := range lines {
		buf.WriteString(l.line)
		buf.WriteString("\n")
	}
	buf.WriteString(`time=2026-08-06T10:00:06Z level=INFO msg="` + benign + `"` + "\n")

	const username = "s55logtester"
	if err := os.WriteFile(filepath.Join("logs", username+".log"), []byte(buf.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := newRenderServer(t)
	srv.username = username

	for _, path := range []string{"/logs", "/api/logs", "/system/logs"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			switch path {
			case "/logs":
				srv.handleLogsPage(rec, req)
			case "/api/logs":
				srv.handleAPILogs(rec, req)
			default:
				srv.handleSystemLogsPage(rec, req)
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200", path, rec.Code)
			}
			body := rec.Body.String()

			for _, l := range lines {
				if strings.Contains(body, l.marker) {
					t.Errorf("%s: leaked raw canary marker %q (%s)", path, l.marker, l.name)
				}
			}
			if got := strings.Count(body, "[REDACTED]"); got < len(lines) {
				t.Errorf("%s: got %d [REDACTED] markers, want at least %d", path, got, len(lines))
			}
			if !strings.Contains(body, benign) {
				t.Errorf("%s: benign line %q must render verbatim, not redacted", path, benign)
			}
			// EVERY sensitive fixture is level=ERROR, so exactly len(lines)
			// spans must carry the log-error class — counted, not merely
			// witnessed once. A bare existence check survives a mutant that
			// keeps the first ERROR line classified and downgrades all the
			// later ones; an exact count kills it. The benign control is
			// level=INFO and is deliberately NOT part of lines, so it never
			// contributes to this count.
			if got := strings.Count(body, `class="log-line log-error"`); got != len(lines) {
				t.Errorf("%s: got %d log-error classified lines, want exactly %d", path, got, len(lines))
			}
		})
	}
}

// ---------------------------------------------------------------------
// 10. Nav activation: exactly one System child current, per route.
// ---------------------------------------------------------------------

// TestS5_5SystemActiveChildPerRoute mirrors
// TestS5_3OverviewQueueExactlyOneAriaCurrentDestination's approach
// (s5_2_chrome_test.go): it re-implements base.html's client-side
// updateActiveNav rule against the rendered C2 nav markup for each of the
// three System routes, proving exactly one System child is ever "current",
// and it is the right one.
func TestS5_5SystemActiveChildPerRoute(t *testing.T) {
	srv := buildF3PageServer(t)

	for _, path := range []string{"/system/status", "/system/diagnostics", "/system/logs"} {
		t.Run(path, func(t *testing.T) {
			body := f3GetPage(t, srv, path, "en")
			const active = "system" // SECTION_RULES: every /system/* path -> system

			tags := s5_3NavAnchorTagRe.FindAllString(body, -1)
			if len(tags) == 0 {
				t.Fatal("no C2 nav destination anchors found in the rendered page")
			}

			var currentHrefs []string
			for _, tag := range tags {
				href := ""
				if m := s5_3HrefAttrRe.FindStringSubmatch(tag); m != nil {
					href = m[1]
				}
				section := ""
				if m := s5_3NavSectionAttrRe.FindStringSubmatch(tag); m != nil {
					section = m[1]
				}
				isParent := strings.Contains(tag, "data-nav-parent")
				isChild := strings.Contains(tag, "data-nav-child")
				sectionMatches := section == active
				isCurrent := sectionMatches
				if isChild {
					isCurrent = sectionMatches && href == path
				}
				if !isParent && isCurrent {
					currentHrefs = append(currentHrefs, href)
				}
			}
			if len(currentHrefs) != 1 {
				t.Errorf("simulated nav activation on %s must mark exactly one destination current, got %d: %v", path, len(currentHrefs), currentHrefs)
			} else if currentHrefs[0] != path {
				t.Errorf("the one current destination must be %s itself, got %s", path, currentHrefs[0])
			}
		})
	}
}

// ---------------------------------------------------------------------
// 11. Legacy /health and /logs stay direct 200.
// ---------------------------------------------------------------------

// TestS5_5LegacyHealthLogsStillDirect200 proves /health and /logs remain
// direct 200 renders (never redirected) after this slice's route additions,
// and that /health still carries its settings-form machinery marker.
func TestS5_5LegacyHealthLogsStillDirect200(t *testing.T) {
	srv := buildF3PageServer(t)
	h := srv.handler()

	for _, path := range []string{"/health", "/logs"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 (direct, not redirected)", path, rec.Code)
		}
	}

	healthBody := f3GetPage(t, srv, "/health", "en")
	if !strings.Contains(healthBody, `hx-get="/api/health"`) {
		t.Error("/health must keep its settings-form machinery marker (hx-get=\"/api/health\")")
	}
}

// ---------------------------------------------------------------------
// 12. Logs toolbar controls stay in the DOM at mobile widths.
// ---------------------------------------------------------------------

// TestS5_5LogsToolbarControlsRemainOnMobile pins the DOM contract behind the
// mobile requirement: every toolbar control stays in the markup (compact/
// reflow is CSS-only, via the existing flex-wrap classes) — no hidden/
// sm:hidden/max-sm:hidden/lg:hidden class and no inline display:none on any
// toolbar control, on BOTH /logs and /system/logs. Browser-level viewport
// verification happens separately (webapp-testing); this only pins the DOM.
func TestS5_5LogsToolbarControlsRemainOnMobile(t *testing.T) {
	srv := buildF3PageServer(t)
	for _, path := range []string{"/logs", "/system/logs"} {
		body := f3GetPage(t, srv, path, "en")

		start := strings.Index(body, `role="toolbar"`)
		end := strings.Index(body, `<div class="card relative">`)
		if start < 0 || end < 0 || end <= start {
			t.Fatalf("%s: could not locate the logs toolbar block", path)
		}
		toolbar := body[start:end]

		for _, id := range []string{"logs-filter-level", "logs-filter-subsystem", "logs-filter-search", "logs-filter-reconnect", "logs-copy-btn"} {
			if !strings.Contains(toolbar, `id="`+id+`"`) {
				t.Errorf("%s: toolbar control %q missing from the DOM", path, id)
			}
		}
		for _, banned := range []string{
			`class="hidden`, ` hidden"`, ` hidden `,
			"sm:hidden", "max-sm:hidden", "md:hidden", "lg:hidden", "xl:hidden",
			"display:none", "display: none",
		} {
			if strings.Contains(toolbar, banned) {
				t.Errorf("%s: toolbar carries a hiding marker %q", path, banned)
			}
		}
		if !strings.Contains(toolbar, "logs-toolbar-group") {
			t.Errorf("%s: toolbar must use the existing wrap classes (logs-toolbar-group)", path)
		}
	}
}

// ---------------------------------------------------------------------
// 13. Redirect matrix shrunk to 13.
// ---------------------------------------------------------------------

// TestS5_5RedirectMatrixShrunkTo13 proves compatibilityRedirects lost exactly
// the three /system/* entries (16 -> 13 at the time of task S5-5; task S5-6
// later removed the ten /settings/* entries and task S5-8 the last two
// /analytics/* ones, so the map now holds only /help — this test's own name
// is pinned to the S5-5 slice's history and is left
// unchanged), that each of the three routes now renders directly (200, never
// a 30x), and that the remaining entries still 302 to their unchanged
// targets. Building the full mux via srv.handler() also means a
// duplicate-pattern registration panic would surface here (and in every
// other test in this file that calls it) — this is the exact mechanism that
// would catch a S5-6-style regression that left a route in both this map and
// its own direct-route registration.
func TestS5_5RedirectMatrixShrunkTo13(t *testing.T) {
	if len(compatibilityRedirects) != 1 {
		t.Fatalf("len(compatibilityRedirects) = %d, want 1", len(compatibilityRedirects))
	}
	for _, route := range []string{"/system/status", "/system/diagnostics", "/system/logs"} {
		if target, ok := compatibilityRedirects[route]; ok {
			t.Errorf("compatibilityRedirects must no longer contain %q (still maps to %q)", route, target)
		}
	}

	srv := buildF3PageServer(t)
	h := srv.handler()

	for _, route := range []string{"/system/status", "/system/diagnostics", "/system/logs"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, route, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 direct (not a redirect)", route, rec.Code)
		}
	}

	for route, target := range compatibilityRedirects {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, route, nil))
		if rec.Code != http.StatusFound {
			t.Errorf("GET %s = %d, want 302", route, rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != target {
			t.Errorf("GET %s Location = %q, want %q", route, loc, target)
		}
	}
}

// ---------------------------------------------------------------------
// 14. Locale keys: present, non-empty, translated in both languages.
// ---------------------------------------------------------------------

// s55DeliberatelyIdenticalKeys lists the new S5-5 keys whose EN/RU values are
// deliberately identical (protocol/brand proper nouns), mirroring the
// s53DeliberatelyIdenticalKeys precedent (s5_3_i18n_test.go) for this task's
// own key set.
var s55DeliberatelyIdenticalKeys = map[string]bool{
	"system.status.oauth.label":  true,
	"system.status.gql.label":    true,
	"system.status.pubsub.label": true,
}

// TestS5_5LocaleKeysPresentAndTranslated mirrors
// TestS5_3LocaleKeysPresentAndTranslated's precedent for every new S5-5 key:
// non-empty and actually translated (not echoing the key back) in both EN
// and RU, and RU differs from EN except for the deliberate proper-noun
// exemptions above.
func TestS5_5LocaleKeysPresentAndTranslated(t *testing.T) {
	loc, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}
	keys := []string{
		"system.tab.status", "system.tab.diagnostics",
		"system.status.unavailable", "system.status.signal.last_check_label",
		"system.status.heading", "system.status.subtitle", "system.status.refresh",
		"system.status.lifecycle.label", "system.status.lifecycle.started_label", "system.status.lifecycle.transition_started_label",
		"system.status.oauth.label", "system.status.gql.label", "system.status.pubsub.label",
		"system.status.drops_sync.label", "system.status.drops_sync.attempt_label", "system.status.drops_sync.success_label",
		"system.status.resources.heading", "system.status.resources.sampled_label",
		"system.status.resources.pointer", "system.status.resources.metrics",
		"system.status.lifecycle.controls_note", "system.status.lifecycle.desired_label",
		"system.status.col.subsystem", "system.status.col.status",
		"system.status.col.freshness", "system.status.col.detail",
		"system.status.table.caption", "system.status.table.region_label",
		"system.status.freshness.none", "system.status.build.label",
		"system.diagnostics.heading", "system.diagnostics.subtitle",
		"system.diagnostics.watchdog.evaluated_label", "system.diagnostics.watchdog.enabled", "system.diagnostics.watchdog.disabled",
		"system.diagnostics.version_label", "system.diagnostics.build_info_unavailable",
		"logs.journal_note",
		"js.system.canary_run_accepted", "js.system.canary_run_failed",
	}
	for _, k := range keys {
		en := loc.T(i18n.LangEN, k)
		ru := loc.T(i18n.LangRU, k)
		if en == k {
			t.Errorf("EN missing translation for %q (echoed the key back)", k)
		}
		if ru == k {
			t.Errorf("RU missing translation for %q (echoed the key back)", k)
		}
		if en == "" || ru == "" {
			t.Errorf("%q has an empty value in one language (en=%q ru=%q)", k, en, ru)
		}
		if en == ru && !s55DeliberatelyIdenticalKeys[k] {
			t.Errorf("%q has identical EN/RU text %q — looks untranslated", k, en)
		}
	}
}

// ---------------------------------------------------------------------
// 15. formatSystemBytes is total over the whole uint64 range.
// ---------------------------------------------------------------------

// s55SafeFormatSystemBytes calls formatSystemBytes and converts a panic into a
// returned value, so one out-of-range input reports as a normal test failure
// instead of tearing down the whole package's test binary.
func s55SafeFormatSystemBytes(n uint64) (out string, panicked any) {
	defer func() { panicked = recover() }()
	out = formatSystemBytes(n)
	return out, nil
}

// TestS5_5FormatSystemBytesTotalOverUint64 pins formatSystemBytes as a TOTAL
// function over uint64. The unit table it indexes must cover every exponent
// the divisor loop can reach: a value >= 1<<60 drives the exponent to 5, and
// 5 is out of range for a five-rune "KMGTP" table — an index panic inside a
// page render, reachable from a real resource snapshot (a cgroup-v2 memory
// limit is an ordinary uint64 the sampler surfaces verbatim).
//
// Outputs below 1<<60 are pinned to their exact existing text, so widening
// the table cannot silently reformat any value the dashboard renders today.
func TestS5_5FormatSystemBytesTotalOverUint64(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   uint64
		want string
	}{
		{"zero", 0, "0B"},
		{"sub-unit", 512, "512B"},
		{"exact KiB", 1 << 10, "1.0KB"},
		{"fractional KiB", 1536, "1.5KB"},
		{"exact MiB", 1 << 20, "1.0MB"},
		{"exact GiB", 1 << 30, "1.0GB"},
		{"exact TiB", 1 << 40, "1.0TB"},
		{"exact PiB", 1 << 50, "1.0PB"},
		{"exact EiB", 1 << 60, "1.0EB"},
		{"max uint64", math.MaxUint64, "16.0EB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, panicked := s55SafeFormatSystemBytes(tc.in)
			if panicked != nil {
				t.Fatalf("formatSystemBytes(%d) panicked: %v", tc.in, panicked)
			}
			if got != tc.want {
				t.Errorf("formatSystemBytes(%d) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestS5_5ResourcesRenderExtremeMemoryLimit drives the extreme value through
// the REAL /system/status render path (the seam Q3-1 is actually reachable
// from): a resource snapshot whose memory limit is the largest uint64 must
// render a bounded string rather than panicking mid-response.
func TestS5_5ResourcesRenderExtremeMemoryLimit(t *testing.T) {
	srv := newRenderServer(t)
	srv.SetResourceSnapshotProvider(func() resources.Snapshot {
		return resources.Snapshot{
			Available: true,
			SampledAt: time.Now().UTC().Format(time.RFC3339),
			Memory: resources.Memory{
				Available:  true,
				UsedBytes:  1 << 60,
				LimitBytes: math.MaxUint64,
			},
		}
	})

	req := httptest.NewRequest(http.MethodGet, "/system/status", nil)
	req.AddCookie(&http.Cookie{Name: langCookieName, Value: "en"})
	rec := httptest.NewRecorder()
	srv.handleSystemStatusPage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/system/status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"1.0EB", "/ 16.0EB"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected the memory row to render %q", want)
		}
	}
}

// ---------------------------------------------------------------------
// 16. Elapsed-time clocks localize through the existing common.ago key.
// ---------------------------------------------------------------------

// TestS5_5StatusClockLocalizesElapsedSuffix proves every S5-5 freshness clock
// renders its "ago" suffix through the request's own translator (the existing
// repo-wide common.ago key — see handlers_overview.go:698,927 — reused, no new
// locale key), not a hardcoded English literal. EN output is unchanged
// byte-for-byte; RU renders "назад" and must not carry the English clock.
func TestS5_5StatusClockLocalizesElapsedSuffix(t *testing.T) {
	newSrv := func() *Server {
		srv := newRenderServer(t)
		srv.SetCampaignsProvider(&s55FakeCampaignsProvider{status: drops.SyncStatus{
			LastSyncAt:    time.Now().Add(-5 * time.Minute),
			LastSuccessAt: time.Now().Add(-3 * time.Hour),
		}})
		return srv
	}
	render := func(lang string) string {
		req := httptest.NewRequest(http.MethodGet, "/system/status", nil)
		req.AddCookie(&http.Cookie{Name: langCookieName, Value: lang})
		rec := httptest.NewRecorder()
		newSrv().handleSystemStatusPage(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s /system/status = %d, want 200", lang, rec.Code)
		}
		return rec.Body.String()
	}

	en, ru := render("en"), render("ru")
	enTr, ruTr := enTR(t), ruTR(t)

	// The suffix comes from common.ago in both languages.
	if got := enTr("common.ago"); got != "ago" {
		t.Fatalf("fixture drift: EN common.ago = %q, want %q", got, "ago")
	}
	if got := ruTr("common.ago"); got != "назад" {
		t.Fatalf("fixture drift: RU common.ago = %q, want %q", got, "назад")
	}

	for _, tc := range []struct {
		lang, body, label, want string
	}{
		{"en", en, enTr("system.status.drops_sync.attempt_label"), "5m ago"},
		{"en", en, enTr("system.status.drops_sync.success_label"), "3h 0m ago"},
		{"ru", ru, ruTr("system.status.drops_sync.attempt_label"), "5m назад"},
		{"ru", ru, ruTr("system.status.drops_sync.success_label"), "3h 0m назад"},
	} {
		got, ok := s55FreshnessValue(tc.body, tc.label)
		if !ok {
			t.Errorf("%s: no freshness cell labeled %q was rendered", tc.lang, tc.label)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: row %q rendered %q, want %q", tc.lang, tc.label, got, tc.want)
		}
	}

	// The RU page must not carry the English elapsed-clock text at all.
	for _, leaked := range []string{"5m ago", "3h 0m ago", "m ago", "s ago"} {
		if strings.Contains(ru, leaked) {
			t.Errorf("RU /system/status leaked the English elapsed clock %q", leaked)
		}
	}
}

// TestS5_5SystemAgoSemanticsPreserved pins systemAgo's behavior across the
// localization change: a zero timestamp still renders nothing at all (so the
// caller omits the clock rather than claiming a false freshness), a future
// timestamp still clamps to zero rather than rendering a negative duration,
// and the seconds / minutes / hours+minutes thresholds are unmoved.
func TestS5_5SystemAgoSemanticsPreserved(t *testing.T) {
	en, ru := enTR(t), ruTR(t)
	now := time.Now()

	if got := systemAgo(time.Time{}, en); got != "" {
		t.Errorf("systemAgo(zero) = %q, want \"\" (no clock at all)", got)
	}
	if got := systemAgo(time.Time{}, ru); got != "" {
		t.Errorf("RU systemAgo(zero) = %q, want \"\"", got)
	}
	if got := systemAgo(now.Add(time.Hour), en); got != "0s ago" {
		t.Errorf("systemAgo(future) = %q, want %q (clamped to zero, never negative)", got, "0s ago")
	}
	if got := systemAgo(now.Add(time.Hour), ru); got != "0s назад" {
		t.Errorf("RU systemAgo(future) = %q, want %q", got, "0s назад")
	}

	for _, tc := range []struct {
		name           string
		ago            time.Duration
		wantEN, wantRU string
	}{
		{"seconds", 30 * time.Second, "30s ago", "30s назад"},
		{"just under a minute", 59 * time.Second, "59s ago", "59s назад"},
		{"minutes", 5 * time.Minute, "5m ago", "5m назад"},
		{"just under an hour", 59 * time.Minute, "59m ago", "59m назад"},
		{"hours and minutes", 3*time.Hour + 7*time.Minute, "3h 7m ago", "3h 7m назад"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Nudge past the boundary so time elapsed during the call cannot
			// tip the duration into the next bucket.
			at := now.Add(-tc.ago - 100*time.Millisecond)
			if got := systemAgo(at, en); got != tc.wantEN {
				t.Errorf("EN systemAgo(-%v) = %q, want %q", tc.ago, got, tc.wantEN)
			}
			if got := systemAgo(at, ru); got != tc.wantRU {
				t.Errorf("RU systemAgo(-%v) = %q, want %q", tc.ago, got, tc.wantRU)
			}
		})
	}
}

// ---------------------------------------------------------------------
// S5-5 browser-evidence harness: mirrors the TestF3EvidenceHarness
// precedent (f3_harness_test.go — READ-ONLY reference, never edited here)
// but adds what that harness cannot provide: a dashboard with REAL Basic
// Auth enabled, and a deliberate populated-vs-unknown provider contrast
// (PubSub simply never recorded, alongside genuinely OK OAuth/GQL-API/
// WatchTransport rows) so a human or Playwright evidence run can see every
// S5-5 honesty state at once in one page load — unknown never rendered as
// healthy, two DISTINCT drops-sync clocks, a clean lifecycle row next to a
// degraded drops-sync row, redacted vs. benign log lines, the honest
// absent-update/absent-build-info lines, and the resources mini-table's
// available/absent split.
// ---------------------------------------------------------------------

// s55EvidenceLogLines is the harness's on-disk log fixture: 8 benign,
// slog-shaped lines spanning INFO/WARN/ERROR and several subsystems —
// including one whose msg is exactly "inventory: HTTP 500" (the same
// s54BenignLastError control string used elsewhere in this package), so
// /system/logs and /system/status can be visually cross-checked against the
// identical benign text — plus exactly two sensitive canary lines (a
// Bearer-token Authorization header, and an absolute URL carrying a
// client_secret query credential) that supportbundle.Redact must turn into
// "[REDACTED]" end to end on both /logs and /system/logs.
var s55EvidenceLogLines = []string{
	`time=2026-08-06T09:00:00.000+00:00 level=INFO msg="Twitch Channel Points Miner" version=v0.27.1`,
	`time=2026-08-06T09:00:01.000+00:00 level=INFO msg="Streamer is online" streamer=s55stream`,
	`time=2026-08-06T09:00:02.000+00:00 level=INFO msg="Points earned" reason=WATCH points=10 streamer=s55stream`,
	`time=2026-08-06T09:00:03.000+00:00 level=WARN msg="Request retry scheduled" attempt=2`,
	`time=2026-08-06T09:00:04.000+00:00 level=ERROR msg="GraphQL request failed" status=502`,
	`time=2026-08-06T09:00:05.000+00:00 level=INFO msg="Claiming drop" drop="Evidence Crate"`,
	`time=2026-08-06T09:00:06.000+00:00 level=INFO msg="Settings saved to config file"`,
	`time=2026-08-06T09:00:07.000+00:00 level=WARN msg="inventory: HTTP 500" campaign="Evidence Campaign"`,
	`time=2026-08-06T09:00:08.000+00:00 level=ERROR msg="request failed" detail="Authorization: Bearer S55_CANARY_BEARER_e2e_0123456789abcdef"`,
	`time=2026-08-06T09:00:09.000+00:00 level=ERROR msg="request failed: Post "https://gql.example.invalid/creds?client_secret=S55_CANARY_URL_SECRET_e2e": context deadline exceeded"`,
}

// s55WriteEvidenceLogFixture writes s55EvidenceLogLines to logs/<username>.log
// under dir (the current working directory, which the caller must already
// have t.Chdir'd into — logger.LogFilePath resolves a relative "logs/..."
// path, exactly like f3WriteLogFixture's own convention). Registers an
// explicit t.Cleanup that removes the file (and the now-empty logs/ dir):
// belt-and-suspenders on top of t.TempDir()'s own automatic cleanup, so
// nothing from this harness is ever left behind.
func s55WriteEvidenceLogFixture(t *testing.T, dir, username string) {
	t.Helper()
	logsDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logsDir, username+".log")
	content := strings.Join(s55EvidenceLogLines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Remove(path)
		if entries, err := os.ReadDir(logsDir); err == nil && len(entries) == 0 {
			_ = os.Remove(logsDir)
		}
	})
}

// TestS5_5EvidenceHarness serves a real, fully-wired dashboard on localhost
// with REAL Basic Auth enabled and a deliberate populated-vs-unknown
// provider contrast. Env-gated: skipped unless MINER_S5_5_HARNESS=1. Never
// talks to Twitch, Discord, or any real network.
//
// Usage:
//
//	MINER_S5_5_HARNESS=1 MINER_S5_5_HARNESS_ADDR=127.0.0.1:8974 \
//	  go test -run TestS5_5EvidenceHarness -timeout 1800s ./internal/web/
//
// The server stops when the harness receives SIGINT/SIGTERM or 30 minutes
// elapse.
func TestS5_5EvidenceHarness(t *testing.T) {
	if os.Getenv("MINER_S5_5_HARNESS") != "1" {
		t.Skip("harness disabled (set MINER_S5_5_HARNESS=1)")
	}
	addr := os.Getenv("MINER_S5_5_HARNESS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8974"
	}
	const username = "s55evidence"
	const dashUser = "s55"
	const dashPass = "s55-evidence"

	workDir := t.TempDir()
	t.Chdir(workDir)
	s55WriteEvidenceLogFixture(t, workDir, username)

	dbDir := t.TempDir()
	db, err := database.Open(dbDir)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	svc, err := analytics.NewService(db, dbDir, 0)
	if err != nil {
		t.Fatalf("analytics: %v", err)
	}

	streamers := []*models.Streamer{
		models.NewStreamer("streamer_a", models.StreamerSettings{}),
		models.NewStreamer("streamer_b", models.StreamerSettings{}),
	}

	cfg := config.DefaultConfig()
	cfg.Streamers = []config.StreamerConfig{{Username: "streamer_a"}, {Username: "streamer_b"}}
	rt := settings.BuildRuntimeSettings(&cfg)

	now := time.Now()

	srv := NewServer(config.AnalyticsSettings{Host: "127.0.0.1", Port: 0, Refresh: 5, DaysAgo: 30}, username, workDir, svc, streamers)
	srv.SetDiscordEnabled(true)

	// REAL Basic Auth enabled — the one thing the F3 harness never turns on.
	// Fake fixture creds, test-only. Must be called before srv.handler()
	// below: handler() captures this snapshot into the auth middleware at
	// call time (see server.go's Start doc comment), exactly like
	// newAuthedServer (handlers_support_bundle_test.go) does for the
	// non-harness tests.
	srv.SetDashboardConfig(runtimeconfig.Dashboard{Username: dashUser, Password: runtimeconfig.NewSecret(dashPass)})

	// Populated-vs-unknown contrast: OAuth/GQL-API/WatchTransport are
	// genuinely OK; PubSub is simply never recorded (omitted from Signals
	// entirely) so /system/status renders it "unknown" — never "ok" — right
	// next to three real OK rows.
	srv.SetHealthProvider(&s55FakeHealthProvider{snap: health.Snapshot{
		ActiveClientID: "TV",
		Signals: []health.Signal{
			{Name: health.SignalOAuth, Status: health.StatusOK, CheckedAt: now.Add(-30 * time.Second)},
			{Name: health.SignalGQLAPI, Status: health.StatusOK, CheckedAt: now.Add(-30 * time.Second)},
			{Name: health.SignalWatchTransport, Status: health.StatusOK, CheckedAt: now.Add(-1 * time.Minute)},
			// SignalPubSub deliberately absent — the unknown-vs-ok contrast.
		},
	}})

	// Drops sync: degraded (LastError set, benign — must render verbatim
	// through Redact) with two DISTINCT clocks (attempt more recent than
	// success).
	srv.SetCampaignsProvider(&s55FakeCampaignsProvider{
		status: drops.SyncStatus{
			LastSyncAt:         now.Add(-5 * time.Minute),
			LastSuccessAt:      now.Add(-3 * time.Hour),
			LastError:          "inventory: HTTP 500",
			IntervalMinutes:    60,
			Runs:               12,
			DashboardCampaigns: 5,
			TrackedCampaigns:   2,
		},
		campaigns: f3BuildCampaigns(),
	})
	srv.SetDropCatalogProvider(&f3Catalog{upcoming: f3BuildUpcoming(), past: f3BuildPast()})
	srv.SetDiscoveryProvider(f3Discovery{})

	// Drops-progress watchdog: enabled, one healthy + one stalled drop.
	srv.SetDropProgressProvider(s55FakeDropProgressProvider{snap: health.ProgressSnapshot{
		Enabled:     true,
		EvaluatedAt: now.Add(-2 * time.Minute),
		Drops: []health.DropProgress{
			{CampaignID: "c1", CampaignName: "Anniversary Drops", DropID: "d1", DropName: "Gold Crate", Status: health.ProgressHealthy},
			{CampaignID: "c2", CampaignName: "Winter Event", DropID: "d3", DropName: "Snow Jacket", Status: health.ProgressStalled},
		},
	}})

	// Lifecycle: a clean, genuinely OK row (running, no error) — contrasts
	// with the degraded drops-sync row and the unknown PubSub row above.
	srv.SetLifecycleController(&s55FakeLifecycleController{snap: lifecycle.Snapshot{
		Observed:  lifecycle.ObservedRunning,
		StartedAt: now.Add(-2 * time.Hour),
		// TransitionStartedAt and LastError deliberately left zero/empty.
	}})
	// lifecycleUpdateState is deliberately NEVER wired: /system/diagnostics
	// must show the honest B8 "build information unavailable" line and no
	// positive update row at all.

	// Process/container resources: CPU+Memory available with plausible
	// values and a short history; Network+Disk deliberately unavailable so
	// their rows render the honest "—" absence marker, never a fabricated 0.
	srv.SetResourceSnapshotProvider(func() resources.Snapshot {
		return resources.Snapshot{
			Available: true,
			SampledAt: time.Now().UTC().Format(time.RFC3339),
			CPU: resources.CPU{
				Available: true, Percent: 12.4, LimitCores: 4,
				History: []float64{0.10, 0.14, 0.12, 0.18, 0.15},
			},
			Memory: resources.Memory{
				Available: true, UsedBytes: 512 * 1024 * 1024, LimitBytes: 2 * 1024 * 1024 * 1024, Percent: 25,
				History: []float64{0.22, 0.24, 0.23, 0.25, 0.25},
			},
			Network: resources.Network{Available: false},
			Disk:    resources.Disk{Available: false},
		}
	})

	srv.SetPolicyProvider(&f3Policy{mode: "smart", rules: map[string]config.DropRule{}})
	srv.SetSettingsProvider(&f3Settings{rt: rt})
	srv.SetSettingsUpdateCallback(func(_ context.Context, _ settings.RuntimeSettings) error { return nil })
	srv.SetFollowedProvider(f3Followed{})

	// Report "running" so the status overlay never covers the pages during
	// browser evidence runs — every provider here is faked, so there is no
	// real startup sequence for it to reflect.
	srv.GetStatusBroadcaster().SetStatus(StatusRunning, "")

	mux := srv.handler()
	httpSrv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = httpSrv.ListenAndServe() }()
	t.Logf("S5-5 evidence harness serving on http://%s (dashboard user=%s)", addr, dashUser)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sig:
	case <-time.After(30 * time.Minute):
	}
	_ = httpSrv.Shutdown(context.Background())
}
