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
	"sync"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/review"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// NewReviewer returns the AgentReviewer for codex.
//
// Argv shape: codex exec --skip-git-repo-check --json [-m <model>]
// [-c model_reasoning_effort=<level>] -.
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
//
// Configured skills are passed through verbatim — NOT paraphrased. Codex's
// skill system injects a catalog of installed skills into every exec session
// and loads the matching SKILL.md when the prompt names a skill (codex's
// `$Skill` form, or plain-text/description match), so the agent runs codex's
// real review workflow rather than a generic restatement of it. An older
// version replaced `/review` with 28 words of generic instruction, which
// obscured the skill signal and degraded review quality.
//
// Native `codex exec review` is intentionally NOT used: it rejects a prompt
// when a scope flag is set and codex hooks don't fire during exec, leaving no
// channel for Entire's scope clause, per-run prompt, and checkpoint context.
// Plain `codex exec -` with the composed prompt on stdin runs the same skill
// while carrying our customization.
//
// Per-spawn overrides from RunConfig.{Model, ReasoningEffort} translate to
// codex CLI flags `-m <model>` and `-c model_reasoning_effort=<level>`,
// inserted before the trailing `-` stdin marker. Empty values are skipped —
// codex falls back to whatever the user's ~/.codex/config.toml configures.
func buildCodexReviewCmd(ctx context.Context, cfg reviewtypes.RunConfig) *exec.Cmd {
	args := []string{codexExecCommand, "--skip-git-repo-check", "--json"}
	if cfg.Model != "" {
		args = append(args, "-m", cfg.Model)
	}
	if cfg.ReasoningEffort != "" {
		args = append(args, "-c", "model_reasoning_effort="+cfg.ReasoningEffort)
	}
	args = append(args, "-")

	prompt := review.ComposeReviewPrompt(cfg)
	cmd := exec.CommandContext(ctx, "codex", args...)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = review.AppendReviewEnv(os.Environ(), "codex", cfg, prompt)
	return cmd
}

const codexExecCommand = "exec"

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
// Live-token semantics: codex's `--json` output carries `usage` ONLY on
// `turn.completed` envelopes. Verified against codex-cli 0.130.0 stdout
// for both short and long prompts — no intermediate envelope
// (item.started, item.completed, etc.) carries a usage block. Codex's
// on-disk session log (the `event_msg{type:"token_count"}` shape the
// transcript parser consumes) is a separate format, not surfaced
// through `exec --json`.
//
// Tokens are emitted at every `turn.completed` envelope so multi-turn
// runs show iterative updates in the TUI.
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
		// The rollout token tailer (started on thread.started) runs concurrently
		// and also sends on out. stopTailer halts it and waits for it to exit.
		// It MUST run before any Finished emission: ReviewerTemplate requires
		// Finished to be the last event before the channel closes, so a late
		// Tokens from the tailer would violate that contract (and a send after
		// close(out) would panic). Idempotent (sync.Once) so it's safe to call
		// explicitly before each terminal Finished AND from the defer backstop;
		// the defer (LIFO, registered after close(out)) still guards early or
		// unexpected returns.
		stop := make(chan struct{})
		var tailWG sync.WaitGroup
		var stopOnce sync.Once
		stopTailer := func() {
			stopOnce.Do(func() { close(stop) })
			tailWG.Wait()
		}
		defer stopTailer()
		out <- reviewtypes.Started{}
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, min(1024*1024, maxBuf)), maxBuf)
		var seenTurnComplete, emittedTokens, tailerStarted bool
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
			case "thread.started":
				// Launch the rollout token tailer once. codex's exec --json stdout
				// only carries usage on the terminal turn.completed, so we tail the
				// rollout file (located by thread_id) for live per-turn token totals
				// — the same source codex's interactive UI reads.
				if !tailerStarted && env.ThreadID != "" {
					tailerStarted = true
					tailWG.Add(1)
					go func(id string) {
						defer tailWG.Done()
						tailRolloutTokens(id, out, stop)
					}(env.ThreadID)
				}
			case "turn.started":
				// Turn marker — no event emitted.
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
				//
				// No Tokens emission here: codex --json only carries usage on
				// `turn.completed`. An earlier version of this parser also
				// emitted Tokens here in case a future codex build attached
				// a usage block, but that branch was unreachable against real
				// codex output and is gone — emit on turn.completed only.
			case "turn.completed":
				seenTurnComplete = true
				turnUsage = env.Usage
				// Emit Tokens at every turn boundary so multi-turn reviews
				// show iterative updates in the TUI. The post-loop emission
				// is kept as a backstop for streams that ended without a
				// turn.completed (defensive — shouldn't happen in practice).
				if env.Usage.InputTokens > 0 || env.Usage.OutputTokens > 0 {
					out <- reviewtypes.Tokens{
						In:  env.Usage.InputTokens,
						Out: env.Usage.OutputTokens,
					}
					emittedTokens = true
				}
			default:
				logging.Debug(context.Background(), "codex parser: unknown envelope type",
					slog.String("type", env.Type))
			}
		}
		if err := scanner.Err(); err != nil {
			stopTailer()
			out <- reviewtypes.RunError{Err: fmt.Errorf("read stdout: %w", err)}
			out <- reviewtypes.Finished{Success: false}
			return
		}
		if seenTurnComplete {
			// Defensive backstop: the per-turn emission inside the
			// `case "turn.completed"` arm above is the primary path and
			// already emitted Tokens for every turn with non-zero usage.
			// This fires only if no per-turn emission happened (e.g., a
			// terminal turn.completed envelope arrived without a usage
			// block — defensive against codex output drift).
			//
			// codex reports cached_input_tokens as a subset of input_tokens
			// and reasoning_output_tokens as a subset of output_tokens
			// (matching OpenAI's chat-completions usage shape), so do NOT
			// sum the subset fields — that would double-count.
			if !emittedTokens {
				out <- reviewtypes.Tokens{
					In:  turnUsage.InputTokens,
					Out: turnUsage.OutputTokens,
				}
			}
			// Stop the tailer (and wait) before Finished so no late Tokens can
			// land after the terminal event.
			stopTailer()
			// Success is hard-coded true here because codex's `turn.completed`
			// envelope has no turn-level error field in 0.130.0. If a future
			// codex version adds one (e.g., an `error` or `is_error` field on
			// the envelope), capture it into a local during the switch case and
			// thread it through here as `!turnErr` — mirroring claude's
			// `!resultErr` pattern.
			out <- reviewtypes.Finished{Success: true}
			return
		}
		stopTailer()
		out <- reviewtypes.Finished{Success: false}
	}()
	return out
}

type codexEnvelope struct {
	Type     string     `json:"type"`
	ThreadID string     `json:"thread_id"` // present on thread.started; locates the rollout file
	Item     codexItem  `json:"item"`
	Usage    codexUsage `json:"usage"`
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
