package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version"
)

// applyErrorMessage is the ONE generic body returned for every settings-apply
// failure — no raw SQLite/filesystem/context detail ever reaches the client
// (the caller's own slog.Error, in internal/miner, already carries that
// detail server-side). It also doubles as the truthful invariant this whole
// pass exists to guarantee: a fail-closed apply's error return means nothing
// was mutated.
const applyErrorMessage = "Settings could not be applied; no changes were made"

// writeApplyError maps a settings-apply error to a safe HTTP status: 503 for
// a shutdown/draining miner or an already-closed database (both mean "retry
// is safe, nothing changed"), 500 for everything else (a rejected/failed
// durable admission or persistence step for THIS attempt — also safe to
// retry, since a fail-closed apply never mutates on error, but not a
// transient condition the caller can wait out).
func writeApplyError(w http.ResponseWriter, err error) {
	if errors.Is(err, settings.ErrShuttingDown) || errors.Is(err, database.ErrClosed) {
		writeServiceUnavailable(w, applyErrorMessage)
		return
	}
	writeInternalError(w, applyErrorMessage)
}

func (s *Server) handleSettingsPage(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	refresh := s.refresh
	discordEnabled := s.discordEnabled
	debugURL := s.debugURL
	s.mu.RUnlock()

	data := SettingsPageData{
		Username:       s.username,
		RefreshMinutes: refresh,
		Version:        version.Version,
		DiscordEnabled: discordEnabled,
		DebugURL:       debugURL,
	}
	s.renderPage(w, r, "settings.html", data)
}

func (s *Server) handleAPISettings(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	provider := s.settingsProvider
	callback := s.onSettingsUpdate
	s.mu.RUnlock()

	if r.Method == http.MethodGet {
		if provider == nil {
			writeServiceUnavailable(w, "Settings not available")
			return
		}
		currentSettings := provider.GetRuntimeSettings()
		writeJSONOK(w, currentSettings)
		return
	}

	if r.Method == http.MethodPost {
		if provider == nil {
			writeServiceUnavailable(w, "Settings not available")
			return
		}
		// A nil callback used to mean "silently do nothing, then report 200"
		// (G22/G23) — refuse instead, so a client can never be told its
		// change applied when it never even reached the apply pipeline.
		if callback == nil {
			writeServiceUnavailable(w, "Settings not available")
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeBadRequest(w, "Failed to read request body: "+err.Error())
			return
		}

		// Decode ONTO the current settings, not onto a zero value: the apply
		// path (settings.ApplyToConfig) replaces the config wholesale, so a
		// partial body used to zero every omitted field — dropping all
		// streamers when "streamers" was absent, resetting logger/analytics
		// blocks, and letting ValidateConfig silently clamp zeroed intervals.
		// Seeding gives merge semantics: an absent key keeps its current
		// value, a present key (including an explicit empty list) replaces it.
		newSettings := provider.GetRuntimeSettings()

		// One exception: a slice of structs must not be decoded "over" its
		// seeded elements. encoding/json resets the slice length to zero and
		// appends, which reuses the retained backing array — a posted element
		// would then inherit leftover fields (including the per-streamer
		// Settings pointer) from whatever previously sat at its index. Clear
		// the seed and restore it only when the key was genuinely absent.
		seededStreamers := newSettings.Streamers
		newSettings.Streamers = nil

		if err := json.Unmarshal(body, &newSettings); err != nil {
			writeBadRequest(w, "Invalid JSON: "+err.Error())
			return
		}

		// The struct unmarshal above succeeded, so the body is a JSON object
		// and this probe cannot fail.
		var probe map[string]json.RawMessage
		_ = json.Unmarshal(body, &probe)
		if _, present := probe["streamers"]; !present {
			newSettings.Streamers = seededStreamers
		}

		// Fail-closed: on error nothing below runs — no cache update, no
		// success body. The apply itself already guarantees nothing was
		// mutated on a non-nil error (see settings.SettingsUpdateCallback).
		if err := callback(r.Context(), newSettings); err != nil {
			writeApplyError(w, err)
			return
		}

		s.mu.Lock()
		s.refresh = newSettings.Analytics.Refresh
		s.daysAgo = newSettings.Analytics.DaysAgo
		s.mu.Unlock()

		writeSuccess(w)
		return
	}

	writeNotAllowed(w)
}

func (s *Server) handleAPISettingsReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeNotAllowed(w)
		return
	}

	s.mu.RLock()
	provider := s.settingsProvider
	callback := s.onSettingsUpdate
	s.mu.RUnlock()

	if provider == nil {
		writeServiceUnavailable(w, "Settings not available")
		return
	}
	if callback == nil {
		writeServiceUnavailable(w, "Settings not available")
		return
	}

	defaults := provider.GetDefaultSettings()

	// Fail-closed: on error, do NOT update the cache and do NOT return the
	// defaults as if they were applied (the previous behavior echoed them
	// back unconditionally, which would lie about what actually happened).
	if err := callback(r.Context(), defaults); err != nil {
		writeApplyError(w, err)
		return
	}

	s.mu.Lock()
	s.refresh = defaults.Analytics.Refresh
	s.daysAgo = defaults.Analytics.DaysAgo
	s.mu.Unlock()

	writeJSONOK(w, defaults)
}
