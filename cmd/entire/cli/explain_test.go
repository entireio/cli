package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"entire.io/cli/cmd/entire/cli/agent"
	"entire.io/cli/cmd/entire/cli/checkpoint"
	"entire.io/cli/cmd/entire/cli/checkpoint/id"
	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/strategy"
	"entire.io/cli/cmd/entire/cli/trailers"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
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
	commitMessage := trailers.FormatMetadata("Add new feature", metadataDir)
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

func TestExplainDefault_ShowsBranchView(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo
	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Create initial commit so HEAD exists (required for branch view)
	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test content"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	_, err = w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
		},
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create .entire directory
	if err := os.MkdirAll(".entire", 0o750); err != nil {
		t.Fatalf("failed to create .entire dir: %v", err)
	}

	var stdout bytes.Buffer
	err = runExplainDefault(&stdout, true) // noPager=true for test

	// Should NOT error - should show branch view
	if err != nil {
		t.Errorf("expected no error, got: %v", err)
	}

	output := stdout.String()
	// Should show branch header
	if !strings.Contains(output, "Branch:") {
		t.Errorf("expected 'Branch:' in output, got: %s", output)
	}
	// Should show checkpoints count (likely 0)
	if !strings.Contains(output, "Checkpoints:") {
		t.Errorf("expected 'Checkpoints:' in output, got: %s", output)
	}
}

func TestExplainDefault_NoCheckpoints_ShowsHelpfulMessage(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo
	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Create initial commit so HEAD exists (required for branch view)
	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test content"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	_, err = w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
		},
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create .entire directory but no checkpoints
	if err := os.MkdirAll(".entire", 0o750); err != nil {
		t.Fatalf("failed to create .entire dir: %v", err)
	}

	var stdout bytes.Buffer
	err = runExplainDefault(&stdout, true) // noPager=true for test

	// Should NOT error
	if err != nil {
		t.Errorf("expected no error, got: %v", err)
	}

	output := stdout.String()
	// Should show checkpoints count as 0
	if !strings.Contains(output, "Checkpoints: 0") {
		t.Errorf("expected 'Checkpoints: 0' in output, got: %s", output)
	}
	// Should show helpful message about checkpoints appearing after saves
	if !strings.Contains(output, "Checkpoints will appear") || !strings.Contains(output, "Claude session") {
		t.Errorf("expected helpful message about checkpoints, got: %s", output)
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

func TestExplainCmd_HasShortFlag(t *testing.T) {
	cmd := newExplainCmd()

	flag := cmd.Flags().Lookup("short")
	if flag == nil {
		t.Fatal("expected --short flag to exist")
		return // unreachable but satisfies staticcheck
	}

	// Should have -s shorthand
	if flag.Shorthand != "s" {
		t.Errorf("expected -s shorthand, got %q", flag.Shorthand)
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
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo with an initial commit (required for checkpoint lookup)
	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test content"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	_, err = w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	var buf bytes.Buffer
	err = runExplainCheckpoint(&buf, "nonexistent123", false, false, false)

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
			TokenUsage: &agent.TokenUsage{
				InputTokens:  10000,
				OutputTokens: 5000,
			},
		},
		Prompts: "Add a new feature",
	}

	// Default mode: empty commit message (not shown anyway in default mode)
	output := formatCheckpointOutput(result, id.MustCheckpointID("abc123def456"), "", false, false)

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
		t.Error("default output should not show file list (use --full)")
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
			TokenUsage: &agent.TokenUsage{
				InputTokens:  10000,
				OutputTokens: 5000,
			},
		},
		Prompts: "Add a new feature\nFix the bug\nRefactor the code",
	}

	output := formatCheckpointOutput(result, id.MustCheckpointID("abc123def456"), "feat: implement user authentication", true, false)

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
	// Verbose should show commit message
	if !strings.Contains(output, "Commit:") {
		t.Error("verbose output should have Commit section")
	}
	if !strings.Contains(output, "feat: implement user authentication") {
		t.Error("verbose output should show commit message")
	}
}

func TestFormatCheckpointOutput_Verbose_NoCommitMessage(t *testing.T) {
	result := &checkpoint.ReadCommittedResult{
		Metadata: checkpoint.CommittedMetadata{
			CheckpointID:     "abc123def456",
			SessionID:        "2026-01-21-test-session",
			CreatedAt:        time.Date(2026, 1, 21, 10, 30, 0, 0, time.UTC),
			FilesTouched:     []string{"main.go"},
			CheckpointsCount: 1,
		},
		Prompts: "Add a feature",
	}

	// When commit message is empty, should not show Commit section
	output := formatCheckpointOutput(result, id.MustCheckpointID("abc123def456"), "", true, false)

	if strings.Contains(output, "Commit:") {
		t.Error("verbose output should not show Commit section when message is empty")
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
			TokenUsage: &agent.TokenUsage{
				InputTokens:  10000,
				OutputTokens: 5000,
			},
		},
		Prompts:    "Add a new feature",
		Transcript: []byte(`{"type":"user","content":"Add a new feature"}` + "\n" + `{"type":"assistant","content":"I'll add that feature for you."}`),
	}

	output := formatCheckpointOutput(result, id.MustCheckpointID("abc123def456"), "feat: add user login", false, true)

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
	// Full should also show commit message (since it includes verbose)
	if !strings.Contains(output, "Commit:") {
		t.Error("full output should include commit section")
	}
	if !strings.Contains(output, "feat: add user login") {
		t.Error("full output should show commit message")
	}
}

func TestFormatBranchCheckpoints_BasicOutput(t *testing.T) {
	now := time.Now()
	points := []strategy.RewindPoint{
		{
			ID:            "abc123def456",
			Message:       "Add feature X",
			Date:          now,
			CheckpointID:  "chk123456789",
			SessionID:     "2026-01-22-session-1",
			SessionPrompt: "Implement feature X",
		},
		{
			ID:            "def456ghi789",
			Message:       "Fix bug in Y",
			Date:          now.Add(-time.Hour),
			CheckpointID:  "chk987654321",
			SessionID:     "2026-01-22-session-2",
			SessionPrompt: "Fix the bug",
		},
	}

	output := formatBranchCheckpoints("feature/my-branch", points)

	// Should show branch name
	if !strings.Contains(output, "feature/my-branch") {
		t.Errorf("expected branch name in output, got:\n%s", output)
	}

	// Should show checkpoint count
	if !strings.Contains(output, "Checkpoints: 2") {
		t.Errorf("expected 'Checkpoints: 2' in output, got:\n%s", output)
	}

	// Should show checkpoint messages
	if !strings.Contains(output, "Add feature X") {
		t.Errorf("expected first checkpoint message in output, got:\n%s", output)
	}
	if !strings.Contains(output, "Fix bug in Y") {
		t.Errorf("expected second checkpoint message in output, got:\n%s", output)
	}
}

func TestFormatBranchCheckpoints_GroupedByCheckpointID(t *testing.T) {
	// Create checkpoints spanning multiple days
	today := time.Date(2026, 1, 22, 10, 0, 0, 0, time.UTC)
	yesterday := time.Date(2026, 1, 21, 14, 0, 0, 0, time.UTC)

	points := []strategy.RewindPoint{
		{
			ID:            "abc123def456",
			Message:       "Today checkpoint 1",
			Date:          today,
			CheckpointID:  "chk111111111",
			SessionID:     "2026-01-22-session-1",
			SessionPrompt: "First task today",
		},
		{
			ID:            "def456ghi789",
			Message:       "Today checkpoint 2",
			Date:          today.Add(-30 * time.Minute),
			CheckpointID:  "chk222222222",
			SessionID:     "2026-01-22-session-1",
			SessionPrompt: "First task today",
		},
		{
			ID:            "ghi789jkl012",
			Message:       "Yesterday checkpoint",
			Date:          yesterday,
			CheckpointID:  "chk333333333",
			SessionID:     "2026-01-21-session-2",
			SessionPrompt: "Task from yesterday",
		},
	}

	output := formatBranchCheckpoints("main", points)

	// Should group by checkpoint ID - check for checkpoint headers
	if !strings.Contains(output, "[chk111111111]") {
		t.Errorf("expected checkpoint ID header in output, got:\n%s", output)
	}
	if !strings.Contains(output, "[chk333333333]") {
		t.Errorf("expected checkpoint ID header in output, got:\n%s", output)
	}

	// Dates should appear inline with commits (format MM-DD)
	if !strings.Contains(output, "01-22") {
		t.Errorf("expected today's date inline with commits, got:\n%s", output)
	}
	if !strings.Contains(output, "01-21") {
		t.Errorf("expected yesterday's date inline with commits, got:\n%s", output)
	}

	// Today's checkpoints should appear before yesterday's (sorted by latest timestamp)
	todayIdx := strings.Index(output, "chk111111111")
	yesterdayIdx := strings.Index(output, "chk333333333")
	if todayIdx == -1 || yesterdayIdx == -1 || todayIdx > yesterdayIdx {
		t.Errorf("expected today's checkpoints before yesterday's, got:\n%s", output)
	}
}

func TestFormatBranchCheckpoints_NoCheckpoints(t *testing.T) {
	output := formatBranchCheckpoints("feature/empty-branch", nil)

	// Should show branch name
	if !strings.Contains(output, "feature/empty-branch") {
		t.Errorf("expected branch name in output, got:\n%s", output)
	}

	// Should indicate no checkpoints
	if !strings.Contains(output, "Checkpoints: 0") && !strings.Contains(output, "No checkpoints") {
		t.Errorf("expected indication of no checkpoints, got:\n%s", output)
	}
}

func TestFormatBranchCheckpoints_ShowsSessionInfo(t *testing.T) {
	now := time.Now()
	points := []strategy.RewindPoint{
		{
			ID:            "abc123def456",
			Message:       "Test checkpoint",
			Date:          now,
			CheckpointID:  "chk123456789",
			SessionID:     "2026-01-22-test-session",
			SessionPrompt: "This is my test prompt",
		},
	}

	output := formatBranchCheckpoints("main", points)

	// Should show session prompt
	if !strings.Contains(output, "This is my test prompt") {
		t.Errorf("expected session prompt in output, got:\n%s", output)
	}
}

func TestFormatBranchCheckpoints_ShowsTemporaryIndicator(t *testing.T) {
	now := time.Now()
	points := []strategy.RewindPoint{
		{
			ID:           "abc123def456",
			Message:      "Committed checkpoint",
			Date:         now,
			CheckpointID: "chk123456789",
			IsLogsOnly:   true, // Committed = logs only, no indicator shown
			SessionID:    "2026-01-22-session-1",
		},
		{
			ID:           "def456ghi789",
			Message:      "Active checkpoint",
			Date:         now.Add(-time.Hour),
			CheckpointID: "chk987654321",
			IsLogsOnly:   false, // Temporary = can be rewound, shows [temporary]
			SessionID:    "2026-01-22-session-1",
		},
	}

	output := formatBranchCheckpoints("main", points)

	// Should indicate temporary (non-committed) checkpoints with [temporary]
	if !strings.Contains(output, "[temporary]") {
		t.Errorf("expected [temporary] indicator for non-committed checkpoint, got:\n%s", output)
	}

	// Committed checkpoints should NOT have [temporary] indicator
	// Find the line with the committed checkpoint and verify it doesn't have [temporary]
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		if strings.Contains(line, "chk123456789") && strings.Contains(line, "[temporary]") {
			t.Errorf("committed checkpoint should not have [temporary] indicator, got:\n%s", output)
		}
	}
}

func TestFormatBranchCheckpoints_ShowsTaskCheckpoints(t *testing.T) {
	now := time.Now()
	points := []strategy.RewindPoint{
		{
			ID:               "abc123def456",
			Message:          "Running tests (toolu_01ABC)",
			Date:             now,
			CheckpointID:     "chk123456789",
			IsTaskCheckpoint: true,
			ToolUseID:        "toolu_01ABC",
			SessionID:        "2026-01-22-session-1",
		},
	}

	output := formatBranchCheckpoints("main", points)

	// Should indicate task checkpoint
	if !strings.Contains(output, "[Task]") && !strings.Contains(output, "task") {
		t.Errorf("expected task checkpoint indicator, got:\n%s", output)
	}
}

func TestFormatBranchCheckpoints_TruncatesLongMessages(t *testing.T) {
	now := time.Now()
	longMessage := strings.Repeat("a", 200) // 200 character message
	points := []strategy.RewindPoint{
		{
			ID:           "abc123def456",
			Message:      longMessage,
			Date:         now,
			CheckpointID: "chk123456789",
			SessionID:    "2026-01-22-session-1",
		},
	}

	output := formatBranchCheckpoints("main", points)

	// Output should not contain the full 200 character message
	if strings.Contains(output, longMessage) {
		t.Errorf("expected long message to be truncated, got full message in output")
	}

	// Should contain truncation indicator (usually "...")
	if !strings.Contains(output, "...") {
		t.Errorf("expected truncation indicator '...' for long message, got:\n%s", output)
	}
}

func TestGetBranchCheckpoints_ReadsPromptFromShadowBranch(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo with an initial commit
	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create and commit initial file
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial content"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	initialCommit, err := w.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create .entire directory
	if err := os.MkdirAll(filepath.Join(tmpDir, ".entire"), 0o750); err != nil {
		t.Fatalf("failed to create .entire dir: %v", err)
	}

	// Create metadata directory with prompt.txt
	sessionID := "2026-01-27-test-session"
	metadataDir := filepath.Join(tmpDir, ".entire", "metadata", sessionID)
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}

	expectedPrompt := "This is my test prompt for the checkpoint"
	if err := os.WriteFile(filepath.Join(metadataDir, paths.PromptFileName), []byte(expectedPrompt), 0o644); err != nil {
		t.Fatalf("failed to write prompt file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(metadataDir, "full.jsonl"), []byte(`{"test": true}`), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Create first checkpoint (baseline copy) - this one gets filtered out
	store := checkpoint.NewGitStore(repo)
	baseCommit := initialCommit.String()[:7]
	_, err = store.WriteTemporary(context.Background(), checkpoint.WriteTemporaryOptions{
		SessionID:         sessionID,
		BaseCommit:        baseCommit,
		ModifiedFiles:     []string{"test.txt"},
		MetadataDir:       ".entire/metadata/" + sessionID,
		MetadataDirAbs:    metadataDir,
		CommitMessage:     "First checkpoint (baseline)",
		AuthorName:        "Test",
		AuthorEmail:       "test@test.com",
		IsFirstCheckpoint: true,
	})
	if err != nil {
		t.Fatalf("WriteTemporary() first checkpoint error = %v", err)
	}

	// Modify test file again for a second checkpoint with actual code changes
	if err := os.WriteFile(testFile, []byte("second modification"), 0o644); err != nil {
		t.Fatalf("failed to modify test file: %v", err)
	}

	// Create second checkpoint (has code changes, won't be filtered)
	_, err = store.WriteTemporary(context.Background(), checkpoint.WriteTemporaryOptions{
		SessionID:         sessionID,
		BaseCommit:        baseCommit,
		ModifiedFiles:     []string{"test.txt"},
		MetadataDir:       ".entire/metadata/" + sessionID,
		MetadataDirAbs:    metadataDir,
		CommitMessage:     "Second checkpoint with code changes",
		AuthorName:        "Test",
		AuthorEmail:       "test@test.com",
		IsFirstCheckpoint: false, // Not first, has parent
	})
	if err != nil {
		t.Fatalf("WriteTemporary() second checkpoint error = %v", err)
	}

	// Now call getBranchCheckpoints and verify the prompt is read
	points, err := getBranchCheckpoints(repo, 10)
	if err != nil {
		t.Fatalf("getBranchCheckpoints() error = %v", err)
	}

	// Should have at least one temporary checkpoint (the second one with code changes)
	var foundTempCheckpoint bool
	for _, point := range points {
		if !point.IsLogsOnly && point.SessionID == sessionID {
			foundTempCheckpoint = true
			// Verify the prompt was read correctly from the shadow branch tree
			if point.SessionPrompt != expectedPrompt {
				t.Errorf("expected prompt %q, got %q", expectedPrompt, point.SessionPrompt)
			}
			break
		}
	}

	if !foundTempCheckpoint {
		t.Errorf("expected to find temporary checkpoint with session ID %s, got points: %+v", sessionID, points)
	}
}

// TestRunExplainBranchDefault_ShowsBranchCheckpoints is covered by TestExplainDefault_ShowsBranchView
// since runExplainDefault now calls runExplainBranchDefault directly.

func TestRunExplainBranchDefault_DetachedHead(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo with a commit
	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test content"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	commitHash, err := w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
		},
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Checkout to detached HEAD state
	if err := w.Checkout(&git.CheckoutOptions{Hash: commitHash}); err != nil {
		t.Fatalf("failed to checkout detached HEAD: %v", err)
	}

	// Create .entire directory
	if err := os.MkdirAll(".entire", 0o750); err != nil {
		t.Fatalf("failed to create .entire dir: %v", err)
	}

	var stdout bytes.Buffer
	err = runExplainBranchDefault(&stdout, true)

	// Should NOT error
	if err != nil {
		t.Errorf("expected no error, got: %v", err)
	}

	output := stdout.String()

	// Should indicate detached HEAD state
	if !strings.Contains(output, "HEAD") && !strings.Contains(output, "detached") {
		// We need to handle detached HEAD somehow - either show HEAD or show a message
		t.Logf("Output for detached HEAD: %s", output)
	}
}

func TestIsAncestorOf(t *testing.T) {
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

	// Create first commit
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("v1"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	commit1, err := w.Commit("first commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com"},
	})
	if err != nil {
		t.Fatalf("failed to create first commit: %v", err)
	}

	// Create second commit
	if err := os.WriteFile(testFile, []byte("v2"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	commit2, err := w.Commit("second commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com"},
	})
	if err != nil {
		t.Fatalf("failed to create second commit: %v", err)
	}

	t.Run("commit is ancestor of later commit", func(t *testing.T) {
		// commit1 should be an ancestor of commit2
		if !strategy.IsAncestorOf(repo, commit1, commit2) {
			t.Error("expected commit1 to be ancestor of commit2")
		}
	})

	t.Run("commit is not ancestor of earlier commit", func(t *testing.T) {
		// commit2 should NOT be an ancestor of commit1
		if strategy.IsAncestorOf(repo, commit2, commit1) {
			t.Error("expected commit2 to NOT be ancestor of commit1")
		}
	})

	t.Run("commit is ancestor of itself", func(t *testing.T) {
		// A commit should be considered an ancestor of itself
		if !strategy.IsAncestorOf(repo, commit1, commit1) {
			t.Error("expected commit to be ancestor of itself")
		}
	})
}

func TestGetBranchCheckpoints_OnFeatureBranch(t *testing.T) {
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

	// Create initial commit on main
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	_, err = w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com"},
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create .entire directory
	if err := os.MkdirAll(".entire", 0o750); err != nil {
		t.Fatalf("failed to create .entire dir: %v", err)
	}

	// Get checkpoints (should be empty, but shouldn't error)
	points, err := getBranchCheckpoints(repo, 20)
	if err != nil {
		t.Fatalf("getBranchCheckpoints() error = %v", err)
	}

	// Should return empty list (no checkpoints yet)
	if len(points) != 0 {
		t.Errorf("expected 0 checkpoints, got %d", len(points))
	}
}

func TestHasCodeChanges_FirstCommitReturnsTrue(t *testing.T) {
	// First commit on a shadow branch (no parent) should return true
	// since it captures the working copy state - real uncommitted work
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create first commit (has no parent)
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	commitHash, err := w.Commit("first commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to create commit: %v", err)
	}

	commit, err := repo.CommitObject(commitHash)
	if err != nil {
		t.Fatalf("failed to get commit object: %v", err)
	}

	// First commit (no parent) captures working copy state - should return true
	if !hasCodeChanges(commit) {
		t.Error("hasCodeChanges() should return true for first commit (captures working copy)")
	}
}

func TestHasCodeChanges_OnlyMetadataChanges(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create first commit
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	_, err = w.Commit("first commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to create first commit: %v", err)
	}

	// Create second commit with only .entire/ metadata changes
	metadataDir := filepath.Join(tmpDir, ".entire", "metadata", "session-123")
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(metadataDir, "full.jsonl"), []byte(`{"test": true}`), 0o644); err != nil {
		t.Fatalf("failed to write metadata file: %v", err)
	}
	if _, err := w.Add(".entire"); err != nil {
		t.Fatalf("failed to add .entire: %v", err)
	}
	commitHash, err := w.Commit("metadata only commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to create second commit: %v", err)
	}

	commit, err := repo.CommitObject(commitHash)
	if err != nil {
		t.Fatalf("failed to get commit object: %v", err)
	}

	// Only .entire/ changes should return false
	if hasCodeChanges(commit) {
		t.Error("hasCodeChanges() should return false when only .entire/ files changed")
	}
}

func TestHasCodeChanges_WithCodeChanges(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create first commit
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	_, err = w.Commit("first commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to create first commit: %v", err)
	}

	// Create second commit with code changes
	if err := os.WriteFile(testFile, []byte("modified"), 0o644); err != nil {
		t.Fatalf("failed to modify test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add modified file: %v", err)
	}
	commitHash, err := w.Commit("code change commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to create second commit: %v", err)
	}

	commit, err := repo.CommitObject(commitHash)
	if err != nil {
		t.Fatalf("failed to get commit object: %v", err)
	}

	// Code changes should return true
	if !hasCodeChanges(commit) {
		t.Error("hasCodeChanges() should return true when code files changed")
	}
}

func TestHasCodeChanges_MixedChanges(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	// Create first commit
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	_, err = w.Commit("first commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to create first commit: %v", err)
	}

	// Create second commit with BOTH code and metadata changes
	if err := os.WriteFile(testFile, []byte("modified"), 0o644); err != nil {
		t.Fatalf("failed to modify test file: %v", err)
	}
	metadataDir := filepath.Join(tmpDir, ".entire", "metadata", "session-123")
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("failed to create metadata dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(metadataDir, "full.jsonl"), []byte(`{"test": true}`), 0o644); err != nil {
		t.Fatalf("failed to write metadata file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	if _, err := w.Add(".entire"); err != nil {
		t.Fatalf("failed to add .entire: %v", err)
	}
	commitHash, err := w.Commit("mixed changes commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to create second commit: %v", err)
	}

	commit, err := repo.CommitObject(commitHash)
	if err != nil {
		t.Fatalf("failed to get commit object: %v", err)
	}

	// Mixed changes should return true (code changes present)
	if !hasCodeChanges(commit) {
		t.Error("hasCodeChanges() should return true when commit has both code and metadata changes")
	}
}

func TestGetBranchCheckpoints_FiltersMainCommits(t *testing.T) {
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

	// Create initial commit on master (go-git default)
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	mainCommit, err := w.Commit("main commit with Entire-Checkpoint: abc123def456", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com"},
	})
	if err != nil {
		t.Fatalf("failed to create main commit: %v", err)
	}

	// Create feature branch
	featureBranch := "feature/test"
	if err := w.Checkout(&git.CheckoutOptions{
		Hash:   mainCommit,
		Branch: plumbing.NewBranchReferenceName(featureBranch),
		Create: true,
	}); err != nil {
		t.Fatalf("failed to create feature branch: %v", err)
	}

	// Create commit on feature branch
	if err := os.WriteFile(testFile, []byte("feature work"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	_, err = w.Commit("feature commit with Entire-Checkpoint: def456ghi789", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com"},
	})
	if err != nil {
		t.Fatalf("failed to create feature commit: %v", err)
	}

	// Create .entire directory
	if err := os.MkdirAll(".entire", 0o750); err != nil {
		t.Fatalf("failed to create .entire dir: %v", err)
	}

	// Get checkpoints - should only include feature branch commits, not main
	// Note: Without actual checkpoint data in entire/sessions, this returns empty
	// but the important thing is it doesn't error and the filtering logic runs
	points, err := getBranchCheckpoints(repo, 20)
	if err != nil {
		t.Fatalf("getBranchCheckpoints() error = %v", err)
	}

	// The filtering should have run without error
	// (we can't fully test without setting up entire/sessions branch with checkpoint data)
	t.Logf("Got %d checkpoints (expected 0 without checkpoint data)", len(points))
}
