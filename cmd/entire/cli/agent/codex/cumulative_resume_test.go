package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// resumeRollout writes n calls, each a token_usage_record before the call's
// running total, with a replayed record and a restarted counter part-way.
func resumeRollout(n int) []byte {
	var b strings.Builder
	in, out := 0, 0
	for i := range n {
		if i == n/2 {
			in, out = 0, 0 // a resumed process restarts its counter
		}
		fmt.Fprintf(&b, `{"type":"token_usage_record","payload":{"response_id":"resp_%d","usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d}}}`+"\n", i, 20+i, i, 2+i)
		if i%7 == 3 {
			fmt.Fprintf(&b, `{"type":"token_usage_record","payload":{"response_id":"resp_%d","usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d}}}`+"\n", i-1, 21+i, i, 2+i)
		}
		in += 20 + i
		out += 2 + i
		fmt.Fprintf(&b, `{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"cached_input_tokens":0,"output_tokens":%d}}}}`+"\n", in, out)
		b.WriteString(`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[]}}` + "\n")
	}
	return []byte(b.String())
}

func sameUsage(a, b *agent.TokenUsage) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func TestResumeCumulativeTokenUsage_MatchesTheFullRead(t *testing.T) {
	t.Parallel()
	c := &CodexAgent{}
	ctx := context.Background()
	data := resumeRollout(30)

	for _, step := range []int{1, 13, 97, 1501} {
		var cursor json.RawMessage
		for end := 0; end <= len(data); end += step {
			obs, err := c.ResumeCumulativeTokenUsage(ctx, agent.CumulativeTokenInput{TranscriptData: data[:end]}, cursor)
			if err != nil {
				t.Fatal(err)
			}
			if obs.Restarted {
				t.Fatalf("step %d end %d: growth read as a rewrite", step, end)
			}
			if want := cumulativeRolloutUsage(data[:agent.CompleteLinesEnd(data[:end], 0)]); !sameUsage(obs.Total, want) {
				t.Fatalf("step %d end %d: resumed %+v, full %+v", step, end, obs.Total, want)
			}
			cursor = obs.Cursor
		}
	}
}

func TestResumeCumulativeTokenUsage_RewriteRestarts(t *testing.T) {
	t.Parallel()
	c := &CodexAgent{}
	ctx := context.Background()
	data := resumeRollout(12)
	obs, err := c.ResumeCumulativeTokenUsage(ctx, agent.CumulativeTokenInput{TranscriptData: data}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rewritten := data[:agent.CompleteLinesEnd(data[:len(data)/3], 0)]
	again, err := c.ResumeCumulativeTokenUsage(ctx, agent.CumulativeTokenInput{TranscriptData: rewritten}, obs.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Restarted || !sameUsage(again.Total, cumulativeRolloutUsage(rewritten)) {
		t.Fatalf("rewrite: %+v", again)
	}
}
