package debug

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/logger"
)

const (
	defaultLogLines = 1000
	maxLogLines     = 2000

	// maxLogTailBytes is one total read budget for the retained log family;
	// it is generous enough for maxLogLines of typical slog output.
	maxLogTailBytes = 4 << 20
)

// SnapshotFunc assembles the current Snapshot; it is called on every
// GET /debug/snapshot request and must be safe for concurrent use.
type SnapshotFunc func() Snapshot

// Server is the localhost-only diagnostic HTTP server. It deliberately binds
// to 127.0.0.1 only - never a configurable host - so the internal state it
// exposes is unreachable from other machines.
type Server struct {
	port     int
	snapshot SnapshotFunc
	// logPath is the miner's canonical active log path; /debug/log also reads
	// its exact retained segments. Empty when
	// file logging is disabled.
	logPath string

	srv  *http.Server
	addr string
}

func NewServer(port int, snapshot SnapshotFunc, logPath string) *Server {
	return &Server{
		port:     port,
		snapshot: snapshot,
		logPath:  logPath,
	}
}

// Start binds 127.0.0.1:port and begins serving in a background goroutine.
// A bind failure (e.g. port already in use) is returned immediately rather
// than surfacing asynchronously.
func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/snapshot", s.handleSnapshot)
	mux.HandleFunc("/debug/log", s.handleLog)

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(s.port))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to bind debug server on %s: %w", addr, err)
	}

	s.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// /debug/log can stream up to maxLogTailBytes (~4MB); allow a slow
		// local reader to finish while still bounding a stuck write.
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	s.addr = listener.Addr().String()

	slog.Info("Debug server listening (localhost only)",
		"addr", s.addr,
		"snapshot", "http://"+s.addr+"/debug/snapshot",
		"log", "http://"+s.addr+"/debug/log",
	)

	go func() {
		if err := s.srv.Serve(listener); err != http.ErrServerClosed {
			slog.Error("Debug server error", "error", err)
		}
	}()

	return nil
}

// Addr returns the base URL the server is bound to (e.g.
// "http://127.0.0.1:5757"). Only valid after a successful Start.
func (s *Server) Addr() string {
	return "http://" + s.addr
}

func (s *Server) Stop() {
	if s.srv != nil {
		_ = s.srv.Close()
	}
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	data, err := json.MarshalIndent(s.snapshot(), "", "  ")
	if err != nil {
		http.Error(w, "failed to build snapshot: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(data)
	_, _ = w.Write([]byte("\n"))
}

func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.logPath == "" {
		http.Error(w, "file logging is disabled (logger.save = false), no log file to serve", http.StatusNotFound)
		return
	}

	lines := defaultLogLines
	if raw := r.URL.Query().Get("lines"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			http.Error(w, "invalid lines parameter", http.StatusBadRequest)
			return
		}
		lines = min(n, maxLogLines)
	}

	tail, err := tailFile(s.logPath, lines)
	if err != nil {
		http.Error(w, "failed to read log file: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(tail)
}

// tailFile returns the last n lines across the retained family at path, using
// maxLogTailBytes as one aggregate physical-read budget.
func tailFile(path string, n int) ([]byte, error) {
	return logger.TailLogFamily(path, n, maxLogTailBytes)
}
