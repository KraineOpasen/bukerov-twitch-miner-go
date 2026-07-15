package web

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/logger"
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

	s.mu.RLock()
	refresh := s.refresh
	discordEnabled := s.discordEnabled
	debugURL := s.debugURL
	s.mu.RUnlock()

	lines, enabled := s.readLogTail()
	data := LogsPageData{
		Username:       s.username,
		RefreshMinutes: refresh,
		Version:        version.Version,
		DiscordEnabled: discordEnabled,
		DebugURL:       debugURL,
		Lines:          lines,
		FileLogging:    enabled,
	}
	s.renderPage(w, r, "logs.html", data)
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
		views = append(views, LogLineView{Class: logLineClass(line), Text: line})
	}
	return views, true
}

// logLineClass maps a slog text line to a color class: ERROR (and offline
// transitions) red, WARN amber, everything else neutral.
func logLineClass(line string) string {
	if strings.Contains(line, "went offline") {
		return "log-error"
	}
	switch logLevelOf(line) {
	case "ERROR":
		return "log-error"
	case "WARN":
		return "log-warn"
	default:
		return "log-info"
	}
}

// logLevelOf extracts the slog TextHandler level token (level=INFO) from a line.
func logLevelOf(line string) string {
	const key = "level="
	i := strings.Index(line, key)
	if i < 0 {
		return ""
	}
	rest := line[i+len(key):]
	if j := strings.IndexByte(rest, ' '); j >= 0 {
		rest = rest[:j]
	}
	return rest
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
