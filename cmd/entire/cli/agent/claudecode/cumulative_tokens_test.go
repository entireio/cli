package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/testutil"
)

func TestCumulativeTokenUsage(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// m1 is one streamed message split across a thinking and a tool_use line,
	// each repeating the final usage; m2 is a second call. The Task call
	// launches subagent sub1, whose transcript repeats its one message too and
	// launches sub2 of its own.
	data := buildJSONL(
		`{"type":"user","uuid":"u1","message":{"content":"go"}}`,
		`{"type":"assistant","uuid":"a1","message":{"id":"m1","content":[{"type":"thinking"}],"usage":{"input_tokens":3,"cache_creation_input_tokens":500,"cache_read_input_tokens":9000,"output_tokens":120}}}`,
		`{"type":"assistant","uuid":"a2","message":{"id":"m1","content":[{"type":"tool_use"}],"usage":{"input_tokens":3,"cache_creation_input_tokens":500,"cache_read_input_tokens":9000,"output_tokens":120}}}`,
		makeTaskToolUseLine(t, "a3", "toolu_1"),
		makeTaskResultLine(t, "u2", "toolu_1", "sub1"),
		`{"type":"assistant","uuid":"a4","message":{"id":"m2","usage":{"input_tokens":2,"cache_read_input_tokens":9500,"output_tokens":40}}}`,
	)
	subagentsDir := filepath.Join(dir, "sess-1", "subagents")
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONLFile(t, filepath.Join(subagentsDir, "agent-sub1.jsonl"),
		`{"type":"assistant","uuid":"s1","message":{"id":"sm1","usage":{"input_tokens":7,"output_tokens":3}}}`,
		`{"type":"assistant","uuid":"s2","message":{"id":"sm1","usage":{"input_tokens":7,"output_tokens":3}}}`,
		makeTaskToolUseLine(t, "s3", "toolu_2"),
		makeTaskResultLine(t, "s4", "toolu_2", "sub2"),
	)
	writeJSONLFile(t, filepath.Join(subagentsDir, "agent-sub2.jsonl"),
		`{"type":"assistant","uuid":"n1","message":{"id":"nm1","usage":{"input_tokens":11,"output_tokens":5}}}`,
	)
	c := &ClaudeCodeAgent{}

	total, children, err := c.CumulativeTokenUsage(context.Background(), agent.CumulativeTokenInput{
		SessionID: "sess-1", TranscriptPath: filepath.Join(dir, "sess-1.jsonl"), TranscriptData: data,
	})
	if err != nil {
		t.Fatal(err)
	}
	testutil.AssertCumulative(t, total, children,
		&agent.TokenUsage{InputTokens: 5, CacheCreationTokens: 500, CacheReadTokens: 18500, OutputTokens: 160, APICallCount: 2},
		map[string]*agent.TokenUsage{
			"sub1": {InputTokens: 7, OutputTokens: 3, APICallCount: 1},
			"sub2": {InputTokens: 11, OutputTokens: 5, APICallCount: 1},
		})

	total, children, err = c.CumulativeTokenUsage(context.Background(), agent.CumulativeTokenInput{
		TranscriptData: buildJSONL(`{"type":"user","uuid":"u1","message":{"content":"hi"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	testutil.AssertCumulative(t, total, children, nil, nil)

	// A synthetic API-error entry has an ID but no usage: no call, no data.
	total, children, err = c.CumulativeTokenUsage(context.Background(), agent.CumulativeTokenInput{
		TranscriptData: buildJSONL(`{"type":"assistant","uuid":"e1","isApiErrorMessage":true,"message":{"id":"err1","content":[{"type":"text","text":"API Error"}]}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	testutil.AssertCumulative(t, total, children, nil, nil)
}
