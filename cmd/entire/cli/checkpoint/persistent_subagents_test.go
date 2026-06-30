package checkpoint

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// TestUpdateCommitted_ReplacesSubagents proves the turn-end finalize update path
// (SessionTranscript with Subagents -> replaceSubagents) writes the subagent
// roster into the per-session metadata.json on the pushed v1 branch. This is the
// path that publishes subagents recorded after the last condensation.
func TestUpdateCommitted_ReplacesSubagents(t *testing.T) {
	t.Parallel()
	repo, store, cpID := setupRepoForUpdate(t)

	err := store.Write(context.Background(), SessionTranscript{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Subagents: []SubagentLink{
			{ToolUseID: "t1", AgentID: "a1", SubagentType: "dev", Description: "build", FileCount: 2},
			{ToolUseID: "t2", AgentID: "a2", SubagentType: "reviewer"},
		},
	})
	require.NoError(t, err)

	ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)
	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)
	f, err := tree.File(cpID.Path() + "/0/" + paths.MetadataFileName)
	require.NoError(t, err)
	content, err := f.Contents()
	require.NoError(t, err)

	var meta Metadata
	require.NoError(t, json.Unmarshal([]byte(content), &meta))
	require.Len(t, meta.Subagents, 2, "finalize update must publish the subagent roster into metadata.json")
	require.Equal(t, "t1", meta.Subagents[0].ToolUseID)
	require.Equal(t, "a1", meta.Subagents[0].AgentID)
	require.Equal(t, 2, meta.Subagents[0].FileCount)
	require.Equal(t, "reviewer", meta.Subagents[1].SubagentType)
}
