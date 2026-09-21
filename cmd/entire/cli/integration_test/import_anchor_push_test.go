//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agentimport"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// Imported metadata references code by SHA, not by a Git parent edge. Exercise
// the real pre-push hook: the code and checkpoint refs have independent arrival.
func TestImportClaudeCode_AnchorFirstAndSecondPush(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend
		remote := env.SetupEmptyNamedBareRemote("origin")
		fallback := gitOutput(t, env.RepoDir, "rev-parse", "master")
		checkpointID := importAnchorFixture(t, env, "", fallback)
		env.GitPushWithHooks("origin", "HEAD")
		require.Equal(t, "commit", gitOutput(t, remote, "cat-file", "-t", fallback))
		if backend == StoreGitBranch {
			require.False(t, env.CheckpointsPresentOnRemote(remote), "first code branch must precede metadata branch")
		} else {
			require.True(t, env.CheckpointExistsOnRemote(remote, checkpointID))
		}
		env.WriteFile("later.txt", "later")
		env.GitAdd("later.txt")
		env.GitCommit("later code")
		env.GitPushWithHooks("origin", "HEAD")
		require.True(t, env.CheckpointExistsOnRemote(remote, checkpointID))
		assertImportedAnchorMetadata(t, env, remote, checkpointID, fallback)
	})
}

func TestImportClaudeCode_MixedTurnAnchors(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend
		recorded := gitOutput(t, env.RepoDir, "rev-parse", "master")
		env.WriteFile("new-default.txt", "new default")
		env.GitAdd("new-default.txt")
		env.GitCommit("advance default")
		fallback := gitOutput(t, env.RepoDir, "rev-parse", "HEAD")
		testutil.RunGit(t, env.RepoDir, "branch", "-f", "master", fallback)
		firstID := importAnchorFixture(t, env, recorded, fallback)
		assertImportedAnchorMetadata(t, env, env.RepoDir, firstID, recorded)
		secondID := agentimport.DeriveCheckpointID("anchor-fixture", "u2").String()
		assertImportedAnchorMetadata(t, env, env.RepoDir, secondID, fallback)
	})
}

// Separate checkpoint storage is exempt from the first-publication deferral the
// git-branch backend applies to an empty code remote: a dedicated repo can
// never become the code remote's default branch, so the very first push
// delivers the code branch AND the imported checkpoint, on both backends.
func TestImportClaudeCode_DedicatedStorageFirstPush(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		srv := startGitHTTPSServer(t, "testorg/code", "testorg/checkpoints")
		codeBare, checkpointBare := srv.BareDirs["testorg/code"], srv.BareDirs["testorg/checkpoints"]
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend
		// Both repos are served by the same host over HTTPS, and origin's owner
		// matches the checkpoint repo's: an owner the push URL cannot supply
		// (a filesystem path) or one that differs makes checkpointRemoteIsInherited
		// reject the setting and send the checkpoints to origin instead.
		testutil.AddRemote(t, env.RepoDir, "origin", srv.URL+"/testorg/code.git")
		env.setGitConfigBaseline()
		env.ExtraEnv = srv.plainGitPushEnv("import-test-token")
		env.PatchSettings(map[string]any{"strategy_options": map[string]any{
			"checkpoint_remote": map[string]any{"provider": "github", "repo": "testorg/checkpoints"},
		}})
		fallback := gitOutput(t, env.RepoDir, "rev-parse", "master")
		id := importAnchorFixture(t, env, "", fallback)

		env.GitPushWithHooks("origin", "HEAD")

		require.Equal(t, "commit", gitOutput(t, codeBare, "cat-file", "-t", fallback))
		require.True(t, env.CheckpointExistsOnRemote(checkpointBare, id), "dedicated store receives the import on the first push")
		require.False(t, env.CheckpointsPresentOnRemote(codeBare), "the code remote never receives checkpoint data in dedicated mode")
		// git-refs only (the git-branch backend keeps no queue, so this is
		// vacuously empty there): nothing is left pending, i.e. the exemption
		// delivered rather than deferred.
		require.Empty(t, env.QueuedCheckpointRefs())
		assertImportedAnchorMetadata(t, env, checkpointBare, id, fallback)
	})
}

func TestImportClaudeCode_CheckpointDeliveryDoesNotDeliverCodeAnchor(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		for _, scenario := range []string{"rejected-code-push", "different-branch"} {
			t.Run(scenario, func(t *testing.T) {
				t.Parallel()
				env := NewFeatureBranchEnv(t)
				env.CheckpointStore = backend
				remote := env.SetupBareRemote()
				env.WriteFile("anchor.txt", "local code")
				env.GitAdd("anchor.txt")
				env.GitCommit("local anchor")
				fallback := gitOutput(t, env.RepoDir, "rev-parse", "HEAD")
				testutil.RunGit(t, env.RepoDir, "branch", "-f", "master", fallback)
				id := importAnchorFixture(t, env, "", fallback)
				if scenario == "rejected-code-push" {
					hook := filepath.Join(remote, "hooks", "update")
					// Accept checkpoint refs but reject the outer user's branch.
					require.NoError(t, os.WriteFile(hook, []byte("#!/bin/sh\ncase \"$1\" in refs/heads/entire/*|refs/entire/*) exit 0;; *) exit 1;; esac\n"), 0o755))
					require.Error(t, env.GitPushWithHooksAllowError("origin", "HEAD"))
					// Retire only this fixture's reject hook for the later retry.
					require.NoError(t, os.Remove(hook))
				} else {
					env.GitPushWithHooks("origin", "HEAD~1:refs/heads/other")
				}
				require.True(t, env.CheckpointExistsOnRemote(remote, id))
				assertImportedAnchorMetadata(t, env, remote, id, fallback)
				// A rejected receive may retain dangling objects; only published
				// ref reachability establishes that the code branch arrived.
				reachable := gitOutput(t, remote, "rev-list", "--all")
				require.NotContains(t, strings.Split(reachable, "\n"), fallback)
				checkpointState := env.RemoteCheckpointState(remote)
				env.GitPushWithHooks("origin", "HEAD")
				require.Equal(t, "commit", gitOutput(t, remote, "cat-file", "-t", fallback))
				require.Equal(t, checkpointState, env.RemoteCheckpointState(remote), "code retry must not rewrite imported payloads")
			})
		}
	})
}

// The standalone command's half of the refusal the onboarding path proves in
// TestRunSelectedImports_EmptyRepoSkipsBeforeDiscovery: a repo with no commit
// has nothing to anchor to, so import fails — on --dry-run too — instead of
// writing anchorless checkpoints.
func TestImportClaudeCode_AnchorlessRepoRefusesImport(t *testing.T) {
	t.Parallel()
	// Both backends: CheckpointsPresentLocally reads a different namespace for
	// each (the v1 branch vs refs/entire/checkpoints/), so a single-backend run
	// leaves the other one's "nothing was written" unasserted.
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewTestEnv(t)
		env.CheckpointStore = backend
		env.InitRepo()
		testutil.WriteFile(t, env.ClaudeProjectDir, "anchorless.jsonl",
			`{"type":"user","uuid":"u1","timestamp":"2026-06-20T00:00:00Z","message":{"role":"user","content":"first"}}`+"\n")
		for _, extra := range [][]string{nil, {"--dry-run"}} {
			out, err := env.RunCLIWithError(append([]string{"import", agentClaudeCode}, extra...)...)
			require.Error(t, err, "output: %s", out)
			require.Contains(t, out, "without a valid anchor commit")
		}
		require.False(t, env.CheckpointsPresentLocally())
	})
}

func importAnchorFixture(t *testing.T, env *TestEnv, recorded, fallback string) string {
	t.Helper()
	lines := []string{`{"type":"user","uuid":"u1","timestamp":"2026-06-20T00:00:00Z","message":{"role":"user","content":"first"}}`}
	if recorded != "" {
		lines = append(lines, fmt.Sprintf(`{"type":"user","toolUseResult":{"gitOperation":{"commit":{"sha":%q,"kind":"committed"}}},"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"committed"}]}}`, recorded))
	}
	lines = append(lines, `{"type":"user","uuid":"u2","timestamp":"2026-06-20T00:01:00Z","message":{"role":"user","content":"second"}}`)
	testutil.WriteFile(t, env.ClaudeProjectDir, "anchor-fixture.jsonl", strings.Join(lines, "\n")+"\n")
	out := env.RunCLI("import", agentClaudeCode, "--path", filepath.Clean(env.ClaudeProjectDir))
	require.Contains(t, out, "Imported 2")
	id := agentimport.DeriveCheckpointID("anchor-fixture", "u1").String()
	want := fallback
	if recorded != "" {
		want = recorded
	}
	assertImportedAnchorMetadata(t, env, env.RepoDir, id, want)
	return id
}

func assertImportedAnchorMetadata(t *testing.T, env *TestEnv, repoDir, checkpointID, want string) {
	t.Helper()
	ref, base := "entire/checkpoints/v1", checkpointID[:2]+"/"+checkpointID[2:]+"/"
	if env.usingGitRefs() {
		ref, base = checkpointRefName(checkpointID), ""
	}
	for _, name := range []string{"metadata.json", "0/metadata.json"} {
		raw := gitOutput(t, repoDir, "show", ref+":"+base+name)
		var metadata struct {
			CommitSHA string `json:"commit_sha"`
		}
		require.NoError(t, json.Unmarshal([]byte(raw), &metadata))
		require.Equal(t, want, metadata.CommitSHA, "%s:%s%s", ref, base, name)
	}
}
