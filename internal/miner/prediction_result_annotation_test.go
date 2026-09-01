package miner

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/pubsub"
)

var predictionAnnotationTestSequence atomic.Uint64

// predictionResultPubSubMsg builds the predictions-user prediction-result
// message exactly as the pool's parse layer would hand it to the miner, with
// a payload VALID under the pool's terminal validator for each type: WIN
// carries a numeric points_won, LOSE/REFUND carry none (canonical zero).
func predictionResultPubSubMsg(eventID, resultType string) *pubsub.PubSubMessage {
	result := map[string]interface{}{"type": resultType}
	if resultType == "WIN" {
		result["points_won"] = float64(1000)
	}
	return &pubsub.PubSubMessage{
		Topic: pubsub.NewTopic(pubsub.TopicPredictionsUser, "user"),
		Type:  "prediction-result",
		Data: map[string]interface{}{
			"prediction": map[string]interface{}{
				"event_id": eventID,
				"result":   result,
			},
		},
	}
}

// R10 (miner half) — a prediction-result the pool did NOT admit (duplicate,
// untracked, post-cleanup, unconfirmed) must not write a WIN/LOSE analytics
// annotation: the pool's admission verdict, not the raw payload, gates it.
func TestPredictionResultAnnotationRequiresPoolAdmission(t *testing.T) {
	login := fmt.Sprintf("a3-annot-reject-%d", predictionAnnotationTestSequence.Add(1))
	m, _, _ := newCapabilityMiner(t, login)
	s := m.streamers.Get(login)
	if s == nil {
		t.Fatal("streamer not found")
	}

	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := analytics.NewService(db, t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	m.analyticsSvc = svc

	// Zero-valued outcome = the pool did not admit this message as the first
	// authoritative terminal result.
	m.handlePubSubMessage(predictionResultPubSubMsg("a3-evt-unadmitted", "WIN"), s, pubsub.MessageOutcome{})

	annotations, err := svc.Repository().GetAnnotationRecords(login, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(annotations) != 0 {
		t.Fatalf("unadmitted prediction-result wrote %d annotations (%+v), want 0", len(annotations), annotations)
	}
}

// The accepted verdict still annotates — admission gating must not
// over-suppress the one legitimate terminal annotation per round.
func TestPredictionResultAnnotationWrittenOnceForAcceptedResult(t *testing.T) {
	login := fmt.Sprintf("a3-annot-accept-%d", predictionAnnotationTestSequence.Add(1))
	m, _, _ := newCapabilityMiner(t, login)
	s := m.streamers.Get(login)
	if s == nil {
		t.Fatal("streamer not found")
	}

	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := analytics.NewService(db, t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	m.analyticsSvc = svc

	msg := predictionResultPubSubMsg("a3-evt-accepted", "WIN")
	m.handlePubSubMessage(msg, s, pubsub.MessageOutcome{PredictionResultAccepted: true})
	// A replayed delivery of the same payload arrives with a rejected verdict.
	m.handlePubSubMessage(msg, s, pubsub.MessageOutcome{})

	annotations, err := svc.Repository().GetAnnotationRecords(login, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(annotations) != 1 || annotations[0].Type != "WIN" {
		t.Fatalf("annotations = %+v, want exactly one WIN", annotations)
	}
}

// An accepted tracked LOSE writes exactly one LOSE annotation; the replayed
// delivery arrives with a rejected verdict and adds nothing.
func TestPredictionResultAnnotationAcceptedLoseWrittenOnce(t *testing.T) {
	login := fmt.Sprintf("a3-annot-lose-%d", predictionAnnotationTestSequence.Add(1))
	m, _, _ := newCapabilityMiner(t, login)
	s := m.streamers.Get(login)
	if s == nil {
		t.Fatal("streamer not found")
	}

	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := analytics.NewService(db, t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	m.analyticsSvc = svc

	msg := predictionResultPubSubMsg("a3-evt-lose", "LOSE")
	m.handlePubSubMessage(msg, s, pubsub.MessageOutcome{PredictionResultAccepted: true})
	m.handlePubSubMessage(msg, s, pubsub.MessageOutcome{})

	annotations, err := svc.Repository().GetAnnotationRecords(login, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(annotations) != 1 || annotations[0].Type != "LOSE" {
		t.Fatalf("annotations = %+v, want exactly one LOSE", annotations)
	}
}

// OWNER-CONTRACT PIN (CR1): a REFUND is a valid terminal result for pool
// admission, history/accounting, ROI emission and cleanup, but terminal
// analytics annotations are WIN/LOSE only. Today this already holds
// end-to-end — analytics.Service.RecordAnnotation drops any type without a
// registered color, REFUND included — so this test pins the contract
// against a regression on either layer (e.g. a REFUND color being added,
// or the miner gate widening).
func TestPredictionResultAnnotationAcceptedRefundWritesNone(t *testing.T) {
	login := fmt.Sprintf("a3-annot-refund-%d", predictionAnnotationTestSequence.Add(1))
	m, _, _ := newCapabilityMiner(t, login)
	s := m.streamers.Get(login)
	if s == nil {
		t.Fatal("streamer not found")
	}

	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := analytics.NewService(db, t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	m.analyticsSvc = svc

	// Validator-valid REFUND (no points_won), admitted by the pool.
	m.handlePubSubMessage(predictionResultPubSubMsg("a3-evt-refund", "REFUND"), s, pubsub.MessageOutcome{PredictionResultAccepted: true})

	annotations, err := svc.Repository().GetAnnotationRecords(login, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(annotations) != 0 {
		t.Fatalf("accepted REFUND wrote %d annotations (%+v), want 0", len(annotations), annotations)
	}
}

// OWNER-CONTRACT PIN (TRACKED-ONLY): terminal Prediction business telemetry
// belongs only to a locally tracked confirmed round. A WELL-FORMED WIN or
// LOSE for a round this miner never tracked (or already cleaned up), and a
// malformed WIN, all arrive with a rejected pool verdict and must write zero
// terminal annotations. The account-scoped topic alone is transport
// evidence, not business authority.
func TestPredictionResultAnnotationTrackedOnlyContract(t *testing.T) {
	login := fmt.Sprintf("a3-annot-trackedonly-%d", predictionAnnotationTestSequence.Add(1))
	m, _, _ := newCapabilityMiner(t, login)
	s := m.streamers.Get(login)
	if s == nil {
		t.Fatal("streamer not found")
	}

	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := analytics.NewService(db, t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	m.analyticsSvc = svc

	// Well-formed untracked WIN and LOSE.
	m.handlePubSubMessage(predictionResultPubSubMsg("a3-evt-untracked-win", "WIN"), s, pubsub.MessageOutcome{})
	m.handlePubSubMessage(predictionResultPubSubMsg("a3-evt-untracked-lose", "LOSE"), s, pubsub.MessageOutcome{})
	// Malformed WIN (no points_won) — rejected by the pool's validation
	// boundary, so its verdict is likewise false.
	m.handlePubSubMessage(&pubsub.PubSubMessage{
		Topic: pubsub.NewTopic(pubsub.TopicPredictionsUser, "user"),
		Type:  "prediction-result",
		Data: map[string]interface{}{
			"prediction": map[string]interface{}{
				"event_id": "a3-evt-malformed-win",
				"result":   map[string]interface{}{"type": "WIN"},
			},
		},
	}, s, pubsub.MessageOutcome{})

	annotations, err := svc.Repository().GetAnnotationRecords(login, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(annotations) != 0 {
		t.Fatalf("untracked/malformed terminal results wrote %d annotations (%+v), want 0", len(annotations), annotations)
	}
}
