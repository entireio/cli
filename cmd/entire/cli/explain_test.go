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

// toCheckpointsWithMeta converts a slice of RewindPoints to checkpointWithMeta
// with nil metadata. This is a test helper for backwards compatibility.
func toCheckpointsWithMeta(points []strategy.RewindPoint) []checkpointWithMeta {
	result := make([]checkpointWithMeta, len(points))
	for i, p := range points {
		result[i] = checkpointWithMeta{Point: p, Metadata: nil}
	}
	return result
}

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

func TestNewExplainCmd_NewFlags(t *testing.T) {
	cmd := newExplainCmd()

	// Verbose flag
	if cmd.Flags().Lookup("verbose") == nil {
		t.Error("expected --verbose flag to exist")
	}
	if cmd.Flags().ShorthandLookup("v") == nil {
		t.Error("expected -v shorthand to exist")
	}

	// Full flag
	if cmd.Flags().Lookup("full") == nil {
		t.Error("expected --full flag to exist")
	}

	// Generate flag
	if cmd.Flags().Lookup("generate") == nil {
		t.Error("expected --generate flag to exist")
	}

	// Limit flag
	if cmd.Flags().Lookup("limit") == nil {
		t.Error("expected --limit flag to exist")
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

// setupExplainTestRepo creates a git repo with an initial commit and .entire directory.
// It sets the working directory to the temp directory via t.Chdir.
func setupExplainTestRepo(t *testing.T) {
	t.Helper()
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
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test"), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if _, err := w.Add("test.txt"); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}
	_, err = w.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com"},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	if err := os.MkdirAll(".entire", 0o750); err != nil {
		t.Fatalf("failed to create .entire dir: %v", err)
	}
}

func TestExplainDefault_NoCurrentSession(t *testing.T) {
	setupExplainTestRepo(t)

	var stdout bytes.Buffer
	// With the new branch-centric view, no current session should show branch info instead of error
	err := runExplainDefault(&stdout, true, false, false, false, false, 0)

	// Should NOT error - just show branch-level info
	if err != nil {
		t.Fatalf("runExplainDefault() should not error with no session, got: %v", err)
	}

	// Should show branch info
	output := stdout.String()
	if !strings.Contains(output, "Branch:") {
		t.Errorf("expected output to contain 'Branch:', got:\n%s", output)
	}
}

func TestExplainBothFlagsError(t *testing.T) {
	// Test that providing both --session and --commit returns an error
	var stdout bytes.Buffer
	err := runExplain(&stdout, "session-id", "commit-sha", false, false, false, false, false, 0)

	if err == nil {
		t.Error("expected error when both flags provided, got nil")
	}
	// Case-insensitive check for "cannot specify both"
	errLower := strings.ToLower(err.Error())
	if !strings.Contains(errLower, "cannot specify both") {
		t.Errorf("expected 'cannot specify both' in error, got: %v", err)
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

func TestRunExplainDefault_ShowsBranchInfo(t *testing.T) {
	setupExplainTestRepo(t)

	var stdout bytes.Buffer
	// Call runExplainDefault with noPager=true to avoid pager issues in test
	err := runExplainDefault(&stdout, true, false, false, false, false, 0)

	// Should not error (may have no checkpoints, but should still show branch info)
	if err != nil {
		t.Fatalf("runExplainDefault() error = %v", err)
	}

	// For now, we're just checking the function runs and includes branch info
	output := stdout.String()
	if !strings.Contains(output, "Branch:") {
		t.Errorf("expected output to contain 'Branch:', got:\n%s", output)
	}
}

func TestRunExplainDefault_DefaultLimitOnMainBranch(t *testing.T) {
	setupExplainTestRepo(t)

	var stdout bytes.Buffer
	// Call with limit=0 (auto) - should apply default limit on main branch
	err := runExplainDefault(&stdout, true, false, false, false, false, 0)

	if err != nil {
		t.Fatalf("runExplainDefault() error = %v", err)
	}

	output := stdout.String()
	// On main/master branch with 0 checkpoints, shouldn't show limit message
	// (the limit message only appears when totalCount > limit)
	// Instead, verify the new format is shown
	if !strings.Contains(output, "Branch:") {
		t.Errorf("expected output to contain 'Branch:', got:\n%s", output)
	}
	if !strings.Contains(output, "Checkpoints: 0") {
		t.Errorf("expected output to contain 'Checkpoints: 0', got:\n%s", output)
	}
	// Should show intent/outcome placeholders in new format
	if !strings.Contains(output, "Intent:") {
		t.Errorf("expected output to contain 'Intent:', got:\n%s", output)
	}
}

func TestFormatBranchExplain_DefaultOutput(t *testing.T) {
	now := time.Now()
	points := []strategy.RewindPoint{
		{
			CheckpointID: "abc123def456",
			Date:         now,
			SessionID:    "2026-01-19-session1",
		},
	}

	output := formatBranchExplain("feature/test", toCheckpointsWithMeta(points), nil, false, false, false, 0, 1)

	// Branch header
	if !strings.Contains(output, "Branch: feature/test") {
		t.Errorf("expected 'Branch: feature/test', got:\n%s", output)
	}
	if !strings.Contains(output, "Checkpoints: 1") {
		t.Errorf("expected 'Checkpoints: 1', got:\n%s", output)
	}

	// Checkpoint listing
	if !strings.Contains(output, "[abc123def456]") {
		t.Errorf("expected checkpoint ID in output, got:\n%s", output)
	}

	// Placeholder text
	if !strings.Contains(output, "Intent:") {
		t.Errorf("expected 'Intent:', got:\n%s", output)
	}
}

func TestFormatBranchExplain_MultipleCheckpoints(t *testing.T) {
	now := time.Now()
	points := []strategy.RewindPoint{
		{
			CheckpointID: "abc123def456",
			Date:         now,
			SessionID:    "2026-01-19-session1",
		},
		{
			CheckpointID: "def456789012",
			Date:         now.Add(-time.Hour),
			SessionID:    "2026-01-19-session1",
		},
		{
			CheckpointID: "ghi789012345",
			Date:         now.Add(-2 * time.Hour),
			SessionID:    "2026-01-19-session1",
		},
	}

	output := formatBranchExplain("feature/test", toCheckpointsWithMeta(points), nil, false, false, false, 0, 3)

	// All checkpoints should be listed
	if !strings.Contains(output, "[abc123def456]") {
		t.Errorf("expected first checkpoint ID, got:\n%s", output)
	}
	if !strings.Contains(output, "[def456789012]") {
		t.Errorf("expected second checkpoint ID, got:\n%s", output)
	}
	if !strings.Contains(output, "[ghi789012345]") {
		t.Errorf("expected third checkpoint ID, got:\n%s", output)
	}

	// Should show checkpoints count
	if !strings.Contains(output, "Checkpoints: 3") {
		t.Errorf("expected 'Checkpoints: 3', got:\n%s", output)
	}
}

func TestFormatBranchExplain_WithVerbose(t *testing.T) {
	now := time.Now()
	checkpoints := []checkpointWithMeta{
		{
			Point: strategy.RewindPoint{
				CheckpointID:  "abc123def456",
				Date:          now,
				SessionID:     "2026-01-19-session1",
				SessionPrompt: "Add a logout button",
			},
			Metadata: &checkpoint.CommittedMetadata{
				Intent:       "Add logout button",
				FilesTouched: []string{"src/auth.go", "src/handler.go", "tests/auth_test.go"},
			},
		},
	}

	output := formatBranchExplain("feature/test", checkpoints, nil, true, false, false, 0, 1)

	// Verbose mode should show session ID in header line (in parentheses)
	if !strings.Contains(output, "(2026-01-19-session1)") {
		t.Errorf("expected session ID in header line, got:\n%s", output)
	}

	// Verbose mode should show prompt
	if !strings.Contains(output, "Prompt: Add a logout button") {
		t.Errorf("expected prompt in verbose output, got:\n%s", output)
	}

	// Verbose mode should show files count and first few files
	if !strings.Contains(output, "Files: 3") {
		t.Errorf("expected files count in verbose output, got:\n%s", output)
	}
	if !strings.Contains(output, "src/auth.go") {
		t.Errorf("expected file names in verbose output, got:\n%s", output)
	}
}

func TestFormatBranchExplain_WithVerbose_TruncatesLongPrompt(t *testing.T) {
	now := time.Now()
	longPrompt := strings.Repeat("a", 150)
	checkpoints := []checkpointWithMeta{
		{
			Point: strategy.RewindPoint{
				CheckpointID:  "abc123def456",
				Date:          now,
				SessionID:     "session1",
				SessionPrompt: longPrompt,
			},
		},
	}

	output := formatBranchExplain("feature/test", checkpoints, nil, true, false, false, 0, 1)

	// Long prompts should be truncated with ellipsis
	if !strings.Contains(output, "Prompt:") {
		t.Errorf("expected prompt in verbose output, got:\n%s", output)
	}
	if strings.Contains(output, longPrompt) {
		t.Errorf("prompt should be truncated, got full prompt in:\n%s", output)
	}
	if !strings.Contains(output, "...") {
		t.Errorf("truncated prompt should have ellipsis, got:\n%s", output)
	}
}

func TestFormatBranchExplain_WithVerbose_ManyFiles(t *testing.T) {
	now := time.Now()
	checkpoints := []checkpointWithMeta{
		{
			Point: strategy.RewindPoint{
				CheckpointID: "abc123def456",
				Date:         now,
				SessionID:    "session1",
			},
			Metadata: &checkpoint.CommittedMetadata{
				FilesTouched: []string{"a.go", "b.go", "c.go", "d.go", "e.go"},
			},
		},
	}

	output := formatBranchExplain("feature/test", checkpoints, nil, true, false, false, 0, 1)

	// Should show count and first 3 files with ellipsis
	if !strings.Contains(output, "Files: 5") {
		t.Errorf("expected files count in verbose output, got:\n%s", output)
	}
	if !strings.Contains(output, "a.go") || !strings.Contains(output, "b.go") || !strings.Contains(output, "c.go") {
		t.Errorf("expected first 3 files in verbose output, got:\n%s", output)
	}
	if !strings.Contains(output, "...)") {
		t.Errorf("expected ellipsis for many files, got:\n%s", output)
	}
}

func TestFormatBranchExplain_LimitedOnMain(t *testing.T) {
	now := time.Now()
	// Create 3 points but simulate showing only 2
	points := []strategy.RewindPoint{
		{
			CheckpointID: "abc123def456",
			Date:         now,
			SessionID:    "2026-01-19-session1",
		},
		{
			CheckpointID: "def456789012",
			Date:         now.Add(-time.Hour),
			SessionID:    "2026-01-19-session1",
		},
	}

	// isDefault=true, limit=2, totalCount=15 (more than limit)
	output := formatBranchExplain("main", toCheckpointsWithMeta(points), nil, false, false, true, 2, 15)

	// Header should show limited view info
	if !strings.Contains(output, "Checkpoints: 15 (showing last 2)") {
		t.Errorf("expected limited count in header, got:\n%s", output)
	}

	// Footer should show total with hint
	if !strings.Contains(output, "15 total checkpoints") {
		t.Errorf("expected total count in footer, got:\n%s", output)
	}
	if !strings.Contains(output, "--limit") {
		t.Errorf("expected --limit hint in footer, got:\n%s", output)
	}
}

func TestFormatBranchExplain_NoCheckpoints(t *testing.T) {
	output := formatBranchExplain("feature/empty", nil, nil, false, false, false, 0, 0)

	// Should show branch with 0 checkpoints
	if !strings.Contains(output, "Branch: feature/empty") {
		t.Errorf("expected branch name, got:\n%s", output)
	}
	if !strings.Contains(output, "Checkpoints: 0") {
		t.Errorf("expected 'Checkpoints: 0', got:\n%s", output)
	}

	// Should still show intent/outcome placeholders
	if !strings.Contains(output, "Intent:") {
		t.Errorf("expected 'Intent:', got:\n%s", output)
	}
}

func TestFormatBranchExplain_WithFull(t *testing.T) {
	now := time.Now()
	transcript := `{"type":"user","message":{"content":"Add logout"}}
{"type":"assistant","message":{"content":"Done!"}}`

	checkpoints := []checkpointWithMeta{
		{
			Point: strategy.RewindPoint{
				CheckpointID: "abc123def456",
				Date:         now,
				SessionID:    "session1",
			},
			Transcript: transcript,
		},
	}

	output := formatBranchExplain("feature/test", checkpoints, nil, false, true, false, 0, 1)

	// Full mode should show transcript markers
	if !strings.Contains(output, "--- Transcript ---") {
		t.Errorf("expected transcript start marker, got:\n%s", output)
	}
	if !strings.Contains(output, "--- End Transcript ---") {
		t.Errorf("expected transcript end marker, got:\n%s", output)
	}
	// Should contain transcript content
	if !strings.Contains(output, "Add logout") {
		t.Errorf("expected transcript content, got:\n%s", output)
	}
}

func TestFormatBranchExplain_WithFull_NoTranscript(t *testing.T) {
	now := time.Now()
	checkpoints := []checkpointWithMeta{
		{
			Point: strategy.RewindPoint{
				CheckpointID: "abc123def456",
				Date:         now,
				SessionID:    "session1",
			},
			Transcript: "", // Empty transcript
		},
	}

	output := formatBranchExplain("feature/test", checkpoints, nil, false, true, false, 0, 1)

	// Should NOT show transcript markers when transcript is empty
	if strings.Contains(output, "--- Transcript ---") {
		t.Errorf("should not show transcript markers when empty, got:\n%s", output)
	}
}

func TestFormatBranchExplain_TruncatesLongCheckpointID(t *testing.T) {
	now := time.Now()
	points := []strategy.RewindPoint{
		{
			CheckpointID: "abc123def456789012345", // Longer than 12 chars
			Date:         now,
			SessionID:    "2026-01-19-session1",
		},
	}

	output := formatBranchExplain("feature/test", toCheckpointsWithMeta(points), nil, false, false, false, 0, 1)

	// Should truncate to 12 chars
	if !strings.Contains(output, "[abc123def456]") {
		t.Errorf("expected truncated checkpoint ID (12 chars), got:\n%s", output)
	}
	// Should NOT contain the full ID
	if strings.Contains(output, "[abc123def456789012345]") {
		t.Errorf("expected checkpoint ID to be truncated, got:\n%s", output)
	}
}

func TestFormatBranchExplain_WithMetadata(t *testing.T) {
	now := time.Now()
	checkpoints := []checkpointWithMeta{
		{
			Point: strategy.RewindPoint{
				CheckpointID: "abc123def456",
				Date:         now,
				SessionID:    "2026-01-19-session1",
			},
			Metadata: &checkpoint.CommittedMetadata{
				Intent:  "Add user authentication",
				Outcome: "Implemented JWT-based auth",
			},
		},
	}

	output := formatBranchExplain("feature/test", checkpoints, nil, false, false, false, 0, 1)

	// Should display real intent from metadata (now on separate line)
	if !strings.Contains(output, "Intent:") || !strings.Contains(output, "Add user authentication") {
		t.Errorf("expected real intent, got:\n%s", output)
	}
	// Should display real outcome from metadata (now on separate line)
	if !strings.Contains(output, "Outcome:") || !strings.Contains(output, "Implemented JWT-based auth") {
		t.Errorf("expected real outcome, got:\n%s", output)
	}
	// Should NOT show placeholder text for intent/outcome
	if strings.Contains(output, "(not generated)") {
		t.Errorf("expected no placeholder text when metadata is present, got:\n%s", output)
	}
}

func TestFormatBranchExplain_PartialMetadata(t *testing.T) {
	// Test with only Intent populated, Outcome should show placeholder
	now := time.Now()
	checkpoints := []checkpointWithMeta{
		{
			Point: strategy.RewindPoint{
				CheckpointID: "abc123def456",
				Date:         now,
				SessionID:    "2026-01-19-session1",
			},
			Metadata: &checkpoint.CommittedMetadata{
				Intent:  "Fix login bug",
				Outcome: "", // Empty - should show placeholder
			},
		},
	}

	output := formatBranchExplain("feature/test", checkpoints, nil, false, false, false, 0, 1)

	// Should display real intent (now on separate line)
	if !strings.Contains(output, "Intent:") || !strings.Contains(output, "Fix login bug") {
		t.Errorf("expected real intent, got:\n%s", output)
	}
	// Should show placeholder for empty outcome (now on separate line)
	if !strings.Contains(output, "Outcome:") || !strings.Contains(output, "(not generated)") {
		t.Errorf("expected placeholder for empty outcome, got:\n%s", output)
	}
}

func TestFormatBranchExplain_NilMetadata(t *testing.T) {
	// Test with nil metadata - should show placeholders
	now := time.Now()
	checkpoints := []checkpointWithMeta{
		{
			Point: strategy.RewindPoint{
				CheckpointID: "abc123def456",
				Date:         now,
				SessionID:    "2026-01-19-session1",
			},
			Metadata: nil, // No metadata loaded
		},
	}

	output := formatBranchExplain("feature/test", checkpoints, nil, false, false, false, 0, 1)

	// Should show placeholders for both (now on separate lines)
	if !strings.Contains(output, "Intent:") || !strings.Contains(output, "(not generated)") {
		t.Errorf("expected placeholder for intent with nil metadata, got:\n%s", output)
	}
	// Count occurrences of "(not generated)" - should appear for both intent and outcome
	notGenCount := strings.Count(output, "(not generated)")
	if notGenCount < 2 {
		t.Errorf("expected placeholder for both intent and outcome, got only %d placeholders:\n%s", notGenCount, output)
	}
}

func TestFormatBranchExplain_MixedMetadata(t *testing.T) {
	// Test with multiple checkpoints - some with metadata, some without
	now := time.Now()
	checkpoints := []checkpointWithMeta{
		{
			Point: strategy.RewindPoint{
				CheckpointID: "abc123def456",
				Date:         now,
				SessionID:    "2026-01-19-session1",
			},
			Metadata: &checkpoint.CommittedMetadata{
				Intent:  "First checkpoint intent",
				Outcome: "First checkpoint outcome",
			},
		},
		{
			Point: strategy.RewindPoint{
				CheckpointID: "def456789012",
				Date:         now.Add(-time.Hour),
				SessionID:    "2026-01-19-session1",
			},
			Metadata: nil, // No metadata for this one
		},
	}

	output := formatBranchExplain("feature/test", checkpoints, nil, false, false, false, 0, 2)

	// First checkpoint should have real values (now on separate lines)
	if !strings.Contains(output, "First checkpoint intent") {
		t.Errorf("expected first checkpoint intent, got:\n%s", output)
	}
	if !strings.Contains(output, "First checkpoint outcome") {
		t.Errorf("expected first checkpoint outcome, got:\n%s", output)
	}

	// The output should contain at least one placeholder (from the second checkpoint)
	// Count occurrences of "(not generated)" - should be 2 (intent + outcome for second checkpoint)
	placeholderCount := strings.Count(output, "(not generated)")
	if placeholderCount != 2 {
		t.Errorf("expected 2 placeholders for second checkpoint, got %d in:\n%s", placeholderCount, output)
	}
}

func TestFormatBranchExplain_WithGeneratedSummary(t *testing.T) {
	// Test that GeneratedSummary is used when Metadata has no intent/outcome
	now := time.Now()
	checkpoints := []checkpointWithMeta{
		{
			Point: strategy.RewindPoint{
				CheckpointID: "abc123def456",
				Date:         now,
				SessionID:    "2026-01-19-session1",
			},
			Metadata: nil, // No stored metadata
			GeneratedSummary: &Summary{
				Intent:  "Generated intent from transcript",
				Outcome: "Generated outcome from transcript",
			},
		},
	}

	output := formatBranchExplain("feature/test", checkpoints, nil, false, false, false, 0, 1)

	// Should display generated intent (now on separate line)
	if !strings.Contains(output, "Generated intent from transcript") {
		t.Errorf("expected generated intent, got:\n%s", output)
	}
	// Should display generated outcome (now on separate line)
	if !strings.Contains(output, "Generated outcome from transcript") {
		t.Errorf("expected generated outcome, got:\n%s", output)
	}
	// Should NOT show placeholder text
	if strings.Contains(output, "(not generated)") {
		t.Errorf("expected no placeholder text when generated summary is present, got:\n%s", output)
	}
}

func TestFormatBranchExplain_MetadataTakesPrecedenceOverGenerated(t *testing.T) {
	// Test that stored Metadata takes precedence over GeneratedSummary
	now := time.Now()
	checkpoints := []checkpointWithMeta{
		{
			Point: strategy.RewindPoint{
				CheckpointID: "abc123def456",
				Date:         now,
				SessionID:    "2026-01-19-session1",
			},
			Metadata: &checkpoint.CommittedMetadata{
				Intent:  "Stored intent",
				Outcome: "Stored outcome",
			},
			GeneratedSummary: &Summary{
				Intent:  "Generated intent - should not be used",
				Outcome: "Generated outcome - should not be used",
			},
		},
	}

	output := formatBranchExplain("feature/test", checkpoints, nil, false, false, false, 0, 1)

	// Should display stored intent, not generated (now on separate line)
	if !strings.Contains(output, "Stored intent") {
		t.Errorf("expected stored intent, got:\n%s", output)
	}
	if strings.Contains(output, "Generated intent") {
		t.Errorf("should not show generated intent when stored exists, got:\n%s", output)
	}
	// Should display stored outcome, not generated (now on separate line)
	if !strings.Contains(output, "Stored outcome") {
		t.Errorf("expected stored outcome, got:\n%s", output)
	}
	if strings.Contains(output, "Generated outcome") {
		t.Errorf("should not show generated outcome when stored exists, got:\n%s", output)
	}
}
