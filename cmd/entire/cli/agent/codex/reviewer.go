package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/review"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// NewReviewer returns the AgentReviewer for codex.
//
// Argv shape: codex exec --skip-git-repo-check --json -.
// Prompt is piped via stdin (the trailing "-" tells codex to read from stdin).
// Stdout is newline-delimited JSON envelopes (one event per line); no chrome
// filter needed — each line is parsed directly into an Event.
func NewReviewer() *reviewtypes.ReviewerTemplate {
	return &reviewtypes.ReviewerTemplate{
		AgentName: "codex",
		BuildCmd:  buildCodexReviewCmd,
		Parser:    parseCodexOutput,
	}
}

// buildCodexReviewCmd builds the exec.Cmd for a codex review run.
// Exposed at package level for test inspection of argv, stdin, and env.
func buildCodexReviewCmd(ctx context.Context, cfg reviewtypes.RunConfig) *exec.Cmd {
	promptCfg := cfg
	promptCfg.Skills = expandCodexBuiltinReview(cfg.Skills)
	args := []string{codexExecCommand, "--skip-git-repo-check", "--json", "-"}
	prompt := review.ComposeReviewPrompt(promptCfg)
	cmd := exec.CommandContext(ctx, "codex", args...)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = review.AppendReviewEnv(os.Environ(), "codex", cfg, prompt)
	return cmd
}

// Codex's native `exec review --base <branch>` rejects an additional prompt,
// so expand `/review` into text and run normal `codex exec -`. That preserves
// Entire's scoped base clause, per-run instructions, and checkpoint context.
const codexBuiltinReviewPrompt = "Review the current branch changes and report actionable findings. " +
	"Prioritize correctness, regressions, security, and missing test coverage. Do not make code changes."

const codexExecCommand = "exec"

func expandCodexBuiltinReview(skills []string) []string {
	out := make([]string, 0, len(skills))
	for _, skill := range skills {
		if skill == "/review" {
			out = append(out, codexBuiltinReviewPrompt)
			continue
		}
		out = append(out, skill)
	}
	return out
}

// parseCodexOutput converts codex's `exec --json` stdout into a stream of
// Events. Each stdout line is one JSON envelope (top-level "type" field).
//
// Envelope types this parser handles:
//   - thread.started        session id; swallowed
//   - turn.started          marker; swallowed
//   - item.started          tool invocation begins → emits ToolCall when
//     item.type == "command_execution"
//   - item.completed        tool invocation ends OR an agent_message;
//     agent_message → AssistantText
//     command_execution → swallowed (start already announced)
//   - turn.completed        terminal usage block → Tokens, then Finished
//
// On a scanner error or a missing turn.completed envelope, emits RunError
// (scanner) or Finished{Success: false} (missing turn) accordingly.
//
// Tokens are emitted only at the terminal `turn.completed` envelope, not
// incrementally — codex's usage fields land once at end-of-turn.
//
// Package-private; called directly from this package's tests so they can
// drive raw stdout fixtures through the parser without going through the
// ReviewerTemplate.Start spawn path.
func parseCodexOutput(r io.Reader) <-chan reviewtypes.Event {
	return parseCodexOutputBuf(r, codexReviewMaxScannerBuf)
}

// codexReviewMaxScannerBuf is the production bufio.Scanner cap for the codex
// review parser. Codex packs the entire stdout of `command_execution` tools
// into the aggregated_output field on item.completed envelopes inline, so a
// chatty grep/cat/find over a large repo can put many MB into one envelope.
// 16MB was too tight; 64MB is generous without imposing real memory cost
// (one buffer per active review run).
const codexReviewMaxScannerBuf = 64 * 1024 * 1024

// parseCodexOutputBuf is the parameterized variant of parseCodexOutput, used
// by tests to shrink the scanner cap so the "token too long" branch can be
// exercised without writing 64MB of fixture data.
func parseCodexOutputBuf(r io.Reader, maxBuf int) <-chan reviewtypes.Event {
	out := make(chan reviewtypes.Event, 32)
	go func() {
		defer close(out)
		out <- reviewtypes.Started{}
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, min(1024*1024, maxBuf)), maxBuf)
		var seenTurnComplete bool
		var turnUsage codexUsage
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			var env codexEnvelope
			if err := json.Unmarshal(line, &env); err != nil {
				out <- reviewtypes.RunError{Err: fmt.Errorf("codex --json: %w", err)}
				continue
			}
			// Add cases here when codex's envelope or item types grow; the
			// default arm logs unknown types at Debug so drift can be
			// triaged via ENTIRE_LOG_LEVEL=debug.
			switch env.Type {
			case "thread.started", "turn.started":
				// Session/turn markers — no event emitted.
			case "item.started":
				if env.Item.Type == "command_execution" {
					out <- reviewtypes.ToolCall{Name: "exec", Args: env.Item.Command}
				}
			case "item.completed":
				if env.Item.Type == "agent_message" && env.Item.Text != "" {
					out <- reviewtypes.AssistantText{Text: env.Item.Text}
				}
				// command_execution completion is intentionally swallowed —
				// item.started already announced it. aggregated_output is the
				// tool's stdout, not the model's narrative.
			case "turn.completed":
				seenTurnComplete = true
				turnUsage = env.Usage
			default:
				logging.Debug(context.Background(), "codex parser: unknown envelope type",
					slog.String("type", env.Type))
			}
		}
		if err := scanner.Err(); err != nil {
			out <- reviewtypes.RunError{Err: fmt.Errorf("read stdout: %w", err)}
			out <- reviewtypes.Finished{Success: false}
			return
		}
		if seenTurnComplete {
			// codex reports cached_input_tokens as a subset of input_tokens
			// and reasoning_output_tokens as a subset of output_tokens
			// (matching OpenAI's chat-completions usage shape), so do NOT
			// sum the subset fields — that would double-count.
			out <- reviewtypes.Tokens{
				In:  turnUsage.InputTokens,
				Out: turnUsage.OutputTokens,
			}
			// Success is hard-coded true here because codex's `turn.completed`
			// envelope has no turn-level error field in 0.130.0. If a future
			// codex version adds one (e.g., an `error` or `is_error` field on
			// the envelope), capture it into a local during the switch case and
			// thread it through here as `!turnErr` — mirroring claude's
			// `!resultErr` pattern.
			out <- reviewtypes.Finished{Success: true}
			return
		}
		out <- reviewtypes.Finished{Success: false}
	}()
	return out
}

type codexEnvelope struct {
	Type  string     `json:"type"`
	Item  codexItem  `json:"item"`
	Usage codexUsage `json:"usage"`
}

type codexItem struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Text    string `json:"text"`
}

type codexUsage struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
}
