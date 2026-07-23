package miner

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/auth"
)

// Review regression (P2-C3 contract): a fast consumer rejection loop (IRC
// redials every second) must not spawn one recovery observer per event — at
// most ONE observer goroutine exists at a time, so the goroutine population
// never grows with the event rate. Retry PACING is the auth layer's job (its
// per-generation backoff gate), NOT this guard's: once the observer slot is
// free again, a new rejection may observe immediately and the auth layer
// answers it with ErrRecoveryBackoff and zero network traffic if it is too
// soon.
func TestConsumerRecoveryObserverBounded(t *testing.T) {
	m := &Miner{auth: auth.NewTwitchAuth("tester", "device-xyz")}

	var calls atomic.Int64
	release := make(chan struct{})
	m.authRecoverFn = func(ctx context.Context, gen uint64) error {
		calls.Add(1)
		<-release
		return nil
	}

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.recoverFromRejectedGeneration(0, "test")
		}()
	}
	wg.Wait()

	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("recovery observations = %d, want exactly 1 for 100 rapid rejections", got)
	}

	close(release)
	// Once the observer slot frees, a NEW rejection may observe again — the
	// miner imposes no cooldown of its own (the auth backoff gate owns
	// pacing).
	for m.authRecoveryObserver.Load() && time.Now().Before(deadline) {
	}
	m.recoverFromRejectedGeneration(0, "test")
	for calls.Load() < 2 && time.Now().Before(deadline) {
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("freed observer slot did not allow a new observation: %d calls", got)
	}

	// A stale rejection (generation already rotated past) never observes.
	for m.authRecoveryObserver.Load() && time.Now().Before(deadline) {
	}
	m.auth.ReplaceCredentials(auth.TokenResponse{AccessToken: "rotated"}) // bumps the generation past 0
	m.recoverFromRejectedGeneration(0, "test")
	if got := calls.Load(); got != 2 {
		t.Fatalf("stale rejection started an observation: %d calls", got)
	}
}

// P2-C3: a backoff refusal from the auth layer is retryable — it must never
// escalate the operator reauth path.
func TestBackoffRefusalDoesNotEscalateReauth(t *testing.T) {
	m := &Miner{auth: auth.NewTwitchAuth("tester", "device-xyz")}
	m.authRecoverFn = func(ctx context.Context, gen uint64) error {
		return auth.ErrRecoveryBackoff
	}

	m.recoverFromRejectedGeneration(0, "test")
	deadline := time.Now().Add(5 * time.Second)
	for m.authRecoveryObserver.Load() && time.Now().Before(deadline) {
	}

	m.mu.Lock()
	notified, required := m.reauthNotified, m.reauthRequired
	m.mu.Unlock()
	if notified || required {
		t.Fatalf("backoff refusal escalated the reauth path (notified=%v required=%v)", notified, required)
	}
}
