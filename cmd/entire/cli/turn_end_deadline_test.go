package cli

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/agent/vogon"
	"github.com/stretchr/testify/require"
)

func TestTurnEndFinalizeDeadline(t *testing.T) {
	t.Parallel()

	claude, err := agent.GetByAgentType(agent.AgentTypeClaudeCode)
	require.NoError(t, err)
	timeouter, ok := agent.AsTurnEndHookTimeouter(claude)
	require.True(t, ok)
	require.Equal(t, processStart.Add(agent.TurnEndBudgetWithin(timeouter.TurnEndHookTimeout())), turnEndFinalizeDeadline(agent.AgentTypeClaudeCode))

	fallback := processStart.Add(agent.TurnEndBudgetWithin(defaultTurnEndHookTimeout))
	require.Equal(t, fallback, turnEndFinalizeDeadline(agent.AgentTypeOpenCode), "an agent without a declared timeout gets the default")
	require.Equal(t, fallback, turnEndFinalizeDeadline(types.AgentType("future-agent")), "an unknown agent gets the default")
}

// turnEndHookTimeoutFallbacks lists the agents that deliberately take
// defaultTurnEndHookTimeout instead of declaring agent.TurnEndHookTimeouter,
// with the reason. Every other built-in agent must declare its timeout
// (external agents always take the default), so a new agent cannot inherit the
// default without someone deciding it should.
var turnEndHookTimeoutFallbacks = map[types.AgentName]string{
	agent.AgentNameCursor:   "Cursor documents only a \"platform default\" hook timeout, with no number",
	vogon.AgentNameVogon:    "deterministic test agent with no host process to time it out",
	agent.AgentNameOpenCode: "OpenCode applies no hook timeout; Entire's plugin blocks on the hook, so the default is the only bound",
}

// builtinAgents returns every registered agent except external ones, which
// always take the default (no protocol field carries a hook timeout) and which
// other tests in this package register globally.
func builtinAgents(t *testing.T) map[types.AgentName]agent.Agent {
	t.Helper()
	agents := make(map[types.AgentName]agent.Agent)
	for _, name := range agent.List() {
		ag, err := agent.Get(name)
		require.NoError(t, err)
		if _, external := ag.(agent.CapabilityDeclarer); external {
			continue
		}
		agents[name] = ag
	}
	return agents
}

func TestTurnEndHookTimeoutLedger(t *testing.T) {
	t.Parallel()

	for name, ag := range builtinAgents(t) {
		_, declares := agent.AsTurnEndHookTimeouter(ag)
		reason, listed := turnEndHookTimeoutFallbacks[name]
		switch {
		case declares && listed:
			t.Errorf("%s declares agent.TurnEndHookTimeouter; remove its stale turnEndHookTimeoutFallbacks entry", name)
		case !declares && !listed:
			t.Errorf("%s neither declares agent.TurnEndHookTimeouter nor appears in turnEndHookTimeoutFallbacks; declare its turn-end hook timeout, or list it with the reason it takes the default", name)
		case listed:
			require.NotEmpty(t, reason, "%s needs a reason", name)
		}
	}
	for name := range turnEndHookTimeoutFallbacks {
		_, err := agent.Get(name)
		require.NoError(t, err, "turnEndHookTimeoutFallbacks lists %s, which is not a registered agent", name)
	}
}

// A declared timeout must leave a finalize budget that is positive (or Entire
// never finalizes) and inside the timeout (or the host kills the hook first).
func TestDeclaredTurnEndHookTimeoutsLeaveABudget(t *testing.T) {
	t.Parallel()

	declaring := 0
	for name, ag := range builtinAgents(t) {
		timeouter, ok := agent.AsTurnEndHookTimeouter(ag)
		if !ok {
			continue
		}
		declaring++
		timeout := timeouter.TurnEndHookTimeout()
		budget := agent.TurnEndBudgetWithin(timeout)
		if timeout <= 0 {
			t.Errorf("%s declares a turn-end hook timeout of %v; declare the host's real limit or list it in turnEndHookTimeoutFallbacks", name, timeout)
			continue
		}
		if budget <= 0 || budget >= timeout {
			t.Errorf("%s: budget %v for hook timeout %v must be positive and below the timeout", name, budget, timeout)
		}
	}
	if declaring == 0 {
		t.Fatal("no built-in agent declares agent.TurnEndHookTimeouter; the loop checked nothing")
	}
}
