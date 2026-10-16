package cli

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/codex"

	"github.com/stretchr/testify/require"
)

// TestTaskUsageRecountable pins which completed tasks condensation may recount
// from their stored transcript. A Codex child completed without a stop of its
// own (the session-end sweep of a child whose rollout never resolved) carries
// no usage, and recounting its forked rollout would add the parent's tokens.
func TestTaskUsageRecountable(t *testing.T) {
	t.Parallel()

	reported := &agent.TokenUsage{InputTokens: 1, APICallCount: 1}
	tests := []struct {
		name  string
		ag    agent.Agent
		event agent.Event
		want  bool
	}{
		{name: "Claude Code usage from the transcript", ag: claudecode.NewClaudeCodeAgent(), want: true},
		{name: "usage the event reported", ag: claudecode.NewClaudeCodeAgent(), event: agent.Event{TokenUsage: reported}},
		{name: "Codex child without usage", ag: codex.NewCodexAgent()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, taskUsageRecountable(tt.ag, &tt.event))
		})
	}
}
