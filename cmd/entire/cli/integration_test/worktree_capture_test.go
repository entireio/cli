//go:build integration

package integration

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

const worktreeFixContent = "package main // fixed in the worktree\n"

// worktreeGit returns git's STDOUT only. Callers parse the result as a commit
// hash, a blob's contents, a common dir and ls-tree names, so stderr must not
// reach it: git writes advice and gc notices there even under GitIsolatedEnv,
// and a warning concatenated onto a hash fails in a way that looks like a bug in
// the code under test. Stderr is captured separately so a failure still reports
// what git said.
func worktreeGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// addLinkedWorktree creates a linked worktree of env.RepoDir on a new branch and
// returns its path, with the settings file committed first so the worktree's
// branch carries it — otherwise every hook in there no-ops on
// settings.IsSetUpAndEnabled and the test would pass for the wrong reason.
func addLinkedWorktree(t *testing.T, env *TestEnv, branch string) string {
	t.Helper()

	worktreeGit(t, env.RepoDir, "add", "-f", ".entire/settings.json")
	worktreeGit(t, env.RepoDir, "commit", "-m", "commit entire settings", "--no-gpg-sign", "--no-verify")

	parent := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(parent); err == nil {
		parent = resolved
	}
	dir := filepath.Join(parent, "wt")
	worktreeGit(t, env.RepoDir, "worktree", "add", "-b", branch, dir)
	if _, err := os.Stat(filepath.Join(dir, ".entire", "settings.json")); err != nil {
		t.Fatalf("linked worktree is missing .entire/settings.json, so hooks there would no-op: %v", err)
	}
	return dir
}

// TestWorktreeCapture_TurnInALinkedWorktreeIsCapturedThere is the acceptance test
// for the cwd fix, and covers a gap the suite had entirely: nothing else
// dispatches a capture turn whose working directory is a linked worktree.
//
// The hook subprocess deliberately runs in the MAIN checkout while the event
// names the worktree. That is the real shape — Claude Code's "cwd" follows
// Claude into a worktree, but a hook process can be spawned from anywhere — and
// it is what makes the assertion meaningful: capture must follow the event, not
// the process.
//
// Before the fix, the agent's file was outside the resolved repo root, so
// FilterAndNormalizePaths dropped it, totalChanges hit zero, and turn end took
// the "no files modified during session" early return: no checkpoint at all.
func TestWorktreeCapture_TurnInALinkedWorktreeIsCapturedThere(t *testing.T) {
	env := NewFeatureBranchEnv(t)
	env.InitEntire()
	wtDir := addLinkedWorktree(t, env, "fix/pr-1")

	// The hook process sits in the main checkout throughout.
	runner := NewHookRunner(env.RepoDir, env.ClaudeProjectDir, t)
	session := env.NewSession()

	if err := runner.SimulateUserPromptSubmitWithCwd(session.ID, "fix the PR", session.TranscriptPath, wtDir); err != nil {
		t.Fatalf("prompt submit: %v", err)
	}

	fixPath := filepath.Join(wtDir, "fix.go")
	if err := os.WriteFile(fixPath, []byte(worktreeFixContent), 0o600); err != nil {
		t.Fatalf("write agent file: %v", err)
	}
	session.CreateTranscript("fix the PR", []FileChange{{Path: fixPath, Content: worktreeFixContent}})

	if err := runner.SimulateStopWithCwd(session.ID, session.TranscriptPath, wtDir); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// The transcript belongs with the turn, in the worktree it happened in.
	if _, err := os.Stat(filepath.Join(wtDir, ".entire", "metadata", session.ID, "full.jsonl")); err != nil {
		t.Errorf("transcript was not written into the worktree that the turn happened in: %v", err)
	}

	// And the agent's file must be in a checkpoint, with the content the agent
	// wrote — not absent, and not the main checkout's version of the path.
	shadow := worktreeShadowBranchWithFile(t, env, "fix.go")
	if shadow == "" {
		t.Fatal("no checkpoint captured fix.go: the turn happened in the worktree and was dropped")
	}
	got := worktreeGit(t, env.RepoDir, "show", shadow+":fix.go")
	if got != strings.TrimSpace(worktreeFixContent) {
		t.Errorf("checkpointed fix.go = %q, want the agent's content %q", got, worktreeFixContent)
	}
}

// TestWorktreeCapture_WorktreeLaunchedSessionRecordsTheWorktree covers the case
// the acceptance test above does NOT: a session whose FIRST turn happens in the
// linked worktree, so initializeSession runs under the override and records
// where the session is acting rather than where the hook process stands.
//
// This is a separate fact from capture landing correctly, and worth pinning on
// its own because other work builds on it: commit linking unions
// worktree-matched and ancestry-matched sessions, and it is SessionState's
// WorktreePath that decides the first half. A session that records the worktree
// it acted in is linkable from that worktree without any ancestry signal, which
// is what makes it work for agents that publish no session ID and on Windows
// where proclive cannot introspect.
//
// It also pins the pairing. WorktreeID is derived FROM worktreePath
// (manual_commit_session.go:606-612), not resolved separately from the process
// directory, so both move together and the WorktreePath/WorktreeID disalignment
// that reconcileWorktreePathForResumedTurn exists to prevent cannot arise here.
// Assert both, or a future change could move one and leave the other.
func TestWorktreeCapture_WorktreeLaunchedSessionRecordsTheWorktree(t *testing.T) {
	env := NewFeatureBranchEnv(t)
	env.InitEntire()
	wtDir := addLinkedWorktree(t, env, "fix/pr-2")

	// Hook process in the main checkout; only the event names the worktree.
	runner := NewHookRunner(env.RepoDir, env.ClaudeProjectDir, t)
	session := env.NewSession()

	if err := runner.SimulateUserPromptSubmitWithCwd(session.ID, "start in the worktree", session.TranscriptPath, wtDir); err != nil {
		t.Fatalf("prompt submit: %v", err)
	}

	state, err := env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("read session state: %v", err)
	}

	if state.WorktreePath != wtDir {
		// Report only this. An empty WorktreeID is CORRECT when WorktreePath is
		// the main checkout, so also asserting the pairing here would accuse the
		// code of a disalignment that is not there and send the next reader after
		// the wrong thing.
		t.Fatalf("WorktreePath = %q, want the worktree the session acted in %q", state.WorktreePath, wtDir)
	}
	// Reached only with WorktreePath at the linked worktree, which is what makes
	// an empty WorktreeID a genuine disalignment rather than a correct pairing:
	// the identifier is the directory name under .git/worktrees/ and is empty
	// only for a main worktree.
	if state.WorktreeID == "" {
		t.Errorf("WorktreeID is empty while WorktreePath is the linked worktree %q: "+
			"the two have come apart, which is the disalignment "+
			"reconcileWorktreePathForResumedTurn refuses to create", state.WorktreePath)
	}
	wantHead := worktreeGit(t, wtDir, "rev-parse", "HEAD")
	if state.BaseCommit != wantHead {
		t.Errorf("BaseCommit = %q, want the worktree's HEAD %q", state.BaseCommit, wantHead)
	}
}

// TestWorktreeCapture_NonCanonicalCwdIsStoredAsGitSpellsIt pins that the root the
// override stores is canonicalised rather than taken verbatim from the payload.
//
// It matters because the stored value becomes the session's WorktreePath, and
// commit linking compares that against a root git resolved — exactWorktreeMatches
// compares the raw strings, so two spellings of one directory read as two
// worktrees and the session is never found. macOS makes this concrete every day
// (/var vs /private/var), and the same class appears on Windows with separators.
//
// The fixture uses a symlink to the worktree as the event's cwd, which is the
// portable way to produce a second spelling of one directory.
func TestWorktreeCapture_NonCanonicalCwdIsStoredAsGitSpellsIt(t *testing.T) {
	env := NewFeatureBranchEnv(t)
	env.InitEntire()
	wtDir := addLinkedWorktree(t, env, "fix/pr-3")

	link := filepath.Join(t.TempDir(), "link-to-wt")
	if err := os.Symlink(wtDir, link); err != nil {
		t.Skipf("symlinks unavailable here, so a second spelling cannot be produced: %v", err)
	}
	if link == wtDir {
		t.Fatal("fixture is wrong: the link and the worktree are the same string")
	}

	runner := NewHookRunner(env.RepoDir, env.ClaudeProjectDir, t)
	session := env.NewSession()
	if err := runner.SimulateUserPromptSubmitWithCwd(session.ID, "work", session.TranscriptPath, link); err != nil {
		t.Fatalf("prompt submit: %v", err)
	}

	state, err := env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("read session state: %v", err)
	}
	if state.WorktreePath == link {
		t.Errorf("WorktreePath stored the payload's spelling %q; commit linking compares "+
			"against git's spelling %q and would not match", link, wtDir)
	}
	if state.WorktreePath != wtDir {
		t.Errorf("WorktreePath = %q, want git's spelling of the worktree %q", state.WorktreePath, wtDir)
	}
}

// TestWorktreeCapture_ForeignRepoCwdIsRefused pins the gate. A working directory
// that resolves to a different repository must never redirect capture, however
// the event came to name it — the transcript decides what is written where, and
// event.CWD arrives in a hook payload.
func TestWorktreeCapture_ForeignRepoCwdIsRefused(t *testing.T) {
	env := NewFeatureBranchEnv(t)
	env.InitEntire()

	foreign := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(foreign); err == nil {
		foreign = resolved
	}
	testutil.InitRepo(t, foreign)
	testutil.WriteFile(t, foreign, "seed.txt", "seed\n")
	testutil.GitAdd(t, foreign, "seed.txt")
	testutil.GitCommit(t, foreign, "init")

	runner := NewHookRunner(env.RepoDir, env.ClaudeProjectDir, t)
	session := env.NewSession()

	if err := runner.SimulateUserPromptSubmitWithCwd(session.ID, "work", session.TranscriptPath, foreign); err != nil {
		t.Fatalf("prompt submit: %v", err)
	}
	env.WriteFile("main.go", "package main\n")
	session.CreateTranscript("work", []FileChange{{Path: filepath.Join(env.RepoDir, "main.go"), Content: "package main\n"}})
	if err := runner.SimulateStopWithCwd(session.ID, session.TranscriptPath, foreign); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if _, err := os.Stat(filepath.Join(foreign, ".entire")); err == nil {
		t.Error("capture followed a cwd in an unrelated repository: .entire was created there")
	}
	if _, err := os.Stat(filepath.Join(env.RepoDir, ".entire", "metadata", session.ID, "full.jsonl")); err != nil {
		t.Errorf("capture did not stay in the session's own repository: %v", err)
	}
}

// TestWorktreeCapture_NestedIndependentRepoCwdIsRefused is the sibling-repo gate
// in the shape it actually reaches users: a second, independent repository
// checked out INSIDE the parent's working tree (git-aggregator / vendored
// source layouts), not a sibling directory. The path is inside the parent
// worktree root, so a root comparison would accept it; only the git common dir
// separates them, which is why the gate compares that.
//
// Refusing is the current, deliberate behaviour, not a fix: capture for the
// nested repo is lost either way, because git status of the parent never
// reports a nested checkout's individual files. Routing a turn across a common
// dir would also split it from its session state, which lives per common dir.
// This test pins the boundary so that changing it is a decision rather than an
// accident.
//
// The same boundary from the other side, seen in a support case: enable Entire
// in BOTH repositories and the one session ID produces two state files, one per
// common dir, mutually invisible — and the nested one is then deleted by the
// orphan sweep in listAllSessionStates (strategy/manual_commit_session.go:117,
// no shadow branch, not active, no checkpoint ID) during the very commit hook
// that failed to find it. So relaxing this gate is not enough on its own: a turn
// routed across a common dir would write a checkpoint into a repository whose
// session state lives in the other one. The store split has to be solved with
// it, not after it.
func TestWorktreeCapture_NestedIndependentRepoCwdIsRefused(t *testing.T) {
	env := NewFeatureBranchEnv(t)
	env.InitEntire()

	nested := filepath.Join(env.RepoDir, "src", "vendored-addons")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatalf("create nested dir: %v", err)
	}
	testutil.InitRepo(t, nested)
	testutil.WriteFile(t, nested, "addon.py", "# vendored\n")
	testutil.GitAdd(t, nested, "addon.py")
	testutil.GitCommit(t, nested, "init nested")

	// Sanity: the nested repo really is inside the parent's worktree root and
	// really is a separate repository. Without both, this proves nothing.
	if !strings.HasPrefix(nested, env.RepoDir) {
		t.Fatalf("fixture is wrong: %s is not inside %s", nested, env.RepoDir)
	}
	if absCommonDir(t, nested) == absCommonDir(t, env.RepoDir) {
		t.Fatal("fixture is wrong: nested checkout shares the parent's common dir")
	}

	runner := NewHookRunner(env.RepoDir, env.ClaudeProjectDir, t)
	session := env.NewSession()
	if err := runner.SimulateUserPromptSubmitWithCwd(session.ID, "edit the addon", session.TranscriptPath, nested); err != nil {
		t.Fatalf("prompt submit: %v", err)
	}
	env.WriteFile("parent.go", "package main\n")
	session.CreateTranscript("edit the addon", []FileChange{{Path: filepath.Join(env.RepoDir, "parent.go"), Content: "package main\n"}})
	if err := runner.SimulateStopWithCwd(session.ID, session.TranscriptPath, nested); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if _, err := os.Stat(filepath.Join(nested, ".entire")); err == nil {
		t.Error("capture followed a cwd into a nested independent repository: .entire was created there")
	}
	if _, err := os.Stat(filepath.Join(env.RepoDir, ".entire", "metadata", session.ID, "full.jsonl")); err != nil {
		t.Errorf("capture did not stay in the session's own repository: %v", err)
	}
}

// absCommonDir returns dir's git common directory as an absolute path. git
// reports it relative to the directory it was asked from, so two unrelated
// repositories both answer ".git" from their own roots and compare equal — the
// mistake this helper exists to stop a test from making.
func absCommonDir(t *testing.T, dir string) string {
	t.Helper()
	out := worktreeGit(t, dir, "rev-parse", "--git-common-dir")
	if !filepath.IsAbs(out) {
		out = filepath.Join(dir, out)
	}
	resolved, err := filepath.EvalSymlinks(out)
	if err != nil {
		return filepath.Clean(out)
	}
	return resolved
}

// worktreeShadowBranchWithFile returns the first entire/* shadow branch whose
// tree contains path, or "" when none does.
func worktreeShadowBranchWithFile(t *testing.T, env *TestEnv, path string) string {
	t.Helper()
	for _, branch := range env.ListBranchesWithPrefix("entire/") {
		if branch == "entire/checkpoints/v1" {
			continue
		}
		listing := worktreeGit(t, env.RepoDir, "ls-tree", "-r", "--name-only", branch)
		for _, name := range strings.Split(listing, "\n") {
			if strings.TrimSpace(name) == path {
				return branch
			}
		}
	}
	return ""
}
