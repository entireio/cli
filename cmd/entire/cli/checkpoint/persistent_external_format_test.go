package checkpoint

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/redact"
)

// installFormatExternalAgent registers a mock external agent with no
// compactor whose info declares transcriptFormat ("" declares none).
func installFormatExternalAgent(t *testing.T, name, agentType, transcriptFormat string) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	t.Cleanup(agent.SnapshotRegistryForTesting())

	format := ""
	if transcriptFormat != "" {
		format = `"transcript_format":"` + transcriptFormat + `",`
	}
	script := `#!/bin/sh
case "$1" in
  info)
    echo '{"protocol_version":1,"name":"` + name + `","type":"` + agentType + `","description":"format test agent",` + format + `"protected_dirs":[],"hook_names":[],"capabilities":{}}'
    ;;
esac
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "entire-agent-"+name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := external.DiscoverAndRegisterNamedAlways(context.Background(), types.AgentName(name)); err != nil {
		t.Fatalf("register mock external agent: %v", err)
	}
}

// writeModelTranscriptCheckpoint writes a checkpoint whose transcript is in
// Claude Code's format and reports the model of each call.
func writeModelTranscriptCheckpoint(t *testing.T, agentType types.AgentType) string {
	t.Helper()
	native := strings.Join([]string{
		`{"type":"user","uuid":"u1","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"hello one"}}`,
		`{"type":"assistant","uuid":"a1","timestamp":"2026-01-01T00:00:01Z","message":{"id":"msg_1","role":"assistant","model":"claude-test","content":[{"type":"text","text":"reply one"}],"usage":{"input_tokens":5,"cache_read_input_tokens":3,"output_tokens":7}}}`,
	}, "\n") + "\n"

	repo := setupTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID("f6a1b2c3d4e5")
	err := store.Write(context.Background(), Session{
		CheckpointID: cpID,
		SessionID:    "session-format",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(native)),
		Agent:        agentType,
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	compact, ok := readBranchFile(t, store, cpID.Path()+"/0/"+paths.CompactTranscriptFileName)
	if !ok {
		t.Fatal("transcript.jsonl missing from checkpoint tree")
	}
	return compact
}

// An external agent declaring transcript_format gets that format's built-in
// conversion. Detection alone converts Claude-style JSONL from an agent not
// named claude-code with Cursor's accounting, which records no model.
func TestWriteCommitted_ExternalAgentDeclaredTranscriptFormat(t *testing.T) {
	// Cannot use t.Parallel: mutates $PATH and the agent registry.
	installFormatExternalAgent(t, "fmtdeclared", "Format Declared", "claude-code")
	compact := writeModelTranscriptCheckpoint(t, "Format Declared")
	if !strings.Contains(compact, `"agent":"fmtdeclared"`) || !strings.Contains(compact, `"model":"claude-test"`) {
		t.Fatalf("transcript.jsonl was not converted as the declared format:\n%s", compact)
	}
}

func TestWriteCommitted_ExternalAgentWithoutTranscriptFormatDetects(t *testing.T) {
	// Cannot use t.Parallel: mutates $PATH and the agent registry.
	installFormatExternalAgent(t, "fmtnone", "Format None", "")
	compact := writeModelTranscriptCheckpoint(t, "Format None")
	if !strings.Contains(compact, "reply one") || strings.Contains(compact, `"model"`) {
		t.Fatalf("transcript.jsonl should be the detected (Cursor-accounting) conversion:\n%s", compact)
	}
}
