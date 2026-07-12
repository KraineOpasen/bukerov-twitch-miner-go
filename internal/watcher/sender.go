package watcher

import (
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
// the one mechanism that actually earns watch time, shared by the fixed-list
// MinuteWatcher and the directory-discovery watch slot so both report viewing
// identically.
type MinuteSender struct {
	client     *api.TwitchClient
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
func (s *MinuteSender) Send(streamer *models.Streamer) (simulateErr error, err error) {
	sig, token, err := s.client.GetPlaybackAccessToken(streamer.Username)
	if err != nil {
		return nil, fmt.Errorf("failed to get playback token: %w", err)
	}

	simulateErr = s.simulateWatching(streamer.Username, sig, token)

	if streamer.Stream.SpadeURL == "" {
		return simulateErr, fmt.Errorf("no spade URL")
	}

	payload, err := streamer.Stream.EncodePayload()
	if err != nil {
		return simulateErr, fmt.Errorf("failed to encode payload: %w", err)
	}

	req, err := http.NewRequest("POST", streamer.Stream.SpadeURL, strings.NewReader(spadeFormBody(payload)))
	if err != nil {
		return simulateErr, err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", constants.TVUserAgent)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return simulateErr, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return simulateErr, fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	return simulateErr, nil
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

// simulateWatching mimics a player fetching the stream: playlist, lowest
// quality variant, and a HEAD request on the newest segment.
func (s *MinuteSender) simulateWatching(channel, sig, token string) error {
	playlistURL := fmt.Sprintf("%s/api/channel/hls/%s.m3u8", constants.UsherURL, channel)

	params := url.Values{
		"sig":   {sig},
		"token": {token},
	}

	resp, err := s.httpClient.Get(playlistURL + "?" + params.Encode())
	if err != nil {
		return fmt.Errorf("failed to get playlist: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("playlist request failed with status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read playlist: %w", err)
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
		return fmt.Errorf("no stream URL found in playlist")
	}

	streamListResp, err := s.httpClient.Get(lowestQualityURL)
	if err != nil {
		return fmt.Errorf("failed to get stream list: %w", err)
	}
	defer func() { _ = streamListResp.Body.Close() }()

	if streamListResp.StatusCode != http.StatusOK {
		return fmt.Errorf("stream list request failed with status %d", streamListResp.StatusCode)
	}

	streamListBody, err := io.ReadAll(streamListResp.Body)
	if err != nil {
		return fmt.Errorf("failed to read stream list: %w", err)
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
		return fmt.Errorf("no segment URL found")
	}

	req, err := http.NewRequest("HEAD", segmentURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create HEAD request: %w", err)
	}
	req.Header.Set("User-Agent", constants.TVUserAgent)

	headResp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("HEAD request failed: %w", err)
	}
	defer func() { _ = headResp.Body.Close() }()

	if headResp.StatusCode != http.StatusOK {
		return fmt.Errorf("HEAD request returned status %d", headResp.StatusCode)
	}

	return nil
}
