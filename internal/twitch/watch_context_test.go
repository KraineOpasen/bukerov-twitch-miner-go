package twitch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// B2 — watch context ownership, transport half.
//
// doRefreshPlaybackSession already ACCEPTS a context: every watch-owned
// playback-session refresh, online confirmation and stream-info read funnels
// through it. These tests assert the behavioural half of that contract — the
// context it is handed actually reaches the GQL request and the retry wait, so
// a cancelled watch generation stops the work it started instead of riding the
// client's 30s timeout and up to four backoff sleeps to completion.
//
// No real Twitch traffic, no OAuth, no user tokens: newTestClient points the
// client at a local httptest server with a dummy credential.

// blockingGQLHandler is a GQL handler that parks every request until the test
// releases it or the request's own context is cancelled. entered is closed on
// the first request so a test can synchronise on "the request is genuinely in
// flight" without sleeping.
type blockingGQLHandler struct {
	once     sync.Once
	stopOnce sync.Once
	entered  chan struct{}
	release  chan struct{}
}

// handlerParkCap is the failure guard on a parked handler: never load-bearing
// for a pass, only a backstop so a stuck request cannot wedge the suite.
const handlerParkCap = 20 * time.Second

func newBlockingGQLHandler() *blockingGQLHandler {
	return &blockingGQLHandler{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (h *blockingGQLHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.once.Do(func() { close(h.entered) })
	select {
	case <-h.release:
	case <-r.Context().Done():
	case <-time.After(handlerParkCap):
		// Hard cap so a parked handler can never deadlock httptest's
		// Close (which waits for outstanding requests) if neither the
		// test nor the client ever releases it.
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"data":{"user":{"stream":null}}}`)
}

// stop releases any parked handler goroutine so the httptest server can close.
func (h *blockingGQLHandler) stop() { h.stopOnce.Do(func() { close(h.release) }) }

func watchTestStreamer(t *testing.T) *models.Streamer {
	t.Helper()
	s := newTestStreamer("watchedchannel")
	s.ChannelID = "cid"
	return s
}

// TestRefreshPlaybackSessionCancellationReachesBlockedGQLRequest is the
// TOKEN/GQL falsifier: with the stream-info request parked server-side,
// cancelling the context handed to doRefreshPlaybackSession must abort the
// request, not wait out the client's 30s timeout.
func TestRefreshPlaybackSessionCancellationReachesBlockedGQLRequest(t *testing.T) {
	h := newBlockingGQLHandler()
	c := newTestClient(t, h.ServeHTTP)
	// Registered AFTER newTestClient so LIFO cleanup releases the parked
	// handler BEFORE httptest's Close waits on it.
	t.Cleanup(h.stop)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := c.doRefreshPlaybackSession(ctx, watchTestStreamer(t), playbackRefreshIntent{forceStreamInfo: true})
		done <- err
	}()

	select {
	case <-h.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream-info request never reached the server")
	}

	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled refresh must report an error, not a silent success")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled refresh must surface context.Canceled, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelling the refresh context did not abort the in-flight GQL request: " +
			"the watch generation does not own its own transport")
	}
}

// retryAfterHandler answers every request with a transient 503 so
// doGQLRequestWithRetry enters its exponential-backoff wait, and reports how
// many attempts it saw.
type retryingGQLHandler struct {
	mu       sync.Mutex
	attempts int
	first    chan struct{}
	once     sync.Once
}

func newRetryingGQLHandler() *retryingGQLHandler {
	return &retryingGQLHandler{first: make(chan struct{})}
}

func (h *retryingGQLHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	h.mu.Lock()
	h.attempts++
	h.mu.Unlock()
	h.once.Do(func() { close(h.first) })
	w.WriteHeader(http.StatusServiceUnavailable)
}

func (h *retryingGQLHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.attempts
}

// TestRefreshPlaybackSessionCancellationInterruptsRetryWait is the
// RETRY/BACKOFF falsifier: a transient 503 puts the request into
// gql.RetryWait's exponential backoff. Cancelling the watch context must
// interrupt that wait instead of sleeping out the full retry schedule
// (0.5s..8s per attempt, gqlMaxRetries+1 attempts).
func TestRefreshPlaybackSessionCancellationInterruptsRetryWait(t *testing.T) {
	h := newRetryingGQLHandler()
	c := newTestClient(t, h.ServeHTTP)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.doRefreshPlaybackSession(ctx, watchTestStreamer(t), playbackRefreshIntent{forceStreamInfo: true})
	}()

	select {
	case <-h.first:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream-info request never reached the server")
	}

	// The first 503 has been answered, so the caller is now inside the first
	// backoff wait (BaseBackoff is 500ms, so this window is comfortable).
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("cancelling the refresh context did not interrupt the retry backoff wait "+
			"(attempts observed: %d): the watch generation does not own its own retry waits", h.count())
	}
}

// TestCheckStreamerOnlineContextCancellationStaysUnknown pins the ONLINE CHECK
// semantics the repair must preserve: a cancelled watch generation makes the
// check inconclusive (UNKNOWN), never an authoritative OFFLINE.
func TestCheckStreamerOnlineContextCancellationStaysUnknown(t *testing.T) {
	h := newBlockingGQLHandler()
	c := newTestClient(t, h.ServeHTTP)
	t.Cleanup(h.stop)

	// Already CONFIRMED online, so the check takes the metadata-refresh (GQL)
	// path rather than the bring-online path, which would first scrape the
	// channel page over the real network.
	s := watchTestStreamer(t)
	s.SetConfirmedOnline()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan models.StatusTransition, 1)
	go func() { done <- c.CheckStreamerOnlineContext(ctx, s) }()

	select {
	case <-h.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the online check never reached the server")
	}
	cancel()

	select {
	case tr := <-done:
		if tr.Current == models.StatusOffline {
			t.Fatal("a cancelled online check must never assert an authoritative OFFLINE")
		}
		if tr.Current != models.StatusUnknown {
			t.Fatalf("a cancelled online check must classify as UNKNOWN, got %v", tr.Current)
		}
		if s.GetStatus() == models.StatusOffline {
			t.Fatal("a cancelled online check must not publish OFFLINE onto the streamer")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelling the online check did not abort the in-flight GQL request")
	}
}

// TestCancelledRefreshOfAConfirmedOnlineStreamerIsNotAuthoritativeOnline is the
// inverse hazard: doRefreshPlaybackSession's no-op gate returns (res, nil), and
// classifyCheck maps a nil error to StatusOnline. A cancelled generation that
// reached that gate would re-confirm ONLINE with zero network evidence, so the
// cancellation gate must sit BEFORE it.
func TestCancelledRefreshOfAConfirmedOnlineStreamerIsNotAuthoritativeOnline(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a cancelled refresh must issue no request at all")
		w.WriteHeader(http.StatusOK)
	})

	s := watchTestStreamer(t)
	// A fresh stream update puts the streamer inside the 2-minute gate, so an
	// ungated metadata refresh would take the no-op path and return nil.
	s.Stream.Update("b1", "title", nil, nil, 1)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := c.doRefreshPlaybackSession(ctx, s, playbackRefreshIntent{})
	if err == nil {
		t.Fatal("a cancelled refresh must not return a nil error: classifyCheck would read it as authoritative ONLINE")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled refresh must surface context.Canceled, got %v", err)
	}
	if res.NoOp {
		t.Fatal("a cancelled refresh must not be reported as a benign gated no-op")
	}
	if status, reason := classifyCheck(err); status != models.StatusUnknown {
		t.Fatalf("a cancelled refresh must classify as UNKNOWN (reason %q), got %v", reason, status)
	}
}

// TestBackgroundOwnedCallersKeepTheirOwnLifetime is the caller-audit guard: the
// context-free entry points every non-watch subsystem uses must still run to
// completion under their own background ownership, unaffected by any watch
// generation.
func TestBackgroundOwnedCallersKeepTheirOwnLifetime(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"data":{"user":{"stream":null}}}`)
	})

	s := watchTestStreamer(t)
	s.SetConfirmedOnline()
	if err := c.UpdateStream(s); !errors.Is(err, ErrStreamerIsOffline) {
		t.Fatalf("UpdateStream must still reach Twitch under its own ownership, got %v", err)
	}
	if tr := c.CheckStreamerOnline(s); tr.Current != models.StatusOffline {
		t.Fatalf("CheckStreamerOnline must still resolve authoritatively under its own ownership, got %v", tr.Current)
	}
}

// TestGetPlaybackAccessTokenCancellationReachesTheRequest proves the production
// playback-token method plumbs its context all the way to the GQL request — the
// watch generation's first network step of every minute-watched send.
func TestGetPlaybackAccessTokenCancellationReachesTheRequest(t *testing.T) {
	h := newBlockingGQLHandler()
	c := newTestClient(t, h.ServeHTTP)
	t.Cleanup(h.stop)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, _, err := c.GetPlaybackAccessToken(ctx, "watchedchannel")
		done <- err
	}()

	select {
	case <-h.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the playback-token request never reached the server")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled playback-token request must surface context.Canceled, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelling the context did not abort the in-flight playback-token request")
	}
}

// TestExportedRefreshEntryPointsHonourTheirContext pins the two exported entry
// points the watch generation actually calls. Both are ctx pass-throughs added
// by this change; without a witness, either could be reverted to
// context.Background() with every other test still green — and
// RefreshPlaybackSession is the one the broker's joined refresh worker uses, so
// a background-bound one pins the loop goroutine for the whole transport budget.
func TestExportedRefreshEntryPointsHonourTheirContext(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a cancelled refresh must issue no request at all")
		w.WriteHeader(http.StatusOK)
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	t.Run("RefreshPlaybackSession", func(t *testing.T) {
		// fetchSpade=false so the only I/O this intent would do is the GQL
		// stream-info read against the local handler above. With a spade fetch
		// the request would leave for the real channel page and never reach the
		// handler, making the assertion vacuous.
		res := c.RefreshPlaybackSession(ctx, watchTestStreamer(t), false, models.ExpectedSession{})
		if res.Applied {
			t.Fatal("a cancelled refresh must not apply a session")
		}
		if res.NoOp {
			t.Fatal("a cancelled refresh must not be reported as a benign gated no-op")
		}
		if res.Reason != string(models.ReasonTimeout) {
			t.Fatalf("a cancelled refresh must report the cancellation reason, got %q", res.Reason)
		}
	})

	t.Run("ConfirmOnline", func(t *testing.T) {
		res, err := c.ConfirmOnline(ctx, watchTestStreamer(t))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled bring-online must surface context.Canceled, got %v", err)
		}
		if res.Applied {
			t.Fatal("a cancelled bring-online must not apply a session")
		}
	})
}
