package discovery

import (
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

// fakeSender counts sends.
type fakeSender struct {
	sent []string
	err  error
}

func (f *fakeSender) Send(s *models.Streamer) (error, error) {
	f.sent = append(f.sent, s.Username)
	return nil, f.err
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

func newTestManager(games []string, campaigns *fakeCampaigns, client *fakeClient, sender *fakeSender) *Manager {
	m := NewManager(nil, campaigns, &fakeTracked{}, testRateLimits(), games)
	m.client = client
	m.sender = sender
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

	m := newTestManager([]string{"World of Tanks"}, provider, &fakeClient{}, &fakeSender{})

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
	m := newTestManager([]string{"World of Tanks"}, provider, &fakeClient{}, &fakeSender{})

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
	m := newTestManager([]string{"World of Tanks"}, provider, client, &fakeSender{})

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
	m := newTestManager([]string{"World of Tanks"}, provider, client, &fakeSender{})

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
	m := newTestManager([]string{"World of Tanks"}, provider, client, &fakeSender{})

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
	m := newTestManager([]string{"World of Tanks"}, provider, &fakeClient{}, &fakeSender{})

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
	m := newTestManager([]string{"World of Tanks"}, provider, &fakeClient{}, &fakeSender{})

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
	m := newTestManager([]string{"World of Tanks"}, provider, client, &fakeSender{})
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

func TestProcessWatchAbandonsChannelAddedToStreamerList(t *testing.T) {
	provider := &fakeCampaigns{campaigns: []*models.Campaign{activeCampaign("g1", "World of Tanks")}}
	sender := &fakeSender{}
	m := newTestManager([]string{"World of Tanks"}, provider, &fakeClient{}, sender)

	promoted := onlineCandidate("promoted_channel", "1", "World of Tanks", "g1", 9000)
	backup := onlineCandidate("backup_channel", "2", "World of Tanks", "g1", 500)
	m.pool = []*Channel{promoted, backup}
	m.current = promoted

	// The user adds the watched discovered channel to the streamer list.
	m.tracked = &fakeTracked{names: []string{"promoted_channel"}}

	m.processWatch()

	if m.current != backup {
		t.Fatalf("expected switch to backup_channel after promotion to the streamer list, got %+v", m.current)
	}
	if len(sender.sent) != 1 || sender.sent[0] != "backup_channel" {
		t.Errorf("expected the minute to go to backup_channel, got %v", sender.sent)
	}
}

func TestInvalidReason(t *testing.T) {
	provider := &fakeCampaigns{campaigns: []*models.Campaign{activeCampaign("g1", "World of Tanks")}}
	m := newTestManager([]string{"World of Tanks"}, provider, &fakeClient{}, &fakeSender{})

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

func TestProcessWatchSelectsAndSends(t *testing.T) {
	provider := &fakeCampaigns{campaigns: []*models.Campaign{activeCampaign("g1", "World of Tanks")}}
	sender := &fakeSender{}
	m := newTestManager([]string{"World of Tanks"}, provider, &fakeClient{}, sender)

	best := onlineCandidate("best_channel", "1", "World of Tanks", "g1", 9000)
	m.pool = []*Channel{best}

	m.processWatch()

	if m.current != best {
		t.Fatalf("expected best_channel selected as current, got %+v", m.current)
	}
	if len(sender.sent) != 1 || sender.sent[0] != "best_channel" {
		t.Errorf("expected one minute-watched send to best_channel, got %v", sender.sent)
	}
}

func TestProcessWatchSwitchesWhenCurrentGoesOffline(t *testing.T) {
	provider := &fakeCampaigns{campaigns: []*models.Campaign{activeCampaign("g1", "World of Tanks")}}
	sender := &fakeSender{}
	m := newTestManager([]string{"World of Tanks"}, provider, &fakeClient{}, sender)

	dying := onlineCandidate("dying_channel", "1", "World of Tanks", "g1", 9000)
	backup := onlineCandidate("backup_channel", "2", "World of Tanks", "g1", 500)
	m.pool = []*Channel{dying, backup}
	m.current = dying

	dying.Streamer.IsOnline = false

	m.processWatch()

	if m.current != backup {
		t.Fatalf("expected switch to backup_channel, got %+v", m.current)
	}
	if len(sender.sent) != 1 || sender.sent[0] != "backup_channel" {
		t.Errorf("expected the minute to go to backup_channel, got %v", sender.sent)
	}
}

func TestProcessWatchClearsSlotWhenPoolExhausted(t *testing.T) {
	provider := &fakeCampaigns{campaigns: []*models.Campaign{activeCampaign("g1", "World of Tanks")}}
	sender := &fakeSender{}
	m := newTestManager([]string{"World of Tanks"}, provider, &fakeClient{}, sender)

	only := onlineCandidate("only_channel", "1", "World of Tanks", "g1", 9000)
	m.pool = []*Channel{only}
	m.current = only
	only.Streamer.IsOnline = false

	m.processWatch()

	if m.current != nil {
		t.Fatalf("expected the slot to empty out, got %+v", m.current)
	}
	if len(sender.sent) != 0 {
		t.Errorf("expected no sends with an empty pool, got %v", sender.sent)
	}
	select {
	case <-m.resync:
	default:
		t.Error("expected an immediate resync to be requested when the pool is exhausted")
	}
}

func TestProcessWatchExhaustedPoolRequestsEarlyResync(t *testing.T) {
	// All pool candidates verify offline after the last sync: the watch loop
	// must request an early directory re-query instead of waiting out the
	// full campaign-sync interval (60+ minutes) with a dead pool.
	provider := &fakeCampaigns{campaigns: []*models.Campaign{activeCampaign("g1", "World of Tanks")}}
	client := &fakeClient{online: map[string]bool{}}
	m := newTestManager([]string{"World of Tanks"}, provider, client, &fakeSender{})

	dead := onlineCandidate("dead_channel", "1", "World of Tanks", "g1", 100)
	dead.Streamer.IsOnline = false
	m.pool = []*Channel{dead}
	// lastSync is zero => far past the retry cadence, so the resync fires.

	m.processWatch()

	if m.current != nil {
		t.Fatalf("expected no selection from a dead pool, got %+v", m.current)
	}
	select {
	case <-m.resync:
	default:
		t.Error("expected an early resync request when the pool is exhausted")
	}

	// With a fresh sync the same situation must NOT re-query early: the
	// rate limit keeps failed selections at the empty-pool cadence.
	m.lastSync = time.Now()
	m.processWatch()
	select {
	case <-m.resync:
		t.Error("expected no resync request while the last sync is recent")
	default:
	}
}

func TestProcessWatchDoesNothingWhenDisabled(t *testing.T) {
	sender := &fakeSender{}
	client := &fakeClient{}
	m := newTestManager(nil, &fakeCampaigns{}, client, sender)

	m.processWatch()

	if len(sender.sent) != 0 || len(client.checked) != 0 {
		t.Error("expected a disabled subsystem to make no calls at all")
	}
}

func TestProcessWatchSwitchesWhenCampaignExhausted(t *testing.T) {
	// The game's only campaign gets fully claimed between ticks: the slot
	// must abandon the channel even though it is still online.
	provider := &fakeCampaigns{campaigns: []*models.Campaign{activeCampaign("g1", "World of Tanks")}}
	sender := &fakeSender{}
	m := newTestManager([]string{"World of Tanks"}, provider, &fakeClient{}, sender)

	current := onlineCandidate("current_channel", "1", "World of Tanks", "g1", 9000)
	m.pool = []*Channel{current}
	m.current = current

	provider.campaigns = nil // final reward claimed -> tracker drops the campaign

	m.processWatch()

	if m.current != nil {
		t.Fatalf("expected the slot to clear once the game has no active campaign, got %+v", m.current)
	}
	if len(sender.sent) != 0 {
		t.Errorf("expected no minute sends after campaigns exhausted, got %v", sender.sent)
	}
}

func TestProcessWatchAbandonsDeconfiguredGame(t *testing.T) {
	// The game is removed from settings while its campaign is still active:
	// the slot must not keep watching a channel of a de-configured game.
	provider := &fakeCampaigns{campaigns: []*models.Campaign{
		activeCampaign("g1", "World of Tanks"),
		activeCampaign("g2", "Rust"),
	}}
	sender := &fakeSender{}
	m := newTestManager([]string{"World of Tanks", "Rust"}, provider, &fakeClient{}, sender)

	wotChannel := onlineCandidate("wot_channel", "1", "World of Tanks", "g1", 9000)
	rustChannel := onlineCandidate("rust_channel", "2", "Rust", "g2", 100)
	m.pool = []*Channel{wotChannel, rustChannel}
	m.current = wotChannel

	m.UpdateSettings([]string{"Rust"}, testRateLimits())
	<-m.resync // drain so the assertion below checks processWatch, not UpdateSettings

	m.processWatch()

	if m.current != rustChannel {
		t.Fatalf("expected switch to the still-configured game's channel, got %+v", m.current)
	}
	if len(sender.sent) != 1 || sender.sent[0] != "rust_channel" {
		t.Errorf("expected the minute to go to rust_channel, got %v", sender.sent)
	}
}

func TestUpdateSettingsTriggersResync(t *testing.T) {
	m := newTestManager(nil, &fakeCampaigns{}, &fakeClient{}, &fakeSender{})

	m.UpdateSettings([]string{"World of Tanks"}, testRateLimits())

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
	m := newTestManager([]string{"World of Tanks"}, provider, &fakeClient{}, &fakeSender{})

	current := onlineCandidate("current_channel", "1", "World of Tanks", "g1", 9000)
	m.pool = []*Channel{current}
	m.current = current

	m.UpdateSettings(nil, testRateLimits())
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

	m := newTestManager([]string{"World of Tanks"}, provider, client, &fakeSender{})

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
	m := newTestManager([]string{"World of Tanks"}, provider, client, &fakeSender{})

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
	m := newTestManager([]string{"World of Tanks"}, provider, client, &fakeSender{})

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
	m := newTestManager([]string{"World of Tanks"}, provider, &fakeClient{}, &fakeSender{})

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

func TestStateDisabled(t *testing.T) {
	m := newTestManager(nil, &fakeCampaigns{}, &fakeClient{}, &fakeSender{})
	if st := m.State(); st.Enabled {
		t.Error("expected Enabled=false with no configured games")
	}
}
