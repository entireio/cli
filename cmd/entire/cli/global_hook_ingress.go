package cli

import (
	"context"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/globalhooks"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/spf13/cobra"
)

const globalHookIngressAnnotation = "entire-global-hook-ingress"

// globalHookScopeReplayActive identifies the internal session reconstruction path.
func globalHookScopeReplayActive() bool { return os.Getenv("ENTIRE_BINDING_REPLAY") == "1" }

// globalHookOwnsAgent is shared by both live agent ingress paths.
func globalHookOwnsAgent(ctx context.Context, name types.AgentName) bool {
	if !settings.GlobalTierEnabled(ctx) {
		return false
	}
	if _, err := globalhooks.Load(); err != nil {
		return false
	}
	ag, err := agent.Get(name)
	if err != nil {
		return false
	}
	support, ok := agent.AsUserHookSupport(ag)
	if !ok {
		return false
	}
	current, err := support.AreUserHooksInstalled(ctx)
	return err == nil && current
}

func newGlobalAgentHooksCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "global", Short: "User-level agent hook handlers", Hidden: true}
	supports, _ := agent.UserHookSupports()
	for _, candidate := range supports {
		ag, err := agent.Get(candidate.Name)
		if err != nil {
			continue
		}
		handler, ok := agent.AsHookSupport(ag)
		if !ok {
			continue
		}
		child := newAgentHooksCmd(candidate.Name, handler)
		for _, verb := range child.Commands() {
			verb.Annotations = map[string]string{globalHookIngressAnnotation: globalHookIngressAnnotation}
		}
		cmd.AddCommand(child)
	}
	return cmd
}
