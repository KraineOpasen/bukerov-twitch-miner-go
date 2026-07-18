package web

import (
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/i18n"
)

// This file pins the v0.13.7 logger Settings-UI hotfix (§7): the dead `less`
// checkbox is removed (config field retained for back-compat), and a localized
// restart-required note is shown because logger settings apply only at startup.

// §8.33: the dead `less` checkbox and its i18n labels are gone; the working
// `colored` checkbox stays.
func TestSettingsDropsDeadLessCheckbox(t *testing.T) {
	settings, err := templatesFS.ReadFile("templates/settings.html")
	if err != nil {
		t.Fatalf("read settings.html: %v", err)
	}
	s := string(settings)
	if strings.Contains(s, `id="less"`) {
		t.Error("the dead `less` checkbox must be removed from the Settings page")
	}
	for _, key := range []string{"set.logger.less_label", "set.logger.less_desc"} {
		if strings.Contains(s, key) {
			t.Errorf("the `less` i18n reference %q must be removed from the Settings page", key)
		}
	}
	if !strings.Contains(s, `id="colored"`) {
		t.Error("the working `colored` checkbox must stay")
	}
}

// §8.32: logger settings apply only at startup, so the Settings page carries a
// localized restart-required note.
func TestLoggerRestartNotePresent(t *testing.T) {
	settings, err := templatesFS.ReadFile("templates/settings.html")
	if err != nil {
		t.Fatalf("read settings.html: %v", err)
	}
	if !strings.Contains(string(settings), "set.logger.restart_note") {
		t.Error("the logger section must render the restart-required note")
	}

	loc, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}
	if en := loc.T(i18n.LangEN, "set.logger.restart_note"); !strings.Contains(strings.ToLower(en), "restart") {
		t.Errorf("EN restart note must mention restarting, got %q", en)
	}
	if ru := loc.T(i18n.LangRU, "set.logger.restart_note"); !strings.Contains(ru, "перезапуск") {
		t.Errorf("RU restart note must mention restart, got %q", ru)
	}
}
