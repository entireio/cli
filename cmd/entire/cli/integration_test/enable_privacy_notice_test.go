//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// Agents and CI run `entire enable` without a terminal, so the notice that
// checkpoints carry transcripts to the push destination must reach that output
// too, on every path, and be the last thing printed.
func TestEnable_PrivacyNotice(t *testing.T) {
	t.Parallel()
	const (
		privacy = "  They contain your agent transcripts. If that repo is public, anyone can read them.\n" +
			"  Secret redaction is best effort, not bulletproof. Review with `entire checkpoint explain`.\n" +
			"  To stop uploads: `entire configure --local --skip-push-sessions`\n" +
			"  To use a separate private repo: `entire configure --local --checkpoint-remote github:<owner>/<repo>`\n"
		redaction = "  Secret redaction is best effort, not bulletproof. Review with `entire checkpoint explain`.\n"
	)
	enableAgent := []string{"enable", "--agent", agentClaudeCode, "--telemetry=false"}
	for _, tc := range []struct {
		name     string
		noRemote bool
		fork     bool
		before   []string
		args     []string
		// after is setup output the notice must follow.
		after, notice string
	}{
		{name: "agent", args: enableAgent, after: "Ready.", notice: "Checkpoints will be uploaded when you push to origin.\n" + privacy},
		// Nothing pins the destination, so the line naming it is only the
		// current election and must come with the note that says so.
		{name: "agent with two remotes", fork: true, args: enableAgent, after: "Note: this repo's remotes make the checkpoint destination ambiguous.", notice: "Checkpoints will be uploaded when you push to origin.\n" + privacy},
		{name: "yes", args: []string{"enable", "--yes", "--telemetry=false"}, after: "Ready.", notice: "Checkpoints will be uploaded when you push to origin.\n" + privacy},
		{name: "rerun", before: enableAgent, args: []string{"enable"}, after: "Entire is already enabled.", notice: "Checkpoints will be uploaded when you push to origin.\n" + privacy},
		{name: "push disabled", args: append(enableAgent, "--skip-push-sessions"), after: "Ready.", notice: "Checkpoint uploads are off (push_sessions: false). Checkpoints stay in this clone.\n" + redaction},
		{name: "no remote", noRemote: true, args: enableAgent, after: "Ready.", notice: "No Git remotes configured. Once you add one, checkpoints will be uploaded when you push to it.\n" + privacy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := freshRepoEnv(t)
			if !tc.noRemote {
				env.SetupBareRemote()
			}
			if tc.fork {
				env.SetupNamedBareRemote(forkRemote)
			}
			if tc.before != nil {
				env.RunCLI(tc.before...)
			}
			out := env.RunCLI(tc.args...)
			require.Contains(t, out, tc.after)
			require.True(t, strings.HasSuffix(out, "\n"+tc.notice), "notice should end the output; got:\n%s", out)
			require.Less(t, strings.Index(out, tc.after), strings.LastIndex(out, tc.notice), "notice should follow %q; got:\n%s", tc.after, out)
		})
	}
}

// A checkpoint_push_remote naming a missing remote stops ordinary checkpoint
// sync, but pushes still upload to a dedicated checkpoint_remote, so enable
// must keep the notice there. The push half pins why: if this state ever
// stops uploading, the notice should go too.
func TestEnable_PrivacyNoticeWhileDedicatedUploadsContinue(t *testing.T) {
	t.Parallel()
	const notice = "Dedicated checkpoint uploads can still proceed.\n" +
		"  They contain your agent transcripts. If that repo is public, anyone can read them.\n" +
		"  Secret redaction is best effort, not bulletproof. Review with `entire checkpoint explain`.\n" +
		"  To stop uploads: `entire configure --local --skip-push-sessions`\n"
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend
		origin := env.SetupBareRemote()
		// The dedicated URL is derived from origin's protocol, which a bare
		// path does not have.
		testutil.RunGit(t, env.RepoDir, "remote", "set-url", "origin", "file://"+origin)
		dedicated := t.TempDir()
		testutil.RunGit(t, dedicated, "init", "--bare")
		for _, derived := range []string{
			"https://github.com/" + checkpointRemoteRepoSlug + ".git",
			"git@github.com:" + checkpointRemoteRepoSlug + ".git",
		} {
			testutil.RunGit(t, env.RepoDir, "config", "--add", "url.file://"+dedicated+".insteadOf", derived)
		}
		env.setGitConfigBaseline()
		// The local layer vouches for the store, which a file:// origin cannot.
		require.NoError(t, os.WriteFile(filepath.Join(env.RepoDir, ".entire", "settings.local.json"),
			[]byte(`{"strategy_options":{"checkpoint_push_remote":"missing","checkpoint_remote":{"provider":"github","repo":"`+checkpointRemoteRepoSlug+`"}}}`), 0o644))

		out := env.RunCLI("enable")
		require.Contains(t, out, "Cannot resolve checkpoint remote selection")
		require.True(t, strings.HasSuffix(out, notice), "notice should end the output; got:\n%s", out)

		createCheckpointedCommit(t, env, "Add module", "mod.go", "package mod", "Add module")
		env.RunPrePush("origin")
		require.True(t, env.CheckpointsPresentOnRemote(dedicated), "pushes should still upload to the dedicated store")
		require.False(t, env.CheckpointsPresentOnRemote(origin))
	})
}

// `entire agent add` shares enable's --agent setup path but has no destination
// report after it, so the multi-remote note belongs to enable alone.
func TestAgentAdd_OmitsCheckpointDestinationNote(t *testing.T) {
	t.Parallel()
	env := freshRepoEnv(t)
	env.SetupBareRemote()
	env.SetupNamedBareRemote(forkRemote)
	env.RunCLI("enable", "--agent", agentClaudeCode, "--telemetry=false")

	out := env.RunCLI("agent", "add", "codex")
	require.Contains(t, out, "Ready.")
	require.NotContains(t, out, "make the checkpoint destination ambiguous")
}
