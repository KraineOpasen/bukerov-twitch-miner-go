package web

import (
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/i18n"
)

// TestLifecycleConfirmStopMentionsContainerAndVolume pins the honesty
// contract of the Stop confirm text (design v6 §4/§10): it must say the
// container keeps running and that the intent lives on the /database
// volume, in BOTH languages — a wording contract, not just a key existing.
func TestLifecycleConfirmStopMentionsContainerAndVolume(t *testing.T) {
	loc, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}

	en := loc.T(i18n.LangEN, "lc.confirm.stop")
	for _, want := range []string{"container", "/database", "docker stop"} {
		if !strings.Contains(en, want) {
			t.Errorf("EN lc.confirm.stop %q must mention %q", en, want)
		}
	}

	ru := loc.T(i18n.LangRU, "lc.confirm.stop")
	for _, want := range []string{"контейнер", "/database", "docker stop"} {
		if !strings.Contains(ru, want) {
			t.Errorf("RU lc.confirm.stop %q must mention %q", ru, want)
		}
	}
}

// TestLifecycleInsecureDisabledMentionsEnvVar pins that the InsecureNoAuth
// explanation names the exact environment variable an operator needs to
// recognize (design v6 §10), in both languages.
func TestLifecycleInsecureDisabledMentionsEnvVar(t *testing.T) {
	loc, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}

	en := loc.T(i18n.LangEN, "lc.insecure_disabled")
	if !strings.Contains(en, "DASHBOARD_INSECURE_NO_AUTH") {
		t.Errorf("EN lc.insecure_disabled %q must mention DASHBOARD_INSECURE_NO_AUTH", en)
	}

	ru := loc.T(i18n.LangRU, "lc.insecure_disabled")
	if !strings.Contains(ru, "DASHBOARD_INSECURE_NO_AUTH") {
		t.Errorf("RU lc.insecure_disabled %q must mention DASHBOARD_INSECURE_NO_AUTH", ru)
	}
}
