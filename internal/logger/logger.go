package logger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
)

type Logger struct {
	file    *rotatingWriter
	console *consoleWriter
	handler slog.Handler
}

// LogFilePath returns the canonical active path Setup writes to for storageKey
// when file logging (settings.Save) is enabled. Exposed so other components
// can locate the same retained family without following a mutable username.
// It returns an empty path for a key that cannot be one safe file component.
func LogFilePath(storageKey string) string {
	if validateStorageKey(storageKey) != nil {
		return ""
	}
	return filepath.Join("logs", storageKey+".log")
}

func Setup(storageKey string, settings config.LoggerSettings) (*Logger, error) {
	return setupWithOptions(storageKey, settings, rotationOptions{})
}

func setupWithOptions(storageKey string, settings config.LoggerSettings, opts rotationOptions) (*Logger, error) {
	opts = opts.withDefaults()
	consoleLevel := parseLevel(settings.ConsoleLevel)
	fileLevel := parseLevel(settings.FileLevel)

	l := &Logger{}

	// Set up the optional file handler first: it can fail (mkdir/open), and the
	// console handler below starts a background writer goroutine, so creating the
	// console last means an early return here never leaks that goroutine.
	var fileHandler slog.Handler
	if settings.Save {
		if err := validateStorageKey(storageKey); err != nil {
			return nil, err
		}
		if err := os.MkdirAll("logs", 0755); err != nil {
			return nil, err
		}

		logPath := LogFilePath(storageKey)
		segmentStarted := opts.now()
		fileMode := os.FileMode(0o644)
		if settings.AutoClear {
			if err := pruneCompletedSegments(logPath, maxCompletedSegments, opts.ops); err != nil {
				return nil, err
			}
			var err error
			segmentStarted, fileMode, err = inspectActiveSegment(logPath, segmentStarted, opts.ops)
			if err != nil {
				return nil, err
			}
		}

		file, err := opts.ops.openFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, fileMode)
		if err != nil {
			return nil, err
		}
		l.file = newRotatingWriter(file, logPath, fileMode, segmentStarted, settings.AutoClear, opts)

		fileHandler = slog.NewTextHandler(l.file, &slog.HandlerOptions{
			Level: fileLevel,
		})
	}

	// The console handler colorizes each line for stdout (what Docker/Portainer
	// display), keyed off the record's level and msg category. Coloring is driven
	// solely by the explicit settings.Colored toggle — never by TTY autodetection,
	// because the primary consumer is a web log viewer (Portainer/Dozzle) reading
	// the container's stdout over the Docker API without a TTY. The file handler
	// deliberately stays a plain slog.TextHandler so the on-disk log — served
	// verbatim by the /debug/log endpoint — contains no ANSI escape codes.
	console := newConsoleHandler(os.Stdout, consoleLevel, settings.Colored)
	l.console = console.cw

	handlers := []slog.Handler{console}
	if fileHandler != nil {
		handlers = append(handlers, fileHandler)
	}

	handler := fanoutHandler{handlers: handlers}
	l.handler = handler
	slog.SetDefault(slog.New(handler))

	return l, nil
}

func validateStorageKey(storageKey string) error {
	if storageKey == "" || storageKey == "." || storageKey == ".." ||
		!filepath.IsLocal(storageKey) || filepath.Base(storageKey) != storageKey ||
		filepath.VolumeName(storageKey) != "" || strings.ContainsAny(storageKey, `<>:"/\|?*`) ||
		strings.HasSuffix(storageKey, ".") || strings.HasSuffix(storageKey, " ") {
		return fmt.Errorf("invalid logger storage key %q: must be one cross-platform-safe path component", storageKey)
	}
	for _, char := range storageKey {
		if char < ' ' {
			return fmt.Errorf("invalid logger storage key %q: contains a control character", storageKey)
		}
	}
	windowsBase := strings.ToUpper(strings.SplitN(storageKey, ".", 2)[0])
	windowsDevice := windowsBase == "CON" || windowsBase == "PRN" || windowsBase == "AUX" || windowsBase == "NUL" ||
		windowsBase == "CONIN$" || windowsBase == "CONOUT$"
	if strings.HasPrefix(windowsBase, "COM") || strings.HasPrefix(windowsBase, "LPT") {
		suffix := windowsBase[3:]
		windowsDevice = windowsDevice || suffix == "1" || suffix == "2" || suffix == "3" || suffix == "4" || suffix == "5" ||
			suffix == "6" || suffix == "7" || suffix == "8" || suffix == "9" || suffix == "¹" || suffix == "²" || suffix == "³"
	}
	if windowsDevice {
		return fmt.Errorf("invalid logger storage key %q: reserved on Windows", storageKey)
	}
	return nil
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
	// Every enabled handler must get the record even if an earlier one fails —
	// a stdout write error must not cost the on-disk log its copy of the line.
	// Errors are collected and joined rather than short-circuiting.
	var errs []error
	for _, hh := range h.handlers {
		if !hh.Enabled(ctx, r.Level) {
			continue
		}
		// Clone because handlers may retain or mutate the record.
		if err := hh.Handle(ctx, r.Clone()); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
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
	// Flush any lines still queued for stdout before dropping the writer, then
	// close the log file.
	if l.console != nil {
		l.console.Close()
	}
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
