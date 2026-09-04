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
	// logTailLines is how many trailing log lines the viewer LISTS, i.e. the
	// cap on dashboard-visible lines (see visibleLogLines).
	logTailLines = 500
	// logScanLines is how many trailing RAW lines are read and classified to
	// find those. It is deliberately much larger than logTailLines: file
	// logging defaults to DEBUG (internal/config/config.go), so the retained
	// tail is dominated by transport chatter the Logs page suppresses. If the
	// raw window were the same size as the visible cap, a busy miner could
	// fill all of it with suppressed lines and the page would show a handful
	// of entries — or, at the limit, an empty state on a perfectly healthy
	// miner.
	logScanLines = 5000
	// logTailMaxBytes is one total read budget for the retained log family,
	// and the hard ceiling on the scan above.
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

// readLogTail reads the last logScanLines lines of the retained log family and
// classifies each one. The second return is false when file logging is off
// (the file doesn't exist), so the page can explain how to enable it rather
// than showing a bare empty state.
//
// It is a faithful reader: every non-blank line of the tail comes back,
// carrying its classification. Deciding which lines the human Logs page
// actually lists is the render seam's job (LogLineView.DashboardVisible,
// applied in the logs_lines partial), so nothing here censors the retained
// evidence.
func (s *Server) readLogTail() ([]LogLineView, bool) {
	path := s.logPath
	if path == "" {
		path = stableLogPath(s.basePath, s.username)
	}
	if path == "" {
		return nil, false
	}
	raw, err := logger.TailLogFamily(path, logScanLines, logTailMaxBytes)
	if err != nil {
		// Missing file => file logging disabled (or nothing written yet).
		if os.IsNotExist(err) {
			return nil, false
		}
		slog.Error("Failed to read retained log family", "path", path, "error", err)
		return nil, true
	}

	all := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(all) > logScanLines {
		all = all[len(all)-logScanLines:]
	}

	views := make([]LogLineView, 0, len(all))
	for _, line := range all {
		if strings.TrimSpace(line) == "" {
			continue
		}
		p := classifyLogLine(line)
		views = append(views, LogLineView{
			Class:            p.Class,
			Emoji:            p.Emoji,
			Text:             line,
			Level:            p.Level,
			Subsystem:        p.Subsystem,
			Reconnect:        p.Reconnect,
			DashboardVisible: p.DashboardVisible,
		})
	}
	return views, true
}

// tailLogFile returns a complete-line family tail under one byte budget.
func tailLogFile(path string, maxBytes int64) ([]byte, error) {
	return logger.TailLogFamily(path, int(^uint(0)>>1), maxBytes)
}
