//go:build integration

package integration

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// C5/C6: shallow and partial USER clones. A user who cloned with
// `git clone --depth=1` or `--filter=blob:none` must still be able to enable
// Entire, run a session, commit, push through the real hook, and read
// checkpoints — without the checkpoint machinery making the repo shallower
// (self-inflicted .git/shallow, regressions #1443/#1276) or stamping promisor
// config onto the named [remote "origin"] section (regression #934). The
// .git/config guard baselined by the clone helpers fails at cleanup if origin
// gains promisor pollution beyond what git itself wrote at clone time.

// cloneShallowFrom makes a real `git clone --depth=1` of bareDir over the smart
// file:// transport (required for --depth to be honored from a local bare).
func cloneShallowFrom(t *testing.T, env *TestEnv, bareDir string) *TestEnv {
	t.Helper()
	return env.cloneFromWithArgs("file://"+bareDir,
		[]string{"-c", "protocol.file.allow=always", "--depth=1"})
}

// clonePartialFrom makes a real `git clone --filter=blob:none` of bareDir over
// the smart file:// transport. enableFilterOnBare turns on uploadpack.allowFilter
// so the server honors the filter (git writes promisor config onto origin at
// clone time — the guard tolerates that baseline and only catches later CLI
// pollution).
func clonePartialFrom(t *testing.T, env *TestEnv, bareDir string) *TestEnv {
	t.Helper()
	enableFilterOnBare(t, bareDir, testutil.GitIsolatedEnv())
	return env.cloneFromWithArgs("file://"+bareDir,
		[]string{"-c", "protocol.file.allow=always", "--filter=blob:none"})
}

// isShallowRepo reports whether the repo at dir is shallow (has a .git/shallow
// graft). Uses git's own answer rather than probing the file so a linked
// worktree resolves correctly.
func isShallowRepo(t *testing.T, dir string) bool {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "rev-parse", "--is-shallow-repository")
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.Output()
	require.NoError(t, err, "git rev-parse --is-shallow-repository")
	return strings.TrimSpace(string(out)) == "true"
}

// TestShallowClone_SessionCommitPushResume is C5: a --depth=1 user clone can run
// the full session -> commit -> real-hook push -> resume/read flow, and the
// checkpoint machinery never makes the working repo shallower than git left it
// (no self-inflicted .git/shallow, regressions #1443/#1276).
func TestShallowClone_SessionCommitPushResume(t *testing.T) {
	t.Parallel()

	ForEachBackend(t, func(t *testing.T, backend string) {
		seed := NewFeatureBranchEnv(t)
		seed.CheckpointStore = backend
		bareDir := seed.SetupBareRemote()
		// Seed a couple of commits so --depth=1 genuinely truncates history.
		seed.WriteFile("history.txt", "one")
		seed.GitAdd("history.txt")
		seed.GitCommit("history one")
		seed.GitPush("origin", "HEAD")

		clone := cloneShallowFrom(t, seed, bareDir)
		clone.CheckpointStore = backend
		require.True(t, isShallowRepo(t, clone.RepoDir),
			"the clone must actually be shallow for this test to be meaningful")

		checkpointID := createCheckpointedCommit(t, clone, "Shallow work", "shallow.go", "package shallow", "Shallow work")
		require.NotEmpty(t, checkpointID)

		clone.GitPushWithHooks("origin", "HEAD")
		require.True(t, clone.CheckpointExistsOnRemote(bareDir, checkpointID),
			"[%s] the checkpoint should sync to the remote from a shallow clone", backend)

		// The checkpoint sync must not have deepened/re-shallowed the user repo:
		// it stays shallow (git's clone-time state), never self-inflicting a new
		// shallow graft or corrupting the boundary.
		require.True(t, isShallowRepo(t, clone.RepoDir),
			"[%s] a shallow clone must remain shallow after checkpoint sync (no self-inflicted .git/shallow churn)", backend)

		// Reading the checkpoint works (auto-fetch tolerates the shallow boundary
		// rather than corrupting it).
		explain := clone.RunCLI("checkpoint", "explain", "--checkpoint", checkpointID)
		require.Contains(t, explain, "Shallow work",
			"[%s] the checkpoint should be readable from the shallow clone", backend)
	})
}

// TestPartialClone_ReadFetchesBlobsNoPromisorPollution is C6: a
// --filter=blob:none user clone reading a remote-only checkpoint lazily fetches
// the metadata blobs it needs (which the partial clone omitted), and the CLI
// never stamps additional promisor/partialclonefilter config onto
// [remote "origin"] (regression #934). The .git/config guard (baselined at
// clone time) enforces the no-pollution property at cleanup; the inline check
// gives a clearer failure.
//
// The checkpoint is created and pushed by the full seed repo, so the partial
// clone genuinely lacks its blobs and must fetch them on read.
func TestPartialClone_ReadFetchesBlobsNoPromisorPollution(t *testing.T) {
	t.Parallel()

	ForEachBackend(t, func(t *testing.T, backend string) {
		seed := NewFeatureBranchEnv(t)
		seed.CheckpointStore = backend
		bareDir := seed.SetupBareRemote()

		// Seed repo creates a checkpoint and pushes it (branch + checkpoints).
		checkpointID := createCheckpointedCommit(t, seed, "Partial work", "partial.go", "package partial", "Partial work")
		require.NotEmpty(t, checkpointID)
		seed.GitPush("origin", "HEAD")
		seed.RunPrePush("origin")
		require.True(t, seed.CheckpointExistsOnRemote(bareDir, checkpointID),
			"[%s] the checkpoint should be on the remote before the partial clone reads it", backend)

		// A --filter=blob:none clone gets trees but not blobs, so the checkpoint's
		// metadata blobs are genuinely absent locally.
		clone := clonePartialFrom(t, seed, bareDir)
		clone.CheckpointStore = backend

		// Snapshot origin's promisor state before the read triggers any fetch.
		originPromisorBefore := originPromisorConfig(t, clone.RepoDir)

		// Reading the checkpoint lazily fetches the omitted metadata blobs; it must
		// succeed without pulling the whole repo.
		explain := clone.RunCLI("checkpoint", "explain", "--checkpoint", checkpointID)
		require.Contains(t, explain, "Partial work",
			"[%s] explain should lazily fetch the metadata blobs a partial clone omitted", backend)

		// The read must not have stamped new promisor config onto origin (the
		// cleanup guard also enforces this; assert it inline for a clearer failure).
		require.Equal(t, originPromisorBefore, originPromisorConfig(t, clone.RepoDir),
			"[%s] a checkpoint read must not stamp new promisor config onto [remote \"origin\"] (#934)", backend)
	})
}

// originPromisorConfig returns the promisor-related git config values for the
// named origin remote, joined for comparison. Reads only the [remote "origin"]
// keys git uses to mark a partial clone.
func originPromisorConfig(t *testing.T, dir string) string {
	t.Helper()
	var parts []string
	for _, key := range []string{"remote.origin.promisor", "remote.origin.partialclonefilter"} {
		cmd := exec.CommandContext(t.Context(), "git", "config", "--get", key)
		cmd.Dir = dir
		cmd.Env = testutil.GitIsolatedEnv()
		out, _ := cmd.Output() // missing key -> exit 1, empty output
		parts = append(parts, key+"="+strings.TrimSpace(string(out)))
	}
	return strings.Join(parts, " ")
}
