package health

import (
	"sync"
	"time"
)

// AvoidEntry is one temporarily excluded channel, exposed for the debug
// snapshot.
type AvoidEntry struct {
	Login  string    `json:"login"`
	Until  time.Time `json:"until"`
	Reason string    `json:"reason"`
}

// AvoidList is the progress watchdog's channel-switch lever: instead of
// imperatively overriding the slot broker, the watchdog marks a channel as
// temporarily avoided and the broker (and directory discovery) simply stop
// selecting it until the entry expires — the broker keeps full authority over
// slot allocation. Entries are self-expiring; nothing needs to clean them up.
// Safe for concurrent use.
type AvoidList struct {
	mu      sync.Mutex
	entries map[string]AvoidEntry
	now     func() time.Time
}

func NewAvoidList() *AvoidList {
	return &AvoidList{entries: make(map[string]AvoidEntry), now: time.Now}
}

// Avoid excludes the channel from watch selection until the given time.
// Re-avoiding an already-avoided channel extends (never shortens) the window.
func (a *AvoidList) Avoid(login string, until time.Time, reason string) {
	if login == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if prev, ok := a.entries[login]; ok && prev.Until.After(until) {
		return
	}
	a.entries[login] = AvoidEntry{Login: login, Until: until, Reason: reason}
}

// Clear removes the channel's avoid entry (e.g. its drop progressed again).
func (a *AvoidList) Clear(login string) {
	a.mu.Lock()
	delete(a.entries, login)
	a.mu.Unlock()
}

// IsAvoided reports whether the channel is currently excluded. Expired
// entries are dropped lazily. Satisfies watcher.AvoidChecker and the
// discovery subsystem's equivalent.
func (a *AvoidList) IsAvoided(login string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.entries[login]
	if !ok {
		return false
	}
	if a.now().After(e.Until) {
		delete(a.entries, login)
		return false
	}
	return true
}

// Entries returns the currently active avoid entries (expired ones pruned),
// for the debug snapshot.
func (a *AvoidList) Entries() []AvoidEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	out := make([]AvoidEntry, 0, len(a.entries))
	for login, e := range a.entries {
		if now.After(e.Until) {
			delete(a.entries, login)
			continue
		}
		out = append(out, e)
	}
	return out
}
