// Package discovery implements directory-based channel discovery: for each
// configured game it periodically lists live, drops-enabled channels from the
// Twitch directory (the same listing twitch.tv/directory shows), keeps them
// as a candidate pool sorted by viewer count, and proposes the best candidate
// to the unified slot broker.
//
// Discovery is a candidate SOURCE for the watcher's slot broker
// (internal/watcher), not an independent watch slot: it never sends
// minute-watched itself. It maintains a "current" best candidate (verifying it
// online and switching when it goes offline, changes game, loses its drops, or
// the game's last campaign is claimed) and hands it to the broker via
// WatchCandidates; the broker decides whether that candidate actually occupies
// one of the (at most constants.MaxSimultaneousStreams) Twitch watch slots,
// competing on equal footing with the configured streamer list. Discovered
// channels are ephemeral models.Streamer objects that never enter the streamer
// manager, the pubsub pool, chat, or the rotation's fairness store; they exist
// only to make drop-campaign progress for the configured games.
//
// The subsystem is fully disabled (no goroutine work, no API calls) while the
// configured game list is empty.
package discovery

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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
	CheckStreamerOnline(streamer *models.Streamer) models.StatusTransition
	GetDirectoryStreams(gameName string, limit int) ([]api.DirectoryStream, error)
}

// SlotStatus lets discovery ask the slot broker whether its proposed channel
// actually holds a watch slot, so the dashboard reports it as "watching" only
// when it really is (the broker may keep both slots on configured streamers).
// Satisfied by *watcher.MinuteWatcher.
type SlotStatus interface {
	IsWatching(login string) bool
	// WatchingOrigin returns the origin ("configured"/"discovery", i.e. one of
	// the watcher.Origin* values) of the slot currently holding login, or "" when
	// login is not being watched. In tracked-only mode it lets discovery tell a
	// channel the rotation already holds (origin != discovery — yield it, it is a
	// duplicate) from one the broker placed on discovery's own proposal (keep it),
	// which IsWatching alone cannot distinguish.
	WatchingOrigin(login string) string
}

// TrackedLoginsProvider exposes the logins of the configured streamer list so
// discovery never duplicates a channel the watch rotation already covers
// (double minute-watched reporting for one channel would both waste the slot
// and look anomalous). Satisfied by *streamer.Manager.
type TrackedLoginsProvider interface {
	Names() []string
}

// AvoidChecker reports whether the drop-progress watchdog has temporarily
// excluded a channel (its drop progress stalled there despite session
// recovery). Discovery must neither keep nor select such a channel — the
// exclusion is what makes the watchdog's channel-switch stage actually switch.
// Satisfied by *health.AvoidList; may be nil.
type AvoidChecker interface {
	IsAvoided(login string) bool
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

	// Subscribed marks that the authenticated account is subscribed to this
	// channel, per the periodic ChannelPointsContext multiplier probe (see
	// Manager.RefreshSubscribedSet). Consulted only when DiscoveryPreferSubscribed
	// is on; a stale-by-one-sync value is acceptable, like Viewers. It is a proxy
	// — a subscription grants an active points multiplier — the same signal the
	// SUBSCRIBED watch priority uses.
	Subscribed bool

	// offline marks a candidate that failed its online verification after
	// being listed; it is skipped until the next directory sync rebuilds the
	// pool.
	offline bool
}

// Manager owns the discovery candidate pool and the current best proposal.
type Manager struct {
	client     twitchAPI
	campaigns  CampaignsProvider
	tracked    TrackedLoginsProvider
	slotStatus SlotStatus
	avoid      AvoidChecker
	gameRanks  atomic.Pointer[map[string]int]
	subscribed atomic.Pointer[map[string]bool] // lock-free published subscribed set; nil = none
	settings   config.RateLimitSettings

	// mode selects candidacy: DiscoveryModeAll farms non-tracked directory
	// channels (the default), DiscoveryModeTrackedOnly inverts the exclusion
	// gates to farm only configured-list channels the rotation is not already
	// watching. Guarded by mu (written by UpdateSettings, read by the sync/watch
	// loops).
	mode config.DiscoveryMode

	// preferSubscribed floats a subscribed candidate above a non-subscribed one
	// in the candidate comparator (DiscoveryPreferSubscribed). Guarded by mu.
	preferSubscribed bool

	// subKnown accumulates the ChannelPointsContext probe results across ticks
	// (login -> subscribed), and subCursor rotates the bounded per-tick probe
	// across the whole pool. Both guarded by mu, driven by RefreshSubscribedSet.
	subKnown  map[string]bool
	subCursor int

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
	mode config.DiscoveryMode,
	preferSubscribed bool,
) *Manager {
	return &Manager{
		client:           client,
		campaigns:        campaigns,
		tracked:          tracked,
		settings:         settings,
		mode:             config.NormalizeDiscoveryMode(string(mode)),
		preferSubscribed: preferSubscribed,
		games:            games,
		resync:           make(chan struct{}, 1),
	}
}

// SetSlotStatus wires the slot broker so State() can report whether the
// proposed channel actually holds a watch slot. Call before Start.
func (m *Manager) SetSlotStatus(s SlotStatus) {
	m.mu.Lock()
	m.slotStatus = s
	m.mu.Unlock()
}

// SetAvoidChecker wires the progress watchdog's temporary channel exclusions.
// Call before Start. Safe for concurrent use.
func (m *Manager) SetAvoidChecker(a AvoidChecker) {
	m.mu.Lock()
	m.avoid = a
	m.mu.Unlock()
}

// SetGameRanks publishes the campaign-policy engine's cross-game ordering
// (keyed by lowercase game name; lower rank = higher priority) so the
// discovered-channel pool is built in policy order rather than the raw
// configured list order. nil (GAME_ORDER/disabled) preserves the configured
// order exactly. Lock-free for the reader; safe for concurrent use.
func (m *Manager) SetGameRanks(ranks map[string]int) {
	if ranks == nil {
		m.gameRanks.Store(nil)
		return
	}
	m.gameRanks.Store(&ranks)
}

// orderGamesByPolicy returns games reordered by the published policy ranks
// (stable; games absent from the map keep their configured relative order,
// sorted after ranked ones). With no ranks published it returns games
// unchanged, so the configured order is bit-identical.
func (m *Manager) orderGamesByPolicy(games []string) []string {
	ranksPtr := m.gameRanks.Load()
	if ranksPtr == nil {
		return games
	}
	ranks := *ranksPtr
	rank := func(g string) int {
		if r, ok := ranks[strings.ToLower(g)]; ok {
			return r
		}
		return 1 << 30 // unranked games sort last, keeping their relative order
	}
	ordered := make([]string, len(games))
	copy(ordered, games)
	sort.SliceStable(ordered, func(i, j int) bool {
		return rank(ordered[i]) < rank(ordered[j])
	})
	return ordered
}

// SetSubscribedLogins publishes the set of channel logins (lowercase) the
// authenticated account is subscribed to, so the candidate comparator can float
// subscribed channels when DiscoveryPreferSubscribed is on. nil clears it.
// Lock-free for the reader (mirrors SetGameRanks); safe for concurrent use.
func (m *Manager) SetSubscribedLogins(set map[string]bool) {
	if set == nil {
		m.subscribed.Store(nil)
		return
	}
	m.subscribed.Store(&set)
}

// subscribedLogins returns the current lock-free subscribed snapshot (may be
// nil). Callers must treat it as read-only — it is shared with SetSubscribedLogins.
func (m *Manager) subscribedLogins() map[string]bool {
	if p := m.subscribed.Load(); p != nil {
		return *p
	}
	return nil
}

// RefreshSubscribedSet probes a bounded, rotating slice of the candidate pool
// (plus the current pick) for subscription status using the injected probe — the
// miner backs it with a ChannelPointsContext ActiveMultipliers>0 check, the same
// proxy the SUBSCRIBED watch priority uses — accumulates the results, prunes them
// to the live pool, and republishes the lock-free subscribed set. It is a no-op
// that clears the set when the prefer-subscribed toggle is off, so no
// point-context calls are spent while the feature is disabled. Called on the
// miner's slow probe loop; mu is never held across probe() (a network call).
func (m *Manager) RefreshSubscribedSet(probe func(login string) bool) {
	m.mu.RLock()
	prefer := m.preferSubscribed
	m.mu.RUnlock()
	if !prefer {
		// Clear so a set captured while the toggle was on can't linger and keep
		// skewing the comparator after the operator opts out.
		m.SetSubscribedLogins(nil)
		return
	}

	// Snapshot the pool logins (+ the current pick) and pick this tick's bounded,
	// rotating probe targets under the lock; probe them without it.
	m.mu.Lock()
	poolLogins := make([]string, 0, len(m.pool))
	for _, ch := range m.pool {
		poolLogins = append(poolLogins, ch.Streamer.GetUsername())
	}
	current := ""
	if m.current != nil {
		current = m.current.Streamer.GetUsername()
	}
	targets := m.rotateProbeTargetsLocked(poolLogins, current)
	m.mu.Unlock()

	results := make(map[string]bool, len(targets))
	for _, login := range targets {
		results[login] = probe(login)
	}

	// Merge results into the accumulated known-set, prune entries whose channel
	// left the pool, and publish a copy of the still-subscribed logins.
	m.mu.Lock()
	if m.subKnown == nil {
		m.subKnown = make(map[string]bool)
	}
	for login, sub := range results {
		m.subKnown[login] = sub
	}
	live := make(map[string]bool, len(poolLogins)+1)
	for _, l := range poolLogins {
		live[l] = true
	}
	if current != "" {
		live[current] = true
	}
	set := make(map[string]bool)
	for login, sub := range m.subKnown {
		if !live[login] {
			delete(m.subKnown, login)
			continue
		}
		if sub {
			set[login] = true
		}
	}
	m.mu.Unlock()

	m.SetSubscribedLogins(set)
}

// rotateProbeTargetsLocked returns the current pick (if any) plus up to
// maxCandidateChecksPerTick pool logins, advancing subCursor so successive calls
// sweep the whole pool instead of re-probing only the top entries. Caller holds
// mu.
func (m *Manager) rotateProbeTargetsLocked(poolLogins []string, current string) []string {
	targets := make([]string, 0, maxCandidateChecksPerTick+1)
	seen := make(map[string]bool)
	if current != "" {
		targets = append(targets, current)
		seen[current] = true
	}
	n := len(poolLogins)
	for i := 0; i < n && len(targets) < maxCandidateChecksPerTick+1; i++ {
		login := poolLogins[(m.subCursor+i)%n]
		if seen[login] {
			continue
		}
		seen[login] = true
		targets = append(targets, login)
	}
	if n > 0 {
		m.subCursor = (m.subCursor + maxCandidateChecksPerTick) % n
	}
	return targets
}

// isAvoided reports whether the watchdog currently excludes the login.
func (m *Manager) isAvoided(login string) bool {
	m.mu.RLock()
	a := m.avoid
	m.mu.RUnlock()
	return a != nil && a.IsAvoided(login)
}

// SourceName identifies discovery as a candidate source to the slot broker.
func (m *Manager) SourceName() string { return "discovery" }

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

// trackedOnly reports whether discovery is restricted to the configured
// streamer list (DiscoveryModeTrackedOnly). The zero value is DiscoveryModeAll,
// so an unset mode preserves the original behavior.
func (m *Manager) trackedOnly() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.mode == config.DiscoveryModeTrackedOnly
}

// isWatchingSlot reports whether the slot broker currently watches login (by any
// source). Used by the tracked-only selection gate so discovery never proposes a
// channel that is already being watched — proposing it would only duplicate the
// watch and waste discovery's slot contribution.
func (m *Manager) isWatchingSlot(login string) bool {
	m.mu.RLock()
	s := m.slotStatus
	m.mu.RUnlock()
	return s != nil && s.IsWatching(login)
}

// watchedByRotation reports whether login holds a watch slot owned by a source
// OTHER than discovery (i.e. the configured rotation). This is the case a
// tracked-only current must yield: the rotation already covers it, so keeping it
// as discovery's proposal is redundant. A channel the broker placed on
// discovery's own proposal (origin == discovery) is deliberately excluded, so
// abandoning it here would only cause the slot to flap.
func (m *Manager) watchedByRotation(login string) bool {
	m.mu.RLock()
	s := m.slotStatus
	m.mu.RUnlock()
	if s == nil {
		return false
	}
	o := s.WatchingOrigin(login)
	return o != "" && o != watcher.OriginDiscovery
}

func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	m.ctx, m.cancel = context.WithCancel(ctx)
	m.mu.Unlock()

	// Only the directory sync loop runs here. The slot broker drives candidate
	// preparation (and the actual minute-watched reporting) on its own loop by
	// calling WatchCandidates, so discovery has no independent watch loop.
	go m.syncLoop()
}

func (m *Manager) Stop() {
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Unlock()
}

// UpdateSettings replaces the configured game list, discovery mode,
// prefer-subscribed toggle, and rate limits at runtime, e.g. from the Settings
// page, and triggers an immediate directory resync so changes apply without
// waiting out the current interval.
func (m *Manager) UpdateSettings(games []string, mode config.DiscoveryMode, preferSubscribed bool, settings config.RateLimitSettings) {
	m.mu.Lock()
	m.games = games
	m.mode = config.NormalizeDiscoveryMode(string(mode))
	m.preferSubscribed = preferSubscribed
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
	games := m.orderGamesByPolicy(m.getGames())

	m.mu.RLock()
	regularInterval := time.Duration(m.settings.CampaignSyncInterval) * time.Minute
	trackedOnly := m.mode == config.DiscoveryModeTrackedOnly
	preferSubscribed := m.preferSubscribed
	m.mu.RUnlock()

	subscribedSet := m.subscribedLogins() // lock-free snapshot, read before the pool-build lock

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
			// In "all" mode, channels on the configured streamer list are the
			// rotation's business — duplicating them here would double-report
			// watch minutes for the same channel and waste the discovery slot.
			// "tracked_only" inverts this: it keeps ONLY configured-list channels
			// and drops everything else from the pool.
			if trackedOnly {
				if !trackedSet[strings.ToLower(ds.Login)] {
					continue
				}
			} else if trackedSet[strings.ToLower(ds.Login)] {
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
			ch.Subscribed = subscribedSet[strings.ToLower(ds.Login)]
			ch.offline = false
			candidates = append(candidates, ch)
		}

		// Reference miners pick the most-viewed drops-enabled channel first;
		// VIEWER_COUNT sort should return this order already, but sorting
		// makes it explicit and independent of the requested sort mode.
		// Cross-game priority is the configured list order, preserved because
		// listings are processed in that order. When DiscoveryPreferSubscribed is
		// on, subscription is a tertiary key layered over viewers: a subscribed
		// channel floats above a non-subscribed one regardless of viewer count
		// (within a game; cross-game order still dominates via listing order).
		sort.SliceStable(candidates, func(i, j int) bool {
			ci, cj := candidates[i], candidates[j]
			if preferSubscribed && ci.Subscribed != cj.Subscribed {
				return ci.Subscribed
			}
			return ci.Viewers > cj.Viewers
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

	ids := ch.Streamer.Stream.GetCampaignIDs()
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
// StreamerFor returns the ephemeral streamer object of a discovered channel
// (current proposal or pool member), or nil when the login is unknown to
// discovery. The progress watchdog uses it to resolve the farming channel for
// read-only checks; all mutation still goes through the slot broker.
func (m *Manager) StreamerFor(login string) *models.Streamer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if ch := m.findExistingLocked(login); ch != nil {
		return ch.Streamer
	}
	return nil
}

func (m *Manager) findExistingLocked(login string) *Channel {
	for _, ch := range m.pool {
		if ch.Streamer.GetUsername() == login {
			return ch
		}
	}
	if m.current != nil && m.current.Streamer.GetUsername() == login {
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
		login = hadCurrent.Streamer.GetUsername()
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
// Candidate proposal (driven by the slot broker)

// WatchCandidates implements watcher.CandidateSource: it returns discovery's
// current best channel (if any) for the slot broker to consider, without ever
// sending minute-watched itself. It is only ever called from the broker's loop
// goroutine, so — like the old watch loop — it may do its own online
// verification with no lock held during the network calls; the sync loop and
// State() coordinate with it through mu exactly as before.
func (m *Manager) WatchCandidates() []watcher.Candidate {
	ch := m.prepareCurrent()
	if ch == nil {
		return nil
	}
	game, _, _, _ := m.channelFacts(ch)
	return []watcher.Candidate{{
		Streamer: ch.Streamer,
		Origin:   watcher.OriginDiscovery,
		Reason:   "best available drops-enabled channel for " + game,
	}}
}

// prepareCurrent selects and validates the current best discovered channel and
// returns it (or nil when nothing is watchable). It performs exactly the
// selection, stale re-verification, and auto-switching the old watch loop did,
// but never reports a watched minute — that is the slot broker's job once it
// places the channel in a slot. Runs on the broker's loop goroutine.
func (m *Manager) prepareCurrent() *Channel {
	games := m.getGames()
	if len(games) == 0 {
		return nil
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
			return nil
		}
		m.setCurrent(next)
		game, _, viewers, _ := m.channelFacts(next)
		slog.Info("Discovered channel selected",
			"channel", next.Streamer.GetUsername(),
			"game", game,
			"viewers", viewers,
			"reason", "best available drops-enabled channel by viewer count")
		events.Record(events.TypeDiscoverySelected, next.Streamer.GetUsername(),
			"directory candidate: "+game)
		current = next
	}

	// Mirror the watcher: re-verify a stream whose info has gone stale. This
	// also refreshes the channel's available campaign IDs and current game.
	if current.Streamer.Stream.UpdateElapsed() > staleStreamRecheck {
		m.client.CheckStreamerOnline(current.Streamer)
	}

	if reason, invalid := m.invalidReason(current); invalid {
		// Mark the channel offline only on an AUTHORITATIVE offline; an unknown
		// (transient check failure) must not be recorded as offline — the channel
		// is switched away from for whatever reason but stays re-checkable.
		if current.Streamer.GetStatus() == models.StatusOffline {
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
				"from", current.Streamer.GetUsername(), "to", "",
				"game", game, "reason", reason)
			m.logPoolEmpty(games)
			m.triggerResync()
			return nil
		}
		m.setCurrent(next)
		game, _, viewers, _ := m.channelFacts(next)
		slog.Info("Switching discovered channel",
			"from", current.Streamer.GetUsername(),
			"to", next.Streamer.GetUsername(),
			"game", game,
			"viewers", viewers,
			"reason", reason)
		events.Record(events.TypeDiscoverySwitched, next.Streamer.GetUsername(),
			"from "+current.Streamer.GetUsername()+": "+reason)
		current = next
	}

	// Keep the displayed viewer count fresh from the last online check even
	// though the broker, not discovery, does the actual minute-watched send.
	if v := current.Streamer.Stream.GetViewersCount(); v > 0 {
		m.mu.Lock()
		current.Viewers = v
		m.mu.Unlock()
	}

	return current
}

// invalidReason reports whether the currently watched discovered channel
// should be abandoned, and why. These are exactly the auto-switch conditions:
// removed from settings, offline, switched to a different game, lost its
// available drops, or the game's campaigns are exhausted (final reward
// claimed).
func (m *Manager) invalidReason(ch *Channel) (string, bool) {
	game, gameID, _, _ := m.channelFacts(ch)
	trackedOnly := m.trackedOnly()

	if !m.gameConfigured(game) {
		return "game removed from directory discovery settings", true
	}
	if trackedOnly {
		// tracked_only inverts the exclusion: a channel dropped from the
		// configured list is no longer eligible, and one the rotation is now
		// watching itself must be yielded so discovery does not duplicate the
		// watch (a channel the broker placed on discovery's own proposal keeps
		// origin == discovery and is left alone by watchedByRotation).
		if !m.isTracked(ch.Streamer.GetUsername()) {
			return "channel is no longer on the configured streamer list (tracked-only discovery)", true
		}
		if m.watchedByRotation(ch.Streamer.GetUsername()) {
			return "channel is already watched by the rotation (no duplicate watch)", true
		}
	} else if m.isTracked(ch.Streamer.GetUsername()) {
		return "channel is now on the configured streamer list (rotation covers it)", true
	}
	if ch.Streamer.GetStatus() == models.StatusOffline {
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
	if m.isAvoided(ch.Streamer.GetUsername()) {
		return "temporarily excluded by the drop-progress watchdog (stalled progress)", true
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

	trackedOnly := m.trackedOnly()
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
		if trackedOnly {
			// tracked_only: only configured-list channels are eligible, and never
			// one the rotation is already watching — proposing an already-watched
			// channel would just duplicate the watch instead of filling an idle
			// slot with a different tracked channel.
			if !m.isTracked(ch.Streamer.GetUsername()) {
				continue
			}
			if m.isWatchingSlot(ch.Streamer.GetUsername()) {
				continue
			}
		} else if m.isTracked(ch.Streamer.GetUsername()) {
			continue
		}
		if m.isAvoided(ch.Streamer.GetUsername()) {
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

		// A NEW discovery slot requires CONFIRMED online (fail closed): an
		// unverified candidate is skipped this tick and re-checked later, but only
		// an authoritative offline is recorded as offline (so a transient failure
		// doesn't permanently exclude a live channel until the next resync).
		if ch.Streamer.GetStatus() != models.StatusOnline {
			if ch.Streamer.GetStatus() == models.StatusOffline {
				m.mu.Lock()
				ch.offline = true
				m.mu.Unlock()
			}
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

	// A channel is "watching" only when the slot broker has actually placed the
	// current proposal in a watch slot — the broker may keep both slots on
	// configured streamers, in which case discovery's current is merely
	// waiting.
	watched := func(login string) bool {
		return m.slotStatus != nil && m.slotStatus.IsWatching(login)
	}
	if m.current != nil && watched(m.current.Streamer.GetUsername()) {
		st.Watching = m.current.Streamer.GetUsername()
	}

	seen := make(map[string]bool, len(m.pool)+1)
	appendChannel := func(ch *Channel) {
		if seen[ch.Streamer.GetUsername()] {
			return
		}
		seen[ch.Streamer.GetUsername()] = true

		cs := ChannelState{
			Login:   ch.Streamer.GetUsername(),
			Game:    ch.Game,
			Viewers: ch.Viewers,
		}
		switch {
		case m.current == ch && watched(ch.Streamer.GetUsername()):
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
