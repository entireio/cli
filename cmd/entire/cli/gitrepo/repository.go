package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	repo, err := openPathWithAlternates(repoRoot)
	if err != nil {
		if hasAlternates, altErr := hasObjectAlternates(repoRoot); altErr == nil && hasAlternates {
			return nil, fmt.Errorf("failed to open repository with alternates support: %w", err)
		}

		// Intentional PlainOpen fallback for unusual layouts that do not use
		// alternates. Repositories with alternates must not silently downgrade
		// because PlainOpen cannot read absolute alternate object directories.
		if fallbackRepo, fallbackErr := git.PlainOpen(repoRoot); fallbackErr == nil {
			return fallbackRepo, nil
		}
		return nil, fmt.Errorf("failed to open repository: %w", err)
	}
	return repo, nil
}

// ResolveDotGitPath resolves the .git entry for a worktree without opening the
// repository. Callers that need Git metadata should use this shared resolver.
func ResolveDotGitPath(repoRoot string) (string, error) {
	return resolveDotGitPath(repoRoot)
}

// ResolveCommonGitPath resolves the shared Git directory for a resolved .git
// directory. An empty result means the repository has no commondir file.
func ResolveCommonGitPath(dotGitPath string) (string, error) {
	return resolveCommonGitPath(dotGitPath)
}

func hasObjectAlternates(repoRoot string) (bool, error) {
	repoRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return false, fmt.Errorf("resolve repository root: %w", err)
	}

	dotGitPath, err := resolveDotGitPath(repoRoot)
	if err != nil {
		return false, fmt.Errorf("resolve .git path: %w", err)
	}

	commonGitPath, err := resolveCommonGitPath(dotGitPath)
	if err != nil {
		return false, fmt.Errorf("resolve common git path: %w", err)
	}

	candidates := []string{filepath.Join(dotGitPath, "objects", "info", "alternates")}
	if commonGitPath != "" {
		candidates = append(candidates, filepath.Join(commonGitPath, "objects", "info", "alternates"))
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

func openPathWithAlternates(repoRoot string) (*git.Repository, error) {
	repoRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve repository root: %w", err)
	}

	dotGitPath, err := resolveDotGitPath(repoRoot)
	if err != nil {
		return nil, err
	}

	commonGitPath, err := resolveCommonGitPath(dotGitPath)
	if err != nil {
		return nil, err
	}

	// Wrap the git-dir filesystems so relative alternate object directories are
	// rewritten to absolute paths on read; go-git cannot follow relative
	// alternates on its own. The alternates file lives under the common git dir
	// for linked worktrees, so wrap both.
	// wrapConfigSanitize sits outside wrapAlternatesRewrite: it drops go-git-
	// incompatible negative fetch refspecs from config reads (#778); the
	// alternates wrapper rewrites objects/info/alternates. They intercept
	// different files and compose cleanly.
	dotGitFS := wrapConfigSanitize(wrapAlternatesRewrite(osfs.New(dotGitPath, osfs.WithBoundOS())))
	var commonGitFS billy.Filesystem
	if commonGitPath != "" {
		commonGitFS = wrapConfigSanitize(wrapAlternatesRewrite(osfs.New(commonGitPath, osfs.WithBoundOS())))
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
	if repoUsesReftable(dotGitPath, commonGitPath) {
		repo, err := git.Open(newReftableStorer(storage, dotGitPath), worktreeFS)
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

func resolveDotGitPath(repoRoot string) (string, error) {
	gitPath := filepath.Join(repoRoot, gitDir)
	info, err := os.Stat(gitPath)
	if err != nil {
		return "", fmt.Errorf("stat .git path: %w", err)
	}
	if info.IsDir() {
		return gitPath, nil
	}

	content, err := os.ReadFile(gitPath) //nolint:gosec // gitPath is resolved from the git worktree root.
	if err != nil {
		return "", fmt.Errorf("read .git file: %w", err)
	}

	line, _, _ := strings.Cut(string(content), "\n")
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
	if !ok {
		return "", errors.New(".git file has no gitdir prefix")
	}

	gitdir = strings.TrimSpace(gitdir)
	if filepath.IsAbs(gitdir) {
		return filepath.Clean(gitdir), nil
	}
	return filepath.Clean(filepath.Join(repoRoot, gitdir)), nil
}

func resolveCommonGitPath(dotGitPath string) (string, error) {
	content, err := os.ReadFile(filepath.Join(dotGitPath, "commondir")) //nolint:gosec // dotGitPath is resolved from the git worktree root.
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read commondir file: %w", err)
	}

	commonPath := strings.TrimSpace(string(content))
	if commonPath == "" {
		return "", nil
	}
	if filepath.IsAbs(commonPath) {
		return filepath.Clean(commonPath), nil
	}
	return filepath.Clean(filepath.Join(dotGitPath, commonPath)), nil
}
