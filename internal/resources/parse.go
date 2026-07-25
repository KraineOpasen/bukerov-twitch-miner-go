package resources

import (
	"strconv"
	"strings"
)

// userHZ is the kernel clock-tick rate (getconf CLK_TCK). It is 100 on every
// Linux platform this miner targets; /proc/self/stat reports CPU time in these
// ticks. Reading it dynamically would need cgo (sysconf), which conflicts with
// the static-binary / no-new-deps constraint, so it is a documented constant.
const userHZ = 100

// cgroupV1MemUnlimited is PAGE_COUNTER_MAX (page-aligned int64 max, = int64max-4095
// = 0x7FFFFFFFFFFFF000). cgroup v1 writes this to memory.limit_in_bytes when no
// limit is set. It is NOT plain int64 max (9223372036854775807) — they differ by
// 4095 — so an exact-equality check on int64 max would miss it.
const cgroupV1MemUnlimited uint64 = 0x7FFFFFFFFFFFF000

// parseProcStatTicks extracts utime+stime (total process CPU time, in clock
// ticks) from the single line of /proc/self/stat. The comm field (field 2) is
// wrapped in parentheses and may itself contain spaces and parentheses, so the
// fields after it are located from the LAST ')' rather than by naive splitting.
// Field N (1-indexed, proc(5)) maps to index N-3 of the post-comm slice:
// utime=field14=index11, stime=field15=index12.
func parseProcStatTicks(data []byte) (ticks uint64, ok bool) {
	s := string(data)
	rp := strings.LastIndex(s, ")")
	if rp < 0 || rp+2 > len(s) {
		return 0, false
	}
	f := strings.Fields(s[rp+2:])
	if len(f) < 13 {
		return 0, false
	}
	utime, err1 := strconv.ParseUint(f[11], 10, 64)
	stime, err2 := strconv.ParseUint(f[12], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return utime + stime, true
}

// parseNetDev sums receive and transmit byte counters across every non-loopback
// interface in /proc/net/dev. This file is network-namespace-scoped, so the
// totals are the container's own traffic under the scratch Docker deployment
// (and the host namespace's only when run as a bare binary). The loopback line
// ("lo:") is skipped so internal traffic is not counted; the interface name is
// used only to exclude and is then discarded — it never leaves this function. A
// malformed interface line is skipped (not fatal); ok is true when at least one
// interface parsed.
func parseNetDev(data []byte) (rx, tx uint64, ok bool) {
	var any bool
	for _, line := range strings.Split(string(data), "\n") {
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue // header lines have no ':'; interface names never contain one
		}
		name := strings.TrimSpace(line[:colon])
		if name == "" || name == "lo" {
			continue
		}
		f := strings.Fields(line[colon+1:])
		if len(f) < 16 {
			continue // receive(8) + transmit(8) columns
		}
		rxb, err1 := strconv.ParseUint(f[0], 10, 64) // receive bytes
		txb, err2 := strconv.ParseUint(f[8], 10, 64) // transmit bytes
		if err1 != nil || err2 != nil {
			continue
		}
		rx += rxb
		tx += txb
		any = true
	}
	return rx, tx, any
}

// parseSelfIO extracts read_bytes/write_bytes (actual block-device I/O, not the
// rchar/wchar syscall byte counts) from /proc/self/io. ok is true only when both
// keys are found and parse.
func parseSelfIO(data []byte) (read, write uint64, ok bool) {
	var haveR, haveW bool
	for _, line := range strings.Split(string(data), "\n") {
		key, val, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		val = strings.TrimSpace(val)
		switch key {
		case "read_bytes":
			if v, err := strconv.ParseUint(val, 10, 64); err == nil {
				read, haveR = v, true
			}
		case "write_bytes":
			if v, err := strconv.ParseUint(val, 10, 64); err == nil {
				write, haveW = v, true
			}
		}
	}
	return read, write, haveR && haveW
}

// parseMeminfoTotal returns total physical memory in bytes from the "MemTotal:"
// line of /proc/meminfo (reported in kB).
func parseMeminfoTotal(data []byte) (bytes uint64, ok bool) {
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			return 0, false
		}
		kb, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			return 0, false
		}
		return kb * 1024, true
	}
	return 0, false
}

// parseVmRSS returns the process resident set size in bytes from the "VmRSS:"
// line of /proc/self/status (reported in kB). This is the /proc fallback for
// used memory when no cgroup accounting is available.
func parseVmRSS(data []byte) (bytes uint64, ok bool) {
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			return 0, false
		}
		kb, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			return 0, false
		}
		return kb * 1024, true
	}
	return 0, false
}

// cgroupControllerPath resolves the on-disk cgroup directory for one controller
// by reading /proc/self/cgroup. It handles both layouts from a single source of
// truth:
//   - unified v2: the "0::<path>" line, when <cgroupRoot>/cgroup.controllers exists.
//   - legacy v1: the "<id>:<controllers>:<path>" line whose controller list
//     contains an exact token match for `controller` (so "cpu" is distinguished
//     from "cpuacct"/"cpuset").
//
// v2 reports true in `isV2`; the returned dir already includes the nested path.
// A missing/unreadable /proc/self/cgroup yields ok=false.
func cgroupControllerPath(readFile func(string) ([]byte, error), procRoot, cgroupRoot, controller string) (dir string, isV2, ok bool) {
	data, err := readFile(procRoot + "/self/cgroup")
	if err != nil {
		return "", false, false
	}
	// v2 unified is authoritative only when the unified controllers file exists
	// at the cgroup root; a bare "0::/" line also appears on hybrid v1 hosts.
	_, v2Root := readFile(cgroupRoot + "/cgroup.controllers")
	unifiedAvailable := v2Root == nil

	var v1Path string
	var haveV1 bool
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		if parts[1] == "" { // "0::<path>" — unified v2 hierarchy
			if unifiedAvailable {
				return cgroupRoot + parts[2], true, true
			}
			continue
		}
		for _, c := range strings.Split(parts[1], ",") {
			if c == controller {
				v1Path = parts[2]
				haveV1 = true
			}
		}
	}
	if haveV1 {
		return cgroupRoot + "/" + controller + v1Path, false, true
	}
	return "", false, false
}

// readCgroupFile reads a single cgroup file for a controller, walking up to the
// controller root if the file is absent at the nested path (nested cgroups do
// not always carry their own copy). Returns the trimmed contents.
func readCgroupFile(readFile func(string) ([]byte, error), procRoot, cgroupRoot, controller, file string) (string, bool) {
	dir, isV2, ok := cgroupControllerPath(readFile, procRoot, cgroupRoot, controller)
	if !ok {
		return "", false
	}
	if data, err := readFile(dir + "/" + file); err == nil {
		return strings.TrimSpace(string(data)), true
	}
	// Fallback to the controller root (v1) / cgroup root (v2).
	root := cgroupRoot + "/" + controller
	if isV2 {
		root = cgroupRoot
	}
	if data, err := readFile(root + "/" + file); err == nil {
		return strings.TrimSpace(string(data)), true
	}
	return "", false
}

// cpuLimitCores returns how many CPU cores this process/container may use. It
// prefers a cgroup CPU quota (v2 cpu.max "quota period", or v1
// cpu.cfs_quota_us/cpu.cfs_period_us) and falls back to the number of usable
// CPUs. A quota is never allowed to exceed the physical CPU count, and the
// result is floored at a small positive value so it can safely divide a percent.
func cpuLimitCores(readFile func(string) ([]byte, error), procRoot, cgroupRoot string, numCPU int) float64 {
	limit := float64(numCPU)

	// cgroup v2: cpu.max is "<quota> <period>" or "max <period>".
	if s, ok := readCgroupFile(readFile, procRoot, cgroupRoot, "", "cpu.max"); ok {
		if f := strings.Fields(s); len(f) >= 2 && f[0] != "max" {
			quota, e1 := strconv.ParseFloat(f[0], 64)
			period, e2 := strconv.ParseFloat(f[1], 64)
			if e1 == nil && e2 == nil && quota > 0 && period > 0 {
				limit = quota / period
			}
		}
	} else if qs, ok := readCgroupFile(readFile, procRoot, cgroupRoot, "cpu", "cpu.cfs_quota_us"); ok {
		// cgroup v1: quota <= 0 (e.g. -1) means "no limit".
		if quota, err := strconv.ParseInt(qs, 10, 64); err == nil && quota > 0 {
			if ps, ok := readCgroupFile(readFile, procRoot, cgroupRoot, "cpu", "cpu.cfs_period_us"); ok {
				if period, err := strconv.ParseInt(ps, 10, 64); err == nil && period > 0 {
					limit = float64(quota) / float64(period)
				}
			}
		}
	}

	if numCPU > 0 && limit > float64(numCPU) {
		limit = float64(numCPU) // a quota can exceed physical cores; the process still cannot
	}
	if limit < 0.01 {
		limit = 0.01
	}
	return limit
}

// memoryUsed returns current used memory in bytes, preferring cgroup accounting
// (v2 memory.current, then v1 memory.usage_in_bytes) and falling back to the
// process RSS from /proc/self/status.
func memoryUsed(readFile func(string) ([]byte, error), procRoot, cgroupRoot string) (uint64, bool) {
	if s, ok := readCgroupFile(readFile, procRoot, cgroupRoot, "", "memory.current"); ok {
		if v, err := strconv.ParseUint(s, 10, 64); err == nil {
			return v, true
		}
	}
	if s, ok := readCgroupFile(readFile, procRoot, cgroupRoot, "memory", "memory.usage_in_bytes"); ok {
		if v, err := strconv.ParseUint(s, 10, 64); err == nil {
			return v, true
		}
	}
	if data, err := readFile(procRoot + "/self/status"); err == nil {
		if v, ok := parseVmRSS(data); ok {
			return v, true
		}
	}
	return 0, false
}

// memoryLimit returns the effective memory limit in bytes. It reads the cgroup
// limit (v2 memory.max, then v1 memory.limit_in_bytes), treats the "unlimited"
// sentinels as no limit, and always caps the result at physical RAM. When the
// cgroup is unlimited it falls back to physical RAM (isPhysical=true). A returned
// limit of 0 means "no meaningful limit is known".
func memoryLimit(readFile func(string) ([]byte, error), procRoot, cgroupRoot string, physTotal uint64) (limit uint64, isPhysical bool) {
	var cgLimit uint64
	var cgUnlimited = true

	if s, ok := readCgroupFile(readFile, procRoot, cgroupRoot, "", "memory.max"); ok {
		if s == "max" {
			cgUnlimited = true
		} else if v, err := strconv.ParseUint(s, 10, 64); err == nil {
			cgLimit, cgUnlimited = v, false
		}
	} else if s, ok := readCgroupFile(readFile, procRoot, cgroupRoot, "memory", "memory.limit_in_bytes"); ok {
		if v, err := strconv.ParseUint(s, 10, 64); err == nil {
			// Treat the sentinel, zero, or anything >= physical RAM as unlimited.
			if v == cgroupV1MemUnlimited || v == 0 || (physTotal > 0 && v >= physTotal) {
				cgUnlimited = true
			} else {
				cgLimit, cgUnlimited = v, false
			}
		}
	}

	if cgUnlimited {
		return physTotal, true // 0 when physTotal is unknown -> "no meaningful limit"
	}
	if physTotal > 0 && cgLimit > physTotal {
		return physTotal, false
	}
	return cgLimit, false
}
