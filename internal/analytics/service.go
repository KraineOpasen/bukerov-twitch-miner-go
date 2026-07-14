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
	if err := s.repo.RecordPoints(streamer.Username, streamer.GetChannelPoints(), eventType); err != nil {
		slog.Error("Failed to record points", "streamer", streamer.Username, "error", err)
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

	if err := s.repo.RecordAnnotation(streamer.Username, eventType, text, color); err != nil {
		slog.Error("Failed to record annotation", "streamer", streamer.Username, "error", err)
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
