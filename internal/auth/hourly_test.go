package auth

import (
	"context"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// tickController replaces the timer seam with an explicitly driven channel so
// tests deliver exactly N "hours" with no wall-clock waits.
type tickController struct {
	ch       chan time.Time
	requests atomic.Int64
}

func newTickController() *tickController {
	return &tickController{ch: make(chan time.Time)}
}

func (tc *tickController) timer(d time.Duration) <-chan time.Time {
	tc.requests.Add(1)
	return tc.ch
}

// startValidator runs RunHourlyValidation on a goroutine and returns a cancel
// plus a done channel for leak-free teardown.
func startValidator(a *TwitchAuth) (context.CancelFunc, chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		a.RunHourlyValidation(ctx)
		close(done)
	}()
	return cancel, done
}

// waitCond spins until cond() or the deadline; channel-free polling of an
// externally-visible effect (counter), bounded so a failure cannot hang.
func waitCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// H1: a second concurrent validator refuses to start.
func TestHourlyValidatorSingleton(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.userID = "uid-1"
	tc := newTickController()
	a.timerAfter = tc.timer

	cancel, done := startValidator(a)
	defer func() { cancel(); <-done }()
	waitCond(t, "first validator to arm its timer", func() bool { return tc.requests.Load() >= 1 })

	// The duplicate must return immediately (it never blocks on the timer).
	dupDone := make(chan struct{})
	go func() {
		a.RunHourlyValidation(context.Background())
		close(dupDone)
	}()
	select {
	case <-dupDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("duplicate validator did not exit immediately")
	}
}

// H2/H5: exactly one validation per delivered hour; a healthy validation
// causes no credential churn, no rotation callback, and no auth-file write.
func TestHourlyTickValidatesOncePerHourNoChurn(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.userID = "uid-1"
	if err := a.SaveAuth(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	before, _ := os.ReadFile(a.cookiesPath())

	var rotations atomic.Int64
	a.SetRotationCallback(func(uint64) { rotations.Add(1) })

	tc := newTickController()
	a.timerAfter = tc.timer
	cancel, done := startValidator(a)
	defer func() { cancel(); <-done }()

	for i := 1; i <= 2; i++ {
		tc.ch <- time.Time{}
		waitCond(t, "validation to happen", func() bool {
			_, _, _, v := f.counts()
			return v >= i
		})
	}
	// Rearm proves the loop is idle again (nothing extra in flight).
	waitCond(t, "loop to rearm", func() bool { return tc.requests.Load() >= 3 })

	if _, _, _, v := f.counts(); v != 2 {
		t.Fatalf("validations = %d, want exactly 2 for 2 delivered hours", v)
	}
	if rotations.Load() != 0 {
		t.Fatalf("healthy hourly validation fired the rotation callback")
	}
	if a.Generation() != 0 {
		t.Fatalf("healthy hourly validation bumped the generation")
	}
	after, _ := os.ReadFile(a.cookiesPath())
	if string(after) != string(before) {
		t.Fatalf("healthy hourly validation rewrote the auth file")
	}
}

// H3/H4: shutdown cancels the validator without goroutine leaks.
func TestHourlyValidatorStopsOnCancel(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	tc := newTickController()
	a.timerAfter = tc.timer

	cancel, done := startValidator(a)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("validator leaked after cancellation")
	}
	// After exit the slot frees up: a new validator may start.
	a.mu.Lock()
	running := a.validatorRunning
	a.mu.Unlock()
	if running {
		t.Fatalf("validatorRunning not cleared on exit")
	}
}

// H6: a transient hourly failure keeps the session and triggers no recovery.
func TestHourlyTransientKeepsSession(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.userID = "uid-1"
	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }
	f.mu.Unlock()

	a.hourlyTick(context.Background())

	if device, _, refresh, _ := f.counts(); device != 0 || refresh != 0 {
		t.Fatalf("transient hourly failure triggered recovery traffic")
	}
	if a.GetAuthToken() != "test-access-1" {
		t.Fatalf("transient hourly failure destroyed the session")
	}
}

// H7: an hourly 401 joins an in-flight recovery instead of starting a second
// one — exactly one refresh request in total.
func TestHourly401JoinsExistingRecovery(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"
	a.userID = "uid-1"

	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) { f.oauthError(w, 401, "invalid access token") }
	release := make(chan struct{})
	f.refreshHandler = func(w http.ResponseWriter, r *http.Request) {
		<-release
		f.writeJSON(w, 200, TokenResponse{
			AccessToken: "test-access-2", RefreshToken: "test-refresh-2",
			ExpiresIn: 14000, Scope: requiredScopes(), TokenType: "bearer",
		})
	}
	f.mu.Unlock()

	// An external caller owns the recovery and is parked inside the refresh.
	ownerDone := make(chan struct{})
	go func() {
		_, _ = a.Recover(context.Background(), 0)
		close(ownerDone)
	}()
	waitCond(t, "owner flight to register", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.recovering
	})

	// The hourly tick sees 401 and must JOIN, not start a second refresh.
	tickDone := make(chan struct{})
	go func() {
		a.hourlyTick(context.Background())
		close(tickDone)
	}()
	waitCond(t, "hourly tick to validate", func() bool {
		_, _, _, v := f.counts()
		return v >= 1
	})

	close(release)
	<-ownerDone
	<-tickDone

	if _, _, refresh, _ := f.counts(); refresh != 1 {
		t.Fatalf("hourly 401 started a second refresh: %d refreshes", refresh)
	}
	if a.Generation() != 1 {
		t.Fatalf("recovery did not publish")
	}
}

// Pending persistence is retried at the validation checkpoint (B8 companion).
func TestValidationCheckpointRetriesPendingPersistence(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"
	a.userID = "uid-1"

	// First persist fails: rotation stays authoritative in memory, dirty.
	failing := true
	a.fsRename = func(oldPath, newPath string) error {
		if failing {
			return os.ErrPermission
		}
		return os.Rename(oldPath, newPath)
	}
	if _, err := a.Recover(context.Background(), 0); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if !a.Health().PersistPending {
		t.Fatalf("failed persist did not mark the state persist-pending")
	}
	if a.GetAuthToken() != "test-access-2" {
		t.Fatalf("failed persist discarded the rotated pair from memory")
	}

	// Next healthy validation is the safe checkpoint: persistence succeeds.
	failing = false
	if st, err := a.ValidateAndApply(context.Background()); st != ValidateStatusValid || err != nil {
		t.Fatalf("validate: %v %v", st, err)
	}
	if a.Health().PersistPending {
		t.Fatalf("checkpoint did not clear the pending persistence")
	}
	if body := readCookieFile(t, a); !strings.Contains(body, "test-refresh-2") {
		t.Fatalf("checkpoint persisted the wrong pair")
	}
}
