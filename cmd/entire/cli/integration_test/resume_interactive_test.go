//go:build integration && unix

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// RunResumeInteractive executes the resume command with a pty, allowing
// interactive prompt responses. The respond function receives the pty for
// reading output and writing input. See RunCommandInteractive for details.
func (env *TestEnv) RunResumeInteractive(branchName string, respond func(ptyFile *os.File) string) (string, error) {
	env.T.Helper()
	return env.RunCommandInteractive([]string{"resume", branchName}, respond)
}

// TestResume_LocalLogNewerTimestamp_UserConfirmsOverwrite tests that when the user
// confirms the overwrite prompt interactively, the local log is replaced.
func TestResume_LocalLogNewerTimestamp_UserConfirmsOverwrite(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	// Create a session with a specific timestamp
	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	content := "def hello; end"
	env.WriteFile("hello.rb", content)

	session.CreateTranscript(
		"Create hello method",
		[]FileChange{{Path: "hello.rb", Content: content}},
	)
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Commit the session's changes (manual-commit requires user to commit)
	env.GitCommitWithShadowHooks("Create hello method", "hello.rb")

	featureBranch := env.GetCurrentBranch()

	// Create a local log with a NEWER timestamp than the checkpoint
	if err := os.MkdirAll(env.ClaudeProjectDir, 0o755); err != nil {
		t.Fatalf("failed to create Claude project dir: %v", err)
	}
	existingLog := filepath.Join(env.ClaudeProjectDir, session.ID+".jsonl")
	futureTimestamp := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	newerContent := fmt.Sprintf(`{"type":"human","timestamp":"%s","message":{"content":"newer local work"}}`, futureTimestamp)
	if err := os.WriteFile(existingLog, []byte(newerContent), 0o644); err != nil {
		t.Fatalf("failed to write existing log: %v", err)
	}

	// Switch to main
	env.GitCheckoutBranch(masterBranch)

	// Resume interactively and confirm the overwrite
	output, err := env.RunResumeInteractive(featureBranch, func(ptyFile *os.File) string {
		out, promptErr := WaitForPromptAndRespond(ptyFile, "[y/N]", "y\n", 10*time.Second)
		if promptErr != nil {
			t.Logf("Warning: %v", promptErr)
		}
		return out
	})
	if err != nil {
		t.Fatalf("resume with user confirmation failed: %v\nOutput: %s", err, output)
	}

	// Verify local log was overwritten with checkpoint content
	data, err := os.ReadFile(existingLog)
	if err != nil {
		t.Fatalf("failed to read log: %v", err)
	}
	if strings.Contains(string(data), "newer local work") {
		t.Errorf("local log should have been overwritten after user confirmed, but still has newer content: %s", string(data))
	}
	if !strings.Contains(string(data), "Create hello method") {
		t.Errorf("restored log should contain checkpoint transcript, got: %s", string(data))
	}
}

// TestResume_LocalLogNewerTimestamp_UserDeclinesOverwrite tests that when the user
// declines the overwrite prompt interactively, the local log is preserved.
func TestResume_LocalLogNewerTimestamp_UserDeclinesOverwrite(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	// Create a session with a specific timestamp
	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	content := "def hello; end"
	env.WriteFile("hello.rb", content)

	session.CreateTranscript(
		"Create hello method",
		[]FileChange{{Path: "hello.rb", Content: content}},
	)
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Commit the session's changes (manual-commit requires user to commit)
	env.GitCommitWithShadowHooks("Create hello method", "hello.rb")

	featureBranch := env.GetCurrentBranch()

	// Create a local log with a NEWER timestamp than the checkpoint
	if err := os.MkdirAll(env.ClaudeProjectDir, 0o755); err != nil {
		t.Fatalf("failed to create Claude project dir: %v", err)
	}
	existingLog := filepath.Join(env.ClaudeProjectDir, session.ID+".jsonl")
	futureTimestamp := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	newerContent := fmt.Sprintf(`{"type":"human","timestamp":"%s","message":{"content":"newer local work"}}`, futureTimestamp)
	if err := os.WriteFile(existingLog, []byte(newerContent), 0o644); err != nil {
		t.Fatalf("failed to write existing log: %v", err)
	}

	// Switch to main
	env.GitCheckoutBranch(masterBranch)

	// Resume interactively and decline the overwrite
	output, err := env.RunResumeInteractive(featureBranch, func(ptyFile *os.File) string {
		out, promptErr := WaitForPromptAndRespond(ptyFile, "[y/N]", "n\n", 10*time.Second)
		if promptErr != nil {
			t.Logf("Warning: %v", promptErr)
		}
		return out
	})
	// Command should succeed (graceful exit) but not overwrite
	t.Logf("Resume with user decline output: %s, err: %v", output, err)

	// Verify local log was NOT overwritten
	data, err := os.ReadFile(existingLog)
	if err != nil {
		t.Fatalf("failed to read log: %v", err)
	}
	if !strings.Contains(string(data), "newer local work") {
		t.Errorf("local log should NOT have been overwritten after user declined, but content changed to: %s", string(data))
	}

	// Output should indicate the resume was cancelled
	if !strings.Contains(output, "cancelled") && !strings.Contains(output, "preserved") {
		t.Logf("Note: Expected 'cancelled' or 'preserved' in output, got: %s", output)
	}
}
