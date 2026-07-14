package web

import (
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/i18n"
)

// langCookieName holds the user's selected UI language. It is the source of
// truth for localization (per the design decision to keep language a
// presentation concern, not part of the miner's operational config.json).
const langCookieName = "lang"

// mustLocalizer loads the embedded catalogs, degrading to an empty localizer
// (which renders raw keys) rather than failing server start-up if the embedded
// JSON is somehow unreadable — a condition the i18n key-parity test already
// guards against at build time.
func mustLocalizer() *i18n.Localizer {
	loc, err := i18n.New()
	if err != nil {
		slog.Error("i18n: failed to load locales; UI will show raw keys", "error", err)
		return i18n.Empty()
	}
	return loc
}

// langFromRequest resolves the request language from the cookie, defaulting when
// absent or unrecognized. Untrusted input is normalized to a supported value.
func (s *Server) langFromRequest(r *http.Request) string {
	if r != nil {
		if c, err := r.Cookie(langCookieName); err == nil {
			return i18n.NormalizeLang(c.Value)
		}
	}
	return i18n.DefaultLang
}

// placeholderFuncMap defines the localization funcs at parse time so templates
// that call them parse successfully. Each per-language clone overrides these
// with real, language-bound implementations (see funcMapFor).
func placeholderFuncMap() template.FuncMap {
	return template.FuncMap{
		"t":          func(string) string { return "" },
		"lang":       func() string { return i18n.DefaultLang },
		"jsMessages": func() template.JS { return template.JS("{}") },
	}
}

// funcMapFor returns the localization funcs bound to a specific language:
//   - t "key"      -> the translated string (fallback: default lang, then key)
//   - lang         -> the active language code (for <html lang> and the switcher)
//   - jsMessages   -> the js.* catalog as a JSON literal injected for client JS
func funcMapFor(loc *i18n.Localizer, lang string) template.FuncMap {
	return template.FuncMap{
		"t":    func(key string) string { return loc.T(lang, key) },
		"lang": func() string { return lang },
		"jsMessages": func() template.JS {
			// json.Marshal escapes <, >, & to \u00xx, so embedding the catalog in
			// an inline <script> can't break out of the element.
			b, err := json.Marshal(loc.JSMessages(lang))
			if err != nil {
				return template.JS("{}")
			}
			return template.JS(b)
		},
	}
}

// handleAPILang persists the chosen UI language in a cookie and asks the client
// to refresh so every string re-renders in the new language. State-changing, so
// it is POST-only and — being registered on the shared mux — is covered by the
// same csrfProtectMiddleware as every other mutating endpoint.
func (s *Server) handleAPILang(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeNotAllowed(w)
		return
	}
	lang := i18n.NormalizeLang(r.FormValue("lang"))
	http.SetCookie(w, &http.Cookie{
		Name:     langCookieName,
		Value:    lang,
		Path:     "/",
		MaxAge:   365 * 24 * 60 * 60,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	// HTMX performs a full page reload on HX-Refresh; a plain client gets 204 and
	// can reload itself.
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}
