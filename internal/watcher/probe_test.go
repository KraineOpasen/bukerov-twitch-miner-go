package watcher

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// fakeToken is a stand-in playbackTokenProvider.
type fakeToken struct {
	sig, token string
	err        error
}

func (f fakeToken) GetPlaybackAccessToken(string) (string, string, error) {
	return f.sig, f.token, f.err
}

// rtBehavior configures the fake round-tripper per watch-transport stage.
type rtBehavior struct {
	playlistStatus, variantStatus, segmentStatus, beaconStatus int
	playlistBody, variantBody                                  string
	playlistErr, variantErr, segmentErr, beaconErr             error
}

func okBehavior() rtBehavior {
	return rtBehavior{
		playlistStatus: 200, playlistBody: "#EXTM3U\nhttp://variant.test/low.m3u8\n",
		variantStatus: 200, variantBody: "#EXTM3U\nhttp://seg.test/s.ts\n",
		segmentStatus: 200,
		beaconStatus:  204,
	}
}

type fakeRT struct{ b rtBehavior }

func (f fakeRT) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err // honor cancellation like a real transport
	}
	mk := func(status int, body string) *http.Response {
		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
			Request:    req,
		}
	}
	switch {
	case req.Method == http.MethodPost: // spade beacon
		if f.b.beaconErr != nil {
			return nil, f.b.beaconErr
		}
		return mk(f.b.beaconStatus, ""), nil
	case req.Method == http.MethodHead: // segment
		if f.b.segmentErr != nil {
			return nil, f.b.segmentErr
		}
		return mk(f.b.segmentStatus, ""), nil
	case strings.Contains(req.URL.Host, "variant"): // lowest-quality variant
		if f.b.variantErr != nil {
			return nil, f.b.variantErr
		}
		return mk(f.b.variantStatus, f.b.variantBody), nil
	default: // usher playlist
		if f.b.playlistErr != nil {
			return nil, f.b.playlistErr
		}
		return mk(f.b.playlistStatus, f.b.playlistBody), nil
	}
}

func testSender(b rtBehavior, tok fakeToken) *MinuteSender {
	return &MinuteSender{
		client:     tok,
		httpClient: &http.Client{Transport: fakeRT{b}},
	}
}

func canaryStreamer() *models.Streamer {
	s := models.NewStreamer("probe_channel", models.StreamerSettings{})
	s.ChannelID = "cid"
	s.Stream.SetSpadeURL("http://spade.test/track")
	s.Stream.SetPayload("cid", "bid", "uid", "probe_channel", nil)
	return s
}

func TestProbeSuccess(t *testing.T) {
	s := testSender(okBehavior(), fakeToken{sig: "sig", token: "tok"})
	res := s.Probe(context.Background(), canaryStreamer())
	if !res.OK {
		t.Fatalf("expected OK probe, got stage=%q code=%q", res.Stage, res.ErrorCode)
	}
}

func TestProbePlaybackTokenError(t *testing.T) {
	s := testSender(okBehavior(), fakeToken{err: context.DeadlineExceeded})
	res := s.Probe(context.Background(), canaryStreamer())
	if res.OK || res.Stage != StagePlaybackToken {
		t.Fatalf("expected playback_token failure, got OK=%v stage=%q", res.OK, res.Stage)
	}
}

func TestProbePlaylistError(t *testing.T) {
	b := okBehavior()
	b.playlistStatus = 403
	res := testSender(b, fakeToken{}).Probe(context.Background(), canaryStreamer())
	if res.OK || res.Stage != StagePlaylist || res.Status != 403 {
		t.Fatalf("expected playlist HTTP 403 failure, got OK=%v stage=%q status=%d", res.OK, res.Stage, res.Status)
	}
}

func TestProbeSegmentError(t *testing.T) {
	b := okBehavior()
	b.segmentStatus = 500
	res := testSender(b, fakeToken{}).Probe(context.Background(), canaryStreamer())
	if res.OK || res.Stage != StageSegment || res.Status != 500 {
		t.Fatalf("expected segment HTTP 500 failure, got OK=%v stage=%q status=%d", res.OK, res.Stage, res.Status)
	}
}

func TestProbeBeaconNon2xx(t *testing.T) {
	b := okBehavior()
	b.beaconStatus = 400
	res := testSender(b, fakeToken{}).Probe(context.Background(), canaryStreamer())
	if res.OK || res.Stage != StageBeacon || res.Status != 400 {
		t.Fatalf("expected beacon HTTP 400 failure, got OK=%v stage=%q status=%d", res.OK, res.Stage, res.Status)
	}
}

func TestProbeContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := testSender(okBehavior(), fakeToken{}).Probe(ctx, canaryStreamer())
	if res.OK {
		t.Fatal("expected a cancelled probe to fail")
	}
}

// TestProbeResultCarriesNoSensitiveData is the redaction guard: the probe result
// exposes only stage/status/code, never a token or a signed URL.
func TestProbeResultCarriesNoSensitiveData(t *testing.T) {
	b := okBehavior()
	b.playlistStatus = 403
	res := testSender(b, fakeToken{sig: "SECRETSIG", token: "SECRETTOKEN"}).
		Probe(context.Background(), canaryStreamer())
	blob := res.ErrorCode + " " + string(res.Stage)
	for _, secret := range []string{"SECRETSIG", "SECRETTOKEN", "http://", "https://", "sig=", "token="} {
		if strings.Contains(blob, secret) {
			t.Fatalf("probe result leaked %q: %q", secret, blob)
		}
	}
}

// --- Send behavior preservation (the step extraction must not change it) ---

// TestSendSimulateFailureIsNonFatal locks the key Send invariant: a playlist
// simulation failure is returned as simulateErr but does NOT fail the send — the
// beacon still posts and err is nil.
func TestSendSimulateFailureIsNonFatal(t *testing.T) {
	b := okBehavior()
	b.playlistStatus = 403 // simulate fails
	// beacon still 204 (OK)
	simErr, err := testSender(b, fakeToken{sig: "s", token: "t"}).Send(canaryStreamer())
	if simErr == nil {
		t.Error("expected a non-nil simulateErr when the playlist fails")
	}
	if err != nil {
		t.Errorf("a playlist failure must not fail the send (simulate is best-effort), got err=%v", err)
	}
}

func TestSendTokenErrorIsFatalAndSkipsSimulate(t *testing.T) {
	simErr, err := testSender(okBehavior(), fakeToken{err: context.DeadlineExceeded}).Send(canaryStreamer())
	if err == nil {
		t.Error("expected a fatal error when the playback token fails")
	}
	if simErr != nil {
		t.Errorf("simulate must not run when the token step fails, got simulateErr=%v", simErr)
	}
}

func TestSendBeaconNon2xxIsFatal(t *testing.T) {
	b := okBehavior()
	b.beaconStatus = 400
	_, err := testSender(b, fakeToken{sig: "s", token: "t"}).Send(canaryStreamer())
	if err == nil {
		t.Error("expected a non-2xx spade response to fail the send")
	}
}

func TestSendMissingSpadeURLIsFatal(t *testing.T) {
	s := canaryStreamer()
	s.Stream.SetSpadeURL("")
	_, err := testSender(okBehavior(), fakeToken{sig: "s", token: "t"}).Send(s)
	if err == nil {
		t.Error("expected a missing spade URL to fail the send")
	}
}
