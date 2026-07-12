package debug

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func startTestServer(t *testing.T, snapshot SnapshotFunc, logPath string) string {
	t.Helper()

	// Port 0 lets the OS pick a free port (config validation prevents 0 in
	// production); Addr() reports what was actually bound.
	srv := NewServer(0, snapshot, logPath)
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start debug server: %v", err)
	}
	t.Cleanup(srv.Stop)

	return srv.Addr()
}

func TestSnapshotEndpointServesJSON(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	base := startTestServer(t, func() Snapshot {
		return Snapshot{
			GeneratedAt: now,
			Status:      StatusRunning,
			Username:    "tester",
			Watching:    WatchingInfo{Mode: "rotation", ActivePair: []string{"a", "b"}},
		}
	}, "")

	resp, err := http.Get(base + "/debug/snapshot")
	if err != nil {
		t.Fatalf("GET /debug/snapshot: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("expected JSON content type, got %q", ct)
	}

	var snap Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if snap.Status != StatusRunning || snap.Watching.Mode != "rotation" || len(snap.Watching.ActivePair) != 2 {
		t.Fatalf("snapshot did not round-trip: %+v", snap)
	}
}

func TestLogEndpointTailsFile(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "miner.log")
	var content strings.Builder
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&content, "line-%d\n", i)
	}
	if err := os.WriteFile(logPath, []byte(content.String()), 0644); err != nil {
		t.Fatalf("failed to write log fixture: %v", err)
	}

	base := startTestServer(t, func() Snapshot { return Snapshot{} }, logPath)

	resp, err := http.Get(base + "/debug/log?lines=10")
	if err != nil {
		t.Fatalf("GET /debug/log: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read body: %v", err)
	}

	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(lines) != 10 {
		t.Fatalf("expected 10 lines, got %d: %q", len(lines), string(body))
	}
	if lines[0] != "line-41" || lines[9] != "line-50" {
		t.Fatalf("expected tail lines 41..50, got %q..%q", lines[0], lines[len(lines)-1])
	}
}

func TestLogEndpointWithoutLogFileConfigured(t *testing.T) {
	base := startTestServer(t, func() Snapshot { return Snapshot{} }, "")

	resp, err := http.Get(base + "/debug/log")
	if err != nil {
		t.Fatalf("GET /debug/log: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 when file logging is disabled, got %d", resp.StatusCode)
	}
}

func TestTailFileMoreLinesThanFile(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "short.log")
	if err := os.WriteFile(logPath, []byte("a\nb\n"), 0644); err != nil {
		t.Fatalf("failed to write log fixture: %v", err)
	}

	out, err := tailFile(logPath, 100)
	if err != nil {
		t.Fatalf("tailFile: %v", err)
	}
	if string(out) != "a\nb\n" {
		t.Fatalf("expected whole file back, got %q", string(out))
	}
}
