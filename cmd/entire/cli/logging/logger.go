// Package logging provides structured logging for the Entire CLI using slog.
//
// A Logger owns one log file. The entry point builds one, puts it in the
// context with WithLogger, and closes it from there; nothing here is
// process-global.
package logging

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// LogLevelEnvVar is the environment variable that controls log level.
const LogLevelEnvVar = "ENTIRE_LOG_LEVEL"

// LogsDir is the directory where log files are stored (relative to repo root).
const LogsDir = ".entire/logs"

const logFileName = "entire.log"

const writeBufferSize = 8192

type Config struct {
	// Dir is created if absent. Required rather than defaulted: only the caller
	// knows whether writing here is allowed.
	Dir string

	// Level is the minimum level to emit; the zero value is slog.LevelInfo.
	Level slog.Level
}

// Logger is safe for concurrent use: slog handlers may be called from several
// goroutines and bufio.Writer is not goroutine-safe, so mu serializes every
// write and Close. Writes after Close are dropped — losing a log line must
// never surface as an error in the caller.
//
// The file is created on first write, not by New, so a command that logs
// nothing (shell completion, version, help) neither pays for opening it nor
// leaves an empty entire.log behind.
type Logger struct {
	slog *slog.Logger

	mu      sync.Mutex
	dir     string
	buf     *bufio.Writer
	file    *os.File
	closed  bool
	openErr error
}

// New returns a Logger that writes JSON to cfg.Dir/entire.log, creating both on
// the first line actually written. It never falls back to stderr: an injected
// logger that wrote to the terminal would splash operational lines over the
// user's output.
//
// A directory that cannot be created or opened is reported by Close, not here —
// by then lines have already been dropped, which is the same outcome as any
// other write failure and must never surface as an error in the caller.
func New(cfg Config) (*Logger, error) {
	if cfg.Dir == "" {
		return nil, errors.New("logging: Config.Dir is required")
	}

	l := &Logger{dir: cfg.Dir}
	l.slog = slog.New(slog.NewJSONHandler(logWriter{l}, &slog.HandlerOptions{Level: cfg.Level}))
	return l, nil
}

// open creates the log file. Caller holds mu. A failure is remembered so the
// next line does not retry the syscalls.
func (l *Logger) open() error {
	if l.openErr != nil {
		return l.openErr
	}
	if err := os.MkdirAll(l.dir, 0o750); err != nil {
		l.openErr = fmt.Errorf("create log directory: %w", err)
		return l.openErr
	}
	path := filepath.Join(l.dir, logFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // fixed filename under a caller-vetted directory
	if err != nil {
		l.openErr = fmt.Errorf("open log file: %w", err)
		return l.openErr
	}
	l.file = f
	l.buf = bufio.NewWriterSize(f, writeBufferSize)
	return nil
}

// Slog returns the underlying *slog.Logger, for packages that take one by
// injection and should not depend on this type. Nil-safe.
func (l *Logger) Slog() *slog.Logger {
	if l == nil {
		return nil
	}
	return l.slog
}

// Close flushes the buffer and closes the log file, and reports a failure to
// open it if one happened. Idempotent, and safe to call concurrently with
// logging: later writes are dropped rather than reopening the file.
func (l *Logger) Close() error {
	if l == nil {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.closed = true
	flushErr := l.openErr
	if l.buf != nil {
		if err := l.buf.Flush(); err != nil {
			flushErr = fmt.Errorf("flush log buffer: %w", err)
		}
		l.buf = nil
	}
	if l.file != nil {
		if err := l.file.Close(); err != nil && flushErr == nil {
			flushErr = fmt.Errorf("close log file: %w", err)
		}
		l.file = nil
	}
	return flushErr
}

// logWriter keeps Write off Logger's public surface.
type logWriter struct{ l *Logger }

func (w logWriter) Write(p []byte) (int, error) {
	w.l.mu.Lock()
	defer w.l.mu.Unlock()

	if w.l.closed {
		return len(p), nil
	}
	if w.l.buf == nil {
		if err := w.l.open(); err != nil {
			return len(p), nil // dropped, reported by Close
		}
	}
	n, err := w.l.buf.Write(p)
	if err != nil {
		return n, fmt.Errorf("write to log buffer: %w", err)
	}
	return n, nil
}

// ParseLevel maps a log level name to a slog.Level. ok is false for an
// unrecognized non-empty name, so the caller can warn about a typo. An empty
// name is INFO and reports ok.
func ParseLevel(s string) (level slog.Level, ok bool) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "":
		return slog.LevelInfo, true
	case "DEBUG":
		return slog.LevelDebug, true
	case "INFO":
		return slog.LevelInfo, true
	case "WARN", "WARNING":
		return slog.LevelWarn, true
	case "ERROR":
		return slog.LevelError, true
	default:
		return slog.LevelInfo, false
	}
}

// Debug logs at DEBUG level with context values automatically extracted.
func Debug(ctx context.Context, msg string, attrs ...any) {
	log(ctx, slog.LevelDebug, msg, attrs...)
}

// Info logs at INFO level with context values automatically extracted.
func Info(ctx context.Context, msg string, attrs ...any) {
	log(ctx, slog.LevelInfo, msg, attrs...)
}

// Warn logs at WARN level with context values automatically extracted.
func Warn(ctx context.Context, msg string, attrs ...any) {
	log(ctx, slog.LevelWarn, msg, attrs...)
}

// Error logs at ERROR level with context values automatically extracted.
func Error(ctx context.Context, msg string, attrs ...any) {
	log(ctx, slog.LevelError, msg, attrs...)
}

// log resolves the logger from the context, falling back to slog.Default() for
// a context built before the entry point ran.
//
// Context attributes lose to caller-supplied ones: slog does not dedupe attrs,
// and over a hundred call sites pass session_id by hand, so emitting both would
// put the key in the line twice.
func log(ctx context.Context, level slog.Level, msg string, attrs ...any) {
	l := LoggerFromContext(ctx).Slog()
	if l == nil {
		l = slog.Default()
	}

	// Before building anything: slog checks the level inside Log, by which point
	// the context walk and both attr slices are already paid for. Most calls
	// here are Debug, suppressed at the default level.
	if !l.Enabled(nil, level) { //nolint:staticcheck // nil context is intentional, as below
		return
	}

	contextAttrs := attrsFromContext(ctx)
	// Only scan the caller's attrs when there is a session to collide with;
	// attrsFromContext emits it first.
	dropSessionID := len(contextAttrs) > 0 &&
		contextAttrs[0].Key == sessionIDAttrKey &&
		hasSessionIDAttr(attrs)

	allAttrs := make([]any, 0, len(contextAttrs)+len(attrs))
	for _, a := range contextAttrs {
		if dropSessionID && a.Key == sessionIDAttrKey {
			continue
		}
		allAttrs = append(allAttrs, a)
	}
	allAttrs = append(allAttrs, attrs...)

	// Context values are already extracted as attributes.
	l.Log(nil, level, msg, allAttrs...) //nolint:staticcheck // nil context is intentional - we extract values as attributes
}

// hasSessionIDAttr reports whether attrs already carry a session_id. attrs
// follows slog's convention: Attr values and loose key/value pairs may be
// mixed, so a string element is a key and the next its value. A session_id
// inside a slog.Group is a different JSON path and does not collide.
func hasSessionIDAttr(attrs []any) bool {
	for i := 0; i < len(attrs); i++ {
		switch a := attrs[i].(type) {
		case slog.Attr:
			if a.Key == sessionIDAttrKey {
				return true
			}
		case string:
			if a == sessionIDAttrKey {
				return true
			}
			i++ // the next element is this key's value, not a key
		}
	}
	return false
}

// attrsFromContext extracts logging attributes from a context. Session comes
// first; log() relies on that ordering.
func attrsFromContext(ctx context.Context) []slog.Attr {
	if ctx == nil {
		return nil
	}

	attrs := make([]slog.Attr, 0, 3) // sized for the three below
	if s := sessionIDFromContext(ctx); s != "" {
		attrs = append(attrs, slog.String(sessionIDAttrKey, s))
	}
	if v := ctx.Value(componentKey); v != nil {
		if s, ok := v.(string); ok && s != "" {
			attrs = append(attrs, slog.String("component", s))
		}
	}
	if v := ctx.Value(agentKey); v != nil {
		if s, ok := v.(string); ok && s != "" {
			attrs = append(attrs, slog.String("agent", s))
		}
	}

	return attrs
}
