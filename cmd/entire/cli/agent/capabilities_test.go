package agent

import (
	"context"
	"io"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// --- Mock types for testing ---

// mockBaseAgent implements only agent.Agent (no optional interfaces).
type mockBaseAgent struct{}

func (m *mockBaseAgent) Name() types.AgentName                        { return "mock" }
func (m *mockBaseAgent) Type() types.AgentType                        { return "Mock" }
func (m *mockBaseAgent) Description() string                          { return "mock agent" }
func (m *mockBaseAgent) IsPreview() bool                              { return false }
func (m *mockBaseAgent) DetectPresence(context.Context) (bool, error) { return false, nil }
func (m *mockBaseAgent) ProtectedDirs() []string                      { return nil }
func (m *mockBaseAgent) ReadTranscript(string) ([]byte, error)        { return nil, nil }
func (m *mockBaseAgent) ChunkTranscript(context.Context, []byte, int) ([][]byte, error) {
	return nil, nil
}
func (m *mockBaseAgent) ReassembleTranscript([][]byte) ([]byte, error)     { return nil, nil }
func (m *mockBaseAgent) GetSessionID(*HookInput) string                    { return "" }
func (m *mockBaseAgent) GetSessionDir(string) (string, error)              { return "", nil }
func (m *mockBaseAgent) ResolveSessionFile(string, string) string          { return "" }
func (m *mockBaseAgent) ReadSession(*HookInput) (*AgentSession, error)     { return nil, nil } //nolint:nilnil // test mock
func (m *mockBaseAgent) WriteSession(context.Context, *AgentSession) error { return nil }
func (m *mockBaseAgent) FormatResumeCommand(string) string                 { return "" }

// mockBuiltinHookAgent is a built-in agent that implements HookSupport but NOT CapabilityDeclarer.
type mockBuiltinHookAgent struct {
	mockBaseAgent
}

func (m *mockBuiltinHookAgent) HookNames() []string { return nil }
func (m *mockBuiltinHookAgent) ParseHookEvent(context.Context, string, io.Reader) (*Event, error) {
	return nil, nil //nolint:nilnil // test mock
}
func (m *mockBuiltinHookAgent) InstallHooks(context.Context, bool, bool) (int, error) {
	return 0, nil
}
func (m *mockBuiltinHookAgent) UninstallHooks(context.Context) error   { return nil }
func (m *mockBuiltinHookAgent) AreHooksInstalled(context.Context) bool { return false }

// mockFullAgent implements all optional interfaces AND CapabilityDeclarer.
type mockFullAgent struct {
	mockBaseAgent

	caps DeclaredCaps
}

func (m *mockFullAgent) DeclaredCapabilities() DeclaredCaps { return m.caps }

// HookSupport
func (m *mockFullAgent) HookNames() []string { return nil }
func (m *mockFullAgent) ParseHookEvent(context.Context, string, io.Reader) (*Event, error) {
	return nil, nil //nolint:nilnil // test mock
}
func (m *mockFullAgent) InstallHooks(context.Context, bool, bool) (int, error) { return 0, nil }
func (m *mockFullAgent) UninstallHooks(context.Context) error                  { return nil }
func (m *mockFullAgent) AreHooksInstalled(context.Context) bool                { return false }

// TranscriptAnalyzer
func (m *mockFullAgent) GetTranscriptPosition(string) (int, error) { return 0, nil }
func (m *mockFullAgent) ExtractModifiedFilesFromOffset(string, int) ([]string, int, error) {
	return nil, 0, nil
}
func (m *mockFullAgent) ExtractPrompts(string, int) ([]string, error) { return nil, nil }
func (m *mockFullAgent) ExtractSummary(string) (string, error)        { return "", nil }

// TranscriptPreparer
func (m *mockFullAgent) PrepareTranscript(context.Context, string) error { return nil }

// TokenCalculator
func (m *mockFullAgent) CalculateTokenUsage([]byte, int) (*TokenUsage, error) { return nil, nil } //nolint:nilnil // test mock

// TextGenerator
func (m *mockFullAgent) GenerateText(context.Context, string, string) (string, error) {
	return "", nil
}

// HookResponseWriter
func (m *mockFullAgent) WriteHookResponse(string) error { return nil }

// SubagentAwareExtractor
func (m *mockFullAgent) ExtractAllModifiedFiles([]byte, int, string) ([]string, error) {
	return nil, nil
}
func (m *mockFullAgent) CalculateTotalTokenUsage([]byte, int, string) (*TokenUsage, error) {
	return nil, nil //nolint:nilnil // test mock
}

// --- Tests ---

func TestAsHookSupport(t *testing.T) {
	t.Parallel()

	t.Run("not implemented", func(t *testing.T) {
		t.Parallel()
		ag := &mockBaseAgent{}
		_, ok := AsHookSupport(ag)
		if ok {
			t.Error("expected false for agent not implementing HookSupport")
		}
	})

	t.Run("builtin agent", func(t *testing.T) {
		t.Parallel()
		ag := &mockBuiltinHookAgent{}
		hs, ok := AsHookSupport(ag)
		if !ok || hs == nil {
			t.Error("expected true for built-in agent implementing HookSupport")
		}
	})

	t.Run("declared true", func(t *testing.T) {
		t.Parallel()
		ag := &mockFullAgent{caps: DeclaredCaps{Hooks: true}}
		hs, ok := AsHookSupport(ag)
		if !ok || hs == nil {
			t.Error("expected true when capability declared true")
		}
	})

	t.Run("declared false", func(t *testing.T) {
		t.Parallel()
		ag := &mockFullAgent{caps: DeclaredCaps{Hooks: false}}
		_, ok := AsHookSupport(ag)
		if ok {
			t.Error("expected false when capability declared false")
		}
	})
}

func TestAsTranscriptAnalyzer(t *testing.T) {
	t.Parallel()

	t.Run("not implemented", func(t *testing.T) {
		t.Parallel()
		_, ok := AsTranscriptAnalyzer(&mockBaseAgent{})
		if ok {
			t.Error("expected false")
		}
	})

	t.Run("declared true", func(t *testing.T) {
		t.Parallel()
		ag := &mockFullAgent{caps: DeclaredCaps{TranscriptAnalyzer: true}}
		ta, ok := AsTranscriptAnalyzer(ag)
		if !ok || ta == nil {
			t.Error("expected true")
		}
	})

	t.Run("declared false", func(t *testing.T) {
		t.Parallel()
		ag := &mockFullAgent{caps: DeclaredCaps{TranscriptAnalyzer: false}}
		_, ok := AsTranscriptAnalyzer(ag)
		if ok {
			t.Error("expected false")
		}
	})
}

func TestAsTranscriptPreparer(t *testing.T) {
	t.Parallel()

	t.Run("not implemented", func(t *testing.T) {
		t.Parallel()
		_, ok := AsTranscriptPreparer(&mockBaseAgent{})
		if ok {
			t.Error("expected false")
		}
	})

	t.Run("declared true", func(t *testing.T) {
		t.Parallel()
		ag := &mockFullAgent{caps: DeclaredCaps{TranscriptPreparer: true}}
		tp, ok := AsTranscriptPreparer(ag)
		if !ok || tp == nil {
			t.Error("expected true")
		}
	})

	t.Run("declared false", func(t *testing.T) {
		t.Parallel()
		ag := &mockFullAgent{caps: DeclaredCaps{TranscriptPreparer: false}}
		_, ok := AsTranscriptPreparer(ag)
		if ok {
			t.Error("expected false")
		}
	})
}

func TestAsTokenCalculator(t *testing.T) {
	t.Parallel()

	t.Run("not implemented", func(t *testing.T) {
		t.Parallel()
		_, ok := AsTokenCalculator(&mockBaseAgent{})
		if ok {
			t.Error("expected false")
		}
	})

	t.Run("declared true", func(t *testing.T) {
		t.Parallel()
		ag := &mockFullAgent{caps: DeclaredCaps{TokenCalculator: true}}
		tc, ok := AsTokenCalculator(ag)
		if !ok || tc == nil {
			t.Error("expected true")
		}
	})

	t.Run("declared false", func(t *testing.T) {
		t.Parallel()
		ag := &mockFullAgent{caps: DeclaredCaps{TokenCalculator: false}}
		_, ok := AsTokenCalculator(ag)
		if ok {
			t.Error("expected false")
		}
	})
}

func TestAsTextGenerator(t *testing.T) {
	t.Parallel()

	t.Run("not implemented", func(t *testing.T) {
		t.Parallel()
		_, ok := AsTextGenerator(&mockBaseAgent{})
		if ok {
			t.Error("expected false")
		}
	})

	t.Run("declared true", func(t *testing.T) {
		t.Parallel()
		ag := &mockFullAgent{caps: DeclaredCaps{TextGenerator: true}}
		tg, ok := AsTextGenerator(ag)
		if !ok || tg == nil {
			t.Error("expected true")
		}
	})

	t.Run("declared false", func(t *testing.T) {
		t.Parallel()
		ag := &mockFullAgent{caps: DeclaredCaps{TextGenerator: false}}
		_, ok := AsTextGenerator(ag)
		if ok {
			t.Error("expected false")
		}
	})
}

func TestAsHookResponseWriter(t *testing.T) {
	t.Parallel()

	t.Run("not implemented", func(t *testing.T) {
		t.Parallel()
		_, ok := AsHookResponseWriter(&mockBaseAgent{})
		if ok {
			t.Error("expected false")
		}
	})

	t.Run("declared true", func(t *testing.T) {
		t.Parallel()
		ag := &mockFullAgent{caps: DeclaredCaps{HookResponseWriter: true}}
		hrw, ok := AsHookResponseWriter(ag)
		if !ok || hrw == nil {
			t.Error("expected true")
		}
	})

	t.Run("declared false", func(t *testing.T) {
		t.Parallel()
		ag := &mockFullAgent{caps: DeclaredCaps{HookResponseWriter: false}}
		_, ok := AsHookResponseWriter(ag)
		if ok {
			t.Error("expected false")
		}
	})
}

func TestAsSubagentAwareExtractor(t *testing.T) {
	t.Parallel()

	t.Run("not implemented", func(t *testing.T) {
		t.Parallel()
		_, ok := AsSubagentAwareExtractor(&mockBaseAgent{})
		if ok {
			t.Error("expected false")
		}
	})

	t.Run("declared true", func(t *testing.T) {
		t.Parallel()
		ag := &mockFullAgent{caps: DeclaredCaps{SubagentAwareExtractor: true}}
		sae, ok := AsSubagentAwareExtractor(ag)
		if !ok || sae == nil {
			t.Error("expected true")
		}
	})

	t.Run("declared false", func(t *testing.T) {
		t.Parallel()
		ag := &mockFullAgent{caps: DeclaredCaps{SubagentAwareExtractor: false}}
		_, ok := AsSubagentAwareExtractor(ag)
		if ok {
			t.Error("expected false")
		}
	})
}
