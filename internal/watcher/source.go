package watcher

import "github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"

// Watch-slot origins. Every channel that occupies one of the (at most
// constants.MaxSimultaneousStreams) Twitch watch slots comes from one of these.
const (
	// OriginConfigured is a channel from the user's fixed streamer list, owned
	// directly by the watcher.
	OriginConfigured = "configured"
	// OriginDiscovery is a channel proposed by directory discovery.
	OriginDiscovery = "discovery"
)

// Candidate is a channel a source proposes for a watch slot. The broker (the
// MinuteWatcher acting as the single owner of the watch slots), not the
// source, decides whether the candidate is actually watched. A source
// proposing a candidate never sends minute-watched itself.
type Candidate struct {
	// Streamer is the (possibly ephemeral) streamer object to watch. It must
	// already be brought online (spade URL + stream payload populated) so the
	// broker can report a watched minute for it.
	Streamer *models.Streamer
	// Origin is one of the Origin* constants, used for the explainable slot
	// snapshot and to keep source-specific per-send bookkeeping straight.
	Origin string
	// Reason is a short source-supplied note (e.g. why discovery picked this
	// channel) surfaced in the snapshot when the candidate is waiting.
	Reason string
}

// CandidateSource supplies extra watch candidates each tick, on top of the
// configured streamer list the broker owns directly. It exists so directory
// discovery (and any future source) competes for the same two watch slots
// instead of running an independent, un-arbitrated third watch loop.
//
// WatchCandidates is called from the broker's loop goroutine while the source
// mutates its own state from another goroutine, so it must return an
// independent snapshot and must not block on the network or hold a lock across
// external calls.
type CandidateSource interface {
	// SourceName identifies the source in logs and snapshots (e.g. "discovery").
	SourceName() string
	// WatchCandidates returns the source's proposed channels in the source's
	// own priority order (most-wanted first). The broker fills at most
	// constants.MaxSimultaneousStreams slots total across the configured list
	// and every source combined.
	WatchCandidates() []Candidate
}
