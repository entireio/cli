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
	out := env.RunCLI("import", agentClaudeCode, "--dry-run")
	require.Contains(t, out, "Would import 2", "dry-run should count 2 turns; got: %s", out)
	require.NotContains(t, env.RunCLI("checkpoint", "list"), "[imported]", "dry-run must not write checkpoints")

	// Diverge the feature branch from the default branch before importing, so
	// the commit_sha assertion below can tell the default-branch tip apart
	// from HEAD (they are identical right after NewFeatureBranchEnv).
	env.WriteFile("diverge.txt", "x")
	env.GitAdd("diverge.txt")
	env.GitCommit("diverge feature branch")

	// 2. Real import writes the imported checkpoints (onto the v1 metadata branch).
	out = env.RunCLI("import", agentClaudeCode)
	require.Contains(t, out, "Imported 2", "got: %s", out)

	// 2b. Every imported checkpoint carries the default branch's tip as its
	//     commit_sha anchor (NOT the feature branch's HEAD). This pins the
	//     command-level wiring (import_cmd → resolveImportLinkCommitSHA →
	//     agentimport.Options): omitempty would silently hide a dropped wire.
	defaultTip := gitOutput(t, env.RepoDir, "rev-parse", "master")
	importedID := agentimport.DeriveCheckpointID(sessionID, "u1").String()
	sessionMD := gitOutput(t, env.RepoDir, "show", "entire/checkpoints/v1:"+SessionMetadataPath(importedID))
	require.Contains(t, sessionMD, `"commit_sha": "`+defaultTip+`"`,
		"imported session metadata should anchor to the default branch tip; got: %s", sessionMD)
	rootMD := gitOutput(t, env.RepoDir, "show", "entire/checkpoints/v1:"+CheckpointSummaryPath(importedID))
	require.Contains(t, rootMD, `"commit_sha": "`+defaultTip+`"`,
		"imported root summary should anchor to the default branch tip; got: %s", rootMD)

	// 3. checkpoint list surfaces imported entries labeled [imported], and does
	//    NOT duplicate them as [temporary] (regression: the imports were once
	//    mis-read by the shadow-branch scanner).
	listOut := env.RunCLI("checkpoint", "list")
	require.Contains(t, listOut, "[imported]", "checkpoint list should label imported checkpoints; got: %s", listOut)
	require.NotContains(t, listOut, "[temporary]", "imported checkpoints must not appear as temporary; got: %s", listOut)

	// 4. explain resolves an imported checkpoint by ID (regression: explain once
	//    only consulted the committed/shadow paths and missed imports).
	explainOut := env.RunCLI("checkpoint", "explain", importedID)
	require.Contains(t, explainOut, "first", "explain should show the imported turn's prompt; got: %s", explainOut)

	// 4b. --generate on an imported checkpoint is refused (read-only history).
	//     The guard runs on metadata alone, before any transcript content load.
	genOut, genErr := env.RunCLIWithError("checkpoint", "explain", importedID, "--generate")
	require.Error(t, genErr, "generate on imported checkpoint should fail")
	require.Contains(t, genOut, "imported history is read-only", "got: %s", genOut)

	// 5. Re-running import is idempotent.
	out = env.RunCLI("import", agentClaudeCode)
	require.Contains(t, out, "(2 already imported)", "re-run should skip already-imported turns; got: %s", out)
}
