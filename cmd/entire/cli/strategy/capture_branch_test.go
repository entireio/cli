package strategy

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

// TestCaptureSessionBranch verifies the branch is recorded while on a branch and
// cleared on a detached HEAD (so a stale value can't survive into resume).
func TestCaptureSessionBranch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "x")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")

	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}

	// On a branch: captures the current branch name (overwriting any prior value).
	state := &SessionState{Branch: "stale"}
	captureSessionBranch(repo, state)
	if want := head.Name().Short(); state.Branch != want {
		t.Errorf("on-branch: Branch = %q, want %q", state.Branch, want)
	}

	// Detached HEAD: clears the stale branch so resume derives it instead.
	if err := repo.Storer.SetReference(plumbing.NewHashReference(plumbing.HEAD, head.Hash())); err != nil {
		t.Fatalf("detach HEAD: %v", err)
	}
	state.Branch = "stale-branch"
	captureSessionBranch(repo, state)
	if state.Branch != "" {
		t.Errorf("detached HEAD should clear Branch, got %q", state.Branch)
	}
}
