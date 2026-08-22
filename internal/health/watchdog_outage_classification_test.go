package health

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/watcher"
)

// --- canary self-timeout vs. watchdog outage gate (producer→consumer) ---
//
// These tests drive the REAL producer→consumer chain through one shared
// Center: a real Canary whose probe exceeds its own local deadline records
// the watch_transport signal, and the real ProgressWatchdog consumes it on
// its next evaluation. A canary-local timeout proves only "the canary did
// not complete before its budget" — it is NOT independent evidence of a
// global Twitch outage, and must not destroy an otherwise valid drop-stall
// evidence window.

// timedOutCanary builds a real Canary sharing the harness's Center whose
// probe deterministically exceeds its own local deadline: CheckStreamerOnline
// is context-unaware and blocks on a gate until the test joins it in cleanup,
// so runDetached abandons it once the (shortened, test-only) canary timeout
// fires. The recorded signal is the owner-identified exemplar: StatusFailed,
// Stage "stream_info", ErrorCode "timeout".
func timedOutCanary(t *testing.T, center *Center) *Canary {
	t.Helper()
	gate, done := make(chan struct{}), make(chan struct{})
	client := &fakeClient{channelID: "cid", online: true, spade: "http://spade.test/x", onlineGate: gate, onlineDone: done}
	c := newCanary(center, client, &fakeProber{res: watcher.ProbeResult{OK: true}}, &fakeNotifier{}, nil)
	c.timeout = 20 * time.Millisecond
	t.Cleanup(func() { close(gate); <-done })
	return c
}

// TestWatchdogCanarySelfTimeoutPreservesStallEvidence is the regression for
// the semantic conflation: partial but valid stall evidence has accrued
// (delay window open, clean observations, delivered reports), then the
// canary exceeds ITS OWN local timeout and records the failure into the
// shared Center. The watchdog's next pass must NOT discard the accumulated
// evidence window on that inconclusive, canary-local result alone.
//
// On the pre-fix base this fails: twitchOutage classified the retained
// timeout as a Twitch outage, gatesHold refused confirmation, and
// resetEvidence zeroed NoProgressObs/ReportsSinceProgress (the failure
// output shows the reset state and the gate's "connectivity is degraded
// (watch_transport failing)" detail).
func TestWatchdogCanarySelfTimeoutPreservesStallEvidence(t *testing.T) {
	h := newWatchdogHarness(t)
	canary := timedOutCanary(t, h.center)

	// Partial but valid stall evidence: 20m delay, 2 clean observations,
	// 6 delivered reports (thresholds: 20m / 3 obs / 5 reports).
	stallReady(t, h)
	if st := h.state(t); st.NoProgressObs != 2 || st.ReportsSinceProgress != 6 {
		t.Fatalf("setup: expected partial evidence (2 obs, 6 reports), got %+v", st)
	}

	// The canary self-timeout lands in the SAME Center the watchdog reads.
	canary.runOnce(true)
	sig, ok := h.center.Signal(SignalWatchTransport)
	if !ok || sig.Status != StatusFailed || sig.ErrorCode != "timeout" || sig.Stage != "stream_info" {
		t.Fatalf("setup: expected a recorded canary self-timeout, got ok=%v sig=%+v", ok, sig)
	}

	// Next watchdog pass: another clean observation with reports flowing.
	h.tick(10*time.Minute, true, 3)
	st := h.state(t)
	if st.NoProgressObs < 2 || st.ReportsSinceProgress < 6 {
		t.Fatalf("a canary-local self-timeout alone destroyed the stall evidence window, got %+v", st)
	}
}

// TestWatchdogWatchTransportProvenanceDecisionTable pins the complete
// classification through the watchdog's public behavior: every deny-listed
// inconclusive/channel-local/local-abort code must NOT gate (the genuine
// stall confirms on schedule), and every trustworthy code — HTTP-bearing
// probe failures, genuine status-less network failures, bare failures with no
// code, unknown future codes — must keep gating AND keep resetting the
// accrued evidence (the conservative pre-existing behavior).
func TestWatchdogWatchTransportProvenanceDecisionTable(t *testing.T) {
	cases := []struct {
		code  string
		gates bool
	}{
		// canary-local budget/lifecycle aborts
		{"timeout", false},
		{"cancelled", false},
		// canary-channel-local conditions
		{"channel_offline", false},
		{"channel_resolve_failed", false},
		{"spade_url_missing", false},
		// explicitly inconclusive checks (suffix = models.StatusReason)
		{"stream_status_graphql_error", false},
		{"stale_session_error", false},
		{"session_snapshot_error", false},
		// trustworthy outage evidence — conservative gate preserved
		{"", true}, // bare StatusFailed (no provenance): default-gate
		{"beacon_http_503", true},
		{"playlist_http_403", true},
		{"segment_http_404", true},
		{"playback_token_error", true},
		{"playlist_error", true},
		{"segment_error", true},
		{"beacon_error", true},
		{"some_future_unknown_code", true},
	}
	for _, tc := range cases {
		name := tc.code
		if name == "" {
			name = "bare_failed"
		}
		t.Run(name, func(t *testing.T) {
			h := newWatchdogHarness(t)
			stallReady(t, h)
			h.center.Record(Signal{Name: SignalWatchTransport, Status: StatusFailed, ErrorCode: tc.code, CheckedAt: h.now})
			h.tick(10*time.Minute, true, 3)
			if tc.gates {
				assertNoRecovery(t, h, "connectivity is degraded")
				if st := h.state(t); st.NoProgressObs != 0 || st.ReportsSinceProgress != 0 {
					t.Fatalf("trustworthy outage evidence must reset the stall evidence, got %+v", st)
				}
			} else if _, triggered := h.drops.counts(); triggered != 1 {
				t.Fatalf("an inconclusive %q result must not suppress the genuine stall, got triggered=%d state=%+v",
					tc.code, triggered, h.state(t))
			}
		})
	}
}

// TestWatchdogDegradedWatchTransportStillGates: the degraded arm stays fully
// conservative — even an abort-looking ErrorCode does not carve a degraded
// watch_transport signal out of the outage gate (the carve-out is scoped to
// StatusFailed provenance only).
func TestWatchdogDegradedWatchTransportStillGates(t *testing.T) {
	h := newWatchdogHarness(t)
	stallReady(t, h)
	h.center.Record(Signal{Name: SignalWatchTransport, Status: StatusDegraded, ErrorCode: "timeout", CheckedAt: h.now})
	h.tick(10*time.Minute, true, 3)
	assertNoRecovery(t, h, "connectivity is degraded")
}

// TestWatchdogOAuthFailureStillResetsEvidence: a real account-side outage
// signal keeps both halves of the pre-existing conservative behavior — it
// blocks confirmation AND discards the accrued evidence window.
func TestWatchdogOAuthFailureStillResetsEvidence(t *testing.T) {
	h := newWatchdogHarness(t)
	stallReady(t, h)
	h.center.Record(Signal{Name: SignalOAuth, Status: StatusFailed, ErrorCode: "reauth_required", CheckedAt: h.now})
	h.tick(10*time.Minute, true, 3)
	assertNoRecovery(t, h, "connectivity is degraded")
	if st := h.state(t); st.NoProgressObs != 0 || st.ReportsSinceProgress != 0 {
		t.Fatalf("an OAuth outage must reset the stall evidence, got %+v", st)
	}
}

// TestCanaryBudgetAbortMidProbeRecordsAbortCode pins the producer-side
// normalization: when the prober fails status-less (no HTTP response reached —
// probeFail derives "<stage>_error"/Status 0) and the probe context is already
// dead, only the producer can tell "genuine network failure" from "my own
// budget expired mid-request". The canary must record the abort
// classification (abortReason → "timeout") with the reached stage, not the
// ambiguous stage code the consumer would then treat as outage evidence.
func TestCanaryBudgetAbortMidProbeRecordsAbortCode(t *testing.T) {
	prober := &fakeProber{waitCtx: true} // blocks until ctx.Done, returns a status-less beacon failure
	c := newCanary(NewCenter(), onlineClient(), prober, &fakeNotifier{}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	sig := c.probe(ctx, "canary_chan")

	if sig.Status != StatusFailed {
		t.Fatalf("expected a failure, got %+v", sig)
	}
	if sig.ErrorCode != "timeout" || sig.Stage != string(watcher.StageBeacon) {
		t.Fatalf("a status-less probe failure under an expired canary budget must record the abort classification (stage preserved), got %+v", sig)
	}
	if sig.Detail != "probe exceeded the canary timeout" {
		t.Fatalf("the normalized abort must carry the abort detail, got %q", sig.Detail)
	}
}

// TestCanaryPlaybackTokenFailurePastDeadlineKeepsCode: GetPlaybackAccessToken
// is context-unaware with its own bounded retries, so it can outlive the
// canary budget and still return a genuine GQL verdict — a dead probe ctx is
// NOT causal for that stage (ctxAwareStage), and the real account/API
// evidence must keep its own gating code.
func TestCanaryPlaybackTokenFailurePastDeadlineKeepsCode(t *testing.T) {
	prober := &fakeProber{waitCtx: true, ctxRes: &watcher.ProbeResult{
		Stage: watcher.StagePlaybackToken, ErrorCode: "playback_token_error",
	}}
	c := newCanary(NewCenter(), onlineClient(), prober, &fakeNotifier{}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	sig := c.probe(ctx, "canary_chan")

	if sig.Status != StatusFailed || sig.ErrorCode != "playback_token_error" {
		t.Fatalf("a playback-token failure must keep its gating code past the deadline, got %+v", sig)
	}
}

// TestCanaryCancelledAbortRecordsCancelledCode pins abortReason's non-deadline
// branch through the producer: an explicit cancel (Stop/shutdown) records the
// deny-listed "cancelled" classification.
func TestCanaryCancelledAbortRecordsCancelledCode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := newCanary(NewCenter(), onlineClient(), &fakeProber{res: watcher.ProbeResult{OK: true}}, &fakeNotifier{}, nil)

	sig := c.probe(ctx, "canary_chan")

	if sig.Status != StatusFailed || sig.ErrorCode != "cancelled" {
		t.Fatalf("an explicitly cancelled probe must record the cancelled classification, got %+v", sig)
	}
}

// TestWatchdogRealProducerDenyListCodes proves the reachable deny-listed
// classes end-to-end through the REAL producer→consumer chain (real Canary
// recording into the shared Center, real watchdog evaluating): a
// producer-side code rename can no longer silently reintroduce
// evidence-destroying gating for these classes with the suite green.
// stale_session_error stays synthetic-only by design (the canary's
// single-writer ephemeral streamer cannot change generation mid-probe);
// session_snapshot_error's producer is the real MinuteSender, exercised via
// the live-ctx status-less prober-failure path instead.
func TestWatchdogRealProducerDenyListCodes(t *testing.T) {
	cases := []struct {
		name     string
		client   *fakeClient
		wantCode string // exact match, or prefix when ending in "_"
	}{
		{"channel_offline", &fakeClient{channelID: "cid", online: false}, "channel_offline"},
		{"channel_resolve_failed", &fakeClient{idErr: errors.New("gql said no")}, "channel_resolve_failed"},
		{"spade_url_missing", &fakeClient{channelID: "cid", online: true, spade: ""}, "spade_url_missing"},
		{"stream_status_inconclusive", &fakeClient{channelID: "cid", noTransition: true}, "stream_status_"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newWatchdogHarness(t)
			stallReady(t, h)

			canary := newCanary(h.center, tc.client, &fakeProber{res: watcher.ProbeResult{OK: true}}, &fakeNotifier{}, nil)
			canary.runOnce(true)

			sig, ok := h.center.Signal(SignalWatchTransport)
			if !ok || sig.Status != StatusFailed {
				t.Fatalf("setup: expected a recorded producer failure, got ok=%v sig=%+v", ok, sig)
			}
			if strings.HasSuffix(tc.wantCode, "_") {
				if !strings.HasPrefix(sig.ErrorCode, tc.wantCode) {
					t.Fatalf("expected a %q-prefixed code from the real producer, got %+v", tc.wantCode, sig)
				}
			} else if sig.ErrorCode != tc.wantCode {
				t.Fatalf("expected %q from the real producer, got %+v", tc.wantCode, sig)
			}

			h.tick(10*time.Minute, true, 3)
			if _, triggered := h.drops.counts(); triggered != 1 {
				t.Fatalf("a real-producer %q result must not suppress the genuine stall, got triggered=%d state=%+v",
					sig.ErrorCode, triggered, h.state(t))
			}
		})
	}
}

// TestInconclusiveWatchTransportPredicate pins the predicate's decision table
// directly (the plan's U2 direct test surface), complementing the behavioral
// table above: every deny-listed code and the prefix boundary, against
// representative gating codes.
func TestInconclusiveWatchTransportPredicate(t *testing.T) {
	cases := []struct {
		code         string
		inconclusive bool
	}{
		{"timeout", true},
		{"cancelled", true},
		{"channel_offline", true},
		{"channel_resolve_failed", true},
		{"spade_url_missing", true},
		{"stream_status_initial", true},
		{"stream_status_graphql_error", true},
		{"stale_session_error", true},
		{"session_snapshot_error", true},
		{"", false},
		{"stream_status", false}, // prefix boundary: no underscore suffix
		{"beacon_http_503", false},
		{"playlist_http_403", false},
		{"playback_token_error", false},
		{"playlist_error", false},
		{"segment_error", false},
		{"beacon_error", false},
		{"some_future_unknown_code", false},
	}
	for _, tc := range cases {
		got := inconclusiveWatchTransport(Signal{Name: SignalWatchTransport, Status: StatusFailed, ErrorCode: tc.code})
		if got != tc.inconclusive {
			t.Errorf("inconclusiveWatchTransport(%q) = %v, want %v", tc.code, got, tc.inconclusive)
		}
	}
}

// TestCanaryHTTPFailurePastDeadlineKeepsCode: an HTTP-status-bearing probe
// failure is a remote response on the farming transport — genuine remote
// evidence — and must keep its code even when the canary budget expired while
// it was in flight.
func TestCanaryHTTPFailurePastDeadlineKeepsCode(t *testing.T) {
	prober := &fakeProber{waitCtx: true, ctxRes: &watcher.ProbeResult{
		Stage: watcher.StageBeacon, Status: 503, ErrorCode: "beacon_http_503",
	}}
	c := newCanary(NewCenter(), onlineClient(), prober, &fakeNotifier{}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	sig := c.probe(ctx, "canary_chan")

	if sig.Status != StatusFailed || sig.ErrorCode != "beacon_http_503" {
		t.Fatalf("an HTTP-bearing failure must keep its code past the deadline, got %+v", sig)
	}
}

// TestCanaryStatuslessFailureWithLiveCtxKeepsStageCode: a genuine status-less
// network failure with the probe context still live keeps the stage code — it
// is real transport evidence, not a budget abort.
func TestCanaryStatuslessFailureWithLiveCtxKeepsStageCode(t *testing.T) {
	prober := &fakeProber{res: watcher.ProbeResult{Stage: watcher.StagePlaylist, ErrorCode: "playlist_error"}}
	c := newCanary(NewCenter(), onlineClient(), prober, &fakeNotifier{}, nil)

	sig := c.probe(context.Background(), "canary_chan")

	if sig.Status != StatusFailed || sig.ErrorCode != "playlist_error" || sig.Stage != string(watcher.StagePlaylist) {
		t.Fatalf("a live-ctx status-less failure must keep its stage code, got %+v", sig)
	}
}

// TestWatchdogRetainedCanaryTimeoutDoesNotMasqueradeAsOutage pins the
// persistence half of the defect: Center retains the last watch_transport
// signal until another probe overwrites it, so a single self-timeout used to
// hold the outage gate across every later evaluation. A retained
// inconclusive result must not indefinitely suppress genuine stall
// detection.
//
// Timeline (derived from the harness WatchdogConfig — StallDelay 20m,
// StallConfirmations 3, stallMinReports 5, ticks of 10m with 3 reports):
// the earliest legitimate confirmation is the third observed tick, +30m
// after tracking starts. On the pre-fix base the retained timeout resets
// the evidence on every pass, so confirmation never happens at all until
// some later probe overwrites the signal — with the default 6h canary
// interval (1h is the validated floor) that defers a real stall by at least
// another full interval plus
// a complete fresh evidence window.
func TestWatchdogRetainedCanaryTimeoutDoesNotMasqueradeAsOutage(t *testing.T) {
	h := newWatchdogHarness(t)
	canary := timedOutCanary(t, h.center)

	// The self-timeout is recorded BEFORE any evidence accrues, and no other
	// producer ever overwrites it.
	canary.runOnce(true)
	if sig, ok := h.center.Signal(SignalWatchTransport); !ok || sig.ErrorCode != "timeout" {
		t.Fatalf("setup: expected a retained canary self-timeout, got ok=%v sig=%+v", ok, sig)
	}

	h.w.evaluate(h.now) // tracking starts
	for i := 0; i < 3; i++ {
		h.tick(10*time.Minute, true, 3)
	}

	// The full stall window (delay, observations, reports) accrued while the
	// stale timeout signal was still the latest watch_transport record: the
	// genuine stall must confirm and recovery must start.
	if _, triggered := h.drops.counts(); triggered != 1 {
		st := h.state(t)
		t.Fatalf("a retained canary self-timeout suppressed genuine stall confirmation: triggered=%d state=%+v", triggered, st)
	}
}
