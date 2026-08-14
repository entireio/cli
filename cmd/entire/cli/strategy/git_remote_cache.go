package strategy

import (
	"context"
	"sync"
)

// The checkpoint sync remote is elected from scratch on every call — a
// documented tradeoff (see CheckpointReadRemotes), and every election shells out
// to git to answer two questions about .git/config: which remotes exist, and
// does remote X exist. Those answers are identical for every election within one
// command, and there are several: `entire checkpoint list` resolves the chain
// four times (metadata-disconnection warning, the branch listing itself, the
// git-refs store's remote discovery, and the imported-rewind-point pass),
// costing 9 git subprocesses where the same command on the pre-election code
// spent 3.
//
// This caches only those two .git/config reads — never the election result.
// Settings and the captured-election file (#1991) stay uncached, so a write
// followed by a re-resolve still observes the new value; only "which git remotes
// exist" is memoized, and that changes just once in the whole CLI (`entire repo
// mirror use`, which invalidates below).
//
// Context-scoped and opt-in rather than a package global: an uninstrumented
// context behaves exactly as before, so tests keep the uncached path and cannot
// leak one temp repo's remote list into another. Production installs it once per
// process in main().

type gitRemoteCacheKey struct{}

// gitRemoteCache memoizes .git/config remote reads for one command invocation.
// Guarded by a mutex because a single command may resolve the chain from several
// goroutines (checkpoint hydration fans out).
type gitRemoteCache struct {
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
// memoized for the lifetime of that context. Install once per command; never
// span a git-remote mutation without calling InvalidateGitRemoteCache.
func WithGitRemoteCache(ctx context.Context) context.Context {
	if cacheFromContext(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, gitRemoteCacheKey{}, &gitRemoteCache{member: map[string]bool{}})
}

// InvalidateGitRemoteCache drops the memoized remote reads. Callers that add,
// rename, remove or re-point a git remote must call this, or later elections in
// the same command answer from the pre-mutation config.
func InvalidateGitRemoteCache(ctx context.Context) {
	c := cacheFromContext(ctx)
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ordered, c.orderedSet = nil, false
	c.member = map[string]bool{}
}

func cacheFromContext(ctx context.Context) *gitRemoteCache {
	c, ok := ctx.Value(gitRemoteCacheKey{}).(*gitRemoteCache)
	if !ok {
		return nil
	}
	return c
}

// cachedRemotesInConfigOrder returns the memoized remote list, computing it via
// read on a miss. Without a cache installed it just calls read.
func cachedRemotesInConfigOrder(ctx context.Context, read func() []string) []string {
	c := cacheFromContext(ctx)
	if c == nil {
		return read()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.orderedSet {
		c.ordered, c.orderedSet = read(), true
	}
	return c.ordered
}

// cachedIsConfiguredRemote returns the memoized answer for name, computing it
// via probe on a miss. Without a cache installed it just calls probe.
func cachedIsConfiguredRemote(ctx context.Context, name string, probe func() bool) bool {
	c := cacheFromContext(ctx)
	if c == nil {
		return probe()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if got, ok := c.member[name]; ok {
		return got
	}
	got := probe()
	c.member[name] = got
	return got
}
