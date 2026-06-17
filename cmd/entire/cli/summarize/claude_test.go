package summarize

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

type stubTextGenerator struct {
	text string
	err  error
}

func (s *stubTextGenerator) GenerateText(context.Context, string, string) (string, error) {
	return s.text, s.err
}

func (s *stubTextGenerator) Name() types.AgentName { return "stub" }

func (s *stubTextGenerator) Type() types.AgentType { return "Stub" }

func (s *stubTextGenerator) Description() string { return "stub" }

func (s *stubTextGenerator) IsPreview() bool { return false }

func (s *stubTextGenerator) DetectPresence(context.Context) (bool, error) { return true, nil }

func (s *stubTextGenerator) ProtectedDirs() []string { return nil }

func (s *stubTextGenerator) ReadTranscript(string) ([]byte, error) { return nil, nil }

func (s *stubTextGenerator) ChunkTranscript(context.Context, []byte, int) ([][]byte, error) {
	return nil, nil
}

func (s *stubTextGenerator) ReassembleTranscript([][]byte) ([]byte, error) { return nil, nil }

func (s *stubTextGenerator) GetSessionID(*agent.HookInput) string { return "" }

func (s *stubTextGenerator) GetSessionDir(string) (string, error) { return "", nil }

func (s *stubTextGenerator) ResolveSessionFile(string, string) string { return "" }

func (s *stubTextGenerator) ReadSession(*agent.HookInput) (*agent.AgentSession, error) {
	return &agent.AgentSession{}, nil
}

func (s *stubTextGenerator) WriteSession(context.Context, *agent.AgentSession) error { return nil }

func (s *stubTextGenerator) FormatResumeCommand(string) string { return "" }

func TestClaudeGenerator_TextGeneratorError(t *testing.T) {
	t.Parallel()

	gen := &ClaudeGenerator{
		TextGenerator: &stubTextGenerator{err: context.DeadlineExceeded},
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

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got: %v", err)
	}
}

func TestClaudeGenerator_UsesDefaultTextGenerator(t *testing.T) {
	t.Parallel()

	originalFactory := defaultTextGeneratorFactory
	defaultTextGeneratorFactory = func() (agent.TextGenerator, error) {
		return &stubTextGenerator{
			text: `{"intent":"default intent","outcome":"default outcome","learnings":{"repo":[],"code":[],"workflow":[]},"friction":[],"open_items":[]}`,
		}, nil
	}
	t.Cleanup(func() {
		defaultTextGeneratorFactory = originalFactory
	})

	gen := &ClaudeGenerator{}
	input := Input{
		Transcript: []Entry{
			{Type: EntryTypeUser, Content: "Hello"},
		},
	}

	summary, err := gen.Generate(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summary.Intent != "default intent" {
		t.Fatalf("unexpected intent: %s", summary.Intent)
	}
	if summary.Outcome != "default outcome" {
		t.Fatalf("unexpected outcome: %s", summary.Outcome)
	}
}

func TestClaudeGenerator_ErrorCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		textOutput    string
		expectedError string
	}{
		{
			name:          "invalid JSON response",
			textOutput:    "not valid json",
			expectedError: "parse summary JSON",
		},
		{
			name:          "invalid summary JSON",
			textOutput:    "not a valid summary object",
			expectedError: "parse summary JSON",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gen := &ClaudeGenerator{
				TextGenerator: &stubTextGenerator{text: tt.textOutput},
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
	t.Parallel()

	gen := &ClaudeGenerator{
		TextGenerator: &stubTextGenerator{text: `{"intent":"User wanted to fix a bug","outcome":"Bug was fixed successfully","learnings":{"repo":["The repo uses Go modules"],"code":[{"path":"main.go","line":10,"finding":"Entry point"}],"workflow":["Run tests before committing"]},"friction":["Slow CI pipeline"],"open_items":["Add more tests"]}`},
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

	if len(summary.Learnings.Code) != 1 || summary.Learnings.Code[0].Path != testMainGoFile {
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
	t.Parallel()

	gen := &ClaudeGenerator{
		TextGenerator: &stubTextGenerator{text: "```json\n{\"intent\":\"Test markdown extraction\",\"outcome\":\"Works\",\"learnings\":{\"repo\":[],\"code\":[],\"workflow\":[]},\"friction\":[],\"open_items\":[]}\n```"},
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

func TestClaudeGenerator_GeneratedJSON(t *testing.T) {
	t.Parallel()

	gen := &ClaudeGenerator{
		TextGenerator: &stubTextGenerator{text: `{"intent":"Array response intent","outcome":"Array response outcome","learnings":{"repo":[],"code":[],"workflow":[]},"friction":[],"open_items":[]}`},
	}

	input := Input{
		Transcript: []Entry{
			{Type: EntryTypeUser, Content: "Summarize this"},
		},
	}

	summary, err := gen.Generate(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if summary.Intent != "Array response intent" {
		t.Errorf("unexpected intent: %s", summary.Intent)
	}

	if summary.Outcome != "Array response outcome" {
		t.Errorf("unexpected outcome: %s", summary.Outcome)
	}
}

func TestClaudeGenerator_InvalidGeneratedJSON(t *testing.T) {
	t.Parallel()

	gen := &ClaudeGenerator{
		TextGenerator: &stubTextGenerator{text: `[{"type":"system","subtype":"init"},{"type":"assistant","message":"Working on it"}]`},
	}

	input := Input{
		Transcript: []Entry{
			{Type: EntryTypeUser, Content: "Summarize this"},
		},
	}

	_, err := gen.Generate(context.Background(), input)
	if err == nil {
		t.Fatal("expected error")
	}

	if !strings.Contains(err.Error(), "parse summary JSON") {
		t.Errorf("expected parse error, got: %v", err)
	}
}

func TestBuildSummarizationPrompt(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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
			t.Parallel()
			result := extractJSONFromMarkdown(tt.input)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}
