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
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
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

func runAntigravityHook(t *testing.T, repoDir, hookName string, input map[string]any) error {
	t.Helper()
	inputJSON, err := json.Marshal(input)
	require.NoError(t, err)

	cmd := execx.NonInteractive(context.Background(), getTestBinary(), "hooks", "antigravity", hookName)
	cmd.Dir = repoDir
	cmd.Stdin = bytes.NewReader(inputJSON)
	cmd.Env = testutil.GitIsolatedEnv()
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
