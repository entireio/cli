package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/spf13/cobra"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

func newCheckpointResumeTestCmd(t *testing.T) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	cmd := newCheckpointResumeCmd()
	out := &bytes.Buffer{}
	cmd.SetContext(context.Background())
	cmd.SetOut(out)
	cmd.SetErr(out)
	return cmd, out
}

// setupCheckpointResumeRepo creates an isolated repo, chdirs into it, and
// points the Claude session dir at a temp location so restore flows can write.
func setupCheckpointResumeRepo(t *testing.T) (*git.Repository, *git.Worktree, plumbing.Hash) {
	t.Helper()
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", filepath.Join(tmpDir, "claude-projects"))
	return setupResumeTestRepo(t, tmpDir, false)
}

func TestCheckpointResume_RejectsPositionalWithTargetFlags(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	setupResumeTestRepo(t, tmpDir, false)

	for _, flag := range []string{"--checkpoint=abc123def456", "--commit=HEAD", "--branch=feature"} {
		cmd, _ := newCheckpointResumeTestCmd(t)
		cmd.SetArgs([]string{"sometarget", flag})
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "cannot combine") {
			t.Errorf("Execute() with positional + %s: err = %v, want 'cannot combine'", flag, err)
		}
	}
}

// A ULID-shaped target that matches a committed checkpoint must resolve as a
// checkpoint even when a branch of the same name exists (checkpoint wins).
// The checkpoint's commit is on no branch, so the restore-only fallback runs
// and HEAD must not move.
func TestCheckpointResumeAuto_ChecksCheckpointBeforeBranch(t *testing.T) {
	repo, _, head := setupCheckpointResumeRepo(t)
	cpID := id.MustCheckpointID("01HZXW5J8KQ2M3N4P5Q6R7S8T9")
	writeCommittedResumeCheckpoint(t, repo, cpID, "session-cp-first", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	branchRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(cpID.String()), head)
	if err := repo.Storer.SetReference(branchRef); err != nil {
		t.Fatalf("create branch: %v", err)
	}

	cmd, out := newCheckpointResumeTestCmd(t)
	cmd.SetArgs([]string{cpID.String()})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v\noutput: %s", err, out.String())
	}
	if !strings.Contains(out.String(), "not on any local branch") {
		t.Errorf("output should mention restore-only fallback, got: %s", out.String())
	}
	branch, err := GetCurrentBranch(context.Background())
	if err != nil || branch != masterBaseBranch {
		t.Errorf("HEAD moved: branch = %q err = %v, want master", branch, err)
	}
}

// A target that is a branch name (even hex-shaped) with no matching checkpoint
// must delegate to the branch flow, i.e. check the branch out.
func TestCheckpointResumeAuto_BranchBeforeCommit(t *testing.T) {
	repo, w, _ := setupCheckpointResumeRepo(t)
	// Ignore .entire/ (as `entire enable` would) so the RunE's logging.Init
	// creating .entire/logs/ doesn't register as an uncommitted change and
	// trip switchToBranchForResume's dirty-worktree check.
	if err := os.WriteFile(".gitignore", []byte(".entire/\n"), 0o600); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	if _, err := w.Add(".gitignore"); err != nil {
		t.Fatalf("add .gitignore: %v", err)
	}
	gitignoreCommit, err := w.Commit("add gitignore", &git.CommitOptions{
		Author: &object.Signature{Name: "Test User", Email: "test@example.com"},
	})
	if err != nil {
		t.Fatalf("commit .gitignore: %v", err)
	}
	branchRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("abcdef"), gitignoreCommit)
	if err := repo.Storer.SetReference(branchRef); err != nil {
		t.Fatalf("create branch: %v", err)
	}

	cmd, out := newCheckpointResumeTestCmd(t)
	cmd.SetArgs([]string{"abcdef"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v\noutput: %s", err, out.String())
	}
	branch, err := GetCurrentBranch(context.Background())
	if err != nil || branch != "abcdef" {
		t.Errorf("current branch = %q err = %v, want abcdef", branch, err)
	}
}

// --commit on a trailer-carrying commit resumes that commit's checkpoint. The
// commit is on master (indexed by buildCheckpointBranchIndex) and master is
// already checked out, so the flow ends in a restored session.
func TestCheckpointResumeCommit_ResolvesTrailer(t *testing.T) {
	repo, w, _ := setupCheckpointResumeRepo(t)
	cpID := id.MustCheckpointID("abc123def456")
	writeCommittedResumeCheckpoint(t, repo, cpID, "session-commit", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	commitHash, err := w.Commit("work\n\nEntire-Checkpoint: "+cpID.String(), &git.CommitOptions{
		AllowEmptyCommits: true,
		Author:            &object.Signature{Name: "Test User", Email: "test@example.com"},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	cmd, out := newCheckpointResumeTestCmd(t)
	cmd.SetArgs([]string{"--commit", commitHash.String()})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v\noutput: %s", err, out.String())
	}
	if !strings.Contains(out.String(), "session-commit") {
		t.Errorf("output should mention restored session ID, got: %s", out.String())
	}
}

// "HEAD" resolves via branchCommit's origin/<name> fallback (as
// refs/remotes/origin/HEAD) in the old auto-detection order. With no local
// branch named "HEAD", it must fall through to commit resolution and resume
// the checkpoint referenced by HEAD's trailer.
func TestCheckpointResumeAuto_HeadResolvesAsCommit(t *testing.T) {
	repo, w, head := setupCheckpointResumeRepo(t)
	cpID := id.MustCheckpointID("abc123def456")
	writeCommittedResumeCheckpoint(t, repo, cpID, "session-head", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	if _, err := w.Commit("work\n\nEntire-Checkpoint: "+cpID.String(), &git.CommitOptions{
		AllowEmptyCommits: true,
		Author:            &object.Signature{Name: "Test User", Email: "test@example.com"},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Seed origin/HEAD like a real clone has: auto-detection must not classify
	// "HEAD" as a branch via branchCommit's origin/<name> fallback.
	if err := repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", masterBaseBranch), head)); err != nil {
		t.Fatalf("create origin/master: %v", err)
	}
	if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.NewRemoteHEADReferenceName("origin"), plumbing.NewRemoteReferenceName("origin", masterBaseBranch))); err != nil {
		t.Fatalf("create origin/HEAD: %v", err)
	}

	cmd, out := newCheckpointResumeTestCmd(t)
	cmd.SetArgs([]string{"HEAD"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v\noutput: %s", err, out.String())
	}
	if !strings.Contains(out.String(), "session-head") {
		t.Errorf("output should mention restored session ID, got: %s", out.String())
	}
}

// --checkpoint forces checkpoint interpretation, including prefix matching.
// The checkpoint's commit is on no branch, so the restore-only fallback runs.
func TestCheckpointResumeFlag_Checkpoint(t *testing.T) {
	repo, _, _ := setupCheckpointResumeRepo(t)
	cpID := id.MustCheckpointID("abc123def456")
	writeCommittedResumeCheckpoint(t, repo, cpID, "session-flag", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	cmd, out := newCheckpointResumeTestCmd(t)
	cmd.SetArgs([]string{"--checkpoint", "abc123"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v\noutput: %s", err, out.String())
	}
	if !strings.Contains(out.String(), "session-flag") {
		t.Errorf("output should mention restored session ID, got: %s", out.String())
	}
}

func TestCheckpointResumeFlag_AmbiguousCheckpointPrefix(t *testing.T) {
	repo, _, _ := setupCheckpointResumeRepo(t)
	cpA := id.MustCheckpointID("abc123def456")
	cpB := id.MustCheckpointID("abc123aaa111")
	writeCommittedResumeCheckpoint(t, repo, cpA, "session-a", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	writeCommittedResumeCheckpoint(t, repo, cpB, "session-b", time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC))

	cmd, out := newCheckpointResumeTestCmd(t)
	cmd.SetArgs([]string{"--checkpoint", "abc123"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("Execute() = nil, want error for ambiguous checkpoint prefix")
	}
	output := out.String()
	if !strings.Contains(output, "Ambiguous checkpoint prefix") {
		t.Errorf("output should render the ambiguity failure, got: %s", output)
	}
	for _, cpID := range []id.CheckpointID{cpA, cpB} {
		if !strings.Contains(output, cpID.String()) {
			t.Errorf("output should list match %s, got: %s", cpID, output)
		}
	}
}

// When the checkpoint's branch is checked out in another worktree, resume must
// point there instead of switching branches or restoring logs.
func TestCheckpointResume_WorktreeClash(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	repo, w, baseHead := setupCheckpointResumeRepo(t)
	cpID := id.MustCheckpointID("abc123def456")
	writeCommittedResumeCheckpoint(t, repo, cpID, "session-clash", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	// Put the trailer commit on a branch that is NOT master: commit on master,
	// point "feat" at it, then move master back to the base commit.
	trailerCommit, err := w.Commit("work\n\nEntire-Checkpoint: "+cpID.String(), &git.CommitOptions{
		AllowEmptyCommits: true,
		Author:            &object.Signature{Name: "Test User", Email: "test@example.com"},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("feat"), trailerCommit)); err != nil {
		t.Fatalf("create feat: %v", err)
	}
	if err := repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(masterBaseBranch), baseHead)); err != nil {
		t.Fatalf("reset master: %v", err)
	}

	clashDir := filepath.Join(t.TempDir(), "clash-wt")
	worktreeAdd := exec.CommandContext(context.Background(), "git", "worktree", "add", clashDir, "feat")
	if addOut, err := worktreeAdd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, addOut)
	}
	t.Cleanup(func() {
		if err := exec.CommandContext(context.Background(), "git", "worktree", "remove", clashDir, "--force").Run(); err != nil {
			t.Logf("git worktree remove: %v", err)
		}
	})

	cmd, out := newCheckpointResumeTestCmd(t)
	cmd.SetArgs([]string{cpID.String()})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v\noutput: %s", err, out.String())
	}
	output := out.String()
	if !strings.Contains(output, "already checked out") {
		t.Errorf("output should mention the worktree clash, got: %s", output)
	}
	if !strings.Contains(output, "entire checkpoint resume "+cpID.String()) {
		t.Errorf("output should include the checkpoint-specific resume command, got: %s", output)
	}
	branch, err := GetCurrentBranch(context.Background())
	if err != nil || branch != masterBaseBranch {
		t.Errorf("HEAD moved: branch = %q err = %v, want master", branch, err)
	}
}

// A target that is neither a checkpoint, local branch, nor commit must fall
// back to remote branches: resuming another machine's work usually means the
// branch only exists on origin. --force skips the fetch confirmation.
func TestCheckpointResumeAuto_RemoteBranchFallback(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	repo, _, _ := setupCheckpointResumeRepo(t)

	originDir := t.TempDir()
	testutil.InitRepo(t, originDir)
	testutil.WriteFile(t, originDir, "f.txt", "remote content")
	testutil.GitAdd(t, originDir, "f.txt")
	testutil.GitCommit(t, originDir, "remote work")
	branchCmd := exec.CommandContext(context.Background(), "git", "branch", "remote-feature")
	branchCmd.Dir = originDir
	if out, err := branchCmd.CombinedOutput(); err != nil {
		t.Fatalf("git branch: %v\n%s", err, out)
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{originDir}}); err != nil {
		t.Fatalf("create remote: %v", err)
	}

	cmd, out := newCheckpointResumeTestCmd(t)
	cmd.SetArgs([]string{"remote-feature", "--force"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v\noutput: %s", err, out.String())
	}
	branch, err := GetCurrentBranch(context.Background())
	if err != nil || branch != "remote-feature" {
		t.Errorf("current branch = %q err = %v, want remote-feature", branch, err)
	}
}

func TestCheckpointResumeCommit_NoTrailer(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	_, _, head := setupResumeTestRepo(t, tmpDir, false)

	cmd, out := newCheckpointResumeTestCmd(t)
	cmd.SetArgs([]string{"--commit", head.String()})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() = nil, want error for commit without trailer")
	}
	if !strings.Contains(out.String(), "No associated Entire checkpoint") {
		t.Errorf("output should explain missing trailer, got: %s", out.String())
	}
}

func TestCheckpointResumeAuto_NothingMatched(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	setupResumeTestRepo(t, tmpDir, false)

	cmd, _ := newCheckpointResumeTestCmd(t)
	cmd.SetArgs([]string{"no/such-target"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "nothing matched") {
		t.Errorf("Execute() err = %v, want 'nothing matched'", err)
	}
}

// go test runs are non-interactive (CanPromptInteractively is false under
// testing.Testing()), so bare invocation exercises the non-TTY listing.
func TestCheckpointResumeBare_NonTTYListsCheckpoints(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	repo, _, _ := setupResumeTestRepo(t, tmpDir, false)
	cpOld := id.MustCheckpointID("aaa111bbb222")
	cpNew := id.MustCheckpointID("ccc333ddd444")
	writeCommittedResumeCheckpoint(t, repo, cpOld, "session-old", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	writeCommittedResumeCheckpoint(t, repo, cpNew, "session-new", time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC))

	cmd, out := newCheckpointResumeTestCmd(t)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v\noutput: %s", err, out.String())
	}
	output := out.String()
	for _, want := range []string{cpOld.String(), cpNew.String(), "entire checkpoint resume <checkpoint-id>"} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q:\n%s", want, output)
		}
	}
	if strings.Index(output, cpNew.String()) > strings.Index(output, cpOld.String()) {
		t.Errorf("newest checkpoint should be listed first:\n%s", output)
	}
}

func TestCheckpointResumeOptionLabel_Fallbacks(t *testing.T) {
	t.Parallel()

	unindexed := checkpoint.CheckpointInfo{
		CheckpointID: id.MustCheckpointID("aaa111bbb222"),
		CreatedAt:    time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	indexed := checkpoint.CheckpointInfo{
		CheckpointID: id.MustCheckpointID("ccc333ddd444"),
		CreatedAt:    time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	branchIndex := map[string]string{indexed.CheckpointID.String(): "feature"}

	fallbackLabel := checkpointResumeOptionLabel(unindexed, branchIndex)
	if !strings.Contains(fallbackLabel, "no local branch") {
		t.Errorf("label = %q, want to contain %q", fallbackLabel, "no local branch")
	}
	if !strings.Contains(fallbackLabel, unknownAgentLabel) {
		t.Errorf("label = %q, want to contain %q", fallbackLabel, unknownAgentLabel)
	}

	indexedLabel := checkpointResumeOptionLabel(indexed, branchIndex)
	if !strings.Contains(indexedLabel, "feature") {
		t.Errorf("label = %q, want to contain branch %q", indexedLabel, "feature")
	}
}

func TestCheckpointResumeBare_NoCheckpoints(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	setupResumeTestRepo(t, tmpDir, false)

	cmd, out := newCheckpointResumeTestCmd(t)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(out.String(), "No committed checkpoints found") {
		t.Errorf("output should say no checkpoints found, got: %s", out.String())
	}
}
