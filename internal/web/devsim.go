package web

import (
	"errors"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"
)

// devPredictionsEnabled reports whether the dev-only prediction simulator is
// turned on. It is off by default and only activates when MINER_DEV_PREDICTIONS
// is set to a truthy value, so simulated rounds can never appear in a real run
// or be triggered accidentally.
func devPredictionsEnabled() bool {
	switch os.Getenv("MINER_DEV_PREDICTIONS") {
	case "1", "true", "TRUE", "yes", "on":
		return true
	default:
		return false
	}
}

// enableDevPredictions swaps in an in-memory simulator as the source of live
// predictions and the target of manual bets/skips, and registers the dev-only
// control routes. It deliberately REPLACES the real providers while active —
// this is a local testing surface, not something that ever ships on — and logs
// a loud warning so it is never mistaken for production behaviour.
func (s *Server) enableDevPredictions(mux *http.ServeMux) {
	s.mu.RLock()
	base := s.overviewProvider
	s.mu.RUnlock()

	sim := newDevPredictionSim(base)
	sim.seed()

	s.mu.Lock()
	s.overviewProvider = sim
	s.predictionControl = sim
	s.mu.Unlock()

	mux.HandleFunc("/api/dev/predictions/seed", sim.handleSeed)
	mux.HandleFunc("/api/dev/predictions/reset", sim.handleReset)
	mux.HandleFunc("/api/dev/predictions/fail", sim.handleFail)
	mux.HandleFunc("/api/dev/predictions/latency", sim.handleLatency)

	slog.Warn("DEV MODE: prediction simulator enabled (MINER_DEV_PREDICTIONS) — live predictions are FAKE and no real Twitch bets are placed")
}

// devOutcome / devRound model a single simulated prediction round entirely in
// memory. Status is derived from elapsed time so the full ACTIVE → LOCKED →
// RESOLVED lifecycle (and cleanup) happens without any background goroutine.
type devOutcome struct {
	id      string
	title   string
	color   string
	users   int
	points  int
	percent float64
	odds    float64
}

type devRound struct {
	eventID   string
	streamer  string
	title     string
	createdAt time.Time
	window    float64 // seconds until the round locks
	balance   int

	betPlaced  bool
	manualBet  bool
	betOutcome string // outcome id
	betAmount  int
	autoSkip   bool
	pending    bool
	lastErr    string
}

// devPredictionSim is the simulator. It implements OverviewProvider (delegating
// WatchSlots to the real provider so the sidebar still works) and
// PredictionControlProvider (validating exactly like the real pool, but placing
// against fake Twitch that can be told to add latency or fail).
type devPredictionSim struct {
	base OverviewProvider

	mu       sync.Mutex
	rounds   map[string]*devRound
	seq      int
	failNext bool
	latency  time.Duration
}

func newDevPredictionSim(base OverviewProvider) *devPredictionSim {
	return &devPredictionSim{
		base:    base,
		rounds:  make(map[string]*devRound),
		latency: 400 * time.Millisecond,
	}
}

const devLockGrace = 20 * time.Second // how long a round stays LOCKED before it is reaped

func (d *devPredictionSim) WatchSlots() WatchSlotsView {
	if d.base != nil {
		return d.base.WatchSlots()
	}
	return WatchSlotsView{Watching: map[string]bool{}}
}

// statusOf returns the time-derived status of a round and whether it should be
// reaped (fully resolved and past its grace).
func statusOf(r *devRound, now time.Time) (status string, reap bool) {
	locksAt := r.createdAt.Add(time.Duration(r.window) * time.Second)
	switch {
	case now.Before(locksAt):
		return "ACTIVE", false
	case now.Before(locksAt.Add(devLockGrace)):
		return "LOCKED", false
	default:
		return "RESOLVED", true
	}
}

// refreshLocked applies time-derived status transitions: it reaps finished
// rounds and lets the simulated auto-bet fire when its moment arrives (unless
// suppressed or a manual bet already landed). Caller holds d.mu.
func (d *devPredictionSim) refreshLocked(now time.Time) {
	for id, r := range d.rounds {
		status, reap := statusOf(r, now)
		if reap {
			delete(d.rounds, id)
			continue
		}
		if status != "ACTIVE" {
			continue
		}
		// Simulated auto-bet fires ~3s before lock.
		autoAt := r.createdAt.Add(time.Duration(r.window-3) * time.Second)
		if !now.Before(autoAt) && !r.betPlaced && !r.autoSkip && !r.pending {
			r.betPlaced = true
			r.manualBet = false
			r.betOutcome = r.firstOutcomeID()
			r.betAmount = 250
		}
	}
}

func (r *devRound) firstOutcomeID() string {
	if len(r.outcomes()) > 0 {
		return r.outcomes()[0].id
	}
	return ""
}

// outcomes returns the fixed outcome set for a round (kept small and stable for
// predictable dev testing). Encoded by title so a round is fully described by
// its seed.
func (r *devRound) outcomes() []devOutcome {
	return devOutcomeSets[r.title]
}

var devOutcomeSets = map[string][]devOutcome{
	"Will they clutch the round?": {
		{id: "o-yes", title: "Yes", color: "#387aff", users: 812, points: 41000, percent: 62, odds: 1.6},
		{id: "o-no", title: "No", color: "#ff4d6d", users: 498, points: 25000, percent: 38, odds: 2.6},
	},
	"How many kills this game?": {
		{id: "k-low", title: "0–10", color: "#387aff", users: 220, points: 9000, percent: 30, odds: 3.3},
		{id: "k-mid", title: "11–20", color: "#7a6ec4", users: 340, points: 15000, percent: 46, odds: 1.9},
		{id: "k-high", title: "21+", color: "#e0b074", users: 170, points: 6000, percent: 24, odds: 4.1},
	},
}

// LivePredictions renders the current simulated rounds as the board expects.
func (d *devPredictionSim) LivePredictions() []LivePrediction {
	now := time.Now()
	d.mu.Lock()
	d.refreshLocked(now)

	out := make([]LivePrediction, 0, len(d.rounds))
	for _, r := range d.rounds {
		status, _ := statusOf(r, now)
		if status != "ACTIVE" && status != "LOCKED" {
			continue
		}
		lp := LivePrediction{
			Streamer:                r.streamer,
			EventID:                 r.eventID,
			Title:                   r.title,
			Status:                  status,
			CreatedAt:               r.createdAt,
			PredictionWindowSeconds: r.window,
			BetPlaced:               r.betPlaced,
			BetConfirmed:            r.betPlaced,
			BetAmount:               r.betAmount,
			Online:                  true,
			Balance:                 r.balance,
			ManualBet:               r.manualBet,
			AutoBetSkipped:          r.autoSkip,
			ManualPending:           r.pending,
			ManualError:             r.lastErr,
		}
		total := 0
		for _, o := range r.outcomes() {
			total += o.points
		}
		lp.TotalPoints = total
		for _, o := range r.outcomes() {
			chosen := r.betPlaced && o.id == r.betOutcome
			if chosen {
				lp.BetOutcomeTitle = o.title
			}
			lp.Outcomes = append(lp.Outcomes, LivePredictionOutcome{
				ID:              o.id,
				Title:           o.title,
				Color:           o.color,
				PercentageUsers: o.percent,
				Odds:            o.odds,
				TotalPoints:     o.points,
				Chosen:          chosen,
			})
		}
		out = append(out, lp)
	}
	d.mu.Unlock()

	sort.SliceStable(out, func(i, j int) bool { return out[i].EventID < out[j].EventID })
	return out
}

// PlaceManualBet validates and places against fake Twitch, honouring the
// simulated latency and forced-failure switches so error/latency/race handling
// can be exercised locally.
func (d *devPredictionSim) PlaceManualBet(eventID, outcomeID string, amount int) (string, error) {
	now := time.Now()
	d.mu.Lock()
	d.refreshLocked(now)
	r := d.rounds[eventID]
	if r == nil {
		d.mu.Unlock()
		return "", errors.New("this prediction is no longer available")
	}
	if amount <= 0 {
		d.mu.Unlock()
		return "", errors.New("enter a positive amount")
	}
	if amount < 10 {
		d.mu.Unlock()
		return "", errors.New("the minimum bet is 10 points")
	}
	var title string
	for _, o := range r.outcomes() {
		if o.id == outcomeID {
			title = o.title
		}
	}
	if title == "" {
		d.mu.Unlock()
		return "", errors.New("that outcome is no longer available")
	}
	if status, _ := statusOf(r, now); status != "ACTIVE" {
		d.mu.Unlock()
		return "", errors.New("this prediction has already closed")
	}
	if r.betPlaced {
		defer d.mu.Unlock()
		if r.manualBet {
			return "", errors.New("a bet has already been placed on this prediction")
		}
		return "", errors.New("auto-bet already placed a bet on this prediction")
	}
	if r.pending {
		d.mu.Unlock()
		return "", errors.New("a bet is already being placed for this prediction")
	}
	if amount > r.balance {
		d.mu.Unlock()
		return "", errors.New("not enough channel points for that bet")
	}
	r.pending = true
	latency := d.latency
	fail := d.failNext
	d.failNext = false
	d.mu.Unlock()

	// Simulate the network round trip outside the lock.
	if latency > 0 {
		time.Sleep(latency)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	r.pending = false
	if fail {
		r.lastErr = "Twitch could not place the bet right now — please try again"
		return "", errors.New(r.lastErr)
	}
	// Re-check the round did not close while we were "talking to Twitch".
	if status, _ := statusOf(r, time.Now()); status != "ACTIVE" {
		return "", errors.New("this prediction has already closed")
	}
	if r.betPlaced {
		return "", errors.New("a bet has already been placed on this prediction")
	}
	r.betPlaced = true
	r.manualBet = true
	r.autoSkip = true
	r.betOutcome = outcomeID
	r.betAmount = amount
	r.balance -= amount
	r.lastErr = ""
	return title, nil
}

func (d *devPredictionSim) SetAutoBetSkip(eventID string, skip bool) error {
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.refreshLocked(now)
	r := d.rounds[eventID]
	if r == nil {
		return errors.New("this prediction is no longer available")
	}
	if r.betPlaced {
		if r.manualBet {
			return errors.New("a bet has already been placed on this prediction")
		}
		return errors.New("auto-bet already placed a bet on this prediction")
	}
	if status, _ := statusOf(r, now); status != "ACTIVE" {
		return errors.New("this prediction has already closed")
	}
	r.autoSkip = skip
	return nil
}

// seed installs the default fixture set: one 2-outcome round with a healthy
// balance and one 3-outcome round with a deliberately low balance (to exercise
// the insufficient-points path).
func (d *devPredictionSim) seed() {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	d.addLocked("dev-streamer", "Will they clutch the round?", 120, 50000, now)
	d.addLocked("dev-lowbal", "How many kills this game?", 90, 30, now)
}

func (d *devPredictionSim) addLocked(streamer, title string, window float64, balance int, now time.Time) {
	d.seq++
	id := "dev-evt-" + strconv.Itoa(d.seq)
	d.rounds[id] = &devRound{
		eventID:   id,
		streamer:  streamer,
		title:     title,
		createdAt: now,
		window:    window,
		balance:   balance,
	}
}

func (d *devPredictionSim) handleSeed(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeNotAllowed(w)
		return
	}
	d.seed()
	writeSuccess(w)
}

func (d *devPredictionSim) handleReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeNotAllowed(w)
		return
	}
	d.mu.Lock()
	d.rounds = make(map[string]*devRound)
	d.mu.Unlock()
	writeSuccess(w)
}

func (d *devPredictionSim) handleFail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeNotAllowed(w)
		return
	}
	on := r.URL.Query().Get("on") != "0"
	d.mu.Lock()
	d.failNext = on
	d.mu.Unlock()
	writeJSONOK(w, map[string]interface{}{"failNext": on})
}

func (d *devPredictionSim) handleLatency(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeNotAllowed(w)
		return
	}
	ms, _ := strconv.Atoi(r.URL.Query().Get("ms"))
	if ms < 0 {
		ms = 0
	}
	d.mu.Lock()
	d.latency = time.Duration(ms) * time.Millisecond
	d.mu.Unlock()
	writeJSONOK(w, map[string]interface{}{"latencyMs": ms})
}
