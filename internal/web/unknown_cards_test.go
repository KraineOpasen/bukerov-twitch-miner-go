package web

import (
	"encoding/json"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

func echoTr(k string) string { return k }

// TestBuildCardsTriState pins the dashboard tri-state grouping: confirmed-online
// goes to the live group, confirmed-offline to the offline group, and an
// unknown (not slotted) into its OWN group with State="unknown", Unconfirmed=true,
// is_live=false and NO offline duration — never silently into the offline group.
func TestBuildCardsTriState(t *testing.T) {
	online := models.NewStreamer("liveone", models.DefaultStreamerSettings())
	online.SetConfirmedOnline()

	unknown := models.NewStreamer("unsure", models.DefaultStreamerSettings())
	unknown.SetConfirmedOnline()
	unknown.SetUnknown(models.ReasonTransportError) // unknown, last-confirmed online, NOT slotted

	offline := models.NewStreamer("gone", models.DefaultStreamerSettings())
	offline.SetConfirmedOffline()

	srv := &Server{}
	slots := WatchSlotsView{Watching: map[string]bool{}} // nobody holds a slot
	live, unk, off, _, _ := srv.buildCards(
		[]*models.Streamer{online, unknown, offline},
		slots, map[string]streamerStats{}, map[string]bool{}, echoTr,
	)

	if len(live) != 1 || live[0].Name != "liveone" {
		t.Fatalf("live group = %+v, want [liveone]", live)
	}
	if !live[0].IsLive || live[0].Status != "online" {
		t.Errorf("online card: IsLive=%v Status=%q, want true/online", live[0].IsLive, live[0].Status)
	}

	if len(unk) != 1 || unk[0].Name != "unsure" {
		t.Fatalf("unknown group = %+v, want [unsure]", unk)
	}
	uc := unk[0]
	if uc.State != "unknown" {
		t.Errorf("unknown card State = %q, want unknown", uc.State)
	}
	if !uc.Unconfirmed {
		t.Error("unknown card must be flagged Unconfirmed")
	}
	if uc.IsLive {
		t.Error("unknown card must not be is_live")
	}
	if uc.Status != "unknown" {
		t.Errorf("unknown card Status = %q, want unknown", uc.Status)
	}
	if uc.OfflineDuration != "" {
		t.Errorf("unknown card must not carry an offline duration, got %q", uc.OfflineDuration)
	}

	if len(off) != 1 || off[0].Name != "gone" || off[0].State != "offline" {
		t.Fatalf("offline group = %+v, want [gone/offline]", off)
	}
}

// TestBuildCardsSlottedUnknownStaysLiveUnconfirmed proves that a streamer holding
// a watch slot that goes online→unknown stays in the live group as "watching" but
// flagged Unconfirmed and NOT counted as is_live.
func TestBuildCardsSlottedUnknownStaysLiveUnconfirmed(t *testing.T) {
	s := models.NewStreamer("slotted", models.DefaultStreamerSettings())
	s.SetConfirmedOnline()
	s.SetUnknown(models.ReasonTransportError)

	srv := &Server{}
	slots := WatchSlotsView{Watching: map[string]bool{"slotted": true}} // holds a slot
	live, unk, _, _, _ := srv.buildCards(
		[]*models.Streamer{s}, slots, map[string]streamerStats{}, map[string]bool{}, echoTr,
	)

	if len(unk) != 0 {
		t.Fatalf("a slotted unknown must stay in the live group, unknown group = %+v", unk)
	}
	if len(live) != 1 {
		t.Fatalf("live group = %+v, want the slotted-unconfirmed card", live)
	}
	c := live[0]
	if c.State != "watching" || !c.Watching {
		t.Errorf("slotted unknown State = %q Watching=%v, want watching/true", c.State, c.Watching)
	}
	if !c.Unconfirmed {
		t.Error("slotted unknown must be flagged Unconfirmed")
	}
	if c.IsLive {
		t.Error("slotted unknown must not be counted as is_live")
	}
}

// TestStreamerInfoIsLiveDerivedJSON verifies the backward-compatible JSON contract:
// the additive `status` field is present and `is_live == (status == "online")`.
func TestStreamerInfoIsLiveDerivedJSON(t *testing.T) {
	cases := []struct {
		status string
		live   bool
	}{
		{"online", true},
		{"offline", false},
		{"unknown", false},
	}
	for _, tc := range cases {
		info := StreamerInfo{Name: "x", Status: tc.status, IsLive: tc.status == "online"}
		b, err := json.Marshal(info)
		if err != nil {
			t.Fatal(err)
		}
		var round map[string]any
		if err := json.Unmarshal(b, &round); err != nil {
			t.Fatal(err)
		}
		if round["status"] != tc.status {
			t.Errorf("json status = %v, want %q", round["status"], tc.status)
		}
		if round["is_live"] != tc.live {
			t.Errorf("json is_live = %v, want %v (must equal status==online)", round["is_live"], tc.live)
		}
	}
}
