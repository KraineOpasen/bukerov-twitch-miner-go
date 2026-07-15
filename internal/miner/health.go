package miner

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/health"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/pubsub"
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

// healthWatchdogConfig maps the persisted health settings to the drop-progress
// watchdog's config.
func healthWatchdogConfig(s config.HealthSettings) health.WatchdogConfig {
	return health.WatchdogConfig{
		Enabled:            s.WatchdogEnabled,
		StallDelay:         time.Duration(s.WatchdogStallDelayMinutes) * time.Minute,
		StallConfirmations: s.WatchdogStallConfirmations,
		RecoveryCooldown:   time.Duration(s.WatchdogRecoveryCooldownMinutes) * time.Minute,
		AvoidTTL:           time.Duration(s.WatchdogAvoidTTLMinutes) * time.Minute,
		Rearm:              time.Duration(s.WatchdogRearmHours) * time.Hour,
	}
}

// resolveStreamer maps a channel login to its live streamer object for the
// progress watchdog: the configured list first, then discovery's ephemeral
// channels. Returns nil when unknown.
func (m *Miner) resolveStreamer(login string) *models.Streamer {
	if m.streamers != nil {
		if s := m.streamers.Get(login); s != nil {
			return s
		}
	}
	if m.discovery != nil {
		return m.discovery.StreamerFor(login)
	}
	return nil
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

// minerDropNotifier adapts the (possibly late-created) notification manager to
// health.DropNotifier for the progress watchdog's stall/recovery alerts.
type minerDropNotifier struct{ m *Miner }

func (n minerDropNotifier) NotifyDropStalled(campaign, drop, channel, detail string) {
	n.m.mu.RLock()
	mgr := n.m.notifications
	n.m.mu.RUnlock()
	if mgr != nil {
		mgr.NotifyDropStalled(campaign, drop, channel, detail)
	}
}

func (n minerDropNotifier) NotifyDropRecovered(campaign, drop, channel, detail string) {
	n.m.mu.RLock()
	mgr := n.m.notifications
	n.m.mu.RUnlock()
	if mgr != nil {
		mgr.NotifyDropRecovered(campaign, drop, channel, detail)
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
		m.healthCenter.Record(stalenessSignal(health.SignalGQLAPI, last, now, threshold,
			"no successful API response recently",
			m.client.RecentGQLFailures(threshold), degradeGQLFailureThreshold, "repeated API request failures"))
		m.healthCenter.SetActiveClientID(m.client.ActiveClientID())
	}

	// PubSub. Evaluated per-connection rather than on the pool-wide max-PONG,
	// which would be blind to a single dead or topic-less index among healthy
	// siblings (see pubsubSignal).
	if m.wsPool != nil {
		m.healthCenter.Record(pubsubSignal(m.wsPool.ConnSnapshot(), m.wsPool.LastActivity(), now, threshold, m.wsPool.RecentReconnects(threshold)))
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

		// Drops progress: the progress watchdog owns the semantics (healthy /
		// recovering / confirmed STALLED); composing the signal here keeps the
		// health center single-writer per signal. When the watchdog is disabled
		// (or was never constructed) the signal falls back to the passive
		// tracked-campaigns view — a disabled watchdog must not misreport "no
		// active drop campaign" while campaigns are tracked and progressing.
		if m.progressWatchdog != nil && m.progressWatchdog.Snapshot().Enabled {
			m.healthCenter.Record(m.progressWatchdog.ProgressSignal(now))
		} else {
			prog := health.Signal{Name: health.SignalDropsProgress, CheckedAt: now, Status: health.StatusIdle, Detail: "no active drop campaign"}
			if st.TrackedCampaigns > 0 {
				prog.Status = health.StatusOK
				prog.Detail = fmt.Sprintf("%d active campaign(s) tracked (stall watchdog disabled)", st.TrackedCampaigns)
			}
			m.healthCenter.Record(prog)
		}
	}
}

// DropProgress exposes the progress watchdog's published per-drop state for
// the Drops page badges (web.DropProgressProvider).
func (m *Miner) DropProgress() health.ProgressSnapshot {
	if m.progressWatchdog == nil {
		return health.ProgressSnapshot{}
	}
	return m.progressWatchdog.Snapshot()
}

const (
	// degradeReconnectThreshold / degradeGQLFailureThreshold are how many PubSub
	// reconnects / exhausted GQL cycles within the connection-timeout window mark
	// the link "degraded" (yellow), short of the full staleness that marks it
	// "lost" (red). Two is deliberately above the single routine reconnect Twitch
	// periodically requests, and above a single transient GQL cycle (which
	// already absorbs gqlMaxRetries+1 attempts) — so it flags a genuine
	// flapping/failing pattern, not a one-off blip.
	degradeReconnectThreshold  = 2
	degradeGQLFailureThreshold = 2
)

// stalenessSignal builds an OK/degraded/failed/unknown signal. It reports
// StatusFailed once the last-success timestamp is older than threshold (full
// blackout); otherwise, if the transport has accumulated failCount trouble
// events (reconnects / exhausted request cycles) at or above degradeThreshold
// within the window, it reports StatusDegraded; otherwise StatusOK.
func stalenessSignal(name string, last, now time.Time, threshold time.Duration, staleDetail string, failCount, degradeThreshold int, degradeDetail string) health.Signal {
	sig := health.Signal{Name: name, CheckedAt: last}
	switch {
	case last.IsZero():
		sig.Status = health.StatusUnknown
	case now.Sub(last) > threshold:
		sig.Status = health.StatusFailed
		sig.Detail = staleDetail
		sig.ErrorCode = "stale"
	case degradeThreshold > 0 && failCount >= degradeThreshold:
		sig.Status = health.StatusDegraded
		sig.Detail = degradeDetail
		sig.ErrorCode = "degraded"
	default:
		sig.Status = health.StatusOK
	}
	return sig
}

// pubsubSignal composes the PubSub health signal from per-connection state
// rather than the pool-wide max-PONG, so a single stuck connection is not masked
// by its healthy siblings. It flags two distinct failure modes, each naming the
// offending index:
//
//   - dead socket: an open, non-reconnecting connection whose last PONG is older
//     than the staleness threshold (the socket is gone but the pool-wide max is
//     kept fresh by other connections);
//   - lost topics: an open, non-reconnecting connection carrying zero topics
//     while the pool holds more than one connection. A second connection exists
//     only because >50 topics are subscribed (MaxTopicsPerConnection), so a
//     topic-less member has silently dropped its subscriptions — the exact
//     zombie a failed reconnect used to produce. This one is invisible to any
//     PONG-based check because a topic-less socket still ponds normally.
//
// A connection mid-reconnect is expected to be briefly quiet and is never
// flagged. With no connections yet (no topics submitted) it falls back to the
// pool-wide staleness view, which reports Unknown on a zero timestamp.
func pubsubSignal(conns []pubsub.ConnState, lastActivity, now time.Time, threshold time.Duration, reconnects int) health.Signal {
	if len(conns) == 0 {
		return stalenessSignal(health.SignalPubSub, lastActivity, now, threshold,
			"no PubSub activity recently",
			reconnects, degradeReconnectThreshold, "frequent PubSub reconnects")
	}

	multi := len(conns) > 1
	for _, c := range conns {
		if c.Reconnecting || c.Closed {
			continue
		}
		if !c.LastPong.IsZero() && now.Sub(c.LastPong) > threshold {
			return health.Signal{
				Name:      health.SignalPubSub,
				CheckedAt: now,
				Status:    health.StatusFailed,
				Detail:    fmt.Sprintf("connection index=%d has received no PONG for over %s", c.Index, threshold),
				ErrorCode: "connection_stale",
			}
		}
		if multi && c.Topics == 0 {
			return health.Signal{
				Name:      health.SignalPubSub,
				CheckedAt: now,
				Status:    health.StatusStalled,
				Detail:    fmt.Sprintf("connection index=%d is subscribed to 0 topics — subscriptions were lost", c.Index),
				ErrorCode: "topics_lost",
			}
		}
	}

	// No per-index hard failure, but frequent reconnects across the window mark
	// the link impaired (yellow) — short of the full staleness that would make it
	// failed. Sits between the per-index red checks above and the healthy path, so
	// a dead/topic-less connection still wins.
	if reconnects >= degradeReconnectThreshold {
		return health.Signal{
			Name:      health.SignalPubSub,
			CheckedAt: lastActivity,
			Status:    health.StatusDegraded,
			Detail:    fmt.Sprintf("frequent reconnects (%d) in the last %s", reconnects, threshold),
			ErrorCode: "degraded",
		}
	}

	return health.Signal{Name: health.SignalPubSub, CheckedAt: lastActivity, Status: health.StatusOK}
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
	if m.progressWatchdog != nil {
		m.progressWatchdog.UpdateSettings(healthWatchdogConfig(applied))
	}
	slog.Info("Health settings updated",
		"canaryEnabled", applied.CanaryEnabled, "canaryChannel", applied.CanaryChannel,
		"watchdogEnabled", applied.WatchdogEnabled)
}
