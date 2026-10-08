package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"

	"github.com/spf13/cobra"
)

// newSessionResumeTestCmd builds `session resume` the way the tests for its
// sibling build `checkpoint resume`: standalone, so Execute() dispatches here
// rather than from a root that would not carry these args.
func newSessionResumeTestCmd(t *testing.T) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	cmd := newResumeCmd()
	out := &bytes.Buffer{}
	cmd.SetContext(context.Background())
	cmd.SetOut(out)
	cmd.SetErr(out)
	return cmd, out
}

// A shared session has a checkpoint but no branch of its own: it never
// committed, so no commit trailer names it and no branch contains it. Resuming
// it must restore the session log in place rather than looking for a branch.
//
// This is the case `entire session share` exists to produce, and the reason
// `session resume` takes more than a branch.
func TestSessionResume_CheckpointIDWithoutBranch(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", filepath.Join(tmpDir, "claude-projects"))
	repo, _, _ := setupResumeTestRepo(t, tmpDir, false)

	cpID := id.MustCheckpointID("abc123def456")
	writeCommittedResumeCheckpoint(t, repo, cpID, "shared-session", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	before, err := GetCurrentBranch(context.Background())
	if err != nil {
		t.Fatalf("GetCurrentBranch: %v", err)
	}

	cmd, out := newSessionResumeTestCmd(t)
	cmd.SetArgs([]string{cpID.String()})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v\noutput: %s", err, out.String())
	}

	output := out.String()
	if !strings.Contains(output, "not on any local branch") {
		t.Errorf("resume should say it is restoring without a branch, got: %s", output)
	}
	if !strings.Contains(output, "shared-session") {
		t.Errorf("resume should restore the shared session, got: %s", output)
	}

	after, err := GetCurrentBranch(context.Background())
	if err != nil {
		t.Fatalf("GetCurrentBranch after resume: %v", err)
	}
	if after != before {
		t.Errorf("branchless resume moved HEAD: branch = %q, want %q", after, before)
	}
}

// A prefix is enough, matching every other place a checkpoint ID is accepted —
// the full ID is long, and sharing means someone types or pastes it.
func TestSessionResume_CheckpointPrefixWithoutBranch(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", filepath.Join(tmpDir, "claude-projects"))
	repo, _, _ := setupResumeTestRepo(t, tmpDir, false)

	cpID := id.MustCheckpointID("abc123def456")
	writeCommittedResumeCheckpoint(t, repo, cpID, "prefix-session", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	cmd, out := newSessionResumeTestCmd(t)
	cmd.SetArgs([]string{"abc123"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v\noutput: %s", err, out.String())
	}
	if !strings.Contains(out.String(), "prefix-session") {
		t.Errorf("resume should accept a checkpoint prefix, got: %s", out.String())
	}
}

// Routing the positional argument through the shared resolver must not cost
// `session resume` the branch handling it already had.
func TestSessionResume_BranchStillResolves(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", filepath.Join(tmpDir, "claude-projects"))
	setupResumeTestRepo(t, tmpDir, true)

	cmd, out := newSessionResumeTestCmd(t)
	cmd.SetArgs([]string{"feature"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v\noutput: %s", err, out.String())
	}

	branch, err := GetCurrentBranch(context.Background())
	if err != nil {
		t.Fatalf("GetCurrentBranch: %v", err)
	}
	if branch != "feature" {
		t.Errorf("branch target should still check the branch out: branch = %q, want feature", branch)
	}
}

// A target that names nothing must say so against all three interpretations,
// not just the one the old branch-only path knew about.
func TestSessionResume_UnknownTargetReportsAllInterpretations(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", filepath.Join(tmpDir, "claude-projects"))
	setupResumeTestRepo(t, tmpDir, false)

	cmd, out := newSessionResumeTestCmd(t)
	cmd.SetArgs([]string{"no-such-target"})
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("Execute() = nil, want an error for an unknown target\noutput: %s", out.String())
	}
	if !strings.Contains(err.Error(), "checkpoint ID, branch, or commit") {
		t.Errorf("error should name every interpretation tried, got: %v", err)
	}
}
