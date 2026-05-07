package review_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/review"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// stubSynthesisProvider is a test double for SynthesisProvider that records
// the prompt it received and returns a canned response.
type stubSynthesisProvider struct {
	capturedPrompt string
	response       string
	err            error
}

func (s *stubSynthesisProvider) Synthesize(_ context.Context, prompt string) (string, error) {
	s.capturedPrompt = prompt
	if s.err != nil {
		return "", s.err
	}
	return s.response, nil
}

type contextWaitingSynthesisProvider struct {
	capturedPrompt string
	capturedErr    error
}

func (s *contextWaitingSynthesisProvider) Synthesize(ctx context.Context, prompt string) (string, error) {
	s.capturedPrompt = prompt
	<-ctx.Done()
	s.capturedErr = ctx.Err()
	return "", ctx.Err()
}

// buildSink is a helper to construct a SynthesisSink for tests.
func buildSink(
	provider review.SynthesisProvider,
	w *bytes.Buffer,
	inputTTY bool,
	promptYN func(ctx context.Context, question string, def bool) (bool, error),
	perRunPrompt string,
) review.SynthesisSink {
	return review.SynthesisSink{
		Provider:     provider,
		Writer:       w,
		InputTTY:     inputTTY,
		PromptYN:     promptYN,
		PerRunPrompt: perRunPrompt,
	}
}

// makeTwoAgentSummary returns a RunSummary with two agents that each have
// non-empty AssistantText narrative, suitable for triggering synthesis.
func makeTwoAgentSummary() reviewtypes.RunSummary {
	return reviewtypes.RunSummary{
		StartedAt:  time.Now(),
		FinishedAt: time.Now(),
		Cancelled:  false,
		AgentRuns: []reviewtypes.AgentRun{
			{
				Name:   "agent-a",
				Status: reviewtypes.AgentStatusSucceeded,
				Buffer: []reviewtypes.Event{reviewtypes.AssistantText{Text: "Narrative A."}},
			},
			{
				Name:   "agent-b",
				Status: reviewtypes.AgentStatusSucceeded,
				Buffer: []reviewtypes.Event{reviewtypes.AssistantText{Text: "Narrative B."}},
			},
		},
	}
}

// TestSynthesisSink_CompileTimeInterfaceCheck verifies SynthesisSink satisfies
// the Sink interface at compile time (duplicates the var _ check in the impl).
func TestSynthesisSink_CompileTimeInterfaceCheck(t *testing.T) {
	t.Parallel()
	var _ reviewtypes.Sink = review.SynthesisSink{}
}

// TestSynthesisSink_AgentEventIsNoOp verifies AgentEvent produces no output.
func TestSynthesisSink_AgentEventIsNoOp(t *testing.T) {
	t.Parallel()
	w := &bytes.Buffer{}
	stub := &stubSynthesisProvider{response: "verdict"}
	sink := buildSink(stub, w, true, nil, "")

	sink.AgentEvent("agent-a", reviewtypes.AssistantText{Text: "hello"})
	sink.AgentEvent("agent-b", reviewtypes.ToolCall{Name: "Bash", Args: "ls"})

	if w.Len() > 0 {
		t.Errorf("AgentEvent should produce no output, got: %q", w.String())
	}
	if stub.capturedPrompt != "" {
		t.Error("AgentEvent should not call provider")
	}
}

// TestSynthesisSink_SkipsWhenCancelled verifies RunFinished is a no-op when
// summary.Cancelled is true.
func TestSynthesisSink_SkipsWhenCancelled(t *testing.T) {
	t.Parallel()
	w := &bytes.Buffer{}
	stub := &stubSynthesisProvider{response: "verdict"}
	promptCalled := false
	promptFn := func(_ context.Context, _ string, _ bool) (bool, error) {
		promptCalled = true
		return true, nil
	}
	sink := buildSink(stub, w, true, promptFn, "")

	summary := makeTwoAgentSummary()
	summary.Cancelled = true
	sink.RunFinished(summary)

	if promptCalled {
		t.Error("prompt should not be shown when run was cancelled")
	}
	if stub.capturedPrompt != "" {
		t.Error("provider should not be called when run was cancelled")
	}
	if w.Len() > 0 {
		t.Errorf("no output expected for cancelled run, got: %q", w.String())
	}
}

// TestSynthesisSink_SkipsWhenNonTTY verifies RunFinished is a no-op when
// InputTTY is false (CI, piped output).
func TestSynthesisSink_SkipsWhenNonTTY(t *testing.T) {
	t.Parallel()
	w := &bytes.Buffer{}
	stub := &stubSynthesisProvider{response: "verdict"}
	promptCalled := false
	promptFn := func(_ context.Context, _ string, _ bool) (bool, error) {
		promptCalled = true
		return true, nil
	}
	sink := buildSink(stub, w, false, promptFn, "")

	sink.RunFinished(makeTwoAgentSummary())

	if promptCalled {
		t.Error("prompt should not be shown in non-TTY mode")
	}
	if stub.capturedPrompt != "" {
		t.Error("provider should not be called in non-TTY mode")
	}
}

// TestSynthesisSink_SkipsWhenFewerThanTwoUsableAgents verifies that synthesis
// is skipped when fewer than 2 agents produced usable narrative output.
func TestSynthesisSink_SkipsWhenFewerThanTwoUsableAgents(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		summary reviewtypes.RunSummary
	}{
		{
			name: "zero agents",
			summary: reviewtypes.RunSummary{
				StartedAt:  time.Now(),
				FinishedAt: time.Now(),
				AgentRuns:  nil,
			},
		},
		{
			name: "one agent with narrative",
			summary: reviewtypes.RunSummary{
				StartedAt:  time.Now(),
				FinishedAt: time.Now(),
				AgentRuns: []reviewtypes.AgentRun{
					{
						Name:   "agent-a",
						Status: reviewtypes.AgentStatusSucceeded,
						Buffer: []reviewtypes.Event{reviewtypes.AssistantText{Text: "Findings."}},
					},
				},
			},
		},
		{
			name: "two agents but only one has narrative",
			summary: reviewtypes.RunSummary{
				StartedAt:  time.Now(),
				FinishedAt: time.Now(),
				AgentRuns: []reviewtypes.AgentRun{
					{
						Name:   "agent-a",
						Status: reviewtypes.AgentStatusSucceeded,
						Buffer: []reviewtypes.Event{reviewtypes.AssistantText{Text: "Findings."}},
					},
					{
						Name:   "agent-b",
						Status: reviewtypes.AgentStatusFailed,
						Buffer: []reviewtypes.Event{}, // no narrative
					},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := &bytes.Buffer{}
			stub := &stubSynthesisProvider{response: "verdict"}
			promptCalled := false
			promptFn := func(_ context.Context, _ string, _ bool) (bool, error) {
				promptCalled = true
				return true, nil
			}
			sink := buildSink(stub, w, true, promptFn, "")
			sink.RunFinished(tc.summary)

			if promptCalled {
				t.Errorf("[%s] prompt should not be shown with <2 usable agents", tc.name)
			}
			if stub.capturedPrompt != "" {
				t.Errorf("[%s] provider should not be called with <2 usable agents", tc.name)
			}
		})
	}
}

// TestSynthesisSink_UserPicksNo verifies that when the user picks N, the
// provider is not called and nothing is written.
func TestSynthesisSink_UserPicksNo(t *testing.T) {
	t.Parallel()
	w := &bytes.Buffer{}
	stub := &stubSynthesisProvider{response: "verdict"}
	promptFn := func(_ context.Context, _ string, _ bool) (bool, error) {
		return false, nil // user picks N
	}
	sink := buildSink(stub, w, true, promptFn, "")

	sink.RunFinished(makeTwoAgentSummary())

	if stub.capturedPrompt != "" {
		t.Error("provider should not be called when user picks N")
	}
	if w.Len() > 0 {
		t.Errorf("no output expected when user picks N, got: %q", w.String())
	}
}

// TestSynthesisSink_UserPicksYes verifies that when the user picks Y, the
// provider is called and its response is written to the writer.
func TestSynthesisSink_UserPicksYes(t *testing.T) {
	t.Parallel()
	w := &bytes.Buffer{}
	stub := &stubSynthesisProvider{response: "Unified verdict: looks good."}
	promptFn := func(_ context.Context, _ string, _ bool) (bool, error) {
		return true, nil // user picks Y
	}
	sink := buildSink(stub, w, true, promptFn, "")

	sink.RunFinished(makeTwoAgentSummary())

	if stub.capturedPrompt == "" {
		t.Fatal("provider should have been called when user picks Y")
	}
	out := w.String()
	if !strings.Contains(out, "Generating summary...") {
		t.Errorf("writer should show progress before provider response, got: %q", out)
	}
	if !strings.Contains(out, "Unified verdict: looks good.") {
		t.Errorf("writer should contain provider response, got: %q", out)
	}
}

// TestSynthesisSink_ProviderUsesRunContext verifies the provider receives the
// cancellable context supplied by the orchestrator instead of Background.
func TestSynthesisSink_ProviderUsesRunContext(t *testing.T) {
	t.Parallel()
	w := &bytes.Buffer{}
	provider := &contextWaitingSynthesisProvider{}
	promptFn := func(_ context.Context, _ string, _ bool) (bool, error) {
		return true, nil
	}
	runCtx, cancelRun := context.WithCancel(context.Background())
	cancelRun()
	sink := buildSink(provider, w, true, promptFn, "")
	sink.RunContext = runCtx

	sink.RunFinished(makeTwoAgentSummary())

	if provider.capturedPrompt == "" {
		t.Fatal("provider should have been called")
	}
	if !errors.Is(provider.capturedErr, context.Canceled) {
		t.Fatalf("provider context error = %v, want context.Canceled", provider.capturedErr)
	}
}

// TestSynthesisSink_ProviderTimeout verifies the provider call has a deadline
// guard even when no run context is supplied.
func TestSynthesisSink_ProviderTimeout(t *testing.T) {
	t.Parallel()
	w := &bytes.Buffer{}
	provider := &contextWaitingSynthesisProvider{}
	promptFn := func(_ context.Context, _ string, _ bool) (bool, error) {
		return true, nil
	}
	sink := buildSink(provider, w, true, promptFn, "")
	sink.ProviderTimeout = time.Nanosecond

	sink.RunFinished(makeTwoAgentSummary())

	if provider.capturedPrompt == "" {
		t.Fatal("provider should have been called")
	}
	if !errors.Is(provider.capturedErr, context.DeadlineExceeded) {
		t.Fatalf("provider context error = %v, want context.DeadlineExceeded", provider.capturedErr)
	}
}

// TestSynthesisSink_ProviderErrorDegradeGracefully verifies that a provider
// error results in a "synthesis unavailable" message rather than a panic or
// swallowed error.
func TestSynthesisSink_ProviderErrorDegradeGracefully(t *testing.T) {
	t.Parallel()
	w := &bytes.Buffer{}
	stub := &stubSynthesisProvider{
		err: errors.New("API quota exceeded"),
	}
	promptFn := func(_ context.Context, _ string, _ bool) (bool, error) {
		return true, nil // user picks Y
	}
	sink := buildSink(stub, w, true, promptFn, "")

	// Must not panic.
	sink.RunFinished(makeTwoAgentSummary())

	out := w.String()
	if !strings.Contains(out, "synthesis unavailable") {
		t.Errorf("expected 'synthesis unavailable' in output, got: %q", out)
	}
	if !strings.Contains(out, "API quota exceeded") {
		t.Errorf("expected error message in output, got: %q", out)
	}
}

// TestSynthesisSink_PerRunPromptThreaded verifies that the PerRunPrompt field
// is threaded through to the composed prompt sent to the provider.
func TestSynthesisSink_PerRunPromptThreaded(t *testing.T) {
	t.Parallel()
	w := &bytes.Buffer{}
	stub := &stubSynthesisProvider{response: "verdict"}
	promptFn := func(_ context.Context, _ string, _ bool) (bool, error) {
		return true, nil
	}
	perRunPrompt := "Focus specifically on security vulnerabilities."
	sink := buildSink(stub, w, true, promptFn, perRunPrompt)

	sink.RunFinished(makeTwoAgentSummary())

	if !strings.Contains(stub.capturedPrompt, perRunPrompt) {
		t.Errorf("per-run prompt %q not found in provider prompt:\n%s", perRunPrompt, stub.capturedPrompt)
	}
}

// TestSynthesisSink_PromptDefaultIsNo verifies the default value passed to
// the PromptYN function is false (N), so pressing Enter accepts the default N.
func TestSynthesisSink_PromptDefaultIsNo(t *testing.T) {
	t.Parallel()
	w := &bytes.Buffer{}
	stub := &stubSynthesisProvider{response: "verdict"}
	var capturedDefault bool
	promptFn := func(_ context.Context, _ string, def bool) (bool, error) {
		capturedDefault = def
		return false, nil // user picks N
	}
	sink := buildSink(stub, w, true, promptFn, "")

	sink.RunFinished(makeTwoAgentSummary())

	if capturedDefault {
		t.Error("default for synthesis prompt should be false (N), got true")
	}
}
