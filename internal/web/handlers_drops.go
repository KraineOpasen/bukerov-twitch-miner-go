package web

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
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

// handleAPIDropsSync backs the manual "Sync Drops now" action. It asks the drops
// tracker to schedule an immediate campaign resync — cooldown-gated and
// coalescing, so it can neither launch a parallel sync nor become a request
// storm — and returns the outcome plus the last sync's bookkeeping as JSON.
// It never blocks on the network sync (completion is observed via the polled
// status) and never surfaces a secret: only operation-level counts and the
// sanitized last-error string are returned. CSRF is enforced globally by
// csrfProtectMiddleware, so only the method is checked here.
func (s *Server) handleAPIDropsSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeNotAllowed(w)
		return
	}
	s.mu.RLock()
	provider := s.campaignsProvider
	s.mu.RUnlock()
	if provider == nil {
		writeServiceUnavailable(w, "Drops tracking is not available")
		return
	}

	res := provider.RequestManualSync()
	st := res.Status
	writeJSONOK(w, map[string]interface{}{
		"triggered":          res.Triggered,
		"retryAfterSecs":     int(res.RetryAfter.Round(time.Second).Seconds()),
		"runs":               st.Runs,
		"lastSyncAtMillis":   unixMilliOrZero(st.LastSyncAt),
		"dashboardCampaigns": st.DashboardCampaigns,
		"recoveredCampaigns": st.RecoveredCampaigns,
		"trackedCampaigns":   st.TrackedCampaigns,
		"lastError":          st.LastError,
	})
}

// unixMilliOrZero renders a time as Unix milliseconds, or 0 for the zero time
// (so "never synced" is unambiguous in JSON).
func unixMilliOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
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

	tr := func(key string) string { return s.i18n.T(s.langFromRequest(r), key) }

	var policyByID map[string]*DropPolicyView
	if policyProvider != nil {
		_, decisions := policyProvider.PolicySnapshot()
		_, rules := policyProvider.CurrentCampaignPolicy()
		policyByID = buildDropPolicyByCampaign(campaigns, decisions, rules, tr)
	}

	data := DropsListData{Campaigns: buildDropCampaignViews(campaigns, dropHealthByCampaign(progress, tr), policyByID, tr)}

	s.renderPartial(w, r, "drops_list", data)
}

// handleAPIDropsUpcoming renders the dedicated "Upcoming" tab: campaigns Twitch
// announced that have not started yet, filtered to the operator's game filter.
// Display-only — these are never farmed and never show active-progress UI. The
// tab distinguishes four honest states (never-synced / empty / stale / populated)
// instead of reusing the active tab's empty message. It only reads the backend
// snapshot the drops tracker already holds; it never triggers a Twitch sync, so
// the 1-minute auto-refresh is free of network cost.
func (s *Server) handleAPIDropsUpcoming(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	provider := s.dropCatalogProvider
	s.mu.RUnlock()

	tr := func(key string) string { return s.i18n.T(s.langFromRequest(r), key) }
	loc := s.displayLocation()

	if provider == nil {
		s.renderPartial(w, r, "drops_upcoming", DropsUpcomingData{State: UpcomingStateNeverSynced})
		return
	}

	status := provider.CampaignSyncStatus()
	upcoming := provider.RelevantUpcomingCampaigns()
	views := buildUpcomingViews(upcoming, time.Now(), loc, tr)

	data := DropsUpcomingData{Campaigns: views}
	if !status.LastSuccessAt.IsZero() {
		data.LastSuccessText = formatLocalDateTime(status.LastSuccessAt, loc)
	}
	data.State, data.HasError = classifyUpcomingState(status, len(views))

	s.renderPartial(w, r, "drops_upcoming", data)
}

// classifyUpcomingState maps the last full-sync bookkeeping and the current
// (relevant) upcoming card count to the Upcoming tab's honest lifecycle state.
// It relies on the tracker's backend contract: a FAILED sync preserves the
// previous upcoming snapshot (so cached cards survive an error, shown "stale"),
// and only a SUCCESSFUL empty sync publishes an empty snapshot ("empty"). The
// bool return is whether the last refresh errored.
func classifyUpcomingState(status drops.SyncStatus, cardCount int) (UpcomingState, bool) {
	neverSynced := status.LastSuccessAt.IsZero() && status.LastSyncAt.IsZero()
	hasError := status.LastError != ""
	switch {
	case neverSynced:
		return UpcomingStateNeverSynced, false
	case hasError:
		// Last refresh failed: keep any cached cards, or show the refresh-failed
		// message when there is no cache. Both render under the "stale" state.
		return UpcomingStateStale, true
	case cardCount > 0:
		return UpcomingStatePopulated, false
	default:
		return UpcomingStateEmpty, false
	}
}

// buildUpcomingViews turns relevant upcoming campaigns into display-only cards.
// Absolute start/end times are rendered in the dashboard's configured time zone;
// no active-progress, watched-minutes, or health fields are populated.
func buildUpcomingViews(campaigns []*models.Campaign, now time.Time, loc *time.Location, tr func(string) string) []UpcomingCampaignView {
	views := make([]UpcomingCampaignView, 0, len(campaigns))
	for _, c := range campaigns {
		if c == nil {
			continue
		}
		v := UpcomingCampaignView{
			ID:       c.ID,
			Name:     c.Name,
			StartsIn: startsInLabel(c.StartAt, now, loc, tr),
			Rewards:  upcomingRewardNames(c),
		}
		if c.Game != nil {
			v.GameName = c.Game.Name
			v.BoxArtURL = boxArtURL(c.Game.Name)
		}
		if !c.StartAt.IsZero() {
			v.StartLocal = formatLocalDateTime(c.StartAt, loc)
		}
		if !c.EndAt.IsZero() {
			v.EndLocal = formatLocalDateTime(c.EndAt, loc)
		}
		views = append(views, v)
	}
	return views
}

// upcomingRewardNames collects a campaign's reward names (drop benefit name,
// falling back to the drop name), de-duplicated and order-preserving.
func upcomingRewardNames(c *models.Campaign) []string {
	seen := make(map[string]struct{}, len(c.Drops))
	var out []string
	for _, d := range c.Drops {
		name := strings.TrimSpace(d.Benefit)
		if name == "" {
			name = strings.TrimSpace(d.Name)
		}
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// formatLocalDateTime renders an absolute instant in the given location.
func formatLocalDateTime(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.Local
	}
	return t.In(loc).Format("2 Jan 2006, 15:04 MST")
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

	tr := func(key string) string { return s.i18n.T(s.langFromRequest(r), key) }
	s.renderPartial(w, r, "drops_past", DropsPastData{Groups: buildPastGroups(records, tr)})
}

// startsInLabel renders how far in the future a campaign starts. The far-future
// branch renders a calendar date in the dashboard's configured time zone (loc)
// so it agrees with the card's absolute start time; the relative branches are
// pure durations and location-independent.
func startsInLabel(startAt, now time.Time, loc *time.Location, tr func(string) string) string {
	if startAt.IsZero() {
		return ""
	}
	if loc == nil {
		loc = time.Local
	}
	d := startAt.Sub(now)
	if d <= 0 {
		return tr("drops.starts.now")
	}
	switch {
	case d < time.Hour:
		return fmt.Sprintf(tr("drops.starts.in_minutes"), int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf(tr("drops.starts.in_hours"), int(d.Hours()))
	default:
		return tr("drops.starts.on_date") + " " + startAt.In(loc).Format("2 Jan")
	}
}

// buildPastGroups turns catalog records (already ordered by campaign_key, then
// end_at DESC) into recurring-grouped view models for the "Past" tab.
func buildPastGroups(records []drops.CatalogRecord, tr func(string) string) []PastCampaignGroup {
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
			inst.StatusLabel = tr("drops.past.claimed")
		} else {
			inst.StatusLabel = tr("drops.past.not_claimed")
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
func buildDropCampaignViews(campaigns []*models.Campaign, healthByID map[string]*DropHealthView, policyByID map[string]*DropPolicyView, tr func(string) string) []DropCampaignView {
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
		view := buildDropCampaignView(c, tr)
		view.Health = healthByID[c.ID]
		view.Policy = policyByID[c.ID]
		views = append(views, view)
	}
	return views
}

// dropHealthByCampaign turns the watchdog snapshot into per-campaign badge
// views, keyed by campaign ID for the queue builder to merge.
func dropHealthByCampaign(snap health.ProgressSnapshot, tr func(string) string) map[string]*DropHealthView {
	if !snap.Enabled || len(snap.Drops) == 0 {
		return nil
	}
	out := make(map[string]*DropHealthView, len(snap.Drops))
	for i := range snap.Drops {
		d := snap.Drops[i]
		out[d.CampaignID] = buildDropHealthView(d, tr)
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
func buildDropHealthView(d health.DropProgress, tr func(string) string) *DropHealthView {
	view := &DropHealthView{Status: d.Status}

	stageLabel := recoveryStageLabels[d.RecoveryStageName]
	if stageLabel == "" {
		stageLabel = d.RecoveryStageName
	}

	switch d.Status {
	case health.ProgressStalled:
		view.Label = tr("health.status.stalled")
		view.BadgeColor = "#ef4444"
		view.Lines = append(view.Lines, "Automatic recovery did not help")
		if stageLabel != "" {
			view.Lines = append(view.Lines, "Last attempt: "+stageLabel)
		}
		if !d.LastProgressAt.IsZero() {
			view.Lines = append(view.Lines, "No progress for "+healthDurationSince(d.LastProgressAt))
		}
	case health.ProgressRecovering:
		view.Label = tr("health.status.recovering")
		view.BadgeColor = "#f59e0b"
		if !d.LastProgressAt.IsZero() {
			view.Lines = append(view.Lines, "No progress for "+healthDurationSince(d.LastProgressAt))
		}
		view.Lines = append(view.Lines, fmt.Sprintf("Watch reports: %d delivered", d.ReportsSinceProgress))
		if stageLabel != "" {
			view.Lines = append(view.Lines, "Stage: "+stageLabel)
		}
	default:
		view.Label = tr("health.status.healthy")
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

func buildDropCampaignView(c *models.Campaign, tr func(string) string) DropCampaignView {
	view := DropCampaignView{
		ID:                c.ID,
		Name:              c.Name,
		ChannelRestricted: c.IsChannelRestricted(),
		Claimed:           c.ClaimStatus == models.CampaignClaimStatusAlreadyClaimed,
		OverallPercent:    c.OverallProgressPercent(),
		Drops:             buildDropDetailViews(c, tr),
	}

	if c.Game != nil {
		view.GameName = c.Game.Name
		view.BoxArtURL = boxArtURL(c.Game.Name)
	}

	if view.Claimed {
		view.StatusLabel = tr("drops.status.claimed")
	} else {
		view.StatusLabel = tr("drops.status.in_progress")
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
func buildDropDetailViews(c *models.Campaign, tr func(string) string) []DropDetailView {
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
			StatusLabel: tr("drops.status.in_progress"),
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
			StatusLabel: tr("drops.status.claimed"),
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
