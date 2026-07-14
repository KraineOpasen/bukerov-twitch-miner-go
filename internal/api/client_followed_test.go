package api

import "testing"

// followsPage builds a ChannelFollows-shaped response for the given logins with
// the given hasNextPage flag. Each edge gets a cursor equal to its login so the
// paginator has something to advance on.
func followsPage(hasNext bool, logins ...string) map[string]interface{} {
	edges := make([]interface{}, 0, len(logins))
	for _, login := range logins {
		edges = append(edges, map[string]interface{}{
			"cursor": login,
			"node": map[string]interface{}{
				"login":       login,
				"displayName": login + "_disp",
			},
		})
	}
	return map[string]interface{}{
		"data": map[string]interface{}{
			"user": map[string]interface{}{
				"follows": map[string]interface{}{
					"edges":    edges,
					"pageInfo": map[string]interface{}{"hasNextPage": hasNext},
				},
			},
		},
	}
}

func TestCollectFollowedChannelsSinglePage(t *testing.T) {
	calls := 0
	got, truncated, err := collectFollowedChannels(func(cursor string) (map[string]interface{}, error) {
		calls++
		return followsPage(false, "Alpha", "Bravo"), nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if truncated {
		t.Errorf("truncated = true, want false")
	}
	if calls != 1 {
		t.Errorf("fetch called %d times, want 1", calls)
	}
	if len(got) != 2 {
		t.Fatalf("got %d channels, want 2: %+v", len(got), got)
	}
	// Logins are lowercased; display names preserved.
	if got[0].Login != "alpha" || got[0].DisplayName != "Alpha_disp" {
		t.Errorf("channel[0] = %+v, want login=alpha display=Alpha_disp", got[0])
	}
}

func TestCollectFollowedChannelsPaginatesByCursor(t *testing.T) {
	// Two pages: the paginator must feed page 1's last cursor back into fetch.
	var seenCursors []string
	got, truncated, err := collectFollowedChannels(func(cursor string) (map[string]interface{}, error) {
		seenCursors = append(seenCursors, cursor)
		if cursor == "" {
			return followsPage(true, "one", "two"), nil
		}
		return followsPage(false, "three"), nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if truncated {
		t.Errorf("truncated = true, want false")
	}
	if len(got) != 3 {
		t.Fatalf("got %d channels, want 3: %+v", len(got), got)
	}
	// First call has empty cursor, second resumes from page 1's last cursor ("two").
	if len(seenCursors) != 2 || seenCursors[0] != "" || seenCursors[1] != "two" {
		t.Errorf("cursors = %v, want [\"\" \"two\"]", seenCursors)
	}
}

func TestCollectFollowedChannelsDedups(t *testing.T) {
	// A login repeated across pages (and cased differently) is counted once.
	got, _, err := collectFollowedChannels(func(cursor string) (map[string]interface{}, error) {
		if cursor == "" {
			return followsPage(true, "Dup", "unique"), nil
		}
		return followsPage(false, "DUP", "other"), nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d channels, want 3 (dup collapsed): %+v", len(got), got)
	}
	seen := map[string]int{}
	for _, c := range got {
		seen[c.Login]++
	}
	if seen["dup"] != 1 {
		t.Errorf("dup appears %d times, want 1", seen["dup"])
	}
}

func TestCollectFollowedChannelsEmpty(t *testing.T) {
	got, truncated, err := collectFollowedChannels(func(cursor string) (map[string]interface{}, error) {
		return followsPage(false), nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if truncated || len(got) != 0 {
		t.Errorf("empty page: got %d channels truncated=%v, want 0/false", len(got), truncated)
	}
}

func TestCollectFollowedChannelsCapTruncates(t *testing.T) {
	// Every page is full and always reports more, so the cap must stop the crawl
	// and report truncated=true. Each page yields fresh logins so dedup does not
	// mask the cap.
	page := 0
	got, truncated, err := collectFollowedChannels(func(cursor string) (map[string]interface{}, error) {
		logins := make([]string, followedPageSize)
		for i := range logins {
			logins[i] = loginAt(page, i)
		}
		page++
		return followsPage(true, logins...), nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !truncated {
		t.Errorf("truncated = false, want true at the cap")
	}
	if len(got) != maxFollowedFetch {
		t.Errorf("got %d channels, want cap %d", len(got), maxFollowedFetch)
	}
}

func TestCollectFollowedChannelsCapExactNoTruncate(t *testing.T) {
	// Exactly the cap's worth of channels with no further pages: full, but not
	// truncated, because nothing was cut.
	pages := maxFollowedFetch / followedPageSize
	page := 0
	got, truncated, err := collectFollowedChannels(func(cursor string) (map[string]interface{}, error) {
		logins := make([]string, followedPageSize)
		for i := range logins {
			logins[i] = loginAt(page, i)
		}
		last := page == pages-1
		page++
		return followsPage(!last, logins...), nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != maxFollowedFetch {
		t.Fatalf("got %d channels, want %d", len(got), maxFollowedFetch)
	}
	if truncated {
		t.Errorf("truncated = true, want false when the cap is hit exactly with no more pages")
	}
}

// loginAt makes a unique login for page p, index i.
func loginAt(p, i int) string {
	return "p" + itoa(p) + "_c" + itoa(i)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
