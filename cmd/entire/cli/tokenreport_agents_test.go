package cli

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/tokenreport"
)

// TestTokenreportAgents_MatchesRegistry guards against drift between the
// agent.AgentType* constants in registry.go and tokenreport's internal
// per-agent profile map, which cannot import the agent package (see
// tokenreport/doc.go) and therefore duplicates the eight type-identifier
// strings as unexported constants. If a registry constant is renamed or a
// new agent is added without updating tokenreport/profile.go, this test
// fails.
func TestTokenreportAgents_MatchesRegistry(t *testing.T) {
	t.Parallel()

	registryAgents := []types.AgentType{
		agent.AgentTypeClaudeCode,
		agent.AgentTypeCodex,
		agent.AgentTypeCursor,
		agent.AgentTypeGemini,
		agent.AgentTypeOpenCode,
		agent.AgentTypePi,
		agent.AgentTypeCopilotCLI,
		agent.AgentTypeFactoryAIDroid,
	}

	known := tokenreport.KnownAgents()
	if len(known) != len(registryAgents) {
		t.Fatalf("tokenreport.KnownAgents() has %d entries, want %d matching registry constants", len(known), len(registryAgents))
	}

	knownSet := make(map[types.AgentType]bool, len(known))
	for _, a := range known {
		knownSet[a] = true
	}

	for _, a := range registryAgents {
		t.Run(string(a), func(t *testing.T) {
			t.Parallel()

			if !knownSet[a] {
				t.Errorf("agent.AgentType %q is not present in tokenreport.KnownAgents(); registry and tokenreport profile map have drifted", a)
			}

			profile := tokenreport.ProfileFor(a)
			if !profile.Verified && a != agent.AgentTypeFactoryAIDroid {
				t.Errorf("tokenreport.ProfileFor(%q).Verified = false, want true (Factory AI Droid is the only unverified known agent)", a)
			}
		})
	}

	if got := tokenreport.ProfileFor(agent.AgentTypeUnknown); got.Verified {
		t.Errorf("tokenreport.ProfileFor(agent.AgentTypeUnknown) has Verified = true, want false for the zero/unknown profile")
	}
}
