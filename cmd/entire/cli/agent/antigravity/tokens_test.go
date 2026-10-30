package antigravity

import (
	"os"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
)

// testdata/transcript_tokens.jsonl is a real agy 1.4.2 conversation (content
// trimmed, token fields as written): two user turns of three model calls each.
// The first turn's six steps are lines 1-6.

func readTokensFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/transcript_tokens.jsonl")
	require.NoError(t, err)
	return data
}

func TestCalculateTokenUsage_SumsPlannerResponseSteps(t *testing.T) {
	t.Parallel()
	usage, err := (&AntigravityAgent{}).CalculateTokenUsage(readTokensFixture(t), 0)
	require.NoError(t, err)
	require.Equal(t, &agent.TokenUsage{InputTokens: 45051, OutputTokens: 564, APICallCount: 6}, usage)
}

func TestCalculateTokenUsage_CountsOnlyStepsAfterTheOffset(t *testing.T) {
	t.Parallel()
	a := &AntigravityAgent{}
	data := readTokensFixture(t)

	second, err := a.CalculateTokenUsage(data, 6)
	require.NoError(t, err)
	require.Equal(t, &agent.TokenUsage{InputTokens: 23643, OutputTokens: 291, APICallCount: 3}, second)

	end, err := a.GetTranscriptPosition("testdata/transcript_tokens.jsonl")
	require.NoError(t, err)
	none, err := a.CalculateTokenUsage(data, end)
	require.NoError(t, err)
	require.Nil(t, none, "no new steps is no data, not a zero count")
}

// The offset counts non-blank lines, the same metric GetTranscriptPosition
// records at turn start, so blank lines must not shift the range.
func TestCalculateTokenUsage_OffsetSkipsBlankLines(t *testing.T) {
	t.Parallel()
	data := []byte(`{"step_index":0,"type":"USER_INPUT","content":"one"}

{"step_index":1,"type":"PLANNER_RESPONSE","input_tokens":100,"output_tokens":10,"cache_read_tokens":0}

{"step_index":2,"type":"USER_INPUT","content":"two"}
{"step_index":3,"type":"PLANNER_RESPONSE","input_tokens":200,"output_tokens":20,"cache_read_tokens":5}
`)
	usage, err := (&AntigravityAgent{}).CalculateTokenUsage(data, 2)
	require.NoError(t, err)
	require.Equal(t, &agent.TokenUsage{InputTokens: 200, OutputTokens: 20, CacheReadTokens: 5, APICallCount: 1}, usage)
}

// Older agy writes no token fields. Nil, not zero, is what lets the
// lifecycle fall back to the statusline source.
func TestCalculateTokenUsage_NoTokenFieldsIsNil(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/transcript_sample.jsonl")
	require.NoError(t, err)
	usage, err := (&AntigravityAgent{}).CalculateTokenUsage(data, 0)
	require.NoError(t, err)
	require.Nil(t, usage)
}

func TestCalculateTokenUsage_SkipsMalformedAndRepeatedSteps(t *testing.T) {
	t.Parallel()
	data := []byte(`{"step_index":1,"type":"PLANNER_RESPONSE","input_tokens":100,"output_tokens":10}
not json
{"step_index":1,"type":"PLANNER_RESPONSE","input_tokens":100,"output_tokens":10}
{"step_index":2,"type":"GENERIC","input_tokens":999}
{"step_index":3,"type":"PLANNER_RESPONSE","input_tokens":300,"output_tokens":30}
`)
	usage, err := (&AntigravityAgent{}).CalculateTokenUsage(data, 0)
	require.NoError(t, err)
	require.Equal(t, &agent.TokenUsage{InputTokens: 400, OutputTokens: 40, APICallCount: 2}, usage)
}

// A repeated step is recognized by its step_index; steps that carry none are
// distinct calls, not repeats of step 0, while a real step 0 written twice
// still counts once.
func TestCalculateTokenUsage_StepsWithoutAnIndexAreNotRepeats(t *testing.T) {
	t.Parallel()
	data := []byte(`{"type":"PLANNER_RESPONSE","input_tokens":100,"output_tokens":10}
{"type":"PLANNER_RESPONSE","input_tokens":200,"output_tokens":20}
{"step_index":0,"type":"PLANNER_RESPONSE","input_tokens":5,"output_tokens":1}
{"step_index":0,"type":"PLANNER_RESPONSE","input_tokens":5,"output_tokens":1}
`)
	usage, err := (&AntigravityAgent{}).CalculateTokenUsage(data, 0)
	require.NoError(t, err)
	require.Equal(t, &agent.TokenUsage{InputTokens: 305, OutputTokens: 31, APICallCount: 3}, usage)
}

// The lifecycle reaches the calculator through agent.CalculateTokenUsage; this
// pins that agy is not routed past it (e.g. by a capability declaration).
func TestCalculateTokenUsage_ReachedThroughTheFramework(t *testing.T) {
	t.Parallel()
	usage := agent.CalculateTokenUsage(t.Context(), &AntigravityAgent{}, readTokensFixture(t), 6, "")
	require.Equal(t, &agent.TokenUsage{InputTokens: 23643, OutputTokens: 291, APICallCount: 3}, usage)
}
