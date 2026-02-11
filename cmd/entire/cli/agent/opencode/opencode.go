package opencode

import (
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
	agent.Register(agent.AgentNameOpencode, NewOpenCodeAgent)
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
	return agent.AgentNameOpencode
}

// Type returns the agent type identifier.
func (o *OpenCodeAgent) Type() agent.AgentType {
	return agent.AgentTypeOpencode
}

// Description returns a human-readable description.
func (o *OpenCodeAgent) Description() string {
	return "OpenCode - Open source AI coding agent"
}

// DetectPresence checks if OpenCode is configured in the repository.
//
// OpenCode uses either a project config file (opencode.json) or a
// project-local configuration directory (.opencode/). We treat either
// as a signal that OpenCode is in use for this repository.
func (o *OpenCodeAgent) DetectPresence() (bool, error) {
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		// Not in a git repo, fall back to CWD-relative check
		repoRoot = "."
	}

	opencodeDir := filepath.Join(repoRoot, ".opencode")
	if _, err := os.Stat(opencodeDir); err == nil {
		return true, nil
	}

	configFile := filepath.Join(repoRoot, "opencode.json")
	if _, err := os.Stat(configFile); err == nil {
		return true, nil
	}

	return false, nil
}

// GetHookConfigPath returns the path to OpenCode's primary config file.
// This is informational only; hooks are typically configured via plugins.
func (o *OpenCodeAgent) GetHookConfigPath() string {
	return "opencode.json"
}

// SupportsHooks reports whether the agent supports lifecycle hooks managed
// directly by Entire. OpenCode integrations are typically implemented via
// its plugin system invoking `entire hooks ...`, so we return false here
// to indicate that Entire does not install or manage those hooks itself.
func (o *OpenCodeAgent) SupportsHooks() bool {
	return false
}

// ParseHookInput parses hook callback input from stdin.
//
// Since OpenCode hooks are expected to be implemented via its plugin system
// and are not yet standardized for Entire, this returns a descriptive error
// if called. This keeps the implementation explicit and easy to extend once
// a concrete hook payload schema is agreed upon.
func (o *OpenCodeAgent) ParseHookInput(_ agent.HookType, _ io.Reader) (*agent.HookInput, error) { //nolint:ireturn // interface contract
	return nil, errors.New("OpenCode hooks are not yet implemented in Entire")
}

// GetSessionID extracts the session ID from hook input.
// For OpenCode this is currently a simple passthrough.
func (o *OpenCodeAgent) GetSessionID(input *agent.HookInput) string {
	if input == nil {
		return ""
	}
	return input.SessionID
}

// TransformSessionID converts an OpenCode session ID to an Entire session ID.
// This is currently an identity mapping to match other modern agents.
func (o *OpenCodeAgent) TransformSessionID(agentSessionID string) string {
	return agentSessionID
}

// ExtractAgentSessionID extracts the OpenCode session ID from an Entire session ID.
// For backwards compatibility with legacy date-prefixed IDs, it strips the prefix
// if present, mirroring the behavior of other agents.
func (o *OpenCodeAgent) ExtractAgentSessionID(entireSessionID string) string {
	return sessionid.ModelSessionID(entireSessionID)
}

// ProtectedDirs returns directories that OpenCode uses for project-local config/state.
func (o *OpenCodeAgent) ProtectedDirs() []string { return []string{".opencode"} }

// GetSessionDir returns the root directory where OpenCode stores session data.
//
// OpenCode uses an XDG-style data root, typically:
//   ~/.local/share/opencode/storage/session/<projectID>/<sessionID>.json
//
// Because project IDs are internal to OpenCode, we return the session storage
// root and let ResolveSessionFile locate the concrete session file.
func (o *OpenCodeAgent) GetSessionDir(_ string) (string, error) {
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

// ResolveSessionFile attempts to locate a concrete session file for a given
// OpenCode session ID. The expected layout is:
//
//   <sessionDir>/<projectID>/<sessionID>.json
//
// We scan one level of subdirectories looking for a matching session file and
// fall back to <sessionDir>/<sessionID>.json if no match is found. This keeps
// the implementation robust across minor layout changes while avoiding deep
// recursive scans.
func (o *OpenCodeAgent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	if sessionDir == "" || agentSessionID == "" {
		return ""
	}

	entries, err := os.ReadDir(sessionDir)
	if err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			candidate := filepath.Join(sessionDir, entry.Name(), agentSessionID+".json")
			if _, statErr := os.Stat(candidate); statErr == nil {
				return candidate
			}
		}
	}

	// Fallback: treat sessionDir as flat storage
	return filepath.Join(sessionDir, agentSessionID+".json")
}

// ReadSession reads a session from OpenCode's storage.
//
// At this stage, OpenCode's on-disk session format is treated as opaque. We
// read the raw bytes and store them in NativeData so higher-level features
// that do not rely on a specific transcript schema can still function.
// ModifiedFiles and token usage are left to be implemented once a stable
// transcript schema is finalized.
func (o *OpenCodeAgent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) { //nolint:ireturn // interface contract
	if input == nil {
		return nil, errors.New("hook input is nil")
	}
	if input.SessionRef == "" {
		return nil, errors.New("session reference (transcript path) is required")
	}

	data, err := os.ReadFile(input.SessionRef)
	if err != nil {
		return nil, fmt.Errorf("failed to read OpenCode session: %w", err)
	}

	return &agent.AgentSession{
		SessionID:  input.SessionID,
		AgentName:  o.Name(),
		SessionRef: input.SessionRef,
		NativeData: data,
	}, nil
}

// WriteSession writes a session back to OpenCode's storage.
//
// Since the session format is treated as opaque, this simply writes NativeData
// back to the provided SessionRef path. It is the caller's responsibility to
// ensure that the data is in a format OpenCode can understand (for example,
// by starting from a session that was originally produced by OpenCode).
func (o *OpenCodeAgent) WriteSession(session *agent.AgentSession) error {
	if session == nil {
		return errors.New("session is nil")
	}

	if session.AgentName != "" && session.AgentName != o.Name() {
		return fmt.Errorf("session belongs to agent %q, not %q", session.AgentName, o.Name())
	}

	if session.SessionRef == "" {
		return errors.New("session reference (transcript path) is required")
	}

	if len(session.NativeData) == 0 {
		return errors.New("session has no native data to write")
	}

	if err := os.WriteFile(session.SessionRef, session.NativeData, 0o600); err != nil {
		return fmt.Errorf("failed to write OpenCode session: %w", err)
	}

	return nil
}

// FormatResumeCommand returns the command to resume an OpenCode session.
func (o *OpenCodeAgent) FormatResumeCommand(sessionID string) string {
	if sessionID == "" {
		return "opencode"
	}
	return "opencode --session " + sessionID
}

