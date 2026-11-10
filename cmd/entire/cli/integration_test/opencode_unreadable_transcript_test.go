//go:build integration

package integration

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// An OpenCode whose transcript can never be exported (for example, the
// `opencode` binary is not on the hook's PATH) must not leave commits whose
// Entire-Checkpoint trailer names a checkpoint that is never written.
// The mock export file is never written here, so every export fails as it
// does when the binary is missing.
func TestOpenCodeUnreadableTranscript_CommitsKeepTheirCheckpoints(t *testing.T) {
	t.Parallel()

	t.Run("mid-turn commit", func(t *testing.T) {
		t.Parallel()
		env := NewFeatureBranchEnv(t)
		env.InitEntireWithAgent(agent.AgentNameOpenCode)
		session := env.NewOpenCodeSession()
		if err := env.SimulateOpenCodeSessionStart(session.ID, ""); err != nil {
			t.Fatal(err)
		}
		if err := env.SimulateOpenCodeTurnStart(session.ID, "", "Add a script and commit it"); err != nil {
			t.Fatal(err)
		}
		env.WriteFile("script.sh", "#!/bin/sh\necho hello\n")
		env.GitCommitWithHooksAsAgent("Add script", "script.sh")
		assertTrailerCheckpointWritten(t, env)
	})

	t.Run("commit after a failed turn end", func(t *testing.T) {
		t.Parallel()
		env := NewFeatureBranchEnv(t)
		env.InitEntireWithAgent(agent.AgentNameOpenCode)
		session := env.NewOpenCodeSession()
		if err := env.SimulateOpenCodeSessionStart(session.ID, ""); err != nil {
			t.Fatal(err)
		}
		if err := env.SimulateOpenCodeTurnStart(session.ID, "", "Add a script"); err != nil {
			t.Fatal(err)
		}
		env.WriteFile("script.sh", "#!/bin/sh\necho hello\n")
		// The turn end cannot export the transcript; its error is the bug's
		// precondition, not the assertion.
		_ = env.SimulateOpenCodeTurnEnd(session.ID, "") //nolint:errcheck // expected to fail
		env.GitCommitWithHooksAsAgent("Add script", "script.sh")
		assertTrailerCheckpointWritten(t, env)
	})
}

func assertTrailerCheckpointWritten(t *testing.T, env *TestEnv) {
	t.Helper()
	checkpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if checkpointID == "" {
		t.Skip("commit was not stamped; nothing can dangle")
	}
	if _, ok := checkpointBlob(env, checkpointID, paths.MetadataFileName); !ok {
		t.Fatalf("commit trailer names checkpoint %s, which was never written", checkpointID)
	}
	metadata, ok := checkpointBlob(env, checkpointID, "0/"+paths.MetadataFileName)
	if !ok || !strings.Contains(metadata, "script.sh") {
		t.Fatalf("checkpoint %s does not record the committed file: %s", checkpointID, metadata)
	}
}
