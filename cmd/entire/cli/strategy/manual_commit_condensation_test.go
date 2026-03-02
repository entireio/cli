package strategy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"

	// Register agents so GetByAgentType works in tests.
	_ "github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/cursor"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/factoryaidroid"
)

// calculateTokenUsage is a test helper that looks up the Factory AI Droid agent
// and calculates token usage from pre-loaded transcript bytes.
func calculateTokenUsage(_ types.AgentType, data []byte, offset int) *agent.TokenUsage {
	ag, err := agent.GetByAgentType(agent.AgentTypeFactoryAIDroid)
	if err != nil {
		return nil
	}
	return agent.CalculateTokenUsage(context.Background(), ag, data, offset, "")
}

func TestCalculateTokenUsage_CursorReturnsNil(t *testing.T) {
	t.Parallel()

	// Cursor transcripts don't contain token usage data, so CalculateTokenUsage
	// should return nil (not an empty struct) to signal "no data available".
	transcript := []byte(`{"role":"user","message":{"content":[{"type":"text","text":"hello"}]}}`)

	ag, err := agent.GetByAgentType(agent.AgentTypeCursor)
	if err != nil {
		t.Fatalf("GetByAgentType(Cursor) error: %v", err)
	}
	result := agent.CalculateTokenUsage(context.Background(), ag, transcript, 0, "")
	if result != nil {
		t.Errorf("CalculateTokenUsage(Cursor) = %+v, want nil", result)
	}
}

func TestCalculateTokenUsage_EmptyData(t *testing.T) {
	t.Parallel()

	ag, err := agent.GetByAgentType(agent.AgentTypeClaudeCode)
	if err != nil {
		t.Fatalf("GetByAgentType(ClaudeCode) error: %v", err)
	}
	result := agent.CalculateTokenUsage(context.Background(), ag, nil, 0, "")
	if result == nil {
		t.Fatal("CalculateTokenUsage(empty) = nil, want non-nil empty struct")
	}
	if result.InputTokens != 0 || result.OutputTokens != 0 {
		t.Errorf("expected zero tokens for empty data, got %+v", result)
	}
}

func TestCalculateTokenUsage_ClaudeCodeBasic(t *testing.T) {
	t.Parallel()

	// Claude Code JSONL: "usage" with "id" lives inside the "message" JSON object
	lines := []string{
		`{"type":"human","uuid":"u1","message":{"content":"hello"}}`,
		`{"type":"assistant","uuid":"u2","message":{"id":"msg_001","usage":{"input_tokens":10,"output_tokens":5}}}`,
	}
	data := []byte(strings.Join(lines, "\n") + "\n")

	ag, err := agent.GetByAgentType(agent.AgentTypeClaudeCode)
	if err != nil {
		t.Fatalf("GetByAgentType(ClaudeCode) error: %v", err)
	}
	result := agent.CalculateTokenUsage(context.Background(), ag, data, 0, "")
	if result == nil {
		t.Fatal("CalculateTokenUsage(ClaudeCode) = nil, want non-nil")
	}
	if result.OutputTokens != 5 {
		t.Errorf("OutputTokens = %d, want 5", result.OutputTokens)
	}
	if result.APICallCount != 1 {
		t.Errorf("APICallCount = %d, want 1", result.APICallCount)
	}
}

func TestCalculateTokenUsage_ClaudeCodeWithOffset(t *testing.T) {
	t.Parallel()

	// 4-line transcript; start at offset 2 to only count the second pair
	lines := []string{
		`{"type":"human","uuid":"u1","message":{"content":"first"}}`,
		`{"type":"assistant","uuid":"u2","message":{"id":"msg_001","usage":{"input_tokens":10,"output_tokens":5}}}`,
		`{"type":"human","uuid":"u3","message":{"content":"second"}}`,
		`{"type":"assistant","uuid":"u4","message":{"id":"msg_002","usage":{"input_tokens":20,"output_tokens":15}}}`,
	}
	data := []byte(strings.Join(lines, "\n") + "\n")

	ag, err := agent.GetByAgentType(agent.AgentTypeClaudeCode)
	if err != nil {
		t.Fatalf("GetByAgentType(ClaudeCode) error: %v", err)
	}
	full := agent.CalculateTokenUsage(context.Background(), ag, data, 0, "")
	sliced := agent.CalculateTokenUsage(context.Background(), ag, data, 2, "")

	if full == nil || sliced == nil {
		t.Fatal("expected non-nil results")
	}
	if full.OutputTokens != 20 {
		t.Errorf("full OutputTokens = %d, want 20", full.OutputTokens)
	}
	if sliced.OutputTokens != 15 {
		t.Errorf("sliced OutputTokens = %d, want 15", sliced.OutputTokens)
	}
}

// cursorSampleTranscript is a subset of a real Cursor session transcript.
// Cursor uses "role" (not "type") and wraps user text in <user_query> tags.
var cursorSampleTranscript = strings.Join([]string{
	`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\ncreate a file with contents 'a' and commit, then create another file with contents 'b' and commit\n</user_query>"}]}}`,
	`{"role":"assistant","message":{"content":[{"type":"text","text":"Creating two files (contents 'a' and 'b') and committing each."}]}}`,
	`{"role":"assistant","message":{"content":[{"type":"text","text":"Both files are tracked and the working tree is clean."}]}}`,
	`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\ncreate a file with contents 'c' and commit\n</user_query>"}]}}`,
	`{"role":"assistant","message":{"content":[{"type":"text","text":"Created c.txt with contents c and committed it."}]}}`,
	`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nadd a file called bingo and commit\n</user_query>"}]}}`,
	`{"role":"assistant","message":{"content":[{"type":"text","text":"Created bingo and committed it."}]}}`,
}, "\n") + "\n"

func TestCountTranscriptItems_Cursor(t *testing.T) {
	t.Parallel()

	count := countTranscriptItems(agent.AgentTypeCursor, cursorSampleTranscript)
	if count != 7 {
		t.Errorf("countTranscriptItems(Cursor) = %d, want 7", count)
	}
}

func TestCountTranscriptItems_CursorEmpty(t *testing.T) {
	t.Parallel()

	count := countTranscriptItems(agent.AgentTypeCursor, "")
	if count != 0 {
		t.Errorf("countTranscriptItems(Cursor, empty) = %d, want 0", count)
	}
}

func TestExtractUserPrompts_Cursor(t *testing.T) {
	t.Parallel()

	// Cursor uses "role":"user" instead of "type":"human". extractUserPromptsFromLines
	// handles both via the "role" fallback.
	prompts := extractUserPrompts(agent.AgentTypeCursor, cursorSampleTranscript)
	if len(prompts) != 3 {
		t.Fatalf("extractUserPrompts(Cursor) returned %d prompts, want 3", len(prompts))
	}

	if !strings.Contains(prompts[0], "create a file with contents 'a'") {
		t.Errorf("prompt[0] = %q, expected to contain file creation request", prompts[0])
	}
	if !strings.Contains(prompts[2], "bingo") {
		t.Errorf("prompt[2] = %q, expected to contain 'bingo'", prompts[2])
	}

	// Verify <user_query> tags are stripped
	for i, p := range prompts {
		if strings.Contains(p, "<user_query>") || strings.Contains(p, "</user_query>") {
			t.Errorf("prompt[%d] still contains <user_query> tags: %q", i, p)
		}
	}
}

func TestExtractUserPrompts_CursorEmpty(t *testing.T) {
	t.Parallel()

	prompts := extractUserPrompts(agent.AgentTypeCursor, "")
	if len(prompts) != 0 {
		t.Errorf("extractUserPrompts(Cursor, empty) = %v, want empty", prompts)
	}
}

func TestCalculateTokenUsage_CursorRealTranscript(t *testing.T) {
	t.Parallel()

	// Even with a multi-line real transcript, Cursor should return nil
	ag, err := agent.GetByAgentType(agent.AgentTypeCursor)
	if err != nil {
		t.Fatalf("GetByAgentType(Cursor) error: %v", err)
	}
	result := agent.CalculateTokenUsage(context.Background(), ag, []byte(cursorSampleTranscript), 0, "")
	if result != nil {
		t.Errorf("CalculateTokenUsage(Cursor, real transcript) = %+v, want nil", result)
	}
}

func TestCalculateTokenUsage_CursorWithOffset(t *testing.T) {
	t.Parallel()

	// Offset should not matter — Cursor always returns nil
	ag, err := agent.GetByAgentType(agent.AgentTypeCursor)
	if err != nil {
		t.Fatalf("GetByAgentType(Cursor) error: %v", err)
	}
	result := agent.CalculateTokenUsage(context.Background(), ag, []byte(cursorSampleTranscript), 3, "")
	if result != nil {
		t.Errorf("CalculateTokenUsage(Cursor, offset=3) = %+v, want nil", result)
	}
}

func TestGenerateContextFromPrompts_CJKTruncation(t *testing.T) {
	t.Parallel()

	// 600 CJK characters exceeds the 500-rune truncation limit.
	prompt := strings.Repeat("あ", 600)

	result := generateContextFromPrompts([]string{prompt})

	if !utf8.Valid(result) {
		t.Error("generateContextFromPrompts produced invalid UTF-8 when truncating a CJK prompt")
	}

	resultStr := string(result)
	if !strings.Contains(resultStr, "...") {
		t.Error("expected truncated CJK prompt to contain '...' suffix")
	}
	// Should not contain more than 500 CJK characters
	if strings.Contains(resultStr, strings.Repeat("あ", 501)) {
		t.Error("CJK prompt was not truncated")
	}
}

func TestGenerateContextFromPrompts_EmojiTruncation(t *testing.T) {
	t.Parallel()

	// 600 emoji exceeds the 500-rune truncation limit.
	prompt := strings.Repeat("🎉", 600)

	result := generateContextFromPrompts([]string{prompt})

	if !utf8.Valid(result) {
		t.Error("generateContextFromPrompts produced invalid UTF-8 when truncating an emoji prompt")
	}

	resultStr := string(result)
	if !strings.Contains(resultStr, "...") {
		t.Error("expected truncated emoji prompt to contain '...' suffix")
	}
}

func TestGenerateContextFromPrompts_ASCIITruncation(t *testing.T) {
	t.Parallel()

	// Pure ASCII: should truncate at 500 runes with "..." suffix.
	prompt := strings.Repeat("a", 600)

	result := generateContextFromPrompts([]string{prompt})

	if !utf8.Valid(result) {
		t.Error("generateContextFromPrompts produced invalid UTF-8 when truncating an ASCII prompt")
	}

	resultStr := string(result)
	if !strings.Contains(resultStr, "...") {
		t.Error("expected truncated prompt to contain '...' suffix")
	}

	if strings.Contains(resultStr, strings.Repeat("a", 501)) {
		t.Error("prompt was not truncated")
	}
}

func TestGenerateContextFromPrompts_ShortCJKNotTruncated(t *testing.T) {
	t.Parallel()

	// 200 CJK characters is under the 500-rune limit, should not be truncated.
	prompt := strings.Repeat("あ", 200)

	result := generateContextFromPrompts([]string{prompt})

	if !utf8.Valid(result) {
		t.Error("generateContextFromPrompts produced invalid UTF-8")
	}

	resultStr := string(result)
	if strings.Contains(resultStr, "...") {
		t.Error("short CJK prompt should not be truncated")
	}
}

// droidSampleTranscript is a Droid JSONL transcript with user and assistant messages
// in Droid's envelope format: {"type":"message","message":{"role":"...","content":[...]}}.
var droidSampleTranscript = strings.Join([]string{
	`{"type":"session_start","session":{"session_id":"s1"}}`,
	`{"type":"message","id":"m1","message":{"role":"user","content":[{"type":"text","text":"create a file called hello.go"}]}}`,
	`{"type":"message","id":"m2","message":{"role":"assistant","content":[{"type":"text","text":"I'll create that file."}]}}`,
	`{"type":"message","id":"m3","message":{"role":"user","content":[{"type":"text","text":"<ide_opened_file>some content</ide_opened_file>now add a main function"}]}}`,
	`{"type":"message","id":"m4","message":{"role":"assistant","content":[{"type":"text","text":"Added the main function."}]}}`,
}, "\n") + "\n"

func TestExtractUserPrompts_Droid(t *testing.T) {
	t.Parallel()

	prompts := extractUserPrompts(agent.AgentTypeFactoryAIDroid, droidSampleTranscript)
	if len(prompts) != 2 {
		t.Fatalf("extractUserPrompts(Droid) returned %d prompts, want 2", len(prompts))
	}

	if prompts[0] != "create a file called hello.go" {
		t.Errorf("prompt[0] = %q, want %q", prompts[0], "create a file called hello.go")
	}

	// Verify IDE tags are stripped
	if strings.Contains(prompts[1], "<ide_opened_file>") {
		t.Errorf("prompt[1] still contains IDE tags: %q", prompts[1])
	}
	if prompts[1] != "now add a main function" {
		t.Errorf("prompt[1] = %q, want %q", prompts[1], "now add a main function")
	}
}

func TestExtractUserPrompts_DroidEmpty(t *testing.T) {
	t.Parallel()

	prompts := extractUserPrompts(agent.AgentTypeFactoryAIDroid, "")
	if len(prompts) != 0 {
		t.Errorf("extractUserPrompts(Droid, empty) = %v, want empty", prompts)
	}
}

// droidMessage builds a Droid JSONL "message" line with the given id, role, and optional usage.
func droidMessage(t *testing.T, id, role string, usage map[string]int) string {
	t.Helper()
	inner := map[string]interface{}{
		"role":    role,
		"content": []interface{}{},
	}
	if usage != nil {
		inner["id"] = id
		inner["usage"] = usage
	}
	msg, err := json.Marshal(inner)
	if err != nil {
		t.Fatalf("failed to marshal inner message: %v", err)
	}
	line := map[string]interface{}{
		"type":    "message",
		"id":      id,
		"message": json.RawMessage(msg),
	}
	b, err := json.Marshal(line)
	if err != nil {
		t.Fatalf("failed to marshal droid line: %v", err)
	}
	return string(b)
}

func TestCalculateTokenUsage_DroidStartOffsetSkipsNonMessageLines(t *testing.T) {
	t.Parallel()

	// Build a Droid transcript with non-message entries interspersed:
	// Line 0: session_start (non-message)
	// Line 1: user message (no tokens)
	// Line 2: assistant message with 10 input, 20 output tokens
	// Line 3: session_event (non-message)
	// Line 4: assistant message with 5 input, 30 output tokens
	transcript := "" +
		`{"type":"session_start","id":"s1"}` + "\n" +
		droidMessage(t, "m1", "user", nil) + "\n" +
		droidMessage(t, "m2", "assistant", map[string]int{
			"input_tokens": 10, "output_tokens": 20,
		}) + "\n" +
		`{"type":"session_event","data":"heartbeat"}` + "\n" +
		droidMessage(t, "m3", "assistant", map[string]int{
			"input_tokens": 5, "output_tokens": 30,
		}) + "\n"

	data := []byte(transcript)

	// With startOffset=0: should count all messages (m2 + m3)
	usageAll := calculateTokenUsage(agent.AgentTypeFactoryAIDroid, data, 0)
	if usageAll.InputTokens != 15 {
		t.Errorf("startOffset=0: InputTokens = %d, want 15", usageAll.InputTokens)
	}
	if usageAll.OutputTokens != 50 {
		t.Errorf("startOffset=0: OutputTokens = %d, want 50", usageAll.OutputTokens)
	}
	if usageAll.APICallCount != 2 {
		t.Errorf("startOffset=0: APICallCount = %d, want 2", usageAll.APICallCount)
	}

	// With startOffset=3: skip lines 0-2 (session_start, m1, m2).
	// Only line 3 (session_event, filtered) and line 4 (m3) remain.
	// Should count only m3's tokens.
	usageFrom3 := calculateTokenUsage(agent.AgentTypeFactoryAIDroid, data, 3)
	if usageFrom3.InputTokens != 5 {
		t.Errorf("startOffset=3: InputTokens = %d, want 5", usageFrom3.InputTokens)
	}
	if usageFrom3.OutputTokens != 30 {
		t.Errorf("startOffset=3: OutputTokens = %d, want 30", usageFrom3.OutputTokens)
	}
	if usageFrom3.APICallCount != 1 {
		t.Errorf("startOffset=3: APICallCount = %d, want 1", usageFrom3.APICallCount)
	}

	// Regression: using the OLD buggy code would have parsed all messages (ignoring
	// non-message entries), producing [m1, m2, m3], then sliced at index 3 which
	// is out of bounds — returning all tokens instead of just m3's.
	// With startOffset=1: skip only line 0 (session_start).
	// Lines 1 (m1), 2 (m2), 3 (session_event, filtered), 4 (m3) remain.
	usageFrom1 := calculateTokenUsage(agent.AgentTypeFactoryAIDroid, data, 1)
	if usageFrom1.InputTokens != 15 {
		t.Errorf("startOffset=1: InputTokens = %d, want 15", usageFrom1.InputTokens)
	}
	if usageFrom1.APICallCount != 2 {
		t.Errorf("startOffset=1: APICallCount = %d, want 2", usageFrom1.APICallCount)
	}
}

// Verify that startOffset beyond transcript length returns empty usage.
func TestCalculateTokenUsage_DroidStartOffsetBeyondEnd(t *testing.T) {
	t.Parallel()

	data := []byte(
		`{"type":"session_start","id":"s1"}` + "\n" +
			droidMessage(t, "m1", "assistant", map[string]int{
				"input_tokens": 10, "output_tokens": 20,
			}) + "\n",
	)

	usage := calculateTokenUsage(agent.AgentTypeFactoryAIDroid, data, 100)
	if usage.InputTokens != 0 {
		t.Errorf("InputTokens = %d, want 0", usage.InputTokens)
	}
	if usage.APICallCount != 0 {
		t.Errorf("APICallCount = %d, want 0", usage.APICallCount)
	}
}
