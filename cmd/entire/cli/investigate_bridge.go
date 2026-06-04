package cli

// investigate_bridge.go wires cli-package implementations into the
// investigate subpackage's NewCommand Deps struct. The bridge lives in
// the cli package to break the import cycle between investigate and the
// per-agent packages / checkpoint store.

import (
	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	"github.com/entireio/cli/cmd/entire/cli/agent/spawn"
	"github.com/entireio/cli/cmd/entire/cli/agentlaunch"
	"github.com/entireio/cli/cmd/entire/cli/investigate"
)

// buildInvestigateDeps builds the investigate.Deps used by
// investigate.NewCommand. LoopRun is left nil so production uses
// investigate.RunInvestigateLoop.
func buildInvestigateDeps() investigate.Deps {
	return investigate.Deps{
		GetAgentsWithHooksInstalled: GetAgentsWithHooksInstalled,
		NewSilentError: func(err error) error {
			return NewSilentError(err)
		},
		SpawnerFor:                   launchableSpawnerFor,
		LaunchFix:                    agentlaunch.LaunchFixAgent,
		HeadHasInvestigateCheckpoint: headHasInvestigateCheckpoint,
	}
}

// launchableSpawnerFor returns the Spawner for known launchable agents,
// or nil for non-launchable agents (cursor, opencode, factoryai-droid,
// copilot-cli, vogon). Lives in the cli package so the investigate
// subpackage does not import the per-agent packages (import cycle).
func launchableSpawnerFor(agentName string) spawn.Spawner {
	switch agentName {
	case string(agent.AgentNameClaudeCode):
		return claudecode.NewSpawner()
	case string(agent.AgentNameCodex):
		return codex.NewSpawner()
	case string(agent.AgentNameGemini):
		return geminicli.NewSpawner()
	default:
		return nil
	}
}
