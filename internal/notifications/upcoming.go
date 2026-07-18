package notifications

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// maxUpcomingAttempts bounds how many times a failed upcoming-campaign alert is
// retried (once per full sync) before it is given up, so a permanently failing
// destination can never turn into an unbounded retry stream. Package variable so
// tests can shrink it.
var maxUpcomingAttempts = 5

// upcomingNotifyTimeout is the hard bound on the synchronous Discord send for an
// upcoming-campaign alert, so a slow provider can never stall the drops sync loop
// for long. Package variable so tests can shrink it.
var upcomingNotifyTimeout = 10 * time.Second

// nowMillis returns the current time in Unix milliseconds. Indirected so tests
// can pin observation timestamps if needed.
var nowMillis = func() int64 { return time.Now().UnixMilli() }

// NotifyUpcomingDropCampaign is the edge-triggered, opt-in alert for a newly
// announced relevant upcoming drop campaign. It is idempotent per campaign via
// the durable dedupe table, so the drops tracker may call it for every relevant
// upcoming campaign on every successful full sync — only a genuinely new one
// (with the event enabled and a destination configured) produces a single
// Discord message. It is safe to call concurrently (serialized by upcomingMu)
// and never blocks longer than upcomingNotifyTimeout.
//
// Disposition rules (durable, survive restarts):
//   - already notified / already suppressed -> no-op (no backfill of a campaign
//     first seen while the event was off);
//   - new + event off or no destination     -> recorded suppressed, nothing sent;
//   - new/pending + enabled + destination    -> one bounded send; on success ->
//     notified; on provider error -> pending (retried next full sync, capped at
//     maxUpcomingAttempts).
func (m *Manager) NotifyUpcomingDropCampaign(ctx context.Context, c *models.Campaign) {
	if c == nil || c.ID == "" {
		return
	}

	m.upcomingMu.Lock()
	defer m.upcomingMu.Unlock()

	notifType := string(NotificationTypeUpcomingCampaign)

	rec, err := m.repo.GetUpcomingNotifyState(c.ID, notifType)
	if err != nil {
		slog.Error("Upcoming-campaign notification: could not read dedupe state; skipping this cycle",
			"campaignID", c.ID, "error", err)
		return
	}
	switch rec.Status {
	case UpcomingStatusNotified:
		return // already delivered
	case UpcomingStatusSuppressed:
		return // first seen while disabled -> never backfill
	}

	// rec is absent (brand-new) or pending (a prior enabled attempt failed).
	m.mu.RLock()
	discord := m.discord
	discordEnabled := m.discordConfig.Enabled
	m.mu.RUnlock()

	cfg, cerr := m.repo.GetConfig()
	if cerr != nil {
		slog.Error("Upcoming-campaign notification: could not read config; skipping this cycle",
			"campaignID", c.ID, "error", cerr)
		return
	}

	destinationReady := discordEnabled && discord != nil && cfg.SystemChannelID != ""
	if !cfg.UpcomingDropsEnabled || !destinationReady {
		// Opt-in off, Discord off, or no system channel: record the campaign as
		// seen-but-suppressed (only if new; a pending row is left intact so it can
		// still deliver once the destination/opt-in is restored). This is not a
		// Drops error and produces no repeated WARN.
		if !rec.Found {
			if err := m.repo.InsertUpcomingSuppressedIfAbsent(c.ID, notifType, nowMillis()); err != nil {
				slog.Error("Upcoming-campaign notification: could not record suppressed state",
					"campaignID", c.ID, "error", err)
			}
		}
		return
	}

	// Bounded retry: stop after too many failures so a permanently broken
	// destination never retries forever.
	if rec.Found && rec.Attempts >= maxUpcomingAttempts {
		return
	}

	sendCtx, cancel := context.WithTimeout(ctx, upcomingNotifyTimeout)
	defer cancel()

	notification := m.buildUpcomingNotification(c, cfg.SystemChannelID)
	if serr := discord.Send(sendCtx, notification); serr != nil {
		slog.Warn("Upcoming-campaign notification delivery failed; eligible for retry on the next full sync",
			"campaign", c.Name, "campaignID", c.ID, "attempt", rec.Attempts+1, "error", serr)
		if err := m.repo.MarkUpcomingFailed(c.ID, notifType, nowMillis()); err != nil {
			slog.Error("Upcoming-campaign notification: could not record failed attempt",
				"campaignID", c.ID, "error", err)
		}
		return
	}

	// Delivered. Fan out to the push providers too (best-effort, like the other
	// operator alerts) and mark the campaign notified so it is never repeated.
	m.dispatchPush(NotificationTypeUpcomingCampaign, "", notification.Title+" — "+upcomingPushLine(c))
	if err := m.repo.MarkUpcomingNotified(c.ID, notifType, nowMillis()); err != nil {
		slog.Error("Upcoming-campaign notification: delivered but could not record dedupe state; a duplicate is possible on restart",
			"campaignID", c.ID, "error", err)
	}
	slog.Info("Sent upcoming Twitch Drops campaign notification", "campaign", c.Name, "campaignID", c.ID)
}

// buildUpcomingNotification renders the Discord message for an upcoming
// campaign. It reports absolute local times (never the raw UTC instant) and the
// campaign ID only as a diagnostic footer; it never includes tokens, cookies,
// raw GraphQL, internal errors, progress, or any promise of a guaranteed reward.
func (m *Manager) buildUpcomingNotification(c *models.Campaign, channelID string) Notification {
	loc := m.displayLocation()
	now := time.Now()

	var b strings.Builder
	fmt.Fprintf(&b, "**%s**", c.Name)
	if game := campaignGameName(c); game != "" {
		fmt.Fprintf(&b, "\nGame: %s", game)
	}
	if !c.StartAt.IsZero() {
		fmt.Fprintf(&b, "\nStarts: %s", formatLocalDateTime(c.StartAt, loc))
		if in := startsInText(c.StartAt, now); in != "" {
			fmt.Fprintf(&b, " (%s)", in)
		}
	}
	if !c.EndAt.IsZero() {
		fmt.Fprintf(&b, "\nEnds: %s", formatLocalDateTime(c.EndAt, loc))
	}
	if rewards := campaignRewardNames(c); len(rewards) > 0 {
		fmt.Fprintf(&b, "\nRewards: %s", strings.Join(rewards, ", "))
	}
	fmt.Fprintf(&b, "\nCampaign ID: %s", c.ID)
	fmt.Fprintf(&b, "\nDetected: %s", formatLocalDateTime(now, loc))

	return Notification{
		Type:      NotificationTypeUpcomingCampaign,
		Title:     "🎁 Upcoming Twitch Drops campaign",
		Message:   b.String(),
		ChannelID: channelID,
	}
}

// upcomingPushLine is the compact one-line summary sent to push providers.
func upcomingPushLine(c *models.Campaign) string {
	parts := []string{c.Name}
	if game := campaignGameName(c); game != "" {
		parts = append(parts, game)
	}
	if !c.StartAt.IsZero() {
		parts = append(parts, "starts "+c.StartAt.UTC().Format("2 Jan 15:04 MST"))
	}
	return strings.Join(parts, " · ")
}

// campaignGameName returns the campaign's game display name (falling back to the
// internal name), or "" when unknown.
func campaignGameName(c *models.Campaign) string {
	if c.Game == nil {
		return ""
	}
	if c.Game.DisplayName != "" {
		return c.Game.DisplayName
	}
	return c.Game.Name
}

// campaignRewardNames collects the campaign's reward names (drop benefit name,
// falling back to the drop's own name), de-duplicated, for the alert body.
func campaignRewardNames(c *models.Campaign) []string {
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
		loc = time.UTC
	}
	return t.In(loc).Format("2 Jan 2006, 15:04 MST")
}

// startsInText renders how far in the future a campaign starts as a short human
// phrase; "" when it has already started (between observation and rendering).
func startsInText(startAt, now time.Time) string {
	d := startAt.Sub(now)
	if d <= 0 {
		return "starts now"
	}
	switch {
	case d < time.Hour:
		return fmt.Sprintf("in %dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("in %dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("in %dd", int(d.Hours()/24))
	}
}
