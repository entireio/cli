package codexcli

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestParseTranscript(t *testing.T) {
	t.Parallel()

	data := makeTranscriptJSONL(t,
		TranscriptLine{Timestamp: "2024-01-01T00:00:00Z", Type: eventTypeSessionMeta, Payload: mustMarshalRaw(t, sessionMetaPayload{ID: "sess-1", CWD: "/tmp"})},
		TranscriptLine{Timestamp: "2024-01-01T00:01:00Z", Type: eventTypeEventMsg, Payload: mustMarshalRaw(t, eventMsgPayload{Type: eventMsgUserMessage, Message: "hello"})},
	)

	lines, err := ParseTranscript(data)
	if err != nil {
		t.Fatalf("ParseTranscript() error = %v", err)
	}

	if len(lines) != 2 {
		t.Errorf("ParseTranscript() got %d lines, want 2", len(lines))
	}

	if lines[0].Type != eventTypeSessionMeta {
		t.Errorf("First line type = %q, want %q", lines[0].Type, eventTypeSessionMeta)
	}
	if lines[1].Type != eventTypeEventMsg {
		t.Errorf("Second line type = %q, want %q", lines[1].Type, eventTypeEventMsg)
	}
}

func TestParseTranscript_SkipsMalformed(t *testing.T) {
	t.Parallel()

	data := []byte(`{"timestamp":"t1","type":"session_meta","payload":{}}
not valid json
{"timestamp":"t2","type":"event_msg","payload":{}}
`)

	lines, err := ParseTranscript(data)
	if err != nil {
		t.Fatalf("ParseTranscript() error = %v", err)
	}

	if len(lines) != 2 {
		t.Errorf("ParseTranscript() got %d lines, want 2 (skipping malformed)", len(lines))
	}
}

func TestSerializeTranscript(t *testing.T) {
	t.Parallel()

	lines := []TranscriptLine{
		{Timestamp: "t1", Type: eventTypeSessionMeta},
		{Timestamp: "t2", Type: eventTypeEventMsg},
	}

	data, err := SerializeTranscript(lines)
	if err != nil {
		t.Fatalf("SerializeTranscript() error = %v", err)
	}

	parsed, err := ParseTranscript(data)
	if err != nil {
		t.Fatalf("ParseTranscript(serialized) error = %v", err)
	}

	if len(parsed) != 2 {
		t.Errorf("Round-trip got %d lines, want 2", len(parsed))
	}
}

func TestExtractModifiedFiles(t *testing.T) {
	t.Parallel()

	lines := []TranscriptLine{
		makeExecCommandItem(t, `{"cmd":"apply_patch <<'EOF'\n--- a/foo.go\n+++ b/foo.go\nEOF"}`),
		makeExecCommandItem(t, `{"cmd":"cat > bar.txt"}`),
		makeExecCommandItem(t, `{"cmd":"ls -la"}`),                                               // not file-modifying
		makeExecCommandItem(t, `{"cmd":"apply_patch <<'EOF'\n--- a/foo.go\n+++ b/foo.go\nEOF"}`), // duplicate
	}

	files := ExtractModifiedFiles(lines)

	hasFile := func(name string) bool {
		for _, f := range files {
			if f == name {
				return true
			}
		}
		return false
	}

	if !hasFile("foo.go") {
		t.Error("ExtractModifiedFiles() missing foo.go from apply_patch")
	}
	if !hasFile("bar.txt") {
		t.Error("ExtractModifiedFiles() missing bar.txt from cat >")
	}
}

func TestExtractModifiedFiles_SedCommand(t *testing.T) {
	t.Parallel()

	lines := []TranscriptLine{
		makeExecCommandItem(t, `{"cmd":"sed -i 's/old/new/g' config.yaml"}`),
	}

	files := ExtractModifiedFiles(lines)

	if len(files) != 1 || files[0] != "config.yaml" {
		t.Errorf("ExtractModifiedFiles() = %v, want [config.yaml]", files)
	}
}

func TestExtractLastUserPrompt(t *testing.T) {
	t.Parallel()

	lines := []TranscriptLine{
		makeEventMsg(t, eventMsgUserMessage, "first prompt"),
		makeEventMsg(t, eventMsgAgentMessage, "response 1"),
		makeEventMsg(t, eventMsgUserMessage, "second prompt"),
	}

	got := ExtractLastUserPrompt(lines)
	if got != "second prompt" {
		t.Errorf("ExtractLastUserPrompt() = %q, want %q", got, "second prompt")
	}
}

func TestExtractLastUserPrompt_Empty(t *testing.T) {
	t.Parallel()

	got := ExtractLastUserPrompt(nil)
	if got != "" {
		t.Errorf("ExtractLastUserPrompt(nil) = %q, want empty", got)
	}
}

func TestExtractAllUserPrompts(t *testing.T) {
	t.Parallel()

	lines := []TranscriptLine{
		makeEventMsg(t, eventMsgUserMessage, "prompt 1"),
		makeEventMsg(t, eventMsgAgentMessage, "response"),
		makeEventMsg(t, eventMsgUserMessage, "prompt 2"),
		makeEventMsg(t, eventMsgTokenCount, ""),
	}

	prompts := ExtractAllUserPrompts(lines)
	if len(prompts) != 2 {
		t.Fatalf("ExtractAllUserPrompts() got %d prompts, want 2", len(prompts))
	}
	if prompts[0] != "prompt 1" || prompts[1] != "prompt 2" {
		t.Errorf("ExtractAllUserPrompts() = %v, want [prompt 1, prompt 2]", prompts)
	}
}

func TestExtractLastAssistantMessage(t *testing.T) {
	t.Parallel()

	lines := []TranscriptLine{
		makeEventMsg(t, eventMsgAgentMessage, "first response"),
		makeEventMsg(t, eventMsgAgentMessage, "second response"),
	}

	got := ExtractLastAssistantMessage(lines)
	if got != "second response" {
		t.Errorf("ExtractLastAssistantMessage() = %q, want %q", got, "second response")
	}
}

func TestExtractSessionID(t *testing.T) {
	t.Parallel()

	lines := []TranscriptLine{
		{Type: eventTypeSessionMeta, Payload: mustMarshalRaw(t, sessionMetaPayload{ID: "sess-abc-123"})},
		makeEventMsg(t, eventMsgUserMessage, "hello"),
	}

	got := ExtractSessionID(lines)
	if got != "sess-abc-123" {
		t.Errorf("ExtractSessionID() = %q, want %q", got, "sess-abc-123")
	}
}

func TestExtractSessionCWD(t *testing.T) {
	t.Parallel()

	lines := []TranscriptLine{
		{Type: eventTypeSessionMeta, Payload: mustMarshalRaw(t, sessionMetaPayload{CWD: "/home/user/project"})},
	}

	got := ExtractSessionCWD(lines)
	if got != "/home/user/project" {
		t.Errorf("ExtractSessionCWD() = %q, want %q", got, "/home/user/project")
	}
}

func TestCalculateTokenUsage(t *testing.T) {
	t.Parallel()

	lines := []TranscriptLine{
		makeTokenCountEvent(t, 100, 50, 200, 20),
		makeTokenCountEvent(t, 300, 100, 500, 50), // cumulative — last one wins
		makeEventMsg(t, eventMsgTaskComplete, ""),
	}

	usage := CalculateTokenUsage(lines)

	if usage.InputTokens != 300 {
		t.Errorf("InputTokens = %d, want 300", usage.InputTokens)
	}
	if usage.CacheReadTokens != 100 {
		t.Errorf("CacheReadTokens = %d, want 100", usage.CacheReadTokens)
	}
	if usage.OutputTokens != 500 {
		t.Errorf("OutputTokens = %d, want 500", usage.OutputTokens)
	}
	if usage.APICallCount != 1 {
		t.Errorf("APICallCount = %d, want 1", usage.APICallCount)
	}
}

func TestCalculateTokenUsage_Empty(t *testing.T) {
	t.Parallel()

	usage := CalculateTokenUsage(nil)
	if usage.InputTokens != 0 || usage.OutputTokens != 0 || usage.APICallCount != 0 {
		t.Errorf("empty transcript should return zero usage, got %+v", usage)
	}
}

func TestCalculateTokenUsageFromFile(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	path := tmpDir + "/transcript.jsonl"

	data := makeTranscriptJSONL(t,
		makeTokenCountEvent(t, 100, 0, 50, 10),
		makeEventMsg(t, eventMsgTaskComplete, ""),
	)

	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	usage, err := CalculateTokenUsageFromFile(path, 0)
	if err != nil {
		t.Fatalf("CalculateTokenUsageFromFile() error = %v", err)
	}

	if usage.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", usage.InputTokens)
	}
	if usage.APICallCount != 1 {
		t.Errorf("APICallCount = %d, want 1", usage.APICallCount)
	}
}

func TestCalculateTokenUsageFromFile_EmptyPath(t *testing.T) {
	t.Parallel()

	usage, err := CalculateTokenUsageFromFile("", 0)
	if err != nil {
		t.Fatalf("CalculateTokenUsageFromFile() error = %v", err)
	}
	if usage.InputTokens != 0 {
		t.Errorf("InputTokens = %d, want 0", usage.InputTokens)
	}
}

func TestIsFileModifyingCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		cmd  string
		want bool
	}{
		{"apply_patch <<'EOF'", true},
		{"cat > file.txt", true},
		{"cat >> file.txt", true},
		{"tee output.log", true},
		{"sed -i 's/a/b/' file.go", true},
		{"ls -la", false},
		{"git status", false},
		{"grep pattern file", false},
		{"echo hello > file.txt", true},
		{"mv old.txt new.txt", true},
		{"cp src.txt dst.txt", true},
		{"mkdir -p dir/subdir", true},
		{"touch newfile.txt", true},
		{"rm old.txt", true},
	}

	for _, tt := range tests {
		t.Run(tt.cmd, func(t *testing.T) {
			t.Parallel()
			if got := isFileModifyingCommand(tt.cmd); got != tt.want {
				t.Errorf("isFileModifyingCommand(%q) = %v, want %v", tt.cmd, got, tt.want)
			}
		})
	}
}

func TestExtractFilesFromCommand_ApplyPatch(t *testing.T) {
	t.Parallel()

	cmd := "apply_patch <<'EOF'\n--- a/src/main.go\n+++ b/src/main.go\n@@ -1,3 +1,4 @@\n package main\n+import \"fmt\"\nEOF"
	files := extractFilesFromCommand(cmd, "")

	// Both --- a/ and +++ b/ lines produce paths; dedup happens in ExtractModifiedFiles
	if len(files) == 0 {
		t.Fatal("extractFilesFromCommand() returned no files")
	}
	found := false
	for _, f := range files {
		if f == "src/main.go" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("extractFilesFromCommand() = %v, want to contain src/main.go", files)
	}
}

func TestExtractFilesFromCommand_CatRedirect(t *testing.T) {
	t.Parallel()

	cmd := "cat > output.txt"
	files := extractFilesFromCommand(cmd, "")

	if len(files) != 1 || files[0] != "output.txt" {
		t.Errorf("extractFilesFromCommand(%q) = %v, want [output.txt]", cmd, files)
	}
}

func TestExtractFilesFromCommand_SedInPlace(t *testing.T) {
	t.Parallel()

	cmd := "sed -i 's/old/new/g' config.yaml"
	files := extractFilesFromCommand(cmd, "")

	if len(files) != 1 || files[0] != "config.yaml" {
		t.Errorf("extractFilesFromCommand(%q) = %v, want [config.yaml]", cmd, files)
	}
}

func TestExtractFilesFromCommand_ApplyPatchWithWorkdir(t *testing.T) {
	t.Parallel()

	cmd := "apply_patch <<'EOF'\n--- a/file.go\n+++ b/file.go\nEOF"
	files := extractFilesFromCommand(cmd, "/home/user/project")

	// Both --- a/ and +++ b/ lines produce paths; dedup happens in ExtractModifiedFiles
	if len(files) == 0 {
		t.Fatal("extractFilesFromCommand() returned no files")
	}
	found := false
	for _, f := range files {
		if f == "/home/user/project/file.go" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("extractFilesFromCommand() = %v, want to contain /home/user/project/file.go", files)
	}
}

// Helper functions

func mustMarshalRaw(t *testing.T, v interface{}) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}
	return data
}

func makeTranscriptJSONL(t *testing.T, lines ...TranscriptLine) []byte {
	t.Helper()
	data, err := SerializeTranscript(lines)
	if err != nil {
		t.Fatalf("SerializeTranscript() error = %v", err)
	}
	return data
}

func makeEventMsg(t *testing.T, msgType, message string) TranscriptLine {
	t.Helper()
	return TranscriptLine{
		Type: eventTypeEventMsg,
		Payload: mustMarshalRaw(t, eventMsgPayload{
			Type:    msgType,
			Message: message,
		}),
	}
}

func makeExecCommandItem(t *testing.T, args string) TranscriptLine {
	t.Helper()
	return TranscriptLine{
		Type: eventTypeResponseItem,
		Payload: mustMarshalRaw(t, responseItemPayload{
			Type:      responseItemFunctionCall,
			Name:      "exec_command",
			Arguments: args,
		}),
	}
}

func makeTokenCountEvent(t *testing.T, input, cached, output, reasoning int) TranscriptLine {
	t.Helper()
	info := tokenCountInfo{}
	info.TotalTokenUsage.InputTokens = input
	info.TotalTokenUsage.CachedInputTokens = cached
	info.TotalTokenUsage.OutputTokens = output
	info.TotalTokenUsage.ReasoningOutputTokens = reasoning

	return TranscriptLine{
		Type: eventTypeEventMsg,
		Payload: mustMarshalRaw(t, eventMsgPayload{
			Type: eventMsgTokenCount,
			Info: mustMarshalRaw(t, info),
		}),
	}
}

// Ensure the TokenUsage type is correctly wired
func TestTokenUsageType(t *testing.T) {
	t.Parallel()
	usage := &agent.TokenUsage{
		InputTokens:  100,
		OutputTokens: 50,
		APICallCount: 1,
	}
	if usage.InputTokens != 100 {
		t.Error("TokenUsage.InputTokens should be 100")
	}
	if usage.OutputTokens != 50 {
		t.Error("TokenUsage.OutputTokens should be 50")
	}
	if usage.APICallCount != 1 {
		t.Error("TokenUsage.APICallCount should be 1")
	}
}
