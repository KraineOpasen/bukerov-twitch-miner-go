package models

import (
	"encoding/base64"
	"encoding/json"
	"time"
)

// PlaybackSessionSnapshot is an immutable, caller-owned copy of the coherent
// watch session a single minute-watched beacon depends on: the spade endpoint,
// the beacon payload, and the broadcast they belong to, all read together under
// one lock so they cannot come from different observations. Generation is the
// playback-session generation captured at the same instant; the sender re-checks
// it just before the beacon (see Stream.SessionGeneration) and suppresses the
// send if the session changed underneath it, so an old payload can never be
// posted to a new spade URL.
//
// The zero value means the session is unknown/uninitialised (Generation == 0);
// callers must treat a zero-generation snapshot as "not brought online yet".
type PlaybackSessionSnapshot struct {
	Generation  uint64
	BroadcastID string
	SpadeURL    string

	// payload is the beacon events, copied out of the Stream. It is unexported so
	// callers go through EncodePayload and never mutate a shared event map (the
	// producer, Stream.SetPayload, replaces it wholesale and never mutates in
	// place, so a slice copy is a sufficient, race-free hand-off).
	payload []MinuteWatchedEvent
}

// Initialized reports whether the snapshot represents a real brought-online
// session (a non-zero generation). A zero-value snapshot is "unknown".
func (p PlaybackSessionSnapshot) Initialized() bool { return p.Generation != 0 }

// HasSpadeURL reports whether a spade endpoint was discovered for this session.
func (p PlaybackSessionSnapshot) HasSpadeURL() bool { return p.SpadeURL != "" }

// HasPayload reports whether a beacon payload was built for this session.
func (p PlaybackSessionSnapshot) HasPayload() bool { return len(p.payload) > 0 }

// EncodePayload marshals the captured beacon payload to the base64 body the spade
// endpoint expects. It reads only the snapshot's own copy — never the live
// Stream — so it stays coherent with SpadeURL/BroadcastID for the whole send.
func (p PlaybackSessionSnapshot) EncodePayload() (string, error) {
	data, err := json.Marshal(p.payload)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// SessionSnapshot returns a coherent, caller-owned copy of the current playback
// session: spade URL, beacon payload, broadcast ID, and the generation they were
// read at — all under one lock so a send cannot mix fields from two different
// observations. The payload slice is copied so the caller owns it.
func (s *Stream) SessionSnapshot() PlaybackSessionSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var payload []MinuteWatchedEvent
	if len(s.payload) > 0 {
		payload = make([]MinuteWatchedEvent, len(s.payload))
		copy(payload, s.payload)
	}
	return PlaybackSessionSnapshot{
		Generation:  s.sessionGen,
		BroadcastID: s.BroadcastID,
		SpadeURL:    s.spadeURL,
		payload:     payload,
	}
}

// SessionGeneration returns the current playback-session generation. The minute
// sender captures it (via SessionSnapshot) at the start of a send and re-reads it
// just before the beacon; a change means a new broadcast, a new spade URL, or a
// re-published payload landed mid-send and the beacon must be suppressed.
func (s *Stream) SessionGeneration() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessionGen
}

// BeginSessionObservation starts a new full-session refresh observation and
// returns its id. A refresh MUST call this BEFORE its network I/O; only the
// latest-begun observation may publish the spade URL (see PublishSpadeURLIfCurrent),
// so a slow older refresh started against an earlier session can never overwrite a
// newer one that started later (newest-STARTED-wins).
func (s *Stream) BeginSessionObservation() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessionObs++
	return s.sessionObs
}

// SessionObservation returns the current (latest-begun) full-session observation
// id without starting a new one, for a caller that wants to check whether its own
// observation is still current.
func (s *Stream) SessionObservation() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessionObs
}

// PublishSpadeURLIfCurrent publishes a freshly-fetched spade URL only if obs is
// still the latest-begun observation (newest-STARTED-wins). It returns false and
// publishes nothing when a newer observation has since begun — the stale refresh
// is dropped rather than clobbering the newer session. On a real change it bumps
// the session generation like SetSpadeURL.
//
// Production no longer publishes the spade URL on its own — a full session refresh
// folds it into a single ApplyPlaybackSessionIfCurrent (so the whole tuple is
// published atomically). This primitive is retained as the minimal
// observation-ordering building block and for tests.
func (s *Stream) PublishSpadeURLIfCurrent(obs uint64, url string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if obs != s.sessionObs {
		return false
	}
	s.setSpadeURLLocked(url)
	return true
}

// ExpectedSession is an optimistic-concurrency precondition for an atomic session
// publication. A zero field is EXPLICITLY "unspecified" (do not check it), never a
// wildcard: a concrete value must match exactly or the apply is rejected as stale.
type ExpectedSession struct {
	// BroadcastID, when non-empty, must equal the current broadcast ID at apply
	// time (a current empty ID does NOT match a concrete expected one).
	BroadcastID string
	// Generation, when non-zero, must equal the current session generation at
	// apply time.
	Generation uint64
}

// PlaybackSessionCandidate is an immutable, fully-parsed watch session built OFF
// the Stream lock (from network responses) and published in a single atomic apply.
// A field is applied only when present, so the candidate can carry a full session
// (broadcast + metadata + payload + spade URL) or a spade-only update:
//   - BroadcastID != "" applies broadcast + Title/Game/Tags/ViewersCount (and
//     re-arms the watch streak on a changed broadcast);
//   - hasSpadeURL applies the spade URL;
//   - a non-empty payload applies the beacon payload.
//
// Building it never touches the live Stream, so no partial tuple is ever visible.
type PlaybackSessionCandidate struct {
	BroadcastID  string
	Title        string
	Game         *Game
	Tags         []Tag
	ViewersCount int

	spadeURL    string
	hasSpadeURL bool
	payload     []MinuteWatchedEvent
}

// WithSpadeURL returns a copy of the candidate carrying a freshly-discovered spade
// URL to publish atomically with the rest of the session.
func (c PlaybackSessionCandidate) WithSpadeURL(url string) PlaybackSessionCandidate {
	c.spadeURL = url
	c.hasSpadeURL = true
	return c
}

// WithPayload returns a copy of the candidate carrying the beacon payload built
// from the same observed broadcast identity.
func (c PlaybackSessionCandidate) WithPayload(channelID, broadcastID, userID, channel string, game *Game) PlaybackSessionCandidate {
	c.payload = BuildMinuteWatchedPayload(channelID, broadcastID, userID, channel, game)
	return c
}

// IsEmpty reports whether the candidate would change nothing (no broadcast, no
// spade URL, no payload) — the apply is then a no-op.
func (c PlaybackSessionCandidate) IsEmpty() bool {
	return c.BroadcastID == "" && !c.hasSpadeURL && len(c.payload) == 0
}

// SessionApplyResult is the outcome of an atomic session publication.
type SessionApplyResult struct {
	Applied            bool
	Stale              bool
	Reason             string // bounded reason code when stale ("" on apply)
	Generation         uint64 // the published generation (0 when not applied)
	CurrentGeneration  uint64 // the live generation observed at apply time
	CurrentBroadcastID string // the live broadcast ID observed at apply time
}

// Bounded reason-code vocabulary for a stale/rejected apply.
const (
	SessionStaleSupersededObs = "superseded_observation"
	SessionStaleGeneration    = "generation_drift"
	SessionStaleBroadcast     = "broadcast_changed"
)

// ApplyPlaybackSessionIfCurrent publishes the candidate's watch session in ONE
// atomic step under the Stream lock — broadcast ID, title/game/tags/viewers, spade
// URL, and beacon payload together, with exactly one new session generation for
// the published tuple — but only when the optimistic preconditions still hold:
//
//  1. obs is still the latest-begun full-session observation (newest-STARTED-wins);
//  2. expected.Generation, when non-zero, still equals the current generation;
//  3. expected.BroadcastID, when non-empty, still equals the current broadcast ID.
//
// If any precondition fails, NOTHING is published (a newer session is preserved
// without partial overwrite) and the result is a typed stale outcome — never a
// silent success. A changed broadcast re-arms the watch streak exactly as
// Stream.Update does.
func (s *Stream) ApplyPlaybackSessionIfCurrent(obs uint64, cand PlaybackSessionCandidate, expected ExpectedSession) SessionApplyResult {
	s.mu.Lock()
	defer s.mu.Unlock()

	res := SessionApplyResult{CurrentGeneration: s.sessionGen, CurrentBroadcastID: s.BroadcastID}

	switch {
	case obs != s.sessionObs:
		res.Stale, res.Reason = true, SessionStaleSupersededObs
		return res
	case expected.Generation != 0 && s.sessionGen != expected.Generation:
		res.Stale, res.Reason = true, SessionStaleGeneration
		return res
	case expected.BroadcastID != "" && s.BroadcastID != expected.BroadcastID:
		res.Stale, res.Reason = true, SessionStaleBroadcast
		return res
	}

	if cand.BroadcastID != "" {
		if cand.BroadcastID != s.BroadcastID {
			if s.BroadcastID != "" {
				// A changed broadcast is a new live session: re-arm the streak, exactly
				// as Stream.Update's changed-ID path does.
				s.armWatchStreakLocked()
			}
			s.BroadcastID = cand.BroadcastID
		}
		s.Title = cand.Title
		s.Game = cand.Game
		s.Tags = cand.Tags
		s.ViewersCount = cand.ViewersCount
		s.lastUpdate = time.Now()
	}
	if cand.hasSpadeURL {
		s.spadeURL = cand.spadeURL
	}
	if len(cand.payload) > 0 {
		s.payload = cand.payload
	}

	// Exactly one new generation for the whole published tuple.
	s.sessionGen++

	res.Applied = true
	res.Generation = s.sessionGen
	res.CurrentGeneration = s.sessionGen
	res.CurrentBroadcastID = s.BroadcastID
	return res
}
