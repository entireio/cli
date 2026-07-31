package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// testFeatureBranch is the branch name setupResumeTestRepo hardcodes when
// createFeatureBranch is true.
const testFeatureBranch = "feature"

// writeResumeTestCommit writes filename with placeholder content and commits it
// with the given message.
func writeResumeTestCommit(t *testing.T, tmpDir string, w *git.Worktree, filename, message string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(tmpDir, filename), []byte(filename), 0o644); err != nil {
		t.Fatalf("write %s: %v", filename, err)
	}
	if _, err := w.Add(filename); err != nil {
		t.Fatalf("add %s: %v", filename, err)
	}
	if _, err := w.Commit(message, &git.CommitOptions{
		Author: &object.Signature{Name: "Test User", Email: "test@example.com"},
	}); err != nil {
		t.Fatalf("commit %s: %v", filename, err)
	}
}

// TestResumeErrorsSurviveSilentErrorWrapping pins the errors.As contract the
// JSON action report (and any orchestrating caller) relies on: typed resume
// errors must stay recoverable through SilentError and fmt.Errorf wrapping.
func TestResumeErrorsSurviveSilentErrorWrapping(t *testing.T) {
	t.Parallel()

	t.Run("worktree clash", func(t *testing.T) {
		t.Parallel()
		wrapped := fmt.Errorf("resume: %w", NewSilentError(&ResumeWorktreeClashError{
			Branch:       "feat",
			WorktreePath: "/work/other",
		}))
		var clash *ResumeWorktreeClashError
		if !errors.As(wrapped, &clash) {
			t.Fatalf("errors.As failed to recover ResumeWorktreeClashError from %v", wrapped)
		}
		if clash.Branch != "feat" || clash.WorktreePath != "/work/other" {
			t.Errorf("recovered fields = %q, %q; want feat, /work/other", clash.Branch, clash.WorktreePath)
		}
		if !strings.Contains(clash.Error(), "feat") || !strings.Contains(clash.Error(), "/work/other") {
			t.Errorf("Error() = %q, want branch and worktree path", clash.Error())
		}
	})

	t.Run("no checkpoint", func(t *testing.T) {
		t.Parallel()
		wrapped := fmt.Errorf("resume: %w", &ResumeNoCheckpointError{Branch: "feat"})
		var noCheckpoint *ResumeNoCheckpointError
		if !errors.As(wrapped, &noCheckpoint) {
			t.Fatalf("errors.As failed to recover ResumeNoCheckpointError from %v", wrapped)
		}
		if noCheckpoint.Branch != "feat" {
			t.Errorf("recovered Branch = %q, want feat", noCheckpoint.Branch)
		}
	})

	t.Run("metadata unavailable", func(t *testing.T) {
		t.Parallel()
		wrapped := NewSilentError(&ResumeMetadataUnavailableError{CheckpointID: id.MustCheckpointID("abc123def456")})
		var unavailable *ResumeMetadataUnavailableError
		if !errors.As(wrapped, &unavailable) {
			t.Fatalf("errors.As failed to recover ResumeMetadataUnavailableError from %v", wrapped)
		}
		if unavailable.CheckpointID.String() != "abc123def456" {
			t.Errorf("recovered CheckpointID = %q, want abc123def456", unavailable.CheckpointID)
		}
	})
}

// TestEnsureTrailResumeBranchAvailableReturnsTypedClashError pins the honest
// exit code for the worktree-clash path: guidance is still printed, but the
// caller gets a typed error instead of a nil that exits 0.
func TestEnsureTrailResumeBranchAvailableReturnsTypedClashError(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	setupResumeTestRepo(t, tmpDir, true)

	ctx := context.Background()
	otherWorktree := filepath.Join(t.TempDir(), "feature-wt")

	branch := testFeatureBranch
	gitCmd := exec.CommandContext(ctx, "git", "worktree", "add", otherWorktree, branch)
	gitCmd.Dir = tmpDir
	if out, err := gitCmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}

	var out bytes.Buffer
	err := ensureTrailResumeBranchAvailable(ctx, &out, branch)
	if err == nil {
		t.Fatal("ensureTrailResumeBranchAvailable() = nil, want worktree clash error")
	}
	var clash *ResumeWorktreeClashError
	if !errors.As(err, &clash) {
		t.Fatalf("error = %v, want ResumeWorktreeClashError", err)
	}
	if clash.Branch != branch {
		t.Errorf("clash.Branch = %q, want %s", clash.Branch, branch)
	}
	if clash.WorktreePath == "" {
		t.Error("clash.WorktreePath is empty, want the other worktree's path")
	}
	if !strings.Contains(out.String(), "Resume from that worktree") {
		t.Errorf("output = %q, want the resume-from-worktree guidance", out.String())
	}
}

// TestCheckRemoteMetadata_LocalOnlyRefReturnsMetadataUnavailable covers the
// non-bootstrappable-refs path: previously printed guidance and returned
// nil, nil (exit 0 with nothing resumed).
func TestCheckRemoteMetadata_LocalOnlyRefReturnsMetadataUnavailable(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	setupResumeTestRepo(t, tmpDir, false)

	localOnly := checkpoint.PersistentRefs{
		Primary: plumbing.NewBranchReferenceName("entire/checkpoints/v1"),
		Read:    plumbing.ReferenceName("refs/entire/local-only"),
	}
	if localOnly.ReadBootstrappableFromOrigin() {
		t.Fatal("test setup: refs must not be bootstrappable from origin")
	}

	var stderr bytes.Buffer
	checkpointID := id.MustCheckpointID("aaa111bbb222")
	_, err := checkRemoteMetadata(context.Background(), os.Stdout, &stderr, checkpointID, localOnly)
	if err == nil {
		t.Fatal("checkRemoteMetadata() = nil error for local-only ref, want ResumeMetadataUnavailableError")
	}
	var unavailable *ResumeMetadataUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("error = %v, want ResumeMetadataUnavailableError", err)
	}
	if unavailable.CheckpointID != checkpointID {
		t.Errorf("CheckpointID = %s, want %s", unavailable.CheckpointID, checkpointID)
	}
	if !strings.Contains(stderr.String(), "local-only") {
		t.Errorf("stderr = %q, want local-only guidance retained", stderr.String())
	}
}

// TestRestoreFromCurrentBranch_NewerCommitsNonInteractiveProceedsWithNotice
// pins the prompt guard: a non-interactive run (go test is non-interactive by
// default) with commits newer than the checkpoint must proceed with a notice
// instead of attempting a prompt and failing.
func TestRestoreFromCurrentBranch_NewerCommitsNonInteractiveProceedsWithNotice(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", filepath.Join(tmpDir, "claude-projects"))

	repo, w, _ := setupResumeTestRepo(t, tmpDir, false)
	checkpointID := id.MustCheckpointID("aaa111bbb222")
	writeCommittedResumeCheckpoint(t, repo, checkpointID, "session-notice", time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC))

	writeResumeTestCommit(t, tmpDir, w, "checkpointed.txt", "Add feature\n\nEntire-Checkpoint: "+checkpointID.String())
	writeResumeTestCommit(t, tmpDir, w, "newer.txt", "Manual follow-up commit")

	var stdout, stderr bytes.Buffer
	sessions, err := restoreFromCurrentBranch(context.Background(), &stdout, &stderr, "master", false)
	if err != nil {
		t.Fatalf("restoreFromCurrentBranch() error = %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	if len(sessions) == 0 {
		t.Fatalf("restoreFromCurrentBranch() restored no sessions\nstdout: %s\nstderr: %s", stdout.String(), stderr.String())
	}

	notice := "has no checkpoint; resuming from"
	if !strings.Contains(stdout.String(), notice) {
		t.Errorf("stdout = %q, want notice containing %q", stdout.String(), notice)
	}
	if !strings.Contains(stdout.String(), "(1 commit newer than the checkpoint)") {
		t.Errorf("stdout = %q, want singular commit-count phrasing", stdout.String())
	}
}
