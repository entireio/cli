//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// TestUnregisteredCommonDirCheckout_CommitGetsCheckpoint drives the full
// capture flow in a checkout whose .git directory holds only its own HEAD and
// index and shares another repository's objects and refs through commondir,
// with no <common>/worktrees registration. Container launchers materialize
// checkouts this way over mounted host metadata, and Git accepts the layout
// without registration. Entire used to require the Git directory to be an
// immediate child of <common>/worktrees, so every repository open failed:
// session init and `checkpoint list` errored and commits got no trailer.
func TestUnregisteredCommonDirCheckout_CommitGetsCheckpoint(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)

	host := filepath.Join(t.TempDir(), "host")
	if err := os.MkdirAll(host, 0o755); err != nil {
		t.Fatalf("mkdir host: %v", err)
	}
	gitOutput(t, host, "init", "-b", "main")
	gitOutput(t, host, "config", "user.name", "Test User")
	gitOutput(t, host, "config", "user.email", "test@example.com")
	gitOutput(t, host, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(host, "README.md"), []byte("# host\n"), 0o644); err != nil {
		t.Fatalf("write host README: %v", err)
	}
	gitOutput(t, host, "add", "README.md")
	gitOutput(t, host, "commit", "-m", "Initial host commit")
	hostCommon := filepath.Join(host, ".git")

	checkoutGitDir := filepath.Join(env.RepoDir, ".git")
	if err := os.MkdirAll(checkoutGitDir, 0o755); err != nil {
		t.Fatalf("mkdir checkout .git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(checkoutGitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("write checkout HEAD: %v", err)
	}
	if err := os.WriteFile(filepath.Join(checkoutGitDir, "commondir"), []byte(hostCommon+"\n"), 0o644); err != nil {
		t.Fatalf("write checkout commondir: %v", err)
	}
	gitOutput(t, env.RepoDir, "read-tree", "-u", "--reset", "HEAD")

	// Preconditions: Git treats this as a working checkout of the host's
	// repository, and nothing registers it.
	if got := gitOutput(t, env.RepoDir, "rev-parse", "--path-format=absolute", "--git-common-dir"); !sameDir(t, got, hostCommon) {
		t.Fatalf("git common dir = %q, want %q", got, hostCommon)
	}
	if _, err := os.Stat(filepath.Join(hostCommon, "worktrees")); !os.IsNotExist(err) {
		t.Fatalf("host must have no worktrees registration directory, stat err = %v", err)
	}

	env.RunCLI("enable", "--agent", "claude-code", "--telemetry=false")
	initialHead := gitOutput(t, env.RepoDir, "rev-parse", "HEAD")

	sess := env.NewSession()
	prompt := "Create app.txt"
	if err := env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, prompt, sess.TranscriptPath); err != nil {
		t.Fatalf("user-prompt-submit: %v", err)
	}
	env.WriteFile("app.txt", "hello\n")
	sess.CreateTranscript(prompt, []FileChange{{Path: "app.txt", Content: "hello\n"}})
	if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// Session state lives in the shared common directory, i.e. the host's.
	// env.GetSessionState assumes <RepoDir>/.git is the common dir, so read it
	// directly.
	data, err := os.ReadFile(filepath.Join(hostCommon, "entire-sessions", sess.ID+".json"))
	if err != nil {
		t.Fatalf("no session state in the host common dir — session init failed: %v", err)
	}
	var state strategy.SessionState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("parse session state: %v", err)
	}
	if state.StepCount == 0 || !slices.Contains(state.FilesTouched, "app.txt") {
		t.Fatalf("expected a recorded turn-end step touching app.txt, got StepCount=%d FilesTouched=%v", state.StepCount, state.FilesTouched)
	}

	env.GitCommitWithHooks("Add app", "app.txt")
	head := gitOutput(t, env.RepoDir, "rev-parse", "HEAD")
	if head == initialHead {
		t.Fatal("expected the commit to advance HEAD")
	}
	if hostHead := gitOutput(t, host, "rev-parse", "main"); hostHead != head {
		t.Fatalf("host main = %s, want the checkout's commit %s (refs are shared)", hostHead, head)
	}

	checkpointID := env.GetCheckpointIDFromCommitMessage(head)
	if checkpointID == "" {
		t.Fatalf("commit has no Entire-Checkpoint trailer:\n%s", env.GetCommitMessage(head))
	}
	if listOut := env.RunCLI("checkpoint", "list"); !strings.Contains(listOut, checkpointID) {
		t.Fatalf("checkpoint list missing checkpoint %s:\n%s", checkpointID, listOut)
	}
}

func sameDir(t *testing.T, a, b string) bool {
	t.Helper()
	aInfo, err := os.Stat(a)
	if err != nil {
		t.Fatalf("stat %s: %v", a, err)
	}
	bInfo, err := os.Stat(b)
	if err != nil {
		t.Fatalf("stat %s: %v", b, err)
	}
	return os.SameFile(aInfo, bInfo)
}

// TestUnregisteredCommonDirCheckout_GuestTurnCommitGetsCheckpoint resumes a
// session homed in a linked worktree in an unregistered checkout of the same
// repository. The turn records the checkout's file under the empty worktree ID,
// and the turn end's pruning must keep that record although git worktree list
// never shows the checkout (and lists the main worktree under that ID), or the
// checkout's commit gets no trailer.
func TestUnregisteredCommonDirCheckout_GuestTurnCommitGetsCheckpoint(t *testing.T) {
	t.Parallel()
	parent := NewRepoWithCommit(t)
	home := worktreeEnv(t, parent, "home")

	base := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(base); err == nil {
		base = resolved
	}
	checkoutDir := filepath.Join(base, "checkout")
	gitOutput(t, parent.RepoDir, "branch", "checkout-branch")
	if err := os.MkdirAll(filepath.Join(checkoutDir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir checkout .git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(checkoutDir, ".git", "HEAD"), []byte("ref: refs/heads/checkout-branch\n"), 0o644); err != nil {
		t.Fatalf("write checkout HEAD: %v", err)
	}
	if err := os.WriteFile(filepath.Join(checkoutDir, ".git", "commondir"), []byte(filepath.Join(parent.RepoDir, ".git")+"\n"), 0o644); err != nil {
		t.Fatalf("write checkout commondir: %v", err)
	}
	gitOutput(t, checkoutDir, "read-tree", "-u", "--reset", "HEAD")
	checkoutEnv := *parent
	checkoutEnv.RepoDir = checkoutDir
	checkoutEnv.InitEntire()
	checkout := &checkoutEnv

	sess := home.NewSession()
	first := "Create the home file"
	if err := home.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, first, sess.TranscriptPath); err != nil {
		t.Fatalf("user-prompt-submit at home: %v", err)
	}
	home.WriteFile("home.go", "package home\n")
	sess.CreateTranscript(first, []FileChange{{Path: filepath.Join(home.RepoDir, "home.go"), Content: "package home\n"}})
	if err := home.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("stop at home: %v", err)
	}

	second := "Create the checkout file"
	if err := checkout.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, second, sess.TranscriptPath); err != nil {
		t.Fatalf("user-prompt-submit in the checkout: %v", err)
	}
	checkout.WriteFile("guest.go", "package guest\n")
	sess.CreateTranscript(second, []FileChange{{Path: filepath.Join(checkoutDir, "guest.go"), Content: "package guest\n"}})
	if err := checkout.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("stop in the checkout: %v", err)
	}

	state, err := parent.GetSessionState(sess.ID)
	if err != nil || state == nil {
		t.Fatalf("load session state: %v", err)
	}
	if entry := state.OtherWorktrees[session.WorktreeKey("")]; entry == nil || !slices.Contains(entry.FilesTouched, "guest.go") {
		t.Fatalf("turn end dropped the checkout's record: %+v", state.OtherWorktrees)
	}

	checkout.GitCommitWithHooks("Add guest file", "guest.go")
	if cpID := checkout.GetCheckpointIDFromCommitMessage(checkout.GetHeadHash()); cpID == "" {
		t.Fatalf("checkout commit has no Entire-Checkpoint trailer:\n%s", checkout.GetCommitMessage(checkout.GetHeadHash()))
	}
}
