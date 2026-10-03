package agent_test

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/antigravity"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/codex"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/copilotcli"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/cursor"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/factoryaidroid"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/opencode"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/pi"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/vogon"
)

// agentHomeConfinementExemptions documents agents that cannot provide a confined
// analyzer for their transcript format. Every home provider must either supply
// the confined capabilities it needs or explain that limitation here.
var agentHomeConfinementExemptions = map[types.AgentName]string{}

// TestAgentHomeProvidersImplementConfinedReads binds home reporting to confined
// analysis, preventing path-based analyzers from bypassing the session boundary.
func TestAgentHomeProvidersImplementConfinedReads(t *testing.T) {
	t.Parallel()

	checked := 0
	for _, name := range agent.List() {
		ag, err := agent.Get(name)
		if err != nil {
			t.Fatalf("agent.Get(%s): %v", name, err)
		}

		if _, isHomeProvider := agent.AsAgentHomeProvider(ag); !isHomeProvider {
			continue
		}
		checked++

		if reason, exempt := agentHomeConfinementExemptions[name]; exempt {
			if reason == "" {
				t.Errorf("%s is in agentHomeConfinementExemptions with no reason; every exemption must explain why confinement is impossible for this agent", name)
			}
			continue
		}

		if _, isAnalyzer := agent.AsTranscriptAnalyzer(ag); isAnalyzer {
			if _, isConfined := agent.AsConfinedTranscriptAnalyzer(ag); !isConfined {
				t.Errorf("%s implements AgentHomeProvider and TranscriptAnalyzer but not ConfinedTranscriptAnalyzer.\n"+
					"A caller holding this agent's recorded AgentHome has no way to read its transcript through a "+
					"confined path — add ExtractModifiedFilesFromBytes (see claudecode, codex, copilotcli, pi, "+
					"factoryaidroid for the pattern), or add this agent to agentHomeConfinementExemptions with the "+
					"reason confinement is impossible for it.", name)
			}
		}

		if _, extractsPrompts := agent.AsPromptExtractor(ag); extractsPrompts {
			if _, extractsBytes := agent.AsTranscriptPromptExtractor(ag); !extractsBytes {
				t.Errorf("%s implements AgentHomeProvider and PromptExtractor but not TranscriptPromptExtractor; late-flush prompts must use confined transcript bytes", name)
			}
		}

		if _, isSubagentAware := agent.AsSubagentAwareExtractor(ag); isSubagentAware {
			if _, isConfinedSubagent := agent.AsConfinedSubagentAwareExtractor(ag); !isConfinedSubagent {
				t.Errorf("%s implements AgentHomeProvider and SubagentAwareExtractor but not ConfinedSubagentAwareExtractor.\n"+
					"A caller holding this agent's recorded AgentHome has no way to read its subagent transcripts "+
					"through a confined path — add CalculateTotalTokenUsageUnderHome (see claudecode, factoryaidroid "+
					"for the pattern), or add this agent to agentHomeConfinementExemptions with the reason "+
					"confinement is impossible for it.", name)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no registered agent implements AgentHomeProvider — the detection pattern has gone stale, or agent self-registration broke")
	}
}
