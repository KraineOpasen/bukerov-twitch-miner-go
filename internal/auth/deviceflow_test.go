package auth

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// F1: authorization_pending keeps polling until the grant lands.
func TestDeviceFlowPendingThenSuccess(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)

	var polls atomic.Int64
	f.mu.Lock()
	f.tokenHandler = func(w http.ResponseWriter, r *http.Request) {
		if polls.Add(1) < 3 {
			f.oauthError(w, 400, "authorization_pending")
			return
		}
		f.writeJSON(w, 200, TokenResponse{
			AccessToken: "test-access-df", RefreshToken: "test-refresh-df",
			ExpiresIn: 14000, Scope: requiredScopes(), TokenType: "bearer",
		})
	}
	f.mu.Unlock()

	if err := a.DeviceFlowLogin(context.Background()); err != nil {
		t.Fatalf("device flow: %v", err)
	}
	if polls.Load() != 3 {
		t.Fatalf("polls = %d, want 3", polls.Load())
	}
	if a.GetAuthToken() != "test-access-df" || a.Generation() != 1 {
		t.Fatalf("granted pair not published")
	}
}

// F2/F3: terminal rejections end the flow with the matching sentinel.
func TestDeviceFlowTerminalRejections(t *testing.T) {
	cases := []struct {
		message string
		want    error
	}{
		{"access_denied", ErrAccessDenied},
		{"authorization request declined and denied by user", ErrAccessDenied},
		{"invalid device code", ErrInvalidDeviceCode},
		{"expired device code", ErrExpiredCode},
		{"some entirely new rejection", ErrAuthProtocol},
	}
	for _, tc := range cases {
		f := newFakeOAuth(t)
		a := newLifecycleAuth(t, f)
		f.mu.Lock()
		f.tokenHandler = func(w http.ResponseWriter, r *http.Request) { f.oauthError(w, 400, tc.message) }
		f.mu.Unlock()

		err := a.DeviceFlowLogin(context.Background())
		if !errors.Is(err, tc.want) {
			t.Fatalf("message %q: error = %v, want %v", tc.message, err, tc.want)
		}
		if a.GetAuthToken() != "" || a.Generation() != 0 {
			t.Fatalf("message %q: failed flow published credentials", tc.message)
		}
	}
}

// F4: context cancellation stops polling promptly.
func TestDeviceFlowContextCancellation(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	f.mu.Lock()
	f.tokenHandler = func(w http.ResponseWriter, r *http.Request) { f.oauthError(w, 400, "authorization_pending") }
	f.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	var once sync.Once
	a.timerAfter = func(d time.Duration) <-chan time.Time {
		once.Do(func() { close(started) })
		ch := make(chan time.Time, 1)
		ch <- time.Time{}
		return ch
	}

	errCh := make(chan error, 1)
	go func() { errCh <- a.DeviceFlowLogin(ctx) }()
	<-started
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("device flow ignored context cancellation")
	}
}

// F5: the server interval is respected and slow_down grows it by 5s (RFC 8628).
func TestDeviceFlowIntervalAndSlowDown(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)

	var mu sync.Mutex
	var waits []time.Duration
	a.timerAfter = func(d time.Duration) <-chan time.Time {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		ch := make(chan time.Time, 1)
		ch <- time.Time{}
		return ch
	}

	var polls atomic.Int64
	f.mu.Lock()
	f.tokenHandler = func(w http.ResponseWriter, r *http.Request) {
		switch polls.Add(1) {
		case 1:
			f.oauthError(w, 400, "slow_down")
		case 2:
			f.oauthError(w, 400, "authorization_pending")
		default:
			f.writeJSON(w, 200, TokenResponse{
				AccessToken: "test-access-df", RefreshToken: "test-refresh-df",
				ExpiresIn: 14000, Scope: requiredScopes(), TokenType: "bearer",
			})
		}
	}
	f.mu.Unlock()

	if err := a.DeviceFlowLogin(context.Background()); err != nil {
		t.Fatalf("device flow: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(waits) != 3 {
		t.Fatalf("timer waits = %d, want 3", len(waits))
	}
	if waits[0] != 5*time.Second || waits[1] != 10*time.Second || waits[2] != 10*time.Second {
		t.Fatalf("interval sequence = %v, want [5s 10s 10s] (slow_down adds 5s)", waits)
	}
}

// F6: concurrent recovery callers with no refresh token observe ONE device
// flow and ONE Code event.
func TestConcurrentRecoveryOneDeviceFlowOneCodeEvent(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-old"

	var codeEvents, started, completed atomic.Int64
	a.SetEventCallback(func(ev AuthEvent) {
		switch ev.Type {
		case AuthEventCode:
			codeEvents.Add(1)
		case AuthEventStarted:
			started.Add(1)
		case AuthEventCompleted:
			completed.Add(1)
		}
	})

	const callers = 25
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = a.Recover(context.Background(), 0)
		}()
	}
	wg.Wait()

	device, _, _, _ := f.counts()
	if device != 1 {
		t.Fatalf("device flows = %d, want 1", device)
	}
	if codeEvents.Load() != 1 || started.Load() != 1 || completed.Load() != 1 {
		t.Fatalf("events: started=%d code=%d completed=%d, want exactly one of each",
			started.Load(), codeEvents.Load(), completed.Load())
	}
}

// F7: success persists the complete rotated pair on disk.
func TestDeviceFlowSuccessPersistsCompletePair(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)

	if err := a.DeviceFlowLogin(context.Background()); err != nil {
		t.Fatalf("device flow: %v", err)
	}
	body := readCookieFile(t, a)
	for _, want := range []string{"test-access-df", "test-refresh-df", "expires_at", "token_type"} {
		if !strings.Contains(body, want) {
			t.Fatalf("persisted record missing %q", want)
		}
	}
}

// F8: a failed flow writes no auth file at all.
func TestDeviceFlowFailureWritesNothing(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	f.mu.Lock()
	f.tokenHandler = func(w http.ResponseWriter, r *http.Request) { f.oauthError(w, 400, "access_denied") }
	f.mu.Unlock()

	if err := a.DeviceFlowLogin(context.Background()); err == nil {
		t.Fatalf("expected failure")
	}
	if _, err := os.Stat(a.cookiesPath()); !os.IsNotExist(err) {
		t.Fatalf("failed device flow wrote an auth file")
	}
}
