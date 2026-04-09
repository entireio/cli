package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
)

// prePromptStateFile returns the absolute path to the pre-prompt state file for a session.
// Test-only helper; production code constructs the filename inline.
func prePromptStateFile(ctx context.Context, sessionID string) string {
	tmpDirAbs, err := paths.AbsPath(ctx, paths.EntireTmpDir)
	if err != nil {
		tmpDirAbs = paths.EntireTmpDir
	}
	return filepath.Join(tmpDirAbs, fmt.Sprintf("pre-prompt-%s.json", sessionID))
}

// preTaskStateFile returns the absolute path to the pre-task state file for a tool use.
// Test-only helper; production code constructs the filename inline.
func preTaskStateFile(ctx context.Context, toolUseID string) string {
	tmpDirAbs, err := paths.AbsPath(ctx, paths.EntireTmpDir)
	if err != nil {
		tmpDirAbs = paths.EntireTmpDir
	}
	return filepath.Join(tmpDirAbs, fmt.Sprintf("pre-task-%s.json", toolUseID))
}

func TestPreTaskStateFile(t *testing.T) {
	toolUseID := "toolu_abc123"
	// preTaskStateFile returns an absolute path within the repo
	// Verify it ends with the expected relative path suffix
	expectedSuffix := filepath.Join(paths.EntireTmpDir, "pre-task-toolu_abc123.json")
	got := preTaskStateFile(context.Background(), toolUseID)
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
	// Verify that state files written by older CLI versions with deprecated fields
	// are correctly migrated to TranscriptOffset on load.
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo
	if err := os.MkdirAll(".git/objects", 0o755); err != nil {
		t.Fatalf("Failed to create .git: %v", err)
	}
	if err := os.WriteFile(".git/HEAD", []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("Failed to create HEAD: %v", err)
	}
	paths.ClearWorktreeRootCache()
	if err := os.MkdirAll(paths.EntireTmpDir, 0o755); err != nil {
		t.Fatalf("Failed to create tmp dir: %v", err)
	}

	sessionID := "test-backward-compat"
	stateFile := prePromptStateFile(context.Background(), sessionID)

	// Test 1: Oldest format (last_transcript_line_count) migrates to TranscriptOffset
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

	state, err := LoadPrePromptState(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("LoadPrePromptState() error = %v", err)
	}
	require.NotNil(t, state, "LoadPrePromptState() returned nil")

	if state.TranscriptOffset != 42 {
		t.Errorf("TranscriptOffset = %d, want 42 (migrated from last_transcript_line_count)", state.TranscriptOffset)
	}
	if state.LastTranscriptLineCount != 0 {
		t.Errorf("LastTranscriptLineCount = %d, want 0 (should be cleared after migration)", state.LastTranscriptLineCount)
	}
	if state.StepTranscriptStart != 0 {
		t.Errorf("StepTranscriptStart = %d, want 0 (should be cleared after migration)", state.StepTranscriptStart)
	}
	if state.LastTranscriptIdentifier != "user-5" {
		t.Errorf("LastTranscriptIdentifier = %q, want %q", state.LastTranscriptIdentifier, "user-5")
	}

	// Test 2: step_transcript_start migrates to TranscriptOffset (takes precedence over oldest)
	stepFormatJSON := `{
		"session_id": "test-backward-compat",
		"timestamp": "2026-01-01T00:00:00Z",
		"untracked_files": [],
		"step_transcript_start": 100,
		"last_transcript_line_count": 42
	}`
	if err := os.WriteFile(stateFile, []byte(stepFormatJSON), 0o644); err != nil {
		t.Fatalf("Failed to write step-format state file: %v", err)
	}

	state, err = LoadPrePromptState(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("LoadPrePromptState() error = %v", err)
	}
	if state.TranscriptOffset != 100 {
		t.Errorf("TranscriptOffset = %d, want 100 (step_transcript_start takes precedence)", state.TranscriptOffset)
	}

	// Test 3: start_message_index (Gemini format) migrates to TranscriptOffset
	geminiFormatJSON := `{
		"session_id": "test-backward-compat",
		"timestamp": "2026-01-01T00:00:00Z",
		"untracked_files": [],
		"start_message_index": 25,
		"last_transcript_identifier": "msg-42"
	}`
	if err := os.WriteFile(stateFile, []byte(geminiFormatJSON), 0o644); err != nil {
		t.Fatalf("Failed to write gemini-format state file: %v", err)
	}

	state, err = LoadPrePromptState(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("LoadPrePromptState() error = %v", err)
	}
	if state.TranscriptOffset != 25 {
		t.Errorf("TranscriptOffset = %d, want 25 (migrated from start_message_index)", state.TranscriptOffset)
	}
	if state.StartMessageIndex != 0 {
		t.Errorf("StartMessageIndex = %d, want 0 (should be cleared after migration)", state.StartMessageIndex)
	}

	// Test 4: transcript_offset (new format) takes precedence over all deprecated fields
	newFormatJSON := `{
		"session_id": "test-backward-compat",
		"timestamp": "2026-01-01T00:00:00Z",
		"untracked_files": [],
		"transcript_offset": 200,
		"step_transcript_start": 100,
		"start_message_index": 50,
		"last_transcript_line_count": 42
	}`
	if err := os.WriteFile(stateFile, []byte(newFormatJSON), 0o644); err != nil {
		t.Fatalf("Failed to write new-format state file: %v", err)
	}

	state, err = LoadPrePromptState(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("LoadPrePromptState() error = %v", err)
	}
	if state.TranscriptOffset != 200 {
		t.Errorf("TranscriptOffset = %d, want 200 (new field takes precedence)", state.TranscriptOffset)
	}

	// Cleanup
	if err := CleanupPrePromptState(context.Background(), sessionID); err != nil {
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
			// On Windows, git status returns paths with backslashes (e.g., "src\file.ts").
			// filepath.ToSlash normalizes these to forward slashes for consistent storage.
			// On Linux/macOS, filepath.Separator is already '/', so ToSlash is a no-op —
			// we use strings.ReplaceAll to simulate the Windows input on all platforms.
			name: "platform separator paths are normalized to forward slashes",
			files: []string{
				"src" + string(filepath.Separator) + "file.ts",
				"lib" + string(filepath.Separator) + "nested" + string(filepath.Separator) + "util.go",
			},
			basePath: "/repo",
			want: []string{
				"src/file.ts",
				"lib/nested/util.go",
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
	paths.ClearWorktreeRootCache()

	// Create .entire/tmp directory
	if err := os.MkdirAll(paths.EntireTmpDir, 0o755); err != nil {
		t.Fatalf("Failed to create tmp dir: %v", err)
	}

	// Test with no pre-task files
	taskID, found := FindActivePreTaskFile(context.Background())
	if found {
		t.Error("FindActivePreTaskFile(context.Background()) should return false when no pre-task files exist")
	}
	if taskID != "" {
		t.Errorf("FindActivePreTaskFile(context.Background()) taskID = %v, want empty", taskID)
	}

	// Create a pre-task file
	preTaskFile := filepath.Join(paths.EntireTmpDir, "pre-task-toolu_abc123.json")
	if err := os.WriteFile(preTaskFile, []byte(`{"tool_use_id": "toolu_abc123"}`), 0o644); err != nil {
		t.Fatalf("Failed to create pre-task file: %v", err)
	}

	// Test with one pre-task file
	taskID, found = FindActivePreTaskFile(context.Background())
	if !found {
		t.Error("FindActivePreTaskFile(context.Background()) should return true when pre-task file exists")
	}
	if taskID != "toolu_abc123" {
		t.Errorf("FindActivePreTaskFile(context.Background()) taskID = %v, want toolu_abc123", taskID)
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
	paths.ClearWorktreeRootCache()

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
	transcriptContent := `{"type":"user","uuid":"user-1","message":{"content":"Hello"}}
{"type":"assistant","uuid":"asst-1","message":{"content":[{"type":"text","text":"Hi"}]}}
{"type":"user","uuid":"user-2","message":{"content":"How are you?"}}`

	transcriptPath := setupTestRepoWithTranscript(t, transcriptContent, "transcript.jsonl")

	sessionID := "test-session-123"
	ag := claudecode.NewClaudeCodeAgent()

	// Capture state with transcript path using Claude agent (JSONL format)
	if err := CapturePrePromptState(context.Background(), ag, sessionID, transcriptPath); err != nil {
		t.Fatalf("CapturePrePromptState(context.Background(),) error = %v", err)
	}

	// Load and verify
	state, err := LoadPrePromptState(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("LoadPrePromptState() error = %v", err)
	}
	if state == nil {
		t.Fatal("LoadPrePromptState() returned nil")
		return // unreachable but satisfies staticcheck
	}

	// Verify transcript offset was captured (3 JSONL lines)
	if state.TranscriptOffset != 3 {
		t.Errorf("TranscriptOffset = %d, want 3", state.TranscriptOffset)
	}

	// Cleanup
	if err := CleanupPrePromptState(context.Background(), sessionID); err != nil {
		t.Errorf("CleanupPrePromptState() error = %v", err)
	}
}

func TestPrePromptState_WithEmptyTranscriptPath(t *testing.T) {
	setupTestRepoWithTranscript(t, "", "") // No transcript file

	sessionID := "test-session-empty-transcript"
	ag := claudecode.NewClaudeCodeAgent()

	// Capture state with empty transcript path
	if err := CapturePrePromptState(context.Background(), ag, sessionID, ""); err != nil {
		t.Fatalf("CapturePrePromptState(context.Background(),) error = %v", err)
	}

	// Load and verify
	state, err := LoadPrePromptState(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("LoadPrePromptState() error = %v", err)
	}
	if state == nil {
		t.Fatal("LoadPrePromptState() returned nil")
		return // unreachable but satisfies staticcheck
	}

	// Transcript position should be zero when no transcript provided
	if state.TranscriptOffset != 0 {
		t.Errorf("TranscriptOffset = %d, want 0", state.TranscriptOffset)
	}

	// Cleanup
	if err := CleanupPrePromptState(context.Background(), sessionID); err != nil {
		t.Errorf("CleanupPrePromptState() error = %v", err)
	}
}

func TestPrePromptState_WithSummaryOnlyTranscript(t *testing.T) {
	// Summary rows have leafUuid but not uuid
	transcriptContent := `{"type":"summary","leafUuid":"leaf-1","summary":"Previous context"}
{"type":"summary","leafUuid":"leaf-2","summary":"More context"}`

	transcriptPath := setupTestRepoWithTranscript(t, transcriptContent, "transcript-summary.jsonl")

	sessionID := "test-session-summary-only"
	ag := claudecode.NewClaudeCodeAgent()

	// Capture state
	if err := CapturePrePromptState(context.Background(), ag, sessionID, transcriptPath); err != nil {
		t.Fatalf("CapturePrePromptState(context.Background(),) error = %v", err)
	}

	// Load and verify
	state, err := LoadPrePromptState(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("LoadPrePromptState() error = %v", err)
	}
	require.NotNil(t, state, "LoadPrePromptState() returned nil")

	// TranscriptOffset should be 2 (2 JSONL lines counted by ClaudeCodeAgent)
	if state.TranscriptOffset != 2 {
		t.Errorf("TranscriptOffset = %d, want 2", state.TranscriptOffset)
	}

	// Cleanup
	if err := CleanupPrePromptState(context.Background(), sessionID); err != nil {
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
	changes, err := DetectFileChanges(context.Background(), nil)
	if err != nil {
		t.Fatalf("DetectFileChanges(context.Background(),nil) error = %v", err)
	}

	// New should be nil when there are no untracked files
	if len(changes.New) != 0 {
		t.Errorf("DetectFileChanges(context.Background(),nil) New = %v, want empty", changes.New)
	}

	// Deleted should contain the deleted tracked file
	if len(changes.Deleted) != 1 {
		t.Errorf("DetectFileChanges(context.Background(),nil) Deleted = %v, want [tracked.txt]", changes.Deleted)
	} else if changes.Deleted[0] != "tracked.txt" {
		t.Errorf("DetectFileChanges(context.Background(),nil) Deleted[0] = %v, want tracked.txt", changes.Deleted[0])
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
	changes, err := DetectFileChanges(context.Background(), []string{"pre-existing-untracked.txt"})
	if err != nil {
		t.Fatalf("DetectFileChanges(context.Background(),) error = %v", err)
	}

	// New should contain only new-file.txt (not pre-existing-untracked.txt)
	if len(changes.New) != 1 {
		t.Errorf("DetectFileChanges(context.Background(),) New = %v, want [new-file.txt]", changes.New)
	} else if changes.New[0] != "new-file.txt" {
		t.Errorf("DetectFileChanges(context.Background(),) New[0] = %v, want new-file.txt", changes.New[0])
	}

	// Deleted should contain tracked1.txt
	if len(changes.Deleted) != 1 {
		t.Errorf("DetectFileChanges(context.Background(),) Deleted = %v, want [tracked1.txt]", changes.Deleted)
	} else if changes.Deleted[0] != "tracked1.txt" {
		t.Errorf("DetectFileChanges(context.Background(),) Deleted[0] = %v, want tracked1.txt", changes.Deleted[0])
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
	changes, err := DetectFileChanges(context.Background(), []string{})
	if err != nil {
		t.Fatalf("DetectFileChanges(context.Background(),) error = %v", err)
	}

	if len(changes.Modified) != 0 {
		t.Errorf("DetectFileChanges(context.Background(),) Modified = %v, want empty", changes.Modified)
	}

	if len(changes.New) != 0 {
		t.Errorf("DetectFileChanges(context.Background(),) New = %v, want empty", changes.New)
	}

	if len(changes.Deleted) != 0 {
		t.Errorf("DetectFileChanges(context.Background(),) Deleted = %v, want empty", changes.Deleted)
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
	changes, err := DetectFileChanges(context.Background(), nil)
	if err != nil {
		t.Fatalf("DetectFileChanges(context.Background(),nil) error = %v", err)
	}

	// Modified should contain tracked.txt
	if len(changes.Modified) != 1 {
		t.Errorf("DetectFileChanges(context.Background(),nil) Modified = %v, want [tracked.txt]", changes.Modified)
	} else if changes.Modified[0] != "tracked.txt" {
		t.Errorf("DetectFileChanges(context.Background(),nil) Modified[0] = %v, want tracked.txt", changes.Modified[0])
	}

	// New should contain untracked.txt
	if len(changes.New) != 1 {
		t.Errorf("DetectFileChanges(context.Background(),nil) New = %v, want [untracked.txt]", changes.New)
	} else if changes.New[0] != "untracked.txt" {
		t.Errorf("DetectFileChanges(context.Background(),nil) New[0] = %v, want untracked.txt", changes.New[0])
	}

	// Deleted should be empty
	if len(changes.Deleted) != 0 {
		t.Errorf("DetectFileChanges(context.Background(),nil) Deleted = %v, want empty", changes.Deleted)
	}
}

func TestMergeUnique(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		base  []string
		extra []string
		want  []string
	}{
		{
			name:  "disjoint sets",
			base:  []string{"a.go", "b.go"},
			extra: []string{"c.go", "d.go"},
			want:  []string{"a.go", "b.go", "c.go", "d.go"},
		},
		{
			name:  "overlapping sets",
			base:  []string{"a.go", "b.go"},
			extra: []string{"b.go", "c.go"},
			want:  []string{"a.go", "b.go", "c.go"},
		},
		{
			name:  "empty extra",
			base:  []string{"a.go"},
			extra: nil,
			want:  []string{"a.go"},
		},
		{
			name:  "empty base",
			base:  nil,
			extra: []string{"a.go"},
			want:  []string{"a.go"},
		},
		{
			name:  "both empty",
			base:  nil,
			extra: nil,
			want:  nil,
		},
		{
			name:  "identical sets",
			base:  []string{"a.go", "b.go"},
			extra: []string{"a.go", "b.go"},
			want:  []string{"a.go", "b.go"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := mergeUnique(tt.base, tt.extra)
			if len(got) != len(tt.want) {
				t.Fatalf("mergeUnique() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("mergeUnique()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestFilterToUncommittedFiles_ReallyModified(t *testing.T) {
	// Verify that files with genuinely different content are kept.

	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	repo, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("failed to init repo: %v", err)
	}

	filePath := filepath.Join(tmpDir, "file.txt")
	if err := os.WriteFile(filePath, []byte("original\n"), 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}
	if _, err := wt.Add("file.txt"); err != nil {
		t.Fatalf("failed to add file: %v", err)
	}
	if _, err := wt.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
		},
	}); err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	// Modify the file with genuinely different content
	if err := os.WriteFile(filePath, []byte("modified\n"), 0o644); err != nil {
		t.Fatalf("failed to rewrite file: %v", err)
	}

	result := filterToUncommittedFiles(context.Background(), []string{"file.txt"}, tmpDir)
	if len(result) != 1 || result[0] != "file.txt" {
		t.Errorf("filterToUncommittedFiles() = %v, want [file.txt]", result)
	}
}
