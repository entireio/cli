// Package antigravity implements the Agent interface for Antigravity (Google's agentic coding CLI).
package antigravity

import (
	"context"
	"errors"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

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
// Antigravity supplies transcriptPath directly in every hook payload; no session discovery needed.

func (a *AntigravityAgent) GetSessionID(input *agent.HookInput) string { return input.SessionID }
func (a *AntigravityAgent) GetSessionDir(_ string) (string, error)     { return "", nil }
func (a *AntigravityAgent) ResolveSessionFile(_, _ string) string      { return "" }
func (a *AntigravityAgent) ReadSession(_ *agent.HookInput) (*agent.AgentSession, error) {
	return nil, errors.New("antigravity: legacy ReadSession not supported; use transcriptPath from hook stdin")
}
func (a *AntigravityAgent) WriteSession(_ context.Context, _ *agent.AgentSession) error { return nil }
func (a *AntigravityAgent) FormatResumeCommand(sessionID string) string {
	return "agy --conversation " + sessionID
}
