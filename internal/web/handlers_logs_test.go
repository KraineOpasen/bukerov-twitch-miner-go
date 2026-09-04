package web

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
)

func TestTailLogFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "miner.log")
	var buf bytes.Buffer
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&buf, "time=2026-07-14T00:00:00Z level=INFO msg=\"line %d\"\n", i)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	// Read whole file when under the cap.
	all, err := tailLogFile(path, 1<<20)
	if err != nil {
		t.Fatalf("tail (whole): %v", err)
	}
	if got := bytes.Count(all, []byte("\n")); got != 200 {
		t.Errorf("whole-file line count = %d, want 200", got)
	}

	// Small cap: returns only the tail, aligned to a clean line boundary.
	tail, err := tailLogFile(path, 300)
	if err != nil {
		t.Fatalf("tail (capped): %v", err)
	}
	s := string(tail)
	if strings.Contains(s, "line 1\"") && !strings.Contains(s, "line 199") {
		t.Errorf("capped tail should drop early lines, got:\n%s", s)
	}
	if !strings.Contains(s, "line 200") {
		t.Errorf("capped tail must include the last line")
	}
	// First line must be complete (no mid-line truncation).
	first := strings.SplitN(s, "\n", 2)[0]
	if first != "" && !strings.HasPrefix(first, "time=") {
		t.Errorf("first tail line is truncated: %q", first)
	}
}

func TestTailLogFileMissing(t *testing.T) {
	_, err := tailLogFile(filepath.Join(t.TempDir(), "nope.log"), 1000)
	if !os.IsNotExist(err) {
		t.Errorf("missing file should return os.IsNotExist error, got %v", err)
	}
}

func TestReadLogTailCrossesRotatedSegments(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("logs", 0o755); err != nil {
		t.Fatal(err)
	}

	activePath := filepath.Join("logs", "family.log")
	var archive, active strings.Builder
	for i := 1; i <= 350; i++ {
		fmt.Fprintf(&archive, "time=2026-07-13T00:00:00Z level=INFO msg=archive-%03d\n", i)
	}
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&active, "time=2026-07-14T00:00:00Z level=INFO msg=active-%03d\n", i)
	}
	if err := os.WriteFile(activePath+".rotated-00000000000000000001", []byte(archive.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(activePath, []byte(active.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Server{username: "family"}
	views, enabled := s.readLogTail()
	if !enabled {
		t.Fatal("readLogTail reported a retained family as disabled")
	}
	// 550 lines fit inside the raw scan window (logScanLines), so the whole
	// family comes back in chronological order across the rotation boundary.
	if len(views) != 550 {
		t.Fatalf("views=%d, want 550", len(views))
	}
	if !strings.Contains(views[0].Text, "archive-001") || !strings.Contains(views[349].Text, "archive-350") {
		t.Fatalf("archive portion is not chronological: first=%q boundary=%q", views[0].Text, views[349].Text)
	}
	if !strings.Contains(views[350].Text, "active-001") || !strings.Contains(views[549].Text, "active-200") {
		t.Fatalf("active portion is not chronological: boundary=%q last=%q", views[350].Text, views[549].Text)
	}
}

// TestReadLogTailScanWindowAndVisibleCap pins the two separate budgets the
// Logs page runs on: readLogTail classifies up to logScanLines RAW lines,
// and the render seam lists at most logTailLines VISIBLE ones (newest).
//
// The two must not be the same number. File logging defaults to DEBUG, so a
// busy miner's tail is mostly suppressed transport chatter; if the raw window
// were only logTailLines deep, the page could show a handful of entries — or
// an empty state — on a perfectly healthy miner.
func TestReadLogTailScanWindowAndVisibleCap(t *testing.T) {
	// The two budgets are different numbers on purpose, and the scan window
	// must stay comfortably deeper than the visible cap.
	if logScanLines <= logTailLines {
		t.Fatalf("logScanLines=%d must exceed logTailLines=%d", logScanLines, logTailLines)
	}

	t.Chdir(t.TempDir())
	if err := os.MkdirAll("logs", 0o755); err != nil {
		t.Fatal(err)
	}

	// One suppressed chatter line for every meaningful one, and more raw lines
	// in total than the scan window. The ratio matters: the SCANNED window
	// must hold strictly more than logTailLines meaningful lines, otherwise
	// every assertion below would hold even with the cap removed. Each line
	// carries a monotonic seq so the direction of both truncations is
	// observable — taking the oldest end instead of the newest is the easiest
	// way to get this wrong.
	const totalRaw = logScanLines + 1000
	var b strings.Builder
	for i := 1; i <= totalRaw; i++ {
		if i%2 == 0 {
			fmt.Fprintf(&b, "time=2026-07-14T00:00:00Z level=INFO msg=\"Points earned\" reason=WATCH points=10 seq=%06d\n", i)
			continue
		}
		fmt.Fprintf(&b, "time=2026-07-14T00:00:00Z level=DEBUG msg=\"WebSocket send\" index=0 type=PING seq=%06d\n", i)
	}
	if err := os.WriteFile(filepath.Join("logs", "budget.log"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	// Meaningful lines inside the scan window (the newest logScanLines raw
	// lines), which is what the visible cap actually has to bind against.
	if inWindow := logScanLines / 2; inWindow <= logTailLines {
		t.Fatalf("fixture is too weak: the scan window holds %d meaningful lines, need more than the %d visible cap", inWindow, logTailLines)
	}

	s := &Server{username: "budget"}
	views, enabled := s.readLogTail()
	if !enabled {
		t.Fatal("readLogTail reported the retained file as disabled")
	}
	if len(views) != logScanLines {
		t.Fatalf("readLogTail classified %d lines, want exactly logScanLines=%d", len(views), logScanLines)
	}
	// The raw window must be the NEWEST logScanLines lines, not the oldest.
	if !strings.Contains(views[len(views)-1].Text, fmt.Sprintf("seq=%06d", totalRaw)) {
		t.Errorf("raw scan window does not end at the newest line: last=%q", views[len(views)-1].Text)
	}
	if !strings.Contains(views[0].Text, fmt.Sprintf("seq=%06d", totalRaw-logScanLines+1)) {
		t.Errorf("raw scan window does not start at the expected offset: first=%q", views[0].Text)
	}

	visible := visibleLogLines(views)
	if len(visible) != logTailLines {
		t.Fatalf("visible lines = %d, want exactly the %d cap", len(visible), logTailLines)
	}
	// ...and the visible cap must also keep the NEWEST end.
	if !strings.Contains(visible[len(visible)-1].Text, fmt.Sprintf("seq=%06d", totalRaw)) {
		t.Errorf("visible cap does not end at the newest line: last=%q", visible[len(visible)-1].Text)
	}
	if !strings.Contains(visible[0].Text, fmt.Sprintf("seq=%06d", totalRaw-2*logTailLines+2)) {
		t.Errorf("visible cap does not keep the newest %d lines: first=%q", logTailLines, visible[0].Text)
	}
	for _, v := range visible {
		if !v.DashboardVisible {
			t.Error("visibleLogLines returned a suppressed line")
		}
		if strings.Contains(v.Text, "WebSocket send") {
			t.Error("suppressed transport chatter reached the visible set")
		}
	}
}

func TestReadLogTailUsesImmutableStorageKey(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("logs", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("logs", "oldlogin.log"), []byte("time=x level=INFO msg=stable-storage-key\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("logs", "newlogin.log"), []byte("time=x level=INFO msg=mutable-login-wrong\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	constructors := map[string]func() *Server{
		"full": func() *Server {
			return NewServer(config.AnalyticsSettings{}, "newlogin", filepath.Join("database", "oldlogin"), nil, nil)
		},
		"early": func() *Server {
			return NewServerEarly(config.AnalyticsSettings{}, "newlogin", filepath.Join("database", "oldlogin"), nil)
		},
	}
	for name, construct := range constructors {
		t.Run(name, func(t *testing.T) {
			s := construct()
			s.username = "renamed-again"
			views, enabled := s.readLogTail()
			if !enabled || len(views) != 1 {
				t.Fatalf("enabled=%v views=%d, want stable StorageKey family", enabled, len(views))
			}
			if !strings.Contains(views[0].Text, "stable-storage-key") || strings.Contains(views[0].Text, "mutable-login-wrong") {
				t.Fatalf("web log source followed mutable username: %+v", views)
			}
		})
	}
}

func TestReadLogTailHonorsTwoMiBAcrossFamily(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("logs", 0o755); err != nil {
		t.Fatal(err)
	}
	activePath := filepath.Join("logs", "web-budget.log")
	payload := strings.Repeat("x", 5000)
	var archive, active strings.Builder
	for i := 1; i <= 300; i++ {
		fmt.Fprintf(&archive, "time=2026-08-27T00:00:00Z level=INFO msg=archive-%03d-%s\n", i, payload)
	}
	for i := 1; i <= 300; i++ {
		fmt.Fprintf(&active, "time=2026-08-28T00:00:00Z level=INFO msg=active-%03d-%s\n", i, payload)
	}
	if err := os.WriteFile(completedSegmentPathForWeb(activePath, 1), []byte(archive.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(activePath, []byte(active.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	views, enabled := (&Server{logPath: activePath}).readLogTail()
	if !enabled {
		t.Fatal("budgeted retained family reported disabled")
	}
	physicalEquivalent := 0
	for _, view := range views {
		physicalEquivalent += len(view.Text) + 1
		if !strings.HasPrefix(view.Text, "time=") {
			t.Fatalf("partial leading line escaped budget: %q", view.Text[:min(len(view.Text), 80)])
		}
	}
	if physicalEquivalent > logTailMaxBytes {
		t.Fatalf("rendered family bytes=%d exceed web cap=%d", physicalEquivalent, logTailMaxBytes)
	}
	joined := viewsText(views)
	if strings.Contains(joined, "archive-001-") || !strings.Contains(joined, "active-300-") {
		t.Fatal("web family budget selected the wrong end of retained history")
	}
}

func TestReadLogTailServesArchivesWithoutActive(t *testing.T) {
	dir := t.TempDir()
	activePath := filepath.Join(dir, "archives-only.log")
	if err := os.WriteFile(completedSegmentPathForWeb(activePath, 1), []byte("time=x level=INFO msg=archive-only\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	views, enabled := (&Server{logPath: activePath}).readLogTail()
	if !enabled || len(views) != 1 || !strings.Contains(views[0].Text, "archive-only") {
		t.Fatalf("archives-only web family: enabled=%v views=%+v", enabled, views)
	}
}

func TestReadLogTailUnsafeIdentityDoesNotScanCurrentDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	canaryPath := ".rotated-00000000000000000001"
	if err := os.WriteFile(canaryPath, []byte("cwd-canary-must-not-be-disclosed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	views, enabled := (&Server{username: "../unsafe"}).readLogTail()
	if enabled || len(views) != 0 {
		t.Fatalf("unsafe empty locator scanned cwd: enabled=%v views=%+v", enabled, views)
	}
	data, err := os.ReadFile(canaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "cwd-canary-must-not-be-disclosed\n" {
		t.Fatalf("cwd canary changed: %q", data)
	}
}

func completedSegmentPathForWeb(activePath string, sequence uint64) string {
	return fmt.Sprintf("%s.rotated-%020d", activePath, sequence)
}

func viewsText(views []LogLineView) string {
	var out strings.Builder
	for _, view := range views {
		out.WriteString(view.Text)
		out.WriteByte('\n')
	}
	return out.String()
}

// TestReadLogTailClassifies exercises the handler-level pipeline: a real log
// file on disk comes back as classified views (class + emoji + untouched
// text), independent of any logger setting — the classification has no
// Logger.Colored input at all.
func TestReadLogTailClassifies(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("logs", 0o755); err != nil {
		t.Fatal(err)
	}
	content := strings.Join([]string{
		`time=2026-07-14T10:00:00Z level=INFO msg="Streamer is online" streamer=shroud`,
		`time=2026-07-14T10:01:00Z level=INFO msg="Points earned" streamer=shroud points=10 reason=WATCH`,
		`time=2026-07-14T10:02:00Z level=WARN msg="GQL request failed, retrying" attempt=2`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join("logs", "tester.log"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Server{username: "tester"}
	views, enabled := s.readLogTail()
	if !enabled {
		t.Fatal("readLogTail reported file logging disabled")
	}
	if len(views) != 3 {
		t.Fatalf("got %d views, want 3", len(views))
	}
	want := []struct{ class, emoji string }{
		{"log-streamer-online", "🟢"},
		{"log-points-watch", "👀"},
		{"log-warn", "⚠️"},
	}
	for i, w := range want {
		if views[i].Class != w.class || views[i].Emoji != w.emoji {
			t.Errorf("views[%d] = {%s %s}, want {%s %s}", i, views[i].Class, views[i].Emoji, w.class, w.emoji)
		}
		if !strings.HasPrefix(views[i].Text, "time=") {
			t.Errorf("views[%d].Text was altered: %q", i, views[i].Text)
		}
	}
}

// TestLogsLinesPartialColoring renders the line partial and checks each line
// gets its semantic class, its emoji in a separate aria-hidden span, and that
// the text is present (and escaped).
func TestLogsLinesPartialColoring(t *testing.T) {
	partials := testPartials(t)
	// The partial renders only dashboard-visible lines, so these fixtures
	// declare visibility explicitly; this test is about colour/escaping.
	data := LogsLinesData{FileLogging: true, Lines: []LogLineView{
		{Class: "log-info", Emoji: "ℹ️", Text: `level=INFO msg="hello"`, Level: "info", Subsystem: "other", DashboardVisible: true},
		{Class: "log-warn", Emoji: "⚠️", Text: `level=WARN msg="careful"`, Level: "warning", Subsystem: "other", DashboardVisible: true},
		{Class: "log-error", Emoji: "❌", Text: `level=ERROR msg="boom <script>alert(1)</script>"`, Level: "error", Subsystem: "other", DashboardVisible: true},
		{Class: "log-points-streak", Emoji: "🔥", Text: `level=INFO msg="Points earned" reason=WATCH_STREAK`, Level: "info", Subsystem: "points", DashboardVisible: true},
	}}
	var buf bytes.Buffer
	if err := partials.ExecuteTemplate(&buf, "logs_lines", data); err != nil {
		t.Fatalf("render logs_lines: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"log-info", "log-warn", "log-error", "log-points-streak",
		"hello", "careful",
		"ℹ️", "⚠️", "❌", "🔥",
		`class="log-emoji" aria-hidden="true"`,
		`class="log-text"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("logs_lines output missing %q:\n%s", want, out)
		}
	}
	// Untrusted log text must be HTML-escaped, never executable.
	if strings.Contains(out, "<script>") {
		t.Errorf("log text was not escaped")
	}
}

// TestLogsLinesPartialNoDoubleEmoji renders a line whose raw text already
// starts with an emoji: the decorative span must stay empty so the icon never
// doubles.
func TestLogsLinesPartialNoDoubleEmoji(t *testing.T) {
	partials := testPartials(t)
	raw := `🟢 time=x level=INFO msg="Streamer is online" streamer=a`
	p := classifyLogLine(raw)
	if !p.HasLeadingEmoji || p.Emoji != "" {
		t.Fatalf("classify(%q) = %+v, want HasLeadingEmoji with empty Emoji", raw, p)
	}
	data := LogsLinesData{FileLogging: true, Lines: []LogLineView{{
		Class: p.Class, Emoji: p.Emoji, Text: raw,
		Level: p.Level, Subsystem: p.Subsystem, DashboardVisible: p.DashboardVisible,
	}}}
	var buf bytes.Buffer
	if err := partials.ExecuteTemplate(&buf, "logs_lines", data); err != nil {
		t.Fatalf("render logs_lines: %v", err)
	}
	out := buf.String()
	if got := strings.Count(out, "🟢"); got != 1 {
		t.Errorf("rendered partial contains %d 🟢, want exactly 1 (the original text's):\n%s", got, out)
	}
}

// TestLogsPageRendersBothLanguages renders /logs through base.html in RU and EN.
// With no log file present, the page shows the "file logging disabled" state.
func TestLogsPageRendersBothLanguages(t *testing.T) {
	s := newRenderServer(t) // username "" => no log file => disabled state

	// RU (default).
	recRU := httptest.NewRecorder()
	s.handleLogsPage(recRU, httptest.NewRequest(http.MethodGet, "/logs", nil))
	if recRU.Code != http.StatusOK {
		t.Fatalf("RU /logs = %d, want 200", recRU.Code)
	}
	ru := recRU.Body.String()
	if !strings.Contains(ru, "Запись логов в файл отключена") {
		t.Errorf("RU logs page should show the disabled-file-logging message")
	}

	// EN via cookie.
	reqEN := httptest.NewRequest(http.MethodGet, "/logs", nil)
	reqEN.AddCookie(&http.Cookie{Name: langCookieName, Value: "en"})
	recEN := httptest.NewRecorder()
	s.handleLogsPage(recEN, reqEN)
	en := recEN.Body.String()
	if !strings.Contains(en, "File logging is disabled") {
		t.Errorf("EN logs page should show the English disabled message")
	}
}
