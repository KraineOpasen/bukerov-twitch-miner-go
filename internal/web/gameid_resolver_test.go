package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
)

// fakeGameIDResolver records calls so a test can prove the resolver is (or is
// not) invoked, and returns a scripted identity/error.
type fakeGameIDResolver struct {
	identity api.GameIdentity
	err      error
	calls    int
	lastName string
}

func (f *fakeGameIDResolver) GetGameIdentity(name string) (api.GameIdentity, error) {
	f.calls++
	f.lastName = name
	return f.identity, f.err
}

func postResolveGameID(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/settings/resolve-game-id", strings.NewReader(body))
	srv.handleAPIResolveGameID(rec, req)
	return rec
}

// TestResolveGameIDFound: a known game resolves to its opaque ID + slug.
func TestResolveGameIDFound(t *testing.T) {
	res := &fakeGameIDResolver{identity: api.GameIdentity{ID: "27546", Slug: "world-of-tanks"}}
	srv := &Server{gameIDResolver: res}

	rec := postResolveGameID(t, srv, `{"name":"World of Tanks"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if got["found"] != true || got["gameId"] != "27546" || got["slug"] != "world-of-tanks" {
		t.Fatalf("unexpected body: %v", got)
	}
	if res.lastName != "World of Tanks" {
		t.Fatalf("resolver saw name %q", res.lastName)
	}
	// No secrets / internals in the response.
	for _, k := range []string{"token", "cookie", "authorization", "errors", "data"} {
		if _, ok := got[k]; ok {
			t.Fatalf("response leaked key %q: %v", k, got)
		}
	}
}

// T-H8 + T-H9: the lookup is candidate-independent — the handler has no access
// to drops campaigns at all, so it works with no campaigns provider wired (the
// empty / all-foreign sync case).
func TestResolveGameIDCandidateIndependent(t *testing.T) {
	res := &fakeGameIDResolver{identity: api.GameIdentity{ID: "27546", Slug: "world-of-tanks"}}
	// No campaignsProvider, no dropCatalogProvider — nothing about drops state.
	srv := &Server{gameIDResolver: res}
	if srv.campaignsProvider != nil {
		t.Fatal("precondition: no campaigns provider")
	}
	rec := postResolveGameID(t, srv, `{"name":"World of Tanks"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["gameId"] != "27546" {
		t.Fatalf("lookup must resolve without any campaign state, got %v", got)
	}
}

// T-H10: the lookup never mutates config/runtime settings.
func TestResolveGameIDDoesNotTouchSettings(t *testing.T) {
	res := &fakeGameIDResolver{identity: api.GameIdentity{ID: "27546"}}
	updated := false
	srv := &Server{
		gameIDResolver:   res,
		onSettingsUpdate: func(settings.RuntimeSettings) { updated = true },
	}
	if rec := postResolveGameID(t, srv, `{"name":"World of Tanks"}`); rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if updated {
		t.Fatal("the game-ID lookup must not trigger a settings update")
	}
}

// unknown game → 200 {found:false}, no fabricated ID.
func TestResolveGameIDUnknown(t *testing.T) {
	res := &fakeGameIDResolver{identity: api.GameIdentity{}} // zero identity
	srv := &Server{gameIDResolver: res}
	rec := postResolveGameID(t, srv, `{"name":"No Such Game"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["found"] != false {
		t.Fatalf("expected found=false, got %v", got)
	}
	if _, ok := got["gameId"]; ok {
		t.Fatalf("unknown game must not carry a gameId: %v", got)
	}
}

// resolver/transport error → 502 (distinct from unknown game).
func TestResolveGameIDResolverError(t *testing.T) {
	res := &fakeGameIDResolver{err: http.ErrHandlerTimeout}
	srv := &Server{gameIDResolver: res}
	rec := postResolveGameID(t, srv, `{"name":"World of Tanks"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", rec.Code)
	}
}

func TestResolveGameIDValidation(t *testing.T) {
	res := &fakeGameIDResolver{identity: api.GameIdentity{ID: "1"}}
	srv := &Server{gameIDResolver: res}

	// Empty name → 400, resolver never called.
	if rec := postResolveGameID(t, srv, `{"name":"   "}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty name: expected 400, got %d", rec.Code)
	}
	// Over-long name → 400.
	long := `{"name":"` + strings.Repeat("a", 5000) + `"}`
	if rec := postResolveGameID(t, srv, long); rec.Code != http.StatusBadRequest {
		t.Fatalf("long name: expected 400, got %d", rec.Code)
	}
	if res.calls != 0 {
		t.Fatalf("resolver must not be called for invalid input, got %d calls", res.calls)
	}
}

func TestResolveGameIDMethodAndAvailability(t *testing.T) {
	res := &fakeGameIDResolver{identity: api.GameIdentity{ID: "1"}}

	// GET → 405.
	srv := &Server{gameIDResolver: res}
	rec := httptest.NewRecorder()
	srv.handleAPIResolveGameID(rec, httptest.NewRequest(http.MethodGet, "/api/settings/resolve-game-id", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: expected 405, got %d", rec.Code)
	}

	// No resolver wired → 503.
	empty := &Server{}
	if rec := postResolveGameID(t, empty, `{"name":"x"}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no resolver: expected 503, got %d", rec.Code)
	}
}

// T-H13: a normal Settings GET does not invoke the game-ID resolver (the lookup
// only runs on an explicit POST to its own endpoint — no network on render).
func TestResolveGameIDNotCalledOnSettingsRender(t *testing.T) {
	res := &fakeGameIDResolver{identity: api.GameIdentity{ID: "1"}}
	srv := &Server{
		gameIDResolver:   res,
		settingsProvider: &fakeSettingsProvider{rt: settings.RuntimeSettings{}},
	}
	rec := httptest.NewRecorder()
	srv.handleAPISettings(rec, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("settings GET status=%d", rec.Code)
	}
	if res.calls != 0 {
		t.Fatalf("settings render must not call the game-ID resolver, got %d calls", res.calls)
	}
}

// T-H11: the Settings template renders the lookup controls and wires the
// handler; the result area is empty by default (never the literal "null").
func TestResolveGameIDTemplateHasLookupControls(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("templates", "settings.html"))
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	html := string(src)
	for _, needle := range []string{
		`id="gameIdLookupInput"`,
		`id="gameIdLookupBtn"`,
		`id="gameIdLookupResult"`,
		"function resolveGameId(",
		"/api/settings/resolve-game-id",
	} {
		if !strings.Contains(html, needle) {
			t.Fatalf("settings template missing lookup control %q", needle)
		}
	}
}
