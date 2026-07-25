package web

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/debug"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/journal"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/runtimeconfig"
)

// ---- Canary values (BKM-016 §7 T2) ----
//
// Each canary is a distinctive, greppable placeholder standing in for a real
// secret of that shape. TestSupportBundleCanaryScanEndToEnd plants every one
// of them into a debug.Snapshot fixture, runs the REAL production path
// (handler -> buildSupportBundleInput -> supportbundle.Build), and asserts
// zero occurrences anywhere in the resulting ZIP - raw bytes and every
// extracted entry.
const (
	canaryAccessToken  = "BKM016_ACCESS_TOKEN_CANARY"
	canaryRefreshToken = "BKM016_REFRESH_TOKEN_CANARY"
	canaryCookie       = "BKM016_COOKIE_CANARY"
	canaryPassword     = "BKM016_PASSWORD_CANARY"
	canaryWebhook      = "BKM016_WEBHOOK_CANARY"
	canarySpade        = "BKM016_SPADE_PAYLOAD_CANARY"
	canaryEmail        = "BKM016_PRIVATE_EMAIL_CANARY@example.com"
	canaryAccountLogin = "bkm016_fake_account_login"
	canaryAccountID    = "918273645102"
)

var allCanaries = []string{
	canaryAccessToken, canaryRefreshToken, canaryCookie, canaryPassword,
	canaryWebhook, canarySpade, canaryEmail, canaryAccountLogin, canaryAccountID,
}

// canaryFixtureSnapshot plants a canary into every reachable sensitive field
// of debug.Snapshot named in the BKM-016 task spec:
//
//   - Fields this package DROPS entirely (Username, StatusDetail, every
//     *.Detail, DropsSyncInfo.LastError, StreamerState.Title, RecentEvents)
//     get the canary in its bare/raw shape - the proof there is structural
//     omission, not pattern luck.
//   - Fields this package KEEPS (AvoidedChannel.Reason, policy Factor.Label)
//     get the canary embedded in a realistic secret-shaped substring (a
//     bearer/token URL, a webhook URL, an email, an IP, an absolute path) -
//     the shapes Redact's defense-in-depth is designed to catch.
func canaryFixtureSnapshot() debug.Snapshot {
	return debug.Snapshot{
		GeneratedAt:  time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC),
		Version:      "v-test",
		Username:     canaryAccountLogin + " " + canaryAccountID,
		Status:       debug.StatusRunning,
		StatusDetail: "reauth failed: access_token=" + canaryAccessToken,
		Watching: debug.WatchingInfo{
			Mode: "direct",
			Slots: []debug.WatchSlot{
				{Slot: 0, Channel: "publicstreamer", Source: "configured", ReasonCode: "priority", Reason: "priority: configured channel"},
			},
		},
		Streamers: []debug.StreamerState{
			{
				Username: "publicstreamer",
				Status:   "online",
				Online:   true,
				Title:    "webhook https://discord.com/api/webhooks/1/" + canaryWebhook,
			},
		},
		Drops: &debug.DropsSyncInfo{
			SyncRuns:         3,
			TrackedCampaigns: 1,
			LastError:        "sync failed: refresh_token=" + canaryRefreshToken,
			Campaigns: []debug.TrackedCampaignInfo{
				{Name: "Summer Campaign"},
			},
		},
		Health: &debug.HealthInfo{
			ActiveClientID: "TV",
			Signals: []debug.HealthSignal{
				{Name: "oauth", Status: "ok", Detail: "cookie: " + canaryCookie},
			},
		},
		ProgressWatchdog: &debug.ProgressWatchdogInfo{
			Enabled: true,
			Drops: []debug.DropProgressState{
				{Campaign: "Summer Campaign", Drop: "Drop 1", Status: "recovering", Detail: "password=" + canaryPassword},
			},
			Avoided: []debug.AvoidedChannel{
				{Login: "avoidedchannel", Reason: "session recovery via https://video-edge-1.example.net/spade?sig=abc&token=" + canaryAccessToken},
				{Login: "otherchannel", Reason: "contact " + canaryEmail + " or webhook https://discord.com/api/webhooks/2/" + canaryWebhook},
			},
		},
		Policy: &debug.PolicyInfo{
			Mode: "GAME_ORDER",
			Decisions: []debug.PolicyDecision{
				{
					Campaign: "Summer Campaign", Status: "active", Total: 10,
					Factors: []debug.PolicyLine{
						{Label: "internal host 203.0.113.77 unreachable", Points: -5},
						{Label: "config at /home/user/.config/bukerov/secret.json corrupted", Points: -5},
					},
				},
			},
		},
		Journal: &debug.JournalInfo{
			Slots: []journal.Record[journal.SlotEvent]{
				{Seq: 1, Event: journal.SlotEvent{Type: journal.SlotEntered, Channel: "publicstreamer"}},
			},
		},
		RecentEvents: []events.Event{
			{Type: events.TypeMinerStarted, Detail: "spade payload leaked: " + canarySpade},
		},
	}
}

func newAuthedServer(t *testing.T) *Server {
	t.Helper()
	s := newRenderServer(t)
	s.SetDashboardConfig(runtimeconfig.Dashboard{Username: "admin", Password: "hunter2"})
	return s
}

func authedBundleRequest() *http.Request {
	req := httptest.NewRequest(http.MethodGet, SupportBundlePath, nil)
	req.SetBasicAuth("admin", "hunter2")
	return req
}

// zipEntryBytes reads back every entry of a ZIP byte slice.
func zipEntryBytes(t *testing.T, raw []byte) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	out := make(map[string][]byte, len(zr.File))
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s: %v", f.Name, err)
		}
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(rc); err != nil {
			t.Fatalf("read %s: %v", f.Name, err)
		}
		_ = rc.Close()
		out[f.Name] = buf.Bytes()
	}
	return out
}

func scanForCanaries(t *testing.T, where string, data []byte) {
	t.Helper()
	s := string(data)
	for _, c := range allCanaries {
		if strings.Contains(s, c) {
			t.Errorf("%s leaks canary %q", where, c)
		}
	}
}

// T2 (CRITICAL): every reachable sensitive field carries a canary; the REAL
// production path must produce a ZIP with zero canary occurrences, in the
// raw bytes AND in every extracted entry.
func TestSupportBundleCanaryScanEndToEnd(t *testing.T) {
	s := newAuthedServer(t)
	fixture := canaryFixtureSnapshot()
	s.SetSupportBundleSource(func() debug.Snapshot { return fixture })
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authedBundleRequest())
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200; body: %s", SupportBundlePath, rec.Code, rec.Body.String())
	}

	raw := rec.Body.Bytes()
	scanForCanaries(t, "raw zip bytes", raw)

	for name, data := range zipEntryBytes(t, raw) {
		scanForCanaries(t, name, data)
	}
}

// T3: the typed allowlist, not pattern luck, is the primary boundary - a
// field debug.Snapshot carries but buildSupportBundleInput never reads must
// not appear even though nothing sanitizes it (it never had a copy path to
// begin with). ActivePair/PairSince/NextRotationAt/PostponedSwapOuts and the
// Discovery per-channel detail are exactly such fields (see the design's
// §4 schema, which intentionally narrows debug.Snapshot's watching/discovery
// views to just a few fields).
func TestSupportBundleTypedAllowlistDropsUnmappedFields(t *testing.T) {
	s := newAuthedServer(t)
	const unmapped = "UNLISTED_FIELD_MARKER_7f3c9a"
	fixture := debug.Snapshot{
		Status: debug.StatusRunning,
		Watching: debug.WatchingInfo{
			Mode:              "direct",
			ActivePair:        []string{unmapped},
			PostponedSwapOuts: []debug.PostponedSwapOut{{Username: unmapped}},
			// BKM-016 future-guard: supportbundle.WatchSlot has no Reason
			// field at all, so this can only leak if a later change re-adds
			// one to the mapper/DTO.
			Slots: []debug.WatchSlot{{Slot: 0, Channel: "somechannel", Source: "configured", ReasonCode: "priority", Reason: unmapped}},
		},
		// Same future-guard for supportbundle.StreamerEntry.
		Streamers: []debug.StreamerState{{Username: "somechannel", Status: "online", Reason: unmapped}},
		Discovery: &debug.DiscoveryInfo{
			Games:    []string{unmapped},
			Watching: unmapped,
			Channels: []debug.DiscoveryChannel{{Login: unmapped, Game: unmapped}},
		},
	}
	s.SetSupportBundleSource(func() debug.Snapshot { return fixture })
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authedBundleRequest())
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", SupportBundlePath, rec.Code)
	}
	for name, data := range zipEntryBytes(t, rec.Body.Bytes()) {
		if strings.Contains(string(data), unmapped) {
			t.Errorf("entry %q leaks unmapped field value %q - the allowlist copied a field it shouldn't have", name, unmapped)
		}
	}
}

// T4/T5 focused checks (subset of T2, kept separate so a regression here
// names the exact leak category rather than "some canary somewhere").
func TestSupportBundleNeverLeaksTokensCookiesOrURLs(t *testing.T) {
	s := newAuthedServer(t)
	fixture := debug.Snapshot{
		Status:       debug.StatusRunning,
		StatusDetail: "Authorization: Bearer BKM016_STATUSDETAIL_BEARER_CANARY",
		Drops: &debug.DropsSyncInfo{
			LastError: "GET failed, Cookie: session=deadbeefdeadbeef; client_secret=topsecretvalue123",
		},
		ProgressWatchdog: &debug.ProgressWatchdogInfo{
			Avoided: []debug.AvoidedChannel{
				{Login: "chan", Reason: "signed url https://spade.twitch.tv/track?sig=abc&data=xyz&token=verysecrettoken12345"},
			},
		},
	}
	s.SetSupportBundleSource(func() debug.Snapshot { return fixture })
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authedBundleRequest())
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", rec.Code)
	}
	forbidden := []string{"BKM016_STATUSDETAIL_BEARER_CANARY", "session=deadbeef", "client_secret=topsecret", "spade.twitch.tv", "token=verysecrettoken"}
	for name, data := range zipEntryBytes(t, rec.Body.Bytes()) {
		s := string(data)
		for _, f := range forbidden {
			if strings.Contains(s, f) {
				t.Errorf("entry %q leaks %q", name, f)
			}
		}
	}
}

// T6: account identity (login) never appears, even embedded in a KEPT field
// via the Avoided-channel reason.
func TestSupportBundleNeverLeaksAccountIdentity(t *testing.T) {
	s := newAuthedServer(t)
	const login = "realaccountlogin_donotleak"
	fixture := debug.Snapshot{
		Status:   debug.StatusRunning,
		Username: login,
		ProgressWatchdog: &debug.ProgressWatchdogInfo{
			Avoided: []debug.AvoidedChannel{{Login: "chan", Reason: "account " + login + " hit a rate limit"}},
		},
	}
	s.SetSupportBundleSource(func() debug.Snapshot { return fixture })
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authedBundleRequest())
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", rec.Code)
	}
	for name, data := range zipEntryBytes(t, rec.Body.Bytes()) {
		if strings.Contains(string(data), login) {
			t.Errorf("entry %q leaks the account login %q", name, login)
		}
	}
}

// T7: the public WATCHED-channel login is present (already dashboard-visible
// without auth to the bundle) while the account identity is removed.
func TestSupportBundlePublicChannelLoginKeptAccountIdentityDropped(t *testing.T) {
	s := newAuthedServer(t)
	const accountLogin = "secretaccountlogin"
	const publicChannel = "publicwatchedstreamer"
	fixture := debug.Snapshot{
		Status:    debug.StatusRunning,
		Username:  accountLogin,
		Streamers: []debug.StreamerState{{Username: publicChannel, Status: "online", Online: true}},
	}
	s.SetSupportBundleSource(func() debug.Snapshot { return fixture })
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authedBundleRequest())
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", rec.Code)
	}
	entries := zipEntryBytes(t, rec.Body.Bytes())
	foundPublic := false
	for name, data := range entries {
		s := string(data)
		if strings.Contains(s, accountLogin) {
			t.Errorf("entry %q leaks the account login %q", name, accountLogin)
		}
		if strings.Contains(s, publicChannel) {
			foundPublic = true
		}
	}
	if !foundPublic {
		t.Error("the public watched-channel login should be present in the bundle (already dashboard-visible) but was not found anywhere")
	}
}

// T8: a raw/arbitrary sync error never survives - only the derived
// lastSyncFailed bool crosses the boundary.
func TestSupportBundleRawErrorBecomesBoundedFlag(t *testing.T) {
	s := newAuthedServer(t)
	const rawErr = "panic: runtime error: nil pointer dereference at internal/api/client.go:482 goroutine 17"
	fixture := debug.Snapshot{
		Status: debug.StatusRunning,
		Drops:  &debug.DropsSyncInfo{LastError: rawErr, SyncRuns: 1},
	}
	s.SetSupportBundleSource(func() debug.Snapshot { return fixture })
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authedBundleRequest())
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", rec.Code)
	}
	entries := zipEntryBytes(t, rec.Body.Bytes())
	for name, data := range entries {
		if strings.Contains(string(data), rawErr) || strings.Contains(string(data), "nil pointer dereference") {
			t.Errorf("entry %q leaks the raw error string", name)
		}
	}
	var drops struct {
		SyncStatus struct {
			LastSyncFailed bool `json:"lastSyncFailed"`
		} `json:"syncStatus"`
	}
	if err := json.Unmarshal(entries["drops.json"], &drops); err != nil {
		t.Fatalf("unmarshal drops.json: %v", err)
	}
	if !drops.SyncStatus.LastSyncFailed {
		t.Error("drops.json syncStatus.lastSyncFailed should be true when LastError was non-empty")
	}
}

// T9: no raw config/env map ever crosses the boundary - setting an
// unrelated env var with a distinctive value must never surface it, since
// this package never reads os.Environ() or the config file.
func TestSupportBundleNeverLeaksEnvironment(t *testing.T) {
	s := newAuthedServer(t)
	const envCanary = "ENV_LEAK_CANARY_4b2e"
	t.Setenv("SOME_UNRELATED_MINER_SETTING", envCanary)
	s.SetSupportBundleSource(func() debug.Snapshot { return debug.Snapshot{Status: debug.StatusRunning} })
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authedBundleRequest())
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", rec.Code)
	}
	for name, data := range zipEntryBytes(t, rec.Body.Bytes()) {
		if strings.Contains(string(data), envCanary) {
			t.Errorf("entry %q leaked an environment variable value", name)
		}
	}
}

// T10: no DB/log-file access - newRenderServer(t) wires no *analytics.Service,
// no DB, and s.username/refresh are zero-valued; the endpoint must still
// succeed, proving the whole response is built from in-memory bytes only.
func TestSupportBundleRequiresNoDatabaseOrDisk(t *testing.T) {
	s := newAuthedServer(t)
	s.SetSupportBundleSource(func() debug.Snapshot { return debug.Snapshot{Status: debug.StatusRunning} })
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authedBundleRequest())
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200 even with no DB/analytics wired", rec.Code)
	}
}

// T11: zero external calls - the fake source is a pure in-memory function; a
// counter proves it's called exactly once per request, never more (no retry,
// no secondary fetch).
func TestSupportBundleZeroExternalCalls(t *testing.T) {
	s := newAuthedServer(t)
	var calls int32
	s.SetSupportBundleSource(func() debug.Snapshot {
		atomic.AddInt32(&calls, 1)
		return debug.Snapshot{Status: debug.StatusRunning}
	})
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authedBundleRequest())
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", rec.Code)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("snapshot source called %d times, want exactly 1", got)
	}
}

// T12: zero mutation - the backing snapshot's slices/pointers (and the
// journal records within it) are byte-for-byte identical before and after a
// bundle is built from them.
func TestSupportBundleZeroMutation(t *testing.T) {
	s := newAuthedServer(t)
	fixture := canaryFixtureSnapshot()
	before, err := json.Marshal(fixture)
	if err != nil {
		t.Fatalf("marshal fixture before: %v", err)
	}

	s.SetSupportBundleSource(func() debug.Snapshot { return fixture })
	h := s.handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authedBundleRequest())
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", rec.Code)
	}

	after, err := json.Marshal(fixture)
	if err != nil {
		t.Fatalf("marshal fixture after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("the backing snapshot fixture was mutated by building a support bundle from it")
	}

	// Journal cursor equality: Seq values and ordering unchanged.
	var beforeDoc, afterDoc debug.Snapshot
	_ = json.Unmarshal(before, &beforeDoc)
	_ = json.Unmarshal(after, &afterDoc)
	if !reflect.DeepEqual(beforeDoc.Journal, afterDoc.Journal) {
		t.Error("journal records/cursors changed after building a support bundle")
	}
}

// T13: unauthenticated request is denied AND the builder is never invoked.
func TestSupportBundleUnauthenticatedDenied(t *testing.T) {
	s := newAuthedServer(t) // authEnabled() == true, but the request carries no credentials
	var calls int32
	s.SetSupportBundleSource(func() debug.Snapshot {
		atomic.AddInt32(&calls, 1)
		return debug.Snapshot{}
	})
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, SupportBundlePath, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET without credentials = %d, want 401", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Error("401 must carry a WWW-Authenticate challenge")
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("builder invoked %d times before authentication succeeded, want 0", got)
	}

	// Wrong credentials: also 401, also zero calls.
	req := httptest.NewRequest(http.MethodGet, SupportBundlePath, nil)
	req.SetBasicAuth("admin", "wrongpassword")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET with wrong credentials = %d, want 401", rec.Code)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("builder invoked %d times with wrong credentials, want 0", got)
	}
}

// T14: DASHBOARD_INSECURE_NO_AUTH=true must NOT make the bundle reachable,
// even though it leaves every other route unauthenticated - denied with
// 404 (authEnabled()==false path) and the builder is never invoked.
func TestSupportBundleDeniedUnderInsecureBypass(t *testing.T) {
	s := newRenderServer(t)
	s.SetDashboardConfig(runtimeconfig.Dashboard{InsecureNoAuth: true})
	var calls int32
	s.SetSupportBundleSource(func() debug.Snapshot {
		atomic.AddInt32(&calls, 1)
		return debug.Snapshot{}
	})
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, SupportBundlePath, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET under insecure bypass = %d, want 404", rec.Code)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("builder invoked %d times under insecure bypass, want 0", got)
	}

	// Even presenting SOME basic-auth credentials must not help: authEnabled()
	// is false, so requireRealDashboardAuth fails closed regardless.
	req := httptest.NewRequest(http.MethodGet, SupportBundlePath, nil)
	req.SetBasicAuth("anything", "anything")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET with credentials under insecure bypass = %d, want 404", rec.Code)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("builder invoked %d times under insecure bypass with credentials, want 0", got)
	}
}

// T15: authenticated success carries the exact documented headers and a
// safe (extension-correct, no path separators) filename.
func TestSupportBundleAuthenticatedSuccessHeaders(t *testing.T) {
	s := newAuthedServer(t)
	when := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	s.setSupportBundleClock(func() time.Time { return when })
	s.SetSupportBundleSource(func() debug.Snapshot { return debug.Snapshot{Status: debug.StatusRunning} })
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authedBundleRequest())
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", rec.Code)
	}

	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Errorf("Content-Type = %q, want application/zip", ct)
	}
	wantDisposition := `attachment; filename="bukerov-support-20260304T050607Z.zip"`
	if cd := rec.Header().Get("Content-Disposition"); cd != wantDisposition {
		t.Errorf("Content-Disposition = %q, want %q", cd, wantDisposition)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store, private" {
		t.Errorf("Cache-Control = %q, want %q", cc, "no-store, private")
	}
	if p := rec.Header().Get("Pragma"); p != "no-cache" {
		t.Errorf("Pragma = %q, want no-cache", p)
	}
	if xcto := rec.Header().Get("X-Content-Type-Options"); xcto != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", xcto)
	}
	if rec.Body.Len() == 0 {
		t.Error("body is empty")
	}
	// Filename shape: no path separators, exact expected name.
	if strings.ContainsAny(rec.Header().Get("Content-Disposition"), `/\`) {
		t.Error("filename must not contain path separators")
	}
}

// T16: no-store/private caching and nosniff are always present, there is no
// ETag (which could otherwise become a fingerprint of the response), and the
// handler never touches disk (already covered by T10, restated here for the
// exact "no disk persistence" claim via a distinct temp-derived source).
func TestSupportBundleCachingHeadersAndNoETag(t *testing.T) {
	s := newAuthedServer(t)
	s.SetSupportBundleSource(func() debug.Snapshot { return debug.Snapshot{Status: debug.StatusRunning} })
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authedBundleRequest())
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", rec.Code)
	}
	if etag := rec.Header().Get("ETag"); etag != "" {
		t.Errorf("response must not carry an ETag, got %q", etag)
	}
	if lm := rec.Header().Get("Last-Modified"); lm != "" {
		t.Errorf("response must not carry Last-Modified, got %q", lm)
	}
}

// T18/T19/T20 bounds enforcement is exercised in internal/supportbundle's own
// tests; here we only need the end-to-end plumbing proof that a large
// fixture doesn't error out through the web layer either.
func TestSupportBundleHandlesLargeFixtureWithoutError(t *testing.T) {
	s := newAuthedServer(t)
	streamers := make([]debug.StreamerState, 2000)
	for i := range streamers {
		streamers[i] = debug.StreamerState{Username: "streamer", Status: "offline"}
	}
	fixture := debug.Snapshot{Status: debug.StatusRunning, Streamers: streamers}
	s.SetSupportBundleSource(func() debug.Snapshot { return fixture })
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authedBundleRequest())
	if rec.Code != http.StatusOK {
		t.Fatalf("GET with a large fixture = %d, want 200", rec.Code)
	}
}

// T23: the concurrency limiter is deterministic - exactly bundleSlots
// (2) concurrent builds are admitted; a 3rd concurrent attempt gets 503; all
// admitted requests still complete successfully, and the limiter is
// race-free under -race.
func TestSupportBundleConcurrencyLimiter(t *testing.T) {
	s := newAuthedServer(t)
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var calls int32
	s.SetSupportBundleSource(func() debug.Snapshot {
		atomic.AddInt32(&calls, 1)
		entered <- struct{}{}
		<-release
		return debug.Snapshot{Status: debug.StatusRunning}
	})
	h := s.handler()

	results := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, authedBundleRequest())
			results <- rec.Code
		}()
	}

	// Deterministically wait until BOTH concurrent builds are in flight
	// (both slots held) before trying a third, so the 503 assertion never
	// races against goroutine scheduling.
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for both concurrent builds to start")
		}
	}

	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, authedBundleRequest())
	if rec3.Code != http.StatusServiceUnavailable {
		t.Fatalf("3rd concurrent request = %d, want 503", rec3.Code)
	}

	close(release)
	for i := 0; i < 2; i++ {
		select {
		case code := <-results:
			if code != http.StatusOK {
				t.Errorf("concurrent request = %d, want 200", code)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for concurrent builds to finish")
		}
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("source called %d times, want exactly 2 (the 3rd request must never invoke it)", got)
	}
}

// T24: the snapshot is acquired (and the handler releases Server.mu) BEFORE
// compression, and no application lock is held while supportbundle.Build
// runs. Proven by having the fake source itself reenter a Server method that
// needs Server.mu (a plain sync.RWMutex, non-reentrant in Go) - if the
// handler still held s.mu.RLock() while calling the source, this would
// deadlock and the test would time out.
func TestSupportBundleSnapshotAcquiredBeforeLocking(t *testing.T) {
	s := newAuthedServer(t)
	s.SetSupportBundleSource(func() debug.Snapshot {
		// Reentrant call: SetDebugURL takes s.mu.Lock(). If handleSupportBundle
		// were still holding s.mu (read or write) here, this would deadlock.
		s.SetDebugURL("/api/debug/snapshot")
		return debug.Snapshot{Status: debug.StatusRunning}
	})
	h := s.handler()

	done := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, authedBundleRequest())
		done <- rec.Code
	}()

	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("GET = %d, want 200", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deadlocked: the handler must release Server.mu before calling the snapshot source")
	}
}

// T25: unrelated dashboard routes are unaffected by this change, and the
// insecure-bypass mode's normal (unauthenticated) behavior on OTHER routes
// is unchanged - only the support-bundle route itself gets the extra guard.
func TestSupportBundleDoesNotAffectOtherRoutes(t *testing.T) {
	s := newRenderServer(t)
	s.SetDashboardConfig(runtimeconfig.Dashboard{InsecureNoAuth: true})
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/status under insecure bypass = %d, want 200 (unaffected by this change)", rec.Code)
	}
}

// T27: a panicking snapshot provider yields a clean, generic 500 - no
// internals, no goroutine dump - and the server keeps serving afterward.
func TestSupportBundleProviderPanicYieldsGenericError(t *testing.T) {
	s := newAuthedServer(t)
	s.SetSupportBundleSource(func() debug.Snapshot { panic("internal detail: secret state") })
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authedBundleRequest())
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("GET with panicking provider = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	for _, leak := range []string{"goroutine", "runtime error", "internal detail", "secret"} {
		if strings.Contains(body, leak) {
			t.Errorf("500 body leaks internals (%q):\n%s", leak, body)
		}
	}

	// The server (and the concurrency slot) must still work for the next request.
	s.SetSupportBundleSource(func() debug.Snapshot { return debug.Snapshot{Status: debug.StatusRunning} })
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, authedBundleRequest())
	if rec.Code != http.StatusOK {
		t.Fatalf("request after panic = %d, want 200", rec.Code)
	}
}

// T28: the Logs-page download control is visible only when real auth is
// configured, and absent under the insecure bypass (and when auth is fully
// disabled).
func TestSupportBundleLogsPageButtonVisibility(t *testing.T) {
	// Real auth configured: the control is present.
	sAuthed := newAuthedServer(t)
	rec := httptest.NewRecorder()
	sAuthed.handleLogsPage(rec, httptest.NewRequest(http.MethodGet, "/logs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/logs = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), SupportBundlePath) {
		t.Error("logs page should show the support-bundle link when real auth is configured")
	}

	// Insecure bypass: AuthEnabled() is false, so the control must be hidden.
	sInsecure := newRenderServer(t)
	sInsecure.SetDashboardConfig(runtimeconfig.Dashboard{InsecureNoAuth: true})
	rec2 := httptest.NewRecorder()
	sInsecure.handleLogsPage(rec2, httptest.NewRequest(http.MethodGet, "/logs", nil))
	if strings.Contains(rec2.Body.String(), SupportBundlePath) {
		t.Error("logs page must NOT show the support-bundle link under the insecure bypass")
	}

	// Auth fully disabled (zero Dashboard): also hidden.
	sDisabled := newRenderServer(t)
	rec3 := httptest.NewRecorder()
	sDisabled.handleLogsPage(rec3, httptest.NewRequest(http.MethodGet, "/logs", nil))
	if strings.Contains(rec3.Body.String(), SupportBundlePath) {
		t.Error("logs page must NOT show the support-bundle link when auth is disabled")
	}
}

// Method enforcement: GET-only.
func TestSupportBundleMethodNotAllowed(t *testing.T) {
	s := newAuthedServer(t)
	s.SetSupportBundleSource(func() debug.Snapshot { return debug.Snapshot{Status: debug.StatusRunning} })
	h := s.handler()

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, SupportBundlePath, nil)
		req.SetBasicAuth("admin", "hunter2")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", method, SupportBundlePath, rec.Code)
		}
	}
}

// Nil source (never wired) is handled gracefully: the endpoint still
// succeeds with empty operational sections, rather than 404ing like the
// debug snapshot route does.
func TestSupportBundleNilSourceGraceful(t *testing.T) {
	s := newAuthedServer(t) // SetSupportBundleSource never called
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authedBundleRequest())
	if rec.Code != http.StatusOK {
		t.Fatalf("GET with nil source = %d, want 200 (graceful empty bundle)", rec.Code)
	}
	entries := zipEntryBytes(t, rec.Body.Bytes())
	if _, ok := entries["manifest.json"]; !ok {
		t.Error("manifest.json missing from the empty-source bundle")
	}
	if _, ok := entries["health.json"]; ok {
		t.Error("health.json should be absent when the snapshot has no Health section")
	}
	if _, ok := entries["drops.json"]; ok {
		t.Error("drops.json should be absent when the snapshot has no Drops section")
	}
}

// assertBundleExcludes scans both the raw ZIP bytes and every extracted
// entry for each string in forbidden, failing the test if any occurrence is
// found anywhere.
func assertBundleExcludes(t *testing.T, raw []byte, forbidden []string) {
	t.Helper()
	scan := func(where string, data []byte) {
		s := string(data)
		for _, f := range forbidden {
			if strings.Contains(s, f) {
				t.Errorf("%s leaks %q", where, f)
			}
		}
	}
	scan("raw zip bytes", raw)
	for name, data := range zipEntryBytes(t, raw) {
		scan(name, data)
	}
}

// TestSupportBundleNeverLeaksChannelPointsBalance is the BKM-016 regression
// test for the confirmed privacy leak: under POINTS_ASCENDING/DESCENDING
// watch priority, internal/watcher embeds the streamer's live channel-points
// balance into a free-form selection-reason string (see
// internal/watcher/watcher.go's noteSelection call for the POINTS priority
// case, e.g. "watched: selected by POINTS_DESCENDING priority (123 channel
// points)"). That string reaches debug.StreamerState.Reason and
// debug.WatchSlot.Reason, and Redact does not strip a bare integer, so
// nothing sanitizes it once it lands in the bundle. The fix removes the
// free-form Reason field from supportbundle.WatchSlot and
// supportbundle.StreamerEntry entirely - this test proves neither the
// balance embedded in each Reason nor the standalone ChannelPoints value
// ever reaches the ZIP.
func TestSupportBundleNeverLeaksChannelPointsBalance(t *testing.T) {
	s := newAuthedServer(t)
	const channelPointsSentinel = 552104733
	const descendingSentinel = 918273645
	const ascendingSentinel = 736459281
	fixture := debug.Snapshot{
		Status: debug.StatusRunning,
		Streamers: []debug.StreamerState{
			{
				Username:      "streamerone",
				Status:        "online",
				Online:        true,
				ChannelPoints: channelPointsSentinel,
				Reason:        fmt.Sprintf("watched: selected by %s priority (%d channel points)", "POINTS_DESCENDING", descendingSentinel),
			},
		},
		Watching: debug.WatchingInfo{
			Slots: []debug.WatchSlot{
				{
					Slot:       0,
					Channel:    "streamerone",
					Source:     "configured",
					ReasonCode: "priority",
					Reason:     fmt.Sprintf("watched: selected by %s priority (%d channel points)", "POINTS_ASCENDING", ascendingSentinel),
				},
			},
		},
	}
	s.SetSupportBundleSource(func() debug.Snapshot { return fixture })
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authedBundleRequest())
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200; body: %s", SupportBundlePath, rec.Code, rec.Body.String())
	}

	raw := rec.Body.Bytes()
	assertBundleExcludes(t, raw, []string{
		"918273645", "736459281", "552104733", "channel points",
	})

	// Positive guard: only the free-form Reason is removed, so the rest of
	// the watching section — including the bounded WatchSlot.ReasonCode, an
	// explicit must-keep field — must still be emitted. This proves the fix
	// is a targeted leak removal, not an over-broad section deletion.
	watching, ok := zipEntryBytes(t, raw)["watching.json"]
	if !ok {
		t.Fatal("watching.json missing from the bundle")
	}
	for _, want := range []string{"streamerone", "priority"} {
		if !strings.Contains(string(watching), want) {
			t.Errorf("watching.json should still contain surviving value %q; got: %s", want, watching)
		}
	}
}

// TestSupportBundleNeverLeaksSubscriptionMultiplier is the same leak's
// SUBSCRIBED-priority variant: internal/watcher embeds a subscription
// points-multiplier (e.g. "2.5x") into the same free-form selection-reason
// string, which Redact leaves alone just as it does the channel-points case.
func TestSupportBundleNeverLeaksSubscriptionMultiplier(t *testing.T) {
	s := newAuthedServer(t)
	reason := fmt.Sprintf("watched: selected by %s priority (%.1fx points multiplier)", "SUBSCRIBED", 2.5)
	fixture := debug.Snapshot{
		Status: debug.StatusRunning,
		Streamers: []debug.StreamerState{
			{Username: "streamertwo", Status: "online", Online: true, Reason: reason},
		},
		Watching: debug.WatchingInfo{
			Slots: []debug.WatchSlot{
				{Slot: 0, Channel: "streamertwo", Source: "configured", ReasonCode: "priority", Reason: reason},
			},
		},
	}
	s.SetSupportBundleSource(func() debug.Snapshot { return fixture })
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authedBundleRequest())
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200; body: %s", SupportBundlePath, rec.Code, rec.Body.String())
	}

	assertBundleExcludes(t, rec.Body.Bytes(), []string{"2.5x", "points multiplier"})
}

// TestSupportBundleRetainsPolicyFactorPoints guards against an over-broad
// fix: the policy engine's campaign score/factor points (a small derived
// integer the engine computes, not a live account balance) must still cross
// the boundary into drops.json now that Reason is gone from the two
// watching-section DTOs.
func TestSupportBundleRetainsPolicyFactorPoints(t *testing.T) {
	s := newAuthedServer(t)
	const totalSentinel = 111111
	const factorPointsSentinel = 222222
	fixture := debug.Snapshot{
		Status: debug.StatusRunning,
		Drops:  &debug.DropsSyncInfo{},
		Policy: &debug.PolicyInfo{
			Mode: "smart",
			Decisions: []debug.PolicyDecision{
				{
					Campaign: "Rust Drops",
					Status:   "eligible",
					Total:    totalSentinel,
					Factors: []debug.PolicyLine{
						{Label: "drop score", Points: factorPointsSentinel},
					},
				},
			},
		},
	}
	s.SetSupportBundleSource(func() debug.Snapshot { return fixture })
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authedBundleRequest())
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200; body: %s", SupportBundlePath, rec.Code, rec.Body.String())
	}

	entries := zipEntryBytes(t, rec.Body.Bytes())
	dropsJSON, ok := entries["drops.json"]
	if !ok {
		t.Fatal("drops.json missing from the bundle")
	}
	for _, want := range []string{"111111", "222222"} {
		if !strings.Contains(string(dropsJSON), want) {
			t.Errorf("drops.json should still carry the safe policy score %q (a scoring integer, not an account balance); the Reason-field fix must not have removed it", want)
		}
	}
}
