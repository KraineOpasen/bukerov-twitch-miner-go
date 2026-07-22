package api

import (
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// cpStrictClient serves the ChannelPointsContext op with the given body and
// counts ClaimCommunityPoints (bonus) calls so a test can assert none happened.
func cpStrictClient(t *testing.T, contextBody string, claims *int32) *TwitchClient {
	t.Helper()
	return newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch gqlOperationName(r) {
		case "ClaimCommunityPoints":
			if claims != nil {
				atomic.AddInt32(claims, 1)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"data":{"claimCommunityPoints":{"claim":{"id":"x"}}}}`)
		default: // ChannelPointsContext
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, contextBody)
		}
	})
}

// B4A: a top-level GraphQL "errors" array — even alongside an otherwise-valid
// communityPoints data node — must classify UNKNOWN, write NO field, and trigger
// NO bonus claim.
func TestLoadChannelPointsContextTopLevelErrorsIgnoresPartialData(t *testing.T) {
	// errors present AND a structurally valid enabled context + an availableClaim.
	body := `{"errors":[{"message":"service error"}],` +
		`"data":{"community":{"channel":{"self":{"communityPoints":{` +
		`"balance":123,"availableClaim":{"id":"claim-xyz"},` +
		`"activeMultipliers":[{"factor":5}]}}}}}}`

	var claims int32
	c := cpStrictClient(t, body, &claims)

	s := newTestStreamer("chan")
	s.SetChannelPoints(9000)
	s.SetChannelPointsCapability(models.CapabilityEnabled, models.CapReasonConfirmedContext)
	s.ActiveMultipliers = []models.Multiplier{{Factor: 2}}
	s.Status = models.StatusOnline

	err := c.LoadChannelPointsContext(s)
	if err == nil {
		t.Fatal("top-level errors must surface as an error")
	}
	if got := s.GetChannelPointsCapability(); got != models.CapabilityUnknown {
		t.Fatalf("capability = %v, want Unknown", got)
	}
	if s.LastConfirmedChannelPointsCapability() != models.CapabilityEnabled {
		t.Fatal("last-confirmed Enabled must be preserved")
	}
	if s.GetChannelPoints() != 9000 {
		t.Fatalf("balance must be preserved, got %d", s.GetChannelPoints())
	}
	if len(s.ActiveMultipliers) != 1 || s.ActiveMultipliers[0].Factor != 2 {
		t.Fatalf("multipliers must be preserved, got %v", s.ActiveMultipliers)
	}
	if n := atomic.LoadInt32(&claims); n != 0 {
		t.Fatalf("no bonus claim may be sent on a top-level-errors response, got %d", n)
	}
}

// B4B multipliers: ALL-OR-NOTHING. A malformed element preserves the prior set; a
// valid empty array clears; a fully valid array replaces atomically.
func TestLoadChannelPointsContextMultiplierStrictness(t *testing.T) {
	prior := []models.Multiplier{{Factor: 2}, {Factor: 3}}

	cases := []struct {
		name        string
		multipliers string // the activeMultipliers JSON value
		wantFactors []float64
	}{
		{"null element preserves prior", `[null]`, []float64{2, 3}},
		{"valid + malformed element preserves prior", `[{"factor":9},null]`, []float64{2, 3}},
		{"missing factor preserves prior", `[{}]`, []float64{2, 3}},
		{"wrong factor type preserves prior", `[{"factor":"x"}]`, []float64{2, 3}},
		{"valid empty clears", `[]`, nil},
		{"fully valid replaces", `[{"factor":5}]`, []float64{5}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"data":{"community":{"channel":{"self":{"communityPoints":{` +
				`"balance":10,"activeMultipliers":` + tc.multipliers + `}}}}}}`
			c := cpStrictClient(t, body, nil)

			s := newTestStreamer("chan")
			s.ActiveMultipliers = append([]models.Multiplier(nil), prior...)

			if err := c.LoadChannelPointsContext(s); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(s.ActiveMultipliers) != len(tc.wantFactors) {
				t.Fatalf("multipliers = %v, want factors %v", s.ActiveMultipliers, tc.wantFactors)
			}
			for i, f := range tc.wantFactors {
				if s.ActiveMultipliers[i].Factor != f {
					t.Fatalf("multiplier[%d].Factor = %v, want %v", i, s.ActiveMultipliers[i].Factor, f)
				}
			}
		})
	}
}

// B4B goals: ALL-OR-NOTHING with no-clear-on-empty upsert semantics.
func TestLoadChannelPointsContextGoalStrictness(t *testing.T) {
	settings := models.DefaultStreamerSettings()
	settings.CommunityGoals = true

	newWithPriorGoal := func() *models.Streamer {
		s := models.NewStreamer("chan", settings)
		s.CommunityGoals = map[string]*models.CommunityGoal{"g-prev": {GoalID: "g-prev", Title: "Prev"}}
		return s
	}

	t.Run("mixed valid/malformed => no partial upsert", func(t *testing.T) {
		body := `{"data":{"community":{"channel":{` +
			`"self":{"communityPoints":{"balance":10}},` +
			`"communityPointsSettings":{"goals":[{"id":"g1","title":"T"},null]}}}}}`
		c := cpStrictClient(t, body, nil)
		s := newWithPriorGoal()
		if err := c.LoadChannelPointsContext(s); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := s.CommunityGoals["g1"]; ok {
			t.Fatal("a malformed element must reject the whole goals field (no partial upsert)")
		}
		if _, ok := s.CommunityGoals["g-prev"]; !ok {
			t.Fatal("prior goals must be preserved when the goals field is rejected")
		}
	})

	t.Run("fully valid upserts", func(t *testing.T) {
		body := `{"data":{"community":{"channel":{` +
			`"self":{"communityPoints":{"balance":10}},` +
			`"communityPointsSettings":{"goals":[{"id":"g1","title":"T"}]}}}}}`
		c := cpStrictClient(t, body, nil)
		s := newWithPriorGoal()
		if err := c.LoadChannelPointsContext(s); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := s.CommunityGoals["g1"]; !ok {
			t.Fatal("a fully valid goals list must upsert")
		}
	})

	t.Run("valid empty does not clear", func(t *testing.T) {
		body := `{"data":{"community":{"channel":{` +
			`"self":{"communityPoints":{"balance":10}},` +
			`"communityPointsSettings":{"goals":[]}}}}}`
		c := cpStrictClient(t, body, nil)
		s := newWithPriorGoal()
		if err := c.LoadChannelPointsContext(s); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := s.CommunityGoals["g-prev"]; !ok {
			t.Fatal("a valid empty goals list must NOT clear existing goals (PubSub-owned lifecycle)")
		}
	})
}

// B5 end-to-end: an accepted Enabled context carrying an availableClaim still
// sends NO bonus mutation when the streamer is not eligible at reservation time
// (here: Offline). The atomic reservation is the gate, so the ClaimCommunityPoints
// op is never issued.
func TestLoadChannelPointsContextOfflineSkipsBonusClaim(t *testing.T) {
	body := `{"data":{"community":{"channel":{"self":{"communityPoints":{` +
		`"balance":50,"availableClaim":{"id":"claim-xyz"}}}}}}}`
	var claims int32
	c := cpStrictClient(t, body, &claims)

	s := newTestStreamer("chan")
	s.Status = models.StatusOffline // not eligible for a bonus claim

	if err := c.LoadChannelPointsContext(s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := atomic.LoadInt32(&claims); n != 0 {
		t.Fatalf("an offline streamer must not send a bonus claim, got %d", n)
	}
}
