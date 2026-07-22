package api

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"sync"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// --- Corrective pass 2: refresh-intent, no-op gate, stale propagation ---

const validStreamInfoBody = `{"data":{"user":{"stream":{"id":"b1","viewersCount":3},"broadcastSettings":{"title":"t","game":{"id":"g1","name":"GameX"}}}}}`

// newSessionClient builds a client whose GQL endpoint is a local httptest server
// and whose Spade discovery always succeeds (injected fake), so the full
// bring-online / refresh path can be exercised end-to-end.
func newSessionClient(t *testing.T, gql http.HandlerFunc) *TwitchClient {
	t.Helper()
	c := newTestClient(t, gql)
	c.twitchBaseURL = "https://" + testChannelHost
	c.spadeHTTP = &fakeSpadeHTTP{handler: validHandler}
	return c
}

// TestUpdateStreamNoOpDoesNotSupersedeInFlightRefresh (Blocker 1): a gated metadata
// UpdateStream must do NO work and start NO observation, so it cannot supersede a
// concurrent real refresh that is mid-flight — the real refresh still applies.
func TestUpdateStreamNoOpDoesNotSupersedeInFlightRefresh(t *testing.T) {
	c := newSessionClient(t, updateStreamHandler(t, validStreamInfoBody, ""))
	s := models.NewStreamer("streamer", models.StreamerSettings{ClaimDrops: false})
	s.ChannelID = "cid"
	// Fresh lastUpdate so a metadata UpdateStream is DUE=false (a no-op).
	s.Stream.Update("b1", "t", &models.Game{ID: "g1", Name: "GameX"}, nil, 1)

	type noopObs struct {
		res             SessionRefreshResult
		err             error
		obsPre, obsPost uint64
	}
	noopc := make(chan noopObs, 1)

	// While the forced refresh is mid-flight (observation begun, about to apply),
	// fire a gated metadata UpdateStream and capture whether it started an
	// observation.
	c.beforeSessionApply = func() {
		obsPre := s.Stream.SessionObservation()
		res, err := c.doRefreshPlaybackSession(context.Background(), s, playbackRefreshIntent{})
		obsPost := s.Stream.SessionObservation()
		noopc <- noopObs{res, err, obsPre, obsPost}
	}

	// The forced recovery always fetches stream info and applies.
	res := c.RefreshPlaybackSession(s, false, models.ExpectedSession{})
	got := <-noopc

	if !got.res.NoOp || got.err != nil {
		t.Fatalf("a gated metadata refresh must be an explicit NoOp with no error, got %+v err=%v", got.res, got.err)
	}
	if got.obsPre != got.obsPost {
		t.Fatalf("a no-op must not begin an observation: obs %d -> %d", got.obsPre, got.obsPost)
	}
	if got.res.Applied || got.res.Stale {
		t.Fatalf("a no-op must neither apply nor go stale, got %+v", got.res)
	}
	if !res.Applied || res.Stale {
		t.Fatalf("the in-flight refresh must still apply (not superseded by the no-op), got %+v", res)
	}
}

// TestUpdateStreamStaleIsNotNilSuccess (Blocker 2): when the session apply is
// superseded during a metadata refresh, UpdateStream must return
// ErrPlaybackSessionStale — never a silent nil-success — and the error carries no
// secret.
func TestUpdateStreamStaleIsNotNilSuccess(t *testing.T) {
	c := newSessionClient(t, updateStreamHandler(t, validStreamInfoBody, ""))
	s := models.NewStreamer("streamer", models.StreamerSettings{ClaimDrops: false})
	s.ChannelID = "cid"

	// Supersede the refresh's observation just before it applies.
	c.beforeSessionApply = func() { s.Stream.BeginSessionObservation() }

	err := c.UpdateStream(s)
	if !errors.Is(err, ErrPlaybackSessionStale) {
		t.Fatalf("a stale apply must surface as ErrPlaybackSessionStale, got %v", err)
	}
	for _, secret := range []string{"http://", "https://", "token", "sig="} {
		if err != nil && containsStr(err.Error(), secret) {
			t.Fatalf("stale error leaked %q: %q", secret, err.Error())
		}
	}
	// classifyCheck maps it to inconclusive UNKNOWN, never online/offline.
	if st, reason := classifyCheck(err); st != models.StatusUnknown || reason != models.ReasonSessionStale {
		t.Fatalf("stale must classify as Unknown/session_stale, got %v/%v", st, reason)
	}
}

// TestCheckStreamerOnlineStaleApplyDoesNotConfirmOnline (Blocker 2): a bring-online
// check whose session apply is superseded must record UNKNOWN — never confirm
// online — and must emit NO online/offline event.
func TestCheckStreamerOnlineStaleApplyDoesNotConfirmOnline(t *testing.T) {
	c := newSessionClient(t, updateStreamHandler(t, validStreamInfoBody, ""))
	s := models.NewStreamer("streamer", models.StreamerSettings{ClaimDrops: false})
	s.ChannelID = "cid"

	c.beforeSessionApply = func() { s.Stream.BeginSessionObservation() }

	tr := c.CheckStreamerOnline(s)
	if tr.Current != models.StatusUnknown || tr.Reason != models.ReasonSessionStale {
		t.Fatalf("a stale bring-online must be Unknown/session_stale, got %+v", tr)
	}
	if s.GetStatus() != models.StatusUnknown || s.GetIsOnline() {
		t.Fatalf("stale must not confirm online, got status=%v online=%v", s.GetStatus(), s.GetIsOnline())
	}
	if tr.OnlineConfirmed || tr.OfflineConfirmed {
		t.Fatalf("a stale apply must emit no online/offline event, got %+v", tr)
	}
}

// TestOnlineRefreshStaleDoesNotConfirmOnline (Blocker 2, online-refresh path): an
// already-online streamer whose metadata refresh apply is superseded records the
// inconclusive UNKNOWN (session_stale) via classifyCheck — a stale apply never
// re-confirms online.
func TestOnlineRefreshStaleDoesNotConfirmOnline(t *testing.T) {
	c := newSessionClient(t, updateStreamHandler(t, validStreamInfoBody, ""))
	s := models.NewStreamer("streamer", models.StreamerSettings{ClaimDrops: false})
	s.ChannelID = "cid"
	s.SetConfirmedOnline() // already online -> the online-refresh path

	c.beforeSessionApply = func() { s.Stream.BeginSessionObservation() }

	tr := c.CheckStreamerOnline(s)
	if tr.Current != models.StatusUnknown || tr.Reason != models.ReasonSessionStale {
		t.Fatalf("a stale online-refresh must be inconclusive Unknown/session_stale, never online, got %+v", tr)
	}
	if tr.OnlineConfirmed {
		t.Fatalf("a stale apply must not (re)confirm online, got %+v", tr)
	}
}

// TestConcurrentChecksNewestSessionObservationWins (Blocker 2): when a newer
// session applies while an older check is mid-flight, the newer session wins and
// the older check is stale — it does not overwrite the newer session.
func TestConcurrentChecksNewestSessionObservationWins(t *testing.T) {
	c := newSessionClient(t, updateStreamHandler(t, validStreamInfoBody, ""))
	s := models.NewStreamer("streamer", models.StreamerSettings{ClaimDrops: false})
	s.ChannelID = "cid"
	s.Stream.SetSpadeURL("https://spade.twitch.tv/known")

	// Just before the older refresh applies, a NEWER observation publishes a newer
	// session (broadcast bNEW).
	c.beforeSessionApply = func() {
		obs := s.Stream.BeginSessionObservation()
		cand := models.PlaybackSessionCandidate{BroadcastID: "bNEW"}.
			WithSpadeURL("https://spade.twitch.tv/new").
			WithPayload("cid", "bNEW", "uid", "streamer", nil)
		if r := s.Stream.ApplyPlaybackSessionIfCurrent(obs, cand, models.ExpectedSession{}); !r.Applied {
			t.Errorf("the newer session must apply, got %+v", r)
		}
	}

	err := c.UpdateStream(s) // the older refresh
	if !errors.Is(err, ErrPlaybackSessionStale) {
		t.Fatalf("the older refresh must be stale, got %v", err)
	}
	snap := s.Stream.SessionSnapshot()
	if snap.BroadcastID != "bNEW" || snap.SpadeURL != "https://spade.twitch.tv/new" {
		t.Fatalf("the newest session must win, got broadcast=%q spade=%q", snap.BroadcastID, snap.SpadeURL)
	}
}

// TestUnknownWithFreshLastUpdateStillRequiresGQLToConfirmOnline (Blocker 3): with a
// fresh lastUpdate and status Unknown, bring-online must STILL fetch stream info —
// a valid stream confirms online, stream:null confirms offline, and a malformed
// response stays Unknown. A successful Spade fetch alone never confirms online.
func TestUnknownWithFreshLastUpdateStillRequiresGQLToConfirmOnline(t *testing.T) {
	cases := []struct {
		name       string
		streamBody string
		wantStatus models.StreamerStatus
	}{
		{"valid stream => online", validStreamInfoBody, models.StatusOnline},
		{"stream null => offline", `{"data":{"user":{"stream":null}}}`, models.StatusOffline},
		{"stream absent => unknown", `{"data":{"user":{}}}`, models.StatusUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newSessionClient(t, updateStreamHandler(t, tc.streamBody, ""))
			s := models.NewStreamer("streamer", models.StreamerSettings{ClaimDrops: false})
			s.ChannelID = "cid"
			// FRESH lastUpdate (a prior successful refresh) — the old gate would have
			// skipped the stream-info GQL and confirmed online on stale cached data.
			s.Stream.Update("bOLD", "t", &models.Game{ID: "g1", Name: "GameX"}, nil, 1)
			// Status is Unknown (fresh streamer). The spade fetch will succeed.

			tr := c.CheckStreamerOnline(s)
			if tr.Current != tc.wantStatus {
				t.Fatalf("with a fresh lastUpdate, bring-online must be driven by the stream-info GQL: want %v, got %v (%+v)",
					tc.wantStatus, tr.Current, tr)
			}
			if s.GetStatus() != tc.wantStatus {
				t.Fatalf("streamer status = %v, want %v", s.GetStatus(), tc.wantStatus)
			}
		})
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return len(sub) == 0
}

// --- Blocker 4: stale playback refresh must not publish campaign availability ---

// TestStalePlaybackRefreshDoesNotPublishCampaignAvailability: a full refresh whose
// playback apply is superseded must NOT publish the channel-side availability it
// fetched — the stale refresh's campaign IDs never become authoritative.
func TestStalePlaybackRefreshDoesNotPublishCampaignAvailability(t *testing.T) {
	availNew := `{"data":{"channel":{"id":"cid","viewerDropCampaigns":[{"id":"camp-NEW"}]}}}`
	c := newSessionClient(t, updateStreamHandler(t, validStreamInfoBody, availNew))
	s := models.NewStreamer("streamer", models.StreamerSettings{ClaimDrops: true})
	s.ChannelID = "cid"
	s.Stream.SetCampaignIDs([]string{"camp-OLD"}) // prior Known list

	// Supersede the playback observation just before the apply.
	c.beforeSessionApply = func() { s.Stream.BeginSessionObservation() }

	err := c.UpdateStream(s)
	if !errors.Is(err, ErrPlaybackSessionStale) {
		t.Fatalf("expected a stale refresh, got %v", err)
	}
	state, ids := s.Stream.CampaignAvailability()
	if !reflect.DeepEqual(ids, []string{"camp-OLD"}) {
		t.Fatalf("a stale refresh must not publish new availability IDs, got %v (state %v)", ids, state)
	}
	if state != models.CampaignAvailabilityKnown {
		t.Fatalf("the prior Known availability must be untouched, got %v", state)
	}
}

// TestSuccessfulRefreshPublishesCampaignAvailability: the current (applied) refresh
// still publishes Known IDs — Blocker 4 must not break the normal path.
func TestSuccessfulRefreshPublishesCampaignAvailability(t *testing.T) {
	availNew := `{"data":{"channel":{"id":"cid","viewerDropCampaigns":[{"id":"camp-NEW"}]}}}`
	c := newSessionClient(t, updateStreamHandler(t, validStreamInfoBody, availNew))
	s := models.NewStreamer("streamer", models.StreamerSettings{ClaimDrops: true})
	s.ChannelID = "cid"
	s.Stream.SetCampaignIDs([]string{"camp-OLD"})

	if err := c.UpdateStream(s); err != nil {
		t.Fatalf("UpdateStream: %v", err)
	}
	state, ids := s.Stream.CampaignAvailability()
	if state != models.CampaignAvailabilityKnown || !reflect.DeepEqual(ids, []string{"camp-NEW"}) {
		t.Fatalf("a successful refresh must publish the fresh Known IDs, got %v state %v", ids, state)
	}
}

// --- Corrective pass 3: availability observation aligned with session observation ---

// runAvailabilityInterleave drives two concurrent refreshes on ONE streamer with
// channel barriers (no time.Sleep): refresh A begins its session observation FIRST
// but blocks inside its stream-info fetch; refresh B begins SECOND, fetches
// everything, and parks just before its playback apply. Both then park before
// apply, and the caller chooses which applies first. In every case the NEWER
// session B must win BOTH the playback session and the channel availability — the
// older refresh A can never supersede B's availability observation.
func runAvailabilityInterleave(t *testing.T, olderAppliesFirst bool) {
	t.Helper()
	const (
		availA = `{"data":{"channel":{"id":"cid","viewerDropCampaigns":[{"id":"camp-A"}]}}}`
		availB = `{"data":{"channel":{"id":"cid","viewerDropCampaigns":[{"id":"camp-B"}]}}}`
	)

	var aStreamOnce sync.Once
	aStreamReached := make(chan struct{})
	releaseAStream := make(chan struct{})
	aBeforeApply := make(chan struct{})
	releaseAApply := make(chan struct{})
	bBeforeApply := make(chan struct{})
	releaseBApply := make(chan struct{})

	cA := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch gqlOperationName(r) {
		case constants_VideoPlayerStreamInfoOverlayChannel:
			aStreamOnce.Do(func() { close(aStreamReached) })
			<-releaseAStream
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(validStreamInfoBody))
		case constants_DropsHighlightServiceAvailableDrops:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(availA))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":{}}`))
		}
	})
	cB := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch gqlOperationName(r) {
		case constants_VideoPlayerStreamInfoOverlayChannel:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(validStreamInfoBody))
		case constants_DropsHighlightServiceAvailableDrops:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(availB))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":{}}`))
		}
	})
	cA.beforeSessionApply = func() { close(aBeforeApply); <-releaseAApply }
	cB.beforeSessionApply = func() { close(bBeforeApply); <-releaseBApply }

	s := models.NewStreamer("streamer", models.StreamerSettings{ClaimDrops: true})
	s.ChannelID = "cid"
	s.Stream.SetCampaignIDs([]string{"camp-OLD"}) // prior Known list

	var resA, resB SessionRefreshResult
	aDone := make(chan struct{})
	bDone := make(chan struct{})

	// A begins its session (and availability) observation first, then blocks in
	// stream-info.
	go func() { resA = cA.RefreshPlaybackSession(s, false, models.ExpectedSession{}); close(aDone) }()
	<-aStreamReached

	// B begins its observations second and runs to just before its apply.
	go func() { resB = cB.RefreshPlaybackSession(s, false, models.ExpectedSession{}); close(bDone) }()
	<-bBeforeApply

	// Release A's stream-info; A fetches its availability and reaches its apply.
	close(releaseAStream)
	<-aBeforeApply

	if olderAppliesFirst {
		close(releaseAApply)
		<-aDone
		close(releaseBApply)
		<-bDone
	} else {
		close(releaseBApply)
		<-bDone
		close(releaseAApply)
		<-aDone
	}

	if !resA.Stale {
		t.Fatalf("the older refresh A must be stale, got %+v", resA)
	}
	if !resB.Applied || resB.Stale {
		t.Fatalf("the newer refresh B must apply, got %+v", resB)
	}
	state, ids := s.Stream.CampaignAvailability()
	if state != models.CampaignAvailabilityKnown || !reflect.DeepEqual(ids, []string{"camp-B"}) {
		t.Fatalf("availability must be the newer session's (Known [camp-B]); camp-A must never publish and camp-OLD must be replaced — got state=%v ids=%v",
			state, ids)
	}
}

// TestOlderPlaybackRefreshCannotSupersedeNewerAvailabilityObservation: the newer
// session B applies first; the older A is stale. B's playback session and
// availability stay consistent (Known [camp-B]).
func TestOlderPlaybackRefreshCannotSupersedeNewerAvailabilityObservation(t *testing.T) {
	runAvailabilityInterleave(t, false)
}

// TestNewerPlaybackRefreshAvailabilityWinsWhenOlderCompletesFirst: the other
// completion order — the older A applies (and goes stale) first, then B applies.
// The result is still Known [camp-B].
func TestNewerPlaybackRefreshAvailabilityWinsWhenOlderCompletesFirst(t *testing.T) {
	runAvailabilityInterleave(t, true)
}

// TestObservationPairCannotInvertBetweenConcurrentRefreshes (CP4) is the
// deterministic split-window regression for the atomic observation-pair
// allocator. Two concurrent refreshes on ONE streamer are driven with channel
// barriers (no time.Sleep) so their observation reservations interleave at
// exactly the point a split allocator could invert:
//
//	A reserves its pair (blocks right after) → B reserves its pair and parks
//	before apply → A resumes and parks before apply → B applies → A applies.
//
// With the atomic allocator, A always gets (session=1, avail=1) and B always
// gets (session=2, avail=2): B is newest in BOTH domains, so B applies the
// playback session AND publishes camp-B, and A is stale. With the OLD split
// allocator (M23: session and availability reserved in two separate critical
// sections, A pausing between them) the pair inverts to A=(s1,a2), B=(s2,a1):
// B still wins the playback session but its availability observation (a1) is now
// stale against A's later a2, so B's camp-B publish is rejected and availability
// is left at the prior camp-OLD — a session/availability inconsistency. This
// test pins the atomic outcome (Known [camp-B]).
func TestObservationPairCannotInvertBetweenConcurrentRefreshes(t *testing.T) {
	const (
		availA = `{"data":{"channel":{"id":"cid","viewerDropCampaigns":[{"id":"camp-A"}]}}}`
		availB = `{"data":{"channel":{"id":"cid","viewerDropCampaigns":[{"id":"camp-B"}]}}}`
	)

	availHandler := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			switch gqlOperationName(r) {
			case constants_VideoPlayerStreamInfoOverlayChannel:
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(validStreamInfoBody))
			case constants_DropsHighlightServiceAvailableDrops:
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(body))
			default:
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"data":{}}`))
			}
		}
	}

	cA := newTestClient(t, availHandler(availA))
	cB := newTestClient(t, availHandler(availB))

	aReserved := make(chan struct{})
	releaseA := make(chan struct{})
	aBeforeApply := make(chan struct{})
	releaseAApply := make(chan struct{})
	bBeforeApply := make(chan struct{})
	releaseBApply := make(chan struct{})

	// A parks immediately after reserving its observation pair (in the split
	// allocator this seam sits BETWEEN the two Begin* calls, so A's availability id
	// is reserved only after it resumes — forcing the inversion).
	cA.afterRefreshObservation = func() { close(aReserved); <-releaseA }
	cA.beforeSessionApply = func() { close(aBeforeApply); <-releaseAApply }
	cB.beforeSessionApply = func() { close(bBeforeApply); <-releaseBApply }

	s := models.NewStreamer("streamer", models.StreamerSettings{ClaimDrops: true})
	s.ChannelID = "cid"
	s.Stream.SetCampaignIDs([]string{"camp-OLD"}) // prior Known list

	var resA, resB SessionRefreshResult
	aDone := make(chan struct{})
	bDone := make(chan struct{})

	// A reserves first, then blocks at afterRefreshObservation.
	go func() { resA = cA.RefreshPlaybackSession(s, false, models.ExpectedSession{}); close(aDone) }()
	<-aReserved

	// B reserves second and runs to just before its apply.
	go func() { resB = cB.RefreshPlaybackSession(s, false, models.ExpectedSession{}); close(bDone) }()
	<-bBeforeApply

	// Only now release A — in the split allocator this is where A reserves its
	// (higher) availability id, after B already reserved a lower one. A runs to just
	// before its own apply.
	close(releaseA)
	<-aBeforeApply

	// Both parked before apply. Let the newer session B apply first, then A.
	close(releaseBApply)
	<-bDone
	close(releaseAApply)
	<-aDone

	if !resA.Stale {
		t.Fatalf("the older refresh A must be stale, got %+v", resA)
	}
	if !resB.Applied || resB.Stale {
		t.Fatalf("the newer refresh B must apply, got %+v", resB)
	}
	state, ids := s.Stream.CampaignAvailability()
	if state != models.CampaignAvailabilityKnown || !reflect.DeepEqual(ids, []string{"camp-B"}) {
		t.Fatalf("atomic pair must keep availability consistent with the newer session (Known [camp-B]); "+
			"an inverted pair would leave it at camp-OLD — got state=%v ids=%v", state, ids)
	}
}

// TestMalformedAvailabilityStillPublishesUnknown: a successful playback apply with
// a FAILED availability lookup still records Unknown (keeping prior IDs) — the
// existing tri-state contract is preserved.
func TestMalformedAvailabilityStillPublishesUnknown(t *testing.T) {
	availErr := `{"errors":[{"message":"service error"}],"data":null}`
	c := newSessionClient(t, updateStreamHandler(t, validStreamInfoBody, availErr))
	s := models.NewStreamer("streamer", models.StreamerSettings{ClaimDrops: true})
	s.ChannelID = "cid"
	s.Stream.SetCampaignIDs([]string{"camp-OLD"})

	if err := c.UpdateStream(s); err != nil {
		t.Fatalf("UpdateStream: %v", err)
	}
	state, ids := s.Stream.CampaignAvailability()
	if state != models.CampaignAvailabilityUnknown {
		t.Fatalf("a failed availability lookup must record Unknown, got %v", state)
	}
	if !reflect.DeepEqual(ids, []string{"camp-OLD"}) {
		t.Fatalf("prior IDs must be retained as last-known, got %v", ids)
	}
}
