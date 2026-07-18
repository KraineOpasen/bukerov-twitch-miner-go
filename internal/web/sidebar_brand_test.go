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

// §8.21: the brand uses the dedicated orange semantic token, not a raid/error hue.
func TestSidebarBrandUsesOrangeToken(t *testing.T) {
	base := readBase(t)
	if !strings.Contains(base, "--ui-brand-orange") {
		t.Error("expected a dedicated --ui-brand-orange semantic token")
	}
	if !strings.Contains(base, ".sidebar-brand-title { color: var(--ui-brand-orange)") {
		t.Error("brand title must be colored by the --ui-brand-orange token")
	}
	if strings.Contains(base, ".sidebar-brand-title { color: var(--ui-raid)") ||
		strings.Contains(base, ".sidebar-brand-title { color: var(--ui-roi-neg)") {
		t.Error("brand must use its own orange token, not a raid/error color")
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

// §8.24 + §6.1: the relocated toggle keeps the responsive sidebar reachable — a
// real, accessible control gated on the 900px drawer breakpoint, hidden (no gap)
// on desktop.
func TestSidebarToggleResponsiveAndAccessible(t *testing.T) {
	base := readBase(t)
	if !strings.Contains(base, `aria-controls="app-sidebar"`) || !strings.Contains(base, `aria-expanded="false"`) {
		t.Error("the toggle must reference the sidebar via aria-controls and carry an initial aria-expanded")
	}
	if !strings.Contains(base, ".sidebar-toggle { display: none; }") {
		t.Error("the toggle must be hidden by default (desktop/tablet), occupying no space")
	}
	if !strings.Contains(base, "@media (max-width: 900px)") || !strings.Contains(base, "display: inline-flex") {
		t.Error("the toggle must become visible at the <=900px drawer breakpoint")
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
