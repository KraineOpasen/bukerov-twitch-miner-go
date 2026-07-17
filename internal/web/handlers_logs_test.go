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
	data := LogsLinesData{FileLogging: true, Lines: []LogLineView{
		{Class: "log-info", Emoji: "ℹ️", Text: `level=INFO msg="hello"`},
		{Class: "log-warn", Emoji: "⚠️", Text: `level=WARN msg="careful"`},
		{Class: "log-error", Emoji: "❌", Text: `level=ERROR msg="boom <script>alert(1)</script>"`},
		{Class: "log-points-streak", Emoji: "🔥", Text: `level=INFO msg="Points earned" reason=WATCH_STREAK`},
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
	data := LogsLinesData{FileLogging: true, Lines: []LogLineView{{Class: p.Class, Emoji: p.Emoji, Text: raw}}}
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
