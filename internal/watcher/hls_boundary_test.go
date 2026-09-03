package watcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// C1 — watch URL / redirect trust boundary.
//
// Everything here runs against the REAL production path (MinuteSender.Send and
// MinuteSender.Probe) through an injected http.RoundTripper. No Twitch traffic,
// no OAuth, no user tokens, no config, no real network of any kind: the fake
// transport is the only thing a request can reach, which is also what makes
// "this forbidden target was never contacted" a provable statement rather than
// an assertion about intent.
//
// The sentinel sig/token below stand in for the signed usher material a real
// playback token carries; several tests assert they never escape into an error,
// a result or a header.

const (
	c1Channel = "c1chan"
	c1Sig     = "SENTINELSIG"
	c1Token   = "SENTINELTOKEN"
	// c1Spade is a BEACON url. The beacon has its own, deliberately different
	// contract (redirects forbidden, see TestSendRejectsBeaconRedirects) and is
	// NOT governed by the HLS trust boundary these tests pin.
	c1Spade = "https://spade.test/beacon"
)

// c1MasterURL mirrors, exactly, the master playlist URL simulateWatching builds
// for a channel. Duplicating the construction here is deliberate: it pins the
// shape of the one request whose target is not remote-derived, so a change to
// it shows up as a test failure rather than as a silently different fixture.
func c1MasterURL(channel string) string {
	return fmt.Sprintf("%s/api/channel/hls/%s.m3u8?%s", constants.UsherURL, channel,
		url.Values{"sig": {c1Sig}, "token": {c1Token}}.Encode())
}

// hlsStep is one canned response from hlsRecordRT.
type hlsStep struct {
	status   int
	location string
	body     string
	err      error
}

// hlsRecordRT is a recording RoundTripper: it answers exact request URLs from
// steps (falling back to fallback), and records EVERY attempted request — URL,
// method and headers, in order. The recording is what lets a test prove a
// forbidden target received zero transport hits: a request that never reaches
// the transport is a request that was rejected before any I/O.
type hlsRecordRT struct {
	mu       sync.Mutex
	steps    map[string]hlsStep
	fallback hlsStep
	hits     []string
	methods  []string
	headers  []http.Header
}

func newHLSRecordRT() *hlsRecordRT {
	return &hlsRecordRT{
		steps: map[string]hlsStep{},
		// An unregistered target answers 404 rather than a useful body, so a
		// test that reaches an unexpected URL fails loudly instead of drifting
		// into a passing path.
		fallback: hlsStep{status: http.StatusNotFound},
	}
}

func (rt *hlsRecordRT) RoundTrip(req *http.Request) (*http.Response, error) {
	u := req.URL.String()

	// The attempt is recorded BEFORE the cancellation check, so "this target was
	// never contacted" means the request never reached the transport at all —
	// not merely that a dead context stopped it once it got here.
	rt.mu.Lock()
	rt.hits = append(rt.hits, u)
	rt.methods = append(rt.methods, req.Method)
	rt.headers = append(rt.headers, req.Header.Clone())
	step, ok := rt.steps[u]
	if !ok {
		if req.Method == http.MethodPost { // the spade beacon
			step = hlsStep{status: http.StatusNoContent}
		} else {
			step = rt.fallback
		}
	}
	rt.mu.Unlock()

	if err := req.Context().Err(); err != nil {
		return nil, err // behave like a real transport under cancellation
	}
	if step.err != nil {
		return nil, step.err
	}
	header := make(http.Header)
	if step.location != "" {
		header.Set("Location", step.location)
	}
	return &http.Response{
		StatusCode: step.status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(step.body)),
		Request:    req,
	}, nil
}

func (rt *hlsRecordRT) hitCount(target string) int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	n := 0
	for _, h := range rt.hits {
		if h == target {
			n++
		}
	}
	return n
}

func (rt *hlsRecordRT) allHits() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([]string(nil), rt.hits...)
}

// c1HLSHits returns every attempted request URL except the beacon POST, i.e.
// exactly the requests the C1 boundary governs.
func c1HLSHits(rt *hlsRecordRT) []string {
	out := []string{}
	for _, h := range rt.allHits() {
		if h != c1Spade {
			out = append(out, h)
		}
	}
	return out
}

// headerFor returns the header of the FIRST attempt at target.
func (rt *hlsRecordRT) headerFor(target string) http.Header {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for i, h := range rt.hits {
		if h == target {
			return rt.headers[i]
		}
	}
	return nil
}

// methodFor returns the HTTP method of the FIRST attempt at target.
func (rt *hlsRecordRT) methodFor(target string) string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for i, h := range rt.hits {
		if h == target {
			return rt.methods[i]
		}
	}
	return ""
}

// c1Sender wires a MinuteSender to rt with a brought-online streamer, ready to
// drive the real Send/Probe path.
func c1Sender(rt *hlsRecordRT, channel string) (*MinuteSender, *models.Streamer) {
	s := models.NewStreamer(channel, models.StreamerSettings{})
	s.ChannelID = "cid"
	s.Stream.SetSpadeURL(c1Spade)
	mustSetPayload(s.Stream, "cid", "bid", "44322889", channel, nil, nil)

	sender := &MinuteSender{
		client:     fakeToken{sig: c1Sig, token: c1Token},
		httpClient: &http.Client{Transport: rt},
	}
	return sender, s
}

// c1Playlist is a minimal well-formed playlist whose single URI line is uri.
func c1Playlist(uri string) string {
	return "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\n" + uri + "\n"
}

// c1OKChain registers a plain, non-redirecting master -> media -> segment chain
// that reaches the beacon, so a test only has to perturb the one hop it is
// about.
func c1OKChain(rt *hlsRecordRT) {
	rt.steps[c1MasterURL(c1Channel)] = hlsStep{status: 200, body: c1Playlist("https://variant.test/low.m3u8")}
	rt.steps["https://variant.test/low.m3u8"] = hlsStep{status: 200, body: c1Playlist("https://seg.test/s.ts")}
	rt.steps["https://seg.test/s.ts"] = hlsStep{status: 200}
}

// ---------------------------------------------------------------------------
// A. Pre-I/O rejection: a forbidden target is never contacted.
// ---------------------------------------------------------------------------

// c1ForbiddenTargets is the representative reject matrix. Each entry is a URL
// that a remote playlist body (or a remote Location header) could name and that
// the C1 policy must refuse BEFORE any I/O.
var c1ForbiddenTargets = []struct {
	name string
	url  string
}{
	{"http_downgrade", "http://variant.test/low.m3u8"},
	{"explicit_non_443_port", "https://variant.test:8443/low.m3u8"},
	{"explicit_port_80", "https://variant.test:80/low.m3u8"},
	{"userinfo", "https://user:pw@variant.test/low.m3u8"},
	{"userinfo_no_password", "https://user@variant.test/low.m3u8"},
	{"fragment", "https://variant.test/low.m3u8#frag"},
	{"empty_fragment", "https://variant.test/low.m3u8#"},
	{"ipv4_literal", "https://127.0.0.1/low.m3u8"},
	{"ipv4_literal_public", "https://93.184.216.34/low.m3u8"},
	{"link_local_metadata", "https://169.254.169.254/latest/meta-data"},
	{"ipv6_literal", "https://[::1]/low.m3u8"},
	{"ipv6_zone", "https://[fe80::1%25eth0]/low.m3u8"},
	{"localhost", "https://localhost/low.m3u8"},
	{"localhost_uppercase", "https://LOCALHOST/low.m3u8"},
	{"localhost_trailing_dot", "https://localhost./low.m3u8"},
	{"localhost_subdomain", "https://api.localhost/low.m3u8"},
	{"mdns_local", "https://printer.local/low.m3u8"},
	{"numeric_decimal_ip", "https://2130706433/low.m3u8"},
	{"numeric_octal_ip", "https://0177.0.0.1/low.m3u8"},
	{"numeric_hex_ip", "https://0x7f000001/low.m3u8"},
	{"numeric_short_form", "https://127.1/low.m3u8"},
	{"numeric_last_label", "https://variant.test.1/low.m3u8"},
	{"empty_host", "https:///low.m3u8"},
	{"empty_label", "https://variant..test/low.m3u8"},
	{"leading_dot", "https://.variant.test/low.m3u8"},
	{"non_ascii_host", "https://exámple.test/low.m3u8"},
	{"non_https_scheme", "file:///etc/passwd"},
	{"opaque_scheme", "javascript:alert(1)"},
	// Refused by url.Parse itself (an ASCII percent-escape is not legal in a
	// host), i.e. this exercises "malformed URLs fail closed" rather than the
	// localhost name rule it superficially resembles.
	{"malformed_escape", "https://%6c%6fcalhost/low.m3u8"},
	// As a playlist line this is refused by url.Parse — but only because this
	// module declares go1.26, which is when url.Parse began insisting a
	// bracketed authority really is an IPv6 address. The policy does not lean on
	// that: the bracket rule refuses the shape on its own, which
	// TestHLSDoRefusesForbiddenTargetsWithoutIO proves with a URL value built
	// rather than parsed.
	{"bracketed_non_ip", "https://[not-an-ip]/low.m3u8"},
	{"colon_no_port", "https://variant.test:/low.m3u8"},
}

// TestSendRejectsForbiddenVariantURLBeforeAnyIO drives the real Send path with a
// master playlist whose single URI line names a forbidden target. The target
// must never be contacted, and the master request must remain the ONLY request
// the transport ever sees on the HLS leg.
func TestSendRejectsForbiddenVariantURLBeforeAnyIO(t *testing.T) {
	master := c1MasterURL(c1Channel)

	for _, tc := range c1ForbiddenTargets {
		t.Run(tc.name, func(t *testing.T) {
			rt := newHLSRecordRT()
			rt.steps[master] = hlsStep{status: 200, body: c1Playlist(tc.url)}
			// Registered so that an implementation which DOES dereference the
			// forbidden target produces a clean, unambiguous "it was contacted"
			// signal rather than an opaque 404.
			rt.steps[tc.url] = hlsStep{status: 200, body: c1Playlist("https://seg.test/s.ts")}
			rt.steps["https://seg.test/s.ts"] = hlsStep{status: 200}

			sender, streamer := c1Sender(rt, c1Channel)
			res := sender.Send(context.Background(), streamer)

			if got := rt.hitCount(tc.url); got != 0 {
				t.Fatalf("forbidden variant target was contacted %d time(s): %q", got, tc.url)
			}
			if hits := rt.allHits(); len(hits) != 2 || hits[0] != master || hits[1] != c1Spade {
				t.Fatalf("expected exactly the master request then the beacon, got %q", hits)
			}
			if res.SimulateErr == nil {
				t.Fatal("expected a redacted SimulateErr for the rejected variant target")
			}
			var sf *simulateFailure
			if !errors.As(res.SimulateErr, &sf) {
				t.Fatalf("expected a redacted *simulateFailure, got %T (%v)", res.SimulateErr, res.SimulateErr)
			}
			if sf.Stage != StagePlaylist {
				t.Fatalf("expected StagePlaylist, got %q", sf.Stage)
			}
			// The HLS leg is best-effort for a production send: the beacon
			// still goes out and the minute still counts.
			if !res.Delivered {
				t.Fatalf("a rejected HLS target must stay best-effort for Send, got %+v", res)
			}
		})
	}
}

// TestSendRejectsForbiddenSegmentURLBeforeAnyIO is the same proof one hop
// deeper: the media playlist names a forbidden segment.
func TestSendRejectsForbiddenSegmentURLBeforeAnyIO(t *testing.T) {
	master := c1MasterURL(c1Channel)
	const media = "https://variant.test/low.m3u8"

	for _, tc := range c1ForbiddenTargets {
		t.Run(tc.name, func(t *testing.T) {
			rt := newHLSRecordRT()
			rt.steps[master] = hlsStep{status: 200, body: c1Playlist(media)}
			rt.steps[media] = hlsStep{status: 200, body: c1Playlist(tc.url)}
			rt.steps[tc.url] = hlsStep{status: 200}

			sender, streamer := c1Sender(rt, c1Channel)
			res := sender.Send(context.Background(), streamer)

			if got := rt.hitCount(tc.url); got != 0 {
				t.Fatalf("forbidden segment target was contacted %d time(s): %q", got, tc.url)
			}
			if hits := rt.allHits(); len(hits) != 3 || hits[0] != master || hits[1] != media || hits[2] != c1Spade {
				t.Fatalf("expected master, media, beacon only, got %q", hits)
			}
			if res.SimulateErr == nil {
				t.Fatal("expected a redacted SimulateErr for the rejected segment target")
			}
			var sf *simulateFailure
			if !errors.As(res.SimulateErr, &sf) {
				t.Fatalf("expected a redacted *simulateFailure, got %T (%v)", res.SimulateErr, res.SimulateErr)
			}
			if sf.Stage != StageSegment {
				t.Fatalf("expected StageSegment, got %q", sf.Stage)
			}
		})
	}
}

// TestSendRejectsForbiddenMasterURLBeforeAnyIO covers master URL ADMISSION. The
// master target is the one URL simulateWatching builds itself — but it embeds a
// remote-supplied login, so it is still validated before I/O. A login carrying a
// '#' turns the rest of the master URL (including the signed query) into a
// fragment; that URL must be refused with zero requests of any kind.
func TestSendRejectsForbiddenMasterURLBeforeAnyIO(t *testing.T) {
	const evilChannel = "c1chan#evil"

	rt := newHLSRecordRT()
	c1OKChain(rt)
	// Whatever URL a non-validating implementation would end up building, it
	// would still reach the transport — and allHits below proves it did not.

	sender, streamer := c1Sender(rt, evilChannel)
	res := sender.Send(context.Background(), streamer)

	hits := rt.allHits()
	if len(hits) != 1 || hits[0] != c1Spade {
		t.Fatalf("a rejected master URL must produce no HLS request at all, got %q", hits)
	}
	if res.SimulateErr == nil {
		t.Fatal("expected a redacted SimulateErr for the rejected master URL")
	}
	var sf *simulateFailure
	if !errors.As(res.SimulateErr, &sf) {
		t.Fatalf("expected a redacted *simulateFailure, got %T (%v)", res.SimulateErr, res.SimulateErr)
	}
	if sf.Stage != StagePlaylist {
		t.Fatalf("expected StagePlaylist, got %q", sf.Stage)
	}
}

// TestHLSRequestHeadersAreUnchanged pins the exact headers this path puts on the
// wire, because they are live Twitch traffic and the boundary had no business
// changing them: the two playlist GETs carry no User-Agent (net/http supplies
// its default), and only the segment HEAD carries the TV User-Agent. A refactor
// that unified the three requests would most likely do so by giving them all the
// same header, which is a behaviour change this test refuses.
func TestHLSRequestHeadersAreUnchanged(t *testing.T) {
	master := c1MasterURL(c1Channel)
	const media = "https://variant.test/low.m3u8"
	const segment = "https://seg.test/s.ts"

	rt := newHLSRecordRT()
	c1OKChain(rt)

	sender, streamer := c1Sender(rt, c1Channel)
	if res := sender.Send(context.Background(), streamer); !res.Delivered {
		t.Fatalf("expected Delivered, got %+v", res)
	}

	for _, target := range []string{master, media} {
		if ua := rt.headerFor(target).Get("User-Agent"); ua != "" {
			t.Fatalf("the playlist GET for %q must not set a User-Agent, got %q", target, ua)
		}
		if got := rt.methodFor(target); got != http.MethodGet {
			t.Fatalf("expected a GET for %q, got %q", target, got)
		}
	}
	if ua := rt.headerFor(segment).Get("User-Agent"); ua != constants.TVUserAgent {
		t.Fatalf("the segment HEAD must carry the TV User-Agent, got %q", ua)
	}
	if got := rt.methodFor(segment); got != http.MethodHead {
		t.Fatalf("expected a HEAD for the segment, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// B. Allowed host policy: no same-origin rule, no Twitch-suffix allowlist.
// ---------------------------------------------------------------------------

// TestSendAcceptsAllowedHLSTargets proves the boundary did NOT quietly become an
// origin or suffix allowlist. Every entry names a target on a host unrelated to
// Twitch and unrelated to the master's origin; each must be contacted.
func TestSendAcceptsAllowedHLSTargets(t *testing.T) {
	master := c1MasterURL(c1Channel)

	cases := []struct {
		name     string
		line     string
		resolved string
	}{
		{"cross_origin_registered_name", "https://unrelated-cdn.example/low.m3u8", "https://unrelated-cdn.example/low.m3u8"},
		{"explicit_port_443", "https://variant.test:443/low.m3u8", "https://variant.test:443/low.m3u8"},
		{"uppercase_host", "https://VARIANT.TEST/low.m3u8", "https://VARIANT.TEST/low.m3u8"},
		{"deep_subdomain", "https://video-weaver.lhr03.hls.ttvnw.net/v1/playlist.m3u8", "https://video-weaver.lhr03.hls.ttvnw.net/v1/playlist.m3u8"},
		{"host_with_digits", "https://cdn-12.edge3.example/low.m3u8", "https://cdn-12.edge3.example/low.m3u8"},
		{"query_preserved", "https://variant.test/low.m3u8?sig=abc&token=def", "https://variant.test/low.m3u8?sig=abc&token=def"},
		{"protocol_relative", "//other.test/low.m3u8", "https://other.test/low.m3u8"},
		{"trailing_dot_fqdn", "https://variant.test./low.m3u8", "https://variant.test./low.m3u8"},
		// The C1/C2 boundary marker. A single-label intranet name passes every
		// rule here BY DESIGN: deciding it needs name resolution, which this
		// boundary deliberately does not do. If a later change makes this row
		// fail, it has quietly taken on the DNS/connect concern.
		{"single_label_intranet_host", "https://intranet/low.m3u8", "https://intranet/low.m3u8"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := newHLSRecordRT()
			rt.steps[master] = hlsStep{status: 200, body: c1Playlist(tc.line)}
			rt.steps[tc.resolved] = hlsStep{status: 200, body: c1Playlist("https://seg.test/s.ts")}
			rt.steps["https://seg.test/s.ts"] = hlsStep{status: 200}

			sender, streamer := c1Sender(rt, c1Channel)
			res := sender.Send(context.Background(), streamer)

			if got := rt.hitCount(tc.resolved); got != 1 {
				t.Fatalf("allowed target %q was contacted %d time(s); hits=%q", tc.resolved, got, rt.allHits())
			}
			if res.SimulateErr != nil {
				t.Fatalf("allowed target must not produce a simulate failure, got %v", res.SimulateErr)
			}
			if !res.Delivered {
				t.Fatalf("expected Delivered, got %+v", res)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// C. Redirect target validation, per HLS role.
// ---------------------------------------------------------------------------

// c1RedirectRole describes one of the three HLS requests, so the redirect
// matrix below can be run identically against each.
type c1RedirectRole struct {
	name string
	// setup registers a chain in which the named role answers 302 -> target.
	setup func(rt *hlsRecordRT, target string)
	stage ProbeStage
	// hlsHits is how many HLS requests the leg makes before the refused
	// redirect: the requests up to and including the one that answered 302.
	// Asserting it is what makes "the target was never contacted" mean
	// something for a target whose Request.URL.String() drops a fragment and so
	// can never equal the fixture key.
	hlsHits int
}

func c1RedirectRoles() []c1RedirectRole {
	master := c1MasterURL(c1Channel)
	const media = "https://variant.test/low.m3u8"
	const segment = "https://seg.test/s.ts"

	return []c1RedirectRole{
		{
			name: "master",
			setup: func(rt *hlsRecordRT, target string) {
				rt.steps[master] = hlsStep{status: 302, location: target}
				rt.steps[media] = hlsStep{status: 200, body: c1Playlist(segment)}
				rt.steps[segment] = hlsStep{status: 200}
			},
			stage:   StagePlaylist,
			hlsHits: 1,
		},
		{
			name: "media",
			setup: func(rt *hlsRecordRT, target string) {
				rt.steps[master] = hlsStep{status: 200, body: c1Playlist(media)}
				rt.steps[media] = hlsStep{status: 302, location: target}
				rt.steps[segment] = hlsStep{status: 200}
			},
			stage:   StagePlaylist,
			hlsHits: 2,
		},
		{
			name: "segment",
			setup: func(rt *hlsRecordRT, target string) {
				rt.steps[master] = hlsStep{status: 200, body: c1Playlist(media)}
				rt.steps[media] = hlsStep{status: 200, body: c1Playlist(segment)}
				rt.steps[segment] = hlsStep{status: 302, location: target}
			},
			stage:   StageSegment,
			hlsHits: 3,
		},
	}
}

// TestSendValidatesHLSRedirectTargetsBeforeContact proves that at EVERY HLS hop
// a redirect to a forbidden target is refused before that target is contacted.
func TestSendValidatesHLSRedirectTargetsBeforeContact(t *testing.T) {
	for _, role := range c1RedirectRoles() {
		for _, tc := range c1ForbiddenTargets {
			t.Run(role.name+"/"+tc.name, func(t *testing.T) {
				rt := newHLSRecordRT()
				role.setup(rt, tc.url)
				// Registered so a following implementation is unambiguous.
				rt.steps[tc.url] = hlsStep{status: 200, body: c1Playlist("https://seg.test/s.ts")}

				sender, streamer := c1Sender(rt, c1Channel)
				res := sender.Send(context.Background(), streamer)

				if got := rt.hitCount(tc.url); got != 0 {
					t.Fatalf("forbidden %s redirect target was contacted %d time(s): %q", role.name, got, tc.url)
				}
				// hitCount alone cannot see a target whose Request.URL.String()
				// differs from the fixture (a fragment is dropped), so pin the
				// whole leg: the chain must stop at the request that answered
				// the 302 and go no further.
				if hits := c1HLSHits(rt); len(hits) != role.hlsHits {
					t.Fatalf("expected the %s leg to stop after %d request(s), got %q", role.name, role.hlsHits, hits)
				}
				if res.SimulateErr == nil {
					t.Fatalf("expected a redacted SimulateErr for the rejected %s redirect", role.name)
				}
				var sf *simulateFailure
				if !errors.As(res.SimulateErr, &sf) {
					t.Fatalf("expected a redacted *simulateFailure, got %T (%v)", res.SimulateErr, res.SimulateErr)
				}
				if sf.Stage != role.stage {
					t.Fatalf("expected stage %q, got %q", role.stage, sf.Stage)
				}
			})
		}
	}
}

// TestSendFollowsAllowedHLSRedirects is the other half of the matrix: a
// permitted redirect at every role must still be followed, so C1 hardens the
// boundary without turning redirects off.
func TestSendFollowsAllowedHLSRedirects(t *testing.T) {
	master := c1MasterURL(c1Channel)
	const media = "https://variant.test/low.m3u8"
	const segment = "https://seg.test/s.ts"

	cases := []struct {
		name   string
		setup  func(rt *hlsRecordRT)
		target string
	}{
		{
			name: "master",
			setup: func(rt *hlsRecordRT) {
				rt.steps[master] = hlsStep{status: 302, location: "https://master-hop2.test/m.m3u8"}
				rt.steps["https://master-hop2.test/m.m3u8"] = hlsStep{status: 200, body: c1Playlist(media)}
				rt.steps[media] = hlsStep{status: 200, body: c1Playlist(segment)}
				rt.steps[segment] = hlsStep{status: 200}
			},
			target: "https://master-hop2.test/m.m3u8",
		},
		{
			name: "media",
			setup: func(rt *hlsRecordRT) {
				rt.steps[master] = hlsStep{status: 200, body: c1Playlist(media)}
				rt.steps[media] = hlsStep{status: 302, location: "https://media-hop2.test/v.m3u8"}
				rt.steps["https://media-hop2.test/v.m3u8"] = hlsStep{status: 200, body: c1Playlist(segment)}
				rt.steps[segment] = hlsStep{status: 200}
			},
			target: "https://media-hop2.test/v.m3u8",
		},
		{
			name: "segment",
			setup: func(rt *hlsRecordRT) {
				rt.steps[master] = hlsStep{status: 200, body: c1Playlist(media)}
				rt.steps[media] = hlsStep{status: 200, body: c1Playlist(segment)}
				rt.steps[segment] = hlsStep{status: 302, location: "https://seg-hop2.test/s.ts"}
				rt.steps["https://seg-hop2.test/s.ts"] = hlsStep{status: 200}
			},
			target: "https://seg-hop2.test/s.ts",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := newHLSRecordRT()
			tc.setup(rt)

			sender, streamer := c1Sender(rt, c1Channel)
			res := sender.Send(context.Background(), streamer)

			if got := rt.hitCount(tc.target); got != 1 {
				t.Fatalf("allowed %s redirect was not followed (hits=%d); all=%q", tc.name, got, rt.allHits())
			}
			// The segment request is a HEAD and must stay one across the hop:
			// production never reads that body, so a helper that silently
			// promoted it to a GET would start downloading media segments.
			if tc.name == "segment" {
				if got := rt.methodFor(tc.target); got != http.MethodHead {
					t.Fatalf("the redirected segment request must stay a HEAD, got %q", got)
				}
			}
			if res.SimulateErr != nil {
				t.Fatalf("allowed redirect must not fail the simulation, got %v", res.SimulateErr)
			}
			if !res.Delivered {
				t.Fatalf("expected Delivered, got %+v", res)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// D. Redirect ceiling: Go's ten-actual-request maximum survives the custom
//    CheckRedirect policy.
// ---------------------------------------------------------------------------

// TestHLSRedirectCeilingMatchesGoDefault builds a redirect chain longer than the
// ceiling. Exactly ten requests may be made on that leg; the eleventh target
// must never be contacted.
func TestHLSRedirectCeilingMatchesGoDefault(t *testing.T) {
	master := c1MasterURL(c1Channel)

	rt := newHLSRecordRT()
	hop := func(i int) string { return fmt.Sprintf("https://hop%d.test/m.m3u8", i) }

	// master -> hop1 -> hop2 -> ... -> hop12. The master request itself is the
	// first of the ten permitted requests, so hop1..hop9 are reachable and
	// hop10 is the first target beyond the ceiling.
	rt.steps[master] = hlsStep{status: 302, location: hop(1)}
	for i := 1; i <= 12; i++ {
		rt.steps[hop(i)] = hlsStep{status: 302, location: hop(i + 1)}
	}

	sender, streamer := c1Sender(rt, c1Channel)
	res := sender.Send(context.Background(), streamer)

	hlsHits := 0
	for _, h := range rt.allHits() {
		if h != c1Spade {
			hlsHits++
		}
	}
	if hlsHits != 10 {
		t.Fatalf("expected exactly 10 actual HLS requests (Go's ceiling), got %d: %q", hlsHits, rt.allHits())
	}
	if got := rt.hitCount(hop(9)); got != 1 {
		t.Fatalf("hop9 is inside the ceiling and should have been contacted once, got %d", got)
	}
	if got := rt.hitCount(hop(10)); got != 0 {
		t.Fatalf("hop10 is beyond the ceiling and must never be contacted, got %d hit(s)", got)
	}
	if res.SimulateErr == nil {
		t.Fatal("an exhausted redirect chain must surface as a simulate failure")
	}
}

// ---------------------------------------------------------------------------
// E. Referer: the signed previous URL must never be forwarded.
// ---------------------------------------------------------------------------

// TestHLSRedirectsStripReferer starts from the signed master URL (sig + token in
// the query, which is exactly what Go's refererForURL would copy verbatim into a
// Referer) and follows an allowed redirect. The redirected request must carry no
// Referer at all, and no sentinel may appear in any header of any request, in
// the redacted SimulateErr, or in the probe result.
func TestHLSRedirectsStripReferer(t *testing.T) {
	master := c1MasterURL(c1Channel)
	const media = "https://variant.test/low.m3u8"
	const segment = "https://seg.test/s.ts"

	cases := []struct {
		name   string
		setup  func(rt *hlsRecordRT)
		target string
	}{
		{
			name: "master_hop",
			setup: func(rt *hlsRecordRT) {
				rt.steps[master] = hlsStep{status: 302, location: "https://master-hop2.test/m.m3u8"}
				rt.steps["https://master-hop2.test/m.m3u8"] = hlsStep{status: 200, body: c1Playlist(media)}
				rt.steps[media] = hlsStep{status: 200, body: c1Playlist(segment)}
				rt.steps[segment] = hlsStep{status: 200}
			},
			target: "https://master-hop2.test/m.m3u8",
		},
		{
			name: "media_hop",
			setup: func(rt *hlsRecordRT) {
				rt.steps[master] = hlsStep{status: 200, body: c1Playlist(media + "?sig=" + c1Sig + "&token=" + c1Token)}
				rt.steps[media+"?sig="+c1Sig+"&token="+c1Token] = hlsStep{status: 302, location: "https://media-hop2.test/v.m3u8"}
				rt.steps["https://media-hop2.test/v.m3u8"] = hlsStep{status: 200, body: c1Playlist(segment)}
				rt.steps[segment] = hlsStep{status: 200}
			},
			target: "https://media-hop2.test/v.m3u8",
		},
		{
			name: "segment_hop",
			setup: func(rt *hlsRecordRT) {
				rt.steps[master] = hlsStep{status: 200, body: c1Playlist(media)}
				rt.steps[media] = hlsStep{status: 200, body: c1Playlist(segment + "?sig=" + c1Sig)}
				rt.steps[segment+"?sig="+c1Sig] = hlsStep{status: 302, location: "https://seg-hop2.test/s.ts"}
				rt.steps["https://seg-hop2.test/s.ts"] = hlsStep{status: 200}
			},
			target: "https://seg-hop2.test/s.ts",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := newHLSRecordRT()
			tc.setup(rt)

			sender, streamer := c1Sender(rt, c1Channel)
			res := sender.Send(context.Background(), streamer)

			if got := rt.hitCount(tc.target); got != 1 {
				t.Fatalf("redirect target not followed exactly once (hits=%d); all=%q", got, rt.allHits())
			}
			h := rt.headerFor(tc.target)
			if ref := h.Get("Referer"); ref != "" {
				t.Fatalf("redirected request carried a Referer: %q", ref)
			}
			rt.mu.Lock()
			headers := append([]http.Header(nil), rt.headers...)
			rt.mu.Unlock()
			for i, hdr := range headers {
				for k, vs := range hdr {
					for _, v := range vs {
						for _, secret := range []string{c1Sig, c1Token} {
							if strings.Contains(v, secret) {
								t.Fatalf("request %d header %s leaked %q: %q", i, k, secret, v)
							}
						}
					}
				}
			}
			if res.SimulateErr != nil {
				t.Fatalf("allowed redirect must not fail the simulation, got %v", res.SimulateErr)
			}
		})
	}
}

// TestHLSBoundaryErrorsNeverLeakSignedMaterial drives a rejected redirect (the
// path where Go embeds the raw Location in the *url.Error it returns) and a
// rejected child URL, and asserts nothing signed or raw escapes into the value
// the miner logs, or into the probe's operator-visible fields.
func TestHLSBoundaryErrorsNeverLeakSignedMaterial(t *testing.T) {
	master := c1MasterURL(c1Channel)
	const leak = "http://leak.test/steal?sig=" + c1Sig + "&token=" + c1Token

	rt := newHLSRecordRT()
	rt.steps[master] = hlsStep{status: 302, location: leak}
	rt.steps[leak] = hlsStep{status: 200, body: c1Playlist("https://seg.test/s.ts")}

	sender, streamer := c1Sender(rt, c1Channel)

	res := sender.Send(context.Background(), streamer)
	if res.SimulateErr == nil {
		t.Fatal("expected a rejected redirect to produce a SimulateErr")
	}
	assertNoSignedMaterial(t, "SendResult.SimulateErr", res.SimulateErr.Error())

	probe := sender.Probe(context.Background(), streamer)
	if probe.OK {
		t.Fatal("expected the probe to fail on a rejected redirect")
	}
	assertNoSignedMaterial(t, "ProbeResult.ErrorCode", probe.ErrorCode)
	assertNoSignedMaterial(t, "ProbeResult.Stage", string(probe.Stage))
}

func assertNoSignedMaterial(t *testing.T, what, got string) {
	t.Helper()
	for _, secret := range []string{c1Sig, c1Token, "sig=", "token=", "usher.ttvnw.net", "leak.test", "http://", "https://"} {
		if strings.Contains(got, secret) {
			t.Fatalf("%s leaked %q: %q", what, secret, got)
		}
	}
}

// ---------------------------------------------------------------------------
// F. Bare relative URI lines resolve against the FINAL response URL.
// ---------------------------------------------------------------------------

// TestRelativeVariantResolvesAgainstFinalMasterURL proves the relative base is
// the master's post-redirect URL, not the URL originally requested.
func TestRelativeVariantResolvesAgainstFinalMasterURL(t *testing.T) {
	master := c1MasterURL(c1Channel)
	const finalMaster = "https://final-master.test/hls/master.m3u8"
	const wantMedia = "https://final-master.test/hls/low.m3u8"
	// The URL a pre-redirect base would produce. It is registered and answers
	// successfully, so resolving against the wrong base fails by assertion
	// rather than by an incidental transport error.
	const wrongMedia = constants.UsherURL + "/api/channel/hls/low.m3u8"

	rt := newHLSRecordRT()
	rt.steps[master] = hlsStep{status: 302, location: finalMaster}
	rt.steps[finalMaster] = hlsStep{status: 200, body: c1Playlist("low.m3u8")}
	rt.steps[wantMedia] = hlsStep{status: 200, body: c1Playlist("https://seg.test/s.ts")}
	rt.steps[wrongMedia] = hlsStep{status: 200, body: c1Playlist("https://seg.test/s.ts")}
	rt.steps["https://seg.test/s.ts"] = hlsStep{status: 200}

	sender, streamer := c1Sender(rt, c1Channel)
	res := sender.Send(context.Background(), streamer)

	if got := rt.hitCount(wantMedia); got != 1 {
		t.Fatalf("relative media URI was not resolved against the final master URL (hits=%d); all=%q", got, rt.allHits())
	}
	if got := rt.hitCount(wrongMedia); got != 0 {
		t.Fatalf("relative media URI was resolved against the PRE-redirect base %d time(s)", got)
	}
	if res.SimulateErr != nil {
		t.Fatalf("expected the relative chain to succeed, got %v", res.SimulateErr)
	}
}

// TestRelativeSegmentResolvesAgainstFinalMediaURL is the same proof for the
// media playlist -> segment role.
func TestRelativeSegmentResolvesAgainstFinalMediaURL(t *testing.T) {
	master := c1MasterURL(c1Channel)
	const media = "https://variant.test/v/low.m3u8"
	const finalMedia = "https://final-media.test/seg/media.m3u8"
	const wantSegment = "https://final-media.test/seg/s1.ts"
	const wrongSegment = "https://variant.test/v/s1.ts"

	rt := newHLSRecordRT()
	rt.steps[master] = hlsStep{status: 200, body: c1Playlist(media)}
	rt.steps[media] = hlsStep{status: 302, location: finalMedia}
	rt.steps[finalMedia] = hlsStep{status: 200, body: c1Playlist("s1.ts")}
	rt.steps[wantSegment] = hlsStep{status: 200}
	rt.steps[wrongSegment] = hlsStep{status: 200}

	sender, streamer := c1Sender(rt, c1Channel)
	res := sender.Send(context.Background(), streamer)

	if got := rt.hitCount(wantSegment); got != 1 {
		t.Fatalf("relative segment URI was not resolved against the final media URL (hits=%d); all=%q", got, rt.allHits())
	}
	if got := rt.hitCount(wrongSegment); got != 0 {
		t.Fatalf("relative segment URI was resolved against the PRE-redirect base %d time(s)", got)
	}
	if res.SimulateErr != nil {
		t.Fatalf("expected the relative chain to succeed, got %v", res.SimulateErr)
	}
}

// TestRelativeURIStillPassesTheURLPolicy proves a relative line cannot smuggle a
// forbidden target past the validator: the RESOLVED absolute URL is checked with
// the same policy, before any I/O.
func TestRelativeURIStillPassesTheURLPolicy(t *testing.T) {
	master := c1MasterURL(c1Channel)
	const finalMaster = "https://localhost-shaped.test/hls/master.m3u8"

	cases := []struct {
		name     string
		line     string
		resolved string
	}{
		{"relative_with_fragment", "low.m3u8#frag", "https://localhost-shaped.test/hls/low.m3u8#frag"},
		{"protocol_relative_to_ip", "//127.0.0.1/low.m3u8", "https://127.0.0.1/low.m3u8"},
		{"protocol_relative_to_localhost", "//localhost/low.m3u8", "https://localhost/low.m3u8"},
		{"absolute_downgrade", "http://localhost-shaped.test/hls/low.m3u8", "http://localhost-shaped.test/hls/low.m3u8"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := newHLSRecordRT()
			rt.steps[master] = hlsStep{status: 302, location: finalMaster}
			rt.steps[finalMaster] = hlsStep{status: 200, body: c1Playlist(tc.line)}
			rt.steps[tc.resolved] = hlsStep{status: 200, body: c1Playlist("https://seg.test/s.ts")}

			sender, streamer := c1Sender(rt, c1Channel)
			res := sender.Send(context.Background(), streamer)

			if got := rt.hitCount(tc.resolved); got != 0 {
				t.Fatalf("a resolved-but-forbidden relative target was contacted %d time(s): %q", got, tc.resolved)
			}
			// hitCount alone is not enough for a target whose only difference
			// is a fragment (Request.URL.String() drops it), so pin the whole
			// HLS leg: master, then the redirected master, and nothing else.
			if hits := c1HLSHits(rt); len(hits) != 2 {
				t.Fatalf("expected exactly the master and its redirect target, got %q", hits)
			}
			if res.SimulateErr == nil {
				t.Fatal("expected a simulate failure for the resolved-but-forbidden target")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// G. Playlist body bound.
// ---------------------------------------------------------------------------

// c1PlaylistOfSize builds a playlist of exactly size bytes whose only URI line
// is uri (padding is one long comment line, which the URI scan skips).
func c1PlaylistOfSize(t *testing.T, size int, uri string) string {
	t.Helper()
	tail := uri + "\n"
	if len(tail)+2 > size {
		t.Fatalf("size %d too small for uri %q", size, uri)
	}
	pad := size - len(tail)
	return strings.Repeat("#", pad-1) + "\n" + tail
}

func TestPlaylistBodyBound(t *testing.T) {
	master := c1MasterURL(c1Channel)
	const media = "https://variant.test/low.m3u8"
	const segment = "https://seg.test/s.ts"
	const limit = 1 << 20

	t.Run("master_at_limit_accepted", func(t *testing.T) {
		rt := newHLSRecordRT()
		rt.steps[master] = hlsStep{status: 200, body: c1PlaylistOfSize(t, limit, media)}
		rt.steps[media] = hlsStep{status: 200, body: c1Playlist(segment)}
		rt.steps[segment] = hlsStep{status: 200}

		sender, streamer := c1Sender(rt, c1Channel)
		res := sender.Send(context.Background(), streamer)

		if res.SimulateErr != nil {
			t.Fatalf("a master playlist of exactly %d bytes must be accepted, got %v", limit, res.SimulateErr)
		}
		if got := rt.hitCount(media); got != 1 {
			t.Fatalf("expected the media playlist to be fetched once, got %d", got)
		}
	})

	t.Run("master_over_limit_rejected", func(t *testing.T) {
		rt := newHLSRecordRT()
		rt.steps[master] = hlsStep{status: 200, body: c1PlaylistOfSize(t, limit+1, media)}
		rt.steps[media] = hlsStep{status: 200, body: c1Playlist(segment)}
		rt.steps[segment] = hlsStep{status: 200}

		sender, streamer := c1Sender(rt, c1Channel)
		res := sender.Send(context.Background(), streamer)

		if res.SimulateErr == nil {
			t.Fatalf("a master playlist of %d bytes must be rejected", limit+1)
		}
		if got := rt.hitCount(media); got != 0 {
			t.Fatalf("an oversized master playlist must not proceed to a child request, got %d", got)
		}
	})

	t.Run("media_at_limit_accepted", func(t *testing.T) {
		rt := newHLSRecordRT()
		rt.steps[master] = hlsStep{status: 200, body: c1Playlist(media)}
		rt.steps[media] = hlsStep{status: 200, body: c1PlaylistOfSize(t, limit, segment)}
		rt.steps[segment] = hlsStep{status: 200}

		sender, streamer := c1Sender(rt, c1Channel)
		res := sender.Send(context.Background(), streamer)

		if res.SimulateErr != nil {
			t.Fatalf("a media playlist of exactly %d bytes must be accepted, got %v", limit, res.SimulateErr)
		}
		if got := rt.hitCount(segment); got != 1 {
			t.Fatalf("expected the segment to be fetched once, got %d", got)
		}
	})

	t.Run("media_over_limit_rejected", func(t *testing.T) {
		rt := newHLSRecordRT()
		rt.steps[master] = hlsStep{status: 200, body: c1Playlist(media)}
		rt.steps[media] = hlsStep{status: 200, body: c1PlaylistOfSize(t, limit+1, segment)}
		rt.steps[segment] = hlsStep{status: 200}

		sender, streamer := c1Sender(rt, c1Channel)
		res := sender.Send(context.Background(), streamer)

		if res.SimulateErr == nil {
			t.Fatalf("a media playlist of %d bytes must be rejected", limit+1)
		}
		if got := rt.hitCount(segment); got != 0 {
			t.Fatalf("an oversized media playlist must not proceed to a segment request, got %d", got)
		}
	})
}

// ---------------------------------------------------------------------------
// H. Send vs Probe: the existing best-effort / fatal split is unchanged.
// ---------------------------------------------------------------------------

func TestHLSBoundaryIsBestEffortInSendAndFatalInProbe(t *testing.T) {
	master := c1MasterURL(c1Channel)
	const media = "https://variant.test/low.m3u8"

	cases := []struct {
		name  string
		setup func(rt *hlsRecordRT)
		stage ProbeStage
	}{
		{
			name: "forbidden_media_url",
			setup: func(rt *hlsRecordRT) {
				rt.steps[master] = hlsStep{status: 200, body: c1Playlist("http://variant.test/low.m3u8")}
			},
			stage: StagePlaylist,
		},
		{
			name: "forbidden_segment_url",
			setup: func(rt *hlsRecordRT) {
				rt.steps[master] = hlsStep{status: 200, body: c1Playlist(media)}
				rt.steps[media] = hlsStep{status: 200, body: c1Playlist("https://127.0.0.1/s.ts")}
			},
			stage: StageSegment,
		},
		{
			name: "forbidden_redirect",
			setup: func(rt *hlsRecordRT) {
				rt.steps[master] = hlsStep{status: 302, location: "https://localhost/m.m3u8"}
			},
			stage: StagePlaylist,
		},
		{
			name: "oversized_master_body",
			setup: func(rt *hlsRecordRT) {
				rt.steps[master] = hlsStep{status: 200, body: c1PlaylistOfSize(t, (1<<20)+1, media)}
			},
			stage: StagePlaylist,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rtSend := newHLSRecordRT()
			tc.setup(rtSend)
			sender, streamer := c1Sender(rtSend, c1Channel)

			res := sender.Send(context.Background(), streamer)
			if !res.Delivered {
				t.Fatalf("Send must stay best-effort across the HLS leg, got %+v", res)
			}
			if res.Failure != nil {
				t.Fatalf("an HLS boundary rejection must not become a fatal Send failure, got %+v", res.Failure)
			}
			if res.SimulateErr == nil {
				t.Fatal("expected the informational SimulateErr to be set")
			}

			rtProbe := newHLSRecordRT()
			tc.setup(rtProbe)
			probeSender, probeStreamer := c1Sender(rtProbe, c1Channel)

			probe := probeSender.Probe(context.Background(), probeStreamer)
			if probe.OK {
				t.Fatal("the same rejection must be fatal for the health probe")
			}
			if probe.Stage != tc.stage {
				t.Fatalf("expected probe stage %q, got %q", tc.stage, probe.Stage)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// I. Regression safety for the shared client and for B2's cancellation
//    ownership.
// ---------------------------------------------------------------------------

// TestHLSRequestsDoNotMutateSharedClient mirrors the guarantee postBeacon
// already gives: the redirect policy is installed on a per-request shallow copy,
// so concurrent sends through the same MinuteSender cannot race on it.
func TestHLSRequestsDoNotMutateSharedClient(t *testing.T) {
	rt := newHLSRecordRT()
	c1OKChain(rt)

	sender, streamer := c1Sender(rt, c1Channel)
	orig := sender.httpClient

	if res := sender.Send(context.Background(), streamer); !res.Delivered {
		t.Fatalf("expected Delivered, got %+v", res)
	}

	if sender.httpClient != orig {
		t.Fatal("the shared httpClient must not be replaced")
	}
	if sender.httpClient.CheckRedirect != nil {
		t.Fatal("the shared httpClient's CheckRedirect must remain unmodified")
	}
}

// TestHLSBoundaryConcurrentSendsAreRaceFree runs many sends through one
// MinuteSender at once. Under -race this fails if the redirect policy is
// installed on the shared client instead of a per-request copy.
func TestHLSBoundaryConcurrentSendsAreRaceFree(t *testing.T) {
	rt := newHLSRecordRT()
	c1OKChain(rt)
	rt.steps[c1MasterURL(c1Channel)] = hlsStep{status: 302, location: "https://master-hop2.test/m.m3u8"}
	rt.steps["https://master-hop2.test/m.m3u8"] = hlsStep{status: 200, body: c1Playlist("https://variant.test/low.m3u8")}

	sender, streamer := c1Sender(rt, c1Channel)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if res := sender.Send(context.Background(), streamer); !res.Delivered {
				t.Errorf("expected Delivered, got %+v", res)
			}
		}()
	}
	wg.Wait()
}

// TestHLSRedirectCancellationStaysOwnedByCallerContext is the B2 regression
// guard: the redirect helper must not detach from the caller's context. A
// generation cancelled mid-chain aborts the next hop and reports Cancelled, not
// a Twitch transport failure.
func TestHLSRedirectCancellationStaysOwnedByCallerContext(t *testing.T) {
	master := c1MasterURL(c1Channel)

	rt := newHLSRecordRT()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The master answers a permitted redirect; the generation is cancelled as
	// that response is produced, so the SECOND hop must never be attempted.
	rt.steps[master] = hlsStep{status: 302, location: "https://master-hop2.test/m.m3u8"}
	rt.steps["https://master-hop2.test/m.m3u8"] = hlsStep{status: 200, body: c1Playlist("https://variant.test/low.m3u8")}
	rt.steps["https://variant.test/low.m3u8"] = hlsStep{status: 200, body: c1Playlist("https://seg.test/s.ts")}
	rt.steps["https://seg.test/s.ts"] = hlsStep{status: 200}

	sender, streamer := c1Sender(rt, c1Channel)

	cancelOnMaster := &cancelAfterURL{rt: rt, url: master, cancel: cancel}
	sender.httpClient = &http.Client{Transport: cancelOnMaster}

	res := sender.Send(ctx, streamer)

	if !res.Cancelled {
		t.Fatalf("a cancelled generation must report Cancelled, got %+v", res)
	}
	if res.SimulateErr != nil {
		t.Fatalf("a cancelled generation must not report a simulate failure, got %v", res.SimulateErr)
	}
	if res.Failure != nil {
		t.Fatalf("a cancelled generation must not fabricate a transport failure, got %+v", res.Failure)
	}
	// The hop is built and handed to the transport — net/http does not test the
	// context between hops — but it is aborted there, and the chain stops: no
	// media playlist, no segment, and no beacon on a generation being torn down.
	hits := rt.allHits()
	if len(hits) != 2 || hits[0] != master || hits[1] != "https://master-hop2.test/m.m3u8" {
		t.Fatalf("a cancelled generation must stop at the aborted hop, got %q", hits)
	}
	if got := rt.hitCount(c1Spade); got != 0 {
		t.Fatalf("a cancelled generation must not post a beacon, got %d", got)
	}
}

// cancelAfterURL cancels the generation immediately after url has been served,
// so the next redirect hop starts on an already-dead context.
type cancelAfterURL struct {
	rt     *hlsRecordRT
	url    string
	cancel context.CancelFunc
}

func (c *cancelAfterURL) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.rt.RoundTrip(req)
	if req.URL.String() == c.url {
		c.cancel()
	}
	return resp, err
}

// ---------------------------------------------------------------------------
// J. The helper contracts themselves, independent of their current callers.
// ---------------------------------------------------------------------------

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("fixture %q did not parse: %v", raw, err)
	}
	return u
}

// TestHLSDoRefusesForbiddenTargetsWithoutIO pins the invariant the whole
// boundary rests on, at the one function that performs the I/O: hlsDo never
// contacts a target that fails the policy, whoever asked it to. Its callers
// resolve and validate first, so several of these shapes cannot reach it today
// — which is the point: the check is the floor, not a duplicate of the callers'.
func TestHLSDoRefusesForbiddenTargetsWithoutIO(t *testing.T) {
	cases := []struct {
		name   string
		target *url.URL
	}{
		{"nil_target", nil},
		{"parsed_fragment", &url.URL{Scheme: "https", Host: "variant.test", Path: "/low.m3u8", Fragment: "frag"}},
		{"raw_fragment", &url.URL{Scheme: "https", Host: "variant.test", Path: "/low.m3u8", RawFragment: "frag"}},
		{"opaque_url", &url.URL{Scheme: "https", Opaque: "//variant.test/low.m3u8"}},
		// Validate-one-thing-send-another: URL.String() writes Opaque and
		// ignores Host entirely, so a value carrying both would be checked
		// against variant.test and requested from evil.test. Refusing an opaque
		// URL is what keeps the field the policy inspects and the string the
		// request is built from talking about the same URL.
		{"opaque_url_shadowing_host", &url.URL{Scheme: "https", Opaque: "//evil.test/low.m3u8", Host: "variant.test"}},
		{"http_scheme", &url.URL{Scheme: "http", Host: "variant.test", Path: "/low.m3u8"}},
		{"userinfo", &url.URL{Scheme: "https", Host: "variant.test", User: url.UserPassword("u", "p"), Path: "/low.m3u8"}},
		{"ip_literal", &url.URL{Scheme: "https", Host: "127.0.0.1", Path: "/low.m3u8"}},
		// A bracketed authority whose contents are not an IP at all. Inside this
		// module url.Parse refuses to build it (a go1.26 language-version
		// behaviour), so presenting it needs a URL value assembled rather than
		// parsed — and then Hostname() hands back an innocent-looking registered
		// name and only the bracket check keeps it out. Pinning it here means the
		// rule holds on its own merits rather than on that parser behaviour.
		{"bracketed_non_ip", &url.URL{Scheme: "https", Host: "[not-an-ip]", Path: "/low.m3u8"}},
		{"bracketed_ipv6_zone", &url.URL{Scheme: "https", Host: "[fe80::1%eth0]", Path: "/low.m3u8"}},
		{"empty_host", &url.URL{Scheme: "https", Path: "/low.m3u8"}},
		{"root_label_only_host", &url.URL{Scheme: "https", Host: ".", Path: "/low.m3u8"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := newHLSRecordRT()
			sender, _ := c1Sender(rt, c1Channel)

			resp, final, err := sender.hlsDo(context.Background(), http.MethodGet, tc.target, nil)

			if err == nil {
				t.Fatal("expected the target to be refused")
			}
			if !errors.Is(err, errHLSURLRejected) {
				t.Fatalf("expected an errHLSURLRejected refusal, got %v", err)
			}
			if resp != nil || final != nil {
				t.Fatalf("a refused target must yield no response and no final URL, got resp=%v final=%v", resp, final)
			}
			if hits := rt.allHits(); len(hits) != 0 {
				t.Fatalf("a refused target must never reach the transport, got %q", hits)
			}
			assertNoSignedMaterial(t, "hlsDo refusal", err.Error())
		})
	}
}

// TestHLSDoSendsAndTracksAnAllowedTarget is the positive half of the contract:
// an admissible target is requested once and reported back as the final URL.
func TestHLSDoSendsAndTracksAnAllowedTarget(t *testing.T) {
	const target = "https://variant.test/low.m3u8"

	rt := newHLSRecordRT()
	rt.steps[target] = hlsStep{status: 200, body: "#EXTM3U\n"}
	sender, _ := c1Sender(rt, c1Channel)

	resp, final, err := sender.hlsDo(context.Background(), http.MethodGet, mustParseURL(t, target), nil)
	if err != nil {
		t.Fatalf("expected the target to be admitted, got %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if final == nil || final.String() != target {
		t.Fatalf("expected the final URL to be %q, got %v", target, final)
	}
	if hits := rt.allHits(); len(hits) != 1 || hits[0] != target {
		t.Fatalf("expected exactly one request to %q, got %q", target, hits)
	}
}

// TestSimulateWatchingPreIORefusalsCarryNoSignedMaterial pins the redaction contract
// for the errors this boundary ITSELF produces, at their source rather than only
// at the two call sites that redact. A pre-I/O refusal is entirely ours to shape,
// and the master URL it refuses carries the playback sig and token — so it must
// name a reason and never the URL.
//
// This is deliberately not a claim about every error simulateWatching can return.
// Once a request is actually sent, net/http builds a *url.Error around the URL it
// contacted, and there is no way to return that error without a URL in it; those
// stay safe because redactSimulateErr and probeFail discard them at the call
// sites, which existing tests pin.
func TestSimulateWatchingPreIORefusalsCarryNoSignedMaterial(t *testing.T) {
	cases := []struct {
		name    string
		channel string
	}{
		// A login that makes the master URL unparseable: the parse error would
		// quote the whole signed URL if it were ever propagated.
		{"unparseable_master_url", "c1%zzchan"},
		// A login that turns the signed query into a URL fragment.
		{"fragment_in_master_url", "c1chan#evil"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := newHLSRecordRT()
			sender, _ := c1Sender(rt, c1Channel)

			stage, status, err := sender.simulateWatching(context.Background(), tc.channel, c1Sig, c1Token)

			if err == nil {
				t.Fatal("expected the master URL to be refused")
			}
			if stage != StagePlaylist {
				t.Fatalf("expected StagePlaylist, got %q", stage)
			}
			if status != 0 {
				t.Fatalf("a pre-I/O refusal reached no response, so status must be 0, got %d", status)
			}
			if hits := rt.allHits(); len(hits) != 0 {
				t.Fatalf("a refused master URL must never reach the transport, got %q", hits)
			}
			assertNoSignedMaterial(t, "simulateWatching raw error", err.Error())
		})
	}
}

// ---------------------------------------------------------------------------
// K. Which URI line a playlist actually selects.
// ---------------------------------------------------------------------------

// TestPlaylistURISelection pins the selection rule across the playlist shapes
// Twitch really serves. The rule is "the last URI line": a master playlist is
// ordered best-quality first, so the last one is the lowest-bandwidth rendition
// this simulation has always chosen, and a media playlist's last URI line is its
// newest segment. Tag lines are not URI lines — a media playlist ending in
// #EXT-X-ENDLIST must still select the segment above it.
func TestPlaylistURISelection(t *testing.T) {
	master := c1MasterURL(c1Channel)

	t.Run("master", func(t *testing.T) {
		cases := []struct {
			name    string
			body    string
			want    string
			notWant string
		}{
			{
				name: "quality_ladder_picks_the_last_rendition",
				body: "#EXTM3U\n" +
					"#EXT-X-STREAM-INF:BANDWIDTH=6000000\nhttps://variant.test/chunked.m3u8\n" +
					"#EXT-X-STREAM-INF:BANDWIDTH=1500000\nhttps://variant.test/720p30.m3u8\n" +
					"#EXT-X-STREAM-INF:BANDWIDTH=230000\nhttps://variant.test/160p30.m3u8\n",
				want:    "https://variant.test/160p30.m3u8",
				notWant: "https://variant.test/chunked.m3u8",
			},
			{
				name:    "crlf_line_endings",
				body:    "#EXTM3U\r\n#EXT-X-STREAM-INF:BANDWIDTH=1\r\nhttps://variant.test/low.m3u8\r\n",
				want:    "https://variant.test/low.m3u8",
				notWant: "",
			},
			{
				name:    "trailing_blank_lines",
				body:    "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nhttps://variant.test/low.m3u8\n\n   \n\n",
				want:    "https://variant.test/low.m3u8",
				notWant: "",
			},
			{
				name: "trailing_tag_line",
				body: "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nhttps://variant.test/low.m3u8\n" +
					"#EXT-X-SESSION-DATA:DATA-ID=\"com.example\"\n",
				want:    "https://variant.test/low.m3u8",
				notWant: "",
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				rt := newHLSRecordRT()
				rt.steps[master] = hlsStep{status: 200, body: tc.body}
				rt.steps[tc.want] = hlsStep{status: 200, body: c1Playlist("https://seg.test/s.ts")}
				if tc.notWant != "" {
					rt.steps[tc.notWant] = hlsStep{status: 200, body: c1Playlist("https://seg.test/s.ts")}
				}
				rt.steps["https://seg.test/s.ts"] = hlsStep{status: 200}

				sender, streamer := c1Sender(rt, c1Channel)
				if res := sender.Send(context.Background(), streamer); res.SimulateErr != nil {
					t.Fatalf("expected the playlist to parse, got %v", res.SimulateErr)
				}
				if got := rt.hitCount(tc.want); got != 1 {
					t.Fatalf("expected %q to be requested once, got %d; all=%q", tc.want, got, rt.allHits())
				}
				if tc.notWant != "" {
					if got := rt.hitCount(tc.notWant); got != 0 {
						t.Fatalf("expected %q NOT to be requested, got %d", tc.notWant, got)
					}
				}
			})
		}
	})

	t.Run("media", func(t *testing.T) {
		const media = "https://variant.test/low.m3u8"

		cases := []struct {
			name    string
			body    string
			want    string
			notWant string
		}{
			{
				name: "newest_segment_is_the_last_uri_line",
				body: "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:100\n" +
					"#EXTINF:2.000,\nhttps://seg.test/100.ts\n" +
					"#EXTINF:2.000,\nhttps://seg.test/101.ts\n",
				want:    "https://seg.test/101.ts",
				notWant: "https://seg.test/100.ts",
			},
			{
				name: "endlist_tag_is_not_a_segment",
				body: "#EXTM3U\n" +
					"#EXTINF:2.000,\nhttps://seg.test/100.ts\n" +
					"#EXTINF:2.000,\nhttps://seg.test/101.ts\n" +
					"#EXT-X-ENDLIST\n",
				want:    "https://seg.test/101.ts",
				notWant: "https://seg.test/100.ts",
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				rt := newHLSRecordRT()
				rt.steps[master] = hlsStep{status: 200, body: c1Playlist(media)}
				rt.steps[media] = hlsStep{status: 200, body: tc.body}
				rt.steps[tc.want] = hlsStep{status: 200}
				rt.steps[tc.notWant] = hlsStep{status: 200}

				sender, streamer := c1Sender(rt, c1Channel)
				if res := sender.Send(context.Background(), streamer); res.SimulateErr != nil {
					t.Fatalf("expected the playlist to parse, got %v", res.SimulateErr)
				}
				if got := rt.hitCount(tc.want); got != 1 {
					t.Fatalf("expected %q to be requested once, got %d; all=%q", tc.want, got, rt.allHits())
				}
				if got := rt.hitCount(tc.notWant); got != 0 {
					t.Fatalf("expected %q NOT to be requested, got %d", tc.notWant, got)
				}
			})
		}
	})

	t.Run("no_uri_line_at_all", func(t *testing.T) {
		rt := newHLSRecordRT()
		rt.steps[master] = hlsStep{status: 200, body: "#EXTM3U\n#EXT-X-VERSION:3\n\n"}

		sender, streamer := c1Sender(rt, c1Channel)
		res := sender.Send(context.Background(), streamer)

		if res.SimulateErr == nil {
			t.Fatal("a playlist with no URI line must fail the simulation")
		}
		if hits := c1HLSHits(rt); len(hits) != 1 {
			t.Fatalf("expected only the master request, got %q", hits)
		}
	})
}

// ---------------------------------------------------------------------------
// L. The two pure helpers whose rules the URL policy leans on.
// ---------------------------------------------------------------------------

// TestIsNumericHostLabel pins the classification net.ParseIP cannot do. Every
// "true" row is a spelling that makes a host name an alias for an address, and
// every "false" row is an ordinary label that must keep resolving normally.
func TestIsNumericHostLabel(t *testing.T) {
	cases := []struct {
		label string
		want  bool
	}{
		{"1", true},
		{"007", true},
		{"2130706433", true},
		{"0x7f000001", true},
		{"0X7F000001", true},
		{"0xdeadBEEF", true},
		{"0x", true}, // "0x" with no digits is still the hexadecimal form
		{"", false},
		{"net", false},
		{"0xzz", false},
		{"1a", false},
		{"a1", false},
		{"-1", false},
		{"1.2", false}, // a label never contains a dot; this must not be numeric
		{"ttvnw", false},
		{"edge3", false},
	}

	for _, tc := range cases {
		if got := isNumericHostLabel(tc.label); got != tc.want {
			t.Errorf("isNumericHostLabel(%q) = %v, want %v", tc.label, got, tc.want)
		}
	}
}

// TestReadPlaylistBody pins the bound exactly at its edge, and pins that an
// overflow is refused rather than silently truncated — a truncating read would
// hand the parser a body whose last line is a fragment of a URL.
func TestReadPlaylistBody(t *testing.T) {
	t.Run("under_limit", func(t *testing.T) {
		body, err := readPlaylistBody(strings.NewReader("#EXTM3U\n"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(body) != "#EXTM3U\n" {
			t.Fatalf("body was altered: %q", body)
		}
	})

	t.Run("exactly_at_limit", func(t *testing.T) {
		in := strings.Repeat("a", 1<<20)
		body, err := readPlaylistBody(strings.NewReader(in))
		if err != nil {
			t.Fatalf("a body of exactly the limit must be accepted, got %v", err)
		}
		if len(body) != 1<<20 {
			t.Fatalf("expected %d bytes, got %d", 1<<20, len(body))
		}
	})

	t.Run("one_byte_over_limit", func(t *testing.T) {
		in := strings.Repeat("a", (1<<20)+1)
		body, err := readPlaylistBody(strings.NewReader(in))
		if !errors.Is(err, errPlaylistTooLarge) {
			t.Fatalf("expected errPlaylistTooLarge, got %v", err)
		}
		if body != nil {
			t.Fatalf("an overflowing body must not be returned, got %d bytes", len(body))
		}
	})

	t.Run("read_error_propagates", func(t *testing.T) {
		sentinel := errors.New("sentinel read failure")
		if _, err := readPlaylistBody(iotest.ErrReader(sentinel)); !errors.Is(err, sentinel) {
			t.Fatalf("expected the read error to propagate, got %v", err)
		}
	})
}

// TestLastPlaylistURI pins the backwards line scan directly. The scan walks the
// bytes rather than splitting them (so a hostile playlist's memory cost stays
// set by the body bound, not by how many newlines it contains), which makes its
// boundary handling worth testing on its own rather than only through Send.
func TestLastPlaylistURI(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"empty", "", ""},
		{"only_newlines", "\n\n\n", ""},
		{"only_tags", "#EXTM3U\n#EXT-X-VERSION:3\n", ""},
		{"single_line_no_newline", "low.m3u8", "low.m3u8"},
		{"no_trailing_newline", "#EXTM3U\nlow.m3u8", "low.m3u8"},
		{"trailing_newline", "#EXTM3U\nlow.m3u8\n", "low.m3u8"},
		{"trailing_blank_lines", "#EXTM3U\nlow.m3u8\n\n  \n\t\n", "low.m3u8"},
		{"crlf", "#EXTM3U\r\nlow.m3u8\r\n", "low.m3u8"},
		{"last_of_several", "a.m3u8\nb.m3u8\nc.m3u8\n", "c.m3u8"},
		{"tag_after_uri", "low.m3u8\n#EXT-X-ENDLIST\n", "low.m3u8"},
		{"leading_blank_line", "\n\nlow.m3u8\n", "low.m3u8"},
		{"surrounding_whitespace", "#EXTM3U\n   low.m3u8   \n", "low.m3u8"},
		{"uri_first_then_tags", "low.m3u8\n#A\n#B\n\n", "low.m3u8"},
	}

	for _, tc := range cases {
		if got := lastPlaylistURI([]byte(tc.body)); got != tc.want {
			t.Errorf("lastPlaylistURI(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

// TestLastPlaylistURIOnAPathologicalBody exercises the shape the body bound
// exists for: a body at the limit made almost entirely of newlines. The old
// split-based scan turned that into roughly sixteen times the body in slice
// headers; this only has to come back with the right answer without misbehaving.
func TestLastPlaylistURIOnAPathologicalBody(t *testing.T) {
	const want = "https://variant.test/low.m3u8"
	body := strings.Repeat("\n", (1<<20)-len(want)-1) + want + "\n"
	if len(body) != 1<<20 {
		t.Fatalf("fixture is %d bytes, want %d", len(body), 1<<20)
	}
	if got := lastPlaylistURI([]byte(body)); got != want {
		t.Fatalf("lastPlaylistURI on a newline-heavy body = %q, want %q", got, want)
	}
}

// TestRelativeURIResolvesAgainstTheRequestedURLWithoutARedirect covers the
// ordinary shape production actually meets: a bare relative URI line in a
// playlist that was NOT redirected. The redirect cases above pin which base is
// used when the two candidates differ; this pins the far more common case where
// there is only one.
func TestRelativeURIResolvesAgainstTheRequestedURLWithoutARedirect(t *testing.T) {
	master := c1MasterURL(c1Channel)

	t.Run("relative_media_playlist", func(t *testing.T) {
		// The master lives at /api/channel/hls/c1chan.m3u8, so a sibling
		// "low.m3u8" is /api/channel/hls/low.m3u8 on the same origin.
		const wantMedia = constants.UsherURL + "/api/channel/hls/low.m3u8"

		rt := newHLSRecordRT()
		rt.steps[master] = hlsStep{status: 200, body: c1Playlist("low.m3u8")}
		rt.steps[wantMedia] = hlsStep{status: 200, body: c1Playlist("https://seg.test/s.ts")}
		rt.steps["https://seg.test/s.ts"] = hlsStep{status: 200}

		sender, streamer := c1Sender(rt, c1Channel)
		if res := sender.Send(context.Background(), streamer); res.SimulateErr != nil {
			t.Fatalf("expected the relative chain to succeed, got %v", res.SimulateErr)
		}
		if got := rt.hitCount(wantMedia); got != 1 {
			t.Fatalf("expected %q to be requested once, got %d; all=%q", wantMedia, got, rt.allHits())
		}
	})

	t.Run("relative_segment", func(t *testing.T) {
		const media = "https://variant.test/v/low.m3u8"
		const wantSegment = "https://variant.test/v/s1.ts"

		rt := newHLSRecordRT()
		rt.steps[master] = hlsStep{status: 200, body: c1Playlist(media)}
		rt.steps[media] = hlsStep{status: 200, body: c1Playlist("s1.ts")}
		rt.steps[wantSegment] = hlsStep{status: 200}

		sender, streamer := c1Sender(rt, c1Channel)
		if res := sender.Send(context.Background(), streamer); res.SimulateErr != nil {
			t.Fatalf("expected the relative chain to succeed, got %v", res.SimulateErr)
		}
		if got := rt.hitCount(wantSegment); got != 1 {
			t.Fatalf("expected %q to be requested once, got %d; all=%q", wantSegment, got, rt.allHits())
		}
	})

	t.Run("root_relative_segment", func(t *testing.T) {
		const media = "https://variant.test/v/low.m3u8"
		const wantSegment = "https://variant.test/abs/s1.ts"

		rt := newHLSRecordRT()
		rt.steps[master] = hlsStep{status: 200, body: c1Playlist(media)}
		rt.steps[media] = hlsStep{status: 200, body: c1Playlist("/abs/s1.ts")}
		rt.steps[wantSegment] = hlsStep{status: 200}

		sender, streamer := c1Sender(rt, c1Channel)
		if res := sender.Send(context.Background(), streamer); res.SimulateErr != nil {
			t.Fatalf("expected the relative chain to succeed, got %v", res.SimulateErr)
		}
		if got := rt.hitCount(wantSegment); got != 1 {
			t.Fatalf("expected %q to be requested once, got %d; all=%q", wantSegment, got, rt.allHits())
		}
	})
}

// TestResolveHLSTargetRefusesEveryForbiddenFixture exercises the caller-side
// validation layer directly. Through Send it is masked: hlsDo re-validates, so
// removing the check here changes nothing observable. Calling it directly is
// what keeps that layer honest rather than decorative.
func TestResolveHLSTargetRefusesEveryForbiddenFixture(t *testing.T) {
	base, err := url.Parse(constants.UsherURL + "/api/channel/hls/c1chan.m3u8")
	if err != nil {
		t.Fatalf("base fixture did not parse: %v", err)
	}

	for _, tc := range c1ForbiddenTargets {
		t.Run(tc.name, func(t *testing.T) {
			for _, b := range []*url.URL{nil, base} {
				got, err := resolveHLSTarget(b, tc.url)
				if err == nil {
					t.Fatalf("resolveHLSTarget(base=%v, %q) admitted %v", b, tc.url, got)
				}
				if !errors.Is(err, errHLSURLRejected) {
					t.Fatalf("expected an errHLSURLRejected refusal, got %v", err)
				}
				if got != nil {
					t.Fatalf("a refused target must yield no URL, got %v", got)
				}
				assertNoSignedMaterial(t, "resolveHLSTarget refusal", err.Error())
			}
		})
	}

	t.Run("empty_reference", func(t *testing.T) {
		if _, err := resolveHLSTarget(base, ""); !errors.Is(err, errHLSURLRejected) {
			t.Fatalf("an empty URI line must be refused, got %v", err)
		}
	})
}

// TestValidateHLSURLNameRules covers the host-name rules at the shapes the
// behavioural matrix does not reach, and pins the admissions that mark this
// boundary's deliberate limits.
func TestValidateHLSURLNameRules(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		allowed bool
	}{
		{"bare_local_tld", "https://local/low.m3u8", false},
		{"local_tld_trailing_dot", "https://printer.local./low.m3u8", false},
		{"localhost_mixed_case_trailing_dot", "https://LocalHost./low.m3u8", false},
		{"underscore_in_label", "https://edge_1.cdn.example/low.m3u8", true},
		{"punycode_host", "https://xn--e1afmkfd.example/low.m3u8", true},
		{"hyphenated_label", "https://video-weaver.lhr03.hls.ttvnw.net/x", true},
		// Deliberate limits: deciding either of these needs name resolution,
		// which this boundary does not do.
		{"single_label_intranet", "https://intranet/low.m3u8", true},
		{"public_suffix_lookalike", "https://not-really-twitch.example/x", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.raw)
			if err != nil {
				t.Fatalf("fixture %q did not parse: %v", tc.raw, err)
			}
			err = validateHLSURL(u)
			if tc.allowed && err != nil {
				t.Fatalf("expected %q to be admitted, got %v", tc.raw, err)
			}
			if !tc.allowed && err == nil {
				t.Fatalf("expected %q to be refused", tc.raw)
			}
		})
	}
}
