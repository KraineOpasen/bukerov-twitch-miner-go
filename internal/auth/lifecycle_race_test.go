package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
)

// J1/J2/J5: concurrent snapshot readers, recoveries keyed on a stale
// generation, validations, and saves — the system must stay race-free and
// converge on one final generation with a complete pair. Meaningful under
// -race.
func TestLifecycleConcurrencyConverges(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"
	a.userID = "uid-1"

	// Rotate the refresh response so every successful refresh publishes a
	// fresh, self-consistent pair.
	f.mu.Lock()
	n := 0
	f.refreshHandler = func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		n++
		i := n
		f.mu.Unlock()
		f.writeJSON(w, 200, TokenResponse{
			AccessToken:  "test-access-r" + string(rune('0'+i%10)),
			RefreshToken: "test-refresh-r" + string(rune('0'+i%10)),
			ExpiresIn:    14000, Scope: requiredScopes(), TokenType: "bearer",
		})
	}
	f.mu.Unlock()

	// Rotation callback re-enters read APIs (J4: callback re-entry must not
	// deadlock or corrupt state).
	a.SetRotationCallback(func(uint64) {
		_ = a.Snapshot()
		_ = a.GetAuthToken()
		_ = a.Health()
	})

	ctx := context.Background()
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				_, _ = a.Recover(ctx, a.Generation()-1) // mostly stale keys
				_, _ = a.Recover(ctx, 0)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				_, _ = a.ValidateAndApply(ctx)
				_ = a.Snapshot()
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 5 {
				_ = a.SaveAuth()
			}
		}()
	}
	wg.Wait()

	// J5: one final explicit recovery converges to a usable credential set.
	snap, err := a.Recover(ctx, a.Generation()-1)
	if err != nil {
		t.Fatalf("final recover: %v", err)
	}
	if snap.AccessToken == "" {
		t.Fatalf("converged state has no access token")
	}
	if !a.Health().HasRefreshToken {
		t.Fatalf("converged state lost its refresh token")
	}
	if device, _, _, _ := f.counts(); device != 0 {
		t.Fatalf("refresh-capable session fell back to device flow under concurrency")
	}
}

// J3: shutdown (lifecycle-context cancellation) aborts an in-flight recovery
// cleanly; after a fresh lifecycle a later attempt recovers. A caller's own
// context, by contrast, only bounds that caller's wait.
func TestShutdownAbortsFlightCallerCtxOnlyBoundsWait(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"

	entered := make(chan struct{})
	f.mu.Lock()
	f.refreshHandler = func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done() // parked until the request context dies
		w.WriteHeader(500)
	}
	f.mu.Unlock()

	lifecycleCtx, shutdown := context.WithCancel(context.Background())
	a.SetLifecycleContext(lifecycleCtx)

	// The initiating caller stops waiting early; the flight keeps running.
	callerCtx, callerCancel := context.WithCancel(context.Background())
	callerErr := make(chan error, 1)
	go func() {
		_, err := a.Recover(callerCtx, 0)
		callerErr <- err
	}()
	<-entered
	callerCancel()
	if err := <-callerErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("caller wait error = %v, want context.Canceled", err)
	}
	a.mu.Lock()
	stillFlying := a.recovering
	a.mu.Unlock()
	if !stillFlying {
		t.Fatalf("caller cancellation aborted the shared flight")
	}

	// Shutdown aborts the flight.
	shutdown()
	waitCond(t, "flight to abort on shutdown", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return !a.recovering
	})
	if a.Generation() != 0 {
		t.Fatalf("aborted flight published a generation")
	}

	// Fresh lifecycle: a new attempt succeeds.
	a.SetLifecycleContext(context.Background())
	f.mu.Lock()
	f.refreshHandler = nil
	f.mu.Unlock()
	if _, err := a.Recover(context.Background(), 0); err != nil {
		t.Fatalf("post-shutdown recover: %v", err)
	}
}
