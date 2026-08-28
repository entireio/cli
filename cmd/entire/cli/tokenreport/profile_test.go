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
				EffortSource:        EffortSourcePerCallField,
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
				EffortSource:        EffortSourceTurnContext,
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
				EffortSource:        EffortSourceThinkingLevelEvents,
				TotalsOnly:          false,
				Verified:            true,
			},
		},
		{
			// Cursor's model name is a display hint the report can show, not a
			// per-call effort field: RecordsEffort is false because effort
			// rules need per-call data, while EffortSource still names where
			// that display hint comes from.
			name:  "Cursor",
			agent: agentCursor,
			want: AgentProfile{
				RecordsThinking:     false,
				RecordsCacheTTL:     false,
				RecordsEffort:       false,
				RecordsModelPerCall: false,
				RecordsToolCalls:    false,
				RecordsSubagents:    false,
				RecordsCost:         false,
				EffortSource:        EffortSourceModelName,
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

// TestProfileFor_UnknownAgent asserts the safe default for an agent with no
// known profile: totals-only, with every record-* flag and Verified false,
// rather than a bare zero value that a caller could mistake for "this agent
// records nothing, not even a total".
func TestProfileFor_UnknownAgent(t *testing.T) {
	t.Parallel()

	want := AgentProfile{TotalsOnly: true}
	got := ProfileFor(types.AgentType("Some Unknown Agent"))
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ProfileFor(unknown) = %+v, want %+v", got, want)
	}
	if got.Verified {
		t.Error("ProfileFor(unknown).Verified = true, want false")
	}
}

func TestKnownAgents(t *testing.T) {
	t.Parallel()

	known := KnownAgents()
	if len(known) == 0 {
		t.Fatal("KnownAgents() returned no agents")
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

	if again := KnownAgents(); !reflect.DeepEqual(known, again) {
		t.Errorf("KnownAgents() is not deterministic: got %v then %v", known, again)
	}
}
