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

	// Nil resolves the user's authentication sources. Tests inject temporary
	// sources separately from CommandRunner, which only replaces the process.
	textGenerationAuth *textGenerationAuth
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

// DetectPresence reports whether Entire's Antigravity hooks are configured for
// this workspace. Every Antigravity surface keeps its runtime data user-scope
// under ~/.gemini (see brainDirs), so the only meaningful workspace-level
// signal is whether our entry exists in .agents/hooks.json.
func (a *AntigravityAgent) DetectPresence(ctx context.Context) (bool, error) {
	return a.AreHooksInstalled(ctx)
}

func (a *AntigravityAgent) ProtectedDirs() []string { return []string{".agents", ".gemini"} }

// --- Legacy methods ---

func (a *AntigravityAgent) GetSessionID(input *agent.HookInput) string { return input.SessionID }
func (a *AntigravityAgent) GetSessionDir(_ string) (string, error) {
	if override := os.Getenv(antigravityTestBrainDirEnv); override != "" {
		return override, nil
	}
	home, err := a.SessionHome()
	if err != nil {
		return "", err
	}
	return a.HomeLayout().StoresUnder(home)[0], nil
}

// SessionHome returns ~/.gemini, the directory every Antigravity surface keeps
// its data in. agy has no variable that relocates it.
func (a *AntigravityAgent) SessionHome() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("antigravity: failed to get home directory: %w", err)
	}
	return filepath.Join(homeDir, ".gemini"), nil
}

// HomeLayout reports one brain per Antigravity surface sharing agy's
// hooks.json: the CLI's first, then the IDE's through 2.5.x, then the 2.0
// app's (which the IDE also uses from 2.6.0). Hook payloads carry
// transcriptPath values inside the surface's own brain.
func (a *AntigravityAgent) HomeLayout() agent.HomeLayout {
	return agent.HomeLayout{Stores: []string{"antigravity-cli/brain", "antigravity-ide/brain", "antigravity/brain"}}
}

var _ agent.HomeLayoutProvider = (*AntigravityAgent)(nil)

// brainDirs returns every brain directory a hook-supplied transcriptPath may
// lie in, the CLI's (GetSessionDir) first. They are derived from
// GetSessionDir's own result, so the test override relocates them with it.
func (a *AntigravityAgent) brainDirs() ([]string, error) {
	cliBrain, err := a.GetSessionDir("")
	if err != nil {
		return nil, err
	}
	return a.surfaceBrains(cliBrain), nil
}

// surfaceBrains returns the brains beside cliBrain when it is the CLI store of
// a home in agy's layout, and only cliBrain otherwise.
func (a *AntigravityAgent) surfaceBrains(cliBrain string) []string {
	cliBrain = filepath.Clean(cliBrain)
	if stores := a.HomeLayout().StoresUnder(filepath.Dir(filepath.Dir(cliBrain))); stores[0] == cliBrain {
		return stores
	}
	return []string{cliBrain}
}

// ResolveSessionFileCandidates returns agentSessionID's transcript in each
// surface's brain, the CLI's first, so `entire attach` finds IDE and app
// conversations too.
func (a *AntigravityAgent) ResolveSessionFileCandidates(sessionDir, agentSessionID string) []string {
	brains := a.surfaceBrains(sessionDir)
	candidates := make([]string, 0, len(brains))
	for _, brain := range brains {
		candidates = append(candidates, transcriptInBrain(brain, agentSessionID))
	}
	return candidates
}

var _ agent.SessionFileCandidatesProvider = (*AntigravityAgent)(nil)

func (a *AntigravityAgent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	return transcriptInBrain(sessionDir, agentSessionID)
}

// transcriptInBrain is where agy writes conversation agentSessionID's
// transcript in brain. Callers validate agentSessionID first.
func transcriptInBrain(brain, agentSessionID string) string {
	return filepath.Join(brain, agentSessionID, ".system_generated", "logs", "transcript_full.jsonl")
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
	// Through the agent's session store, like every other agent: the write is
	// contained to agy's brain directory and never follows a symlink.
	if err := agent.WriteSessionFile(a, session, session.NativeData, 0o600); err != nil {
		return fmt.Errorf("antigravity: write transcript: %w", err)
	}
	return nil
}

// HookConfigRelPath implements agent.HookConfigLocator: agy loads workspace
// hooks from .agents/hooks.json at the worktree root.
func (a *AntigravityAgent) HookConfigRelPath() string {
	return ".agents/" + AgentsHooksFileName
}

func (a *AntigravityAgent) FormatResumeCommand(sessionID string) string {
	return "agy --conversation " + sessionID
}
