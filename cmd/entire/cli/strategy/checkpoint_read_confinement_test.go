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

// electionHeadHash returns dir's HEAD commit hash. Every caller wanted HEAD, so
// the ref is not a parameter.
func electionHeadHash(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "rev-parse", "HEAD")
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.Output()
	require.NoError(t, err, "git rev-parse HEAD")
	return strings.TrimSpace(string(out))
}

// A read-only origin must never seed the local primary.
func TestEnsurePrimaryRef_NonElectedOriginNeverSeedsLocal(t *testing.T) {
	tests := []struct {
		name      string
		configure func(t *testing.T, dir string)
	}{
		{
			name: "another remote is elected",
			configure: func(t *testing.T, dir string) {
				t.Helper()
				testutil.AddRemote(t, dir, "upstream", "https://example.com/upstream.git")
				testutil.WriteCheckpointPushRemoteSetting(t, dir, "upstream")
			},
		},
		{
			name: "election fails closed",
			configure: func(t *testing.T, dir string) {
				t.Helper()
				testutil.WriteCheckpointPushRemoteSetting(t, dir, "gone")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := initRemoteElectionRepo(t)
			testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
			tt.configure(t, tmpDir)

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
		})
	}
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
