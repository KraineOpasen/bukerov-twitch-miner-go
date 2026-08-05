package web

// S5-3 Phase 9/10 (item 22) i18n completeness: every new key this slice adds
// resolves to a non-empty, actually-translated string in both languages
// (never an echoed key, never accidental copy-paste identical text), mirrors
// the existing TestS5_2LocaleKeysPresentAndTranslated pattern for this
// slice's own key set. The generic parity/no-empty-value gates already run
// via `go test ./internal/i18n/...`; this pins the SPECIFIC S5-3 keys.

import (
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/i18n"
)

// s53DeliberatelyIdenticalKeys holds the small set of new keys that are
// intentionally the SAME raw text in both languages: queue.slot.mode_idle is
// literal machine evidence (Watching.Mode == "idle"), never translated, the
// same convention every ReasonCode's raw value already follows elsewhere in
// this codebase.
var s53DeliberatelyIdenticalKeys = map[string]bool{
	"queue.slot.mode_idle": true,
}

func TestS5_3LocaleKeysPresentAndTranslated(t *testing.T) {
	loc, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}
	keys := []string{
		"nav.queue",
		"lc.reason.transitioning", "js.lc.state_unconfirmed", "lc.stale.diagnostics_link",
		"queue.reason.restricted_drop", "queue.reason.streak", "queue.reason.active_drop",
		"queue.reason.fair_rotation", "queue.reason.priority", "queue.reason.discovery_fill",
		"queue.reason.lower_priority", "queue.reason.unknown",
		"queue.status.watching", "queue.status.queued", "queue.status.offline",
		"queue.status.disabled", "queue.status.unknown",
		"queue.slot.link_label", "queue.slot.empty", "queue.slot.no_data", "queue.slot.mode_idle",
		"queue.title", "queue.roster.caption", "queue.col.status", "queue.col.channel",
		"queue.col.reason", "queue.col.points", "queue.col.points_today",
		"queue.dpba.text", "queue.toolbar.label",
		"queue.filter.label", "queue.filter.placeholder", "queue.filter.clear",
		"queue.filter.status.label", "queue.filter.status.all", "queue.filter.reset",
		"queue.sort.label", "queue.sort.default", "queue.sort.channel", "queue.sort.points", "queue.sort.today",
		"queue.empty.title", "queue.empty.action",
		"js.queue.sort_applied", "js.queue.shown_count", "js.queue.no_results",
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
		if en == ru && !s53DeliberatelyIdenticalKeys[k] {
			t.Errorf("%q has identical EN/RU text %q — looks untranslated", k, en)
		}
	}
}

// TestS5_3DiscoveryFillRussianMeansFreeNotSimple proves the RU translation of
// queue.reason.discovery_fill describes an unoccupied ("свободного") slot,
// matching the EN "idle slot" meaning — never "простого" ("simple"), which
// changes the meaning entirely (CodeRabbit PR152 finding).
func TestS5_3DiscoveryFillRussianMeansFreeNotSimple(t *testing.T) {
	loc, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}
	ru := loc.T(i18n.LangRU, "queue.reason.discovery_fill")
	if strings.Contains(ru, "простого") {
		t.Errorf("queue.reason.discovery_fill RU text must not use \"простого\" (simple), got %q", ru)
	}
	if !strings.Contains(ru, "свободного") {
		t.Errorf("queue.reason.discovery_fill RU text must describe an unoccupied slot with \"свободного\", got %q", ru)
	}
}

// TestS5_3JSKeysReachClientCatalog proves every js.*-prefixed S5-3 key
// actually reaches the client-side window.I18N catalog (JSMessages only
// forwards the js.* namespace - see i18n.go) in both languages, so the
// stale-gating and queue-page scripts can resolve them at runtime.
func TestS5_3JSKeysReachClientCatalog(t *testing.T) {
	loc, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}
	jsKeys := []string{"js.lc.state_unconfirmed", "js.queue.sort_applied", "js.queue.shown_count", "js.queue.no_results"}
	for _, lang := range []string{i18n.LangEN, i18n.LangRU} {
		msgs := loc.JSMessages(lang)
		for _, k := range jsKeys {
			if _, ok := msgs[k]; !ok {
				t.Errorf("[%s] window.I18N catalog missing %q", lang, k)
			}
		}
	}
}
