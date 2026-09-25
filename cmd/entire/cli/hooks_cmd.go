package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/logging"

	// Import agents to ensure they are registered before we iterate
	_ "github.com/entireio/cli/cmd/entire/cli/agent/antigravity"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/codex"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/copilotcli"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/cursor"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/factoryaidroid"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/opencode"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/pi"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/vogon"

	// support external agents
	"github.com/entireio/cli/cmd/entire/cli/agent/external"

	"github.com/spf13/cobra"
)

func newHooksCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "hooks",
		Short:  "Hook handlers",
		Long:   "Commands called by hooks. These are internal and not for direct user use.",
		Hidden: true, // Internal command, not for direct user use
		// RunE handles external agent hooks that aren't registered as subcommands.
		// When Cobra can't match a subcommand (e.g., "entire hooks my-ext-agent stop"),
		// it falls back to this RunE with args ["my-ext-agent", "stop"].
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) < 2 {
				return cmd.Help()
			}
			agentName := types.AgentName(args[0])
			hookName := args[1]

			// Lazily discover external agents only when actually needed.
			discoveryCtx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()
			external.DiscoverAndRegister(discoveryCtx)

			// Verify the agent was discovered
			if _, err := agent.Get(agentName); err != nil {
				// Hooks left behind by a retired built-in agent must not fail
				// every event in the agent that still runs them. Checked after
				// discovery so an external plugin may claim the name, and not
				// taken when that plugin is installed but discovery missed it
				// (a timeout): its hook should fail loudly, not vanish.
				if agentName == retiredGeminiAgentName && !retiredGeminiNameClaimed() {
					logging.Debug(cmd.Context(), "ignoring hook for retired agent",
						"agent", string(agentName), "hook", hookName)
					return nil
				}
				return fmt.Errorf("unknown agent %q (not found as built-in or external plugin)", agentName)
			}

			return executeAgentHook(cmd, agentName, hookName, true)
		},
	}

	// Git hooks are strategy-level (not agent-specific)
	cmd.AddCommand(newHooksGitCmd())

	// Only built-in agents are registered eagerly (no process spawning).
	// External agents are discovered lazily via the RunE fallback above.
	for _, agentName := range agent.List() {
		ag, err := agent.Get(agentName)
		if err != nil {
			continue
		}
		if handler, ok := agent.AsHookSupport(ag); ok {
			sub := newAgentHooksCmd(agentName, handler)
			// title-tee is not a lifecycle verb: it runs globally (outside
			// git repos, without the enabled check) and owns its stdout.
			//
			// Exempt from the root `.entire` guard, and only this command:
			// cobra.EnableTraverseRunHooks means root's PersistentPreRunE runs
			// for it, and RequireEntireDir answers nil only for
			// ErrNotARepository — an unresolvable repository (git's
			// safe.directory refusal, git absent from PATH) or a `.entire` that
			// is not a real directory makes it print a multi-line remedy to
			// stderr and fail the command. agy fires this on every agent state
			// change, so that would be a repeating stderr blob and a non-zero
			// exit per fire, against this command's NEVER-exit-non-zero
			// contract, with token capture silently dead. The tee writes only
			// to the per-user cache, never under `.entire`, so it has nothing
			// the guard protects. The lifecycle verbs keep the guard: they are
			// the checkpoint-writing path and must fail closed.
			if agentName == agent.AgentNameAntigravity {
				sub.AddCommand(exemptFromEntireDirCheck(newAntigravityTitleTeeCmd()))
			}
			cmd.AddCommand(sub)
		}
	}

	return cmd
}
