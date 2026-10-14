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
	noUsage := writeDroidSession(t, projectDir, "quiet-1", `{"type":"session_start","id":"quiet-1"}`, `{"model":"m"}`)
	droid := &FactoryAIDroidAgent{}

	total, children, err := droid.CumulativeTokenUsage(context.Background(), agent.CumulativeTokenInput{SessionID: "parent-1", TranscriptPath: parent})
	if err != nil {
		t.Fatal(err)
	}
	testutil.AssertCumulative(t, total, children,
		&agent.TokenUsage{InputTokens: 40, OutputTokens: 325, CacheCreationTokens: 2000, CacheReadTokens: 15000},
		map[string]*agent.TokenUsage{"worker-1": {InputTokens: 7, OutputTokens: 3}})

	total, children, err = droid.CumulativeTokenUsage(context.Background(), agent.CumulativeTokenInput{SessionID: "quiet-1", TranscriptPath: noUsage})
	if err != nil {
		t.Fatal(err)
	}
	testutil.AssertCumulative(t, total, children, nil, nil)
}

// writeDroidSession writes <id>.jsonl holding one session_start line, and
// <id>.settings.json beside it.
func writeDroidSession(t *testing.T, dir, id, sessionStart, settings string) string {
	t.Helper()
	transcript := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(transcript, []byte(sessionStart+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	return transcript
}
