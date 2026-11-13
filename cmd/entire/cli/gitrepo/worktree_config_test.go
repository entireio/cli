package gitrepo

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
)

// A bare repository with extensions.worktreeConfig and a config.worktree opens:
// go-git returns the merged config with a nil Raw, which the bare check must
// not dereference (#3154).
func TestOpenPath_BareWithWorktreeConfig(t *testing.T) {
	t.Parallel()
	seed := t.TempDir()
	gitenv.Run(t, seed, "init", "-q")
	gitenv.Run(t, seed, "commit", "-q", "--allow-empty", "--no-gpg-sign", "-m", "seed")
	bare := filepath.Join(t.TempDir(), "repo.git")
	gitenv.Run(t, seed, "clone", "-q", "--bare", seed, bare)
	gitenv.Run(t, bare, "config", "extensions.worktreeConfig", "true")
	gitenv.Run(t, bare, "config", "--worktree", "core.sparseCheckout", "true")
	_, err := os.Stat(filepath.Join(bare, "config.worktree"))
	require.NoError(t, err)

	repo, err := OpenPath(bare)
	require.NoError(t, err)
	repo.Close()
}
