package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/review"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// NewReviewer returns the AgentReviewer for Pi.
//
// Argv shape: pi --mode json --print --no-extensions -e <Entire extension>
// [--model <model>] <prompt> (see buildPiReviewCmd).
// The prompt is passed as a positional message because Pi's CLI accepts prompts
// as message arguments in non-interactive mode. Stdout is newline-delimited JSON
// session events; the parser maps Pi's AgentSessionEvent stream into Entire's
// review Event stream.
func NewReviewer() *reviewtypes.ReviewerTemplate {
	return &reviewtypes.ReviewerTemplate{
		AgentName: string(agent.AgentNamePi),
		Prepare:   writeReviewExtension,
		BuildCmd:  buildPiReviewCmd,
		Parser:    parsePiReviewOutput,
	}
}

// reviewExtensionName is where the review copy of Entire's extension lives,
// as a name inside the per-user cache directory.
const reviewExtensionName = "pi-review/entire-extension.ts"

// buildPiReviewCmd builds the exec.Cmd for a pi review run.
//
// The reviewer runs inside the checkout under review, and pi loads every
// project extension under .pi/extensions as code at startup, so a branch
// could run anything as the reviewing user just by being reviewed. Extension
// discovery is therefore off (--no-extensions), and Entire's own extension,
// which normally comes from that same project directory, is loaded from a copy
// the binary writes (writeReviewExtension) so the review is still captured.
// If that copy cannot be located, the review runs untracked rather than
// falling back to discovery.
func buildPiReviewCmd(ctx context.Context, cfg reviewtypes.RunConfig) *exec.Cmd {
	prompt := review.ComposeReviewPrompt(cfg)
	args := []string{"--mode", "json", "--print", "--no-extensions"}
	if extPath, err := reviewExtensionPath(); err == nil {
		args = append(args, "--extension", extPath)
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	args = append(args, prompt)
	cmd := exec.CommandContext(ctx, "pi", args...)
	cmd.Env = review.AppendReviewEnv(os.Environ(), string(agent.AgentNamePi), cfg, prompt)
	return cmd
}

// writeReviewExtension writes the extension buildPiReviewCmd loads, rendered
// from the binary. Rewritten on every run so it always matches this binary.
func writeReviewExtension(context.Context) error {
	root, err := userdirs.CacheRoot()
	if err != nil {
		return fmt.Errorf("resolve cache dir: %w", err)
	}
	if err := osroot.MkdirAllNoSymlink(root, path.Dir(reviewExtensionName), 0o700); err != nil {
		return fmt.Errorf("create pi review extension dir: %w", err)
	}
	// 0644 because pi reads the extension itself.
	if err := jsonutil.WriteFileAtomicIn(root, reviewExtensionName, []byte(renderExtension()), 0o644); err != nil {
		return fmt.Errorf("write pi review extension: %w", err)
	}
	return nil
}

// reviewExtensionPath is the absolute path of the file writeReviewExtension
// writes, for pi's argv.
func reviewExtensionPath() (string, error) {
	root, err := userdirs.CacheRoot()
	if err != nil {
		return "", fmt.Errorf("resolve cache dir: %w", err)
	}
	return filepath.Join(root.Name(), filepath.FromSlash(reviewExtensionName)), nil
}

func parsePiReviewOutput(r io.Reader) <-chan reviewtypes.Event {
	out := make(chan reviewtypes.Event, 32)
	go func() {
		defer close(out)
		out <- reviewtypes.Started{}

		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, min(1024*1024, piReviewMaxScannerBuf)), piReviewMaxScannerBuf)
		messageIDsWithTextDelta := map[string]struct{}{}
		messageIDsWithUsage := map[string]struct{}{}
		messageUsageByTurn := map[int]map[piReviewUsageKey]struct{}{}
		turnNumber := 0
		tokens := reviewtypes.Tokens{}
		finished := false
		success := true

		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			var env piReviewEnvelope
			if err := json.Unmarshal(line, &env); err != nil {
				out <- reviewtypes.RunError{Err: fmt.Errorf("pi --mode json: %w", err)}
				continue
			}

			switch env.Type {
			case "turn_start":
				turnNumber++
			case "session", "agent_start", "queue_update", "compaction_start", "compaction_end", "auto_retry_start", "auto_retry_end":
				// Session/control events do not map to user-visible review output.
			case "message_update":
				if text := env.AssistantMessageEvent.TextDelta(); text != "" {
					messageIDsWithTextDelta[env.MessageID()] = struct{}{}
					out <- reviewtypes.AssistantText{Text: text}
				}
			case "message_end":
				if env.Message.Role == "assistant" {
					if env.Message.StopReason == "error" || env.Message.StopReason == "aborted" {
						success = false
					}
					if env.Message.Usage != nil {
						emitPiReviewTokens(out, env, &tokens, messageIDsWithUsage, messageUsageByTurn, turnNumber)
					}
					if _, sawDelta := messageIDsWithTextDelta[env.MessageID()]; !sawDelta {
						if text := piReviewMessageText(env.Message.Content); text != "" {
							out <- reviewtypes.AssistantText{Text: text}
						}
					}
				}
			case "tool_execution_start":
				out <- reviewtypes.ToolCall{Name: env.ToolName, Args: piReviewJSONArg(env.Args)}
			case "tool_execution_end":
				// Tool errors are part of normal agent execution (for example grep
				// finding no matches). The agent's stopReason determines review
				// success/failure.
			case "turn_end":
				if env.Message.StopReason == "error" || env.Message.StopReason == "aborted" {
					success = false
				}
				if env.Message.Usage != nil {
					emitPiReviewTokens(out, env, &tokens, messageIDsWithUsage, messageUsageByTurn, turnNumber)
				}
			case "agent_end":
				finished = true
				out <- reviewtypes.Finished{Success: success}
			default:
				// Unknown future events are ignored; Pi's event stream is additive.
			}
		}

		if err := scanner.Err(); err != nil {
			out <- reviewtypes.RunError{Err: fmt.Errorf("read stdout: %w", err)}
			out <- reviewtypes.Finished{Success: false}
			return
		}
		if !finished {
			out <- reviewtypes.Finished{Success: false}
		}
	}()
	return out
}

const piReviewMaxScannerBuf = 64 * 1024 * 1024

type piReviewEnvelope struct {
	Type                  string                  `json:"type"`
	ID                    string                  `json:"id"`
	Message               piReviewMessage         `json:"message"`
	AssistantMessageEvent piAssistantMessageEvent `json:"assistantMessageEvent"`
	ToolName              string                  `json:"toolName"`
	Args                  json.RawMessage         `json:"args"`
}

func (e piReviewEnvelope) MessageID() string {
	if e.Message.ID != "" {
		return e.Message.ID
	}
	return e.ID
}

type piReviewMessage struct {
	ID         string          `json:"id"`
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Usage      *piReviewUsage  `json:"usage"`
	StopReason string          `json:"stopReason"`
}

type piAssistantMessageEvent struct {
	Type  string `json:"type"`
	Delta string `json:"delta"`
	Text  string `json:"text"`
}

func (e piAssistantMessageEvent) TextDelta() string {
	switch e.Type {
	case "text_delta":
		if e.Delta != "" {
			return e.Delta
		}
		return e.Text
	default:
		return ""
	}
}

type piReviewUsage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cacheRead"`
	CacheWrite int `json:"cacheWrite"`
}

type piReviewUsageKey struct {
	Input      int
	Output     int
	CacheRead  int
	CacheWrite int
}

func emitPiReviewTokens(out chan<- reviewtypes.Event, env piReviewEnvelope, total *reviewtypes.Tokens, seen map[string]struct{}, messageUsageByTurn map[int]map[piReviewUsageKey]struct{}, turnNumber int) {
	if env.Message.Usage == nil || total == nil {
		return
	}
	if shouldSkipPiReviewUsage(env, seen, messageUsageByTurn, turnNumber) {
		return
	}
	*total = addPiReviewTokens(*total, env.Message.Usage)
	out <- *total
}

func shouldSkipPiReviewUsage(env piReviewEnvelope, seen map[string]struct{}, messageUsageByTurn map[int]map[piReviewUsageKey]struct{}, turnNumber int) bool {
	usage := env.Message.Usage
	if usage == nil {
		return true
	}
	sig := piReviewUsageKey{Input: usage.Input, Output: usage.Output, CacheRead: usage.CacheRead, CacheWrite: usage.CacheWrite}
	if env.Type == "message_end" && messageUsageByTurn != nil {
		if messageUsageByTurn[turnNumber] == nil {
			messageUsageByTurn[turnNumber] = map[piReviewUsageKey]struct{}{}
		}
		messageUsageByTurn[turnNumber][sig] = struct{}{}
	}
	if key := env.MessageID(); key != "" {
		if _, ok := seen[key]; ok {
			return true
		}
		seen[key] = struct{}{}
		return false
	}
	// Pi streams can emit usage on both message_end and turn_end. Some realistic
	// streams omit ids on both events, so fall back to the current turn's usage
	// signature to avoid counting a no-id turn_end duplicate of the message_end.
	if env.Type == "turn_end" && messageUsageByTurn != nil {
		if _, ok := messageUsageByTurn[turnNumber][sig]; ok {
			return true
		}
	}
	return false
}

func addPiReviewTokens(total reviewtypes.Tokens, usage *piReviewUsage) reviewtypes.Tokens {
	if usage == nil {
		return total
	}
	// Pi's usage shape is normalized across providers. For OpenAI-shaped
	// backends, cached input is reported as a subset of input tokens; summing
	// cacheRead/cacheWrite into Tokens.In would therefore double-count. The
	// review event contract has only aggregate input/output fields, so report
	// the provider's top-level input total and leave cache detail to transcript
	// token accounting, which stores cache fields separately.
	total.In += usage.Input
	total.Out += usage.Output
	return total
}

func piReviewJSONArg(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	return string(raw)
}

func piReviewMessageText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var items []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return ""
	}
	text := ""
	for _, item := range items {
		if item.Type != "text" || item.Text == "" {
			continue
		}
		if text != "" {
			text += "\n"
		}
		text += item.Text
	}
	return text
}
