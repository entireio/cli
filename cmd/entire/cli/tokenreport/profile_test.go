package tokenreport

import (
	"reflect"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

func TestProfileFor(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		agent types.AgentType
		want  AgentProfile
	}{
		{
			name:  "Claude Code",
			agent: agentClaudeCode,
			want: AgentProfile{
				RecordsThinking:     true,
				RecordsCacheTTL:     true,
				RecordsEffort:       true,
				RecordsModelPerCall: true,
				RecordsToolCalls:    true,
				RecordsSubagents:    true,
				RecordsCost:         false,
				EffortSource:        effortSourcePerCallField,
				TotalsOnly:          false,
				Verified:            true,
			},
		},
		{
			name:  "Codex",
			agent: agentCodex,
			want: AgentProfile{
				RecordsThinking:     true,
				RecordsCacheTTL:     false,
				RecordsEffort:       true,
				RecordsModelPerCall: true,
				RecordsToolCalls:    true,
				RecordsSubagents:    false,
				RecordsCost:         false,
				EffortSource:        effortSourceTurnContext,
				TotalsOnly:          false,
				Verified:            true,
			},
		},
		{
			name:  "OpenCode",
			agent: agentOpenCode,
			want: AgentProfile{
				RecordsThinking:     true,
				RecordsCacheTTL:     false,
				RecordsEffort:       false,
				RecordsModelPerCall: true,
				RecordsToolCalls:    true,
				RecordsSubagents:    true,
				RecordsCost:         false,
				EffortSource:        "",
				TotalsOnly:          false,
				Verified:            true,
			},
		},
		{
			name:  "Gemini CLI",
			agent: agentGemini,
			want: AgentProfile{
				RecordsThinking:     true,
				RecordsCacheTTL:     false,
				RecordsEffort:       false,
				RecordsModelPerCall: true,
				RecordsToolCalls:    true,
				RecordsSubagents:    false,
				RecordsCost:         false,
				EffortSource:        "",
				TotalsOnly:          false,
				Verified:            true,
			},
		},
		{
			name:  "Pi",
			agent: agentPi,
			want: AgentProfile{
				RecordsThinking:     false,
				RecordsCacheTTL:     true,
				RecordsEffort:       true,
				RecordsModelPerCall: true,
				RecordsToolCalls:    true,
				RecordsSubagents:    false,
				RecordsCost:         true,
				EffortSource:        effortSourceThinkingLevelEvents,
				TotalsOnly:          false,
				Verified:            true,
			},
		},
		{
			name:  "Cursor",
			agent: agentCursor,
			want: AgentProfile{
				RecordsThinking:     false,
				RecordsCacheTTL:     false,
				RecordsEffort:       true,
				RecordsModelPerCall: false,
				RecordsToolCalls:    false,
				RecordsSubagents:    false,
				RecordsCost:         false,
				EffortSource:        effortSourceModelName,
				TotalsOnly:          true,
				Verified:            true,
			},
		},
		{
			name:  "Copilot CLI",
			agent: agentCopilotCLI,
			want: AgentProfile{
				RecordsThinking:     false,
				RecordsCacheTTL:     false,
				RecordsEffort:       false,
				RecordsModelPerCall: true,
				RecordsToolCalls:    false,
				RecordsSubagents:    false,
				RecordsCost:         false,
				EffortSource:        "",
				TotalsOnly:          true,
				Verified:            true,
			},
		},
		{
			name:  "Factory AI Droid",
			agent: agentFactoryAIDroid,
			want: AgentProfile{
				RecordsThinking:     false,
				RecordsCacheTTL:     false,
				RecordsEffort:       false,
				RecordsModelPerCall: false,
				RecordsToolCalls:    false,
				RecordsSubagents:    false,
				RecordsCost:         false,
				EffortSource:        "",
				TotalsOnly:          true,
				Verified:            false,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := ProfileFor(tc.agent)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ProfileFor(%q) = %+v, want %+v", tc.agent, got, tc.want)
			}
			if got.Levers != nil {
				t.Errorf("ProfileFor(%q).Levers = %v, want nil in B1", tc.agent, got.Levers)
			}
			if got.EffortSettingVerified {
				t.Errorf("ProfileFor(%q).EffortSettingVerified = true, want false in B1", tc.agent)
			}
		})
	}
}

func TestProfileFor_UnknownAgent(t *testing.T) {
	t.Parallel()

	got := ProfileFor(types.AgentType("Some Unknown Agent"))
	if !reflect.DeepEqual(got, AgentProfile{}) {
		t.Errorf("ProfileFor(unknown) = %+v, want zero value", got)
	}
	if got.Verified {
		t.Error("ProfileFor(unknown).Verified = true, want false")
	}
}

func TestKnownAgents(t *testing.T) {
	t.Parallel()

	known := KnownAgents()
	if len(known) != 8 {
		t.Fatalf("KnownAgents() returned %d agents, want 8: %v", len(known), known)
	}

	seen := make(map[types.AgentType]bool, len(known))
	for _, a := range known {
		if seen[a] {
			t.Errorf("KnownAgents() contains duplicate %q", a)
		}
		seen[a] = true

		profile := ProfileFor(a)
		if reflect.DeepEqual(profile, AgentProfile{}) {
			t.Errorf("ProfileFor(%q) returned zero value; every known agent should have a non-zero profile", a)
		}
	}
}
