package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Hooks report one worktree under different spellings (a symlinked temp dir
// such as macOS /var vs /private/var, a trailing slash). Pending content
// recorded under both is still from one worktree; calling it "several" would
// pin the session to its home forever.
func TestPendingContentWorktree_SameDirUnderAnotherSpelling(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(dir, link))

	s := &State{}
	s.NotePendingContentAt(dir, false)
	s.NotePendingContentAt(link, true)
	s.NotePendingContentAt(dir+string(filepath.Separator), true)

	require.NotEqual(t, PendingContentInSeveralWorktrees, s.PendingContentWorktree)
	require.True(t, s.PendingContentRecordedOnlyIn(link))
	require.True(t, s.PendingContentRecordedOnlyIn(dir))

	other := t.TempDir()
	s.NotePendingContentAt(other, true)
	require.Equal(t, PendingContentInSeveralWorktrees, s.PendingContentWorktree, "a genuinely different tree is still several")
}
