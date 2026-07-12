package logger

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
)

type Logger struct {
	file    *os.File
	handler slog.Handler
}

// LogFilePath returns the path of the log file Setup writes to for username
// when file logging (settings.Save) is enabled. Exposed so other components
// (the debug endpoint's log tail) can locate the same file.
func LogFilePath(username string) string {
	return filepath.Join("logs", username+".log")
}

func Setup(username string, settings config.LoggerSettings) (*Logger, error) {
	consoleLevel := parseLevel(settings.ConsoleLevel)
	fileLevel := parseLevel(settings.FileLevel)

	l := &Logger{}

	// The console handler colorizes each line for stdout (what Docker/Portainer
	// display), keyed off the record's level and msg category. The file handler
	// deliberately stays a plain slog.TextHandler so the on-disk log — served
	// verbatim by the /debug/log endpoint — contains no ANSI escape codes.
	handlers := []slog.Handler{newConsoleHandler(os.Stdout, consoleLevel)}

	if settings.Save {
		if err := os.MkdirAll("logs", 0755); err != nil {
			return nil, err
		}

		logPath := LogFilePath(username)

		if settings.AutoClear {
			clearOldLogs(logPath, 7)
		}

		file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return nil, err
		}
		l.file = file

		handlers = append(handlers, slog.NewTextHandler(file, &slog.HandlerOptions{
			Level: fileLevel,
		}))
	}

	handler := fanoutHandler{handlers: handlers}
	l.handler = handler
	slog.SetDefault(slog.New(handler))

	return l, nil
}

// fanoutHandler dispatches every record to each underlying handler, each of
// which enforces its own level. This is what lets the console (colored, INFO by
// default) and the file (plain, DEBUG by default) diverge in both level and
// formatting while sharing a single slog.Logger.
type fanoutHandler struct {
	handlers []slog.Handler
}

func (h fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, hh := range h.handlers {
		if hh.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h fanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, hh := range h.handlers {
		if !hh.Enabled(ctx, r.Level) {
			continue
		}
		// Clone because handlers may retain or mutate the record.
		if err := hh.Handle(ctx, r.Clone()); err != nil {
			return err
		}
	}
	return nil
}

func (h fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, len(h.handlers))
	for i, hh := range h.handlers {
		next[i] = hh.WithAttrs(attrs)
	}
	return fanoutHandler{handlers: next}
}

func (h fanoutHandler) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, len(h.handlers))
	for i, hh := range h.handlers {
		next[i] = hh.WithGroup(name)
	}
	return fanoutHandler{handlers: next}
}

func (l *Logger) Close() {
	if l.file != nil {
		_ = l.file.Close()
	}
}

func parseLevel(level string) slog.Level {
	switch level {
	case "DEBUG":
		return slog.LevelDebug
	case "INFO":
		return slog.LevelInfo
	case "WARN", "WARNING":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func clearOldLogs(logPath string, daysToKeep int) {
	info, err := os.Stat(logPath)
	if err != nil {
		return
	}

	if time.Since(info.ModTime()) > time.Duration(daysToKeep)*24*time.Hour {
		_ = os.Remove(logPath)
	}
}
