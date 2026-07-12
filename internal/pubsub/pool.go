package pubsub

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

type MessageHandler func(msg *PubSubMessage, streamer *models.Streamer)
type StatusHandler func(streamer string, online bool)
type AuthErrorHandler func(err error)

type WebSocketPool struct {
	clients     []*WebSocketClient
	client      *api.TwitchClient
	streamers   []*models.Streamer
	authToken   string
	settings    config.RateLimitSettings
	predictions map[string]*models.EventPrediction

	onMessage      MessageHandler
	onStatusChange StatusHandler
	onAuthError    AuthErrorHandler

	mu sync.RWMutex
}

func NewWebSocketPool(twitchClient *api.TwitchClient, authToken string, streamers []*models.Streamer, settings config.RateLimitSettings) *WebSocketPool {
	return &WebSocketPool{
		client:      twitchClient,
		streamers:   streamers,
		authToken:   authToken,
		settings:    settings,
		predictions: make(map[string]*models.EventPrediction),
	}
}

func (p *WebSocketPool) SetMessageHandler(handler MessageHandler) {
	p.onMessage = handler
}

func (p *WebSocketPool) SetStatusHandler(handler StatusHandler) {
	p.onStatusChange = handler
}

func (p *WebSocketPool) SetAuthErrorHandler(handler AuthErrorHandler) {
	p.onAuthError = handler
}

// PredictionSnapshot is a read-only view of a tracked prediction event,
// exposed for the debug endpoint.
type PredictionSnapshot struct {
	Streamer     string
	EventID      string
	Title        string
	Status       string
	CreatedAt    time.Time
	BetPlaced    bool
	BetConfirmed bool
	BetAmount    int
}

// PredictionsSnapshot returns a view of every prediction event the pool is
// currently tracking. Safe to call from any goroutine.
func (p *WebSocketPool) PredictionsSnapshot() []PredictionSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()

	snapshots := make([]PredictionSnapshot, 0, len(p.predictions))
	for _, e := range p.predictions {
		snapshots = append(snapshots, PredictionSnapshot{
			Streamer:     e.Streamer.Username,
			EventID:      e.EventID,
			Title:        e.Title,
			Status:       string(e.Status),
			CreatedAt:    e.CreatedAt,
			BetPlaced:    e.BetPlaced,
			BetConfirmed: e.BetConfirmed,
			BetAmount:    e.Bet.Decision.Amount,
		})
	}
	return snapshots
}

// LastActivity returns the most recent PONG timestamp across all connections
// in the pool, i.e. the last confirmed sign of life from Twitch PubSub. Used
// by the connection-health watchdog.
func (p *WebSocketPool) LastActivity() time.Time {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var latest time.Time
	for _, ws := range p.clients {
		if t := ws.LastPong(); t.After(latest) {
			latest = t
		}
	}
	return latest
}

func (p *WebSocketPool) Submit(topic Topic) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.clients) == 0 || p.clients[len(p.clients)-1].TopicCount() >= constants.MaxTopicsPerConnection {
		ws := NewWebSocketClient(len(p.clients), p.authToken, p.settings.WebsocketPingInterval, p.handleMessage, p.handleError)
		if err := ws.Connect(); err != nil {
			return err
		}
		p.clients = append(p.clients, ws)
	}

	p.clients[len(p.clients)-1].Listen(topic)
	return nil
}

func (p *WebSocketPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, ws := range p.clients {
		ws.Close()
	}
	p.clients = nil
}

func (p *WebSocketPool) Unsubscribe(topic Topic) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, ws := range p.clients {
		if ws.Unlisten(topic) {
			return
		}
	}
}

func (p *WebSocketPool) UpdateStreamers(streamers []*models.Streamer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.streamers = streamers
}

func (p *WebSocketPool) findStreamer(channelID string) *models.Streamer {
	for _, s := range p.streamers {
		if s.ChannelID == channelID {
			return s
		}
	}
	return nil
}

func (p *WebSocketPool) handleMessage(msg *PubSubMessage) {
	streamer := p.findStreamer(msg.ChannelID)
	if streamer == nil {
		return
	}

	switch msg.Topic.Type {
	case TopicCommunityPointsUser:
		p.handleCommunityPointsUser(msg, streamer)
	case TopicVideoPlaybackByID:
		p.handleVideoPlayback(msg, streamer)
	case TopicRaid:
		p.handleRaid(msg, streamer)
	case TopicCommunityMomentsChannel:
		p.handleMoment(msg, streamer)
	case TopicPredictionsChannel:
		p.handlePredictionChannel(msg, streamer)
	case TopicPredictionsUser:
		p.handlePredictionUser(msg, streamer)
	case TopicCommunityPointsChannel:
		p.handleCommunityPointsChannel(msg, streamer)
	}

	if p.onMessage != nil {
		p.onMessage(msg, streamer)
	}
}

func (p *WebSocketPool) handleCommunityPointsUser(msg *PubSubMessage, streamer *models.Streamer) {
	switch msg.Type {
	case "points-earned", "points-spent":
		if msg.Data == nil {
			return
		}
		if balance, ok := msg.Data["balance"].(map[string]interface{}); ok {
			if bal, ok := balance["balance"].(float64); ok {
				streamer.SetChannelPoints(int(bal))
			}
		}

		if msg.Type == "points-earned" {
			if pointGain, ok := msg.Data["point_gain"].(map[string]interface{}); ok {
				earned := 0
				reasonCode := ""
				if pts, ok := pointGain["total_points"].(float64); ok {
					earned = int(pts)
				}
				if rc, ok := pointGain["reason_code"].(string); ok {
					reasonCode = rc
				}
				slog.Info("Points earned",
					"streamer", streamer.Username,
					"points", earned,
					"reason", reasonCode,
				)
				streamer.UpdateHistory(reasonCode, earned)

				// Passive WATCH gains arrive every few minutes per streamer
				// and would drown everything else in the ring buffer, so only
				// the notable reason codes (CLAIM, WATCH_STREAK, RAID, ...)
				// are recorded.
				if reasonCode != "WATCH" {
					events.Record(events.TypePointsEarned, streamer.Username, fmt.Sprintf("+%d (%s)", earned, reasonCode))
				}
			}
		}

	case "claim-available":
		if msg.Data == nil {
			return
		}
		if claim, ok := msg.Data["claim"].(map[string]interface{}); ok {
			if claimID, ok := claim["id"].(string); ok {
				if err := p.client.ClaimBonus(streamer, claimID); err != nil {
					slog.Error("Failed to claim bonus", "error", err)
				} else {
					events.Record(events.TypeBonusClaimed, streamer.Username, "bonus claimed")
				}
			}
		}
	}
}

func (p *WebSocketPool) handleVideoPlayback(msg *PubSubMessage, streamer *models.Streamer) {
	switch msg.Type {
	case "stream-up":
		streamer.StreamUpTime = time.Now()
	case "stream-down":
		if streamer.GetIsOnline() {
			streamer.SetOffline()
			slog.Info("Streamer went offline", "streamer", streamer.Username)
			if p.onStatusChange != nil {
				p.onStatusChange(streamer.Username, false)
			}
		}
	case "viewcount":
		wasOnline := streamer.GetIsOnline()
		if streamer.StreamUpElapsed() {
			p.client.CheckStreamerOnline(streamer)
			if !wasOnline && streamer.GetIsOnline() && p.onStatusChange != nil {
				p.onStatusChange(streamer.Username, true)
			}
		}
	}
}

func (p *WebSocketPool) handleRaid(msg *PubSubMessage, streamer *models.Streamer) {
	if msg.Type != "raid_update_v2" || !streamer.Settings.FollowRaid {
		return
	}

	raidData, ok := msg.Message["raid"].(map[string]interface{})
	if !ok {
		return
	}

	raidID, _ := raidData["id"].(string)
	targetLogin, _ := raidData["target_login"].(string)

	if raidID != "" && targetLogin != "" {
		raid := &models.Raid{
			RaidID:      raidID,
			TargetLogin: targetLogin,
		}
		if err := p.client.JoinRaid(streamer, raid); err != nil {
			slog.Error("Failed to join raid", "error", err)
		} else {
			events.Record(events.TypeRaidJoined, streamer.Username, "raid to "+targetLogin)
		}
	}
}

func (p *WebSocketPool) handleMoment(msg *PubSubMessage, streamer *models.Streamer) {
	if msg.Type != "active" || !streamer.Settings.ClaimMoments {
		return
	}

	if msg.Data == nil {
		return
	}

	if momentID, ok := msg.Data["moment_id"].(string); ok {
		if err := p.client.ClaimMoment(streamer, momentID); err != nil {
			slog.Error("Failed to claim moment", "error", err)
		} else {
			events.Record(events.TypeMomentClaimed, streamer.Username, "moment claimed")
		}
	}
}

func (p *WebSocketPool) handlePredictionChannel(msg *PubSubMessage, streamer *models.Streamer) {
	if !streamer.Settings.MakePredictions {
		return
	}

	if msg.Data == nil {
		return
	}

	eventData, ok := msg.Data["event"].(map[string]interface{})
	if !ok {
		return
	}

	eventID, _ := eventData["id"].(string)
	eventStatus, _ := eventData["status"].(string)

	switch msg.Type {
	case "event-created":
		p.mu.RLock()
		_, exists := p.predictions[eventID]
		p.mu.RUnlock()

		if exists || eventStatus != "ACTIVE" {
			return
		}

		title, _ := eventData["title"].(string)
		createdAtStr, _ := eventData["created_at"].(string)
		predictionWindowSeconds, _ := eventData["prediction_window_seconds"].(float64)
		outcomes, _ := eventData["outcomes"].([]interface{})

		createdAt, _ := time.Parse(time.RFC3339, createdAtStr)

		adjustedWindow := streamer.GetPredictionWindow(predictionWindowSeconds)

		event := models.NewEventPrediction(
			streamer,
			eventID,
			title,
			createdAt,
			adjustedWindow,
			eventStatus,
			outcomes,
		)

		if !streamer.GetIsOnline() {
			return
		}

		closingBetAfter := event.ClosingBetAfter(time.Now())
		if closingBetAfter <= 0 {
			return
		}

		if streamer.Settings.Bet.MinimumPoints > 0 &&
			streamer.GetChannelPoints() <= streamer.Settings.Bet.MinimumPoints {
			slog.Info("Not enough points for prediction",
				"streamer", streamer.Username,
				"points", streamer.GetChannelPoints(),
				"minimum", streamer.Settings.Bet.MinimumPoints,
			)
			return
		}

		p.mu.Lock()
		p.predictions[eventID] = event
		p.mu.Unlock()

		slog.Info("Prediction event scheduled",
			"streamer", streamer.Username,
			"event", title,
			"placeIn", closingBetAfter,
		)

		go func() {
			time.Sleep(time.Duration(closingBetAfter) * time.Second)
			p.mu.RLock()
			evt, exists := p.predictions[eventID]
			p.mu.RUnlock()

			if exists && evt.Status == models.PredictionActive {
				if err := p.client.MakePrediction(evt); err != nil {
					slog.Error("Failed to make prediction", "error", err)
				}
			}
		}()

	case "event-updated":
		p.mu.RLock()
		event, exists := p.predictions[eventID]
		p.mu.RUnlock()

		if !exists {
			return
		}

		event.Status = models.PredictionStatus(eventStatus)

		if !event.BetPlaced && event.Bet.Decision.ID == "" {
			if outcomes, ok := eventData["outcomes"].([]interface{}); ok {
				event.Bet.UpdateOutcomes(outcomes)
			}
		}
	}
}

func (p *WebSocketPool) handlePredictionUser(msg *PubSubMessage, streamer *models.Streamer) {
	if msg.Data == nil {
		return
	}

	prediction, ok := msg.Data["prediction"].(map[string]interface{})
	if !ok {
		return
	}

	eventID, _ := prediction["event_id"].(string)

	p.mu.RLock()
	event, exists := p.predictions[eventID]
	p.mu.RUnlock()

	if !exists {
		return
	}

	switch msg.Type {
	case "prediction-made":
		event.BetConfirmed = true
		slog.Info("Prediction confirmed", "event", event.Title)
		events.Record(events.TypeBetPlaced, streamer.Username, fmt.Sprintf("bet %d points on %q", event.Bet.Decision.Amount, event.Title))

	case "prediction-result":
		if !event.BetConfirmed {
			return
		}

		result, ok := prediction["result"].(map[string]interface{})
		if !ok {
			return
		}

		placed, won, gained := event.ParseResult(result)
		_ = placed
		_ = won

		slog.Info("Prediction result",
			"event", event.Title,
			"result", event.Result.Type,
			"gained", gained,
		)
		events.Record(events.TypeBetResult, streamer.Username, fmt.Sprintf("%s %+d points on %q", event.Result.Type, gained, event.Title))

		streamer.UpdateHistory("PREDICTION", gained)

		switch event.Result.Type {
		case models.ResultRefund:
			streamer.UpdateHistoryWithCounter("REFUND", -placed, -1)
		case models.ResultWin:
			streamer.UpdateHistoryWithCounter("PREDICTION", -won, -1)
		}
	}
}

func (p *WebSocketPool) handleCommunityPointsChannel(msg *PubSubMessage, streamer *models.Streamer) {
	if !streamer.Settings.CommunityGoals {
		return
	}

	if msg.Data == nil {
		return
	}

	goalData, ok := msg.Data["community_goal"].(map[string]interface{})
	if !ok {
		return
	}

	goal := models.CommunityGoalFromPubSub(goalData)

	switch msg.Type {
	case "community-goal-created":
		streamer.AddCommunityGoal(goal)
	case "community-goal-updated":
		streamer.UpdateCommunityGoal(goal)
	case "community-goal-deleted":
		if goalID, ok := goalData["id"].(string); ok {
			streamer.DeleteCommunityGoal(goalID)
		}
	}

	if msg.Type == "community-goal-updated" || msg.Type == "community-goal-created" {
		p.contributeToGoals(streamer)
	}
}

func (p *WebSocketPool) contributeToGoals(streamer *models.Streamer) {
	for _, goal := range streamer.CommunityGoals {
		if goal.Status != models.CommunityGoalStarted || !goal.IsInStock {
			continue
		}

		amountLeft := goal.AmountLeft()
		points := streamer.GetChannelPoints()
		if amountLeft <= 0 || points <= 0 {
			continue
		}

		amount := computeGoalContribution(goal, points, streamer.Settings)
		if amount <= 0 {
			slog.Info("Skipping community goal contribution: configured limit resolves to zero",
				"streamer", streamer.Username,
				"goal", goal.Title,
				"balance", points,
				"maxPercent", streamer.Settings.CommunityGoalsMaxPercent,
				"maxAmount", streamer.Settings.CommunityGoalsMaxAmount)
			continue
		}

		if err := p.client.ContributeToCommunityGoal(streamer, goal.GoalID, goal.Title, amount); err != nil {
			slog.Error("Failed to contribute to community goal", "error", err)
		}
	}
}

// computeGoalContribution decides how many points to contribute to a single
// community goal, honoring both Twitch's server-side per-user-per-stream cap and
// the user-configured percentage / absolute limits.
//
// Twitch's ContributeCommunityPointsCommunityGoal mutation accepts an arbitrary
// integer `amount`, so partial contributions are supported at the API level; the
// only server-imposed ceiling is goal.PerStreamUserMaxContribution.
func computeGoalContribution(goal *models.CommunityGoal, points int, settings models.StreamerSettings) int {
	amount := min(goal.AmountLeft(), points)

	// Never exceed Twitch's per-user-per-stream server cap when it is advertised.
	if goal.PerStreamUserMaxContribution > 0 {
		amount = min(amount, goal.PerStreamUserMaxContribution)
	}

	// User-configured percentage cap of the current balance (0 = no cap).
	if pct := settings.CommunityGoalsMaxPercent; pct > 0 && pct < 100 {
		capped := int(int64(points) * int64(pct) / 100)
		amount = min(amount, capped)
	}

	// User-configured absolute cap per contribution (0 = no cap).
	if maxAmount := settings.CommunityGoalsMaxAmount; maxAmount > 0 {
		amount = min(amount, maxAmount)
	}

	return amount
}

func (p *WebSocketPool) handleError(err error) {
	slog.Error("WebSocket error", "error", err)

	if errors.Is(err, ErrBadAuth) && p.onAuthError != nil {
		p.onAuthError(err)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
