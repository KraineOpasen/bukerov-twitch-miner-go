package web

// S5-4 Drops test harness: a configurable CampaignsProvider stub that lets
// each test control SyncStatus independently (task Phase 4's R17 state
// matrix needs never-synced/failed/aged/fresh permutations that the shared
// f3Campaigns fixture in f3_harness_test.go — always fresh, always
// successful — cannot express), plus small Drop/Campaign builders for the
// authoritative-evidence fields (DropInstanceID, Claimability,
// AccountConnection) the R17/DP-C/B11/Claims logic reads.

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/drops"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// s54Campaigns is a CampaignsProvider whose SyncStatus (and campaign list) a
// test sets directly, so each R17 state can be exercised in isolation.
type s54Campaigns struct {
	campaigns []*models.Campaign
	status    drops.SyncStatus
}

func (s *s54Campaigns) Campaigns() []*models.Campaign { return s.campaigns }
func (s *s54Campaigns) SyncStatus() drops.SyncStatus  { return s.status }
func (s *s54Campaigns) RequestManualSync() drops.ManualSyncResult {
	return drops.ManualSyncResult{Triggered: true, Status: s.status}
}

// s54ServerWith builds the shared F3 page server and swaps in an s54Campaigns
// stub reporting exactly the given campaigns/status — every other provider
// (discovery, policy, health, catalog, ...) stays the shared f3 fixture.
func s54ServerWith(t *testing.T, campaigns []*models.Campaign, status drops.SyncStatus) *Server {
	t.Helper()
	srv := buildF3PageServer(t)
	srv.SetCampaignsProvider(&s54Campaigns{campaigns: campaigns, status: status})
	return srv
}

// s54UnobservedDrop is a drop Twitch has never returned an inventory
// observation for at all: no minted instance, zero watched minutes. This is
// the exact R17/unknown-progress and Claims-unknown fixture — genuinely no
// authoritative evidence exists yet, so it must never render as a fabricated
// 0% or a fabricated claim state.
func s54UnobservedDrop(name string, required int) *models.Drop {
	return &models.Drop{
		Name:            name,
		Benefit:         name + " benefit",
		MinutesRequired: required,
		Claimability:    models.ClaimabilityUnknown,
	}
}

// s54ClaimableDrop is a drop Twitch authoritatively marked ready to claim (a
// minted instance, ClaimabilityKnownTrue), not yet claimed.
func s54ClaimableDrop(name string, watched, required int) *models.Drop {
	return &models.Drop{
		Name:                  name,
		Benefit:               name + " benefit",
		MinutesRequired:       required,
		CurrentMinutesWatched: watched,
		DropInstanceID:        "inst-" + name,
		Claimability:          models.ClaimabilityKnownTrue,
	}
}

// s54InProgressDrop is a drop Twitch HAS observed (a minted instance, a real
// watched-minute reading) but has NOT authoritatively marked claimable yet —
// still earning. Distinct from s54UnobservedDrop: this one has real evidence,
// just not a positive claimable signal.
func s54InProgressDrop(name string, watched, required int) *models.Drop {
	return &models.Drop{
		Name:                  name,
		Benefit:               name + " benefit",
		MinutesRequired:       required,
		CurrentMinutesWatched: watched,
		DropInstanceID:        "inst-" + name,
		Claimability:          models.ClaimabilityKnownFalse,
	}
}

// s54ClaimedDrop is a drop Twitch has authoritatively confirmed claimed.
func s54ClaimedDrop(name string, required int) *models.Drop {
	return &models.Drop{
		Name:            name,
		Benefit:         name + " benefit",
		MinutesRequired: required,
		IsClaimed:       true,
		Claimability:    models.ClaimabilityKnownFalse,
	}
}

// s54Campaign builds a campaign with the given drops, in progress (not yet
// claimed at the campaign level) and unrestricted (no Channels), so tests
// only need to set the fields their scenario actually cares about.
func s54Campaign(id, name, game string, ds []*models.Drop) *models.Campaign {
	return &models.Campaign{
		ID:          id,
		Name:        name,
		Game:        &models.Game{ID: "g-" + id, Name: game, DisplayName: game},
		StartAt:     time.Now().Add(-24 * time.Hour),
		EndAt:       time.Now().Add(24 * time.Hour),
		Drops:       ds,
		ClaimStatus: models.CampaignClaimStatusInProgress,
	}
}

// s54SensitiveLastErrorCanary is one shared privacy-regression fixture: a
// drops.SyncStatus.LastError value engineered to trip a specific
// supportbundle.Redact detection rule (an Authorization/Set-Cookie header, a
// key=value secret assignment, an embedded URL, an embedded newline, or a
// long high-entropy run), carrying a unique marker substring so a test can
// assert the raw value never reaches an HTTP response body. Shared by the
// DegradedStrip.Cause (buildDropsListData) and handleAPIDropsSync redaction
// tests in s5_4_drops_test.go (task S5-4 privacy fix) so the same six
// canaries prove both LastError exposures are closed identically.
type s54SensitiveLastErrorCanary struct {
	name  string // subtest name
	value string // the raw (pre-redaction) LastError value
	// marker is a substring unique to this canary: its presence anywhere in
	// a response proves the raw value leaked.
	marker string
	// alsoAbsent lists additional raw substrings (e.g. a bare hostname split
	// out of an embedded URL) that must also never appear unredacted.
	alsoAbsent []string
}

// s54SensitiveLastErrorCanaries are ASCII-only (so JSON/HTML escaping can
// never mask a leak the way a multi-byte or quote-heavy value might).
var s54SensitiveLastErrorCanaries = []s54SensitiveLastErrorCanary{
	{
		name:   "bearer",
		value:  "twitch GQL ViewerDropsDashboard: request failed: Authorization: Bearer S5_4_CANARY_BEARER_TOKEN_1234",
		marker: "S5_4_CANARY_BEARER_TOKEN_1234",
	},
	{
		name:   "cookie",
		value:  "twitch GQL Inventory: request failed, Set-Cookie: session=S5_4_CANARY_COOKIE_VALUE_5678",
		marker: "S5_4_CANARY_COOKIE_VALUE_5678",
	},
	{
		name:   "secret",
		value:  "campaign details unavailable (stale Twitch query metadata): client_secret=S5_4_CANARY_SECRET_9012",
		marker: "S5_4_CANARY_SECRET_9012",
	},
	{
		name:       "url",
		value:      `request failed: Post "https://gql.twitch.tv/gql?sig=S5_4_CANARY_SIG_3456": context deadline exceeded`,
		marker:     "S5_4_CANARY_SIG_3456",
		alsoAbsent: []string{"gql.twitch.tv"},
	},
	{
		name:   "multiline",
		value:  "twitch GQL Inventory: request failed: connection reset\nS5_4_CANARY_MULTILINE_TRACE_LINE",
		marker: "S5_4_CANARY_MULTILINE_TRACE_LINE",
	},
	{
		name:   "entropy",
		value:  "campaign details unavailable: Xk9Qm2Pv7Rt4Ws8Zc6Fh0Jd5Lg8Nn3Ss7Uu",
		marker: "Xk9Qm2Pv7Rt4Ws8Zc6Fh0Jd5Lg8Nn3Ss7Uu",
	},
}

// s54BenignLastError is the paired benign control: a LastError value with no
// sensitive shape at all, which must survive supportbundle.Redact unchanged
// — exactly what the pre-existing TestS5_4CurrentRetainsCardsOnFailedSync /
// TestS5_4CurrentAttemptedButNeverSucceededHasNoChipOrEmptyState fixtures
// already assume (both already assert this literal renders verbatim).
const s54BenignLastError = "inventory: HTTP 500"

// s54PostDropsSync issues a POST against /api/drops/sync through srv's full
// handler chain (routing and the csrfProtectMiddleware same-origin check
// included) and fails the test on a non-200 response, returning the raw
// response body. Mirrors f3GetPage's shape for the POST case — f3GetPage
// itself only ever issues GET. A bare httptest.NewRequest carries no Origin/
// Referer/Sec-Fetch-Site header, so checkSameOrigin's same-origin check
// passes it through untouched, exactly like the pre-existing
// handlers_drops_sync_test.go POST tests (e.g. TestHandleAPIDropsSyncTriggered).
func s54PostDropsSync(t *testing.T, srv *Server) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/drops/sync", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/drops/sync = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}
