package copilotcli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

const streamBufferMax = 4 * 1024 * 1024 // 4 MiB

// parseCopilotStream consumes `copilot --output-format json --stream on` NDJSON
// output, dispatches progress callbacks, and returns the concatenated assistant
// content.
//
// Real-CLI shape (GitHub Copilot CLI 1.0.48):
//   - session.* / user.message                           → ignored (bootstrap + echo)
//   - assistant.turn_start                               → PhaseConnecting
//   - assistant.message_start                            → ignored (ephemeral marker)
//   - assistant.message_delta (deltaContent)             → PhaseFirstToken (1st) /
//     PhaseGenerating (subsequent),
//     content concatenated
//   - assistant.message (non-ephemeral, outputTokens)    → captures token count
//   - assistant.turn_end                                 → turn marker, no-op
//   - result (exitCode, usage.totalApiDurationMs)        → PhaseDone (terminal);
//     non-zero exitCode is
//     treated as a stream error
//
// The "error" event shape is defensive: Copilot's current public stream
// typically reports failures via non-zero result.exitCode plus a stderr
// message, not an in-stream JSON envelope. The error-event handler is kept
// for forward-compat with potential future CLI versions that adopt one.
func parseCopilotStream(stdout io.Reader, progress agent.ProgressFn) (string, error) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), streamBufferMax)

	var (
		result          strings.Builder
		connectingFired bool
		firstTokenFired bool
		sawTerminal     bool
		usage           *copilotStreamUsage
		outputTokens    int
		streamedChars   int
		malformed       int
		start           = time.Now()
	)

	dispatch := func(p agent.GenerationProgress) {
		if progress != nil {
			progress(p)
		}
	}

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev copilotStreamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			// Copilot may emit transient noise (blank lines, partial flushes); a
			// schema-incompatible line is recoverable per-event but tracked so
			// protocol regressions surface in the no-content error instead of
			// disappearing silently.
			malformed++
			continue
		}

		switch ev.Type {
		case "assistant.turn_start":
			// Copilot emits one assistant.turn_start per tool-using turn. The
			// progress UI expects PhaseConnecting once per generation, so we
			// fire only on the first turn.
			if !connectingFired {
				connectingFired = true
				dispatch(agent.GenerationProgress{Phase: agent.PhaseConnecting})
			}

		case "assistant.message_delta":
			if ev.Data == nil || ev.Data.DeltaContent == "" {
				continue
			}
			if !firstTokenFired {
				firstTokenFired = true
				// Copilot's delta events carry no TTFT, input-token, or
				// cached-token data; the progress writer's graceful
				// degradation will render "Provider responded -- generating..."
				// without parens.
				dispatch(agent.GenerationProgress{Phase: agent.PhaseFirstToken})
			} else {
				// Estimate running output tokens from accumulated character
				// count so the writer's "Writing summary... (~Nk tokens)"
				// counter ticks during streaming (matches Claude's pattern
				// from PR #964). Authoritative output_tokens still arrives
				// later via assistant.message events and overrides on Done.
				streamedChars += len(ev.Data.DeltaContent)
				dispatch(agent.GenerationProgress{Phase: agent.PhaseGenerating, OutputTokens: streamedChars / 4})
			}
			result.WriteString(ev.Data.DeltaContent)

		case "assistant.message":
			// Each non-ephemeral assistant.message carries the outputTokens for
			// one turn. Multi-turn generations (tool-using runs) emit several;
			// sum them so PhaseDone reports the total output tokens for the
			// whole generation.
			if ev.Data != nil && ev.Data.OutputTokens > 0 {
				outputTokens += ev.Data.OutputTokens
			}

		case "result":
			sawTerminal = true
			usage = ev.Usage
			if ev.ExitCode != 0 {
				// Partial decode tolerated: schema drift falls through to
				// "unspecified error" rather than leaking the raw line (which
				// may carry echoed user content or model fragments) into logs
				// and *TextGenerationError.Stderr.
				return "", fmt.Errorf("copilot stream error: non-zero exit code %d", ev.ExitCode)
			}

		case "error":
			return "", fmt.Errorf("copilot stream error: %s", agent.SafeErrorMessage(line))
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("reading copilot stream: %w", err)
	}
	if !firstTokenFired {
		if malformed > 0 {
			return "", fmt.Errorf("copilot stream produced no assistant content (%d malformed lines skipped)", malformed)
		}
		return "", errors.New("copilot stream produced no assistant content")
	}
	if progress != nil {
		done := agent.GenerationProgress{Phase: agent.PhaseDone}
		if outputTokens > 0 {
			done.OutputTokens = outputTokens
		}
		if usage != nil && usage.TotalAPIDurationMs > 0 {
			done.DurationMs = usage.TotalAPIDurationMs
		}
		if done.DurationMs == 0 {
			// EOF / no-terminal fallback: compute locally so the progress
			// writer always has a duration to render.
			done.DurationMs = int(time.Since(start).Milliseconds())
		}
		dispatch(done)
	}
	_ = sawTerminal // kept for diagnostic clarity; absence is acceptable (EOF fallback)
	return result.String(), nil
}

// GenerateTextStreaming implements agent.StreamingTextGenerator.
func (c *CopilotCLIAgent) GenerateTextStreaming(
	ctx context.Context,
	prompt, model string,
	progress agent.ProgressFn,
) (string, error) {
	tmpl := &agent.StreamingGeneratorTemplate{
		AgentName:   "copilot-cli",
		DisplayName: "copilot",
		BuildCmd:    c.buildStreamCmd,
		Parser:      parseCopilotStream,
		LooksLikeUnrecognizedFlag: func(stderr string) bool {
			return agent.LooksLikeUnrecognizedFlag(stderr, "stream", "output-format")
		},
	}

	result, err := tmpl.Generate(ctx, prompt, model, progress)
	if err != nil {
		if errors.Is(err, agent.ErrUnrecognizedStreamingFlag) {
			return c.GenerateText(ctx, prompt, model)
		}
		return "", fmt.Errorf("copilot streaming generate: %w", err)
	}
	return result, nil
}

func (c *CopilotCLIAgent) buildStreamCmd(ctx context.Context, prompt, model string) *exec.Cmd {
	commandRunner := c.CommandRunner
	if commandRunner == nil {
		commandRunner = exec.CommandContext
	}
	args := []string{"--output-format", "json", "--stream", "on", "--allow-all-tools", "--disable-builtin-mcps"}
	if model != "" {
		args = append(args, "--model", model)
	}
	cmd := commandRunner(ctx, "copilot", args...)
	cmd.Stdin = strings.NewReader(prompt)
	return cmd
}
