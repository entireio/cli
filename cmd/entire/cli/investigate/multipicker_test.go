package investigate

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPickInvestigateAgents_RequiresTwo(t *testing.T) {
	t.Parallel()
	_, err := PickInvestigateAgents(context.Background(), []AgentChoice{{Name: "claude-code"}}, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "at least 2")
}

func TestPickInvestigateAgents_ContextCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := PickInvestigateAgents(ctx, []AgentChoice{
		{Name: "claude-code"}, {Name: "codex"},
	}, false)
	require.ErrorIs(t, err, ErrInvestigatePickerCancelled)
}

func TestPickInvestigateAgents_ResultSortedAlphabetically(t *testing.T) {
	t.Parallel()
	got := sortAgentChoices([]AgentChoice{
		{Name: "codex"},
		{Name: "claude-code"},
		{Name: "gemini-cli"},
	})
	require.Equal(t, []AgentChoice{
		{Name: "claude-code"},
		{Name: "codex"},
		{Name: "gemini-cli"},
	}, got)
}

// TestPickInvestigateAgents_PromptDefaultsEmpty documents the contract
// that Prompt defaults to the empty string. The huh form isn't drivable
// from a non-TTY test; this test pins the type-level guarantee that
// consumers can rely on "no prompt entered" being Prompt == "".
func TestPickInvestigateAgents_PromptDefaultsEmpty(t *testing.T) {
	t.Parallel()
	var zero PickedInvestigate
	require.Empty(t, zero.Prompt)
	require.Empty(t, zero.Names)
}
