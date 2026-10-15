package copilotcli

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/testutil"
)

func TestCumulativeTokenUsage(t *testing.T) {
	t.Parallel()

	// inputTokens is cache-inclusive: 19873 = 10 fresh + 13528 read + 6335 written.
	record := func(agentID, callID string, in, out, read, write int) string {
		return fmt.Sprintf(`{"type":"session.usage_record",%q:%q,"data":{"usage":{"model":"m","inputTokens":%d,"outputTokens":%d,"cacheReadTokens":%d,"cacheWriteTokens":%d,"reasoningTokens":0,"apiCallId":%q}}}`,
			"agentId", agentID, in, out, read, write, callID)
	}
	shutdown := func(in, out, read, write, calls int) string {
		return fmt.Sprintf(`{"type":"session.shutdown","data":{"modelMetrics":{"m":{"requests":{"count":%d},"usage":{"inputTokens":%d,"outputTokens":%d,"cacheReadTokens":%d,"cacheWriteTokens":%d}}}}}`,
			calls, in, out, read, write)
	}
	tests := []struct {
		name     string
		lines    []string
		want     *agent.TokenUsage
		children map[string]*agent.TokenUsage
	}{
		{"usage records split cache-inclusive input, dedupe apiCallId, ignore shutdown",
			[]string{
				record("", "msg_1", 19873, 380, 13528, 6335), record("", "msg_1", 19873, 380, 13528, 6335),
				record("", "msg_2", 20348, 19, 19863, 479), shutdown(40221, 399, 33391, 6814, 2),
				record("sub-1", "msg_3", 500, 20, 400, 0),
			},
			&agent.TokenUsage{InputTokens: 16, CacheReadTokens: 33391, CacheCreationTokens: 6814, OutputTokens: 399, APICallCount: 2},
			map[string]*agent.TokenUsage{"sub-1": {InputTokens: 100, CacheReadTokens: 400, OutputTokens: 20, APICallCount: 1}}},
		{"usage from before records began is kept",
			[]string{shutdown(1000, 50, 800, 100, 2), record("", "msg_3", 500, 20, 400, 0), shutdown(1500, 70, 1200, 100, 3)},
			&agent.TokenUsage{InputTokens: 200, CacheReadTokens: 1200, CacheCreationTokens: 100, OutputTokens: 70, APICallCount: 3}, nil},
		{"shutdown fallback sums per process segment",
			[]string{shutdown(1000, 50, 800, 100, 2), shutdown(3000, 90, 2500, 300, 4), shutdown(600, 10, 500, 0, 1)},
			&agent.TokenUsage{InputTokens: 300, CacheReadTokens: 3000, CacheCreationTokens: 300, OutputTokens: 100, APICallCount: 5}, nil},
		{"no usage is nil", []string{`{"type":"user.message","data":{"content":"hi"}}`}, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data := []byte(strings.Join(tt.lines, "\n") + "\n")
			total, children, err := (&CopilotCLIAgent{}).CumulativeTokenUsage(context.Background(), agent.CumulativeTokenInput{TranscriptData: data})
			if err != nil {
				t.Fatal(err)
			}
			testutil.AssertCumulative(t, total, children, tt.want, tt.children)
		})
	}
}
