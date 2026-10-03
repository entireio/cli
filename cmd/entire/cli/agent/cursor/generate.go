package cursor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// generateDenyAllConfig is the project config written into the generation
// run's workspace (.cursor/cli.json): it denies every permission kind Cursor
// has. Summary generation needs no tools, because the transcript is already
// in the prompt, and the prompt carries untrusted transcript content.
//
// Cursor has no "no tools" flag, and neither --mode ask (documented as
// read-only) nor --sandbox enabled stops a shell read or a network request:
// verified live on cursor-agent 2026.05.16, where both still read a file
// outside the workspace and reached a local HTTP listener. Deny rules in the
// workspace's project config do stop them.
const generateDenyAllConfig = `{"permissions":{"allow":[],"deny":["Shell(*)","Read(*)","Write(*)","WebFetch(*)","Mcp(*:*)"]}}` + "\n"

// GenerateText sends a prompt to the Cursor agent CLI and returns the raw text response.
//
// The prompt is piped via stdin rather than as a positional argument, avoiding
// argv size limits. --print triggers non-interactive mode and --trust accepts
// the workspace without a prompt. The workspace is a fresh directory holding
// only generateDenyAllConfig.
//
// There is deliberately no --force: it auto-approves shell commands, and with
// it an injected instruction could read, write, and fetch as the user. Without
// it, and with every permission denied, a text-only prompt still completes.
func (c *CursorAgent) GenerateText(ctx context.Context, prompt string, model string) (string, error) {
	workspace, cleanup, err := agent.NewTextGenerationDir()
	if err != nil {
		return "", fmt.Errorf("cursor text generation failed: %w", err)
	}
	defer cleanup()
	if err := writeDenyAllConfig(workspace); err != nil {
		return "", fmt.Errorf("cursor text generation failed: %w", err)
	}

	args := []string{"--print", "--trust", "--workspace", workspace}
	if model != "" {
		args = append(args, "--model", model)
	}

	result, capturedStderr, stdoutBytes, err := agent.RunIsolatedTextGeneratorCLIIn(ctx, c.CommandRunner, workspace, "agent", "cursor", args, prompt)
	if err != nil {
		return "", &agent.TextGenerationError{
			Err:         fmt.Errorf("cursor text generation failed: %w", err),
			Stderr:      capturedStderr,
			StdoutBytes: stdoutBytes,
		}
	}
	return result, nil
}

// writeDenyAllConfig writes generateDenyAllConfig to <workspace>/.cursor/cli.json.
// workspace is a directory this process just created and nothing else knows,
// so no path in it can already be a link.
func writeDenyAllConfig(workspace string) error {
	dir := filepath.Join(workspace, ".cursor")
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("create cursor project config dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cli.json"), []byte(generateDenyAllConfig), 0o600); err != nil {
		return fmt.Errorf("write cursor project config: %w", err)
	}
	return nil
}
