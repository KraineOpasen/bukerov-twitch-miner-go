package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/lifecycle"
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

// lifecycleMutationBlocked consults the SAME injected lifecycle controller
// GET /api/lifecycle reads (design v6 §4/OD2, task contract D11): a nil
// controller (no lifecycle wired at all — every pre-Ф4c test, and any build
// without it) means "no guard", exactly today's behavior, so the dozens of
// bare-&Server{} settings tests keep passing unmodified. Blocked whenever
// the miner is paused/stopped (settings mutations there are pointless: the
// generation reading them is torn down) or a lifecycle transition is
// currently in flight (pausing/stopping/restarting/starting-pending) — but
// NOT while running or degraded (the underlying generation is still live,
// or fail-closed apply already refuses safely) and NOT while failed (apply
// already fails closed via the existing 503 backstop). This is UX sugar
// only: writeApplyError's fail-closed ErrShuttingDown->503 path remains the
// authoritative backstop for the unavoidable race between this check and
// the actual apply.
func (s *Server) lifecycleMutationBlocked() bool {
	s.mu.RLock()
	ctrl := s.lifecycleController
	s.mu.RUnlock()
	if ctrl == nil {
		return false
	}
	snap := ctrl.Snapshot()
	if snap.Observed == lifecycle.ObservedPaused || snap.Observed == lifecycle.ObservedStopped {
		return true
	}
	return snap.Transition != lifecycle.TransitionNone
}

// writeSettingsConflict writes the friendly, server-localized 409 body for a
// settings mutation refused because the miner is paused/stopped/mid-transition.
func (s *Server) writeSettingsConflict(w http.ResponseWriter, r *http.Request) {
	writeConflict(w, s.i18n.T(s.langFromRequest(r), "lc.settings_conflict"))
}

// beginSettingsTxn acquires the settings mutation transaction and returns the
// function that releases it. Every settings mutation is a read-modify-write:
// the handler reads the CURRENT settings, merges the posted (usually partial)
// body onto that snapshot, and hands the merged whole to the apply callback,
// which replaces the config wholesale. Read and apply therefore have to be
// one atomic step — two concurrent POSTs that both read before either applies
// merge onto the same stale snapshot, and whichever applies last silently
// reverts the other's change even when the two bodies touch entirely disjoint
// keys. Serializing only the apply is not enough, and neither is the miner's
// own coordinatorMu: the losing read has already happened by the time either
// is reached. This is the same read-modify-write hazard healthFormMu closes
// for the health forms, at the seam that owns the settings equivalent.
//
// Lock order: settingsTxnMu -> s.mu, never the reverse. The transaction holds
// settingsTxnMu across the apply callback, which re-enters this Server
// (AttachStreamers/SetDiscordEnabled) and takes s.mu there; s.mu is never
// held while acquiring settingsTxnMu. No path takes both settingsTxnMu and
// healthFormMu.
//
// The lock is held across the apply's disk and Discord I/O by design — the
// transaction is not atomic otherwise — which is the same cost the miner's
// coordinatorMu already imposes on every apply.
//
// The TryLock fast path exists only so the contended path has somewhere to
// report from: see settingsTxnContended (server.go).
func (s *Server) beginSettingsTxn() func() {
	if !s.settingsTxnMu.TryLock() {
		if s.settingsTxnContended != nil {
			s.settingsTxnContended()
		}
		s.settingsTxnMu.Lock()
	}
	return s.settingsTxnMu.Unlock
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
		if s.lifecycleMutationBlocked() {
			s.writeSettingsConflict(w, r)
			return
		}
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

		// Everything from here to the success bookkeeping is ONE transaction
		// (see beginSettingsTxn). The body read stays outside it: it is
		// client-paced I/O, and a slow sender must not hold every other
		// settings mutation off.
		releaseTxn := s.beginSettingsTxn()
		defer releaseTxn()

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

	if s.lifecycleMutationBlocked() {
		s.writeSettingsConflict(w, r)
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

	// A reset is a settings mutation like any other and shares the same
	// transaction (see beginSettingsTxn): without it, a reset could be
	// interleaved with a partial POST such that the POST's merged snapshot —
	// read before the reset applied — is written straight back over the
	// defaults, leaving the settings neither reset nor as posted.
	releaseTxn := s.beginSettingsTxn()
	defer releaseTxn()

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
