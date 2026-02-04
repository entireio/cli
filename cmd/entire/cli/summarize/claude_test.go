package summarize

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestClaudeGenerator_CommandNotFound(t *testing.T) {
	gen := &ClaudeGenerator{
		CommandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			// Return a command that doesn't exist
			return exec.CommandContext(ctx, "nonexistent-command-that-should-not-exist-12345")
		},
	}

	input := Input{
		Transcript: []Entry{
			{Type: EntryTypeUser, Content: "Hello"},
		},
	}

	_, err := gen.Generate(context.Background(), input)
	if err == nil {
		t.Fatal("expected error when command not found")
	}

	// The error message should indicate the command failed
	if !strings.Contains(err.Error(), "not found") && !strings.Contains(err.Error(), "executable file not found") {
		t.Errorf("expected 'not found' error, got: %v", err)
	}
}

func TestClaudeGenerator_NonZeroExit(t *testing.T) {
	gen := &ClaudeGenerator{
		CommandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			// Return a command that will exit with non-zero status
			return exec.CommandContext(ctx, "sh", "-c", "echo 'error message' >&2; exit 1")
		},
	}

	input := Input{
		Transcript: []Entry{
			{Type: EntryTypeUser, Content: "Hello"},
		},
	}

	_, err := gen.Generate(context.Background(), input)
	if err == nil {
		t.Fatal("expected error on non-zero exit")
	}

	if !strings.Contains(err.Error(), "exit 1") {
		t.Errorf("expected exit code in error, got: %v", err)
	}
}

func TestClaudeGenerator_ErrorCases(t *testing.T) {
	tests := []struct {
		name          string
		cmdOutput     string
		expectedError string
	}{
		{
			name:          "invalid JSON response",
			cmdOutput:     "not valid json",
			expectedError: "parse claude CLI response",
		},
		{
			name:          "invalid summary JSON",
			cmdOutput:     `{"result": "not a valid summary object"}`,
			expectedError: "parse summary JSON",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gen := &ClaudeGenerator{
				CommandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
					return exec.CommandContext(ctx, "echo", tt.cmdOutput)
				},
			}

			input := Input{
				Transcript: []Entry{
					{Type: EntryTypeUser, Content: "Hello"},
				},
			}

			_, err := gen.Generate(context.Background(), input)
			if err == nil {
				t.Fatal("expected error")
			}

			if !strings.Contains(err.Error(), tt.expectedError) {
				t.Errorf("expected error containing %q, got: %v", tt.expectedError, err)
			}
		})
	}
}

func TestClaudeGenerator_ValidResponse(t *testing.T) {
	gen := &ClaudeGenerator{
		CommandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			// Use compact JSON to avoid newline issues with echo
			response := `{"result":"{\"intent\":\"User wanted to fix a bug\",\"outcome\":\"Bug was fixed successfully\",\"learnings\":{\"repo\":[\"The repo uses Go modules\"],\"code\":[{\"path\":\"main.go\",\"line\":10,\"finding\":\"Entry point\"}],\"workflow\":[\"Run tests before committing\"]},\"friction\":[\"Slow CI pipeline\"],\"open_items\":[\"Add more tests\"]}"}`
			return exec.CommandContext(ctx, "sh", "-c", "printf '%s' '"+response+"'")
		},
	}

	input := Input{
		Transcript: []Entry{
			{Type: EntryTypeUser, Content: "Fix the bug"},
		},
	}

	summary, err := gen.Generate(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if summary.Intent != "User wanted to fix a bug" {
		t.Errorf("unexpected intent: %s", summary.Intent)
	}

	if summary.Outcome != "Bug was fixed successfully" {
		t.Errorf("unexpected outcome: %s", summary.Outcome)
	}

	if len(summary.Learnings.Repo) != 1 || summary.Learnings.Repo[0] != "The repo uses Go modules" {
		t.Errorf("unexpected repo learnings: %v", summary.Learnings.Repo)
	}

	if len(summary.Learnings.Code) != 1 || summary.Learnings.Code[0].Path != "main.go" {
		t.Errorf("unexpected code learnings: %v", summary.Learnings.Code)
	}

	if len(summary.Friction) != 1 || summary.Friction[0] != "Slow CI pipeline" {
		t.Errorf("unexpected friction: %v", summary.Friction)
	}

	if len(summary.OpenItems) != 1 || summary.OpenItems[0] != "Add more tests" {
		t.Errorf("unexpected open items: %v", summary.OpenItems)
	}
}

func TestClaudeGenerator_MarkdownCodeBlock(t *testing.T) {
	gen := &ClaudeGenerator{
		CommandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			// Return summary wrapped in markdown code block - use literal newlines escaped in the JSON string
			response := `{"result":"` + "```json\\n{\\\"intent\\\":\\\"Test markdown extraction\\\",\\\"outcome\\\":\\\"Works\\\",\\\"learnings\\\":{\\\"repo\\\":[],\\\"code\\\":[],\\\"workflow\\\":[]},\\\"friction\\\":[],\\\"open_items\\\":[]}\\n```" + `"}`
			return exec.CommandContext(ctx, "sh", "-c", "printf '%s' '"+response+"'")
		},
	}

	input := Input{
		Transcript: []Entry{
			{Type: EntryTypeUser, Content: "Test"},
		},
	}

	summary, err := gen.Generate(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if summary.Intent != "Test markdown extraction" {
		t.Errorf("unexpected intent: %s", summary.Intent)
	}
}

func TestBuildSummarizationPrompt(t *testing.T) {
	transcriptText := "[User] Hello\n\n[Assistant] Hi"

	prompt := buildSummarizationPrompt(transcriptText)

	if !strings.Contains(prompt, "<transcript>") {
		t.Error("prompt should contain <transcript> tag")
	}

	if !strings.Contains(prompt, transcriptText) {
		t.Error("prompt should contain the transcript text")
	}

	if !strings.Contains(prompt, "</transcript>") {
		t.Error("prompt should contain </transcript> tag")
	}

	if !strings.Contains(prompt, `"intent"`) {
		t.Error("prompt should contain JSON schema example")
	}

	if !strings.Contains(prompt, "Return ONLY the JSON object") {
		t.Error("prompt should contain instruction for JSON-only output")
	}
}

func TestExtractJSONFromMarkdown(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "plain JSON",
			input:    `{"key": "value"}`,
			expected: `{"key": "value"}`,
		},
		{
			name:     "json code block",
			input:    "```json\n{\"key\": \"value\"}\n```",
			expected: `{"key": "value"}`,
		},
		{
			name:     "plain code block",
			input:    "```\n{\"key\": \"value\"}\n```",
			expected: `{"key": "value"}`,
		},
		{
			name:     "with whitespace",
			input:    "  \n```json\n{\"key\": \"value\"}\n```  \n",
			expected: `{"key": "value"}`,
		},
		{
			name:     "unclosed block",
			input:    "```json\n{\"key\": \"value\"}",
			expected: `{"key": "value"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractJSONFromMarkdown(tt.input)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}
