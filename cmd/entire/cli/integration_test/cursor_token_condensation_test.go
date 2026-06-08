//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// runCursorHook drives the real `entire hooks cursor <hookName>` binary with a
// JSON payload on stdin, mirroring how Cursor IDE invokes the hooks. There is no
// Cursor HookRunner in the shared harness yet, so this is defined locally.
func runCursorHook(t *testing.T, env *TestEnv, cursorProjectDir, hookName string, input map[string]any) {
	t.Helper()

	inputJSON, err := json.Marshal(input)
	require.NoError(t, err)

	cmd := exec.Command(getTestBinary(), "hooks", "cursor", hookName)
	cmd.Dir = env.RepoDir
	cmd.Stdin = bytes.NewReader(inputJSON)
	cmd.Env = append(testutil.GitIsolatedEnv(),
		"ENTIRE_TEST_CURSOR_PROJECT_DIR="+cursorProjectDir,
	)

	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "cursor %s hook failed\ninput: %s\noutput: %s", hookName, inputJSON, out)
	t.Logf("cursor %s output: %s", hookName, out)
}

// TestCursorTokenUsage_SurvivesCondensation reproduces the gap introduced
// alongside PR #1263. The Cursor `stop` hook is the only authoritative source
// of token accounting for Cursor sessions (the JSONL transcript carries no
// usage fields), and the PR correctly threads those tokens through
// handleLifecycleTurnEnd into the *live* session state.
//
// But at commit time, condensation recomputes TokenUsage purely from the
// transcript (manual_commit_condensation.go: extractSessionData* →
// agent.CalculateTokenUsage). Cursor implements no TokenCalculator, so that
// recompute yields nil and the hook-provided tokens are dropped from the
// committed checkpoint metadata.
//
// This test asserts the DESIRED end state — Cursor's hook tokens should be
// present in the committed checkpoint. It is expected to FAIL on PR #1263 as
// written, demonstrating the gap, and should pass once condensation stops
// clobbering an already-populated TokenUsage with a transcript recompute that
// the agent cannot satisfy.
//
// Contrast with Copilot CLI, which does NOT hit this gap: Copilot implements
// agent.TokenCalculator and writes its usage (session.shutdown) into the JSONL
// transcript, so the condensation-time recompute recovers it from the
// transcript. The Copilot-specific branch in sessionStateBackfillTokenUsage
// only repairs the *session-state* (full-session) total for `entire status`;
// it does not feed committed checkpoint metadata. Cursor's tokens never reach
// the transcript at all, so the same recompute can only return nil.
func TestCursorTokenUsage_SurvivesCondensation(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.InitEntireWithAgent(agent.AgentNameCursor)

	// Cursor stores transcripts outside the repo; point the agent at a temp dir.
	cursorProjectDir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(cursorProjectDir); err == nil {
		cursorProjectDir = resolved
	}

	const conversationID = "cursor-tok-session"

	// Cursor IDE nested layout: <dir>/<id>/<id>.jsonl. The transcript has NO
	// usage fields — exactly why the stop hook is the only token source.
	transcriptDir := filepath.Join(cursorProjectDir, conversationID)
	require.NoError(t, os.MkdirAll(transcriptDir, 0o755))
	transcriptPath := filepath.Join(transcriptDir, conversationID+".jsonl")
	require.NoError(t, os.WriteFile(transcriptPath,
		[]byte(`{"type":"user","text":"add a feature"}`+"\n"+
			`{"type":"assistant","text":"done"}`+"\n"), 0o600))

	// 1. session-start — registers the session as owned by Cursor.
	runCursorHook(t, env, cursorProjectDir, "session-start", map[string]any{
		"conversation_id": conversationID,
		"transcript_path": transcriptPath,
		"model":           "cursor-default",
	})

	// 2. before-submit-prompt — captures pre-turn state (untracked files).
	runCursorHook(t, env, cursorProjectDir, "before-submit-prompt", map[string]any{
		"conversation_id": conversationID,
		"transcript_path": transcriptPath,
		"prompt":          "add a feature",
	})

	// 3. Agent edits a file AFTER turn-start so it is detected as a change.
	env.WriteFile("feature.go", "package main\n// new feature\n")

	// 4. stop — carries per-turn token usage. fresh input = 5000-4000-800 = 200.
	runCursorHook(t, env, cursorProjectDir, "stop", map[string]any{
		"conversation_id":    conversationID,
		"transcript_path":    transcriptPath,
		"model":              "cursor-default",
		"loop_count":         1,
		"input_tokens":       5000,
		"output_tokens":      50,
		"cache_read_tokens":  4000,
		"cache_write_tokens": 800,
	})

	// Sanity check: the PR's live-layer feature works — the hook tokens land in
	// session state (this is what `entire status` reads). If this fails, the
	// test setup is wrong, not the condensation gap.
	statePath := filepath.Join(env.RepoDir, ".git", "entire-sessions", conversationID+".json")
	stateBytes, err := os.ReadFile(statePath)
	require.NoError(t, err, "session state file should exist after stop")

	var liveState strategy.SessionState
	require.NoError(t, json.Unmarshal(stateBytes, &liveState))
	require.NotNil(t, liveState.TokenUsage, "PRECONDITION: stop hook tokens must reach live session state")
	require.Equal(t, 200, liveState.TokenUsage.InputTokens, "fresh input = 5000-4000-800")
	require.Equal(t, 50, liveState.TokenUsage.OutputTokens)
	require.Equal(t, 4000, liveState.TokenUsage.CacheReadTokens)
	require.Equal(t, 800, liveState.TokenUsage.CacheCreationTokens)

	// 5. User commits — triggers condensation into the metadata branch.
	env.GitCommitWithShadowHooks("Add feature", "feature.go")

	// 6. A checkpoint must have been condensed.
	checkpointID := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, checkpointID, "expected a condensed checkpoint after commit")

	// 7. THE GAP: the committed session metadata should carry the Cursor tokens.
	metadataPath := SessionMetadataPath(checkpointID)
	content, found := env.ReadFileFromBranch(paths.MetadataBranchName, metadataPath)
	require.True(t, found, "session metadata should exist at %s", metadataPath)

	var meta checkpoint.CommittedMetadata
	require.NoError(t, json.Unmarshal([]byte(content), &meta))

	require.NotNilf(t, meta.TokenUsage,
		"committed checkpoint metadata dropped Cursor's hook-provided token usage "+
			"(condensation recomputed TokenUsage from a transcript Cursor never populates)\nmetadata: %s",
		content)
	require.Equal(t, 200, meta.TokenUsage.InputTokens, "committed InputTokens must match the stop hook")
	require.Equal(t, 50, meta.TokenUsage.OutputTokens, "committed OutputTokens must match the stop hook")
	require.Equal(t, 4000, meta.TokenUsage.CacheReadTokens)
	require.Equal(t, 800, meta.TokenUsage.CacheCreationTokens)
}

// TestCursorTokenUsage_PerCheckpointScoping guards against the obvious-but-wrong
// fix for the gap in TestCursorTokenUsage_SurvivesCondensation: "just reuse
// state.TokenUsage when the transcript recompute is empty."
//
// Committed checkpoint metadata is scoped per checkpoint (the transcript-based
// path computes usage from CheckpointTranscriptStart), but the Cursor hook
// tokens accumulate into a session-wide running total in state.TokenUsage that
// condensation deliberately does NOT reset (manual_commit_condensation.go resets
// StepCount and CheckpointTranscriptStart but leaves TokenUsage accumulating).
//
// So in a two-commit session:
//   - checkpoint 1 should carry turn 1's tokens only
//   - checkpoint 2 should carry turn 2's tokens only (NOT turn1+turn2)
//
// A fix that stamps the running session total onto every checkpoint would make
// checkpoint 2 report the cumulative total — this test fails in that case,
// forcing a per-checkpoint-scoped fix. On PR #1263 as written it fails earlier,
// at checkpoint 1, because committed token usage is nil.
func TestCursorTokenUsage_PerCheckpointScoping(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.InitEntireWithAgent(agent.AgentNameCursor)

	cursorProjectDir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(cursorProjectDir); err == nil {
		cursorProjectDir = resolved
	}

	const conversationID = "cursor-scope-session"

	transcriptDir := filepath.Join(cursorProjectDir, conversationID)
	require.NoError(t, os.MkdirAll(transcriptDir, 0o755))
	transcriptPath := filepath.Join(transcriptDir, conversationID+".jsonl")

	// appendTranscript grows the transcript so each turn advances
	// CheckpointTranscriptStart, mirroring a real multi-turn session.
	appendTranscript := func(lines string) {
		t.Helper()
		f, err := os.OpenFile(transcriptPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		require.NoError(t, err)
		_, werr := f.WriteString(lines)
		require.NoError(t, f.Close())
		require.NoError(t, werr)
	}

	appendTranscript(`{"type":"user","text":"turn one"}` + "\n" + `{"type":"assistant","text":"ok"}` + "\n")

	runCursorHook(t, env, cursorProjectDir, "session-start", map[string]any{
		"conversation_id": conversationID,
		"transcript_path": transcriptPath,
		"model":           "cursor-default",
	})

	// --- Turn 1: fresh input = 5000-4000-800 = 200, output 50 ---
	runCursorHook(t, env, cursorProjectDir, "before-submit-prompt", map[string]any{
		"conversation_id": conversationID,
		"transcript_path": transcriptPath,
		"prompt":          "turn one",
	})
	env.WriteFile("turn1.go", "package main\n// turn 1\n")
	runCursorHook(t, env, cursorProjectDir, "stop", map[string]any{
		"conversation_id":    conversationID,
		"transcript_path":    transcriptPath,
		"model":              "cursor-default",
		"loop_count":         1,
		"input_tokens":       5000,
		"output_tokens":      50,
		"cache_read_tokens":  4000,
		"cache_write_tokens": 800,
	})
	env.GitCommitWithShadowHooks("Turn 1", "turn1.go")
	checkpoint1 := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, checkpoint1, "expected a checkpoint after turn 1 commit")

	// --- Turn 2: fresh input = 3000-2000-500 = 500, output 30 ---
	// Cumulative session total after this turn would be input 700, output 80 —
	// the wrong answer a naive fix would attribute to checkpoint 2.
	appendTranscript(`{"type":"user","text":"turn two"}` + "\n" + `{"type":"assistant","text":"ok"}` + "\n")
	runCursorHook(t, env, cursorProjectDir, "before-submit-prompt", map[string]any{
		"conversation_id": conversationID,
		"transcript_path": transcriptPath,
		"prompt":          "turn two",
	})
	env.WriteFile("turn2.go", "package main\n// turn 2\n")
	runCursorHook(t, env, cursorProjectDir, "stop", map[string]any{
		"conversation_id":    conversationID,
		"transcript_path":    transcriptPath,
		"model":              "cursor-default",
		"loop_count":         1,
		"input_tokens":       3000,
		"output_tokens":      30,
		"cache_read_tokens":  2000,
		"cache_write_tokens": 500,
	})
	env.GitCommitWithShadowHooks("Turn 2", "turn2.go")
	checkpoint2 := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, checkpoint2, "expected a checkpoint after turn 2 commit")
	require.NotEqual(t, checkpoint1, checkpoint2, "turn 2 must produce a distinct checkpoint")

	// Checkpoint 1 carries only turn 1's tokens.
	cp1 := readCommittedTokenUsage(t, env, checkpoint1)
	require.NotNil(t, cp1, "checkpoint 1 must carry turn 1 token usage")
	require.Equal(t, 200, cp1.InputTokens, "checkpoint 1 InputTokens = turn 1 only")
	require.Equal(t, 50, cp1.OutputTokens, "checkpoint 1 OutputTokens = turn 1 only")

	// Checkpoint 2 carries only turn 2's tokens — NOT the cumulative 700/80.
	cp2 := readCommittedTokenUsage(t, env, checkpoint2)
	require.NotNil(t, cp2, "checkpoint 2 must carry turn 2 token usage")
	require.Equal(t, 500, cp2.InputTokens,
		"checkpoint 2 InputTokens must be turn 2 only (500), not the cumulative session total (700)")
	require.Equal(t, 30, cp2.OutputTokens,
		"checkpoint 2 OutputTokens must be turn 2 only (30), not the cumulative session total (80)")
}

// readCommittedTokenUsage reads the committed CommittedMetadata for a checkpoint
// from the metadata branch and returns its TokenUsage (nil if absent).
func readCommittedTokenUsage(t *testing.T, env *TestEnv, checkpointID string) *agent.TokenUsage {
	t.Helper()
	content, found := env.ReadFileFromBranch(paths.MetadataBranchName, SessionMetadataPath(checkpointID))
	require.Truef(t, found, "session metadata should exist for checkpoint %s", checkpointID)
	var meta checkpoint.CommittedMetadata
	require.NoError(t, json.Unmarshal([]byte(content), &meta))
	return meta.TokenUsage
}
