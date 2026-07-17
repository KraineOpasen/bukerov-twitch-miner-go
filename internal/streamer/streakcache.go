package streamer

import (
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/util"
)

// streakCacheTTL bounds how long a persisted grant is trusted. A watch-streak
// grant only matters while its broadcast is still live; 48h comfortably
// covers even marathon streams while guaranteeing stale entries can never
// block a pursuit weeks later.
const streakCacheTTL = 48 * time.Hour

// StreakGrant is one persisted watch-streak grant: which broadcast it
// belonged to and when it was granted.
type StreakGrant struct {
	BroadcastID string    `json:"broadcastId"`
	GrantedAt   time.Time `json:"grantedAt"`
}

// StreakCache persists watch-streak grants (login -> grant) across process
// restarts in a small JSON file next to the database, in the spirit of
// 0x8fv's watch_streak_warm_start_cache. It is deliberately NOT a schema
// migration: the fact is ephemeral (hours), needs no relational queries, and
// a lost/corrupt file only degrades to the historical behavior (re-pursue).
type StreakCache struct {
	mu   sync.Mutex
	path string
}

func NewStreakCache(path string) *StreakCache {
	return &StreakCache{path: path}
}

// Load returns the persisted grants younger than streakCacheTTL. Fail-safe by
// contract: a missing, unreadable or corrupt file yields an empty map (and at
// most a warning) — never an error, never a crash.
func (c *StreakCache) Load(now time.Time) map[string]StreakGrant {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loadLocked(now)
}

func (c *StreakCache) loadLocked(now time.Time) map[string]StreakGrant {
	out := make(map[string]StreakGrant)
	data, err := os.ReadFile(c.path)
	if err != nil {
		return out // first run or removed file: empty cache
	}
	var raw map[string]StreakGrant
	if err := json.Unmarshal(data, &raw); err != nil {
		slog.Warn("Streak cache is corrupt; ignoring it (streak pursuit falls back to pre-cache behavior)",
			"path", c.path, "error", err)
		return out
	}
	for login, g := range raw {
		if g.BroadcastID == "" || g.GrantedAt.IsZero() || now.Sub(g.GrantedAt) > streakCacheTTL {
			continue
		}
		out[login] = g
	}
	return out
}

// Record persists a grant for login. Empty broadcast IDs are skipped — an
// unidentified broadcast cannot be matched after a restart, so persisting it
// would be noise. Writes are atomic (temp+rename) and never called under the
// pool/watcher locks.
func (c *StreakCache) Record(login, broadcastID string, grantedAt time.Time) {
	if broadcastID == "" || login == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	grants := c.loadLocked(grantedAt)
	grants[login] = StreakGrant{BroadcastID: broadcastID, GrantedAt: grantedAt}

	data, err := json.MarshalIndent(grants, "", "  ")
	if err != nil {
		slog.Warn("Failed to encode streak cache", "error", err)
		return
	}
	if err := util.WriteFileAtomic(c.path, data, 0o644); err != nil {
		slog.Warn("Failed to write streak cache; grants will not survive a restart", "path", c.path, "error", err)
	}
}
