package web

import (
	"net/http"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/policy"
)

// policyStatusDisplay maps a feasibility status to a label and inline color
// (mirroring the Health Center palette).
func policyStatusDisplay(status policy.FeasStatus) (label, color string) {
	switch status {
	case policy.StatusSafe:
		return "SAFE", "#22c55e"
	case policy.StatusAtRisk:
		return "AT RISK", "#f59e0b"
	case policy.StatusNextRewardOnly:
		return "NEXT REWARD ONLY", "#f59e0b"
	case policy.StatusImpossible:
		return "IMPOSSIBLE", "#ef4444"
	default:
		return string(status), "#a1a1aa"
	}
}

// buildDropPolicyByCampaign turns the published policy decisions + current
// per-drop rules into per-campaign badge views, keyed by campaign ID.
func buildDropPolicyByCampaign(campaigns []*models.Campaign, decisions []policy.Decision, rules map[string]config.DropRule) map[string]*DropPolicyView {
	if len(decisions) == 0 {
		return nil
	}
	byID := make(map[string]policy.Decision, len(decisions))
	for _, d := range decisions {
		byID[d.CampaignID] = d
	}

	out := make(map[string]*DropPolicyView, len(campaigns))
	for _, c := range campaigns {
		d, ok := byID[c.ID]
		if !ok {
			continue
		}
		label, color := policyStatusDisplay(d.Status)
		v := &DropPolicyView{
			Status:                string(d.Status),
			StatusColor:           color,
			StatusLabel:           label,
			Total:                 d.Total,
			Excluded:              d.Excluded,
			ExcludeReason:         d.ExcludeReason,
			TimeUntilEnd:          d.Feasibility.TimeUntilEnd.Round(time.Minute).String(),
			MinutesToNextReward:   d.Feasibility.MinutesToNextReward,
			CanCompleteNextReward: d.Feasibility.CanCompleteNextReward,
			CanCompleteAll:        d.Feasibility.CanCompleteAll,
		}
		for _, f := range d.Factors {
			v.Factors = append(v.Factors, PolicyFactorView{Points: f.Points, Label: f.Label})
		}

		// Per-drop controls target the campaign's current drop's reward key.
		if drop := c.CurrentDrop(); drop != nil {
			gameID := ""
			if c.Game != nil {
				gameID = c.Game.ID
			}
			v.RewardKey = models.NormalizeRewardKey(gameID, drop.Name)
			v.SubscriberOnlyKnown = drop.SubscriberOnlyKnown
			if r, ok := rules[v.RewardKey]; ok {
				v.Skip = r.Skip
				v.HighPriority = r.HighPriority
				v.AlwaysFinishStarted = r.AlwaysFinishStarted
				v.NextRewardOnly = r.NextRewardOnly
				v.IgnoreSubscriberOnly = r.IgnoreSubscriberOnly
			}
		}
		out[c.ID] = v
	}
	return out
}

// handleAPIPolicyMode applies a new campaign-policy mode and re-renders the
// campaign queue.
func (s *Server) handleAPIPolicyMode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		writeInternalError(w, "invalid form")
		return
	}
	s.mu.RLock()
	provider := s.policyProvider
	s.mu.RUnlock()
	if provider != nil {
		provider.ApplyCampaignPolicy(r.FormValue("mode"))
	}
	s.renderDropsList(w, r)
}

// handleAPIPolicyDropRule sets or resets the per-drop rule for a reward key and
// re-renders the campaign queue. A "reset" form value (or all-unchecked) clears
// the rule.
func (s *Server) handleAPIPolicyDropRule(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		writeInternalError(w, "invalid form")
		return
	}
	s.mu.RLock()
	provider := s.policyProvider
	s.mu.RUnlock()

	key := r.FormValue("rewardKey")
	if provider != nil && key != "" {
		var rule config.DropRule // "reset" → zero value clears
		if r.FormValue("reset") == "" {
			rule = config.DropRule{
				Skip:                 checked(r, "skip"),
				HighPriority:         checked(r, "highPriority"),
				AlwaysFinishStarted:  checked(r, "alwaysFinishStarted"),
				NextRewardOnly:       checked(r, "nextRewardOnly"),
				IgnoreSubscriberOnly: checked(r, "ignoreSubscriberOnly"),
			}
		}
		provider.SetDropRule(key, rule)
	}
	s.renderDropsList(w, r)
}

func checked(r *http.Request, name string) bool {
	v := r.FormValue(name)
	return v == "on" || v == "true"
}
