package web

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"

	"github.com/PatrickWalther/twitch-miner-go/internal/models"
	"github.com/PatrickWalther/twitch-miner-go/internal/version"
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
	s.mu.RUnlock()

	data := DropsPageData{
		Username:       s.username,
		RefreshMinutes: refresh,
		Version:        version.Version,
		DiscordEnabled: discordEnabled,
		DebugURL:       debugURL,
	}
	s.renderPage(w, "drops.html", data)
}

// handleAPIDrops renders the campaign-queue partial (also used for htmx
// auto-refresh so progress stays live without a full page reload).
func (s *Server) handleAPIDrops(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	provider := s.campaignsProvider
	s.mu.RUnlock()

	var campaigns []*models.Campaign
	if provider != nil {
		campaigns = provider.Campaigns()
	}

	data := DropsListData{Campaigns: buildDropCampaignViews(campaigns)}

	w.Header().Set("Content-Type", "text/html")
	tmpl := s.templates["partials"]
	if tmpl == nil {
		writeInternalError(w, "Partials not loaded")
		return
	}
	if err := tmpl.ExecuteTemplate(w, "drops_list", data); err != nil {
		slog.Error("Failed to render drops list", "error", err)
		writeInternalError(w, "Failed to render")
	}
}

// buildDropCampaignViews turns tracked campaigns into the Drops-page queue,
// ordered to mirror the watcher's DROPS priority: still-earnable campaigns
// come before already-claimed ones, channel-restricted campaigns (whose
// progress can only ever be earned on their own channel) outrank unrestricted
// ones, and within those groups the campaign closest to its reward — then the
// one ending soonest — comes first.
func buildDropCampaignViews(campaigns []*models.Campaign) []DropCampaignView {
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
		views = append(views, buildDropCampaignView(c))
	}
	return views
}

func buildDropCampaignView(c *models.Campaign) DropCampaignView {
	view := DropCampaignView{
		Name:              c.Name,
		ChannelRestricted: c.IsChannelRestricted(),
		Claimed:           c.ClaimStatus == models.CampaignClaimStatusAlreadyClaimed,
		OverallPercent:    c.OverallProgressPercent(),
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

// boxArtURL builds a Twitch box-art CDN URL for a game by display name. The
// miner never captures a box-art URL from the drops GraphQL response, so the
// dashboard reconstructs it the same way Twitch's own frontend does.
func boxArtURL(gameName string) string {
	if gameName == "" {
		return ""
	}
	return fmt.Sprintf("https://static-cdn.jtvnw.net/ttv-boxart/%s-144x192.jpg", url.PathEscape(gameName))
}
