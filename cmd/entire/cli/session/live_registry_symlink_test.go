package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
)

// symlinkedRepo builds <root>/real/.git plus a <root>/link -> <root>/real
// symlink and returns (realCommonDir, linkedCommonDir, realWorktree,
// linkedWorktree). Both common dirs name the same directory through different
// path spellings — the shape macOS produces for every /tmp path (/tmp is a
// symlink to /private/tmp) and that any worktree under a symlinked parent
// produces on Linux.
func symlinkedRepo(t *testing.T) (realCommonDir, linkedCommonDir, realWorktree, linkedWorktree string) {
	t.Helper()
	root := t.TempDir()
	realWorktree = filepath.Join(root, "real")
	realCommonDir = filepath.Join(realWorktree, ".git")
	if err := os.MkdirAll(realCommonDir, 0o750); err != nil {
		t.Fatalf("mkdir real common dir: %v", err)
	}
	linkedWorktree = filepath.Join(root, "link")
	if err := os.Symlink(realWorktree, linkedWorktree); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	linkedCommonDir = filepath.Join(linkedWorktree, ".git")
	return realCommonDir, linkedCommonDir, realWorktree, linkedWorktree
}

func liveStateForSymlinkTest(sessionID, worktree string) *State {
	now := time.Now()
	return &State{
		SessionID:           sessionID,
		AgentType:           agent.AgentTypeClaudeCode,
		Phase:               PhaseActive,
		WorktreePath:        worktree,
		LastInteractionTime: &now,
		Owner:               &proclive.Identity{PID: 7, Start: "start", Host: "host"},
	}
}

// A live entry registered under one spelling of its common dir must be
// removable through a symlink-equivalent spelling. Otherwise the entry
// outlives the session and a later commit in another repo can auto-adopt a
// session that is already gone.
func TestUnregisterLiveSessionMatchesSymlinkedCommonDir(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	realCommonDir, linkedCommonDir, realWorktree, _ := symlinkedRepo(t)

	state := liveStateForSymlinkTest("live-symlink-001", realWorktree)
	if err := RegisterLiveSession(state, realCommonDir); err != nil {
		t.Fatalf("RegisterLiveSession: %v", err)
	}
	if err := UnregisterLiveSession(state.SessionID, linkedCommonDir); err != nil {
		t.Fatalf("UnregisterLiveSession: %v", err)
	}

	entries, err := ListLiveSessions()
	if err != nil {
		t.Fatalf("ListLiveSessions: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("entry survived unregister through symlinked common dir: %+v", entries)
	}
}

// Same for the worktree leg of the ownership check.
func TestUnregisterLiveSessionForWorktreeMatchesSymlinkedWorktreePath(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	realCommonDir, _, realWorktree, linkedWorktree := symlinkedRepo(t)

	state := liveStateForSymlinkTest("live-symlink-002", realWorktree)
	if err := RegisterLiveSession(state, realCommonDir); err != nil {
		t.Fatalf("RegisterLiveSession: %v", err)
	}
	if err := UnregisterLiveSessionForWorktree(
		context.Background(), state.SessionID, realCommonDir, linkedWorktree,
	); err != nil {
		t.Fatalf("UnregisterLiveSessionForWorktree: %v", err)
	}

	entries, err := ListLiveSessions()
	if err != nil {
		t.Fatalf("ListLiveSessions: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("entry survived unregister through symlinked worktree path: %+v", entries)
	}
}

// The adopt claim is the cross-repository handoff token. If the finalize step
// spells the claiming common dir differently from the step that took the
// claim, the release is a no-op and the stale claim blocks every further
// adoption of that session for AdoptClaimMaxAge (freshAdoptClaim skips it as
// an auto-adopt candidate).
func TestReleaseLiveSessionClaimMatchesSymlinkedOwner(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	realCommonDir, linkedCommonDir, realWorktree, linkedWorktree := symlinkedRepo(t)
	ctx := context.Background()

	state := liveStateForSymlinkTest("live-symlink-003", realWorktree)
	if err := RegisterLiveSession(state, realCommonDir); err != nil {
		t.Fatalf("RegisterLiveSession: %v", err)
	}
	claimed, err := ClaimLiveSessionContext(ctx, state.SessionID, AdoptClaim{
		ByCommonDir:    realCommonDir,
		ByWorktreePath: realWorktree,
		AttemptID:      "attempt-1",
		At:             time.Now(),
	})
	if err != nil || !claimed {
		t.Fatalf("ClaimLiveSessionContext claimed=%v err=%v", claimed, err)
	}

	released, err := ReleaseLiveSessionClaimIfOwned(ctx, state.SessionID, AdoptClaim{
		ByCommonDir:    linkedCommonDir,
		ByWorktreePath: linkedWorktree,
		AttemptID:      "attempt-1",
	})
	if err != nil {
		t.Fatalf("ReleaseLiveSessionClaimIfOwned: %v", err)
	}
	if !released {
		t.Fatal("claim not released through symlink-equivalent owner paths")
	}
	claim, err := LiveSessionClaimContext(ctx, state.SessionID)
	if err != nil {
		t.Fatalf("LiveSessionClaimContext: %v", err)
	}
	if claim != nil {
		t.Fatalf("claim still present after release: %+v", claim)
	}
}

// A claim holder must be able to renew its own claim when the second call
// spells its paths through a symlink; otherwise adoptClaimHeldByOther reports
// the holder's own claim as someone else's and the retry is refused.
func TestClaimLiveSessionRenewsThroughSymlinkedOwner(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	realCommonDir, linkedCommonDir, realWorktree, linkedWorktree := symlinkedRepo(t)
	ctx := context.Background()

	state := liveStateForSymlinkTest("live-symlink-004", realWorktree)
	if err := RegisterLiveSession(state, realCommonDir); err != nil {
		t.Fatalf("RegisterLiveSession: %v", err)
	}
	if claimed, err := ClaimLiveSessionContext(ctx, state.SessionID, AdoptClaim{
		ByCommonDir: realCommonDir, ByWorktreePath: realWorktree,
		AttemptID: "attempt-1", At: time.Now(),
	}); err != nil || !claimed {
		t.Fatalf("first claim claimed=%v err=%v", claimed, err)
	}

	claimed, err := ClaimLiveSessionContext(ctx, state.SessionID, AdoptClaim{
		ByCommonDir: linkedCommonDir, ByWorktreePath: linkedWorktree,
		AttemptID: "attempt-1", At: time.Now(),
	})
	if err != nil {
		t.Fatalf("renewing claim: %v", err)
	}
	if !claimed {
		t.Fatal("owner could not renew its own claim through symlink-equivalent paths")
	}
}
