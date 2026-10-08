//go:build integration && (linux || darwin)

package integration

import (
	"encoding/json"
	"os/exec"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/stretchr/testify/require"
)

// A commit made by a process outside a running agent (a hookless agent, a
// script or queue job, a GUI client) must not pick up that agent's mid-turn
// session through the no-TTY fast path or post-commit's recency shortcut.
// Each case records the session's owner as a live process that is not an
// ancestor of the commit hook, which is what such a commit looks like.

// foreignActiveSession starts a Claude Code session that has edited
// claude_file.txt mid-turn, then records its owner as a live process outside
// the commit hook's ancestry.
func foreignActiveSession(t *testing.T, env *TestEnv) *Session {
	t.Helper()
	session := env.NewSession()
	require.NoError(t, env.SimulateUserPromptSubmitWithTranscriptPath(session.ID, session.TranscriptPath))
	env.WriteFile("claude_file.txt", "content from Claude")
	session.CreateTranscript("Create a file", []FileChange{{Path: "claude_file.txt", Content: "content from Claude"}})
	makeOwnerForeign(t, env, session.ID)
	return session
}

func makeOwnerForeign(t *testing.T, env *TestEnv, sessionID string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "sleep", "60")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Wait() }) //nolint:errcheck // killed by the test context
	owner, ok := proclive.IdentityOf(cmd.Process.Pid)
	require.True(t, ok, "IdentityOf(child) must resolve on supported platforms")

	state, err := env.GetSessionState(sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	state.Owner = &owner
	require.NoError(t, env.WriteSessionState(sessionID, state))
}

func headCheckpointID(t *testing.T, env *TestEnv) string {
	t.Helper()
	cpID, found := trailers.ParseCheckpoint(env.GetHeadCommitMessage())
	if !found {
		return ""
	}
	return cpID.String()
}

func TestForeignCommit_NoTTYUnrelatedFileIsNotLinked(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	foreignActiveSession(t, env)

	env.WriteFile("other_file.txt", "from a hookless agent")
	env.GitCommitWithShadowHooksAsAgent("hookless agent commit", "other_file.txt")

	require.Empty(t, headCheckpointID(t, env), "a commit of a file the session never touched, made outside its agent, must not carry its trailer")
}

func TestForeignCommit_NoTTYWithSessionFileIsLinked(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	foreignActiveSession(t, env)

	env.GitCommitWithShadowHooksAsAgent("commit the agent's file", "claude_file.txt")

	require.NotEmpty(t, headCheckpointID(t, env), "a commit containing the session's work still links through content detection")
}

func TestForeignCommit_InFlightTaskDoesNotLinkUnrelatedFile(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	session := foreignActiveSession(t, env)
	require.NoError(t, env.SimulatePreTask(session.ID, session.TranscriptPath, "toolu_inflight"))
	makeOwnerForeign(t, env, session.ID) // the task hook re-resolves state; keep the owner foreign

	env.WriteFile("other_file.txt", "from a hookless agent")
	env.GitCommitWithShadowHooksAsAgent("hookless agent commit", "other_file.txt")

	require.Empty(t, headCheckpointID(t, env), "an in-flight task record is not evidence the session made this commit")
}

func TestForeignCommit_AlwaysLinkingAtTerminalIsNotLinked(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	env.WriteSettings(map[string]any{"enabled": true, "commit_linking": "always"})
	foreignActiveSession(t, env)

	env.WriteFile("other_file.txt", "typed by a person")
	env.GitCommitWithShadowHooks("terminal commit", "other_file.txt")

	require.Empty(t, headCheckpointID(t, env), "commit_linking=always skips the prompt, not the evidence that the commit is the session's")
}

func TestForeignCommit_CallerSessionClaimKeepsFastPath(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	session := foreignActiveSession(t, env)
	// The agent's shell tool exports its session ID; the commit runs under it
	// even though its recorded owner is not in the hook's ancestry.
	env.ExtraEnv = append(env.ExtraEnv, "CLAUDE_CODE_SESSION_ID="+session.ID)

	env.WriteFile("shell_edit.txt", "written by the agent through its shell")
	env.GitCommitWithShadowHooksAsAgent("agent commit", "shell_edit.txt")

	require.NotEmpty(t, headCheckpointID(t, env), "a caller-session claim is positive evidence the commit is the agent's own")
}

func TestForeignCommit_OwnAgentNoTTYCommitStillLinks(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	session := env.NewSession()
	require.NoError(t, env.SimulateUserPromptSubmitWithTranscriptPath(session.ID, session.TranscriptPath))
	env.WriteFile("claude_file.txt", "content from Claude")
	session.CreateTranscript("Create a file", []FileChange{{Path: "claude_file.txt", Content: "content from Claude"}})
	// The recorded owner is the test binary, an ancestor of the commit hook:
	// the agent committing a file it changed through its shell.

	env.WriteFile("shell_edit.txt", "written by the agent through its shell")
	env.GitCommitWithShadowHooksAsAgent("agent commit", "shell_edit.txt")

	require.NotEmpty(t, headCheckpointID(t, env), "an agent's own no-TTY commit keeps the fast path")
}

func TestForeignCommit_PostCommitDoesNotCondenseForeignSession(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	foreign := foreignActiveSession(t, env)

	committer := env.NewSession()
	require.NoError(t, env.SimulateUserPromptSubmitWithTranscriptPath(committer.ID, committer.TranscriptPath))
	env.WriteFile("committer_file.txt", "content from the committing agent")
	committer.CreateTranscript("Create another file", []FileChange{{Path: "committer_file.txt", Content: "content from the committing agent"}})

	env.GitCommitWithShadowHooksAsAgent("committing agent's commit", "committer_file.txt")
	cpID := headCheckpointID(t, env)
	require.NotEmpty(t, cpID, "the committing agent's own commit links")

	summaryContent, found := env.ReadFileFromBranch(paths.MetadataBranchName, CheckpointSummaryPath(cpID))
	require.True(t, found, "checkpoint summary must exist")
	var summary checkpoint.CheckpointSummary
	require.NoError(t, json.Unmarshal([]byte(summaryContent), &summary))
	env.AssertCheckpointContainsSession(t, summary, committer.ID)
	env.AssertCheckpointExcludesSession(t, summary, foreign.ID)
}
