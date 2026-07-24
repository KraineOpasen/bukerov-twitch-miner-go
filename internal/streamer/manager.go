package streamer

import (
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// ProgressCallback is called during loading to report progress.
type ProgressCallback func(current, total int, username string)

// Manager handles loading, storing, and updating streamers.
//
// Identity is ID-first (BKM-006): the stable Twitch ChannelID is the
// authoritative match key, resolved from the CONFIGURED login via
// twitchClient.GetChannelID (there is no ID->login Twitch operation, so
// reconciliation is always config-driven). byID and byLogin are kept
// consistent with the ordered streamers slice under mu — byID never changes
// for a tracked streamer (ChannelID is immutable), byLogin is repointed in
// place on a rename so the SAME *models.Streamer is retained (settings,
// slot/watch state, history, PubSub subscriptions all survive a rename
// untouched).
//
// Lock order (strict, never reversed): Manager.mu -> Streamer.mu. Network I/O
// (GetChannelID, LoadChannelPointsContext) never runs while mu is held — see
// resolveConfigs (Phase A, unlocked) and reconcile (Phase B, locked, pure).
type Manager struct {
	client   twitchClient
	defaults models.StreamerSettings

	// streakCache persists watch-streak grants across restarts;
	// streakHydration is its snapshot loaded once before streamers are
	// created, applied to each new Streamer's Stream. Both may be nil
	// (feature off / library use) — everything degrades to the historical
	// re-pursue behavior.
	streakCache     *StreakCache
	streakHydration map[string]StreakGrant

	streamers []*models.Streamer
	// byID maps a streamer's stable ChannelID to its runtime object. Never
	// repointed once populated for a given key (the object retires only by
	// removal); the authoritative identity index.
	byID map[string]*models.Streamer
	// byLogin maps the CURRENT lowercase login to its runtime object. Updated
	// in place on a rename (old key removed, new key added, same pointer).
	byLogin map[string]*models.Streamer
	mu      sync.RWMutex
}

// SetStreakCache wires the persisted streak-grant cache. Must be called
// before LoadFromConfig so hydration covers the initial roster; runtime-added
// streamers hydrate from the same snapshot.
func (m *Manager) SetStreakCache(c *StreakCache) {
	m.streakCache = c
	if c != nil {
		m.streakHydration = c.Load(time.Now())
	}
}

// hydrateStreak seeds a persisted grant into a freshly created streamer. It
// runs before the streamer is published to byID/byLogin/streamers, so no
// other goroutine can observe it yet — reading via GetUsername() here is not
// required for safety, but is used for consistency with every other external
// reader.
func (m *Manager) hydrateStreak(s *models.Streamer) {
	if g, ok := m.streakHydration[s.GetUsername()]; ok {
		s.Stream.HydrateStreakGrant(g.BroadcastID, g.GrantedAt)
	}
}

// RecordStreakGrant persists the just-granted watch streak for username, so a
// restart mid-broadcast does not re-pursue it. No-op without a cache or when
// the broadcast was never identified.
func (m *Manager) RecordStreakGrant(username string) {
	if m.streakCache == nil {
		return
	}
	s := m.Get(username)
	if s == nil {
		return
	}
	bid, at := s.Stream.StreakEarnedGrant()
	m.streakCache.Record(s.GetUsername(), bid, at)
}

// twitchClient is the slice of the Twitch API the manager needs to resolve a
// streamer's channel ID and hydrate its channel-points context. Narrowed to
// an interface (satisfied by *api.TwitchClient, the production caller) so the
// reconciliation path can be exercised in tests without HTTP.
type twitchClient interface {
	GetChannelID(username string) (string, error)
	LoadChannelPointsContext(streamer *models.Streamer) error
	CheckStreamerOnline(streamer *models.Streamer) models.StatusTransition
}

// NewManager creates a new streamer manager.
func NewManager(client twitchClient, defaults models.StreamerSettings) *Manager {
	return &Manager{
		client:   client,
		defaults: defaults,
		byID:     make(map[string]*models.Streamer),
		byLogin:  make(map[string]*models.Streamer),
	}
}

// RenameEvent records one config-driven rename reconciled IN PLACE: the SAME
// runtime *models.Streamer had its login updated after Twitch confirmed the
// old and new logins resolve to the identical stable ChannelID. Consumers
// (miner config surgery, analytics history migration, chat presence) use
// this to bring every login-keyed side effect in line with the new identity.
type RenameEvent struct {
	Streamer  *models.Streamer
	OldLogin  string
	NewLogin  string
	ChannelID string
}

// ConflictKind classifies why a config entry's identity could not be
// reconciled automatically.
type ConflictKind string

const (
	// ConflictDuplicateSettings: two (or more) config entries resolve to the
	// SAME ChannelID but carry DIFFERENT effective settings. Ambiguous — the
	// manager never guesses which one should win, so neither is applied.
	ConflictDuplicateSettings ConflictKind = "duplicate_settings"
	// ConflictLoginCollision: the canonical (Twitch-resolved) login for a
	// ChannelID is already bound, in this manager, to a DIFFERENT
	// ChannelID. Fail closed: no overwrite, no deletion, no history move —
	// the already-tracked identity keeps its login ("stored ID wins").
	ConflictLoginCollision ConflictKind = "login_collision"
	// ConflictStoredChannelIDMismatch (BKM-006 Corrective Pass 1, C1): a
	// config entry carries a non-empty, PERSISTED ChannelID (the identity
	// this exact entry was bound to on a previous successful reconcile), but
	// its configured login now resolves to a DIFFERENT ChannelID. The stored
	// identity is an EXPECTED, immutable anchor, not a hint — the freshly
	// resolved (foreign) identity is never adopted: no streamer is added or
	// renamed for it, no existing streamer is overwritten, and no analytics
	// or config mutation of any kind happens for this entry. This is the
	// cold-restart counterpart of ConflictLoginCollision (which only ever
	// fires once byID/byLogin are already populated in-process); this kind
	// fires even from an EMPTY manager, because the expectation is read from
	// the config entry itself rather than from other tracked streamers.
	ConflictStoredChannelIDMismatch ConflictKind = "stored_channel_id_mismatch"
)

// ReconcileConflict is a privacy-safe (logins + ChannelID only — never a
// token, URL, header, or payload) description of a config entry that could
// not be reconciled automatically. It satisfies the error interface so
// callers can log or wrap it directly.
type ReconcileConflict struct {
	Kind      ConflictKind
	ChannelID string
	Logins    []string
}

func (c ReconcileConflict) Error() string {
	switch c.Kind {
	case ConflictDuplicateSettings:
		return fmt.Sprintf("streamer reconciliation conflict: logins %v resolve to the same channel (%s) with different settings; none applied",
			c.Logins, c.ChannelID)
	case ConflictLoginCollision:
		return fmt.Sprintf("streamer reconciliation conflict: login binding for channel %s collides with an existing different channel (%v); identity retained",
			c.ChannelID, c.Logins)
	case ConflictStoredChannelIDMismatch:
		return fmt.Sprintf("streamer reconciliation conflict: login %v now resolves to channel %s, which differs from its persisted stable identity; refusing to adopt the foreign identity",
			c.Logins, c.ChannelID)
	default:
		return fmt.Sprintf("streamer reconciliation conflict (channel %s)", c.ChannelID)
	}
}

// resolvedEntry is one config entry's login->ChannelID resolution result,
// captured OUTSIDE any manager/streamer lock (Phase A). err is preserved (not
// just logged) so Phase B can tell "unresolved this cycle" apart from a
// confirmed identity and never delete/rename/add on transient data.
type resolvedEntry struct {
	login    string
	settings models.StreamerSettings
	id       string
	err      error
	// expectedID is the config entry's PERSISTED ChannelID (config.StreamerConfig.ChannelID),
	// trimmed. Empty means "no expectation yet" (first-bind path). Non-empty
	// is an EXPECTED IMMUTABLE identity (BKM-006 Corrective Pass 1, C1): if
	// the freshly resolved id differs, the entry is a conflict, never a hint
	// to overwrite.
	expectedID string
}

// resolveConfigs performs Phase A of the ID-first reconciliation: it resolves
// every config entry's login to its stable ChannelID via the Twitch client,
// with NO manager or streamer lock held — network I/O must never run under
// mu. A resolution failure (transient error, stale persisted-query hash, or a
// genuinely unknown login) is recorded on the entry rather than dropped, so
// Phase B can decide to keep any already-tracked streamer untouched instead
// of guessing.
func (m *Manager) resolveConfigs(configs []config.StreamerConfig, defaults models.StreamerSettings, onProgress ProgressCallback) []resolvedEntry {
	out := make([]resolvedEntry, 0, len(configs))
	total := len(configs)
	for i, sc := range configs {
		if onProgress != nil {
			onProgress(i+1, total, sc.Username)
		}
		login := strings.ToLower(sc.Username)
		if login == "" {
			continue
		}
		effective := defaults
		if sc.Settings != nil {
			effective = *sc.Settings
		}

		id, err := m.client.GetChannelID(login)
		if err != nil {
			if errors.Is(err, api.ErrPersistedQueryNotFound) {
				slog.Warn("Could not resolve channel ID: stale Twitch query metadata (not a missing channel); keeping any existing streamer, will retry on next apply",
					"username", login, "error", err)
			} else {
				slog.Warn("Could not resolve channel ID for configured streamer; keeping any existing streamer, will retry on next apply",
					"username", login, "error", err)
			}
		}
		out = append(out, resolvedEntry{
			login:      login,
			settings:   effective,
			id:         id,
			err:        err,
			expectedID: strings.TrimSpace(sc.ChannelID),
		})
	}
	return out
}

// planStepKind classifies one ReconcilePlan step.
type planStepKind int

const (
	// planStepAdd: no streamer is currently tracked for this ChannelID; a new
	// one will be created.
	planStepAdd planStepKind = iota
	// planStepUpdate: an EXISTING tracked streamer (matched by ChannelID) will
	// be updated in place — a login rename (if canonicalLogin differs from its
	// current login, guarded by the CAS obs) and/or a settings replacement.
	planStepUpdate
)

// planStep is one immutable, already-decided reconciliation action, computed
// by PlanReconcile under a read lock with no mutation, replayed verbatim by
// CommitPlan under the write lock.
type planStep struct {
	kind           planStepKind
	channelID      string
	canonicalLogin string
	effective      models.StreamerSettings
	// tracked is non-nil only for planStepUpdate.
	tracked *models.Streamer
	// obs is the login-observation generation captured for tracked at PLAN
	// time (BKM-006 I12) — CommitPlan passes it to RenameIfCurrent unchanged,
	// so a rename decided from a stale/slow resolution can still be
	// recognized as superseded by a newer one and discarded rather than
	// rolling anything back.
	obs uint64
	// dupLogins is set (len > 1) when more than one config entry coalesced
	// into this single step, for the diagnostic "reconciled duplicate config
	// entries" log line CommitPlan emits.
	dupLogins []string
}

// ReconcilePlan is the immutable result of PlanReconcile: everything
// CommitPlan will do to reconcile the roster, decided under a read lock from
// a Phase-A resolution that ran fully unlocked — but with NO mutation of the
// manager or any streamer applied yet (BeginLoginObservation is the sole
// exception: it must run before Phase A per I12, and is a self-contained
// per-streamer generation bump that decides nothing by itself). A caller may
// discard a plan without any side effect ever having occurred. This is what
// lets the miner's rename coordinator preflight durable persistence
// (analytics history migration, config.json) BEFORE committing an identity
// change to the live runtime (BKM-006 Corrective Pass 1, C2).
type ReconcilePlan struct {
	steps     []planStep
	conflicts []ReconcileConflict
	// survivors are currently-tracked streamers that CommitPlan's removal
	// sweep must keep even though they own no planStep this cycle (a
	// conflict, or an unresolved entry, that still names them).
	survivors map[*models.Streamer]bool
}

// PlannedRenames returns every login rename this plan intends to commit, by
// re-reading each planStepUpdate's tracked streamer's CURRENT login at call
// time (not a value cached during planning) — self-correcting if the
// streamer already changed login by some other means between PlanReconcile
// and this call. A caller (the miner's rename coordinator) uses this to
// preflight durable persistence BEFORE any runtime mutation occurs.
func (p *ReconcilePlan) PlannedRenames() []RenameEvent {
	var out []RenameEvent
	for _, step := range p.steps {
		if step.kind != planStepUpdate {
			continue
		}
		if current := step.tracked.GetUsername(); current != step.canonicalLogin {
			out = append(out, RenameEvent{
				Streamer: step.tracked, OldLogin: current, NewLogin: step.canonicalLogin, ChannelID: step.channelID,
			})
		}
	}
	return out
}

// ResolvedChannelIDs returns the stable ChannelID resolved for every entry
// this plan will track after CommitPlan (added and updated), keyed by the
// CANONICAL (lowercase) login CommitPlan will assign it. The miner's rename
// coordinator uses this to stamp config.json's ChannelID fields from the SAME
// resolution the durable persist step is about to save, entirely before any
// runtime mutation.
func (p *ReconcilePlan) ResolvedChannelIDs() map[string]string {
	out := make(map[string]string, len(p.steps))
	for _, step := range p.steps {
		out[step.canonicalLogin] = step.channelID
	}
	return out
}

// PlanReconcile performs Phase A (resolve every config entry's stable
// ChannelID, unlocked) and Phase B's DECISION-MAKING ONLY (grouping,
// conflict detection, add-vs-update classification) under a read lock — with
// NO mutation of the manager or any streamer. See ReconcilePlan's doc comment
// for why this split exists.
func (m *Manager) PlanReconcile(configs []config.StreamerConfig, defaults models.StreamerSettings, onProgress ProgressCallback) *ReconcilePlan {
	// Stamp a login-observation generation for every currently-tracked
	// streamer BEFORE any Phase A I/O (I12): a rename decision computed from
	// THIS call's (possibly slow) resolution can then be recognized as stale
	// if a faster, more recent apply already moved the streamer's login in
	// the meantime, and is discarded instead of rolling it back.
	m.mu.Lock()
	m.defaults = defaults
	obsSnapshot := make(map[*models.Streamer]uint64, len(m.streamers))
	for _, s := range m.streamers {
		obsSnapshot[s] = s.BeginLoginObservation()
	}
	m.mu.Unlock()

	entries := m.resolveConfigs(configs, defaults, onProgress)

	plan := &ReconcilePlan{survivors: make(map[*models.Streamer]bool)}

	m.mu.RLock()
	defer m.mu.RUnlock()

	groups := make(map[string][]resolvedEntry)
	var order []string

	for _, e := range entries {
		if e.err != nil {
			// Unresolved this cycle (transient/PQNF/unknown): keep any
			// already-tracked streamer under that exact login untouched. Never
			// delete, never rename, never fabricate an identity. If the login
			// lookup misses (e.g. the tracked streamer's current login already
			// diverged from this stale config entry) but a persisted ChannelID
			// still names it, fall back to the ID lookup — same fail-closed
			// "keep whatever is already tracked" intent (C1).
			if s := m.byLogin[e.login]; s != nil {
				plan.survivors[s] = true
			} else if e.expectedID != "" {
				if s := m.byID[e.expectedID]; s != nil {
					plan.survivors[s] = true
				}
			}
			continue
		}
		if e.expectedID != "" && e.expectedID != e.id {
			// BKM-006 Corrective Pass 1, C1: the persisted ChannelID is an
			// EXPECTED IMMUTABLE identity, not a hint. A freshly resolved id
			// that differs is a foreign broadcaster — never adopted, never
			// overwritten. No add, no rename, no settings change, no analytics
			// or config mutation for this entry; whatever is ALREADY tracked
			// under the stored id (if anything, e.g. not on a cold restart) is
			// kept untouched.
			plan.conflicts = append(plan.conflicts, ReconcileConflict{
				Kind:      ConflictStoredChannelIDMismatch,
				ChannelID: e.expectedID,
				Logins:    []string{e.login},
			})
			slog.Warn("Streamer reconciliation conflict: configured login now resolves to a DIFFERENT channel than its persisted stable identity; refusing to adopt the foreign identity",
				"login", e.login, "storedChannelID", e.expectedID, "resolvedChannelID", e.id)
			if s := m.byID[e.expectedID]; s != nil {
				plan.survivors[s] = true
			}
			continue
		}
		if _, ok := groups[e.id]; !ok {
			order = append(order, e.id)
		}
		groups[e.id] = append(groups[e.id], e)
	}

	for _, id := range order {
		grp := groups[id]

		var canonicalLogin string
		var effective models.StreamerSettings
		if len(grp) > 1 {
			allEqual := true
			for _, e := range grp[1:] {
				if !settingsEqual(grp[0].settings, e.settings) {
					allEqual = false
					break
				}
			}
			if !allEqual {
				plan.conflicts = append(plan.conflicts, ReconcileConflict{
					Kind:      ConflictDuplicateSettings,
					ChannelID: id,
					Logins:    loginsOf(grp),
				})
				slog.Warn("Streamer reconciliation conflict: same channel configured more than once with different settings; none applied",
					"channelID", id, "logins", loginsOf(grp))
				if s := m.byID[id]; s != nil {
					plan.survivors[s] = true
				}
				continue
			}
			// Coalesce: every duplicate entry agrees on settings, so they
			// describe the SAME intent for one channel. The canonical login is
			// simply the last-listed Twitch-resolved login (deterministic,
			// order-stable) — there is no ID->login op to prefer one over the
			// other authoritatively.
			canonicalLogin = grp[len(grp)-1].login
			effective = grp[0].settings
		} else {
			canonicalLogin = grp[0].login
			effective = grp[0].settings
		}

		tracked := m.byID[id]

		if tracked == nil {
			if owner := m.byLogin[canonicalLogin]; owner != nil {
				// canonicalLogin is already bound to a DIFFERENT ChannelID.
				// Fail closed: no overwrite, no deletion, no history move — the
				// already-tracked identity keeps its login. owner's OWN
				// ChannelID never appeared in this cycle's resolved groups (its
				// login was claimed by this conflicting entry instead), so it
				// must be marked a survivor explicitly or the removal scan
				// below would delete it merely for being untouched.
				plan.conflicts = append(plan.conflicts, ReconcileConflict{
					Kind:      ConflictLoginCollision,
					ChannelID: id,
					Logins:    []string{canonicalLogin, owner.GetUsername()},
				})
				slog.Warn("Streamer reconciliation conflict: configured login already resolves to a different tracked channel; skipping",
					"login", canonicalLogin, "channelID", id, "existingChannelID", owner.ChannelID)
				plan.survivors[owner] = true
				continue
			}

			var dup []string
			if len(grp) > 1 {
				dup = loginsOf(grp)
			}
			plan.steps = append(plan.steps, planStep{
				kind: planStepAdd, channelID: id, canonicalLogin: canonicalLogin, effective: effective, dupLogins: dup,
			})
			continue
		}

		if owner := m.byLogin[canonicalLogin]; owner != nil && owner != tracked {
			// owner's own ChannelID also never appeared in this cycle's
			// resolved groups (its login was claimed by this entry instead) —
			// mark it a survivor too so it is not incidentally removed.
			plan.conflicts = append(plan.conflicts, ReconcileConflict{
				Kind:      ConflictLoginCollision,
				ChannelID: id,
				Logins:    []string{canonicalLogin, owner.GetUsername()},
			})
			slog.Warn("Streamer reconciliation conflict: configured login already resolves to a different tracked channel; identity retained",
				"login", canonicalLogin, "channelID", id, "existingLogin", tracked.GetUsername())
			plan.survivors[tracked] = true
			plan.survivors[owner] = true
			continue
		}

		obs, ok := obsSnapshot[tracked]
		if !ok {
			obs = tracked.BeginLoginObservation()
		}
		var dup []string
		if len(grp) > 1 {
			dup = loginsOf(grp)
		}
		plan.steps = append(plan.steps, planStep{
			kind: planStepUpdate, channelID: id, canonicalLogin: canonicalLogin, effective: effective,
			tracked: tracked, obs: obs, dupLogins: dup,
		})
		plan.survivors[tracked] = true
	}

	return plan
}

// CommitPlan applies a plan PlanReconcile computed: creates streamers for
// every planStepAdd, renames/updates settings for every planStepUpdate (a
// rename is still guarded by the CAS obs captured at plan time — I12 — so a
// commit that lost a race with a newer reconciliation is discarded rather
// than rolling anything back), and removes every currently-tracked streamer
// the plan's survivor set does not name. Defensively re-checks byID/byLogin
// at commit time before creating a NEW streamer for a planStepAdd: if the
// manager and this plan were NOT serialized by an external lock (the
// production miner's rename coordinator does serialize them; see
// applySettings) and something else raced a create for the same ChannelID in
// between, this falls back to updating the already-created streamer instead
// of ever tracking two runtime objects for one immutable identity.
// LoadChannelPointsContext for genuinely new streamers runs in Phase C,
// unlocked, exactly as before this pass' PlanReconcile/CommitPlan split.
func (m *Manager) CommitPlan(plan *ReconcilePlan) (added, removed []*models.Streamer, changed []SettingsChange, renamed []RenameEvent, conflicts []ReconcileConflict) {
	conflicts = plan.conflicts
	survivors := plan.survivors

	m.mu.Lock()

	var addedStreamers []*models.Streamer

	applyUpdate := func(tracked *models.Streamer, channelID, canonicalLogin string, effective models.StreamerSettings, obs uint64) {
		oldLogin := tracked.GetUsername()
		if oldLogin != canonicalLogin {
			if tracked.RenameIfCurrent(canonicalLogin, obs) {
				delete(m.byLogin, oldLogin)
				m.byLogin[canonicalLogin] = tracked
				renamed = append(renamed, RenameEvent{Streamer: tracked, OldLogin: oldLogin, NewLogin: canonicalLogin, ChannelID: channelID})
				slog.Debug("Reconciled streamer rename by stable channel ID in place",
					"channelID", channelID, "newLogin", canonicalLogin)
			} else {
				// A newer apply already renamed this streamer; resync the
				// index to its actual current login instead of clobbering it.
				actual := tracked.GetUsername()
				m.byLogin[actual] = tracked
			}
		}
		old := tracked.GetSettings()
		if !settingsEqual(old, effective) {
			tracked.SetSettings(effective)
			changed = append(changed, SettingsChange{Streamer: tracked, Old: old, New: effective})
		}
		survivors[tracked] = true
	}

	for _, step := range plan.steps {
		switch step.kind {
		case planStepAdd:
			if tracked := m.byID[step.channelID]; tracked != nil {
				// Raced with another commit since planning; see doc comment.
				if owner := m.byLogin[step.canonicalLogin]; owner != nil && owner != tracked {
					conflicts = append(conflicts, ReconcileConflict{
						Kind: ConflictLoginCollision, ChannelID: step.channelID,
						Logins: []string{step.canonicalLogin, owner.GetUsername()},
					})
					survivors[tracked] = true
					survivors[owner] = true
					continue
				}
				applyUpdate(tracked, step.channelID, step.canonicalLogin, step.effective, tracked.BeginLoginObservation())
				continue
			}
			if owner := m.byLogin[step.canonicalLogin]; owner != nil {
				conflicts = append(conflicts, ReconcileConflict{
					Kind: ConflictLoginCollision, ChannelID: step.channelID,
					Logins: []string{step.canonicalLogin, owner.GetUsername()},
				})
				survivors[owner] = true
				continue
			}
			s := models.NewStreamer(step.canonicalLogin, step.effective)
			m.hydrateStreak(s)
			s.ChannelID = step.channelID
			m.byID[step.channelID] = s
			m.byLogin[step.canonicalLogin] = s
			survivors[s] = true
			addedStreamers = append(addedStreamers, s)
			added = append(added, s)
			slog.Info("Added new streamer", "username", step.canonicalLogin, "channelID", step.channelID)
			if len(step.dupLogins) > 1 {
				slog.Info("Reconciled duplicate config entries for one channel",
					"channelID", step.channelID, "logins", step.dupLogins, "canonicalLogin", step.canonicalLogin)
			}
		case planStepUpdate:
			applyUpdate(step.tracked, step.channelID, step.canonicalLogin, step.effective, step.obs)
			if len(step.dupLogins) > 1 {
				slog.Info("Reconciled duplicate config entries for one channel",
					"channelID", step.channelID, "logins", step.dupLogins, "canonicalLogin", step.canonicalLogin)
			}
		}
	}

	var kept []*models.Streamer
	for _, s := range m.streamers {
		if survivors[s] {
			kept = append(kept, s)
			continue
		}
		removed = append(removed, s)
		delete(m.byID, s.ChannelID)
		delete(m.byLogin, s.GetUsername())
		slog.Info("Removed streamer", "username", s.GetUsername())
	}
	kept = append(kept, addedStreamers...)
	m.streamers = kept

	m.mu.Unlock()

	// Phase C: hydrate channel-points context for genuinely new streamers
	// only, outside any lock. A failure here is non-fatal — the streamer is
	// kept with whatever points it last had (zero for a brand-new one).
	for _, s := range addedStreamers {
		if err := m.client.LoadChannelPointsContext(s); err != nil {
			if errors.Is(err, api.ErrPersistedQueryNotFound) {
				slog.Warn("Channel points context unavailable for new streamer (stale Twitch query metadata); keeping streamer",
					"streamer", s.GetUsername(), "error", err)
			} else {
				slog.Warn("Failed to load channel points for new streamer", "streamer", s.GetUsername(), "error", err)
			}
		}
	}

	return added, removed, changed, renamed, conflicts
}

// reconcile is the shared ID-first reconciliation core for both
// LoadFromConfig (initial, empty roster) and the LEGACY (non-rename-coordinator)
// ApplySettings entry point: it composes PlanReconcile immediately followed by
// CommitPlan, preserving the exact external behavior this function always had.
// Callers that need to preflight durable persistence BEFORE committing a
// runtime mutation (the miner's rename coordinator, BKM-006 Corrective Pass 1
// C2) must call PlanReconcile and CommitPlan directly instead, with their own
// work in between.
func (m *Manager) reconcile(configs []config.StreamerConfig, defaults models.StreamerSettings, onProgress ProgressCallback) (added, removed []*models.Streamer, changed []SettingsChange, renamed []RenameEvent, conflicts []ReconcileConflict) {
	plan := m.PlanReconcile(configs, defaults, onProgress)
	return m.CommitPlan(plan)
}

// loginsOf extracts the logins of a resolvedEntry group for a privacy-safe
// conflict/coalesce log line (no tokens/URLs/headers/payloads — just logins).
func loginsOf(grp []resolvedEntry) []string {
	out := make([]string, len(grp))
	for i, e := range grp {
		out[i] = e.login
	}
	return out
}

// LoadFromConfig loads streamers from configuration.
// Returns an error if no valid streamers are found.
func (m *Manager) LoadFromConfig(configs []config.StreamerConfig, onProgress ProgressCallback) error {
	slog.Info("Loading streamers", "count", len(configs))

	m.mu.RLock()
	defaults := m.defaults
	m.mu.RUnlock()

	added, _, _, _, conflicts := m.reconcile(configs, defaults, onProgress)
	for _, c := range conflicts {
		slog.Warn("Streamer not loaded due to reconciliation conflict", "detail", c.Error())
	}
	for _, s := range added {
		slog.Info("Loaded streamer",
			"username", s.GetUsername(),
			"channelID", s.ChannelID,
			"points", s.GetChannelPoints(),
		)
	}

	if m.Count() == 0 {
		return fmt.Errorf("no valid streamers found")
	}
	return nil
}

// All returns a copy of the loaded streamer list. Callers (pubsub pool,
// watcher, drops tracker, web server) hold the result long-term, so handing
// out the internal slice would let a later ApplySettings append/replace
// mutate their view concurrently.
func (m *Manager) All() []*models.Streamer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]*models.Streamer(nil), m.streamers...)
}

// Count returns the number of loaded streamers.
func (m *Manager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.streamers)
}

// Get returns a streamer by its CURRENT login (case-insensitive). Because
// byLogin is repointed on every rename, this always resolves the new login
// immediately after a reconcile — the old login no longer resolves to
// anything (I3).
func (m *Manager) Get(username string) *models.Streamer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.byLogin[strings.ToLower(username)]
}

// GetByChannelID returns a streamer by its stable, immutable ChannelID — the
// authoritative identity key that survives a rename untouched.
func (m *Manager) GetByChannelID(id string) *models.Streamer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.byID[id]
}

// Names returns a list of all streamer usernames.
func (m *Manager) Names() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	names := make([]string, len(m.streamers))
	for i, s := range m.streamers {
		names[i] = s.GetUsername()
	}
	return names
}

// PointsMap returns a map of streamer usernames to their current points.
func (m *Manager) PointsMap() map[string]int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	points := make(map[string]int, len(m.streamers))
	for _, s := range m.streamers {
		points[s.GetUsername()] = s.GetChannelPoints()
	}
	return points
}

// SettingsChange records one EXISTING streamer whose effective settings were
// replaced by an ApplySettings call, with the before/after snapshots. Added and
// removed streamers are never reported here — they appear only in their own
// lists — so a caller can reconcile each roster member exactly once.
type SettingsChange struct {
	Streamer *models.Streamer
	Old      models.StreamerSettings
	New      models.StreamerSettings
}

// settingsEqual compares two effective settings snapshots by value, following
// pointers (ChatLogs) so nil ("inherit global") stays distinct from an explicit
// false override.
func settingsEqual(a, b models.StreamerSettings) bool {
	return reflect.DeepEqual(a, b)
}

// ApplySettings reconciles the runtime roster with configs by stable
// ChannelID (BKM-006): a login that now resolves to an already-tracked
// ChannelID updates that SAME streamer in place (rename — settings,
// slot/watch state, history, and PubSub subscriptions all survive
// untouched) rather than removing and re-adding it as a second object.
// Returns the added and removed streamers, a change record for every
// existing streamer whose effective settings actually differ, and every
// rename actually applied, so the caller can reconcile runtime capabilities
// (PubSub topics, chat presence), config.json, and analytics history without
// a restart. Kept objects retain their identity — ChannelID included.
func (m *Manager) ApplySettings(configs []config.StreamerConfig, defaults models.StreamerSettings) (added, removed []*models.Streamer, changed []SettingsChange, renamed []RenameEvent) {
	added, removed, changed, renamed, conflicts := m.reconcile(configs, defaults, nil)
	for _, c := range conflicts {
		slog.Warn("Streamer settings not applied due to reconciliation conflict", "detail", c.Error())
	}
	return added, removed, changed, renamed
}

// CheckOnlineStatus checks the online status for all streamers.
func (m *Manager) CheckOnlineStatus() {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, streamer := range m.streamers {
		m.client.CheckStreamerOnline(streamer)
	}
}

// PrintReport logs a session report for all streamers.
func (m *Manager) PrintReport() {
	m.mu.RLock()
	defer m.mu.RUnlock()

	slog.Info("=== Session Report ===")

	for _, streamer := range m.streamers {
		slog.Info("Streamer stats",
			"username", streamer.GetUsername(),
			"points", streamer.GetChannelPoints(),
		)

		for reason, entry := range streamer.History {
			if entry.Counter > 0 || entry.Amount != 0 {
				slog.Info("  History",
					"reason", reason,
					"count", entry.Counter,
					"amount", entry.Amount,
				)
			}
		}
	}
}
