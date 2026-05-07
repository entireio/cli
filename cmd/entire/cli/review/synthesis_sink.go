// Package review — see env.go for package-level rationale.
//
// synthesis_sink.go provides SynthesisSink, an opt-in Sink that prompts the
// user (y/N, default N) after all agents finish, then asks a configured
// summary provider to synthesize a unified verdict across the per-agent
// narratives. Skipped silently in non-TTY mode, on cancellation, or when
// fewer than 2 agents produced usable output.
//
// Composition: appended AFTER DumpSink in TTY-mode sink slices, so the
// y/N prompt appears below the per-agent narrative dump.
package review

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"charm.land/huh/v2"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/mdrender"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// SynthesisProvider abstracts the LLM call that produces the cross-agent
// verdict. Injected via Deps so tests can stub the provider call without a
// real API roundtrip. Production wiring (in review_bridge.go) calls into
// the same provider entire explain uses.
type SynthesisProvider interface {
	// Synthesize takes the composed synthesis prompt and returns the
	// verdict text. Errors are surfaced to the caller; SynthesisSink
	// degrades gracefully on error rather than failing the run.
	Synthesize(ctx context.Context, prompt string) (string, error)
}

// SynthesisSink composes a multi-agent verdict by calling a configured
// summary provider after the run finishes. AgentEvent is a no-op; all
// work happens in RunFinished.
type SynthesisSink struct {
	Provider        SynthesisProvider
	Writer          io.Writer
	InputTTY        bool // true if stdin can prompt the user
	PromptYN        func(ctx context.Context, question string, def bool) (bool, error)
	PerRunPrompt    string          // if non-empty, included in the synthesis prompt for context
	RunContext      context.Context // optional; nil falls back to context.Background()
	ProviderTimeout time.Duration   // optional; zero uses defaultSynthesisProviderTimeout
}

// Compile-time interface check.
var _ reviewtypes.Sink = SynthesisSink{}

const defaultSynthesisProviderTimeout = 2 * time.Minute

// AgentEvent is a no-op; SynthesisSink only acts in RunFinished.
func (SynthesisSink) AgentEvent(_ string, _ reviewtypes.Event) {}

// RunFinished optionally synthesizes a cross-agent verdict.
//
// Skip silently when:
//   - stdin isn't a TTY (s.InputTTY == false)
//   - the run was cancelled (summary.Cancelled)
//   - fewer than 2 agents produced usable output (status Succeeded or Failed
//     with non-empty narrative buffer)
//
// Otherwise prompt y/N (default N). On y: compose prompt, call provider,
// print response. On provider failure: print "synthesis unavailable: <err>"
// with the underlying error; user can still commit.
func (s SynthesisSink) RunFinished(summary reviewtypes.RunSummary) {
	if !s.InputTTY {
		return
	}
	if summary.Cancelled {
		return
	}
	if usableAgentCount(summary) < 2 {
		return
	}

	ctx := s.runContext()
	promptFn := s.PromptYN
	if promptFn == nil {
		promptFn = realPromptYN
	}

	yes, err := promptFn(ctx, "Synthesize a unified verdict across all agent reviews?", false)
	if err != nil {
		// huh form errors (terminal-resize anomalies, stdin EOF, stub
		// failures) shouldn't block the user from committing — they get the
		// same silent skip as a "no" answer. Logged at debug for diagnostics.
		logging.Debug(ctx, "synthesis prompt error",
			slog.String("error", err.Error()))
		return
	}
	if !yes {
		return
	}

	synthesisPrompt := composeSynthesisPrompt(summary, s.PerRunPrompt)
	providerCtx, cancelProvider := s.providerContext()
	defer cancelProvider()
	fmt.Fprintln(s.Writer, "Generating summary...")
	result, provErr := s.Provider.Synthesize(providerCtx, synthesisPrompt)
	if provErr != nil {
		fmt.Fprintf(s.Writer, "synthesis unavailable: %v\n", provErr)
		return
	}
	// The synthesis verdict is markdown — render it through the same palette
	// dispatch / DumpSink use, so multi-agent reviews finish with a visually
	// consistent block. Non-TTY writers receive raw markdown unchanged.
	//
	// Use Fprint (not Fprintln): mdrender.Render returns glamour output that
	// already ends with a newline, and the raw-markdown fallback path has its
	// own terminal newline from the LLM response. Adding Fprintln would double
	// the trailing blank line.
	rendered, err := mdrender.RenderForWriter(s.Writer, result)
	if err != nil {
		rendered = result
	}
	fmt.Fprint(s.Writer, rendered)
}

func (s SynthesisSink) runContext() context.Context {
	if s.RunContext != nil {
		return s.RunContext
	}
	return context.Background()
}

func (s SynthesisSink) providerContext() (context.Context, context.CancelFunc) {
	timeout := s.ProviderTimeout
	if timeout <= 0 {
		timeout = defaultSynthesisProviderTimeout
	}
	return context.WithTimeout(s.runContext(), timeout)
}

// usableAgentCount returns the number of agents that produced usable narrative
// output (non-empty AssistantText in their buffer). Used to determine whether
// synthesis is worth offering.
func usableAgentCount(summary reviewtypes.RunSummary) int {
	return len(usableAgentRuns(summary))
}

// realPromptYN is the production y/N prompt using a huh Confirm form.
// Default is false (N). On user cancellation (Ctrl+C) returns false, nil so
// the caller treats it as a "no" answer; on real form errors the error is
// returned so RunFinished can log it via the debug-error path.
func realPromptYN(ctx context.Context, question string, def bool) (bool, error) {
	answer := def
	form := newAccessibleForm(huh.NewGroup(
		huh.NewConfirm().
			Title(question).
			Value(&answer),
	))
	if err := form.RunWithContext(ctx); err != nil {
		if errors.Is(err, huh.ErrUserAborted) || errors.Is(err, context.Canceled) {
			return false, nil
		}
		return false, fmt.Errorf("synthesis confirm form: %w", err)
	}
	return answer, nil
}
