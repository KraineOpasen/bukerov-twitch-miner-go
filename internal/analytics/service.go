package analytics

import (
	"errors"
	"fmt"
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

// annotationColors is the per-event-type chart marker palette persisted with
// every annotation (the chart falls back to it for types it has no theme
// token for). Shared by RecordAnnotation and the event-backed markers
// RecordPointEvent writes.
var annotationColors = map[string]string{
	"WATCH_STREAK":    "#45c1ff",
	"PREDICTION_MADE": "#ffe045",
	"WIN":             "#36b535",
	"LOSE":            "#ff4545",
	"RAID":            "#d9a25c",
}

// RecordPoints records a balance-timeline sample from the streamer's CURRENT
// balance. It is the timeline-only path: points-spent frames (reason "Spent")
// and points-earned frames that cannot be admitted to the exact ledger (no
// event identity, or an amount that is not an exact integer). Such a sample
// is never an exact earning: the Statistics page estimates it from balance
// deltas and labels it so. Accepted points-earned events go through
// RecordPointEvent instead.
func (s *Service) RecordPoints(streamer *models.Streamer, eventType string) {
	eventType = timelineReason(eventType)
	if err := s.repo.RecordPoints(streamer.GetUsername(), streamer.GetChannelPoints(), eventType); err != nil {
		slog.Error("Failed to record points", "streamer", streamer.GetUsername(), "error", err)
	}
	s.maybePrune()
}

// RecordPointEvent persists one accepted points-earned event as an exact
// earning fact: the ledger row carrying the event-local amount and balance,
// the balance-timeline sample it produces, and — for WATCH_STREAK and RAID —
// the chart annotation built from the SAME event-local amount, all in one
// transaction. Nothing about the earning is re-read from the mutable Streamer:
// the timeline sample is written at the frame's own balance and falls back to
// the streamer's current balance only when the frame carried no balance at all
// (a display value for the chart, never an accounting input). ev.Timestamp
// defaults to the service clock. Returns whether the event was newly recorded
// (false for an exact re-delivery, which writes nothing) and the write error,
// which is also logged — a failed analytics write never disrupts mining.
func (s *Service) RecordPointEvent(streamer *models.Streamer, ev PointEvent) (bool, error) {
	login := streamer.GetUsername()
	if ev.EventID == "" {
		err := errors.New("analytics: point event has no identity")
		slog.Error("Refusing to record point event", "streamer", login, "reason", ev.ReasonCode, "error", err)
		return false, err
	}
	if ev.Timestamp == 0 {
		ev.Timestamp = s.now().UnixMilli()
	}
	timeline := ev.BalanceAfter
	if !ev.BalanceKnown {
		timeline = streamer.GetChannelPoints()
	}

	recorded, err := s.repo.RecordPointEvent(login, ev, timeline, pointEventAnnotation(ev))
	switch {
	case errors.Is(err, ErrStreamerDeleted):
		slog.Debug("Dropping point event for a deleted streamer", "streamer", login, "reason", ev.ReasonCode)
		return false, err
	case err != nil:
		slog.Error("Failed to record point event", "streamer", login, "reason", ev.ReasonCode, "error", err)
		return false, err
	case !recorded:
		slog.Info("Duplicate point event ignored", "streamer", login, "reason", ev.ReasonCode)
	}
	s.maybePrune()
	return recorded, nil
}

// pointEventAnnotation builds the chart marker for the reasons that carry one
// (WATCH_STREAK, RAID) from the event-local amount; nil for every other
// reason. The marker text is a display fact — it is never parsed back into an
// accounting number.
func pointEventAnnotation(ev PointEvent) *PointEventAnnotation {
	var label string
	switch ev.ReasonCode {
	case "WATCH_STREAK":
		label = "Watch Streak"
	case "RAID":
		label = "Raid"
	default:
		return nil
	}
	return &PointEventAnnotation{
		EventType: ev.ReasonCode,
		Text:      fmt.Sprintf("+%d - %s", ev.TotalPoints, label),
		Color:     annotationColors[ev.ReasonCode],
	}
}

func (s *Service) RecordAnnotation(streamer *models.Streamer, eventType, text string) {
	color, ok := annotationColors[eventType]
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
