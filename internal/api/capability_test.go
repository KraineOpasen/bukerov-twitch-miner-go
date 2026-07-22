package api

import (
	"io"
	"net/http"
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
