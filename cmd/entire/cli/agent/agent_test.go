package agent

import (
	"io"
	"testing"
	"time"
)

const mockAgentName AgentName = "mock" // Used by mock implementations
const mockAgentType AgentType = "Mock Agent"

// mockAgent is a minimal implementation of Agent for testing interface compliance.
type mockAgent struct{}

var _ Agent = (*mockAgent)(nil) // Compile-time interface check

func (m *mockAgent) Name() AgentName               { return mockAgentName }
func (m *mockAgent) Type() AgentType               { return mockAgentType }
func (m *mockAgent) Description() string           { return "Mock agent for testing" }
func (m *mockAgent) DetectPresence() (bool, error) { return false, nil }
func (m *mockAgent) GetHookConfigPath() string     { return "" }
func (m *mockAgent) SupportsHooks() bool           { return false }

//nolint:nilnil // Mock implementation
func (m *mockAgent) ParseHookInput(_ HookType, _ io.Reader) (*HookInput, error) {
	return nil, nil
}
func (m *mockAgent) GetSessionID(_ *HookInput) string             { return "" }
func (m *mockAgent) TransformSessionID(agentID string) string     { return agentID }
func (m *mockAgent) ExtractAgentSessionID(entireID string) string { return entireID }
func (m *mockAgent) GetSessionDir(_ string) (string, error)       { return "", nil }

//nolint:nilnil // Mock implementation
func (m *mockAgent) ReadSession(_ *HookInput) (*AgentSession, error) { return nil, nil }
func (m *mockAgent) WriteSession(_ *AgentSession) error              { return nil }
func (m *mockAgent) FormatResumeCommand(_ string) string             { return "" }
func (m *mockAgent) IsInstalled() (bool, error)                      { return false, nil }
func (m *mockAgent) InstallURL() string                               { return "" }

// mockHookSupport implements both Agent and HookSupport interfaces.
type mockHookSupport struct {
	mockAgent
}

var _ HookSupport = (*mockHookSupport)(nil) // Compile-time interface check

func (m *mockHookSupport) InstallHooks(_, _ bool) (int, error) { return 0, nil }
func (m *mockHookSupport) UninstallHooks() error               { return nil }
func (m *mockHookSupport) AreHooksInstalled() bool             { return false }
func (m *mockHookSupport) GetSupportedHooks() []HookType       { return nil }

// mockFileWatcher implements both Agent and FileWatcher interfaces.
type mockFileWatcher struct {
	mockAgent
}

var _ FileWatcher = (*mockFileWatcher)(nil) // Compile-time interface check

func (m *mockFileWatcher) GetWatchPaths() ([]string, error) { return nil, nil }

//nolint:nilnil // Mock implementation
func (m *mockFileWatcher) OnFileChange(_ string) (*SessionChange, error) { return nil, nil }

func TestAgentInterfaceCompliance(t *testing.T) {
	t.Run("Agent interface can be implemented", func(t *testing.T) {
		var agent Agent = &mockAgent{}
		if agent.Name() != mockAgentName {
			t.Errorf("expected Name() to return %q, got %q", mockAgentName, agent.Name())
		}
	})

	t.Run("HookSupport embeds Agent", func(t *testing.T) {
		var hookSupport HookSupport = &mockHookSupport{}
		// HookSupport should satisfy Agent interface
		var agent Agent = hookSupport
		if agent.Name() != mockAgentName {
			t.Errorf("expected Name() to return %q, got %q", mockAgentName, agent.Name())
		}
	})

	t.Run("FileWatcher embeds Agent", func(t *testing.T) {
		var fileWatcher FileWatcher = &mockFileWatcher{}
		// FileWatcher should satisfy Agent interface
		var agent Agent = fileWatcher
		if agent.Name() != mockAgentName {
			t.Errorf("expected Name() to return %q, got %q", mockAgentName, agent.Name())
		}
	})
}

func TestHookTypeConstants(t *testing.T) {
	tests := []struct {
		hookType HookType
		expected string
	}{
		{HookSessionStart, "session_start"},
		{HookUserPromptSubmit, "user_prompt_submit"},
		{HookStop, "stop"},
		{HookPreToolUse, "pre_tool_use"},
		{HookPostToolUse, "post_tool_use"},
	}

	for _, tt := range tests {
		t.Run(string(tt.hookType), func(t *testing.T) {
			if string(tt.hookType) != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, string(tt.hookType))
			}
		})
	}
}

func TestEntryTypeConstants(t *testing.T) {
	tests := []struct {
		entryType EntryType
		expected  string
	}{
		{EntryUser, "user"},
		{EntryAssistant, "assistant"},
		{EntryTool, "tool"},
		{EntrySystem, "system"},
	}

	for _, tt := range tests {
		t.Run(string(tt.entryType), func(t *testing.T) {
			if string(tt.entryType) != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, string(tt.entryType))
			}
		})
	}
}

//nolint:govet // testing struct field assignment
func TestHookInputStructure(t *testing.T) {
	input := HookInput{
		HookType:     HookPreToolUse,
		SessionID:    "test-session",
		SessionRef:   "/path/to/session",
		Timestamp:    time.Now(),
		ToolName:     "Write",
		ToolUseID:    "tool-123",
		ToolInput:    []byte(`{"file_path": "test.go"}`),
		ToolResponse: nil,
		RawData:      map[string]interface{}{"extra": "data"},
	}

	if input.HookType != HookPreToolUse {
		t.Errorf("expected HookType %q, got %q", HookPreToolUse, input.HookType)
	}
	if input.SessionID != "test-session" {
		t.Errorf("expected SessionID %q, got %q", "test-session", input.SessionID)
	}
}

//nolint:govet // testing struct field assignment
func TestSessionChangeStructure(t *testing.T) {
	change := SessionChange{
		SessionID:  "test-session",
		SessionRef: "/path/to/session",
		EventType:  HookSessionStart,
		Timestamp:  time.Now(),
	}

	if change.SessionID != "test-session" {
		t.Errorf("expected SessionID %q, got %q", "test-session", change.SessionID)
	}
	if change.EventType != HookSessionStart {
		t.Errorf("expected EventType %q, got %q", HookSessionStart, change.EventType)
	}
}
