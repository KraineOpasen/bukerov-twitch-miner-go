package web

import (
	"net/http"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version"
)

// QueuePageData is the /overview/queue page's view model (task Phase 5/6):
// the C12 slot-pair header, the complete configured-streamer roster (C4/C3),
// and whether that roster is empty (S-EMPTY -> /settings/streamers, never an
// error).
type QueuePageData struct {
	Username       string
	RefreshMinutes int
	Version        string
	DiscordEnabled bool
	DebugURL       string

	SlotPair [2]c12SlotData
	Roster   []queueRosterRow

	// RosterEmpty is true when the complete configured streamer list has zero
	// entries - S-EMPTY, never an error, links to Settings. EmptyState is the
	// C1 state-block payload rendered in that case; zero-valued and unused
	// otherwise.
	RosterEmpty bool
	EmptyState  StateBlockData
}

// handleOverviewQueuePage renders /overview/queue: the S5-3 direct-render
// route completing the Overview section's two pages (task Phase 5). Every
// existing direct-render page handler (handleOverviewPage, handleEventsPage,
// handleHelpGettingStarted in handlers_chrome.go) serves GET and HEAD
// identically by never gating on r.Method at all - net/http's ResponseWriter
// already suppresses the body for HEAD at the transport layer - so this
// handler matches that exact, established shape rather than adding new
// method-checking behavior no sibling handler has.
func (s *Server) handleOverviewQueuePage(w http.ResponseWriter, r *http.Request) {
	data := s.buildQueuePageData(s.langFromRequest(r))
	s.renderPage(w, r, "queue.html", data)
}

// buildQueuePageData assembles the queue page's view model from the same
// in-memory state buildOverviewData reads (streamers, watch slots, the S5-3
// safe watching-evidence adapter) - no new Twitch calls, no new provider, no
// new serialized API response.
func (s *Server) buildQueuePageData(lang string) QueuePageData {
	tr := func(key string) string { return s.i18n.T(lang, key) }
	streamers := s.snapshotStreamers()
	stats, _, _ := s.ensureStats(streamers)

	s.mu.RLock()
	refresh := s.refresh
	discordEnabled := s.discordEnabled
	debugURL := s.debugURL
	provider := s.overviewProvider
	s.mu.RUnlock()

	var slots WatchSlotsView
	if provider != nil {
		slots = provider.WatchSlots()
	}
	if slots.Watching == nil {
		slots.Watching = map[string]bool{}
	}

	evidence := s.watchSlotEvidence()
	roster := s.buildQueueRoster(streamers, slots, stats, evidence, tr)

	data := QueuePageData{
		Username:       s.username,
		RefreshMinutes: refresh,
		Version:        version.Version,
		DiscordEnabled: discordEnabled,
		DebugURL:       debugURL,
		SlotPair:       c12Pair(evidence, streamersByName(streamers), stats, tr),
		Roster:         roster,
		RosterEmpty:    len(roster) == 0,
	}
	if data.RosterEmpty {
		data.EmptyState = StateBlockData{
			State:        "EMPTY",
			Variant:      "block",
			Message:      tr("queue.empty.title"),
			ActionLabel:  tr("queue.empty.action"),
			ActionTarget: "/settings/streamers",
		}
	}
	return data
}
