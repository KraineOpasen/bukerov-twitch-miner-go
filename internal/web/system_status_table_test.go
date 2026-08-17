package web

// Dashboard Stage 1, first seam: /system/status renders its subsystems as a
// dense evidence TABLE — the composition frozen by owner decision 10 and by
// docs/dashboard/stage-4-visual-design-system.md §11 route 23 ("Components
// C4, C0 per row … Gates: freshness on every row mandatory").
//
// These tests pin the contracts that composition is actually FOR, so a later
// refactor cannot quietly regress them:
//
//   - freshness is rendered on EVERY row, and a row with no reading says so
//     out loud instead of dropping the line (which would make "never
//     checked" look identical to "this row has no clock concept");
//   - status is icon + text + colour, never colour alone;
//   - an unknown subsystem can never borrow the ok tier;
//   - the evidence a health.Signal already carries (stage, detail, error
//     code) reaches the page instead of being discarded, and is redacted on
//     the way out;
//   - the miner's own state stays a read-only echo OUTSIDE the subsystem
//     table, with no mutation control anywhere on the page.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/health"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/lifecycle"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/resources"
)

// sstRows returns the inner HTML of every subsystem table row, in render
// order. Rows are located by the data-system-row marker the template stamps
// on each <tr>, so an assertion can be scoped to ONE row rather than to the
// whole page — a page-wide substring check cannot tell "every row has
// freshness" from "one row has freshness and four have none".
func sstRows(t *testing.T, body string) []string {
	t.Helper()
	const (
		open  = `data-system-row>`
		close = `</tr>`
	)
	var rows []string
	for rest := body; ; {
		i := strings.Index(rest, open)
		if i < 0 {
			return rows
		}
		rest = rest[i+len(open):]
		end := strings.Index(rest, close)
		if end < 0 {
			t.Fatalf("unterminated subsystem row in rendered body")
		}
		rows = append(rows, rest[:end])
		rest = rest[end+len(close):]
	}
}

// sstRender renders /system/status through the real page handler.
func sstRender(t *testing.T, srv *Server, lang string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/system/status", nil)
	req.AddCookie(&http.Cookie{Name: langCookieName, Value: lang})
	rec := httptest.NewRecorder()
	srv.handleSystemStatusPage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s /system/status = %d, want 200", lang, rec.Code)
	}
	return rec.Body.String()
}

// sstMixedServer wires a deliberately mixed fixture: OAuth has been checked,
// PubSub never has, drops sync has both of its clocks, and the resource
// sampler has produced a snapshot. That mix is what makes the
// "freshness on EVERY row" assertion non-vacuous — the page must populate
// the column for the checked rows AND for the never-checked one.
func sstMixedServer(t *testing.T) *Server {
	t.Helper()
	srv := newRenderServer(t)
	srv.SetHealthProvider(&s55FakeHealthProvider{snap: health.Snapshot{
		Signals: []health.Signal{
			{Name: health.SignalOAuth, Status: health.StatusOK, CheckedAt: time.Now().Add(-30 * time.Second)},
			{Name: health.SignalGQLAPI, Status: health.StatusDegraded, CheckedAt: time.Now().Add(-5 * time.Minute), Stage: "refresh"},
			// PubSub deliberately absent from the snapshot: never recorded.
		},
	}})
	srv.SetResourceSnapshotProvider(func() resources.Snapshot {
		return resources.Snapshot{
			Available: true,
			SampledAt: time.Now().Add(-5 * time.Second).UTC().Format(time.RFC3339),
			CPU:       resources.CPU{Available: true, Percent: 1.5, LimitCores: 8},
		}
	})
	return srv
}

// TestSystemStatusFreshnessOnEveryRow proves the frozen "freshness on every
// row" gate: every rendered subsystem row carries a freshness cell, and a
// row with no reading renders the explicit no-reading note rather than an
// empty cell or a dropped line.
func TestSystemStatusFreshnessOnEveryRow(t *testing.T) {
	body := sstRender(t, sstMixedServer(t), "en")
	tr := enTR(t)
	none := tr("system.status.freshness.none")

	rows := sstRows(t, body)
	if len(rows) != 5 {
		t.Fatalf("expected 5 subsystem rows (OAuth, GQL, PubSub, drops sync, resources), got %d", len(rows))
	}

	withReading, withoutReading := 0, 0
	for i, row := range rows {
		if !strings.Contains(row, "data-system-freshness") {
			t.Errorf("row %d rendered no freshness cell at all", i)
			continue
		}
		switch {
		case strings.Contains(row, none):
			withoutReading++
		case strings.Contains(row, tr("common.ago")):
			withReading++
		default:
			t.Errorf("row %d freshness cell carries neither an age nor the %q note: %s", i, none, row)
		}
	}

	// Non-vacuity in both directions: the fixture has checked rows and a
	// never-checked one, so the gate is proven for both cases at once.
	if withReading == 0 {
		t.Error("fixture drift: no row rendered a real age, so the gate is untested for readings")
	}
	if withoutReading == 0 {
		t.Error("fixture drift: no row rendered the no-reading note, so the gate is untested for absences")
	}
}

// TestSystemStatusStatusIsIconTextAndColour proves every subsystem status is
// encoded as icon + text + tier (Stage 4 §3 P4), never colour alone.
func TestSystemStatusStatusIsIconTextAndColour(t *testing.T) {
	body := sstRender(t, sstMixedServer(t), "en")

	for i, row := range sstRows(t, body) {
		if !strings.Contains(row, `class="c10-badge c10-badge--`) {
			t.Errorf("row %d renders no C10 status badge (colour alone is forbidden)", i)
			continue
		}
		if !strings.Contains(row, `class="c10-badge-icon"`) {
			t.Errorf("row %d status badge carries no icon", i)
		}
		label := `<span class="c10-badge-label">`
		j := strings.Index(row, label)
		if j < 0 {
			t.Errorf("row %d status badge carries no label element", i)
			continue
		}
		text := row[j+len(label):]
		if k := strings.Index(text, "</span>"); k <= 0 {
			t.Errorf("row %d status badge label is empty — the status must always be readable as text", i)
		}
	}
}

// TestSystemStatusUnknownRowNeverUsesOkBadge proves the S-UNK invariant
// survives the move to badges: with nothing wired, no row may render the ok
// tier, and every row must render the neutral one.
func TestSystemStatusUnknownRowNeverUsesOkBadge(t *testing.T) {
	srv := newRenderServer(t)
	srv.SetHealthProvider(&s55FakeHealthProvider{snap: health.Snapshot{}})
	// lifecycleController, campaignsProvider, resourceSnapshot: left nil.

	body := sstRender(t, srv, "en")
	rows := sstRows(t, body)
	if len(rows) != 5 {
		t.Fatalf("expected 5 subsystem rows, got %d", len(rows))
	}
	for i, row := range rows {
		if strings.Contains(row, "c10-badge--ok") {
			t.Errorf("row %d rendered the ok badge tier with no evidence behind it", i)
		}
		if !strings.Contains(row, "c10-badge--neutral") {
			t.Errorf("row %d must render the neutral tier when nothing is known, got: %s", i, row)
		}
	}
	if strings.Contains(body, "health-sev-ok") {
		t.Error("no signal is genuinely OK in this fixture; health-sev-ok must never appear")
	}
}

// TestSystemStatusSurfacesSignalEvidence proves the page renders the stage,
// detail and error code a health.Signal already carries. Before this seam
// /system/status read only Status and CheckedAt and silently discarded all
// three, even though the legacy /health page has always rendered them.
func TestSystemStatusSurfacesSignalEvidence(t *testing.T) {
	srv := newRenderServer(t)
	srv.SetHealthProvider(&s55FakeHealthProvider{snap: health.Snapshot{
		Signals: []health.Signal{{
			Name:      health.SignalOAuth,
			Status:    health.StatusDegraded,
			CheckedAt: time.Now().Add(-time.Minute),
			Stage:     "token_refresh",
			Detail:    "retrying after a transient failure",
			ErrorCode: "gql_429",
		}},
	}})

	body := sstRender(t, srv, "en")
	for _, want := range []string{"token_refresh", "retrying after a transient failure", "gql_429"} {
		if !strings.Contains(body, want) {
			t.Errorf("the OAuth row must surface its own evidence %q", want)
		}
	}
}

// TestSystemStatusRedactsSignalEvidence proves the newly surfaced signal
// evidence crosses the boundary through supportbundle.Redact, exactly like
// the lifecycle and drops-sync errors this page already redacted. The paired
// benign control must still render verbatim.
func TestSystemStatusRedactsSignalEvidence(t *testing.T) {
	for _, canary := range s54SensitiveLastErrorCanaries {
		t.Run(canary.name, func(t *testing.T) {
			srv := newRenderServer(t)
			srv.SetHealthProvider(&s55FakeHealthProvider{snap: health.Snapshot{
				Signals: []health.Signal{{
					Name:      health.SignalOAuth,
					Status:    health.StatusFailed,
					CheckedAt: time.Now(),
					Detail:    canary.value,
				}},
			}})
			body := sstRender(t, srv, "en")

			if strings.Contains(body, canary.marker) {
				t.Errorf("rendered body leaked the raw canary marker %q", canary.marker)
			}
			for _, absent := range canary.alsoAbsent {
				if strings.Contains(body, absent) {
					t.Errorf("rendered body leaked %q from the raw signal detail", absent)
				}
			}
			if !strings.Contains(body, "[REDACTED]") {
				t.Error("expected the redacted marker [REDACTED] in the rendered body")
			}
		})
	}

	srv := newRenderServer(t)
	srv.SetHealthProvider(&s55FakeHealthProvider{snap: health.Snapshot{
		Signals: []health.Signal{{
			Name:      health.SignalOAuth,
			Status:    health.StatusFailed,
			CheckedAt: time.Now(),
			Detail:    s54BenignLastError,
		}},
	}})
	body := sstRender(t, srv, "en")
	if !strings.Contains(body, s54BenignLastError) {
		t.Errorf("expected the benign signal detail %q to render verbatim", s54BenignLastError)
	}
	if strings.Contains(body, "[REDACTED]") {
		t.Error("a genuinely benign signal detail must not trigger redaction")
	}
}

// TestSystemStatusTableSemantics proves the dense surface is a real table
// with the accessibility contract Stage 4 §9 requires of one: a caption, a
// scoped column header per column, and a row header per subsystem.
func TestSystemStatusTableSemantics(t *testing.T) {
	body := sstRender(t, sstMixedServer(t), "en")
	tr := enTR(t)

	if !strings.Contains(body, `<table class="c4-table" id="system-subsystems">`) {
		t.Error("the subsystem surface must be a semantic <table>, not a card grid")
	}
	if !strings.Contains(body, `<caption class="visually-hidden">`+tr("system.status.table.caption")) {
		t.Error("the subsystem table must carry its localized visually-hidden caption")
	}
	for _, col := range []string{
		tr("system.status.col.subsystem"),
		tr("system.status.col.status"),
		tr("system.status.col.freshness"),
		tr("system.status.col.detail"),
	} {
		if !strings.Contains(body, `<th scope="col">`+col) {
			t.Errorf("column header %q must be a <th scope=\"col\">", col)
		}
	}
	if n := strings.Count(body, `<th scope="col">`); n != 4 {
		t.Errorf("expected exactly 4 scoped column headers, found %d", n)
	}
	if n := strings.Count(body, `<th scope="row"`); n != 5 {
		t.Errorf("expected one <th scope=\"row\"> per subsystem row (5), found %d", n)
	}
}

// TestSystemStatusLifecycleIsReadOnlyEchoOutsideTable proves the miner's own
// state is rendered as an echo ABOVE the subsystem table (never as one of
// its rows) and that the page still offers no way to change it.
func TestSystemStatusLifecycleIsReadOnlyEchoOutsideTable(t *testing.T) {
	srv := sstMixedServer(t)
	srv.SetLifecycleController(&s55FakeLifecycleController{snap: lifecycle.Snapshot{
		Observed:  lifecycle.ObservedRunning,
		StartedAt: time.Now().Add(-2 * time.Hour),
	}})
	body := sstRender(t, srv, "en")
	tr := enTR(t)

	label := tr("system.status.lifecycle.label")
	if !strings.Contains(body, label) {
		t.Fatalf("the lifecycle echo %q must still render on the page", label)
	}
	for i, row := range sstRows(t, body) {
		if strings.Contains(row, label) {
			t.Errorf("row %d is the lifecycle row: the echo belongs above the subsystem table, not inside it", i)
		}
	}
	if !strings.Contains(body, tr("system.status.lifecycle.controls_note")) {
		t.Error("the echo must name where the lifecycle controls actually live")
	}
	// Scoped to lifecycle affordances specifically: the page legitimately
	// carries the chrome's own hx-post language switcher, so a blanket
	// hx-post ban would be false. What must never appear is a way to
	// command the miner from here.
	for _, forbidden := range []string{"/api/lifecycle", "<form", "hx-target=\"#lifecycle-panel\""} {
		if strings.Contains(body, forbidden) {
			t.Errorf("/system/status must carry no lifecycle mutation affordance, found %q", forbidden)
		}
	}
}

// TestSystemStatusResourcesRowCarriesSampledFreshness proves the resources
// row is a first-class table row whose freshness comes from the sampler's
// own SampledAt — a field already published by /api/resources that this page
// simply never read, which is why its row used to have no clock at all.
func TestSystemStatusResourcesRowCarriesSampledFreshness(t *testing.T) {
	body := sstRender(t, sstMixedServer(t), "en")
	tr := enTR(t)

	rows := sstRows(t, body)
	last := rows[len(rows)-1]
	if !strings.Contains(last, tr("system.status.resources.heading")) {
		t.Fatalf("the last subsystem row must be the resources row, got: %s", last)
	}
	got, ok := s55FreshnessValue(last, tr("system.status.resources.sampled_label"))
	if !ok {
		t.Fatalf("the resources row must bind an age to the %q label", tr("system.status.resources.sampled_label"))
	}
	if !strings.HasSuffix(got, tr("common.ago")) {
		t.Errorf("resources freshness %q must be a localized elapsed age", got)
	}

	// An unavailable sampler must degrade to unknown + no reading, never ok.
	srv := newRenderServer(t)
	srv.SetResourceSnapshotProvider(func() resources.Snapshot { return resources.UnavailableSnapshot() })
	rows = sstRows(t, sstRender(t, srv, "en"))
	last = rows[len(rows)-1]
	if !strings.Contains(last, tr("system.status.freshness.none")) {
		t.Error("an unavailable sampler must render the no-reading note, not an empty freshness cell")
	}
	if strings.Contains(last, "c10-badge--ok") {
		t.Error("an unavailable sampler must never render the ok tier")
	}
}

// TestSystemStatusTableLocaleKeysPresentAndTranslated proves every locale key
// this seam introduced exists in both catalogs, is not an echoed key name,
// and is genuinely translated rather than copy-pasted across languages.
func TestSystemStatusTableLocaleKeysPresentAndTranslated(t *testing.T) {
	en, ru := enTR(t), ruTR(t)
	for _, key := range []string{
		"system.status.col.subsystem",
		"system.status.col.status",
		"system.status.col.freshness",
		"system.status.col.detail",
		"system.status.table.caption",
		"system.status.freshness.none",
		"system.status.resources.sampled_label",
		"system.status.lifecycle.controls_note",
		"system.status.build.label",
	} {
		e, r := en(key), ru(key)
		switch {
		case e == "" || r == "":
			t.Errorf("%q is empty in EN (%q) or RU (%q)", key, e, r)
		case e == key || r == key:
			t.Errorf("%q echoed its own key back (EN %q, RU %q)", key, e, r)
		case e == r:
			t.Errorf("%q has identical EN/RU text %q — looks untranslated", key, e)
		}
	}
}
