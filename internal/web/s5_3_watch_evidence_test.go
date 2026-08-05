package web

// S5-3 Phase 3 tests (OD-S5-3-1): the safe watching-evidence adapter must
// copy only the allowlisted fields from the existing, unconditionally-wired
// Server.supportBundleSource, release s.mu before calling the provider, and
// never let debug.WatchSlot.Reason / debug.WaitingSlot.Reason - the free-form
// watch-selection explanation, which can embed a dynamic private value (a
// channel-points balance, a subscription points-multiplier) - reach any
// rendered HTML.

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/debug"
)

const (
	s53SentinelSlotReason    = "SENTINEL-SLOT-REASON-do-not-render-channel-points-balance-77291"
	s53SentinelWaitingReason = "SENTINEL-WAITING-REASON-do-not-render-subscription-multiplier-40218"
)

// s53FakeSnapshot returns a fake debug.Snapshot whose occupied/waiting
// entries carry a real ReasonCode (safe) alongside a unique secret sentinel
// in Reason (unsafe) - proving the adapter copies the former and structurally
// cannot copy the latter.
func s53FakeSnapshot() debug.Snapshot {
	return debug.Snapshot{
		Watching: debug.WatchingInfo{
			Mode:        "direct",
			EvaluatedAt: time.Now(),
			Slots: []debug.WatchSlot{
				{Slot: 1, Channel: "streamer_a", Source: "configured", ReasonCode: "priority", Reason: s53SentinelSlotReason, Campaign: "Autumn Drops"},
			},
			Waiting: []debug.WaitingSlot{
				{Channel: "streamer_b", Source: "configured", ReasonCode: "lower_priority", Reason: s53SentinelWaitingReason},
			},
		},
	}
}

// s53OverviewProvider is a minimal OverviewProvider reporting streamer_a as
// occupying a slot, WITHOUT ever setting the pre-existing, unrelated
// WatchSlotsView.Reason field - so any sentinel found in rendered output is
// unambiguously attributable to the new S5-3 adapter, not the pre-existing
// (and differently-scoped) card.WatchReason tooltip path.
type s53OverviewProvider struct{}

func (s53OverviewProvider) WatchSlots() WatchSlotsView {
	return WatchSlotsView{
		ActivePair: []string{"streamer_a"},
		Watching:   map[string]bool{"streamer_a": true},
		Origin:     map[string]string{"streamer_a": "configured"},
		Mode:       "direct",
	}
}
func (s53OverviewProvider) LivePredictions() []LivePrediction { return nil }

// s53LeakTestServer builds the shared F3 page fixture (streamer_a/streamer_b
// already configured/tracked) and wires the sentinel-bearing snapshot as the
// support-bundle source plus the matching overview provider.
func s53LeakTestServer(t *testing.T) *Server {
	t.Helper()
	srv := buildF3PageServer(t)
	srv.SetOverviewProvider(s53OverviewProvider{})
	srv.SetSupportBundleSource(func() debug.Snapshot { return s53FakeSnapshot() })
	return srv
}

// ---- adapter unit tests -----------------------------------------------------

// TestS5_3WatchSlotEvidenceCopiesOnlyAllowlistedFields proves the adapter's
// return value carries the safe ReasonCode/Campaign/Slot/Source/Mode fields
// but structurally cannot carry Reason: watchSlotEntry/waitingSlotEntry have
// no such field, so even reflecting over the value can't find the sentinel.
func TestS5_3WatchSlotEvidenceCopiesOnlyAllowlistedFields(t *testing.T) {
	srv := s53LeakTestServer(t)
	evidence := srv.watchSlotEvidence()

	if evidence.Mode != "direct" {
		t.Errorf("Mode = %q, want %q", evidence.Mode, "direct")
	}
	if len(evidence.Slots) != 1 || evidence.Slots[0].Channel != "streamer_a" || evidence.Slots[0].ReasonCode != "priority" || evidence.Slots[0].Campaign != "Autumn Drops" {
		t.Fatalf("Slots = %+v, want one streamer_a/priority/Autumn Drops entry", evidence.Slots)
	}
	if len(evidence.Waiting) != 1 || evidence.Waiting[0].Channel != "streamer_b" || evidence.Waiting[0].ReasonCode != "lower_priority" {
		t.Fatalf("Waiting = %+v, want one streamer_b/lower_priority entry", evidence.Waiting)
	}

	// Defense in depth: reflect over the ENTIRE returned value (as %#v would)
	// and prove the sentinel cannot appear, structurally, not just "wasn't
	// copied this time".
	dump := reflectDump(evidence)
	if strings.Contains(dump, s53SentinelSlotReason) || strings.Contains(dump, s53SentinelWaitingReason) {
		t.Fatalf("watchSlotEvidence value must never contain the free-form Reason sentinel, got: %s", dump)
	}

	// No field named "Reason" exists anywhere on watchSlotEntry/waitingSlotEntry.
	for _, typ := range []reflect.Type{reflect.TypeOf(watchSlotEntry{}), reflect.TypeOf(waitingSlotEntry{})} {
		if _, ok := typ.FieldByName("Reason"); ok {
			t.Errorf("%s must not declare a Reason field at all", typ.Name())
		}
	}
}

// reflectDump renders v the same way %#v would, for a defense-in-depth
// substring check independent of any specific rendering path.
func reflectDump(v interface{}) string {
	return reflectString(reflect.ValueOf(v))
}

func reflectString(v reflect.Value) string {
	var b strings.Builder
	var walk func(reflect.Value)
	walk = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				walk(v.Field(i))
			}
		case reflect.Slice, reflect.Array:
			for i := 0; i < v.Len(); i++ {
				walk(v.Index(i))
			}
		case reflect.String:
			b.WriteString(v.String())
			b.WriteString(" ")
		}
	}
	walk(v)
	return b.String()
}

// TestS5_3WatchSlotEvidenceNilProviderNormalizesToZeroValue proves a nil
// provider (never wired) degrades to the honest S-NOBACK/S-UNK zero value,
// never a panic, never an error surfaced to a caller.
func TestS5_3WatchSlotEvidenceNilProviderNormalizesToZeroValue(t *testing.T) {
	srv := newRenderServer(t)
	evidence := srv.watchSlotEvidence()
	if evidence.Mode != "" || len(evidence.Slots) != 0 || len(evidence.Waiting) != 0 {
		t.Errorf("nil provider must normalize to the zero value, got %+v", evidence)
	}
}

// TestS5_3WatchSlotEvidencePanickingProviderNormalizesToZeroValue proves a
// panicking provider (recovered by the shared safeSupportBundleSnapshot
// helper) also degrades to the zero value rather than crashing the request.
func TestS5_3WatchSlotEvidencePanickingProviderNormalizesToZeroValue(t *testing.T) {
	srv := newRenderServer(t)
	srv.SetSupportBundleSource(func() debug.Snapshot { panic("boom") })
	evidence := srv.watchSlotEvidence()
	if evidence.Mode != "" || len(evidence.Slots) != 0 {
		t.Errorf("panicking provider must normalize to the zero value, got %+v", evidence)
	}
}

// TestS5_3WatchSlotEvidenceReleasesLockBeforeCallingProvider proves the
// adapter reads the provider pointer under s.mu, releases it, and only THEN
// calls the provider (OD-S5-3-1 item 5): if the lock were still held during
// the call, a provider that itself takes s.mu.Lock() (as every other Set*/
// read path in this package does) would deadlock forever against the
// adapter's read lock. Bounded by a timeout so a regression fails fast
// instead of hanging the test run.
func TestS5_3WatchSlotEvidenceReleasesLockBeforeCallingProvider(t *testing.T) {
	srv := newRenderServer(t)
	srv.SetSupportBundleSource(func() debug.Snapshot {
		srv.SetDebugURL("probe") // takes s.mu.Lock() - would deadlock if s.mu.RLock() were still held
		return debug.Snapshot{}
	})

	done := make(chan struct{})
	go func() {
		_ = srv.watchSlotEvidence()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchSlotEvidence deadlocked calling the provider - it must release s.mu first")
	}
}

// ---- rendered-HTML leak proof (OD-S5-3-1 item 10) --------------------------

// TestS5_3FreeFormReasonNeverLeaksIntoOverviewOrSidebar proves the secret
// sentinels in debug.WatchSlot.Reason / debug.WaitingSlot.Reason never appear
// on ANY S5-3 surface that renders watch-slot evidence - /overview,
// /overview/queue, the sidebar Now Watching partial (/api/now-watching), and
// the Overview live partial (/api/overview) - in either language (Q3
// MINOR-2: extended from /overview + /api/now-watching only). The SAFE,
// closed-enum ReasonCode evidence ("priority") must still surface on the
// queue page, so an assertion of "sentinel absent" is never vacuously true
// because nothing rendered at all.
func TestS5_3FreeFormReasonNeverLeaksIntoOverviewOrSidebar(t *testing.T) {
	srv := s53LeakTestServer(t)
	h := srv.handler()

	paths := []string{"/overview", "/overview/queue", "/api/now-watching", "/api/overview"}
	for _, path := range paths {
		for _, lang := range []string{"en", "ru"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.AddCookie(&http.Cookie{Name: langCookieName, Value: lang})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("[%s] GET %s = %d, want 200", lang, path, rec.Code)
			}
			body := rec.Body.String()
			if strings.Contains(body, s53SentinelSlotReason) {
				t.Errorf("[%s] %s leaked the occupied-slot free-form Reason sentinel", lang, path)
			}
			if strings.Contains(body, s53SentinelWaitingReason) {
				t.Errorf("[%s] %s leaked the waiting-slot free-form Reason sentinel", lang, path)
			}
		}
	}

	// Not vacuous: the safe, closed-enum ReasonCode evidence ("priority")
	// does surface on the queue page, in both languages.
	for _, lang := range []string{"en", "ru"} {
		req := httptest.NewRequest(http.MethodGet, "/overview/queue", nil)
		req.AddCookie(&http.Cookie{Name: langCookieName, Value: lang})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		body := rec.Body.String()
		want := srv.i18n.T(lang, "queue.reason.priority")
		if !strings.Contains(body, want) {
			t.Errorf("[%s] /overview/queue must still render the safe ReasonCode evidence %q - otherwise the sentinel-absence check above is vacuous", lang, want)
		}
	}
}

// ---- item I: single evidence snapshot per buildOverviewData execution -----

// TestS5_3BuildOverviewDataReadsEvidenceOnce proves buildOverviewData calls
// the support-bundle provider exactly once per execution (CodeRabbit PR152
// finding: buildNowWatching used to call s.watchSlotEvidence() again on its
// own, on top of the explicit read used for SlotPair/SlotPairProvenance -
// two provider calls, two debug.Snapshot builds, per /overview render).
func TestS5_3BuildOverviewDataReadsEvidenceOnce(t *testing.T) {
	srv := buildF3PageServer(t)
	var calls int32
	srv.SetSupportBundleSource(func() debug.Snapshot {
		atomic.AddInt32(&calls, 1)
		return debug.Snapshot{Watching: debug.WatchingInfo{Mode: "idle", EvaluatedAt: time.Now()}}
	})

	srv.buildOverviewData("en")

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("support-bundle provider called %d times during one buildOverviewData execution, want exactly 1", got)
	}
}

// TestS5_3BuildOverviewDataSidebarAndSlotPairShareOneEvidenceTick proves the
// sidebar pair (NowWatching.EmptyPad) and the Overview slot pair (SlotPair)
// can never observe two different broker ticks within one response: with an
// evidence source that alternates Mode between calls, a regression that
// re-reads the provider a second time would make the two disagree.
func TestS5_3BuildOverviewDataSidebarAndSlotPairShareOneEvidenceTick(t *testing.T) {
	srv := buildF3PageServer(t)
	var calls int32
	srv.SetSupportBundleSource(func() debug.Snapshot {
		n := atomic.AddInt32(&calls, 1)
		mode := "idle"
		if n%2 == 0 {
			mode = "direct" // deliberately NOT idle, so a second read is observably different
		}
		return debug.Snapshot{Watching: debug.WatchingInfo{Mode: mode, EvaluatedAt: time.Now()}}
	})

	data := srv.buildOverviewData("en")

	if len(data.NowWatching.EmptyPad) == 0 {
		t.Fatal("expected empty sidebar padding with no occupied slots")
	}
	sidebarMode := data.NowWatching.EmptyPad[0].EmptyReasonMode
	slotPairMode := data.SlotPair[0].EmptyReasonMode
	if sidebarMode != slotPairMode {
		t.Errorf("sidebar pair mode %q != Overview slot pair mode %q - the sidebar and Overview pair observed different broker ticks within one response", sidebarMode, slotPairMode)
	}
}

// TestS5_3NoNewSerializedAPIResponse proves S5-3 introduced no new JSON/API
// endpoint that could carry the evidence (task Phase 3: "No new serialized
// API response should normally exist") - the pre-existing /api/now-watching
// and /api/overview responses are HTML partials, never JSON, and no new
// route under /api/ exists for watch-slot evidence.
func TestS5_3NoNewSerializedAPIResponse(t *testing.T) {
	srv := s53LeakTestServer(t)
	h := srv.handler()

	for _, path := range []string{"/api/now-watching", "/api/overview"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200 (a non-200 error response could otherwise make the JSON check below pass vacuously)", path, rec.Code)
		}
		ct := rec.Header().Get("Content-Type")
		if strings.Contains(ct, "json") {
			t.Errorf("%s must remain an HTML partial, not JSON (Content-Type=%q)", path, ct)
		}
	}
}
