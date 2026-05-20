package codex

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

// parseCodexStream consumes `codex exec --json` NDJSON output, dispatches
// progress callbacks, and returns the final agent_message text.
func parseCodexStream(stdout io.Reader, progress agent.ProgressFn) (string, error) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), streamBufferMax)

	var (
		resultText      string
		sawTurnComplete bool
		usage           *codexStreamUsage
		turnStartedAt   time.Time
		turnDuration    time.Duration
		malformed       int
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
		var ev codexStreamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			// Codex may emit transient noise (blank lines, partial flushes); a
			// schema-incompatible line is recoverable per-event but tracked so
			// protocol regressions surface in the "no agent_message" error
			// instead of disappearing silently.
			malformed++
			continue
		}

		switch ev.Type {
		case "turn.started":
			turnStartedAt = time.Now()
			dispatch(agent.GenerationProgress{Phase: agent.PhaseConnecting})

		case "item.completed":
			// Codex emits the full agent_message in one item; we capture
			// the text but defer PhaseFirstToken until turn.completed so
			// the cached_input_tokens usage clause can be attached. The
			// CLI buffers and emits items in one chunk per turn, so there
			// is no incremental "first token" signal to surface anyway.
			if ev.Item != nil && ev.Item.Type == "agent_message" {
				resultText = ev.Item.Text
			}

		case "turn.completed":
			sawTurnComplete = true
			usage = ev.Usage
			if !turnStartedAt.IsZero() {
				turnDuration = time.Since(turnStartedAt)
			}

		case "turn.failed", "error":
			return "", fmt.Errorf("codex turn failed: %s", agent.SafeErrorMessage(line))
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("reading codex stream: %w", err)
	}
	if !sawTurnComplete {
		return "", errors.New("codex stream ended without a turn.completed event")
	}
	if resultText == "" {
		if malformed > 0 {
			return "", fmt.Errorf("codex stream produced no agent_message (%d malformed lines skipped)", malformed)
		}
		return "", errors.New("codex stream produced no agent_message")
	}
	if progress != nil {
		firstToken := agent.GenerationProgress{Phase: agent.PhaseFirstToken}
		if usage != nil {
			firstToken.InputTokens = usage.InputTokens
			firstToken.CachedInputTokens = usage.CachedInputTokens
		}
		dispatch(firstToken)

		done := agent.GenerationProgress{Phase: agent.PhaseDone}
		if usage != nil {
			done.OutputTokens = usage.OutputTokens
			done.InputTokens = usage.InputTokens
			done.CachedInputTokens = usage.CachedInputTokens
		}
		if turnDuration > 0 {
			done.DurationMs = int(turnDuration.Milliseconds())
		}
		dispatch(done)
	}
	return resultText, nil
}

// GenerateTextStreaming implements agent.StreamingTextGenerator.
func (c *CodexAgent) GenerateTextStreaming(
	ctx context.Context,
	prompt, model string,
	progress agent.ProgressFn,
) (string, error) {
	tmpl := &agent.StreamingGeneratorTemplate{
		AgentName: "codex",
		BuildCmd:  c.buildStreamCmd,
		Parser:    parseCodexStream,
		LooksLikeUnrecognizedFlag: func(stderr string) bool {
			return agent.LooksLikeUnrecognizedFlag(stderr, "json")
		},
	}

	result, err := tmpl.Generate(ctx, prompt, model, progress)
	if err != nil {
		if errors.Is(err, agent.ErrUnrecognizedStreamingFlag) {
			return c.GenerateText(ctx, prompt, model)
		}
		return "", fmt.Errorf("codex streaming generate: %w", err)
	}
	return result, nil
}

func (c *CodexAgent) buildStreamCmd(ctx context.Context, prompt, model string) *exec.Cmd {
	commandRunner := c.CommandRunner
	if commandRunner == nil {
		commandRunner = exec.CommandContext
	}
	args := []string{"exec", "--skip-git-repo-check", "--json"}
	if model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, "-")
	cmd := commandRunner(ctx, "codex", args...)
	cmd.Stdin = strings.NewReader(prompt)
	return cmd
}
