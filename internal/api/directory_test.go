package api

import "testing"

func directoryResponse(edges []interface{}) map[string]interface{} {
	return map[string]interface{}{
		"data": map[string]interface{}{
			"game": map[string]interface{}{
				"id":          "27546",
				"name":        "World of Tanks",
				"displayName": "World of Tanks",
				"streams": map[string]interface{}{
					"edges": edges,
				},
			},
		},
	}
}

func directoryEdge(id, login, display string, viewers float64) map[string]interface{} {
	return map[string]interface{}{
		"node": map[string]interface{}{
			"id":           "stream-" + id,
			"title":        "playing " + login,
			"viewersCount": viewers,
			"broadcaster": map[string]interface{}{
				"id":          id,
				"login":       login,
				"displayName": display,
			},
			"game": map[string]interface{}{
				"id":   "27546",
				"name": "World of Tanks",
			},
		},
	}
}

func TestParseDirectoryStreams(t *testing.T) {
	resp := directoryResponse([]interface{}{
		directoryEdge("111", "tanker_one", "Tanker_One", 5400),
		directoryEdge("222", "tanker_two", "Tanker_Two", 130),
	})

	streams := parseDirectoryStreams(resp)
	if len(streams) != 2 {
		t.Fatalf("expected 2 streams, got %d", len(streams))
	}

	first := streams[0]
	if first.ChannelID != "111" || first.Login != "tanker_one" || first.DisplayName != "Tanker_One" {
		t.Errorf("unexpected broadcaster fields: %+v", first)
	}
	if first.Viewers != 5400 {
		t.Errorf("expected 5400 viewers, got %d", first.Viewers)
	}
	if first.GameID != "27546" || first.GameName != "World of Tanks" {
		t.Errorf("unexpected game fields: %+v", first)
	}
	if !first.DropsEnabled {
		t.Error("expected DropsEnabled for directory streams returned under the DROPS_ENABLED filter")
	}
}

func TestParseDirectoryStreamsSkipsIncompleteNodes(t *testing.T) {
	edges := []interface{}{
		directoryEdge("111", "valid_channel", "Valid", 10),
		// Broadcaster missing entirely.
		map[string]interface{}{"node": map[string]interface{}{"viewersCount": 50.0}},
		// Broadcaster present but login empty.
		map[string]interface{}{"node": map[string]interface{}{
			"broadcaster": map[string]interface{}{"id": "333", "login": ""},
		}},
		// Not a map at all.
		"garbage",
	}

	streams := parseDirectoryStreams(directoryResponse(edges))
	if len(streams) != 1 || streams[0].Login != "valid_channel" {
		t.Errorf("expected only the valid channel to survive, got %+v", streams)
	}
}

func TestParseDirectoryStreamsHandlesMalformedResponses(t *testing.T) {
	cases := []map[string]interface{}{
		nil,
		{},
		{"data": map[string]interface{}{}},
		{"data": map[string]interface{}{"game": nil}},
		{"data": map[string]interface{}{"game": map[string]interface{}{}}},
		{"data": map[string]interface{}{"game": map[string]interface{}{"streams": map[string]interface{}{}}}},
	}
	for i, resp := range cases {
		if got := parseDirectoryStreams(resp); len(got) != 0 {
			t.Errorf("case %d: expected no streams for malformed response, got %+v", i, got)
		}
	}
}

func TestSlugifyGameName(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"World of Tanks", "world-of-tanks"},
		{"Tom Clancy's Rainbow Six Siege", "tom-clancys-rainbow-six-siege"},
		{"  Rust ", "rust"},
		{"Counter-Strike 2", "counter-strike-2"},
		{"PUBG: BATTLEGROUNDS", "pubg-battlegrounds"},
		// Non-apostrophe punctuation becomes hyphens (DevilXD's algorithm),
		// matching e.g. twitch.tv/directory/category/warhammer-40-000-space-marine-2
		{"Warhammer 40,000: Space Marine 2", "warhammer-40-000-space-marine-2"},
		{"", ""},
	}
	for _, c := range cases {
		if got := SlugifyGameName(c.name); got != c.want {
			t.Errorf("SlugifyGameName(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}
