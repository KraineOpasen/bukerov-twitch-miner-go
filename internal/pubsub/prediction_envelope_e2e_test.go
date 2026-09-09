package pubsub_test

// END-TO-END proof of the P1.5 decision-capture contract.
//
// Every other envelope test in this change stops at one seam: the producer
// tests end at an in-memory sink, the store tests begin at a hand-built
// payload. Neither can detect the failure that matters most — a field that the
// producer records, the adapter forgets, and the store therefore never sees.
// A hand-built payload written and read back would prove only that the store
// round-trips a struct it was handed, which is not the claim being made.
//
// This file drives a REAL automatic betting decision through the REAL
// production path: the pool's producer, the miner's translation adapter, the
// analytics collector's single writer, SQLite, and the public observation
// reader. It lives in the external `pubsub_test` package because that is the
// only package that may import both `internal/miner` (which imports pubsub)
// and `internal/analytics`.

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/miner"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/pubsub"

	_ "modernc.org/sqlite"
)

// capturedRound is everything one end-to-end drive produced, read back out of
// SQLite through the public reader.
type capturedRound struct {
	records []analytics.ObservationRecord
	reading analytics.ObservationSessionReading
}

// decisionFact returns the single auto_decision fact that carries an envelope.
func (c capturedRound) decisionFact(t *testing.T) analytics.ObservationRecord {
	t.Helper()
	var found []analytics.ObservationRecord
	for _, r := range c.records {
		if r.Payload.DecisionEnvelope != nil {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d facts carry a decision envelope, want exactly 1: an attempt that files "+
			"its inputs more than once, or not at all, cannot be replayed as one decision", len(found))
	}
	return found[0]
}

// driveCapturedRound runs one real auto-bet decision all the way into SQLite
// and reads it back.
//
// The collector is closed rather than polled: Close fences intake, drains
// what is queued, joins the writer goroutine and finalizes the session, so the
// read below sees a settled session rather than a race with the writer.
func driveCapturedRound(
	t *testing.T,
	balance int,
	risk config.PredictionRiskSettings,
	settings models.BetSettings,
	outcomes []interface{},
	update []interface{},
	gate pubsub.BetHealthGate,
) (capturedRound, *pubsub.TestAutoBetHarness) {
	t.Helper()
	skipUnderRaceDetector(t)

	svc, _ := newCapturingService(t)

	h := pubsub.NewTestAutoBetHarness("pool-e2e", balance)
	if gate != nil {
		h.Pool().SetBetHealthGate(gate)
	}
	h.SetRisk(risk)

	// THE REAL ADAPTER. Not a stand-in: this is the same value the miner wires
	// onto a live pool, so a field it fails to translate is a field this test
	// fails on.
	settle := h.Pool().SetPredictionObservationSink(miner.NewPredictionObservationSink(svc))

	h.AdmitRound("e2e-1", settings, outcomes)
	if len(update) > 0 {
		h.UpdateOutcomes("e2e-1", update)
	}
	incarnation := h.RoundIncarnation("e2e-1")

	h.DriveAutoBet("e2e-1")

	settle(nil)
	if err := svc.Close(); err != nil {
		t.Fatalf("close analytics: %v", err)
	}

	repo, ok := svc.Repository().(*analytics.SQLiteRepository)
	if !ok {
		t.Fatal("analytics repository is not the SQLite implementation")
	}
	ctx := context.Background()
	records, err := repo.ObservationsByRound(ctx, incarnation)
	if err != nil {
		t.Fatalf("read observations: %v", err)
	}
	var reading analytics.ObservationSessionReading
	if len(records) > 0 {
		r, _, err := repo.ReadObservationSession(ctx, records[0].CollectorEpoch)
		if err != nil {
			t.Fatalf("read session: %v", err)
		}
		reading = r
	}
	if len(records) == 0 {
		t.Fatal("the decision produced no stored facts at all: the capture path is broken end to end")
	}
	// A drop here is not a flaky test, it is the collector telling us the host
	// could not commit a fact inside the production write deadline. Saying so
	// explicitly is what keeps a lost fact from reading as a passing run.
	if reading.Session.DroppedCount != 0 {
		t.Fatalf("the collector dropped %d fact(s); the session is %q. A dropped fact leaves a "+
			"causal gap, so this run cannot witness the capture contract",
			reading.Session.DroppedCount, reading.Session.CloseState)
	}
	return capturedRound{records: records, reading: reading}, h
}

// skipUnderRaceDetector stops a fully integrated run in a build where the
// collector's production write deadline cannot be met. See the
// raceDetectorEnabled build-tag pair for the measurement and what it costs.
func skipUnderRaceDetector(t *testing.T) {
	t.Helper()
	if raceDetectorEnabled {
		t.Skip("the collector's 5ms production write deadline is unmeetable under -race " +
			"(a single insert measures 5.6-9.1ms against it), so every fact is dropped before " +
			"anything can be asserted; run this file without -race")
	}
}

// newCapturingService opens a private database and starts a real analytics
// collector over it.
//
// The two PRAGMAs matter. The collector enforces a HARD 5 ms budget per fact
// (analytics.ObservationWriteDeadline) and never retries: a commit that misses
// it is a permanent, counted drop. With SQLite's default synchronous=FULL every
// insert fsyncs, which on an ordinary host exceeds that budget and drops every
// fact — so without this the test would observe nothing and blame the capture
// path. The analytics package's own tests solve the same problem by relaxing
// the deadline, which is unexported and unreachable from here.
//
// This tunes the HOST, not the code under test: the production deadline, the
// real collector, the real sanitizer and the real store are all still exercised.
func newCapturingService(t *testing.T) (*analytics.Service, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "miner.db")
	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	for _, pragma := range []string{"PRAGMA journal_mode=MEMORY", "PRAGMA synchronous=OFF"} {
		if _, err := sqlDB.Exec(pragma); err != nil {
			t.Fatalf("%s: %v", pragma, err)
		}
	}
	svc, err := analytics.NewService(&database.DB{DB: sqlDB}, filepath.Dir(path), 0)
	if err != nil {
		t.Fatalf("new analytics service: %v", err)
	}
	if err := svc.Start(); err != nil {
		t.Fatalf("start analytics: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	awaitCapturing(t, svc)
	return svc, sqlDB
}

// awaitCapturing blocks until the collector has finished bootstrapping and is
// actually accepting facts.
//
// Service.Start is deliberately nonblocking — the collector bootstraps on its
// own goroutine — and a fact offered before intake opens is not a drop: it took
// no causal position, so it is silently lost and the session still reads
// COMPLETE. A test that raced the bootstrap would therefore observe nothing and
// have no counter to prove why. PredictionCaptureState is the exported readiness
// signal the producer itself uses; "" means capture is fully active.
func awaitCapturing(t *testing.T, svc *analytics.Service) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if svc.PredictionCaptureState("chan-1", "streamer") == "" {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("the collector never opened intake; capture state = %q",
		svc.PredictionCaptureState("chan-1", "streamer"))
}

// twoOutcomes is the ordinary two-outcome round the betting fixtures use.
func twoOutcomes() []interface{} {
	return []interface{}{
		map[string]interface{}{"id": "o1", "title": "Yes", "total_points": float64(300), "total_users": float64(3)},
		map[string]interface{}{"id": "o2", "title": "No", "total_points": float64(200), "total_users": float64(2)},
	}
}

// TestARealDecisionsInputsSurviveTheWholeCapturePath is the end-to-end seam.
//
// It regresses the failure no single-seam test can see: a field the producer
// records and the miner's adapter silently fails to translate. Before P1.5 the
// stored auto_decision fact carried a stake and a balance and nothing else, so
// the decision could be read and never re-derived. A weaker test that built an
// analytics payload by hand and read it back would pass even with the adapter
// leg entirely missing.
func TestARealDecisionsInputsSurviveTheWholeCapturePath(t *testing.T) {
	settings := models.BetSettings{
		Strategy:      models.StrategySmartMoney,
		Percentage:    5,
		PercentageGap: 20,
		MaxPoints:     50000,
		MinimumPoints: 10,
		Delay:         6,
		DelayMode:     models.DelayModeFromEnd,
	}
	// top_predictors is the ONLY source of TopPoints, and SMART_MONEY selects
	// on it. Feeding it through UpdateOutcomes is what makes this the first
	// test in the repository that exercises that strategy at all.
	update := []interface{}{
		map[string]interface{}{"id": "o1", "total_points": float64(300), "total_users": float64(3),
			"top_predictors": []interface{}{
				map[string]interface{}{"points": float64(1200)},
				map[string]interface{}{"points": float64(5000)},
			}},
		map[string]interface{}{"id": "o2", "total_points": float64(200), "total_users": float64(2),
			"top_predictors": []interface{}{
				map[string]interface{}{"points": float64(90)},
			}},
	}

	got, h := driveCapturedRound(t, 100000,
		config.PredictionRiskSettings{HealthGateEnabled: false},
		settings, twoOutcomes(), update, nil)

	if h.PlacementCalls() != 1 {
		t.Fatalf("Twitch placement calls = %d, want exactly 1: capture must not add, remove or "+
			"retry a call", h.PlacementCalls())
	}

	rec := got.decisionFact(t)
	if rec.PayloadUndecodable {
		t.Fatal("the stored payload could not be decoded; the envelope is unreadable")
	}
	env := rec.Payload.DecisionEnvelope

	if env.AttemptID == 0 {
		t.Fatal("the envelope carries no attempt discriminator, so its facts cannot be linked to " +
			"the placement calls they produced")
	}
	if env.CalculateStage != analytics.DecisionStageExecuted ||
		env.SettingsStage != analytics.DecisionStageExecuted ||
		env.SkipStage != analytics.DecisionStageExecuted ||
		env.StakeStage != analytics.DecisionStageExecuted {
		t.Fatalf("a placed decision reports an unexecuted stage: %+v", env)
	}
	if env.HealthStage != analytics.DecisionHealthDisabled {
		t.Fatalf("health stage = %q, want %q: the gate was switched off, which is not the same "+
			"fact as a gate that ran and allowed the bet", env.HealthStage, analytics.DecisionHealthDisabled)
	}

	// The nine settings fields, through every leg.
	s := env.Settings
	if s == nil {
		t.Fatal("the decision recorded no settings: the strategy it ran is unknowable")
	}
	if s.Strategy != "SMART_MONEY" || s.Percentage != 5 || s.PercentageGap != 20 ||
		s.MaxPoints != 50000 || s.MinimumPoints != 10 || s.StealthMode ||
		s.Delay != 6 || s.DelayMode != "FROM_END" {
		t.Fatalf("settings did not survive the capture path: %+v", s)
	}
	if s.FilterCondition != nil {
		t.Fatalf("a round with no filter condition stored one: %+v", s.FilterCondition)
	}

	// The model state, including the derived values and the TopPoints
	// aggregate that no wire projection carries.
	if len(env.Outcomes) != 2 {
		t.Fatalf("outcomes = %d, want 2", len(env.Outcomes))
	}
	o1, o2 := env.Outcomes[0], env.Outcomes[1]
	if !o1.Present || !o2.Present {
		t.Fatalf("an outcome the model held was stored as absent: %+v %+v", o1, o2)
	}
	if o1.ID != "o1" || o2.ID != "o2" || o1.Slot != 0 || o2.Slot != 1 {
		t.Fatalf("outcome identity or ordering was lost: %+v %+v", o1, o2)
	}
	if o1.TopPoints != 5000 || o2.TopPoints != 90 {
		t.Fatalf("TopPoints = %d/%d, want 5000/90: this is the aggregate SMART_MONEY selects on "+
			"and the one value no wire projection can supply", o1.TopPoints, o2.TopPoints)
	}
	if o1.TotalPoints != 300 || o1.TotalUsers != 3 || o2.TotalPoints != 200 || o2.TotalUsers != 2 {
		t.Fatalf("outcome aggregates were lost: %+v %+v", o1, o2)
	}
	// Derived values are recorded as the model held them, not recomputed.
	if o1.Odds != 1.67 || o2.Odds != 2.5 {
		t.Fatalf("odds = %v/%v, want 1.67/2.5 exactly as the model derived them", o1.Odds, o2.Odds)
	}
	if o1.PercentageUsers != 60 || o2.PercentageUsers != 40 {
		t.Fatalf("percentageUsers = %v/%v, want 60/40", o1.PercentageUsers, o2.PercentageUsers)
	}

	if env.Balance == nil || *env.Balance != 100000 {
		t.Fatalf("balance = %v, want 100000: without it no stake can be re-derived", env.Balance)
	}
	if env.BetTotalUsers == nil || *env.BetTotalUsers != 5 ||
		env.BetTotalPoints == nil || *env.BetTotalPoints != 500 {
		t.Fatalf("bet-level aggregates lost: users=%v points=%v", env.BetTotalUsers, env.BetTotalPoints)
	}

	// SMART_MONEY picks the outcome with the highest TopPoints — slot 0.
	if env.ChoiceIndex == nil || *env.ChoiceIndex != 0 {
		t.Fatalf("choice index = %v, want 0", env.ChoiceIndex)
	}
	if env.ChoiceOutcomeID != "o1" {
		t.Fatalf("choice outcome id = %q, want o1: the id the placement call actually carried",
			env.ChoiceOutcomeID)
	}
	if env.ChoiceAmount == nil || *env.ChoiceAmount != 5000 {
		t.Fatalf("choice amount = %v, want 5000 (5%% of 100000)", env.ChoiceAmount)
	}
	if env.SkipResult == nil || *env.SkipResult {
		t.Fatalf("skip result = %v, want a recorded false: 'the filter ran and passed' and 'the "+
			"filter never ran' were the same absence before this contract", env.SkipResult)
	}
	if env.FinalAmount == nil || *env.FinalAmount != 5000 {
		t.Fatalf("final amount = %v, want 5000", env.FinalAmount)
	}
	if env.ClampApplied == nil || *env.ClampApplied {
		t.Fatalf("clamp applied = %v, want a recorded false", env.ClampApplied)
	}

	// The placement call that actually went to Twitch carries the same attempt
	// discriminator, so the call is linked to the envelope that produced its
	// arguments.
	var placements int
	for _, r := range got.records {
		if r.Kind != analytics.KindPlacement {
			continue
		}
		placements++
		if r.Payload.Counters["autoAttemptId"] != int64(env.AttemptID) {
			t.Fatalf("placement fact %q carries attempt %d, want %d: without the link the call "+
				"cannot be attributed to the decision that produced it",
				r.Payload.Phase, r.Payload.Counters["autoAttemptId"], env.AttemptID)
		}
	}
	if placements != 2 {
		t.Fatalf("placement facts = %d, want 2 (CALL_STARTED and CALL_RETURNED)", placements)
	}

	gotID, gotAmt := h.LastPlacement()
	if gotID != "o1" || gotAmt != 5000 {
		t.Fatalf("Twitch was called with (%q, %d); the envelope claims (%q, %d)",
			gotID, gotAmt, env.ChoiceOutcomeID, *env.ChoiceAmount)
	}
}

// TestStoredInputsAreImmuneToLaterSettingsAndOutcomeChanges regresses the
// aliasing defect: capturing a pointer or a slice by reference rather than by
// value, so that editing the streamer's settings, updating the round's
// outcomes, or replacing the round afterwards silently rewrites a fact that was
// already recorded.
//
// A naive implementation passes every other test in this change and fails only
// here, because nothing else mutates the model after the decision.
func TestStoredInputsAreImmuneToLaterSettingsAndOutcomeChanges(t *testing.T) {
	filter := &models.FilterCondition{
		By: models.OutcomeTotalUsers, Where: models.ConditionGT, Value: 1,
	}
	settings := models.BetSettings{
		Strategy:        models.StrategyNumber1,
		Percentage:      5,
		MaxPoints:       50000,
		FilterCondition: filter,
		Delay:           6,
		DelayMode:       models.DelayModeFromEnd,
	}

	skipUnderRaceDetector(t)
	svc, _ := newCapturingService(t)

	h := pubsub.NewTestAutoBetHarness("pool-mutate", 100000)
	h.SetRisk(config.PredictionRiskSettings{HealthGateEnabled: false})
	settle := h.Pool().SetPredictionObservationSink(miner.NewPredictionObservationSink(svc))
	h.AdmitRound("mut-1", settings, twoOutcomes())
	incarnation := h.RoundIncarnation("mut-1")

	h.DriveAutoBet("mut-1")

	// Everything the decision read is now mutated behind its back, exactly as a
	// live settings apply and an event-updated frame would do.
	filter.Value = 999999
	filter.By = models.OutcomeTopPoints
	filter.Where = models.ConditionLTE
	h.UpdateOutcomes("mut-1", []interface{}{
		map[string]interface{}{"id": "o1", "total_points": float64(999999), "total_users": float64(4242),
			"top_predictors": []interface{}{map[string]interface{}{"points": float64(777)}}},
		map[string]interface{}{"id": "o2", "total_points": float64(1), "total_users": float64(1)},
	})
	// And the round is replaced by a fresh admission of the same Twitch event.
	h.AdmitRound("mut-1", models.BetSettings{Strategy: models.StrategyHighOdds, Percentage: 99}, twoOutcomes())

	settle(nil)
	if err := svc.Close(); err != nil {
		t.Fatalf("close analytics: %v", err)
	}
	repo := svc.Repository().(*analytics.SQLiteRepository)
	records, err := repo.ObservationsByRound(context.Background(), incarnation)
	if err != nil {
		t.Fatalf("read observations: %v", err)
	}
	got := capturedRound{records: records}
	env := got.decisionFact(t).Payload.DecisionEnvelope

	if env.Settings.FilterCondition == nil {
		t.Fatal("the recorded filter condition vanished")
	}
	fc := env.Settings.FilterCondition
	if fc.Value != 1 || fc.By != "total_users" || fc.Where != "GT" {
		t.Fatalf("the recorded filter condition followed a later edit: %+v — the envelope aliased "+
			"the live pointer instead of deep-copying it, so the stored input is no longer the one "+
			"the decision used", fc)
	}
	if env.Settings.Strategy != "NUMBER_1" || env.Settings.Percentage != 5 {
		t.Fatalf("the recorded settings followed the round replacement: %+v", env.Settings)
	}
	if env.Outcomes[0].TotalPoints != 300 || env.Outcomes[0].TotalUsers != 3 {
		t.Fatalf("the recorded outcome state followed a later frame: %+v — the envelope kept a "+
			"live model reference instead of copying the values it read", env.Outcomes[0])
	}
	if env.Outcomes[0].TopPoints != 0 {
		t.Fatalf("TopPoints = %d, want 0: the decision ran before any top_predictors arrived, and "+
			"a later frame must not backfill an input it never had", env.Outcomes[0].TopPoints)
	}
}

// TestTheCaptureContractIsSufficientToRederiveTheDecision is the sufficiency
// proof the P1.5 readiness gate asked for.
//
// It is a TEST-ONLY consumer: it re-derives each decision using ONLY the stored
// envelope — no live config, no network, no global RNG, and crucially no use of
// the recorded choice or stake as an input, which would be circular. The
// recorded results are the comparison TARGET, never the oracle. It deliberately
// does not implement a general evaluator; it covers the declared cases and
// stops.
func TestTheCaptureContractIsSufficientToRederiveTheDecision(t *testing.T) {
	for _, tc := range []struct {
		name     string
		strategy models.Strategy
		update   []interface{}
		wantSlot int
	}{
		{
			name: "the fixed-index strategy", strategy: models.StrategyNumber1, wantSlot: 0,
		},
		{
			name: "the most-voted strategy", strategy: models.StrategyMostVoted, wantSlot: 0,
		},
		{
			// o2 has fewer points, so the pool pays it better odds — but only
			// once a frame has been through UpdateOutcomes, which is what
			// derives Odds at all. Without one both odds are 0 and the legacy
			// argmax returns slot 0, so the frame is what makes this case
			// exercise the strategy rather than the tie rule.
			name: "the high-odds strategy", strategy: models.StrategyHighOdds, wantSlot: 1,
			update: []interface{}{
				map[string]interface{}{"id": "o1", "total_points": float64(300), "total_users": float64(3)},
				map[string]interface{}{"id": "o2", "total_points": float64(200), "total_users": float64(2)},
			},
		},
		{
			name: "the smart-money strategy", strategy: models.StrategySmartMoney, wantSlot: 1,
			update: []interface{}{
				map[string]interface{}{"id": "o1", "total_points": float64(300), "total_users": float64(3),
					"top_predictors": []interface{}{map[string]interface{}{"points": float64(10)}}},
				map[string]interface{}{"id": "o2", "total_points": float64(200), "total_users": float64(2),
					"top_predictors": []interface{}{map[string]interface{}{"points": float64(9999)}}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := models.BetSettings{
				Strategy: tc.strategy, Percentage: 5, PercentageGap: 20,
				MaxPoints: 50000, Delay: 6, DelayMode: models.DelayModeFromEnd,
			}
			got, _ := driveCapturedRound(t, 100000,
				config.PredictionRiskSettings{HealthGateEnabled: false},
				settings, twoOutcomes(), tc.update, nil)
			env := got.decisionFact(t).Payload.DecisionEnvelope

			slot, amount, ok := rederiveFromEnvelope(env)
			if !ok {
				t.Fatalf("the stored envelope is not sufficient to re-derive strategy %q", tc.strategy)
			}
			if slot != tc.wantSlot {
				t.Fatalf("re-derived slot %d, want %d", slot, tc.wantSlot)
			}
			if env.ChoiceIndex == nil || slot != *env.ChoiceIndex {
				t.Fatalf("re-derived slot %d does not match the recorded choice %v: the captured "+
					"inputs do not reproduce the decision they claim to explain", slot, env.ChoiceIndex)
			}
			if env.ChoiceAmount == nil || amount != *env.ChoiceAmount {
				t.Fatalf("re-derived stake %d does not match the recorded %v", amount, env.ChoiceAmount)
			}
		})
	}
}

// rederiveFromEnvelope recomputes a decision from the stored inputs ALONE.
//
// It reads only Settings, Outcomes and Balance. It never reads ChoiceIndex,
// ChoiceOutcomeID, ChoiceAmount, FinalAmount or any gate result — those are
// what it is checked against. It returns ok=false for a case whose inputs the
// envelope does not claim to carry, rather than guessing.
func rederiveFromEnvelope(env *analytics.ObservationDecisionEnvelope) (slot int, amount int64, ok bool) {
	if env == nil || env.Settings == nil || env.Balance == nil ||
		env.CalculateStage != analytics.DecisionStageExecuted || len(env.Outcomes) == 0 {
		return 0, 0, false
	}
	value := func(o analytics.ObservationModelOutcome, key string) float64 {
		switch key {
		case "total_users":
			return float64(o.TotalUsers)
		case "total_points":
			return float64(o.TotalPoints)
		case "top_points":
			return float64(o.TopPoints)
		case "odds":
			return o.Odds
		case "odds_percentage":
			return o.OddsPercentage
		case "percentage_users":
			return o.PercentageUsers
		}
		return 0
	}
	// argmax with a strict >, so the FIRST maximum wins — the legacy tie
	// behaviour, preserved rather than improved.
	argmax := func(key string) int {
		best := 0
		for i := 1; i < len(env.Outcomes); i++ {
			if value(env.Outcomes[i], key) > value(env.Outcomes[best], key) {
				best = i
			}
		}
		return best
	}
	switch env.Settings.Strategy {
	case "NUMBER_1":
		slot = 0
	case "MOST_VOTED":
		slot = argmax("total_users")
	case "HIGH_ODDS":
		slot = argmax("odds")
	case "PERCENTAGE":
		slot = argmax("odds_percentage")
	case "SMART_MONEY":
		slot = argmax("top_points")
	case "SMART":
		if len(env.Outcomes) < 2 {
			return 0, 0, false
		}
		if math.Abs(env.Outcomes[0].PercentageUsers-env.Outcomes[1].PercentageUsers) <
			float64(env.Settings.PercentageGap) {
			slot = argmax("odds")
		} else {
			slot = argmax("total_users")
		}
	default:
		return 0, 0, false
	}
	// Stealth is deliberately not re-derived here: its transformation depends
	// on a draw the envelope does not and must not carry.
	if env.Settings.StealthMode {
		return 0, 0, false
	}
	amount = int64(float64(*env.Balance) * (float64(env.Settings.Percentage) / 100))
	if amount > int64(env.Settings.MaxPoints) {
		amount = int64(env.Settings.MaxPoints)
	}
	return slot, amount, true
}

// TestObservationNeverChangesTheDecisionItObserves regresses the one failure a
// capture change can cause that no envelope assertion would catch: the observer
// altering the business path it is supposed to watch.
//
// The same round is driven twice — once with the real capture path attached and
// once with no sink at all — and the placement call, its arguments and the
// round's own decision state must be identical. A capture that re-read the
// settings, called Calculate a second time, or reordered a gate would diverge
// here and nowhere else.
func TestObservationNeverChangesTheDecisionItObserves(t *testing.T) {
	settings := models.BetSettings{
		Strategy: models.StrategyMostVoted, Percentage: 7, PercentageGap: 20,
		MaxPoints: 50000, Delay: 6, DelayMode: models.DelayModeFromEnd,
	}
	risk := config.PredictionRiskSettings{MaxStakePercent: 10, HealthGateEnabled: false}

	// Observed.
	observed, hObserved := driveCapturedRound(t, 100000, risk, settings, twoOutcomes(), nil, nil)
	_ = observed

	// Unobserved: the same fixture with no sink wired, which is what every
	// pre-P1.5 deployment and every existing betting test runs.
	hPlain := pubsub.NewTestAutoBetHarness("pool-plain", 100000)
	hPlain.SetRisk(risk)
	hPlain.AdmitRound("e2e-1", settings, twoOutcomes())
	hPlain.DriveAutoBet("e2e-1")

	if hObserved.PlacementCalls() != hPlain.PlacementCalls() {
		t.Fatalf("observed run made %d Twitch calls, unobserved made %d: capture changed the "+
			"number of calls reaching Twitch", hObserved.PlacementCalls(), hPlain.PlacementCalls())
	}
	gotID, gotAmt := hObserved.LastPlacement()
	wantID, wantAmt := hPlain.LastPlacement()
	if gotID != wantID || gotAmt != wantAmt {
		t.Fatalf("observed run placed (%q, %d), unobserved placed (%q, %d): capture changed the "+
			"arguments that reach Twitch", gotID, gotAmt, wantID, wantAmt)
	}
}

// TestAStealthDecisionRecordsItsInputsWithoutAnObserverDraw regresses the
// entropy defect: an observer that re-ran Calculate, seeded or captured the
// global RNG, or reimplemented the stealth transformation to "complete" the
// record.
//
// Stealth reduces the stake by a random 1..5 below the chosen outcome's
// TopPoints, so the stake is not reproducible from the inputs. The envelope
// must therefore record the complete INPUTS and the ORIGINAL result, and never
// a draw. The test asserts the recorded amount is consistent with the observed
// transformation over its whole range rather than pinning one random value, and
// that exactly one Twitch call happened — a second Calculate would redraw and
// the two amounts would disagree.
func TestAStealthDecisionRecordsItsInputsWithoutAnObserverDraw(t *testing.T) {
	settings := models.BetSettings{
		Strategy: models.StrategyNumber1, Percentage: 90, MaxPoints: 50000,
		StealthMode: true, Delay: 6, DelayMode: models.DelayModeFromEnd,
	}
	update := []interface{}{
		map[string]interface{}{"id": "o1", "total_points": float64(300), "total_users": float64(3),
			"top_predictors": []interface{}{map[string]interface{}{"points": float64(4000)}}},
		map[string]interface{}{"id": "o2", "total_points": float64(200), "total_users": float64(2)},
	}
	got, h := driveCapturedRound(t, 100000,
		config.PredictionRiskSettings{HealthGateEnabled: false},
		settings, twoOutcomes(), update, nil)

	env := got.decisionFact(t).Payload.DecisionEnvelope
	if env.Settings == nil || !env.Settings.StealthMode {
		t.Fatal("stealth mode was not recorded, so the transformation cannot be explained")
	}
	if len(env.Outcomes) == 0 || env.Outcomes[0].TopPoints != 4000 {
		t.Fatalf("the stealth ceiling was not recorded: %+v", env.Outcomes)
	}
	if env.ChoiceAmount == nil {
		t.Fatal("no original stake recorded")
	}
	// 90% of 100000 is 90000, which is >= TopPoints, so stealth fired:
	// amount = 4000 - reduce, reduce in [1,5).
	amount := *env.ChoiceAmount
	if amount <= 3995 || amount > 3999 {
		t.Fatalf("stealth stake = %d, want 4000-reduce with reduce in [1,5): the recorded amount "+
			"is not the one the single Calculate call produced", amount)
	}
	// The effective transformation is DERIVABLE from the recorded inputs and
	// the recorded original result. That derivation is not a captured draw, and
	// it is only valid for exactly this input and choice.
	effective := int64(env.Outcomes[0].TopPoints) - amount
	if effective < 1 || effective > 4 {
		t.Fatalf("derived stealth reduction = %d, outside the algorithm's range", effective)
	}
	if h.PlacementCalls() != 1 {
		t.Fatalf("Twitch calls = %d, want 1: a second Calculate would have redrawn the stake",
			h.PlacementCalls())
	}
	gotID, gotAmt := h.LastPlacement()
	if gotID != "o1" || int64(gotAmt) != amount {
		t.Fatalf("Twitch received (%q, %d) but the envelope recorded amount %d: the observer "+
			"recorded a different draw than the one that was placed", gotID, gotAmt, amount)
	}
}

// TestAStealthDecisionFollowedByAFilterSkipStillRecordsItsInputs covers the
// combination the capture contract calls out: stealth transforms the stake and
// the filter then rejects the bet, so nothing is placed. The envelope must
// still carry the complete inputs and the original stealth-transformed stake —
// a skip is not an excuse to file an empty record.
func TestAStealthDecisionFollowedByAFilterSkipStillRecordsItsInputs(t *testing.T) {
	settings := models.BetSettings{
		Strategy: models.StrategyNumber1, Percentage: 90, MaxPoints: 50000,
		StealthMode: true, Delay: 6, DelayMode: models.DelayModeFromEnd,
		FilterCondition: &models.FilterCondition{
			By: models.OutcomeTotalUsers, Where: models.ConditionGT, Value: 1e9,
		},
	}
	update := []interface{}{
		map[string]interface{}{"id": "o1", "total_points": float64(300), "total_users": float64(3),
			"top_predictors": []interface{}{map[string]interface{}{"points": float64(4000)}}},
		map[string]interface{}{"id": "o2", "total_points": float64(200), "total_users": float64(2)},
	}
	got, h := driveCapturedRound(t, 100000,
		config.PredictionRiskSettings{HealthGateEnabled: false},
		settings, twoOutcomes(), update, nil)

	if h.PlacementCalls() != 0 {
		t.Fatalf("Twitch calls = %d, want 0 for a filter-rejected bet", h.PlacementCalls())
	}
	rec := got.decisionFact(t)
	if rec.Payload.ReasonCode != "FILTER_REJECTED" {
		t.Fatalf("terminal reason = %q, want FILTER_REJECTED", rec.Payload.ReasonCode)
	}
	env := rec.Payload.DecisionEnvelope
	if env.SkipResult == nil || !*env.SkipResult {
		t.Fatalf("skip result = %v, want a recorded true", env.SkipResult)
	}
	if env.SkipCompared == nil {
		t.Fatal("the filter's compared value was not recorded, so the rejection cannot be checked")
	}
	if env.ChoiceAmount == nil || *env.ChoiceAmount <= 3995 || *env.ChoiceAmount > 3999 {
		t.Fatalf("the stealth-transformed stake was not recorded on a skipped bet: %v",
			env.ChoiceAmount)
	}
	if env.Settings.FilterCondition == nil ||
		env.Settings.FilterCondition.By != "total_users" ||
		env.Settings.FilterCondition.Where != "GT" ||
		env.Settings.FilterCondition.Value != 1e9 {
		t.Fatalf("the filter that rejected the bet was not recorded: %+v", env.Settings.FilterCondition)
	}
}

// TestACapturedRoundKeepsItsSessionAndRoundIdentity proves the envelope is
// anchored to the exact collector session, pool, round incarnation and attempt
// it belongs to, rather than being joinable only by timestamp or event id — the
// two coordinates a replaced round reuses.
func TestACapturedRoundKeepsItsSessionAndRoundIdentity(t *testing.T) {
	settings := models.BetSettings{
		Strategy: models.StrategyNumber1, Percentage: 5, MaxPoints: 50000,
		Delay: 6, DelayMode: models.DelayModeFromEnd,
	}
	got, _ := driveCapturedRound(t, 100000,
		config.PredictionRiskSettings{HealthGateEnabled: false},
		settings, twoOutcomes(), nil, nil)

	rec := got.decisionFact(t)
	if rec.RoundIncarnationID == "" {
		t.Fatal("the decision fact carries no round incarnation, so it cannot be tied to one " +
			"local admission of the round")
	}
	if rec.PoolInstanceID != "pool-e2e" {
		t.Fatalf("pool provenance = %q, want pool-e2e", rec.PoolInstanceID)
	}
	if rec.CollectorSequence == 0 || rec.CollectorEpoch == 0 {
		t.Fatalf("the fact has no causal position: epoch %d seq %d",
			rec.CollectorEpoch, rec.CollectorSequence)
	}
	if got.reading.Session.ProducerRevision != analytics.ObservationProducerRevision {
		t.Fatalf("session revision = %q, want %q: a reader must be able to tell a session that "+
			"could carry an envelope from one that never could",
			got.reading.Session.ProducerRevision, analytics.ObservationProducerRevision)
	}
	// Every fact of the attempt shares one discriminator.
	want := int64(rec.Payload.DecisionEnvelope.AttemptID)
	var seen int
	for _, r := range got.records {
		id, hasID := r.Payload.Counters["autoAttemptId"]
		if !hasID {
			continue
		}
		seen++
		if id != want {
			t.Fatalf("fact %q/%q carries attempt %d, want %d",
				r.Kind, r.Payload.Phase, id, want)
		}
	}
	if seen < 4 {
		t.Fatalf("only %d facts carry the attempt discriminator; the due fact, the decision and "+
			"both placement calls all belong to the same attempt", seen)
	}
}
