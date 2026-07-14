package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
)

// fakeFollowedProvider satisfies web.FollowedProvider for endpoint tests. It
// records the logins handed to ImportStreamers so the dedup/skip contract can be
// asserted without a live miner.
type fakeFollowedProvider struct {
	followed    []api.FollowedChannel
	truncated   bool
	followedErr error
	tracked     []string

	importedWith []string // captured argument of the last ImportStreamers call
}

func (f *fakeFollowedProvider) FollowedChannels() ([]api.FollowedChannel, bool, error) {
	return f.followed, f.truncated, f.followedErr
}

func (f *fakeFollowedProvider) TrackedUsernames() []string { return f.tracked }

func (f *fakeFollowedProvider) ImportStreamers(logins []string) (int, error) {
	f.importedWith = logins
	// Mirror the real dedup contract: only logins not already tracked count.
	tracked := map[string]bool{}
	for _, t := range f.tracked {
		tracked[strings.ToLower(strings.TrimSpace(t))] = true
	}
	added := 0
	seen := map[string]bool{}
	for _, l := range logins {
		l = strings.ToLower(strings.TrimSpace(l))
		if l == "" || tracked[l] || seen[l] {
			continue
		}
		seen[l] = true
		added++
	}
	return added, nil
}

func TestFollowedEndpointMarksTrackedAndSorts(t *testing.T) {
	srv := newStatsTestServer(t)
	srv.SetFollowedProvider(&fakeFollowedProvider{
		followed: []api.FollowedChannel{
			{Login: "Zeta", DisplayName: "Zeta"},
			{Login: "Alpha", DisplayName: "Alpha"},
			{Login: "Beta", DisplayName: "Beta"},
		},
		tracked:   []string{"beta"},
		truncated: true,
	})

	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/followed", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp followedResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if !resp.Truncated {
		t.Errorf("truncated = false, want true")
	}
	if resp.Cap != maxFollowedFetch {
		t.Errorf("cap = %d, want %d", resp.Cap, maxFollowedFetch)
	}
	if len(resp.Channels) != 3 {
		t.Fatalf("got %d channels, want 3", len(resp.Channels))
	}
	// Untracked first (alpha, zeta alphabetically), tracked (beta) last.
	if resp.Channels[0].Login != "alpha" || resp.Channels[1].Login != "zeta" {
		t.Errorf("untracked order wrong: %+v", resp.Channels)
	}
	last := resp.Channels[2]
	if last.Login != "beta" || !last.AlreadyTracked {
		t.Errorf("tracked channel should sort last and be flagged: %+v", last)
	}
	for _, c := range resp.Channels[:2] {
		if c.AlreadyTracked {
			t.Errorf("untracked channel flagged as tracked: %+v", c)
		}
	}
}

func TestFollowedEndpointNoProvider(t *testing.T) {
	srv := newStatsTestServer(t)
	// No SetFollowedProvider call.
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/followed", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when no provider wired", rec.Code)
	}
}

func TestFollowedImportAddsSelected(t *testing.T) {
	srv := newStatsTestServer(t)
	fp := &fakeFollowedProvider{tracked: []string{"beta"}}
	srv.SetFollowedProvider(fp)

	body := strings.NewReader(`{"logins":["alpha","beta","gamma"]}`)
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/followed/import", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Success bool   `json:"success"`
		Added   int    `json:"added"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// alpha + gamma are new; beta is already tracked.
	if resp.Added != 2 {
		t.Errorf("added = %d, want 2", resp.Added)
	}
	if !resp.Success {
		t.Errorf("success = false, want true")
	}
	if len(fp.importedWith) != 3 {
		t.Errorf("provider should receive all 3 selected logins, got %v", fp.importedWith)
	}
}

func TestFollowedImportEmptySelection(t *testing.T) {
	srv := newStatsTestServer(t)
	srv.SetFollowedProvider(&fakeFollowedProvider{})

	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/followed/import", strings.NewReader(`{"logins":[]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for empty selection", rec.Code)
	}
}

func TestFollowedImportRejectsGet(t *testing.T) {
	srv := newStatsTestServer(t)
	srv.SetFollowedProvider(&fakeFollowedProvider{})

	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/followed/import", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405 for GET on import", rec.Code)
	}
}

// TestSettingsPageRendersImportButton guards against a template parse/render
// regression in the followed-import modal markup: loadTemplates logs and skips a
// broken template rather than failing, so only a real render catches it.
func TestSettingsPageRendersImportButton(t *testing.T) {
	srv := newStatsTestServer(t)

	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"import-followed-btn", "followed-import-modal", "Import from followed"} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page missing %q", want)
		}
	}
}

func TestImportMessage(t *testing.T) {
	cases := []struct {
		added, requested int
		want             string
	}{
		{0, 3, "No new channels added (all selected were already tracked)."},
		{1, 1, "Added 1 channel to the tracked list."},
		{4, 5, "Added 4 channels to the tracked list."},
	}
	for _, tc := range cases {
		if got := importMessage(tc.added, tc.requested); got != tc.want {
			t.Errorf("importMessage(%d,%d) = %q, want %q", tc.added, tc.requested, got, tc.want)
		}
	}
}
