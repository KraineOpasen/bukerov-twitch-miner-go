package miner

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/pubsub"
)

// TestObservationAdapterIsFieldComplete is the drift guard for the one place
// the producer's type and the store's type meet. Both are flat structs with
// the same field names, so reflection can prove the adapter forwards EVERY
// field rather than silently dropping one that a later change adds.
func TestObservationAdapterIsFieldComplete(t *testing.T) {
	in := pubsub.PredictionObservation{
		PoolInstanceID:               "pool-9",
		RoutedChannelID:              "chan-routed",
		RoutedLogin:                  "routed-login",
		RoundOwnerChannelID:          "chan-owner",
		RoundOwnerLogin:              "owner-login",
		RetentionGroupOwnerChannelID: "chan-retention",
		RetentionGroupOwnerLogin:     "retention-login",
		RoundIncarnationID:           "round:pool-9:7",
		EventID:                      "event-42",
		Kind:                         pubsub.ObsKindPlacement,
		SourceTopicType:              "predictions-channel-v1",
		SourceMessageType:            "event-updated",
		ProducerAtMS:                 1_700_000_000_000,
		ProducerTimeSource:           pubsub.ObsTimeProducer,
		ReceivedAtMS:                 1_700_000_000_500,
		ConnectionIndex:              4,
		ConnectionGeneration:         13,
		ConnectionSequence:           99,
		ConnectionKnown:              true,
		Payload: pubsub.ObservationPayload{
			Phase:       "CALL_RETURNED",
			RoundState:  "ACTIVE",
			Decision:    "PLACE",
			ReasonCode:  "OK",
			ErrorClass:  "NONE",
			Manual:      boolValue(true),
			OutcomeSlot: intValue(1),
			Outcomes: []pubsub.ObservationOutcome{
				{Slot: 0, Color: "BLUE", TotalPoints: 300, TotalUsers: 3, TopPredictorsExamined: 2},
				{Slot: 1, Color: "PINK", TotalPoints: 200, TotalUsers: 2},
			},
			Counters: map[string]int64{"stake": 250},
			Presence: map[string]string{"event": pubsub.ObsPresent},
		},
	}

	// Every field of the producer's struct must be non-zero above, or the
	// comparison below could pass while the adapter drops it.
	assertNoZeroFields(t, reflect.ValueOf(in), "pubsub.PredictionObservation")

	got := toAnalyticsObservation(in)
	want := analytics.PredictionObservation{
		PoolInstanceID:               "pool-9",
		RoutedChannelID:              "chan-routed",
		RoutedLogin:                  "routed-login",
		RoundOwnerChannelID:          "chan-owner",
		RoundOwnerLogin:              "owner-login",
		RetentionGroupOwnerChannelID: "chan-retention",
		RetentionGroupOwnerLogin:     "retention-login",
		RoundIncarnationID:           "round:pool-9:7",
		EventID:                      "event-42",
		Kind:                         analytics.KindPlacement,
		SourceTopicType:              analytics.TopicTypePredictionsChannel,
		SourceMessageType:            analytics.MessageTypeEventUpdated,
		ProducerAtMS:                 1_700_000_000_000,
		ProducerTimeSource:           analytics.TimeSourceProducer,
		ReceivedAtMS:                 1_700_000_000_500,
		ConnectionIndex:              4,
		ConnectionGeneration:         13,
		ConnectionSequence:           99,
		ConnectionKnown:              true,
		Payload: analytics.ObservationPayload{
			Phase:       "CALL_RETURNED",
			RoundState:  "ACTIVE",
			Decision:    "PLACE",
			ReasonCode:  "OK",
			ErrorClass:  "NONE",
			Manual:      boolValue(true),
			OutcomeSlot: intValue(1),
			Outcomes: []analytics.ObservationOutcome{
				{Slot: 0, Color: "BLUE", TotalPoints: 300, TotalUsers: 3, TopPredictorsExamined: 2},
				{Slot: 1, Color: "PINK", TotalPoints: 200, TotalUsers: 2},
			},
			Counters: map[string]int64{"stake": 250},
			Presence: map[string]string{"event": "PRESENT"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("adapter dropped or altered a field:\n got=%+v\nwant=%+v", got, want)
	}
}

// assertNoZeroFields fails when any exported field of a struct is still its
// zero value, so a field-completeness fixture cannot rot into a partial one.
func assertNoZeroFields(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	typ := v.Type()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		name := path + "." + typ.Field(i).Name
		if f.Kind() == reflect.Struct {
			assertNoZeroFields(t, f, name)
			continue
		}
		if f.IsZero() {
			t.Fatalf("%s is still the zero value; the adapter fixture must exercise every field", name)
		}
	}
}

func boolValue(b bool) *bool { return &b }
func intValue(i int) *int    { return &i }

// TestObservationKindVocabulariesAgree pins the two packages' closed
// vocabularies together. They are declared independently — deliberately, so
// neither package depends on the other — so a test has to hold them equal.
func TestObservationKindVocabulariesAgree(t *testing.T) {
	for _, pair := range [][2]string{
		{pubsub.ObsKindSourceUnknown, analytics.KindSourceUnknown},
		{pubsub.ObsKindChannelEvent, analytics.KindChannelEvent},
		{pubsub.ObsKindScheduleDecision, analytics.KindScheduleDecision},
		{pubsub.ObsKindAutoDecision, analytics.KindAutoDecision},
		{pubsub.ObsKindManualControl, analytics.KindManualControl},
		{pubsub.ObsKindPlacement, analytics.KindPlacement},
		{pubsub.ObsKindUserPredictionMade, analytics.KindUserPredictionMade},
		{pubsub.ObsKindUserTerminal, analytics.KindUserTerminal},
		{pubsub.ObsKindRoundCleanup, analytics.KindRoundCleanup},
		{pubsub.ObsTimeProducer, analytics.TimeSourceProducer},
		{pubsub.ObsTimeServer, analytics.TimeSourceServer},
		{pubsub.ObsTimeReceiver, analytics.TimeSourceReceiver},
	} {
		if pair[0] != pair[1] {
			t.Fatalf("vocabularies drifted: pubsub %q vs analytics %q", pair[0], pair[1])
		}
	}
}

// TestObservationSinkTolerAtesNilService proves the adapter is safe to hold
// before analytics exists and never panics on a producer path.
func TestObservationSinkToleratesNilService(t *testing.T) {
	sink := predictionObservationSink{}
	sink.RecordPredictionObservation(pubsub.PredictionObservation{PoolInstanceID: "pool"})
}

// TestSinkSatisfiesTheProducerContract proves the adapter is what the pool
// will actually accept, so wiring cannot silently no-op.
func TestSinkSatisfiesTheProducerContract(t *testing.T) {
	var _ pubsub.PredictionObservationSink = predictionObservationSink{}
}

// TestAttachPredictionObservationsIsSafeWithoutAnalytics proves a miner with
// no analytics service wires nothing, leaving every producer call site a
// no-op — the configuration every pre-existing test runs in.
func TestAttachPredictionObservationsIsSafeWithoutAnalytics(t *testing.T) {
	m := &Miner{}
	m.attachPredictionObservations() // no pool, no analytics
	m.wsPool = pubsub.NewWebSocketPool(nil, nil, nil, config.RateLimitSettings{})
	m.attachPredictionObservations() // pool but no analytics
	if m.wsPool.PoolInstanceID() == "" {
		t.Fatal("a production pool must mint an instance identity")
	}
}

// recordingProducerSink captures what a real pool emits, so a miner-level test
// can assert on the trail rather than on the call it believes it made.
type recordingProducerSink struct {
	mu        sync.Mutex
	got       []pubsub.PredictionObservation
	uncertain int64
}

func (r *recordingProducerSink) RecordPredictionObservation(o pubsub.PredictionObservation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, o)
}

func (r *recordingProducerSink) BeginPredictionProducerEpisode() func() { return func() {} }

func (r *recordingProducerSink) NotePredictionProducerShutdownUncertain() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.uncertain++
}

func (r *recordingProducerSink) uncertainShutdownCount() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.uncertain
}

func (r *recordingProducerSink) all() []pubsub.PredictionObservation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]pubsub.PredictionObservation(nil), r.got...)
}

// TestTheMinerSettlesItsPoolAsAProducer is the regression for a defect an
// independent review found: deleting the miner's shutdown wiring left the
// whole module green, so a pool that could not prove it had stopped would
// finalize its session as COMPLETE. And there was no wiring at all for the
// other half — a pool never closed leaves nothing to check.
func TestTheMinerSettlesItsPoolAsAProducer(t *testing.T) {
	t.Run("attaching makes the pool a live producer", func(t *testing.T) {
		m := &Miner{}
		m.wsPool = pubsub.NewWebSocketPool(nil, nil, nil, config.RateLimitSettings{})
		sink := &recordingProducerSink{}
		// The real attach path needs an analytics service; wire the sink
		// directly and hold the handle the same way attach does.
		m.settlePredictionProducer = m.wsPool.SetPredictionObservationSink(sink)

		if m.settlePredictionProducer == nil {
			t.Fatal("wiring a sink produced no settle handle; a pool that is never closed would " +
				"then leave nothing for the collector to notice")
		}
		if got := sink.uncertainShutdownCount(); got != 0 {
			t.Fatalf("attaching recorded %d uncertain shutdowns, want 0", got)
		}

		// A pool that could not prove it stopped.
		m.settlePredictionObservations(errors.New("connections not joined"))
		if got := sink.uncertainShutdownCount(); got != 1 {
			t.Fatalf("a pool that could not prove it stopped recorded %d uncertainties, want 1", got)
		}
	})

	// The shutdown path itself. It is asserted structurally because reaching
	// it needs a fully built miner: the property is that the pool's OWN Close
	// result is the evidence the handle is settled with, and that the settle
	// happens on the shutdown path at all — deleting the line otherwise left
	// the whole module green.
	t.Run("stop settles with the pool's own Close result", func(t *testing.T) {
		src, err := os.ReadFile("miner.go")
		if err != nil {
			t.Fatal(err)
		}
		i := strings.Index(string(src), "poolErr := m.wsPool.Close()")
		if i < 0 {
			t.Fatal("the pool is no longer closed on the shutdown path; this test no longer " +
				"asserts anything")
		}
		after := string(src)[i:]
		j := strings.Index(after, "m.settlePredictionObservations(poolErr)")
		if j < 0 {
			t.Fatal("the shutdown path does not settle the Prediction producer with the pool's " +
				"own Close result; a pool that could not prove it stopped would leave its " +
				"session finalizing as if everything had been observed")
		}
		if k := strings.Index(after, "drainErrs = append(drainErrs, poolErr)"); k < 0 || j > k {
			t.Fatal("the settle does not happen before the Close result is aggregated away")
		}
	})

	t.Run("no analytics, nothing to settle", func(t *testing.T) {
		m := &Miner{}
		m.wsPool = pubsub.NewWebSocketPool(nil, nil, nil, config.RateLimitSettings{})
		m.attachPredictionObservations() // pool but no analytics
		// Settling is safe and does nothing: this is the configuration every
		// pre-existing test runs in.
		m.settlePredictionObservations(errors.New("anything"))
	})
}

// TestAnOperatorActionOpensExactlyOneMinerRoot closes a coverage gap an
// independent review found: the miner's choice of entry point had no oracle at
// all. Reverting overview.go to the pool's DIRECT entry point left the whole
// module green while every dashboard action was recorded as if it had been
// initiated at the pool — destroying the operator-origin distinction the trail
// exists to carry — and dropped the sealed correlation token with it.
func TestAnOperatorActionOpensExactlyOneMinerRoot(t *testing.T) {
	m := &Miner{}
	m.wsPool = pubsub.NewWebSocketPool(nil, nil, nil, config.RateLimitSettings{})
	sink := &recordingProducerSink{}
	m.wsPool.SetPredictionObservationSink(sink)

	// An untracked round: the action is refused, which is the point — the
	// trail must still show where it entered and tie its facts together.
	if _, err := m.PlaceManualBet("no-such-round", "o1", 100); err == nil {
		t.Fatal("a manual bet on an untracked round was accepted")
	}

	var roots []pubsub.PredictionObservation
	for _, o := range sink.all() {
		switch o.Payload.Phase {
		case "MANUAL_MINER_ROOT", "MANUAL_DIRECT_ROOT":
			roots = append(roots, o)
		}
	}
	if len(roots) != 1 {
		t.Fatalf("an operator action opened %d manual roots, want exactly 1", len(roots))
	}
	if roots[0].Payload.Phase != "MANUAL_MINER_ROOT" {
		t.Fatalf("the action was recorded as %q; a dashboard bet that opens the pool's DIRECT "+
			"root is indistinguishable from one initiated at the pool itself",
			roots[0].Payload.Phase)
	}
	token := roots[0].Payload.Counters["manualActionId"]
	if token == 0 {
		t.Fatal("the manual root carries no correlation token")
	}
	// Every fact of the action shares it — that is what ties the root to a
	// descendant naming a round this pool never admitted.
	for _, o := range sink.all() {
		if o.Kind != "manual_control" {
			continue
		}
		if got := o.Payload.Counters["manualActionId"]; got != token {
			t.Fatalf("fact %q carries token %d, want the action's %d: %+v",
				o.Payload.Phase, got, token, o.Payload)
		}
	}
}

// TestSetAnalyticsServiceNilClearsOwnership is the ownership fix: injecting
// nil must NOT leave the miner believing something external owns a service,
// because it would then build one for itself in setupComponents and never
// close it — leaking that service's observation collector goroutine.
func TestSetAnalyticsServiceNilClearsOwnership(t *testing.T) {
	m := &Miner{}

	m.SetAnalyticsService(nil)
	if m.externalAnalytics {
		t.Fatal("SetAnalyticsService(nil) claimed external ownership of a service that does not exist")
	}
	if m.analyticsSvc != nil {
		t.Fatal("SetAnalyticsService(nil) left a service behind")
	}

	svc := &analytics.Service{}
	m.SetAnalyticsService(svc)
	if !m.externalAnalytics || m.analyticsSvc != svc {
		t.Fatal("SetAnalyticsService(svc) did not take external ownership")
	}

	// And back to nil clears it again — ownership follows the argument.
	m.SetAnalyticsService(nil)
	if m.externalAnalytics {
		t.Fatal("ownership was not cleared on a second nil injection")
	}
}

// TestObservationSinkIsAttachedAfterTheServiceExists is the regression for a
// defect an independent review found: attachPredictionObservations ran BEFORE
// the block that builds a self-owned analytics service, so on that path the
// pool kept a nil sink and P1 was silently inert for the life of the process.
//
// It is asserted against setupComponents' source because the ordering is the
// whole defect — a runtime assertion would need a fully wired miner, and the
// bug is precisely that the wiring order was wrong.
func TestObservationSinkIsAttachedAfterTheServiceExists(t *testing.T) {
	src, err := os.ReadFile("miner.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func (m *Miner) setupComponents(")
	if start < 0 {
		t.Fatal("setupComponents not found")
	}
	end := strings.Index(body[start:], "\n}\n")
	if end < 0 {
		t.Fatal("could not bound setupComponents")
	}
	fn := body[start : start+end]

	buildAt := strings.Index(fn, "analytics.NewService(")
	attachAt := strings.Index(fn, "m.attachPredictionObservations()")
	if buildAt < 0 || attachAt < 0 {
		t.Fatalf("expected both the analytics build and the sink attach in setupComponents (build=%d attach=%d)", buildAt, attachAt)
	}
	if attachAt < buildAt {
		t.Fatal("the observation sink is attached BEFORE the analytics service is built; on the self-owned path the pool would keep a nil sink and capture would be silently inert")
	}
	if strings.Count(fn, "m.attachPredictionObservations()") != 1 {
		t.Fatal("the sink must be attached exactly once, after both ownership paths have settled")
	}
	// A self-owned service has no App lifecycle step, so the miner must start
	// it itself or the collector never bootstraps.
	if !strings.Contains(fn, "svc.Start()") {
		t.Fatal("a self-owned analytics service is never started; its observation collector would never bootstrap")
	}
}
