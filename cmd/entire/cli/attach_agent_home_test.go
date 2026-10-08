package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

// TestAttach_ClearsTheAgentHomeOfAReplacedTranscript checks that attach drops a
// stored agent home when it records a different transcript, and keeps it when
// the transcript is unchanged. The next turn start records the right home.
func TestAttach_ClearsTheAgentHomeOfAReplacedTranscript(t *testing.T) {
	const home = "/recorded/agent/home"
	tests := []struct {
		name       string
		transcript func(attached string) string
		wantHome   string
	}{
		{name: "different transcript", transcript: func(string) string { return "/elsewhere/session.jsonl" }},
		{name: "same transcript", transcript: func(attached string) string { return attached }, wantHome: home},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupAttachTestRepo(t)
			sessionID := "test-attach-agent-home"
			claudeDir := t.TempDir()
			t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", claudeDir)
			attached := filepath.Join(claudeDir, sessionID+".jsonl")
			if err := os.WriteFile(attached, []byte(`{"type":"user","message":{"role":"user","content":"hello"},"uuid":"u1"}`+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			// Stale, so attach does not wait for the transcript to settle.
			stale := time.Now().Add(-3 * time.Minute)
			if err := os.Chtimes(attached, stale, stale); err != nil {
				t.Fatal(err)
			}

			ctx := context.Background()
			store, err := session.NewStateStore(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Save(ctx, &session.State{
				SessionID:      sessionID,
				AgentType:      agent.AgentTypeClaudeCode,
				StartedAt:      time.Now(),
				TranscriptPath: tt.transcript(attached),
				AgentHome:      home,
			}); err != nil {
				t.Fatal(err)
			}

			var out bytes.Buffer
			if err := runAttach(ctx, &out, &out, sessionID, agent.AgentNameClaudeCode, attachOptions{Force: true}); err != nil {
				t.Fatalf("runAttach failed: %v", err)
			}

			state, err := store.Load(ctx, sessionID)
			if err != nil || state == nil {
				t.Fatalf("Load = %v, %v", state, err)
			}
			if state.AgentHome != tt.wantHome {
				t.Errorf("AgentHome = %q, want %q", state.AgentHome, tt.wantHome)
			}
		})
	}
}
