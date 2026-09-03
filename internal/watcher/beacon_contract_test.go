package watcher

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/journal"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// --- Behavioral RED: the minute-watched WIRE CONTRACT --------------------
//
// These tests exercise the REAL production path — models.BuildMinuteWatchedPayload
// -> PlaybackSessionSnapshot.EncodePayload -> spadeFormBody -> MinuteSender.postBeacon
// — through an injected RoundTripper. Nothing here duplicates a production helper:
// the body asserted on is the exact byte string handed to http.NewRequestWithContext,
// decoded back the way the spade endpoint would decode it.
//
// No Twitch, Internet, LAN, credential, cookie, token or runtime config is touched.

// contractRT captures the exact beacon request (URL, headers, body) and answers
// the beacon with a configurable status. Non-POST requests are the HLS legs and
// succeed trivially.
type contractRT struct {
	beaconStatus int

	mu      sync.Mutex
	bodies  []string
	headers []http.Header
	urls    []string
}

func (r *contractRT) RoundTrip(req *http.Request) (*http.Response, error) {
	mk := func(status int, body string) *http.Response {
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}
	}
	switch req.Method {
	case http.MethodPost:
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.bodies = append(r.bodies, string(body))
		r.headers = append(r.headers, req.Header.Clone())
		r.urls = append(r.urls, req.URL.String())
		r.mu.Unlock()
		return mk(r.beaconStatus, ""), nil
	case http.MethodHead:
		return mk(200, ""), nil
	default:
		if strings.Contains(req.URL.Host, "variant") {
			return mk(200, "#EXTM3U\nhttps://seg.test/s.ts\n"), nil
		}
		return mk(200, "#EXTM3U\nhttps://variant.test/low.m3u8\n"), nil
	}
}

func (r *contractRT) only(t *testing.T) (body string, hdr http.Header) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.bodies) != 1 {
		t.Fatalf("expected exactly one beacon, got %d", len(r.bodies))
	}
	return r.bodies[0], r.headers[0]
}

func (r *contractRT) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

// decodeBeaconProperties reverses the exact production body encoding — form value
// "data" -> standard base64 -> JSON array of events — and returns the single
// minute-watched event's properties, decoded with UseNumber so JSON numbers stay
// distinguishable from JSON strings.
func decodeBeaconProperties(t *testing.T, body string) map[string]any {
	t.Helper()
	form, err := url.ParseQuery(body)
	if err != nil {
		t.Fatalf("beacon body is not form-encoded: %v", err)
	}
	raw, ok := form["data"]
	if !ok || len(raw) != 1 {
		t.Fatalf("beacon body must carry exactly one \"data\" value, got %v", form)
	}
	decoded, err := base64.StdEncoding.DecodeString(raw[0])
	if err != nil {
		t.Fatalf("beacon \"data\" is not standard base64: %v", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(decoded)))
	dec.UseNumber()
	var events []struct {
		Event      string         `json:"event"`
		Properties map[string]any `json:"properties"`
	}
	if err := dec.Decode(&events); err != nil {
		t.Fatalf("beacon payload is not a JSON event array: %v (raw=%s)", err, decoded)
	}
	if len(events) != 1 {
		t.Fatalf("expected exactly one event, got %d", len(events))
	}
	if events[0].Event != "minute-watched" {
		t.Fatalf("expected event \"minute-watched\", got %q", events[0].Event)
	}
	return events[0].Properties
}

// contractClientTime is the exact instant every contract fixture stamps its
// payload with, so a test can assert the value that actually reaches the wire
// rather than merely its shape.
const contractClientTime = "2026-09-03T13:24:44.123Z"

func contractClock() models.Clock {
	fixed := time.Date(2026, 9, 3, 16, 24, 44, 123456789, time.FixedZone("MSK", 3*60*60))
	return func() time.Time { return fixed }
}

// contractStreamer builds a brought-online streamer whose session carries a real
// production payload: a NUMERIC Twitch user ID, a known game, and a payload
// stamped from a fixed clock.
func contractStreamer(t *testing.T, spade string) *models.Streamer {
	t.Helper()
	s := models.NewStreamer("cyganzor", models.StreamerSettings{})
	s.ChannelID = "123456"
	s.Stream.Update("987654321", "title", &models.Game{ID: "509658", Name: "Just Chatting"}, nil, 1)
	s.Stream.SetSpadeURL(spade)
	if err := s.Stream.SetPayload("123456", "987654321", "44322889", "cyganzor", &models.Game{ID: "509658", Name: "Just Chatting"}, contractClock()); err != nil {
		t.Fatalf("fixture payload must build: %v", err)
	}
	return s
}

// TestBeaconEmitsModernMinuteWatchedContract is the primary RED. The evidenced
// modern minute-watched contract (DevilXD/TwitchDropsMiner @ 65d1092 channel.py
// _watch_payload, independently corroborated by INKCR0W/TwitchDropsMinerGo @
// 7ee5387 internal/watch/watch.go) requires every field below, with these exact
// JSON types. The legacy payload omits six of them and sends user_id as a JSON
// string, so Twitch transport-accepts the beacon without crediting the watch.
func TestBeaconEmitsModernMinuteWatchedContract(t *testing.T) {
	rt := &contractRT{beaconStatus: http.StatusNoContent}
	s := &MinuteSender{client: fakeToken{sig: "s", token: "t"}, httpClient: &http.Client{Transport: rt}}
	streamer := contractStreamer(t, "https://spade.twitch.tv/track")

	if res := s.Send(context.Background(), streamer); !res.Delivered {
		t.Fatalf("a 204 beacon must be Delivered, got %+v", res)
	}
	body, _ := rt.only(t)
	props := decodeBeaconProperties(t, body)

	wantString := map[string]string{
		"broadcast_id": "987654321",
		"channel_id":   "123456",
		"channel":      "cyganzor",
		"game":         "Just Chatting",
		"game_id":      "509658",
		"player":       "site",
	}
	for k, want := range wantString {
		got, ok := props[k]
		if !ok {
			t.Errorf("missing required field %q", k)
			continue
		}
		sv, ok := got.(string)
		if !ok {
			t.Errorf("field %q must be a JSON string, got %T (%v)", k, got, got)
			continue
		}
		if sv != want {
			t.Errorf("field %q = %q, want %q", k, sv, want)
		}
	}

	wantBool := map[string]bool{
		"hidden":    false,
		"is_live":   true,
		"live":      true,
		"logged_in": true,
		"muted":     false,
	}
	for k, want := range wantBool {
		got, ok := props[k]
		if !ok {
			t.Errorf("missing required field %q (a false-valued boolean must still be serialized)", k)
			continue
		}
		bv, ok := got.(bool)
		if !ok {
			t.Errorf("field %q must be a JSON boolean, got %T (%v)", k, got, got)
			continue
		}
		if bv != want {
			t.Errorf("field %q = %v, want %v", k, bv, want)
		}
	}

	// user_id must be a JSON NUMBER, not a JSON string. The primary reference
	// converts it at the auth boundary (twitch.py:422 int(validate_response
	// ["user_id"])) while explicitly str()-wrapping broadcast_id/channel_id/
	// game_id in the same literal — the asymmetry is deliberate.
	uid, ok := props["user_id"]
	if !ok {
		t.Errorf("missing required field \"user_id\"")
	} else if num, isNum := uid.(json.Number); !isNum {
		t.Errorf("field \"user_id\" must be a JSON number, got %T (%v)", uid, uid)
	} else if num.String() != "44322889" {
		t.Errorf("field \"user_id\" = %s, want 44322889", num.String())
	}

	// minutes_logged must be exactly the number 1.
	ml, ok := props["minutes_logged"]
	if !ok {
		t.Errorf("missing required field \"minutes_logged\"")
	} else if num, isNum := ml.(json.Number); !isNum {
		t.Errorf("field \"minutes_logged\" must be a JSON number, got %T (%v)", ml, ml)
	} else if num.String() != "1" {
		t.Errorf("field \"minutes_logged\" = %s, want 1", num.String())
	}

	// client_time must be present and an ISO-8601 UTC instant with exactly three
	// fractional digits and a literal Z (isonow(): timespec="milliseconds").
	ct, ok := props["client_time"]
	if !ok {
		t.Errorf("missing required field \"client_time\"")
	} else if sv, isStr := ct.(string); !isStr {
		t.Errorf("field \"client_time\" must be a JSON string, got %T (%v)", ct, ct)
	} else if !isMillisecondUTC(sv) {
		t.Errorf("field \"client_time\" = %q, want an ISO-8601 UTC instant like 2026-09-03T16:24:44.000Z", sv)
	} else if sv != contractClientTime {
		// Not just the right SHAPE: the exact instant the payload was BUILT with.
		// A timestamp re-derived at send time would differ here.
		t.Errorf("field \"client_time\" = %q, want the build-time value %q", sv, contractClientTime)
	}

	// The beacon must carry no credential material.
	if _, bad := props["token"]; bad {
		t.Errorf("beacon payload must never carry token material")
	}
}

// isMillisecondUTC reports whether s is exactly the wire layout: an ISO-8601 UTC
// instant with three fractional digits and a literal Z. It parses against the
// exported layout constant rather than restating it, and checks the length so a
// value Go would accept loosely cannot slip through.
func isMillisecondUTC(s string) bool {
	if len(s) != len(models.MinuteWatchedClientTimeLayout) {
		return false
	}
	parsed, err := time.Parse(models.MinuteWatchedClientTimeLayout, s)
	if err != nil {
		return false
	}
	return parsed.Format(models.MinuteWatchedClientTimeLayout) == s
}

// TestBeaconHTTP200IsNotDelivered is the second RED. Both modern references treat
// ONLY 204 No Content as a credited minute-watched beacon (channel.py:493
// `return response.status == 204`; watch.go `response.StatusCode ==
// http.StatusNoContent`). The legacy code accepts 200 as success too, which is
// exactly the false-positive Delivered state the production incident showed:
// transport-accepted, never credited.
func TestBeaconHTTP200IsNotDelivered(t *testing.T) {
	rt := &contractRT{beaconStatus: http.StatusOK}
	s := &MinuteSender{client: fakeToken{sig: "s", token: "t"}, httpClient: &http.Client{Transport: rt}}
	streamer := contractStreamer(t, "https://spade.twitch.tv/track")

	res := s.Send(context.Background(), streamer)
	if res.Delivered {
		t.Fatalf("HTTP 200 must NOT be Delivered — Twitch does not credit it")
	}
	if res.Failure == nil {
		t.Fatalf("HTTP 200 must produce a bounded beacon failure, got %+v", res)
	}
	if res.Failure.Stage != StageBeacon {
		t.Errorf("stage = %q, want %q", res.Failure.Stage, StageBeacon)
	}
	if res.Failure.Status != http.StatusOK {
		t.Errorf("status = %d, want 200", res.Failure.Status)
	}
	if res.Failure.ErrorCode != "beacon_http_200" {
		t.Errorf("error code = %q, want %q", res.Failure.ErrorCode, "beacon_http_200")
	}
	if rt.count() != 1 {
		t.Errorf("expected exactly one beacon attempt, got %d", rt.count())
	}
}

// TestBeaconHTTP204IsDelivered pins the positive half of the strict status rule.
func TestBeaconHTTP204IsDelivered(t *testing.T) {
	rt := &contractRT{beaconStatus: http.StatusNoContent}
	s := &MinuteSender{client: fakeToken{sig: "s", token: "t"}, httpClient: &http.Client{Transport: rt}}
	streamer := contractStreamer(t, "https://spade.twitch.tv/track")

	res := s.Send(context.Background(), streamer)
	if !res.Delivered || res.Failure != nil {
		t.Fatalf("HTTP 204 must be Delivered with no failure, got %+v", res)
	}
}

// TestBeaconCarriesNoCredentialHeaders pins the sensitive-header decision: the
// primary reference sends NO Authorization/Client-Id/Device-Id/Referer/Cookie to
// the spade endpoint (twitch.py: _AuthState.headers() is passed only to the
// CLIENT_URL GET and the GQL POST). The repaired beacon must stay credential-free.
func TestBeaconCarriesNoCredentialHeaders(t *testing.T) {
	rt := &contractRT{beaconStatus: http.StatusNoContent}
	s := &MinuteSender{client: fakeToken{sig: "SECRETSIG", token: "SECRETTOKEN"}, httpClient: &http.Client{Transport: rt}}
	streamer := contractStreamer(t, "https://spade.twitch.tv/track")

	if res := s.Send(context.Background(), streamer); !res.Delivered {
		t.Fatalf("expected delivery, got %+v", res)
	}
	body, hdr := rt.only(t)
	for _, banned := range []string{"Authorization", "Client-Id", "Client-Session-Id", "X-Device-Id", "Cookie", "Referer", "Origin"} {
		if v := hdr.Get(banned); v != "" {
			t.Errorf("beacon must not carry %s header (got %q)", banned, v)
		}
	}
	if strings.Contains(body, "SECRETSIG") || strings.Contains(body, "SECRETTOKEN") {
		t.Errorf("beacon body must never carry playback sig/token material")
	}
}

// --- Caller regression: an uncredited beacon must not be booked as delivered ---

// TestHTTP200EarnsNoSlotDeliverySuccess drives the REAL MinuteSender against a
// spade endpoint answering 200 and feeds the exact SendResult into the real
// slot-journal recorder. Before the strict-204 rule this produced a
// "delivery_success" record and a delivered minute for a watch Twitch never
// credited; now it must produce a bounded delivery_failure and nothing else.
func TestHTTP200EarnsNoSlotDeliverySuccess(t *testing.T) {
	rt := &contractRT{beaconStatus: http.StatusOK}
	sender := &MinuteSender{client: fakeToken{sig: "s", token: "t"}, httpClient: &http.Client{Transport: rt}}
	streamer := contractStreamer(t, "https://spade.twitch.tv/track")

	clk := newJClock()
	w := journaledWatcher(clk)
	w.journalSlotTransitions([]slotOccupant{occ(streamer, 0, OriginConfigured, "priority")})

	res := sender.Send(context.Background(), streamer)
	w.recordSlotDelivery(streamer, res)

	if got := onlyOfType(slotEvents(w), journal.SlotDeliverySuccess); len(got) != 0 {
		t.Fatalf("an uncredited HTTP 200 beacon must journal no delivery_success, got %+v", got)
	}
	failures := onlyOfType(slotEvents(w), journal.SlotDeliveryFailure)
	if len(failures) != 1 {
		t.Fatalf("expected exactly one delivery_failure, got %+v", failures)
	}
	if failures[0].Stage != string(StageBeacon) || failures[0].Status != 200 || failures[0].ErrorCode != "beacon_http_200" {
		t.Fatalf("expected a bounded beacon_http_200 failure, got %+v", failures[0])
	}
}

// TestHTTP204EarnsSlotDeliverySuccess is the positive control for the same path.
func TestHTTP204EarnsSlotDeliverySuccess(t *testing.T) {
	rt := &contractRT{beaconStatus: http.StatusNoContent}
	sender := &MinuteSender{client: fakeToken{sig: "s", token: "t"}, httpClient: &http.Client{Transport: rt}}
	streamer := contractStreamer(t, "https://spade.twitch.tv/track")

	clk := newJClock()
	w := journaledWatcher(clk)
	w.journalSlotTransitions([]slotOccupant{occ(streamer, 0, OriginConfigured, "priority")})

	res := sender.Send(context.Background(), streamer)
	w.recordSlotDelivery(streamer, res)

	if got := onlyOfType(slotEvents(w), journal.SlotDeliverySuccess); len(got) != 1 {
		t.Fatalf("a credited 204 beacon must journal exactly one delivery_success, got %+v", got)
	}
	if got := onlyOfType(slotEvents(w), journal.SlotDeliveryFailure); len(got) != 0 {
		t.Fatalf("a credited beacon must journal no failure, got %+v", got)
	}
}

// TestHTTP200RoutesToTheFailureArmNotTheDeliveredArm pins the exact shape the
// watch loop switches on. The loop's delivered branch is the switch DEFAULT
// (watcher.go), so a non-delivered outcome that forgot to populate Failure would
// be silently credited: local watched minutes, watch-time fairness credit,
// streak progress and the drops progress-sync trigger all hang off that arm.
// A 200 must therefore be Delivered=false AND Failure!=nil, never merely
// "not delivered".
func TestHTTP200RoutesToTheFailureArmNotTheDeliveredArm(t *testing.T) {
	rt := &contractRT{beaconStatus: http.StatusOK}
	sender := &MinuteSender{client: fakeToken{sig: "s", token: "t"}, httpClient: &http.Client{Transport: rt}}
	streamer := contractStreamer(t, "https://spade.twitch.tv/track")

	res := sender.Send(context.Background(), streamer)

	switch {
	case res.Cancelled:
		t.Fatal("an HTTP 200 beacon is not a teardown")
	case res.Stale:
		t.Fatal("an HTTP 200 beacon is not a stale session")
	case res.Failure != nil: // the failure arm — correct
	default:
		t.Fatal("an HTTP 200 beacon fell through to the DELIVERED arm; it would be credited as a watched minute")
	}
	if streamer.Stream.GetMinuteWatched() != 0 {
		t.Fatalf("no local watched minute may be credited for an uncredited beacon, got %v", streamer.Stream.GetMinuteWatched())
	}
}

// TestProbeRejectsHTTP200 pins that the health canary and the progress
// watchdog — which share this exact postBeacon — also refuse to report a
// transport-accepted-but-uncredited beacon as healthy.
func TestProbeRejectsHTTP200(t *testing.T) {
	rt := &contractRT{beaconStatus: http.StatusOK}
	sender := &MinuteSender{client: fakeToken{sig: "s", token: "t"}, httpClient: &http.Client{Transport: rt}}
	streamer := contractStreamer(t, "https://spade.twitch.tv/track")

	res := sender.Probe(context.Background(), streamer)
	if res.OK {
		t.Fatal("a 200 beacon must not report the watch transport healthy")
	}
	if res.Stage != StageBeacon || res.Status != 200 || res.ErrorCode != "beacon_http_200" {
		t.Fatalf("expected a bounded beacon_http_200 probe failure, got %+v", res)
	}
}

// TestBeaconFailsClosedWithoutAUsableUserID proves the whole chain fails closed
// BEFORE any Spade request when the viewer identity cannot be encoded: no
// payload is published, so the send stops at the session-snapshot gate and zero
// beacons reach the network. A user_id of 0 is never sent.
func TestBeaconFailsClosedWithoutAUsableUserID(t *testing.T) {
	rt := &contractRT{beaconStatus: http.StatusNoContent}
	sender := &MinuteSender{client: fakeToken{sig: "s", token: "t"}, httpClient: &http.Client{Transport: rt}}

	streamer := models.NewStreamer("cyganzor", models.StreamerSettings{})
	streamer.ChannelID = "123456"
	streamer.Stream.Update("987654321", "title", nil, nil, 1)
	streamer.Stream.SetSpadeURL("https://spade.twitch.tv/track")
	if err := streamer.Stream.SetPayload("123456", "987654321", "not-a-number", "cyganzor", nil, nil); err == nil {
		t.Fatal("an unusable user id must be refused at build time")
	}

	res := sender.Send(context.Background(), streamer)
	if res.Delivered {
		t.Fatal("a session without a creditable payload must never report a delivered minute")
	}
	if res.Failure == nil || res.Failure.Stage != StageSessionSnapshot || res.Failure.ErrorCode != "no_payload" {
		t.Fatalf("expected a bounded session-snapshot failure, got %+v", res.Failure)
	}
	if rt.count() != 0 {
		t.Fatalf("no Spade request may be attempted without a creditable payload, got %d", rt.count())
	}
}

// TestConcurrentBeaconsAreRaceFree runs under -race over the real send path.
func TestConcurrentBeaconsAreRaceFree(t *testing.T) {
	rt := &contractRT{beaconStatus: http.StatusNoContent}
	sender := &MinuteSender{client: fakeToken{sig: "s", token: "t"}, httpClient: &http.Client{Transport: rt}}
	streamer := contractStreamer(t, "https://spade.twitch.tv/track")

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if res := sender.Send(context.Background(), streamer); !res.Delivered {
				t.Errorf("concurrent send must deliver, got %+v", res)
			}
		}()
	}
	wg.Wait()
	if rt.count() != 16 {
		t.Fatalf("expected 16 beacons, got %d", rt.count())
	}
}
