package geminicli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/review"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// NewReviewer returns the AgentReviewer for gemini.
//
// Argv shape: gemini -p " " (space placeholder to trigger headless mode).
// Prompt is piped via stdin; per gemini --help the -p flag appends to stdin
// content, so passing a single space lets stdin carry the actual prompt.
// Stdout in this mode is the assistant text directly — parsed line-by-line.
func NewReviewer() *reviewtypes.ReviewerTemplate {
	return &reviewtypes.ReviewerTemplate{
		AgentName: string(agent.AgentNameGemini),
		BuildCmd:  buildGeminiReviewCmd,
		Parser:    parseGeminiOutput,
	}
}

// buildGeminiReviewCmd builds the exec.Cmd for a gemini review run.
// Exposed at package level for test inspection of argv, stdin, and env.
func buildGeminiReviewCmd(ctx context.Context, cfg reviewtypes.RunConfig) *exec.Cmd {
	prompt := review.ComposeReviewPrompt(cfg)
	// Per the existing GenerateText implementation: pass "-p " " " as the
	// argv placeholder to trigger headless (non-interactive) mode, and pipe
	// the actual prompt via stdin to avoid argv size limits.
	cmd := exec.CommandContext(ctx, "gemini", "-p", " ")
	cmd.Stdin = strings.NewReader(prompt)
	// Agent name must equal string(ag.Name()) — adoptReviewEnv compares
	// ENTIRE_REVIEW_AGENT against it; any drift silently skips adoption.
	cmd.Env = review.AppendReviewEnv(os.Environ(), string(agent.AgentNameGemini), cfg, prompt)
	return cmd
}

// parseGeminiOutput converts gemini's -p mode stdout into a stream of Events.
// Gemini emits clean assistant output with no chrome — the parser emits Started
// once, then one AssistantText per non-empty line, then Finished{Success: true}
// on clean EOF or RunError + Finished{Success: false} on a torn stream.
//
// Exposed for golden-file contract testing.
func parseGeminiOutput(r io.Reader) <-chan reviewtypes.Event {
	out := make(chan reviewtypes.Event, 32)
	go func() {
		defer close(out)
		out <- reviewtypes.Started{}
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			out <- reviewtypes.AssistantText{Text: line}
		}
		if err := scanner.Err(); err != nil {
			out <- reviewtypes.RunError{Err: fmt.Errorf("read stdout: %w", err)}
			out <- reviewtypes.Finished{Success: false}
			return
		}
		out <- reviewtypes.Finished{Success: true}
	}()
	return out
}
