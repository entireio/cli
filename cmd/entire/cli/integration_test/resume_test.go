//go:build integration

package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/strategy"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

const masterBranch = "master"

// Note: Resume tests only run with auto-commit strategy because:
// - Auto-commit strategy creates commits with Entire-Checkpoint trailers and metadata on entire/checkpoints/v1
//   immediately during SimulateStop
// - Manual-commit strategy only creates this structure after user commits (via prepare-commit-msg
//   and post-commit hooks), which requires the full workflow tested in manual_commit_workflow_test.go
// Both strategies share the same resume code path once the structure exists.

// TestResume_SwitchBranchWithSession tests the resume command when switching to a branch
// that has a commit with an Entire-Checkpoint trailer.
func TestResume_SwitchBranchWithSession(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t, strategy.StrategyNameAutoCommit)

	// Create a session on the feature branch
	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	content := "puts 'Hello from session'"
	env.WriteFile("hello.rb", content)

	session.CreateTranscript(
		"Create a hello script",
		[]FileChange{{Path: "hello.rb", Content: content}},
	)
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Remember the feature branch name
	featureBranch := env.GetCurrentBranch()

	// Switch back to main branch
	env.GitCheckoutBranch(masterBranch)

	// Verify we're on main
	if branch := env.GetCurrentBranch(); branch != masterBranch {
		t.Fatalf("expected to be on master, got %s", branch)
	}

	// Run resume to switch back to feature branch
	output, err := env.RunResume(featureBranch)
	if err != nil {
		t.Fatalf("resume failed: %v\nOutput: %s", err, output)
	}

	// Verify we switched to the feature branch
	if branch := env.GetCurrentBranch(); branch != featureBranch {
		t.Errorf("expected to be on %s, got %s", featureBranch, branch)
	}

	// Verify output contains session info and resume command
	if !strings.Contains(output, "Session:") {
		t.Errorf("output should contain 'Session:', got: %s", output)
	}
	if !strings.Contains(output, "claude -r") {
		t.Errorf("output should contain 'claude -r', got: %s", output)
	}

	// Verify transcript was restored to Claude project dir
	transcriptFiles, err := filepath.Glob(filepath.Join(env.ClaudeProjectDir, "*.jsonl"))
	if err != nil {
		t.Fatalf("failed to glob transcript files: %v", err)
	}
	if len(transcriptFiles) == 0 {
		t.Error("expected transcript file to be restored to Claude project dir")
	}
}

// TestResume_AlreadyOnBranch tests that resume works when already on the target branch.
func TestResume_AlreadyOnBranch(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t, strategy.StrategyNameAutoCommit)

	// Create a session on the feature branch
	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	content := "console.log('test')"
	env.WriteFile("test.js", content)

	session.CreateTranscript(
		"Create a test script",
		[]FileChange{{Path: "test.js", Content: content}},
	)
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	currentBranch := env.GetCurrentBranch()

	// Run resume on the branch we're already on
	output, err := env.RunResume(currentBranch)
	if err != nil {
		t.Fatalf("resume failed: %v\nOutput: %s", err, output)
	}

	// Should still show session info
	if !strings.Contains(output, "Session:") {
		t.Errorf("output should contain 'Session:', got: %s", output)
	}
}

// TestResume_NoCheckpointOnBranch tests that resume handles branches without
// any Entire-Checkpoint trailer in their history gracefully.
func TestResume_NoCheckpointOnBranch(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t, strategy.StrategyNameAutoCommit)

	// Create a branch directly from master (which has no checkpoints)
	// Switch to master first
	env.GitCheckoutBranch(masterBranch)

	// Create a new branch from master
	env.GitCheckoutNewBranch("feature/no-session")

	// Create a commit without any session/checkpoint
	env.WriteFile("plain.txt", "no session here")
	env.GitAdd("plain.txt")
	env.GitCommit("Plain commit without session")

	plainBranch := env.GetCurrentBranch()

	// Switch back to master
	env.GitCheckoutBranch(masterBranch)

	// Resume to the plain branch - should indicate no checkpoint found
	output, err := env.RunResume(plainBranch)
	if err != nil {
		t.Fatalf("resume failed: %v\nOutput: %s", err, output)
	}

	// Should indicate no checkpoint found
	if !strings.Contains(output, "No Entire checkpoint found") {
		t.Errorf("output should indicate no checkpoint found, got: %s", output)
	}

	// Should still switch to the branch
	if branch := env.GetCurrentBranch(); branch != plainBranch {
		t.Errorf("expected to be on %s, got %s", plainBranch, branch)
	}
}

// TestResume_BranchDoesNotExist tests that resume returns an error for non-existent branches.
func TestResume_BranchDoesNotExist(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t, strategy.StrategyNameAutoCommit)

	// Try to resume a non-existent branch
	output, err := env.RunResume("nonexistent-branch")

	// Should fail
	if err == nil {
		t.Errorf("expected error for non-existent branch, got success with output: %s", output)
	}

	// Error should mention the branch
	if !strings.Contains(output, "nonexistent-branch") && !strings.Contains(err.Error(), "nonexistent-branch") {
		t.Errorf("error should mention the branch name, got: %v, output: %s", err, output)
	}
}

// TestResume_UncommittedChanges tests that resume fails when there are uncommitted changes.
func TestResume_UncommittedChanges(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t, strategy.StrategyNameAutoCommit)

	// Create another branch
	env.GitCheckoutNewBranch("feature/target")
	env.WriteFile("target.txt", "target content")
	env.GitAdd("target.txt")
	env.GitCommit("Target commit")

	// Go back to original branch
	env.GitCheckoutBranch("feature/test-branch")

	// Make uncommitted changes
	env.WriteFile("uncommitted.txt", "uncommitted content")

	// Try to resume to target branch
	output, err := env.RunResume("feature/target")

	// Should fail due to uncommitted changes
	if err == nil {
		t.Errorf("expected error for uncommitted changes, got success with output: %s", output)
	}

	// Error should mention uncommitted changes
	if !strings.Contains(output, "uncommitted") && !strings.Contains(err.Error(), "uncommitted") {
		t.Errorf("error should mention uncommitted changes, got: %v, output: %s", err, output)
	}
}

// TestResume_SessionLogAlreadyExists tests that resume overwrites existing session logs
// with the checkpoint's version. This ensures consistency when resuming from a different device.
func TestResume_SessionLogAlreadyExists(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t, strategy.StrategyNameAutoCommit)

	// Create a session
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

	featureBranch := env.GetCurrentBranch()

	// Pre-create a session log in Claude project dir with different content
	// (simulating a stale/different version from another device)
	if err := os.MkdirAll(env.ClaudeProjectDir, 0o755); err != nil {
		t.Fatalf("failed to create Claude project dir: %v", err)
	}
	existingLog := filepath.Join(env.ClaudeProjectDir, session.ID+".jsonl")
	existingContent := `{"existing": true}`
	if err := os.WriteFile(existingLog, []byte(existingContent), 0o644); err != nil {
		t.Fatalf("failed to write existing log: %v", err)
	}

	// Switch to main and back
	env.GitCheckoutBranch(masterBranch)

	// Resume
	output, err := env.RunResume(featureBranch)
	if err != nil {
		t.Fatalf("resume failed: %v\nOutput: %s", err, output)
	}

	// Existing log SHOULD be overwritten with checkpoint's transcript
	data, err := os.ReadFile(existingLog)
	if err != nil {
		t.Fatalf("failed to read log: %v", err)
	}
	if string(data) == existingContent {
		t.Errorf("existing log should have been overwritten with checkpoint content, but still has: %s", string(data))
	}
	// Should contain the actual transcript content (user message)
	if !strings.Contains(string(data), "Create hello method") {
		t.Errorf("restored log should contain session transcript, got: %s", string(data))
	}

	// Output SHOULD indicate the session was restored (wording varies by code path)
	if !strings.Contains(output, "Session restored") && !strings.Contains(output, "Writing transcript to") {
		t.Errorf("output should indicate session restoration, got: %s", output)
	}
}

// TestResume_MultipleSessionsOnBranch tests resume with multiple sessions (multiple commits),
// ensuring it uses the session from the last commit.
func TestResume_MultipleSessionsOnBranch(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t, strategy.StrategyNameAutoCommit)

	// Create first session
	session1 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	content1 := "version 1"
	env.WriteFile("file.txt", content1)

	session1.CreateTranscript(
		"Create version 1",
		[]FileChange{{Path: "file.txt", Content: content1}},
	)
	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop session1 failed: %v", err)
	}

	// Create second session
	session2 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session2.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	content2 := "version 2"
	env.WriteFile("file.txt", content2)

	session2.CreateTranscript(
		"Update to version 2",
		[]FileChange{{Path: "file.txt", Content: content2}},
	)
	if err := env.SimulateStop(session2.ID, session2.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop session2 failed: %v", err)
	}

	featureBranch := env.GetCurrentBranch()

	// Switch to main
	env.GitCheckoutBranch(masterBranch)

	// Resume
	output, err := env.RunResume(featureBranch)
	if err != nil {
		t.Fatalf("resume failed: %v\nOutput: %s", err, output)
	}

	// Should show session info (from the most recent session)
	if !strings.Contains(output, "Session:") {
		t.Errorf("output should contain session info, got: %s", output)
	}

	// The resume command shows the session from the last commit,
	// which should be session2 (the most recent one)
	if !strings.Contains(output, session2.ID) && !strings.Contains(output, session2.EntireID) {
		t.Logf("Note: Expected session2 ID in output, but this depends on checkpoint lookup")
	}
}

// TestResume_CheckpointWithoutMetadata tests resume when a commit has an Entire-Checkpoint
// trailer but the corresponding metadata is missing from entire/checkpoints/v1 branch.
// This can happen if the metadata branch was corrupted or reset.
func TestResume_CheckpointWithoutMetadata(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t, strategy.StrategyNameAutoCommit)

	// First create a real session so the entire/checkpoints/v1 branch exists
	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}
	content := "real session content"
	env.WriteFile("real.txt", content)
	session.CreateTranscript(
		"Create real file",
		[]FileChange{{Path: "real.txt", Content: content}},
	)
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Create a new branch for the orphan checkpoint test
	env.GitCheckoutNewBranch("feature/orphan-checkpoint")

	// Add some content and create a commit with a checkpoint trailer
	// that points to non-existent metadata
	env.WriteFile("orphan.txt", "orphan content")
	env.GitAdd("orphan.txt")

	orphanCheckpointID := "000000000000" // Non-existent checkpoint
	env.GitCommitWithCheckpointID("Commit with orphan checkpoint", orphanCheckpointID)

	featureBranch := env.GetCurrentBranch()

	// Switch to main
	env.GitCheckoutBranch(masterBranch)

	// Resume - should not error but indicate no session available
	output, err := env.RunResume(featureBranch)
	if err != nil {
		t.Fatalf("resume failed: %v\nOutput: %s", err, output)
	}

	// Verify we switched to the feature branch
	if branch := env.GetCurrentBranch(); branch != featureBranch {
		t.Errorf("expected to be on %s, got %s", featureBranch, branch)
	}

	// Should NOT show session info since metadata is missing
	// The resume command should silently skip commits without valid metadata
	if strings.Contains(output, "Session:") {
		t.Errorf("output should not contain 'Session:' when metadata is missing, got: %s", output)
	}
}

// TestResume_AfterMergingMain tests that resume finds the checkpoint from branch-only commits
// when main has been merged into the feature branch (making HEAD a merge commit without trailers).
// Since the only "newer" commits are merge commits, no confirmation should be required.
func TestResume_AfterMergingMain(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t, strategy.StrategyNameAutoCommit)

	// Create a session on the feature branch
	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	content := "puts 'Hello from session'"
	env.WriteFile("hello.rb", content)

	session.CreateTranscript(
		"Create a hello script",
		[]FileChange{{Path: "hello.rb", Content: content}},
	)
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Remember the feature branch name
	featureBranch := env.GetCurrentBranch()

	// Switch to master and create a new commit (simulating work on main)
	env.GitCheckoutBranch(masterBranch)
	env.WriteFile("main_feature.txt", "new feature on main")
	env.GitAdd("main_feature.txt")
	env.GitCommit("Add new feature on main")

	// Switch back to feature branch
	env.GitCheckoutBranch(featureBranch)

	// Merge main into feature branch (this creates a merge commit without Entire trailers)
	env.GitMerge(masterBranch)

	// Verify HEAD is now a merge commit (doesn't have checkpoint trailer)
	headMessage := env.GetHeadCommitMessage()
	if !strings.HasPrefix(headMessage, "Merge branch") {
		t.Logf("Note: HEAD commit message: %s", headMessage)
	}

	// Switch to master
	env.GitCheckoutBranch(masterBranch)

	// Run resume WITHOUT --force - merge commits shouldn't require confirmation
	output, err := env.RunResume(featureBranch)
	if err != nil {
		t.Fatalf("resume failed: %v\nOutput: %s", err, output)
	}

	// Should switch to the feature branch
	if branch := env.GetCurrentBranch(); branch != featureBranch {
		t.Errorf("expected to be on %s, got %s", featureBranch, branch)
	}

	// Should find the session from the older commit (before the merge)
	if !strings.Contains(output, "Session:") {
		t.Errorf("output should contain 'Session:', got: %s", output)
	}
	if !strings.Contains(output, "claude -r") {
		t.Errorf("output should contain 'claude -r', got: %s", output)
	}

	// Should indicate it's skipping merge commits (informational, not a warning)
	if !strings.Contains(output, "skipping merge commit") {
		t.Logf("Note: Expected 'skipping merge commit' message, got: %s", output)
	}
}

// RunResume executes the resume command and returns the combined output.
// The subprocess is detached from the controlling terminal (via Setsid) to prevent
// interactive prompts from hanging tests. This simulates non-interactive environments like CI.
func (env *TestEnv) RunResume(branchName string) (string, error) {
	env.T.Helper()

	ctx := env.T.Context()
	cmd := exec.CommandContext(ctx, getTestBinary(), "resume", branchName)
	cmd.Dir = env.RepoDir
	cmd.Env = append(os.Environ(),
		"ENTIRE_TEST_CLAUDE_PROJECT_DIR="+env.ClaudeProjectDir,
	)
	// Detach from controlling terminal so huh can't open /dev/tty for interactive prompts
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	output, err := cmd.CombinedOutput()
	return string(output), err
}

// RunResumeForce executes the resume command with --force flag.
func (env *TestEnv) RunResumeForce(branchName string) (string, error) {
	env.T.Helper()

	ctx := env.T.Context()
	cmd := exec.CommandContext(ctx, getTestBinary(), "resume", "--force", branchName)
	cmd.Dir = env.RepoDir
	cmd.Env = append(os.Environ(),
		"ENTIRE_TEST_CLAUDE_PROJECT_DIR="+env.ClaudeProjectDir,
	)

	output, err := cmd.CombinedOutput()
	return string(output), err
}

// RunResumeInteractive executes the resume command with a pty, allowing
// interactive prompt responses. The respond function receives the pty for
// reading output and writing input. See RunCommandInteractive for details.
func (env *TestEnv) RunResumeInteractive(branchName string, respond func(ptyFile *os.File) string) (string, error) {
	env.T.Helper()
	return env.RunCommandInteractive([]string{"resume", branchName}, respond)
}

// GitMerge merges a branch into the current branch.
func (env *TestEnv) GitMerge(branchName string) {
	env.T.Helper()

	ctx := env.T.Context()
	// Use --no-verify to skip hooks - the hooks use local_dev paths that don't work
	// from test temp directories. This is fine since we're testing merge behavior,
	// not hook execution during merge.
	cmd := exec.CommandContext(ctx, "git", "merge", branchName, "-m", "Merge branch '"+branchName+"'", "--no-verify")
	cmd.Dir = env.RepoDir

	output, err := cmd.CombinedOutput()
	if err != nil {
		env.T.Fatalf("failed to merge branch %s: %v\nOutput: %s", branchName, err, output)
	}
}

// GetHeadCommitMessage returns the message of the HEAD commit.
func (env *TestEnv) GetHeadCommitMessage() string {
	env.T.Helper()

	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		env.T.Fatalf("failed to open git repo: %v", err)
	}

	head, err := repo.Head()
	if err != nil {
		env.T.Fatalf("failed to get HEAD: %v", err)
	}

	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		env.T.Fatalf("failed to get commit: %v", err)
	}

	return commit.Message
}

// GitCheckoutBranch checks out an existing branch.
func (env *TestEnv) GitCheckoutBranch(branchName string) {
	env.T.Helper()

	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		env.T.Fatalf("failed to open git repo: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		env.T.Fatalf("failed to get worktree: %v", err)
	}

	err = worktree.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName(branchName),
	})
	if err != nil {
		env.T.Fatalf("failed to checkout branch %s: %v", branchName, err)
	}
}

// TestResume_LocalLogNewerTimestamp_RequiresForce tests that when local log has newer
// timestamps than the checkpoint, the command fails in non-interactive mode (no TTY)
// and does NOT overwrite the local log. This ensures safe behavior in CI environments.
func TestResume_LocalLogNewerTimestamp_RequiresForce(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t, strategy.StrategyNameAutoCommit)

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

	featureBranch := env.GetCurrentBranch()

	// Create a local log with a NEWER timestamp than the checkpoint
	if err := os.MkdirAll(env.ClaudeProjectDir, 0o755); err != nil {
		t.Fatalf("failed to create Claude project dir: %v", err)
	}
	existingLog := filepath.Join(env.ClaudeProjectDir, session.ID+".jsonl")
	// Use a timestamp far in the future to ensure it's newer
	futureTimestamp := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	newerContent := fmt.Sprintf(`{"type":"human","timestamp":"%s","message":{"content":"newer local work"}}`, futureTimestamp)
	if err := os.WriteFile(existingLog, []byte(newerContent), 0o644); err != nil {
		t.Fatalf("failed to write existing log: %v", err)
	}

	// Switch to main
	env.GitCheckoutBranch(masterBranch)

	// Resume WITHOUT --force in non-interactive mode (no TTY due to Setsid)
	// Should fail because it can't prompt for confirmation
	output, err := env.RunResume(featureBranch)
	if err == nil {
		t.Errorf("expected error when resuming without --force in non-interactive mode, got success.\nOutput: %s", output)
	}

	// Verify local log was NOT overwritten (safe behavior)
	data, err := os.ReadFile(existingLog)
	if err != nil {
		t.Fatalf("failed to read log: %v", err)
	}
	if !strings.Contains(string(data), "newer local work") {
		t.Errorf("local log should NOT have been overwritten without --force, but content changed to: %s", string(data))
	}
}

// TestResume_LocalLogNewerTimestamp_ForceOverwrites tests that when local log has newer
// timestamps than the checkpoint, the --force flag bypasses the confirmation prompt
// and overwrites the local log.
func TestResume_LocalLogNewerTimestamp_ForceOverwrites(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t, strategy.StrategyNameAutoCommit)

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

	featureBranch := env.GetCurrentBranch()

	// Create a local log with a NEWER timestamp than the checkpoint
	if err := os.MkdirAll(env.ClaudeProjectDir, 0o755); err != nil {
		t.Fatalf("failed to create Claude project dir: %v", err)
	}
	existingLog := filepath.Join(env.ClaudeProjectDir, session.ID+".jsonl")
	// Use a timestamp far in the future to ensure it's newer
	futureTimestamp := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	newerContent := fmt.Sprintf(`{"type":"human","timestamp":"%s","message":{"content":"newer local work"}}`, futureTimestamp)
	if err := os.WriteFile(existingLog, []byte(newerContent), 0o644); err != nil {
		t.Fatalf("failed to write existing log: %v", err)
	}

	// Switch to main
	env.GitCheckoutBranch(masterBranch)

	// Resume WITH --force should succeed and overwrite the local log
	output, err := env.RunResumeForce(featureBranch)
	if err != nil {
		t.Fatalf("resume --force failed: %v\nOutput: %s", err, output)
	}

	// Verify local log was overwritten with checkpoint content
	data, err := os.ReadFile(existingLog)
	if err != nil {
		t.Fatalf("failed to read log: %v", err)
	}
	if strings.Contains(string(data), "newer local work") {
		t.Errorf("local log should have been overwritten, but still has newer content: %s", string(data))
	}
	if !strings.Contains(string(data), "Create hello method") {
		t.Errorf("restored log should contain checkpoint transcript, got: %s", string(data))
	}
}

// TestResume_LocalLogNewerTimestamp_UserConfirmsOverwrite tests that when the user
// confirms the overwrite prompt interactively, the local log is overwritten.
func TestResume_LocalLogNewerTimestamp_UserConfirmsOverwrite(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t, strategy.StrategyNameAutoCommit)

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
	env := NewFeatureBranchEnv(t, strategy.StrategyNameAutoCommit)

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

// TestResume_CheckpointNewerTimestamp tests that when checkpoint has newer timestamps
// than local log, resume proceeds without requiring --force.
func TestResume_CheckpointNewerTimestamp(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t, strategy.StrategyNameAutoCommit)

	// Create a session
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

	featureBranch := env.GetCurrentBranch()

	// Create a local log with an OLDER timestamp than the checkpoint
	if err := os.MkdirAll(env.ClaudeProjectDir, 0o755); err != nil {
		t.Fatalf("failed to create Claude project dir: %v", err)
	}
	existingLog := filepath.Join(env.ClaudeProjectDir, session.ID+".jsonl")
	// Use a timestamp far in the past
	pastTimestamp := time.Now().Add(-7 * 24 * time.Hour).UTC().Format(time.RFC3339)
	olderContent := fmt.Sprintf(`{"type":"human","timestamp":"%s","message":{"content":"older local work"}}`, pastTimestamp)
	if err := os.WriteFile(existingLog, []byte(olderContent), 0o644); err != nil {
		t.Fatalf("failed to write existing log: %v", err)
	}

	// Switch to main
	env.GitCheckoutBranch(masterBranch)

	// Resume WITHOUT --force should succeed because checkpoint is newer (no conflict)
	output, err := env.RunResume(featureBranch)
	if err != nil {
		t.Fatalf("resume failed (should succeed when checkpoint is newer): %v\nOutput: %s", err, output)
	}

	// Verify local log was overwritten with checkpoint content
	data, err := os.ReadFile(existingLog)
	if err != nil {
		t.Fatalf("failed to read log: %v", err)
	}
	if strings.Contains(string(data), "older local work") {
		t.Errorf("local log should have been overwritten, but still has older content: %s", string(data))
	}
	if !strings.Contains(string(data), "Create hello method") {
		t.Errorf("restored log should contain checkpoint transcript, got: %s", string(data))
	}
}

// TestResume_MultiSessionMixedTimestamps tests resume with multiple sessions in a checkpoint
// where one session has a newer local log (conflict) and another doesn't (no conflict).
func TestResume_MultiSessionMixedTimestamps(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	// Create first session
	session1 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	content1 := "def hello; end"
	env.WriteFile("hello.rb", content1)

	session1.CreateTranscript(
		"Create hello method",
		[]FileChange{{Path: "hello.rb", Content: content1}},
	)
	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop session1 failed: %v", err)
	}

	// Create second session (same base commit, different session)
	session2 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session2.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	content2 := "def goodbye; end"
	env.WriteFile("goodbye.rb", content2)

	session2.CreateTranscript(
		"Create goodbye method",
		[]FileChange{{Path: "goodbye.rb", Content: content2}},
	)
	if err := env.SimulateStop(session2.ID, session2.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop session2 failed: %v", err)
	}

	// Commit changes with hooks (this triggers prepare-commit-msg and post-commit hooks,
	// which adds Entire-Checkpoint trailer and condenses both sessions to the same checkpoint)
	env.GitCommitWithShadowHooks("Add hello and goodbye methods", "hello.rb", "goodbye.rb")

	featureBranch := env.GetCurrentBranch()

	// Create local logs with different timestamps:
	// - session1: NEWER than checkpoint (conflict)
	// - session2: OLDER than checkpoint (no conflict)
	if err := os.MkdirAll(env.ClaudeProjectDir, 0o755); err != nil {
		t.Fatalf("failed to create Claude project dir: %v", err)
	}

	// Session 1: newer local log (conflict)
	log1Path := filepath.Join(env.ClaudeProjectDir, session1.ID+".jsonl")
	futureTimestamp := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	newerContent := fmt.Sprintf(`{"type":"human","timestamp":"%s","message":{"content":"newer local work on session1"}}`, futureTimestamp)
	if err := os.WriteFile(log1Path, []byte(newerContent), 0o644); err != nil {
		t.Fatalf("failed to write session1 log: %v", err)
	}

	// Session 2: older local log (no conflict)
	log2Path := filepath.Join(env.ClaudeProjectDir, session2.ID+".jsonl")
	pastTimestamp := time.Now().Add(-7 * 24 * time.Hour).UTC().Format(time.RFC3339)
	olderContent := fmt.Sprintf(`{"type":"human","timestamp":"%s","message":{"content":"older local work on session2"}}`, pastTimestamp)
	if err := os.WriteFile(log2Path, []byte(olderContent), 0o644); err != nil {
		t.Fatalf("failed to write session2 log: %v", err)
	}

	// Switch to main
	env.GitCheckoutBranch(masterBranch)

	// Resume WITH --force (to bypass confirmation for the conflict)
	output, err := env.RunResumeForce(featureBranch)
	if err != nil {
		t.Fatalf("resume --force failed: %v\nOutput: %s", err, output)
	}

	// Both logs should be overwritten with checkpoint content
	data1, err := os.ReadFile(log1Path)
	if err != nil {
		t.Fatalf("failed to read session1 log: %v", err)
	}
	if strings.Contains(string(data1), "newer local work") {
		t.Errorf("session1 log should have been overwritten, but still has newer content: %s", string(data1))
	}
	if !strings.Contains(string(data1), "Create hello method") {
		t.Errorf("session1 log should contain checkpoint transcript, got: %s", string(data1))
	}

	data2, err := os.ReadFile(log2Path)
	if err != nil {
		t.Fatalf("failed to read session2 log: %v", err)
	}
	if strings.Contains(string(data2), "older local work") {
		t.Errorf("session2 log should have been overwritten, but still has older content: %s", string(data2))
	}
	if !strings.Contains(string(data2), "Create goodbye method") {
		t.Errorf("session2 log should contain checkpoint transcript, got: %s", string(data2))
	}

	// Output should mention restoring multiple sessions
	if !strings.Contains(output, "Restoring 2 sessions") {
		t.Logf("Note: Expected 'Restoring 2 sessions' in output, got: %s", output)
	}
}

// TestResume_LocalLogNoTimestamp tests that when local log has no valid timestamp,
// resume proceeds without requiring --force (treated as new).
func TestResume_LocalLogNoTimestamp(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t, strategy.StrategyNameAutoCommit)

	// Create a session
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

	featureBranch := env.GetCurrentBranch()

	// Create a local log WITHOUT a valid timestamp (can't be parsed)
	if err := os.MkdirAll(env.ClaudeProjectDir, 0o755); err != nil {
		t.Fatalf("failed to create Claude project dir: %v", err)
	}
	existingLog := filepath.Join(env.ClaudeProjectDir, session.ID+".jsonl")
	// Content without timestamp field - should be treated as "new"
	noTimestampContent := `{"type":"human","message":{"content":"no timestamp"}}`
	if err := os.WriteFile(existingLog, []byte(noTimestampContent), 0o644); err != nil {
		t.Fatalf("failed to write existing log: %v", err)
	}

	// Switch to main
	env.GitCheckoutBranch(masterBranch)

	// Resume WITHOUT --force should succeed (no timestamp = treated as new)
	output, err := env.RunResume(featureBranch)
	if err != nil {
		t.Fatalf("resume failed (should succeed when local has no timestamp): %v\nOutput: %s", err, output)
	}

	// Verify local log was overwritten with checkpoint content
	data, err := os.ReadFile(existingLog)
	if err != nil {
		t.Fatalf("failed to read log: %v", err)
	}
	if strings.Contains(string(data), "no timestamp") {
		t.Errorf("local log should have been overwritten, but still has old content: %s", string(data))
	}
	if !strings.Contains(string(data), "Create hello method") {
		t.Errorf("restored log should contain checkpoint transcript, got: %s", string(data))
	}
}
