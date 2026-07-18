package web

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
)

// GameIDResolver resolves an exact game name to its Twitch identity for the
// Settings "find game ID" helper. Satisfied by *api.TwitchClient. Read-only: it
// performs a single GQL lookup on an explicit operator action and never mutates
// config or runtime settings.
type GameIDResolver interface {
	GetGameIdentity(gameName string) (api.GameIdentity, error)
}

// maxGameNameLookupLen bounds the lookup input so a pasted blob is never sent to
// Twitch as a "game name".
const maxGameNameLookupLen = 200

// handleAPIResolveGameID is the read-only Settings helper that resolves an exact
// game name to its opaque Twitch game ID (and directory slug), so the operator
// can fill in the strict DropCampaignGameIDs field. It is candidate-independent:
// it queries Twitch's directory-redirect operation directly, so it works even
// when no campaign for that game is live — the all-foreign-sync case that the
// strict filter exists for. It never reads or writes config/runtime settings,
// touches the network only on an explicit POST, holds no lock across the call,
// and returns only safe identity data (never tokens/headers/raw GQL payloads).
func (s *Server) handleAPIResolveGameID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeNotAllowed(w)
		return
	}

	// Snapshot the resolver pointer under the lock, then release it before the
	// network call — no mutex is held across the GQL request.
	s.mu.RLock()
	resolver := s.gameIDResolver
	s.mu.RUnlock()
	if resolver == nil {
		writeServiceUnavailable(w, "Game ID lookup not available")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		writeBadRequest(w, "Failed to read request body")
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeBadRequest(w, "Invalid JSON")
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeBadRequest(w, "game name required")
		return
	}
	if len(name) > maxGameNameLookupLen {
		writeBadRequest(w, "game name too long")
		return
	}

	identity, err := resolver.GetGameIdentity(name)
	if err != nil {
		// Transport / GQL / stale-hash failure — kept distinct from "unknown
		// game". Detail is logged server-side only; the client sees a generic
		// message with no internals.
		slog.Warn("Settings game-ID lookup failed", "name", name, "error", err)
		writeError(w, http.StatusBadGateway, "Could not retrieve the Game ID from Twitch")
		return
	}
	if identity.ID == "" {
		// A successful lookup for a game Twitch does not recognize.
		writeJSONOK(w, map[string]any{"name": name, "found": false})
		return
	}
	writeJSONOK(w, map[string]any{
		"name":   name,
		"found":  true,
		"gameId": identity.ID,
		"slug":   identity.Slug,
	})
}
