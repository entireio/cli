package cli

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	"github.com/stretchr/testify/assert"
)

func TestFormatResumeCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		agentType agent.AgentType
		sessionID string
		want      string
	}{
		{
			name:      "claude_code",
			agentType: agent.AgentTypeClaudeCode,
			sessionID: "abc123",
			want:      "claude -r abc123",
		},
		{
			name:      "gemini",
			agentType: agent.AgentTypeGemini,
			sessionID: "xyz789",
			want:      "gemini --resume xyz789",
		},
		{
			name:      "unknown_agent_returns_empty",
			agentType: "nonexistent-agent",
			sessionID: "abc123",
			want:      "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := formatResumeCommand(tt.agentType, tt.sessionID)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNewDashboardCmd_Exists(t *testing.T) {
	t.Parallel()

	cmd := newDashboardCmd()
	assert.Equal(t, "dashboard", cmd.Use)
	assert.NotEmpty(t, cmd.Short)
	assert.NotNil(t, cmd.RunE)
}
