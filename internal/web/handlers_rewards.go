package web

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// RewardView is the JSON shape of a custom channel-points reward sent to the
// streamer-page Rewards modal.
type RewardView struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	Prompt          string `json:"prompt,omitempty"`
	Cost            int    `json:"cost"`
	RequiresInput   bool   `json:"requiresInput"`
	Available       bool   `json:"available"`
	Unavailable     string `json:"unavailableReason,omitempty"`
	SubOnly         bool   `json:"subOnly"`
	ImageURL        string `json:"imageUrl,omitempty"`
	BackgroundColor string `json:"backgroundColor,omitempty"`
}

// rewardsResponse is the payload for GET /api/streamer/{name}/rewards: the
// reward list, the streamer's current auto-redeem configuration, and the
// current points balance so the modal can render everything in one round trip.
type rewardsResponse struct {
	Rewards    []RewardView         `json:"rewards"`
	AutoRedeem autoRedeemConfigView `json:"autoRedeem"`
	Balance    int                  `json:"balance"`
}

type autoRedeemConfigView struct {
	Enabled   bool     `json:"enabled"`
	Budget    int      `json:"budget"`
	RewardIDs []string `json:"rewardIds"`
}

type redeemRequest struct {
	RewardID  string `json:"rewardId"`
	TextInput string `json:"textInput"`
}

// handleAPIStreamerRewards dispatches the /api/streamer/{name}/{action} routes
// for custom-reward listing, redemption, and auto-redeem configuration.
func (s *Server) handleAPIStreamerRewards(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	provider := s.rewardsProvider
	s.mu.RUnlock()

	if provider == nil {
		writeServiceUnavailable(w, "Rewards are not available yet")
		return
	}

	rest := strings.TrimPrefix(r.URL.Path, "/api/streamer/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	name := strings.ToLower(parts[0])
	action := parts[1]

	switch action {
	case "rewards":
		s.handleListRewards(w, r, provider, name)
	case "redeem":
		s.handleRedeemReward(w, r, provider, name)
	case "auto-redeem":
		s.handleAutoRedeem(w, r, provider, name)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleListRewards(w http.ResponseWriter, r *http.Request, provider RewardsProvider, name string) {
	if r.Method != http.MethodGet {
		writeNotAllowed(w)
		return
	}

	rewards, err := provider.ListCustomRewards(name)
	if err != nil {
		// A not-tracked streamer or a transient fetch failure both surface as
		// an empty list with a 200 so the modal shows a friendly empty state
		// rather than an error page.
		writeJSONOK(w, rewardsResponse{
			Rewards:    []RewardView{},
			AutoRedeem: autoRedeemView(provider.GetAutoRedeem(name)),
			Balance:    s.streamerBalance(name),
		})
		return
	}

	writeJSONOK(w, rewardsResponse{
		Rewards:    buildRewardViews(rewards),
		AutoRedeem: autoRedeemView(provider.GetAutoRedeem(name)),
		Balance:    s.streamerBalance(name),
	})
}

// streamerBalance returns the current cached channel-points balance for a
// tracked streamer, or 0 when it isn't tracked. ListCustomRewards refreshes
// this value from Twitch just before this is read, so it reflects the latest
// balance.
func (s *Server) streamerBalance(name string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, st := range s.streamers {
		if st.GetUsername() == name {
			return st.GetChannelPoints()
		}
	}
	return 0
}

func (s *Server) handleRedeemReward(w http.ResponseWriter, r *http.Request, provider RewardsProvider, name string) {
	if r.Method != http.MethodPost {
		writeNotAllowed(w)
		return
	}

	var req redeemRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeBadRequest(w, "Invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.RewardID) == "" {
		writeBadRequest(w, "rewardId is required")
		return
	}

	if err := provider.RedeemCustomReward(name, req.RewardID, req.TextInput); err != nil {
		// A failed redemption is an expected, user-facing outcome (reward gone,
		// not enough points, cooldown, ...) so it is reported as a 200 with
		// success:false rather than an HTTP error.
		writeJSONOK(w, map[string]interface{}{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	writeJSONOK(w, map[string]interface{}{
		"success": true,
		"message": "Reward redeemed",
	})
}

func (s *Server) handleAutoRedeem(w http.ResponseWriter, r *http.Request, provider RewardsProvider, name string) {
	if r.Method != http.MethodPost {
		writeNotAllowed(w)
		return
	}

	var view autoRedeemConfigView
	if err := json.NewDecoder(r.Body).Decode(&view); err != nil {
		writeBadRequest(w, "Invalid JSON: "+err.Error())
		return
	}

	if view.Budget < 0 {
		view.Budget = 0
	}

	cfg := config.AutoRedeemConfig{
		Enabled:   view.Enabled,
		Budget:    view.Budget,
		RewardIDs: view.RewardIDs,
	}
	if err := provider.SetAutoRedeem(name, cfg); err != nil {
		writeBadRequest(w, err.Error())
		return
	}

	writeSuccess(w)
}

func buildRewardViews(rewards []*models.CustomReward) []RewardView {
	views := make([]RewardView, 0, len(rewards))
	for _, r := range rewards {
		view := RewardView{
			ID:              r.ID,
			Title:           r.Title,
			Prompt:          r.Prompt,
			Cost:            r.Cost,
			RequiresInput:   r.IsUserInputRequired,
			Available:       r.IsAvailable(),
			SubOnly:         r.IsSubOnly,
			ImageURL:        r.ImageURL,
			BackgroundColor: r.BackgroundColor,
		}
		if !view.Available {
			view.Unavailable = rewardUnavailableReason(r)
		}
		views = append(views, view)
	}
	return views
}

// rewardUnavailableReason returns a short human label explaining why a reward
// cannot be redeemed right now, for the modal's status pill.
func rewardUnavailableReason(r *models.CustomReward) string {
	switch {
	case !r.IsEnabled:
		return "Disabled"
	case r.IsPaused:
		return "Paused"
	case !r.IsInStock:
		return "Out of stock"
	case !r.CooldownExpiresAt.IsZero():
		return "On cooldown"
	default:
		return "Unavailable"
	}
}

func autoRedeemView(cfg config.AutoRedeemConfig) autoRedeemConfigView {
	ids := cfg.RewardIDs
	if ids == nil {
		ids = []string{}
	}
	return autoRedeemConfigView{
		Enabled:   cfg.Enabled,
		Budget:    cfg.Budget,
		RewardIDs: ids,
	}
}
