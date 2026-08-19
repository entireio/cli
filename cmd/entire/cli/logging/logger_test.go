package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
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

// testLogFilePath returns the expected log file path for a test temp directory.
func testLogFilePath(tmpDir string) string {
	return filepath.Join(tmpDir, ".entire", "logs", "entire.log")
}

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		want     slog.Level
	}{
		{"empty defaults to INFO", "", slog.LevelInfo},
		{"DEBUG lowercase", "debug", slog.LevelDebug},
		{"DEBUG uppercase", "DEBUG", slog.LevelDebug},
		{"INFO lowercase", "info", slog.LevelInfo},
		{"INFO uppercase", "INFO", slog.LevelInfo},
		{"WARN lowercase", "warn", slog.LevelWarn},
		{"WARN uppercase", "WARN", slog.LevelWarn},
		{"ERROR lowercase", "error", slog.LevelError},
		{"ERROR uppercase", "ERROR", slog.LevelError},
		{"invalid defaults to INFO", "invalid", slog.LevelInfo},
		{"warning alias", "warning", slog.LevelWarn},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseLogLevel(tt.envValue)
			if got != tt.want {
				t.Errorf("parseLogLevel(%q) = %v, want %v", tt.envValue, got, tt.want)
			}
		})
	}
}

func TestInit_CreatesLogDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo so WorktreeRoot works
	initGitRepo(t, tmpDir)

	err := Init(context.Background(), testSessionID)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	defer Close()

	logsDir := filepath.Join(tmpDir, ".entire", "logs")
	if _, err := os.Stat(logsDir); os.IsNotExist(err) {
		t.Errorf("Init() did not create .entire/logs/ directory")
	}
}

func TestInit_CreatesLogFile(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	initGitRepo(t, tmpDir)

	err := Init(context.Background(), testSessionID)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	defer Close()

	if _, err := os.Stat(testLogFilePath(tmpDir)); os.IsNotExist(err) {
		t.Errorf("Init() did not create log file at %s", testLogFilePath(tmpDir))
	}
}

func TestInit_WritesJSONLogs(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	initGitRepo(t, tmpDir)

	sessionID := "2025-01-15-json-test"
	err := Init(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	// Log something
	Info(context.Background(), "test message", slog.String("key", "value"))

	// Close to flush
	Close()

	// Read log file
	content, err := os.ReadFile(testLogFilePath(tmpDir))
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}

	// Parse as JSON
	var logEntry map[string]interface{}
	if err := json.Unmarshal(content, &logEntry); err != nil {
		t.Errorf("Log output is not valid JSON: %v\nContent: %s", err, content)
	}

	// Verify expected fields
	if msg, ok := logEntry["msg"].(string); !ok || msg != "test message" {
		t.Errorf("Expected msg='test message', got %v", logEntry["msg"])
	}
	if key, ok := logEntry["key"].(string); !ok || key != "value" {
		t.Errorf("Expected key='value', got %v", logEntry["key"])
	}
	if _, ok := logEntry["time"]; !ok {
		t.Error("Expected 'time' field in log entry")
	}
	if _, ok := logEntry["level"]; !ok {
		t.Error("Expected 'level' field in log entry")
	}
}

func TestInit_RespectsLogLevel(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	initGitRepo(t, tmpDir)

	// Set log level to WARN
	t.Setenv(LogLevelEnvVar, "WARN")

	sessionID := "2025-01-15-level-test"
	err := Init(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	ctx := context.Background()

	// These should NOT be logged
	Debug(ctx, "debug message")
	Info(ctx, "info message")

	// This SHOULD be logged
	Warn(ctx, "warn message")

	Close()

	// Read log file
	content, err := os.ReadFile(testLogFilePath(tmpDir))
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}

	contentStr := string(content)
	if strings.Contains(contentStr, "debug message") {
		t.Error("DEBUG message should not be logged when level is WARN")
	}
	if strings.Contains(contentStr, "info message") {
		t.Error("INFO message should not be logged when level is WARN")
	}
	if !strings.Contains(contentStr, "warn message") {
		t.Error("WARN message should be logged when level is WARN")
	}
}

func TestInit_InvalidLogLevelWarns(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	initGitRepo(t, tmpDir)

	// Capture stderr
	var buf bytes.Buffer
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Failed to create pipe: %v", err)
	}
	os.Stderr = w

	t.Setenv(LogLevelEnvVar, "INVALID_LEVEL")

	sessionID := "2025-01-15-invalid-level"
	err = Init(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	w.Close()
	os.Stderr = oldStderr

	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("Failed to read from pipe: %v", err)
	}
	stderrOutput := buf.String()

	if !strings.Contains(stderrOutput, "invalid log level") {
		t.Errorf("Expected warning about invalid log level on stderr, got: %s", stderrOutput)
	}

	Close()
}

func TestInit_FallsBackToStderrOnError(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	initGitRepo(t, tmpDir)

	// Make logs directory unwritable (simulate permission error)
	logsDir := filepath.Join(tmpDir, ".entire", "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("Failed to create logs dir: %v", err)
	}

	// Create a directory with the same name as the log file to cause an error
	if err := os.MkdirAll(testLogFilePath(tmpDir), 0o755); err != nil {
		t.Fatalf("Failed to create blocking dir: %v", err)
	}

	// Init should not return error, but fall back to stderr
	err := Init(context.Background(), testSessionID)
	if err != nil {
		t.Errorf("Init() should not error, but got: %v", err)
	}

	// Verify logger still works (writing to stderr)
	Info(context.Background(), "fallback test")

	Close()
}

// EnsureInitialized must route logs to the file for commands that never call
// Init — otherwise they land on the user's terminal via slog.Default().
func TestEnsureInitialized_InitializesWhenUnset(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	initGitRepo(t, tmpDir)
	resetLogger()

	done := EnsureInitialized(context.Background())
	Info(context.Background(), "swept a session")
	done()

	data, err := os.ReadFile(testLogFilePath(tmpDir))
	if err != nil {
		t.Fatalf("EnsureInitialized() did not create a log file: %v", err)
	}
	if !strings.Contains(string(data), "swept a session") {
		t.Errorf("log file missing the message; got %q", string(data))
	}
}

// A caller reachable from a hook must not close the hook's log file out from
// under it, so the cleanup returned for an already-initialized logger is inert.
func TestEnsureInitialized_NoOpWhenAlreadyInitialized(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	initGitRepo(t, tmpDir)
	resetLogger()

	if err := Init(context.Background(), testSessionID); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	defer Close()

	done := EnsureInitialized(context.Background())
	done() // must not tear down the logger installed by Init above

	Info(context.Background(), "still logging after ensure cleanup")
	Close() // flush

	data, err := os.ReadFile(testLogFilePath(tmpDir))
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(data), "still logging after ensure cleanup") {
		t.Errorf("EnsureInitialized cleanup closed a logger it did not open; got %q", string(data))
	}
	if !strings.Contains(string(data), testSessionID) {
		t.Errorf("session ID from the original Init was lost; got %q", string(data))
	}
}

// Logging after the cleanup must fall back to slog.Default(), not disappear.
// The handler holds the buffered writer by value, so a logger left in place
// after Close writes into an orphaned buffer over a closed file: short lines are
// never flushed and longer ones fail. `entire doctor` hits exactly this — it
// keeps condensing sessions after the exited-session sweep tears its logging
// down.
func TestEnsureInitialized_CleanupRestoresFallback(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	initGitRepo(t, tmpDir)
	resetLogger()

	done := EnsureInitialized(context.Background())
	Info(context.Background(), "during the sweep")
	done()

	var fallback bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&fallback, nil)))
	defer slog.SetDefault(prev)

	Info(context.Background(), "after the sweep")

	if !strings.Contains(fallback.String(), "after the sweep") {
		t.Errorf("line logged after cleanup was swallowed; slog.Default() got %q", fallback.String())
	}
	data, err := os.ReadFile(testLogFilePath(tmpDir))
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(data), "during the sweep") {
		t.Errorf("log file missing the swept line; got %q", string(data))
	}
}

func TestClose_SafeToCallMultipleTimes(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	initGitRepo(t, tmpDir)

	sessionID := "2025-01-15-close-test"
	err := Init(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	// Should not panic
	Close()
	Close()
	Close()
}

func TestLogging_BeforeInit(_ *testing.T) {
	// Reset any global state
	resetLogger()

	// These should not panic, should use default stderr logger
	ctx := context.Background()
	Debug(ctx, "debug before init")
	Info(ctx, "info before init")
	Warn(ctx, "warn before init")
	Error(ctx, "error before init")
}

// Helper to initialize a git repo for tests
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	t.Chdir(dir)
	cmd := "git init && git config user.email 'test@test.com' && git config user.name 'Test'"
	output, err := execCommand(t, "sh", "-c", cmd)
	if err != nil {
		t.Fatalf("Failed to init git repo: %v\nOutput: %s", err, output)
	}
}

func execCommand(t *testing.T, name string, args ...string) (string, error) {
	t.Helper()
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func TestLogging_IncludesContextValues(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	initGitRepo(t, tmpDir)

	sessionID := "2025-01-15-context-test"
	err := Init(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	// Create context with values
	ctx := context.Background()
	ctx = WithComponent(ctx, testComponent)
	ctx = WithAgent(ctx, testAgent)

	// Log with context
	Info(ctx, "context test message")

	Close()

	// Read log file
	content, err := os.ReadFile(testLogFilePath(tmpDir))
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}

	// Parse as JSON
	var logEntry map[string]interface{}
	if err := json.Unmarshal(content, &logEntry); err != nil {
		t.Fatalf("Log output is not valid JSON: %v\nContent: %s", err, content)
	}

	// session_id comes from Init()
	if logEntry["session_id"] != sessionID {
		t.Errorf("Expected session_id='%s' (from Init), got %v", sessionID, logEntry["session_id"])
	}
	if logEntry["component"] != testComponent {
		t.Errorf("Expected component='%s', got %v", testComponent, logEntry["component"])
	}
	if logEntry["agent"] != testAgent {
		t.Errorf("Expected agent='%s', got %v", testAgent, logEntry["agent"])
	}
}

func TestLogging_AdditionalAttrs(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	initGitRepo(t, tmpDir)

	sessionID := "2025-01-15-attrs-test"
	err := Init(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	ctx := context.Background()

	// Log with additional attrs
	Info(ctx, "attrs test",
		slog.String("hook", "pre-push"),
		slog.Int("duration_ms", 150),
		slog.Bool("success", true),
	)

	Close()

	// Read log file
	content, err := os.ReadFile(testLogFilePath(tmpDir))
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}

	// Parse as JSON
	var logEntry map[string]interface{}
	if err := json.Unmarshal(content, &logEntry); err != nil {
		t.Fatalf("Log output is not valid JSON: %v\nContent: %s", err, content)
	}

	// session_id comes from Init(), additional attrs work alongside
	if logEntry["session_id"] != sessionID {
		t.Errorf("Expected session_id='%s' (from Init), got %v", sessionID, logEntry["session_id"])
	}
	if logEntry["hook"] != "pre-push" {
		t.Errorf("Expected hook='pre-push', got %v", logEntry["hook"])
	}
	if logEntry["duration_ms"] != float64(150) {
		t.Errorf("Expected duration_ms=150, got %v", logEntry["duration_ms"])
	}
	if logEntry["success"] != true {
		t.Errorf("Expected success=true, got %v", logEntry["success"])
	}
}

func TestLogging_ConcurrentInitAndLog(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	initGitRepo(t, tmpDir)

	if err := Init(context.Background(), ""); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	defer Close()

	const (
		logGoroutines   = 8
		initGoroutines  = 4
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
				Info(context.Background(), "concurrent log", slog.Int("worker", worker), slog.Int("iteration", j))
			}
		}(i)
	}

	for range initGoroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range iterations {
				if err := Init(context.Background(), ""); err != nil {
					t.Errorf("Init() error = %v", err)
					return
				}
			}
		}()
	}

	for range closeGoroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range iterations {
				Close()
			}
		}()
	}

	close(start)
	wg.Wait()
}

func TestInit_RejectsInvalidSessionIDs(t *testing.T) {
	tests := []struct {
		name      string
		sessionID string
		wantErr   bool
	}{
		{"empty session ID is allowed", "", false},
		{"path traversal with slash", "../../../tmp/evil", true},
		{"path traversal with backslash", "..\\..\\tmp\\evil", true},
		{"contains forward slash", "2025-01-15/session", true},
		{"contains backslash", "2025-01-15\\session", true},
		{"valid session ID", "2025-01-15-valid-session", false},
		{"valid UUID-like ID", "abc123-def456-ghi789", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset logger state before each test
			resetLogger()

			// Set up git repo for cases that we expect to succeed
			if !tt.wantErr {
				tmpDir := t.TempDir()
				t.Chdir(tmpDir)
				initGitRepo(t, tmpDir)
			}

			err := Init(context.Background(), tt.sessionID)
			if (err != nil) != tt.wantErr {
				t.Errorf("Init(%q) error = %v, wantErr %v", tt.sessionID, err, tt.wantErr)
			}
			if err != nil && tt.wantErr {
				// Verify error message mentions session ID
				if !strings.Contains(err.Error(), "session ID") {
					t.Errorf("Init(%q) error should mention 'session ID', got: %v", tt.sessionID, err)
				}
			}
			Close()
		})
	}
}
