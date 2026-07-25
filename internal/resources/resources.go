// Package resources samples the miner process/container's own CPU, memory,
// network and disk usage from local sources only (Go runtime, /proc/self,
// /proc/net/dev, and cgroup v2/v1) and exposes a bounded, immutable snapshot for
// the dashboard's resource mini-widgets.
//
// It is deliberately HTTP-free (like internal/analytics): the web layer owns the
// endpoint and templates. Nothing here reads the host beyond the calling
// process's own view, makes external/Twitch calls, opens a docker socket, or
// mutates miner state. Every value that leaves the process is a normalized
// number or an availability flag — never a path, hostname, IP, interface name,
// PID, cgroup path, or raw error — so the JSON DTO below is itself the privacy
// allowlist boundary.
package resources

// historyCap is the number of points kept per sparkline series (~5 minutes at a
// 5s sampling cadence). Bounded so history can never grow without limit.
const historyCap = 60

// Snapshot is the immutable, allowlisted view returned to the dashboard. Its JSON
// shape is the full public contract: only normalized numbers, per-section
// availability booleans, one UTC timestamp, and bounded normalized (0..1)
// history arrays for the micro-bars. No identifying or free-form data.
type Snapshot struct {
	Available bool    `json:"available"`
	SampledAt string  `json:"sampledAt"` // RFC3339 UTC; "" before the first sample
	CPU       CPU     `json:"cpu"`
	Memory    Memory  `json:"memory"`
	Network   Network `json:"network"`
	Disk      Disk    `json:"disk"`
}

// CPU is the process/container CPU usage. Percent is normalized to LimitCores so
// 100 means "all available cores fully busy". Available is false until a second
// sample exists (a rate needs two points) or when /proc/self/stat is unreadable.
type CPU struct {
	Available  bool      `json:"available"`
	Percent    float64   `json:"percent"`
	LimitCores float64   `json:"limitCores"`
	History    []float64 `json:"history"`
}

// Memory is the process/container memory usage. LimitBytes is 0 when no
// meaningful limit is known (unlimited cgroup and unknown physical RAM).
type Memory struct {
	Available  bool      `json:"available"`
	UsedBytes  uint64    `json:"usedBytes"`
	LimitBytes uint64    `json:"limitBytes"`
	Percent    float64   `json:"percent"`
	History    []float64 `json:"history"`
}

// Network is the aggregate receive/transmit throughput across all non-loopback
// interfaces (never a per-interface or named breakdown). History is the combined
// activity normalized to its own recent window.
type Network struct {
	Available     bool      `json:"available"`
	RxBytesPerSec float64   `json:"rxBytesPerSec"`
	TxBytesPerSec float64   `json:"txBytesPerSec"`
	History       []float64 `json:"history"`
}

// Disk is the process block-I/O read/write throughput. History is the combined
// activity normalized to its own recent window.
type Disk struct {
	Available        bool      `json:"available"`
	ReadBytesPerSec  float64   `json:"readBytesPerSec"`
	WriteBytesPerSec float64   `json:"writeBytesPerSec"`
	History          []float64 `json:"history"`
}

// clone returns a copy whose history slices are independent of the receiver, so
// a caller mutating the returned snapshot cannot affect the sampler's published
// state or another concurrent reader.
func (s Snapshot) clone() Snapshot {
	s.CPU.History = cloneFloats(s.CPU.History)
	s.Memory.History = cloneFloats(s.Memory.History)
	s.Network.History = cloneFloats(s.Network.History)
	s.Disk.History = cloneFloats(s.Disk.History)
	return s
}

func cloneFloats(in []float64) []float64 {
	out := make([]float64, len(in))
	copy(out, in)
	return out
}

// UnavailableSnapshot returns a fully-typed snapshot with every section marked
// unavailable and empty (non-nil) history slices. It is what the endpoint serves
// when no sampler is wired yet or a provider fails — a graceful, honest "N/A"
// rather than a 404, a 500, or a misleading zero.
func UnavailableSnapshot() Snapshot {
	return Snapshot{
		CPU:     CPU{History: []float64{}},
		Memory:  Memory{History: []float64{}},
		Network: Network{History: []float64{}},
		Disk:    Disk{History: []float64{}},
	}
}

// histPoint is one sampled value plus whether it was valid at that tick. Invalid
// points (first sample, counter reset, unreadable source) keep the sparkline's
// x-axis aligned but render as a baseline gap.
type histPoint struct {
	value float64
	valid bool
}

// ring is a fixed-capacity buffer that keeps the newest points, oldest evicted.
type ring struct {
	buf  []histPoint
	head int // next write index; also the oldest when full
	size int
	cap  int
}

func newRing(capacity int) *ring {
	if capacity < 0 {
		capacity = 0
	}
	return &ring{buf: make([]histPoint, capacity), cap: capacity}
}

// push appends a point, overwriting the oldest once full. A zero-capacity ring
// is a no-op (never panics).
func (r *ring) push(value float64, valid bool) {
	if r.cap == 0 {
		return
	}
	r.buf[r.head] = histPoint{value: value, valid: valid}
	r.head = (r.head + 1) % r.cap
	if r.size < r.cap {
		r.size++
	}
}

// points returns a copy of the buffered points, oldest to newest.
func (r *ring) points() []histPoint {
	if r.size == 0 { // also covers cap==0 (no modulo, no divide-by-zero)
		return nil
	}
	out := make([]histPoint, r.size)
	start := (r.head - r.size + r.cap) % r.cap
	for i := 0; i < r.size; i++ {
		out[i] = r.buf[(start+i)%r.cap]
	}
	return out
}

// normalizePercent maps percent points (0..100) to bar heights (0..1) on a fixed
// scale, so CPU/memory bars are comparable across time. Invalid points render as
// a zero-height gap.
func normalizePercent(points []histPoint) []float64 {
	out := make([]float64, len(points))
	for i, p := range points {
		if !p.valid {
			continue
		}
		out[i] = clamp01(p.value / 100)
	}
	return out
}

// normalizeWindow maps unbounded rate points to bar heights (0..1) relative to
// the largest value in the current window, so network/disk sparklines scale to
// recent activity. Invalid points render as a zero-height gap.
func normalizeWindow(points []histPoint) []float64 {
	out := make([]float64, len(points))
	var max float64
	for _, p := range points {
		if p.valid && p.value > max {
			max = p.value
		}
	}
	if max <= 0 {
		return out
	}
	for i, p := range points {
		if !p.valid {
			continue
		}
		out[i] = clamp01(p.value / max)
	}
	return out
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
