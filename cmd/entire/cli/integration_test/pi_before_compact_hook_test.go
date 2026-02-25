//go:build integration

package integration

import (
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

func TestPiBeforeCompactHook_ResetsTranscriptOffset(t *testing.T) {
	t.Parallel()

	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire()

	transcriptPath := filepath.Join(env.RepoDir, ".entire", "tmp", "pi-compaction.jsonl")
	env.WriteFile(filepath.ToSlash(filepath.Join(".entire", "tmp", "pi-compaction.jsonl")), `{"type":"message","id":"1","message":{"role":"user","content":"hi"}}`)

	sessionID := "pi-compaction-session"

	state := &strategy.SessionState{
		SessionID:                 sessionID,
		AgentType:                 agent.AgentTypePi,
		CheckpointTranscriptStart: 42,
	}
	saveSessionStateToRepo(t, env.RepoDir, state)

	if err := env.SimulatePiBeforeCompact(sessionID, transcriptPath); err != nil {
		t.Fatalf("before-compact hook failed: %v", err)
	}

	loaded := loadSessionStateFromRepo(t, env.RepoDir, sessionID)
	if loaded == nil {
		t.Fatal("expected non-nil session state")
	}
	if loaded.CheckpointTranscriptStart != 0 {
		t.Fatalf("CheckpointTranscriptStart = %d, want 0", loaded.CheckpointTranscriptStart)
	}
}
