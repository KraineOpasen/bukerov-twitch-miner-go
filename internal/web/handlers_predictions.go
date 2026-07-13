package web

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
)

// manualBetRequest is the body of POST /api/prediction/bet. Amount is a plain
// integer of channel points; the outcome and round are identified by their
// stable ids so a stale board can be rejected server-side.
type manualBetRequest struct {
	EventID   string `json:"eventId"`
	OutcomeID string `json:"outcomeId"`
	Amount    int    `json:"amount"`
}

// skipRequest is the body of POST /api/prediction/skip. Skip=true suppresses
// auto-bet for this one round; skip=false undoes it while still open.
type skipRequest struct {
	EventID string `json:"eventId"`
	Skip    bool   `json:"skip"`
}

// handleAPIPredictionBet places a manual bet on a live prediction round. The
// backend re-verifies everything (round exists, still open, outcome belongs to
// the round, amount valid, balance sufficient, no existing bet, not already
// in-flight) before touching Twitch — the request body is never trusted.
//
// Structural problems with the request (wrong method, malformed JSON, missing
// ids) are hard 4xx errors. Everything a user can legitimately hit — round
// closed, not enough points, auto-bet got there first, Twitch rejected — is a
// 200 with {success:false, message:"…"} carrying a human-readable reason, so
// the card shows a clear inline message rather than an error page. This mirrors
// the custom-reward redeem endpoint exactly.
func (s *Server) handleAPIPredictionBet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeNotAllowed(w)
		return
	}

	s.mu.RLock()
	provider := s.predictionControl
	s.mu.RUnlock()
	if provider == nil {
		writeServiceUnavailable(w, "Manual betting is not available yet")
		return
	}

	var req manualBetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeBadRequest(w, "Invalid JSON: "+err.Error())
		return
	}
	req.EventID = strings.TrimSpace(req.EventID)
	req.OutcomeID = strings.TrimSpace(req.OutcomeID)
	if req.EventID == "" {
		writeBadRequest(w, "eventId is required")
		return
	}
	if req.OutcomeID == "" {
		writeBadRequest(w, "outcomeId is required")
		return
	}

	outcomeTitle, err := provider.PlaceManualBet(req.EventID, req.OutcomeID, req.Amount)
	if err != nil {
		// Expected, user-facing outcome: report as 200 success:false with the
		// already-friendly message the provider returned.
		writeJSONOK(w, map[string]interface{}{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	slog.Info("Manual bet placed via dashboard", "event", req.EventID, "amount", req.Amount)
	writeJSONOK(w, map[string]interface{}{
		"success": true,
		"message": "Bet placed",
		"outcome": outcomeTitle,
		"amount":  req.Amount,
	})
}

// handleAPIPredictionSkip toggles per-round auto-bet suppression. It never
// changes global or persisted settings — only this one round is affected, and
// the flag is cleared automatically when the round is cleaned up.
func (s *Server) handleAPIPredictionSkip(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeNotAllowed(w)
		return
	}

	s.mu.RLock()
	provider := s.predictionControl
	s.mu.RUnlock()
	if provider == nil {
		writeServiceUnavailable(w, "Manual betting is not available yet")
		return
	}

	var req skipRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeBadRequest(w, "Invalid JSON: "+err.Error())
		return
	}
	req.EventID = strings.TrimSpace(req.EventID)
	if req.EventID == "" {
		writeBadRequest(w, "eventId is required")
		return
	}

	if err := provider.SetAutoBetSkip(req.EventID, req.Skip); err != nil {
		writeJSONOK(w, map[string]interface{}{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	msg := "Auto-bet skipped for this round"
	if !req.Skip {
		msg = "Auto-bet re-enabled for this round"
	}
	writeJSONOK(w, map[string]interface{}{
		"success": true,
		"skip":    req.Skip,
		"message": msg,
	})
}
