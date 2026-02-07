package gitutil

import (
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePorcelainStatus_Empty(t *testing.T) {
	status := parsePorcelainStatus("")
	assert.Empty(t, status)
	assert.True(t, status.IsClean())
}

func TestParsePorcelainStatus_ModifiedFile(t *testing.T) {
	// " M file.go\0"
	raw := " M file.go\x00"
	status := parsePorcelainStatus(raw)

	require.Contains(t, status, "file.go")
	assert.Equal(t, git.Unmodified, status["file.go"].Staging)
	assert.Equal(t, git.Modified, status["file.go"].Worktree)
}

func TestParsePorcelainStatus_StagedFile(t *testing.T) {
	// "M  file.go\0"
	raw := "M  file.go\x00"
	status := parsePorcelainStatus(raw)

	require.Contains(t, status, "file.go")
	assert.Equal(t, git.Modified, status["file.go"].Staging)
	assert.Equal(t, git.Unmodified, status["file.go"].Worktree)
}

func TestParsePorcelainStatus_AddedDeletedUntracked(t *testing.T) {
	// "A  new.go\0D  old.go\0?? unknown.txt\0"
	raw := "A  new.go\x00D  old.go\x00?? unknown.txt\x00"
	status := parsePorcelainStatus(raw)

	require.Contains(t, status, "new.go")
	assert.Equal(t, git.Added, status["new.go"].Staging)

	require.Contains(t, status, "old.go")
	assert.Equal(t, git.Deleted, status["old.go"].Staging)

	require.Contains(t, status, "unknown.txt")
	assert.Equal(t, git.Untracked, status["unknown.txt"].Staging)
	assert.Equal(t, git.Untracked, status["unknown.txt"].Worktree)
}

func TestParsePorcelainStatus_Rename(t *testing.T) {
	// Rename: "R  new.go\0old.go\0"
	raw := "R  new.go\x00old.go\x00"
	status := parsePorcelainStatus(raw)

	require.Contains(t, status, "new.go")
	assert.Equal(t, git.Renamed, status["new.go"].Staging)
	assert.Equal(t, git.Unmodified, status["new.go"].Worktree)
	assert.Equal(t, "old.go", status["new.go"].Extra)

	// Old name should NOT appear as a separate entry
	assert.NotContains(t, status, "old.go")
}

func TestParsePorcelainStatus_Copy(t *testing.T) {
	// Copy: "C  dest.go\0src.go\0"
	raw := "C  dest.go\x00src.go\x00"
	status := parsePorcelainStatus(raw)

	require.Contains(t, status, "dest.go")
	assert.Equal(t, git.Copied, status["dest.go"].Staging)
	assert.Equal(t, "src.go", status["dest.go"].Extra)
	assert.NotContains(t, status, "src.go")
}

func TestParsePorcelainStatus_WorktreeRename(t *testing.T) {
	// Worktree-side rename (defensive): " R new.go\0old.go\0"
	raw := " R new.go\x00old.go\x00"
	status := parsePorcelainStatus(raw)

	require.Contains(t, status, "new.go")
	assert.Equal(t, git.Renamed, status["new.go"].Worktree)
	assert.Equal(t, "old.go", status["new.go"].Extra)
	assert.NotContains(t, status, "old.go")
}

func TestParsePorcelainStatus_MixedStagedAndWorktree(t *testing.T) {
	// Staged modify + worktree modify: "MM file.go\0"
	raw := "MM file.go\x00"
	status := parsePorcelainStatus(raw)

	require.Contains(t, status, "file.go")
	assert.Equal(t, git.Modified, status["file.go"].Staging)
	assert.Equal(t, git.Modified, status["file.go"].Worktree)
}

func TestParsePorcelainStatus_MultipleFiles(t *testing.T) {
	raw := "M  a.go\x00" +
		" M b.go\x00" +
		"A  c.go\x00" +
		"?? d.txt\x00" +
		"R  e.go\x00old_e.go\x00" +
		" D f.go\x00"

	status := parsePorcelainStatus(raw)

	assert.Len(t, status, 6)

	assert.Equal(t, git.Modified, status["a.go"].Staging)
	assert.Equal(t, git.Modified, status["b.go"].Worktree)
	assert.Equal(t, git.Added, status["c.go"].Staging)
	assert.Equal(t, git.Untracked, status["d.txt"].Worktree)
	assert.Equal(t, git.Renamed, status["e.go"].Staging)
	assert.Equal(t, "old_e.go", status["e.go"].Extra)
	assert.Equal(t, git.Deleted, status["f.go"].Worktree)
}

func TestParsePorcelainStatus_TypeChange(t *testing.T) {
	// Type change (symlink → file): "T  link.go\0"
	raw := "T  link.go\x00"
	status := parsePorcelainStatus(raw)

	require.Contains(t, status, "link.go")
	// T maps to Modified since go-git has no dedicated TypeChanged code
	assert.Equal(t, git.Modified, status["link.go"].Staging)
}

func TestParsePorcelainStatus_UpdatedButUnmerged(t *testing.T) {
	// Merge conflict: "UU conflict.go\0"
	raw := "UU conflict.go\x00"
	status := parsePorcelainStatus(raw)

	require.Contains(t, status, "conflict.go")
	assert.Equal(t, git.UpdatedButUnmerged, status["conflict.go"].Staging)
	assert.Equal(t, git.UpdatedButUnmerged, status["conflict.go"].Worktree)
}

func TestPorcelainStatusCode_AllCodes(t *testing.T) {
	tests := []struct {
		input    byte
		expected git.StatusCode
	}{
		{' ', git.Unmodified},
		{'M', git.Modified},
		{'A', git.Added},
		{'D', git.Deleted},
		{'R', git.Renamed},
		{'C', git.Copied},
		{'?', git.Untracked},
		{'!', git.Untracked},
		{'U', git.UpdatedButUnmerged},
		{'T', git.Modified},
		{'X', git.Unmodified}, // unknown → Unmodified
	}

	for _, tt := range tests {
		t.Run(string(tt.input), func(t *testing.T) {
			assert.Equal(t, tt.expected, porcelainStatusCode(tt.input))
		})
	}
}

func TestParseNulDelimitedNames_Empty(t *testing.T) {
	assert.Nil(t, parseNulDelimitedNames(""))
}

func TestParseNulDelimitedNames_SingleFile(t *testing.T) {
	names := parseNulDelimitedNames("file.go\x00")
	assert.Equal(t, []string{"file.go"}, names)
}

func TestParseNulDelimitedNames_MultipleFiles(t *testing.T) {
	names := parseNulDelimitedNames("a.go\x00b.go\x00c.txt\x00")
	assert.Equal(t, []string{"a.go", "b.go", "c.txt"}, names)
}

func TestParseNulDelimitedNames_NoTrailingNul(t *testing.T) {
	// git output may or may not have a trailing NUL
	names := parseNulDelimitedNames("a.go\x00b.go")
	assert.Equal(t, []string{"a.go", "b.go"}, names)
}
