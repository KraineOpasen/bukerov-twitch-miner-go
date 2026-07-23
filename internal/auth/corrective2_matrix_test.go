package auth

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- P2-C1 matrix: device-flow candidate outcomes ---

// P2-C1.4/P2-C1.5: a candidate that validates with an unexpected client ID or
// missing required scopes is unusable — never published.
func TestDeviceFlowAnomalousCandidateNotPublished(t *testing.T) {
	cases := map[string]validateResponse{
		"client-id-mismatch": {
			ClientID: "some-other-client", Login: "tester",
			Scopes: requiredScopes(), UserID: "uid-1", ExpiresIn: 5000,
		},
		"missing-scopes": {
			ClientID: "", Login: "tester", // ClientID filled per-instance below
			Scopes: []string{"chat:read"}, UserID: "uid-1", ExpiresIn: 5000,
		},
	}
	for name, res := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeOAuth(t)
			a := newLifecycleAuth(t, f)
			if res.ClientID == "" {
				res.ClientID = a.clientID
			}
			f.mu.Lock()
			f.validateHandler = func(w http.ResponseWriter, r *http.Request) {
				f.writeJSON(w, 200, res)
			}
			f.mu.Unlock()

			err := a.DeviceFlowLogin(context.Background())
			if !errors.Is(err, ErrAuthProtocol) {
				t.Fatalf("anomalous candidate error = %v, want ErrAuthProtocol", err)
			}
			if a.GetAuthToken() != "" || a.Generation() != 0 {
				t.Fatalf("anomalous candidate was published")
			}
		})
	}
}

// P2-C1.6/P2-C1.8 (Corrective Pass 3): a device-flow candidate stays a private
// candidate under revalidation through a /validate outage longer than the old
// finite budget (5), then promotes within the same flight — the interactive
// grant is never re-prompted and the candidate never leaks before promotion.
func TestDeviceFlowCandidateSurvivesLongValidateOutage(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)

	var validates atomic.Int64
	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) {
		if validates.Add(1) <= 8 {
			// Before promotion the candidate must be invisible to every reader.
			if a.GetAuthToken() != "" || a.Snapshot().AccessToken != "" || a.Generation() != 0 {
				t.Errorf("candidate leaked into active readers during revalidation")
			}
			w.WriteHeader(503)
			return
		}
		validFor(f, a, w)
	}
	f.mu.Unlock()

	if err := a.DeviceFlowLogin(context.Background()); err != nil {
		t.Fatalf("a long transient validate outage must not fail the device flow: %v", err)
	}
	if a.GetAuthToken() != "test-access-df" || a.Generation() != 1 {
		t.Fatalf("device candidate not promoted after the outage cleared")
	}
	if device, _, _, _ := f.counts(); device != 1 {
		t.Fatalf("device flows = %d, want exactly 1 (the granted pair must be reused, not re-prompted)", device)
	}
}

// --- P2-C2 matrix: refresh candidate outcomes ---

// P2-C2.4/P2-C2.5: a definitive candidate rejection durably drops the
// consumed one-time refresh token (memory AND disk), so no restart or later
// recovery can ever re-present it; the next recovery goes to device flow.
func TestRefreshCandidateRejectionDropsConsumedTokenDurably(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	clock := newTestClock()
	a.now = clock.Now
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"
	seedStoredAuth(t, a)

	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) {
		if authHeaderToken(r) == "test-access-2" {
			f.oauthError(w, 401, "invalid access token")
			return
		}
		validFor(f, a, w)
	}
	f.mu.Unlock()

	if _, err := a.Recover(context.Background(), 0); err == nil {
		t.Fatalf("rejected refresh candidate must fail the recovery")
	}
	if a.Health().HasRefreshToken {
		t.Fatalf("consumed refresh token retained in memory after candidate rejection")
	}
	fresh := NewTwitchAuth("tester", "device-xyz")
	if err := fresh.LoadStoredAuth(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if fresh.Health().HasRefreshToken {
		t.Fatalf("consumed refresh token resurrected from disk")
	}

	clock.Advance(time.Hour)
	if _, err := a.Recover(context.Background(), 0); err != nil {
		t.Fatalf("post-rejection recovery: %v", err)
	}
	device, _, refresh, _ := f.counts()
	if refresh != 1 || device != 1 {
		t.Fatalf("post-rejection recovery traffic: refresh=%d device=%d, want 1/1", refresh, device)
	}
}

// P2-C2.6/P2-C2.7 (Corrective Pass 3): while a candidate is under revalidation
// (a permanent /validate outage), the persisted record and every snapshot still
// carry ONLY the old published set — candidate material is never serialized and
// never visible to a reader. The revalidation loop is left running on a
// cancellable context and stopped at the end.
func TestStagedCandidateNeverSerializedOrSnapshotted(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	// A manual (parking) timer so the revalidation loop stages once and then
	// blocks on the paced wait — never a busy spin on the permanent outage.
	mt := newManualTimer()
	a.timerAfter = mt.after
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"
	seedStoredAuth(t, a)

	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }
	f.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _, _ = a.Recover(ctx, 0); close(done) }()

	// Wait until the refresh candidate is privately staged and parked on a
	// paced wait before inspecting the reader-visible state.
	waitCond(t, "candidate to be staged", func() bool {
		a.mu.Lock()
		staged := a.pendingCandidate != nil
		a.mu.Unlock()
		return staged && mt.waitCount() >= 1
	})

	if snap := a.Snapshot(); snap.AccessToken != "test-access-1" || snap.Generation != 0 {
		t.Fatalf("staged candidate visible in Snapshot: %+v", snap)
	}
	if err := a.SaveAuth(); err != nil {
		t.Fatalf("save: %v", err)
	}
	body := readCookieFile(t, a)
	for _, leak := range []string{"test-access-2", "test-refresh-2"} {
		if strings.Contains(body, leak) {
			t.Fatalf("staged candidate material serialized to disk: %q", leak)
		}
	}
	if !strings.Contains(body, "test-refresh-1") {
		t.Fatalf("old published record damaged during staging")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("revalidation owner did not stop on cancellation")
	}
}

// P2-C2.8: a staged candidate whose RE-validation is authoritatively rejected
// is discarded (consumed token dropped) and the following recovery runs
// exactly one device flow.
func TestStagedCandidateRevalidationRejectedFallsToDeviceFlow(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	clock := newTestClock()
	a.now = clock.Now
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"

	// The refresh candidate (test-access-2) validates transiently past the old
	// finite budget (5), then is authoritatively rejected within the same
	// flight -> discarded (consumed refresh dropped) -> the next recovery falls
	// to device flow.
	var refreshValidates atomic.Int64
	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) {
		if authHeaderToken(r) == "test-access-2" {
			if refreshValidates.Add(1) <= 8 {
				w.WriteHeader(503)
			} else {
				f.oauthError(w, 401, "invalid access token")
			}
			return
		}
		validFor(f, a, w) // device candidate belongs to this profile
	}
	f.mu.Unlock()

	if _, err := a.Recover(context.Background(), 0); err == nil {
		t.Fatalf("an authoritative candidate rejection must fail the recovery")
	}
	if a.Health().HasRefreshToken {
		t.Fatalf("consumed refresh token survived the candidate rejection")
	}
	clock.Advance(time.Hour) // clear the per-generation backoff window
	if _, err := a.Recover(context.Background(), 0); err != nil {
		t.Fatalf("device-flow recovery after discard: %v", err)
	}
	device, _, refresh, _ := f.counts()
	if refresh != 1 || device != 1 {
		t.Fatalf("traffic: refresh=%d device=%d, want exactly 1/1", refresh, device)
	}
	if a.GetAuthToken() != "test-access-df" {
		t.Fatalf("device-flow credentials not promoted")
	}
}

// --- P2-C3 matrix: backoff gate behavior ---

// P2-C3.3: the deterministic capped exponential schedule.
func TestRecoveryBackoffSchedule(t *testing.T) {
	want := []time.Duration{
		30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute,
		8 * time.Minute, 10 * time.Minute, 10 * time.Minute,
	}
	for i, w := range want {
		if got := recoveryBackoff(i + 1); got != w {
			t.Fatalf("backoff(%d) = %v, want %v", i+1, got, w)
		}
	}
}

// P2-C3.4: consecutive failures extend the gate exponentially; the attempt
// counter is observable through when attempts are allowed again.
func TestBackoffGrowsAcrossConsecutiveFailures(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	clock := newTestClock()
	a.now = clock.Now
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"
	f.mu.Lock()
	f.refreshHandler = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }
	f.mu.Unlock()

	// Failure 1 -> 30s window.
	if _, err := a.Recover(context.Background(), 0); err == nil {
		t.Fatalf("expected failure 1")
	}
	clock.Advance(29 * time.Second)
	if _, err := a.Recover(context.Background(), 0); !errors.Is(err, ErrRecoveryBackoff) {
		t.Fatalf("attempt inside 30s window not gated: %v", err)
	}
	clock.Advance(2 * time.Second) // 31s total: allowed -> failure 2 -> 60s window
	if _, err := a.Recover(context.Background(), 0); errors.Is(err, ErrRecoveryBackoff) || err == nil {
		t.Fatalf("attempt past the window should run (and fail at the endpoint), got %v", err)
	}
	clock.Advance(45 * time.Second)
	if _, err := a.Recover(context.Background(), 0); !errors.Is(err, ErrRecoveryBackoff) {
		t.Fatalf("second window did not grow to 60s: attempt at +45s ran")
	}
	if _, _, refresh, _ := f.counts(); refresh != 2 {
		t.Fatalf("refresh grants = %d, want exactly 2 (gated attempts must cause zero traffic)", refresh)
	}
}

// P2-C3.5: a successful recovery clears the gate, and a NEW generation is
// never blocked by an old generation's gate.
func TestBackoffClearedOnSuccessAndScopedToGeneration(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	clock := newTestClock()
	a.now = clock.Now
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"

	var failing atomic.Bool
	failing.Store(true)
	f.mu.Lock()
	f.refreshHandler = func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			w.WriteHeader(500)
			return
		}
		f.writeJSON(w, 200, TokenResponse{
			AccessToken: "test-access-2", RefreshToken: "test-refresh-2",
			ExpiresIn: 14000, Scope: requiredScopes(), TokenType: "bearer",
		})
	}
	f.mu.Unlock()

	if _, err := a.Recover(context.Background(), 0); err == nil {
		t.Fatalf("expected seed failure")
	}
	failing.Store(false)
	clock.Advance(time.Minute)
	if _, err := a.Recover(context.Background(), 0); err != nil {
		t.Fatalf("recovery past window: %v", err)
	}
	// Success published generation 1 and cleared the gate: a hypothetical
	// failure of generation 1 must be allowed IMMEDIATELY (no leftover gate).
	failing.Store(true)
	if _, err := a.Recover(context.Background(), 1); errors.Is(err, ErrRecoveryBackoff) {
		t.Fatalf("old generation's gate blocked the new generation")
	}
}

// P2-C3.6: a stale rejection during an armed gate neither trips the gate nor
// resets it — it returns the current snapshot with zero traffic.
func TestStaleRejectionDoesNotTouchGate(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	clock := newTestClock()
	a.now = clock.Now
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"

	// Publish generation 1, then fail a recovery of generation 1 to arm the
	// gate for it.
	a.publishTokenPair(&TokenResponse{
		AccessToken: "test-access-2", RefreshToken: "test-refresh-2",
		ExpiresIn: 14000, Scope: requiredScopes(), TokenType: "bearer",
	})
	f.mu.Lock()
	f.refreshHandler = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }
	f.mu.Unlock()
	if _, err := a.Recover(context.Background(), 1); err == nil {
		t.Fatalf("expected gate-arming failure")
	}
	refreshesBefore := func() int { _, _, r, _ := f.counts(); return r }()

	// Stale rejection (generation 0): snapshot fast-path, no gate effect.
	snap, err := a.Recover(context.Background(), 0)
	if err != nil || snap.Generation != 1 {
		t.Fatalf("stale rejection mishandled: %+v %v", snap, err)
	}
	// Gate still armed for generation 1:
	if _, err := a.Recover(context.Background(), 1); !errors.Is(err, ErrRecoveryBackoff) {
		t.Fatalf("stale rejection disturbed the armed gate: %v", err)
	}
	if got := func() int { _, _, r, _ := f.counts(); return r }(); got != refreshesBefore {
		t.Fatalf("stale/gated attempts caused traffic: %d -> %d", refreshesBefore, got)
	}
}

// P2-C3.8: an hourly 401 during an armed backoff window causes no OAuth
// traffic and no escalation — the tick logs and retries later.
func TestHourlyTickDuringBackoffNoStorm(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	clock := newTestClock()
	a.now = clock.Now
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"

	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) { f.oauthError(w, 401, "invalid access token") }
	f.refreshHandler = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }
	f.mu.Unlock()

	for range 5 {
		a.hourlyTick(context.Background())
	}
	if _, _, refresh, _ := f.counts(); refresh != 1 {
		t.Fatalf("hourly ticks inside the backoff window spent %d refresh grants, want 1", refresh)
	}
}

// --- P2-C5 matrix: complete-set replacement ---

// P2-C5.1/P2-C5.2: ReplaceCredentials is all-or-nothing — every field of the
// previous set (refresh token, scopes, token type, expiry, identity
// provenance, validation state, staged candidate, backoff gate) is replaced
// or cleared, with exactly one generation bump.
func TestReplaceCredentialsIsCompleteReplacement(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"
	a.tokenType = "bearer"
	a.scopes = requiredScopes()
	a.expiresAt = time.Now().Add(time.Hour)
	a.userID = "uid-1"
	a.userIDAuthoritative = true
	a.validationState = "valid"

	// Arm gate + stage a candidate to prove both are discarded.
	a.gateGen, a.gateFailures, a.gateNextAllowed = 0, 3, time.Now().Add(time.Hour)
	a.pendingCandidate = &tokenCandidate{pair: &TokenResponse{AccessToken: "test-access-cand"}, forGeneration: 0}

	gen := a.ReplaceCredentials(TokenResponse{AccessToken: "external-access"})
	if gen != 1 || a.Generation() != 1 {
		t.Fatalf("generation = %d, want exactly one bump to 1", a.Generation())
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "external-access" {
		t.Fatalf("access token not replaced")
	}
	if a.refreshToken != "" || a.tokenType != "" || len(a.scopes) != 0 || !a.expiresAt.IsZero() {
		t.Fatalf("mixed credential set after replacement: refresh=%q type=%q scopes=%v expires=%v",
			a.refreshToken, a.tokenType, a.scopes, a.expiresAt)
	}
	if a.userIDAuthoritative {
		t.Fatalf("identity provenance survived the replacement")
	}
	if a.userID != "" {
		t.Fatalf("user ID survived the replacement: %q", a.userID)
	}
	if a.validationState != "unknown" {
		t.Fatalf("validation state = %q, want unknown", a.validationState)
	}
	if a.pendingCandidate != nil {
		t.Fatalf("staged candidate survived the replacement")
	}
	if a.gateFailures != 0 || !a.gateNextAllowed.IsZero() {
		t.Fatalf("backoff gate survived the replacement")
	}
	if !a.persistDirty {
		t.Fatalf("replacement not marked persist-pending")
	}
}

// P2: SetUserID never CONFERS runtime identity confirmation — after it, a
// validate 200 whose login differs from the configured profile is still
// FOREIGN (the configured-username anchor applies, not rename tolerance).
func TestSetUserIDDoesNotConferAuthority(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.SetUserID("uid-1")

	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) {
		f.writeJSON(w, 200, validateResponse{
			ClientID: a.clientID, Login: "someoneelse",
			Scopes: requiredScopes(), UserID: "uid-1", ExpiresIn: 5000,
		})
	}
	f.mu.Unlock()

	status, err := a.ValidateAndApply(context.Background())
	if status != ValidateStatusIdentityMismatch || !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("SetUserID conferred rename tolerance to an unvalidated binding: %v %v", status, err)
	}
}

// --- P2-C6 companion: atomicWriteFile's own doc stays platform-qualified ---

func TestAtomicWriteFileDocIsPlatformQualified(t *testing.T) {
	src, err := os.ReadFile("persist.go")
	if err != nil {
		t.Fatalf("read persist.go: %v", err)
	}
	s := string(src)
	idx := strings.Index(s, "func (a *TwitchAuth) atomicWriteFile")
	if idx < 0 {
		t.Fatalf("atomicWriteFile not found")
	}
	doc := s[:idx]
	if tail := strings.LastIndex(doc, "\n}\n"); tail >= 0 {
		doc = doc[tail:]
	}
	if !strings.Contains(doc, "non-Unix") && !strings.Contains(doc, "Windows") {
		t.Fatalf("atomicWriteFile doc comment lost its platform qualification")
	}
}

// P2-C1/C2 robustness: a transient validate blip WITHIN the retry budget is
// ridden out in-flight — the candidate is promoted with no extra refresh and
// no staging/abort. (Removing the retry loop makes this fail.)
func TestCandidateValidationRidesOutTransientBlip(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"

	var calls atomic.Int64
	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 { // brief blip, within the retry budget
			w.WriteHeader(503)
			return
		}
		validFor(f, a, w)
	}
	f.mu.Unlock()

	snap, err := a.Recover(context.Background(), 0)
	if err != nil {
		t.Fatalf("a transient validate blip within the retry budget must not fail recovery: %v", err)
	}
	if snap.AccessToken != "test-access-2" || a.Generation() != 1 {
		t.Fatalf("candidate not promoted after riding out the blip: %+v", snap)
	}
	if _, _, refresh, _ := f.counts(); refresh != 1 {
		t.Fatalf("the validate blip caused an extra refresh grant: %d", refresh)
	}
}
