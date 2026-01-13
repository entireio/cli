// Package geminicli implements the Agent interface for Gemini CLI.
package geminicli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"entire.io/cli/cmd/entire/cli/agent"
	"entire.io/cli/cmd/entire/cli/paths"
)

//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
	agent.Register(agent.AgentNameGemini, NewGeminiCLIAgent)
}

// GeminiCLIAgent implements the Agent interface for Gemini CLI.
//
//nolint:revive // GeminiCLIAgent is clearer than Agent in this context
type GeminiCLIAgent struct{}

func NewGeminiCLIAgent() agent.Agent {
	return &GeminiCLIAgent{}
}

// Name returns the agent identifier.
func (g *GeminiCLIAgent) Name() string {
	return agent.AgentNameGemini
}

// Description returns a human-readable description.
func (g *GeminiCLIAgent) Description() string {
	return "Gemini CLI - Google's AI coding assistant"
}

// DetectPresence checks if Gemini CLI is configured in the repository.
func (g *GeminiCLIAgent) DetectPresence() (bool, error) {
	// Check for .gemini directory
	if _, err := os.Stat(".gemini"); err == nil {
		return true, nil
	}
	// Check for .gemini/settings.json
	if _, err := os.Stat(".gemini/settings.json"); err == nil {
		return true, nil
	}
	return false, nil
}

// GetHookConfigPath returns the path to Gemini's hook config file.
func (g *GeminiCLIAgent) GetHookConfigPath() string {
	return ".gemini/settings.json"
}

// SupportsHooks returns true as Gemini CLI supports lifecycle hooks.
func (g *GeminiCLIAgent) SupportsHooks() bool {
	return true
}

// ParseHookInput parses Gemini CLI hook input from stdin.
func (g *GeminiCLIAgent) ParseHookInput(hookType agent.HookType, reader io.Reader) (*agent.HookInput, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read input: %w", err)
	}

	if len(data) == 0 {
		return nil, errors.New("empty input")
	}

	input := &agent.HookInput{
		HookType:  hookType,
		Timestamp: time.Now(),
		RawData:   make(map[string]interface{}),
	}

	// Parse based on hook type
	switch hookType {
	case agent.HookSessionStart, agent.HookStop:
		var raw sessionInfoRaw
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("failed to parse session info: %w", err)
		}
		input.SessionID = raw.SessionID
		input.SessionRef = raw.TranscriptPath
		// Store Gemini-specific fields in RawData
		input.RawData["cwd"] = raw.Cwd
		input.RawData["hook_event_name"] = raw.HookEventName
		if raw.Source != "" {
			input.RawData["source"] = raw.Source
		}
		if raw.Reason != "" {
			input.RawData["reason"] = raw.Reason
		}

	case agent.HookUserPromptSubmit:
		// BeforeAgent is Gemini's equivalent to Claude's UserPromptSubmit
		// It provides the user's prompt in the "prompt" field
		var raw agentHookInputRaw
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("failed to parse agent hook input: %w", err)
		}
		input.SessionID = raw.SessionID
		input.SessionRef = raw.TranscriptPath
		input.RawData["cwd"] = raw.Cwd
		input.RawData["hook_event_name"] = raw.HookEventName
		if raw.Prompt != "" {
			input.UserPrompt = raw.Prompt
			input.RawData["prompt"] = raw.Prompt
		}

	case agent.HookPreToolUse, agent.HookPostToolUse:
		var raw toolHookInputRaw
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("failed to parse tool hook input: %w", err)
		}
		input.SessionID = raw.SessionID
		input.SessionRef = raw.TranscriptPath
		input.ToolName = raw.ToolName
		input.ToolInput = raw.ToolInput
		if hookType == agent.HookPostToolUse {
			input.ToolResponse = raw.ToolResponse
		}
		input.RawData["cwd"] = raw.Cwd
		input.RawData["hook_event_name"] = raw.HookEventName
	}

	return input, nil
}

// GetSessionID extracts the session ID from hook input.
func (g *GeminiCLIAgent) GetSessionID(input *agent.HookInput) string {
	return input.SessionID
}

// TransformSessionID converts a Gemini session ID to an Entire session ID.
// Format: YYYY-MM-DD-<gemini-session-id>
func (g *GeminiCLIAgent) TransformSessionID(agentSessionID string) string {
	return paths.EntireSessionID(agentSessionID)
}

// ExtractAgentSessionID extracts the Gemini session ID from an Entire session ID.
func (g *GeminiCLIAgent) ExtractAgentSessionID(entireSessionID string) string {
	// Expected format: YYYY-MM-DD-<agent-session-id> (11 chars prefix: "2025-12-02-")
	if len(entireSessionID) > 11 && entireSessionID[4] == '-' && entireSessionID[7] == '-' && entireSessionID[10] == '-' {
		return entireSessionID[11:]
	}
	// Return as-is if not in expected format (backwards compatibility)
	return entireSessionID
}

// GetSessionDir returns the directory where Gemini stores session transcripts.
// Gemini stores sessions in ~/.gemini/tmp/<project-hash>/chats/
func (g *GeminiCLIAgent) GetSessionDir(repoPath string) (string, error) {
	// Check for test environment override
	if override := os.Getenv("ENTIRE_TEST_GEMINI_PROJECT_DIR"); override != "" {
		return override, nil
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}

	// Gemini uses a hash of the project path for the directory name
	projectDir := SanitizePathForGemini(repoPath)
	return filepath.Join(homeDir, ".gemini", "tmp", projectDir, "chats"), nil
}

// ReadSession reads a session from Gemini's storage (JSON transcript file).
// The session data is stored in NativeData as raw JSON bytes.
func (g *GeminiCLIAgent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
	if input.SessionRef == "" {
		return nil, errors.New("session reference (transcript path) is required")
	}

	// Read the raw JSON file
	data, err := os.ReadFile(input.SessionRef)
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}

	// Parse to extract computed fields
	modifiedFiles, err := ExtractModifiedFiles(data)
	if err != nil {
		// Non-fatal: we can still return the session without modified files
		modifiedFiles = nil
	}

	return &agent.AgentSession{
		SessionID:     input.SessionID,
		AgentName:     g.Name(),
		SessionRef:    input.SessionRef,
		StartTime:     time.Now(),
		NativeData:    data,
		ModifiedFiles: modifiedFiles,
	}, nil
}

// WriteSession writes a session to Gemini's storage (JSON transcript file).
// Uses the NativeData field which contains raw JSON bytes.
func (g *GeminiCLIAgent) WriteSession(session *agent.AgentSession) error {
	if session == nil {
		return errors.New("session is nil")
	}

	// Verify this session belongs to Gemini CLI
	if session.AgentName != "" && session.AgentName != g.Name() {
		return fmt.Errorf("session belongs to agent %q, not %q", session.AgentName, g.Name())
	}

	if session.SessionRef == "" {
		return errors.New("session reference (transcript path) is required")
	}

	if len(session.NativeData) == 0 {
		return errors.New("session has no native data to write")
	}

	// Write the raw JSON data
	if err := os.WriteFile(session.SessionRef, session.NativeData, 0o600); err != nil {
		return fmt.Errorf("failed to write transcript: %w", err)
	}

	return nil
}

// FormatResumeCommand returns the command to resume a Gemini CLI session.
func (g *GeminiCLIAgent) FormatResumeCommand(sessionID string) string {
	return "gemini --resume " + sessionID
}

// SanitizePathForGemini converts a path to Gemini's project directory format.
// Gemini uses a hash-like sanitization similar to Claude.
var nonAlphanumericRegex = regexp.MustCompile(`[^a-zA-Z0-9]`)

func SanitizePathForGemini(path string) string {
	return nonAlphanumericRegex.ReplaceAllString(path, "-")
}
