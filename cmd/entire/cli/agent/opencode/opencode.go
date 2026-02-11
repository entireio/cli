// Package opencode implements the Agent interface for OpenCode.
package opencode

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/sessionid"
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
func NewOpenCodeAgent() agent.Agent {
	return &OpenCodeAgent{}
}

// Name returns the agent registry key.
func (o *OpenCodeAgent) Name() agent.AgentName {
	return agent.AgentNameOpenCode
}

// Type returns the agent type identifier.
func (o *OpenCodeAgent) Type() agent.AgentType {
	return agent.AgentTypeOpenCode
}

// Description returns a human-readable description.
func (o *OpenCodeAgent) Description() string {
	return "OpenCode - Open-source AI coding assistant"
}

// DetectPresence checks if OpenCode is configured in the repository.
func (o *OpenCodeAgent) DetectPresence() (bool, error) {
	// Get repo root to check for .opencode directory
	// This is needed because the CLI may be run from a subdirectory
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		// Not in a git repo, fall back to CWD-relative check
		repoRoot = "."
	}

	// Check for .opencode directory
	opencodeDir := filepath.Join(repoRoot, ".opencode")
	if _, err := os.Stat(opencodeDir); err == nil {
		return true, nil
	}
	// Check for opencode.json config file
	configJSON := filepath.Join(repoRoot, "opencode.json")
	if _, err := os.Stat(configJSON); err == nil {
		return true, nil
	}
	// Check for opencode.jsonc config file
	configJSONC := filepath.Join(repoRoot, "opencode.jsonc")
	if _, err := os.Stat(configJSONC); err == nil {
		return true, nil
	}
	return false, nil
}

// GetHookConfigPath returns the path to OpenCode's config file.
// OpenCode does not support lifecycle hooks, so this returns the config file
// path for reference only.
func (o *OpenCodeAgent) GetHookConfigPath() string {
	return "opencode.json"
}

// SupportsHooks returns false as OpenCode does not support lifecycle hooks.
func (o *OpenCodeAgent) SupportsHooks() bool {
	return false
}

// ParseHookInput returns an error as OpenCode does not support hooks.
func (o *OpenCodeAgent) ParseHookInput(_ agent.HookType, _ io.Reader) (*agent.HookInput, error) {
	return nil, errors.New("opencode does not support lifecycle hooks")
}

// GetSessionID extracts the session ID from hook input.
func (o *OpenCodeAgent) GetSessionID(input *agent.HookInput) string {
	return input.SessionID
}

// TransformSessionID converts an OpenCode session ID to an Entire session ID.
// This is an identity function - the agent session ID IS the Entire session ID.
func (o *OpenCodeAgent) TransformSessionID(agentSessionID string) string {
	return agentSessionID
}

// ExtractAgentSessionID extracts the OpenCode session ID from an Entire session ID.
// Since Entire session ID = agent session ID (identity), this returns the input unchanged.
// For backwards compatibility with legacy date-prefixed IDs, it strips the prefix if present.
func (o *OpenCodeAgent) ExtractAgentSessionID(entireSessionID string) string {
	return sessionid.ModelSessionID(entireSessionID)
}

// ProtectedDirs returns directories that OpenCode uses for config/state.
func (o *OpenCodeAgent) ProtectedDirs() []string { return []string{".opencode"} }

// GetSessionDir returns the directory where OpenCode stores session data.
// OpenCode stores sessions in XDG data directory: ~/.local/share/opencode/storage/session/
func (o *OpenCodeAgent) GetSessionDir(_ string) (string, error) {
	// Check for test environment override
	if override := os.Getenv("ENTIRE_TEST_OPENCODE_PROJECT_DIR"); override != "" {
		return override, nil
	}

	dataDir := os.Getenv("XDG_DATA_HOME")
	if dataDir == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("failed to get home directory: %w", err)
		}
		dataDir = filepath.Join(homeDir, ".local", "share")
	}

	return filepath.Join(dataDir, "opencode", "storage", "session"), nil
}

// ResolveSessionFile returns the path to an OpenCode session file.
// OpenCode stores session files as <id>.json.
func (o *OpenCodeAgent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	return filepath.Join(sessionDir, agentSessionID+".json")
}

// ReadSession reads a session from OpenCode's storage (JSON session file).
// The session data is stored in NativeData as raw JSON bytes.
func (o *OpenCodeAgent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
	if input.SessionRef == "" {
		return nil, errors.New("session reference (file path) is required")
	}

	data, err := os.ReadFile(input.SessionRef)
	if err != nil {
		return nil, fmt.Errorf("failed to read session file: %w", err)
	}

	return &agent.AgentSession{
		SessionID:  input.SessionID,
		AgentName:  o.Name(),
		SessionRef: input.SessionRef,
		NativeData: data,
	}, nil
}

// WriteSession writes a session to OpenCode's storage (JSON session file).
func (o *OpenCodeAgent) WriteSession(session *agent.AgentSession) error {
	if session == nil {
		return errors.New("session is nil")
	}

	if session.AgentName != "" && session.AgentName != o.Name() {
		return fmt.Errorf("session belongs to agent %q, not %q", session.AgentName, o.Name())
	}

	if session.SessionRef == "" {
		return errors.New("session reference (file path) is required")
	}

	if len(session.NativeData) == 0 {
		return errors.New("session has no native data to write")
	}

	// Validate it's valid JSON before writing
	if !json.Valid(session.NativeData) {
		return errors.New("session native data is not valid JSON")
	}

	if err := os.WriteFile(session.SessionRef, session.NativeData, 0o600); err != nil {
		return fmt.Errorf("failed to write session file: %w", err)
	}

	return nil
}

// FormatResumeCommand returns the command to resume an OpenCode session.
func (o *OpenCodeAgent) FormatResumeCommand(sessionID string) string {
	return "opencode run --session " + sessionID
}
