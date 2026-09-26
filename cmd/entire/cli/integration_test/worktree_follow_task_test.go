//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
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
