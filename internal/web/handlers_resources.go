package web

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/resources"
)

// ResourcesPath is the read-only endpoint feeding the dashboard resource
// mini-widgets (CPU / Memory / Network / Disk I/O). It only returns the sampler's
// last in-memory snapshot — it never samples /proc or cgroup itself, makes no
// external call, and mutates no state. Registered on the shared mux, it inherits
// the standard middleware chain (security headers, and Basic Auth when
// DASHBOARD_USERNAME/PASSWORD are set); GET is CSRF-safe. Its auth behavior is
// therefore identical to every other read endpoint — open when auth is disabled,
// 401 when configured — deliberately NOT the support bundle's stricter gate.
const ResourcesPath = "/api/resources"

// SetResourceSnapshotProvider wires the resource sampler's latest-snapshot reader
// into the dashboard. A nil provider is handled gracefully: the endpoint serves a
// typed all-unavailable snapshot (never a 404 or 500), so the widgets show N/A.
func (s *Server) SetResourceSnapshotProvider(fn func() resources.Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resourceSnapshot = fn
}

// handleAPIResources serves the latest resource snapshot as JSON. GET-only;
// never caches; returns a typed all-unavailable snapshot with 200 when no
// provider is wired (before mining starts) or a provider panics.
func (s *Server) handleAPIResources(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	s.mu.RLock()
	fn := s.resourceSnapshot
	s.mu.RUnlock()

	snap := resources.UnavailableSnapshot()
	if fn != nil {
		snap = safeResourceSnapshot(fn)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(snap)
}

// safeResourceSnapshot reads the provider, converting a provider panic into a
// typed all-unavailable snapshot (logged server-side only) so a broken sampler
// degrades to N/A instead of a 500 or a leaked stack trace.
func safeResourceSnapshot(fn func() resources.Snapshot) (snap resources.Snapshot) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("Resource snapshot provider panicked", "error", p)
			snap = resources.UnavailableSnapshot()
		}
	}()
	return fn()
}
