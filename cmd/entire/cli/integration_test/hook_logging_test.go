//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestHookLogging_WritesToSessionLogFile(t *testing.T) {
	t.Parallel()

	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire()

	// Create a session state file in .git/entire-sessions/ with a known session ID
	sessionID := "test-logging-session-123"
	writeTestSessionStateForLogging(t, env.RepoDir, sessionID)

	// Create the logs directory (Init should create it, but ensure it exists)
	logsDir := filepath.Join(env.RepoDir, paths.EntireDir, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("failed to create logs directory: %v", err)
	}

	// Run a hook with ENTIRE_LOG_LEVEL=debug to ensure logs are written
	// Use post-commit since it takes no arguments
	cmd := exec.CommandContext(t.Context(), getTestBinary(), "hooks", "git", "post-commit")
	cmd.Dir = env.RepoDir
	cmd.Env = append(testutil.GitIsolatedEnv(),
		"ENTIRE_TEST_CLAUDE_PROJECT_DIR="+env.ClaudeProjectDir,
		"ENTIRE_LOG_LEVEL=debug",
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("hook output: %s", output)
		// Don't fail - hook may succeed even with warnings
	}

	// Verify log file was created (all logs go to entire.log)
	logFile := filepath.Join(logsDir, "entire.log")
	if _, err := os.Stat(logFile); os.IsNotExist(err) {
		t.Errorf("expected log file at %s but it doesn't exist", logFile)
		t.Logf("hook stderr/stdout: %s", output)

		// List what's in the logs dir for debugging
		entries, dirErr := os.ReadDir(logsDir)
		if dirErr != nil {
			t.Logf("failed to read logs directory: %v", dirErr)
		}
		t.Logf("logs directory contents: %v", entries)
	}

	// Verify log file contains expected content
	if _, err := os.Stat(logFile); err == nil {
		content, err := os.ReadFile(logFile)
		if err != nil {
			t.Fatalf("failed to read log file: %v", err)
		}

		logContent := string(content)
		t.Logf("log file content:\n%s", logContent)

		// Should contain JSON with hook name
		if !strings.Contains(logContent, `"hook"`) {
			t.Error("log file should contain hook field")
		}
		if !strings.Contains(logContent, `"post-commit"`) {
			t.Error("log file should contain post-commit hook name")
		}
		if !strings.Contains(logContent, `"component"`) {
			t.Error("log file should contain component field")
		}
	}
}

func TestHookLogging_WritesWithoutSession(t *testing.T) {
	t.Parallel()

	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire()

	// Don't create a session state file - logging should still write to entire.log

	// Run a hook with ENTIRE_LOG_LEVEL=debug
	cmd := exec.CommandContext(t.Context(), getTestBinary(), "hooks", "git", "post-commit")
	cmd.Dir = env.RepoDir
	cmd.Env = append(testutil.GitIsolatedEnv(),
		"ENTIRE_TEST_CLAUDE_PROJECT_DIR="+env.ClaudeProjectDir,
		"ENTIRE_LOG_LEVEL=debug",
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		// Don't fail - hook may succeed
		_ = output
	}

	// Log file should still be created (entire.log is fixed, not session-dependent)
	logsDir := filepath.Join(env.RepoDir, paths.EntireDir, "logs")
	logFile := filepath.Join(logsDir, "entire.log")
	content, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("expected entire.log to be created even without session: %v", err)
	}

	// Logs should NOT contain session_id (no session was active)
	logContent := string(content)
	if strings.Contains(logContent, `"session_id"`) {
		t.Error("logs without an active session should not contain session_id")
	}
}

// writeTestSessionStateForLogging creates a session state file for hook logging tests.
func writeTestSessionStateForLogging(t *testing.T, repoDir, sessionID string) {
	t.Helper()
	stateDir := filepath.Join(repoDir, ".git", session.SessionStateDirName)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("failed to create session state directory: %v", err)
	}

	now := time.Now()
	state := session.State{
		SessionID:           sessionID,
		StartedAt:           now,
		LastInteractionTime: &now,
		Phase:               session.PhaseActive,
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("failed to marshal state: %v", err)
	}
	stateFile := filepath.Join(stateDir, sessionID+".json")
	if err := os.WriteFile(stateFile, data, 0o600); err != nil {
		t.Fatalf("failed to write session state file: %v", err)
	}
}

// TestRedactionDiagnostics_ReachEntireLog is the regression test for the
// support report where a user could not tell whether their custom redaction
// rules were loaded: the redact package logged through the process-default
// slog logger (bare stderr — swallowed in hook contexts) because nothing
// wires it to the CLI's .entire/logs/ logger, and the happy path logged
// nothing at all. After the fix, a hook invocation must land two things in
// .entire/logs/entire.log:
//
//  1. component=redaction warnings for broken rules — an inline
//     custom_redactions pattern and a PII custom_pattern that do not
//     compile (the PII one was a second unrouted slog.Warn call site), and
//  2. a load-time "redaction configured" summary line whose counts expose
//     configured-vs-compiled drift, so "are my rules active?" is answerable
//     from the log alone and a broken rule cannot read as an all-clear.
func TestRedactionDiagnostics_ReachEntireLog(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)

	env.PatchSettings(map[string]any{
		"redaction": map[string]any{
			"custom_redactions": map[string]any{
				"good-rule":   "GOODRULE_[A-Z0-9]{4}",
				"broken-rule": "BROKEN_[unclosed",
			},
			"pii": map[string]any{
				"enabled": true,
				"custom_patterns": map[string]any{
					"bad-pii": "PII_[unclosed",
				},
			},
		},
	})
	env.WriteFile(filepath.Join(".entire", "redactors", "acme.yaml"), `name: acme
version: 1.0.0
rules:
  - id: acme-token
    regex: 'ACME_[A-Z0-9]{6}'
`)

	// Any hook invocation initializes logging and configures redaction.
	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("UserPromptSubmit: %v", err)
	}

	logPath := filepath.Join(env.RepoDir, ".entire", "logs", "entire.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read entire.log: %v", err)
	}
	log := string(data)

	// Assert per-line so the component tag is proven on the diagnostic lines
	// themselves — a log-wide substring check would let the summary line's
	// component tag mask an untagged warning.
	findLine := func(marker string) string {
		for _, line := range strings.Split(log, "\n") {
			if strings.Contains(line, marker) {
				return line
			}
		}
		return ""
	}
	for _, marker := range []string{
		"skipping invalid custom_redactions pattern",
		"skipping invalid custom PII pattern",
	} {
		line := findLine(marker)
		if line == "" {
			t.Errorf("entire.log missing compile-failure warning %q", marker)
		} else if !strings.Contains(line, `"component":"redaction"`) {
			t.Errorf("warning line is not tagged component=redaction: %s", line)
		}
	}
	summaryLine := findLine("redaction configured")
	if summaryLine == "" {
		t.Errorf("entire.log missing the load-time 'redaction configured' summary line")
	} else {
		// The counts must reflect the fixture: 2 inline configured but only
		// 1 compiled (+1 pack rule) — a summary claiming everything is
		// active while a rule is broken would be a false all-clear.
		for _, want := range []string{
			`"component":"redaction"`,
			`"packs":1`,
			`"pack_rules":1`,
			`"inline_patterns":2`,
			`"active_rules":2`,
		} {
			if !strings.Contains(summaryLine, want) {
				t.Errorf("summary line missing %s: %s", want, summaryLine)
			}
		}
	}
	if t.Failed() {
		t.Logf("entire.log contents:\n%s", log)
	}
}
