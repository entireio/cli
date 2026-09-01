//go:build integration

package integration

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

// wipeLocalCheckpointState ensures subsequent reads can only succeed remotely.
func wipeLocalCheckpointState(t *testing.T, env *TestEnv) {
	t.Helper()
	repo, err := git.PlainOpen(env.RepoDir)
	require.NoError(t, err)

	for _, ref := range []plumbing.ReferenceName{
		plumbing.NewBranchReferenceName(paths.MetadataBranchName),
		plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName),
		plumbing.NewRemoteReferenceName("upstream", paths.MetadataBranchName),
	} {
		_ = repo.Storer.RemoveReference(ref) //nolint:errcheck // refs may legitimately not exist per scenario
	}

	if env.usingGitRefs() {
		iter, err := repo.References()
		require.NoError(t, err)
		var toDelete []plumbing.ReferenceName
		require.NoError(t, iter.ForEach(func(r *plumbing.Reference) error {
			if strings.HasPrefix(r.Name().String(), checkpointRefPrefix) {
				toDelete = append(toDelete, r.Name())
			}
			return nil
		}))
		for _, name := range toDelete {
			require.NoError(t, repo.Storer.RemoveReference(name))
		}
	}
}

func localPrimaryExists(t *testing.T, env *TestEnv) bool {
	t.Helper()
	repo, err := git.PlainOpen(env.RepoDir)
	require.NoError(t, err)
	_, err = repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	return err == nil
}

func rootlessWorktreeCommit(t *testing.T, env *TestEnv) string {
	t.Helper()
	return env.gitOutput(env.RepoDir, "commit-tree", "HEAD^{tree}", "-m", "checkpoint root")
}

func TestCheckpointReadRemotes_ConfiguredReadBack(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		bareOrigin := env.SetupBareRemote()
		bareUpstream := env.SetupNamedBareRemote("upstream")
		env.PatchSettings(map[string]any{
			"strategy_options": map[string]any{
				"checkpoint_push_remote": "upstream",
			},
		})

		checkpointID := createCheckpointedCommit(t, env, "Add readback module", "readback.go", "package readback", "Add readback module")
		env.RunPrePush("upstream")
		if !env.CheckpointExistsOnRemote(bareUpstream, checkpointID) {
			t.Fatalf("checkpoint %s should be on the elected remote", checkpointID)
		}
		if env.CheckpointsPresentOnRemote(bareOrigin) {
			t.Fatal("origin must stay empty in this scenario")
		}

		wipeLocalCheckpointState(t, env)

		output := env.RunCLI("checkpoint", "explain", "--checkpoint", checkpointID)
		if !strings.Contains(output, "Add readback module") {
			t.Errorf("read must find the checkpoint on the elected remote, got:\n%s", output)
		}

		if env.usingGitRefs() {
			// Discovery (checkpoint list) finds the elected remote's refs
			// without a dedicated checkpoint_remote.
			wipeLocalCheckpointState(t, env)
			listOutput := env.RunCLI("checkpoint", "list")
			if !strings.Contains(listOutput, "Add readback module") && !strings.Contains(listOutput, checkpointID[:8]) {
				t.Errorf("git-refs discovery should surface the elected remote's checkpoint, got:\n%s", listOutput)
			}
		}
	})
}

// The legacy tier may serve reads but must not recreate the local primary.
func TestCheckpointReadRemotes_LegacyOriginTierServed(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		bareOrigin := env.SetupBareRemote()
		_ = env.SetupNamedBareRemote("upstream")

		// Checkpoint lands on origin under the default election.
		checkpointID := createCheckpointedCommit(t, env, "Add legacy module", "legacy.go", "package legacy", "Add legacy module")
		env.RunPrePush("origin")
		if !env.CheckpointExistsOnRemote(bareOrigin, checkpointID) {
			t.Fatalf("checkpoint %s should be on origin", checkpointID)
		}

		// The election moves to upstream AFTER the data landed on origin —
		// the fork-adoption shape that makes origin the legacy tier.
		env.PatchSettings(map[string]any{
			"strategy_options": map[string]any{
				"checkpoint_push_remote": "upstream",
			},
		})

		wipeLocalCheckpointState(t, env)
		localPrimaryHash := ""
		if !env.usingGitRefs() {
			localPrimaryHash = rootlessWorktreeCommit(t, env)
			env.gitOutput(env.RepoDir, "update-ref", "refs/heads/"+paths.MetadataBranchName, localPrimaryHash)
		}

		output := env.RunCLI("checkpoint", "explain", "--checkpoint", checkpointID)
		if !strings.Contains(output, "Add legacy module") {
			t.Errorf("read must fall back to the legacy origin tier, got:\n%s", output)
		}

		if !env.usingGitRefs() {
			got := env.refHash(env.RepoDir, "refs/heads/"+paths.MetadataBranchName)
			if got != localPrimaryHash {
				t.Errorf("legacy reads must not rewrite the local primary: got %s, want %s", got, localPrimaryHash)
			}
		}

		if env.usingGitRefs() {
			// Discovery unions origin's legacy refs in even though the
			// election points at upstream.
			wipeLocalCheckpointState(t, env)
			listOutput := env.RunCLI("checkpoint", "list")
			if !strings.Contains(listOutput, "Add legacy module") && !strings.Contains(listOutput, checkpointID[:8]) {
				t.Errorf("git-refs discovery should union the legacy origin tier's refs, got:\n%s", listOutput)
			}
		}
	})
}

// An elected metadata branch may exist without containing a checkpoint that
// still lives on the legacy origin branch. Selection must happen per requested
// checkpoint, not merely per branch existence.
func TestCheckpointReadRemotes_ElectedBranchMissingCheckpointFallsBackToOrigin(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.CheckpointStore = StoreGitBranch

	bareOrigin := env.SetupBareRemote()
	_ = env.SetupNamedBareRemote("upstream")

	checkpointID := createCheckpointedCommit(t, env, "Add split module", "split.go", "package split", "Add split module")
	env.RunPrePush("origin")
	if !env.CheckpointExistsOnRemote(bareOrigin, checkpointID) {
		t.Fatalf("checkpoint %s should be on origin", checkpointID)
	}

	upstreamHash := rootlessWorktreeCommit(t, env)
	env.gitOutput(env.RepoDir, "push", "--quiet", "--no-verify", "upstream", upstreamHash+":refs/heads/"+paths.MetadataBranchName)
	env.PatchSettings(map[string]any{
		"strategy_options": map[string]any{
			"checkpoint_push_remote": "upstream",
		},
	})

	wipeLocalCheckpointState(t, env)

	output := env.RunCLI("checkpoint", "explain", "--checkpoint", checkpointID)
	if !strings.Contains(output, "Add split module") {
		t.Errorf("read must continue to origin when the elected branch lacks the checkpoint, got:\n%s", output)
	}
}

// TestCheckpointReadRemotes_ElectedUnreachableLegacyStillServes: the
// end-to-end local-ref confinement pin. The elected remote (upstream) becomes
// unreachable after the legacy data landed on origin; reads still succeed via
// the legacy tier while the local primary stays untouched (git-branch — the
// v1 branch is the local ref the #1374 hazard concerns).
func TestCheckpointReadRemotes_ElectedUnreachableLegacyStillServes(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		bareOrigin := env.SetupBareRemote()
		_ = env.SetupNamedBareRemote("upstream")

		checkpointID := createCheckpointedCommit(t, env, "Add outage module", "outage.go", "package outage", "Add outage module")
		env.RunPrePush("origin")
		if !env.CheckpointExistsOnRemote(bareOrigin, checkpointID) {
			t.Fatalf("checkpoint %s should be on origin", checkpointID)
		}

		env.PatchSettings(map[string]any{
			"strategy_options": map[string]any{
				"checkpoint_push_remote": "upstream",
			},
		})
		// The elected remote goes dark: point it at a path that doesn't exist.
		testutil.RunGit(t, env.RepoDir, "remote", "set-url", "upstream", env.RepoDir+"/nonexistent-remote")
		env.setGitConfigBaseline()

		wipeLocalCheckpointState(t, env)

		if env.usingGitRefs() {
			// git-refs hydration INSTALLS the canonical local checkpoint ref,
			// and only the elected remote holds the authoritative tip — so an
			// unreachable elected remote must REFUSE rather than let origin
			// install a possibly-stale tip that later backfills parent onto
			// (PR #1951 review: stale canonical ref installation). The
			// availability loss is confined to this outage window.
			output, err := env.RunCLIWithError("checkpoint", "explain", "--checkpoint", checkpointID)
			if err == nil && strings.Contains(output, "Add outage module") {
				t.Errorf("git-refs reads must not serve via a legacy install while the elected remote is unreachable, got:\n%s", output)
			}
			return
		}

		// git-branch reads are pure remote-tracking tree reads — no local ref
		// is installed — so the legacy tier may keep serving through the
		// outage.
		output := env.RunCLI("checkpoint", "explain", "--checkpoint", checkpointID)
		if !strings.Contains(output, "Add outage module") {
			t.Errorf("reads must survive an unreachable elected remote via the legacy tier, got:\n%s", output)
		}
		if localPrimaryExists(t, env) {
			t.Error("a stale/legacy origin must never recreate the local primary, even with the elected remote unreachable")
		}
	})
}
