package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Test constants to avoid goconst warnings
const (
	testSessionID = "2025-01-15-test-session"
	testComponent = "hooks"
	testAgent     = "claude-code"
)

// newTestLogger builds a Logger under a fresh temp directory and returns it
// alongside the path of the file it writes to.
func newTestLogger(t *testing.T, level slog.Level) (*Logger, string) {
	t.Helper()

	dir := filepath.Join(t.TempDir(), LogsDir)
	l, err := New(Config{Dir: dir, Level: level})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, filepath.Join(dir, logFileName)
}

// bufferLogger returns a Logger writing to buf, for assertions that need the
// raw line rather than a file.
func bufferLogger(buf *bytes.Buffer) *Logger {
	return &Logger{slog: slog.New(slog.NewJSONHandler(buf, nil))}
}

// readLog closes the logger to flush its buffer, then returns the file content.
func readLog(t *testing.T, l *Logger, path string) string {
	t.Helper()

	if err := l.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "" // nothing was ever written, so the file was never created
	}
	if err != nil {
		t.Fatalf("failed to read log file: %v", err)
	}
	return string(content)
}

func TestParseLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		input  string
		want   slog.Level
		wantOK bool
	}{
		{"empty defaults to INFO", "", slog.LevelInfo, true},
		{"DEBUG lowercase", "debug", slog.LevelDebug, true},
		{"DEBUG uppercase", "DEBUG", slog.LevelDebug, true},
		{"INFO mixed case", "Info", slog.LevelInfo, true},
		{"WARN", "warn", slog.LevelWarn, true},
		{"warning alias", "warning", slog.LevelWarn, true},
		{"ERROR", "error", slog.LevelError, true},
		{"surrounding whitespace", "  debug  ", slog.LevelDebug, true},
		{"unrecognized reports not-ok at INFO", "invalid", slog.LevelInfo, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := ParseLevel(tt.input)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("ParseLevel(%q) = (%v, %v), want (%v, %v)", tt.input, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// The file appears on the first line written, not at New: a command that logs
// nothing must not pay for opening it or leave an empty entire.log behind.
func TestNew_CreatesFileOnFirstWrite(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "nested", LogsDir)
	l, err := New(Config{Dir: dir})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = l.Close() }()
	path := filepath.Join(dir, logFileName)

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("New() created the log file before anything was logged (stat err = %v)", err)
	}

	Info(WithLogger(context.Background(), l), "first line")
	if err := l.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("first write did not create the log file: %v", err)
	}
}

func TestNew_RejectsEmptyDir(t *testing.T) {
	t.Parallel()

	if _, err := New(Config{}); err == nil {
		t.Error("New() with no Dir = nil error, want an error")
	}
}

// An unusable directory must not fail the caller — losing a log line is never
// worth an error a command has to handle — but Close reports it, so it is not
// lost entirely.
func TestLogger_UnusableDirDropsLinesAndCloseReports(t *testing.T) {
	t.Parallel()

	occupied := filepath.Join(t.TempDir(), "logs")
	if err := os.WriteFile(occupied, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("failed to write blocking file: %v", err)
	}
	l, err := New(Config{Dir: occupied})
	if err != nil {
		t.Fatalf("New() error = %v; an unusable dir must surface at Close, not here", err)
	}

	Warn(WithLogger(context.Background(), l), "dropped")

	if err := l.Close(); err == nil {
		t.Error("Close() = nil error after the log file could not be opened")
	}
}

func TestLogger_WritesJSON(t *testing.T) {
	t.Parallel()

	l, path := newTestLogger(t, slog.LevelInfo)
	ctx := WithLogger(context.Background(), l)
	Info(ctx, "json test message", slog.String("key", "value"))

	line := strings.TrimSpace(readLog(t, l, path))
	var entry map[string]any
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		t.Fatalf("log output is not valid JSON: %v\nContent: %s", err, line)
	}
	if entry["msg"] != "json test message" {
		t.Errorf("msg = %v, want %q", entry["msg"], "json test message")
	}
	if entry["key"] != "value" {
		t.Errorf("key = %v, want %q", entry["key"], "value")
	}
	if entry["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", entry["level"])
	}
}

func TestLogger_RespectsConfiguredLevel(t *testing.T) {
	t.Parallel()

	l, path := newTestLogger(t, slog.LevelWarn)
	ctx := WithLogger(context.Background(), l)
	Debug(ctx, "debug is below the threshold")
	Info(ctx, "info is below the threshold")
	Warn(ctx, "warn is at the threshold")

	content := readLog(t, l, path)
	if strings.Contains(content, "below the threshold") {
		t.Errorf("lines below the configured level were written: %s", content)
	}
	if !strings.Contains(content, "warn is at the threshold") {
		t.Errorf("line at the configured level is missing: %s", content)
	}
}

// TestLoggersAreIndependent is the payoff of owning no package state: two
// loggers configured differently coexist, and neither is disturbed by the
// other's construction or close.
func TestLoggersAreIndependent(t *testing.T) {
	t.Parallel()

	quiet, quietPath := newTestLogger(t, slog.LevelError)
	chatty, chattyPath := newTestLogger(t, slog.LevelDebug)

	Info(WithLogger(context.Background(), quiet), "quiet line")
	Info(WithLogger(context.Background(), chatty), "chatty line")

	if got := readLog(t, quiet, quietPath); strings.Contains(got, "quiet line") {
		t.Errorf("line below the quiet logger's level was written: %s", got)
	}
	got := readLog(t, chatty, chattyPath)
	if !strings.Contains(got, "chatty line") {
		t.Errorf("chatty logger missing its own line: %s", got)
	}
	if strings.Contains(got, "quiet line") {
		t.Errorf("chatty logger received the other logger's line: %s", got)
	}
}

func TestClose_IsIdempotentAndDropsLaterWrites(t *testing.T) {
	t.Parallel()

	l, path := newTestLogger(t, slog.LevelInfo)
	ctx := WithLogger(context.Background(), l)
	Info(ctx, "before close")

	for range 3 {
		if err := l.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}

	// Losing a log line must never surface as a failure in the caller.
	Info(ctx, "after close")

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read log file: %v", err)
	}
	if !strings.Contains(string(content), "before close") {
		t.Errorf("line logged before Close is missing: %s", content)
	}
	if strings.Contains(string(content), "after close") {
		t.Errorf("line logged after Close was written: %s", content)
	}
}

// TestLogger_ConcurrentWritesAndClose guards the instance mutex: slog handlers
// may be called from several goroutines and bufio.Writer is not goroutine-safe,
// so writes racing each other or racing Close would corrupt the buffer.
func TestLogger_ConcurrentWritesAndClose(t *testing.T) {
	t.Parallel()

	l, _ := newTestLogger(t, slog.LevelDebug)
	ctx := WithLogger(context.Background(), l)

	const (
		logGoroutines   = 8
		closeGoroutines = 2
		iterations      = 200
	)

	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := range logGoroutines {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-start
			for j := range iterations {
				Info(ctx, "concurrent log", slog.Int("worker", worker), slog.Int("iteration", j))
			}
		}(i)
	}
	for range closeGoroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range iterations {
				if err := l.Close(); err != nil {
					t.Errorf("Close() error = %v", err)
					return
				}
			}
		}()
	}

	close(start)
	wg.Wait()
}

// TestLog_ResolvesLoggerFromContext pins the single resolution path: a caller
// logs exactly where downstream packages that received the same logger by
// injection log, and a context carrying no logger never reaches it.
func TestLog_ResolvesLoggerFromContext(t *testing.T) {
	t.Parallel()

	l, path := newTestLogger(t, slog.LevelInfo)

	Warn(WithLogger(context.Background(), l), "routed to the injected logger")
	Warn(context.Background(), "routed to the default logger")

	content := readLog(t, l, path)
	if !strings.Contains(content, "routed to the injected logger") {
		t.Errorf("line did not reach the injected logger: %s", content)
	}
	if strings.Contains(content, "routed to the default logger") {
		t.Errorf("a logger-less context reached the injected logger: %s", content)
	}
}

func TestLogging_IncludesContextValues(t *testing.T) {
	t.Parallel()

	l, path := newTestLogger(t, slog.LevelInfo)
	ctx := WithLogger(context.Background(), l)
	ctx = WithSessionID(ctx, testSessionID)
	ctx = WithComponent(ctx, testComponent)
	ctx = WithAgent(ctx, testAgent)

	Info(ctx, "context test message")

	var entry map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(readLog(t, l, path))), &entry); err != nil {
		t.Fatalf("log output is not valid JSON: %v", err)
	}
	for key, want := range map[string]string{
		"session_id": testSessionID,
		"component":  testComponent,
		"agent":      testAgent,
	} {
		if entry[key] != want {
			t.Errorf("%s = %v, want %q", key, entry[key], want)
		}
	}
}

func TestLogging_AdditionalAttrs(t *testing.T) {
	t.Parallel()

	l, path := newTestLogger(t, slog.LevelInfo)
	ctx := WithSessionID(WithLogger(context.Background(), l), testSessionID)

	Info(ctx, "attrs test",
		slog.String("hook", "pre-push"),
		slog.Int("duration_ms", 150),
		slog.Bool("success", true),
	)

	var entry map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(readLog(t, l, path))), &entry); err != nil {
		t.Fatalf("log output is not valid JSON: %v", err)
	}
	if entry["session_id"] != testSessionID {
		t.Errorf("session_id = %v, want %q", entry["session_id"], testSessionID)
	}
	if entry["hook"] != "pre-push" {
		t.Errorf("hook = %v, want pre-push", entry["hook"])
	}
	if entry["duration_ms"] != float64(150) {
		t.Errorf("duration_ms = %v, want 150", entry["duration_ms"])
	}
	if entry["success"] != true {
		t.Errorf("success = %v, want true", entry["success"])
	}
}

// TestLog_CallerSuppliedSessionIDWins guards the collision between the session
// on the context and the one over a hundred call sites still pass by hand. slog
// does not dedupe attrs, so emitting both puts session_id in the JSON line
// twice; the caller's must be the one that survives.
func TestLog_CallerSuppliedSessionIDWins(t *testing.T) {
	t.Parallel()

	callerSessionID := "caller-supplied-session"
	tests := []struct {
		name  string
		attrs []any
	}{
		{"as slog.Attr", []any{slog.String("session_id", callerSessionID)}},
		{"as a loose key/value pair", []any{"session_id", callerSessionID}},
		{
			"after a loose pair whose value collides with the key",
			[]any{"resolved_from", "session_id", slog.String("session_id", callerSessionID)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			ctx := WithSessionID(WithLogger(context.Background(), bufferLogger(&buf)), "context-session")
			Warn(ctx, "stamped once", tt.attrs...)

			line := buf.String()
			// Counting the key with its colon: the third case deliberately
			// passes "session_id" as a *value*, which must not be mistaken for
			// a second key here any more than it is by log() itself.
			if got := strings.Count(line, `"session_id":`); got != 1 {
				t.Errorf("session_id appears %d times, want 1: %s", got, line)
			}
			var entry map[string]any
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatalf("log output is not valid JSON: %v\nContent: %s", err, line)
			}
			if entry["session_id"] != callerSessionID {
				t.Errorf("session_id = %v, want the caller's %q", entry["session_id"], callerSessionID)
			}
		})
	}
}

// TestLog_ContextSessionIDIsScoped pins what the context value buys over the
// package global it replaced: re-stamping a derived context shadows the session
// for that scope only, leaving the parent's lines attributed to the parent.
func TestLog_ContextSessionIDIsScoped(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	outer := WithSessionID(WithLogger(context.Background(), bufferLogger(&buf)), "outer-session")
	inner := WithSessionID(outer, "inner-session")

	Warn(inner, "inner line")
	Warn(outer, "outer line")

	for _, want := range []struct{ msg, sessionID string }{
		{"inner line", "inner-session"},
		{"outer line", "outer-session"},
	} {
		var found bool
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if !strings.Contains(line, want.msg) {
				continue
			}
			found = true
			var entry map[string]any
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatalf("log output is not valid JSON: %v\nContent: %s", err, line)
			}
			if entry["session_id"] != want.sessionID {
				t.Errorf("%q: session_id = %v, want %q", want.msg, entry["session_id"], want.sessionID)
			}
		}
		if !found {
			t.Errorf("%q missing from log output: %s", want.msg, buf.String())
		}
	}
}
