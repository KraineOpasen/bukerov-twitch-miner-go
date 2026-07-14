package web

import (
	"html/template"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/i18n"
)

// testLoadTemplates builds the per-language template sets for tests using the
// real embedded catalogs.
func testLoadTemplates(t *testing.T) (map[string]map[string]*template.Template, map[string]*template.Template) {
	t.Helper()
	loc, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}
	return loadTemplates(loc)
}

// testPartials returns the default-language partial set for direct
// ExecuteTemplate calls in tests.
func testPartials(t *testing.T) *template.Template {
	t.Helper()
	_, partials := testLoadTemplates(t)
	p := partials[i18n.DefaultLang]
	if p == nil {
		t.Fatal("default-language partials not loaded")
	}
	return p
}

// newRenderServer builds a minimal Server wired with localized templates, for
// tests that exercise renderPage/renderPartial or handlers directly.
func newRenderServer(t *testing.T) *Server {
	t.Helper()
	loc, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}
	pages, partials := loadTemplates(loc)
	return &Server{i18n: loc, templates: pages, partials: partials}
}
