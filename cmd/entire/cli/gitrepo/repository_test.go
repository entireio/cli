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

// TestOpenPath_RelativeAlternate covers a shared clone whose
// objects/info/alternates entry is a relative path (e.g. created with
// `git clone --reference`). go-git cannot follow relative alternates on its own;
// OpenPath must rewrite them to absolute so objects in the alternate resolve.
func TestOpenPath_RelativeAlternate(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	altDir := t.TempDir()

	rootHash := initRepoWithFile(t, rootDir, "root.txt", "root\n")
	altHash := initRepoWithFile(t, altDir, "alt.txt", "alt\n")

	// Write the alternate as a path relative to rootDir/.git/objects, matching
	// what git records for a relative/shared clone.
	rootObjects := filepath.Join(rootDir, gitDir, "objects")
	altObjects := filepath.Join(altDir, gitDir, "objects")
	relAlt, err := filepath.Rel(rootObjects, altObjects)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(relAlt, ".."), "expected a relative alternate, got %q", relAlt)
	writeAlternates(t, rootDir, []string{relAlt})

	repo, err := OpenPath(rootDir)
	require.NoError(t, err)
	defer repo.Close()

	for _, hash := range []string{rootHash, altHash} {
		_, err := repo.CommitObject(plumbing.NewHash(hash))
		require.NoError(t, err, "commit %s should be readable via relative alternate", hash)
	}
}

// TestOpenPath_MultipleRelativeAlternates covers a repository whose
// alternates file lists several relative entries. Each entry must be
// rewritten in place and the file's overall structure preserved so go-git
// can enumerate every alternate object directory.
func TestOpenPath_MultipleRelativeAlternates(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	altADir := t.TempDir()
	altBDir := t.TempDir()
	altCDir := t.TempDir()

	rootHash := initRepoWithFile(t, rootDir, "root.txt", "root\n")
	altAHash := initRepoWithFile(t, altADir, "a.txt", "a\n")
	altBHash := initRepoWithFile(t, altBDir, "b.txt", "b\n")
	altCHash := initRepoWithFile(t, altCDir, "c.txt", "c\n")

	rootObjects := filepath.Join(rootDir, gitDir, "objects")
	relA, err := filepath.Rel(rootObjects, filepath.Join(altADir, gitDir, "objects"))
	require.NoError(t, err)
	relB, err := filepath.Rel(rootObjects, filepath.Join(altBDir, gitDir, "objects"))
	require.NoError(t, err)
	relC, err := filepath.Rel(rootObjects, filepath.Join(altCDir, gitDir, "objects"))
	require.NoError(t, err)
	for _, rel := range []string{relA, relB, relC} {
		require.True(t, strings.HasPrefix(rel, ".."), "expected relative alternate, got %q", rel)
	}
	writeAlternates(t, rootDir, []string{relA, relB, relC})

	repo, err := OpenPath(rootDir)
	require.NoError(t, err)
	defer repo.Close()

	for _, hash := range []string{rootHash, altAHash, altBHash, altCHash} {
		commit, err := repo.CommitObject(plumbing.NewHash(hash))
		require.NoError(t, err, "commit %s should be readable through one of the relative alternates", hash)
		_, err = commit.Tree()
		require.NoError(t, err, "tree for commit %s should be readable", hash)
	}
}

// TestOpenPath_NestedRelativeAlternates covers an alternate chain where every
// link is relative: root → alt1 → alt2. The rewrite on the root's alternates
// file lets go-git enter alt1, but alt1's own alternates file is read via
// go-git's AlternatesFS (not our wrapper) so the alt1 → alt2 hop must also
// resolve correctly for objects in alt2 to be reachable.
func TestOpenPath_NestedRelativeAlternates(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	alt1Dir := t.TempDir()
	alt2Dir := t.TempDir()

	rootHash := initRepoWithFile(t, rootDir, "root.txt", "root\n")
	alt1Hash := initRepoWithFile(t, alt1Dir, "alt1.txt", "alt1\n")
	alt2Hash := initRepoWithFile(t, alt2Dir, "alt2.txt", "alt2\n")

	rootObjects := filepath.Join(rootDir, gitDir, "objects")
	alt1Objects := filepath.Join(alt1Dir, gitDir, "objects")
	alt2Objects := filepath.Join(alt2Dir, gitDir, "objects")

	relRootToAlt1, err := filepath.Rel(rootObjects, alt1Objects)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(relRootToAlt1, ".."), "expected a relative alternate, got %q", relRootToAlt1)
	writeAlternates(t, rootDir, []string{relRootToAlt1})

	relAlt1ToAlt2, err := filepath.Rel(alt1Objects, alt2Objects)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(relAlt1ToAlt2, ".."), "expected a relative nested alternate, got %q", relAlt1ToAlt2)
	writeAlternates(t, alt1Dir, []string{relAlt1ToAlt2})

	repo, err := OpenPath(rootDir)
	require.NoError(t, err)
	defer repo.Close()

	for _, hash := range []string{rootHash, alt1Hash, alt2Hash} {
		commit, err := repo.CommitObject(plumbing.NewHash(hash))
		require.NoError(t, err, "commit %s should be readable through nested relative alternates", hash)
		_, err = commit.Tree()
		require.NoError(t, err, "tree for commit %s should be readable", hash)
	}
}

// TestOpenPath_LinkedWorktreeRelativeAlternate covers a linked worktree
// whose common git directory holds a relative alternates entry. The alternates
// file lives under the common git dir (not the worktree's own gitdir), so the
// rewrite must apply to the common-dir filesystem and resolve relative entries
// against <common-dir>/objects rather than the worktree gitdir.
func TestOpenPath_LinkedWorktreeRelativeAlternate(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	mainDir := filepath.Join(rootDir, "main")
	altDir := filepath.Join(rootDir, "alt")
	worktreeDir := filepath.Join(rootDir, "wt")
	require.NoError(t, os.MkdirAll(mainDir, 0o755))
	require.NoError(t, os.MkdirAll(altDir, 0o755))
	require.NoError(t, os.MkdirAll(worktreeDir, 0o755))

	mainHash := initRepoWithFile(t, mainDir, "main.txt", "main\n")
	altHash := initRepoWithFile(t, altDir, "alt.txt", "alt\n")

	mainGitDir := filepath.Join(mainDir, gitDir)
	mainObjects := filepath.Join(mainGitDir, "objects")
	altObjects := filepath.Join(altDir, gitDir, "objects")

	relAlt, err := filepath.Rel(mainObjects, altObjects)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(relAlt, ".."), "expected a relative alternate, got %q", relAlt)
	writeAlternates(t, mainDir, []string{relAlt})

	worktreeGitDir := filepath.Join(mainGitDir, "worktrees", "wt")
	require.NoError(t, os.MkdirAll(worktreeGitDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(worktreeDir, gitDir), []byte("gitdir: "+worktreeGitDir+"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(worktreeGitDir, "commondir"), []byte("../..\n"), 0o644))

	mainHEAD, err := os.ReadFile(filepath.Join(mainGitDir, "HEAD"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(worktreeGitDir, "HEAD"), mainHEAD, 0o644))

	repo, err := OpenPath(worktreeDir)
	require.NoError(t, err)
	defer repo.Close()

	for _, hash := range []string{mainHash, altHash} {
		commit, err := repo.CommitObject(plumbing.NewHash(hash))
		require.NoError(t, err, "commit %s should be readable through linked worktree relative alternate", hash)
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
