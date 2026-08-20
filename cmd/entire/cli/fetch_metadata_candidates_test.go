package cli

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// When both remotes contain metadata, they point at different commits so tests
// can identify which candidate served the fetch.
func metadataCandidatesFixture(t *testing.T, onOrigin, onUpstream bool) (localDir, originHash, upstreamHash string) {
	t.Helper()
	testutil.IsolateGitConfigEnv(t)

	tmpDir := t.TempDir()
	originBare := filepath.Join(tmpDir, "origin.git")
	upstreamBare := filepath.Join(tmpDir, "upstream.git")
	localDir = filepath.Join(tmpDir, "local")
	runGit(t, tmpDir, "init", "--bare", originBare)
	runGit(t, tmpDir, "init", "--bare", upstreamBare)

	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "README.md", "hello")
	testutil.GitAdd(t, localDir, "README.md")
	testutil.GitCommit(t, localDir, "init")
	runGit(t, localDir, "remote", "add", "origin", originBare)
	runGit(t, localDir, "remote", "add", "upstream", upstreamBare)

	// Metadata branch at commit A.
	runGit(t, localDir, "branch", paths.MetadataBranchName)
	if onOrigin {
		// Commit A → origin (the legacy tier's copy).
		runGit(t, localDir, "push", "--quiet", "origin", paths.MetadataBranchName)
		originHash = revParse(t, localDir, paths.MetadataBranchName)
	}

	if onUpstream {
		if onOrigin {
			// Advance to commit B so the elected remote's tip differs from
			// origin's.
			runGit(t, localDir, "checkout", "--quiet", paths.MetadataBranchName)
			testutil.WriteFile(t, localDir, "metadata-b.txt", "checkpoint B")
			testutil.GitAdd(t, localDir, "metadata-b.txt")
			testutil.GitCommit(t, localDir, "checkpoint B")
			runGit(t, localDir, "checkout", "--quiet", "-")
		}
		runGit(t, localDir, "push", "--quiet", "upstream", paths.MetadataBranchName)
		upstreamHash = revParse(t, localDir, paths.MetadataBranchName)
	}

	// Drop local metadata state so the fetch decides what gets created.
	// Tracking refs may or may not exist depending on git's push behavior, so
	// their deletion is best-effort.
	runGit(t, localDir, "branch", "-D", paths.MetadataBranchName)
	for _, ref := range []string{
		"refs/remotes/origin/" + paths.MetadataBranchName,
		"refs/remotes/upstream/" + paths.MetadataBranchName,
	} {
		cmd := exec.CommandContext(t.Context(), "git", "-C", localDir, "update-ref", "-d", ref)
		cmd.Env = testutil.GitIsolatedEnv()
		_ = cmd.Run() //nolint:errcheck // best-effort cleanup of maybe-missing tracking refs
	}

	testutil.WriteCheckpointPushRemoteSetting(t, localDir, "upstream")
	t.Chdir(localDir)
	return localDir, originHash, upstreamHash
}

func refExists(t *testing.T, dir, ref string) bool {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "-C", dir, "rev-parse", "--verify", "--quiet", ref)
	cmd.Env = testutil.GitIsolatedEnv()
	return cmd.Run() == nil
}

// A legacy-tier read must not advance the local primary (#1374 confinement).
func TestFetchMetadataTreeOnly_LegacyOriginFetchNeverAdvancesLocal(t *testing.T) {
	localDir, originHash, _ := metadataCandidatesFixture(t, true, false)

	require.NoError(t, FetchMetadataTreeOnly(context.Background()))

	assert.False(t, refExists(t, localDir, "refs/heads/"+paths.MetadataBranchName),
		"a legacy-tier fetch must never create the local metadata branch")
	require.True(t, refExists(t, localDir, "refs/remotes/origin/"+paths.MetadataBranchName),
		"the legacy-tier fetch must land in origin's tracking ref")
	assert.Equal(t, originHash, revParse(t, localDir, "refs/remotes/origin/"+paths.MetadataBranchName))
}

func TestFetchMetadataTreeOnly_ElectedCandidateWinsAndAdvancesLocal(t *testing.T) {
	localDir, originHash, upstreamHash := metadataCandidatesFixture(t, true, true)

	require.NoError(t, FetchMetadataTreeOnly(context.Background()))

	require.True(t, refExists(t, localDir, "refs/heads/"+paths.MetadataBranchName),
		"the elected remote's fetch must create the local metadata branch")
	got := revParse(t, localDir, "refs/heads/"+paths.MetadataBranchName)
	assert.Equal(t, upstreamHash, got, "the elected candidate must win")
	assert.NotEqual(t, originHash, got)
}

// The legacy read tier must never become a policy push or local-update target.
func TestResolveCheckpointPolicyTargets_SplitsReadAndPush(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	testutil.AddRemote(t, dir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, dir, "upstream", "https://example.com/upstream.git")
	testutil.WriteCheckpointPushRemoteSetting(t, dir, "upstream")
	t.Chdir(dir)

	readTargets, pushTarget, err := resolveCheckpointPolicyTargets(context.Background())
	require.NoError(t, err)

	require.Len(t, readTargets, 2)
	assert.Equal(t, "upstream", readTargets[0].Remote)
	assert.False(t, readTargets[0].SkipLocalUpdate, "the elected remote may advance the local policy ref")
	assert.Equal(t, "origin", readTargets[1].Remote)
	assert.True(t, readTargets[1].SkipLocalUpdate, "the legacy tier is read-only")

	require.NotNil(t, pushTarget)
	assert.Equal(t, "upstream", pushTarget.Remote, "the push target is the elected remote only")
	assert.False(t, pushTarget.SkipLocalUpdate)
	assert.NotEmpty(t, pushTarget.Dir)
}
