package resources

import (
	"errors"
	"os"
	"testing"
)

// procStat builds a /proc/self/stat line with a given utime/stime (fields 14/15)
// and a deliberately awkward comm containing spaces and parentheses, to exercise
// the last-')' parse.
func procStat(utime, stime uint64) string {
	// state + 10 placeholders, then utime (field 14) + stime (field 15).
	return "1234 (my (weird) proc) R 0 0 0 0 0 0 0 0 0 0 " +
		itoa(utime) + " " + itoa(stime) + " 0 0 0 0"
}

func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

func TestParseProcStatTicks(t *testing.T) {
	ticks, ok := parseProcStatTicks([]byte(procStat(200, 55)))
	if !ok || ticks != 255 {
		t.Fatalf("ticks=%d ok=%v, want 255 true", ticks, ok)
	}
	// Malformed inputs never panic and report not-ok.
	for _, bad := range []string{"", "no parens here", "1234 (proc)", "1234 (proc) R only few"} {
		if _, ok := parseProcStatTicks([]byte(bad)); ok {
			t.Errorf("parseProcStatTicks(%q) unexpectedly ok", bad)
		}
	}
}

func TestParseNetDev(t *testing.T) {
	const dev = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo:    6627      18    0    0    0     0          0         0     6627      18    0    0    0     0       0          0
  eth0: 1000000    7961    0   18    0     0          0         0  2000000    7970    0    0    0     0       0          0
  wlan0:  500000     100    0    0    0     0          0         0   250000     100    0    0    0     0       0          0`
	rx, tx, ok := parseNetDev([]byte(dev))
	if !ok {
		t.Fatal("parseNetDev not ok")
	}
	// lo excluded; eth0 + wlan0 summed.
	if rx != 1500000 || tx != 2250000 {
		t.Fatalf("rx=%d tx=%d, want 1500000 2250000 (lo excluded)", rx, tx)
	}
	// A malformed data line is skipped, good lines still summed.
	const partial = "hdr1\nhdr2\n eth0: 42 x y\n  eth1: 100 1 2 3 4 5 6 7 200 1 2 3 4 5 6 7\n"
	rx, tx, ok = parseNetDev([]byte(partial))
	if !ok || rx != 100 || tx != 200 {
		t.Fatalf("partial: rx=%d tx=%d ok=%v, want 100 200 true", rx, tx, ok)
	}
	// Only garbage -> not ok.
	if _, _, ok := parseNetDev([]byte("hdr1\nhdr2\n")); ok {
		t.Error("empty net/dev should be not-ok")
	}
}

func TestParseSelfIO(t *testing.T) {
	const io = "rchar: 4092\nwchar: 0\nsyscr: 9\nsyscw: 0\nread_bytes: 5242880\nwrite_bytes: 1048576\ncancelled_write_bytes: 0\n"
	r, w, ok := parseSelfIO([]byte(io))
	if !ok || r != 5242880 || w != 1048576 {
		t.Fatalf("r=%d w=%d ok=%v, want 5242880 1048576 true", r, w, ok)
	}
	// Missing write_bytes -> not ok (uses read_bytes/write_bytes, not rchar/wchar).
	if _, _, ok := parseSelfIO([]byte("rchar: 10\nread_bytes: 20\n")); ok {
		t.Error("missing write_bytes should be not-ok")
	}
}

func TestParseMeminfoAndVmRSS(t *testing.T) {
	if b, ok := parseMeminfoTotal([]byte("MemFree: 1 kB\nMemTotal:       16461176 kB\n")); !ok || b != 16461176*1024 {
		t.Fatalf("MemTotal=%d ok=%v, want %d", b, ok, 16461176*1024)
	}
	if b, ok := parseVmRSS([]byte("VmHWM:\t 1952 kB\nVmRSS:\t 1952 kB\n")); !ok || b != 1952*1024 {
		t.Fatalf("VmRSS=%d ok=%v, want %d", b, ok, 1952*1024)
	}
	if _, ok := parseVmRSS([]byte("VmHWM: 1 kB\n")); ok {
		t.Error("missing VmRSS should be not-ok")
	}
}

// v1FS returns a fake filesystem mirroring the hybrid cgroup-v1 layout: memory
// controller under a nested path, cpu/cpuacct at root, no unified controllers.
func v1FS() map[string]string {
	return map[string]string{
		"/proc/self/cgroup": "7:pids:/\n3:memory:/process_api/abc\n2:cpuacct:/\n1:cpu:/\n0::/\n",
		"/sys/fs/cgroup/memory/process_api/abc/memory.usage_in_bytes": "779255808",
		"/sys/fs/cgroup/memory/process_api/abc/memory.limit_in_bytes": "9223372036854771712",
		"/sys/fs/cgroup/cpu/cpu.cfs_quota_us":                         "-1",
		"/sys/fs/cgroup/cpu/cpu.cfs_period_us":                        "100000",
		"/proc/meminfo":                                               "MemTotal:       16461176 kB\n",
	}
}

// v2FS returns a fake unified cgroup-v2 layout.
func v2FS() map[string]string {
	return map[string]string{
		"/proc/self/cgroup":                     "0::/mygroup\n",
		"/sys/fs/cgroup/cgroup.controllers":     "cpuset cpu io memory pids",
		"/sys/fs/cgroup/mygroup/cpu.max":        "200000 100000",
		"/sys/fs/cgroup/mygroup/memory.current": "268435456",
		"/sys/fs/cgroup/mygroup/memory.max":     "536870912",
		"/proc/meminfo":                         "MemTotal:       16461176 kB\n",
	}
}

func fsRead(files map[string]string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		if v, ok := files[path]; ok {
			return []byte(v), nil
		}
		return nil, os.ErrNotExist
	}
}

func TestCgroupV1MemoryNestedPathAndUnlimited(t *testing.T) {
	rd := fsRead(v1FS())
	used, ok := memoryUsed(rd, "/proc", "/sys/fs/cgroup")
	if !ok || used != 779255808 {
		t.Fatalf("used=%d ok=%v, want 779255808 (nested v1 path)", used, ok)
	}
	limit, isPhys := memoryLimit(rd, "/proc", "/sys/fs/cgroup", 16461176*1024)
	if !isPhys || limit != 16461176*1024 {
		t.Fatalf("limit=%d isPhys=%v, want %d true (sentinel -> physical RAM)", limit, isPhys, 16461176*1024)
	}
}

func TestCgroupV1CPUUnlimitedFallsBackToNumCPU(t *testing.T) {
	rd := fsRead(v1FS())
	if cores := cpuLimitCores(rd, "/proc", "/sys/fs/cgroup", 4); cores != 4 {
		t.Fatalf("cores=%v, want 4 (quota -1 -> NumCPU)", cores)
	}
}

func TestCgroupV1CPUWithQuota(t *testing.T) {
	files := v1FS()
	files["/sys/fs/cgroup/cpu/cpu.cfs_quota_us"] = "200000"
	rd := fsRead(files)
	if cores := cpuLimitCores(rd, "/proc", "/sys/fs/cgroup", 4); cores != 2 {
		t.Fatalf("cores=%v, want 2 (200000/100000)", cores)
	}
}

func TestCgroupV2MemoryAndCPU(t *testing.T) {
	rd := fsRead(v2FS())
	used, ok := memoryUsed(rd, "/proc", "/sys/fs/cgroup")
	if !ok || used != 268435456 {
		t.Fatalf("used=%d ok=%v, want 268435456", used, ok)
	}
	limit, isPhys := memoryLimit(rd, "/proc", "/sys/fs/cgroup", 16461176*1024)
	if isPhys || limit != 536870912 {
		t.Fatalf("limit=%d isPhys=%v, want 536870912 false (real v2 limit)", limit, isPhys)
	}
	if cores := cpuLimitCores(rd, "/proc", "/sys/fs/cgroup", 4); cores != 2 {
		t.Fatalf("cores=%v, want 2 (cpu.max 200000/100000)", cores)
	}
}

func TestCgroupCPUQuotaCappedAtNumCPU(t *testing.T) {
	files := v2FS()
	files["/sys/fs/cgroup/mygroup/cpu.max"] = "800000 100000" // 8 cores
	rd := fsRead(files)
	if cores := cpuLimitCores(rd, "/proc", "/sys/fs/cgroup", 4); cores != 4 {
		t.Fatalf("cores=%v, want 4 (quota 8 capped at NumCPU)", cores)
	}
}

func TestCgroupMemoryLimitCappedAtRAM(t *testing.T) {
	files := v2FS()
	files["/sys/fs/cgroup/mygroup/memory.max"] = "34359738368" // 32 GiB
	rd := fsRead(files)
	limit, isPhys := memoryLimit(rd, "/proc", "/sys/fs/cgroup", 16856244224) // 16 GiB
	if isPhys || limit != 16856244224 {
		t.Fatalf("limit=%d isPhys=%v, want 16856244224 false (capped at RAM)", limit, isPhys)
	}
}

func TestMemoryVmRSSFallback(t *testing.T) {
	// No cgroup memory files; only /proc/self/status + meminfo.
	files := map[string]string{
		"/proc/self/cgroup": "1:cpu:/\n",
		"/proc/self/status": "VmRSS:\t 4096 kB\n",
		"/proc/meminfo":     "MemTotal: 1048576 kB\n",
	}
	rd := fsRead(files)
	used, ok := memoryUsed(rd, "/proc", "/sys/fs/cgroup")
	if !ok || used != 4096*1024 {
		t.Fatalf("used=%d ok=%v, want %d (VmRSS fallback)", used, ok, 4096*1024)
	}
	limit, isPhys := memoryLimit(rd, "/proc", "/sys/fs/cgroup", 1048576*1024)
	if !isPhys || limit != 1048576*1024 {
		t.Fatalf("limit=%d isPhys=%v, want %d true", limit, isPhys, 1048576*1024)
	}
}

func TestMemoryFullyUnavailable(t *testing.T) {
	files := map[string]string{"/proc/self/cgroup": "1:cpu:/\n"} // no memory source at all
	if _, ok := memoryUsed(fsRead(files), "/proc", "/sys/fs/cgroup"); ok {
		t.Error("memoryUsed should be not-ok with no source (no 0-byte fabrication)")
	}
}

func TestCgroupWalkUpFallback(t *testing.T) {
	// Nested memory file absent, but present at the controller root.
	files := map[string]string{
		"/proc/self/cgroup":                           "3:memory:/process_api/abc\n",
		"/sys/fs/cgroup/memory/memory.usage_in_bytes": "12345",
	}
	used, ok := memoryUsed(fsRead(files), "/proc", "/sys/fs/cgroup")
	if !ok || used != 12345 {
		t.Fatalf("used=%d ok=%v, want 12345 (root fallback)", used, ok)
	}
}

func TestCgroupControllerTokenExactness(t *testing.T) {
	// cpuacct at root, cpu with a quota nested — the cpu controller must not be
	// matched by substring against cpuacct/cpuset.
	files := map[string]string{
		"/proc/self/cgroup":                          "2:cpuacct:/\n1:cpu:/mygrp\n",
		"/sys/fs/cgroup/cpu/mygrp/cpu.cfs_quota_us":  "300000",
		"/sys/fs/cgroup/cpu/mygrp/cpu.cfs_period_us": "100000",
	}
	if cores := cpuLimitCores(fsRead(files), "/proc", "/sys/fs/cgroup", 8); cores != 3 {
		t.Fatalf("cores=%v, want 3 (cpu at /mygrp, not cpuacct)", cores)
	}
}

func TestReadFilePermissionDeniedDegrades(t *testing.T) {
	rd := func(path string) ([]byte, error) {
		if path == "/proc/self/status" || path == "/proc/self/cgroup" {
			return nil, os.ErrPermission
		}
		return nil, os.ErrNotExist
	}
	if _, ok := memoryUsed(rd, "/proc", "/sys/fs/cgroup"); ok {
		t.Error("permission-denied everywhere should degrade to not-ok, not panic")
	}
	if !errors.Is(os.ErrPermission, os.ErrPermission) {
		t.Fatal("sanity") // guards against accidental import pruning
	}
}
