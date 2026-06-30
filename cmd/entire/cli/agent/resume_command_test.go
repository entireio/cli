package agent

import (
	"reflect"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

func TestResumeCommandSpecFor(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		agentName types.AgentName
		sessionID string
		want      ForegroundCommandSpec
		wantOK    bool
	}{
		{
			name:      "claude code",
			agentName: AgentNameClaudeCode,
			sessionID: "session-123",
			want:      ForegroundCommandSpec{Binary: "claude", Args: []string{"-r", "session-123"}},
			wantOK:    true,
		},
		{
			name:      "codex",
			agentName: AgentNameCodex,
			sessionID: "session-123",
			want:      ForegroundCommandSpec{Binary: "codex", Args: []string{"resume", "session-123"}},
			wantOK:    true,
		},
		{
			name:      "copilot",
			agentName: AgentNameCopilotCLI,
			sessionID: "session-123",
			want:      ForegroundCommandSpec{Binary: "copilot", Args: []string{"--resume", "session-123"}},
			wantOK:    true,
		},
		{
			name:      "factory ai droid",
			agentName: AgentNameFactoryAIDroid,
			sessionID: "session-123",
			want:      ForegroundCommandSpec{Binary: "droid", Args: []string{"--session-id", "session-123"}},
			wantOK:    true,
		},
		{
			name:      "gemini",
			agentName: AgentNameGemini,
			sessionID: "session-123",
			want:      ForegroundCommandSpec{Binary: "gemini", Args: []string{"--resume", "session-123"}},
			wantOK:    true,
		},
		{
			name:      "opencode",
			agentName: AgentNameOpenCode,
			sessionID: "session-123",
			want:      ForegroundCommandSpec{Binary: "opencode", Args: []string{"-s", "session-123"}},
			wantOK:    true,
		},
		{
			name:      "pi",
			agentName: AgentNamePi,
			sessionID: "session-123",
			want:      ForegroundCommandSpec{Binary: "pi", Args: []string{"--session", "session-123"}},
			wantOK:    true,
		},
		{
			name:      "leading dash session id is not launchable",
			agentName: AgentNameClaudeCode,
			sessionID: "--dangerously-skip-permissions",
			wantOK:    false,
		},
		{
			name:      "cursor is print only",
			agentName: AgentNameCursor,
			sessionID: "session-123",
			wantOK:    false,
		},
		{
			name:      "unknown is print only",
			agentName: types.AgentName("unknown"),
			sessionID: "session-123",
			wantOK:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := ResumeCommandSpecFor(tc.agentName, tc.sessionID)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("spec = %#v, want %#v", got, tc.want)
			}
		})
	}
}
