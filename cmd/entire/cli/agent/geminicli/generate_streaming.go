package geminicli

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

// parseGeminiStream consumes `gemini --output-format stream-json` NDJSON
// output, dispatches progress callbacks, and returns the concatenated
// assistant content.
//
// Real-CLI shape (gemini-cli 0.38.2):
//   - init                                              -> PhaseConnecting
//   - message(role=user)                                -> ignored (echo)
//   - message(role=assistant, delta=true)               -> PhaseFirstToken (1st)
//     / PhaseGenerating (subsequent)
//     content concatenated into result
//   - result(status=success, stats={...})               -> PhaseDone (terminal)
//   - result(status=error, error={type,message}, stats) -> returns error
//
// Note: Gemini typically emits ONE assistant message per turn rather than
// chunking deltas, so PhaseGenerating may not fire on real captures even
// though it works for multi-message fixtures.
func parseGeminiStream(stdout io.Reader, progress agent.ProgressFn) (string, error) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), streamBufferMax)

	var (
		result          strings.Builder
		sawInit         bool
		firstTokenFired bool
		stats           *geminiStreamStats
		malformed       int
		start           = time.Now()
		totalChars      int
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
		var ev geminiStreamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			// Gemini may emit transient noise (blank lines, partial flushes);
			// a schema-incompatible line is recoverable per-event but tracked
			// so protocol regressions surface in the no-content error instead
			// of disappearing silently.
			malformed++
			continue
		}

		switch ev.Type {
		case "init":
			sawInit = true
			dispatch(agent.GenerationProgress{Phase: agent.PhaseConnecting})

		case "message":
			if ev.Role != "assistant" {
				continue
			}
			result.WriteString(ev.Content)
			totalChars += len(ev.Content)
			if !firstTokenFired {
				firstTokenFired = true
				// Gemini's init/message events carry no TTFT, input-token,
				// or cached-token data; usage is deferred to the terminal
				// result event. PhaseFirstToken is dispatched without those
				// fields so the progress writer's graceful degradation
				// renders "Provider responded -- generating..." without
				// parens.
				dispatch(agent.GenerationProgress{Phase: agent.PhaseFirstToken})
			} else {
				dispatch(agent.GenerationProgress{
					Phase:        agent.PhaseGenerating,
					OutputTokens: totalChars / 4,
				})
			}

		case "result":
			stats = ev.Stats
			if ev.Status == "error" {
				detail := ""
				if ev.Error != nil {
					detail = ev.Error.Message
				}
				if detail == "" {
					// Partial decode tolerated: schema drift falls through to
					// "unspecified error" rather than leaking raw lines (which
					// may carry echoed user content or model fragments) into
					// logs and *TextGenerationError.Stderr.
					detail = "unspecified error"
				}
				return "", fmt.Errorf("gemini stream error: %s", detail)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("reading gemini stream: %w", err)
	}
	if !sawInit {
		return "", errors.New("gemini stream ended without an init event")
	}
	if !firstTokenFired {
		if malformed > 0 {
			return "", fmt.Errorf("gemini stream produced no assistant content (%d malformed lines skipped)", malformed)
		}
		return "", errors.New("gemini stream produced no assistant content")
	}
	if progress != nil {
		done := agent.GenerationProgress{Phase: agent.PhaseDone}
		if stats != nil {
			done.OutputTokens = stats.OutputTokens
			done.InputTokens = stats.InputTokens
			done.CachedInputTokens = stats.Cached
			if stats.DurationMs > 0 {
				done.DurationMs = stats.DurationMs
			}
		}
		if done.DurationMs == 0 {
			done.DurationMs = int(time.Since(start).Milliseconds())
		}
		dispatch(done)
	}
	return result.String(), nil
}

// GenerateTextStreaming implements agent.StreamingTextGenerator.
func (g *GeminiCLIAgent) GenerateTextStreaming(
	ctx context.Context,
	prompt, model string,
	progress agent.ProgressFn,
) (string, error) {
	tmpl := &agent.StreamingGeneratorTemplate{
		AgentName:   "gemini",
		DisplayName: "gemini",
		BuildCmd:    g.buildStreamCmd,
		Parser:      parseGeminiStream,
		LooksLikeUnrecognizedFlag: func(stderr string) bool {
			return agent.LooksLikeUnrecognizedFlag(stderr, "output-format", "stream-json")
		},
	}

	result, err := tmpl.Generate(ctx, prompt, model, progress)
	if err != nil {
		if errors.Is(err, agent.ErrUnrecognizedStreamingFlag) {
			return g.GenerateText(ctx, prompt, model)
		}
		return "", fmt.Errorf("gemini streaming generate: %w", err)
	}
	return result, nil
}

func (g *GeminiCLIAgent) buildStreamCmd(ctx context.Context, prompt, model string) *exec.Cmd {
	commandRunner := g.CommandRunner
	if commandRunner == nil {
		commandRunner = exec.CommandContext
	}
	args := []string{"--output-format", "stream-json", "-p", " "}
	if model != "" {
		args = append(args, "--model", model)
	}
	cmd := commandRunner(ctx, "gemini", args...)
	cmd.Stdin = strings.NewReader(prompt)
	return cmd
}
