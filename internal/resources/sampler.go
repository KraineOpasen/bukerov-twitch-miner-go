package resources

import (
	"context"
	"os"
	"runtime"
	"sync/atomic"
	"time"
)

// sampleInterval is how often the sampler reads counters. ~5s keeps overhead
// negligible while giving rate metrics a stable delta (the dashboard polls at the
// same cadence). Not runtime-reconfigurable, so a plain ticker is used. No jitter
// is applied — jitter exists elsewhere to humanize Twitch-facing traffic; this
// loop makes no network calls and wants a deterministic cadence for clean rates.
const sampleInterval = 5 * time.Second

// Sampler periodically reads local resource counters and publishes an immutable
// Snapshot. A single goroutine (Run) owns all mutable state (baselines + history
// rings); readers call Latest, which only loads an atomically-published snapshot,
// so concurrent reads are race-free without locking the sampler's internals.
type Sampler struct {
	interval   time.Duration
	clock      func() time.Time
	readFile   func(string) ([]byte, error)
	procRoot   string
	cgroupRoot string
	numCPU     int

	latest atomic.Pointer[Snapshot]

	// Owned exclusively by the sampler goroutine (never read by Latest).
	cpuHist, memHist, netHist, diskHist *ring
	havePrev                            bool
	prevAt                              time.Time
	prevCPUTicks                        uint64
	prevCPUOK                           bool
	prevNetRx, prevNetTx                uint64
	prevNetOK                           bool
	prevDiskRead, prevDiskWrite         uint64
	prevDiskOK                          bool
}

// New builds a Sampler reading the real /proc and cgroup trees.
func New() *Sampler {
	return newAt(os.ReadFile, "/proc", "/sys/fs/cgroup", runtime.NumCPU(), time.Now, sampleInterval)
}

// newAt builds a Sampler with injectable seams (file reader, roots, CPU count,
// clock, interval) for deterministic, sleep-free tests.
func newAt(readFile func(string) ([]byte, error), procRoot, cgroupRoot string, numCPU int, clock func() time.Time, interval time.Duration) *Sampler {
	s := &Sampler{
		interval:   interval,
		clock:      clock,
		readFile:   readFile,
		procRoot:   procRoot,
		cgroupRoot: cgroupRoot,
		numCPU:     numCPU,
		cpuHist:    newRing(historyCap),
		memHist:    newRing(historyCap),
		netHist:    newRing(historyCap),
		diskHist:   newRing(historyCap),
	}
	// Publish an initial all-unavailable snapshot so Latest before the first tick
	// returns a valid N/A rather than nil.
	init := UnavailableSnapshot()
	s.latest.Store(&init)
	return s
}

// Run samples immediately (priming the baseline; that first sample leaves rate
// metrics pending) and then every interval until ctx is cancelled, at which point
// it returns — matching the miner's context-driven loop convention.
func (s *Sampler) Run(ctx context.Context) {
	s.sample(s.clock())
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sample(s.clock())
		}
	}
}

// Latest returns the most recently published snapshot. Safe for concurrent
// callers; never blocks the sampler.
func (s *Sampler) Latest() Snapshot {
	if p := s.latest.Load(); p != nil {
		return p.clone()
	}
	return UnavailableSnapshot()
}

// sample reads every source once, computes the four metrics (gauges immediately,
// rates only with a prior sample), appends one history point per series, and
// atomically publishes the resulting snapshot. Baselines are re-anchored on every
// tick — including failed/reset ones — so the next tick can recover.
func (s *Sampler) sample(now time.Time) {
	physTotal := uint64(0)
	if data, err := s.readFile(s.procRoot + "/meminfo"); err == nil {
		physTotal, _ = parseMeminfoTotal(data)
	}

	cpuTicks, cpuOK := uint64(0), false
	if data, err := s.readFile(s.procRoot + "/self/stat"); err == nil {
		cpuTicks, cpuOK = parseProcStatTicks(data)
	}
	limitCores := cpuLimitCores(s.readFile, s.procRoot, s.cgroupRoot, s.numCPU)

	memUsed, memUsedOK := memoryUsed(s.readFile, s.procRoot, s.cgroupRoot)
	memLimit, _ := memoryLimit(s.readFile, s.procRoot, s.cgroupRoot, physTotal)

	netRx, netTx, netOK := uint64(0), uint64(0), false
	if data, err := s.readFile(s.procRoot + "/net/dev"); err == nil {
		netRx, netTx, netOK = parseNetDev(data)
	}

	diskRead, diskWrite, diskOK := uint64(0), uint64(0), false
	if data, err := s.readFile(s.procRoot + "/self/io"); err == nil {
		diskRead, diskWrite, diskOK = parseSelfIO(data)
	}

	var dt float64
	if s.havePrev {
		dt = now.Sub(s.prevAt).Seconds()
	}

	snap := Snapshot{Available: true, SampledAt: now.UTC().Format(time.RFC3339)}

	// CPU (rate): ticks/sec -> cpu-seconds/sec (÷userHZ) -> % of available cores.
	cpu := CPU{LimitCores: limitCores}
	if cpuOK {
		if rate, ok := rateFrom(cpuTicks, s.prevCPUTicks, s.havePrev && s.prevCPUOK, dt); ok {
			cpu.Percent = clampPct(rate / userHZ / limitCores * 100)
			cpu.Available = true
			s.cpuHist.push(cpu.Percent, true)
		} else {
			s.cpuHist.push(0, false)
		}
	} else {
		s.cpuHist.push(0, false)
	}
	cpu.History = normalizePercent(s.cpuHist.points())
	snap.CPU = cpu

	// Memory (gauge): available on the first sample when a source reads.
	mem := Memory{}
	if memUsedOK {
		mem.Available = true
		mem.UsedBytes = memUsed
		mem.LimitBytes = memLimit
		if memLimit > 0 {
			mem.Percent = clampPct(float64(memUsed) / float64(memLimit) * 100)
			s.memHist.push(mem.Percent, true)
		} else {
			s.memHist.push(0, false) // no meaningful limit -> no bar
		}
	} else {
		s.memHist.push(0, false)
	}
	mem.History = normalizePercent(s.memHist.points())
	snap.Memory = mem

	// Network (rate): aggregate RX/TX across non-loopback interfaces.
	net := Network{}
	if netOK {
		rxRate, rxOK := rateFrom(netRx, s.prevNetRx, s.havePrev && s.prevNetOK, dt)
		txRate, txOK := rateFrom(netTx, s.prevNetTx, s.havePrev && s.prevNetOK, dt)
		if rxOK && txOK {
			net.Available = true
			net.RxBytesPerSec = rxRate
			net.TxBytesPerSec = txRate
			s.netHist.push(rxRate+txRate, true)
		} else {
			s.netHist.push(0, false)
		}
	} else {
		s.netHist.push(0, false)
	}
	net.History = normalizeWindow(s.netHist.points())
	snap.Network = net

	// Disk (rate): process block-I/O read/write.
	disk := Disk{}
	if diskOK {
		rRate, rOK := rateFrom(diskRead, s.prevDiskRead, s.havePrev && s.prevDiskOK, dt)
		wRate, wOK := rateFrom(diskWrite, s.prevDiskWrite, s.havePrev && s.prevDiskOK, dt)
		if rOK && wOK {
			disk.Available = true
			disk.ReadBytesPerSec = rRate
			disk.WriteBytesPerSec = wRate
			s.diskHist.push(rRate+wRate, true)
		} else {
			s.diskHist.push(0, false)
		}
	} else {
		s.diskHist.push(0, false)
	}
	disk.History = normalizeWindow(s.diskHist.points())
	snap.Disk = disk

	// Re-anchor baselines every tick (even on failure/reset) so recovery is one
	// tick away; a failed read stores !OK so the next rate stays pending, never a
	// bogus delta against a zero baseline.
	s.prevAt = now
	s.prevCPUTicks, s.prevCPUOK = cpuTicks, cpuOK
	s.prevNetRx, s.prevNetTx, s.prevNetOK = netRx, netTx, netOK
	s.prevDiskRead, s.prevDiskWrite, s.prevDiskOK = diskRead, diskWrite, diskOK
	s.havePrev = true

	s.latest.Store(&snap)
}

// rateFrom converts two cumulative counter reads into a per-second rate. It
// returns ok=false (rate unavailable this tick) when there is no valid previous
// read, when the wall delta is non-positive (clock step / equal timestamps), or
// when the counter went backwards (reset or 64-bit wrap — treated identically,
// never "corrected" into a huge bogus rate).
func rateFrom(cur, prev uint64, prevOK bool, dt float64) (float64, bool) {
	if !prevOK || dt <= 0 || cur < prev {
		return 0, false
	}
	return float64(cur-prev) / dt, true
}

func clampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
