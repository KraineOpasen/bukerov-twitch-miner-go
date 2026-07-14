package i18n

import (
	"reflect"
	"strings"
	"testing"
)

func newTestLocalizer(t *testing.T) *Localizer {
	t.Helper()
	loc, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return loc
}

// TestLocaleKeyParity is the completeness gate: every supported language must
// define exactly the same set of keys. A key present in one catalog but missing
// in another would leave that string untranslated in one language — the very
// language-mixing this feature exists to remove. This fails CI if the catalogs
// drift.
func TestLocaleKeyParity(t *testing.T) {
	loc := newTestLocalizer(t)

	ref := DefaultLang
	refKeys := loc.Keys(ref)
	if len(refKeys) == 0 {
		t.Fatalf("default locale %q has no keys", ref)
	}
	refSet := make(map[string]struct{}, len(refKeys))
	for _, k := range refKeys {
		refSet[k] = struct{}{}
	}

	for _, lang := range SupportedLangs() {
		if lang == ref {
			continue
		}
		keys := loc.Keys(lang)
		set := make(map[string]struct{}, len(keys))
		for _, k := range keys {
			set[k] = struct{}{}
		}
		for _, k := range refKeys {
			if _, ok := set[k]; !ok {
				t.Errorf("locale %q is missing key %q (present in %q)", lang, k, ref)
			}
		}
		for _, k := range keys {
			if _, ok := refSet[k]; !ok {
				t.Errorf("locale %q has extra key %q (absent from %q)", lang, k, ref)
			}
		}
	}
}

// TestNoEmptyValues guards against a key defined but left blank in any locale.
func TestNoEmptyValues(t *testing.T) {
	loc := newTestLocalizer(t)
	for _, lang := range SupportedLangs() {
		for _, k := range loc.Keys(lang) {
			if strings.TrimSpace(loc.T(lang, k)) == "" {
				t.Errorf("locale %q key %q has an empty value", lang, k)
			}
		}
	}
}

func TestTFallbackChain(t *testing.T) {
	loc := newTestLocalizer(t)

	// Present key resolves in each language.
	if got := loc.T(LangEN, "nav.overview"); got != "Overview" {
		t.Errorf("EN nav.overview = %q, want Overview", got)
	}
	if got := loc.T(LangRU, "nav.overview"); got != "Обзор" {
		t.Errorf("RU nav.overview = %q, want Обзор", got)
	}
	// Unknown key falls back to the key itself (visible, not blank).
	if got := loc.T(LangEN, "does.not.exist"); got != "does.not.exist" {
		t.Errorf("missing key = %q, want the key itself", got)
	}
	// Unknown language normalizes to the default.
	if got := loc.T("fr", "nav.overview"); got != loc.T(DefaultLang, "nav.overview") {
		t.Errorf("unknown lang did not fall back to default")
	}
}

func TestNormalizeLang(t *testing.T) {
	cases := map[string]string{
		"ru": LangRU, "en": LangEN, "": DefaultLang, "fr": DefaultLang, "RU": DefaultLang,
	}
	for in, want := range cases {
		if got := NormalizeLang(in); got != want {
			t.Errorf("NormalizeLang(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestJSMessagesOnlyJSKeys(t *testing.T) {
	loc := newTestLocalizer(t)
	msgs := loc.JSMessages(LangRU)
	if len(msgs) == 0 {
		t.Fatal("JSMessages returned nothing")
	}
	for k := range msgs {
		if !strings.HasPrefix(k, jsKeyPrefix) {
			t.Errorf("JSMessages leaked non-js key %q", k)
		}
	}
	if got := msgs["js.overlay.error"]; got != "Ошибка" {
		t.Errorf("js.overlay.error (ru) = %q, want Ошибка", got)
	}
}

// TestJSMessagesKeyParity ensures the injected client catalog is identical in
// shape across languages (guards the same mixing risk on the JS side).
func TestJSMessagesKeyParity(t *testing.T) {
	loc := newTestLocalizer(t)
	ru := keysOf(loc.JSMessages(LangRU))
	en := keysOf(loc.JSMessages(LangEN))
	if !reflect.DeepEqual(ru, en) {
		t.Errorf("JS catalog keys differ between ru and en:\n ru=%v\n en=%v", ru, en)
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
