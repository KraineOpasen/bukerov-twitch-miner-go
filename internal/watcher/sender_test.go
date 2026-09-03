package watcher

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// TestSpadeFormBodyPercentEncodesPayload guards against the form-corruption bug:
// the base64 minute-watched payload must be percent-encoded so a '+' survives
// transit instead of being decoded as a space by the spade endpoint's form
// parser. It checks a payload that deliberately contains every base64 special
// character ('+', '/', '=') round-trips exactly through a standard form parse.
func TestSpadeFormBodyPercentEncodesPayload(t *testing.T) {
	// A base64 blob exercising the characters a raw "data="+payload body would
	// mangle: '+' (space), plus '/' and '=' for good measure.
	payload := "aGVsbG8+d29ybGQ/Zm9v==" // arbitrary, contains + / =

	body := spadeFormBody(payload)

	if strings.Contains(body, "+") {
		t.Fatalf("form body must not contain a raw '+', got %q", body)
	}

	parsed, err := url.ParseQuery(body)
	if err != nil {
		t.Fatalf("form body did not parse: %v", err)
	}
	if got := parsed.Get("data"); got != payload {
		t.Fatalf("payload did not survive a form round-trip: sent %q, server sees %q", payload, got)
	}
}

// TestSpadeFormBodyRoundTripsRealPayload confirms a realistic base64-encoded
// minute-watched JSON blob decodes back to identical bytes after the form
// encode/parse cycle a real spade request goes through.
func TestSpadeFormBodyRoundTripsRealPayload(t *testing.T) {
	original := []byte(`[{"event":"minute-watched","properties":{"channel_id":"123","broadcast_id":"456","player":"site","user_id":"789","live":true,"channel":"somestreamer"}}]`)
	payload := base64.StdEncoding.EncodeToString(original)

	body := spadeFormBody(payload)

	parsed, err := url.ParseQuery(body)
	if err != nil {
		t.Fatalf("form body did not parse: %v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(parsed.Get("data"))
	if err != nil {
		t.Fatalf("server-side base64 decode failed: %v", err)
	}
	if string(decoded) != string(original) {
		t.Fatalf("payload corrupted in transit:\n sent: %s\n  got: %s", original, decoded)
	}
}

// --- R11: postBeacon must reject redirects on the beacon POST only ---------
//
// beaconRedirectRT is a fake RoundTripper purpose-built for these tests.
// Unlike probe_test.go's fakeRT (one canned response per role, no redirects),
// each role can answer with a 3xx + Location, and an exact request URL can be
// pinned to its own canned response via extra — which is how a test controls
// what a redirect's second hop sees, and proves whether a "should never be
// contacted" target was actually contacted.
type beaconRedirectStep struct {
	status   int
	location string
	body     string
}

type beaconRedirectRT struct {
	mu sync.Mutex

	playlist, variant, segment, beacon beaconRedirectStep
	// extra maps an exact request URL to a canned response, checked before the
	// role-based defaults below. Each exact URL used in these tests is only
	// ever requested with one HTTP method, so a method-agnostic map is enough.
	extra map[string]beaconRedirectStep

	hits map[string]int
	body map[string][]byte
}

func newBeaconRedirectRT() *beaconRedirectRT {
	return &beaconRedirectRT{
		extra: map[string]beaconRedirectStep{},
		hits:  map[string]int{},
		body:  map[string][]byte{},
	}
}

// okChain configures a normal, non-redirecting playlist -> variant -> segment
// chain so Send/Probe can reach the beacon step deterministically.
func (rt *beaconRedirectRT) okChain() {
	rt.playlist = beaconRedirectStep{status: 200, body: "#EXTM3U\nhttps://variant.test/low.m3u8\n"}
	rt.variant = beaconRedirectStep{status: 200, body: "#EXTM3U\nhttps://seg.test/s.ts\n"}
	rt.segment = beaconRedirectStep{status: 200}
}

func (rt *beaconRedirectRT) hitCount(url string) int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.hits[url]
}

func (rt *beaconRedirectRT) bodyFor(url string) []byte {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.body[url]
}

// resetHits clears only the observed-request bookkeeping, not the configured
// routes, so a test can reuse the same MinuteSender/transport for a second
// Send and still assert on that second Send's requests alone.
func (rt *beaconRedirectRT) resetHits() {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.hits = map[string]int{}
	rt.body = map[string][]byte{}
}

func (rt *beaconRedirectRT) RoundTrip(req *http.Request) (*http.Response, error) {
	u := req.URL.String()

	var reqBody []byte
	if req.Body != nil {
		var err error
		reqBody, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, fmt.Errorf("read request body: %w", err)
		}
	}

	rt.mu.Lock()
	rt.hits[u]++
	if len(reqBody) > 0 {
		rt.body[u] = reqBody
	}
	resp, ok := rt.extra[u]
	if !ok {
		switch {
		case req.Method == http.MethodPost:
			resp = rt.beacon
		case req.Method == http.MethodHead:
			resp = rt.segment
		case strings.Contains(req.URL.Host, "variant"):
			resp = rt.variant
		default:
			resp = rt.playlist
		}
	}
	rt.mu.Unlock()

	header := make(http.Header)
	if resp.location != "" {
		header.Set("Location", resp.location)
	}
	return &http.Response{
		StatusCode: resp.status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(resp.body)),
		Request:    req,
	}, nil
}

// TestBeaconRedirectRTRoundTripReturnsBodyReadError proves the test harness
// itself is deterministic about a request-body read failure: RoundTrip must
// surface the error instead of silently treating an unreadable body as an
// empty one, and must not record the failed request in rt.hits or rt.body.
func TestBeaconRedirectRTRoundTripReturnsBodyReadError(t *testing.T) {
	sentinelErr := errors.New("sentinel body read error")

	rt := newBeaconRedirectRT()
	rt.okChain()

	req, err := http.NewRequest(http.MethodPost, "http://spade.test/beacon", iotest.ErrReader(sentinelErr))
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}

	resp, roundTripErr := rt.RoundTrip(req)

	if roundTripErr == nil {
		t.Fatal("expected RoundTrip to return a non-nil error on a body read failure")
	}
	if !errors.Is(roundTripErr, sentinelErr) {
		t.Fatalf("expected the returned error to wrap the sentinel error, got %v", roundTripErr)
	}
	if resp != nil {
		t.Fatalf("expected a nil response on a body read failure, got %+v", resp)
	}
	u := req.URL.String()
	if got := rt.hitCount(u); got != 0 {
		t.Fatalf("a failed body read must not be recorded in rt.hits, got %d hit(s)", got)
	}
	if body := rt.bodyFor(u); body != nil {
		t.Fatalf("a failed body read must not record a body, got %q", body)
	}
}

// beaconRedirectSender builds a MinuteSender wired to rt and a brought-online
// streamer using spadeURL, ready to Send/Probe through the real production
// path (MinuteSender.Send / MinuteSender.Probe, never a reimplementation of
// postBeacon).
func beaconRedirectSender(rt *beaconRedirectRT, spadeURL string) (*MinuteSender, *models.Streamer) {
	s := models.NewStreamer("redirect_channel", models.StreamerSettings{})
	s.ChannelID = "cid"
	s.Stream.SetSpadeURL(spadeURL)
	s.Stream.SetPayload("cid", "bid", "uid", "redirect_channel", nil)

	sender := &MinuteSender{
		client:     fakeToken{sig: "sig", token: "tok"},
		httpClient: &http.Client{Transport: rt},
	}
	return sender, s
}

// TestSendRejectsBeaconRedirects is the redirect matrix: every redirect status
// a beacon POST could receive must be rejected, never followed, and reported
// as the existing unexpected-status beacon failure.
func TestSendRejectsBeaconRedirects(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(fmt.Sprintf("%d", status), func(t *testing.T) {
			rt := newBeaconRedirectRT()
			rt.okChain()

			const leakURL = "http://redirect-target.test/leak"
			rt.beacon = beaconRedirectStep{status: status, location: leakURL}
			// Registered so a broken implementation that DOES follow the
			// redirect produces an unambiguous Delivered=true instead of an
			// opaque transport error - see mutation probes M1/M3/M4.
			rt.extra[leakURL] = beaconRedirectStep{status: 200}

			sender, streamer := beaconRedirectSender(rt, "http://spade.test/beacon")
			res := sender.Send(context.Background(), streamer)

			if res.Delivered {
				t.Fatalf("redirect %d: beacon must not be Delivered", status)
			}
			if res.Failure == nil {
				t.Fatalf("redirect %d: expected a Failure, got none", status)
			}
			if res.Failure.Stage != StageBeacon {
				t.Fatalf("redirect %d: expected StageBeacon, got %q", status, res.Failure.Stage)
			}
			if res.Failure.Status != status {
				t.Fatalf("redirect %d: expected Failure.Status == %d, got %d", status, status, res.Failure.Status)
			}
			wantCode := fmt.Sprintf("beacon_http_%d", status)
			if res.Failure.ErrorCode != wantCode {
				t.Fatalf("redirect %d: expected ErrorCode %q, got %q", status, wantCode, res.Failure.ErrorCode)
			}
			if got := rt.hitCount(leakURL); got != 0 {
				t.Fatalf("redirect %d: redirect target was contacted %d time(s)", status, got)
			}
			if status == 307 || status == 308 {
				if body := rt.bodyFor(leakURL); len(body) != 0 {
					t.Fatalf("redirect %d: beacon payload was replayed to the redirect target: %q", status, body)
				}
			}
		})
	}
}

// TestSendRejectsBeaconHTTPSToHTTPDowngrade proves an https beacon URL that
// redirects to a plain http target is rejected exactly like any other
// redirect, and the downgrade target is never contacted.
func TestSendRejectsBeaconHTTPSToHTTPDowngrade(t *testing.T) {
	rt := newBeaconRedirectRT()
	rt.okChain()

	const downgradeURL = "http://downgrade-target.test/leak"
	rt.beacon = beaconRedirectStep{status: 302, location: downgradeURL}
	rt.extra[downgradeURL] = beaconRedirectStep{status: 200}

	sender, streamer := beaconRedirectSender(rt, "https://spade.test/beacon")
	res := sender.Send(context.Background(), streamer)

	if res.Delivered {
		t.Fatal("beacon must not be Delivered when the redirect downgrades https to http")
	}
	if res.Failure == nil || res.Failure.Stage != StageBeacon || res.Failure.Status != 302 {
		t.Fatalf("expected a StageBeacon failure with status 302, got %+v", res.Failure)
	}
	if got := rt.hitCount(downgradeURL); got != 0 {
		t.Fatalf("http downgrade target was contacted %d time(s)", got)
	}
}

// --- Normal controls: unchanged direct-response behavior -------------------

func TestSendBeaconDirect200IsDelivered(t *testing.T) {
	rt := newBeaconRedirectRT()
	rt.okChain()
	rt.beacon = beaconRedirectStep{status: 200}

	sender, streamer := beaconRedirectSender(rt, "http://spade.test/beacon")
	res := sender.Send(context.Background(), streamer)

	if !res.Delivered || res.Failure != nil {
		t.Fatalf("expected a direct 200 beacon to be Delivered, got %+v", res)
	}
}

func TestSendBeaconDirect204IsDelivered(t *testing.T) {
	rt := newBeaconRedirectRT()
	rt.okChain()
	rt.beacon = beaconRedirectStep{status: 204}

	sender, streamer := beaconRedirectSender(rt, "http://spade.test/beacon")
	res := sender.Send(context.Background(), streamer)

	if !res.Delivered || res.Failure != nil {
		t.Fatalf("expected a direct 204 beacon to be Delivered, got %+v", res)
	}
}

func TestSendBeaconDirectNon2xxIsFailure(t *testing.T) {
	rt := newBeaconRedirectRT()
	rt.okChain()
	rt.beacon = beaconRedirectStep{status: 500}

	sender, streamer := beaconRedirectSender(rt, "http://spade.test/beacon")
	res := sender.Send(context.Background(), streamer)

	if res.Delivered {
		t.Fatal("a direct non-2xx beacon must not be Delivered")
	}
	if res.Failure == nil || res.Failure.Stage != StageBeacon || res.Failure.Status != 500 {
		t.Fatalf("expected a StageBeacon failure with status 500, got %+v", res.Failure)
	}
}

// TestSendBeaconUsesInjectedTransportAfterClientCopy proves the beacon request
// still goes through s.httpClient's configured Transport after postBeacon
// copies the client to override CheckRedirect. It uses a scheme only this
// test's Transport understands: a fresh, unrelated http.Client (default
// Transport) would fail outright on that scheme instead of silently
// succeeding, so a regression to "construct a fresh client" fails this test
// by assertion (see mutation probe M5), not by a compile error.
func TestSendBeaconUsesInjectedTransportAfterClientCopy(t *testing.T) {
	rt := newBeaconRedirectRT()
	rt.okChain()
	rt.beacon = beaconRedirectStep{status: 204}

	const beaconURL = "beacontest://spade.local/beacon"

	sender, streamer := beaconRedirectSender(rt, beaconURL)
	res := sender.Send(context.Background(), streamer)

	if !res.Delivered || res.Failure != nil {
		t.Fatalf("expected the beacon POST to reach the injected Transport and be Delivered, got %+v", res)
	}
	if got := rt.hitCount(beaconURL); got != 1 {
		t.Fatalf("expected exactly one request through the injected Transport, got %d", got)
	}
}

// TestSendTwoPhaseBeaconRejectionThenNormalRedirectsFollowed is the mandatory
// two-phase isolation test. Phase A proves a beacon redirect is rejected
// without touching the shared client. Phase B reuses the SAME MinuteSender for
// a second Send where playlist/variant/segment now redirect and are followed
// normally (proving the fix is scoped to the beacon only) and the beacon
// succeeds directly. A single Send is not enough: simulateWatching runs before
// postBeacon, so only a second Send on the same sender can show the shared
// client's redirect-following behavior survived the first Send's beacon-only
// override.
func TestSendTwoPhaseBeaconRejectionThenNormalRedirectsFollowed(t *testing.T) {
	rt := newBeaconRedirectRT()
	rt.okChain()

	const leakURL = "http://redirect-target.test/leak"
	rt.beacon = beaconRedirectStep{status: 302, location: leakURL}
	rt.extra[leakURL] = beaconRedirectStep{status: 200}

	sender, streamer := beaconRedirectSender(rt, "http://spade.test/beacon")
	origClient := sender.httpClient

	// --- Phase A: playlist/variant/segment succeed, beacon redirects -------
	resA := sender.Send(context.Background(), streamer)
	if resA.Delivered {
		t.Fatal("phase A: beacon redirect must not be Delivered")
	}
	if resA.Failure == nil || resA.Failure.Stage != StageBeacon || resA.Failure.Status != 302 {
		t.Fatalf("phase A: expected a StageBeacon 302 failure, got %+v", resA.Failure)
	}
	if got := rt.hitCount(leakURL); got != 0 {
		t.Fatalf("phase A: redirect target was contacted %d time(s)", got)
	}
	if sender.httpClient != origClient {
		t.Fatal("phase A: the shared httpClient must not be replaced")
	}
	if sender.httpClient.CheckRedirect != nil {
		t.Fatal("phase A: the shared httpClient's CheckRedirect must remain unmodified")
	}

	// --- Phase B: reset only the observed-request bookkeeping, then make ---
	// playlist/variant/segment redirect to a second hop that succeeds; the
	// beacon now answers directly with 204.
	rt.resetHits()

	const playlistTarget = "https://playlist-target.test/hop2.m3u8"
	const variantTarget = "https://variant-target.test/hop2.m3u8"
	const segmentTarget = "https://seg-target.test/hop2"

	rt.playlist = beaconRedirectStep{status: 302, location: playlistTarget}
	rt.extra[playlistTarget] = beaconRedirectStep{status: 200, body: "#EXTM3U\nhttps://variant.test/redirect-hop\n"}

	rt.variant = beaconRedirectStep{status: 302, location: variantTarget}
	rt.extra[variantTarget] = beaconRedirectStep{status: 200, body: "#EXTM3U\nhttps://seg.test/redirect-hop\n"}

	rt.segment = beaconRedirectStep{status: 302, location: segmentTarget}
	rt.extra[segmentTarget] = beaconRedirectStep{status: 200}

	rt.beacon = beaconRedirectStep{status: 204}

	resB := sender.Send(context.Background(), streamer)

	if !resB.Delivered {
		t.Fatalf("phase B: expected the second Send to be Delivered, got %+v", resB)
	}
	if resB.SimulateErr != nil {
		t.Fatalf("phase B: expected SimulateErr to be nil, got %v", resB.SimulateErr)
	}
	if got := rt.hitCount(playlistTarget); got != 1 {
		t.Fatalf("phase B: playlist redirect target was not followed, hits=%d", got)
	}
	if got := rt.hitCount(variantTarget); got != 1 {
		t.Fatalf("phase B: variant redirect target was not followed, hits=%d", got)
	}
	if got := rt.hitCount(segmentTarget); got != 1 {
		t.Fatalf("phase B: segment HEAD redirect target was not followed, hits=%d", got)
	}
	if sender.httpClient != origClient {
		t.Fatal("phase B: the shared httpClient must still be the original instance")
	}
	if sender.httpClient.CheckRedirect != nil {
		t.Fatal("phase B: the shared httpClient's CheckRedirect must remain unmodified")
	}
}

// TestProbeBeaconRedirectRejected covers the Probe surface: a beacon redirect
// must fail the probe at StageBeacon with the redirect's status, and the
// redirect target must never be contacted.
func TestProbeBeaconRedirectRejected(t *testing.T) {
	rt := newBeaconRedirectRT()
	rt.okChain()

	const leakURL = "http://redirect-target.test/leak"
	rt.beacon = beaconRedirectStep{status: 303, location: leakURL}
	rt.extra[leakURL] = beaconRedirectStep{status: 200}

	sender, streamer := beaconRedirectSender(rt, "http://spade.test/beacon")
	res := sender.Probe(context.Background(), streamer)

	if res.OK {
		t.Fatal("expected the probe to fail on a beacon redirect")
	}
	if res.Stage != StageBeacon || res.Status != 303 {
		t.Fatalf("expected StageBeacon status 303, got stage=%q status=%d", res.Stage, res.Status)
	}
	if got := rt.hitCount(leakURL); got != 0 {
		t.Fatalf("redirect target was contacted %d time(s)", got)
	}
}
