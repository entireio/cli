package compact

import (
	"testing"
)

func TestCompact_GeminiFixture(t *testing.T) {
	t.Parallel()
	assertFixtureTransform(t, agentOpts("gemini-cli"), "testdata/gemini_full.jsonl", "testdata/gemini_expected.jsonl")
}

func TestCompact_GeminiStartLine(t *testing.T) {
	t.Parallel()

	input := []byte(`{
		"sessionId": "s1",
		"messages": [
			{"id":"m1","timestamp":"2026-01-01T00:00:00Z","type":"user","content":"hello"},
			{"id":"m2","timestamp":"2026-01-01T00:00:01Z","type":"gemini","content":"hi there","tokens":{"input":10,"output":5}},
			{"id":"m3","timestamp":"2026-01-01T00:00:02Z","type":"user","content":"bye"}
		]
	}`)

	t.Run("skip first message", func(t *testing.T) {
		t.Parallel()
		opts := MetadataFields{Agent: "gemini-cli", CLIVersion: "0.5.1", StartLine: 1}
		expected := []string{
			`{"v":1,"agent":"gemini-cli","cli_version":"0.5.1","type":"assistant","ts":"2026-01-01T00:00:01Z","id":"m2","input_tokens":10,"output_tokens":5,"content":[{"type":"text","text":"hi there"}]}`,
			`{"v":1,"agent":"gemini-cli","cli_version":"0.5.1","type":"user","ts":"2026-01-01T00:00:02Z","content":[{"text":"bye"}]}`,
		}
		result, err := Compact(input, opts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertJSONLines(t, result, expected)
	})

	t.Run("skip all messages", func(t *testing.T) {
		t.Parallel()
		opts := MetadataFields{Agent: "gemini-cli", CLIVersion: "0.5.1", StartLine: 100}
		result, err := Compact(input, opts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertJSONLines(t, result, nil)
	})

	t.Run("no truncation", func(t *testing.T) {
		t.Parallel()
		opts := MetadataFields{Agent: "gemini-cli", CLIVersion: "0.5.1", StartLine: 0}
		result, err := Compact(input, opts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		lines := nonEmptyLines(result)
		if len(lines) != 3 {
			t.Fatalf("expected 3 lines, got %d", len(lines))
		}
	})
}

func TestIsGeminiFormat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want bool
	}{
		{
			name: "valid gemini",
			in:   `{"sessionId":"s1","messages":[]}`,
			want: true,
		},
		{
			name: "opencode has info key",
			in:   `{"info":{"id":"s1"},"messages":[]}`,
			want: false,
		},
		{
			name: "JSONL not JSON object",
			in:   `{"type":"user","message":{}}` + "\n" + `{"type":"assistant","message":{}}`,
			want: false,
		},
		{
			name: "empty",
			in:   "",
			want: false,
		},
		{
			name: "missing messages key",
			in:   `{"sessionId":"s1"}`,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isGeminiFormat([]byte(tt.in))
			if got != tt.want {
				t.Errorf("isGeminiFormat(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
