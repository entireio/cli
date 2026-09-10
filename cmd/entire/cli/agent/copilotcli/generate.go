package copilotcli

import (
	"context"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// generateTextArgs is the pinned tool policy for text generation.
//
// Summary generation is a text-in text-out call, and its prompt carries
// untrusted transcript content, so the subprocess follows the isolation
// contract the Claude generator documents in claudecode.buildGenerateArgs:
// nothing granted by configuration may let the input drive tool execution.
// The previous --allow-all-tools flag granted the opposite (blanket
// auto-approval, including shell), so it is replaced with explicit denials:
//
//   - --deny-tool shell/write/url: per `copilot help permissions`, a deny rule
//     is an automatic denial that never prompts and takes precedence over
//     every allow flag. Denying the execution-bearing kinds keeps a headless
//     run deterministic. There is deliberately no --allow-all-tools. A
//     text-only prompt completes without any approval flag (its "required for
//     non-interactive mode" help text is about letting tool calls run, not an
//     argv requirement), and any stray approval-gated call is auto-denied.
//   - --no-ask-user: disables the ask_user tool, the one remaining
//     interactive tool that could block a headless run.
//   - --no-custom-instructions: skips AGENTS.md and related instruction
//     files, matching the Claude generator's rule of loading no settings that
//     could steer the run.
//   - --disable-builtin-mcps: skips the GitHub MCP server, which is not
//     needed for text generation and inflates per-call input tokens.
//
// Known residual, deliberately accepted: deny/allow flags control approval
// prompts only. Tool AVAILABILITY is a separate, stricter layer
// (--available-tools / --excluded-tools) that this policy does not touch, so
// read-only tools remain visible and unprompted, and injected transcript
// content can still steer file reads whose contents land in the summary. The
// Claude generator carries the same read residual. Tightening to
// --available-tools with an empty set is the follow-up if that residual is
// ever closed, and it needs a live verification that an empty availability
// list means "no tools" on the pinned CLI version.
//
// Flag semantics verified against Copilot CLI 1.0.81 (help text plus argv
// acceptance). The "completes without an approval flag" claim is exercised
// only by the opt-in smoke test below, so a CLI version that regresses it
// would degrade summaries until that test is run.
// TestGenerateText_PinsMinimalToolSurface pins the argv so a future flag
// change is a reviewed decision rather than a drive-by edit.
var generateTextArgs = []string{
	"--deny-tool", "shell",
	"--deny-tool", "write",
	"--deny-tool", "url",
	"--no-ask-user",
	"--no-custom-instructions",
	"--disable-builtin-mcps",
}

// GenerateText sends a prompt to the Copilot CLI and returns the raw text response.
//
// The prompt is piped via stdin rather than -p to avoid argv size limits. The
// subprocess runs from os.TempDir with GIT_* stripped and the pinned minimal
// tool policy above (see agent.RunIsolatedTextGeneratorCLI and
// generateTextArgs).
func (c *CopilotCLIAgent) GenerateText(ctx context.Context, prompt string, model string) (string, error) {
	args := append([]string(nil), generateTextArgs...)
	if model != "" {
		args = append(args, "--model", model)
	}

	result, capturedStderr, stdoutBytes, err := agent.RunIsolatedTextGeneratorCLI(ctx, c.CommandRunner, "copilot", "copilot", args, prompt)
	if err != nil {
		return "", &agent.TextGenerationError{
			Err:         fmt.Errorf("copilot text generation failed: %w", err),
			Stderr:      capturedStderr,
			StdoutBytes: stdoutBytes,
		}
	}
	return result, nil
}
