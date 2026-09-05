package pubsub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// recordingSink captures everything the pool produces. It is deliberately
// nonblocking and lock-cheap, exactly as the sink contract requires: a sink
// that blocked here would deadlock the producer paths it is called from.
type recordingSink struct {
	mu        sync.Mutex
	got       []PredictionObservation
	hook      func(PredictionObservation)
	begun     int64
	settled   int64
	uncertain int64
}

func (r *recordingSink) RecordPredictionObservation(obs PredictionObservation) {
	r.mu.Lock()
	r.got = append(r.got, obs)
	hook := r.hook
	r.mu.Unlock()
	if hook != nil {
		hook(obs)
	}
}

func (r *recordingSink) BeginPredictionProducerEpisode() func() {
	r.mu.Lock()
	r.begun++
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			r.settled++
			r.mu.Unlock()
		})
	}
}

// NotePredictionProducerShutdownUncertain records a shutdown whose evidence was
// inconclusive.
func (r *recordingSink) NotePredictionProducerShutdownUncertain() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.uncertain++
}

// uncertainShutdowns reports how many inconclusive shutdowns were recorded.
func (r *recordingSink) uncertainShutdowns() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.uncertain
}

// episodes reports how many producer episodes were registered and how many
// have settled.
func (r *recordingSink) episodes() (begun, settled int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.begun, r.settled
}

func (r *recordingSink) all() []PredictionObservation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]PredictionObservation(nil), r.got...)
}

// reset forgets the facts recorded so far, so a test can set a fixture up and
// then assert only on what the step under test produced.
func (r *recordingSink) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = nil
}

func (r *recordingSink) phases() []string {
	var out []string
	for _, o := range r.all() {
		out = append(out, o.Kind+"/"+o.Payload.Phase+"/"+o.Payload.ReasonCode)
	}
	return out
}

// observedPool is newTestPool plus an instance identity and a recording sink.
func observedPool(t *testing.T, placer predictionPlacer) (*WebSocketPool, *recordingSink) {
	t.Helper()
	p := newTestPool(placer)
	p.instanceID = "pool-test"
	sink := &recordingSink{}
	p.SetPredictionObservationSink(sink)
	return p, sink
}

// admitRound installs a tracked round the way the pool's OWN admission path
// does: with a local admission incarnation allocated from this pool instance
// and its admission counter. addRound (in manual_bet_test.go, which predates
// P1 and is not this task's to change) installs the round without one, which
// is right for the betting tests that use it and wrong for every observation
// test — a round nobody admitted has no round identity to observe.
func admitRound(p *WebSocketPool, s *models.Streamer, eventID string) *models.EventPrediction {
	ep := addRound(p, s, eventID)
	p.mu.Lock()
	p.control[eventID].incarnation = p.newRoundIncarnation()
	p.mu.Unlock()
	return ep
}

// TestObservationSinkAbsentIsANoOp is the most important producer property:
// with no sink wired, observing costs nothing and changes nothing. Every
// pre-existing test in this package runs in exactly this configuration.
func TestObservationSinkAbsentIsANoOp(t *testing.T) {
	placer := &fakePlacer{}
	p := newTestPool(placer)
	s := newTestStreamer(1000)
	admitRound(p, s, "e1")

	if p.observing() {
		t.Fatal("a pool with no sink reports itself as observing")
	}
	// Every producer entry point must tolerate the absent sink.
	p.observeRoundFact("e1", "chan-1", "streamer", ObsKindAutoDecision, ObservationPayload{Phase: "AUTO_DUE"})
	p.observeAutoSkip("e1", "chan-1", "streamer", "OK", nil)
	p.observeManualPhase("e1", "chan-1", "streamer", "MANUAL_DIRECT_ROOT", "OK", nil)
	p.observeRoundCleanup("e1", "chan-1", "streamer", "round:x:1", "CLEANUP_APPLIED", "OK")
	p.observeUnclassifiedFrame(&PubSubMessage{Topic: NewTopic(TopicPredictionsChannel, "chan-1")}, s, "event", ObsNotObserved)

	if _, err := p.PlaceManualBet("e1", "o1", 100); err != nil {
		t.Fatalf("manual bet with no sink: %v", err)
	}
	if placer.callCount() != 1 {
		t.Fatalf("placement calls = %d, want exactly 1", placer.callCount())
	}
}

// TestObservationDoesNotChangeManualBetBehaviour proves the observer is
// invisible to the placement contract: identical return values, identical
// Twitch call count and identical arguments with and without a sink.
func TestObservationDoesNotChangeManualBetBehaviour(t *testing.T) {
	run := func(observed bool) (string, int, string, int, error) {
		placer := &fakePlacer{}
		var p *WebSocketPool
		if observed {
			p, _ = observedPool(t, placer)
		} else {
			p = newTestPool(placer)
		}
		s := newTestStreamer(1000)
		admitRound(p, s, "e1")
		title, err := p.PlaceManualBet("e1", "o2", 250)
		return title, placer.callCount(), placer.lastID, placer.lastAmt, err
	}
	t1, c1, id1, a1, e1 := run(false)
	t2, c2, id2, a2, e2 := run(true)
	if t1 != t2 || !sameError(e1, e2) || c1 != c2 || id1 != id2 || a1 != a2 {
		t.Fatalf("observing changed the placement:\n unobserved: %q %v calls=%d id=%q amt=%d\n   observed: %q %v calls=%d id=%q amt=%d",
			t1, e1, c1, id1, a1, t2, e2, c2, id2, a2)
	}

	// The same for a REJECTED placement.
	rejected := func(observed bool) (string, int, error) {
		placer := &fakePlacer{err: errors.New("twitch says no")}
		var p *WebSocketPool
		if observed {
			p, _ = observedPool(t, placer)
		} else {
			p = newTestPool(placer)
		}
		s := newTestStreamer(1000)
		admitRound(p, s, "e1")
		title, err := p.PlaceManualBet("e1", "o1", 100)
		return title, placer.callCount(), err
	}
	rt1, rc1, re1 := rejected(false)
	rt2, rc2, re2 := rejected(true)
	if rt1 != rt2 || !sameError(re1, re2) || rc1 != rc2 {
		t.Fatalf("observing changed a rejected placement: %q/%v/%d vs %q/%v/%d", rt1, re1, rc1, rt2, re2, rc2)
	}
}

func sameError(a, b error) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return a.Error() == b.Error()
	}
}

// TestManualRootsAreMutuallyExclusive proves exactly one manual root is opened
// per operator action: the direct entry point opens MANUAL_DIRECT_ROOT, the
// relayed one opens MANUAL_MINER_ROOT, and neither ever opens both.
func TestManualRootsAreMutuallyExclusive(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(p *WebSocketPool) (string, error)
		want string
	}{
		{"direct", func(p *WebSocketPool) (string, error) { return p.PlaceManualBet("e1", "o1", 100) }, "MANUAL_DIRECT_ROOT"},
		{"relayed", func(p *WebSocketPool) (string, error) {
			return p.PlaceManualBetRelayed("e1", "o1", 100, p.NextManualActionToken())
		}, "MANUAL_MINER_ROOT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, sink := observedPool(t, &fakePlacer{})
			admitRound(p, newTestStreamer(1000), "e1")
			if _, err := tc.call(p); err != nil {
				t.Fatal(err)
			}
			var roots []string
			for _, o := range sink.all() {
				switch o.Payload.Phase {
				case "MANUAL_DIRECT_ROOT", "MANUAL_MINER_ROOT":
					roots = append(roots, o.Payload.Phase)
				}
			}
			if len(roots) != 1 || roots[0] != tc.want {
				t.Fatalf("roots = %v, want exactly [%s]", roots, tc.want)
			}
		})
	}
}

// TestManualPlacementEmitsEveryPhase proves the manual control trail is
// complete: the root, the pool lookup, eligibility, arguments, reservation,
// validation, the single placement call's two sides, and execution.
func TestManualPlacementEmitsEveryPhase(t *testing.T) {
	p, sink := observedPool(t, &fakePlacer{})
	admitRound(p, newTestStreamer(1000), "e1")
	if _, err := p.PlaceManualBet("e1", "o1", 100); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"manual_control/MANUAL_DIRECT_ROOT/OK",
		"manual_control/MANUAL_POOL_LOOKUP/OK",
		"manual_control/MANUAL_ELIGIBILITY/OK",
		"manual_control/MANUAL_ARGUMENTS/OK",
		"manual_control/MANUAL_RESERVATION/OK",
		"manual_control/MANUAL_VALIDATION/OK",
		"placement/CALL_STARTED/OK",
		"placement/CALL_RETURNED/OK",
		"manual_control/MANUAL_EXECUTION/OK",
	}
	if got := sink.phases(); !reflect.DeepEqual(got, want) {
		t.Fatalf("manual phases =\n %v\nwant\n %v", got, want)
	}
}

// TestPlacementCallIsBracketedExactlyOnce proves CALL_STARTED lands
// immediately before the ONE Twitch call and CALL_RETURNED immediately after
// it — never twice, never around a call that did not happen.
func TestPlacementCallIsBracketedExactlyOnce(t *testing.T) {
	placer := &fakePlacer{}
	p, sink := observedPool(t, placer)
	admitRound(p, newTestStreamer(1000), "e1")

	var atCall []string
	sink.hook = func(obs PredictionObservation) {
		if obs.Kind == ObsKindPlacement {
			atCall = append(atCall, obs.Payload.Phase+":"+itoaTest(placer.callCount()))
		}
	}
	if _, err := p.PlaceManualBet("e1", "o1", 100); err != nil {
		t.Fatal(err)
	}
	// CALL_STARTED must be observed with the call not yet made, CALL_RETURNED
	// with exactly one call made.
	want := []string{"CALL_STARTED:0", "CALL_RETURNED:1"}
	if !reflect.DeepEqual(atCall, want) {
		t.Fatalf("placement bracketing = %v, want %v", atCall, want)
	}
	if placer.callCount() != 1 {
		t.Fatalf("Twitch calls = %d, want exactly 1", placer.callCount())
	}
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestPlacementFailureClassIsClosed proves a placement failure is recorded as
// a CLOSED class and that the error's own text never reaches a fact.
func TestPlacementFailureClassIsClosed(t *testing.T) {
	secret := "Post \"https://gql.twitch.tv/gql\": x-device-id=SECRET123 rejected"
	p, sink := observedPool(t, &fakePlacer{err: errors.New(secret)})
	admitRound(p, newTestStreamer(1000), "e1")
	if _, err := p.PlaceManualBet("e1", "o1", 100); err == nil {
		t.Fatal("expected the placement to fail")
	}
	var sawReturn bool
	for _, o := range sink.all() {
		blob, _ := json.Marshal(o)
		if strings.Contains(string(blob), "SECRET123") || strings.Contains(string(blob), "gql.twitch.tv") {
			t.Fatalf("a raw placement error reached a fact: %s", blob)
		}
		if o.Kind == ObsKindPlacement && o.Payload.Phase == "CALL_RETURNED" {
			sawReturn = true
			if o.Payload.ErrorClass != "REJECTED_BY_TWITCH" {
				t.Fatalf("error class = %q, want REJECTED_BY_TWITCH", o.Payload.ErrorClass)
			}
		}
	}
	if !sawReturn {
		t.Fatal("no CALL_RETURNED fact for a failed placement")
	}
}

func TestPlacementErrorClassMapping(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, "NONE"},
		{ErrPredictionNotFound, "ROUND_CLOSED"},
		{ErrRoundClosed, "ROUND_CLOSED"},
		{ErrAlreadyBet, "ROUND_CLOSED"},
		{ErrAutoBetPlaced, "ROUND_CLOSED"},
		{ErrStreamerOffline, "ROUND_CLOSED"},
		{ErrOutcomeNotFound, "INVALID_ARGUMENT"},
		{ErrInvalidAmount, "INVALID_ARGUMENT"},
		{ErrAmountTooLow, "INVALID_ARGUMENT"},
		{ErrManualBetInFlight, "INVALID_ARGUMENT"},
		{ErrInsufficientPoints, "NOT_ENOUGH_POINTS"},
		{errors.New("anything else"), "REJECTED_BY_TWITCH"},
	} {
		if got := placementErrorClass(tc.err); got != tc.want {
			t.Fatalf("placementErrorClass(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// TestChannelFrameObservationIsSanitized proves what a round frame contributes
// to the trail: the topic TYPE (never Topic.String()), the message type, the
// bounded outcome aggregates — and never a predictor identity, a title, or the
// transport fingerprint.
func TestChannelFrameObservationIsSanitized(t *testing.T) {
	p, sink := observedPool(t, &fakePlacer{})
	s := newTestStreamer(1000)
	p.streamers = []*models.Streamer{s}

	msg := &PubSubMessage{
		Topic:            NewTopic(TopicPredictionsChannel, "chan-1"),
		Type:             "event-updated",
		ChannelID:        "chan-1",
		Timestamp:        time.Unix(1700000000, 0),
		TimestampSource:  TimestampFromProducer,
		EventFingerprint: "sha256:transport-fingerprint-must-not-be-persisted",
		Data: map[string]interface{}{
			"event": map[string]interface{}{
				"id":                        "e-secret",
				"status":                    "LOCKED",
				"title":                     "A title that must never be stored",
				"prediction_window_seconds": float64(120),
				"outcomes": []interface{}{
					map[string]interface{}{
						"id": "o1", "title": "Yes", "color": "BLUE",
						"total_points": float64(300), "total_users": float64(3),
						"top_predictors": []interface{}{
							map[string]interface{}{"user_display_name": "PredictorOne"},
							map[string]interface{}{"user_display_name": "PredictorTwo"},
						},
					},
					map[string]interface{}{"id": "o2", "title": "No", "color": "PINK",
						"total_points": float64(200), "total_users": float64(2)},
				},
			},
		},
		ConnectionIndex: 3, ConnectionGeneration: 9, ConnectionSequence: 42, ConnectionKnown: true,
	}
	p.handlePredictionChannel(msg, s)

	got := sink.all()
	if len(got) != 1 {
		t.Fatalf("emitted %d facts for one round frame, want 1", len(got))
	}
	o := got[0]
	if o.Kind != ObsKindChannelEvent || o.Payload.Phase != "ROUND_UPDATED" {
		t.Fatalf("kind/phase = %s/%s", o.Kind, o.Payload.Phase)
	}
	if o.SourceTopicType != string(TopicPredictionsChannel) || o.SourceMessageType != "event-updated" {
		t.Fatalf("source = %s/%s", o.SourceTopicType, o.SourceMessageType)
	}
	// This frame is about a round THIS pool never admitted, so it names no
	// local round and belongs to no retention group — but it keeps its routed
	// identity, which is what a privacy erasure reaches it by.
	if o.RoutedChannelID != "chan-1" || o.RoutedLogin != "streamer" {
		t.Fatalf("identity = %+v", o)
	}
	if o.RoundIncarnationID != "" || o.RetentionGroupOwnerChannelID != "" {
		t.Fatalf("an untracked round claimed a retention group: %+v", o)
	}
	if o.ProducerTimeSource != ObsTimeProducer || o.ProducerAtMS != time.Unix(1700000000, 0).UnixMilli() {
		t.Fatalf("producer time = %d/%s", o.ProducerAtMS, o.ProducerTimeSource)
	}
	if o.ConnectionIndex != 3 || o.ConnectionGeneration != 9 || o.ConnectionSequence != 42 || !o.ConnectionKnown {
		t.Fatalf("connection provenance = %+v", o)
	}
	if len(o.Payload.Outcomes) != 2 {
		t.Fatalf("outcomes = %+v", o.Payload.Outcomes)
	}
	if o.Payload.Outcomes[0].TopPredictorsExamined != 2 {
		t.Fatalf("examined predictors = %d, want 2 (the LENGTH only)", o.Payload.Outcomes[0].TopPredictorsExamined)
	}

	blob, _ := json.Marshal(o)
	for _, forbidden := range []string{
		"A title that must never be stored",
		"PredictorOne", "PredictorTwo",
		"sha256:transport-fingerprint-must-not-be-persisted",
		msg.Topic.String(),
	} {
		if strings.Contains(string(blob), forbidden) {
			t.Fatalf("forbidden content %q reached a fact: %s", forbidden, blob)
		}
	}
}

// TestOutcomeProjectionReportsABreachInsteadOfHidingIt proves the producer
// builds a bounded projection that still lets the store SEE a cap breach.
//
// It used to project exactly the first 64 outcomes and clamp the predictor
// cohort to 256, so a round with 104 outcomes and a 356-strong cohort arrived
// at the store looking like an ordinary 64-outcome round with a 256 cohort,
// and was committed as a complete fact. The producer's bound must not be able
// to launder an over-cap round into an in-cap one.
//
// The projection stops one past the ceiling — enough for the store to refuse
// the fact, not enough to be unbounded — and the cohort SIZE is reported
// truthfully. A size is not an identity: nothing here reads an entry, so no
// predictor is representable at any length.
func TestOutcomeProjectionReportsABreachInsteadOfHidingIt(t *testing.T) {
	predictors := make([]interface{}, maxTopPredictorsExamined+100)
	for i := range predictors {
		predictors[i] = map[string]interface{}{"user_display_name": "someone"}
	}
	build := func(n int) []interface{} {
		raw := make([]interface{}, n)
		for i := range raw {
			raw[i] = map[string]interface{}{
				"color": "BLUE", "total_points": float64(i), "top_predictors": predictors}
		}
		return raw
	}

	// At the ceiling: projected whole, and the store accepts it.
	atCap := projectOutcomes(build(maxObservedOutcomes))
	if len(atCap) != maxObservedOutcomes {
		t.Fatalf("projected %d of %d outcomes at the ceiling", len(atCap), maxObservedOutcomes)
	}

	// Past it: the projection stays bounded but is visibly over the ceiling.
	over := projectOutcomes(build(maxObservedOutcomes + 40))
	if len(over) != maxObservedOutcomes+1 {
		t.Fatalf("projected %d outcomes, want exactly one past the %d ceiling so the store can "+
			"refuse the fact", len(over), maxObservedOutcomes)
	}
	for i, o := range over {
		if o.Slot != i {
			t.Fatalf("outcome %d has slot %d", i, o.Slot)
		}
		if o.TopPredictorsExamined != len(predictors) {
			t.Fatalf("cohort = %d, want the real %d: a clamped count would make an over-cap "+
				"cohort indistinguishable from one exactly at the cap",
				o.TopPredictorsExamined, len(predictors))
		}
	}

	// No predictor identity is representable at any size.
	blob, err := json.Marshal(over)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "someone") {
		t.Fatalf("a predictor identity reached the projection: %s", blob)
	}
}

// TestUnreadableFrameIsObservedAsSourceUnknown proves a Prediction frame that
// cannot be read stays visible as a fact instead of vanishing — and that it
// claims no round, so it can never be swept as part of one.
func TestUnreadableFrameIsObservedAsSourceUnknown(t *testing.T) {
	p, sink := observedPool(t, &fakePlacer{})
	s := newTestStreamer(1000)
	p.handlePredictionChannel(&PubSubMessage{
		Topic: NewTopic(TopicPredictionsChannel, "chan-1"), Type: "event-updated",
		ChannelID: "chan-1", Data: map[string]interface{}{"not-an-event": 1},
	}, s)

	got := sink.all()
	if len(got) != 1 || got[0].Kind != ObsKindSourceUnknown {
		t.Fatalf("facts = %+v, want one source_unknown", got)
	}
	if got[0].Payload.Presence["event"] != ObsAbsentOnWire {
		t.Fatalf("presence = %v, want event ABSENT_ON_WIRE", got[0].Payload.Presence)
	}
	if got[0].RetentionGroupOwnerChannelID != "" {
		t.Fatal("an unreadable frame claimed a retention group; it names no round")
	}
}

// TestRoundCleanupIsObserved proves a dropped round leaves a fact behind that
// still names the round it was about.
func TestRoundCleanupIsObserved(t *testing.T) {
	p, sink := observedPool(t, &fakePlacer{})
	admitRound(p, newTestStreamer(1000), "e1")
	p.removePrediction("e1")

	got := sink.all()
	if len(got) != 1 || got[0].Kind != ObsKindRoundCleanup || got[0].Payload.Phase != "CLEANUP_APPLIED" {
		t.Fatalf("facts = %+v, want one round_cleanup/CLEANUP_APPLIED", got)
	}
	if got[0].EventID != "e1" || got[0].RetentionGroupOwnerChannelID != "chan-1" {
		t.Fatalf("cleanup fact lost the round identity: %+v", got[0])
	}
}

// TestObservationsCarryPoolProvenance proves every fact names the pool that
// produced it, which the store requires as NOT NULL.
func TestObservationsCarryPoolProvenance(t *testing.T) {
	p, sink := observedPool(t, &fakePlacer{})
	admitRound(p, newTestStreamer(1000), "e1")
	if _, err := p.PlaceManualBet("e1", "o1", 100); err != nil {
		t.Fatal(err)
	}
	for _, o := range sink.all() {
		if o.PoolInstanceID != "pool-test" {
			t.Fatalf("fact without pool provenance: %+v", o)
		}
	}
	// A production pool mints a distinct identity per instance.
	a, b := newPoolInstanceID(), newPoolInstanceID()
	if a == b || !strings.HasPrefix(a, "pool-") {
		t.Fatalf("pool instance ids = %q / %q", a, b)
	}
}

// TestManualBetUnderConcurrencyEmitsOneRootPerAction hammers the manual path
// with the race detector on and proves the trail stays consistent: one root
// per attempt and at most one Twitch call overall.
func TestManualBetUnderConcurrencyEmitsOneRootPerAction(t *testing.T) {
	placer := &fakePlacer{delay: time.Millisecond}
	p, sink := observedPool(t, placer)
	admitRound(p, newTestStreamer(100000), "e1")

	const attempts = 12
	var wg sync.WaitGroup
	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func() {
			defer wg.Done()
			_, _ = p.PlaceManualBet("e1", "o1", 100)
		}()
	}
	wg.Wait()

	roots := 0
	for _, o := range sink.all() {
		if o.Payload.Phase == "MANUAL_DIRECT_ROOT" {
			roots++
		}
		if o.Payload.Phase == "MANUAL_MINER_ROOT" {
			t.Fatal("a direct call opened a relayed root")
		}
	}
	if roots != attempts {
		t.Fatalf("roots = %d, want exactly one per attempt (%d)", roots, attempts)
	}
	if placer.callCount() != 1 {
		t.Fatalf("Twitch calls = %d, want exactly 1 despite %d concurrent attempts", placer.callCount(), attempts)
	}
}

// TestObservationGoldenSequence pins the exact fact sequence a complete manual
// round produces, against a checked-in fixture. It is the regression guard
// against a producer silently gaining, losing or reordering a fact.
func TestObservationGoldenSequence(t *testing.T) {
	p, sink := observedPool(t, &fakePlacer{})
	admitRound(p, newTestStreamer(1000), "e1")
	if _, err := p.PlaceManualBetRelayed("e1", "o2", 250, p.NextManualActionToken()); err != nil {
		t.Fatal(err)
	}
	p.removePrediction("e1")

	type goldenFact struct {
		Kind       string `json:"kind"`
		Phase      string `json:"phase"`
		Reason     string `json:"reasonCode,omitempty"`
		Decision   string `json:"decision,omitempty"`
		ErrorClass string `json:"errorClass,omitempty"`
		Manual     *bool  `json:"manual,omitempty"`
		EventID    string `json:"eventId"`
		Channel    string `json:"routedChannelId"`
		Retention  string `json:"retentionGroupOwnerChannelId,omitempty"`
		Round      string `json:"roundIncarnationId,omitempty"`
		Action     int64  `json:"manualActionId,omitempty"`
	}
	var got []goldenFact
	for _, o := range sink.all() {
		got = append(got, goldenFact{
			Kind: o.Kind, Phase: o.Payload.Phase, Reason: o.Payload.ReasonCode,
			Decision: o.Payload.Decision, ErrorClass: o.Payload.ErrorClass, Manual: o.Payload.Manual,
			EventID: o.EventID, Channel: o.RoutedChannelID, Retention: o.RetentionGroupOwnerChannelID,
			Round: o.RoundIncarnationID, Action: o.Payload.Counters["manualActionId"],
		})
	}
	actual, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	actual = append(actual, '\n')

	golden := filepath.Join("testdata", "prediction_observations", "manual_round.json")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, actual, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden fixture (re-record with UPDATE_GOLDEN=1): %v", err)
	}
	if string(actual) != string(want) {
		t.Fatalf("observation sequence drifted from %s\n--- got ---\n%s\n--- want ---\n%s", golden, actual, want)
	}
}

// TestRepeatedCleanupWritesNoUnreachableIdentifier is the regression for the
// general form of the manual-bet defect below. removePrediction is idempotent:
// the second call finds nothing in the maps, resolves no channel and no
// incarnation, and used to emit a cleanup fact carrying the Twitch event id
// with NO identity column at all. A channel-scoped privacy erasure deletes by
// routed channel or by retention-group channel; a row with neither is
// unreachable forever, so an erased channel's event id would survive its own
// erasure.
func TestRepeatedCleanupWritesNoUnreachableIdentifier(t *testing.T) {
	p, sink := observedPool(t, &fakePlacer{})
	s := newTestStreamer(1000)
	admitRound(p, s, "event-cleaned-twice")

	p.removePrediction("event-cleaned-twice")
	p.removePrediction("event-cleaned-twice")

	got := sink.all()
	var cleanups []PredictionObservation
	for _, o := range got {
		if o.Kind == ObsKindRoundCleanup {
			cleanups = append(cleanups, o)
		}
	}
	if len(cleanups) != 2 {
		t.Fatalf("want both cleanup attempts recorded, got %d of %d facts", len(cleanups), len(got))
	}
	// The FIRST one still resolves the round: it must keep its identity, or the
	// guard would be buying privacy by destroying the trail.
	if cleanups[0].EventID != "event-cleaned-twice" || cleanups[0].RoutedChannelID != "chan-1" {
		t.Fatalf("the first cleanup lost the identity it could resolve: %+v", cleanups[0])
	}
	// The SECOND resolves nothing, so it must name nothing.
	if cleanups[1].EventID != "" {
		t.Fatalf("a cleanup with no resolvable identity carries the Twitch event id %q — no erasure can reach it: %+v",
			cleanups[1].EventID, cleanups[1])
	}
	assertNoUnreachableIdentifier(t, got)
}

// TestUnreachableFactsCarryNoChannelScopedIdentifier states the invariant
// itself, at the one funnel every fact leaves this package by. event_id and
// round_owner_channel_id are both channel-scoped and NEITHER is a door the
// store's erasure can open — only the routed and retention-group pairs are —
// so a fact holding neither of those pairs must hold neither identifier.
func TestUnreachableFactsCarryNoChannelScopedIdentifier(t *testing.T) {
	p, sink := observedPool(t, &fakePlacer{})
	p.observe(PredictionObservation{
		Kind:                "round_cleanup",
		EventID:             "some-twitch-event",
		RoundOwnerChannelID: "chan-9",
		RoundOwnerLogin:     "victim",
	})
	got := sink.all()
	if len(got) != 1 {
		t.Fatalf("want exactly one fact, got %d", len(got))
	}
	if got[0].EventID != "" || got[0].RoundOwnerChannelID != "" || got[0].RoundOwnerLogin != "" {
		t.Fatalf("an unreachable fact kept a channel-scoped identifier: %+v", got[0])
	}
	assertNoUnreachableIdentifier(t, got)
}

// assertNoUnreachableIdentifier is the shared predicate: no fact that a
// channel-scoped erasure cannot reach may name a channel or one of its rounds.
func assertNoUnreachableIdentifier(t *testing.T, got []PredictionObservation) {
	t.Helper()
	for _, o := range got {
		if o.RoutedChannelID != "" || o.RetentionGroupOwnerChannelID != "" {
			continue // reachable by erasure: an identifier is fine
		}
		if o.EventID != "" || o.RoundOwnerChannelID != "" {
			t.Fatalf("a fact no erasure can reach names a channel or its round: %+v", o)
		}
	}
}

// TestUntrackedManualBetWritesNoUnreachableIdentifier is the regression for a
// defect an independent review found: a manual bet on an untracked round
// emitted the caller-supplied Twitch event id on facts with NO identity
// columns at all — rows no channel-scoped privacy erasure could ever reach.
func TestUntrackedManualBetWritesNoUnreachableIdentifier(t *testing.T) {
	p, sink := observedPool(t, &fakePlacer{})
	// No round is registered, so the pool cannot resolve any identity.
	if _, err := p.PlaceManualBet("event-that-is-not-tracked", "o1", 100); err == nil {
		t.Fatal("expected the untracked round to be refused")
	}
	got := sink.all()
	if len(got) == 0 {
		t.Fatal("an attempted manual bet on an unknown round left no trace at all")
	}
	for _, o := range got {
		if o.RoutedChannelID != "" || o.RetentionGroupOwnerChannelID != "" {
			continue // reachable by erasure: an identifier is fine
		}
		if o.EventID != "" {
			t.Fatalf("a fact with no identity carries the Twitch event id %q — no erasure can reach it: %+v", o.EventID, o)
		}
	}
	// The audit value survives: the attempt and its outcome are still recorded.
	var sawRoot, sawLookup bool
	for _, o := range got {
		switch o.Payload.Phase {
		case "MANUAL_DIRECT_ROOT":
			sawRoot = true
		case "MANUAL_POOL_LOOKUP":
			if o.Payload.ReasonCode == "NO_ROUND" {
				sawLookup = true
			}
		}
	}
	if !sawRoot || !sawLookup {
		t.Fatalf("the attempt was not recorded: root=%v lookup=%v (%v)", sawRoot, sawLookup, sink.phases())
	}
}

// TestPlacementErrorClassSeparatesTransportFromRejection proves a local or
// network fault is not recorded as a Twitch rejection — conflating them would
// make the trail claim Twitch refused a bet it never received.
func TestPlacementErrorClassSeparatesTransportFromRejection(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"context cancelled", context.Canceled, "TRANSPORT"},
		{"deadline exceeded", context.DeadlineExceeded, "TRANSPORT"},
		{"url error", &url.Error{Op: "Post", URL: "https://gql.twitch.tv/gql", Err: errors.New("dial tcp: timeout")}, "TRANSPORT"},
		{"syscall error", os.NewSyscallError("connect", errors.New("connection refused")), "TRANSPORT"},
		{"twitch rejection", errors.New("a provider rejection"), "REJECTED_BY_TWITCH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := placementErrorClass(tc.err); got != tc.want {
				t.Fatalf("placementErrorClass = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestScheduleDecisionIsObserved closes a coverage gap an independent review
// found: schedule_decision had no behavioural test at all. A new round that is
// accepted, and one refused because the toggle is off, must both leave the
// decision the existing code already made.
func TestScheduleDecisionIsObserved(t *testing.T) {
	newFrame := func(eventID string) *PubSubMessage {
		return &PubSubMessage{
			Topic: NewTopic(TopicPredictionsChannel, "chan-1"), Type: "event-created",
			ChannelID: "chan-1",
			Data: map[string]interface{}{"event": map[string]interface{}{
				"id": eventID, "status": "ACTIVE", "created_at": time.Now().Format(time.RFC3339),
				"prediction_window_seconds": float64(600),
				"outcomes": []interface{}{
					map[string]interface{}{"id": "o1", "color": "BLUE", "total_points": float64(10)},
					map[string]interface{}{"id": "o2", "color": "PINK", "total_points": float64(20)},
				},
			}},
		}
	}

	t.Run("accepted", func(t *testing.T) {
		p, sink := observedPool(t, &fakePlacer{})
		s := newTestStreamer(100000)
		p.streamers = []*models.Streamer{s}
		p.handlePredictionChannel(newFrame("sched-1"), s)

		var decisions []string
		for _, o := range sink.all() {
			if o.Kind == ObsKindScheduleDecision {
				decisions = append(decisions, o.Payload.Phase+"/"+o.Payload.Decision+"/"+o.Payload.ReasonCode)
			}
		}
		want := []string{"SCHEDULE_ACCEPTED/PLACE/OK"}
		if !reflect.DeepEqual(decisions, want) {
			t.Fatalf("schedule decisions = %v, want %v (all facts: %v)", decisions, want, sink.phases())
		}
	})

	t.Run("skipped when predictions are off", func(t *testing.T) {
		p, sink := observedPool(t, &fakePlacer{})
		s := newTestStreamer(100000)
		settings := s.GetSettings()
		settings.MakePredictions = false
		s.SetSettings(settings)
		p.streamers = []*models.Streamer{s}
		p.handlePredictionChannel(newFrame("sched-2"), s)

		var decisions []string
		for _, o := range sink.all() {
			if o.Kind == ObsKindScheduleDecision {
				decisions = append(decisions, o.Payload.Phase+"/"+o.Payload.Decision+"/"+o.Payload.ReasonCode)
			}
		}
		want := []string{"SCHEDULE_SKIPPED/SKIP/TOGGLE_OFF"}
		if !reflect.DeepEqual(decisions, want) {
			t.Fatalf("schedule decisions = %v, want %v", decisions, want)
		}
	})
}

// TestAutoDecisionIsObserved closes a coverage gap: auto_decision had no
// behavioural test. The due fact, the decision and the placement's two sides
// must all appear, and the Twitch call must still happen exactly once.
func TestAutoDecisionIsObserved(t *testing.T) {
	placer := &fakePlacer{}
	p, sink := observedPool(t, placer)
	s := newTestStreamer(100000)
	admitRound(p, s, "auto-1")

	p.placeAutoBet("auto-1")

	var phases []string
	for _, o := range sink.all() {
		if o.Kind == ObsKindAutoDecision || o.Kind == ObsKindPlacement {
			phases = append(phases, o.Kind+"/"+o.Payload.Phase)
		}
	}
	want := []string{
		"auto_decision/AUTO_DUE",
		"auto_decision/AUTO_DECIDED",
		"placement/CALL_STARTED",
		"placement/CALL_RETURNED",
	}
	if !reflect.DeepEqual(phases, want) {
		t.Fatalf("auto phases = %v, want %v", phases, want)
	}
	if placer.callCount() != 1 {
		t.Fatalf("Twitch calls = %d, want exactly 1", placer.callCount())
	}
	// Every auto fact is marked non-manual, and the round is identified.
	for _, o := range sink.all() {
		if o.Kind == ObsKindAutoDecision {
			if o.Payload.Manual == nil || *o.Payload.Manual {
				t.Fatalf("auto fact not marked automated: %+v", o.Payload)
			}
			if o.EventID != "auto-1" || o.RetentionGroupOwnerChannelID != "chan-1" {
				t.Fatalf("auto fact lost the round identity: %+v", o)
			}
		}
	}
}

// TestAutoDecisionSkipIsObserved proves a declined auto-bet records the closed
// reason and makes NO Twitch call.
func TestAutoDecisionSkipIsObserved(t *testing.T) {
	placer := &fakePlacer{}
	p, sink := observedPool(t, placer)
	s := newTestStreamer(100000)
	admitRound(p, s, "auto-2")
	// Suppress this round: the existing per-round auto-bet skip.
	if err := p.SetAutoBetSkip("auto-2", true); err != nil {
		t.Fatal(err)
	}

	p.placeAutoBet("auto-2")

	var skipped bool
	for _, o := range sink.all() {
		if o.Kind == ObsKindAutoDecision && o.Payload.Phase == "AUTO_SKIPPED" {
			skipped = true
			if o.Payload.Decision != "SKIP" || o.Payload.ReasonCode != "FILTER_REJECTED" {
				t.Fatalf("skip fact = %+v, want SKIP/FILTER_REJECTED", o.Payload)
			}
		}
		if o.Kind == ObsKindPlacement {
			t.Fatalf("a skipped auto-bet produced a placement fact: %+v", o)
		}
	}
	if !skipped {
		t.Fatalf("no AUTO_SKIPPED fact; got %v", sink.phases())
	}
	if placer.callCount() != 0 {
		t.Fatalf("Twitch calls = %d, want 0 for a skipped auto-bet", placer.callCount())
	}
}

// TestUserFrameObservationsAreRecorded closes the last producer coverage gap:
// user_prediction_made and user_terminal had no behavioural test. The terminal
// verdict recorded must be the one the admission logic already reached — the
// trail reads it, never re-decides it.
func TestUserFrameObservationsAreRecorded(t *testing.T) {
	p, sink := observedPool(t, &fakePlacer{})
	s := newTestStreamer(100000)
	p.streamers = []*models.Streamer{s}
	event := admitRound(p, s, "user-1")
	event.Bet.Decision = models.Decision{Choice: 0, Amount: 250, ID: "o1"}

	made := &PubSubMessage{
		Topic: NewTopic(TopicPredictionsUser, "user-id"), Type: "prediction-made",
		ChannelID: "chan-1",
		Data:      map[string]interface{}{"prediction": map[string]interface{}{"event_id": "user-1"}},
	}
	p.handlePredictionUser(made, s)

	var confirmed bool
	for _, o := range sink.all() {
		if o.Kind == ObsKindUserPredictionMade {
			confirmed = true
			if o.Payload.Phase != "PLACEMENT_CONFIRMED" || o.Payload.ReasonCode != "ACCEPTED" {
				t.Fatalf("confirmation fact = %+v", o.Payload)
			}
			if o.SourceTopicType != string(TopicPredictionsUser) {
				t.Fatalf("confirmation fact topic = %q", o.SourceTopicType)
			}
			if o.Payload.Counters["stake"] != 250 {
				t.Fatalf("confirmation fact lost the stake: %+v", o.Payload.Counters)
			}
		}
	}
	if !confirmed {
		t.Fatalf("no user_prediction_made fact; got %v", sink.phases())
	}

	// A terminal frame for a round that was never confirmed is REFUSED by the
	// existing admission logic; the trail must record that verdict, not
	// invent one.
	unknown := &PubSubMessage{
		Topic: NewTopic(TopicPredictionsUser, "user-id"), Type: "prediction-result",
		ChannelID: "chan-1",
		Data: map[string]interface{}{"prediction": map[string]interface{}{
			"event_id": "not-a-tracked-round",
			"result":   map[string]interface{}{"type": "WIN", "points_won": float64(500)},
		}},
	}
	outcome := p.handlePredictionUser(unknown, s)
	if outcome.PredictionResultAccepted {
		t.Fatal("an untracked terminal was admitted; the observer must not change admission")
	}
	var terminal bool
	for _, o := range sink.all() {
		if o.Kind == ObsKindUserTerminal {
			terminal = true
			if o.Payload.ReasonCode != "NO_ROUND" {
				t.Fatalf("terminal fact = %+v, want the NO_ROUND verdict the admission logic reached", o.Payload)
			}
		}
	}
	if !terminal {
		t.Fatalf("no user_terminal fact; got %v", sink.phases())
	}
}

// TestEveryTerminalExitIsObservedWithItsOwnVerdict closes a coverage gap an
// independent review found, and repairs a misstatement inside it.
//
// The gap: of the five exits from the terminal path only ONE was exercised, so
// deleting the other four emissions — or collapsing every admitted verdict to
// REJECTED, so a won bet recorded as a loss — left the suite green.
//
// The misstatement: three distinct causes refuse admission (round not tracked,
// no confirmed bet on it, terminal already accepted) and only two reason codes
// existed, so a TRACKED but unconfirmed round was recorded as NO_ROUND on a row
// that names that very round's incarnation and event id — the record
// contradicting itself.
func TestEveryTerminalExitIsObservedWithItsOwnVerdict(t *testing.T) {
	frame := func(eventID string, result interface{}) *PubSubMessage {
		prediction := map[string]interface{}{"event_id": eventID}
		if result != nil {
			prediction["result"] = result
		}
		return &PubSubMessage{
			Topic: NewTopic(TopicPredictionsUser, "user-id"), Type: "prediction-result",
			ChannelID: "chan-1",
			Data:      map[string]interface{}{"prediction": prediction},
		}
	}
	win := map[string]interface{}{"type": "WIN", "points_won": float64(500)}

	// tracked prepares a pool holding one admitted round, optionally with a
	// confirmed bet on it.
	tracked := func(t *testing.T, confirm bool) (*WebSocketPool, *recordingSink, *models.Streamer) {
		t.Helper()
		p, sink := observedPool(t, &fakePlacer{})
		s := newTestStreamer(100000)
		p.streamers = []*models.Streamer{s}
		event := admitRound(p, s, "round-1")
		event.Bet.Decision = models.Decision{Choice: 0, Amount: 250, ID: "o1"}
		if confirm {
			p.handlePredictionUser(&PubSubMessage{
				Topic: NewTopic(TopicPredictionsUser, "user-id"), Type: "prediction-made",
				ChannelID: "chan-1",
				Data: map[string]interface{}{"prediction": map[string]interface{}{
					"event_id": "round-1"}},
			}, s)
		}
		sink.reset()
		return p, sink, s
	}

	terminalOf := func(t *testing.T, sink *recordingSink) PredictionObservation {
		t.Helper()
		var found []PredictionObservation
		for _, o := range sink.all() {
			if o.Kind == ObsKindUserTerminal {
				found = append(found, o)
			}
		}
		if len(found) != 1 {
			t.Fatalf("want exactly one user_terminal fact, got %d (%v)", len(found), sink.phases())
		}
		return found[0]
	}

	t.Run("no event id on the wire", func(t *testing.T) {
		p, sink, s := tracked(t, true)
		p.handlePredictionUser(frame("", win), s)
		got := terminalOf(t, sink)
		if got.Payload.Phase != "TERMINAL_REJECTED" || got.Payload.ReasonCode != "NO_ROUND" {
			t.Fatalf("terminal fact = %+v", got.Payload)
		}
	})

	t.Run("no result object", func(t *testing.T) {
		p, sink, s := tracked(t, true)
		p.handlePredictionUser(frame("round-1", nil), s)
		got := terminalOf(t, sink)
		if got.Payload.Phase != "TERMINAL_REJECTED" || got.Payload.ReasonCode != "REJECTED" {
			t.Fatalf("terminal fact = %+v", got.Payload)
		}
		if got.Payload.Presence["result"] != ObsAbsentOnWire {
			t.Fatalf("result presence = %q, want %q", got.Payload.Presence["result"], ObsAbsentOnWire)
		}
	})

	t.Run("a result the validator refuses", func(t *testing.T) {
		p, sink, s := tracked(t, true)
		p.handlePredictionUser(frame("round-1", map[string]interface{}{"type": "TELEPORTED"}), s)
		got := terminalOf(t, sink)
		if got.Payload.Phase != "TERMINAL_REJECTED" || got.Payload.ReasonCode != "REJECTED" {
			t.Fatalf("terminal fact = %+v", got.Payload)
		}
	})

	// The repaired misstatement: a tracked round with no confirmed bet is not
	// "no round", and the fact names the round it is about.
	t.Run("a tracked round with no confirmed bet", func(t *testing.T) {
		p, sink, s := tracked(t, false)
		p.handlePredictionUser(frame("round-1", win), s)
		got := terminalOf(t, sink)
		if got.Payload.Phase != "TERMINAL_DELIVERED" {
			t.Fatalf("terminal fact = %+v", got.Payload)
		}
		if got.Payload.ReasonCode != "NOT_CONFIRMED" {
			t.Fatalf("a tracked round with no confirmed bet was recorded as %q; the same row "+
				"carries its event id, so NO_ROUND would contradict itself: %+v",
				got.Payload.ReasonCode, got)
		}
	})

	t.Run("a second terminal for one round", func(t *testing.T) {
		p, sink, s := tracked(t, true)
		if !p.handlePredictionUser(frame("round-1", win), s).PredictionResultAccepted {
			t.Fatal("the first terminal was not admitted; the fixture is wrong")
		}
		sink.reset()
		if p.handlePredictionUser(frame("round-1", win), s).PredictionResultAccepted {
			t.Fatal("a duplicate terminal was admitted twice")
		}
		got := terminalOf(t, sink)
		if got.Payload.Phase != "TERMINAL_DELIVERED" || got.Payload.ReasonCode != "DUPLICATE" {
			t.Fatalf("terminal fact = %+v", got.Payload)
		}
	})

	// The admitted verdicts, each distinct: a won bet recorded as a loss is
	// the failure this pins.
	for _, tc := range []struct {
		name   string
		result map[string]interface{}
		want   string
		payout int64
	}{
		{"a win", map[string]interface{}{"type": "WIN", "points_won": float64(500)}, "WON", 500},
		{"a loss", map[string]interface{}{"type": "LOSE", "points_won": float64(0)}, "LOST", 0},
		{"a refund", map[string]interface{}{"type": "REFUND", "points_won": float64(0)}, "REFUNDED", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, sink, s := tracked(t, true)
			if !p.handlePredictionUser(frame("round-1", tc.result), s).PredictionResultAccepted {
				t.Fatal("the terminal was refused; the fixture is wrong")
			}
			got := terminalOf(t, sink)
			if got.Payload.Phase != "TERMINAL_ADMITTED" {
				t.Fatalf("terminal fact = %+v", got.Payload)
			}
			if got.Payload.ReasonCode != tc.want {
				t.Fatalf("verdict = %q, want %q: the trail must not record one outcome as another",
					got.Payload.ReasonCode, tc.want)
			}
			if got.Payload.Counters["payout"] != tc.payout {
				t.Fatalf("payout = %d, want %d", got.Payload.Counters["payout"], tc.payout)
			}
			if got.RoundIncarnationID == "" {
				t.Fatal("an admitted terminal names no local round")
			}
		})
	}
}

// TestReAdmittedRoundIsANewLocalRound is the producer half of an independent
// review's F2: a round incarnation must identify one LOCAL ADMISSION, not a
// Twitch event.
//
// The store used to derive it by hashing the channel and the event id, which
// made it identical for every admission of that event, in every pool, for the
// whole life of the database. Two admissions of one event id — the ordinary
// consequence of a round being cleaned up and created again — then shared a
// retention unit and an erasure group, and nothing downstream could tell which
// admission a fact belonged to.
//
// It is now allocated where the admission actually happens: the pool instance
// that admitted the round, plus that pool's admission counter.
func TestReAdmittedRoundIsANewLocalRound(t *testing.T) {
	frame := func(eventID string) *PubSubMessage {
		return &PubSubMessage{
			Topic: NewTopic(TopicPredictionsChannel, "chan-1"), Type: "event-created",
			ChannelID: "chan-1",
			Data: map[string]interface{}{"event": map[string]interface{}{
				"id": eventID, "status": "ACTIVE", "created_at": time.Now().Format(time.RFC3339),
				"prediction_window_seconds": float64(600),
				"outcomes": []interface{}{
					map[string]interface{}{"id": "o1", "color": "BLUE", "total_points": float64(10)},
					map[string]interface{}{"id": "o2", "color": "PINK", "total_points": float64(20)},
				},
			}},
		}
	}

	p, sink := observedPool(t, &fakePlacer{})
	s := newTestStreamer(100000)
	p.streamers = []*models.Streamer{s}

	// The SAME Twitch event id, admitted, cleaned up, and admitted again.
	p.handlePredictionChannel(frame("same-event"), s)
	p.removePrediction("same-event")
	p.handlePredictionChannel(frame("same-event"), s)

	var rounds []string
	for _, o := range sink.all() {
		if o.RoundIncarnationID != "" {
			rounds = append(rounds, o.RoundIncarnationID)
		}
	}
	if len(rounds) < 2 {
		t.Fatalf("facts naming a local round = %v, want at least the two admissions (all: %v)", rounds, sink.phases())
	}
	distinct := map[string]bool{}
	for _, r := range rounds {
		distinct[r] = true
	}
	if len(distinct) != 2 {
		t.Fatalf("two admissions of one event produced %d local rounds (%v); each admission is its own round",
			len(distinct), rounds)
	}
	// Every incarnation names the pool that admitted it, so facts from two
	// pool generations can never be mistaken for one round either.
	for r := range distinct {
		if !strings.HasPrefix(r, "round:pool-test:") {
			t.Fatalf("round incarnation %q does not name its pool instance", r)
		}
	}

	// A second pool never reuses the first pool's identities, even though its
	// admission counter starts over.
	q, qsink := observedPool(t, &fakePlacer{})
	q.instanceID = "pool-other"
	q.streamers = []*models.Streamer{s}
	q.handlePredictionChannel(frame("same-event"), s)
	for _, o := range qsink.all() {
		if o.RoundIncarnationID != "" && distinct[o.RoundIncarnationID] {
			t.Fatalf("a second pool reused round incarnation %q", o.RoundIncarnationID)
		}
	}
}

// TestEveryTrackedRoundIsAdmittedWithAnIncarnation is a STRUCTURAL guard. A
// roundControl published without an incarnation would silently produce facts
// with no round identity: they would still be stored, still be erasable by
// their routed identity, and simply stop belonging to their round. That is a
// quiet failure no behavioural test on today's single admission site would
// catch, so the source is asserted directly.
func TestEveryTrackedRoundIsAdmittedWithAnIncarnation(t *testing.T) {
	src, err := os.ReadFile("pool.go")
	if err != nil {
		t.Fatal(err)
	}
	// The one production construction site, and the only accepted form.
	const admission = "&roundControl{incarnation: p.newRoundIncarnation()}"
	if n := strings.Count(string(src), admission); n != 1 {
		t.Fatalf("pool.go has %d admissions of the form %q, want exactly 1", n, admission)
	}
	for _, line := range strings.Split(string(src), "\n") {
		if !strings.Contains(line, "&roundControl{") {
			continue
		}
		if !strings.Contains(line, admission) {
			t.Fatalf("a round control is published without a local admission incarnation:\n\t%s", strings.TrimSpace(line))
		}
	}
}

// TestProducerTimeIsNotClaimedForAReceiverClockReading is an independent
// review's F3, time half.
//
// ParsePubSubMessage always sets Timestamp: when a frame carries no producer
// time it falls back to server_time, and when it carries neither it falls back
// to this process's own clock. The observer read only the value and, finding it
// non-zero, labelled every frame PRODUCER. A durable trail then asserted that
// Twitch had stated a time for frames whose time this process had invented —
// and the RECEIVER branch was unreachable for any parsed frame.
//
// TestAWiredPoolIsAProducerUntilItIsSettled is the regression for a defect an
// independent review found: nothing tied the POOL's lifetime to the
// collector's. The only link was a nil-check on the pool's Close result inside
// the miner, and a Close that never happens returns no result to check — so a
// pool still alive at shutdown left a session finalizing COMPLETE, claiming to
// have observed everything, with a producer demonstrably still wired to it.
//
// Wiring a sink now opens a producer episode for the pool's whole life, and
// the handle that closes it is the only thing that can end it.
func TestAWiredPoolIsAProducerUntilItIsSettled(t *testing.T) {
	t.Run("a pool that proves it stopped", func(t *testing.T) {
		p := newTestPool(&fakePlacer{})
		sink := &recordingSink{}
		settle := p.SetPredictionObservationSink(sink)

		begun, settled := sink.episodes()
		if begun != 1 || settled != 0 {
			t.Fatalf("wiring a sink opened %d episodes and settled %d, want 1 and 0: a wired "+
				"pool is a live producer until something says otherwise", begun, settled)
		}
		settle(nil)
		if _, settled = sink.episodes(); settled != 1 {
			t.Fatalf("the handle settled %d episodes, want 1", settled)
		}
		if got := sink.uncertainShutdowns(); got != 0 {
			t.Fatalf("a clean shutdown recorded %d uncertainties, want 0", got)
		}
		// One-shot: settling again changes nothing.
		settle(errors.New("a second call"))
		if _, settled = sink.episodes(); settled != 1 {
			t.Fatalf("a second settle re-settled the episode (%d)", settled)
		}
		if got := sink.uncertainShutdowns(); got != 0 {
			t.Fatalf("a second settle recorded %d uncertainties, want 0", got)
		}
	})

	t.Run("a pool that cannot prove it stopped", func(t *testing.T) {
		p := newTestPool(&fakePlacer{})
		sink := &recordingSink{}
		settle := p.SetPredictionObservationSink(sink)

		settle(errors.New("the pool could not join its connections"))
		if got := sink.uncertainShutdowns(); got != 1 {
			t.Fatalf("a shutdown that could not prove itself recorded %d uncertainties, want 1: "+
				"the session must not finalize as if everything had been observed", got)
		}
		if _, settled := sink.episodes(); settled != 1 {
			t.Fatal("the episode was not settled")
		}
	})

	t.Run("no sink wired", func(t *testing.T) {
		p := newTestPool(&fakePlacer{})
		// A miner without analytics wires nothing; the handle must still be
		// safe to hold and to call.
		settle := p.SetPredictionObservationSink(nil)
		settle(errors.New("anything"))
	})
}

// TestATimerRecordsWhichAdmissionItWasScheduledFor is the regression for a
// defect an independent review found: the auto-bet timer captured only the
// Twitch event id, and re-looked-up the round when it fired. A round that was
// cleaned up and admitted again while the timer slept left the timer filing
// AUTO_DUE and everything after it under the NEW incarnation — an auto
// decision on a round nothing ever scheduled, with nothing in the trail saying
// so. A timer whose round vanished emitted nothing at all, so a decision that
// never happened was indistinguishable from one never scheduled.
func TestATimerRecordsWhichAdmissionItWasScheduledFor(t *testing.T) {
	t.Run("the round it was scheduled for", func(t *testing.T) {
		p, sink := observedPool(t, &fakePlacer{})
		s := newTestStreamer(100000)
		p.streamers = []*models.Streamer{s}
		admitRound(p, s, "auto-1")
		scheduled := p.roundIncarnation("auto-1")

		p.placeAutoBetScheduled("auto-1", scheduled)

		due := factWithPhase(t, sink, "AUTO_DUE")
		if due.Payload.ReasonCode != "OK" {
			t.Fatalf("AUTO_DUE on the scheduled round reads %q, want OK", due.Payload.ReasonCode)
		}
		if due.RoundIncarnationID != scheduled {
			t.Fatalf("AUTO_DUE names %q, want the scheduled %q", due.RoundIncarnationID, scheduled)
		}
	})

	t.Run("a different admission of the same event", func(t *testing.T) {
		p, sink := observedPool(t, &fakePlacer{})
		s := newTestStreamer(100000)
		p.streamers = []*models.Streamer{s}
		admitRound(p, s, "auto-1")
		scheduled := p.roundIncarnation("auto-1")

		// The round is cleaned up and the SAME Twitch event admitted again —
		// exactly what happens while a timer sleeps.
		p.removePrediction("auto-1")
		admitRound(p, s, "auto-1")
		current := p.roundIncarnation("auto-1")
		if current == scheduled {
			t.Fatal("the fixture did not produce a second admission")
		}
		sink.reset()

		p.placeAutoBetScheduled("auto-1", scheduled)

		due := factWithPhase(t, sink, "AUTO_DUE")
		if due.Payload.ReasonCode != "CONFLICT" {
			t.Fatalf("an auto decision on an admission nobody scheduled reads %q, want CONFLICT: "+
				"the trail would otherwise show it as an ordinary scheduled decision",
				due.Payload.ReasonCode)
		}
	})

	t.Run("a round that is gone", func(t *testing.T) {
		p, sink := observedPool(t, &fakePlacer{})
		s := newTestStreamer(100000)
		p.streamers = []*models.Streamer{s}
		admitRound(p, s, "auto-1")
		scheduled := p.roundIncarnation("auto-1")
		p.removePrediction("auto-1")
		sink.reset()

		p.placeAutoBetScheduled("auto-1", scheduled)

		skipped := factWithPhase(t, sink, "AUTO_SKIPPED")
		if skipped.Payload.ReasonCode != "NO_ROUND" {
			t.Fatalf("a timer whose round vanished reads %q, want NO_ROUND", skipped.Payload.ReasonCode)
		}
		if skipped.RoundIncarnationID != scheduled {
			t.Fatalf("the fact names %q, want the round it was scheduled for (%q)",
				skipped.RoundIncarnationID, scheduled)
		}
	})
}

// TestReservationFactsAreOrderedByTheLockThatDecidedThem is the regression for
// an ordering inversion an independent review found. The double-submit guard
// decides under the pool lock and used to emit AFTER unlocking, so two callers
// the lock had serialized could reach the collector in the opposite order: the
// loser's CONFLICT recorded at a causal position BEFORE the winner's
// reservation — a replay showing the conflict before the thing it conflicted
// with.
//
// This is asserted structurally. The interleaving is a two-instruction window,
// so a concurrency test would reach it rarely, would not fail when the fix was
// removed, and would be worse than no test at all.
func TestReservationFactsAreOrderedByTheLockThatDecidedThem(t *testing.T) {
	src := readSourceFile(t, "pool.go")
	guard := sliceBetween(t, src,
		"// Fast pre-check + double-submit guard, holding no lock across the network.",
		"rc.manualPending = false")

	// Every release of the lock inside the guard has to come AFTER that
	// branch's reservation fact. The branches are alternatives, so this is
	// stated per release rather than by counting depth.
	lines := strings.Split(guard, "\n")
	releases := 0
	for i, line := range lines {
		if strings.TrimSpace(line) != "p.mu.Unlock()" {
			continue
		}
		releases++
		emitted := false
		for j := i - 1; j >= 0; j-- {
			prev := strings.TrimSpace(lines[j])
			if prev == "" || strings.HasPrefix(prev, "//") {
				continue
			}
			emitted = strings.Contains(prev, `"MANUAL_RESERVATION"`)
			break
		}
		if !emitted {
			t.Fatalf("the lock is released at guard line %d without this branch having emitted "+
				"its reservation fact first; a caller the lock serialized can then be recorded "+
				"out of order:\n%s", i, strings.Join(lines[max(0, i-4):i+1], "\n"))
		}
	}
	if releases != 4 {
		t.Fatalf("the guard releases the lock %d times, want 4 (three refusals and the "+
			"reservation); this test has stopped matching the code it asserts about", releases)
	}
	if n := strings.Count(guard, `"MANUAL_RESERVATION"`); n != 4 {
		t.Fatalf("the guard emits %d reservation facts, want 4", n)
	}

	// The cleanup fact has the same shape against a re-admission.
	remove := sliceBetween(t, src, "func (p *WebSocketPool) removePrediction(", "\n}")
	obs := strings.Index(remove, "observeRoundCleanup")
	unlock := strings.Index(remove, "p.mu.Unlock()")
	if obs < 0 || unlock < 0 {
		t.Fatal("removePrediction no longer emits a cleanup fact or no longer takes the lock")
	}
	if obs > unlock {
		t.Fatal("the cleanup fact is emitted after the lock is released; a re-admission of the " +
			"same event can then be recorded as beginning before the old round ended")
	}
}

// factWithPhase returns the single recorded fact carrying phase, failing if
// there is not exactly one.
func factWithPhase(t *testing.T, sink *recordingSink, phase string) PredictionObservation {
	t.Helper()
	var found []PredictionObservation
	for _, o := range sink.all() {
		if o.Payload.Phase == phase {
			found = append(found, o)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one %s fact, got %d (%v)", phase, len(found), sink.phases())
	}
	return found[0]
}

// readSourceFile reads one of this package's own source files, so a structural
// assertion reads the code rather than a copy of it.
func readSourceFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// sliceBetween returns the source between the first occurrence of from and the
// first occurrence of to after it.
func sliceBetween(t *testing.T, src, from, to string) string {
	t.Helper()
	i := strings.Index(src, from)
	if i < 0 {
		t.Fatalf("anchor %q is gone; this test no longer asserts anything", from)
	}
	rest := src[i:]
	j := strings.Index(rest, to)
	if j < 0 {
		t.Fatalf("anchor %q is gone; this test no longer asserts anything", to)
	}
	return rest[:j]
}

// TestConnectionSequenceNumbersOnlyDispatchedDeliveries pins the ordering
// contract this task's connection-provenance stamp depends on, which nothing
// asserted: the stamp sits AFTER the replay-dedup fence and after the
// generation fence, so a suppressed frame consumes no delivery number.
//
// The axis is meant to be dense — consecutive numbers on the frames a
// connection actually delivered — unlike the collector sequence, which is
// deliberately gap-carrying because a gap there IS a loss. Moving the stamp
// above either fence, or into the parser, would silently make the connection
// axis gap-carrying too, and every reading of it would then be wrong about
// how many deliveries a connection made.
func TestConnectionSequenceNumbersOnlyDispatchedDeliveries(t *testing.T) {
	var got []*PubSubMessage
	ws := NewWebSocketClient(0, nil, 3600, 0, func(m *PubSubMessage) {
		got = append(got, m)
	}, nil)
	ws.mu.Lock()
	ws.connGen = 1
	ws.mu.Unlock()

	frame := func(n int) WSMessage {
		return WSMessage{Type: "MESSAGE", Data: &WSData{
			Topic: "community-points-user-v1.123",
			Message: fmt.Sprintf(
				`{"type":"points-earned","data":{"channel_id":"123","point_gain":{"reason_code":"WATCH","total_points":%d}}}`, n),
		}}
	}

	ws.handleMessage(frame(1))
	// The same frame again, inside the one-second replay window: suppressed.
	ws.handleMessage(frame(1))
	// A frame attributed to a retired generation: fenced out.
	ws.handleMessageForGen(frame(2), 0)
	ws.handleMessage(frame(3))

	if len(got) != 2 {
		t.Fatalf("dispatched %d frames, want 2 (a replay and a stale-generation frame are both "+
			"suppressed)", len(got))
	}
	for i, m := range got {
		if !m.ConnectionKnown {
			t.Fatalf("delivery %d carries no connection provenance", i)
		}
		if m.ConnectionSequence != uint64(i+1) {
			t.Fatalf("delivery %d carries connection sequence %d, want %d: a frame nobody "+
				"received must not consume a delivery number",
				i, m.ConnectionSequence, i+1)
		}
	}
}

// TestOutcomeAbsenceIsNeverEncodedAsAZero is the regression for a defect an
// independent review found: an outcome whose top_predictors was an EMPTY LIST
// and one whose top_predictors key never arrived projected to byte-identical
// facts — same zero count, same omitted JSON key, same digest. The same held
// for a present empty colour against a missing colour key. A trail that
// records "we looked and there were none" and "nobody told us" identically
// cannot answer the question it exists for.
func TestOutcomeAbsenceIsNeverEncodedAsAZero(t *testing.T) {
	outcome := func(extra map[string]interface{}) []interface{} {
		o := map[string]interface{}{"total_points": float64(300), "total_users": float64(3)}
		for k, v := range extra {
			o[k] = v
		}
		return []interface{}{o}
	}
	for _, tc := range []struct {
		name       string
		extra      map[string]interface{}
		wantTP     string
		wantColor  string
		wantExamin int
	}{
		{"top_predictors present and empty",
			map[string]interface{}{"color": "BLUE", "top_predictors": []interface{}{}},
			ObsPresent, ObsPresent, 0},
		{"top_predictors never arrived",
			map[string]interface{}{"color": "BLUE"},
			ObsAbsentOnWire, ObsPresent, 0},
		{"top_predictors explicitly null",
			map[string]interface{}{"color": "BLUE", "top_predictors": nil},
			ObsNullOnWire, ObsPresent, 0},
		{"top_predictors with the wrong shape",
			map[string]interface{}{"color": "BLUE", "top_predictors": float64(7)},
			ObsInvalid, ObsPresent, 0},
		{"a colour present and empty",
			map[string]interface{}{"color": "", "top_predictors": []interface{}{}},
			ObsPresent, ObsPresent, 0},
		{"no colour key at all",
			map[string]interface{}{"top_predictors": []interface{}{}},
			ObsPresent, ObsAbsentOnWire, 0},
		{"two predictors examined",
			map[string]interface{}{"color": "BLUE", "top_predictors": []interface{}{"a", "b"}},
			ObsPresent, ObsPresent, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := projectOutcomes(outcome(tc.extra))
			if len(got) != 1 {
				t.Fatalf("projected %d outcomes, want 1", len(got))
			}
			if got[0].TopPredictors != tc.wantTP {
				t.Fatalf("top_predictors state = %q, want %q", got[0].TopPredictors, tc.wantTP)
			}
			if got[0].ColorState != tc.wantColor {
				t.Fatalf("colour state = %q, want %q", got[0].ColorState, tc.wantColor)
			}
			if got[0].TopPredictorsExamined != tc.wantExamin {
				t.Fatalf("examined = %d, want %d", got[0].TopPredictorsExamined, tc.wantExamin)
			}
		})
	}

	// The load-bearing pair, stated as the inequality it is.
	empty := projectOutcomes(outcome(map[string]interface{}{"top_predictors": []interface{}{}}))
	missing := projectOutcomes(outcome(nil))
	if empty[0].TopPredictorsExamined != missing[0].TopPredictorsExamined {
		t.Fatal("fixture error: both cohorts must count zero, or the states are not what " +
			"separates them")
	}
	if empty[0] == missing[0] {
		t.Fatalf("an empty top_predictors list and a key that never arrived project to the same "+
			"fact (%+v) — no reader can tell them apart", empty[0])
	}
}

// TestMessageTypeStatesAreDistinguishedOnTheWire is the regression for a
// defect an independent review found: four different things Twitch can do with
// the "type" key all reached the store as the same empty value.
//
// A key that never arrived, one sent explicitly null, one sent with the wrong
// shape and a type outside this build's vocabulary are four distinct
// observations, and a trail that stores them identically cannot answer what it
// exists to answer. The raw value of an unrecognized type is still never kept.
func TestMessageTypeStatesAreDistinguishedOnTheWire(t *testing.T) {
	const event = `"event":{"id":"e1","status":"ACTIVE"}`
	for _, tc := range []struct {
		name  string
		inner string
		wire  string
		want  string
	}{
		{"a type this build understands", `{"type":"event-updated","data":{` + event + `}}`,
			ObsPresent, "event-updated"},
		{"no type key at all", `{"data":{` + event + `}}`,
			ObsAbsentOnWire, ObsAbsentOnWire},
		{"type sent as null", `{"type":null,"data":{` + event + `}}`,
			ObsNullOnWire, ObsNullOnWire},
		{"type sent with the wrong shape", `{"type":7,"data":{` + event + `}}`,
			ObsInvalid, ObsInvalid},
		{"type sent as an empty string", `{"type":"","data":{` + event + `}}`,
			ObsPresent, ObsUnknownPresent},
		{"a type outside this build's vocabulary", `{"type":"event-teleported","data":{` + event + `}}`,
			ObsPresent, "event-teleported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := ParsePubSubMessage(&WSData{
				Topic:   "predictions-channel-v1.chan-1",
				Message: tc.inner,
			})
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if msg.TypePresence != tc.wire {
				t.Fatalf("wire state = %q, want %q", msg.TypePresence, tc.wire)
			}
			if got := observationFromMessage(msg, nil).SourceMessageType; got != tc.want {
				t.Fatalf("observed message type = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestUnparsedMessageClaimsNoWireState: a message a caller built rather than
// parsed off a wire observed no "type" key, so it must state no wire fact
// about one. Storing ABSENT_ON_WIRE here would assert something about a wire
// this fact never came from.
func TestUnparsedMessageClaimsNoWireState(t *testing.T) {
	o := observationFromMessage(&PubSubMessage{ChannelID: "chan-1"}, nil)
	if o.SourceMessageType != "" {
		t.Fatalf("observed message type = %q, want none: no wire was read", o.SourceMessageType)
	}
}

// Provenance now travels with the value, and a receiver-clock reading is
// recorded as the absence of a producer time rather than as one.
func TestProducerTimeIsNotClaimedForAReceiverClockReading(t *testing.T) {
	frame := func(inner string) *PubSubMessage {
		t.Helper()
		msg, err := ParsePubSubMessage(&WSData{
			Topic:   "predictions-channel-v1.chan-1",
			Message: inner,
		})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		return msg
	}

	const event = `"event":{"id":"e1","status":"ACTIVE"}`

	t.Run("producer time", func(t *testing.T) {
		msg := frame(`{"type":"event-updated","data":{"timestamp":"2026-01-02T03:04:05Z",` + event + `}}`)
		if msg.TimestampSource != TimestampFromProducer {
			t.Fatalf("source = %q, want PRODUCER", msg.TimestampSource)
		}
		o := observationFromMessage(msg, nil)
		if o.ProducerTimeSource != ObsTimeProducer {
			t.Fatalf("observation time source = %q, want PRODUCER", o.ProducerTimeSource)
		}
		if o.ProducerAtMS != time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).UnixMilli() {
			t.Fatalf("producer time = %d, want the frame's own timestamp", o.ProducerAtMS)
		}
	})

	t.Run("server time", func(t *testing.T) {
		msg := frame(`{"type":"event-updated","server_time":1700000000,"data":{` + event + `}}`)
		if msg.TimestampSource != TimestampFromServer {
			t.Fatalf("source = %q, want SERVER", msg.TimestampSource)
		}
		o := observationFromMessage(msg, nil)
		if o.ProducerTimeSource != ObsTimeServer {
			t.Fatalf("observation time source = %q, want SERVER: a server envelope time is not "+
				"a time the producer stated", o.ProducerTimeSource)
		}
		if o.ProducerAtMS != time.Unix(1700000000, 0).UnixMilli() {
			t.Fatalf("server time = %d, want the envelope's server_time", o.ProducerAtMS)
		}
	})

	t.Run("no time on the wire at all", func(t *testing.T) {
		before := time.Now()
		msg := frame(`{"type":"event-updated","data":{` + event + `}}`)
		if msg.TimestampSource != TimestampFromReceiver {
			t.Fatalf("source = %q, want RECEIVER", msg.TimestampSource)
		}
		// The transport value is unchanged: it is still this process's clock.
		if msg.Timestamp.Before(before) {
			t.Fatalf("fallback timestamp %v predates the parse", msg.Timestamp)
		}
		o := observationFromMessage(msg, nil)
		if o.ProducerTimeSource != ObsTimeReceiver {
			t.Fatalf("observation time source = %q, want RECEIVER", o.ProducerTimeSource)
		}
		if o.ProducerAtMS != 0 {
			t.Fatalf("producer time = %d, want none: this process's own clock reading is not a "+
				"time the producer stated", o.ProducerAtMS)
		}
	})
}

// TestFireAndForgetTimersRegisterAProducerEpisode proves the two producers the
// pool's own Close cannot join are visible to the collector's shutdown fence.
//
// wsPool.Close joins the pool's CONNECTIONS. A scheduled auto-bet and a
// scheduled cleanup are producers of their own, sleeping on timers that Close
// neither cancels nor waits for, so a nil result from it proved nothing about
// them. One could fire after the collector had finalized a session as
// COMPLETE, and that session's claim to have observed everything would simply
// be false.
func TestFireAndForgetTimersRegisterAProducerEpisode(t *testing.T) {
	t.Run("the cleanup timer", func(t *testing.T) {
		p, sink := observedPool(t, &fakePlacer{})
		admitRound(p, newTestStreamer(1000), "e1")

		// Measured as a DELTA: wiring the sink opens the pool's own lifetime
		// episode, and this subtest is about the timer's.
		baseBegun, baseSettled := sink.episodes()
		p.scheduleCleanup("e1", time.Millisecond)
		// Registered BEFORE the goroutine starts, so the fence can never miss
		// a timer that has been scheduled but not yet begun running.
		if begun, _ := sink.episodes(); begun-baseBegun != 1 {
			t.Fatalf("scheduling a cleanup registered %d episodes, want 1", begun-baseBegun)
		}

		var settled int64
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, settled = sink.episodes(); settled-baseSettled == 1 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if settled-baseSettled != 1 {
			t.Fatal("the cleanup timer never settled its episode; the collector would report an " +
				"obligation it could not settle for the life of the session")
		}
	})

	t.Run("the auto-bet timer", func(t *testing.T) {
		p, sink := observedPool(t, &fakePlacer{})
		s := newTestStreamer(100000)
		p.streamers = []*models.Streamer{s}
		baseBegun, baseSettled := sink.episodes()
		// A window short enough that the timer fires DURING this subtest. A
		// long one would leave the producer sleeping after the test returned,
		// free to place a bet against a fake placer belonging to some later
		// test -- the episode would be registered and never settled, which is
		// both the wrong assertion and contaminated state for everything after.
		p.handlePredictionChannel(&PubSubMessage{
			Topic: NewTopic(TopicPredictionsChannel, "chan-1"), Type: "event-created",
			ChannelID: "chan-1",
			Data: map[string]interface{}{"event": map[string]interface{}{
				"id": "auto-episode", "status": "ACTIVE", "created_at": time.Now().Format(time.RFC3339),
				// The default bet delay is 6s from the end, so a 7s window
				// makes the placement due at once (the remaining fraction of a
				// second truncates to a zero sleep). Long enough to be
				// scheduled at all, short enough that the timer settles inside
				// this subtest instead of sleeping past it.
				"prediction_window_seconds": float64(7),
				"outcomes": []interface{}{
					map[string]interface{}{"id": "o1", "color": "BLUE", "total_points": float64(10)},
					map[string]interface{}{"id": "o2", "color": "PINK", "total_points": float64(20)},
				},
			}},
		}, s)

		if begun, _ := sink.episodes(); begun-baseBegun != 1 {
			t.Fatalf("an accepted schedule registered %d episodes, want 1 for its auto-bet timer",
				begun-baseBegun)
		}

		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if _, settled := sink.episodes(); settled-baseSettled == 1 {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("the auto-bet timer never settled its episode; it is still sleeping, and it will " +
			"place against whichever pool it wakes into")
	})
}

// TestPresenceDistinguishesWhatTheWireActuallyDid is an independent review's
// F3, presence half.
//
// The projection had two states, PRESENT and ABSENT, and mapped every optional
// field onto them with a bool. A key Twitch never sent, a key it sent as null,
// and a key it sent with the wrong type all became "ABSENT" -- three different
// things the broadcaster's service did, recorded identically. A trail whose
// whole value is being able to say what arrived cannot answer that question
// with a bool.
func TestPresenceDistinguishesWhatTheWireActuallyDid(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event map[string]interface{}
		want  string
	}{
		{"key never sent", map[string]interface{}{"id": "e1", "status": "ACTIVE"}, ObsAbsentOnWire},
		{"key sent as null", map[string]interface{}{"id": "e1", "status": "ACTIVE", "outcomes": nil}, ObsNullOnWire},
		{"key sent with the wrong shape", map[string]interface{}{"id": "e1", "status": "ACTIVE", "outcomes": "two"}, ObsInvalid},
		{"key sent as an empty list", map[string]interface{}{"id": "e1", "status": "ACTIVE", "outcomes": []interface{}{}}, ObsPresent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, sink := observedPool(t, &fakePlacer{})
			s := newTestStreamer(1000)
			p.streamers = []*models.Streamer{s}
			p.handlePredictionChannel(&PubSubMessage{
				Topic: NewTopic(TopicPredictionsChannel, "chan-1"), Type: "event-updated",
				ChannelID: "chan-1", Data: map[string]interface{}{"event": tc.event},
			}, s)

			got := sink.all()
			if len(got) != 1 {
				t.Fatalf("emitted %d facts, want 1", len(got))
			}
			if state := got[0].Payload.Presence["outcomes"]; state != tc.want {
				t.Fatalf("outcomes presence = %q, want %q", state, tc.want)
			}
		})
	}

	// An empty list is PRESENT, not absent: a round whose outcomes list really
	// was empty is a different fact from one that never carried the key.
	t.Run("an empty list is a present value", func(t *testing.T) {
		empty := wirePresence(map[string]interface{}{"outcomes": []interface{}{}}, "outcomes", isList)
		missing := wirePresence(map[string]interface{}{}, "outcomes", isList)
		if empty == missing {
			t.Fatalf("an empty list and a missing key are both %q", empty)
		}
	})

	// A path that does not read a field records that, rather than claiming an
	// absence it never checked for.
	t.Run("not observed is not the same as absent", func(t *testing.T) {
		if got := wirePresence(nil, "outcomes", isList); got != ObsNotObserved {
			t.Fatalf("unobserved field = %q, want %q", got, ObsNotObserved)
		}
	})
}

// TestAnUnreadableFrameNamesTheFieldItsOwnPathInspected is a review finding:
// the predictions-USER path reads msg.Data["prediction"], but reported "event"
// as the unreadable key -- a field it never looks at -- and hardcoded
// ABSENT_ON_WIRE regardless of what was actually there.
//
// Both halves matter for the same reason. A reader of the trail cannot tell
// which field was unreadable if the fact names the wrong one, and cannot tell a
// missing key from one sent with the wrong shape if every case reports the
// same state. The widened vocabulary was pointless at exactly the call sites
// that describe a frame nobody could read.
func TestAnUnreadableFrameNamesTheFieldItsOwnPathInspected(t *testing.T) {
	for _, tc := range []struct {
		name  string
		topic TopicType
		data  map[string]interface{}
		key   string
		want  string
	}{
		{"user path, key missing", TopicPredictionsUser,
			map[string]interface{}{"other": 1}, "prediction", ObsAbsentOnWire},
		{"user path, key null", TopicPredictionsUser,
			map[string]interface{}{"prediction": nil}, "prediction", ObsNullOnWire},
		{"user path, wrong shape", TopicPredictionsUser,
			map[string]interface{}{"prediction": "not an object"}, "prediction", ObsInvalid},
		{"user path, no envelope", TopicPredictionsUser, nil, "prediction", ObsNotObserved},
		{"channel path, key missing", TopicPredictionsChannel,
			map[string]interface{}{"other": 1}, "event", ObsAbsentOnWire},
		{"channel path, wrong shape", TopicPredictionsChannel,
			map[string]interface{}{"event": []interface{}{}}, "event", ObsInvalid},
		{"channel path, no envelope", TopicPredictionsChannel, nil, "event", ObsNotObserved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, sink := observedPool(t, &fakePlacer{})
			s := newTestStreamer(1000)
			msg := &PubSubMessage{
				Topic: NewTopic(tc.topic, "chan-1"), Type: "event-updated",
				ChannelID: "chan-1", Data: tc.data,
			}
			if tc.topic == TopicPredictionsUser {
				p.handlePredictionUser(msg, s)
			} else {
				p.handlePredictionChannel(msg, s)
			}

			got := sink.all()
			if len(got) != 1 || got[0].Kind != ObsKindSourceUnknown {
				t.Fatalf("facts = %+v, want one source_unknown", got)
			}
			state, named := got[0].Payload.Presence[tc.key]
			if !named {
				t.Fatalf("the fact names %v, not the %q this path actually inspects",
					got[0].Payload.Presence, tc.key)
			}
			if state != tc.want {
				t.Fatalf("%s presence = %q, want %q", tc.key, state, tc.want)
			}
		})
	}
}

// TestPresenceStatesSurviveTheStore proves the store keeps the distinctions
// rather than folding them back into UNKNOWN, which would make the producer's
// extra precision unobservable.
func TestPresenceStatesSurviveTheStore(t *testing.T) {
	for _, state := range []string{
		ObsPresent, ObsAbsentOnWire, ObsNullOnWire, ObsInvalid,
		ObsUnknownPresent, ObsNotObserved, ObsUnavailable,
	} {
		p, sink := observedPool(t, &fakePlacer{})
		admitRound(p, newTestStreamer(1000), "e1")
		p.observeUserFrame(&PubSubMessage{
			Topic: NewTopic(TopicPredictionsUser, "chan-1"), Type: "prediction-result",
			ChannelID: "chan-1",
		}, newTestStreamer(1000), "e1", ObsKindUserTerminal, "TERMINAL_DELIVERED", "REJECTED",
			nil, map[string]string{"result": state})

		got := sink.all()
		if len(got) != 1 {
			t.Fatalf("%s: emitted %d facts", state, len(got))
		}
		if got[0].Payload.Presence["result"] != state {
			t.Fatalf("%s was rewritten to %q by the producer", state, got[0].Payload.Presence["result"])
		}
	}
}

// TestAManualActionCarriesOneSealedToken proves the correlation token the
// contract requires: one per operator action, minted before delegation, and
// carried by every fact that action produces.
//
// The case it exists for is the one with no round. A manual bet on an event
// this pool never admitted stores no event id -- that identifier is
// caller-supplied and reached no local state, so recording it would put an
// unreachable Twitch identifier on a fact no channel-scoped erasure could
// find. Without the token, such an action's root and its NO_ROUND descendant
// had nothing in common at all.
func TestAManualActionCarriesOneSealedToken(t *testing.T) {
	t.Run("an untracked round still correlates", func(t *testing.T) {
		p, sink := observedPool(t, &fakePlacer{})
		token := p.NextManualActionToken()
		if _, err := p.PlaceManualBetRelayed("never-admitted", "o1", 100, token); err == nil {
			t.Fatal("a bet on an untracked round succeeded")
		}

		facts := sink.all()
		if len(facts) < 2 {
			t.Fatalf("emitted %d facts, want the root and its lookup", len(facts))
		}
		for _, o := range facts {
			if o.Kind != ObsKindManualControl {
				continue
			}
			if got := o.Payload.Counters["manualActionId"]; got != int64(token) {
				t.Fatalf("%s carries token %d, want the action's %d", o.Payload.Phase, got, token)
			}
			if o.Payload.Presence["correlationToken"] != ObsPresent {
				t.Fatalf("%s does not report its correlation token", o.Payload.Phase)
			}
			// The unreachable identifier is still not stored.
			if o.EventID != "" {
				t.Fatalf("%s stored the caller-supplied event id %q", o.Payload.Phase, o.EventID)
			}
		}
	})

	t.Run("every fact of one action shares the token, and two actions differ", func(t *testing.T) {
		p, sink := observedPool(t, &fakePlacer{})
		admitRound(p, newTestStreamer(100000), "e1")

		first := p.NextManualActionToken()
		if _, err := p.PlaceManualBetRelayed("e1", "o1", 100, first); err != nil {
			t.Fatal(err)
		}
		tokens := map[int64]bool{}
		for _, o := range sink.all() {
			if id, ok := o.Payload.Counters["manualActionId"]; ok {
				tokens[id] = true
			}
		}
		if len(tokens) != 1 || !tokens[int64(first)] {
			t.Fatalf("one action produced tokens %v, want only %d", tokens, first)
		}

		second := p.NextManualActionToken()
		if second == first {
			t.Fatal("two operator actions were minted the same token")
		}
	})

	t.Run("a direct call mints its own", func(t *testing.T) {
		p, sink := observedPool(t, &fakePlacer{})
		admitRound(p, newTestStreamer(100000), "e1")
		if _, err := p.PlaceManualBet("e1", "o1", 100); err != nil {
			t.Fatal(err)
		}
		for _, o := range sink.all() {
			if o.Kind != ObsKindManualControl {
				continue
			}
			if o.Payload.Counters["manualActionId"] == 0 {
				t.Fatalf("%s has no correlation token; a direct call opens its own root and must "+
					"seal it the same way", o.Payload.Phase)
			}
		}
	})

	// An AUTOMATED placement is not an operator action and carries no token.
	t.Run("auto placements carry no token", func(t *testing.T) {
		p, sink := observedPool(t, &fakePlacer{})
		admitRound(p, newTestStreamer(100000), "auto-1")
		p.placeAutoBet("auto-1")
		for _, o := range sink.all() {
			if _, ok := o.Payload.Counters["manualActionId"]; ok {
				t.Fatalf("an automated %s carries a manual correlation token", o.Payload.Phase)
			}
		}
	})
}
