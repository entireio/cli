package gitenv

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TraceCommands records native Git process starts after this call. The returned
// reader includes all starts so far, including nested Git invocations. Call only
// after fixture setup and root-cache warming when measuring a read operation.
// It changes process-global state and cannot be used in parallel tests.
func TraceCommands(t *testing.T) func() [][]string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "git-trace.jsonl")
	t.Setenv("GIT_TRACE2_EVENT", path)
	return func() [][]string {
		t.Helper()
		data, err := os.ReadFile(path) //nolint:gosec // fixed filename inside this test's t.TempDir, not caller input
		if err != nil {
			t.Fatalf("read Git trace: %v", err)
		}
		var commands [][]string
		for line := range bytes.SplitSeq(data, []byte{'\n'}) {
			if len(line) == 0 {
				continue
			}
			var event struct {
				Event string   `json:"event"`
				Argv  []string `json:"argv"`
			}
			if err := json.Unmarshal(line, &event); err != nil {
				t.Fatalf("decode Git trace: %v", err)
			}
			if event.Event == "start" {
				commands = append(commands, event.Argv)
			}
		}
		return commands
	}
}
