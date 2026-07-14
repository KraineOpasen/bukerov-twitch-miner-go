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

func TestLogLineClass(t *testing.T) {
	cases := map[string]string{
		`time=... level=INFO msg="Streamer is online" streamer=shroud`: "log-info",
		`time=... level=DEBUG msg="tick"`:                              "log-info",
		`time=... level=WARN msg="retrying" attempt=2`:                 "log-warn",
		`time=... level=ERROR msg="auth failed"`:                       "log-error",
		`time=... level=INFO msg="Streamer went offline" streamer=x`:   "log-error", // offline => red
		`time=... level=WARN msg="Streamer went offline" streamer=y`:   "log-error", // offline overrides warn
	}
	for line, want := range cases {
		if got := logLineClass(line); got != want {
			t.Errorf("logLineClass(%q) = %q, want %q", line, got, want)
		}
	}
}

func TestLogLevelOf(t *testing.T) {
	if got := logLevelOf(`time=x level=ERROR msg="y"`); got != "ERROR" {
		t.Errorf("level = %q, want ERROR", got)
	}
	if got := logLevelOf(`no level here`); got != "" {
		t.Errorf("missing level = %q, want empty", got)
	}
}

// TestLogsLinesPartialColoring renders the line partial and checks each level
// gets its color class and the text is present (and escaped).
func TestLogsLinesPartialColoring(t *testing.T) {
	partials := testPartials(t)
	data := LogsLinesData{FileLogging: true, Lines: []LogLineView{
		{Class: "log-info", Text: `level=INFO msg="hello"`},
		{Class: "log-warn", Text: `level=WARN msg="careful"`},
		{Class: "log-error", Text: `level=ERROR msg="boom <script>"`},
	}}
	var buf bytes.Buffer
	if err := partials.ExecuteTemplate(&buf, "logs_lines", data); err != nil {
		t.Fatalf("render logs_lines: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"log-info", "log-warn", "log-error", "hello", "careful"} {
		if !strings.Contains(out, want) {
			t.Errorf("logs_lines output missing %q:\n%s", want, out)
		}
	}
	// Untrusted log text must be HTML-escaped.
	if strings.Contains(out, "<script>") {
		t.Errorf("log text was not escaped")
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
