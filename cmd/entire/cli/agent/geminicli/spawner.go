package geminicli

import (
	"context"
	"os/exec"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent/spawn"
)

// geminiSpawner produces argv: gemini -p " "; prompt via stdin.
// The " " argv placeholder triggers headless mode; the prompt goes via stdin
// because gemini's -p flag appends to stdin content.
type geminiSpawner struct{}

// NewSpawner returns a Spawner for gemini-cli's non-interactive review/investigate mode.
func NewSpawner() spawn.Spawner { return geminiSpawner{} }

func (geminiSpawner) Name() string { return "gemini-cli" }

func (geminiSpawner) BuildCmd(ctx context.Context, env []string, prompt string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "gemini", "-p", " ")
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = env
	return cmd
}
