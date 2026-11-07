//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// These tests cover sessions that share one checkout (#3011). git status shows
// every dirty file in the worktree, not which session changed it, so a turn end
// must not take files the turn did not change: files already dirty when its
// prompt started, and files another session changed during an overlapping turn.

// newSharedCheckoutEnv returns a feature-branch repo with a.txt and b.txt
// committed and clean.
func newSharedCheckoutEnv(t *testing.T) *TestEnv {
	t.Helper()
	env := NewFeatureBranchEnv(t)
	env.WriteFile("a.txt", "a\n")
	env.WriteFile("b.txt", "b\n")
	env.GitAdd("a.txt", "b.txt")
	env.GitCommit("Add a.txt and b.txt")
	return env
}

func startTurn(t *testing.T, env *TestEnv, sess *Session) {
	t.Helper()
	if err := env.SimulateUserPromptSubmitWithTranscriptPath(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("SimulateUserPromptSubmit(%s) failed: %v", sess.ID, err)
	}
}

func endTurn(t *testing.T, env *TestEnv, sess *Session) {
	t.Helper()
	if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop(%s) failed: %v", sess.ID, err)
	}
}

func filesTouched(t *testing.T, env *TestEnv, sess *Session) []string {
	t.Helper()
	state, err := env.GetSessionState(sess.ID)
	if err != nil || state == nil {
		t.Fatalf("GetSessionState(%s) failed: %v (state=%v)", sess.ID, err, state)
	}
	files := slices.Clone(state.FilesTouched)
	slices.Sort(files)
	return files
}

// headCheckpointID returns HEAD's Entire-Checkpoint trailer, or "" when it
// has none. It lives in a file built on every platform because
// foreign_commit_linking_test.go, built only on linux and darwin, uses it too.
func headCheckpointID(t *testing.T, env *TestEnv) string {
	t.Helper()
	cpID, found := trailers.ParseCheckpoint(env.GetHeadCommitMessage())
	if !found {
		return ""
	}
	return cpID.String()
}

func headCheckpointSummary(t *testing.T, env *TestEnv) checkpoint.CheckpointSummary {
	t.Helper()
	cpID := headCheckpointID(t, env)
	if cpID == "" {
		t.Fatalf("HEAD should carry an Entire-Checkpoint trailer: %q", env.GetHeadCommitMessage())
	}
	content, found := env.ReadFileFromBranch(paths.MetadataBranchName, CheckpointSummaryPath(cpID))
	if !found {
		t.Fatalf("checkpoint summary %s not found", cpID)
	}
	var summary checkpoint.CheckpointSummary
	if err := json.Unmarshal([]byte(content), &summary); err != nil {
		t.Fatalf("parse checkpoint summary: %v", err)
	}
	return summary
}

// TestSharedCheckout_OverlappingTurnsClaimOnlyTheirOwnFiles is the issue's
// reproduction: two sessions start clean, edit different files during
// overlapping turns, and each commit must link only the session that edited
// the committed file.
func TestSharedCheckout_OverlappingTurnsClaimOnlyTheirOwnFiles(t *testing.T) {
	t.Parallel()
	env := newSharedCheckoutEnv(t)

	sessA := env.NewSession()
	sessB := env.NewSession()
	startTurn(t, env, sessA)
	startTurn(t, env, sessB)

	env.WriteFile("a.txt", "a, edited by A\n")
	sessA.CreateTranscript("edit a.txt", []FileChange{{Path: "a.txt", Content: "a, edited by A\n"}})
	env.WriteFile("b.txt", "b, edited by B\n")
	sessB.CreateTranscript("edit b.txt", []FileChange{{Path: "b.txt", Content: "b, edited by B\n"}})

	endTurn(t, env, sessA)
	endTurn(t, env, sessB)

	if got := filesTouched(t, env, sessA); !slices.Equal(got, []string{"a.txt"}) {
		t.Errorf("session A FilesTouched = %v, want [a.txt]", got)
	}
	if got := filesTouched(t, env, sessB); !slices.Equal(got, []string{"b.txt"}) {
		t.Errorf("session B FilesTouched = %v, want [b.txt]", got)
	}

	env.GitCommitWithHooks("Commit a.txt", "a.txt")
	summary := headCheckpointSummary(t, env)
	env.AssertCheckpointContainsSession(t, summary, sessA.ID)
	env.AssertCheckpointExcludesSession(t, summary, sessB.ID)

	env.GitCommitWithHooks("Commit b.txt", "b.txt")
	summary = headCheckpointSummary(t, env)
	env.AssertCheckpointContainsSession(t, summary, sessB.ID)
	env.AssertCheckpointExcludesSession(t, summary, sessA.ID)
}

// TestSharedCheckout_TranscriptNameInAnotherCaseIsClaimed covers a transcript
// that spells a path in another case than git does, as an agent on a
// case-insensitive filesystem (macOS, Windows) can: writing readme.md there
// changes README.md. During an overlap the turn keeps only the files its
// transcript names, and README.md must still count as named. It runs where
// the filesystem ignores case.
func TestSharedCheckout_TranscriptNameInAnotherCaseIsClaimed(t *testing.T) {
	t.Parallel()
	env := newSharedCheckoutEnv(t)
	env.WriteFile("README.md", "readme\n")
	env.GitAdd("README.md")
	env.GitCommit("Add README.md")
	requireCaseInsensitive(t, env, "README.md", "readme.md")

	sessA := env.NewSession()
	sessB := env.NewSession()
	startTurn(t, env, sessA)
	startTurn(t, env, sessB)

	env.WriteFile("a.txt", "a, edited by A\n")
	sessA.CreateTranscript("edit a.txt", []FileChange{{Path: "a.txt", Content: "a, edited by A\n"}})
	env.WriteFile("README.md", "readme, edited by B\n")
	sessB.CreateTranscript("edit the readme", []FileChange{{Path: "readme.md", Content: "readme, edited by B\n"}})

	endTurn(t, env, sessA)
	endTurn(t, env, sessB)

	if got := filesTouched(t, env, sessB); !slices.Contains(got, "README.md") {
		t.Fatalf("session B FilesTouched = %v, want README.md: its transcript names it as readme.md", got)
	}

	env.GitCommitWithHooks("Commit README.md", "README.md")
	summary := headCheckpointSummary(t, env)
	env.AssertCheckpointContainsSession(t, summary, sessB.ID)
	env.AssertCheckpointExcludesSession(t, summary, sessA.ID)
}

// TestSharedCheckout_DeletionNamedInAnotherCaseIsClaimed is the deletion side
// of the case rule: B's transcript names readme.md, B deletes it, and git
// lists README.md as deleted. With the file gone there is nothing to compare,
// so the worktree root decides, by finding its other entries under another
// case. It runs where the filesystem ignores case.
func TestSharedCheckout_DeletionNamedInAnotherCaseIsClaimed(t *testing.T) {
	t.Parallel()
	env := newSharedCheckoutEnv(t)
	env.WriteFile("README.md", "readme\n")
	env.GitAdd("README.md")
	env.GitCommit("Add README.md")
	requireCaseInsensitive(t, env, "README.md", "readme.md")

	sessA := env.NewSession()
	sessB := env.NewSession()
	startTurn(t, env, sessA)
	startTurn(t, env, sessB)

	env.WriteFile("a.txt", "a, edited by A\n")
	sessA.CreateTranscript("edit a.txt", []FileChange{{Path: "a.txt", Content: "a, edited by A\n"}})
	if err := os.Remove(filepath.Join(env.RepoDir, "README.md")); err != nil {
		t.Fatalf("remove README.md: %v", err)
	}
	sessB.CreateTranscript("drop the readme", []FileChange{{Path: "readme.md", Content: ""}})

	endTurn(t, env, sessA)
	endTurn(t, env, sessB)

	if got := filesTouched(t, env, sessB); !slices.Contains(got, "README.md") {
		t.Fatalf("session B FilesTouched = %v, want README.md: its transcript names it as readme.md", got)
	}

	env.GitRm("README.md")
	env.GitCommitStagedWithHooks("Remove README.md")
	summary := headCheckpointSummary(t, env)
	env.AssertCheckpointContainsSession(t, summary, sessB.ID)
	env.AssertCheckpointExcludesSession(t, summary, sessA.ID)
}

// requireCaseInsensitive skips the test unless the repository's filesystem
// ignores case, the macOS and Windows default.
func requireCaseInsensitive(t *testing.T, env *TestEnv, file, spelling string) {
	t.Helper()
	a, err := os.Lstat(filepath.Join(env.RepoDir, file))
	if err != nil {
		t.Fatalf("lstat %s: %v", file, err)
	}
	if b, err := os.Lstat(filepath.Join(env.RepoDir, spelling)); err != nil || !os.SameFile(a, b) {
		t.Skip("needs a case-insensitive filesystem; TestNamedFiles_OtherCaseIsAnotherFile covers the rule here")
	}
}

// TestSharedCheckout_StatusOnlyChangesDuringOverlapAreNotClaimed pins the
// trade-off of the overlap rule: a file only git status shows, changed while
// another session's turn was running, could be either session's, so neither
// takes it. A file the transcript names is still claimed.
func TestSharedCheckout_StatusOnlyChangesDuringOverlapAreNotClaimed(t *testing.T) {
	t.Parallel()
	env := newSharedCheckoutEnv(t)

	sessA := env.NewSession()
	sessB := env.NewSession()
	startTurn(t, env, sessA)
	startTurn(t, env, sessB)

	// A edits a.txt with its Edit tool and b.txt through a shell command.
	env.WriteFile("a.txt", "a, edited by A\n")
	env.WriteFile("b.txt", "b, edited by A's shell\n")
	env.WriteFile("new.txt", "created by A's shell\n")
	sessA.CreateTranscript("edit a.txt, then run a script", []FileChange{{Path: "a.txt", Content: "a, edited by A\n"}})
	sessB.CreateTranscript("read the docs", nil)

	endTurn(t, env, sessA)
	endTurn(t, env, sessB)

	if got := filesTouched(t, env, sessA); !slices.Equal(got, []string{"a.txt"}) {
		t.Errorf("session A FilesTouched = %v, want [a.txt]: status-only files during an overlap are ambiguous", got)
	}
	if got := filesTouched(t, env, sessB); len(got) != 0 {
		t.Errorf("session B FilesTouched = %v, want none", got)
	}
}

// TestSharedCheckout_FileDirtyBeforePromptIsNotClaimed covers a read-only
// session that starts while a tracked file is already dirty: it changed
// nothing, so a commit of that file must not link it.
func TestSharedCheckout_FileDirtyBeforePromptIsNotClaimed(t *testing.T) {
	t.Parallel()
	env := newSharedCheckoutEnv(t)
	env.WriteFile("a.txt", "a, edited by the user\n")

	sess := env.NewSession()
	startTurn(t, env, sess)
	sess.CreateTranscript("explain a.txt", nil)
	endTurn(t, env, sess)

	if got := filesTouched(t, env, sess); len(got) != 0 {
		t.Errorf("FilesTouched = %v, want none: a.txt was dirty before the prompt and the turn did not change it", got)
	}

	env.GitCommitWithHooks("Commit a.txt", "a.txt")
	if cpID := headCheckpointID(t, env); cpID != "" {
		t.Errorf("commit of a file the session did not change should carry no trailer, got checkpoint %s", cpID)
	}
}

// TestSharedCheckout_SequentialTurnDoesNotTakeEarlierSessionsFile covers the
// sequential case: A's turn ends before B's starts, so a.txt is already dirty
// in B's prompt-start baseline and stays A's alone.
func TestSharedCheckout_SequentialTurnDoesNotTakeEarlierSessionsFile(t *testing.T) {
	t.Parallel()
	env := newSharedCheckoutEnv(t)

	sessA := env.NewSession()
	startTurn(t, env, sessA)
	env.WriteFile("a.txt", "a, edited by A\n")
	sessA.CreateTranscript("edit a.txt", []FileChange{{Path: "a.txt", Content: "a, edited by A\n"}})
	endTurn(t, env, sessA)

	sessB := env.NewSession()
	startTurn(t, env, sessB)
	env.WriteFile("b.txt", "b, edited by B's shell\n")
	sessB.CreateTranscript("run the formatter on b.txt", nil)
	endTurn(t, env, sessB)

	if got := filesTouched(t, env, sessA); !slices.Equal(got, []string{"a.txt"}) {
		t.Errorf("session A FilesTouched = %v, want [a.txt]", got)
	}
	// B's shell edit is still found through git status: no other turn ran
	// during B's.
	if got := filesTouched(t, env, sessB); !slices.Equal(got, []string{"b.txt"}) {
		t.Errorf("session B FilesTouched = %v, want [b.txt]", got)
	}

	env.GitCommitWithHooks("Commit a.txt", "a.txt")
	summary := headCheckpointSummary(t, env)
	env.AssertCheckpointContainsSession(t, summary, sessA.ID)
	env.AssertCheckpointExcludesSession(t, summary, sessB.ID)
}

// TestSharedCheckout_ShellEditToDirtyFileIsStillClaimed guards the git status
// fallback's purpose (#2720): a shell edit to a tracked file, even one that
// was already dirty when the prompt started, is recorded when no other
// session's turn ran during this one.
func TestSharedCheckout_ShellEditToDirtyFileIsStillClaimed(t *testing.T) {
	t.Parallel()
	env := newSharedCheckoutEnv(t)
	env.WriteFile("a.txt", "a, edited by the user\n")

	sess := env.NewSession()
	startTurn(t, env, sess)
	env.WriteFile("a.txt", "a, edited by the user, then by the agent's shell\n")
	env.WriteFile("b.txt", "b, edited by the agent's shell\n")
	sess.CreateTranscript("run the formatter", nil)
	endTurn(t, env, sess)

	if got := filesTouched(t, env, sess); !slices.Equal(got, []string{"a.txt", "b.txt"}) {
		t.Errorf("FilesTouched = %v, want [a.txt b.txt]", got)
	}
}

// runForegroundTask runs a foreground Task call in sess's turn, with during
// standing for whatever happens in the worktree while the subagent runs, and
// returns the task record's files: none when the task changed nothing, which
// leaves no record.
func runForegroundTask(t *testing.T, env *TestEnv, sess *Session, toolUseID, agentID string, during func()) []string {
	t.Helper()
	sess.TranscriptBuilder.AddUserMessage("delegate to a subagent")
	sess.TranscriptBuilder.AddTaskToolUse(toolUseID, "do the work")
	if err := sess.TranscriptBuilder.WriteToFile(sess.TranscriptPath); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	if err := env.SimulatePreTask(sess.ID, sess.TranscriptPath, toolUseID); err != nil {
		t.Fatalf("SimulatePreTask failed: %v", err)
	}
	during()
	sess.TranscriptBuilder.AddTaskToolResult(toolUseID, agentID)
	if err := sess.TranscriptBuilder.WriteToFile(sess.TranscriptPath); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	if err := env.SimulatePostTask(PostTaskInput{SessionID: sess.ID, TranscriptPath: sess.TranscriptPath, ToolUseID: toolUseID, AgentID: agentID}); err != nil {
		t.Fatalf("SimulatePostTask failed: %v", err)
	}
	state, err := env.GetSessionState(sess.ID)
	if err != nil || state == nil {
		t.Fatalf("GetSessionState failed: %v (state=%v)", err, state)
	}
	rec := state.FindTaskRecord(toolUseID)
	if rec == nil {
		return nil
	}
	if rec.CompletedAt.IsZero() {
		t.Fatalf("task record %s not completed: %+v", toolUseID, rec)
	}
	files := slices.Clone(rec.Files)
	slices.Sort(files)
	return files
}

// TestSharedCheckout_ForegroundTaskClaimsOnlyItsOwnChanges covers the same
// narrowing for a foreground subagent, whose completion also reads git
// status: a file dirty before the task launched is not the task's, and while
// another session's turn runs, neither is a file only git status shows.
func TestSharedCheckout_ForegroundTaskClaimsOnlyItsOwnChanges(t *testing.T) {
	t.Parallel()
	env := newSharedCheckoutEnv(t)
	env.WriteFile("a.txt", "a, edited by the user\n")

	sess := env.NewSession()
	startTurn(t, env, sess)
	got := runForegroundTask(t, env, sess, "toolu_01Solo", "a1111222233334444", func() {
		env.WriteFile("task.txt", "created by the subagent's shell\n")
	})
	if !slices.Equal(got, []string{"task.txt"}) {
		t.Errorf("task record files = %v, want [task.txt]: a.txt was dirty before the task launched", got)
	}

	other := env.NewSession()
	startTurn(t, env, other)
	got = runForegroundTask(t, env, sess, "toolu_01Overlap", "a5555666677778888", func() {
		env.WriteFile("b.txt", "b, edited by the other session\n")
		other.CreateTranscript("edit b.txt", []FileChange{{Path: "b.txt", Content: "b, edited by the other session\n"}})
	})
	if len(got) != 0 {
		t.Errorf("task record files = %v, want none: b.txt changed while another session's turn ran", got)
	}
}

// TestSharedCheckout_ParentKeepsShellEditWhileItsWorkerRuns covers a Factory AI
// Droid Worker, which runs as a session of its own and is named in its
// parent's task record only once its first turn ends. While that turn still
// runs, it must not count as another session's overlapping turn for the parent
// that launched it, or the parent's own shell edits go unclaimed.
func TestSharedCheckout_ParentKeepsShellEditWhileItsWorkerRuns(t *testing.T) {
	t.Parallel()

	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire()
	env.WriteFile(".gitignore", ".entire/\n")
	env.WriteFile("README.md", "# Test Repository\n")
	env.GitAdd(".gitignore")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	parent := env.NewFactoryDroidSession()
	parent.CreateDroidTranscript("Run a Worker, then regenerate the README", nil)
	if err := env.SimulateFactoryDroidUserPromptSubmitWithTranscript(parent.ID, parent.TranscriptPath); err != nil {
		t.Fatalf("parent UserPromptSubmit failed: %v", err)
	}

	worker := env.NewFactoryDroidSession()
	worker.CreateDroidTranscript("# Task Tool Invocation", nil)
	worker.MarkAsWorkerSession(parent.ID, "toolu_worker_01", "worker: Summarize the repo")
	if err := env.SimulateFactoryDroidUserPromptSubmitWithTranscript(worker.ID, worker.TranscriptPath); err != nil {
		t.Fatalf("worker UserPromptSubmit failed: %v", err)
	}

	// A shell command in the parent's turn rewrites README.md; the parent's
	// transcript doesn't name it. The Worker's turn is still running.
	env.WriteFile("README.md", "# Test Repository\n\nRegenerated.\n")
	if err := env.SimulateFactoryDroidStop(parent.ID, parent.TranscriptPath); err != nil {
		t.Fatalf("parent Stop failed: %v", err)
	}

	state, err := env.GetSessionState(parent.ID)
	if err != nil || state == nil {
		t.Fatalf("GetSessionState(parent) failed: %v (state=%v)", err, state)
	}
	if !containsFile(state.FilesTouched, "README.md") {
		t.Errorf("the parent's shell edit must stay claimed while its own Worker runs, FilesTouched = %v", state.FilesTouched)
	}
}
