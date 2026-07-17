package logger

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
)

// --- test doubles ---

// recordingHandler records the messages it receives, guarded for concurrent use.
type recordingHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (r *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (r *recordingHandler) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	r.msgs = append(r.msgs, rec.Message)
	r.mu.Unlock()
	return nil
}

func (r *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *recordingHandler) WithGroup(string) slog.Handler      { return r }

func (r *recordingHandler) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.msgs)
}

// errorHandler always fails, standing in for a stdout write that returns EPIPE.
type errorHandler struct{ err error }

func (h errorHandler) Enabled(context.Context, slog.Level) bool  { return true }
func (h errorHandler) Handle(context.Context, slog.Record) error { return h.err }
func (h errorHandler) WithAttrs([]slog.Attr) slog.Handler        { return h }
func (h errorHandler) WithGroup(string) slog.Handler             { return h }

// blockingWriter blocks every Write until release is closed, simulating a
// wedged stdout consumer (a full Docker log pipe whose reader stopped draining).
type blockingWriter struct{ release chan struct{} }

func (b *blockingWriter) Write(p []byte) (int, error) {
	<-b.release
	return len(p), nil
}

// syncBuffer is a concurrency-safe io.Writer capturing output for assertions.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *syncBuffer) lineCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return bytes.Count(s.buf.Bytes(), []byte("\n"))
}

func infoRecord(msg string) slog.Record {
	return slog.NewRecord(time.Now(), slog.LevelInfo, msg, 0)
}

// --- fanout error handling (issue 2) ---

// TestFanoutContinuesAfterHandlerError guards the fix for the file-log-loss bug:
// when an earlier handler (stdout) errors, the fanout must still deliver the
// record to the remaining handlers (the file), and surface the error rather
// than swallow it.
func TestFanoutContinuesAfterHandlerError(t *testing.T) {
	boom := errors.New("stdout write failed")
	rec := &recordingHandler{}
	fh := fanoutHandler{handlers: []slog.Handler{errorHandler{err: boom}, rec}}

	err := fh.Handle(context.Background(), infoRecord("hello"))
	if err == nil {
		t.Fatal("fanout must surface the failing handler's error")
	}
	if !errors.Is(err, boom) {
		t.Errorf("expected the joined error to contain the handler error, got %v", err)
	}
	if rec.count() != 1 {
		t.Errorf("the second handler must still receive the record despite the first erroring, got %d records", rec.count())
	}
}

// TestFanoutJoinsMultipleErrors confirms every failing handler's error is
// reported, not just the first.
func TestFanoutJoinsMultipleErrors(t *testing.T) {
	e1, e2 := errors.New("one"), errors.New("two")
	rec := &recordingHandler{}
	fh := fanoutHandler{handlers: []slog.Handler{errorHandler{err: e1}, errorHandler{err: e2}, rec}}

	err := fh.Handle(context.Background(), infoRecord("hi"))
	if !errors.Is(err, e1) || !errors.Is(err, e2) {
		t.Errorf("expected both handler errors joined, got %v", err)
	}
	if rec.count() != 1 {
		t.Errorf("the healthy handler must still receive the record, got %d", rec.count())
	}
}

// --- console non-blocking write (issue 1) ---

// TestConsoleWriterHungStdoutDoesNotBlockCaller is the core of issue 1: a wedged
// stdout consumer must never block the logging goroutine. With a permanently
// blocked writer, Handle must keep returning promptly (dropping lines once the
// buffer fills) instead of stalling. If it stalled, this test would hang.
func TestConsoleWriterHungStdoutDoesNotBlockCaller(t *testing.T) {
	blocked := &blockingWriter{release: make(chan struct{})}
	h := newConsoleHandler(blocked, slog.LevelDebug, false)

	done := make(chan struct{})
	go func() {
		for i := 0; i < consoleBufferLines+500; i++ {
			_ = h.Handle(context.Background(), infoRecord("line"))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("logging stalled behind a wedged stdout consumer")
	}

	if h.cw.Dropped() == 0 {
		t.Error("expected lines to be dropped once the buffer filled behind the wedged writer")
	}

	close(blocked.release) // let the background writer drain and exit
	h.cw.Close()
}

// TestFanoutHungConsoleDoesNotStallOtherHandlers proves the whole-process stall
// is gone: even with the console writer wedged, the file (recording) handler in
// the same fanout still receives every record, and the callers never block.
func TestFanoutHungConsoleDoesNotStallOtherHandlers(t *testing.T) {
	blocked := &blockingWriter{release: make(chan struct{})}
	console := newConsoleHandler(blocked, slog.LevelDebug, false)
	rec := &recordingHandler{}
	log := slog.New(fanoutHandler{handlers: []slog.Handler{console, rec}})

	const total = consoleBufferLines + 200
	done := make(chan struct{})
	go func() {
		for i := 0; i < total; i++ {
			log.Info("msg", "i", i)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fanout stalled behind the wedged console writer")
	}

	if got := rec.count(); got != total {
		t.Errorf("the non-wedged handler must receive every record; got %d want %d", got, total)
	}
	if console.cw.Dropped() == 0 {
		t.Error("expected some console lines dropped while stdout was wedged")
	}

	close(blocked.release)
	console.cw.Close()
}

// TestConsoleHandlerConcurrentWriters exercises many goroutines logging through
// one handler at once (run under -race). Every line is accounted for: written to
// the sink or counted as dropped, never silently lost.
func TestConsoleHandlerConcurrentWriters(t *testing.T) {
	var sink syncBuffer
	h := newConsoleHandler(&sink, slog.LevelDebug, false)

	const goroutines, perG = 8, 300
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				_ = h.Handle(context.Background(), infoRecord("line"))
			}
		}()
	}
	wg.Wait()
	h.cw.Close() // flush queued lines; Wait inside establishes happens-before for the read

	const total = goroutines * perG
	written := sink.lineCount()
	dropped := int(h.cw.Dropped())
	if written+dropped != total {
		t.Errorf("every line must be written or dropped: written=%d dropped=%d total=%d", written, dropped, total)
	}
	// A generous buffer with a fast sink should not drop anything.
	if dropped != 0 {
		t.Errorf("no line should drop with a fast sink, dropped=%d", dropped)
	}
}

// TestConsoleWriterPreservesOrderAndContent verifies the async path keeps lines
// intact and in order (single-consumer goroutine), and that Close flushes them.
func TestConsoleWriterPreservesOrderAndContent(t *testing.T) {
	var sink syncBuffer
	h := newConsoleHandler(&sink, slog.LevelDebug, false)

	const n = 500
	for i := 0; i < n; i++ {
		_ = h.Handle(context.Background(), infoRecord("m"+strconv.Itoa(i)))
	}
	h.cw.Close()

	lines := strings.Split(strings.TrimRight(sink.String(), "\n"), "\n")
	if len(lines) != n {
		t.Fatalf("expected %d flushed lines, got %d", n, len(lines))
	}
	for i, line := range lines {
		want := "m" + strconv.Itoa(i)
		if !strings.Contains(line, want) {
			t.Fatalf("line %d out of order or corrupted: want it to contain %q, got %q", i, want, line)
		}
	}
}

// TestConsoleHandlerColorizes checks the decorate refactor still wraps a styled
// record in its ANSI color and emoji when coloring is on.
func TestConsoleHandlerColorizes(t *testing.T) {
	var sink syncBuffer
	h := newConsoleHandler(&sink, slog.LevelDebug, true)

	_ = h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelError, "boom", 0))
	h.cw.Close()

	out := sink.String()
	if !strings.Contains(out, ansiRed) || !strings.Contains(out, ansiReset) {
		t.Errorf("expected an error line wrapped in red + reset, got %q", out)
	}
	if !strings.Contains(out, "🔴") {
		t.Errorf("expected the error emoji prefix, got %q", out)
	}
}

// TestConsoleHandlerPlainWhenColorOff confirms color-off output is the raw
// TextHandler line with no ANSI codes.
func TestConsoleHandlerPlainWhenColorOff(t *testing.T) {
	var sink syncBuffer
	h := newConsoleHandler(&sink, slog.LevelDebug, false)

	_ = h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelError, "boom", 0))
	h.cw.Close()

	if out := sink.String(); strings.Contains(out, "\033[") {
		t.Errorf("color-off output must contain no ANSI escapes, got %q", out)
	}
}

// TestConsoleWriterCloseIdempotent guards the sync.Once close.
func TestConsoleWriterCloseIdempotent(t *testing.T) {
	h := newConsoleHandler(&syncBuffer{}, slog.LevelDebug, false)
	h.cw.Close()
	h.cw.Close() // must not panic on the second close
}

// TestConsoleWriterWriteDuringCloseDoesNotPanic is the regression for the
// shutdown panic: several goroutines keep logging while another calls Close
// concurrently. Sending on a closed channel panics in Go (and -race does not
// catch it), so if Close closed the lines channel this would crash. A write
// racing shutdown must instead drop safely. A panic in any goroutine crashes
// the test binary, so completing the loop is the assertion.
func TestConsoleWriterWriteDuringCloseDoesNotPanic(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		var sink syncBuffer
		h := newConsoleHandler(&sink, slog.LevelDebug, false)

		var writers sync.WaitGroup
		for g := 0; g < 6; g++ {
			writers.Add(1)
			go func() {
				defer writers.Done()
				for i := 0; i < 500; i++ {
					// Handle -> cw.write; must never panic even after Close.
					_ = h.Handle(context.Background(), infoRecord("x"))
				}
			}()
		}

		// Close concurrently with the in-flight writers.
		closed := make(chan struct{})
		go func() {
			h.cw.Close()
			close(closed)
		}()

		writers.Wait()
		<-closed // Close (and thus run's drain+exit) has finished
	}
}

// TestSetupFileLogStaysPlainWithColoredOn is the end-to-end guard for the
// "Colored" setting's scope: with coloring ON, the console handler decorates
// stdout while the on-disk file — the one the web Logs page reads — stays
// plain slog text with neither ANSI escapes nor emoji.
func TestSetupFileLogStaysPlainWithColoredOn(t *testing.T) {
	t.Chdir(t.TempDir())
	prev := slog.Default()
	defer slog.SetDefault(prev)

	l, err := Setup("plaintester", config.LoggerSettings{
		Save:         true,
		Colored:      true,
		ConsoleLevel: "INFO",
		FileLevel:    "INFO",
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	slog.Info("Streamer is online", "streamer", "shroud")
	slog.Warn("careful")
	l.Close()

	data, err := os.ReadFile(LogFilePath("plaintester"))
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	out := string(data)
	if strings.Contains(out, "\033[") {
		t.Errorf("file log contains ANSI escape sequences:\n%s", out)
	}
	for _, emoji := range []string{"🟢", "⚠️"} {
		if strings.Contains(out, emoji) {
			t.Errorf("file log contains console emoji %q:\n%s", emoji, out)
		}
	}
	if !strings.Contains(out, `msg="Streamer is online"`) || !strings.Contains(out, "msg=careful") {
		t.Errorf("file log lost its plain-text records:\n%s", out)
	}
}
