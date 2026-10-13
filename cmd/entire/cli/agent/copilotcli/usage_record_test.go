package copilotcli

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// usageRecordSessionFixture is a three-prompt Copilot CLI 1.0.94 session
// (prompt and response text scrubbed). Its session.usage_record events are
// the per-call ground truth; the closing session.shutdown aggregates them.
const usageRecordSessionFixture = "testdata/usage_record_session.jsonl"

func readUsageRecordFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(usageRecordSessionFixture)
	require.NoError(t, err)
	return data
}

// promptOffsets returns the transcript position (line count) at each
// userPromptSubmitted hook — where Entire starts a turn's token window.
func promptOffsets(t *testing.T, data []byte) []int {
	t.Helper()
	var offsets []int
	for i, line := range bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n")) {
		if bytes.Contains(line, []byte(`"hookType":"userPromptSubmitted"`)) {
			offsets = append(offsets, i)
		}
	}
	require.Len(t, offsets, 3)
	return offsets
}

// Ground truth, from summing the fixture's usage records per prompt with
// fresh input = inputTokens - cacheReadTokens - cacheWriteTokens.
var usageRecordFixtureTurns = []agent.TokenUsage{
	{InputTokens: 16, CacheReadTokens: 20185, CacheCreationTokens: 20681, OutputTokens: 415, APICallCount: 2},
	{InputTokens: 10, CacheReadTokens: 20185, CacheCreationTokens: 309, OutputTokens: 66, APICallCount: 1},
	{InputTokens: 16, CacheReadTokens: 41038, CacheCreationTokens: 337, OutputTokens: 206, APICallCount: 2},
}

var usageRecordFixtureTotal = agent.TokenUsage{
	InputTokens: 42, CacheReadTokens: 81408, CacheCreationTokens: 21327, OutputTokens: 687, APICallCount: 5,
}

// sliceTranscript returns the first n lines of data, newline-terminated.
func sliceTranscript(data []byte, n int) []byte {
	lines := bytes.SplitAfter(data, []byte("\n"))
	return bytes.Join(lines[:n], nil)
}

func TestCalculateTokenUsage_UsageRecordSlicesAreScopedAndSumToShutdown(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}
	data := readUsageRecordFixture(t)
	offsets := promptOffsets(t, data)

	var sum *agent.TokenUsage
	for i, from := range offsets {
		end := len(bytes.SplitAfter(bytes.TrimRight(data, "\n"), []byte("\n")))
		if i+1 < len(offsets) {
			end = offsets[i+1]
		}
		usage, err := ag.CalculateTokenUsage(sliceTranscript(data, end), from)
		require.NoError(t, err)
		require.Equal(t, usageRecordFixtureTurns[i], *usage, "turn %d", i+1)
		sum = types.AddTokenUsage(sum, usage)
	}
	require.Equal(t, usageRecordFixtureTotal, *sum)

	// The full read prefers the shutdown aggregate; normalized, it must equal
	// the sum of the per-call slices.
	full, err := ag.CalculateTokenUsage(data, 0)
	require.NoError(t, err)
	require.Equal(t, usageRecordFixtureTotal, *full)
}

func TestCalculateTokenUsage_UsageRecordFullReadBeforeShutdown(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}
	data := readUsageRecordFixture(t)

	// Hooks run before Copilot writes session.shutdown, so a full read at hook
	// time sees only the usage records.
	trimmed := strings.TrimRight(string(data), "\n")
	withoutShutdown := trimmed[:strings.LastIndex(trimmed, "\n")+1]
	require.NotContains(t, withoutShutdown, eventTypeSessionShutdown)

	usage, err := ag.CalculateTokenUsage([]byte(withoutShutdown), 0)
	require.NoError(t, err)
	require.Equal(t, usageRecordFixtureTotal, *usage)
}

func TestCalculateTokenUsage_SlicedReadIgnoresShutdownWithUsageRecords(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}
	data := readUsageRecordFixture(t)
	offsets := promptOffsets(t, data)

	// The last checkpoint's slice runs to the end of the file and so contains
	// the session-wide shutdown; it must still count only its own calls.
	usage, err := ag.CalculateTokenUsage(data, offsets[2])
	require.NoError(t, err)
	require.Equal(t, usageRecordFixtureTurns[2], *usage)
}

func TestCalculateTokenUsage_NoDoubleCountAcrossSources(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}

	lines := []string{
		// Older run: assistant.message carries outputTokens, no usage record.
		`{"type":"assistant.message","data":{"apiCallId":"old-1","outputTokens":40},"id":"1"}`,
		// A usage record and an assistant.message for the same call: count once.
		`{"type":"session.usage_record","data":{"usage":{"apiCallId":"new-1","inputTokens":1000,"outputTokens":30,"cacheReadTokens":600,"cacheWriteTokens":300}},"id":"2"}`,
		`{"type":"assistant.message","data":{"apiCallId":"new-1","outputTokens":30},"id":"3"}`,
		// No call ID to match on while records exist: the record is authoritative.
		`{"type":"assistant.message","data":{"outputTokens":99},"id":"4"}`,
	}
	content := []byte(strings.Join(lines, "\n") + "\n")

	for _, from := range []int{0, 1} {
		usage, err := ag.CalculateTokenUsage(content, from)
		require.NoError(t, err)
		want := agent.TokenUsage{InputTokens: 100, CacheReadTokens: 600, CacheCreationTokens: 300, OutputTokens: 30, APICallCount: 1}
		if from == 0 {
			want.OutputTokens += 40
			want.APICallCount++
		}
		require.Equal(t, want, *usage, "fromOffset %d", from)
	}
}

func TestCalculateTokenUsage_ResumedSessionSumsEachRunOnce(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}

	// A resumed session appends one shutdown per process run, each covering
	// only its own run. The live run after the last shutdown has no aggregate
	// yet and is counted per call.
	lines := []string{
		`{"type":"assistant.message","data":{"outputTokens":10},"id":"1"}`,
		`{"type":"session.shutdown","data":{"modelMetrics":{"m":{"requests":{"count":1},"usage":{"inputTokens":500,"outputTokens":10,"cacheReadTokens":0,"cacheWriteTokens":400}}}},"id":"2"}`,
		`{"type":"session.shutdown","data":{"modelMetrics":{}},"id":"3"}`,
		`{"type":"session.usage_record","data":{"usage":{"apiCallId":"a","inputTokens":700,"outputTokens":20,"cacheReadTokens":400,"cacheWriteTokens":200}},"id":"4"}`,
		`{"type":"session.shutdown","data":{"modelMetrics":{"m":{"requests":{"count":1},"usage":{"inputTokens":700,"outputTokens":20,"cacheReadTokens":400,"cacheWriteTokens":200}}}},"id":"5"}`,
		`{"type":"session.usage_record","data":{"usage":{"apiCallId":"b","inputTokens":900,"outputTokens":5,"cacheReadTokens":600,"cacheWriteTokens":250}},"id":"6"}`,
	}
	content := []byte(strings.Join(lines, "\n") + "\n")

	usage, err := ag.CalculateTokenUsage(content, 0)
	require.NoError(t, err)
	require.Equal(t, agent.TokenUsage{
		InputTokens:         100 + 100 + 50,
		CacheReadTokens:     0 + 400 + 600,
		CacheCreationTokens: 400 + 200 + 250,
		OutputTokens:        10 + 20 + 5,
		APICallCount:        3,
	}, *usage)
}

func TestCalculateTokenUsage_UsageRecordClampsInconsistentInput(t *testing.T) {
	t.Parallel()
	ag := &CopilotCLIAgent{}

	content := []byte(`{"type":"session.usage_record","data":{"usage":{"inputTokens":10,"outputTokens":3,"cacheReadTokens":20,"cacheWriteTokens":5}},"id":"1"}` + "\n")
	usage, err := ag.CalculateTokenUsage(content, 0)
	require.NoError(t, err)
	require.Equal(t, agent.TokenUsage{CacheReadTokens: 20, CacheCreationTokens: 5, OutputTokens: 3, APICallCount: 1}, *usage)
}
