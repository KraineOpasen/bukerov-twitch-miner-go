package twitch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/auth"
)

// B2 final closure — who owns the WAIT on a shared auth recovery.
//
// A watch-generation request that Twitch rejects with HTTP 401 does not fail
// immediately: it joins the auth layer's single-flight recovery and waits for
// it (recoverAuth, bounded by authRecoveryWait). Two different lifetimes meet at
// that line and must not be confused:
//
//   - THIS CALLER'S WAIT belongs to the watch generation. Cancelling the
//     generation has to end it, or a teardown parks for up to 60s inside a
//     request the generation no longer owns — the exact class of hidden
//     ownership this concern exists to remove.
//   - THE SHARED RECOVERY FLIGHT belongs to the auth layer, which runs it on its
//     own lifecycle context precisely so that one caller walking away cannot
//     abort a refresh (or kill a device code) that every other caller is
//     waiting on.
//
// Both halves are asserted below, through the REAL production path: c.recoverFn
// is left nil, so the call reaches c.auth.Recover with the context recoverAuth
// actually derived. Rebinding either that derivation or the Recover argument to
// context.Background() makes this test hang until its backstop and fail.
//
// What these tests deliberately do NOT cover, so the gap is not mistaken for
// coverage: the 60s authRecoveryWait DEADLINE itself. They only ever cancel,
// so a mutant that drops the WithTimeout and passes the caller's context
// straight through keeps every assertion here green. Asserting the deadline
// would mean either a 60s test or turning authRecoveryWait into a shrinkable
// package variable; the bound is unchanged, pre-existing code, so it is
// recorded as a known gap rather than paid for with either. The second half
// below (the shared flight outliving one caller) is a guard, not a proof: it
// catches a waiter that signals the flight complete, but the auth layer's own
// suite is what owns the single-flight invariant.
//
// No network: the flight is parked at the first event deviceFlowAuthenticate
// emits, which is its opening statement — before any request is built — and the
// auth layer's lifecycle context is already cancelled, so even the released
// flight cannot dial. No OAuth endpoint, no credentials, no config.

// recoveryWaitBackstop bounds every wait in this test so a regression in the
// production code fails the test instead of wedging the package's suite.
const recoveryWaitBackstop = 15 * time.Second

// parkedRecoveryFlight starts the auth layer's single-flight recovery and holds
// it parked, returning the client whose callers can join that flight, a channel
// closed when the owning call returns, and the release function.
//
// The park is the auth layer's own event callback: emitEvent releases the auth
// mutex before invoking it, so a parked callback blocks the FLIGHT without
// holding any lock a joining caller needs.
func parkedRecoveryFlight(t *testing.T) (*TwitchClient, <-chan struct{}) {
	t.Helper()

	a := auth.NewTwitchAuth("tester", "device-id")

	// No credentials are installed, so the flight takes the device-code path.
	// Its lifecycle context is already cancelled: if the park is ever released,
	// the very first request fails on the context before reaching a dial.
	dead, cancelDead := context.WithCancel(context.Background())
	cancelDead()
	a.SetLifecycleContext(dead)

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	a.SetEventCallback(func(auth.AuthEvent) {
		once.Do(func() { close(entered) })
		<-release
	})

	c := NewTwitchClient(a, "device-id")

	ownerDone := make(chan struct{})
	go func() {
		defer close(ownerDone)
		// This caller OWNS the flight; its own context is irrelevant to the test.
		_, _ = c.recoverAuth(context.Background(), 0)
	}()

	t.Cleanup(func() {
		close(release)
		select {
		case <-ownerDone:
		case <-time.After(recoveryWaitBackstop):
			t.Error("the shared auth recovery never finished after the park was released")
		}
	})

	select {
	case <-entered:
	case <-time.After(recoveryWaitBackstop):
		t.Fatal("the shared auth recovery flight never started")
	}

	return c, ownerDone
}

// TestGenerationCancellationEndsThisCallersAuthRecoveryWait is the witness the
// mutation campaign was missing: it proves the wait recoverAuth performs is
// bounded by the CALLER's context (the watch generation), not by an
// independent one.
func TestGenerationCancellationEndsThisCallersAuthRecoveryWait(t *testing.T) {
	c, ownerDone := parkedRecoveryFlight(t)

	genCtx, cancelGen := context.WithCancel(context.Background())
	defer cancelGen()

	joined := make(chan error, 1)
	go func() {
		_, err := c.recoverAuth(genCtx, 0)
		joined <- err
	}()

	// The join must be a real wait: while the generation is live this caller has
	// no verdict to return, so it must still be parked on the shared flight.
	select {
	case err := <-joined:
		t.Fatalf("the caller returned %v while the shared recovery was still running and its own "+
			"context was still live; it had no verdict to report", err)
	case <-time.After(250 * time.Millisecond):
	}

	cancelGen()

	select {
	case err := <-joined:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a wait ended by the caller's own cancellation must report it, got %v", err)
		}
	case <-time.After(recoveryWaitBackstop):
		t.Fatal("cancelling the watch generation did not end this caller's wait on the shared auth " +
			"recovery: the wait is bound to some other context, so a teardown would park inside a " +
			"request the generation no longer owns")
	}

	// The other half: one caller walking away must not have torn down the shared
	// flight that every other caller depends on. Settle first — the owning
	// caller is parked in auth.Recover's own select, so a regression that let a
	// cancelled waiter signal the flight complete would wake it on another
	// goroutine, and checking instantly would often look past that.
	time.Sleep(100 * time.Millisecond)
	select {
	case <-ownerDone:
		t.Fatal("cancelling ONE caller aborted the shared auth recovery flight; the flight runs on " +
			"the auth layer's own lifecycle context and must outlive any single caller's wait")
	default:
	}

	stillWaiting := make(chan error, 1)
	sibling, cancelSibling := context.WithCancel(context.Background())
	defer cancelSibling()
	go func() {
		_, err := c.recoverAuth(sibling, 0)
		stillWaiting <- err
	}()
	select {
	case err := <-stillWaiting:
		t.Fatalf("a fresh caller did not join the still-running shared recovery, it returned %v", err)
	case <-time.After(250 * time.Millisecond):
	}
	cancelSibling()
	select {
	case <-stillWaiting:
	case <-time.After(recoveryWaitBackstop):
		t.Fatal("the sibling caller's wait did not observe its own cancellation either")
	}
}

// TestAlreadyCancelledGenerationDoesNotWaitOnAuthRecovery is the degenerate
// twin: a caller whose generation is ALREADY gone must not spend any of the
// 60s recovery budget before reporting the cancellation.
func TestAlreadyCancelledGenerationDoesNotWaitOnAuthRecovery(t *testing.T) {
	c, _ := parkedRecoveryFlight(t)

	genCtx, cancelGen := context.WithCancel(context.Background())
	cancelGen()

	done := make(chan error, 1)
	go func() {
		_, err := c.recoverAuth(genCtx, 0)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a dead generation joining a running recovery must report its own cancellation, got %v", err)
		}
	case <-time.After(recoveryWaitBackstop):
		t.Fatal("a caller whose generation was already cancelled still waited on the shared auth " +
			"recovery instead of returning at once")
	}
}
