package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
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

	generateFlag := cmd.Flags().Lookup("generate")
	if generateFlag == nil {
		t.Error("expected --generate flag to exist")
	}

	forceFlag := cmd.Flags().Lookup("force")
	if forceFlag == nil {
		t.Error("expected --force flag to exist")
	}
}

func TestExplainCmd_SearchAllFlag(t *testing.T) {
	cmd := newExplainCmd()

	// Verify --search-all flag exists
	flag := cmd.Flags().Lookup("search-all")
	if flag == nil {
		t.Fatal("expected --search-all flag to exist")
	}

	if flag.DefValue != "false" {
		t.Errorf("expected default value 'false', got %q", flag.DefValue)
	}
}

func TestExplainCmd_RejectsPositionalArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"positional arg without flags", []string{"abc123"}},
		{"positional arg with checkpoint flag", []string{"abc123", "--checkpoint", "def456"}},
		{"positional arg after flags", []string{"--checkpoint", "def456", "abc123"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newExplainCmd()
			var stdout, stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs(tt.args)

			err := cmd.Execute()
			if err == nil {
				t.Fatalf("expected error for positional args, got nil")
			}

			// Should show helpful error with hint
			if !strings.Contains(err.Error(), "unexpected argument") {
				t.Errorf("expected 'unexpected argument' error, got: %v", err)
			}
			if !strings.Contains(err.Error(), "Hint:") {
				t.Errorf("expected hint in error message, got: %v", err)
			}
		})
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
	err := runExplainCommit(&stdout, "nonexistent", false, false, false, false)

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
	err = runExplainCommit(&stdout, commitHash.String(), false, false, false, false)
	if err != nil {
		t.Fatalf("runExplainCommit() should not error for non-Entire commits, got: %v", err)
	}

	output := stdout.String()

	// Should show message indicating no Entire checkpoint (new behavior)
	if !strings.Contains(output, "No associated Entire checkpoint") {
		t.Errorf("expected output to indicate no Entire checkpoint, got: %s", output)
	}
	// Should mention the commit hash
	if !strings.Contains(output, commitHash.String()[:7]) {
		t.Errorf("expected output to contain short commit hash, got: %s", output)
	}
}

func TestExplainCommit_WithMetadataTrailerButNoCheckpoint(t *testing.T) {
	// Test that commits with Entire-Metadata trailer (but no Entire-Checkpoint)
	// now show "no checkpoint" message (new behavior)
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

	// Create a commit with Entire-Metadata trailer (but NO Entire-Checkpoint)
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("feature content"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}

	// Commit with Entire-Metadata trailer (no Entire-Checkpoint)
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
	err = runExplainCommit(&stdout, commitHash.String(), false, false, false, false)
	if err != nil {
		t.Fatalf("runExplainCommit() error = %v", err)
	}

	output := stdout.String()

	// New behavior: should show "no checkpoint" message since there's no Entire-Checkpoint trailer
	if !strings.Contains(output, "No associated Entire checkpoint") {
		t.Errorf("expected 'No associated Entire checkpoint' message, got: %s", output)
	}
	// Should mention the commit hash
	if !strings.Contains(output, commitHash.String()[:7]) {
		t.Errorf("expected output to contain short commit hash, got: %s", output)
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
	var stdout, stderr bytes.Buffer
	err := runExplain(&stdout, &stderr, "session-id", "commit-sha", "", false, false, false, false, false, false, false)

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

func TestExplainCmd_HasRawTranscriptFlag(t *testing.T) {
	cmd := newExplainCmd()

	flag := cmd.Flags().Lookup("raw-transcript")
	if flag == nil {
		t.Error("expected --raw-transcript flag to exist")
	}
}

func TestRunExplain_MutualExclusivityError(t *testing.T) {
	var buf, errBuf bytes.Buffer

	// Providing both --session and --checkpoint should error
	err := runExplain(&buf, &errBuf, "session-id", "", "checkpoint-id", false, false, false, false, false, false, false)

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

	var buf, errBuf bytes.Buffer
	err = runExplainCheckpoint(&buf, &errBuf, "nonexistent123", false, false, false, false, false, false, false)

	if err == nil {
		t.Error("expected error for nonexistent checkpoint")
	}
	if !strings.Contains(err.Error(), "checkpoint not found") {
		t.Errorf("expected 'checkpoint not found' error, got: %v", err)
	}
}

func TestFormatCheckpointOutput_Short(t *testing.T) {
	summary := &checkpoint.CheckpointSummary{
		CheckpointID:     id.MustCheckpointID("abc123def456"),
		CheckpointsCount: 3,
		FilesTouched:     []string{"main.go", "util.go"},
		TokenUsage: &agent.TokenUsage{
			InputTokens:  10000,
			OutputTokens: 5000,
		},
	}
	content := &checkpoint.SessionContent{
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
	output := formatCheckpointOutput(summary, content, id.MustCheckpointID("abc123def456"), nil, checkpoint.Author{}, false, false)

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
	// Transcript with user prompts that match what we expect to see
	transcriptContent := []byte(`{"type":"user","uuid":"u1","message":{"content":"Add a new feature"}}
{"type":"assistant","uuid":"a1","message":{"content":[{"type":"text","text":"I'll add the feature"}]}}
{"type":"user","uuid":"u2","message":{"content":"Fix the bug"}}
{"type":"assistant","uuid":"a2","message":{"content":[{"type":"text","text":"Fixed it"}]}}
{"type":"user","uuid":"u3","message":{"content":"Refactor the code"}}
`)

	summary := &checkpoint.CheckpointSummary{
		CheckpointID:     id.MustCheckpointID("abc123def456"),
		CheckpointsCount: 3,
		FilesTouched:     []string{"main.go", "util.go", "config.yaml"},
		TokenUsage: &agent.TokenUsage{
			InputTokens:  10000,
			OutputTokens: 5000,
		},
	}
	content := &checkpoint.SessionContent{
		Metadata: checkpoint.CommittedMetadata{
			CheckpointID:              "abc123def456",
			SessionID:                 "2026-01-21-test-session",
			CreatedAt:                 time.Date(2026, 1, 21, 10, 30, 0, 0, time.UTC),
			FilesTouched:              []string{"main.go", "util.go", "config.yaml"},
			CheckpointsCount:          3,
			CheckpointTranscriptStart: 0, // All content is this checkpoint's
			TokenUsage: &agent.TokenUsage{
				InputTokens:  10000,
				OutputTokens: 5000,
			},
		},
		Prompts:    "Add a new feature\nFix the bug\nRefactor the code",
		Transcript: transcriptContent,
	}

	output := formatCheckpointOutput(summary, content, id.MustCheckpointID("abc123def456"), nil, checkpoint.Author{}, true, false)

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
	// Verbose should show scoped transcript section
	if !strings.Contains(output, "Transcript (checkpoint scope):") {
		t.Error("verbose output should have Transcript (checkpoint scope) section")
	}
	if !strings.Contains(output, "Add a new feature") {
		t.Error("verbose output should show prompts")
	}
}

func TestFormatCheckpointOutput_Verbose_NoCommitMessage(t *testing.T) {
	summary := &checkpoint.CheckpointSummary{
		CheckpointID:     id.MustCheckpointID("abc123def456"),
		CheckpointsCount: 1,
		FilesTouched:     []string{"main.go"},
	}
	content := &checkpoint.SessionContent{
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
	output := formatCheckpointOutput(summary, content, id.MustCheckpointID("abc123def456"), nil, checkpoint.Author{}, true, false)

	if strings.Contains(output, "Commits:") {
		t.Error("verbose output should not show Commits section when nil (not searched)")
	}
}

func TestFormatCheckpointOutput_Full(t *testing.T) {
	// Use proper transcript format that matches actual Claude transcripts
	transcriptData := `{"type":"user","message":{"content":"Add a new feature"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"I'll add that feature for you."}]}}`

	summary := &checkpoint.CheckpointSummary{
		CheckpointID:     id.MustCheckpointID("abc123def456"),
		CheckpointsCount: 3,
		FilesTouched:     []string{"main.go", "util.go"},
		TokenUsage: &agent.TokenUsage{
			InputTokens:  10000,
			OutputTokens: 5000,
		},
	}
	content := &checkpoint.SessionContent{
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
		Transcript: []byte(transcriptData),
	}

	output := formatCheckpointOutput(summary, content, id.MustCheckpointID("abc123def456"), nil, checkpoint.Author{}, false, true)

	// Should show checkpoint ID (like default)
	if !strings.Contains(output, "abc123def456") {
		t.Error("expected checkpoint ID in output")
	}
	// Full should also include verbose sections (files)
	if !strings.Contains(output, "Files:") {
		t.Error("full output should include files section")
	}
	// Full shows full session transcript (not scoped)
	if !strings.Contains(output, "Transcript (full session):") {
		t.Error("full output should have Transcript (full session) section")
	}
	// Should contain actual transcript content (parsed format)
	if !strings.Contains(output, "Add a new feature") {
		t.Error("full output should show transcript content")
	}
	if !strings.Contains(output, "[Assistant]") {
		t.Error("full output should show assistant messages in parsed transcript")
	}
}

func TestFormatCheckpointOutput_WithSummary(t *testing.T) {
	cpID := id.MustCheckpointID("abc123456789")
	summary := &checkpoint.CheckpointSummary{
		CheckpointID: cpID,
		FilesTouched: []string{"file1.go", "file2.go"},
	}
	content := &checkpoint.SessionContent{
		Metadata: checkpoint.CommittedMetadata{
			CheckpointID: cpID,
			SessionID:    "2026-01-22-test-session",
			CreatedAt:    time.Date(2026, 1, 22, 10, 30, 0, 0, time.UTC),
			FilesTouched: []string{"file1.go", "file2.go"},
			Summary: &checkpoint.Summary{
				Intent:  "Implement user authentication",
				Outcome: "Added login and logout functionality",
				Learnings: checkpoint.LearningsSummary{
					Repo:     []string{"Uses JWT for auth tokens"},
					Code:     []checkpoint.CodeLearning{{Path: "auth.go", Line: 42, Finding: "Token validation happens here"}},
					Workflow: []string{"Always run tests after auth changes"},
				},
				Friction:  []string{"Had to refactor session handling"},
				OpenItems: []string{"Add password reset flow"},
			},
		},
		Prompts: "Add user authentication",
	}

	// Test default output (non-verbose) with summary
	output := formatCheckpointOutput(summary, content, cpID, nil, checkpoint.Author{}, false, false)

	// Should show AI-generated intent and outcome
	if !strings.Contains(output, "Intent: Implement user authentication") {
		t.Errorf("expected AI intent in output, got:\n%s", output)
	}
	if !strings.Contains(output, "Outcome: Added login and logout functionality") {
		t.Errorf("expected AI outcome in output, got:\n%s", output)
	}
	// Non-verbose should NOT show learnings
	if strings.Contains(output, "Learnings:") {
		t.Errorf("non-verbose should not show learnings, got:\n%s", output)
	}

	// Test verbose output with summary
	verboseOutput := formatCheckpointOutput(summary, content, cpID, nil, checkpoint.Author{}, true, false)

	// Verbose should show learnings sections
	if !strings.Contains(verboseOutput, "Learnings:") {
		t.Errorf("verbose output should show learnings, got:\n%s", verboseOutput)
	}
	if !strings.Contains(verboseOutput, "Repository:") {
		t.Errorf("verbose output should show repository learnings, got:\n%s", verboseOutput)
	}
	if !strings.Contains(verboseOutput, "Uses JWT for auth tokens") {
		t.Errorf("verbose output should show repo learning content, got:\n%s", verboseOutput)
	}
	if !strings.Contains(verboseOutput, "Code:") {
		t.Errorf("verbose output should show code learnings, got:\n%s", verboseOutput)
	}
	if !strings.Contains(verboseOutput, "auth.go:42:") {
		t.Errorf("verbose output should show code learning with line number, got:\n%s", verboseOutput)
	}
	if !strings.Contains(verboseOutput, "Workflow:") {
		t.Errorf("verbose output should show workflow learnings, got:\n%s", verboseOutput)
	}
	if !strings.Contains(verboseOutput, "Friction:") {
		t.Errorf("verbose output should show friction, got:\n%s", verboseOutput)
	}
	if !strings.Contains(verboseOutput, "Open Items:") {
		t.Errorf("verbose output should show open items, got:\n%s", verboseOutput)
	}
}

func TestFormatSummaryDetails(t *testing.T) {
	summary := &checkpoint.Summary{
		Intent:  "Test intent",
		Outcome: "Test outcome",
		Learnings: checkpoint.LearningsSummary{
			Repo:     []string{"Repo learning 1", "Repo learning 2"},
			Code:     []checkpoint.CodeLearning{{Path: "test.go", Line: 10, EndLine: 20, Finding: "Code finding"}},
			Workflow: []string{"Workflow learning"},
		},
		Friction:  []string{"Friction item"},
		OpenItems: []string{"Open item 1", "Open item 2"},
	}

	var sb strings.Builder
	formatSummaryDetails(&sb, summary)
	output := sb.String()

	// Check learnings
	if !strings.Contains(output, "Learnings:") {
		t.Error("should have Learnings section")
	}
	if !strings.Contains(output, "Repo learning 1") {
		t.Error("should include repo learnings")
	}
	if !strings.Contains(output, "test.go:10-20:") {
		t.Error("should show code learning with line range")
	}

	// Check friction
	if !strings.Contains(output, "Friction:") {
		t.Error("should have Friction section")
	}
	if !strings.Contains(output, "Friction item") {
		t.Error("should include friction items")
	}

	// Check open items
	if !strings.Contains(output, "Open Items:") {
		t.Error("should have Open Items section")
	}
	if !strings.Contains(output, "Open item 1") {
		t.Error("should include open items")
	}
}

func TestFormatSummaryDetails_EmptyCategories(t *testing.T) {
	// Test with empty learnings - should not show Learnings section
	summary := &checkpoint.Summary{
		Intent:    "Test intent",
		Outcome:   "Test outcome",
		Learnings: checkpoint.LearningsSummary{},
		Friction:  []string{},
		OpenItems: []string{},
	}

	var sb strings.Builder
	formatSummaryDetails(&sb, summary)
	output := sb.String()

	// Empty summary should have no sections
	if strings.Contains(output, "Learnings:") {
		t.Error("empty learnings should not show Learnings section")
	}
	if strings.Contains(output, "Friction:") {
		t.Error("empty friction should not show Friction section")
	}
	if strings.Contains(output, "Open Items:") {
		t.Error("empty open items should not show Open Items section")
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

	output := formatBranchCheckpoints("feature/my-branch", points, "")

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

	output := formatBranchCheckpoints("main", points, "")

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
	output := formatBranchCheckpoints("feature/empty-branch", nil, "")

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

	output := formatBranchCheckpoints("main", points, "")

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

	output := formatBranchCheckpoints("main", points, "")

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

	output := formatBranchCheckpoints("main", points, "")

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

	output := formatBranchCheckpoints("main", points, "")

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

func TestGetCurrentWorktreeHash_MainWorktree(t *testing.T) {
	// In a temp dir with a real .git directory (main worktree), getCurrentWorktreeHash
	// should return the hash of empty string (main worktree ID is "").
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	if _, err := git.PlainInit(tmpDir, false); err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	hash := getCurrentWorktreeHash()
	expected := checkpoint.HashWorktreeID("") // Main worktree has empty ID
	if hash != expected {
		t.Errorf("getCurrentWorktreeHash() = %q, want %q (hash of empty worktree ID)", hash, expected)
	}
}

func TestGetReachableTemporaryCheckpoints_FiltersByWorktree(t *testing.T) {
	// Shadow branches are namespaced by worktree hash (entire/<commit>-<worktreeHash>).
	// Only shadow branches matching the current worktree should be included.
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

	// Create initial commit
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	initialCommit, err := w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Setup metadata for both sessions
	sessionIDLocal := "2026-02-10-local-session"
	sessionIDOther := "2026-02-10-other-session"
	for _, sid := range []string{sessionIDLocal, sessionIDOther} {
		metaDir := filepath.Join(tmpDir, ".entire", "metadata", sid)
		if err := os.MkdirAll(metaDir, 0o755); err != nil {
			t.Fatalf("failed to create metadata dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(metaDir, paths.PromptFileName), []byte("test"), 0o644); err != nil {
			t.Fatalf("failed to write prompt: %v", err)
		}
		if err := os.WriteFile(filepath.Join(metaDir, "full.jsonl"), []byte(`{"test":true}`), 0o644); err != nil {
			t.Fatalf("failed to write transcript: %v", err)
		}
	}

	store := checkpoint.NewGitStore(repo)
	baseCommit := initialCommit.String()[:7]

	writeCheckpoints := func(sessionID, worktreeID string) {
		t.Helper()
		metaDirAbs := filepath.Join(tmpDir, ".entire", "metadata", sessionID)
		// Baseline
		if _, err := store.WriteTemporary(context.Background(), checkpoint.WriteTemporaryOptions{
			SessionID: sessionID, BaseCommit: baseCommit, WorktreeID: worktreeID,
			ModifiedFiles: []string{"test.txt"}, MetadataDir: ".entire/metadata/" + sessionID,
			MetadataDirAbs: metaDirAbs, CommitMessage: "baseline", AuthorName: "Test",
			AuthorEmail: "test@test.com", IsFirstCheckpoint: true,
		}); err != nil {
			t.Fatalf("WriteTemporary baseline error: %v", err)
		}
		// Code change checkpoint
		if err := os.WriteFile(testFile, []byte(sessionID+" changes"), 0o644); err != nil {
			t.Fatalf("failed to modify test file: %v", err)
		}
		if _, err := store.WriteTemporary(context.Background(), checkpoint.WriteTemporaryOptions{
			SessionID: sessionID, BaseCommit: baseCommit, WorktreeID: worktreeID,
			ModifiedFiles: []string{"test.txt"}, MetadataDir: ".entire/metadata/" + sessionID,
			MetadataDirAbs: metaDirAbs, CommitMessage: "code changes", AuthorName: "Test",
			AuthorEmail: "test@test.com", IsFirstCheckpoint: false,
		}); err != nil {
			t.Fatalf("WriteTemporary code changes error: %v", err)
		}
	}

	writeCheckpoints(sessionIDLocal, "")               // Main worktree (matches test env)
	writeCheckpoints(sessionIDOther, "other-worktree") // Different worktree

	// getBranchCheckpoints should only include local worktree's checkpoints
	points, err := getBranchCheckpoints(repo, 20)
	if err != nil {
		t.Fatalf("getBranchCheckpoints error: %v", err)
	}

	for _, p := range points {
		if p.SessionID == sessionIDOther {
			t.Errorf("found checkpoint from other worktree (session %s) - should be filtered out", sessionIDOther)
		}
	}
	var foundLocal bool
	for _, p := range points {
		if p.SessionID == sessionIDLocal {
			foundLocal = true
		}
	}
	if !foundLocal {
		t.Errorf("expected local worktree checkpoint (session %s), got: %+v", sessionIDLocal, points)
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

	// Should indicate detached HEAD state in branch name
	if !strings.Contains(output, "HEAD") && !strings.Contains(output, "detached") {
		t.Errorf("expected output to indicate detached HEAD state, got: %s", output)
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
	// Note: Without actual checkpoint data in entire/checkpoints/v1, this returns empty
	// but the important thing is it doesn't error and the filtering logic runs
	points, err := getBranchCheckpoints(repo, 20)
	if err != nil {
		t.Fatalf("getBranchCheckpoints() error = %v", err)
	}

	// Without checkpoint data (no entire/checkpoints/v1 branch), should return 0 checkpoints
	// This validates the filtering code path runs without error
	if len(points) != 0 {
		t.Errorf("expected 0 checkpoints without checkpoint data, got %d", len(points))
	}
}

func TestScopeTranscriptForCheckpoint_SlicesTranscript(t *testing.T) {
	// Transcript with 5 lines - prompts 1, 2, 3 with their responses
	fullTranscript := []byte(`{"type":"user","uuid":"u1","message":{"content":"prompt 1"}}
{"type":"assistant","uuid":"a1","message":{"content":[{"type":"text","text":"response 1"}]}}
{"type":"user","uuid":"u2","message":{"content":"prompt 2"}}
{"type":"assistant","uuid":"a2","message":{"content":[{"type":"text","text":"response 2"}]}}
{"type":"user","uuid":"u3","message":{"content":"prompt 3"}}
`)

	// Checkpoint starts at line 2 (after prompt 1 and response 1)
	// Should only include lines 2-4 (prompt 2, response 2, prompt 3)
	scoped := scopeTranscriptForCheckpoint(fullTranscript, 2, agent.AgentTypeClaudeCode)

	// Parse the scoped transcript to verify content
	lines, err := parseTranscriptFromBytes(scoped)
	if err != nil {
		t.Fatalf("failed to parse scoped transcript: %v", err)
	}

	if len(lines) != 3 {
		t.Fatalf("expected 3 lines in scoped transcript, got %d", len(lines))
	}

	// First line should be prompt 2 (u2), not prompt 1
	if lines[0].UUID != "u2" {
		t.Errorf("expected first line to be u2 (prompt 2), got %s", lines[0].UUID)
	}

	// Last line should be prompt 3 (u3)
	if lines[2].UUID != "u3" {
		t.Errorf("expected last line to be u3 (prompt 3), got %s", lines[2].UUID)
	}
}

func TestScopeTranscriptForCheckpoint_ZeroLinesReturnsAll(t *testing.T) {
	transcript := []byte(`{"type":"user","uuid":"u1","message":{"content":"prompt 1"}}
{"type":"user","uuid":"u2","message":{"content":"prompt 2"}}
`)

	// With linesAtStart=0, should return full transcript
	scoped := scopeTranscriptForCheckpoint(transcript, 0, agent.AgentTypeClaudeCode)

	lines, err := parseTranscriptFromBytes(scoped)
	if err != nil {
		t.Fatalf("failed to parse scoped transcript: %v", err)
	}

	if len(lines) != 2 {
		t.Fatalf("expected 2 lines with linesAtStart=0, got %d", len(lines))
	}
}

func TestExtractPromptsFromScopedTranscript(t *testing.T) {
	// Transcript with 4 lines - 2 user prompts, 2 assistant responses
	transcript := []byte(`{"type":"user","uuid":"u1","message":{"content":"First prompt"}}
{"type":"assistant","uuid":"a1","message":{"content":[{"type":"text","text":"First response"}]}}
{"type":"user","uuid":"u2","message":{"content":"Second prompt"}}
{"type":"assistant","uuid":"a2","message":{"content":[{"type":"text","text":"Second response"}]}}
`)

	prompts := extractPromptsFromTranscript(transcript, "")

	if len(prompts) != 2 {
		t.Fatalf("expected 2 prompts, got %d", len(prompts))
	}

	if prompts[0] != "First prompt" {
		t.Errorf("expected first prompt 'First prompt', got %q", prompts[0])
	}

	if prompts[1] != "Second prompt" {
		t.Errorf("expected second prompt 'Second prompt', got %q", prompts[1])
	}
}

func TestFormatCheckpointOutput_UsesScopedPrompts(t *testing.T) {
	// Full transcript with 4 lines (2 prompts + 2 responses)
	// Checkpoint starts at line 2 (should only show second prompt)
	fullTranscript := []byte(`{"type":"user","uuid":"u1","message":{"content":"First prompt - should NOT appear"}}
{"type":"assistant","uuid":"a1","message":{"content":[{"type":"text","text":"First response"}]}}
{"type":"user","uuid":"u2","message":{"content":"Second prompt - SHOULD appear"}}
{"type":"assistant","uuid":"a2","message":{"content":[{"type":"text","text":"Second response"}]}}
`)

	summary := &checkpoint.CheckpointSummary{
		CheckpointID: id.MustCheckpointID("abc123def456"),
		FilesTouched: []string{"main.go"},
	}
	content := &checkpoint.SessionContent{
		Metadata: checkpoint.CommittedMetadata{
			CheckpointID:              "abc123def456",
			SessionID:                 "2026-01-30-test-session",
			CreatedAt:                 time.Date(2026, 1, 30, 10, 30, 0, 0, time.UTC),
			FilesTouched:              []string{"main.go"},
			CheckpointTranscriptStart: 2, // Checkpoint starts at line 2
		},
		Prompts:    "First prompt - should NOT appear\nSecond prompt - SHOULD appear", // Full prompts (not scoped yet)
		Transcript: fullTranscript,
	}

	// Verbose output should use scoped prompts
	output := formatCheckpointOutput(summary, content, id.MustCheckpointID("abc123def456"), nil, checkpoint.Author{}, true, false)

	// Should show ONLY the second prompt (scoped)
	if !strings.Contains(output, "Second prompt - SHOULD appear") {
		t.Errorf("expected scoped prompt in output, got:\n%s", output)
	}

	// Should NOT show the first prompt (it's before this checkpoint's scope)
	if strings.Contains(output, "First prompt - should NOT appear") {
		t.Errorf("expected first prompt to be excluded from scoped output, got:\n%s", output)
	}
}

func TestFormatCheckpointOutput_FallsBackToStoredPrompts(t *testing.T) {
	// Test backwards compatibility: when no transcript exists, use stored prompts
	summary := &checkpoint.CheckpointSummary{
		CheckpointID: id.MustCheckpointID("abc123def456"),
		FilesTouched: []string{"main.go"},
	}
	content := &checkpoint.SessionContent{
		Metadata: checkpoint.CommittedMetadata{
			CheckpointID:              "abc123def456",
			SessionID:                 "2026-01-30-test-session",
			CreatedAt:                 time.Date(2026, 1, 30, 10, 30, 0, 0, time.UTC),
			FilesTouched:              []string{"main.go"},
			CheckpointTranscriptStart: 0,
		},
		Prompts:    "Stored prompt from older checkpoint",
		Transcript: nil, // No transcript available
	}

	// Verbose output should fall back to stored prompts
	output := formatCheckpointOutput(summary, content, id.MustCheckpointID("abc123def456"), nil, checkpoint.Author{}, true, false)

	// Intent should use stored prompt
	if !strings.Contains(output, "Stored prompt from older checkpoint") {
		t.Errorf("expected fallback to stored prompts, got:\n%s", output)
	}
}

func TestFormatCheckpointOutput_FullShowsEntireTranscript(t *testing.T) {
	// Test that --full mode shows the entire transcript, not scoped
	fullTranscript := []byte(`{"type":"user","uuid":"u1","message":{"content":"First prompt"}}
{"type":"assistant","uuid":"a1","message":{"content":[{"type":"text","text":"First response"}]}}
{"type":"user","uuid":"u2","message":{"content":"Second prompt"}}
{"type":"assistant","uuid":"a2","message":{"content":[{"type":"text","text":"Second response"}]}}
`)

	summary := &checkpoint.CheckpointSummary{
		CheckpointID: id.MustCheckpointID("abc123def456"),
		FilesTouched: []string{"main.go"},
	}
	content := &checkpoint.SessionContent{
		Metadata: checkpoint.CommittedMetadata{
			CheckpointID:              "abc123def456",
			SessionID:                 "2026-01-30-test-session",
			CreatedAt:                 time.Date(2026, 1, 30, 10, 30, 0, 0, time.UTC),
			FilesTouched:              []string{"main.go"},
			CheckpointTranscriptStart: 2, // Checkpoint starts at line 2
		},
		Transcript: fullTranscript,
	}

	// Full mode should show the ENTIRE transcript (not scoped)
	output := formatCheckpointOutput(summary, content, id.MustCheckpointID("abc123def456"), nil, checkpoint.Author{}, false, true)

	// Should show the full transcript including first prompt (even though scoped prompts exclude it)
	if !strings.Contains(output, "First prompt") {
		t.Errorf("expected --full to show entire transcript including first prompt, got:\n%s", output)
	}
	if !strings.Contains(output, "Second prompt") {
		t.Errorf("expected --full to show entire transcript including second prompt, got:\n%s", output)
	}
}

func TestRunExplainCommit_NoCheckpointTrailer(t *testing.T) {
	// Create test repo with a commit that has no Entire-Checkpoint trailer
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Create a commit without checkpoint trailer
	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("content"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	hash, err := w.Commit("Regular commit without trailer", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to create commit: %v", err)
	}

	var buf bytes.Buffer
	err = runExplainCommit(&buf, hash.String()[:7], false, false, false, false)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "No associated Entire checkpoint") {
		t.Errorf("expected 'No associated Entire checkpoint' message, got: %s", output)
	}
}

func TestRunExplainCommit_WithCheckpointTrailer(t *testing.T) {
	// Create test repo with a commit that has an Entire-Checkpoint trailer
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Create a commit with checkpoint trailer
	w, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("content"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}

	// Create commit with checkpoint trailer
	checkpointID := "abc123def456"
	commitMsg := "Feature commit\n\nEntire-Checkpoint: " + checkpointID + "\n"
	hash, err := w.Commit(commitMsg, &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("failed to create commit: %v", err)
	}

	var buf bytes.Buffer
	// This should try to look up the checkpoint and fail (checkpoint doesn't exist in store)
	// but it should still attempt the lookup rather than showing commit details
	err = runExplainCommit(&buf, hash.String()[:7], false, false, false, false)

	// Should error because the checkpoint doesn't exist in the store
	if err == nil {
		t.Fatalf("expected error for missing checkpoint in store, got nil")
	}

	// Error should mention checkpoint not found
	if !strings.Contains(err.Error(), "checkpoint not found") && !strings.Contains(err.Error(), "abc123def456") {
		t.Errorf("expected error about checkpoint not found, got: %v", err)
	}
}

func TestFormatBranchCheckpoints_SessionFilter(t *testing.T) {
	now := time.Now()
	points := []strategy.RewindPoint{
		{
			ID:            "abc123def456",
			Message:       "Checkpoint from session 1",
			Date:          now,
			CheckpointID:  "chk111111111",
			SessionID:     "2026-01-22-session-alpha",
			SessionPrompt: "Task for session alpha",
		},
		{
			ID:            "def456ghi789",
			Message:       "Checkpoint from session 2",
			Date:          now.Add(-time.Hour),
			CheckpointID:  "chk222222222",
			SessionID:     "2026-01-22-session-beta",
			SessionPrompt: "Task for session beta",
		},
		{
			ID:            "ghi789jkl012",
			Message:       "Another checkpoint from session 1",
			Date:          now.Add(-2 * time.Hour),
			CheckpointID:  "chk333333333",
			SessionID:     "2026-01-22-session-alpha",
			SessionPrompt: "Another task for session alpha",
		},
	}

	t.Run("no filter shows all checkpoints", func(t *testing.T) {
		output := formatBranchCheckpoints("main", points, "")

		// Should show all checkpoints
		if !strings.Contains(output, "Checkpoints: 3") {
			t.Errorf("expected 'Checkpoints: 3' in output, got:\n%s", output)
		}
		// Should show prompts from both sessions
		if !strings.Contains(output, "Task for session alpha") {
			t.Errorf("expected alpha session prompt in output, got:\n%s", output)
		}
		if !strings.Contains(output, "Task for session beta") {
			t.Errorf("expected beta session prompt in output, got:\n%s", output)
		}
	})

	t.Run("filter by exact session ID", func(t *testing.T) {
		output := formatBranchCheckpoints("main", points, "2026-01-22-session-alpha")

		// Should show only alpha checkpoints (2 of them)
		if !strings.Contains(output, "Checkpoints: 2") {
			t.Errorf("expected 'Checkpoints: 2' in output, got:\n%s", output)
		}
		if !strings.Contains(output, "Task for session alpha") {
			t.Errorf("expected alpha session prompt in output, got:\n%s", output)
		}
		// Should NOT contain beta session prompt
		if strings.Contains(output, "Task for session beta") {
			t.Errorf("expected output to NOT contain beta session prompt, got:\n%s", output)
		}
		// Should show filter info
		if !strings.Contains(output, "Filtered by session:") {
			t.Errorf("expected 'Filtered by session:' in output, got:\n%s", output)
		}
	})

	t.Run("filter by session ID prefix", func(t *testing.T) {
		output := formatBranchCheckpoints("main", points, "2026-01-22-session-b")

		// Should show only beta checkpoint (1)
		if !strings.Contains(output, "Checkpoints: 1") {
			t.Errorf("expected 'Checkpoints: 1' in output, got:\n%s", output)
		}
		if !strings.Contains(output, "Task for session beta") {
			t.Errorf("expected beta session prompt in output, got:\n%s", output)
		}
	})

	t.Run("filter with no matches", func(t *testing.T) {
		output := formatBranchCheckpoints("main", points, "nonexistent-session")

		// Should show 0 checkpoints
		if !strings.Contains(output, "Checkpoints: 0") {
			t.Errorf("expected 'Checkpoints: 0' in output, got:\n%s", output)
		}
		// Should show filter info even with no matches
		if !strings.Contains(output, "Filtered by session:") {
			t.Errorf("expected 'Filtered by session:' in output, got:\n%s", output)
		}
	})
}

func TestRunExplain_SessionFlagFiltersListView(t *testing.T) {
	// Test that --session alone (without --checkpoint or --commit) filters the list view
	// This is a unit test for the routing logic
	var buf, errBuf bytes.Buffer

	// When session is specified alone, it should NOT error for mutual exclusivity
	// It should route to the list view with a filter (which may fail for other reasons
	// like not being in a git repo, but not for mutual exclusivity)
	err := runExplain(&buf, &errBuf, "some-session", "", "", false, false, false, false, false, false, false)

	// Should NOT be a mutual exclusivity error
	if err != nil && strings.Contains(err.Error(), "cannot specify multiple") {
		t.Errorf("--session alone should not trigger mutual exclusivity error, got: %v", err)
	}
}

func TestRunExplain_SessionWithCheckpointStillMutuallyExclusive(t *testing.T) {
	// Test that --session with --checkpoint is still an error
	var buf, errBuf bytes.Buffer

	err := runExplain(&buf, &errBuf, "some-session", "", "some-checkpoint", false, false, false, false, false, false, false)

	if err == nil {
		t.Error("expected error when --session and --checkpoint both specified")
	}
	if !strings.Contains(err.Error(), "cannot specify multiple") {
		t.Errorf("expected 'cannot specify multiple' error, got: %v", err)
	}
}

func TestRunExplain_SessionWithCommitStillMutuallyExclusive(t *testing.T) {
	// Test that --session with --commit is still an error
	var buf, errBuf bytes.Buffer

	err := runExplain(&buf, &errBuf, "some-session", "some-commit", "", false, false, false, false, false, false, false)

	if err == nil {
		t.Error("expected error when --session and --commit both specified")
	}
	if !strings.Contains(err.Error(), "cannot specify multiple") {
		t.Errorf("expected 'cannot specify multiple' error, got: %v", err)
	}
}

func TestFormatCheckpointOutput_WithAuthor(t *testing.T) {
	summary := &checkpoint.CheckpointSummary{
		CheckpointID: id.MustCheckpointID("abc123def456"),
		FilesTouched: []string{"main.go"},
	}
	content := &checkpoint.SessionContent{
		Metadata: checkpoint.CommittedMetadata{
			CheckpointID:              "abc123def456",
			SessionID:                 "2026-01-30-test-session",
			CreatedAt:                 time.Date(2026, 1, 30, 10, 30, 0, 0, time.UTC),
			FilesTouched:              []string{"main.go"},
			CheckpointTranscriptStart: 0,
		},
		Prompts:    "Add a new feature",
		Transcript: nil, // No transcript available
	}

	author := checkpoint.Author{
		Name:  "Alice Developer",
		Email: "alice@example.com",
	}

	// With author, should show author line
	output := formatCheckpointOutput(summary, content, id.MustCheckpointID("abc123def456"), nil, author, true, false)

	if !strings.Contains(output, "Author: Alice Developer <alice@example.com>") {
		t.Errorf("expected author line in output, got:\n%s", output)
	}
}

func TestFormatCheckpointOutput_EmptyAuthor(t *testing.T) {
	// Test backwards compatibility: when no transcript exists, use stored prompts
	summary := &checkpoint.CheckpointSummary{
		CheckpointID: id.MustCheckpointID("abc123def456"),
		FilesTouched: []string{"main.go"},
	}
	content := &checkpoint.SessionContent{
		Metadata: checkpoint.CommittedMetadata{
			CheckpointID:              "abc123def456",
			SessionID:                 "2026-01-30-test-session",
			CreatedAt:                 time.Date(2026, 1, 30, 10, 30, 0, 0, time.UTC),
			FilesTouched:              []string{"main.go"},
			CheckpointTranscriptStart: 0,
		},
		Prompts:    "Add a new feature",
		Transcript: nil, // No transcript available
	}

	// Empty author - should not show author line
	author := checkpoint.Author{}

	output := formatCheckpointOutput(summary, content, id.MustCheckpointID("abc123def456"), nil, author, true, false)

	if strings.Contains(output, "Author:") {
		t.Errorf("expected no author line for empty author, got:\n%s", output)
	}
}

func TestGetAssociatedCommits(t *testing.T) {
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

	checkpointID := id.MustCheckpointID("abc123def456")

	// Create first commit without checkpoint trailer
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	_, err = w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Now().Add(-2 * time.Hour),
		},
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create commit with matching checkpoint trailer
	if err := os.WriteFile(testFile, []byte("with checkpoint"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	commitMsg := trailers.FormatCheckpoint("feat: add feature", checkpointID)
	_, err = w.Commit(commitMsg, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Alice Developer",
			Email: "alice@example.com",
			When:  time.Now().Add(-1 * time.Hour),
		},
	})
	if err != nil {
		t.Fatalf("failed to create checkpoint commit: %v", err)
	}

	// Create another commit without checkpoint trailer
	if err := os.WriteFile(testFile, []byte("after checkpoint"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	_, err = w.Commit("unrelated commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("failed to create unrelated commit: %v", err)
	}

	// Test: should find the one commit with matching checkpoint
	commits, err := getAssociatedCommits(repo, checkpointID, false)
	if err != nil {
		t.Fatalf("getAssociatedCommits error: %v", err)
	}

	if len(commits) != 1 {
		t.Fatalf("expected 1 associated commit, got %d", len(commits))
	}

	commit := commits[0]
	if commit.Author != "Alice Developer" {
		t.Errorf("expected author 'Alice Developer', got %q", commit.Author)
	}
	if !strings.Contains(commit.Message, "feat: add feature") {
		t.Errorf("expected message to contain 'feat: add feature', got %q", commit.Message)
	}
	if len(commit.ShortSHA) != 7 {
		t.Errorf("expected 7-char short SHA, got %d chars: %q", len(commit.ShortSHA), commit.ShortSHA)
	}
	if len(commit.SHA) != 40 {
		t.Errorf("expected 40-char full SHA, got %d chars", len(commit.SHA))
	}
}

func TestGetAssociatedCommits_NoMatches(t *testing.T) {
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

	// Create commit without checkpoint trailer
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test content"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	_, err = w.Commit("regular commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
		},
	})
	if err != nil {
		t.Fatalf("failed to create commit: %v", err)
	}

	// Search for a checkpoint ID that doesn't exist (valid format: 12 hex chars)
	checkpointID := id.MustCheckpointID("aaaa11112222")
	commits, err := getAssociatedCommits(repo, checkpointID, false)
	if err != nil {
		t.Fatalf("getAssociatedCommits error: %v", err)
	}

	if len(commits) != 0 {
		t.Errorf("expected 0 associated commits, got %d", len(commits))
	}
}

func TestGetAssociatedCommits_MultipleMatches(t *testing.T) {
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

	checkpointID := id.MustCheckpointID("abc123def456")

	// Create initial commit
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	_, err = w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Now().Add(-3 * time.Hour),
		},
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create first commit with checkpoint trailer
	if err := os.WriteFile(testFile, []byte("first"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	commitMsg := trailers.FormatCheckpoint("first checkpoint commit", checkpointID)
	_, err = w.Commit(commitMsg, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Now().Add(-2 * time.Hour),
		},
	})
	if err != nil {
		t.Fatalf("failed to create first checkpoint commit: %v", err)
	}

	// Create second commit with same checkpoint trailer (e.g., amend scenario)
	if err := os.WriteFile(testFile, []byte("second"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	commitMsg = trailers.FormatCheckpoint("second checkpoint commit", checkpointID)
	_, err = w.Commit(commitMsg, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Now().Add(-1 * time.Hour),
		},
	})
	if err != nil {
		t.Fatalf("failed to create second checkpoint commit: %v", err)
	}

	// Test: should find both commits with matching checkpoint
	commits, err := getAssociatedCommits(repo, checkpointID, false)
	if err != nil {
		t.Fatalf("getAssociatedCommits error: %v", err)
	}

	if len(commits) != 2 {
		t.Fatalf("expected 2 associated commits, got %d", len(commits))
	}

	// Should be in reverse chronological order (newest first)
	if !strings.Contains(commits[0].Message, "second") {
		t.Errorf("expected newest commit first, got %q", commits[0].Message)
	}
	if !strings.Contains(commits[1].Message, "first") {
		t.Errorf("expected older commit second, got %q", commits[1].Message)
	}
}

func TestFormatCheckpointOutput_WithAssociatedCommits(t *testing.T) {
	summary := &checkpoint.CheckpointSummary{
		CheckpointID: id.MustCheckpointID("abc123def456"),
		FilesTouched: []string{"main.go"},
	}
	content := &checkpoint.SessionContent{
		Metadata: checkpoint.CommittedMetadata{
			CheckpointID:              "abc123def456",
			SessionID:                 "2026-02-04-test-session",
			CreatedAt:                 time.Date(2026, 2, 4, 10, 30, 0, 0, time.UTC),
			FilesTouched:              []string{"main.go"},
			CheckpointTranscriptStart: 0,
		},
		Prompts:    "Add a new feature",
		Transcript: nil, // No transcript available
	}

	associatedCommits := []associatedCommit{
		{
			SHA:      "abc123def4567890abc123def4567890abc12345",
			ShortSHA: "abc123d",
			Message:  "feat: add feature",
			Author:   "Alice Developer",
			Date:     time.Date(2026, 2, 4, 11, 0, 0, 0, time.UTC),
		},
		{
			SHA:      "def456abc7890123def456abc7890123def45678",
			ShortSHA: "def456a",
			Message:  "fix: update feature",
			Author:   "Bob Developer",
			Date:     time.Date(2026, 2, 4, 12, 0, 0, 0, time.UTC),
		},
	}

	output := formatCheckpointOutput(summary, content, id.MustCheckpointID("abc123def456"), associatedCommits, checkpoint.Author{}, true, false)

	// Should show commits section with count
	if !strings.Contains(output, "Commits: (2)") {
		t.Errorf("expected 'Commits: (2)' in output, got:\n%s", output)
	}
	// Should show commit details
	if !strings.Contains(output, "abc123d") {
		t.Errorf("expected short SHA 'abc123d' in output, got:\n%s", output)
	}
	if !strings.Contains(output, "def456a") {
		t.Errorf("expected short SHA 'def456a' in output, got:\n%s", output)
	}
	if !strings.Contains(output, "feat: add feature") {
		t.Errorf("expected commit message in output, got:\n%s", output)
	}
	if !strings.Contains(output, "fix: update feature") {
		t.Errorf("expected commit message in output, got:\n%s", output)
	}
	// Should show date in format YYYY-MM-DD
	if !strings.Contains(output, "2026-02-04") {
		t.Errorf("expected date in output, got:\n%s", output)
	}
}

// createMergeCommit creates a merge commit with two parents using go-git plumbing APIs.
// Returns the merge commit hash.
func createMergeCommit(t *testing.T, repo *git.Repository, parent1, parent2 plumbing.Hash, treeHash plumbing.Hash, message string) plumbing.Hash {
	t.Helper()

	sig := object.Signature{
		Name:  "Test",
		Email: "test@example.com",
		When:  time.Now(),
	}
	commit := object.Commit{
		Author:       sig,
		Committer:    sig,
		Message:      message,
		TreeHash:     treeHash,
		ParentHashes: []plumbing.Hash{parent1, parent2},
	}
	obj := repo.Storer.NewEncodedObject()
	if err := commit.Encode(obj); err != nil {
		t.Fatalf("failed to encode merge commit: %v", err)
	}
	hash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("failed to store merge commit: %v", err)
	}
	return hash
}

func TestGetBranchCheckpoints_WithMergeFromMain(t *testing.T) {
	// Regression test: when main is merged into a feature branch, getBranchCheckpoints
	// should still find feature branch checkpoints from before the merge.
	// The old repo.Log() approach did a full DAG walk, entering main's history through
	// merge commits and eventually hitting consecutiveMainLimit, silently dropping
	// older feature branch checkpoints.
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

	// Create initial commit on master
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	initialCommit, err := w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now().Add(-5 * time.Hour)},
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create feature branch from initial commit
	featureBranch := plumbing.NewBranchReferenceName("feature/test")
	if err := w.Checkout(&git.CheckoutOptions{
		Hash:   initialCommit,
		Branch: featureBranch,
		Create: true,
	}); err != nil {
		t.Fatalf("failed to create feature branch: %v", err)
	}

	// Create first feature checkpoint commit (BEFORE the merge)
	cpID1 := id.MustCheckpointID("aaa111bbb222")
	if err := os.WriteFile(testFile, []byte("feature work 1"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	featureCommit1, err := w.Commit(trailers.FormatCheckpoint("feat: first feature", cpID1), &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now().Add(-4 * time.Hour)},
	})
	if err != nil {
		t.Fatalf("failed to create first feature commit: %v", err)
	}

	// Switch to master and add commits (simulating work on main)
	if err := w.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("master"),
	}); err != nil {
		t.Fatalf("failed to checkout master: %v", err)
	}
	if err := os.WriteFile(testFile, []byte("main work"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	mainCommit, err := w.Commit("main: add work", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now().Add(-3 * time.Hour)},
	})
	if err != nil {
		t.Fatalf("failed to create main commit: %v", err)
	}

	// Switch back to feature branch
	if err := w.Checkout(&git.CheckoutOptions{
		Branch: featureBranch,
	}); err != nil {
		t.Fatalf("failed to checkout feature branch: %v", err)
	}

	// Create merge commit: merge main into feature (feature is first parent, main is second parent)
	featureCommitObj, commitObjErr := repo.CommitObject(featureCommit1)
	if commitObjErr != nil {
		t.Fatalf("failed to get feature commit object: %v", commitObjErr)
	}
	featureTree, treeErr := featureCommitObj.Tree()
	if treeErr != nil {
		t.Fatalf("failed to get feature commit tree: %v", treeErr)
	}
	mergeHash := createMergeCommit(t, repo, featureCommit1, mainCommit, featureTree.Hash, "Merge branch 'master' into feature/test")

	// Update feature branch ref to point to merge commit
	ref := plumbing.NewHashReference(featureBranch, mergeHash)
	if err := repo.Storer.SetReference(ref); err != nil {
		t.Fatalf("failed to update feature branch ref: %v", err)
	}

	// Reset worktree to merge commit
	if err := w.Reset(&git.ResetOptions{Commit: mergeHash, Mode: git.HardReset}); err != nil {
		t.Fatalf("failed to reset to merge: %v", err)
	}

	// Create second feature checkpoint commit (AFTER the merge)
	cpID2 := id.MustCheckpointID("ccc333ddd444")
	if err := os.WriteFile(testFile, []byte("feature work 2"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	_, err = w.Commit(trailers.FormatCheckpoint("feat: second feature", cpID2), &git.CommitOptions{
		Author:    &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now().Add(-1 * time.Hour)},
		Parents:   []plumbing.Hash{mergeHash},
		Committer: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now().Add(-1 * time.Hour)},
	})
	if err != nil {
		t.Fatalf("failed to create second feature commit: %v", err)
	}

	// Create .entire directory
	if err := os.MkdirAll(".entire", 0o750); err != nil {
		t.Fatalf("failed to create .entire dir: %v", err)
	}

	// Test getAssociatedCommits - should find BOTH feature checkpoint commits
	// by walking first-parent chain (skipping the merge's second parent into main)
	commits1, err := getAssociatedCommits(repo, cpID1, false)
	if err != nil {
		t.Fatalf("getAssociatedCommits for cpID1 error: %v", err)
	}
	if len(commits1) != 1 {
		t.Errorf("expected 1 commit for cpID1 (first feature checkpoint), got %d", len(commits1))
	}

	commits2, err := getAssociatedCommits(repo, cpID2, false)
	if err != nil {
		t.Fatalf("getAssociatedCommits for cpID2 error: %v", err)
	}
	if len(commits2) != 1 {
		t.Errorf("expected 1 commit for cpID2 (second feature checkpoint), got %d", len(commits2))
	}
}

func TestGetBranchCheckpoints_MergeCommitAtHEAD(t *testing.T) {
	// Test that when HEAD itself is a merge commit, walkFirstParentCommits
	// correctly follows the first parent (feature branch history) and
	// doesn't walk into the second parent (main branch history).
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

	// Create initial commit on master
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	initialCommit, err := w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now().Add(-5 * time.Hour)},
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create feature branch
	featureBranch := plumbing.NewBranchReferenceName("feature/merge-at-head")
	if err := w.Checkout(&git.CheckoutOptions{
		Hash:   initialCommit,
		Branch: featureBranch,
		Create: true,
	}); err != nil {
		t.Fatalf("failed to create feature branch: %v", err)
	}

	// Create feature checkpoint commit
	cpID := id.MustCheckpointID("eee555fff666")
	if err := os.WriteFile(testFile, []byte("feature work"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	featureCommit, err := w.Commit(trailers.FormatCheckpoint("feat: feature work", cpID), &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now().Add(-3 * time.Hour)},
	})
	if err != nil {
		t.Fatalf("failed to create feature commit: %v", err)
	}

	// Switch to master and add a commit
	if err := w.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("master"),
	}); err != nil {
		t.Fatalf("failed to checkout master: %v", err)
	}
	mainFile := filepath.Join(tmpDir, "main.txt")
	if err := os.WriteFile(mainFile, []byte("main work"), 0o644); err != nil {
		t.Fatalf("failed to write main file: %v", err)
	}
	if _, err := w.Add("main.txt"); err != nil {
		t.Fatalf("failed to add main file: %v", err)
	}
	mainCommit, err := w.Commit("main: add work", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now().Add(-2 * time.Hour)},
	})
	if err != nil {
		t.Fatalf("failed to create main commit: %v", err)
	}

	// Switch back to feature and create merge commit AT HEAD
	if err := w.Checkout(&git.CheckoutOptions{
		Branch: featureBranch,
	}); err != nil {
		t.Fatalf("failed to checkout feature branch: %v", err)
	}

	featureCommitObj, commitObjErr := repo.CommitObject(featureCommit)
	if commitObjErr != nil {
		t.Fatalf("failed to get feature commit object: %v", commitObjErr)
	}
	featureTree, treeErr := featureCommitObj.Tree()
	if treeErr != nil {
		t.Fatalf("failed to get feature commit tree: %v", treeErr)
	}
	mergeHash := createMergeCommit(t, repo, featureCommit, mainCommit, featureTree.Hash, "Merge branch 'master' into feature/merge-at-head")

	// Update feature branch ref to merge commit (HEAD IS the merge)
	ref := plumbing.NewHashReference(featureBranch, mergeHash)
	if err := repo.Storer.SetReference(ref); err != nil {
		t.Fatalf("failed to update feature branch ref: %v", err)
	}

	// Create .entire directory
	if err := os.MkdirAll(".entire", 0o750); err != nil {
		t.Fatalf("failed to create .entire dir: %v", err)
	}

	// HEAD is the merge commit itself.
	// getAssociatedCommits should walk: merge -> featureCommit -> initial
	// and find the checkpoint on featureCommit.
	commits, err := getAssociatedCommits(repo, cpID, false)
	if err != nil {
		t.Fatalf("getAssociatedCommits error: %v", err)
	}
	if len(commits) != 1 {
		t.Fatalf("expected 1 associated commit when HEAD is merge commit, got %d", len(commits))
	}
	if !strings.Contains(commits[0].Message, "feat: feature work") {
		t.Errorf("expected feature commit message, got %q", commits[0].Message)
	}
}

func TestWalkFirstParentCommits_SkipsMergeParents(t *testing.T) {
	// Verify that walkFirstParentCommits follows only first parents and doesn't
	// enter the second parent (merge source) of merge commits.
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

	// Create initial commit (shared ancestor)
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	initialCommit, err := w.Commit("A: initial", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now().Add(-5 * time.Hour)},
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create feature branch with one commit
	featureBranch := plumbing.NewBranchReferenceName("feature/walk-test")
	if err := w.Checkout(&git.CheckoutOptions{
		Hash:   initialCommit,
		Branch: featureBranch,
		Create: true,
	}); err != nil {
		t.Fatalf("failed to create feature branch: %v", err)
	}
	if err := os.WriteFile(testFile, []byte("feature"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add test file: %v", err)
	}
	featureCommit, err := w.Commit("B: feature work", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now().Add(-4 * time.Hour)},
	})
	if err != nil {
		t.Fatalf("failed to create feature commit: %v", err)
	}

	// Create main branch commit (will be merge source)
	if err := w.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("master"),
	}); err != nil {
		t.Fatalf("failed to checkout master: %v", err)
	}
	mainFile := filepath.Join(tmpDir, "main.txt")
	if err := os.WriteFile(mainFile, []byte("main"), 0o644); err != nil {
		t.Fatalf("failed to write main file: %v", err)
	}
	if _, err := w.Add("main.txt"); err != nil {
		t.Fatalf("failed to add main file: %v", err)
	}
	mainCommit, err := w.Commit("C: main work", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now().Add(-3 * time.Hour)},
	})
	if err != nil {
		t.Fatalf("failed to create main commit: %v", err)
	}

	// Switch to feature and create merge commit
	if err := w.Checkout(&git.CheckoutOptions{
		Branch: featureBranch,
	}); err != nil {
		t.Fatalf("failed to checkout feature: %v", err)
	}
	featureCommitObj, commitObjErr := repo.CommitObject(featureCommit)
	if commitObjErr != nil {
		t.Fatalf("failed to get feature commit object: %v", commitObjErr)
	}
	featureTree, treeErr := featureCommitObj.Tree()
	if treeErr != nil {
		t.Fatalf("failed to get feature commit tree: %v", treeErr)
	}
	mergeHash := createMergeCommit(t, repo, featureCommit, mainCommit, featureTree.Hash, "M: merge main into feature")

	// Walk should visit: M (merge) -> B (feature) -> A (initial)
	// It should NOT visit C (main work), because that's the second parent of the merge.
	var visited []string
	err = walkFirstParentCommits(repo, mergeHash, 0, func(c *object.Commit) error {
		visited = append(visited, strings.Split(c.Message, "\n")[0])
		return nil
	})
	if err != nil {
		t.Fatalf("walkFirstParentCommits error: %v", err)
	}

	expected := []string{"M: merge main into feature", "B: feature work", "A: initial"}
	if len(visited) != len(expected) {
		t.Fatalf("expected %d commits visited, got %d: %v", len(expected), len(visited), visited)
	}
	for i, msg := range expected {
		if visited[i] != msg {
			t.Errorf("commit %d: expected %q, got %q", i, msg, visited[i])
		}
	}

	// Verify C was NOT visited
	for _, msg := range visited {
		if strings.Contains(msg, "C: main work") {
			t.Error("walkFirstParentCommits visited main branch commit (second parent of merge) - should only follow first parents")
		}
	}
}

func TestFormatCheckpointOutput_NoCommitsOnBranch(t *testing.T) {
	summary := &checkpoint.CheckpointSummary{
		CheckpointID: id.MustCheckpointID("abc123def456"),
		FilesTouched: []string{"main.go"},
	}
	content := &checkpoint.SessionContent{
		Metadata: checkpoint.CommittedMetadata{
			CheckpointID:              "abc123def456",
			SessionID:                 "2026-02-04-test-session",
			CreatedAt:                 time.Date(2026, 2, 4, 10, 30, 0, 0, time.UTC),
			FilesTouched:              []string{"main.go"},
			CheckpointTranscriptStart: 0,
		},
		Prompts:    "Add a new feature",
		Transcript: nil, // No transcript available
	}

	// No associated commits - use empty slice (not nil) to indicate "searched but found none"
	associatedCommits := []associatedCommit{}

	output := formatCheckpointOutput(summary, content, id.MustCheckpointID("abc123def456"), associatedCommits, checkpoint.Author{}, true, false)

	// Should show message indicating no commits found
	if !strings.Contains(output, "Commits: No commits found on this branch") {
		t.Errorf("expected 'Commits: No commits found on this branch' in output, got:\n%s", output)
	}
}

func TestGetAssociatedCommits_SearchAllFindsMergedBranchCommits(t *testing.T) {
	// Regression test: --search-all should find checkpoint commits that live on
	// a feature branch merged into main via a true merge commit. These commits
	// are on the second parent of the merge, so first-parent-only traversal
	// won't find them — but --search-all should use full DAG walk.
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

	checkpointID := id.MustCheckpointID("aabb11223344")

	// Create initial commit on main
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}
	mainBase, err := w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now().Add(-4 * time.Hour)},
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create a "feature branch" commit with checkpoint trailer (will become second parent)
	if err := os.WriteFile(testFile, []byte("feature work"), 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}
	featureMsg := trailers.FormatCheckpoint("feat: add feature", checkpointID)
	featureCommit, err := w.Commit(featureMsg, &git.CommitOptions{
		Author: &object.Signature{Name: "Feature Dev", Email: "dev@example.com", When: time.Now().Add(-3 * time.Hour)},
	})
	if err != nil {
		t.Fatalf("failed to create feature commit: %v", err)
	}

	// Move HEAD back to mainBase to simulate being on main
	// Create a new commit on "main" that diverges
	if err := os.WriteFile(testFile, []byte("main work"), 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}
	mainCommitObj, err := repo.CommitObject(mainBase)
	if err != nil {
		t.Fatalf("failed to get main base commit: %v", err)
	}
	mainTree, err := mainCommitObj.Tree()
	if err != nil {
		t.Fatalf("failed to get tree: %v", err)
	}

	// Create a second main commit (to diverge from feature)
	mainTip := createCommitWithTree(t, repo, mainTree.Hash, []plumbing.Hash{mainBase}, "main: parallel work")

	// Create merge commit: first parent = mainTip, second parent = featureCommit
	featureCommitObj, err := repo.CommitObject(featureCommit)
	if err != nil {
		t.Fatalf("failed to get feature commit: %v", err)
	}
	featureTree, err := featureCommitObj.Tree()
	if err != nil {
		t.Fatalf("failed to get feature tree: %v", err)
	}
	mergeHash := createMergeCommit(t, repo, mainTip, featureCommit, featureTree.Hash, "Merge feature into main")

	// Point HEAD at merge commit
	ref := plumbing.NewHashReference("refs/heads/main", mergeHash)
	if err := repo.Storer.SetReference(ref); err != nil {
		t.Fatalf("failed to set HEAD: %v", err)
	}
	headRef := plumbing.NewSymbolicReference("HEAD", "refs/heads/main")
	if err := repo.Storer.SetReference(headRef); err != nil {
		t.Fatalf("failed to set HEAD: %v", err)
	}

	// Without --search-all (first-parent only): should NOT find the feature commit
	// because it's on the second parent of the merge
	commits, err := getAssociatedCommits(repo, checkpointID, false)
	if err != nil {
		t.Fatalf("getAssociatedCommits error: %v", err)
	}
	if len(commits) != 0 {
		t.Errorf("expected 0 commits without --search-all (first-parent only), got %d", len(commits))
	}

	// With --search-all (full DAG walk): SHOULD find the feature commit
	commits, err = getAssociatedCommits(repo, checkpointID, true)
	if err != nil {
		t.Fatalf("getAssociatedCommits --search-all error: %v", err)
	}
	if len(commits) != 1 {
		t.Fatalf("expected 1 commit with --search-all, got %d", len(commits))
	}
	if commits[0].Author != "Feature Dev" {
		t.Errorf("expected author 'Feature Dev', got %q", commits[0].Author)
	}
}

func TestGetBranchCheckpoints_DefaultBranchFindsMergedCheckpoints(t *testing.T) {
	// Regression test: on the default branch, getBranchCheckpoints should find
	// checkpoint commits that came in via merge commits (second parents).
	// First-parent-only traversal would miss these.
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

	// Create initial commit on master (this is the default branch)
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}
	masterBase, err := w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now().Add(-4 * time.Hour)},
	})
	if err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create a feature branch commit with checkpoint trailer
	cpID := id.MustCheckpointID("fea112233344")
	if err := os.WriteFile(testFile, []byte("feature work"), 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}
	featureCommit, err := w.Commit(trailers.FormatCheckpoint("feat: add feature", cpID), &git.CommitOptions{
		Author: &object.Signature{Name: "Feature Dev", Email: "dev@example.com", When: time.Now().Add(-3 * time.Hour)},
	})
	if err != nil {
		t.Fatalf("failed to create feature commit: %v", err)
	}

	// Get tree hashes for creating commits via plumbing
	masterBaseObj, err := repo.CommitObject(masterBase)
	if err != nil {
		t.Fatalf("failed to get master base: %v", err)
	}
	masterTree, err := masterBaseObj.Tree()
	if err != nil {
		t.Fatalf("failed to get tree: %v", err)
	}
	featureObj, err := repo.CommitObject(featureCommit)
	if err != nil {
		t.Fatalf("failed to get feature commit: %v", err)
	}
	featureTree, err := featureObj.Tree()
	if err != nil {
		t.Fatalf("failed to get feature tree: %v", err)
	}

	// Create a second commit on master (diverge from feature)
	masterTip := createCommitWithTree(t, repo, masterTree.Hash, []plumbing.Hash{masterBase}, "main: parallel work")

	// Create merge commit on master: first parent = masterTip, second parent = featureCommit
	mergeHash := createMergeCommit(t, repo, masterTip, featureCommit, featureTree.Hash, "Merge feature into master")

	// Point master at merge commit
	ref := plumbing.NewHashReference("refs/heads/master", mergeHash)
	if err := repo.Storer.SetReference(ref); err != nil {
		t.Fatalf("failed to set ref: %v", err)
	}
	headRef := plumbing.NewSymbolicReference("HEAD", "refs/heads/master")
	if err := repo.Storer.SetReference(headRef); err != nil {
		t.Fatalf("failed to set HEAD: %v", err)
	}

	// Write committed checkpoint metadata so getBranchCheckpoints can find it
	store := checkpoint.NewGitStore(repo)
	if err := store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "test-session",
		Strategy:     "auto-commit",
		FilesTouched: []string{"test.txt"},
		Prompts:      []string{"add feature"},
	}); err != nil {
		t.Fatalf("failed to write committed checkpoint: %v", err)
	}

	// getBranchCheckpoints on master should find the checkpoint from the merged feature branch
	points, err := getBranchCheckpoints(repo, 100)
	if err != nil {
		t.Fatalf("getBranchCheckpoints error: %v", err)
	}

	// Should find at least the checkpoint from the merged feature branch
	var found bool
	for _, p := range points {
		if p.CheckpointID == cpID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected to find checkpoint %s from merged feature branch on default branch, got %d points: %v", cpID, len(points), points)
	}
}

// createCommitWithTree creates a commit with a specific tree and parent hashes.
func createCommitWithTree(t *testing.T, repo *git.Repository, treeHash plumbing.Hash, parents []plumbing.Hash, message string) plumbing.Hash {
	t.Helper()
	sig := object.Signature{
		Name:  "Test",
		Email: "test@example.com",
		When:  time.Now(),
	}
	commit := object.Commit{
		Author:       sig,
		Committer:    sig,
		Message:      message,
		TreeHash:     treeHash,
		ParentHashes: parents,
	}
	obj := repo.Storer.NewEncodedObject()
	if err := commit.Encode(obj); err != nil {
		t.Fatalf("failed to encode commit: %v", err)
	}
	hash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("failed to store commit: %v", err)
	}
	return hash
}
