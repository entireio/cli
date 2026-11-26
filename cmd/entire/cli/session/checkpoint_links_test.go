package session

import (
	"context"
	"encoding/json"
	"testing"
	"time"

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

func checkpointIDs(s *State) []id.CheckpointID {
	ids := make([]id.CheckpointID, 0, len(s.CheckpointLinks))
	for _, l := range s.CheckpointLinks {
		ids = append(ids, l.CheckpointID)
	}
	return ids
}

// Links recorded out of commit order (a turn-end sweep linking commits made
// before a same-turn home condensation) are kept in commit order, ties in
// insertion order, and the predecessor follows that order.
func TestAddCheckpointLink_KeepsCommitOrder(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	at := func(sec int) time.Time { return base.Add(time.Duration(sec) * time.Second) }
	s := &State{}
	s.AddCheckpointLink(CheckpointLink{CheckpointID: "00000000000c", CommittedAt: at(3), At: at(4)})
	s.AddCheckpointLink(CheckpointLink{CheckpointID: "00000000000d", CommittedAt: at(1), At: at(9)})
	s.AddCheckpointLink(CheckpointLink{CheckpointID: "00000000000e", CommittedAt: at(2), At: at(9)})
	s.AddCheckpointLink(CheckpointLink{CheckpointID: "00000000000f", CommittedAt: at(3), At: at(9)})
	s.AddCheckpointLink(CheckpointLink{CheckpointID: "000000000010", CommittedAt: at(9)})

	assert.Equal(t, []id.CheckpointID{"00000000000d", "00000000000e", "00000000000c", "00000000000f", "000000000010"}, checkpointIDs(s),
		"sorted by commit time; the tie at 3s keeps insertion order")
	assert.Equal(t, id.CheckpointID("00000000000e"), s.PreviousCheckpoint("00000000000c"))
	assert.Equal(t, id.CheckpointID("00000000000c"), s.PreviousCheckpoint("00000000000f"))
	assert.True(t, s.PreviousCheckpoint("00000000000d").IsEmpty())
}

// Entries written before committed_at existed sort by At, and one with no
// time at all (seeded from LastCheckpointID) keeps its place at the front.
func TestAddCheckpointLink_MigratedEntriesKeepTheirPlace(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	s := &State{CheckpointLinks: []CheckpointLink{
		{CheckpointID: "000000000001", Kind: CheckpointLinkHome},
		{CheckpointID: "000000000002", At: base.Add(5 * time.Second)},
	}}
	s.AddCheckpointLink(CheckpointLink{CheckpointID: "000000000003", CommittedAt: base})
	s.AddCheckpointLink(CheckpointLink{CheckpointID: "000000000004"})
	assert.Equal(t, []id.CheckpointID{"000000000001", "000000000003", "000000000002", "000000000004"}, checkpointIDs(s))
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
