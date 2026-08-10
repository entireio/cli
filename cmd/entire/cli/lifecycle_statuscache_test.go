package cli

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// TestStatusCacheSafe_ClosedAllowlist pins which lifecycle events may reuse a
// single worktree status. Allowing an event whose handler writes a tracked file
// between its first and last status read produces silently stale checkpoints.
//
// This table is a manual enumeration, not a compile-time exhaustive one: adding
// a constant to the agent package does not break this test. The exhaustive
// linter on statusCacheSafe's switch is what forces a new EventType to be
// classified — update this table alongside that switch.
func TestStatusCacheSafe_ClosedAllowlist(t *testing.T) {
	t.Parallel()

	tests := []struct {
		event agent.EventType
		want  bool
		why   string
	}{
		{agent.TurnStart, true, "runs before the agent acts; EnsureSetup hoisted above the first status read"},
		{agent.SessionStart, false, "no second status read to share; not reviewed for tracked-file writes"},
		{agent.TurnEnd, false, "post-agent: DetectFileChanges must observe the agent's edits"},
		{agent.Compaction, false, "shares TurnEnd's save path"},
		{agent.SessionEnd, false, "post-agent"},
		{agent.SubagentStart, false, "not reviewed for tracked-file writes"},
		{agent.SubagentEnd, false, "post-agent: DetectFileChanges must observe the subagent's edits"},
		{agent.ModelUpdate, false, "no status read"},
		{agent.ToolUse, false, "mid-turn: the agent is actively editing files"},
	}

	for _, tt := range tests {
		t.Run(tt.event.String(), func(t *testing.T) {
			t.Parallel()

			if got := statusCacheSafe(tt.event); got != tt.want {
				t.Errorf("statusCacheSafe(%s) = %v, want %v (%s)",
					tt.event, got, tt.want, tt.why)
			}
		})
	}
}

// TestStatusCacheSafe_UnknownEventDeniedByDefault guards the default branch: an
// EventType added to the agent package but not considered here must not silently
// inherit caching.
func TestStatusCacheSafe_UnknownEventDeniedByDefault(t *testing.T) {
	t.Parallel()

	// Well past the last defined constant.
	if statusCacheSafe(agent.EventType(9999)) {
		t.Error("statusCacheSafe(unknown) = true, want false: new events must be opt-in")
	}
}
