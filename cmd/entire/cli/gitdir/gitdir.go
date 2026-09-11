// Package gitdir owns access to the git common directory — .git in an ordinary
// checkout, and the main repository's .git when running from a linked worktree.
//
// Entire keeps a lot of state there, deliberately: session state, advisory
// locks, the checkpoint push queue, the redaction prefix cache, investigation
// runs, review manifests, and the captured checkpoint-sync election all live in
// the common dir rather than under .entire, because they are per-clone and must
// not be committed or walked into a checkpoint tree. That makes this directory
// the same kind of trust surface .entire is, and it gets the same treatment: one
// *os.Root per common dir, opened once and shared, with every read and write
// resolved as a name inside it.
//
// Two properties come from that, and neither survives a call site reverting to
// os.ReadFile on a joined path:
//
//   - Containment. Names here are built from agent-supplied session IDs and
//     investigation run IDs. Several call sites carry hand-written comments
//     explaining that an unvalidated ID would be a path-traversal sink feeding
//     os.RemoveAll. A root makes that structural instead of a precondition each
//     caller has to keep honouring.
//   - One directory handle per clone. The resolver shells out to git; before
//     this package, strategy.GetGitCommonDir did so on *every* call with no
//     cache at all, on hook paths.
//
// Unlike .entire there is no create/no-create split: the common dir is the
// repository, so it always exists by the time anything here is called. What
// needs creating are Entire's own subdirectories inside it, via
// osroot.MkdirAllNoSymlink.
package gitdir

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

var (
	// resolveMu guards the resolved-path cache, which is keyed by the working
	// directory the answer was derived in (see CommonDir).
	resolveMu     sync.RWMutex
	cachedDir     string
	cachedFromCwd string
)

// CommonDir returns the absolute path of the git common directory, caching the
// result per working directory.
//
// Absolute is the load-bearing word. `git rev-parse --git-common-dir` answers
// relative to the process's directory — from a subdirectory it returns
// "../../../.git" — and the two implementations this replaced passed that
// straight through, so every path built on it was only valid while the process
// stayed put. Resolving it once here means a stored lock path or queue path
// still names the same file later, and it is what lets Open hold a handle
// rather than re-resolving a string.
func CommonDir(ctx context.Context) (string, error) {
	cwd, err := os.Getwd() //nolint:forbidigo // cache key, and the base the git answer is relative to
	if err != nil {
		cwd = ""
	}

	resolveMu.RLock()
	if cachedDir != "" && cachedFromCwd == cwd {
		cached := cachedDir
		resolveMu.RUnlock()
		return cached, nil
	}
	resolveMu.RUnlock()

	// The environment is deliberately inherited rather than scrubbed with
	// gitrepo.EnvWithoutRepoOverrides. The rule that exists for cmd.Dir/-C
	// subprocesses is about not silently operating on the hook's repo when the
	// caller meant a different one; here the hook's repo IS the target, so a
	// GIT_DIR git exported to us is the right answer, not contamination.
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-common-dir")
	cmd.Dir = "."
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get git common dir: %w", err)
	}

	dir, err := filepath.Abs(strings.TrimSpace(string(output)))
	if err != nil {
		return "", fmt.Errorf("resolve git common dir: %w", err)
	}

	resolveMu.Lock()
	cachedDir = dir
	cachedFromCwd = cwd
	resolveMu.Unlock()

	return dir, nil
}

// ClearCache forgets the resolved path. Tests that change directory call it;
// production does not need to, because the cache is keyed by cwd.
func ClearCache() {
	resolveMu.Lock()
	cachedDir = ""
	cachedFromCwd = ""
	resolveMu.Unlock()
}

// Open returns the shared *os.Root over the git common directory. The returned
// root is owned by this package and shared with every other caller; do not
// close it.
func Open(ctx context.Context) (*os.Root, error) {
	dir, err := CommonDir(ctx)
	if err != nil {
		return nil, err
	}
	return OpenAt(dir)
}

// OpenAt is Open for an explicit git directory — the common dir for callers that
// already resolved one or that act on another clone, and the per-worktree git
// dir for the few things that genuinely live there rather than in the common dir
// (the rebase/cherry-pick sequence markers).
func OpenAt(commonDir string) (*os.Root, error) {
	if commonDir == "" {
		return nil, errors.New("gitdir: common dir is required")
	}
	abs, err := filepath.Abs(commonDir)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", commonDir, err)
	}

	// osroot.Shared owns the open-at-most-once registry, shared with the other
	// anchors, and returns a missing directory unwrapped so callers can classify
	// it with os.IsNotExist — an ordinary outcome for a store asked about a repo
	// that has none.
	return osroot.Shared(abs) //nolint:wrapcheck // see comment
}

// Reset closes and forgets every cached root, and the resolved path with them.
// Call it after deleting or replacing a common dir: a root that outlives its
// directory is a handle to an unlinked inode, so writes through it succeed and
// land nowhere. The root registry is shared with the other anchors, so this
// clears those too.
func Reset() {
	osroot.ResetShared()
	ClearCache()
}

// CommonDirForWorktree returns the absolute git common directory for the
// repository at worktreeRoot, independent of the process's working directory.
//
// Callers acting on a repo passed as an argument (agent import, session adopt)
// must use this rather than CommonDir: the cwd-resolved form answers for
// whatever repo the process happens to be running in, which is how test
// fixtures once leaked session state into a developer's real
// .git/entire-sessions and hijacked commit linking.
//
// Not cached: the per-cwd cache CommonDir keeps would be wrong here, since the
// answer varies with the argument rather than with the process.
func CommonDirForWorktree(ctx context.Context, worktreeRoot string) (string, error) {
	// An empty root would silently degrade to the process cwd (cmd.Dir = ""),
	// reproducing exactly the accidental-repo leak this exists to prevent.
	if worktreeRoot == "" {
		return "", errors.New("gitdir: worktree root required to resolve a git common dir")
	}
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-common-dir")
	cmd.Dir = worktreeRoot
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolve git common dir for %s: %w", worktreeRoot, err)
	}
	// git answers relative to cmd.Dir, so this resolves against worktreeRoot and
	// never against the process's own directory.
	dir := strings.TrimSpace(string(output))
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(worktreeRoot, dir)
	}
	return filepath.Clean(dir), nil
}

// OpenPathIn returns the shared root for commonDir together with absPath's name
// inside it, so a consumer holding an absolute path it was handed earlier can
// still read through the one root rather than through the path.
//
// It refuses a path that is not under commonDir instead of silently reading it.
// That refusal is only worth anything when commonDir is resolved INDEPENDENTLY
// of absPath: a caller that derives the base by walking up from the target makes
// the check vacuous, because the relative path is then correct by construction.
// settings.clonePreferencesRoot is the one caller, and it recovers the common
// dir by removing a compile-time constant suffix, which fails loudly on a path
// of the wrong shape rather than anchoring somewhere else.
//
// (The investigation stores used to be described here as callers. They are not:
// they hold a commonDir of their own and resolve run ids as names inside it,
// which is the stronger form — see investigate.StateStore.)
func OpenPathIn(commonDir, absPath string) (root *os.Root, name string, err error) {
	base, err := filepath.Abs(commonDir)
	if err != nil {
		return nil, "", fmt.Errorf("resolve %s: %w", commonDir, err)
	}
	target, err := filepath.Abs(absPath)
	if err != nil {
		return nil, "", fmt.Errorf("resolve %s: %w", absPath, err)
	}
	rel, err := filepath.Rel(base, target)
	if err != nil || rel == "." || paths.IsRelativeTraversal(rel) {
		return nil, "", fmt.Errorf("%q is not under %q", absPath, commonDir)
	}
	root, err = OpenAt(base)
	if err != nil {
		return nil, "", err
	}
	return root, filepath.ToSlash(rel), nil
}
