package copilotcli

import (
	"context"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// flagDenyTool withholds one built-in tool from the generation run; Entire's
// summaries need no tools at all, so every one it knows about is denied.
const flagDenyTool = "--deny-tool"

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
//   - --available-tools=<no such tool>: tool AVAILABILITY is a separate,
//     stricter layer than the approval flags above: deny/allow only decide
//     whether a visible tool prompts, so read-only tools (view, glob, grep)
//     stayed visible and unprompted, and injected transcript content could
//     have a file read into the summary. An allowlist naming a tool that does
//     not exist leaves the model no tools at all, and keeps doing so when a
//     Copilot release adds one. An EMPTY list is not equivalent: verified on
//     Copilot CLI 1.0.83, `--available-tools` with no value leaves every tool
//     available.
//   - -s: the allowlist makes Copilot print a "Disabled tools" notice on
//     stdout, which is where the summary JSON comes back; silent mode keeps
//     stdout to the model's response.
//   - --disallow-temp-dir: Copilot grants file access to the system temp
//     directory by default, on top of the working directory. With no tools
//     that grant is unused; this keeps it unused if a tool ever slips through.
//
// Flag semantics verified against Copilot CLI 1.0.83 (help text, argv
// acceptance, and a live run in which a read request inside the working
// directory and the temp directory returned nothing). The "completes without
// an approval flag" claim is exercised only by the opt-in smoke test below,
// so a CLI version that regresses it would degrade summaries until that test
// is run.
// TestGenerateText_PinsMinimalToolSurface pins the argv so a future flag
// change is a reviewed decision rather than a drive-by edit.
var generateTextArgs = []string{
	flagDenyTool, "shell",
	flagDenyTool, "write",
	flagDenyTool, "url",
	"--no-ask-user",
	"--no-custom-instructions",
	"--disable-builtin-mcps",
	"--available-tools=" + noSuchTool,
	"-s",
	"--disallow-temp-dir",
}

// noSuchTool is the one entry in the generation run's tool allowlist. It
// names no real tool, so nothing is available (see generateTextArgs).
const noSuchTool = "entire_text_generation_uses_no_tools"

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
