package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sync/errgroup"
)

const (
	// scanDefaultDepth is the default value of `entire scan --depth`. Depth 1
	// inspects the scan root's direct children, depth 2 also their children —
	// enough to cover both a flat ~/dev and a ~/dev/<org>/<repo> layout.
	scanDefaultDepth = 2

	// scanConcurrencyLimit bounds the git subprocesses and per-repo inspections
	// in flight. Same bound as the dispatch wizard's repo discovery.
	scanConcurrencyLimit = 8

	// scanNodeModulesDir is skipped while descending: it is large, deep, and
	// any repo inside it is a dependency rather than something to enable.
	scanNodeModulesDir = "node_modules"
)

// scanCandidate is a git working tree discovered by findGitRepos.
type scanCandidate struct {
	// Root is the working tree's toplevel as git reports it, absolute.
	Root string
	// LinkedWorktree reports whether Root is a linked worktree (`git worktree
	// add`) rather than a repository's main working tree.
	LinkedWorktree bool
}

// findGitRepos walks roots breadth-first and returns every git working tree it
// finds, sorted by path and deduplicated by resolved toplevel.
//
// A directory containing a `.git` entry — directory or gitfile, so linked
// worktrees count — is a candidate and is NOT descended into. That single rule
// keeps nested repositories (vendored copies, `.worktrees/`, submodules) out of
// the results and bounds the walk inside large monorepos.
//
// maxDepth caps how far below each root the walk goes: the root is depth 0, its
// children depth 1. Directories at maxDepth are still examined as candidates but
// are never expanded.
//
// Unreadable directories are skipped silently — a scan over ~/dev must not fail
// because one folder is permission-denied — so the error return is reserved for
// context cancellation.
func findGitRepos(ctx context.Context, roots []string, maxDepth int) ([]scanCandidate, error) {
	dirs := collectScanDirs(ctx, roots, maxDepth)
	return resolveScanCandidates(ctx, dirs)
}

type scanQueueItem struct {
	dir   string
	depth int
}

// collectScanDirs performs the breadth-first walk and returns the directories
// that look like working trees, in discovery order.
func collectScanDirs(ctx context.Context, roots []string, maxDepth int) []string {
	queue := make([]scanQueueItem, 0, len(roots))
	visited := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		abs, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		abs = filepath.Clean(abs)
		if _, seen := visited[abs]; seen {
			continue
		}
		visited[abs] = struct{}{}
		queue = append(queue, scanQueueItem{dir: abs, depth: 0})
	}

	var found []string
	for len(queue) > 0 {
		if ctx.Err() != nil {
			return found
		}
		item := queue[0]
		queue = queue[1:]

		if hasGitEntry(item.dir) {
			found = append(found, item.dir)
			continue
		}
		if item.depth >= maxDepth {
			continue
		}
		for _, child := range readScanChildren(item.dir) {
			if _, seen := visited[child]; seen {
				continue
			}
			visited[child] = struct{}{}
			queue = append(queue, scanQueueItem{dir: child, depth: item.depth + 1})
		}
	}
	return found
}

// readScanChildren returns the sub-directories of dir worth descending into.
// Unreadable directories yield nothing. Symlinked directories are excluded:
// os.ReadDir reports link entries as symlinks (not directories), so following
// them would risk cycles and duplicate results outside the scanned tree.
func readScanChildren(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	children := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") || name == scanNodeModulesDir {
			continue
		}
		children = append(children, filepath.Join(dir, name))
	}
	return children
}

// hasGitEntry reports whether dir holds a `.git` directory or gitfile.
func hasGitEntry(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// resolveScanCandidates asks git to confirm each candidate directory, dropping
// bare repositories and anything git refuses to resolve.
func resolveScanCandidates(ctx context.Context, dirs []string) ([]scanCandidate, error) {
	resolved := make([]*scanCandidate, len(dirs))

	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(scanConcurrencyLimit)
	for i, dir := range dirs {
		group.Go(func() error {
			if err := groupCtx.Err(); err != nil {
				return err //nolint:wrapcheck // context error propagated verbatim to abort the scan
			}
			resolved[i] = resolveScanCandidate(groupCtx, dir)
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, fmt.Errorf("resolving git repositories: %w", err)
	}

	candidates := make([]scanCandidate, 0, len(resolved))
	seen := make(map[string]struct{}, len(resolved))
	for _, candidate := range resolved {
		if candidate == nil {
			continue
		}
		if _, dup := seen[candidate.Root]; dup {
			continue
		}
		seen[candidate.Root] = struct{}{}
		candidates = append(candidates, *candidate)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Root < candidates[j].Root })
	return candidates, nil
}

// resolveScanCandidate resolves one candidate directory with a single git
// invocation. Returns nil when the directory is not a usable working tree.
//
// The flag order matters: `--is-bare-repository` comes first because
// `--show-toplevel` fails outright inside a bare repository, and git stops
// emitting output at the first failing flag. Asking for bareness up front means
// a bare repo still tells us so before the command exits non-zero.
func resolveScanCandidate(ctx context.Context, dir string) *scanCandidate {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse",
		"--is-bare-repository", "--git-common-dir", "--show-toplevel")
	output, err := cmd.Output()
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "true" {
		return nil // bare repository: no working tree to enable Entire in
	}
	if err != nil || len(lines) < 3 {
		return nil
	}

	commonDir := strings.TrimSpace(lines[1])
	toplevel := strings.TrimSpace(lines[2])
	if toplevel == "" {
		return nil
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(dir, commonDir)
	}

	return &scanCandidate{
		Root:           filepath.Clean(toplevel),
		LinkedWorktree: !samePathOnDisk(commonDir, filepath.Join(toplevel, ".git")),
	}
}

// samePathOnDisk compares two paths after resolving symlinks, so a candidate
// reached through /tmp is not mistaken for a different location than the
// /private/tmp path git reports on macOS.
func samePathOnDisk(a, b string) bool {
	return resolveSymlinksBestEffort(a) == resolveSymlinksBestEffort(b)
}

func resolveSymlinksBestEffort(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}
