package review

import (
	"context"
	"io"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// ExposedComposeSynthesisPrompt exposes composeSynthesisPrompt for
// package-external tests (synthesis_prompt_test.go, synthesis_sink_test.go).
// Only compiled during `go test`.
var ExposedComposeSynthesisPrompt = composeSynthesisPrompt

// ExposedRunConfigWithReviewConfig exposes runConfigWithReviewConfig so
// external tests can assert the settings.ReviewConfig → RunConfig mapping
// (skills/prompt branch plus the Model/ReasoningEffort per-spawn knobs).
var ExposedRunConfigWithReviewConfig = runConfigWithReviewConfig

// SinkComposeInputs is the test-facing alias for multiAgentSinkInputs.
// It lets external tests drive composeMultiAgentSinks with explicit isTTY
// and canPrompt values without depending on real TTY detection.
type SinkComposeInputs struct {
	Out               io.Writer
	IsTTY             bool
	CanPrompt         bool
	AgentNames        []string
	CancelRun         context.CancelFunc
	SynthesisProvider SynthesisProvider
	PromptYN          func(ctx context.Context, question string, def bool) (bool, error)
	PerRunPrompt      string
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
		canPrompt:         in.CanPrompt,
		agentNames:        in.AgentNames,
		cancelRun:         in.CancelRun,
		synthesisProvider: in.SynthesisProvider,
		promptYN:          in.PromptYN,
		perRunPrompt:      in.PerRunPrompt,
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
