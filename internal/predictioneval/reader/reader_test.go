package reader_test

// The reader's contract, proven END TO END against the REAL SQLite store.
//
// Everything in this file exists because the replay stack has one failure mode
// that no unit test of a pure function can see: the persisted fact and the
// value the replay reads are declared by two DIFFERENT packages. internal/
// analytics owns the stored envelope; internal/predictioneval re-declares a
// pure mirror of it so the four replay seams can stay free of database/sql.
// The mirror is copied by hand in reader.go. A field that the store grows and
// the mirror does not carry, or a field the converter forgets to copy, is a
// SILENT loss: every downstream stage keeps working, the scorecard still says
// AGREE, and the replay is quietly answering a question about inputs the
// decision never had. So the tests here write real rows through the real
// repository and read them back through the real reader, and they assert the
// values rather than the shapes.
//
// Why the repository and not the collector. The analytics COLLECTOR enforces a
// hard 5 ms per-fact write deadline (analytics.ObservationWriteDeadline), and
// under -race a single SQLite insert takes longer than that, so a collector-
// driven capture test drops every fact and has to skip under the race
// detector. The deadline lives on the collector, NOT on the repository:
// (*analytics.SQLiteRepository).AppendObservation has no deadline at all. So
// the fixtures below drive the repository directly — open a collector session,
// append facts at ascending causal positions, finalize the accounting — which
// produces exactly the rows a real collector run would leave behind, and does
// it deterministically under -race.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"fmt"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval/reader"
	"strconv"
	"sync/atomic"
)

// observationTestDir is the directory the process-wide database singleton is
// opened against. It is a package variable because database.Open is a
// sync.Once singleton: the FIRST caller fixes the path for the whole test
// binary, so if that caller passed a t.TempDir() the directory would be
// removed at the end of that one test and every later test would be talking to
// a deleted file. TestMain opens it once, against a directory that outlives
// every test, exactly as internal/analytics' own TestMain does.
var observationTestDir string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "predictioneval-reader-test-*")
	if err != nil {
		panic(err)
	}
	observationTestDir = dir
	if _, err := database.Open(dir); err != nil {
		panic(err)
	}
	code := m.Run()
	if err := os.RemoveAll(dir); err != nil && code == 0 {
		// Only escalate on an otherwise-green run: a cleanup failure is worth
		// knowing about, and is not worth masking a real test failure.
		fmt.Fprintf(os.Stderr, "predictioneval/reader: could not remove %s: %v\n", dir, err)
		code = 1
	}
	os.Exit(code)
}

// ---------------------------------------------------------------------------
// The fixture
// ---------------------------------------------------------------------------

const (
	// The round coordinates every fixture fact repeats. They are distinctive
	// strings rather than "a"/"b" so a column read out of the wrong position
	// is visible in a failure message instead of plausible.
	fixturePool        = "pool-instance-7f3a"
	fixtureIncarnation = "pool-instance-7f3a#11"
	fixtureChannel     = "channel-90210"
	fixtureEvent       = "event-6d1c9b"
	fixtureOrigin      = "ACTIVE_AT_ADMISSION"

	// fixtureAttemptID is the minted discriminator that links the attempt's
	// facts. It is deliberately not 1: an implementation that fell back to a
	// counter, an index or a truthy default would still produce 1.
	fixtureAttemptID uint64 = 4242

	fixtureOutcomeA = "outcome-a-1f0d55e2"
	fixtureOutcomeB = "outcome-b-2e4b77c1"

	fixtureStartMS = 1_700_000_000_000
	fixtureCloseMS = 1_700_000_900_000
)

func intOf(v int) *int           { return &v }
func int64Of(v int64) *int64     { return &v }
func boolOf(v bool) *bool        { return &v }
func floatOf(v float64) *float64 { return &v }

// observationStore returns a registered analytics repository on the shared
// singleton. Tests isolate themselves by using a distinct collector session
// per test rather than a separate database, which is what the store's own
// (epoch, session id) pair scoping is designed for.
func observationStore(t *testing.T) *analytics.SQLiteRepository {
	t.Helper()
	db, err := database.Open(observationTestDir)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	repo, err := analytics.NewSQLiteRepository(db, t.TempDir())
	if err != nil {
		t.Fatalf("new analytics repository: %v", err)
	}
	return repo
}

// sessionIDSeq makes every fixture session id unique within the test binary.
var sessionIDSeq atomic.Uint64

// collectorSessionID mints a session id in the shape the store's own
// newCollectorSessionID produces — the literal prefix "obs-" followed by 32
// hex characters — derived from a label and a per-call counter. The store
// requires the id to be unique (a UNIQUE column) and otherwise treats it as
// opaque, so the derivation satisfies that constraint without weakening
// anything the store checks.
//
// The label alone is deliberately NOT the whole input; see below.
func collectorSessionID(label string) string {
	// Unique per CALL, not per label. The store enforces a UNIQUE
	// collector_session_id, `go test -count=N` re-runs every test in the SAME
	// process against the SAME database (database.Open is a sync.Once
	// singleton, which is why TestMain opens one directory for the whole
	// binary), and the fixtures are not torn down between repetitions. A
	// label-derived constant therefore collides on the second repetition and
	// the suite fails inside OpenObservationSession — on the fixture, not on
	// the code under test. Both call sites use the returned value rather than
	// re-deriving it, so per-call uniqueness is safe.
	n := sessionIDSeq.Add(1)
	sum := sha256.Sum256([]byte(
		"predictioneval/reader fixture session: " + label + "#" + strconv.FormatUint(n, 10)))
	return "obs-" + hex.EncodeToString(sum[:16])
}

// baseFact is the column half every fixture fact shares. Each value here is
// constrained by a CHECK in the observation schema — the kind, the capture
// origin, the topic and message types, the time source — so a fact that
// violated one would be refused by SQLite rather than quietly stored.
func baseFact(kind string) analytics.PredictionObservation {
	return analytics.PredictionObservation{
		PoolInstanceID: fixturePool,
		// A fact about an admitted round must name the identity the round is
		// retained and erased under; the schema enforces the pair.
		RoundIncarnationID:           fixtureIncarnation,
		RetentionGroupOwnerChannelID: fixtureChannel,
		RoutedChannelID:              fixtureChannel,
		RoundOwnerChannelID:          fixtureChannel,
		// Capture was complete at admission, so there is no gap cause. That
		// matters downstream: a gap cause would make every scorecard carry
		// LimitationCaptureGap, and this fixture is meant to be a clean case.
		RoundCaptureOrigin: fixtureOrigin,
		EventID:            fixtureEvent,
		Kind:               kind,
		SourceTopicType:    analytics.TopicTypePredictionsChannel,
		SourceMessageType:  "event-updated",
		SourceFingerprint:  "fingerprint-" + kind,
		ProducerAtMS:       fixtureStartMS + 10,
		ProducerTimeSource: analytics.TimeSourceProducer,
		ReceivedAtMS:       fixtureStartMS + 20,
	}
}

// fixtureAdmissionSettings is the ADMISSION-time settings snapshot, recorded
// on the schedule fact. It deliberately disagrees with the decision-time
// snapshot in fixtureDecisionEnvelope: the store records the two
// independently and never merges them, so a reader that confused one for the
// other would produce a replay computed from settings the decision never used.
func fixtureAdmissionSettings() *analytics.ObservationBetSettings {
	return &analytics.ObservationBetSettings{
		Strategy:      "HIGH_ODDS",
		Percentage:    11,
		PercentageGap: 3,
		MaxPoints:     9_000,
		MinimumPoints: 75,
		StealthMode:   false,
		Delay:         1.25,
		DelayMode:     "FROM_START",
		FilterCondition: &analytics.ObservationFilterCondition{
			By: "odds", Where: "LTE", Value: 8.5,
		},
	}
}

// fixtureDecisionEnvelope is ONE automatic decision attempt as the pinned
// producer records it: every stage ran, every optional pointer is present, and
// every field carries a distinctive value so a dropped, defaulted or
// neighbour-swapped field is visible rather than plausible.
//
// The values are INTERNALLY CONSISTENT — they are what the pinned policy
// actually produces from these inputs. The arithmetic is spelled out on
// TestThePersistedSessionReplaysThroughEveryStageWithTheIndependentComparisonsAgreeing,
// which is the test that depends on it.
//
// Two choices are worth naming because they look arbitrary and are not:
//
//   - StealthMode is ON while stealth does NOT apply. The pinned condition is
//     "base stake >= the chosen outcome's top single stake", and 1000 >= 7500
//     is false. That keeps the whole replay independent (nothing is
//     conditioned on an observed random draw) while still exercising the
//     branch where the projector hands Evaluate the observed realization and
//     Evaluate must not consume it.
//   - HealthStage is ALLOWED and HealthReason is non-empty. The pinned
//     producer assigns the gate's reason alongside ALLOWED unconditionally
//     (internal/pubsub/pool.go, the AutoBetDecision block), so the pair is
//     representable and the field must survive the mirror. Leaving it empty
//     would have made this the one envelope field the round trip proves
//     nothing about, because it is an omitempty string.
func fixtureDecisionEnvelope() *analytics.ObservationDecisionEnvelope {
	return &analytics.ObservationDecisionEnvelope{
		AttemptID:     fixtureAttemptID,
		SettingsStage: analytics.DecisionStageExecuted,
		Settings: &analytics.ObservationBetSettings{
			Strategy:      "MOST_VOTED",
			Percentage:    25,
			PercentageGap: 20,
			MaxPoints:     50_000,
			MinimumPoints: 250,
			StealthMode:   true,
			Delay:         6.5,
			DelayMode:     "FROM_END",
			FilterCondition: &analytics.ObservationFilterCondition{
				By: "percentage_users", Where: "GT", Value: 40.5,
			},
		},

		CalculateStage: analytics.DecisionStageExecuted,
		Balance:        int64Of(4_000),
		Outcomes: []analytics.ObservationModelOutcome{
			{
				Slot: 0, Present: true, ID: fixtureOutcomeA,
				TotalUsers: 12, TotalPoints: 3_400, TopPoints: 2_000,
				PercentageUsers: 38.75, Odds: 4.25, OddsPercentage: 23.5,
			},
			{
				Slot: 1, Present: true, ID: fixtureOutcomeB,
				TotalUsers: 41, TotalPoints: 9_100, TopPoints: 7_500,
				PercentageUsers: 61.25, Odds: 1.6, OddsPercentage: 62.5,
			},
		},
		BetTotalUsers:  int64Of(53),
		BetTotalPoints: int64Of(12_500),

		ChoiceIndex:     intOf(1),
		ChoiceOutcomeID: fixtureOutcomeB,
		ChoiceAmount:    int64Of(1_000),

		SkipStage:    analytics.DecisionStageExecuted,
		SkipResult:   boolOf(false),
		SkipCompared: floatOf(61.25),

		HealthStage:  analytics.DecisionHealthAllowed,
		HealthReason: "health_pubsub_degraded",

		StakeStage:          analytics.DecisionStageExecuted,
		RiskMaxStakePercent: intOf(20),
		RiskReservePoints:   intOf(500),
		StakeAllowed:        int64Of(800),
		StakeReason:         "max_stake_percent",
		StakeLimit:          int64Of(800),

		ClampApplied: boolOf(true),
		FinalAmount:  int64Of(800),
	}
}

// fixtureAttemptFacts is one collector session's worth of facts, in causal
// order: the schedule fact that admitted the round, the AUTO_DUE fact that
// opened the attempt, the AUTO_DECIDED terminal fact carrying the envelope,
// and the two placement facts that sit on the FAR side of the causal cut.
//
// The two placement facts are load-bearing rather than decorative: they carry
// the same attempt id as the decision, so a materializer that grouped by id
// without bounding the prefix would feed a bet's own outcome back into the
// decision that made it.
func fixtureAttemptFacts() []analytics.PredictionObservation {
	attempt := int64(fixtureAttemptID)

	schedule := baseFact(analytics.KindScheduleDecision)
	schedule.Payload = analytics.ObservationPayload{
		Phase:             "SCHEDULE_ACCEPTED",
		RoundState:        "ACTIVE",
		Decision:          "DEFER",
		ReasonCode:        "OK",
		Counters:          map[string]int64{"windowSeconds": 300, "minimumPoints": 250},
		AdmissionSettings: fixtureAdmissionSettings(),
	}

	due := baseFact(analytics.KindAutoDecision)
	due.Payload = analytics.ObservationPayload{
		Phase:      "AUTO_DUE",
		RoundState: "ACTIVE",
		ReasonCode: "OK",
		Counters:   map[string]int64{"autoAttemptId": attempt, "balance": 4_000},
	}

	decided := baseFact(analytics.KindAutoDecision)
	decided.Payload = analytics.ObservationPayload{
		Phase:            "AUTO_DECIDED",
		RoundState:       "ACTIVE",
		Decision:         "PLACE",
		ReasonCode:       "OK",
		OutcomeSlot:      intOf(1),
		Counters:         map[string]int64{"autoAttemptId": attempt, "balance": 4_000, "stake": 800},
		DecisionEnvelope: fixtureDecisionEnvelope(),
	}

	callStarted := baseFact(analytics.KindPlacement)
	callStarted.Payload = analytics.ObservationPayload{
		Phase:       "CALL_STARTED",
		OutcomeSlot: intOf(1),
		Counters:    map[string]int64{"autoAttemptId": attempt, "stake": 800},
	}

	callReturned := baseFact(analytics.KindPlacement)
	callReturned.Payload = analytics.ObservationPayload{
		Phase:      "CALL_RETURNED",
		ReasonCode: "OK",
		ErrorClass: "NONE",
		Counters:   map[string]int64{"autoAttemptId": attempt, "stake": 800},
	}

	return []analytics.PredictionObservation{schedule, due, decided, callStarted, callReturned}
}

// seedSession writes facts at ascending causal positions 1..N and finalizes
// the session with accounting that classifies it AS_FINALIZED / COMPLETE:
// committed plus dropped must account for every reserved position, and a
// COMPLETE session must hold exactly positions 1..last with no gap.
func seedSession(t *testing.T, repo *analytics.SQLiteRepository, label string,
	facts []analytics.PredictionObservation) (epoch int64, sessionID string) {
	t.Helper()
	ctx := context.Background()

	sessionID = collectorSessionID(label)
	epoch, err := repo.OpenObservationSession(ctx, sessionID, fixtureStartMS)
	if err != nil {
		t.Fatalf("open collector session: %v", err)
	}
	for i := range facts {
		if err := repo.AppendObservation(ctx, facts[i], sessionID, epoch, int64(i+1)); err != nil {
			t.Fatalf("append fact at causal position %d: %v", i+1, err)
		}
	}
	applied, err := repo.FinalizeObservationSession(ctx, epoch, analytics.ObservationAccounting{
		LastAssignedSequence: int64(len(facts)),
		Committed:            int64(len(facts)),
	}, fixtureCloseMS)
	if err != nil || !applied {
		t.Fatalf("finalize collector session: applied=%v err=%v", applied, err)
	}
	return epoch, sessionID
}

// loadFixtureSession seeds the decision fixture and reads it back through the
// real reader, which is the boundary every test below is really about.
func loadFixtureSession(t *testing.T, label string) (predictioneval.SourceDataset, int64, string) {
	t.Helper()
	repo := observationStore(t)
	epoch, sessionID := seedSession(t, repo, label, fixtureAttemptFacts())

	ds, err := reader.LoadSession(context.Background(), repo, epoch, reader.DefaultMaxRecords)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if len(ds.Records) != 5 {
		t.Fatalf("loaded %d facts, want the 5 that were written", len(ds.Records))
	}
	return ds, epoch, sessionID
}

// terminalRecord returns the AUTO_DECIDED fact — the one that carries the
// envelope — from a loaded dataset.
func terminalRecord(t *testing.T, ds predictioneval.SourceDataset) predictioneval.SourceRecord {
	t.Helper()
	for _, r := range ds.Records {
		if r.Kind == predictioneval.KindAutoDecision && r.Payload.Phase == predictioneval.PhaseAutoDecided {
			return r
		}
	}
	t.Fatal("the loaded dataset holds no AUTO_DECIDED fact")
	return predictioneval.SourceRecord{}
}

// asJSONTree renders a value as the generic tree its json tags describe. Two
// trees compare equal exactly when every tagged field carries the same value,
// independent of declaration order — which is what makes it a usable oracle
// for "the mirror preserved everything" without this test having to enumerate
// (and therefore be able to forget) a field.
func asJSONTree(t *testing.T, what string, v interface{}) map[string]interface{} {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", what, err)
	}
	var tree map[string]interface{}
	if err := json.Unmarshal(raw, &tree); err != nil {
		t.Fatalf("unmarshal %s: %v", what, err)
	}
	return tree
}

// ---------------------------------------------------------------------------
// A. Round trip through the real store
// ---------------------------------------------------------------------------

// TestAFullDecisionEnvelopeSurvivesTheRealStoreAndArrivesIntactAtTheReplayMirror
// is the anti-drift proof for the pure mirror.
//
// internal/predictioneval cannot import internal/analytics — that would drag
// database/sql and the SQLite driver behind the purity fence — so reader.go
// copies the stored envelope field by field into a re-declared mirror type.
// Hand-written copying has exactly one interesting failure: a field that is
// never assigned. The mirror still compiles, the replay still runs, every
// stage still produces an answer, and the answer is computed from an input the
// decision never had. Nothing downstream can detect that, because downstream
// only ever sees the mirror.
//
// So this test writes a REAL row through the REAL repository with every
// envelope field populated by a distinctive value, reads it back through the
// REAL reader, and asserts the whole envelope — including the outcome vector
// and the filter condition — arrives byte-for-byte equal in value. It fails
// the moment any single assignment in convertEnvelope / convertSettings is
// dropped, mistyped or wired to the wrong source field.
func TestAFullDecisionEnvelopeSurvivesTheRealStoreAndArrivesIntactAtTheReplayMirror(t *testing.T) {
	ds, epoch, sessionID := loadFixtureSession(t, "envelope-round-trip")
	rec := terminalRecord(t, ds)

	// The row's own columns first. If these are wrong the envelope below is
	// being read out of a fact that is not the one that was written.
	if rec.CollectorEpoch != epoch || rec.CollectorSessionID != sessionID {
		t.Fatalf("terminal fact carries (epoch %d, session %q), want (%d, %q)",
			rec.CollectorEpoch, rec.CollectorSessionID, epoch, sessionID)
	}
	if rec.CollectorSequence != 3 {
		t.Fatalf("terminal fact sits at causal position %d, want 3", rec.CollectorSequence)
	}
	if rec.PoolInstanceID != fixturePool || rec.RoundIncarnationID != fixtureIncarnation ||
		rec.EventID != fixtureEvent {
		t.Fatalf("terminal fact round coordinates = (%q, %q, %q), want (%q, %q, %q)",
			rec.PoolInstanceID, rec.RoundIncarnationID, rec.EventID,
			fixturePool, fixtureIncarnation, fixtureEvent)
	}
	if rec.RoundCaptureOrigin != fixtureOrigin || rec.RoundCaptureGapCause != "" {
		t.Fatalf("capture provenance = (%q, %q), want (%q, \"\") — a round admitted with a "+
			"complete prefix must not read back as one with a gap",
			rec.RoundCaptureOrigin, rec.RoundCaptureGapCause, fixtureOrigin)
	}
	if rec.PayloadVersion != predictioneval.SupportedPayloadVersion {
		t.Fatalf("payload version = %d, want %d", rec.PayloadVersion, predictioneval.SupportedPayloadVersion)
	}
	if rec.PayloadUndecodable {
		t.Fatal("the stored payload did not decode: the fixture no longer round-trips through payload_json")
	}
	if rec.ObservationSHA256 == "" {
		t.Fatal("the row came back with no stored witness; the digest column was not carried across the fence")
	}
	if rec.ObservationID == "" {
		t.Fatal("the row came back with no observation id")
	}

	// The payload's non-envelope half.
	if rec.Payload.Phase != predictioneval.PhaseAutoDecided || rec.Payload.Decision != "PLACE" ||
		rec.Payload.ReasonCode != "OK" || rec.Payload.RoundState != "ACTIVE" {
		t.Fatalf("terminal payload = phase %q decision %q reason %q roundState %q, want AUTO_DECIDED/PLACE/OK/ACTIVE",
			rec.Payload.Phase, rec.Payload.Decision, rec.Payload.ReasonCode, rec.Payload.RoundState)
	}
	if rec.Payload.OutcomeSlot == nil || *rec.Payload.OutcomeSlot != 1 {
		t.Fatalf("terminal outcome slot = %v, want a pointer to 1 — an absent slot and slot 0 are "+
			"different facts and must not collapse", rec.Payload.OutcomeSlot)
	}
	if got := rec.Payload.Counters[predictioneval.CounterAutoAttemptID]; got != int64(fixtureAttemptID) {
		t.Fatalf("terminal fact carries attempt id %d, want %d", got, fixtureAttemptID)
	}
	if got := rec.Payload.Counters[predictioneval.CounterStake]; got != 800 {
		t.Fatalf("terminal fact carries stake counter %d, want 800", got)
	}

	env := rec.Payload.DecisionEnvelope
	if env == nil {
		t.Fatal("the terminal fact came back with NO decision envelope: the whole replay input was lost " +
			"between the store and the mirror")
	}

	// The exhaustive half: every tagged field of the stored envelope against
	// every tagged field of the mirror. The two types declare identical json
	// tags (pinned by TestTheReplayMirrorCarriesAFieldForEveryFieldTheStoreCanPersist),
	// so a difference here is a value that did not survive.
	want := asJSONTree(t, "the stored envelope", fixtureDecisionEnvelope())
	got := asJSONTree(t, "the mirrored envelope", env)
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("the envelope changed on its way through the store and the mirror.\n stored: %#v\n mirror: %#v",
			want, got)
	}

	// The diagnosable half. The tree comparison above proves everything and
	// explains nothing, so the load-bearing scalars are also named
	// individually: when one of them breaks, this is the failure a reader
	// wants to see first.
	if env.AttemptID != fixtureAttemptID {
		t.Errorf("envelope attempt id = %d, want %d", env.AttemptID, fixtureAttemptID)
	}
	if env.SettingsStage != predictioneval.StageExecuted ||
		env.CalculateStage != predictioneval.StageExecuted ||
		env.SkipStage != predictioneval.StageExecuted ||
		env.StakeStage != predictioneval.StageExecuted {
		t.Errorf("stage states = settings %q calculate %q skip %q stake %q, want all EXECUTED",
			env.SettingsStage, env.CalculateStage, env.SkipStage, env.StakeStage)
	}
	if env.HealthStage != predictioneval.HealthAllowed || env.HealthReason != "health_pubsub_degraded" {
		t.Errorf("health verdict = (%q, %q), want (%q, %q)",
			env.HealthStage, env.HealthReason, predictioneval.HealthAllowed, "health_pubsub_degraded")
	}
	if env.Balance == nil || *env.Balance != 4_000 {
		t.Errorf("envelope balance = %v, want a pointer to 4000", env.Balance)
	}
	if env.ChoiceIndex == nil || *env.ChoiceIndex != 1 {
		t.Errorf("envelope choice index = %v, want a pointer to 1", env.ChoiceIndex)
	}
	if env.ChoiceOutcomeID != fixtureOutcomeB {
		t.Errorf("envelope chosen outcome id = %q, want %q", env.ChoiceOutcomeID, fixtureOutcomeB)
	}
	if env.ChoiceAmount == nil || *env.ChoiceAmount != 1_000 {
		t.Errorf("envelope choice amount = %v, want a pointer to 1000", env.ChoiceAmount)
	}
	if env.SkipResult == nil || *env.SkipResult {
		t.Errorf("envelope skip result = %v, want a pointer to false — a recorded false and an "+
			"absent answer are different facts", env.SkipResult)
	}
	if env.SkipCompared == nil || *env.SkipCompared != 61.25 {
		t.Errorf("envelope skip comparand = %v, want a pointer to 61.25", env.SkipCompared)
	}
	if env.RiskMaxStakePercent == nil || *env.RiskMaxStakePercent != 20 ||
		env.RiskReservePoints == nil || *env.RiskReservePoints != 500 {
		t.Errorf("envelope risk inputs = (%v, %v), want pointers to (20, 500)",
			env.RiskMaxStakePercent, env.RiskReservePoints)
	}
	if env.StakeAllowed == nil || *env.StakeAllowed != 800 ||
		env.StakeLimit == nil || *env.StakeLimit != 800 || env.StakeReason != "max_stake_percent" {
		t.Errorf("envelope stake gate return = (%v, %q, %v), want pointers to (800, max_stake_percent, 800)",
			env.StakeAllowed, env.StakeReason, env.StakeLimit)
	}
	if env.ClampApplied == nil || !*env.ClampApplied || env.FinalAmount == nil || *env.FinalAmount != 800 {
		t.Errorf("envelope clamp = (%v, %v), want pointers to (true, 800)", env.ClampApplied, env.FinalAmount)
	}

	// The filter condition is a nested pointer struct and is the easiest thing
	// in the envelope to lose whole: nil is a VALUE here, not an absence,
	// because the pinned policy answers "do not skip" immediately when there
	// is no condition.
	s := env.Settings
	if s == nil {
		t.Fatal("the decision-time settings snapshot was lost between the store and the mirror")
	}
	if s.Strategy != "MOST_VOTED" || s.Percentage != 25 || s.PercentageGap != 20 ||
		s.MaxPoints != 50_000 || s.MinimumPoints != 250 || !s.StealthMode ||
		s.Delay != 6.5 || s.DelayMode != "FROM_END" {
		t.Errorf("decision-time settings = %+v, want the fixture's nine distinctive values", *s)
	}
	if s.FilterCondition == nil {
		t.Fatal("the filter condition was lost: nil is the policy's 'never skip' input, so losing a " +
			"real condition silently changes what a replay computes")
	}
	if *s.FilterCondition != (predictioneval.SourceFilterCondition{
		By: "percentage_users", Where: "GT", Value: 40.5,
	}) {
		t.Errorf("filter condition = %+v, want {percentage_users GT 40.5}", *s.FilterCondition)
	}

	// The outcome vector, including the derived per-outcome values the model
	// had accumulated. These are the strategy's actual inputs.
	if len(env.Outcomes) != 2 {
		t.Fatalf("the mirrored outcome vector holds %d entries, want 2", len(env.Outcomes))
	}
	wantOutcomes := []predictioneval.SourceModelOutcome{
		{Slot: 0, Present: true, ID: fixtureOutcomeA, TotalUsers: 12, TotalPoints: 3_400,
			TopPoints: 2_000, PercentageUsers: 38.75, Odds: 4.25, OddsPercentage: 23.5},
		{Slot: 1, Present: true, ID: fixtureOutcomeB, TotalUsers: 41, TotalPoints: 9_100,
			TopPoints: 7_500, PercentageUsers: 61.25, Odds: 1.6, OddsPercentage: 62.5},
	}
	for i, wantOutcome := range wantOutcomes {
		if env.Outcomes[i] != wantOutcome {
			t.Errorf("mirrored outcome %d = %+v, want %+v", i, env.Outcomes[i], wantOutcome)
		}
	}

	// The admission-time settings travel on their own fact and must NOT be
	// merged with the decision-time snapshot. They carry deliberately
	// different values, so a converter that confused the two is visible.
	var admission *predictioneval.SourceBetSettings
	for _, r := range ds.Records {
		if r.Payload.AdmissionSettings != nil {
			admission = r.Payload.AdmissionSettings
		}
	}
	if admission == nil {
		t.Fatal("the admission-time settings were lost: the schedule fact's own settings snapshot " +
			"never reached the mirror")
	}
	if admission.Strategy != "HIGH_ODDS" || admission.Percentage != 11 || admission.MaxPoints != 9_000 ||
		admission.StealthMode || admission.DelayMode != "FROM_START" {
		t.Errorf("admission settings = %+v, want the admission fixture's values (a decision-time "+
			"snapshot here would mean the two facts were merged)", *admission)
	}
}

// ---------------------------------------------------------------------------
// B. Mirror completeness, by reflection
// ---------------------------------------------------------------------------

// TestTheReplayMirrorCarriesAFieldForEveryFieldTheStoreCanPersist is the test
// that catches the NEXT field, not this one.
//
// The round-trip test above proves that every field the fixture populates
// survives. It cannot prove anything about a field that does not exist yet.
// When P1.5 grows the stored envelope — a new gate input, a new stage state, a
// new derived outcome value — the mirror does not follow automatically, the
// converter does not fail to compile, and the replay silently starts ignoring
// an input that decisions were computed from. records.go names this test as
// the thing that prevents it.
//
// So this walks the STORE's types with reflection and requires the mirror to
// carry a field of the same name, with the same json tag, and to have exactly
// as many fields — equal counts plus a name for every stored name is a
// bijection, so a mirror field with no stored counterpart is caught too.
func TestTheReplayMirrorCarriesAFieldForEveryFieldTheStoreCanPersist(t *testing.T) {
	for _, pair := range []struct {
		what   string
		stored reflect.Type
		mirror reflect.Type
	}{
		{
			what:   "the decision envelope",
			stored: reflect.TypeOf(analytics.ObservationDecisionEnvelope{}),
			mirror: reflect.TypeOf(predictioneval.SourceDecisionEnvelope{}),
		},
		{
			what:   "the bet settings snapshot",
			stored: reflect.TypeOf(analytics.ObservationBetSettings{}),
			mirror: reflect.TypeOf(predictioneval.SourceBetSettings{}),
		},
		{
			what:   "the filter condition",
			stored: reflect.TypeOf(analytics.ObservationFilterCondition{}),
			mirror: reflect.TypeOf(predictioneval.SourceFilterCondition{}),
		},
		{
			what:   "the model outcome",
			stored: reflect.TypeOf(analytics.ObservationModelOutcome{}),
			mirror: reflect.TypeOf(predictioneval.SourceModelOutcome{}),
		},
	} {
		t.Run(pair.what, func(t *testing.T) {
			if pair.stored.NumField() != pair.mirror.NumField() {
				t.Errorf("%s: the store declares %d fields (%s) and the mirror %d (%s). A mirror "+
					"that is not the same size either drops an input the replay must read or "+
					"invents one the store never wrote",
					pair.what, pair.stored.NumField(), pair.stored,
					pair.mirror.NumField(), pair.mirror)
			}
			for i := 0; i < pair.stored.NumField(); i++ {
				storedField := pair.stored.Field(i)
				mirrorField, ok := pair.mirror.FieldByName(storedField.Name)
				if !ok {
					t.Errorf("%s: %s.%s has NO counterpart in %s. Every decision replayed from a "+
						"fact carrying that field would be computed without it, and nothing "+
						"downstream could tell",
						pair.what, pair.stored, storedField.Name, pair.mirror)
					continue
				}
				// The json tags are what the persisted projection is keyed by,
				// and what the round-trip proof above compares through. A
				// mirror field with the right name and the wrong tag reads a
				// different key out of the same payload.
				storedTag := storedField.Tag.Get("json")
				mirrorTag := mirrorField.Tag.Get("json")
				if storedTag != mirrorTag {
					t.Errorf("%s: field %s is tagged %q in the store and %q in the mirror",
						pair.what, storedField.Name, storedTag, mirrorTag)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// C. The full pipeline, over persisted data
// ---------------------------------------------------------------------------

// TestThePersistedSessionReplaysThroughEveryStageWithTheIndependentComparisonsAgreeing
// drives all four seams over facts that came out of SQLite, and requires the
// replay to re-derive what the producer recorded.
//
// The fixture's recorded results are not decoration: they are what the pinned
// policy actually produces from the fixture's inputs, computed by hand so the
// test is an oracle rather than a mirror of the implementation. The arithmetic,
// in the order the pinned caller walks it:
//
//	choice    MOST_VOTED is argmax(totalUsers) with a STRICT >, so a tie keeps
//	          the lowest index. totalUsers = [12, 41] -> index 1, outcome B.
//	base      percentage of balance, truncated by the int conversion, then
//	          capped by MaxPoints: 4000 * (25/100) = 1000.0 -> 1000; MaxPoints
//	          is 50000, so nothing is capped.
//	stealth   applies only when stealthMode AND base >= the CHOSEN outcome's
//	          topPoints. 1000 >= 7500 is false, so stealth does NOT apply, the
//	          stake stays 1000, and every downstream value stays INDEPENDENT
//	          (nothing is conditioned on the unreproducible random draw).
//	filter    by=percentage_users is not total_users/total_points, so it reads
//	          the CHOSEN outcome: 61.25. GT 40.5 holds -> do not skip.
//	health    ALLOWED is echoed, never recomputed, and is counted apart.
//	gate      maxStakePercent 20 -> 4000*20 = 80000, /100 = 800. 800 < 1000, so
//	          the percent gate binds: allowed=800, reason=max_stake_percent,
//	          limit=800. reserve 500 is a FLOOR: 4000-800 = 3200 >= 500, so
//	          there is no violation.
//	clamp     the percent gate bound, so the caller adopts the allowance:
//	          final=800, clampApplied=true.
//	minimum   800 >= PinnedMinimumStake (10) -> not below.
//	filter    acted on LAST, after the minimum: it said "do not skip", so the
//	          attempt reaches the placement call -> WOULD_ATTEMPT_PLACEMENT,
//	          whose producer counterpart is the reason code OK.
//
// A zero-case pass is the failure this test most has to avoid: a pipeline that
// silently excluded the attempt would produce a scorecard-free run with no
// disagreements at all. len(pk.Attempts) is therefore asserted explicitly
// before anything else is believed.
func TestThePersistedSessionReplaysThroughEveryStageWithTheIndependentComparisonsAgreeing(t *testing.T) {
	ds, epoch, sessionID := loadFixtureSession(t, "full-pipeline")

	// ---- Seam 1: materialize. -----------------------------------------
	pk, err := predictioneval.MaterializePairedKnowledge(ds)
	if err != nil {
		t.Fatalf("MaterializePairedKnowledge: %v", err)
	}
	if len(pk.Attempts) != 1 {
		t.Fatalf("materialized %d attempts, want exactly 1. Excluded=%+v Anomalies=%v — a replay "+
			"that produces no case cannot disagree with anything, so zero attempts must never "+
			"read as a pass", len(pk.Attempts), pk.Excluded, pk.Anomalies)
	}
	if len(pk.Excluded) != 0 {
		t.Fatalf("the fixture session excluded %+v; every fact of it is readable under the "+
			"supported contract", pk.Excluded)
	}
	if len(pk.Anomalies) != 0 {
		t.Fatalf("the fixture session reported anomalies %v; it is finalized COMPLETE, fully "+
			"witnessed, and written under the supported producer revision", pk.Anomalies)
	}

	attempt := pk.Attempts[0]
	wantKey := predictioneval.AttemptKey{
		CollectorEpoch:     epoch,
		CollectorSessionID: sessionID,
		PoolInstanceID:     fixturePool,
		AttemptID:          fixtureAttemptID,
	}
	if attempt.Key != wantKey {
		t.Fatalf("attempt key = %+v, want %+v", attempt.Key, wantKey)
	}
	// The causal cut: the AUTO_DUE and AUTO_DECIDED facts are common
	// knowledge; the two placement facts are not, and must be reachable only
	// from Score.
	if len(attempt.CommonInputSlice) != 2 || attempt.TerminalIndex != 1 {
		t.Fatalf("common-knowledge slice holds %d facts with terminal index %d, want 2 and 1 — the "+
			"placement facts must sit on the far side of the cut",
			len(attempt.CommonInputSlice), attempt.TerminalIndex)
	}
	if len(attempt.PostDecision) != 2 {
		t.Fatalf("post-decision facts = %d, want the 2 placement facts", len(attempt.PostDecision))
	}
	if attempt.CommonInputSlice[attempt.TerminalIndex].Payload.Phase != predictioneval.PhaseAutoDecided {
		t.Fatalf("the terminal fact of the slice is phase %q, want AUTO_DECIDED",
			attempt.CommonInputSlice[attempt.TerminalIndex].Payload.Phase)
	}
	if !attempt.SawDueFact || attempt.DueReason != "OK" {
		t.Fatalf("attempt opening fact = (saw %v, reason %q), want (true, OK)",
			attempt.SawDueFact, attempt.DueReason)
	}
	if attempt.CommonInputDigest == "" {
		t.Fatal("the attempt carries no common-input digest, so nothing witnesses which facts the " +
			"replay treated as its inputs")
	}
	if attempt.RoundCaptureGapCause != "" {
		t.Fatalf("attempt capture gap cause = %q, want empty", attempt.RoundCaptureGapCause)
	}

	// ---- Seam 2: project. ---------------------------------------------
	decisionCase, err := predictioneval.ProjectDecisionCase(attempt)
	if err != nil {
		t.Fatalf("ProjectDecisionCase: %v", err)
	}
	if !decisionCase.Eligibility.Eligible || len(decisionCase.Eligibility.Reasons) != 0 {
		t.Fatalf("the case is ineligible for %v; every input the pinned policy reads was recorded",
			decisionCase.Eligibility.Reasons)
	}
	if !decisionCase.Eligibility.ExercisesPolicy {
		t.Fatal("the case does not exercise the policy, so it would contribute no evidence at all: " +
			"the fixture's CalculateStage says the policy ran")
	}
	in := decisionCase.Inputs
	if !in.ReachedDecision || in.PreDecisionExit != "" {
		t.Fatalf("inputs report reachedDecision=%v preDecisionExit=%q, want true and empty",
			in.ReachedDecision, in.PreDecisionExit)
	}
	if !in.BalancePresent || in.Balance != 4_000 {
		t.Fatalf("projected balance = (%d, present %v), want 4000", in.Balance, in.BalancePresent)
	}
	if !in.OutcomesPresent || len(in.Outcomes) != 2 {
		t.Fatalf("projected outcomes = %d (present %v), want 2", len(in.Outcomes), in.OutcomesPresent)
	}
	if in.Settings == nil || in.Settings.Strategy != predictioneval.StrategyMostVoted {
		t.Fatalf("projected settings = %+v, want the MOST_VOTED decision-time snapshot", in.Settings)
	}
	if !in.RiskPresent || in.RiskMaxStakePercent != 20 || in.RiskReservePoints != 500 {
		t.Fatalf("projected risk inputs = (present %v, %d%%, reserve %d), want (true, 20, 500)",
			in.RiskPresent, in.RiskMaxStakePercent, in.RiskReservePoints)
	}
	if in.HealthState != predictioneval.HealthAllowed {
		t.Fatalf("projected health verdict = %q, want ALLOWED", in.HealthState)
	}
	if in.MinimumStake != predictioneval.PinnedMinimumStake {
		t.Fatalf("projected minimum stake = %d, want the pinned %d",
			in.MinimumStake, predictioneval.PinnedMinimumStake)
	}
	// Stealth mode is on, so the projector hands over the ONE observed value.
	// The evaluation below must still not consume it, because stealth does not
	// apply on these numbers.
	if decisionCase.Observed.StealthAmount == nil || *decisionCase.Observed.StealthAmount != 1_000 {
		t.Fatalf("observed realization = %v, want a pointer to the recorded pre-risk stake 1000",
			decisionCase.Observed.StealthAmount)
	}

	// ---- Seam 3: evaluate. --------------------------------------------
	ev := predictioneval.Evaluate(decisionCase.Inputs, decisionCase.Observed)

	if ev.Choice.State != predictioneval.StageStateExecuted || ev.Choice.Index != 1 ||
		!ev.Choice.Selected || ev.Choice.OutcomeID != fixtureOutcomeB {
		t.Fatalf("choice stage = %+v, want EXECUTED index 1 selected on %q", ev.Choice, fixtureOutcomeB)
	}
	if ev.BaseStake.State != predictioneval.StageStateExecuted || ev.BaseStake.Amount != 1_000 ||
		ev.BaseStake.Capped {
		t.Fatalf("base stake stage = %+v, want EXECUTED 1000 uncapped (4000 * 25/100, MaxPoints 50000)",
			ev.BaseStake)
	}
	if ev.Stealth.Outcome != predictioneval.StealthNotApplicable || ev.Stealth.Applies ||
		ev.Stealth.Realized != 1_000 {
		t.Fatalf("stealth stage = %+v, want NOT_APPLICABLE with the independently derived 1000: the "+
			"base stake 1000 is below the chosen outcome's top single stake 7500, so the pinned "+
			"policy never draws", ev.Stealth)
	}
	if ev.Filter.State != predictioneval.StageStateExecuted || ev.Filter.Skip ||
		ev.Filter.Compared != 61.25 || ev.Filter.Applied {
		t.Fatalf("filter stage = %+v, want EXECUTED, do-not-skip, compared 61.25, not applied", ev.Filter)
	}
	if ev.Health.State != predictioneval.StageStateWitnessed ||
		ev.Health.Verdict != predictioneval.HealthAllowed {
		t.Fatalf("health stage = %+v, want a WITNESSED echo of ALLOWED", ev.Health)
	}
	if ev.StakeGate.State != predictioneval.StageStateExecuted || ev.StakeGate.Proposed != 1_000 ||
		ev.StakeGate.Allowed != 800 || ev.StakeGate.Reason != predictioneval.GatePercent ||
		ev.StakeGate.Limit != 800 {
		t.Fatalf("stake gate stage = %+v, want EXECUTED proposed 1000 allowed 800 via %q limit 800",
			ev.StakeGate, predictioneval.GatePercent)
	}
	if ev.Clamp.State != predictioneval.StageStateExecuted || !ev.Clamp.Applied ||
		!ev.Clamp.HasFinal || ev.Clamp.FinalAmount != 800 {
		t.Fatalf("clamp stage = %+v, want EXECUTED applied with final 800", ev.Clamp)
	}
	if ev.Minimum.State != predictioneval.StageStateExecuted ||
		ev.Minimum.Threshold != predictioneval.PinnedMinimumStake || ev.Minimum.Below {
		t.Fatalf("minimum stage = %+v, want EXECUTED at threshold %d and not below",
			ev.Minimum, predictioneval.PinnedMinimumStake)
	}
	if !ev.PolicyAmountKnown || ev.PolicyAmount != 1_000 {
		t.Fatalf("policy amount = (%d, known %v), want Calculate's complete return 1000",
			ev.PolicyAmount, ev.PolicyAmountKnown)
	}
	if ev.Action != predictioneval.ActionWouldAttemptPlacement {
		t.Fatalf("replayed action = %q (%q), want WOULD_ATTEMPT_PLACEMENT", ev.Action, ev.ActionReason)
	}
	if ev.LegacyFailure != predictioneval.LegacyFailureNone {
		t.Fatalf("the pinned policy reported the legacy failure %q on an input it handles cleanly",
			ev.LegacyFailure)
	}

	// ---- Seam 4: score. -----------------------------------------------
	settlement := predictioneval.ProjectSettlementFacts(attempt)
	if !settlement.PlacementCallStarted || !settlement.PlacementCallReturned ||
		!settlement.PlacementAccepted || settlement.PostDecisionFacts != 2 {
		t.Fatalf("settlement facts = %+v, want both placement facts, accepted", settlement)
	}
	if settlement.PlacementStake == nil || *settlement.PlacementStake != 800 ||
		settlement.PlacementSlot == nil || *settlement.PlacementSlot != 1 {
		t.Fatalf("settlement placement = (stake %v, slot %v), want pointers to (800, 1)",
			settlement.PlacementStake, settlement.PlacementSlot)
	}

	sc := predictioneval.Score(decisionCase, ev, settlement)

	if sc.ReplayedAction != predictioneval.ActionWouldAttemptPlacement || sc.RecordedTerminal != "OK" {
		t.Fatalf("scorecard action pair = (%q, %q), want (WOULD_ATTEMPT_PLACEMENT, OK)",
			sc.ReplayedAction, sc.RecordedTerminal)
	}
	if sc.CommonInputDigest != attempt.CommonInputDigest {
		t.Fatalf("the scorecard's digest %q is not the attempt's %q", sc.CommonInputDigest, attempt.CommonInputDigest)
	}

	// Every independent comparison the fixture is designed to produce, named
	// so the tally below cannot drift away from the comparisons themselves.
	independent := map[string]bool{
		"choiceIndex": false, "choiceOutcomeId": false, "choiceAmount": false,
		"skipResult": false, "skipCompared": false,
		"stakeAllowed": false, "stakeReason": false, "stakeLimit": false,
		"clampApplied": false, "finalAmount": false, "terminalReason": false,
		// The terminal REASON alone is not the terminal action. A record whose
		// phase says AUTO_SKIPPED/SKIP while its envelope replays to a
		// placement contradicts itself, and comparing only the reason code
		// would count that as agreement.
		"terminalPhase": false, "terminalDecision": false,
	}
	for _, cmp := range sc.Comparisons {
		if cmp.Verdict != predictioneval.VerdictAgree {
			t.Errorf("comparison %q is %s (recorded %q, computed %q, basis %s): the replay did not "+
				"re-derive what the producer recorded",
				cmp.Field, cmp.Verdict, cmp.Recorded, cmp.Computed, cmp.Basis)
		}
		if seen, expected := independent[cmp.Field]; expected {
			if seen {
				t.Errorf("comparison %q appeared twice", cmp.Field)
			}
			if cmp.Basis != predictioneval.BasisIndependent {
				t.Errorf("comparison %q has basis %s, want INDEPENDENT: nothing in this fixture is "+
					"downstream of a conditioned stealth draw", cmp.Field, cmp.Basis)
			}
			independent[cmp.Field] = true
		}
	}
	for field, seen := range independent {
		if !seen {
			t.Errorf("the scorecard never compared %q; a comparison that disappears is a check that "+
				"stopped happening, not a check that passed", field)
		}
	}

	if sc.IndependentDisagree != 0 {
		t.Fatalf("independent disagreements = %d, want 0. Comparisons: %+v",
			sc.IndependentDisagree, sc.Comparisons)
	}
	if sc.ConditionedDisagree != 0 {
		t.Fatalf("conditioned disagreements = %d, want 0", sc.ConditionedDisagree)
	}
	if sc.IndependentAgree != len(independent) {
		t.Fatalf("independent agreements = %d, want the %d named comparisons. Comparisons: %+v",
			sc.IndependentAgree, len(independent), sc.Comparisons)
	}
	if sc.IndependentAgree == 0 {
		t.Fatal("the scorecard agreed on nothing independently: a run with no independent evidence " +
			"must never read as a successful replay")
	}
	// The health verdict is echoed from the record, so its agreement is
	// guaranteed by construction and must be counted apart from evidence.
	if sc.WitnessedEchoes != 1 {
		t.Errorf("witnessed echoes = %d, want exactly 1 (the health gate)", sc.WitnessedEchoes)
	}
	if sc.ConditionedAgree != 0 || sc.CircularComparisons != 0 || sc.UnavailablePairs != 0 {
		t.Errorf("conditioned=%d circular=%d unavailable=%d, want all zero: this fixture's stealth "+
			"stage does not apply, so no value is conditioned on an observed draw",
			sc.ConditionedAgree, sc.CircularComparisons, sc.UnavailablePairs)
	}

	if sc.Settlement.Assessment != predictioneval.SettlementAppliesToReplay {
		t.Errorf("settlement assessment = %q, want APPLIES_TO_REPLAY: the replay reproduced the "+
			"decision independently and the placement call returned",
			sc.Settlement.Assessment)
	}
	if sc.Settlement.ROI != predictioneval.SettlementUnknown {
		t.Errorf("settlement ROI = %q, want UNKNOWN — this package never computes profitability",
			sc.Settlement.ROI)
	}

	// The limitations a clean case must NOT carry. Each of these would mean
	// the scorecard is quietly weaker than it looks.
	for _, forbidden := range []string{
		predictioneval.LimitationCaseExcluded,
		predictioneval.LimitationPolicyNotRun,
		predictioneval.LimitationCaptureGap,
		predictioneval.LimitationNoDueFact,
		predictioneval.LimitationStealthConditioned,
	} {
		for _, got := range sc.Limitations {
			if got == forbidden {
				t.Errorf("the scorecard carries the limitation %q, which this fixture is built to "+
					"avoid: %v", forbidden, sc.Limitations)
			}
		}
	}
	// And the two it MUST carry, because they are true of every result this
	// package produces for a case whose health verdict was witnessed.
	for _, required := range []string{
		predictioneval.LimitationNoROIComputed,
		predictioneval.LimitationHealthWitnessed,
	} {
		found := false
		for _, got := range sc.Limitations {
			if got == required {
				found = true
			}
		}
		if !found {
			t.Errorf("the scorecard does not carry the limitation %q; a consumer would read more "+
				"into it than it proves. Got: %v", required, sc.Limitations)
		}
	}
}

// ---------------------------------------------------------------------------
// D. Provenance is carried, not invented
// ---------------------------------------------------------------------------

// TestProvenanceAndVocabularyAreCarriedFromTheStoreRatherThanInvented pins the
// two ways a replay can lie about its own evidence.
//
// The first is provenance: a scorecard names the session it came from, how the
// store classified it, and how many of its facts actually had their stored
// digest recomputed. Every one of those is the STORE's verdict. A reader that
// re-derived them — or worse, filled them in with plausible values — would let
// a truncated or unwitnessed session read as an authoritative one.
//
// The second is vocabulary. predictioneval deliberately re-declares the
// store's constants instead of importing them, because importing internal/
// analytics would drag database/sql behind the purity fence. Re-declaration
// costs drift, and the payment is this test: when the producer contract moves,
// the pin fails loudly here instead of the replay silently reading facts
// written under a contract nobody checked.
func TestProvenanceAndVocabularyAreCarriedFromTheStoreRatherThanInvented(t *testing.T) {
	t.Run("the pinned contract identities match the store", func(t *testing.T) {
		if predictioneval.SupportedProducerRevision != analytics.ObservationProducerRevision {
			t.Errorf("the replay binds to producer revision %q but the store now writes %q. This is "+
				"a decision for a human: the replay must be re-read against the new contract "+
				"before it is re-pinned, never bumped to follow it",
				predictioneval.SupportedProducerRevision, analytics.ObservationProducerRevision)
		}
		if predictioneval.SupportedPayloadVersion != analytics.ObservationPayloadVersion {
			t.Errorf("the replay reads payload version %d but the store writes %d",
				predictioneval.SupportedPayloadVersion, analytics.ObservationPayloadVersion)
		}
		if predictioneval.PinnedMaxOutcomes != analytics.MaxObservationOutcomes {
			t.Errorf("the replay's outcome ceiling is %d but the store's is %d; a vector between the "+
				"two would be replayed as if it had been stored whole",
				predictioneval.PinnedMaxOutcomes, analytics.MaxObservationOutcomes)
		}
		// The legacy prefix must not match the live contract, or every current
		// session would be classified as pre-envelope and yield nothing.
		if strings.HasPrefix(analytics.ObservationProducerRevision, predictioneval.LegacyProducerRevisionPrefix) {
			t.Errorf("the store's producer revision %q starts with the legacy prefix %q, so every "+
				"live session would be read as pre-envelope",
				analytics.ObservationProducerRevision, predictioneval.LegacyProducerRevisionPrefix)
		}
	})

	t.Run("the re-declared vocabulary matches the store's", func(t *testing.T) {
		for _, tc := range []struct{ what, replay, store string }{
			{"kind auto_decision", predictioneval.KindAutoDecision, analytics.KindAutoDecision},
			{"kind placement", predictioneval.KindPlacement, analytics.KindPlacement},
			{"kind user_terminal", predictioneval.KindUserTerminal, analytics.KindUserTerminal},
			{"stage EXECUTED", predictioneval.StageExecuted, analytics.DecisionStageExecuted},
			{"stage NOT_REACHED", predictioneval.StageNotReached, analytics.DecisionStageNotReached},
			{"health NOT_REACHED", predictioneval.HealthNotReached, analytics.DecisionHealthNotReached},
			{"health DISABLED", predictioneval.HealthDisabled, analytics.DecisionHealthDisabled},
			{"health NO_GATE", predictioneval.HealthNoGate, analytics.DecisionHealthNoGate},
			{"health ALLOWED", predictioneval.HealthAllowed, analytics.DecisionHealthAllowed},
			{"health DENIED", predictioneval.HealthDenied, analytics.DecisionHealthDenied},
			{"the unknown-value sentinel", predictioneval.ValueUnknown, analytics.ValueUnknown},
		} {
			if tc.replay != tc.store {
				t.Errorf("%s: the replay spells it %q and the store %q. A stage state that does not "+
					"match is read as UNKNOWN and the case becomes ineligible for a reason that "+
					"is not true of it", tc.what, tc.replay, tc.store)
			}
		}
	})

	// The Reading* constants are re-declared UNEXPORTED in predictioneval, so
	// they cannot be compared directly from outside it. They are pinned
	// behaviourally instead, which is the property that actually matters: each
	// of the store's readings must produce the anomaly that qualifies it.
	t.Run("each of the store's session readings produces its own anomaly", func(t *testing.T) {
		for _, tc := range []struct {
			reading string
			want    string
		}{
			{analytics.ReadingUnfinalized, predictioneval.AnomalySessionUnfinalized},
			{analytics.ReadingAdministrativelyTruncated, predictioneval.AnomalySessionTruncated},
			{analytics.ReadingIntegrityError, predictioneval.AnomalySessionIntegrityError},
		} {
			pk, err := predictioneval.MaterializePairedKnowledge(predictioneval.SourceDataset{
				Source: predictioneval.SourceProvenance{
					CollectorEpoch:     1,
					CollectorSessionID: collectorSessionID("reading-" + tc.reading),
					ProducerRevision:   predictioneval.SupportedProducerRevision,
					SessionReading:     tc.reading,
				},
			})
			if err != nil {
				t.Fatalf("materialize a %s session: %v", tc.reading, err)
			}
			found := false
			for _, a := range pk.Anomalies {
				if a == tc.want {
					found = true
				}
			}
			if !found {
				t.Errorf("a session the store classified %q produced anomalies %v, want %q. The "+
					"replay's re-declared reading vocabulary has drifted from the store's, so a "+
					"session that is not authoritative reads as one that is",
					tc.reading, pk.Anomalies, tc.want)
			}
		}
	})

	t.Run("the loaded dataset carries the store's own verdict", func(t *testing.T) {
		ds, epoch, sessionID := loadFixtureSession(t, "provenance")
		src := ds.Source

		if src.CollectorEpoch != epoch || src.CollectorSessionID != sessionID {
			t.Errorf("provenance identity = (%d, %q), want (%d, %q)",
				src.CollectorEpoch, src.CollectorSessionID, epoch, sessionID)
		}
		if src.ProducerRevision != analytics.ObservationProducerRevision {
			t.Errorf("carried producer revision = %q, want the stamped %q. The revision belongs to "+
				"the rows and is only ever compared against, never restamped by whatever build "+
				"happens to be replaying",
				src.ProducerRevision, analytics.ObservationProducerRevision)
		}
		if src.SessionReading != analytics.ReadingAsFinalized {
			t.Errorf("carried session reading = %q (%s), want %q",
				src.SessionReading, src.SessionDetail, analytics.ReadingAsFinalized)
		}
		if src.CloseState != analytics.SessionComplete {
			t.Errorf("carried close state = %q, want %q", src.CloseState, analytics.SessionComplete)
		}
		if src.SessionDetail != "" {
			t.Errorf("a cleanly finalized session carries the detail %q, want none", src.SessionDetail)
		}
		// Five facts were written, and the store's bounded witness sweep
		// recomputes every one of them at this size. An unchecked remainder
		// must be reported as unchecked, never rounded up to verified.
		if src.WitnessesVerified != 5 || src.WitnessesUnchecked != 0 {
			t.Errorf("witnesses = (verified %d, unchecked %d), want (5, 0). These are the store's "+
				"count of digests it actually RECOMPUTED; a reader that invented them would let "+
				"an unverified session claim to be witnessed",
				src.WitnessesVerified, src.WitnessesUnchecked)
		}
		if src.FactsPresent != 5 || src.CommittedCount != 5 || src.DroppedCount != 0 {
			t.Errorf("fact accounting = (present %d, committed %d, dropped %d), want (5, 5, 0)",
				src.FactsPresent, src.CommittedCount, src.DroppedCount)
		}

		// And the provenance survives the seams unchanged: a scorecard names
		// its own evidence, so the dataset's verdict has to reach it verbatim.
		pk, err := predictioneval.MaterializePairedKnowledge(ds)
		if err != nil {
			t.Fatalf("MaterializePairedKnowledge: %v", err)
		}
		if pk.Source != src {
			t.Errorf("materialized provenance = %+v, want the dataset's %+v", pk.Source, src)
		}
		if pk.Model.SupportedProducerRevision != predictioneval.SupportedProducerRevision ||
			pk.Model.ModelVersion != predictioneval.ModelVersion ||
			pk.Model.PolicyRevision != predictioneval.PolicyRevision {
			t.Errorf("model provenance = %+v, want this build's identity", pk.Model)
		}
	})
}

// ---------------------------------------------------------------------------
// E. Bounds and coherence
// ---------------------------------------------------------------------------

// TestLoadSessionRefusesEveryReadItCannotHandBackWhole covers the three ways
// LoadSession must fail rather than return something a downstream stage cannot
// tell apart from a complete dataset.
//
// The bound is the important one. A truncated dataset looks EXACTLY like a
// complete one to every stage after it: an attempt whose terminal fact was cut
// off is excluded as NO_TERMINAL_FACT, which is also what a genuinely
// unfinished attempt looks like. So the reader asks the store for one more
// fact than the caller's bound and refuses the whole load when the extra one
// exists, instead of silently handing back a prefix.
func TestLoadSessionRefusesEveryReadItCannotHandBackWhole(t *testing.T) {
	ctx := context.Background()
	repo := observationStore(t)

	// Six trivial facts: the bound is about how many rows exist, not what they
	// say, so these carry no envelope and no attempt id.
	facts := make([]analytics.PredictionObservation, 0, 6)
	for i := 0; i < 6; i++ {
		f := baseFact(analytics.KindChannelEvent)
		f.Payload = analytics.ObservationPayload{
			Phase:      "ROUND_UPDATED",
			RoundState: "ACTIVE",
			Counters:   map[string]int64{"outcomeCount": 2},
		}
		facts = append(facts, f)
	}
	epoch, _ := seedSession(t, repo, "bounds", facts)

	t.Run("a session larger than the bound is refused, not truncated", func(t *testing.T) {
		ds, err := reader.LoadSession(ctx, repo, epoch, 3)
		if !errors.Is(err, reader.ErrLimitExceeded) {
			t.Fatalf("LoadSession over a 6-fact session with limit 3 returned %d facts and err %v, "+
				"want ErrLimitExceeded. A silently truncated dataset is indistinguishable from a "+
				"complete one downstream", len(ds.Records), err)
		}
		if len(ds.Records) != 0 || ds.Source != (predictioneval.SourceProvenance{}) {
			t.Fatalf("the refused load still handed back %d facts and provenance %+v",
				len(ds.Records), ds.Source)
		}
	})

	t.Run("the bound is on the session, not on the read", func(t *testing.T) {
		// One under: still refused. Exactly at: accepted whole. This is the
		// boundary the reader's "ask for limit+1" probe exists to draw, and it
		// is off by one in either direction if the probe is dropped.
		if _, err := reader.LoadSession(ctx, repo, epoch, 5); !errors.Is(err, reader.ErrLimitExceeded) {
			t.Fatalf("a 6-fact session read with limit 5 returned %v, want ErrLimitExceeded", err)
		}
		ds, err := reader.LoadSession(ctx, repo, epoch, 6)
		if err != nil {
			t.Fatalf("a 6-fact session read with limit 6 returned %v, want the whole session", err)
		}
		if len(ds.Records) != 6 {
			t.Fatalf("read %d facts at the exact bound, want 6", len(ds.Records))
		}
		// A non-positive bound means the package default, not "no bound".
		ds, err = reader.LoadSession(ctx, repo, epoch, 0)
		if err != nil || len(ds.Records) != 6 {
			t.Fatalf("read with limit 0 returned %d facts and err %v, want the 6 facts under the "+
				"default bound of %d", len(ds.Records), err, reader.DefaultMaxRecords)
		}
	})

	t.Run("an epoch with no session row is not an empty session", func(t *testing.T) {
		// The distinction is load-bearing: an empty dataset would let a caller
		// conclude that nothing happened during a collector run that never
		// existed.
		ds, err := reader.LoadSession(ctx, repo, epoch+1_000_000, reader.DefaultMaxRecords)
		if !errors.Is(err, reader.ErrSessionNotFound) {
			t.Fatalf("LoadSession for an unknown epoch returned %d facts and err %v, want ErrSessionNotFound",
				len(ds.Records), err)
		}
		if len(ds.Records) != 0 || ds.Source != (predictioneval.SourceProvenance{}) {
			t.Fatalf("the not-found load handed back %d facts and provenance %+v",
				len(ds.Records), ds.Source)
		}
	})

	t.Run("a cancelled context aborts the load instead of returning a partial dataset", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		ds, err := reader.LoadSession(cancelled, repo, epoch, reader.DefaultMaxRecords)
		if err == nil {
			t.Fatalf("LoadSession under a cancelled context succeeded with %d facts; a caller that "+
				"cancelled must never be handed a dataset assembled anyway", len(ds.Records))
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("LoadSession under a cancelled context returned %v, want a cancellation", err)
		}
		if len(ds.Records) != 0 || ds.Source != (predictioneval.SourceProvenance{}) {
			t.Fatalf("the aborted load still handed back %d facts and provenance %+v — a partial "+
				"dataset is exactly what the cancellation path must not produce",
				len(ds.Records), ds.Source)
		}
	})
}

// ---------------------------------------------------------------------------
// F. Legacy obs-v1 is readable and acquires nothing
// ---------------------------------------------------------------------------

// TestALegacyProducerSessionStaysReadableAndYieldsNoDecisionCase defends the
// upgrade boundary from both sides at once.
//
// Facts written under the pre-envelope contract must stay READABLE — a trail
// whose whole point is surviving an upgrade would be worthless if a revision
// bump made every historical row unreadable — and they must yield NOTHING to
// the replay, because their decision inputs were never recorded. The dangerous
// implementation is not the one that crashes: it is the one that fills the
// missing envelope in from defaults, from the current configuration, or from a
// neighbouring fact, and produces a confident scorecard about a decision whose
// inputs nobody ever wrote down.
//
// The producer revision is stamped from a compile-time constant, so an obs-v1
// session cannot be written through the store from here. The equivalent is
// proven at the seam instead, with a dataset shaped exactly as a legacy one
// reads: a fact that is intact in every column and carries no envelope.
func TestALegacyProducerSessionStaysReadableAndYieldsNoDecisionCase(t *testing.T) {
	const legacyRevision = "obs-v1|policy-deadbeef"

	legacyFact := predictioneval.SourceRecord{
		ObservationID:      "1:obs-legacy:1",
		CollectorSessionID: "obs-legacy",
		CollectorEpoch:     1,
		CollectorSequence:  1,
		PoolInstanceID:     fixturePool,
		RoundIncarnationID: fixtureIncarnation,
		RoundCaptureOrigin: fixtureOrigin,
		EventID:            fixtureEvent,
		Kind:               predictioneval.KindAutoDecision,
		PayloadVersion:     predictioneval.SupportedPayloadVersion,
		ObservationSHA256:  "0f0e0d0c0b0a09080706050403020100",
		Payload: predictioneval.SourcePayload{
			// A terminal auto fact in every respect EXCEPT that it carries no
			// envelope, which is exactly what the previous contract wrote.
			// NO autoAttemptId. The discriminator and the envelope were added
			// in the SAME commit, so a genuine pre-envelope fact carries
			// neither. An earlier version of this fixture injected the counter,
			// which made the test pass on a fact shape obs-v1 never wrote — and
			// therefore made it no oracle at all for the case it names. The
			// counters below are the ones that contract actually emitted.
			Phase:      predictioneval.PhaseAutoDecided,
			RoundState: "ACTIVE",
			Decision:   "PLACE",
			ReasonCode: "OK",
			Counters:   map[string]int64{"stake": 50, "balance": 1000},
		},
	}

	ds := predictioneval.SourceDataset{
		Source: predictioneval.SourceProvenance{
			CollectorEpoch:     1,
			CollectorSessionID: "obs-legacy",
			ProducerRevision:   legacyRevision,
			SessionReading:     analytics.ReadingAsFinalized,
			CloseState:         analytics.SessionComplete,
			WitnessesVerified:  1,
			FactsPresent:       1,
			CommittedCount:     1,
		},
		Records: []predictioneval.SourceRecord{legacyFact},
	}

	pk, err := predictioneval.MaterializePairedKnowledge(ds)
	if err != nil {
		t.Fatalf("MaterializePairedKnowledge over a legacy session: %v. A readable pre-envelope "+
			"session is a data condition, never an error", err)
	}

	if len(pk.Attempts) != 0 {
		t.Fatalf("a pre-envelope session yielded %d attempts, want 0. Its decision inputs were "+
			"never recorded, so anything replayed from it was manufactured: %+v",
			len(pk.Attempts), pk.Attempts)
	}
	if len(pk.Excluded) != 1 {
		t.Fatalf("a pre-envelope session produced %d exclusions, want exactly 1 — an attempt that "+
			"cannot become a case must say so, never disappear silently: %+v",
			len(pk.Excluded), pk.Excluded)
	}
	if got := pk.Excluded[0].Reason; got != predictioneval.ExclusionLegacyProducerNoEnvelope {
		t.Errorf("exclusion reason = %q, want %q. Under the pre-envelope contract an absent "+
			"envelope IS the contract, not a defect in the fact, and the two are acted on "+
			"differently by a reader",
			got, predictioneval.ExclusionLegacyProducerNoEnvelope)
	}
	// The exclusion names the FACT, not an attempt: the pre-envelope producer
	// minted no discriminator, so there is no attempt identity to name. An
	// exclusion claiming one would be inventing the very linkage obs-v2 exists
	// to provide.
	if pk.Excluded[0].Key != nil {
		t.Errorf("the exclusion names attempt %+v, but the pre-envelope contract minted no "+
			"attempt id at all — the identity is not there to be named",
			*pk.Excluded[0].Key)
	}
	if pk.Excluded[0].ObservationID != legacyFact.ObservationID {
		t.Errorf("the exclusion names observation %q, want %q: an operator has to be able to find "+
			"the fact that was refused", pk.Excluded[0].ObservationID, legacyFact.ObservationID)
	}

	foundForeign := false
	for _, a := range pk.Anomalies {
		if a == predictioneval.AnomalyForeignProducerRevision {
			foundForeign = true
		}
	}
	if !foundForeign {
		t.Errorf("anomalies = %v, want %q: the whole reading is qualified by having been written "+
			"under a contract this model does not replay, and that has to be reported beside "+
			"the exclusion rather than instead of it",
			pk.Anomalies, predictioneval.AnomalyForeignProducerRevision)
	}

	// Readable, not merely rejected: the reading still carries the session's
	// own provenance verbatim, which is what makes a legacy session inspectable
	// at all.
	if pk.Source.ProducerRevision != legacyRevision {
		t.Errorf("the legacy session's revision came back as %q, want %q carried verbatim",
			pk.Source.ProducerRevision, legacyRevision)
	}
}
