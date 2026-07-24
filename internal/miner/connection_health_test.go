package miner

import (
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/health"
)

// These tests pin the connection-health state machine that replaces the naive
// `apiStale || pubsubStale` watchdog. They are fully deterministic: every clock
// value is passed explicitly and every input fact is synthetic, so there is no
// real network, no Discord, and no wall-clock waiting.

const testThreshold = 5 * time.Minute

// mkNow is a fixed reference instant. Using a constant base keeps the arithmetic
// obvious and deterministic across runs.
func mkNow() time.Time { return time.Date(2026, 7, 24, 1, 0, 0, 0, time.UTC) }

// ---------------------------------------------------------------------------
// classifyAPI — idle must never be a connectivity failure.
// ---------------------------------------------------------------------------

func TestClassifyAPI(t *testing.T) {
	now := mkNow()
	recent := now.Add(-time.Minute)
	old := now.Add(-30 * time.Minute)

	cases := []struct {
		name string
		h    api.APIConnHealth
		want apiConnState
	}{
		{
			name: "never used is unknown",
			h:    api.APIConnHealth{},
			want: apiConnUnknown,
		},
		{
			// C — idle client startup: constructor stamped a success that has aged,
			// zero attempts, zero failures. Must be idle, never down.
			name: "aged constructor success, no attempts, no failures is idle",
			h:    api.APIConnHealth{LastSuccess: old},
			want: apiConnIdle,
		},
		{
			// A — production pattern: used a while ago, quiet now, nothing failing.
			name: "attempted and succeeded long ago, quiet now is idle",
			h:    api.APIConnHealth{LastAttempt: old, LastSuccess: old},
			want: apiConnIdle,
		},
		{
			name: "recent useful success is ok",
			h:    api.APIConnHealth{LastAttempt: recent, LastSuccess: recent},
			want: apiConnOK,
		},
		{
			// D — confirmed sustained transport failures with no success in window.
			name: "transport failing and no recent success is down",
			h:    api.APIConnHealth{LastAttempt: recent, LastSuccess: old, RecentTransportFailures: 2},
			want: apiConnDown,
		},
		{
			// A single transient failure amid successes must NOT degrade — this
			// preserves the pre-existing auto-bet gate's tolerance for an isolated
			// blip (degrade tier is degradeGQLFailureThreshold, not 1).
			name: "one transport failure with recent success is ok not degraded",
			h:    api.APIConnHealth{LastAttempt: recent, LastSuccess: recent, RecentTransportFailures: 1},
			want: apiConnOK,
		},
		{
			name: "one functional failure with recent success is ok not degraded",
			h:    api.APIConnHealth{LastAttempt: recent, LastSuccess: recent, RecentFunctionalFailures: 1},
			want: apiConnOK,
		},
		{
			// Below the degrade bar and no recent success: absorbed as idle — still
			// never down.
			name: "single transport failure without recent success is absorbed (idle)",
			h:    api.APIConnHealth{LastAttempt: recent, LastSuccess: old, RecentTransportFailures: 1},
			want: apiConnIdle,
		},
		{
			// M — a success within the window keeps it out of DOWN even if the
			// failure window still holds recent failures (recovering => degraded).
			name: "transport failing but recent success is degraded not down",
			h:    api.APIConnHealth{LastAttempt: recent, LastSuccess: recent, RecentTransportFailures: 5},
			want: apiConnDegraded,
		},
		{
			// J — functional errors (PQNF/403/top-level GQL) are reachable: degraded,
			// never down (no transport failures recorded).
			name: "functional failures only are degraded not down",
			h:    api.APIConnHealth{LastAttempt: recent, LastSuccess: old, RecentFunctionalFailures: 3},
			want: apiConnDegraded,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyAPI(now, testThreshold, tc.h)
			if got != tc.want {
				t.Fatalf("classifyAPI = %v, want %v (h=%+v)", got, tc.want, tc.h)
			}
		})
	}
}

// L — a real failure followed by pure idle: once the failures age out of the
// window (no new attempts fabricate failures), the API returns to idle, never
// stuck "down" and never fabricating new failures on each tick.
func TestClassifyAPIFailureThenIdle(t *testing.T) {
	now := mkNow()
	// At failure time: 2 transport failures in window, no recent success -> down.
	atFailure := api.APIConnHealth{LastAttempt: now, LastSuccess: now.Add(-30 * time.Minute), RecentTransportFailures: 2}
	if got := classifyAPI(now, testThreshold, atFailure); got != apiConnDown {
		t.Fatalf("at failure: classifyAPI = %v, want down", got)
	}
	// Later, no attempts happened, so the sliding window has drained to zero and
	// lastAttempt is now stale. Must be idle, not down.
	later := now.Add(2 * testThreshold)
	drained := api.APIConnHealth{LastAttempt: now, LastSuccess: now.Add(-30 * time.Minute), RecentTransportFailures: 0}
	if got := classifyAPI(later, testThreshold, drained); got != apiConnIdle {
		t.Fatalf("after failures age out with no new attempts: classifyAPI = %v, want idle", got)
	}
}

// N — threshold boundary is deterministic. A success exactly at the threshold
// edge is still "recent" (<=), just past it is stale.
func TestClassifyAPIThresholdBoundary(t *testing.T) {
	now := mkNow()
	tests := []struct {
		name    string
		success time.Time
		fails   int
		want    apiConnState
	}{
		{"just inside threshold is ok", now.Add(-testThreshold + time.Second), 0, apiConnOK},
		{"exactly at threshold is ok", now.Add(-testThreshold), 0, apiConnOK},
		{"just past threshold with no failures is idle", now.Add(-testThreshold - time.Second), 0, apiConnIdle},
		{"just past threshold with failures is down", now.Add(-testThreshold - time.Second), 2, apiConnDown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := api.APIConnHealth{LastAttempt: now, LastSuccess: tc.success, RecentTransportFailures: tc.fails}
			if got := classifyAPI(now, testThreshold, h); got != tc.want {
				t.Fatalf("classifyAPI = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// classifyConnection — the overall level.
// ---------------------------------------------------------------------------

func healthyAPI(now time.Time) api.APIConnHealth {
	return api.APIConnHealth{LastAttempt: now.Add(-time.Minute), LastSuccess: now.Add(-time.Minute)}
}
func idleAPI(now time.Time) api.APIConnHealth {
	return api.APIConnHealth{LastAttempt: now.Add(-30 * time.Minute), LastSuccess: now.Add(-30 * time.Minute)}
}
func downAPI(now time.Time) api.APIConnHealth {
	return api.APIConnHealth{LastAttempt: now.Add(-time.Minute), LastSuccess: now.Add(-30 * time.Minute), RecentTransportFailures: 3}
}

func activePubSub(now time.Time) time.Time { return now.Add(-time.Minute) }
func stalePubSub(now time.Time) time.Time  { return now.Add(-30 * time.Minute) }

// A — the confirmed production case: API idle, PubSub active. Must be HEALTHY.
func TestClassifyConnectionIdleAPIActivePubSubIsHealthy(t *testing.T) {
	now := mkNow()
	in := connInputs{
		now: now, threshold: testThreshold,
		apiPresent: true, api: idleAPI(now),
		pubsubPresent: true, pubsubLastActivity: activePubSub(now),
	}
	out := classifyConnection(in)
	if out.level != connHealthy {
		t.Fatalf("idle API + active PubSub must be HEALTHY, got level=%v apiState=%v detail=%q",
			out.level, out.apiState, out.detail)
	}
	if out.apiState != apiConnIdle {
		t.Fatalf("apiState = %v, want idle", out.apiState)
	}
}

// D — API confirmed failing, PubSub active. DEGRADED only, never LOST.
func TestClassifyConnectionAPIDownPubSubActiveIsDegraded(t *testing.T) {
	now := mkNow()
	out := classifyConnection(connInputs{
		now: now, threshold: testThreshold,
		apiPresent: true, api: downAPI(now),
		pubsubPresent: true, pubsubLastActivity: activePubSub(now),
	})
	if out.level != connDegraded {
		t.Fatalf("API down + PubSub active must be DEGRADED, got %v", out.level)
	}
	if out.apiState != apiConnDown {
		t.Fatalf("apiState = %v, want down", out.apiState)
	}
}

// E — API healthy, PubSub stale. DEGRADED only, never LOST.
func TestClassifyConnectionAPIOKPubSubStaleIsDegraded(t *testing.T) {
	now := mkNow()
	out := classifyConnection(connInputs{
		now: now, threshold: testThreshold,
		apiPresent: true, api: healthyAPI(now),
		pubsubPresent: true, pubsubLastActivity: stalePubSub(now),
	})
	if out.level != connDegraded {
		t.Fatalf("API ok + PubSub stale must be DEGRADED, got %v", out.level)
	}
	if !out.pubsubDown {
		t.Fatalf("pubsubDown = false, want true")
	}
}

// F — BOTH confirmed unavailable is the only path to LOST.
func TestClassifyConnectionBothDownIsLost(t *testing.T) {
	now := mkNow()
	out := classifyConnection(connInputs{
		now: now, threshold: testThreshold,
		apiPresent: true, api: downAPI(now),
		pubsubPresent: true, pubsubLastActivity: stalePubSub(now),
	})
	if out.level != connLost {
		t.Fatalf("API down + PubSub down must be LOST, got %v", out.level)
	}
}

// J — functional GQL errors (reachable) with active PubSub are degraded, never lost.
func TestClassifyConnectionFunctionalErrorsNotLost(t *testing.T) {
	now := mkNow()
	funcErr := api.APIConnHealth{LastAttempt: now.Add(-time.Minute), LastSuccess: now.Add(-30 * time.Minute), RecentFunctionalFailures: 4}
	out := classifyConnection(connInputs{
		now: now, threshold: testThreshold,
		apiPresent: true, api: funcErr,
		pubsubPresent: true, pubsubLastActivity: activePubSub(now),
	})
	if out.level == connLost {
		t.Fatalf("functional GQL errors must not be LOST")
	}
	if out.apiState != apiConnDegraded {
		t.Fatalf("apiState = %v, want degraded", out.apiState)
	}
}

// K — an authentication failure that produced no transport failures must not be
// classified as a network blackout: with active PubSub it can never be LOST.
func TestClassifyConnectionAuthFailureNotLost(t *testing.T) {
	now := mkNow()
	// 401 handling records an attempt but no transport failure (auth lifecycle).
	authOnly := api.APIConnHealth{LastAttempt: now.Add(-time.Minute), LastSuccess: now.Add(-30 * time.Minute)}
	out := classifyConnection(connInputs{
		now: now, threshold: testThreshold,
		apiPresent: true, api: authOnly,
		pubsubPresent: true, pubsubLastActivity: stalePubSub(now),
	})
	if out.level == connLost {
		t.Fatalf("auth-only failure (no transport failures) must not be LOST even with stale PubSub")
	}
}

// A/B — walk a multi-hour timeline of idle-with-occasional-success while PubSub
// stays active: the level must be HEALTHY on every single tick (no LOST, ever),
// independent of how many thresholds elapse (N8).
func TestClassifyConnectionProductionTimelineNeverLost(t *testing.T) {
	base := mkNow()
	// A useful success every 20 minutes; between them, no attempts at all.
	lastSuccess := base
	for min := 0; min <= 6*60; min++ { // six hours, minute by minute
		now := base.Add(time.Duration(min) * time.Minute)
		if min%20 == 0 {
			lastSuccess = now // the periodic keepalive success
		}
		in := connInputs{
			now: now, threshold: testThreshold,
			apiPresent: true,
			api:        api.APIConnHealth{LastAttempt: lastSuccess, LastSuccess: lastSuccess},
			// PubSub PONGs keep it fresh throughout.
			pubsubPresent: true, pubsubLastActivity: now.Add(-30 * time.Second),
		}
		if out := classifyConnection(in); out.level == connLost {
			t.Fatalf("minute %d: production idle/keepalive timeline must never be LOST (apiState=%v)", min, out.apiState)
		}
	}
}

// ---------------------------------------------------------------------------
// Transitions — dedupe, partial vs full recovery, restored only on full.
// ---------------------------------------------------------------------------

func lostOutcome(now time.Time) connOutcome {
	return classifyConnection(connInputs{
		now: now, threshold: testThreshold,
		apiPresent: true, api: downAPI(now),
		pubsubPresent: true, pubsubLastActivity: stalePubSub(now),
	})
}
func degradedOutcome(now time.Time) connOutcome {
	return classifyConnection(connInputs{
		now: now, threshold: testThreshold,
		apiPresent: true, api: downAPI(now),
		pubsubPresent: true, pubsubLastActivity: activePubSub(now),
	})
}
func healthyOutcome(now time.Time) connOutcome {
	return classifyConnection(connInputs{
		now: now, threshold: testThreshold,
		apiPresent: true, api: healthyAPI(now),
		pubsubPresent: true, pubsubLastActivity: activePubSub(now),
	})
}

// Entering LOST from healthy notifies exactly once.
func TestTransitionEnterLostNotifiesOnce(t *testing.T) {
	now := mkNow()
	st, tr := nextConnTransition(connHealthState{}, lostOutcome(now))
	if st.level != connLost {
		t.Fatalf("level = %v, want lost", st.level)
	}
	if !tr.notifyLost {
		t.Fatalf("entering LOST must notify lost")
	}
	if tr.notifyRestored {
		t.Fatalf("entering LOST must not notify restored")
	}
}

// G — repeated watchdog ticks while remaining LOST notify only once.
func TestTransitionLostDedupe(t *testing.T) {
	now := mkNow()
	st, _ := nextConnTransition(connHealthState{}, lostOutcome(now))
	for i := 0; i < 10; i++ {
		var tr connTransition
		st, tr = nextConnTransition(st, lostOutcome(now.Add(time.Duration(i)*time.Minute)))
		if tr.notifyLost {
			t.Fatalf("tick %d: duplicate lost notification while staying LOST", i)
		}
		if st.level != connLost {
			t.Fatalf("tick %d: level drifted from lost to %v", i, st.level)
		}
	}
}

// H — partial recovery (LOST -> DEGRADED) must not send a restored notification
// (it would falsely claim both API and PubSub are back).
func TestTransitionPartialRecoveryNoRestored(t *testing.T) {
	now := mkNow()
	st, _ := nextConnTransition(connHealthState{}, lostOutcome(now))
	st, tr := nextConnTransition(st, degradedOutcome(now))
	if st.level != connDegraded {
		t.Fatalf("partial recovery level = %v, want degraded", st.level)
	}
	if tr.notifyRestored {
		t.Fatalf("partial recovery (LOST->DEGRADED) must NOT notify restored")
	}
	if tr.notifyLost {
		t.Fatalf("partial recovery must not re-notify lost")
	}
}

// I — full recovery sends exactly one restored notification, and only on
// reaching HEALTHY (not on the intermediate partial step).
func TestTransitionFullRecoveryRestoredOnce(t *testing.T) {
	now := mkNow()
	st, _ := nextConnTransition(connHealthState{}, lostOutcome(now)) // -> LOST
	st, trPartial := nextConnTransition(st, degradedOutcome(now))    // -> DEGRADED (partial)
	if trPartial.notifyRestored {
		t.Fatalf("no restored on partial step")
	}
	st, trFull := nextConnTransition(st, healthyOutcome(now)) // -> HEALTHY
	if st.level != connHealthy {
		t.Fatalf("final level = %v, want healthy", st.level)
	}
	if !trFull.notifyRestored {
		t.Fatalf("full recovery must notify restored exactly once")
	}
	// A subsequent healthy tick must not re-notify.
	_, trSteady := nextConnTransition(st, healthyOutcome(now))
	if trSteady.notifyRestored {
		t.Fatalf("steady healthy must not re-notify restored")
	}
}

// Direct full recovery (LOST -> HEALTHY, no intermediate partial) also sends
// exactly one restored notification.
func TestTransitionLostDirectlyToHealthyRestoredOnce(t *testing.T) {
	now := mkNow()
	st, _ := nextConnTransition(connHealthState{}, lostOutcome(now)) // -> LOST
	st, tr := nextConnTransition(st, healthyOutcome(now))            // -> HEALTHY
	if st.level != connHealthy {
		t.Fatalf("level = %v, want healthy", st.level)
	}
	if !tr.notifyRestored {
		t.Fatalf("LOST->HEALTHY must notify restored")
	}
}

// A degraded->healthy transition (that never went through LOST) must not send a
// restored notification, since no lost alert was ever sent.
func TestTransitionDegradedToHealthyNoRestored(t *testing.T) {
	now := mkNow()
	st, _ := nextConnTransition(connHealthState{}, degradedOutcome(now))
	st, tr := nextConnTransition(st, healthyOutcome(now))
	if st.level != connHealthy {
		t.Fatalf("level = %v, want healthy", st.level)
	}
	if tr.notifyRestored {
		t.Fatalf("degraded->healthy (no prior lost) must not notify restored")
	}
}

// ---------------------------------------------------------------------------
// P — message accuracy.
// ---------------------------------------------------------------------------

func TestMessageAccuracy(t *testing.T) {
	now := mkNow()

	// Idle GQL signal must never claim the API is unavailable or that there was
	// no successful response (N10), and must read healthy/idle.
	idleSig := apiConnSignal(apiConnIdle, idleAPI(now), now)
	if idleSig.Status == health.StatusFailed {
		t.Fatalf("idle GQL signal must not be FAILED")
	}
	low := strings.ToLower(idleSig.Detail)
	for _, bad := range []string{"unavailable", "no successful api response", "blackout"} {
		if strings.Contains(low, bad) {
			t.Fatalf("idle GQL detail must not say %q, got %q", bad, idleSig.Detail)
		}
	}

	// A confirmed-down GQL signal is FAILED and truthfully references failures.
	downSig := apiConnSignal(apiConnDown, downAPI(now), now)
	if downSig.Status != health.StatusFailed {
		t.Fatalf("down GQL signal must be FAILED, got %q", downSig.Status)
	}

	// The LOST detail must name both impaired paths and must never claim
	// harvesting was paused (the code does not pause harvesting).
	lost := lostOutcome(now)
	ld := strings.ToLower(lost.lostDetail)
	if ld == "" {
		t.Fatalf("lost detail must be non-empty")
	}
	if strings.Contains(ld, "harvesting paused") || strings.Contains(ld, "harvesting has resumed") {
		t.Fatalf("lost detail must not fabricate a harvesting pause/resume, got %q", lost.lostDetail)
	}
	namesAPI := strings.Contains(ld, "api")
	namesPeer := strings.Contains(ld, "pubsub") || strings.Contains(ld, "connectivity")
	if !namesAPI || !namesPeer {
		t.Fatalf("lost detail must name both impaired paths, got %q", lost.lostDetail)
	}

	// The DEGRADED (single-path) detail must be truthful and non-empty.
	deg := degradedOutcome(now)
	if strings.TrimSpace(deg.degradedDetail) == "" {
		t.Fatalf("degraded detail must be non-empty")
	}
	if strings.Contains(strings.ToLower(deg.degradedDetail), "harvesting paused") {
		t.Fatalf("degraded detail must not claim harvesting paused, got %q", deg.degradedDetail)
	}
}
