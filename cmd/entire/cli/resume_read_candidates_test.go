package cli

import (
	"context"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Membership in the read chain does not authorize local-ref mutation.
func TestPromoteRemoteTrackingPrimary_NonElectedOriginNeverPromotes(t *testing.T) {
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
			name: "election fails open for reads",
			configure: func(t *testing.T, dir string) {
				t.Helper()
				testutil.WriteCheckpointPushRemoteSetting(t, dir, "gone")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testutil.IsolateGitConfigEnv(t)
			dir := t.TempDir()
			testutil.InitRepo(t, dir)
			testutil.WriteFile(t, dir, "f.txt", "init")
			testutil.GitAdd(t, dir, "f.txt")
			testutil.GitCommit(t, dir, "init")
			testutil.AddRemote(t, dir, "origin", "https://example.com/origin.git")
			tt.configure(t, dir)

			staleHash := revParse(t, dir, "HEAD")
			testutil.GitUpdateRef(t, dir, "refs/remotes/origin/"+paths.MetadataBranchName, staleHash)

			t.Chdir(dir)
			ctx := context.Background()
			readRemotes := strategy.CheckpointReadRemotes(ctx)
			require.NotEmpty(t, readRemotes)
			require.Equal(t, "origin", readRemotes[len(readRemotes)-1])

			repo, err := openRepository(ctx)
			require.NoError(t, err)
			defer repo.Close()

			promoteRemoteTrackingPrimary(ctx, repo, checkpoint.ResolveRefs(ctx))

			assert.False(t, refExists(t, dir, "refs/heads/"+paths.MetadataBranchName),
				"a non-elected origin candidate must never promote into the local primary")
		})
	}
}
