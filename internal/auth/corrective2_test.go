package auth

import (
	"context"
	"errors"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- Corrective Pass 2 helpers ---

// authHeaderToken extracts the bearer value of the "OAuth <token>" validate
// header so fake handlers can answer differently per presented token.
func authHeaderToken(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "OAuth ")
}

// validFor writes the canonical healthy validate response for this profile.
func validFor(f *fakeOAuth, a *TwitchAuth, w http.ResponseWriter) {
	f.writeJSON(w, 200, validateResponse{
		ClientID: a.clientID, Login: "tester",
		Scopes: requiredScopes(), UserID: "uid-1", ExpiresIn: 5000,
	})
}

// testClock is a mutex-guarded manual clock for the a.now seam.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock { return &testClock{now: time.Unix(1_700_000_000, 0)} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// --- P2-C1: a device-flow grant is a CANDIDATE until validated ---

// P2-C1.1/P2-C1.2: a freshly granted device-flow pair whose /oauth2/validate
// is authoritatively rejected must NOT become active credentials: nothing
// published, nothing persisted, no Completed event.
func TestDeviceFlowRejectedCandidateNotPublished(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)

	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) {
		f.oauthError(w, 401, "invalid access token")
	}
	f.mu.Unlock()

	var completed atomic.Int64
	a.SetEventCallback(func(ev AuthEvent) {
		if ev.Type == AuthEventCompleted {
			completed.Add(1)
		}
	})

	err := a.DeviceFlowLogin(context.Background())
	if err == nil {
		t.Fatalf("a rejected candidate must fail the device flow")
	}
	if _, _, _, validate := f.counts(); validate == 0 {
		t.Fatalf("the granted pair was never validated before the publication decision")
	}
	if a.GetAuthToken() != "" || a.Generation() != 0 {
		t.Fatalf("unvalidated device-flow pair was published as active credentials")
	}
	if _, err := os.Stat(a.cookiesPath()); !os.IsNotExist(err) {
		t.Fatalf("unvalidated device-flow pair was persisted")
	}
	if completed.Load() != 0 {
		t.Fatalf("Completed event emitted for an unvalidated candidate")
	}
}

// P2-C1.3: a device-flow candidate that validates as a DIFFERENT account than
// the configured profile is never adopted.
func TestDeviceFlowForeignCandidateNotAdopted(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)

	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) {
		f.writeJSON(w, 200, validateResponse{
			ClientID: a.clientID, Login: "someoneelse",
			Scopes: requiredScopes(), UserID: "uid-FOREIGN", ExpiresIn: 5000,
		})
	}
	f.mu.Unlock()

	err := a.DeviceFlowLogin(context.Background())
	if !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("foreign candidate error = %v, want ErrIdentityMismatch", err)
	}
	if a.GetAuthToken() != "" || a.GetUserID() == "uid-FOREIGN" || a.Generation() != 0 {
		t.Fatalf("foreign device-flow candidate was adopted")
	}
}

// P2-C1.7: a validated healthy candidate IS promoted, with the validate
// response's authoritative identity bound at publication time.
func TestDeviceFlowValidatedCandidatePromotedWithIdentity(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)

	if err := a.DeviceFlowLogin(context.Background()); err != nil {
		t.Fatalf("device flow: %v", err)
	}
	if a.GetAuthToken() != "test-access-df" || a.Generation() != 1 {
		t.Fatalf("validated candidate not promoted")
	}
	if a.GetUserID() != "uid-1" {
		t.Fatalf("authoritative validated identity not bound at publication: %q", a.GetUserID())
	}
	if _, _, _, validate := f.counts(); validate != 1 {
		t.Fatalf("candidate validations = %d, want exactly 1", validate)
	}
}

// --- P2-C2: a refresh grant is a CANDIDATE until validated ---

// P2-C2.1: a refresh-granted pair whose validation is authoritatively
// rejected must not be published; the old credentials stay in place.
func TestRefreshCandidateRejectedNotPublished(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"

	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) {
		f.oauthError(w, 401, "invalid access token")
	}
	f.mu.Unlock()

	_, err := a.Recover(context.Background(), 0)
	if err == nil {
		t.Fatalf("a rejected refresh candidate must fail the recovery")
	}
	if a.Generation() != 0 || a.GetAuthToken() != "test-access-1" {
		t.Fatalf("unvalidated refresh pair was published as active credentials")
	}
}

// P2-C2.3/P2-C2.15: a transient candidate validation stages the pair
// privately (the one-time refresh token is already consumed) — nothing is
// published, and the NEXT recovery for the same generation re-validates the
// staged candidate instead of spending a second refresh grant.
func TestRefreshTransientValidationStagedNoSecondRefresh(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	clock := newTestClock()
	a.now = clock.Now
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"

	var stillTransient atomic.Bool
	stillTransient.Store(true)
	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) {
		if stillTransient.Load() {
			w.WriteHeader(503)
			return
		}
		validFor(f, a, w)
	}
	f.mu.Unlock()

	_, err := a.Recover(context.Background(), 0)
	if err == nil {
		t.Fatalf("transient candidate validation must surface a retryable failure, not success")
	}
	if a.Generation() != 0 || a.GetAuthToken() != "test-access-1" {
		t.Fatalf("candidate published despite an unvalidated (transient) outcome")
	}

	stillTransient.Store(false)
	// Past any backoff window, the same-generation retry must re-validate the
	// staged candidate — never present the consumed refresh token again.
	clock.Advance(time.Hour)
	snap, err := a.Recover(context.Background(), 0)
	if err != nil {
		t.Fatalf("staged-candidate retry: %v", err)
	}
	if snap.AccessToken != "test-access-2" || a.Generation() != 1 {
		t.Fatalf("staged candidate not promoted on retry: %+v", snap)
	}
	if _, _, refresh, _ := f.counts(); refresh != 1 {
		t.Fatalf("refresh grants = %d, want exactly 1 (consumed token must never be re-presented)", refresh)
	}
	f.mu.Lock()
	seen := append([]string(nil), f.refreshTokensSeen...)
	f.mu.Unlock()
	if len(seen) != 1 || seen[0] != "test-refresh-1" {
		t.Fatalf("old refresh token presented %d times: %v", len(seen), seen)
	}
}

// --- P2-C3: sequential failed recoveries are rate-limited by the auth layer ---

// P2-C3.2: back-to-back failed recoveries for the SAME generation must not
// each reach the OAuth endpoint — the second attempt inside the backoff
// window is refused with ErrRecoveryBackoff and zero network traffic.
func TestSequentialFailedRecoveriesAreRateLimited(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"

	f.mu.Lock()
	f.refreshHandler = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }
	f.mu.Unlock()

	if _, err := a.Recover(context.Background(), 0); err == nil {
		t.Fatalf("first recovery should fail (500)")
	}
	_, _, refreshesAfterFirst, _ := f.counts()

	_, err := a.Recover(context.Background(), 0)
	if !errors.Is(err, ErrRecoveryBackoff) {
		t.Fatalf("immediate same-generation retry error = %v, want ErrRecoveryBackoff", err)
	}
	if _, _, refreshes, _ := f.counts(); refreshes != refreshesAfterFirst {
		t.Fatalf("backoff window did not stop endpoint traffic: %d -> %d refresh grants",
			refreshesAfterFirst, refreshes)
	}
}

// --- P2-C4: collision-free profile temp namespace ---

// P2-C4.1: distinct final paths must never share a temp prefix — the lossy
// metacharacter sanitization ("b?b" and "b*b" both -> "b_b") is gone.
func TestTempPrefixCollisionFree(t *testing.T) {
	pairs := [][2]string{
		{"cookies/b?b.json", "cookies/b*b.json"},
		{"cookies/a[1].json", "cookies/a[2].json"},
		{"cookies/x_y.json", "cookies/x?y.json"},
	}
	for _, p := range pairs {
		if ownTempPrefix(p[0]) == ownTempPrefix(p[1]) {
			t.Fatalf("temp prefix collision: %q and %q both map to %q", p[0], p[1], ownTempPrefix(p[0]))
		}
	}
}

// --- P2-C5: a stale recovery owner never publishes over an external rotation ---

// P2-C5.3: if the credential generation changes while a recovery flight is in
// the air, the flight's (now stale) result must not be published, persisted,
// or announced over the newer set.
func TestExternalRotationSupersedesInFlightRecovery(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"

	// The external publication lands DURING the candidate's validation round
	// trip — after the flight's early staleness check, so only the atomic
	// compare-and-promote at the publication point can stop the stale flight.
	entered := make(chan struct{})
	release := make(chan struct{})
	f.mu.Lock()
	f.refreshHandler = func(w http.ResponseWriter, r *http.Request) {
		f.writeJSON(w, 200, TokenResponse{
			AccessToken: "test-access-stale", RefreshToken: "test-refresh-stale",
			ExpiresIn: 14000, Scope: requiredScopes(), TokenType: "bearer",
		})
	}
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) {
		if authHeaderToken(r) == "test-access-stale" {
			close(entered)
			<-release
		}
		validFor(f, a, w)
	}
	f.mu.Unlock()

	var rotations atomic.Int64
	a.SetRotationCallback(func(uint64) { rotations.Add(1) })

	done := make(chan struct{})
	go func() {
		_, _ = a.Recover(context.Background(), 0)
		close(done)
	}()
	<-entered

	// An external complete-set replacement supersedes the in-flight recovery.
	a.publishTokenPair(&TokenResponse{
		AccessToken: "test-access-ext", RefreshToken: "test-refresh-ext",
		ExpiresIn: 14000, Scope: requiredScopes(), TokenType: "bearer",
	})
	close(release)
	<-done

	if got := a.GetAuthToken(); got != "test-access-ext" {
		t.Fatalf("stale recovery flight published over the newer external set: token = %q", got)
	}
	if a.Generation() != 1 {
		t.Fatalf("stale flight bumped the generation past the external rotation: %d", a.Generation())
	}
	if rotations.Load() != 0 {
		t.Fatalf("stale flight fired the rotation callback %d times", rotations.Load())
	}
}

// --- P2-C6: persistence claims are platform-qualified at the source ---

// The SaveAuth doc comment must scope its durability claims per platform
// exactly like atomicWriteFile does: an unconditional "atomic replace /
// previous record always intact" promise contradicts the official os.Rename
// documentation on non-Unix platforms. Enforced against the source so the
// claim cannot silently regress.
func TestSaveAuthDocCommentIsPlatformQualified(t *testing.T) {
	src, err := os.ReadFile("persist.go")
	if err != nil {
		t.Fatalf("read persist.go: %v", err)
	}
	m := regexp.MustCompile(`(?s)((?:^//[^\n]*\n)+)func \(a \*TwitchAuth\) SaveAuth\(\)`).
		FindSubmatch(src)
	if m == nil {
		// (?m) for ^ inside the block:
		m = regexp.MustCompile(`(?ms)((?:^//[^\n]*\n)+)func \(a \*TwitchAuth\) SaveAuth\(\)`).
			FindSubmatch(src)
	}
	if m == nil {
		t.Fatalf("SaveAuth doc comment not found in persist.go")
	}
	doc := string(m[1])

	if !strings.Contains(doc, "Unix") {
		t.Fatalf("SaveAuth doc comment carries no platform qualification of its durability claim:\n%s", doc)
	}
	if strings.Contains(doc, "never observed partially written and a crash mid-save leaves the previous record intact") {
		t.Fatalf("SaveAuth doc comment still makes the unconditional atomic-replace promise:\n%s", doc)
	}
}
