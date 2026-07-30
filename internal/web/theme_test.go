package web

import (
	"regexp"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/i18n"
)

// This file pins the F1 "theme-token-consolidation" behavior: the dark/
// light/system theme toggle, its pre-paint FOUC bootstrap, the single
// consolidated token source in input.css, and the ApexCharts theme hook on
// statistics.html/streamer.html. See design-spec.md §6 for the source list.

// readEmbeddedStatic returns the raw bytes of a static asset as they ship in
// the binary (from the same embed.FS the server serves), analogous to
// readEmbeddedTemplate (sidebar_countdown_test.go) but for staticFS.
func readEmbeddedStatic(t *testing.T, name string) string {
	t.Helper()
	b, err := staticFS.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// allTemplateNames enumerates every embedded template — base/page templates
// plus partials — so token-definition and external-asset checks cover the
// whole surface, not just base.html.
func allTemplateNames(t *testing.T) []string {
	t.Helper()
	var names []string
	entries, err := templatesFS.ReadDir("templates")
	if err != nil {
		t.Fatalf("read templates dir: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".html") {
			names = append(names, "templates/"+e.Name())
		}
	}
	partials, err := templatesFS.ReadDir("templates/partials")
	if err != nil {
		t.Fatalf("read templates/partials dir: %v", err)
	}
	for _, e := range partials {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".html") {
			names = append(names, "templates/partials/"+e.Name())
		}
	}
	return names
}

// ---------------------------------------------------------------------
// 1. Bootstrap placement (§6.1)
// ---------------------------------------------------------------------

func TestThemeBootstrapPlacement(t *testing.T) {
	base := readEmbeddedTemplate(t, "templates/base.html")

	firstScript := strings.Index(base, "<script")
	appCSSLink := strings.Index(base, `<link rel="stylesheet" href="/static/css/app.css">`)
	if firstScript < 0 || appCSSLink < 0 {
		t.Fatalf("expected both a <script> and the app.css <link> in base.html (script=%d link=%d)", firstScript, appCSSLink)
	}
	if firstScript > appCSSLink {
		t.Error("the theme bootstrap script must be the FIRST <script> in <head>, before the app.css <link>")
	}

	end := strings.Index(base[firstScript:], "</script>")
	if end < 0 {
		t.Fatal("the first <script> in base.html has no closing tag")
	}
	bootstrap := base[firstScript : firstScript+end]

	for _, want := range []string{
		"miner-theme",                   // localStorage key
		"'dark'", "'light'", "'system'", // the three-value whitelist
		"data-theme", "data-theme-mode",
		"localStorage.getItem",
	} {
		if !strings.Contains(bootstrap, want) {
			t.Errorf("bootstrap script missing %q", want)
		}
	}
}

// ---------------------------------------------------------------------
// 2. Modes/invariants (§6.2)
// ---------------------------------------------------------------------

// themeControllerScript extracts the theme controller <script> block (the
// one wiring clicks + the OS-preference listener), distinct from the
// pre-paint bootstrap script and from the unrelated status-overlay script
// that also happens to call location.reload elsewhere in base.html.
func themeControllerScript(t *testing.T, base string) string {
	t.Helper()
	const marker = "// Theme controller:"
	start := strings.Index(base, marker)
	if start < 0 {
		t.Fatal("theme controller script not found (missing the '// Theme controller:' marker)")
	}
	end := strings.Index(base[start:], "</script>")
	if end < 0 {
		t.Fatal("theme controller script has no closing </script>")
	}
	return base[start : start+end]
}

func TestThemeControllerInvariants(t *testing.T) {
	base := readEmbeddedTemplate(t, "templates/base.html")
	ctrl := themeControllerScript(t, base)

	if !strings.Contains(ctrl, "matchMedia('(prefers-color-scheme: light)')") {
		t.Error("controller must resolve system preference via matchMedia('(prefers-color-scheme: light)')")
	}
	if !strings.Contains(ctrl, `data-theme-mode') === 'system'`) {
		t.Error("the OS-preference change handler must be guarded to fire only when the stored mode is 'system'")
	}
	if strings.Contains(ctrl, "location.reload") {
		t.Error("theme switching must never reload the page")
	}
	if !strings.Contains(ctrl, "localStorage.setItem") {
		t.Error("the controller must persist the chosen mode via localStorage.setItem")
	}
	if !strings.Contains(ctrl, "addEventListener('change'") {
		t.Error("the controller must listen for prefers-color-scheme changes")
	}
}

// The bootstrap script itself must also never reload the page.
func TestThemeBootstrapNeverReloads(t *testing.T) {
	base := readEmbeddedTemplate(t, "templates/base.html")
	firstScript := strings.Index(base, "<script")
	end := strings.Index(base[firstScript:], "</script>")
	bootstrap := base[firstScript : firstScript+end]
	if strings.Contains(bootstrap, "location.reload") {
		t.Error("the FOUC bootstrap script must never reload the page")
	}
}

// ---------------------------------------------------------------------
// 3. Toggle accessibility (§6.3)
// ---------------------------------------------------------------------

// themeToggleBlock extracts just the #theme-toggle markup (up to the
// adjacent RU/EN language group), so counts below aren't polluted by the
// rest of the page.
func themeToggleBlock(t *testing.T, base string) string {
	t.Helper()
	// Start from the aria-label attribute (which precedes id="theme-toggle"
	// in the opening tag), so it — and everything after it — is included.
	start := strings.Index(base, `aria-label="{{ t "a11y.theme" }}"`)
	if start < 0 {
		t.Fatal(`no aria-label="{{ t "a11y.theme" }}" found in base.html`)
	}
	rest := base[start:]
	end := strings.Index(rest, `aria-label="{{ t "a11y.language" }}"`)
	if end < 0 {
		t.Fatal("could not bound the theme toggle block (RU/EN language group not found after it)")
	}
	return rest[:end]
}

func TestThemeToggleAccessibility(t *testing.T) {
	base := readEmbeddedTemplate(t, "templates/base.html")

	if n := strings.Count(base, `id="theme-toggle"`); n != 1 {
		t.Fatalf("expected exactly one #theme-toggle, found %d", n)
	}

	block := themeToggleBlock(t, base)

	if !strings.Contains(block, `role="group"`) {
		t.Error("theme toggle container must carry role=\"group\"")
	}
	if !strings.Contains(block, `aria-label="{{ t "a11y.theme" }}"`) {
		t.Error("theme toggle container must localize its aria-label via t \"a11y.theme\"")
	}

	for _, v := range []string{"light", "system", "dark"} {
		if !strings.Contains(block, `data-theme-value="`+v+`"`) {
			t.Errorf("missing theme button for mode %q", v)
		}
	}
	if n := strings.Count(block, `class="theme-btn"`); n != 3 {
		t.Errorf("expected exactly 3 .theme-btn buttons, found %d", n)
	}
	if n := strings.Count(block, `type="button"`); n != 3 {
		t.Errorf("expected all 3 theme buttons to be type=\"button\", found %d", n)
	}
	if n := strings.Count(block, `aria-pressed="false"`); n != 3 {
		t.Errorf("expected all 3 theme buttons to carry an initial aria-pressed=\"false\", found %d", n)
	}
	if n := strings.Count(block, `aria-hidden="true"`); n != 3 {
		t.Errorf("expected the 3 decorative icons to carry aria-hidden=\"true\", found %d", n)
	}
	for _, key := range []string{`t "theme.light"`, `t "theme.system"`, `t "theme.dark"`} {
		if !strings.Contains(block, key) {
			t.Errorf("theme toggle must localize the button label/aria-label via %s", key)
		}
	}
}

// ---------------------------------------------------------------------
// 4. Single source of truth (§6.4)
// ---------------------------------------------------------------------

func TestTokensSingleSourceInInputCSS(t *testing.T) {
	css := readEmbeddedStatic(t, "static/css/input.css")

	if !strings.Contains(css, `[data-theme="light"]`) {
		t.Error(`input.css must define a :root[data-theme="light"] block`)
	}
	if !strings.Contains(css, `[data-theme="dark"]`) {
		t.Error(`input.css must define a :root[data-theme="dark"] block`)
	}

	required := []string{
		"--surface-page", "--surface-sidebar", "--surface-card", "--surface-elevated", "--surface-input",
		"--text-primary", "--text-secondary", "--text-muted",
		"--border-default", "--border-strong",
		"--brand-orange", "--brand-purple", "--interactive", "--focus-ring", "--link-text",
		"--status-success", "--status-warning", "--status-danger", "--status-info", "--status-offline",
		"--chart-grid", "--chart-label",
	}
	for _, tok := range required {
		if !strings.Contains(css, tok+":") {
			t.Errorf("input.css missing required semantic token %s", tok)
		}
	}

	// --chart-annotation-text (a per-theme token) was replaced by a pair of
	// THEME-INVARIANT ink candidates (Q3 audit): annotation marker
	// backgrounds are persisted per-event colours from the database, not
	// derived from the active theme, so the chart scripts pick whichever ink
	// contrasts better against the actual background rather than trusting a
	// single theme-driven ink that can go unreadable against an arbitrary
	// persisted hue. These two must be defined exactly once each (not
	// per-theme) — see the invariant-token check below.
	if strings.Contains(css, "--chart-annotation-text:") {
		t.Error("input.css must not define --chart-annotation-text anymore — replaced by --chart-annotation-ink-dark/-light")
	}
}

// TestChartAnnotationInkTokensAreThemeInvariant guards the Q3-audit fix for
// unreadable annotation labels in light theme: --chart-annotation-ink-dark/
// -light must be defined exactly ONCE each (outside any :root[data-theme=...]
// block), since the ink candidates themselves don't change with the theme —
// only which one the chart script picks (by contrast against the actual,
// theme-independent persisted marker colour) changes.
func TestChartAnnotationInkTokensAreThemeInvariant(t *testing.T) {
	css := readEmbeddedStatic(t, "static/css/input.css")
	for _, tok := range []string{"--chart-annotation-ink-dark", "--chart-annotation-ink-light"} {
		if n := strings.Count(css, tok+":"); n != 1 {
			t.Errorf("expected %s to be defined exactly once (theme-invariant), found %d definition(s)", tok, n)
		}
	}
}

// TestNoTokenDefinitionsInTemplates checks that the specific --ui-*/--log-*/
// --rw-* palette tokens that used to be defined inline (base.html, logs.html,
// overview.html) are gone from every template — definitions now live only in
// input.css; usages (var(--ui-watching) etc.) are untouched and expected.
//
// This intentionally checks the enumerated real token names rather than the
// broader `--(ui|log|rw)-[a-z0-9-]+\s*:` pattern: overview.html legitimately
// sets a per-element `--rw-accent` custom property (e.g.
// `style="--rw-accent:var(--rw-cpu)"`), which is not a palette token and is
// unrelated to this consolidation — the broad pattern would false-positive
// on it.
func TestNoTokenDefinitionsInTemplates(t *testing.T) {
	tokens := []string{
		// --ui-*
		"ui-watching", "ui-online", "ui-queue", "ui-offline", "ui-gain", "ui-warn",
		"ui-roi-pos", "ui-roi-neg", "ui-refund", "ui-watch", "ui-claim", "ui-raid",
		"ui-streak", "ui-prediction", "ui-other", "ui-brand-orange",
		// --log-*
		"log-bright-green", "log-lime", "log-red", "log-yellow", "log-gold", "log-orange",
		"log-amber", "log-turquoise", "log-cyan", "log-ultramarine", "log-violet",
		"log-magenta", "log-info-blue", "log-debug", "log-info", "log-warning", "log-error",
		// --rw-*
		"rw-cpu", "rw-mem", "rw-net", "rw-disk",
	}
	defRes := make([]*regexp.Regexp, len(tokens))
	for i, tok := range tokens {
		defRes[i] = regexp.MustCompile(`--` + tok + `\s*:`)
	}

	for _, name := range allTemplateNames(t) {
		content := readEmbeddedTemplate(t, name)
		for i, re := range defRes {
			if re.MatchString(content) {
				t.Errorf("%s still defines token --%s (definitions must live only in input.css)", name, tokens[i])
			}
		}
	}
}

// ---------------------------------------------------------------------
// 5. No external assets (§6.5)
// ---------------------------------------------------------------------

func TestNoExternalAssetReferences(t *testing.T) {
	scriptSrcRe := regexp.MustCompile(`<script[^>]*\ssrc\s*=\s*["']https?://`)
	linkHrefRe := regexp.MustCompile(`<link[^>]*\shref\s*=\s*["']https?://`)

	for _, name := range allTemplateNames(t) {
		content := readEmbeddedTemplate(t, name)
		if scriptSrcRe.MatchString(content) {
			t.Errorf("%s references an external <script src>", name)
		}
		if linkHrefRe.MatchString(content) {
			t.Errorf("%s references an external <link href>", name)
		}
	}

	css := readEmbeddedStatic(t, "static/css/input.css")
	if strings.Contains(css, "@import url(http") || strings.Contains(css, `@import "http`) {
		t.Error("input.css must not @import an external stylesheet")
	}
	if regexp.MustCompile(`url\(\s*["']?https?://`).MatchString(css) {
		t.Error("input.css must not reference external fonts/assets via url(http...)")
	}
}

// ---------------------------------------------------------------------
// 6. ApexCharts theme hook (§6.6)
// ---------------------------------------------------------------------

func TestApexChartsThemeHook(t *testing.T) {
	stats := readEmbeddedTemplate(t, "templates/statistics.html")
	if !strings.Contains(stats, "miner:themechange") {
		t.Error("statistics.html must listen for miner:themechange")
	}
	if !strings.Contains(stats, "updateOptions") {
		t.Error("statistics.html must call updateOptions on the existing chart instances")
	}
	for _, lit := range []string{"#232430", "#e8e6ef", "#15161c"} {
		if strings.Contains(stats, lit) {
			t.Errorf("statistics.html must not contain the old hardcoded literal %s", lit)
		}
	}

	streamer := readEmbeddedTemplate(t, "templates/streamer.html")
	if !strings.Contains(streamer, "miner:themechange") {
		t.Error("streamer.html must listen for miner:themechange")
	}
	if !strings.Contains(streamer, "updateOptions") {
		t.Error("streamer.html must call updateOptions on the existing chart instance")
	}
}

// ---------------------------------------------------------------------
// 7. Reduced motion (§6.7)
// ---------------------------------------------------------------------

func TestReducedMotionCoversThemeToggle(t *testing.T) {
	css := readEmbeddedStatic(t, "static/css/input.css")
	if !strings.Contains(css, "@media (prefers-reduced-motion: reduce)") {
		t.Fatal("input.css must keep a prefers-reduced-motion media block")
	}
	if !strings.Contains(css, ".theme-btn { transition: none; }") {
		t.Error("the reduced-motion block must disable .theme-btn transitions")
	}
}

// ---------------------------------------------------------------------
// 8. i18n keys (§6.8)
// ---------------------------------------------------------------------

func TestThemeI18nKeysTranslated(t *testing.T) {
	loc, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}
	for _, lang := range []string{i18n.LangEN, i18n.LangRU} {
		for _, key := range []string{"a11y.theme", "theme.dark", "theme.light", "theme.system"} {
			if got := loc.T(lang, key); got == key {
				t.Errorf("%s: key %q has no translation (T echoed the key back)", lang, key)
			}
		}
	}
}

// ---------------------------------------------------------------------
// 9. On-accent text AA fix (orchestrator audit)
// ---------------------------------------------------------------------

// TestOnAccentTokensFixSolidAccentButtons guards a regression the
// orchestrator's audit caught: swapping a literal `color: #fff` for
// `var(--text-primary)` is only correct on a low-opacity tint over a neutral
// surface (text-primary already contrasts correctly per theme there) — on a
// SOLID, saturated accent/success fill (e.g. .lang-btn.is-active's
// purple-600 background), --text-primary inverts to dark ink in light theme
// and fails AA against the mid-saturation fill. Those buttons need a fixed
// on-accent/on-success text token instead, defined once per theme but equal
// to white in both (dark keeps its original #fff look; light stays legible
// against the solid fill). This also guards against a raw Layer-A primitive
// (e.g. --prim-night-950) leaking into a component rule, which breaks the
// primitive/semantic layering the token system depends on.
func TestOnAccentTokensFixSolidAccentButtons(t *testing.T) {
	css := readEmbeddedStatic(t, "static/css/input.css")

	for _, tok := range []string{"--text-on-accent", "--text-on-success", "--text-on-danger"} {
		if n := strings.Count(css, tok+":"); n < 2 {
			t.Errorf("expected %s to be defined for both themes in input.css, found %d definition(s)", tok, n)
		}
	}

	for _, want := range []string{
		".lang-btn.is-active { background: var(--color-purple-600); color: var(--text-on-accent); }",
		`.theme-btn[aria-pressed="true"] { background: var(--color-purple-600); color: var(--text-on-accent); }`,
		".stat-range-btn.is-active { background: var(--color-purple-600); color: var(--text-on-accent); }",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("input.css missing expected AA-safe rule: %q", want)
		}
	}
	if !strings.Contains(css, "color: var(--text-on-accent)") {
		t.Error(".btn-bet must color its text via var(--text-on-accent)")
	}
	if !strings.Contains(css, "color: var(--text-on-success)") {
		t.Error(".btn-bet-go must color its text via var(--text-on-success), not a raw primitive")
	}
	if strings.Contains(css, "color: var(--prim-night-950)") || strings.Contains(css, "color: var(--prim-night-on-success)") {
		t.Error("component rules must not reference a Layer-A primitive directly — use the semantic on-accent/on-success token")
	}

	// .roi-period-btn moved from statistics.html's inline <style> into
	// input.css (F3 dedup — the page-local style block is now defined
	// alongside every other page's styles); the same AA-safe-token and
	// no-hardcoded-tint invariants apply at its new location.
	if strings.Contains(css, "color:#fff") || strings.Contains(css, "color: #fff") {
		t.Error("input.css must not hardcode color:#fff on .roi-period-btn.is-active — use var(--text-on-accent)")
	}
	if !strings.Contains(css, ".roi-period-btn.is-active { background: var(--color-purple-600); color: var(--text-on-accent); }") {
		t.Error("input.css .roi-period-btn.is-active must use var(--text-on-accent)")
	}
	if strings.Contains(css, "#8b7fd11f") {
		t.Error("input.css .roi-period-btn:hover must not hardcode the #8b7fd11f tint — use color-mix over var(--interactive)")
	}

	overviewLive := readEmbeddedTemplate(t, "templates/partials/overview_live.html")
	if strings.Contains(overviewLive, "rgba(127,168,140") {
		t.Error("overview_live.html must not hardcode the rgba(127,168,140,...) success tint — use color-mix over var(--color-success)")
	}
}

// ---------------------------------------------------------------------
// 10. Cascade order (orchestrator audit — critical layout regression)
// ---------------------------------------------------------------------

// TestCascadeOrderMatchesOriginalDocument pins the source order that F1's
// consolidation into a single input.css must reproduce. Before F1, three
// separate <style> sources cascaded in document order — input.css (via
// <link>, in <head>) first, then base.html's own inline <style> (also in
// <head>, right after the <link>), then a page template's own inline
// <style> (in <body>, rendered last) — and for elements carrying more than
// one independently-styled class (equal specificity), the LAST-loaded rule
// won for any property both rules set.
//
// Consolidating everything into one file collapses those three sources into
// one cascade, so the SOURCE ORDER inside input.css must replicate
// (original-input.css content) -> (base.html-derived rules) -> (page-
// template-derived rules) for these cross-source conflicts to resolve the
// same way. Getting this backwards is exactly how a real regression slipped
// through once already: `<button class="sidebar-toggle qa-btn">` has
// `.qa-btn{display:inline-flex}` (originally input.css) and
// `.sidebar-toggle{display:none}` (originally base.html) — with the base.html
// rule loading second and winning before F1. If .qa-btn ends up placed AFTER
// the base.html-derived block in the consolidated file, it wins instead,
// the toggle becomes visible on desktop, and — as the first child of the
// `.app-shell` grid — breaks the whole sidebar/main layout.
func TestCascadeOrderMatchesOriginalDocument(t *testing.T) {
	css := readEmbeddedStatic(t, "static/css/input.css")

	qaBtnIdx := strings.Index(css, ".qa-btn {")
	toggleHideIdx := strings.Index(css, ".sidebar-toggle { display: none; }")
	if qaBtnIdx < 0 || toggleHideIdx < 0 {
		t.Fatalf("expected both .qa-btn and .sidebar-toggle's default-hide rule in input.css (qa-btn=%d toggle=%d)", qaBtnIdx, toggleHideIdx)
	}
	if qaBtnIdx > toggleHideIdx {
		t.Fatal(".qa-btn (originally input.css) must come BEFORE .sidebar-toggle's display:none (originally base.html) " +
			"— otherwise .qa-btn's display:inline-flex wins the tie and the mobile-only toggle button becomes visible on desktop, " +
			"breaking the .app-shell grid layout")
	}

	// The <=900px breakpoint that reserves top padding for the fixed
	// hamburger (originally base.html, "3.25rem 1rem 1rem") must win over
	// the plain mobile padding (originally input.css's own Overview-redesign
	// media block, "1rem") — so its rule must come LAST.
	paddingOneRemIdx := strings.Index(css, ".app-main { padding: 1rem; }")
	paddingHamburgerIdx := strings.Index(css, ".app-main { padding: 3.25rem 1rem 1rem; }")
	if paddingOneRemIdx < 0 || paddingHamburgerIdx < 0 {
		t.Fatalf("expected both .app-main mobile-padding rules in input.css (plain=%d hamburger-reserving=%d)", paddingOneRemIdx, paddingHamburgerIdx)
	}
	if paddingOneRemIdx > paddingHamburgerIdx {
		t.Fatal(".app-main { padding: 1rem } (originally input.css) must come BEFORE .app-main { padding: 3.25rem 1rem 1rem } " +
			"(originally base.html, reserving room for the fixed hamburger) — otherwise mobile content overlaps the toggle button")
	}

	// Both conflicts sit inside their own `@media (max-width: 900px)` block;
	// confirm each padding rule is actually reachable from its own nearby
	// breakpoint (not just present somewhere unguarded in the file).
	for _, idx := range []int{paddingOneRemIdx, paddingHamburgerIdx} {
		window := css[max(0, idx-400):idx]
		if !strings.Contains(window, "@media (max-width: 900px)") {
			t.Errorf("expected an @media (max-width: 900px) guard shortly before offset %d", idx)
		}
	}
}

// ---------------------------------------------------------------------
// 11. Annotation-label readability fix (Q3 independent review)
// ---------------------------------------------------------------------

// TestAnnotationLabelInkPicksByContrast guards the MAJOR-1 fix: annotation
// marker backgrounds are persisted per-event colours from the database
// (analytics.RecordAnnotation / miner.go), completely independent of the
// active theme, so a single theme-driven ink token (--chart-annotation-text,
// which used to resolve to white in light theme) went unreadable against
// several of those persisted hues (contrast as low as 1.31:1). Both chart
// scripts must instead pick whichever of the two theme-invariant ink
// candidates gives better WCAG contrast against the actual background at
// render time, and must no longer reference the removed token.
func TestAnnotationLabelInkPicksByContrast(t *testing.T) {
	for _, tmplName := range []string{"templates/statistics.html", "templates/streamer.html"} {
		content := readEmbeddedTemplate(t, tmplName)
		for _, want := range []string{
			"relativeLuminance", "contrastRatio", "readableInkFor",
			"--chart-annotation-ink-dark", "--chart-annotation-ink-light",
		} {
			if !strings.Contains(content, want) {
				t.Errorf("%s missing luminance-based ink selection: %q", tmplName, want)
			}
		}
		if strings.Contains(content, "--chart-annotation-text") {
			t.Errorf("%s must not reference the removed --chart-annotation-text token anymore", tmplName)
		}
	}
}

// TestAnnotationMappingKeyedByType guards the MAJOR-2 fix: a.reason is the
// always-non-empty, human-readable free text persisted alongside every
// annotation (e.g. "+450 - Watch Streak"), so `a.reason || a.type` never
// actually reaches a.type — the reason-keyed lookup was dead code that
// always fell through to the persisted per-event colour. The background
// lookup must key on the machine-readable a.type instead, via a dedicated
// type -> colour map covering every type analytics.RecordAnnotation /
// miner.go can emit (WATCH_STREAK, RAID, PREDICTION_MADE, WIN, LOSE).
func TestAnnotationMappingKeyedByType(t *testing.T) {
	stats := readEmbeddedTemplate(t, "templates/statistics.html")
	if strings.Contains(stats, "a.reason || a.type") {
		t.Error("statistics.html must not key the annotation colour lookup by 'a.reason || a.type' " +
			"— a.reason is always non-empty free text and shadows a.type, making the mapping dead code")
	}
	if !strings.Contains(stats, "annotationTypePalette") {
		t.Error("statistics.html must define an annotation-type -> colour map (annotationTypePalette)")
	}
	for _, want := range []string{"WATCH_STREAK", "RAID", "PREDICTION_MADE", "WIN", "LOSE"} {
		if !strings.Contains(stats, want) {
			t.Errorf("statistics.html missing annotation-type mapping entry %q", want)
		}
	}
	if !strings.Contains(stats, "a.type && ANN[a.type]") {
		t.Error("statistics.html must key the annotation background lookup by a.type (via the ANN type-map), not a.reason")
	}
}

// ---------------------------------------------------------------------
// 12. meta theme-color first-load sync (Q3 independent review, MINOR 3)
// ---------------------------------------------------------------------

// TestThemeControllerSyncsMetaOnInit guards the MINOR-3 fix: the bootstrap
// script's own meta-theme-color update is a no-op on first load, because it
// runs before its <meta id="meta-theme-color"> tag exists in the DOM
// (getElementById returns null at that point in parsing). The controller
// script, which runs later (once the element exists), must call apply(...)
// once at init — not just sync the toggle buttons — so the meta tag
// actually reflects the resolved theme before the user's first interaction.
func TestThemeControllerSyncsMetaOnInit(t *testing.T) {
	base := readEmbeddedTemplate(t, "templates/base.html")

	firstScript := strings.Index(base, "<script")
	bootstrapEnd := strings.Index(base[firstScript:], "</script>") + firstScript
	bootstrap := base[firstScript:bootstrapEnd]
	if !strings.Contains(bootstrap, "__MINER_THEME_META") {
		t.Error("bootstrap script must define window.__MINER_THEME_META (shared dark/light meta-color map)")
	}

	ctrl := themeControllerScript(t, base)
	if !strings.Contains(ctrl, "__MINER_THEME_META") {
		t.Error("controller must read window.__MINER_THEME_META (shared with the bootstrap script, no literal fallback)")
	}
	if !strings.Contains(ctrl, "apply(initialMode, resolve(initialMode))") {
		t.Error("controller must sync meta theme-color (and re-affirm data-theme attributes) at init via apply(initialMode, resolve(initialMode))")
	}
}
