package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"entire.io/cli/cmd/entire/cli/checkpoint"
	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/strategy"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestNewExplainCmd(t *testing.T) {
	cmd := newExplainCmd()

	if cmd.Use != "explain" {
		t.Errorf("expected Use to be 'explain', got %s", cmd.Use)
	}

	// Verify flags exist
	sessionFlag := cmd.Flags().Lookup("session")
	if sessionFlag == nil {
		t.Error("expected --session flag to exist")
	}

	commitFlag := cmd.Flags().Lookup("commit")
	if commitFlag == nil {
		t.Error("expected --commit flag to exist")
	}
}

func TestExplainSession_NotFound(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo
	if _, err := git.PlainInit(tmpDir, false); err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Create .entire directory
	if err := os.MkdirAll(".entire", 0o750); err != nil {
		t.Fatalf("failed to create .entire dir: %v", err)
	}

	var stdout bytes.Buffer
	err := runExplainSession(&stdout, "nonexistent-session", false)

	if err == nil {
		t.Error("expected error for nonexistent session, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got: %v", err)
	}
}

func TestExplainCommit_NotFound(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo
	if _, err := git.PlainInit(tmpDir, false); err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	var stdout bytes.Buffer
	err := runExplainCommit(&stdout, "nonexistent")

	if err == nil {
		t.Error("expected error for nonexistent commit, got nil")
	}
	if !strings.Contains(err.Error(), "not found") && !strings.Contains(err.Error(), "resolve") {
		t.Errorf("expected 'not found' or 'resolve' in error, got: %v", err)
	}
}

func TestExplainCommit_NoEntireData(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo
	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create a commit without Entire metadata
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test content"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	commitHash, err := w.Commit("regular commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
		},
	})
	if err != nil {
		t.Fatalf("failed to create commit: %v", err)
	}

	var stdout bytes.Buffer
	err = runExplainCommit(&stdout, commitHash.String())
	if err != nil {
		t.Fatalf("runExplainCommit() should not error for non-Entire commits, got: %v", err)
	}

	output := stdout.String()

	// Should show git info
	if !strings.Contains(output, commitHash.String()[:7]) {
		t.Errorf("expected output to contain short commit hash, got: %s", output)
	}
	if !strings.Contains(output, "regular commit") {
		t.Errorf("expected output to contain commit message, got: %s", output)
	}
	// Should show no Entire data message
	if !strings.Contains(output, "No Entire session data") {
		t.Errorf("expected output to indicate no Entire data, got: %s", output)
	}
}

func TestExplainCommit_WithEntireData(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo
	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create session metadata directory first
	sessionID := "2025-12-09-test-session-xyz789"
	sessionDir := filepath.Join(tmpDir, ".entire", "metadata", sessionID)
	if err := os.MkdirAll(sessionDir, 0o750); err != nil {
		t.Fatalf("failed to create session dir: %v", err)
	}

	// Create prompt file
	promptContent := "Add new feature"
	if err := os.WriteFile(filepath.Join(sessionDir, paths.PromptFileName), []byte(promptContent), 0o644); err != nil {
		t.Fatalf("failed to create prompt file: %v", err)
	}

	// Create a commit with Entire metadata trailer
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("feature content"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}

	// Commit with Entire-Metadata trailer
	metadataDir := ".entire/metadata/" + sessionID
	commitMessage := paths.FormatMetadataTrailer("Add new feature", metadataDir)
	commitHash, err := w.Commit(commitMessage, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
		},
	})
	if err != nil {
		t.Fatalf("failed to create commit: %v", err)
	}

	var stdout bytes.Buffer
	err = runExplainCommit(&stdout, commitHash.String())
	if err != nil {
		t.Fatalf("runExplainCommit() error = %v", err)
	}

	output := stdout.String()

	// Should show commit info
	if !strings.Contains(output, commitHash.String()[:7]) {
		t.Errorf("expected output to contain short commit hash, got: %s", output)
	}
	// Should show session info - the session ID is extracted from the metadata path
	// The format is test-session-xyz789 (extracted from the full path)
	if !strings.Contains(output, "Session:") {
		t.Errorf("expected output to contain 'Session:', got: %s", output)
	}
}

func TestExplainDefault_NoCurrentSession(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo
	if _, err := git.PlainInit(tmpDir, false); err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Create .entire directory but no current_session file
	if err := os.MkdirAll(".entire", 0o750); err != nil {
		t.Fatalf("failed to create .entire dir: %v", err)
	}

	var stdout bytes.Buffer
	err := runExplainDefault(&stdout, false)

	if err == nil {
		t.Error("expected error when no current session, got nil")
	}
	// Case-insensitive check for "no active session"
	errLower := strings.ToLower(err.Error())
	if !strings.Contains(errLower, "no active session") {
		t.Errorf("expected 'no active session' in error, got: %v", err)
	}
}

func TestExplainBothFlagsError(t *testing.T) {
	// Test that providing both --session and --commit returns an error
	var stdout bytes.Buffer
	err := runExplain(&stdout, "session-id", "commit-sha", "", false, false, false)

	if err == nil {
		t.Error("expected error when both flags provided, got nil")
	}
	// Case-insensitive check for "cannot specify multiple"
	errLower := strings.ToLower(err.Error())
	if !strings.Contains(errLower, "cannot specify multiple") {
		t.Errorf("expected 'cannot specify multiple' in error, got: %v", err)
	}
}

func TestFormatSessionInfo(t *testing.T) {
	now := time.Now()
	session := &strategy.Session{
		ID:          "2025-12-09-test-session-abc",
		Description: "Test description",
		Strategy:    "manual-commit",
		StartTime:   now,
		Checkpoints: []strategy.Checkpoint{
			{
				CheckpointID: "abc1234567890",
				Message:      "First checkpoint",
				Timestamp:    now.Add(-time.Hour),
			},
			{
				CheckpointID: "def0987654321",
				Message:      "Second checkpoint",
				Timestamp:    now,
			},
		},
	}

	// Create checkpoint details matching the session checkpoints
	checkpointDetails := []checkpointDetail{
		{
			Index:     1,
			ShortID:   "abc1234",
			Timestamp: now.Add(-time.Hour),
			Message:   "First checkpoint",
			Interactions: []interaction{{
				Prompt:    "Fix the bug",
				Responses: []string{"Fixed the bug in auth module"},
				Files:     []string{"auth.go"},
			}},
			Files: []string{"auth.go"},
		},
		{
			Index:     2,
			ShortID:   "def0987",
			Timestamp: now,
			Message:   "Second checkpoint",
			Interactions: []interaction{{
				Prompt:    "Add tests",
				Responses: []string{"Added unit tests"},
				Files:     []string{"auth_test.go"},
			}},
			Files: []string{"auth_test.go"},
		},
	}

	output := formatSessionInfo(session, "", checkpointDetails)

	// Verify output contains expected sections
	if !strings.Contains(output, "Session:") {
		t.Error("expected output to contain 'Session:'")
	}
	if !strings.Contains(output, session.ID) {
		t.Error("expected output to contain session ID")
	}
	if !strings.Contains(output, "Strategy:") {
		t.Error("expected output to contain 'Strategy:'")
	}
	if !strings.Contains(output, "manual-commit") {
		t.Error("expected output to contain strategy name")
	}
	if !strings.Contains(output, "Checkpoints: 2") {
		t.Error("expected output to contain 'Checkpoints: 2'")
	}
	// Check checkpoint details
	if !strings.Contains(output, "Checkpoint 1") {
		t.Error("expected output to contain 'Checkpoint 1'")
	}
	if !strings.Contains(output, "## Prompt") {
		t.Error("expected output to contain '## Prompt'")
	}
	if !strings.Contains(output, "## Responses") {
		t.Error("expected output to contain '## Responses'")
	}
	if !strings.Contains(output, "Files Modified") {
		t.Error("expected output to contain 'Files Modified'")
	}
}

func TestFormatSessionInfo_WithSourceRef(t *testing.T) {
	now := time.Now()
	session := &strategy.Session{
		ID:          "2025-12-09-test-session-abc",
		Description: "Test description",
		Strategy:    "auto-commit",
		StartTime:   now,
		Checkpoints: []strategy.Checkpoint{
			{
				CheckpointID: "abc1234567890",
				Message:      "First checkpoint",
				Timestamp:    now,
			},
		},
	}

	checkpointDetails := []checkpointDetail{
		{
			Index:     1,
			ShortID:   "abc1234",
			Timestamp: now,
			Message:   "First checkpoint",
		},
	}

	// Test with source ref provided
	sourceRef := "entire/metadata@abc123def456"
	output := formatSessionInfo(session, sourceRef, checkpointDetails)

	// Verify source ref is displayed
	if !strings.Contains(output, "Source Ref:") {
		t.Error("expected output to contain 'Source Ref:'")
	}
	if !strings.Contains(output, sourceRef) {
		t.Errorf("expected output to contain source ref %q, got:\n%s", sourceRef, output)
	}
}

func TestFormatCommitInfo(t *testing.T) {
	now := time.Now()
	info := &commitInfo{
		SHA:       "abc1234567890abcdef1234567890abcdef123456",
		ShortSHA:  "abc1234",
		Message:   "Test commit message",
		Author:    "Test Author",
		Email:     "test@example.com",
		Date:      now,
		Files:     []string{"file1.go", "file2.go"},
		HasEntire: false,
		SessionID: "",
	}

	output := formatCommitInfo(info)

	// Verify output contains expected sections
	if !strings.Contains(output, "Commit:") {
		t.Error("expected output to contain 'Commit:'")
	}
	if !strings.Contains(output, info.ShortSHA) {
		t.Error("expected output to contain short SHA")
	}
	if !strings.Contains(output, info.SHA) {
		t.Error("expected output to contain full SHA")
	}
	if !strings.Contains(output, "Message:") {
		t.Error("expected output to contain 'Message:'")
	}
	if !strings.Contains(output, info.Message) {
		t.Error("expected output to contain commit message")
	}
	if !strings.Contains(output, "Files Modified") {
		t.Error("expected output to contain 'Files Modified'")
	}
	if !strings.Contains(output, "No Entire session data") {
		t.Error("expected output to contain no Entire data message")
	}
}

func TestFormatCommitInfo_WithEntireData(t *testing.T) {
	now := time.Now()
	info := &commitInfo{
		SHA:       "abc1234567890abcdef1234567890abcdef123456",
		ShortSHA:  "abc1234",
		Message:   "Test commit message",
		Author:    "Test Author",
		Email:     "test@example.com",
		Date:      now,
		Files:     []string{"file1.go"},
		HasEntire: true,
		SessionID: "2025-12-09-test-session",
	}

	output := formatCommitInfo(info)

	// Verify output contains expected sections
	if !strings.Contains(output, "Session:") {
		t.Error("expected output to contain 'Session:'")
	}
	if !strings.Contains(output, info.SessionID) {
		t.Error("expected output to contain session ID")
	}
	if strings.Contains(output, "No Entire session data") {
		t.Error("expected output to NOT contain no Entire data message")
	}
}

// Helper to verify common session functions work with SessionSource interface
func TestStrategySessionSourceInterface(t *testing.T) {
	// This ensures manual-commit strategy implements SessionSource
	var s = strategy.NewManualCommitStrategy()

	// Cast to SessionSource - manual-commit strategy should implement it
	source, ok := s.(strategy.SessionSource)
	if !ok {
		t.Fatal("ManualCommitStrategy should implement SessionSource interface")
	}

	// GetAdditionalSessions should exist and be callable
	_, err := source.GetAdditionalSessions()
	if err != nil {
		t.Logf("GetAdditionalSessions returned error: %v", err)
	}
}

func TestFormatSessionInfo_CheckpointNumberingReversed(t *testing.T) {
	now := time.Now()
	session := &strategy.Session{
		ID:          "2025-12-09-test-session",
		Strategy:    "auto-commit",
		StartTime:   now.Add(-2 * time.Hour),
		Checkpoints: []strategy.Checkpoint{}, // Not used for format test
	}

	// Simulate checkpoints coming in newest-first order from ListSessions
	// but numbered with oldest=1, newest=N
	checkpointDetails := []checkpointDetail{
		{
			Index:     3, // Newest checkpoint should have highest number
			ShortID:   "ccc3333",
			Timestamp: now,
			Message:   "Third (newest) checkpoint",
			Interactions: []interaction{{
				Prompt:    "Latest change",
				Responses: []string{},
			}},
		},
		{
			Index:     2,
			ShortID:   "bbb2222",
			Timestamp: now.Add(-time.Hour),
			Message:   "Second checkpoint",
			Interactions: []interaction{{
				Prompt:    "Middle change",
				Responses: []string{},
			}},
		},
		{
			Index:     1, // Oldest checkpoint should be #1
			ShortID:   "aaa1111",
			Timestamp: now.Add(-2 * time.Hour),
			Message:   "First (oldest) checkpoint",
			Interactions: []interaction{{
				Prompt:    "Initial change",
				Responses: []string{},
			}},
		},
	}

	output := formatSessionInfo(session, "", checkpointDetails)

	// Verify checkpoint ordering in output
	// Checkpoint 3 should appear before Checkpoint 2 which should appear before Checkpoint 1
	idx3 := strings.Index(output, "Checkpoint 3")
	idx2 := strings.Index(output, "Checkpoint 2")
	idx1 := strings.Index(output, "Checkpoint 1")

	if idx3 == -1 || idx2 == -1 || idx1 == -1 {
		t.Fatalf("expected all checkpoints to be in output, got:\n%s", output)
	}

	// In the output, they should appear in the order they're in the slice (newest first)
	if idx3 > idx2 || idx2 > idx1 {
		t.Errorf("expected checkpoints to appear in order 3, 2, 1 in output (newest first), got positions: 3=%d, 2=%d, 1=%d", idx3, idx2, idx1)
	}

	// Verify the dates appear correctly
	if !strings.Contains(output, "Latest change") {
		t.Error("expected output to contain 'Latest change' prompt")
	}
	if !strings.Contains(output, "Initial change") {
		t.Error("expected output to contain 'Initial change' prompt")
	}
}

func TestFormatSessionInfo_EmptyCheckpoints(t *testing.T) {
	now := time.Now()
	session := &strategy.Session{
		ID:          "2025-12-09-empty-session",
		Strategy:    "manual-commit",
		StartTime:   now,
		Checkpoints: []strategy.Checkpoint{},
	}

	output := formatSessionInfo(session, "", nil)

	if !strings.Contains(output, "Checkpoints: 0") {
		t.Errorf("expected output to contain 'Checkpoints: 0', got:\n%s", output)
	}
}

func TestFormatSessionInfo_CheckpointWithTaskMarker(t *testing.T) {
	now := time.Now()
	session := &strategy.Session{
		ID:          "2025-12-09-task-session",
		Strategy:    "auto-commit",
		StartTime:   now,
		Checkpoints: []strategy.Checkpoint{},
	}

	checkpointDetails := []checkpointDetail{
		{
			Index:            1,
			ShortID:          "abc1234",
			Timestamp:        now,
			IsTaskCheckpoint: true,
			Message:          "Task checkpoint",
			Interactions: []interaction{{
				Prompt:    "Run tests",
				Responses: []string{},
			}},
		},
	}

	output := formatSessionInfo(session, "", checkpointDetails)

	if !strings.Contains(output, "[Task]") {
		t.Errorf("expected output to contain '[Task]' marker, got:\n%s", output)
	}
}

func TestFormatSessionInfo_CheckpointWithDate(t *testing.T) {
	// Test that checkpoint headers include the full date
	timestamp := time.Date(2025, 12, 10, 14, 35, 0, 0, time.UTC)
	session := &strategy.Session{
		ID:          "2025-12-10-dated-session",
		Strategy:    "auto-commit",
		StartTime:   timestamp,
		Checkpoints: []strategy.Checkpoint{},
	}

	checkpointDetails := []checkpointDetail{
		{
			Index:     1,
			ShortID:   "abc1234",
			Timestamp: timestamp,
			Message:   "Test checkpoint",
		},
	}

	output := formatSessionInfo(session, "", checkpointDetails)

	// Should contain "2025-12-10 14:35" in the checkpoint header
	if !strings.Contains(output, "2025-12-10 14:35") {
		t.Errorf("expected output to contain date '2025-12-10 14:35', got:\n%s", output)
	}
}

func TestFormatSessionInfo_ShowsMessageWhenNoInteractions(t *testing.T) {
	// Test that checkpoints without transcript content show the commit message
	now := time.Now()
	session := &strategy.Session{
		ID:          "2025-12-12-incremental-session",
		Strategy:    "auto-commit",
		StartTime:   now,
		Checkpoints: []strategy.Checkpoint{},
	}

	// Checkpoint with message but no interactions (like incremental checkpoints)
	checkpointDetails := []checkpointDetail{
		{
			Index:            1,
			ShortID:          "abc1234",
			Timestamp:        now,
			IsTaskCheckpoint: true,
			Message:          "Starting 'dev' agent: Implement feature X (toolu_01ABC)",
			Interactions:     []interaction{}, // Empty - no transcript available
		},
	}

	output := formatSessionInfo(session, "", checkpointDetails)

	// Should show the commit message when there are no interactions
	if !strings.Contains(output, "Starting 'dev' agent: Implement feature X (toolu_01ABC)") {
		t.Errorf("expected output to contain commit message when no interactions, got:\n%s", output)
	}

	// Should NOT show "## Prompt" or "## Responses" sections since there are no interactions
	if strings.Contains(output, "## Prompt") {
		t.Errorf("expected output to NOT contain '## Prompt' when no interactions, got:\n%s", output)
	}
	if strings.Contains(output, "## Responses") {
		t.Errorf("expected output to NOT contain '## Responses' when no interactions, got:\n%s", output)
	}
}

func TestFormatSessionInfo_ShowsMessageAndFilesWhenNoInteractions(t *testing.T) {
	// Test that checkpoints without transcript but with files show both message and files
	now := time.Now()
	session := &strategy.Session{
		ID:          "2025-12-12-incremental-with-files",
		Strategy:    "auto-commit",
		StartTime:   now,
		Checkpoints: []strategy.Checkpoint{},
	}

	checkpointDetails := []checkpointDetail{
		{
			Index:            1,
			ShortID:          "def5678",
			Timestamp:        now,
			IsTaskCheckpoint: true,
			Message:          "Running tests for API endpoint (toolu_02DEF)",
			Interactions:     []interaction{}, // Empty - no transcript
			Files:            []string{"api/endpoint.go", "api/endpoint_test.go"},
		},
	}

	output := formatSessionInfo(session, "", checkpointDetails)

	// Should show the commit message
	if !strings.Contains(output, "Running tests for API endpoint (toolu_02DEF)") {
		t.Errorf("expected output to contain commit message, got:\n%s", output)
	}

	// Should also show the files
	if !strings.Contains(output, "Files Modified") {
		t.Errorf("expected output to contain 'Files Modified', got:\n%s", output)
	}
	if !strings.Contains(output, "api/endpoint.go") {
		t.Errorf("expected output to contain modified file, got:\n%s", output)
	}
}

func TestFormatSessionInfo_DoesNotShowMessageWhenHasInteractions(t *testing.T) {
	// Test that checkpoints WITH interactions don't show the message separately
	// (the interactions already contain the content)
	now := time.Now()
	session := &strategy.Session{
		ID:          "2025-12-12-full-checkpoint",
		Strategy:    "auto-commit",
		StartTime:   now,
		Checkpoints: []strategy.Checkpoint{},
	}

	checkpointDetails := []checkpointDetail{
		{
			Index:            1,
			ShortID:          "ghi9012",
			Timestamp:        now,
			IsTaskCheckpoint: true,
			Message:          "Completed 'dev' agent: Implement feature (toolu_03GHI)",
			Interactions: []interaction{
				{
					Prompt:    "Implement the feature",
					Responses: []string{"I've implemented the feature by..."},
					Files:     []string{"feature.go"},
				},
			},
		},
	}

	output := formatSessionInfo(session, "", checkpointDetails)

	// Should show the interaction content
	if !strings.Contains(output, "Implement the feature") {
		t.Errorf("expected output to contain prompt, got:\n%s", output)
	}
	if !strings.Contains(output, "I've implemented the feature by") {
		t.Errorf("expected output to contain response, got:\n%s", output)
	}

	// The message should NOT appear as a separate line (it's redundant when we have interactions)
	// The output should contain ## Prompt and ## Responses for the interaction
	if !strings.Contains(output, "## Prompt") {
		t.Errorf("expected output to contain '## Prompt' when has interactions, got:\n%s", output)
	}
}

func TestExplainCmd_HasCheckpointFlag(t *testing.T) {
	cmd := newExplainCmd()

	flag := cmd.Flags().Lookup("checkpoint")
	if flag == nil {
		t.Error("expected --checkpoint flag to exist")
	}
}

func TestExplainCmd_HasVerboseFlag(t *testing.T) {
	cmd := newExplainCmd()

	flag := cmd.Flags().Lookup("verbose")
	if flag == nil {
		t.Fatal("expected --verbose flag to exist")
	}

	// Should have -v shorthand
	if flag.Shorthand != "v" {
		t.Errorf("expected -v shorthand, got %q", flag.Shorthand)
	}
}

func TestExplainCmd_HasFullFlag(t *testing.T) {
	cmd := newExplainCmd()

	flag := cmd.Flags().Lookup("full")
	if flag == nil {
		t.Error("expected --full flag to exist")
	}
}

func TestRunExplain_MutualExclusivityError(t *testing.T) {
	var buf bytes.Buffer

	// Providing both --session and --checkpoint should error
	err := runExplain(&buf, "session-id", "", "checkpoint-id", false, false, false)

	if err == nil {
		t.Error("expected error when multiple flags provided")
	}
	if !strings.Contains(err.Error(), "cannot specify multiple") {
		t.Errorf("expected 'cannot specify multiple' error, got: %v", err)
	}
}

func TestRunExplainCheckpoint_NotFound(t *testing.T) {
	// Skip if no git repo
	if _, err := openRepository(); err != nil {
		t.Skip("Skipping: not in a git repository")
	}

	var buf bytes.Buffer
	err := runExplainCheckpoint(&buf, "nonexistent123", false, false, false)

	if err == nil {
		t.Error("expected error for nonexistent checkpoint")
	}
	if !strings.Contains(err.Error(), "checkpoint not found") {
		t.Errorf("expected 'checkpoint not found' error, got: %v", err)
	}
}

func TestFormatCheckpointOutput_Default(t *testing.T) {
	result := &checkpoint.ReadCommittedResult{
		Metadata: checkpoint.CommittedMetadata{
			CheckpointID:     "abc123def456",
			SessionID:        "2026-01-21-test-session",
			CreatedAt:        time.Date(2026, 1, 21, 10, 30, 0, 0, time.UTC),
			FilesTouched:     []string{"main.go", "util.go"},
			CheckpointsCount: 3,
			TokenUsage: &checkpoint.TokenUsage{
				InputTokens:  10000,
				OutputTokens: 5000,
			},
		},
		Prompts: "Add a new feature",
	}

	output := formatCheckpointOutput(result, "abc123def456", false, false)

	// Should show checkpoint ID
	if !strings.Contains(output, "abc123def456") {
		t.Error("expected checkpoint ID in output")
	}
	// Should show session ID
	if !strings.Contains(output, "2026-01-21-test-session") {
		t.Error("expected session ID in output")
	}
	// Should show timestamp
	if !strings.Contains(output, "2026-01-21") {
		t.Error("expected timestamp in output")
	}
	// Should show token usage (10000 + 5000 = 15000)
	if !strings.Contains(output, "15000") {
		t.Error("expected token count in output")
	}
	// Should show Intent label
	if !strings.Contains(output, "Intent:") {
		t.Error("expected Intent label in output")
	}
	// Should NOT show full file list in default mode
	if strings.Contains(output, "main.go") {
		t.Error("default output should not show file list (use --verbose)")
	}
}

func TestFormatCheckpointOutput_Verbose(t *testing.T) {
	result := &checkpoint.ReadCommittedResult{
		Metadata: checkpoint.CommittedMetadata{
			CheckpointID:     "abc123def456",
			SessionID:        "2026-01-21-test-session",
			CreatedAt:        time.Date(2026, 1, 21, 10, 30, 0, 0, time.UTC),
			FilesTouched:     []string{"main.go", "util.go", "config.yaml"},
			CheckpointsCount: 3,
			TokenUsage: &checkpoint.TokenUsage{
				InputTokens:  10000,
				OutputTokens: 5000,
			},
		},
		Prompts: "Add a new feature\nFix the bug\nRefactor the code",
	}

	output := formatCheckpointOutput(result, "abc123def456", true, false)

	// Should show checkpoint ID (like default)
	if !strings.Contains(output, "abc123def456") {
		t.Error("expected checkpoint ID in output")
	}
	// Should show session ID (like default)
	if !strings.Contains(output, "2026-01-21-test-session") {
		t.Error("expected session ID in output")
	}
	// Verbose should show files
	if !strings.Contains(output, "main.go") {
		t.Error("verbose output should show files")
	}
	if !strings.Contains(output, "util.go") {
		t.Error("verbose output should show all files")
	}
	if !strings.Contains(output, "config.yaml") {
		t.Error("verbose output should show all files")
	}
	// Should show "Files:" section header
	if !strings.Contains(output, "Files:") {
		t.Error("verbose output should have Files section")
	}
	// Verbose should show all prompts (not just first line)
	if !strings.Contains(output, "Prompts:") {
		t.Error("verbose output should have Prompts section")
	}
	if !strings.Contains(output, "Add a new feature") {
		t.Error("verbose output should show prompts")
	}
}

func TestFormatCheckpointOutput_Full(t *testing.T) {
	result := &checkpoint.ReadCommittedResult{
		Metadata: checkpoint.CommittedMetadata{
			CheckpointID:     "abc123def456",
			SessionID:        "2026-01-21-test-session",
			CreatedAt:        time.Date(2026, 1, 21, 10, 30, 0, 0, time.UTC),
			FilesTouched:     []string{"main.go", "util.go"},
			CheckpointsCount: 3,
			TokenUsage: &checkpoint.TokenUsage{
				InputTokens:  10000,
				OutputTokens: 5000,
			},
		},
		Prompts:    "Add a new feature",
		Transcript: []byte(`{"type":"user","content":"Add a new feature"}` + "\n" + `{"type":"assistant","content":"I'll add that feature for you."}`),
	}

	output := formatCheckpointOutput(result, "abc123def456", false, true)

	// Should show checkpoint ID (like default)
	if !strings.Contains(output, "abc123def456") {
		t.Error("expected checkpoint ID in output")
	}
	// Full should also include verbose sections (files, prompts)
	if !strings.Contains(output, "Files:") {
		t.Error("full output should include files section")
	}
	if !strings.Contains(output, "Prompts:") {
		t.Error("full output should include prompts section")
	}
	// Full should show transcript section
	if !strings.Contains(output, "Transcript:") {
		t.Error("full output should have Transcript section")
	}
	// Should contain actual transcript content
	if !strings.Contains(output, "Add a new feature") {
		t.Error("full output should show transcript content")
	}
	if !strings.Contains(output, "assistant") {
		t.Error("full output should show assistant messages in transcript")
	}
}
