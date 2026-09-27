package testutil

import (
	"path/filepath"
	"testing"

	cliagent "github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestRestoredSessionTranscriptPathSupportsAntigravity(t *testing.T) {
	brainDir := filepath.Join(t.TempDir(), "brain")
	t.Setenv("ENTIRE_TEST_ANTIGRAVITY_BRAIN_DIR", brainDir)

	got, ok := RestoredSessionTranscriptPath(t, "/repo", SessionMetadata{
		Agent:     string(cliagent.AgentTypeAntigravity),
		SessionID: "conv-123",
	})
	if !ok {
		t.Fatal("RestoredSessionTranscriptPath() ok = false, want true")
	}

	want := filepath.Join(brainDir, "conv-123", ".system_generated", "logs", "transcript_full.jsonl")
	if got != want {
		t.Fatalf("RestoredSessionTranscriptPath() = %q, want %q", got, want)
	}
}
