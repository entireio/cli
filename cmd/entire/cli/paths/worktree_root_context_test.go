package paths

import (
	"context"
	"path/filepath"
	"testing"
)

// TestWorktreeRoot_ContextOverrideWinsOverCwd is the core of the cwd fix: a turn
// that happened in a linked worktree carries that worktree explicitly, so every
// repo-relative resolution follows the turn rather than the hook process.
func TestWorktreeRoot_ContextOverrideWinsOverCwd(t *testing.T) {
	cwdRepo := t.TempDir()
	eventRoot := t.TempDir()
	t.Chdir(cwdRepo)

	got, err := WorktreeRoot(WithWorktreeRoot(context.Background(), eventRoot))
	if err != nil {
		t.Fatalf("WorktreeRoot: %v", err)
	}
	if got != filepath.Clean(eventRoot) {
		t.Errorf("WorktreeRoot = %q, want the context root %q", got, eventRoot)
	}
}

// TestWorktreeRoot_ContextOverrideSkipsGitEntirely pins that an explicit root is
// returned as an answer rather than as a hint: it must not shell out, so it
// works in a directory that is not a repository at all. That is what makes the
// dispatch path testable with a hand-built event before any agent populates one.
func TestWorktreeRoot_ContextOverrideSkipsGitEntirely(t *testing.T) {
	notARepo := t.TempDir()
	t.Chdir(notARepo)

	// Sanity: without the override this directory has no worktree root.
	if _, err := WorktreeRoot(context.Background()); err == nil {
		t.Skip("temp dir unexpectedly resolves to a repository; nothing to prove here")
	}

	want := t.TempDir()
	got, err := WorktreeRoot(WithWorktreeRoot(context.Background(), want))
	if err != nil {
		t.Fatalf("WorktreeRoot with override in a non-repository: %v", err)
	}
	if got != filepath.Clean(want) {
		t.Errorf("WorktreeRoot = %q, want %q", got, want)
	}
}

// TestWorktreeRoot_ContextOverrideDoesNotPoisonTheCwdCache guards the reason the
// override returns before the cache logic. WorktreeRoot memoizes keyed on cwd;
// storing an override under that key would hand the overridden root to the next
// caller in the same process that did not set one — in a hook process that is
// every subsequent resolution of the turn.
func TestWorktreeRoot_ContextOverrideDoesNotPoisonTheCwdCache(t *testing.T) {
	repo := t.TempDir()
	t.Chdir(repo)

	baseline, baseErr := WorktreeRoot(context.Background())

	foreign := t.TempDir()
	if _, err := WorktreeRoot(WithWorktreeRoot(context.Background(), foreign)); err != nil {
		t.Fatalf("WorktreeRoot with override: %v", err)
	}

	after, afterErr := WorktreeRoot(context.Background())
	if (baseErr == nil) != (afterErr == nil) {
		t.Fatalf("unoverridden resolution changed shape: before err=%v, after err=%v", baseErr, afterErr)
	}
	if after != baseline {
		t.Errorf("unoverridden WorktreeRoot = %q after an overridden call, want the unchanged %q", after, baseline)
	}
	if after == filepath.Clean(foreign) {
		t.Errorf("cache poisoned: unoverridden WorktreeRoot returned the overridden root %q", foreign)
	}
}

func TestWithWorktreeRoot_EmptyIsANoOp(t *testing.T) {
	ctx := context.Background()
	if got := WithWorktreeRoot(ctx, ""); got != ctx {
		t.Error("WithWorktreeRoot with an empty root returned a new context; want the original unchanged")
	}
	if root, ok := WorktreeRootFromContext(WithWorktreeRoot(ctx, "")); ok {
		t.Errorf("WorktreeRootFromContext reported a root %q for an empty override", root)
	}
}

func TestWorktreeRootFromContext(t *testing.T) {
	if root, ok := WorktreeRootFromContext(context.Background()); ok {
		t.Errorf("WorktreeRootFromContext on a bare context returned %q, want not-found", root)
	}

	dirty := "/tmp/repo/../repo/"
	root, ok := WorktreeRootFromContext(WithWorktreeRoot(context.Background(), dirty))
	if !ok {
		t.Fatal("WorktreeRootFromContext did not find the root that was set")
	}
	if want := filepath.Clean(dirty); root != want {
		t.Errorf("root = %q, want the cleaned %q", root, want)
	}
}
