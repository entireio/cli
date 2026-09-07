package compact

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

func TestCompact_CopilotFixture(t *testing.T) {
	t.Parallel()

	assertFixtureTransform(t, agentOpts("copilot-cli"), "testdata/copilot_full.jsonl", "testdata/copilot_expected.jsonl")
}

// TestCompact_CopilotAssistantReasoningTextFallback exercises the real
// compactCopilot pipeline (not a hand-mocked call) against the actual
// testdata/copilot_full.jsonl fixture. Raw line 7 is an assistant.message
// event with an empty "content" field whose only displayable text lives in
// "reasoningText" ("Simple task - create a directory and an markdown file
// inside it."), alongside two tool requests. copilotAssistantLine must
// recover that text as a content block instead of silently dropping it, the
// same class of bug already fixed for the CLI-side extraction path in
// cmd/entire/cli/agent/copilotcli/transcript.go (#1070).
func TestCompact_CopilotAssistantReasoningTextFallback(t *testing.T) {
	t.Parallel()

	input, err := os.ReadFile("testdata/copilot_full.jsonl")
	if err != nil {
		t.Fatalf("failed to read fixture: %v", err)
	}

	result, err := compactCopilot(input, agentOpts("copilot-cli"))
	if err != nil {
		t.Fatalf("compactCopilot returned error: %v", err)
	}

	lines := nonEmptyLines(result)

	// The fixture's first assistant.message (raw line 7, messageId
	// c1034751-6d32-4f2d-9e58-c45256f6ff36) is the second compacted line.
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 compacted lines, got %d:\n%s", len(lines), string(result))
	}

	var got struct {
		ID      string `json:"id"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Name string `json:"name"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &got); err != nil {
		t.Fatalf("failed to unmarshal compacted line: %v\nline: %s", err, lines[1])
	}

	if got.ID != "c1034751-6d32-4f2d-9e58-c45256f6ff36" {
		t.Fatalf("unexpected message id %q, wanted the reasoningText-only assistant.message; full line: %s", got.ID, lines[1])
	}

	const wantReasoningText = "Simple task - create a directory and an markdown file inside it."

	var gotText string
	var toolNames []string
	for _, block := range got.Content {
		switch block.Type {
		case transcript.ContentTypeText:
			gotText = block.Text
		case transcript.ContentTypeToolUse:
			toolNames = append(toolNames, block.Name)
		}
	}

	if gotText != wantReasoningText {
		t.Errorf("reasoningText fallback dropped: got text block %q, want %q\nfull compacted line: %s", gotText, wantReasoningText, lines[1])
	}

	// Regression guard: the tool_use blocks that were already present must
	// survive alongside the recovered text block.
	wantTools := []string{"report_intent", "bash"}
	if len(toolNames) != len(wantTools) || toolNames[0] != wantTools[0] || toolNames[1] != wantTools[1] {
		t.Errorf("tool_use blocks changed: got %v, want %v\nfull compacted line: %s", toolNames, wantTools, lines[1])
	}
}

// TestCompact_CopilotAssistantContentTakesPrecedenceOverReasoningText is the
// normal-case regression guard: when "content" is populated, it must still be
// used verbatim and reasoningText must never override it.
func TestCompact_CopilotAssistantContentTakesPrecedenceOverReasoningText(t *testing.T) {
	t.Parallel()

	line := copilotLine{
		Type:      "assistant.message",
		Timestamp: "2026-04-07T21:08:41.062Z",
		Data:      json.RawMessage(`{"messageId":"m1","content":"real content","reasoningText":"should not appear","toolRequests":[]}`),
	}

	out := copilotAssistantLine(newTranscriptLine(agentOpts("copilot-cli")), line)
	if out == nil {
		t.Fatalf("copilotAssistantLine returned nil for populated content")
	}

	var content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(out.Content, &content); err != nil {
		t.Fatalf("failed to unmarshal content: %v", err)
	}
	if len(content) != 1 || content[0].Type != transcript.ContentTypeText || content[0].Text != "real content" {
		t.Errorf("got %+v, want a single text block with %q", content, "real content")
	}
}

// TestCompact_CopilotAssistantEmptyContentNoReasoningText mirrors the
// fixture's raw line 20 case: content empty, no reasoningText key at all.
// copilotAssistantLine must not synthesize a text block out of nothing.
func TestCompact_CopilotAssistantEmptyContentNoReasoningText(t *testing.T) {
	t.Parallel()

	line := copilotLine{
		Type:      "assistant.message",
		Timestamp: "2026-04-07T21:08:36.409Z",
		Data:      json.RawMessage(`{"messageId":"m2","content":"","toolRequests":[{"toolCallId":"t1","name":"create","arguments":{}}]}`),
	}

	out := copilotAssistantLine(newTranscriptLine(agentOpts("copilot-cli")), line)
	if out == nil {
		t.Fatalf("copilotAssistantLine returned nil unexpectedly")
	}

	var content []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(out.Content, &content); err != nil {
		t.Fatalf("failed to unmarshal content: %v", err)
	}
	for _, block := range content {
		if block.Type == transcript.ContentTypeText {
			t.Errorf("unexpected text block synthesized with no content and no reasoningText: %s", out.Content)
		}
	}
}
