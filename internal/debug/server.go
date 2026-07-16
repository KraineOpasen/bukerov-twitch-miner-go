package debug

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
)

const (
	defaultLogLines = 1000
	maxLogLines     = 2000

	// maxLogTailBytes bounds how much of the log file is read from the end
	// when serving /debug/log; generous enough for maxLogLines of typical
	// slog output without ever loading a multi-day log wholesale.
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
	// logPath is the miner's log file, served by /debug/log. Empty when
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

// tailFile returns the last n lines of the file at path, reading at most
// maxLogTailBytes from its end.
func tailFile(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}

	offset := info.Size() - maxLogTailBytes
	truncated := offset > 0
	if !truncated {
		offset = 0
	}

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}

	if truncated {
		// The first line is almost certainly cut mid-way; drop it.
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}

	data = bytes.TrimRight(data, "\n")
	if len(data) == 0 {
		return nil, nil
	}

	// Walk backwards to the start of the n-th line from the end.
	pos := len(data)
	for i := 0; i < n; i++ {
		next := bytes.LastIndexByte(data[:pos], '\n')
		if next < 0 {
			pos = 0
			break
		}
		pos = next
	}
	if pos > 0 {
		pos++ // skip the newline itself
	}

	out := data[pos:]
	return append(out, '\n'), nil
}
