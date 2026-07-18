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
		// directory, data.game present) from failure modes, so callers can
		// keep their previous channel pool instead of clearing it:
		// - GQL errors (e.g. a rotated persisted-query hash);
		// - data.game explicitly null, meaning the slug didn't resolve — in
		//   that case also drop the cached slug so the next sync re-resolves
		//   instead of failing forever on a stale/guessed value.
		if errs, ok := resp["errors"].([]interface{}); ok && len(errs) > 0 {
			return nil, fmt.Errorf("directory query for %q returned GQL errors: %v", gameName, errs)
		}
		if data, ok := resp["data"].(map[string]interface{}); ok {
			if g, present := data["game"]; present && g == nil {
				c.invalidateGameSlug(gameName)
				return nil, fmt.Errorf("directory slug %q for game %q did not resolve", slug, gameName)
			}
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

// invalidateGameSlug drops a cached slug that turned out not to resolve, so
// the next lookup goes back through the GQL slug redirect.
func (c *TwitchClient) invalidateGameSlug(gameName string) {
	key := strings.ToLower(strings.TrimSpace(gameName))
	c.slugMu.Lock()
	delete(c.gameSlugs, key)
	c.slugMu.Unlock()
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

// GameIdentity is a game's stable Twitch identity as returned by the
// directory-redirect lookup: the opaque game ID plus its directory slug. The ID
// is an opaque string — callers must not lowercase, regex-check, or otherwise
// transform it.
type GameIdentity struct {
	ID   string
	Slug string
}

// GetGameIdentity resolves a game's display name to its Twitch identity (opaque
// game ID + directory slug) via the same DirectoryGameRedirect operation the
// site uses for /directory/game/<name> URLs — the one candidate-independent way
// to obtain a game ID, so it works even when no campaign for that game is live.
// The input name is TrimSpace'd; a blank name makes no network call. A game
// Twitch does not know returns a zero GameIdentity and no error; GQL/transport
// errors are returned as errors (kept distinct from "unknown game"). The ID and
// slug are taken verbatim from the response — no derivation, no fallback.
func (c *TwitchClient) GetGameIdentity(gameName string) (GameIdentity, error) {
	name := strings.TrimSpace(gameName)
	if name == "" {
		return GameIdentity{}, nil
	}

	op := constants.DirectoryGameRedirect.WithVariables(map[string]interface{}{
		"name": name,
	})

	resp, err := c.postGQLRequest(op)
	if err != nil {
		return GameIdentity{}, err
	}

	// A non-empty top-level errors array is a lookup FAILURE (e.g. a rotated
	// persisted-query hash), checked before data so a response that carries both
	// errors and a null game surfaces the error rather than being mistaken for
	// "Twitch doesn't know this game" (which is data.game == null with no errors).
	if errs, ok := resp["errors"].([]interface{}); ok && len(errs) > 0 {
		return GameIdentity{}, fmt.Errorf("game identity lookup for %q returned GQL errors: %v", name, errs)
	}

	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		return GameIdentity{}, nil
	}
	game, ok := data["game"].(map[string]interface{})
	if !ok || game == nil {
		return GameIdentity{}, nil
	}
	id, _ := game["id"].(string)
	slug, _ := game["slug"].(string)
	return GameIdentity{ID: id, Slug: slug}, nil
}

// GetGameSlug resolves a game's display name to its directory slug via the same
// GQL operation twitch.tv uses when redirecting /directory/game/<name> URLs.
// Returns "" (no error) when Twitch doesn't know the game. It reuses
// GetGameIdentity so the single GQL response is parsed one way (discovery reads
// the slug; the Settings game-ID lookup reads the ID).
func (c *TwitchClient) GetGameSlug(gameName string) (string, error) {
	identity, err := c.GetGameIdentity(gameName)
	if err != nil {
		return "", err
	}
	return identity.Slug, nil
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
