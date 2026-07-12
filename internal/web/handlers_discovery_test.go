package web

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/discovery"
)

type fakeDiscoveryProvider struct {
	state discovery.State
}

func (f *fakeDiscoveryProvider) State() discovery.State { return f.state }

func TestBuildDiscoveryViewOrdersByStatusThenViewers(t *testing.T) {
	state := discovery.State{
		Enabled: true,
		Games:   []string{"World of Tanks"},
		Channels: []discovery.ChannelState{
			{Login: "gone_channel", Game: "World of Tanks", Viewers: 9999, Status: "offline"},
			{Login: "small_available", Game: "World of Tanks", Viewers: 100, Status: "available"},
			{Login: "watched_channel", Game: "World of Tanks", Viewers: 500, Status: "watching", MinutesWatched: 12},
			{Login: "big_available", Game: "World of Tanks", Viewers: 2000, Status: "available"},
		},
		Watching: "watched_channel",
	}

	data := buildDiscoveryView(state)

	got := make([]string, len(data.Channels))
	for i, ch := range data.Channels {
		got[i] = ch.Login
	}
	want := []string{"watched_channel", "big_available", "small_available", "gone_channel"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unexpected order: got %v, want %v", got, want)
		}
	}

	watching := data.Channels[0]
	if !watching.Watching || !watching.HasMinutesWatched || watching.MinutesWatched != 12 {
		t.Errorf("unexpected watching row: %+v", watching)
	}
	if data.Channels[1].ViewersFormatted != "2,000" {
		t.Errorf("expected formatted viewer count, got %q", data.Channels[1].ViewersFormatted)
	}
	if !data.Channels[3].Offline {
		t.Errorf("expected offline flag on gone_channel, got %+v", data.Channels[3])
	}
}

// TestDiscoveryListTemplateRenders exercises the three states of the
// discovery_list partial: disabled, empty pool, and a populated pool.
func TestDiscoveryListTemplateRenders(t *testing.T) {
	templates := loadTemplates()
	partials := templates["partials"]
	if partials == nil {
		t.Fatal("partials template not loaded")
	}

	var buf bytes.Buffer
	if err := partials.ExecuteTemplate(&buf, "discovery_list", DiscoveryListData{Enabled: false}); err != nil {
		t.Fatalf("disabled render failed: %v", err)
	}
	if !strings.Contains(buf.String(), "Directory discovery is off") {
		t.Errorf("disabled state should point at Settings:\n%s", buf.String())
	}

	buf.Reset()
	if err := partials.ExecuteTemplate(&buf, "discovery_list", DiscoveryListData{
		Enabled: true, Games: []string{"World of Tanks", "Rust"},
	}); err != nil {
		t.Fatalf("empty-pool render failed: %v", err)
	}
	if !strings.Contains(buf.String(), "No live drops-enabled channels") || !strings.Contains(buf.String(), "World of Tanks") {
		t.Errorf("empty-pool state should list configured games:\n%s", buf.String())
	}

	buf.Reset()
	if err := partials.ExecuteTemplate(&buf, "discovery_list", DiscoveryListData{
		Enabled: true,
		Games:   []string{"World of Tanks"},
		Channels: []DiscoveredChannelView{
			{Login: "watched_channel", Game: "World of Tanks", Status: "watching", ViewersFormatted: "5,400",
				Watching: true, HasMinutesWatched: true, MinutesWatched: 12},
			{Login: "backup_channel", Game: "World of Tanks", Status: "available", ViewersFormatted: "130"},
			{Login: "gone_channel", Game: "World of Tanks", Status: "offline", ViewersFormatted: "99", Offline: true},
		},
	}); err != nil {
		t.Fatalf("populated render failed: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"watched_channel", "Watching", "12 min watched", "5,400", "Available", "Offline", "twitch.tv/backup_channel"} {
		if !strings.Contains(out, want) {
			t.Errorf("populated output missing %q:\n%s", want, out)
		}
	}
}

func TestHandleAPIDiscovery(t *testing.T) {
	s := &Server{templates: loadTemplates()}
	s.SetDiscoveryProvider(&fakeDiscoveryProvider{state: discovery.State{
		Enabled:  true,
		Games:    []string{"World of Tanks"},
		Channels: []discovery.ChannelState{{Login: "some_channel", Game: "World of Tanks", Viewers: 42, Status: "available"}},
	}})

	rec := httptest.NewRecorder()
	s.handleAPIDiscovery(rec, httptest.NewRequest("GET", "/api/discovery", nil))

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "some_channel") {
		t.Errorf("response missing channel row:\n%s", rec.Body.String())
	}
}

// TestDropsPageRendersDiscoverySection executes the full Drops page (through
// base.html) to prove the new Discovered Channels section parses and renders.
func TestDropsPageRendersDiscoverySection(t *testing.T) {
	s := &Server{templates: loadTemplates()}

	rec := httptest.NewRecorder()
	s.renderPage(rec, "drops.html", DropsPageData{Username: "tester", RefreshMinutes: 5})

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Discovered Channels") || !strings.Contains(body, "/api/discovery") {
		t.Errorf("drops page missing the discovery section:\n%s", body)
	}
}

func TestHandleAPIDiscoveryWithoutProvider(t *testing.T) {
	s := &Server{templates: loadTemplates()}

	rec := httptest.NewRecorder()
	s.handleAPIDiscovery(rec, httptest.NewRequest("GET", "/api/discovery", nil))

	if rec.Code != 200 {
		t.Fatalf("expected 200 with the disabled-state body, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Directory discovery is off") {
		t.Errorf("expected disabled-state body without a provider:\n%s", rec.Body.String())
	}
}
