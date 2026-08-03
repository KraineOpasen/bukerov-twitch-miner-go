package web

// S5-2 component library tests (task §K.8/K.9/K.13): the six new
// templates/components/*.html partials (C0/C1/C10/C11/C17) parse and render
// in both languages, C1's role="alert" is exclusive to the FAIL variant,
// S-NOBACK renders nothing at all, and the new locale keys this slice adds
// are present, non-empty and actually translated (not copy-pasted) in both
// languages.

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/i18n"
)

// execComponent executes a named component template from the standalone
// partial set (which now embeds templates/components/*.html alongside
// templates/partials/*.html — see loadTemplates in server.go) in the given
// language and returns the rendered fragment.
func execComponent(t *testing.T, lang, name string, data interface{}) string {
	t.Helper()
	tmpl := testPartialsLang(t, lang)
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		t.Fatalf("execute %s (lang=%s): %v", name, lang, err)
	}
	return buf.String()
}

// TestS5_2ComponentsParseAndRenderBothLanguages proves every one of the six
// C0/C1/C10/C11/C17 component definitions parses (loadTemplates already ran
// via testPartialsLang) and renders real output in both RU and EN.
func TestS5_2ComponentsParseAndRenderBothLanguages(t *testing.T) {
	for _, lang := range []string{i18n.LangEN, i18n.LangRU} {
		c0 := execComponent(t, lang, "c0.provenance_chip", ProvenanceChipData{AgeLabel: "12s", Source: "SSE"})
		if !strings.Contains(c0, "c0-chip") {
			t.Errorf("[%s] c0.provenance_chip did not render", lang)
		}

		c1 := execComponent(t, lang, "c1.state_block", StateBlockData{State: "FAIL", Variant: "block", Message: "boom"})
		if !strings.Contains(c1, "c1-block") || !strings.Contains(c1, "boom") {
			t.Errorf("[%s] c1.state_block did not render", lang)
		}

		c10 := execComponent(t, lang, "c10.badge", BadgeData{Tier: "ok", Icon: "✓", Label: "Live"})
		if !strings.Contains(c10, "c10-badge") || !strings.Contains(c10, "Live") {
			t.Errorf("[%s] c10.badge did not render", lang)
		}

		c11 := execComponent(t, lang, "c11.progress", ProgressData{Mode: "determinate", Percent: 42, Label: "42/100"})
		if !strings.Contains(c11, "42%") {
			t.Errorf("[%s] c11.progress (determinate) did not render the percent", lang)
		}
		c11u := execComponent(t, lang, "c11.progress", ProgressData{Mode: "unknown"})
		if strings.Contains(c11u, "0%") {
			t.Errorf("[%s] c11.progress (unknown) must never render as 0%%, got %q", lang, c11u)
		}

		c17toast := execComponent(t, lang, "c17.toast_stack", nil)
		if !strings.Contains(c17toast, `id="toast-container"`) || !strings.Contains(c17toast, `role="status"`) || !strings.Contains(c17toast, `aria-live="polite"`) {
			t.Errorf("[%s] c17.toast_stack missing the polite live-region markup", lang)
		}

		c17lc := execComponent(t, lang, "c17.lifecycle_alert", nil)
		for _, want := range []string{`id="health-banner"`, `id="lifecycle-auth-banner"`, `role="alert"`} {
			if !strings.Contains(c17lc, want) {
				t.Errorf("[%s] c17.lifecycle_alert missing %q", lang, want)
			}
		}
	}
}

// TestS5_2C1OnlyFailCarriesRoleAlert pins Stage 4 §7's rule: of the nine C1
// states, only S-FAIL is role="alert" — every other state is plain content.
func TestS5_2C1OnlyFailCarriesRoleAlert(t *testing.T) {
	plain := []string{"EMPTY", "PART", "STALE", "UNK", "DEGR", "BLOCK", "DENY", "DEFER"}
	for _, state := range plain {
		out := execComponent(t, i18n.LangEN, "c1.state_block", StateBlockData{State: state, Variant: "block", Message: "x"})
		if strings.Contains(out, `role="alert"`) {
			t.Errorf("C1 state %s must not carry role=alert (only FAIL does)", state)
		}
		if strings.TrimSpace(out) == "" {
			t.Errorf("C1 state %s must render content (only NOBACK/empty renders nothing)", state)
		}
	}

	fail := execComponent(t, i18n.LangEN, "c1.state_block", StateBlockData{State: "FAIL", Variant: "block", Message: "x"})
	if !strings.Contains(fail, `role="alert"`) {
		t.Error("C1 FAIL state must carry role=alert")
	}
}

// TestS5_2C1NoBackRendersNothing pins S-NOBACK: absence of the control, not
// a greyed-out placeholder — an empty or "NOBACK" State must render zero
// output.
func TestS5_2C1NoBackRendersNothing(t *testing.T) {
	for _, state := range []string{"", "NOBACK"} {
		out := execComponent(t, i18n.LangEN, "c1.state_block", StateBlockData{State: state, Message: "should never appear", ActionLabel: "should never appear either"})
		if strings.TrimSpace(out) != "" {
			t.Errorf("S-NOBACK (State=%q) must render nothing, got %q", state, out)
		}
	}
}

// s5_2NumericPercentRe matches any bare digit-percent token (e.g. "0%",
// "42%") — used to prove the unknown C11 mode never renders a numeric
// percent under any label text.
var s5_2NumericPercentRe = regexp.MustCompile(`\d+%`)

// TestS5_2C11ProgressModeSpecificStructure (test-gap) proves the C11
// progress component renders genuinely mode-specific structure rather than
// merely containing a percent substring somewhere: determinate output
// carries the track/fill (at the exact width) and percent markup, while
// unknown output carries none of that — no track, no fill, no percent node,
// no bare numeric percent anywhere — only the dash and the localized
// unknown label.
func TestS5_2C11ProgressModeSpecificStructure(t *testing.T) {
	det := execComponent(t, i18n.LangEN, "c11.progress", ProgressData{Mode: "determinate", Percent: 42, Label: "42/100"})
	for _, want := range []string{
		"c11-progress--determinate",
		"c11-progress-track",
		"c11-progress-fill",
		`style="width:42%"`,
		"c11-progress-percent",
		"42%",
	} {
		if !strings.Contains(det, want) {
			t.Errorf("determinate c11.progress missing %q, got %q", want, det)
		}
	}

	unk := execComponent(t, i18n.LangEN, "c11.progress", ProgressData{Mode: "unknown"})
	if !strings.Contains(unk, "c11-progress--unknown") {
		t.Errorf("unknown c11.progress missing c11-progress--unknown, got %q", unk)
	}
	if !strings.Contains(unk, "c11-progress-dash") {
		t.Errorf("unknown c11.progress missing c11-progress-dash, got %q", unk)
	}

	loc, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}
	unknownLabel := loc.T(i18n.LangEN, "c11.unknown")
	if unknownLabel == "" || unknownLabel == "c11.unknown" {
		t.Fatalf("c11.unknown localized label is missing or unresolved: %q", unknownLabel)
	}
	if !strings.Contains(unk, unknownLabel) {
		t.Errorf("unknown c11.progress missing the localized unknown label %q, got %q", unknownLabel, unk)
	}

	for _, banned := range []string{"c11-progress-track", "c11-progress-fill", "c11-progress-percent"} {
		if strings.Contains(unk, banned) {
			t.Errorf("unknown c11.progress must not render %q, got %q", banned, unk)
		}
	}
	if s5_2NumericPercentRe.MatchString(unk) {
		t.Errorf("unknown c11.progress must not render any numeric percent, got %q", unk)
	}
}

// TestS5_2LocaleKeysPresentAndTranslated guards the full set of new keys this
// slice adds (task §J): each must resolve to a non-empty, actually-different
// string in both RU and EN (never an echoed key, never accidental copy-paste
// identical text). The generic parity/no-empty-value gates already run via
// go test ./internal/i18n/...; this pins the SPECIFIC S5-2 key set.
func TestS5_2LocaleKeysPresentAndTranslated(t *testing.T) {
	loc, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}
	keys := []string{
		"nav.analytics", "nav.events", "nav.system", "nav.help",
		"a11y.skip_to_content", "a11y.close_menu",
		"c0.unknown", "c0.session", "c11.unknown", "c11.syncing",
		"help.title", "help.lead", "help.pending",
		"events.title", "events.enabled_text", "events.link_notifications",
		"events.disabled_text", "events.link_settings", "events.pending_note",
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
		if strings.TrimSpace(en) == "" || strings.TrimSpace(ru) == "" {
			t.Errorf("%q has an empty value in one language (en=%q ru=%q)", k, en, ru)
		}
		if en == ru {
			t.Errorf("%q has identical EN/RU text %q — looks untranslated", k, en)
		}
	}
}
