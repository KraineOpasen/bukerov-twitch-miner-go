package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
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

func TestParseCheckInterval(t *testing.T) {
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
