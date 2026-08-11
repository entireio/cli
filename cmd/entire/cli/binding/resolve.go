// Package binding records which repositories an agent session's activity
// touches. Slice 1 (this package's initial state) only OBSERVES: a resolver
// maps out-of-repo evidence paths to their repos and a machine-level session
// record stores the bindings. Nothing acts on the record yet; the additive
// adopt slice consumes it.
//
// Resolver results — including not-a-repo misses — are cached for the
// lifetime of the process: the resolver is designed for short-lived hook
// processes, and a long-lived consumer (e.g. entire mcp) must either accept
// stale results or call ClearResolveCache.
package binding

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/entireio/cli/cmd/entire/cli/execx"
)

// RepoIdentity names a repo by its worktree root and (absolute) git common
// dir. The common dir is the clone-wide key: linked worktrees share it.
type RepoIdentity struct {
	WorktreeRoot string `json:"worktree_root"`
	CommonDir    string `json:"common_dir"`
}

// maxAncestorWalk bounds the walk toward an existing ancestor so garbage
// paths (agent hallucinations, hosts with unbounded virtual trees) cannot
// spin the resolver. 10 levels is generous: evidence paths deleted more than
// ten directories deep are vanishingly rare, and exceeding the bound only
// costs a piece of binding evidence — capture itself is never affected.
const maxAncestorWalk = 10

var (
	resolveMu    sync.RWMutex
	resolveCache = map[string]resolveResult{}
)

type resolveResult struct {
	id RepoIdentity
	ok bool
}

// ResolveRepoForPath maps an absolute file path to the repo containing it.
// The file need not exist (deleted files are evidence too): resolution walks
// up to the nearest existing ancestor directory, bounded, and runs one
// `git -C <dir> rev-parse --show-toplevel --git-common-dir`. Results —
// including "not a repo" — are cached per directory, because one turn can
// carry many paths from the same foreign tree. Relative input returns miss:
// absolutizing against an ambient cwd here would attribute evidence to the
// wrong repo; that responsibility stays with the caller, which knows the
// agent's cwd.
func ResolveRepoForPath(ctx context.Context, absPath string) (RepoIdentity, bool) {
	if !filepath.IsAbs(absPath) {
		return RepoIdentity{}, false
	}
	dir := nearestExistingAncestor(filepath.Dir(absPath))
	if dir == "" {
		return RepoIdentity{}, false
	}

	resolveMu.RLock()
	cached, hit := resolveCache[dir]
	resolveMu.RUnlock()
	if hit {
		return cached.id, cached.ok
	}

	id, ok := resolveViaGit(ctx, dir)
	resolveMu.Lock()
	resolveCache[dir] = resolveResult{id: id, ok: ok}
	resolveMu.Unlock()
	return id, ok
}

func nearestExistingAncestor(dir string) string {
	for range maxAncestorWalk {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}

func resolveViaGit(ctx context.Context, dir string) (RepoIdentity, bool) {
	// --path-format=absolute (git ≥2.31) emits both outputs absolute AND
	// symlink-canonical, so the same clone reached via a symlink and via its
	// real path yields one CommonDir — RepoIdentity's clone-wide-key
	// invariant. No minimum git version is documented for the CLI, so support
	// is probed once per process (a rev-parse failure cannot distinguish
	// flag-unsupported from not-a-repo, and retrying flagless on every miss
	// would double the forks) and older gits take the flagless path.
	return runRevParse(ctx, dir, supportsPathFormat(ctx))
}

var (
	pathFormatMu        sync.Mutex
	pathFormatSupported *bool // nil = not yet probed
)

// supportsPathFormat reports whether the installed git understands
// --path-format (added in 2.31, released 2021). Probed via one `git version`
// fork per process; an unparseable or failing probe conservatively selects
// the flagless fallback, which is correct on every git.
func supportsPathFormat(ctx context.Context) bool {
	pathFormatMu.Lock()
	defer pathFormatMu.Unlock()
	if pathFormatSupported == nil {
		v := probePathFormatSupport(ctx)
		pathFormatSupported = &v
	}
	return *pathFormatSupported
}

func probePathFormatSupport(ctx context.Context) bool {
	out, err := execx.NonInteractive(ctx, "git", "version").Output()
	if err != nil {
		return false
	}
	// "git version 2.50.1 (Apple Git-155)" / "git version 2.31.0.windows.1"
	fields := strings.Fields(string(out))
	if len(fields) < 3 {
		return false
	}
	parts := strings.SplitN(fields[2], ".", 3)
	if len(parts) < 2 {
		return false
	}
	major, majErr := strconv.Atoi(parts[0])
	minor, minErr := strconv.Atoi(parts[1])
	if majErr != nil || minErr != nil {
		return false
	}
	return major > 2 || (major == 2 && minor >= 31)
}

func runRevParse(ctx context.Context, dir string, absoluteFlag bool) (RepoIdentity, bool) {
	args := []string{"-C", dir, "rev-parse"}
	if absoluteFlag {
		args = append(args, "--path-format=absolute")
	}
	args = append(args, "--show-toplevel", "--git-common-dir")
	out, err := execx.NonInteractive(ctx, "git", args...).Output()
	if err != nil {
		return RepoIdentity{}, false // not a repo, git missing, flag unsupported, etc. — a normal miss
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 {
		return RepoIdentity{}, false
	}
	root := strings.TrimSpace(lines[0])
	commonDir := strings.TrimSpace(lines[1])
	if !absoluteFlag {
		// Pre-2.31 fallback: rev-parse emits ../.git style relative output
		// depending on depth — resolve against the queried dir, never the
		// process cwd — and canonicalize via EvalSymlinks, because the joined
		// path preserves symlink components --path-format=absolute would have
		// resolved, which would split one clone into two CommonDir keys.
		if !filepath.IsAbs(commonDir) {
			commonDir = filepath.Join(dir, commonDir)
		}
		root = canonicalize(root)
		commonDir = canonicalize(commonDir)
	}
	commonDir = filepath.Clean(commonDir)
	if root == "" || commonDir == "" {
		return RepoIdentity{}, false
	}
	return RepoIdentity{WorktreeRoot: root, CommonDir: commonDir}, true
}

// canonicalize resolves symlinks best-effort: on error (e.g. a component
// disappeared mid-resolution) the cleaned input is still a usable identity.
func canonicalize(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return resolved
}

// ClearResolveCache resets the resolver cache and the cached
// --path-format support probe. Test-only.
func ClearResolveCache() {
	resolveMu.Lock()
	resolveCache = map[string]resolveResult{}
	resolveMu.Unlock()
	pathFormatMu.Lock()
	pathFormatSupported = nil
	pathFormatMu.Unlock()
}

// ResolveCacheSizeForTesting reports the number of cached directory
// resolutions. Test-only.
func ResolveCacheSizeForTesting() int {
	resolveMu.RLock()
	defer resolveMu.RUnlock()
	return len(resolveCache)
}
