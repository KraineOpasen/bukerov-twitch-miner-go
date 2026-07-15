package web

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/health"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version"
)

// HealthSignalView is one row in the Health Center table.
type HealthSignalView struct {
	Label       string
	Status      string // display text: OK / FAILED / IDLE / STALLED / UNKNOWN
	StatusColor string // hex color for the status pill
	Ago         string // "24s ago" / "1h 12m ago" / "" when never checked
	Stage       string
	Detail      string
	ErrorCode   string
}

// HealthView is the Health Center page/partial view model.
type HealthView struct {
	// Base-layout fields expected by base.html on every full-page view model
	// (sidebar links, footer). Populated for the full-page render; harmless for
	// the htmx partial, which doesn't reference them.
	Username       string
	RefreshMinutes int
	Version        string
	DiscordEnabled bool
	DebugURL       string

	ActiveClientID          string
	Signals                 []HealthSignalView
	CanaryEnabled           bool
	CanaryChannel           string
	CanaryIntervalMinutes   int
	CanaryMaxStalenessHours int
	CanaryConfigured        bool
	Disclaimer              string
	Available               bool

	// Drop-progress watchdog settings (Stage 3).
	WatchdogEnabled                 bool
	WatchdogStallDelayMinutes       int
	WatchdogStallConfirmations      int
	WatchdogRecoveryCooldownMinutes int
	WatchdogAvoidTTLMinutes         int
	WatchdogRearmHours              int
}

var healthSignalLabels = map[string]string{
	health.SignalOAuth:          "OAuth",
	health.SignalGQLAPI:         "GQL API",
	health.SignalPubSub:         "PubSub",
	health.SignalWatchTransport: "Watch Transport",
	health.SignalDropsInventory: "Drops Inventory Sync",
	health.SignalDropsProgress:  "Drops Progress",
}

func healthStatusDisplay(status string, tr func(string) string) (string, string) {
	switch status {
	case health.StatusOK:
		return tr("health.status.ok"), "#22c55e"
	case health.StatusFailed:
		return tr("health.status.failed"), "#ef4444"
	case health.StatusStalled:
		return tr("health.status.stalled"), "#ef4444"
	case health.StatusIdle:
		return tr("health.status.idle"), "#a1a1aa"
	default:
		return tr("health.status.unknown"), "#a1a1aa"
	}
}

// formatHealthAgo renders how long ago a check happened, compactly.
func formatHealthAgo(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		return fmt.Sprintf("%dh %dm ago", h, m)
	}
}

func (s *Server) buildHealthView(tr func(string) string) HealthView {
	s.mu.RLock()
	provider := s.healthProvider
	username := s.username
	refresh := s.refresh
	discordEnabled := s.discordEnabled
	debugURL := s.debugURL
	s.mu.RUnlock()

	view := HealthView{
		Disclaimer:     tr("health.disclaimer"),
		Username:       username,
		RefreshMinutes: refresh,
		Version:        version.Version,
		DiscordEnabled: discordEnabled,
		DebugURL:       debugURL,
	}
	if provider == nil {
		return view
	}
	view.Available = true

	snap := provider.HealthSnapshot()
	view.ActiveClientID = snap.ActiveClientID
	for _, sig := range snap.Signals {
		label := healthSignalLabels[sig.Name]
		if label == "" {
			label = sig.Name
		}
		text, color := healthStatusDisplay(sig.Status, tr)
		view.Signals = append(view.Signals, HealthSignalView{
			Label:       label,
			Status:      text,
			StatusColor: color,
			Ago:         formatHealthAgo(sig.CheckedAt),
			Stage:       sig.Stage,
			Detail:      sig.Detail,
			ErrorCode:   sig.ErrorCode,
		})
	}

	cfg := provider.CurrentHealthSettings()
	view.CanaryEnabled = cfg.CanaryEnabled
	view.CanaryChannel = cfg.CanaryChannel
	view.CanaryIntervalMinutes = cfg.CanaryIntervalMinutes
	view.CanaryMaxStalenessHours = cfg.CanaryMaxStalenessHours
	view.CanaryConfigured = cfg.CanaryChannel != ""
	view.WatchdogEnabled = cfg.WatchdogEnabled
	view.WatchdogStallDelayMinutes = cfg.WatchdogStallDelayMinutes
	view.WatchdogStallConfirmations = cfg.WatchdogStallConfirmations
	view.WatchdogRecoveryCooldownMinutes = cfg.WatchdogRecoveryCooldownMinutes
	view.WatchdogAvoidTTLMinutes = cfg.WatchdogAvoidTTLMinutes
	view.WatchdogRearmHours = cfg.WatchdogRearmHours
	return view
}

// handleHealthPage renders the full Health Center page.
func (s *Server) handleHealthPage(w http.ResponseWriter, r *http.Request) {
	tr := func(key string) string { return s.i18n.T(s.langFromRequest(r), key) }
	s.renderPage(w, r, "health.html", s.buildHealthView(tr))
}

// handleAPIHealth renders the Health Center partial (htmx auto-refresh).
func (s *Server) handleAPIHealth(w http.ResponseWriter, r *http.Request) {
	s.renderHealthPartial(w, r)
}

// handleAPIHealthCanaryRun triggers an on-demand canary probe and re-renders.
func (s *Server) handleAPIHealthCanaryRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.RLock()
	provider := s.healthProvider
	s.mu.RUnlock()
	if provider != nil {
		provider.RunCanaryNow()
	}
	s.renderHealthPartial(w, r)
}

// handleAPIHealthSettings applies canary settings changes at runtime.
func (s *Server) handleAPIHealthSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		writeInternalError(w, "invalid form")
		return
	}

	s.mu.RLock()
	provider := s.healthProvider
	s.mu.RUnlock()
	if provider == nil {
		s.renderHealthPartial(w, r)
		return
	}

	// The canary and watchdog settings live on separate forms; each posts only
	// its own fields (identified by a hidden "section" input), so saving one
	// section never clobbers unsaved edits — or checkbox state — of the other.
	// healthFormMu makes the read-patch-apply sequence atomic across requests:
	// without it, two concurrent section saves could write a stale copy of one
	// section over the other's just-applied values.
	s.healthFormMu.Lock()
	defer s.healthFormMu.Unlock()

	cur := provider.CurrentHealthSettings()
	cfg := cur
	switch r.FormValue("section") {
	case "watchdog":
		cfg.WatchdogEnabled = r.FormValue("watchdogEnabled") == "on" || r.FormValue("watchdogEnabled") == "true"
		cfg.WatchdogStallDelayMinutes = atoiDefault(r.FormValue("watchdogStallDelayMinutes"), cur.WatchdogStallDelayMinutes)
		cfg.WatchdogStallConfirmations = atoiDefault(r.FormValue("watchdogStallConfirmations"), cur.WatchdogStallConfirmations)
		cfg.WatchdogRecoveryCooldownMinutes = atoiDefault(r.FormValue("watchdogRecoveryCooldownMinutes"), cur.WatchdogRecoveryCooldownMinutes)
		cfg.WatchdogAvoidTTLMinutes = atoiDefault(r.FormValue("watchdogAvoidTTLMinutes"), cur.WatchdogAvoidTTLMinutes)
		cfg.WatchdogRearmHours = atoiDefault(r.FormValue("watchdogRearmHours"), cur.WatchdogRearmHours)
	default:
		cfg.CanaryEnabled = r.FormValue("canaryEnabled") == "on" || r.FormValue("canaryEnabled") == "true"
		cfg.CanaryChannel = r.FormValue("canaryChannel")
		cfg.CanaryIntervalMinutes = atoiDefault(r.FormValue("canaryIntervalMinutes"), cur.CanaryIntervalMinutes)
		cfg.CanaryMaxStalenessHours = atoiDefault(r.FormValue("canaryMaxStalenessHours"), cur.CanaryMaxStalenessHours)
	}
	provider.ApplyHealthSettings(cfg)
	s.renderHealthPartial(w, r)
}

func (s *Server) renderHealthPartial(w http.ResponseWriter, r *http.Request) {
	tr := func(key string) string { return s.i18n.T(s.langFromRequest(r), key) }
	s.renderPartial(w, r, "health_center", s.buildHealthView(tr))
}

func atoiDefault(s string, def int) int {
	if v, err := strconv.Atoi(s); err == nil {
		return v
	}
	return def
}
