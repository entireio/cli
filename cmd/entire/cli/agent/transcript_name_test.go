package agent

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTranscriptNameUnderHome_FoldCaseKeepsPathSpelling pins that the
// Windows comparison folds case only for containment: the returned name is
// joined onto a home and persisted, so it keeps the transcript's own casing.
func TestTranscriptNameUnderHome_FoldCaseKeepsPathSpelling(t *testing.T) {
	t.Parallel()

	home := filepath.FromSlash("/Users/Bob/.claude")
	path := filepath.FromSlash("/users/bob/.claude/projects/MyProject/Session.jsonl")

	rel, ok := transcriptNameUnderHome(path, home, true)
	require.True(t, ok)
	require.Equal(t, filepath.FromSlash("projects/MyProject/Session.jsonl"), rel)

	_, ok = transcriptNameUnderHome(path, home, false)
	require.False(t, ok, "case-sensitive comparison must not match a differently cased home")

	_, ok = transcriptNameUnderHome(filepath.FromSlash("/users/bob/other/s.jsonl"), home, true)
	require.False(t, ok)
	_, ok = transcriptNameUnderHome(home, home, true)
	require.False(t, ok, "the home itself is not a name beneath it")
}
