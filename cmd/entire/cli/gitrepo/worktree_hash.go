package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
)

// WorktreeContentHashBudget bounds native Git clean-filter processing on the
// post-commit hook path. A broken or hanging custom filter must not leave the
// hook process blocked indefinitely.
const WorktreeContentHashBudget = 5 * time.Second

// HashWorktreeFiles returns the Git blob hash of each regular working-tree file
// after applying Git's path-specific clean filters. Git hash-object follows
// symlinks, so callers comparing Git symlink blobs must handle those separately.
// Large path sets are split across commands so an oversized argv cannot fail
// before Git inspects anything.
//
// The paths must be repository-relative. They are file operands, not pathspecs,
// so names containing pathspec magic are interpreted literally. The returned
// map can contain successful hashes alongside a non-nil error; callers can
// retain filter-aware results and degrade only the files Git could not hash.
func HashWorktreeFiles(
	ctx context.Context,
	worktreeRoot string,
	paths []string,
) (map[string]plumbing.Hash, error) {
	ctx, cancel := context.WithTimeout(ctx, WorktreeContentHashBudget)
	defer cancel()
	return hashWorktreeFiles(ctx, worktreeRoot, paths, gitHashObjectPathBudget)
}

func hashWorktreeFiles(
	ctx context.Context,
	worktreeRoot string,
	paths []string,
	pathBudget int,
) (map[string]plumbing.Hash, error) {
	hashes := make(map[string]plumbing.Hash, len(paths))
	var errs []error
	for _, batch := range chunkPaths(paths, pathBudget) {
		batchHashes, err := hashWorktreeFileBatch(ctx, worktreeRoot, batch)
		if err == nil {
			for i, path := range batch {
				hashes[path] = batchHashes[i]
			}
			continue
		}

		// A missing or unreadable file fails the whole hash-object invocation.
		// Retry that batch one file at a time so its neighbours keep their
		// filter-aware hashes instead of all dropping to the raw-byte fallback.
		var exitErr *exec.ExitError
		if len(batch) == 1 || ctx.Err() != nil || !errors.As(err, &exitErr) {
			errs = append(errs, err)
			continue
		}
		for _, path := range batch {
			pathHashes, pathErr := hashWorktreeFileBatch(ctx, worktreeRoot, []string{path})
			if pathErr != nil {
				errs = append(errs, pathErr)
				continue
			}
			hashes[path] = pathHashes[0]
		}
	}
	return hashes, errors.Join(errs...)
}

func hashWorktreeFileBatch(ctx context.Context, worktreeRoot string, paths []string) ([]plumbing.Hash, error) {
	args := []string{"-C", worktreeRoot, "hash-object", "--"}
	args = append(args, paths...)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = EnvWithoutRepoOverrides()
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if stderr := strings.TrimSpace(string(exitErr.Stderr)); stderr != "" {
				return nil, fmt.Errorf("git hash-object: %w: %s", err, stderr)
			}
		}
		return nil, fmt.Errorf("git hash-object: %w", err)
	}

	fields := strings.Fields(string(out))
	if len(fields) != len(paths) {
		return nil, fmt.Errorf("git hash-object returned %d hashes for %d paths", len(fields), len(paths))
	}
	hashes := make([]plumbing.Hash, len(fields))
	for i, field := range fields {
		if !plumbing.IsHash(field) {
			return nil, fmt.Errorf("git hash-object returned invalid hash %q", field)
		}
		hashes[i] = plumbing.NewHash(field)
	}
	return hashes, nil
}

// Keep path argv comfortably below Windows' 32 KiB command-line limit,
// including room for fixed arguments and os/exec quoting. Unix limits are much
// larger. A single repository-relative path can exceed the budget and is sent
// alone; filesystem path limits still keep that command bounded.
const gitHashObjectPathBudget = 8 * 1024

func chunkPaths(paths []string, budget int) [][]string {
	var chunks [][]string
	var (
		chunk []string
		size  int
	)
	for _, path := range paths {
		if len(chunk) > 0 && size+len(path)+1 > budget {
			chunks = append(chunks, chunk)
			chunk = nil
			size = 0
		}
		chunk = append(chunk, path)
		size += len(path) + 1
	}
	if len(chunk) > 0 {
		chunks = append(chunks, chunk)
	}
	return chunks
}
