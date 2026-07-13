package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/auth"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/util"
)

var (
	ErrStreamerDoesNotExist = errors.New("streamer does not exist")
	ErrStreamerIsOffline    = errors.New("streamer is offline")

	// ErrRewardUnavailable is returned when a custom channel-points reward is
	// not (or no longer) redeemable — it disappeared from the channel, was
	// disabled/paused, went out of stock, or is on cooldown. Distinct from a
	// transport error so callers can show a "reward no longer available"
	// message instead of a generic failure.
	ErrRewardUnavailable = errors.New("reward is not available")

	// ErrInsufficientPoints is returned when the viewer's channel-points
	// balance is below a custom reward's cost.
	ErrInsufficientPoints = errors.New("not enough channel points")

	// ErrRewardInputRequired is returned when a reward requires viewer text
	// input but none was supplied.
	ErrRewardInputRequired = errors.New("this reward requires text input")

	// ErrUnauthorized indicates the Twitch OAuth token was rejected (expired
	// or revoked). Callers should treat this as "reauthorization required"
	// rather than a transient failure.
	ErrUnauthorized = errors.New("twitch: unauthorized (token expired or revoked)")
)

const (
	// gqlMaxRetries is the number of retries attempted after the initial try,
	// i.e. up to gqlMaxRetries+1 total attempts per GQL request.
	gqlMaxRetries  = 4
	gqlBaseBackoff = 500 * time.Millisecond
	gqlMaxBackoff  = 8 * time.Second
)

// isTransientGQLStatus reports whether an HTTP status code represents a
// transient failure worth retrying (rate limiting or server-side errors).
// 4xx errors other than 429 (bad auth, bad request, etc.) are not retried
// since retrying them would just repeat the same failure.
func isTransientGQLStatus(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests || statusCode >= http.StatusInternalServerError
}

type TwitchClient struct {
	auth          *auth.TwitchAuth
	deviceID      string
	clientSession string
	clientVersion string
	clientID      string
	userAgent     string
	client        *http.Client

	twilightBuildIDPattern *regexp.Regexp
	spadeURLPattern        *regexp.Regexp
	settingsURLPattern     *regexp.Regexp

	authErrorHandler func()

	healthMu    sync.RWMutex
	lastSuccess time.Time

	// gameSlugs caches game display name (lowercased) -> directory slug
	// lookups for the discovery subsystem; slugs are stable, so caching
	// halves that subsystem's GQL calls per sync.
	slugMu    sync.Mutex
	gameSlugs map[string]string

	mu sync.RWMutex
}

func NewTwitchClient(twitchAuth *auth.TwitchAuth, deviceID string) *TwitchClient {
	return &TwitchClient{
		auth:                   twitchAuth,
		deviceID:               deviceID,
		clientSession:          util.RandomHex(16),
		clientVersion:          constants.DefaultClientVersion,
		clientID:               constants.ClientIDTV,
		userAgent:              constants.TVUserAgent,
		client:                 &http.Client{Timeout: 30 * time.Second},
		twilightBuildIDPattern: regexp.MustCompile(`window\.__twilightBuildID\s*=\s*"([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})"`),
		spadeURLPattern:        regexp.MustCompile(`"spade_url":"(.*?)"`),
		settingsURLPattern:     regexp.MustCompile(`(https://static.twitchcdn.net/config/settings.*?js|https://assets.twitch.tv/config/settings.*?.js)`),
		lastSuccess:            time.Now(),
	}
}

// SetAuthErrorHandler registers a callback invoked the first time (and every
// subsequent time) a request fails with ErrUnauthorized.
func (c *TwitchClient) SetAuthErrorHandler(handler func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.authErrorHandler = handler
}

// LastSuccessAt returns when the client last completed a request without an
// auth or transport error. Used by the connection-health watchdog.
func (c *TwitchClient) LastSuccessAt() time.Time {
	c.healthMu.RLock()
	defer c.healthMu.RUnlock()
	return c.lastSuccess
}

func (c *TwitchClient) markSuccess() {
	c.healthMu.Lock()
	c.lastSuccess = time.Now()
	c.healthMu.Unlock()
}

func (c *TwitchClient) handleUnauthorized() {
	c.mu.RLock()
	handler := c.authErrorHandler
	c.mu.RUnlock()

	if handler != nil {
		handler()
	}
}

// isAuthError reports whether an HTTP status code or GQL response body
// indicates the OAuth token was rejected.
func isAuthError(statusCode int, result map[string]interface{}) bool {
	if statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden {
		return true
	}

	if errMsg, ok := result["error"].(string); ok && strings.EqualFold(errMsg, "Unauthorized") {
		return true
	}

	if errs, ok := result["errors"].([]interface{}); ok {
		for _, e := range errs {
			if em, ok := e.(map[string]interface{}); ok {
				if msg, ok := em["message"].(string); ok && strings.Contains(strings.ToLower(msg), "unauthorized") {
					return true
				}
			}
		}
	}

	return false
}

func (c *TwitchClient) PostGQL(operation constants.GQLOperation) (map[string]interface{}, error) {
	return c.postGQLRequest(operation)
}

func (c *TwitchClient) PostGQLBatch(operations []constants.GQLOperation) ([]map[string]interface{}, error) {
	return c.postGQLBatchRequest(operations)
}

func (c *TwitchClient) postGQLRequest(operation constants.GQLOperation) (map[string]interface{}, error) {
	body, err := json.Marshal(operation)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal operation: %w", err)
	}

	respBody, statusCode, err := c.doGQLRequestWithClientIDFallback(body, operation.OperationName)
	if err != nil {
		return nil, err
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	if isAuthError(statusCode, result) {
		c.handleUnauthorized()
		return nil, fmt.Errorf("%w: operation %s", ErrUnauthorized, operation.OperationName)
	}

	c.markSuccess()
	return result, nil
}

func (c *TwitchClient) postGQLBatchRequest(operations []constants.GQLOperation) ([]map[string]interface{}, error) {
	body, err := json.Marshal(operations)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal operations: %w", err)
	}

	names := make([]string, len(operations))
	for i, op := range operations {
		names[i] = op.OperationName
	}

	respBody, statusCode, err := c.doGQLRequestWithClientIDFallback(body, strings.Join(names, ","))
	if err != nil {
		return nil, err
	}

	var result []map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	if statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden {
		c.handleUnauthorized()
		return nil, fmt.Errorf("%w (status %d)", ErrUnauthorized, statusCode)
	}
	for _, item := range result {
		if isAuthError(statusCode, item) {
			c.handleUnauthorized()
			return nil, ErrUnauthorized
		}
	}

	c.markSuccess()
	return result, nil
}

// isPersistedQueryNotFound reports whether a GQL response body carries a
// PersistedQueryNotFound error. Twitch returns this (typically with HTTP 200)
// when the persisted-query sha256 hash it has on record for the given Client-Id
// no longer matches — usually because Twitch rotated/invalidated the hashes or
// the client ID itself. It is detected via a raw substring match so it works
// for both single and batched responses regardless of the exact error shape
// (errors[].message vs errorType).
func isPersistedQueryNotFound(respBody []byte) bool {
	return bytes.Contains(respBody, []byte("PersistedQueryNotFound"))
}

// clientIDCandidates returns the ordered list of client IDs to try for a GQL
// request: the currently-active one first, followed by the remaining known
// public client IDs from constants.GQLClientIDFallbacks (de-duplicated).
func clientIDCandidates(active string) []string {
	candidates := []string{active}
	for _, id := range constants.GQLClientIDFallbacks {
		if id != active {
			candidates = append(candidates, id)
		}
	}
	return candidates
}

// doGQLRequestWithClientIDFallback sends a GQL request and, on a
// PersistedQueryNotFound response, transparently retries with the alternate
// public Twitch client IDs before giving up. This guards against the
// well-known failure where Twitch rotates or invalidates the persisted-query
// hashes tied to a hardcoded client ID, which would otherwise break every GQL
// call at once. Transient network/HTTP failures are handled one layer down by
// doGQLRequestWithRetry — this layer deals only with the stale
// client-ID/query-hash case, and its returned error is passed straight
// through.
//
// It starts with the currently-active client ID and, on PersistedQueryNotFound,
// walks the remaining candidates. When an alternate client ID succeeds it is
// promoted to the active default (so subsequent requests use it directly) and
// logged at WARN level so the rotation is visible and the hardcoded default can
// be updated.
func (c *TwitchClient) doGQLRequestWithClientIDFallback(body []byte, operationLabel string) ([]byte, int, error) {
	activeClientID := c.getClientID()
	candidates := clientIDCandidates(activeClientID)

	var (
		respBody   []byte
		statusCode int
		err        error
	)

	for i, clientID := range candidates {
		respBody, statusCode, err = c.doGQLRequestWithRetry(body, operationLabel, clientID)
		if err != nil {
			return respBody, statusCode, err
		}

		if !isPersistedQueryNotFound(respBody) {
			if clientID != activeClientID {
				c.setClientID(clientID)
				slog.Warn("GQL PersistedQueryNotFound resolved by switching Twitch client ID; consider updating the default client ID",
					"operation", operationLabel,
					"previousClientID", activeClientID,
					"newClientID", clientID,
				)
			}
			return respBody, statusCode, nil
		}

		slog.Warn("GQL request returned PersistedQueryNotFound; trying next client ID",
			"operation", operationLabel,
			"clientID", clientID,
			"remainingCandidates", len(candidates)-i-1,
		)
	}

	slog.Error("GQL request returned PersistedQueryNotFound on all known client IDs; persisted-query hashes are likely stale and need updating",
		"operation", operationLabel,
		"clientIDsTried", len(candidates),
	)

	// Return the last response so the caller parses it as usual and fails
	// downstream gracefully (missing data) rather than treating this as a
	// hard transport error.
	return respBody, statusCode, nil
}

// doGQLRequestWithRetry sends the given already-marshaled GQL request body
// using the supplied client ID, retrying with exponential backoff on transient
// failures: network-level errors (timeouts, connection resets) and HTTP 429/5xx
// responses. Other HTTP errors (4xx auth/logic errors) are returned immediately
// since retrying them would just reproduce the same failure.
func (c *TwitchClient) doGQLRequestWithRetry(body []byte, operationLabel, clientID string) ([]byte, int, error) {
	var lastErr error

	for attempt := 0; attempt <= gqlMaxRetries; attempt++ {
		req, err := http.NewRequest("POST", constants.GQLURL, bytes.NewReader(body))
		if err != nil {
			return nil, 0, fmt.Errorf("failed to create request: %w", err)
		}
		c.setGQLHeaders(req, clientID)

		respBody, statusCode, err := c.doGQLOnce(req)
		if err == nil {
			slog.Debug("GQL response", "operation", operationLabel, "status", statusCode)
			return respBody, statusCode, nil
		}

		lastErr = err

		transient := statusCode == 0 || isTransientGQLStatus(statusCode)
		if !transient {
			return nil, statusCode, err
		}

		if attempt == gqlMaxRetries {
			break
		}

		wait := gqlBackoffDuration(attempt)
		slog.Warn("GQL request failed, retrying",
			"operation", operationLabel,
			"attempt", attempt+1,
			"maxAttempts", gqlMaxRetries+1,
			"waitSeconds", wait.Seconds(),
			"error", lastErr,
		)
		time.Sleep(wait)
	}

	slog.Error("GQL request exhausted all retries, skipping this cycle",
		"operation", operationLabel,
		"attempts", gqlMaxRetries+1,
		"error", lastErr,
	)

	return nil, 0, fmt.Errorf("gql request failed after %d attempts: %w", gqlMaxRetries+1, lastErr)
}

// doGQLOnce performs a single HTTP round trip. It returns the response body
// on success, or an error with the observed status code (0 for network-level
// errors, where no HTTP response was received at all).
func (c *TwitchClient) doGQLOnce(req *http.Request) ([]byte, int, error) {
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("failed to read response: %w", err)
	}

	if isTransientGQLStatus(resp.StatusCode) {
		return nil, resp.StatusCode, fmt.Errorf("transient GQL error: status %d", resp.StatusCode)
	}

	return respBody, resp.StatusCode, nil
}

// gqlBackoffDuration returns the exponential backoff delay (with jitter) for
// the given zero-based retry attempt, capped at gqlMaxBackoff.
func gqlBackoffDuration(attempt int) time.Duration {
	backoff := gqlBaseBackoff * time.Duration(1<<uint(attempt))
	if backoff > gqlMaxBackoff {
		backoff = gqlMaxBackoff
	}
	jitter := time.Duration(rand.Int63n(int64(backoff)/2 + 1))
	return backoff + jitter
}

func (c *TwitchClient) setGQLHeaders(req *http.Request, clientID string) {
	req.Header.Set("Authorization", "OAuth "+c.auth.GetAuthToken())
	req.Header.Set("Client-Id", clientID)
	req.Header.Set("Client-Session-Id", c.clientSession)
	req.Header.Set("Client-Version", c.getClientVersion())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("X-Device-Id", c.deviceID)
}

// getClientID returns the currently-active Twitch client ID used for GQL
// requests. It starts as constants.ClientIDTV and may be swapped to a fallback
// by doGQLRequestWithClientIDFallback when Twitch invalidates the default.
func (c *TwitchClient) getClientID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.clientID
}

func (c *TwitchClient) setClientID(id string) {
	c.mu.Lock()
	c.clientID = id
	c.mu.Unlock()
}

func (c *TwitchClient) getClientVersion() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.clientVersion
}

func (c *TwitchClient) UpdateClientVersion() string {
	resp, err := c.client.Get(constants.TwitchURL)
	if err != nil {
		return c.getClientVersion()
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return c.getClientVersion()
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return c.getClientVersion()
	}

	matches := c.twilightBuildIDPattern.FindSubmatch(body)
	if len(matches) < 2 {
		return c.getClientVersion()
	}

	c.mu.Lock()
	c.clientVersion = string(matches[1])
	c.mu.Unlock()

	slog.Debug("Updated client version", "version", c.clientVersion)
	return c.clientVersion
}

func (c *TwitchClient) GetChannelID(username string) (string, error) {
	op := constants.GetIDFromLogin.WithVariables(map[string]interface{}{
		"login": strings.ToLower(username),
	})

	resp, err := c.postGQLRequest(op)
	if err != nil {
		return "", err
	}

	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		return "", ErrStreamerDoesNotExist
	}

	user, ok := data["user"].(map[string]interface{})
	if !ok || user == nil {
		return "", ErrStreamerDoesNotExist
	}

	id, ok := user["id"].(string)
	if !ok {
		return "", ErrStreamerDoesNotExist
	}

	return id, nil
}

func (c *TwitchClient) GetStreamInfo(streamer *models.Streamer) (map[string]interface{}, error) {
	op := constants.VideoPlayerStreamInfoOverlayChannel.WithVariables(map[string]interface{}{
		"channel": streamer.Username,
	})

	resp, err := c.postGQLRequest(op)
	if err != nil {
		return nil, err
	}

	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		return nil, ErrStreamerIsOffline
	}

	user, ok := data["user"].(map[string]interface{})
	if !ok || user == nil {
		return nil, ErrStreamerIsOffline
	}

	stream, ok := user["stream"].(map[string]interface{})
	if !ok || stream == nil {
		return nil, ErrStreamerIsOffline
	}

	return user, nil
}

func (c *TwitchClient) UpdateStream(streamer *models.Streamer) error {
	if !streamer.Stream.UpdateRequired() {
		return nil
	}

	streamInfo, err := c.GetStreamInfo(streamer)
	if err != nil {
		return err
	}

	stream, ok := streamInfo["stream"].(map[string]interface{})
	if !ok {
		return ErrStreamerIsOffline
	}

	broadcastSettings, _ := streamInfo["broadcastSettings"].(map[string]interface{})

	broadcastID, _ := stream["id"].(string)
	title := ""
	if broadcastSettings != nil {
		title, _ = broadcastSettings["title"].(string)
	}

	var game *models.Game
	if broadcastSettings != nil {
		if gameData, ok := broadcastSettings["game"].(map[string]interface{}); ok && gameData != nil {
			game = &models.Game{}
			game.ID, _ = gameData["id"].(string)
			game.Name, _ = gameData["name"].(string)
			game.DisplayName, _ = gameData["displayName"].(string)
		}
	}

	var tags []models.Tag
	if tagsData, ok := stream["tags"].([]interface{}); ok {
		for _, t := range tagsData {
			if tagMap, ok := t.(map[string]interface{}); ok {
				tag := models.Tag{}
				tag.ID, _ = tagMap["id"].(string)
				tag.LocalizedName, _ = tagMap["localizedName"].(string)
				tags = append(tags, tag)
			}
		}
	}

	viewersCount := 0
	if vc, ok := stream["viewersCount"].(float64); ok {
		viewersCount = int(vc)
	}

	streamer.Stream.Update(broadcastID, strings.TrimSpace(title), game, tags, viewersCount)

	if game != nil && game.Name != "" && game.ID != "" && streamer.Settings.ClaimDrops {
		campaignIDs, _ := c.GetCampaignIDsFromStreamer(streamer)
		streamer.Stream.CampaignIDs = campaignIDs
	}

	streamer.Stream.SetPayload(
		streamer.ChannelID,
		broadcastID,
		c.auth.GetUserID(),
		streamer.Username,
		game,
	)

	return nil
}

func (c *TwitchClient) GetSpadeURL(streamer *models.Streamer) error {
	streamerURL := fmt.Sprintf("%s/%s", constants.TwitchURL, streamer.Username)

	req, err := http.NewRequest("GET", streamerURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:85.0) Gecko/20100101 Firefox/85.0")

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	settingsMatches := c.settingsURLPattern.FindSubmatch(body)
	if len(settingsMatches) < 2 {
		return fmt.Errorf("failed to find settings URL")
	}

	settingsResp, err := c.client.Get(string(settingsMatches[1]))
	if err != nil {
		return err
	}
	defer func() { _ = settingsResp.Body.Close() }()

	settingsBody, err := io.ReadAll(settingsResp.Body)
	if err != nil {
		return err
	}

	spadeMatches := c.spadeURLPattern.FindSubmatch(settingsBody)
	if len(spadeMatches) < 2 {
		return fmt.Errorf("failed to find spade URL")
	}

	streamer.Stream.SpadeURL = string(spadeMatches[1])
	return nil
}

func (c *TwitchClient) CheckStreamerOnline(streamer *models.Streamer) {
	if time.Since(streamer.GetOfflineAt()) < time.Minute {
		return
	}

	streamer.SetLastChecked(time.Now())

	if !streamer.GetIsOnline() {
		if err := c.GetSpadeURL(streamer); err != nil {
			slog.Debug("Failed to get spade URL", "streamer", streamer.Username, "error", err)
			streamer.SetOffline()
			return
		}

		if err := c.UpdateStream(streamer); err != nil {
			slog.Debug("Failed to update stream", "streamer", streamer.Username, "error", err)
			streamer.SetOffline()
			return
		}

		streamer.SetOnline()
		slog.Info("Streamer is online", "streamer", streamer.Username)
	} else {
		if err := c.UpdateStream(streamer); err != nil {
			slog.Info("Streamer went offline", "streamer", streamer.Username)
			streamer.SetOffline()
		}
	}
}

func (c *TwitchClient) LoadChannelPointsContext(streamer *models.Streamer) error {
	op := constants.ChannelPointsContext.WithVariables(map[string]interface{}{
		"channelLogin": streamer.Username,
	})

	resp, err := c.postGQLRequest(op)
	if err != nil {
		return err
	}

	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		return ErrStreamerDoesNotExist
	}

	community, ok := data["community"].(map[string]interface{})
	if !ok || community == nil {
		return ErrStreamerDoesNotExist
	}

	channel, ok := community["channel"].(map[string]interface{})
	if !ok || channel == nil {
		return ErrStreamerDoesNotExist
	}

	self, ok := channel["self"].(map[string]interface{})
	if !ok {
		return nil
	}

	communityPoints, ok := self["communityPoints"].(map[string]interface{})
	if !ok {
		return nil
	}

	if balance, ok := communityPoints["balance"].(float64); ok {
		streamer.SetChannelPoints(int(balance))
	}

	if multipliers, ok := communityPoints["activeMultipliers"].([]interface{}); ok {
		streamer.ActiveMultipliers = nil
		for _, m := range multipliers {
			if mMap, ok := m.(map[string]interface{}); ok {
				if factor, ok := mMap["factor"].(float64); ok {
					streamer.ActiveMultipliers = append(streamer.ActiveMultipliers, models.Multiplier{Factor: factor})
				}
			}
		}
	}

	if streamer.Settings.CommunityGoals {
		if settings, ok := channel["communityPointsSettings"].(map[string]interface{}); ok {
			if goals, ok := settings["goals"].([]interface{}); ok {
				for _, g := range goals {
					if goalMap, ok := g.(map[string]interface{}); ok {
						goal := models.CommunityGoalFromGQL(goalMap)
						streamer.AddCommunityGoal(goal)
					}
				}
			}
		}
	}

	if availableClaim, ok := communityPoints["availableClaim"].(map[string]interface{}); ok && availableClaim != nil {
		if claimID, ok := availableClaim["id"].(string); ok {
			if err := c.ClaimBonus(streamer, claimID); err != nil {
				slog.Error("Failed to claim bonus", "error", err)
			}
		}
	}

	return nil
}

// ClaimAvailableBonus is the polling fallback for channel-points bonus chests.
// The primary claim path is driven by the community-points-user PubSub
// "claim-available" event, but PubSub delivery is known to occasionally drop
// events (a recurring pain point across Twitch miners), leaving a chest
// unclaimed until it expires. This re-reads the channel-points context
// directly and claims any bonus currently available, returning true when a
// claim was actually made so the caller can log that the fallback - rather
// than PubSub - caught it.
func (c *TwitchClient) ClaimAvailableBonus(streamer *models.Streamer) (bool, error) {
	op := constants.ChannelPointsContext.WithVariables(map[string]interface{}{
		"channelLogin": streamer.Username,
	})

	resp, err := c.postGQLRequest(op)
	if err != nil {
		return false, err
	}

	claimID := availableClaimID(resp)
	if claimID == "" {
		return false, nil
	}

	if err := c.ClaimBonus(streamer, claimID); err != nil {
		return false, err
	}
	return true, nil
}

// availableClaimID walks a ChannelPointsContext GraphQL response down to the
// currently-claimable bonus chest's claim ID, returning "" when no bonus is
// available. It defends against every level being absent so a partial or
// unexpected response is treated as "nothing to claim" rather than panicking.
func availableClaimID(resp map[string]interface{}) string {
	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		return ""
	}
	community, ok := data["community"].(map[string]interface{})
	if !ok || community == nil {
		return ""
	}
	channel, ok := community["channel"].(map[string]interface{})
	if !ok || channel == nil {
		return ""
	}
	self, ok := channel["self"].(map[string]interface{})
	if !ok || self == nil {
		return ""
	}
	communityPoints, ok := self["communityPoints"].(map[string]interface{})
	if !ok || communityPoints == nil {
		return ""
	}
	availableClaim, ok := communityPoints["availableClaim"].(map[string]interface{})
	if !ok || availableClaim == nil {
		return ""
	}
	id, _ := availableClaim["id"].(string)
	return id
}

func (c *TwitchClient) ClaimBonus(streamer *models.Streamer, claimID string) error {
	slog.Info("Claiming bonus", "streamer", streamer.Username)

	op := constants.ClaimCommunityPoints.WithVariables(map[string]interface{}{
		"input": map[string]interface{}{
			"channelID": streamer.ChannelID,
			"claimID":   claimID,
		},
	})

	_, err := c.postGQLRequest(op)
	return err
}

func (c *TwitchClient) ClaimMoment(streamer *models.Streamer, momentID string) error {
	slog.Info("Claiming moment", "streamer", streamer.Username)

	op := constants.CommunityMomentCalloutClaim.WithVariables(map[string]interface{}{
		"input": map[string]interface{}{
			"momentID": momentID,
		},
	})

	_, err := c.postGQLRequest(op)
	return err
}

func (c *TwitchClient) JoinRaid(streamer *models.Streamer, raid *models.Raid) error {
	if streamer.Raid != nil && streamer.Raid.RaidID == raid.RaidID {
		return nil
	}

	slog.Info("Joining raid", "from", streamer.Username, "to", raid.TargetLogin)

	streamer.Raid = raid

	op := constants.JoinRaid.WithVariables(map[string]interface{}{
		"input": map[string]interface{}{
			"raidID": raid.RaidID,
		},
	})

	_, err := c.postGQLRequest(op)
	return err
}

// PlacePredictionBet places a single prediction bet for an explicit outcome and
// amount and interprets Twitch's response. It is the one and only Twitch entry
// point for placing a prediction — both the scheduled auto-bet and a manual
// dashboard bet go through it — so there is no second betting implementation to
// keep in sync. It deliberately does NOT mutate the event's local bet state
// (BetPlaced / Decision); the caller owns that bookkeeping under its own
// synchronization, which is what lets the pool serialize auto and manual bets
// against each other without this method knowing about locks.
//
// A returned error is either a transport/auth error from postGQLRequest or a
// "prediction error: <CODE>" carrying Twitch's own rejection code (e.g.
// NOT_ENOUGH_POINTS, EVENT_NOT_ACTIVE), so callers can surface a precise reason.
func (c *TwitchClient) PlacePredictionBet(event *models.EventPrediction, outcomeID string, amount int) error {
	op := constants.MakePrediction.WithVariables(map[string]interface{}{
		"input": map[string]interface{}{
			"eventID":       event.EventID,
			"outcomeID":     outcomeID,
			"points":        amount,
			"transactionID": util.RandomHex(16),
		},
	})

	resp, err := c.postGQLRequest(op)
	if err != nil {
		return err
	}

	if data, ok := resp["data"].(map[string]interface{}); ok {
		if makePrediction, ok := data["makePrediction"].(map[string]interface{}); ok {
			if errData, ok := makePrediction["error"].(map[string]interface{}); ok && errData != nil {
				if code, ok := errData["code"].(string); ok {
					return fmt.Errorf("prediction error: %s", code)
				}
				return fmt.Errorf("prediction error")
			}
		}
	}

	return nil
}

func (c *TwitchClient) GetCampaignIDsFromStreamer(streamer *models.Streamer) ([]string, error) {
	op := constants.DropsHighlightServiceAvailableDrops.WithVariables(map[string]interface{}{
		"channelID": streamer.ChannelID,
	})

	resp, err := c.postGQLRequest(op)
	if err != nil {
		return nil, err
	}

	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		return nil, nil
	}

	channel, ok := data["channel"].(map[string]interface{})
	if !ok || channel == nil {
		return nil, nil
	}

	campaigns, ok := channel["viewerDropCampaigns"].([]interface{})
	if !ok || campaigns == nil {
		return nil, nil
	}

	var ids []string
	for _, campaign := range campaigns {
		if c, ok := campaign.(map[string]interface{}); ok {
			if id, ok := c["id"].(string); ok {
				ids = append(ids, id)
			}
		}
	}

	return ids, nil
}

// GetDropCampaignDetails fetches the full details for a single drop campaign,
// including its timeBasedDrops (each with its own start/end dates, required
// minutes and benefit). The ViewerDropsDashboard listing only returns campaign
// summaries without this per-drop breakdown, so the details must be fetched
// per campaign before the campaign can be tracked. Returns the raw
// `data.user.dropCampaign` map, or nil if the campaign is not found.
func (c *TwitchClient) GetDropCampaignDetails(campaignID string) (map[string]interface{}, error) {
	op := constants.DropCampaignDetails.WithVariables(map[string]interface{}{
		"dropID":       campaignID,
		"channelLogin": c.auth.GetUserID(),
	})

	resp, err := c.postGQLRequest(op)
	if err != nil {
		return nil, err
	}

	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		return nil, nil
	}

	user, ok := data["user"].(map[string]interface{})
	if !ok || user == nil {
		return nil, nil
	}

	campaign, ok := user["dropCampaign"].(map[string]interface{})
	if !ok || campaign == nil {
		return nil, nil
	}

	return campaign, nil
}

func (c *TwitchClient) GetPlaybackAccessToken(username string) (string, string, error) {
	op := constants.PlaybackAccessToken.WithVariables(map[string]interface{}{
		"login":      username,
		"isLive":     true,
		"isVod":      false,
		"vodID":      "",
		"playerType": "site",
	})

	resp, err := c.postGQLRequest(op)
	if err != nil {
		return "", "", err
	}

	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		slog.Debug("PlaybackAccessToken: no data", "username", username, "response", resp)
		return "", "", fmt.Errorf("no data in response")
	}

	sat, ok := data["streamPlaybackAccessToken"].(map[string]interface{})
	if !ok || sat == nil {
		sat, ok = data["streamAccessToken"].(map[string]interface{})
		if !ok || sat == nil {
			slog.Debug("PlaybackAccessToken: no token found", "username", username, "data", data)
			return "", "", fmt.Errorf("no stream access token")
		}
	}

	signature, _ := sat["signature"].(string)
	value, _ := sat["value"].(string)

	if signature == "" || value == "" {
		return "", "", fmt.Errorf("empty stream access token")
	}

	return signature, value, nil
}

func (c *TwitchClient) ClaimDrop(drop *models.Drop) (bool, error) {
	slog.Info("Claiming drop", "drop", drop.Name)

	op := constants.DropsPageClaimDropRewards.WithVariables(map[string]interface{}{
		"input": map[string]interface{}{
			"dropInstanceID": drop.DropInstanceID,
		},
	})

	resp, err := c.postGQLRequest(op)
	if err != nil {
		return false, err
	}

	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		return false, nil
	}

	claimRewards, ok := data["claimDropRewards"].(map[string]interface{})
	if !ok || claimRewards == nil {
		if errs, ok := data["errors"].([]interface{}); ok && len(errs) > 0 {
			return false, nil
		}
		return false, nil
	}

	if status, ok := claimRewards["status"].(string); ok {
		return status == "ELIGIBLE_FOR_ALL" || status == "DROP_INSTANCE_ALREADY_CLAIMED", nil
	}

	return false, nil
}

func (c *TwitchClient) ContributeToCommunityGoal(streamer *models.Streamer, goalID, title string, amount int) error {
	op := constants.ContributeCommunityPointsCommunityGoal.WithVariables(map[string]interface{}{
		"input": map[string]interface{}{
			"amount":        amount,
			"channelID":     streamer.ChannelID,
			"goalID":        goalID,
			"transactionID": util.RandomHex(16),
		},
	})

	resp, err := c.postGQLRequest(op)
	if err != nil {
		return err
	}

	if data, ok := resp["data"].(map[string]interface{}); ok {
		if contribute, ok := data["contributeCommunityPointsCommunityGoal"].(map[string]interface{}); ok {
			if errData, ok := contribute["error"].(map[string]interface{}); ok && errData != nil {
				return fmt.Errorf("contribution error: %v", errData)
			}
		}
	}

	streamer.SetChannelPoints(streamer.GetChannelPoints() - amount)

	slog.Info("Contributed to community goal",
		"streamer", streamer.Username,
		"goal", title,
		"amount", amount,
		"remainingBalance", streamer.GetChannelPoints())

	return nil
}

// GetCustomRewards returns the streamer's custom channel-points rewards (the
// personal rewards a viewer can redeem with points, not Community Goals). It
// reuses the ChannelPointsContext query — the same one that carries the point
// balance and goals — and, since that response also includes the up-to-date
// balance, refreshes the streamer's cached points as a side effect so callers
// can immediately compare cost against balance.
func (c *TwitchClient) GetCustomRewards(streamer *models.Streamer) ([]*models.CustomReward, error) {
	op := constants.ChannelPointsContext.WithVariables(map[string]interface{}{
		"channelLogin": streamer.Username,
	})

	resp, err := c.postGQLRequest(op)
	if err != nil {
		return nil, err
	}

	channel := channelPointsChannel(resp)
	if channel == nil {
		return nil, ErrStreamerDoesNotExist
	}

	if self, ok := channel["self"].(map[string]interface{}); ok && self != nil {
		if communityPoints, ok := self["communityPoints"].(map[string]interface{}); ok {
			if balance, ok := communityPoints["balance"].(float64); ok {
				streamer.SetChannelPoints(int(balance))
			}
		}
	}

	settings, ok := channel["communityPointsSettings"].(map[string]interface{})
	if !ok || settings == nil {
		return nil, nil
	}

	rawRewards, ok := settings["customRewards"].([]interface{})
	if !ok {
		return nil, nil
	}

	rewards := make([]*models.CustomReward, 0, len(rawRewards))
	for _, r := range rawRewards {
		if rewardMap, ok := r.(map[string]interface{}); ok {
			reward := models.CustomRewardFromGQL(rewardMap)
			if reward.ID != "" {
				rewards = append(rewards, reward)
			}
		}
	}

	return rewards, nil
}

// channelPointsChannel walks a ChannelPointsContext response down to the
// community.channel object, returning nil if any level is missing so callers
// treat an unexpected shape as "no data" rather than panicking.
func channelPointsChannel(resp map[string]interface{}) map[string]interface{} {
	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		return nil
	}
	community, ok := data["community"].(map[string]interface{})
	if !ok || community == nil {
		return nil
	}
	channel, ok := community["channel"].(map[string]interface{})
	if !ok || channel == nil {
		return nil
	}
	return channel
}

// RedeemCustomReward spends channel points on a custom reward. The reward's
// current cost, title and prompt are echoed back in the input because Twitch
// rejects the redemption (PROPERTIES_MISMATCH) if any of them no longer match
// the server's copy — which is exactly the "reward changed between showing the
// list and clicking" race we want surfaced as a clear error. textInput carries
// the viewer's message for user-input rewards and is omitted otherwise.
//
// Errors are mapped to friendly messages: transport failures propagate as-is,
// while Twitch's own rejection codes become ErrInsufficientPoints /
// ErrRewardUnavailable or a descriptive error, never a panic. On success the
// streamer's cached balance is decremented by the cost.
func (c *TwitchClient) RedeemCustomReward(streamer *models.Streamer, reward *models.CustomReward, textInput string) error {
	slog.Info("Redeeming custom reward", "streamer", streamer.Username, "reward", reward.Title, "cost", reward.Cost)

	input := map[string]interface{}{
		"channelID":     streamer.ChannelID,
		"cost":          reward.Cost,
		"pricingType":   "POINTS",
		"prompt":        reward.Prompt,
		"rewardID":      reward.ID,
		"title":         reward.Title,
		"transactionID": util.RandomHex(16),
	}
	if reward.IsUserInputRequired && textInput != "" {
		input["textInput"] = textInput
	}

	op := constants.RedeemCustomReward.WithVariables(map[string]interface{}{
		"input": input,
	})

	resp, err := c.postGQLRequest(op)
	if err != nil {
		return err
	}

	if err := redeemResponseError(resp); err != nil {
		return err
	}

	streamer.SetChannelPoints(streamer.GetChannelPoints() - reward.Cost)
	return nil
}

// redeemResponseError inspects a RedeemCustomReward response and returns a
// friendly error when the redemption failed, or nil on success. It handles
// both top-level GraphQL errors and the mutation payload's own error object
// (data.redeemCommunityPointsCustomReward.error — the field keeps its old name
// even though the operation was renamed).
func redeemResponseError(resp map[string]interface{}) error {
	if errs, ok := resp["errors"].([]interface{}); ok && len(errs) > 0 {
		if em, ok := errs[0].(map[string]interface{}); ok {
			if msg, ok := em["message"].(string); ok && msg != "" {
				return fmt.Errorf("redemption rejected: %s", msg)
			}
		}
		return fmt.Errorf("redemption rejected")
	}

	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("redemption failed: unexpected response")
	}

	payload, ok := data["redeemCommunityPointsCustomReward"].(map[string]interface{})
	if !ok {
		payload, _ = data["redeemCustomReward"].(map[string]interface{})
	}
	if payload == nil {
		// No payload and no errors: treat as success rather than inventing a
		// failure, mirroring how Twitch omits the object on some success paths.
		return nil
	}

	errObj, ok := payload["error"].(map[string]interface{})
	if !ok || errObj == nil {
		return nil
	}

	code, _ := errObj["code"].(string)
	return redeemErrorForCode(code)
}

// redeemErrorForCode maps a Twitch redemption error code to a user-facing
// error, preferring the shared sentinels so callers can branch on them.
func redeemErrorForCode(code string) error {
	switch code {
	case "INSUFFICIENT_POINTS":
		return ErrInsufficientPoints
	case "NOT_AVAILABLE", "DISABLED", "OUT_OF_STOCK":
		return ErrRewardUnavailable
	case "COOLDOWN":
		return fmt.Errorf("reward is on cooldown")
	case "MAX_PER_STREAM_EXCEEDED":
		return fmt.Errorf("maximum redemptions per stream reached")
	case "MAX_PER_USER_PER_STREAM_EXCEEDED":
		return fmt.Errorf("you have already redeemed this reward this stream")
	case "PROPERTIES_MISMATCH":
		return fmt.Errorf("reward changed — refresh and try again")
	case "":
		return fmt.Errorf("redemption failed")
	default:
		return fmt.Errorf("redemption failed: %s", code)
	}
}
