package session

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Edits recorded for other worktrees round-trip, leave the home's fields
// where older CLIs read them, and count as pending work.
func TestStateOtherWorktrees(t *testing.T) {
	t.Parallel()
	state := State{
		SessionID:    "s",
		FilesTouched: []string{"home.go"},
		OtherWorktrees: map[string]*WorktreeWork{
			WorktreeKey("wt"): {Path: "/r/wt", BaseCommit: "abc", FilesTouched: []string{"a.go", "b.go"}, TouchedFileHashes: map[string]string{"a.go": "h"}},
		},
		LinkedByWorktreeEntry: true,
	}
	data, err := json.Marshal(state)
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))
	assert.Equal(t, []any{"home.go"}, raw["files_touched"], "the home's files keep their key")
	assert.NotContains(t, raw, "LinkedByWorktreeEntry", "the in-memory marker is never stored")

	var back State
	require.NoError(t, json.Unmarshal(data, &back))
	assert.Equal(t, state.OtherWorktrees, back.OtherWorktrees)
	assert.False(t, back.LinkedByWorktreeEntry)

	// A state file written before the field existed loads with none.
	var legacy State
	require.NoError(t, json.Unmarshal([]byte(`{"session_id":"s","files_touched":["x"]}`), &legacy))
	assert.Nil(t, legacy.OtherWorktrees)
}

func TestStateHasPendingFiles(t *testing.T) {
	t.Parallel()
	onlyElsewhere := State{OtherWorktrees: map[string]*WorktreeWork{"wt": {FilesTouched: []string{"a.go"}}}}
	assert.True(t, onlyElsewhere.HasPendingFiles())
	assert.True(t, onlyElsewhere.HasPendingWork(), "files in another worktree are pending work")
	assert.Equal(t, 1, onlyElsewhere.PendingFileCount())

	emptyEntries := State{OtherWorktrees: map[string]*WorktreeWork{"wt": {}, "x": nil}}
	assert.False(t, emptyEntries.HasPendingFiles())
	assert.False(t, emptyEntries.HasPendingWork())

	both := State{FilesTouched: []string{"h.go"}, OtherWorktrees: map[string]*WorktreeWork{"wt": {FilesTouched: []string{"a.go", "b.go"}}}}
	assert.Equal(t, 3, both.PendingFileCount())
	assert.Equal(t, []string{"a.go", "b.go", "h.go"}, both.PendingFiles())
	assert.Equal(t, []string{"a.go"}, onlyElsewhere.PendingFiles(), "files only in another worktree are pending files")

	// A path pending in two worktrees is one repository path, in the list and
	// the count alike.
	sameName := State{FilesTouched: []string{"README.md"}, OtherWorktrees: map[string]*WorktreeWork{"wt": {FilesTouched: []string{"README.md", "c.go"}}}}
	assert.Equal(t, []string{"README.md", "c.go"}, sameName.PendingFiles())
	assert.Equal(t, 2, sameName.PendingFileCount(), "the count disagrees with the list")
}

func TestWorktreeKey(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "/", WorktreeKey(""), "the main worktree's key can't collide with a linked worktree's ID")
	assert.Equal(t, "feature", WorktreeKey("feature"))
}
