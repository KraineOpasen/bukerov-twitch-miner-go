package web

import (
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/logger"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version"
)

const (
	// logTailLines is how many trailing log lines the viewer shows.
	logTailLines = 500
	// logTailMaxBytes is one total read budget for the retained log family.
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
	authEnabled := s.dashboard.AuthEnabled()
	s.mu.RUnlock()

	lines, enabled := s.readLogTail()
	data := LogsPageData{
		Username:               s.username,
		RefreshMinutes:         refresh,
		Version:                version.Version,
		DiscordEnabled:         discordEnabled,
		DebugURL:               debugURL,
		SupportBundleAvailable: authEnabled,
		Lines:                  lines,
		FileLogging:            enabled,
	}
	s.renderPage(w, r, "logs.html", data)
}

// handleAPILogs renders just the log-lines partial for htmx auto-refresh.
func (s *Server) handleAPILogs(w http.ResponseWriter, r *http.Request) {
	lines, enabled := s.readLogTail()
	s.renderPartial(w, r, "logs_lines", LogsLinesData{Lines: lines, FileLogging: enabled})
}

// readLogTail reads the last logTailLines lines of the retained log family and
// classifies each by level for coloring. The second return is false when file
// logging is off (the file doesn't exist), so the page can explain how to
// enable it rather than showing a bare empty state.
func (s *Server) readLogTail() ([]LogLineView, bool) {
	path := s.logPath
	if path == "" {
		path = stableLogPath(s.basePath, s.username)
	}
	if path == "" {
		return nil, false
	}
	raw, err := logger.TailLogFamily(path, logTailLines, logTailMaxBytes)
	if err != nil {
		// Missing file => file logging disabled (or nothing written yet).
		if os.IsNotExist(err) {
			return nil, false
		}
		slog.Error("Failed to read retained log family", "path", path, "error", err)
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
		p := classifyLogLine(line)
		views = append(views, LogLineView{Class: p.Class, Emoji: p.Emoji, Text: line})
	}
	return views, true
}

// tailLogFile returns a complete-line family tail under one byte budget.
func tailLogFile(path string, maxBytes int64) ([]byte, error) {
	return logger.TailLogFamily(path, int(^uint(0)>>1), maxBytes)
}
