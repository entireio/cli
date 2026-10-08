//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// forgeTransport stands in for github.com and gitlab.com at the ssh layer, so
// remotes keep real forge URLs (the ownership vote parses them) while every
// connection is served from a local bare and recorded. insteadOf cannot do
// this: `git remote get-url` applies it, so the vote would see a local path.
//
// A connection to a repo with no bare fails, so a push to a store the test did
// not create both fails and shows up in Contacted.
type forgeTransport struct {
	root string
	log  string
}

func newForgeTransport(t *testing.T, env *TestEnv) *forgeTransport {
	t.Helper()
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	ft := &forgeTransport{root: root, log: filepath.Join(root, "contacts.log")}
	// git hands ssh "[options] <user@host> <verb 'path'>"; the last two
	// arguments are all this needs.
	script := `#!/bin/sh
prev=""; last=""
for a in "$@"; do prev="$last"; last="$a"; done
host="${prev#*@}"
verb="${last%% *}"
path="${last#* }"; path="${path#\'}"; path="${path%\'}"; path="${path#/}"
echo "$verb $host/$path" >> '` + ft.log + `'
exec "$verb" '` + root + `'/"$host/$path"
`
	wrapper := filepath.Join(root, "ssh")
	require.NoError(t, os.WriteFile(wrapper, []byte(script), 0o755))
	env.ExtraEnv = append(env.ExtraEnv,
		"GIT_SSH_COMMAND="+wrapper,
		// A custom ssh command is otherwise probed with -G to guess its variant.
		"GIT_SSH_VARIANT=ssh",
		// Nothing here may leave the machine: an https derivation fails
		// instead of dialing a real forge.
		"GIT_ALLOW_PROTOCOL=ssh",
		"ENTIRE_CHECKPOINT_TOKEN=",
	)
	return ft
}

// Bare creates the repo served for host/slug and returns its path.
func (ft *forgeTransport) Bare(t *testing.T, host, slug string) string {
	t.Helper()
	dir := filepath.Join(ft.root, host, slug+".git")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	testutil.RunGit(t, dir, "init", "--bare")
	return dir
}

// Contacted reports every host/path any git process connected to.
func (ft *forgeTransport) Contacted(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(ft.log)
	if os.IsNotExist(err) {
		return ""
	}
	require.NoError(t, err)
	return string(data)
}

// addForgeRemote adds a code remote and seeds it with the current branch, as
// SetupNamedBareRemote does: the git-branch backend defers its first checkpoint
// push until the remote has a branch.
func addForgeRemote(t *testing.T, env *TestEnv, name, url string) {
	t.Helper()
	testutil.RunGit(t, env.RepoDir, "remote", "add", name, url)
	env.setGitConfigBaseline()
	cmd := execx.NonInteractive(t.Context(), "git", "push", "--no-verify", name, "HEAD")
	cmd.Dir = env.RepoDir
	cmd.Env = env.cliEnv()
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "seed %s:\n%s", name, out)
}

// setCheckpointRemote writes checkpoint_remote into settings.json (committed
// layer) or settings.local.json (the per-clone claim).
func setCheckpointRemote(t *testing.T, env *TestEnv, file, provider, repo string) {
	t.Helper()
	path := filepath.Join(env.RepoDir, ".entire", file)
	m := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		require.NoError(t, json.Unmarshal(data, &m))
	}
	opts, ok := m["strategy_options"].(map[string]any)
	if !ok {
		opts = map[string]any{}
	}
	opts["checkpoint_remote"] = map[string]any{"provider": provider, "repo": repo}
	m["strategy_options"] = opts
	data, err := json.MarshalIndent(m, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

// pushWithHooksOutput is GitPushWithHooks returning what the user sees.
func pushWithHooksOutput(t *testing.T, env *TestEnv, remote string) string {
	t.Helper()
	env.InstallRealPrePushHook()
	cmd := execx.NonInteractive(t.Context(), "git", "push", remote, "HEAD")
	cmd.Dir = env.RepoDir
	cmd.Env = env.cliEnv()
	out, err := cmd.CombinedOutput()
	t.Logf("git push %s output:\n%s", remote, out)
	require.NoError(t, err, "git push %s must succeed; a refused store never blocks a push:\n%s", remote, out)
	return string(out)
}

// TestCheckpointRemoteEgress pins where transcripts physically go for each
// ownership verdict, over a real `git push` with the installed pre-push hook.
// Each case asserts the positive destination and that the store the vote
// refused was never connected to at all.
func TestCheckpointRemoteEgress(t *testing.T) {
	t.Parallel()

	t.Run("owner's own store receives checkpoints", func(t *testing.T) {
		t.Parallel()
		ForEachBackend(t, func(t *testing.T, backend string) {
			env := NewFeatureBranchEnv(t)
			env.CheckpointStore = backend
			ft := newForgeTransport(t, env)
			origin := ft.Bare(t, "github.com", "alice/app")
			store := ft.Bare(t, "github.com", "alice/app-checkpoints")
			addForgeRemote(t, env, "origin", "git@github.com:alice/app.git")
			setCheckpointRemote(t, env, paths.SettingsFileName, "github", "alice/app-checkpoints")

			id := createCheckpointedCommit(t, env, "Add a", "a.go", "package a", "Add a")
			out := pushWithHooksOutput(t, env, "origin")

			// The control: proves the transport sees dedicated pushes, so the
			// absences asserted below are not vacuous.
			require.True(t, env.CheckpointExistsOnRemote(store, id))
			require.False(t, env.CheckpointExistsOnRemote(origin, id))
			require.NotContains(t, out, "not to the configured checkpoint_remote")
		})
	})

	t.Run("fork contributor's checkpoints stay in the fork", func(t *testing.T) {
		t.Parallel()
		ForEachBackend(t, func(t *testing.T, backend string) {
			env := NewFeatureBranchEnv(t)
			env.CheckpointStore = backend
			ft := newForgeTransport(t, env)
			ft.Bare(t, "github.com", "upstream/app")
			fork := ft.Bare(t, "github.com", "me/app")
			// Clone upstream, add a fork: origin's owner MATCHES the store.
			addForgeRemote(t, env, "origin", "git@github.com:upstream/app.git")
			addForgeRemote(t, env, forkRemote, "git@github.com:me/app.git")
			setCheckpointRemote(t, env, paths.SettingsFileName, "github", "upstream/app-checkpoints")
			setBranchTrackingRemote(t, env, forkRemote)
			env.RunCLI("enable", "--yes", "--checkpoint-push-remote", forkRemote)

			id := createCheckpointedCommit(t, env, "Add b", "b.go", "package b", "Add b")
			out := pushWithHooksOutput(t, env, forkRemote)

			require.True(t, env.CheckpointExistsOnRemote(fork, id))
			require.NotContains(t, ft.Contacted(t), "upstream/app-checkpoints")
			// Disproved is the ordinary fork case: no warning on every push.
			require.NotContains(t, out, "not to the configured checkpoint_remote")

			// enable and status describe this clone in the same sentence, even
			// though origin alone would reach the store (the case enable used
			// to go silent on).
			want := "Checkpoints sync to fork, not to the configured checkpoint_remote upstream/app-checkpoints"
			require.Contains(t, env.RunCLI("status"), want)
			require.Contains(t, env.RunCLI("enable", "--yes"), want)
			require.NotContains(t, ft.Contacted(t), "upstream/app-checkpoints",
				"status and enable must stay local-only")
		})
	})

	t.Run("owner name on another forge vouches for nothing", func(t *testing.T) {
		t.Parallel()
		ForEachBackend(t, func(t *testing.T, backend string) {
			env := NewFeatureBranchEnv(t)
			env.CheckpointStore = backend
			ft := newForgeTransport(t, env)
			origin := ft.Bare(t, "github.com", "alice/app")
			addForgeRemote(t, env, "origin", "git@github.com:alice/app.git")
			// Same owner name, different forge: before 1c935ce907 this voted
			// Ours and pushed to gitlab.com/alice/leak with the user's key.
			setCheckpointRemote(t, env, paths.SettingsFileName, "gitlab", "alice/leak")

			id := createCheckpointedCommit(t, env, "Add c", "c.go", "package c", "Add c")
			out := pushWithHooksOutput(t, env, "origin")

			require.True(t, env.CheckpointExistsOnRemote(origin, id))
			contacted := ft.Contacted(t)
			require.NotContains(t, contacted, "gitlab.com")
			require.NotContains(t, contacted, "alice/leak")
			// Unprovable is the one verdict pre-push warns about.
			require.Contains(t, out, `Checkpoints are going to "origin", not to the configured checkpoint_remote alice/leak`)
			require.Contains(t, out, "entire enable --local --checkpoint-remote gitlab:alice/leak")
		})
	})

	t.Run("a local claim goes to the claimed provider's host", func(t *testing.T) {
		t.Parallel()
		ForEachBackend(t, func(t *testing.T, backend string) {
			env := NewFeatureBranchEnv(t)
			env.CheckpointStore = backend
			ft := newForgeTransport(t, env)
			origin := ft.Bare(t, "github.com", "alice/app")
			store := ft.Bare(t, "gitlab.com", "alice/store")
			addForgeRemote(t, env, "origin", "git@github.com:alice/app.git")
			setCheckpointRemote(t, env, "settings.local.json", "gitlab", "alice/store")

			id := createCheckpointedCommit(t, env, "Add d", "d.go", "package d", "Add d")
			out := pushWithHooksOutput(t, env, "origin")

			// Before 876c88bd46 the URL kept origin's host: github.com/alice/store.
			require.True(t, env.CheckpointExistsOnRemote(store, id))
			require.False(t, env.CheckpointExistsOnRemote(origin, id))
			require.NotContains(t, ft.Contacted(t), "github.com/alice/store")
			require.NotContains(t, out, "not to the configured checkpoint_remote")
		})
	})

	// Guards the helper, not the product: a failing wrapper would make every
	// NotContains above pass trivially.
	t.Run("transport records connections", func(t *testing.T) {
		t.Parallel()
		env := NewFeatureBranchEnv(t)
		ft := newForgeTransport(t, env)
		ft.Bare(t, "github.com", "alice/app")
		addForgeRemote(t, env, "origin", "git@github.com:alice/app.git")
		cmd := execx.NonInteractive(t.Context(), "git", "push", "--no-verify", "origin", "HEAD")
		cmd.Dir = env.RepoDir
		cmd.Env = env.cliEnv()
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		require.Contains(t, ft.Contacted(t), "git-receive-pack github.com/alice/app")
	})
}
