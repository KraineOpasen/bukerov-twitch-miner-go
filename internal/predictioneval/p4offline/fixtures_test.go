package p4offline_test

// SYNTHETIC FIXTURES.
//
// Every dataset built here is hand-constructed to isolate one seam decision.
// None of it was collected from Twitch, a production database or a live
// account, and nothing here can become a production fact by being evaluated.
// The shapes mirror what the pinned producer writes (one AUTO_DUE, one
// terminal envelope, CALL_STARTED/CALL_RETURNED pairs carrying the same stake
// and slot, a user_terminal with no attempt discriminator) so a seam is
// exercised on the case it names rather than on a producer-impossible record.

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval/p4offline"
)

const (
	synthEpoch   = int64(20260901)
	synthSession = "p4-synth-session-a"
	synthPool    = "pool-a"
)

// synth accumulates one collector session's facts in causal order.
type synth struct {
	epoch   int64
	session string
	pool    string
	seq     int64
	records []predictioneval.SourceRecord
	source  predictioneval.SourceProvenance
}

func newSynth() *synth {
	return &synth{
		epoch: synthEpoch, session: synthSession, pool: synthPool,
		source: predictioneval.SourceProvenance{
			CollectorEpoch:     synthEpoch,
			CollectorSessionID: synthSession,
			ProducerRevision:   predictioneval.SupportedProducerRevision,
			SessionReading:     "AS_FINALIZED",
			CloseState:         "COMPLETE",
		},
	}
}

func (s *synth) next() int64 { s.seq++; return s.seq }

// fact builds one fact at the next causal position WITHOUT adding it.
func (s *synth) fact(kind, phase, round, event string, attemptID int64) predictioneval.SourceRecord {
	seq := s.next()
	r := predictioneval.SourceRecord{
		ObservationID:      kind + "-" + phase + "-" + strconv.FormatInt(seq, 10),
		CollectorSessionID: s.session,
		CollectorEpoch:     s.epoch,
		CollectorSequence:  seq,
		PoolInstanceID:     s.pool,
		RoundIncarnationID: round,
		RoundCaptureOrigin: "ACTIVE_AT_ADMISSION",
		EventID:            event,
		Kind:               kind,
		PayloadVersion:     predictioneval.SupportedPayloadVersion,
		ObservationSHA256:  "witness-" + strconv.FormatInt(seq, 10),
		Payload:            predictioneval.SourcePayload{Phase: phase},
	}
	if attemptID > 0 {
		r.Payload.Counters = map[string]int64{predictioneval.CounterAutoAttemptID: attemptID}
	}
	return r
}

func (s *synth) add(r predictioneval.SourceRecord) predictioneval.SourceRecord {
	s.records = append(s.records, r)
	return r
}

func (s *synth) due(round, event string, attemptID int64) predictioneval.SourceRecord {
	r := s.fact(predictioneval.KindAutoDecision, predictioneval.PhaseAutoDue, round, event, attemptID)
	r.Payload.ReasonCode = "OK"
	return s.add(r)
}

// terminal is the ONE fact that ends an attempt. phase AUTO_DECIDED writes
// PLACE, AUTO_SKIPPED writes SKIP, exactly as the producer does.
func (s *synth) terminal(round, event string, attemptID int64, phase, reason string,
	env *predictioneval.SourceDecisionEnvelope) predictioneval.SourceRecord {
	r := s.fact(predictioneval.KindAutoDecision, phase, round, event, attemptID)
	r.Payload.ReasonCode = reason
	r.Payload.Decision = "SKIP"
	if phase == predictioneval.PhaseAutoDecided {
		r.Payload.Decision = "PLACE"
	}
	if env != nil {
		clone := *env
		clone.AttemptID = uint64(attemptID)
		env = &clone
		if phase == predictioneval.PhaseAutoDecided && env.ChoiceIndex != nil {
			slot := *env.ChoiceIndex
			r.Payload.OutcomeSlot = &slot
			if env.FinalAmount != nil {
				r.Payload.Counters[predictioneval.CounterStake] = *env.FinalAmount
			}
		}
	}
	r.Payload.DecisionEnvelope = env
	return s.add(r)
}

// placement is one half of a placement call, carrying the stake and slot on
// BOTH halves the way the producer does.
func (s *synth) placement(round, event string, attemptID int64, phase string, stake int64, slot int,
	reason, errClass string) predictioneval.SourceRecord {
	r := s.fact(predictioneval.KindPlacement, phase, round, event, attemptID)
	if r.Payload.Counters == nil {
		r.Payload.Counters = map[string]int64{}
	}
	r.Payload.Counters[predictioneval.CounterStake] = stake
	sl := slot
	r.Payload.OutcomeSlot = &sl
	r.Payload.ReasonCode = reason
	r.Payload.ErrorClass = errClass
	return s.add(r)
}

// call adds the coherent, locally-OK pair the producer writes around one
// placement call.
func (s *synth) call(round, event string, attemptID int64, stake int64, slot int) (started, returned predictioneval.SourceRecord) {
	started = s.placement(round, event, attemptID, predictioneval.PhaseCallStarted, stake, slot, "OK", "NONE")
	returned = s.placement(round, event, attemptID, predictioneval.PhaseCallReturned, stake, slot, "OK", "NONE")
	return started, returned
}

// manualCall is a manual placement call: Manual true, no attempt id.
func (s *synth) manualCall(round, event string, stake int64, slot int) predictioneval.SourceRecord {
	r := s.fact(predictioneval.KindPlacement, predictioneval.PhaseCallStarted, round, event, 0)
	manual := true
	r.Payload.Manual = &manual
	r.Payload.Counters = map[string]int64{predictioneval.CounterStake: stake}
	sl := slot
	r.Payload.OutcomeSlot = &sl
	r.Payload.ReasonCode = "OK"
	r.Payload.ErrorClass = "NONE"
	return s.add(r)
}

// userTerminal is the round's settlement fact: NO attempt discriminator.
func (s *synth) userTerminal(round, event, reason string, stake, payout int64) predictioneval.SourceRecord {
	r := s.fact(predictioneval.KindUserTerminal, predictioneval.PhaseTerminalAdmitted, round, event, 0)
	r.Payload.RoundState = "RESOLVED"
	r.Payload.ReasonCode = reason
	r.Payload.Counters = map[string]int64{
		predictioneval.CounterStake:  stake,
		predictioneval.CounterPayout: payout,
	}
	return s.add(r)
}

// dataset builds a dataset whose provenance DESCRIBES its records: every
// count is derived, so a fixture cannot state one fact count and carry
// another.
func (s *synth) dataset() predictioneval.SourceDataset {
	src := s.source
	owned := int64(0)
	for _, r := range s.records {
		if r.CollectorEpoch == src.CollectorEpoch && r.CollectorSessionID == src.CollectorSessionID {
			owned++
		}
	}
	src.FactsPresent = owned
	src.CommittedCount = owned
	src.WitnessesVerified = owned
	src.WitnessesUnchecked = 0
	src.DroppedCount = 0
	return predictioneval.SourceDataset{Source: src, Records: append([]predictioneval.SourceRecord(nil), s.records...)}
}

func ptrI64(v int64) *int64     { return &v }
func ptrInt(v int) *int         { return &v }
func ptrBool(v bool) *bool      { return &v }
func ptrF64(v float64) *float64 { return &v }

// synthOutcomes is the recurring two-outcome vector: o1 holds 600 of 1000
// points (6 users, top single stake 90), o2 holds 400 (4 users, top 80).
func synthOutcomes() []predictioneval.SourceModelOutcome {
	return []predictioneval.SourceModelOutcome{
		{Slot: 0, Present: true, ID: "o1", TotalUsers: 6, TotalPoints: 600, TopPoints: 90,
			PercentageUsers: 60, Odds: 1.66, OddsPercentage: 60.24},
		{Slot: 1, Present: true, ID: "o2", TotalUsers: 4, TotalPoints: 400, TopPoints: 80,
			PercentageUsers: 40, Odds: 2.5, OddsPercentage: 40},
	}
}

// synthPlacedEnvelope is a WELL-FORMED envelope for an attempt whose policy
// ran all the way to a placement: MOST_VOTED at 5% of a 1000 balance picks o1
// with a stake of 50, no risk gates, stealth OFF.
func synthPlacedEnvelope() *predictioneval.SourceDecisionEnvelope {
	return &predictioneval.SourceDecisionEnvelope{
		AttemptID:      1,
		SettingsStage:  predictioneval.StageExecuted,
		CalculateStage: predictioneval.StageExecuted,
		SkipStage:      predictioneval.StageExecuted,
		HealthStage:    predictioneval.HealthAllowed,
		StakeStage:     predictioneval.StageExecuted,
		Settings: &predictioneval.SourceBetSettings{
			Strategy: predictioneval.StrategyMostVoted, Percentage: 5,
			PercentageGap: 20, MaxPoints: 50_000, Delay: 6, DelayMode: "FROM_END",
		},
		Balance:             ptrI64(1000),
		Outcomes:            synthOutcomes(),
		BetTotalUsers:       ptrI64(10),
		BetTotalPoints:      ptrI64(1000),
		ChoiceIndex:         ptrInt(0),
		ChoiceOutcomeID:     "o1",
		ChoiceAmount:        ptrI64(50),
		SkipResult:          ptrBool(false),
		SkipCompared:        ptrF64(0),
		RiskMaxStakePercent: ptrInt(0),
		RiskReservePoints:   ptrInt(0),
		StakeAllowed:        ptrI64(50),
		StakeReason:         "",
		StakeLimit:          ptrI64(0),
		ClampApplied:        ptrBool(false),
		FinalAmount:         ptrI64(50),
	}
}

// synthSkippedEnvelope is a well-formed envelope for an attempt the policy
// ended BELOW_MINIMUM_POINTS: the same round on a balance of 100, so 5% is 5,
// under the pinned minimum of 10.
func synthSkippedEnvelope() *predictioneval.SourceDecisionEnvelope {
	env := synthPlacedEnvelope()
	env.Balance = ptrI64(100)
	env.ChoiceAmount = ptrI64(5)
	env.StakeAllowed = ptrI64(5)
	env.FinalAmount = ptrI64(5)
	return env
}

// placedAttempt adds a complete placing attempt: AUTO_DUE, the terminal
// envelope, and the coherent locally-OK call pair.
func (s *synth) placedAttempt(round, event string, attemptID int64) {
	s.due(round, event, attemptID)
	env := synthPlacedEnvelope()
	s.terminal(round, event, attemptID, predictioneval.PhaseAutoDecided, "OK", env)
	s.call(round, event, attemptID, *env.FinalAmount, *env.ChoiceIndex)
}

// skippedAttempt adds a complete skipping attempt: AUTO_DUE and the terminal
// envelope, no call.
func (s *synth) skippedAttempt(round, event string, attemptID int64) {
	s.due(round, event, attemptID)
	s.terminal(round, event, attemptID, predictioneval.PhaseAutoSkipped, "BELOW_MINIMUM_POINTS", synthSkippedEnvelope())
}

// singleEpisode returns the one episode a selection holds, failing loudly
// otherwise.
func singleEpisode(t testingT, sel p4selection) p4episode {
	t.Helper()
	if !sel.SessionAdmitted {
		t.Fatalf("session was not admitted: %v", sel.SessionRefusals)
	}
	if len(sel.Episodes) != 1 {
		t.Fatalf("want exactly one episode, got %d: %+v", len(sel.Episodes), sel.Episodes)
	}
	return sel.Episodes[0]
}

// selectedCase builds one placed attempt, applies mutate to its envelope and
// optional extra facts, and returns the dataset, the selected episode and
// its factset.
func selectedCase(t *testing.T, mutate func(*predictioneval.SourceDecisionEnvelope), extra func(*synth)) (predictioneval.SourceDataset, p4offline.EpisodeSelection, p4offline.CommonFactset) {
	t.Helper()
	s := newSynth()
	s.due("r1", "e1", 1)
	env := synthPlacedEnvelope()
	if mutate != nil {
		mutate(env)
	}
	s.terminal("r1", "e1", 1, predictioneval.PhaseAutoDecided, "OK", env)
	stake, slot := int64(50), 0
	if env.FinalAmount != nil {
		stake = *env.FinalAmount
	}
	if env.ChoiceIndex != nil {
		slot = *env.ChoiceIndex
	}
	s.call("r1", "e1", 1, stake, slot)
	if extra != nil {
		extra(s)
	}
	ds := s.dataset()
	ep := singleEpisode(t, mustSelect(t, ds))
	if ep.Excluded {
		t.Fatalf("fixture episode excluded: %v", ep.ExclusionReasons)
	}
	fs, err := p4offline.BuildCommonFactset(ds, ep.Episode)
	if err != nil {
		t.Fatalf("BuildCommonFactset: %v", err)
	}
	return ds, ep, fs
}

// selectedFactset is selectedCase for tests that need no dataset.
func selectedFactset(t *testing.T, mutate func(*predictioneval.SourceDecisionEnvelope), extra func(*synth)) (p4offline.EpisodeSelection, p4offline.CommonFactset) {
	t.Helper()
	_, ep, fs := selectedCase(t, mutate, extra)
	return ep, fs
}

// decider is what both policy results implement.
type decider interface {
	Decision(p4offline.CommonFactset) (p4offline.PolicyDecision, error)
}

// decisionOf binds a policy result to its factset, failing loudly.
func decisionOf(t *testing.T, r decider, fs p4offline.CommonFactset) p4offline.PolicyDecision {
	t.Helper()
	d, err := r.Decision(fs)
	if err != nil {
		t.Fatalf("Decision: %v", err)
	}
	return d
}

// legalMapping is a hand-built LEGAL mapping for tests that need a decision
// shape the fixtures cannot produce natively.
func legalMapping(policy, native string, class p4offline.ActionClass) p4offline.ActionMapping {
	return p4offline.ActionMapping{MapVersion: p4offline.NativeActionMapVersion, Policy: policy,
		NativeAction: native, Class: class, Legal: true}
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// digestOf is the lower-case hex SHA-256 of b, computed by the test itself.
func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
