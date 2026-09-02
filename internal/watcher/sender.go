package watcher

import (
	"context"
	"fmt"
	"io"
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
	// full beacon body to a third party. This override is local to a shallow
	// copy of the shared client, so playlist/variant/segment requests made
	// through s.httpClient elsewhere keep following redirects normally.
	beaconClient := *s.httpClient
	beaconClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}

	resp, err := beaconClient.Do(req)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return resp.StatusCode, false, fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	return resp.StatusCode, false, nil
}

// simulateWatching mimics a player fetching the stream: playlist, lowest quality
// variant, and a HEAD request on the newest segment. It returns the stage that
// failed and the HTTP status reached there (0 if the request itself failed before
// a response or the failure is a parse error), plus the raw error; on success it
// returns ("", 0, nil).
func (s *MinuteSender) simulateWatching(ctx context.Context, channel, sig, token string) (ProbeStage, int, error) {
	playlistURL := fmt.Sprintf("%s/api/channel/hls/%s.m3u8", constants.UsherURL, channel)

	params := url.Values{
		"sig":   {sig},
		"token": {token},
	}

	resp, err := s.httpGet(ctx, playlistURL+"?"+params.Encode())
	if err != nil {
		return StagePlaylist, 0, fmt.Errorf("failed to get playlist: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return StagePlaylist, resp.StatusCode, fmt.Errorf("playlist request failed with status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return StagePlaylist, resp.StatusCode, fmt.Errorf("failed to read playlist: %w", err)
	}

	lines := strings.Split(string(body), "\n")
	var lowestQualityURL string
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "http") {
			lowestQualityURL = line
			break
		}
	}

	if lowestQualityURL == "" {
		return StagePlaylist, 0, fmt.Errorf("no stream URL found in playlist")
	}

	streamListResp, err := s.httpGet(ctx, lowestQualityURL)
	if err != nil {
		return StagePlaylist, 0, fmt.Errorf("failed to get stream list: %w", err)
	}
	defer func() { _ = streamListResp.Body.Close() }()

	if streamListResp.StatusCode != http.StatusOK {
		return StagePlaylist, streamListResp.StatusCode, fmt.Errorf("stream list request failed with status %d", streamListResp.StatusCode)
	}

	streamListBody, err := io.ReadAll(streamListResp.Body)
	if err != nil {
		return StagePlaylist, streamListResp.StatusCode, fmt.Errorf("failed to read stream list: %w", err)
	}

	streamLines := strings.Split(string(streamListBody), "\n")
	var segmentURL string
	for i := len(streamLines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(streamLines[i])
		if strings.HasPrefix(line, "http") {
			segmentURL = line
			break
		}
	}

	if segmentURL == "" {
		return StageSegment, 0, fmt.Errorf("no segment URL found")
	}

	req, err := http.NewRequestWithContext(ctx, "HEAD", segmentURL, nil)
	if err != nil {
		return StageSegment, 0, fmt.Errorf("failed to create HEAD request: %w", err)
	}
	req.Header.Set("User-Agent", constants.TVUserAgent)

	headResp, err := s.httpClient.Do(req)
	if err != nil {
		return StageSegment, 0, fmt.Errorf("HEAD request failed: %w", err)
	}
	defer func() { _ = headResp.Body.Close() }()

	if headResp.StatusCode != http.StatusOK {
		return StageSegment, headResp.StatusCode, fmt.Errorf("HEAD request returned status %d", headResp.StatusCode)
	}

	return "", 0, nil
}

// httpGet issues a context-aware GET, so the playlist/variant fetches can be
// cancelled by a probe's context.
func (s *MinuteSender) httpGet(ctx context.Context, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, err
	}
	return s.httpClient.Do(req)
}
