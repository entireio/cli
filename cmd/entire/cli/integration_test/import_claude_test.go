//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agentimport"
)

func TestImportClaudeCode_EndToEnd(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	// Write a two-turn Claude transcript into the (overridden) Claude project dir.
	sessionID := "sess1"
	content := strings.Join([]string{
		`{"type":"user","uuid":"u1","timestamp":"2026-06-20T00:00:00Z","message":{"role":"user","content":"first"}}`,
		`{"type":"assistant","uuid":"a1","message":{"id":"m1","model":"claude-x","content":[{"type":"text","text":"ok"}],"usage":{"output_tokens":5}}}`,
		`{"type":"user","uuid":"u2","timestamp":"2026-06-20T00:01:00Z","message":{"role":"user","content":"second"}}`,
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(env.ClaudeProjectDir, sessionID+".jsonl"), []byte(content), 0o644))

	// 1. Dry-run reports counts and writes nothing.
	out := env.RunCLI("import", "claude-code", "--dry-run")
	require.Contains(t, out, "Would import 2", "dry-run should count 2 turns; got: %s", out)
	require.NotContains(t, env.RunCLI("checkpoint", "list"), "[imported]", "dry-run must not write checkpoints")

	// 2. Real import writes the imported checkpoints (onto the v1 metadata branch).
	out = env.RunCLI("import", "claude-code")
	require.Contains(t, out, "Imported 2", "got: %s", out)

	// 3. checkpoint list surfaces imported entries labeled [imported], and does
	//    NOT duplicate them as [temporary] (regression: the imports were once
	//    mis-read by the shadow-branch scanner).
	listOut := env.RunCLI("checkpoint", "list")
	require.Contains(t, listOut, "[imported]", "checkpoint list should label imported checkpoints; got: %s", listOut)
	require.NotContains(t, listOut, "[temporary]", "imported checkpoints must not appear as temporary; got: %s", listOut)

	// 4. explain resolves an imported checkpoint by ID (regression: explain once
	//    only consulted the committed/shadow paths and missed imports).
	importedID := agentimport.DeriveCheckpointID(sessionID, "u1").String()
	explainOut := env.RunCLI("checkpoint", "explain", importedID)
	require.Contains(t, explainOut, "first", "explain should show the imported turn's prompt; got: %s", explainOut)

	// 4b. --generate on an imported checkpoint is refused (read-only history).
	//     The guard runs on metadata alone, before any transcript content load.
	genOut, genErr := env.RunCLIWithError("checkpoint", "explain", importedID, "--generate")
	require.Error(t, genErr, "generate on imported checkpoint should fail")
	require.Contains(t, genOut, "imported history is read-only", "got: %s", genOut)

	// 5. Re-running import is idempotent.
	out = env.RunCLI("import", "claude-code")
	require.Contains(t, out, "(2 already imported)", "re-run should skip already-imported turns; got: %s", out)

	// 6. Rewinding to an imported checkpoint is refused with a clear message.
	rewindOut, rewindErr := env.RunCLIWithError("checkpoint", "rewind", "--to", importedID)
	require.Error(t, rewindErr, "rewind to imported checkpoint should fail")
	require.Contains(t, rewindOut, "read-only and not rewindable", "got: %s", rewindOut)
}
