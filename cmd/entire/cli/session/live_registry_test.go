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

func TestLiveRegistry_RegisterListUnregister(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	now := time.Now()
	state := &State{
		SessionID:           "live-reg-001",
		AgentType:           agent.AgentTypeClaudeCode,
		Phase:               PhaseActive,
		WorktreePath:        "/tmp/repo-a",
		LastInteractionTime: &now,
		FilesTouched:        []string{"feature.txt"},
		Owner:               &proclive.Identity{PID: 42, Start: "start", Host: "host"},
	}
	commonDir := filepath.Join(t.TempDir(), ".git")
	if err := RegisterLiveSession(state, commonDir); err != nil {
		t.Fatalf("RegisterLiveSession: %v", err)
	}

	entries, err := ListLiveSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("ListLiveSessions len=%d, want 1", len(entries))
	}
	if entries[0].SessionID != state.SessionID || entries[0].CommonDir != filepath.Clean(commonDir) {
		t.Fatalf("entry = %+v", entries[0])
	}

	if err := UnregisterLiveSession(state.SessionID); err != nil {
		t.Fatal(err)
	}
	entries, err = ListLiveSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("after unregister len=%d, want 0", len(entries))
	}
}

func TestLiveRegistry_SaveHooksRegisterAndClearUnregisters(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	dir := filepath.Join(t.TempDir(), "entire-sessions")
	store := NewStateStoreWithDir(dir)
	now := time.Now()
	state := &State{
		SessionID:           "live-reg-save-001",
		AgentType:           agent.AgentTypeClaudeCode,
		Phase:               PhaseActive,
		StartedAt:           now,
		LastInteractionTime: &now,
		BaseCommit:          "abc123",
		WorktreePath:        "/tmp/repo-a",
		FilesTouched:        []string{"a.txt"},
	}
	if err := store.Save(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	entries, err := ListLiveSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].SessionID != state.SessionID {
		t.Fatalf("expected registry entry after Save, got %+v", entries)
	}

	if err := store.Clear(context.Background(), state.SessionID); err != nil {
		t.Fatal(err)
	}
	entries, err = ListLiveSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected empty registry after Clear, got %+v", entries)
	}
}

func TestShouldRegisterLive_RejectsTombstone(t *testing.T) {
	state := &State{
		SessionID:               "x",
		Phase:                   PhaseActive,
		AdoptedIntoWorktreePath: "/other",
	}
	if ShouldRegisterLive(state) {
		t.Fatal("tombstoned session must not register")
	}
}

func TestRegisterLiveSession_NilStateNoPanic(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	if err := RegisterLiveSession(nil, "/tmp/git"); err != nil {
		t.Fatalf("RegisterLiveSession(nil) = %v, want nil", err)
	}
}

func TestRegisterLiveSession_AbsolutizesRelativeCommonDir(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)

	now := time.Now()
	state := &State{
		SessionID:           "live-reg-rel-001",
		AgentType:           agent.AgentTypeClaudeCode,
		Phase:               PhaseActive,
		WorktreePath:        repo,
		LastInteractionTime: &now,
		FilesTouched:        []string{"feature.txt"},
	}
	if err := RegisterLiveSession(state, ".git"); err != nil {
		t.Fatalf("RegisterLiveSession: %v", err)
	}

	// Reinterpret from a different CWD — persisted CommonDir must stay absolute.
	t.Chdir(t.TempDir())
	entries, err := ListLiveSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("len=%d, want 1", len(entries))
	}
	want, err := filepath.Abs(filepath.Join(repo, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	want = filepath.Clean(want)
	if entries[0].CommonDir != want {
		t.Fatalf("CommonDir = %q, want absolute %q", entries[0].CommonDir, want)
	}
	if !filepath.IsAbs(entries[0].CommonDir) {
		t.Fatalf("CommonDir must be absolute, got %q", entries[0].CommonDir)
	}
}

func TestListLiveSessions_SweepsExpiredEntries(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	stale := time.Now().Add(-LiveSessionMaxAge - time.Hour)
	fresh := time.Now()
	staleState := &State{
		SessionID:           "live-reg-stale-001",
		AgentType:           agent.AgentTypeClaudeCode,
		Phase:               PhaseActive,
		WorktreePath:        "/tmp/repo-stale",
		LastInteractionTime: &stale,
		FilesTouched:        []string{"a.txt"},
	}
	freshState := &State{
		SessionID:           "live-reg-fresh-001",
		AgentType:           agent.AgentTypeClaudeCode,
		Phase:               PhaseActive,
		WorktreePath:        "/tmp/repo-fresh",
		LastInteractionTime: &fresh,
		FilesTouched:        []string{"b.txt"},
	}
	commonDir := filepath.Join(t.TempDir(), ".git")
	if err := RegisterLiveSession(staleState, commonDir); err != nil {
		t.Fatal(err)
	}
	if err := RegisterLiveSession(freshState, commonDir); err != nil {
		t.Fatal(err)
	}

	entries, err := ListLiveSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].SessionID != freshState.SessionID {
		t.Fatalf("expected only fresh entry after TTL sweep, got %+v", entries)
	}
}
