package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
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

func TestPrePromptState_BackwardCompat_LastTranscriptLineCount(t *testing.T) {
	// Verify that state files written by older CLI versions with "last_transcript_line_count"
	// are correctly migrated to StepTranscriptStart on load.
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo
	if err := os.MkdirAll(".git/objects", 0o755); err != nil {
		t.Fatalf("Failed to create .git: %v", err)
	}
	if err := os.WriteFile(".git/HEAD", []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("Failed to create HEAD: %v", err)
	}
	paths.ClearRepoRootCache()
	if err := os.MkdirAll(paths.EntireTmpDir, 0o755); err != nil {
		t.Fatalf("Failed to create tmp dir: %v", err)
	}

	sessionID := "test-backward-compat"
	stateFile := prePromptStateFile(sessionID)

	// Write a state file using the OLD JSON tag (last_transcript_line_count)
	oldFormatJSON := `{
		"session_id": "test-backward-compat",
		"timestamp": "2026-01-01T00:00:00Z",
		"untracked_files": [],
		"last_transcript_identifier": "user-5",
		"last_transcript_line_count": 42
	}`
	if err := os.WriteFile(stateFile, []byte(oldFormatJSON), 0o644); err != nil {
		t.Fatalf("Failed to write old-format state file: %v", err)
	}

	// Load should migrate the deprecated field
	state, err := LoadPrePromptState(sessionID)
	if err != nil {
		t.Fatalf("LoadPrePromptState() error = %v", err)
	}
	if state == nil {
		t.Fatal("LoadPrePromptState() returned nil")
	}

	if state.StepTranscriptStart != 42 {
		t.Errorf("StepTranscriptStart = %d, want 42 (migrated from last_transcript_line_count)", state.StepTranscriptStart)
	}
	if state.LastTranscriptLineCount != 0 {
		t.Errorf("LastTranscriptLineCount = %d, want 0 (should be cleared after migration)", state.LastTranscriptLineCount)
	}
	if state.LastTranscriptIdentifier != "user-5" {
		t.Errorf("LastTranscriptIdentifier = %q, want %q", state.LastTranscriptIdentifier, "user-5")
	}

	// Also test: new format takes precedence over old
	bothFieldsJSON := `{
		"session_id": "test-backward-compat",
		"timestamp": "2026-01-01T00:00:00Z",
		"untracked_files": [],
		"step_transcript_start": 100,
		"last_transcript_line_count": 42
	}`
	if err := os.WriteFile(stateFile, []byte(bothFieldsJSON), 0o644); err != nil {
		t.Fatalf("Failed to write both-fields state file: %v", err)
	}

	state, err = LoadPrePromptState(sessionID)
	if err != nil {
		t.Fatalf("LoadPrePromptState() error = %v", err)
	}
	if state.StepTranscriptStart != 100 {
		t.Errorf("StepTranscriptStart = %d, want 100 (new field should take precedence)", state.StepTranscriptStart)
	}

	// Cleanup
	if err := CleanupPrePromptState(sessionID); err != nil {
		t.Errorf("CleanupPrePromptState() error = %v", err)
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
	if state.StepTranscriptStart != 3 {
		t.Errorf("StepTranscriptStart = %d, want 3", state.StepTranscriptStart)
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
	if state.StepTranscriptStart != 0 {
		t.Errorf("StepTranscriptStart = %d, want 0", state.StepTranscriptStart)
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
	if state.StepTranscriptStart != 2 {
		t.Errorf("StepTranscriptStart = %d, want 2", state.StepTranscriptStart)
	}
	if state.LastTranscriptIdentifier != "" {
		t.Errorf("LastTranscriptIdentifier = %q, want empty (summary rows don't have uuid)", state.LastTranscriptIdentifier)
	}

	// Cleanup
	if err := CleanupPrePromptState(sessionID); err != nil {
		t.Errorf("CleanupPrePromptState() error = %v", err)
	}
}

func TestDetectFileChanges_DeletedFilesWithNilPreState(t *testing.T) {
	// This test verifies that DetectFileChanges detects deleted files
	// even when previouslyUntracked is nil. Deleted file detection
	// doesn't depend on pre-prompt state.

	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo with go-git
	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("failed to init repo: %v", err)
	}

	// Create and commit a tracked file
	trackedFile := filepath.Join(tmpDir, "tracked.txt")
	if err := os.WriteFile(trackedFile, []byte("tracked content"), 0o644); err != nil {
		t.Fatalf("failed to write tracked file: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	if _, err := worktree.Add("tracked.txt"); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}

	if _, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
		},
	}); err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	// Delete the tracked file (simulating user deletion during session)
	if err := os.Remove(trackedFile); err != nil {
		t.Fatalf("failed to delete tracked file: %v", err)
	}

	// Call DetectFileChanges with nil previouslyUntracked
	changes, err := DetectFileChanges(nil)
	if err != nil {
		t.Fatalf("DetectFileChanges(nil) error = %v", err)
	}

	// New should be nil when there are no untracked files
	if len(changes.New) != 0 {
		t.Errorf("DetectFileChanges(nil) New = %v, want empty", changes.New)
	}

	// Deleted should contain the deleted tracked file
	if len(changes.Deleted) != 1 {
		t.Errorf("DetectFileChanges(nil) Deleted = %v, want [tracked.txt]", changes.Deleted)
	} else if changes.Deleted[0] != "tracked.txt" {
		t.Errorf("DetectFileChanges(nil) Deleted[0] = %v, want tracked.txt", changes.Deleted[0])
	}
}

func TestDetectFileChanges_NewAndDeletedFiles(t *testing.T) {
	// This test verifies that DetectFileChanges correctly identifies both
	// new files (untracked files not in previouslyUntracked) and deleted files
	// (tracked files that were deleted).

	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo with go-git
	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("failed to init repo: %v", err)
	}

	// Create and commit tracked files
	trackedFile1 := filepath.Join(tmpDir, "tracked1.txt")
	trackedFile2 := filepath.Join(tmpDir, "tracked2.txt")
	if err := os.WriteFile(trackedFile1, []byte("content1"), 0o644); err != nil {
		t.Fatalf("failed to write tracked1: %v", err)
	}
	if err := os.WriteFile(trackedFile2, []byte("content2"), 0o644); err != nil {
		t.Fatalf("failed to write tracked2: %v", err)
	}

	// Also create a pre-existing untracked file (simulating file that existed before session)
	preExistingUntracked := filepath.Join(tmpDir, "pre-existing-untracked.txt")
	if err := os.WriteFile(preExistingUntracked, []byte("pre-existing"), 0o644); err != nil {
		t.Fatalf("failed to write pre-existing untracked: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	if _, err := worktree.Add("tracked1.txt"); err != nil {
		t.Fatalf("failed to add tracked1: %v", err)
	}
	if _, err := worktree.Add("tracked2.txt"); err != nil {
		t.Fatalf("failed to add tracked2: %v", err)
	}

	if _, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
		},
	}); err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	// Simulate session: delete tracked1.txt and create a new file
	if err := os.Remove(trackedFile1); err != nil {
		t.Fatalf("failed to delete tracked1: %v", err)
	}

	newFile := filepath.Join(tmpDir, "new-file.txt")
	if err := os.WriteFile(newFile, []byte("new content"), 0o644); err != nil {
		t.Fatalf("failed to write new file: %v", err)
	}

	// Call DetectFileChanges with pre-existing untracked files
	changes, err := DetectFileChanges([]string{"pre-existing-untracked.txt"})
	if err != nil {
		t.Fatalf("DetectFileChanges() error = %v", err)
	}

	// New should contain only new-file.txt (not pre-existing-untracked.txt)
	if len(changes.New) != 1 {
		t.Errorf("DetectFileChanges() New = %v, want [new-file.txt]", changes.New)
	} else if changes.New[0] != "new-file.txt" {
		t.Errorf("DetectFileChanges() New[0] = %v, want new-file.txt", changes.New[0])
	}

	// Deleted should contain tracked1.txt
	if len(changes.Deleted) != 1 {
		t.Errorf("DetectFileChanges() Deleted = %v, want [tracked1.txt]", changes.Deleted)
	} else if changes.Deleted[0] != "tracked1.txt" {
		t.Errorf("DetectFileChanges() Deleted[0] = %v, want tracked1.txt", changes.Deleted[0])
	}
}

func TestDetectFileChanges_NoChanges(t *testing.T) {
	// This test verifies DetectFileChanges returns empty slices
	// when there are no new, modified, or deleted files.

	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo with go-git
	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("failed to init repo: %v", err)
	}

	// Create and commit a tracked file
	trackedFile := filepath.Join(tmpDir, "tracked.txt")
	if err := os.WriteFile(trackedFile, []byte("content"), 0o644); err != nil {
		t.Fatalf("failed to write tracked file: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	if _, err := worktree.Add("tracked.txt"); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}

	if _, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
		},
	}); err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	// Call DetectFileChanges with empty previouslyUntracked - no changes should be detected
	changes, err := DetectFileChanges([]string{})
	if err != nil {
		t.Fatalf("DetectFileChanges() error = %v", err)
	}

	if len(changes.Modified) != 0 {
		t.Errorf("DetectFileChanges() Modified = %v, want empty", changes.Modified)
	}

	if len(changes.New) != 0 {
		t.Errorf("DetectFileChanges() New = %v, want empty", changes.New)
	}

	if len(changes.Deleted) != 0 {
		t.Errorf("DetectFileChanges() Deleted = %v, want empty", changes.Deleted)
	}
}

func TestDetectFileChanges_NilPreviouslyUntracked_ReturnsModified(t *testing.T) {
	// This test verifies that DetectFileChanges with nil previouslyUntracked
	// returns all untracked files as New and also returns Modified files.

	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo with go-git
	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("failed to init repo: %v", err)
	}

	// Create and commit a tracked file
	trackedFile := filepath.Join(tmpDir, "tracked.txt")
	if err := os.WriteFile(trackedFile, []byte("content"), 0o644); err != nil {
		t.Fatalf("failed to write tracked file: %v", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	if _, err := worktree.Add("tracked.txt"); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}

	if _, err := worktree.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
		},
	}); err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	// Modify the tracked file and create an untracked file
	if err := os.WriteFile(trackedFile, []byte("modified content"), 0o644); err != nil {
		t.Fatalf("failed to modify tracked file: %v", err)
	}
	untrackedFile := filepath.Join(tmpDir, "untracked.txt")
	if err := os.WriteFile(untrackedFile, []byte("untracked"), 0o644); err != nil {
		t.Fatalf("failed to write untracked file: %v", err)
	}

	// Call DetectFileChanges with nil (all untracked files should be returned)
	changes, err := DetectFileChanges(nil)
	if err != nil {
		t.Fatalf("DetectFileChanges(nil) error = %v", err)
	}

	// Modified should contain tracked.txt
	if len(changes.Modified) != 1 {
		t.Errorf("DetectFileChanges(nil) Modified = %v, want [tracked.txt]", changes.Modified)
	} else if changes.Modified[0] != "tracked.txt" {
		t.Errorf("DetectFileChanges(nil) Modified[0] = %v, want tracked.txt", changes.Modified[0])
	}

	// New should contain untracked.txt
	if len(changes.New) != 1 {
		t.Errorf("DetectFileChanges(nil) New = %v, want [untracked.txt]", changes.New)
	} else if changes.New[0] != "untracked.txt" {
		t.Errorf("DetectFileChanges(nil) New[0] = %v, want untracked.txt", changes.New[0])
	}

	// Deleted should be empty
	if len(changes.Deleted) != 0 {
		t.Errorf("DetectFileChanges(nil) Deleted = %v, want empty", changes.Deleted)
	}
}
