//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/redact"
)

// History import is withdrawn, but checkpoints it already wrote stay readable.
// They are seeded here directly, the way import wrote them, since there is no
// command left that creates one: explain must still read one by ID, --generate
// must still refuse it, and neither list output may show it.
func TestImportedCheckpoints_StayReadableButUnlisted(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend
		const prompt = "seeded imported prompt"
		cpID := seedImportedCheckpoint(t, env, backend, prompt)

		explainOut := env.RunCLI("checkpoint", "explain", cpID)
		require.Contains(t, explainOut, prompt, "explain should read an existing imported checkpoint; got: %s", explainOut)

		genOut, genErr := env.RunCLIWithError("checkpoint", "explain", cpID, "--generate")
		require.Error(t, genErr, "--generate on an imported checkpoint should fail")
		require.Contains(t, genOut, "imported history is read-only", "got: %s", genOut)

		listOut := env.RunCLI("checkpoint", "list")
		require.NotContains(t, listOut, prompt, "checkpoint list must not show imported checkpoints; got: %s", listOut)

		var rows []map[string]any
		require.NoError(t, json.Unmarshal([]byte(env.RunCLI("checkpoint", "list", "--json")), &rows))
		for _, row := range rows {
			require.NotEqual(t, cpID, row["checkpoint_id"], "checkpoint list --json must not show imported checkpoints")
		}
	})
}

// seedImportedCheckpoint writes one read-only imported checkpoint in the
// backend under test and returns its ID. The backend is pinned in the repo's
// local settings and resolved from the repo root, not the test process's cwd.
func seedImportedCheckpoint(t *testing.T, env *TestEnv, backend, prompt string) string {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(env.RepoDir, ".entire", "settings.local.json"),
		[]byte(`{"checkpoints":{"primary":{"type":"`+backend+`"}}}`), 0o644))
	ctx := settings.WithWorktreeRoot(context.Background(), env.RepoDir)

	repo, err := gitrepo.OpenPath(env.RepoDir)
	require.NoError(t, err)
	defer repo.Close()
	stores, err := checkpoint.Open(ctx, repo, checkpoint.OpenOptions{})
	require.NoError(t, err)
	cpID, err := checkpoint.GenerateCheckpointID(ctx)
	require.NoError(t, err)

	transcript, err := redact.JSONLBytes([]byte(`{"type":"user","uuid":"u1","timestamp":"2026-06-20T00:00:00Z","message":{"role":"user","content":"` + prompt + `"}}` + "\n"))
	require.NoError(t, err)
	require.NoError(t, stores.Persistent.Write(ctx, checkpoint.Session(checkpoint.WriteOptions{
		CheckpointID:     cpID,
		SessionID:        "imported-session",
		CreatedAt:        time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC),
		Strategy:         "import",
		Kind:             string(session.KindImported),
		Agent:            agentClaudeCode,
		Transcript:       transcript,
		Prompts:          []string{prompt},
		CheckpointsCount: 1,
	})))
	return cpID.String()
}
