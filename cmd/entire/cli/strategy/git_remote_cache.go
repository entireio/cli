package strategy

import (
	"context"
	"sync"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// The checkpoint sync remote is elected from scratch on every call — a
// documented tradeoff (see CheckpointReadRemotes), and every election shells out
// to git to answer two questions about .git/config: which remotes exist, and
// does remote X exist. Those answers are identical for every election within one
// command against one repository, and there are several: `entire checkpoint list`
// resolves the chain four times (metadata-disconnection warning, the branch
// listing itself, the git-refs store's remote discovery, and the
// imported-rewind-point pass), costing 9 git subprocesses where the same command
// on the pre-election code spent 3.
//
// This caches only those two .git/config reads — never the election result.
// Settings and the captured-election file (#1991) stay uncached, so a write
// followed by a re-resolve still observes the new value; only "which git remotes
// exist" is memoized.
//
// Context-scoped and opt-in rather than a package global: an uninstrumented
// context behaves exactly as before, so tests keep the uncached path and cannot
// leak one temp repo's remote list into another. main() installs it on the root
// context; `entire mcp` narrows that to one window per request (see
// WithGitRemoteCache).

type gitRemoteCacheKey struct{}

// gitRemoteCache memoizes .git/config remote reads, partitioned by repository.
//
// The partition is load-bearing, not tidiness: `entire dispatch --repos a,b`
// walks several repositories in ONE process, scoping each one's election with
// settings.WithWorktreeRoot so the git calls run in that repo (see
// dispatch/mode_local.go, and c04a2e312 "honor read candidates per repository",
// which fixed exactly this). A cache keyed only by remote name would answer
// repo B's election from repo A's remote list and silently re-break that fix.
//
// Locking is two-level, and deliberately so. gitRemoteCache.mu guards only the
// byDir map — never a git subprocess. Each repository's answers sit behind their
// own mutex, so `entire dispatch --repos a,b`, which fans out over repositories
// with an errgroup (dispatch/mode_local.go), keeps running their git reads in
// parallel. Holding one global lock across the exec would have made this cache
// serialize cross-repository work that ran concurrently before it existed.
//
// Within one repository callers still serialize, which is the point for two
// racing reads of the same key — the second waits and takes the memoized answer
// instead of shelling out again. Two reads of different keys in one repository
// serialize too; the key set is tiny (the remote list, plus membership for the
// elected remote and origin), so that is a bounded cost for a much simpler
// invariant than per-key locking.
type gitRemoteCache struct {
	mu sync.Mutex
	// byDir maps the repository root to its memoized answers.
	byDir map[string]*remoteSnapshot
}

// remoteSnapshot holds one repository's memoized .git/config answers, behind its
// own mutex so a git read for one repository never blocks another's.
type remoteSnapshot struct {
	mu sync.Mutex
	// ordered is configuredRemotesInConfigOrder's result; orderedSet
	// distinguishes "not yet read" from a legitimately empty remote list.
	ordered    []string
	orderedSet bool
	// member holds isConfiguredRemote answers per remote name. Kept separate
	// from ordered because the two ask git different questions: ordered lists
	// only remotes carrying a url key in local config, while isConfiguredRemote
	// runs `git remote get-url`, which also sees pushurl-only remotes and
	// inherited scopes. Deriving one from the other would change election
	// semantics, so each is memoized against its own git call.
	member map[string]bool
}

// WithGitRemoteCache returns a context whose .git/config remote reads are
// memoized for the lifetime of that context, reusing an already-installed cache
// if there is one. Never span a git-remote mutation without calling
// InvalidateGitRemoteCache.
//
// main() installs this on the root context, which is the whole process. Answers
// are partitioned per repository (see gitRemoteCache), so a command
// walking several repositories stays correct. What process scope does NOT give is
// freshness over time: `entire mcp` serves an agent session from one context, so
// it installs a fresh window per request rather than pinning one snapshot for
// hours. A command that runs long enough for a remote to be added underneath it
// should do the same.
func WithGitRemoteCache(ctx context.Context) context.Context {
	if cacheFromContext(ctx) != nil {
		return ctx
	}
	return WithFreshGitRemoteCache(ctx)
}

// WithFreshGitRemoteCache installs a new cache even when the parent context
// already carries one, giving a long-lived process a per-unit-of-work window
// instead of one snapshot for its whole lifetime.
func WithFreshGitRemoteCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, gitRemoteCacheKey{}, &gitRemoteCache{byDir: map[string]*remoteSnapshot{}})
}

// InvalidateGitRemoteCache drops the memoized remote reads for every repository.
// Callers that add, rename, remove or re-point a git remote must call this, or
// later elections in the same command answer from the pre-mutation config. A
// caller performing several mutations needs one call after the last of them.
func InvalidateGitRemoteCache(ctx context.Context) {
	c := cacheFromContext(ctx)
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Dropping the map is enough: a goroutine already holding a snapshot finishes
	// against it and its result is simply discarded with the old map, rather than
	// being observed by anyone after the mutation.
	c.byDir = map[string]*remoteSnapshot{}
}

func cacheFromContext(ctx context.Context) *gitRemoteCache {
	c, ok := ctx.Value(gitRemoteCacheKey{}).(*gitRemoteCache)
	if !ok {
		return nil
	}
	return c
}

// gitReadRepo identifies the repository the memoized git reads resolve against,
// and so the cache partition: the context's scoped worktree root when there is
// one, else the repo containing the process working directory that `git` would
// inherit. Returns false when neither can be established, which callers read as
// "do not cache" — an unidentifiable repository must not share another's answers.
//
// The repo root rather than the raw cwd: `git remote get-url` answers the same
// from any subdirectory of a repository, so two calls from different
// subdirectories should share one answer. paths.WorktreeRoot is memoized per cwd,
// so this stays far cheaper than the subprocess it is protecting.
func gitReadRepo(ctx context.Context) (string, bool) {
	if root, ok := settings.WorktreeRoot(ctx); ok && root != "" {
		return root, true
	}
	root, err := paths.WorktreeRoot(ctx)
	if err != nil || root == "" {
		return "", false
	}
	return root, true
}

// snapshotFor returns the per-repository snapshot, or nil when this call must not
// be cached. Acquires c.mu for the map access and releases it before returning;
// callers must NOT hold it. The lock is deliberately never held across a git read
// (see gitRemoteCache).
func (c *gitRemoteCache) snapshotFor(ctx context.Context) *remoteSnapshot {
	repoRoot, ok := gitReadRepo(ctx)
	if !ok {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	snap := c.byDir[repoRoot]
	if snap == nil {
		snap = &remoteSnapshot{member: map[string]bool{}}
		c.byDir[repoRoot] = snap
	}
	return snap
}

// cachedRemotesInConfigOrder returns the memoized remote list for this call's
// repository, computing it via read on a miss. Without a cache installed, or when
// the repository cannot be established, it just calls read.
//
// A read that FAILS is never memoized. read reports "no remotes" and "could not
// tell" as distinct outcomes precisely so this can hold: caching a failure's empty
// list would turn one transient git hiccup into a process-long "this repo has no
// remotes", and the election answers that by silently skipping checkpoint sync.
func cachedRemotesInConfigOrder(ctx context.Context, read func(context.Context) ([]string, error)) []string {
	c := cacheFromContext(ctx)
	if c == nil {
		names, _ := read(ctx) //nolint:errcheck // uncached path keeps the historical best-effort contract
		return names
	}
	snap := c.snapshotFor(ctx)
	if snap == nil {
		names, _ := read(ctx) //nolint:errcheck // unidentifiable repo: same best-effort contract
		return names
	}
	snap.mu.Lock()
	defer snap.mu.Unlock()
	if snap.orderedSet {
		return snap.ordered
	}
	names, err := read(ctx)
	if err != nil {
		// Transient: answer this call, leave the slot unset so the next one retries.
		return nil
	}
	snap.ordered, snap.orderedSet = names, true
	return snap.ordered
}

// cachedIsConfiguredRemote returns the memoized answer for name in this call's
// repository, computing it via probe on a miss. Without a cache installed, or when
// the repository cannot be established, it just calls probe.
//
// A probe that FAILS is never memoized, for the mirror-image reason: a memoized
// false makes ResolveCheckpointSyncRemote fail closed on a configured
// checkpoint_push_remote ("is not a configured git remote") for the rest of the
// process, from one git that could not run.
func cachedIsConfiguredRemote(ctx context.Context, name string, probe func() (bool, error)) bool {
	c := cacheFromContext(ctx)
	if c == nil {
		got, _ := probe() //nolint:errcheck // uncached path keeps the historical best-effort contract
		return got
	}
	snap := c.snapshotFor(ctx)
	if snap == nil {
		got, _ := probe() //nolint:errcheck // unidentifiable repo: same best-effort contract
		return got
	}
	snap.mu.Lock()
	defer snap.mu.Unlock()
	if got, ok := snap.member[name]; ok {
		return got
	}
	got, err := probe()
	if err != nil {
		return false // answer this call; the next one retries
	}
	snap.member[name] = got
	return got
}
