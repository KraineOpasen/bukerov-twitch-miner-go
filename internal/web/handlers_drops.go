package web

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/drops"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/health"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/policy"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version"
)

func (s *Server) handleDropsPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/drops" {
		http.NotFound(w, r)
		return
	}

	s.mu.RLock()
	refresh := s.refresh
	discordEnabled := s.discordEnabled
	debugURL := s.debugURL
	policyProvider := s.policyProvider
	s.mu.RUnlock()

	mode := string(policy.DefaultMode)
	if policyProvider != nil {
		mode, _ = policyProvider.CurrentCampaignPolicy()
	}

	data := DropsPageData{
		Username:       s.username,
		RefreshMinutes: refresh,
		Version:        version.Version,
		DiscordEnabled: discordEnabled,
		DebugURL:       debugURL,
		PolicyMode:     mode,
		PolicyModes: []string{
			string(policy.ModeGameOrder), string(policy.ModeEndingSoonest),
			string(policy.ModeClosestToReward), string(policy.ModeLowAvailability), string(policy.ModeSmart),
		},
	}
	s.renderPage(w, r, "drops.html", data)
}

// handleAPIDrops renders the campaign-queue partial (also used for htmx
// auto-refresh so progress stays live without a full page reload).
func (s *Server) handleAPIDrops(w http.ResponseWriter, r *http.Request) {
	s.renderDropsList(w, r)
}

// renderDropsList builds and renders the campaign-queue partial, merging the
// progress-watchdog health badges and the campaign-policy decisions. Shared by
// the drops poll and the policy mode / per-drop-rule POST handlers.
func (s *Server) renderDropsList(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	provider := s.campaignsProvider
	progressProvider := s.dropProgressProvider
	policyProvider := s.policyProvider
	s.mu.RUnlock()

	var campaigns []*models.Campaign
	if provider != nil {
		campaigns = provider.Campaigns()
	}

	var progress health.ProgressSnapshot
	if progressProvider != nil {
		progress = progressProvider.DropProgress()
	}

	var policyByID map[string]*DropPolicyView
	if policyProvider != nil {
		_, decisions := policyProvider.PolicySnapshot()
		_, rules := policyProvider.CurrentCampaignPolicy()
		policyByID = buildDropPolicyByCampaign(campaigns, decisions, rules)
	}

	data := DropsListData{Campaigns: buildDropCampaignViews(campaigns, dropHealthByCampaign(progress), policyByID)}

	s.renderPartial(w, r, "drops_list", data)
}

// handleAPIDropsUpcoming renders the "Upcoming" tab: campaigns Twitch announced
// that have not started yet. Display-only — these are never farmed.
func (s *Server) handleAPIDropsUpcoming(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	provider := s.dropCatalogProvider
	s.mu.RUnlock()

	var upcoming []*models.Campaign
	if provider != nil {
		upcoming = provider.UpcomingCampaigns()
	}

	views := make([]DropCampaignView, 0, len(upcoming))
	now := time.Now()
	for _, c := range upcoming {
		v := buildDropCampaignView(c)
		v.Upcoming = true
		v.StartsInLabel = startsInLabel(c.StartAt, now)
		views = append(views, v)
	}

	s.renderPartial(w, r, "drops_list", DropsListData{Campaigns: views})
}

// handleAPIDropsPast renders the "Past" tab: expired campaigns from the durable
// catalog, grouped by recurring identity.
func (s *Server) handleAPIDropsPast(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	provider := s.dropCatalogProvider
	s.mu.RUnlock()

	var records []drops.CatalogRecord
	if provider != nil {
		recs, err := provider.PastCampaigns()
		if err != nil {
			slog.Error("Failed to load past drop campaigns", "error", err)
			writeInternalError(w, "Failed to load past campaigns")
			return
		}
		records = recs
	}

	s.renderPartial(w, r, "drops_past", DropsPastData{Groups: buildPastGroups(records)})
}

// startsInLabel renders how far in the future a campaign starts.
func startsInLabel(startAt, now time.Time) string {
	if startAt.IsZero() {
		return ""
	}
	d := startAt.Sub(now)
	if d <= 0 {
		return "starting now"
	}
	switch {
	case d < time.Hour:
		return fmt.Sprintf("starts in %dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("starts in %dh", int(d.Hours()))
	default:
		return "starts " + startAt.Format("2 Jan")
	}
}

// buildPastGroups turns catalog records (already ordered by campaign_key, then
// end_at DESC) into recurring-grouped view models for the "Past" tab.
func buildPastGroups(records []drops.CatalogRecord) []PastCampaignGroup {
	var groups []PastCampaignGroup
	byKey := map[string]int{} // campaign_key -> index in groups

	for _, rec := range records {
		idx, ok := byKey[rec.CampaignKey]
		if !ok {
			idx = len(groups)
			byKey[rec.CampaignKey] = idx
			g := PastCampaignGroup{
				CampaignKey: rec.CampaignKey,
				Name:        rec.Name,
				GameName:    rec.Game,
				BoxArtURL:   boxArtURL(rec.Game),
			}
			if !rec.EndAt.IsZero() {
				g.LastEnded = rec.EndAt.Format("2 Jan 2006")
			}
			groups = append(groups, g)
		}
		g := &groups[idx]
		g.Count++
		if rec.Claimed {
			g.ClaimedCount++
		}
		inst := PastInstanceView{
			CampaignID: rec.CampaignID,
			Claimed:    rec.Claimed,
		}
		if !rec.StartAt.IsZero() {
			inst.StartLabel = rec.StartAt.Format("2 Jan 2006")
		}
		if !rec.EndAt.IsZero() {
			inst.EndLabel = rec.EndAt.Format("2 Jan 2006")
		}
		if rec.Claimed {
			inst.StatusLabel = "Claimed"
		} else {
			inst.StatusLabel = "Not claimed"
		}
		g.Instances = append(g.Instances, inst)
	}
	return groups
}

// buildDropCampaignViews turns tracked campaigns into the Drops-page queue,
// ordered to mirror the watcher's DROPS priority: still-earnable campaigns
// come before already-claimed ones, channel-restricted campaigns (whose
// progress can only ever be earned on their own channel) outrank unrestricted
// ones, and within those groups the campaign closest to its reward — then the
// one ending soonest — comes first.
func buildDropCampaignViews(campaigns []*models.Campaign, healthByID map[string]*DropHealthView, policyByID map[string]*DropPolicyView) []DropCampaignView {
	ordered := make([]*models.Campaign, len(campaigns))
	copy(ordered, campaigns)

	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]

		aClaimed := a.ClaimStatus == models.CampaignClaimStatusAlreadyClaimed
		bClaimed := b.ClaimStatus == models.CampaignClaimStatusAlreadyClaimed
		if aClaimed != bClaimed {
			return !aClaimed
		}

		if a.IsChannelRestricted() != b.IsChannelRestricted() {
			return a.IsChannelRestricted()
		}

		if ap, bp := a.OverallProgressPercent(), b.OverallProgressPercent(); ap != bp {
			return ap > bp
		}

		if !a.EndAt.Equal(b.EndAt) {
			return a.EndAt.Before(b.EndAt)
		}
		return a.Name < b.Name
	})

	views := make([]DropCampaignView, 0, len(ordered))
	for _, c := range ordered {
		view := buildDropCampaignView(c)
		view.Health = healthByID[c.ID]
		view.Policy = policyByID[c.ID]
		views = append(views, view)
	}
	return views
}

// dropHealthByCampaign turns the watchdog snapshot into per-campaign badge
// views, keyed by campaign ID for the queue builder to merge.
func dropHealthByCampaign(snap health.ProgressSnapshot) map[string]*DropHealthView {
	if !snap.Enabled || len(snap.Drops) == 0 {
		return nil
	}
	out := make(map[string]*DropHealthView, len(snap.Drops))
	for i := range snap.Drops {
		d := snap.Drops[i]
		out[d.CampaignID] = buildDropHealthView(d)
	}
	return out
}

// healthDurationSince renders how long something has lasted ("8m", "1h 5m").
func healthDurationSince(t time.Time) string {
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
}

// recoveryStageLabels maps the watchdog's stable stage names to the short
// human labels shown on the Drops-page badge.
var recoveryStageLabels = map[string]string{
	"progress_sync":    "inventory sync",
	"full_resync":      "full campaign resync",
	"stream_info":      "stream info refresh",
	"transport_probe":  "watch transport probe",
	"session_recreate": "watch session recreate",
	"channel_switch":   "channel switch",
	"notify":           "operator notification",
}

// buildDropHealthView renders one watchdog drop state as the card badge plus
// its explanatory lines (mirroring the Health Center's honest, redacted style).
func buildDropHealthView(d health.DropProgress) *DropHealthView {
	view := &DropHealthView{Status: d.Status}

	stageLabel := recoveryStageLabels[d.RecoveryStageName]
	if stageLabel == "" {
		stageLabel = d.RecoveryStageName
	}

	switch d.Status {
	case health.ProgressStalled:
		view.Label = "STALLED"
		view.BadgeColor = "#ef4444"
		view.Lines = append(view.Lines, "Automatic recovery did not help")
		if stageLabel != "" {
			view.Lines = append(view.Lines, "Last attempt: "+stageLabel)
		}
		if !d.LastProgressAt.IsZero() {
			view.Lines = append(view.Lines, "No progress for "+healthDurationSince(d.LastProgressAt))
		}
	case health.ProgressRecovering:
		view.Label = "RECOVERING"
		view.BadgeColor = "#f59e0b"
		if !d.LastProgressAt.IsZero() {
			view.Lines = append(view.Lines, "No progress for "+healthDurationSince(d.LastProgressAt))
		}
		view.Lines = append(view.Lines, fmt.Sprintf("Watch reports: %d delivered", d.ReportsSinceProgress))
		if stageLabel != "" {
			view.Lines = append(view.Lines, "Stage: "+stageLabel)
		}
	default:
		view.Label = "HEALTHY"
		view.BadgeColor = "#22c55e"
		if !d.LastProgressAt.IsZero() {
			view.Lines = append(view.Lines, "Last progress: "+formatHealthAgo(d.LastProgressAt))
		}
		if d.Channel != "" {
			view.Lines = append(view.Lines, "Channel: "+d.Channel)
		}
	}
	return view
}

func buildDropCampaignView(c *models.Campaign) DropCampaignView {
	view := DropCampaignView{
		ID:                c.ID,
		Name:              c.Name,
		ChannelRestricted: c.IsChannelRestricted(),
		Claimed:           c.ClaimStatus == models.CampaignClaimStatusAlreadyClaimed,
		OverallPercent:    c.OverallProgressPercent(),
		Drops:             buildDropDetailViews(c),
	}

	if c.Game != nil {
		view.GameName = c.Game.Name
		view.BoxArtURL = boxArtURL(c.Game.Name)
	}

	if view.Claimed {
		view.StatusLabel = "Already claimed"
	} else {
		view.StatusLabel = "In progress"
	}

	if drop := c.CurrentDrop(); drop != nil {
		view.DropName = drop.Name
		view.DropBenefit = drop.Benefit
		if drop.MinutesRequired > 0 {
			view.HasMinuteProgress = true
			view.MinutesWatched = drop.CurrentMinutesWatched
			view.MinutesRequired = drop.MinutesRequired
			view.MinutesRemaining = drop.MinutesRemaining()
			view.MinutePercent = drop.ClampedProgress()
		}
	}

	return view
}

// buildDropDetailViews assembles the per-drop breakdown shown in the Drops-page
// modal. Still-earnable drops come from Campaign.Drops (ordered by watch
// requirement so tiers read in progression order); already-claimed rewards come
// from Campaign.ClaimedDropNames, which the claim-history pass populated after
// stripping those drops from Campaign.Drops. Together they list every drop in
// the campaign with its own status, not just the current/final one.
func buildDropDetailViews(c *models.Campaign) []DropDetailView {
	ordered := make([]*models.Drop, len(c.Drops))
	copy(ordered, c.Drops)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].MinutesRequired < ordered[j].MinutesRequired
	})

	views := make([]DropDetailView, 0, len(ordered)+len(c.ClaimedDropNames))
	for _, d := range ordered {
		detail := DropDetailView{
			Name:        d.Name,
			Benefit:     d.Benefit,
			ImageURL:    d.ImageURL,
			StatusLabel: "In progress",
			Percent:     d.ClampedProgress(),
		}
		if d.MinutesRequired > 0 {
			detail.HasMinuteProgress = true
			detail.MinutesWatched = d.CurrentMinutesWatched
			detail.MinutesRequired = d.MinutesRequired
		}
		views = append(views, detail)
	}

	for _, name := range c.ClaimedDropNames {
		views = append(views, DropDetailView{
			Name:        name,
			Claimed:     true,
			StatusLabel: "Already claimed",
			Percent:     100,
		})
	}

	return views
}

// boxArtURL builds a Twitch box-art CDN URL for a game by display name. The
// miner never captures a box-art URL from the drops GraphQL response, so the
// dashboard reconstructs it the same way Twitch's own frontend does.
func boxArtURL(gameName string) string {
	if gameName == "" {
		return ""
	}
	return fmt.Sprintf("https://static-cdn.jtvnw.net/ttv-boxart/%s-144x192.jpg", url.PathEscape(gameName))
}
