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
	"regexp"
	"slices"
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
	tr := enTR(t)

	rows := sstRows(t, body)
	if len(rows) == 0 {
		t.Fatal("no subsystem rows rendered")
	}
	oauth := ""
	for _, row := range rows {
		if strings.Contains(row, tr("system.status.oauth.label")) {
			oauth = row
			break
		}
	}
	if oauth == "" {
		t.Fatal("the OAuth row was not rendered")
	}
	// Row-scoped: a regression that hangs the OAuth signal's evidence on a
	// different subsystem must fail here, which a page-wide Contains cannot
	// detect. The label prefixes are pinned too, so a locale-key typo shows
	// up as a failure rather than as a raw key on the page.
	for _, want := range []string{
		tr("health.card.stage") + " token_refresh",
		"retrying after a transient failure",
		tr("health.card.error_code") + " gql_429",
	} {
		if !strings.Contains(oauth, want) {
			t.Errorf("the OAuth row must surface its own evidence %q; row was: %s", want, oauth)
		}
	}
	for _, other := range rows {
		if other == oauth {
			continue
		}
		if strings.Contains(other, "gql_429") {
			t.Errorf("another row carries the OAuth signal's error code: %s", other)
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

	if !strings.Contains(body, `<caption class="visually-hidden">`+tr("system.status.table.caption")) {
		t.Error("the subsystem table must carry its localized visually-hidden caption")
	}
	// Scoped to this table, not the whole response: base.html and every
	// partial parsed into the page set also render into `body`, so a page-wide
	// count would break this page's assertions from a change that never
	// touched this page.
	start := strings.Index(body, `<table class="c4-table" id="system-subsystems">`)
	if start < 0 {
		t.Fatal("the subsystem surface must be a semantic <table>, not a card grid")
	}
	end := strings.Index(body[start:], "</table>")
	if end < 0 {
		t.Fatal("unterminated subsystem table")
	}
	table := body[start : start+end]

	// Attribute-order tolerant: some headers also carry a width class, so
	// match the element rather than one exact byte sequence.
	colHeader := regexp.MustCompile(`<th scope="col"[^>]*>([^<]*)`)
	var headers []string
	for _, m := range colHeader.FindAllStringSubmatch(table, -1) {
		headers = append(headers, strings.TrimSpace(m[1]))
	}
	for _, col := range []string{
		tr("system.status.col.subsystem"),
		tr("system.status.col.status"),
		tr("system.status.col.freshness"),
		tr("system.status.col.detail"),
	} {
		if !slices.Contains(headers, col) {
			t.Errorf("column header %q must be a <th scope=\"col\">; got %v", col, headers)
		}
	}
	if len(headers) != 4 {
		t.Errorf("expected exactly 4 scoped column headers, found %d: %v", len(headers), headers)
	}
	if n := strings.Count(table, `<th scope="row"`); n != 5 {
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
	// Moving the row out of the table must not move it out of the
	// mandatory-freshness gate: with a real StartedAt the band states its
	// age, and with no controller at all it states the absence in words.
	if !strings.Contains(body, tr("system.status.lifecycle.started_label")) {
		t.Error("the echo must state the age of its own evidence")
	}
	bare := sstRender(t, sstMixedServer(t), "en") // no lifecycle controller wired
	head := bare[:strings.Index(bare, `id="system-subsystems"`)]
	if !strings.Contains(head, tr("system.status.freshness.none")) {
		t.Error("with no lifecycle controller the echo must say it has no reading, not fall silent")
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
	if len(rows) == 0 {
		t.Fatal("no subsystem rows rendered")
	}
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
	if len(rows) == 0 {
		t.Fatal("no subsystem rows rendered for the unavailable sampler")
	}
	last = rows[len(rows)-1]
	if !strings.Contains(last, tr("system.status.freshness.none")) {
		t.Error("an unavailable sampler must render the no-reading note, not an empty freshness cell")
	}
	if strings.Contains(last, "c10-badge--ok") {
		t.Error("an unavailable sampler must never render the ok tier")
	}
}

// TestSystemStatusFreshnessUsesProvenanceChip proves route 23's "C0 per row"
// component requirement is met by the real component, not by an ad-hoc
// lookalike: every freshness cell renders a c0.provenance_chip, and a row
// with no reading takes the chip's own S-UNK variant rather than being
// eyeballed from a bare dash.
func TestSystemStatusFreshnessUsesProvenanceChip(t *testing.T) {
	body := sstRender(t, sstMixedServer(t), "en")
	tr := enTR(t)

	rows := sstRows(t, body)
	if len(rows) != 5 {
		t.Fatalf("expected 5 subsystem rows, got %d", len(rows))
	}
	// Pair the chip variant to the row's OWN evidence: a row with a reading
	// must be live, a row without one must be the chip's S-UNK variant. The
	// counters below then prove both cases were actually exercised, so
	// neither branch can rot into a vacuous pass.
	live, unknown := 0, 0
	for i, row := range rows {
		if !strings.Contains(row, `class="c0-chip`) {
			t.Errorf("row %d renders no C0 provenance chip", i)
			continue
		}
		hasReading := strings.Contains(row, tr("common.ago"))
		switch {
		case hasReading && strings.Contains(row, "c0-chip--unknown"):
			t.Errorf("row %d has a reading but renders the S-UNK chip variant", i)
		case hasReading:
			live++
		case strings.Contains(row, "c0-chip--unknown"):
			unknown++
		default:
			t.Errorf("row %d has no reading but does not render the S-UNK chip variant: %s", i, row)
		}
	}
	if live == 0 || unknown == 0 {
		t.Errorf("fixture drift: need both cases exercised, got %d live and %d unknown chips", live, unknown)
	}
}

// TestSystemStatusResourcesRowMakesNoHealthClaim proves the resources row
// never borrows the health vocabulary. resources.Snapshot reports Available
// on its FIRST sample while the per-section rates still need a second one,
// so deriving "ok" from availability would render a green row sitting above
// four em-dashes — and no health provider judges the sampler at all.
func TestSystemStatusResourcesRowMakesNoHealthClaim(t *testing.T) {
	tr := enTR(t)

	// The exact partial state: the snapshot is available, every section is not.
	srv := newRenderServer(t)
	srv.SetResourceSnapshotProvider(func() resources.Snapshot {
		return resources.Snapshot{
			Available: true,
			SampledAt: time.Now().Add(-3 * time.Second).UTC().Format(time.RFC3339),
			// CPU/Memory/Network/Disk all zero-value: Available false.
		}
	})
	rows := sstRows(t, sstRender(t, srv, "en"))
	last := rows[len(rows)-1]
	if !strings.Contains(last, tr("system.status.resources.heading")) {
		t.Fatalf("the last subsystem row must be the resources row, got: %s", last)
	}
	if strings.Contains(last, "c10-badge--ok") {
		t.Error("a first-sample snapshot with no section data must not render an ok tier")
	}
	if strings.Contains(last, tr("health.status.ok")) {
		t.Error("the resources row must not borrow the health vocabulary")
	}
	if !strings.Contains(last, tr("system.status.resources.pointer")) {
		t.Error("the resources row must point at the values below it")
	}
	// Non-vacuity: the row is still present and still stamped.
	if _, ok := s55FreshnessValue(last, tr("system.status.resources.sampled_label")); !ok {
		t.Error("the resources row must still carry its sampled freshness")
	}
}

// TestSystemStatusCardsMirrorTableRows proves route 23's Transform T: the
// <lg card representation renders the SAME subsystems as the >=lg table, so
// no row and no evidence silently disappears at a narrower viewport.
func TestSystemStatusCardsMirrorTableRows(t *testing.T) {
	for _, lang := range []string{"en", "ru"} {
		body := sstRender(t, sstMixedServer(t), lang)

		tableRows := sstRows(t, body)
		cards := strings.Count(body, "data-system-card")
		if cards != len(tableRows) {
			t.Errorf("%s: %d table rows but %d cards — the two representations must carry the same rows", lang, len(tableRows), cards)
		}
		if !strings.Contains(body, `class="c4-table-card mb-6 hidden lg:block"`) {
			t.Errorf("%s: the table must be the >=lg representation only", lang)
		}
		// The toggle is on the wrapper, not on .c3-roster-cards itself: that
		// component sets display:flex, so a display utility on the same
		// element would be a specificity coin-flip (proven in the browser —
		// the cards stayed visible at >=lg until the wrapper was added).
		if !strings.Contains(body, `<div class="lg:hidden">`) {
			t.Errorf("%s: the card representation must be gated by a wrapper", lang)
		}
		if strings.Contains(body, `class="c3-roster-cards mb-6 lg:hidden"`) {
			t.Errorf("%s: a display utility on .c3-roster-cards itself is a specificity coin-flip", lang)
		}
		// Every subsystem label must appear in both representations.
		cardBlock := body[strings.Index(body, "system-subsystem-cards"):]
		tr := func(k string) string {
			if lang == "ru" {
				return ruTR(t)(k)
			}
			return enTR(t)(k)
		}
		for _, key := range []string{
			"system.status.oauth.label", "system.status.gql.label", "system.status.pubsub.label",
			"system.status.drops_sync.label", "system.status.resources.heading",
		} {
			if !strings.Contains(cardBlock, tr(key)) {
				t.Errorf("%s: card representation is missing subsystem %q", lang, tr(key))
			}
		}
	}
}

// TestSystemStatusScrollContainerIsReachableByKeyboard proves the table's
// horizontal scroll container is focusable and named. It contains no
// interactive element of its own, so without this a keyboard-only user on a
// browser that does not auto-focus scroll regions could never reach the
// last column at a narrow width (WCAG 2.1.1). The repo's own precedent is
// the log viewport in logs.html.
func TestSystemStatusScrollContainerIsReachableByKeyboard(t *testing.T) {
	body := sstRender(t, sstMixedServer(t), "en")
	tr := enTR(t)

	i := strings.Index(body, `class="c4-table-card`)
	if i < 0 {
		t.Fatal("the scroll container is missing")
	}
	tag := body[i:]
	if j := strings.Index(tag, ">"); j > 0 {
		tag = tag[:j]
	}
	for _, want := range []string{`tabindex="0"`, `role="region"`, `aria-label="` + tr("system.status.table.region_label")} {
		if !strings.Contains(tag, want) {
			t.Errorf("the scroll container must carry %s; got <div %s>", want, tag)
		}
	}
}

// TestSystemDiagnosticsCanarySurfacesEvidence pins the deliberate cross-page
// consequence of fixing systemSignalRow: /system/diagnostics builds its
// watch-transport canary row through the same shared builder, so it too now
// shows the stage/detail/error-code the signal already carried and the page
// used to discard. Without this test that change is invisible and untested
// on the page it also affects.
func TestSystemDiagnosticsCanarySurfacesEvidence(t *testing.T) {
	srv := newRenderServer(t)
	srv.SetHealthProvider(&s55FakeHealthProvider{snap: health.Snapshot{
		Signals: []health.Signal{{
			Name:      health.SignalWatchTransport,
			Status:    health.StatusDegraded,
			CheckedAt: time.Now().Add(-time.Minute),
			Stage:     "beacon",
			Detail:    "watch beacon accepted",
			ErrorCode: "canary_slow",
		}},
	}})

	req := httptest.NewRequest(http.MethodGet, "/system/diagnostics", nil)
	req.AddCookie(&http.Cookie{Name: langCookieName, Value: "en"})
	rec := httptest.NewRecorder()
	srv.handleSystemDiagnosticsPage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/system/diagnostics = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	tr := enTR(t)

	for _, want := range []string{
		tr("health.card.stage") + " beacon",
		"watch beacon accepted",
		tr("health.card.error_code") + " canary_slow",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the canary row must surface its own evidence %q", want)
		}
	}
	// The diagnostics page keeps its own markup: it must NOT have silently
	// acquired the status page's table.
	if strings.Contains(body, `id="system-subsystems"`) {
		t.Error("/system/diagnostics must keep its own composition, not the status table")
	}
}

// TestSystemStatusBuildBandShowsVersionOnly proves the build band renders the
// version and nothing it cannot evidence: no commit SHA, no image digest, no
// build time, and above all no "up to date" claim — /system/diagnostics owns
// the update statement and states the absence explicitly.
func TestSystemStatusBuildBandShowsVersionOnly(t *testing.T) {
	body := sstRender(t, sstMixedServer(t), "en")
	tr := enTR(t)

	if !strings.Contains(body, tr("system.status.build.label")) {
		t.Error("the build band must render")
	}
	for _, forbidden := range []string{
		"up to date", "актуальная версия",
		tr("system.diagnostics.build_info_unavailable"),
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("/system/status must not claim %q — that statement belongs to /system/diagnostics", forbidden)
		}
	}
	// The band links to the page that owns build/update detail rather than
	// restating it.
	if !strings.Contains(body, `href="/system/diagnostics"`) {
		t.Error("the build band must link to the owner of build/update detail")
	}
}
