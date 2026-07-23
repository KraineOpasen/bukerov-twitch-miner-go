package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/auth"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
)

// Regression: a REAL batch 401 arrives with the documented OBJECT error body,
// which cannot unmarshal into the batch's []map shape — the status check must
// therefore run BEFORE the unmarshal, or recovery/replay never fires for
// batches.
func TestBatch401ObjectBodyRecoversAndReplays(t *testing.T) {
	rec := &replayRecorder{}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if rec.record(r) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"Unauthorized","status":401,"message":"Token invalid or missing required scope"}`)
			return
		}
		_, _ = io.WriteString(w, validBatchBody)
	})
	recoveries := installCountingRecoverFn(c)

	ops := []constants.GQLOperation{
		constants.GetIDFromLogin.WithVariables(map[string]interface{}{"login": "a"}),
		constants.GetIDFromLogin.WithVariables(map[string]interface{}{"login": "b"}),
	}
	if _, err := c.PostGQLBatch(ops); err != nil {
		t.Fatalf("batch after recovery: %v", err)
	}
	if got := recoveries.Load(); got != 1 {
		t.Fatalf("recoveries = %d, want 1", got)
	}
	if got := rec.count(); got != 2 {
		t.Fatalf("HTTP requests = %d, want 2 (original + one replay)", got)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.authHdrs[1] != "OAuth rotated-token" {
		t.Fatalf("replay did not use the rotated token")
	}
}

// Regression: a plain 403 must not advance the connection-health timestamp and
// must surface as an error instead of being parsed as data (previously its
// error body reached GetChannelID's parser and became a false
// ErrStreamerDoesNotExist — the startup fail-fast sentinel).
func TestPlain403IsErrorNotDataAndNotHealthy(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"Forbidden","status":403,"message":"blocked"}`)
	})
	before := c.LastSuccessAt()

	_, err := c.GetChannelID("somebody")
	if err == nil {
		t.Fatalf("403 must surface as an error")
	}
	if errors.Is(err, ErrStreamerDoesNotExist) {
		t.Fatalf("403 must not be misread as ErrStreamerDoesNotExist (startup fail-fast sentinel)")
	}
	if errors.Is(err, ErrUnauthorized) {
		t.Fatalf("403 must not be classified as an auth rejection")
	}
	if got := c.LastSuccessAt(); !got.Equal(before) {
		t.Fatalf("403 advanced the connection-health timestamp")
	}
}

// Regression: a TRANSIENT recovery failure (endpoint 5xx / bounded-wait
// timeout while a device flow is still running) must NOT fire the operator
// reauth handler — only a definitive failure does.
func TestTransientRecoveryFailureDoesNotEscalate(t *testing.T) {
	cases := map[string]error{
		"transient-endpoint": fmt.Errorf("%w: %w", auth.ErrRecoveryFailed, auth.ErrAuthTransient),
		"wait-timeout":       context.DeadlineExceeded,
		"shutdown":           context.Canceled,
	}
	for name, rerr := range cases {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{}`)
			})
			c.recoverFn = func(uint64) (auth.Snapshot, error) { return auth.Snapshot{}, rerr }
			fired := installAuthErrorCounter(c)

			op := constants.GetIDFromLogin.WithVariables(map[string]interface{}{"login": "somebody"})
			_, err := c.PostGQL(op)
			if !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("expected ErrUnauthorized, got %v", err)
			}
			if fired.Load() != 0 {
				t.Fatalf("transient recovery failure fired the reauth handler")
			}
		})
	}

	t.Run("definitive-still-escalates", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{}`)
		})
		c.recoverFn = func(uint64) (auth.Snapshot, error) {
			return auth.Snapshot{}, fmt.Errorf("%w: %w", auth.ErrRecoveryFailed, auth.ErrAccessDenied)
		}
		fired := installAuthErrorCounter(c)
		op := constants.GetIDFromLogin.WithVariables(map[string]interface{}{"login": "somebody"})
		if _, err := c.PostGQL(op); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("expected ErrUnauthorized, got %v", err)
		}
		if fired.Load() != 1 {
			t.Fatalf("definitive recovery failure must fire the reauth handler exactly once, got %d", fired.Load())
		}
	})
}
