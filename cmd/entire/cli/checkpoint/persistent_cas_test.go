package checkpoint

import (
	"context"
	"sync"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/redact"
)

func TestGitStore_ConcurrentWritesPreserveBothCheckpoints(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	if err := store.ensureSessionsBranch(t.Context()); err != nil {
		t.Fatalf("ensure sessions branch: %v", err)
	}

	ctxA, ctxB, release := synchronizedFirstCAS(t)
	errCh := make(chan error, 2)
	go func() {
		errCh <- store.Write(ctxA, concurrentSession(id.MustCheckpointID("a1b2c3d4e5f6"), "session-a"))
	}()
	go func() {
		errCh <- store.Write(ctxB, concurrentSession(id.MustCheckpointID("b1c2d3e4f5a6"), "session-b"))
	}()
	release()
	for range 2 {
		if err := <-errCh; err != nil {
			t.Fatalf("concurrent write: %v", err)
		}
	}

	for _, checkpointID := range []id.CheckpointID{
		id.MustCheckpointID("a1b2c3d4e5f6"),
		id.MustCheckpointID("b1c2d3e4f5a6"),
	} {
		if _, err := store.Read(t.Context(), checkpointID); err != nil {
			t.Errorf("read checkpoint %s after race: %v", checkpointID, err)
		}
	}
}

func TestGitRefsStore_ConcurrentWritesPreserveBothSessions(t *testing.T) {
	t.Parallel()
	store := newRefsStore(t)
	checkpointID := id.MustCheckpointID("a1b2c3d4e5f6")
	ctxA, ctxB, release := synchronizedFirstCAS(t)
	errCh := make(chan error, 2)
	go func() {
		errCh <- store.Write(ctxA, concurrentSession(checkpointID, "session-a"))
	}()
	go func() {
		errCh <- store.Write(ctxB, concurrentSession(checkpointID, "session-b"))
	}()
	release()
	for range 2 {
		if err := <-errCh; err != nil {
			t.Fatalf("concurrent write: %v", err)
		}
	}

	summary, err := store.Read(t.Context(), checkpointID)
	if err != nil {
		t.Fatalf("read checkpoint after race: %v", err)
	}
	if got := len(summary.Sessions); got != 2 {
		t.Fatalf("session count after race = %d, want 2", got)
	}
}

func concurrentSession(checkpointID id.CheckpointID, sessionID string) Session {
	return Session{
		CheckpointID: checkpointID,
		SessionID:    sessionID,
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(sessionID + " transcript\n")),
		Prompts:      []string{"do the work"},
		FilesTouched: []string{sessionID + ".go"},
		AuthorName:   "Test Author",
		AuthorEmail:  "test@example.com",
	}
}

func synchronizedFirstCAS(t *testing.T) (context.Context, context.Context, func()) {
	t.Helper()
	ready := make(chan struct{}, 2)
	proceed := make(chan struct{})
	makeContext := func() context.Context {
		var once sync.Once
		return withBeforeRefCAS(t.Context(), func() {
			once.Do(func() {
				ready <- struct{}{}
				<-proceed
			})
		})
	}
	release := func() {
		t.Helper()
		<-ready
		<-ready
		close(proceed)
	}
	return makeContext(), makeContext(), release
}
