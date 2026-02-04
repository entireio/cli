package cli

import (
	"os"
	"path/filepath"
	"testing"

	"entire.io/cli/cmd/entire/cli/paths"
)

func TestPreTaskStateFile(t *testing.T) {
	toolUseID := "toolu_abc123"
	// preTaskStateFile returns an absolute path within the repo
	// Verify it ends with the expected relative path suffix
	expectedSuffix := filepath.Join(paths.EntireTmpDir, "pre-task-toolu_abc123.json")
	got := preTaskStateFile(toolUseID)
	if !filepath.IsAbs(got) {
		// If we're not in a git repo, it falls back to relative paths
		if got != expectedSuffix {
			t.Errorf("preTaskStateFile() = %v, want %v", got, expectedSuffix)
		}
	} else {
		// When in a git repo, the path should end with the expected suffix
		if !hasSuffix(got, expectedSuffix) {
			t.Errorf("preTaskStateFile() = %v, should end with %v", got, expectedSuffix)
		}
	}
}

// hasSuffix checks if path ends with suffix, handling path separators correctly
func hasSuffix(path, suffix string) bool {
	return len(path) >= len(suffix) && path[len(path)-len(suffix):] == suffix
}

func TestPreTaskState_CaptureLoadCleanup(t *testing.T) {
	// Create a temporary directory for testing
	tmpDir := t.TempDir()

	// Save current directory and restore after test
	// Create a test git repo
	testRepoDir := filepath.Join(tmpDir, "testrepo")
	if err := os.MkdirAll(testRepoDir, 0o755); err != nil {
		t.Fatalf("Failed to create test repo dir: %v", err)
	}

	// Initialize git repo (using git command since we need a real repo)
	t.Chdir(testRepoDir)

	// Create .entire/tmp directory
	if err := os.MkdirAll(paths.EntireTmpDir, 0o755); err != nil {
		t.Fatalf("Failed to create tmp dir: %v", err)
	}

	// Initialize git repo manually (need at least .git directory)
	if err := os.MkdirAll(".git/objects", 0o755); err != nil {
		t.Fatalf("Failed to create .git: %v", err)
	}
	if err := os.WriteFile(".git/HEAD", []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("Failed to create HEAD: %v", err)
	}

	toolUseID := "toolu_test123"

	// Test load when state doesn't exist
	state, err := LoadPreTaskState(toolUseID)
	if err != nil {
		t.Errorf("LoadPreTaskState() error = %v, want nil", err)
	}
	if state != nil {
		t.Error("LoadPreTaskState() should return nil for non-existent state")
	}

	// Create a state file manually to test load
	stateFile := preTaskStateFile(toolUseID)
	stateContent := `{
		"tool_use_id": "toolu_test123",
		"timestamp": "2025-01-01T00:00:00Z",
		"untracked_files": ["file1.txt", "file2.txt"]
	}`
	if err := os.WriteFile(stateFile, []byte(stateContent), 0o644); err != nil {
		t.Fatalf("Failed to create state file: %v", err)
	}

	// Test load
	state, err = LoadPreTaskState(toolUseID)
	if err != nil {
		t.Errorf("LoadPreTaskState() error = %v", err)
	}
	if state == nil {
		t.Fatal("LoadPreTaskState() returned nil")
	}
	if state.ToolUseID != toolUseID {
		t.Errorf("ToolUseID = %v, want %v", state.ToolUseID, toolUseID)
	}
	if len(state.UntrackedFiles) != 2 {
		t.Errorf("UntrackedFiles count = %d, want 2", len(state.UntrackedFiles))
	}

	// Test cleanup
	err = CleanupPreTaskState(toolUseID)
	if err != nil {
		t.Errorf("CleanupPreTaskState() error = %v", err)
	}

	// Verify file was removed
	if _, err := os.Stat(stateFile); !os.IsNotExist(err) {
		t.Error("State file should be removed after cleanup")
	}
}

func TestComputeNewFilesFromTask(t *testing.T) {
	preState := &PreTaskState{
		ToolUseID:      "toolu_test",
		UntrackedFiles: []string{"existing1.txt", "existing2.txt"},
	}

	// Test with pre-state
	currentFiles := []string{"existing1.txt", "newfile.txt", "existing2.txt", "anotherNew.txt"}
	newFiles := computeNewFilesFromTaskState(preState, currentFiles)

	if len(newFiles) != 2 {
		t.Errorf("computeNewFilesFromTaskState() returned %d files, want 2", len(newFiles))
	}

	// Check that the new files are correct
	expectedNew := map[string]bool{"newfile.txt": true, "anotherNew.txt": true}
	for _, f := range newFiles {
		if !expectedNew[f] {
			t.Errorf("Unexpected new file: %s", f)
		}
	}

	// Test with nil pre-state
	newFiles = computeNewFilesFromTaskState(nil, currentFiles)
	if newFiles != nil {
		t.Errorf("computeNewFilesFromTaskState(nil) should return nil, got %v", newFiles)
	}
}

func TestFilterAndNormalizePaths_SiblingDirectories(t *testing.T) {
	// This test verifies the fix for the bug where files in sibling directories
	// were filtered out when Claude runs from a subdirectory.
	// When Claude is in /repo/frontend and edits /repo/api/file.ts,
	// the relative path would be ../api/file.ts which was incorrectly filtered.
	// The fix uses repo root instead of cwd, so paths should be api/file.ts.

	tests := []struct {
		name     string
		files    []string
		basePath string // simulates repo root or cwd
		want     []string
	}{
		{
			name: "files in sibling directories with repo root base",
			files: []string{
				"/repo/api/src/lib/github.ts",
				"/repo/api/src/types.ts",
				"/repo/frontend/src/pages/api.ts",
			},
			basePath: "/repo", // repo root
			want: []string{
				"api/src/lib/github.ts",
				"api/src/types.ts",
				"frontend/src/pages/api.ts",
			},
		},
		{
			name: "files in sibling directories with subdirectory base (old buggy behavior)",
			files: []string{
				"/repo/api/src/lib/github.ts",
				"/repo/frontend/src/pages/api.ts",
			},
			basePath: "/repo/frontend", // cwd in subdirectory
			want: []string{
				// Only frontend file should remain, api file gets filtered
				// because ../api/... starts with ..
				"src/pages/api.ts",
			},
		},
		{
			name: "relative paths pass through unchanged",
			files: []string{
				"src/file.ts",
				"lib/util.go",
			},
			basePath: "/repo",
			want: []string{
				"src/file.ts",
				"lib/util.go",
			},
		},
		{
			name: "infrastructure paths are filtered",
			files: []string{
				"/repo/src/file.ts",
				"/repo/.entire/metadata/session.json",
			},
			basePath: "/repo",
			want: []string{
				"src/file.ts",
				// .entire path should be filtered
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FilterAndNormalizePaths(tt.files, tt.basePath)
			if len(got) != len(tt.want) {
				t.Errorf("FilterAndNormalizePaths() returned %d files, want %d\ngot: %v\nwant: %v",
					len(got), len(tt.want), got, tt.want)
				return
			}
			for i, want := range tt.want {
				if got[i] != want {
					t.Errorf("FilterAndNormalizePaths()[%d] = %v, want %v", i, got[i], want)
				}
			}
		})
	}
}

func TestFindActivePreTaskFile(t *testing.T) {
	// Create a temporary directory for testing and change to it
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize a git repo so that AbsPath can find the repo root
	if err := os.MkdirAll(".git/objects", 0o755); err != nil {
		t.Fatalf("Failed to create .git: %v", err)
	}
	if err := os.WriteFile(".git/HEAD", []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("Failed to create HEAD: %v", err)
	}

	// Clear the repo root cache to pick up the new repo
	paths.ClearRepoRootCache()

	// Create .entire/tmp directory
	if err := os.MkdirAll(paths.EntireTmpDir, 0o755); err != nil {
		t.Fatalf("Failed to create tmp dir: %v", err)
	}

	// Test with no pre-task files
	taskID, found := FindActivePreTaskFile()
	if found {
		t.Error("FindActivePreTaskFile() should return false when no pre-task files exist")
	}
	if taskID != "" {
		t.Errorf("FindActivePreTaskFile() taskID = %v, want empty", taskID)
	}

	// Create a pre-task file
	preTaskFile := filepath.Join(paths.EntireTmpDir, "pre-task-toolu_abc123.json")
	if err := os.WriteFile(preTaskFile, []byte(`{"tool_use_id": "toolu_abc123"}`), 0o644); err != nil {
		t.Fatalf("Failed to create pre-task file: %v", err)
	}

	// Test with one pre-task file
	taskID, found = FindActivePreTaskFile()
	if !found {
		t.Error("FindActivePreTaskFile() should return true when pre-task file exists")
	}
	if taskID != "toolu_abc123" {
		t.Errorf("FindActivePreTaskFile() taskID = %v, want toolu_abc123", taskID)
	}
}

// setupTestRepoWithTranscript sets up a temporary git repo with a transcript file
// and returns the transcriptPath. Used by PrePromptState transcript tests.
func setupTestRepoWithTranscript(t *testing.T, transcriptContent string, transcriptName string) (transcriptPath string) {
	t.Helper()

	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo
	if err := os.MkdirAll(".git/objects", 0o755); err != nil {
		t.Fatalf("Failed to create .git: %v", err)
	}
	if err := os.WriteFile(".git/HEAD", []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("Failed to create HEAD: %v", err)
	}

	// Clear the repo root cache to pick up the new repo
	paths.ClearRepoRootCache()

	// Create .entire/tmp directory
	if err := os.MkdirAll(paths.EntireTmpDir, 0o755); err != nil {
		t.Fatalf("Failed to create tmp dir: %v", err)
	}

	// Create transcript file if content provided
	if transcriptContent != "" {
		transcriptPath = filepath.Join(tmpDir, transcriptName)
		if err := os.WriteFile(transcriptPath, []byte(transcriptContent), 0o644); err != nil {
			t.Fatalf("Failed to create transcript file: %v", err)
		}
	}

	return transcriptPath
}

func TestPrePromptState_WithTranscriptPosition(t *testing.T) {
	const expectedUUID = "user-2"
	transcriptContent := `{"type":"user","uuid":"user-1","message":{"content":"Hello"}}
{"type":"assistant","uuid":"asst-1","message":{"content":[{"type":"text","text":"Hi"}]}}
{"type":"user","uuid":"` + expectedUUID + `","message":{"content":"How are you?"}}`

	transcriptPath := setupTestRepoWithTranscript(t, transcriptContent, "transcript.jsonl")

	sessionID := "test-session-123"

	// Capture state with transcript path
	if err := CapturePrePromptState(sessionID, transcriptPath); err != nil {
		t.Fatalf("CapturePrePromptState() error = %v", err)
	}

	// Load and verify
	state, err := LoadPrePromptState(sessionID)
	if err != nil {
		t.Fatalf("LoadPrePromptState() error = %v", err)
	}
	if state == nil {
		t.Fatal("LoadPrePromptState() returned nil")
		return // unreachable but satisfies staticcheck
	}

	// Verify transcript position was captured
	if state.LastTranscriptIdentifier != expectedUUID {
		t.Errorf("LastTranscriptIdentifier = %q, want %q", state.LastTranscriptIdentifier, expectedUUID)
	}
	if state.LastTranscriptLineCount != 3 {
		t.Errorf("LastTranscriptLineCount = %d, want 3", state.LastTranscriptLineCount)
	}

	// Cleanup
	if err := CleanupPrePromptState(sessionID); err != nil {
		t.Errorf("CleanupPrePromptState() error = %v", err)
	}
}

func TestPrePromptState_WithEmptyTranscriptPath(t *testing.T) {
	setupTestRepoWithTranscript(t, "", "") // No transcript file

	sessionID := "test-session-empty-transcript"

	// Capture state with empty transcript path
	if err := CapturePrePromptState(sessionID, ""); err != nil {
		t.Fatalf("CapturePrePromptState() error = %v", err)
	}

	// Load and verify
	state, err := LoadPrePromptState(sessionID)
	if err != nil {
		t.Fatalf("LoadPrePromptState() error = %v", err)
	}
	if state == nil {
		t.Fatal("LoadPrePromptState() returned nil")
		return // unreachable but satisfies staticcheck
	}

	// Transcript position should be empty/zero when no transcript provided
	if state.LastTranscriptIdentifier != "" {
		t.Errorf("LastTranscriptIdentifier = %q, want empty", state.LastTranscriptIdentifier)
	}
	if state.LastTranscriptLineCount != 0 {
		t.Errorf("LastTranscriptLineCount = %d, want 0", state.LastTranscriptLineCount)
	}

	// Cleanup
	if err := CleanupPrePromptState(sessionID); err != nil {
		t.Errorf("CleanupPrePromptState() error = %v", err)
	}
}

func TestPrePromptState_WithSummaryOnlyTranscript(t *testing.T) {
	// Summary rows have leafUuid but not uuid
	transcriptContent := `{"type":"summary","leafUuid":"leaf-1","summary":"Previous context"}
{"type":"summary","leafUuid":"leaf-2","summary":"More context"}`

	transcriptPath := setupTestRepoWithTranscript(t, transcriptContent, "transcript-summary.jsonl")

	sessionID := "test-session-summary-only"

	// Capture state
	if err := CapturePrePromptState(sessionID, transcriptPath); err != nil {
		t.Fatalf("CapturePrePromptState() error = %v", err)
	}

	// Load and verify
	state, err := LoadPrePromptState(sessionID)
	if err != nil {
		t.Fatalf("LoadPrePromptState() error = %v", err)
	}
	if state == nil {
		t.Fatal("LoadPrePromptState() returned nil")
	}

	// Line count should be 2, but UUID should be empty (summary rows don't have uuid)
	if state.LastTranscriptLineCount != 2 {
		t.Errorf("LastTranscriptLineCount = %d, want 2", state.LastTranscriptLineCount)
	}
	if state.LastTranscriptIdentifier != "" {
		t.Errorf("LastTranscriptIdentifier = %q, want empty (summary rows don't have uuid)", state.LastTranscriptIdentifier)
	}

	// Cleanup
	if err := CleanupPrePromptState(sessionID); err != nil {
		t.Errorf("CleanupPrePromptState() error = %v", err)
	}
}

func TestComputeFileChanges_SingleGitStatus(t *testing.T) {
	// This test verifies that ComputeFileChanges returns both new and deleted files
	// from a single git status call (consolidation of ComputeNewFiles + ComputeDeletedFiles)

	// For now, just verify the function signature exists and returns expected types
	// The actual git operations are tested via integration tests

	// Call with nil preState should return empty slices, not error
	newFiles, deletedFiles, err := ComputeFileChanges(nil)
	if err != nil {
		t.Errorf("ComputeFileChanges(nil) error = %v, want nil", err)
	}
	if newFiles != nil {
		t.Errorf("ComputeFileChanges(nil) newFiles = %v, want nil", newFiles)
	}
	if deletedFiles != nil {
		t.Errorf("ComputeFileChanges(nil) deletedFiles = %v, want nil", deletedFiles)
	}
}
