package checkpoint

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// TestSanitizeTranscriptForAgentType_BlanksInjectedInstructions pins the store's
// own sanitizer call for each agent whose transcript carries instruction text.
// Every upstream path sanitizes too; this one must not depend on them.
func TestSanitizeTranscriptForAgentType_BlanksInjectedInstructions(t *testing.T) {
	t.Parallel()

	const private = "Client Acme Bank: staging db server is at 10.20.30.40"
	tests := map[string]struct {
		agentType  types.AgentType
		transcript string
		want       string
	}{
		"Claude Code": {
			agentType: agent.AgentTypeClaudeCode,
			transcript: `{"type":"user","uuid":"u1","message":{"role":"user","content":"hello"}}` + "\n" +
				`{"type":"attachment","uuid":"i1","attachment":{"type":"instructions","files":[{"path":"/home/dev/.claude/memory/clients.md","content":"` + private + `"}]}}` + "\n",
			want: "hello",
		},
		"Codex": {
			agentType: agent.AgentTypeCodex,
			transcript: `{"type":"session_meta","payload":{"id":"abc","base_instructions":{"text":"` + private + `"}}}` + "\n" +
				`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}` + "\n",
			want: "hello",
		},
		"Pi": {
			agentType: agent.AgentTypePi,
			transcript: `{"type":"message","id":"m1","message":{"role":"system","sections":{"project_context":"` + private + `"}}}` + "\n" +
				`{"type":"message","id":"m2","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}` + "\n",
			want: "hello",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := string(SanitizeTranscriptForAgentType(tc.agentType, []byte(tc.transcript)))

			if strings.Contains(got, private) {
				t.Errorf("instruction text survived the store's sanitizer:\n%s", got)
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("conversation text %q was lost:\n%s", tc.want, got)
			}
			// Trimmed, because the Codex sanitizer drops the final newline.
			gotBreaks := strings.Count(strings.TrimSpace(got), "\n")
			wantBreaks := strings.Count(strings.TrimSpace(tc.transcript), "\n")
			if gotBreaks != wantBreaks {
				t.Errorf("line count changed: got %d line breaks, want %d", gotBreaks, wantBreaks)
			}
		})
	}
}
