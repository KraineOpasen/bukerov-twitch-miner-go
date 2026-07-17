package web

import (
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/i18n"
)

// TestColoredSettingLabels pins the clarified wording of the Logger "Colored"
// setting: it names the Docker console as its scope and explicitly says the
// built-in Logs page (whose colors/emojis are server-classified) is not
// affected. The JSON key and Go field are untouched — only the UI text.
func TestColoredSettingLabels(t *testing.T) {
	loc, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}

	ruLabel := loc.T(i18n.LangRU, "set.logger.colored_label")
	if !strings.Contains(ruLabel, "консоли Docker") {
		t.Errorf("RU label %q must mention the Docker console", ruLabel)
	}
	ruDesc := loc.T(i18n.LangRU, "set.logger.colored_desc")
	for _, want := range []string{"stdout", "«Логи»", "эмодзи"} {
		if !strings.Contains(ruDesc, want) {
			t.Errorf("RU description %q must mention %q", ruDesc, want)
		}
	}

	enLabel := loc.T(i18n.LangEN, "set.logger.colored_label")
	if !strings.Contains(enLabel, "Docker console") {
		t.Errorf("EN label %q must mention the Docker console", enLabel)
	}
	enDesc := loc.T(i18n.LangEN, "set.logger.colored_desc")
	for _, want := range []string{"stdout", "Logs page", "emojis"} {
		if !strings.Contains(enDesc, want) {
			t.Errorf("EN description %q must mention %q", enDesc, want)
		}
	}
}
