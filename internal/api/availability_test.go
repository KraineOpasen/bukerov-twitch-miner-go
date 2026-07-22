package api

import (
	"io"
	"net/http"
	"reflect"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// TestGetCampaignIDsFromStreamerServiceFailureIsError verifies that a
// service-layer failure on the channel-side availability lookup (top-level GQL
// errors, data:null, or an absent/null channel node) surfaces as an ERROR — so
// the caller records availability UNKNOWN rather than an authoritative empty
// list — while a genuinely resolved response (channel present; campaigns
// absent/empty/populated) returns without error.
func TestGetCampaignIDsFromStreamerServiceFailureIsError(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr bool
		wantIDs []string
	}{
		{
			// HTTP 200 carrying a top-level errors array with data:null is the exact
			// degraded/rate-limited shape that must NOT become Known+empty.
			name:    "top-level GQL errors => error (unknown, not authoritative empty)",
			body:    `{"errors":[{"message":"service error"}],"data":null}`,
			wantErr: true,
		},
		{
			name:    "data null => error",
			body:    `{"data":null}`,
			wantErr: true,
		},
		{
			name:    "channel absent => error",
			body:    `{"data":{}}`,
			wantErr: true,
		},
		{
			name:    "channel null => error",
			body:    `{"data":{"channel":null}}`,
			wantErr: true,
		},
		{
			name:    "channel present, viewerDropCampaigns absent => authoritative empty",
			body:    `{"data":{"channel":{"id":"chan-1"}}}`,
			wantErr: false,
			wantIDs: nil,
		},
		{
			name:    "channel present, viewerDropCampaigns empty => authoritative empty",
			body:    `{"data":{"channel":{"id":"chan-1","viewerDropCampaigns":[]}}}`,
			wantErr: false,
			wantIDs: nil,
		},
		{
			name:    "channel present, campaigns populated => ids",
			body:    `{"data":{"channel":{"id":"chan-1","viewerDropCampaigns":[{"id":"camp-1"},{"id":"camp-2"}]}}}`,
			wantErr: false,
			wantIDs: []string{"camp-1", "camp-2"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, tc.body)
			})
			s := newTestStreamer("streamer")
			s.ChannelID = "chan-1"

			ids, err := c.GetCampaignIDsFromStreamer(s)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error (=> availability UNKNOWN), got ids=%v nil err", ids)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(ids, tc.wantIDs) {
				t.Fatalf("ids = %v, want %v", ids, tc.wantIDs)
			}
		})
	}
}

// updateStreamHandler answers the two GQL operations UpdateStream issues: the
// stream-info overlay and the channel-side availability lookup. availabilityBody
// is served for the availability op; when it is "" the test asserts the op is
// never invoked (the game-unresolved path must skip it).
func updateStreamHandler(t *testing.T, streamInfoBody, availabilityBody string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch gqlOperationName(r) {
		case constants_VideoPlayerStreamInfoOverlayChannel:
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, streamInfoBody)
		case constants_DropsHighlightServiceAvailableDrops:
			if availabilityBody == "" {
				t.Fatalf("availability lookup must be SKIPPED when the game is unresolved")
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, availabilityBody)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"data":{}}`)
		}
	}
}

const (
	constants_VideoPlayerStreamInfoOverlayChannel = "VideoPlayerStreamInfoOverlayChannel"
	constants_DropsHighlightServiceAvailableDrops = "DropsHighlightService_AvailableDrops"
)

// TestUpdateStreamAvailabilityUnknownOnLookupFailure verifies the end-to-end
// wiring for the MEDIUM finding: when the availability lookup fails at the GQL
// service layer, UpdateStream must record availability UNKNOWN and RETAIN the
// previous IDs as last-known continuity data — never clear them to an
// authoritative empty list (which the assignment path would read as "No").
func TestUpdateStreamAvailabilityUnknownOnLookupFailure(t *testing.T) {
	streamInfo := `{"data":{"user":{"stream":{"id":"b1","viewersCount":3},"broadcastSettings":{"title":"t","game":{"id":"g1","name":"GameX"}}}}}`
	availErr := `{"errors":[{"message":"service error"}],"data":null}`

	c := newTestClient(t, updateStreamHandler(t, streamInfo, availErr))

	s := models.NewStreamer("streamer", models.StreamerSettings{ClaimDrops: true})
	s.ChannelID = "chan-1"
	s.Stream.SetCampaignIDs([]string{"camp-1"}) // prior Known list, seq bumped

	if err := c.UpdateStream(s); err != nil {
		t.Fatalf("UpdateStream: %v", err)
	}

	state, ids := s.Stream.CampaignAvailability()
	if state != models.CampaignAvailabilityUnknown {
		t.Fatalf("availability = %v, want Unknown after a failed lookup", state)
	}
	if !reflect.DeepEqual(ids, []string{"camp-1"}) {
		t.Fatalf("previous IDs must be retained as last-known continuity, got %v", ids)
	}
}

// TestUpdateStreamAvailabilityUnknownWhenGameUnresolved verifies the LOW finding
// fix: when a stream refresh cannot resolve the channel's game (null/absent
// game), UpdateStream must mark availability UNKNOWN (keeping previous IDs)
// rather than leaving a stale Known list that reads as a fresh authoritative
// "Yes". The availability op must NOT be issued in this path.
func TestUpdateStreamAvailabilityUnknownWhenGameUnresolved(t *testing.T) {
	streamInfoNoGame := `{"data":{"user":{"stream":{"id":"b1","viewersCount":3},"broadcastSettings":{"title":"t"}}}}`

	// availabilityBody "" => the handler fails the test if the op is issued.
	c := newTestClient(t, updateStreamHandler(t, streamInfoNoGame, ""))

	s := models.NewStreamer("streamer", models.StreamerSettings{ClaimDrops: true})
	s.ChannelID = "chan-1"
	s.Stream.SetCampaignIDs([]string{"camp-1"}) // prior Known list

	if err := c.UpdateStream(s); err != nil {
		t.Fatalf("UpdateStream: %v", err)
	}

	state, ids := s.Stream.CampaignAvailability()
	if state != models.CampaignAvailabilityUnknown {
		t.Fatalf("availability = %v, want Unknown when the game is unresolved", state)
	}
	if !reflect.DeepEqual(ids, []string{"camp-1"}) {
		t.Fatalf("previous IDs must be retained as last-known continuity, got %v", ids)
	}
}

// TestGetCampaignIDsFromStreamerAllOrNothing verifies the exact-list policy: a
// present viewerDropCampaigns array becomes an authoritative Known list ONLY when
// every element is well-formed; a single malformed element makes the whole lookup
// an error (=> availability UNKNOWN), never a valid subset. Valid IDs are
// deduplicated and returned in deterministic (sorted) order.
func TestGetCampaignIDsFromStreamerAllOrNothing(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr bool
		wantIDs []string
	}{
		{
			name:    "valid + null element => error",
			body:    `{"data":{"channel":{"viewerDropCampaigns":[{"id":"c1"},null]}}}`,
			wantErr: true,
		},
		{
			name:    "valid + empty object => error",
			body:    `{"data":{"channel":{"viewerDropCampaigns":[{"id":"c1"},{}]}}}`,
			wantErr: true,
		},
		{
			name:    "valid + non-string id => error",
			body:    `{"data":{"channel":{"viewerDropCampaigns":[{"id":"c1"},{"id":42}]}}}`,
			wantErr: true,
		},
		{
			name:    "valid + empty-string id => error",
			body:    `{"data":{"channel":{"viewerDropCampaigns":[{"id":"c1"},{"id":""}]}}}`,
			wantErr: true,
		},
		{
			name:    "non-object element => error",
			body:    `{"data":{"channel":{"viewerDropCampaigns":[42]}}}`,
			wantErr: true,
		},
		{
			name:    "duplicate valid ids => one deterministic id",
			body:    `{"data":{"channel":{"viewerDropCampaigns":[{"id":"c1"},{"id":"c1"}]}}}`,
			wantErr: false,
			wantIDs: []string{"c1"},
		},
		{
			name:    "valid complete list => sorted exact ids",
			body:    `{"data":{"channel":{"viewerDropCampaigns":[{"id":"c2"},{"id":"c1"},{"id":"c3"}]}}}`,
			wantErr: false,
			wantIDs: []string{"c1", "c2", "c3"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, tc.body)
			})
			s := newTestStreamer("streamer")
			s.ChannelID = "chan-1"

			ids, err := c.GetCampaignIDsFromStreamer(s)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error (=> UNKNOWN) for a malformed element, got ids=%v", ids)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(ids, tc.wantIDs) {
				t.Fatalf("ids = %v, want %v", ids, tc.wantIDs)
			}
		})
	}
}

// TestUpdateStreamMalformedListPreservesPreviousIDs verifies that a malformed
// availability response (partial list) does NOT clear the previous Known IDs and
// does NOT flip to an authoritative No — it records UNKNOWN, keeping the prior
// list as last-known continuity data.
func TestUpdateStreamMalformedListPreservesPreviousIDs(t *testing.T) {
	streamInfo := `{"data":{"user":{"stream":{"id":"b1","viewersCount":3},"broadcastSettings":{"title":"t","game":{"id":"g1","name":"GameX"}}}}}`
	malformedAvail := `{"data":{"channel":{"viewerDropCampaigns":[{"id":"c1"},null]}}}`

	c := newTestClient(t, updateStreamHandler(t, streamInfo, malformedAvail))

	s := models.NewStreamer("streamer", models.StreamerSettings{ClaimDrops: true})
	s.ChannelID = "chan-1"
	s.Stream.SetCampaignIDs([]string{"camp-1"}) // prior Known list

	if err := c.UpdateStream(s); err != nil {
		t.Fatalf("UpdateStream: %v", err)
	}

	state, ids := s.Stream.CampaignAvailability()
	if state != models.CampaignAvailabilityUnknown {
		t.Fatalf("availability = %v, want Unknown after a malformed (partial) list", state)
	}
	if !reflect.DeepEqual(ids, []string{"camp-1"}) {
		t.Fatalf("previous IDs must be retained as last-known continuity, got %v", ids)
	}
}
