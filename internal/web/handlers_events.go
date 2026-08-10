package web

// Task S5-7: the Events group's four direct-render routes.
//
//   - /events          (route 9)  — the session-scoped event journal.
//   - /events/browser  (route 10) — browser Notification API status/test.
//   - /events/sound    (route 11) — WebAudio sound status/test.
//   - /events/discord  (route 12) — Discord notification status/test.
//
// Route 9 reads ONLY the process-wide in-memory event ring
// (internal/events.Recent): session-scoped (S-SESS), nothing persisted, no
// polling/SSE/WS, no new API endpoint, and no delivery evidence — B3 (a
// per-notification delivery journal) has no backend, and the
// upcoming_campaign_notifications table explicitly does not satisfy it, so
// no delivery column or facet exists anywhere here. The ten underlying
// event types are presented in five OWNER-mandated presentation groups;
// the types themselves are unchanged.
//
// Routes 10-11 are pure client-side capability status pages: permission
// requests and test playback happen exclusively on an explicit user gesture
// (see the templates' source contracts in s5_7_events_test.go), and nothing
// is persisted. Route 12 is status/test only: it POSTs the EXISTING
// /api/notifications/test endpoint on an explicit click and never reads,
// renders or logs the bot token — route 20 (/settings/events-notifications)
// remains the sole notification_config/point-rule owner and route 21
// (/settings/discord) the sole Discord connection config owner; routes 9-12
// never write settings or config.

import (
	"net/http"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/util"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version"
)

// eventJournalLimit caps how much of the ring the journal reads — the same
// bound the ring itself has (internal/events defaultCapacity).
const eventJournalLimit = 200

// eventJournalGroup is one of the exactly five PRESENTATION groups the
// journal renders. Key is the stable ?group= filter value; LabelKey the
// localized display name; Types the underlying (unchanged) event types the
// group presents.
type eventJournalGroup struct {
	Key      string
	LabelKey string
	Types    []events.Type
}

// eventJournalGroups is the OWNER-mandated five-group / ten-type mapping
// (task S5-7 item 1). Nothing outside these ten types ever renders on the
// journal.
var eventJournalGroups = []eventJournalGroup{
	{Key: "streamer-status", LabelKey: "events.group.streamer_status", Types: []events.Type{events.TypeStreamerOnline, events.TypeStreamerOffline}},
	{Key: "points-rewards", LabelKey: "events.group.points_rewards", Types: []events.Type{events.TypeBonusClaimed, events.TypePointsEarned, events.TypeRewardRedeemed}},
	{Key: "predictions", LabelKey: "events.group.predictions", Types: []events.Type{events.TypeBetPlaced, events.TypeBetResult}},
	{Key: "drops", LabelKey: "events.group.drops", Types: []events.Type{events.TypeDropClaimed, events.TypeMomentClaimed}},
	{Key: "raids", LabelKey: "events.group.raids", Types: []events.Type{events.TypeRaidJoined}},
}

// eventJournalGroupByType derives the type -> group lookup once from
// eventJournalGroups, so the mapping can never drift from its single
// definition above.
var eventJournalGroupByType = func() map[events.Type]*eventJournalGroup {
	m := make(map[events.Type]*eventJournalGroup)
	for i := range eventJournalGroups {
		g := &eventJournalGroups[i]
		for _, ty := range g.Types {
			m[ty] = g
		}
	}
	return m
}()

// journalEvents returns the ring snapshot the journal renders: the
// recentEvents test seam when set (see server.go), events.Recent otherwise.
// events.Recent() is the journal's ONLY production data source.
func (s *Server) journalEvents() []events.Event {
	s.mu.RLock()
	fn := s.recentEvents
	s.mu.RUnlock()
	if fn != nil {
		return fn(eventJournalLimit)
	}
	return events.Recent(eventJournalLimit)
}

// eventsPageShell builds the base.html shell fields shared by all four
// Events pages, mirroring settingsCategoryPageData.
func (s *Server) eventsPageShell() (username string, refresh int, discordEnabled bool, debugURL string) {
	s.mu.RLock()
	refresh = s.refresh
	discordEnabled = s.discordEnabled
	debugURL = s.debugURL
	s.mu.RUnlock()
	return s.username, refresh, discordEnabled, debugURL
}

// handleEventsPage renders /events (route 9): the session-scoped event
// journal. Renders identically regardless of Discord configuration — the
// Discord-availability content the S5-2 minimal landing used to show now
// lives on /events/discord.
func (s *Server) handleEventsPage(w http.ResponseWriter, r *http.Request) {
	if !requireGetOrHead(w, r) {
		return
	}

	lang := s.langFromRequest(r)
	tr := func(key string) string { return s.i18n.T(lang, key) }
	username, refresh, discordEnabled, debugURL := s.eventsPageShell()

	// Normalize the group filter: anything but the five known keys means All.
	group := r.URL.Query().Get("group")
	var active *eventJournalGroup
	for i := range eventJournalGroups {
		if eventJournalGroups[i].Key == group {
			active = &eventJournalGroups[i]
			break
		}
	}
	if active == nil {
		group = ""
	}

	refreshHref := "/events"
	if group != "" {
		refreshHref += "?group=" + group
	}

	filters := make([]EventFilterView, 0, len(eventJournalGroups)+1)
	filters = append(filters, EventFilterView{Key: "all", Label: tr("events.filter.all"), Href: "/events", Active: group == ""})
	for i := range eventJournalGroups {
		g := &eventJournalGroups[i]
		filters = append(filters, EventFilterView{
			Key:    g.Key,
			Label:  tr(g.LabelKey),
			Href:   "/events?group=" + g.Key,
			Active: group == g.Key,
		})
	}

	recent := s.journalEvents()
	journalTotal := 0
	var rows []EventRowView
	for _, e := range recent {
		g, ok := eventJournalGroupByType[e.Type]
		if !ok {
			continue // not one of the ten journal types
		}
		journalTotal++
		if active != nil && g != active {
			continue
		}
		rows = append(rows, EventRowView{
			Ago:        util.FormatDuration(time.Since(e.Time)) + " " + tr("common.ago"),
			TypeKey:    string(e.Type),
			GroupKey:   g.Key,
			GroupLabel: tr(g.LabelKey),
			Label:      tr(eventTypeKeys[e.Type]),
			Streamer:   e.Streamer,
			Detail:     e.Detail,
		})
	}

	data := EventsPageData{
		Username:       username,
		RefreshMinutes: refresh,
		Version:        version.Version,
		DiscordEnabled: discordEnabled,
		DebugURL:       debugURL,
		Filters:        filters,
		ActiveGroup:    group,
		Rows:           rows,
		RefreshHref:    refreshHref,
	}
	if len(rows) == 0 {
		// S-EMPTY, honestly split: an entirely empty session journal vs a
		// populated journal whose selected group has no events yet.
		msgKey := "events.empty"
		if active != nil && journalTotal > 0 {
			msgKey = "events.empty_group"
		}
		data.EmptyState = &StateBlockData{State: "EMPTY", Variant: "block", Message: tr(msgKey)}
	}

	s.renderPage(w, r, "events.html", data)
}

// handleEventsBrowserPage renders /events/browser (route 10): a pure
// Notification-API status page — all state is computed client-side, and
// permission/test actions run only on an explicit user gesture.
func (s *Server) handleEventsBrowserPage(w http.ResponseWriter, r *http.Request) {
	if !requireGetOrHead(w, r) {
		return
	}
	username, refresh, discordEnabled, debugURL := s.eventsPageShell()
	s.renderPage(w, r, "events_browser.html", EventsBrowserPageData{
		Username:       username,
		RefreshMinutes: refresh,
		Version:        version.Version,
		DiscordEnabled: discordEnabled,
		DebugURL:       debugURL,
	})
}

// handleEventsSoundPage renders /events/sound (route 11): status plus a
// gesture-driven WebAudio test, fail-open by design, nothing persisted.
func (s *Server) handleEventsSoundPage(w http.ResponseWriter, r *http.Request) {
	if !requireGetOrHead(w, r) {
		return
	}
	username, refresh, discordEnabled, debugURL := s.eventsPageShell()
	s.renderPage(w, r, "events_sound.html", EventsSoundPageData{
		Username:       username,
		RefreshMinutes: refresh,
		Version:        version.Version,
		DiscordEnabled: discordEnabled,
		DebugURL:       debugURL,
	})
}

// handleEventsDiscordPage renders /events/discord (route 12): status/test
// only. ConfigValid/ConfigError come from Manager.IsConfigValid — the same
// static, token-free signal the legacy /notifications page uses (a nil
// manager follows that page's precedent: treated as valid, and the test
// endpoint itself answers honestly with 503 if invoked). The bot token is
// never read here.
func (s *Server) handleEventsDiscordPage(w http.ResponseWriter, r *http.Request) {
	if !requireGetOrHead(w, r) {
		return
	}
	username, refresh, discordEnabled, debugURL := s.eventsPageShell()

	s.mu.RLock()
	notifMgr := s.notificationManager
	s.mu.RUnlock()

	configValid := true
	configError := ""
	if notifMgr != nil {
		configValid, configError = notifMgr.IsConfigValid()
	}

	s.renderPage(w, r, "events_discord.html", EventsDiscordPageData{
		Username:       username,
		RefreshMinutes: refresh,
		Version:        version.Version,
		DiscordEnabled: discordEnabled,
		DebugURL:       debugURL,
		ConfigValid:    configValid,
		ConfigError:    configError,
	})
}
