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
func policyStatusDisplay(status policy.FeasStatus, tr func(string) string) (label, color string) {
	switch status {
	case policy.StatusSafe:
		return tr("drops.policy.status.safe"), "#22c55e"
	case policy.StatusAtRisk:
		return tr("drops.policy.status.at_risk"), "#f59e0b"
	case policy.StatusNextRewardOnly:
		return tr("drops.policy.status.next_reward_only"), "#f59e0b"
	case policy.StatusImpossible:
		return tr("drops.policy.status.impossible"), "#ef4444"
	default:
		return string(status), "#a1a1aa"
	}
}

// buildDropPolicyByCampaign turns the published policy decisions + current
// per-drop rules into per-campaign badge views, keyed by campaign ID.
func buildDropPolicyByCampaign(campaigns []*models.Campaign, decisions []policy.Decision, rules map[string]config.DropRule, tr func(string) string) map[string]*DropPolicyView {
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
		label, color := policyStatusDisplay(d.Status, tr)
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
	// Paused/stopped/in-transition is a lifecycle CONFLICT, not a transient
	// unavailability: the operator can resolve it by resuming, and this repo
	// already answers it with a friendly localized 409 on the settings routes.
	// Reporting it as the fence's 503 would be truthful about the outcome but
	// wrong about the cause. This check is UX sugar exactly as it is on those
	// routes — the fence inside the miner remains the authoritative backstop
	// for the unavoidable race between checking here and mutating there.
	if s.lifecycleMutationBlocked() {
		s.writeSettingsConflict(w, r)
		return
	}
	s.mu.RLock()
	provider := s.policyProvider
	s.mu.RUnlock()
	if provider != nil {
		// Fail-closed: on error, no re-render. renderDropsList re-samples the
		// provider and would otherwise paint a 200 success for a change that
		// never happened — including one refused because the generation
		// backing this provider has been retired.
		//
		// The provider == nil case (no generation has reached setupComponents
		// yet) deliberately keeps its existing behaviour of rendering the
		// partial: that is a separate, pre-existing condition with its own
		// product decision, pinned by an existing test on the sibling health
		// route, and not the stale-generation defect this fence closes.
		if err := provider.ApplyCampaignPolicy(r.FormValue("mode")); err != nil {
			s.writePolicyMutationError(w, err)
			return
		}
	}
	s.renderDropsList(w, r)
}

// policyErrorMessage is the one generic body for a refused Drops-page control
// change, mirroring applyErrorMessage's discipline (no raw internal detail
// reaches the client) while using the Drops page's own words rather than the
// settings pipeline's.
const policyErrorMessage = "Drop policy could not be changed; no changes were made"

// writePolicyMutationError maps a policy mutation failure to a safe status:
// 503 when the target generation is draining/retired (retry is safe, nothing
// changed), 500 otherwise.
func (s *Server) writePolicyMutationError(w http.ResponseWriter, err error) {
	if mutationRefusedAsUnavailable(err) {
		writeServiceUnavailable(w, policyErrorMessage)
		return
	}
	writeInternalError(w, policyErrorMessage)
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
	// Paused/stopped/in-transition is a lifecycle CONFLICT, not a transient
	// unavailability: the operator can resolve it by resuming, and this repo
	// already answers it with a friendly localized 409 on the settings routes.
	// Reporting it as the fence's 503 would be truthful about the outcome but
	// wrong about the cause. This check is UX sugar exactly as it is on those
	// routes — the fence inside the miner remains the authoritative backstop
	// for the unavoidable race between checking here and mutating there.
	if s.lifecycleMutationBlocked() {
		s.writeSettingsConflict(w, r)
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
		// Fail-closed, exactly as handleAPIPolicyMode above.
		if err := provider.SetDropRule(key, rule); err != nil {
			s.writePolicyMutationError(w, err)
			return
		}
	}
	s.renderDropsList(w, r)
}

func checked(r *http.Request, name string) bool {
	v := r.FormValue(name)
	return v == "on" || v == "true"
}
