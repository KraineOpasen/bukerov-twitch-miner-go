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

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/drops"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/eligibility"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/policy"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/twitch"
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

	// maxCandidateChecksPerTick caps how many pool candidates are brought
	// online (spade URL + stream info fetches) in a single watch tick, so a
	// pool full of stale entries can't cause a burst of requests. Remaining
	// candidates are tried on subsequent ticks.
	maxCandidateChecksPerTick = 3
)

// staleStreamRecheck mirrors the watcher's behavior of re-verifying a watched
// stream that hasn't been refreshed in a while. It is a package variable only
// so focused tests can advance this gate without a ten-minute sleep.
var staleStreamRecheck = 10 * time.Minute

// CampaignsProvider exposes the drop campaigns currently tracked by the drops
// tracker (already filtered by date window, inventory, account-wide claim
// history, and the drop-name blacklist). Satisfied by *drops.DropsTracker.
type CampaignsProvider interface {
	Campaigns() []*models.Campaign
}

// brokerCampaignsProvider is an optional, broader account-campaign view used
// only by the lower-authority provisional observation path. The ordinary
// Known-availability path continues to consume Campaigns() exactly as before.
// Focused fakes and older providers intentionally fall back to Campaigns().
type brokerCampaignsProvider interface {
	BrokerCampaignSnapshot() drops.BrokerCampaignSnapshot
}

// twitchAPI is the slice of the Twitch client this subsystem needs; narrowed
// to an interface so tests can substitute a fake. Satisfied by
// *twitch.TwitchClient.
type twitchAPI interface {
	// CheckStreamerOnlineContext is the context-owned form: candidate
	// re-verification happens inside WatchCandidates, which the slot broker
	// calls ON its own loop goroutine, so those checks belong to the watch
	// generation and must stop when it does.
	CheckStreamerOnlineContext(ctx context.Context, streamer *models.Streamer) models.StatusTransition
	GetDirectoryStreams(gameName string, limit int) ([]twitch.DirectoryStream, error)
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

// candidatePolicyPublisher is the optional same-tick semantic handoff exposed
// by the Unified Slot Broker. Manager already receives that broker through
// SetSlotStatus, so no second owner or wiring path is introduced.
type candidatePolicyPublisher interface {
	SetDiscoveryCandidatePolicy(string, watcher.CandidateCampaignPolicy)
}

// campaignPolicyProvider exposes the Unified Slot Broker's one immutable
// semantic publication. In production the same watcher snapshot therefore
// drives discovery's proposal and the final broker comparison; the local
// SetCampaignPolicy snapshot remains the rank-only/test compatibility fallback.
type campaignPolicyProvider interface {
	DiscoveryCampaignPolicy() (map[string]int, map[string]policy.CampaignSemantic)
}

// provisionalProofProvider is the broker's optional exact-tuple proof query.
// Its absence is fail-closed: an UNKNOWN proposal keeps fill-only authority.
// A true result changes only discovery's local comparison for continuity; the
// candidate remains explicitly provisional so the broker must independently
// consume and validate its proof.
type provisionalProofProvider interface {
	HasProvisionalProof(*models.Streamer, models.ProvisionalDropCandidate) bool
}

// provisionalQuarantineProvider exposes only the broker's exact narrow
// channel/drop/session negative. Discovery consults it inside the per-campaign
// loop, so a quarantined best tuple does not hide another eligible Drop on the
// same channel (or the next eligible channel) for an extra broker cycle.
type provisionalQuarantineProvider interface {
	IsProvisionalQuarantined(*models.Streamer, models.ProvisionalDropCandidate) bool
}

// provisionalQuarantineReconciler is the broker's sole mutation boundary for
// negative lifecycle state. Discovery supplies a strictly newer complete scope
// only after both its Directory enumeration and broker campaign snapshot have
// passed their end-of-build fences.
type provisionalQuarantineReconciler interface {
	RequireProvisionalScope()
	ReconcileProvisionalQuarantine(
		uint64,
		watcher.ProvisionalDirectoryAuthority,
		[]watcher.ProvisionalAccountWork,
		[]watcher.ProvisionalQuarantineOwnerScope,
	) bool
}

// routineRefreshRunner linearizes routine metadata refresh with provisional
// lease admission under the broker's observation ownership lock. A standalone
// Manager without a broker retains the historical direct-refresh fallback.
type routineRefreshRunner interface {
	RunRoutineRefresh(*models.Streamer, func()) bool
}

type provisionalCandidateOwner interface {
	OwnsProvisionalCandidate(*models.Streamer, models.ProvisionalDropCandidate) bool
}

// TrackedLoginsProvider exposes the logins of the configured streamer list so
// discovery never duplicates a channel the watch rotation already covers
// (double minute-watched reporting for one channel would both waste the slot
// and look anomalous). Satisfied by *streamer.Manager.
type TrackedLoginsProvider interface {
	Names() []string
}

// trackedStreamersProvider is the optional identity-bearing extension supplied
// by *streamer.Manager. Names remains the ordinary directory exclusion API;
// All is consulted only by the lower-authority provisional path so a configured
// login can be proposed with its exact broker-owned *models.Streamer object.
// A provider that exposes only Names keeps the historical behavior unchanged.
type trackedStreamersProvider interface {
	All() []*models.Streamer
}

// configuredDirectoryEvidence is a provisional-only copy of one successful
// DROPS_ENABLED Directory row for a configured login. Configured rows remain
// excluded from the ordinary discovery pool in the default mode; retaining the
// row here supplies only the independent evidence required by an unrestricted
// UNKNOWN bootstrap. Restricted campaigns rely on their exact complete ACL and
// do not require a Directory row.
type configuredDirectoryEvidence struct {
	game          string
	gameID        string
	channelID     string
	dropsEnabled  bool
	observationID uint64
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

	// directoryGameID is the game ID carried by the successful Directory row
	// itself. It is intentionally distinct from GameID, whose legacy fallback to
	// the account campaign's game ID remains useful to the ordinary discovery
	// path but is not independent evidence for a provisional observation.
	directoryGameID string

	// directoryObservation is the Manager generation in which this exact channel
	// was returned by a successful DROPS_ENABLED Directory listing. A retained
	// row from an errored listing keeps its old generation and therefore cannot
	// bootstrap an unrestricted campaign.
	directoryObservation uint64

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
	// campaignPolicy is the immutable compatibility/fallback publication used
	// when no Unified Slot Broker policy provider is wired (mainly focused tests).
	// Production reads the broker's one active snapshot through slotStatus so
	// source selection and final arbitration cannot mix policy generations.
	campaignPolicy atomic.Pointer[campaignPolicySnapshot]
	subscribed     atomic.Pointer[map[string]bool] // lock-free published subscribed set; nil = none
	settings       config.RateLimitSettings

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
	// directoryObservation advances for every directory refresh attempt that
	// queried at least one active game. Only rows returned by a successful
	// listing are stamped with the new value, making retained-on-error rows
	// observably stale without relying on wall-clock guesses.
	directoryObservation uint64
	// configuredDirectory is keyed by normalized login and guarded by mu. It is
	// deliberately separate from pool/current: configured objects must never be
	// run through discovery's ordinary Known assignment or online-refresh path.
	configuredDirectory map[string]configuredDirectoryEvidence
	// directoryAllUncertain is the fail-closed transition state between a
	// settings change and its replacement Directory publication.
	directoryAllUncertain bool
	// directoryUncertainGameIDs contains only active game IDs whose current
	// listing errored. Successful games remain independently authoritative, so an
	// unrelated game failure cannot starve or retain their provisional scope.
	directoryUncertainGameIDs map[string]struct{}
	// directoryScopeGeneration advances on every applied Directory refresh
	// publication, including complete-empty and errored retained-pool attempts.
	// A full quarantine scope must observe the same value before and after build.
	directoryScopeGeneration uint64
	// provisionalScopeGeneration advances only after one complete, final-fenced
	// full candidate scope has been built. Broker pruning rejects equal or older
	// generations, so delayed/replayed snapshots cannot delete newer negatives.
	provisionalScopeGeneration atomic.Uint64

	// emptyLogged makes the "pool is empty" INFO line fire once per
	// transition instead of every tick.
	emptyLogged bool

	resync chan struct{}

	// rewardSkips is the operator's effective farming-exclusion decision
	// (DropRule.Skip entries) for the shared drops evaluator. Guarded by mu;
	// the stored value is immutable. Nil excludes nothing.
	rewardSkips *models.RewardSkips

	ctx    context.Context
	cancel context.CancelFunc

	mu sync.RWMutex
}

func NewManager(
	client *twitch.TwitchClient,
	campaigns CampaignsProvider,
	tracked TrackedLoginsProvider,
	settings config.RateLimitSettings,
	games []string,
	mode config.DiscoveryMode,
	preferSubscribed bool,
) *Manager {
	return &Manager{
		client:                client,
		campaigns:             campaigns,
		tracked:               tracked,
		settings:              settings,
		mode:                  config.NormalizeDiscoveryMode(string(mode)),
		preferSubscribed:      preferSubscribed,
		games:                 games,
		directoryAllUncertain: true,
		resync:                make(chan struct{}, 1),
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

// UpdateRewardSkips replaces the operator's effective farming-exclusion
// decision (DropRule.Skip entries), published by the miner at wiring time and
// on every rule change. eligibleCampaignsForChannel consults it, so a channel
// whose only tracked campaign carries a skipped current drop is neither
// proposed to the broker nor kept as the current discovery channel
// (channelCarriesActiveCampaign turns false and invalidReason abandons it).
// Safe for concurrent use; nil excludes nothing.
func (m *Manager) UpdateRewardSkips(skips *models.RewardSkips) {
	m.mu.Lock()
	m.rewardSkips = skips
	m.mu.Unlock()
}

// currentRewardSkips snapshots the immutable farming-exclusion decision under
// the lock; callers use the returned value lock-free.
func (m *Manager) currentRewardSkips() *models.RewardSkips {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.rewardSkips
}

type campaignPolicySnapshot struct {
	gameRanks         map[string]int
	campaignSemantics map[string]policy.CampaignSemantic
}

// SetCampaignPolicy atomically publishes the compatibility/fallback game-level
// pre-order and exact per-campaign semantic facts. Both maps are copied and
// immutable. With no broker policy provider, the next local current evaluation
// sees them without directory I/O; exact candidate ordering is derived only
// after that channel's advertised campaign IDs and channel ACL are verified.
func (m *Manager) SetCampaignPolicy(gameRanks map[string]int, campaignSemantics map[string]policy.CampaignSemantic) {
	next := &campaignPolicySnapshot{
		gameRanks:         cloneGameRanks(gameRanks),
		campaignSemantics: cloneCampaignSemantics(campaignSemantics),
	}
	current := m.campaignPolicy.Load()
	if equalCampaignPolicy(current, next) {
		return
	}
	m.campaignPolicy.Store(next)
}

// SetGameRanks preserves the rank-only internal compatibility seam used by
// older callers/tests. Production receives exact semantic facts and game ranks
// from the Unified Slot Broker snapshot; game aggregation is never mistaken
// for a channel's exact bounded campaign utility. nil restores GAME_ORDER behavior.
func (m *Manager) SetGameRanks(ranks map[string]int) {
	if len(ranks) == 0 {
		m.campaignPolicy.Store(nil)
		return
	}
	m.SetCampaignPolicy(ranks, nil)
}

func cloneGameRanks(source map[string]int) map[string]int {
	if source == nil {
		return nil
	}
	cloned := make(map[string]int, len(source))
	for game, rank := range source {
		cloned[game] = rank
	}
	return cloned
}

func cloneCampaignSemantics(source map[string]policy.CampaignSemantic) map[string]policy.CampaignSemantic {
	if source == nil {
		return nil
	}
	cloned := make(map[string]policy.CampaignSemantic, len(source))
	for campaignID, fact := range source {
		cloned[campaignID] = fact
	}
	return cloned
}

func equalCampaignPolicy(current, next *campaignPolicySnapshot) bool {
	if current == nil || next == nil {
		return current == next
	}
	return equalGameRanks(current.gameRanks, next.gameRanks) &&
		equalCampaignSemantics(current.campaignSemantics, next.campaignSemantics)
}

func equalGameRanks(left, right map[string]int) bool {
	if (left == nil) != (right == nil) {
		return false
	}
	if len(left) != len(right) {
		return false
	}
	for game, rank := range left {
		if other, ok := right[game]; !ok || other != rank {
			return false
		}
	}
	return true
}

func equalCampaignSemantics(left, right map[string]policy.CampaignSemantic) bool {
	if (left == nil) != (right == nil) {
		return false
	}
	if len(left) != len(right) {
		return false
	}
	for campaignID, fact := range left {
		if other, ok := right[campaignID]; !ok || other != fact {
			return false
		}
	}
	return true
}

func (m *Manager) currentCampaignPolicy() *campaignPolicySnapshot {
	m.mu.RLock()
	status := m.slotStatus
	m.mu.RUnlock()
	if provider, ok := status.(campaignPolicyProvider); ok {
		gameRanks, campaignSemantics := provider.DiscoveryCampaignPolicy()
		if gameRanks != nil || campaignSemantics != nil {
			return &campaignPolicySnapshot{
				gameRanks:         gameRanks,
				campaignSemantics: campaignSemantics,
			}
		}
	}
	return m.campaignPolicy.Load()
}

const unrankedGamePriority = 1 << 30

type gamePolicyPriority struct {
	semantic   int
	configured int
}

type gamePolicyOrder struct {
	ranks      map[string]int
	configured map[string]int
}

func newGamePolicyOrder(games []string, ranks map[string]int) gamePolicyOrder {
	order := gamePolicyOrder{
		ranks:      ranks,
		configured: make(map[string]int, len(games)),
	}
	for i, game := range games {
		order.configured[strings.ToLower(game)] = i
	}
	return order
}

func (o gamePolicyOrder) priority(game string) gamePolicyPriority {
	key := strings.ToLower(game)
	configured := unrankedGamePriority
	if index, ok := o.configured[key]; ok {
		configured = index
	}
	semantic := configured
	if o.ranks != nil {
		semantic = unrankedGamePriority
		if rank, ok := o.ranks[key]; ok {
			semantic = rank
		}
	}
	return gamePolicyPriority{semantic: semantic, configured: configured}
}

func (o gamePolicyOrder) less(left, right string) bool {
	lp, rp := o.priority(left), o.priority(right)
	if lp.semantic != rp.semantic {
		return lp.semantic < rp.semantic
	}
	return lp.configured < rp.configured
}

// orderGamesByPolicy returns games reordered by the published policy ranks
// (stable; games absent from the map keep their configured relative order,
// sorted after ranked ones). With no ranks published it returns games
// unchanged, so the configured order is bit-identical.
func (m *Manager) orderGamesByPolicy(games []string) []string {
	published := m.currentCampaignPolicy()
	if published == nil || published.gameRanks == nil {
		return games
	}
	order := newGamePolicyOrder(games, published.gameRanks)
	ordered := make([]string, len(games))
	copy(ordered, games)
	sort.SliceStable(ordered, func(i, j int) bool {
		return order.less(ordered[i], ordered[j])
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

func (m *Manager) configuredStreamers() []*models.Streamer {
	provider, ok := m.tracked.(trackedStreamersProvider)
	if !ok {
		return nil
	}
	return provider.All()
}

func containsConfiguredGame(games []string, game string) bool {
	for _, configured := range games {
		if strings.EqualFold(configured, game) {
			return true
		}
	}
	return false
}

// provisionalConfiguredGameEnabled preserves discovery's dormant scope: even
// an exact restricted ACL may bootstrap without a Directory row, but only for
// a game the operator enabled in DirectoryGames. Game ID is resolved through
// the existing account campaign view when the live payload lacks a name.
func (m *Manager) provisionalConfiguredGameEnabled(gameID, liveGame string) bool {
	games := m.getGames()
	if m.campaigns == nil {
		return false
	}
	return provisionalGameEnabledAtSource(games, gameID, liveGame, m.campaigns.Campaigns())
}

func provisionalGameEnabledAtSource(
	games []string,
	gameID, liveGame string,
	campaigns []*models.Campaign,
) bool {
	if len(games) == 0 || gameID == "" {
		return false
	}
	if liveGame != "" && containsConfiguredGame(games, liveGame) {
		return true
	}
	for _, campaign := range campaigns {
		if campaign == nil || campaign.Game == nil || campaign.Game.ID != gameID {
			continue
		}
		if containsConfiguredGame(games, campaign.Game.Name) ||
			containsConfiguredGame(games, campaign.Game.DisplayName) {
			return true
		}
	}
	return false
}

// configuredProvisionalCandidates reads configured streamers only through
// their existing immutable/status snapshots. It deliberately performs no
// online refresh and never calls candidatePolicyFacts/channelCarriesActiveCampaign,
// so Known-positive/empty assignments remain wholly owned by the pre-existing
// configured drops path. Only UNKNOWN cold-start tuples can leave this method.
type sourceWatchProposal struct {
	candidate   watcher.Candidate
	channel     *Channel
	facts       watcher.CandidateCampaignPolicy
	provisional *provisionalCampaignEvaluation
	priority    gamePolicyPriority
	continuity  bool
}

func (m *Manager) configuredProvisionalCandidates(published *campaignPolicySnapshot) []sourceWatchProposal {
	streamers := m.configuredStreamers()
	if len(streamers) == 0 {
		return nil
	}

	m.mu.RLock()
	directory := make(map[string]configuredDirectoryEvidence, len(m.configuredDirectory))
	for login, evidence := range m.configuredDirectory {
		directory[login] = evidence
	}
	m.mu.RUnlock()

	order := newGamePolicyOrder(m.getGames(), policyGameRanks(published))
	result := make([]sourceWatchProposal, 0, len(streamers))
	seen := make(map[string]bool, len(streamers))
	for _, streamer := range streamers {
		if streamer == nil || streamer.Stream == nil {
			continue
		}
		login := streamer.GetUsername()
		loginKey := strings.ToLower(login)
		if login == "" || seen[loginKey] || m.isAvoided(login) {
			continue
		}
		seen[loginKey] = true

		stream := streamer.Stream.ProvisionalDropSnapshot()
		if !m.provisionalConfiguredGameEnabled(stream.GameID, streamer.Stream.GameName()) {
			continue
		}
		channel := &Channel{
			Streamer: streamer,
			Game:     streamer.Stream.GameName(),
			GameID:   stream.GameID,
		}
		if channel.Game == "" {
			channel.Game = stream.GameID
		}
		if evidence, ok := directory[loginKey]; ok && evidence.channelID == streamer.ChannelID {
			channel.Game = evidence.game
			channel.directoryGameID = evidence.gameID
			channel.DropsEnabled = evidence.dropsEnabled
			channel.directoryObservation = evidence.observationID
		}

		provisional, eligible := m.provisionalCandidateForChannel(channel, published)
		if !eligible || !m.provisionalCandidateStillCurrentAtSource(
			channel, provisional.candidate, provisional.sourceGeneration, provisional.sourceFenced,
		) {
			continue
		}
		candidate := provisional.candidate
		copy := *provisional
		result = append(result, sourceWatchProposal{
			candidate: watcher.Candidate{
				Streamer:        streamer,
				Origin:          watcher.OriginDiscovery,
				Reason:          "configured channel provisional UNKNOWN drops observation for " + channel.Game,
				ProvisionalDrop: &candidate,
			},
			channel:     channel,
			provisional: &copy,
			priority:    order.priority(channel.Game),
			continuity:  copy.owned,
		})
	}
	return result
}

// reconcileProvisionalQuarantineScope publishes one complete candidate
// namespace before quarantine filtering, proof continuity, ranking, and
// per-login selection. It is deliberately separate from WatchCandidates'
// result: the latter omits quarantined and unselected tuples and therefore
// cannot authorize lifecycle pruning.
func (m *Manager) reconcileProvisionalQuarantineScope(published *campaignPolicySnapshot) {
	m.mu.RLock()
	status := m.slotStatus
	directoryAuthority := m.provisionalDirectoryAuthorityLocked()
	directoryGeneration := m.directoryScopeGeneration
	configuredGames := append([]string(nil), m.games...)
	channels := append([]*Channel(nil), m.pool...)
	if m.current != nil {
		channels = append(channels, m.current)
	}
	directory := make(map[string]configuredDirectoryEvidence, len(m.configuredDirectory))
	for login, evidence := range m.configuredDirectory {
		directory[login] = evidence
	}
	m.mu.RUnlock()

	reconciler, ok := status.(provisionalQuarantineReconciler)
	if !ok {
		return
	}
	// Activate exact-scope admission before any source or build fence can return.
	// A source that becomes usable later in this WatchCandidates tick must not
	// publish a provisional envelope outside a successfully committed namespace.
	reconciler.RequireProvisionalScope()
	source, fenced := m.accountCampaignSnapshot()
	if !fenced || source.Generation == 0 || source.SourceRevision != source.CurrentRevision {
		return
	}
	accountWork := provisionalOpenAccountWorkAtSource(configuredGames, source.Campaigns)

	// Add the configured roster with its exact objects. Virtual Channel wrappers
	// carry only the retained independent Directory evidence needed by open
	// campaigns; restricted candidates remain ACL-authoritative without it.
	for _, streamer := range m.configuredStreamers() {
		if streamer == nil || streamer.Stream == nil {
			continue
		}
		stream := streamer.Stream.ProvisionalDropSnapshot()
		channel := &Channel{
			Streamer: streamer,
			Game:     streamer.Stream.GameName(),
			GameID:   stream.GameID,
		}
		if channel.Game == "" {
			channel.Game = stream.GameID
		}
		if evidence, exists := directory[strings.ToLower(streamer.GetUsername())]; exists &&
			evidence.channelID == streamer.ChannelID {
			channel.Game = evidence.game
			channel.directoryGameID = evidence.gameID
			channel.DropsEnabled = evidence.dropsEnabled
			channel.directoryObservation = evidence.observationID
		}
		channels = append(channels, channel)
	}

	seen := make(map[*models.Streamer]struct{}, len(channels))
	scopes := make([]watcher.ProvisionalQuarantineOwnerScope, 0, len(channels))
	for _, channel := range channels {
		if channel == nil || channel.Streamer == nil || channel.Streamer.Stream == nil {
			continue
		}
		streamer := channel.Streamer
		if _, duplicate := seen[streamer]; duplicate {
			continue
		}
		seen[streamer] = struct{}{}
		stream := streamer.Stream.ProvisionalDropSnapshot()
		if stream.BroadcastID == "" || stream.SessionGeneration == 0 {
			continue
		}
		scope := watcher.ProvisionalQuarantineOwnerScope{
			Streamer:          streamer,
			BroadcastID:       stream.BroadcastID,
			SessionGeneration: stream.SessionGeneration,
		}
		liveGame := streamer.Stream.GameName()
		if liveGame == "" {
			liveGame, _, _, _ = m.channelFacts(channel)
		}
		// Temporary watchdog avoidance affects ranking/admission only. It is not
		// source authority and therefore cannot remove an otherwise-live tuple from
		// the full quarantine scope.
		if provisionalGameEnabledAtSource(configuredGames, stream.GameID, liveGame, source.Campaigns) {
			for _, evaluation := range m.provisionalCandidatesForChannelAtSource(channel, published, source, true) {
				scope.Candidates = append(scope.Candidates, evaluation.candidate)
			}
		}
		scopes = append(scopes, scope)
	}

	// End-of-build fences. An errored/replaced Directory publication, a newer
	// broker campaign envelope, or session churn makes the entire scope a no-op;
	// partial snapshots never delete negatives.
	m.mu.RLock()
	directoryStillCurrent := sameProvisionalDirectoryAuthority(
		m.provisionalDirectoryAuthorityLocked(), directoryAuthority,
	) &&
		m.directoryScopeGeneration == directoryGeneration
	m.mu.RUnlock()
	if !directoryStillCurrent {
		return
	}
	currentSource, currentFenced := m.accountCampaignSnapshot()
	if !currentFenced || currentSource.Generation != source.Generation ||
		currentSource.SourceRevision != currentSource.CurrentRevision ||
		currentSource.SourceRevision != source.SourceRevision {
		return
	}
	for _, scope := range scopes {
		stream := scope.Streamer.Stream.ProvisionalDropSnapshot()
		if stream.BroadcastID != scope.BroadcastID || stream.SessionGeneration != scope.SessionGeneration {
			return
		}
	}

	scopeGeneration := m.provisionalScopeGeneration.Add(1)
	if scopeGeneration == 0 {
		scopeGeneration = m.provisionalScopeGeneration.Add(1)
	}
	reconciler.ReconcileProvisionalQuarantine(scopeGeneration, directoryAuthority, accountWork, scopes)
}

// provisionalDirectoryAuthorityLocked snapshots the per-game incompleteness
// fence in deterministic order. Caller holds m.mu for reading or writing.
func (m *Manager) provisionalDirectoryAuthorityLocked() watcher.ProvisionalDirectoryAuthority {
	authority := watcher.ProvisionalDirectoryAuthority{AllUncertain: m.directoryAllUncertain}
	if authority.AllUncertain {
		return authority
	}
	authority.UncertainGameIDs = make([]string, 0, len(m.directoryUncertainGameIDs))
	for gameID := range m.directoryUncertainGameIDs {
		authority.UncertainGameIDs = append(authority.UncertainGameIDs, gameID)
	}
	sort.Strings(authority.UncertainGameIDs)
	return authority
}

func sameProvisionalDirectoryAuthority(left, right watcher.ProvisionalDirectoryAuthority) bool {
	if left.AllUncertain != right.AllUncertain ||
		len(left.UncertainGameIDs) != len(right.UncertainGameIDs) {
		return false
	}
	for i := range left.UncertainGameIDs {
		if left.UncertainGameIDs[i] != right.UncertainGameIDs[i] {
			return false
		}
	}
	return true
}

func provisionalOpenAccountWorkAtSource(
	configuredGames []string,
	campaigns []*models.Campaign,
) []watcher.ProvisionalAccountWork {
	workByKey := make(map[string]watcher.ProvisionalAccountWork)
	for _, campaign := range campaigns {
		if campaign == nil || campaign.ID == "" || campaign.Game == nil || campaign.Game.ID == "" ||
			(!containsConfiguredGame(configuredGames, campaign.Game.Name) &&
				!containsConfiguredGame(configuredGames, campaign.Game.DisplayName)) ||
			!campaign.HasRemainingUnclaimedWork() || campaign.ACL.Source == models.ACLSourceNone ||
			!campaign.ACLComplete() || campaign.ACLState() != models.ACLUnrestricted {
			continue
		}
		drop := campaign.CurrentDrop()
		if drop == nil || drop.ID == "" || drop.HasPreconditionsMet == nil || !*drop.HasPreconditionsMet {
			continue
		}
		work := watcher.ProvisionalAccountWork{
			CampaignID: campaign.ID,
			DropID:     drop.ID,
			GameID:     campaign.Game.ID,
		}
		key := work.CampaignID + "\x00" + work.DropID + "\x00" + work.GameID
		workByKey[key] = work
	}
	work := make([]watcher.ProvisionalAccountWork, 0, len(workByKey))
	for _, item := range workByKey {
		work = append(work, item)
	}
	sort.Slice(work, func(i, j int) bool {
		left := work[i].CampaignID + "\x00" + work[i].DropID + "\x00" + work[i].GameID
		right := work[j].CampaignID + "\x00" + work[j].DropID + "\x00" + work[j].GameID
		return left < right
	})
	return work
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
	m.directoryScopeGeneration++
	if m.directoryScopeGeneration == 0 {
		m.directoryScopeGeneration++
	}
	m.directoryAllUncertain = true
	m.directoryUncertainGameIDs = nil
	for login, evidence := range m.configuredDirectory {
		if !containsConfiguredGame(games, evidence.game) {
			delete(m.configuredDirectory, login)
		}
	}
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
		streams []twitch.DirectoryStream
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
	m.directoryScopeGeneration++
	if m.directoryScopeGeneration == 0 {
		m.directoryScopeGeneration++
	}
	directoryAllUncertain := false
	directoryUncertainGameIDs := make(map[string]struct{})
	for _, listing := range listings {
		if listing.err != nil {
			if listing.gameID == "" {
				directoryAllUncertain = true
				directoryUncertainGameIDs = nil
				break
			}
			directoryUncertainGameIDs[listing.gameID] = struct{}{}
		}
	}
	var directoryObservation uint64
	if len(listings) > 0 {
		m.directoryObservation++
		if m.directoryObservation == 0 { // reserve zero for "never observed"
			m.directoryObservation++
		}
		directoryObservation = m.directoryObservation
	}
	// Start from the retained evidence set. A failed listing keeps its prior row
	// (which is stale because the manager generation advanced); a successful
	// listing replaces every configured row previously observed for that game.
	configuredDirectory := make(map[string]configuredDirectoryEvidence, len(m.configuredDirectory))
	for login, evidence := range m.configuredDirectory {
		if containsConfiguredGame(games, evidence.game) {
			configuredDirectory[login] = evidence
		}
	}
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
		for login, evidence := range configuredDirectory {
			if strings.EqualFold(evidence.game, l.game) {
				delete(configuredDirectory, login)
			}
		}

		candidates := make([]*Channel, 0, len(l.streams))
		for _, ds := range l.streams {
			if !ds.DropsEnabled || ds.Login == "" || ds.ChannelID == "" {
				continue
			}
			loginKey := strings.ToLower(ds.Login)
			if trackedSet[loginKey] && ds.GameID != "" {
				configuredDirectory[loginKey] = configuredDirectoryEvidence{
					game:          l.game,
					gameID:        ds.GameID,
					channelID:     ds.ChannelID,
					dropsEnabled:  true,
					observationID: directoryObservation,
				}
			}
			// In "all" mode, channels on the configured streamer list are the
			// rotation's business — duplicating them here would double-report
			// watch minutes for the same channel and waste the discovery slot.
			// "tracked_only" inverts this: it keeps ONLY configured-list channels
			// and drops everything else from the pool.
			if trackedOnly {
				if !trackedSet[loginKey] {
					continue
				}
			} else if trackedSet[loginKey] {
				continue
			}
			ch := m.findExistingLocked(ds.Login)
			if ch == nil {
				ch = &Channel{Streamer: newEphemeralStreamer(ds.Login, ds.ChannelID)}
			}
			ch.Game = l.game
			ch.GameID = ds.GameID
			ch.directoryGameID = ds.GameID
			if ch.GameID == "" {
				ch.GameID = l.gameID
			}
			ch.Viewers = ds.Viewers
			ch.DropsEnabled = true
			ch.directoryObservation = directoryObservation
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
	m.configuredDirectory = configuredDirectory
	m.directoryAllUncertain = directoryAllUncertain
	m.directoryUncertainGameIDs = directoryUncertainGameIDs
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

// accountCampaigns returns the broker-facing account campaign snapshot when
// the provider exposes it. This may be broader than the confirmed assignment
// view used by eligibleCampaignsForChannel, which deliberately continues to
// call Campaigns() directly.
func (m *Manager) accountCampaignSnapshot() (drops.BrokerCampaignSnapshot, bool) {
	if m.campaigns == nil {
		return drops.BrokerCampaignSnapshot{}, false
	}
	if provider, ok := m.campaigns.(brokerCampaignsProvider); ok {
		return provider.BrokerCampaignSnapshot(), true
	}
	return drops.BrokerCampaignSnapshot{Campaigns: m.campaigns.Campaigns()}, false
}

// channelCarriesActiveCampaign reports whether at least one of the campaigns
// Twitch lists as available on this channel (Stream.CampaignIDs) is a
// tracker-active one. It also publishes the exact eligible intersection on the
// ephemeral Streamer, so broker hard classification sees an allowed restricted
// campaign just as it does for a configured streamer.
func (m *Manager) channelCarriesActiveCampaign(ch *Channel) bool {
	eligible := m.eligibleCampaignsForChannel(ch)
	ch.Streamer.Stream.SetCampaigns(eligible)
	return len(eligible) > 0
}

// eligibleCampaignsForChannel derives the authoritative assignment for an
// ephemeral discovery streamer. A new discovery proposal requires a Known
// channel-side availability snapshot, an exact advertised/account-known
// campaign intersection, real remaining work, and the shared Drops evaluator.
// Retained IDs under Unknown are continuity/diagnostic data only and can never
// create a discovery assignment.
func (m *Manager) eligibleCampaignsForChannel(ch *Channel) []*models.Campaign {
	if m.campaigns == nil {
		return nil
	}

	availability, ids := ch.Streamer.Stream.CampaignAvailability()
	if availability != models.CampaignAvailabilityKnown {
		return nil
	}
	if len(ids) == 0 {
		return nil
	}
	idSet := make(map[string]bool, len(ids))
	for _, id := range ids {
		idSet[id] = true
	}

	_, gameID, _, _ := m.channelFacts(ch)
	if gameID == "" {
		return nil
	}
	// One coherent farming-exclusion decision per channel evaluation (the
	// stored value is immutable), mirroring the tracker's per-sweep snapshot.
	skips := m.currentRewardSkips()
	var eligible []*models.Campaign
	for _, c := range m.campaigns.Campaigns() {
		if c == nil || c.ID == "" || !idSet[c.ID] {
			continue
		}
		if c.Game == nil || c.Game.ID == "" || c.Game.ID != gameID {
			continue
		}
		if !c.HasRemainingUnclaimedWork() {
			continue
		}
		drop := c.CurrentDrop()
		if drop == nil {
			continue
		}
		// Operator farming exclusion: a campaign whose current drop carries a
		// Skip rule never justifies a discovery proposal (the channel would be
		// watched ONLY to farm the skipped reward). Keyed exactly like the
		// policy ranker's rule lookup; c.Game is non-nil past the game gate
		// above.
		if skips.SkipsReward(c.Game.ID, drop.Name) {
			continue
		}
		decision := (eligibility.Evaluator{}).EvaluateDrops(ch.Streamer, c, drop, eligibility.AvailabilityYes)
		if !decision.Eligible {
			continue
		}
		eligible = append(eligible, c)
	}
	return eligible
}

// provisionalCampaignEvaluation is discovery-local ordering metadata for one
// exact lower-authority observation proposal. It must never be converted into
// watcher.CandidateCampaignPolicy: channel-advertised availability is UNKNOWN,
// so there is no confirmed campaign policy fact to publish.
type provisionalCampaignEvaluation struct {
	candidate        models.ProvisionalDropCandidate
	restricted       bool
	ranked           bool
	utility          policy.SemanticUtility
	proved           bool
	owned            bool
	sourceGeneration uint64
	sourceFenced     bool
}

// provisionalCandidateForChannel derives a pure observation candidate for an
// UNKNOWN channel without touching Stream.CampaignIDs or Stream.Campaigns. The
// account campaign, live stream, Directory row and playback-session identities
// are all exact; the final snapshot check fences concurrent stream refreshes.
func (m *Manager) provisionalCandidateForChannel(
	ch *Channel,
	published *campaignPolicySnapshot,
) (*provisionalCampaignEvaluation, bool) {
	source, sourceFenced := m.accountCampaignSnapshot()
	evaluations := m.provisionalCandidatesForChannelAtSource(ch, published, source, sourceFenced)
	var best *provisionalCampaignEvaluation
	for i := range evaluations {
		evaluation := evaluations[i]
		if m.isProvisionalQuarantined(ch.Streamer, evaluation.candidate) {
			continue
		}
		evaluation.owned = m.ownsProvisionalCandidate(ch.Streamer, evaluation.candidate)
		evaluation.proved = m.hasProvisionalProof(ch.Streamer, evaluation.candidate)
		if best == nil || compareProvisionalCampaigns(evaluation, *best) < 0 {
			copy := evaluation
			best = &copy
		}
	}
	if best == nil || !m.provisionalCandidateStillCurrentAtSource(
		ch, best.candidate, best.sourceGeneration, best.sourceFenced,
	) {
		return nil, false
	}
	return best, true
}

// provisionalCandidatesForChannelAtSource derives the complete eligible tuple
// scope for one exact owner before broker quarantine, proof continuity, ranking,
// or best-candidate selection. That distinction is what lets the broker retain
// quarantined A while B is selected and safely prune only campaign/drop slots
// absent from a later complete authoritative account snapshot.
func (m *Manager) provisionalCandidatesForChannelAtSource(
	ch *Channel,
	published *campaignPolicySnapshot,
	source drops.BrokerCampaignSnapshot,
	sourceFenced bool,
) []provisionalCampaignEvaluation {
	if ch == nil || m.campaigns == nil || ch.Streamer.GetStatus() != models.StatusOnline {
		return nil
	}
	// This bootstrap is cold-start-only. An UNKNOWN refresh after a previously
	// confirmed discovery assignment follows the existing invalidation path,
	// which clears that assignment; it must not relabel stale authority as a new
	// provisional lease.
	if len(ch.Streamer.Stream.GetCampaigns()) != 0 {
		return nil
	}

	stream := ch.Streamer.Stream.ProvisionalDropSnapshot()
	if stream.Availability.State != models.CampaignAvailabilityUnknown ||
		stream.Availability.ObservationID == 0 || stream.Availability.ObservedAt.IsZero() || stream.GameID == "" ||
		stream.BroadcastID == "" || stream.SessionGeneration == 0 {
		return nil
	}

	directoryGameID, dropsEnabled, channelObservation, currentObservation := m.provisionalDirectoryFacts(ch)
	login, channelID := ch.Streamer.GetUsername(), ch.Streamer.ChannelID
	if login == "" || channelID == "" {
		return nil
	}

	if sourceFenced && (source.Generation == 0 || source.SourceRevision != source.CurrentRevision) {
		return nil
	}

	// One coherent operator farming-exclusion decision per channel evaluation
	// (the stored value is immutable). The gate below closes both the runtime
	// rule-flip window — broker views republish only on sync passes, while
	// this constructor runs every broker tick — and the unfenced raw-pool
	// fallback of accountCampaignSnapshot.
	skips := m.currentRewardSkips()

	result := make([]provisionalCampaignEvaluation, 0, len(source.Campaigns))
	for _, campaign := range source.Campaigns {
		if campaign == nil || campaign.ID == "" || campaign.Game == nil ||
			campaign.Game.ID == "" || campaign.Game.ID != stream.GameID ||
			!campaign.HasRemainingUnclaimedWork() {
			continue
		}
		if stream.HasConfirmedCampaign(campaign.ID) {
			continue
		}
		drop := campaign.CurrentDrop()
		if drop == nil || drop.ID == "" || drop.HasPreconditionsMet == nil || !*drop.HasPreconditionsMet {
			continue
		}
		// Operator farming exclusion (DropRule.Skip): a skipped current reward
		// never seeds a provisional observation candidate — the channel would
		// be watched solely to farm the reward the operator excluded. Keyed on
		// the campaign's actual current drop name, exactly like every other
		// RewardSkips boundary.
		if skips.SkipsReward(campaign.Game.ID, drop.Name) {
			continue
		}
		if decision := (eligibility.Evaluator{}).EvaluateDrops(
			ch.Streamer, campaign, drop, eligibility.AvailabilityUnknown,
		); !decision.Eligible {
			continue
		}

		// Provisional observation accepts only an explicitly typed, complete ACL.
		// Legacy Channels compatibility and an unknown/incomplete response are not
		// enough authority for this bootstrap path.
		if campaign.ACL.Source == models.ACLSourceNone || !campaign.ACLComplete() {
			continue
		}

		candidate := models.ProvisionalDropCandidate{
			CampaignID:           campaign.ID,
			Campaign:             campaign.Name,
			DropID:               drop.ID,
			Drop:                 drop.Name,
			GameID:               stream.GameID,
			Login:                login,
			ChannelID:            channelID,
			BroadcastID:          stream.BroadcastID,
			SessionGeneration:    stream.SessionGeneration,
			AvailabilityObs:      stream.Availability.ObservationID,
			AvailabilityKnownGen: stream.Availability.KnownGeneration,
		}
		evaluation := provisionalCampaignEvaluation{
			candidate:        candidate,
			sourceGeneration: source.Generation,
			sourceFenced:     sourceFenced,
		}
		switch campaign.ACLState() {
		case models.ACLUnrestricted:
			if directoryGameID == "" || directoryGameID != stream.GameID ||
				!dropsEnabled || channelObservation == 0 || channelObservation != currentObservation {
				continue
			}
			evaluation.candidate.Evidence = models.ProvisionalEvidenceDirectory
			evaluation.candidate.DirectoryObs = channelObservation
		case models.ACLRestricted:
			if !campaign.AllowsChannel(channelID) || len(campaign.ACL.ChannelIDs) == 0 {
				continue
			}
			evaluation.restricted = true
			evaluation.candidate.Evidence = models.ProvisionalEvidenceRestrictedACL
			evaluation.candidate.RestrictedACL = append([]string(nil), campaign.ACL.ChannelIDs...)
			sort.Strings(evaluation.candidate.RestrictedACL)
		default:
			continue
		}
		if evaluation.candidate.Campaign == "" {
			evaluation.candidate.Campaign = campaign.ID
		}
		if evaluation.candidate.Drop == "" {
			evaluation.candidate.Drop = drop.ID
		}
		if !evaluation.candidate.Valid() {
			continue
		}

		if published != nil && published.campaignSemantics != nil {
			evaluation.utility, evaluation.ranked = policy.BuildSemanticUtilityWithRemainingWork(
				[]string{campaign.ID}, []string{campaign.ID}, published.campaignSemantics,
			)
		}
		result = append(result, evaluation)
	}
	return result
}

func (m *Manager) hasProvisionalProof(streamer *models.Streamer, candidate models.ProvisionalDropCandidate) bool {
	m.mu.RLock()
	status := m.slotStatus
	m.mu.RUnlock()
	provider, ok := status.(provisionalProofProvider)
	return ok && provider.HasProvisionalProof(streamer, candidate)
}

func (m *Manager) isProvisionalQuarantined(streamer *models.Streamer, candidate models.ProvisionalDropCandidate) bool {
	m.mu.RLock()
	status := m.slotStatus
	m.mu.RUnlock()
	provider, ok := status.(provisionalQuarantineProvider)
	return ok && provider.IsProvisionalQuarantined(streamer, candidate)
}

func (m *Manager) runRoutineRefresh(ctx context.Context, streamer *models.Streamer) bool {
	if streamer == nil || m.client == nil {
		return false
	}
	m.mu.RLock()
	status := m.slotStatus
	m.mu.RUnlock()
	refresh := func() { m.client.CheckStreamerOnlineContext(ctx, streamer) }
	if runner, ok := status.(routineRefreshRunner); ok {
		return runner.RunRoutineRefresh(streamer, refresh)
	}
	refresh()
	return true
}

func (m *Manager) ownsProvisionalCandidate(streamer *models.Streamer, candidate models.ProvisionalDropCandidate) bool {
	m.mu.RLock()
	status := m.slotStatus
	m.mu.RUnlock()
	provider, ok := status.(provisionalCandidateOwner)
	return ok && provider.OwnsProvisionalCandidate(streamer, candidate)
}

func (m *Manager) provisionalDirectoryFacts(ch *Channel) (gameID string, dropsEnabled bool, channelObservation, currentObservation uint64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return ch.directoryGameID, ch.DropsEnabled, ch.directoryObservation, m.directoryObservation
}

// provisionalCandidateStillCurrent closes the read window between derivation
// and publication. Stream facts are re-read atomically; Directory facts share
// the Manager lock. A refresh to Known (including Known-empty), a game/session
// change, or a newer open-campaign Directory generation suppresses the tuple.
func (m *Manager) provisionalCandidateStillCurrent(ch *Channel, candidate models.ProvisionalDropCandidate) bool {
	return m.provisionalCandidateStillCurrentAtSource(ch, candidate, 0, false)
}

func (m *Manager) provisionalCandidateStillCurrentAtSource(
	ch *Channel,
	candidate models.ProvisionalDropCandidate,
	sourceGeneration uint64,
	requireSameSource bool,
) bool {
	if ch == nil || ch.Streamer == nil || ch.Streamer.Stream == nil || !candidate.Valid() ||
		ch.Streamer.GetStatus() != models.StatusOnline ||
		ch.Streamer.GetUsername() != candidate.Login || ch.Streamer.ChannelID != candidate.ChannelID {
		return false
	}
	stream := ch.Streamer.Stream.ProvisionalDropSnapshot()
	if stream.Availability.State != models.CampaignAvailabilityUnknown ||
		stream.Availability.ObservationID != candidate.AvailabilityObs ||
		stream.Availability.ObservedAt.IsZero() ||
		stream.GameID != candidate.GameID || stream.BroadcastID != candidate.BroadcastID ||
		stream.SessionGeneration != candidate.SessionGeneration ||
		stream.HasConfirmedCampaign(candidate.CampaignID) {
		return false
	}
	directoryGameID, dropsEnabled, channelObservation, currentObservation := m.provisionalDirectoryFacts(ch)
	// A concurrent drops re-point may have published a confirmed assignment
	// after the cold-start check but before this final fence. Never let the same
	// stream leave discovery carrying both authorities: the ordinary assignment
	// path owns it from this point on.
	if len(ch.Streamer.Stream.GetCampaigns()) != 0 {
		return false
	}
	if m.isProvisionalQuarantined(ch.Streamer, candidate) {
		return false
	}

	// Re-read the broker-facing account authority at the publication boundary.
	// A production snapshot must be both caught up to the current campaign pool
	// and, for the candidate derived in this call, the very same broker envelope.
	// That gives each proposal a source-snapshot linearization point and prevents
	// campaign removal, drop advancement, or open/restricted ACL drift from
	// escaping between derivation and return.
	source, fenced := m.accountCampaignSnapshot()
	if fenced {
		if source.Generation == 0 || source.SourceRevision != source.CurrentRevision ||
			(requireSameSource && source.Generation != sourceGeneration) {
			return false
		}
	} else if requireSameSource {
		return false
	}
	if !provisionalCandidateBackedByCampaign(ch.Streamer, candidate, source.Campaigns, m.currentRewardSkips()) {
		return false
	}

	if candidate.Evidence == models.ProvisionalEvidenceDirectory {
		return directoryGameID == candidate.GameID && dropsEnabled && candidate.DirectoryObs != 0 &&
			candidate.DirectoryObs == channelObservation && channelObservation == currentObservation
	}
	return candidate.Evidence == models.ProvisionalEvidenceRestrictedACL && candidate.DirectoryObs == 0
}

func provisionalCandidateBackedByCampaign(
	streamer *models.Streamer,
	candidate models.ProvisionalDropCandidate,
	campaigns []*models.Campaign,
	skips *models.RewardSkips,
) bool {
	if streamer == nil || !candidate.Valid() {
		return false
	}
	for _, campaign := range campaigns {
		if campaign == nil || campaign.ID != candidate.CampaignID || campaign.Game == nil ||
			campaign.Game.ID != candidate.GameID || !campaign.HasRemainingUnclaimedWork() {
			continue
		}
		drop := campaign.CurrentDrop()
		if drop == nil || drop.ID != candidate.DropID || drop.HasPreconditionsMet == nil || !*drop.HasPreconditionsMet {
			return false
		}
		// Operator farming exclusion — the publication fence re-checks the
		// live rule set so a proposal derived just before a runtime Skip flip
		// cannot leave discovery within the same tick.
		if skips.SkipsReward(campaign.Game.ID, drop.Name) {
			return false
		}
		if decision := (eligibility.Evaluator{}).EvaluateDrops(
			streamer, campaign, drop, eligibility.AvailabilityUnknown,
		); !decision.Eligible {
			return false
		}
		if campaign.ACL.Source == models.ACLSourceNone || !campaign.ACLComplete() {
			return false
		}
		switch candidate.Evidence {
		case models.ProvisionalEvidenceDirectory:
			return campaign.ACLState() == models.ACLUnrestricted
		case models.ProvisionalEvidenceRestrictedACL:
			if campaign.ACLState() != models.ACLRestricted || !campaign.AllowsChannel(candidate.ChannelID) {
				return false
			}
			currentACL := append([]string(nil), campaign.ACL.ChannelIDs...)
			sort.Strings(currentACL)
			return stringSlicesEqual(currentACL, candidate.RestrictedACL)
		default:
			return false
		}
	}
	return false
}

func stringSlicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// compareProvisionalCampaigns returns negative when left is the stronger choice
// among provisional account campaigns for the same lower-authority source. The
// existing restricted hard fact and published semantic utility are reused; IDs
// and labels never invent a cross-channel preference.
func compareProvisionalCampaigns(left, right provisionalCampaignEvaluation) int {
	if left.proved != right.proved {
		if left.proved {
			return -1
		}
		return 1
	}
	if left.restricted != right.restricted {
		if left.restricted {
			return -1
		}
		return 1
	}
	if left.ranked != right.ranked {
		if left.ranked {
			return -1
		}
		return 1
	}
	if left.ranked {
		if comparison := -policy.CompareSemanticUtility(left.utility, right.utility); comparison != 0 {
			return comparison
		}
	}
	if left.owned != right.owned {
		if left.owned {
			return -1
		}
		return 1
	}
	return 0
}

// candidatePolicyFacts derives the exact immutable facts carried with a
// discovery proposal. Game-best ranks are intentionally absent here: only an
// eligible campaign actually advertised by this channel may supply its class.
func (m *Manager) candidatePolicyFacts(ch *Channel, published *campaignPolicySnapshot) (watcher.CandidateCampaignPolicy, bool) {
	eligible := m.eligibleCampaignsForChannel(ch)
	ch.Streamer.Stream.SetCampaigns(eligible)
	if len(eligible) == 0 {
		return watcher.CandidateCampaignPolicy{}, false
	}

	campaignIDs, remainingWorkIDs := watcher.CampaignSemanticEvidence(eligible)
	facts := watcher.CandidateCampaignPolicy{
		CampaignIDs:              campaignIDs,
		RemainingWorkCampaignIDs: remainingWorkIDs,
	}
	for _, c := range eligible {
		if c.IsChannelRestricted() {
			facts.Restricted = true
		}
	}
	if published != nil && published.campaignSemantics != nil {
		facts.Utility, facts.Ranked = policy.BuildSemanticUtilityWithRemainingWork(
			facts.CampaignIDs,
			facts.RemainingWorkCampaignIDs,
			published.campaignSemantics,
		)
	}

	// Prefer a restricted campaign name when the hard restricted fact owns the
	// slot explanation; otherwise name the primary campaign selected by the
	// common bounded projection. This label never participates in comparison.
	chosen := eligible[0]
	for _, c := range eligible {
		if facts.Restricted && c.IsChannelRestricted() {
			chosen = c
			break
		}
		if !facts.Restricted && facts.Ranked && c.ID == facts.Utility.PrimaryCampaignID {
			chosen = c
			break
		}
	}
	facts.Campaign = chosen.Name
	if facts.Campaign == "" {
		facts.Campaign = chosen.ID
	}
	return facts, true
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
	m.configuredDirectory = nil
	m.emptyLogged = false
	m.directoryScopeGeneration++
	if m.directoryScopeGeneration == 0 {
		m.directoryScopeGeneration++
	}
	m.directoryAllUncertain = false
	m.directoryUncertainGameIDs = nil
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
func (m *Manager) WatchCandidates(ctx context.Context) []watcher.Candidate {
	published := m.currentCampaignPolicy()
	ch := m.prepareCurrentWithPolicy(ctx, published)
	m.reconcileProvisionalQuarantineScope(published)
	order := newGamePolicyOrder(m.getGames(), policyGameRanks(published))
	var proposals []sourceWatchProposal
	if ch != nil {
		game, _, _, _ := m.channelFacts(ch)
		if availability := ch.Streamer.Stream.ProvisionalDropSnapshot().Availability.State; availability == models.CampaignAvailabilityKnown {
			facts, eligible := m.candidatePolicyFacts(ch, published)
			if eligible {
				proposals = append(proposals, sourceWatchProposal{
					candidate: watcher.Candidate{
						Streamer: ch.Streamer,
						Origin:   watcher.OriginDiscovery,
						Reason:   "best available drops-enabled channel for " + game,
					},
					channel: ch, facts: facts, priority: order.priority(game), continuity: true,
				})
			}
		} else {
			// UNKNOWN is a separate, strictly lower-authority path. It competes
			// only through the common source comparator below and carries no
			// confirmed Campaign Policy publication.
			provisional, eligible := m.provisionalCandidateForChannel(ch, published)
			if eligible && len(ch.Streamer.Stream.GetCampaigns()) == 0 &&
				m.provisionalCandidateStillCurrentAtSource(
					ch, provisional.candidate, provisional.sourceGeneration, provisional.sourceFenced,
				) {
				candidate := provisional.candidate
				copy := *provisional
				proposals = append(proposals, sourceWatchProposal{
					candidate: watcher.Candidate{
						Streamer:        ch.Streamer,
						Origin:          watcher.OriginDiscovery,
						Reason:          "provisional UNKNOWN drops observation for " + game,
						ProvisionalDrop: &candidate,
					},
					channel: ch, provisional: &copy, priority: order.priority(game),
					continuity: copy.owned,
				})
			}
		}
	}

	configured := m.configuredProvisionalCandidates(published)
	if len(proposals) == 1 {
		for _, candidate := range configured {
			if candidate.candidate.Streamer.GetUsername() == proposals[0].candidate.Streamer.GetUsername() {
				// tracked_only may have built an ephemeral clone for the same
				// configured login. Only the exact All() object may carry configured
				// provisional authority.
				proposals = nil
				break
			}
		}
	}
	proposals = append(proposals, configured...)
	if len(proposals) == 0 {
		m.publishCandidatePolicy("", watcher.CandidateCampaignPolicy{})
		return nil
	}

	// One source snapshot may carry one ordinary proposal plus several distinct
	// provisional fallbacks. Campaign Policy supplies the order; continuity wins
	// only a complete tie, so a strict stronger tuple precedes (and can replace)
	// the active lease while equal/weaker tuples cannot churn it. The broker keeps
	// the full provisional list through final admission so a conflicting first
	// tuple cannot hide a clean second tuple from an otherwise-idle slot.
	sort.SliceStable(proposals, func(i, j int) bool {
		comparison := compareEvaluatedCandidatePolicy(
			proposals[i].facts, proposals[i].provisional,
			proposals[j].facts, proposals[j].provisional,
			proposals[i].priority, proposals[j].priority,
			published,
		)
		if comparison != 0 {
			return comparison < 0
		}
		return proposals[i].continuity && !proposals[j].continuity
	})

	result := make([]watcher.Candidate, 0, len(proposals))
	seen := make(map[string]bool, len(proposals))
	var ordinary *sourceWatchProposal
	for i := range proposals {
		proposal := &proposals[i]
		login := proposal.candidate.Streamer.GetUsername()
		if seen[login] {
			continue
		}
		if proposal.provisional != nil {
			if proposal.channel == nil || !m.provisionalCandidateStillCurrentAtSource(
				proposal.channel, proposal.provisional.candidate,
				proposal.provisional.sourceGeneration, proposal.provisional.sourceFenced,
			) {
				continue
			}
		} else if ordinary == nil {
			ordinary = proposal
		} else {
			// CandidatePolicyPublisher is intentionally a one-login snapshot; keep
			// the historical single ordinary discovery proposal.
			continue
		}
		seen[login] = true
		result = append(result, proposal.candidate)
	}
	if ordinary != nil {
		m.publishCandidatePolicy(ordinary.candidate.Streamer.GetUsername(), ordinary.facts)
	} else {
		m.publishCandidatePolicy("", watcher.CandidateCampaignPolicy{})
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func (m *Manager) publishCandidatePolicy(login string, facts watcher.CandidateCampaignPolicy) {
	if publisher, ok := m.slotStatus.(candidatePolicyPublisher); ok {
		publisher.SetDiscoveryCandidatePolicy(login, facts)
	}
}

// prepareCurrent selects and validates the current best discovered channel and
// returns it (or nil when nothing is watchable). It performs exactly the
// selection, stale re-verification, and auto-switching the old watch loop did,
// but never reports a watched minute — that is the slot broker's job once it
// places the channel in a slot. Runs on the broker's loop goroutine.
func (m *Manager) prepareCurrent(ctx context.Context) *Channel {
	return m.prepareCurrentWithPolicy(ctx, m.currentCampaignPolicy())
}

func (m *Manager) prepareCurrentWithPolicy(ctx context.Context, published *campaignPolicySnapshot) *Channel {
	games := m.getGames()
	if len(games) == 0 {
		return nil
	}
	m.mu.RLock()
	current := m.current
	m.mu.RUnlock()

	if current == nil {
		next := m.selectBestWithPolicy(ctx, nil, published)
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
			"reason", "best available eligible channel under Campaign Policy and existing discovery tie-breaks")
		events.Record(events.TypeDiscoverySelected, next.Streamer.GetUsername(),
			"directory candidate: "+game)
		current = next
	}

	// Mirror the watcher: re-verify a stream whose info has gone stale. This
	// also refreshes the channel's available campaign IDs and current game.
	if current.Streamer.Stream.UpdateElapsed() > staleStreamRecheck {
		m.runRoutineRefresh(ctx, current.Streamer)
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

		next := m.selectBestWithPolicy(ctx, current, published)
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

	// A policy preference is not an ordinary invalidation: the existing channel
	// may still be perfectly watchable while another known pool candidate is now
	// semantically stronger. Reconsider only that strict relation on every
	// proposal so a stronger candidate added by a later pool refresh can
	// converge without another rank publication. Equal or weaker ranks retain
	// current continuity, and the local pool is enough — no directory resync is
	// required merely to apply Campaign Policy.
	if next := m.selectStrictlyStrongerWithPolicy(ctx, current, published); next != nil {
		previous := current
		m.setCurrent(next)
		game, _, viewers, _ := m.channelFacts(next)
		slog.Info("Switching discovered channel",
			"from", previous.Streamer.GetUsername(),
			"to", next.Streamer.GetUsername(),
			"game", game,
			"viewers", viewers,
			"reason", "Campaign Policy assigned strictly stronger bounded campaign utility")
		events.Record(events.TypeDiscoverySwitched, next.Streamer.GetUsername(),
			"from "+previous.Streamer.GetUsername()+": stronger bounded Campaign Policy utility")
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
	if availability := ch.Streamer.Stream.ProvisionalDropSnapshot().Availability.State; availability == models.CampaignAvailabilityKnown {
		if !m.channelCarriesActiveCampaign(ch) {
			return "channel no longer carries an active unclaimed drop campaign", true
		}
	} else {
		if len(ch.Streamer.Stream.GetCampaigns()) != 0 {
			// Preserve the pre-existing UNKNOWN invalidation behavior: derive the
			// empty authoritative intersection and clear the prior assignment.
			m.channelCarriesActiveCampaign(ch)
			return "channel no longer carries an active unclaimed drop campaign", true
		}
		if _, eligible := m.provisionalCandidateForChannel(ch, m.currentCampaignPolicy()); !eligible {
			return "channel has no bounded provisional UNKNOWN drops observation", true
		}
	}
	if m.isAvoided(ch.Streamer.GetUsername()) {
		return "temporarily excluded by the drop-progress watchdog (stalled progress)", true
	}
	return "", false
}

// selectBest returns the best watchable candidate from the pool under the
// latest policy ordering. Game ranks are only a cheap directory/check pre-order:
// final selection uses each verified channel's exact advertised campaign IDs,
// active tracker campaigns and channel ACL. Within equal exact bounded
// utility, configured game order and the pool's existing per-game
// subscribed/viewer ordering remain stable. exclude (may be nil) is the
// channel being switched away from. At most maxCandidateChecksPerTick
// candidates are brought online per call to bound the API burst; the rest wait
// for the next tick.
func (m *Manager) selectBest(ctx context.Context, exclude *Channel) *Channel {
	return m.selectBestWithPolicy(ctx, exclude, m.currentCampaignPolicy())
}

func (m *Manager) selectBestWithPolicy(ctx context.Context, exclude *Channel, published *campaignPolicySnapshot) *Channel {
	order := newGamePolicyOrder(m.getGames(), policyGameRanks(published))
	return m.selectBestOrdered(ctx, exclude, order, published)
}

// selectStrictlyStrongerWithPolicy returns an eligible candidate only when its
// exact hard restricted/semantic campaign priority is strictly stronger than
// current's. Equal facts retain current continuity even if configured order,
// viewer count or subscription preference would select the other channel from
// scratch.
func (m *Manager) selectStrictlyStrongerWithPolicy(ctx context.Context, current *Channel, published *campaignPolicySnapshot) *Channel {
	order := newGamePolicyOrder(m.getGames(), policyGameRanks(published))
	next := m.selectBestOrdered(ctx, current, order, published)
	if next == nil || !m.candidateStrictlyStronger(next, current, order, published) {
		return nil
	}
	return next
}

func policyGameRanks(published *campaignPolicySnapshot) map[string]int {
	if published == nil {
		return nil
	}
	return published.gameRanks
}

type orderedCandidate struct {
	channel  *Channel
	priority gamePolicyPriority
}

type evaluatedCandidate struct {
	orderedCandidate
	facts       watcher.CandidateCampaignPolicy
	provisional *provisionalCampaignEvaluation
}

// evaluateCandidate keeps the authoritative and provisional paths disjoint.
// Known availability uses the pre-existing assignment/policy derivation;
// UNKNOWN can return only a pure observation tuple.
func (m *Manager) evaluateCandidate(ch *Channel, published *campaignPolicySnapshot) (watcher.CandidateCampaignPolicy, *provisionalCampaignEvaluation, bool) {
	if ch.Streamer.Stream.ProvisionalDropSnapshot().Availability.State == models.CampaignAvailabilityKnown {
		facts, eligible := m.candidatePolicyFacts(ch, published)
		return facts, nil, eligible
	}
	if len(ch.Streamer.Stream.GetCampaigns()) != 0 {
		// Keep the old UNKNOWN behavior for an already-confirmed assignment:
		// candidatePolicyFacts clears it through SetCampaigns(nil), and no
		// provisional candidate is admitted in the same evaluation.
		m.candidatePolicyFacts(ch, published)
		return watcher.CandidateCampaignPolicy{}, nil, false
	}
	provisional, eligible := m.provisionalCandidateForChannel(ch, published)
	return watcher.CandidateCampaignPolicy{}, provisional, eligible
}

func (m *Manager) selectBestOrdered(ctx context.Context, exclude *Channel, order gamePolicyOrder, published *campaignPolicySnapshot) *Channel {
	m.mu.RLock()
	pool := make([]orderedCandidate, 0, len(m.pool))
	for _, ch := range m.pool {
		pool = append(pool, orderedCandidate{
			channel:  ch,
			priority: order.priority(ch.Game),
		})
	}
	m.mu.RUnlock()
	sort.SliceStable(pool, func(i, j int) bool {
		left, right := pool[i].priority, pool[j].priority
		if left.semantic != right.semantic {
			return left.semantic < right.semantic
		}
		return left.configured < right.configured
	})

	trackedOnly := m.trackedOnly()
	checks := 0
	var best *evaluatedCandidate
	for _, candidate := range pool {
		ch := candidate.channel
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
				break
			}
			checks++
			m.runRoutineRefresh(ctx, ch.Streamer)
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
		facts, provisional, eligible := m.evaluateCandidate(ch, published)
		if !eligible {
			continue
		}

		if v := ch.Streamer.Stream.GetViewersCount(); v > 0 {
			m.mu.Lock()
			ch.Viewers = v
			m.mu.Unlock()
		}
		candidate.priority = order.priority(game)
		evaluated := evaluatedCandidate{orderedCandidate: candidate, facts: facts, provisional: provisional}
		if best == nil || betterExactCandidate(evaluated, *best, published) {
			copy := evaluated
			best = &copy
		}
	}
	if best == nil {
		return nil
	}
	return best.channel
}

// candidateStrictlyStronger compares only scheduler semantics: restricted hard
// class, then exact per-channel bounded Campaign Policy utility. Configured/viewer/
// subscription ordering is deliberately excluded so equal semantic facts keep
// the valid current proposal.
func (m *Manager) candidateStrictlyStronger(
	candidate, current *Channel,
	order gamePolicyOrder,
	published *campaignPolicySnapshot,
) bool {
	candidateFacts, candidateProvisional, candidateEligible := m.evaluateCandidate(candidate, published)
	currentFacts, currentProvisional, currentEligible := m.evaluateCandidate(current, published)
	if !candidateEligible {
		return false
	}
	if !currentEligible {
		return true
	}
	candidateGame, _, _, _ := m.channelFacts(candidate)
	currentGame, _, _, _ := m.channelFacts(current)
	return compareEvaluatedCandidatePolicy(
		candidateFacts, candidateProvisional,
		currentFacts, currentProvisional,
		order.priority(candidateGame), order.priority(currentGame),
		published,
	) < 0
}

func betterExactCandidate(candidate, current evaluatedCandidate, published *campaignPolicySnapshot) bool {
	if cmp := compareEvaluatedCandidatePolicy(
		candidate.facts, candidate.provisional,
		current.facts, current.provisional,
		candidate.priority, current.priority,
		published,
	); cmp != 0 {
		return cmp < 0
	}
	// Exact semantic equality is a genuine new-selection tie: retain the
	// configured cross-game order and then the stable pool order, which already
	// carries preferSubscribed/viewer ordering inside each game.
	return candidate.priority.configured < current.priority.configured
}

// compareEvaluatedCandidatePolicy returns negative when left is stronger. A
// still-unproved provisional tuple remains below every confirmed candidate. An
// exact broker-proved tuple, however, has direct server authority and therefore
// enters the existing restricted/active + Campaign Policy comparison instead
// of being switched away before the broker can consume that proof.
func compareEvaluatedCandidatePolicy(
	leftFacts watcher.CandidateCampaignPolicy,
	leftProvisional *provisionalCampaignEvaluation,
	rightFacts watcher.CandidateCampaignPolicy,
	rightProvisional *provisionalCampaignEvaluation,
	leftGame, rightGame gamePolicyPriority,
	published *campaignPolicySnapshot,
) int {
	leftAuthoritative := leftProvisional == nil || leftProvisional.proved
	rightAuthoritative := rightProvisional == nil || rightProvisional.proved
	if leftAuthoritative != rightAuthoritative {
		if leftAuthoritative {
			return -1
		}
		return 1
	}
	if !leftAuthoritative {
		return compareProvisionalCampaigns(*leftProvisional, *rightProvisional)
	}
	if leftProvisional != nil {
		leftFacts = provisionalCampaignPolicy(*leftProvisional)
	}
	if rightProvisional != nil {
		rightFacts = provisionalCampaignPolicy(*rightProvisional)
	}
	return compareCandidatePolicy(leftFacts, rightFacts, leftGame, rightGame, published)
}

func provisionalCampaignPolicy(evaluation provisionalCampaignEvaluation) watcher.CandidateCampaignPolicy {
	return watcher.CandidateCampaignPolicy{
		Utility:                  evaluation.utility,
		Ranked:                   evaluation.ranked,
		Restricted:               evaluation.restricted,
		Campaign:                 evaluation.candidate.Campaign,
		CampaignIDs:              []string{evaluation.candidate.CampaignID},
		RemainingWorkCampaignIDs: []string{evaluation.candidate.CampaignID},
	}
}

// compareCandidatePolicy returns negative when left is semantically stronger.
// With a production exact campaign map, game ranks never decide the result;
// they are only a directory/check pre-order. The rank-only compatibility seam
// falls back to game semantics, preserving historical GAME_ORDER tests.
func compareCandidatePolicy(
	left, right watcher.CandidateCampaignPolicy,
	leftGame, rightGame gamePolicyPriority,
	published *campaignPolicySnapshot,
) int {
	if left.Restricted != right.Restricted {
		if left.Restricted {
			return -1
		}
		return 1
	}
	if published != nil && published.campaignSemantics != nil {
		if left.Ranked != right.Ranked {
			if left.Ranked {
				return -1
			}
			return 1
		}
		if left.Ranked {
			return -policy.CompareSemanticUtility(left.Utility, right.Utility)
		}
		return 0
	}
	if leftGame.semantic < rightGame.semantic {
		return -1
	}
	if leftGame.semantic > rightGame.semantic {
		return 1
	}
	return 0
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
