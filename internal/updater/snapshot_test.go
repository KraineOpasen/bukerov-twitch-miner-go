package updater

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// New initializes the snapshot with CurrentVersion/Enabled/CheckInterval and
// the idle/none baseline - no cycle has run yet, so LastCheckAt/NextCheckAt
// stay zero.
func TestSnapshotInitial(t *testing.T) {
	u := New(Options{Repo: "owner/repo", CurrentVersion: "v1.0.0", Enabled: true, CheckInterval: time.Hour})

	snap := u.Snapshot()
	if snap.CurrentVersion != "v1.0.0" {
		t.Errorf("CurrentVersion = %q, want v1.0.0", snap.CurrentVersion)
	}
	if !snap.Enabled {
		t.Error("Enabled = false, want true")
	}
	if snap.CheckInterval != time.Hour {
		t.Errorf("CheckInterval = %v, want 1h", snap.CheckInterval)
	}
	if snap.Phase != PhaseIdle {
		t.Errorf("Phase = %q, want %q", snap.Phase, PhaseIdle)
	}
	if snap.LastOutcome != OutcomeNone {
		t.Errorf("LastOutcome = %q, want empty", snap.LastOutcome)
	}
	if !snap.LastCheckAt.IsZero() || !snap.NextCheckAt.IsZero() {
		t.Error("LastCheckAt/NextCheckAt must be zero before any cycle has run")
	}
}

// Run on a non-release (dev/dirty) build stamps PhaseDormant and returns
// immediately, never touching LastCheckAt.
func TestSnapshotDormantOnDevBuild(t *testing.T) {
	u := New(Options{Repo: "owner/repo", CurrentVersion: "dev"})
	u.Run(context.Background())

	snap := u.Snapshot()
	if snap.Phase != PhaseDormant {
		t.Errorf("Phase = %q, want %q", snap.Phase, PhaseDormant)
	}
	if !snap.LastCheckAt.IsZero() {
		t.Error("LastCheckAt must stay zero: a dormant updater never checks")
	}
}

func TestSnapshotCheckFailedOnAPIError(t *testing.T) {
	srv := newReleaseServer(t, "v2.0.0", []byte("bin"), false, 1000) // always fails
	defer srv.Close()

	u := New(Options{
		Repo: "owner/repo", CurrentVersion: "v1.0.0",
		apiBaseURL: srv.URL, httpClient: srv.Client(), retryDelay: time.Millisecond,
	})
	u.checkAndMaybeUpdate(context.Background())

	snap := u.Snapshot()
	if snap.Phase != PhaseIdle {
		t.Errorf("Phase = %q, want %q", snap.Phase, PhaseIdle)
	}
	if snap.LastOutcome != OutcomeCheckFailed {
		t.Errorf("LastOutcome = %q, want %q", snap.LastOutcome, OutcomeCheckFailed)
	}
	if snap.LastError == "" {
		t.Error("LastError empty, want the check failure recorded")
	}
}

func TestSnapshotUpToDate(t *testing.T) {
	srv := newReleaseServer(t, "v1.0.0", []byte("bin"), false, 0)
	defer srv.Close()

	u := New(Options{Repo: "owner/repo", CurrentVersion: "v1.0.0", apiBaseURL: srv.URL, httpClient: srv.Client()})
	u.checkAndMaybeUpdate(context.Background())

	snap := u.Snapshot()
	if snap.Phase != PhaseIdle || snap.LastOutcome != OutcomeUpToDate {
		t.Errorf("Phase/LastOutcome = %q/%q, want %q/%q", snap.Phase, snap.LastOutcome, PhaseIdle, OutcomeUpToDate)
	}
	if snap.LastError != "" {
		t.Errorf("LastError = %q, want cleared on up-to-date", snap.LastError)
	}
}

func TestSnapshotUpdateAvailableDisabled(t *testing.T) {
	srv := newReleaseServer(t, "v9.9.9", []byte("bin"), true, 0)
	defer srv.Close()

	u := New(Options{
		Repo: "owner/repo", CurrentVersion: "v1.0.0", Enabled: false,
		apiBaseURL: srv.URL, httpClient: srv.Client(),
	})
	u.checkAndMaybeUpdate(context.Background())

	snap := u.Snapshot()
	if snap.Phase != PhaseIdle || snap.LastOutcome != OutcomeUpdateAvailable {
		t.Errorf("Phase/LastOutcome = %q/%q, want %q/%q", snap.Phase, snap.LastOutcome, PhaseIdle, OutcomeUpdateAvailable)
	}
	if snap.LatestVersion != "v9.9.9" {
		t.Errorf("LatestVersion = %q, want v9.9.9", snap.LatestVersion)
	}
	if snap.ReleaseURL == "" {
		t.Error("ReleaseURL not stamped")
	}
}

func TestSnapshotGateBlocked(t *testing.T) {
	srv := newReleaseServer(t, "v9.9.9", []byte("bin"), true, 0)
	defer srv.Close()

	u := New(Options{
		Repo: "owner/repo", CurrentVersion: "v1.0.0", Enabled: true,
		apiBaseURL: srv.URL, httpClient: srv.Client(),
		Gate: func() bool { return false },
	})
	u.checkAndMaybeUpdate(context.Background())

	snap := u.Snapshot()
	if snap.Phase != PhaseIdle || snap.LastOutcome != OutcomeGateBlocked {
		t.Errorf("Phase/LastOutcome = %q/%q, want %q/%q", snap.Phase, snap.LastOutcome, PhaseIdle, OutcomeGateBlocked)
	}
}

func TestSnapshotApplyFailed(t *testing.T) {
	srv := newReleaseServer(t, "v9.9.9", []byte("bin"), false /* no checksums -> refuse */, 0)
	defer srv.Close()

	dir := t.TempDir()
	exec := filepath.Join(dir, "twitch-miner-go")
	_ = os.WriteFile(exec, []byte("old"), 0755)

	u := New(Options{
		Repo: "owner/repo", CurrentVersion: "v1.0.0", Enabled: true,
		apiBaseURL: srv.URL, execPath: exec, httpClient: srv.Client(),
	})
	u.checkAndMaybeUpdate(context.Background())

	snap := u.Snapshot()
	if snap.Phase != PhaseIdle || snap.LastOutcome != OutcomeApplyFailed {
		t.Errorf("Phase/LastOutcome = %q/%q, want %q/%q", snap.Phase, snap.LastOutcome, PhaseIdle, OutcomeApplyFailed)
	}
	if snap.LastError == "" {
		t.Error("LastError empty, want the apply failure recorded")
	}
}

func TestSnapshotApplied(t *testing.T) {
	binary := []byte("BRAND NEW BINARY CONTENTS")
	srv := newReleaseServer(t, "v9.9.9", binary, true, 0)
	defer srv.Close()

	dir := t.TempDir()
	exec := filepath.Join(dir, "twitch-miner-go")
	_ = os.WriteFile(exec, []byte("old"), 0755)

	u := New(Options{
		Repo: "owner/repo", CurrentVersion: "v1.0.0", Enabled: true,
		apiBaseURL: srv.URL, execPath: exec, httpClient: srv.Client(),
	})
	u.checkAndMaybeUpdate(context.Background())

	snap := u.Snapshot()
	if snap.Phase != PhaseRestartPending {
		t.Errorf("Phase = %q, want %q", snap.Phase, PhaseRestartPending)
	}
	if snap.LastOutcome != OutcomeApplied {
		t.Errorf("LastOutcome = %q, want %q", snap.LastOutcome, OutcomeApplied)
	}
	if snap.AppliedFrom != "v1.0.0" || snap.AppliedVersion != "v9.9.9" {
		t.Errorf("AppliedFrom/AppliedVersion = %q/%q, want v1.0.0/v9.9.9", snap.AppliedFrom, snap.AppliedVersion)
	}
	if snap.AppliedAt.IsZero() {
		t.Error("AppliedAt not stamped")
	}
}

// LastCheckAt/NextCheckAt are exactly CheckInterval apart, with NO jitter -
// unlike other interval-driven loops in this repo, this one must stay exact.
func TestSnapshotCheckTimesExactlyIntervalApart(t *testing.T) {
	srv := newReleaseServer(t, "v1.0.0", []byte("bin"), false, 0)
	defer srv.Close()

	fixed := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	u := New(Options{
		Repo: "owner/repo", CurrentVersion: "v1.0.0", CheckInterval: time.Hour,
		apiBaseURL: srv.URL, httpClient: srv.Client(),
	})
	u.opts.now = func() time.Time { return fixed }

	u.checkAndMaybeUpdate(context.Background())

	snap := u.Snapshot()
	if !snap.LastCheckAt.Equal(fixed) {
		t.Errorf("LastCheckAt = %v, want %v", snap.LastCheckAt, fixed)
	}
	want := fixed.Add(time.Hour)
	if !snap.NextCheckAt.Equal(want) {
		t.Errorf("NextCheckAt = %v, want %v (exactly CheckInterval apart, no jitter)", snap.NextCheckAt, want)
	}
}

func TestSnapshotLastErrorTruncation(t *testing.T) {
	longMsg := strings.Repeat("x", maxLastErrorLen+500)
	u := New(Options{Repo: "owner/repo", CurrentVersion: "v1.0.0"})
	u.setIdleOutcome(OutcomeCheckFailed, longMsg)

	got := u.Snapshot().LastError
	if len(got) != maxLastErrorLen {
		t.Fatalf("LastError length = %d, want %d (truncated)", len(got), maxLastErrorLen)
	}
	if got != longMsg[:maxLastErrorLen] {
		t.Error("truncated LastError is not a prefix of the original message")
	}
}

// Truncation must never split a multi-byte UTF-8 rune at the maxLastErrorLen
// byte boundary: a rune straddling the cut point is dropped whole, backing
// the cut off to the rune's start, rather than leaving an invalid trailing
// partial encoding.
func TestSnapshotLastErrorTruncationNeverSplitsMultiByteRune(t *testing.T) {
	// '日' (U+65E5) is a 3-byte UTF-8 encoding; placing it at
	// maxLastErrorLen-1 makes its bytes straddle exactly maxLastErrorLen -
	// the naive s[:maxLastErrorLen] cut would land on its second byte.
	prefix := strings.Repeat("x", maxLastErrorLen-1)
	longMsg := prefix + "日" + strings.Repeat("y", 100)

	u := New(Options{Repo: "owner/repo", CurrentVersion: "v1.0.0"})
	u.setIdleOutcome(OutcomeCheckFailed, longMsg)

	got := u.Snapshot().LastError
	if !utf8.ValidString(got) {
		t.Fatalf("truncated LastError is not valid UTF-8: %q", got)
	}
	if got != prefix {
		t.Errorf("LastError = %q, want exactly the ASCII prefix %q (the straddling rune dropped whole)", got, prefix)
	}
	if len(got) >= maxLastErrorLen {
		t.Errorf("truncated length = %d, want < %d (backed off from the mid-rune cut)", len(got), maxLastErrorLen)
	}
}

// Snapshot() must be race-clean against concurrent cycles running on the
// same Updater (run with -race).
func TestSnapshotConcurrentReadersDuringCycles(t *testing.T) {
	srv := newReleaseServer(t, "v1.0.0", []byte("bin"), false, 0)
	defer srv.Close()

	u := New(Options{Repo: "owner/repo", CurrentVersion: "v1.0.0", apiBaseURL: srv.URL, httpClient: srv.Client()})

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = u.Snapshot()
			}
		}
	}()

	for i := 0; i < 20; i++ {
		u.checkAndMaybeUpdate(context.Background())
	}
	close(stop)
	wg.Wait()
}
