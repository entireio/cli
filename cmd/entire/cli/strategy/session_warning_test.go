package strategy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/go-git/go-git/v6"
)

func TestWarnIfTooManySessions_AboveThreshold(t *testing.T) {

	dir := t.TempDir()
	if _, err := git.PlainInit(dir, false); err != nil {
		t.Fatalf("git init: %v", err)
	}
	t.Chdir(dir)

	ctx := context.Background()
	store, err := session.NewStateStore(ctx)
	if err != nil {
		t.Fatalf("NewStateStore: %v", err)
	}

	// Create sessions above the threshold.
	for i := 0; i <= orphanedSessionWarningThreshold; i++ {
		s := &session.State{
			SessionID:  fmt.Sprintf("session-%03d", i),
			BaseCommit: "abc123",
			StartedAt:  time.Now(),
			Phase:      session.PhaseIdle,
		}
		if err := store.Save(ctx, s); err != nil {
			t.Fatalf("Save session %d: %v", i, err)
		}
	}

	// Calling warnIfTooManySessions should create the rate-limit file.
	warnIfTooManySessions(ctx)

	stateDir := filepath.Join(dir, ".git", session.SessionStateDirName)
	warningPath := filepath.Join(stateDir, sessionWarningFile)
	if _, err := os.Stat(warningPath); err != nil {
		t.Errorf("expected rate-limit file at %s: %v", warningPath, err)
	}
}

func TestWarnIfTooManySessions_BelowThreshold(t *testing.T) {

	dir := t.TempDir()
	if _, err := git.PlainInit(dir, false); err != nil {
		t.Fatalf("git init: %v", err)
	}
	t.Chdir(dir)

	ctx := context.Background()
	store, err := session.NewStateStore(ctx)
	if err != nil {
		t.Fatalf("NewStateStore: %v", err)
	}

	// Create sessions at exactly the threshold (not above).
	for i := 0; i < orphanedSessionWarningThreshold; i++ {
		s := &session.State{
			SessionID:  fmt.Sprintf("session-%03d", i),
			BaseCommit: "abc123",
			StartedAt:  time.Now(),
			Phase:      session.PhaseIdle,
		}
		if err := store.Save(ctx, s); err != nil {
			t.Fatalf("Save session %d: %v", i, err)
		}
	}

	warnIfTooManySessions(ctx)

	stateDir := filepath.Join(dir, ".git", session.SessionStateDirName)
	warningPath := filepath.Join(stateDir, sessionWarningFile)
	if _, err := os.Stat(warningPath); !os.IsNotExist(err) {
		t.Errorf("rate-limit file should not exist below threshold, stat err: %v", err)
	}
}

func TestWarnIfTooManySessions_RateLimited(t *testing.T) {

	dir := t.TempDir()
	if _, err := git.PlainInit(dir, false); err != nil {
		t.Fatalf("git init: %v", err)
	}
	t.Chdir(dir)

	ctx := context.Background()
	store, err := session.NewStateStore(ctx)
	if err != nil {
		t.Fatalf("NewStateStore: %v", err)
	}

	for i := 0; i <= orphanedSessionWarningThreshold; i++ {
		s := &session.State{
			SessionID:  fmt.Sprintf("session-%03d", i),
			BaseCommit: "abc123",
			StartedAt:  time.Now(),
			Phase:      session.PhaseIdle,
		}
		if err := store.Save(ctx, s); err != nil {
			t.Fatalf("Save session %d: %v", i, err)
		}
	}

	// Pre-create the rate-limit file with a recent mtime.
	stateDir := filepath.Join(dir, ".git", session.SessionStateDirName)
	warningPath := filepath.Join(stateDir, sessionWarningFile)
	if err := os.WriteFile(warningPath, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	info1, _ := os.Stat(warningPath)
	mtime1 := info1.ModTime()

	// Wait a tiny bit so mtime would differ if the file were rewritten.
	time.Sleep(10 * time.Millisecond)

	warnIfTooManySessions(ctx)

	info2, _ := os.Stat(warningPath)
	mtime2 := info2.ModTime()

	if !mtime1.Equal(mtime2) {
		t.Errorf("rate-limit file was rewritten (mtime changed from %v to %v); expected no update within cooldown", mtime1, mtime2)
	}
}
