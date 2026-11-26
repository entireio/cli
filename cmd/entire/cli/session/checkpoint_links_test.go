package session

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAddCheckpointLink_AppendsInOrderAndIsIdempotent(t *testing.T) {
	t.Parallel()
	s := &State{}
	assert.True(t, s.AddCheckpointLink(CheckpointLink{CheckpointID: "aaaaaaaaaaaa", Kind: CheckpointLinkHome, LinkedCommits: []string{"sha1"}, TurnID: "t1"}))
	assert.True(t, s.AddCheckpointLink(CheckpointLink{CheckpointID: "bbbbbbbbbbbb", Kind: CheckpointLinkCrossRepo, RepoKey: "gh/o/d", LinkedCommits: []string{"sha2"}}))
	assert.False(t, s.AddCheckpointLink(CheckpointLink{CheckpointID: "aaaaaaaaaaaa", LinkedCommits: []string{"sha1"}}), "a repeat adds nothing")
	assert.False(t, s.AddCheckpointLink(CheckpointLink{}), "a link without a checkpoint is ignored")
	assert.True(t, s.AddCheckpointLink(CheckpointLink{CheckpointID: "aaaaaaaaaaaa", LinkedCommits: []string{"sha3"}}), "a new commit for a listed checkpoint is merged")

	require.Len(t, s.CheckpointLinks, 2)
	assert.Equal(t, []string{"sha1", "sha3"}, s.CheckpointLinks[0].LinkedCommits)
	assert.Equal(t, "t1", s.CheckpointLinks[0].TurnID)
	assert.Equal(t, CheckpointLinkCrossRepo, s.CheckpointLinks[1].Kind)
	assert.Equal(t, id.CheckpointID("aaaaaaaaaaaa"), s.PreviousCheckpoint("bbbbbbbbbbbb"))
	assert.True(t, s.PreviousCheckpoint("aaaaaaaaaaaa").IsEmpty())
	assert.True(t, s.PreviousCheckpoint("cccccccccccc").IsEmpty())

	s.RemoveCheckpointLink("aaaaaaaaaaaa")
	require.Len(t, s.CheckpointLinks, 1)
	assert.Equal(t, id.CheckpointID("bbbbbbbbbbbb"), s.CheckpointLinks[0].CheckpointID)
}

func TestNormalizeAfterLoad_SeedsCheckpointLinksFromLastCheckpoint(t *testing.T) {
	t.Parallel()
	var legacy State
	require.NoError(t, json.Unmarshal([]byte(`{"session_id":"s1","phase":"idle","last_checkpoint_id":"abcdef123456"}`), &legacy))
	legacy.NormalizeAfterLoad(context.Background())
	require.Len(t, legacy.CheckpointLinks, 1)
	assert.Equal(t, CheckpointLink{CheckpointID: "abcdef123456", Kind: CheckpointLinkHome}, legacy.CheckpointLinks[0])

	var empty State
	require.NoError(t, json.Unmarshal([]byte(`{"session_id":"s2","phase":"idle"}`), &empty))
	empty.NormalizeAfterLoad(context.Background())
	assert.Empty(t, empty.CheckpointLinks, "a session never condensed has nothing to seed")

	listed := State{SessionID: "s3", LastCheckpointID: "abcdef123456", CheckpointLinks: []CheckpointLink{{CheckpointID: "111111111111", Kind: CheckpointLinkHome}}}
	listed.NormalizeAfterLoad(context.Background())
	assert.Len(t, listed.CheckpointLinks, 1, "an existing list is never reseeded")
}

func TestEntireSessionID(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "s1", (&State{SessionID: "s1"}).EntireSessionID())
	assert.Equal(t, "Unknown Agent:s1", (&State{SessionID: "s1", AgentType: "Unknown Agent"}).EntireSessionID())
}
