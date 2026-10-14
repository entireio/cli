package opencode

import (
	"context"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/testutil"
)

func TestCumulativeTokenUsage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		export string
		want   *agent.TokenUsage
	}{
		{"reasoning folds into output; in-flight zero message counts as a call",
			`{"info":{"id":"ses_1"},"messages":[
				{"info":{"id":"msg_1","role":"user"},"parts":[]},
				{"info":{"id":"msg_2","role":"assistant","tokens":{"input":12,"output":80,"reasoning":40,"cache":{"read":9000,"write":300}}},"parts":[]},
				{"info":{"id":"msg_3","role":"assistant","tokens":{"input":0,"output":0,"reasoning":0,"cache":{"read":0,"write":0}}},"parts":[]}]}`,
			&agent.TokenUsage{InputTokens: 12, OutputTokens: 120, CacheReadTokens: 9000, CacheCreationTokens: 300, APICallCount: 2}},
		{"no assistant tokens is nil", `{"info":{"id":"ses_1"},"messages":[{"info":{"id":"msg_1","role":"user"},"parts":[]}]}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			total, children, err := (&OpenCodeAgent{}).CumulativeTokenUsage(context.Background(), agent.CumulativeTokenInput{TranscriptData: []byte(tt.export)})
			if err != nil {
				t.Fatal(err)
			}
			testutil.AssertCumulative(t, total, children, tt.want, nil)
		})
	}
}
