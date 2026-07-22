package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// --- Corrective pass Group B: production UpdateStream boundary ---

// payloadBroadcastID decodes the broadcast_id embedded in a snapshot's beacon
// payload (the api package cannot read the unexported payload field directly).
func payloadBroadcastID(t *testing.T, snap models.PlaybackSessionSnapshot) string {
	t.Helper()
	if !snap.HasPayload() {
		return ""
	}
	b64, err := snap.EncodePayload()
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var events []models.MinuteWatchedEvent
	if err := json.Unmarshal(data, &events); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if len(events) == 0 {
		return ""
	}
	id, _ := events[0].Properties["broadcast_id"].(string)
	return id
}

// TestUpdateStreamNoPartialSessionWindow proves the corrective-pass Blocker-1 fix:
// the campaign-availability network I/O that runs between parsing stream info and
// publishing the session must NOT expose a partial playback session. The test
// blocks the availability GQL query mid-flight and asserts the live snapshot is
// still ENTIRELY the old broadcast (never new-broadcast + old-payload); a
// beforeSessionApply barrier asserts the same just before the single atomic apply;
// and a concurrent reader asserts every snapshot's broadcast and payload agree.
// Run under -race.
func TestUpdateStreamNoPartialSessionWindow(t *testing.T) {
	streamInfoB2 := `{"data":{"user":{"stream":{"id":"b2","viewersCount":3},"broadcastSettings":{"title":"t2","game":{"id":"g1","name":"GameX"}}}}}`

	availReady := make(chan struct{})
	release := make(chan struct{})
	var readyOnce sync.Once

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch gqlOperationName(r) {
		case constants_VideoPlayerStreamInfoOverlayChannel:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(streamInfoB2))
		case constants_DropsHighlightServiceAvailableDrops:
			// The availability lookup is in flight: hold it so the test can observe
			// the session state during the campaign-availability I/O window.
			readyOnce.Do(func() { close(availReady) })
			<-release
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":{"channel":{"id":"cid","viewerDropCampaigns":[]}}}`))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":{}}`))
		}
	})

	// Seed an OLD session (broadcast b1, payload b1) via the legacy setters.
	s := models.NewStreamer("streamer", models.StreamerSettings{ClaimDrops: true})
	s.ChannelID = "cid"
	s.Stream.Update("b1", "t1", &models.Game{ID: "g1", Name: "GameX"}, nil, 1)
	s.Stream.SetSpadeURL("https://spade.twitch.tv/u1")
	s.Stream.SetPayload("cid", "b1", "uid", "streamer", &models.Game{ID: "g1", Name: "GameX"})

	isOld := func() bool {
		snap := s.Stream.SessionSnapshot()
		return snap.BroadcastID == "b1" && payloadBroadcastID(t, snap) == "b1"
	}
	isNew := func() bool {
		snap := s.Stream.SessionSnapshot()
		return snap.BroadcastID == "b2" && payloadBroadcastID(t, snap) == "b2"
	}

	// Barrier: just before the single atomic apply, the live session must still be
	// entirely old (no field of the new tuple published yet).
	var preApplyOld atomic.Bool
	preApplyOld.Store(true)
	c.beforeSessionApply = func() {
		if !isOld() {
			preApplyOld.Store(false)
		}
	}

	// Concurrent reader: every snapshot must be wholly old or wholly new.
	var mixed atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				if !isOld() && !isNew() {
					mixed.Add(1)
				}
			}
		}
	}()

	// Bypass the 2-minute refresh gate so the stream-info + availability fetch runs.
	s.Stream.ForceUpdateRequired()

	errc := make(chan error, 1)
	go func() { errc <- c.UpdateStream(s) }()

	// While the availability query is held, the session must be entirely old.
	<-availReady
	if !isOld() {
		t.Fatal("a partial playback session was visible during the campaign-availability I/O")
	}
	close(release)

	if err := <-errc; err != nil {
		t.Fatalf("UpdateStream: %v", err)
	}
	close(stop)
	wg.Wait()

	if !preApplyOld.Load() {
		t.Fatal("the pre-apply barrier observed a partial session (publication was not atomic)")
	}
	if mixed.Load() != 0 {
		t.Fatalf("a concurrent reader observed %d incoherent snapshots", mixed.Load())
	}
	if !isNew() {
		snap := s.Stream.SessionSnapshot()
		t.Fatalf("after the refresh the session must be the new broadcast, got broadcast=%q payloadBc=%q",
			snap.BroadcastID, payloadBroadcastID(t, snap))
	}
}
