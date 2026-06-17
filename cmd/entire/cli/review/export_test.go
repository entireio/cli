package review

import (
	"context"
	"io"

	"charm.land/huh/v2"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// ExposedComposeSynthesisPrompt exposes composeSynthesisPrompt for
// package-external tests (synthesis_prompt_test.go, synthesis_sink_test.go).
// Only compiled during `go test`.
var ExposedComposeSynthesisPrompt = composeSynthesisPrompt

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

func ExposedBuildAgentMultiSelect(options []huh.Option[string], picked *[]string) *huh.MultiSelect[string] {
	return buildAgentMultiSelect(options, picked)
}

// ExposedFindTUISink exposes findTUISink for tests.
func ExposedFindTUISink(sinks []reviewtypes.Sink) (*TUISink, bool) {
	return findTUISink(sinks)
}
