package testutil

import (
	"context"
	"testing"

	cliagent "github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"

	_ "github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/codex"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/copilotcli"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/cursor"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/factoryaidroid"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/opencode"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/vogon"
)

// RestoredSessionTranscriptPath returns the path where resume should restore
// the transcript for agents that use file-backed restored sessions.
func RestoredSessionTranscriptPath(t *testing.T, repoDir string, meta SessionMetadata) (string, bool) {
	t.Helper()

	external.DiscoverAndRegisterAlways(context.Background())

	agentType := types.AgentType(meta.Agent)
	if agentType == cliagent.AgentTypeOpenCode {
		// OpenCode restores by importing the session into its database, not by
		// writing the transcript path returned by ResolveSessionFile.
		return "", false
	}

	ag, err := cliagent.GetByAgentType(agentType)
	if err != nil {
		t.Fatalf("resolve agent %q for restored transcript path: %v", meta.Agent, err)
	}

	sessionDir, err := ag.GetSessionDir(repoDir)
	if err != nil {
		t.Fatalf("get session dir for agent %q: %v", meta.Agent, err)
	}

	return ag.ResolveSessionFile(sessionDir, meta.SessionID), true
}
