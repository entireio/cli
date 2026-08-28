package cli

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/tokenreport"
)

// TestTokenreportAgents_MatchesRegistry guards against drift between the
// registered, non-test-only, built-in agents in the agent package and
// tokenreport's internal per-agent profile map, which cannot import the
// agent package (see tokenreport/doc.go) and therefore duplicates each
// agent's Type() string as an unexported constant. It asserts the two sets
// of agent types are equal: every registered built-in agent has a
// tokenreport profile, and tokenreport claims no agent the registry
// doesn't have. It does not check profile field values — those belong to
// profile_test.go.
//
// Two kinds of registered agent are excluded because they are not part of
// tokenreport's fixed catalog of known transcript shapes:
//   - agent.TestOnly agents (e.g. Vogon), which exist solely for testing.
//   - agent.CapabilityDeclarer agents: only dynamically-discovered external
//     agents implement this (see its doc comment: built-in agents do not),
//     and several unrelated tests elsewhere in this package register fake
//     external agents into this same process-global registry without
//     restoring it afterward (their own comments note "external agent
//     discovery mutates the [registry]" as the reason they avoid
//     t.Parallel()). Those leak for the rest of the test binary's run, so
//     the full `go test ./cmd/entire/cli/` sees registry entries like
//     "Hook Test Agent" or "Roger Roger Agent" that a package-only test run
//     never would. They are also semantically out of scope here regardless
//     of the leak: an arbitrary external agent's transcript shape is
//     unknown by definition, which is exactly what ProfileFor's
//     unknown-agent default (AgentProfile{TotalsOnly: true}) is for.
func TestTokenreportAgents_MatchesRegistry(t *testing.T) {
	t.Parallel()

	registered := make(map[types.AgentType]bool)
	for _, name := range agent.List() {
		a, err := agent.Get(name)
		if err != nil {
			t.Fatalf("agent.Get(%q): %v", name, err)
		}
		if to, ok := a.(agent.TestOnly); ok && to.IsTestOnly() {
			continue // test-only agents (e.g. Vogon) have no token report profile
		}
		if _, ok := a.(agent.CapabilityDeclarer); ok {
			continue // dynamically-discovered external agent, not a built-in
		}
		registered[a.Type()] = true
	}

	known := tokenreport.KnownAgents()
	knownSet := make(map[types.AgentType]bool, len(known))
	for _, a := range known {
		knownSet[a] = true
	}

	for agentType := range registered {
		t.Run(string(agentType), func(t *testing.T) {
			t.Parallel()

			if !knownSet[agentType] {
				t.Errorf("registered agent type %q has no tokenreport profile; registry and tokenreport profile map have drifted", agentType)
			}
		})
	}

	for agentType := range knownSet {
		if !registered[agentType] {
			t.Errorf("tokenreport.KnownAgents() contains %q, which is not a registered non-test-only agent", agentType)
		}
	}

	if knownSet[agent.AgentTypeUnknown] {
		t.Error("tokenreport.KnownAgents() must not contain agent.AgentTypeUnknown")
	}
}
