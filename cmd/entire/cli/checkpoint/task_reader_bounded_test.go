package checkpoint

import (
	"context"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/redact"
	"github.com/stretchr/testify/require"
)

// TestReadTaskBlob_RefusesBlobsOverTheLimit: task.json and the subagent
// transcript are pushed data, so the reader must not materialize an
// arbitrarily large blob. A blob at the limit reads; one byte over does not.
func TestReadTaskBlob_RefusesBlobsOverTheLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cid := id.MustCheckpointID("aabbccdd0303")
	content := []byte("0123456789\n")
	require.NoError(t, store.Write(ctx, Session{
		CheckpointID:     cid,
		SessionID:        "bounded-read-session",
		Strategy:         "manual-commit",
		Transcript:       redact.AlreadyRedacted([]byte(`{"msg":"parent"}` + "\n")),
		CheckpointsCount: 1,
		AuthorName:       "Test Author",
		AuthorEmail:      "test@example.com",
		Tasks: []TaskPayload{{
			ToolUseID:  "toolu_bounded",
			AgentID:    "agentbounded",
			Transcript: redact.AlreadyRedacted(content),
		}},
	}))
	tree, err := store.getCheckpointFetchingTree(ctx, cid)
	require.NoError(t, err)
	file, err := tree.File("tasks/toolu_bounded/agent-agentbounded.jsonl")
	require.NoError(t, err)

	got, err := readTaskBlob(file, int64(len(content)))
	require.NoError(t, err)
	require.Equal(t, content, got)

	_, err = readTaskBlob(file, int64(len(content))-1)
	require.ErrorContains(t, err, "exceeds")
}
