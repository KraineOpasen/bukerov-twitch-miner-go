package watcher

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
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
// Satisfied by *api.TwitchClient.
type playbackTokenProvider interface {
	GetPlaybackAccessToken(username string) (sig, token string, err error)
}

type MinuteSender struct {
	client     playbackTokenProvider
	httpClient *http.Client
}

func NewMinuteSender(client *api.TwitchClient) *MinuteSender {
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
//   - Failure != nil: a fatal stage failed (session snapshot unusable, playback
//     token, or beacon rejected).
//
// SimulateErr is the best-effort playlist/segment error: informational only,
// never fatal for a production Send (it is surfaced for debug logging exactly as
// the old (simulateErr, err) return did).
type SendResult struct {
	Delivered   bool
	Stale       bool
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
// The steps run with context.Background(), exactly as the original inline
// http.NewRequest calls did.
func (s *MinuteSender) Send(streamer *models.Streamer) SendResult {
	session := streamer.Stream.SessionSnapshot()
	if !session.HasSpadeURL() {
		return SendResult{Failure: &WatchFailure{Stage: StageSessionSnapshot, ErrorCode: "no_spade_url"}}
	}
	if !session.HasPayload() {
		return SendResult{Failure: &WatchFailure{Stage: StageSessionSnapshot, ErrorCode: "no_payload"}}
	}

	login := streamer.GetUsername()
	sig, token, err := s.client.GetPlaybackAccessToken(login)
	if err != nil {
		return SendResult{Failure: &WatchFailure{Stage: StagePlaybackToken, ErrorCode: "playback_token_error"}}
	}

	_, _, simulateErr := s.simulateWatching(context.Background(), login, sig, token)

	status, stale, beaconErr := s.postBeacon(context.Background(), streamer, session)
	switch {
	case stale:
		return SendResult{Stale: true, SimulateErr: simulateErr}
	case beaconErr != nil:
		return SendResult{SimulateErr: simulateErr, Failure: &WatchFailure{Stage: StageBeacon, Status: status, ErrorCode: beaconErrorCode(status)}}
	default:
		return SendResult{Delivered: true, Generation: session.Generation, SimulateErr: simulateErr}
	}
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
// Send, every step is fatal (a probe wants to know the first thing that breaks)
// and cancellable via ctx. The streamer must already be brought online.
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
	sig, token, err := s.client.GetPlaybackAccessToken(login)
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

	resp, err := s.httpClient.Do(req)
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
