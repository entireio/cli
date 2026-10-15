package factoryaidroid

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

	// Layout of a real session store: the parent under its project directory,
	// its Worker (and an unrelated one) at the sessions root.
	root := t.TempDir()
	projectDir := filepath.Join(root, "-repo")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	parent := writeDroidSession(t, projectDir, "parent-1", `{"type":"session_start","id":"parent-1"}`,
		`{"model":"m","tokenUsage":{"inputTokens":40,"outputTokens":300,"cacheCreationTokens":2000,"cacheReadTokens":15000,"thinkingTokens":25}}`)
	writeDroidSession(t, root, "worker-1",
		`{"type":"session_start","id":"worker-1","sessionTitle":"worker: read","callingSessionId":"parent-1","callingToolUseId":"call-1"}`,
		`{"model":"m","tokenUsage":{"inputTokens":7,"outputTokens":3,"cacheCreationTokens":0,"cacheReadTokens":0,"thinkingTokens":0}}`)
	writeDroidSession(t, root, "other-1",
		`{"type":"session_start","id":"other-1","callingSessionId":"someone-else","callingToolUseId":"call-2"}`,
		`{"model":"m","tokenUsage":{"inputTokens":99,"outputTokens":99,"cacheCreationTokens":0,"cacheReadTokens":0,"thinkingTokens":0}}`)
	// No tokenUsage in settings.json (exec/BYOK): fall back to transcript usage.
	usageLine := `{"type":"message","id":"e1","message":{"role":"assistant","id":"msg_1","content":[],"usage":{"input_tokens":6,"cache_read_input_tokens":50,"output_tokens":4}}}`
	fallback := writeDroidSession(t, projectDir, "exec-1", `{"type":"session_start","id":"exec-1"}`+"\n"+usageLine+"\n"+usageLine, `{"model":"m"}`)
	quiet := writeDroidSession(t, projectDir, "quiet-1", `{"type":"session_start","id":"quiet-1"}`, `{"model":"m"}`)
	// settings.json is written on a debounce and can lag the transcript: each
	// field takes the larger source, so the total never goes down.
	lagging := writeDroidSession(t, projectDir, "lag-1", `{"type":"session_start","id":"lag-1"}`+"\n"+usageLine,
		`{"model":"m","tokenUsage":{"inputTokens":3,"outputTokens":9,"cacheCreationTokens":0,"cacheReadTokens":20,"thinkingTokens":0}}`)
	droid := &FactoryAIDroidAgent{}

	for _, tt := range []struct {
		id, path     string
		want         *agent.TokenUsage
		wantChildren map[string]*agent.TokenUsage
	}{
		// thinkingTokens is a Droid-side estimate already inside outputTokens.
		{"parent-1", parent, &agent.TokenUsage{InputTokens: 40, OutputTokens: 300, CacheCreationTokens: 2000, CacheReadTokens: 15000},
			map[string]*agent.TokenUsage{"worker-1": {InputTokens: 7, OutputTokens: 3}}},
		{"exec-1", fallback, &agent.TokenUsage{InputTokens: 6, CacheReadTokens: 50, OutputTokens: 4, APICallCount: 1}, nil},
		{"quiet-1", quiet, nil, nil},
		{"lag-1", lagging, &agent.TokenUsage{InputTokens: 6, CacheReadTokens: 50, OutputTokens: 9, APICallCount: 1}, nil},
	} {
		total, children, err := droid.CumulativeTokenUsage(context.Background(), agent.CumulativeTokenInput{SessionID: tt.id, TranscriptPath: tt.path})
		if err != nil {
			t.Fatal(err)
		}
		testutil.AssertCumulative(t, total, children, tt.want, tt.wantChildren)
	}
}

// writeDroidSession writes <id>.jsonl and <id>.settings.json beside it.
func writeDroidSession(t *testing.T, dir, id, transcriptLines, settings string) string {
	t.Helper()
	transcript := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(transcript, []byte(transcriptLines+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	return transcript
}
