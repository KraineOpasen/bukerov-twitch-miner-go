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

// MinuteSender performs a single "watch minute" for a streamer: it fetches a
// playback access token, touches the HLS playlist/segment like a real player
// would, and posts the minute-watched event to the streamer's spade URL. It is
// the one mechanism that actually earns watch time, driven by the unified slot
// broker so every watch slot reports viewing identically.
//
// The same steps are also exposed, instrumented, via Probe for the health
// canary — there is no second beacon implementation.
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

// Send reports one watched minute for the streamer. The streamer must have
// been brought online first (spade URL + stream payload populated, e.g. via
// TwitchClient.CheckStreamerOnline). The playlist simulation is best-effort:
// its failure is returned separately via simulateErr for optional debug
// logging, while a failure to post the spade event fails the whole call.
//
// Control flow is unchanged from before the step extraction: playback token
// (fatal) -> playlist/segment simulation (best-effort, informational) -> spade
// POST (fatal). The steps run with context.Background(), exactly as the
// original inline http.NewRequest calls did (NewRequest uses Background too).
func (s *MinuteSender) Send(streamer *models.Streamer) (simulateErr error, err error) {
	sig, token, err := s.client.GetPlaybackAccessToken(streamer.Username)
	if err != nil {
		return nil, fmt.Errorf("failed to get playback token: %w", err)
	}

	_, _, simulateErr = s.simulateWatching(context.Background(), streamer.Username, sig, token)

	_, err = s.postBeacon(context.Background(), streamer)
	return simulateErr, err
}

// ProbeStage names the watch-transport step a canary probe reached. These are
// stable identifiers surfaced (redacted) in the Health Center.
type ProbeStage string

const (
	StagePlaybackToken ProbeStage = "playback_token"
	StagePlaylist      ProbeStage = "playlist"
	StageSegment       ProbeStage = "segment"
	StageBeacon        ProbeStage = "beacon"
)

// ProbeResult is the redacted outcome of a watch-transport probe. It carries
// only the stage reached, the HTTP status at a failing request (0 if none), a
// stable error code, and how long the probe took — never the raw error, the
// signed playback URL (which embeds sig/token), or any header.
type ProbeResult struct {
	OK        bool
	Stage     ProbeStage
	Status    int
	ErrorCode string
	Duration  time.Duration
}

// Probe runs the exact watch-transport sequence Send uses — playback token ->
// playlist/lowest-variant/segment -> spade beacon POST — but stage-instrumented
// and redacted, for the health canary. Unlike Send, every step is fatal (a
// probe wants to know the first thing that breaks) and cancellable via ctx.
// The streamer must already be brought online (spade URL populated).
func (s *MinuteSender) Probe(ctx context.Context, streamer *models.Streamer) ProbeResult {
	start := time.Now()
	elapsed := func() time.Duration { return time.Since(start) }

	sig, token, err := s.client.GetPlaybackAccessToken(streamer.Username)
	if err != nil {
		return probeFail(StagePlaybackToken, 0, elapsed())
	}

	if stage, status, err := s.simulateWatching(ctx, streamer.Username, sig, token); err != nil {
		return probeFail(stage, status, elapsed())
	}

	if status, err := s.postBeacon(ctx, streamer); err != nil {
		return probeFail(StageBeacon, status, elapsed())
	}

	return ProbeResult{OK: true, Duration: elapsed()}
}

// probeFail builds a redacted failure result. The error code is derived only
// from the stage and HTTP status (both safe to expose), never the raw error.
func probeFail(stage ProbeStage, status int, dur time.Duration) ProbeResult {
	code := string(stage) + "_error"
	if status > 0 {
		code = fmt.Sprintf("%s_http_%d", stage, status)
	}
	return ProbeResult{Stage: stage, Status: status, ErrorCode: code, Duration: dur}
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

// postBeacon posts the minute-watched event to the streamer's spade URL and
// reports the HTTP status reached (0 before any response) plus the raw error.
// Extracted verbatim from Send's former inline body — same order, same error
// messages, same accepted statuses (204/200) — so Send's behavior is unchanged.
func (s *MinuteSender) postBeacon(ctx context.Context, streamer *models.Streamer) (int, error) {
	spadeURL := streamer.Stream.GetSpadeURL()
	if spadeURL == "" {
		return 0, fmt.Errorf("no spade URL")
	}

	payload, err := streamer.Stream.EncodePayload()
	if err != nil {
		return 0, fmt.Errorf("failed to encode payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", spadeURL, strings.NewReader(spadeFormBody(payload)))
	if err != nil {
		return 0, err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", constants.TVUserAgent)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	return resp.StatusCode, nil
}

// simulateWatching mimics a player fetching the stream: playlist, lowest
// quality variant, and a HEAD request on the newest segment. It returns the
// stage that failed and the HTTP status reached there (0 if the request itself
// failed before a response or the failure is a parse error), plus the raw
// error; on success it returns ("", 0, nil). The request/parse logic is
// unchanged from the original single-error version — only the return shape (a
// stage + status for the canary) and context propagation were added.
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

// httpGet issues a context-aware GET, replacing the former http.Client.Get so
// the playlist/variant fetches can be cancelled by a probe's context.
// http.Client.Get itself builds a Background-context request, so for Send (which
// passes context.Background()) this is behavior-identical.
func (s *MinuteSender) httpGet(ctx context.Context, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, err
	}
	return s.httpClient.Do(req)
}
