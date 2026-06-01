package claudecode

import (
	"context"
	"os/exec"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/spawn"
)

// claudeCodeSpawner produces argv: claude -p <prompt>; no stdin.
type claudeCodeSpawner struct{}

// NewSpawner returns a Spawner for claude-code's non-interactive review/investigate mode.
func NewSpawner() spawn.Spawner { return claudeCodeSpawner{} }

func (claudeCodeSpawner) Name() string { return string(agent.AgentNameClaudeCode) }

func (claudeCodeSpawner) BuildCmd(ctx context.Context, env []string, prompt string) *exec.Cmd {
	// --permission-mode bypassPermissions auto-accepts every tool call.
	// `-p` (print) mode has no UI to answer permission prompts, so the
	// default mode silently denies anything not pre-approved — including
	// Bash (so `git`, `grep`, `ls` would be blocked). The prompt forbids
	// destructive commands; the flag is a no-op for the review path.
	cmd := exec.CommandContext(ctx, "claude", "-p", "--permission-mode", "bypassPermissions", prompt)
	cmd.Env = env
	return cmd
}
