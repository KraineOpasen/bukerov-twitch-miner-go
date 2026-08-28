package pubsub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/auth"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/twitch"
)

var bonusArbitrationTestSeq atomic.Uint64

type bonusArbitrationRoundTripper struct {
	mutations    atomic.Int32
	firstEntered chan struct{}
	releaseFirst chan struct{}
	releaseOnce  sync.Once
}

func (rt *bonusArbitrationRoundTripper) release() {
	rt.releaseOnce.Do(func() { close(rt.releaseFirst) })
}

func (rt *bonusArbitrationRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	var operation struct {
		Name string `json:"operationName"`
	}
	if err := json.Unmarshal(body, &operation); err != nil {
		return nil, err
	}

	response := `{"data":{}}`
	switch operation.Name {
	case "ChannelPointsContext":
		response = `{"data":{"community":{"channel":{"self":{"communityPoints":{"availableClaim":{"id":"claim-shared"}}}}}}}`
	case "ClaimCommunityPoints":
		if rt.mutations.Add(1) == 1 {
			close(rt.firstEntered)
			<-rt.releaseFirst
		}
		response = `{"data":{"claimCommunityPoints":{"claim":{"id":"accepted"}}}}`
	}

	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(response)),
		Request:    req,
	}, nil
}

func waitBonusBarrier(t *testing.T, ch <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

// TestBonusClaimPubSubAndPollShareOneMutation is the production-seam RED for
// bonus arbitration. The PubSub handler owns the first in-flight mutation while
// the real GQL fallback observes the same authoritative claim ID. Both paths
// must converge on one Streamer-owned mutation owner and one local success
// event; the fallback is a benign loser rather than a second Twitch mutation.
func TestBonusClaimPubSubAndPollShareOneMutation(t *testing.T) {
	previousLogger := slog.Default()
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	rt := &bonusArbitrationRoundTripper{
		firstEntered: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	previousTransport := http.DefaultTransport
	http.DefaultTransport = rt
	t.Cleanup(func() {
		rt.release()
		http.DefaultTransport = previousTransport
	})

	twitchAuth := auth.NewTwitchAuth("tester", "device-id")
	twitchAuth.ReplaceCredentials(auth.TokenResponse{AccessToken: "dummy-token"})
	twitchAuth.SetUserID("100")
	client := twitch.NewTwitchClient(twitchAuth, "device-id")

	streamer := newTestStreamer(1000)
	streamer.Username = fmt.Sprintf("bonus-arbitration-%d", bonusArbitrationTestSeq.Add(1))
	pool := &WebSocketPool{actor: client}

	pubSubDone := make(chan struct{})
	go func() {
		defer close(pubSubDone)
		pool.handleCommunityPointsUser(&PubSubMessage{
			Type: "claim-available",
			Data: map[string]interface{}{
				"claim": map[string]interface{}{"id": "claim-shared"},
			},
		}, streamer)
	}()

	waitBonusBarrier(t, rt.firstEntered, "first PubSub mutation")
	claimedByPoll, pollErr := client.ClaimAvailableBonus(streamer)
	if claimedByPoll && pollErr == nil {
		events.Record(events.TypeBonusClaimed, streamer.GetUsername(), "bonus claimed (GQL fallback)")
	}
	rt.release()
	waitBonusBarrier(t, pubSubDone, "PubSub claimant completion")

	if pollErr != nil {
		t.Fatalf("poll loser returned an error: %v", pollErr)
	}
	if got := rt.mutations.Load(); got != 1 {
		t.Fatalf("ClaimCommunityPoints mutation count = %d, want 1", got)
	}
	if claimedByPoll {
		t.Fatal("poll loser reported a second successful claim")
	}

	eventsForClaim := 0
	for _, event := range events.Recent(200) {
		if event.Type == events.TypeBonusClaimed && event.Streamer == streamer.GetUsername() {
			eventsForClaim++
		}
	}
	if eventsForClaim != 1 {
		t.Fatalf("local bonus success events = %d, want 1", eventsForClaim)
	}
	if output := logs.String(); strings.Contains(output, "level=ERROR") || strings.Contains(output, "Failed to claim bonus") {
		t.Fatalf("benign poll loser emitted an ERROR: %s", output)
	} else if strings.Contains(output, "claim-shared") {
		t.Fatalf("claim ID leaked into arbitration logs: %s", output)
	}
}

// The reverse ordering pins the other production permutation: the fallback
// poll owns the mutation and a concurrent PubSub delivery is a benign loser.
func TestBonusClaimPollOwnerPubSubLoserIsBenign(t *testing.T) {
	previousLogger := slog.Default()
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	rt := &bonusArbitrationRoundTripper{
		firstEntered: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	previousTransport := http.DefaultTransport
	http.DefaultTransport = rt
	t.Cleanup(func() {
		rt.release()
		http.DefaultTransport = previousTransport
	})

	twitchAuth := auth.NewTwitchAuth("tester", "device-id")
	twitchAuth.ReplaceCredentials(auth.TokenResponse{AccessToken: "dummy-token"})
	twitchAuth.SetUserID("100")
	client := twitch.NewTwitchClient(twitchAuth, "device-id")
	streamer := newTestStreamer(1000)
	streamer.Username = fmt.Sprintf("bonus-reverse-%d", bonusArbitrationTestSeq.Add(1))
	pool := &WebSocketPool{actor: client}

	pollDone := make(chan error, 1)
	go func() {
		claimed, err := client.ClaimAvailableBonus(streamer)
		if claimed && err == nil {
			events.Record(events.TypeBonusClaimed, streamer.GetUsername(), "bonus claimed (GQL fallback)")
		}
		pollDone <- err
	}()
	waitBonusBarrier(t, rt.firstEntered, "fallback-poll mutation")

	pool.handleCommunityPointsUser(&PubSubMessage{
		Type: "claim-available",
		Data: map[string]interface{}{
			"claim": map[string]interface{}{"id": "claim-shared"},
		},
	}, streamer)
	if got := rt.mutations.Load(); got != 1 {
		t.Fatalf("PubSub loser caused %d mutations before release, want 1", got)
	}

	rt.release()
	select {
	case err := <-pollDone:
		if err != nil {
			t.Fatalf("poll owner returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for poll owner")
	}

	eventsForClaim := 0
	for _, event := range events.Recent(200) {
		if event.Type == events.TypeBonusClaimed && event.Streamer == streamer.GetUsername() {
			eventsForClaim++
		}
	}
	if eventsForClaim != 1 {
		t.Fatalf("local bonus success events = %d, want 1", eventsForClaim)
	}
	if output := logs.String(); strings.Contains(output, "level=ERROR") || strings.Contains(output, "Failed to claim bonus") {
		t.Fatalf("benign PubSub loser emitted an ERROR: %s", output)
	} else if strings.Contains(output, "claim-shared") {
		t.Fatalf("claim ID leaked into arbitration logs: %s", output)
	}
}
