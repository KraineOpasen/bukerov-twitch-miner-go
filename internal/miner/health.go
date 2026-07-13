package miner

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/health"
)

// healthCanaryConfig maps the persisted health settings to the canary's config.
func healthCanaryConfig(s config.HealthSettings) health.CanaryConfig {
	return health.CanaryConfig{
		Enabled:      s.CanaryEnabled,
		Channel:      s.CanaryChannel,
		Interval:     time.Duration(s.CanaryIntervalMinutes) * time.Minute,
		MaxStaleness: time.Duration(s.CanaryMaxStalenessHours) * time.Hour,
	}
}

// minerHealthNotifier adapts the miner's (possibly late-created) notification
// manager to health.Notifier, reading m.notifications at call time so the canary
// still notifies if Discord is enabled after startup.
type minerHealthNotifier struct{ m *Miner }

func (n minerHealthNotifier) NotifyHealthTransition(signal string, healthy bool, detail string) {
	n.m.mu.RLock()
	mgr := n.m.notifications
	n.m.mu.RUnlock()
	if mgr != nil {
		mgr.NotifyHealthTransition(signal, healthy, detail)
	}
}

// refreshHealthCenter records the non-canary health signals from the miner's
// existing providers. It runs on the health-watchdog tick, so it adds no
// goroutine. It records only redacted, non-sensitive summaries.
func (m *Miner) refreshHealthCenter(now time.Time) {
	if m.healthCenter == nil {
		return
	}

	m.mu.RLock()
	threshold := time.Duration(m.config.RateLimits.ConnectionTimeoutMinutes) * time.Minute
	reauth := m.reauthRequired
	m.mu.RUnlock()

	// OAuth.
	oauth := health.Signal{Name: health.SignalOAuth, CheckedAt: now, Status: health.StatusOK}
	if reauth {
		oauth.Status = health.StatusFailed
		oauth.Detail = "authorization expired or was revoked"
		oauth.ErrorCode = "reauth_required"
	}
	m.healthCenter.Record(oauth)

	// GQL API + active client ID.
	if m.client != nil {
		last := m.client.LastSuccessAt()
		m.healthCenter.Record(stalenessSignal(health.SignalGQLAPI, last, now, threshold, "no successful API response recently"))
		m.healthCenter.SetActiveClientID(m.client.ActiveClientID())
	}

	// PubSub.
	if m.wsPool != nil {
		last := m.wsPool.LastActivity()
		m.healthCenter.Record(stalenessSignal(health.SignalPubSub, last, now, threshold, "no PubSub activity recently"))
	}

	// Drops inventory sync + progress.
	if m.dropsTracker != nil {
		st := m.dropsTracker.SyncStatus()

		inv := health.Signal{Name: health.SignalDropsInventory, CheckedAt: st.LastSyncAt}
		switch {
		case st.LastSyncAt.IsZero():
			inv.Status = health.StatusUnknown
		case st.LastError != "":
			inv.Status = health.StatusFailed
			inv.Detail = "the last inventory sync errored"
			inv.ErrorCode = "sync_error"
		default:
			inv.Status = health.StatusOK
		}
		m.healthCenter.Record(inv)

		// Drops progress is OK while campaigns are tracked, IDLE when none.
		// Confirmed STALLED detection is the Stage 3 drop-progress watchdog.
		prog := health.Signal{Name: health.SignalDropsProgress, CheckedAt: now, Status: health.StatusIdle, Detail: "no active drop campaign"}
		if st.TrackedCampaigns > 0 {
			prog.Status = health.StatusOK
			prog.Detail = fmt.Sprintf("%d active campaign(s) tracked", st.TrackedCampaigns)
		}
		m.healthCenter.Record(prog)
	}
}

// stalenessSignal builds an OK/failed/unknown signal from a last-success
// timestamp compared against a threshold.
func stalenessSignal(name string, last, now time.Time, threshold time.Duration, staleDetail string) health.Signal {
	sig := health.Signal{Name: name, CheckedAt: last}
	switch {
	case last.IsZero():
		sig.Status = health.StatusUnknown
	case now.Sub(last) > threshold:
		sig.Status = health.StatusFailed
		sig.Detail = staleDetail
		sig.ErrorCode = "stale"
	default:
		sig.Status = health.StatusOK
	}
	return sig
}

// --- web.HealthProvider implementation ---

// HealthSnapshot returns the current aggregated health signals for the dashboard
// and debug endpoint.
func (m *Miner) HealthSnapshot() health.Snapshot {
	if m.healthCenter == nil {
		return health.Snapshot{}
	}
	return m.healthCenter.Snapshot()
}

// RunCanaryNow triggers an on-demand watch-transport probe (the "Run canary now"
// button). Duplicate runs are suppressed inside the canary.
func (m *Miner) RunCanaryNow() {
	if m.canary != nil {
		m.canary.RunNow()
	}
}

// CurrentHealthSettings returns the persisted canary settings for the Health
// Center form.
func (m *Miner) CurrentHealthSettings() config.HealthSettings {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config.Health
}

// ApplyHealthSettings validates, applies (runtime, no restart), and persists new
// canary settings.
func (m *Miner) ApplyHealthSettings(s config.HealthSettings) {
	m.mu.Lock()
	m.config.Health = s
	config.ValidateConfig(m.config)
	applied := m.config.Health
	if m.configPath != "" {
		if err := config.SaveConfig(m.configPath, m.config); err != nil {
			slog.Error("Failed to save config", "error", err)
		}
	}
	m.mu.Unlock()

	if m.canary != nil {
		m.canary.UpdateSettings(healthCanaryConfig(applied))
	}
	slog.Info("Health canary settings updated", "enabled", applied.CanaryEnabled, "channel", applied.CanaryChannel)
}
