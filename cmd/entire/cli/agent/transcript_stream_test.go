package agent_test

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/agent/copilotcli"
	"github.com/entireio/cli/cmd/entire/cli/agent/factoryaidroid"
	"github.com/stretchr/testify/require"
)

func TestStreamingTranscriptAnalysis(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		analyzer agent.StreamingTranscriptAnalyzer
		line     string
	}{
		{"Claude", &claudecode.ClaudeCodeAgent{}, `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"hello.txt"}}]}}`},
		{"Codex", &codex.CodexAgent{}, `{"type":"response_item","payload":{"type":"custom_tool_call","name":"apply_patch","input":"*** Begin Patch\n*** Add File: hello.txt\n+hello\n*** End Patch\n"}}`},
		{"Copilot", &copilotcli.CopilotCLIAgent{}, `{"type":"tool.execution_complete","data":{"toolTelemetry":{"properties":{"filePaths":"[\"hello.txt\"]"},"metrics":{"linesAdded":1,"linesRemoved":0}}}}`},
		{"Droid", &factoryaidroid.FactoryAIDroidAgent{}, `{"type":"message","message":{"role":"assistant","content":[{"type":"tool_use","name":"Create","input":{"file_path":"hello.txt"}}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Blank and malformed records still advance the offset. The last record
			// has no newline and must be processed even through short reads.
			content := tc.line + "\n\ninvalid JSON\n" + tc.line
			for _, offset := range []int{0, 1, 3, 4, 100} {
				t.Run(strconv.Itoa(offset), func(t *testing.T) {
					t.Parallel()
					files, pos, err := tc.analyzer.ExtractModifiedFilesFromReader(iotest.OneByteReader(strings.NewReader(content)), offset)
					require.NoError(t, err)
					require.Equal(t, 4, pos)
					if offset < 4 {
						require.Equal(t, []string{"hello.txt"}, files)
					} else {
						require.Empty(t, files)
					}
				})
			}
			_, _, err := tc.analyzer.ExtractModifiedFilesFromReader(iotest.ErrReader(io.ErrClosedPipe), 0)
			require.ErrorIs(t, err, io.ErrClosedPipe)
		})
	}
}

// Compare the production streaming route with the bytes route retained for
// callers that already need a complete transcript.
func BenchmarkTranscriptAnalysis(b *testing.B) {
	for _, size := range []int{8, 64} {
		b.Run(fmt.Sprintf("%dMiB", size), func(b *testing.B) {
			home := b.TempDir()
			path := filepath.Join(home, "rollout.jsonl")
			line := `{"type":"event_msg","payload":{"padding":"` + strings.Repeat("x", 970) + `"}}` + "\n"
			lines := size * 1024 * 1024 / len(line)
			content := strings.Repeat(line, lines)
			require.NoError(b, os.WriteFile(path, []byte(content), 0o600))
			ag := &codex.CodexAgent{}
			for _, stream := range []bool{false, true} {
				b.Run(fmt.Sprintf("stream=%t", stream), func(b *testing.B) {
					b.ReportAllocs()
					b.SetBytes(int64(len(content)))
					for b.Loop() {
						var pos int
						if stream {
							file, err := agent.OpenTranscriptFileUnderHome(path, home)
							require.NoError(b, err)
							_, pos, err = ag.ExtractModifiedFilesFromReader(file, lines-1)
							require.NoError(b, file.Close())
							require.NoError(b, err)
						} else {
							data, err := agent.ReadTranscriptFileUnderHome(path, home)
							require.NoError(b, err)
							_, pos, err = ag.ExtractModifiedFilesFromBytes(data, lines-1)
							require.NoError(b, err)
						}
						require.Equal(b, lines, pos)
					}
				})
			}
		})
	}
}
