//go:build integration

package integration

// Only a commit's final trailer block links it to a checkpoint, and post-commit
// creates only a checkpoint prepare-commit-msg stamped. A checkpoint ID
// anywhere else in the message (pasted, quoted, or left over from another
// commit) names a checkpoint the commit did not stamp, so the hooks must stamp
// a fresh trailer and condense the session's work there.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

func TestTrailerAuthority_BodyLineNamingUnknownCheckpoint(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	authorityAgentTurn(env, "mine.txt", "my agent work\n")

	_, final := advCommit(env,
		"Fix the bug\n\nEntire-Checkpoint: "+advForeignCheckpoint+"\n\nSigned-off-by: Test User <test@example.com>\n",
		"message", keepAll, "mine.txt")

	stamped := requireFreshFinalTrailer(t, final, advForeignCheckpoint)
	require.True(t, advCheckpointStored(env, stamped), "the session's work belongs in the checkpoint prepare stamped")
	require.False(t, advCheckpointStored(env, advForeignCheckpoint),
		"a body line must not claim a checkpoint ID this clone does not have")
}

func TestTrailerAuthority_IndentedLineNamingUnknownCheckpoint(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	authorityAgentTurn(env, "mine.txt", "my agent work\n")

	_, final := advCommit(env, "Fix the bug\n\n  Entire-Checkpoint: "+advForeignCheckpoint+"\n",
		"message", keepAll, "mine.txt")

	stamped := requireFreshFinalTrailer(t, final, advForeignCheckpoint)
	require.True(t, advCheckpointStored(env, stamped), "the session's work belongs in the checkpoint prepare stamped")
	require.False(t, advCheckpointStored(env, advForeignCheckpoint),
		"an indented line is not a trailer, so it must not claim a checkpoint ID")
}

func TestTrailerAuthority_BodyLineNamingExistingCheckpoint(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	authorityAgentTurn(env, "first.txt", "first change\n")
	env.GitCommitWithHooks("First change", "first.txt")
	existing := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	require.NotEmpty(t, existing, "precondition: the first commit is linked")
	before, ok := checkpointBlob(env, existing, paths.MetadataFileName)
	require.True(t, ok, "precondition: the first checkpoint is stored")

	authorityAgentTurn(env, "second.txt", "second change\n")
	_, final := advCommit(env,
		"Follow-up\n\nEntire-Checkpoint: "+existing+"\n\nSigned-off-by: Test User <test@example.com>\n",
		"message", keepAll, "second.txt")

	stamped := requireFreshFinalTrailer(t, final, existing)
	require.True(t, advCheckpointStored(env, stamped), "the follow-up commit gets a checkpoint of its own")
	after, ok := checkpointBlob(env, existing, paths.MetadataFileName)
	require.True(t, ok)
	require.Equal(t, before, after, "the checkpoint the body mentions must not change")
}

func TestTrailerAuthority_AgentCommitWithBodyLine(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	sess := env.NewSession()
	require.NoError(t, env.SimulateUserPromptSubmitWithTranscriptPath(sess.ID, sess.TranscriptPath))
	env.WriteFile("agent.txt", "agent work\n")
	sess.CreateTranscript("write agent.txt", []FileChange{{Path: "agent.txt", Content: "agent work\n"}})

	env.GitCommitWithHooksAsAgent(
		"Agent change\n\nEntire-Checkpoint: "+advForeignCheckpoint+"\n\nSigned-off-by: Test User <test@example.com>\n",
		"agent.txt")

	stamped := requireFreshFinalTrailer(t, env.GetCommitMessage(env.GetHeadHash()), advForeignCheckpoint)
	require.True(t, advCheckpointStored(env, stamped), "the agent's work belongs in the checkpoint prepare stamped")
	require.False(t, advCheckpointStored(env, advForeignCheckpoint),
		"a body line must not claim a checkpoint ID this clone does not have")
}

// `git commit -C <sha>` reuses another commit's message, its final trailer
// block included. A teammate's checkpoint this clone has not fetched is not one
// prepare-commit-msg stamped, so post-commit must not create it with this
// session's work.
func TestTrailerAuthority_ReusedMessageNamingUnfetchedCheckpoint(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	git := gitWithEntireCommitHooksEnv(t, env)
	env.WriteFile("teammate.txt", "teammate work\n")
	env.GitAdd("teammate.txt")
	env.GitCommitWithCheckpointID("Teammate change", advForeignCheckpoint)
	teammate := env.GetHeadHash()

	authorityAgentTurn(env, "mine.txt", "my agent work\n")
	_, err := git(nil, "add", "mine.txt")
	require.NoError(t, err)
	out, err := git(nil, "commit", "--no-gpg-sign", "-C", teammate)
	require.NoError(t, err, out)

	require.False(t, advCheckpointStored(env, advForeignCheckpoint),
		"a reused message's trailer must not claim a checkpoint ID this clone does not have")
	require.Equal(t, "Teammate change\n\nEntire-Checkpoint: "+advForeignCheckpoint+"\n", env.GetCommitMessage(env.GetHeadHash()),
		"a reused message keeps its trailer and gets no stamp, so the commit stays unlinked")
}

// A trailer typed in the editor in place of the one prepare-commit-msg stamped
// names a checkpoint prepare did not stamp; post-commit must not create it.
func TestTrailerAuthority_TrailerReplacedInTheEditorNamingUnfetchedCheckpoint(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	git := gitWithEntireCommitHooksEnv(t, env)
	editors := t.TempDir()
	writeScripts(t, editors, map[string]string{
		"replace": "#!/bin/sh\nsed 's/^Entire-Checkpoint: .*/Entire-Checkpoint: " + advForeignCheckpoint + "/' \"$1\" > \"$1.new\" && mv \"$1.new\" \"$1\"\n",
	})
	authorityAgentTurn(env, "mine.txt", "my agent work\n")
	_, err := git(nil, "add", "mine.txt")
	require.NoError(t, err)

	editor := "GIT_EDITOR=" + shellQuote(filepath.ToSlash(filepath.Join(editors, "replace")))
	out, err := git([]string{editor}, "commit", "--no-gpg-sign", "-m", "My change", "-e")
	require.NoError(t, err, out)

	recorded := env.GetCommitMessage(env.GetHeadHash())
	require.Equal(t, "My change\n\nEntire-Checkpoint: "+advForeignCheckpoint+"\n", recorded, "precondition: the user replaced the stamped trailer")
	require.False(t, advCheckpointStored(env, advForeignCheckpoint),
		"a trailer prepare did not stamp must not claim a checkpoint ID this clone does not have")
}

// A paragraph added below the trailer prepare-commit-msg stamped (an editor, a
// commit-msg hook, a chained tool) moves it out of the final trailer block. The
// stamp record still proves the ID is this commit's, so post-commit must
// create its checkpoint rather than leave the commit naming a missing one.
func TestTrailerAuthority_ParagraphAddedBelowTheStampedTrailer(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	git := gitWithEntireCommitHooksEnv(t, env)
	editors := t.TempDir()
	writeScripts(t, editors, map[string]string{
		"append": "#!/bin/sh\nprintf '\\nRefs: JIRA-123\\n' >> \"$1\"\n",
	})
	authorityAgentTurn(env, "mine.txt", "my agent work\n")
	_, err := git(nil, "add", "mine.txt")
	require.NoError(t, err)

	editor := "GIT_EDITOR=" + shellQuote(filepath.ToSlash(filepath.Join(editors, "append")))
	out, err := git([]string{editor}, "commit", "--no-gpg-sign", "-m", "My change", "-e")
	require.NoError(t, err, out)

	recorded := env.GetCommitMessage(env.GetHeadHash())
	require.Empty(t, trailers.ParseAllCheckpointsFromFinalTrailerBlock(recorded, ""), "precondition: the stamped trailer is no longer in the final trailer block: %q", recorded)
	ids := trailers.ParseAllCheckpoints(recorded)
	require.Len(t, ids, 1, "precondition: the commit keeps the stamped trailer: %q", recorded)
	require.True(t, advCheckpointStored(env, ids[0].String()),
		"the session's work belongs in the checkpoint prepare stamped, wherever the message moved it")
}

// Once post-commit creates the checkpoint of a stamped trailer moved out of
// the last paragraph, the commands that write into a commit's checkpoint must
// find it too: attach joins it instead of rewriting the commit under a second
// checkpoint, and `explain --commit --generate` resolves it.
func TestTrailerAuthority_StampedTrailerBelowAParagraphStaysLinked(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	git := gitWithEntireCommitHooksEnv(t, env)
	editors := t.TempDir()
	writeScripts(t, editors, map[string]string{
		"append": "#!/bin/sh\nprintf '\\nRefs: JIRA-123\\n' >> \"$1\"\n",
	})
	authorityAgentTurn(env, "mine.txt", "my agent work\n")
	_, err := git(nil, "add", "mine.txt")
	require.NoError(t, err)
	editor := "GIT_EDITOR=" + shellQuote(filepath.ToSlash(filepath.Join(editors, "append")))
	out, err := git([]string{editor}, "commit", "--no-gpg-sign", "-m", "My change", "-e")
	require.NoError(t, err, out)

	head := env.GetHeadHash()
	recorded := env.GetCommitMessage(head)
	require.Empty(t, trailers.ParseAllCheckpointsFromFinalTrailerBlock(recorded, ""), "precondition: the stamped trailer is no longer in the final trailer block: %q", recorded)
	ids := trailers.ParseAllCheckpoints(recorded)
	require.Len(t, ids, 1, "precondition: the commit keeps the stamped trailer: %q", recorded)
	stamped := ids[0].String()
	require.True(t, advCheckpointStored(env, stamped), "precondition: post-commit created the stamped checkpoint")

	sessionID := "attach-research-session"
	tb := NewTranscriptBuilder()
	tb.AddUserMessage("explain the change")
	tb.AddAssistantMessage("It adds mine.txt.")
	require.NoError(t, tb.WriteToFile(filepath.Join(env.ClaudeProjectDir, sessionID+".jsonl")))
	attachOut := env.RunCLI("session", "attach", sessionID, "-a", agentClaudeCode, "-f")
	require.Contains(t, attachOut, "Added to existing checkpoint "+stamped, "attach must join the stamped checkpoint: %s", attachOut)
	require.Equal(t, head, env.GetHeadHash(), "attach must not rewrite the commit")

	// An unknown provider fails before any provider call, after the commit's
	// checkpoint is resolved.
	env.PatchSettings(map[string]any{"summary_generation": map[string]any{"provider": "no-such-provider"}})
	genOut, genErr := env.RunCLIWithError("checkpoint", "explain", "--commit", head, "--generate")
	require.Error(t, genErr)
	require.NotContains(t, genOut, "only outside its final trailer block", "--generate must resolve the stamped checkpoint")
	require.Contains(t, genOut, "no-such-provider", "--generate must reach summary generation: %s", genOut)
}

// An amend that restores the session's last trailer and is then cancelled
// must not leave that existing ID looking like a later commit's own stamp. A
// follow-up commit naming it gets a trailer of its own, and its work is
// recorded there, as it would be had the amend never run.
func TestTrailerAuthority_CancelledAmendRestoreThenCommitNamingIt(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	git := gitWithEntireCommitHooksEnv(t, env)
	editors := t.TempDir()
	writeScripts(t, editors, map[string]string{
		// Leaves only the trailer, so commit-msg strips it and git aborts.
		"cancel": "#!/bin/sh\ngrep -E '^(Entire-Checkpoint|#)' \"$1\" > \"$1.new\"; mv \"$1.new\" \"$1\"\n",
	})
	authorityAgentTurn(env, "first.txt", "first\n")
	_, err := git(nil, "add", "first.txt")
	require.NoError(t, err)
	out, err := git(nil, "commit", "--no-gpg-sign", "-m", "First")
	require.NoError(t, err, out)
	restored := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	require.NotEmpty(t, restored, "precondition: the first commit is linked")
	before, ok := checkpointBlob(env, restored, paths.MetadataFileName)
	require.True(t, ok, "precondition: the first checkpoint is stored")

	// Reword without the trailer, so a plain --amend restores it.
	out, err = git(nil, "commit", "--no-gpg-sign", "--amend", "-m", "First, reworded")
	require.NoError(t, err, out)
	editor := "GIT_EDITOR=" + shellQuote(filepath.ToSlash(filepath.Join(editors, "cancel")))
	out, err = git([]string{editor}, "commit", "--no-gpg-sign", "--amend")
	require.Error(t, err, "precondition: the amend is cancelled: %s", out)

	authorityAgentTurn(env, "second.txt", "second\n")
	_, err = git(nil, "add", "second.txt")
	require.NoError(t, err)
	out, err = git(nil, "commit", "--no-gpg-sign", "-m", "Follow-up\n\nEntire-Checkpoint: "+restored)
	require.NoError(t, err, out)

	final := env.GetCommitMessage(env.GetHeadHash())
	ids := trailers.ParseAllCheckpointsFromFinalTrailerBlock(final, "")
	require.Len(t, ids, 2, "prepare stamps its own trailer after the restored one: %q", final)
	require.True(t, advCheckpointStored(env, ids[1].String()), "the follow-up's work is recorded in its own checkpoint")
	after, ok := checkpointBlob(env, restored, paths.MetadataFileName)
	require.True(t, ok)
	require.Equal(t, before, after, "the first commit's checkpoint must not change")
}

// The final trailer block names a checkpoint this clone does not have, then
// one it has. Neither is one prepare-commit-msg stamped, so prepare must stamp
// a fresh trailer, and post-commit must condense there, not into the first.
func TestTrailerAuthority_FinalBlockNamingUnknownThenExistingCheckpoint(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	authorityAgentTurn(env, "first.txt", "first change\n")
	env.GitCommitWithHooks("First change", "first.txt")
	existing := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	require.NotEmpty(t, existing, "precondition: the first commit is linked")
	before, ok := checkpointBlob(env, existing, paths.MetadataFileName)
	require.True(t, ok, "precondition: the first checkpoint is stored")

	authorityAgentTurn(env, "second.txt", "second change\n")
	_, final := advCommit(env,
		"Follow-up\n\nEntire-Checkpoint: "+advForeignCheckpoint+"\nEntire-Checkpoint: "+existing+"\n",
		"message", keepAll, "second.txt")

	require.False(t, advCheckpointStored(env, advForeignCheckpoint),
		"an unknown checkpoint in the final trailer block is not this commit's stamp: %q", final)
	after, ok := checkpointBlob(env, existing, paths.MetadataFileName)
	require.True(t, ok)
	require.Equal(t, before, after, "the existing checkpoint must not change")
	ids := trailers.ParseAllCheckpointsFromFinalTrailerBlock(final, "")
	require.Len(t, ids, 3, "prepare stamps its own trailer after the two it did not stamp: %q", final)
	require.True(t, advCheckpointStored(env, ids[2].String()), "the session's work goes into the trailer prepare stamped")
}

// setRepoGitConfig sets one repository config value and makes it the
// .git/config guard's new baseline.
func setRepoGitConfig(env *TestEnv, key, value string) {
	env.T.Helper()
	testutil.RunGit(env.T, env.RepoDir, "config", key, value)
	require.Equal(env.T, value, strings.TrimSpace(testutil.RunGit(env.T, env.RepoDir, "config", "--get", key)))
	data, err := os.ReadFile(filepath.Join(env.RepoDir, ".git", "config"))
	require.NoError(env.T, err)
	env.AcceptGitConfigChanges(string(data))
}

// authorityAgentTurn runs one complete agent turn that writes path.
func authorityAgentTurn(env *TestEnv, path, content string) {
	env.T.Helper()
	sess := env.NewSession()
	require.NoError(env.T, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, "change "+path, sess.TranscriptPath))
	env.WriteFile(path, content)
	sess.CreateTranscript("change "+path, []FileChange{{Path: path, Content: content}})
	require.NoError(env.T, env.SimulateStop(sess.ID, sess.TranscriptPath))
}

// requireFreshFinalTrailer returns the checkpoint the commit's final trailer block
// names, requiring it to differ from the one its body mentions.
func requireFreshFinalTrailer(t *testing.T, message, mentioned string) string {
	t.Helper()
	stamped := trailers.ParseAllCheckpointsFromFinalTrailerBlock(message, "")
	require.Len(t, stamped, 1, "prepare-commit-msg must stamp a trailer in the final trailer block: %q", message)
	require.NotEqual(t, mentioned, stamped[0].String(), "the stamped trailer must not reuse the body's ID: %q", message)
	return stamped[0].String()
}
