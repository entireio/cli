package claudecode

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/entireio/cli/cmd/entire/cli/review"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// NewReviewer returns the AgentReviewer for claude-code.
//
// Argv shape: claude -p <prompt>. Plain-text stdout.
// The prompt is passed as a command-line argument; stdin is unused.
// Stdout in -p mode is the assistant's plain-text response (no JSON envelope).
func NewReviewer() *reviewtypes.ReviewerTemplate {
	return &reviewtypes.ReviewerTemplate{
		AgentName: "claude-code",
		BuildCmd:  buildReviewCmd,
		Parser:    parseClaudeOutput,
	}
}

// buildReviewCmd builds the exec.Cmd for a claude review run.
// Exposed at package level for test inspection of argv and env.
func buildReviewCmd(ctx context.Context, cfg reviewtypes.RunConfig) *exec.Cmd {
	prompt := review.ComposeReviewPrompt(cfg)
	cmd := exec.CommandContext(ctx, "claude", "-p", prompt)
	cmd.Env = review.AppendReviewEnv(os.Environ(), "claude-code", cfg, prompt)
	return cmd
}

// parseClaudeOutput converts claude's -p mode stdout into a stream of Events.
// In -p mode claude emits the assistant's response as plain text (one line per
// stdout line). The parser emits Started once, then one AssistantText per
// non-empty line, then Finished{Success: true} on clean EOF or
// RunError + Finished{Success: false} on a torn stream (scanner error).
//
// Exposed for golden-file contract testing.
func parseClaudeOutput(r io.Reader) <-chan reviewtypes.Event {
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
