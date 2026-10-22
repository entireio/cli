package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// resumeTranscript writes n assistant messages, each split over three lines
// with a tool result between them, as Claude Code writes parallel tool calls.
// A message's later lines carry its final output count.
func resumeTranscript(n int) []byte {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, `{"type":"user","uuid":"u%d","message":{"role":"user","content":"go %d"}}`+"\n", i, i)
		for part := range 3 {
			fmt.Fprintf(&b, `{"type":"assistant","uuid":"a%d-%d","message":{"id":"msg_%d","content":[{"type":"text","text":"x"}],"usage":{"input_tokens":%d,"cache_read_input_tokens":%d,"output_tokens":%d}}}`+"\n",
				i, part, i, 10+i, i, 1+part)
			fmt.Fprintf(&b, `{"type":"user","uuid":"t%d-%d","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_%d_%d","content":"ok"}]}}`+"\n", i, part, i, part)
		}
	}
	return []byte(b.String())
}

func fullTotal(t *testing.T, c *ClaudeCodeAgent, data []byte) *agent.TokenUsage {
	t.Helper()
	total, _, err := c.CumulativeTokenUsage(context.Background(), agent.CumulativeTokenInput{TranscriptData: data[:agent.CompleteLinesEnd(data, 0)]})
	if err != nil {
		t.Fatal(err)
	}
	return total
}

func sameUsage(a, b *agent.TokenUsage) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func TestResumeCumulativeTokenUsage_MatchesTheFullRead(t *testing.T) {
	t.Parallel()
	c := &ClaudeCodeAgent{}
	ctx := context.Background()
	data := resumeTranscript(70) // more messages than the open window

	for _, step := range []int{29, 61, 997} {
		var cursor json.RawMessage
		for end := 0; end <= len(data); end += step {
			obs, err := c.ResumeCumulativeTokenUsage(ctx, agent.CumulativeTokenInput{TranscriptData: data[:end]}, cursor)
			if err != nil {
				t.Fatal(err)
			}
			if obs.Restarted {
				t.Fatalf("step %d end %d: growth read as a rewrite", step, end)
			}
			if want := fullTotal(t, c, data[:end]); !sameUsage(obs.Total, want) {
				t.Fatalf("step %d end %d: resumed %+v, full %+v", step, end, obs.Total, want)
			}
			cursor = obs.Cursor
		}
	}
}

func TestResumeCumulativeTokenUsage_RewriteRestarts(t *testing.T) {
	t.Parallel()
	c := &ClaudeCodeAgent{}
	ctx := context.Background()
	data := resumeTranscript(10)
	obs, err := c.ResumeCumulativeTokenUsage(ctx, agent.CumulativeTokenInput{TranscriptData: data}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// An older copy, then one more message appended to it.
	rewritten := append(append([]byte(nil), data[:agent.CompleteLinesEnd(data[:len(data)/2], 0)]...), resumeTranscript(11)[len(resumeTranscript(10)):]...)
	again, err := c.ResumeCumulativeTokenUsage(ctx, agent.CumulativeTokenInput{TranscriptData: rewritten}, obs.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Restarted {
		t.Fatal("a rewritten transcript must report a restart")
	}
	if want := fullTotal(t, c, rewritten); !sameUsage(again.Total, want) {
		t.Fatalf("after rewrite %+v, full %+v", again.Total, want)
	}

	// An empty transcript, and a cursor that consumed nothing, are not rewrites.
	empty, err := c.ResumeCumulativeTokenUsage(ctx, agent.CumulativeTokenInput{TranscriptData: nil}, nil)
	if err != nil || empty.Total != nil || empty.Restarted {
		t.Fatalf("empty: %+v %v", empty, err)
	}
	grown, err := c.ResumeCumulativeTokenUsage(ctx, agent.CumulativeTokenInput{TranscriptData: data}, empty.Cursor)
	if err != nil || grown.Restarted {
		t.Fatalf("growth after an empty read: %+v %v", grown, err)
	}

	// Another agent's cursor is ignored, not trusted.
	foreign := json.RawMessage(`{"v":1,"agent":"codex","offset":40,"check":"x"}`)
	other, err := c.ResumeCumulativeTokenUsage(ctx, agent.CumulativeTokenInput{TranscriptData: data}, foreign)
	if err != nil || other.Restarted || !sameUsage(other.Total, obs.Total) {
		t.Fatalf("foreign cursor: %+v %v", other, err)
	}
}
