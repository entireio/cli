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

// TestUnregisteredCommonDirCheckout_LinkedHomeTurnIsNotRecorded resumes a
// session homed in a linked worktree in an unregistered checkout of the same
// repository. The checkout has the main worktree's empty ID and git never
// lists it, so the turn's files are not recorded (only its tokens), and the
// session isn't left pending on work no commit can carry. The user's commit
// there after the turn doesn't link the session; the agent's own commit during
// a turn there does, as a guest, and leaves the home's work pending.
func TestUnregisteredCommonDirCheckout_LinkedHomeTurnIsNotRecorded(t *testing.T) {
	t.Parallel()
	parent := NewRepoWithCommit(t)
	home := worktreeEnv(t, parent, "home")
	checkout := unregisteredCheckout(t, parent)
	checkoutDir := checkout.RepoDir

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
	before, err := parent.GetSessionState(sess.ID)
	if err != nil || before == nil {
		t.Fatalf("load session state after the home turn: %v", err)
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
	if entry := state.OtherWorktrees[session.WorktreeKey("")]; entry != nil {
		t.Fatalf("the checkout's turn was recorded: %+v", entry)
	}
	if slices.Contains(state.FilesTouched, "guest.go") {
		t.Fatalf("the checkout's file was recorded as the home's: %v", state.FilesTouched)
	}
	if !slices.Equal(state.FilesTouched, before.FilesTouched) || state.StepCount != before.StepCount {
		t.Fatalf("the home's record changed: FilesTouched %v -> %v, StepCount %d -> %d", before.FilesTouched, state.FilesTouched, before.StepCount, state.StepCount)
	}

	checkout.GitCommitWithHooks("Add guest file", "guest.go")
	if cpID := checkout.GetCheckpointIDFromCommitMessage(checkout.GetHeadHash()); cpID != "" {
		t.Fatalf("the user's commit in the checkout linked the session through an unrecorded turn: %s", cpID)
	}

	third := "Commit another checkout file"
	if err := checkout.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, third, sess.TranscriptPath); err != nil {
		t.Fatalf("user-prompt-submit in the checkout: %v", err)
	}
	checkout.WriteFile("agent.go", "package agent\n")
	sess.CreateTranscript(third, []FileChange{{Path: filepath.Join(checkoutDir, "agent.go"), Content: "package agent\n"}})
	checkout.GitCommitWithHooksAsAgent("Add agent file", "agent.go")
	if cpID := checkout.GetCheckpointIDFromCommitMessage(checkout.GetHeadHash()); cpID == "" {
		t.Fatalf("the agent's commit in the checkout has no Entire-Checkpoint trailer:\n%s", checkout.GetCommitMessage(checkout.GetHeadHash()))
	}
	state, err = parent.GetSessionState(sess.ID)
	if err != nil || state == nil {
		t.Fatalf("load session state after the agent's commit: %v", err)
	}
	if !slices.Contains(state.FilesTouched, "home.go") {
		t.Fatalf("the guest commit consumed the home's pending work: %v", state.FilesTouched)
	}
}

// TestUnregisteredCommonDirCheckout_MixedTurnCountsHomeStep runs one turn in
// an unregistered checkout that edits both the checkout and, by absolute path,
// the session's linked home. The checkout's step records only the turn's
// tokens, so the home's step is the turn's counted step: a session with no
// steps since its last checkpoint must not hold recorded files at StepCount 0.
func TestUnregisteredCommonDirCheckout_MixedTurnCountsHomeStep(t *testing.T) {
	t.Parallel()
	parent := NewRepoWithCommit(t)
	home := worktreeEnv(t, parent, "home")
	checkout := unregisteredCheckout(t, parent)

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
	home.GitCommitWithHooks("Add home file", "home.go")
	condensed, err := parent.GetSessionState(sess.ID)
	if err != nil || condensed == nil {
		t.Fatalf("load session state after the home commit: %v", err)
	}
	if condensed.StepCount != 0 || len(condensed.FilesTouched) != 0 {
		t.Fatalf("fixture: the home commit should leave no steps or files, got StepCount %d, FilesTouched %v", condensed.StepCount, condensed.FilesTouched)
	}

	second := "Edit the checkout and the home"
	if err := checkout.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, second, sess.TranscriptPath); err != nil {
		t.Fatalf("user-prompt-submit in the checkout: %v", err)
	}
	checkout.WriteFile("guest.go", "package guest\n")
	home.WriteFile("home2.go", "package home\n")
	sess.CreateTranscript(second, []FileChange{
		{Path: filepath.Join(checkout.RepoDir, "guest.go"), Content: "package guest\n"},
		{Path: filepath.Join(home.RepoDir, "home2.go"), Content: "package home\n"},
	})
	if err := checkout.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("stop in the checkout: %v", err)
	}

	state, err := parent.GetSessionState(sess.ID)
	if err != nil || state == nil {
		t.Fatalf("load session state: %v", err)
	}
	if !slices.Equal(state.FilesTouched, []string{"home2.go"}) {
		t.Fatalf("the home's record should hold only its own edit, got %v", state.FilesTouched)
	}
	if state.StepCount != 1 {
		t.Fatalf("the home's step was not counted: StepCount %d", state.StepCount)
	}
}

// unregisteredCheckout makes a checkout of parent's repository on a branch of
// its own, sharing parent's Git directory through commondir with no
// <common>/worktrees registration, and returns an environment for it.
func unregisteredCheckout(t *testing.T, parent *TestEnv) *TestEnv {
	t.Helper()
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
	checkout := *parent
	checkout.RepoDir = checkoutDir
	checkout.InitEntire()
	return &checkout
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
