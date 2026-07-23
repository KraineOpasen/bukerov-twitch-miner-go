package api

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/auth"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// Minimal valid GQL bodies. No real tokens or payloads appear anywhere in this
// file — every credential is a fake fixture string.
const (
	validSingleBody = `{"data":{}}`
	validBatchBody  = `[{"data":{}},{"data":{}}]`
)

// installCountingRecoverFn injects the tests-only recovery seam: it simulates a
// successful credential rotation to "rotated-token" (generation bumped past the
// rejected one) and counts invocations atomically.
func installCountingRecoverFn(c *TwitchClient) *atomic.Int64 {
	var calls atomic.Int64
	c.recoverFn = func(rejected uint64) (auth.Snapshot, error) {
		calls.Add(1)
		return auth.Snapshot{
			AccessToken: "rotated-token",
			UserID:      "100",
			Username:    "tester",
			Generation:  rejected + 1,
		}, nil
	}
	return &calls
}

// installFailingRecoverFn injects a recovery seam that always fails, counting
// invocations atomically.
func installFailingRecoverFn(c *TwitchClient) *atomic.Int64 {
	var calls atomic.Int64
	c.recoverFn = func(rejected uint64) (auth.Snapshot, error) {
		calls.Add(1)
		return auth.Snapshot{}, fmt.Errorf("recovery unavailable")
	}
	return &calls
}

// installAuthErrorCounter registers an auth-error handler that counts firings.
func installAuthErrorCounter(c *TwitchClient) *atomic.Int64 {
	var fired atomic.Int64
	c.SetAuthErrorHandler(func() { fired.Add(1) })
	return &fired
}

// replayRecorder captures, per HTTP request, the Authorization header and the
// raw request body, guarded by a mutex so assertions are race-clean.
type replayRecorder struct {
	mu       sync.Mutex
	authHdrs []string
	bodies   [][]byte
}

func (rec *replayRecorder) record(r *http.Request) int {
	body, _ := io.ReadAll(r.Body)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.authHdrs = append(rec.authHdrs, r.Header.Get("Authorization"))
	rec.bodies = append(rec.bodies, body)
	return len(rec.bodies)
}

func (rec *replayRecorder) count() int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return len(rec.bodies)
}

func (rec *replayRecorder) authHeader(i int) string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.authHdrs[i]
}

func (rec *replayRecorder) body(i int) []byte {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.bodies[i]
}

// G1 (+G9): the first single-op request is rejected with HTTP 401; exactly one
// recovery runs and exactly one replay is sent, signed with the rotated token.
func TestSingleGQLFirst401RecoversAndReplaysOnce(t *testing.T) {
	rec := &replayRecorder{}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n := rec.record(r)
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, validSingleBody)
	})
	recoveries := installCountingRecoverFn(c)

	op := constants.GetIDFromLogin.WithVariables(map[string]interface{}{"login": "somebody"})
	if _, err := c.PostGQL(op); err != nil {
		t.Fatalf("expected the replayed request to succeed, got %v", err)
	}

	if got := rec.count(); got != 2 {
		t.Fatalf("expected exactly 2 HTTP requests (original + one replay), got %d", got)
	}
	if got := recoveries.Load(); got != 1 {
		t.Fatalf("expected exactly 1 recovery, got %d", got)
	}
	if got := rec.authHeader(0); got != "OAuth dummy-token" {
		t.Fatalf("first request must be signed with the original token, got %q", got)
	}
	if got := rec.authHeader(1); got != "OAuth rotated-token" {
		t.Fatalf("replay must be signed with the rotated token, got %q", got)
	}
}

// G2: the same recover-and-replay-once contract applies to a batch request —
// the batch is one HTTP request and is replayed as one.
func TestBatchGQLFirst401RecoversAndReplaysOnce(t *testing.T) {
	rec := &replayRecorder{}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n := rec.record(r)
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `[]`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, validBatchBody)
	})
	recoveries := installCountingRecoverFn(c)

	ops := []constants.GQLOperation{
		constants.GetIDFromLogin.WithVariables(map[string]interface{}{"login": "one"}),
		constants.GetIDFromLogin.WithVariables(map[string]interface{}{"login": "two"}),
	}
	result, err := c.PostGQLBatch(ops)
	if err != nil {
		t.Fatalf("expected the replayed batch to succeed, got %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 batch entries, got %d", len(result))
	}

	if got := rec.count(); got != 2 {
		t.Fatalf("expected exactly 2 HTTP requests (original batch + one replay), got %d", got)
	}
	if got := recoveries.Load(); got != 1 {
		t.Fatalf("expected exactly 1 recovery, got %d", got)
	}
	if got := rec.authHeader(1); got != "OAuth rotated-token" {
		t.Fatalf("batch replay must be signed with the rotated token, got %q", got)
	}
}

// G3: a mutation-shaped operation is replayed EXACTLY once after a 401 — never
// a third request, so a non-idempotent mutation can never be triple-sent.
func TestMutationReplayedExactlyOnceAfter401(t *testing.T) {
	rec := &replayRecorder{}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n := rec.record(r)
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, validSingleBody)
	})
	recoveries := installCountingRecoverFn(c)

	op := constants.ClaimCommunityPoints.WithVariables(map[string]interface{}{
		"input": map[string]interface{}{"channelID": "123", "claimID": "abc"},
	})
	if _, err := c.PostGQL(op); err != nil {
		t.Fatalf("expected the replayed mutation to succeed, got %v", err)
	}

	if got := rec.count(); got != 2 {
		t.Fatalf("a mutation must be sent exactly twice (original + one replay), never more; got %d requests", got)
	}
	if got := recoveries.Load(); got != 1 {
		t.Fatalf("expected exactly 1 recovery, got %d", got)
	}
}

// G4: when the replay is ALSO rejected with 401, the call surfaces
// ErrUnauthorized with no second recovery and no third request, and the
// registered auth-error handler fires.
func TestReplay401ReturnsUnauthorizedNoSecondRecovery(t *testing.T) {
	rec := &replayRecorder{}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{}`)
	})
	recoveries := installCountingRecoverFn(c)
	handlerFired := installAuthErrorCounter(c)

	op := constants.GetIDFromLogin.WithVariables(map[string]interface{}{"login": "somebody"})
	_, err := c.PostGQL(op)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}

	if got := rec.count(); got != 2 {
		t.Fatalf("expected exactly 2 HTTP requests (no third attempt after a rejected replay), got %d", got)
	}
	if got := recoveries.Load(); got != 1 {
		t.Fatalf("expected exactly 1 recovery (never a second for the rejected replay), got %d", got)
	}
	if got := handlerFired.Load(); got < 1 {
		t.Fatal("the auth-error handler must fire when the replay is rejected")
	}
}

// G5: HTTP 403 is a permission/business rejection, NOT a token rejection — it
// must never trigger recovery, ErrUnauthorized, or the auth-error handler.
func TestPlain403DoesNotTriggerRecovery(t *testing.T) {
	rec := &replayRecorder{}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"Forbidden"}`)
	})
	recoveries := installCountingRecoverFn(c)
	handlerFired := installAuthErrorCounter(c)

	op := constants.GetIDFromLogin.WithVariables(map[string]interface{}{"login": "somebody"})
	_, err := c.PostGQL(op)
	if errors.Is(err, ErrUnauthorized) {
		t.Fatalf("a 403 must never be classified as ErrUnauthorized, got %v", err)
	}

	if got := recoveries.Load(); got != 0 {
		t.Fatalf("a 403 must never trigger auth recovery, got %d recoveries", got)
	}
	if got := handlerFired.Load(); got != 0 {
		t.Fatalf("a 403 must never fire the auth-error handler, fired %d times", got)
	}
	if got := rec.count(); got != 1 {
		t.Fatalf("expected exactly 1 HTTP request (no retry, no replay), got %d", got)
	}
}

// G6: a GQL-layer permission error (HTTP 200, top-level errors array without an
// Unauthorized message) is a business rejection, not a token rejection.
func TestGQLPermissionErrorBodyDoesNotTriggerRecovery(t *testing.T) {
	rec := &replayRecorder{}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"errors":[{"message":"permission denied"}]}`)
	})
	recoveries := installCountingRecoverFn(c)

	op := constants.GetIDFromLogin.WithVariables(map[string]interface{}{"login": "somebody"})
	_, err := c.PostGQL(op)
	if errors.Is(err, ErrUnauthorized) {
		t.Fatalf("a permission-denied GQL body must never be classified as ErrUnauthorized, got %v", err)
	}

	if got := recoveries.Load(); got != 0 {
		t.Fatalf("a permission-denied GQL body must never trigger auth recovery, got %d recoveries", got)
	}
}

// G7: the documented HTTP-200 Unauthorized body shape ({"error":"Unauthorized",
// "status":401,...}) IS an authoritative token rejection: one recovery, one
// replay, success.
func TestExplicitUnauthorizedBodyTriggersOneRecovery(t *testing.T) {
	rec := &replayRecorder{}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n := rec.record(r)
		w.WriteHeader(http.StatusOK)
		if n == 1 {
			_, _ = io.WriteString(w, `{"error":"Unauthorized","status":401,"message":"token invalid"}`)
			return
		}
		_, _ = io.WriteString(w, validSingleBody)
	})
	recoveries := installCountingRecoverFn(c)

	op := constants.GetIDFromLogin.WithVariables(map[string]interface{}{"login": "somebody"})
	if _, err := c.PostGQL(op); err != nil {
		t.Fatalf("expected the replayed request to succeed, got %v", err)
	}

	if got := recoveries.Load(); got != 1 {
		t.Fatalf("expected exactly 1 recovery, got %d", got)
	}
	if got := rec.count(); got != 2 {
		t.Fatalf("expected exactly 2 HTTP requests, got %d", got)
	}
}

// G8: the replayed request body is byte-identical to the original — recovery
// rotates only the Authorization header, never the marshaled operation.
func TestReplayBodyIdentical(t *testing.T) {
	rec := &replayRecorder{}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n := rec.record(r)
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, validSingleBody)
	})
	installCountingRecoverFn(c)

	op := constants.GetIDFromLogin.WithVariables(map[string]interface{}{"login": "somebody"})
	if _, err := c.PostGQL(op); err != nil {
		t.Fatalf("expected the replayed request to succeed, got %v", err)
	}

	if got := rec.count(); got != 2 {
		t.Fatalf("expected exactly 2 HTTP requests, got %d", got)
	}
	first, second := rec.body(0), rec.body(1)
	if len(first) == 0 {
		t.Fatal("captured original request body must not be empty")
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("replayed body must be byte-identical to the original:\n first=%s\nsecond=%s", first, second)
	}
}

// G10: the transient-retry budget (429/5xx with backoff) is untouched by the
// auth layer: two 500s then a 200 succeed in exactly 3 requests with zero
// recovery involvement. The two backoff sleeps (500ms base) are inherent to
// the retry policy under test, not test synchronization.
func TestTransientRetryBudgetUnchangedByAuthLayer(t *testing.T) {
	rec := &replayRecorder{}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n := rec.record(r)
		if n <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"Internal Server Error"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, validSingleBody)
	})
	recoveries := installCountingRecoverFn(c)

	op := constants.GetIDFromLogin.WithVariables(map[string]interface{}{"login": "somebody"})
	if _, err := c.PostGQL(op); err != nil {
		t.Fatalf("expected success after transient retries, got %v", err)
	}

	if got := rec.count(); got != 3 {
		t.Fatalf("expected exactly 3 HTTP requests (2 transient failures + 1 success), got %d", got)
	}
	if got := recoveries.Load(); got != 0 {
		t.Fatalf("transient 5xx retries must never involve auth recovery, got %d recoveries", got)
	}
}

// G11: the client-ID fallback walk and the auth replay compose without
// multiplying requests: candidate 1 gets PersistedQueryNotFound, candidate 2
// gets a 401 which triggers exactly one recovery, and the single replay
// (whatever candidate order it uses) succeeds — 3 requests total.
func TestClientIDFallbackStillBoundedWithAuthReplay(t *testing.T) {
	rec := &replayRecorder{}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n := rec.record(r)
		switch n {
		case 1:
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, persistedQueryNotFoundBody)
		case 2:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{}`)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, validSingleBody)
		}
	})
	recoveries := installCountingRecoverFn(c)

	op := constants.GetIDFromLogin.WithVariables(map[string]interface{}{"login": "somebody"})
	if _, err := c.PostGQL(op); err != nil {
		t.Fatalf("expected success after fallback + recovery + replay, got %v", err)
	}

	if got := recoveries.Load(); got != 1 {
		t.Fatalf("expected exactly 1 recovery, got %d", got)
	}
	if got := rec.count(); got != 3 {
		t.Fatalf("expected exactly 3 HTTP requests (PQNF + 401 + replay), got %d", got)
	}
}

// G12: a failed auth recovery must not advance the connection-health
// timestamp — an unauthorized cycle is not a successful request.
func TestAuthFailureDoesNotAdvanceHealthTimestamp(t *testing.T) {
	rec := &replayRecorder{}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{}`)
	})
	recoveries := installFailingRecoverFn(c)

	before := c.LastSuccessAt()

	op := constants.GetIDFromLogin.WithVariables(map[string]interface{}{"login": "somebody"})
	_, err := c.PostGQL(op)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized when recovery fails, got %v", err)
	}

	if got := c.LastSuccessAt(); !got.Equal(before) {
		t.Fatalf("LastSuccessAt must not advance on an auth failure: before=%v after=%v", before, got)
	}
	if got := recoveries.Load(); got != 1 {
		t.Fatalf("expected exactly 1 recovery attempt, got %d", got)
	}
}

// G13: an unauthorized failure classifies as UNKNOWN (reason unauthorized) in
// the tri-state stream-status contract — never a false offline.
func TestUnauthorizedClassifiesUnknownNotOffline(t *testing.T) {
	status, reason := classifyCheck(fmt.Errorf("wrap: %w", ErrUnauthorized))
	if status != models.StatusUnknown {
		t.Fatalf("expected StatusUnknown for a wrapped ErrUnauthorized, got %v", status)
	}
	if reason != models.ReasonUnauthorized {
		t.Fatalf("expected ReasonUnauthorized, got %q", reason)
	}
	if status == models.StatusOffline {
		t.Fatal("an auth failure must never classify as offline")
	}
}
