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

	"github.com/PatrickWalther/twitch-miner-go/internal/auth"
	"github.com/PatrickWalther/twitch-miner-go/internal/constants"
	"github.com/PatrickWalther/twitch-miner-go/internal/models"
	"github.com/PatrickWalther/twitch-miner-go/internal/util"
)

var (
	ErrStreamerDoesNotExist = errors.New("streamer does not exist")
	ErrStreamerIsOffline    = errors.New("streamer is offline")
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
	userAgent     string
	client        *http.Client

	twilightBuildIDPattern *regexp.Regexp
	spadeURLPattern        *regexp.Regexp
	settingsURLPattern     *regexp.Regexp

	mu sync.RWMutex
}

func NewTwitchClient(twitchAuth *auth.TwitchAuth, deviceID string) *TwitchClient {
	return &TwitchClient{
		auth:                   twitchAuth,
		deviceID:               deviceID,
		clientSession:          util.RandomHex(16),
		clientVersion:          constants.DefaultClientVersion,
		userAgent:              constants.TVUserAgent,
		client:                 &http.Client{Timeout: 30 * time.Second},
		twilightBuildIDPattern: regexp.MustCompile(`window\.__twilightBuildID\s*=\s*"([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})"`),
		spadeURLPattern:        regexp.MustCompile(`"spade_url":"(.*?)"`),
		settingsURLPattern:     regexp.MustCompile(`(https://static.twitchcdn.net/config/settings.*?js|https://assets.twitch.tv/config/settings.*?.js)`),
	}
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

	respBody, err := c.doGQLRequestWithRetry(body, operation.OperationName)
	if err != nil {
		return nil, err
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

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

	respBody, err := c.doGQLRequestWithRetry(body, strings.Join(names, ","))
	if err != nil {
		return nil, err
	}

	var result []map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return result, nil
}

// doGQLRequestWithRetry sends the given already-marshaled GQL request body,
// retrying with exponential backoff on transient failures: network-level
// errors (timeouts, connection resets) and HTTP 429/5xx responses. Other
// HTTP errors (4xx auth/logic errors) are returned immediately since
// retrying them would just reproduce the same failure.
func (c *TwitchClient) doGQLRequestWithRetry(body []byte, operationLabel string) ([]byte, error) {
	var lastErr error

	for attempt := 0; attempt <= gqlMaxRetries; attempt++ {
		req, err := http.NewRequest("POST", constants.GQLURL, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
		c.setGQLHeaders(req)

		respBody, statusCode, err := c.doGQLOnce(req)
		if err == nil {
			slog.Debug("GQL response", "operation", operationLabel, "status", statusCode)
			return respBody, nil
		}

		lastErr = err

		transient := statusCode == 0 || isTransientGQLStatus(statusCode)
		if !transient {
			return nil, err
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

	return nil, fmt.Errorf("gql request failed after %d attempts: %w", gqlMaxRetries+1, lastErr)
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

func (c *TwitchClient) setGQLHeaders(req *http.Request) {
	req.Header.Set("Authorization", "OAuth "+c.auth.GetAuthToken())
	req.Header.Set("Client-Id", constants.ClientIDTV)
	req.Header.Set("Client-Session-Id", c.clientSession)
	req.Header.Set("Client-Version", c.getClientVersion())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("X-Device-Id", c.deviceID)
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

func (c *TwitchClient) MakePrediction(event *models.EventPrediction) error {
	decision := event.Bet.Calculate(event.Streamer.GetChannelPoints())

	if decision.Amount < 10 {
		slog.Info("Bet amount too low", "amount", decision.Amount)
		return nil
	}

	skip, comparedValue := event.Bet.Skip()
	if skip {
		slog.Info("Skipping bet", "filter", event.Bet.Settings.FilterCondition, "value", comparedValue)
		return nil
	}

	slog.Info("Placing prediction bet",
		"event", event.Title,
		"choice", decision.Choice,
		"amount", decision.Amount,
	)

	op := constants.MakePrediction.WithVariables(map[string]interface{}{
		"input": map[string]interface{}{
			"eventID":       event.EventID,
			"outcomeID":     decision.ID,
			"points":        decision.Amount,
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
			}
		}
	}

	event.BetPlaced = true
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
	slog.Info("Contributing to community goal", "goal", title, "amount", amount)

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
	return nil
}
