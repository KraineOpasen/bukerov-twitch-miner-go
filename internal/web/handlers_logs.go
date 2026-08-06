package web

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/logger"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/supportbundle"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version"
)

const (
	// logTailLines is how many trailing log lines the viewer shows.
	logTailLines = 500
	// logTailMaxBytes bounds how much of the file's tail is read (memory guard).
	logTailMaxBytes = 2 << 20 // 2 MiB
)

// handleLogsPage renders the full Logs page: a live tail of the miner's on-disk
// log file, colored by level. Replaces the old sidebar fallback that jumped to
// Settings#logger.
func (s *Server) handleLogsPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/logs" {
		http.NotFound(w, r)
		return
	}
	s.renderPage(w, r, "logs.html", s.buildLogsPageData())
}

// buildLogsPageData assembles the LogsPageData shared verbatim (task S5-5)
// by the canonical /logs route (handleLogsPage above) and its /system/logs
// alias (handleSystemLogsPage, handlers_system.go) — the exact same log
// tail feeding the exact same "logs.html" template, extracted here so the
// two routes can never drift apart.
func (s *Server) buildLogsPageData() LogsPageData {
	s.mu.RLock()
	refresh := s.refresh
	discordEnabled := s.discordEnabled
	debugURL := s.debugURL
	s.mu.RUnlock()

	lines, enabled := s.readLogTail()
	return LogsPageData{
		Username:       s.username,
		RefreshMinutes: refresh,
		Version:        version.Version,
		DiscordEnabled: discordEnabled,
		DebugURL:       debugURL,
		Lines:          lines,
		FileLogging:    enabled,
	}
}

// handleAPILogs renders just the log-lines partial for htmx auto-refresh.
func (s *Server) handleAPILogs(w http.ResponseWriter, r *http.Request) {
	lines, enabled := s.readLogTail()
	s.renderPartial(w, r, "logs_lines", LogsLinesData{Lines: lines, FileLogging: enabled})
}

// readLogTail reads the last logTailLines lines of the miner's log file and
// classifies each by level for coloring. The second return is false when file
// logging is off (the file doesn't exist), so the page can explain how to
// enable it rather than showing a bare empty state.
func (s *Server) readLogTail() ([]LogLineView, bool) {
	path := logger.LogFilePath(s.username)
	raw, err := tailLogFile(path, logTailMaxBytes)
	if err != nil {
		// Missing file => file logging disabled (or nothing written yet).
		if os.IsNotExist(err) {
			return nil, false
		}
		return nil, true
	}

	all := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(all) > logTailLines {
		all = all[len(all)-logTailLines:]
	}

	views := make([]LogLineView, 0, len(all))
	for _, line := range all {
		if strings.TrimSpace(line) == "" {
			continue
		}
		// Classification reads the RAW line (level=/msg=/reason=/result=
		// tokens, an emoji prefix) so a later redaction can never skew which
		// bucket a line lands in; only the rendered Text crosses the
		// supportbundle.Redact boundary (task S5-5 privacy fix — the single
		// seam covering /logs, /api/logs, and /system/logs). Redact leaves a
		// benign line unchanged (aside from its 512-rune cap) and replaces
		// an entire sensitive-shaped line with "[REDACTED]" — never a
		// partial redaction.
		p := classifyLogLine(line)
		views = append(views, LogLineView{Class: p.Class, Emoji: p.Emoji, Text: supportbundle.Redact(line)})
	}
	return views, true
}

// tailLogFile returns the last maxBytes of the file at path (aligned to the next
// line boundary so the first returned line isn't truncated mid-way).
func tailLogFile(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}

	if info.Size() <= maxBytes {
		return io.ReadAll(f)
	}

	if _, err := f.Seek(info.Size()-maxBytes, io.SeekStart); err != nil {
		return nil, err
	}
	buf := bufio.NewReader(f)
	// Drop the partial first line so the tail starts at a clean boundary.
	if _, err := buf.ReadBytes('\n'); err != nil && err != io.EOF {
		return nil, err
	}
	var out bytes.Buffer
	if _, err := io.Copy(&out, buf); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
