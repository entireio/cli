// Package opencode implements the Agent interface for OpenCode.
package opencode

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"entire.io/cli/cmd/entire/cli/agent"
	"entire.io/cli/cmd/entire/cli/paths"
)

//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
	agent.Register(agent.AgentNameOpenCode, NewOpenCodeAgent)
}

// OpenCodeAgent implements the Agent interface for OpenCode.
//
//nolint:revive // OpenCodeAgent is clearer than Agent in this context
type OpenCodeAgent struct{}

// NewOpenCodeAgent creates a new OpenCode agent instance.
//
//nolint:ireturn // Factory pattern requires returning the interface
func NewOpenCodeAgent() agent.Agent {
	return &OpenCodeAgent{}
}

// Name returns the agent identifier.
func (o *OpenCodeAgent) Name() string {
	return agent.AgentNameOpenCode
}

// Description returns a human-readable description.
func (o *OpenCodeAgent) Description() string {
	return "OpenCode - Multi-agent AI coding CLI"
}

// DetectPresence checks if OpenCode is configured in the repository.
func (o *OpenCodeAgent) DetectPresence() (bool, error) {
	// Check for .opencode directory
	if _, err := os.Stat(".opencode"); err == nil {
		return true, nil
	}
	// Check for opencode.json
	if _, err := os.Stat("opencode.json"); err == nil {
		return true, nil
	}
	return false, nil
}

// GetHookConfigPath returns the path to OpenCode's hook config file.
func (o *OpenCodeAgent) GetHookConfigPath() string {
	return "opencode.json"
}

// SupportsHooks returns true as OpenCode supports lifecycle hooks via plugin.
func (o *OpenCodeAgent) SupportsHooks() bool {
	return true
}

// HookInput represents the JSON input from OpenCode plugin.
type HookInput struct {
	SessionID              string          `json:"session_id"`
	SessionRef             string          `json:"session_ref"`
	Timestamp              string          `json:"timestamp"`
	ToolName               string          `json:"tool_name,omitempty"`
	ToolUseID              string          `json:"tool_use_id,omitempty"`
	ToolInput              json.RawMessage `json:"tool_input,omitempty"`
	ToolResponse           json.RawMessage `json:"tool_response,omitempty"`
	TranscriptPath         string          `json:"transcript_path,omitempty"`
	SubagentTranscriptPath string          `json:"subagent_transcript_path,omitempty"`
}

// ParseHookInput parses OpenCode hook input from stdin.
func (o *OpenCodeAgent) ParseHookInput(hookType agent.HookType, reader io.Reader) (*agent.HookInput, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read input: %w", err)
	}

	if len(data) == 0 {
		return nil, errors.New("empty input")
	}

	var openCodeInput HookInput
	if err := json.Unmarshal(data, &openCodeInput); err != nil {
		return nil, fmt.Errorf("failed to parse JSON: %w", err)
	}

	// Parse timestamp, defaulting to current time on empty or invalid input
	timestamp, err := time.Parse(time.RFC3339, openCodeInput.Timestamp)
	if err != nil {
		timestamp = time.Now()
	}

	// Create normalized HookInput
	input := &agent.HookInput{
		HookType:   hookType,
		SessionID:  openCodeInput.SessionID,
		SessionRef: openCodeInput.SessionRef,
		Timestamp:  timestamp,
		ToolName:   openCodeInput.ToolName,
		ToolUseID:  openCodeInput.ToolUseID,
		ToolInput:  openCodeInput.ToolInput,
		RawData: map[string]any{
			"transcript_path":          openCodeInput.TranscriptPath,
			"subagent_transcript_path": openCodeInput.SubagentTranscriptPath,
		},
	}

	return input, nil
}

// GetSessionID extracts the session ID from hook input.
func (o *OpenCodeAgent) GetSessionID(input *agent.HookInput) string {
	return input.SessionID
}

// TransformSessionID converts an OpenCode session ID to an Entire session ID.
// Format: YYYY-MM-DD-<opencode-session-id>
func (o *OpenCodeAgent) TransformSessionID(agentSessionID string) string {
	return paths.EntireSessionID(agentSessionID)
}

// ExtractAgentSessionID extracts the OpenCode session ID from an Entire session ID.
func (o *OpenCodeAgent) ExtractAgentSessionID(entireSessionID string) string {
	// Expected format: YYYY-MM-DD-<agent-session-id> (11 chars prefix: "2025-12-02-")
	if len(entireSessionID) > 11 && entireSessionID[4] == '-' && entireSessionID[7] == '-' && entireSessionID[10] == '-' {
		return entireSessionID[11:]
	}
	// Return as-is if not in expected format (backwards compatibility)
	return entireSessionID
}

// GetSessionDir returns the directory where OpenCode exports session transcripts.
// OpenCode plugin exports transcripts to .entire/opencode/sessions/
func (o *OpenCodeAgent) GetSessionDir(repoPath string) (string, error) {
	return filepath.Join(repoPath, ".entire", "opencode", "sessions"), nil
}

// ReadSession reads a session from OpenCode's exported transcript.
// The session data is stored in NativeData as raw JSONL bytes.
func (o *OpenCodeAgent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
	// Get transcript path from RawData
	transcriptPath, ok := input.RawData["transcript_path"].(string)
	if !ok || transcriptPath == "" {
		return nil, errors.New("transcript path not found in hook input")
	}

	// Read the raw JSONL file
	data, err := os.ReadFile(transcriptPath) //nolint:gosec // path comes from trusted hook input
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}

	return &agent.AgentSession{
		SessionID:     input.SessionID,
		AgentName:     o.Name(),
		SessionRef:    transcriptPath,
		StartTime:     input.Timestamp,
		NativeData:    data,
		ModifiedFiles: []string{}, // TODO: Extract from OpenCode transcript
	}, nil
}

// WriteSession writes a session to OpenCode's storage.
// For OpenCode, this is handled by the plugin, so this is a no-op.
func (o *OpenCodeAgent) WriteSession(session *agent.AgentSession) error {
	if session == nil {
		return errors.New("session is nil")
	}

	// Verify this session belongs to OpenCode
	if session.AgentName != "" && session.AgentName != o.Name() {
		return fmt.Errorf("session belongs to agent %q, not %q", session.AgentName, o.Name())
	}

	// OpenCode plugin handles transcript exports, so nothing to do here
	return nil
}

// FormatResumeCommand returns the command to resume an OpenCode session.
func (o *OpenCodeAgent) FormatResumeCommand(sessionID string) string {
	return "opencode --session " + sessionID
}

// GetHookNames returns the hook verbs OpenCode supports.
// These become subcommands: entire hooks opencode <verb>
func (o *OpenCodeAgent) GetHookNames() []string {
	return []string{
		HookNameSessionStart,
		HookNameStop,
	}
}
