package discovery

import (
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// fakeCampaigns is a canned CampaignsProvider.
type fakeCampaigns struct {
	campaigns []*models.Campaign
}

func (f *fakeCampaigns) Campaigns() []*models.Campaign { return f.campaigns }

// fakeClient records CheckStreamerOnline calls and can flip streamers online.
type fakeClient struct {
	online  map[string]bool
	checked []string
	streams []api.DirectoryStream
	err     error
}

func (f *fakeClient) CheckStreamerOnline(s *models.Streamer) {
	f.checked = append(f.checked, s.Username)
	if f.online[s.Username] {
		s.SetOnline()
	} else {
		s.SetOffline()
	}
}

func (f *fakeClient) GetDirectoryStreams(gameName string, limit int) ([]api.DirectoryStream, error) {
	return f.streams, f.err
}

// fakeSlotStatus reports a fixed set of logins as holding a watch slot, so
// State() (which asks the broker whether a proposed channel really is being
// watched) can be exercised without a live broker. origin optionally overrides
// the reported slot origin per login; a watching login with no explicit origin
// defaults to "discovery" (the broker placed discovery's own proposal).
type fakeSlotStatus struct {
	watching map[string]bool
	origin   map[string]string
}

func (f *fakeSlotStatus) IsWatching(login string) bool { return f.watching[login] }

func (f *fakeSlotStatus) WatchingOrigin(login string) string {
	if o, ok := f.origin[login]; ok {
		return o
	}
	if f.watching[login] {
		return "discovery"
	}
	return ""
}

func activeCampaign(gameID, gameName string) *models.Campaign {
	return &models.Campaign{
		ID:          "camp-" + gameID,
		Name:        gameName + " Campaign",
		Game:        &models.Game{ID: gameID, Name: gameName, DisplayName: gameName},
		ClaimStatus: models.CampaignClaimStatusInProgress,
		Drops:       []*models.Drop{{ID: "drop-1", Name: "Reward", MinutesRequired: 120}},
	}
}

func onlineCandidate(login, channelID, game, gameID string, viewers int) *Channel {
	ch := &Channel{
		Streamer:     newEphemeralStreamer(login, channelID),
		Game:         game,
		GameID:       gameID,
		Viewers:      viewers,
		DropsEnabled: true,
	}
	ch.Streamer.IsOnline = true
	ch.Streamer.Stream.Update("b-"+channelID, "title", &models.Game{ID: gameID, Name: game}, nil, viewers)
	ch.Streamer.Stream.CampaignIDs = []string{"camp-" + gameID}
	return ch
}

type fakeTracked struct {
	names []string
}

func (f *fakeTracked) Names() []string { return f.names }

func newTestManager(games []string, campaigns *fakeCampaigns, client *fakeClient) *Manager {
	m := NewManager(nil, campaigns, &fakeTracked{}, testRateLimits(), games, config.DiscoveryModeAll)
	m.client = client
	return m
}

// newTrackedOnlyManager builds a manager in tracked_only mode with the given
// configured streamer list, for exercising the inverted exclusion gates.
func newTrackedOnlyManager(games, tracked []string, campaigns *fakeCampaigns, client *fakeClient) *Manager {
	m := NewManager(nil, campaigns, &fakeTracked{names: tracked}, testRateLimits(), games, config.DiscoveryModeTrackedOnly)
	m.client = client
	return m
}

func testRateLimits() (rl config.RateLimitSettings) {
	rl = config.DefaultRateLimitSettings()
	return rl
}

func TestActiveCampaignGames(t *testing.T) {
	claimed := activeCampaign("g2", "Other Game")
	claimed.ClaimStatus = models.CampaignClaimStatusAlreadyClaimed
	noDrops := activeCampaign("g3", "Empty Game")
	noDrops.Drops = nil

	provider := &fakeCampaigns{campaigns: []*models.Campaign{
		activeCampaign("g1", "World of Tanks"),
		claimed,
		noDrops,
	}}

	m := newTestManager([]string{"World of Tanks"}, provider, &fakeClient{})

	games := m.activeCampaignGames()
	if games["world of tanks"] != "g1" {
		t.Errorf("expected World of Tanks mapped to g1, got %v", games)
	}
	if _, ok := games["other game"]; ok {
		t.Error("already-claimed campaign must not make its game active")
	}
	if _, ok := games["empty game"]; ok {
		t.Error("campaign with no remaining drops must not make its game active")
	}

	if !m.gameStillActive("g1") {
		t.Error("expected g1 active")
	}
	if m.gameStillActive("g2") || m.gameStillActive("g3") || m.gameStillActive("") {
		t.Error("expected g2/g3/empty inactive")
	}
}

func TestSelectBestPrefersPoolOrder(t *testing.T) {
	provider := &fakeCampaigns{campaigns: []*models.Campaign{activeCampaign("g1", "World of Tanks")}}
	m := newTestManager([]string{"World of Tanks"}, provider, &fakeClient{})

	top := onlineCandidate("big_streamer", "1", "World of Tanks", "g1", 9000)
	second := onlineCandidate("small_streamer", "2", "World of Tanks", "g1", 100)
	m.pool = []*Channel{top, second}

	got := m.selectBest(nil)
	if got != top {
		t.Fatalf("expected the first (most-viewed) candidate, got %+v", got)
	}
}

func TestSelectBestSkipsIneligibleCandidates(t *testing.T) {
	provider := &fakeCampaigns{campaigns: []*models.Campaign{activeCampaign("g1", "World of Tanks")}}
	client := &fakeClient{online: map[string]bool{}}
	m := newTestManager([]string{"World of Tanks"}, provider, client)

	offlineMarked := onlineCandidate("marked_offline", "1", "World of Tanks", "g1", 9000)
	offlineMarked.offline = true

	wrongGame := onlineCandidate("wrong_game", "2", "World of Tanks", "g1", 5000)
	wrongGame.Streamer.Stream.Update("b-2", "t", &models.Game{ID: "other", Name: "Other"}, nil, 5000)

	noCampaigns := onlineCandidate("no_campaigns", "3", "World of Tanks", "g1", 3000)
	noCampaigns.Streamer.Stream.CampaignIDs = nil

	inactiveGame := onlineCandidate("inactive_game", "4", "Dead Game", "dead", 2000)

	good := onlineCandidate("good_channel", "5", "World of Tanks", "g1", 1000)

	m.pool = []*Channel{offlineMarked, wrongGame, noCampaigns, inactiveGame, good}

	got := m.selectBest(nil)
	if got != good {
		var login string
		if got != nil {
			login = got.Streamer.Username
		}
		t.Fatalf("expected good_channel, got %q", login)
	}
}

func TestSelectBestVerifiesOfflineCandidatesAndMarksThem(t *testing.T) {
	provider := &fakeCampaigns{campaigns: []*models.Campaign{activeCampaign("g1", "World of Tanks")}}
	client := &fakeClient{online: map[string]bool{}}
	m := newTestManager([]string{"World of Tanks"}, provider, client)

	stale := onlineCandidate("stale_channel", "1", "World of Tanks", "g1", 9000)
	stale.Streamer.IsOnline = false // listed by the directory but needs verification

	m.pool = []*Channel{stale}

	if got := m.selectBest(nil); got != nil {
		t.Fatalf("expected no candidate when verification finds it offline, got %+v", got)
	}
	if len(client.checked) != 1 || client.checked[0] != "stale_channel" {
		t.Errorf("expected exactly one online check for stale_channel, got %v", client.checked)
	}
	if !stale.offline {
		t.Error("expected failed candidate to be marked offline so it is skipped until the next sync")
	}
}

func TestSelectBestBoundsChecksPerTick(t *testing.T) {
	provider := &fakeCampaigns{campaigns: []*models.Campaign{activeCampaign("g1", "World of Tanks")}}
	client := &fakeClient{online: map[string]bool{}}
	m := newTestManager([]string{"World of Tanks"}, provider, client)

	for i := 0; i < maxCandidateChecksPerTick+3; i++ {
		ch := onlineCandidate("candidate", string(rune('a'+i)), "World of Tanks", "g1", 100)
		ch.Streamer.IsOnline = false
		m.pool = append(m.pool, ch)
	}

	if got := m.selectBest(nil); got != nil {
		t.Fatalf("expected nil when all candidates offline, got %+v", got)
	}
	if len(client.checked) != maxCandidateChecksPerTick {
		t.Errorf("expected at most %d online checks per tick, got %d", maxCandidateChecksPerTick, len(client.checked))
	}
}

func TestSelectBestRequiresChannelLevelActiveCampaign(t *testing.T) {
	// The game stays "active" thanks to a new unclaimed campaign, but the
	// top-viewed channel only carries the recurring campaign the account has
	// already claimed. The slot must skip it and take the smaller channel
	// actually running the unclaimed campaign.
	claimed := activeCampaign("g1", "World of Tanks")
	claimed.ID = "camp-old"
	claimed.ClaimStatus = models.CampaignClaimStatusAlreadyClaimed

	fresh := activeCampaign("g1", "World of Tanks")
	fresh.ID = "camp-new"

	provider := &fakeCampaigns{campaigns: []*models.Campaign{claimed, fresh}}
	m := newTestManager([]string{"World of Tanks"}, provider, &fakeClient{})

	topButClaimed := onlineCandidate("top_channel", "1", "World of Tanks", "g1", 9000)
	topButClaimed.Streamer.Stream.CampaignIDs = []string{"camp-old"}

	smallButFresh := onlineCandidate("small_channel", "2", "World of Tanks", "g1", 100)
	smallButFresh.Streamer.Stream.CampaignIDs = []string{"camp-new"}

	m.pool = []*Channel{topButClaimed, smallButFresh}

	if got := m.selectBest(nil); got != smallButFresh {
		var login string
		if got != nil {
			login = got.Streamer.Username
		}
		t.Fatalf("expected small_channel (carries the unclaimed campaign), got %q", login)
	}
}

func TestChannelCarriesActiveCampaignHonorsChannelRestriction(t *testing.T) {
	restricted := activeCampaign("g1", "World of Tanks")
	restricted.ID = "camp-restricted"
	restricted.Channels = []string{"allowed-channel-id"}

	provider := &fakeCampaigns{campaigns: []*models.Campaign{restricted}}
	m := newTestManager([]string{"World of Tanks"}, provider, &fakeClient{})

	allowed := onlineCandidate("allowed_channel", "allowed-channel-id", "World of Tanks", "g1", 100)
	allowed.Streamer.Stream.CampaignIDs = []string{"camp-restricted"}
	if !m.channelCarriesActiveCampaign(allowed) {
		t.Error("expected allow-listed channel to qualify for the restricted campaign")
	}

	other := onlineCandidate("other_channel", "other-channel-id", "World of Tanks", "g1", 100)
	other.Streamer.Stream.CampaignIDs = []string{"camp-restricted"}
	if m.channelCarriesActiveCampaign(other) {
		t.Error("expected non-allow-listed channel to be rejected for a channel-restricted campaign")
	}
}

func TestSyncOnceExcludesTrackedStreamers(t *testing.T) {
	// Channels already on the configured streamer list belong to the
	// rotation; discovery must not double-watch them.
	provider := &fakeCampaigns{campaigns: []*models.Campaign{activeCampaign("g1", "World of Tanks")}}
	client := &fakeClient{streams: []api.DirectoryStream{
		{ChannelID: "1", Login: "tracked_streamer", Viewers: 9000, GameID: "g1", DropsEnabled: true},
		{ChannelID: "2", Login: "free_channel", Viewers: 100, GameID: "g1", DropsEnabled: true},
	}}
	m := newTestManager([]string{"World of Tanks"}, provider, client)
	m.tracked = &fakeTracked{names: []string{"tracked_streamer"}}

	m.syncOnce()

	if len(m.pool) != 1 || m.pool[0].Streamer.Username != "free_channel" {
		names := make([]string, len(m.pool))
		for i, ch := range m.pool {
			names[i] = ch.Streamer.Username
		}
		t.Fatalf("expected only free_channel in the pool, got %v", names)
	}
}

func TestSyncOnceTrackedOnlyKeepsOnlyTracked(t *testing.T) {
	// tracked_only inverts the syncOnce exclusion gate: the pool keeps ONLY
	// channels on the configured streamer list and drops everything else.
	provider := &fakeCampaigns{campaigns: []*models.Campaign{activeCampaign("g1", "World of Tanks")}}
	client := &fakeClient{streams: []api.DirectoryStream{
		{ChannelID: "1", Login: "tracked_streamer", Viewers: 9000, GameID: "g1", DropsEnabled: true},
		{ChannelID: "2", Login: "free_channel", Viewers: 100, GameID: "g1", DropsEnabled: true},
	}}
	m := newTrackedOnlyManager([]string{"World of Tanks"}, []string{"tracked_streamer"}, provider, client)

	m.syncOnce()

	if len(m.pool) != 1 || m.pool[0].Streamer.Username != "tracked_streamer" {
		names := make([]string, len(m.pool))
		for i, ch := range m.pool {
			names[i] = ch.Streamer.Username
		}
		t.Fatalf("expected only tracked_streamer in the tracked-only pool, got %v", names)
	}
}

func TestSelectBestTrackedOnlySkipsWatchedAndNonTracked(t *testing.T) {
	// tracked_only: a non-tracked candidate is never eligible, and a tracked one
	// the rotation already watches is skipped so discovery fills an idle slot
	// with a different tracked channel instead of duplicating the watch.
	provider := &fakeCampaigns{campaigns: []*models.Campaign{activeCampaign("g1", "World of Tanks")}}
	m := newTrackedOnlyManager([]string{"World of Tanks"},
		[]string{"watched_tracked", "idle_tracked"}, provider, &fakeClient{})
	// The rotation already watches watched_tracked (highest viewers).
	m.SetSlotStatus(&fakeSlotStatus{watching: map[string]bool{"watched_tracked": true}})

	watched := onlineCandidate("watched_tracked", "1", "World of Tanks", "g1", 9000)
	notTracked := onlineCandidate("not_tracked", "2", "World of Tanks", "g1", 5000)
	idle := onlineCandidate("idle_tracked", "3", "World of Tanks", "g1", 100)
	m.pool = []*Channel{watched, notTracked, idle}

	got := m.selectBest(nil)
	if got != idle {
		var login string
		if got != nil {
			login = got.Streamer.Username
		}
		t.Fatalf("expected idle_tracked (tracked, not already watched), got %q", login)
	}
}

func TestInvalidReasonTrackedOnly(t *testing.T) {
	provider := &fakeCampaigns{campaigns: []*models.Campaign{activeCampaign("g1", "World of Tanks")}}
	m := newTrackedOnlyManager([]string{"World of Tanks"},
		[]string{"tracked_chan", "rotation_chan", "self_chan"}, provider, &fakeClient{})
	m.SetSlotStatus(&fakeSlotStatus{
		watching: map[string]bool{"rotation_chan": true, "self_chan": true},
		origin:   map[string]string{"rotation_chan": "configured", "self_chan": "discovery"},
	})

	// Not on the configured list any more → yielded.
	notTracked := onlineCandidate("dropped_chan", "9", "World of Tanks", "g1", 100)
	if reason, invalid := m.invalidReason(notTracked); !invalid || !strings.Contains(reason, "no longer on the configured") {
		t.Fatalf("expected a de-tracked channel abandoned, got invalid=%v reason=%q", invalid, reason)
	}

	// Tracked and watched by the rotation itself → yielded (no duplicate watch).
	rotation := onlineCandidate("rotation_chan", "1", "World of Tanks", "g1", 100)
	if reason, invalid := m.invalidReason(rotation); !invalid || !strings.Contains(reason, "already watched by the rotation") {
		t.Fatalf("expected a rotation-held channel abandoned, got invalid=%v reason=%q", invalid, reason)
	}

	// Tracked and watched because the broker placed discovery's own proposal
	// (origin == discovery) → kept, so the slot does not flap.
	self := onlineCandidate("self_chan", "2", "World of Tanks", "g1", 100)
	if reason, invalid := m.invalidReason(self); invalid {
		t.Fatalf("expected discovery's own placed channel to stay valid, got reason=%q", reason)
	}

	// Tracked and not watched at all → valid.
	free := onlineCandidate("tracked_chan", "3", "World of Tanks", "g1", 100)
	if reason, invalid := m.invalidReason(free); invalid {
		t.Fatalf("expected an unwatched tracked channel to stay valid, got reason=%q", reason)
	}
}

func TestPrepareCurrentAbandonsChannelAddedToStreamerList(t *testing.T) {
	provider := &fakeCampaigns{campaigns: []*models.Campaign{activeCampaign("g1", "World of Tanks")}}
	m := newTestManager([]string{"World of Tanks"}, provider, &fakeClient{})

	promoted := onlineCandidate("promoted_channel", "1", "World of Tanks", "g1", 9000)
	backup := onlineCandidate("backup_channel", "2", "World of Tanks", "g1", 500)
	m.pool = []*Channel{promoted, backup}
	m.current = promoted

	// The user adds the proposed discovered channel to the streamer list.
	m.tracked = &fakeTracked{names: []string{"promoted_channel"}}

	got := m.prepareCurrent()

	if got != backup || m.current != backup {
		t.Fatalf("expected switch to backup_channel after promotion to the streamer list, got %+v", m.current)
	}
}

// staticAvoidChecker is a fixed avoid set for exercising the watchdog's
// channel-switch exclusion inside discovery.
type staticAvoidChecker struct{ avoided map[string]bool }

func (a *staticAvoidChecker) IsAvoided(login string) bool { return a.avoided[login] }

// TestDiscoveryHonorsAvoidList: the watchdog's channel-switch stage only works
// for discovery-held slots if discovery itself abandons an avoided current
// channel (invalidReason) and refuses to select an avoided candidate
// (selectBest) — otherwise it would keep proposing the excluded channel and
// no replacement would ever be picked.
func TestDiscoveryHonorsAvoidList(t *testing.T) {
	provider := &fakeCampaigns{campaigns: []*models.Campaign{activeCampaign("g1", "World of Tanks")}}
	m := newTestManager([]string{"World of Tanks"}, provider, &fakeClient{})
	avoid := &staticAvoidChecker{avoided: map[string]bool{"avoided_chan": true}}
	m.SetAvoidChecker(avoid)

	avoided := onlineCandidate("avoided_chan", "1", "World of Tanks", "g1", 500)
	if reason, invalid := m.invalidReason(avoided); !invalid || !strings.Contains(reason, "watchdog") {
		t.Fatalf("expected the avoided current channel to be abandoned, got invalid=%v reason=%q", invalid, reason)
	}

	backup := onlineCandidate("backup_chan", "2", "World of Tanks", "g1", 100)
	m.pool = []*Channel{avoided, backup}
	if got := m.selectBest(nil); got != backup {
		t.Fatalf("expected selectBest to skip the avoided channel and pick backup, got %+v", got)
	}

	// Exclusion lifted: the higher-viewer channel becomes selectable again.
	avoid.avoided = map[string]bool{}
	if got := m.selectBest(nil); got != avoided {
		t.Fatalf("expected the channel to be selectable after the exclusion lifts, got %+v", got)
	}
}

func TestInvalidReason(t *testing.T) {
	provider := &fakeCampaigns{campaigns: []*models.Campaign{activeCampaign("g1", "World of Tanks")}}
	m := newTestManager([]string{"World of Tanks"}, provider, &fakeClient{})

	healthy := onlineCandidate("healthy", "1", "World of Tanks", "g1", 100)
	if reason, invalid := m.invalidReason(healthy); invalid {
		t.Errorf("expected healthy channel to stay valid, got %q", reason)
	}

	offline := onlineCandidate("offline", "2", "World of Tanks", "g1", 100)
	offline.Streamer.IsOnline = false
	if _, invalid := m.invalidReason(offline); !invalid {
		t.Error("expected offline channel to be invalid")
	}

	switched := onlineCandidate("switched", "3", "World of Tanks", "g1", 100)
	switched.Streamer.Stream.Update("b", "t", &models.Game{ID: "other"}, nil, 1)
	if _, invalid := m.invalidReason(switched); !invalid {
		t.Error("expected channel that switched game to be invalid")
	}

	noDrops := onlineCandidate("nodrops", "4", "World of Tanks", "g1", 100)
	noDrops.Streamer.Stream.CampaignIDs = nil
	if _, invalid := m.invalidReason(noDrops); !invalid {
		t.Error("expected channel without available campaigns to be invalid")
	}

	deadGame := onlineCandidate("deadgame", "5", "Dead Game", "dead", 100)
	if reason, invalid := m.invalidReason(deadGame); !invalid {
		t.Error("expected channel of a game without active campaigns to be invalid")
	} else if reason == "" {
		t.Error("expected a human-readable reason")
	}
}

func TestPrepareCurrentSelectsBest(t *testing.T) {
	provider := &fakeCampaigns{campaigns: []*models.Campaign{activeCampaign("g1", "World of Tanks")}}
	m := newTestManager([]string{"World of Tanks"}, provider, &fakeClient{})

	best := onlineCandidate("best_channel", "1", "World of Tanks", "g1", 9000)
	m.pool = []*Channel{best}

	got := m.prepareCurrent()

	if got != best || m.current != best {
		t.Fatalf("expected best_channel selected as current, got %+v", m.current)
	}
}

// TestWatchCandidatesProposesCurrent verifies discovery proposes its current
// pick to the broker (as a watcher.Candidate) rather than watching it itself.
func TestWatchCandidatesProposesCurrent(t *testing.T) {
	provider := &fakeCampaigns{campaigns: []*models.Campaign{activeCampaign("g1", "World of Tanks")}}
	m := newTestManager([]string{"World of Tanks"}, provider, &fakeClient{})

	best := onlineCandidate("best_channel", "1", "World of Tanks", "g1", 9000)
	m.pool = []*Channel{best}

	cands := m.WatchCandidates()
	if len(cands) != 1 {
		t.Fatalf("expected exactly one proposed candidate, got %d", len(cands))
	}
	if cands[0].Streamer != best.Streamer {
		t.Errorf("expected the current pick proposed, got %q", cands[0].Streamer.Username)
	}
	if cands[0].Origin != "discovery" {
		t.Errorf("expected discovery origin, got %q", cands[0].Origin)
	}
}

func TestWatchCandidatesEmptyWhenNothingWatchable(t *testing.T) {
	// No campaigns => nothing to farm => no candidate proposed, and no send is
	// ever performed by discovery itself.
	m := newTestManager(nil, &fakeCampaigns{}, &fakeClient{})
	if cands := m.WatchCandidates(); cands != nil {
		t.Fatalf("expected no candidate when disabled, got %v", cands)
	}
}

func TestPrepareCurrentSwitchesWhenCurrentGoesOffline(t *testing.T) {
	provider := &fakeCampaigns{campaigns: []*models.Campaign{activeCampaign("g1", "World of Tanks")}}
	m := newTestManager([]string{"World of Tanks"}, provider, &fakeClient{})

	dying := onlineCandidate("dying_channel", "1", "World of Tanks", "g1", 9000)
	backup := onlineCandidate("backup_channel", "2", "World of Tanks", "g1", 500)
	m.pool = []*Channel{dying, backup}
	m.current = dying

	dying.Streamer.IsOnline = false

	got := m.prepareCurrent()

	if got != backup || m.current != backup {
		t.Fatalf("expected switch to backup_channel, got %+v", m.current)
	}
}

func TestPrepareCurrentClearsSlotWhenPoolExhausted(t *testing.T) {
	provider := &fakeCampaigns{campaigns: []*models.Campaign{activeCampaign("g1", "World of Tanks")}}
	m := newTestManager([]string{"World of Tanks"}, provider, &fakeClient{})

	only := onlineCandidate("only_channel", "1", "World of Tanks", "g1", 9000)
	m.pool = []*Channel{only}
	m.current = only
	only.Streamer.IsOnline = false

	if got := m.prepareCurrent(); got != nil || m.current != nil {
		t.Fatalf("expected the slot to empty out, got %+v", m.current)
	}
	select {
	case <-m.resync:
	default:
		t.Error("expected an immediate resync to be requested when the pool is exhausted")
	}
}

func TestPrepareCurrentExhaustedPoolRequestsEarlyResync(t *testing.T) {
	// All pool candidates verify offline after the last sync: preparation must
	// request an early directory re-query instead of waiting out the full
	// campaign-sync interval (60+ minutes) with a dead pool.
	provider := &fakeCampaigns{campaigns: []*models.Campaign{activeCampaign("g1", "World of Tanks")}}
	client := &fakeClient{online: map[string]bool{}}
	m := newTestManager([]string{"World of Tanks"}, provider, client)

	dead := onlineCandidate("dead_channel", "1", "World of Tanks", "g1", 100)
	dead.Streamer.IsOnline = false
	m.pool = []*Channel{dead}
	// lastSync is zero => far past the retry cadence, so the resync fires.

	if got := m.prepareCurrent(); got != nil || m.current != nil {
		t.Fatalf("expected no selection from a dead pool, got %+v", m.current)
	}
	select {
	case <-m.resync:
	default:
		t.Error("expected an early resync request when the pool is exhausted")
	}

	// With a fresh sync the same situation must NOT re-query early: the rate
	// limit keeps failed selections at the empty-pool cadence.
	m.lastSync = time.Now()
	m.prepareCurrent()
	select {
	case <-m.resync:
		t.Error("expected no resync request while the last sync is recent")
	default:
	}
}

func TestPrepareCurrentDoesNothingWhenDisabled(t *testing.T) {
	client := &fakeClient{}
	m := newTestManager(nil, &fakeCampaigns{}, client)

	if got := m.prepareCurrent(); got != nil {
		t.Errorf("expected nil from a disabled subsystem, got %+v", got)
	}
	if len(client.checked) != 0 {
		t.Error("expected a disabled subsystem to make no online checks")
	}
}

func TestPrepareCurrentSwitchesWhenCampaignExhausted(t *testing.T) {
	// The game's only campaign gets fully claimed between ticks: the slot must
	// abandon the channel even though it is still online.
	provider := &fakeCampaigns{campaigns: []*models.Campaign{activeCampaign("g1", "World of Tanks")}}
	m := newTestManager([]string{"World of Tanks"}, provider, &fakeClient{})

	current := onlineCandidate("current_channel", "1", "World of Tanks", "g1", 9000)
	m.pool = []*Channel{current}
	m.current = current

	provider.campaigns = nil // final reward claimed -> tracker drops the campaign

	if got := m.prepareCurrent(); got != nil || m.current != nil {
		t.Fatalf("expected the slot to clear once the game has no active campaign, got %+v", m.current)
	}
}

func TestPrepareCurrentAbandonsDeconfiguredGame(t *testing.T) {
	// The game is removed from settings while its campaign is still active:
	// discovery must not keep proposing a channel of a de-configured game.
	provider := &fakeCampaigns{campaigns: []*models.Campaign{
		activeCampaign("g1", "World of Tanks"),
		activeCampaign("g2", "Rust"),
	}}
	m := newTestManager([]string{"World of Tanks", "Rust"}, provider, &fakeClient{})

	wotChannel := onlineCandidate("wot_channel", "1", "World of Tanks", "g1", 9000)
	rustChannel := onlineCandidate("rust_channel", "2", "Rust", "g2", 100)
	m.pool = []*Channel{wotChannel, rustChannel}
	m.current = wotChannel

	m.UpdateSettings([]string{"Rust"}, config.DiscoveryModeAll, testRateLimits())
	<-m.resync // drain so the assertion below checks prepareCurrent, not UpdateSettings

	got := m.prepareCurrent()

	if got != rustChannel || m.current != rustChannel {
		t.Fatalf("expected switch to the still-configured game's channel, got %+v", m.current)
	}
}

func TestUpdateSettingsTriggersResync(t *testing.T) {
	m := newTestManager(nil, &fakeCampaigns{}, &fakeClient{})

	m.UpdateSettings([]string{"World of Tanks"}, config.DiscoveryModeAll, testRateLimits())

	if got := m.getGames(); len(got) != 1 || got[0] != "World of Tanks" {
		t.Errorf("expected games updated, got %v", got)
	}
	select {
	case <-m.resync:
	default:
		t.Error("expected UpdateSettings to request an immediate resync")
	}
}

func TestSyncOnceDisabledClearsPoolAndCurrent(t *testing.T) {
	provider := &fakeCampaigns{campaigns: []*models.Campaign{activeCampaign("g1", "World of Tanks")}}
	m := newTestManager([]string{"World of Tanks"}, provider, &fakeClient{})

	current := onlineCandidate("current_channel", "1", "World of Tanks", "g1", 9000)
	m.pool = []*Channel{current}
	m.current = current

	m.UpdateSettings(nil, config.DiscoveryModeAll, testRateLimits())
	m.syncOnce()

	if m.current != nil || len(m.pool) != 0 {
		t.Errorf("expected pool and slot cleared when discovery is disabled, got current=%v pool=%d", m.current, len(m.pool))
	}
}

func TestSyncOnceBuildsPoolSortedByViewers(t *testing.T) {
	provider := &fakeCampaigns{campaigns: []*models.Campaign{activeCampaign("g1", "World of Tanks")}}
	client := &fakeClient{streams: []api.DirectoryStream{
		{ChannelID: "2", Login: "mid_channel", Viewers: 500, GameID: "g1", DropsEnabled: true},
		{ChannelID: "1", Login: "big_channel", Viewers: 9000, GameID: "g1", DropsEnabled: true},
		// Entry without a login must be dropped.
		{ChannelID: "3", Login: "", Viewers: 100, GameID: "g1", DropsEnabled: true},
	}}

	m := newTestManager([]string{"World of Tanks"}, provider, client)

	m.syncOnce()

	if len(m.pool) != 2 {
		t.Fatalf("expected 2 candidates (entry without login dropped), got %d", len(m.pool))
	}
	if m.pool[0].Streamer.Username != "big_channel" || m.pool[1].Streamer.Username != "mid_channel" {
		t.Errorf("expected viewers-descending order, got [%s, %s]",
			m.pool[0].Streamer.Username, m.pool[1].Streamer.Username)
	}
	if m.pool[0].Game != "World of Tanks" {
		t.Errorf("expected candidates tagged with the configured game name, got %q", m.pool[0].Game)
	}
}

func TestSyncOnceKeepsExistingStreamerObjects(t *testing.T) {
	provider := &fakeCampaigns{campaigns: []*models.Campaign{activeCampaign("g1", "World of Tanks")}}
	client := &fakeClient{streams: []api.DirectoryStream{
		{ChannelID: "1", Login: "sticky_channel", Viewers: 100, GameID: "g1", DropsEnabled: true},
	}}
	m := newTestManager([]string{"World of Tanks"}, provider, client)

	m.syncOnce()
	first := m.pool[0].Streamer

	client.streams[0].Viewers = 200
	m.syncOnce()

	if m.pool[0].Streamer != first {
		t.Error("expected the ephemeral streamer object to be reused across syncs (it carries online/watch state)")
	}
	if m.pool[0].Viewers != 200 {
		t.Errorf("expected viewer count refreshed, got %d", m.pool[0].Viewers)
	}
}

func TestSyncOnceSkipsGamesWithoutActiveCampaign(t *testing.T) {
	provider := &fakeCampaigns{} // no campaigns at all
	client := &fakeClient{streams: []api.DirectoryStream{
		{ChannelID: "1", Login: "channel", Viewers: 100, GameID: "g1", DropsEnabled: true},
	}}
	m := newTestManager([]string{"World of Tanks"}, provider, client)

	interval := m.syncOnce()

	if len(m.pool) != 0 {
		t.Errorf("expected empty pool when no campaign is active for the game, got %d", len(m.pool))
	}
	if interval != emptyPoolRetryInterval {
		t.Errorf("expected the short empty-pool retry interval, got %v", interval)
	}
}

func TestStateSnapshot(t *testing.T) {
	provider := &fakeCampaigns{campaigns: []*models.Campaign{activeCampaign("g1", "World of Tanks")}}
	m := newTestManager([]string{"World of Tanks"}, provider, &fakeClient{})
	// The broker reports the current pick as actually holding a slot.
	m.SetSlotStatus(&fakeSlotStatus{watching: map[string]bool{"watching_channel": true}})

	watching := onlineCandidate("watching_channel", "1", "World of Tanks", "g1", 9000)
	available := onlineCandidate("available_channel", "2", "World of Tanks", "g1", 500)
	offline := onlineCandidate("offline_channel", "3", "World of Tanks", "g1", 100)
	offline.offline = true

	m.pool = []*Channel{watching, available, offline}
	m.current = watching
	m.lastSync = time.Now()

	st := m.State()
	if !st.Enabled {
		t.Error("expected Enabled with a configured game")
	}
	if st.Watching != "watching_channel" {
		t.Errorf("expected watching_channel, got %q", st.Watching)
	}
	if len(st.Channels) != 3 {
		t.Fatalf("expected 3 channels, got %d", len(st.Channels))
	}

	statuses := map[string]string{}
	for _, ch := range st.Channels {
		statuses[ch.Login] = ch.Status
	}
	if statuses["watching_channel"] != "watching" ||
		statuses["available_channel"] != "available" ||
		statuses["offline_channel"] != "offline" {
		t.Errorf("unexpected statuses: %v", statuses)
	}
}

// TestStateProposalNotWatchedShowsAvailable covers the new slot-broker
// semantics: discovery's current pick is only "watching" when the broker
// actually placed it in a slot; otherwise it is merely a waiting proposal.
func TestStateProposalNotWatchedShowsAvailable(t *testing.T) {
	provider := &fakeCampaigns{campaigns: []*models.Campaign{activeCampaign("g1", "World of Tanks")}}
	m := newTestManager([]string{"World of Tanks"}, provider, &fakeClient{})
	// Broker is keeping both slots on configured streamers: nothing discovered
	// is being watched.
	m.SetSlotStatus(&fakeSlotStatus{watching: map[string]bool{}})

	proposal := onlineCandidate("proposal_channel", "1", "World of Tanks", "g1", 9000)
	m.pool = []*Channel{proposal}
	m.current = proposal
	m.lastSync = time.Now()

	st := m.State()
	if st.Watching != "" {
		t.Errorf("expected no channel reported as watched, got %q", st.Watching)
	}
	if len(st.Channels) != 1 || st.Channels[0].Status != "available" {
		t.Errorf("expected the un-slotted proposal shown as available, got %+v", st.Channels)
	}
}

func TestStateDisabled(t *testing.T) {
	m := newTestManager(nil, &fakeCampaigns{}, &fakeClient{})
	if st := m.State(); st.Enabled {
		t.Error("expected Enabled=false with no configured games")
	}
}
