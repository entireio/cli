package cli

import (
	"errors"
	"fmt"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

func TestWithLegacyDivergedFix(t *testing.T) {
	t.Parallel()
	cid := id.MustCheckpointID("a1b2c3d4e5f6")
	local := plumbing.NewBranchReferenceName("entire/checkpoints/v1")

	t.Run("names the remote and the command", func(t *testing.T) {
		t.Parallel()
		diverged := fmt.Errorf("write: %w", &checkpoint.LegacyCheckpointDivergedError{
			CheckpointID: cid,
			Refs:         [2]plumbing.ReferenceName{local, plumbing.NewRemoteReferenceName("origin", "entire/checkpoints/v1")},
		})

		err := withLegacyDivergedFix(diverged)

		require.EqualError(t, err, "checkpoint a1b2c3d4e5f6 has different copies on this clone and origin. "+
			"Run 'entire doctor migrate-checkpoints' to choose one.")
		assert.ErrorIs(t, err, checkpoint.ErrLegacyCheckpointDiverged)
	})

	t.Run("without a remote-tracking ref", func(t *testing.T) {
		t.Parallel()
		err := withLegacyDivergedFix(&checkpoint.LegacyCheckpointDivergedError{CheckpointID: cid, Refs: [2]plumbing.ReferenceName{local, local}})

		require.EqualError(t, err, "checkpoint a1b2c3d4e5f6 has different copies on this clone and a remote. "+
			"Run 'entire doctor migrate-checkpoints' to choose one.")
	})

	t.Run("leaves other errors alone", func(t *testing.T) {
		t.Parallel()
		other := errors.New("disk full")

		assert.Same(t, other, withLegacyDivergedFix(other))
	})
}

// Session IDs in a remote's copy are not trusted, so escape sequences in them
// must not reach the terminal and redraw the prompt.
func TestLegacySessionList_StripsControlCharacters(t *testing.T) {
	t.Parallel()
	listed := legacySessionList([]string{"ok", "evil\x1b]0;title\x07\x1b[2K\u202e"}, []string{"ok"})

	assert.Equal(t, "ok (changed), evil]0;title[2K", listed)
}
