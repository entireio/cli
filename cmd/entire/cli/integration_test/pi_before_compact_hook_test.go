//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

func TestPiBeforeCompactHook_ResetsTranscriptOffset(t *testing.T) {
	t.Parallel()

	env := NewTestEnv(t)
	env.InitRepo()
	env.InitEntire("manual-commit")

	transcriptPath := filepath.Join(env.RepoDir, ".entire", "tmp", "pi-compaction.jsonl")
	env.WriteFile(filepath.ToSlash(filepath.Join(".entire", "tmp", "pi-compaction.jsonl")), `{"type":"message","id":"1","message":{"role":"user","content":"hi"}}`)

	sessionID := "pi-compaction-session"

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	if err := os.Chdir(env.RepoDir); err != nil {
		t.Fatalf("failed to chdir to repo: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(wd)
	})

	state := &strategy.SessionState{
		SessionID:                 sessionID,
		AgentType:                 agent.AgentTypePi,
		CheckpointTranscriptStart: 42,
	}
	if err := strategy.SaveSessionState(state); err != nil {
		t.Fatalf("failed to save session state: %v", err)
	}

	if err := env.SimulatePiBeforeCompact(sessionID, transcriptPath); err != nil {
		t.Fatalf("before-compact hook failed: %v", err)
	}

	loaded, err := strategy.LoadSessionState(sessionID)
	if err != nil {
		t.Fatalf("failed to load session state: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected non-nil session state")
	}
	if loaded.CheckpointTranscriptStart != 0 {
		t.Fatalf("CheckpointTranscriptStart = %d, want 0", loaded.CheckpointTranscriptStart)
	}
}
