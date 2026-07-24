package api

import (
	"sync"
	"time"
)

// APIConnHealth is an immutable, secret-free snapshot of the GQL client's
// connectivity accounting at a point in time, for the connection-health
// classifier. It distinguishes an IDLE client (no requests attempted) from a
// genuinely FAILING one (requests attempted, transport unreachable) so normal
// idle is never mistaken for an outage. It carries only timestamps and counts —
// never URLs, tokens, or payloads.
type APIConnHealth struct {
	LastAttempt              time.Time
	LastSuccess              time.Time
	RecentTransportFailures  int
	RecentFunctionalFailures int
}

// apiConnAccount holds the connectivity accounting kept separate from the
// functional-success timestamp (TwitchClient.lastSuccess). It is
// self-synchronized; the shared *TwitchClient records into it from many
// goroutines (watcher, drops, discovery, canary).
type apiConnAccount struct {
	mu          sync.Mutex
	lastAttempt time.Time
	// functionalFailures records requests that reached Twitch but returned no
	// useful data (top-level GQL errors, PersistedQueryNotFound, HTTP 403):
	// reachable, so NOT connectivity failures, but functional degradations.
	functionalFailures eventWindow
}

func (a *apiConnAccount) markAttempt(now time.Time) {
	a.mu.Lock()
	a.lastAttempt = now
	a.mu.Unlock()
}

func (a *apiConnAccount) markFunctionalFailure(now time.Time) {
	a.functionalFailures.mark(now)
}

func (a *apiConnAccount) lastAttemptAt() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastAttempt
}

func (a *apiConnAccount) recentFunctionalFailures(now time.Time, window time.Duration) int {
	return a.functionalFailures.count(now, window)
}

// ConnHealth returns an immutable snapshot of the client's connectivity
// accounting evaluated at now over the trailing window. Safe from any goroutine.
func (c *TwitchClient) ConnHealth(now time.Time, window time.Duration) APIConnHealth {
	c.healthMu.RLock()
	lastSuccess := c.lastSuccess
	c.healthMu.RUnlock()
	return APIConnHealth{
		LastAttempt:              c.connAcct.lastAttemptAt(),
		LastSuccess:              lastSuccess,
		RecentTransportFailures:  c.gqlFailures.count(now, window),
		RecentFunctionalFailures: c.connAcct.recentFunctionalFailures(now, window),
	}
}

// LastAttemptAt returns when the client last attempted a GQL round trip.
func (c *TwitchClient) LastAttemptAt() time.Time { return c.connAcct.lastAttemptAt() }
