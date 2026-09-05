package pubsub

import (
	"context"
	"encoding/json"
	"errors"
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
	mu   sync.Mutex
	got  []PredictionObservation
	hook func(PredictionObservation)
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

func (r *recordingSink) all() []PredictionObservation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]PredictionObservation(nil), r.got...)
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

// TestObservationSinkAbsentIsANoOp is the most important producer property:
// with no sink wired, observing costs nothing and changes nothing. Every
// pre-existing test in this package runs in exactly this configuration.
func TestObservationSinkAbsentIsANoOp(t *testing.T) {
	placer := &fakePlacer{}
	p := newTestPool(placer)
	s := newTestStreamer(1000)
	addRound(p, s, "e1")

	if p.observing() {
		t.Fatal("a pool with no sink reports itself as observing")
	}
	// Every producer entry point must tolerate the absent sink.
	p.observeRoundFact("e1", "chan-1", "streamer", ObsKindAutoDecision, ObservationPayload{Phase: "AUTO_DUE"})
	p.observeAutoSkip("e1", "chan-1", "streamer", "OK", nil)
	p.observeManualPhase("e1", "chan-1", "streamer", "MANUAL_DIRECT_ROOT", "OK", nil)
	p.observeRoundCleanup("e1", "chan-1", "streamer", "CLEANUP_APPLIED", "OK")
	p.observeUnclassifiedFrame(&PubSubMessage{Topic: NewTopic(TopicPredictionsChannel, "chan-1")}, s, "event")

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
		addRound(p, s, "e1")
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
		addRound(p, s, "e1")
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
		{"relayed", func(p *WebSocketPool) (string, error) { return p.PlaceManualBetRelayed("e1", "o1", 100) }, "MANUAL_MINER_ROOT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, sink := observedPool(t, &fakePlacer{})
			addRound(p, newTestStreamer(1000), "e1")
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
	addRound(p, newTestStreamer(1000), "e1")
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
	addRound(p, newTestStreamer(1000), "e1")

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
	addRound(p, newTestStreamer(1000), "e1")
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
	if o.RoutedChannelID != "chan-1" || o.RoutedLogin != "streamer" || o.RetentionGroupOwnerChannelID != "chan-1" {
		t.Fatalf("identity = %+v", o)
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

// TestOutcomeProjectionIsBounded proves the producer never builds a fact the
// store would have to truncate.
func TestOutcomeProjectionIsBounded(t *testing.T) {
	raw := make([]interface{}, maxObservedOutcomes+40)
	predictors := make([]interface{}, maxTopPredictorsExamined+100)
	for i := range predictors {
		predictors[i] = map[string]interface{}{"user_display_name": "someone"}
	}
	for i := range raw {
		raw[i] = map[string]interface{}{"color": "BLUE", "total_points": float64(i), "top_predictors": predictors}
	}
	out := projectOutcomes(raw)
	if len(out) != maxObservedOutcomes {
		t.Fatalf("projected %d outcomes, want the %d cap", len(out), maxObservedOutcomes)
	}
	for i, o := range out {
		if o.Slot != i {
			t.Fatalf("outcome %d has slot %d", i, o.Slot)
		}
		if o.TopPredictorsExamined != maxTopPredictorsExamined {
			t.Fatalf("examined = %d, want the %d cap", o.TopPredictorsExamined, maxTopPredictorsExamined)
		}
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
	if got[0].Payload.Presence["event"] != ObsAbsent {
		t.Fatalf("presence = %v, want event ABSENT", got[0].Payload.Presence)
	}
	if got[0].RetentionGroupOwnerChannelID != "" {
		t.Fatal("an unreadable frame claimed a retention group; it names no round")
	}
}

// TestRoundCleanupIsObserved proves a dropped round leaves a fact behind that
// still names the round it was about.
func TestRoundCleanupIsObserved(t *testing.T) {
	p, sink := observedPool(t, &fakePlacer{})
	addRound(p, newTestStreamer(1000), "e1")
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
	addRound(p, newTestStreamer(1000), "e1")
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
	addRound(p, newTestStreamer(100000), "e1")

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
	addRound(p, newTestStreamer(1000), "e1")
	if _, err := p.PlaceManualBetRelayed("e1", "o2", 250); err != nil {
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
	}
	var got []goldenFact
	for _, o := range sink.all() {
		got = append(got, goldenFact{
			Kind: o.Kind, Phase: o.Payload.Phase, Reason: o.Payload.ReasonCode,
			Decision: o.Payload.Decision, ErrorClass: o.Payload.ErrorClass, Manual: o.Payload.Manual,
			EventID: o.EventID, Channel: o.RoutedChannelID, Retention: o.RetentionGroupOwnerChannelID,
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
	addRound(p, s, "auto-1")

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
	addRound(p, s, "auto-2")
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
	event := addRound(p, s, "user-1")
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
