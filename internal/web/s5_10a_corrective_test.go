package web

// S5-10a final accessibility corrective pass: regression guards for the four
// WCAG contrast fixes, the new contentinfo landmark, and the two unscoped
// <th> fixes closed out after S5-10's own audit (see stage-4-visual-design-
// system.md's S5-10 semantic-vocabulary addendum, and PR #167's "S5-10a
// corrective completion" section for the full before/after contrast
// evidence, captured with a real browser against a live server since Go
// cannot evaluate CSS cascade/color-mix/oklch itself).
//
// The SAFE policy status pill (handlers_policy.go:49's "#22c55e") is
// deliberately NOT touched here: live measurement traced its previously
// reported 2.06:1 to a measurement-tool bug (an oklch()-declared background
// on the campaign card's .card wrapper wasn't recognized as opaque, so the
// walk fell through to a distant ancestor's background) rather than a real
// rendering defect — the actual effective contrast is 6.64:1 in both
// themes. See the PR description for the full methodology note.

import (
	"bytes"
	"strings"
	"testing"
)

// TestS5_10aInteractiveSolidClearsAADark guards the dark-theme fix for white
// text-on-accent over --interactive-solid (previously 4.33:1, failing AA;
// .lang-btn.is-active, .theme-btn[aria-pressed], .stat-range-btn.is-active,
// .roi-period-btn.is-active and .btn-bet all share this token pairing).
// --interactive-solid now reuses --prim-night-purple-700 (the existing hover
// primitive, zero new hue) instead of --prim-night-purple-600, and the hover
// pair is derived via color-mix so it stays visually distinct from the new
// resting fill.
func TestS5_10aInteractiveSolidClearsAADark(t *testing.T) {
	css := readEmbeddedStatic(t, "static/css/input.css")

	if !strings.Contains(css, "--interactive-solid: var(--prim-night-purple-700);") {
		t.Error("dark --interactive-solid must resolve through --prim-night-purple-700 (clears 4.5:1 for white text-on-accent; the old --prim-night-purple-600 measured 4.33:1)")
	}
	if strings.Contains(css, "--interactive-solid: var(--prim-night-purple-600);") {
		t.Error("dark --interactive-solid must not regress to the AA-failing --prim-night-purple-600")
	}
	if !strings.Contains(css, "--interactive-solid-hover: color-mix(in srgb, var(--prim-night-purple-700) 82%, black);") {
		t.Error("dark --interactive-solid-hover must stay visually distinct from the new resting fill via a darkened color-mix, not silently collapse to the same value")
	}

	// Light theme already passed (6.71:1) and must not be touched.
	if !strings.Contains(css, "--interactive-solid: var(--interactive);") {
		t.Error("light --interactive-solid must remain untouched (it already cleared AA)")
	}
}

// TestS5_10aTextPlaceholderClearsAABothThemes guards the fix for
// --text-placeholder (empty-art fallback glyph: drops_past.html,
// drops_list.html, drops_upcoming.html, c13_campaign_card.html), which
// measured 1.99:1 dark / 2.98:1 light against --surface-page — failing even
// the 3:1 large-text floor its text-2xl consumers need, and failing the
// full 4.5:1 floor the text-lg consumer (drops_list.html's drop-image
// placeholder) needs regardless of size. Aliased to --text-muted, the same
// treatment already applied to --text-faint, for the same reason: no
// existing palette value between the failing primitive and --text-muted
// clears AA against every actual consumer.
func TestS5_10aTextPlaceholderClearsAABothThemes(t *testing.T) {
	css := readEmbeddedStatic(t, "static/css/input.css")

	if n := strings.Count(css, "--text-placeholder: var(--text-muted);"); n != 2 {
		t.Errorf("--text-placeholder must alias --text-muted in both themes, found %d occurrence(s)", n)
	}
	if strings.Contains(css, "--text-placeholder: var(--prim-night-600);") {
		t.Error("dark --text-placeholder must not regress to the AA-failing --prim-night-600 (1.99:1)")
	}
	if strings.Contains(css, "--text-placeholder: var(--prim-day-600);") {
		t.Error("light --text-placeholder must not regress to the AA-failing --prim-day-600 (2.98:1)")
	}
}

// TestS5_10aStatusOverlayUsesAASafeAccentDark guards the dark-theme fix for
// text-interactive (--interactive) over --surface-card, previously 4.43:1 —
// the startup-overlay heading (#status-title) and its sibling verification
// link both sit directly on the overlay's --surface-card box with no own
// background, so both shared the identical failing pairing and both move to
// --interactive-muted (an existing "secondary-weight accent" token, already
// used elsewhere for link/hover roles), which clears 6.23:1 dark / 5.38:1
// light.
//
// A second, structurally similar verification-URI link lives in the
// non-blocking #lifecycle-auth-banner (updateAuthBanner, shown at
// generation>1) — but its background is a DIFFERENT, translucent
// status-warning tint composited over the page background, not
// --surface-card. --interactive-muted was tried there first since the
// markup looks identical; a real-browser pixel sample of the actual
// composited fill caught that it does fix dark (3.95:1 -> 5.55:1) but
// REGRESSES light (5.10:1, already passing -> 4.09:1, now failing) —
// --interactive-muted's light value is lighter than plain --interactive's,
// which helps against a dark fill and hurts against a light one. --link-text
// (dark: a separate, even-lighter primitive; light: identical to plain
// --interactive) clears both without that trade-off: 7.62:1 dark / 5.10:1
// light (light unchanged from the original passing value). Different
// backgrounds genuinely need different tokens here — same markup shape does
// not imply the same fix.
func TestS5_10aStatusOverlayUsesAASafeAccentDark(t *testing.T) {
	base := readEmbeddedTemplate(t, "templates/base.html")

	if !strings.Contains(base, `id="status-title" class="text-interactive-muted`) {
		t.Error("#status-title must use text-interactive-muted, not the AA-failing text-interactive (4.43:1 dark against surface-card)")
	}
	if !strings.Contains(base, `target="_blank" class="text-interactive-muted font-medium">${status.auth.verificationUri}`) {
		t.Error("the status-overlay verification-URI link shares status-title's exact failing pairing (same surface-card box, no own background) and must get the same fix")
	}
	if !strings.Contains(base, `target="_blank" class="text-link-text font-medium">${status.auth.verificationUri}`) {
		t.Error("the lifecycle-auth-banner verification-URI link must use text-link-text — text-interactive-muted passes dark (5.55:1) but fails light (4.09:1) against this element's actual composited background; text-link-text clears both (7.62:1 / 5.10:1)")
	}
	if strings.Contains(base, `class="text-interactive font-medium">${status.auth.verificationUri}`) {
		t.Error("a verification-URI link still uses the AA-failing plain text-interactive class")
	}
}

// TestS5_10aContentinfoLandmarkPresent guards the missing contentinfo
// landmark base.html carried on all 30 canonical routes (§9 of the design
// doc has required banner/navigation/main/contentinfo since Stage 4). The
// new <footer> is placed as a sibling of .app-shell — not nested inside
// <main>/<aside>/<nav> — so it keeps the browser's implicit contentinfo
// role rather than needing an explicit (and redundant) role attribute; see
// PR #167's browser evidence for the live landmark-tree confirmation across
// all 30 routes in both themes.
func TestS5_10aContentinfoLandmarkPresent(t *testing.T) {
	base := readEmbeddedTemplate(t, "templates/base.html")

	footerIdx := strings.Index(base, "<footer")
	if footerIdx == -1 {
		t.Fatal("base.html must carry a <footer> landmark — no contentinfo lives on any of the 30 canonical routes without it")
	}
	shellCloseIdx := strings.Index(base, `{{template "c17.toast_stack" .}}`)
	if shellCloseIdx == -1 {
		t.Fatal("base.html layout markers moved; update this test's structural assertion")
	}
	if footerIdx >= shellCloseIdx {
		t.Error("the <footer> must appear before the toast-stack marker (i.e. as a body-level sibling of .app-shell, not nested inside main/aside/nav where it would lose its implicit contentinfo role)")
	}
	mainCloseIdx := strings.LastIndex(base[:shellCloseIdx], "</main>")
	if mainCloseIdx == -1 || footerIdx < mainCloseIdx {
		t.Error("the <footer> must appear after </main> closes, not nested inside it")
	}

	footerCloseIdx := strings.Index(base[footerIdx:], "</footer>")
	if footerCloseIdx == -1 {
		t.Fatal("<footer> is never closed")
	}
	footerContent := base[footerIdx : footerIdx+footerCloseIdx+len("</footer>")]
	if strings.Contains(footerContent, "Twitch Drops Miner") {
		t.Error("the new landmark footer must not repeat the sidebar footer's brand name — the sidebar already shows it (and, at normal desktop width, both would be visible on screen at once)")
	}
}

// TestS5_10aDiscoveryListTableHeadersScoped guards discovery_list.html's
// five unscoped <th> cells (channel/game/viewers/drops/status), one of the
// two unscoped-header sites S5-10's own audit flagged and left unfixed.
func TestS5_10aDiscoveryListTableHeadersScoped(t *testing.T) {
	partials := testPartials(t)

	var buf bytes.Buffer
	if err := partials.ExecuteTemplate(&buf, "discovery_list", DiscoveryListData{
		Enabled: true,
		Games:   []string{"World of Tanks"},
		Channels: []DiscoveredChannelView{
			{Login: "watched_channel", Game: "World of Tanks", Status: "watching", ViewersFormatted: "5,400", Watching: true},
		},
	}); err != nil {
		t.Fatalf("render: %v", err)
	}

	assertAllTableHeadersScoped(t, buf.String(), 5)
}

// TestS5_10aDropsPastTableHeadersScoped guards drops_past.html's three
// unscoped <th> cells (started/ended/reward) inside its per-campaign
// instance table, the second unscoped-header site S5-10's audit flagged.
func TestS5_10aDropsPastTableHeadersScoped(t *testing.T) {
	partials := testPartials(t)

	var buf bytes.Buffer
	if err := partials.ExecuteTemplate(&buf, "drops_past", DropsPastData{
		Groups: []PastCampaignGroup{
			{
				Name: "Anniversary Drops", Count: 1, ClaimedCount: 1,
				Instances: []PastInstanceView{{StartLabel: "01.01", EndLabel: "02.01", Claimed: true, StatusLabel: "Claimed"}},
			},
		},
	}); err != nil {
		t.Fatalf("render: %v", err)
	}

	assertAllTableHeadersScoped(t, buf.String(), 3)
}

// TestS5_10aDiscoveryTableHorizontallyScrollable guards a mobile-viewport
// regression the 30-route responsive sweep caught while verifying this
// pass's other fixes: discovery_list.html's table had no overflow-x
// wrapper, so at 375px (RU, whose column copy runs longer than EN) the
// table forced the document 2px wider than the viewport. drops_past.html's
// sibling table already uses this exact wrapper pattern.
func TestS5_10aDiscoveryTableHorizontallyScrollable(t *testing.T) {
	tmpl := readEmbeddedTemplate(t, "templates/partials/discovery_list.html")

	tableIdx := strings.Index(tmpl, `<table class="w-full text-sm">`)
	if tableIdx == -1 {
		t.Fatal("discovery_list.html's populated-state table markup moved; update this test")
	}
	wrapperIdx := strings.LastIndex(tmpl[:tableIdx], `<div class="overflow-x-auto">`)
	if wrapperIdx == -1 {
		t.Fatal("discovery_list.html's table must be wrapped in an overflow-x-auto container, or it overflows the viewport on narrow screens")
	}
}

// assertAllTableHeadersScoped counts every <th rendered and fails unless
// each one carries scope="col" — a plain substring count rather than an
// HTML parse, matching this package's existing string-assertion style
// (e.g. TestOnAccentTokensFixSolidAccentButtons).
func assertAllTableHeadersScoped(t *testing.T, out string, wantHeaders int) {
	t.Helper()
	// "<th " (trailing space, every real cell here carries a class attribute)
	// deliberately excludes "<thead>", which is also a "<th"-prefixed
	// substring and would otherwise inflate the count.
	total := strings.Count(out, "<th ")
	scoped := strings.Count(out, `<th scope="col"`)
	if total == 0 {
		t.Fatalf("no <th cells rendered at all — fixture produced no table:\n%s", out)
	}
	if total < wantHeaders {
		t.Fatalf("rendered %d <th cells, want at least %d — fixture may not be exercising the table", total, wantHeaders)
	}
	if scoped != total {
		t.Errorf("%d of %d <th cells carry scope=\"col\"; every column header must be scoped:\n%s", scoped, total, out)
	}
}
