package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/entireio/cli/cmd/entire/cli/session"
)

// Adopting a session into another worktree of the same repository keeps its
// edits elsewhere linkable: the old home's pending files become an entry, and
// any entry for the new home is dropped (its files come from its status).
func TestAdoptedOtherWorktrees(t *testing.T) {
	t.Parallel()
	source := &session.State{
		WorktreePath:      "/r/main",
		WorktreeID:        "",
		BaseCommit:        "base",
		FilesTouched:      []string{"home.go"},
		TouchedFileHashes: map[string]string{"home.go": "h1"},
		OtherWorktrees: map[string]*session.WorktreeWork{
			"target": {Path: "/r/target", FilesTouched: []string{"t.go"}},
			"other":  {Path: "/r/other", FilesTouched: []string{"o.go"}},
		},
	}
	got := adoptedOtherWorktrees(source, "target", "", true)
	assert.NotContains(t, got, "target")
	assert.Equal(t, []string{"o.go"}, got["other"].FilesTouched)
	home := got[session.WorktreeKey("")]
	if assert.NotNil(t, home) {
		assert.Equal(t, []string{"home.go"}, home.FilesTouched)
		assert.Equal(t, "h1", home.TouchedFileHashes["home.go"])
		assert.Equal(t, "base", home.BaseCommit)
	}
	// A linked source home records its marker, and is left out unmarked.
	linkedSource := &session.State{WorktreePath: "/r/feat", WorktreeID: "feat", FilesTouched: []string{"f.go"}}
	assert.Equal(t, "m1", adoptedOtherWorktrees(linkedSource, "target", "m1", true)["feat"].Incarnation)
	assert.Nil(t, adoptedOtherWorktrees(linkedSource, "target", "", false))
	// The clone is independent of the source.
	got["other"].FilesTouched[0] = "changed"
	assert.Equal(t, "o.go", source.OtherWorktrees["other"].FilesTouched[0])
}
