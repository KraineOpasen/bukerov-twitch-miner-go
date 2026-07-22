package api

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// A structurally valid communityPoints context is the authoritative "enabled"
// signal; every failure/missing shape is UNKNOWN, never Disabled.
func TestLoadChannelPointsContextCapabilityClassification(t *testing.T) {
	const enabledBody = `{"data":{"community":{"channel":{"self":{"communityPoints":{"balance":42}}}}}}`
	const missingSelfBody = `{"data":{"community":{"channel":{}}}}`
	const missingCommunityPointsBody = `{"data":{"community":{"channel":{"self":{}}}}}`
	const missingChannelBody = `{"data":{"community":{"channel":null}}}`

	cases := []struct {
		name string
		body string
		want models.CapabilityState
	}{
		{"valid context -> enabled", enabledBody, models.CapabilityEnabled},
		{"missing self -> unknown", missingSelfBody, models.CapabilityUnknown},
		{"missing communityPoints -> unknown", missingCommunityPointsBody, models.CapabilityUnknown},
		{"missing channel -> unknown", missingChannelBody, models.CapabilityUnknown},
		{"pqnf -> unknown", persistedQueryNotFoundBody, models.CapabilityUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, tc.body)
			})
			s := newTestStreamer("chan")
			_ = c.LoadChannelPointsContext(s)
			if got := s.GetChannelPointsCapability(); got != tc.want {
				t.Fatalf("capability = %v, want %v", got, tc.want)
			}
		})
	}
}

// A transient failure must not erase a previously-confirmed Enabled capability
// (nor the balance): the classification is Unknown and preserves last-confirmed.
func TestLoadChannelPointsContextUnknownPreservesConfirmed(t *testing.T) {
	s := newTestStreamer("chan")
	s.SetChannelPoints(9000)
	s.SetChannelPointsCapability(models.CapabilityEnabled, models.CapReasonConfirmedContext)

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, persistedQueryNotFoundBody)
	})
	_ = c.LoadChannelPointsContext(s)

	if s.GetChannelPointsCapability() != models.CapabilityUnknown {
		t.Fatalf("transient failure should classify unknown, got %v", s.GetChannelPointsCapability())
	}
	if s.LastConfirmedChannelPointsCapability() != models.CapabilityEnabled {
		t.Fatal("unknown must preserve last-confirmed enabled")
	}
	if s.GetChannelPoints() != 9000 {
		t.Fatalf("balance must be preserved, got %d", s.GetChannelPoints())
	}
}

// B7 scenario 9 (concurrent, channel-synchronized, no sleeps): when a slower
// LoadChannelPointsContext response is superseded by a newer confirmation, the
// stale response must be dropped WHOLE — in particular it must NOT fire a bonus
// claim off its stale context. Deterministic: the handler blocks the first
// (old) ChannelPointsContext request until the second (new) one has completed
// and applied, using channels rather than timing.
func TestLoadChannelPointsContextStaleDropsBonusClaim(t *testing.T) {
	var claimHits int32
	var ctxReq int32
	arrived := make(chan struct{})
	release := make(chan struct{})

	const ctxWithClaim = `{"data":{"community":{"channel":{"self":{"communityPoints":{"balance":100,"availableClaim":{"id":"claim-1"}}}}}}}`

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "ClaimCommunityPoints") {
			atomic.AddInt32(&claimHits, 1)
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"data":{"claimCommunityPoints":{"claim":{"id":"claim-1"}}}}`)
			return
		}
		// ChannelPointsContext: the first (old) request blocks until released.
		if atomic.AddInt32(&ctxReq, 1) == 1 {
			close(arrived)
			<-release
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, ctxWithClaim)
	})

	s := newTestStreamer("chan")
	s.SetConfirmedOnline() // so the bonus-claim task's liveness gate passes

	done := make(chan struct{})
	go func() { _ = c.LoadChannelPointsContext(s); close(done) }() // OLD request

	<-arrived // OLD has captured its sequence and is now blocked in the handler.

	// NEW request completes first, applying Enabled+balance and (being eligible)
	// claiming the bonus once.
	_ = c.LoadChannelPointsContext(s)
	if got := atomic.LoadInt32(&claimHits); got != 1 {
		t.Fatalf("the fresh context should claim exactly once, got %d", got)
	}

	// Release the OLD request; its context is now stale and must be dropped
	// before the bonus-claim block — no additional claim.
	close(release)
	<-done
	if got := atomic.LoadInt32(&claimHits); got != 1 {
		t.Fatalf("stale context must NOT fire a second bonus claim, got %d total", got)
	}
}
