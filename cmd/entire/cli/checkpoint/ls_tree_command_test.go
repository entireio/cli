package checkpoint

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestCheckpointLsTreeCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		id       string
		basePath string
		want     string
	}{
		{name: "refs store lists the checkpoint's own ref", id: "01M3PWG7BKWYH0XJKS810J0XEX", basePath: "", want: "git ls-tree refs/entire/checkpoints/EX/01M3PWG7BKWYH0XJKS810J0XEX"},
		{name: "branch store lists the sharded v1 directory", id: "abc123def456", basePath: "ab/c123def456/", want: "git ls-tree entire/checkpoints/v1 ab/c123def456/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := (&treeWriter{}).checkpointLsTreeCommand(id.MustCheckpointID(tt.id), tt.basePath); got != tt.want {
				t.Errorf("checkpointLsTreeCommand() = %q, want %q", got, tt.want)
			}
		})
	}
}

// After a rename to the lowercase shard the uppercase name no longer resolves,
// so the hint names the ref that holds the checkpoint.
func TestCheckpointLsTreeCommand_NamesTheStoredRef(t *testing.T) {
	t.Parallel()
	dir, repo := shardRenameRepo(t, "true")
	chain := commitChain(t, dir, 1)
	testutil.RunGit(t, dir, "update-ref", "refs/entire/checkpoints/6B/01M2DCHJCHTR9T9MZTSB7WV76B", chain[0])
	r := repo()
	_, err := NormalizeShardSpelling(context.Background(), r)
	require.NoError(t, err)

	tw := &treeWriter{repo: r}
	assert.Equal(t, "git ls-tree refs/entire/checkpoints/6b/01M2DCHJCHTR9T9MZTSB7WV76B",
		tw.checkpointLsTreeCommand(foldableULID, ""))
}
