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
		// The policy line is git-refs only (the default): the git-branch
		// backend does not read the policy, and under manual there is nothing to offer.
		privacyNoPolicy = "  They contain your agent transcripts. If that repo is public, anyone can read them.\n" +
			"  Secret redaction is best effort, not bulletproof. Review with `entire checkpoint explain`.\n" +
			"  To stop uploads: `entire configure --local --skip-push-sessions`\n" +
			"  To use a separate private repo: `entire configure --local --checkpoint-remote github:<owner>/<repo>`\n"
		privacy = "  They contain your agent transcripts. If that repo is public, anyone can read them.\n" +
			"  Secret redaction is best effort, not bulletproof. Review with `entire checkpoint explain`.\n" +
			"  To stop uploads: `entire configure --local --skip-push-sessions`\n" +
			"  To upload only the checkpoints you choose: `entire configure --checkpoint-push-policy manual`\n" +
			"  To use a separate private repo: `entire configure --local --checkpoint-remote github:<owner>/<repo>`\n"
		redaction = "  Secret redaction is best effort, not bulletproof. Review with `entire checkpoint explain`.\n"
	)
	enableAgent := []string{"enable", "--agent", agentClaudeCode, "--telemetry=false"}
	for _, tc := range []struct {
		name     string
		noRemote bool
		fork     bool
		branch   bool
		// policy seeds the stored checkpoint push policy.
		policy string
		before []string
		args   []string
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
		{name: "git-branch", branch: true, args: enableAgent, after: "Ready.", notice: "Checkpoints will be uploaded when you push to origin.\n" + privacyNoPolicy},
		{name: "manual", policy: "manual", args: enableAgent, after: "Ready.", notice: "Checkpoints will be uploaded to origin when you run `entire checkpoint push`.\n" + privacyNoPolicy},
		{name: "manual no remote", noRemote: true, policy: "manual", args: enableAgent, after: "Ready.", notice: "No Git remotes configured. Once you add one, `entire checkpoint push` will upload checkpoints to it.\n" + privacyNoPolicy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := freshRepoEnv(t)
			if tc.branch {
				env.CheckpointStore = StoreGitBranch
			}
			if tc.policy != "" {
				setCheckpointPushPolicy(t, env, tc.policy)
			}
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
		want := notice
		if backend == StoreGitRefs {
			want += "  To upload only the checkpoints you choose: `entire configure --checkpoint-push-policy manual`\n"
		}
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend
		origin, dedicated := setupDedicatedCheckpointStore(t, env, `"checkpoint_push_remote":"missing",`)

		out := env.RunCLI("enable")
		require.Contains(t, out, "Cannot resolve checkpoint remote selection")
		require.True(t, strings.HasSuffix(out, want), "notice should end the output; got:\n%s", out)

		createCheckpointedCommit(t, env, "Add module", "mod.go", "package mod", "Add module")
		env.RunPrePush("origin")
		require.True(t, env.CheckpointsPresentOnRemote(dedicated), "pushes should still upload to the dedicated store")
		require.False(t, env.CheckpointsPresentOnRemote(origin))
	})
}

// setupDedicatedCheckpointStore points the clone's checkpoint_remote at a local
// bare and returns origin and that bare. extraOptions is spliced into the
// local strategy_options, empty or ending in a comma.
func setupDedicatedCheckpointStore(t *testing.T, env *TestEnv, extraOptions string) (origin, dedicated string) {
	t.Helper()
	origin = env.SetupBareRemote()
	// The dedicated URL is derived from origin's protocol, which a bare
	// path does not have.
	testutil.RunGit(t, env.RepoDir, "remote", "set-url", "origin", "file://"+origin)
	dedicated = t.TempDir()
	testutil.RunGit(t, dedicated, "init", "--bare")
	for _, derived := range []string{
		"https://github.com/" + checkpointRemoteRepoSlug + ".git",
		"git@github.com:" + checkpointRemoteRepoSlug + ".git",
	} {
		testutil.RunGit(t, env.RepoDir, "config", "--add", "url.file://"+dedicated+".insteadOf", derived)
	}
	env.setGitConfigBaseline()
	// The local layer vouches for the store, which a file:// origin cannot.
	require.NoError(t, os.MkdirAll(filepath.Join(env.RepoDir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(env.RepoDir, ".entire", "settings.local.json"),
		[]byte(`{"strategy_options":{`+extraOptions+`"checkpoint_remote":{"provider":"github","repo":"`+checkpointRemoteRepoSlug+`"}}}`), 0o644))
	return origin, dedicated
}

// Pre-push uploads nothing while the checkpoint push policy cannot be read,
// to any destination, so enable must not promise uploads there. It still names
// the destination, and offers the command that replaces the policy.
func TestEnable_UnreadablePushPolicyPromisesNoUploads(t *testing.T) {
	t.Parallel()
	const (
		waits  = "none will upload until the checkpoint push policy can be read: "
		reason = `checkpoint push policy "stricter" is not one of "automatic", "manual"`
		notice = "  They contain your agent transcripts. If that repo is public, anyone can read them.\n" +
			"  Secret redaction is best effort, not bulletproof. Review with `entire checkpoint explain`.\n" +
			"  To stop uploads: `entire configure --local --skip-push-sessions`\n" +
			"  To replace the unreadable checkpoint push policy: `entire configure --checkpoint-push-policy <automatic|manual>`\n"
		privateRepo = "  To use a separate private repo: `entire configure --local --checkpoint-remote github:<owner>/<repo>`\n"
	)
	for _, tc := range []struct {
		name string
		// setup prepares the remotes; nil leaves the clone without any.
		setup func(t *testing.T, env *TestEnv)
		// lead is where the destination line starts, and tail what follows its reason.
		lead, tail string
	}{
		{
			name:  "origin",
			setup: func(_ *testing.T, env *TestEnv) { env.SetupBareRemote() },
			lead:  "Checkpoints go to origin, but " + waits,
			tail:  notice + privateRepo,
		},
		{
			name: "no remote",
			lead: "No Git remotes configured. Once you add one, checkpoints go to it, but " + waits,
			tail: notice + privateRepo,
		},
		{
			name: "dedicated",
			setup: func(t *testing.T, env *TestEnv) {
				t.Helper()
				setupDedicatedCheckpointStore(t, env, "")
			},
			lead: "Checkpoints go to the dedicated destination git@github.com:org/checkpoints.git, but " + waits,
			tail: notice,
		},
		{
			name: "dedicated without a resolved remote",
			setup: func(t *testing.T, env *TestEnv) {
				t.Helper()
				setupDedicatedCheckpointStore(t, env, `"checkpoint_push_remote":"missing",`)
			},
			lead: ". Checkpoints go to the dedicated destination, but " + waits,
			tail: notice,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := freshRepoEnv(t)
			// Only the git-refs backend reads the policy.
			env.CheckpointStore = StoreGitRefs
			setCheckpointPushPolicy(t, env, "stricter")
			if tc.setup != nil {
				tc.setup(t, env)
			}

			out := env.RunCLI("enable", "--agent", agentClaudeCode, "--telemetry=false")

			require.NotContains(t, out, "will be uploaded")
			require.NotContains(t, out, "can still proceed")
			require.NotContains(t, out, "use the dedicated destination")
			start := strings.LastIndex(out, tc.lead)
			require.GreaterOrEqual(t, start, 0, "want a line starting %q; got:\n%s", tc.lead, out)
			rest := out[start:]
			require.Contains(t, rest, waits)
			require.Contains(t, rest, reason+"\n"+tc.tail)
			require.True(t, strings.HasSuffix(out, reason+"\n"+tc.tail), "the notice should end the output; got:\n%s", out)
		})
	}
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
