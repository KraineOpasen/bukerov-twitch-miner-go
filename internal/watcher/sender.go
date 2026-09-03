package watcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/twitch"
)

// MinuteSender performs a single "watch minute" for a streamer: it captures one
// coherent playback-session snapshot, fetches a playback access token, touches
// the HLS playlist/segment like a real player would, and posts the minute-watched
// beacon to the session's spade URL. It is the one mechanism that actually earns
// watch time, driven by the unified slot broker so every watch slot reports
// viewing identically.
//
// The same steps are also exposed, instrumented, via Probe for the health
// canary — there is no second beacon implementation.
//
// Session coherence: both Send and Probe capture ONE PlaybackSessionSnapshot at
// the start and use its spade URL AND payload together, then re-check the session
// generation immediately before the beacon. The spade URL and payload are never
// read as two separate, independently-racing calls, so an old broadcast's payload
// can never be posted to a newer broadcast's spade URL; if the session changed
// mid-send the beacon is suppressed as StageStaleSession instead.
//
// playbackTokenProvider is the slice of the Twitch client the sender needs;
// narrowed to an interface so Probe can be tested without a real client.
// Satisfied by *twitch.TwitchClient.
type playbackTokenProvider interface {
	GetPlaybackAccessToken(ctx context.Context, username string) (sig, token string, err error)
}

type MinuteSender struct {
	client     playbackTokenProvider
	httpClient *http.Client
}

func NewMinuteSender(client *twitch.TwitchClient) *MinuteSender {
	return &MinuteSender{
		client:     client,
		httpClient: &http.Client{Timeout: 20 * time.Second},
	}
}

// ProbeStage names the watch-transport step a send or probe reached. These are
// stable, redacted identifiers surfaced (never with a raw URL/token) in the
// Health Center and in recovery signatures.
type ProbeStage string

const (
	// StageSessionSnapshot: the captured session was unusable (no spade URL or no
	// payload — the channel was not brought online) before any network I/O.
	StageSessionSnapshot ProbeStage = "session_snapshot"
	StagePlaybackToken   ProbeStage = "playback_token"
	StagePlaylist        ProbeStage = "playlist"
	StageSegment         ProbeStage = "segment"
	StageBeacon          ProbeStage = "beacon"
	// StageStaleSession: the playback session changed (new broadcast, completed
	// refresh) between snapshot capture and the beacon, so the beacon was
	// suppressed. This is NOT a transport failure and NOT an authoritative offline.
	StageStaleSession ProbeStage = "stale_session"
)

// WatchFailure is the redacted outcome of a fatal send/probe stage. It carries
// only the stage, the HTTP status at a failing request (0 if none), and a stable
// bounded error code — never the raw error, the signed playback URL (which embeds
// sig/token), the spade URL, the payload, or any header.
type WatchFailure struct {
	Stage     ProbeStage
	Status    int
	ErrorCode string
}

// SendResult is the typed outcome of one production minute-watched send. Exactly
// one operative outcome holds:
//   - Delivered: the beacon was accepted against Generation (a watched minute
//     counts).
//   - Stale: the playback session changed between snapshot capture and the beacon
//     (new broadcast or completed refresh); the beacon was NOT sent. This is
//     neither an authoritative offline nor a Twitch transport failure — the loop
//     simply retries next tick against the new session.
//   - Cancelled: the caller's context (the watch generation) was cancelled
//     during the send. The beacon may or may not have been attempted, but the
//     outcome is NOT a Twitch transport failure and NOT an offline signal — the
//     generation is being torn down, so it is classified separately instead of
//     being laundered into a StageBeacon failure that would trigger a fresh
//     online check on a dying generation.
//   - Failure != nil: a fatal stage failed (session snapshot unusable, playback
//     token, or beacon rejected).
//
// SimulateErr is the best-effort playlist/segment outcome: informational only,
// never fatal for a production Send. It is REDACTED at construction (stage and
// HTTP status only, see simulateFailure) because the raw transport error wraps
// the signed usher URL, which embeds the playback sig and token — and this value
// is logged. It is deliberately left nil on a Cancelled result: an aborted
// request carries no information about Twitch, and reporting one would log a
// simulate failure for every teardown.
type SendResult struct {
	Delivered   bool
	Stale       bool
	Cancelled   bool
	Generation  uint64
	SimulateErr error
	Failure     *WatchFailure
}

// Send reports one watched minute for the streamer. The streamer must have been
// brought online first (a coherent session snapshot with a spade URL + payload).
// Control flow preserves the historical contract: session snapshot (fatal) ->
// playback token (fatal) -> playlist/segment simulation (best-effort,
// informational) -> generation re-check + spade beacon (fatal), with a session
// that changed mid-send reported as a non-fatal Stale outcome instead of a beacon.
//
// Every step runs on ctx — the watch generation's context. A cancelled
// generation therefore aborts the playback-token request, the three HLS
// requests and the beacon POST instead of riding each one's 20-30s transport
// timeout to completion, and cannot newly start a beacon it no longer owns.
func (s *MinuteSender) Send(ctx context.Context, streamer *models.Streamer) SendResult {
	session := streamer.Stream.SessionSnapshot()
	if !session.HasSpadeURL() || !session.HasPayload() {
		// Ownership is re-checked at the point of reporting, exactly as the
		// playback-token and beacon branches do. These two gates do no I/O, so
		// without the check a teardown landing on a slot whose session has not
		// converged would surface as a StageSessionSnapshot transport failure —
		// fabricating a failure statistic and sending the broker off to re-check
		// a dying generation's channel.
		if cancelled(ctx) {
			return SendResult{Cancelled: true}
		}
		if !session.HasSpadeURL() {
			return SendResult{Failure: &WatchFailure{Stage: StageSessionSnapshot, ErrorCode: "no_spade_url"}}
		}
		return SendResult{Failure: &WatchFailure{Stage: StageSessionSnapshot, ErrorCode: "no_payload"}}
	}

	login := streamer.GetUsername()
	sig, token, err := s.client.GetPlaybackAccessToken(ctx, login)
	if err != nil {
		if cancelled(ctx) {
			return SendResult{Cancelled: true}
		}
		return SendResult{Failure: &WatchFailure{Stage: StagePlaybackToken, ErrorCode: "playback_token_error"}}
	}

	simStage, simStatus, simErr := s.simulateWatching(ctx, login, sig, token)
	simulateErr := redactSimulateErr(simStage, simStatus, simErr)

	// A cancelled generation must not NEWLY START the beacon. The HLS stages
	// above are deliberately non-fatal, so without this gate a send whose
	// playlist fetch was aborted by cancellation would still go on to POST a
	// minute-watched event — and whether that POST were rejected would depend
	// on the transport noticing the dead context, not on ownership.
	if ctx.Err() != nil {
		// No SimulateErr: an aborted request carries no information about Twitch,
		// and reporting one would put a "Failed to simulate watching" line in the
		// log for every teardown — the same misreading res.Cancelled exists to
		// prevent in the statistics.
		return SendResult{Cancelled: true}
	}

	status, stale, beaconErr := s.postBeacon(ctx, streamer, session)
	switch {
	case stale:
		return SendResult{Stale: true, SimulateErr: simulateErr}
	case beaconErr != nil:
		// Classify before reporting: a beacon that failed because the watch
		// generation was cancelled is a teardown, not a Twitch transport
		// failure. Suppressing the distinction would fabricate a failure
		// statistic and send the broker off to re-check the channel's online
		// state on a generation that no longer exists.
		if cancelled(ctx) {
			return SendResult{Cancelled: true}
		}
		return SendResult{SimulateErr: simulateErr, Failure: &WatchFailure{Stage: StageBeacon, Status: status, ErrorCode: beaconErrorCode(status)}}
	default:
		return SendResult{Delivered: true, Generation: session.Generation, SimulateErr: simulateErr}
	}
}

// cancelled reports whether a step failed because the OWNER's context ended,
// rather than because Twitch rejected the request.
//
// It deliberately keys on the owner's context alone and NOT on the error chain:
// a context.DeadlineExceeded arriving from Twitch (or from a transport's own
// per-request budget) while the watch generation is still alive is a genuine
// transport failure and must keep being reported as one. Only the owner ending
// makes an outcome a teardown. Cancellation is monotonic, so an owner-aborted
// request always finds ctx.Err() non-nil here.
func cancelled(ctx context.Context) bool {
	return ctx.Err() != nil
}

// ProbeResult is the redacted outcome of a watch-transport probe. It carries only
// the stage reached, the HTTP status at a failing request (0 if none), a stable
// error code, and how long the probe took — never the raw error, the signed
// playback URL, the spade URL, or any header.
type ProbeResult struct {
	OK        bool
	Stage     ProbeStage
	Status    int
	ErrorCode string
	Duration  time.Duration
}

// Probe runs the exact watch-transport sequence Send uses — session snapshot ->
// playback token -> playlist/lowest-variant/segment -> generation re-check + spade
// beacon — but stage-instrumented and redacted, for the health canary. Unlike
// Send, every step is fatal (a probe wants to know the first thing that breaks);
// both run entirely on their caller's ctx. The streamer must already be brought
// online.
func (s *MinuteSender) Probe(ctx context.Context, streamer *models.Streamer) ProbeResult {
	start := time.Now()
	elapsed := func() time.Duration { return time.Since(start) }

	session := streamer.Stream.SessionSnapshot()
	if !session.HasSpadeURL() {
		return probeFail(StageSessionSnapshot, 0, elapsed())
	}
	if !session.HasPayload() {
		return probeFail(StageSessionSnapshot, 0, elapsed())
	}

	login := streamer.GetUsername()
	sig, token, err := s.client.GetPlaybackAccessToken(ctx, login)
	if err != nil {
		return probeFail(StagePlaybackToken, 0, elapsed())
	}

	if stage, status, err := s.simulateWatching(ctx, login, sig, token); err != nil {
		return probeFail(stage, status, elapsed())
	}

	status, stale, err := s.postBeacon(ctx, streamer, session)
	if stale {
		return probeFail(StageStaleSession, 0, elapsed())
	}
	if err != nil {
		return probeFail(StageBeacon, status, elapsed())
	}

	return ProbeResult{OK: true, Duration: elapsed()}
}

// simulateFailure is the redacted best-effort playlist/segment outcome: the
// stage reached and the HTTP status there, never the raw transport error. The
// raw error wraps the signed usher playback URL (sig + token in the query), and
// SendResult.SimulateErr is written to the debug log, so the raw form must never
// reach a SendResult, a ProbeResult or a log line: every caller of
// simulateWatching redacts it at the call site (redactSimulateErr here,
// probeFail on the probe path). Mirrors the redaction WatchFailure and
// ProbeResult already apply.
type simulateFailure struct {
	Stage  ProbeStage
	Status int
}

func (e *simulateFailure) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("watch simulation failed at stage %s (status %d)", e.Stage, e.Status)
	}
	return fmt.Sprintf("watch simulation failed at stage %s", e.Stage)
}

// redactSimulateErr converts simulateWatching's (stage, status, err) triple into
// the redacted informational error Send reports. nil in, nil out.
func redactSimulateErr(stage ProbeStage, status int, err error) error {
	if err == nil {
		return nil
	}
	return &simulateFailure{Stage: stage, Status: status}
}

// probeFail builds a redacted failure result. The error code is derived only from
// the stage and HTTP status (both safe to expose), never the raw error.
func probeFail(stage ProbeStage, status int, dur time.Duration) ProbeResult {
	code := string(stage) + "_error"
	if status > 0 {
		code = fmt.Sprintf("%s_http_%d", stage, status)
	}
	return ProbeResult{Stage: stage, Status: status, ErrorCode: code, Duration: dur}
}

// beaconErrorCode derives a stable bounded error code for a beacon failure from
// the HTTP status only (0 before any response).
func beaconErrorCode(status int) string {
	if status > 0 {
		return fmt.Sprintf("beacon_http_%d", status)
	}
	return "beacon_error"
}

// spadeFormBody wraps the base64 minute-watched payload into the
// application/x-www-form-urlencoded body the spade endpoint expects. The value
// must be percent-encoded: standard base64 can contain '+', which a form parser
// would otherwise decode as a space and corrupt the event. This mirrors the
// reference python miner (which posts data={"data": b64} via requests) and the
// real web player (btoa + encodeURIComponent).
func spadeFormBody(payload string) string {
	return url.Values{"data": {payload}}.Encode()
}

// postBeacon posts the minute-watched event to the captured session's spade URL,
// using the SAME snapshot's payload — the spade URL and payload are never read as
// two separate racing calls. Immediately before sending it re-checks the live
// session generation against the captured one; a change (new broadcast, completed
// refresh) means the session moved underneath us, so it reports stale=true and
// sends nothing rather than posting an old payload to a newer session. Returns the
// HTTP status reached (0 before any response), whether the send was skipped as
// stale, and the raw error (redacted by the caller).
func (s *MinuteSender) postBeacon(ctx context.Context, streamer *models.Streamer, session models.PlaybackSessionSnapshot) (int, bool, error) {
	payload, err := session.EncodePayload()
	if err != nil {
		return 0, false, fmt.Errorf("failed to encode payload: %w", err)
	}

	// Coherence gate: the session must not have changed since it was captured.
	// This is the single point where the whole send is committed to one
	// observation of the watch session.
	if streamer.Stream.SessionGeneration() != session.Generation {
		return 0, true, nil
	}

	req, err := http.NewRequestWithContext(ctx, "POST", session.SpadeURL, strings.NewReader(spadeFormBody(payload)))
	if err != nil {
		return 0, false, err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", constants.TVUserAgent)

	// The beacon POST must never follow a redirect: a redirected target could
	// be cross-origin, downgrade https to http, or (for 307/308) replay the
	// full beacon body to a third party. The HLS leg is deliberately different
	// — it still follows redirects, under its own validating policy in hlsDo —
	// so the two must not be unified. Both install their policy on a shallow
	// per-request copy, leaving the shared s.httpClient untouched.
	beaconClient := *s.httpClient
	beaconClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}

	resp, err := beaconClient.Do(req)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = resp.Body.Close() }()

	// ONLY 204 No Content is a credited minute-watched beacon. Both current
	// reference implementations require exactly that (DevilXD/TwitchDropsMiner @
	// 65d1092 channel.py send_watch: `return response.status == 204`;
	// INKCR0W/TwitchDropsMinerGo @ 7ee5387 internal/watch/watch.go:
	// `response.StatusCode == http.StatusNoContent`).
	//
	// Accepting HTTP 200 as success is what made an uncredited beacon
	// indistinguishable from a credited one: Twitch answers a stale or malformed
	// minute-watched payload at the transport layer without counting the watch, so
	// a 200 was being laundered into Delivered — local watched minutes, slot
	// delivery_success and watch-time fairness credit for a minute Twitch never
	// granted. Every non-204 status now returns the existing bounded
	// StageBeacon/status/beacon_http_<status> failure instead.
	if resp.StatusCode != http.StatusNoContent {
		// Drain (bounded) before closing: a rejected beacon is now a once-a-minute
		// path, and an undrained body forces the transport to discard the
		// connection instead of reusing it. The content is never inspected.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBeaconDrainBytes))
		return resp.StatusCode, false, fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	return resp.StatusCode, false, nil
}

// --- C1: the HLS URL / redirect trust boundary -----------------------------
//
// Everything the HLS simulation dereferences after the master request is chosen
// by a remote party: the master playlist body names the rendition, the rendition
// body names the segment, and a Location header can redirect any of the three.
// The requests also carry the playback sig and token in their query, so where
// they are sent, and what is forwarded with them, is a trust decision — not a
// transport detail.
//
// The policy below is URL-layer only and is applied BEFORE any I/O at every one
// of those points. It deliberately does NOT resolve names or reason about the
// address actually connected to: hostname-to-address policy (private ranges,
// rebinding, the peer a dialer really reached) is a separate concern with a
// separate mechanism, and pretending a URL check covers it would be worse than
// not claiming it at all.

// maxBeaconDrainBytes bounds how much of a rejected beacon response is drained
// before the body is closed, so a remote party answering with an endless body
// cannot make the miner read forever.
const maxBeaconDrainBytes = 1 << 16 // 64 KiB

const (
	// maxPlaylistBytes bounds a playlist body. Both playlists this code reads
	// are small text documents; a remote party that answers with an endless
	// body would otherwise be allocating memory inside the miner.
	maxPlaylistBytes = 1 << 20 // 1 MiB

	// maxHLSRedirects mirrors net/http's own defaultCheckRedirect ceiling.
	// Installing a CheckRedirect REPLACES that default rather than extending
	// it, so the ceiling has to be restated or it would silently disappear.
	// Like the default, it bounds the number of ACTUAL requests per Do at ten:
	// len(via) is the number already made when the next hop is offered.
	maxHLSRedirects = 10
)

// errHLSURLRejected is the sentinel behind every URL-policy refusal. The reason
// wrapped with it is a short, bounded, low-cardinality token — never the URL,
// the host, the query, or a Location value.
//
// Today nothing forces that: Send and Probe both discard this error and report
// only (stage, status). It is written this way so the safety of the value does
// not DEPEND on that discipline — a future caller that logs what simulateWatching
// returns should find nothing worth redacting.
var errHLSURLRejected = errors.New("hls target rejected")

var (
	errPlaylistTooLarge    = errors.New("hls playlist body exceeds the size bound")
	errTooManyHLSRedirects = errors.New("hls request exceeded the redirect ceiling")
)

func rejectHLSURL(reason string) error {
	return fmt.Errorf("%w: %s", errHLSURLRejected, reason)
}

// validateHLSURL applies the watch URL policy to one absolute request target.
//
// Admissible: https, on the default port or an explicit :443, to an ASCII
// registered name. Cross-origin is fine — Twitch hands the player from usher to
// whichever CDN edge serves the channel, and those are different hosts — so
// there is deliberately no same-origin rule and no domain allowlist.
//
// Refused: any other scheme, any other explicit port, userinfo, a fragment, an
// empty host, an IP literal (v4 or v6, zone identifiers included), localhost,
// .localhost, .local, a host that is really a number in disguise, a host with a
// character no registered name may carry, and anything that failed to parse.
func validateHLSURL(u *url.URL) error {
	if u == nil {
		return rejectHLSURL("no_url")
	}
	if u.Opaque != "" {
		return rejectHLSURL("opaque_url")
	}
	if u.Scheme != "https" {
		return rejectHLSURL("scheme_not_https")
	}
	if u.User != nil {
		return rejectHLSURL("userinfo")
	}
	if u.Fragment != "" || u.RawFragment != "" {
		return rejectHLSURL("fragment")
	}
	// A bracketed authority is the IP-literal form. Refuse it on the brackets
	// themselves rather than on what is inside them: url.Parse only insists the
	// contents really are an IPv6 address for modules declaring go1.26 or later
	// (this one does), a caller can hand this function a URL value it built
	// itself, and Hostname() unwraps the brackets — so "[not-an-ip]" would
	// otherwise arrive here looking like an ordinary registered name. It is also
	// the only form that carries an IPv6 zone identifier.
	if strings.ContainsAny(u.Host, "[]") {
		return rejectHLSURL("ip_literal")
	}
	switch u.Port() {
	case "", "443":
	default:
		return rejectHLSURL("port_not_443")
	}
	// url.Parse accepts "https://host:/path": Port() is empty, yet the
	// authority still carries a colon. Fail closed rather than guess.
	if u.Port() == "" && strings.Contains(u.Host, ":") {
		return rejectHLSURL("malformed_authority")
	}

	host := u.Hostname()
	if net.ParseIP(host) != nil {
		return rejectHLSURL("ip_literal")
	}
	for i := 0; i < len(host); i++ {
		switch c := host[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '.', c == '_':
		case c > 0x7f:
			return rejectHLSURL("non_ascii_host")
		default:
			// Anything else — ':', '%', '/', a space, a control byte. url.Parse
			// already refuses most of these in a host, so this branch mostly
			// catches a URL value a caller assembled rather than parsed; it is
			// the floor, not the first line.
			return rejectHLSURL("invalid_host_char")
		}
	}

	// One trailing dot is the DNS root label. Strip it before the name checks
	// below so "localhost." cannot walk past them, and lower-case the name
	// because url.Parse normalises the scheme but never the host. This is also
	// the single owner of the empty-host rule: it catches both a missing
	// authority and a host that is nothing but the root label.
	name := strings.ToLower(strings.TrimSuffix(host, "."))
	if name == "" {
		return rejectHLSURL("empty_host")
	}
	labels := strings.Split(name, ".")
	for _, label := range labels {
		if label == "" {
			return rejectHLSURL("empty_label")
		}
	}
	if name == "localhost" || strings.HasSuffix(name, ".localhost") {
		return rejectHLSURL("localhost")
	}
	if name == "local" || strings.HasSuffix(name, ".local") {
		return rejectHLSURL("mdns_local")
	}
	if isNumericHostLabel(labels[len(labels)-1]) {
		return rejectHLSURL("numeric_host")
	}
	return nil
}

// isNumericHostLabel reports whether a host's rightmost label is a number, in
// any of the spellings that make a name an alias for an address: decimal
// ("2130706433"), octal by leading zero ("0177.0.0.1" -> last label "1"), or
// hexadecimal ("0x7f000001"). net.ParseIP alone is not enough — it rejects every
// one of those strings — so a host ending in a number is refused outright rather
// than resolved and second-guessed.
func isNumericHostLabel(label string) bool {
	if label == "" {
		return false
	}
	if len(label) >= 2 && label[0] == '0' && (label[1] == 'x' || label[1] == 'X') {
		for i := 2; i < len(label); i++ {
			if !isHexDigit(label[i]) {
				return false
			}
		}
		return true
	}
	for i := 0; i < len(label); i++ {
		if label[i] < '0' || label[i] > '9' {
			return false
		}
	}
	return true
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// resolveHLSTarget turns one HLS URI into an absolute target that has passed the
// policy: the master URL this code builds, a bare URI line from a playlist body,
// or a redirect target. base is the FINAL response URL of the playlist that
// named ref (nil when ref is absolute by construction).
//
// Only the two roles production already dereferences take a relative form — a
// master playlist naming its rendition, a rendition naming its segment — and
// resolving them against the final URL, not the requested one, is what makes a
// relative line mean the same thing to this code as to a player: after a
// redirect, "low.m3u8" is a sibling of where the playlist actually came from.
//
// A literal '#' is refused on the raw string rather than after parsing: an empty
// fragment ("...#") leaves both URL.Fragment and URL.RawFragment empty, so the
// parsed check alone cannot see it. Refusing fragments buys no security property
// by itself — a fragment is never put on the wire — it keeps the URL this code
// approves identical to the URL it sends, so no later reader has to work out
// which parts of a target were only decorative.
func resolveHLSTarget(base *url.URL, ref string) (*url.URL, error) {
	if ref == "" {
		return nil, rejectHLSURL("empty_uri")
	}
	if strings.Contains(ref, "#") {
		return nil, rejectHLSURL("fragment")
	}

	var (
		u   *url.URL
		err error
	)
	if base == nil {
		u, err = url.Parse(ref)
	} else {
		u, err = base.Parse(ref)
	}
	if err != nil {
		// The parse error text quotes the offending URL, which on this path can
		// carry the signed query. Only the reason survives.
		return nil, rejectHLSURL("unparseable")
	}
	if err := validateHLSURL(u); err != nil {
		return nil, err
	}
	return u, nil
}

// hlsDo issues one policy-governed HLS request and returns the response together
// with the FINAL request URL — the base a relative URI in the returned body must
// be resolved against.
//
// Redirects stay allowed, because the real playback path relies on them, but
// every hop is validated before it is contacted, the Referer Go would synthesise
// from the previous (signed) URL is stripped, and the ten-request ceiling is
// restated because installing CheckRedirect replaces Go's default.
//
// The policy goes on a per-request SHALLOW COPY of the shared client, exactly as
// postBeacon does: the injected Transport, Timeout and Jar are preserved, the
// caller's context still owns cancellation at every hop (net/http carries the
// original request's ctx onto each redirect), s.httpClient is never mutated, and
// concurrent sends through one MinuteSender cannot race on the field.
//
// One invariant is worth stating because url.URL cannot express it: an empty
// fragment ("https://h/p#") leaves both Fragment and RawFragment empty, so
// validateHLSURL alone cannot see it. Every target reaching here has already
// been through the raw-string check in resolveHLSTarget or in the redirect
// closure below, which is where that case is caught.
func (s *MinuteSender) hlsDo(ctx context.Context, method string, target *url.URL, extra http.Header) (*http.Response, *url.URL, error) {
	if err := validateHLSURL(target); err != nil {
		return nil, nil, err
	}

	req, err := http.NewRequestWithContext(ctx, method, target.String(), nil)
	if err != nil {
		return nil, nil, rejectHLSURL("unbuildable_request")
	}
	for key, values := range extra {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	// final is the URL actually requested, taken from the request rather than
	// from the argument, so the base a relative URI is later resolved against is
	// exact by construction rather than by an argument round-tripping through
	// String() and back. It is then written only from inside CheckRedirect,
	// which net/http invokes synchronously on the goroutine driving this single
	// Do; the closure and the client copy are per request, so there is no state
	// here to share or race.
	final := req.URL

	client := *s.httpClient
	client.CheckRedirect = func(hop *http.Request, via []*http.Request) error {
		// Go builds the Referer from the previous request's URL, query and all
		// (refererForURL strips userinfo, never the query) — and on this path
		// that query is the playback sig and token. Strip it first, so no early
		// return below can leave it in place.
		hop.Header.Del("Referer")

		if len(via) >= maxHLSRedirects {
			return errTooManyHLSRedirects
		}
		// net/http has already resolved the Location header against the previous
		// URL, but it drops an empty fragment ("...#") while doing so, leaving
		// both URL.Fragment and URL.RawFragment empty. Check the raw header too,
		// so "reject fragments" means the same thing at a redirect as it does for
		// a playlist's URI line. net/http always sets Response on a redirect
		// request, so the nil guard is defensive: were it ever nil, the parsed
		// check below would still catch every fragment that changes the request,
		// leaving only the inert empty one.
		if hop.Response != nil && strings.Contains(hop.Response.Header.Get("Location"), "#") {
			return rejectHLSURL("fragment")
		}
		if err := validateHLSURL(hop.URL); err != nil {
			return err
		}
		final = hop.URL
		return nil
	}

	resp, err := client.Do(req)
	if err != nil {
		// When CheckRedirect refuses, Do returns the previous response next to
		// the error with its body already closed, so there is nothing here to
		// release. The error itself quotes a URL and is redacted by the callers
		// of simulateWatching, exactly as every other transport error is.
		return nil, nil, err
	}
	return resp, final, nil
}

// readPlaylistBody reads a playlist body under the size bound. It reads one byte
// past the bound so a body exactly at the limit is still accepted and anything
// larger is distinguishable — and refused without ever being held in full.
func readPlaylistBody(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxPlaylistBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxPlaylistBytes {
		return nil, errPlaylistTooLarge
	}
	return body, nil
}

// lastPlaylistURI returns a playlist's last URI line: the last non-blank line
// that is not a tag or comment. Twitch orders a master playlist best-quality
// first, so the last URI line is the lowest-bandwidth rendition — the one this
// simulation has always picked. The line may be absolute or relative; the caller
// resolves and validates it.
//
// It walks the bytes backwards instead of splitting them, so the only string it
// allocates is the line it returns. Splitting would undo the point of the body
// bound: a bounded 1 MiB body made entirely of newlines becomes roughly 16 MiB
// of slice headers, and the memory a hostile playlist costs would once again be
// set by its contents rather than by its size.
func lastPlaylistURI(body []byte) string {
	for end := len(body); end > 0; {
		start := bytes.LastIndexByte(body[:end], '\n') + 1
		line := strings.TrimSpace(string(body[start:end]))
		end = start - 1
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return line
	}
	return ""
}

// simulateWatching mimics a player fetching the stream: master playlist, lowest
// quality rendition, and a HEAD request on the newest segment. Each of the three
// targets — and every redirect between them — passes the watch URL policy before
// it is contacted. It returns the stage that failed and the HTTP status reached
// there (0 if the request failed before a response, was refused before it was
// sent, or the failure is a parse error), plus the raw error, which every caller
// redacts; on success it returns ("", 0, nil).
func (s *MinuteSender) simulateWatching(ctx context.Context, channel, sig, token string) (ProbeStage, int, error) {
	params := url.Values{
		"sig":   {sig},
		"token": {token},
	}
	// The master target is the only one this code builds itself, but it still
	// embeds a login this process did not choose, so it is admitted through the
	// same gate as every remote-derived target.
	master, err := resolveHLSTarget(nil, fmt.Sprintf("%s/api/channel/hls/%s.m3u8?%s", constants.UsherURL, channel, params.Encode()))
	if err != nil {
		return StagePlaylist, 0, fmt.Errorf("playlist url refused: %w", err)
	}

	resp, masterFinal, err := s.hlsDo(ctx, http.MethodGet, master, nil)
	if err != nil {
		return StagePlaylist, 0, fmt.Errorf("failed to get playlist: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return StagePlaylist, resp.StatusCode, fmt.Errorf("playlist request failed with status %d", resp.StatusCode)
	}

	body, err := readPlaylistBody(resp.Body)
	if err != nil {
		return StagePlaylist, resp.StatusCode, fmt.Errorf("failed to read playlist: %w", err)
	}

	lowestQualityLine := lastPlaylistURI(body)
	if lowestQualityLine == "" {
		return StagePlaylist, 0, fmt.Errorf("no stream URL found in playlist")
	}
	lowestQualityURL, err := resolveHLSTarget(masterFinal, lowestQualityLine)
	if err != nil {
		return StagePlaylist, 0, fmt.Errorf("stream list url refused: %w", err)
	}

	streamListResp, streamListFinal, err := s.hlsDo(ctx, http.MethodGet, lowestQualityURL, nil)
	if err != nil {
		return StagePlaylist, 0, fmt.Errorf("failed to get stream list: %w", err)
	}
	defer func() { _ = streamListResp.Body.Close() }()

	if streamListResp.StatusCode != http.StatusOK {
		return StagePlaylist, streamListResp.StatusCode, fmt.Errorf("stream list request failed with status %d", streamListResp.StatusCode)
	}

	streamListBody, err := readPlaylistBody(streamListResp.Body)
	if err != nil {
		return StagePlaylist, streamListResp.StatusCode, fmt.Errorf("failed to read stream list: %w", err)
	}

	segmentLine := lastPlaylistURI(streamListBody)
	if segmentLine == "" {
		return StageSegment, 0, fmt.Errorf("no segment URL found")
	}
	segmentURL, err := resolveHLSTarget(streamListFinal, segmentLine)
	if err != nil {
		return StageSegment, 0, fmt.Errorf("segment url refused: %w", err)
	}

	// The segment response body is deliberately not bounded the way the two
	// playlists are: this is a HEAD, production never reads the body, and the
	// bound exists to stop a body being read, not to describe one.
	headResp, _, err := s.hlsDo(ctx, http.MethodHead, segmentURL, http.Header{"User-Agent": {constants.TVUserAgent}})
	if err != nil {
		return StageSegment, 0, fmt.Errorf("HEAD request failed: %w", err)
	}
	defer func() { _ = headResp.Body.Close() }()

	if headResp.StatusCode != http.StatusOK {
		return StageSegment, headResp.StatusCode, fmt.Errorf("HEAD request returned status %d", headResp.StatusCode)
	}

	return "", 0, nil
}
