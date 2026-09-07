package agent

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/stretchr/testify/require"
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
func (m *mockBuiltinHookAgent) InstallHooks(context.Context, bool) (int, error) {
	return 0, nil
}
func (m *mockBuiltinHookAgent) UninstallHooks(context.Context) error            { return nil }
func (m *mockBuiltinHookAgent) AreHooksInstalled(context.Context) (bool, error) { return false, nil }

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
func (m *mockFullAgent) InstallHooks(context.Context, bool) (int, error) { return 0, nil }
func (m *mockFullAgent) UninstallHooks(context.Context) error            { return nil }
func (m *mockFullAgent) AreHooksInstalled(context.Context) (bool, error) { return false, nil }

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

// ModelExtractor
func (m *mockFullAgent) ExtractModel([]byte) (string, error) { return "mock-model", nil }

// TextGenerator
func (m *mockFullAgent) GenerateText(context.Context, string, string) (string, error) {
	return "", nil
}

// StreamingTextGenerator
func (m *mockFullAgent) GenerateTextStreaming(context.Context, string, string, ProgressFn) (string, error) {
	return "", nil
}

// TranscriptCompactor
func (m *mockFullAgent) CompactTranscript(context.Context, string) (*CompactedTranscript, error) {
	return &CompactedTranscript{}, nil
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

// mockBuiltinStreamingAgent is a built-in agent that implements StreamingTextGenerator but NOT CapabilityDeclarer.
type mockBuiltinStreamingAgent struct {
	mockBaseAgent
}

func (m *mockBuiltinStreamingAgent) GenerateTextStreaming(context.Context, string, string, ProgressFn) (string, error) {
	return "", nil
}

// mockBuiltinPromptAgent is a built-in agent that implements PromptExtractor but NOT CapabilityDeclarer.
type mockBuiltinPromptAgent struct {
	mockBaseAgent
}

func (m *mockBuiltinPromptAgent) ExtractPrompts(string, int) ([]string, error) {
	return []string{"test prompt"}, nil
}

// mockHookTimeoutAgent is a built-in agent that implements HookTimeoutProvider
// but NOT CapabilityDeclarer, mirroring how CodexAgent implements it: an
// override for one named event, zero (no override) for everything else.
type mockHookTimeoutAgent struct {
	mockBaseAgent

	overrideEvent string
	overrideValue time.Duration
}

func (m *mockHookTimeoutAgent) HookTimeout(hookName string) time.Duration {
	if hookName == m.overrideEvent {
		return m.overrideValue
	}
	return 0
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

func TestAsModelExtractor(t *testing.T) {
	t.Parallel()

	// AsModelExtractor is an ungated, built-in-only helper (no DeclaredCaps
	// field): implementing the interface is sufficient.
	t.Run("not implemented", func(t *testing.T) {
		t.Parallel()
		_, ok := AsModelExtractor(&mockBaseAgent{})
		if ok {
			t.Error("expected false")
		}
	})

	t.Run("implemented", func(t *testing.T) {
		t.Parallel()
		me, ok := AsModelExtractor(&mockFullAgent{})
		if !ok || me == nil {
			t.Error("expected true")
		}
	})

	t.Run("nil agent", func(t *testing.T) {
		t.Parallel()
		_, ok := AsModelExtractor(nil)
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

func TestAsTranscriptCompactor(t *testing.T) {
	t.Parallel()

	t.Run("not implemented", func(t *testing.T) {
		t.Parallel()
		_, ok := AsTranscriptCompactor(&mockBaseAgent{})
		if ok {
			t.Error("expected false")
		}
	})

	t.Run("declared true", func(t *testing.T) {
		t.Parallel()
		ag := &mockFullAgent{caps: DeclaredCaps{CompactTranscript: true}}
		tc, ok := AsTranscriptCompactor(ag)
		if !ok || tc == nil {
			t.Error("expected true")
		}
	})

	t.Run("declared false", func(t *testing.T) {
		t.Parallel()
		ag := &mockFullAgent{caps: DeclaredCaps{CompactTranscript: false}}
		_, ok := AsTranscriptCompactor(ag)
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

func TestAsPromptExtractor(t *testing.T) {
	t.Parallel()

	t.Run("not implemented", func(t *testing.T) {
		t.Parallel()
		_, ok := AsPromptExtractor(&mockBaseAgent{})
		if ok {
			t.Error("expected false for agent not implementing PromptExtractor")
		}
	})

	t.Run("builtin agent", func(t *testing.T) {
		t.Parallel()
		ag := &mockBuiltinPromptAgent{}
		pe, ok := AsPromptExtractor(ag)
		if !ok || pe == nil {
			t.Error("expected true for built-in agent implementing PromptExtractor")
		}
	})

	t.Run("declared with TranscriptAnalyzer true", func(t *testing.T) {
		t.Parallel()
		ag := &mockFullAgent{caps: DeclaredCaps{TranscriptAnalyzer: true}}
		pe, ok := AsPromptExtractor(ag)
		if !ok || pe == nil {
			t.Error("expected true when TranscriptAnalyzer capability declared true")
		}
	})

	t.Run("declared with TranscriptAnalyzer false", func(t *testing.T) {
		t.Parallel()
		ag := &mockFullAgent{caps: DeclaredCaps{TranscriptAnalyzer: false}}
		_, ok := AsPromptExtractor(ag)
		if ok {
			t.Error("expected false when TranscriptAnalyzer capability declared false")
		}
	})

	t.Run("nil agent", func(t *testing.T) {
		t.Parallel()
		_, ok := AsPromptExtractor(nil)
		if ok {
			t.Error("expected false for nil agent")
		}
	})
}

func TestAsStreamingTextGenerator(t *testing.T) {
	t.Parallel()

	t.Run("not implemented", func(t *testing.T) {
		t.Parallel()
		ag := &mockBaseAgent{}
		_, ok := AsStreamingTextGenerator(ag)
		if ok {
			t.Error("expected false for agent not implementing StreamingTextGenerator")
		}
	})

	t.Run("builtin agent", func(t *testing.T) {
		t.Parallel()
		ag := &mockBuiltinStreamingAgent{}
		stg, ok := AsStreamingTextGenerator(ag)
		if !ok || stg == nil {
			t.Error("expected true for built-in agent implementing StreamingTextGenerator")
		}
	})

	t.Run("declared true", func(t *testing.T) {
		t.Parallel()
		ag := &mockFullAgent{caps: DeclaredCaps{StreamingTextGenerator: true}}
		stg, ok := AsStreamingTextGenerator(ag)
		if !ok || stg == nil {
			t.Error("expected true when capability declared true")
		}
	})

	t.Run("declared false", func(t *testing.T) {
		t.Parallel()
		ag := &mockFullAgent{caps: DeclaredCaps{StreamingTextGenerator: false}}
		_, ok := AsStreamingTextGenerator(ag)
		if ok {
			t.Error("expected false when capability declared false")
		}
	})
}

func TestAsHookTimeoutProvider(t *testing.T) {
	t.Parallel()

	t.Run("not implemented", func(t *testing.T) {
		t.Parallel()
		_, ok := AsHookTimeoutProvider(&mockBaseAgent{})
		if ok {
			t.Error("expected false for agent not implementing HookTimeoutProvider")
		}
	})

	t.Run("builtin agent", func(t *testing.T) {
		t.Parallel()
		ag := &mockHookTimeoutAgent{overrideEvent: "stop", overrideValue: 5 * time.Second}
		htp, ok := AsHookTimeoutProvider(ag)
		if !ok || htp == nil {
			t.Error("expected true for built-in agent implementing HookTimeoutProvider")
		}
	})
}

// TestHookTimeoutFor is REQUIREMENT 2 from the differentiated-hook-timeout
// task: an agent implementing HookTimeoutProvider gets its own declared value
// honored for the event it overrides, and every other agent/event still
// resolves to the flat default — proving the mechanism differentiates
// without silently applying an override everywhere.
//
// Not parallel: it mutates the process-global default via
// SetDefaultHookTimeoutForTesting.
func TestHookTimeoutFor(t *testing.T) {
	restore := SetDefaultHookTimeoutForTesting(7 * time.Second)
	defer restore()

	t.Run("agent without the capability always gets the default", func(t *testing.T) {
		ag := &mockBaseAgent{}
		require.Equal(t, DefaultHookTimeout(), HookTimeoutFor(ag, "stop"))
		require.Equal(t, DefaultHookTimeout(), HookTimeoutFor(ag, "session-end"))
	})

	t.Run("agent with the capability gets its own value for the overridden event", func(t *testing.T) {
		ag := &mockHookTimeoutAgent{overrideEvent: "session-end", overrideValue: 3 * time.Second}

		require.Equal(t, 3*time.Second, HookTimeoutFor(ag, "session-end"),
			"the overridden event must get the agent's own value, not the default")
		require.Equal(t, DefaultHookTimeout(), HookTimeoutFor(ag, "stop"),
			"an event the agent does not override must still fall back to the default")
	})

	t.Run("a non-positive override falls back to the default rather than disabling the timeout", func(t *testing.T) {
		ag := &mockHookTimeoutAgent{overrideEvent: "stop", overrideValue: 0}
		require.Equal(t, DefaultHookTimeout(), HookTimeoutFor(ag, "stop"))
	})
}

// TestSetDefaultHookTimeoutForTesting pins the test seam DefaultHookTimeout
// needs: production's flat default is 30s (matching Codex's own
// defaultHookTimeoutSec), and nothing should have to wait out a real 30s to
// exercise the timeout mechanism.
func TestSetDefaultHookTimeoutForTesting(t *testing.T) {
	require.Equal(t, 30*time.Second, DefaultHookTimeout(), "production default must stay 30s")

	restore := SetDefaultHookTimeoutForTesting(50 * time.Millisecond)
	require.Equal(t, 50*time.Millisecond, DefaultHookTimeout())

	restore()
	require.Equal(t, 30*time.Second, DefaultHookTimeout(), "restore must put the real default back")
}
