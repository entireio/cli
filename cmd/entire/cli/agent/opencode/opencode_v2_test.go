package opencode

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// v2Export is a minimal OpenCode 2 export: a user text turn and an assistant
// turn with a file-modifying tool call.
const v2Export = `{
  "info": {"id": "ses_v2", "title": "V2 session", "time": {"created": 1700000000000, "updated": 1700000001000}},
  "messages": [
    {
      "id": "msg_1",
      "type": "user",
      "time": {"created": 1700000000000},
      "text": "hello",
      "files": [],
      "agents": []
    },
    {
      "id": "msg_2",
      "type": "assistant",
      "time": {"created": 1700000000001, "completed": 1700000000002},
      "tokens": {"input": 10, "output": 5, "reasoning": 2, "cache": {"read": 1, "write": 0}},
      "cost": 0.01,
      "content": [
        {"type": "text", "text": "hi there"},
        {
          "type": "tool",
          "id": "call_1",
          "name": "write",
          "state": {
            "status": "completed",
            "input": {"path": "/tmp/x.go"},
            "content": [{"type": "text", "text": "ok"}]
          }
        }
      ]
    }
  ]
}`

func TestNormalizeExportSession_V2(t *testing.T) {
	t.Parallel()

	session, err := ParseExportSession([]byte(v2Export))
	if err != nil {
		t.Fatalf("ParseExportSession: %v", err)
	}
	if session == nil {
		t.Fatal("nil session")
	}

	if session.Info.ID != "ses_v2" {
		t.Errorf("info id = %q, want ses_v2", session.Info.ID)
	}
	if session.Info.CreatedAt != 1700000000000 || session.Info.UpdatedAt != 1700000001000 {
		t.Errorf("info times = (%d,%d)", session.Info.CreatedAt, session.Info.UpdatedAt)
	}
	if len(session.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(session.Messages))
	}

	user := session.Messages[0]
	if user.Info.Role != roleUser || ExtractTextFromParts(user.Parts) != "hello" {
		t.Errorf("user message = %+v", user)
	}

	assistant := session.Messages[1]
	if assistant.Info.Role != roleAssistant {
		t.Fatalf("assistant role = %q", assistant.Info.Role)
	}
	if assistant.Info.Tokens == nil || assistant.Info.Tokens.Input != 10 || assistant.Info.Tokens.Output != 5 {
		t.Errorf("assistant tokens = %+v", assistant.Info.Tokens)
	}
	if assistant.Info.Cost != 0.01 {
		t.Errorf("assistant cost = %v", assistant.Info.Cost)
	}

	var tool *Part
	for i := range assistant.Parts {
		if assistant.Parts[i].Type == "tool" {
			tool = &assistant.Parts[i]
		}
	}
	if tool == nil {
		t.Fatal("tool part missing")
	}
	if tool.Tool != "write" || tool.CallID != "call_1" {
		t.Errorf("tool = %+v", tool)
	}
	if tool.State == nil || tool.State.Status != "completed" || tool.State.Output != "ok" {
		t.Errorf("tool state = %+v", tool.State)
	}

	files := modifiedFilesFromMessages(session.Messages, 0)
	if len(files) != 1 || files[0] != "/tmp/x.go" {
		t.Errorf("modified files = %v, want [/tmp/x.go]", files)
	}
}

func TestExtractAllUserPrompts_V2(t *testing.T) {
	t.Parallel()

	prompts, err := ExtractAllUserPrompts([]byte(v2Export))
	if err != nil {
		t.Fatalf("ExtractAllUserPrompts: %v", err)
	}
	if len(prompts) != 1 || prompts[0] != "hello" {
		t.Fatalf("prompts = %v, want [hello]", prompts)
	}
}

func TestCalculateTokenUsage_V2(t *testing.T) {
	t.Parallel()

	usage, err := (&OpenCodeAgent{}).CalculateTokenUsage([]byte(v2Export), 0)
	if err != nil {
		t.Fatalf("CalculateTokenUsage: %v", err)
	}
	if usage.InputTokens != 10 || usage.OutputTokens != 5 || usage.CacheReadTokens != 1 {
		t.Fatalf("usage = %+v", usage)
	}
}

// TestChunkReassemblePreservesV2 proves the raw transcript stored for a
// checkpoint round-trips OpenCode 2 fields, which the typed v1 model would drop
// (and which `opencode session import` needs to resume the session).
func TestChunkReassemblePreservesV2(t *testing.T) {
	t.Parallel()

	agent := &OpenCodeAgent{}
	chunks, err := agent.ChunkTranscript(context.Background(), []byte(v2Export), 200)
	if err != nil {
		t.Fatalf("ChunkTranscript: %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want >= 2", len(chunks))
	}

	reassembled, err := agent.ReassembleTranscript(chunks)
	if err != nil {
		t.Fatalf("ReassembleTranscript: %v", err)
	}
	if !strings.Contains(string(reassembled), `"type":"assistant"`) {
		t.Fatalf("reassembled transcript dropped v2 fields: %s", reassembled)
	}

	session, err := ParseExportSession(reassembled)
	if err != nil {
		t.Fatalf("ParseExportSession(reassembled): %v", err)
	}
	if len(session.Messages) != 2 {
		t.Fatalf("reassembled messages = %d, want 2", len(session.Messages))
	}
}

// TestRunOpenCodeExportToFile_PrefersSessionSubcommand pins the OpenCode 2
// invocation while keeping the v1 fallback: a v1-only stub fails `session
// export` and succeeds `export`.
func TestRunOpenCodeExportToFile_PrefersSessionSubcommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub opencode is a shell script")
	}
	// No t.Parallel: t.Setenv.

	dir := t.TempDir()
	root := mustOpenRoot(t, dir)
	const staged = ".export-ses_v2.json-1"

	stubDir := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = session ]; then exit 1; fi\n" +
		"printf '%s' '" + v2Export + "'\n"
	if err := os.WriteFile(filepath.Join(stubDir, "opencode"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir)

	if err := runOpenCodeExportToFile(context.Background(), root, "ses_v2", staged); err != nil {
		t.Fatalf("runOpenCodeExportToFile: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, staged))
	if err != nil {
		t.Fatal(err)
	}
	if !openCodeExportLooksValid(got) {
		t.Fatalf("staged export is not valid session JSON: %s", got)
	}
}

// TestRunOpenCodeExportToFile_RejectsHelpOutput proves a subcommand that exits 0
// with a help page does not count as a successful export (the OpenCode 2 failure
// mode observed in the wild).
func TestRunOpenCodeExportToFile_RejectsHelpOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub opencode is a shell script")
	}
	// No t.Parallel: t.Setenv.

	dir := t.TempDir()
	root := mustOpenRoot(t, dir)

	stubDir := t.TempDir()
	script := "#!/bin/sh\nprintf 'DESCRIPTION\\n  OpenCode command line interface\\nUSAGE\\n  opencode <subcommand>\\n'\n"
	if err := os.WriteFile(filepath.Join(stubDir, "opencode"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir)

	err := runOpenCodeExportToFile(context.Background(), root, "ses_help", ".export-ses_help.json-1")
	if err == nil {
		t.Fatal("expected help output to be rejected as an export")
	}
}

func TestOpenCodeExportLooksValid(t *testing.T) {
	t.Parallel()

	if !openCodeExportLooksValid([]byte(v2Export)) {
		t.Error("v2 export not recognized as valid")
	}
	if openCodeExportLooksValid([]byte("USAGE\n  opencode\n")) {
		t.Error("help text recognized as valid")
	}
}
