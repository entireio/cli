//go:build integration

package integration

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
)

// TestLinkedWorktree_EntireSetUpVisible verifies that when Entire is enabled
// in a repo and the user (or an agent like Claude Code's worktrees feature)
// creates a linked worktree under .claude/worktrees/<branch>, running
// `entire status` inside that linked worktree treats it as set up — i.e.
// resolves .entire/settings.json against the main worktree root, not the
// linked worktree root.
//
// Regression: prior to anchoring .entire/* paths at MainWorktreeRoot in
// paths.AbsPath, every git hook fired from inside a linked worktree (e.g.
// prepare-commit-msg, post-commit, pre-push) saw IsSetUpAndEnabled = false
// and silently bailed out. The user-visible symptom was that commits made in
// agent worktrees never received the Entire-Checkpoint trailer, were never
// condensed, and never pushed `entire/checkpoints/v1`. Status was the
// shortest path to a regression test for that gating decision.
func TestLinkedWorktree_EntireSetUpVisible(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)

	// Sanity check: from the main worktree, status reports a healthy repo.
	mainStatus := env.RunCLI("status")
	if strings.Contains(mainStatus, "not set up") {
		t.Fatalf("baseline status from main worktree shows not-set-up; setup is wrong:\n%s", mainStatus)
	}

	// Create a linked worktree shaped like Claude Code's worktrees feature.
	// `git worktree add -b <branch> <path>` requires the parent dir to exist.
	worktreeDir := filepath.Join(env.RepoDir, ".claude", "worktrees", "feature-x")
	runGitIn(t, env.RepoDir, "worktree", "add", "-b", "feature-x", worktreeDir)

	// Run `entire status` inside the linked worktree. This is the integration
	// surface that proves AbsPath now anchors .entire/* paths at the main
	// repo: IsSetUp walks paths.AbsPath -> MainWorktreeRoot rather than
	// show-toplevel of the linked worktree.
	out, err := runCLIIn(env, worktreeDir, "status")
	if err != nil {
		t.Fatalf("entire status inside linked worktree failed: %v\n%s", err, out)
	}
	if strings.Contains(out, "not set up") {
		t.Errorf("entire status from linked worktree reports not-set-up; .entire/* path resolution is not anchored at main worktree.\nOutput:\n%s", out)
	}
}

func runGitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

func runCLIIn(env *TestEnv, dir string, args ...string) (string, error) {
	cmd := execx.NonInteractive(context.Background(), getTestBinary(), args...)
	cmd.Dir = dir
	cmd.Env = env.cliEnv()
	out, err := cmd.CombinedOutput()
	return string(out), err
}
