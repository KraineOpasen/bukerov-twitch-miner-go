package analytics

import (
	"log/slog"
	"strings"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

type Service struct {
	repo     Repository
	basePath string
}

func NewService(db *database.DB, basePath string) (*Service, error) {
	repo, err := NewSQLiteRepository(db, basePath)
	if err != nil {
		return nil, err
	}
	return &Service{
		repo:     repo,
		basePath: basePath,
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
}

func (s *Service) RecordAnnotation(streamer *models.Streamer, eventType, text string) {
	colors := map[string]string{
		"WATCH_STREAK":    "#45c1ff",
		"PREDICTION_MADE": "#ffe045",
		"WIN":             "#36b535",
		"LOSE":            "#ff4545",
	}

	color, ok := colors[eventType]
	if !ok {
		return
	}

	if err := s.repo.RecordAnnotation(streamer.Username, eventType, text, color); err != nil {
		slog.Error("Failed to record annotation", "streamer", streamer.Username, "error", err)
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
