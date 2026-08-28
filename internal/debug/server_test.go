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

func TestLogEndpointTailsRetainedFamily(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "family.log")
	var archive, active strings.Builder
	for i := 1; i <= 1800; i++ {
		fmt.Fprintf(&archive, "archive-%04d\n", i)
	}
	for i := 1; i <= 700; i++ {
		fmt.Fprintf(&active, "active-%04d\n", i)
	}
	if err := os.WriteFile(logPath+".rotated-00000000000000000001", []byte(archive.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte(active.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	base := startTestServer(t, func() Snapshot { return Snapshot{} }, logPath)
	tests := []struct {
		name      string
		query     string
		wantLines int
		first     string
		boundary  string
		last      string
	}{
		{name: "default-1000", query: "", wantLines: 1000, first: "archive-1501", boundary: "active-0001", last: "active-0700"},
		{name: "clamped-2000", query: "?lines=5000", wantLines: 2000, first: "archive-0501", boundary: "active-0001", last: "active-0700"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get(base + "/debug/log" + tc.query)
			if err != nil {
				t.Fatalf("GET /debug/log: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
			if len(lines) != tc.wantLines {
				t.Fatalf("lines=%d, want %d", len(lines), tc.wantLines)
			}
			boundaryIndex := tc.wantLines - 700
			if lines[0] != tc.first || lines[boundaryIndex] != tc.boundary || lines[len(lines)-1] != tc.last {
				t.Fatalf("family tail order/boundary wrong: first=%q boundary=%q last=%q", lines[0], lines[boundaryIndex], lines[len(lines)-1])
			}
		})
	}
}

func TestLogEndpointHonorsFourMiBAcrossFamily(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "budget.log")
	payload := strings.Repeat("x", 3000)
	var archive, active strings.Builder
	for i := 1; i <= 1000; i++ {
		fmt.Fprintf(&archive, "archive-%04d-%s\n", i, payload)
	}
	for i := 1; i <= 1000; i++ {
		fmt.Fprintf(&active, "active-%04d-%s\n", i, payload)
	}
	if err := os.WriteFile(logPath+".rotated-00000000000000000001", []byte(archive.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte(active.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	base := startTestServer(t, func() Snapshot { return Snapshot{} }, logPath)
	resp, err := http.Get(base + "/debug/log?lines=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > maxLogTailBytes {
		t.Fatalf("debug response bytes=%d exceed cap=%d", len(body), maxLogTailBytes)
	}
	if len(body) > 0 && body[len(body)-1] != '\n' {
		t.Fatal("debug response ends with a partial record")
	}
	if strings.Contains(string(body), "archive-0001-") || !strings.Contains(string(body), "active-1000-") {
		t.Fatal("debug family byte cap selected the wrong end of retained history")
	}
}

func TestLogEndpointServesArchivesWithoutActive(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "archives-only.log")
	if err := os.WriteFile(logPath+".rotated-00000000000000000001", []byte("archive-only\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := startTestServer(t, func() Snapshot { return Snapshot{} }, logPath)
	resp, err := http.Get(base + "/debug/log")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "archive-only\n" {
		t.Fatalf("archives-only debug family: status=%d body=%q", resp.StatusCode, body)
	}
}
