//go:build integration

package integration

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// Two clones change the same pre-switch checkpoint on entire/checkpoints/v1:
// the desktop attaches a session to it, and the laptop's session saves into it
// again through an amend in the same turn. After the laptop switches to
// git-refs, neither v1 copy can become the checkpoint's ref without hiding the
// other's changes, so the laptop's save is refused. The work is not lost: it is
// saved with the next commit. The refusal must say so and point at
// migrate-checkpoints, which must not pick a copy without a person choosing.
func TestLegacyCheckpointDiverged_RefusedSaveIsReportedAndMigrateWontPickACopy(t *testing.T) {
	t.Parallel()

	laptop := NewFeatureBranchEnv(t)
	laptop.CheckpointStore = StoreGitBranch
	// Nobody is at a terminal, so notices meant for one land on stderr where
	// the test reads them, and commands take their non-interactive path.
	laptop.ExtraEnv = append(laptop.ExtraEnv, interactive.EnvTestTTY+"=0")
	sess := laptop.NewSession()
	agentTurn(t, laptop, sess, "first.go", "start the feature")
	laptop.GitCommitWithHooks("Add feature", "first.go")
	checkpointID := laptop.GetCheckpointIDFromCommitMessage(laptop.GetHeadHash())
	require.NotEmpty(t, checkpointID)

	bare := laptop.SetupBareRemote()
	laptop.GitPush("origin", paths.MetadataBranchName)

	desktop := laptop.CloneFrom(bare)
	testutil.RunGit(t, desktop.RepoDir, "branch", "--no-track", paths.MetadataBranchName, "origin/"+paths.MetadataBranchName)
	attachSession(t, desktop, "desktop-research")
	desktop.GitPush("origin", paths.MetadataBranchName)

	// The laptop saves into its checkpoint again without fetching first.
	sameTurn(t, laptop, sess, "laptop.go", "continue on the laptop")
	amend(laptop, "laptop.go")

	laptop.CheckpointStore = StoreGitRefs
	testutil.RunGit(t, laptop.RepoDir, "fetch", "origin")

	sameTurn(t, laptop, sess, "after-switch.go", "refused-work-marker")
	refused := amend(laptop, "after-switch.go")
	assert.Contains(t, refused, fmt.Sprintf(
		"[entire] Checkpoint %s was not saved. This commit's session work will be saved with your next commit instead. "+
			"Run 'entire doctor migrate-checkpoints' to fix it.", checkpointID))

	agentTurn(t, laptop, sess, "next.go", "next prompt")
	laptop.GitCommitWithHooks("Next commit", "next.go")
	next := laptop.GetCheckpointIDFromCommitMessage(laptop.GetHeadHash())
	require.NotEmpty(t, next)
	require.NotEqual(t, checkpointID, next)
	assert.Contains(t, checkpointRefsHolding(t, laptop, "refused-work-marker"), next,
		"the refused turn's work is saved with the next commit")

	// An agent's commit has no terminal for the notice, so doctor's scan names
	// the stuck checkpoint too.
	// Doctor's exit code reflects its other checks (the refused session is
	// still pending), so only the output is asserted.
	scan, scanErr := laptop.RunCLIWithError("doctor")
	t.Logf("doctor exit: %v", scanErr)
	assert.Contains(t, scan, "Legacy checkpoints: 1 needs a copy chosen")
	assert.Contains(t, scan, checkpointID+" differs between this clone and origin.")
	assert.Contains(t, scan, "Fix: run 'entire doctor migrate-checkpoints' to choose which copy to keep.")

	// After the switch, migrate-checkpoints resolves only checkpoints the write
	// path refused. Without a terminal it cannot ask which copy to keep, so it
	// lists the checkpoint and fails instead of guessing.
	out, err := laptop.RunCLIWithError("doctor", "migrate-checkpoints")
	require.Error(t, err, "migrate-checkpoints must not report success while a checkpoint still differs")
	assert.Contains(t, out, "1 checkpoint differs between this clone and origin and needs you to choose a copy:")
	assert.Contains(t, out, checkpointID)
	assert.Contains(t, out, "Run 'entire doctor migrate-checkpoints' in a terminal to choose.")
	refs := testutil.RunGit(t, laptop.RepoDir, "for-each-ref", "--format=%(refname)", checkpointRefPrefix)
	assert.NotContains(t, refs, checkpointID, "no ref is written without a person choosing a copy")

	// In a terminal the person chooses this clone's copy and declines the push.
	chosen := runCLIInTerminal(t, laptop, "1\nn\n", "doctor", "migrate-checkpoints")
	assert.Contains(t, chosen, fmt.Sprintf("Checkpoint %s differs between this clone and origin.", checkpointID))
	assert.Contains(t, chosen, "test-session-1 (changed), desktop-research",
		"the laptop's session differs on both sides, so origin's copy is not a superset")
	assert.Contains(t, chosen, "Wrote the chosen copy of 1 checkpoint(s) to refs.")
	refs = testutil.RunGit(t, laptop.RepoDir, "for-each-ref", "--format=%(refname)", checkpointRefPrefix)
	assert.Contains(t, refs, checkpointID, "the chosen copy becomes the checkpoint's ref")

	// With a ref in place, attaching to the checkpoint works again.
	attachSession(t, laptop, "laptop-research", "--commit", "HEAD~1")
}

// runCLIInTerminal runs the CLI as if in a terminal, answering its accessible
// prompts from input, and returns the combined output.
func runCLIInTerminal(t *testing.T, env *TestEnv, input string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), getTestBinary(), args...)
	cmd.Dir = env.RepoDir
	cmd.Env = append(env.cliEnv(), "ENTIRE_TEST_TTY=1", "ACCESSIBLE=1")
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "output:\n%s", out)
	return string(out)
}

// agentTurn runs a new turn of sess that writes file.
func agentTurn(t *testing.T, env *TestEnv, sess *Session, file, prompt string) {
	t.Helper()
	require.NoError(t, env.SimulateUserPromptSubmit(sess.ID))
	sameTurn(t, env, sess, file, prompt)
}

// sameTurn continues sess's current turn, writing file. Without a new prompt
// the session still holds its last checkpoint, so an amend saves into it again.
func sameTurn(t *testing.T, env *TestEnv, sess *Session, file, prompt string) {
	t.Helper()
	content := "package main\n\n// " + prompt + "\n"
	env.WriteFile(file, content)
	sess.CreateTranscript(prompt, []FileChange{{Path: file, Content: content}})
	require.NoError(t, env.SimulateStop(sess.ID, sess.TranscriptPath))
}

// amend amends HEAD with file as git does: post-commit, then post-rewrite. It
// returns the post-commit hook's output.
func amend(env *TestEnv, file string) string {
	env.T.Helper()
	before := env.GetHeadHash()
	out := env.GitCommitAmendWithHooks("Add feature", file)
	env.GitPostRewriteWithHooks("amend", [2]string{before, env.GetHeadHash()})
	return out
}

// attachSession attaches a hookless research session to HEAD's checkpoint, or
// to the commit args name.
func attachSession(t *testing.T, env *TestEnv, sessionID string, args ...string) {
	t.Helper()
	tb := NewTranscriptBuilder()
	tb.AddUserMessage("explain the feature")
	tb.AddAssistantMessage("It adds a feature.")
	require.NoError(t, tb.WriteToFile(filepath.Join(env.ClaudeProjectDir, sessionID+".jsonl")))
	out := env.RunCLI(append([]string{"session", "attach", sessionID, "-a", agentClaudeCode, "-f"}, args...)...)
	require.Contains(t, out, "Added to existing checkpoint")
}

// checkpointRefsHolding returns the checkpoint refs whose tree contains text.
func checkpointRefsHolding(t *testing.T, env *TestEnv, text string) string {
	t.Helper()
	var holding []string
	refs := testutil.RunGit(t, env.RepoDir, "for-each-ref", "--format=%(refname)", checkpointRefPrefix)
	for _, ref := range strings.Fields(refs) {
		cmd := exec.CommandContext(t.Context(), "git", "grep", "-q", "-F", text, ref)
		cmd.Dir = env.RepoDir
		cmd.Env = testutil.GitIsolatedEnv()
		if cmd.Run() == nil {
			holding = append(holding, ref)
		}
	}
	return strings.Join(holding, "\n")
}
