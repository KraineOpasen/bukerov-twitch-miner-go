package logger

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/PatrickWalther/twitch-miner-go/internal/config"
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

	var writers []io.Writer
	writers = append(writers, os.Stdout)

	l := &Logger{}

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
		writers = append(writers, file)
	}

	multiWriter := io.MultiWriter(writers...)

	level := consoleLevel
	if settings.Save && fileLevel < consoleLevel {
		level = fileLevel
	}

	handler := slog.NewTextHandler(multiWriter, &slog.HandlerOptions{
		Level: level,
	})

	l.handler = handler
	slog.SetDefault(slog.New(handler))

	return l, nil
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
