package gitrepo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
)

func TestOpenPath_MultipleAlternates(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	altADir := t.TempDir()
	altBDir := t.TempDir()
	altIntermediateDir := t.TempDir()
	altNestedDir := t.TempDir()

	rootHash := initRepoWithFile(t, rootDir, "root.txt", "root\n")
	altAHash := initRepoWithFile(t, altADir, "a.txt", "a\n")
	altBHash := initRepoWithFile(t, altBDir, "b.txt", "b\n")
	_ = initRepoWithFile(t, altIntermediateDir, "intermediate.txt", "intermediate\n")
	altNestedHash := initRepoWithFile(t, altNestedDir, "nested.txt", "nested\n")

	writeAlternates(t, altIntermediateDir, []string{
		filepath.Join(altNestedDir, gitDir, "objects"),
	})
	writeAlternates(t, rootDir, []string{
		filepath.Join(altADir, gitDir, "objects"),
		filepath.Join(altBDir, gitDir, "objects"),
		filepath.Join(altIntermediateDir, gitDir, "objects"),
	})

	repo, err := OpenPath(rootDir)
	require.NoError(t, err)
	defer repo.Close()

	for _, hash := range []string{rootHash, altAHash, altBHash, altNestedHash} {
		commit, err := repo.CommitObject(plumbing.NewHash(hash))
		require.NoError(t, err, "commit %s should be readable", hash)
		_, err = commit.Tree()
		require.NoError(t, err, "tree for commit %s should be readable", hash)
	}
}

func TestHasObjectAlternates(t *testing.T) {
	t.Parallel()

	t.Run("main repository", func(t *testing.T) {
		t.Parallel()

		repoDir := t.TempDir()
		_ = initRepoWithFile(t, repoDir, "root.txt", "root\n")

		hasAlternates, err := hasObjectAlternates(repoDir)
		require.NoError(t, err)
		require.False(t, hasAlternates)

		alternateDir := t.TempDir()
		writeAlternates(t, repoDir, []string{filepath.Join(alternateDir, gitDir, "objects")})

		hasAlternates, err = hasObjectAlternates(repoDir)
		require.NoError(t, err)
		require.True(t, hasAlternates)
	})

	t.Run("linked worktree common dir", func(t *testing.T) {
		t.Parallel()

		rootDir := t.TempDir()
		worktreeDir := filepath.Join(rootDir, "worktree")
		mainGitDir := filepath.Join(rootDir, "main.git")
		worktreeGitDir := filepath.Join(mainGitDir, "worktrees", "worktree")
		alternateDir := filepath.Join(rootDir, "alternate.git", "objects")

		require.NoError(t, os.MkdirAll(worktreeDir, 0o755))
		require.NoError(t, os.MkdirAll(worktreeGitDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(worktreeDir, gitDir), []byte("gitdir: "+worktreeGitDir+"\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(worktreeGitDir, "commondir"), []byte("../..\n"), 0o644))

		infoDir := filepath.Join(mainGitDir, "objects", "info")
		require.NoError(t, os.MkdirAll(infoDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(infoDir, "alternates"), []byte(alternateDir+"\n"), 0o644))

		hasAlternates, err := hasObjectAlternates(worktreeDir)
		require.NoError(t, err)
		require.True(t, hasAlternates)
	})
}

func initRepoWithFile(t *testing.T, repoDir, name, content string) string {
	t.Helper()
	repo, err := git.PlainInit(repoDir, false)
	require.NoError(t, err)
	defer repo.Close()

	cfg, err := repo.Config()
	require.NoError(t, err)
	cfg.User.Name = "Test User"
	cfg.User.Email = "test@example.com"
	if cfg.Raw == nil {
		cfg.Raw = config.New()
	}
	cfg.Raw.Section("commit").SetOption("gpgsign", "false")
	require.NoError(t, repo.SetConfig(cfg))

	require.NoError(t, os.WriteFile(filepath.Join(repoDir, name), []byte(content), 0o644))

	worktree, err := repo.Worktree()
	require.NoError(t, err)
	_, err = worktree.Add(name)
	require.NoError(t, err)
	hash, err := worktree.Commit("add "+name, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	require.NoError(t, err)
	return hash.String()
}

func writeAlternates(t *testing.T, repoDir string, alternates []string) {
	t.Helper()
	infoDir := filepath.Join(repoDir, gitDir, "objects", "info")
	require.NoError(t, os.MkdirAll(infoDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(infoDir, "alternates"), []byte(strings.Join(alternates, "\n")+"\n"), 0o644))
}
