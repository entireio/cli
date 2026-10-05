package agent_test

import (
	"os"
	"path/filepath"
	"runtime"
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

func TestCheckTranscriptReadable_ExternalPaths(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	readable := filepath.Join(dir, "transcript.jsonl")
	require.NoError(t, os.WriteFile(readable, []byte("external"), 0o600))
	require.NoError(t, agent.CheckTranscriptReadable(readable))

	require.ErrorIs(t, agent.CheckTranscriptReadable(filepath.Join(dir, "missing.jsonl")), os.ErrNotExist)
	require.Error(t, agent.CheckTranscriptReadable(dir), "a directory is not a readable transcript")

	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		return // root ignores, and Windows lacks, the permission bits removed below
	}
	unreadable := filepath.Join(dir, "unreadable.jsonl")
	require.NoError(t, os.WriteFile(unreadable, []byte("external"), 0o600))
	require.NoError(t, os.Chmod(unreadable, 0o000))
	require.ErrorIs(t, agent.CheckTranscriptReadable(unreadable), os.ErrPermission)
}
