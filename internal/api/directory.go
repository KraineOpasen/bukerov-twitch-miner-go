package api

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
)

// DirectoryStream is one live channel returned by a game-directory listing.
type DirectoryStream struct {
	ChannelID   string
	Login       string
	DisplayName string
	Title       string
	Viewers     int
	GameID      string
	GameName    string
	// DropsEnabled reports that Twitch returned this stream under the
	// DROPS_ENABLED directory filter.
	DropsEnabled bool
}

// GetDirectoryStreams lists live channels from a game's Twitch directory
// (twitch.tv/directory/category/<slug>), restricted to drops-enabled streams
// and sorted by viewer count, exactly like reference drop miners populate
// their channel pools. gameName is the display name as configured by the user
// (e.g. "World of Tanks"); it is resolved to the directory slug via GQL with
// a local slugify fallback.
func (c *TwitchClient) GetDirectoryStreams(gameName string, limit int) ([]DirectoryStream, error) {
	slug := c.resolveGameSlug(gameName)
	if slug == "" {
		return nil, fmt.Errorf("cannot resolve directory slug for game %q", gameName)
	}

	// Variable shape mirrors DevilXD/TwitchDropsMiner's GameDirectory
	// operation for the current hash era (includeCostreaming), with
	// VIEWER_COUNT sort so the top channel is the most-viewed one. The
	// requestID value is a magic constant copied verbatim from Twitch's own
	// client traffic by every reference implementation.
	op := constants.DirectoryPageGame.WithVariables(map[string]interface{}{
		"limit":              limit,
		"slug":               slug,
		"imageWidth":         50,
		"includeCostreaming": false,
		"options": map[string]interface{}{
			"broadcasterLanguages":   []interface{}{},
			"freeformTags":           nil,
			"includeRestricted":      []string{"SUB_ONLY_LIVE"},
			"recommendationsContext": map[string]interface{}{"platform": "web"},
			"sort":                   "VIEWER_COUNT",
			"tags":                   []interface{}{},
			"systemFilters":          []string{"DROPS_ENABLED"},
			"requestID":              "JIRA-VXP-2397",
		},
		"sortTypeIsRecency": false,
	})

	resp, err := c.postGQLRequest(op)
	if err != nil {
		return nil, err
	}

	streams := parseDirectoryStreams(resp)
	if len(streams) == 0 {
		// Distinguish "no live drops-enabled channels" (a legitimate empty
		// directory, data.game present) from a failed query (GQL errors,
		// e.g. a rotated persisted-query hash), so callers can keep their
		// previous channel pool instead of clearing it on a transient error.
		if errs, ok := resp["errors"].([]interface{}); ok && len(errs) > 0 {
			return nil, fmt.Errorf("directory query for %q returned GQL errors: %v", gameName, errs)
		}
	}
	return streams, nil
}

// resolveGameSlug resolves a game display name to its directory slug,
// preferring the GQL lookup (cached per game — slugs are stable) and falling
// back to local slugification. Fallback results are deliberately not cached
// so a later successful lookup can correct them.
func (c *TwitchClient) resolveGameSlug(gameName string) string {
	key := strings.ToLower(strings.TrimSpace(gameName))
	if key == "" {
		return ""
	}

	c.slugMu.Lock()
	slug, ok := c.gameSlugs[key]
	c.slugMu.Unlock()
	if ok {
		return slug
	}

	slug, err := c.GetGameSlug(gameName)
	if err != nil || slug == "" {
		return SlugifyGameName(gameName)
	}

	c.slugMu.Lock()
	if c.gameSlugs == nil {
		c.gameSlugs = make(map[string]string)
	}
	c.gameSlugs[key] = slug
	c.slugMu.Unlock()
	return slug
}

// parseDirectoryStreams walks a DirectoryPage_Game response down to
// data.game.streams.edges[].node, tolerating any missing level (an unexpected
// shape yields an empty list, never a panic). Kept separate from the request
// so the parsing can be unit-tested against a canned response.
func parseDirectoryStreams(resp map[string]interface{}) []DirectoryStream {
	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		return nil
	}
	game, ok := data["game"].(map[string]interface{})
	if !ok || game == nil {
		return nil
	}

	gameID, _ := game["id"].(string)
	gameName, _ := game["name"].(string)
	if dn, ok := game["displayName"].(string); ok && gameName == "" {
		gameName = dn
	}

	streams, ok := game["streams"].(map[string]interface{})
	if !ok || streams == nil {
		return nil
	}
	edges, ok := streams["edges"].([]interface{})
	if !ok {
		return nil
	}

	var result []DirectoryStream
	for _, e := range edges {
		edge, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		node, ok := edge["node"].(map[string]interface{})
		if !ok || node == nil {
			continue
		}

		ds := DirectoryStream{
			GameID:   gameID,
			GameName: gameName,
			// The query itself filters on DROPS_ENABLED, so every returned
			// stream carries drops.
			DropsEnabled: true,
		}

		if broadcaster, ok := node["broadcaster"].(map[string]interface{}); ok && broadcaster != nil {
			ds.ChannelID, _ = broadcaster["id"].(string)
			ds.Login, _ = broadcaster["login"].(string)
			ds.DisplayName, _ = broadcaster["displayName"].(string)
		}
		ds.Title, _ = node["title"].(string)
		if v, ok := node["viewersCount"].(float64); ok {
			ds.Viewers = int(v)
		}
		// Prefer the per-stream game if present (defensive: a stream can lag
		// behind a category change).
		if g, ok := node["game"].(map[string]interface{}); ok && g != nil {
			if id, ok := g["id"].(string); ok && id != "" {
				ds.GameID = id
			}
			if name, ok := g["name"].(string); ok && name != "" {
				ds.GameName = name
			}
		}

		if ds.Login == "" || ds.ChannelID == "" {
			continue
		}
		result = append(result, ds)
	}

	return result
}

// GetGameSlug resolves a game's display name to its directory slug via the
// same GQL operation twitch.tv uses when redirecting /directory/game/<name>
// URLs. Returns "" (no error) when Twitch doesn't know the game.
func (c *TwitchClient) GetGameSlug(gameName string) (string, error) {
	op := constants.DirectoryGameRedirect.WithVariables(map[string]interface{}{
		"name": gameName,
	})

	resp, err := c.postGQLRequest(op)
	if err != nil {
		return "", err
	}

	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		return "", nil
	}
	game, ok := data["game"].(map[string]interface{})
	if !ok || game == nil {
		return "", nil
	}
	slug, _ := game["slug"].(string)
	return slug, nil
}

var slugNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// SlugifyGameName approximates Twitch's directory slug for a game name using
// the same algorithm as DevilXD/TwitchDropsMiner's Game.slug: apostrophes are
// removed outright, every other non-alphanumeric run becomes a single hyphen:
// "World of Tanks" -> "world-of-tanks", "Tom Clancy's Rainbow Six Siege" ->
// "tom-clancys-rainbow-six-siege". Only a fallback for when the slug-redirect
// lookup fails — rare name-collision categories have numeric-suffixed
// canonical slugs this cannot reproduce.
func SlugifyGameName(name string) string {
	slug := strings.ToLower(strings.TrimSpace(name))
	slug = strings.ReplaceAll(slug, "'", "")
	slug = slugNonAlnum.ReplaceAllString(slug, "-")
	return strings.Trim(slug, "-")
}
