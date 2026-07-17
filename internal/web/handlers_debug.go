package web

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/debug"
)

// DebugSnapshotPath is the dashboard route serving the debug snapshot. The
// Logs page button links here (a relative URL) instead of the localhost-only
// debug server, which a remote browser can never reach: "localhost" in a
// browser is the viewer's machine, not the container. The route reuses the
// exact in-process snapshot builder the 127.0.0.1-only debug server uses —
// no self-HTTP call, no second snapshot implementation — and, living on the
// main mux, inherits the full dashboard middleware chain (security headers,
// Basic Auth when configured, same-origin protection).
const DebugSnapshotPath = "/api/debug/snapshot"

// SetDebugSnapshotProvider wires the miner's in-process snapshot builder into
// the dashboard. It is only called when Debug.Enabled is true; while unset,
// handleAPIDebugSnapshot fails closed with 404.
func (s *Server) SetDebugSnapshotProvider(fn func() debug.Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.debugSnapshot = fn
}

// handleAPIDebugSnapshot serves the debug snapshot on the main dashboard.
// GET-only; 404 while debug mode is off (no provider wired); never caches.
func (s *Server) handleAPIDebugSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	s.mu.RLock()
	fn := s.debugSnapshot
	s.mu.RUnlock()
	if fn == nil {
		http.NotFound(w, r)
		return
	}

	data, err := marshalDebugSnapshot(fn)
	if err != nil {
		// Log the detail server-side only; the client gets a plain 5xx with
		// no internals.
		slog.Error("Failed to build debug snapshot for dashboard", "error", err)
		writeInternalError(w, "failed to build snapshot")
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
	_, _ = w.Write([]byte("\n"))
}

// marshalDebugSnapshot builds and renders the snapshot, converting a provider
// panic into an error so the handler answers with a clean 500 instead of a
// dropped connection.
func marshalDebugSnapshot(fn func() debug.Snapshot) (data []byte, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("snapshot provider panicked: %v", p)
		}
	}()
	return json.MarshalIndent(fn(), "", "  ")
}
