package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/worktreedir"
)

// TestSplitSubagentFiles_KeepsTheCommittedSpellingOfAStagedRenameWindows: a
// foreground subagent edits README.md, stages a case-only rename to
// readme.md, and commits only README.md. HEAD then holds the edit as
// README.md while the index spells the file readme.md. The edit must count as
// committed under HEAD's spelling, which attachTaskToCommitCheckpoint matches
// against the commit's checkpoint, and the staged rename stays pending as
// readme.md. CI's Windows job runs it by its name.
func TestSplitSubagentFiles_KeepsTheCommittedSpellingOfAStagedRenameWindows(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// The case probe keeps dir's root open; Windows cannot remove an open
	// directory.
	t.Cleanup(func() { osroot.Forget(dir) })
	testutil.InitRepo(t, dir)
	if !worktreedir.NewCaseProbe(dir).IgnoresCase(".") {
		t.Skip("a case-only rename is two files on a case-sensitive filesystem")
	}
	// As git init sets it on such a filesystem; go-git's init does not.
	testutil.RunGit(t, dir, "config", "core.ignorecase", "true")
	testutil.WriteFile(t, dir, "README.md", "readme\n")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "Add README.md")

	testutil.RunGit(t, dir, "mv", "-f", "README.md", "readme.md")
	testutil.WriteFile(t, dir, "README.md", "readme, edited\n")
	testutil.RunGit(t, dir, "commit", "-q", "-m", "Commit the edit", "--", "README.md")
	require.Equal(t, "R  README.md -> readme.md", strings.TrimSpace(testutil.RunGit(t, dir, "status", "--porcelain", "--", "README.md", "readme.md")),
		"fixture: the commit took the edit and left the rename staged")

	uncommitted, committed := splitSubagentFiles(context.Background(), dir, []string{"README.md"})
	require.Equal(t, []string{"readme.md"}, uncommitted, "the staged rename is still to be committed")
	require.Equal(t, []string{"README.md"}, committed, "HEAD holds the subagent's edit as README.md")

	// Committing the rename leaves nothing pending, under the index's spelling.
	testutil.RunGit(t, dir, "commit", "-q", "-m", "Commit the rename")
	uncommitted, committed = splitSubagentFiles(context.Background(), dir, []string{"README.md"})
	require.Empty(t, uncommitted, "the rename is committed")
	require.Equal(t, []string{"readme.md"}, committed, "HEAD holds the file as readme.md now")
}
