package paths

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

// TestWithWorktreeRoot_OverridesCwdResolution asserts the context override wins
// over the cwd-derived lookup, so callers can inspect an arbitrary repo without
// chdir'ing the process.
func TestWithWorktreeRoot_OverridesCwdResolution(t *testing.T) {
	t.Parallel()

	want := t.TempDir()
	ctx := WithWorktreeRoot(context.Background(), want)

	got, err := WorktreeRoot(ctx)
	if err != nil {
		t.Fatalf("WorktreeRoot returned error: %v", err)
	}
	if got != filepath.Clean(want) {
		t.Fatalf("WorktreeRoot = %q, want %q", got, filepath.Clean(want))
	}
}

// TestWithWorktreeRoot_EmptyRootIsIgnored keeps the override opt-in: an empty
// root must leave the context untouched rather than pinning the root to "".
func TestWithWorktreeRoot_EmptyRootIsIgnored(t *testing.T) {
	t.Parallel()

	ctx := WithWorktreeRoot(context.Background(), "")
	if _, ok := worktreeRootFromContext(ctx); ok {
		t.Fatal("empty root should not install an override")
	}
}

// TestWithWorktreeRoot_DoesNotPoisonCache is the regression guard for the risk
// that made this override tricky: WorktreeRoot caches by cwd, so an override
// call must neither read nor write that cache. Otherwise scanning repo A would
// make every later cwd-based lookup return A.
func TestWithWorktreeRoot_DoesNotPoisonCache(t *testing.T) {
	// Not parallel: it inspects and clears the package-level cache.
	ClearWorktreeRootCache()
	t.Cleanup(ClearWorktreeRootCache)

	override := t.TempDir()
	if _, err := WorktreeRoot(WithWorktreeRoot(context.Background(), override)); err != nil {
		t.Fatalf("WorktreeRoot returned error: %v", err)
	}

	worktreeRootMu.RLock()
	cached := worktreeRootCache
	worktreeRootMu.RUnlock()
	if cached != "" {
		t.Fatalf("override call populated the cwd cache with %q", cached)
	}
}

// TestWithWorktreeRoot_IgnoresPrimedCache proves the override is checked before
// the cache, not after: a primed cache entry for the current cwd must not win.
func TestWithWorktreeRoot_IgnoresPrimedCache(t *testing.T) {
	// Not parallel: it mutates the package-level cache.
	ClearWorktreeRootCache()
	t.Cleanup(ClearWorktreeRootCache)

	primed := t.TempDir()
	worktreeRootMu.Lock()
	worktreeRootCache = primed
	worktreeRootCacheDir = ""
	worktreeRootMu.Unlock()

	override := t.TempDir()
	got, err := WorktreeRoot(WithWorktreeRoot(context.Background(), override))
	if err != nil {
		t.Fatalf("WorktreeRoot returned error: %v", err)
	}
	if got != filepath.Clean(override) {
		t.Fatalf("WorktreeRoot = %q, want the override %q", got, filepath.Clean(override))
	}
}

// TestWithWorktreeRoot_ConcurrentOverridesAreIndependent covers the scan
// fan-out: many goroutines resolving different roots at once must each see
// their own.
func TestWithWorktreeRoot_ConcurrentOverridesAreIndependent(t *testing.T) {
	t.Parallel()

	roots := make([]string, 8)
	for i := range roots {
		roots[i] = t.TempDir()
	}

	var wg sync.WaitGroup
	errs := make([]error, len(roots))
	got := make([]string, len(roots))
	wg.Add(len(roots))
	for i, root := range roots {
		go func() {
			defer wg.Done()
			got[i], errs[i] = WorktreeRoot(WithWorktreeRoot(context.Background(), root))
		}()
	}
	wg.Wait()

	for i, root := range roots {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: WorktreeRoot returned error: %v", i, errs[i])
		}
		if got[i] != filepath.Clean(root) {
			t.Fatalf("goroutine %d: WorktreeRoot = %q, want %q", i, got[i], filepath.Clean(root))
		}
	}
}

// TestAbsPath_UsesWorktreeRootOverride asserts the override propagates to the
// derived helpers, which is what makes per-agent hook detection work.
func TestAbsPath_UsesWorktreeRootOverride(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	ctx := WithWorktreeRoot(context.Background(), root)

	got, err := AbsPath(ctx, ".entire/settings.json")
	if err != nil {
		t.Fatalf("AbsPath returned error: %v", err)
	}
	want := filepath.Join(filepath.Clean(root), ".entire/settings.json")
	if got != want {
		t.Fatalf("AbsPath = %q, want %q", got, want)
	}
}
