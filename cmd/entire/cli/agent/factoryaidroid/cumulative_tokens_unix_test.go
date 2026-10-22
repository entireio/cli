//go:build unix

package factoryaidroid

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/testutil"
)

// Worker discovery and the settings sidecar read only regular files: a symlink
// could lead out of the session store, and opening a FIFO would block.
func TestCumulativeTokenUsageReadsOnlyRegularFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	parent := writeCumulativeDroidSession(t, root, "parent-1", `{"type":"session_start","id":"parent-1"}`,
		`{"model":"m","tokenUsage":{"inputTokens":40,"outputTokens":300,"cacheCreationTokens":0,"cacheReadTokens":0,"thinkingTokens":0}}`)
	writeCumulativeDroidSession(t, root, "worker-1",
		`{"type":"session_start","id":"worker-1","callingSessionId":"parent-1","callingToolUseId":"call-1"}`,
		`{"model":"m","tokenUsage":{"inputTokens":7,"outputTokens":3,"cacheCreationTokens":0,"cacheReadTokens":0,"thinkingTokens":0}}`)

	// A Worker outside the store, linked in.
	outside := writeCumulativeDroidSession(t, t.TempDir(), "worker-2",
		`{"type":"session_start","id":"worker-2","callingSessionId":"parent-1","callingToolUseId":"call-2"}`,
		`{"model":"m","tokenUsage":{"inputTokens":99,"outputTokens":99,"cacheCreationTokens":0,"cacheReadTokens":0,"thinkingTokens":0}}`)
	if err := os.Symlink(outside, filepath.Join(root, "worker-2.jsonl")); err != nil {
		t.Fatal(err)
	}
	// A FIFO named like a Worker transcript.
	if err := syscall.Mkfifo(filepath.Join(root, "worker-3.jsonl"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A session whose settings sidecar is a symlink.
	linked := writeCumulativeDroidSession(t, root, "linked-1", `{"type":"session_start","id":"linked-1"}`, `{"model":"m"}`)
	settings := filepath.Join(root, "linked-1.settings.json")
	if err := os.Remove(settings); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "parent-1.settings.json"), settings); err != nil {
		t.Fatal(err)
	}

	droid := &FactoryAIDroidAgent{}
	total, children, err := droid.CumulativeTokenUsage(context.Background(), agent.CumulativeTokenInput{SessionID: "parent-1", TranscriptPath: parent})
	if err != nil {
		t.Fatal(err)
	}
	testutil.AssertCumulative(t, total, children, &agent.TokenUsage{InputTokens: 40, OutputTokens: 300},
		map[string]*agent.TokenUsage{"worker-1": {InputTokens: 7, OutputTokens: 3}})

	// A symlinked sidecar is not followed: the reading fails rather than
	// reporting the linked file's total, and the ledger keeps its last one.
	if total, _, err = droid.CumulativeTokenUsage(context.Background(), agent.CumulativeTokenInput{SessionID: "linked-1", TranscriptPath: linked}); err == nil {
		t.Fatalf("symlinked settings read as %+v, want an error", total)
	}
}
