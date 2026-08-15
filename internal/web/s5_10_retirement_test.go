package web

// Task S5-10: Retirement & audit — the final Stage 5 slice per
// docs/dashboard/stage-4-visual-design-system.md §15/§16/§18. This file pins
// the slice's deterministic completion criteria: every legacy neutral-/
// purple-/amber-/green-/emerald-/red- scale alias is gone from templates
// (grep-verified zero references, §15), no primitive token ever reaches a
// template (§18 item 7), the now-dead aliases don't silently return to
// input.css, dashboard.html is retired, and all 30 canonical routes still
// render.
//
// s5_10LegacyAliasRe deliberately matches on the bare color-tier name with
// no property-prefix assumption (unlike earlier slices' narrower `(?:bg|
// text|border|...)-` patterns) — this repo's own S5-10 census caught real
// legacy references those prefix lists missed entirely (`accent-purple-600`
// on native checkbox inputs, `border-t-purple-500` on spinner elements).
// A prefix-anchored regex would have silently let both classes back in.

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// s5_10LegacyAliasRe matches any Tailwind utility built on one of the 25
// names input.css's pre-S5-10 `--scale-*`/`--color-*` indirection layer
// aliased to a semantic token, regardless of which property prefix or
// variant (hover:, group-hover:, border-t-, accent-, ...) precedes it. This
// is deliberately the EXACT 25-name vocabulary, not every neutral/purple/
// amber/green/emerald/red-NNN combination: several tiers (e.g. red-400)
// were always plain, un-aliased Tailwind builtin colors this project never
// tokenized, explicitly left alone by S5-10 — a broader numeric-range
// pattern flags those as false positives.
var s5_10LegacyAliasRe = regexp.MustCompile(`\b(?:neutral-(?:100|200|300|400|500|600|700|800|900|950)|purple-(?:300|400|500|600|700)|amber-(?:500|600|700)|green-(?:500|600)|emerald-(?:500|600)|red-(?:500|600|700))\b`)

// s5_10TemplateCommentRe strips Go html/template comments ({{/* ... */}}),
// which never reach rendered output, before the legacy-alias/primitive
// sweep — otherwise prose that mentions a legacy class name by name (to
// explain why some other class ISN'T it) reads as a false positive.
var s5_10TemplateCommentRe = regexp.MustCompile(`(?s)\{\{/\*.*?\*/\}\}`)

// s5_10PrimitiveRe matches a direct primitive reference. Templates may only
// ever name semantic roles; primitives are Layer A, confined to input.css.
var s5_10PrimitiveRe = regexp.MustCompile(`--prim-[a-z0-9-]+`)

// s5_10AllTemplateFiles enumerates every embedded template — pages,
// partials, and components — not just the 39 this slice touched, so this
// stays a live guard against any future template reintroducing a legacy
// alias or a primitive, not a one-time snapshot of S5-10's own diff.
func s5_10AllTemplateFiles(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, glob := range []string{"templates/*.html", "templates/partials/*.html", "templates/components/*.html"} {
		matches, err := fs.Glob(templatesFS, glob)
		if err != nil {
			t.Fatalf("glob %s: %v", glob, err)
		}
		names = append(names, matches...)
	}
	if len(names) == 0 {
		t.Fatal("s5_10AllTemplateFiles found zero templates — embed glob broken?")
	}
	return names
}

// TestS5_10ZeroLegacyAliasReferencesInTemplates pins §15/§18: migrated and
// new templates take semantic utilities exclusively. Every legacy alias
// reference found by the S5-10 census (575+ across 39 files, several only
// visible once the property-prefix assumption was dropped — see
// s5_10LegacyAliasRe's doc comment) must be gone — literal zero, no
// exceptions. The one holdout an earlier pass left (streamer.html's
// bg-neutral-700 search-clear button) is migrated to the
// --surface-control-muted semantic token (S5-10 corrective pass); there is
// no longer a whitelist for this test to consult.
func TestS5_10ZeroLegacyAliasReferencesInTemplates(t *testing.T) {
	for _, name := range s5_10AllTemplateFiles(t) {
		src := s5_10TemplateCommentRe.ReplaceAllString(readEmbeddedTemplate(t, name), "")
		matches := s5_10LegacyAliasRe.FindAllString(src, -1)
		for _, m := range matches {
			t.Errorf("%s: legacy alias reference %q survives S5-10 retirement (want a semantic token — zero exceptions remain)", name, m)
		}
	}
}

// TestS5_10ZeroPrimitiveReferencesInTemplates pins §18 item 7: primitives
// never appear in templates. Only input.css's Layer A may reference them.
func TestS5_10ZeroPrimitiveReferencesInTemplates(t *testing.T) {
	for _, name := range s5_10AllTemplateFiles(t) {
		src := readEmbeddedTemplate(t, name)
		if m := s5_10PrimitiveRe.FindAllString(src, -1); len(m) > 0 {
			t.Errorf("%s: references primitive token(s) %v — templates take semantic roles only (§18)", name, m)
		}
	}
}

// s5_10DeletedAliases are the legacy custom-property names S5-10 removed
// from input.css once their last template reference was migrated. Every one
// of these definitions reappearing (in either the Tailwind scale-indirection
// layer or the @theme inline exposure layer) means an alias was silently
// reintroduced rather than a consumer being migrated to its semantic token —
// exactly the drift §15's "grep-verified zero references" gate exists to
// catch.
var s5_10DeletedAliases = []string{
	"--scale-neutral-950", "--scale-neutral-900", "--scale-neutral-800",
	"--scale-neutral-700", "--scale-neutral-600", "--scale-neutral-500",
	"--scale-neutral-400", "--scale-neutral-300", "--scale-neutral-200",
	"--scale-neutral-100",
	"--scale-purple-700", "--scale-purple-600", "--scale-purple-500",
	"--scale-purple-400", "--scale-purple-300",
	"--scale-amber-700", "--scale-amber-600", "--scale-amber-500",
	"--scale-green-600", "--scale-green-500",
	"--scale-emerald-600", "--scale-emerald-500",
	"--scale-red-700", "--scale-red-600", "--scale-red-500",
	"--color-neutral-950", "--color-neutral-900", "--color-neutral-800",
	"--color-neutral-700", "--color-neutral-600", "--color-neutral-500",
	"--color-neutral-400", "--color-neutral-300", "--color-neutral-200",
	"--color-neutral-100",
	"--color-purple-700", "--color-purple-600", "--color-purple-500",
	"--color-purple-400", "--color-purple-300",
	"--color-amber-700", "--color-amber-600", "--color-amber-500",
	"--color-green-600", "--color-green-500",
	"--color-emerald-600", "--color-emerald-500",
	"--color-red-700", "--color-red-600", "--color-red-500",
}

// TestS5_10DeletedLegacyAliasesDoNotReturn guards input.css itself: none of
// the 50 alias definitions S5-10 deleted may be redefined — including
// neutral-700 (both the --scale-neutral-700/--color-neutral-700 pair and
// the internal input.css @utility rules that used to consume the bare
// Tailwind neutral-700 class), the final holdout retired by the S5-10
// corrective pass. There is no longer a deliberately-kept exception: every
// legacy alias this slice ever defined is gone.
func TestS5_10DeletedLegacyAliasesDoNotReturn(t *testing.T) {
	css, err := staticFS.ReadFile("static/css/input.css")
	if err != nil {
		t.Fatalf("read input.css: %v", err)
	}
	src := string(css)
	for _, name := range s5_10DeletedAliases {
		if strings.Contains(src, name+":") {
			t.Errorf("input.css: deleted legacy alias %q was reintroduced", name)
		}
	}
}

// TestS5_10DashboardRetired pins §16: dashboard.html — dead since the S5-2
// chrome redirect landed (see f4c_pages_test.go's "dead legacy dashboard.html"
// note) — is deleted, not just unrouted.
func TestS5_10DashboardRetired(t *testing.T) {
	if _, err := templatesFS.ReadFile("templates/dashboard.html"); err == nil {
		t.Error("templates/dashboard.html still embedded — S5-10 must delete the dead legacy page")
	}
}

// s5_10CanonicalRoutes are the 30 routes docs/dashboard/stage-4-visual-
// design-system.md §11 defines as the frozen 7-section/30-route IA. S5-10's
// own acceptance checklist (§18 item 1) requires all 30 intact — none added,
// removed, or renamed — through the retirement pass.
var s5_10CanonicalRoutes = []string{
	"/overview", "/overview/queue",
	"/drops/current", "/drops/upcoming", "/drops/claims", "/drops/past",
	"/analytics/points", "/analytics/roi",
	"/events", "/events/browser", "/events/sound", "/events/discord",
	"/settings/streamers", "/settings/rotation", "/settings/drops",
	"/settings/predictions", "/settings/chat-raids", "/settings/transport",
	"/settings/analytics-logging", "/settings/events-notifications",
	"/settings/discord", "/settings/system",
	"/system/status", "/system/diagnostics", "/system/logs",
	"/help/getting-started", "/help/glossary", "/help/troubleshooting",
	"/help/notifications-audio", "/help/diagnostics-support",
}

// TestS5_10AllCanonicalRoutesIntact pins §18 item 1: all 30 routes render
// (f3GetPage fatals on a non-200), in both languages.
func TestS5_10AllCanonicalRoutesIntact(t *testing.T) {
	if len(s5_10CanonicalRoutes) != 30 {
		t.Fatalf("s5_10CanonicalRoutes has %d entries, want exactly 30", len(s5_10CanonicalRoutes))
	}
	srv := buildF3PageServer(t)
	for _, lang := range []string{"en", "ru"} {
		for _, route := range s5_10CanonicalRoutes {
			f3GetPage(t, srv, route, lang)
		}
	}
}
