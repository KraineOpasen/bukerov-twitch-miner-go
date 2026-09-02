package watcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/eligibility"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/journal"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/policy"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/twitch"
)

// onlineChecker is the slice of the Twitch client the watcher needs to
// re-verify a stale stream; narrowed to an interface so the broker's send loop
// can be tested with a fake. Satisfied by *twitch.TwitchClient.
//
// It deliberately asks for the CONTEXT form: the broker's two online checks
// belong to the watch generation and must stop when it does. The plain
// CheckStreamerOnline keeps its own background ownership for pubsub, the
// miner's stream-check loop, the streamer manager and the health canary, none
// of which this watcher may cancel. Directory discovery is deliberately NOT on
// that list: its candidate re-verification runs on this loop goroutine (see
// CandidateSource) and so takes the generation context too.
type onlineChecker interface {
	CheckStreamerOnlineContext(ctx context.Context, streamer *models.Streamer) models.StatusTransition
}

// minuteReporter abstracts MinuteSender so the loop can be exercised in tests
// without real HTTP. Satisfied by *MinuteSender. The ctx is the watch
// generation's: a send outlives neither its generation nor its slot.
type minuteReporter interface {
	Send(ctx context.Context, streamer *models.Streamer) SendResult
}

// MinuteWatcher is the unified slot broker: the single owner of the (at most
// constants.MaxSimultaneousStreams) Twitch watch slots. It selects channels
// from the configured streamer list AND from registered candidate sources
// (directory discovery), then is the only component that drives MinuteSender.
// Directory discovery never sends minute-watched itself; it only proposes
// candidates the broker may place in a slot.
type MinuteWatcher struct {
	client     onlineChecker
	streamers  []*models.Streamer
	priorities []config.Priority
	settings   config.RateLimitSettings
	// routineRefreshAfter is a per-instance deterministic test seam for the
	// configured-stream stale recheck. Zero preserves the production 10-minute
	// threshold.
	routineRefreshAfter time.Duration

	// pendingPriorities/pendingSettings/hasPending stage a runtime settings
	// update from UpdateSettings (any goroutine) under mu; the loop applies
	// them into priorities/settings at the start of the next tick. This keeps
	// priorities/settings loop-owned (read lock-free during selection) while
	// updates stay race-free.
	pendingPriorities []config.Priority
	pendingSettings   config.RateLimitSettings
	hasPending        bool

	// pendingStreamers/hasPendingStreamers stage a runtime replacement of the
	// configured streamer list (Settings-page add/remove) the same way:
	// UpdateStreamers (any goroutine) stages under mu, the loop swaps it into
	// the loop-owned streamers field at the start of the next tick — after
	// remapping every index-keyed piece of loop state (see applyStreamerList).
	pendingStreamers    []*models.Streamer
	hasPendingStreamers bool

	// sources supply extra watch candidates (e.g. directory discovery) that
	// compete for the same slots as the configured list. Guarded by mu; set at
	// startup and snapshotted at the start of each tick.
	sources []CandidateSource

	// store persists accumulated watch time per streamer so rotation
	// fairness survives restarts. May be nil (e.g. analytics disabled), in
	// which case rotation falls back to in-memory recency only.
	store *WatchTimeStore

	// rotation is only ever read/written from the loop() goroutine, so it
	// needs no locking of its own.
	rotation rotationState

	// streakDiag tracks, per streamer index, which watch-streak pursuit events
	// have already been logged for the current streak, so each is logged at
	// most once instead of every tick. Like rotation, it is only touched from
	// the loop() goroutine and needs no locking.
	streakDiag map[int]streakDiagState

	// selectionReasons and selectionMode are per-tick scratch state for the
	// debug snapshot, rebuilt on every processWatching pass. Like rotation,
	// they are only touched from the loop() goroutine; the copy other
	// goroutines may read is debugState below.
	selectionReasons map[int]string
	selectionMode    string

	// lastSlots is the previous tick's slot allocation (login -> reason code +
	// broadcast ID), loop-owned, used to log slot changes only when they
	// actually change. The broadcast ID is captured alongside the reason so a
	// "released" log can name the broadcast the slot was on, even though that
	// streamer is no longer in this tick's slots slice by then.
	lastSlots map[string]slotLogState

	// lastConfiguredWatched is the set of configured channels (login -> the
	// streamer that held the slot) that occupied a watch slot on the PREVIOUS
	// tick. Each tick, any configured channel watched last tick but NOT in this
	// tick's slots has genuinely lost its slot, so its continuous-watch accumulator
	// is reset (see resetLostSlotContinuity) — otherwise regaining the slot within
	// maxContinuousGap would credit the unwatched interval and reach the
	// streak-pursuit cap early. Loop-owned; login-keyed, so (like reportStats) it
	// carries no index and needs no remapping in applyStreamerList.
	lastConfiguredWatched map[string]*models.Streamer

	// displaceParity alternates the displacement victim in the pure
	// cold-start tie case (equal-rank configured occupants with no rotation
	// recency), so neither channel is starved for the whole uptime. Loop-owned;
	// only touched from pickDisplaceable during a processWatching tick.
	displaceParity uint64

	// slotJournal is the optional bounded diagnostic journal of watch-slot
	// lifecycle transitions (BKM-013). Nil disables all slot journaling, making
	// every instrumentation hook an immediate no-op — selection and sending
	// behave identically to a build without it. Injected via SetSlotJournal.
	// Appended only from the loop() goroutine; the journal carries its own lock
	// for the cross-goroutine debug reader.
	slotJournal *journal.Journal[journal.SlotEvent]

	// slotResidence tracks, per currently-slotted login, when the residence
	// began and its running transport-delivery counters, so terminal journal
	// events carry residence duration and success/failure totals. Loop-owned
	// (only journalSlotTransitions / recordSlotDelivery touch it); lazily created
	// and populated only while slotJournal is set.
	slotResidence map[string]*slotResidence

	// debugState is the last published watch-decision snapshot, guarded by
	// debugMu because the debug HTTP endpoint reads it from its own goroutine.
	debugMu    sync.Mutex
	debugState DebugState

	// brokerSnapshot/watchingLogins publish the immutable slot allocation for
	// the dashboard, the debug endpoint, and discovery to read lock-free.
	brokerSnapshot atomic.Pointer[BrokerSnapshot]
	watchingLogins atomic.Pointer[map[string]bool]

	// observationMu is the short, dedicated ownership lock for provisional
	// exact-Drop leases and beacon permits. It is never held across a network
	// call. Selection publishes a whole lease snapshot while health and probe
	// goroutines use the public methods in provisional.go.
	observationMu             sync.Mutex
	provisionalMonitoring     bool
	provisionalLease          *ProvisionalLease
	provisionalLeaseStreamer  *models.Streamer
	provisionalLeasePublished atomic.Pointer[ProvisionalLease]
	provisionalLeaseSeq       uint64
	observationPermitSeq      uint64
	observationPermits        map[uint64]observationPermitRecord
	// routineRefreshes counts in-flight routine metadata/status refreshes by
	// exact Streamer object. Entries are registered under observationMu before
	// network I/O and removed afterwards, so provisional lease admission and
	// refresh start have one linearization point without holding a lock across
	// the request.
	routineRefreshes          map[*models.Streamer]uint64
	provisionalBootstrapReady bool
	provisionalQuarantine     provisionalQuarantineState
	provisionalProofs         map[string]provisionalProofRecord
	quarantineFenceLeaseID    uint64
	quarantineFenceRun        uint64
	quarantineFenceDrainAt    time.Time

	// refresher rebuilds a slotted channel's watch session (spade URL, stream
	// info, beacon payload) for the staged session-refresh requests. Set once at
	// construction; nil only in tests that never exercise refreshes.
	refresher sessionRefresher

	// pendingRefresh stages correlated watch-session refresh requests from
	// RequestSessionRefresh (any goroutine) under mu, coalesced per login; the
	// loop drains and executes them at the start of each tick, keeping the loop
	// goroutine the single writer of slotted channels' watch sessions.
	pendingRefresh map[string]SessionRefreshRequest

	// sessionConverge is the loop-owned per-slotted-login bookkeeping for
	// convergeIncompleteSlotSessions (session.go): which incomplete broadcast
	// identity a login is chasing a spade URL for, how many bounded spade-fetch
	// attempts have been staged for it, and the backoff anchor. Like
	// rotation/slotResidence it is touched only by the loop goroutine and needs
	// no locking; pruned to the current tick's slots at the top of every
	// convergeIncompleteSlotSessions call, so a slot release/replacement
	// immediately invalidates the old convergence ownership.
	sessionConverge map[string]*sessionConvergeState

	// refreshObsSeq mints a unique, monotonic observation id per refresh
	// execution so every published outcome is distinguishable for correlation.
	refreshObsSeq atomic.Uint64

	// refreshOutcomes publishes the last session-refresh outcome per login for
	// the progress watchdog and the debug snapshot to read lock-free.
	refreshOutcomes atomic.Pointer[map[string]SessionRefreshOutcome]

	// reportStats is the loop-owned per-channel minute-watched delivery
	// accounting for currently slotted channels; reportStatsSnap is its
	// published immutable copy (see session.go).
	reportStats     map[string]ReportStats
	reportStatsSnap atomic.Pointer[map[string]ReportStats]

	// avoid, when set, temporarily excludes channels from watch selection (the
	// progress watchdog's channel-switch recovery stage). Guarded by mu;
	// snapshotted at the start of each tick.
	avoid AvoidChecker

	// campaignScores is the pre-existing raw-score publication surface retained
	// for source compatibility with session.go. Production allocation no longer
	// reads it: raw policy points are not comparable across modes and must never
	// be mixed with persisted watch minutes.
	campaignScores atomic.Pointer[map[string]int]

	// campaignSemanticPolicy publishes the policy engine's per-login configured
	// projection and exact per-campaign semantic facts as one immutable object.
	// The broker captures one pointer at the start of each allocation tick; every
	// configured/discovery comparison therefore belongs to the same decision set.
	campaignSemanticPolicy atomic.Pointer[campaignSemanticSnapshot]

	// rewardSkips is the operator's effective farming-exclusion decision
	// (DropRule.Skip entries), published by the miner alongside the semantic
	// policy. Slot admission consults it as the watch-side fail-safe: even if
	// an assignment writer upstream forgot to pre-filter, a channel justified
	// ONLY by a skipped reward's campaign never earns a new watch slot. The
	// stored value is immutable; nil excludes nothing.
	rewardSkips atomic.Pointer[models.RewardSkips]
	// activeCampaignSemanticPolicy is non-nil only while one processWatching
	// allocation is in progress. Discovery's concurrent sync goroutine may read
	// it too, so it is atomic; receiving the just-captured snapshot for that short
	// interval is safe and keeps every current broker proposal on one generation.
	activeCampaignSemanticPolicy atomic.Pointer[campaignSemanticSnapshot]
	// discoveryCandidatePolicy is the exact source publication made inside the
	// same WatchCandidates call that returns the proposal. It closes the window
	// where a newly verified ephemeral channel was absent from the miner's prior
	// per-login snapshot. Only cross-source arbitration reads it; configured and
	// discovery ordinals are both resolved through campaignSemanticPolicy.
	discoveryCandidatePolicy atomic.Pointer[discoveryCandidatePolicySnapshot]

	// preferConfigured, when true, forbids a non-configured (discovery)
	// candidate from displacing a configured streamer that already holds a slot,
	// so tracked streamers always keep their slot and discovery only fills idle
	// ones. Set from any goroutine, read lock-free by the loop's pickDisplaceable.
	// Default false preserves the pre-existing rank-based arbitration. Advisory
	// only — it never lets discovery exceed the slot cap or take an occupied slot.
	preferConfigured atomic.Bool

	// ctx/cancel record the current generation's context. Production code must
	// NOT read ctx: the loop and everything it starts take the context as an
	// explicit parameter (Start captures it once and threads it down), so a
	// helper reaching for this field would get a retired generation's cancelled
	// context after a restart, or nil on a watcher built without Start. It is
	// kept because Stop needs cancel, and because the broker tests install a
	// generation context through it.
	ctx    context.Context
	cancel context.CancelFunc
	// loopDone is closed when the watch loop goroutine exits; Stop waits on
	// it (bounded by stopJoinTimeout) so in-flight watch_time writes drain
	// before the database is closed.
	loopDone chan struct{}

	// sender performs the actual watch-minute reporting (playback token,
	// playlist touch, spade event). The broker is the sole caller of it.
	sender minuteReporter

	// pacer spaces the per-slot sends across the tick interval. nil uses the
	// default context-aware sleep with ±20% jitter; tests override it to avoid
	// real pauses. Returns false when the wait was interrupted by shutdown.
	pacer func(d time.Duration) bool

	// onMinuteWatched, if set, is invoked once after any watch tick that
	// successfully reported at least one minute-watched. The drops tracker uses
	// it to refresh drop progress promptly (a watched minute means real progress
	// was just made) instead of waiting out its sync interval. Guarded by mu.
	onMinuteWatched func()
	// onWatchStreakTransition persists the immutable Stream snapshot returned by
	// the one atomic timeout transition. It is copied under mu and invoked after
	// Stream.mu has been released.
	onWatchStreakTransition func(*models.Streamer, models.WatchStreakPersistence)

	// lostMiningMinutes accumulates estimated "idle slot" watch time for the
	// daily summary: per tick, wall-clock minutes for slots that were fillable
	// (a live eligible candidate existed) but produced no watched minute this
	// tick. It counts only genuine lost capacity — a slot left empty because
	// nothing was online is NOT counted. It is an in-memory, process-lifetime
	// best-effort figure: LostMiningMinutes drains it, and a restart resets it.
	// Guarded by lostMu (a distinct lock so the daily-summary goroutine's drain
	// never contends with the loop's mu-protected state).
	lostMu            sync.Mutex
	lostMiningMinutes float64

	mu sync.RWMutex
}

// rotationState tracks the fair watch-pair rotation used when more streamers
// are online than Twitch allows to watch simultaneously
// (constants.MaxSimultaneousStreams). See selectRotating for the algorithm.
type rotationState struct {
	activePair [2]int // streamer indexes currently occupying the watch slots
	hasPair    bool   // whether activePair has been initialized yet

	lastSwitch time.Time // when activePair last actually changed

	lastWatched map[int]time.Time // last tick each streamer index was actually watched (fairness tie-break + boost victim selection)

	// A near-streak swap-out may be deferred once for the current pair
	// approach. deferUntil is the explicit, bounded deadline; deferUsed stays
	// armed after expiry so repeated broker evaluations cannot extend it. Both
	// reset only after an actual pair change.
	deferUntil    time.Time
	deferStreamer int
	deferUsed     bool
	// deficitMinutes is the persisted WatchTimeStore snapshot that produced the
	// current base pair, keyed by login so a runtime streamer-list reorder needs
	// no remap. It is evidence for equal-semantic-class arbitration, never a
	// second fairness authority.
	deficitMinutes map[string]float64

	// Boost latch: keep the SAME channel in the ephemeral DROPS/STREAK boost
	// seat (and displace the SAME base-pair member) across ticks when continuity
	// is functionally required. A streak that is ELIGIBLE (so it can bootstrap
	// its first delivered minute) or PURSUING may survive an equal-class base-pair
	// reconciliation so MinuteWatched is not reset before the grant or bounded
	// timeout. An ordinary drop may remain only while its
	// current hard/semantic facts strictly outrank the fair pair; equality returns
	// ownership to persisted-deficit fairness. The latch yields immediately to a
	// strictly higher-priority candidate (see strictlyHigherBoost), so a
	// channel-restricted drop can still preempt it.
	boostLatched bool
	boostTarget  int // eligible streamer held for continuity; it may later enter the fair base pair
	boostVictim  int // displaced base member, or -1 while target itself belongs to the base pair
}

// clearBoostLatch drops any sticky boost so the next tick re-picks a boost seat
// from scratch. Base-pair reconciliation deliberately does not call it: boost
// continuity is independent of changes in the persisted-fairness base pair.
func (r *rotationState) clearBoostLatch() {
	r.boostLatched = false
	r.boostTarget = -1
	r.boostVictim = -1
}

func (r *rotationState) clearActiveDeferral() {
	r.deferUntil = time.Time{}
	r.deferStreamer = 0
}

func (r *rotationState) resetDeferralApproach() {
	r.clearActiveDeferral()
	r.deferUsed = false
}

// streakDiagState records which watch-streak pursuit log lines have already
// been emitted for a streamer's current (still-missing) streak.
type streakDiagState struct {
	broadcastID string
	pursuing    bool // "Pursuing watch streak" already logged
	released    bool // Stream-owned 20m bounded-timeout release already logged
}

// streakExpectedGrantMinutes is roughly how many CONTINUOUSLY-watched minutes it
// normally takes Twitch to grant a still-earnable watch streak. It is a
// DIAGNOSTIC reference only and causes no state transition.
const streakExpectedGrantMinutes = 15.0

// streakPursuitCapMinutes is an alias of the Stream owner's single behavioral
// hard cap. It is not reconstructed from diagnostic timing references.
const streakPursuitCapMinutes = models.WatchStreakPursuitCapMinutes

// StreakPursuitCapMinutes is the UI-facing hard pursuit cap, in continuously
// watched minutes: the Stream-owned bounded window after which the streak boost
// seat is released with outcome UNKNOWN. The web
// dashboard reads it as the watch-streak progress-bar denominator, so the UI and
// the watcher share one 20-minute source of truth instead of a hardcoded copy. It
// is the pursuit/watch window, NOT a promise that a reward is delivered at minute
// 20 — see streakPursuitCapMinutes above for the full semantics.
const StreakPursuitCapMinutes = streakPursuitCapMinutes

func NewMinuteWatcher(
	client *twitch.TwitchClient,
	streamers []*models.Streamer,
	priorities []config.Priority,
	settings config.RateLimitSettings,
	store *WatchTimeStore,
) *MinuteWatcher {
	return &MinuteWatcher{
		client:     client,
		streamers:  streamers,
		priorities: priorities,
		settings:   settings,
		store:      store,
		sender:     NewMinuteSender(client),
		refresher:  client,
	}
}

// AddSource registers a candidate source (e.g. directory discovery) whose
// proposed channels compete for the same watch slots as the configured list.
// Call before Start. Safe for concurrent use.
func (w *MinuteWatcher) AddSource(src CandidateSource) {
	if src == nil {
		return
	}
	w.mu.Lock()
	w.sources = append(w.sources, src)
	w.mu.Unlock()
}

// SetRewardSkips publishes the operator's effective farming-exclusion
// decision (DropRule.Skip entries). The producer may call this concurrently
// with the broker loop; pass nil to clear. The decision is consulted at slot
// admission so a drop-only-justified channel whose campaign's current drop is
// skipped never earns a new watch slot, independent of upstream assignment
// filtering.
func (w *MinuteWatcher) SetRewardSkips(skips *models.RewardSkips) {
	w.rewardSkips.Store(skips)
}

// SetCampaignSemanticClasses publishes immutable per-login ordinal facts from
// Campaign Policy. Lower classes are stronger; pass nil to clear. The producer
// may call this concurrently with the broker loop.
func (w *MinuteWatcher) SetCampaignSemanticClasses(classes map[string]policy.SemanticClass) {
	if classes == nil {
		w.SetCampaignSemanticPolicy(nil, nil, nil)
		return
	}
	utilities := make(map[string]policy.SemanticUtility, len(classes))
	for login, class := range classes {
		utilities[login] = policy.SemanticUtility{SemanticClass: class}
	}
	w.SetCampaignSemanticPolicy(utilities, nil, nil)
}

// SetCampaignSemanticPolicy atomically publishes both source-owned projections
// from one Campaign Policy evaluation. Inputs are cloned so the stored snapshot
// stays immutable while refreshPolicy builds and publishes later evaluations.
func (w *MinuteWatcher) SetCampaignSemanticPolicy(
	byLogin map[string]policy.SemanticUtility,
	byCampaign map[string]policy.CampaignSemantic,
	gameRanks map[string]int,
) {
	if byLogin == nil && byCampaign == nil && gameRanks == nil {
		w.campaignSemanticPolicy.Store(nil)
		return
	}
	w.campaignSemanticPolicy.Store(&campaignSemanticSnapshot{
		byLogin:    cloneSemanticUtilities(byLogin),
		byCampaign: cloneCampaignSemantics(byCampaign),
		gameRanks:  cloneSemanticGameRanks(gameRanks),
	})
}

func cloneSemanticUtilities(source map[string]policy.SemanticUtility) map[string]policy.SemanticUtility {
	if source == nil {
		return nil
	}
	cloned := make(map[string]policy.SemanticUtility, len(source))
	for key, utility := range source {
		cloned[key] = utility
	}
	return cloned
}

func cloneCampaignSemantics(source map[string]policy.CampaignSemantic) map[string]policy.CampaignSemantic {
	if source == nil {
		return nil
	}
	cloned := make(map[string]policy.CampaignSemantic, len(source))
	for key, fact := range source {
		cloned[key] = fact
	}
	return cloned
}

func cloneSemanticGameRanks(source map[string]int) map[string]int {
	if source == nil {
		return nil
	}
	cloned := make(map[string]int, len(source))
	for key, rank := range source {
		cloned[key] = rank
	}
	return cloned
}

// DiscoveryCampaignPolicy returns the immutable semantic snapshot that
// discovery must use for candidate ordering. During WatchCandidates it is the
// broker-active tick snapshot; outside a broker tick it is the latest complete
// miner publication. Callers must treat both maps as read-only.
func (w *MinuteWatcher) DiscoveryCampaignPolicy() (map[string]int, map[string]policy.CampaignSemantic) {
	snapshot := w.campaignSemanticSnapshotForTick()
	if snapshot == nil {
		return nil, nil
	}
	return snapshot.gameRanks, snapshot.byCampaign
}

// SetDiscoveryCandidatePolicy publishes discovery's exact verified campaign
// facts for the one proposal it is about to return. Clearing with an empty
// login prevents stale proposal facts. Discovery calls this synchronously from
// WatchCandidates on the broker loop goroutine; the atomic snapshot also keeps
// tests and future caller order race-safe.
func (w *MinuteWatcher) SetDiscoveryCandidatePolicy(login string, facts CandidateCampaignPolicy) {
	if login == "" {
		w.discoveryCandidatePolicy.Store(nil)
		return
	}
	facts.CampaignIDs = append([]string(nil), facts.CampaignIDs...)
	facts.RemainingWorkCampaignIDs = append([]string(nil), facts.RemainingWorkCampaignIDs...)
	w.discoveryCandidatePolicy.Store(&discoveryCandidatePolicySnapshot{login: login, facts: facts})
}

// stopJoinTimeout bounds how long Stop waits for the watch loop to drain its
// in-flight tick (which may be writing watch_time rows) before giving up so a
// hung loop can never block shutdown indefinitely. Package variable so tests
// can shrink it.
var stopJoinTimeout = 5 * time.Second

// ErrStopJoinTimeout reports a DIRTY watcher teardown: the bounded join in Stop
// expired while generation-owned work was still running, so the old generation
// is NOT quiescent and its goroutine may still touch the store, the streamers
// and the Twitch client. It is the watcher's contribution to the existing
// dirty-teardown classification — internal/miner folds it into the drain error
// its own errLoopJoinTimeout sentinel wraps, which internal/lifecycle already
// recognises through miner.IsJoinTimeoutError, so a generation that did not
// quiesce is never retired as though it were gone. No second lifecycle
// controller, scheduler or generation counter is introduced for it.
var ErrStopJoinTimeout = errors.New("watcher: watch generation did not quiesce within the stop join timeout")

// ErrGenerationLive reports a refused Start: this instance's previous watch
// generation has not quiesced, so admitting a new one would let two generations
// race over the same slots, streamers and store. The caller must join the
// previous generation (Stop returning nil) before starting another.
var ErrGenerationLive = errors.New("watcher: a watch generation is already live on this watcher")

// Start runs one watch generation: it derives the generation context from ctx
// and spawns the single loop goroutine that owns every watch-generation
// operation. It refuses to start a second generation while a previous one is
// still live — silently overwriting ctx/cancel/loopDone would orphan the first
// loop and let two generations mine the same slots against the same store.
func (w *MinuteWatcher) Start(ctx context.Context) error {
	w.mu.Lock()
	if prev := w.loopDone; prev != nil {
		select {
		case <-prev:
			// The previous generation joined: its state is safe to replace.
		default:
			w.mu.Unlock()
			return ErrGenerationLive
		}
	}
	w.ctx, w.cancel = context.WithCancel(ctx)
	loopCtx := w.ctx
	done := make(chan struct{})
	w.loopDone = done
	w.mu.Unlock()

	go func() {
		defer close(done)
		w.loop(loopCtx)
	}()
	return nil
}

// Stop cancels the watch generation and joins it, bounded by stopJoinTimeout,
// so an in-flight tick's watch_time write completes before the caller proceeds
// to close the database.
//
// It returns nil for a CLEAN stop — the generation observed cancellation and
// every operation it owned has finished, so nothing of it is still running when
// Stop returns. It returns ErrStopJoinTimeout for a DIRTY teardown: the join
// deadline expired with owned work still in flight. That error is not cosmetic:
// the caller (internal/miner) folds it into the shutdown drain error, which
// internal/lifecycle classifies as a dirty teardown, so a generation that never
// quiesced stays known as such instead of being retired silently.
func (w *MinuteWatcher) Stop() error {
	// Stop accepting provisional ownership before joining the loop. Any
	// already-running network call remains outside observationMu and may finish,
	// so its permit deliberately remains live until ReleaseObservationPermit;
	// a later re-enable cannot forget transport that outlived the bounded join.
	w.observationMu.Lock()
	w.provisionalMonitoring = false
	w.clearProvisionalLeaseLocked()
	w.provisionalProofs = nil
	w.provisionalQuarantine = provisionalQuarantineState{
		namespace:       w.provisionalQuarantine.namespace,
		enforceAccepted: w.provisionalQuarantine.enforceAccepted,
	}
	w.observationMu.Unlock()

	w.mu.Lock()
	if w.cancel != nil {
		w.cancel()
	}
	done := w.loopDone
	w.mu.Unlock()

	if done == nil {
		return nil
	}
	timer := time.NewTimer(stopJoinTimeout)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
		slog.Warn("Watcher loop did not finish within the stop timeout; teardown is dirty and the watch generation is still live",
			"timeout", stopJoinTimeout)
		return fmt.Errorf("%w after %s", ErrStopJoinTimeout, stopJoinTimeout)
	}
}

// SetOnMinuteWatched registers a callback invoked after each watch tick that
// successfully reported at least one minute-watched. Pass nil to clear it.
func (w *MinuteWatcher) SetOnMinuteWatched(fn func()) {
	w.mu.Lock()
	w.onMinuteWatched = fn
	w.mu.Unlock()
}

// SetOnWatchStreakTransition registers the existing cache owner's persistence
// adapter for terminal streak transitions. Pass nil to clear it.
func (w *MinuteWatcher) SetOnWatchStreakTransition(fn func(*models.Streamer, models.WatchStreakPersistence)) {
	w.mu.Lock()
	w.onWatchStreakTransition = fn
	w.mu.Unlock()
}

// UpdateSettings stages a runtime priority/rate-limit change. It is applied by
// the loop goroutine at the start of the next tick (see applyPendingSettings),
// so priorities/settings stay loop-owned and readable without locking during
// selection, while the update itself is race-free.
func (w *MinuteWatcher) UpdateSettings(priorities []config.Priority, settings config.RateLimitSettings) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pendingPriorities = priorities
	w.pendingSettings = settings
	w.hasPending = true
}

// UpdateStreamers stages a runtime replacement of the configured streamer
// list (Settings-page add/remove). The loop goroutine applies it at the start
// of the next tick (see applyPendingSettings), so streamers stays loop-owned
// and readable without locking during selection. Two calls before a tick are
// last-write-wins: only the newest list is ever applied. The slice is copied
// so later caller-side mutations cannot reach loop state. A removed streamer
// is released softly: it simply stops being a slot candidate on the next
// tick, and the normal per-tick selection reassigns its slot.
func (w *MinuteWatcher) UpdateStreamers(streamers []*models.Streamer) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pendingStreamers = append([]*models.Streamer(nil), streamers...)
	w.hasPendingStreamers = true
}

// applyPendingSettings moves any staged runtime settings into the loop-owned
// priorities/settings fields. Runs on the loop goroutine at the start of each
// tick; also snapshots the registered sources and the avoid checker.
func (w *MinuteWatcher) applyPendingSettings() ([]CandidateSource, AvoidChecker) {
	w.mu.Lock()
	if w.hasPending {
		w.priorities = w.pendingPriorities
		w.settings = w.pendingSettings
		w.hasPending = false
	}
	var stagedStreamers []*models.Streamer
	applyStreamers := false
	if w.hasPendingStreamers {
		stagedStreamers = w.pendingStreamers
		w.pendingStreamers = nil
		w.hasPendingStreamers = false
		applyStreamers = true
	}
	sources := append([]CandidateSource(nil), w.sources...)
	avoid := w.avoid
	w.mu.Unlock()

	// The swap itself happens outside mu: streamers and the rotation/streak
	// state are loop-owned, and this runs on the loop goroutine.
	if applyStreamers {
		w.applyStreamerList(stagedStreamers)
	}
	return sources, avoid
}

// applyStreamerList replaces the loop-owned streamer list and remaps every
// index-keyed piece of loop state — rotation pair, boost latch, fairness
// recency (lastWatched), swap-out deferrals, and streak log bookkeeping —
// from old indexes to new ones by username. Entries whose streamer left the
// list are dropped. If a rotation-pair or boost seat member was removed, that
// state is reset so the next selection recomputes it from scratch; per-tick
// scratch (selectionReasons) needs nothing, it is rebuilt every tick.
// Username-keyed state (reportStats, pendingRefresh, lastConfiguredWatched,
// WatchTimeStore rows) is index-free and intentionally untouched. The
// per-allocation deficit snapshot is discarded and rebuilt from WatchTimeStore
// after the roster swap, so a removal/re-add cannot inherit stale evidence.
func (w *MinuteWatcher) applyStreamerList(newList []*models.Streamer) {
	oldList := w.streamers
	newIndexByLogin := make(map[string]int, len(newList))
	for i, s := range newList {
		newIndexByLogin[s.GetUsername()] = i
	}
	translate := func(oldIdx int) (int, bool) {
		if oldIdx < 0 || oldIdx >= len(oldList) {
			return -1, false
		}
		newIdx, ok := newIndexByLogin[oldList[oldIdx].GetUsername()]
		return newIdx, ok
	}

	if len(w.rotation.lastWatched) > 0 {
		remapped := make(map[int]time.Time, len(w.rotation.lastWatched))
		for oldIdx, at := range w.rotation.lastWatched {
			if newIdx, ok := translate(oldIdx); ok {
				remapped[newIdx] = at
			}
		}
		w.rotation.lastWatched = remapped
	}
	if !w.rotation.deferUntil.IsZero() {
		if newIdx, ok := translate(w.rotation.deferStreamer); ok {
			w.rotation.deferStreamer = newIdx
		} else {
			w.rotation.clearActiveDeferral()
		}
	}
	if len(w.streakDiag) > 0 {
		remapped := make(map[int]streakDiagState, len(w.streakDiag))
		for oldIdx, state := range w.streakDiag {
			if newIdx, ok := translate(oldIdx); ok {
				remapped[newIdx] = state
			}
		}
		w.streakDiag = remapped
	}

	if w.rotation.hasPair {
		a, okA := translate(w.rotation.activePair[0])
		b, okB := translate(w.rotation.activePair[1])
		if okA && okB {
			w.rotation.activePair = [2]int{a, b}
		} else {
			// A pair member was removed: drop the pair (and the boost seat that
			// references it) so this tick's selection recomputes both.
			w.rotation.hasPair = false
			w.rotation.resetDeferralApproach()
			w.rotation.clearBoostLatch()
		}
	}
	if w.rotation.boostLatched {
		target, okT := translate(w.rotation.boostTarget)
		victim, okV := -1, w.rotation.boostVictim == -1
		if !okV {
			victim, okV = translate(w.rotation.boostVictim)
		}
		if okT && okV {
			w.rotation.boostTarget = target
			w.rotation.boostVictim = victim
		} else {
			w.rotation.clearBoostLatch()
		}
	}

	// Deterministically drop login-keyed loop-owned state for streamers that
	// left the roster, so a removed streamer leaves no runtime residue and a
	// same-process re-add of the login starts clean (BKM-018A). Most of these
	// maps already self-prune to the current slots within a tick or two, but
	// doing it here — on the loop goroutine, their sole writer — closes the one
	// genuine lifetime leak (refreshOutcomes, a merged copy-on-write map never
	// otherwise pruned and user-visible via LastSessionRefresh) and releases the
	// *models.Streamer pointer lastConfiguredWatched would otherwise retain.
	// delete on a nil (lazily-created) map is a safe no-op.
	for _, s := range oldList {
		login := s.GetUsername()
		if _, kept := newIndexByLogin[login]; kept {
			continue
		}
		delete(w.lastSlots, login)
		delete(w.lastConfiguredWatched, login)
		delete(w.reportStats, login)
		delete(w.pendingRefresh, login)
		delete(w.sessionConverge, login)
		delete(w.slotResidence, login)
	}
	w.pruneRefreshOutcomes(newIndexByLogin)
	// A pointer-preserving rename may already have changed GetUsername on both
	// oldList and newList, so there is no reliable old key to remap here. Clearing
	// is safe: every contested production selection refreshes this evidence from
	// the persisted store before consulting it.
	w.rotation.deficitMinutes = nil

	w.streamers = newList
}

// pruneRefreshOutcomes drops published session-refresh outcomes for any login no
// longer in the roster (keys of current). Copy-on-write like publishRefreshOutcomes
// and, like it, runs only on the loop goroutine, so the atomic swap is the single
// writer's. A no-op when nothing must be dropped, so the common tick pays only a
// scan.
func (w *MinuteWatcher) pruneRefreshOutcomes(current map[string]int) {
	prev := w.refreshOutcomes.Load()
	if prev == nil || len(*prev) == 0 {
		return
	}
	drop := false
	for login := range *prev {
		if _, ok := current[login]; !ok {
			drop = true
			break
		}
	}
	if !drop {
		return
	}
	next := make(map[string]SessionRefreshOutcome, len(*prev))
	for login, o := range *prev {
		if _, ok := current[login]; ok {
			next[login] = o
		}
	}
	w.refreshOutcomes.Store(&next)
}

func (w *MinuteWatcher) randomizedDelay(base time.Duration) time.Duration {
	jitter := (rand.Float64() - 0.5) * 0.4
	return time.Duration(float64(base) * (1.0 + jitter))
}

// pace waits between two per-slot sends, spreading them across the tick
// interval (with ±20% jitter) while remaining responsive to shutdown. Returns
// false if the context was cancelled during the wait, so the send loop stops.
func (w *MinuteWatcher) pace(ctx context.Context, d time.Duration) bool {
	if w.pacer != nil {
		return w.pacer(d)
	}
	timer := time.NewTimer(w.randomizedDelay(d))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (w *MinuteWatcher) loop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		tickStart := time.Now()
		w.processWatching(ctx)

		// processWatching already spreads this tick's per-slot sends across
		// roughly one interval (pace(interval/len(slots)) after each slot), so a
		// continuously-watched channel is reported about once per interval. Sleep
		// only the REMAINDER of the interval here, never a second full one:
		// otherwise the effective per-channel cadence would be ~2×interval, which
		// sits right on the watch-streak continuity threshold
		// (maxContinuousGap = 2×interval, see processWatching) and halves the
		// drop-progress heartbeat rate. When processWatching returned early
		// without pacing (no slots watched), the elapsed time is ~0 and this waits
		// a full jittered interval, so the loop never busy-spins. Jitter is
		// preserved — it now lives on this single wait instead of being duplicated.
		interval := time.Duration(w.settings.MinuteWatchedInterval) * time.Second
		remaining := w.randomizedDelay(interval) - time.Since(tickStart)
		if remaining <= 0 {
			continue
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (w *MinuteWatcher) processWatching(ctx context.Context) {
	sources, avoid := w.applyPendingSettings()
	w.activeCampaignSemanticPolicy.Store(w.captureCampaignSemanticSnapshot())
	defer w.activeCampaignSemanticPolicy.Store(nil)

	w.selectionReasons = make(map[int]string)
	w.selectionMode = ModeIdle
	now := time.Now()

	onlineStreamers := w.getOnlineStreamers(avoid)

	// Re-verify stale streams (network) before selecting. RunRoutineRefresh
	// linearizes this routine caller with provisional lease admission while
	// leaving send-failure and explicit recovery refreshes below untouched.
	routineRefreshAfter := w.routineRefreshAfter
	if routineRefreshAfter == 0 {
		routineRefreshAfter = 10 * time.Minute
	}
	for _, idx := range onlineStreamers {
		if ctx.Err() != nil {
			// The generation is gone: start no further re-verification.
			return
		}
		if w.client != nil && w.streamers[idx].Stream.UpdateElapsed() > routineRefreshAfter {
			streamer := w.streamers[idx]
			w.RunRoutineRefresh(streamer, func() {
				w.client.CheckStreamerOnlineContext(ctx, streamer)
			})
		}
	}

	// Phase A: pick from the configured streamer list with the unchanged
	// priority/rotation logic. Phase B: layer external candidates (directory
	// discovery) on top and enforce the global MaxSimultaneousStreams cap.
	var configuredWatch []int
	if len(onlineStreamers) > 0 {
		configuredWatch = w.selectStreamersToWatch(onlineStreamers)
	}
	extra := w.gatherCandidates(ctx, sources, avoid)
	if ctx.Err() != nil {
		// The generation ended during candidate preparation. Continuing would
		// arbitrate on a candidate set the sources never actually produced, and
		// publish slot releases and a broker snapshot on behalf of a generation
		// that no longer exists. End the tick instead, like the two sibling
		// guards above and below.
		return
	}
	// Rotation refreshes this evidence itself. Direct mode normally needs no
	// fairness comparison, but two configured occupants plus an external
	// contender do: capture the same persisted deficit before cross-source
	// displacement so equal semantic facts keep the most-owed configured seat.
	if w.selectionMode != ModeRotation && len(configuredWatch) == constants.MaxSimultaneousStreams && len(extra) > 0 {
		w.refreshDeficitMinutes(onlineStreamers, now)
	}
	slots, waiting, provisionalContenders := w.arbitrateWithProvisionalContenders(configuredWatch, extra, now)
	slots, waiting = w.reconcileProvisionalSlots(slots, waiting, now, provisionalContenders)

	// The per-streamer debug state reflects the FINAL configured-watched set
	// (a pick displaced by a higher-priority discovery drop is reported as not
	// watched); the broker snapshot is the explainable slot allocation.
	w.publishDebugState(configuredWatchedIndexes(slots), w.selectionMode)
	w.publishBrokerSnapshot(slots, waiting, now)
	w.logSlotChanges(slots)

	// Break continuous-watch accumulation for any configured channel that just
	// lost its watch slot, so a slot regained within maxContinuousGap does not
	// credit the unwatched interval toward the streak-pursuit cap (see
	// resetLostSlotContinuity). Runs before the no-slots early return below, since
	// losing every slot is itself a continuity break.
	w.resetLostSlotContinuity(slots)

	// Stage a bounded, deduplicated, event-triggered convergence refresh for any
	// committed slot whose session is still missing a spade URL (see
	// convergeIncompleteSlotSessions for the full contract), so it can converge
	// instead of being stuck delivering nothing indefinitely. Must run BEFORE
	// executeSessionRefreshes so a refresh staged this tick can execute and
	// land this very tick.
	w.convergeIncompleteSlotSessions(slots, now)

	// Execute any staged watch-session refreshes before the sends, so a
	// successful refresh takes effect for this very tick. Requests for channels
	// that lost their slot complete as skipped.
	w.executeSessionRefreshes(ctx, slots)

	interval := time.Duration(w.settings.MinuteWatchedInterval) * time.Second

	// Slots that could have been productively filled this tick (granted slots
	// plus channels that contended for one). Used to estimate lost mining time.
	fillable := len(slots) + len(waiting)

	if len(slots) == 0 {
		// No slots granted: any contender that didn't get one is lost capacity.
		w.accrueLostMining(fillable, 0, interval)
		w.publishReportStats(slots)
		return
	}

	var watchingNames []string
	for _, sl := range slots {
		watchingNames = append(watchingNames, sl.streamer.GetUsername())
	}
	slog.Debug("Watching streams", "count", len(slots), "max", constants.MaxSimultaneousStreams, "streamers", watchingNames)

	sleepBetween := interval / time.Duration(len(slots))

	// A continuously-watched streamer is reported once per loop, so consecutive
	// reports land ~interval apart. Anything past twice that means it lost its
	// watch slot for at least a cycle - a break in continuity that resets the
	// watch-streak progress (see Stream.UpdateMinuteWatched).
	maxContinuousGap := 2 * interval

	reported := false
	watchedOK := 0
	for _, sl := range slots {
		if ctx.Err() != nil {
			// A cancelled generation must not NEWLY START a send — and so can
			// never newly start a minute-watched beacon it no longer owns. An
			// already-running send observes the same ctx inside the transport.
			return
		}
		streamer := sl.streamer
		leaseID := uint64(0)
		var (
			permit    ObservationPermit
			permitted bool
		)
		if sl.provisionalProven && sl.provisionalDrop != nil {
			permit, permitted = w.AcquireProvisionalProofPermit(streamer, sl.provisionalProofID, *sl.provisionalDrop)
		} else if sl.provisionalDrop != nil {
			lease, ok := w.ProvisionalLease()
			if !ok || !lease.Candidate.SameLeaseIdentity(*sl.provisionalDrop) {
				continue
			}
			leaseID = lease.LeaseID
			if lease.State == ProvisionalLeasePending {
				permit, permitted = w.acquireProvisionalBootstrapPermit(streamer, leaseID)
			} else {
				permit, permitted = w.AcquireObservationPermit(streamer, leaseID)
			}
		} else {
			permit, permitted = w.AcquireObservationPermit(streamer, leaseID)
		}
		if !permitted {
			// A stale/mismatched provisional owner or causally conflicting ordinary
			// send is suppressed without inventing a transport failure or successful-
			// delivery statistic. Pending sends are allowed only when a fresh complete
			// absence or exact-tuple UNKNOWN observation opened one bootstrap token.
			continue
		}

		res := w.sendMinuteWatched(ctx, streamer)
		w.completeObservationPermit(permit, res.Delivered)
		switch {
		case res.Cancelled:
			// The watch generation was cancelled mid-send. That is a teardown,
			// not a Twitch transport failure: do not note a failure outcome, do
			// not re-check the channel online (a fresh network call on a dying
			// generation), and do not journal a delivery. Leave the tick here —
			// quiescence is what the generation owes its owner now.
			slog.Debug("Watch generation cancelled mid-send; abandoning the tick",
				"streamer", streamer.GetUsername(), "origin", sl.origin)
			return
		case res.Stale:
			// The playback session changed between snapshot capture and the beacon
			// (a new broadcast or a completed refresh). No minute was delivered, but
			// this is NOT a transport failure and NOT an offline signal: do not note
			// a failure and do not re-check online — the next tick retries against
			// the new session. It also does not count as delivered lost mining
			// against Twitch, so leave watchedOK alone.
			slog.Debug("Skipped minute watched: session changed mid-send (stale)",
				"streamer", streamer.GetUsername(), "origin", sl.origin)
		case res.Failure != nil:
			w.noteReportOutcome(streamer.GetUsername(), false, time.Now())
			slog.Debug("Failed to send minute watched", "streamer", streamer.GetUsername(), "origin", sl.origin,
				"stage", string(res.Failure.Stage), "code", res.Failure.ErrorCode)
			// A failed send usually means the stream just ended; re-check the
			// online state so the next tick drops or switches it (and, for a
			// discovery channel, so discovery's own maintenance abandons it).
			if w.client != nil {
				w.client.CheckStreamerOnlineContext(ctx, streamer)
			}
		default:
			reported = true
			watchedOK++
			w.noteReportOutcome(streamer.GetUsername(), true, time.Now())
			slog.Debug("Sent minute watched", "streamer", streamer.GetUsername(), "origin", sl.origin, "minutesWatched", streamer.Stream.GetMinuteWatched())
			delta := streamer.Stream.UpdateMinuteWatched(maxContinuousGap)
			if sl.idx >= 0 {
				// Configured channel: credit fair-rotation watch time and track
				// streak pursuit. Discovery channels are intentionally excluded
				// from the fairness store and streak accounting.
				// Deliberately NOT bound to the generation context. The
				// bounded join in Stop exists precisely so an in-flight
				// watch_time write DRAINS before the caller closes the
				// database; cancelling this write would discard credited
				// watch time to save milliseconds of shutdown, which is the
				// opposite of what the join is for.
				if w.store != nil && delta > 0 {
					if err := w.store.RecordMinutes(streamer.GetUsername(), delta, time.Now()); err != nil {
						slog.Debug("Failed to record watch time", "streamer", streamer.GetUsername(), "error", err)
					}
				}
				w.noteStreakProgress(sl.idx)
			}
		}

		// Diagnostic-only: account this send against the slot's residence and
		// journal the first success / every failure. No-op unless a journal is
		// injected; never changes control flow above.
		w.recordSlotDelivery(streamer, res)

		if !w.pace(ctx, sleepBetween) {
			return
		}
	}

	w.publishReportStats(slots)

	// Estimate lost mining time for this tick: of the fillable slots, how many
	// produced no watched minute (a granted slot whose send failed while a live
	// candidate existed). Empty slots with no candidate are not counted.
	w.accrueLostMining(fillable, watchedOK, interval)

	// A watched minute means real drop progress was just made; nudge any
	// listener (the drops tracker) to refresh promptly instead of waiting out
	// its sync interval.
	if reported {
		w.mu.RLock()
		hook := w.onMinuteWatched
		w.mu.RUnlock()
		if hook != nil {
			hook()
		}
	}
}

// gatherCandidates collects the proposed channels from every registered
// source. An ordinary candidate whose login is configured is dropped. The sole
// exception is an exact provisional tuple carried by the very same configured
// *models.Streamer pointer: this lets the broker overlay observation ownership
// on a legitimate Phase-A slot without accepting a same-login clone or creating
// a second channel identity. Earlier-source dedup and watchdog avoidance still
// apply to both paths.
func (w *MinuteWatcher) gatherCandidates(ctx context.Context, sources []CandidateSource, avoid AvoidChecker) []Candidate {
	if len(sources) == 0 {
		return nil
	}
	if ctx.Err() != nil {
		// The generation is gone: start no candidate preparation. Sources do
		// their own re-verification I/O here, on THIS goroutine (see
		// CandidateSource), so entering it after cancellation would both work on
		// behalf of a dead generation and delay its join.
		return nil
	}
	configured := make(map[string]*models.Streamer, len(w.streamers))
	for _, s := range w.streamers {
		configured[s.GetUsername()] = s
	}
	var out []Candidate
	seen := make(map[string]bool)
	for _, src := range sources {
		for _, c := range src.WatchCandidates(ctx) {
			if c.Streamer == nil {
				continue
			}
			login := c.Streamer.GetUsername()
			if owner, isConfigured := configured[login]; isConfigured {
				if c.ProvisionalDrop == nil || c.Streamer != owner || !c.ProvisionalDrop.Valid() ||
					c.ProvisionalDrop.Login != login || c.ProvisionalDrop.ChannelID != owner.ChannelID {
					continue
				}
			}
			if seen[login] {
				continue
			}
			if avoid != nil && avoid.IsAvoided(login) {
				continue
			}
			seen[login] = true
			if c.Origin == "" {
				c.Origin = src.SourceName()
			}
			c.ProvisionalDrop = cloneProvisionalCandidate(c.ProvisionalDrop)
			out = append(out, c)
		}
	}
	return out
}

// unknownSlotRetentionGrace bounds how long a channel that was being watched and
// then went UNKNOWN (an online→unknown transient check failure) may keep its watch
// slot without a fresh confirmation. Within the grace its slot and continuous-watch
// accumulator are preserved so a network blip doesn't drop a live drop mid-campaign;
// past it the slot is released to a confirmed-online channel so a permanently-stuck
// unknown (a dead connection) can never pin a slot indefinitely. A failed
// minute-watched send or the stale re-check resolves most cases well within it.
const unknownSlotRetentionGrace = 2 * time.Minute

func (w *MinuteWatcher) getOnlineStreamers(avoid AvoidChecker) []int {
	var online []int
	// One coherent farming-exclusion decision per admission pass (the stored
	// value is immutable), mirroring the per-tick semantic-policy capture.
	skips := w.rewardSkips.Load()
	for i, s := range w.streamers {
		confirmed := s.GetIsOnline()
		// A streamer that just went online→unknown while holding a slot stays a
		// candidate through the blip (continuity retention); it never lets an
		// unknown channel claim a NEW slot — only keep an existing one.
		retained := !confirmed && w.retainsSlotWhileUnknown(s)
		if !confirmed && !retained {
			continue
		}
		// DisableWatch is a hard opt-out: the streamer stays tracked and
		// online for display, but never becomes a watch-slot candidate -
		// even when it's the only online channel (unlike PreferenceAvoid).
		if s.GetSettings().DisableWatch {
			w.noteSelection(i, "watching disabled for this streamer in its settings")
			continue
		}
		// A temporary watchdog avoid works like DisableWatch, but expires on
		// its own: the progress watchdog excludes a channel whose drop
		// progress stalled despite session recovery, so the broker switches
		// to the next eligible channel instead.
		if avoid != nil && avoid.IsAvoided(s.GetUsername()) {
			w.noteSelection(i, "temporarily avoided by the drop-progress watchdog (stalled progress recovery)")
			continue
		}
		if retained {
			w.noteSelection(i, "status unconfirmed - retaining the current watch slot and continuity during a transient check failure")
			online = append(online, i)
			continue
		}
		// Capability gate for a NEW slot, via the single centralized policy
		// (eligibility.SlotCandidateEligible): a channel earns a new watch slot
		// only with a confirmed-useful task - an active eligible Drops entitlement,
		// OR a points task whose Channel Points capability is confirmed Enabled. A
		// points-only channel whose capability is Disabled OR merely Unknown gets
		// no new slot (unknown is never a basis to grant one, and never coerced to
		// enabled); Drops are evaluated independently, so a disabled/unknown-points
		// channel with a live drop still qualifies. Retained (BKM-002 continuity)
		// slots handled above bypass this gate, so an in-progress session is never
		// dropped by it.
		// The Drops input is the production-evaluated eligible-assignment signal
		// (HasEligibleAssignedDropCampaign), NOT the stale DropsCondition: a bare
		// advertised campaign ID no longer earns a slot - the drops tracker must
		// have actually assigned an eligible campaign (active entitlement, not
		// claimed, feasible, coherent ACL, allowed channel, confirmed availability).
		// Points capability still grants a slot independently. The operator's
		// farming exclusions are applied HERE as the watch-side fail-safe: an
		// assigned campaign whose current drop is Skip-ruled contributes no drop
		// justification, even if the assignment writer forgot to pre-filter.
		if ok, reason := watcherEligibility.SlotCandidateEligible(s, s.HasEligibleAssignedDropCampaignExcluding(skips)); !ok {
			w.noteSelection(i, "not eligible for a new watch slot ("+string(reason)+")")
			continue
		}
		if s.GetOnlineAt().IsZero() || time.Since(s.GetOnlineAt()) > 30*time.Second {
			online = append(online, i)
		} else {
			w.noteSelection(i, "went online less than 30s ago - waiting for the stream to settle before watching")
		}
	}
	return online
}

// retainsSlotWhileUnknown reports whether a currently-UNKNOWN streamer should stay
// a watch candidate this tick to preserve an in-progress session. It requires all
// of: the streamer is unknown but was last confirmed ONLINE (an online→unknown
// blip, not initial-unknown or a confirmed-offline channel); it actually held a
// configured watch slot on the previous tick (so this only ever RETAINS a slot,
// never grants a new one); and the uncertainty is recent (bounded by
// unknownSlotRetentionGrace so a stuck unknown eventually releases the slot). An
// authoritative offline (GQL stream:null or a PubSub stream-down) sets StatusOffline
// and ends retention immediately.
func (w *MinuteWatcher) retainsSlotWhileUnknown(s *models.Streamer) bool {
	if s.GetStatus() != models.StatusUnknown || s.GetLastConfirmedStatus() != models.StatusOnline {
		return false
	}
	if _, held := w.lastConfiguredWatched[s.GetUsername()]; !held {
		return false
	}
	since := s.GetUnknownSince()
	return !since.IsZero() && time.Since(since) < unknownSlotRetentionGrace
}

// watcherEligibility is the single centralized eligibility policy used for
// new-slot candidacy (SlotCandidateEligible). It is stateless (system clock);
// there is deliberately no second, divergent capability policy in the watcher.
var watcherEligibility = eligibility.Evaluator{}

// selectStreamersToWatch picks which online streamers to send minute-watched
// events for. Twitch only credits watch time for up to
// constants.MaxSimultaneousStreams (2) channels at once.
//
// With 2 or fewer online streamers there's nothing to choose between: watch
// all of them, exactly as before this rotation feature existed.
//
// With more than 2 online, a fixed top-2-by-priority pick would starve every
// other online channel indefinitely, so instead we rotate the watched pair
// across all online streamers over time (selectRotating), with DROPS/STREAK
// only influencing how often a channel gets an extra turn - never granting
// it a permanent exclusive slot.
func (w *MinuteWatcher) selectStreamersToWatch(onlineIndexes []int) []int {
	candidates := w.filterAvoided(onlineIndexes)
	if len(candidates) <= constants.MaxSimultaneousStreams {
		// Not enough online streamers to need rotation; drop any stale pair
		// so a fresh one is computed next time we go above the limit.
		w.rotation.hasPair = false
		w.rotation.resetDeferralApproach()
		w.rotation.clearBoostLatch()
		w.selectionMode = ModeDirect
		return w.selectByPriority(candidates)
	}
	w.selectionMode = ModeRotation
	return w.selectRotating(candidates)
}

// filterAvoided drops streamers marked PreferenceAvoid from the candidate
// set, unless doing so would leave nothing to watch (e.g. the only online
// channel is marked avoid) - avoid excludes a channel from active watching
// except when it's the only online channel at all.
func (w *MinuteWatcher) filterAvoided(onlineIndexes []int) []int {
	if len(onlineIndexes) <= 1 {
		return onlineIndexes
	}

	filtered := make([]int, 0, len(onlineIndexes))
	var avoided []string
	for _, idx := range onlineIndexes {
		if w.streamers[idx].GetSettings().Preference == models.PreferenceAvoid {
			avoided = append(avoided, w.streamers[idx].GetUsername())
			w.noteSelection(idx, `excluded from watching: preference is set to "avoid" and other channels are online`)
			continue
		}
		filtered = append(filtered, idx)
	}

	if len(filtered) == 0 {
		// Every online streamer is marked avoid - watching something is
		// still required, so the exclusion is lifted entirely.
		for _, idx := range onlineIndexes {
			w.noteSelection(idx, `"avoid" preference ignored: every online channel is marked avoid, so something must still be watched`)
		}
		return onlineIndexes
	}
	if len(avoided) > 0 {
		slog.Debug("Excluding avoided streamers from watch selection", "avoided", avoided)
	}
	return filtered
}

// isPreferred reports whether the streamer at idx is marked PreferencePrefer.
func (w *MinuteWatcher) isPreferred(idx int) bool {
	return w.streamers[idx].GetSettings().Preference == models.PreferencePrefer
}

// selectRotating implements persisted-deficit fairness for the case where more
// streamers are online than fit in the two watch slots.
//
// The base pair is evaluated on every ordinary broker tick. Online streamers
// are ranked by accumulated watch minutes over the trailing watchTimeWindow
// (persisted in SQLite, see store.go), ascending. Ties use in-memory recency
// and then login, giving a deterministic result under candidate permutations.
// Whoever is watched accumulates minutes and becomes less owed, so every valid
// contender progresses without a separate timer or queue.
//
// On top of that fair pair, one strictly stronger DROPS/STREAK candidate may
// take one seat. Hard restricted/streak/drop classes come first; active-drop
// candidates inside the same hard class use Campaign Policy's unitless bounded
// semantic utility. Equal full utilities remain governed by persisted deficit.
// The boost latch is independent of base-pair changes only while a watch streak
// remains pursuit-eligible (including its zero-minute bootstrap). Ordinary
// equal-semantic drops converge to the
// persisted-deficit base pair; a strictly stronger hard/semantic contender may
// still take a seat.
//
// A fairness replacement that would interrupt an in-progress streak may use
// one explicit deferUntil deadline for the current approach. Re-evaluation
// cannot extend that deadline, and offline or no-longer-protected members leave
// immediately.
func (w *MinuteWatcher) selectRotating(onlineIndexes []int) []int {
	now := time.Now()
	w.reconcileLeastWatchedPair(onlineIndexes, now)

	pair := w.applyPriorityBoost(w.rotation.activePair, onlineIndexes)

	for _, idx := range pair {
		w.noteSelectionIfEmpty(idx, "watched: holds a fair slot based on persisted accumulated watch time at this broker evaluation")
	}

	if w.rotation.lastWatched == nil {
		w.rotation.lastWatched = make(map[int]time.Time)
	}
	w.rotation.lastWatched[pair[0]] = now
	w.rotation.lastWatched[pair[1]] = now

	return []int{pair[0], pair[1]}
}

// streakDeferDelay bounds the one explicit deferral used when an immediate
// fairness replacement would interrupt a streamer actively pursuing its watch
// streak. It is completion protection, not a scheduler cadence.
const streakDeferDelay = 2 * time.Minute

// preferenceWeightBiasMinutes is the fixed handicap (in accumulated watch
// minutes) applied in favor of PreferencePrefer streamers when ranking the
// base rotation pair. It's deliberately small relative to typical rotation
// windows so it only breaks near-ties instead of overriding the fairness
// guarantee.
const preferenceWeightBiasMinutes = 5.0

// reconcileLeastWatchedPair evaluates the persisted fairness ranking on every
// broker tick, unless the one bounded streak-completion deferral is active.
func (w *MinuteWatcher) reconcileLeastWatchedPair(onlineIndexes []int, now time.Time) {
	weights := w.watchWeights(onlineIndexes, now)

	candidates := make([]int, len(onlineIndexes))
	copy(candidates, onlineIndexes)
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		// A preferred streamer is treated as if it had watched slightly less
		// than it actually has, tipping the ranking in its favor without
		// overriding the fairness guarantee: it still accumulates real watch
		// time and falls back in line once the gap exceeds this handicap.
		wa, wb := weights[a], weights[b]
		if w.isPreferred(a) {
			wa -= preferenceWeightBiasMinutes
		}
		if w.isPreferred(b) {
			wb -= preferenceWeightBiasMinutes
		}
		if wa != wb {
			return wa < wb
		}
		la, lb := w.rotation.lastWatched[a], w.rotation.lastWatched[b]
		if !la.Equal(lb) {
			return la.Before(lb)
		}
		return compareNormalizedLogins(w.streamers[a].GetUsername(), w.streamers[b].GetUsername()) < 0
	})
	newPair := [2]int{candidates[0], candidates[1]}

	// Deferral is legal only while the current pair remains fully online. An
	// offline/ineligible member must never linger in activePair.
	if w.rotation.hasPair && containsPair(onlineIndexes, w.rotation.activePair) {
		if samePair(newPair, w.rotation.activePair) {
			w.rotation.clearActiveDeferral()
			w.captureDeficitMinutes(onlineIndexes, weights)
			return
		}

		if !w.rotation.deferUntil.IsZero() {
			idx := w.rotation.deferStreamer
			stillLeaving := idx != newPair[0] && idx != newPair[1]
			if now.Before(w.rotation.deferUntil) && stillLeaving &&
				containsIndex(onlineIndexes, idx) && w.nearStreakCompletion(idx) {
				w.noteSelection(idx, "watched: fairness replacement deferred until the bounded watch-streak deadline")
				w.captureDeficitMinutes(onlineIndexes, weights)
				return
			}
			w.rotation.clearActiveDeferral()
		}

		if !w.rotation.deferUsed {
			for _, idx := range w.rotation.activePair {
				if idx == newPair[0] || idx == newPair[1] || !w.nearStreakCompletion(idx) {
					continue
				}
				w.rotation.deferUsed = true
				w.rotation.deferStreamer = idx
				w.rotation.deferUntil = now.Add(streakDeferDelay)
				w.noteSelection(idx, "watched: fairness replacement deferred once to finish an in-progress watch streak")
				w.captureDeficitMinutes(onlineIndexes, weights)
				return
			}
		}
	}

	oldPair := w.rotation.activePair
	hadPair := w.rotation.hasPair
	changed := !hadPair || !samePair(newPair, oldPair)

	w.rotation.activePair = newPair
	w.rotation.hasPair = true
	w.captureDeficitMinutes(onlineIndexes, weights)

	if changed {
		w.rotation.lastSwitch = now
		w.rotation.resetDeferralApproach()
		// Keep the continuity target, but reselect which member it displaces
		// against the new fair pair. Carrying the old victim across pair changes
		// would repeatedly evict the same most-owed channel and starve it.
		if w.rotation.boostLatched {
			w.rotation.boostVictim = -1
		}
		w.logPairChange(oldPair, hadPair, newPair)
	}
}

// logPairChange emits an INFO log whenever the fair-rotation watch pair is
// recomputed, so the switch is visible in production logs (previously this was
// a DEBUG line and never showed at the default INFO console level). It reports
// exactly which member left and which took its place, plus the reason: a
// prefer-weighted streamer tipping the ranking ("prefer weight") versus the
// plain least-accumulated-watch-time ordering ("fair rotation"). Transient
// DROPS/STREAK boosts (applyPriorityBoost) don't change the stored pair and so
// aren't reported here — they'd fire every tick.
func (w *MinuteWatcher) logPairChange(oldPair [2]int, hadPair bool, newPair [2]int) {
	newNames := []string{w.streamers[newPair[0]].GetUsername(), w.streamers[newPair[1]].GetUsername()}

	var preferred []string
	for _, idx := range newPair {
		if w.isPreferred(idx) {
			preferred = append(preferred, w.streamers[idx].GetUsername())
		}
	}

	reason := "fair rotation"
	if len(preferred) > 0 {
		reason = "prefer weight"
	}

	if !hadPair {
		slog.Info("Rotating watch pair", "pair", newNames, "reason", "initial pair")
		return
	}

	var swappedIn, swappedOut []string
	for _, idx := range newPair {
		if idx != oldPair[0] && idx != oldPair[1] {
			swappedIn = append(swappedIn, w.streamers[idx].GetUsername())
		}
	}
	for _, idx := range oldPair {
		if idx != newPair[0] && idx != newPair[1] {
			swappedOut = append(swappedOut, w.streamers[idx].GetUsername())
		}
	}

	attrs := []any{
		"pair", newNames,
		"swappedIn", swappedIn,
		"swappedOut", swappedOut,
		"reason", reason,
	}
	if len(preferred) > 0 {
		attrs = append(attrs, "preferred", preferred)
	}
	slog.Info("Rotating watch pair", attrs...)
}

// watchWeights returns each online streamer's accumulated watch minutes
// over the trailing window, used to rank who's most "owed" a turn. Streamers
// absent from the store's response (including when store is nil, e.g.
// analytics disabled) are treated as 0.
func (w *MinuteWatcher) watchWeights(onlineIndexes []int, now time.Time) map[int]float64 {
	weights := make(map[int]float64, len(onlineIndexes))
	if w.store == nil {
		return weights
	}

	usernames := make([]string, len(onlineIndexes))
	for i, idx := range onlineIndexes {
		usernames[i] = w.streamers[idx].GetUsername()
	}

	minutes, err := w.store.WindowMinutes(usernames, now)
	if err != nil {
		slog.Debug("Failed to load watch-time window", "error", err)
		return weights
	}

	for _, idx := range onlineIndexes {
		weights[idx] = minutes[w.streamers[idx].GetUsername()]
	}
	return weights
}

func containsPair(online []int, pair [2]int) bool {
	var a, b bool
	for _, idx := range online {
		if idx == pair[0] {
			a = true
		}
		if idx == pair[1] {
			b = true
		}
	}
	return a && b
}

func samePair(a, b [2]int) bool {
	return a == b || (a[0] == b[1] && a[1] == b[0])
}

// applyPriorityBoost lets one DROPS/STREAK-eligible online streamer take over a
// base-pair seat for the current tick, without affecting the base ranking
// computed by reconcileLeastWatchedPair.
//
// Continuity latch: a pursuit-eligible watch streak is held across ticks rather
// than re-picked every tick. This includes the zero-minute bootstrap needed to
// receive its first delivered interval. Its victim is retained while base
// membership is stable and reselected fairly when the base pair changes.
// Before the latch, the boost re-selected the least-recently-watched eligible
// channel every tick, which (whenever 3+ channels were eligible) rotated the
// watched set on every single tick: no channel was ever watched on consecutive
// ticks, MinuteWatched was perpetually reset to 0, and no streak ever
// completed. The latch keeps that pursuit-eligible channel in the boost seat
// until the streak is granted, reaches its bounded timeout, goes offline, or a STRICTLY
// higher-priority candidate appears (e.g. a channel-restricted drop), at which
// point it hands the seat off and the previously-displaced base member is
// re-evaluated so it is no longer starved.
//
// Hold duration is deliberately driven by the Stream-owned pursuit state rather
// than a scheduler timer. A pursuing streak self-limits through its bounded
// state. An ordinary drop has no continuity exception: it may hold a seat only
// while its current hard/semantic facts win, and a full tie returns to persisted
// fairness. That does not starve the other online streamers:
//   - the OTHER slot is reconciled from persisted deficit on every broker
//     evaluation, so the most-owed non-boosted channel keeps surfacing;
//   - the boosted channel records its own watch time (RecordMinutes), so the
//     fair-rotation ranking naturally keeps it OUT of the base pair — no
//     double-dipping — which de-starves the rest;
//   - the only channel not watched while the latch holds is the current victim,
//     deliberately the LESS-owed of the two base-pair members, and its identity
//     moves as the base pair recomputes, so no single channel is locked out.
//
// The bounded cost is throughput while a real streak pursuit holds one seat:
// the remaining channels temporarily share the other rotating slot.
func (w *MinuteWatcher) applyPriorityBoost(pair [2]int, onlineIndexes []int) [2]int {
	best := w.selectBoostTarget(pair, onlineIndexes)

	keepHeld := false
	if w.rotation.boostLatched {
		held := w.rotation.boostTarget
		heldValid := held >= 0 && containsIndex(onlineIndexes, held) && w.isBoostEligible(held)
		heldNeedsContinuity := heldValid && w.streakNeedsContinuity(held)
		strongerOffPair := heldNeedsContinuity && best != -1 && w.strictlyHigherBoost(best, held)
		if heldNeedsContinuity && !strongerOffPair {
			if held == pair[0] || held == pair[1] {
				// Persisted fairness brought the held channel into the base pair.
				// It remains continuous without consuming an extra boost seat.
				w.rotation.boostVictim = -1
				return pair
			}
			if w.boostCanRemainAgainstPair(held, pair) {
				best = held
				keepHeld = true
			}
		}
	}
	if best == -1 {
		w.rotation.clearBoostLatch()
		return pair
	}

	// While the same channel is held, keep displacing the same base seat so the
	// surviving base member also stays continuously watched. On a hand-off to a
	// new target, re-evaluate the victim so a base member that was displaced for
	// the whole previous boost gets its turn instead of staying starved.
	var victim int
	if keepHeld && (w.rotation.boostVictim == pair[0] || w.rotation.boostVictim == pair[1]) &&
		(!w.nearStreakCompletion(w.rotation.boostVictim) || w.strictlyHigherBoost(best, w.rotation.boostVictim)) {
		victim = w.rotation.boostVictim
	} else {
		victim = w.selectBoostVictim(pair, best)
	}
	if victim == -1 {
		w.rotation.clearBoostLatch()
		return pair
	}

	switch {
	case w.streamers[best].HasChannelRestrictedCampaign():
		w.noteSelection(best, "watched: boosted into a slot - channel-restricted drop campaign only progresses on this exact channel")
	case w.streamers[best].DropsCondition():
		w.noteSelection(best, "watched: boosted into a slot - active drop campaign")
	default:
		w.noteSelection(best, "watched: boosted into a slot - watch streak not yet earned this stream")
	}
	w.noteSelection(victim, "not watched this tick: displaced by a DROPS/STREAK boost (keeps its rotation slot and returns when the boost ends)")

	w.rotation.boostLatched = true
	w.rotation.boostTarget = best
	w.rotation.boostVictim = victim

	if pair[0] == victim {
		pair[0] = best
	} else {
		pair[1] = best
	}
	return pair
}

// selectBoostTarget returns the highest-priority off-pair boost-eligible
// streamer. For campaign-driven candidates it must strictly outrank every
// boost-eligible base member; equal bounded campaign utilities stay with the
// persisted-deficit base pair. Existing equal fresh-streak boost behavior is
// preserved because that continuity state has no Campaign Policy semantics.
func (w *MinuteWatcher) selectBoostTarget(pair [2]int, onlineIndexes []int) int {
	best := -1
	for _, idx := range onlineIndexes {
		if idx == pair[0] || idx == pair[1] {
			continue
		}
		if !w.isBoostEligible(idx) {
			continue
		}
		if best == -1 || w.betterBoostCandidate(idx, best) {
			best = idx
		}
	}
	if best == -1 {
		return -1
	}

	if !w.boostCanDisplacePair(best, pair) {
		return -1
	}
	return best
}

// boostCanDisplacePair preserves the existing semantic admission rule for a
// new boost target. A strictly weaker target cannot override the strongest
// boost-eligible base member; equal drop semantics remain with persisted
// fairness rather than becoming a second scheduler.
func (w *MinuteWatcher) boostCanDisplacePair(target int, pair [2]int) bool {
	baseBest := w.strongestBoostEligible(pair)
	if baseBest != -1 {
		strict := w.compareStrictBoost(target, baseBest)
		if strict < 0 {
			return false
		}
		if strict == 0 && w.streamers[target].DropsCondition() && w.streamers[baseBest].DropsCondition() {
			// Both seats are driven by equal campaign semantics. The base pair
			// already contains the persisted-deficit winner, so an off-pair
			// boost would invert the required tie-break.
			return false
		}
	}
	return true
}

// boostCanRemainAgainstPair applies the same hard/semantic ordering to an
// already-latched target. Only a pursuit-eligible watch streak has a functional
// continuity requirement (including the zero-minute bootstrap needed to become
// PURSUING). Ordinary active or restricted drops return to fresh target/victim
// selection every tick, so an old latch cannot bypass current policy utility or
// persisted-deficit fairness even when the drop still strictly outranks the base
// pair. A strictly stronger base contender still ends streak continuity.
func (w *MinuteWatcher) boostCanRemainAgainstPair(target int, pair [2]int) bool {
	if !w.streakNeedsContinuity(target) {
		return false
	}
	baseBest := w.strongestBoostEligible(pair)
	if baseBest == -1 {
		return true
	}
	return w.compareStrictBoost(target, baseBest) >= 0
}

func (w *MinuteWatcher) strongestBoostEligible(pair [2]int) int {
	best := -1
	for _, idx := range pair {
		if !w.isBoostEligible(idx) {
			continue
		}
		if best == -1 || w.betterBoostCandidate(idx, best) {
			best = idx
		}
	}
	return best
}

// selectBoostVictim returns the base-pair seat the boost should displace. An
// in-progress streak is protected from an equal/weaker target, but not from a
// strictly stronger contender such as a channel-restricted drop. Otherwise
// evict the weakest hard/semantic member, then the less-owed member, recency,
// and login.
func (w *MinuteWatcher) selectBoostVictim(pair [2]int, target int) int {
	victim := -1
	for _, slot := range pair {
		if w.nearStreakCompletion(slot) && !w.strictlyHigherBoost(target, slot) {
			continue
		}
		if victim == -1 || w.betterBoostVictim(slot, victim) {
			victim = slot
		}
	}
	return victim
}

func (w *MinuteWatcher) betterBoostVictim(candidate, current int) bool {
	if cmp := w.compareStrictBoost(candidate, current); cmp != 0 {
		return cmp < 0
	}
	if w.streamers[candidate].DropsCondition() && w.streamers[current].DropsCondition() {
		cw := w.effectiveDeficitMinutes(candidate)
		bw := w.effectiveDeficitMinutes(current)
		if cw != bw {
			return cw > bw
		}
	}
	cl := w.rotation.lastWatched[candidate]
	bl := w.rotation.lastWatched[current]
	if !cl.Equal(bl) {
		return cl.After(bl)
	}
	return compareNormalizedLogins(w.streamers[candidate].GetUsername(), w.streamers[current].GetUsername()) > 0
}

// strictlyHigherBoost reports whether cand has strictly stronger hard or
// campaign-semantic facts than held. Persisted deficit, recency, and login are
// deliberately excluded: equal bounded-utility fairness chooses the initial
// seat, but cannot churn a continuity latch every tick.
func (w *MinuteWatcher) strictlyHigherBoost(cand, held int) bool {
	return w.compareStrictBoost(cand, held) > 0
}

// compareStrictBoost returns positive when a is strictly preferred to b under
// the non-fairness part of broker precedence:
//
//	channel-restricted drop
//	> in-progress streak (then more banked streak minutes)
//	> Campaign Policy bounded utility when both candidates carry active drops
//
// A plain active drop and a fresh pending streak deliberately remain equal at
// this strict layer, preserving the pre-change rotation contract (recency picks
// between them). Semantic utility remains a lexicographic ordinal comparison,
// never a weighted sum with watch minutes.
func (w *MinuteWatcher) compareStrictBoost(a, b int) int {
	ar := w.streamers[a].HasChannelRestrictedCampaign()
	br := w.streamers[b].HasChannelRestrictedCampaign()
	if ar != br {
		if ar {
			return 1
		}
		return -1
	}

	ap := w.streakInProgress(a)
	bp := w.streakInProgress(b)
	if ap != bp {
		if ap {
			return 1
		}
		return -1
	}
	if ap && bp {
		am := w.streamers[a].Stream.GetMinuteWatched()
		bm := w.streamers[b].Stream.GetMinuteWatched()
		if am != bm {
			if am > bm {
				return 1
			}
			return -1
		}
	}

	ad := w.streamers[a].DropsCondition()
	bd := w.streamers[b].DropsCondition()
	if ad && bd {
		if cmp := w.compareCampaignSemanticUtility(a, b); cmp != 0 {
			return cmp
		}
	}
	return 0
}

// compareCampaignSemanticUtility returns positive when a has the better
// published bounded utility. A ranked campaign beats an absent/unranked one;
// primary then at most one secondary class are compared lexicographically.
// Campaign IDs never reach this boundary.
func (w *MinuteWatcher) compareCampaignSemanticUtility(a, b int) int {
	return w.compareCampaignSemanticStreamers(w.streamers[a], w.streamers[b])
}

func (w *MinuteWatcher) compareCampaignSemanticStreamers(a, b *models.Streamer) int {
	ua, oka := w.campaignSemanticUtilityForStreamer(a)
	ub, okb := w.campaignSemanticUtilityForStreamer(b)
	if oka != okb {
		if oka {
			return 1
		}
		return -1
	}
	if !oka {
		return 0
	}
	return policy.CompareSemanticUtility(ua, ub)
}

func (w *MinuteWatcher) campaignSemanticUtilityForStreamer(s *models.Streamer) (policy.SemanticUtility, bool) {
	if active := w.activeCampaignSemanticPolicy.Load(); active != nil {
		if active.byLogin == nil {
			return policy.SemanticUtility{}, false
		}
		utility, ok := active.byLogin[s.GetUsername()]
		return utility, ok
	}

	snapshot := w.campaignSemanticPolicy.Load()
	if snapshot != nil && snapshot.byCampaign != nil {
		return currentCampaignSemanticUtility(s, snapshot.byCampaign)
	}
	if snapshot == nil || snapshot.byLogin == nil {
		return policy.SemanticUtility{}, false
	}
	utility, ok := snapshot.byLogin[s.GetUsername()]
	return utility, ok
}

// captureCampaignSemanticSnapshot freezes both the published policy generation
// and current configured campaign assignments at the start of one broker tick.
// A non-nil exact-empty sentinel represents "captured before first policy": it
// prevents a first publication made between Phase A and discovery arbitration
// from entering only the second half of that tick.
func (w *MinuteWatcher) captureCampaignSemanticSnapshot() *campaignSemanticSnapshot {
	published := w.campaignSemanticPolicy.Load()
	if published == nil {
		return &campaignSemanticSnapshot{
			byLogin:    map[string]policy.SemanticUtility{},
			byCampaign: map[string]policy.CampaignSemantic{},
			gameRanks:  map[string]int{},
		}
	}
	if published.byCampaign == nil {
		return published
	}

	byLogin := make(map[string]policy.SemanticUtility, len(w.streamers))
	for _, s := range w.streamers {
		if utility, ok := currentCampaignSemanticUtility(s, published.byCampaign); ok {
			byLogin[s.GetUsername()] = utility
		}
	}
	return &campaignSemanticSnapshot{
		byLogin:    byLogin,
		byCampaign: published.byCampaign,
		gameRanks:  published.gameRanks,
	}
}

// currentCampaignSemanticUtility reprojects the exact tracker-assigned
// campaigns through the shared bounded builder. Published feasibility can only
// be downgraded here: a removed, claimed, or locally completed campaign cannot
// retain stale positive secondary utility before the next policy refresh.
func currentCampaignSemanticUtility(s *models.Streamer, published map[string]policy.CampaignSemantic) (policy.SemanticUtility, bool) {
	ids, remainingWorkIDs := CampaignSemanticEvidence(s.Stream.GetCampaigns())
	return policy.BuildSemanticUtilityWithRemainingWork(ids, remainingWorkIDs, published)
}

// CampaignSemanticEvidence extracts exact CampaignID evidence from one
// channel's current tracker-assigned campaign snapshot. Duplicate IDs are
// retained for the policy builder to deduplicate, while current remaining-work
// eligibility is fail-closed across duplicates: every copy of an ID must still
// contain real unclaimed work before that ID can provide secondary utility.
func CampaignSemanticEvidence(campaigns []*models.Campaign) (campaignIDs, remainingWorkCampaignIDs []string) {
	remainingByID := make(map[string]bool, len(campaigns))
	seen := make(map[string]bool, len(campaigns))
	for _, campaign := range campaigns {
		if campaign == nil || campaign.ID == "" || campaign.CurrentDrop() == nil {
			continue
		}
		campaignIDs = append(campaignIDs, campaign.ID)
		remaining := campaign.HasRemainingUnclaimedWork()
		if seen[campaign.ID] {
			remainingByID[campaign.ID] = remainingByID[campaign.ID] && remaining
		} else {
			seen[campaign.ID] = true
			remainingByID[campaign.ID] = remaining
		}
	}

	emitted := make(map[string]bool, len(remainingByID))
	for _, id := range campaignIDs {
		if remainingByID[id] && !emitted[id] {
			remainingWorkCampaignIDs = append(remainingWorkCampaignIDs, id)
			emitted[id] = true
		}
	}
	return campaignIDs, remainingWorkCampaignIDs
}

func (w *MinuteWatcher) campaignSemanticSnapshotForTick() *campaignSemanticSnapshot {
	if active := w.activeCampaignSemanticPolicy.Load(); active != nil {
		return active
	}
	return w.campaignSemanticPolicy.Load()
}

// orderByCampaignSemanticClass returns a copy ordered by each streamer's
// published bounded utility (known before absent, stronger first), then login
// for a deterministic full-utility tie. With no semantic facts published, the
// pre-policy configured/input order is preserved exactly.
func (w *MinuteWatcher) orderByCampaignSemanticClass(indexes []int) []int {
	if w.campaignSemanticSnapshotForTick() == nil {
		return indexes
	}
	ordered := append([]int(nil), indexes...)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		ua, oka := w.campaignSemanticUtilityForStreamer(w.streamers[a])
		ub, okb := w.campaignSemanticUtilityForStreamer(w.streamers[b])
		if oka != okb {
			return oka
		}
		if oka {
			if cmp := policy.CompareSemanticUtility(ua, ub); cmp != 0 {
				return cmp > 0
			}
		}
		if !oka {
			return false
		}
		return compareNormalizedLogins(w.streamers[a].GetUsername(), w.streamers[b].GetUsername()) < 0
	})
	return ordered
}

// refreshDeficitMinutes captures persisted WatchTimeStore evidence for the
// current allocation tick. Refreshing before every contested broker decision
// makes rename/removal lifecycle changes converge immediately and keeps this
// map evidence rather than an independent fairness authority.
func (w *MinuteWatcher) refreshDeficitMinutes(indexes []int, now time.Time) {
	w.captureDeficitMinutes(indexes, w.watchWeights(indexes, now))
}

func (w *MinuteWatcher) captureDeficitMinutes(indexes []int, weights map[int]float64) {
	w.rotation.deficitMinutes = make(map[string]float64, len(indexes))
	for _, idx := range indexes {
		w.rotation.deficitMinutes[w.streamers[idx].GetUsername()] = weights[idx]
	}
}

// effectiveDeficitMinutes returns the persisted fairness evidence captured
// for the current contested allocation, including the existing bounded
// PreferencePrefer handicap. Lower means the channel is owed more watch time.
func (w *MinuteWatcher) effectiveDeficitMinutes(idx int) float64 {
	minutes := w.rotation.deficitMinutes[w.streamers[idx].GetUsername()]
	if w.isPreferred(idx) {
		minutes -= preferenceWeightBiasMinutes
	}
	return minutes
}

// containsIndex reports whether idx is present in the online index slice.
func containsIndex(online []int, idx int) bool {
	for _, o := range online {
		if o == idx {
			return true
		}
	}
	return false
}

func (w *MinuteWatcher) isBoostEligible(idx int) bool {
	s := w.streamers[idx]
	decision, streakAdmitted := w.watchStreakDecision(idx)
	if s.DropsCondition() {
		return true
	}
	return streakAdmitted && decision.PursuitEligible
}

// watchStreakDecision is the sole watcher adapter around the Stream owner. The
// two external admission gates (user setting and existing offline-age policy)
// are shared by direct selection, broker protection and diagnostics. The Stream
// atomically owns eligibility, state and timeout; persistence is invoked only
// after its lock is released. Evaluate before the Drops short-circuit so an
// active Drop cannot accidentally keep a 20-minute streak timeout unlatched.
func (w *MinuteWatcher) watchStreakDecision(idx int) (models.WatchStreakDecision, bool) {
	s := w.streamers[idx]
	if !s.GetSettings().WatchStreak || (!s.GetOfflineAt().IsZero() && time.Since(s.GetOfflineAt()) <= 30*time.Minute) {
		return models.WatchStreakDecision{}, false
	}
	decision := s.Stream.EvaluateWatchStreak(time.Now())
	if decision.Transitioned {
		w.mu.RLock()
		hook := w.onWatchStreakTransition
		w.mu.RUnlock()
		if hook != nil {
			hook(s, decision.Persistence)
		}
	}
	return decision, true
}

// streakPursuitExhausted is a compatibility wrapper for focused tests and
// diagnostics; it consumes the same owner verdict and reconstructs no state.
func (w *MinuteWatcher) streakPursuitExhausted(idx int) bool {
	decision, admitted := w.watchStreakDecision(idx)
	return admitted && decision.State == models.WatchStreakTimedOutUnknown
}

// streakInProgress reports whether a boost-eligible streamer is actively
// pursuing its watch streak: some watch time banked, streak still missing, and
// the Stream-owned bounded pursuit not yet timed out. Preferring these when picking the
// boost seat lets the watcher finish a streak it already started instead of
// alternating between several fresh pending-streak streamers each tick and
// completing none of them.
func (w *MinuteWatcher) streakInProgress(idx int) bool {
	decision, admitted := w.watchStreakDecision(idx)
	return admitted && decision.State == models.WatchStreakPursuing
}

// streakNeedsContinuity includes ELIGIBLE so a newly selected streak can bank
// the first delivered minute needed to become PURSUING. Both terminal states
// make PursuitEligible false, so a bound grant or exact timeout ends continuity.
func (w *MinuteWatcher) streakNeedsContinuity(idx int) bool {
	decision, admitted := w.watchStreakDecision(idx)
	return admitted && decision.PursuitEligible
}

// betterBoostCandidate reports whether cand should take the single prioritized
// seat over best. Hard/continuity and semantic facts are compared first;
// persisted deficit decides only inside equal bounded semantic utility, followed by
// recency and login for a deterministic total order.
func (w *MinuteWatcher) betterBoostCandidate(cand, best int) bool {
	if cmp := w.compareStrictBoost(cand, best); cmp != 0 {
		return cmp > 0
	}

	// Persisted deficit is the fairness tie-break only when both candidates
	// have equal bounded campaign semantic utility. Fresh-streak/drop arbitration
	// had no campaign semantics before this change and keeps its recency rule.
	if w.streamers[cand].DropsCondition() && w.streamers[best].DropsCondition() {
		cw := w.effectiveDeficitMinutes(cand)
		bw := w.effectiveDeficitMinutes(best)
		if cw != bw {
			return cw < bw
		}
	}

	cl := w.rotation.lastWatched[cand]
	bl := w.rotation.lastWatched[best]
	if !cl.Equal(bl) {
		return cl.Before(bl)
	}

	return compareNormalizedLogins(w.streamers[cand].GetUsername(), w.streamers[best].GetUsername()) < 0
}

// compareNormalizedLogins provides the broker's final deterministic identity
// order. Twitch logins are case-insensitive; a raw-string fallback keeps a
// total order even for malformed case variants of the same login.
func compareNormalizedLogins(a, b string) int {
	na := strings.ToLower(strings.TrimSpace(a))
	nb := strings.ToLower(strings.TrimSpace(b))
	if na < nb {
		return -1
	}
	if na > nb {
		return 1
	}
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

// nearStreakCompletion reports whether the streamer is actively pursuing a watch
// streak that has neither been earned nor exhausted, so an in-flight rotation
// swap-out or a boost displacement should avoid interrupting it. With the
// event-driven model there is no fixed "minutes-to-completion" line any more
// (the pursuit ends on the WATCH_STREAK grant or the Stream-owned 20m cap), so
// this mirrors streakInProgress. The swap-out deferral it feeds is bounded
// (once per approach), so it can never stall the rotation.
func (w *MinuteWatcher) nearStreakCompletion(idx int) bool {
	return w.streakInProgress(idx)
}

// noteStreakProgress logs watch-streak pursuit for a streamer that just had a
// minute-watched successfully reported. It emits at most one "Pursuing watch
// streak" INFO and one outcome-neutral bounded-timeout line per streak, so the operator
// can both see the bot actively chasing streaks (previously invisible - the
// only streak log was the earned "Points earned" line, which never appears
// when streaks aren't being credited) and, crucially, tell apart "not watched
// enough yet" from "watched enough but Twitch never granted it".
func (w *MinuteWatcher) noteStreakProgress(idx int) {
	s := w.streamers[idx]
	decision, admitted := w.watchStreakDecision(idx)
	if !admitted || decision.State == models.WatchStreakGranted || decision.State == models.WatchStreakUnidentified {
		delete(w.streakDiag, idx)
		return
	}

	if w.streakDiag == nil {
		w.streakDiag = make(map[int]streakDiagState)
	}
	state := w.streakDiag[idx]
	if state.broadcastID != decision.BroadcastID {
		state = streakDiagState{broadcastID: decision.BroadcastID}
	}
	mw := decision.ContinuousMinutes
	evidence := decision.WatchEvidence

	if !state.pursuing {
		state.pursuing = true
		slog.Info("Pursuing watch streak (holding a boost slot until Twitch grants it or the bounded watch window elapses)",
			"streamer", s.GetUsername(),
			"continuousWatchedMinutes", mw,
			"watchEvents", evidence,
			"broadcastID", decision.BroadcastID)
	}

	// Log the bounded-window release exactly once, as an OUTCOME-NEUTRAL
	// transition. At this point the streak outcome is genuinely unknown: releasing
	// only frees the seat (StreakPending stays true, so a late real WATCH_STREAK is
	// still accepted and recorded once), and the grant travels on the best-effort
	// PubSub transport, which cannot tell "not granted" apart from "not delivered".
	// So this line asserts neither "granted" nor "not granted" — only that the
	// bounded pursuit window elapsed (releaseReason=bounded_timeout, outcome=unknown).
	// The WATCH-evidence counter is a diagnostic field, never a release trigger; the
	// only inference drawn from it is the narrow, non-outcome hint that ZERO WATCH
	// credits for the whole broadcast may point at a transport/authorization problem
	// worth checking (a counted view normally produces WATCH credits).
	if decision.State == models.WatchStreakTimedOutUnknown && !state.released {
		state.released = true
		attrs := []any{
			"streamer", s.GetUsername(),
			"broadcastID", decision.BroadcastID,
			"continuousWatchedMinutes", mw,
			"watchEvents", evidence,
			"releaseReason", "bounded_timeout",
			"outcome", "unknown",
		}
		if evidence == 0 {
			slog.Warn("Releasing the watch-streak boost slot: bounded watch window elapsed, streak outcome unknown (a late WATCH_STREAK is still accepted); no WATCH point credits arrived for this broadcast - check authorization/transport", attrs...)
		} else {
			slog.Info("Releasing the watch-streak boost slot: bounded watch window elapsed, streak outcome unknown (a late WATCH_STREAK is still accepted)", attrs...)
		}
	}

	w.streakDiag[idx] = state
}

// selectByPriority is the original priority-based picker, used as-is when
// there are <= constants.MaxSimultaneousStreams online (no rotation needed).
func (w *MinuteWatcher) selectByPriority(onlineIndexes []int) []int {
	// Preferred streamers are moved to the front (stably, so relative order
	// is otherwise unchanged). This only breaks ties within each priority
	// step below - it never lets a preferred streamer skip ahead of one that
	// actually satisfies a higher-ranked priority.
	ordered := make([]int, len(onlineIndexes))
	copy(ordered, onlineIndexes)
	sort.SliceStable(ordered, func(i, j int) bool {
		return w.isPreferred(ordered[i]) && !w.isPreferred(ordered[j])
	})
	onlineIndexes = ordered

	watching := make(map[int]bool)

	remainingSlots := func() int {
		return constants.MaxSimultaneousStreams - len(watching)
	}

	for _, priority := range w.priorities {
		if remainingSlots() <= 0 {
			break
		}

		switch priority {
		case config.PriorityOrder:
			for _, idx := range onlineIndexes {
				if !watching[idx] {
					watching[idx] = true
					w.noteSelection(idx, "watched: selected by ORDER priority (position in the configured streamer list)")
					if remainingSlots() <= 0 {
						break
					}
				}
			}

		case config.PriorityPointsAscending, config.PriorityPointsDescending:
			type indexedPoints struct {
				index  int
				points int
			}
			items := make([]indexedPoints, 0, len(onlineIndexes))
			for _, idx := range onlineIndexes {
				items = append(items, indexedPoints{index: idx, points: w.streamers[idx].GetChannelPoints()})
			}
			sort.SliceStable(items, func(i, j int) bool {
				if priority == config.PriorityPointsAscending {
					return items[i].points < items[j].points
				}
				return items[i].points > items[j].points
			})
			for _, item := range items {
				if !watching[item.index] {
					watching[item.index] = true
					w.noteSelection(item.index, fmt.Sprintf("watched: selected by %s priority (%d channel points)", priority, item.points))
					if remainingSlots() <= 0 {
						break
					}
				}
			}

		case config.PriorityStreak:
			for _, idx := range onlineIndexes {
				decision, admitted := w.watchStreakDecision(idx)
				if admitted && decision.PursuitEligible {
					if !watching[idx] {
						watching[idx] = true
						w.noteSelection(idx, "watched: selected by STREAK priority - watch streak not yet earned this stream")
						if remainingSlots() <= 0 {
							break
						}
					}
				}
			}

		case config.PriorityDrops:
			// Within each DROPS pass, order competing streamers by the campaign
			// policy engine's bounded semantic utility so the active mode
			// (SMART/ENDING_SOONEST/…) decides between several farmable
			// campaigns. With no policy facts published
			// this is a no-op and the configured order is preserved. The
			// restricted-first pass below is kept regardless, so the
			// "channel-restricted drop only progresses here" invariant holds in
			// every mode.
			dropsOrder := w.orderByCampaignSemanticClass(onlineIndexes)
			for _, idx := range dropsOrder {
				if w.streamers[idx].DropsCondition() && w.streamers[idx].HasChannelRestrictedCampaign() {
					if !watching[idx] {
						watching[idx] = true
						w.noteSelection(idx, "watched: selected by DROPS priority - channel-restricted drop campaign only progresses on this exact channel")
						if remainingSlots() <= 0 {
							break
						}
					}
				}
			}
			for _, idx := range dropsOrder {
				if remainingSlots() <= 0 {
					break
				}
				if w.streamers[idx].DropsCondition() {
					if !watching[idx] {
						watching[idx] = true
						w.noteSelection(idx, "watched: selected by DROPS priority - active drop campaign")
						if remainingSlots() <= 0 {
							break
						}
					}
				}
			}

		case config.PrioritySubscribed:
			type indexedMultiplier struct {
				index      int
				multiplier float64
			}
			var items []indexedMultiplier
			for _, idx := range onlineIndexes {
				if w.streamers[idx].ViewerHasPointsMultiplier() {
					items = append(items, indexedMultiplier{
						index:      idx,
						multiplier: w.streamers[idx].TotalPointsMultiplier(),
					})
				}
			}
			sort.SliceStable(items, func(i, j int) bool {
				return items[i].multiplier > items[j].multiplier
			})
			for _, item := range items {
				if !watching[item.index] {
					watching[item.index] = true
					w.noteSelection(item.index, fmt.Sprintf("watched: selected by SUBSCRIBED priority (%.1fx points multiplier)", item.multiplier))
					if remainingSlots() <= 0 {
						break
					}
				}
			}
		}
	}

	result := make([]int, 0, len(watching))
	for idx := range watching {
		result = append(result, idx)
	}
	return result
}

func (w *MinuteWatcher) sendMinuteWatched(ctx context.Context, streamer *models.Streamer) SendResult {
	res := w.sender.Send(ctx, streamer)
	if res.SimulateErr != nil {
		slog.Debug("Failed to simulate watching", "streamer", streamer.GetUsername(), "error", res.SimulateErr)
	}
	return res
}

// accrueLostMining credits this tick's idle-slot time to the lost-mining
// accumulator. fillable is how many slots could have been productively used
// this tick (bounded by the slot cap), watchedOK is how many actually reported
// a minute, and interval is the tick length. Lost = the shortfall × interval;
// zero when every fillable slot was watched (or nothing was fillable).
func (w *MinuteWatcher) accrueLostMining(fillable, watchedOK int, interval time.Duration) {
	capacity := fillable
	if capacity > constants.MaxSimultaneousStreams {
		capacity = constants.MaxSimultaneousStreams
	}
	lost := capacity - watchedOK
	if lost <= 0 {
		return
	}
	w.lostMu.Lock()
	w.lostMiningMinutes += float64(lost) * interval.Minutes()
	w.lostMu.Unlock()
}

// LostMiningMinutes returns the accumulated estimated lost mining minutes and
// resets the accumulator to zero (drain semantics). Called once per daily
// summary; the returned value covers the period since the previous drain. It is
// in-memory and process-lifetime: a restart resets it, so a summary after a
// mid-day restart only reflects post-restart idle time.
func (w *MinuteWatcher) LostMiningMinutes() float64 {
	w.lostMu.Lock()
	defer w.lostMu.Unlock()
	v := w.lostMiningMinutes
	w.lostMiningMinutes = 0
	return v
}
