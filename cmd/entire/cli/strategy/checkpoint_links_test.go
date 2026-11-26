package strategy

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	cpkg "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The journal another repository appends to is folded into the session's
// list in journal order, before the home checkpoint being recorded, and
// folding twice adds nothing.
func TestRecordHomeCheckpointLink_FoldsJournalFirstAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	ctx := context.Background()

	state := &SessionState{SessionID: "s1", TurnID: "turn-2"}
	state.AddCheckpointLink(session.CheckpointLink{CheckpointID: "000000000001", Kind: session.CheckpointLinkHome})
	home := &crossRepoHome{state: state, commonDir: filepath.Join(dir, ".git")}
	for i, cp := range []string{"0000000000d1", "0000000000d2"} {
		require.NoError(t, appendCrossRepoLink(home, cpkg.LinkedRepoCommit{
			RepoKey: "gh/o/d", SHA: []string{"d1sha", "d2sha"}[i], CheckpointID: cp,
			LinkedBy: linkedByTrailer, TurnID: "turn-2", At: time.Now().UTC(),
		}))
	}

	recordHomeCheckpointLink(ctx, state, "000000000002", "c2sha")
	foldCrossRepoLinks(ctx, state)

	got := make([]id.CheckpointID, 0, len(state.CheckpointLinks))
	for _, l := range state.CheckpointLinks {
		got = append(got, l.CheckpointID)
	}
	assert.Equal(t, []id.CheckpointID{"000000000001", "0000000000d1", "0000000000d2", "000000000002"}, got)
	d1 := state.CheckpointLinks[1]
	assert.Equal(t, session.CheckpointLinkCrossRepo, d1.Kind)
	assert.Equal(t, []string{"d1sha"}, d1.LinkedCommits)
	assert.Equal(t, "turn-2", d1.TurnID)
	assert.Equal(t, "gh/o/d", d1.RepoKey)
	last := state.CheckpointLinks[3]
	assert.Equal(t, session.CheckpointLinkHome, last.Kind)
	assert.Equal(t, []string{"c2sha"}, last.LinkedCommits)
	assert.Equal(t, "turn-2", last.TurnID)
	assert.Equal(t, id.CheckpointID("0000000000d2"), state.PreviousCheckpoint("000000000002"))
}

func TestClearDeletedCheckpointFromState_DropsTheLink(t *testing.T) {
	t.Parallel()
	st := &SessionState{CheckpointLinks: []session.CheckpointLink{{CheckpointID: "000000000001"}, {CheckpointID: "000000000002"}}}
	assert.Contains(t, deleteStateFields(st, "000000000001"), "checkpoint_links")
	assert.True(t, clearDeletedCheckpointFromState(st, "000000000001"))
	assert.Len(t, st.CheckpointLinks, 1)
	assert.False(t, clearDeletedCheckpointFromState(st, "000000000009"))
}
