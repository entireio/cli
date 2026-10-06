package transcript

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse_NativeFixturesEqualDecodeOfConvert(t *testing.T) {
	t.Parallel()

	fixtures := []struct {
		file string
		opts Options
	}{
		{"claude_full.jsonl", defaultOpts},
		{"claude_full2.jsonl", defaultOpts},
		{"codex_full.jsonl", agentOpts("codex")},
		{"copilot_full.jsonl", agentOpts("copilot-cli")},
		{"droid_full.jsonl", agentOpts("factoryai-droid")},
		{"gemini_full.jsonl", agentOpts("gemini-cli")},
		{"opencode_full.jsonl", agentOpts("opencode")},
	}

	for _, f := range fixtures {
		t.Run(f.file, func(t *testing.T) {
			t.Parallel()
			native, err := os.ReadFile(filepath.Join("testdata", f.file))
			require.NoError(t, err)

			converted, err := Convert(native, f.opts)
			require.NoError(t, err)
			want, err := Decode(converted)
			require.NoError(t, err)
			require.NotEmpty(t, want)

			got, err := Parse(native, f.opts)
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
}

func TestParse_ExpectedFixturesDecodeCleanly(t *testing.T) {
	t.Parallel()

	paths, err := filepath.Glob(filepath.Join("testdata", "*_expected*.jsonl"))
	require.NoError(t, err)
	require.NotEmpty(t, paths)

	for _, p := range paths {
		t.Run(filepath.Base(p), func(t *testing.T) {
			t.Parallel()
			b, err := os.ReadFile(p)
			require.NoError(t, err)

			want, err := Decode(b)
			require.NoError(t, err)
			require.NotEmpty(t, want)

			got, err := Parse(b, Options{Agent: "ignored", CLIVersion: "9", StartLine: 5})
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
}

func TestParse_PropagatesConvertError(t *testing.T) {
	t.Parallel()

	lines, err := Parse([]byte(`{"sessionId":"s","messages":"not-an-array"}`), defaultOpts)
	require.Error(t, err)
	assert.Nil(t, lines)
}

func TestIsEntireFormat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"user line", `{"v":1,"type":"user","content":[]}`, true},
		{"assistant line after blank lines", "\n  \n" + `{"v":1,"type":"assistant"}`, true},
		{"native claude line without v", `{"type":"user","message":{"content":"hi"}}`, false},
		{"v is not an integer", `{"v":"1","type":"user"}`, false},
		{"unknown type", `{"v":1,"type":"system"}`, false},
		{"non-object first line", `[1]`, false},
		{"empty", ``, false},
		{"only first line is inspected", `{"type":"user"}` + "\n" + `{"v":1,"type":"user"}`, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, isEntireFormat([]byte(tc.input)))
		})
	}
}
