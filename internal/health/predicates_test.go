package health

import "testing"

// TestSignalStatePredicates pins the semantics the dashboard and the drop
// watchdog rely on: degraded is NOT healthy (it is impaired) and is the only
// status for which Degraded() is true.
func TestSignalStatePredicates(t *testing.T) {
	cases := []struct {
		status   string
		healthy  bool
		degraded bool
	}{
		{StatusOK, true, false},
		{StatusIdle, true, false},
		{StatusDegraded, false, true},
		{StatusFailed, false, false},
		{StatusStalled, false, false},
		{StatusUnknown, false, false},
	}
	for _, c := range cases {
		s := Signal{Status: c.status}
		if s.Healthy() != c.healthy {
			t.Errorf("%s: Healthy() = %v, want %v", c.status, s.Healthy(), c.healthy)
		}
		if s.Degraded() != c.degraded {
			t.Errorf("%s: Degraded() = %v, want %v", c.status, s.Degraded(), c.degraded)
		}
	}
}

// TestTwitchOutageTreatsDegradedAsOutage guards the coupling: a degraded
// transport must count as an outage so the drop-progress watchdog defers stall
// confirmation while the network is flapping (not just on full failure).
func TestTwitchOutageTreatsDegradedAsOutage(t *testing.T) {
	center := NewCenter()
	w := &ProgressWatchdog{center: center}

	center.Record(Signal{Name: SignalGQLAPI, Status: StatusOK})
	if out, _ := w.twitchOutage(); out {
		t.Fatal("healthy GQL must not report an outage")
	}

	center.Record(Signal{Name: SignalGQLAPI, Status: StatusDegraded})
	if out, name := w.twitchOutage(); !out || name != SignalGQLAPI {
		t.Fatalf("degraded GQL must report an outage, got out=%v name=%q", out, name)
	}
}
