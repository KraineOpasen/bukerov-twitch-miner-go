package miner

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/health"
)

// This file holds the connection-health state machine. It replaces the old
// naive `apiStale || pubsubStale` watchdog logic, which mistook a normal idle
// API (no requests attempted) for a connectivity outage and raised false
// "Connection lost" alerts. The core rule: an API is only "down" when requests
// were actually attempted AND failed at the transport layer — the mere passage
// of time with no requests is IDLE, never LOST.

// apiConnState is the classified state of the GQL API connectivity path.
type apiConnState int

const (
	apiConnUnknown  apiConnState = iota // never attempted, no recorded success
	apiConnIdle                         // used before, quiet now, nothing failing
	apiConnOK                           // recent useful success
	apiConnDegraded                     // recent failures (transport below the down bar, or functional/reachable errors)
	apiConnDown                         // sustained transport failures with no success in the window
)

// apiDownFailThreshold is how many exhausted transport-failure cycles within the
// window, combined with no successful response in that window, mark the API
// path as confirmed unavailable. Reuses the degrade threshold so "down" is
// distinguished from "degraded" by the absence of any recent success, not by a
// different count.
const apiDownFailThreshold = degradeGQLFailureThreshold

// classifyAPI maps an immutable API health snapshot to a connectivity state. It
// is deliberately NOT staleness-based: idle (no attempts, no failures) is never
// down, no matter how old the last success is.
func classifyAPI(now time.Time, threshold time.Duration, h api.APIConnHealth) apiConnState {
	recentSuccess := !h.LastSuccess.IsZero() && now.Sub(h.LastSuccess) <= threshold
	transportFailing := h.RecentTransportFailures >= apiDownFailThreshold
	// Degraded uses the SAME failure-count bar as the pre-existing Health-Center
	// degrade tier (degradeGQLFailureThreshold): a single transient GQL cycle is
	// absorbed, not flagged. This keeps the fix surgical — it does not change the
	// auto-bet gate's tolerance for an isolated blip, only removes the idle false
	// positive.
	degradedEvidence := h.RecentTransportFailures >= degradeGQLFailureThreshold ||
		h.RecentFunctionalFailures >= degradeGQLFailureThreshold
	everUsed := !h.LastAttempt.IsZero() || !h.LastSuccess.IsZero()

	switch {
	case transportFailing && !recentSuccess:
		return apiConnDown
	case degradedEvidence:
		return apiConnDegraded
	case recentSuccess:
		return apiConnOK
	case !everUsed:
		return apiConnUnknown
	default:
		return apiConnIdle
	}
}

// connLevel is the overall connection-health level surfaced to the operator.
type connLevel int

const (
	connHealthy  connLevel = iota // nothing confirmed wrong (idle is healthy)
	connDegraded                  // impaired but at least one critical path is fine
	connLost                      // BOTH critical paths confirmed unavailable
)

// connInputs is the immutable snapshot fed to classifyConnection.
type connInputs struct {
	now       time.Time
	threshold time.Duration

	apiPresent bool
	api        api.APIConnHealth

	pubsubPresent      bool
	pubsubLastActivity time.Time
	pubsubReconnects   int
}

// connOutcome is the classification result.
type connOutcome struct {
	level          connLevel
	apiState       apiConnState
	pubsubDown     bool
	pubsubDegraded bool
	detail         string // detail for the current level (lost or degraded), "" when healthy
	lostDetail     string
	degradedDetail string
}

// classifyConnection combines the API and PubSub paths. LOST requires positive
// evidence that BOTH paths are unavailable; a single failed path is only
// DEGRADED (no Discord "connection lost").
func classifyConnection(in connInputs) connOutcome {
	out := connOutcome{apiState: apiConnUnknown}
	if in.apiPresent {
		out.apiState = classifyAPI(in.now, in.threshold, in.api)
	}
	apiDown := out.apiState == apiConnDown
	apiImpaired := out.apiState == apiConnDegraded

	if in.pubsubPresent {
		if !in.pubsubLastActivity.IsZero() && in.now.Sub(in.pubsubLastActivity) > in.threshold {
			out.pubsubDown = true
		} else if in.pubsubReconnects >= degradeReconnectThreshold {
			out.pubsubDegraded = true
		}
	}

	switch {
	case apiDown && out.pubsubDown:
		out.level = connLost
		out.lostDetail = connLostDetail(in.threshold)
		out.detail = out.lostDetail
	case apiDown || out.pubsubDown || apiImpaired || out.pubsubDegraded:
		out.level = connDegraded
		out.degradedDetail = connDegradedDetail(out, in.threshold)
		out.detail = out.degradedDetail
	default:
		out.level = connHealthy
	}
	return out
}

// connLostDetail is the truthful LOST banner/notification detail. LOST is only
// reached when BOTH paths are confirmed unavailable, so it always names both.
// It does NOT claim harvesting was "paused": no code path pauses harvesting; a
// genuine two-path blackout simply interrupts it.
func connLostDetail(threshold time.Duration) string {
	m := int(threshold.Minutes())
	return fmt.Sprintf("Repeated Twitch API request failures and no PubSub activity for over %d minutes — connectivity lost; harvesting may be interrupted.", m)
}

// connDegradedDetail is the truthful DEGRADED detail, naming the impaired
// path(s) without ever claiming a full outage or a harvesting pause.
func connDegradedDetail(out connOutcome, threshold time.Duration) string {
	m := int(threshold.Minutes())
	apiDown := out.apiState == apiConnDown
	apiImpaired := out.apiState == apiConnDegraded
	switch {
	case apiDown && out.pubsubDegraded:
		return fmt.Sprintf("Repeated Twitch API request failures and frequent PubSub reconnects in the last %d minutes; PubSub still active.", m)
	case apiDown:
		return "Repeated Twitch API request failures; PubSub still active."
	case out.pubsubDown && apiImpaired:
		return fmt.Sprintf("No PubSub activity for over %d minutes; the Twitch API is also intermittently failing.", m)
	case out.pubsubDown:
		return fmt.Sprintf("No PubSub activity for over %d minutes; the Twitch API is still responding.", m)
	case apiImpaired && out.pubsubDegraded:
		return fmt.Sprintf("Intermittent Twitch API failures and frequent PubSub reconnects in the last %d minutes.", m)
	case apiImpaired:
		return fmt.Sprintf("Repeated Twitch API request failures in the last %d minutes.", m)
	case out.pubsubDegraded:
		return fmt.Sprintf("Frequent PubSub reconnects in the last %d minutes.", m)
	default:
		return "Twitch connectivity impaired."
	}
}

// apiConnSignal composes the Health Center's gql_api Signal from the classified
// API state. Idle reads as StatusIdle (healthy for the bet gate and dashboard) —
// never StatusFailed — and its detail never claims the API is unavailable or
// that there was "no successful API response", which would be untrue during a
// normal quiet period.
func apiConnSignal(state apiConnState, h api.APIConnHealth, now time.Time) health.Signal {
	sig := health.Signal{Name: health.SignalGQLAPI, CheckedAt: now}
	if !h.LastSuccess.IsZero() {
		sig.CheckedAt = h.LastSuccess
	}
	switch state {
	case apiConnOK:
		sig.Status = health.StatusOK
	case apiConnIdle:
		sig.Status = health.StatusIdle
		sig.Detail = "No API requests recently; connection idle."
	case apiConnDegraded:
		sig.Status = health.StatusDegraded
		sig.Detail = "Recent Twitch API request failures."
		sig.ErrorCode = "degraded"
	case apiConnDown:
		sig.Status = health.StatusFailed
		sig.Detail = "Repeated Twitch API request failures with no successful response."
		sig.ErrorCode = "unreachable"
	default: // apiConnUnknown
		sig.Status = health.StatusUnknown
	}
	return sig
}

// connHealthState is the persistent transition state owned by the single
// watchdog goroutine. lostAlertActive tracks whether a "connection lost"
// notification has been sent and not yet cleared by a full recovery, so that a
// LOST -> DEGRADED (partial) -> HEALTHY sequence still emits exactly one
// "restored" (on reaching HEALTHY), and a partial recovery emits none.
type connHealthState struct {
	level           connLevel
	lostAlertActive bool
}

// connTransition describes the side effects of one classification tick. Empty
// when nothing changed (dedupe).
type connTransition struct {
	notifyLost      bool
	notifyRestored  bool
	enteredLost     bool
	fullRestore     bool
	partialRestore  bool
	enteredDegraded bool
	stabilized      bool
}

// nextConnTransition is the pure transition function. Given the previous state
// and the freshly classified outcome, it returns the next state and the
// transition-only side effects to perform. Notifications are edge-triggered:
// lost on entry (deduped while staying lost), restored only on reaching HEALTHY
// while a lost alert is outstanding.
func nextConnTransition(prev connHealthState, out connOutcome) (connHealthState, connTransition) {
	next := connHealthState{level: out.level, lostAlertActive: prev.lostAlertActive}
	var tr connTransition

	switch out.level {
	case connLost:
		if prev.level != connLost {
			tr.enteredLost = true
			tr.notifyLost = true
			next.lostAlertActive = true
		}
	case connHealthy:
		if prev.lostAlertActive {
			tr.fullRestore = true
			tr.notifyRestored = true
			next.lostAlertActive = false
		} else if prev.level == connDegraded {
			tr.stabilized = true
		}
	case connDegraded:
		if prev.lostAlertActive {
			if prev.level == connLost {
				tr.partialRestore = true
			}
			// lostAlertActive stays true: still impaired after a lost episode.
		} else if prev.level == connHealthy {
			tr.enteredDegraded = true
		}
	}
	return next, tr
}

// evaluateConnectionHealth is called once per watchdog tick. It captures a
// snapshot of the API and PubSub connectivity, classifies it, applies the
// resulting transition (logs, Discord notifications, dashboard banner, debug
// fields), and advances the state. No notification or web I/O is performed while
// holding m.mu.
func (m *Miner) evaluateConnectionHealth(now time.Time, state *connHealthState) {
	m.mu.RLock()
	threshold := time.Duration(m.config.RateLimits.ConnectionTimeoutMinutes) * time.Minute
	m.mu.RUnlock()

	in := connInputs{now: now, threshold: threshold}
	if m.client != nil {
		in.apiPresent = true
		in.api = m.client.ConnHealth(now, threshold)
	}
	if m.wsPool != nil {
		in.pubsubPresent = true
		in.pubsubLastActivity = m.wsPool.LastActivity()
		in.pubsubReconnects = m.wsPool.RecentReconnects(threshold)
	}

	out := classifyConnection(in)
	prevLevel := state.level
	newState, tr := nextConnTransition(*state, out)
	*state = newState

	minutes := int(threshold.Minutes())
	switch {
	case tr.enteredLost:
		slog.Error("Connection lost", "apiState", out.apiState, "pubsubDown", out.pubsubDown, "thresholdMinutes", minutes)
	case tr.fullRestore:
		slog.Info("Connection restored")
	case tr.partialRestore:
		slog.Warn("Connection partially restored", "detail", out.degradedDetail)
	case tr.enteredDegraded:
		slog.Warn("Connection degraded", "detail", out.degradedDetail)
	case tr.stabilized:
		slog.Info("Connection stabilized")
	}

	// Transition-only Discord notifications, performed outside any miner lock.
	if tr.notifyLost && m.notifications != nil {
		m.notifications.NotifyConnectionLost(out.lostDetail)
	}
	if tr.notifyRestored && m.notifications != nil {
		m.notifications.NotifyConnectionRestored()
	}

	// Reflect the current level in the miner fields + dashboard banner, but only
	// when the level actually changed, to avoid redundant SSE broadcasts.
	if newState.level != prevLevel {
		m.applyConnectionLevel(newState.level, out)
	}
}

// applyConnectionLevel sets the miner's connection fields (read by the debug
// snapshot) under the lock, then updates the dashboard banner outside the lock.
func (m *Miner) applyConnectionLevel(level connLevel, out connOutcome) {
	lost := level == connLost
	degraded := level == connDegraded
	lostDetail := ""
	degradedDetail := ""
	if lost {
		lostDetail = out.lostDetail
	}
	if degraded {
		degradedDetail = out.degradedDetail
	}

	m.mu.Lock()
	m.connectionLost = lost
	m.connectionDetail = lostDetail
	m.connectionDegraded = degraded
	m.connectionDegradedDetail = degradedDetail
	m.mu.Unlock()

	if m.webServer != nil {
		b := m.webServer.GetStatusBroadcaster()
		b.SetConnectionLost(lost, lostDetail)
		b.SetConnectionDegraded(degraded, degradedDetail)
	}
}
