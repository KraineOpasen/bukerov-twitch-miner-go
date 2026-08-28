package miner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/auth"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamer"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/twitch"
)

var bonusPollTestSequence atomic.Uint64

type bonusPollRoundTripper struct {
	contexts  atomic.Int64
	mutations atomic.Int64
}

func (rt *bonusPollRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
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
		if rt.contexts.Add(1) == 1 {
			response = `{"errors":[{"message":"temporary context failure"}]}`
		} else {
			response = `{"data":{"community":{"channel":{"self":{"communityPoints":{"balance":777,"availableClaim":{"id":"claim-shared"}}}}}}}`
		}
	case "ClaimCommunityPoints":
		rt.mutations.Add(1)
		response = `{"data":{"claimCommunityPoints":{"claim":{"id":"accepted"}}}}`
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(response)),
		Request:    req,
	}, nil
}

func TestPollBonusesRecoversFromUnknownAndRecordsOneTruthfulEvent(t *testing.T) {
	previousLogger := slog.Default()
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	rt := &bonusPollRoundTripper{}
	previousTransport := http.DefaultTransport
	http.DefaultTransport = rt
	t.Cleanup(func() { http.DefaultTransport = previousTransport })

	twitchAuth := auth.NewTwitchAuth("tester", "device-id")
	twitchAuth.ReplaceCredentials(auth.TokenResponse{AccessToken: "dummy-token"})
	twitchAuth.SetUserID("100")
	client := twitch.NewTwitchClient(twitchAuth, "device-id")

	login := fmt.Sprintf("bonus-poll-%d", bonusPollTestSequence.Add(1))
	cfg := &config.Config{
		Username:         "tester",
		StreamerSettings: models.DefaultStreamerSettings(),
		Streamers:        []config.StreamerConfig{{Username: login}},
	}
	manager := streamer.NewManager(fakeStreamerAPI{}, cfg.StreamerSettings)
	if err := manager.LoadFromConfig(cfg.Streamers, nil); err != nil {
		t.Fatalf("load streamers: %v", err)
	}
	s := manager.Get(login)
	s.SetConfirmedOnline()
	s.SetChannelPointsCapability(models.CapabilityEnabled, models.CapReasonConfirmedContext)

	m := &Miner{config: cfg, client: client, streamers: manager}
	m.pollBonuses() // inconclusive context => Unknown, no mutation
	if state := s.GetChannelPointsCapability(); state != models.CapabilityUnknown {
		t.Fatalf("first poll capability=%v, want unknown", state)
	}
	m.pollBonuses() // full context recovers Enabled and wins the mutation
	m.pollBonuses() // completed duplicate is benign

	if rt.contexts.Load() != 3 || rt.mutations.Load() != 1 {
		t.Fatalf("context requests=%d mutations=%d, want 3/1", rt.contexts.Load(), rt.mutations.Load())
	}
	if points := s.GetChannelPoints(); points != 777 {
		t.Fatalf("recovered full-context balance=%d, want 777", points)
	}
	eventCount := 0
	for _, event := range events.Recent(200) {
		if event.Type == events.TypeBonusClaimed && event.Streamer == login {
			eventCount++
		}
	}
	if eventCount != 1 {
		t.Fatalf("fallback success events=%d, want 1", eventCount)
	}
	output := logs.String()
	if got := strings.Count(output, "Claimed channel points bonus via GQL fallback poll"); got != 1 {
		t.Fatalf("truthful fallback success logs=%d, want 1; logs: %s", got, output)
	}
	if strings.Contains(output, "PubSub missed") || strings.Contains(output, "claim-shared") {
		t.Fatalf("fallback log made an unsupported claim or leaked claim ID: %s", output)
	}
}
