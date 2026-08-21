package strategy

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func initRemoteElectionRepo(t *testing.T) string {
	t.Helper()
	testutil.IsolateGitConfigEnv(t)
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	return tmpDir
}

func electionHeadHash(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "rev-parse", "HEAD")
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.Output()
	require.NoError(t, err, "git rev-parse HEAD")
	return strings.TrimSpace(string(out))
}

// A read-only origin must never seed the local primary while another remote
// is elected: the elected remote is authoritative for the store's state, and
// a stale origin driving local-ref writes is the #1374-class hazard.
func TestEnsurePrimaryRef_NonElectedOriginNeverSeedsLocal(t *testing.T) {
	tmpDir := initRemoteElectionRepo(t)
	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, tmpDir, "upstream", "https://example.com/upstream.git")
	testutil.WriteCheckpointPushRemoteSetting(t, tmpDir, "upstream")

	staleHash := electionHeadHash(t, tmpDir)
	testutil.GitUpdateRef(t, tmpDir, "refs/remotes/origin/"+paths.MetadataBranchName, staleHash)

	t.Chdir(tmpDir)
	ctx := context.Background()
	repo, err := OpenRepository(ctx)
	require.NoError(t, err)
	defer repo.Close()

	require.NoError(t, EnsurePrimaryRef(ctx, repo))

	localRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err, "an orphan primary ref should have been created")
	assert.NotEqual(t, staleHash, localRef.Hash().String(),
		"a non-elected origin must not seed the local primary")
}

// Regression (PR #1951 review): a FAILED election (checkpoint_push_remote
// names a missing remote) used to fall through to a fresh orphan even when
// origin's tracking ref held the real v1 history. The orphan buys no safety —
// checkpoint pushes are already fail-closed while the election is broken —
// and it guarantees a diverged history the moment the user fixes the setting
// (every later sync is non-fast-forward). Seeding a MISSING local primary
// from origin under a failed election is the seeding counterpart of the read
// chain's fail-open: no existing local ref is advanced (so this is not the
// #1374 replay-on-divergence hazard), and a possibly-behind real history
// reconciles far better than a guaranteed-disjoint orphan.
func TestEnsurePrimaryRef_FailedElectionSeedsMissingPrimaryFromOrigin(t *testing.T) {
	tmpDir := initRemoteElectionRepo(t)
	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	testutil.WriteCheckpointPushRemoteSetting(t, tmpDir, "gone")

	originHash := electionHeadHash(t, tmpDir)
	testutil.GitUpdateRef(t, tmpDir, "refs/remotes/origin/"+paths.MetadataBranchName, originHash)

	t.Chdir(tmpDir)
	ctx := context.Background()
	repo, err := OpenRepository(ctx)
	require.NoError(t, err)
	defer repo.Close()

	require.NoError(t, EnsurePrimaryRef(ctx, repo))

	localRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)
	assert.Equal(t, originHash, localRef.Hash().String(),
		"a failed election with real origin history must seed from origin, not create a disjoint orphan")
}

// The same failed election with NO origin history still creates the orphan —
// fail-open seeding needs something real to seed from.
func TestEnsurePrimaryRef_FailedElectionWithoutOriginHistoryCreatesOrphan(t *testing.T) {
	tmpDir := initRemoteElectionRepo(t)
	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	testutil.WriteCheckpointPushRemoteSetting(t, tmpDir, "gone")

	t.Chdir(tmpDir)
	ctx := context.Background()
	repo, err := OpenRepository(ctx)
	require.NoError(t, err)
	defer repo.Close()

	require.NoError(t, EnsurePrimaryRef(ctx, repo))

	_, err = repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err, "an orphan primary ref should have been created")
}

func TestEnsurePrimaryRef_ElectedUpstreamTrackingSeedsLocal(t *testing.T) {
	tmpDir := initRemoteElectionRepo(t)
	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, tmpDir, "upstream", "https://example.com/upstream.git")
	testutil.WriteCheckpointPushRemoteSetting(t, tmpDir, "upstream")

	headHash := electionHeadHash(t, tmpDir)
	testutil.GitUpdateRef(t, tmpDir, "refs/remotes/upstream/"+paths.MetadataBranchName, headHash)

	t.Chdir(tmpDir)
	ctx := context.Background()
	repo, err := OpenRepository(ctx)
	require.NoError(t, err)
	defer repo.Close()

	require.NoError(t, EnsurePrimaryRef(ctx, repo))

	localRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)
	assert.Equal(t, headHash, localRef.Hash().String(),
		"the elected upstream tracking ref must seed the local primary")
}
