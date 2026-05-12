package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

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
// Envelope types observed in codex 0.130.0:
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
// Exposed for golden-file contract testing.
func parseCodexOutput(r io.Reader) <-chan reviewtypes.Event {
	out := make(chan reviewtypes.Event, 32)
	go func() {
		defer close(out)
		out <- reviewtypes.Started{}
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
		var seenTurnComplete bool
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
			switch env.Type {
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
				// codex reports cached_input_tokens as a subset of input_tokens
				// and reasoning_output_tokens as a subset of output_tokens
				// (matching OpenAI's chat-completions usage shape), so do NOT
				// sum the subset fields — that would double-count.
				out <- reviewtypes.Tokens{
					In:  env.Usage.InputTokens,
					Out: env.Usage.OutputTokens,
				}
				out <- reviewtypes.Finished{Success: true}
			}
		}
		if err := scanner.Err(); err != nil {
			out <- reviewtypes.RunError{Err: fmt.Errorf("read stdout: %w", err)}
			out <- reviewtypes.Finished{Success: false}
			return
		}
		if !seenTurnComplete {
			out <- reviewtypes.Finished{Success: false}
		}
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
