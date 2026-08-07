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
	"unicode/utf8"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/supportbundle"
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

// s55ReadLogTailFixture writes lines to a temp log file and returns the views
// readLogTail produces for them, in order.
func s55ReadLogTailFixture(t *testing.T, lines []string) []LogLineView {
	t.Helper()
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("logs", 0o755); err != nil {
		t.Fatal(err)
	}
	const username = "s55trunctester"
	if err := os.WriteFile(filepath.Join("logs", username+".log"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Server{username: username}
	views, enabled := s.readLogTail()
	if !enabled {
		t.Fatal("readLogTail reported file logging disabled")
	}
	if len(views) != len(lines) {
		t.Fatalf("got %d views, want %d", len(views), len(lines))
	}
	return views
}

// TestReadLogTailMarksTruncatedBenignLines pins the honesty contract on the
// privacy seam's OTHER outcome. supportbundle.Redact has two of them: a
// sensitive-shaped line is replaced wholesale by "[REDACTED]", and a BENIGN
// line is bounded to Redact's internal rune cap. The second is silent — a
// long technical log line that used to render in full now renders shorter
// with nothing to say so — so the web layer adds an explicit, language-neutral
// truncation marker.
//
// The marker is never attached to a redacted line: "[REDACTED]" is a fixed
// constant precisely so it discloses nothing about the original's length, and
// marking a long sensitive line would put that length signal straight back.
func TestReadLogTailMarksTruncatedBenignLines(t *testing.T) {
	// Every fixture line is deliberately benign in shape unless named
	// otherwise: no URL, path, IP, email, secret assignment, or 32+ char
	// mixed letter/digit run — so Redact's only effect is its length bound.
	const (
		shortBenign      = `time=2026-08-07T10:00:00Z level=INFO msg="inventory sync finished"`
		shortMultibyte   = `time=2026-08-07T10:00:01Z level=INFO msg="синхронизация инвентаря завершена"`
		shortSensitive   = `time=2026-08-07T10:00:04Z level=ERROR msg="client_secret=S55_TRUNC_SHORT"`
		truncationMarker = " […]"
	)
	// Comfortably past Redact's internal cap in runes, in both a single-byte
	// and a multi-byte alphabet.
	longBenign := `time=2026-08-07T10:00:02Z level=WARN msg="` + strings.Repeat("retrying inventory fetch ", 60) + `"`
	longMultibyte := `time=2026-08-07T10:00:03Z level=WARN msg="` + strings.Repeat("повтор загрузки инвентаря ", 60) + `"`
	longSensitive := `time=2026-08-07T10:00:05Z level=ERROR msg="client_secret=S55_TRUNC_LONG ` + strings.Repeat("tail padding ", 60) + `"`

	raw := []string{shortBenign, shortMultibyte, longBenign, longMultibyte, shortSensitive, longSensitive}
	views := s55ReadLogTailFixture(t, raw)
	got := make([]string, len(views))
	for i, v := range views {
		got[i] = v.Text
	}

	// 1. Bounded benign lines render byte-identically, marker-free — in both
	//    a single-byte and a multi-byte alphabet.
	for i, in := range []string{shortBenign, shortMultibyte} {
		if got[i] != in {
			t.Errorf("bounded benign line %d rendered %q, want it unchanged: %q", i, got[i], in)
		}
		if strings.Contains(got[i], truncationMarker) {
			t.Errorf("bounded benign line %d must not be marked truncated: %q", i, got[i])
		}
	}

	// 2. Long benign lines keep Redact's sanitized bounded prefix EXACTLY and
	//    gain a visible truncation marker. Comparing against Redact's own
	//    output (rather than a copied rune constant) keeps this rune-correct
	//    for multi-byte input without duplicating supportbundle's contract.
	for i, in := range []string{longBenign, longMultibyte} {
		idx := 2 + i
		bounded := supportbundle.Redact(in)
		if bounded == "[REDACTED]" {
			t.Fatalf("fixture bug: long line %d is sensitive-shaped, not benign", idx)
		}
		if utf8.RuneCountInString(bounded) >= utf8.RuneCountInString(in) {
			t.Fatalf("fixture bug: long line %d (%d runes) is not past Redact's cap", idx, utf8.RuneCountInString(in))
		}
		if want := bounded + truncationMarker; got[idx] != want {
			t.Errorf("long benign line %d rendered %q, want the bounded prefix plus %q", idx, got[idx], truncationMarker)
		}
		if !strings.HasPrefix(in, bounded) {
			t.Errorf("long benign line %d: displayed prefix is not a prefix of the raw line", idx)
		}
		if !utf8.ValidString(got[idx]) {
			t.Errorf("long benign line %d was cut mid-rune: %q", idx, got[idx])
		}
		// The marker must not smuggle any of the dropped suffix back in.
		if dropped := strings.TrimPrefix(in, bounded); dropped != "" && strings.Contains(got[idx], dropped) {
			t.Errorf("long benign line %d leaked its truncated suffix", idx)
		}
	}

	// 3. Sensitive lines stay exactly "[REDACTED]" — no raw suffix, and no
	//    truncation marker that would betray the original's length. A short
	//    and a long sensitive line must render INDISTINGUISHABLY.
	for i, in := range []string{shortSensitive, longSensitive} {
		idx := 4 + i
		if got[idx] != "[REDACTED]" {
			t.Errorf("sensitive line %d rendered %q, want exactly %q", idx, got[idx], "[REDACTED]")
		}
		if strings.Contains(got[idx], truncationMarker) {
			t.Errorf("sensitive line %d must never carry the truncation marker: %q", idx, got[idx])
		}
		for _, canary := range []string{"S55_TRUNC_SHORT", "S55_TRUNC_LONG", "tail padding"} {
			if strings.Contains(got[idx], canary) {
				t.Errorf("sensitive line %d leaked %q from the raw line %q", idx, canary, in)
			}
		}
	}
	if got[4] != got[5] {
		t.Errorf("short vs long sensitive lines rendered differently (%q vs %q) — the marker leaks length", got[4], got[5])
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
