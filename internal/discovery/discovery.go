// Package discovery implements directory-based channel discovery: for each
// configured game it periodically lists live, drops-enabled channels from the
// Twitch directory (the same listing twitch.tv/directory shows), keeps them
// as a candidate pool sorted by viewer count, and farms the best candidate in
// ONE extra watch slot.
//
// This subsystem is deliberately independent from the fixed streamer list and
// its 2-slot watch rotation (internal/watcher): discovered channels are
// ephemeral models.Streamer objects that never enter the streamer manager,
// the pubsub pool, chat, or the rotation's fairness store. They exist only to
// make drop-campaign progress for the configured games. When the watched
// channel goes offline, switches game, loses its drops, or the game's last
// campaign is fully claimed, the slot automatically moves to the next-best
// candidate — mirroring the auto-channel-switching of reference drop miners.
//
// The subsystem is fully disabled (no goroutine work, no API calls) while the
// configured game list is empty.
package discovery

import (
	"context"
	"log/slog"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/watcher"
)

const (
	// directoryPageSize bounds how many live channels are requested per game
	// per directory sync. The pool only ever needs a handful of fallbacks
	// beyond the top channel, so a single page is plenty and keeps the extra
	// GQL load per sync at one request per configured game.
	directoryPageSize = 30

	// emptyPoolRetryInterval is how quickly the directory is re-queried while
	// the candidate pool is empty (no live drops-enabled channel right now, or
	// the drops tracker hasn't synced campaigns yet). Much shorter than the
	// regular campaign-sync cadence so the slot recovers quickly, while still
	// only costing one small GQL query per game per retry.
	emptyPoolRetryInterval = 2 * time.Minute

	// staleStreamRecheck mirrors the watcher's behavior of re-verifying a
	// watched stream that hasn't been refreshed in a while.
	staleStreamRecheck = 10 * time.Minute

	// maxCandidateChecksPerTick caps how many pool candidates are brought
	// online (spade URL + stream info fetches) in a single watch tick, so a
	// pool full of stale entries can't cause a burst of requests. Remaining
	// candidates are tried on subsequent ticks.
	maxCandidateChecksPerTick = 3
)

// CampaignsProvider exposes the drop campaigns currently tracked by the drops
// tracker (already filtered by date window, inventory, account-wide claim
// history, and the drop-name blacklist). Satisfied by *drops.DropsTracker.
type CampaignsProvider interface {
	Campaigns() []*models.Campaign
}

// twitchAPI is the slice of the Twitch client this subsystem needs; narrowed
// to an interface so tests can substitute a fake. Satisfied by
// *api.TwitchClient.
type twitchAPI interface {
	CheckStreamerOnline(streamer *models.Streamer)
	GetDirectoryStreams(gameName string, limit int) ([]api.DirectoryStream, error)
}

// minuteSender abstracts watcher.MinuteSender for tests.
type minuteSender interface {
	Send(streamer *models.Streamer) (simulateErr error, err error)
}

// TrackedLoginsProvider exposes the logins of the configured streamer list so
// discovery never duplicates a channel the watch rotation already covers
// (double minute-watched reporting for one channel would both waste the slot
// and look anomalous). Satisfied by *streamer.Manager.
type TrackedLoginsProvider interface {
	Names() []string
}

// Channel is one discovered directory candidate. All fields except Streamer
// are guarded by the owning Manager's mu — they are written by the sync loop
// and read by the watch loop and State() concurrently. Streamer has its own
// internal locking and is only driven from the watch loop.
type Channel struct {
	// Streamer is the ephemeral streamer object used for online checks and
	// minute-watched reporting. Never registered with the streamer manager.
	Streamer *models.Streamer

	// Game is the configured game name this channel was discovered under;
	// GameID is Twitch's ID for it, used to detect the channel switching
	// games and to match drop campaigns.
	Game   string
	GameID string

	// Viewers is the viewer count reported by the last directory sync (or by
	// the last stream refresh for the watched channel).
	Viewers int

	// DropsEnabled records that the directory listing returned this channel
	// under the DROPS_ENABLED filter.
	DropsEnabled bool

	// offline marks a candidate that failed its online verification after
	// being listed; it is skipped until the next directory sync rebuilds the
	// pool.
	offline bool
}

// Manager owns the discovery pool and the extra watch slot.
type Manager struct {
	client    twitchAPI
	sender    minuteSender
	campaigns CampaignsProvider
	tracked   TrackedLoginsProvider
	settings  config.RateLimitSettings

	games []string

	pool     []*Channel
	current  *Channel
	lastSync time.Time

	// emptyLogged makes the "pool is empty" INFO line fire once per
	// transition instead of every tick.
	emptyLogged bool

	resync chan struct{}

	ctx    context.Context
	cancel context.CancelFunc

	mu sync.RWMutex
}

func NewManager(
	client *api.TwitchClient,
	campaigns CampaignsProvider,
	tracked TrackedLoginsProvider,
	settings config.RateLimitSettings,
	games []string,
) *Manager {
	return &Manager{
		client:    client,
		sender:    watcher.NewMinuteSender(client),
		campaigns: campaigns,
		tracked:   tracked,
		settings:  settings,
		games:     games,
		resync:    make(chan struct{}, 1),
	}
}

// trackedLogins returns the configured streamer logins as a lowercase set.
func (m *Manager) trackedLogins() map[string]bool {
	if m.tracked == nil {
		return nil
	}
	names := m.tracked.Names()
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[strings.ToLower(n)] = true
	}
	return set
}

// isTracked reports whether the login is on the configured streamer list.
func (m *Manager) isTracked(login string) bool {
	if m.tracked == nil {
		return false
	}
	lower := strings.ToLower(login)
	for _, n := range m.tracked.Names() {
		if strings.ToLower(n) == lower {
			return true
		}
	}
	return false
}

func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	m.ctx, m.cancel = context.WithCancel(ctx)
	m.mu.Unlock()

	go m.syncLoop()
	go m.watchLoop()
}

func (m *Manager) Stop() {
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Unlock()
}

// UpdateSettings replaces the configured game list (and rate limits) at
// runtime, e.g. from the Settings page, and triggers an immediate directory
// resync so changes apply without waiting out the current interval.
func (m *Manager) UpdateSettings(games []string, settings config.RateLimitSettings) {
	m.mu.Lock()
	m.games = games
	m.settings = settings
	m.mu.Unlock()

	m.triggerResync()
}

func (m *Manager) getGames() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.games
}

// gameConfigured reports whether the given game name is still on the
// configured list (case-insensitive). A channel whose game was removed from
// the settings must be abandoned even if its campaigns are still active.
func (m *Manager) gameConfigured(game string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, g := range m.games {
		if strings.EqualFold(g, game) {
			return true
		}
	}
	return false
}

// channelFacts returns a consistent snapshot of the mutable Channel fields
// (they are written by the sync loop under mu).
func (m *Manager) channelFacts(ch *Channel) (game, gameID string, viewers int, offline bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return ch.Game, ch.GameID, ch.Viewers, ch.offline
}

// randomizedDelay applies the same ±20% jitter the minute watcher uses, so
// the extra slot's reporting cadence is as human-looking as the main one.
func randomizedDelay(base time.Duration) time.Duration {
	jitter := (rand.Float64() - 0.5) * 0.4
	return time.Duration(float64(base) * (1.0 + jitter))
}

// ---------------------------------------------------------------------------
// Directory sync loop

func (m *Manager) syncLoop() {
	for {
		interval := m.syncOnce()

		select {
		case <-m.ctx.Done():
			return
		case <-m.resync:
		case <-time.After(interval):
		}
	}
}

// syncOnce refreshes the candidate pool from the Twitch directory and returns
// how long to wait before the next sync. While the subsystem is disabled or
// the pool comes up empty it returns a short retry interval; otherwise the
// campaign-sync cadence applies.
func (m *Manager) syncOnce() time.Duration {
	games := m.getGames()

	m.mu.RLock()
	regularInterval := time.Duration(m.settings.CampaignSyncInterval) * time.Minute
	m.mu.RUnlock()

	if len(games) == 0 {
		m.clearPool()
		// Nothing to do until UpdateSettings triggers a resync; the long wait
		// costs nothing because the resync channel wakes the loop instantly.
		return regularInterval
	}

	gameCampaigns := m.activeCampaignGames()

	// Directory listings are fetched outside the lock (network); the pool is
	// then rebuilt and swapped in one locked pass, which is also where reused
	// Channel entries get their fields refreshed.
	type gameListing struct {
		game    string
		gameID  string
		streams []api.DirectoryStream
		err     error
	}
	var (
		listings      []gameListing
		inactiveGames []string
	)
	for _, game := range games {
		gameID, hasActive := gameCampaigns[strings.ToLower(game)]
		if !hasActive {
			inactiveGames = append(inactiveGames, game)
			continue
		}
		streams, err := m.client.GetDirectoryStreams(game, directoryPageSize)
		listings = append(listings, gameListing{game: game, gameID: gameID, streams: streams, err: err})
	}

	if len(inactiveGames) > 0 {
		slog.Debug("Directory discovery: games without an active unclaimed drop campaign are skipped",
			"games", inactiveGames)
	}

	trackedSet := m.trackedLogins()

	m.mu.Lock()
	var newPool []*Channel
	for _, l := range listings {
		if l.err != nil {
			// Keep the previous candidates for this game: a transient query
			// failure shouldn't drop a working pool.
			for _, ch := range m.pool {
				if ch.Game == l.game {
					newPool = append(newPool, ch)
				}
			}
			continue
		}

		candidates := make([]*Channel, 0, len(l.streams))
		for _, ds := range l.streams {
			if !ds.DropsEnabled || ds.Login == "" || ds.ChannelID == "" {
				continue
			}
			// Channels on the configured streamer list are the rotation's
			// business — duplicating them here would double-report watch
			// minutes for the same channel and waste the discovery slot.
			if trackedSet[strings.ToLower(ds.Login)] {
				continue
			}
			ch := m.findExistingLocked(ds.Login)
			if ch == nil {
				ch = &Channel{Streamer: newEphemeralStreamer(ds.Login, ds.ChannelID)}
			}
			ch.Game = l.game
			ch.GameID = ds.GameID
			if ch.GameID == "" {
				ch.GameID = l.gameID
			}
			ch.Viewers = ds.Viewers
			ch.DropsEnabled = true
			ch.offline = false
			candidates = append(candidates, ch)
		}

		// Reference miners pick the most-viewed drops-enabled channel first;
		// VIEWER_COUNT sort should return this order already, but sorting
		// makes it explicit and independent of the requested sort mode.
		// Cross-game priority is the configured list order, preserved because
		// listings are processed in that order.
		sort.SliceStable(candidates, func(i, j int) bool {
			return candidates[i].Viewers > candidates[j].Viewers
		})

		newPool = append(newPool, candidates...)
	}

	m.pool = newPool
	m.lastSync = time.Now()
	poolEmpty := len(newPool) == 0
	if !poolEmpty {
		m.emptyLogged = false
	}
	m.mu.Unlock()

	for _, l := range listings {
		if l.err != nil {
			slog.Warn("Directory discovery: failed to query game directory, keeping previous candidates",
				"game", l.game, "error", l.err)
		}
	}

	slog.Debug("Directory discovery: pool refreshed",
		"games", games, "candidates", len(newPool))

	if poolEmpty {
		m.logPoolEmpty(games)
		return emptyPoolRetryInterval
	}

	return regularInterval
}

// activeCampaignGames returns the set of games (keyed by lowercase name) that
// still have at least one active, unclaimed drop campaign, mapped to the
// game's Twitch ID. A game disappearing from this set is what makes the slot
// move on after the final reward of its last campaign is claimed.
func (m *Manager) activeCampaignGames() map[string]string {
	result := make(map[string]string)
	if m.campaigns == nil {
		return result
	}
	for _, c := range m.campaigns.Campaigns() {
		if c.Game == nil || len(c.Drops) == 0 || c.ClaimStatus == models.CampaignClaimStatusAlreadyClaimed {
			continue
		}
		for _, name := range []string{c.Game.Name, c.Game.DisplayName} {
			if name != "" {
				result[strings.ToLower(name)] = c.Game.ID
			}
		}
	}
	return result
}

// gameStillActive reports whether the given game ID still has an active
// unclaimed campaign tracked by the drops tracker.
func (m *Manager) gameStillActive(gameID string) bool {
	if m.campaigns == nil || gameID == "" {
		return false
	}
	for _, c := range m.campaigns.Campaigns() {
		if c.Game == nil || c.Game.ID != gameID {
			continue
		}
		if len(c.Drops) > 0 && c.ClaimStatus != models.CampaignClaimStatusAlreadyClaimed {
			return true
		}
	}
	return false
}

// channelCarriesActiveCampaign reports whether at least one of the campaigns
// Twitch lists as available on this channel (Stream.CampaignIDs) is a
// tracker-active one: still unclaimed per the account's claim history, not
// blacklisted (those never reach Campaigns()), and — for channel-restricted
// campaigns — actually allowing this channel. This mirrors the intersection
// updateStreamerCampaigns performs for tracked streamers; without it a
// top-viewed channel carrying only an already-claimed recurring campaign
// would hold the slot forever while a smaller channel runs the new one.
func (m *Manager) channelCarriesActiveCampaign(ch *Channel) bool {
	if m.campaigns == nil {
		return false
	}

	ids := ch.Streamer.Stream.CampaignIDs
	if len(ids) == 0 {
		return false
	}
	idSet := make(map[string]bool, len(ids))
	for _, id := range ids {
		idSet[id] = true
	}

	_, gameID, _, _ := m.channelFacts(ch)
	for _, c := range m.campaigns.Campaigns() {
		if c.Game == nil || c.Game.ID != gameID {
			continue
		}
		if len(c.Drops) == 0 || c.ClaimStatus == models.CampaignClaimStatusAlreadyClaimed {
			continue
		}
		if !idSet[c.ID] {
			continue
		}
		if c.IsChannelRestricted() && !c.AllowsChannel(ch.Streamer.ChannelID) {
			continue
		}
		return true
	}
	return false
}

// findExistingLocked looks up a pool/current entry by login so its ephemeral
// Streamer (carrying online state and the watch payload) survives pool
// rebuilds. Caller must hold mu.
func (m *Manager) findExistingLocked(login string) *Channel {
	for _, ch := range m.pool {
		if ch.Streamer.Username == login {
			return ch
		}
	}
	if m.current != nil && m.current.Streamer.Username == login {
		return m.current
	}
	return nil
}

func (m *Manager) clearPool() {
	m.mu.Lock()
	hadCurrent := m.current
	var game, login string
	if hadCurrent != nil {
		game = hadCurrent.Game
		login = hadCurrent.Streamer.Username
	}
	m.pool = nil
	m.current = nil
	m.emptyLogged = false
	m.mu.Unlock()

	if hadCurrent != nil {
		slog.Info("Switching discovered channel",
			"from", login, "to", "",
			"game", game,
			"reason", "directory discovery disabled (game list empty)")
	}
}

// newEphemeralStreamer builds the throwaway streamer object for a discovered
// channel. Only ClaimDrops matters: it makes UpdateStream fetch the channel's
// available drop-campaign IDs, exactly like it does for tracked streamers.
// Everything else (bets, raids, moments, chat, streaks) stays off — those
// behaviors belong to the configured streamer list, not the discovery slot.
func newEphemeralStreamer(login, channelID string) *models.Streamer {
	s := models.NewStreamer(login, models.StreamerSettings{
		ClaimDrops: true,
		Chat:       models.ChatNever,
	})
	s.ChannelID = channelID
	return s
}

// ---------------------------------------------------------------------------
// Watch loop (the extra slot)

func (m *Manager) watchLoop() {
	for {
		select {
		case <-m.ctx.Done():
			return
		default:
		}

		m.processWatch()

		m.mu.RLock()
		interval := time.Duration(m.settings.MinuteWatchedInterval) * time.Second
		m.mu.RUnlock()

		select {
		case <-m.ctx.Done():
			return
		case <-time.After(randomizedDelay(interval)):
		}
	}
}

func (m *Manager) processWatch() {
	games := m.getGames()
	if len(games) == 0 {
		return
	}

	m.mu.RLock()
	current := m.current
	m.mu.RUnlock()

	if current == nil {
		next := m.selectBest(nil)
		if next == nil {
			m.logPoolEmpty(games)
			// The pool exists but produced nothing watchable (candidates
			// verified offline/ineligible since the last directory sync) —
			// ask for an early re-query instead of waiting out the full
			// campaign-sync interval. Rate-limited inside maybeResync.
			m.maybeResync()
			return
		}
		m.setCurrent(next)
		game, _, viewers, _ := m.channelFacts(next)
		slog.Info("Discovered channel selected",
			"channel", next.Streamer.Username,
			"game", game,
			"viewers", viewers,
			"reason", "best available drops-enabled channel by viewer count")
		events.Record(events.TypeDiscoverySelected, next.Streamer.Username,
			"directory slot: "+game)
		current = next
	}

	// Mirror the watcher: re-verify a stream whose info has gone stale. This
	// also refreshes the channel's available campaign IDs and current game.
	if current.Streamer.Stream.UpdateElapsed() > staleStreamRecheck {
		m.client.CheckStreamerOnline(current.Streamer)
	}

	if reason, invalid := m.invalidReason(current); invalid {
		if !current.Streamer.GetIsOnline() {
			m.mu.Lock()
			current.offline = true
			m.mu.Unlock()
		}

		next := m.selectBest(current)
		if next == nil {
			m.mu.Lock()
			m.current = nil
			m.mu.Unlock()
			game, _, _, _ := m.channelFacts(current)
			slog.Info("Switching discovered channel",
				"from", current.Streamer.Username, "to", "",
				"game", game, "reason", reason)
			m.logPoolEmpty(games)
			m.triggerResync()
			return
		}
		m.setCurrent(next)
		game, _, viewers, _ := m.channelFacts(next)
		slog.Info("Switching discovered channel",
			"from", current.Streamer.Username,
			"to", next.Streamer.Username,
			"game", game,
			"viewers", viewers,
			"reason", reason)
		events.Record(events.TypeDiscoverySwitched, next.Streamer.Username,
			"from "+current.Streamer.Username+": "+reason)
		current = next
	}

	simulateErr, err := m.sender.Send(current.Streamer)
	if simulateErr != nil {
		slog.Debug("Discovery: failed to simulate watching",
			"channel", current.Streamer.Username, "error", simulateErr)
	}
	if err != nil {
		slog.Debug("Discovery: failed to send minute watched",
			"channel", current.Streamer.Username, "error", err)
		// A failed send usually means the stream just ended; re-check now so
		// the next tick switches instead of failing again.
		m.client.CheckStreamerOnline(current.Streamer)
		return
	}

	m.mu.RLock()
	maxContinuousGap := 2 * time.Duration(m.settings.MinuteWatchedInterval) * time.Second
	m.mu.RUnlock()
	current.Streamer.Stream.UpdateMinuteWatched(maxContinuousGap)
	if v := current.Streamer.Stream.GetViewersCount(); v > 0 {
		m.mu.Lock()
		current.Viewers = v
		m.mu.Unlock()
	}
	slog.Debug("Discovery: sent minute watched",
		"channel", current.Streamer.Username,
		"minutesWatched", current.Streamer.Stream.GetMinuteWatched())
}

// invalidReason reports whether the currently watched discovered channel
// should be abandoned, and why. These are exactly the auto-switch conditions:
// removed from settings, offline, switched to a different game, lost its
// available drops, or the game's campaigns are exhausted (final reward
// claimed).
func (m *Manager) invalidReason(ch *Channel) (string, bool) {
	game, gameID, _, _ := m.channelFacts(ch)

	if !m.gameConfigured(game) {
		return "game removed from directory discovery settings", true
	}
	if m.isTracked(ch.Streamer.Username) {
		return "channel is now on the configured streamer list (rotation covers it)", true
	}
	if !ch.Streamer.GetIsOnline() {
		return "channel went offline", true
	}
	if !m.gameStillActive(gameID) {
		return "no active unclaimed drop campaign remains for this game", true
	}
	if gid := ch.Streamer.Stream.GameID(); gid != "" && gid != gameID {
		return "channel switched to a different game", true
	}
	if !m.channelCarriesActiveCampaign(ch) {
		return "channel no longer carries an active unclaimed drop campaign", true
	}
	return "", false
}

// selectBest returns the best watchable candidate from the pool: candidates
// are already ordered by configured game order then viewer count, so the
// first one that verifies as online, still on its game, and carrying an
// available drop campaign wins. exclude (may be nil) is the channel being
// switched away from. At most maxCandidateChecksPerTick candidates are
// brought online per call to bound the API burst; the rest wait for the next
// tick.
func (m *Manager) selectBest(exclude *Channel) *Channel {
	m.mu.RLock()
	pool := make([]*Channel, len(m.pool))
	copy(pool, m.pool)
	m.mu.RUnlock()

	checks := 0
	for _, ch := range pool {
		if ch == exclude {
			continue
		}
		game, gameID, _, offline := m.channelFacts(ch)
		if offline {
			continue
		}
		// The pool may briefly hold channels of a just-removed game or a
		// just-added tracked streamer until the triggered resync rebuilds it;
		// never select those.
		if !m.gameConfigured(game) {
			continue
		}
		if m.isTracked(ch.Streamer.Username) {
			continue
		}
		if !m.gameStillActive(gameID) {
			continue
		}

		// Refresh candidates that were never brought online as well as ones
		// whose stream data has gone stale — a channel disqualified by a
		// transient failure (e.g. an empty CampaignIDs fetch) must be
		// re-checked eventually, not skipped forever while it stays online.
		online := ch.Streamer.GetIsOnline()
		stale := online && ch.Streamer.Stream.UpdateElapsed() > staleStreamRecheck
		if !online || stale {
			if checks >= maxCandidateChecksPerTick {
				return nil
			}
			checks++
			m.client.CheckStreamerOnline(ch.Streamer)
		}

		if !ch.Streamer.GetIsOnline() {
			m.mu.Lock()
			ch.offline = true
			m.mu.Unlock()
			continue
		}
		if gid := ch.Streamer.Stream.GameID(); gid != "" && gid != gameID {
			continue
		}
		if !m.channelCarriesActiveCampaign(ch) {
			continue
		}

		if v := ch.Streamer.Stream.GetViewersCount(); v > 0 {
			m.mu.Lock()
			ch.Viewers = v
			m.mu.Unlock()
		}
		return ch
	}
	return nil
}

func (m *Manager) setCurrent(ch *Channel) {
	m.mu.Lock()
	m.current = ch
	m.emptyLogged = false
	m.mu.Unlock()
}

func (m *Manager) logPoolEmpty(games []string) {
	m.mu.Lock()
	already := m.emptyLogged
	m.emptyLogged = true
	m.mu.Unlock()

	if already {
		return
	}
	slog.Info("Discovery pool empty: no live drops-enabled channel available right now",
		"games", games)
}

func (m *Manager) triggerResync() {
	select {
	case m.resync <- struct{}{}:
	default:
	}
}

// maybeResync requests an early directory re-query when the pool cannot
// produce a watchable channel, rate-limited to the empty-pool retry cadence
// so repeated failed selections never query the directory more often than a
// genuinely empty pool would.
func (m *Manager) maybeResync() {
	m.mu.RLock()
	last := m.lastSync
	m.mu.RUnlock()

	if time.Since(last) >= emptyPoolRetryInterval {
		m.triggerResync()
	}
}

// ---------------------------------------------------------------------------
// State snapshot (web UI + debug endpoint)

// ChannelState is a read-only view of one pool candidate.
type ChannelState struct {
	Login   string
	Game    string
	Viewers int
	// Status is "watching", "available", or "offline".
	Status string
	// MinutesWatched is only populated for the currently watched channel.
	MinutesWatched float64
}

// State is a read-only snapshot of the discovery subsystem.
type State struct {
	Enabled  bool
	Games    []string
	Channels []ChannelState
	Watching string
	LastSync time.Time
}

// State returns a snapshot of the subsystem for the dashboard and the debug
// endpoint. Safe to call from any goroutine.
func (m *Manager) State() State {
	m.mu.RLock()
	defer m.mu.RUnlock()

	st := State{
		Enabled:  len(m.games) > 0,
		Games:    append([]string(nil), m.games...),
		LastSync: m.lastSync,
	}

	if m.current != nil {
		st.Watching = m.current.Streamer.Username
	}

	seen := make(map[string]bool, len(m.pool)+1)
	appendChannel := func(ch *Channel) {
		if seen[ch.Streamer.Username] {
			return
		}
		seen[ch.Streamer.Username] = true

		cs := ChannelState{
			Login:   ch.Streamer.Username,
			Game:    ch.Game,
			Viewers: ch.Viewers,
		}
		switch {
		case m.current == ch:
			cs.Status = "watching"
			cs.MinutesWatched = ch.Streamer.Stream.GetMinuteWatched()
		case ch.offline:
			cs.Status = "offline"
		default:
			cs.Status = "available"
		}
		st.Channels = append(st.Channels, cs)
	}

	if m.current != nil {
		appendChannel(m.current)
	}
	for _, ch := range m.pool {
		appendChannel(ch)
	}

	return st
}
