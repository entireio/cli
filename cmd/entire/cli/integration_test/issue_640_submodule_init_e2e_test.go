//go:build integration

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// TestIssue640_SubmoduleWorktree_SessionCreatesCheckpoint is a full-flow
// reproduction of #640: inside a git submodule the working tree's .git is a FILE
// pointing at the superproject's modules dir ("gitdir: ../.git/modules/<name>").
// GetWorktreeID did not recognize that layout and returned "unexpected gitdir
// format", so session initialization failed and no checkpoint was ever created
// for work done inside a submodule.
//
// It builds a real submodule, points the harness at the submodule worktree, and
// drives the real hook binary end-to-end (user-prompt-submit, a file change, and
// stop). It then asserts a rewind point exists — i.e. session init succeeded and
// a checkpoint was saved for work done inside the submodule.
func TestIssue640_SubmoduleWorktree_SessionCreatesCheckpoint(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	root := env.T.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	upstream := filepath.Join(root, "upstream")
	super := filepath.Join(root, "super")

	runGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Env = testutil.GitIsolatedEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git -C %s %v: %v\n%s", dir, args, err, out)
		}
	}
	writeFileAt := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	// Upstream repo that the submodule points at.
	if err := os.MkdirAll(upstream, 0o755); err != nil {
		t.Fatalf("mkdir upstream: %v", err)
	}
	runGit(upstream, "init")
	runGit(upstream, "config", "user.name", "Test User")
	runGit(upstream, "config", "user.email", "test@example.com")
	runGit(upstream, "config", "commit.gpgsign", "false")
	writeFileAt(filepath.Join(upstream, "lib.txt"), "lib")
	runGit(upstream, "add", "lib.txt")
	runGit(upstream, "commit", "-m", "upstream init")

	// Superproject with the upstream added as a submodule at ./sub. The local
	// file transport is disabled by default (CVE-2022-39253), so allow it for
	// this hermetic setup.
	if err := os.MkdirAll(super, 0o755); err != nil {
		t.Fatalf("mkdir super: %v", err)
	}
	runGit(super, "init")
	runGit(super, "config", "user.name", "Test User")
	runGit(super, "config", "user.email", "test@example.com")
	runGit(super, "config", "commit.gpgsign", "false")
	writeFileAt(filepath.Join(super, "README.md"), "# super")
	runGit(super, "add", "README.md")
	runGit(super, "commit", "-m", "super init")
	runGit(super, "-c", "protocol.file.allow=always", "submodule", "add", upstream, "sub")
	runGit(super, "commit", "-m", "add submodule sub")

	sub := filepath.Join(super, "sub")

	// Confirm the #640 precondition: the submodule's .git is a FILE whose gitdir
	// points into the superproject's modules directory.
	gitFileContent, err := os.ReadFile(filepath.Join(sub, ".git"))
	if err != nil {
		t.Fatalf("read submodule .git file: %v", err)
	}
	if !strings.Contains(filepath.ToSlash(string(gitFileContent)), ".git/modules/") {
		t.Fatalf("submodule .git file is not a modules gitdir (precondition for #640): %q", gitFileContent)
	}

	// Point the harness at the submodule worktree and drive the real flow there.
	env.RepoDir = sub
	env.GitCheckoutNewBranch("feature/sub-work")
	env.InitEntire()

	session := env.NewSession()
	if err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Create app.txt"); err != nil {
		t.Fatalf("user-prompt-submit: %v", err)
	}
	env.WriteFile("app.txt", "hello")
	session.CreateTranscript("Create app.txt", []FileChange{{Path: "app.txt", Content: "hello"}})
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// End-to-end proof via the real `checkpoint rewind --list`: a checkpoint was
	// created for the work done inside the submodule. Without the fix, session
	// init failed on the submodule gitdir, so no checkpoint (and no rewind point)
	// exists.
	if points := env.GetRewindPoints(); len(points) == 0 {
		t.Fatal("no rewind point after a session inside a submodule — session init failed on the submodule gitdir, so no checkpoint was created (#640)")
	}
}
