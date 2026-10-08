package antigravity

import (
	"context"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// GenerateText submits a non-interactive prompt to the Antigravity CLI. The
// binary is `agy`; -p is the short alias for --print (single-prompt mode).
//
// The prompt travels in argv. Earlier releases accepted it on stdin behind a
// single-space -p placeholder (the Gemini CLI convention, verified on agy
// 1.0.16), but agy 1.2.7 ignores stdin in print mode: `-p " "` fails with
// "Error: empty prompt", and `-p -` is answered as the literal message "-"
// (both observed live, trail 444, 2026-09-22), which is how
// `entire dispatch --local --agent antigravity` came to hand agy an empty
// prompt. argv is the only documented route ("Usage: agy --print 'your
// prompt here'").
//
// That makes prompt size this agent's problem in a way it is not for agents
// that use RunIsolatedTextGeneratorCLI's stdin. Linux caps a SINGLE argument
// at MAX_ARG_STRLEN (128 KiB) however large the total ARG_MAX is, so an
// unbounded prompt fails with E2BIG. summarize.maxCondensedTranscriptBytes is
// what keeps summary prompts inside it. Windows' ~32 KiB whole-command-line
// limit is tighter than any useful transcript budget and is not covered; a
// long enough prompt still fails there, loudly.
func (a *AntigravityAgent) GenerateText(ctx context.Context, prompt string, model string) (string, error) {
	args := []string{"-p", prompt}
	if model != "" {
		args = append(args, "--model", model)
	}
	result, capturedStderr, stdoutBytes, err := agent.RunIsolatedTextGeneratorCLI(ctx, a.CommandRunner, "agy", "antigravity", args, "")
	if err != nil {
		return "", &agent.TextGenerationError{
			Err:         fmt.Errorf("antigravity text generation failed: %w", err),
			Stderr:      capturedStderr,
			StdoutBytes: stdoutBytes,
		}
	}
	return result, nil
}
