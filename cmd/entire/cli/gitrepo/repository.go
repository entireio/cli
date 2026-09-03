package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-billy/v6"
	"github.com/go-git/go-billy/v6/osfs"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/cache"
	gitfilesystem "github.com/go-git/go-git/v6/storage/filesystem"
	"github.com/go-git/go-git/v6/storage/filesystem/dotgit"
)

const gitDir = ".git"

// sharedObjectCache is one process-wide object cache. go-git uses the cache
// passed to the storage as its delta-base cache, so a per-open cache makes
// every open re-read pack data from scratch.
var sharedObjectCache = cache.NewObjectLRUDefault()

// OpenCurrent opens the current git worktree with object alternates enabled.
// The caller owns the returned repository and must close it.
//
// A worktree root that will not resolve is an error, not a reason to open the
// current directory. This used to fall back to ".", which is a different
// repository whenever the two disagree, and go-git disagrees with git in exactly
// the cases that made the resolution fail:
//
//   - git exports GIT_DIR and GIT_WORK_TREE to the hooks it runs, and
//     paths.WorktreeRoot honours them. OpenPath(".") cannot see them, so a hook
//     running for repo A whose git lookup failed opened repo B, the unrelated
//     repository it happened to be sitting in. Verified.
//   - git refuses a repository whose ownership fails its safe.directory check,
//     and refuses a .git it cannot parse. go-git applies neither check, so the
//     fallback opened repositories the user's own git declines to touch.
//
// Five of the six callers write through the repository they are handed, and all
// five sit behind the root pre-run, which already refuses to run when the
// worktree root does not resolve — so this closes a trap rather than a live bug.
// The sixth is a best-effort advisory that genuinely wants the old behaviour and
// now asks for it by name: see OpenCurrentOrCwd.
func OpenCurrent(ctx context.Context) (*git.Repository, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve worktree root: %w", err)
	}
	return OpenPath(repoRoot)
}

// OpenCurrentOrCwd is OpenCurrent with the current directory as a fallback when
// the worktree root will not resolve.
//
// It exists for one caller, WarnCheckpointPolicyIfNeeded, and the properties
// that make it acceptable there do not generalise: it is dispatched from
// main.go after cobra has finished, so unlike every other repository open it has
// no pre-run guard in front of it; it only READS a policy ref to decide whether
// to print an advisory line; and it discards every error it gets. Opening the
// wrong repository there costs a warning that is skipped or shown, not a write
// that lands in the wrong place.
//
// Do not reach for this because OpenCurrent started returning an error. That
// error means Entire could not establish which repository this is, and for
// anything that writes, stopping is the whole point.
func OpenCurrentOrCwd(ctx context.Context) (*git.Repository, error) {
	if repo, err := OpenCurrent(ctx); err == nil {
		return repo, nil
	}
	return OpenPath(".")
}

// OpenPath opens a git repository with object alternates enabled.
// The caller owns the returned repository and must close it.
func OpenPath(repoRoot string) (*git.Repository, error) {
	repoRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve repository root: %w", err)
	}
	repoRoot = filepath.Clean(repoRoot)

	metadata, err := ResolveWorktreeMetadata(repoRoot)
	if err != nil {
		if errors.Is(err, ErrWorktreeMetadataNotFound) {
			repo, bareErr := openBareRepository(repoRoot)
			if bareErr == nil {
				return repo, nil
			}
			return nil, fmt.Errorf("failed to open repository at %s: worktree metadata: %w; bare repository validation: %w", repoRoot, err, bareErr)
		}
		return nil, fmt.Errorf("resolve worktree metadata: %w", err)
	}

	repo, err := openWorktreeRepository(repoRoot, metadata)
	if err == nil {
		return repo, nil
	}

	hasAlternates, alternatesErr := hasObjectAlternates(metadata)
	if alternatesErr != nil {
		return nil, fmt.Errorf("inspect repository after specialized open failed: %w", alternatesErr)
	}
	usesReftable, reftableErr := inspectRepoUsesReftable(metadata.GitDir, metadata.CommonDir)
	if reftableErr != nil {
		return nil, fmt.Errorf("inspect repository after specialized open failed: %w", reftableErr)
	}
	if hasAlternates || usesReftable {
		return nil, fmt.Errorf("failed to open repository with specialized storage: %w", err)
	}

	// A plain fallback is safe only after complete worktree metadata validation
	// and positive evidence that neither specialized storage feature is present.
	if fallbackRepo, fallbackErr := git.PlainOpen(repoRoot); fallbackErr == nil {
		return fallbackRepo, nil
	}
	return nil, fmt.Errorf("failed to open repository: %w", err)
}

func openBareRepository(repoRoot string) (*git.Repository, error) {
	repo, err := git.PlainOpen(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("failed to open repository: %w", err)
	}
	config, err := repo.Config()
	if err != nil {
		_ = repo.Close()
		return nil, fmt.Errorf("validate bare repository config: %w", err)
	}
	if !config.Core.IsBare {
		_ = repo.Close()
		return nil, fmt.Errorf("path %s has no worktree metadata and is not a bare repository", repoRoot)
	}
	return repo, nil
}

func hasObjectAlternates(metadata WorktreeMetadata) (bool, error) {
	candidates := []string{filepath.Join(metadata.GitDir, "objects", "info", "alternates")}
	if metadata.CommonDir != metadata.GitDir {
		candidates = append(candidates, filepath.Join(metadata.CommonDir, "objects", "info", "alternates"))
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("stat alternates file: %w", err)
		}
	}
	return false, nil
}

func openWorktreeRepository(repoRoot string, metadata WorktreeMetadata) (*git.Repository, error) {
	// Wrap the git-dir filesystems so relative alternate object directories are
	// rewritten to absolute paths on read; go-git cannot follow relative
	// alternates on its own. The alternates file lives under the common git dir
	// for linked worktrees, so wrap both.
	dotGitFS := wrapAlternatesRewrite(osfs.New(metadata.GitDir, osfs.WithBoundOS()))
	var commonGitFS billy.Filesystem
	if metadata.CommonDir != metadata.GitDir {
		commonGitFS = wrapAlternatesRewrite(osfs.New(metadata.CommonDir, osfs.WithBoundOS()))
	}

	repositoryFS := dotgit.NewRepositoryFilesystem(dotGitFS, commonGitFS)
	// Shared clones write absolute object directories to objects/info/alternates;
	// an OS-rooted filesystem lets go-git follow those paths outside .git.
	storage := gitfilesystem.NewStorageWithOptions(
		repositoryFS,
		sharedObjectCache,
		gitfilesystem.Options{
			AlternatesFS: newAlternatesFilesystem(),
		},
	)

	// go-git's filesystem storer cannot read the reftable ref backend: it reads
	// refs from .git/refs, packed-refs and .git/HEAD, none of which are
	// authoritative in a reftable repository, and its extension check rejects
	// extensions.refstorage=reftable outright. Route ref operations through the
	// git CLI for such repositories while keeping object storage on go-git.
	//
	// TODO: drop the reftable branch below and the whole reftableStorer
	// (reftable.go) once go-git ships a native reftable reader/writer. At that
	// point a plain git.Open(storage, worktreeFS) will handle reftable
	// repositories directly and this CLI-backed shim is dead weight.
	worktreeFS := osfs.New(repoRoot, osfs.WithBoundOS())
	usesReftable, err := inspectRepoUsesReftable(metadata.GitDir, metadata.CommonDir)
	if err != nil {
		_ = storage.Close()
		return nil, err
	}
	if usesReftable {
		repo, err := git.Open(newReftableStorer(storage, metadata.GitDir), worktreeFS)
		if err != nil {
			_ = storage.Close()
			return nil, fmt.Errorf("open reftable repository storage: %w", err)
		}
		return repo, nil
	}

	repo, err := git.Open(storage, worktreeFS)
	if err != nil {
		_ = storage.Close()
		return nil, fmt.Errorf("open repository storage: %w", err)
	}
	return repo, nil
}
