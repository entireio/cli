package summarize

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

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

	entries, err := BuildCondensedTranscriptFromBytes([]byte(geminiJSON), agent.AgentTypeGemini)
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

	entries, err := BuildCondensedTranscriptFromBytes([]byte(geminiJSON), agent.AgentTypeGemini)
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

	entries, err := BuildCondensedTranscriptFromBytes([]byte(geminiJSON), agent.AgentTypeGemini)
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

	// "description" arg (checked before "pattern" in extractGeminiToolDetail)
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

	entries, err := BuildCondensedTranscriptFromBytes([]byte(geminiJSON), agent.AgentTypeGemini)
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

	entries, err := BuildCondensedTranscriptFromBytes([]byte(geminiJSON), agent.AgentTypeGemini)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(entries) != 0 {
		t.Errorf("expected 0 entries for empty Gemini transcript, got %d", len(entries))
	}
}

func TestBuildCondensedTranscriptFromBytes_GeminiInvalidJSON(t *testing.T) {
	_, err := BuildCondensedTranscriptFromBytes([]byte(`not json`), agent.AgentTypeGemini)
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
		CommandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			response := `{"result":"{\"intent\":\"Test intent\",\"outcome\":\"Test outcome\",\"learnings\":{\"repo\":[],\"code\":[],\"workflow\":[]},\"friction\":[],\"open_items\":[]}"}`
			return exec.CommandContext(ctx, "sh", "-c", "printf '%s' '"+response+"'")
		},
	}

	transcript := []byte(`{"type":"user","message":{"content":"Hello"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"Hi there"}]}}`)

	summary, err := GenerateFromTranscript(context.Background(), transcript, []string{"file.go"}, "", mockGenerator)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summary == nil {
		t.Fatal("expected non-nil summary")
	}
	if summary.Intent != "Test intent" {
		t.Errorf("unexpected intent: %s", summary.Intent)
	}
}

func TestGenerateFromTranscript_EmptyTranscript(t *testing.T) {
	mockGenerator := &ClaudeGenerator{}

	summary, err := GenerateFromTranscript(context.Background(), []byte{}, []string{}, "", mockGenerator)
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
	_, err := GenerateFromTranscript(context.Background(), transcript, []string{}, "", nil)
	// Error is expected (claude CLI not available), but function should not panic
	if err == nil {
		t.Log("Unexpectedly succeeded - claude CLI must be available")
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
