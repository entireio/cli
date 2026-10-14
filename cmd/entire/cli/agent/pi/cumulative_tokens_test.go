package pi

import (
	"context"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/testutil"
)

func TestCumulativeTokenUsage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		lines []string
		want  *agent.TokenUsage
	}{
		{"sums assistant messages on every branch",
			[]string{
				`{"type":"session","id":"s1"}`,
				`{"type":"message","id":"m1","message":{"role":"user","content":[{"type":"text","text":"go"}]}}`,
				`{"type":"message","id":"m2","parentId":"m1","message":{"role":"assistant","content":[],"usage":{"input":100,"output":50,"cacheRead":10,"cacheWrite":5}}}`,
				// m3 branches from m1 again, abandoning m2; both calls were spent.
				`{"type":"message","id":"m3","parentId":"m1","message":{"role":"assistant","content":[],"usage":{"input":30,"output":20,"cacheRead":110,"cacheWrite":0}}}`,
			},
			&agent.TokenUsage{InputTokens: 130, OutputTokens: 70, CacheReadTokens: 120, CacheCreationTokens: 5, APICallCount: 2}},
		{"no usage is nil", []string{`{"type":"message","id":"m1","message":{"role":"user","content":[]}}`}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data := []byte(strings.Join(tt.lines, "\n") + "\n")
			total, children, err := (&PiAgent{}).CumulativeTokenUsage(context.Background(), agent.CumulativeTokenInput{TranscriptData: data})
			if err != nil {
				t.Fatal(err)
			}
			testutil.AssertCumulative(t, total, children, tt.want, nil)
		})
	}
}
