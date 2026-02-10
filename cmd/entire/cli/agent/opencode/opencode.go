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

// Ensure OpenCodeAgent implements required interfaces.
var (
	_ agent.Agent       = (*OpenCodeAgent)(nil)
	_ agent.HookSupport = (*OpenCodeAgent)(nil)
	_ agent.HookHandler = (*OpenCodeAgent)(nil)
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

// OpenCodeAgent implements Agent for OpenCode.
type OpenCodeAgent struct{}

// init registers the agent.
func init() {
	agent.Register(agent.AgentNameOpenCode, NewOpenCodeAgent)
}

// NewOpenCodeAgent creates a new OpenCode agent.
func NewOpenCodeAgent() agent.Agent {
	return &OpenCodeAgent{}
}

// Name returns the registry key.
func (o *OpenCodeAgent) Name() agent.AgentName {
	return agent.AgentNameOpenCode
}

// Type returns the display name.
func (o *OpenCodeAgent) Type() agent.AgentType {
	return agent.AgentTypeOpenCode
}

// Description returns a human-readable description.
func (o *OpenCodeAgent) Description() string {
	return "OpenCode - event-bridged via plugin"
}

// DetectPresence checks for OpenCode project markers.
func (o *OpenCodeAgent) DetectPresence() (bool, error) {
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
func (o *OpenCodeAgent) GetHookConfigPath() string {
	return filepath.Join(pluginDirName, pluginFileName)
}

// SupportsHooks returns true (via plugin bridge).
func (o *OpenCodeAgent) SupportsHooks() bool {
	return true
}

// GetHookNames returns supported hook verbs.
func (o *OpenCodeAgent) GetHookNames() []string {
	return []string{HookNamePromptSubmit, HookNameStop}
}

// ParseHookInput parses JSON from stdin produced by the OpenCode plugin.
func (o *OpenCodeAgent) ParseHookInput(hookType agent.HookType, reader io.Reader) (*agent.HookInput, error) {
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

// GetSessionID returns SessionID from input.
func (o *OpenCodeAgent) GetSessionID(input *agent.HookInput) string {
	if input == nil {
		return ""
	}
	return input.SessionID
}

// TransformSessionID is a passthrough for OpenCode.
func (o *OpenCodeAgent) TransformSessionID(agentSessionID string) string {
	return agentSessionID
}

// ExtractAgentSessionID returns the agent session ID from Entire session ID (passthrough).
func (o *OpenCodeAgent) ExtractAgentSessionID(entireSessionID string) string {
	return entireSessionID
}

// GetSessionDir maps repo to OpenCode storage (project hash unknown; use repo root marker).
// For now, return the repo root; session reading code will locate storage by session ID.
func (o *OpenCodeAgent) GetSessionDir(repoPath string) (string, error) {
	if repoPath == "" {
		return "", errors.New("repo path is required")
	}
	return repoPath, nil
}

// ReadSession reconstructs from storage; minimal stub for now.
func (o *OpenCodeAgent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
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
func (o *OpenCodeAgent) WriteSession(session *agent.AgentSession) error {
	return errors.New("opencode WriteSession not supported")
}

// FormatResumeCommand returns a best-effort resume suggestion.
func (o *OpenCodeAgent) FormatResumeCommand(sessionID string) string {
	if strings.TrimSpace(sessionID) == "" {
		return "opencode"
	}
	return "opencode --session " + sessionID
}

// InstallHooks writes the bridge plugin into .opencode/plugins/entire.js.
func (o *OpenCodeAgent) InstallHooks(localDev bool, force bool) (int, error) {
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
	if err := os.WriteFile(pluginPath, content, 0o640); err != nil {
		return 0, fmt.Errorf("failed to write plugin: %w", err)
	}

	return 1, nil
}

// UninstallHooks removes the bridge plugin.
func (o *OpenCodeAgent) UninstallHooks() error {
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
func (o *OpenCodeAgent) AreHooksInstalled() bool {
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		repoRoot = "."
	}
	pluginPath := filepath.Join(repoRoot, pluginDirName, pluginFileName)
	_, err = os.Stat(pluginPath)
	return err == nil
}

// GetSupportedHooks returns hook types this agent surfaces.
func (o *OpenCodeAgent) GetSupportedHooks() []agent.HookType {
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
            stdin: JSON.stringify({ session_id: e.sessionID, prompt: e.properties?.message?.content || '' })
          });
          await p.exited;
        }
        if (e?.type === 'session.status' && e.properties?.status?.type === 'idle' && e.sessionID) {
          const p = Bun.spawn(['entire', 'hooks', 'opencode', 'stop'], {
            stdin: JSON.stringify({ session_id: e.sessionID })
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
