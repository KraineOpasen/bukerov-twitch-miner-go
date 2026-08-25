package streamer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/util"
)

const streakCacheVersion = 2

// streakCacheEntry keeps the existing top-level login -> object cache shape.
// BroadcastID/GrantedAt are read-only compatibility fields for the v1 format.
// The old code inferred BroadcastID from the stream observed at arrival time,
// which is not proof, so a v1 grant is migrated explicitly as GRANTED_UNBOUND.
type streakCacheEntry struct {
	Version  int                           `json:"version,omitempty"`
	Revision uint64                        `json:"revision,omitempty"`
	Timeout  *models.WatchStreakTimeout    `json:"timeout,omitempty"`
	Grants   []models.WatchStreakGrantFact `json:"grants,omitempty"`

	BroadcastID string    `json:"broadcastId,omitempty"`
	GrantedAt   time.Time `json:"grantedAt,omitempty"`
}

// StreakCache persists the Stream-owned watch-streak terminal snapshot across
// restarts in the existing atomic JSON cache. It is deliberately not a second
// state machine: validation, binding and transitions remain owned by Stream.
type StreakCache struct {
	mu   sync.Mutex
	path string
}

func NewStreakCache(path string) *StreakCache {
	return &StreakCache{path: path}
}

// Load returns validated Stream snapshots. A missing file is a normal first
// run; unreadable or corrupt input is reported and fails explicitly safe to no
// manufactured grant or timeout.
func (c *StreakCache) Load(now time.Time) map[string]models.WatchStreakPersistence {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loadLocked(now)
}

func (c *StreakCache) loadLocked(now time.Time) map[string]models.WatchStreakPersistence {
	out := make(map[string]models.WatchStreakPersistence)
	data, err := os.ReadFile(c.path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("Streak cache is unreadable; ignoring it without manufacturing streak success",
				"path", c.path, "error", err)
		}
		return out
	}

	var raw map[string]streakCacheEntry
	if err := json.Unmarshal(data, &raw); err != nil {
		slog.Warn("Streak cache is corrupt; ignoring it without manufacturing streak success",
			"path", c.path, "error", err)
		return out
	}
	for login, entry := range raw {
		if state, ok := normalizeStreakCacheEntry(strings.ToLower(login), entry, now); ok {
			out[strings.ToLower(login)] = state
		}
	}
	return out
}

func normalizeStreakCacheEntry(login string, entry streakCacheEntry, _ time.Time) (models.WatchStreakPersistence, bool) {
	state := models.WatchStreakPersistence{Revision: entry.Revision}
	if entry.Timeout != nil && entry.Timeout.BroadcastID != "" && validStreakFact(entry.Timeout.TimedOutAt) {
		timeout := *entry.Timeout
		state.Timeout = &timeout
	}

	seen := make(map[string]struct{}, len(entry.Grants)+1)
	for _, grant := range entry.Grants {
		validBound := grant.Binding == models.WatchStreakGrantBound && grant.BroadcastID != ""
		validUnbound := grant.Binding == models.WatchStreakGrantUnbound && grant.BroadcastID == ""
		if grant.EventID == "" || (!validBound && !validUnbound) || !validStreakFact(grant.AcceptedAt) {
			continue
		}
		if _, duplicate := seen[grant.EventID]; duplicate {
			continue
		}
		seen[grant.EventID] = struct{}{}
		state.Grants = append(state.Grants, grant)
	}

	// v1 compatibility: the historical BroadcastID association was an arrival-
	// time guess, not provenance. Preserve the real grant without falsely ending
	// any broadcast-specific pursuit.
	if len(entry.Grants) == 0 && entry.BroadcastID != "" && validStreakFact(entry.GrantedAt) {
		h := sha256.Sum256([]byte("legacy-watch-streak-v1\x00" + login + "\x00" + entry.BroadcastID + "\x00" + entry.GrantedAt.UTC().Format(time.RFC3339Nano)))
		eventID := "legacy-v1:" + hex.EncodeToString(h[:])
		state.Grants = append(state.Grants, models.WatchStreakGrantFact{
			EventID:    eventID,
			Binding:    models.WatchStreakGrantUnbound,
			AcceptedAt: entry.GrantedAt,
		})
		if state.Revision == 0 {
			state.Revision = 1
		}
	}

	sort.Slice(state.Grants, func(i, j int) bool { return state.Grants[i].EventID < state.Grants[j].EventID })
	return state, state.Timeout != nil || len(state.Grants) > 0
}

func validStreakFact(at time.Time) bool {
	return !at.IsZero()
}

func entryFromPersistence(state models.WatchStreakPersistence) streakCacheEntry {
	entry := streakCacheEntry{Version: streakCacheVersion, Revision: state.Revision}
	if state.Timeout != nil {
		timeout := *state.Timeout
		entry.Timeout = &timeout
	}
	entry.Grants = append([]models.WatchStreakGrantFact(nil), state.Grants...)
	sort.Slice(entry.Grants, func(i, j int) bool { return entry.Grants[i].EventID < entry.Grants[j].EventID })
	return entry
}

// Record atomically writes one full immutable transition snapshot. Revisions
// prevent a delayed timeout write from replacing a newer grant/ledger state.
// It returns true only when this snapshot became the persisted value.
func (c *StreakCache) Record(login string, incoming models.WatchStreakPersistence, now time.Time) bool {
	login = strings.ToLower(strings.TrimSpace(login))
	if login == "" || incoming.Revision == 0 {
		return false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	states := c.loadLocked(now)
	if current, ok := states[login]; ok && incoming.Revision <= current.Revision {
		return false
	}
	normalized, ok := normalizeStreakCacheEntry(login, entryFromPersistence(incoming), now)
	if !ok {
		return false
	}
	states[login] = normalized
	return c.writeLocked(states)
}

// Rename moves the login key after an identity-preserving streamer rename and
// publishes the exact full snapshot captured from the live Stream owner. A
// pre-existing destination key may belong to a retired different identity, so
// cache-local revision comparison must not guess that it is newer truth. The
// manager serializes this call with Record and captures ownerState inside that
// serialization boundary; a racing accepted event is therefore present in this
// snapshot or records a later revision after the move.
func (c *StreakCache) Rename(oldLogin, newLogin string, ownerState models.WatchStreakPersistence, now time.Time) bool {
	oldLogin = strings.ToLower(strings.TrimSpace(oldLogin))
	newLogin = strings.ToLower(strings.TrimSpace(newLogin))
	if oldLogin == "" || newLogin == "" {
		return false
	}
	if oldLogin == newLogin {
		return true
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	states := c.loadLocked(now)
	_, oldExists := states[oldLogin]
	_, newExists := states[newLogin]
	delete(states, oldLogin)
	delete(states, newLogin)
	if normalized, ok := normalizeStreakCacheEntry(newLogin, entryFromPersistence(ownerState), now); ok {
		states[newLogin] = normalized
	} else if !oldExists && !newExists {
		return true
	}
	return c.writeLocked(states)
}

func (c *StreakCache) writeLocked(states map[string]models.WatchStreakPersistence) bool {
	raw := make(map[string]streakCacheEntry, len(states))
	for key, state := range states {
		raw[key] = entryFromPersistence(state)
	}
	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		slog.Warn("Failed to encode streak cache", "error", err)
		return false
	}
	if err := util.WriteFileAtomic(c.path, data, 0o644); err != nil {
		slog.Warn("Failed to write streak cache; terminal state will not survive restart", "path", c.path, "error", err)
		return false
	}
	return true
}

// Remove deletes login's terminal snapshot so deletion/re-add cannot inherit
// it. The write remains atomic and case-insensitive.
func (c *StreakCache) Remove(login string) {
	login = strings.ToLower(strings.TrimSpace(login))
	if login == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	data, err := os.ReadFile(c.path)
	if err != nil {
		return
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return
	}
	removed := false
	for key := range raw {
		if strings.EqualFold(key, login) {
			delete(raw, key)
			removed = true
		}
	}
	if !removed {
		return
	}
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return
	}
	if err := util.WriteFileAtomic(c.path, out, 0o644); err != nil {
		slog.Warn("Failed to write streak cache after removing a deleted streamer", "path", c.path, "error", err)
	}
}
