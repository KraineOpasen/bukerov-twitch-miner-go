package miner

import (
	"reflect"
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
