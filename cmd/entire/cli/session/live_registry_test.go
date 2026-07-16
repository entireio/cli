package session

import (
	"context"
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
