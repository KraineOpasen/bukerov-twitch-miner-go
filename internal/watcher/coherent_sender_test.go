package watcher

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// --- Group C: coherent sender (one snapshot per send/probe) ---

// recordingRT counts beacon POSTs and records the URL + body of each, so a test
// can prove exactly one coherent beacon (or none) is sent. Non-POST requests
// (playlist/variant/segment) succeed trivially.
type recordingRT struct {
	mu         sync.Mutex
	beaconURLs []string
	beaconBody []string
}

func (r *recordingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	mk := func(status int, body string) *http.Response {
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}
	}
	switch req.Method {
	case http.MethodPost:
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.beaconURLs = append(r.beaconURLs, req.URL.String())
		r.beaconBody = append(r.beaconBody, string(body))
		r.mu.Unlock()
		return mk(204, ""), nil
	case http.MethodHead:
		return mk(200, ""), nil
	default:
		if strings.Contains(req.URL.Host, "variant") {
			return mk(200, "#EXTM3U\nhttp://seg.test/s.ts\n"), nil
		}
		return mk(200, "#EXTM3U\nhttp://variant.test/low.m3u8\n"), nil
	}
}

func (r *recordingRT) beacons() ([]string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.beaconURLs...), append([]string(nil), r.beaconBody...)
}

// hookToken invokes onCall (e.g. to mutate the session mid-send) before returning
// the token, modelling a concurrent refresh/new-broadcast landing during a send.
type hookToken struct {
	sig, token string
	err        error
	onCall     func()
}

func (h hookToken) GetPlaybackAccessToken(string) (string, string, error) {
	if h.onCall != nil {
		h.onCall()
	}
	return h.sig, h.token, h.err
}

func coherentStreamer(spade string) *models.Streamer {
	s := models.NewStreamer("chan", models.StreamerSettings{})
	s.ChannelID = "cid"
	s.Stream.Update("b1", "t", nil, nil, 1)
	s.Stream.SetSpadeURL(spade)
	s.Stream.SetPayload("cid", "b1", "uid", "chan", nil)
	return s
}

// TestSendUsesOneCoherentSnapshot (C1/C9): a normal Send posts exactly one beacon
// to the captured spade URL and reports the captured generation.
func TestSendUsesOneCoherentSnapshot(t *testing.T) {
	rt := &recordingRT{}
	s := &MinuteSender{client: fakeToken{sig: "s", token: "t"}, httpClient: &http.Client{Transport: rt}}
	streamer := coherentStreamer("https://spade.twitch.tv/track")
	gen := streamer.Stream.SessionGeneration()

	res := s.Send(streamer)
	if !res.Delivered || res.Generation != gen {
		t.Fatalf("expected a delivered send recording the captured generation, got %+v (gen=%d)", res, gen)
	}
	urls, _ := rt.beacons()
	if len(urls) != 1 || urls[0] != "https://spade.twitch.tv/track" {
		t.Fatalf("expected exactly one beacon to the captured spade URL, got %v", urls)
	}
}

// TestSendStaleSessionSkipsBeacon (C3/C4): when the session changes between
// snapshot capture and the beacon, zero beacons are sent and the result is Stale
// (not a failure, not delivered) — an old payload can never be posted to a new
// spade URL.
func TestSendStaleSessionSkipsBeacon(t *testing.T) {
	rt := &recordingRT{}
	streamer := coherentStreamer("https://spade.twitch.tv/old")
	tok := hookToken{sig: "s", token: "t", onCall: func() {
		// A refresh lands mid-send: new spade URL (and thus new generation).
		streamer.Stream.SetSpadeURL("https://spade.twitch.tv/new")
	}}
	s := &MinuteSender{client: tok, httpClient: &http.Client{Transport: rt}}

	res := s.Send(streamer)
	if !res.Stale || res.Delivered || res.Failure != nil {
		t.Fatalf("a session change mid-send must be Stale (not delivered, not a failure), got %+v", res)
	}
	if urls, _ := rt.beacons(); len(urls) != 0 {
		t.Fatalf("a stale send must post zero beacons, got %v", urls)
	}
}

// TestSendNewBroadcastMidSendSkipsBeacon (C3): a new broadcast landing mid-send
// is likewise detected and the beacon suppressed.
func TestSendNewBroadcastMidSendSkipsBeacon(t *testing.T) {
	rt := &recordingRT{}
	streamer := coherentStreamer("https://spade.twitch.tv/track")
	tok := hookToken{sig: "s", token: "t", onCall: func() {
		streamer.Stream.Update("b2", "t", nil, nil, 2) // new broadcast
	}}
	s := &MinuteSender{client: tok, httpClient: &http.Client{Transport: rt}}

	res := s.Send(streamer)
	if !res.Stale {
		t.Fatalf("a new broadcast mid-send must yield a Stale result, got %+v", res)
	}
	if urls, _ := rt.beacons(); len(urls) != 0 {
		t.Fatalf("a new broadcast mid-send must post zero beacons, got %v", urls)
	}
}

// TestProbeUsesOneCoherentSnapshot (C2): the probe posts exactly one beacon to
// the captured spade URL.
func TestProbeUsesOneCoherentSnapshot(t *testing.T) {
	rt := &recordingRT{}
	s := &MinuteSender{client: fakeToken{sig: "s", token: "t"}, httpClient: &http.Client{Transport: rt}}
	streamer := coherentStreamer("https://spade.twitch.tv/track")

	res := s.Probe(context.Background(), streamer)
	if !res.OK {
		t.Fatalf("expected an OK probe, got %+v", res)
	}
	if urls, _ := rt.beacons(); len(urls) != 1 || urls[0] != "https://spade.twitch.tv/track" {
		t.Fatalf("expected one beacon to the captured spade URL, got %v", urls)
	}
}

// TestProbeStaleSessionReported (C2): a mid-probe session change is reported as
// the stale_session stage, not a transport failure and not OK.
func TestProbeStaleSessionReported(t *testing.T) {
	rt := &recordingRT{}
	streamer := coherentStreamer("https://spade.twitch.tv/old")
	tok := hookToken{sig: "s", token: "t", onCall: func() {
		streamer.Stream.SetSpadeURL("https://spade.twitch.tv/new")
	}}
	s := &MinuteSender{client: tok, httpClient: &http.Client{Transport: rt}}

	res := s.Probe(context.Background(), streamer)
	if res.OK || res.Stage != StageStaleSession {
		t.Fatalf("a mid-probe session change must report stale_session, got %+v", res)
	}
	if urls, _ := rt.beacons(); len(urls) != 0 {
		t.Fatalf("a stale probe must post zero beacons, got %v", urls)
	}
}

// TestSendResultCarriesNoSecrets (C9): the typed result exposes no token/URL.
func TestSendResultCarriesNoSecrets(t *testing.T) {
	rt := &recordingRT{}
	streamer := coherentStreamer("https://spade.twitch.tv/track")
	s := &MinuteSender{client: fakeToken{sig: "SECRETSIG", token: "SECRETTOKEN"}, httpClient: &http.Client{Transport: rt}}
	res := s.Send(streamer)
	blob := ""
	if res.Failure != nil {
		blob = res.Failure.ErrorCode + " " + string(res.Failure.Stage)
	}
	for _, secret := range []string{"SECRETSIG", "SECRETTOKEN", "https://", "spade.twitch"} {
		if strings.Contains(blob, secret) {
			t.Fatalf("send result leaked %q: %q", secret, blob)
		}
	}
}
