package watcher

import (
	"sort"
	"strings"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// ProvisionalLeaseState is the broker-owned lifecycle of one exact-Drop
// observation. Pending reserves exclusive attribution but may send one
// bootstrap beacon only after each fresh complete Pending observation; it has
// no exact server baseline. Observing has a fresh
// post-reservation exact-row baseline; Proven means a later clean Inventory run
// increased the exact Drop's server minutes.
type ProvisionalLeaseState string

const (
	ProvisionalLeasePending   ProvisionalLeaseState = "pending"
	ProvisionalLeaseObserving ProvisionalLeaseState = "observing"
	ProvisionalLeaseProven    ProvisionalLeaseState = "proven"
)

// ProvisionalPendingObservationKind distinguishes clean exhaustive absence from
// a present exact tuple whose self/minutes payload was null. Both may open one
// bootstrap send; only Absence is usable as no-progress evidence by health.
type ProvisionalPendingObservationKind string

const (
	ProvisionalPendingObservationNone         ProvisionalPendingObservationKind = ""
	ProvisionalPendingObservationAbsence      ProvisionalPendingObservationKind = "absence"
	ProvisionalPendingObservationTupleUnknown ProvisionalPendingObservationKind = "tuple_unknown"
)

// ProvisionalLease is an immutable, caller-owned observation snapshot. Run
// numbers are supplied by the progress owner and must increase strictly;
// delivery ACKs never touch these fields and therefore can never prove a Drop.
type ProvisionalLease struct {
	LeaseID    uint64
	Candidate  models.ProvisionalDropCandidate
	State      ProvisionalLeaseState
	ReservedAt time.Time
	// PendingObservation describes the latest accepted Pending cursor. It is
	// empty after an exact Found row arms the lease.
	PendingObservation ProvisionalPendingObservationKind

	BaselineRun     uint64
	BaselineAt      time.Time
	BaselineMinutes int
	MaxRun          uint64
	MaxAt           time.Time
	MaxMinutes      int
}

// ObservationPermit is an opaque, single-use ownership token. Callers must
// release every granted permit after their beacon/probe finishes. Its fields
// intentionally remain private so permits cannot be forged or retargeted.
type ObservationPermit struct {
	token   uint64
	leaseID uint64
	proofID uint64
}

type observationPermitRecord struct {
	streamer       *models.Streamer
	leaseID        uint64
	proofID        uint64
	leaseIDAtGrant uint64
	conflict       observationConflictSnapshot
}

type observationConflictSnapshot struct {
	gameID            string
	channelID         string
	broadcastID       string
	sessionGeneration uint64
	availability      models.CampaignAvailabilityState
	availabilityObs   uint64
	knownGeneration   uint64
	campaignIDs       []string
}

// provisionalProofRecord is direct server authority for one exact
// channel/drop/broadcast/session tuple. It is process-local and never exposed
// through BrokerSnapshot or persisted. The source must continue proposing a
// currently valid tuple every broker tick or the proof is discarded.
type provisionalProofRecord struct {
	proof ProvisionalProof
	owner *models.Streamer
}

type provenProvisionalCandidate struct {
	candidate models.ProvisionalDropCandidate
	owner     *models.Streamer
	proofID   uint64
}

// provisionalQuarantineState is broker-owned negative authority for the
// current monitoring lifecycle. It is structurally bounded by the latest
// complete, source-fenced discovery scope: one bucket per exact Streamer
// object, one broadcast/session per object, and only candidate identities that
// still exist in that authoritative scope. No wall-clock expiry or arbitrary
// eviction participates in correctness.
type provisionalQuarantineState struct {
	namespace        uint64
	sourceGeneration uint64
	owners           map[*models.Streamer]provisionalQuarantineOwner
	// accepted is the last broker-validated structural admission namespace.
	// Once enforceAccepted is set by any fenced reconciliation attempt, a
	// provisional envelope must be an exact member before it may acquire a lease
	// or become a terminal negative. This closes the build/reconcile/publication
	// race without retaining an append-only history.
	accepted        map[*models.Streamer]provisionalQuarantineOwner
	enforceAccepted bool
}

type provisionalQuarantineOwner struct {
	// candidates is keyed by the stable account-work slot (campaign/drop/game).
	// Its value is the latest terminal causal identity for that slot. A later
	// playback session is immediately reconsiderable and, only if it independently
	// reaches a terminal negative, replaces the prior value instead of appending.
	candidates map[string]models.ProvisionalDropCandidate
}

// ProvisionalQuarantineOwnerScope is one exact private owner in discovery's
// complete broker-facing account-campaign snapshot. Candidates must include
// every currently eligible tuple, including tuples already quarantined and
// therefore omitted from ranking/selection. An empty Candidates slice is an
// authoritative statement that this owner currently has no eligible tuple.
//
// The broker validates the owner/session and every tuple again under
// observationMu before it prunes negative state. Source generation zero is
// deliberately unusable, so unfenced compatibility providers can never delete
// a negative.
type ProvisionalQuarantineOwnerScope struct {
	Streamer          *models.Streamer
	BroadcastID       string
	SessionGeneration uint64
	Candidates        []models.ProvisionalDropCandidate
}

// ProvisionalDirectoryAuthority identifies only the active game IDs whose
// current Directory enumeration is uncertain. An empty value means every
// active game's current listing completed. AllUncertain is used between a
// settings change and its first replacement Directory publication.
//
// UncertainGameIDs must be canonical, sorted, and unique. This makes the
// authority snapshot deterministic and lets the broker reject malformed or
// mutable caller state before pruning.
type ProvisionalDirectoryAuthority struct {
	AllUncertain     bool
	UncertainGameIDs []string
}

// ProvisionalAccountWork is one exact open-campaign work slot in the complete,
// source-fenced account snapshot. It is independent of Directory owners: while
// a game's listing is uncertain, removal or Drop advancement in this authority
// can still prune an old Directory negative and accepted entry.
type ProvisionalAccountWork struct {
	CampaignID string
	DropID     string
	GameID     string
}

// ProvisionalProof is process-local direct server authority for one candidate.
// It is available only to the progress watchdog through ProvisionalProofs; it
// is not part of BrokerSnapshot and carries no JSON surface.
type ProvisionalProof struct {
	ProofID   uint64
	Candidate models.ProvisionalDropCandidate

	BaselineRun     uint64
	BaselineAt      time.Time
	BaselineMinutes int
	ProvenRun       uint64
	ProvenAt        time.Time
	ProvenMinutes   int
}

// SetProvisionalMonitoringEnabled enables or disables the bounded observation
// path. Disabled is the safe zero value: no provisional proposal may consume a
// slot without a health owner capable of taking a baseline and judging it.
func (w *MinuteWatcher) SetProvisionalMonitoringEnabled(enabled bool) {
	w.observationMu.Lock()
	defer w.observationMu.Unlock()
	if enabled == w.provisionalMonitoring {
		return
	}
	w.provisionalMonitoring = enabled
	if enabled {
		// Reusing a MinuteWatcher after disable/Stop starts a new authority
		// namespace. A negative from an earlier health owner must never cross that
		// lifecycle boundary even when every Twitch scalar happens to match.
		w.provisionalLeaseSeq++
		if w.provisionalLeaseSeq == 0 {
			w.provisionalLeaseSeq++
		}
		w.provisionalQuarantine = provisionalQuarantineState{
			namespace:       w.provisionalLeaseSeq,
			enforceAccepted: w.provisionalQuarantine.enforceAccepted,
		}
		return
	}
	w.clearProvisionalLeaseLocked()
	w.provisionalProofs = nil
	w.provisionalQuarantine = provisionalQuarantineState{
		namespace:       w.provisionalQuarantine.namespace,
		enforceAccepted: w.provisionalQuarantine.enforceAccepted,
	}
}

// ProvisionalLease returns the currently reserved exact-Drop lease. The
// published pointer is replaced wholesale on every transition; its ACL slice
// is cloned again for the caller so no reader can mutate broker state.
func (w *MinuteWatcher) ProvisionalLease() (ProvisionalLease, bool) {
	lease := w.provisionalLeasePublished.Load()
	if lease == nil {
		return ProvisionalLease{}, false
	}
	return cloneProvisionalLease(*lease), true
}

// ArmProvisionalLease records the first fresh, clean exact-Found Drop Inventory
// observation after reservation. The caller must attest that the exact tuple was
// present; an exhaustive-array absence belongs in ObserveProvisionalAbsence. It
// never infers a baseline from an ACK or elapsed wall time.
func (w *MinuteWatcher) ArmProvisionalLease(leaseID, run uint64, at time.Time, minutes int) bool {
	w.observationMu.Lock()
	defer w.observationMu.Unlock()
	lease := w.provisionalLease
	if lease == nil || lease.LeaseID != leaseID || lease.State != ProvisionalLeasePending ||
		run == 0 || run <= lease.MaxRun || at.IsZero() || !at.After(lease.MaxAt) ||
		!at.After(lease.ReservedAt) || minutes < 0 ||
		!provisionalCandidateCurrent(w.provisionalLeaseStreamer, lease.Candidate, w.rewardSkips.Load()) {
		return false
	}
	lease.State = ProvisionalLeaseObserving
	lease.BaselineRun = run
	lease.BaselineAt = at
	lease.BaselineMinutes = minutes
	lease.MaxRun = run
	lease.MaxAt = at
	lease.MaxMinutes = minutes
	lease.PendingObservation = ProvisionalPendingObservationNone
	w.provisionalBootstrapReady = false
	// A fresh exact baseline supersedes any terminal decision that was formed
	// while the tuple was absent/unknown. Observing must be able to send again so
	// only a later exact delta (or a later terminal cycle) decides the lease.
	w.clearQuarantineFenceLocked()
	w.publishProvisionalLeaseLocked()
	return true
}

// ObserveProvisionalAbsence records a successful exhaustive Inventory run in
// which the exact candidate tuple was absent. Absence is not a zero-minute row:
// the lease remains Pending and Baseline fields stay unset. The freshness cursor
// prevents that same run from later being reused to arm the lease; only a
// strictly newer exact-Found observation may do so.
func (w *MinuteWatcher) ObserveProvisionalAbsence(leaseID, run uint64, at time.Time) bool {
	return w.observeProvisionalPending(leaseID, run, at, ProvisionalPendingObservationAbsence)
}

// ObserveProvisionalTupleUnknown records a successful exhaustive Inventory run
// where the exact tuple existed but its self/minutes value was null. This is
// UNKNOWN rather than no-progress: it advances freshness and opens one bootstrap
// send, but its marker tells health not to count it as an absence confirmation.
func (w *MinuteWatcher) ObserveProvisionalTupleUnknown(leaseID, run uint64, at time.Time) bool {
	return w.observeProvisionalPending(leaseID, run, at, ProvisionalPendingObservationTupleUnknown)
}

func (w *MinuteWatcher) observeProvisionalPending(
	leaseID, run uint64,
	at time.Time,
	kind ProvisionalPendingObservationKind,
) bool {
	w.observationMu.Lock()
	defer w.observationMu.Unlock()
	lease := w.provisionalLease
	if lease == nil || lease.LeaseID != leaseID || lease.State != ProvisionalLeasePending ||
		run == 0 || run <= lease.MaxRun || at.IsZero() || !at.After(lease.MaxAt) ||
		!at.After(lease.ReservedAt) ||
		!provisionalCandidateCurrent(w.provisionalLeaseStreamer, lease.Candidate, w.rewardSkips.Load()) {
		return false
	}
	lease.MaxRun = run
	lease.MaxAt = at
	lease.PendingObservation = kind
	// Fresh complete authority replaces (rather than accumulates) the token. A
	// repeated Run/At is rejected above and therefore cannot mint another send.
	w.provisionalBootstrapReady = true
	w.publishProvisionalLeaseLocked()
	return true
}

// ObserveProvisionalProgress accepts only a strictly newer, monotone clean
// server observation. Equal minutes are useful no-progress evidence but do not
// prove anything; only an increase over the post-reservation baseline moves the
// lease to Proven.
func (w *MinuteWatcher) ObserveProvisionalProgress(leaseID, run uint64, at time.Time, minutes int) bool {
	w.observationMu.Lock()
	defer w.observationMu.Unlock()
	lease := w.provisionalLease
	if lease == nil || lease.LeaseID != leaseID || lease.State == ProvisionalLeasePending ||
		run <= lease.MaxRun || at.IsZero() || !at.After(lease.MaxAt) || minutes < lease.MaxMinutes ||
		!provisionalCandidateCurrent(w.provisionalLeaseStreamer, lease.Candidate, w.rewardSkips.Load()) {
		return false
	}
	// An ordinary beacon admitted as causally nonconflicting is not settled
	// until its transport returns and ReleaseObservationPermit verifies that the
	// stream facts did not drift between permit grant and Sender capture. Never
	// accept a server delta inside that ambiguity window.
	for _, permit := range w.observationPermits {
		if permit.leaseIDAtGrant == leaseID {
			return false
		}
	}
	if minutes > lease.BaselineMinutes && !w.canStoreProvisionalProofLocked(lease.Candidate, w.provisionalLeaseStreamer) {
		// Admission normally makes this unreachable. Preserve the hard proof-cache
		// bound defensively without evicting an already proved authority.
		w.clearProvisionalLeaseLocked()
		return false
	}
	lease.MaxRun = run
	lease.MaxAt = at
	lease.MaxMinutes = minutes
	if minutes > lease.BaselineMinutes {
		lease.State = ProvisionalLeaseProven
		if w.provisionalProofs == nil {
			w.provisionalProofs = make(map[string]provisionalProofRecord)
		}
		// Attribution for one exact account Drop is singular, but independent
		// proved Drops may coexist. Replacing only a same campaign/drop proof
		// avoids both ambiguous health ownership and arbitrary cross-game starvation.
		for key, existing := range w.provisionalProofs {
			if existing.proof.Candidate.CampaignID == lease.Candidate.CampaignID &&
				existing.proof.Candidate.DropID == lease.Candidate.DropID {
				delete(w.provisionalProofs, key)
			}
		}
		w.provisionalProofs[lease.Candidate.QuarantineKey()] = provisionalProofRecord{
			proof: ProvisionalProof{
				ProofID:         lease.LeaseID,
				Candidate:       cloneProvisionalCandidateValue(lease.Candidate),
				BaselineRun:     lease.BaselineRun,
				BaselineAt:      lease.BaselineAt,
				BaselineMinutes: lease.BaselineMinutes,
				ProvenRun:       run,
				ProvenAt:        at,
				ProvenMinutes:   minutes,
			},
			owner: w.provisionalLeaseStreamer,
		}
	}
	w.publishProvisionalLeaseLocked()
	return true
}

// ReleaseProvisionalLease releases the matching lease without creating a
// negative fact. A continuing proposal may be reconsidered on a later tick.
func (w *MinuteWatcher) ReleaseProvisionalLease(leaseID uint64) bool {
	w.observationMu.Lock()
	defer w.observationMu.Unlock()
	if w.provisionalLease == nil || w.provisionalLease.LeaseID != leaseID {
		return false
	}
	w.clearProvisionalLeaseLocked()
	return true
}

// QuarantineProvisionalLease records a narrow process-local negative for the
// exact channel/drop/broadcast/session tuple and releases its lease. A changed
// broadcast or session generation has a different key and may be reconsidered.
func (w *MinuteWatcher) QuarantineProvisionalLease(leaseID uint64, expected models.ProvisionalDropCandidate) bool {
	w.observationMu.Lock()
	defer w.observationMu.Unlock()
	lease := w.provisionalLease
	if lease == nil || lease.LeaseID != leaseID || lease.State == ProvisionalLeaseProven ||
		!lease.Candidate.SameLeaseIdentity(expected) {
		return false
	}
	for _, permit := range w.observationPermits {
		if permit.leaseID == leaseID || permit.leaseIDAtGrant == leaseID {
			w.quarantineFenceLeaseID = leaseID
			if lease.MaxRun > w.quarantineFenceRun {
				w.quarantineFenceRun = lease.MaxRun
			}
			// Release stamps this only after the final matching transport returns.
			w.quarantineFenceDrainAt = time.Time{}
			return false
		}
	}
	// Even when the latest matching transport drained before health reached this
	// method, its delayed server delta may still be absent from the observation
	// health is about to judge. The first terminal request therefore starts a
	// drain fence unconditionally and cannot itself create a negative.
	if w.quarantineFenceLeaseID != leaseID {
		w.quarantineFenceLeaseID = leaseID
		w.quarantineFenceRun = lease.MaxRun
		w.quarantineFenceDrainAt = time.Now()
		return false
	}
	// A beacon/probe that was live at an earlier terminal decision may still
	// produce a delayed server update after transport release. Require one clean,
	// strictly newer exact observation that itself completed after the final
	// matching permit drained before turning that decision into a negative. A
	// run completed while the permit was live cannot become post-drain evidence
	// merely because health consumes it later. Equal minutes then permit
	// quarantine; a positive delta proves the candidate and the Proven guard
	// above keeps it authoritative.
	if w.quarantineFenceLeaseID == leaseID &&
		(w.quarantineFenceDrainAt.IsZero() || lease.MaxRun <= w.quarantineFenceRun ||
			!lease.MaxAt.After(w.quarantineFenceDrainAt)) {
		return false
	}
	if !w.recordProvisionalQuarantineLocked(w.provisionalLeaseStreamer, lease.Candidate) {
		w.clearProvisionalLeaseLocked()
		return false
	}
	delete(w.provisionalProofs, lease.Candidate.QuarantineKey())
	w.clearProvisionalLeaseLocked()
	return true
}

// ProvisionalProofs returns the broker's currently live promoted proofs.
// Candidate ACL memory is cloned for the caller.
func (w *MinuteWatcher) ProvisionalProofs() []ProvisionalProof {
	w.observationMu.Lock()
	defer w.observationMu.Unlock()
	proofs := make([]ProvisionalProof, 0, len(w.provisionalProofs))
	for _, record := range w.provisionalProofs {
		proof := record.proof
		proof.Candidate = cloneProvisionalCandidateValue(proof.Candidate)
		proofs = append(proofs, proof)
	}
	sort.Slice(proofs, func(i, j int) bool {
		a, b := proofs[i].Candidate, proofs[j].Candidate
		if a.CampaignID != b.CampaignID {
			return a.CampaignID < b.CampaignID
		}
		if a.DropID != b.DropID {
			return a.DropID < b.DropID
		}
		if a.Login != b.Login {
			return a.Login < b.Login
		}
		if a.BroadcastID != b.BroadcastID {
			return a.BroadcastID < b.BroadcastID
		}
		return a.SessionGeneration < b.SessionGeneration
	})
	return proofs
}

// HasProvisionalProof reports whether direct server authority exists for the
// causal proof identity. Discovery uses it only to preserve its exact proved
// source choice. The query is read-only: only the broker's final gathered
// proposal set may adopt a refreshed envelope. Admission and every send still
// revalidate mutable stream facts.
func (w *MinuteWatcher) HasProvisionalProof(streamer *models.Streamer, candidate models.ProvisionalDropCandidate) bool {
	w.observationMu.Lock()
	defer w.observationMu.Unlock()
	if !w.provisionalMonitoring {
		return false
	}
	record, ok := w.provisionalProofs[candidate.QuarantineKey()]
	if !ok || record.owner != streamer || !record.proof.Candidate.SameProofIdentity(candidate) ||
		!provisionalCandidateCurrent(streamer, candidate, w.rewardSkips.Load()) {
		return false
	}
	return true
}

// IsProvisionalQuarantined reports only the broker's narrow process-local
// negative for this exact private owner/channel/drop/broadcast/session tuple.
// Both the owner pointer and its scalars must match; a same-login clone or
// retargeted tuple cannot inherit authority. Routine UNKNOWN/Directory
// observations keep the key blocked, while a new broadcast or playback-session
// generation naturally produces a different key and is reconsidered.
func (w *MinuteWatcher) IsProvisionalQuarantined(streamer *models.Streamer, candidate models.ProvisionalDropCandidate) bool {
	if streamer == nil || !candidate.Valid() || streamer.GetUsername() != candidate.Login ||
		streamer.ChannelID != candidate.ChannelID {
		return false
	}
	w.observationMu.Lock()
	defer w.observationMu.Unlock()
	return w.isProvisionalQuarantinedLocked(streamer, candidate)
}

// RequireProvisionalScope activates fail-closed exact-scope admission before a
// caller begins building its fenced namespace. A build that returns early or
// loses a source fence can then preserve existing authority without allowing a
// later envelope from that tick to use the direct-test compatibility path.
func (w *MinuteWatcher) RequireProvisionalScope() {
	w.observationMu.Lock()
	w.provisionalQuarantine.enforceAccepted = true
	w.observationMu.Unlock()
}

// ReconcileProvisionalQuarantine consumes discovery's source-fenced eligible
// tuple scope. The account/roster half is always complete. Directory authority
// identifies only active games whose current listing failed; a Directory
// negative is retained on absence only for one of those games. RestrictedACL
// negatives are always account/roster-prunable.
//
// Generation zero is the explicit unfenced value. An older generation,
// malformed/partial account scope, or any owner/session drift is a no-op: stale
// authority may preserve a negative but can never delete one.
//
// Only existing negatives are pruned. Merely selecting B does not create a
// negative for B or delete quarantined A while A remains in the full scope.
func (w *MinuteWatcher) ReconcileProvisionalQuarantine(
	sourceGeneration uint64,
	directoryAuthority ProvisionalDirectoryAuthority,
	accountWork []ProvisionalAccountWork,
	scopes []ProvisionalQuarantineOwnerScope,
) bool {
	if sourceGeneration == 0 {
		return false
	}
	// Any fenced production attempt activates fail-closed exact-scope admission,
	// even if validation or the later commit fence rejects this attempt. This
	// prevents a subsequently derived envelope from bypassing a failed scope.
	w.RequireProvisionalScope()

	if directoryAuthority.AllUncertain && len(directoryAuthority.UncertainGameIDs) != 0 {
		return false
	}
	uncertainGames := make(map[string]struct{}, len(directoryAuthority.UncertainGameIDs))
	for i, gameID := range directoryAuthority.UncertainGameIDs {
		if gameID == "" || strings.TrimSpace(gameID) != gameID ||
			i > 0 && directoryAuthority.UncertainGameIDs[i-1] >= gameID {
			return false
		}
		uncertainGames[gameID] = struct{}{}
	}
	directoryUncertain := func(gameID string) bool {
		if directoryAuthority.AllUncertain {
			return true
		}
		_, uncertain := uncertainGames[gameID]
		return uncertain
	}
	accountWorkSet := make(map[string]struct{}, len(accountWork))
	for i, work := range accountWork {
		if work.CampaignID == "" || strings.TrimSpace(work.CampaignID) != work.CampaignID ||
			work.DropID == "" || strings.TrimSpace(work.DropID) != work.DropID ||
			work.GameID == "" || strings.TrimSpace(work.GameID) != work.GameID {
			return false
		}
		key := provisionalAccountWorkKey(work.CampaignID, work.DropID, work.GameID)
		if i > 0 {
			previous := accountWork[i-1]
			if provisionalAccountWorkKey(previous.CampaignID, previous.DropID, previous.GameID) >= key {
				return false
			}
		}
		accountWorkSet[key] = struct{}{}
	}
	accountWorkCurrent := func(candidate models.ProvisionalDropCandidate) bool {
		_, current := accountWorkSet[provisionalQuarantineSlotKey(candidate)]
		return current
	}

	// Clone and structurally validate caller memory before taking the ownership
	// lock. The second, mutable-state validation below is the commit fence.
	type ownerScope struct {
		broadcastID       string
		sessionGeneration uint64
		candidates        map[string]models.ProvisionalDropCandidate
	}
	desired := make(map[*models.Streamer]ownerScope, len(scopes))
	for _, scope := range scopes {
		if scope.Streamer == nil || scope.Streamer.Stream == nil || scope.BroadcastID == "" ||
			scope.SessionGeneration == 0 {
			return false
		}
		if _, duplicate := desired[scope.Streamer]; duplicate {
			return false
		}
		owner := ownerScope{
			broadcastID:       scope.BroadcastID,
			sessionGeneration: scope.SessionGeneration,
			candidates:        make(map[string]models.ProvisionalDropCandidate, len(scope.Candidates)),
		}
		for _, candidate := range scope.Candidates {
			if !candidate.Valid() || candidate.Login != scope.Streamer.GetUsername() ||
				candidate.ChannelID != scope.Streamer.ChannelID || candidate.BroadcastID != scope.BroadcastID ||
				candidate.SessionGeneration != scope.SessionGeneration {
				return false
			}
			key := provisionalQuarantineSlotKey(candidate)
			if previous, duplicate := owner.candidates[key]; duplicate &&
				!previous.SameLeaseIdentity(candidate) {
				return false
			}
			owner.candidates[key] = cloneProvisionalCandidateValue(candidate)
		}
		desired[scope.Streamer] = owner
	}

	w.observationMu.Lock()
	defer w.observationMu.Unlock()
	if !w.provisionalMonitoring || sourceGeneration <= w.provisionalQuarantine.sourceGeneration {
		return false
	}
	for streamer, scope := range desired {
		stream := streamer.Stream.ProvisionalDropSnapshot()
		if stream.BroadcastID != scope.broadcastID || stream.SessionGeneration != scope.sessionGeneration {
			return false
		}
		for _, candidate := range scope.candidates {
			// Deliberately skip-agnostic (nil RewardSkips): this fence validates
			// quarantine-scope bookkeeping, not farming justification. Vetoing a
			// just-skipped tuple here would no-op the whole reconcile and stall
			// unrelated negative maintenance while the (possibly stale) broker
			// scope still lists the campaign; farming itself is already refused
			// at admission, permits, lease continuity, and proof retention.
			if !provisionalProofCandidateCurrent(streamer, candidate, nil) {
				return false
			}
		}
	}

	// Atomically replace the exact admission namespace. Completed Directory
	// games and every RestrictedACL candidate come from this scope. For an
	// errored game, retain only the last accepted Directory candidates; fresh
	// envelopes from a partial successful subset are not admitted until that
	// game's enumeration is authoritative again.
	nextAccepted := make(map[*models.Streamer]provisionalQuarantineOwner)
	addAccepted := func(streamer *models.Streamer, key string, candidate models.ProvisionalDropCandidate) {
		owner := nextAccepted[streamer]
		if owner.candidates == nil {
			owner.candidates = make(map[string]models.ProvisionalDropCandidate)
		}
		owner.candidates[key] = cloneProvisionalCandidateValue(candidate)
		nextAccepted[streamer] = owner
	}
	for streamer, owner := range w.provisionalQuarantine.accepted {
		for key, candidate := range owner.candidates {
			if candidate.Evidence == models.ProvisionalEvidenceDirectory &&
				directoryUncertain(candidate.GameID) && accountWorkCurrent(candidate) {
				addAccepted(streamer, key, candidate)
			}
		}
	}
	for streamer, owner := range desired {
		for key, candidate := range owner.candidates {
			if candidate.Evidence == models.ProvisionalEvidenceDirectory && directoryUncertain(candidate.GameID) {
				continue
			}
			addAccepted(streamer, key, candidate)
		}
	}

	for streamer, negatives := range w.provisionalQuarantine.owners {
		current, ownerPresent := desired[streamer]
		for key, negative := range negatives.candidates {
			if _, candidatePresent := current.candidates[key]; ownerPresent && candidatePresent {
				continue
			}
			// Preserve an absent open-campaign negative only when that exact active
			// game's current listing errored. Completed games and games no longer in
			// the active account/configured set are authoritative for absence.
			if negative.Evidence == models.ProvisionalEvidenceDirectory &&
				directoryUncertain(negative.GameID) && accountWorkCurrent(negative) {
				continue
			}
			delete(negatives.candidates, key)
		}
		if len(negatives.candidates) == 0 {
			delete(w.provisionalQuarantine.owners, streamer)
			continue
		}
		w.provisionalQuarantine.owners[streamer] = negatives
	}
	if len(nextAccepted) == 0 {
		nextAccepted = nil
	}
	w.provisionalQuarantine.accepted = nextAccepted
	w.provisionalQuarantine.sourceGeneration = sourceGeneration
	return true
}

func provisionalQuarantineSlotKey(candidate models.ProvisionalDropCandidate) string {
	return provisionalAccountWorkKey(candidate.CampaignID, candidate.DropID, candidate.GameID)
}

func provisionalAccountWorkKey(campaignID, dropID, gameID string) string {
	return campaignID + "\x00" + dropID + "\x00" + gameID
}

func (w *MinuteWatcher) recordProvisionalQuarantineLocked(
	streamer *models.Streamer,
	candidate models.ProvisionalDropCandidate,
) bool {
	if streamer == nil || !candidate.Valid() ||
		w.provisionalQuarantine.enforceAccepted &&
			!w.provisionalCandidateAcceptedLocked(streamer, candidate) {
		return false
	}
	if w.provisionalQuarantine.namespace == 0 {
		w.provisionalLeaseSeq++
		if w.provisionalLeaseSeq == 0 {
			w.provisionalLeaseSeq++
		}
		w.provisionalQuarantine.namespace = w.provisionalLeaseSeq
	}
	if w.provisionalQuarantine.owners == nil {
		w.provisionalQuarantine.owners = make(map[*models.Streamer]provisionalQuarantineOwner)
	}
	owner := w.provisionalQuarantine.owners[streamer]
	if owner.candidates == nil {
		owner.candidates = make(map[string]models.ProvisionalDropCandidate)
	}
	owner.candidates[provisionalQuarantineSlotKey(candidate)] = cloneProvisionalCandidateValue(candidate)
	w.provisionalQuarantine.owners[streamer] = owner
	return true
}

func (w *MinuteWatcher) provisionalCandidateAcceptedLocked(
	streamer *models.Streamer,
	candidate models.ProvisionalDropCandidate,
) bool {
	owner, ok := w.provisionalQuarantine.accepted[streamer]
	if !ok {
		return false
	}
	accepted, ok := owner.candidates[provisionalQuarantineSlotKey(candidate)]
	return ok && accepted.SameLeaseIdentity(candidate)
}

func (w *MinuteWatcher) isProvisionalQuarantinedLocked(
	streamer *models.Streamer,
	candidate models.ProvisionalDropCandidate,
) bool {
	owner, ok := w.provisionalQuarantine.owners[streamer]
	if !ok {
		return false
	}
	negative, ok := owner.candidates[provisionalQuarantineSlotKey(candidate)]
	return ok && negative.QuarantineKey() == candidate.QuarantineKey()
}

// ProvisionalOwner returns the private Streamer object that owns an exact live
// lease or promoted proof. Login-based resolution is insufficient for ephemeral
// discovery channels because a scalar-identical replacement object must never
// inherit the prior object's observation authority. The lookup is read-only and
// does not adopt a refreshed proof envelope.
func (w *MinuteWatcher) ProvisionalOwner(authorityID uint64, expected models.ProvisionalDropCandidate) (*models.Streamer, bool) {
	if authorityID == 0 {
		return nil, false
	}
	w.observationMu.Lock()
	defer w.observationMu.Unlock()
	if !w.provisionalMonitoring {
		return nil, false
	}
	// One coherent farming-exclusion decision for both authority legs.
	skips := w.rewardSkips.Load()
	if lease := w.provisionalLease; lease != nil && lease.LeaseID == authorityID &&
		lease.Candidate.SameLeaseIdentity(expected) &&
		provisionalCandidateCurrent(w.provisionalLeaseStreamer, lease.Candidate, skips) {
		return w.provisionalLeaseStreamer, true
	}
	proof, ok := w.provisionalProofs[expected.QuarantineKey()]
	if !ok || proof.proof.ProofID != authorityID ||
		!proof.proof.Candidate.SameProofIdentity(expected) ||
		!provisionalProofCandidateCurrent(proof.owner, expected, skips) {
		return nil, false
	}
	return proof.owner, true
}

// OwnsProvisionalObservation reports whether streamer is the private object
// instance behind the current bounded lease. It is a read-only ownership query;
// routine refresh callers coordinate atomically through RunRoutineRefresh rather
// than using this pre-I/O boolean. A standalone promoted proof deliberately
// returns false: routine refresh must resume so Known-empty, broadcast, and
// session vetoes can be observed and prune the proof before another send.
func (w *MinuteWatcher) OwnsProvisionalObservation(streamer *models.Streamer) bool {
	if streamer == nil {
		return false
	}
	w.observationMu.Lock()
	defer w.observationMu.Unlock()
	if !w.provisionalMonitoring {
		return false
	}
	if w.provisionalLease != nil && w.provisionalLeaseStreamer == streamer &&
		provisionalCandidateCurrent(streamer, w.provisionalLease.Candidate, w.rewardSkips.Load()) {
		return true
	}
	return false
}

// OwnsProvisionalCandidate is the tuple-aware continuity query used by source
// ordering. Streamer-only ownership is sufficient to defer a routine refresh,
// but not to break equal Campaign Policy ties between two Drops on that same
// channel. Only the exact private owner and full lease identity return true.
func (w *MinuteWatcher) OwnsProvisionalCandidate(streamer *models.Streamer, candidate models.ProvisionalDropCandidate) bool {
	if streamer == nil {
		return false
	}
	w.observationMu.Lock()
	defer w.observationMu.Unlock()
	return w.provisionalLease != nil && w.provisionalLeaseStreamer == streamer &&
		w.provisionalLease.Candidate.SameLeaseIdentity(candidate) &&
		provisionalCandidateCurrent(streamer, candidate, w.rewardSkips.Load())
}

// AcquireObservationPermit arbitrates every in-process minute beacon/probe
// against the active exact-Drop lease. Ordinary callers pass leaseID zero. A
// recovery caller passes its exact lease id; health recovery probes do not
// spend processWatching's private one-shot bootstrap token. Pending normal
// sends start closed, and each accepted fresh complete absence or exact-tuple
// UNKNOWN observation opens exactly one token. ACKs never arm or prove
// anything. The short lock is released before network I/O.
func (w *MinuteWatcher) AcquireObservationPermit(streamer *models.Streamer, leaseID uint64) (ObservationPermit, bool) {
	if streamer == nil {
		return ObservationPermit{}, false
	}
	w.observationMu.Lock()
	defer w.observationMu.Unlock()

	lease := w.provisionalLease
	skips := w.rewardSkips.Load()
	if leaseID != 0 {
		if lease == nil || lease.LeaseID != leaseID ||
			w.provisionalLeaseStreamer != streamer ||
			!provisionalCandidateCurrent(streamer, lease.Candidate, skips) {
			if lease != nil && lease.LeaseID == leaseID && !provisionalCandidateCurrent(streamer, lease.Candidate, skips) {
				w.clearProvisionalLeaseLocked()
			}
			return ObservationPermit{}, false
		}
		if w.quarantineFenceLeaseID == leaseID {
			return ObservationPermit{}, false
		}
	} else if lease != nil {
		// Use one immutable snapshot for both the grant decision and the later
		// grant-to-release drift check. Taking a second snapshot here would leave a
		// mutation window between deciding "nonconflicting" and recording facts.
		conflict := captureObservationConflict(streamer)
		if provisionalConflictsWithObservation(lease.Candidate, conflict) {
			return ObservationPermit{}, false
		}
		return w.grantObservationPermitLocked(streamer, 0, 0, lease.LeaseID, conflict)
	}

	return w.grantObservationPermitLocked(streamer, leaseID, 0, 0, captureObservationConflict(streamer))
}

// acquireProvisionalBootstrapPermit is processWatching's normal Pending-send
// path. A fresh complete Pending observation opens exactly one token, consumed
// atomically at grant even when the later transport fails or becomes stale.
// Public health recovery permits intentionally bypass this token.
func (w *MinuteWatcher) acquireProvisionalBootstrapPermit(streamer *models.Streamer, leaseID uint64) (ObservationPermit, bool) {
	if streamer == nil {
		return ObservationPermit{}, false
	}
	w.observationMu.Lock()
	defer w.observationMu.Unlock()
	lease := w.provisionalLease
	if lease == nil || lease.LeaseID != leaseID || lease.State != ProvisionalLeasePending ||
		w.provisionalLeaseStreamer != streamer || !provisionalCandidateCurrent(streamer, lease.Candidate, w.rewardSkips.Load()) ||
		!w.provisionalBootstrapReady || w.quarantineFenceLeaseID == leaseID {
		return ObservationPermit{}, false
	}
	// Consume before returning the permit so concurrent process ticks cannot use
	// one fresh Inventory observation twice. There is no refund on failure/stale:
	// only a later, strictly newer complete observation may reopen the path.
	w.provisionalBootstrapReady = false
	return w.grantObservationPermitLocked(streamer, leaseID, 0, 0, captureObservationConflict(streamer))
}

// AcquireProvisionalProofPermit atomically couples a promoted slot's send or
// health probe to its still-live causal proof identity. A concurrent source
// invalidation that wins the lock prevents a new beacon. The lock covers only
// ownership validation and permit grant, never network I/O.
func (w *MinuteWatcher) AcquireProvisionalProofPermit(streamer *models.Streamer, proofID uint64, expected models.ProvisionalDropCandidate) (ObservationPermit, bool) {
	if streamer == nil {
		return ObservationPermit{}, false
	}
	w.observationMu.Lock()
	defer w.observationMu.Unlock()
	if w.routineRefreshActiveLocked(streamer) {
		return ObservationPermit{}, false
	}
	proof, ok := w.provisionalProofs[expected.QuarantineKey()]
	if !w.provisionalMonitoring || !ok || proof.proof.ProofID != proofID || proof.owner != streamer ||
		!proof.proof.Candidate.SameProofIdentity(expected) || !provisionalProofCandidateCurrent(streamer, expected, w.rewardSkips.Load()) {
		return ObservationPermit{}, false
	}
	conflict := captureObservationConflict(streamer)
	leaseIDAtGrant := uint64(0)
	if lease := w.provisionalLease; lease != nil {
		if lease.State == ProvisionalLeaseProven && lease.Candidate.SameProofIdentity(expected) &&
			w.provisionalLeaseStreamer == streamer {
			// Promotion consumes its own proved observation lease atomically; this
			// is not a bypass for any other active lease or conflicting candidate.
			w.clearProvisionalLeaseLocked()
		} else {
			if provisionalConflictsWithObservation(lease.Candidate, conflict) {
				return ObservationPermit{}, false
			}
			leaseIDAtGrant = lease.LeaseID
		}
	}
	return w.grantObservationPermitLocked(streamer, 0, proofID, leaseIDAtGrant, conflict)
}

func (w *MinuteWatcher) grantObservationPermitLocked(
	streamer *models.Streamer,
	leaseID, proofID, leaseIDAtGrant uint64,
	conflict observationConflictSnapshot,
) (ObservationPermit, bool) {
	w.observationPermitSeq++
	if w.observationPermitSeq == 0 {
		w.observationPermitSeq++
	}
	if w.observationPermits == nil {
		w.observationPermits = make(map[uint64]observationPermitRecord)
	}
	permit := ObservationPermit{token: w.observationPermitSeq, leaseID: leaseID, proofID: proofID}
	w.observationPermits[permit.token] = observationPermitRecord{
		streamer:       streamer,
		leaseID:        leaseID,
		proofID:        proofID,
		leaseIDAtGrant: leaseIDAtGrant,
		conflict:       conflict,
	}
	return permit, true
}

// ReleaseObservationPermit releases a previously granted permit. Unknown,
// zero, or already-released values are harmless no-ops.
func (w *MinuteWatcher) ReleaseObservationPermit(permit ObservationPermit) {
	if permit.token == 0 {
		return
	}
	w.observationMu.Lock()
	defer w.observationMu.Unlock()
	w.releaseObservationPermitLocked(permit)
}

// completeObservationPermit releases a normal watcher send permit. The
// delivered result is intentionally irrelevant to Pending bootstrap authority:
// its one-shot token was already consumed at grant, and an ACK cannot arm or
// prove the lease.
func (w *MinuteWatcher) completeObservationPermit(permit ObservationPermit, _ bool) {
	if permit.token == 0 {
		return
	}
	w.observationMu.Lock()
	defer w.observationMu.Unlock()
	w.releaseObservationPermitLocked(permit)
}

func (w *MinuteWatcher) releaseObservationPermitLocked(permit ObservationPermit) {
	record, ok := w.observationPermits[permit.token]
	if !ok || record.leaseID != permit.leaseID || record.proofID != permit.proofID {
		return
	}
	delete(w.observationPermits, permit.token)
	if w.provisionalLease != nil && w.quarantineFenceLeaseID == w.provisionalLease.LeaseID &&
		(record.leaseID == w.provisionalLease.LeaseID || record.leaseIDAtGrant == w.provisionalLease.LeaseID) {
		if w.provisionalLease.MaxRun > w.quarantineFenceRun {
			w.quarantineFenceRun = w.provisionalLease.MaxRun
		}
		w.quarantineFenceDrainAt = time.Now()
	}
	if record.leaseIDAtGrant == 0 || w.provisionalLease == nil ||
		w.provisionalLease.LeaseID != record.leaseIDAtGrant {
		return
	}
	// Release runs only after Send/Probe returns. A changed monotonic observation
	// or session fence means the transport could have captured facts that now
	// conflict with the provisional candidate; discard the lease without
	// quarantine so no later Inventory delta can be misattributed.
	if !sameObservationConflict(record.conflict, captureObservationConflict(record.streamer)) {
		w.clearProvisionalLeaseLocked()
	}
}

// reconcileProvisionalSlots is the loop-owned admission/reconciliation point.
// It returns at most one provisional slot and publishes exactly the lease that
// owns it. Invalid, conflicting, quarantined, or unmonitored proposals are
// removed without changing ordinary slot decisions.
func (w *MinuteWatcher) reconcileProvisionalSlots(
	slots []slotOccupant,
	waiting []WaitingChannel,
	now time.Time,
	orderedProvisional ...[]slotOccupant,
) ([]slotOccupant, []WaitingChannel) {
	w.observationMu.Lock()
	defer w.observationMu.Unlock()

	ordinary := make([]slotOccupant, 0, len(slots))
	ordinarySeen := make(map[string]bool, len(slots))
	provisional := make([]slotOccupant, 0, 1)
	restoreFallback := func(slot slotOccupant) bool {
		fallback := slot.provisionalFallback
		if fallback == nil || len(ordinary) >= constants.MaxSimultaneousStreams ||
			ordinarySeen[fallback.streamer.GetUsername()] {
			return false
		}
		ordinary = append(ordinary, *fallback)
		ordinarySeen[fallback.streamer.GetUsername()] = true
		if fallback.idx >= 0 && w.selectionReasons != nil {
			w.selectionReasons[fallback.idx] = slot.provisionalFallbackSelection
		}
		if slot.provisionalFallbackParityChanged {
			w.displaceParity = slot.provisionalFallbackParity
		}
		waiting = removeWaitingChannel(waiting, fallback.streamer.GetUsername(), fallback.origin)
		return true
	}
	// One coherent farming-exclusion decision per reconcile pass (immutable).
	skips := w.rewardSkips.Load()
	for _, slot := range slots {
		if slot.provisionalDrop == nil {
			ordinary = append(ordinary, slot)
			ordinarySeen[slot.streamer.GetUsername()] = true
			continue
		}
		if slot.provisionalProven {
			candidate := *slot.provisionalDrop
			proof, proved := w.provisionalProofs[candidate.QuarantineKey()]
			if proved && proof.proof.ProofID == slot.provisionalProofID && proof.owner == slot.streamer &&
				proof.proof.Candidate.SameProofIdentity(candidate) &&
				(!w.provisionalQuarantine.enforceAccepted ||
					w.provisionalCandidateAcceptedLocked(slot.streamer, candidate)) &&
				provisionalProofCandidateCurrent(slot.streamer, candidate, skips) {
				ordinary = append(ordinary, slot)
				ordinarySeen[slot.streamer.GetUsername()] = true
				continue
			}
			delete(w.provisionalProofs, candidate.QuarantineKey())
			restoreFallback(slot)
			waiting = appendProvisionalWaiting(waiting, slot, "server proof no longer matched the current source/session tuple")
			continue
		}
		if len(orderedProvisional) == 0 {
			provisional = append(provisional, slot)
		}
	}
	if len(orderedProvisional) > 0 {
		provisional = append(provisional, orderedProvisional[0]...)
	}
	if len(ordinary) > constants.MaxSimultaneousStreams {
		for _, slot := range ordinary[constants.MaxSimultaneousStreams:] {
			waiting = append(waiting, WaitingChannel{
				Channel:    slot.streamer.GetUsername(),
				Origin:     slot.origin,
				ReasonCode: ReasonLowerPriority,
				Reason:     "excluded by the defensive global watch-slot cap",
			})
		}
		ordinary = ordinary[:constants.MaxSimultaneousStreams]
	}

	if !w.provisionalMonitoring || len(provisional) == 0 {
		w.clearProvisionalLeaseLocked()
		for _, slot := range provisional {
			restoreFallback(slot)
			waiting = appendProvisionalWaiting(waiting, slot, "provisional observation monitoring is unavailable")
		}
		return ordinary, waiting
	}
	if len(ordinary) >= constants.MaxSimultaneousStreams {
		w.clearProvisionalLeaseLocked()
		for _, slot := range provisional {
			restoreFallback(slot)
			waiting = appendProvisionalWaiting(waiting, slot, "provisional observation is fill-only and no watch slot is idle")
		}
		return ordinary, waiting
	}

	var selected *slotOccupant
	admissible := func(slot *slotOccupant, ignoreLeaseID uint64) bool {
		candidate := *slot.provisionalDrop
		// Routine refresh and provisional admission share observationMu. Whichever
		// registers first wins; the other retries on a later broker tick, and no
		// network call runs while this lock is held.
		if w.routineRefreshActiveLocked(slot.streamer) {
			return false
		}
		if w.provisionalQuarantine.enforceAccepted &&
			!w.provisionalCandidateAcceptedLocked(slot.streamer, candidate) {
			return false
		}
		if !provisionalCandidateCurrent(slot.streamer, candidate, skips) {
			return false
		}
		if w.isProvisionalQuarantinedLocked(slot.streamer, candidate) {
			return false
		}
		conflictSlots := append([]slotOccupant(nil), ordinary...)
		for i := range provisional {
			if &provisional[i] == slot || provisional[i].provisionalFallback == nil {
				continue
			}
			conflictSlots = append(conflictSlots, *provisional[i].provisionalFallback)
		}
		if provisionalConflictsWithSlots(candidate, conflictSlots) {
			return false
		}
		if !w.canStoreProvisionalProofLocked(candidate, slot.streamer) {
			return false
		}
		return !w.provisionalConflictsWithActivePermitsLocked(candidate, ignoreLeaseID)
	}
	// Source order already applies Campaign Policy and exact continuity ties.
	// Scan it once: a strict stronger first contender replaces the old lease;
	// an equal active contender is ordered first and retains its baseline; an
	// inadmissible first contender falls through to the next clean tuple.
	for i := range provisional {
		ignoreLeaseID := uint64(0)
		if w.provisionalLease != nil &&
			w.provisionalLease.Candidate.SameLeaseIdentity(*provisional[i].provisionalDrop) &&
			w.provisionalLeaseStreamer == provisional[i].streamer {
			ignoreLeaseID = w.provisionalLease.LeaseID
		}
		if admissible(&provisional[i], ignoreLeaseID) {
			selected = &provisional[i]
			break
		}
	}

	if selected == nil {
		w.clearProvisionalLeaseLocked()
		for _, slot := range provisional {
			restoreFallback(slot)
			waiting = appendProvisionalWaiting(waiting, slot, "provisional observation lacked exclusive current authority")
		}
		return ordinary, waiting
	}
	for i := range provisional {
		if &provisional[i] == selected {
			continue
		}
		restoreFallback(provisional[i])
		waiting = appendProvisionalWaiting(waiting, provisional[i], "another exact-Drop observation lease is active")
	}
	if len(ordinary) >= constants.MaxSimultaneousStreams {
		w.clearProvisionalLeaseLocked()
		restoreFallback(*selected)
		waiting = appendProvisionalWaiting(waiting, *selected, "provisional observation is fill-only and no watch slot is idle")
		return ordinary, waiting
	}

	candidate := *selected.provisionalDrop
	if w.provisionalLease == nil || !w.provisionalLease.Candidate.SameLeaseIdentity(candidate) ||
		w.provisionalLeaseStreamer != selected.streamer {
		w.clearProvisionalLeaseLocked()
		w.provisionalLeaseSeq++
		if w.provisionalLeaseSeq == 0 {
			w.provisionalLeaseSeq++
		}
		w.provisionalLease = &ProvisionalLease{
			LeaseID:    w.provisionalLeaseSeq,
			Candidate:  cloneProvisionalCandidateValue(candidate),
			State:      ProvisionalLeasePending,
			ReservedAt: now,
		}
		w.provisionalLeaseStreamer = selected.streamer
		w.publishProvisionalLeaseLocked()
	}

	result := append(ordinary, *selected)
	if len(result) > constants.MaxSimultaneousStreams {
		// arbitrate already enforces the cap; retain this defensive bound here so
		// no future caller can publish an observation lease as a third slot.
		result = result[:constants.MaxSimultaneousStreams]
	}
	return result, waiting
}

func removeWaitingChannel(waiting []WaitingChannel, login, origin string) []WaitingChannel {
	filtered := waiting[:0]
	for _, item := range waiting {
		if item.Channel == login && item.Origin == origin {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func appendProvisionalWaiting(waiting []WaitingChannel, slot slotOccupant, reason string) []WaitingChannel {
	return append(waiting, WaitingChannel{
		Channel:    slot.streamer.GetUsername(),
		Origin:     slot.origin,
		ReasonCode: ReasonLowerPriority,
		Reason:     reason,
	})
}

// provenProvisionalCandidates reconciles direct server proofs with this tick's full
// source proposal set. Omission is a source veto; current Known availability,
// liveness/game/session drift, and a new playback generation also remove the
// proof. It matches against every proposal sharing a quarantine key so source
// order cannot decide whether a causal proof-identity match survives.
func (w *MinuteWatcher) provenProvisionalCandidates(candidates []Candidate) []provenProvisionalCandidate {
	current := make(map[string][]provenProvisionalCandidate)
	// One coherent farming-exclusion decision per pass (immutable value).
	skips := w.rewardSkips.Load()
	for _, candidate := range candidates {
		if candidate.Streamer == nil || candidate.ProvisionalDrop == nil ||
			!provisionalProofCandidateCurrent(candidate.Streamer, *candidate.ProvisionalDrop, skips) {
			continue
		}
		key := candidate.ProvisionalDrop.QuarantineKey()
		current[key] = append(current[key], provenProvisionalCandidate{
			candidate: cloneProvisionalCandidateValue(*candidate.ProvisionalDrop),
			owner:     candidate.Streamer,
		})
	}

	w.observationMu.Lock()
	defer w.observationMu.Unlock()
	var proved []provenProvisionalCandidate
	if !w.provisionalMonitoring {
		w.provisionalProofs = nil
		return proved
	}
	for key, proof := range w.provisionalProofs {
		var matched *provenProvisionalCandidate
		for _, proposal := range current[key] {
			if proof.owner != proposal.owner || !proof.proof.Candidate.SameProofIdentity(proposal.candidate) {
				continue
			}
			if w.provisionalQuarantine.enforceAccepted &&
				!w.provisionalCandidateAcceptedLocked(proposal.owner, proposal.candidate) {
				continue
			}
			if matched == nil || provisionalEnvelopeNewer(proposal.candidate, matched.candidate) {
				copy := proposal
				matched = &copy
			}
		}
		if matched == nil {
			delete(w.provisionalProofs, key)
			continue
		}
		proof.proof.Candidate = cloneProvisionalCandidateValue(matched.candidate)
		w.provisionalProofs[key] = proof
		proved = append(proved, provenProvisionalCandidate{
			candidate: cloneProvisionalCandidateValue(matched.candidate),
			owner:     proof.owner,
			proofID:   proof.proof.ProofID,
		})
	}
	return proved
}

func provisionalEnvelopeNewer(candidate, current models.ProvisionalDropCandidate) bool {
	if candidate.AvailabilityObs != current.AvailabilityObs {
		return candidate.AvailabilityObs > current.AvailabilityObs
	}
	return candidate.DirectoryObs > current.DirectoryObs
}

func findProvisionalProofIdentity(candidates []provenProvisionalCandidate, owner *models.Streamer, target *models.ProvisionalDropCandidate) (uint64, bool) {
	if target == nil {
		return 0, false
	}
	for _, candidate := range candidates {
		if candidate.owner == owner && candidate.candidate.SameLeaseIdentity(*target) {
			// Resolve the id from the broker-owned proof under the caller's
			// arbitration snapshot; reconcile repeats the exact check.
			return candidate.proofID, true
		}
	}
	return 0, false
}

func (w *MinuteWatcher) provisionalConflictsWithActivePermitsLocked(candidate models.ProvisionalDropCandidate, ignoreLeaseID uint64) bool {
	for _, permit := range w.observationPermits {
		if ignoreLeaseID != 0 && (permit.leaseID == ignoreLeaseID || permit.leaseIDAtGrant == ignoreLeaseID) {
			continue
		}
		// An ordinary permit is granted before Send/Probe captures its transport
		// session. Mutable stream facts cannot close that small gap soundly, so any
		// live ordinary permit is a short global admission barrier for a NEW
		// unproved lease. The normal loop is sequential; this only defers a
		// concurrent canary/watchdog call until ReleaseObservationPermit.
		if permit.leaseID == 0 && permit.proofID == 0 {
			return true
		}
		if provisionalConflictsWithPermit(candidate, permit) {
			return true
		}
	}
	return false
}

func captureObservationConflict(streamer *models.Streamer) observationConflictSnapshot {
	if streamer == nil || streamer.Stream == nil {
		return observationConflictSnapshot{}
	}
	snapshot := streamer.Stream.ProvisionalDropSnapshot()
	return observationConflictSnapshot{
		gameID:            snapshot.GameID,
		channelID:         streamer.ChannelID,
		broadcastID:       snapshot.BroadcastID,
		sessionGeneration: snapshot.SessionGeneration,
		availability:      snapshot.Availability.State,
		availabilityObs:   snapshot.Availability.ObservationID,
		knownGeneration:   snapshot.Availability.KnownGeneration,
		campaignIDs:       append([]string(nil), snapshot.Availability.CampaignIDs...),
	}
}

func sameObservationConflict(left, right observationConflictSnapshot) bool {
	if left.gameID != right.gameID || left.channelID != right.channelID ||
		left.broadcastID != right.broadcastID || left.sessionGeneration != right.sessionGeneration ||
		left.availability != right.availability || left.availabilityObs != right.availabilityObs ||
		left.knownGeneration != right.knownGeneration || len(left.campaignIDs) != len(right.campaignIDs) {
		return false
	}
	for i := range left.campaignIDs {
		if left.campaignIDs[i] != right.campaignIDs[i] {
			return false
		}
	}
	return true
}

func (w *MinuteWatcher) canStoreProvisionalProofLocked(candidate models.ProvisionalDropCandidate, owner *models.Streamer) bool {
	if len(w.provisionalProofs) < constants.MaxSimultaneousStreams {
		return true
	}
	record, ok := w.provisionalProofs[candidate.QuarantineKey()]
	return ok && record.owner == owner && record.proof.Candidate.SameProofIdentity(candidate)
}

func provisionalConflictsWithPermit(candidate models.ProvisionalDropCandidate, permit observationPermitRecord) bool {
	return provisionalConflictsWithObservation(candidate, permit.conflict)
}

func provisionalConflictsWithObservation(candidate models.ProvisionalDropCandidate, facts observationConflictSnapshot) bool {
	if facts.gameID == "" {
		return true
	}
	if facts.gameID != candidate.GameID {
		return false
	}
	if candidate.Restricted() {
		if facts.channelID == "" {
			return true
		}
		if !containsExact(candidate.RestrictedACL, facts.channelID) {
			return false
		}
	}
	if facts.availability != models.CampaignAvailabilityKnown {
		return true
	}
	return containsExact(facts.campaignIDs, candidate.CampaignID)
}

func provisionalConflictsWithSlots(candidate models.ProvisionalDropCandidate, slots []slotOccupant) bool {
	for _, slot := range slots {
		if provisionalConflictsWithStreamer(candidate, slot.streamer) {
			return true
		}
	}
	return false
}

// provisionalConflictsWithStreamer is the fail-closed causal-attribution test.
// Only authoritative Known absence of the exact campaign can exclude a
// same-game ordinary beacon. For an open campaign, UNKNOWN conflicts. For a
// restricted campaign, the complete ACL excludes a non-member before channel
// availability is considered; an exact member still conflicts on UNKNOWN.
// Missing identity fails closed.
func provisionalConflictsWithStreamer(candidate models.ProvisionalDropCandidate, streamer *models.Streamer) bool {
	if streamer == nil || streamer.Stream == nil {
		return true
	}
	snapshot := streamer.Stream.ProvisionalDropSnapshot()
	if snapshot.GameID == "" {
		return true
	}
	if snapshot.GameID != candidate.GameID {
		return false
	}
	if candidate.Restricted() {
		if streamer.ChannelID == "" {
			return true
		}
		if !containsExact(candidate.RestrictedACL, streamer.ChannelID) {
			return false
		}
	}
	if snapshot.Availability.State != models.CampaignAvailabilityKnown {
		return true
	}
	if !containsExact(snapshot.Availability.CampaignIDs, candidate.CampaignID) {
		return false
	}
	return true
}

func provisionalCandidateCurrent(streamer *models.Streamer, candidate models.ProvisionalDropCandidate, skips *models.RewardSkips) bool {
	// Operator farming exclusion (DropRule.Skip): a skipped reward is never
	// current provisional justification. After a runtime rule flip the next
	// authoritative evaluation refuses the candidate here, so the lease dies
	// and permits are withheld through the ordinary release paths — never via
	// a quarantine negative, and without fabricating any observation state.
	if skips.SkipsProvisionalCandidate(candidate) {
		return false
	}
	if streamer == nil || streamer.Stream == nil || !candidate.Valid() ||
		!streamer.GetIsOnline() || streamer.GetUsername() != candidate.Login ||
		streamer.ChannelID == "" || streamer.ChannelID != candidate.ChannelID {
		return false
	}
	settings := streamer.GetSettings()
	if !settings.ClaimDrops || settings.DisableWatch {
		return false
	}
	// A provisional candidate is valid only before any authoritative campaign
	// assignment exists. A concurrent discovery/tracker assignment wins and the
	// ordinary broker path owns the channel from that point onward.
	if len(streamer.Stream.GetCampaigns()) != 0 {
		return false
	}
	snapshot := streamer.Stream.ProvisionalDropSnapshot()
	if snapshot.Availability.State != models.CampaignAvailabilityUnknown ||
		snapshot.Availability.ObservationID != candidate.AvailabilityObs ||
		snapshot.Availability.KnownGeneration != candidate.AvailabilityKnownGen ||
		snapshot.GameID != candidate.GameID || snapshot.BroadcastID != candidate.BroadcastID ||
		snapshot.SessionGeneration != candidate.SessionGeneration ||
		snapshot.HasConfirmedCampaign(candidate.CampaignID) {
		return false
	}
	if candidate.Restricted() {
		return containsExact(candidate.RestrictedACL, candidate.ChannelID)
	}
	return true
}

// provisionalProofCandidateCurrent revalidates the causal proof identity.
// Routine UNKNOWN and Directory observation generations do not revoke a
// positive server delta, but any authoritative Known publication (including
// Known-empty), owner/session/game drift, or ordinary assignment does.
func provisionalProofCandidateCurrent(streamer *models.Streamer, candidate models.ProvisionalDropCandidate, skips *models.RewardSkips) bool {
	// Operator farming exclusion — see provisionalCandidateCurrent. A proof for
	// a Skip-ruled reward stops justifying sends/slots; the record itself is
	// removed by the existing source-veto/currency deletion paths.
	if skips.SkipsProvisionalCandidate(candidate) {
		return false
	}
	if streamer == nil || streamer.Stream == nil || !candidate.Valid() ||
		!streamer.GetIsOnline() || streamer.GetUsername() != candidate.Login ||
		streamer.ChannelID == "" || streamer.ChannelID != candidate.ChannelID {
		return false
	}
	settings := streamer.GetSettings()
	if !settings.ClaimDrops || settings.DisableWatch || len(streamer.Stream.GetCampaigns()) != 0 {
		return false
	}
	snapshot := streamer.Stream.ProvisionalDropSnapshot()
	if snapshot.Availability.State != models.CampaignAvailabilityUnknown ||
		snapshot.Availability.KnownGeneration != candidate.AvailabilityKnownGen ||
		snapshot.GameID != candidate.GameID || snapshot.BroadcastID != candidate.BroadcastID ||
		snapshot.SessionGeneration != candidate.SessionGeneration ||
		snapshot.HasConfirmedCampaign(candidate.CampaignID) {
		return false
	}
	if candidate.Restricted() {
		return containsExact(candidate.RestrictedACL, candidate.ChannelID)
	}
	return true
}

func containsExact(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (w *MinuteWatcher) clearProvisionalLeaseLocked() {
	w.provisionalLease = nil
	w.provisionalLeaseStreamer = nil
	w.provisionalBootstrapReady = false
	w.clearQuarantineFenceLocked()
	w.provisionalLeasePublished.Store(nil)
}

func (w *MinuteWatcher) clearQuarantineFenceLocked() {
	w.quarantineFenceLeaseID = 0
	w.quarantineFenceRun = 0
	w.quarantineFenceDrainAt = time.Time{}
}

func (w *MinuteWatcher) publishProvisionalLeaseLocked() {
	if w.provisionalLease == nil {
		w.provisionalLeasePublished.Store(nil)
		return
	}
	copy := cloneProvisionalLease(*w.provisionalLease)
	w.provisionalLeasePublished.Store(&copy)
}

func cloneProvisionalLease(lease ProvisionalLease) ProvisionalLease {
	lease.Candidate = cloneProvisionalCandidateValue(lease.Candidate)
	return lease
}

func cloneProvisionalCandidate(candidate *models.ProvisionalDropCandidate) *models.ProvisionalDropCandidate {
	if candidate == nil {
		return nil
	}
	copy := cloneProvisionalCandidateValue(*candidate)
	return &copy
}

func cloneProvisionalCandidateValue(candidate models.ProvisionalDropCandidate) models.ProvisionalDropCandidate {
	candidate.RestrictedACL = append([]string(nil), candidate.RestrictedACL...)
	return candidate
}
