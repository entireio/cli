package qwencode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// realFixture is a genuine Qwen Code v0.21.14 session, produced by running the
// CLI against a local OpenAI-compatible endpoint. It is the regression guard
// for the envelope/payload split described in AGENT.md.
const realFixture = "testdata/real_session_v0_21_14.jsonl"

func readFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(realFixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

func writeFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, readFixture(t), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestIdentity(t *testing.T) {
	t.Parallel()

	a := NewQwenCodeAgent()
	if got := a.Name(); got != agent.AgentNameQwenCode {
		t.Errorf("Name() = %q", got)
	}
	if got := a.Type(); got != agent.AgentTypeQwenCode {
		t.Errorf("Type() = %q", got)
	}
	if dirs := a.ProtectedDirs(); len(dirs) != 1 || dirs[0] != ".qwen" {
		t.Errorf("ProtectedDirs() = %v, want [.qwen]", dirs)
	}
}

func TestRegisteredInRegistry(t *testing.T) {
	t.Parallel()

	got, err := agent.Get(agent.AgentNameQwenCode)
	if err != nil {
		t.Fatalf("agent.Get(qwen-code): %v", err)
	}
	if got.Name() != agent.AgentNameQwenCode {
		t.Errorf("registry returned %q", got.Name())
	}
}

// The fixture is real agent output, so this pins the envelope/payload split:
// a Claude-shaped envelope wrapping a Gemini-shaped message.
func TestRealFixture_EnvelopeAndPayloadShape(t *testing.T) {
	t.Parallel()

	a := &QwenCodeAgent{}
	path := writeFixture(t)

	lines, err := a.GetTranscriptPosition(path)
	if err != nil {
		t.Fatalf("GetTranscriptPosition: %v", err)
	}
	if lines == 0 {
		t.Fatal("fixture parsed to zero lines")
	}

	// role "model" (Gemini), not "assistant" (Claude/OpenAI). Getting this wrong
	// silently drops every assistant message.
	var sawModelRole, sawUsage, sawFunctionCall bool
	scanLines(readFixture(t), 0, func(line Line) {
		if line.Type == typeAssistant && line.Message != nil && line.Message.Role == "model" {
			sawModelRole = true
		}
		if line.Usage != nil && line.Usage.PromptTokenCount > 0 {
			sawUsage = true
		}
		if line.Message != nil {
			for _, p := range line.Message.Parts {
				if p.FunctionCall != nil {
					sawFunctionCall = true
				}
			}
		}
	})

	if !sawModelRole {
		t.Error(`no assistant line with message.role == "model"; the payload shape may have changed`)
	}
	if !sawUsage {
		t.Error("no usageMetadata found; token accounting would silently report zero")
	}
	if !sawFunctionCall {
		t.Error("no functionCall part found; tool calls would not be extracted")
	}
}

func TestCalculateTokenUsage(t *testing.T) {
	t.Parallel()

	a := &QwenCodeAgent{}
	usage, err := a.CalculateTokenUsage(readFixture(t), 0)
	if err != nil {
		t.Fatalf("CalculateTokenUsage: %v", err)
	}
	if usage.InputTokens == 0 || usage.OutputTokens == 0 {
		t.Errorf("expected non-zero usage from the real fixture, got %+v", usage)
	}
	// Qwen publishes no cache-write figure, so this must stay zero rather than
	// being inferred from the totals.
	if usage.CacheCreationTokens != 0 {
		t.Errorf("CacheCreationTokens = %d, want 0 (Qwen reports no cache-write)", usage.CacheCreationTokens)
	}
	if usage.CacheReadTokens == 0 {
		t.Error("expected cachedContentTokenCount to map to CacheReadTokens")
	}
}

// Per-message usage means the offset genuinely scopes, unlike Goose.
func TestCalculateTokenUsage_OffsetScopes(t *testing.T) {
	t.Parallel()

	a := &QwenCodeAgent{}
	data := readFixture(t)

	all, err := a.CalculateTokenUsage(data, 0)
	if err != nil {
		t.Fatalf("CalculateTokenUsage: %v", err)
	}
	total := scanLines(data, -1, func(Line) {})
	none, err := a.CalculateTokenUsage(data, total)
	if err != nil {
		t.Fatalf("CalculateTokenUsage: %v", err)
	}
	if none.InputTokens != 0 {
		t.Errorf("offset past the end should yield zero usage, got %+v", none)
	}
	if all.InputTokens == 0 {
		t.Error("offset 0 should yield the full total")
	}
}

// A tool_result line carries message.role "user". Keying prompts on the role
// would report every tool result as something the user typed.
func TestExtractPrompts_ExcludesToolResults(t *testing.T) {
	t.Parallel()

	a := &QwenCodeAgent{}
	prompts, err := a.ExtractPrompts(writeFixture(t), 0)
	if err != nil {
		t.Fatalf("ExtractPrompts: %v", err)
	}
	if len(prompts) == 0 {
		t.Fatal("expected at least one prompt from the real fixture")
	}
	for _, p := range prompts {
		if strings.Contains(p, "functionResponse") {
			t.Errorf("tool result leaked into prompts: %q", p)
		}
	}

	// Every real prompt line is provenance real_user.
	var realUserLines int
	scanLines(readFixture(t), 0, func(line Line) {
		if line.Type == typeUser && line.Provenance == provenanceRealUser {
			realUserLines++
		}
	})
	if len(prompts) != realUserLines {
		t.Errorf("got %d prompts but %d real_user lines", len(prompts), realUserLines)
	}
}

func TestExtractModifiedFiles_OnlyWritingTools(t *testing.T) {
	t.Parallel()

	const sample = `{"type":"assistant","provenance":"assistant_output","message":{"role":"model","parts":[
{"functionCall":{"id":"1","name":"write_file","args":{"file_path":"a.txt"}}},
{"functionCall":{"id":"2","name":"read_file","args":{"file_path":"b.txt"}}},
{"functionCall":{"id":"3","name":"replace","args":{"file_path":"c.txt"}}},
{"functionCall":{"id":"4","name":"run_shell_command","args":{"command":"ls"}}}]}}`

	files := ExtractModifiedFiles([]byte(strings.ReplaceAll(sample, "\n", "")))
	want := []string{"a.txt", "c.txt"}
	if len(files) != len(want) {
		t.Fatalf("files = %v, want %v (read_file and run_shell_command must not count)", files, want)
	}
	for i := range want {
		if files[i] != want[i] {
			t.Errorf("files[%d] = %q, want %q", i, files[i], want[i])
		}
	}
}

func TestExtractModel(t *testing.T) {
	t.Parallel()

	a := &QwenCodeAgent{}
	model, err := a.ExtractModel(readFixture(t))
	if err != nil {
		t.Fatalf("ExtractModel: %v", err)
	}
	if model == "" {
		t.Error("expected a model from the real fixture")
	}
}

func TestChunkAndReassemble_RoundTrip(t *testing.T) {
	t.Parallel()

	a := &QwenCodeAgent{}
	data := readFixture(t)

	chunks, err := a.ChunkTranscript(context.Background(), data, 1500)
	if err != nil {
		t.Fatalf("ChunkTranscript: %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	merged, err := a.ReassembleTranscript(chunks)
	if err != nil {
		t.Fatalf("ReassembleTranscript: %v", err)
	}
	if strings.TrimSpace(string(merged)) != strings.TrimSpace(string(data)) {
		t.Error("round trip did not reproduce the original transcript")
	}
}

func TestFormatResumeCommand(t *testing.T) {
	t.Parallel()

	a := &QwenCodeAgent{}
	if got := a.FormatResumeCommand("abc-123"); got != "qwen --resume abc-123" {
		t.Errorf("FormatResumeCommand = %q", got)
	}
	// Bare `qwen` would start a fresh session, so the empty case uses --continue.
	if got := a.FormatResumeCommand("  "); got != "qwen --continue" {
		t.Errorf("blank session = %q, want qwen --continue", got)
	}
}

func TestResolveSessionFile(t *testing.T) {
	t.Parallel()

	a := &QwenCodeAgent{}
	got := a.ResolveSessionFile("/chats", "abc")
	if got != filepath.Join("/chats", "abc.jsonl") {
		t.Errorf("ResolveSessionFile = %q", got)
	}
}

func TestWriteSession_RoundTripsThroughDisk(t *testing.T) {
	t.Parallel()

	a := &QwenCodeAgent{}
	dir := t.TempDir()
	ref := filepath.Join(dir, "chats", "abc.jsonl")
	data := readFixture(t)

	if err := a.WriteSession(context.Background(), &agent.AgentSession{
		SessionID: "abc", SessionRef: ref, NativeData: data,
	}); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}
	got, err := os.ReadFile(ref)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(data) {
		t.Error("WriteSession did not write the session verbatim")
	}
}

func TestWriteSession_Rejects(t *testing.T) {
	t.Parallel()

	a := &QwenCodeAgent{}
	ctx := context.Background()
	if err := a.WriteSession(ctx, nil); err == nil {
		t.Error("expected an error for a nil session")
	}
	if err := a.WriteSession(ctx, &agent.AgentSession{SessionRef: "/x"}); err == nil {
		t.Error("expected an error for empty data")
	}
	if err := a.WriteSession(ctx, &agent.AgentSession{NativeData: []byte("x")}); err == nil {
		t.Error("expected an error for a missing session ref")
	}
}

func TestGetTranscriptPosition_MissingFileIsZero(t *testing.T) {
	t.Parallel()

	a := &QwenCodeAgent{}
	got, err := a.GetTranscriptPosition(filepath.Join(t.TempDir(), "absent.jsonl"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if got != 0 {
		t.Errorf("position = %d, want 0", got)
	}
}

// A truncated trailing line is normal while a session is being written, so the
// reader must skip it rather than failing the whole walk.
func TestScanLines_SkipsMalformedLine(t *testing.T) {
	t.Parallel()

	data := []byte(`{"type":"user","provenance":"real_user","message":{"role":"user","parts":[{"text":"one"}]}}
{"type":"user","provenance":"real_user","message":{"role":"user","parts":[{"tex`)

	var texts []string
	total := scanLines(data, 0, func(line Line) {
		if line.Message != nil {
			for _, p := range line.Message.Parts {
				if p.Text != "" {
					texts = append(texts, p.Text)
				}
			}
		}
	})
	if total != 2 {
		t.Errorf("counted %d lines, want 2", total)
	}
	if len(texts) != 1 || texts[0] != "one" {
		t.Errorf("texts = %v, want [one]", texts)
	}
}
