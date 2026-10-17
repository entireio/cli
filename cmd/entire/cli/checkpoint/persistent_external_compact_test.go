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

// installCompactingExternalAgent registers a mock external agent whose
// compact-transcript emits one line per input line ("agent-compacted N").
// mode "fail" exits non-zero, "assets" adds an asset, "secret" embeds a credential,
// "empty" returns an empty transcript, "prefixfail" fails on inputs under 4 lines.
func installCompactingExternalAgent(t *testing.T, name, agentType, mode string) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	t.Cleanup(agent.SnapshotRegistryForTesting())

	extra := ""
	if mode == "assets" {
		extra = `,"assets":[{"name":"img-001.png","media_type":"image/png","data":"aGk="}]`
	}
	text := `agent-compacted $i`
	if mode == "secret" {
		text = `agent-compacted $i sk-ant-api03-xK9mZ2vL8nQ5rT1wY4bC7dF0gH3jE6pA`
	}
	script := `#!/bin/sh
case "$1" in
  info)
    echo '{"protocol_version":1,"name":"` + name + `","type":"` + agentType + `","description":"compact test agent","protected_dirs":[],"hook_names":[],"capabilities":{"compact_transcript":true}}'
    ;;
  compact-transcript)
    [ "` + mode + `" = "fail" ] && { echo "compactor exploded" >&2; exit 1; }
    [ "` + mode + `" = "empty" ] && { echo '{"transcript":""}'; exit 0; }
    n=$(grep -c '' "$3")
    [ "` + mode + `" = "prefixfail" ] && [ "$n" -lt 4 ] && { echo "prefix exploded" >&2; exit 1; }
    i=1
    out=""
    while [ "$i" -le "$n" ]; do
      out="$out{\"v\":1,\"agent\":\"` + name + `\",\"type\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"` + text + `\"}]}
"
      i=$((i+1))
    done
    printf '{"transcript":"%s"` + extra + `}' "$(printf '%s' "$out" | base64 | tr -d '\n')"
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

func writeExternalAgentCheckpoint(t *testing.T, agentType types.AgentType, cpHex string, start int) (*GitStore, string) {
	t.Helper()
	repo := setupTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID(cpHex)
	err := store.Write(context.Background(), Session{
		CheckpointID:              cpID,
		SessionID:                 "session-ext",
		Strategy:                  "manual-commit",
		Transcript:                redact.AlreadyRedacted(claudeStyleTranscript()),
		Agent:                     agentType,
		CheckpointTranscriptStart: start,
		AuthorName:                "Test",
		AuthorEmail:               "test@test.com",
	})
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	return store, cpID.Path() + "/0/"
}

// An external agent declaring compact_transcript writes transcript.jsonl with
// its own compactor.
func TestWriteCommitted_ExternalAgentCompactsTranscript(t *testing.T) {
	// Cannot use t.Parallel: mutates $PATH and the agent registry.
	installCompactingExternalAgent(t, "compactok", "Compact OK", "")
	store, sessionPath := writeExternalAgentCheckpoint(t, "Compact OK", "d4e5f6a1b2c3", 2)

	compact, ok := readBranchFile(t, store, sessionPath+paths.CompactTranscriptFileName)
	if !ok {
		t.Fatal("transcript.jsonl missing from checkpoint tree")
	}
	if !strings.Contains(compact, "agent-compacted 4") || strings.Contains(compact, "reply two") {
		t.Fatalf("transcript.jsonl was not produced by the agent's compactor:\n%s", compact)
	}

	// The checkpoint starts at raw line 2; the agent compacts those two lines to two.
	meta := readSessionMetadata(t, store.repo, id.MustCheckpointID("d4e5f6a1b2c3"))
	marker, ok := meta.GetCompactTranscriptStart()
	if !ok || marker != 2 {
		t.Fatalf("compact_transcript_start = %d (set=%v), want 2", marker, ok)
	}
}

// A failing agent compactor falls back to the built-in compactor.
func TestWriteCommitted_ExternalCompactorFailureFallsBack(t *testing.T) {
	// Cannot use t.Parallel: mutates $PATH and the agent registry.
	installCompactingExternalAgent(t, "compactfail", "Compact Fail", "fail")
	store, sessionPath := writeExternalAgentCheckpoint(t, "Compact Fail", "e5f6a1b2c3d4", 0)

	compact, ok := readBranchFile(t, store, sessionPath+paths.CompactTranscriptFileName)
	if !ok || !strings.Contains(compact, "reply two") {
		t.Fatalf("expected the built-in compact transcript after the agent compactor failed, got (present=%v):\n%s", ok, compact)
	}
}

// Agent output with assets falls back: checkpoints don't store assets.
func TestWriteCommitted_ExternalCompactorAssetsFallBack(t *testing.T) {
	// Cannot use t.Parallel: mutates $PATH and the agent registry.
	installCompactingExternalAgent(t, "compactassets", "Compact Assets", "assets")
	store, sessionPath := writeExternalAgentCheckpoint(t, "Compact Assets", "f6a1b2c3d4e5", 0)

	compact, ok := readBranchFile(t, store, sessionPath+paths.CompactTranscriptFileName)
	if !ok || !strings.Contains(compact, "reply two") || strings.Contains(compact, "agent-compacted") {
		t.Fatalf("expected the built-in compact transcript when the agent returned assets, got (present=%v):\n%s", ok, compact)
	}
}

// The agent's output is redacted before it is stored.
func TestWriteCommitted_ExternalCompactorOutputIsRedacted(t *testing.T) {
	// Cannot use t.Parallel: mutates $PATH and the agent registry.
	installCompactingExternalAgent(t, "compactsecret", "Compact Secret", "secret")
	store, sessionPath := writeExternalAgentCheckpoint(t, "Compact Secret", "a1b2c3d4e5f7", 0)

	compact, ok := readBranchFile(t, store, sessionPath+paths.CompactTranscriptFileName)
	if !ok {
		t.Fatal("transcript.jsonl missing from checkpoint tree")
	}
	if strings.Contains(compact, "sk-ant-api03-xK9mZ2vL8nQ5rT1wY4bC7dF0gH3jE6pA") {
		t.Fatalf("agent-compacted transcript stored an unredacted credential:\n%s", compact)
	}
	if !strings.Contains(compact, "agent-compacted") {
		t.Fatalf("expected the agent's (redacted) output, got:\n%s", compact)
	}
}

// An empty agent transcript falls back to the built-in compactor.
func TestWriteCommitted_ExternalCompactorEmptyFallsBack(t *testing.T) {
	// Cannot use t.Parallel: mutates $PATH and the agent registry.
	installCompactingExternalAgent(t, "compactempty", "Compact Empty", "empty")
	store, sessionPath := writeExternalAgentCheckpoint(t, "Compact Empty", "b2c3d4e5f6a8", 0)

	compact, ok := readBranchFile(t, store, sessionPath+paths.CompactTranscriptFileName)
	if !ok || !strings.Contains(compact, "reply two") {
		t.Fatalf("expected the built-in compact transcript after an empty agent transcript, got (present=%v):\n%s", ok, compact)
	}
}

// When only the prefix can't be compacted, the agent's transcript is kept and
// the checkpoint scope starts at line 0.
func TestWriteCommitted_ExternalCompactorPrefixFailureScopesFromZero(t *testing.T) {
	// Cannot use t.Parallel: mutates $PATH and the agent registry.
	installCompactingExternalAgent(t, "compactprefix", "Compact Prefix", "prefixfail")
	store, sessionPath := writeExternalAgentCheckpoint(t, "Compact Prefix", "c3d4e5f6a1b9", 2)

	compact, ok := readBranchFile(t, store, sessionPath+paths.CompactTranscriptFileName)
	if !ok || !strings.Contains(compact, "agent-compacted 4") {
		t.Fatalf("expected the agent's transcript, got (present=%v):\n%s", ok, compact)
	}
	meta := readSessionMetadata(t, store.repo, id.MustCheckpointID("c3d4e5f6a1b9"))
	if marker, _ := meta.GetCompactTranscriptStart(); marker != 0 {
		t.Fatalf("compact_transcript_start = %d, want 0 when the prefix can't be compacted", marker)
	}
}

func TestLeadingLines(t *testing.T) {
	t.Parallel()
	in := []byte("a\nb\nc")
	tests := []struct {
		n    int
		want string
	}{
		{0, ""},
		{-1, ""},
		{1, "a\n"},
		{2, "a\nb\n"},
		{3, "a\nb\nc"},
		{5, "a\nb\nc"},
	}
	for _, tt := range tests {
		if got := string(leadingLines(in, tt.n)); got != tt.want {
			t.Errorf("leadingLines(%q, %d) = %q, want %q", in, tt.n, got, tt.want)
		}
	}
}

func TestCountLines(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"a\n", 1},
		{"a\n\n  \nb\n", 2},
	}
	for _, tt := range tests {
		if got := countLines([]byte(tt.in)); got != tt.want {
			t.Errorf("countLines(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}
