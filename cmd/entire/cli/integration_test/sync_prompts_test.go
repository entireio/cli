//go:build integration

package integration

import (
	"bytes"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
)

// syncPromptsMarker is a unique string carried only by the user prompt (and so
// the transcript), never by the committed code, file names, or commit message.
const syncPromptsMarker = "sync-prompts-marker-7f3a9c"

// remoteObjectsContain reports whether any object reachable from any ref in
// the bare remote contains needle (commit messages, trees, and blobs alike).
func remoteObjectsContain(t *testing.T, bareDir, needle string) bool {
	t.Helper()
	cmd := execx.NonInteractive(t.Context(), "git", "-C", bareDir, "cat-file", "--batch-all-objects", "--batch")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git cat-file --batch-all-objects failed: %v", err)
	}
	return bytes.Contains(out, []byte(needle))
}

// TestSyncPromptsFalse_CheckpointsSyncWithoutPrompts verifies the full hook
// flow with strategy_options.sync_prompts=false: condensation and pre-push
// still deliver the checkpoint, but neither prompt.txt nor any transcript
// (so no copy of the user's prompt) reaches the checkpoint remote.
func TestSyncPromptsFalse_CheckpointsSyncWithoutPrompts(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend
		bareDir := env.SetupBareRemote()

		env.PatchSettings(map[string]any{
			"strategy_options": map[string]any{
				"sync_prompts": false,
			},
		})

		checkpointID := createCheckpointedCommit(t, env, "Add parser "+syncPromptsMarker, "parser.go", "package parser", "Add parser")
		if checkpointID == "" {
			t.Fatal("should have a checkpoint ID after condensation")
		}

		env.RunPrePush("origin")

		if !env.CheckpointExistsOnRemote(bareDir, checkpointID) {
			t.Fatalf("checkpoint %s metadata should still sync with sync_prompts=false", checkpointID)
		}
		if remoteObjectsContain(t, bareDir, syncPromptsMarker) {
			t.Error("prompt content reached the checkpoint remote despite sync_prompts=false")
		}
	})
}

// TestSyncPromptsDefault_PromptsSync is the control for the test above: with
// the default setting the same marker does reach the remote, proving the
// object scan can see it.
func TestSyncPromptsDefault_PromptsSync(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	bareDir := env.SetupBareRemote()

	checkpointID := createCheckpointedCommit(t, env, "Add lexer "+syncPromptsMarker, "lexer.go", "package lexer", "Add lexer")
	if checkpointID == "" {
		t.Fatal("should have a checkpoint ID after condensation")
	}
	env.RunPrePush("origin")

	if !remoteObjectsContain(t, bareDir, syncPromptsMarker) {
		t.Error("control: expected prompt content on the remote with default settings")
	}
}
