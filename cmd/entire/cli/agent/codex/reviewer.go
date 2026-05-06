package codex

import (
	"bufio"
	"context"
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
// Argv shape: codex exec --skip-git-repo-check -.
// Prompt is piped via stdin (the trailing "-" tells codex to read from stdin).
// Stdout includes chrome (banners, hook notices, exec blocks, CSI sequences)
// that output_filter.go strips before emitting AssistantText events.
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
	args := []string{"exec", "--skip-git-repo-check", "-"}
	if usesCodexBuiltinReview(cfg.Skills) {
		promptCfg.Skills = withoutCodexBuiltinReview(cfg.Skills)
		args = []string{"exec", "review", "--skip-git-repo-check"}
		if cfg.ScopeBaseRef != "" {
			args = append(args, "--base", cfg.ScopeBaseRef)
		}
		args = append(args, "-")
	}
	prompt := review.ComposeReviewPrompt(promptCfg)
	cmd := exec.CommandContext(ctx, "codex", args...)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = review.AppendReviewEnv(os.Environ(), "codex", cfg, prompt)
	return cmd
}

func usesCodexBuiltinReview(skills []string) bool {
	for _, skill := range skills {
		if skill == "/review" {
			return true
		}
	}
	return false
}

func withoutCodexBuiltinReview(skills []string) []string {
	out := make([]string, 0, len(skills))
	for _, skill := range skills {
		if skill == "/review" {
			continue
		}
		out = append(out, skill)
	}
	return out
}

// parseCodexOutput wraps the reader with the chrome filter and converts
// remaining lines into a stream of Events.
// On clean EOF emits Finished{Success: true}. On a scanner error (including
// errors propagated from Strip via pipe CloseWithError) emits RunError then
// Finished{Success: false}.
//
// Exposed for golden-file contract testing.
func parseCodexOutput(r io.Reader) <-chan reviewtypes.Event {
	out := make(chan reviewtypes.Event, 32)
	go func() {
		defer close(out)
		out <- reviewtypes.Started{}
		scanner := bufio.NewScanner(Strip(r))
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
