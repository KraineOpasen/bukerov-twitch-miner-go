package health

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/drops"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/watcher"
)

// DropsView is the slice of the drops tracker the progress watchdog needs:
// the published campaign snapshots, the sync/observation bookkeeping, and the
// two forced-sync recovery levers. Satisfied by *drops.DropsTracker.
type DropsView interface {
	Campaigns() []*models.Campaign
	BrokerCampaignSnapshot() drops.BrokerCampaignSnapshot
	SyncStatus() drops.SyncStatus
	ProgressObservation(campaignID, dropID string) drops.ProgressObservation
	SyncNow()
	TriggerProgressSync()
}

// WatchView is the slice of the slot broker the watchdog needs: who holds a
// slot, per-channel delivery accounting, and the staged session-refresh
// levers (the broker executes those on its own loop goroutine — the watchdog
// never mutates a live streamer itself). Satisfied by *watcher.MinuteWatcher.
type WatchView interface {
	BrokerSnapshot() watcher.BrokerSnapshot
	IsWatching(login string) bool
	ReportStats(login string) (watcher.ReportStats, bool)
	RequestSessionRefresh(req watcher.SessionRefreshRequest)
	LastSessionRefresh(login string) (watcher.SessionRefreshOutcome, bool)
	SetProvisionalMonitoringEnabled(enabled bool)
	ProvisionalLease() (watcher.ProvisionalLease, bool)
	ProvisionalProofs() []watcher.ProvisionalProof
	ProvisionalOwner(authorityID uint64, expected models.ProvisionalDropCandidate) (*models.Streamer, bool)
	ObserveProvisionalAbsence(leaseID, run uint64, at time.Time) bool
	ObserveProvisionalTupleUnknown(leaseID, run uint64, at time.Time) bool
	ArmProvisionalLease(leaseID, run uint64, at time.Time, minutes int) bool
	ObserveProvisionalProgress(leaseID, run uint64, at time.Time, minutes int) bool
	ReleaseProvisionalLease(leaseID uint64) bool
	QuarantineProvisionalLease(leaseID uint64, expected models.ProvisionalDropCandidate) bool
	AcquireObservationPermit(streamer *models.Streamer, leaseID uint64) (watcher.ObservationPermit, bool)
	AcquireProvisionalProofPermit(streamer *models.Streamer, proofID uint64, expected models.ProvisionalDropCandidate) (watcher.ObservationPermit, bool)
	ReleaseObservationPermit(permit watcher.ObservationPermit)
}

// DropNotifier is told when a drop is confirmed stalled beyond recovery, and
// when a previously-stalled drop starts progressing again. Satisfied by the
// miner's notification adapter (may be nil).
type DropNotifier interface {
	NotifyDropStalled(campaign, drop, channel, detail string)
	NotifyDropRecovered(campaign, drop, channel, detail string)
}

// StreamerResolver maps a channel login to its live streamer object
// (configured list or discovery's current channel). Returns nil when unknown.
// The watchdog only READS the streamer through its locked accessors; all
// mutation goes through the broker's staged session refresh.
type StreamerResolver func(login string) *models.Streamer

// WatchdogConfig is the progress watchdog's runtime configuration. Thresholds
// are deliberately conservative: Twitch credits drop minutes in batches
// (typically every ~15 minutes), so a short quiet period is normal and must
// never confirm a stall.
type WatchdogConfig struct {
	Enabled            bool
	StallDelay         time.Duration // min wall time without progress before a stall can confirm
	StallConfirmations int           // consecutive completed inventory observations without progress
	RecoveryCooldown   time.Duration // min gap between two recovery-stage executions
	AvoidTTL           time.Duration // how long a switched-away channel stays excluded
	Rearm              time.Duration // after pipeline exhaustion, when it may run again
}

// Drop progress statuses shown on the Drops page and in the debug snapshot.
const (
	ProgressHealthy    = "healthy"
	ProgressRecovering = "recovering"
	ProgressStalled    = "stalled"
)

const (
	// watchdogEvalCadence is how often the watchdog re-evaluates all tracked
	// drops (jittered ±20%, matching the project convention).
	watchdogEvalCadence = time.Minute
	// stallMinReports is how many successful minute-watched deliveries must
	// have gone to the farming channel since the last observed progress before
	// a stall can confirm — "we are demonstrably watching, Twitch is
	// demonstrably not crediting". Roughly five watched minutes.
	stallMinReports = 5
	// recoveryStageTimeout bounds the blocking recovery stages (forced full
	// resync, transport probe) on the watchdog goroutine.
	recoveryStageTimeout = 60 * time.Second
	// recoveryOutcomeDeadline bounds how long an async recovery stage (a staged
	// session refresh) waits for its matching broker outcome before giving up and
	// publishing a typed timeout. The broker executes a staged refresh at the
	// start of its next watch tick (well under a minute), and the watchdog
	// re-evaluates roughly every minute, so a matching outcome normally lands
	// within one or two passes; the deadline exists only so a broker that never
	// runs the request (slot churn, shutdown) cannot pin the pipeline forever.
	recoveryOutcomeDeadline = 5 * time.Minute
)

// pendingRecovery correlates an in-flight async recovery stage (a staged session
// refresh) with the broker outcome that will complete it. The watchdog does NOT
// advance past the stage until a broker outcome matching this exact RequestID and
// signature is observed (success/failed), the session it targeted is superseded
// (stale), the slot is lost (skipped), or the bounded deadline elapses (timeout).
type pendingRecovery struct {
	requestID   string
	signature   string
	broadcastID string
	generation  uint64
	stageIndex  int // recoveryStages index of the stage that issued this request
	stageName   string
	requestedAt time.Time
	deadline    time.Time
}

// DropProgress is the published per-drop watchdog state: what the Drops page
// badge and the debug snapshot render. It carries no URLs or tokens.
type DropProgress struct {
	CampaignID           string    `json:"campaignId"`
	CampaignName         string    `json:"campaignName"`
	DropID               string    `json:"dropId"`
	DropName             string    `json:"dropName"`
	Channel              string    `json:"channel,omitempty"`
	LastMinutes          int       `json:"lastMinutes"`
	LastProgressAt       time.Time `json:"lastProgressAt,omitzero"`
	ReportsSinceProgress int       `json:"reportsSinceProgress"`
	NoProgressObs        int       `json:"noProgressObservations"`
	Status               string    `json:"status"`
	RecoveryStage        int       `json:"recoveryStage,omitempty"`
	RecoveryStageName    string    `json:"recoveryStageName,omitempty"`
	LastRecoveryAt       time.Time `json:"lastRecoveryAt,omitzero"`
	Detail               string    `json:"detail,omitempty"`
}

// ProgressSnapshot is the immutable published view of every tracked drop's
// watchdog state.
type ProgressSnapshot struct {
	Enabled     bool           `json:"enabled"`
	EvaluatedAt time.Time      `json:"evaluatedAt,omitzero"`
	Drops       []DropProgress `json:"drops"`
}

// dropState is the watchdog's internal per-drop state (keyed by
// campaignID+dropID). The embedded DropProgress is the published part.
type dropState struct {
	DropProgress

	// evidenceSince is when the current uninterrupted stall-evidence window
	// began: the moment every confirmation gate started holding. Zero while any
	// gate fails. All three stall thresholds (delay, observations, reports)
	// count only inside this window, so a confirmed stall always represents at
	// least StallDelay of DEMONSTRABLE farming without credit — evidence
	// accrued while the channel was offline, rotated out, or ineligible never
	// carries over (that would confirm a stall minutes after farming resumes,
	// well inside Twitch's ~15-minute crediting batch).
	evidenceSince      time.Time
	lastObservedSyncAt time.Time // ProgressLastSyncAt already counted as an observation
	baselineReports    int       // farming channel's success count at last progress
	baselineValid      bool      // baselineReports reflects a real ReportStats read
	statsChannel       string    // channel the baseline belongs to
	avoidedChannel     string    // channel this episode's switch stage excluded ("" if none)
	exhaustedAt        time.Time // when the pipeline ran out of stages
	notifiedStalled    bool      // critical notification already sent this episode

	// Provisional observations are deliberately represented separately from a
	// confirmed Stream.Campaigns assignment. The broker owns the lease and the
	// watchdog owns only its server-progress baseline/recovery coordination.
	provisional           bool
	provisionalLeaseID    uint64
	provisionalProof      bool
	provisionalProofID    uint64
	provisionalCandidate  models.ProvisionalDropCandidate
	provisionalOwner      *models.Streamer
	provisionalCaptureRun uint64
	provisionalCaptureAt  time.Time
	provisionalLastRun    uint64
	provisionalLastAt     time.Time
	provisionalSyncAsked  bool
	recoveryDeferred      bool
	terminalDeferred      bool
	terminalDeferredRun   uint64
	terminalDeferredAt    time.Time

	// pending correlates an in-flight async recovery stage with its broker
	// outcome. While non-nil the pipeline is parked on that stage: no new stage
	// runs, no duplicate request is staged, and the status stays Recovering with an
	// honest "awaiting outcome" detail until the outcome matches or the deadline
	// elapses.
	pending *pendingRecovery
}

// resetEvidence discards the stall-evidence window (a gate failed): the delay
// clock, observation counter, and delivery baseline all restart once farming
// is demonstrably active again. The recovery stage and notification flags
// survive — a transient gate blip must not restart the pipeline, only pause it
// and demand fresh evidence before the next stage.
func (st *dropState) resetEvidence() {
	st.evidenceSince = time.Time{}
	st.NoProgressObs = 0
	st.ReportsSinceProgress = 0
	st.statsChannel = "" // force a delivery re-baseline even for the same channel
}

// recoveryStage is one step of the staged recovery pipeline. run executes on
// the watchdog goroutine and returns a redacted human detail; it must be
// idempotent (a re-run after a cooldown performs the same bounded work).
type recoveryStage struct {
	name  string
	label string
	run   func(w *ProgressWatchdog, st *dropState, now time.Time) string
}

// recoveryStages is the ordered pipeline. Stages 3/5 stage their work into the
// slot broker (single-writer rule) instead of mutating the streamer here;
// stage 4 is read-only on the streamer. The spec's "refresh playback token"
// and "refresh playlist" steps map onto the transport probe: the sender holds
// no token/playlist cache to invalidate (both are fetched fresh on every
// send), so a staged, stage-instrumented verification is the honest
// equivalent. The pipeline is finite — after the last stage the drop is
// STALLED until progress resumes or the rearm window elapses.
var recoveryStages = []recoveryStage{
	{
		name:  "progress_sync",
		label: "forced inventory sync",
		run: func(w *ProgressWatchdog, _ *dropState, _ time.Time) string {
			w.drops.TriggerProgressSync()
			return "forced a lightweight inventory sync"
		},
	},
	{
		name:  "full_resync",
		label: "full campaign resync",
		run: func(w *ProgressWatchdog, _ *dropState, _ time.Time) string {
			// SyncNow re-runs the whole discovery pipeline including the
			// campaign/channel intersection recompute. It is ctx-unaware, so it
			// runs under the same detached watchdog the canary uses; an abandoned
			// run completes in the background (serialized by the tracker) and its
			// result is observed on a later tick.
			ctx, cancel := context.WithTimeout(w.parentCtx(), recoveryStageTimeout)
			defer cancel()
			if err := runDetached(ctx, w.drops.SyncNow); err != nil {
				return "forced a full campaign resync (still completing in the background)"
			}
			return "forced a full campaign resync: dashboard, details, inventory, and channel intersection refreshed"
		},
	},
	{
		name:  "stream_info",
		label: "stream info refresh",
		run: func(w *ProgressWatchdog, st *dropState, now time.Time) string {
			// Async: stage a correlated refresh into the broker and PARK on it. The
			// stage does not count as done until a matching outcome is observed (see
			// resolvePending) — merely queuing the request is not recovery.
			return w.stageSessionRefresh(st, watcher.RefreshStreamInfo, st.RecoveryStage-1, "stream_info", now,
				"asked the slot broker to re-fetch stream info (broadcast, game, campaign IDs, payload)")
		},
	},
	{
		name:  "transport_probe",
		label: "watch transport probe",
		run: func(w *ProgressWatchdog, st *dropState, _ time.Time) string {
			streamer, authorityCurrent := w.recoveryStreamer(st)
			if !authorityCurrent {
				st.recoveryDeferred = true
				return "watch transport probe deferred: recovery authority changed"
			}
			if streamer == nil || w.prober == nil {
				return "transport probe skipped: channel object unavailable"
			}
			var (
				permit watcher.ObservationPermit
				ok     bool
			)
			if st.provisionalProof {
				// A promoted proof has no active lease. Revalidate its exact
				// proof/candidate/session ownership atomically with permit grant.
				permit, ok = w.watch.AcquireProvisionalProofPermit(
					streamer, st.provisionalProofID, st.provisionalCandidate,
				)
			} else {
				leaseID := uint64(0)
				if st.provisional {
					leaseID = st.provisionalLeaseID
				}
				permit, ok = w.watch.AcquireObservationPermit(streamer, leaseID)
			}
			if !ok {
				// Denial means another broker-owned observation could make this
				// beacon causally ambiguous. It is a deferral, not a failed probe,
				// and therefore must not advance the recovery pipeline.
				st.recoveryDeferred = true
				return "watch transport probe deferred: observation permit unavailable"
			}
			defer w.watch.ReleaseObservationPermit(permit)
			ctx, cancel := context.WithTimeout(w.parentCtx(), recoveryStageTimeout)
			defer cancel()
			res := w.prober.Probe(ctx, streamer)
			if res.OK {
				return "watch transport verified end-to-end: playback token, playlist, segment, and beacon all accepted"
			}
			return fmt.Sprintf("watch transport probe failed at the %s stage (%s)", res.Stage, res.ErrorCode)
		},
	},
	{
		name:  "session_recreate",
		label: "watch session recreate",
		run: func(w *ProgressWatchdog, st *dropState, now time.Time) string {
			return w.stageSessionRefresh(st, watcher.RefreshSession, st.RecoveryStage-1, "session_recreate", now,
				"asked the slot broker to recreate the watch session (spade URL, stream info, beacon payload)")
		},
	},
	{
		name:  "channel_switch",
		label: "channel switch",
		run: func(w *ProgressWatchdog, st *dropState, now time.Time) string {
			if st.provisional {
				// A provisional failure is scoped to the exact candidate/session;
				// it must not create a broad channel-wide avoid entry.
				return "channel switch skipped for provisional observation: exact-tuple quarantine is narrower"
			}
			if w.avoid == nil || st.Channel == "" {
				return "channel switch skipped: no avoid list or no farming channel"
			}
			cfg := w.snapshotCfg()
			w.avoid.Avoid(st.Channel, now.Add(cfg.AvoidTTL), "drop progress stalled on this channel despite session recovery")
			st.avoidedChannel = st.Channel
			return fmt.Sprintf("temporarily excluded %s from watching (%s) — the slot broker will pick the next eligible channel", st.Channel, cfg.AvoidTTL)
		},
	},
	{
		name:  "notify",
		label: "critical notification",
		run: func(w *ProgressWatchdog, st *dropState, now time.Time) string {
			if st.provisional {
				// Re-read and compare the exact lease before quarantining. Session
				// churn must never let an old watchdog episode poison a new lease.
				lease, ok := w.watch.ProvisionalLease()
				if !ok || lease.LeaseID != st.provisionalLeaseID ||
					!lease.Candidate.SameLeaseIdentity(st.provisionalCandidate) {
					st.exhaustedAt = now
					return "provisional recovery exhausted after the lease changed; stale candidate not quarantined"
				}
				if w.watch.QuarantineProvisionalLease(st.provisionalLeaseID, st.provisionalCandidate) {
					st.exhaustedAt = now
					return "provisional recovery exhausted — exact channel/drop/session tuple quarantined in memory"
				}
				if current, currentOK := w.watch.ProvisionalLease(); currentOK &&
					current.LeaseID == st.provisionalLeaseID &&
					current.Candidate.SameLeaseIdentity(st.provisionalCandidate) {
					st.recoveryDeferred = true
					st.terminalDeferred = true
					st.terminalDeferredRun = st.provisionalLastRun
					st.terminalDeferredAt = st.provisionalLastAt
					return "provisional quarantine deferred until observation ownership drains and a strictly fresh exact-Drop observation completes"
				}
				st.exhaustedAt = now
				return "provisional recovery exhausted — exact lease was already released"
			}
			st.exhaustedAt = now
			if w.notifier != nil && !st.notifiedStalled {
				st.notifiedStalled = true
				w.notifier.NotifyDropStalled(st.CampaignName, st.DropName, st.Channel, st.Detail)
			}
			events.Record(events.TypeDropStalled, st.Channel, st.CampaignName+" / "+st.DropName)
			return "automatic recovery exhausted — operator notified"
		},
	},
}

// ProgressWatchdog detects a tracked drop whose minutes stop accruing even
// though everything upstream looks healthy (OAuth, GQL, channel online,
// beacons accepted, campaign active) and runs the staged recovery pipeline
// above. Detection is deliberately conjunctive — a stall confirms only when
// every gate holds (see evaluateDrop) — and recovery is finite: each stage is
// cooldown-bounded and idempotent, and the pipeline never loops.
type ProgressWatchdog struct {
	center   *Center
	drops    DropsView
	watch    WatchView
	prober   Prober
	notifier DropNotifier
	avoid    *AvoidList
	resolver StreamerResolver
	now      func() time.Time

	mu     sync.Mutex
	cfg    WatchdogConfig
	states map[string]*dropState // loop-owned; under mu only for UpdateSettings-driven resets
	reqSeq uint64                // loop-owned monotone counter minting unique refresh RequestIDs
	ctx    context.Context
	cancel context.CancelFunc

	snap atomic.Pointer[ProgressSnapshot]
}

// NewProgressWatchdog builds the watchdog. notifier, avoid, and resolver may
// be nil (the corresponding stages then degrade to documented no-ops).
func NewProgressWatchdog(center *Center, dropsView DropsView, watch WatchView, prober Prober, notifier DropNotifier, avoid *AvoidList, resolver StreamerResolver, cfg WatchdogConfig) *ProgressWatchdog {
	w := &ProgressWatchdog{
		center:   center,
		drops:    dropsView,
		watch:    watch,
		prober:   prober,
		notifier: notifier,
		avoid:    avoid,
		resolver: resolver,
		now:      time.Now,
		cfg:      cfg,
		states:   make(map[string]*dropState),
	}
	if watch != nil {
		watch.SetProvisionalMonitoringEnabled(cfg.Enabled)
	}
	w.publish(ProgressSnapshot{Enabled: cfg.Enabled})
	return w
}

func (w *ProgressWatchdog) Start(ctx context.Context) {
	if w.watch != nil {
		w.watch.SetProvisionalMonitoringEnabled(w.snapshotCfg().Enabled)
	}
	w.mu.Lock()
	w.ctx, w.cancel = context.WithCancel(ctx)
	w.mu.Unlock()
	go w.loop()
}

func (w *ProgressWatchdog) Stop() {
	w.mu.Lock()
	if w.cancel != nil {
		w.cancel()
	}
	w.mu.Unlock()
	if w.watch != nil {
		w.watch.SetProvisionalMonitoringEnabled(false)
	}
}

// UpdateSettings applies a runtime configuration change without a restart.
func (w *ProgressWatchdog) UpdateSettings(cfg WatchdogConfig) {
	w.mu.Lock()
	w.cfg = cfg
	w.mu.Unlock()
	if w.watch != nil {
		w.watch.SetProvisionalMonitoringEnabled(cfg.Enabled)
	}
}

// Snapshot returns the last published per-drop watchdog state. Lock-free.
func (w *ProgressWatchdog) Snapshot() ProgressSnapshot {
	if s := w.snap.Load(); s != nil {
		out := *s
		out.Drops = append([]DropProgress(nil), s.Drops...)
		return out
	}
	return ProgressSnapshot{}
}

// AvoidEntries exposes the active channel exclusions for the debug snapshot.
func (w *ProgressWatchdog) AvoidEntries() []AvoidEntry {
	if w.avoid == nil {
		return nil
	}
	return w.avoid.Entries()
}

func (w *ProgressWatchdog) loop() {
	w.mu.Lock()
	ctx := w.ctx
	w.mu.Unlock()
	for {
		j := (rand.Float64() - 0.5) * 0.4
		timer := time.NewTimer(time.Duration(float64(watchdogEvalCadence) * (1 + j)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			w.evaluate(w.now())
		}
	}
}

func (w *ProgressWatchdog) parentCtx() context.Context {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ctx != nil {
		return w.ctx
	}
	return context.Background()
}

func (w *ProgressWatchdog) snapshotCfg() WatchdogConfig {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cfg
}

// resolveStreamer returns the live streamer for a login, or nil.
func (w *ProgressWatchdog) resolveStreamer(login string) *models.Streamer {
	if w.resolver == nil || login == "" {
		return nil
	}
	return w.resolver(login)
}

// recoveryStreamer returns the exact broker-private owner for provisional
// authority and the configured/discovery resolver result only for ordinary
// monitoring. Login alone is not an identity boundary: configured and
// discovery Streamer objects may legitimately share it while carrying
// different sessions.
func (w *ProgressWatchdog) recoveryStreamer(st *dropState) (*models.Streamer, bool) {
	if st.provisional || st.provisionalProof {
		authorityID := st.provisionalLeaseID
		if st.provisionalProof {
			authorityID = st.provisionalProofID
		}
		owner, ok := w.watch.ProvisionalOwner(authorityID, st.provisionalCandidate)
		return owner, ok && owner != nil && owner == st.provisionalOwner
	}
	return w.resolveStreamer(st.Channel), true
}

// twitchOutage reports whether the health center currently shows evidence of
// a Twitch-side (or account-side) outage — GQL, PubSub, OAuth, or the canary's
// watch transport failing. During an outage stalls are expected and must not
// confirm (the spec's "no active Twitch outage state" gate).
func (w *ProgressWatchdog) twitchOutage() (bool, string) {
	if w.center == nil {
		return false, ""
	}
	snap := w.center.Snapshot()
	for _, name := range []string{SignalOAuth, SignalGQLAPI, SignalPubSub, SignalWatchTransport} {
		// A degraded (flapping/repeatedly-failing) transport counts as an outage
		// here too: while the network is impaired, drop stalls are expected and
		// must not be confirmed against the streamer.
		if sig, ok := snap.Signal(name); ok && (sig.Status == StatusFailed || sig.Status == StatusDegraded) {
			return true, name
		}
	}
	return false, ""
}

// farmingChannel returns the slotted login whose streamer is assigned this
// campaign (game match + advertised campaign + channel allow-list, as encoded
// by the drops tracker's intersection), or "".
func (w *ProgressWatchdog) farmingChannel(campaign *models.Campaign, drop *models.Drop) string {
	for _, slot := range w.watch.BrokerSnapshot().Slots {
		streamer := w.resolveStreamer(slot.Channel)
		if streamer == nil {
			continue
		}
		for _, c := range streamer.Stream.GetCampaigns() {
			if assignedCampaignMatchesDrop(c, campaign.ID, drop.ID, campaign.Game) {
				return slot.Channel
			}
		}
	}
	return ""
}

func assignedCampaignMatchesDrop(assigned *models.Campaign, campaignID, dropID string, game *models.Game) bool {
	if assigned == nil || assigned.ID != campaignID || assigned.Game == nil || game == nil ||
		assigned.Game.ID != game.ID {
		return false
	}
	drop := assigned.CurrentDrop()
	if drop == nil || drop.ID != dropID {
		return false
	}
	return drop.HasPreconditionsMet == nil || *drop.HasPreconditionsMet
}

func assignedCampaignMatchesCandidate(assigned *models.Campaign, candidate models.ProvisionalDropCandidate) bool {
	if assigned == nil || assigned.ID != candidate.CampaignID || assigned.Game == nil ||
		assigned.Game.ID != candidate.GameID {
		return false
	}
	drop := assigned.CurrentDrop()
	return drop != nil && drop.ID == candidate.DropID &&
		drop.HasPreconditionsMet != nil && *drop.HasPreconditionsMet
}

func (w *ProgressWatchdog) proofHasConfirmedFarmingChannel(proof watcher.ProvisionalProof) bool {
	key := proof.Candidate.CampaignID + "\x00" + proof.Candidate.DropID
	w.mu.Lock()
	st := w.states[key]
	matchedProof := st != nil && st.provisionalProof && st.provisionalProofID == proof.ProofID
	matchedJustProvenLease := st != nil && st.provisional && st.provisionalLeaseID == proof.ProofID
	if (!matchedProof && !matchedJustProvenLease) ||
		!st.provisionalCandidate.SameProofIdentity(proof.Candidate) {
		w.mu.Unlock()
		return false
	}
	owner := st.provisionalOwner
	w.mu.Unlock()

	// Login is not an owner identity: configured and discovery streamers can
	// share it. Only the exact broker-private owner previously bound to this
	// proof may supersede the alternate authority with a confirmed assignment.
	if owner == nil || owner.GetUsername() != proof.Candidate.Login ||
		owner.ChannelID != proof.Candidate.ChannelID || !w.watch.IsWatching(proof.Candidate.Login) {
		return false
	}
	for _, campaign := range owner.Stream.GetCampaigns() {
		if assignedCampaignMatchesCandidate(campaign, proof.Candidate) {
			return true
		}
	}
	return false
}

// evaluate is one watchdog pass over every tracked campaign's current drop:
// update progress evidence, check the stall gates, and — at most once per
// pass, to keep API load minimal — run the next recovery stage of the
// worst-off drop. Runs on the watchdog goroutine.
func (w *ProgressWatchdog) evaluate(now time.Time) {
	cfg := w.snapshotCfg()
	if !cfg.Enabled {
		if w.watch != nil {
			w.watch.SetProvisionalMonitoringEnabled(false)
		}
		w.mu.Lock()
		w.states = make(map[string]*dropState)
		w.mu.Unlock()
		w.publish(ProgressSnapshot{Enabled: false, EvaluatedAt: now})
		return
	}
	// Disabled miner configurations historically construct this component with
	// nil dependencies. Keep later runtime toggles nil-safe as well: publishing
	// an enabled-but-empty snapshot is preferable to crashing a delayed loop
	// tick while configuration wiring catches up.
	if w.drops == nil || w.watch == nil {
		w.publish(ProgressSnapshot{Enabled: true, EvaluatedAt: now})
		return
	}

	sync := w.drops.SyncStatus()
	outage, outageSignal := w.twitchOutage()
	campaigns := w.drops.Campaigns()
	brokerSnapshot := w.drops.BrokerCampaignSnapshot()
	brokerCampaigns := brokerSnapshot.Campaigns
	provisionalRevision := brokerSnapshot.SourceRevision
	provisionalRevisionCurrent := brokerSnapshot.Generation != 0 &&
		provisionalRevision == brokerSnapshot.CurrentRevision &&
		brokerSnapshot.CurrentRevision == sync.Revision
	lease, hasLease := w.watch.ProvisionalLease()
	proofs := w.watch.ProvisionalProofs()

	seen := make(map[string]bool)
	var stageBudget bool // at most one recovery-stage execution per pass

	for _, campaign := range campaigns {
		if campaign != nil {
			drop := campaign.CurrentDrop()
			if drop != nil && hasLease && campaign.ID == lease.Candidate.CampaignID && drop.ID == lease.Candidate.DropID {
				// This exact tuple is intentionally not a confirmed assignment yet;
				// its state is evaluated below through the broker-owned lease.
				continue
			}
			var matchingProof *watcher.ProvisionalProof
			for i := range proofs {
				proof := &proofs[i]
				if drop != nil && campaign.ID == proof.Candidate.CampaignID && drop.ID == proof.Candidate.DropID {
					matchingProof = proof
					break
				}
			}
			proofMatches := matchingProof != nil
			if proofMatches && !w.proofHasConfirmedFarmingChannel(*matchingProof) {
				// A promoted provisional proof remains explicit server authority,
				// not a Stream.Campaigns assignment. It has its own exact-observation
				// monitoring path below.
				continue
			}
			if drop != nil && !proofMatches {
				key := campaign.ID + "\x00" + drop.ID
				if w.retainProvisionalRebindGap(key, brokerCampaigns, provisionalRevisionCurrent, now) {
					// A successful health-owned session refresh invalidates the old
					// strict lease before the next broker tick can publish its rebound
					// Pending lease. Preserve only that exact, deadline-bounded episode;
					// otherwise trackDrop below would erase its pending correlation and
					// restart recovery at stage zero forever.
					seen[key] = true
					continue
				}
			}
		}
		st, key, drop := w.trackDrop(campaign, sync, now)
		if st == nil {
			continue
		}
		seen[key] = true
		w.observeProgress(st, campaign, drop, sync, now)

		if hold, why := w.gatesHold(st, campaign, drop, sync, outage, outageSignal, cfg, now); !hold {
			// A gate failing means a stall cannot be *confirmed* right now. The
			// recovery stage and notification flags survive (a one-tick slot
			// rotation must not restart the pipeline), but the stall EVIDENCE is
			// discarded: whatever accrued while the gate failed does not prove
			// farming-without-credit, and carrying it over would confirm a stall
			// minutes after farming resumes.
			st.resetEvidence()
			if st.Status != ProgressStalled {
				st.Status = ProgressHealthy
			}
			st.Detail = why
			continue
		}

		if st.evidenceSince.IsZero() {
			// Every gate holds again: a fresh evidence window starts here. Seed
			// the observation cursor at the CURRENT sync timestamp so inventory
			// reads that completed before this moment (including the one whose
			// data showed the last progress) are never counted as no-progress
			// observations.
			st.evidenceSince = now
			st.lastObservedSyncAt = sync.ProgressLastSyncAt
			st.NoProgressObs = 0
		} else if sync.ProgressLastError == "" && !sync.ProgressLastSyncAt.IsZero() && sync.ProgressLastSyncAt.After(st.lastObservedSyncAt) {
			// A NEW inventory observation completed successfully inside the
			// evidence window without progress — "checked and unchanged", never
			// "could not check".
			st.lastObservedSyncAt = sync.ProgressLastSyncAt
			st.NoProgressObs++
		}

		stalled := now.Sub(st.evidenceSince) >= cfg.StallDelay &&
			st.NoProgressObs >= cfg.StallConfirmations &&
			st.ReportsSinceProgress >= stallMinReports
		if !stalled {
			if st.Status != ProgressStalled {
				st.Status = ProgressHealthy
				st.Detail = fmt.Sprintf("progress monitored: last advance %s ago, %s of farming evidence, %d clean observations, %d reports",
					now.Sub(st.LastProgressAt).Round(time.Minute), now.Sub(st.evidenceSince).Round(time.Minute),
					st.NoProgressObs, st.ReportsSinceProgress)
			}
			continue
		}

		if !stageBudget && w.advanceRecovery(st, cfg, now) {
			stageBudget = true
		}
	}

	if hasLease {
		if key, keep := w.evaluateProvisional(
			now, cfg, outage, outageSignal, brokerCampaigns, provisionalRevision, provisionalRevisionCurrent, lease, &stageBudget,
		); keep {
			seen[key] = true
		}
	}
	for _, proof := range proofs {
		proofKey := proof.Candidate.CampaignID + "\x00" + proof.Candidate.DropID
		if seen[proofKey] {
			// The campaigns loop already established higher ordinary authority for
			// this exact Drop in this pass. Cache that decision: a concurrent
			// assignment clear must not let the stale proof snapshot recreate and
			// reset the state before the next broker tick.
			continue
		}
		if hasLease && proof.Candidate.CampaignID == lease.Candidate.CampaignID &&
			proof.Candidate.DropID == lease.Candidate.DropID {
			// ObserveProvisionalProgress publishes the proof before the broker's
			// next tick consumes the proven lease. The lease path is authoritative
			// during that short overlap. A concurrently-pruned stale proof for the
			// same health key must not overwrite the active lease state either.
			continue
		}
		if w.proofHasConfirmedFarmingChannel(proof) {
			// Assignment may have become visible after the campaigns loop made its
			// decision. Preserve the established proof state for this pass; the next
			// snapshot transitions it in place through trackDrop.
			seen[proofKey] = true
			continue
		}
		if key, keep := w.evaluateProvisionalProof(
			now, cfg, outage, outageSignal, campaigns, brokerCampaigns, sync,
			provisionalRevision, provisionalRevisionCurrent, proof, &stageBudget,
		); keep {
			seen[key] = true
		}
	}
	// The account campaign view can disappear in the same narrow broker-tick
	// gap. Give an already-correlated episode the same strict bounded retention
	// check before generic unseen cleanup; currentProvisionalDrop inside the
	// helper still requires the broker-facing campaign/drop truth to exist.
	w.mu.Lock()
	gapKeys := make([]string, 0)
	for key, st := range w.states {
		if !seen[key] && st.provisional && st.pending != nil {
			gapKeys = append(gapKeys, key)
		}
	}
	w.mu.Unlock()
	for _, key := range gapKeys {
		if w.retainProvisionalRebindGap(key, brokerCampaigns, provisionalRevisionCurrent, now) {
			seen[key] = true
		}
	}

	// Drop state for campaigns/drops no longer tracked (claimed, claimable,
	// expired, campaign gone) — their episodes are over. An episode that
	// escalated must not leave dangling effects behind: the avoided channel
	// gets a clean slate, and a standing critical alert is explicitly closed
	// (a claimable/claimed drop means the stall resolved; an ended campaign
	// makes the alert moot either way).
	// Recovered notifications are collected under w.mu and fired AFTER it is
	// released: NotifyDropRecovered reaches the notifications manager, which
	// does SQLite (and possibly Discord) I/O — running it under w.mu would hold
	// the lock across that I/O and pin a w.mu→notifications lock order. This
	// mirrors advanceRecovery, which also notifies outside the lock.
	type recoveredNote struct{ campaign, drop, channel string }
	var recovered []recoveredNote

	w.mu.Lock()
	for key, st := range w.states {
		if seen[key] {
			continue
		}
		if st.avoidedChannel != "" && w.avoid != nil {
			w.avoid.Clear(st.avoidedChannel)
		}
		if st.notifiedStalled && w.notifier != nil {
			recovered = append(recovered, recoveredNote{st.CampaignName, st.DropName, st.Channel})
		}
		delete(w.states, key)
	}
	w.mu.Unlock()

	for _, n := range recovered {
		w.notifier.NotifyDropRecovered(n.campaign, n.drop, n.channel,
			"the drop left the tracked set (claimed, claimable, or campaign ended) — the stall alert no longer applies")
	}

	w.publishFromStates(now)
}

// evaluateProvisional coordinates the lower-authority UNKNOWN bootstrap with
// the exact server-progress owner. The broker owns admission, slot capacity and
// causal exclusivity; this method only captures a post-reservation Inventory
// baseline, judges fresh monotone exact-Drop observations, and routes a proven
// no-progress episode through the existing bounded recovery pipeline.
func (w *ProgressWatchdog) evaluateProvisional(
	now time.Time,
	cfg WatchdogConfig,
	outage bool,
	outageSignal string,
	campaigns []*models.Campaign,
	sourceRevision uint64,
	revisionCurrent bool,
	lease watcher.ProvisionalLease,
	stageBudget *bool,
) (string, bool) {
	campaign, drop := currentProvisionalDrop(campaigns, lease.Candidate, now)
	key := lease.Candidate.CampaignID + "\x00" + lease.Candidate.DropID
	owner, owns := w.watch.ProvisionalOwner(lease.LeaseID, lease.Candidate)
	if !owns {
		return key, false
	}
	if campaign == nil || drop == nil {
		// The account campaign/drop truth that minted the proposal disappeared.
		// This is not negative channel evidence and therefore is released without
		// quarantine.
		w.watch.ReleaseProvisionalLease(lease.LeaseID)
		return key, false
	}

	st := w.provisionalState(lease, owner, now)
	obs := w.drops.ProgressObservation(lease.Candidate.CampaignID, lease.Candidate.DropID)
	acceptedNoProgress := false

	if lease.State == watcher.ProvisionalLeasePending {
		if !st.provisionalSyncAsked {
			// Capture whatever exact observation currently exists, including an
			// errored/missing one, then force one lightweight Inventory refresh.
			// No beacon is permitted by the broker until a strictly newer clean
			// exact observation is accepted below: Found arms the baseline, while
			// exhaustive absence/tuple-unknown each authorize one observation send.
			st.provisionalCaptureRun = obs.Run
			st.provisionalCaptureAt = obs.ObservedAt
			st.provisionalSyncAsked = true
			st.Detail = "provisional lease reserved; awaiting a fresh exact-Drop Inventory baseline"
			w.drops.TriggerProgressSync()
			return key, true
		}
		cursorRun, cursorAt := st.provisionalCaptureRun, st.provisionalCaptureAt
		if lease.MaxRun > cursorRun {
			cursorRun, cursorAt = lease.MaxRun, lease.MaxAt
		}
		if strictlyNewerObservation(obs, cursorRun, cursorAt) {
			if !cleanProvisionalObservation(obs, lease.Candidate, sourceRevision, revisionCurrent) {
				// Missing/null/malformed data and a campaign-revision race are
				// UNKNOWN, never numeric zero or negative evidence.
				w.watch.ReleaseProvisionalLease(lease.LeaseID)
				st.Detail = "fresh Inventory observation was incomplete or from another campaign revision; provisional lease released without a negative"
				return key, false
			}
			switch {
			case obs.Found && !obs.AuthoritativeAbsent && !obs.TupleUnknown:
				if obs.Minutes < 0 || (st.LastMinutes > 0 && obs.Minutes < st.LastMinutes) ||
					!w.watch.ArmProvisionalLease(lease.LeaseID, obs.Run, obs.ObservedAt, obs.Minutes) {
					w.watch.ReleaseProvisionalLease(lease.LeaseID)
					st.Detail = "fresh exact-Drop baseline was unavailable or non-monotone; provisional lease released without a negative"
					return key, false
				}
				st.LastMinutes = obs.Minutes
				if st.RecoveryStage == 0 {
					st.LastProgressAt = now
				}
				st.provisionalLastRun = obs.Run
				st.provisionalLastAt = obs.ObservedAt
				st.resetEvidence()
				w.observeReports(st, lease.Candidate.Login)
				if st.RecoveryStage == 0 {
					st.Status = ProgressHealthy
				} else {
					st.Status = ProgressRecovering
				}
				st.Detail = fmt.Sprintf("provisional exact-Drop baseline armed at %d minutes; server delta required", obs.Minutes)
				return key, true
			case obs.AuthoritativeAbsent && !obs.Found && !obs.TupleUnknown:
				// LastMinutes may retain the prior lease's monotone display watermark
				// across a health-owned session rebind. The rebound lease is still
				// baseline-free, so an exhaustive absence remains valid observation
				// evidence and never means that watermark regressed to zero.
				if lease.PendingObservation != watcher.ProvisionalPendingObservationAbsence {
					// Each accepted absence opens exactly one broker-owned normal send.
					// Baseline delivery accounting before the first absence (or after a
					// tuple-unknown pause) so only that subsequently authorized send can
					// contribute to the stall-delivery gate.
					w.observeReports(st, lease.Candidate.Login)
				}
				if !w.watch.ObserveProvisionalAbsence(lease.LeaseID, obs.Run, obs.ObservedAt) {
					st.Detail = "provisional lease changed before complete exact absence could be recorded"
					return key, false
				}
				fresh, ok := w.watch.ProvisionalLease()
				if !ok || fresh.LeaseID != lease.LeaseID || !fresh.Candidate.SameLeaseIdentity(lease.Candidate) {
					return key, false
				}
				lease = fresh
				st.provisionalLastRun = obs.Run
				st.provisionalLastAt = obs.ObservedAt
				acceptedNoProgress = true
				if st.terminalDeferred && strictlyNewerObservation(obs, st.terminalDeferredRun, st.terminalDeferredAt) {
					st.terminalDeferred = false
				}
			case obs.TupleUnknown && !obs.Found && !obs.AuthoritativeAbsent:
				// The exhaustive response contained the target tuple but its progress
				// row was nullable/unknown. This is observability, not an absence or a
				// numeric baseline. Advance the exact cursor and authorize one bounded
				// materialization send, while pausing all accumulated stall evidence.
				if !w.watch.ObserveProvisionalTupleUnknown(lease.LeaseID, obs.Run, obs.ObservedAt) {
					st.Detail = "provisional lease changed before tuple-unknown observation could be recorded"
					return key, false
				}
				fresh, ok := w.watch.ProvisionalLease()
				if !ok || fresh.LeaseID != lease.LeaseID || !fresh.Candidate.SameLeaseIdentity(lease.Candidate) {
					return key, false
				}
				lease = fresh
				st.provisionalLastRun = obs.Run
				st.provisionalLastAt = obs.ObservedAt
				st.resetEvidence()
				if st.RecoveryStage == 0 {
					st.Status = ProgressHealthy
				} else {
					st.Status = ProgressRecovering
				}
				st.Detail = "exact Drop tuple was present with unknown progress; stall evidence paused and one materialization observation authorized"
				return key, true
			default:
				w.watch.ReleaseProvisionalLease(lease.LeaseID)
				st.Detail = "fresh Inventory result did not contain authoritative exact-Drop progress; provisional lease released without a negative"
				return key, false
			}
		} else if lease.MaxRun == 0 {
			st.Detail = "provisional lease reserved; no newer Inventory observation has completed"
			return key, true
		}
		// A Pending lease with MaxRun/MaxAt has observed an explicit complete
		// array absence. It remains baseline-free, but broker-owned sends and the
		// ordinary stall-evidence/recovery owner are now allowed. Only a later
		// exact Found row can arm the numeric baseline.
		if st.provisionalLastRun == 0 {
			st.provisionalLastRun = lease.MaxRun
			st.provisionalLastAt = lease.MaxAt
		}
		if lease.PendingObservation == watcher.ProvisionalPendingObservationTupleUnknown {
			st.resetEvidence()
			if st.RecoveryStage == 0 {
				st.Status = ProgressHealthy
			} else {
				st.Status = ProgressRecovering
			}
			st.Detail = "exact Drop tuple progress remains unknown; stall evidence paused"
			return key, true
		}
		w.observeReports(st, lease.Candidate.Login)
	}

	// A restarted watchdog can inherit an already-armed broker lease. Its
	// baseline/max are broker-published server observations, so adopt them rather
	// than manufacturing another baseline or allowing an ACK to stand in for one.
	if lease.State != watcher.ProvisionalLeasePending && st.provisionalLastRun == 0 {
		st.LastMinutes = lease.MaxMinutes
		st.LastProgressAt = now
		st.provisionalLastRun = lease.MaxRun
		st.provisionalLastAt = lease.MaxAt
	}
	if lease.State != watcher.ProvisionalLeasePending {
		w.observeReports(st, lease.Candidate.Login)
	}

	if lease.State != watcher.ProvisionalLeasePending && strictlyNewerObservation(obs, lease.MaxRun, lease.MaxAt) &&
		(!cleanExactObservation(obs, lease.Candidate, sourceRevision, revisionCurrent) || obs.Minutes < lease.MaxMinutes) {
		// A newer failed, missing, or regressed result is UNKNOWN/incoherent,
		// never zero-progress evidence. Release the still-unproved lease without
		// quarantine so lower-authority work cannot hold a slot indefinitely.
		w.watch.ReleaseProvisionalLease(lease.LeaseID)
		st.Detail = "newer exact progress observation was unavailable or non-monotone; provisional lease released without a negative"
		return key, false
	}
	if lease.State != watcher.ProvisionalLeasePending && strictlyNewerObservation(obs, lease.MaxRun, lease.MaxAt) {
		if w.watch.ObserveProvisionalProgress(lease.LeaseID, obs.Run, obs.ObservedAt, obs.Minutes) {
			st.provisionalLastRun = obs.Run
			st.provisionalLastAt = obs.ObservedAt
			fresh, ok := w.watch.ProvisionalLease()
			if !ok || fresh.LeaseID != lease.LeaseID || !fresh.Candidate.SameLeaseIdentity(lease.Candidate) {
				return key, false
			}
			lease = fresh
			if lease.State == watcher.ProvisionalLeaseProven {
				// Only this fresh post-baseline exact server delta proves the
				// candidate. Delivery counters never enter this branch.
				st.LastMinutes = lease.MaxMinutes
				st.LastProgressAt = now
				st.RecoveryStage = 0
				st.RecoveryStageName = ""
				st.LastRecoveryAt = time.Time{}
				st.exhaustedAt = time.Time{}
				st.pending = nil
				st.terminalDeferred = false
				st.resetEvidence()
				st.Status = ProgressHealthy
				st.Detail = fmt.Sprintf("provisional candidate proved by fresh exact-Drop server progress: %d -> %d minutes", lease.BaselineMinutes, lease.MaxMinutes)
				return key, true
			}
			acceptedNoProgress = true
			if st.terminalDeferred && strictlyNewerObservation(obs, st.terminalDeferredRun, st.terminalDeferredAt) {
				st.terminalDeferred = false
			}
		}
	}

	if lease.State == watcher.ProvisionalLeaseProven {
		st.LastMinutes = lease.MaxMinutes
		st.Status = ProgressHealthy
		st.Detail = fmt.Sprintf("provisional candidate proved by fresh exact-Drop server progress: %d -> %d minutes", lease.BaselineMinutes, lease.MaxMinutes)
		return key, true
	}

	if hold, why := w.provisionalGatesHold(
		st, lease, obs, sourceRevision, revisionCurrent, outage, outageSignal, cfg, now,
	); !hold {
		st.resetEvidence()
		if st.Status != ProgressStalled {
			st.Status = ProgressHealthy
		}
		st.Detail = why
		return key, true
	}

	if st.evidenceSince.IsZero() {
		st.evidenceSince = now
		st.NoProgressObs = 0
	} else if acceptedNoProgress {
		st.NoProgressObs++
	}

	stalled := now.Sub(st.evidenceSince) >= cfg.StallDelay &&
		st.NoProgressObs >= cfg.StallConfirmations &&
		st.ReportsSinceProgress >= stallMinReports
	if !stalled {
		if st.Status != ProgressStalled {
			st.Status = ProgressHealthy
			st.Detail = fmt.Sprintf("provisional observation: %s of farming evidence, %d clean no-progress observations, %d reports",
				now.Sub(st.evidenceSince).Round(time.Minute), st.NoProgressObs, st.ReportsSinceProgress)
		}
		return key, true
	}
	if st.terminalDeferred {
		st.Status = ProgressRecovering
		st.Detail = "provisional terminal quarantine is awaiting a fresh exact-Drop observation after permit drain"
		return key, true
	}

	if !*stageBudget && w.advanceRecovery(st, cfg, now) {
		*stageBudget = true
		if st.provisional && st.RecoveryStage >= len(recoveryStages) {
			// Terminal provisional handling is the narrow broker quarantine
			// above, not a generic Drops Progress STALLED alert. Retire the
			// watchdog state before publishing this pass.
			return key, false
		}
	}
	return key, true
}

// evaluateProvisionalProof keeps monitoring after the broker promotes a
// provisional lease on direct server progress and consumes the lease. The
// proof remains distinct from Stream.Campaigns/AvailableDrops, so it continues
// to use exact Inventory observations as its eligibility/progress authority.
// Once proven, however, recovery is ordinary: channel switch and notification
// apply, and the proof is never retroactively quarantined as "did not prove".
func (w *ProgressWatchdog) evaluateProvisionalProof(
	now time.Time,
	cfg WatchdogConfig,
	outage bool,
	outageSignal string,
	accountCampaigns []*models.Campaign,
	brokerCampaigns []*models.Campaign,
	sync drops.SyncStatus,
	sourceRevision uint64,
	revisionCurrent bool,
	proof watcher.ProvisionalProof,
	stageBudget *bool,
) (string, bool) {
	key := proof.Candidate.CampaignID + "\x00" + proof.Candidate.DropID
	campaign, drop := currentProvisionalDrop(brokerCampaigns, proof.Candidate, now)
	if campaign == nil || drop == nil {
		// A current broker publication that no longer contains this exact current
		// Drop is an authoritative source-prune boundary. Hand the already ordinary
		// recovery episode to dashboard/account monitoring immediately, rather than
		// relying on the independently-published proof slice to disappear in one
		// health pass. A stale proof snapshot may persist for several evaluations;
		// the in-place ordinary state below must survive all of them.
		//
		// If the exact tuple is still present but its game/ACL/source envelope no
		// longer matches proof.Candidate, the campaign publication may simply be one
		// step ahead of watcher proof adoption. Pause instead of handing off/resetting
		// the episode; the watcher will either publish the revalidated envelope or
		// prune the proof.
		if revisionCurrent && !brokerPublishesExactProvisionalTuple(brokerCampaigns, proof.Candidate) &&
			w.handoffProvisionalProofSourceAbsent(accountCampaigns, sync, proof, now) {
			return key, true
		}
		// A revision race or source-envelope adoption gap is UNKNOWN. Preserve the
		// established proof episode for every pass in which the exact proof remains
		// published instead of treating the mismatch as negative authority.
		if w.deferProvisionalProofReconciliation(key, proof) {
			return key, true
		}
		return key, false
	}
	owner, owns := w.watch.ProvisionalOwner(proof.ProofID, proof.Candidate)
	if !owns {
		if w.deferProvisionalProofReconciliation(key, proof) {
			return key, true
		}
		return key, false
	}
	st := w.provisionalProofState(proof, owner)
	w.observeReports(st, proof.Candidate.Login)
	obs := w.drops.ProgressObservation(proof.Candidate.CampaignID, proof.Candidate.DropID)
	acceptedNoProgress := false
	if strictlyNewerObservation(obs, st.provisionalLastRun, st.provisionalLastAt) &&
		cleanExactObservation(obs, proof.Candidate, sourceRevision, revisionCurrent) && obs.Minutes >= st.LastMinutes {
		st.provisionalLastRun = obs.Run
		st.provisionalLastAt = obs.ObservedAt
		if obs.Minutes > st.LastMinutes {
			if st.notifiedStalled && w.notifier != nil {
				w.notifier.NotifyDropRecovered(st.CampaignName, st.DropName, st.Channel,
					fmt.Sprintf("progress resumed: %d minutes", obs.Minutes))
			}
			if st.Status == ProgressStalled || st.RecoveryStage > 0 {
				events.Record(events.TypeDropRecovered, st.Channel, st.CampaignName+" / "+st.DropName)
			}
			if st.avoidedChannel != "" && w.avoid != nil {
				w.avoid.Clear(st.avoidedChannel)
			}
			candidate := st.provisionalCandidate
			proofID := st.provisionalProofID
			*st = dropState{
				DropProgress: DropProgress{
					CampaignID:     candidate.CampaignID,
					CampaignName:   candidate.Campaign,
					DropID:         candidate.DropID,
					DropName:       candidate.Drop,
					Channel:        candidate.Login,
					LastMinutes:    obs.Minutes,
					LastProgressAt: now,
					Status:         ProgressHealthy,
					Detail:         fmt.Sprintf("server-proven alternate Drop progressing: %d minutes", obs.Minutes),
				},
				provisionalProof:     true,
				provisionalProofID:   proofID,
				provisionalCandidate: candidate,
				provisionalOwner:     owner,
				provisionalLastRun:   obs.Run,
				provisionalLastAt:    obs.ObservedAt,
			}
			w.observeReports(st, proof.Candidate.Login)
			return key, true
		}
		acceptedNoProgress = true
	}

	if hold, why := w.provisionalCandidateGatesHold(
		st, st.provisionalCandidate, st.provisionalLastAt, obs, sourceRevision, revisionCurrent,
		outage, outageSignal, cfg, now, true, false,
	); !hold {
		st.resetEvidence()
		if st.Status != ProgressStalled {
			st.Status = ProgressHealthy
		}
		st.Detail = why
		return key, true
	}

	if st.evidenceSince.IsZero() {
		st.evidenceSince = now
		st.NoProgressObs = 0
	} else if acceptedNoProgress {
		st.NoProgressObs++
	}

	stalled := now.Sub(st.evidenceSince) >= cfg.StallDelay &&
		st.NoProgressObs >= cfg.StallConfirmations &&
		st.ReportsSinceProgress >= stallMinReports
	if !stalled {
		if st.Status != ProgressStalled {
			st.Status = ProgressHealthy
			st.Detail = fmt.Sprintf("server-proven provisional Drop: %s of farming evidence, %d clean no-progress observations, %d reports",
				now.Sub(st.evidenceSince).Round(time.Minute), st.NoProgressObs, st.ReportsSinceProgress)
		}
		return key, true
	}

	if !*stageBudget && w.advanceRecovery(st, cfg, now) {
		*stageBudget = true
	}
	return key, true
}

func currentProvisionalDrop(campaigns []*models.Campaign, candidate models.ProvisionalDropCandidate, now time.Time) (*models.Campaign, *models.Drop) {
	for _, campaign := range campaigns {
		if campaign == nil || campaign.ID != candidate.CampaignID ||
			campaign.Game == nil || campaign.Game.ID != candidate.GameID || !candidate.Valid() ||
			(campaign.Status != models.CampaignActive && (campaign.Status != "" || !campaign.InInventory)) ||
			(!campaign.EndAt.IsZero() && now.After(campaign.EndAt)) {
			continue
		}
		drop := campaign.CurrentDrop()
		if drop == nil || drop.ID != candidate.DropID || drop.IsClaimed || drop.IsClaimable ||
			!drop.InActiveWindow() || drop.HasPreconditionsMet == nil || !*drop.HasPreconditionsMet {
			return nil, nil
		}
		// Revalidate the exact typed account-side ACL envelope at the health
		// acceptance boundary. Discovery may have built the candidate from an
		// earlier campaign snapshot; a same-ID game/ACL drift must not be able to
		// arm or promote it merely because the exact Inventory tuple progressed.
		if campaign.ACL.Source == models.ACLSourceNone || !campaign.ACLComplete() {
			return nil, nil
		}
		switch candidate.Evidence {
		case models.ProvisionalEvidenceDirectory:
			if campaign.ACLState() != models.ACLUnrestricted {
				return nil, nil
			}
		case models.ProvisionalEvidenceRestrictedACL:
			if campaign.ACLState() != models.ACLRestricted ||
				!campaign.AllowsChannel(candidate.ChannelID) {
				return nil, nil
			}
			currentACL := append([]string(nil), campaign.ACL.ChannelIDs...)
			sort.Strings(currentACL)
			if len(currentACL) != len(candidate.RestrictedACL) {
				return nil, nil
			}
			for i := range currentACL {
				if currentACL[i] != candidate.RestrictedACL[i] {
					return nil, nil
				}
			}
		default:
			return nil, nil
		}
		return campaign, drop
	}
	return nil, nil
}

// brokerPublishesExactProvisionalTuple deliberately checks only the broker's
// current campaign/Drop identity. Eligibility, game, and ACL/source validation
// belongs to currentProvisionalDrop. Keeping the checks separate distinguishes
// a true source prune from the short publication window where campaign truth is
// newer than the independently adopted proof envelope.
func brokerPublishesExactProvisionalTuple(campaigns []*models.Campaign, candidate models.ProvisionalDropCandidate) bool {
	for _, campaign := range campaigns {
		if campaign == nil || campaign.ID != candidate.CampaignID {
			continue
		}
		drop := campaign.CurrentDrop()
		if drop != nil && drop.ID == candidate.DropID {
			return true
		}
	}
	return false
}

func cleanProvisionalObservation(
	obs drops.ProgressObservation,
	candidate models.ProvisionalDropCandidate,
	sourceRevision uint64,
	revisionCurrent bool,
) bool {
	return revisionCurrent && obs.Revision == sourceRevision && obs.Error == "" && obs.Complete &&
		obs.CampaignID == candidate.CampaignID && obs.DropID == candidate.DropID &&
		obs.Run != 0 && !obs.ObservedAt.IsZero()
}

func cleanExactObservation(
	obs drops.ProgressObservation,
	candidate models.ProvisionalDropCandidate,
	sourceRevision uint64,
	revisionCurrent bool,
) bool {
	return cleanProvisionalObservation(obs, candidate, sourceRevision, revisionCurrent) &&
		obs.Found && !obs.AuthoritativeAbsent && !obs.TupleUnknown && obs.Minutes >= 0
}

func strictlyNewerObservation(obs drops.ProgressObservation, run uint64, at time.Time) bool {
	return obs.Run > run && !obs.ObservedAt.IsZero() && (at.IsZero() || obs.ObservedAt.After(at))
}

func (w *ProgressWatchdog) provisionalState(lease watcher.ProvisionalLease, owner *models.Streamer, now time.Time) *dropState {
	key := lease.Candidate.CampaignID + "\x00" + lease.Candidate.DropID
	outcome, hasOutcome := w.watch.LastSessionRefresh(lease.Candidate.Login)
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.states[key]
	if st == nil || !st.provisional || st.provisionalLeaseID != lease.LeaseID ||
		!st.provisionalCandidate.SameLeaseIdentity(lease.Candidate) {
		if provisionalRecoveryRebind(st, lease, owner, outcome, hasOutcome, now) {
			return st
		}
		st = &dropState{
			DropProgress: DropProgress{
				CampaignID:     lease.Candidate.CampaignID,
				CampaignName:   lease.Candidate.Campaign,
				DropID:         lease.Candidate.DropID,
				DropName:       lease.Candidate.Drop,
				Channel:        lease.Candidate.Login,
				LastProgressAt: now,
				Status:         ProgressHealthy,
				Detail:         "provisional observation lease reserved",
			},
			provisional:          true,
			provisionalLeaseID:   lease.LeaseID,
			provisionalCandidate: lease.Candidate,
			provisionalOwner:     owner,
		}
		w.states[key] = st
	}
	return st
}

// provisionalRecoveryRebind recognizes the one session-generation change that
// belongs to this exact health recovery episode. Stream/session refreshes
// necessarily bump Stream.SessionGeneration, which invalidates the old strict
// lease and causes the broker to reserve a new Pending lease. Without this
// correlated handoff every successful recovery refresh would reset the health
// stage to zero and an unproved candidate could self-loop forever.
//
// The handoff is intentionally narrower than normal lease identity: it requires
// the same private Streamer object, stable account/drop/channel/broadcast and
// evidence facts, an unchanged Known authority epoch, and the exact successful
// outcome for the state's pending RequestID/signature. Any external session
// change, new broadcast, Known publication, source drift, or foreign outcome
// starts a genuinely new episode instead.
func provisionalRecoveryRebind(
	st *dropState,
	lease watcher.ProvisionalLease,
	owner *models.Streamer,
	outcome watcher.SessionRefreshOutcome,
	hasOutcome bool,
	now time.Time,
) bool {
	if st == nil || !st.provisional || st.pending == nil || owner == nil ||
		st.provisionalOwner != owner || lease.State != watcher.ProvisionalLeasePending {
		return false
	}
	before, after := st.provisionalCandidate, lease.Candidate
	p := st.pending
	if !sameProvisionalRecoveryIdentity(before, after) ||
		p.broadcastID == "" || p.broadcastID != before.BroadcastID || p.broadcastID != after.BroadcastID ||
		p.generation == 0 || p.generation != before.SessionGeneration ||
		after.SessionGeneration <= before.SessionGeneration ||
		!hasOutcome || !matchingSuccessfulProvisionalRefresh(before, p, outcome) ||
		outcome.AppliedSessionGeneration != after.SessionGeneration {
		return false
	}

	// Consume the exact successful pending outcome while retaining the already
	// completed recovery stage. The new strict lease must still obtain its own
	// post-reservation exact Inventory baseline before any further beacon.
	st.CampaignName = after.Campaign
	st.DropName = after.Drop
	st.Channel = after.Login
	st.provisionalLeaseID = lease.LeaseID
	st.provisionalCandidate = after
	st.provisionalOwner = owner
	st.provisionalCaptureRun = 0
	st.provisionalCaptureAt = time.Time{}
	st.provisionalLastRun = 0
	st.provisionalLastAt = time.Time{}
	st.provisionalSyncAsked = false
	st.recoveryDeferred = false
	st.terminalDeferred = false
	st.terminalDeferredRun = 0
	st.terminalDeferredAt = time.Time{}
	st.pending = nil
	st.LastRecoveryAt = now
	st.resetEvidence()
	st.Status = ProgressRecovering
	st.Detail = "health recovery refreshed the same broadcast; awaiting a new exact-Drop baseline for the rebound lease"
	return true
}

// retainProvisionalRebindGap covers the narrow ordering window between a
// successful health-owned refresh publishing a new Stream generation and the
// broker's next arbitration publishing the corresponding Pending lease. It
// retains no generic stale state: every correlation/session/source fence must
// still hold and the original pending deadline remains the hard bound.
func (w *ProgressWatchdog) retainProvisionalRebindGap(
	key string,
	campaigns []*models.Campaign,
	revisionCurrent bool,
	now time.Time,
) bool {
	w.mu.Lock()
	st := w.states[key]
	if st == nil || !st.provisional || st.pending == nil {
		w.mu.Unlock()
		return false
	}
	p := *st.pending
	before := st.provisionalCandidate
	owner := st.provisionalOwner
	w.mu.Unlock()

	if owner == nil || !revisionCurrent || now.After(p.deadline) {
		return false
	}
	if campaign, drop := currentProvisionalDrop(campaigns, before, now); campaign == nil || drop == nil {
		return false
	}
	outcome, ok := w.watch.LastSessionRefresh(before.Login)
	if !ok || !matchingSuccessfulProvisionalRefresh(before, &p, outcome) {
		return false
	}
	// The stored pointer came from the broker's exact ProvisionalOwner lookup
	// before the health-owned refresh invalidated the old lease. During this
	// intentional no-lease gap there is no current authority ID to resolve again,
	// and resolving by login could select a configured/discovery clone with the
	// same name. Revalidate the exact private owner directly instead.
	streamer := owner
	if streamer.GetStatus() != models.StatusOnline || streamer.GetUsername() != before.Login ||
		streamer.ChannelID != before.ChannelID {
		return false
	}
	for _, assigned := range streamer.Stream.GetCampaigns() {
		if assigned != nil && assigned.ID == before.CampaignID {
			// Confirmed assignment is higher authority and supersedes the gap
			// immediately rather than waiting for another broker tick.
			return false
		}
	}
	snapshot := streamer.Stream.ProvisionalDropSnapshot()
	if snapshot.GameID != before.GameID || snapshot.BroadcastID != before.BroadcastID ||
		snapshot.SessionGeneration != outcome.AppliedSessionGeneration ||
		snapshot.Availability.State != models.CampaignAvailabilityUnknown ||
		snapshot.Availability.KnownGeneration != before.AvailabilityKnownGen {
		return false
	}

	// Recheck pointer/correlation after the unlocked broker/stream reads so a
	// concurrent disable/reset cannot resurrect a discarded state.
	w.mu.Lock()
	defer w.mu.Unlock()
	current := w.states[key]
	if current != st || current.pending == nil || current.pending.requestID != p.requestID ||
		current.pending.signature != p.signature {
		return false
	}
	current.Status = ProgressRecovering
	current.Detail = "health recovery applied; awaiting the broker's rebound provisional lease"
	return true
}

func matchingSuccessfulProvisionalRefresh(
	before models.ProvisionalDropCandidate,
	p *pendingRecovery,
	outcome watcher.SessionRefreshOutcome,
) bool {
	return p != nil && p.broadcastID != "" && p.broadcastID == before.BroadcastID &&
		p.generation != 0 && p.generation == before.SessionGeneration &&
		outcome.Success && !outcome.Stale && !outcome.Skipped &&
		outcome.RequestID == p.requestID && outcome.Signature == p.signature && outcome.Login == before.Login &&
		outcome.ExpectedBroadcastID == p.broadcastID && outcome.CurrentBroadcastID == before.BroadcastID &&
		outcome.ExpectedSessionGeneration == p.generation &&
		outcome.AppliedSessionGeneration > before.SessionGeneration
}

func sameProvisionalRecoveryIdentity(before, after models.ProvisionalDropCandidate) bool {
	if before.CampaignID != after.CampaignID || before.DropID != after.DropID ||
		before.GameID != after.GameID || before.Login != after.Login || before.ChannelID != after.ChannelID ||
		before.BroadcastID != after.BroadcastID ||
		before.AvailabilityKnownGen != after.AvailabilityKnownGen ||
		before.Evidence != after.Evidence || len(before.RestrictedACL) != len(after.RestrictedACL) {
		return false
	}
	for i := range before.RestrictedACL {
		if before.RestrictedACL[i] != after.RestrictedACL[i] {
			return false
		}
	}
	return true
}

func (w *ProgressWatchdog) provisionalProofState(proof watcher.ProvisionalProof, owner *models.Streamer) *dropState {
	key := proof.Candidate.CampaignID + "\x00" + proof.Candidate.DropID
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.states[key]
	if st == nil || !st.provisionalProof || st.provisionalProofID != proof.ProofID ||
		!st.provisionalCandidate.SameProofIdentity(proof.Candidate) {
		st = &dropState{
			DropProgress: DropProgress{
				CampaignID:     proof.Candidate.CampaignID,
				CampaignName:   proof.Candidate.Campaign,
				DropID:         proof.Candidate.DropID,
				DropName:       proof.Candidate.Drop,
				Channel:        proof.Candidate.Login,
				LastMinutes:    proof.ProvenMinutes,
				LastProgressAt: proof.ProvenAt,
				Status:         ProgressHealthy,
				Detail:         "provisional candidate promoted by exact server progress",
			},
			provisionalProof:     true,
			provisionalProofID:   proof.ProofID,
			provisionalCandidate: proof.Candidate,
			provisionalOwner:     owner,
			provisionalLastRun:   proof.ProvenRun,
			provisionalLastAt:    proof.ProvenAt,
		}
		w.states[key] = st
	} else {
		// A routine UNKNOWN/Directory refresh is a new source envelope but not a
		// new causal proof identity. Preserve recovery evidence while adopting the
		// broker's current envelope so later gates and proof-bound permits never
		// validate against stale observation/evidence metadata. A Known authority
		// epoch or session/broadcast change fails SameProofIdentity above and gets
		// a fresh state.
		st.provisionalCandidate = proof.Candidate
		st.provisionalOwner = owner
		st.CampaignName = proof.Candidate.Campaign
		st.DropName = proof.Candidate.Drop
		st.Channel = proof.Candidate.Login
	}
	return st
}

// handoffProvisionalProofSourceAbsent transitions a server-proven episode to
// ordinary watchdog ownership as soon as the exact current broker campaign
// publication stops backing the proof. The dashboard/account snapshot may lag
// that publication, so it is used only to retain the exact still-tracked Drop;
// ordinary farming gates independently require a matching assigned current
// DropID before any delivery or recovery evidence can accrue.
func (w *ProgressWatchdog) handoffProvisionalProofSourceAbsent(
	accountCampaigns []*models.Campaign,
	sync drops.SyncStatus,
	proof watcher.ProvisionalProof,
	now time.Time,
) bool {
	for _, campaign := range accountCampaigns {
		if campaign == nil || campaign.ID != proof.Candidate.CampaignID {
			continue
		}
		drop := campaign.CurrentDrop()
		if drop == nil || drop.ID != proof.Candidate.DropID {
			return false
		}
		st, _, tracked := w.trackDrop(campaign, sync, now)
		if st == nil || tracked == nil || tracked.ID != proof.Candidate.DropID {
			return false
		}
		st.resetEvidence()
		st.lastObservedSyncAt = sync.ProgressLastSyncAt
		st.Detail = "server-proof source no longer backs the exact Drop; preserving bounded ordinary recovery while confirmed farming revalidates"
		return true
	}
	return false
}

// deferProvisionalProofReconciliation preserves an established promoted-proof
// episode while the exact proof snapshot remains published but its broker
// source revision or owner is temporarily unobservable. Snapshot reconciliation
// can span multiple health evaluations, so every such pass pauses and resets
// evidence without discarding bounded recovery effects. Once the proof snapshot
// disappears, this helper is no longer called and ordinary handoff/cleanup owns
// the state.
func (w *ProgressWatchdog) deferProvisionalProofReconciliation(key string, proof watcher.ProvisionalProof) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.states[key]
	if st == nil || !st.provisionalProof || st.provisionalProofID != proof.ProofID ||
		!st.provisionalCandidate.SameProofIdentity(proof.Candidate) {
		return false
	}
	st.resetEvidence()
	st.Detail = "promoted proof owner/source is temporarily unobservable; preserving the bounded ordinary recovery episode while the exact proof remains published"
	return true
}

func (w *ProgressWatchdog) provisionalGatesHold(
	st *dropState,
	lease watcher.ProvisionalLease,
	obs drops.ProgressObservation,
	sourceRevision uint64,
	revisionCurrent bool,
	outage bool,
	outageSignal string,
	cfg WatchdogConfig,
	now time.Time,
) (bool, string) {
	return w.provisionalCandidateGatesHold(
		st, lease.Candidate, lease.MaxAt, obs, sourceRevision, revisionCurrent,
		outage, outageSignal, cfg, now, false,
		lease.State == watcher.ProvisionalLeasePending &&
			lease.PendingObservation == watcher.ProvisionalPendingObservationAbsence,
	)
}

func (w *ProgressWatchdog) provisionalCandidateGatesHold(
	st *dropState,
	candidate models.ProvisionalDropCandidate,
	lastCleanAt time.Time,
	obs drops.ProgressObservation,
	sourceRevision uint64,
	revisionCurrent bool,
	outage bool,
	outageSignal string,
	cfg WatchdogConfig,
	now time.Time,
	proof bool,
	allowAbsence bool,
) (bool, string) {
	if outage {
		return false, fmt.Sprintf("not counting a provisional stall: Twitch connectivity is degraded (%s failing)", outageSignal)
	}
	if !cleanProvisionalObservation(obs, candidate, sourceRevision, revisionCurrent) {
		return false, "not counting a provisional stall: exact Inventory progress is incomplete or belongs to another campaign revision"
	}
	if !obs.Found && (!allowAbsence || !obs.AuthoritativeAbsent) {
		return false, "not counting a provisional stall: exact Drop progress was unavailable in the latest Inventory observation"
	}
	if lastCleanAt.IsZero() || now.Sub(lastCleanAt) > cfg.StallDelay {
		return false, "not counting a provisional stall: no clean exact-Drop observation completed within the stall-delay window"
	}
	if !w.watch.IsWatching(st.Channel) {
		return false, fmt.Sprintf("%s does not hold its provisional watch slot right now", st.Channel)
	}
	authorityID := st.provisionalLeaseID
	if proof {
		authorityID = st.provisionalProofID
	}
	streamer, owns := w.watch.ProvisionalOwner(authorityID, candidate)
	if !owns || streamer == nil || streamer != st.provisionalOwner || streamer.GetStatus() != models.StatusOnline {
		return false, "provisional channel is no longer confirmed online"
	}
	snapshot := streamer.Stream.ProvisionalDropSnapshot()
	availabilityCurrent := snapshot.Availability.KnownGeneration == candidate.AvailabilityKnownGen
	if !proof {
		availabilityCurrent = availabilityCurrent && snapshot.Availability.ObservationID == candidate.AvailabilityObs
	}
	if snapshot.GameID != candidate.GameID || snapshot.BroadcastID != candidate.BroadcastID ||
		snapshot.SessionGeneration != candidate.SessionGeneration ||
		snapshot.Availability.State != models.CampaignAvailabilityUnknown || !availabilityCurrent {
		return false, "provisional channel/game/availability/session fence changed"
	}
	return true, ""
}

// trackDrop finds (or creates) the watchdog state for the campaign's current
// drop. Returns nil when the campaign has nothing the watchdog should track.
func (w *ProgressWatchdog) trackDrop(campaign *models.Campaign, sync drops.SyncStatus, now time.Time) (*dropState, string, *models.Drop) {
	// Inventory recovery is authoritative current-farming evidence even when
	// Twitch omits the campaign status and date window from that response.
	active := campaign.Status == models.CampaignActive ||
		(campaign.Status == "" && campaign.InInventory)
	if !active || (!campaign.EndAt.IsZero() && now.After(campaign.EndAt)) {
		return nil, "", nil
	}
	drop := campaign.CurrentDrop()
	if drop == nil || drop.IsClaimed || drop.IsClaimable || !drop.InActiveWindow() {
		// Claimable means fully progressed — claiming is the claim flow's job,
		// not a stall.
		return nil, "", nil
	}

	key := campaign.ID + "\x00" + drop.ID
	w.mu.Lock()
	defer w.mu.Unlock()
	st, ok := w.states[key]
	if ok && st.provisionalProof {
		// A promoted proof has already crossed into ordinary watchdog authority.
		// Source omission (including this watchdog's own channel-switch avoid)
		// removes only the broker proof, not the bounded recovery episode. Keep
		// its monotone server watermark, stage, avoid/notification effects and
		// exhaustion state; discard proof-only identity and rebaseline evidence so
		// the next confirmed farmer must establish a fresh ordinary gate window.
		if st.pending != nil {
			st.RecoveryStage = st.pending.stageIndex
			st.RecoveryStageName = ""
			st.LastRecoveryAt = time.Time{}
			st.pending = nil
		}
		st.provisionalProof = false
		st.provisionalProofID = 0
		st.provisionalCandidate = models.ProvisionalDropCandidate{}
		st.provisionalOwner = nil
		st.provisionalCaptureRun = 0
		st.provisionalCaptureAt = time.Time{}
		st.provisionalLastRun = 0
		st.provisionalLastAt = time.Time{}
		st.provisionalSyncAsked = false
		st.recoveryDeferred = false
		st.terminalDeferred = false
		st.terminalDeferredRun = 0
		st.terminalDeferredAt = time.Time{}
		st.lastObservedSyncAt = sync.ProgressLastSyncAt
		st.resetEvidence()
		st.CampaignName = campaign.Name
		st.DropName = drop.Name
		st.Detail = "server-proven alternate authority ended; ordinary watchdog recovery remains bounded and awaits a confirmed farmer"
	} else if !ok || st.provisional {
		// Once the broker no longer publishes the matching provisional lease,
		// this tuple is ordinary confirmed-assignment monitoring again. An
		// unproved lease never carries its narrow recovery/quarantine semantics
		// across that authority transition merely because the map key is shared.
		st = &dropState{DropProgress: DropProgress{
			CampaignID:     campaign.ID,
			CampaignName:   campaign.Name,
			DropID:         drop.ID,
			DropName:       drop.Name,
			LastMinutes:    drop.CurrentMinutesWatched,
			LastProgressAt: now,
			Status:         ProgressHealthy,
			Detail:         "tracking started",
		},
			// Seed the observation cursor so an inventory read that completed
			// BEFORE tracking began can never count as the first no-progress
			// observation.
			lastObservedSyncAt: sync.ProgressLastSyncAt,
		}
		w.states[key] = st
	}
	return st, key, drop
}

// observeProgress folds the latest campaign snapshot, inventory observation,
// and delivery accounting into the drop's state — including the healthy reset
// (and recovered notification) when minutes advanced.
func (w *ProgressWatchdog) observeProgress(st *dropState, campaign *models.Campaign, drop *models.Drop, sync drops.SyncStatus, now time.Time) {
	channel := w.farmingChannel(campaign, drop)
	if channel == "" && st.Channel != "" && w.watch.IsWatching(st.Channel) {
		// The previous farming channel still holds a slot but the campaign
		// assignment vanished from it — keep the channel so gatesHold can name
		// the eligibility loss precisely instead of a generic "no channel".
		channel = st.Channel
	}
	w.observeReports(st, channel)

	if drop.CurrentMinutesWatched > st.LastMinutes {
		recovered := st.notifiedStalled
		if recovered && w.notifier != nil {
			w.notifier.NotifyDropRecovered(st.CampaignName, st.DropName, channel,
				fmt.Sprintf("progress resumed: %d/%d minutes", drop.CurrentMinutesWatched, drop.MinutesRequired))
		}
		if st.Status == ProgressStalled || st.RecoveryStage > 0 {
			events.Record(events.TypeDropRecovered, channel, st.CampaignName+" / "+st.DropName)
		}
		if st.avoidedChannel != "" && w.avoid != nil {
			// The drop moves again; the excluded channel gets a clean slate.
			w.avoid.Clear(st.avoidedChannel)
		}
		*st = dropState{DropProgress: DropProgress{
			CampaignID:     st.CampaignID,
			CampaignName:   st.CampaignName,
			DropID:         st.DropID,
			DropName:       st.DropName,
			Channel:        channel,
			LastMinutes:    drop.CurrentMinutesWatched,
			LastProgressAt: now,
			Status:         ProgressHealthy,
			Detail:         fmt.Sprintf("progress advancing: %d/%d minutes", drop.CurrentMinutesWatched, drop.MinutesRequired),
		},
			statsChannel: channel,
			// Seed the observation cursor so the very sync whose data showed
			// this progress can never be re-counted as a no-progress
			// observation of the fresh episode. The evidence window itself
			// restarts in evaluate once the gates hold.
			lastObservedSyncAt: sync.ProgressLastSyncAt,
		}
		if stats, ok := w.watch.ReportStats(channel); ok {
			st.baselineReports, st.baselineValid = stats.Successes, true
		} else {
			st.baselineValid = false
		}
		return
	}

	// A regressed/out-of-order inventory snapshot must never lower the monotone
	// progress watermark. Otherwise 100 -> 90 -> 100 would look like a false
	// recovery even though the server never advanced beyond the true maximum.
	// No-progress observations are counted in evaluate, and only inside an
	// active evidence window (every gate holding) — see dropState.evidenceSince.
}

// observeReports folds broker delivery accounting without treating it as Drop
// progress. ACKs are only a conjunctive stall gate; they never arm or prove a
// provisional lease.
func (w *ProgressWatchdog) observeReports(st *dropState, channel string) {
	if channel != st.statsChannel {
		// The farming channel changed (rotation, displacement, or our own
		// switch stage): re-baseline the delivery accounting against it.
		st.statsChannel = channel
		st.ReportsSinceProgress = 0
		if stats, ok := w.watch.ReportStats(channel); ok {
			st.baselineReports, st.baselineValid = stats.Successes, true
		} else {
			// The watcher hasn't published stats for this channel yet. Leave the
			// baseline invalid so the first successful read below adopts it,
			// rather than fixing 0 as the base and later counting the channel's
			// lifetime successes as progress-since-baseline.
			st.baselineReports, st.baselineValid = 0, false
		}
	}
	st.Channel = channel

	if channel == "" {
		return
	}
	if stats, ok := w.watch.ReportStats(channel); ok {
		if !st.baselineValid {
			// First successful read after a channel change whose initial read
			// missed: adopt it rather than counting lifetime successes.
			st.baselineReports, st.baselineValid = stats.Successes, true
		}
		if n := stats.Successes - st.baselineReports; n >= 0 {
			st.ReportsSinceProgress = n
		}
	}
}

// gatesHold checks every stall-confirmation gate that is not a threshold:
// all must hold simultaneously or the stall is unconfirmable by design (the
// conservative, false-positive-averse core of the watchdog). Returns the
// human-readable reason of the first failing gate for explainability.
func (w *ProgressWatchdog) gatesHold(st *dropState, campaign *models.Campaign, drop *models.Drop, sync drops.SyncStatus, outage bool, outageSignal string, cfg WatchdogConfig, now time.Time) (bool, string) {
	if outage {
		return false, fmt.Sprintf("not counting a stall: Twitch connectivity is degraded (%s failing)", outageSignal)
	}
	// Inventory observability: a stall can only be confirmed while we can
	// actually SEE the drop's progress. A currently-failing progress sync, or
	// none completing within the stall-delay window, means "cannot check" —
	// Twitch may have credited the drop invisibly, so confirmation (and the
	// evidence clock) must wait for observability to return.
	if sync.ProgressLastError != "" {
		return false, "not counting a stall: inventory reads are currently failing, drop progress is unobservable"
	}
	if !sync.ProgressLastSyncAt.IsZero() && now.Sub(sync.ProgressLastSyncAt) > cfg.StallDelay {
		return false, "not counting a stall: no inventory observation completed within the stall-delay window"
	}
	if st.Channel == "" {
		return false, "no slotted channel is farming this campaign right now (rotation, offline, or waiting)"
	}
	if !w.watch.IsWatching(st.Channel) {
		return false, fmt.Sprintf("%s does not hold a watch slot right now", st.Channel)
	}

	streamer := w.resolveStreamer(st.Channel)
	if streamer == nil {
		return false, "farming channel is not resolvable to a live streamer object"
	}
	if campaign.Game != nil && streamer.Stream.GameID() != campaign.Game.ID {
		return false, fmt.Sprintf("%s switched away from %s — the campaign cannot progress there", st.Channel, campaign.Game.Name)
	}
	if drop.HasPreconditionsMet != nil && !*drop.HasPreconditionsMet {
		return false, "drop preconditions not met on Twitch's side (previous drop or account link pending)"
	}
	// Channel-side eligibility: the tracker's intersection (game + advertised
	// campaign + allow-list) must still assign this campaign to the channel.
	eligible := false
	for _, c := range streamer.Stream.GetCampaigns() {
		if assignedCampaignMatchesDrop(c, campaign.ID, drop.ID, campaign.Game) {
			eligible = true
			break
		}
	}
	if !eligible {
		return false, fmt.Sprintf("campaign is no longer assigned to %s (eligibility/intersection changed)", st.Channel)
	}
	return true, ""
}

// stageSessionRefresh stages a correlated session refresh into the broker for an
// async recovery stage and PARKS the pipeline on it (records the pending
// correlation). It captures the live broadcast/session identity so the broker can
// reject the request if the session moved, and mints a unique RequestID +
// privacy-safe signature so only the exact matching outcome can complete this
// stage. Returns the redacted stage detail. Runs on the watchdog goroutine.
func (w *ProgressWatchdog) stageSessionRefresh(st *dropState, mode watcher.RefreshMode, stageIndex int, stageName string, now time.Time, detail string) string {
	var (
		broadcastID string
		generation  uint64
	)
	streamer, authorityCurrent := w.recoveryStreamer(st)
	if !authorityCurrent {
		st.recoveryDeferred = true
		return detail + " — deferred because recovery authority changed"
	}
	if streamer != nil {
		broadcastID = streamer.Stream.GetBroadcastID()
		generation = streamer.Stream.SessionGeneration()
	}
	w.reqSeq++
	requestID := fmt.Sprintf("%s\x00%s\x00%d", st.CampaignID, st.DropID, w.reqSeq)
	signature := watcher.RecoverySignature{
		Login:             st.Channel,
		BroadcastID:       broadcastID,
		SessionGeneration: generation,
		Stage:             watcher.ProbeStage(stageName),
		Mode:              mode,
	}.String()

	w.watch.RequestSessionRefresh(watcher.SessionRefreshRequest{
		RequestID:           requestID,
		Login:               st.Channel,
		Mode:                mode,
		ExpectedBroadcastID: broadcastID,
		ExpectedGeneration:  generation,
		Signature:           signature,
		Requested:           now,
	})
	st.pending = &pendingRecovery{
		requestID:   requestID,
		signature:   signature,
		broadcastID: broadcastID,
		generation:  generation,
		stageIndex:  stageIndex,
		stageName:   stageName,
		requestedAt: now,
		deadline:    now.Add(recoveryOutcomeDeadline),
	}
	return detail + " — awaiting the broker outcome before advancing"
}

// resolvePending checks whether the in-flight async recovery stage has a matching
// broker outcome, and drives the pipeline accordingly. It returns true when it
// consumed this pass's single recovery-stage budget (a resolution or a still-valid
// wait counts, so a parked drop never lets another drop's stage run out from under
// it). It is the state machine that makes async recovery outcome-driven.
func (w *ProgressWatchdog) resolvePending(st *dropState, now time.Time) bool {
	p := st.pending
	outcome, ok := w.watch.LastSessionRefresh(st.Channel)
	matched := ok && outcome.RequestID == p.requestID && outcome.Signature == p.signature

	if !matched {
		// No matching outcome yet. Match is by EXACT RequestID + signature, so an
		// old/foreign outcome (wrong RequestID, old signature, old broadcast) is
		// ignored, never mistaken for this episode's completion.
		if now.After(p.deadline) {
			st.pending = nil
			st.LastRecoveryAt = now // cooldown before the next stage
			st.Status = ProgressRecovering
			st.Detail = fmt.Sprintf("%s recovery timed out awaiting the broker outcome; will continue after cooldown", p.stageName)
			return true
		}
		st.Status = ProgressRecovering
		st.Detail = fmt.Sprintf("awaiting the %s recovery outcome from the slot broker", p.stageName)
		return true
	}

	// A matching outcome landed. Clear the pending correlation and act on the kind.
	stageIndex, stageName := p.stageIndex, p.stageName
	st.pending = nil
	switch {
	case outcome.Stale:
		// The session the refresh targeted was superseded (new broadcast/session):
		// this recovery episode is over. Rebaseline evidence and restart the
		// pipeline so the fresh session is judged on its own merits.
		w.rebaselineEpisode(st, now)
		st.Detail = "recovery outcome stale — broadcast/session changed; recovery rebaselined for the new session"
		return true
	case outcome.Skipped:
		// The channel lost its slot: staging a request is NOT a completed transport
		// recovery. Roll the stage back so it re-runs once farming is re-confirmed,
		// and discard the stall evidence so the gates must prove active farming
		// again before the stage retries.
		st.RecoveryStage = stageIndex
		st.resetEvidence()
		st.Status = ProgressRecovering
		st.Detail = fmt.Sprintf("%s recovery skipped (watch slot lost); will retry once farming is re-confirmed", stageName)
		return true
	case outcome.Success:
		// The refresh applied. A successful refresh is NOT proof the drop recovered —
		// only real progress confirms that — so stay Recovering and let the next
		// stage run after cooldown if the stall persists.
		st.LastRecoveryAt = now
		st.Status = ProgressRecovering
		st.Detail = fmt.Sprintf("%s recovery applied by the broker; awaiting confirmed drop progress", stageName)
		return true
	default:
		// Matching failure: the stage is done (failed). The next stage may run after
		// the cooldown.
		st.LastRecoveryAt = now
		st.Status = ProgressRecovering
		st.Detail = fmt.Sprintf("%s recovery failed at the broker (%s); next stage after cooldown", stageName, outcome.Reason)
		return true
	}
}

// rebaselineEpisode restarts the recovery pipeline for a fresh session (a stale
// outcome from a broadcast change): evidence, stage, and exhaustion clear, but the
// notified-stalled flag is preserved so a session churn cannot re-fire the
// critical alert. The recovered notification still fires later if real progress
// resumes.
func (w *ProgressWatchdog) rebaselineEpisode(st *dropState, now time.Time) {
	st.pending = nil
	st.RecoveryStage = 0
	st.RecoveryStageName = ""
	st.exhaustedAt = time.Time{}
	st.LastRecoveryAt = time.Time{}
	st.resetEvidence()
	st.Status = ProgressRecovering
}

// advanceRecovery drives the recovery pipeline one step per pass, returning true
// when it consumed this pass's recovery-stage budget. It is finite; once
// exhausted the drop stays STALLED until progress resumes or Rearm elapses. An
// async stage parks the pipeline on a pending broker outcome (resolvePending)
// instead of advancing the instant its request is queued.
func (w *ProgressWatchdog) advanceRecovery(st *dropState, cfg WatchdogConfig, now time.Time) bool {
	// An in-flight async stage takes priority: resolve (or keep waiting on) its
	// outcome before any new stage can run. A queued request is not a completed
	// stage — the stage number does not advance again until the outcome matches.
	if st.pending != nil {
		return w.resolvePending(st, now)
	}

	if !st.exhaustedAt.IsZero() {
		if cfg.Rearm <= 0 || now.Sub(st.exhaustedAt) < cfg.Rearm {
			st.Status = ProgressStalled
			return false
		}
		// Re-arm: a fresh pipeline pass for a long-stalled drop.
		st.exhaustedAt = time.Time{}
		st.RecoveryStage = 0
	}
	if !st.LastRecoveryAt.IsZero() && now.Sub(st.LastRecoveryAt) < cfg.RecoveryCooldown {
		return false
	}
	if st.RecoveryStage >= len(recoveryStages) {
		st.Status = ProgressStalled
		return false
	}

	stage := recoveryStages[st.RecoveryStage]
	st.RecoveryStage++
	st.RecoveryStageName = stage.name
	st.LastRecoveryAt = now
	st.recoveryDeferred = false
	st.Detail = stage.run(w, st, now)
	if st.recoveryDeferred {
		// A denied observation permit means the stage did not run. Roll it
		// back and retry on a later evaluation; denial is neither transport
		// health evidence nor a completed recovery action.
		st.RecoveryStage--
		st.RecoveryStageName = ""
		st.LastRecoveryAt = time.Time{}
		st.Status = ProgressRecovering
		return true
	}
	if st.RecoveryStage >= len(recoveryStages) {
		st.Status = ProgressStalled
	} else {
		st.Status = ProgressRecovering
	}
	events.Record(events.TypeDropRecoveryStep, st.Channel,
		fmt.Sprintf("%s / %s: %s (stage %d/%d)", st.CampaignName, st.DropName, stage.label, st.RecoveryStage, len(recoveryStages)))
	return true
}

// publishFromStates rebuilds and publishes the immutable snapshot.
func (w *ProgressWatchdog) publishFromStates(now time.Time) {
	w.mu.Lock()
	list := make([]DropProgress, 0, len(w.states))
	for _, st := range w.states {
		list = append(list, st.DropProgress)
	}
	w.mu.Unlock()

	// Stable order: worst status first, then campaign name.
	rank := func(s string) int {
		switch s {
		case ProgressStalled:
			return 0
		case ProgressRecovering:
			return 1
		default:
			return 2
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if ri, rj := rank(list[i].Status), rank(list[j].Status); ri != rj {
			return ri < rj
		}
		return list[i].CampaignName < list[j].CampaignName
	})

	w.publish(ProgressSnapshot{Enabled: true, EvaluatedAt: now, Drops: list})
}

func (w *ProgressWatchdog) publish(s ProgressSnapshot) {
	w.snap.Store(&s)
}

// ProgressSignal composes the drops_progress health signal from the published
// snapshot: STALLED if any drop is stalled, OK with a recovering stage note if
// any is recovering, OK when all healthy, IDLE when nothing is tracked. The
// miner's health tick records it, keeping the Center single-writer.
func (w *ProgressWatchdog) ProgressSignal(now time.Time) Signal {
	snap := w.Snapshot()
	sig := Signal{Name: SignalDropsProgress, Status: StatusOK, CheckedAt: now}

	var stalled, recovering *DropProgress
	for i := range snap.Drops {
		switch snap.Drops[i].Status {
		case ProgressStalled:
			if stalled == nil {
				stalled = &snap.Drops[i]
			}
		case ProgressRecovering:
			if recovering == nil {
				recovering = &snap.Drops[i]
			}
		}
	}

	switch {
	case stalled != nil:
		sig.Status = StatusStalled
		sig.Detail = fmt.Sprintf("%q progress stalled on %s despite recovery", stalled.DropName, stalled.Channel)
		sig.ErrorCode = "drop_progress_stalled"
		sig.Stage = stalled.RecoveryStageName
	case recovering != nil:
		sig.Detail = fmt.Sprintf("%q stalled — automatic recovery running (%s)", recovering.DropName, recovering.RecoveryStageName)
		sig.Stage = "recovering:" + recovering.RecoveryStageName
	case len(snap.Drops) > 0:
		sig.Detail = fmt.Sprintf("%d drop(s) tracked, progress advancing normally", len(snap.Drops))
	default:
		sig.Status = StatusIdle
		sig.Detail = "no active drop campaign"
	}
	return sig
}
