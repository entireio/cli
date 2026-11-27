package cli

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestResolveSubagentModel(t *testing.T) {
	t.Parallel()
	pre := &PreTaskState{SubagentModel: "claude-4.5-haiku"}
	for _, tc := range []struct {
		name  string
		event string
		pre   *PreTaskState
		want  string
	}{
		{"stop names it", "gpt-5", pre, "gpt-5"},
		{"from start hook", "", pre, "claude-4.5-haiku"},
		{"neither", "", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveSubagentModel(&agent.Event{SubagentModel: tc.event}, tc.pre); got != tc.want {
				t.Errorf("resolveSubagentModel = %q, want %q", got, tc.want)
			}
		})
	}
}
