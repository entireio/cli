package pi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Compile-time interface assertions
var (
	_ agent.Agent              = (*PiAgent)(nil)
	_ agent.HookSupport        = (*PiAgent)(nil)
	_ agent.TokenCalculator    = (*PiAgent)(nil)
	_ agent.TranscriptAnalyzer = (*PiAgent)(nil)
	_ agent.PromptExtractor    = (*PiAgent)(nil)
	_ agent.ModelExtractor     = (*PiAgent)(nil)
)

// testSessionJSONL — linear session: header + model_change + 4 messages.
const testSessionJSONL = `{"type":"session","version":3,"id":"test-uuid-123","timestamp":"2026-03-27T21:00:00.000Z","cwd":"/tmp/test"}
{"type":"model_change","id":"mc1","parentId":null,"timestamp":"2026-03-27T21:00:00.001Z","provider":"anthropic","modelId":"claude-sonnet-4-6"}
{"type":"message","id":"m1","parentId":"mc1","timestamp":"2026-03-27T21:00:01.000Z","message":{"role":"user","content":[{"type":"text","text":"Create hello.txt"}],"timestamp":1774646400000}}
{"type":"message","id":"m2","parentId":"m1","timestamp":"2026-03-27T21:00:02.000Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"tc1","name":"write","arguments":{"path":"hello.txt","content":"hello world\n"}}],"usage":{"input":100,"output":50,"cacheRead":10,"cacheWrite":5},"stopReason":"toolUse","timestamp":1774646401000}}
{"type":"message","id":"m3","parentId":"m2","timestamp":"2026-03-27T21:00:03.000Z","message":{"role":"toolResult","toolCallId":"tc1","toolName":"write","content":[{"type":"text","text":"Written 12 bytes"}],"isError":false,"timestamp":1774646402000}}
{"type":"message","id":"m4","parentId":"m3","timestamp":"2026-03-27T21:00:04.000Z","message":{"role":"assistant","content":[{"type":"text","text":"Created hello.txt with the content hello world."}],"usage":{"input":200,"output":30,"cacheRead":0,"cacheWrite":0},"stopReason":"stop","timestamp":1774646403000}}
`

// testBranchingSessionJSONL — two branches from m1; m5 (active) wins because it's the last message.
const testBranchingSessionJSONL = `{"type":"session","version":3,"id":"test-branch-123","timestamp":"2026-03-27T22:00:00.000Z","cwd":"/tmp/test"}
{"type":"model_change","id":"mc1","parentId":null,"timestamp":"2026-03-27T22:00:00.001Z","provider":"anthropic","modelId":"claude-sonnet-4-6"}
{"type":"message","id":"m1","parentId":"mc1","timestamp":"2026-03-27T22:00:01.000Z","message":{"role":"user","content":[{"type":"text","text":"Create a file"}],"timestamp":1774650000000}}
{"type":"message","id":"m2","parentId":"m1","timestamp":"2026-03-27T22:00:02.000Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"tc1","name":"write","arguments":{"path":"old.txt","content":"old\n"}}],"usage":{"input":100,"output":50,"cacheRead":0,"cacheWrite":0},"stopReason":"toolUse","timestamp":1774650001000}}
{"type":"message","id":"m3","parentId":"m2","timestamp":"2026-03-27T22:00:03.000Z","message":{"role":"toolResult","toolCallId":"tc1","toolName":"write","content":[{"type":"text","text":"Written 4 bytes"}],"isError":false,"timestamp":1774650002000}}
{"type":"message","id":"m4","parentId":"m3","timestamp":"2026-03-27T22:00:04.000Z","message":{"role":"assistant","content":[{"type":"text","text":"Created old.txt"}],"usage":{"input":200,"output":30,"cacheRead":0,"cacheWrite":0},"stopReason":"stop","timestamp":1774650003000}}
{"type":"message","id":"m5","parentId":"m1","timestamp":"2026-03-27T22:00:05.000Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"tc2","name":"write","arguments":{"path":"new.txt","content":"new\n"}}],"usage":{"input":150,"output":60,"cacheRead":5,"cacheWrite":3},"stopReason":"toolUse","timestamp":1774650004000}}
{"type":"message","id":"m6","parentId":"m5","timestamp":"2026-03-27T22:00:06.000Z","message":{"role":"toolResult","toolCallId":"tc2","toolName":"write","content":[{"type":"text","text":"Written 4 bytes"}],"isError":false,"timestamp":1774650005000}}
{"type":"message","id":"m7","parentId":"m6","timestamp":"2026-03-27T22:00:07.000Z","message":{"role":"assistant","content":[{"type":"text","text":"Created new.txt"}],"usage":{"input":250,"output":40,"cacheRead":0,"cacheWrite":0},"stopReason":"stop","timestamp":1774650006000}}
`

// testFlatSessionJSONL — no parentId references, flat structure.
const testFlatSessionJSONL = `{"type":"session","id":"flat-123"}
{"type":"message","id":"m1","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}
{"type":"message","id":"m2","message":{"role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input":10,"output":5,"cacheRead":0,"cacheWrite":0}}}
`

// testModelSessionJSONL — assistant messages carry message.model (the real Pi
// shape: every assistant message records the model that produced it).
const testModelSessionJSONL = `{"type":"session","version":3,"id":"model-uuid","timestamp":"2026-05-22T21:00:00.000Z","cwd":"/tmp/test"}
{"type":"message","id":"m1","parentId":null,"timestamp":"2026-05-22T21:00:01.000Z","message":{"role":"user","content":[{"type":"text","text":"Hi"}]}}
{"type":"message","id":"m2","parentId":"m1","timestamp":"2026-05-22T21:00:02.000Z","message":{"role":"assistant","content":[{"type":"text","text":"Hello"}],"model":"gpt-5.5","provider":"openai-codex","usage":{"input":100,"output":50,"cacheRead":0,"cacheWrite":0}}}
{"type":"message","id":"m3","parentId":"m2","timestamp":"2026-05-22T21:00:03.000Z","message":{"role":"assistant","content":[{"type":"text","text":"Done"}],"model":"gpt-5.5","provider":"openai-codex","usage":{"input":120,"output":40,"cacheRead":0,"cacheWrite":0}}}
`

// testModelChangeSessionJSONL — model switches mid-session; the most recent
// active-branch assistant message wins.
const testModelChangeSessionJSONL = `{"type":"session","version":3,"id":"model-change-uuid","timestamp":"2026-05-22T22:00:00.000Z","cwd":"/tmp/test"}
{"type":"message","id":"m1","parentId":null,"timestamp":"2026-05-22T22:00:01.000Z","message":{"role":"user","content":[{"type":"text","text":"Hi"}]}}
{"type":"message","id":"m2","parentId":"m1","timestamp":"2026-05-22T22:00:02.000Z","message":{"role":"assistant","content":[{"type":"text","text":"Hello"}],"model":"gpt-5.5","provider":"openai-codex","usage":{"input":100,"output":50,"cacheRead":0,"cacheWrite":0}}}
{"type":"message","id":"m3","parentId":"m2","timestamp":"2026-05-22T22:00:03.000Z","message":{"role":"assistant","content":[{"type":"text","text":"Switched"}],"model":"claude-sonnet-4-6","provider":"anthropic","usage":{"input":120,"output":40,"cacheRead":0,"cacheWrite":0}}}
`

// testModelBranchingJSONL — abandoned branch (m4) uses a different model than
// the active branch (m5); only the active-branch model should be returned.
const testModelBranchingJSONL = `{"type":"session","version":3,"id":"model-branch-uuid","timestamp":"2026-05-22T23:00:00.000Z","cwd":"/tmp/test"}
{"type":"message","id":"m1","parentId":null,"timestamp":"2026-05-22T23:00:01.000Z","message":{"role":"user","content":[{"type":"text","text":"Hi"}]}}
{"type":"message","id":"m4","parentId":"m1","timestamp":"2026-05-22T23:00:02.000Z","message":{"role":"assistant","content":[{"type":"text","text":"abandoned"}],"model":"claude-opus-4-8","provider":"anthropic","usage":{"input":100,"output":50,"cacheRead":0,"cacheWrite":0}}}
{"type":"message","id":"m5","parentId":"m1","timestamp":"2026-05-22T23:00:03.000Z","message":{"role":"assistant","content":[{"type":"text","text":"active"}],"model":"gpt-5.5","provider":"openai-codex","usage":{"input":120,"output":40,"cacheRead":0,"cacheWrite":0}}}
`

func writeJSONL(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeTestSession(t *testing.T) string {
	t.Helper()
	return writeJSONL(t, "2026-03-27T21-00-00-000Z_test-uuid-123.jsonl", testSessionJSONL)
}

func writeBranchingSession(t *testing.T) string {
	t.Helper()
	return writeJSONL(t, "2026-03-27T22-00-00-000Z_test-branch-123.jsonl", testBranchingSessionJSONL)
}

// --- ExtractModifiedFilesFromOffset ---

func TestExtractModifiedFiles(t *testing.T) {
	t.Parallel()
	path := writeTestSession(t)
	files, pos, err := (&PiAgent{}).ExtractModifiedFilesFromOffset(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != "hello.txt" {
		t.Errorf("files = %v, want [hello.txt]", files)
	}
	if pos == 0 {
		t.Error("position should be > 0")
	}
}

func TestExtractModifiedFiles_OffsetPastEnd(t *testing.T) {
	t.Parallel()
	path := writeTestSession(t)
	files, _, err := (&PiAgent{}).ExtractModifiedFilesFromOffset(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Errorf("files = %v, want empty", files)
	}
}

func TestExtractModifiedFiles_Branching(t *testing.T) {
	t.Parallel()
	path := writeBranchingSession(t)
	files, _, err := (&PiAgent{}).ExtractModifiedFilesFromOffset(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != "new.txt" {
		t.Errorf("files = %v, want [new.txt] (only active branch)", files)
	}
}

// --- ExtractPrompts ---

func TestExtractPrompts(t *testing.T) {
	t.Parallel()
	path := writeTestSession(t)
	prompts, err := (&PiAgent{}).ExtractPrompts(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 1 || prompts[0] != "Create hello.txt" {
		t.Errorf("prompts = %v, want [Create hello.txt]", prompts)
	}
}

func TestExtractPrompts_Branching(t *testing.T) {
	t.Parallel()
	path := writeBranchingSession(t)
	prompts, err := (&PiAgent{}).ExtractPrompts(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 1 || prompts[0] != "Create a file" {
		t.Errorf("prompts = %v, want [Create a file]", prompts)
	}
}

// --- GetTranscriptPosition ---

func TestGetTranscriptPosition(t *testing.T) {
	t.Parallel()
	path := writeTestSession(t)
	pos, err := (&PiAgent{}).GetTranscriptPosition(path)
	if err != nil {
		t.Fatal(err)
	}
	if pos != 6 {
		t.Errorf("position = %d, want 6", pos)
	}
}

func TestGetTranscriptPosition_Missing(t *testing.T) {
	t.Parallel()
	pos, err := (&PiAgent{}).GetTranscriptPosition("/nonexistent/file.jsonl")
	if err != nil {
		t.Fatalf("expected no error for missing file, got: %v", err)
	}
	if pos != 0 {
		t.Errorf("position = %d, want 0", pos)
	}
}

// --- CalculateTokenUsage ---

func TestCalculateTokenUsage(t *testing.T) {
	t.Parallel()
	usage, err := (&PiAgent{}).CalculateTokenUsage([]byte(testSessionJSONL), 0)
	if err != nil {
		t.Fatal(err)
	}
	if usage.InputTokens != 300 {
		t.Errorf("InputTokens = %d, want 300", usage.InputTokens)
	}
	if usage.OutputTokens != 80 {
		t.Errorf("OutputTokens = %d, want 80", usage.OutputTokens)
	}
	if usage.CacheReadTokens != 10 {
		t.Errorf("CacheReadTokens = %d, want 10", usage.CacheReadTokens)
	}
	if usage.CacheCreationTokens != 5 {
		t.Errorf("CacheCreationTokens = %d, want 5", usage.CacheCreationTokens)
	}
	if usage.APICallCount != 2 {
		t.Errorf("APICallCount = %d, want 2", usage.APICallCount)
	}
}

func TestCalculateTokenUsage_Branching(t *testing.T) {
	t.Parallel()
	usage, err := (&PiAgent{}).CalculateTokenUsage([]byte(testBranchingSessionJSONL), 0)
	if err != nil {
		t.Fatal(err)
	}
	// Active branch: m5(input=150,output=60,cacheRead=5,cacheWrite=3)
	//                m7(input=250,output=40,cacheRead=0,cacheWrite=0)
	if usage.InputTokens != 400 {
		t.Errorf("InputTokens = %d, want 400", usage.InputTokens)
	}
	if usage.OutputTokens != 100 {
		t.Errorf("OutputTokens = %d, want 100", usage.OutputTokens)
	}
	if usage.APICallCount != 2 {
		t.Errorf("APICallCount = %d, want 2", usage.APICallCount)
	}
}

func TestCalculateTokenUsage_OffsetPastEnd(t *testing.T) {
	t.Parallel()
	usage, err := (&PiAgent{}).CalculateTokenUsage([]byte(testSessionJSONL), 100)
	if err != nil {
		t.Fatal(err)
	}
	if usage.APICallCount != 0 {
		t.Errorf("expected 0 API calls past end, got %+v", usage)
	}
}

func TestCalculateTokenUsage_FlatTranscript(t *testing.T) {
	t.Parallel()
	usage, err := (&PiAgent{}).CalculateTokenUsage([]byte(testFlatSessionJSONL), 0)
	if err != nil {
		t.Fatal(err)
	}
	if usage.InputTokens != 10 || usage.OutputTokens != 5 || usage.APICallCount != 1 {
		t.Errorf("flat: got %+v, want input=10 output=5 calls=1", usage)
	}
}

// Note: pijsonl.ResolveActiveBranch unit tests live in the pijsonl package
// itself; the in-tree tests here verify the agent surface (CalculateTokenUsage,
// ExtractModifiedFilesFromOffset, ExtractPrompts) honours active-branch
// filtering end-to-end.

// --- ExtractModel ---

func TestExtractModel(t *testing.T) {
	t.Parallel()
	model, err := (&PiAgent{}).ExtractModel([]byte(testModelSessionJSONL))
	if err != nil {
		t.Fatal(err)
	}
	if model != "gpt-5.5" {
		t.Errorf("model = %q, want gpt-5.5", model)
	}
}

func TestExtractModel_MostRecentWinsOnModelChange(t *testing.T) {
	t.Parallel()
	model, err := (&PiAgent{}).ExtractModel([]byte(testModelChangeSessionJSONL))
	if err != nil {
		t.Fatal(err)
	}
	if model != "claude-sonnet-4-6" {
		t.Errorf("model = %q, want claude-sonnet-4-6 (most recent)", model)
	}
}

func TestExtractModel_Branching(t *testing.T) {
	t.Parallel()
	model, err := (&PiAgent{}).ExtractModel([]byte(testModelBranchingJSONL))
	if err != nil {
		t.Fatal(err)
	}
	if model != "gpt-5.5" {
		t.Errorf("model = %q, want gpt-5.5 (active branch only)", model)
	}
}

func TestExtractModel_Empty(t *testing.T) {
	t.Parallel()
	model, err := (&PiAgent{}).ExtractModel(nil)
	if err != nil {
		t.Fatal(err)
	}
	if model != "" {
		t.Errorf("model = %q, want empty", model)
	}
}

func TestExtractModel_NoModelField(t *testing.T) {
	t.Parallel()
	// testSessionJSONL records the model only on the model_change entry, not on
	// message.model, so ExtractModel finds nothing.
	model, err := (&PiAgent{}).ExtractModel([]byte(testSessionJSONL))
	if err != nil {
		t.Fatal(err)
	}
	if model != "" {
		t.Errorf("model = %q, want empty (no message.model present)", model)
	}
}

// --- ReadSession / WriteSession ---

func TestReadSession(t *testing.T) {
	t.Parallel()
	path := writeTestSession(t)
	s, err := (&PiAgent{}).ReadSession(&agent.HookInput{
		SessionID:  "test-uuid-123",
		SessionRef: path,
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.SessionID != "test-uuid-123" {
		t.Errorf("SessionID = %q", s.SessionID)
	}
	if s.AgentName != agent.AgentNamePi {
		t.Errorf("AgentName = %q", s.AgentName)
	}
	if len(s.NativeData) == 0 {
		t.Error("NativeData should not be empty")
	}
}

func TestWriteSession_RoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "session.json")
	body := []byte(`{"type":"session","version":3}` + "\n")
	err := (&PiAgent{}).WriteSession(context.Background(), &agent.AgentSession{
		SessionID:  "abc",
		SessionRef: path,
		NativeData: body,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Errorf("written body mismatch")
	}
}

// --- ChunkTranscript / ReassembleTranscript ---

func TestChunkAndReassemble(t *testing.T) {
	t.Parallel()
	body := []byte(strings.Repeat(`{"type":"message"}`+"\n", 50))
	chunks, err := (&PiAgent{}).ChunkTranscript(context.Background(), body, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 2 {
		t.Errorf("expected multiple chunks, got %d", len(chunks))
	}
	reassembled, err := (&PiAgent{}).ReassembleTranscript(chunks)
	if err != nil {
		t.Fatal(err)
	}
	if string(reassembled) != string(body) {
		t.Error("reassembled bytes differ from original")
	}
}

// --- format/identity ---

func TestSelfRegistered(t *testing.T) {
	t.Parallel()
	a, err := agent.Get(agent.AgentNamePi)
	if err != nil {
		t.Fatalf("agent.Get(pi): %v", err)
	}
	if a.Name() != agent.AgentNamePi {
		t.Errorf("Name() = %q", a.Name())
	}
	if a.Type() != agent.AgentTypePi {
		t.Errorf("Type() = %q", a.Type())
	}
}

func TestProtectedDirsContainsDotPi(t *testing.T) {
	t.Parallel()
	dirs := (&PiAgent{}).ProtectedDirs()
	for _, d := range dirs {
		if d == ".pi" {
			return
		}
	}
	t.Errorf(".pi missing from ProtectedDirs: %v", dirs)
}

func TestFormatResumeCommand(t *testing.T) {
	t.Parallel()
	a := &PiAgent{}
	const piContinue = "pi --continue"
	if got := a.FormatResumeCommand(""); got != piContinue {
		t.Errorf("FormatResumeCommand(empty) = %q, want %q", got, piContinue)
	}
	if got := a.FormatResumeCommand("  "); got != piContinue {
		t.Errorf("FormatResumeCommand(whitespace) = %q, want %q", got, piContinue)
	}
	if got, want := a.FormatResumeCommand("abc-123"), "pi --session abc-123"; got != want {
		t.Errorf("FormatResumeCommand(id) = %q, want %q", got, want)
	}
}

// --- DetectPresence ---

func TestDetectPresence_NoPiDir(t *testing.T) {
	// Cannot use t.Parallel — t.Chdir mutates process state.
	t.Chdir(t.TempDir())
	present, err := (&PiAgent{}).DetectPresence(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if present {
		t.Error("DetectPresence should be false when no .pi/ directory exists")
	}
}

func TestDetectPresence_WithPiDir(t *testing.T) {
	// Cannot use t.Parallel — t.Chdir mutates process state.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".pi"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	present, err := (&PiAgent{}).DetectPresence(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !present {
		t.Error("DetectPresence should be true when .pi/ exists in repo")
	}
}
