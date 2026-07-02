// Package review — see env.go for package-level rationale.
//
// synthesis_sink.go provides SynthesisSink, the master adjudication phase of a
// multi-agent review: after all worker agents finish, it asks a configured
// provider to consolidate the per-agent narratives into a final report.
// Skipped silently on cancellation or when fewer than 2 successful agents
// produced usable output. The report runs unconditionally (no y/N prompt) and works in
// both TTY and redirected/CI output.
//
// Composition: appended AFTER DumpSink in the multi-agent sink slice, so the
// final report renders below the per-agent narrative dump.
package review

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	agenttypes "github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/mdrender"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// SynthesisProvider abstracts the LLM call that produces the cross-agent
// verdict. Injected so tests can stub the provider call without a real API
// roundtrip; production wiring uses AgentSynthesisProvider.
type SynthesisProvider interface {
	// Synthesize takes the composed synthesis prompt and returns the
	// verdict text. On error the sink prints "final report unavailable" and
	// signals OnError; the command then surfaces an attempted-but-failed
	// synthesis as a non-zero exit (see runMultiAgentPath).
	Synthesize(ctx context.Context, prompt string) (string, error)
}

// AgentSynthesisProvider asks a named agent's text-generation API to produce
// the final report. This is the profile-native master implementation used by
// `entire review`: workers run as review sessions, while the master is an
// isolated text-generation call so it consolidates reports without creating a
// second review worker session.
type AgentSynthesisProvider struct {
	AgentName string
	Model     string
}

func (p AgentSynthesisProvider) Synthesize(ctx context.Context, prompt string) (string, error) {
	ag, err := agent.Get(agenttypes.AgentName(p.AgentName))
	if err != nil {
		return "", fmt.Errorf("resolve master agent %s: %w", p.AgentName, err)
	}
	tg, ok := agent.AsTextGenerator(ag)
	if !ok {
		return "", fmt.Errorf("master agent %s does not support text generation", p.AgentName)
	}
	return tg.GenerateText(ctx, prompt, p.Model) //nolint:wrapcheck // caller owns display
}

// SynthesisSink composes a multi-agent verdict by calling a configured
// provider after the run finishes — the profile's master adjudication phase.
// Provider is the profile master, so the final report is produced
// unconditionally (no y/N prompt). AgentEvent is a no-op; all work happens in
// RunFinished.
type SynthesisSink struct {
	Provider SynthesisProvider
	Writer   io.Writer
	// RenderWriter is the writer whose terminal capabilities should be used for
	// markdown rendering. It defaults to Writer. TTY review runs use this when
	// Writer is a post-run buffer that will later flush to the real terminal.
	RenderWriter    io.Writer
	PerRunPrompt    string // if non-empty, included in the synthesis prompt for context
	ProfileName     string
	Task            string
	MasterName      string
	RunContext      context.Context // optional; nil falls back to context.Background()
	ProviderTimeout time.Duration   // positive: use it; zero: defaultSynthesisProviderTimeout; negative: disabled (no deadline)
	OnResult        func(result string)
	OnStart         func()
	OnComplete      func(error)
	// OnError is called when an attempted synthesis fails (provider error or
	// timeout). It is NOT called when synthesis is skipped (cancelled run or
	// fewer than two usable reviewers). Lets the caller surface a missing verdict
	// in the command's exit status instead of exiting 0 with no final report.
	OnError func(error)
}

// Compile-time interface check.
var _ reviewtypes.Sink = SynthesisSink{}

// defaultSynthesisProviderTimeout bounds the judge's single consolidation call
// when SynthesisSink.ProviderTimeout is unset. The judge reads every reviewer's
// report and writes the combined verdict in one text-generation call, which
// regularly needs more than the original 2m, so the default is 5m.
const defaultSynthesisProviderTimeout = 5 * time.Minute

// AgentEvent is a no-op; SynthesisSink only acts in RunFinished.
func (SynthesisSink) AgentEvent(_ string, _ reviewtypes.Event) {}

// RunFinished synthesizes a cross-agent final report.
//
// Skip silently when:
//   - the run was cancelled (summary.Cancelled)
//   - fewer than 2 successful agents produced usable output
//
// The master phase is mandatory and runs without a y/N prompt, in TTY and
// redirected output alike. On provider failure: print "final report
// unavailable: <err>" with the underlying error; the user can still commit.
func (s SynthesisSink) RunFinished(summary reviewtypes.RunSummary) {
	if summary.Cancelled {
		return
	}
	if usableAgentCount(summary) < 2 {
		return
	}

	synthesisPrompt := composeSynthesisPrompt(summary, s.PerRunPrompt, s.ProfileName, s.Task)
	providerCtx, cancelProvider := s.providerContext()
	defer cancelProvider()
	if s.MasterName != "" {
		fmt.Fprintf(s.Writer, "Generating final report with %s...\n", s.MasterName)
	} else {
		fmt.Fprintln(s.Writer, "Generating final report...")
	}
	if s.OnStart != nil {
		s.OnStart()
	}
	result, provErr := s.Provider.Synthesize(providerCtx, synthesisPrompt)
	if provErr != nil {
		fmt.Fprintf(s.Writer, "final report unavailable: %v\n", provErr)
		if s.OnError != nil {
			s.OnError(provErr)
		}
		if s.OnComplete != nil {
			s.OnComplete(provErr)
		}
		return
	}
	if s.OnResult != nil {
		s.OnResult(result)
	}
	// The synthesis verdict is markdown — render it through the same palette
	// dispatch / DumpSink use, so multi-agent reviews finish with a visually
	// consistent block. Non-TTY writers receive raw markdown unchanged.
	//
	// Use Fprint (not Fprintln): mdrender.Render returns glamour output that
	// already ends with a newline, and the raw-markdown fallback path has its
	// own terminal newline from the LLM response. Adding Fprintln would double
	// the trailing blank line.
	rendered, err := mdrender.RenderForWriter(s.renderWriter(), result)
	if err != nil {
		rendered = result
	}
	fmt.Fprint(s.Writer, rendered)
	if s.OnComplete != nil {
		s.OnComplete(nil)
	}
}

func (s SynthesisSink) renderWriter() io.Writer {
	if s.RenderWriter != nil {
		return s.RenderWriter
	}
	return s.Writer
}

func (s SynthesisSink) runContext() context.Context {
	if s.RunContext != nil {
		return s.RunContext
	}
	return context.Background()
}

// providerContext bounds the judge's consolidation call. ProviderTimeout follows
// the same three-state convention as the reviewer timeout so a single --timeout
// value can govern the whole command:
//   - positive: use it.
//   - zero (unset): use defaultSynthesisProviderTimeout.
//   - negative: disabled — no deadline (mirrors `--timeout 0` for reviewers).
func (s SynthesisSink) providerContext() (context.Context, context.CancelFunc) {
	switch {
	case s.ProviderTimeout < 0:
		return context.WithCancel(s.runContext())
	case s.ProviderTimeout > 0:
		return context.WithTimeout(s.runContext(), s.ProviderTimeout)
	default:
		return context.WithTimeout(s.runContext(), defaultSynthesisProviderTimeout)
	}
}

// usableAgentCount returns the number of agents that produced usable narrative
// output (non-empty AssistantText in their buffer). Used to determine whether
// synthesis is worth offering.
func usableAgentCount(summary reviewtypes.RunSummary) int {
	return len(usableAgentRuns(summary))
}
