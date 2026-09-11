package agent_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/stretchr/testify/require"
)

func TestReadTranscriptFile_RejectsLeafSymlinkInEntire(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	root, err := entiredir.OpenAt(worktree)
	require.NoError(t, err)
	require.NoError(t, osroot.MkdirAllNoSymlink(root, "tmp", 0o750))
	target := filepath.Join(worktree, paths.EntireDir, "tmp", "target.jsonl")
	require.NoError(t, os.WriteFile(target, []byte("secret"), 0o600))
	link := filepath.Join(worktree, paths.EntireDir, "tmp", "transcript.jsonl")
	if err := os.Symlink("target.jsonl", link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	_, err = agent.ReadTranscriptFile(link)
	require.ErrorIs(t, err, osroot.ErrSymlinkedPath)
}

func TestReadTranscriptFile_AllowsExternalAgentFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("external"), 0o600))
	got, err := agent.ReadTranscriptFile(path)
	require.NoError(t, err)
	require.Equal(t, "external", string(got))
}
