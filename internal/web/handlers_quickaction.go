package web

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
)

// quickActionRequest is the body of a card quick-action. Action is one of
// "cycle-preference" (none → prefer → avoid → none) or "toggle-watch"
// (flip DisableWatch).
type quickActionRequest struct {
	Action string `json:"action"`
}

// handleAPIStreamerQuickAction applies a single per-streamer setting change
// (rotation preference or watch enable/disable) from an Overview card. It
// reuses the exact settings pipeline the full Settings page uses
// (GetRuntimeSettings → onSettingsUpdate), so the change is persisted to
// config.json, applied to the running streamer, and stays in sync with the
// Settings page - no separate state store.
func (s *Server) handleAPIStreamerQuickAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeNotAllowed(w)
		return
	}

	name := strings.TrimPrefix(r.URL.Path, "/api/streamer-action/")
	if name == "" {
		writeBadRequest(w, "Streamer not specified")
		return
	}

	var req quickActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeBadRequest(w, "Invalid JSON: "+err.Error())
		return
	}

	if s.settingsProvider == nil || s.onSettingsUpdate == nil {
		writeServiceUnavailable(w, "Settings not available")
		return
	}

	rt := s.settingsProvider.GetRuntimeSettings()

	idx := -1
	for i := range rt.Streamers {
		if strings.EqualFold(rt.Streamers[i].Username, name) {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeBadRequest(w, "Unknown streamer")
		return
	}

	// Materialise a full override from current effective settings when the
	// streamer was inheriting defaults, so we mutate exactly one field.
	if rt.Streamers[idx].Settings == nil {
		eff := settings.StreamerSettingsToDTO(settings.StreamerSettingsFromDTO(rt.DefaultSettings))
		rt.Streamers[idx].Settings = &eff
	}
	cfg := rt.Streamers[idx].Settings

	var newPreference string
	var newDisableWatch bool

	switch req.Action {
	case "cycle-preference":
		cur := ""
		if cfg.Preference != nil {
			cur = *cfg.Preference
		}
		next := nextPreference(cur)
		cfg.Preference = &next
		newPreference = next
		if cfg.DisableWatch != nil {
			newDisableWatch = *cfg.DisableWatch
		}
	case "toggle-watch":
		cur := false
		if cfg.DisableWatch != nil {
			cur = *cfg.DisableWatch
		}
		next := !cur
		cfg.DisableWatch = &next
		newDisableWatch = next
		if cfg.Preference != nil {
			newPreference = *cfg.Preference
		}
	default:
		writeBadRequest(w, "Unknown action")
		return
	}

	s.onSettingsUpdate(rt)

	writeJSONOK(w, map[string]interface{}{
		"success":      true,
		"preference":   newPreference,
		"disableWatch": newDisableWatch,
	})
}

// nextPreference cycles none → prefer → avoid → none.
func nextPreference(current string) string {
	switch current {
	case "prefer":
		return "avoid"
	case "avoid":
		return ""
	default:
		return "prefer"
	}
}
