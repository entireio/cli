package cli

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/agent/vogon"
)

// Factory AI Droid is banner-only (no context injection, no agent-help skill
// file), so it is the one built-in agent that gets the agent-help pointer
// appended to its SessionStart banner. Every other agent already receives the
// pointer via context injection or a skill file, or relies on the passive
// `entire status` surface, so none of them get a duplicate banner pointer.
func TestAgentHelpBannerSuffix(t *testing.T) {
	t.Parallel()

	got := agentHelpBannerSuffix(agent.AgentNameFactoryAIDroid)
	if !strings.Contains(got, agentHelpCommand) {
		t.Errorf("Factory Droid banner suffix should point at `entire agent-help`, got %q", got)
	}

	for _, name := range []types.AgentName{
		agent.AgentNameClaudeCode,
		agent.AgentNameCodex,
		agent.AgentNameGemini,
		agent.AgentNameCursor,
		agent.AgentNameCopilotCLI,
		agent.AgentNameOpenCode,
		agent.AgentNamePi,
		vogon.AgentNameVogon, // the deterministic test agent must stay banner-free
	} {
		if suffix := agentHelpBannerSuffix(name); suffix != "" {
			t.Errorf("agent %q should not get a banner agent-help pointer (avoids a duplicate), got %q", name, suffix)
		}
	}
}

// The agent-help pointer must survive an agent-supplied ResponseMessage override:
// the override replaces the assembled message wholesale, so the pointer has to be
// appended after it, not before (else Factory Droid loses its only in-session
// pointer the moment an agent sets a custom banner).
func TestFinalizeSessionStartBanner(t *testing.T) {
	t.Parallel()

	// Factory + assembled message: pointer appended to the base.
	if out := finalizeSessionStartBanner("base message", "", agent.AgentNameFactoryAIDroid); !strings.Contains(out, "base message") || !strings.Contains(out, agentHelpCommand) {
		t.Errorf("Factory banner should append the pointer to the base message, got %q", out)
	}

	// Factory + ResponseMessage override: override wins, but the pointer survives.
	out := finalizeSessionStartBanner("base message", "custom override", agent.AgentNameFactoryAIDroid)
	if !strings.Contains(out, "custom override") {
		t.Errorf("ResponseMessage override should replace the assembled message, got %q", out)
	}
	if strings.Contains(out, "base message") {
		t.Errorf("override should replace, not append to, the base message, got %q", out)
	}
	if !strings.Contains(out, agentHelpCommand) {
		t.Errorf("Factory pointer must survive the ResponseMessage override, got %q", out)
	}

	// Non-Factory: no pointer; an override is respected verbatim.
	if out := finalizeSessionStartBanner("base", "custom", agent.AgentNameClaudeCode); out != "custom" {
		t.Errorf("non-banner agent should get the override verbatim with no pointer, got %q", out)
	}
}
