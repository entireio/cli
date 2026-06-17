package summarize

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

type mockTextGenerator struct {
	prompt string
	model  string
	result string
}

func (m *mockTextGenerator) Name() types.AgentName                        { return "mock" }
func (m *mockTextGenerator) Type() types.AgentType                        { return "Mock" }
func (m *mockTextGenerator) Description() string                          { return "mock" }
func (m *mockTextGenerator) IsPreview() bool                              { return false }
func (m *mockTextGenerator) DetectPresence(context.Context) (bool, error) { return false, nil }
func (m *mockTextGenerator) ProtectedDirs() []string                      { return nil }
func (m *mockTextGenerator) ReadTranscript(string) ([]byte, error)        { return nil, nil }
func (m *mockTextGenerator) ChunkTranscript(context.Context, []byte, int) ([][]byte, error) {
	return nil, nil
}
func (m *mockTextGenerator) ReassembleTranscript([][]byte) ([]byte, error) { return nil, nil }
func (m *mockTextGenerator) GetSessionID(*agent.HookInput) string          { return "" }
func (m *mockTextGenerator) GetSessionDir(string) (string, error)          { return "", nil }
func (m *mockTextGenerator) ResolveSessionFile(string, string) string      { return "" }
func (m *mockTextGenerator) ReadSession(*agent.HookInput) (*agent.AgentSession, error) {
	return nil, nil //nolint:nilnil // test stub
}
func (m *mockTextGenerator) WriteSession(context.Context, *agent.AgentSession) error { return nil }
func (m *mockTextGenerator) FormatResumeCommand(string) string                       { return "" }
func (m *mockTextGenerator) GenerateText(_ context.Context, prompt string, model string) (string, error) {
	m.prompt = prompt
	m.model = model
	return m.result, nil
}

type errorTextGenerator struct {
	mockTextGenerator

	err error
}

func (e *errorTextGenerator) GenerateText(context.Context, string, string) (string, error) {
	return "", e.err
}

func TestTextGeneratorAdapter_NilTextGenerator(t *testing.T) {
	t.Parallel()

	generator := &TextGeneratorAdapter{
		TextGenerator: nil,
		Model:         "test-model",
	}

	_, err := generator.Generate(context.Background(), Input{
		Transcript: []Entry{{Type: EntryTypeUser, Content: "test"}},
	})
	if err == nil {
		t.Fatal("expected error for nil TextGenerator")
	}
	if !strings.Contains(err.Error(), "text generator not configured") {
		t.Fatalf("error = %q, want it to contain %q", err.Error(), "text generator not configured")
	}
}

func TestTextGeneratorAdapter_GenerateError(t *testing.T) {
	t.Parallel()

	mock := &mockTextGenerator{}
	generator := &TextGeneratorAdapter{
		TextGenerator: mock,
		Model:         "test-model",
	}

	// Override GenerateText to return an error by using a different mock approach
	errMock := &errorTextGenerator{err: errors.New("provider auth failed")}
	generator.TextGenerator = errMock

	_, err := generator.Generate(context.Background(), Input{
		Transcript: []Entry{{Type: EntryTypeUser, Content: "test"}},
	})
	if err == nil {
		t.Fatal("expected error from GenerateText")
	}
	if !strings.Contains(err.Error(), "provider text generation failed") {
		t.Fatalf("error = %q, want it to contain wrapper message", err.Error())
	}
	if !errors.Is(err, errMock.err) {
		t.Fatalf("error chain should include original error")
	}
}

func TestTextGeneratorAdapter_Generate(t *testing.T) {
	t.Parallel()

	mock := &mockTextGenerator{
		result: "```json\n{\"intent\":\"Intent\",\"outcome\":\"Outcome\",\"learnings\":{\"repo\":[],\"code\":[],\"workflow\":[]},\"friction\":[],\"open_items\":[]}\n```",
	}

	generator := &TextGeneratorAdapter{
		TextGenerator: mock,
		Model:         "test-model",
	}

	summary, err := generator.Generate(context.Background(), Input{
		Transcript: []Entry{
			{Type: EntryTypeUser, Content: "Fix the bug"},
			{Type: EntryTypeAssistant, Content: "I fixed it"},
		},
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	if summary.Intent != "Intent" {
		t.Fatalf("summary.Intent = %q, want %q", summary.Intent, "Intent")
	}
	if mock.model != "test-model" {
		t.Fatalf("GenerateText model = %q, want %q", mock.model, "test-model")
	}
	if !strings.Contains(mock.prompt, "Fix the bug") {
		t.Fatalf("prompt did not include condensed transcript: %q", mock.prompt)
	}
}
