package api

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/auth"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// persistedQueryNotFoundBody is the HTTP-200 error body Twitch returns when the
// persisted-query hash for the requesting client ID is no longer registered.
const persistedQueryNotFoundBody = `{"errors":[{"message":"PersistedQueryNotFound","extensions":{"code":"PERSISTED_QUERY_NOT_FOUND"}}]}`

// newTestClient builds a *TwitchClient whose GQL endpoint points at a local
// httptest server driven by handler. The auth carries a dummy token/user ID so
// no real credentials are involved.
func newTestClient(t *testing.T, handler http.HandlerFunc) *TwitchClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	a := auth.NewTwitchAuth("tester", "device-id")
	a.SetToken("dummy-token")
	a.SetUserID("100")

	c := NewTwitchClient(a, "device-id")
	c.setGQLEndpoint(srv.URL)
	return c
}

func newTestStreamer(username string) *models.Streamer {
	return models.NewStreamer(username, models.StreamerSettings{})
}

// TestLoadChannelPointsContextPQNFDoesNotResetPointsOrMisreport verifies the
// core production incident: a stale ChannelPointsContext hash
// (PersistedQueryNotFound) must surface as ErrPersistedQueryNotFound — never
// ErrStreamerDoesNotExist — and must leave the streamer's last-known points
// untouched.
func TestLoadChannelPointsContextPQNFDoesNotResetPointsOrMisreport(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		// Every client ID gets PersistedQueryNotFound: the hash itself is stale.
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, persistedQueryNotFoundBody)
	})

	s := newTestStreamer("somestreamer")
	s.SetChannelPoints(5000)

	err := c.LoadChannelPointsContext(s)
	if err == nil {
		t.Fatal("expected an error when ChannelPointsContext returns PersistedQueryNotFound")
	}
	if !errors.Is(err, ErrPersistedQueryNotFound) {
		t.Fatalf("expected ErrPersistedQueryNotFound, got %v", err)
	}
	if errors.Is(err, ErrStreamerDoesNotExist) {
		t.Fatal("stale-hash failure must not be reported as ErrStreamerDoesNotExist")
	}
	if got := s.GetChannelPoints(); got != 5000 {
		t.Fatalf("channel points must be preserved on a temporary GQL failure, got %d want 5000", got)
	}
}

// TestGetChannelIDMissingDataIsGenuineNotExist verifies that a well-formed
// response with no matching user still maps to ErrStreamerDoesNotExist (a
// genuine "no such channel"), distinct from the stale-hash case, and never
// panics.
func TestGetChannelIDMissingDataIsGenuineNotExist(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"data absent", `{}`},
		{"data null", `{"data":null}`},
		{"user null", `{"data":{"user":null}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, tc.body)
			})
			_, err := c.GetChannelID("somebody")
			if !errors.Is(err, ErrStreamerDoesNotExist) {
				t.Fatalf("expected ErrStreamerDoesNotExist, got %v", err)
			}
			if errors.Is(err, ErrPersistedQueryNotFound) {
				t.Fatal("a genuine missing user must not be reported as PersistedQueryNotFound")
			}
		})
	}
}

// TestGetChannelIDServiceErrorIsNotMisreportedAsNotExist verifies that an
// HTTP-200 response carrying a top-level "errors" array (a non-PQNF service
// failure such as "service timeout") is NOT mapped to ErrStreamerDoesNotExist:
// the startup fail-fast path treats "does not exist" as a config typo and
// exits the process, so a transient service error must stay retryable.
func TestGetChannelIDServiceErrorIsNotMisreportedAsNotExist(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"errors without data", `{"errors":[{"message":"service timeout"}]}`},
		{"errors with null data", `{"errors":[{"message":"service error"}],"data":null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, tc.body)
			})
			_, err := c.GetChannelID("somebody")
			if err == nil {
				t.Fatal("expected an error for a service-error response")
			}
			if errors.Is(err, ErrStreamerDoesNotExist) {
				t.Fatalf("a transient service error must not be reported as ErrStreamerDoesNotExist, got %v", err)
			}
		})
	}
}

// TestPostGQLEmptyAndMalformedBody verifies the client returns a clear error
// (and never panics) on an empty body or malformed JSON.
func TestPostGQLEmptyAndMalformedBody(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty body", ``},
		{"whitespace only", "   \n\t"},
		{"malformed json", `{"data": {`},
		{"not json", `<html>503 Service Unavailable</html>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, tc.body)
			})
			_, err := c.GetChannelID("somebody")
			if err == nil {
				t.Fatalf("expected an error for %s, got nil", tc.name)
			}
			// Must be a plain parse/transport error, not a misclassified
			// "streamer does not exist".
			if errors.Is(err, ErrStreamerDoesNotExist) {
				t.Fatalf("empty/malformed body must not be reported as ErrStreamerDoesNotExist (got %v)", err)
			}
		})
	}
}

// TestClientIDFallbackSucceedsAndPromotes verifies that when the default client
// ID (TV) gets PersistedQueryNotFound but an alternate (Browser) works, the
// request transparently succeeds, the alternate is promoted to the active
// default, and the working ID is cached for the operation.
func TestClientIDFallbackSucceedsAndPromotes(t *testing.T) {
	var mu sync.Mutex
	perClient := map[string]int{}

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		clientID := r.Header.Get("Client-Id")
		mu.Lock()
		perClient[clientID]++
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
		if clientID == constants.ClientIDTV {
			_, _ = io.WriteString(w, persistedQueryNotFoundBody)
			return
		}
		// Any non-TV client ID succeeds.
		_, _ = io.WriteString(w, `{"data":{"user":{"id":"777"}}}`)
	})

	if got := c.ActiveClientID(); got != "TV" {
		t.Fatalf("expected initial active client ID TV, got %q", got)
	}

	id, err := c.GetChannelID("somebody")
	if err != nil {
		t.Fatalf("expected fallback to succeed, got %v", err)
	}
	if id != "777" {
		t.Fatalf("expected channel ID 777, got %q", id)
	}
	if got := c.ActiveClientID(); got != "Browser" {
		t.Fatalf("expected active client ID promoted to Browser, got %q", got)
	}

	mu.Lock()
	tvTries := perClient[constants.ClientIDTV]
	browserTries := perClient[constants.ClientIDBrowser]
	mu.Unlock()
	if tvTries != 1 || browserTries != 1 {
		t.Fatalf("expected exactly one TV attempt then one Browser attempt, got TV=%d Browser=%d", tvTries, browserTries)
	}

	// A second call must go straight to the promoted/cached client ID: no more
	// TV attempts (no re-walking the candidate list, no log-spam loop).
	if _, err := c.GetChannelID("someone-else"); err != nil {
		t.Fatalf("second call failed: %v", err)
	}
	mu.Lock()
	tvTriesAfter := perClient[constants.ClientIDTV]
	mu.Unlock()
	if tvTriesAfter != 1 {
		t.Fatalf("expected no additional TV attempts after promotion, got %d total", tvTriesAfter)
	}
}

// TestClientIDFallbackAllFail verifies that when every candidate client ID
// returns PersistedQueryNotFound the client returns ErrPersistedQueryNotFound,
// naming the operation and the number of client IDs tried.
func TestClientIDFallbackAllFail(t *testing.T) {
	var attempts int
	var mu sync.Mutex
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, persistedQueryNotFoundBody)
	})

	_, err := c.GetChannelID("somebody")
	if !errors.Is(err, ErrPersistedQueryNotFound) {
		t.Fatalf("expected ErrPersistedQueryNotFound, got %v", err)
	}

	mu.Lock()
	got := attempts
	mu.Unlock()
	if got != len(constants.GQLClientIDFallbacks) {
		t.Fatalf("expected one attempt per known client ID (%d), got %d", len(constants.GQLClientIDFallbacks), got)
	}
}

// TestCheckStreamerOnlineStaleHashDoesNotFlapOffline verifies the review's HIGH
// finding fix: a stale VideoPlayerStreamInfoOverlayChannel hash
// (PersistedQueryNotFound during a stream refresh) must NOT flap an online
// streamer offline — its online state is preserved for PubSub / the next check
// to settle.
func TestCheckStreamerOnlineStaleHashDoesNotFlapOffline(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		// The stream-info refresh (VideoPlayerStreamInfoOverlayChannel) is stale.
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, persistedQueryNotFoundBody)
	})

	s := newTestStreamer("onlinestreamer")
	s.SetOnline()
	if !s.GetIsOnline() {
		t.Fatal("precondition: streamer should start online")
	}

	c.CheckStreamerOnline(s)

	if !s.GetIsOnline() {
		t.Fatal("a stale stream-info hash must not flap an online streamer offline")
	}
}

// TestConcurrentClientIDCacheUnderRace exercises the per-operation client-ID
// cache from many goroutines simultaneously (the real usage: watcher, drops,
// discovery and the health canary all share one client). Run with -race it
// proves the cache has no data race. It combines the direct cache API with the
// full HTTP path.
func TestConcurrentClientIDCacheUnderRace(t *testing.T) {
	// Direct cache stress: concurrent readers and writers across operations.
	t.Run("cache primitives", func(t *testing.T) {
		c := &TwitchClient{defaultClientID: constants.ClientIDTV, opClientID: map[string]string{}}
		ids := constants.GQLClientIDFallbacks
		ops := []string{"ChannelPointsContext", "PlaybackAccessToken", "DropsHighlightService_AvailableDrops", "Inventory"}
		var wg sync.WaitGroup
		for g := 0; g < 40; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for i := 0; i < 250; i++ {
					op := ops[(g+i)%len(ops)]
					_ = c.candidateClientIDs(op)
					c.rememberWorkingClientID(op, ids[(g+i)%len(ids)], (g+i)%2 == 0)
					_ = c.ActiveClientID()
				}
			}(g)
		}
		wg.Wait()
	})

	// Full HTTP path stress: TV always fails PQNF, alternates succeed, so many
	// goroutines race to write the cache and promote the default at once.
	t.Run("full request path", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			if r.Header.Get("Client-Id") == constants.ClientIDTV {
				_, _ = io.WriteString(w, persistedQueryNotFoundBody)
				return
			}
			_, _ = io.WriteString(w, `{"data":{"user":{"id":"777"}}}`)
		})

		var wg sync.WaitGroup
		for g := 0; g < 24; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 15; i++ {
					if _, err := c.GetChannelID("somebody"); err != nil {
						t.Errorf("concurrent GetChannelID failed: %v", err)
						return
					}
				}
			}()
		}
		wg.Wait()

		if got := c.ActiveClientID(); got != "Browser" {
			t.Fatalf("expected active client ID promoted to Browser after concurrent recovery, got %q", got)
		}
	})
}
