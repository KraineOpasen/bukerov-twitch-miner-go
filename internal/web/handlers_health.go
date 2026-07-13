package web

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/health"
)

// canaryDisclaimer is the honest limitation shown on the Health Center: the
// canary proves transport acceptance, not accrual of a specific drop.
const canaryDisclaimer = "The canary confirms Twitch accepts the watch transport and beacon requests. " +
	"Without an active drop campaign it does not prove accrual of a specific drop."

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

func healthStatusDisplay(status string) (string, string) {
	switch status {
	case health.StatusOK:
		return "OK", "#22c55e"
	case health.StatusFailed:
		return "FAILED", "#ef4444"
	case health.StatusStalled:
		return "STALLED", "#ef4444"
	case health.StatusIdle:
		return "IDLE", "#a1a1aa"
	default:
		return "UNKNOWN", "#a1a1aa"
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

func (s *Server) buildHealthView() HealthView {
	s.mu.RLock()
	provider := s.healthProvider
	s.mu.RUnlock()

	view := HealthView{Disclaimer: canaryDisclaimer}
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
		text, color := healthStatusDisplay(sig.Status)
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
	s.renderPage(w, "health.html", s.buildHealthView())
}

// handleAPIHealth renders the Health Center partial (htmx auto-refresh).
func (s *Server) handleAPIHealth(w http.ResponseWriter, r *http.Request) {
	s.renderHealthPartial(w)
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
	s.renderHealthPartial(w)
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
		s.renderHealthPartial(w)
		return
	}

	// The canary and watchdog settings live on separate forms; each posts only
	// its own fields (identified by a hidden "section" input), so saving one
	// section never clobbers unsaved edits — or checkbox state — of the other.
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
	s.renderHealthPartial(w)
}

func (s *Server) renderHealthPartial(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html")
	tmpl := s.templates["partials"]
	if tmpl == nil {
		writeInternalError(w, "Partials not loaded")
		return
	}
	if err := tmpl.ExecuteTemplate(w, "health_center", s.buildHealthView()); err != nil {
		slog.Error("Failed to render health center", "error", err)
		writeInternalError(w, "Failed to render")
	}
}

func atoiDefault(s string, def int) int {
	if v, err := strconv.Atoi(s); err == nil {
		return v
	}
	return def
}
