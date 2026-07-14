package web

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
)

// followedChannelView is one row in the import picker.
type followedChannelView struct {
	Login          string `json:"login"`
	DisplayName    string `json:"displayName"`
	AlreadyTracked bool   `json:"alreadyTracked"`
}

// followedResponse is the GET /api/followed payload. Truncated is true when the
// followed list was capped (Cap) while Twitch reported more, so the UI can say
// "showing first N of more" instead of silently cutting the list.
type followedResponse struct {
	Channels  []followedChannelView `json:"channels"`
	Truncated bool                  `json:"truncated"`
	Cap       int                   `json:"cap"`
}

// handleAPIFollowed returns the authenticated user's followed channels, each
// flagged with whether it is already in the tracked streamer list.
func (s *Server) handleAPIFollowed(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeNotAllowed(w)
		return
	}

	s.mu.RLock()
	provider := s.followedProvider
	s.mu.RUnlock()
	if provider == nil {
		writeServiceUnavailable(w, "Followed-channel import is not available")
		return
	}

	channels, truncated, err := provider.FollowedChannels()
	if err != nil {
		slog.Error("Failed to fetch followed channels", "error", err)
		writeInternalError(w, "Failed to fetch followed channels from Twitch")
		return
	}

	tracked := make(map[string]bool)
	for _, u := range provider.TrackedUsernames() {
		tracked[strings.ToLower(strings.TrimSpace(u))] = true
	}

	views := make([]followedChannelView, 0, len(channels))
	for _, c := range channels {
		login := strings.ToLower(strings.TrimSpace(c.Login))
		if login == "" {
			continue
		}
		name := c.DisplayName
		if name == "" {
			name = c.Login
		}
		views = append(views, followedChannelView{
			Login:          login,
			DisplayName:    name,
			AlreadyTracked: tracked[login],
		})
	}
	// Not-yet-tracked first, then alphabetical, so the actionable rows lead.
	sort.SliceStable(views, func(i, j int) bool {
		if views[i].AlreadyTracked != views[j].AlreadyTracked {
			return !views[i].AlreadyTracked
		}
		return views[i].Login < views[j].Login
	})

	writeJSONOK(w, followedResponse{Channels: views, Truncated: truncated, Cap: maxFollowedFetch})
}

// maxFollowedFetch mirrors the API layer's cap so the response can report it to
// the UI without importing the api package here.
const maxFollowedFetch = 1000

// followedImportRequest is the POST /api/followed/import body.
type followedImportRequest struct {
	Logins []string `json:"logins"`
}

// handleAPIFollowedImport adds the selected followed channels to the tracked
// streamer list (default settings), skipping any already tracked, and persists.
func (s *Server) handleAPIFollowedImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeNotAllowed(w)
		return
	}

	s.mu.RLock()
	provider := s.followedProvider
	s.mu.RUnlock()
	if provider == nil {
		writeServiceUnavailable(w, "Followed-channel import is not available")
		return
	}

	var req followedImportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeBadRequest(w, "Invalid JSON: "+err.Error())
		return
	}
	if len(req.Logins) == 0 {
		writeBadRequest(w, "no channels selected")
		return
	}

	added, err := provider.ImportStreamers(req.Logins)
	if err != nil {
		slog.Error("Failed to import followed channels", "error", err)
		writeInternalError(w, "Failed to import channels")
		return
	}

	slog.Info("Imported followed channels into the tracked list", "requested", len(req.Logins), "added", added)
	writeJSONOK(w, map[string]interface{}{
		"success": true,
		"added":   added,
		"message": importMessage(added, len(req.Logins)),
	})
}

func importMessage(added, requested int) string {
	switch added {
	case 0:
		return "No new channels added (all selected were already tracked)."
	case 1:
		return "Added 1 channel to the tracked list."
	default:
		return fmt.Sprintf("Added %d channels to the tracked list.", added)
	}
}
