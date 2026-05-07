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
	promptCfg.Skills = expandCodexBuiltinReview(cfg.Skills)
	args := []string{codexExecCommand, "--skip-git-repo-check", "-"}
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
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
		state := codexEventNormal
		for scanner.Scan() {
			for _, ev := range collectCodexEventsLine(scanner.Text(), &state) {
				out <- ev
			}
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

type codexEventState int

const (
	codexEventNormal codexEventState = iota
	codexEventUserBlock
	codexEventAssistantBlock
	codexEventExecAwaitCommand
	codexEventExecBlock
	codexEventAfterTokens
)

func collectCodexEventsLine(raw string, state *codexEventState) []reviewtypes.Event {
	cleaned := csiRegex.ReplaceAllString(raw, "")
	trimmed := strings.TrimSpace(cleaned)
	trimmedRight := strings.TrimRight(cleaned, " \t")

	if *state == codexEventAfterTokens {
		return nil
	}
	if isTokensUsedMarker(trimmed) {
		*state = codexEventAfterTokens
		return nil
	}

	switch *state {
	case codexEventUserBlock:
		if isCodexRoleMarker(trimmed) {
			*state = codexEventAssistantBlock
		}
		return nil
	case codexEventAssistantBlock:
		return collectCodexAssistantLine(raw, trimmed, trimmedRight, state)
	case codexEventExecAwaitCommand:
		return collectCodexExecCommandLine(trimmed, trimmedRight, state)
	case codexEventExecBlock:
		switch {
		case trimmed == "":
			*state = codexEventNormal
		case isCodexRoleMarker(trimmed):
			*state = codexEventAssistantBlock
		case isUserRoleMarker(trimmed):
			*state = codexEventUserBlock
		}
		return nil
	case codexEventNormal:
		// Continue below.
	case codexEventAfterTokens:
		return nil
	}

	if isUserRoleMarker(trimmed) {
		*state = codexEventUserBlock
		return nil
	}
	if isCodexRoleMarker(trimmed) {
		*state = codexEventAssistantBlock
		return nil
	}
	if isCodexMetadataLine(trimmed) {
		return nil
	}
	if trimmedRight == codexExecCommand {
		*state = codexEventExecAwaitCommand
		return nil
	}
	if execBlockRegex.MatchString(trimmedRight) {
		*state = codexEventExecBlock
		return []reviewtypes.Event{reviewtypes.ToolCall{Name: codexExecCommand, Args: trimmedRight}}
	}
	if line, ok := FilterLine(raw); ok {
		return []reviewtypes.Event{reviewtypes.AssistantText{Text: line}}
	}
	return nil
}

func collectCodexAssistantLine(raw, trimmed, trimmedRight string, state *codexEventState) []reviewtypes.Event {
	switch {
	case isCodexRoleMarker(trimmed):
		return nil
	case isUserRoleMarker(trimmed):
		*state = codexEventUserBlock
		return nil
	case trimmedRight == codexExecCommand:
		*state = codexEventExecAwaitCommand
		return nil
	case execBlockRegex.MatchString(trimmedRight):
		*state = codexEventExecBlock
		return []reviewtypes.Event{reviewtypes.ToolCall{Name: codexExecCommand, Args: trimmedRight}}
	case isCodexMetadataLine(trimmed):
		return nil
	default:
		if line, ok := FilterLine(raw); ok {
			return []reviewtypes.Event{reviewtypes.AssistantText{Text: line}}
		}
		return nil
	}
}

func collectCodexExecCommandLine(trimmed, trimmedRight string, state *codexEventState) []reviewtypes.Event {
	switch {
	case trimmed == "":
		*state = codexEventNormal
		return nil
	case isCodexRoleMarker(trimmed):
		*state = codexEventAssistantBlock
		return nil
	case isUserRoleMarker(trimmed):
		*state = codexEventUserBlock
		return nil
	default:
		*state = codexEventExecBlock
		return []reviewtypes.Event{reviewtypes.ToolCall{Name: codexExecCommand, Args: trimmedRight}}
	}
}
