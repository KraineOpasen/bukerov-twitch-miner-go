package pubsub

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
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

// BetResult is the settled-bet record the pool emits once, when a prediction
// resolves, for downstream ROI analytics. It carries the full context the pool
// has and the raw annotation string does not: the stake actually placed (kept
// even for a REFUND, which returns the stake), the payout, the net, the chosen
// outcome's odds, the strategy used ("MANUAL" for a dashboard bet), and whether
// it was manual. It is a pubsub-local type so the pool never imports analytics
// and stays independently testable; the miner maps it to analytics.BetRecord.
type BetResult struct {
	EventID    string
	Streamer   string
	Timestamp  time.Time
	Strategy   string
	ResultType string // WIN | LOSE | REFUND
	Placed     int
	Won        int
	Gained     int
	Odds       float64
	Manual     bool
}

// BetResultHandler receives one BetResult per resolved, confirmed bet.
type BetResultHandler func(BetResult)

// Manual-bet / round-control sentinel errors. Their messages are already
// user-safe, so the dashboard can surface them verbatim without leaking raw Go
// or Twitch internals.
var (
	ErrPredictionNotFound = errors.New("this prediction is no longer available")
	ErrOutcomeNotFound    = errors.New("that outcome is no longer available")
	ErrRoundClosed        = errors.New("this prediction has already closed")
	ErrAlreadyBet         = errors.New("a bet has already been placed on this prediction")
	ErrAutoBetPlaced      = errors.New("auto-bet already placed a bet on this prediction")
	ErrInvalidAmount      = errors.New("enter a positive amount")
	ErrAmountTooLow       = errors.New("the minimum bet is 10 points")
	ErrInsufficientPoints = errors.New("not enough channel points for that bet")
	ErrManualBetInFlight  = errors.New("a bet is already being placed for this prediction")
	ErrStreamerOffline    = errors.New("the streamer is offline")
)

// minPredictionBet is Twitch's minimum accepted prediction stake; both the
// auto-bet and manual bets honour it.
const minPredictionBet = 10

// maxPredictionAge bounds how long a tracked round may linger in memory as a
// hard safety net against leaks if a terminal event is ever missed. Real
// prediction windows are minutes; this generous ceiling only ever reaps rounds
// Twitch stopped talking about entirely.
const maxPredictionAge = 2 * time.Hour

// terminalCleanupGrace delays removal of a resolved/cancelled round so the
// separate predictions-user "prediction-result" message (which may arrive after
// the channel "event-updated") can still find the event to record history.
const terminalCleanupGrace = 30 * time.Second

// predictionPlacer is the narrow slice of the Twitch client the pool needs to
// place a prediction bet. Kept as an interface so the betting/concurrency logic
// can be unit-tested and locally dev-simulated without real Twitch calls;
// *api.TwitchClient satisfies it.
type predictionPlacer interface {
	PlacePredictionBet(event *models.EventPrediction, outcomeID string, amount int) error
}

// roundControl is the per-round transient state backing manual betting and the
// per-round auto-bet suppression. One entry lives alongside each tracked
// prediction in the pool and is removed together with it, so the state can
// never outlive its round.
type roundControl struct {
	// placeMu serializes the *entire* place-a-bet operation for this one round
	// (revalidation + the Twitch call + local bookkeeping). It is what makes a
	// manual bet and the scheduled auto-bet mutually exclusive so Twitch can
	// never receive two stakes for the same round. It is round-scoped, so bets
	// on other streamers/rounds run fully in parallel, and it is the only lock
	// held across the network call — never the pool-wide mu.
	placeMu sync.Mutex

	// The flags below are quick booleans guarded by the pool's mu (read under
	// RLock by the snapshot, written under Lock by the betting paths).
	autoBetSkip   bool   // auto-bet suppressed for this round (manual skip, or set after a manual bet)
	manualBet     bool   // the placed bet came from a manual dashboard action
	manualPending bool   // a manual placement is currently in flight (double-submit guard)
	manualErr     string // last manual placement error, human-readable
}

type WebSocketPool struct {
	clients     []*WebSocketClient
	client      *api.TwitchClient
	placer      predictionPlacer
	streamers   []*models.Streamer
	authToken   string
	settings    config.RateLimitSettings
	predictions map[string]*models.EventPrediction
	control     map[string]*roundControl

	onMessage      MessageHandler
	onStatusChange StatusHandler
	onAuthError    AuthErrorHandler
	onBetResult    BetResultHandler

	// reconnects records the timestamps of connection reconnects across the pool
	// as a self-synchronized sliding window. The connection-health watchdog reads
	// RecentReconnects to distinguish a flapping (degraded) link from a clean one.
	reconnects eventWindow

	mu sync.RWMutex
}

func NewWebSocketPool(twitchClient *api.TwitchClient, authToken string, streamers []*models.Streamer, settings config.RateLimitSettings) *WebSocketPool {
	return &WebSocketPool{
		client:      twitchClient,
		placer:      twitchClient,
		streamers:   streamers,
		authToken:   authToken,
		settings:    settings,
		predictions: make(map[string]*models.EventPrediction),
		control:     make(map[string]*roundControl),
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

// SetBetResultHandler registers the sink for settled-bet records (ROI analytics).
// Like the other handlers it is set once at wiring time before the pool starts.
func (p *WebSocketPool) SetBetResultHandler(handler BetResultHandler) {
	p.onBetResult = handler
}

// PredictionOutcomeSnapshot is a read-only view of one prediction outcome,
// carrying everything the dashboard's live-predictions board renders.
type PredictionOutcomeSnapshot struct {
	ID              string
	Title           string
	Color           string
	TotalUsers      int
	TotalPoints     int
	PercentageUsers float64
	Odds            float64
	OddsPercentage  float64
	// Chosen marks the outcome the bot bet on (matches Decision), so the board
	// can highlight it. Only meaningful once BetPlaced is true.
	Chosen bool
}

// PredictionSnapshot is a read-only view of a tracked prediction event,
// exposed for the debug endpoint and the dashboard live-predictions board.
type PredictionSnapshot struct {
	Streamer                string
	EventID                 string
	Title                   string
	Status                  string
	CreatedAt               time.Time
	PredictionWindowSeconds float64
	BetPlaced               bool
	BetConfirmed            bool
	BetAmount               int
	TotalPoints             int
	TotalUsers              int
	Outcomes                []PredictionOutcomeSnapshot

	// --- manual-control state (round-scoped) ---

	// Online / Balance reflect the streamer's current state, so the dashboard
	// can decide whether manual betting is offered and cap the stake.
	Online  bool
	Balance int
	// ManualBet is true when the placed bet was a manual dashboard action (vs
	// auto-bet); BetOutcomeTitle names the chosen outcome once a bet is placed.
	ManualBet       bool
	BetOutcomeTitle string
	// AutoBetSkipped is true when auto-bet is suppressed for this round (manual
	// skip toggle, or implicitly after a manual bet). ManualPending is true
	// while a manual placement is in flight; ManualError carries the last
	// human-readable manual failure.
	AutoBetSkipped bool
	ManualPending  bool
	ManualError    string
}

// PredictionsSnapshot returns a view of every prediction event the pool is
// currently tracking. Safe to call from any goroutine.
func (p *WebSocketPool) PredictionsSnapshot() []PredictionSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()

	snapshots := make([]PredictionSnapshot, 0, len(p.predictions))
	for id, e := range p.predictions {
		snap := PredictionSnapshot{
			Streamer:                e.Streamer.Username,
			EventID:                 e.EventID,
			Title:                   e.Title,
			Status:                  string(e.Status),
			CreatedAt:               e.CreatedAt,
			PredictionWindowSeconds: e.PredictionWindowSeconds,
			BetPlaced:               e.BetPlaced,
			BetConfirmed:            e.BetConfirmed,
			BetAmount:               e.Bet.Decision.Amount,
			TotalPoints:             e.Bet.TotalPoints,
			TotalUsers:              e.Bet.TotalUsers,
			Online:                  e.Streamer.GetIsOnline(),
			Balance:                 e.Streamer.GetChannelPoints(),
		}
		if rc := p.control[id]; rc != nil {
			snap.AutoBetSkipped = rc.autoBetSkip
			snap.ManualBet = rc.manualBet
			snap.ManualPending = rc.manualPending
			snap.ManualError = rc.manualErr
		}
		for i, o := range e.Bet.Outcomes {
			if o == nil {
				continue
			}
			chosen := e.BetPlaced && i == e.Bet.Decision.Choice
			if chosen {
				snap.BetOutcomeTitle = o.Title
			}
			snap.Outcomes = append(snap.Outcomes, PredictionOutcomeSnapshot{
				ID:              o.ID,
				Title:           o.Title,
				Color:           o.Color,
				TotalUsers:      o.TotalUsers,
				TotalPoints:     o.TotalPoints,
				PercentageUsers: o.PercentageUsers,
				Odds:            o.Odds,
				OddsPercentage:  o.OddsPercentage,
				Chosen:          chosen,
			})
		}
		snapshots = append(snapshots, snap)
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

// ConnState is a read-only, per-connection view of one pool member, for the
// per-index health signal and the /debug/snapshot pubsub section. It carries no
// secrets — only counters, a timestamp, and lifecycle flags.
type ConnState struct {
	Index        int
	Topics       int
	LastPong     time.Time
	Reconnecting bool
	Closed       bool
}

// ConnSnapshot returns a per-connection view of the pool. Unlike LastActivity
// (which collapses the whole pool to a single max-PONG and so is blind to one
// dead connection among healthy ones), this exposes every index individually so
// the health watchdog can flag a single stale or topic-less connection. Safe to
// call from any goroutine.
func (p *WebSocketPool) ConnSnapshot() []ConnState {
	p.mu.RLock()
	defer p.mu.RUnlock()

	out := make([]ConnState, 0, len(p.clients))
	for _, ws := range p.clients {
		out = append(out, ws.state())
	}
	return out
}

// RecentReconnects returns how many reconnects occurred across all pool
// connections within the trailing window. Used by the connection-health
// watchdog to raise a "degraded" signal on a flapping link.
func (p *WebSocketPool) RecentReconnects(window time.Duration) int {
	return p.reconnects.count(time.Now(), window)
}

func (p *WebSocketPool) Submit(topic Topic) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.clients) == 0 || p.clients[len(p.clients)-1].TopicCount() >= constants.MaxTopicsPerConnection {
		ws := NewWebSocketClient(len(p.clients), p.authToken, p.settings.WebsocketPingInterval, p.settings.ReconnectDelay, p.handleMessage, p.handleError)
		// Wire the reconnect counter before Connect() starts the read/ping loops,
		// so the handler is set before any reconnect can fire.
		ws.SetReconnectHandler(func() { p.reconnects.mark(time.Now()) })
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
	// Read under the lock: this runs on each connection's read-loop goroutine for
	// every message, while UpdateStreamers replaces p.streamers under the write
	// lock from the settings goroutine. Ranging the slice without the lock is a
	// data race on the slice header (a torn read could pair a new pointer with a
	// stale length). The returned *Streamer is safe to use after unlock — it has
	// its own mutex and the pointer itself is stable.
	p.mu.RLock()
	defer p.mu.RUnlock()
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
			bid := streamer.Stream.GetBroadcastID()
			streamer.SetOffline()
			slog.Info("Streamer went offline",
				"streamer", streamer.Username,
				"channelID", streamer.ChannelID,
				"broadcastID", bid)
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
		p.sweepStaleLocked()
		p.predictions[eventID] = event
		p.control[eventID] = &roundControl{}
		p.mu.Unlock()

		slog.Info("Prediction event scheduled",
			"streamer", streamer.Username,
			"event", title,
			"placeIn", closingBetAfter,
		)

		go func() {
			time.Sleep(time.Duration(closingBetAfter) * time.Second)
			p.placeAutoBet(eventID)
		}()

	case "event-updated":
		p.mu.Lock()
		event, exists := p.predictions[eventID]
		if !exists {
			p.mu.Unlock()
			return
		}

		event.Status = models.PredictionStatus(eventStatus)

		if !event.BetPlaced && event.Bet.Decision.ID == "" {
			if outcomes, ok := eventData["outcomes"].([]interface{}); ok {
				event.Bet.UpdateOutcomes(outcomes)
			}
		}
		p.mu.Unlock()

		// A resolved/cancelled round is finished: drop its tracked + transient
		// state after a short grace so the predictions-user result message can
		// still find it. This is what keeps the round-control map from growing
		// without bound.
		if eventStatus == string(models.PredictionResolved) || eventStatus == string(models.PredictionCanceled) {
			p.scheduleCleanup(eventID, terminalCleanupGrace)
		}
	}
}

// chosenOutcomeOdds returns the odds of the outcome the bot bet on, as known at
// resolution (outcomes stop updating once the round locks). Returns 0 when no
// outcome was chosen or the index is out of range.
func chosenOutcomeOdds(event *models.EventPrediction) float64 {
	choice := event.Bet.Decision.Choice
	if choice < 0 || choice >= len(event.Bet.Outcomes) {
		return 0
	}
	o := event.Bet.Outcomes[choice]
	if o == nil {
		return 0
	}
	return o.Odds
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
		p.mu.Lock()
		event.BetConfirmed = true
		amount := event.Bet.Decision.Amount
		p.mu.Unlock()
		slog.Info("Prediction confirmed", "event", event.Title)
		events.Record(events.TypeBetPlaced, streamer.Username, fmt.Sprintf("bet %d points on %q", amount, event.Title))

	case "prediction-result":
		p.mu.RLock()
		confirmed := event.BetConfirmed
		p.mu.RUnlock()
		if !confirmed {
			return
		}

		result, ok := prediction["result"].(map[string]interface{})
		if !ok {
			return
		}

		p.mu.Lock()
		// The raw stake must be read before ParseResult, which zeroes `placed`
		// for a REFUND; ROI analytics still want to know a stake was put up.
		stake := event.Bet.Decision.Amount
		strategy := string(event.Bet.Settings.Strategy)
		odds := chosenOutcomeOdds(event)
		manual := false
		if rc := p.control[eventID]; rc != nil {
			manual = rc.manualBet
		}
		placed, won, gained := event.ParseResult(result)
		resultType := event.Result.Type
		p.mu.Unlock()
		_ = placed
		_ = won

		if manual {
			strategy = "MANUAL"
		}

		slog.Info("Prediction result",
			"event", event.Title,
			"result", resultType,
			"gained", gained,
		)
		events.Record(events.TypeBetResult, streamer.Username, fmt.Sprintf("%s %+d points on %q", resultType, gained, event.Title))

		streamer.UpdateHistory("PREDICTION", gained)

		switch resultType {
		case models.ResultRefund:
			streamer.UpdateHistoryWithCounter("REFUND", -placed, -1)
		case models.ResultWin:
			streamer.UpdateHistoryWithCounter("PREDICTION", -won, -1)
		}

		// Emit the settled bet for ROI analytics. Done outside the pool lock (no
		// SQLite call under mu); the handler is the miner's analytics recorder.
		if p.onBetResult != nil {
			p.onBetResult(BetResult{
				EventID:    eventID,
				Streamer:   streamer.Username,
				Timestamp:  time.Now(),
				Strategy:   strategy,
				ResultType: string(resultType),
				Placed:     stake,
				Won:        won,
				Gained:     gained,
				Odds:       odds,
				Manual:     manual,
			})
		}

		// The round is over; drop its tracked + transient state promptly.
		p.scheduleCleanup(eventID, terminalCleanupGrace)
	}
}

// placeAutoBet runs the scheduled auto-bet for a single round. It mirrors the
// manual path's locking exactly — the round's placeMu is held across the Twitch
// call, and Decision/Outcomes/BetPlaced are only ever touched under the pool mu
// — so a manual bet and this auto-bet can never both reach Twitch, and it
// honours the per-round suppression set by a manual bet or the manual skip
// toggle. The betting *strategy* is unchanged from before: Calculate + Skip
// from models, the same 10-point minimum, the same logs.
func (p *WebSocketPool) placeAutoBet(eventID string) {
	p.mu.RLock()
	event := p.predictions[eventID]
	rc := p.control[eventID]
	p.mu.RUnlock()
	if event == nil || rc == nil {
		return
	}

	rc.placeMu.Lock()
	defer rc.placeMu.Unlock()

	// Decide + gate under the pool lock; the network call is the only thing done
	// outside it (serialized instead by placeMu).
	p.mu.Lock()
	if rc.autoBetSkip || event.BetPlaced || event.Status != models.PredictionActive {
		p.mu.Unlock()
		return
	}
	decision := event.Bet.Calculate(event.Streamer.GetChannelPoints())
	skip, comparedValue := event.Bet.Skip()
	p.mu.Unlock()

	if decision.Amount < minPredictionBet {
		slog.Info("Bet amount too low", "amount", decision.Amount)
		return
	}
	if skip {
		slog.Info("Skipping bet", "filter", event.Bet.Settings.FilterCondition, "value", comparedValue)
		return
	}

	slog.Info("Placing prediction bet",
		"event", event.Title,
		"choice", decision.Choice,
		"amount", decision.Amount,
	)

	if err := p.placer.PlacePredictionBet(event, decision.ID, decision.Amount); err != nil {
		slog.Error("Failed to make prediction", "error", err)
		return
	}

	p.mu.Lock()
	event.BetPlaced = true
	p.mu.Unlock()
}

// PlaceManualBet places a dashboard-initiated bet on a specific outcome of a
// specific round after re-verifying everything server-side. It never trusts
// client-supplied balance/status/odds: the round, its status, the outcome, the
// open window, the live balance and the "no existing bet" invariant are all
// re-checked here, immediately before the Twitch call, under the round's
// placement lock. On success the round is marked so auto-bet skips it. Returns
// the chosen outcome's title for the confirmation message.
func (p *WebSocketPool) PlaceManualBet(eventID, outcomeID string, amount int) (string, error) {
	p.mu.RLock()
	event := p.predictions[eventID]
	rc := p.control[eventID]
	p.mu.RUnlock()
	if event == nil || rc == nil {
		return "", ErrPredictionNotFound
	}

	if amount <= 0 {
		return "", ErrInvalidAmount
	}
	if amount < minPredictionBet {
		return "", ErrAmountTooLow
	}

	outcomeIdx, outcomeTitle := p.findOutcome(event, outcomeID)
	if outcomeIdx < 0 {
		return "", ErrOutcomeNotFound
	}

	// Fast pre-check + double-submit guard, holding no lock across the network.
	p.mu.Lock()
	switch {
	case event.BetPlaced && rc.manualBet:
		p.mu.Unlock()
		return "", ErrAlreadyBet
	case event.BetPlaced:
		p.mu.Unlock()
		return "", ErrAutoBetPlaced
	case rc.manualPending:
		p.mu.Unlock()
		return "", ErrManualBetInFlight
	}
	rc.manualPending = true
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		rc.manualPending = false
		p.mu.Unlock()
	}()

	// Serialize against the scheduled auto-bet across the Twitch call.
	rc.placeMu.Lock()
	defer rc.placeMu.Unlock()

	// Re-validate against fresh state now the placement lock is held.
	p.mu.RLock()
	betPlaced := event.BetPlaced
	manualByUs := rc.manualBet
	status := event.Status
	online := event.Streamer.GetIsOnline()
	balance := event.Streamer.GetChannelPoints()
	closing := event.ClosingBetAfter(time.Now())
	p.mu.RUnlock()

	switch {
	case betPlaced && manualByUs:
		return "", ErrAlreadyBet
	case betPlaced:
		return "", ErrAutoBetPlaced
	case status != models.PredictionActive:
		return "", ErrRoundClosed
	case closing <= 0:
		return "", ErrRoundClosed
	case !online:
		return "", ErrStreamerOffline
	case amount > balance:
		return "", ErrInsufficientPoints
	}

	if err := p.placer.PlacePredictionBet(event, outcomeID, amount); err != nil {
		human := humanizeBetError(err)
		p.mu.Lock()
		rc.manualErr = human
		p.mu.Unlock()
		return "", errors.New(human)
	}

	p.mu.Lock()
	event.BetPlaced = true
	event.Bet.Decision = models.Decision{Choice: outcomeIdx, Amount: amount, ID: outcomeID}
	rc.manualBet = true
	rc.autoBetSkip = true
	rc.manualErr = ""
	p.mu.Unlock()

	events.Record(events.TypeBetPlaced, event.Streamer.Username, fmt.Sprintf("manual bet %d points on %q", amount, event.Title))
	slog.Info("Manual prediction bet placed",
		"streamer", event.Streamer.Username,
		"event", event.Title,
		"amount", amount,
	)
	return outcomeTitle, nil
}

// SetAutoBetSkip toggles per-round auto-bet suppression without placing a bet.
// It only affects this one round: the flag is cleared when the round is cleaned
// up, so the streamer's next prediction is handled by the normal auto-bet path,
// and it never touches global or persisted settings. Un-skipping is allowed
// while the round is still open and no bet has been placed.
func (p *WebSocketPool) SetAutoBetSkip(eventID string, skip bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	event, ok := p.predictions[eventID]
	rc := p.control[eventID]
	if !ok || rc == nil {
		return ErrPredictionNotFound
	}
	if event.BetPlaced {
		if rc.manualBet {
			return ErrAlreadyBet
		}
		return ErrAutoBetPlaced
	}
	if event.Status != models.PredictionActive {
		return ErrRoundClosed
	}
	rc.autoBetSkip = skip
	return nil
}

// findOutcome returns the index and title of the outcome with the given id
// within this round, or (-1, "") when it is not part of this round — which is
// how a stale or foreign outcome id is rejected.
func (p *WebSocketPool) findOutcome(event *models.EventPrediction, outcomeID string) (int, string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for i, o := range event.Bet.Outcomes {
		if o != nil && o.ID == outcomeID {
			return i, o.Title
		}
	}
	return -1, ""
}

// scheduleCleanup removes a finished round's tracked + transient state after a
// grace delay. Fire-and-forget, matching the existing auto-bet timer pattern;
// removePrediction is idempotent so overlapping schedules are harmless.
func (p *WebSocketPool) scheduleCleanup(eventID string, after time.Duration) {
	go func() {
		time.Sleep(after)
		p.removePrediction(eventID)
	}()
}

// removePrediction deletes a round's prediction and its round-control entry.
// Idempotent.
func (p *WebSocketPool) removePrediction(eventID string) {
	p.mu.Lock()
	delete(p.predictions, eventID)
	delete(p.control, eventID)
	p.mu.Unlock()
}

// sweepStaleLocked drops any tracked round older than maxPredictionAge. The
// caller must hold p.mu for writing. This is a leak backstop for the rare case
// where a round's terminal event never arrives; the normal path is
// scheduleCleanup on resolve/cancel.
func (p *WebSocketPool) sweepStaleLocked() {
	now := time.Now()
	for id, e := range p.predictions {
		if now.Sub(e.CreatedAt) > maxPredictionAge {
			delete(p.predictions, id)
			delete(p.control, id)
		}
	}
}

// humanizeBetError converts a PlacePredictionBet failure into a user-safe
// message, never surfacing a raw Go error or Twitch response to the dashboard.
func humanizeBetError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, api.ErrUnauthorized) {
		return "Twitch rejected the request because the session expired — reauthorize the miner"
	}
	if errors.Is(err, api.ErrPersistedQueryNotFound) {
		// Retrying cannot help here: every known client ID was already tried and
		// the outage lasts until Twitch's rotated query hashes are updated, so
		// the message must not suggest "try again" like the default branch does.
		return "Twitch is temporarily rejecting the miner's requests (stale query metadata) — the bet was not placed"
	}
	msg := strings.ToUpper(err.Error())
	switch {
	case strings.Contains(msg, "NOT_ENOUGH_POINTS"), strings.Contains(msg, "INSUFFICIENT"):
		return ErrInsufficientPoints.Error()
	case strings.Contains(msg, "NOT_ACTIVE"), strings.Contains(msg, "LOCKED"), strings.Contains(msg, "CLOSED"), strings.Contains(msg, "BOUND"):
		return ErrRoundClosed.Error()
	case strings.Contains(msg, "MAX"), strings.Contains(msg, "MIN"):
		return "Twitch rejected the stake amount for this prediction"
	default:
		return "Twitch could not place the bet right now — please try again"
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
