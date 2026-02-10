package opencode

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// Ensure Agent implements required interfaces.
var (
	_ agent.Agent       = (*Agent)(nil)
	_ agent.HookSupport = (*Agent)(nil)
	_ agent.HookHandler = (*Agent)(nil)
)

// Hook names for OpenCode bridge plugin.
const (
	HookNamePromptSubmit = "prompt-submit"
	HookNameStop         = "stop"
)

// Plugin filename and dir.
const (
	pluginDirName  = ".opencode/plugins"
	pluginFileName = "entire.js"
)

// Agent implements the OpenCode agent.
type Agent struct{}

//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
	agent.Register(agent.AgentNameOpenCode, NewAgent)
}

// NewAgent creates a new OpenCode agent.
func NewAgent() agent.Agent {
	return &Agent{}
}

// Name returns the registry key.
func (o *Agent) Name() agent.AgentName {
	return agent.AgentNameOpenCode
}

// Type returns the display name.
func (o *Agent) Type() agent.AgentType {
	return agent.AgentTypeOpenCode
}

// Description returns a human-readable description.
func (o *Agent) Description() string {
	return "OpenCode - event-bridged via plugin"
}

// DetectPresence checks for OpenCode project markers.
func (o *Agent) DetectPresence() (bool, error) {
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		repoRoot = "."
	}

	markers := []string{
		filepath.Join(repoRoot, "opencode.json"),
		filepath.Join(repoRoot, "opencode.jsonc"),
		filepath.Join(repoRoot, "AGENTS.md"),
		filepath.Join(repoRoot, ".opencode"),
	}
	for _, m := range markers {
		if _, err := os.Stat(m); err == nil {
			return true, nil
		}
	}

	if os.Getenv("OPENCODE_CONFIG") != "" || os.Getenv("OPENCODE_CONFIG_DIR") != "" {
		return true, nil
	}
	return false, nil
}

// GetHookConfigPath returns the plugin location.
func (o *Agent) GetHookConfigPath() string {
	return filepath.Join(pluginDirName, pluginFileName)
}

// SupportsHooks returns true (via plugin bridge).
func (o *Agent) SupportsHooks() bool {
	return true
}

// GetHookNames returns supported hook verbs.
func (o *Agent) GetHookNames() []string {
	return []string{HookNamePromptSubmit, HookNameStop}
}

// ParseHookInput parses JSON from stdin produced by the OpenCode plugin.
func (o *Agent) ParseHookInput(hookType agent.HookType, reader io.Reader) (*agent.HookInput, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read input: %w", err)
	}
	if len(data) == 0 {
		return nil, errors.New("empty input")
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse JSON: %w", err)
	}

	input := &agent.HookInput{
		HookType:  hookType,
		Timestamp: time.Now(),
		RawData:   raw,
	}

	if v, ok := raw["session_id"].(string); ok {
		// Validate session ID to prevent path traversal
		if err := validateSessionID(v); err != nil {
			return nil, fmt.Errorf("invalid session_id: %w", err)
		}
		input.SessionID = v
	}
	if v, ok := raw["session_ref"].(string); ok {
		input.SessionRef = v
	}
	if v, ok := raw["prompt"].(string); ok {
		input.UserPrompt = v
	}

	return input, nil
}

// validateSessionID validates that the session ID is safe for use in file paths.
// It rejects IDs containing path separators or traversal sequences.
func validateSessionID(sessionID string) error {
	if sessionID == "" {
		return nil // Empty is allowed (will fall back to unknownSessionID later)
	}
	// Reject path separators and traversal attempts
	if strings.Contains(sessionID, "/") || strings.Contains(sessionID, "\\") {
		return errors.New("session ID contains path separator")
	}
	if strings.Contains(sessionID, "..") {
		return errors.New("session ID contains path traversal sequence")
	}
	return nil
}

// GetSessionID returns SessionID from input.
func (o *Agent) GetSessionID(input *agent.HookInput) string {
	if input == nil {
		return ""
	}
	return input.SessionID
}

// TransformSessionID is a passthrough for OpenCode.
func (o *Agent) TransformSessionID(agentSessionID string) string {
	return agentSessionID
}

// ExtractAgentSessionID returns the agent session ID from Entire session ID (passthrough).
func (o *Agent) ExtractAgentSessionID(entireSessionID string) string {
	return entireSessionID
}

// GetSessionDir maps repo to OpenCode storage (project hash unknown; use repo root marker).
// For now, return the repo root; session reading code will locate storage by session ID.
func (o *Agent) GetSessionDir(repoPath string) (string, error) {
	if repoPath == "" {
		return "", errors.New("repo path is required")
	}
	return repoPath, nil
}

// ReadSession reconstructs from storage; minimal stub for now.
func (o *Agent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
	if input == nil {
		return nil, errors.New("hook input is nil")
	}
	return &agent.AgentSession{
		SessionID:  input.SessionID,
		SessionRef: input.SessionRef,
		AgentName:  o.Name(),
		StartTime:  time.Now(),
		NativeData: nil,
	}, nil
}

// WriteSession is a no-op for OpenCode.
func (o *Agent) WriteSession(_ *agent.AgentSession) error {
	return errors.New("opencode WriteSession not supported")
}

// FormatResumeCommand returns a best-effort resume suggestion.
func (o *Agent) FormatResumeCommand(sessionID string) string {
	if strings.TrimSpace(sessionID) == "" {
		return "opencode"
	}
	return "opencode --session " + sessionID
}

// InstallHooks writes the bridge plugin into .opencode/plugins/entire.js.
func (o *Agent) InstallHooks(_ bool, force bool) (int, error) {
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		repoRoot = "."
	}

	pluginDir := filepath.Join(repoRoot, pluginDirName)
	pluginPath := filepath.Join(pluginDir, pluginFileName)

	if !force {
		if _, err := os.Stat(pluginPath); err == nil {
			return 0, nil // already installed
		}
	}

	if err := os.MkdirAll(pluginDir, 0o750); err != nil {
		return 0, fmt.Errorf("failed to create plugin dir: %w", err)
	}

	content := []byte(strings.TrimSpace(embeddedPlugin))
	if err := os.WriteFile(pluginPath, content, 0o600); err != nil {
		return 0, fmt.Errorf("failed to write plugin: %w", err)
	}

	return 1, nil
}

// UninstallHooks removes the bridge plugin.
func (o *Agent) UninstallHooks() error {
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		repoRoot = "."
	}
	pluginPath := filepath.Join(repoRoot, pluginDirName, pluginFileName)
	if err := os.Remove(pluginPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove plugin: %w", err)
	}
	return nil
}

// AreHooksInstalled checks for plugin presence.
func (o *Agent) AreHooksInstalled() bool {
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		repoRoot = "."
	}
	pluginPath := filepath.Join(repoRoot, pluginDirName, pluginFileName)
	_, err = os.Stat(pluginPath)
	return err == nil
}

// GetSupportedHooks returns hook types this agent surfaces.
func (o *Agent) GetSupportedHooks() []agent.HookType {
	return []agent.HookType{
		agent.HookUserPromptSubmit,
		agent.HookStop,
	}
}

// embeddedPlugin is the JS bridge written to .opencode/plugins/entire.js.
// It forwards OpenCode events to Entire hook commands.
const embeddedPlugin = `
export default async function() {
  return {
    event: async (e) => {
      try {
        if (e?.type === 'chat.message' && e.sessionID) {
          const p = Bun.spawn(['entire', 'hooks', 'opencode', 'prompt-submit'], {
            stdin: new Blob([JSON.stringify({ session_id: e.sessionID, prompt: e.properties?.message?.content || '' })])
          });
          await p.exited;
        }
        if (e?.type === 'session.status' && e.properties?.status?.type === 'idle' && e.sessionID) {
          const p = Bun.spawn(['entire', 'hooks', 'opencode', 'stop'], {
            stdin: new Blob([JSON.stringify({ session_id: e.sessionID })])
          });
          await p.exited;
        }
      } catch (err) {
        // Swallow errors to avoid breaking OpenCode UX
        console.error('entire plugin error', err);
      }
    }
  };
}
`
