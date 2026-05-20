// Package antigravity implements the Agent interface for Antigravity (Google's agentic coding CLI).
package antigravity

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/paths"
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

func (a *AntigravityAgent) DetectPresence(ctx context.Context) (bool, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "."
	}
	for _, candidate := range []string{
		filepath.Join(repoRoot, ".agents"),
		filepath.Join(repoRoot, ".gemini", "jetski"),
	} {
		if _, err := os.Stat(candidate); err == nil {
			return true, nil
		}
	}
	return false, nil
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
	return "antigravity --resume " + sessionID
}

// --- HookSupport stubs — full implementation lands in Chunk 2 (hooks.go, lifecycle.go) ---

func (a *AntigravityAgent) HookNames() []string { return nil }
func (a *AntigravityAgent) ParseHookEvent(_ context.Context, _ string, _ io.Reader) (*agent.Event, error) {
	return nil, nil //nolint:nilnil // stub: real implementation in Chunk 2
}
func (a *AntigravityAgent) InstallHooks(_ context.Context, _ bool, _ bool) (int, error) {
	return 0, nil
}
func (a *AntigravityAgent) UninstallHooks(_ context.Context) error   { return nil }
func (a *AntigravityAgent) AreHooksInstalled(_ context.Context) bool { return false }
