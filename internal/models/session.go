package models

import (
	"encoding/base64"
	"encoding/json"
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
func (s *Stream) PublishSpadeURLIfCurrent(obs uint64, url string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if obs != s.sessionObs {
		return false
	}
	s.setSpadeURLLocked(url)
	return true
}
