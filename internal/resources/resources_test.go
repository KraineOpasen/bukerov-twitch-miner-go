package resources

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestUnavailableSnapshotShape(t *testing.T) {
	snap := UnavailableSnapshot()
	if snap.Available || snap.CPU.Available || snap.Memory.Available || snap.Network.Available || snap.Disk.Available {
		t.Error("UnavailableSnapshot must mark everything unavailable")
	}
	if snap.SampledAt != "" {
		t.Error("UnavailableSnapshot must have an empty sampledAt")
	}
	// History slices must be non-nil so they marshal as [] not null.
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "null") {
		t.Errorf("UnavailableSnapshot marshalled null (want []): %s", b)
	}
}

// allowedKeys is the complete, exact JSON key allowlist. Any key outside this
// set reaching the wire is a privacy regression.
var allowedKeys = map[string]struct{}{
	"available": {}, "sampledAt": {}, "cpu": {}, "memory": {}, "network": {}, "disk": {},
	"percent": {}, "limitCores": {}, "history": {},
	"usedBytes": {}, "limitBytes": {},
	"rxBytesPerSec": {}, "txBytesPerSec": {},
	"readBytesPerSec": {}, "writeBytesPerSec": {},
}

func walkKeys(t *testing.T, v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, sub := range x {
			if _, ok := allowedKeys[k]; !ok {
				t.Errorf("disallowed JSON key %q reached the wire", k)
			}
			walkKeys(t, sub)
		}
	case []any:
		for _, sub := range x {
			walkKeys(t, sub)
		}
	}
}

func TestSnapshotMarshalAllowlist(t *testing.T) {
	// A fully populated snapshot from a real sample.
	s, fs := newTestSampler(v2FS(), 4)
	fs.set("/proc/self/stat", procStat(0, 0))
	fs.set("/proc/net/dev", netDev(1000, 1000))
	fs.set("/proc/self/io", selfIO(0, 0))
	s.sample(time.Unix(0, 0))
	fs.set("/proc/self/stat", procStat(100, 0))
	fs.set("/proc/net/dev", netDev(6000, 6000))
	fs.set("/proc/self/io", selfIO(1000, 2000))
	s.sample(time.Unix(5, 0))

	b, err := json.Marshal(s.Latest())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	walkKeys(t, doc)
}

func TestSnapshotPrivacyNoIdentityLeaks(t *testing.T) {
	// Fixtures deliberately laced with identity sentinels the DTO must never emit.
	files := map[string]string{
		"/proc/self/cgroup": "3:memory:/process_api/SECRETCONTAINERID\n1:cpu:/\n",
		"/sys/fs/cgroup/memory/process_api/SECRETCONTAINERID/memory.usage_in_bytes": "1000",
		"/sys/fs/cgroup/memory/process_api/SECRETCONTAINERID/memory.limit_in_bytes": "9223372036854771712",
		"/sys/fs/cgroup/cpu/cpu.cfs_quota_us":                                       "-1",
		"/sys/fs/cgroup/cpu/cpu.cfs_period_us":                                      "100000",
		"/proc/meminfo":                                                             "MemTotal: 1048576 kB\n",
		"/proc/self/stat":                                                           procStat(0, 0),
		"/proc/self/io":                                                             selfIO(0, 0),
		// Interface name is a sentinel; the summed totals must not carry it.
		"/proc/net/dev": "hdr1\nhdr2\n  eth0SENTINEL: 1000 1 2 3 4 5 6 7 2000 1 2 3 4 5 6 7\n",
	}
	s, fs := newTestSampler(files, 4)
	s.sample(time.Unix(0, 0))
	fs.set("/proc/net/dev", "hdr1\nhdr2\n  eth0SENTINEL: 6000 1 2 3 4 5 6 7 7000 1 2 3 4 5 6 7\n")
	s.sample(time.Unix(5, 0))

	b, _ := json.Marshal(s.Latest())
	body := string(b)
	for _, sentinel := range []string{
		"SECRETCONTAINERID", "process_api", "eth0SENTINEL", "eth0", "lo",
		"/sys/fs/cgroup", "/proc", "cgroup", "MemTotal", "read_bytes",
	} {
		if strings.Contains(body, sentinel) {
			t.Errorf("resource JSON leaked sentinel %q:\n%s", sentinel, body)
		}
	}
}

func TestNormalizePercent(t *testing.T) {
	pts := []histPoint{{50, true}, {0, false}, {150, true}, {-10, true}}
	got := normalizePercent(pts)
	want := []float64{0.5, 0, 1, 0} // clamped; invalid -> 0
	for i := range want {
		if !approx(got[i], want[i]) {
			t.Errorf("normalizePercent[%d]=%v, want %v", i, got[i], want[i])
		}
	}
}

func TestNormalizeWindow(t *testing.T) {
	pts := []histPoint{{100, true}, {50, true}, {0, false}, {200, true}}
	got := normalizeWindow(pts) // max valid = 200
	want := []float64{0.5, 0.25, 0, 1}
	for i := range want {
		if !approx(got[i], want[i]) {
			t.Errorf("normalizeWindow[%d]=%v, want %v", i, got[i], want[i])
		}
	}
	// All invalid / all zero -> all zero, no divide-by-zero.
	if got := normalizeWindow([]histPoint{{0, false}, {0, true}}); got[0] != 0 || got[1] != 0 {
		t.Errorf("normalizeWindow all-zero=%v, want [0 0]", got)
	}
}
