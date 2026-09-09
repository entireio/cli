package cli

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"
)

func TestCountLocalCheckpointRefs_IgnoresMalformedRefs(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	repo, err := gitrepo.OpenPath(tmpDir)
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)

	good1, err := checkpoint.RefName(id.MustCheckpointID("a1b2c3d4e5f6"))
	require.NoError(t, err)
	good2, err := checkpoint.RefName(id.MustCheckpointID("0f0f0f0f0f0f"))
	require.NoError(t, err)
	for _, name := range []plumbing.ReferenceName{
		good1,
		good2,
		"refs/entire/checkpoints/zz/a1b2c3d4e5f6", // wrong shard
		"refs/entire/checkpoints/a1b2c3d4e5f6",    // missing shard
		"refs/heads/entire/checkpoints/v1",        // the branch, not a ref
	} {
		require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(name, head.Hash())))
	}

	n, err := countLocalCheckpointRefs(repo)
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	refs, err := listLocalCheckpointRefs(repo)
	require.NoError(t, err)
	assert.Equal(t, []plumbing.ReferenceName{good2, good1}, refs, "sorted by name")
}

func TestComputeCheckpointValue_FromCommittedCheckpoints(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "init.txt", "init")
	testutil.GitAdd(t, tmpDir, "init.txt")
	testutil.GitCommit(t, tmpDir, "init")
	ctx := settings.WithWorktreeRoot(context.Background(), tmpDir)

	repo, err := gitrepo.OpenPath(tmpDir)
	require.NoError(t, err)
	v1Refs := checkpoint.DefaultV1Refs()
	stores, err := checkpoint.Open(ctx, repo, checkpoint.OpenOptions{Refs: &v1Refs})
	require.NoError(t, err)
	store := stores.Persistent

	write := func(cid string, sessionID string, ag types.AgentType, in, out int) {
		require.NoError(t, store.Write(ctx, checkpoint.Session{
			CheckpointID: id.MustCheckpointID(cid),
			SessionID:    sessionID,
			Strategy:     "manual-commit",
			Transcript:   redact.AlreadyRedacted([]byte("line\n")),
			Prompts:      []string{"hello"},
			AuthorName:   "Test",
			AuthorEmail:  "test@test.com",
			Agent:        ag,
			TokenUsage:   &agent.TokenUsage{InputTokens: in, OutputTokens: out},
		}))
	}
	write("a1b2c3d4e5f6", "session-1", agent.AgentTypeClaudeCode, 1000, 500)
	write("b1b2c3d4e5f6", "session-2", agent.AgentTypeClaudeCode, 2000, 500)
	write("c1b2c3d4e5f6", "session-1", agent.AgentTypeCursor, 0, 0)

	v, err := computeCheckpointValue(ctx, repo)
	require.NoError(t, err)
	assert.Equal(t, 3, v.Checkpoints)
	assert.Equal(t, 2, v.Sessions)
	assert.Equal(t, []string{string(agent.AgentTypeClaudeCode), string(agent.AgentTypeCursor)}, v.Agents)
	assert.Equal(t, 4000, v.Tokens)
	assert.Equal(t, 2, v.TokensSampled, "a checkpoint with no token data does not count as sampled")
	assert.False(t, v.TokensCapped)
	assert.False(t, v.First.IsZero())
	assert.False(t, v.Last.Before(v.First))
}

func TestCheckpointValue_TokensPhrase(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "4k tokens across your 3 checkpoints", checkpointValue{Checkpoints: 3, Tokens: 4000, TokensSampled: 3}.tokensPhrase())
	assert.Equal(t, "18.4M tokens across your 500 most recent checkpoints",
		checkpointValue{Checkpoints: 1200, Tokens: 18_400_000, TokensSampled: 500, TokensCapped: true}.tokensPhrase())
	assert.Equal(t, "1.2k tokens across 212 of your checkpoints",
		checkpointValue{Checkpoints: 300, Tokens: 1200, TokensSampled: 212, TokensCapped: true}.tokensPhrase(),
		"a time-budget cutoff names the sample size, not the cap")
	assert.Equal(t, "9 tokens across your 1 checkpoint", checkpointValue{Checkpoints: 1, Tokens: 9, TokensSampled: 1}.tokensPhrase())
}

func TestRenderCheckpointValue(t *testing.T) {
	t.Parallel()

	first := time.Date(2026, time.March, 12, 9, 0, 0, 0, time.UTC)
	last := time.Date(2026, time.September, 6, 18, 0, 0, 0, time.UTC)

	var buf bytes.Buffer
	renderCheckpointValue(&buf, newStatusStyles(&buf), checkpointValue{
		Checkpoints: 142, Sessions: 37, Agents: []string{"Claude Code", "Codex"},
		Tokens: 18_400_000, TokensSampled: 142, First: first, Last: last,
	})
	assert.Equal(t, "  Checkpoints  142\n"+
		"  Sessions     37\n"+
		"  Agents       Claude Code, Codex\n"+
		"  Agent work   18.4M tokens across your 142 checkpoints\n"+
		"  From         12 Mar 2026 to 6 Sep 2026\n", buf.String())

	buf.Reset()
	renderCheckpointValue(&buf, newStatusStyles(&buf), checkpointValue{Checkpoints: 1, First: first, Last: first.Add(time.Hour)})
	assert.Equal(t, "  Checkpoints  1\n  From         12 Mar 2026\n", buf.String(), "empty rows are omitted and a same-day span collapses")

	buf.Reset()
	renderCheckpointValue(&buf, newStatusStyles(&buf), checkpointValue{})
	assert.Equal(t, "  Checkpoints  0\n", buf.String())
}
