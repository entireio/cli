package cli

import (
	"testing"

	codexagent "github.com/entireio/cli/cmd/entire/cli/agent/codex"
)

func TestExtractTranscriptMetadataForAgent_CodexModel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, data, want string }{
		{"empty", "", ""},
		{"missing", `{"type":"turn_context","payload":{}}`, ""},
		{"latest valid context", `{"type":"turn_context","payload":{"model":"old"}}
{"type":"response_item","payload":{"model":"wrong"}}
invalid
{"type":"turn_context","payload":{"model":"gpt-6-astra"}}
{"type":"turn_context","payload":{"model":42}}
{"type":"turn_context","payload":null}
{"type":"turn_context","payload":{"model":""}}
{"type":"turn_context","payload":`, "gpt-6-astra"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := extractTranscriptMetadataForAgent(codexagent.NewCodexAgent(), "", []byte(tc.data))
			if got.Model != tc.want {
				t.Errorf("model = %q, want %q", got.Model, tc.want)
			}
		})
	}
}
