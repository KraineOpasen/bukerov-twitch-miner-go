package web

// Task S5-9: the Help group's four remaining direct-render routes (routes
// 27-30 of the design's 30-route page matrix, Stage 4 §11).
//
//   - /help/glossary            (27) — machine reason/status code dictionary.
//   - /help/troubleshooting     (28) — the four data-freshness states, explained.
//   - /help/notifications-audio (29) — browser gesture/permission/fail-open model.
//   - /help/diagnostics-support (30) — deep links to the canonical Diagnostics
//     page; this page never generates a snapshot itself.
//
// Route 26 (/help/getting-started) is unchanged by this task except for its
// template's content — its handler (handleHelpGettingStarted) stays in
// handlers_chrome.go (S5-2).
//
// All five Help routes are static, backend-free "reading density" pages
// (Stage 4 §3): no htmx, no polling, no new API endpoint, no fake/live
// status. Every string is resolved by the template via its i18n key (never
// pre-resolved to one language here), exactly like every other page in this
// package — see the {{t "..."}} calls in the templates.
//
// The four PageData structs below (normally placed in viewmodels.go per
// this package's usual convention — see the pre-existing HelpPageData
// there) live here instead: this task's contract scopes its allowed edits
// to a fixed path list that does not include viewmodels.go, so every new
// symbol this slice needs is self-contained in this one new file rather
// than reopening that file for four struct additions.
import (
	"net/http"
	"sort"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version"
)

// helpGlossaryEntry is one machine code plus the i18n keys for its localized
// label and definition. Code is read directly from the same canonical
// package-level dictionary the rest of the dashboard already renders from —
// never hand-typed — so this page cannot become a second source of truth for
// codes it lists. See buildHelpGlossaryPageData.
type helpGlossaryEntry struct {
	Code     string
	LabelKey string
	DefKey   string
}

// helpGlossarySection groups related glossary entries under one heading.
type helpGlossarySection struct {
	HeadingKey string
	Entries    []helpGlossaryEntry
}

// HelpGlossaryPageData feeds /help/glossary (route 27).
type HelpGlossaryPageData struct {
	Username       string
	RefreshMinutes int
	Version        string
	DiscordEnabled bool
	DebugURL       string
	Sections       []helpGlossarySection
}

// HelpTroubleshootingPageData feeds /help/troubleshooting (route 28). Static
// editorial content — no per-request fields beyond the shared page shell.
type HelpTroubleshootingPageData struct {
	Username       string
	RefreshMinutes int
	Version        string
	DiscordEnabled bool
	DebugURL       string
}

// HelpNotificationsAudioPageData feeds /help/notifications-audio (route 29).
// Static editorial content — no per-request fields beyond the shared page
// shell.
type HelpNotificationsAudioPageData struct {
	Username       string
	RefreshMinutes int
	Version        string
	DiscordEnabled bool
	DebugURL       string
}

// HelpDiagnosticsSupportPageData feeds /help/diagnostics-support (route 30).
// Static editorial content — no per-request fields beyond the shared page
// shell.
type HelpDiagnosticsSupportPageData struct {
	Username       string
	RefreshMinutes int
	Version        string
	DiscordEnabled bool
	DebugURL       string
}

// handleHelpGlossaryPage renders /help/glossary: a mono definition list
// built by ranging over the same canonical Go dictionaries the rest of the
// dashboard already renders reason/status/event labels from (never a second,
// independently-typed copy of those codes). See buildHelpGlossaryPageData.
//
// Only two code families proved to have a single canonical Go-owned source
// at implementation time (queue/roster reason-and-status codes, and the
// event-journal type/group dictionaries) — both wired in below. The drop
// claim-state vocabulary, the internal "S-state" design vocabulary, the
// browser/sound permission states, and log levels do NOT have a canonical
// Go dictionary today (they are ad hoc string literals or client-only JS
// vocabularies); listing them here would mean inventing a second source of
// truth, which this glossary must not become, so they are intentionally
// omitted (see help.glossary.scope_note) rather than hand-typed. General
// status concepts (Unknown/Stale/Degraded/Failure) are covered in
// /help/troubleshooting instead, in prose, without needing a code dictionary.
func (s *Server) handleHelpGlossaryPage(w http.ResponseWriter, r *http.Request) {
	if !requireGetOrHead(w, r) {
		return
	}
	s.renderPage(w, r, "help_glossary.html", s.buildHelpGlossaryPageData())
}

// handleHelpTroubleshootingPage renders /help/troubleshooting: prose
// distinguishing Unknown, Stale, Degraded and Failure, deep-linking each to
// the real page where that state is actually observable.
func (s *Server) handleHelpTroubleshootingPage(w http.ResponseWriter, r *http.Request) {
	if !requireGetOrHead(w, r) {
		return
	}
	s.renderPage(w, r, "help_troubleshooting.html", s.buildHelpTroubleshootingPageData())
}

// handleHelpNotificationsAudioPage renders /help/notifications-audio: the
// browser gesture/permission model and the fail-open invariant (a playback
// failure never stops or pauses the miner).
func (s *Server) handleHelpNotificationsAudioPage(w http.ResponseWriter, r *http.Request) {
	if !requireGetOrHead(w, r) {
		return
	}
	s.renderPage(w, r, "help_notifications_audio.html", s.buildHelpNotificationsAudioPageData())
}

// handleHelpDiagnosticsSupportPage renders /help/diagnostics-support: deep
// links to the canonical /system/diagnostics page (and its Diagnostic
// Snapshot action). This handler never builds, requests, or links any
// snapshot-generating endpoint of its own.
func (s *Server) handleHelpDiagnosticsSupportPage(w http.ResponseWriter, r *http.Request) {
	if !requireGetOrHead(w, r) {
		return
	}
	s.renderPage(w, r, "help_diagnostics_support.html", s.buildHelpDiagnosticsSupportPageData())
}

// buildHelpGlossaryPageData ranges over reasonCodeKeys, rosterStatusKeys
// (viewmodels_slots.go) and eventTypeKeys/eventJournalGroups
// (handlers_overview.go / handlers_events.go) — the same package-level
// dictionaries already used to render reason/status/event labels elsewhere —
// to build the glossary's entries. Keys are sorted for a deterministic
// render. Nothing here is a second, independently-maintained list of codes:
// removing or renaming an entry in any of those source dictionaries changes
// what this page renders on the next build, and s5_9_help_test.go's parity
// test fails if the rendered code set and a source dictionary's key set ever
// diverge.
func (s *Server) buildHelpGlossaryPageData() HelpGlossaryPageData {
	username, refresh, discordEnabled, debugURL := s.helpPageShell()

	reasonEntries := make([]helpGlossaryEntry, 0, len(reasonCodeKeys))
	for _, code := range sortedStringKeys(reasonCodeKeys) {
		reasonEntries = append(reasonEntries, helpGlossaryEntry{
			Code:     code,
			LabelKey: reasonCodeKeys[code],
			DefKey:   "help.glossary.def.reason." + code,
		})
	}

	statusEntries := make([]helpGlossaryEntry, 0, len(rosterStatusKeys))
	for _, code := range sortedStringKeys(rosterStatusKeys) {
		statusEntries = append(statusEntries, helpGlossaryEntry{
			Code:     code,
			LabelKey: rosterStatusKeys[code],
			DefKey:   "help.glossary.def.status." + code,
		})
	}

	eventEntries := make([]helpGlossaryEntry, 0, len(eventTypeKeys))
	for _, code := range sortedEventTypeKeys(eventTypeKeys) {
		eventEntries = append(eventEntries, helpGlossaryEntry{
			Code:     string(code),
			LabelKey: eventTypeKeys[code],
			DefKey:   "help.glossary.def.event." + string(code),
		})
	}

	groupEntries := make([]helpGlossaryEntry, 0, len(eventJournalGroups))
	for _, g := range eventJournalGroups {
		groupEntries = append(groupEntries, helpGlossaryEntry{
			Code:     g.Key,
			LabelKey: g.LabelKey,
			DefKey:   "help.glossary.def.event_group." + dashToUnderscore(g.Key),
		})
	}

	return HelpGlossaryPageData{
		Username:       username,
		RefreshMinutes: refresh,
		Version:        version.Version,
		DiscordEnabled: discordEnabled,
		DebugURL:       debugURL,
		Sections: []helpGlossarySection{
			{HeadingKey: "help.glossary.section.reason", Entries: reasonEntries},
			{HeadingKey: "help.glossary.section.status", Entries: statusEntries},
			{HeadingKey: "help.glossary.section.event", Entries: eventEntries},
			{HeadingKey: "help.glossary.section.event_group", Entries: groupEntries},
		},
	}
}

func (s *Server) buildHelpTroubleshootingPageData() HelpTroubleshootingPageData {
	username, refresh, discordEnabled, debugURL := s.helpPageShell()
	return HelpTroubleshootingPageData{
		Username:       username,
		RefreshMinutes: refresh,
		Version:        version.Version,
		DiscordEnabled: discordEnabled,
		DebugURL:       debugURL,
	}
}

func (s *Server) buildHelpNotificationsAudioPageData() HelpNotificationsAudioPageData {
	username, refresh, discordEnabled, debugURL := s.helpPageShell()
	return HelpNotificationsAudioPageData{
		Username:       username,
		RefreshMinutes: refresh,
		Version:        version.Version,
		DiscordEnabled: discordEnabled,
		DebugURL:       debugURL,
	}
}

func (s *Server) buildHelpDiagnosticsSupportPageData() HelpDiagnosticsSupportPageData {
	username, refresh, discordEnabled, debugURL := s.helpPageShell()
	return HelpDiagnosticsSupportPageData{
		Username:       username,
		RefreshMinutes: refresh,
		Version:        version.Version,
		DiscordEnabled: discordEnabled,
		DebugURL:       debugURL,
	}
}

// helpPageShell returns the fields every Help page needs from base.html's
// chrome, factored out once — the same pattern handlers_events.go's
// eventsPageShell and handlers_settings_categories.go's
// settingsCategoryPageData use for their own shared shell fields, so the
// four builders above don't each repeat their own RLock/RUnlock block.
func (s *Server) helpPageShell() (username string, refresh int, discordEnabled bool, debugURL string) {
	s.mu.RLock()
	refresh = s.refresh
	discordEnabled = s.discordEnabled
	debugURL = s.debugURL
	s.mu.RUnlock()
	return s.username, refresh, discordEnabled, debugURL
}

// sortedStringKeys returns m's keys in a deterministic (sorted) order, so
// the glossary renders identically across requests and Go's randomized map
// iteration never leaks into the output.
func sortedStringKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sortedEventTypeKeys returns m's events.Type keys in deterministic
// (lexical, by underlying string value) order.
func sortedEventTypeKeys(m map[events.Type]string) []events.Type {
	keys := make([]events.Type, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

// dashToUnderscore converts an event-journal group's URL-facing Key (e.g.
// "streamer-status") to the underscore form used by its i18n definition key
// (e.g. "help.glossary.def.event_group.streamer_status") — the i18n key
// naming convention used throughout locales/{en,ru}.json never contains a
// literal hyphen.
func dashToUnderscore(s string) string {
	out := []byte(s)
	for i, c := range out {
		if c == '-' {
			out[i] = '_'
		}
	}
	return string(out)
}
