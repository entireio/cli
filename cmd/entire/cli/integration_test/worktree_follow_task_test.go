//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// followPayload is a Claude Code hook payload carrying the agent's cwd.
func followPayload(sessionID, transcriptPath, cwd string, extra map[string]any) map[string]any {
	p := map[string]any{"session_id": sessionID, "transcript_path": transcriptPath, "cwd": cwd}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

func requireNoTmpState(t *testing.T, env *TestEnv, name string) {
	t.Helper()
	_, err := os.Stat(filepath.Join(env.RepoDir, ".entire", "tmp", name))
	require.ErrorIs(t, err, os.ErrNotExist, "%s must not be left behind in %s", name, env.RepoDir)
}

// The agent enters a worktree mid-turn (EnterWorktree): the turn starts in the
// parent and ends in the worktree. The turn's work must be captured in the
// worktree without the worktree's pre-existing untracked files being claimed.
func TestFollow_MidTurnMoveCapturesOnlyTheTurnsWork(t *testing.T) {
	t.Parallel()
	parent := NewRepoWithCommit(t)
	feature := worktreeEnv(t, parent, "feature")
	feature.WriteFile("preexisting.txt", "not the agent's\n")

	sess := parent.NewSession()
	hooks := NewHookRunner(parent.RepoDir, parent.ClaudeProjectDir, t)
	prompt := "work in a worktree"
	require.NoError(t, hooks.runHookWithInput("user-prompt-submit", followPayload(sess.ID, sess.TranscriptPath, parent.RepoDir, map[string]any{"prompt": prompt})))

	feature.WriteFile("feature.txt", "work\n")
	sess.CreateTranscript(prompt, []FileChange{{Path: "feature.txt", Content: "work\n"}})
	require.NoError(t, hooks.runHookWithInput("stop", followPayload(sess.ID, sess.TranscriptPath, feature.RepoDir, nil)))

	state, err := parent.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.Equal(t, feature.RepoDir, state.WorktreePath, "the session follows the agent at turn end")
	require.Contains(t, state.FilesTouched, "feature.txt")
	require.NotContains(t, state.FilesTouched, "preexisting.txt", "an untracked file that predates the turn is not the agent's work")
	requireNoTmpState(t, parent, "pre-prompt-"+sess.ID+".json")
	require.Empty(t, state.TurnWorktreePath, "stale after the turn: must not steer the next turn's baseline lookup")

	promptFile := func(env *TestEnv) string {
		return filepath.Join(env.RepoDir, ".entire", "metadata", sess.ID, "prompt.txt")
	}
	carried, err := os.ReadFile(promptFile(feature))
	require.NoError(t, err, "the turn's prompt moves with the agent")
	require.Equal(t, prompt, string(carried))
	_, err = os.Stat(promptFile(parent))
	require.ErrorIs(t, err, os.ErrNotExist, "the prompt is carried, not copied")
}

// A subagent is launched from the parent and completes in a worktree: its
// record holds only its own files and its launch baseline is consumed.
func TestFollow_TaskStartedInParentCompletesInWorktree(t *testing.T) {
	t.Parallel()
	parent := NewRepoWithCommit(t)
	feature := worktreeEnv(t, parent, "feature")
	feature.WriteFile("preexisting.txt", "not the subagent's\n")

	sess := parent.NewSession()
	hooks := NewHookRunner(parent.RepoDir, parent.ClaudeProjectDir, t)
	require.NoError(t, hooks.runHookWithInput("user-prompt-submit", followPayload(sess.ID, sess.TranscriptPath, parent.RepoDir, map[string]any{"prompt": "delegate"})))
	const toolUseID = "toolu_follow_across"
	require.NoError(t, hooks.runHookWithInput("pre-task", followPayload(sess.ID, sess.TranscriptPath, parent.RepoDir, map[string]any{"tool_use_id": toolUseID, "tool_input": map[string]string{"subagent_type": "dev"}})))

	feature.WriteFile("sub.txt", "subagent work\n")
	sess.CreateSubagentTranscript("a1", []FileChange{{Path: filepath.Join(feature.RepoDir, "sub.txt"), Content: "subagent work\n"}})
	require.NoError(t, hooks.runHookWithInput("post-task", followPayload(sess.ID, sess.TranscriptPath, feature.RepoDir, map[string]any{"tool_use_id": toolUseID, "tool_input": map[string]any{}, "tool_response": map[string]string{"agentId": "a1"}})))

	state, err := parent.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.Len(t, state.TaskRecords, 1)
	require.Equal(t, []string{"sub.txt"}, state.TaskRecords[0].Files)
	requireNoTmpState(t, parent, "pre-task-"+toolUseID+".json")
}

// A session whose home still holds an uncommitted step cannot be re-homed, but
// its hooks now follow the agent into the worktree it works in. Writing that
// turn's step anyway put the worktree's files onto the home's shadow branch
// and moved its base to the worktree's HEAD. The step must be skipped, as it
// was when hooks never left the home tree.
func TestFollow_RefusedRehomeWritesNothingIntoTheHome(t *testing.T) {
	t.Parallel()
	parent := NewRepoWithCommit(t)
	feature := worktreeEnv(t, parent, "feature")
	feature.WriteFile("feature-base.txt", "feature\n")
	testutil.RunGit(t, feature.RepoDir, "add", "feature-base.txt")
	testutil.RunGit(t, feature.RepoDir, "-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "feature base")

	sess := parent.NewSession()
	hooks := NewHookRunner(parent.RepoDir, parent.ClaudeProjectDir, t)
	require.NoError(t, hooks.runHookWithInput("user-prompt-submit", followPayload(sess.ID, sess.TranscriptPath, parent.RepoDir, map[string]any{"prompt": "edit a"})))
	parent.WriteFile("a.txt", "a\n")
	sess.CreateTranscript("edit a", []FileChange{{Path: "a.txt", Content: "a\n"}})
	require.NoError(t, hooks.runHookWithInput("stop", followPayload(sess.ID, sess.TranscriptPath, parent.RepoDir, nil)))
	before, err := parent.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.Equal(t, 1, before.StepCount, "precondition: the home holds an uncommitted step")

	require.NoError(t, hooks.runHookWithInput("user-prompt-submit", followPayload(sess.ID, sess.TranscriptPath, feature.RepoDir, map[string]any{"prompt": "edit b"})))
	feature.WriteFile("b.txt", "b\n")
	sess.CreateTranscript("edit b", []FileChange{{Path: filepath.Join(feature.RepoDir, "b.txt"), Content: "b\n"}})
	require.NoError(t, hooks.runHookWithInput("stop", followPayload(sess.ID, sess.TranscriptPath, feature.RepoDir, nil)))

	after, err := parent.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.Equal(t, parent.RepoDir, after.WorktreePath, "the home's step keeps the session there")
	require.Equal(t, before.BaseCommit, after.BaseCommit, "the home's shadow branch must stay keyed to the home's HEAD")
	require.Equal(t, 1, after.StepCount, "no step from another tree on the home's branch")
	require.NotContains(t, after.FilesTouched, "b.txt")
	require.NotNil(t, after.CaptureDegradedAt, "the uncaptured turn ends degraded, not as a clean capture")
	require.NotEmpty(t, before.Branch)
	require.Equal(t, before.Branch, after.Branch, "resume maps the session to its home's branch, not the worktree's")
}

// A subagent working in a worktree records incremental TodoWrite checkpoints
// there. post-todo bypasses the lifecycle dispatcher, so it never followed the
// payload cwd: it looked for the task baseline and ran git status in the
// launch worktree, found no active task, and recorded nothing.
func TestFollow_SubagentTodoCheckpointsInItsWorktree(t *testing.T) {
	t.Parallel()
	// The pre-task hook's cwd is the parent agent's, so a subagent launched from
	// the parent has its task baseline in the parent's tree while it works in
	// the worktree; one launched from inside the worktree has it there.
	for name, launchedFromParent := range map[string]bool{"launched from the parent": true, "launched in the worktree": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			parent := NewRepoWithCommit(t)
			feature := worktreeEnv(t, parent, "feature")

			sess := parent.NewSession()
			hooks := NewHookRunner(parent.RepoDir, parent.ClaudeProjectDir, t)
			require.NoError(t, hooks.runHookWithInput("user-prompt-submit", followPayload(sess.ID, sess.TranscriptPath, parent.RepoDir, map[string]any{"prompt": "delegate"})))
			const toolUseID = "toolu_follow_todo"
			taskCWD := feature.RepoDir
			if launchedFromParent {
				taskCWD = parent.RepoDir
			}
			require.NoError(t, hooks.runHookWithInput("pre-task", followPayload(sess.ID, sess.TranscriptPath, taskCWD, map[string]any{"tool_use_id": toolUseID, "tool_input": map[string]string{"subagent_type": "dev"}})))

			// A tracked edit: with the baseline in the parent's tree, which
			// cannot say which of this tree's untracked files are new, new-file
			// detection is off by design (pre-existing files are never claimed).
			feature.WriteFile("README.md", "subagent work\n")
			require.NoError(t, hooks.runHookWithInput("post-todo", followPayload(sess.ID, sess.TranscriptPath, feature.RepoDir, map[string]any{
				"tool_name":     "TodoWrite",
				"tool_use_id":   "toolu_follow_todo_write",
				"tool_input":    map[string]any{"todos": []map[string]string{{"content": "write sub.txt", "status": "completed", "activeForm": "writing"}}},
				"tool_response": map[string]any{},
			})))

			verifyIncrementalCheckpointStorage(t, feature, sess.ID, toolUseID)
		})
	}
}

// A subagent that runs entirely in a worktree leaves task content there; at
// turn end in that worktree the session must follow its pending work rather
// than stay homed in a parent that holds none of it.
func TestFollow_TaskInWorktreeDoesNotPinSessionToParent(t *testing.T) {
	t.Parallel()
	parent := NewRepoWithCommit(t)
	feature := worktreeEnv(t, parent, "feature")

	sess := parent.NewSession()
	hooks := NewHookRunner(parent.RepoDir, parent.ClaudeProjectDir, t)
	require.NoError(t, hooks.runHookWithInput("user-prompt-submit", followPayload(sess.ID, sess.TranscriptPath, parent.RepoDir, map[string]any{"prompt": "delegate"})))
	const toolUseID = "toolu_follow_inside"
	require.NoError(t, hooks.runHookWithInput("pre-task", followPayload(sess.ID, sess.TranscriptPath, feature.RepoDir, map[string]any{"tool_use_id": toolUseID, "tool_input": map[string]string{"subagent_type": "dev"}})))
	feature.WriteFile("sub.txt", "subagent work\n")
	require.NoError(t, hooks.runHookWithInput("post-task", followPayload(sess.ID, sess.TranscriptPath, feature.RepoDir, map[string]any{"tool_use_id": toolUseID, "tool_input": map[string]any{}, "tool_response": map[string]string{"agentId": "a1"}})))

	sess.CreateTranscript("delegate", nil)
	require.NoError(t, hooks.runHookWithInput("stop", followPayload(sess.ID, sess.TranscriptPath, feature.RepoDir, nil)))

	state, err := parent.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.Equal(t, feature.RepoDir, state.WorktreePath, "the parent holds none of the pending work")
}

// Codex reports edits per tool use with its working directory. Files recorded
// in the worktree the agent moved to are located there, so they never pin the
// session to the parent; files recorded in both trees keep it where it is.
func TestFollow_CodexToolUseLocatesItsFiles(t *testing.T) {
	t.Parallel()
	parent := NewRepoWithCommit(t)
	feature := worktreeEnv(t, parent, "feature")
	const sessionID = "test-codex-follow-tool-use"
	require.NoError(t, parent.WriteSessionState(sessionID, &strategy.SessionState{
		SessionID:    sessionID,
		AgentType:    "Codex",
		BaseCommit:   parent.GetHeadHash(),
		WorktreePath: parent.RepoDir,
		StartedAt:    time.Now(),
		Phase:        session.PhaseActive,
	}))
	codex := NewCodexHookRunner(parent.RepoDir, t)
	addFile := func(name string) string {
		return "*** Begin Patch\n*** Add File: " + name + "\n+x\n*** End Patch\n"
	}

	require.NoError(t, codex.SimulateCodexPostToolUseApplyPatch(sessionID, feature.RepoDir, addFile("wt.txt")))
	state, err := parent.GetSessionState(sessionID)
	require.NoError(t, err)
	require.Equal(t, []string{"wt.txt"}, state.FilesTouched)
	require.Equal(t, feature.RepoDir, state.PendingContentWorktree, "the edit was recorded in the worktree the agent works in")

	require.NoError(t, codex.SimulateCodexPostToolUseApplyPatch(sessionID, parent.RepoDir, addFile("home.txt")))
	state, err = parent.GetSessionState(sessionID)
	require.NoError(t, err)
	require.Equal(t, session.PendingContentInSeveralWorktrees, state.PendingContentWorktree, "content in both trees must hold the session where it is")
}

// A session that cannot re-home (its home holds saved steps) still has this
// turn's prompt carried to the worktree the turn ended in, while the prompts of
// the steps saved at home stay there.
func TestFollow_MidTurnMoveCarriesOnlyThisTurnsPrompt(t *testing.T) {
	t.Parallel()
	parent := NewRepoWithCommit(t)
	feature := worktreeEnv(t, parent, "feature")

	sess := parent.NewSession()
	hooks := NewHookRunner(parent.RepoDir, parent.ClaudeProjectDir, t)
	require.NoError(t, hooks.runHookWithInput("user-prompt-submit", followPayload(sess.ID, sess.TranscriptPath, parent.RepoDir, map[string]any{"prompt": "first"})))
	parent.WriteFile("home.txt", "home\n")
	sess.CreateTranscript("first", []FileChange{{Path: "home.txt", Content: "home\n"}})
	require.NoError(t, hooks.runHookWithInput("stop", followPayload(sess.ID, sess.TranscriptPath, parent.RepoDir, nil)))

	require.NoError(t, hooks.runHookWithInput("user-prompt-submit", followPayload(sess.ID, sess.TranscriptPath, parent.RepoDir, map[string]any{"prompt": "second"})))
	feature.WriteFile("feature.txt", "work\n")
	sess.CreateTranscript("second", []FileChange{{Path: filepath.Join(feature.RepoDir, "feature.txt"), Content: "work\n"}})
	require.NoError(t, hooks.runHookWithInput("stop", followPayload(sess.ID, sess.TranscriptPath, feature.RepoDir, nil)))

	state, err := parent.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.Equal(t, parent.RepoDir, state.WorktreePath, "precondition: saved steps keep the session home")
	read := func(env *TestEnv) string {
		data, err := os.ReadFile(filepath.Join(env.RepoDir, ".entire", "metadata", sess.ID, "prompt.txt"))
		require.NoError(t, err)
		return string(data)
	}
	require.Equal(t, "first", read(parent), "prompts of steps saved at home stay there")
	require.Equal(t, "second", read(feature), "only this turn's prompt moves")
}
