package web

import (
	"strings"
	"testing"
)

// This file pins the v0.13.7 sidebar hotfix (§6): the orange two-line brand
// wordmark beside the unchanged alien logo, and the relocation of the mobile
// drawer toggle out of the main content area (it no longer sits above "Overview")
// into a fixed, accessible, mobile-only control with no dead JS.

func readBase(t *testing.T) string {
	t.Helper()
	b, err := templatesFS.ReadFile("templates/base.html")
	if err != nil {
		t.Fatalf("read base.html: %v", err)
	}
	return string(b)
}

// §8.19: the existing alien asset is still the sidebar logo (not replaced).
func TestSidebarKeepsExistingAlienAsset(t *testing.T) {
	if !strings.Contains(readBase(t), "/static/images/sidebar-logo-full.png") {
		t.Error("sidebar must keep the existing alien logo asset sidebar-logo-full.png")
	}
}

// §8.20: the exact orange brand text is present, on two lines.
func TestSidebarBrandText(t *testing.T) {
	base := readBase(t)
	for _, want := range []string{
		`<span class="sidebar-brand-title">Twitch Drops Miner</span>`,
		`<span class="sidebar-brand-sub">Channel Points</span>`,
	} {
		if !strings.Contains(base, want) {
			t.Errorf("sidebar brand missing %q", want)
		}
	}
}

// §8.21 + F1 semantic rewrite: the brand uses its own dedicated orange
// semantic token (not a raid/error hue), and that token — like every other
// palette definition — now lives solely in input.css (F1 theme-token
// consolidation); base.html carries no inline palette anymore, only usages
// via var(--...) in its markup (none, in this case — the color lives in the
// CSS rule, not inline on the element).
func TestSidebarBrandUsesOrangeToken(t *testing.T) {
	base := readBase(t)
	css := readEmbeddedStatic(t, "static/css/input.css")

	// The token is defined exactly once per theme (dark block + light block)
	// in input.css, aliased from the shared brand-orange semantic (itself
	// backed by a dedicated dark/light primitive pair) — never as a raw,
	// free-floating hex duplicated across themes.
	if n := strings.Count(css, "--ui-brand-orange:"); n < 2 {
		t.Errorf("expected --ui-brand-orange to be defined for both themes in input.css, found %d definition(s)", n)
	}
	if !strings.Contains(css, "#FF7A00") {
		t.Error("input.css must keep the dark brand-orange primitive (#FF7A00)")
	}
	if !strings.Contains(css, "#a03c00") {
		t.Error("input.css must define an AA-validated light brand-orange primitive (#a03c00)")
	}

	// The rule that actually paints the wordmark also lives in input.css now.
	if !strings.Contains(css, ".sidebar-brand-title { color: var(--ui-brand-orange)") {
		t.Error("brand title must be colored by the --ui-brand-orange token (rule expected in input.css)")
	}
	if strings.Contains(css, ".sidebar-brand-title { color: var(--ui-raid)") ||
		strings.Contains(css, ".sidebar-brand-title { color: var(--ui-roi-neg)") {
		t.Error("brand must use its own orange token, not a raid/error color")
	}

	// base.html must no longer define the palette inline — only input.css
	// (single source of truth per F1).
	if strings.Contains(base, "--ui-brand-orange:") {
		t.Error("base.html must not define --ui-brand-orange inline anymore — the token now lives only in input.css")
	}
	if strings.Contains(base, ".sidebar-brand-title") {
		t.Error("base.html must not carry the .sidebar-brand-title rule anymore — it now lives only in input.css")
	}
}

// §8.22: the obsolete plain "Points Miner" sidebar label is gone.
func TestSidebarDropsOldLabel(t *testing.T) {
	if strings.Contains(readBase(t), "<span>Points Miner</span>") {
		t.Error("sidebar still contains the old <span>Points Miner</span> label")
	}
}

// §8.23 + §8.8: the mobile toggle is not in the main content area above Overview.
// It now lives in the app-shell before <main>, so on desktop the content starts
// directly under the top row with no leftover gap.
func TestSidebarToggleNotAboveOverview(t *testing.T) {
	base := readBase(t)
	toggleIdx := strings.Index(base, `id="sidebar-toggle"`)
	mainIdx := strings.Index(base, `<main class="app-main">`)
	if toggleIdx < 0 || mainIdx < 0 {
		t.Fatalf("expected both the toggle and <main> in base.html (toggle=%d main=%d)", toggleIdx, mainIdx)
	}
	if toggleIdx > mainIdx {
		t.Error("the mobile toggle must not sit inside the main content area above Overview")
	}
	if strings.Contains(base, `class="md:hidden qa-btn"`) {
		t.Error("the old md:hidden toggle button must be removed from the main content row")
	}
}

// §8.24 + §6.1 + F1 semantic rewrite: the relocated toggle keeps the
// responsive sidebar reachable — a real, accessible control gated on the
// 900px drawer breakpoint, hidden (no gap) on desktop. The a11y attributes
// stay on the element in base.html; the display/breakpoint rules moved to
// input.css with the rest of base.html's former inline palette/component
// CSS (F1 theme-token consolidation) — substance (hidden on desktop, visible
// <=900px, one toggle) is preserved, just relocated.
func TestSidebarToggleResponsiveAndAccessible(t *testing.T) {
	base := readBase(t)
	if !strings.Contains(base, `aria-controls="app-sidebar"`) || !strings.Contains(base, `aria-expanded="false"`) {
		t.Error("the toggle must reference the sidebar via aria-controls and carry an initial aria-expanded")
	}

	css := readEmbeddedStatic(t, "static/css/input.css")
	if !strings.Contains(css, ".sidebar-toggle { display: none; }") {
		t.Error("input.css must hide the toggle by default (desktop/tablet), occupying no space")
	}
	if !strings.Contains(css, "@media (max-width: 900px)") {
		t.Error("input.css must gate the toggle's visibility on the <=900px drawer breakpoint")
	}
	// The 900px block must be the one that reveals the toggle (not just any
	// max-width:900px block elsewhere in the stylesheet).
	idx := strings.Index(css, ".sidebar-toggle { display: none; }")
	if idx >= 0 {
		afterToggle := css[idx:]
		mq := strings.Index(afterToggle, "@media (max-width: 900px)")
		if mq < 0 {
			t.Error("the <=900px breakpoint following .sidebar-toggle's default hide must set display: inline-flex")
		} else {
			end := mq + 400
			if end > len(afterToggle) {
				end = len(afterToggle)
			}
			if !strings.Contains(afterToggle[mq:end], "display: inline-flex") {
				t.Error("the <=900px breakpoint following .sidebar-toggle's default hide must set display: inline-flex")
			}
		}
	}

	if strings.Contains(base, ".sidebar-toggle { display: none; }") {
		t.Error("base.html must not define the toggle's display rules inline anymore — they now live only in input.css")
	}
}

// §8.25 + §6.1: exactly one toggle, wired to functional (not dead) drawer JS.
func TestSidebarToggleJSIsLive(t *testing.T) {
	base := readBase(t)
	if n := strings.Count(base, `id="sidebar-toggle"`); n != 1 {
		t.Errorf("expected exactly one sidebar toggle, found %d", n)
	}
	for _, want := range []string{
		"getElementById('sidebar-toggle')",
		"is-open",
		"aria-expanded",
		"Escape",
	} {
		if !strings.Contains(base, want) {
			t.Errorf("toggle JS missing %q — the control must be functional, not dead", want)
		}
	}
}
