package summarize

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
	"github.com/entireio/cli/redact"
	"github.com/stretchr/testify/require"
)

const testMainGoFile = "main.go"

func TestBuildCondensedTranscript_UserPrompts(t *testing.T) {
	lines := []transcript.Line{
		{
			Type: "user",
			UUID: "user-1",
			Message: mustMarshal(t, transcript.UserMessage{
				Content: "Hello, please help me with this task",
			}),
		},
	}

	entries := BuildCondensedTranscript(lines)

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	if entries[0].Type != EntryTypeUser {
		t.Errorf("expected type %s, got %s", EntryTypeUser, entries[0].Type)
	}

	if entries[0].Content != "Hello, please help me with this task" {
		t.Errorf("unexpected content: %s", entries[0].Content)
	}
}

func TestBuildCondensedTranscript_AssistantResponses(t *testing.T) {
	lines := []transcript.Line{
		{
			Type: "assistant",
			UUID: "assistant-1",
			Message: mustMarshal(t, transcript.AssistantMessage{
				Content: []transcript.ContentBlock{
					{Type: "text", Text: "I'll help you with that."},
				},
			}),
		},
	}

	entries := BuildCondensedTranscript(lines)

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	if entries[0].Type != EntryTypeAssistant {
		t.Errorf("expected type %s, got %s", EntryTypeAssistant, entries[0].Type)
	}

	if entries[0].Content != "I'll help you with that." {
		t.Errorf("unexpected content: %s", entries[0].Content)
	}
}

func TestBuildCondensedTranscript_ToolCalls(t *testing.T) {
	lines := []transcript.Line{
		{
			Type: "assistant",
			UUID: "assistant-1",
			Message: mustMarshal(t, transcript.AssistantMessage{
				Content: []transcript.ContentBlock{
					{
						Type: "tool_use",
						Name: "Read",
						Input: mustMarshal(t, transcript.ToolInput{
							FilePath: "/path/to/file.go",
						}),
					},
				},
			}),
		},
	}

	entries := BuildCondensedTranscript(lines)

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	if entries[0].Type != EntryTypeTool {
		t.Errorf("expected type %s, got %s", EntryTypeTool, entries[0].Type)
	}

	if entries[0].ToolName != "Read" {
		t.Errorf("expected tool name Read, got %s", entries[0].ToolName)
	}

	if entries[0].ToolDetail != "/path/to/file.go" {
		t.Errorf("expected tool detail /path/to/file.go, got %s", entries[0].ToolDetail)
	}
}

func TestBuildCondensedTranscript_ToolCallWithCommand(t *testing.T) {
	lines := []transcript.Line{
		{
			Type: "assistant",
			UUID: "assistant-1",
			Message: mustMarshal(t, transcript.AssistantMessage{
				Content: []transcript.ContentBlock{
					{
						Type: "tool_use",
						Name: "Bash",
						Input: mustMarshal(t, transcript.ToolInput{
							Command: "go test ./...",
						}),
					},
				},
			}),
		},
	}

	entries := BuildCondensedTranscript(lines)

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	if entries[0].ToolDetail != "go test ./..." {
		t.Errorf("expected tool detail 'go test ./...', got %s", entries[0].ToolDetail)
	}
}

func TestBuildCondensedTranscript_SkillToolMinimalDetail(t *testing.T) {
	lines := []transcript.Line{
		{
			Type: "assistant",
			UUID: "assistant-1",
			Message: mustMarshal(t, transcript.AssistantMessage{
				Content: []transcript.ContentBlock{
					{
						Type: "tool_use",
						Name: "Skill",
						Input: mustMarshal(t, transcript.ToolInput{
							Skill: "superpowers:brainstorming",
						}),
					},
				},
			}),
		},
	}

	entries := BuildCondensedTranscript(lines)

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	if entries[0].ToolName != "Skill" {
		t.Errorf("expected tool name Skill, got %s", entries[0].ToolName)
	}

	// Should only show the skill name, not any verbose content
	if entries[0].ToolDetail != "superpowers:brainstorming" {
		t.Errorf("expected tool detail 'superpowers:brainstorming', got %s", entries[0].ToolDetail)
	}
}

func TestBuildCondensedTranscript_WebFetchMinimalDetail(t *testing.T) {
	lines := []transcript.Line{
		{
			Type: "assistant",
			UUID: "assistant-1",
			Message: mustMarshal(t, transcript.AssistantMessage{
				Content: []transcript.ContentBlock{
					{
						Type: "tool_use",
						Name: "WebFetch",
						Input: mustMarshal(t, transcript.ToolInput{
							URL:    "https://example.com/docs",
							Prompt: "Extract the API documentation",
						}),
					},
				},
			}),
		},
	}

	entries := BuildCondensedTranscript(lines)

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	// Should only show the URL, not the prompt
	if entries[0].ToolDetail != "https://example.com/docs" {
		t.Errorf("expected tool detail 'https://example.com/docs', got %s", entries[0].ToolDetail)
	}
}

func TestBuildCondensedTranscript_SkipsSkillContentInjection(t *testing.T) {
	skillContent := `Base directory for this skill: /Users/alex/.claude/plugins/cache/superpowers/4.1.1/skills/brainstorming

# Brainstorming Ideas Into Designs

## Overview

This is verbose skill content that should not appear in summaries...`

	lines := []transcript.Line{
		{
			Type: "user",
			UUID: "user-1",
			Message: mustMarshal(t, transcript.UserMessage{
				Content: "Invoke the superpowers:brainstorming skill",
			}),
		},
		{
			Type: "assistant",
			UUID: "assistant-1",
			Message: mustMarshal(t, transcript.AssistantMessage{
				Content: []transcript.ContentBlock{
					{Type: "tool_use", Name: "Skill", Input: mustMarshal(t, transcript.ToolInput{Skill: "superpowers:brainstorming"})},
				},
			}),
		},
		{
			Type: "user",
			UUID: "user-2",
			Message: mustMarshal(t, transcript.UserMessage{
				Content: skillContent, // This should be filtered out
			}),
		},
		{
			Type: "user",
			UUID: "user-3",
			Message: mustMarshal(t, transcript.UserMessage{
				Content: "Now help me brainstorm a feature",
			}),
		},
	}

	entries := BuildCondensedTranscript(lines)

	// Should have: user prompt, tool call, user prompt (NOT the skill content)
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries (skill content filtered), got %d", len(entries))
	}

	// Verify the skill content was filtered
	for _, entry := range entries {
		if entry.Type == EntryTypeUser && strings.Contains(entry.Content, "Base directory for this skill") {
			t.Error("skill content injection should have been filtered out")
		}
	}

	// Verify the real user messages are present
	if entries[0].Content != "Invoke the superpowers:brainstorming skill" {
		t.Errorf("first user message wrong: %s", entries[0].Content)
	}
	if entries[2].Content != "Now help me brainstorm a feature" {
		t.Errorf("last user message wrong: %s", entries[2].Content)
	}
}

//nolint:dupl // Test functions intentionally similar for different tag types
func TestBuildCondensedTranscript_StripIDEContextTags(t *testing.T) {
	lines := []transcript.Line{
		{
			Type: "user",
			UUID: "user-1",
			Message: mustMarshal(t, transcript.UserMessage{
				Content: "<ide_opened_file>some file content</ide_opened_file>Please review this code",
			}),
		},
	}

	entries := BuildCondensedTranscript(lines)

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	if entries[0].Content != "Please review this code" {
		t.Errorf("expected IDE tags to be stripped, got: %s", entries[0].Content)
	}
}

//nolint:dupl // Test functions intentionally similar for different tag types
func TestBuildCondensedTranscript_StripSystemTags(t *testing.T) {
	lines := []transcript.Line{
		{
			Type: "user",
			UUID: "user-1",
			Message: mustMarshal(t, transcript.UserMessage{
				Content: "<system-reminder>internal instructions</system-reminder>User question here",
			}),
		},
	}

	entries := BuildCondensedTranscript(lines)

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	if entries[0].Content != "User question here" {
		t.Errorf("expected system tags to be stripped, got: %s", entries[0].Content)
	}
}

func TestBuildCondensedTranscript_MixedContent(t *testing.T) {
	lines := []transcript.Line{
		{
			Type: "user",
			UUID: "user-1",
			Message: mustMarshal(t, transcript.UserMessage{
				Content: "Create a new file",
			}),
		},
		{
			Type: "assistant",
			UUID: "assistant-1",
			Message: mustMarshal(t, transcript.AssistantMessage{
				Content: []transcript.ContentBlock{
					{Type: "text", Text: "I'll create that file for you."},
					{
						Type: "tool_use",
						Name: "Write",
						Input: mustMarshal(t, transcript.ToolInput{
							FilePath: "/path/to/new.go",
						}),
					},
				},
			}),
		},
	}

	entries := BuildCondensedTranscript(lines)

	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	if entries[0].Type != EntryTypeUser {
		t.Errorf("entry 0: expected type %s, got %s", EntryTypeUser, entries[0].Type)
	}

	if entries[1].Type != EntryTypeAssistant {
		t.Errorf("entry 1: expected type %s, got %s", EntryTypeAssistant, entries[1].Type)
	}

	if entries[2].Type != EntryTypeTool {
		t.Errorf("entry 2: expected type %s, got %s", EntryTypeTool, entries[2].Type)
	}
}

func TestBuildCondensedTranscript_EmptyTranscript(t *testing.T) {
	lines := []transcript.Line{}

	entries := BuildCondensedTranscript(lines)

	if len(entries) != 0 {
		t.Errorf("expected 0 entries for empty transcript, got %d", len(entries))
	}
}

func TestBuildCondensedTranscript_UserArrayContent(t *testing.T) {
	// Test user message with array content (text blocks)
	lines := []transcript.Line{
		{
			Type: "user",
			UUID: "user-1",
			Message: mustMarshal(t, map[string]interface{}{
				"content": []interface{}{
					map[string]interface{}{
						"type": "text",
						"text": "First part",
					},
					map[string]interface{}{
						"type": "text",
						"text": "Second part",
					},
				},
			}),
		},
	}

	entries := BuildCondensedTranscript(lines)

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	expected := "First part\n\nSecond part"
	if entries[0].Content != expected {
		t.Errorf("expected %q, got %q", expected, entries[0].Content)
	}
}

func TestBuildCondensedTranscript_SkipsEmptyContent(t *testing.T) {
	lines := []transcript.Line{
		{
			Type: "user",
			UUID: "user-1",
			Message: mustMarshal(t, transcript.UserMessage{
				Content: "<ide_opened_file>only tags</ide_opened_file>",
			}),
		},
		{
			Type: "assistant",
			UUID: "assistant-1",
			Message: mustMarshal(t, transcript.AssistantMessage{
				Content: []transcript.ContentBlock{
					{Type: "text", Text: ""}, // Empty text
				},
			}),
		},
	}

	entries := BuildCondensedTranscript(lines)

	if len(entries) != 0 {
		t.Errorf("expected 0 entries for empty content, got %d", len(entries))
	}
}

func TestBuildCondensedTranscriptFromBytes_GeminiUserAndAssistant(t *testing.T) {
	geminiJSON := `{"messages":[
		{"type":"user","content":"Help me write a Go function"},
		{"type":"gemini","content":"Sure, here is a function that does what you need."}
	]}`

	entries, err := BuildCondensedTranscriptFromBytes(redact.AlreadyRedacted([]byte(geminiJSON)), agent.AgentTypeGemini)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	if entries[0].Type != EntryTypeUser {
		t.Errorf("entry 0: expected type %s, got %s", EntryTypeUser, entries[0].Type)
	}
	if entries[0].Content != "Help me write a Go function" {
		t.Errorf("entry 0: unexpected content: %s", entries[0].Content)
	}

	if entries[1].Type != EntryTypeAssistant {
		t.Errorf("entry 1: expected type %s, got %s", EntryTypeAssistant, entries[1].Type)
	}
	if entries[1].Content != "Sure, here is a function that does what you need." {
		t.Errorf("entry 1: unexpected content: %s", entries[1].Content)
	}
}

func TestBuildCondensedTranscriptFromBytes_GeminiToolCalls(t *testing.T) {
	geminiJSON := `{"messages":[
		{"type":"user","content":"Read the main.go file"},
		{"type":"gemini","content":"Let me read that file.","toolCalls":[
			{"id":"tc-1","name":"read_file","args":{"file_path":"/src/main.go"}},
			{"id":"tc-2","name":"run_command","args":{"command":"go build ./..."}}
		]}
	]}`

	entries, err := BuildCondensedTranscriptFromBytes(redact.AlreadyRedacted([]byte(geminiJSON)), agent.AgentTypeGemini)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(entries) != 4 {
		t.Fatalf("expected 4 entries (user + assistant + 2 tools), got %d", len(entries))
	}

	// Tool call with file_path arg
	if entries[2].Type != EntryTypeTool {
		t.Errorf("entry 2: expected type %s, got %s", EntryTypeTool, entries[2].Type)
	}
	if entries[2].ToolName != "read_file" {
		t.Errorf("entry 2: expected tool name read_file, got %s", entries[2].ToolName)
	}
	if entries[2].ToolDetail != "/src/main.go" {
		t.Errorf("entry 2: expected tool detail /src/main.go, got %s", entries[2].ToolDetail)
	}

	// Tool call with command arg
	if entries[3].ToolName != "run_command" {
		t.Errorf("entry 3: expected tool name run_command, got %s", entries[3].ToolName)
	}
	if entries[3].ToolDetail != "go build ./..." {
		t.Errorf("entry 3: expected tool detail 'go build ./...', got %s", entries[3].ToolDetail)
	}
}

func TestBuildCondensedTranscriptFromBytes_GeminiToolCallArgShapes(t *testing.T) {
	// Tool call with "path" arg (instead of "file_path")
	geminiJSON := `{"messages":[
		{"type":"gemini","toolCalls":[
			{"id":"tc-1","name":"write_file","args":{"path":"/tmp/out.txt","content":"hello"}},
			{"id":"tc-2","name":"search","args":{"pattern":"TODO","description":"Search for TODOs"}},
			{"id":"tc-3","name":"unknown_tool","args":{"foo":"bar"}}
		]}
	]}`

	entries, err := BuildCondensedTranscriptFromBytes(redact.AlreadyRedacted([]byte(geminiJSON)), agent.AgentTypeGemini)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	// "path" arg
	if entries[0].ToolDetail != "/tmp/out.txt" {
		t.Errorf("entry 0: expected tool detail /tmp/out.txt, got %s", entries[0].ToolDetail)
	}

	// "description" arg (checked before "pattern" in extractGenericToolDetail)
	if entries[1].ToolDetail != "Search for TODOs" {
		t.Errorf("entry 1: expected tool detail 'Search for TODOs', got %s", entries[1].ToolDetail)
	}

	// No recognized args - empty detail
	if entries[2].ToolDetail != "" {
		t.Errorf("entry 2: expected empty tool detail, got %s", entries[2].ToolDetail)
	}
}

func TestBuildCondensedTranscriptFromBytes_GeminiSkipsEmptyContent(t *testing.T) {
	geminiJSON := `{"messages":[
		{"type":"user","content":""},
		{"type":"gemini","content":"","toolCalls":[
			{"id":"tc-1","name":"read_file","args":{"file_path":"main.go"}}
		]},
		{"type":"user","content":"Thanks"}
	]}`

	entries, err := BuildCondensedTranscriptFromBytes(redact.AlreadyRedacted([]byte(geminiJSON)), agent.AgentTypeGemini)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Empty user and empty assistant content should be skipped, only tool call + "Thanks" remain
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Type != EntryTypeTool {
		t.Errorf("entry 0: expected type %s, got %s", EntryTypeTool, entries[0].Type)
	}
	if entries[1].Type != EntryTypeUser {
		t.Errorf("entry 1: expected type %s, got %s", EntryTypeUser, entries[1].Type)
	}
	if entries[1].Content != "Thanks" {
		t.Errorf("entry 1: expected content 'Thanks', got %s", entries[1].Content)
	}
}

func TestBuildCondensedTranscriptFromBytes_GeminiEmptyTranscript(t *testing.T) {
	geminiJSON := `{"messages":[]}`

	entries, err := BuildCondensedTranscriptFromBytes(redact.AlreadyRedacted([]byte(geminiJSON)), agent.AgentTypeGemini)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(entries) != 0 {
		t.Errorf("expected 0 entries for empty Gemini transcript, got %d", len(entries))
	}
}

func TestBuildCondensedTranscriptFromBytes_GeminiInvalidJSON(t *testing.T) {
	_, err := BuildCondensedTranscriptFromBytes(redact.AlreadyRedacted([]byte(`not json`)), agent.AgentTypeGemini)
	if err == nil {
		t.Error("expected error for invalid Gemini JSON")
	}
}

func TestFormatCondensedTranscript_BasicFormat(t *testing.T) {
	input := Input{
		Transcript: []Entry{
			{Type: EntryTypeUser, Content: "Hello"},
			{Type: EntryTypeAssistant, Content: "Hi there"},
			{Type: EntryTypeTool, ToolName: "Read", ToolDetail: "/file.go"},
		},
	}

	result := FormatCondensedTranscript(input)

	expected := `[User] Hello

[Assistant] Hi there

[Tool] Read: /file.go
`
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestFormatCondensedTranscript_WithFiles(t *testing.T) {
	input := Input{
		Transcript: []Entry{
			{Type: EntryTypeUser, Content: "Create files"},
		},
		FilesTouched: []string{"file1.go", "file2.go"},
	}

	result := FormatCondensedTranscript(input)

	expected := `[User] Create files

[Files Modified]
- file1.go
- file2.go
`
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestFormatCondensedTranscript_ToolWithoutDetail(t *testing.T) {
	input := Input{
		Transcript: []Entry{
			{Type: EntryTypeTool, ToolName: "TaskList"},
		},
	}

	result := FormatCondensedTranscript(input)

	expected := "[Tool] TaskList\n"
	if result != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, result)
	}
}

func TestFormatCondensedTranscript_EmptyInput(t *testing.T) {
	input := Input{}

	result := FormatCondensedTranscript(input)

	if result != "" {
		t.Errorf("expected empty string for empty input, got: %s", result)
	}
}

func TestGenerateFromTranscript(t *testing.T) {
	// Test with mock generator
	mockGenerator := &ClaudeGenerator{
		TextGenerator: &stubTextGenerator{text: `{"intent":"Test intent","outcome":"Test outcome","learnings":{"repo":[],"code":[],"workflow":[]},"friction":[],"open_items":[]}`},
	}

	transcript := []byte(`{"type":"user","message":{"content":"Hello"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"Hi there"}]}}`)

	summary, err := GenerateFromTranscript(context.Background(), redact.AlreadyRedacted(transcript), []string{"file.go"}, "", mockGenerator)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, summary, "expected non-nil summary")
	if summary.Intent != "Test intent" {
		t.Errorf("unexpected intent: %s", summary.Intent)
	}
}

// TestGenerateFromTranscript_PreservesClaudeError pins both //nolint:wrapcheck
// contracts in one shot: the stub TextGenerator returns a *ClaudeError, which
// must survive ClaudeGenerator.Generate (claude.go) AND GenerateFromTranscript
// (summarize.go) so the explain layer can map it to actionable user messaging
// via errors.As. A regression at either layer that flattens the typed error
// (e.g. fmt.Errorf("...: %v", err)) would fail this test.
func TestGenerateFromTranscript_PreservesClaudeError(t *testing.T) {
	t.Parallel()

	claudeErr := &claudecode.ClaudeError{
		Kind:      claudecode.ClaudeErrorRateLimit,
		Message:   "Rate limit exceeded",
		APIStatus: 429,
	}
	gen := &ClaudeGenerator{TextGenerator: &stubTextGenerator{err: claudeErr}}

	transcript := []byte(`{"type":"user","message":{"content":"Hello"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"Hi there"}]}}`)

	_, err := GenerateFromTranscript(context.Background(), redact.AlreadyRedacted(transcript), []string{}, "", gen)
	wrapped := fmt.Errorf("explain generate call: %w", err)

	var ce *claudecode.ClaudeError
	if !errors.As(wrapped, &ce) {
		t.Fatalf("errors.As could not recover *ClaudeError from chain: %v", wrapped)
	}
	if ce.Kind != claudecode.ClaudeErrorRateLimit {
		t.Errorf("Kind = %v; want %v", ce.Kind, claudecode.ClaudeErrorRateLimit)
	}
	if ce.APIStatus != 429 {
		t.Errorf("APIStatus = %d; want 429", ce.APIStatus)
	}
}

func TestGenerateFromTranscript_EmptyTranscript(t *testing.T) {
	mockGenerator := &ClaudeGenerator{}

	summary, err := GenerateFromTranscript(context.Background(), redact.AlreadyRedacted([]byte{}), []string{}, "", mockGenerator)
	if err == nil {
		t.Error("expected error for empty transcript")
	}
	if summary != nil {
		t.Error("expected nil summary")
	}
}

func TestGenerateFromTranscript_NilGenerator(t *testing.T) {
	transcript := []byte(`{"type":"user","message":{"content":"Hello"}}`)

	// With nil generator, should use default ClaudeGenerator
	// This will fail because claude CLI isn't available in test, but tests the nil handling
	_, err := GenerateFromTranscript(context.Background(), redact.AlreadyRedacted(transcript), []string{}, "", nil)
	// Error is expected (claude CLI not available), but function should not panic
	if err == nil {
		t.Log("Unexpectedly succeeded - claude CLI must be available")
	}
}

func TestBuildCondensedTranscriptFromBytes_Codex(t *testing.T) {
	t.Parallel()

	codexTranscript := []byte(`{"timestamp":"2026-04-01T23:31:27.000Z","type":"session_meta","payload":{"id":"s1"}}
{"timestamp":"2026-04-01T23:31:28.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"create hello.txt"}]}}
{"timestamp":"2026-04-01T23:31:29.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Creating the file now."}]}}
{"timestamp":"2026-04-01T23:31:30.000Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"call_1","arguments":"{\"cmd\":\"touch hello.txt\",\"workdir\":\"/repo\"}"}}
{"timestamp":"2026-04-01T23:31:31.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"ok"}}
{"timestamp":"2026-04-01T23:31:32.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done."}]}}
`)

	entries, err := BuildCondensedTranscriptFromBytes(redact.AlreadyRedacted(codexTranscript), agent.AgentTypeCodex)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(entries) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(entries))
	}

	if entries[0].Type != EntryTypeUser || entries[0].Content != "create hello.txt" {
		t.Fatalf("unexpected first entry: %#v", entries[0])
	}

	if entries[1].Type != EntryTypeAssistant || entries[1].Content != "Creating the file now." {
		t.Fatalf("unexpected second entry: %#v", entries[1])
	}

	if entries[2].Type != EntryTypeTool || entries[2].ToolName != "exec_command" {
		t.Fatalf("unexpected tool entry: %#v", entries[2])
	}

	if entries[3].Type != EntryTypeAssistant || entries[3].Content != "Done." {
		t.Fatalf("unexpected final entry: %#v", entries[3])
	}
}

func TestBuildCondensedTranscriptFromBytes_Codex_CustomToolCall(t *testing.T) {
	t.Parallel()

	codexTranscript := []byte(`{"timestamp":"t1","type":"session_meta","payload":{"id":"s1"}}
{"timestamp":"t2","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"create hello.txt"}]}}
{"timestamp":"t3","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Creating the file now."}]}}
{"timestamp":"t4","type":"response_item","payload":{"type":"custom_tool_call","status":"completed","call_id":"call_1","name":"apply_patch","input":"*** Begin Patch\n*** Add File: hello.txt\n+Hello World\n*** End Patch\n"}}
{"timestamp":"t5","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_1","output":{"type":"text","text":"Success. Updated: A hello.txt"}}}
{"timestamp":"t6","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done."}]}}
`)

	entries, err := BuildCondensedTranscriptFromBytes(redact.AlreadyRedacted(codexTranscript), agent.AgentTypeCodex)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(entries) != 4 {
		t.Fatalf("expected 4 entries, got %d: %#v", len(entries), entries)
	}

	if entries[0].Type != EntryTypeUser || entries[0].Content != "create hello.txt" {
		t.Fatalf("unexpected first entry: %#v", entries[0])
	}

	if entries[1].Type != EntryTypeAssistant || entries[1].Content != "Creating the file now." {
		t.Fatalf("unexpected second entry: %#v", entries[1])
	}

	if entries[2].Type != EntryTypeTool || entries[2].ToolName != "apply_patch" {
		t.Fatalf("expected apply_patch tool entry, got: %#v", entries[2])
	}

	if entries[3].Type != EntryTypeAssistant || entries[3].Content != "Done." {
		t.Fatalf("unexpected final entry: %#v", entries[3])
	}
}

func TestBuildCondensedTranscriptFromBytes_Codex_ExecCommandDetail(t *testing.T) {
	t.Parallel()

	codexTranscript := []byte(`{"timestamp":"t1","type":"session_meta","payload":{"id":"s1"}}
{"timestamp":"t2","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Running command."}]}}
{"timestamp":"t3","type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"call_1","arguments":"{\"cmd\":\"ls -la\",\"workdir\":\"/repo\"}"}}
{"timestamp":"t4","type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"total 0"}}
`)

	entries, err := BuildCondensedTranscriptFromBytes(redact.AlreadyRedacted(codexTranscript), agent.AgentTypeCodex)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Find the tool entry
	var toolEntry *Entry
	for i := range entries {
		if entries[i].Type == EntryTypeTool {
			toolEntry = &entries[i]
			break
		}
	}
	require.NotNil(t, toolEntry, "no tool entry found in entries: %#v", entries)
	if toolEntry.ToolName != "exec_command" {
		t.Fatalf("expected exec_command, got %q", toolEntry.ToolName)
	}
	if toolEntry.ToolDetail != "ls -la" {
		t.Fatalf("expected tool detail 'ls -la', got %q", toolEntry.ToolDetail)
	}
}

func TestBuildCondensedTranscriptFromBytes_OpenCodeUserAndAssistant(t *testing.T) {
	// OpenCode export JSON format
	ocExportJSON := `{
		"info": {"id": "test-session"},
		"messages": [
			{"info": {"id": "msg-1", "role": "user", "time": {"created": 1708300000}}, "parts": [{"type": "text", "text": "Fix the bug in main.go"}]},
			{"info": {"id": "msg-2", "role": "assistant", "time": {"created": 1708300001}}, "parts": [{"type": "text", "text": "I'll fix the bug."}]}
		]
	}`

	entries, err := BuildCondensedTranscriptFromBytes(redact.AlreadyRedacted([]byte(ocExportJSON)), agent.AgentTypeOpenCode)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	if entries[0].Type != EntryTypeUser {
		t.Errorf("entry 0: expected type %s, got %s", EntryTypeUser, entries[0].Type)
	}
	if entries[0].Content != "Fix the bug in main.go" {
		t.Errorf("entry 0: unexpected content: %s", entries[0].Content)
	}

	if entries[1].Type != EntryTypeAssistant {
		t.Errorf("entry 1: expected type %s, got %s", EntryTypeAssistant, entries[1].Type)
	}
	if entries[1].Content != "I'll fix the bug." {
		t.Errorf("entry 1: unexpected content: %s", entries[1].Content)
	}
}

func TestBuildCondensedTranscriptFromBytes_OpenCodeToolCalls(t *testing.T) {
	// OpenCode export JSON format with tool calls
	ocExportJSON := `{
		"info": {"id": "test-session"},
		"messages": [
			{"info": {"id": "msg-1", "role": "user", "time": {"created": 1708300000}}, "parts": [{"type": "text", "text": "Edit main.go"}]},
			{"info": {"id": "msg-2", "role": "assistant", "time": {"created": 1708300001}}, "parts": [
				{"type": "text", "text": "Editing now."},
				{"type": "tool", "tool": "edit", "callID": "call-1", "state": {"status": "completed", "input": {"filePath": "main.go"}, "output": "Applied"}},
				{"type": "tool", "tool": "bash", "callID": "call-2", "state": {"status": "completed", "input": {"command": "go test ./..."}, "output": "PASS"}}
			]}
		]
	}`

	entries, err := BuildCondensedTranscriptFromBytes(redact.AlreadyRedacted([]byte(ocExportJSON)), agent.AgentTypeOpenCode)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// user + assistant + 2 tool calls
	if len(entries) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(entries))
	}

	if entries[2].Type != EntryTypeTool {
		t.Errorf("entry 2: expected type %s, got %s", EntryTypeTool, entries[2].Type)
	}
	if entries[2].ToolName != "edit" {
		t.Errorf("entry 2: expected tool name edit, got %s", entries[2].ToolName)
	}
	if entries[2].ToolDetail != testMainGoFile {
		t.Errorf("entry 2: expected tool detail main.go, got %s", entries[2].ToolDetail)
	}

	if entries[3].ToolName != "bash" {
		t.Errorf("entry 3: expected tool name bash, got %s", entries[3].ToolName)
	}
	if entries[3].ToolDetail != "go test ./..." {
		t.Errorf("entry 3: expected tool detail 'go test ./...', got %s", entries[3].ToolDetail)
	}
}

func TestBuildCondensedTranscriptFromBytes_OpenCodeSkipsEmptyContent(t *testing.T) {
	// OpenCode export JSON format with empty content messages
	ocExportJSON := `{
		"info": {"id": "test-session"},
		"messages": [
			{"info": {"id": "msg-1", "role": "user", "time": {"created": 1708300000}}, "parts": []},
			{"info": {"id": "msg-2", "role": "assistant", "time": {"created": 1708300001}}, "parts": []},
			{"info": {"id": "msg-3", "role": "user", "time": {"created": 1708300010}}, "parts": [{"type": "text", "text": "Real prompt"}]}
		]
	}`

	entries, err := BuildCondensedTranscriptFromBytes(redact.AlreadyRedacted([]byte(ocExportJSON)), agent.AgentTypeOpenCode)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry (empty content skipped), got %d", len(entries))
	}
	if entries[0].Content != "Real prompt" {
		t.Errorf("expected 'Real prompt', got %s", entries[0].Content)
	}
}

func TestBuildCondensedTranscriptFromBytes_OpenCodeInvalidJSON(t *testing.T) {
	// Invalid JSON now returns an error (not silently skipped like JSONL)
	_, err := BuildCondensedTranscriptFromBytes(redact.AlreadyRedacted([]byte("not json")), agent.AgentTypeOpenCode)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestBuildCondensedTranscriptFromBytes_CompactTranscriptFallback(t *testing.T) {
	compactJSONL := `{"v":1,"agent":"pi","cli_version":"test","type":"user","ts":"2026-01-01T00:00:00Z","content":[{"text":"Create bye.txt"}]}
{"v":1,"agent":"pi","cli_version":"test","type":"assistant","ts":"2026-01-01T00:00:01Z","content":[{"type":"tool_use","id":"tc1","name":"Write","input":{"path":"bye.txt"}},{"type":"text","text":"Created bye.txt"}]}
`

	entries, err := BuildCondensedTranscriptFromBytes(redact.AlreadyRedacted([]byte(compactJSONL)), types.AgentType("Pi"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	if entries[0].Type != EntryTypeUser || entries[0].Content != "Create bye.txt" {
		t.Fatalf("unexpected first entry: %+v", entries[0])
	}
	if entries[1].Type != EntryTypeTool || entries[1].ToolName != "Write" || entries[1].ToolDetail != "bye.txt" {
		t.Fatalf("unexpected tool entry: %+v", entries[1])
	}
	if entries[2].Type != EntryTypeAssistant || entries[2].Content != "Created bye.txt" {
		t.Fatalf("unexpected assistant entry: %+v", entries[2])
	}
}

func TestBuildCondensedTranscriptFromBytes_CursorRoleBasedJSONL(t *testing.T) {
	// Cursor transcripts use "role" instead of "type" and wrap user text in <user_query> tags.
	// The transcript parser normalizes role→type, so condensation should work.
	cursorJSONL := `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nhello\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"text","text":"Hi there!"}]}}
{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nadd one to a file and commit\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"text","text":"Created one.txt with one and committed."}]}}
`

	entries, err := BuildCondensedTranscriptFromBytes(redact.AlreadyRedacted([]byte(cursorJSONL)), agent.AgentTypeCursor)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(entries) == 0 {
		t.Fatal("expected non-empty entries for Cursor transcript, got 0 (role→type normalization may be broken)")
	}

	// Should have 4 entries: 2 user + 2 assistant
	if len(entries) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(entries))
	}

	if entries[0].Type != EntryTypeUser {
		t.Errorf("entry 0: expected type %s, got %s", EntryTypeUser, entries[0].Type)
	}
	if !strings.Contains(entries[0].Content, "hello") {
		t.Errorf("entry 0: expected content containing 'hello', got %q", entries[0].Content)
	}

	if entries[1].Type != EntryTypeAssistant {
		t.Errorf("entry 1: expected type %s, got %s", EntryTypeAssistant, entries[1].Type)
	}
	if entries[1].Content != "Hi there!" {
		t.Errorf("entry 1: expected 'Hi there!', got %q", entries[1].Content)
	}

	if entries[2].Type != EntryTypeUser {
		t.Errorf("entry 2: expected type %s, got %s", EntryTypeUser, entries[2].Type)
	}

	if entries[3].Type != EntryTypeAssistant {
		t.Errorf("entry 3: expected type %s, got %s", EntryTypeAssistant, entries[3].Type)
	}
}

func TestBuildCondensedTranscriptFromBytes_CursorNoToolUseBlocks(t *testing.T) {
	// Cursor transcripts have no tool_use blocks — only text content.
	// This verifies we get entries (not an empty result) even without tool calls.
	cursorJSONL := `{"role":"user","message":{"content":[{"type":"text","text":"write a poem"}]}}
{"role":"assistant","message":{"content":[{"type":"text","text":"Here is a poem about code."}]}}
`

	entries, err := BuildCondensedTranscriptFromBytes(redact.AlreadyRedacted([]byte(cursorJSONL)), agent.AgentTypeCursor)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	// No tool entries should appear
	for i, e := range entries {
		if e.Type == EntryTypeTool {
			t.Errorf("entry %d: unexpected tool entry in Cursor transcript", i)
		}
	}
}

func TestBuildCondensedTranscriptFromBytes_DroidUserAndAssistant(t *testing.T) {
	// Droid uses an envelope: {"type":"message","id":"...","message":{"role":"...","content":[...]}}
	droidJSONL := strings.Join([]string{
		`{"type":"session_start","session":{"session_id":"s1"}}`,
		`{"type":"message","id":"m1","message":{"role":"user","content":[{"type":"text","text":"Help me write a Go function"}]}}`,
		`{"type":"message","id":"m2","message":{"role":"assistant","content":[{"type":"text","text":"Sure, here is a function."}]}}`,
		`{"type":"message","id":"m3","message":{"role":"assistant","content":[{"type":"tool_use","name":"Write","input":{"file_path":"main.go","content":"package main"}}]}}`,
	}, "\n") + "\n"

	entries, err := BuildCondensedTranscriptFromBytes(redact.AlreadyRedacted([]byte(droidJSONL)), agent.AgentTypeFactoryAIDroid)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// session_start is skipped; expect: user + assistant text + tool
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	if entries[0].Type != EntryTypeUser {
		t.Errorf("entry 0: expected type %s, got %s", EntryTypeUser, entries[0].Type)
	}
	if entries[0].Content != "Help me write a Go function" {
		t.Errorf("entry 0: unexpected content: %s", entries[0].Content)
	}

	if entries[1].Type != EntryTypeAssistant {
		t.Errorf("entry 1: expected type %s, got %s", EntryTypeAssistant, entries[1].Type)
	}
	if entries[1].Content != "Sure, here is a function." {
		t.Errorf("entry 1: unexpected content: %s", entries[1].Content)
	}

	if entries[2].Type != EntryTypeTool {
		t.Errorf("entry 2: expected type %s, got %s", EntryTypeTool, entries[2].Type)
	}
	if entries[2].ToolName != "Write" {
		t.Errorf("entry 2: expected tool name Write, got %s", entries[2].ToolName)
	}
	if entries[2].ToolDetail != testMainGoFile {
		t.Errorf("entry 2: expected tool detail main.go, got %s", entries[2].ToolDetail)
	}
}

func TestBuildCondensedTranscriptFromBytes_DroidMalformedInput(t *testing.T) {
	// Completely invalid content should return an error from the Droid parser
	_, err := BuildCondensedTranscriptFromBytes(redact.AlreadyRedacted([]byte("not valid jsonl at all{{{")), agent.AgentTypeFactoryAIDroid)
	// Droid parser is lenient — malformed lines are skipped. With no valid messages,
	// it returns an empty slice (not an error).
	if err != nil {
		t.Fatalf("unexpected error for malformed Droid input: %v", err)
	}
}

func TestBuildCondensedTranscriptFromBytes_DroidEmptyTranscript(t *testing.T) {
	entries, err := BuildCondensedTranscriptFromBytes(redact.AlreadyRedacted([]byte("")), agent.AgentTypeFactoryAIDroid)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries for empty Droid transcript, got %d", len(entries))
	}
}

// mustMarshal is a test helper that marshals v to JSON, failing the test on error.
func mustMarshal(t *testing.T, v interface{}) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}
	return data
}

func TestResolveModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		provider string
		model    string
		want     string
	}{
		{
			name:     "claude code with empty model defaults to DefaultModel",
			provider: string(agent.AgentNameClaudeCode),
			model:    "",
			want:     DefaultModel,
		},
		{
			name:     "other provider passes model through unchanged",
			provider: "codex",
			model:    "gpt-5",
			want:     "gpt-5",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ResolveModel(types.AgentName(tt.provider), tt.model)
			if got != tt.want {
				t.Errorf("ResolveModel(%q, %q) = %q, want %q", tt.provider, tt.model, got, tt.want)
			}
		})
	}
}
