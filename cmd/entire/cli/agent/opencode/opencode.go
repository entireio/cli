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

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
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

// GetHookConfigPath returns the path to OpenCode's plugin file.
// OpenCode uses a TypeScript plugin system; hooks are installed as
// .opencode/plugins/entire.ts which is auto-discovered by OpenCode.
func (o *OpenCodeAgent) GetHookConfigPath() string {
	return filepath.Join(".opencode", "plugins", EntirePluginFileName)
}

// SupportsHooks returns true as OpenCode supports hooks via its plugin system.
func (o *OpenCodeAgent) SupportsHooks() bool {
	return true
}

// ParseHookInput parses OpenCode hook input from stdin.
// The Entire plugin pipes JSON payloads with type and sessionID fields.
func (o *OpenCodeAgent) ParseHookInput(hookType agent.HookType, reader io.Reader) (*agent.HookInput, error) {
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

	var payload pluginPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("failed to parse plugin payload: %w", err)
	}

	input.SessionID = payload.SessionID
	input.RawData["type"] = payload.Type

	// Resolve session transcript path
	sessionDir, dirErr := o.GetSessionDir("")
	if dirErr == nil && input.SessionID != "" {
		input.SessionRef = o.ResolveSessionFile(sessionDir, input.SessionID)
	}

	return input, nil
}

// GetSessionID extracts the session ID from hook input.
func (o *OpenCodeAgent) GetSessionID(input *agent.HookInput) string {
	return input.SessionID
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
// OpenCode stores sessions nested by projectID: <sessionDir>/<projectID>/<id>.json
// Since we don't know the projectID, we walk the directory tree to find a matching file.
// Falls back to a flat path if no match is found.
func (o *OpenCodeAgent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	target := agentSessionID + ".json"

	// Walk the session directory to find the file under any projectID subdirectory
	var found string
	_ = filepath.WalkDir(sessionDir, func(path string, d os.DirEntry, err error) error { //nolint:errcheck // non-fatal; fallback path handles missing dirs
		if err != nil {
			return nil //nolint:nilerr // Skip inaccessible directories
		}
		if d.IsDir() {
			return nil
		}
		if d.Name() == target {
			found = path
			return filepath.SkipAll // found it, stop walking
		}
		return nil
	})

	if found != "" {
		return found
	}

	// Fallback: construct a flat path (may not exist)
	return filepath.Join(sessionDir, target)
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

	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(session.SessionRef), 0o750); err != nil {
		return fmt.Errorf("failed to create parent directory: %w", err)
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
