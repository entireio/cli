package review

import (
	"context"
	"io"
	"time"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// ExposedComposeSynthesisPrompt exposes composeSynthesisPrompt for
// package-external tests (synthesis_prompt_test.go, synthesis_sink_test.go).
// Only compiled during `go test`.
func ExposedComposeSynthesisPrompt(summary reviewtypes.RunSummary, perRunPrompt string) string {
	return composeSynthesisPrompt(summary, perRunPrompt, "", "")
}

// SinkComposeInputs is the test-facing alias for multiAgentSinkInputs.
// It lets external tests drive composeMultiAgentSinks with explicit isTTY
// and canPrompt values without depending on real TTY detection.
type SinkComposeInputs struct {
	Out               io.Writer
	IsTTY             bool
	AgentNames        []string
	CancelRun         context.CancelFunc
	SynthesisProvider SynthesisProvider
	PerRunPrompt      string
	MasterName        string
	JudgeTimeout      time.Duration
	OnSynthesisError  func(error)
}

type SingleAgentSinkComposeInputs struct {
	Out       io.Writer
	IsTTY     bool
	CanPrompt bool
	AgentName string
	CancelRun context.CancelFunc
}

// ExposedComposeMultiAgentSinks exposes composeMultiAgentSinks for tests.
func ExposedComposeMultiAgentSinks(in SinkComposeInputs) []reviewtypes.Sink {
	return composeMultiAgentSinks(multiAgentSinkInputs{
		out:               in.Out,
		isTTY:             in.IsTTY,
		agentNames:        in.AgentNames,
		cancelRun:         in.CancelRun,
		synthesisProvider: in.SynthesisProvider,
		perRunPrompt:      in.PerRunPrompt,
		masterName:        in.MasterName,
		judgeTimeout:      in.JudgeTimeout,
		onSynthesisError:  in.OnSynthesisError,
	})
}

// ExposedComposeSingleAgentSinks exposes composeSingleAgentSinks for tests.
func ExposedComposeSingleAgentSinks(in SingleAgentSinkComposeInputs) []reviewtypes.Sink {
	return composeSingleAgentSinks(singleAgentSinkInputs{
		out:       in.Out,
		isTTY:     in.IsTTY,
		canPrompt: in.CanPrompt,
		agentName: in.AgentName,
		cancelRun: in.CancelRun,
	})
}

// ExposedFindTUISink exposes findTUISink for tests.
func ExposedFindTUISink(sinks []reviewtypes.Sink) (*TUISink, bool) {
	return findTUISink(sinks)
}

// ExposedIsTUIPostRunCompleteSink reports whether s is the TUI finalizer sink.
func ExposedIsTUIPostRunCompleteSink(s reviewtypes.Sink) bool {
	_, ok := s.(tuiPostRunCompleteSink)
	return ok
}
