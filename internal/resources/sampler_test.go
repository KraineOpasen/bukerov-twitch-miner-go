package resources

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

type mutFS struct {
	mu    sync.Mutex
	files map[string]string
}

func (m *mutFS) read(path string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.files[path]; ok {
		return []byte(v), nil
	}
	return nil, os.ErrNotExist
}

func (m *mutFS) set(path, val string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[path] = val
}

func netDev(rx, tx uint64) string {
	return "hdr1\nhdr2\n" +
		"    lo: 500 1 2 3 4 5 6 7 500 1 2 3 4 5 6 7\n" +
		"  eth0: " + itoa(rx) + " 1 2 3 4 5 6 7 " + itoa(tx) + " 1 2 3 4 5 6 7\n"
}

func selfIO(r, w uint64) string {
	return "rchar: 1\nwchar: 1\nread_bytes: " + itoa(r) + "\nwrite_bytes: " + itoa(w) + "\n"
}

// baseFiles is a hybrid-v1 fixture (cpu quota -1 -> NumCPU) with zeroed counters.
func baseFiles() map[string]string {
	f := v1FS()
	f["/proc/self/stat"] = procStat(0, 0)
	f["/proc/net/dev"] = netDev(0, 0)
	f["/proc/self/io"] = selfIO(0, 0)
	return f
}

func newTestSampler(files map[string]string, numCPU int) (*Sampler, *mutFS) {
	fs := &mutFS{files: files}
	return newAt(fs.read, "/proc", "/sys/fs/cgroup", numCPU, time.Now, time.Hour), fs
}

func approx(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}

func TestSamplerFirstSamplePendingRatesGaugeAvailable(t *testing.T) {
	s, fs := newTestSampler(baseFiles(), 2)
	fs.set("/proc/self/stat", procStat(1000, 0))
	s.sample(time.Unix(1000, 0))

	snap := s.Latest()
	if !snap.Available {
		t.Error("top-level available should be true once a sample ran")
	}
	if snap.CPU.Available || snap.Network.Available || snap.Disk.Available {
		t.Error("rate metrics must be pending (unavailable) on the first sample")
	}
	if snap.CPU.LimitCores != 2 {
		t.Errorf("LimitCores=%v, want 2 even while pending", snap.CPU.LimitCores)
	}
	if !snap.Memory.Available {
		t.Error("memory (gauge) must be available on the first sample")
	}
	if snap.Memory.UsedBytes != 779255808 {
		t.Errorf("mem used=%d, want 779255808", snap.Memory.UsedBytes)
	}
}

func TestSamplerCPURate(t *testing.T) {
	s, fs := newTestSampler(baseFiles(), 2)
	t0 := time.Unix(1000, 0)
	fs.set("/proc/self/stat", procStat(1000, 0))
	s.sample(t0)
	fs.set("/proc/self/stat", procStat(1100, 0)) // +100 ticks over 5s
	s.sample(t0.Add(5 * time.Second))

	snap := s.Latest()
	if !snap.CPU.Available {
		t.Fatal("CPU should be available on the second sample")
	}
	// (100 ticks / 100 Hz) cpu-sec / 5 wall-sec / 2 cores * 100 = 10%.
	if !approx(snap.CPU.Percent, 10.0) {
		t.Fatalf("CPU.Percent=%v, want 10.0", snap.CPU.Percent)
	}
}

func TestSamplerCPUNormalizedToCores(t *testing.T) {
	// Same tick delta, 4 cores -> half the percentage of the 2-core case.
	files := baseFiles()
	s, fs := newTestSampler(files, 4)
	t0 := time.Unix(0, 0)
	fs.set("/proc/self/stat", procStat(0, 0))
	s.sample(t0)
	fs.set("/proc/self/stat", procStat(100, 0))
	s.sample(t0.Add(5 * time.Second))
	if got := s.Latest().CPU.Percent; !approx(got, 5.0) {
		t.Fatalf("CPU.Percent=%v, want 5.0 (÷4 cores)", got)
	}
}

func TestSamplerNetworkAndDiskRates(t *testing.T) {
	s, fs := newTestSampler(baseFiles(), 2)
	t0 := time.Unix(0, 0)
	fs.set("/proc/net/dev", netDev(1000000, 2000000))
	fs.set("/proc/self/io", selfIO(0, 0))
	s.sample(t0)
	fs.set("/proc/net/dev", netDev(1500000, 2500000)) // +500000 each over 5s
	fs.set("/proc/self/io", selfIO(5242880, 1048576)) // read +5MiB, write +1MiB
	s.sample(t0.Add(5 * time.Second))

	snap := s.Latest()
	if !snap.Network.Available || !approx(snap.Network.RxBytesPerSec, 100000) || !approx(snap.Network.TxBytesPerSec, 100000) {
		t.Fatalf("network=%+v, want rx/tx 100000", snap.Network)
	}
	if !snap.Disk.Available || !approx(snap.Disk.ReadBytesPerSec, 1048576) || !approx(snap.Disk.WriteBytesPerSec, 209715.2) {
		t.Fatalf("disk=%+v, want read 1048576 write 209715.2", snap.Disk)
	}
}

func TestSamplerDiskZeroIsAvailableNotUnavailable(t *testing.T) {
	s, fs := newTestSampler(baseFiles(), 2)
	t0 := time.Unix(0, 0)
	fs.set("/proc/self/io", selfIO(0, 0))
	s.sample(t0)
	fs.set("/proc/self/io", selfIO(0, 0)) // genuinely no I/O
	s.sample(t0.Add(5 * time.Second))
	snap := s.Latest()
	if !snap.Disk.Available {
		t.Error("a genuine 0 B/s disk rate must be available:true, not unavailable")
	}
	if snap.Disk.ReadBytesPerSec != 0 || snap.Disk.WriteBytesPerSec != 0 {
		t.Errorf("disk rates=%+v, want 0/0", snap.Disk)
	}
}

func TestSamplerCounterReset(t *testing.T) {
	s, fs := newTestSampler(baseFiles(), 2)
	t0 := time.Unix(0, 0)
	fs.set("/proc/net/dev", netDev(5000, 5000))
	s.sample(t0)
	fs.set("/proc/net/dev", netDev(100, 100)) // counter went backwards
	s.sample(t0.Add(5 * time.Second))
	if s.Latest().Network.Available {
		t.Error("counter reset must yield pending (no negative rate)")
	}
	// Baseline moved to 100, so the next delta is against 100, not 5000.
	fs.set("/proc/net/dev", netDev(600, 600))
	s.sample(t0.Add(10 * time.Second))
	snap := s.Latest()
	if !snap.Network.Available || !approx(snap.Network.RxBytesPerSec, 100) {
		t.Fatalf("after reset rebaseline rx=%v avail=%v, want 100 true", snap.Network.RxBytesPerSec, snap.Network.Available)
	}
}

func TestSamplerCounterWrap(t *testing.T) {
	s, fs := newTestSampler(baseFiles(), 2)
	t0 := time.Unix(0, 0)
	fs.set("/proc/net/dev", netDev(1<<64-100, 1<<64-100))
	s.sample(t0)
	fs.set("/proc/net/dev", netDev(50, 50)) // wrapped
	s.sample(t0.Add(5 * time.Second))
	if s.Latest().Network.Available {
		t.Error("64-bit wrap must yield pending, never a huge bogus rate")
	}
}

func TestSamplerZeroAndNegativeDt(t *testing.T) {
	s, fs := newTestSampler(baseFiles(), 2)
	t0 := time.Unix(1000, 0)
	fs.set("/proc/net/dev", netDev(1000, 1000))
	s.sample(t0)
	fs.set("/proc/net/dev", netDev(2000, 2000))
	s.sample(t0) // identical timestamp -> dt==0
	if s.Latest().Network.Available {
		t.Error("zero dt must yield pending (no divide-by-zero)")
	}
	s.sample(t0.Add(-time.Second)) // clock stepped backward -> dt<0
	if s.Latest().Network.Available {
		t.Error("negative dt must yield pending")
	}
	// A forward tick recovers.
	fs.set("/proc/net/dev", netDev(3000, 3000))
	s.sample(t0.Add(5 * time.Second))
	if !s.Latest().Network.Available {
		t.Error("network should recover after a forward tick")
	}
}

func TestSamplerRecoveryAfterMissingSource(t *testing.T) {
	s, fs := newTestSampler(baseFiles(), 2)
	t0 := time.Unix(0, 0)
	fs.set("/proc/net/dev", netDev(1000, 1000))
	s.sample(t0)
	// Source disappears: read fails, prevNetOK becomes false.
	fs.set("/proc/net/dev", "garbage-only\n")
	s.sample(t0.Add(5 * time.Second))
	if s.Latest().Network.Available {
		t.Error("unreadable source -> pending")
	}
	// First good read after recovery is still pending (no delta vs a 0 baseline)...
	fs.set("/proc/net/dev", netDev(9000, 9000))
	s.sample(t0.Add(10 * time.Second))
	if s.Latest().Network.Available {
		t.Error("first read after recovery must be pending (no bogus delta vs zero)")
	}
	// ...the tick after that is valid.
	fs.set("/proc/net/dev", netDev(9500, 9500))
	s.sample(t0.Add(15 * time.Second))
	if got := s.Latest().Network.RxBytesPerSec; !s.Latest().Network.Available || !approx(got, 100) {
		t.Fatalf("recovered rx=%v, want 100", got)
	}
}

func TestSamplerMissingProcNoPanic(t *testing.T) {
	// Every read fails: no panic, all sections unavailable, no fabricated values.
	rd := func(string) ([]byte, error) { return nil, os.ErrNotExist }
	s := newAt(rd, "/proc", "/sys/fs/cgroup", 4, time.Now, time.Hour)
	s.sample(time.Unix(0, 0))
	s.sample(time.Unix(5, 0))
	snap := s.Latest()
	if snap.CPU.Available || snap.Memory.Available || snap.Network.Available || snap.Disk.Available {
		t.Errorf("all sections must be unavailable when /proc is missing: %+v", snap)
	}
	if snap.Memory.UsedBytes != 0 || snap.CPU.Percent != 0 {
		t.Error("unavailable sections must not fabricate values")
	}
}

func TestSamplerMemoryPercent(t *testing.T) {
	s, fs := newTestSampler(v2FS(), 4)
	fs.set("/proc/self/stat", procStat(0, 0))
	fs.set("/proc/net/dev", netDev(0, 0))
	fs.set("/proc/self/io", selfIO(0, 0))
	s.sample(time.Unix(0, 0))
	snap := s.Latest()
	// used 268435456 / limit 536870912 = 50%.
	if !snap.Memory.Available || !approx(snap.Memory.Percent, 50.0) {
		t.Fatalf("mem percent=%v avail=%v, want 50 true", snap.Memory.Percent, snap.Memory.Available)
	}
	if snap.Memory.LimitBytes != 536870912 {
		t.Errorf("mem limit=%d, want 536870912", snap.Memory.LimitBytes)
	}
}

func TestSamplerBoundedHistory(t *testing.T) {
	s, fs := newTestSampler(baseFiles(), 2)
	base := time.Unix(0, 0)
	for i := 0; i < historyCap+20; i++ {
		fs.set("/proc/self/stat", procStat(uint64(i*10), 0))
		fs.set("/proc/net/dev", netDev(uint64(i*1000), uint64(i*1000)))
		s.sample(base.Add(time.Duration(i) * 5 * time.Second))
	}
	snap := s.Latest()
	if len(snap.CPU.History) != historyCap {
		t.Errorf("CPU history len=%d, want bounded at %d", len(snap.CPU.History), historyCap)
	}
	if len(snap.Network.History) != historyCap {
		t.Errorf("Network history len=%d, want bounded at %d", len(snap.Network.History), historyCap)
	}
}

func TestSamplerLatestIsImmutableCopy(t *testing.T) {
	s, fs := newTestSampler(baseFiles(), 2)
	fs.set("/proc/self/stat", procStat(0, 0))
	s.sample(time.Unix(0, 0))
	fs.set("/proc/self/stat", procStat(100, 0))
	s.sample(time.Unix(5, 0))

	snap := s.Latest()
	if len(snap.CPU.History) == 0 {
		t.Fatal("expected history")
	}
	snap.CPU.History[0] = 999 // mutate the returned copy
	snap.CPU.Percent = -1

	again := s.Latest()
	if again.CPU.History[0] == 999 || again.CPU.Percent == -1 {
		t.Error("mutating a returned snapshot must not affect the sampler's published state")
	}
}

func TestSamplerConcurrentReadsRace(t *testing.T) {
	s, fs := newTestSampler(baseFiles(), 2)
	stop := make(chan struct{})

	// One writer, running until the readers finish.
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			fs.set("/proc/self/stat", procStat(uint64(i*10), 0))
			s.sample(time.Unix(int64(i*5), 0))
			i++
		}
	}()

	// Many readers doing a fixed amount of work.
	var readers sync.WaitGroup
	for r := 0; r < 8; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for j := 0; j < 2000; j++ {
				_ = s.Latest()
			}
		}()
	}

	readers.Wait() // readers finish their fixed work
	close(stop)    // then stop the writer
	writer.Wait()
	// -race asserts no data race between sample() (writer) and Latest() (readers).
}

func TestRunStopsOnContextCancel(t *testing.T) {
	s, _ := newTestSampler(baseFiles(), 2) // interval = time.Hour, so only ctx.Done fires
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
	// Run primed exactly one sample before blocking on the (1h) ticker.
	if s.Latest().SampledAt == "" {
		t.Error("Run should have primed one sample")
	}
}

func TestRing(t *testing.T) {
	r := newRing(3)
	for i := 1; i <= 5; i++ {
		r.push(float64(i), true)
	}
	pts := r.points()
	if len(pts) != 3 || pts[0].value != 3 || pts[2].value != 5 {
		t.Fatalf("ring kept %v, want newest [3 4 5]", pts)
	}
	// Zero-capacity ring never panics.
	z := newRing(0)
	z.push(1, true)
	if len(z.points()) != 0 {
		t.Error("zero-cap ring should stay empty")
	}
}
