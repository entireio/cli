// Package antigravity implements the Agent interface for Antigravity (Google's agentic coding CLI).
package antigravity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

const antigravityTestBrainDirEnv = "ENTIRE_TEST_ANTIGRAVITY_BRAIN_DIR"

//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
	agent.Register(agent.AgentNameAntigravity, NewAntigravityAgent)
}

// AntigravityAgent implements the Agent interface for Antigravity.
//
//nolint:revive // AntigravityAgent is clearer than Agent in this context
type AntigravityAgent struct {
	CommandRunner agent.TextCommandRunner
}

var _ agent.OutOfBandTokenSource = (*AntigravityAgent)(nil)

// NewAntigravityAgent creates a new AntigravityAgent instance.
func NewAntigravityAgent() agent.Agent {
	return &AntigravityAgent{}
}

// --- Identity ---

func (a *AntigravityAgent) Name() types.AgentName { return agent.AgentNameAntigravity }
func (a *AntigravityAgent) Type() types.AgentType { return agent.AgentTypeAntigravity }
func (a *AntigravityAgent) Description() string {
	return "Antigravity CLI - Google's agentic coding CLI (Gemini CLI successor)"
}
func (a *AntigravityAgent) IsPreview() bool { return true }

// DetectPresence reports whether Entire's Antigravity hooks are configured for
// this workspace. Antigravity 2.0 stores runtime data user-scope in
// ~/.gemini/antigravity-cli/, so the only meaningful workspace-level signal is
// whether our entry exists in .agents/hooks.json.
func (a *AntigravityAgent) DetectPresence(ctx context.Context) (bool, error) {
	return a.AreHooksInstalled(ctx), nil
}

func (a *AntigravityAgent) ProtectedDirs() []string { return []string{".agents", ".gemini"} }

// --- Legacy methods ---

func (a *AntigravityAgent) GetSessionID(input *agent.HookInput) string { return input.SessionID }
func (a *AntigravityAgent) GetSessionDir(_ string) (string, error) {
	if override := os.Getenv(antigravityTestBrainDirEnv); override != "" {
		return override, nil
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("antigravity: failed to get home directory: %w", err)
	}
	return filepath.Join(homeDir, ".gemini", "antigravity-cli", "brain"), nil
}

func (a *AntigravityAgent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	return filepath.Join(sessionDir, agentSessionID, ".system_generated", "logs", "transcript_full.jsonl")
}

func (a *AntigravityAgent) ReadSession(_ *agent.HookInput) (*agent.AgentSession, error) {
	return nil, errors.New("antigravity: legacy ReadSession not supported; use transcriptPath from hook stdin")
}
func (a *AntigravityAgent) WriteSession(_ context.Context, session *agent.AgentSession) error {
	if session == nil {
		return errors.New("antigravity: session is nil")
	}
	if session.AgentName != "" && session.AgentName != a.Name() {
		return fmt.Errorf("antigravity: session belongs to agent %q, not %q", session.AgentName, a.Name())
	}
	if session.SessionRef == "" {
		return errors.New("antigravity: session reference is required")
	}
	if len(session.NativeData) == 0 {
		return errors.New("antigravity: session has no native data to write")
	}
	if err := os.MkdirAll(filepath.Dir(session.SessionRef), 0o750); err != nil {
		return fmt.Errorf("antigravity: create transcript dir: %w", err)
	}
	if err := os.WriteFile(session.SessionRef, session.NativeData, 0o600); err != nil {
		return fmt.Errorf("antigravity: write transcript: %w", err)
	}
	return nil
}

func (a *AntigravityAgent) FormatResumeCommand(sessionID string) string {
	return "agy --conversation " + sessionID
}
