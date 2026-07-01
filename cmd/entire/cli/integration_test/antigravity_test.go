//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAntigravity_FullEventFlow drives the five Antigravity hooks
// (pre-invocation, pre-tool-use, post-tool-use, post-invocation, stop) against
// a real git repo via `entire hooks antigravity <verb>` subprocesses and
// verifies the lifecycle wiring: session state lazy-initializes on the first
// PreInvocation, file changes from a write_to_file PreToolUse land in
// state.FilesTouched, and a Stop with fullyIdle=true records SessionEnd.
func TestAntigravity_FullEventFlow(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	conversationID := "antigravity-it-conv-id"
	transcriptPath := filepath.Join(env.RepoDir, ".gemini", "antigravity-cli",
		"brain", conversationID, ".system_generated", "logs", "transcript.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(transcriptPath), 0o750))
	require.NoError(t, os.WriteFile(transcriptPath,
		[]byte(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"create foo.txt"}`+"\n"),
		0o600))

	common := map[string]any{
		"conversationId":        conversationID,
		"workspacePaths":        []string{env.RepoDir},
		"transcriptPath":        transcriptPath,
		"artifactDirectoryPath": filepath.Join(env.RepoDir, ".gemini", "antigravity-cli", "artifacts"),
	}

	// agy 1.0.0 wire format: invocationNum is 0-indexed. The first model
	// invocation of a conversation carries invocationNum=0; only that one
	// is mapped to TurnStart by parsePreInvocation. initialNumSteps=1 reflects
	// the user prompt being inserted as step 0 before the first model call —
	// values mirror real captured agy stdin.
	preInv := mergeMaps(common, map[string]any{
		"invocationNum":   0,
		"initialNumSteps": 1,
	})
	require.NoError(t, runAntigravityHook(t, env.RepoDir, "pre-invocation", preInv),
		"pre-invocation hook should succeed and lazy-init session state")

	statePath := filepath.Join(env.RepoDir, ".git", "entire-sessions", conversationID+".json")
	require.FileExists(t, statePath, "session state file should exist after pre-invocation")

	preTU := mergeMaps(common, map[string]any{
		"toolCall": map[string]any{
			"name": "write_to_file",
			"args": map[string]any{
				"TargetFile": "foo.txt",
				"Overwrite":  false,
			},
		},
		"stepIdx": 1,
	})
	require.NoError(t, runAntigravityHook(t, env.RepoDir, "pre-tool-use", preTU),
		"pre-tool-use hook should record the new file in state.FilesTouched")

	postTU := mergeMaps(common, map[string]any{"stepIdx": 1})
	require.NoError(t, runAntigravityHook(t, env.RepoDir, "post-tool-use", postTU),
		"post-tool-use hook should be a no-op")

	postInv := mergeMaps(common, map[string]any{
		"invocationNum":   1,
		"initialNumSteps": 2,
	})
	require.NoError(t, runAntigravityHook(t, env.RepoDir, "post-invocation", postInv),
		"post-invocation hook should be a no-op (Antigravity writes its transcript after Stop, so emitting TurnEnd here would fail with transcript-not-found)")

	stopBackgroundActive := mergeMaps(common, map[string]any{
		"executionNum":      1,
		"terminationReason": "model_stop",
		"error":             "",
		"fullyIdle":         false,
	})
	require.NoError(t, runAntigravityHook(t, env.RepoDir, "stop", stopBackgroundActive),
		"stop hook with fullyIdle=false must not finalize the session")

	stopIdle := mergeMaps(common, map[string]any{
		"executionNum":      1,
		"terminationReason": "model_stop",
		"error":             "",
		"fullyIdle":         true,
	})
	require.NoError(t, runAntigravityHook(t, env.RepoDir, "stop", stopIdle),
		"stop hook with fullyIdle=true should emit SessionEnd")

	stateBytes, err := os.ReadFile(statePath)
	require.NoError(t, err, "session state file should still be readable after the full flow")
	var state map[string]any
	require.NoError(t, json.Unmarshal(stateBytes, &state))

	if sid, ok := state["session_id"].(string); !ok || sid != conversationID {
		t.Errorf("session_id = %v, want %q", state["session_id"], conversationID)
	}

	rawFiles, _ := state["files_touched"].([]any)
	got := make([]string, 0, len(rawFiles))
	for _, v := range rawFiles {
		if s, ok := v.(string); ok {
			got = append(got, s)
		}
	}
	assert.True(t, slices.Contains(got, "foo.txt"),
		"PreToolUse(write_to_file foo.txt) should have populated files_touched; got %v", got)
}

func TestAntigravity_StopAfterAgentCommitDoesNotCreateNewShadowBranch(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	conversationID := "antigravity-it-agent-commit"
	transcriptPath := filepath.Join(env.RepoDir, ".gemini", "antigravity-cli",
		"brain", conversationID, ".system_generated", "logs", "transcript_full.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(transcriptPath), 0o750))
	require.NoError(t, os.WriteFile(transcriptPath,
		[]byte(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"create docs/blue.md and commit it"}`+"\n"),
		0o600))

	common := map[string]any{
		"conversationId":        conversationID,
		"workspacePaths":        []string{env.RepoDir},
		"transcriptPath":        transcriptPath,
		"artifactDirectoryPath": filepath.Join(env.RepoDir, ".gemini", "antigravity-cli", "artifacts"),
	}

	preInv := mergeMaps(common, map[string]any{
		"invocationNum":   0,
		"initialNumSteps": 1,
	})
	require.NoError(t, runAntigravityHook(t, env.RepoDir, "pre-invocation", preInv))

	preTU := mergeMaps(common, map[string]any{
		"toolCall": map[string]any{
			"name": "write_to_file",
			"args": map[string]any{
				"TargetFile": "docs/blue.md",
				"Overwrite":  false,
			},
		},
		"stepIdx": 1,
	})
	require.NoError(t, runAntigravityHook(t, env.RepoDir, "pre-tool-use", preTU))

	env.WriteFile("docs/blue.md", "Blue is a calm colour.\n")
	gitCLICommitWithEntireHooks(t, env, "Add blue docs", "docs/blue.md")

	require.NoError(t, runAntigravityHook(t, env.RepoDir, "stop", mergeMaps(common, map[string]any{
		"executionNum":      1,
		"terminationReason": "model_stop",
		"error":             "",
		"fullyIdle":         true,
	})))

	for _, branch := range env.ListBranchesWithPrefix("entire/") {
		if branch == paths.MetadataBranchName || branch == paths.TrailsBranchName {
			continue
		}
		t.Fatalf("unexpected shadow branch after agent commit and stop: %s", branch)
	}
}

func gitCLICommitWithEntireHooks(t *testing.T, env *TestEnv, message string, files ...string) {
	t.Helper()
	for _, file := range files {
		cmd := exec.Command("git", "add", "--", file)
		cmd.Dir = env.RepoDir
		cmd.Env = env.gitHookEnv()
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git add %s failed: %v\nOutput: %s", file, err, output)
		}
	}

	msgFile := filepath.Join(env.RepoDir, ".git", "COMMIT_EDITMSG")
	require.NoError(t, os.WriteFile(msgFile, []byte(message), 0o600))
	if output, err := env.prepareCommitMsgCmd(false, msgFile, "message").CombinedOutput(); err != nil {
		t.Fatalf("prepare-commit-msg failed: %v\nOutput: %s", err, output)
	}

	commitCmd := exec.Command("git", "commit", "-F", msgFile)
	commitCmd.Dir = env.RepoDir
	commitCmd.Env = env.gitHookEnv()
	if output, err := commitCmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit failed: %v\nOutput: %s", err, output)
	}

	postCmd := exec.Command(getTestBinary(), "hooks", "git", "post-commit")
	postCmd.Dir = env.RepoDir
	postCmd.Env = env.gitHookEnv()
	if output, err := postCmd.CombinedOutput(); err != nil {
		t.Fatalf("post-commit failed: %v\nOutput: %s", err, output)
	}
}

// TestAntigravity_TokenUsageInCheckpointMetadata proves the entire.io UI
// contract end-to-end: agy's out-of-band token counts (the title-script
// context_window, captured into the snapshot store) flow through TurnStart
// baseline → TurnEnd delta → SaveStep → SessionState → condensation → the
// committed checkpoint metadata on entire/checkpoints/v1.
//
// The snapshot totals are cumulative per conversation, so the per-turn delta is
// (latest − baseline). Baseline here is 1000/100 (input/output) captured at
// TurnStart; the latest before Stop is 4500/400 → expected delta 3500/300.
func TestAntigravity_TokenUsageInCheckpointMetadata(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	statusDir := t.TempDir()
	configDir := t.TempDir()
	conversationID := "antigravity-tokens-it"
	extraEnv := []string{
		"ENTIRE_ANTIGRAVITY_STATUS_DIR=" + statusDir,
		"ENTIRE_ANTIGRAVITY_CONFIG_DIR=" + configDir,
	}

	snapshotPath := filepath.Join(statusDir, conversationID+".jsonl")
	appendSnapshotLine := func(line string) {
		f, err := os.OpenFile(snapshotPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		require.NoError(t, err)
		_, werr := f.WriteString(line + "\n")
		require.NoError(t, f.Close())
		require.NoError(t, werr)
	}

	transcriptPath := filepath.Join(env.RepoDir, ".gemini", "antigravity-cli",
		"brain", conversationID, ".system_generated", "logs", "transcript.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(transcriptPath), 0o750))
	require.NoError(t, os.WriteFile(transcriptPath,
		[]byte(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"create tok.txt"}`+"\n"),
		0o600))

	common := map[string]any{
		"conversationId":        conversationID,
		"workspacePaths":        []string{env.RepoDir},
		"transcriptPath":        transcriptPath,
		"artifactDirectoryPath": filepath.Join(env.RepoDir, ".gemini", "antigravity-cli", "artifacts"),
	}

	// 1. Seed the BASELINE snapshot before TurnStart captures it.
	appendSnapshotLine(`{"ts":"2026-06-03T10:00:00Z","conversation_id":"` + conversationID +
		`","context_window":{"total_input_tokens":1000,"total_output_tokens":100,` +
		`"current_usage":{"cache_read_input_tokens":700}}}`)

	// 2. TurnStart (invocationNum 0) → baseline captured = 1000/100.
	preInv := mergeMaps(common, map[string]any{
		"invocationNum":   0,
		"initialNumSteps": 1,
	})
	require.NoError(t, runAntigravityHookWithEnv(t, env.RepoDir, "pre-invocation", preInv, extraEnv),
		"pre-invocation should capture the token baseline")

	// 3. PreToolUse writing a real file, so the turn has a working-tree change
	//    to checkpoint and condense.
	env.WriteFile("tok.txt", "token test contents\n")
	preTU := mergeMaps(common, map[string]any{
		"toolCall": map[string]any{
			"name": "write_to_file",
			"args": map[string]any{"TargetFile": "tok.txt", "Overwrite": false},
		},
		"stepIdx": 1,
	})
	require.NoError(t, runAntigravityHookWithEnv(t, env.RepoDir, "pre-tool-use", preTU, extraEnv),
		"pre-tool-use should record tok.txt in files_touched")

	// 4. Append the SECOND (latest) snapshot: cumulative totals grew.
	appendSnapshotLine(`{"ts":"2026-06-03T10:05:00Z","conversation_id":"` + conversationID +
		`","context_window":{"total_input_tokens":4500,"total_output_tokens":400,` +
		`"current_usage":{"cache_read_input_tokens":1500,"cache_creation_input_tokens":50}}}`)

	// 5. Stop (fullyIdle true) → TurnEnd → OOB delta 3500/300 → SaveStep.
	stopIdle := mergeMaps(common, map[string]any{
		"executionNum":      1,
		"terminationReason": "model_stop",
		"error":             "",
		"fullyIdle":         true,
	})
	require.NoError(t, runAntigravityHookWithEnv(t, env.RepoDir, "stop", stopIdle, extraEnv),
		"stop hook should finalize the turn and accumulate token usage")

	// Sanity: SessionState shows the accumulated delta before commit. This
	// isolates the OOB capture+delta+SaveStep path from condensation.
	statePath := filepath.Join(env.RepoDir, ".git", "entire-sessions", conversationID+".json")
	stateBytes, err := os.ReadFile(statePath)
	require.NoError(t, err)
	var state struct {
		TokenUsage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"token_usage"`
	}
	require.NoError(t, json.Unmarshal(stateBytes, &state))
	require.NotNil(t, state.TokenUsage, "session state should record token usage after stop")
	assert.Equal(t, 3500, state.TokenUsage.InputTokens, "session state input tokens")
	assert.Equal(t, 300, state.TokenUsage.OutputTokens, "session state output tokens")

	// 6. Commit: prepare-commit-msg adds the Entire-Checkpoint trailer, then
	//    post-commit condenses the session (with its token usage) onto
	//    entire/checkpoints/v1.
	env.GitCommitWithShadowHooks("Add tok.txt", "tok.txt")

	// 7. Read the committed checkpoint metadata and assert the token usage
	//    reached both the per-session metadata.json and the summary aggregate.
	checkpointID := env.GetLatestCheckpointID()

	sessionMeta, found := env.ReadFileFromBranch(paths.MetadataBranchName, SessionMetadataPath(checkpointID))
	require.True(t, found, "per-session metadata.json should exist on the metadata branch")
	var sessionMetadata checkpoint.CommittedMetadata
	require.NoError(t, json.Unmarshal([]byte(sessionMeta), &sessionMetadata))
	require.NotNil(t, sessionMetadata.TokenUsage, "committed per-session metadata should carry token_usage")
	assert.Equal(t, 3500, sessionMetadata.TokenUsage.InputTokens, "committed per-session input tokens")
	assert.Equal(t, 300, sessionMetadata.TokenUsage.OutputTokens, "committed per-session output tokens")

	summaryRaw, found := env.ReadFileFromBranch(paths.MetadataBranchName, CheckpointSummaryPath(checkpointID))
	require.True(t, found, "CheckpointSummary metadata.json should exist on the metadata branch")
	var summary checkpoint.CheckpointSummary
	require.NoError(t, json.Unmarshal([]byte(summaryRaw), &summary))
	require.NotNil(t, summary.TokenUsage, "CheckpointSummary should carry aggregated token_usage")
	assert.Equal(t, 3500, summary.TokenUsage.InputTokens, "summary aggregate input tokens")
	assert.Equal(t, 300, summary.TokenUsage.OutputTokens, "summary aggregate output tokens")
}

// TestAntigravity_PromptInCheckpointMetadata proves the condensation-time
// late-flush prompt fallback end-to-end for Antigravity (agy).
//
// Background: agy writes its JSONL transcript AFTER the Stop hook fires, so the
// TurnEnd prompt backfill (lifecycle.go) reads an EMPTY transcript and prompt.txt
// stays empty. The committed "prompt" field would therefore be empty. The
// condensation-time fallback (resolvePromptsFromLateFlushedTranscript) re-extracts
// the prompt from the live transcript at `git commit` time — by then agy has
// finished its asynchronous write, so the transcript is populated.
//
// The test reproduces that exact timing:
//  1. TurnStart (PreInvocation, invocationNum 0) — transcript file is EMPTY.
//  2. PreToolUse write_to_file touches foo.txt; the file is written to the worktree.
//  3. Stop (fullyIdle true) — SaveStep creates the shadow checkpoint while the
//     transcript is STILL EMPTY (proving the backfill cannot recover the prompt).
//  4. The transcript is populated with a USER_INPUT/<USER_REQUEST> step BEFORE the
//     git commit (simulating agy finishing its late write).
//  5. git commit → PostCommit condensation → the committed session metadata's
//     prompt is recovered from the now-populated transcript.
func TestAntigravity_PromptInCheckpointMetadata(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	env.InitEntire()

	const requestText = "add a foo.txt file with the word bar in it"

	conversationID := "antigravity-it-prompt-conv-id"
	transcriptPath := filepath.Join(env.RepoDir, ".gemini", "antigravity-cli",
		"brain", conversationID, ".system_generated", "logs", "transcript.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(transcriptPath), 0o750))

	// CRUX: transcript is EMPTY at TurnStart and TurnEnd. agy writes it after Stop.
	require.NoError(t, os.WriteFile(transcriptPath, []byte{}, 0o600),
		"transcript must start empty to simulate agy's late flush")

	common := map[string]any{
		"conversationId":        conversationID,
		"workspacePaths":        []string{env.RepoDir},
		"transcriptPath":        transcriptPath,
		"artifactDirectoryPath": filepath.Join(env.RepoDir, ".gemini", "antigravity-cli", "artifacts"),
	}

	// TurnStart: first model invocation of the conversation (invocationNum=0).
	preInv := mergeMaps(common, map[string]any{
		"invocationNum":   0,
		"initialNumSteps": 1,
	})
	require.NoError(t, runAntigravityHook(t, env.RepoDir, "pre-invocation", preInv),
		"pre-invocation hook should succeed and lazy-init session state")

	statePath := filepath.Join(env.RepoDir, ".git", "entire-sessions", conversationID+".json")
	require.FileExists(t, statePath, "session state file should exist after pre-invocation")

	// PreToolUse write_to_file: records foo.txt in state.FilesTouched.
	preTU := mergeMaps(common, map[string]any{
		"toolCall": map[string]any{
			"name": "write_to_file",
			"args": map[string]any{
				"TargetFile": "foo.txt",
				"Overwrite":  false,
			},
		},
		"stepIdx": 1,
	})
	require.NoError(t, runAntigravityHook(t, env.RepoDir, "pre-tool-use", preTU),
		"pre-tool-use hook should record the new file in state.FilesTouched")

	// The agent actually writes the file to the worktree so there is a diff to
	// checkpoint and later commit.
	env.WriteFile("foo.txt", "bar\n")

	// Stop (fullyIdle=true): TurnEnd → SaveStep creates the shadow checkpoint.
	// The transcript is STILL EMPTY here, so the TurnEnd prompt backfill recovers
	// nothing and prompt.txt is empty. This is what makes the test exercise the
	// condensation-time fallback rather than the backfill path.
	stopIdle := mergeMaps(common, map[string]any{
		"executionNum":      1,
		"terminationReason": "model_stop",
		"error":             "",
		"fullyIdle":         true,
	})
	require.NoError(t, runAntigravityHook(t, env.RepoDir, "stop", stopIdle),
		"stop hook with fullyIdle=true should emit SessionEnd and SaveStep the checkpoint")

	// LATE FLUSH: agy finishes writing the transcript AFTER Stop, BEFORE the commit.
	// Now the live transcript carries the USER_INPUT step with the <USER_REQUEST>.
	step := map[string]any{
		"step_index": 0,
		"source":     "USER_EXPLICIT",
		"type":       "USER_INPUT",
		"status":     "DONE",
		"content":    "<USER_REQUEST>\n" + requestText + "\n</USER_REQUEST>",
	}
	stepJSON, err := json.Marshal(step)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(transcriptPath, append(stepJSON, '\n'), 0o600),
		"populate the transcript before commit to simulate agy's completed late write")

	// Commit → PostCommit condensation. The fallback re-extracts the prompt from
	// the now-populated live transcript.
	env.GitCommitWithShadowHooks("Add foo.txt", "foo.txt")

	// Resolve the checkpoint ID from the user commit's Entire-Checkpoint trailer.
	headHash := env.GetHeadHash()
	repo, err := git.PlainOpen(env.RepoDir)
	require.NoError(t, err)
	commitObj, err := repo.CommitObject(plumbing.NewHash(headHash))
	require.NoError(t, err)
	checkpointID, found := trailers.ParseCheckpoint(commitObj.Message)
	require.True(t, found, "user commit should carry an Entire-Checkpoint trailer")

	// Sanity-check that the checkpoint's session metadata.json was written (the
	// checkpoint exists and is sharded under the checkpoint ID).
	metadataPath := SessionMetadataPath(checkpointID.String())
	metadataContent, found := env.ReadFileFromBranch(paths.MetadataBranchName, metadataPath)
	require.True(t, found, "session metadata.json should exist at %s", metadataPath)
	var metadata checkpoint.CommittedMetadata
	require.NoError(t, json.Unmarshal([]byte(metadataContent), &metadata),
		"session metadata.json should parse")
	require.Equal(t, conversationID, metadata.SessionID,
		"checkpoint should be linked to the antigravity conversation/session")

	// PRIMARY ASSERTION: the committed session-level "prompt" (prompt.txt on
	// entire/checkpoints/v1) was recovered by the condensation-time late-flush
	// fallback. The transcript was empty at TurnEnd (so the backfill recovered
	// nothing) and only populated before commit, so a non-empty prompt here can
	// ONLY have come from resolvePromptsFromLateFlushedTranscript at condensation.
	promptPath := SessionFilePath(checkpointID.String(), paths.PromptFileName)
	promptContent, found := env.ReadFileFromBranch(paths.MetadataBranchName, promptPath)
	require.True(t, found, "prompt.txt should exist at %s", promptPath)
	require.NotEmpty(t, strings.TrimSpace(promptContent),
		"committed prompt.txt must be non-empty — the condensation-time late-flush "+
			"fallback should have recovered it from the populated transcript")
	assert.Equal(t, requestText, strings.TrimSpace(promptContent),
		"committed prompt should equal the <USER_REQUEST> text from the late-flushed transcript")
}

func runAntigravityHook(t *testing.T, repoDir, hookName string, input map[string]any) error {
	t.Helper()
	return runAntigravityHookWithEnv(t, repoDir, hookName, input, nil)
}

// runAntigravityHookWithEnv runs an Antigravity hook subprocess with extraEnv
// appended to the isolated git env. The override (e.g.
// ENTIRE_ANTIGRAVITY_STATUS_DIR) must be on the child's env to reach the
// lifecycle code that reads token snapshots, since hooks run as a subprocess
// of the real entire binary.
func runAntigravityHookWithEnv(t *testing.T, repoDir, hookName string, input map[string]any, extraEnv []string) error {
	t.Helper()
	inputJSON, err := json.Marshal(input)
	require.NoError(t, err)

	cmd := execx.NonInteractive(context.Background(), getTestBinary(), "hooks", "antigravity", hookName)
	cmd.Dir = repoDir
	cmd.Stdin = bytes.NewReader(inputJSON)
	cmd.Env = append(testutil.GitIsolatedEnv(), extraEnv...)
	output, runErr := cmd.CombinedOutput()
	t.Logf("antigravity hook %s output: %s", hookName, output)
	return runErr
}

func mergeMaps(base, overrides map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(overrides))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}
