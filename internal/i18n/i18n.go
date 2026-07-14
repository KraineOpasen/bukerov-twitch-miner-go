// Package i18n provides a minimal, dependency-free localization layer for the
// dashboard UI: JSON message catalogs embedded at build time plus a lookup with
// a deterministic fallback chain (requested language -> default language -> the
// key itself, so a missing translation is visible rather than blank).
//
// Scope: only user-facing UI text is localized. Operational slog output
// (INFO/WARN/ERROR seen in `docker logs`) is intentionally NOT localized and
// must never go through this package.
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

//go:embed locales/*.json
var localesFS embed.FS

// Language codes and the default. Russian is the default per product decision
// (the operator is Russophone and much of the UI is already Russian).
const (
	LangRU      = "ru"
	LangEN      = "en"
	DefaultLang = LangRU
)

// jsKeyPrefix marks catalog keys that are consumed by client-side JavaScript
// (toasts, dynamically-built DOM). Only these are injected into the page for the
// browser; everything else stays server-side.
const jsKeyPrefix = "js."

// Localizer holds the loaded catalogs, keyed by language then message key.
type Localizer struct {
	messages map[string]map[string]string
}

// New loads every locales/<lang>.json catalog embedded in the binary.
func New() (*Localizer, error) {
	entries, err := localesFS.ReadDir("locales")
	if err != nil {
		return nil, fmt.Errorf("read locales dir: %w", err)
	}

	l := &Localizer{messages: make(map[string]map[string]string)}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		lang := strings.TrimSuffix(e.Name(), ".json")
		data, err := localesFS.ReadFile("locales/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("read locale %s: %w", e.Name(), err)
		}
		var m map[string]string
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("parse locale %s: %w", e.Name(), err)
		}
		l.messages[lang] = m
	}

	if _, ok := l.messages[DefaultLang]; !ok {
		return nil, fmt.Errorf("default locale %q not found in embedded catalogs", DefaultLang)
	}
	return l, nil
}

// Empty returns a Localizer with no catalogs; every lookup falls through to the
// key itself. Used as a safe degraded fallback if catalog loading ever fails.
func Empty() *Localizer {
	return &Localizer{messages: map[string]map[string]string{}}
}

// SupportedLangs returns the available languages in a stable display order
// (default first) for the language switcher.
func SupportedLangs() []string {
	return []string{LangRU, LangEN}
}

// NormalizeLang maps any input to a supported language, defaulting when unknown
// or empty. Safe to call on untrusted cookie/form input.
func NormalizeLang(lang string) string {
	switch lang {
	case LangRU, LangEN:
		return lang
	default:
		return DefaultLang
	}
}

// T returns the message for key in lang, falling back to the default language
// and finally to the key itself.
func (l *Localizer) T(lang, key string) string {
	lang = NormalizeLang(lang)
	if m, ok := l.messages[lang]; ok {
		if s, ok := m[key]; ok {
			return s
		}
	}
	if lang != DefaultLang {
		if s, ok := l.messages[DefaultLang][key]; ok {
			return s
		}
	}
	return key
}

// JSMessages returns the js.*-prefixed catalog entries for lang (with the
// default-language fallback applied per key), for injection into the page so
// client-side JavaScript can localize dynamically-built text.
func (l *Localizer) JSMessages(lang string) map[string]string {
	lang = NormalizeLang(lang)
	out := make(map[string]string)
	// Union of keys across the requested and default languages so a key present
	// only in the default catalog still reaches the client (via T's fallback).
	seen := map[string]struct{}{}
	for _, src := range []string{lang, DefaultLang} {
		for k := range l.messages[src] {
			if !strings.HasPrefix(k, jsKeyPrefix) {
				continue
			}
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			out[k] = l.T(lang, k)
		}
	}
	return out
}

// Keys returns the sorted key set for lang (used by the completeness test).
func (l *Localizer) Keys(lang string) []string {
	m := l.messages[NormalizeLang(lang)]
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
