package miner

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/journal"
)

// deterministic clock for the health-journal tests.
type hClock struct {
	mu sync.Mutex
	t  time.Time
}

func newHClock() *hClock {
	return &hClock{t: time.Date(2026, 7, 24, 8, 0, 0, 0, time.UTC)}
}
func (c *hClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *hClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func journaledMiner(clk *hClock) *Miner {
	return &Miner{healthJournal: journal.New[journal.HealthEvent](64, clk.now)}
}

// driveHealth pushes a sequence of classified outcomes through the REAL
// transition function and the journal recorder, exactly as evaluateConnectionHealth
// does, so the tests exercise the production edge logic (not a reimplementation).
func driveHealth(m *Miner, clk *hClock, outcomes []connOutcome) {
	var state connHealthState
	for _, out := range outcomes {
		prevLevel := state.level
		newState, tr := nextConnTransition(state, out)
		state = newState
		m.recordHealthTransition(prevLevel, newState.level, out, tr)
		clk.advance(time.Minute)
	}
}

func healthEvents(m *Miner) []journal.HealthEvent {
	recs := m.HealthJournalSnapshot()
	out := make([]journal.HealthEvent, len(recs))
	for i, r := range recs {
		out[i] = r.Event
	}
	return out
}

func outHealthy(api apiConnState) connOutcome { return connOutcome{level: connHealthy, apiState: api} }
func outDegraded(api apiConnState, psDown, psDeg bool) connOutcome {
	return connOutcome{level: connDegraded, apiState: api, pubsubDown: psDown, pubsubDegraded: psDeg}
}
func outLost() connOutcome {
	return connOutcome{level: connLost, apiState: apiConnDown, pubsubDown: true}
}

// T11 — health duplicate: identical repeated state does not append a duplicate
// transition or send another alert; repeats are counted as suppressed.
func TestHealthJournalDuplicateDeduped(t *testing.T) {
	clk := newHClock()
	m := journaledMiner(clk)

	driveHealth(m, clk, []connOutcome{
		outLost(), outLost(), outLost(), // enter lost, then 2 identical repeats
		outHealthy(apiConnOK), // recover
	})

	evs := healthEvents(m)
	if len(evs) != 2 {
		t.Fatalf("expected 2 journal events (enter+restore), got %d: %+v", len(evs), evs)
	}
	if evs[0].Reason != journal.HealthReasonEnteredLost || !evs[0].NotificationEmitted {
		t.Fatalf("first event wrong: %+v", evs[0])
	}
	// Exactly one lost notification for three lost ticks.
	notifs := 0
	for _, e := range evs {
		if e.NotificationEmitted {
			notifs++
		}
	}
	if notifs != 2 { // one lost + one restored
		t.Fatalf("expected 2 notification-emitting events (1 lost, 1 restored), got %d", notifs)
	}
	// The recovery event records the two deduped identical ticks.
	if evs[1].SuppressedDuplicates != 2 {
		t.Fatalf("expected SuppressedDuplicates=2 on the restore event, got %d", evs[1].SuppressedDuplicates)
	}
}

// T12 — partial recovery: LOST -> DEGRADED is recorded as partial and emits no
// restored notification.
func TestHealthJournalPartialRecovery(t *testing.T) {
	clk := newHClock()
	m := journaledMiner(clk)

	driveHealth(m, clk, []connOutcome{
		outLost(),
		outDegraded(apiConnDown, false, false), // one path recovered: partial
	})

	evs := healthEvents(m)
	if len(evs) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(evs), evs)
	}
	partial := evs[1]
	if partial.NewLevel != journal.HealthLevelDegraded {
		t.Fatalf("expected degraded, got %s", partial.NewLevel)
	}
	if partial.Recovery != journal.RecoveryPartial || partial.Reason != journal.HealthReasonPartialRestore {
		t.Fatalf("expected partial recovery, got recovery=%s reason=%s", partial.Recovery, partial.Reason)
	}
	if partial.NotificationEmitted {
		t.Fatal("partial recovery must NOT emit a restored notification")
	}
}

// T13 — full recovery: transition to HEALTHY records full recovery and exactly
// one restored notification, even after a partial step.
func TestHealthJournalFullRecovery(t *testing.T) {
	clk := newHClock()
	m := journaledMiner(clk)

	driveHealth(m, clk, []connOutcome{
		outLost(),
		outDegraded(apiConnDown, false, false), // partial (no restored)
		outHealthy(apiConnOK),                  // full recovery
	})

	evs := healthEvents(m)
	if len(evs) != 3 {
		t.Fatalf("expected 3 events, got %d: %+v", len(evs), evs)
	}
	full := evs[2]
	if full.NewLevel != journal.HealthLevelHealthy || full.Recovery != journal.RecoveryFull {
		t.Fatalf("expected full recovery to healthy, got level=%s recovery=%s", full.NewLevel, full.Recovery)
	}
	if full.Reason != journal.HealthReasonFullRestore || !full.NotificationEmitted {
		t.Fatalf("full recovery must record full_restore + emit restored, got %+v", full)
	}
	if full.Evidence != journal.EvidenceAuthoritative {
		t.Fatalf("full recovery on confirmed API success must be authoritative, got %s", full.Evidence)
	}
	// Exactly one restored across the whole sequence.
	restored := 0
	for _, e := range evs {
		if e.Reason == journal.HealthReasonFullRestore && e.NotificationEmitted {
			restored++
		}
	}
	if restored != 1 {
		t.Fatalf("expected exactly one restored notification, got %d", restored)
	}
}

// T14 — API idle: an idle API never produces a failed/lost transition, and a
// recovery that rests only on the API going quiet is labeled inconclusive, not
// authoritative.
func TestHealthJournalIdleNeverFails(t *testing.T) {
	clk := newHClock()
	m := journaledMiner(clk)

	// Idle API with a healthy PubSub stays healthy across ticks — no transitions.
	driveHealth(m, clk, []connOutcome{
		outHealthy(apiConnIdle), outHealthy(apiConnIdle), outHealthy(apiConnIdle),
	})
	if evs := healthEvents(m); len(evs) != 0 {
		t.Fatalf("idle API must not produce any transition, got %+v", evs)
	}

	// A recovery whose API is merely idle (went quiet) is inconclusive evidence.
	clk2 := newHClock()
	m2 := journaledMiner(clk2)
	driveHealth(m2, clk2, []connOutcome{
		outLost(),
		outHealthy(apiConnIdle), // recovered by going quiet, not confirmed success
	})
	evs := healthEvents(m2)
	for _, e := range evs {
		if e.NewLevel == journal.HealthLevelLost && e.Reason != journal.HealthReasonEnteredLost {
			continue
		}
	}
	restore := evs[len(evs)-1]
	if restore.NewLevel != journal.HealthLevelHealthy {
		t.Fatalf("expected a healthy recovery, got %s", restore.NewLevel)
	}
	if restore.Evidence != journal.EvidenceInconclusive {
		t.Fatalf("recovery on idle API must be inconclusive evidence, got %s", restore.Evidence)
	}
	// And no event in the sequence is a failed/lost transition caused by idle.
	for _, e := range evs {
		if e.APIState == journal.APIStateIdle && e.NewLevel == journal.HealthLevelLost {
			t.Fatalf("idle API produced a LOST transition: %+v", e)
		}
	}
}

// TestHealthJournalEnteredLostEvidence: entering LOST rests on confirmed failure
// of both paths and is authoritative.
func TestHealthJournalEnteredLostEvidence(t *testing.T) {
	clk := newHClock()
	m := journaledMiner(clk)
	driveHealth(m, clk, []connOutcome{outLost()})
	e := healthEvents(m)[0]
	if e.Evidence != journal.EvidenceAuthoritative || e.APIState != journal.APIStateDown || !e.PubSubDown {
		t.Fatalf("entered_lost evidence/inputs wrong: %+v", e)
	}
}

// TestHealthJournalMappingCodes checks the internal enum -> stable code mapping.
func TestHealthJournalMappingCodes(t *testing.T) {
	if levelCode(connHealthy) != journal.HealthLevelHealthy ||
		levelCode(connDegraded) != journal.HealthLevelDegraded ||
		levelCode(connLost) != journal.HealthLevelLost {
		t.Fatal("levelCode mapping wrong")
	}
	if apiStateCode(apiConnIdle) != journal.APIStateIdle ||
		apiStateCode(apiConnOK) != journal.APIStateOK ||
		apiStateCode(apiConnDegraded) != journal.APIStateDegraded ||
		apiStateCode(apiConnDown) != journal.APIStateDown ||
		apiStateCode(apiConnUnknown) != journal.APIStateUnknown {
		t.Fatal("apiStateCode mapping wrong")
	}
}

// TestHealthJournalNoSecrets scans a driven health journal for secret markers.
func TestHealthJournalNoSecrets(t *testing.T) {
	clk := newHClock()
	m := journaledMiner(clk)
	driveHealth(m, clk, []connOutcome{outLost(), outDegraded(apiConnDown, false, false), outHealthy(apiConnOK)})
	blob, err := json.Marshal(m.HealthJournalSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"token", "bearer", "cookie", "spade", "https://", "webhook", "oauth"} {
		if strings.Contains(strings.ToLower(string(blob)), bad) {
			t.Fatalf("health journal leaked marker %q: %s", bad, blob)
		}
	}
}
