package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/entireio/cli/cmd/entire/cli/agent/claudecode" // registers .claude as a protected dir
	"github.com/stretchr/testify/require"
)

// An atomic write into an agent config directory leaves its temp file beside
// the target, so a process killed between the create and the rename strands
// one. Nothing else looks there, so it survives until swept — and Entire diffs
// untracked files across a turn, which is how a stray one ends up captured
// into a checkpoint.
func TestListOrphanAgentTemps_FindsAndRemovesStrandedTempFiles(t *testing.T) {
	tmpDir := setupTestRepo(t)

	claudeDir := filepath.Join(tmpDir, ".claude")
	require.NoError(t, os.MkdirAll(claudeDir, 0o750))
	orphan := "settings.json.0123456789abcdef.tmp"
	require.NoError(t, os.WriteFile(filepath.Join(claudeDir, orphan), []byte("partial"), 0o600))
	// Real files, and a plausible non-temp neighbour, must be left alone.
	require.NoError(t, os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte("{}"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(claudeDir, "notes.tmp"), []byte("mine"), 0o600))

	ctx := context.Background()
	found, err := listOrphanAgentTemps(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{".claude/" + orphan}, found)

	deleted, failed := deleteOrphanAgentTemps(ctx, found)
	require.Empty(t, failed)
	require.Equal(t, found, deleted)

	_, err = os.Stat(filepath.Join(claudeDir, orphan))
	require.True(t, os.IsNotExist(err), "the stray temp must be gone")
	for _, keep := range []string{"settings.json", "notes.tmp"} {
		_, err := os.Stat(filepath.Join(claudeDir, keep))
		require.NoError(t, err, "%s must be left alone", keep)
	}
}

// A repository can ship a symlink at .claude. Sweeping is a tidy-up, so it
// skips rather than failing the whole clean; doctor is what reports the link.
func TestListOrphanAgentTemps_SkipsASymlinkedAgentDirectory(t *testing.T) {
	tmpDir := setupTestRepo(t)

	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "x.0123456789abcdef.tmp"), []byte("x"), 0o600))
	if err := os.Symlink(outside, filepath.Join(tmpDir, ".claude")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	found, err := listOrphanAgentTemps(context.Background())
	require.NoError(t, err)
	require.Empty(t, found)

	_, statErr := os.Stat(filepath.Join(outside, "x.0123456789abcdef.tmp"))
	require.NoError(t, statErr, "nothing beyond the link may be touched")
}

func TestListOrphanAgentTemps_NoAgentDirsIsNotAnError(t *testing.T) {
	setupTestRepo(t)
	found, err := listOrphanAgentTemps(context.Background())
	require.NoError(t, err)
	require.Empty(t, found)
}
