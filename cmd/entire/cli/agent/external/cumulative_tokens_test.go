package external

import (
	"context"
	"runtime"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/testutil"
)

func TestWrappedCumulativeTokenUsage(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == osWindows {
		t.Skip("mock agent is a POSIX shell script")
	}

	// The mock answers calculate-tokens only for offset 0, proving the whole
	// transcript is counted.
	script := `#!/bin/sh
case "$1" in
  info) echo '{"protocol_version":1,"name":"tok","type":"Tok","description":"d","protected_dirs":[],"hook_names":[],"capabilities":{"token_calculator":true}}' ;;
  calculate-tokens)
    [ "$3" = "0" ] || exit 1
    echo '{"input_tokens":10,"cache_creation_tokens":1,"cache_read_tokens":200,"output_tokens":5,"api_call_count":2,"subagent_tokens":{"input_tokens":4,"cache_creation_tokens":0,"cache_read_tokens":0,"output_tokens":2,"api_call_count":1}}' ;;
  *) exit 1 ;;
esac
`

	wrapped, err := Wrap(newExternalAgent(t, testBinaryDir(t, script)))
	if err != nil {
		t.Fatal(err)
	}
	source, ok := agent.AsCumulativeTokenSource(wrapped)
	if !ok {
		t.Fatal("token_calculator=true should expose CumulativeTokenSource")
	}
	total, children, err := source.CumulativeTokenUsage(context.Background(), agent.CumulativeTokenInput{TranscriptData: []byte("{}\n")})
	if err != nil {
		t.Fatal(err)
	}
	testutil.AssertCumulative(t, total, children, &agent.TokenUsage{InputTokens: 10, CacheCreationTokens: 1, CacheReadTokens: 200, OutputTokens: 5, APICallCount: 2}, map[string]*agent.TokenUsage{
		externalSubagentsKey: {InputTokens: 4, OutputTokens: 2, APICallCount: 1},
	})
}
