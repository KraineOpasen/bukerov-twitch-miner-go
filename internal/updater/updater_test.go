package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
)

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
		ok   bool
	}{
		{"v1.2.3", "v1.2.4", -1, true},
		{"1.2.3", "1.2.3", 0, true},
		{"v2.0.0", "v1.9.9", 1, true},
		{"v1.2.3", "1.2.3", 0, true},   // leading v is optional/mixed
		{"v1.10.0", "v1.9.0", 1, true}, // numeric, not lexical, comparison
		{"v0.1.0", "v0.1.1", -1, true},
		{"v1.2.3-rc.1", "v1.2.3", -1, true},      // pre-release < release
		{"v1.2.3", "v1.2.3-rc.1", 1, true},       // release > pre-release
		{"v1.2.3-rc.1", "v1.2.3-rc.2", -1, true}, // numeric pre-release fields
		{"v1.2.3-rc.2", "v1.2.3-rc.10", -1, true},
		{"v1.2.3-alpha", "v1.2.3-beta", -1, true},     // lexical pre-release fields
		{"v1.2.3+build.9", "v1.2.3+build.1", 0, true}, // build metadata ignored
		{"dev", "v1.2.3", 0, false},                   // unparseable
		{"v1.2", "v1.2.0", 0, false},                  // not a full triple
		{"", "v1.0.0", 0, false},
	}

	for _, tt := range tests {
		got, ok := compareVersions(tt.a, tt.b)
		if ok != tt.ok {
			t.Errorf("compareVersions(%q, %q) ok = %v, want %v", tt.a, tt.b, ok, tt.ok)
			continue
		}
		if ok && got != tt.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestIsReleaseVersion(t *testing.T) {
	tests := []struct {
		v    string
		want bool
	}{
		{"v1.2.3", true},
		{"1.0.0", true},
		{"dev", false},
		{"v1.2.3-4-gabcdef", false}, // git describe of a dev checkout
		{"v1.2.3-rc.1", false},      // pre-release
		{"ci-abcdef", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isReleaseVersion(tt.v); got != tt.want {
			t.Errorf("isReleaseVersion(%q) = %v, want %v", tt.v, got, tt.want)
		}
	}
}

func TestVersionsEqual(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"v1.2.3", "1.2.3", true}, // ldflags value vs release tag
		{"1.2.3", "1.2.3", true},
		{"v1.2.3", "v1.2.4", false},
		{"dev", "1.2.3", false}, // unparseable -> never equal
		{"", "1.0.0", false},
		{"v1.2.3+build.9", "1.2.3+build.1", true}, // build metadata ignored
	}
	for _, tt := range tests {
		if got := VersionsEqual(tt.a, tt.b); got != tt.want {
			t.Errorf("VersionsEqual(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestParseCheckInterval(t *testing.T) {
	if DefaultCheckInterval != 2*time.Hour {
		t.Fatalf("DefaultCheckInterval = %s, want production cadence 2h", DefaultCheckInterval)
	}
	tests := []struct {
		raw  string
		want time.Duration
	}{
		{"", DefaultCheckInterval},
		{"6h", 6 * time.Hour},
		{"6h30m", 6*time.Hour + 30*time.Minute},
		{"12", 12 * time.Hour},   // bare number = hours
		{"1m", minCheckInterval}, // below the floor -> clamped
		{"garbage", DefaultCheckInterval},
	}
	for _, tt := range tests {
		if got := ParseCheckInterval(tt.raw); got != tt.want {
			t.Errorf("ParseCheckInterval(%q) = %v, want %v", tt.raw, got, tt.want)
		}
	}
}

func TestStableChannelChecksAtStartup(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/repos/owner/repo/releases" {
			t.Errorf("request path = %q, want paginated releases collection", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]release{completeStableRelease("stable-v0.1.0")})
	}))
	defer srv.Close()

	u := New(Options{
		Repo: "owner/repo", CurrentVersion: "0.1.0", ReleaseChannel: "stable", Enabled: true,
		apiBaseURL: srv.URL, httpClient: srv.Client(),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	u.Run(ctx)

	if got := requests.Load(); got != 1 {
		t.Fatalf("startup HTTP requests = %d, want 1", got)
	}
	if got := u.Snapshot().LastOutcome; got != OutcomeUpToDate {
		t.Fatalf("startup outcome = %q, want %q", got, OutcomeUpToDate)
	}
}

func TestAssetName(t *testing.T) {
	got := assetName()
	want := fmt.Sprintf("twitch-miner-go-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		want += ".exe"
	}
	if got != want {
		t.Errorf("assetName() = %q, want %q", got, want)
	}
}

func TestChecksumFor(t *testing.T) {
	body := "abc123  twitch-miner-go-linux-amd64\n" +
		"def456 *twitch-miner-go-linux-arm64\n" +
		"\n" +
		"malformed line here\n"

	if sum, ok := checksumFor(body, "twitch-miner-go-linux-amd64"); !ok || sum != "abc123" {
		t.Errorf("checksumFor amd64 = %q, %v; want abc123, true", sum, ok)
	}
	if sum, ok := checksumFor(body, "twitch-miner-go-linux-arm64"); !ok || sum != "def456" {
		t.Errorf("checksumFor arm64 (binary-mode '*') = %q, %v; want def456, true", sum, ok)
	}
	if _, ok := checksumFor(body, "missing"); ok {
		t.Error("checksumFor missing asset returned ok=true")
	}
}

func TestReplaceExecutableAtomic(t *testing.T) {
	dir := t.TempDir()
	exec := filepath.Join(dir, "twitch-miner-go")
	if err := os.WriteFile(exec, []byte("OLD BINARY"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := replaceExecutable(exec, []byte("NEW BINARY")); err != nil {
		t.Fatalf("replaceExecutable: %v", err)
	}

	got, err := os.ReadFile(exec)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "NEW BINARY" {
		t.Errorf("binary content = %q, want %q", got, "NEW BINARY")
	}

	// No temp files should be left behind.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("expected exactly 1 file after replace, found %d: %v", len(entries), entries)
	}
}

func TestReplaceExecutableWriteError(t *testing.T) {
	// A path whose parent directory does not exist makes the temp-file
	// creation fail regardless of the running user (root included), so this
	// asserts the "read-only / unwritable filesystem" branch reliably.
	bad := filepath.Join(t.TempDir(), "does-not-exist", "twitch-miner-go")
	if err := replaceExecutable(bad, []byte("data")); err == nil {
		t.Fatal("expected an error replacing into a non-existent directory, got nil")
	}
}

// buildRelease serves the current platform's asset and checksums from srv.
func newReleaseServer(t *testing.T, tag string, binary []byte, withChecksums bool, failAPITimes int) *httptest.Server {
	t.Helper()

	sum := sha256.Sum256(binary)
	checksums := hex.EncodeToString(sum[:]) + "  " + assetName() + "\n"

	var apiCalls int32
	mux := http.NewServeMux()

	mux.HandleFunc("/repos/owner/repo/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		if n := atomic.AddInt32(&apiCalls, 1); int(n) <= failAPITimes {
			http.Error(w, "transient", http.StatusInternalServerError)
			return
		}
		rel := release{
			TagName: tag,
			HTMLURL: "https://example.test/releases/" + tag,
			Assets: []asset{
				{Name: assetName(), URL: srvURL(r) + "/download/binary"},
			},
		}
		if withChecksums {
			rel.Assets = append(rel.Assets, asset{Name: "checksums.txt", URL: srvURL(r) + "/download/checksums"})
		}
		_ = json.NewEncoder(w).Encode(rel)
	})
	mux.HandleFunc("/download/binary", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(binary)
	})
	mux.HandleFunc("/download/checksums", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(checksums))
	})

	return httptest.NewServer(mux)
}

func srvURL(r *http.Request) string {
	return "http://" + r.Host
}

func TestApplyUpdateSuccess(t *testing.T) {
	binary := []byte("BRAND NEW BINARY CONTENTS")
	srv := newReleaseServer(t, "v9.9.9", binary, true, 0)
	defer srv.Close()

	dir := t.TempDir()
	exec := filepath.Join(dir, "twitch-miner-go")
	if err := os.WriteFile(exec, []byte("old"), 0755); err != nil {
		t.Fatal(err)
	}

	u := New(Options{
		Repo:           "owner/repo",
		CurrentVersion: "v1.0.0",
		Enabled:        true,
		apiBaseURL:     srv.URL,
		execPath:       exec,
		httpClient:     srv.Client(),
	})

	rel, err := u.latestRelease(context.Background())
	if err != nil {
		t.Fatalf("latestRelease: %v", err)
	}
	if err := u.applyUpdate(context.Background(), rel); err != nil {
		t.Fatalf("applyUpdate: %v", err)
	}

	got, _ := os.ReadFile(exec)
	if string(got) != string(binary) {
		t.Errorf("binary not replaced: got %q", got)
	}
}

func TestApplyUpdateChecksumMismatch(t *testing.T) {
	srv := newReleaseServer(t, "v9.9.9", []byte("real binary"), true, 0)
	defer srv.Close()

	// Point the checksums endpoint at a wrong hash by wrapping the server.
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		rel := release{
			TagName: "v9.9.9",
			Assets: []asset{
				{Name: assetName(), URL: srvURL(r) + "/bin"},
				{Name: "checksums.txt", URL: srvURL(r) + "/sums"},
			},
		}
		_ = json.NewEncoder(w).Encode(rel)
	})
	mux.HandleFunc("/bin", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("real binary")) })
	mux.HandleFunc("/sums", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("deadbeef  " + assetName() + "\n"))
	})
	srv2 := httptest.NewServer(mux)
	defer srv2.Close()

	dir := t.TempDir()
	exec := filepath.Join(dir, "twitch-miner-go")
	_ = os.WriteFile(exec, []byte("old"), 0755)

	u := New(Options{
		Repo: "owner/repo", CurrentVersion: "v1.0.0", Enabled: true,
		apiBaseURL: srv2.URL, execPath: exec, httpClient: srv2.Client(),
	})
	rel, err := u.latestRelease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := u.applyUpdate(context.Background(), rel); err == nil {
		t.Fatal("expected checksum mismatch error, got nil")
	}
	// The original binary must be untouched after a rejected update.
	if got, _ := os.ReadFile(exec); string(got) != "old" {
		t.Errorf("binary was modified despite checksum mismatch: %q", got)
	}
}

func TestLatestReleaseRetries(t *testing.T) {
	// Fail the API twice, succeed on the third attempt.
	srv := newReleaseServer(t, "v2.0.0", []byte("bin"), false, 2)
	defer srv.Close()

	u := New(Options{
		Repo: "owner/repo", CurrentVersion: "v1.0.0",
		apiBaseURL: srv.URL, httpClient: srv.Client(),
		retryDelay: time.Millisecond,
	})

	rel, err := u.latestRelease(context.Background())
	if err != nil {
		t.Fatalf("latestRelease after retries: %v", err)
	}
	if rel.TagName != "v2.0.0" {
		t.Errorf("tag = %q, want v2.0.0", rel.TagName)
	}
}

func TestLatestReleaseGivesUp(t *testing.T) {
	// Always fail: after maxAttempts, an error is returned (not a panic/crash).
	srv := newReleaseServer(t, "v2.0.0", []byte("bin"), false, 1000)
	defer srv.Close()

	u := New(Options{
		Repo: "owner/repo", CurrentVersion: "v1.0.0",
		apiBaseURL: srv.URL, httpClient: srv.Client(),
		retryDelay: time.Millisecond,
	})

	if _, err := u.latestRelease(context.Background()); err == nil {
		t.Fatal("expected an error after exhausting retries, got nil")
	}
}

func TestCheckAndMaybeUpdateWriteErrorDoesNotRestart(t *testing.T) {
	srv := newReleaseServer(t, "v9.9.9", []byte("new binary"), true, 0)
	defer srv.Close()

	restarted := false
	// Unwritable target: parent directory does not exist.
	badExec := filepath.Join(t.TempDir(), "missing-dir", "twitch-miner-go")

	u := New(Options{
		Repo: "owner/repo", CurrentVersion: "v1.0.0", Enabled: true,
		apiBaseURL: srv.URL, execPath: badExec, httpClient: srv.Client(),
		OnUpdate: func() { restarted = true },
	})

	// Must not panic and must not signal a restart when the swap fails.
	u.checkAndMaybeUpdate(context.Background())

	if restarted {
		t.Error("OnUpdate was called even though the binary swap failed")
	}
}

func TestCheckAndMaybeUpdateDisabledNotifiesOnly(t *testing.T) {
	srv := newReleaseServer(t, "v9.9.9", []byte("new binary"), true, 0)
	defer srv.Close()

	dir := t.TempDir()
	exec := filepath.Join(dir, "twitch-miner-go")
	_ = os.WriteFile(exec, []byte("original"), 0755)

	var notifiedCurrent, notifiedLatest string
	notifyCount := 0
	restarted := false

	u := New(Options{
		Repo: "owner/repo", CurrentVersion: "v1.0.0",
		Enabled:    false, // notify/log only
		apiBaseURL: srv.URL, execPath: exec, httpClient: srv.Client(),
		Notify: func(cur, latest, url string) {
			notifyCount++
			notifiedCurrent, notifiedLatest = cur, latest
		},
		OnUpdate: func() { restarted = true },
	})

	u.checkAndMaybeUpdate(context.Background())
	// Second cycle: already notified for this version, should not re-notify.
	u.checkAndMaybeUpdate(context.Background())

	if notifyCount != 1 {
		t.Errorf("Notify called %d times, want exactly 1 (deduped per version)", notifyCount)
	}
	if notifiedCurrent != "v1.0.0" || notifiedLatest != "v9.9.9" {
		t.Errorf("Notify args = (%q, %q), want (v1.0.0, v9.9.9)", notifiedCurrent, notifiedLatest)
	}
	if restarted {
		t.Error("OnUpdate called while auto-update disabled")
	}
	if got, _ := os.ReadFile(exec); string(got) != "original" {
		t.Errorf("binary replaced while disabled: %q", got)
	}
}

func TestCheckAndMaybeUpdateUpToDate(t *testing.T) {
	srv := newReleaseServer(t, "v1.0.0", []byte("bin"), false, 0)
	defer srv.Close()

	restarted := false
	notified := false
	u := New(Options{
		Repo: "owner/repo", CurrentVersion: "v1.0.0", Enabled: true,
		apiBaseURL: srv.URL, httpClient: srv.Client(),
		Notify:   func(_, _, _ string) { notified = true },
		OnUpdate: func() { restarted = true },
	})

	u.checkAndMaybeUpdate(context.Background())

	if notified {
		t.Error("Notify called when already up to date")
	}
	if restarted {
		t.Error("OnUpdate called when already up to date")
	}
}

// --- Fail-closed checksum verification (Stage B hardening) ---

// A release WITHOUT checksums.txt must be refused, not installed unverified.
func TestApplyUpdateRefusedWithoutChecksums(t *testing.T) {
	srv := newReleaseServer(t, "v9.9.9", []byte("new binary"), false /* no checksums.txt */, 0)
	defer srv.Close()

	dir := t.TempDir()
	exec := filepath.Join(dir, "twitch-miner-go")
	_ = os.WriteFile(exec, []byte("old"), 0755)

	u := New(Options{
		Repo: "owner/repo", CurrentVersion: "v1.0.0", Enabled: true,
		apiBaseURL: srv.URL, execPath: exec, httpClient: srv.Client(),
	})
	rel, err := u.latestRelease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = u.applyUpdate(context.Background(), rel)
	if err == nil {
		t.Fatal("expected fail-closed error for a release without checksums.txt, got nil")
	}
	if !strings.Contains(err.Error(), "refusing to install unverified binary") {
		t.Errorf("error should state the fail-closed refusal, got: %v", err)
	}
	if got, _ := os.ReadFile(exec); string(got) != "old" {
		t.Errorf("binary was replaced despite missing checksums.txt: %q", got)
	}
}

// A checksums.txt that exists but cannot be downloaded must also refuse.
func TestApplyUpdateRefusedWhenChecksumsFetchFails(t *testing.T) {
	binary := []byte("new binary")
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		rel := release{
			TagName: "v9.9.9",
			Assets: []asset{
				{Name: assetName(), URL: srvURL(r) + "/bin"},
				{Name: "checksums.txt", URL: srvURL(r) + "/sums"},
			},
		}
		_ = json.NewEncoder(w).Encode(rel)
	})
	mux.HandleFunc("/bin", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(binary) })
	mux.HandleFunc("/sums", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	exec := filepath.Join(dir, "twitch-miner-go")
	_ = os.WriteFile(exec, []byte("old"), 0755)

	u := New(Options{
		Repo: "owner/repo", CurrentVersion: "v1.0.0", Enabled: true,
		apiBaseURL: srv.URL, execPath: exec, httpClient: srv.Client(),
	})
	rel, err := u.latestRelease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := u.applyUpdate(context.Background(), rel); err == nil {
		t.Fatal("expected fail-closed error when checksums.txt cannot be fetched, got nil")
	}
	if got, _ := os.ReadFile(exec); string(got) != "old" {
		t.Errorf("binary was replaced despite unfetchable checksums.txt: %q", got)
	}
}

// A checksums.txt without an entry for this platform's asset must refuse.
func TestApplyUpdateRefusedWhenChecksumEntryMissing(t *testing.T) {
	binary := []byte("new binary")
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		rel := release{
			TagName: "v9.9.9",
			Assets: []asset{
				{Name: assetName(), URL: srvURL(r) + "/bin"},
				{Name: "checksums.txt", URL: srvURL(r) + "/sums"},
			},
		}
		_ = json.NewEncoder(w).Encode(rel)
	})
	mux.HandleFunc("/bin", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(binary) })
	mux.HandleFunc("/sums", func(w http.ResponseWriter, r *http.Request) {
		// Valid file, but for a different asset name.
		_, _ = w.Write([]byte("deadbeef  some-other-asset\n"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	exec := filepath.Join(dir, "twitch-miner-go")
	_ = os.WriteFile(exec, []byte("old"), 0755)

	u := New(Options{
		Repo: "owner/repo", CurrentVersion: "v1.0.0", Enabled: true,
		apiBaseURL: srv.URL, execPath: exec, httpClient: srv.Client(),
	})
	rel, err := u.latestRelease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := u.applyUpdate(context.Background(), rel); err == nil {
		t.Fatal("expected fail-closed error when checksums.txt lacks the asset entry, got nil")
	}
	if got, _ := os.ReadFile(exec); string(got) != "old" {
		t.Errorf("binary was replaced despite missing checksum entry: %q", got)
	}
}

// A failed install surfaces via NotifyFailure exactly once per version.
func TestCheckAndMaybeUpdateNotifiesFailureOnce(t *testing.T) {
	srv := newReleaseServer(t, "v9.9.9", []byte("new binary"), false /* no checksums -> refuse */, 0)
	defer srv.Close()

	dir := t.TempDir()
	exec := filepath.Join(dir, "twitch-miner-go")
	_ = os.WriteFile(exec, []byte("old"), 0755)

	failures := 0
	var failReason string
	restarted := false

	u := New(Options{
		Repo: "owner/repo", CurrentVersion: "v1.0.0", Enabled: true,
		apiBaseURL: srv.URL, execPath: exec, httpClient: srv.Client(),
		NotifyFailure: func(cur, latest, reason string) {
			failures++
			failReason = reason
		},
		OnUpdate: func() { restarted = true },
	})

	u.checkAndMaybeUpdate(context.Background())
	// Second cycle: same broken version, must not re-notify.
	u.checkAndMaybeUpdate(context.Background())

	if failures != 1 {
		t.Errorf("NotifyFailure called %d times, want exactly 1 (deduped per version)", failures)
	}
	if !strings.Contains(failReason, "refusing to install unverified binary") {
		t.Errorf("failure reason should carry the refusal, got: %q", failReason)
	}
	if restarted {
		t.Error("OnUpdate called even though the update was refused")
	}
	if got, _ := os.ReadFile(exec); string(got) != "old" {
		t.Errorf("binary replaced despite refusal: %q", got)
	}
}

// --- Options.Gate (process-lifecycle interlock, contract §10/§15 items 35-39) ---

// A nil Gate must preserve pre-Gate behavior: Enabled alone decides, and the
// update is applied exactly as before Gate existed.
func TestGateNilPreservesEnabledOnlyBehavior(t *testing.T) {
	binary := []byte("BRAND NEW BINARY CONTENTS")
	srv := newReleaseServer(t, "v9.9.9", binary, true, 0)
	defer srv.Close()

	dir := t.TempDir()
	exec := filepath.Join(dir, "twitch-miner-go")
	_ = os.WriteFile(exec, []byte("old"), 0755)

	restarted := false
	u := New(Options{
		Repo: "owner/repo", CurrentVersion: "v1.0.0", Enabled: true,
		apiBaseURL: srv.URL, execPath: exec, httpClient: srv.Client(),
		// Gate intentionally left nil.
		OnUpdate: func() { restarted = true },
	})

	u.checkAndMaybeUpdate(context.Background())

	if !restarted {
		t.Error("OnUpdate not called: nil Gate should not block an Enabled apply")
	}
	if got, _ := os.ReadFile(exec); string(got) != string(binary) {
		t.Errorf("binary not replaced with nil Gate: %q", got)
	}
}

// Gate is consulted fresh on every cycle, not cached: flipping it between
// cycles must change the outcome (mutation-probe target: a static/cached
// Gate read would make this test fail).
func TestGateReevaluatedEveryCycle(t *testing.T) {
	binary := []byte("BRAND NEW BINARY CONTENTS")
	srv := newReleaseServer(t, "v9.9.9", binary, true, 0)
	defer srv.Close()

	dir := t.TempDir()
	exec := filepath.Join(dir, "twitch-miner-go")
	_ = os.WriteFile(exec, []byte("old"), 0755)

	var gateOpen atomic.Bool
	restarts := 0

	u := New(Options{
		Repo: "owner/repo", CurrentVersion: "v1.0.0", Enabled: true,
		apiBaseURL: srv.URL, execPath: exec, httpClient: srv.Client(),
		Gate:     func() bool { return gateOpen.Load() },
		OnUpdate: func() { restarts++ },
	})

	// Cycle 1: gate closed -> must not apply.
	u.checkAndMaybeUpdate(context.Background())
	if restarts != 0 {
		t.Fatalf("OnUpdate called with gate closed on cycle 1")
	}
	if got, _ := os.ReadFile(exec); string(got) != "old" {
		t.Fatalf("binary replaced with gate closed on cycle 1: %q", got)
	}

	// Flip the gate open, then re-check: the SAME Updater must now apply,
	// proving Gate is re-read every cycle rather than cached from cycle 1.
	gateOpen.Store(true)
	u.checkAndMaybeUpdate(context.Background())
	if restarts != 1 {
		t.Fatalf("OnUpdate called %d times after gate opened on cycle 2, want 1", restarts)
	}
	if got, _ := os.ReadFile(exec); string(got) != string(binary) {
		t.Errorf("binary not replaced after gate opened: %q", got)
	}
}

// Gate() == true (the running/paused matrix cells) allows the apply to proceed.
func TestGateTrueAllowsApply(t *testing.T) {
	binary := []byte("BRAND NEW BINARY CONTENTS")
	srv := newReleaseServer(t, "v9.9.9", binary, true, 0)
	defer srv.Close()

	dir := t.TempDir()
	exec := filepath.Join(dir, "twitch-miner-go")
	_ = os.WriteFile(exec, []byte("old"), 0755)

	restarted := false
	u := New(Options{
		Repo: "owner/repo", CurrentVersion: "v1.0.0", Enabled: true,
		apiBaseURL: srv.URL, execPath: exec, httpClient: srv.Client(),
		Gate:     func() bool { return true },
		OnUpdate: func() { restarted = true },
	})

	u.checkAndMaybeUpdate(context.Background())

	if !restarted {
		t.Error("OnUpdate not called with Gate()==true")
	}
	if got, _ := os.ReadFile(exec); string(got) != string(binary) {
		t.Errorf("binary not replaced with Gate()==true: %q", got)
	}
}

// Gate() == false (the stopped matrix cell) blocks the apply, but the
// existing per-version Notify dedup still fires exactly once - "check +
// notify only" per §7's matrix.
func TestGateFalseBlocksApplyButNotifiesOnce(t *testing.T) {
	srv := newReleaseServer(t, "v9.9.9", []byte("new binary"), true, 0)
	defer srv.Close()

	dir := t.TempDir()
	exec := filepath.Join(dir, "twitch-miner-go")
	_ = os.WriteFile(exec, []byte("old"), 0755)

	notifyCount := 0
	restarted := false

	u := New(Options{
		Repo: "owner/repo", CurrentVersion: "v1.0.0", Enabled: true,
		apiBaseURL: srv.URL, execPath: exec, httpClient: srv.Client(),
		Gate:     func() bool { return false },
		Notify:   func(_, _, _ string) { notifyCount++ },
		OnUpdate: func() { restarted = true },
	})

	u.checkAndMaybeUpdate(context.Background())
	// Second cycle: same version, Notify must stay deduped.
	u.checkAndMaybeUpdate(context.Background())

	if notifyCount != 1 {
		t.Errorf("Notify called %d times with Gate()==false, want exactly 1 (deduped per version)", notifyCount)
	}
	if restarted {
		t.Error("OnUpdate called despite Gate()==false")
	}
	if got, _ := os.ReadFile(exec); string(got) != "old" {
		t.Errorf("binary replaced despite Gate()==false: %q", got)
	}
}

// The gate-blocked signal is deduped by version exactly like Notify/
// NotifyFailure: repeated cycles on the same withheld version fire the
// callback once, and a genuinely new version fires exactly one more time.
func TestGateBlockedDedupedByVersionNewVersionRetriggers(t *testing.T) {
	var tag atomic.Value
	tag.Store("v9.9.9")
	binary := []byte("bin")
	sum := sha256.Sum256(binary)
	checksums := hex.EncodeToString(sum[:]) + "  " + assetName() + "\n"

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		tn := tag.Load().(string)
		rel := release{
			TagName: tn,
			HTMLURL: "https://example.test/releases/" + tn,
			Assets: []asset{
				{Name: assetName(), URL: srvURL(r) + "/download/binary"},
				{Name: "checksums.txt", URL: srvURL(r) + "/download/checksums"},
			},
		}
		_ = json.NewEncoder(w).Encode(rel)
	})
	mux.HandleFunc("/download/binary", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(binary) })
	mux.HandleFunc("/download/checksums", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(checksums))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	blockedCalls := 0
	var lastCurrent, lastLatest string

	u := New(Options{
		Repo: "owner/repo", CurrentVersion: "v1.0.0", Enabled: true,
		apiBaseURL: srv.URL, httpClient: srv.Client(),
		Gate: func() bool { return false },
	})
	// Test seam: override the production ring-event recorder with a plain,
	// deterministic callback so this test does not need to assert against
	// the process-global events ring from a parallel-unsafe position.
	u.opts.onGateBlocked = func(current, latest string) {
		blockedCalls++
		lastCurrent, lastLatest = current, latest
	}

	u.checkAndMaybeUpdate(context.Background())
	u.checkAndMaybeUpdate(context.Background())
	if blockedCalls != 1 {
		t.Fatalf("onGateBlocked called %d times for the same version, want exactly 1", blockedCalls)
	}
	if lastCurrent != "v1.0.0" || lastLatest != "v9.9.9" {
		t.Errorf("onGateBlocked args = (%q, %q), want (v1.0.0, v9.9.9)", lastCurrent, lastLatest)
	}

	// A genuinely new latest version must retrigger exactly one more call.
	tag.Store("v9.9.10")
	u.checkAndMaybeUpdate(context.Background())
	if blockedCalls != 2 {
		t.Fatalf("onGateBlocked called %d times after a new version appeared, want exactly 2", blockedCalls)
	}
	if lastCurrent != "v1.0.0" || lastLatest != "v9.9.10" {
		t.Errorf("onGateBlocked args after new version = (%q, %q), want (v1.0.0, v9.9.10)", lastCurrent, lastLatest)
	}
}

// gateBlockedRingMarkerSeq gives each TestGateBlockedDefaultRecordsRingEvent
// invocation (including repeats under `go test -count=N`, which reruns test
// functions within the same process) a distinct "latest version" so the
// assertion below can find the exact event this call produced in the
// process-wide events ring, rather than a stale one from an earlier run.
var gateBlockedRingMarkerSeq atomic.Int64

// The default (production) onGateBlocked records the canonical
// lifecycle_updater_gate_blocked ring event, deduped by version, without a
// test override - proving the wiring in New() actually installs it.
func TestGateBlockedDefaultRecordsRingEvent(t *testing.T) {
	n := gateBlockedRingMarkerSeq.Add(1)
	latest := fmt.Sprintf("v1.0.%d", 900000+n) // unique, valid semver, > v1.0.0

	srv := newReleaseServer(t, latest, []byte("new binary"), true, 0)
	defer srv.Close()

	u := New(Options{
		Repo: "owner/repo", CurrentVersion: "v1.0.0", Enabled: true,
		apiBaseURL: srv.URL, httpClient: srv.Client(),
		Gate: func() bool { return false },
	})

	u.checkAndMaybeUpdate(context.Background())

	found := false
	for _, e := range events.Recent(200) {
		if e.Type == events.TypeLifecycleUpdaterGateBlocked && strings.Contains(e.Detail, latest) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("default onGateBlocked did not record a lifecycle_updater_gate_blocked ring event for %s", latest)
	}
}

// --- Options.Handoff (durable updater core, design Ф5a1) -------------------

// orderedRecorder is a shared, ordered, concurrency-safe event log the
// Handoff tests below use to assert RELATIVE ordering between spyHandoff
// calls and other events (an HTTP hit, OnUpdate firing) recorded into the
// SAME log.
type orderedRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *orderedRecorder) record(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, s)
}

func (r *orderedRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	copy(out, r.events)
	return out
}

// spyHandoff is a test Handoff: it counts calls, can be made to fail on
// command, and appends into a shared orderedRecorder so ordering can be
// asserted against other recorded events.
type spyHandoff struct {
	rec *orderedRecorder

	mu                  sync.Mutex
	recordApplyingCalls int
	recordAppliedCalls  int
	clearCalls          int
	recordApplyingErr   error
	recordAppliedErr    error
	clearErr            error
}

func newSpyHandoff(rec *orderedRecorder) *spyHandoff {
	return &spyHandoff{rec: rec}
}

func (s *spyHandoff) RecordApplying(_ context.Context, _, _, _ string) error {
	s.mu.Lock()
	s.recordApplyingCalls++
	err := s.recordApplyingErr
	s.mu.Unlock()
	s.rec.record("record_applying")
	return err
}

func (s *spyHandoff) RecordApplied(_ context.Context, _, _ string) error {
	s.mu.Lock()
	s.recordAppliedCalls++
	err := s.recordAppliedErr
	s.mu.Unlock()
	s.rec.record("record_applied")
	return err
}

func (s *spyHandoff) Clear(_ context.Context) error {
	s.mu.Lock()
	s.clearCalls++
	err := s.clearErr
	s.mu.Unlock()
	s.rec.record("clear")
	return err
}

func (s *spyHandoff) counts() (applying, applied, clear int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordApplyingCalls, s.recordAppliedCalls, s.clearCalls
}

// RecordApplying is called before applyUpdate ever downloads the asset - the
// INTENT write must land before the first download byte, so a crash during
// download is still classifiable at the next boot.
func TestHandoffRecordApplyingCalledBeforeDownload(t *testing.T) {
	binary := []byte("BRAND NEW BINARY CONTENTS")
	rec := &orderedRecorder{}

	sum := sha256.Sum256(binary)
	checksums := hex.EncodeToString(sum[:]) + "  " + assetName() + "\n"

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		rel := release{
			TagName: "v9.9.9",
			HTMLURL: "https://example.test/releases/v9.9.9",
			Assets: []asset{
				{Name: assetName(), URL: srvURL(r) + "/download/binary"},
				{Name: "checksums.txt", URL: srvURL(r) + "/download/checksums"},
			},
		}
		_ = json.NewEncoder(w).Encode(rel)
	})
	mux.HandleFunc("/download/binary", func(w http.ResponseWriter, r *http.Request) {
		rec.record("asset_download")
		_, _ = w.Write(binary)
	})
	mux.HandleFunc("/download/checksums", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(checksums))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	exec := filepath.Join(dir, "twitch-miner-go")
	_ = os.WriteFile(exec, []byte("old"), 0755)

	h := newSpyHandoff(rec)
	u := New(Options{
		Repo: "owner/repo", CurrentVersion: "v1.0.0", Enabled: true,
		apiBaseURL: srv.URL, execPath: exec, httpClient: srv.Client(),
		Handoff: h,
	})
	u.checkAndMaybeUpdate(context.Background())

	evs := rec.all()
	if len(evs) < 2 || evs[0] != "record_applying" || evs[1] != "asset_download" {
		t.Fatalf("event order = %v, want [record_applying, asset_download, ...]", evs)
	}
	if applying, applied, clear := h.counts(); applying != 1 || applied != 1 || clear != 0 {
		t.Errorf("counts = applying=%d applied=%d clear=%d, want 1/1/0 (success path)", applying, applied, clear)
	}
}

// A Handoff is never consulted on a cycle that never attempts to apply
// anything: up-to-date, disabled, and gate-blocked cycles must all leave
// every Handoff method uncalled.
func TestHandoffNotCalledWhenNoApplyAttempted(t *testing.T) {
	t.Run("up_to_date", func(t *testing.T) {
		srv := newReleaseServer(t, "v1.0.0", []byte("bin"), false, 0)
		defer srv.Close()
		h := newSpyHandoff(&orderedRecorder{})
		u := New(Options{
			Repo: "owner/repo", CurrentVersion: "v1.0.0", Enabled: true,
			apiBaseURL: srv.URL, httpClient: srv.Client(), Handoff: h,
		})
		u.checkAndMaybeUpdate(context.Background())
		if a, ap, c := h.counts(); a != 0 || ap != 0 || c != 0 {
			t.Errorf("counts = %d/%d/%d, want all zero", a, ap, c)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		srv := newReleaseServer(t, "v9.9.9", []byte("bin"), true, 0)
		defer srv.Close()
		h := newSpyHandoff(&orderedRecorder{})
		u := New(Options{
			Repo: "owner/repo", CurrentVersion: "v1.0.0", Enabled: false,
			apiBaseURL: srv.URL, httpClient: srv.Client(), Handoff: h,
		})
		u.checkAndMaybeUpdate(context.Background())
		if a, ap, c := h.counts(); a != 0 || ap != 0 || c != 0 {
			t.Errorf("counts = %d/%d/%d, want all zero", a, ap, c)
		}
	})
	t.Run("gate_blocked", func(t *testing.T) {
		srv := newReleaseServer(t, "v9.9.9", []byte("bin"), true, 0)
		defer srv.Close()
		h := newSpyHandoff(&orderedRecorder{})
		u := New(Options{
			Repo: "owner/repo", CurrentVersion: "v1.0.0", Enabled: true,
			apiBaseURL: srv.URL, httpClient: srv.Client(), Handoff: h,
			Gate: func() bool { return false },
		})
		u.checkAndMaybeUpdate(context.Background())
		if a, ap, c := h.counts(); a != 0 || ap != 0 || c != 0 {
			t.Errorf("counts = %d/%d/%d, want all zero", a, ap, c)
		}
	})
}

// Clear is called on EVERY applyUpdate failure class: missing asset,
// download error, all three fail-closed checksum refusals, checksum
// mismatch, and a failed binary swap.
func TestHandoffClearCalledOnEveryFailureClass(t *testing.T) {
	newBinaryOK := []byte("new binary")

	cases := []struct {
		name     string
		buildSrv func(t *testing.T) *httptest.Server
		execOK   bool // false -> exec path's parent dir is missing (swap failure)
	}{
		{
			name: "missing_asset",
			buildSrv: func(t *testing.T) *httptest.Server {
				mux := http.NewServeMux()
				mux.HandleFunc("/repos/owner/repo/releases/latest", func(w http.ResponseWriter, r *http.Request) {
					rel := release{TagName: "v9.9.9", Assets: []asset{{Name: "some-other-binary", URL: srvURL(r) + "/bin"}}}
					_ = json.NewEncoder(w).Encode(rel)
				})
				return httptest.NewServer(mux)
			},
			execOK: true,
		},
		{
			name: "download_error",
			buildSrv: func(t *testing.T) *httptest.Server {
				mux := http.NewServeMux()
				mux.HandleFunc("/repos/owner/repo/releases/latest", func(w http.ResponseWriter, r *http.Request) {
					rel := release{TagName: "v9.9.9", Assets: []asset{{Name: assetName(), URL: srvURL(r) + "/bin"}}}
					_ = json.NewEncoder(w).Encode(rel)
				})
				mux.HandleFunc("/bin", func(w http.ResponseWriter, r *http.Request) {
					http.Error(w, "boom", http.StatusInternalServerError)
				})
				return httptest.NewServer(mux)
			},
			execOK: true,
		},
		{
			name: "checksums_missing",
			buildSrv: func(t *testing.T) *httptest.Server {
				return newReleaseServer(t, "v9.9.9", newBinaryOK, false, 0)
			},
			execOK: true,
		},
		{
			name: "checksums_fetch_fails",
			buildSrv: func(t *testing.T) *httptest.Server {
				mux := http.NewServeMux()
				mux.HandleFunc("/repos/owner/repo/releases/latest", func(w http.ResponseWriter, r *http.Request) {
					rel := release{TagName: "v9.9.9", Assets: []asset{
						{Name: assetName(), URL: srvURL(r) + "/bin"},
						{Name: "checksums.txt", URL: srvURL(r) + "/sums"},
					}}
					_ = json.NewEncoder(w).Encode(rel)
				})
				mux.HandleFunc("/bin", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(newBinaryOK) })
				mux.HandleFunc("/sums", func(w http.ResponseWriter, r *http.Request) {
					http.Error(w, "boom", http.StatusInternalServerError)
				})
				return httptest.NewServer(mux)
			},
			execOK: true,
		},
		{
			name: "checksum_entry_missing",
			buildSrv: func(t *testing.T) *httptest.Server {
				mux := http.NewServeMux()
				mux.HandleFunc("/repos/owner/repo/releases/latest", func(w http.ResponseWriter, r *http.Request) {
					rel := release{TagName: "v9.9.9", Assets: []asset{
						{Name: assetName(), URL: srvURL(r) + "/bin"},
						{Name: "checksums.txt", URL: srvURL(r) + "/sums"},
					}}
					_ = json.NewEncoder(w).Encode(rel)
				})
				mux.HandleFunc("/bin", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(newBinaryOK) })
				mux.HandleFunc("/sums", func(w http.ResponseWriter, r *http.Request) {
					_, _ = w.Write([]byte("deadbeef  some-other-asset\n"))
				})
				return httptest.NewServer(mux)
			},
			execOK: true,
		},
		{
			name: "checksum_mismatch",
			buildSrv: func(t *testing.T) *httptest.Server {
				mux := http.NewServeMux()
				mux.HandleFunc("/repos/owner/repo/releases/latest", func(w http.ResponseWriter, r *http.Request) {
					rel := release{TagName: "v9.9.9", Assets: []asset{
						{Name: assetName(), URL: srvURL(r) + "/bin"},
						{Name: "checksums.txt", URL: srvURL(r) + "/sums"},
					}}
					_ = json.NewEncoder(w).Encode(rel)
				})
				mux.HandleFunc("/bin", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(newBinaryOK) })
				mux.HandleFunc("/sums", func(w http.ResponseWriter, r *http.Request) {
					_, _ = w.Write([]byte("deadbeef  " + assetName() + "\n"))
				})
				return httptest.NewServer(mux)
			},
			execOK: true,
		},
		{
			name:     "swap_write_failure",
			buildSrv: func(t *testing.T) *httptest.Server { return newReleaseServer(t, "v9.9.9", newBinaryOK, true, 0) },
			execOK:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := tc.buildSrv(t)
			defer srv.Close()

			var exec string
			if tc.execOK {
				dir := t.TempDir()
				exec = filepath.Join(dir, "twitch-miner-go")
				_ = os.WriteFile(exec, []byte("old"), 0755)
			} else {
				exec = filepath.Join(t.TempDir(), "missing-dir", "twitch-miner-go")
			}

			h := newSpyHandoff(&orderedRecorder{})
			u := New(Options{
				Repo: "owner/repo", CurrentVersion: "v1.0.0", Enabled: true,
				apiBaseURL: srv.URL, execPath: exec, httpClient: srv.Client(),
				Handoff: h,
			})
			u.checkAndMaybeUpdate(context.Background())

			applying, applied, clear := h.counts()
			if applying != 1 {
				t.Errorf("RecordApplying calls = %d, want 1", applying)
			}
			if applied != 0 {
				t.Errorf("RecordApplied calls = %d, want 0 (failure path)", applied)
			}
			if clear != 1 {
				t.Errorf("Clear calls = %d, want 1 (mandatory terminalization on failure)", clear)
			}
		})
	}
}

// RecordApplied strictly precedes OnUpdate in program order.
func TestHandoffRecordAppliedBeforeOnUpdate(t *testing.T) {
	binary := []byte("BRAND NEW BINARY CONTENTS")
	srv := newReleaseServer(t, "v9.9.9", binary, true, 0)
	defer srv.Close()

	dir := t.TempDir()
	exec := filepath.Join(dir, "twitch-miner-go")
	_ = os.WriteFile(exec, []byte("old"), 0755)

	rec := &orderedRecorder{}
	h := newSpyHandoff(rec)

	u := New(Options{
		Repo: "owner/repo", CurrentVersion: "v1.0.0", Enabled: true,
		apiBaseURL: srv.URL, execPath: exec, httpClient: srv.Client(),
		Handoff:  h,
		OnUpdate: func() { rec.record("on_update") },
	})
	u.checkAndMaybeUpdate(context.Background())

	evs := rec.all()
	idxApplied, idxOnUpdate := -1, -1
	for i, e := range evs {
		if e == "record_applied" {
			idxApplied = i
		}
		if e == "on_update" {
			idxOnUpdate = i
		}
	}
	if idxApplied == -1 || idxOnUpdate == -1 || idxApplied >= idxOnUpdate {
		t.Fatalf("event order = %v, want record_applied strictly before on_update", evs)
	}
}

// A failing RecordApplying must not prevent the apply (observability must
// never block an update) - the error is routed through onHandoffError
// instead.
func TestHandoffRecordApplyingErrorDoesNotBlockApply(t *testing.T) {
	binary := []byte("BRAND NEW BINARY CONTENTS")
	srv := newReleaseServer(t, "v9.9.9", binary, true, 0)
	defer srv.Close()

	dir := t.TempDir()
	exec := filepath.Join(dir, "twitch-miner-go")
	_ = os.WriteFile(exec, []byte("old"), 0755)

	h := newSpyHandoff(&orderedRecorder{})
	h.recordApplyingErr = errors.New("boom")

	var handoffStage string
	var handoffErr error
	restarted := false

	u := New(Options{
		Repo: "owner/repo", CurrentVersion: "v1.0.0", Enabled: true,
		apiBaseURL: srv.URL, execPath: exec, httpClient: srv.Client(),
		Handoff:  h,
		OnUpdate: func() { restarted = true },
	})
	u.opts.onHandoffError = func(stage string, err error) {
		handoffStage, handoffErr = stage, err
	}

	u.checkAndMaybeUpdate(context.Background())

	if !restarted {
		t.Error("apply must proceed despite a failing RecordApplying (observability must never block an update)")
	}
	if handoffStage != "record_applying" || handoffErr == nil {
		t.Errorf("onHandoffError not routed correctly: stage=%q err=%v", handoffStage, handoffErr)
	}
	if got, _ := os.ReadFile(exec); string(got) != string(binary) {
		t.Errorf("binary not replaced: %q", got)
	}
}

// handoffErrorRingMarkerSeq gives each TestHandoffDefaultErrorRecordsRingEvent
// invocation (including reruns under `go test -count=N`) a distinct marker,
// so the assertion below can find the exact event this call produced in the
// process-wide events ring, rather than a stale one from another run -
// mirrors gateBlockedRingMarkerSeq's precedent for onGateBlocked.
var handoffErrorRingMarkerSeq atomic.Int64

// The default (production) onHandoffError records the canonical
// updater_handoff_write_failed ring event with the stage prefix, without a
// test override - proving the wiring in New() actually installs
// defaultHandoffError (mirrors TestGateBlockedDefaultRecordsRingEvent's
// precedent for onGateBlocked).
func TestHandoffDefaultErrorRecordsRingEvent(t *testing.T) {
	n := handoffErrorRingMarkerSeq.Add(1)
	marker := fmt.Sprintf("handoff-write-failed-marker-%d", n)

	binary := []byte("BRAND NEW BINARY CONTENTS")
	srv := newReleaseServer(t, "v9.9.9", binary, true, 0)
	defer srv.Close()

	dir := t.TempDir()
	exec := filepath.Join(dir, "twitch-miner-go")
	_ = os.WriteFile(exec, []byte("old"), 0755)

	h := newSpyHandoff(&orderedRecorder{})
	h.recordApplyingErr = errors.New(marker)

	u := New(Options{
		Repo: "owner/repo", CurrentVersion: "v1.0.0", Enabled: true,
		apiBaseURL: srv.URL, execPath: exec, httpClient: srv.Client(),
		Handoff: h,
		// onHandoffError intentionally left nil so New() installs the
		// production default (defaultHandoffError) - no test override here.
	})
	u.checkAndMaybeUpdate(context.Background())

	found := false
	for _, e := range events.Recent(200) {
		if e.Type == events.TypeUpdaterHandoffWriteFailed &&
			strings.Contains(e.Detail, "record_applying") && strings.Contains(e.Detail, marker) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("default onHandoffError did not record an updater_handoff_write_failed ring event for %q", marker)
	}
}

// A failing RecordApplied must not prevent OnUpdate from firing (the binary
// swap already succeeded by the time RecordApplied runs) - the error is
// routed through onHandoffError instead, exactly like a failing
// RecordApplying.
func TestHandoffRecordAppliedErrorDoesNotBlockOnUpdate(t *testing.T) {
	binary := []byte("BRAND NEW BINARY CONTENTS")
	srv := newReleaseServer(t, "v9.9.9", binary, true, 0)
	defer srv.Close()

	dir := t.TempDir()
	exec := filepath.Join(dir, "twitch-miner-go")
	_ = os.WriteFile(exec, []byte("old"), 0755)

	h := newSpyHandoff(&orderedRecorder{})
	h.recordAppliedErr = errors.New("boom-applied")

	var handoffStage string
	var handoffErr error
	restarted := false

	u := New(Options{
		Repo: "owner/repo", CurrentVersion: "v1.0.0", Enabled: true,
		apiBaseURL: srv.URL, execPath: exec, httpClient: srv.Client(),
		Handoff:  h,
		OnUpdate: func() { restarted = true },
	})
	u.opts.onHandoffError = func(stage string, err error) {
		handoffStage, handoffErr = stage, err
	}

	u.checkAndMaybeUpdate(context.Background())

	if !restarted {
		t.Error("OnUpdate must still fire despite a failing RecordApplied")
	}
	if handoffStage != "record_applied" || handoffErr == nil {
		t.Errorf("onHandoffError not routed correctly: stage=%q err=%v", handoffStage, handoffErr)
	}
	if got, _ := os.ReadFile(exec); string(got) != string(binary) {
		t.Errorf("binary not replaced: %q", got)
	}
}

// A failing Clear must not change the existing failure handling at all
// (NotifyFailure still fires with the original refusal reason, the binary
// stays untouched, LastOutcome still records apply_failed) - only the
// Clear error itself is additionally routed through onHandoffError.
func TestHandoffClearErrorDoesNotChangeFailureHandling(t *testing.T) {
	srv := newReleaseServer(t, "v9.9.9", []byte("new binary"), false /* no checksums -> refuse */, 0)
	defer srv.Close()

	dir := t.TempDir()
	exec := filepath.Join(dir, "twitch-miner-go")
	_ = os.WriteFile(exec, []byte("old"), 0755)

	h := newSpyHandoff(&orderedRecorder{})
	h.clearErr = errors.New("boom-clear")

	var handoffStage string
	var handoffErr error
	failureCalls := 0
	var failureReason string

	u := New(Options{
		Repo: "owner/repo", CurrentVersion: "v1.0.0", Enabled: true,
		apiBaseURL: srv.URL, execPath: exec, httpClient: srv.Client(),
		Handoff: h,
		NotifyFailure: func(_, _, reason string) {
			failureCalls++
			failureReason = reason
		},
	})
	u.opts.onHandoffError = func(stage string, err error) {
		handoffStage, handoffErr = stage, err
	}

	u.checkAndMaybeUpdate(context.Background())

	if handoffStage != "clear" || handoffErr == nil {
		t.Errorf("onHandoffError not routed correctly: stage=%q err=%v", handoffStage, handoffErr)
	}
	if failureCalls != 1 {
		t.Errorf("NotifyFailure calls = %d, want 1 (a failing Clear must not change failure handling)", failureCalls)
	}
	if !strings.Contains(failureReason, "refusing to install unverified binary") {
		t.Errorf("failure reason = %q, want the refusal reason unchanged", failureReason)
	}
	if got, _ := os.ReadFile(exec); string(got) != "old" {
		t.Errorf("binary replaced despite refusal: %q", got)
	}
	if applying, applied, clear := h.counts(); applying != 1 || applied != 0 || clear != 1 {
		t.Errorf("counts = applying=%d applied=%d clear=%d, want 1/0/1", applying, applied, clear)
	}
	if got := u.Snapshot().LastOutcome; got != OutcomeApplyFailed {
		t.Errorf("LastOutcome = %q, want %q even though Clear itself failed", got, OutcomeApplyFailed)
	}
}
