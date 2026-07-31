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

// TestLifecycleLANWordingContract pins the Ф4d §7a trusted-LAN wording
// contract in BOTH languages:
//   - every new lc.lan.*/lc.result.lan_denied key must name the exact
//     environment variable an operator needs to recognize
//     (DASHBOARD_TRUSTED_LAN_CIDRS);
//   - the two panel-facing texts (lc.lan.allowed/lc.lan.denied) must be
//     explicit that the check uses the direct connection address and
//     ignores proxy headers, so an operator behind a reverse proxy is not
//     misled into thinking Forwarded/X-Forwarded-For/X-Real-IP participate.
func TestLifecycleLANWordingContract(t *testing.T) {
	loc, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}

	directPhrase := map[string]string{
		i18n.LangEN: "direct connection",
		i18n.LangRU: "прямого подключения",
	}

	for _, lang := range []string{i18n.LangEN, i18n.LangRU} {
		for _, key := range []string{"lc.lan.allowed", "lc.lan.denied", "lc.result.lan_denied"} {
			got := loc.T(lang, key)
			if !strings.Contains(got, "DASHBOARD_TRUSTED_LAN_CIDRS") {
				t.Errorf("[%s] %s = %q must name DASHBOARD_TRUSTED_LAN_CIDRS", lang, key, got)
			}
		}

		for _, key := range []string{"lc.lan.allowed", "lc.lan.denied"} {
			got := loc.T(lang, key)
			for _, header := range []string{"Forwarded", "X-Forwarded-For", "X-Real-IP"} {
				if !strings.Contains(got, header) {
					t.Errorf("[%s] %s = %q must state that %s is ignored", lang, key, got, header)
				}
			}
			if !strings.Contains(got, directPhrase[lang]) {
				t.Errorf("[%s] %s = %q must state the check uses the direct connection address", lang, key, got)
			}
		}
	}
}
