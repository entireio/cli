// Package logging provides structured logging for the Entire CLI using slog.
//
// Usage:
//
//	// Initialize logger for a session (typically at session start)
//	if err := logging.Init(sessionID); err != nil {
//	    // handle error
//	}
//	defer logging.Close()
//
//	// Add context values
//	ctx = logging.WithSession(ctx, sessionID)
//	ctx = logging.WithToolCall(ctx, toolCallID)
//
//	// Log with context - session/tool IDs extracted automatically
//	logging.Info(ctx, "hook invoked",
//	    slog.String("hook", hookName),
//	    slog.String("branch", branch),
//	)
package logging

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/validation"
)

// LogLevelEnvVar is the environment variable that controls log level.
const LogLevelEnvVar = "ENTIRE_LOG_LEVEL"

// LogsDir is the directory where log files are stored (relative to repo root).
const LogsDir = ".entire/logs"

var (
	// logger is the package-level logger instance
	logger *slog.Logger

	// logFile holds the current log file handle for cleanup
	logFile *os.File

	// logBufWriter wraps logFile with buffered I/O for performance
	logBufWriter *bufio.Writer

	// currentSessionID stores the session ID from Init() to include in all logs
	currentSessionID string

	// mu protects logger, logFile, logBufWriter, and currentSessionID
	mu sync.RWMutex

	// logLevelGetter is an optional callback to get log level from settings.
	// Set by SetLogLevelGetter before Init is called.
	logLevelGetter func() string
)

// SetLogLevelGetter sets a callback function to get the log level from settings.
// This allows the logging package to read settings without a circular dependency.
// The callback is only used if ENTIRE_LOG_LEVEL env var is not set.
func SetLogLevelGetter(getter func() string) {
	mu.Lock()
	defer mu.Unlock()
	logLevelGetter = getter
}

// Init initializes the logger for a session, writing JSON logs to
// .entire/logs/<session-id>.log.
//
// If the log file cannot be created, falls back to stderr.
// Log level is controlled by ENTIRE_LOG_LEVEL environment variable.
func Init(sessionID string) error {
	// Validate session ID to prevent path traversal attacks
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return fmt.Errorf("invalid session ID for logging: %w", err)
	}

	mu.Lock()
	defer mu.Unlock()

	// Close any existing log file (flush buffer first)
	if logBufWriter != nil {
		_ = logBufWriter.Flush()
		logBufWriter = nil
	}
	if logFile != nil {
		_ = logFile.Close()
		logFile = nil
	}

	// Get log level from environment first, then settings
	levelStr := os.Getenv(LogLevelEnvVar)
	if levelStr == "" && logLevelGetter != nil {
		levelStr = logLevelGetter()
	}
	level := parseLogLevel(levelStr)

	// Warn if invalid level was provided
	if levelStr != "" && !isValidLogLevel(levelStr) {
		fmt.Fprintf(os.Stderr, "[entire] Warning: invalid log level %q, defaulting to INFO\n", levelStr)
	}

	// Determine log file path
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		// Fall back to current directory
		repoRoot = "."
	}

	logsPath := filepath.Join(repoRoot, LogsDir)
	if err := os.MkdirAll(logsPath, 0o750); err != nil {
		// Fall back to stderr
		logger = createLogger(os.Stderr, level)
		return nil
	}

	logFilePath := filepath.Join(logsPath, sessionID+".log")
	f, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // sessionID validated above
	if err != nil {
		// Fall back to stderr
		logger = createLogger(os.Stderr, level)
		return nil
	}

	logFile = f
	logBufWriter = bufio.NewWriterSize(f, 8192) // 8KB buffer for batched writes
	logger = createLogger(logBufWriter, level)
	currentSessionID = sessionID

	return nil
}

// Close closes the log file if one is open.
// Flushes any buffered data before closing.
// Safe to call multiple times.
func Close() {
	mu.Lock()
	defer mu.Unlock()

	if logBufWriter != nil {
		_ = logBufWriter.Flush()
		logBufWriter = nil
	}
	if logFile != nil {
		_ = logFile.Close()
		logFile = nil
	}
	currentSessionID = ""
}

// resetLogger resets the logger to nil (for testing).
func resetLogger() {
	mu.Lock()
	defer mu.Unlock()
	logger = nil
	currentSessionID = ""
	if logBufWriter != nil {
		_ = logBufWriter.Flush()
		logBufWriter = nil
	}
	if logFile != nil {
		_ = logFile.Close()
		logFile = nil
	}
}

// getLogger returns the current logger, or a default stderr logger if not initialized.
func getLogger() *slog.Logger {
	mu.RLock()
	defer mu.RUnlock()

	if logger == nil {
		// Return default stderr logger
		return slog.Default()
	}
	return logger
}

// getSessionID returns the current session ID (thread-safe).
func getSessionID() string {
	mu.RLock()
	defer mu.RUnlock()
	return currentSessionID
}

// createLogger creates a JSON logger writing to the given writer at the specified level.
func createLogger(w io.Writer, level slog.Level) *slog.Logger {
	opts := &slog.HandlerOptions{
		Level: level,
	}
	handler := slog.NewJSONHandler(w, opts)
	return slog.New(handler)
}

// parseLogLevel parses a log level string to slog.Level.
// Returns slog.LevelInfo for empty or invalid values.
func parseLogLevel(s string) slog.Level {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "DEBUG":
		return slog.LevelDebug
	case "INFO":
		return slog.LevelInfo
	case "WARN", "WARNING":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// isValidLogLevel checks if the given string is a valid log level.
func isValidLogLevel(s string) bool {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "DEBUG", "INFO", "WARN", "WARNING", "ERROR", "":
		return true
	default:
		return false
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

// LogDuration logs a message with duration_ms calculated from the start time.
// The level parameter specifies the log level (use slog.LevelDebug, slog.LevelInfo, etc).
// Designed for use with defer:
//
//	defer logging.LogDuration(ctx, slog.LevelInfo, "operation completed", time.Now())
//
// Or with additional attrs:
//
//	defer logging.LogDuration(ctx, slog.LevelDebug, "hook executed", start,
//	    slog.String("hook", hookName),
//	    slog.Bool("success", true),
//	)
func LogDuration(ctx context.Context, level slog.Level, msg string, start time.Time, attrs ...any) {
	durationMs := time.Since(start).Milliseconds()

	// Prepend duration_ms to attrs
	allAttrs := make([]any, 0, len(attrs)+1)
	allAttrs = append(allAttrs, slog.Int64("duration_ms", durationMs))
	allAttrs = append(allAttrs, attrs...)

	log(ctx, level, msg, allAttrs...)
}

// log is the internal logging function that extracts context values and logs.
func log(ctx context.Context, level slog.Level, msg string, attrs ...any) {
	l := getLogger()

	// Build attributes slice with session ID first (if set)
	var allAttrs []any

	// Add session ID from Init() if set (always first for consistency)
	globalSessionID := getSessionID()
	if globalSessionID != "" {
		allAttrs = append(allAttrs, slog.String("session_id", globalSessionID))
	}

	// Extract context values, skipping session_id if already added from Init()
	contextAttrs := attrsFromContext(ctx, globalSessionID)
	for _, a := range contextAttrs {
		allAttrs = append(allAttrs, a)
	}

	// Add caller-provided attributes
	allAttrs = append(allAttrs, attrs...)

	// Pass nil context to slog as we've already extracted context values as attributes.
	// slog handlers are expected to handle nil context gracefully.
	l.Log(nil, level, msg, allAttrs...) //nolint:staticcheck // nil context is intentional - we extract values as attributes
}

// attrsFromContext extracts logging attributes from a context.
// If globalSessionID is non-empty, skips adding session_id from context to avoid duplicates.
func attrsFromContext(ctx context.Context, globalSessionID string) []slog.Attr {
	if ctx == nil {
		return nil
	}

	var attrs []slog.Attr

	// Only add session_id from context if not already set globally
	if globalSessionID == "" {
		if v := ctx.Value(sessionIDKey); v != nil {
			if s, ok := v.(string); ok && s != "" {
				attrs = append(attrs, slog.String("session_id", s))
			}
		}
	}
	if v := ctx.Value(parentSessionIDKey); v != nil {
		if s, ok := v.(string); ok && s != "" {
			attrs = append(attrs, slog.String("parent_session_id", s))
		}
	}
	if v := ctx.Value(toolCallIDKey); v != nil {
		if s, ok := v.(string); ok && s != "" {
			attrs = append(attrs, slog.String("tool_call_id", s))
		}
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
