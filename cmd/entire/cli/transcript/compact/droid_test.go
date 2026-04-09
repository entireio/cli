package compact

import (
	"strings"
	"testing"
)

// --- Factory AI Droid tests ---

func TestCompact_DroidFixture(t *testing.T) {
	t.Parallel()

	droidOpts := agentOpts("factoryai-droid")
	assertFixtureTransform(t, droidOpts, "testdata/droid_full.jsonl", "testdata/droid_expected.jsonl")
}

func TestCompact_FactoryDroidInlineCases(t *testing.T) {
	t.Parallel()

	droidOpts := agentOpts("factoryai-droid")

	tests := []struct {
		name     string
		input    []byte
		expected []string
	}{
		{
			name: "envelope",
			input: []byte(`{"type":"message","id":"m1","timestamp":"t1","message":{"role":"user","content":[{"type":"text","text":"create a file"}]}}
{"type":"message","id":"m2","timestamp":"t2","message":{"role":"assistant","content":[{"type":"text","text":"Done!"},{"type":"tool_use","id":"tu-1","name":"Write","input":{"file_path":"hello.txt","content":"hi"}}]}}
`),
			expected: []string{
				`{"v":1,"agent":"factoryai-droid","cli_version":"0.5.1","type":"user","ts":"t1","content":[{"text":"create a file"}]}`,
				`{"v":1,"agent":"factoryai-droid","cli_version":"0.5.1","type":"assistant","ts":"t2","id":"m2","content":[{"type":"text","text":"Done!"},{"type":"tool_use","id":"tu-1","name":"Write","input":{"file_path":"hello.txt","content":"hi"}}]}`,
			},
		},
		{
			name: "tool result",
			input: []byte(`{"type":"message","id":"m1","timestamp":"t1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu-1","content":"success"},{"type":"text","text":"next step"}]}}
`),
			expected: []string{
				`{"v":1,"agent":"factoryai-droid","cli_version":"0.5.1","type":"user","ts":"t1","content":[{"text":"next step"}]}`,
			},
		},
		{
			name: "drops non-message entries",
			input: []byte(`{"type":"session_start","id":"sess-1","title":"test"}
{"type":"message","id":"m1","timestamp":"t1","message":{"role":"user","content":"hello"}}
{"type":"session_event","data":"something"}
{"type":"message","id":"m2","timestamp":"t2","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}
`),
			expected: []string{
				`{"v":1,"agent":"factoryai-droid","cli_version":"0.5.1","type":"user","ts":"t1","content":[{"text":"hello"}]}`,
				`{"v":1,"agent":"factoryai-droid","cli_version":"0.5.1","type":"assistant","ts":"t2","id":"m2","content":[{"type":"text","text":"hi"}]}`,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result, err := Compact(tc.input, droidOpts)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			assertJSONLines(t, result, tc.expected)
		})
	}
}

func TestIsDroidFormat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input []byte
		want  bool
	}{
		{
			name:  "droid envelope",
			input: []byte(`{"type":"session_start","id":"s1"}` + "\n" + `{"type":"message","id":"m1","message":{"role":"user","content":"hi"}}` + "\n"),
			want:  true,
		},
		{
			name:  "claude code format",
			input: []byte(`{"type":"user","uuid":"u1","timestamp":"t1","message":{"content":"hi"}}` + "\n"),
			want:  false,
		},
		{
			name:  "empty",
			input: []byte{},
			want:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isDroidFormat(tc.input); got != tc.want {
				t.Errorf("isDroidFormat() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsDroidFormat_LargeLeadingLine(t *testing.T) {
	t.Parallel()

	large := strings.Repeat("x", 70*1024)
	input := []byte(
		`{"type":"session_start","title":"` + large + `"}` + "\n" +
			`{"type":"message","id":"m1","message":{"role":"user","content":"hi"}}` + "\n",
	)

	if got := isDroidFormat(input); !got {
		t.Fatalf("isDroidFormat() = %v, want true", got)
	}
}

func TestCompact_DroidStartLine(t *testing.T) {
	t.Parallel()

	opts := MetadataFields{Agent: "factoryai-droid", CLIVersion: "0.5.1", StartLine: 2}

	input := []byte(`{"type":"session_start","id":"sess-1","title":"test"}
{"type":"message","id":"m1","timestamp":"t1","message":{"role":"user","content":"first prompt"}}
{"type":"message","id":"m2","timestamp":"t2","message":{"role":"user","content":"second prompt"}}
{"type":"message","id":"m3","timestamp":"t3","message":{"role":"assistant","content":[{"type":"text","text":"response"}]}}
`)

	expected := []string{
		`{"v":1,"agent":"factoryai-droid","cli_version":"0.5.1","type":"user","ts":"t2","content":[{"text":"second prompt"}]}`,
		`{"v":1,"agent":"factoryai-droid","cli_version":"0.5.1","type":"assistant","ts":"t3","id":"m3","content":[{"type":"text","text":"response"}]}`,
	}

	result, err := Compact(input, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertJSONLines(t, result, expected)
}
