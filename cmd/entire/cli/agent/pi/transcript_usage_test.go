package pi

import (
	"os"
	"testing"

	transcriptlib "github.com/entireio/cli/transcript"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Per-line usage in the converted transcript must add up to the session totals
// that CalculateTokenUsage reports, including contentless and abandoned-branch
// messages.
func TestCompactUsageSumsMatchNativeTotals(t *testing.T) {
	t.Parallel()

	fixture, err := os.ReadFile("../../../../../transcript/testdata/pi_full.jsonl")
	require.NoError(t, err)

	tests := []struct {
		name string
		data string
	}{
		{"fixture with contentless calls", string(fixture)},
		{"branching session", testBranchingSessionJSONL},
		{"linear session", testSessionJSONL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			native, err := (&PiAgent{}).CalculateTokenUsage([]byte(tt.data), 0)
			require.NoError(t, err)

			out, err := transcriptlib.Convert([]byte(tt.data), transcriptlib.Options{Agent: "pi"})
			require.NoError(t, err)
			lines, err := transcriptlib.Decode(out)
			require.NoError(t, err)

			var in, outTok, read, write int64
			for _, l := range lines {
				if l.Model == "" {
					continue
				}
				in += l.InputTokens
				outTok += l.OutputTokens
				read += l.CacheReadTokens
				write += l.CacheWriteTokens
			}
			assert.Equal(t, int64(native.InputTokens), in, "input")
			assert.Equal(t, int64(native.OutputTokens), outTok, "output")
			assert.Equal(t, int64(native.CacheReadTokens), read, "cache read")
			assert.Equal(t, int64(native.CacheCreationTokens), write, "cache write")
		})
	}
}
