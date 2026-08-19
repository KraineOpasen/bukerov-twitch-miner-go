package web

// S5-4 Drops test-first contract (task Phase 6): the four direct routes and
// the /drops alias, the seven-section/four-Drops-children nav with exactly
// one active child per route, the full R17 state matrix (never-synced,
// failed-attempt retention + strip, distinct attempt/success clocks, aged
// staleness, successful-empty), unknown progress never rendering as a
// fabricated 0%, DP-C placement, B11 evidence-gated absence, Upcoming's
// forbidden fields, Claims' S-SESS/no-fabrication/unknown-invariant/no-new-
// endpoint boundary, and Past's claim-authority link to /drops/claims.
//
// See s5_4_drops_harness_test.go for the configurable CampaignsProvider stub
// (s54Campaigns) and Drop/Campaign builders these tests share.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/drops"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/i18n"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// ---------------------------------------------------------------------
// Routes, alias, nav
// ---------------------------------------------------------------------

// TestS5_4DropsDirectRoutesReturn200 proves all four direct routes plus the
// /drops alias serve GET and HEAD with 200 (never a redirect).
func TestS5_4DropsDirectRoutesReturn200(t *testing.T) {
	srv := buildF3PageServer(t)
	h := srv.handler()
	for _, path := range []string{"/drops", "/drops/current", "/drops/upcoming", "/drops/claims", "/drops/past"} {
		rec, body := httpGetBody(t, h, path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200; body=%s", path, rec.Code, body)
		}

		reqHead := httptest.NewRequest(http.MethodHead, path, nil)
		recHead := httptest.NewRecorder()
		h.ServeHTTP(recHead, reqHead)
		if recHead.Code != http.StatusOK {
			t.Errorf("HEAD %s = %d, want 200", path, recHead.Code)
		}
	}
}

// TestS5_4DropsAliasMatchesCurrentRoute proves /drops is a direct alias for
// /drops/current through the exact same pipeline, not a second, diverging
// implementation.
func TestS5_4DropsAliasMatchesCurrentRoute(t *testing.T) {
	srv := buildF3PageServer(t)
	h := srv.handler()
	_, bodyAlias := httpGetBody(t, h, "/drops")
	_, bodyCanonical := httpGetBody(t, h, "/drops/current")
	if bodyAlias != bodyCanonical {
		t.Error("/drops must render byte-identical output to /drops/current (same handler, same pipeline)")
	}
}

// TestS5_4DropsActiveChildPerRoute re-implements base.html's updateActiveNav
// rule (the same simulation TestS5_3OverviewQueueExactlyOneAriaCurrentDestination
// uses) against each of the four Drops routes: exactly one Drops nav child
// is ever current, and it is always the route itself.
func TestS5_4DropsActiveChildPerRoute(t *testing.T) {
	srv := buildF3PageServer(t)
	for _, path := range []string{"/drops/current", "/drops/upcoming", "/drops/claims", "/drops/past"} {
		body := f3GetPage(t, srv, path, "en")
		tags := s5_3NavAnchorTagRe.FindAllString(body, -1)
		if len(tags) == 0 {
			t.Fatalf("%s: no C2 nav destination anchors found", path)
		}

		var currentHrefs []string
		for _, tag := range tags {
			href := ""
			if m := s5_3HrefAttrRe.FindStringSubmatch(tag); m != nil {
				href = m[1]
			}
			section := ""
			if m := s5_3NavSectionAttrRe.FindStringSubmatch(tag); m != nil {
				section = m[1]
			}
			isParent := strings.Contains(tag, "data-nav-parent")
			isChild := strings.Contains(tag, "data-nav-child")
			sectionMatches := section == "drops"
			isCurrent := sectionMatches
			if isChild {
				isCurrent = sectionMatches && href == path
			}
			if !isParent && isCurrent {
				currentHrefs = append(currentHrefs, href)
			}
		}
		if len(currentHrefs) != 1 {
			t.Errorf("%s: expected exactly one current Drops destination, got %d: %v", path, len(currentHrefs), currentHrefs)
		} else if currentHrefs[0] != path {
			t.Errorf("%s: the one current destination must be the route itself, got %s", path, currentHrefs[0])
		}
	}
}

// TestS5_4OneH1PerDropsPage proves every Drops route renders exactly one
// <h1> in both languages.
func TestS5_4OneH1PerDropsPage(t *testing.T) {
	srv := buildF3PageServer(t)
	for _, path := range []string{"/drops/current", "/drops/upcoming", "/drops/claims", "/drops/past"} {
		for _, lang := range []string{"en", "ru"} {
			body := f3GetPage(t, srv, path, lang)
			if n := strings.Count(body, "<h1"); n != 1 {
				t.Errorf("%s (lang=%s): expected exactly one <h1>, found %d", path, lang, n)
			}
		}
	}
}

// TestS5_4PollingCadencesPreserved proves the existing polling cadences moved
// with their content to the new direct routes unchanged: Current 30s
// (/api/drops) + 1m (/api/discovery), Upcoming 1m (/api/drops/upcoming),
// Past load-only (/api/drops/past).
func TestS5_4PollingCadencesPreserved(t *testing.T) {
	srv := buildF3PageServer(t)

	current := f3GetPage(t, srv, "/drops/current", "en")
	if !strings.Contains(current, `hx-get="/api/drops"`) || !strings.Contains(current, "load, every 30s") {
		t.Error("Current must keep its 30s /api/drops poll")
	}
	if !strings.Contains(current, `hx-get="/api/discovery"`) || !strings.Contains(current, "load, every 1m") {
		t.Error("Current must keep its 1m /api/discovery poll")
	}

	upcoming := f3GetPage(t, srv, "/drops/upcoming", "en")
	if !strings.Contains(upcoming, `hx-get="/api/drops/upcoming"`) || !strings.Contains(upcoming, "load, every 1m") {
		t.Error("Upcoming must keep its 1m /api/drops/upcoming poll")
	}

	// base.html's shared chrome legitimately has its own unrelated polling
	// (e.g. the sidebar's "every 30s" now-watching widget), so the "no
	// polling" assertion below is scoped to the drops/past section's own
	// hx-get literal rather than scanning the whole rendered page for the
	// word "every".
	past := f3GetPage(t, srv, "/drops/past", "en")
	if !strings.Contains(past, `hx-get="/api/drops/past" hx-trigger="load"`) {
		t.Error("Past must keep its load-only (no interval) /api/drops/past wiring")
	}
}

// TestS5_4ClaimsHasNoPollingOrNewEndpoint proves Claims is a plain,
// synchronously server-rendered page: it never issues its own htmx request
// (no new /api/drops/claims-style endpoint, no polling of the existing
// drops endpoints either). base.html's shared chrome legitimately carries
// its own unrelated htmx wiring (the sidebar's now-watching poll, the
// language-switcher POST), so this check is scoped to drops/claims-shaped
// endpoints specifically rather than banning hx-* outright.
func TestS5_4ClaimsHasNoPollingOrNewEndpoint(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/drops/claims", "en")
	for _, forbidden := range []string{`hx-get="/api/drops`, `hx-post="/api/drops`, `="/api/claims`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("Claims must not poll or call any drops-related htmx endpoint, found %q", forbidden)
		}
	}
}

// ---------------------------------------------------------------------
// Current / R17
// ---------------------------------------------------------------------

// TestS5_4CurrentNeverSyncedIsUNK proves a campaign provider that has never
// completed (or failed) a sync renders S-UNK, never a fabricated fresh
// success.
func TestS5_4CurrentNeverSyncedIsUNK(t *testing.T) {
	data := buildDropsListData(nil, drops.SyncStatus{}, enTR(t), time.Local)
	if data.NeverSyncedState == nil || data.NeverSyncedState.State != "UNK" {
		t.Fatalf("expected NeverSyncedState UNK, got %+v", data.NeverSyncedState)
	}
	if data.DegradedStrip != nil || data.EmptyState != nil {
		t.Errorf("never-synced must not also set DegradedStrip/EmptyState, got strip=%+v empty=%+v", data.DegradedStrip, data.EmptyState)
	}
}

// TestS5_4CurrentSuccessfulEmptyIsEmptyNotFailure proves a successful sync
// that legitimately finds zero campaigns renders S-EMPTY, never S-FAIL/S-DEGR.
func TestS5_4CurrentSuccessfulEmptyIsEmptyNotFailure(t *testing.T) {
	status := drops.SyncStatus{LastSyncAt: time.Now(), LastSuccessAt: time.Now(), IntervalMinutes: 60}
	data := buildDropsListData(nil, status, enTR(t), time.Local)
	if data.EmptyState == nil {
		t.Fatal("expected EmptyState (S-EMPTY) for a successful sync with zero campaigns")
	}
	if data.EmptyState.State != "EMPTY" {
		t.Errorf("expected State=EMPTY, got %q", data.EmptyState.State)
	}
	if data.DegradedStrip != nil {
		t.Error("a successful empty sync must never also show a DegradedStrip — S-EMPTY is not a failure")
	}
	if data.EmptyState.ActionTarget != "/settings/drops" {
		t.Errorf("expected the empty-state action to target /settings/drops, got %q", data.EmptyState.ActionTarget)
	}
}

// TestS5_4CurrentAttemptedButNeverSucceededHasNoChipOrEmptyState proves the
// narrow branch between "never synced" (S-UNK) and "has succeeded at least
// once" (S-DEGR/S-EMPTY/populated): an attempt happened (LastSyncAt set) but
// none has ever succeeded (LastSuccessAt still zero). There is no success
// clock to attribute a freshness chip to (S-NOBACK), and a confident S-EMPTY
// would overstate what a sync that has never once succeeded actually proved
// — the generic empty-list fallback in drops_list.html covers the body.
func TestS5_4CurrentAttemptedButNeverSucceededHasNoChipOrEmptyState(t *testing.T) {
	status := drops.SyncStatus{LastSyncAt: time.Now(), LastError: "inventory: HTTP 500", IntervalMinutes: 60}
	data := buildDropsListData(nil, status, enTR(t), time.Local)

	if data.NeverSyncedState != nil {
		t.Error("an attempted sync (LastSyncAt set) must not report NeverSyncedState")
	}
	if data.DegradedStrip == nil {
		t.Fatal("expected a DegradedStrip for the failed attempt")
	}
	if data.EmptyState != nil {
		t.Error("a sync that has never once succeeded must not claim S-EMPTY — it hasn't proven the policy found nothing")
	}
	if len(data.Campaigns) != 0 {
		t.Fatalf("expected no campaigns (none were passed in), got %d", len(data.Campaigns))
	}
}

// TestS5_4CurrentRetainsCardsOnFailedSync proves a failed LAST attempt keeps
// rendering the last-known-good cards (a backend guarantee this view layer
// must not additionally hide) alongside an S-DEGR strip. M1 target: removing
// retained campaigns after a failed sync must fail this test.
func TestS5_4CurrentRetainsCardsOnFailedSync(t *testing.T) {
	status := drops.SyncStatus{
		LastSyncAt:      time.Now(),
		LastSuccessAt:   time.Now().Add(-10 * time.Minute),
		LastError:       "inventory: HTTP 500",
		IntervalMinutes: 60,
	}

	data := buildDropsListData([]DropCampaignView{{ID: "c1", Name: "Retained Campaign"}}, status, enTR(t), time.Local)
	if len(data.Campaigns) != 1 {
		t.Fatalf("expected the retained campaign to still be in the view, got %d", len(data.Campaigns))
	}
	if data.DegradedStrip == nil {
		t.Fatal("expected a DegradedStrip (S-DEGR) for the failed attempt")
	}
	if !strings.Contains(data.DegradedStrip.Cause, "HTTP 500") {
		t.Errorf("DegradedStrip must carry the real LastError, got %q", data.DegradedStrip.Cause)
	}

	campaign := s54Campaign("c1", "Retained Campaign", "Game", []*models.Drop{s54ClaimableDrop("D1", 5, 10)})
	srv := s54ServerWith(t, []*models.Campaign{campaign}, status)
	body := f3GetPage(t, srv, "/api/drops", "en")
	if !strings.Contains(body, "Retained Campaign") {
		t.Error("expected the retained campaign's card to still render after a failed sync")
	}
	if !strings.Contains(body, "HTTP 500") {
		t.Error("expected the failed-attempt strip's error text to render")
	}
}

// TestS5_4CurrentDistinctAttemptAndSuccessClocks proves the failed-attempt
// strip's clock (LastSyncAt) and the freshness chip's clock (LastSuccessAt)
// are rendered as two distinct values, never merged. M2 target: collapsing
// them onto one clock must fail this test.
func TestS5_4CurrentDistinctAttemptAndSuccessClocks(t *testing.T) {
	attemptTime := time.Date(2026, 1, 2, 14, 32, 0, 0, time.UTC)
	successTime := time.Date(2026, 1, 2, 13, 58, 0, 0, time.UTC)
	status := drops.SyncStatus{LastSyncAt: attemptTime, LastSuccessAt: successTime, LastError: "boom", IntervalMinutes: 60}

	data := buildDropsListData([]DropCampaignView{{ID: "c1", Name: "C"}}, status, enTR(t), time.UTC)
	if data.DegradedStrip == nil {
		t.Fatal("expected a DegradedStrip")
	}
	if !strings.Contains(data.DegradedStrip.Time, "14:32") {
		t.Errorf("DegradedStrip.Time must carry the ATTEMPT clock (14:32), got %q", data.DegradedStrip.Time)
	}
	if strings.Contains(data.DegradedStrip.Time, "13:58") {
		t.Error("DegradedStrip.Time must not also carry the SUCCESS clock")
	}
	if len(data.Campaigns) != 1 || !strings.Contains(data.Campaigns[0].Chip.AgeLabel, "13:58") {
		t.Errorf("Chip.AgeLabel must carry the SUCCESS clock (13:58), got %+v", data.Campaigns[0].Chip)
	}
	if strings.Contains(data.Campaigns[0].Chip.AgeLabel, "14:32") {
		t.Error("Chip.AgeLabel must not also carry the ATTEMPT clock")
	}
}

// TestS5_4CurrentAgedChipBecomesStale proves data aging past 2x the sync
// interval without a new success escalates the freshness chip to Aged
// (S-STALE), while a recent success does not.
func TestS5_4CurrentAgedChipBecomesStale(t *testing.T) {
	fresh := buildDropsListData([]DropCampaignView{{ID: "c1"}},
		drops.SyncStatus{LastSyncAt: time.Now(), LastSuccessAt: time.Now().Add(-10 * time.Minute), IntervalMinutes: 60},
		enTR(t), time.Local)
	if fresh.Campaigns[0].Chip.Aged {
		t.Error("a sync 10m old on a 60m interval must not be Aged yet")
	}

	stale := buildDropsListData([]DropCampaignView{{ID: "c1"}},
		drops.SyncStatus{LastSyncAt: time.Now(), LastSuccessAt: time.Now().Add(-130 * time.Minute), IntervalMinutes: 60},
		enTR(t), time.Local)
	if !stale.Campaigns[0].Chip.Aged {
		t.Error("a sync 130m old on a 60m interval (>2x) must be Aged (S-STALE)")
	}

	// This surface's freshness verdict is EARNED whenever a threshold exists: it
	// derives one from the sync interval above (config clamps
	// CampaignSyncInterval to [5,120], so one always exists in production), and a
	// fresh reading is then entitled to the C0 chip's positive tier. /overview's
	// slot pair and /system/status both
	// establish no threshold and therefore render Neutral instead. Pin the
	// distinction so a future sweep cannot flatten every provenance chip to
	// Neutral and silently delete a verdict this page actually computed.
	if fresh.Campaigns[0].Chip.Neutral {
		t.Error("drops computes a real staleness threshold, so its fresh reading must keep the earned positive tier, not Neutral")
	}
	if stale.Campaigns[0].Chip.Neutral {
		t.Error("an Aged verdict is a judgement this page earned; it must not be neutralised")
	}
}

// TestS5_4UnknownProgressNeverZero proves a drop with no authoritative
// inventory observation renders C11's unknown state, never a fabricated 0%.
// M3 target: rendering unknown progress as 0% must fail this test.
func TestS5_4UnknownProgressNeverZero(t *testing.T) {
	unobserved := s54UnobservedDrop("Mystery Drop", 120)
	got := dropProgressData(unobserved)
	if got.Mode != "unknown" {
		t.Errorf("a drop with no authoritative observation must report Mode=unknown, got %+v", got)
	}

	unknownCampaign := s54Campaign("c1", "Fresh Campaign", "Game", []*models.Drop{unobserved})
	unknownSrv := s54ServerWith(t, []*models.Campaign{unknownCampaign}, drops.SyncStatus{LastSyncAt: time.Now(), LastSuccessAt: time.Now(), IntervalMinutes: 60})
	unknownBody := f3GetPage(t, unknownSrv, "/api/drops", "en")
	if !strings.Contains(unknownBody, "c11-progress--unknown") {
		t.Error("expected the C11 unknown-progress state to render for the never-observed drop")
	}
	// Deterministic rendered-output check (not just the CSS class): the
	// fabricated "0/120 min watched" / "120 min remaining" counters this bug
	// actually produced must be entirely absent, not merely coexisting with
	// the honest C11 dash state.
	if strings.Contains(unknownBody, "min watched") || strings.Contains(unknownBody, "min remaining") {
		t.Error("an unknown-progress drop must not render fabricated watched/remaining minute counters")
	}

	// Contrast case: a drop with real, determinate progress DOES show its
	// counters alongside C11's determinate bar - proving the gate above
	// suppresses only the fabricated case, not minute counters generally.
	observed := s54InProgressDrop("Observed Drop", 30, 120)
	determinateCampaign := s54Campaign("c2", "Determinate Campaign", "Game", []*models.Drop{observed})
	determinateSrv := s54ServerWith(t, []*models.Campaign{determinateCampaign}, drops.SyncStatus{LastSyncAt: time.Now(), LastSuccessAt: time.Now(), IntervalMinutes: 60})
	determinateBody := f3GetPage(t, determinateSrv, "/api/drops", "en")
	if !strings.Contains(determinateBody, "c11-progress--determinate") {
		t.Error("expected the C11 determinate state to render for the observed drop")
	}
	if !strings.Contains(determinateBody, "30/120") || !strings.Contains(determinateBody, "min watched") {
		t.Error("a determinate-progress drop must still render its real watched/remaining minute counters")
	}
}

// TestS5_4DPCBadgePlacement proves the DP-C badge is present on every
// populated Current card and absent from Current's empty state, Upcoming,
// Claims and Past. M4 target: removing DP-C from a populated Current card
// must fail this test.
func TestS5_4DPCBadgePlacement(t *testing.T) {
	const dpc = "DP-C"

	populated := s54ServerWith(t, []*models.Campaign{
		s54Campaign("c1", "Camp", "Game", []*models.Drop{s54ClaimableDrop("D1", 10, 10)}),
	}, drops.SyncStatus{LastSyncAt: time.Now(), LastSuccessAt: time.Now(), IntervalMinutes: 60})
	body := f3GetPage(t, populated, "/api/drops", "en")
	if !strings.Contains(body, dpc) {
		t.Error("expected the DP-C badge on a populated Current card")
	}

	empty := s54ServerWith(t, nil, drops.SyncStatus{LastSyncAt: time.Now(), LastSuccessAt: time.Now(), IntervalMinutes: 60})
	emptyBody := f3GetPage(t, empty, "/api/drops", "en")
	if strings.Contains(emptyBody, dpc) {
		t.Error("DP-C badge must not appear on Current's empty state")
	}

	if upcoming := f3GetPage(t, populated, "/api/drops/upcoming", "en"); strings.Contains(upcoming, dpc) {
		t.Error("Upcoming must never render the DP-C badge")
	}
	if claims := f3GetPage(t, populated, "/drops/claims", "en"); strings.Contains(claims, dpc) {
		t.Error("Claims must never render the DP-C badge")
	}
	if past := f3GetPage(t, populated, "/api/drops/past", "en"); strings.Contains(past, dpc) {
		t.Error("Past must never render the DP-C badge")
	}
}

// TestS5_4B11AccountLinkBadgeAbsentWhenUnknown proves the B11 account-link
// badge renders nothing at all (S-NOBACK) when Twitch never authoritatively
// reported the campaign's account-link state (the zero-value/default case).
func TestS5_4B11AccountLinkBadgeAbsentWhenUnknown(t *testing.T) {
	c := s54Campaign("c1", "Camp", "Game", []*models.Drop{s54ClaimableDrop("D1", 5, 10)})
	srv := s54ServerWith(t, []*models.Campaign{c}, drops.SyncStatus{LastSyncAt: time.Now(), LastSuccessAt: time.Now(), IntervalMinutes: 60})
	body := f3GetPage(t, srv, "/api/drops", "en")
	for _, want := range []string{"Account linked", "Account not linked"} {
		if strings.Contains(body, want) {
			t.Errorf("account-link badge must be absent (S-NOBACK) when AccountConnection is Unknown, found %q", want)
		}
	}
}

// TestS5_4B11AccountLinkBadgeRendersWhenProven proves the badge renders,
// correctly, once Twitch's account-link evidence is proven either way.
func TestS5_4B11AccountLinkBadgeRendersWhenProven(t *testing.T) {
	linked := s54Campaign("c1", "Linked", "Game", []*models.Drop{s54ClaimableDrop("D1", 5, 10)})
	linked.AccountConnection = models.AccountConnectionConnected
	notLinked := s54Campaign("c2", "NotLinked", "Game", []*models.Drop{s54ClaimableDrop("D2", 5, 10)})
	notLinked.AccountConnection = models.AccountConnectionDisconnected

	srv := s54ServerWith(t, []*models.Campaign{linked, notLinked}, drops.SyncStatus{LastSyncAt: time.Now(), LastSuccessAt: time.Now(), IntervalMinutes: 60})
	body := f3GetPage(t, srv, "/api/drops", "en")
	if !strings.Contains(body, "Account linked") {
		t.Error("expected the account-linked badge for the Connected campaign")
	}
	if !strings.Contains(body, "Account not linked") {
		t.Error("expected the account-not-linked badge for the Disconnected campaign")
	}
}

// ---------------------------------------------------------------------
// Privacy — SyncStatus.LastError redaction (task S5-4 privacy fix)
// ---------------------------------------------------------------------
//
// drops.SyncStatus.LastError is a raw, unbounded error string that can carry
// anything a failed Twitch GQL call put into it — an Authorization header, a
// Set-Cookie value, a client_secret assignment, a signed request URL, an
// embedded stack trace, or a high-entropy token. buildSupportBundleDrops's
// doc comment (handlers_support_bundle.go) already calls this out as
// "deliberately never read" for the support bundle. The Current tab's
// DegradedStrip.Cause (buildDropsListData) and the manual-sync JSON endpoint
// (handleAPIDropsSync) both surfaced this same raw field into dashboard HTTP
// responses; this section proves both are routed through the same approved
// sanitizer (supportbundle.Redact) the support bundle already relies on,
// while a genuinely benign LastError still renders verbatim.

// TestS5_4DegradedStripCauseRedactsSensitiveLastError proves the Current
// tab's failed-attempt strip never leaks a sensitive SyncStatus.LastError,
// at both the buildDropsListData unit level and through the rendered
// /api/drops HTTP response, while the strip itself, both the ATTEMPT and
// SUCCESS clocks, and the retained campaign card keep rendering around the
// redacted cause.
func TestS5_4DegradedStripCauseRedactsSensitiveLastError(t *testing.T) {
	attemptTime := time.Date(2026, 3, 4, 10, 15, 0, 0, time.UTC)
	successTime := time.Date(2026, 3, 4, 8, 5, 0, 0, time.UTC)
	wantAttemptClock := formatLocalDateTime(attemptTime, time.UTC)
	wantSuccessClock := formatLocalDateTime(successTime, time.UTC)
	if wantAttemptClock == wantSuccessClock {
		t.Fatalf("fixture bug: attempt/success clocks must render distinctly, both got %q", wantAttemptClock)
	}

	for _, canary := range s54SensitiveLastErrorCanaries {
		t.Run(canary.name, func(t *testing.T) {
			status := drops.SyncStatus{
				LastSyncAt:      attemptTime,
				LastSuccessAt:   successTime,
				LastError:       canary.value,
				IntervalMinutes: 60,
			}

			// Unit level: buildDropsListData must redact Cause exactly, with
			// at least one populated (retained) campaign view present.
			data := buildDropsListData([]DropCampaignView{{ID: "c1", Name: "Retained Campaign"}}, status, enTR(t), time.UTC)
			if data.DegradedStrip == nil {
				t.Fatal("expected a DegradedStrip for the failed attempt")
			}
			if data.DegradedStrip.Cause != "[REDACTED]" {
				t.Errorf("DegradedStrip.Cause = %q, want exactly \"[REDACTED]\"", data.DegradedStrip.Cause)
			}
			if strings.Contains(data.DegradedStrip.Cause, canary.marker) {
				t.Errorf("DegradedStrip.Cause leaked the raw canary marker %q: %q", canary.marker, data.DegradedStrip.Cause)
			}

			// Rendered level: the same sensitive LastError must never reach
			// the HTTP response body, while the strip, both clocks, and the
			// retained campaign card still render around the redacted cause.
			campaign := s54Campaign("c1", "Retained Campaign", "Game", []*models.Drop{s54ClaimableDrop("D1", 5, 10)})
			srv := s54ServerWith(t, []*models.Campaign{campaign}, status)
			srv.SetDisplayLocation(time.UTC)
			body := f3GetPage(t, srv, "/api/drops", "en")

			if strings.Contains(body, canary.marker) {
				t.Errorf("rendered body leaked the raw canary marker %q", canary.marker)
			}
			for _, absent := range canary.alsoAbsent {
				if strings.Contains(body, absent) {
					t.Errorf("rendered body leaked %q from the raw LastError", absent)
				}
			}
			if !strings.Contains(body, "[REDACTED]") {
				t.Error("expected the redacted marker \"[REDACTED]\" in the rendered body")
			}
			if !strings.Contains(body, enTR(t)("drops.current.state.degraded")) {
				t.Error("expected the S-DEGR strip's localized degraded message to render")
			}
			if !strings.Contains(body, wantAttemptClock) {
				t.Errorf("expected the ATTEMPT clock %q in the S-DEGR strip", wantAttemptClock)
			}
			if !strings.Contains(body, "Retained Campaign") {
				t.Error("expected the retained campaign's card to still render")
			}
			if !strings.Contains(body, wantSuccessClock) {
				t.Errorf("expected the SUCCESS/freshness clock %q in the campaign card's chip", wantSuccessClock)
			}
		})
	}
}

// TestS5_4DegradedStripCausePreservesBenignLastError is the paired benign
// control: a genuinely benign LastError (no sensitive shape at all) must
// still render verbatim — the redaction seam is not a blanket wipe — exactly
// as TestS5_4CurrentRetainsCardsOnFailedSync already assumes.
func TestS5_4DegradedStripCausePreservesBenignLastError(t *testing.T) {
	status := drops.SyncStatus{
		LastSyncAt:      time.Now(),
		LastSuccessAt:   time.Now().Add(-10 * time.Minute),
		LastError:       s54BenignLastError,
		IntervalMinutes: 60,
	}

	data := buildDropsListData([]DropCampaignView{{ID: "c1", Name: "Retained Campaign"}}, status, enTR(t), time.Local)
	if data.DegradedStrip == nil {
		t.Fatal("expected a DegradedStrip for the failed attempt")
	}
	if data.DegradedStrip.Cause != s54BenignLastError {
		t.Errorf("DegradedStrip.Cause = %q, want the benign LastError preserved verbatim (%q)", data.DegradedStrip.Cause, s54BenignLastError)
	}

	campaign := s54Campaign("c1", "Retained Campaign", "Game", []*models.Drop{s54ClaimableDrop("D1", 5, 10)})
	srv := s54ServerWith(t, []*models.Campaign{campaign}, status)
	body := f3GetPage(t, srv, "/api/drops", "en")
	if !strings.Contains(body, s54BenignLastError) {
		t.Errorf("expected the benign LastError %q to still render verbatim in the rendered body", s54BenignLastError)
	}
}

// TestS5_4HandleAPIDropsSyncRedactsSensitiveLastError proves the manual
// "Sync Drops now" endpoint's JSON response never leaks a sensitive
// SyncStatus.LastError — neither in the raw response bytes nor in the
// decoded lastError field — while every other bookkeeping field the caller
// depends on (triggered, retryAfterSecs, runs, lastSyncAtMillis,
// dashboardCampaigns, recoveredCampaigns, trackedCampaigns) stays intact.
func TestS5_4HandleAPIDropsSyncRedactsSensitiveLastError(t *testing.T) {
	fixedTime := time.Date(2026, 5, 6, 9, 30, 0, 0, time.UTC)

	for _, canary := range s54SensitiveLastErrorCanaries {
		t.Run(canary.name, func(t *testing.T) {
			srv := s54ServerWith(t, nil, drops.SyncStatus{
				Runs:               3,
				LastSyncAt:         fixedTime,
				DashboardCampaigns: 2,
				RecoveredCampaigns: 1,
				TrackedCampaigns:   2,
				LastError:          canary.value,
			})
			raw := s54PostDropsSync(t, srv)

			if strings.Contains(string(raw), canary.marker) {
				t.Errorf("raw response body leaked the canary marker %q: %s", canary.marker, raw)
			}
			for _, absent := range canary.alsoAbsent {
				if strings.Contains(string(raw), absent) {
					t.Errorf("raw response body leaked %q from the raw LastError: %s", absent, raw)
				}
			}

			var got map[string]interface{}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("decode: %v; body=%s", err, raw)
			}
			if got["lastError"] != "[REDACTED]" {
				t.Errorf("lastError = %v, want exactly \"[REDACTED]\"", got["lastError"])
			}
			if got["triggered"] != true {
				t.Errorf("triggered = %v, want true", got["triggered"])
			}
			if got["retryAfterSecs"].(float64) != 0 {
				t.Errorf("retryAfterSecs = %v, want 0", got["retryAfterSecs"])
			}
			if got["runs"].(float64) != 3 {
				t.Errorf("runs = %v, want 3", got["runs"])
			}
			if int64(got["lastSyncAtMillis"].(float64)) != fixedTime.UnixMilli() {
				t.Errorf("lastSyncAtMillis = %v, want %d", got["lastSyncAtMillis"], fixedTime.UnixMilli())
			}
			if got["dashboardCampaigns"].(float64) != 2 {
				t.Errorf("dashboardCampaigns = %v, want 2", got["dashboardCampaigns"])
			}
			if got["recoveredCampaigns"].(float64) != 1 {
				t.Errorf("recoveredCampaigns = %v, want 1", got["recoveredCampaigns"])
			}
			if got["trackedCampaigns"].(float64) != 2 {
				t.Errorf("trackedCampaigns = %v, want 2", got["trackedCampaigns"])
			}
		})
	}
}

// TestS5_4HandleAPIDropsSyncPreservesBenignLastError is the paired benign
// control for TestS5_4HandleAPIDropsSyncRedactsSensitiveLastError: a
// genuinely benign LastError still round-trips verbatim through the JSON
// response, alongside the same other-field assertions.
func TestS5_4HandleAPIDropsSyncPreservesBenignLastError(t *testing.T) {
	fixedTime := time.Date(2026, 5, 6, 9, 30, 0, 0, time.UTC)
	srv := s54ServerWith(t, nil, drops.SyncStatus{
		Runs:               3,
		LastSyncAt:         fixedTime,
		DashboardCampaigns: 2,
		RecoveredCampaigns: 1,
		TrackedCampaigns:   2,
		LastError:          s54BenignLastError,
	})
	raw := s54PostDropsSync(t, srv)

	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, raw)
	}
	if got["lastError"] != s54BenignLastError {
		t.Errorf("lastError = %v, want exactly %q", got["lastError"], s54BenignLastError)
	}
	if got["triggered"] != true {
		t.Errorf("triggered = %v, want true", got["triggered"])
	}
	if got["retryAfterSecs"].(float64) != 0 {
		t.Errorf("retryAfterSecs = %v, want 0", got["retryAfterSecs"])
	}
	if got["runs"].(float64) != 3 {
		t.Errorf("runs = %v, want 3", got["runs"])
	}
	if int64(got["lastSyncAtMillis"].(float64)) != fixedTime.UnixMilli() {
		t.Errorf("lastSyncAtMillis = %v, want %d", got["lastSyncAtMillis"], fixedTime.UnixMilli())
	}
	if got["dashboardCampaigns"].(float64) != 2 {
		t.Errorf("dashboardCampaigns = %v, want 2", got["dashboardCampaigns"])
	}
	if got["recoveredCampaigns"].(float64) != 1 {
		t.Errorf("recoveredCampaigns = %v, want 1", got["recoveredCampaigns"])
	}
	if got["trackedCampaigns"].(float64) != 2 {
		t.Errorf("trackedCampaigns = %v, want 2", got["trackedCampaigns"])
	}
}

// ---------------------------------------------------------------------
// Upcoming
// ---------------------------------------------------------------------

// TestS5_4UpcomingHasNoForbiddenFields proves the Upcoming tab never renders
// active-progress, watched-minutes, health, DP-C or account-link markup —
// display-only, exactly as task Phase 5 requires.
func TestS5_4UpcomingHasNoForbiddenFields(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/api/drops/upcoming", "en")
	for _, forbidden := range []string{
		"c11-progress", "min watched", "min remaining", "DP-C",
		"Account linked", "Account not linked", "HEALTHY", "RECOVERING", "STALLED",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("Upcoming must never render forbidden field/marker %q", forbidden)
		}
	}
}

// ---------------------------------------------------------------------
// Claims
// ---------------------------------------------------------------------

// TestS5_4ClaimsSessionBannerAndNoFabrication proves the S-SESS banner always
// renders and no fabricated per-row timestamp column exists.
func TestS5_4ClaimsSessionBannerAndNoFabrication(t *testing.T) {
	c := s54Campaign("c1", "Camp", "Game", []*models.Drop{s54ClaimableDrop("D1", 5, 10)})
	srv := s54ServerWith(t, []*models.Campaign{c}, drops.SyncStatus{LastSyncAt: time.Now(), LastSuccessAt: time.Now()})
	body := f3GetPage(t, srv, "/drops/claims", "en")
	if !strings.Contains(body, "Session-scoped") {
		t.Error("Claims must render the S-SESS session-scope banner")
	}
	if strings.Contains(body, "drops.claims.col.time") || strings.Contains(body, ">Time<") {
		t.Error("Claims must not render a fabricated per-row timestamp column")
	}
}

// TestS5_4ClaimsUnknownNeverBecomesPositiveOrNegative proves a drop with no
// authoritative claim evidence renders state=unknown, never claimed/
// claimable/failed/completed/delivered. M6 target: promoting unknown to
// claimed/completed must fail this test.
func TestS5_4ClaimsUnknownNeverBecomesPositiveOrNegative(t *testing.T) {
	unobserved := s54UnobservedDrop("Mystery", 60)
	c := s54Campaign("c1", "Camp", "Game", []*models.Drop{unobserved})
	rows := buildDropsClaimsRows([]*models.Campaign{c}, enTR(t))
	if len(rows) != 1 {
		t.Fatalf("expected exactly one claim row, got %d", len(rows))
	}
	if rows[0].State != "unknown" {
		t.Errorf("a drop with no authoritative claim evidence must be state=unknown, got %q", rows[0].State)
	}
}

// TestS5_4ClaimsStateMapping proves all four claim states are correctly
// derived from real, already-available evidence: claimed (ClaimedDropNames),
// claimable (ClaimabilityKnownTrue), in_progress (ClaimabilityKnownFalse,
// still earning) and unknown (ClaimabilityUnknown).
func TestS5_4ClaimsStateMapping(t *testing.T) {
	c := s54Campaign("c1", "Camp", "Game", []*models.Drop{
		s54ClaimableDrop("Claimable1", 10, 10),
		s54InProgressDrop("Progress1", 3, 10),
		s54UnobservedDrop("Unknown1", 10),
	})
	c.ClaimedDropNames = []string{"AlreadyClaimed1"}
	rows := buildDropsClaimsRows([]*models.Campaign{c}, enTR(t))

	byName := map[string]ClaimRowView{}
	for _, r := range rows {
		byName[r.DropName] = r
	}
	if byName["AlreadyClaimed1"].State != "claimed" {
		t.Errorf("expected claimed state, got %+v", byName["AlreadyClaimed1"])
	}
	if byName["Claimable1"].State != "claimable" {
		t.Errorf("expected claimable state, got %+v", byName["Claimable1"])
	}
	if byName["Progress1"].State != "in_progress" {
		t.Errorf("expected in_progress state, got %+v", byName["Progress1"])
	}
	if byName["Unknown1"].State != "unknown" {
		t.Errorf("expected unknown state, got %+v", byName["Unknown1"])
	}
}

// TestS5_4DistinctClaimCampaignOptionsDedupesByIDPreservesOrder proves the
// campaign filter's option list is order-preserving, deduped by CampaignID
// (not by name, so two same-named campaign instances stay distinct options),
// and skips rows with no campaign ID at all.
func TestS5_4DistinctClaimCampaignOptionsDedupesByIDPreservesOrder(t *testing.T) {
	rows := []ClaimRowView{
		{CampaignID: "c1", CampaignName: "First"},
		{CampaignID: "c2", CampaignName: "Second"},
		{CampaignID: "c1", CampaignName: "First"},  // repeat of c1, must not duplicate
		{CampaignID: "", CampaignName: "No ID"},    // no campaign id, must be skipped
		{CampaignID: "c3", CampaignName: "Second"}, // same NAME as c2, different ID: distinct option
	}
	got := distinctClaimCampaignOptions(rows)
	want := []ClaimCampaignOption{
		{ID: "c1", Name: "First"},
		{ID: "c2", Name: "Second"},
		{ID: "c3", Name: "Second"},
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d options, got %d: %+v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("option %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestS5_4ClaimsClaimedWithinDropsListStillMapsToClaimed proves
// buildDropsClaimsRows' defensive d.IsClaimed branch (a drop still present in
// Campaign.Drops that is nonetheless already claimed — normally already
// claimed drops are stripped into ClaimedDropNames, but this guards against
// that not yet having happened) also maps to state=claimed, not unknown or
// in_progress.
func TestS5_4ClaimsClaimedWithinDropsListStillMapsToClaimed(t *testing.T) {
	c := s54Campaign("c1", "Camp", "Game", []*models.Drop{s54ClaimedDrop("StillListed", 60)})
	rows := buildDropsClaimsRows([]*models.Campaign{c}, enTR(t))
	if len(rows) != 1 || rows[0].State != "claimed" {
		t.Errorf("expected the still-listed already-claimed drop to map to state=claimed, got %+v", rows)
	}
}

// TestS5_4ClaimsUnavailableWhenNoProvider proves Claims renders an honest
// "unavailable" message (never "no claims") when no campaigns provider is
// wired at all.
func TestS5_4ClaimsUnavailableWhenNoProvider(t *testing.T) {
	srv := buildF3PageServer(t)
	srv.SetCampaignsProvider(nil)
	body := f3GetPage(t, srv, "/drops/claims", "en")
	if !strings.Contains(body, "Drops tracking is not available") {
		t.Error("expected the honest unavailable message when no campaigns provider is wired")
	}
}

// TestS5_4ClaimsFilterMarkupPresent proves the campaign/state filters exist
// and that no period/time filter is offered (no authoritative claim-time
// evidence exists yet to filter on).
func TestS5_4ClaimsFilterMarkupPresent(t *testing.T) {
	c := s54Campaign("c1", "Camp", "Game", []*models.Drop{s54ClaimableDrop("D1", 5, 10)})
	srv := s54ServerWith(t, []*models.Campaign{c}, drops.SyncStatus{LastSyncAt: time.Now(), LastSuccessAt: time.Now()})
	body := f3GetPage(t, srv, "/drops/claims", "en")
	for _, want := range []string{`id="claims-filter-campaign"`, `id="claims-filter-state"`, `id="claims-filter-reset"`} {
		if !strings.Contains(body, want) {
			t.Errorf("Claims missing filter control %q", want)
		}
	}
	if strings.Contains(body, "claims-filter-period") {
		t.Error("Claims must not offer a period/time filter (no claim-time evidence exists)")
	}
}

// s54ClaimsResetHandlerRe extracts the reset button's click-handler body, so
// TestS5_4ClaimsFilterScriptContract can prove it clears both filters before
// the same applyFilter recomputes visibility, not just cosmetically resets
// the <select>s while a stale filter stays in effect.
var s54ClaimsResetHandlerRe = regexp.MustCompile(`(?s)resetBtn\.addEventListener\('click', function \(\) \{(.*?)\}\);`)

// TestS5_4ClaimsFilterScriptContract proves the rendered filter script wires
// each control to the right element by id/selector and to the shared
// applyFilter (so a handler can never go quietly missing or point at the
// wrong target), keeps its AND predicate over both data-claim-* attributes,
// drives the no-match message off the very same per-row match flag each
// row's own visibility is toggled from (so the message can never disagree
// with what the rows show), and that resetting clears both filters before
// that shared function recomputes visibility.
//
// This is a full-literal-source assertion, not a loose substring scan: the
// entire {{define "scripts"}} block in drops_claims.html is static text with
// no template actions inside it, so the exact strings asserted here are
// byte-for-byte what ships to the browser — there is no daylight between
// "this substring is present" and "this is the source the browser parses"
// the way there would be for a templated block. What it cannot catch is a
// JS-engine-level failure unrelated to these lines (e.g. a syntax error
// elsewhere in the IIFE); proving that would need real DOM/JS execution,
// which would require a browser-automation dependency this task's allowed
// paths exclude (go.mod/go.sum) — see the PR reply for the full reasoning.
//
// CodeRabbit (PR #153, Deterministic Test Contract) first found
// TestS5_4ClaimsFilterMarkupPresent only proved the filter controls exist,
// not that any of this happens; then, reviewing that fix, found the
// strengthened version still didn't prove the handlers were connected to
// anything real rather than merely present in isolated fragments — the
// getElementById/querySelectorAll/addEventListener assertions below close
// that gap.
func TestS5_4ClaimsFilterScriptContract(t *testing.T) {
	c := s54Campaign("c1", "Camp", "Game", []*models.Drop{s54ClaimableDrop("D1", 5, 10)})
	srv := s54ServerWith(t, []*models.Campaign{c}, drops.SyncStatus{LastSyncAt: time.Now(), LastSuccessAt: time.Now()})
	body := f3GetPage(t, srv, "/drops/claims", "en")

	for _, want := range []string{
		`document.getElementById('claims-filter-campaign');`,
		`document.getElementById('claims-filter-state');`,
		`document.getElementById('claims-filter-reset');`,
		`document.querySelectorAll('[data-claim-row]')`,
		`document.getElementById('claims-no-match');`,
		`(!campaign || row.getAttribute('data-claim-campaign') === campaign) &&`,
		`(!state || row.getAttribute('data-claim-state') === state);`,
		`row.classList.toggle('hidden', !matches);`,
		`if (matches) visible++;`,
		`noMatch.classList.toggle('hidden', visible !== 0);`,
		`campaignSel.addEventListener('change', applyFilter);`,
		`stateSel.addEventListener('change', applyFilter);`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Claims filter script missing expected contract fragment %q", want)
		}
	}

	resetMatch := s54ClaimsResetHandlerRe.FindStringSubmatch(body)
	if resetMatch == nil {
		t.Fatal("could not locate the reset button's click handler in the rendered script")
	}
	resetBody := resetMatch[1]
	campaignClearIdx := strings.Index(resetBody, "campaignSel.value = '';")
	stateClearIdx := strings.Index(resetBody, "stateSel.value = '';")
	reapplyIdx := strings.Index(resetBody, "applyFilter();")
	if campaignClearIdx == -1 || stateClearIdx == -1 || reapplyIdx == -1 {
		t.Fatalf("reset handler missing one of clear-campaign/clear-state/reapply: %q", resetBody)
	}
	if campaignClearIdx >= reapplyIdx || stateClearIdx >= reapplyIdx {
		t.Error("reset handler must clear both filters before recomputing visibility, not after")
	}
}

// s54ClaimsTableBodyRe and s54ClaimsCardListRe isolate the table and card
// renderings of the claim rows, so TestS5_4ClaimsTableAndCardRowsSynchronized
// can compare the two representations directly instead of assuming they stay
// in lockstep just because both range over the same view-model slice.
var s54ClaimsTableBodyRe = regexp.MustCompile(`(?s)<tbody id="claims-table-body">(.*?)</tbody>`)
var s54ClaimsCardListRe = regexp.MustCompile(`(?s)<ul[^>]*id="claims-cards"[^>]*>(.*?)</ul>`)
var s54ClaimRowAttrsRe = regexp.MustCompile(`data-claim-campaign="([^"]*)" data-claim-state="([^"]*)"`)

func s54ExtractClaimPairs(section string) [][2]string {
	matches := s54ClaimRowAttrsRe.FindAllStringSubmatch(section, -1)
	pairs := make([][2]string, len(matches))
	for i, m := range matches {
		pairs[i] = [2]string{m[1], m[2]}
	}
	return pairs
}

// TestS5_4ClaimsTableAndCardRowsSynchronized proves the desktop table and the
// mobile card list render the identical campaign/state row sequence, in the
// same order, for a fixture spanning all four claim states across two
// campaigns. The client-side filter script relies on this: it queries
// [data-claim-row] once and applies one predicate to every match, so if the
// two representations ever diverged (different row set, different order),
// the filters would silently desync from whichever one the user is looking
// at. This is a server-rendered-markup assertion — it needs no browser.
func TestS5_4ClaimsTableAndCardRowsSynchronized(t *testing.T) {
	c1 := s54Campaign("c1", "Camp One", "Game One", []*models.Drop{
		s54ClaimableDrop("D1", 5, 10),
		s54InProgressDrop("D2", 3, 10),
	})
	c2 := s54Campaign("c2", "Camp Two", "Game Two", []*models.Drop{
		s54UnobservedDrop("D3", 20),
		s54ClaimedDrop("D4", 15),
	})
	srv := s54ServerWith(t, []*models.Campaign{c1, c2}, drops.SyncStatus{LastSyncAt: time.Now(), LastSuccessAt: time.Now()})
	body := f3GetPage(t, srv, "/drops/claims", "en")

	tableMatch := s54ClaimsTableBodyRe.FindStringSubmatch(body)
	cardMatch := s54ClaimsCardListRe.FindStringSubmatch(body)
	if tableMatch == nil || cardMatch == nil {
		t.Fatalf("could not locate the claims table body / card list in the rendered page")
	}
	tablePairs := s54ExtractClaimPairs(tableMatch[1])
	cardPairs := s54ExtractClaimPairs(cardMatch[1])

	if len(tablePairs) != 4 {
		t.Fatalf("expected 4 rows in the table body, got %d: %+v", len(tablePairs), tablePairs)
	}
	if len(tablePairs) != len(cardPairs) {
		t.Fatalf("table has %d rows but the card list has %d — they must render the identical row set", len(tablePairs), len(cardPairs))
	}
	for i := range tablePairs {
		if tablePairs[i] != cardPairs[i] {
			t.Errorf("row %d: table has campaign=%q state=%q but card has campaign=%q state=%q — table and card representations must stay in lockstep", i, tablePairs[i][0], tablePairs[i][1], cardPairs[i][0], cardPairs[i][1])
		}
	}
}

// s54ClaimsNoMatchOpenTagRe isolates the #claims-no-match element's own
// opening tag, so TestS5_4ClaimsNoMatchIsAnAccessibleLiveRegion can prove
// role="status" and aria-live="polite" are attributes of that exact
// element — not merely present somewhere else on the page, which a plain
// strings.Contains scan of the whole body would let slip past (e.g. if the
// attributes moved onto an unrelated wrapper while remaining, coincidentally,
// elsewhere in the markup).
var s54ClaimsNoMatchOpenTagRe = regexp.MustCompile(`<p id="claims-no-match"[^>]*>`)

// TestS5_4ClaimsNoMatchIsAnAccessibleLiveRegion proves the Claims "no
// results" paragraph — the element TestS5_4ClaimsFilterScriptContract already
// proves the filter script's noMatch reference toggles visible — is itself
// announced to assistive technology: id, role="status" and
// aria-live="polite" must all be attributes of that one opening tag.
func TestS5_4ClaimsNoMatchIsAnAccessibleLiveRegion(t *testing.T) {
	c := s54Campaign("c1", "Camp", "Game", []*models.Drop{s54ClaimableDrop("D1", 5, 10)})
	srv := s54ServerWith(t, []*models.Campaign{c}, drops.SyncStatus{LastSyncAt: time.Now(), LastSuccessAt: time.Now()})
	body := f3GetPage(t, srv, "/drops/claims", "en")

	tag := s54ClaimsNoMatchOpenTagRe.FindString(body)
	if tag == "" {
		t.Fatal("could not locate the #claims-no-match opening tag in the rendered page")
	}
	for _, want := range []string{`id="claims-no-match"`, `role="status"`, `aria-live="polite"`} {
		if !strings.Contains(tag, want) {
			t.Errorf("#claims-no-match opening tag %q missing %q — the live-region contract must live on this exact element", tag, want)
		}
	}
}

// ---------------------------------------------------------------------
// Past
// ---------------------------------------------------------------------

// TestS5_4PastLinksToClaimsAndSoftensNotClaimed proves Past defers claim
// detail to /drops/claims (the authority rule) and never presents its
// last-observed catalog snapshot as an unqualified certainty. M5 target:
// Past independently asserting a claim outcome (rather than deferring) must
// fail this test.
func TestS5_4PastLinksToClaimsAndSoftensNotClaimed(t *testing.T) {
	srv := buildF3PageServer(t) // f3BuildPast: one claimed + two unclaimed instances
	body := f3GetPage(t, srv, "/api/drops/past", "en")
	if !strings.Contains(body, `href="/drops/claims"`) {
		t.Error("Past must link to /drops/claims for claim detail (task S5-4 authority rule)")
	}
	if !strings.Contains(body, "Not claimed (last observed)") {
		t.Error("Past's not-claimed label must be time-scoped, never presented as an unqualified certainty")
	}
}

// ---------------------------------------------------------------------
// i18n / a11y
// ---------------------------------------------------------------------

// TestS5_4NewLocaleKeysResolveInBothLanguages proves every new S5-4 locale
// key resolves to real, non-empty, non-fallback text in both languages.
// Cross-file EN/RU key-SET parity is already covered generically by
// internal/i18n's TestLocaleKeyParity; this test additionally pins that
// these specific new values aren't accidentally empty placeholders.
func TestS5_4NewLocaleKeysResolveInBothLanguages(t *testing.T) {
	loc, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}
	keys := []string{
		"drops.tab.claims",
		"drops.current.state.unk", "drops.current.state.degraded", "drops.current.state.attempted_at",
		"drops.current.chip.source", "drops.current.state.empty", "drops.current.state.empty_action_settings",
		"drops.current.state.empty_see_upcoming",
		// Pre-existing key (not new), but the R17 empty/aged states newly
		// depend on it — pinned here so a future removal is caught even
		// though TestLocaleKeyParity alone would not (it only diffs the two
		// catalogs against each other, not against actual usage).
		"drops.upcoming.last_success",
		"drops.card.dpc_badge", "drops.card.account_linked", "drops.card.account_not_linked", "drops.card.claims_link",
		"drops.past.claims_link",
		"drops.claims.subtitle", "drops.claims.session_banner", "drops.claims.empty", "drops.claims.unavailable",
		"drops.claims.table_caption", "drops.claims.col.campaign", "drops.claims.col.drop", "drops.claims.col.state",
		"drops.claims.filter.campaign", "drops.claims.filter.state", "drops.claims.filter.all", "drops.claims.filter.reset",
		"drops.claims.state.claimed", "drops.claims.state.claimable", "drops.claims.state.in_progress", "drops.claims.state.unknown",
		"drops.claims.no_match",
	}
	for _, lang := range []string{i18n.LangEN, i18n.LangRU} {
		for _, key := range keys {
			got := loc.T(lang, key)
			if got == "" || got == key {
				t.Errorf("locale %q key %q resolved to %q, want real localized text", lang, key, got)
			}
		}
	}
}

// TestS5_4LocalizedA11yLabelsBothLanguages proves the Drops nav link and the
// campaign card's accessible name both resolve to real, distinct text in
// both languages.
func TestS5_4LocalizedA11yLabelsBothLanguages(t *testing.T) {
	loc, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}
	for _, lang := range []string{i18n.LangEN, i18n.LangRU} {
		if got := loc.T(lang, "drops.card.show_all_aria"); got == "" || got == "drops.card.show_all_aria" {
			t.Errorf("[%s] drops.card.show_all_aria must resolve to real text, got %q", lang, got)
		}
		if got := loc.T(lang, "nav.drops"); got == "" || got == "nav.drops" {
			t.Errorf("[%s] nav.drops must resolve to real text, got %q", lang, got)
		}
	}
}

// TestS5_4DPCBadgeRUSemanticsNotInverted proves the Russian DP-C badge
// resolves to its current, correct pending-evidence meaning and not the
// earlier, semantically inverted wording: "макет по свидетельствам группы
// C" reads as "mockup based on Group C evidence" (evidence already exists),
// the opposite of what the badge means. TestS5_4DPCBadgePlacement only
// proves the EN "DP-C" marker appears/is absent in the right places on the
// rendered page; it never inspects RU content, so a silent revert of just
// the RU locale value would pass every other S5-4 test.
func TestS5_4DPCBadgeRUSemanticsNotInverted(t *testing.T) {
	loc, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}

	const wantRU = "DP-C: макет в ожидании свидетельств группы C"
	gotRU := loc.T(i18n.LangRU, "drops.card.dpc_badge")
	if gotRU != wantRU {
		t.Errorf("drops.card.dpc_badge[ru] = %q, want exactly %q", gotRU, wantRU)
	}
	if !strings.Contains(gotRU, "в ожидании") {
		t.Errorf("drops.card.dpc_badge[ru] = %q, must contain %q (evidence pending — the badge's actual meaning)", gotRU, "в ожидании")
	}
	if strings.Contains(gotRU, "по свидетельствам") {
		t.Errorf("drops.card.dpc_badge[ru] = %q, must not contain the semantically inverted phrase %q", gotRU, "по свидетельствам")
	}
	if gotRU == "" || gotRU == "drops.card.dpc_badge" {
		t.Errorf("drops.card.dpc_badge[ru] resolved to %q, want real localized text, not a key fallback", gotRU)
	}

	if gotEN := loc.T(i18n.LangEN, "drops.card.dpc_badge"); gotEN == "" || gotEN == "drops.card.dpc_badge" {
		t.Errorf("drops.card.dpc_badge[en] resolved to %q, want real localized text", gotEN)
	}
}
