package paths

import (
	"context"
	"path/filepath"
	"testing"
)

func TestWithWorktreeRoot_PinsResolution(t *testing.T) {
	t.Parallel()

	// A path that is definitely not a git repository: the override must win
	// without ever shelling out to git.
	pinned := filepath.Join(t.TempDir(), "not-a-repo")
	ctx := WithWorktreeRoot(context.Background(), pinned)

	got, err := WorktreeRoot(ctx)
	if err != nil {
		t.Fatalf("WorktreeRoot() error = %v, want nil", err)
	}
	if got != pinned {
		t.Errorf("WorktreeRoot() = %q, want %q", got, pinned)
	}

	abs, err := AbsPath(ctx, EntireDir)
	if err != nil {
		t.Fatalf("AbsPath() error = %v", err)
	}
	if want := filepath.Join(pinned, EntireDir); abs != want {
		t.Errorf("AbsPath() = %q, want %q", abs, want)
	}
}

func TestWithWorktreeRoot_EmptyIsNoOp(t *testing.T) {
	t.Parallel()

	ctx := WithWorktreeRoot(context.Background(), "")
	if _, ok := worktreeRootFromContext(ctx); ok {
		t.Error("empty root was stored in the context, want no-op")
	}
}
