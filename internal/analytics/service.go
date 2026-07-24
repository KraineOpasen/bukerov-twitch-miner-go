package analytics

import (
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// pruneInterval throttles retention sweeps so history pruning runs at most once
// per interval even though it is triggered opportunistically from the (frequent)
// points-recording path. This keeps cleanup periodic without a dedicated polling
// goroutine.
const pruneInterval = time.Hour

type Service struct {
	repo     Repository
	basePath string

	// retentionDays bounds how long history is kept; 0 disables pruning.
	retentionDays int

	// now is injectable so tests can drive the prune throttle deterministically.
	now func() time.Time

	mu          sync.Mutex
	lastPruneAt time.Time
}

func NewService(db *database.DB, basePath string, retentionDays int) (*Service, error) {
	repo, err := NewSQLiteRepository(db, basePath)
	if err != nil {
		return nil, err
	}
	return &Service{
		repo:          repo,
		basePath:      basePath,
		retentionDays: retentionDays,
		now:           time.Now,
	}, nil
}

func (s *Service) Repository() Repository {
	return s.repo
}

func (s *Service) BasePath() string {
	return s.basePath
}

func (s *Service) RecordPoints(streamer *models.Streamer, eventType string) {
	eventType = strings.ReplaceAll(eventType, "_", " ")
	if err := s.repo.RecordPoints(streamer.GetUsername(), streamer.GetChannelPoints(), eventType); err != nil {
		slog.Error("Failed to record points", "streamer", streamer.GetUsername(), "error", err)
	}
	s.maybePrune()
}

func (s *Service) RecordAnnotation(streamer *models.Streamer, eventType, text string) {
	colors := map[string]string{
		"WATCH_STREAK":    "#45c1ff",
		"PREDICTION_MADE": "#ffe045",
		"WIN":             "#36b535",
		"LOSE":            "#ff4545",
		"RAID":            "#d9a25c",
	}

	color, ok := colors[eventType]
	if !ok {
		return
	}

	if err := s.repo.RecordAnnotation(streamer.GetUsername(), eventType, text, color); err != nil {
		slog.Error("Failed to record annotation", "streamer", streamer.GetUsername(), "error", err)
	}
}

// maybePrune runs a retention sweep at most once per pruneInterval. It is called
// from RecordPoints (the frequent write path) so cleanup happens periodically
// without a separate polling loop; the throttle keeps it off the hot path. A
// no-op when retention is disabled (retentionDays <= 0).
func (s *Service) maybePrune() {
	if s.retentionDays <= 0 {
		return
	}

	now := s.now()
	s.mu.Lock()
	if !s.lastPruneAt.IsZero() && now.Sub(s.lastPruneAt) < pruneInterval {
		s.mu.Unlock()
		return
	}
	s.lastPruneAt = now
	s.mu.Unlock()

	cutoff := now.Add(-time.Duration(s.retentionDays) * 24 * time.Hour)
	deleted, err := s.repo.PruneBefore(cutoff)
	if err != nil {
		slog.Error("Failed to prune analytics history", "error", err)
		return
	}
	if deleted > 0 {
		slog.Info("Pruned old analytics history", "rows", deleted, "olderThanDays", s.retentionDays)
	}
}

// RecordBet persists a resolved prediction bet for ROI analytics. Errors are
// logged rather than propagated: a failed analytics write must never disrupt the
// betting/pubsub path that produced the result.
func (s *Service) RecordBet(b BetRecord) {
	if err := s.repo.RecordBet(b); err != nil {
		slog.Error("Failed to record prediction bet", "streamer", b.Streamer, "event", b.EventID, "error", err)
	}
}

// RenameStreamer forwards a config-driven login rename (BKM-006) to the
// repository, preserving the analytics history's internal streamer row — and
// everything keyed by it: points, annotations, chat messages, prediction bets
// — under the SAME identity instead of splitting it across two rows. Names
// are lowercased to match how every other write path here already stores
// them (streamer.Manager always works with lowercase logins). The error is
// returned, not swallowed, so the caller can log a privacy-safe conflict
// without silently losing history — but it is never treated as fatal to the
// settings apply that triggered it.
func (s *Service) RenameStreamer(oldName, newName string) error {
	return s.repo.RenameStreamer(strings.ToLower(oldName), strings.ToLower(newName))
}

func (s *Service) RecordChatMessage(streamer string, username, displayName, message, emotes, badges, color string) error {
	msg := ChatMessage{
		Username:    username,
		DisplayName: displayName,
		Message:     message,
		Emotes:      emotes,
		Badges:      badges,
		Color:       color,
	}
	return s.repo.RecordChatMessage(streamer, msg)
}

func (s *Service) Close() error {
	if s.repo != nil {
		return s.repo.Close()
	}
	return nil
}
