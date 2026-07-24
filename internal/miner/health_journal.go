package miner

import (
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/journal"
)

// Bounded capacities for the diagnostic journals. Slot lifecycle events are more
// frequent than health transitions, so it gets the larger ring. Both are small,
// fixed, in-memory ring buffers — no persistence, no config.
const (
	slotJournalCapacity   = 256
	healthJournalCapacity = 128
)

// levelCode maps the internal aggregate connLevel to the journal's stable code.
func levelCode(l connLevel) string {
	switch l {
	case connHealthy:
		return journal.HealthLevelHealthy
	case connDegraded:
		return journal.HealthLevelDegraded
	case connLost:
		return journal.HealthLevelLost
	default:
		return journal.HealthLevelHealthy
	}
}

// apiStateCode maps the internal apiConnState to the journal's stable code.
func apiStateCode(s apiConnState) string {
	switch s {
	case apiConnIdle:
		return journal.APIStateIdle
	case apiConnOK:
		return journal.APIStateOK
	case apiConnDegraded:
		return journal.APIStateDegraded
	case apiConnDown:
		return journal.APIStateDown
	default: // apiConnUnknown
		return journal.APIStateUnknown
	}
}

// evidenceFor labels the quality of the evidence behind a transition — a
// distinction the state machine computes implicitly but never records.
//   - LOST rests on positive confirmed failure of BOTH paths (authoritative).
//   - HEALTHY rests on confirmed API success (authoritative) OR merely on the
//     API going idle/unknown (inconclusive — recovered by going quiet, not by a
//     fresh confirmed success).
//   - DEGRADED rests on degraded-tier evidence.
func evidenceFor(newLevel connLevel, out connOutcome) string {
	switch newLevel {
	case connLost:
		return journal.EvidenceAuthoritative
	case connHealthy:
		switch out.apiState {
		case apiConnIdle, apiConnUnknown:
			return journal.EvidenceInconclusive
		default: // apiConnOK (healthy cannot be reached with a down/degraded API)
			return journal.EvidenceAuthoritative
		}
	case connDegraded:
		return journal.EvidenceDegraded
	default:
		return journal.EvidenceInconclusive
	}
}

// recoveryFor classifies the recovery kind from the transition flags: full only
// on reaching HEALTHY while a lost alert was outstanding, partial on LOST ->
// DEGRADED, none otherwise.
func recoveryFor(tr connTransition) string {
	switch {
	case tr.fullRestore:
		return journal.RecoveryFull
	case tr.partialRestore:
		return journal.RecoveryPartial
	default:
		return journal.RecoveryNone
	}
}

// healthReasonFor returns the bounded transition reason code.
func healthReasonFor(tr connTransition) string {
	switch {
	case tr.enteredLost:
		return journal.HealthReasonEnteredLost
	case tr.fullRestore:
		return journal.HealthReasonFullRestore
	case tr.partialRestore:
		return journal.HealthReasonPartialRestore
	case tr.enteredDegraded:
		return journal.HealthReasonEnteredDegraded
	case tr.stabilized:
		return journal.HealthReasonStabilized
	default:
		return journal.HealthReasonLevelChanged
	}
}

// buildHealthEvent turns one classified transition into a journal event, or
// reports that the tick was NOT a meaningful transition (a repeated identical
// state) and should be deduped. It is a pure function of already-computed
// outputs — it never reclassifies and never influences a health/notification
// decision.
func buildHealthEvent(prevLevel, newLevel connLevel, out connOutcome, tr connTransition) (journal.HealthEvent, bool) {
	meaningful := newLevel != prevLevel ||
		tr.enteredLost || tr.fullRestore || tr.partialRestore || tr.enteredDegraded || tr.stabilized
	if !meaningful {
		return journal.HealthEvent{}, false
	}
	return journal.HealthEvent{
		Type:                  journal.HealthTransition,
		Domain:                "connection",
		PrevLevel:             levelCode(prevLevel),
		NewLevel:              levelCode(newLevel),
		APIState:              apiStateCode(out.apiState),
		PubSubDown:            out.pubsubDown,
		PubSubDegraded:        out.pubsubDegraded,
		Evidence:              evidenceFor(newLevel, out),
		Recovery:              recoveryFor(tr),
		Reason:                healthReasonFor(tr),
		NotificationRequested: tr.notifyLost || tr.notifyRestored,
	}, true
}

// recordHealthTransition journals a connection-health transition. A repeated
// identical state is deduped — never appended twice — and counted in the next
// recorded transition's SuppressedDuplicates. Called only from the single
// watchdog goroutine (via evaluateConnectionHealth), so healthJournalSuppressed
// needs no lock; the journal's own lock serves the cross-goroutine reader.
func (m *Miner) recordHealthTransition(prevLevel, newLevel connLevel, out connOutcome, tr connTransition) {
	ev, meaningful := buildHealthEvent(prevLevel, newLevel, out, tr)
	if !meaningful {
		m.healthJournalSuppressed++
		return
	}
	// The watchdog only INVOKES the notification manager when it is present — the
	// send at evaluateConnectionHealth is gated on m.notifications != nil. Record
	// only that narrow fact (a request was made); the journal never claims an
	// external alert was actually sent or delivered.
	ev.NotificationRequested = ev.NotificationRequested && m.notifications != nil
	ev.SuppressedDuplicates = m.healthJournalSuppressed
	m.healthJournalSuppressed = 0
	m.healthJournal.Append(ev)
}

// HealthJournalSnapshot returns an immutable, oldest-first copy of the
// connection-health transition journal. Safe to call from any goroutine.
func (m *Miner) HealthJournalSnapshot() []journal.Record[journal.HealthEvent] {
	return m.healthJournal.Snapshot()
}
